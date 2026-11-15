package internal

// skill_package_export.go — Stage P7: the deterministic `.skill` export
// writer.
//
// Design of record: PR #222 §5.6 (import and export fidelity), §6.3
// (the export contract: no silent loss) and §10 (stage P7).
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT THIS IS
//
// A `.skill` archive is the TRANSPORT for a skill package: the bytes a
// package is moved in. Two exports of the same package must therefore
// produce the SAME BYTES, or the transport has no identity of its own
// and an operator cannot tell a faithful copy from a tampered one.
//
// The archive is in the FLAT layout (§5.6): the canonical paths sit at
// the archive's top level, so the entrypoint `SKILL.md` is at the
// archive's top level, and §5.3.1's rule 4 makes the archive root the
// package root. The P5 importer therefore accepts the archive this
// writer produces BY CONSTRUCTION, whatever content directories the
// package carries.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT MAKES IT DETERMINISTIC
//
// Every field that could vary between two exports of the same package
// is pinned:
//
//   - the FILE ORDER: `SKILL.md` first, then the other canonical paths
//     sorted;
//   - the TIMESTAMP: every entry carries 1980-01-01 00:00:00 UTC — the
//     ZIP epoch, and the earliest instant the DOS timestamp can carry;
//   - the PERMISSIONS: 0644, and 0755 for a file the manifest marks
//     executable;
//   - the COMPRESSION: `Store` — no compression at all;
//   - the HEADER FIELDS: no extra field of the caller's own, no
//     comment, no archive comment.
//
// The compression choice is LOAD-BEARING, and it is measured rather than
// assumed. Go's DEFLATE output is NOT stable across Go releases: the
// same bytes compressed by Go 1.26.6 and by Go 1.27.3 produce archives
// of different lengths and different digests. A deflate archive's
// identity would therefore depend on the TOOLCHAIN that produced it,
// which defeats the purpose. `Store` makes the archive's bytes a
// function of the package's bytes and the pinned header fields alone,
// so the same package exports to the same bytes on every Go release.
//
// The cost is size: an archive is the sum of its files rather than a
// compressed fraction of it. A skill package is a handful of text files,
// and the archive's own SHA-256 is the thing an operator verifies.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT IS NEVER EXPORTED
//
// A package is VERIFIED before it is exported, in the SAME snapshot the
// bytes are read from. A missing blob, a corrupted hash, a mismatched
// size, an invalid manifest, a projection that contradicts the manifest
// — any of them refuses the export. There is no "export what is there"
// mode: an archive is a claim that these are the package's bytes, and a
// damaged package cannot make that claim.
//
// The exporter never REPAIRS, never MUTATES and never RESCANS. It reads
// the rows the importer wrote and serialises them.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// skillDocumentContentQuery reads a skill's document and its shred
// stamp.
//
// It is the legacy path's read: an ordinary skill has no package row,
// so its document IS its content.
const skillDocumentContentQuery = `
	SELECT content, deleted_at
	FROM memories
	WHERE id = ? AND collection = 'skills'
`

// ─── The exported archive ──────────────────────────────────────────────────

// PackageExportArchive is a package rendered as a deterministic `.skill`
// archive.
//
// It carries the archive's BYTES and the metadata an operator needs to
// check them: the archive's own SHA-256, the package hash it came from,
// and the file name it should be published under.
type PackageExportArchive struct {
	// SkillID is the canonical `skill:<name>-v<version>` id.
	SkillID string `json:"skill_id"`

	// Name is the package's skill name.
	Name string `json:"name"`

	// Version is the package's effective version.
	Version string `json:"version"`

	// Entrypoint is the canonical path of the package's entrypoint.
	Entrypoint string `json:"entrypoint"`

	// EntrypointSHA256 is the hash of the ORIGINAL entrypoint bytes —
	// the bytes the archive carries, not the derived projection.
	EntrypointSHA256 string `json:"entrypoint_sha256"`

	// PackageHash is the hash of the package's canonical manifest. It
	// is what a re-import must reproduce.
	PackageHash string `json:"package_hash"`

	// FileCount is the number of files the archive carries.
	FileCount int `json:"file_count"`

	// FileName is the archive's published file name — `<name>-<version>.skill`.
	FileName string `json:"file_name"`

	// SHA256 is the digest of the archive's bytes.
	SHA256 string `json:"sha256"`

	// SizeBytes is the length of the archive's bytes.
	SizeBytes int64 `json:"size_bytes"`

	// Content is the archive itself.
	Content []byte `json:"-"`
}

// packageExportFixedTime is the timestamp every exported entry carries.
//
// It is the ZIP epoch. The DOS timestamp in a ZIP header cannot express
// anything earlier, and a timestamp of "now" would make every export
// different from every other.
var packageExportFixedTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// packageExportFileMode is the mode a non-executable entry carries.
const packageExportFileMode = 0o644

// packageExportExecutableMode is the mode an executable entry carries.
//
// The mode is documentation, not identity: the importer derives a
// file's executable flag from its EXTENSION (§7.3's allow-list), not
// from the archive's mode bits. It is pinned anyway, so the archive's
// bytes do not depend on a default.
const packageExportExecutableMode = 0o755

// packageExportEntry is one file in the archive.
type packageExportEntry struct {
	// Path is the CANONICAL path — the path relative to the package
	// root, which in the flat layout is the path relative to the
	// archive's root.
	Path string

	// Content is the file's bytes — the ORIGINAL bytes, never the
	// derived projection.
	Content []byte

	// Executable mirrors the manifest's executable flag.
	Executable bool
}

// ─── The writer ────────────────────────────────────────────────────────────

// writePackageArchive serialises the entries as a deterministic ZIP.
//
// The order is the contract: the entrypoint first, then the other
// canonical paths sorted. The layout is flat: every path sits at the
// archive's top level.
//
// The caller has already verified the package. This function only
// serialises.
func writePackageArchive(entries []packageExportEntry) ([]byte, error) {
	ordered := make([]packageExportEntry, len(entries))
	copy(ordered, entries)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Path == packageEntrypointName {
			return true
		}
		if ordered[j].Path == packageEntrypointName {
			return false
		}
		return ordered[i].Path < ordered[j].Path
	})

	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for i := range ordered {
		entry := &ordered[i]
		mode := os.FileMode(packageExportFileMode)
		if entry.Executable {
			mode = packageExportExecutableMode
		}
		header := &zip.FileHeader{
			Name:   entry.Path,
			Method: zip.Store,
			// The pinned timestamp. The ZIP writer converts it to the
			// DOS form in the header's own time zone, so it is given
			// in UTC explicitly.
			Modified: packageExportFixedTime,
		}
		header.SetMode(mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			return nil, fmt.Errorf("package export: create the archive entry %q: %w", entry.Path, err)
		}
		if _, err := file.Write(entry.Content); err != nil {
			return nil, fmt.Errorf("package export: write the archive entry %q: %w", entry.Path, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("package export: close the archive: %w", err)
	}
	return buf.Bytes(), nil
}

// ─── The export API ────────────────────────────────────────────────────────

// ExportSkillPackage exports ONE package as a deterministic `.skill`
// archive and returns it.
//
// The package is identified by its canonical skill id, or by its name
// AND its version — the same selector the reader takes, so a name alone
// never picks a version silently.
//
// The archive is NOT written to disk here: the caller decides where it
// goes and how it is published. This is the internal interface P8's
// `mpm skill export` calls.
//
// The package is VERIFIED before it is serialised, and the verification
// and the serialisation read the SAME snapshot — so the bytes that were
// verified are the bytes that are exported.
func (dm *DatabaseManager) ExportSkillPackage(ctx context.Context, sel PackageSelector) (*PackageExportArchive, error) {
	skillID, err := sel.resolveSkillID()
	if err != nil {
		return nil, err
	}
	if dm.db == nil {
		return nil, packageReadErrorf(PackageReadReasonDatabaseFailure, "",
			"the database is not initialized")
	}

	// The SINGLE-package export propagates the refusal. It does not go
	// through the batch's per-package omission handling, because a
	// caller that asked for ONE package wants to know WHY it could not
	// be exported, not that it was quietly set aside.
	var archive *PackageExportArchive
	err = dm.withPackageReadSnapshot(ctx, func(tx *sql.Tx) error {
		a, err := exportPackageInTx(ctx, tx, skillID)
		if err != nil {
			return err
		}
		archive = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	return archive, nil
}

// ExportSkillPackages exports a SET of packages as deterministic
// `.skill` archives.
//
// The given ids are expected to be PACKAGES — the caller selects them
// from the packages in scope, not from the skills in the export. An
// ordinary skill is not a package, so it is not offered here.
//
// Every package is read, verified and serialised inside ONE snapshot,
// so the whole set describes ONE database state. A package that fails
// verification is NOT exported, and it is named in the omissions with
// its reason — the export never silently drops a package. A package
// whose row has gone since the scope was computed is named too, with
// the reason `package_not_found`: it was selected, and it is not
// carried.
//
// The returned omissions are the packages that were selected and not
// exported. The caller decides what to do with them; the CLI turns them
// into the loss marker.
func (dm *DatabaseManager) ExportSkillPackages(ctx context.Context, skillIDs []string) ([]*PackageExportArchive, []SkillPackageExportOmitted, error) {
	if dm.db == nil {
		return nil, nil, packageReadErrorf(PackageReadReasonDatabaseFailure, "",
			"the database is not initialized")
	}
	archives, omitted, err := dm.exportSkillPackagesInTx(ctx, skillIDs)
	if err != nil {
		return nil, nil, err
	}
	return archives, omitted, nil
}

// exportSkillPackagesInTx is the snapshot-scoped export.
//
// It is a separate function so that the whole set is read, verified and
// serialised on ONE transaction handle — the property that makes
// "verified, then exported" a single operation rather than a sequence.
func (dm *DatabaseManager) exportSkillPackagesInTx(ctx context.Context, skillIDs []string) ([]*PackageExportArchive, []SkillPackageExportOmitted, error) {
	if dm.db == nil {
		return nil, nil, packageReadErrorf(PackageReadReasonDatabaseFailure, "",
			"the database is not initialized")
	}

	var (
		archives []*PackageExportArchive
		omitted  []SkillPackageExportOmitted
	)
	err := dm.withPackageReadSnapshot(ctx, func(tx *sql.Tx) error {
		archives = nil
		omitted = nil
		for _, skillID := range skillIDs {
			archive, err := exportPackageInTx(ctx, tx, skillID)
			if err != nil {
				// A package that cannot be exported is NAMED, never
				// silently dropped. The reason travels with it.
				omitted = append(omitted, omittedPackageFor(skillID, err))
				continue
			}
			archives = append(archives, archive)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(archives, func(i, j int) bool {
		return archives[i].SkillID < archives[j].SkillID
	})
	archives, omitted = rejectPackageExportCollisions(archives, omitted)
	// The omissions are sorted too, so the export's report is
	// deterministic: the same set of packages always produces the same
	// report, in the same order.
	sort.SliceStable(omitted, func(i, j int) bool {
		return omitted[i].SkillID < omitted[j].SkillID
	})
	return archives, omitted, nil
}

// rejectPackageExportCollisions removes the archives whose FILE NAME
// collides with an archive that was already produced, and records each
// of them as an omission.
//
// The file name is `<name>-<version>.skill`, and it is derived from the
// package's canonical identity — but the derivation is not injective.
// The skill name `a-b` at version `1.0.0` and the skill name `a` at
// version `b-1.0.0` both derive `a-b-1.0.0.skill`.
//
// Two archives with the same name cannot both be published: the second
// would overwrite the first, and an export that silently overwrites an
// archive is exactly the loss R11 exists to prevent. So the FIRST
// archive — in the canonical skill-id order, which is deterministic —
// keeps the name, and every other one is NAMED as an omission with the
// reason `file_name_collision`.
//
// The collision is a property of the package SET, so it is detected
// here rather than at the publisher: the publisher would otherwise see
// two archives with one name and would have to decide which one loses.
func rejectPackageExportCollisions(
	archives []*PackageExportArchive,
	omitted []SkillPackageExportOmitted,
) ([]*PackageExportArchive, []SkillPackageExportOmitted) {
	seen := make(map[string]bool, len(archives))
	kept := make([]*PackageExportArchive, 0, len(archives))
	for _, archive := range archives {
		if seen[archive.FileName] {
			omitted = append(omitted, SkillPackageExportOmitted{
				SkillID:     archive.SkillID,
				Name:        archive.Name,
				Version:     archive.Version,
				FileCount:   archive.FileCount,
				PackageHash: archive.PackageHash,
				Reason:      "file_name_collision",
				Detail: "another package in this export is published under the file name " +
					archive.FileName + ", so this one cannot be published under it too",
			})
			continue
		}
		seen[archive.FileName] = true
		kept = append(kept, archive)
	}
	return kept, omitted
}

// exportPackageInTx verifies ONE package and serialises it, inside the
// caller's snapshot.
//
// The order is load-bearing: the package is VERIFIED first, and a
// package that fails verification is not serialised at all. The bytes
// that were verified are the bytes that are exported, because both read
// the same snapshot.
func exportPackageInTx(ctx context.Context, tx *sql.Tx, skillID string) (*PackageExportArchive, error) {
	report, err := verifyPackageInTx(ctx, tx, skillID)
	if err != nil {
		return nil, err
	}
	if !report.Verified {
		return nil, packageReadErrorf(PackageReadReasonIntegrityFailure, "",
			"the package %s is not intact and cannot be exported: %s",
			skillID, describePackageFailures(report.Failures))
	}

	rows, err := loadPackageFileRows(ctx, tx, skillID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, packageReadErrorf(PackageReadReasonIntegrityFailure, "",
			"the package %s carries no files", skillID)
	}

	entries := make([]packageExportEntry, 0, len(rows))
	for i := range rows {
		file := &rows[i]
		blob, err := loadPackageBlob(ctx, tx, file.SHA256)
		if err != nil {
			return nil, err
		}
		if blob == nil {
			return nil, packageReadErrorf(PackageReadReasonIntegrityFailure, file.Path,
				"the stored blob %s of %s is missing", file.SHA256, file.Path)
		}
		entries = append(entries, packageExportEntry{
			Path:       file.Path,
			Content:    blob.Content,
			Executable: file.Executable,
		})
	}

	content, err := writePackageArchive(entries)
	if err != nil {
		return nil, err
	}

	base, err := packageExportBaseName(report.Name, report.Version)
	if err != nil {
		return nil, packageReadErrorf(PackageReadReasonIntegrityFailure, "",
			"the package %s cannot be published under a safe file name: %v", skillID, err)
	}

	sum := sha256.Sum256(content)
	return &PackageExportArchive{
		SkillID:          skillID,
		Name:             report.Name,
		Version:          report.Version,
		Entrypoint:       packageEntrypointName,
		EntrypointSHA256: entrypointHashOf(rows),
		PackageHash:      report.PackageHash,
		FileCount:        len(rows),
		FileName:         base + ".skill",
		SHA256:           hex.EncodeToString(sum[:]),
		SizeBytes:        int64(len(content)),
		Content:          content,
	}, nil
}

// entrypointHashOf returns the entrypoint's recorded hash from the file
// rows.
//
// The entrypoint file's hash IS the hash of the ORIGINAL entrypoint
// bytes: the manifest's `entrypoint_sha256` is checked against it by
// the verification that ran above, so the two agree.
func entrypointHashOf(rows []PackageManifestFile) string {
	for i := range rows {
		if rows[i].Path == packageEntrypointName {
			return rows[i].SHA256
		}
	}
	return ""
}

// describePackageFailures renders the verification failures as one
// line, so a refusal names what is wrong.
func describePackageFailures(failures []PackageVerificationFailure) string {
	if len(failures) == 0 {
		return "no failure was reported"
	}
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		if failure.Path != "" {
			parts = append(parts, failure.Check+" ("+failure.Path+")")
			continue
		}
		parts = append(parts, failure.Check)
	}
	return strings.Join(parts, ", ")
}

// omittedPackageFor builds the omission record for a package that could
// not be exported.
//
// The reason is carried, because "this package is missing" and "this
// package is missing BECAUSE its blob is gone" are different facts and
// only one of them is actionable.
func omittedPackageFor(skillID string, err error) SkillPackageExportOmitted {
	omitted := SkillPackageExportOmitted{
		SkillID: skillID,
		Reason:  packageExportOmissionReason(err),
	}
	if name, version, parseErr := ParseNameAndVersionFromID(skillID); parseErr == nil {
		omitted.Name = name
		omitted.Version = version
	}
	if detail := packageExportOmissionDetail(err); detail != "" {
		omitted.Detail = detail
	}
	return omitted
}

// packageExportOmissionReason classifies why a package was not
// exported.
func packageExportOmissionReason(err error) string {
	switch PackageReadReasonOf(err) {
	case PackageReadReasonIntegrityFailure:
		return "integrity_failure"
	case PackageReadReasonPackageNotFound:
		return "package_not_found"
	case PackageReadReasonDatabaseFailure:
		return "database_failure"
	default:
		return "export_failed"
	}
}

// packageExportOmissionDetail renders the refusal's message.
func packageExportOmissionDetail(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ─── The legacy single-document skill ──────────────────────────────────────

// ExportSkillDocument exports an ORDINARY skill — one with no package —
// as a one-file `.skill` archive.
//
// An ordinary skill is a single document, so its archive is a single
// entry: `SKILL.md`, holding the skill's own bytes. The archive is a
// valid package for the P5 importer, which is what makes an ordinary
// skill movable by the same transport a package uses.
//
// **A PACKAGED skill is REFUSED.** The skill's row holds the
// *projection* of its package — the original plus, for a package that
// declares no version, one inserted `version:` line — and the
// package's other files (references, scripts, assets) live in their own
// rows and blobs. A one-file archive built from the row would therefore
// carry the projection rather than the original bytes, and would omit
// every other file: the package would come back a DIFFERENT package, or
// no package at all. That is the silent loss §6.3 exists to prevent, so
// the exporter refuses instead. A caller that wants a package exports
// the PACKAGE (`ExportSkillPackage`), which carries the original bytes
// and every file.
//
// **What the transport adapts, stated plainly.** The archive carries the
// skill's ORIGINAL document — the bytes the skill's row holds, which
// for an ordinary skill ARE the original document. The importer, on the
// way back in, derives a projection from it: a document that declares a
// version projects to itself, so the round trip is byte-exact.
//
// A document that declares **no** version is **refused by the
// exporter**: the package format requires a version in the frontmatter
// (§5.2), and the importer's own frontmatter rule refuses such a
// document. The exporter refuses it for the same reason, so the
// refusal happens on this side of the transport rather than after it.
//
// The exporter writes nothing. It reads the skill's row, checks that
// the skill is not a package, checks that the document parses as a
// skill, and serialises it.
func (dm *DatabaseManager) ExportSkillDocument(ctx context.Context, skillID string) (*PackageExportArchive, error) {
	if strings.TrimSpace(skillID) == "" {
		return nil, packageReadErrorf(PackageReadReasonSelectorInvalid, "",
			"a skill id is required")
	}
	if dm.db == nil {
		return nil, packageReadErrorf(PackageReadReasonDatabaseFailure, "",
			"the database is not initialized")
	}

	var archive *PackageExportArchive
	err := dm.withPackageReadSnapshot(ctx, func(tx *sql.Tx) error {
		var (
			content   sql.NullString
			deletedAt sql.NullString
		)
		err := tx.QueryRowContext(ctx, skillDocumentContentQuery, skillID).Scan(&content, &deletedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return packageReadErrorf(PackageReadReasonPackageNotFound, "",
				"no skill is published for %s", skillID)
		}
		if err != nil {
			return packageReadErrorWrap(PackageReadReasonDatabaseFailure, "", err,
				"read the skill document of %s", skillID)
		}
		if deletedAt.Valid {
			return packageReadErrorf(PackageReadReasonPackageNotFound, "",
				"the skill %s has been shredded", skillID)
		}

		// A PACKAGED skill is refused, and the refusal names the way
		// out. A one-file archive built from the row would carry the
		// projection and omit every other file, so the package would
		// not survive the round trip.
		var packaged int
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM skill_packages WHERE skill_id = ?`, skillID).Scan(&packaged)
		if err != nil {
			return packageReadErrorWrap(PackageReadReasonDatabaseFailure, "", err,
				"read the package row of %s", skillID)
		}
		if packaged > 0 {
			return packageReadErrorf(PackageReadReasonSelectorInvalid, "",
				"the skill %s is a PACKAGE, not an ordinary skill: its row holds the "+
					"projection and its other files live in their own rows, so a "+
					"one-file archive would drop them. Export the package instead",
				skillID)
		}

		frontmatter, _, parseErr := ParseSkillFrontmatter(content.String)
		if parseErr != nil {
			return packageReadErrorf(PackageReadReasonIntegrityFailure, "",
				"the skill %s does not carry a usable frontmatter: %v", skillID, parseErr)
		}
		base, err := packageExportBaseName(frontmatter.Name, frontmatter.Version)
		if err != nil {
			return packageReadErrorf(PackageReadReasonIntegrityFailure, "",
				"the skill %s cannot be published under a safe file name: %v", skillID, err)
		}

		serialised, err := writePackageArchive([]packageExportEntry{{
			Path:    packageEntrypointName,
			Content: []byte(content.String),
		}})
		if err != nil {
			return err
		}
		sum := sha256.Sum256(serialised)
		archive = &PackageExportArchive{
			SkillID:          skillID,
			Name:             frontmatter.Name,
			Version:          frontmatter.Version,
			Entrypoint:       packageEntrypointName,
			EntrypointSHA256: sha256Hex([]byte(content.String)),
			PackageHash:      "",
			FileCount:        1,
			FileName:         base + ".skill",
			SHA256:           hex.EncodeToString(sum[:]),
			SizeBytes:        int64(len(serialised)),
			Content:          serialised,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return archive, nil
}

// ─── The safe file name ────────────────────────────────────────────────────

// packageExportBaseName derives the archive's base name from the
// package's canonical identity.
//
// The name is `<name>-<version>`, and it is checked as a FILE NAME —
// not merely as a string. A skill name is validated as an identifier
// (§5.1), and an identifier may contain a path separator: `a/b` is a
// legal skill name and `a/b-1.0.0.skill` is a path, not a file name.
// So the derived name is refused when it is not a single, safe path
// component.
//
// The refusal is a refusal, not a sanitisation: a package whose
// identity cannot be a file name is named in the export's omissions
// with its reason, and the export carries on with the rest. Silently
// renaming it would publish a file whose name does not match the
// identity the index records.
func packageExportBaseName(name, version string) (string, error) {
	if name == "" || version == "" {
		return "", fmt.Errorf("the package has no name or no version")
	}
	base := name + "-" + version
	if err := validatePackageExportFileName(base); err != nil {
		return "", err
	}
	return base, nil
}

// validatePackageExportFileName refuses a base name that is not a safe
// single path component.
//
// The rules are the ones a filesystem actually cares about:
//
//   - no path separator, on either platform's notion of one;
//   - not `.` and not `..`;
//   - no leading dot (a hidden file is a surprise, and a name that
//     begins with `.` is a relative-path trap in some tooling);
//   - no NUL and no control character;
//   - not empty.
//
// The canonical skill id is carried in the export's index, so the name
// is a convenience and the id is the identity.
func validatePackageExportFileName(base string) error {
	if base == "" {
		return fmt.Errorf("the file name is empty")
	}
	if strings.ContainsAny(base, `/\`) {
		return fmt.Errorf("the file name %q contains a path separator", base)
	}
	if base == "." || base == ".." {
		return fmt.Errorf("the file name %q is a relative path element", base)
	}
	if strings.HasPrefix(base, ".") {
		return fmt.Errorf("the file name %q begins with a dot", base)
	}
	for _, r := range base {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("the file name %q contains a control character", base)
		}
	}
	return nil
}
