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
)

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
