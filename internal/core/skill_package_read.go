// skill_package_read.go — the read-only package retrieval and
// integrity-verification API (Stage P6).
//
// Design of record: PR #222 (docs/dev/skill-package-format-design-2026-11-04.md)
// §8.1–§8.7 (the retrieval API) and §13 (the regression matrix).
//
// WHAT THIS FILE IS
// -----------------
// Three read-only operations over an already-imported skill package:
//
//   - PackageFiles    — the metadata-only listing (§8.5 `files`);
//   - PackageReadFile — one file's original bytes, optionally bounded
//     (§8.5 `read_file`, §8.6 bounded reads);
//   - PackageVerify   — the integrity report (§8.5 `verify`);
//
// plus SkillPackageProvenance, the accessor the existing skill `read`
// action uses to attach the §8.2.1 `package` object (R6 / T40).
//
// WHAT THIS FILE IS NOT
// ---------------------
// It is NOT a writer. It never creates, repairs, reimports, rescans or
// replaces anything. It never materialises a package on the filesystem. It
// never executes a stored script, and it never treats retrieved content as
// instructions — the bytes are DATA, whatever they say.
//
// The read path is deliberately SEPARATE from the import and archive
// paths: the importer owns the bytes, this file only reads the rows the
// importer wrote, under the same schema, the same identity rules, the same
// manifest-hashing rule and the same path validation (§9: do not duplicate
// a rule; reuse it).
//
// EVERY READ RE-HASHES WHAT IT RETURNS. The stored SHA-256 is not taken on
// trust: the bytes are hashed again on the way out, so a blob row whose
// content was mutated in place is refused rather than returned (§8.6, T34).
// The one exception is the provenance accessor, which reports the recorded
// hashes and leaves the re-hashing to `verify` — because `read` must stay a
// cheap retrieval, and because `verify` is the operation that proves the
// package.

package internal

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ─── Constants ─────────────────────────────────────────────────────────────

// PackageReadDefaultMaxBytes is the DEFAULT byte cap of a bounded read:
// 64 KiB (§8.6).
//
// The cap is generous on purpose: a file whose first line alone exceeds
// the cap is a file this API cannot serve at that cap, and that is a rare
// shape rather than a normal one.
const PackageReadDefaultMaxBytes = 64 * 1024

// PackageReadMaxBytesLimit is the HARD byte cap of a bounded read: 1 MiB
// (§8.6).
//
// A request above it is REFUSED with `invalid_bounds`, never clamped: a
// silent clamp would return a response the caller did not ask for.
const PackageReadMaxBytesLimit = 1024 * 1024

// PackageReadSourceSkillPackage is the `source` value of a package file
// read. It says where the bytes came from, so a caller can tell a package
// file from any other source.
const PackageReadSourceSkillPackage = "skill_package"

// The transport encodings a package read may declare.
//
// `utf-8` carries the bytes as a JSON string and is lossless because the
// bytes ARE valid UTF-8. `base64` carries the bytes as a JSON string of
// standard base64 and is lossless for ANY byte sequence.
//
// A future JSON layer MUST use the encoding the response names. It must
// never pass binary content through a text field.
const (
	PackageReadEncodingUTF8   = "utf-8"
	PackageReadEncodingBase64 = "base64"
)

// The SQL the reader uses. Every statement is a CONSTANT: the reader never
// builds a statement from a caller-supplied value, and the only values it
// binds are the skill id, a canonical path and a blob hash — all of which
// are bound parameters, never interpolated.
const (
	packageRowQuery = `
		SELECT skill_id, name, version, declared_version, version_source, format,
		       entrypoint, entrypoint_sha256, manifest, package_hash
		FROM skill_packages
		WHERE skill_id = ?
	`

	packageFilesQuery = `
		SELECT path, sha256, size_bytes, content_type, role, executable,
		       secret_scanned, poison_scanned
		FROM skill_package_files
		WHERE skill_id = ?
		ORDER BY path
	`

	packageFileQuery = `
		SELECT path, sha256, size_bytes, content_type, role, executable,
		       secret_scanned, poison_scanned
		FROM skill_package_files
		WHERE skill_id = ? AND path = ?
	`

	packageBlobQuery = `
		SELECT sha256, size_bytes, content
		FROM skill_package_blobs
		WHERE sha256 = ?
	`

	packageProjectionQuery = `
		SELECT content, metadata
		FROM memories
		WHERE id = ? AND collection = 'skills'
	`

	// packageProvenanceQuery reads the provenance of ONE package.
	//
	// It is deliberately a SINGLE statement. A single statement sees ONE
	// consistent snapshot, so the package row, the file count and the
	// entrypoint's scan status cannot come from two different database
	// states — and the provenance accessor needs no transaction at all.
	//
	// That matters because the provenance accessor runs on the `read`
	// action's hot path: a transaction would cost a connection checkout
	// and a BEGIN/ROLLBACK on every skill read, including the ordinary
	// skills that have no package at all.
	packageProvenanceQuery = `
		SELECT p.entrypoint, p.entrypoint_sha256, p.package_hash,
		       p.declared_version, p.version, p.version_source,
		       (SELECT COUNT(*) FROM skill_package_files f WHERE f.skill_id = p.skill_id),
		       (SELECT f.secret_scanned FROM skill_package_files f
		         WHERE f.skill_id = p.skill_id AND f.path = p.entrypoint),
		       (SELECT f.poison_scanned FROM skill_package_files f
		         WHERE f.skill_id = p.skill_id AND f.path = p.entrypoint)
		FROM skill_packages p
		WHERE p.skill_id = ?
	`
)

// ─── Types ─────────────────────────────────────────────────────────────────

// PackageSelector names the package to read.
//
// A package is named by its canonical skill id, OR by a name AND a
// version. Naming it by a name alone leaves the version open, and this
// reader never picks one silently.
type PackageSelector struct {
	// SkillID is the canonical `skill:<name>-v<version>` id.
	SkillID string

	// Name is the package's skill name. It must be accompanied by
	// Version.
	Name string

	// Version is the package's effective version. It must be accompanied
	// by Name.
	Version string
}

// resolveSkillID turns the selector into the canonical skill id.
//
// The two forms are mutually exclusive: supplying both is refused rather
// than merged, because a silent merge of two conflicting identities is
// exactly the class of mistake the package identity rules exist to
// prevent.
func (s PackageSelector) resolveSkillID() (string, error) {
	id := strings.TrimSpace(s.SkillID)
	name := strings.TrimSpace(s.Name)
	version := strings.TrimSpace(s.Version)

	if id != "" {
		if name != "" || version != "" {
			return "", packageReadErrorf(PackageReadReasonSelectorInvalid, "",
				"skill_id and name/version are both supplied; supply one or the other")
		}
		return id, nil
	}
	if name == "" || version == "" {
		return "", packageReadErrorf(PackageReadReasonSelectorInvalid, "",
			"a package is named by skill_id, or by name AND version")
	}
	resolved, err := SkillIDForNameAndVersion(name, version)
	if err != nil {
		return "", packageReadErrorWrap(PackageReadReasonSelectorInvalid, "", err,
			"the package's name and version do not identify a skill")
	}
	return resolved, nil
}

// PackageFileListing is the result of a package file listing (§8.5
// `files`).
//
// It carries METADATA ONLY. No file's content is ever part of it: the
// listing exists so a caller can decide what to ask for, and a listing
// that inlined the bytes would defeat the progressive retrieval the
// design is built on (§8.7).
type PackageFileListing struct {
	// SkillID is the canonical id of the packaged skill.
	SkillID string

	// Name is the package's skill name.
	Name string

	// Version is the package's effective version.
	Version string

	// PackageHash is the hash of the package's canonical manifest.
	PackageHash string

	// FileCount is the number of files the package carries.
	FileCount int

	// Files are the package's files, in deterministic canonical-path
	// order.
	//
	// The scan statuses are the ACTUAL statuses recorded on the file
	// rows — what the scanners really did — not a constant.
	Files []PackageManifestFile
}

// PackageReadBounds is the bounded-read request (§8.6).
//
// The three parameters use three DIFFERENT units and are never converted
// into one another: `offset` and `limit` count LINES, `max_bytes` counts
// BYTES.
type PackageReadBounds struct {
	// Offset is the zero-based index of the first line to return.
	//
	// Negative is `invalid_bounds`. An offset equal to the file's line
	// count is valid and returns an empty, complete read — it is the
	// natural terminal resume.
	Offset int

	// Limit is the maximum number of lines to return.
	//
	// Zero means "no line-count bound": the byte cap and the end of the
	// file are then the only bounds. Negative is `invalid_bounds`.
	Limit int

	// MaxBytes is the maximum number of CONTENT bytes to return.
	//
	// Zero means the default (64 KiB). Negative, or above the 1 MiB hard
	// cap, is `invalid_bounds`.
	MaxBytes int

	// Raw asks for the BYTES rather than a text rendering.
	//
	// With `raw` the transport encoding is base64, for text files as well
	// as binary ones, so "these are the bytes" is never a matter of
	// interpretation. Without it the content must be text-safe: a binary
	// file, or bytes that are not valid UTF-8, is refused with
	// `binary_requires_raw`.
	Raw bool
}

// PackageFileContent is the result of a package file read (§8.5
// `read_file`).
type PackageFileContent struct {
	// SkillID is the canonical id of the packaged skill.
	SkillID string

	// Path is the file's CANONICAL package-relative path.
	Path string

	// SHA256 is the hash of the file's STORED bytes — the hash the
	// package recorded, re-verified against the bytes on the way out.
	//
	// For a bounded read it is the hash of the WHOLE file, not of the
	// returned window: the window is a window into the file, and the
	// file's identity does not change because the read was bounded.
	SHA256 string

	// SizeBytes is the length of the WHOLE file's bytes.
	SizeBytes int64

	// ContentType is the stored content type.
	ContentType string

	// Role is the file's role inside the package.
	Role PackageRole

	// Executable flags a script. A flagged script is STORED and never
	// executed (§7 threat 16).
	Executable bool

	// Scanned records which scanners actually ran on this file.
	Scanned PackageScanStatus

	// Source says where the bytes came from.
	Source string

	// Content is the returned window — the EXACT stored bytes, with
	// their original line endings.
	//
	// For a bounded read it is the concatenation of the returned lines,
	// so that concatenating every successive window reproduces the file
	// byte for byte (T47).
	Content []byte

	// Encoding names the lossless transport form of Content.
	//
	// It is `utf-8` for a text rendering and `base64` for the raw byte
	// form. A caller MUST use the encoding the response names.
	Encoding string

	// Truncated reports that the file was not fully returned.
	Truncated bool

	// Offset is the line index the read started at.
	Offset int

	// NextOffset is the index of the FIRST line that was not returned,
	// or nil when the read is complete.
	//
	// It is a LINE index, never a byte offset (§8.6). Every non-terminal
	// truncated response satisfies `*NextOffset > Offset`.
	NextOffset *int

	// Lines is the number of lines returned.
	Lines int

	// Terminal reports that this response CANNOT be resumed.
	//
	// It is set only for the §8.6 terminal case, and it always
	// accompanies Reason == `line_too_large`.
	Terminal bool

	// Reason is the classification of a terminal response. It is empty for
	// an ordinary response.
	Reason PackageReadReason
}

// EncodedContent returns the content in the transport form the response's
// Encoding field names.
//
// For `utf-8` that is the text itself; for `base64` it is the standard
// base64 encoding of the exact bytes. Both are lossless: the bytes a
// caller reconstructs are the bytes that were stored.
func (c *PackageFileContent) EncodedContent() (string, error) {
	if c == nil {
		// A nil receiver is a CALLER error, not a substrate failure. The
		// reason vocabulary is closed and `database_failure` means "the
		// substrate failed", so this error is deliberately not one of
		// them: a caller branching on the reason must not conclude that
		// the database is broken.
		return "", errors.New("there is no content: the caller passed a nil content")
	}
	switch c.Encoding {
	case PackageReadEncodingUTF8:
		return string(c.Content), nil
	case PackageReadEncodingBase64:
		return base64.StdEncoding.EncodeToString(c.Content), nil
	default:
		return "", packageReadErrorf(PackageReadReasonIntegrityFailure, c.Path,
			"the content's transport encoding %q is not one this reader produces", c.Encoding)
	}
}

// The names of the integrity checks `verify` runs. They are stable, so a
// caller can branch on a check name rather than on prose.
const (
	// PackageVerifyCheckManifestHash — the canonical manifest matches the
	// recorded package hash.
	PackageVerifyCheckManifestHash = "manifest_package_hash"

	// PackageVerifyCheckManifestIdentity — the manifest and the package
	// row describe the same skill, version and entrypoint.
	PackageVerifyCheckManifestIdentity = "manifest_identity"

	// PackageVerifyCheckFileMetadata — the file rows match the manifest.
	PackageVerifyCheckFileMetadata = "file_metadata_matches_manifest"

	// PackageVerifyCheckBlobExists — every referenced blob exists.
	PackageVerifyCheckBlobExists = "blob_exists"

	// PackageVerifyCheckBlobHash — every stored blob's content matches
	// its recorded SHA-256.
	PackageVerifyCheckBlobHash = "blob_content_hash"

	// PackageVerifyCheckBlobSize — every stored size matches the actual
	// byte length.
	PackageVerifyCheckBlobSize = "blob_size"

	// PackageVerifyCheckEntrypoint — the entrypoint hash matches the
	// ORIGINAL SKILL.md bytes.
	PackageVerifyCheckEntrypoint = "entrypoint_hash"

	// PackageVerifyCheckProjection — the skill projection matches the
	// manifest's `projection_sha256`.
	PackageVerifyCheckProjection = "projection_hash"

	// PackageVerifyCheckVersion — the package and the projection identify
	// the expected skill version.
	PackageVerifyCheckVersion = "version_identity"
)

// PackageVerificationFailure names one integrity failure.
type PackageVerificationFailure struct {
	// Check is the stable name of the check that failed.
	Check string

	// Path is the package-relative path the failure is about, when it is
	// about one file.
	Path string

	// Detail is the human-readable description.
	Detail string
}

// PackageVerificationResult is the result of a package verification.
//
// A corrupt package is REPORTED, not refused: the caller asked whether the
// package is sound, and the answer is a structured list of what is not.
// Only "there is no such package", an unusable selector and a substrate
// failure are errors.
type PackageVerificationResult struct {
	// SkillID is the canonical id of the packaged skill.
	SkillID string

	// Name is the package's skill name.
	Name string

	// Version is the package's effective version.
	Version string

	// PackageHash is the hash recorded for the package.
	PackageHash string

	// FileCount is the number of file rows the package carries.
	FileCount int

	// Verified reports whether every check passed.
	//
	// It is false whenever there is at least one failure. A corrupt
	// package is NEVER reported as verified.
	Verified bool

	// Checks are the names of the checks that WERE RUN, in a stable
	// order. A check that could not run is not named: the manifest's
	// identity check runs only when the stored manifest parses, and
	// there is nothing to compare it against otherwise. The failure
	// list is therefore always a subset of this one.
	Checks []string

	// Failures are the integrity failures, in a stable order.
	Failures []PackageVerificationFailure
}

// SkillPackageProvenance is the §8.2.1 provenance object.
//
// It is present whenever the skill IS a package, and it is absent for an
// ordinary single-document skill. It carries the ORIGINAL entrypoint's
// hash — NOT the projection's — so a caller can check that the body it is
// looking at derives from the file the package recorded.
type SkillPackageProvenance struct {
	// Entrypoint is the canonical path of the package's entrypoint.
	Entrypoint string `json:"entrypoint"`

	// EntrypointSHA256 is the hash of the ORIGINAL entrypoint bytes, as
	// supplied (§5.2) — not the hash of the derived projection.
	EntrypointSHA256 string `json:"entrypoint_sha256"`

	// FileCount is the number of files the package carries.
	FileCount int `json:"file_count"`

	// PackageHash is the hash of the package's canonical manifest.
	PackageHash string `json:"package_hash"`

	// Scanned reports whether every scanner that applies to the entrypoint
	// actually ran and passed.
	//
	// It is NOT a constant: it is what the entrypoint's file row
	// records. A disclosed skip reports false.
	Scanned bool `json:"scanned"`

	// ScanStatus names WHICH scanners ran on the entrypoint.
	ScanStatus PackageScanStatus `json:"scan_status"`

	// VersionOverride records how the effective version was arrived at:
	// the declared version, the effective version and the reason.
	//
	// It is ALWAYS present for a packaged skill: the package row's
	// `version` and `version_source` columns are NOT NULL (§6.8), so the
	// record always exists — including when the stored manifest cannot
	// be parsed, which is the case the earlier, manifest-derived version
	// of this object got wrong.
	VersionOverride *PackageVersionOverride `json:"version_override,omitempty"`
}

// ─── The stored rows ───────────────────────────────────────────────────────

// packageRow is the raw `skill_packages` row.
//
// It is the UNVALIDATED view: `verify` needs it so it can report a
// damaged package rather than refuse to look at it.
type packageRow struct {
	SkillID          string
	Name             string
	Version          string
	DeclaredVersion  *string
	VersionSource    string
	Format           string
	Entrypoint       string
	EntrypointSHA256 string
	ManifestJSON     string
	PackageHash      string
}

// packageContext is the package row plus its VALIDATED manifest.
//
// Loading one is the fail-closed gate every read but `verify` passes
// through: the manifest must parse, must hash to the recorded package
// hash, and must agree with the package row about the skill, the version
// and the entrypoint.
type packageContext struct {
	Row      packageRow
	Manifest *PackageManifest
}

// packageBlob is the stored blob row.
type packageBlob struct {
	SHA256    string
	SizeBytes int64
	Content   []byte
}

// ─── The reader ────────────────────────────────────────────────────────────

// PackageFiles lists a package's stored files — metadata only, never
// content (§8.5 `files`).
//
// The listing is deterministic: the files are returned in canonical-path
// order, every time.
func (dm *DatabaseManager) PackageFiles(ctx context.Context, sel PackageSelector) (*PackageFileListing, error) {
	skillID, err := sel.resolveSkillID()
	if err != nil {
		return nil, err
	}

	var out *PackageFileListing
	err = dm.withPackageReadSnapshot(ctx, func(tx *sql.Tx) error {
		pkg, err := loadPackageContext(ctx, tx, skillID)
		if err != nil {
			return err
		}
		files, err := loadPackageFileRows(ctx, tx, skillID)
		if err != nil {
			return err
		}
		// The rows must agree with the manifest. A package whose rows and
		// manifest disagree is not a package this reader can describe.
		if failures := reconcilePackageFiles(pkg, files); len(failures) > 0 {
			return packageReadErrorf(PackageReadReasonIntegrityFailure, failures[0].Path,
				"the file rows do not match the manifest: %s", failures[0].Detail)
		}
		out = &PackageFileListing{
			SkillID:     pkg.Row.SkillID,
			Name:        pkg.Row.Name,
			Version:     pkg.Row.Version,
			PackageHash: pkg.Row.PackageHash,
			FileCount:   len(files),
			Files:       files,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PackageReadFile reads ONE file of a package, by canonical package-
// relative path (§8.5 `read_file`).
//
// The bytes returned are the ORIGINAL stored bytes — for the entrypoint,
// that is the original `SKILL.md`, not the derived projection. The blob is
// re-hashed on the way out, so a stored file whose bytes were mutated is
// refused with `content_hash_mismatch` rather than returned (T34).
func (dm *DatabaseManager) PackageReadFile(ctx context.Context, sel PackageSelector, path string, bounds PackageReadBounds) (*PackageFileContent, error) {
	skillID, err := sel.resolveSkillID()
	if err != nil {
		return nil, err
	}

	// The path is validated BEFORE anything is looked up. It is a
	// package-relative name and never a host filesystem path: a
	// traversing, absolute or malformed path is refused on its own terms.
	canonical, err := CanonicalPackagePath(path)
	if err != nil {
		return nil, packageReadErrorWrap(PackageReadReasonInvalidPath, path, err,
			"the requested path is not a canonical package path")
	}

	var out *PackageFileContent
	err = dm.withPackageReadSnapshot(ctx, func(tx *sql.Tx) error {
		pkg, err := loadPackageContext(ctx, tx, skillID)
		if err != nil {
			return err
		}
		row, err := loadPackageFileRow(ctx, tx, skillID, canonical)
		if err != nil {
			return err
		}
		if row == nil {
			return packageReadErrorf(PackageReadReasonFileNotFound, canonical,
				"the package carries no file at that path")
		}

		// The row is reconciled against the manifest BEFORE its bytes are
		// served. The blob is content-addressed, so re-hashing it proves
		// only that the bytes are the ones the ROW names; the manifest is
		// what commits the package to its file list. A row the manifest
		// does not confirm is refused, exactly as `files` refuses the
		// same package — the two paths must not differ.
		index := manifestFileIndex(pkg.Manifest.Files, canonical)
		if index < 0 {
			return packageReadErrorf(PackageReadReasonIntegrityFailure, canonical,
				"the package has a file at that path the manifest does not name")
		}
		if !packageFileMetadataEqual(pkg.Manifest.Files[index], *row) {
			return packageReadErrorf(PackageReadReasonIntegrityFailure, canonical,
				"the file row does not match the manifest's entry for it")
		}

		blob, err := loadPackageBlob(ctx, tx, row.SHA256)
		if err != nil {
			return err
		}
		if blob == nil {
			return packageReadErrorf(PackageReadReasonIntegrityFailure, canonical,
				"the stored blob %s is missing", row.SHA256)
		}
		// The bytes are re-hashed on the way out. The recorded hash is
		// not taken on trust, and the recorded size must describe the
		// bytes as well.
		if actual := sha256Hex(blob.Content); actual != row.SHA256 {
			return packageReadErrorf(PackageReadReasonContentHashMismatch, canonical,
				"the stored bytes hash to %s, not to the recorded %s", actual, row.SHA256)
		}
		if int64(len(blob.Content)) != row.SizeBytes || blob.SizeBytes != row.SizeBytes {
			return packageReadErrorf(PackageReadReasonContentHashMismatch, canonical,
				"the stored bytes are %d long, not the recorded %d", len(blob.Content), row.SizeBytes)
		}

		window, err := boundPackageRead(blob.Content, bounds)
		if err != nil {
			return err
		}

		content := packageBoundedContent(window)
		encoding := PackageReadEncodingUTF8
		if bounds.Raw {
			encoding = PackageReadEncodingBase64
		} else {
			// Without the raw form the content must be text-safe. A
			// binary asset, or bytes that are not valid UTF-8, is refused
			// rather than carried through a text field.
			if packageFileIsBinary(canonical) {
				return packageReadErrorf(PackageReadReasonBinaryRequiresRaw, canonical,
					"the file is binary content; raw=true is required to carry its bytes")
			}
			if !utf8.Valid(content) {
				return packageReadErrorf(PackageReadReasonBinaryRequiresRaw, canonical,
					"the file's bytes are not valid UTF-8; raw=true is required to carry its bytes")
			}
		}

		out = &PackageFileContent{
			SkillID:     pkg.Row.SkillID,
			Path:        canonical,
			SHA256:      row.SHA256,
			SizeBytes:   row.SizeBytes,
			ContentType: row.ContentType,
			Role:        row.Role,
			Executable:  row.Executable,
			Scanned:     row.Scanned,
			Source:      PackageReadSourceSkillPackage,
			Content:     content,
			Encoding:    encoding,
			Truncated:   window.Truncated,
			Offset:      bounds.Offset,
			NextOffset:  window.NextOffset,
			Lines:       len(window.Lines),
			Terminal:    window.Terminal,
			Reason:      window.Reason,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PackageVerify checks the stored package and reports what is wrong with
// it (§8.5 `verify`).
//
// It is READ-ONLY and it REPAIRS NOTHING: no row is rewritten, no blob is
// re-imported, no scanner is re-run, nothing is replaced. A corrupt
// package is reported as corrupt.
//
// The only errors are "there is no such package", an unusable selector and
// a substrate failure. Damage to the package itself is a RESULT, not an
// error — the caller asked whether the package is sound.
func (dm *DatabaseManager) PackageVerify(ctx context.Context, sel PackageSelector) (*PackageVerificationResult, error) {
	skillID, err := sel.resolveSkillID()
	if err != nil {
		return nil, err
	}

	var out *PackageVerificationResult
	err = dm.withPackageReadSnapshot(ctx, func(tx *sql.Tx) error {
		result, err := verifyPackageInTx(ctx, tx, skillID)
		if err != nil {
			return err
		}
		out = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SkillPackageProvenance returns the §8.2.1 provenance object for a skill.
//
// It returns (nil, nil) for a skill that is NOT a package: the object is
// absent for an ordinary skill, and its absence is not an error.
//
// The accessor is deliberately TOLERANT of a damaged package: `read` must
// keep returning the entrypoint's projection even when the package's
// manifest is broken — that is what `verify` is for. What it never does is
// claim a scan that was not recorded: the scan status is the entrypoint
// file row's own status, and a missing row reports no scan at all.
func (dm *DatabaseManager) SkillPackageProvenance(ctx context.Context, skillID string) (*SkillPackageProvenance, error) {
	if strings.TrimSpace(skillID) == "" {
		return nil, packageReadErrorf(PackageReadReasonSelectorInvalid, "",
			"a skill id is required")
	}
	if dm.db == nil {
		return nil, packageReadErrorf(PackageReadReasonDatabaseFailure, "",
			"the database is not initialized")
	}

	var (
		entrypoint      string
		entrypointSHA   string
		packageHash     string
		declaredVersion sql.NullString
		version         string
		versionSource   string
		fileCount       int
		secretScanned   sql.NullInt64
		poisonScanned   sql.NullInt64
	)
	err := dm.db.QueryRowContext(ctx, packageProvenanceQuery, skillID).Scan(
		&entrypoint, &entrypointSHA, &packageHash,
		&declaredVersion, &version, &versionSource,
		&fileCount, &secretScanned, &poisonScanned)
	if errors.Is(err, sql.ErrNoRows) {
		// An ordinary skill. Not an error: the object is simply absent.
		return nil, nil
	}
	if err != nil {
		return nil, packageReadErrorWrap(PackageReadReasonDatabaseFailure, "", err,
			"read the package provenance for %s", skillID)
	}

	provenance := &SkillPackageProvenance{
		Entrypoint:       entrypoint,
		EntrypointSHA256: entrypointSHA,
		FileCount:        fileCount,
		PackageHash:      packageHash,
		// The entrypoint's OWN scan status — the actual one, from the
		// file row, not a constant. A missing row reports no scan.
		ScanStatus: PackageScanStatus{
			Secret: secretScanned.Valid && secretScanned.Int64 != 0,
			Poison: poisonScanned.Valid && poisonScanned.Int64 != 0,
		},
	}
	provenance.Scanned = provenance.ScanStatus.Secret && provenance.ScanStatus.Poison

	// The version override is the package row's own version provenance:
	// the declared version, the effective version, and how the effective
	// one was arrived at. The importer writes it and the manifest's
	// `version_override` from the same plan, so the row carries the same
	// record — and reading it here costs no manifest parse.
	//
	// It is ALWAYS present for a packaged skill. The row's `version` and
	// `version_source` columns are NOT NULL (§6.8), so the record always
	// exists; the accessor does not test for emptiness, because a
	// packaged skill with no version provenance would be a row that
	// contradicts its own schema, and `verify` — not the provenance —
	// is where such a contradiction is judged.
	override := PackageVersionOverride{
		Effective: version,
		Reason:    versionSource,
	}
	if declaredVersion.Valid {
		declared := declaredVersion.String
		override.Declared = &declared
	}
	provenance.VersionOverride = &override
	return provenance, nil
}

// ─── The read snapshot ─────────────────────────────────────────────────────

// withPackageReadSnapshot runs fn against ONE consistent read snapshot.
//
// Every read in this file takes its metadata and its bytes from the SAME
// snapshot, so a package row and the blob it references can never come
// from two different database states.
//
// The transaction is DEFERRED and read-only: it takes a shared snapshot on
// its first read and holds it until it is released, and it never takes a
// write lock. It is released by the deferred rollback — there is nothing
// to commit, because a read writes nothing.
//
// A NOTE ON `ReadOnly`. The transaction is opened with
// `sql.TxOptions{ReadOnly: true}`, but the SQLite driver
// (mattn/go-sqlite3) IGNORES that option and issues a plain `BEGIN` —
// which is deferred, and therefore takes a shared lock on its first read
// and no write lock at all. The read-only nature of the snapshot
// therefore rests on DISCIPLINE, not on the driver's enforcement.
//
// That discipline is structural rather than merely asserted: the
// transaction handle is only ever passed to `QueryContext` and
// `QueryRowContext`, the reader's SQL is a fixed set of `SELECT`
// constants, and `TestP6TheReaderNeverWrites` pins both facts on the
// source. The census in `TestP6ReadsChangeNothing` then pins the
// behaviour: after every read operation, the whole substrate is
// unchanged.
//
// The ONE reader that does not use this helper is
// `SkillPackageProvenance`: its read is a SINGLE statement, and a single
// statement sees ONE consistent snapshot on its own — so it needs no
// transaction, and the `read` action's hot path pays for neither a
// connection checkout nor a BEGIN/ROLLBACK.
func (dm *DatabaseManager) withPackageReadSnapshot(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if dm.db == nil {
		return packageReadErrorf(PackageReadReasonDatabaseFailure, "",
			"the database is not initialized")
	}
	tx, err := dm.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return packageReadErrorWrap(PackageReadReasonDatabaseFailure, "", err,
			"open a read snapshot")
	}
	defer tx.Rollback()
	return fn(tx)
}

// ─── The row readers ───────────────────────────────────────────────────────

// loadPackageRow reads the raw package row.
//
// A missing row is `package_not_found` — the one refusal the reader
// returns for an absent package.
func loadPackageRow(ctx context.Context, tx *sql.Tx, skillID string) (*packageRow, error) {
	var (
		row             packageRow
		declaredVersion sql.NullString
	)
	err := tx.QueryRowContext(ctx, packageRowQuery, skillID).Scan(
		&row.SkillID, &row.Name, &row.Version, &declaredVersion, &row.VersionSource,
		&row.Format, &row.Entrypoint, &row.EntrypointSHA256, &row.ManifestJSON,
		&row.PackageHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, packageReadErrorf(PackageReadReasonPackageNotFound, "",
			"no package is published for %s", skillID)
	}
	if err != nil {
		return nil, packageReadErrorWrap(PackageReadReasonDatabaseFailure, "", err,
			"read the package row for %s", skillID)
	}
	if declaredVersion.Valid {
		v := declaredVersion.String
		row.DeclaredVersion = &v
	}
	return &row, nil
}

// loadPackageContext reads the package row and VALIDATES its manifest.
//
// This is the fail-closed gate: a manifest that does not parse, that does
// not hash to the recorded package hash, or that disagrees with the
// package row about the skill, the version or the entrypoint is an
// `integrity_failure`. Nothing is repaired and nothing is reinterpreted.
func loadPackageContext(ctx context.Context, tx *sql.Tx, skillID string) (*packageContext, error) {
	row, err := loadPackageRow(ctx, tx, skillID)
	if err != nil {
		return nil, err
	}

	manifest, _, err := ParsePackageManifest([]byte(row.ManifestJSON))
	if err != nil {
		return nil, packageReadErrorWrap(PackageReadReasonIntegrityFailure, "", err,
			"the stored manifest of %s could not be parsed", skillID)
	}
	// The package-hash rule is the manifest module's, not this file's:
	// VerifyPackageHash re-derives the canonical hash and compares it
	// with the hash the manifest claims.
	recomputed, err := VerifyPackageHash([]byte(row.ManifestJSON))
	if err != nil {
		return nil, packageReadErrorWrap(PackageReadReasonIntegrityFailure, "", err,
			"the stored manifest of %s does not describe its own content", skillID)
	}
	if recomputed != row.PackageHash {
		return nil, packageReadErrorf(PackageReadReasonIntegrityFailure, "",
			"the stored manifest of %s hashes to %s, not to the recorded %s",
			skillID, recomputed, row.PackageHash)
	}

	// The manifest and the package row must describe the same package.
	if manifest.Name != row.Name || manifest.Version != row.Version {
		return nil, packageReadErrorf(PackageReadReasonIntegrityFailure, "",
			"the manifest of %s describes %s v%s, the package row describes %s v%s",
			skillID, manifest.Name, manifest.Version, row.Name, row.Version)
	}
	if manifest.Format != row.Format {
		return nil, packageReadErrorf(PackageReadReasonIntegrityFailure, "",
			"the manifest of %s declares the format %q, the package row records %q",
			skillID, manifest.Format, row.Format)
	}
	if !packageDeclaredVersionEqual(manifest.DeclaredVersion, row.DeclaredVersion) {
		return nil, packageReadErrorf(PackageReadReasonIntegrityFailure, "",
			"the manifest of %s and its package row disagree about the declared version",
			skillID)
	}
	if manifest.Entrypoint != row.Entrypoint || manifest.EntrypointSHA256 != row.EntrypointSHA256 {
		return nil, packageReadErrorf(PackageReadReasonIntegrityFailure, "",
			"the manifest of %s and its package row disagree about the entrypoint", skillID)
	}
	if manifest.VersionOverride.Effective != row.Version {
		return nil, packageReadErrorf(PackageReadReasonIntegrityFailure, "",
			"the manifest of %s records the effective version %q, the package row records %q",
			skillID, manifest.VersionOverride.Effective, row.Version)
	}
	if manifest.VersionOverride.Reason != row.VersionSource {
		return nil, packageReadErrorf(PackageReadReasonIntegrityFailure, "",
			"the manifest of %s records the version source %q, the package row records %q",
			skillID, manifest.VersionOverride.Reason, row.VersionSource)
	}

	return &packageContext{Row: *row, Manifest: manifest}, nil
}

// loadPackageFileRows reads a package's file rows, in canonical-path
// order.
//
// The ORDER BY is what makes the listing deterministic: the same package
// always lists its files in the same order.
func loadPackageFileRows(ctx context.Context, tx *sql.Tx, skillID string) ([]PackageManifestFile, error) {
	rows, err := tx.QueryContext(ctx, packageFilesQuery, skillID)
	if err != nil {
		return nil, packageReadErrorWrap(PackageReadReasonDatabaseFailure, "", err,
			"read the file rows of %s", skillID)
	}
	defer rows.Close()

	out := []PackageManifestFile{}
	for rows.Next() {
		f, err := scanPackageFileRow(rows)
		if err != nil {
			return nil, packageReadErrorWrap(PackageReadReasonDatabaseFailure, "", err,
				"read a file row of %s", skillID)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, packageReadErrorWrap(PackageReadReasonDatabaseFailure, "", err,
			"iterate the file rows of %s", skillID)
	}
	return out, nil
}

// loadPackageFileRow reads ONE file row, or nil when the package carries
// no file at that path.
func loadPackageFileRow(ctx context.Context, tx *sql.Tx, skillID, canonicalPath string) (*PackageManifestFile, error) {
	row := tx.QueryRowContext(ctx, packageFileQuery, skillID, canonicalPath)
	f, err := scanPackageFileRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, packageReadErrorWrap(PackageReadReasonDatabaseFailure, "", err,
			"read the file row %s of %s", canonicalPath, skillID)
	}
	return &f, nil
}

// scanPackageFileRow reads one file row's columns.
//
// The scan statuses are the ACTUAL statuses the import recorded — which
// scanners ran — not an assumption.
func scanPackageFileRow(row interface{ Scan(dest ...any) error }) (PackageManifestFile, error) {
	var (
		f          PackageManifestFile
		role       string
		executable int
		secret     int
		poison     int
	)
	if err := row.Scan(&f.Path, &f.SHA256, &f.SizeBytes, &f.ContentType,
		&role, &executable, &secret, &poison); err != nil {
		return PackageManifestFile{}, err
	}
	f.Role = PackageRole(role)
	f.Executable = executable != 0
	f.Scanned = PackageScanStatus{Secret: secret != 0, Poison: poison != 0}
	return f, nil
}

// loadPackageBlob reads one blob, or nil when the blob row is gone.
func loadPackageBlob(ctx context.Context, tx *sql.Tx, sha256 string) (*packageBlob, error) {
	var blob packageBlob
	err := tx.QueryRowContext(ctx, packageBlobQuery, sha256).Scan(
		&blob.SHA256, &blob.SizeBytes, &blob.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, packageReadErrorWrap(PackageReadReasonDatabaseFailure, "", err,
			"read the stored blob %s", sha256)
	}
	return &blob, nil
}

// ─── The bounded read ──────────────────────────────────────────────────────

// packageBoundedWindow is the outcome of a bounded read.
type packageBoundedWindow struct {
	// Lines are the lines the read returns.
	Lines [][]byte

	// Truncated reports that the file was not fully returned.
	Truncated bool

	// NextOffset is the index of the first line that was not returned, or
	// nil when the read is complete.
	NextOffset *int

	// Terminal reports that the response cannot be resumed.
	Terminal bool

	// Reason is the classification of a terminal response.
	Reason PackageReadReason
}

// boundPackageRead selects the lines a bounded read returns (§8.6).
//
// The contract, exactly:
//
//   - `offset` and `limit` count LINES; `max_bytes` counts BYTES.
//   - The byte cap is applied AFTER the line range is selected, and it
//     never selects a different range.
//   - A line cut in half by the byte cap is NOT returned. The cap
//     truncates at a line boundary and the line that would have crossed
//     it is left for the next call.
//   - `next_offset` is the index of the FIRST line that was not returned.
//   - `next_offset` is nil when the read is complete.
//   - EVERY non-terminal truncated response advances the offset:
//     `next_offset > offset`.
//   - The TERMINAL case — the first requested line alone exceeds the cap —
//     returns zero lines, `truncated: true`, `next_offset == offset` and
//     the reason `line_too_large`. It is the ONLY response that may carry
//     an unchanged `next_offset`, and it is never produced for any other
//     reason.
func boundPackageRead(content []byte, bounds PackageReadBounds) (*packageBoundedWindow, error) {
	if bounds.Offset < 0 {
		return nil, packageReadErrorf(PackageReadReasonInvalidBounds, "",
			"offset %d is negative", bounds.Offset)
	}
	if bounds.Limit < 0 {
		return nil, packageReadErrorf(PackageReadReasonInvalidBounds, "",
			"limit %d is negative", bounds.Limit)
	}
	maxBytes := bounds.MaxBytes
	if maxBytes < 0 {
		return nil, packageReadErrorf(PackageReadReasonInvalidBounds, "",
			"max_bytes %d is negative", maxBytes)
	}
	if maxBytes > PackageReadMaxBytesLimit {
		return nil, packageReadErrorf(PackageReadReasonInvalidBounds, "",
			"max_bytes %d is above the %d byte hard cap", maxBytes, PackageReadMaxBytesLimit)
	}
	if maxBytes == 0 {
		maxBytes = PackageReadDefaultMaxBytes
	}

	lines := splitPackageLines(content)
	if bounds.Offset > len(lines) {
		return nil, packageReadErrorf(PackageReadReasonInvalidBounds, "",
			"offset %d is beyond the end of the file, which has %d lines",
			bounds.Offset, len(lines))
	}

	// The line range the request selects, before the byte cap is applied.
	//
	// The limit is compared against the number of REMAINING lines rather
	// than added to the offset, so an absurdly large limit cannot overflow
	// the addition and silently select an empty range.
	end := len(lines)
	if bounds.Limit > 0 {
		remaining := len(lines) - bounds.Offset
		if bounds.Limit < remaining {
			end = bounds.Offset + bounds.Limit
		}
	}

	// The byte cap, applied at a line boundary.
	total := 0
	selected := 0
	for i := bounds.Offset; i < end; i++ {
		n := len(lines[i])
		if total+n > maxBytes {
			break
		}
		total += n
		selected++
	}

	// The terminal case: the FIRST requested line did not fit under the
	// cap. Zero lines, truncated, next_offset == offset, and the reason
	// code that says so.
	if selected == 0 && end > bounds.Offset {
		next := bounds.Offset
		return &packageBoundedWindow{
			Truncated:  true,
			NextOffset: &next,
			Terminal:   true,
			Reason:     PackageReadReasonLineTooLarge,
		}, nil
	}

	next := bounds.Offset + selected
	window := &packageBoundedWindow{Lines: lines[bounds.Offset:next]}
	if next < len(lines) {
		// There are unread lines: the response is truncated and the caller
		// resumes at `next`, which is strictly greater than the offset it
		// asked for.
		window.Truncated = true
		window.NextOffset = &next
	}
	return window, nil
}

// splitPackageLines splits content into lines, KEEPING each line's
// terminator.
//
// The rules, exactly:
//
//   - "" has zero lines;
//   - "a\nb" has the lines "a\n" and "b";
//   - "\n" has the single line "\n";
//   - "a\nb\n" has the lines "a\n" and "b\n" — the final newline
//     terminates the last line rather than starting an empty one;
//   - CRLF is preserved: "a\r\n" is ONE line, terminator included.
//
// Keeping the terminator is what makes the concatenation of successive
// windows reproduce the file byte for byte (T47): nothing is normalised
// away on the way through.
func splitPackageLines(content []byte) [][]byte {
	if len(content) == 0 {
		return nil
	}
	lines := make([][]byte, 0, 8)
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] != '\n' {
			continue
		}
		lines = append(lines, content[start:i+1])
		start = i + 1
	}
	if start < len(content) {
		lines = append(lines, content[start:])
	}
	return lines
}

// packageBoundedContent concatenates the window's lines.
//
// The result is the EXACT bytes of those lines, terminators included: the
// concatenation of every successive window reproduces the file.
func packageBoundedContent(window *packageBoundedWindow) []byte {
	if window == nil || len(window.Lines) == 0 {
		return []byte{}
	}
	total := 0
	for _, line := range window.Lines {
		total += len(line)
	}
	out := make([]byte, 0, total)
	for _, line := range window.Lines {
		out = append(out, line...)
	}
	return out
}

// packageFileIsBinary reports whether a stored file is binary content.
//
// The class comes from the SAME allow-list the importer used, so the
// reader and the writer agree on what "binary" means. A path the
// allow-list does not know is treated as binary: the safe direction,
// because it refuses a text rendering the reader cannot guarantee.
func packageFileIsBinary(canonicalPath string) bool {
	_, class, _, err := packageContentTypeFor(canonicalPath)
	if err != nil {
		return true
	}
	return class == packageContentBinary || class == packageContentExecutable
}

// ─── The verification ──────────────────────────────────────────────────────

// reconcilePackageFiles compares the file rows against the manifest.
//
// It returns the failures it finds, in a deterministic order: the
// manifest's files first, then the rows the manifest does not name.
//
// The comparison is on METADATA only. The bytes are checked separately,
// by the blob checks.
func reconcilePackageFiles(pkg *packageContext, rows []PackageManifestFile) []PackageVerificationFailure {
	var failures []PackageVerificationFailure

	byPath := make(map[string]PackageManifestFile, len(rows))
	for i := range rows {
		byPath[rows[i].Path] = rows[i]
	}

	for i := range pkg.Manifest.Files {
		manifestFile := pkg.Manifest.Files[i]
		row, ok := byPath[manifestFile.Path]
		if !ok {
			failures = append(failures, PackageVerificationFailure{
				Check:  PackageVerifyCheckFileMetadata,
				Path:   manifestFile.Path,
				Detail: "the manifest names a file the package has no row for",
			})
			continue
		}
		if !packageFileMetadataEqual(manifestFile, row) {
			failures = append(failures, PackageVerificationFailure{
				Check:  PackageVerifyCheckFileMetadata,
				Path:   manifestFile.Path,
				Detail: "the file row does not match the manifest's entry for it",
			})
		}
	}

	for i := range rows {
		if manifestFileIndex(pkg.Manifest.Files, rows[i].Path) >= 0 {
			continue
		}
		failures = append(failures, PackageVerificationFailure{
			Check:  PackageVerifyCheckFileMetadata,
			Path:   rows[i].Path,
			Detail: "the package has a file row the manifest does not name",
		})
	}

	return failures
}

// manifestFileIndex returns the index of the manifest entry for a path, or
// -1 when the manifest does not name it.
func manifestFileIndex(files []PackageManifestFile, path string) int {
	for i := range files {
		if files[i].Path == path {
			return i
		}
	}
	return -1
}

// packageFileMetadataEqual reports whether a manifest entry and a file row
// describe the same file.
func packageFileMetadataEqual(a, b PackageManifestFile) bool {
	return a.Path == b.Path &&
		a.SHA256 == b.SHA256 &&
		a.SizeBytes == b.SizeBytes &&
		a.ContentType == b.ContentType &&
		a.Role == b.Role &&
		a.Executable == b.Executable &&
		a.Scanned == b.Scanned
}

// packageDeclaredVersionEqual reports whether the manifest's declared
// version and the package row's declared version are the same.
//
// A package that declares no version has NO declared version in either
// place, so nil and nil agree — and nil against a value does not.
func packageDeclaredVersionEqual(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// verifyPackageInTx runs the integrity checks over one package.
//
// It reports damage; it never repairs it, and it never refuses to look at
// a damaged package.
func verifyPackageInTx(ctx context.Context, tx *sql.Tx, skillID string) (*PackageVerificationResult, error) {
	row, err := loadPackageRow(ctx, tx, skillID)
	if err != nil {
		return nil, err
	}

	result := &PackageVerificationResult{
		SkillID:     row.SkillID,
		Name:        row.Name,
		Version:     row.Version,
		PackageHash: row.PackageHash,
	}
	fail := func(check, path, detail string) {
		result.Failures = append(result.Failures, PackageVerificationFailure{
			Check:  check,
			Path:   path,
			Detail: detail,
		})
	}

	// ── The manifest. ──────────────────────────────────────────────────
	var manifest *PackageManifest
	parsed, _, perr := ParsePackageManifest([]byte(row.ManifestJSON))
	switch {
	case perr != nil:
		fail(PackageVerifyCheckManifestHash, "",
			"the stored manifest could not be parsed: "+perr.Error())
	default:
		manifest = parsed
		recomputed, herr := VerifyPackageHash([]byte(row.ManifestJSON))
		switch {
		case herr != nil:
			fail(PackageVerifyCheckManifestHash, "",
				"the stored manifest does not describe its own content: "+herr.Error())
		case recomputed != row.PackageHash:
			fail(PackageVerifyCheckManifestHash, "",
				"the stored manifest hashes to "+recomputed+", not to the recorded "+row.PackageHash)
		}
		if parsed.Name != row.Name || parsed.Version != row.Version {
			fail(PackageVerifyCheckManifestIdentity, "",
				"the manifest describes "+parsed.Name+" v"+parsed.Version+
					", the package row describes "+row.Name+" v"+row.Version)
		}
		if parsed.Entrypoint != row.Entrypoint {
			fail(PackageVerifyCheckManifestIdentity, parsed.Entrypoint,
				"the manifest names the entrypoint "+parsed.Entrypoint+
					", the package row names "+row.Entrypoint)
		}
		if parsed.EntrypointSHA256 != row.EntrypointSHA256 {
			fail(PackageVerifyCheckManifestIdentity, row.Entrypoint,
				"the manifest and the package row disagree about the entrypoint's hash")
		}
		if parsed.Format != row.Format {
			fail(PackageVerifyCheckManifestIdentity, "",
				"the manifest declares the format "+parsed.Format+
					", the package row records "+row.Format)
		}
		if !packageDeclaredVersionEqual(parsed.DeclaredVersion, row.DeclaredVersion) {
			fail(PackageVerifyCheckManifestIdentity, "",
				"the manifest and the package row disagree about the declared version")
		}
		if parsed.VersionOverride.Effective != row.Version {
			fail(PackageVerifyCheckManifestIdentity, "",
				"the manifest records the effective version "+parsed.VersionOverride.Effective+
					", the package row records "+row.Version)
		}
		if parsed.VersionOverride.Reason != row.VersionSource {
			fail(PackageVerifyCheckManifestIdentity, "",
				"the manifest records the version source "+parsed.VersionOverride.Reason+
					", the package row records "+row.VersionSource)
		}
	}

	// ── The file rows. ──────────────────────────────────────────────────
	rows, err := loadPackageFileRows(ctx, tx, skillID)
	if err != nil {
		return nil, err
	}
	result.FileCount = len(rows)
	if manifest != nil {
		pkg := &packageContext{Row: *row, Manifest: manifest}
		for _, failure := range reconcilePackageFiles(pkg, rows) {
			fail(failure.Check, failure.Path, failure.Detail)
		}
	} else {
		fail(PackageVerifyCheckFileMetadata, "",
			"the manifest could not be parsed, so the file rows cannot be checked against it")
	}

	// ── The blobs. ──────────────────────────────────────────────────────
	for i := range rows {
		file := rows[i]
		blob, err := loadPackageBlob(ctx, tx, file.SHA256)
		if err != nil {
			return nil, err
		}
		if blob == nil {
			fail(PackageVerifyCheckBlobExists, file.Path,
				"the stored blob "+file.SHA256+" is missing")
			continue
		}
		if int64(len(blob.Content)) != blob.SizeBytes {
			fail(PackageVerifyCheckBlobSize, file.Path,
				"the blob row records "+strconv.FormatInt(blob.SizeBytes, 10)+" bytes, the content is "+
					strconv.FormatInt(int64(len(blob.Content)), 10))
		}
		if int64(len(blob.Content)) != file.SizeBytes {
			fail(PackageVerifyCheckBlobSize, file.Path,
				"the file row records "+strconv.FormatInt(file.SizeBytes, 10)+" bytes, the content is "+
					strconv.FormatInt(int64(len(blob.Content)), 10))
		}
		if actual := sha256Hex(blob.Content); actual != file.SHA256 {
			fail(PackageVerifyCheckBlobHash, file.Path,
				"the stored bytes hash to "+actual+", not to the recorded "+file.SHA256)
		}
	}

	// ── The entrypoint. ─────────────────────────────────────────────────
	// The entrypoint's hash must describe the ORIGINAL SKILL.md bytes, not
	// the derived projection.
	var entry *PackageManifestFile
	for i := range rows {
		if rows[i].Path == row.Entrypoint {
			entry = &rows[i]
			break
		}
	}
	switch {
	case entry == nil:
		fail(PackageVerifyCheckEntrypoint, row.Entrypoint,
			"the package has no file row for its entrypoint")
	default:
		blob, err := loadPackageBlob(ctx, tx, entry.SHA256)
		if err != nil {
			return nil, err
		}
		switch {
		case blob == nil:
			fail(PackageVerifyCheckEntrypoint, row.Entrypoint,
				"the entrypoint's stored blob "+entry.SHA256+" is missing")
		default:
			if actual := sha256Hex(blob.Content); actual != row.EntrypointSHA256 {
				fail(PackageVerifyCheckEntrypoint, row.Entrypoint,
					"the entrypoint's bytes hash to "+actual+", not to the recorded "+
						row.EntrypointSHA256)
			}
		}
	}

	// ── The projection. ─────────────────────────────────────────────────
	var (
		projection sql.NullString
		metadata   sql.NullString
	)
	err = tx.QueryRowContext(ctx, packageProjectionQuery, skillID).Scan(&projection, &metadata)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		fail(PackageVerifyCheckProjection, "",
			"the packaged skill has no row in memories")
		fail(PackageVerifyCheckVersion, "",
			"the packaged skill has no row in memories, so it identifies no version")
	case err != nil:
		return nil, packageReadErrorWrap(PackageReadReasonDatabaseFailure, "", err,
			"read the projection of %s", skillID)
	default:
		if manifest == nil {
			fail(PackageVerifyCheckProjection, "",
				"the manifest could not be parsed, so the projection's hash cannot be checked")
		} else if actual := sha256Hex([]byte(projection.String)); actual != manifest.ProjectionSHA256 {
			fail(PackageVerifyCheckProjection, "",
				"the stored projection hashes to "+actual+", not to the recorded "+
					manifest.ProjectionSHA256)
		}
	}

	// ── The version identity. ───────────────────────────────────────────
	name, version, idErr := ParseNameAndVersionFromID(skillID)
	switch {
	case idErr != nil:
		fail(PackageVerifyCheckVersion, "",
			"the skill id does not identify a name and a version: "+idErr.Error())
	default:
		if name != row.Name {
			fail(PackageVerifyCheckVersion, "",
				"the skill id names "+name+", the package row names "+row.Name)
		}
		if version != row.Version {
			fail(PackageVerifyCheckVersion, "",
				"the skill id names version "+version+", the package row records "+row.Version)
		}
	}
	if metadata.Valid {
		var meta map[string]any
		if json.Unmarshal([]byte(metadata.String), &meta) == nil {
			if recorded, ok := meta["version"].(string); ok && recorded != "" && recorded != row.Version {
				fail(PackageVerifyCheckVersion, "",
					"the projection records version "+recorded+", the package records "+row.Version)
			}
		}
	}

	// The checks that RAN, in a stable order. The manifest's identity
	// check runs only when the stored manifest parses: there is nothing
	// to compare it against otherwise. It is not named when it did not
	// run, so the failure list is always a subset of this one.
	result.Checks = verifyChecksRun(manifest != nil)

	result.Verified = len(result.Failures) == 0
	return result, nil
}

// verifyChecksRun returns the names of the checks that ran, in a stable
// order.
//
// The manifest's identity check runs ONLY when the stored manifest
// parses: there is nothing to compare it against otherwise, so it is
// not named when it did not run.
func verifyChecksRun(manifestParsed bool) []string {
	checks := []string{PackageVerifyCheckManifestHash}
	if manifestParsed {
		checks = append(checks, PackageVerifyCheckManifestIdentity)
	}
	return append(checks,
		PackageVerifyCheckFileMetadata,
		PackageVerifyCheckBlobExists,
		PackageVerifyCheckBlobHash,
		PackageVerifyCheckBlobSize,
		PackageVerifyCheckEntrypoint,
		PackageVerifyCheckProjection,
		PackageVerifyCheckVersion,
	)
}
