// skill_package_read_errors.go — the refusal vocabulary of the package
// READER.
//
// Design of record: PR #222 (docs/dev/skill-package-format-design-2026-11-04.md)
// §8.1–§8.7 (the retrieval API) and §13 (the regression matrix).
//
// The vocabulary is CLOSED. A caller may branch on the reason code; it may
// not branch on the prose, which is for the operator.
//
// Every refusal in this file is FAIL CLOSED. A read that cannot prove what
// it is returning does not return: there is no fallback to the unverified
// projection, no fallback to unverified blob bytes, and no "best effort"
// content. A corrupt package is a refusal, never a smaller answer.

package internal

import (
	"errors"
	"fmt"
)

// PackageReadReason is a stable, machine-readable classification for a
// refused package read.
//
// The codes the design names itself are used verbatim:
// `content_hash_mismatch` (§8.6, T34) and `line_too_large` (§8.6, T49).
// The remaining codes name the distinctions the design states but does not
// give a code for, and follow the same lower_snake_case shape.
//
// `line_too_large` is the ONE exception to "refused": it is never a
// refusal. It is the reason of the TERMINAL response (§8.6, T49) — the
// one that carries zero lines and an unchanged `next_offset` — and it is
// never wrapped in a `PackageReadError`, so `PackageReadReasonOf` can
// never return it. A caller that branches on REFUSAL reasons will not
// see this code, and must not expect to.
type PackageReadReason string

const (
	// PackageReadReasonPackageNotFound — no package is published for the
	// requested skill.
	//
	// This covers BOTH "there is no such skill" and "the skill exists but
	// owns no package": a package reader never falls back to the ordinary
	// skill's projection, because that projection is not the package's
	// content.
	PackageReadReasonPackageNotFound PackageReadReason = "package_not_found"

	// PackageReadReasonFileNotFound — the package exists and carries no
	// file at the requested canonical path.
	//
	// It is deliberately NOT used for a missing or corrupt blob: those are
	// integrity failures, and reporting corruption as a missing file would
	// tell the caller to try a different path when the package itself is
	// damaged.
	PackageReadReasonFileNotFound PackageReadReason = "file_not_found"

	// PackageReadReasonInvalidPath — the requested path is not a
	// canonical package path (§5.4).
	//
	// The path is a PACKAGE-RELATIVE name, never a host filesystem path.
	// A path that traverses, is absolute, is empty or carries a NUL is
	// refused before any row is looked up.
	PackageReadReasonInvalidPath PackageReadReason = "invalid_path"

	// PackageReadReasonContentHashMismatch — the stored bytes are not the
	// bytes that were recorded.
	//
	// The reader re-hashes every blob it returns, so a blob row whose
	// content was mutated in place is refused rather than returned (§8.6,
	// T34). The same code covers a size disagreement, because a size that
	// does not describe the bytes is the same defect seen from another
	// side.
	PackageReadReasonContentHashMismatch PackageReadReason = "content_hash_mismatch"

	// PackageReadReasonIntegrityFailure — the stored package is not
	// internally consistent: its manifest does not hash to its recorded
	// package hash, its manifest and its package row describe different
	// skills, its file rows do not match its manifest, or a blob the rows
	// reference is gone.
	//
	// It is the fail-closed class. Nothing is repaired, reimported, rescanned
	// or replaced in response to it — `verify` reports the damage and an
	// operator decides what to do.
	PackageReadReasonIntegrityFailure PackageReadReason = "integrity_failure"

	// PackageReadReasonInvalidBounds — the read's bounds are not
	// admissible: a negative offset, a negative limit, a byte cap above
	// the 1 MiB hard maximum, or an offset beyond the end of the file.
	//
	// A cap above the hard maximum is REFUSED, not clamped: a silent clamp
	// would return a response the caller did not ask for.
	PackageReadReasonInvalidBounds PackageReadReason = "invalid_bounds"

	// PackageReadReasonLineTooLarge — the first requested line alone
	// exceeds the byte cap (§8.6, T49).
	//
	// This is the TERMINAL response, not a failure: it carries zero lines,
	// `truncated: true` and `next_offset == offset`, and the caller must
	// stop rather than resume. It is never returned without those
	// companions, and it is the ONLY response that may carry an unchanged
	// `next_offset`.
	PackageReadReasonLineTooLarge PackageReadReason = "line_too_large"

	// PackageReadReasonUnpublishable — the package is intact, but it
	// cannot be PUBLISHED under a safe file name.
	//
	// A skill's NAME is an identifier, and an identifier may contain a
	// path separator: `a/b` is a legal skill name. The derived file
	// name must be a single safe path component, and one that is not is
	// refused.
	//
	// This reason is DISTINCT from `integrity_failure` on purpose: the
	// package in this case VERIFIED, and its bytes are consistent. It
	// is the publication that failed, not the content.
	PackageReadReasonUnpublishable PackageReadReason = "unpublishable"

	// PackageReadReasonDatabaseFailure — the substrate failed.
	//
	// It is the class for an unusable connection and an unexpected storage
	// error. It never stands in for a content failure: a database that
	// cannot answer is not a package that does not exist.
	PackageReadReasonDatabaseFailure PackageReadReason = "database_failure"

	// PackageReadReasonBinaryRequiresRaw — the content cannot be carried
	// as text without loss.
	//
	// A binary asset, or bytes that are not valid UTF-8, is refused unless
	// the caller asks for the raw form. The refusal is what keeps a later
	// JSON layer from corrupting binary content by accident: the caller
	// has to say "give me the bytes" before it gets a byte-exact answer.
	PackageReadReasonBinaryRequiresRaw PackageReadReason = "binary_requires_raw"

	// PackageReadReasonSelectorInvalid — the package was named in a way
	// that does not identify one package.
	//
	// A package is named by its canonical skill id, OR by a name AND a
	// version. Naming it by a name alone would leave the choice of
	// version open, and silently picking one is exactly the kind of
	// reinterpretation the design refuses.
	PackageReadReasonSelectorInvalid PackageReadReason = "selector_invalid"
)

// PackageReadError is the typed refusal returned by the package reader.
//
// The shape mirrors PackageImportError: a stable reason code, optional
// path context, optional detail, and the wrapped cause.
type PackageReadError struct {
	// Reason is the stable machine-readable code.
	Reason PackageReadReason

	// Path is the offending package-relative path, when the refusal is
	// about one file.
	//
	// It is DISPLAY context only: it is never used to address a file on
	// the host filesystem and it is never compared for identity.
	Path string

	// Detail is optional extra context.
	Detail string

	// cause is the wrapped underlying error, if any.
	cause error
}

// Error implements the error interface.
//
// The message leads with the stable code, so the CLI and the MCP surface
// both carry a greppable refusal rather than prose that has to be matched
// by shape.
func (e *PackageReadError) Error() string {
	if e == nil {
		return "package read refused"
	}
	switch {
	case e.Path != "" && e.Detail != "":
		return fmt.Sprintf("package read refused: %s: %s (%s)", e.Reason, e.Path, e.Detail)
	case e.Path != "":
		return fmt.Sprintf("package read refused: %s: %s", e.Reason, e.Path)
	case e.Detail != "":
		return fmt.Sprintf("package read refused: %s (%s)", e.Reason, e.Detail)
	default:
		return fmt.Sprintf("package read refused: %s", e.Reason)
	}
}

// Unwrap exposes the underlying cause, so errors.Is and errors.As reach
// through the refusal.
func (e *PackageReadError) Unwrap() error { return e.cause }

// OutcomeCode returns the stable outcome_code for this refusal.
//
// The code is the reason itself, so the CLI and the MCP surface both
// carry a greppable code rather than prose that has to be matched by
// shape.
func (e *PackageReadError) OutcomeCode() string {
	if e == nil || e.Reason == "" {
		return "unclassified"
	}
	return string(e.Reason)
}

// PackageReadReasonOf returns the reason carried by err, if err is a
// PackageReadError (or wraps one). It returns the empty reason when err
// is nil or is some other error type.
func PackageReadReasonOf(err error) PackageReadReason {
	if err == nil {
		return ""
	}
	var pe *PackageReadError
	if !errors.As(err, &pe) {
		return ""
	}
	return pe.Reason
}

// packageReadErrorf builds a refusal with a formatted detail.
func packageReadErrorf(reason PackageReadReason, path, format string, args ...any) *PackageReadError {
	return &PackageReadError{
		Reason: reason,
		Path:   path,
		Detail: fmt.Sprintf(format, args...),
	}
}

// packageReadErrorWrap builds a refusal that wraps an underlying error.
func packageReadErrorWrap(reason PackageReadReason, path string, cause error, format string, args ...any) *PackageReadError {
	return &PackageReadError{
		Reason: reason,
		Path:   path,
		Detail: fmt.Sprintf(format, args...),
		cause:  cause,
	}
}
