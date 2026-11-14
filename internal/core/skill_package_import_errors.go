// skill_package_import_errors.go — the refusal vocabulary of the package
// importer.
//
// Design of record: PR #222 (docs/dev/skill-package-format-design-2026-11-04.md)
// §7 (threat model), §5.7 (duplicate detection), §7.2 (the fail-closed
// scanner contract).
//
// The vocabulary is CLOSED. A caller may branch on the reason code; it may
// not branch on the prose, which is for the operator.
//
// Every refusal in this file is FAIL CLOSED: the importer validates and
// scans BEFORE it opens a transaction, so a refusal means NOTHING was
// written — no package row, no file row, no blob row, no projection.

package internal

import (
	"errors"
	"fmt"
)

// PackageImportReason is a stable, machine-readable refusal code for a
// rejected package import.
//
// The codes the design names itself are used verbatim:
// `scanner_secret` (T23), `scanner_poison` (T24), `scanner_unavailable`
// (T43), `symlinked_directory` and `path_escapes_root` (T46),
// `version_conflict` (T29, §5.7). The remaining codes name rules the
// design states but does not give a code for, and follow the same
// lower_snake_case shape.
type PackageImportReason string

const (
	// PackageImportReasonScannerSecret — a file carried content the
	// secret scanner rejected (§7 threat 14, T23).
	PackageImportReasonScannerSecret PackageImportReason = "scanner_secret"

	// PackageImportReasonScannerPoison — a file carried a toxic phrase
	// (§7 threat 14, T24).
	PackageImportReasonScannerPoison PackageImportReason = "scanner_poison"

	// PackageImportReasonScannerUnavailable — the poison scanner's
	// configuration could not be loaded, so the scan could not be
	// performed (§7.2, R10, T43).
	//
	// This is the fail-closed refusal. The poison scanner reads an
	// operator-editable file, and a scan that could not run must not be
	// reported as a scan that passed.
	PackageImportReasonScannerUnavailable PackageImportReason = "scanner_unavailable"

	// PackageImportReasonSymlinkedDirectory — a symlinked directory was
	// found inside the import root. The walk never descends into one
	// (§7 threat 19, T46).
	PackageImportReasonSymlinkedDirectory PackageImportReason = "symlinked_directory"

	// PackageImportReasonPathEscapesRoot — a path resolved outside the
	// resolved import root (§7 threat 19, T46).
	PackageImportReasonPathEscapesRoot PackageImportReason = "path_escapes_root"

	// PackageImportReasonProvenanceFailed — the resurrection could not
	// write the row's artifact provenance, so the import was rolled back.
	//
	// It is deliberately NOT `frontmatter_invalid`: the document was
	// accepted, and what failed was the attribution. Reporting it as a
	// frontmatter refusal would tell the operator their package was
	// malformed when it was not.
	//
	// The refusal exists because the resurrection REPLACES the content of
	// an existing row. Leaving the previous writer's attribution in place
	// would be false attribution, and reporting a success with a stale
	// attribution would be a false success.
	PackageImportReasonProvenanceFailed PackageImportReason = "provenance_failed"

	// PackageImportReasonUnsupportedEntryType — an entry was neither a
	// regular file nor a directory: a FIFO, a device or a socket
	// (§7.1).
	PackageImportReasonUnsupportedEntryType PackageImportReason = "unsupported_entry_type"

	// PackageImportReasonFileCountLimit — the package carries more than
	// the maximum number of files (§7 threat 11, T45).
	PackageImportReasonFileCountLimit PackageImportReason = "file_count_limit"

	// PackageImportReasonSizeLimit — a file, the entrypoint or the
	// package as a whole exceeded its byte cap (§7 threat 11, T45).
	PackageImportReasonSizeLimit PackageImportReason = "size_limit"

	// PackageImportReasonEntrypointNotFound — the package root carries
	// no `SKILL.md` (§5.3).
	PackageImportReasonEntrypointNotFound PackageImportReason = "entrypoint_not_found"

	// PackageImportReasonAmbiguousEntrypoint — more than one file
	// carries the entrypoint name at the package root (§5.3.1 rule 3).
	PackageImportReasonAmbiguousEntrypoint PackageImportReason = "ambiguous_package_root"

	// PackageImportReasonVersionConflict — the same (name, version) is
	// already published with a DIFFERENT package_hash (§5.7, T29).
	//
	// There is no force escape: a published package is immutable (§5.8).
	PackageImportReasonVersionConflict PackageImportReason = "version_conflict"

	// PackageImportReasonSkillIDConflict — the canonical skill id is
	// already taken by a LIVE skill that owns no package.
	//
	// The importer does not overwrite an ordinary skill in place: that
	// is the legacy force path's behaviour, and it would replace a
	// published document under the same id — exactly the mutation §5.8
	// refuses.
	PackageImportReasonSkillIDConflict PackageImportReason = "skill_id_conflict"

	// PackageImportReasonBinaryNotAllowed — the package carries a binary
	// asset and the operator did not opt in (O3, §7.3).
	PackageImportReasonBinaryNotAllowed PackageImportReason = "binary_not_allowed"

	// PackageImportReasonContentTypeNotAllowed — the file's extension is
	// not on the explicit content-type allow-list (§5.5, §7.3).
	PackageImportReasonContentTypeNotAllowed PackageImportReason = "content_type_not_allowed"

	// PackageImportReasonContentTypeMismatch — the file's bytes do not
	// match the content type its extension claims (§5.5: "keyed on
	// extension plus a content sniff").
	PackageImportReasonContentTypeMismatch PackageImportReason = "content_type_mismatch"

	// PackageImportReasonInvalidUTF8 — a file's bytes are not valid
	// UTF-8, so the file cannot be a text file and cannot be stored as
	// one.
	PackageImportReasonInvalidUTF8 PackageImportReason = "invalid_utf8"

	// PackageImportReasonFrontmatterInvalid — the entrypoint's
	// frontmatter could not be parsed, or lacks the fields MPM's
	// frontmatter contract requires (§5.2).
	PackageImportReasonFrontmatterInvalid PackageImportReason = "frontmatter_invalid"

	// PackageImportReasonNameInvalid — the package's name is not a valid
	// skill name (§5.1).
	PackageImportReasonNameInvalid PackageImportReason = "name_invalid"

	// PackageImportReasonVersionInvalid — the effective version is not
	// valid semver (§5.2).
	PackageImportReasonVersionInvalid PackageImportReason = "version_invalid"

	// PackageImportReasonVersionOverrideConflict — the operator supplied
	// an effective-version override for a package that DECLARES a
	// version. The declared version is the effective one (§5.2), so the
	// override is refused rather than silently ignored.
	PackageImportReasonVersionOverrideConflict PackageImportReason = "version_override_conflict"

	// PackageImportReasonPathInvalid — a file's path was refused by the
	// canonical-path primitive (§5.4). The Detail carries the P2 reason.
	PackageImportReasonPathInvalid PackageImportReason = "path_invalid"

	// PackageImportReasonImportRootInvalid — the selected path is not a
	// readable package root: it does not exist, it is not a directory or
	// a `SKILL.md`, or it cannot be resolved.
	PackageImportReasonImportRootInvalid PackageImportReason = "import_root_invalid"

	// PackageImportReasonReadFailed — a file's bytes could not be read.
	// The Detail carries the underlying error.
	PackageImportReasonReadFailed PackageImportReason = "read_failed"

	// PackageImportReasonChangedDuringRead — a file changed between the
	// walk's inspection and the read, so the bytes that were inspected
	// are not the bytes that would be stored (§7 threat 19).
	PackageImportReasonChangedDuringRead PackageImportReason = "changed_during_read"

	// ─────────────────────────────────────────────────────────────────────
	// THE ARCHIVE CODES — Stage P5
	// ─────────────────────────────────────────────────────────────────────
	//
	// The codes below are the ZIP path's own vocabulary (§7.1, §5.3.1,
	// §13 T13–T22, T26, T35, T48). They are ADDITIVE: the directory
	// path's codes are unchanged, and every code the directory path
	// already had keeps its meaning.

	// PackageImportReasonArchiveInvalid — the archive's STRUCTURE is not
	// usable: it is not a ZIP at all, its central directory is truncated
	// or malformed, an entry declares a size that its payload does not
	// match, an entry uses a compression method that cannot be validated
	// safely, or the archive is cut short mid-entry.
	//
	// It is the archive-level analogue of `read_failed`: the bytes
	// could not be obtained in a form the importer can trust. The
	// specific cause is carried in the Detail.
	PackageImportReasonArchiveInvalid PackageImportReason = "archive_invalid"

	// PackageImportReasonChecksumMismatch — an entry's bytes did not
	// match the CRC-32 the archive declares for them (§13 T22).
	//
	// Detecting it requires reading the entry to EOF, which the importer
	// does: the CRC is only computed over the bytes that were actually
	// produced.
	PackageImportReasonChecksumMismatch PackageImportReason = "checksum_mismatch"

	// PackageImportReasonPathOutsideRoot — a file entry does not lie
	// under the derived package root (§5.3.1 rule 5).
	//
	// In the flat layout the root is the archive root and the rule is a
	// no-op. In the wrapped layout it is what makes the wrapper a
	// wrapper rather than one directory among several: an entry beside
	// the wrapper belongs to a different tree, and nothing is dropped
	// silently.
	PackageImportReasonPathOutsideRoot PackageImportReason = "path_outside_root"

	// PackageImportReasonUnsupportedArchiveLayout — the archive's
	// package root is neither the archive root nor a SINGLE enclosing
	// directory, so the archive is neither of the two accepted layouts
	// (O9, §5.3.1).
	//
	// A root deeper than one segment means the package is wrapped in
	// more than one directory. O9 accepts exactly two shapes — the flat
	// layout and the single-wrapper layout — and a deeper root is a
	// third shape, so it is refused rather than silently reinterpreted.
	PackageImportReasonUnsupportedArchiveLayout PackageImportReason = "unsupported_archive_layout"

	// PackageImportReasonArchiveEncrypted — an entry is encrypted, so
	// its bytes cannot be validated by the scanners that guard every
	// stored byte.
	//
	// The importer cannot read an encrypted entry, and it cannot prove
	// anything about what it holds, so it refuses rather than storing
	// bytes it never inspected.
	PackageImportReasonArchiveEncrypted PackageImportReason = "archive_encrypted"

	// ─────────────────────────────────────────────────────────────────────
	// THE PATH CODES — the canonical-path rules, carried verbatim
	// ─────────────────────────────────────────────────────────────────────
	//
	// The ZIP path reports the canonical-path rules (§5.4) with the
	// SAME codes §5.4 and §7 name — `path_traversal` (§7 threat 1,
	// T13), `absolute_path` (threat 2, T14), `drive_letter_prefix`
	// (threat 4, T15), `duplicate_path` (threat 6, T17),
	// `path_case_collision` (threat 7, T18) and
	// `path_unicode_collision` (threat 8, T19).
	//
	// The codes are the P2 path reasons' own strings, so a caller
	// branches on one vocabulary rather than on a second encoding of
	// the same rule. packageImportReasonForPathReason is the TOTAL
	// mapping between the two.

	// PackageImportReasonPathEmpty — the entry's name was empty.
	PackageImportReasonPathEmpty PackageImportReason = "empty_path"

	// PackageImportReasonPathNUL — the entry's name carried a NUL byte.
	PackageImportReasonPathNUL PackageImportReason = "nul_byte"

	// PackageImportReasonPathControlCharacter — the entry's name carried
	// a control character.
	PackageImportReasonPathControlCharacter PackageImportReason = "control_character"

	// PackageImportReasonPathUNC — the entry's name began with the UNC
	// shape `\\server\share`.
	PackageImportReasonPathUNC PackageImportReason = "unc_path"

	// PackageImportReasonPathDriveLetter — the entry's name began with a
	// drive-letter prefix such as `C:` (§7 threat 4, T15).
	PackageImportReasonPathDriveLetter PackageImportReason = "drive_letter_prefix"

	// PackageImportReasonPathAbsolute — the entry's name began with `/`,
	// making it an absolute path (§7 threat 2, T14).
	PackageImportReasonPathAbsolute PackageImportReason = "absolute_path"

	// PackageImportReasonPathBackslash — the entry's name carried a
	// backslash. A canonical path is POSIX-separated; a backslash is
	// never a separator (§7 threat 4).
	PackageImportReasonPathBackslash PackageImportReason = "backslash_separator"

	// PackageImportReasonPathTrailingSlash — the entry's name ended with
	// `/`. A directory entry is SKIPPED rather than refused, so this
	// code is reachable only for a name that ends with a slash and is
	// not a directory entry.
	PackageImportReasonPathTrailingSlash PackageImportReason = "trailing_slash"

	// PackageImportReasonPathEmptySegment — the entry's name carried an
	// empty path segment, i.e. `//`.
	PackageImportReasonPathEmptySegment PackageImportReason = "empty_path_segment"

	// PackageImportReasonPathDotSegment — the entry's name carried a `.`
	// segment.
	PackageImportReasonPathDotSegment PackageImportReason = "dot_segment"

	// PackageImportReasonPathTraversal — the entry's name carried a `..`
	// segment. This is the ZIP-slip refusal (§7 threat 1, T13).
	PackageImportReasonPathTraversal PackageImportReason = "path_traversal"

	// PackageImportReasonPathTooLong — the canonical path exceeded
	// MaxPackagePathBytes bytes (§5.4).
	PackageImportReasonPathTooLong PackageImportReason = "path_too_long"

	// PackageImportReasonPathTooDeep — the canonical path exceeded
	// MaxPackagePathDepth segments (§5.4).
	PackageImportReasonPathTooDeep PackageImportReason = "path_too_deep"

	// PackageImportReasonPathDuplicate — two entries carried the SAME
	// canonical path (§7 threat 6, T17).
	PackageImportReasonPathDuplicate PackageImportReason = "duplicate_path"

	// PackageImportReasonPathUnicodeCollision — two entries carried
	// DIFFERENT names that normalise to the same NFC form (§7 threat 8,
	// T19).
	PackageImportReasonPathUnicodeCollision PackageImportReason = "path_unicode_collision"

	// PackageImportReasonPathCaseCollision — two canonical paths differ
	// only by ASCII case (§7 threat 7, T18).
	PackageImportReasonPathCaseCollision PackageImportReason = "path_case_collision"
)

// packageImportReasonForPathReason maps a canonical-path refusal onto
// the import vocabulary.
//
// The mapping is TOTAL: every reason §5.4 can produce has an import
// counterpart, so an archive entry's path refusal is always classified
// with the rule's own code rather than with a generic one. The two
// vocabularies share their strings for the path rules, so the mapping is
// a renaming rather than a translation — which is what lets a caller
// branch on `path_traversal` without knowing which import path produced
// it.
func packageImportReasonForPathReason(reason PackagePathReason) PackageImportReason {
	switch reason {
	case PackagePathReasonEmpty:
		return PackageImportReasonPathEmpty
	case PackagePathReasonInvalidUTF8:
		return PackageImportReasonInvalidUTF8
	case PackagePathReasonNUL:
		return PackageImportReasonPathNUL
	case PackagePathReasonControlCharacter:
		return PackageImportReasonPathControlCharacter
	case PackagePathReasonUNC:
		return PackageImportReasonPathUNC
	case PackagePathReasonDriveLetter:
		return PackageImportReasonPathDriveLetter
	case PackagePathReasonAbsolute:
		return PackageImportReasonPathAbsolute
	case PackagePathReasonBackslash:
		return PackageImportReasonPathBackslash
	case PackagePathReasonTrailingSlash:
		return PackageImportReasonPathTrailingSlash
	case PackagePathReasonEmptySegment:
		return PackageImportReasonPathEmptySegment
	case PackagePathReasonDotSegment:
		return PackageImportReasonPathDotSegment
	case PackagePathReasonTraversal:
		return PackageImportReasonPathTraversal
	case PackagePathReasonTooLong:
		return PackageImportReasonPathTooLong
	case PackagePathReasonTooDeep:
		return PackageImportReasonPathTooDeep
	case PackagePathReasonDuplicate:
		return PackageImportReasonPathDuplicate
	case PackagePathReasonUnicodeCollision:
		return PackageImportReasonPathUnicodeCollision
	case PackagePathReasonCaseCollision:
		return PackageImportReasonPathCaseCollision
	default:
		// An unknown reason is a bug in the mapping, not in the input.
		// The generic code keeps the refusal classified rather than
		// letting it escape as an unclassified error.
		return PackageImportReasonPathInvalid
	}
}

// PackageImportError is the typed refusal returned by the package
// importer.
//
// It is recognised through errors.As, so a caller may WRAP it
// (fmt.Errorf("...: %w", err)) and the classification still holds — the
// same property PackagePathError and PackageManifestError have.
type PackageImportError struct {
	// Reason is the stable machine-readable code.
	Reason PackageImportReason

	// Path is the offending file's path, relative to the import root.
	//
	// It is DISPLAY context only: it is never used to address a file
	// and it is never compared for identity.
	Path string

	// Detail is optional extra context.
	Detail string

	// cause is the wrapped underlying error, if any.
	cause error
}

// Error implements the error interface.
func (e *PackageImportError) Error() string {
	if e == nil {
		return "package import rejected"
	}
	switch {
	case e.Path != "" && e.Detail != "":
		return fmt.Sprintf("package import rejected: %s: %s (%s)", e.Reason, e.Path, e.Detail)
	case e.Path != "":
		return fmt.Sprintf("package import rejected: %s: %s", e.Reason, e.Path)
	case e.Detail != "":
		return fmt.Sprintf("package import rejected: %s (%s)", e.Reason, e.Detail)
	default:
		return fmt.Sprintf("package import rejected: %s", e.Reason)
	}
}

// Unwrap exposes the underlying cause, so errors.Is and errors.As reach
// through the refusal.
func (e *PackageImportError) Unwrap() error { return e.cause }

// OutcomeCode returns the stable outcome_code for this refusal.
//
// The code is the reason itself, so the CLI and the MCP surface both
// carry a greppable code rather than prose that has to be matched by
// shape.
func (e *PackageImportError) OutcomeCode() string {
	if e == nil || e.Reason == "" {
		return "unclassified"
	}
	return string(e.Reason)
}

// PackageImportReasonOf returns the reason carried by err, if err is a
// PackageImportError (or wraps one). It returns the empty reason when err
// is nil or is some other error type.
func PackageImportReasonOf(err error) PackageImportReason {
	if err == nil {
		return ""
	}
	var pe *PackageImportError
	if !errors.As(err, &pe) {
		return ""
	}
	return pe.Reason
}

// packageImportErrorf builds a refusal with a formatted detail.
func packageImportErrorf(reason PackageImportReason, path, format string, args ...any) *PackageImportError {
	return &PackageImportError{
		Reason: reason,
		Path:   path,
		Detail: fmt.Sprintf(format, args...),
	}
}

// packageImportErrorWrap builds a refusal that wraps an underlying error.
func packageImportErrorWrap(reason PackageImportReason, path string, cause error, format string, args ...any) *PackageImportError {
	return &PackageImportError{
		Reason: reason,
		Path:   path,
		Detail: fmt.Sprintf(format, args...),
		cause:  cause,
	}
}
