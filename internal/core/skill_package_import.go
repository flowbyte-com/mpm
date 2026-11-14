// skill_package_import.go — the directory package importer.
//
// Design of record: PR #222 (docs/dev/skill-package-format-design-2026-11-04.md).
//
// This is Stage P4 of the approved design (§10). It delivers:
//
//   - the DIRECTORY import path — path rules (§5.4), limits enforced
//     DURING the walk (§7 threat 11, R12), symlinked-directory refusal and
//     root confinement (§7 threat 19), per-file scanning with the
//     FAIL-CLOSED scanner check (§7.2, R10), the derived projection with
//     the pre-release effective version (§5.2), the single-transaction
//     write (§7 threat 17, R2) and idempotence (§5.7);
//   - the export loss marker (§6.3, R11), which lives in
//     skill_package_export_loss.go.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE TRUST BOUNDARY
//
// An import directory is UNTRUSTED INPUT. Every byte that crosses into the
// database is untrusted until it has been scanned (§7).
//
// Three rules hold the boundary:
//
//  1. The import root is HELD OPEN as a directory descriptor, and every
//     inspection and every read is issued RELATIVE TO THAT DESCRIPTOR
//     (os.Root). A path is a name and a name can be re-pointed between
//     the moment it is checked and the moment it is opened; a descriptor
//     cannot. Replacing the root path with a symlink after the root was
//     opened changes nothing the walk reads (§7 threat 19).
//     A symlinked directory is refused outright (`symlinked_directory`); a
//     symlink whose target resolves outside the import root is refused
//     (`path_escapes_root`).
//  2. Every byte read is bounded. The per-file, total and entrypoint caps
//     are checked AS bytes are read, so a file that grows after it was
//     inspected cannot exceed the cap.
//  3. Validation and scanning happen BEFORE the transaction opens, so a
//     rejected package writes NOTHING at all (§7 threat 17).
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT THE IMPORT DOES NOT DO
//
//   - It does not execute imported content. A script is stored and
//     FLAGGED; there is no code path from package bytes to execution
//     (§7 threat 16, §7.4 B3).
//   - It does not write a single byte to the filesystem. The package's
//     bytes live in the database (§4.4 Rule 2).
//   - It does not accept a ZIP archive. That is P5, and it has its own
//     root derivation (§5.3.1).
//   - It does not offer an agent-facing surface. The import is
//     operator-driven (§7.4 B4, R4); the CLI lands in P8.

package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/semver"
)

// PackageImportLimits are the traversal limits of the importer.
//
// The values are the design's RECOMMENDED defaults (§7 threat 11, O6):
// 512 files, 32 MiB total content, 4 MiB per file, 256 KiB for the
// entrypoint.
//
// They are PROVISIONAL: O6 is an open operator decision (§12.2), so a
// caller may set its own limits. The zero value of a limit means "use the
// default", so a caller that sets nothing gets the recommended values.
//
// The limits are enforced DURING the traversal, not after a complete
// listing has been collected (§7 threat 11, R12, T45). The walk stops at
// the first violation.
type PackageImportLimits struct {
	// MaxFiles is the maximum number of FILES in the package.
	MaxFiles int

	// MaxTotalBytes is the maximum total content size of the package.
	MaxTotalBytes int64

	// MaxFileBytes is the maximum size of a single file.
	MaxFileBytes int64

	// MaxEntrypointBytes is the maximum size of the entrypoint.
	//
	// The entrypoint cap matters because ListSkills parses frontmatter
	// out of every skill row's full content on every call, so a
	// multi-megabyte entrypoint would slow every skill listing in the
	// workspace (§7 threat 11).
	MaxEntrypointBytes int64
}

// The recommended default limits (§7 threat 11, O6 — provisional).
const (
	// DefaultPackageMaxFiles is the default maximum file count.
	DefaultPackageMaxFiles = 512

	// DefaultPackageMaxTotalBytes is the default maximum total size.
	DefaultPackageMaxTotalBytes int64 = 32 << 20 // 32 MiB

	// DefaultPackageMaxFileBytes is the default maximum single-file size.
	DefaultPackageMaxFileBytes int64 = 4 << 20 // 4 MiB

	// DefaultPackageMaxEntrypointBytes is the default maximum entrypoint size.
	DefaultPackageMaxEntrypointBytes int64 = 256 << 10 // 256 KiB
)

// withDefaults returns the limits with every unset field replaced by its
// recommended default.
func (l PackageImportLimits) withDefaults() PackageImportLimits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = DefaultPackageMaxFiles
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = DefaultPackageMaxTotalBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultPackageMaxFileBytes
	}
	if l.MaxEntrypointBytes <= 0 {
		l.MaxEntrypointBytes = DefaultPackageMaxEntrypointBytes
	}
	return l
}

// PackageImportStatus is the outcome of an import.
type PackageImportStatus string

const (
	// PackageImportStatusImported — the package was written.
	PackageImportStatusImported PackageImportStatus = "imported"

	// PackageImportStatusAlreadyInstalled — the same (name, version)
	// was already published with the SAME package_hash. Nothing was
	// written (§5.7).
	PackageImportStatusAlreadyInstalled PackageImportStatus = "already_installed"

	// PackageImportStatusResurrected — the same package was
	// soft-shredded and the import revived it (§5.7).
	//
	// The import path is not the legacy force path, so R1's
	// package_immutable refusal does not apply to it: a re-import of the
	// same package_hash explicitly resurrects the row.
	PackageImportStatusResurrected PackageImportStatus = "resurrected"
)

// PackageImportOptions are the operator's choices for one import.
type PackageImportOptions struct {
	// Path is the package root — the directory that contains `SKILL.md`
	// — or the path of a standalone `SKILL.md`, which is imported as a
	// one-file package (§13 T02).
	//
	// The operator names the root, so §5.3.1's root derivation governs
	// the ZIP path only; this path has no ambiguity to resolve.
	Path string

	// VersionOverride replaces the default effective version for a
	// package that declares none (§5.2).
	//
	// It is refused for a package that DOES declare a version: the
	// declared version IS the effective one (§5.2), so silently
	// ignoring the operator's override would be worse than refusing it.
	VersionOverride string

	// AllowBinary opts in to binary assets and executable binaries
	// (O3, §7.3).
	//
	// PROVISIONAL: O3 is an open operator decision (§12.2). The default
	// is to REJECT a binary asset, which is the design's recommended
	// default. Even with the opt-in, the bytes are only STORED — an
	// executable binary is never executed (§7 threat 16).
	AllowBinary bool

	// Limits overrides the traversal limits. The zero value means the
	// recommended defaults.
	Limits PackageImportLimits

	// AuthorAgent is the agent recorded as the author of the projected
	// skill row. It defaults to "operator".
	AuthorAgent string

	// ActiveContext is the caller's provenance identity for this import.
	//
	// It is OPTIONAL. The zero value means "no identity supplied", and
	// the import is then attributed to the ambient operator identity
	// rather than to the agent that wrote whatever the import replaces.
	//
	// It matters for a RESURRECTION: the import replaces the content of
	// an existing row in place, so the row's provenance is refreshed to
	// describe the importing invocation. Supplying this context is how a
	// caller says "this invocation wrote the content".
	ActiveContext ActiveContext
}

// PackageImportResult is the outcome of a successful import.
type PackageImportResult struct {
	// Status is the outcome: imported, already_installed or resurrected.
	Status PackageImportStatus

	// SkillID is the canonical `skill:<name>-v<version>` id.
	SkillID string

	// Name is the package's skill name.
	Name string

	// Version is the EFFECTIVE version.
	Version string

	// DeclaredVersion is the version the package declared, or nil when
	// it declared none.
	DeclaredVersion *string

	// VersionSource is how the effective version was arrived at:
	// `declared` or `package_declares_no_version` (§9).
	VersionSource string

	// PackageHash is the SHA-256 of the canonical manifest JSON (§5.5).
	PackageHash string

	// Manifest is the stored canonical manifest JSON.
	Manifest string

	// Entrypoint is the canonical path of the entrypoint.
	Entrypoint string

	// EntrypointSHA256 is the hash of the ORIGINAL entrypoint bytes.
	EntrypointSHA256 string

	// ProjectionSHA256 is the hash of the bytes written to
	// memories.content (§5.2).
	ProjectionSHA256 string

	// Files are the package's files, in canonical path order.
	Files []PackageManifestFile

	// Warnings are non-blocking observations, such as
	// `missing_when_to_use` (O10 — provisional: a warning, not a block).
	Warnings []string

	// Advisories are non-blocking observations about the package's
	// relationship to other packages — currently
	// `possible_duplicate_content` (§5.7).
	Advisories []string
}

// packageImportFile is one file of the package, after the walk and the
// scans.
type packageImportFile struct {
	// canonicalPath is the file's identity (§5.4).
	canonicalPath string

	// rawPath is the name as it appeared in the source tree. It is
	// DISPLAY-ONLY (§5.4).
	rawPath string

	// absPath is the path the bytes are read from.
	absPath string

	// sizeBytes is the length of the bytes.
	sizeBytes int64

	// content is the file's bytes.
	content []byte

	// sha256 is the lowercase hex SHA-256 of the bytes.
	sha256 string

	// contentType is the stored content type.
	contentType string

	// role is the file's role inside the package.
	role PackageRole

	// executable flags a script.
	executable bool

	// scanned records which scanners actually ran.
	scanned PackageScanStatus
}

// ImportSkillPackageFromDir imports a skill package from a directory (or
// from a standalone `SKILL.md`) and stores it atomically.
//
// The import owns ONE transaction, which covers the projected skill row,
// the prior versions' is_latest changes, the content-addressed blob rows,
// the package manifest row, the per-file rows and the transactional
// provenance (§7 threat 17). A failure at any point rolls the whole
// import back.
//
// Validation and scanning happen BEFORE the transaction opens, so a
// rejected package writes nothing at all.
//
// The transaction writer defers its read-back assertion (an uncommitted
// row is invisible to a read outside the transaction), so this function
// performs a POST-COMMIT verification: the package row, the file rows,
// the blob rows and the projected skill row are read back and checked
// after the commit.
func (dm *DatabaseManager) ImportSkillPackageFromDir(opts PackageImportOptions) (*PackageImportResult, error) {
	if dm == nil {
		return nil, packageImportErrorf(PackageImportReasonImportRootInvalid, "", "no database manager")
	}
	if strings.TrimSpace(opts.Path) == "" {
		return nil, packageImportErrorf(PackageImportReasonImportRootInvalid, "", "no import path was given")
	}

	limits := opts.Limits.withDefaults()
	author := opts.AuthorAgent
	if author == "" {
		author = "operator"
	}

	// ── Phase 1: walk, read, scan. No transaction is open. ──────────────
	plan, err := dm.planPackageImport(opts, limits)
	if err != nil {
		return nil, err
	}

	// ── Phase 2: the single transaction. ────────────────────────────────
	result, err := dm.commitPackageImport(plan, author, opts.ActiveContext)
	if err != nil {
		return nil, err
	}

	// ── Phase 3: post-commit verification. ──────────────────────────────
	if err := dm.verifyPackageImport(result); err != nil {
		return nil, err
	}
	return result, nil
}

// packageImportPlan is everything the transaction needs, computed before
// the transaction opens.
type packageImportPlan struct {
	// files are the package's files, sorted by canonical path.
	files []packageImportFile

	// name is the skill name.
	name string

	// declaredVersion is the version the entrypoint declared, or nil.
	declaredVersion *string

	// effectiveVersion is the version the package is known by.
	effectiveVersion string

	// versionSource is how the effective version was arrived at.
	versionSource string

	// projection is the bytes written to memories.content.
	projection string

	// entrypointSHA256 is the hash of the ORIGINAL entrypoint bytes.
	entrypointSHA256 string

	// projectionSHA256 is the hash of the projection bytes.
	projectionSHA256 string

	// manifest is the stored canonical manifest JSON.
	manifest string

	// packageHash is the hash of the canonical manifest JSON.
	packageHash string

	// warnings are the non-blocking observations.
	warnings []string
}

// planPackageImport performs every validation the import needs, and
// returns the plan the transaction will execute.
//
// NOTHING is written here. The walk, the reads, the scans, the hashing,
// the version derivation and the manifest construction all happen before
// a transaction exists, so a refusal at any point leaves the database
// untouched (§7 threat 17).
func (dm *DatabaseManager) planPackageImport(opts PackageImportOptions, limits PackageImportLimits) (*packageImportPlan, error) {
	// The limits are defaulted HERE as well as in the public entry point,
	// so a caller that reaches the planner directly still gets the
	// recommended defaults rather than a zero limit.
	limits = limits.withDefaults()

	root, err := resolvePackageImportRoot(opts.Path)
	if err != nil {
		return nil, err
	}

	// The poison scanner's configuration must be loadable BEFORE any
	// content is scanned. The scanner reads an operator-editable file
	// and can fail open; the import refuses instead (§7.2, R10, T43).
	phrasesPath := dm.poisonPhrasesPath()
	if err := poisonScannerAvailable(phrasesPath); err != nil {
		return nil, packageImportErrorWrap(PackageImportReasonScannerUnavailable, "", err,
			"the toxic-phrase configuration could not be loaded: %v", err)
	}

	files, err := walkPackageTree(root, limits)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, packageImportErrorf(PackageImportReasonEntrypointNotFound, "",
			"the package root %q contains no files", root)
	}

	// The entrypoint is `SKILL.md` at the package root (§5.3). The
	// operator named the root, so there is no competing-root ambiguity
	// to resolve here (§5.3.1 governs the ZIP path only).
	entrypointIndex := -1
	var candidates []string
	for i := range files {
		if files[i].canonicalPath == packageEntrypointName {
			entrypointIndex = i
			continue
		}
		if strings.EqualFold(filepath.Base(files[i].canonicalPath), packageEntrypointName) {
			candidates = append(candidates, files[i].canonicalPath)
		}
	}
	if entrypointIndex < 0 {
		return nil, packageImportErrorf(PackageImportReasonEntrypointNotFound, "",
			"the package root %q has no %s at its top level%s",
			root, packageEntrypointName, formatPackageEntrypointCandidates(candidates))
	}

	// The entrypoint's own cap.
	if int64(len(files[entrypointIndex].content)) > limits.MaxEntrypointBytes {
		return nil, packageImportErrorf(PackageImportReasonSizeLimit, packageEntrypointName,
			"the entrypoint is %d bytes, over the %d-byte cap",
			len(files[entrypointIndex].content), limits.MaxEntrypointBytes)
	}

	// Derive the version and the projection from the ORIGINAL entrypoint
	// bytes. The original bytes are never rewritten (§5.2).
	name, declaredVersion, projection, warnings, err := derivePackageProjection(
		files[entrypointIndex].content, opts.VersionOverride)
	if err != nil {
		return nil, err
	}

	effectiveVersion := ""
	versionSource := ""
	switch {
	case declaredVersion != nil:
		effectiveVersion = *declaredVersion
		versionSource = PackageVersionSourceDeclared
	default:
		effectiveVersion = packageDefaultUnversionedVersion
		if opts.VersionOverride != "" {
			effectiveVersion = opts.VersionOverride
		}
		versionSource = PackageVersionSourceUnversioned
	}
	if !semver.IsValid("v" + effectiveVersion) {
		return nil, packageImportErrorf(PackageImportReasonVersionInvalid, packageEntrypointName,
			"the effective version %q is not valid semver", effectiveVersion)
	}
	if err := validateSkillNameAndVersion(name, effectiveVersion); err != nil {
		return nil, packageImportErrorf(PackageImportReasonNameInvalid, packageEntrypointName, "%v", err)
	}

	// Scan every file, and refuse the WHOLE import on the first
	// violation (§7 threat 18).
	for i := range files {
		if err := dm.scanPackageFile(&files[i], phrasesPath, opts.AllowBinary); err != nil {
			return nil, err
		}
	}

	// Build the manifest and hash it.
	manifestFiles := make([]PackageManifestFile, 0, len(files))
	for i := range files {
		f := &files[i]
		manifestFiles = append(manifestFiles, PackageManifestFile{
			Path:        f.canonicalPath,
			SHA256:      f.sha256,
			SizeBytes:   f.sizeBytes,
			ContentType: f.contentType,
			Role:        f.role,
			Executable:  f.executable,
			Scanned:     f.scanned,
		})
	}

	entrypointSHA := files[entrypointIndex].sha256
	projectionSHA := sha256Hex([]byte(projection))

	manifest := PackageManifest{
		Format:          PackageFormatV1,
		Name:            name,
		Version:         effectiveVersion,
		DeclaredVersion: declaredVersion,
		VersionOverride: PackageVersionOverride{
			Declared:  declaredVersion,
			Effective: effectiveVersion,
			Reason:    versionSource,
		},
		Entrypoint:       packageEntrypointName,
		EntrypointSHA256: entrypointSHA,
		ProjectionSHA256: projectionSHA,
		Files:            manifestFiles,
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	stored, err := manifest.StoredJSON()
	if err != nil {
		return nil, err
	}
	hash, err := manifest.ComputePackageHash()
	if err != nil {
		return nil, err
	}

	return &packageImportPlan{
		files:            files,
		name:             name,
		declaredVersion:  declaredVersion,
		effectiveVersion: effectiveVersion,
		versionSource:    versionSource,
		projection:       projection,
		entrypointSHA256: entrypointSHA,
		projectionSHA256: projectionSHA,
		manifest:         stored,
		packageHash:      hash,
		warnings:         warnings,
	}, nil
}

// packageEntrypointName is the entrypoint's file name (§5.3).
const packageEntrypointName = "SKILL.md"

// packageDefaultUnversionedVersion is the effective version MPM assigns
// to a package that declares none (§5.2, §12.1 O1).
//
// It is a PRE-RELEASE identifier, and that is load-bearing: a pre-release
// sorts below the corresponding release, so a later genuine `1.0.0` wins
// the `is_latest` flag and both packages coexist, while the versionless
// one can never collide with an author-declared `1.0.0`.
const packageDefaultUnversionedVersion = "1.0.0-unversioned"

// formatPackageEntrypointCandidates renders the candidate entrypoints for
// an `entrypoint_not_found` refusal, so the operator can see what the
// tree actually carried.
func formatPackageEntrypointCandidates(candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	sort.Strings(candidates)
	if len(candidates) > 8 {
		candidates = candidates[:8]
	}
	return " (candidates found: " + strings.Join(candidates, ", ") + ")"
}

// sha256Hex returns the lowercase hex SHA-256 of the bytes.
func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// ─────────────────────────────────────────────────────────────────────────────
// THE WALK
// ─────────────────────────────────────────────────────────────────────────────

// resolvePackageImportRoot resolves the operator's path into a package
// root, and reports whether the import is a DIRECTORY package or a
// standalone one-file package.
//
// A standalone `SKILL.md` is a one-file package (§13 T02): the operator
// points at the file, and only that file is imported. Its canonical path
// is `SKILL.md`.
func resolvePackageImportRoot(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
			"the import path %q could not be inspected: %v", path, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		// The operator's own path may be a symlink — resolving it is
		// reasonable, and the RESOLVED path becomes the root against
		// which every entry is confined.
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
				"the import path %q could not be resolved: %v", path, err)
		}
		path = resolved
		info, err = os.Lstat(path)
		if err != nil {
			return "", packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
				"the import path %q could not be inspected: %v", path, err)
		}
	}

	if info.IsDir() {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
				"the import root %q could not be resolved: %v", path, err)
		}
		return resolved, nil
	}

	if info.Mode().IsRegular() {
		if filepath.Base(path) != packageEntrypointName {
			return "", packageImportErrorf(PackageImportReasonEntrypointNotFound, filepath.Base(path),
				"a standalone package file must be named %s", packageEntrypointName)
		}
		return path, nil
	}

	return "", packageImportErrorf(PackageImportReasonImportRootInvalid, "",
		"the import path %q is neither a directory nor a regular file", path)
}

// walkPackageTree walks the import root and returns the package's files.
//
// THE GUARANTEES, STATED EXACTLY
//
//  1. The walk is CONFINEd to the root's OPENED directory descriptor.
//     Every inspection and every read is issued relative to it (os.Root),
//     so the bytes come from the inode the operator selected even if the
//     paths underneath it are re-pointed while the walk runs.
//  2. A symlinked DIRECTORY is refused outright (`symlinked_directory`) —
//     the walk does not descend into it.
//  3. A symlink whose target resolves OUTSIDE the resolved import root is
//     refused (`path_escapes_root`).
//  4. Every file's bytes are read through a limited reader, so a file
//     that grows after it was inspected cannot exceed the cap.
//  5. The opened file's identity is compared with the inspected one, and
//     the opened DIRECTORY's identity is compared with the inspected one,
//     so a file or a directory swapped between the two is detected
//     (`changed_during_read`).
//
// The limits are enforced DURING the walk: the walk stops at the first
// violation and never enumerates the whole tree (§7 threat 11, R12).
func walkPackageTree(root string, limits PackageImportLimits) ([]packageImportFile, error) {
	anchor, err := openPackageImportAnchor(root)
	if err != nil {
		return nil, err
	}
	defer anchor.Close()

	// A standalone one-file package: the root IS the entrypoint.
	if anchor.file != "" {
		content, size, err := anchor.readFile(anchor.file, limits.MaxFileBytes)
		if err != nil {
			return nil, err
		}
		return []packageImportFile{{
			canonicalPath: packageEntrypointName,
			rawPath:       packageEntrypointName,
			absPath:       filepath.Join(anchor.rootPath, anchor.file),
			sizeBytes:     size,
			content:       content,
			sha256:        sha256Hex(content),
		}}, nil
	}

	// The root directory itself, opened THROUGH the anchor, so the listing
	// comes from the anchored inode and not from the path.
	rootDir, err := anchor.root.Open(".")
	if err != nil {
		return nil, packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
			"the import root %q could not be opened: %v", anchor.rootPath, err)
	}
	defer rootDir.Close()

	var files []packageImportFile
	var totalBytes int64
	paths := NewPackagePathSet()

	err = walkPackageDir(anchor, rootDir, ".", limits, paths, &files, &totalBytes, 0)
	if err != nil {
		return nil, err
	}

	sort.SliceStable(files, func(i, j int) bool {
		return files[i].canonicalPath < files[j].canonicalPath
	})
	return files, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// THE ANCHOR — a file-descriptor-relative handle on the import root
// ─────────────────────────────────────────────────────────────────────────────

// packageImportAnchor is the import root, held OPEN.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THE ROOT IS AN OPENED DIRECTORY AND NOT A PATH
// ─────────────────────────────────────────────────────────────────────────────
//
// A path is a NAME, and a name can be re-pointed between the moment it is
// checked and the moment it is opened. That window is the whole of the
// confinement problem: resolving the root with filepath.EvalSymlinks and
// then walking it by path leaves a gap in which the root path can be
// replaced by a symlink to somewhere else, and the walk then reads that
// somewhere else while believing it is reading the root.
//
// The anchor closes the gap by holding the root's DIRECTORY DESCRIPTOR.
// Every read the walk performs is issued RELATIVE TO THAT DESCRIPTOR
// (os.Root), so the bytes come from the inode the operator selected no
// matter what happens to the path afterwards. Replacing the root path with
// a symlink after the anchor is opened changes nothing the walk reads.
//
// The anchor therefore establishes the invariant the design states (§7
// threat 19): the resolved root is compared against every candidate
// before it is read, and the comparison is not a string comparison that a
// concurrent rename can defeat — it is a kernel-enforced resolution
// relative to an open directory.
//
// The path-based logic that remains (resolving a symlink's target so the
// refusal can name it) NEVER READS BYTES. Every byte the importer stores
// comes from an os.Root operation, which is what makes the confinement
// invariant hold even while the paths are being re-pointed underneath it.
type packageImportAnchor struct {
	// root is the anchored directory. Every read is relative to it.
	root *os.Root

	// rootPath is the RESOLVED path the anchor was opened from. It is
	// used for messages and for the path-based classification of a
	// symlink's target — never to read bytes.
	rootPath string

	// file is the name of the single file to import when the import root
	// is a standalone SKILL.md rather than a directory. It is empty for a
	// directory root.
	file string
}

// openPackageImportAnchor opens the import root and binds it to the inode
// that was resolved.
//
// The identity chain is deliberate:
//
//	expected (os.Lstat, path)  →  actual (os.Root, descriptor-relative)
//
// If the two disagree, the root path was re-pointed between the
// resolution and the open, and the import is refused rather than continued
// against an inode the operator did not select.
func openPackageImportAnchor(resolvedPath string) (*packageImportAnchor, error) {
	expected, err := os.Lstat(resolvedPath)
	if err != nil {
		return nil, packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
			"the import root %q could not be inspected: %v", resolvedPath, err)
	}
	if expected.Mode()&os.ModeSymlink != 0 {
		// The root became a symlink after it was resolved. Continuing
		// would resolve it again, to somewhere the operator never named.
		return nil, packageImportErrorf(PackageImportReasonChangedDuringRead, "",
			"the import root %q was replaced by a symlink after it was resolved", resolvedPath)
	}

	if expected.IsDir() {
		root, err := os.OpenRoot(resolvedPath)
		if err != nil {
			return nil, packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
				"the import root %q could not be opened: %v", resolvedPath, err)
		}
		actual, err := root.Stat(".")
		if err != nil {
			root.Close()
			return nil, packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
				"the import root %q could not be inspected after opening: %v", resolvedPath, err)
		}
		if !os.SameFile(expected, actual) {
			root.Close()
			return nil, packageImportErrorf(PackageImportReasonChangedDuringRead, "",
				"the import root %q was replaced between the inspection and the open", resolvedPath)
		}
		return &packageImportAnchor{root: root, rootPath: resolvedPath}, nil
	}

	if !expected.Mode().IsRegular() {
		return nil, packageImportErrorf(PackageImportReasonImportRootInvalid, "",
			"the import path %q is neither a directory nor a regular file", resolvedPath)
	}

	// A standalone one-file package. The anchor is the file's PARENT
	// directory, and the file is read by name relative to it — so the read
	// is descriptor-relative too.
	parent := filepath.Dir(resolvedPath)
	base := filepath.Base(resolvedPath)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return nil, packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
			"the import root %q could not be inspected: %v", parent, err)
	}
	if !parentInfo.IsDir() {
		return nil, packageImportErrorf(PackageImportReasonImportRootInvalid, "",
			"the import root %q is not inside a directory", resolvedPath)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
			"the import root %q could not be opened: %v", parent, err)
	}
	actual, err := root.Lstat(base)
	if err != nil {
		root.Close()
		return nil, packageImportErrorWrap(PackageImportReasonImportRootInvalid, "", err,
			"the import root %q could not be inspected after opening: %v", resolvedPath, err)
	}
	if !os.SameFile(expected, actual) {
		root.Close()
		return nil, packageImportErrorf(PackageImportReasonChangedDuringRead, "",
			"the import root %q was replaced between the inspection and the open", resolvedPath)
	}
	return &packageImportAnchor{root: root, rootPath: parent, file: base}, nil
}

// Close releases the anchor.
func (a *packageImportAnchor) Close() {
	if a != nil && a.root != nil {
		_ = a.root.Close()
	}
}

// absPath renders the anchor-relative POSIX path back into the resolved
// root's path space. It is used for MESSAGES and for the path-based
// classification of a symlink's target — never to read bytes.
func (a *packageImportAnchor) absPath(rel string) string {
	if rel == "." || rel == "" {
		return a.rootPath
	}
	return filepath.Join(a.rootPath, filepath.FromSlash(rel))
}

// walkPackageDir is the recursive walk.
//
// The directory listing is read in BATCHES rather than in one call, so a
// directory holding millions of entries is never materialised: the walk
// stops at the limit instead of enumerating the tree (§7 threat 11).
//
// `dir` is the ALREADY-OPENED directory, and `rel` is its path relative to
// the anchor. The listing therefore comes from the descriptor, so a
// directory replaced by a symlink after it was opened cannot change what
// the walk enumerates.
func walkPackageDir(anchor *packageImportAnchor, dir *os.File, rel string, limits PackageImportLimits, paths *PackagePathSet, files *[]packageImportFile, totalBytes *int64, depth int) error {
	if depth > MaxPackagePathDepth {
		return packageImportErrorf(PackageImportReasonPathInvalid, "",
			"the import tree is deeper than %d segments", MaxPackagePathDepth)
	}

	for {
		// A bounded batch keeps the listing small. The loop ends when
		// the batch comes back empty.
		entries, err := dir.ReadDir(64)
		for _, entry := range entries {
			if err2 := walkPackageEntry(anchor, dir, rel, entry, limits, paths, files, totalBytes, depth); err2 != nil {
				return err2
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return packageImportErrorWrap(PackageImportReasonReadFailed, "", err,
				"the directory %q could not be listed: %v", rel, err)
		}
		if len(entries) == 0 {
			return nil
		}
	}
}

// walkPackageEntry handles one entry of a directory.
//
// `rel` is the directory's path relative to the anchor, POSIX-separated.
// The entry's own path relative to the anchor is `rel/name`.
//
// EVERY inspection and EVERY read here is issued RELATIVE TO THE ANCHOR's
// open directory descriptor, so the walk reads the inode the operator
// selected even if the paths underneath it are re-pointed while the walk
// runs.
func walkPackageEntry(anchor *packageImportAnchor, dir *os.File, rel string, entry fs.DirEntry, limits PackageImportLimits, paths *PackagePathSet, files *[]packageImportFile, totalBytes *int64, depth int) error {
	name := entry.Name()
	childRel := name
	if rel != "." && rel != "" {
		childRel = rel + "/" + name
	}

	// The raw path is relative to the import root, POSIX-separated. It
	// is DISPLAY-ONLY (§5.4): the canonical path is the identity.
	rawPath := childRel

	// The inspection is descriptor-relative and does NOT follow a symlink.
	info, err := anchor.root.Lstat(childRel)
	if err != nil {
		return packageImportErrorWrap(PackageImportReasonReadFailed, rawPath, err,
			"the entry %q could not be inspected: %v", rawPath, err)
	}

	// The bytes to read, and the identity they are expected to have.
	//
	// `readRel` is the anchor-relative path the bytes are read FROM, and
	// `expected` is the identity the opened file must have. For an
	// ordinary file they are the same path; for an admitted symlink they
	// are the symlink's RESOLVED target, expressed relative to the anchor
	// so the read is still descriptor-relative.
	readRel := childRel
	expected := info

	// ── A symlink is never followed blindly. ──────────────────────────
	if info.Mode()&os.ModeSymlink != 0 {
		// The target is resolved so the refusal can NAME it and so its
		// type can be classified. This resolution is PATH-BASED and can
		// be raced — but it never reads bytes, and the read that follows
		// is issued through the anchor. A raced resolution can therefore
		// produce a different refusal REASON, never an escape.
		abs := anchor.absPath(childRel)
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return packageImportErrorWrap(PackageImportReasonPathEscapesRoot, rawPath, err,
				"the symlink %q could not be resolved: %v", rawPath, err)
		}
		resolvedInfo, err := os.Stat(resolved)
		if err != nil {
			return packageImportErrorWrap(PackageImportReasonPathEscapesRoot, rawPath, err,
				"the symlink %q does not resolve to a readable target: %v", rawPath, err)
		}
		// The target is admitted only when it resolves INSIDE the import
		// root. This is the classification; the read below is confined by
		// the anchor regardless of what this check concludes.
		if !pathWithinPackageRoot(anchor.rootPath, resolved) {
			return packageImportErrorf(PackageImportReasonPathEscapesRoot, rawPath,
				"the symlink %q resolves to %q, which is outside the import root", rawPath, resolved)
		}
		if resolvedInfo.IsDir() {
			// The walk refuses to descend into a symlinked directory
			// (§7 threat 19, T46).
			return packageImportErrorf(PackageImportReasonSymlinkedDirectory, rawPath,
				"the entry %q is a symlink to a directory; a symlinked directory is not descended into", rawPath)
		}
		if !resolvedInfo.Mode().IsRegular() {
			return packageImportErrorf(PackageImportReasonUnsupportedEntryType, rawPath,
				"the entry %q is a symlink to a %s", rawPath, resolvedInfo.Mode().Type())
		}
		// The target is a regular file inside the root. It is read
		// through the anchor, by the path the target has RELATIVE TO THE
		// ROOT — a relative path with no `..`, so the anchor's own
		// confinement decides whether it can be read at all.
		targetRel, err := filepath.Rel(anchor.rootPath, resolved)
		if err != nil {
			return packageImportErrorWrap(PackageImportReasonPathEscapesRoot, rawPath, err,
				"the symlink %q could not be made relative to the import root: %v", rawPath, err)
		}
		readRel = filepath.ToSlash(targetRel)
		expected = resolvedInfo
	}

	// ── A directory is descended into. ────────────────────────────────
	if info.IsDir() {
		// The directory is opened THROUGH the anchor, and the OPENED
		// directory's identity is compared with the inspected one. A
		// directory replaced between the two is detected
		// (`changed_during_read`), and the listing that follows comes
		// from the opened descriptor, so it cannot be re-pointed either.
		sub, err := anchor.root.Open(childRel)
		if err != nil {
			return packageImportErrorWrap(PackageImportReasonReadFailed, rawPath, err,
				"the directory %q could not be opened: %v", rawPath, err)
		}
		defer sub.Close()
		subInfo, err := sub.Stat()
		if err != nil {
			return packageImportErrorWrap(PackageImportReasonReadFailed, rawPath, err,
				"the directory %q could not be inspected after opening: %v", rawPath, err)
		}
		if !os.SameFile(info, subInfo) {
			return packageImportErrorf(PackageImportReasonChangedDuringRead, rawPath,
				"the directory %q was replaced between the inspection and the read", rawPath)
		}
		return walkPackageDir(anchor, sub, childRel, limits, paths, files, totalBytes, depth+1)
	}

	// ── Anything that is not a regular file is refused. ───────────────
	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return packageImportErrorf(PackageImportReasonUnsupportedEntryType, rawPath,
			"the entry %q is a %s, not a regular file", rawPath, info.Mode().Type())
	}

	// ── The file count is checked BEFORE the file is read. ───────────
	if len(*files)+1 > limits.MaxFiles {
		return packageImportErrorf(PackageImportReasonFileCountLimit, rawPath,
			"the package carries more than %d files", limits.MaxFiles)
	}

	// ── The per-file cap is checked against the INSPECTED size, and
	// again against the bytes actually read. ──────────────────────────
	if expected.Size() > limits.MaxFileBytes {
		return packageImportErrorf(PackageImportReasonSizeLimit, rawPath,
			"the file is %d bytes, over the %d-byte cap", expected.Size(), limits.MaxFileBytes)
	}

	content, size, err := anchor.readFile(readRel, limits.MaxFileBytes)
	if err != nil {
		return err
	}

	// ── The total cap is checked as bytes are read. ──────────────────
	*totalBytes += size
	if *totalBytes > limits.MaxTotalBytes {
		return packageImportErrorf(PackageImportReasonSizeLimit, rawPath,
			"the package's content exceeds the %d-byte total cap", limits.MaxTotalBytes)
	}

	// ── The canonical path is the identity, and the collision rules are
	// the P2 ones (§5.4). ─────────────────────────────────────────────
	canonical, err := paths.Add(rawPath)
	if err != nil {
		return packageImportErrorWrap(PackageImportReasonPathInvalid, rawPath, err,
			"the path %q was refused: %v", rawPath, err)
	}

	*files = append(*files, packageImportFile{
		canonicalPath: canonical,
		rawPath:       rawPath,
		absPath:       anchor.absPath(childRel),
		sizeBytes:     size,
		content:       content,
		sha256:        sha256Hex(content),
	})
	return nil
}

// readFile reads one file THROUGH THE ANCHOR.
//
// The read is descriptor-relative: the file is opened relative to the
// anchor's open directory, so it is a file the operator's directory
// actually holds — not a file a concurrent rename re-pointed the path at.
//
// The opened file's identity is compared with the inspected one, so a file
// replaced between the inspection and the read is detected
// (`changed_during_read`). The bytes are read from the OPENED descriptor,
// so they cannot change under the reader either.
func (a *packageImportAnchor) readFile(rel string, maxBytes int64) ([]byte, int64, error) {
	info, err := a.root.Lstat(rel)
	if err != nil {
		return nil, 0, packageImportErrorWrap(PackageImportReasonReadFailed, "", err,
			"the file %q could not be inspected: %v", rel, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, packageImportErrorf(PackageImportReasonUnsupportedEntryType, "",
			"the file %q is a symlink", rel)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, packageImportErrorf(PackageImportReasonUnsupportedEntryType, "",
			"the file %q is not a regular file", rel)
	}

	f, err := a.root.Open(rel)
	if err != nil {
		return nil, 0, packageImportErrorWrap(PackageImportReasonReadFailed, "", err,
			"the file %q could not be opened: %v", rel, err)
	}
	defer f.Close()

	opened, err := f.Stat()
	if err != nil {
		return nil, 0, packageImportErrorWrap(PackageImportReasonReadFailed, "", err,
			"the file %q could not be inspected after opening: %v", rel, err)
	}
	if !os.SameFile(info, opened) {
		return nil, 0, packageImportErrorf(PackageImportReasonChangedDuringRead, "",
			"the file %q was replaced between the inspection and the read", rel)
	}
	if !opened.Mode().IsRegular() {
		return nil, 0, packageImportErrorf(PackageImportReasonUnsupportedEntryType, "",
			"the file %q is not a regular file", rel)
	}

	// The bounded read: one byte past the cap is enough to know the cap
	// was exceeded, and the file is never read whole.
	limited := io.LimitReader(f, maxBytes+1)
	content, err := io.ReadAll(limited)
	if err != nil {
		return nil, 0, packageImportErrorWrap(PackageImportReasonReadFailed, "", err,
			"the file %q could not be read: %v", rel, err)
	}
	if int64(len(content)) > maxBytes {
		return nil, 0, packageImportErrorf(PackageImportReasonSizeLimit, "",
			"the file %q is over the %d-byte cap", rel, maxBytes)
	}
	return content, int64(len(content)), nil
}

// pathWithinPackageRoot reports whether candidate lies inside root.
//
// The comparison is path-aware, not a string prefix: a sibling whose name
// merely shares a prefix (`/tmp/pkg-backup` against `/tmp/pkg`) is
// correctly OUTSIDE.
func pathWithinPackageRoot(root, candidate string) bool {
	if root == candidate {
		return true
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}
