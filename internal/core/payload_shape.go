// payload_shape.go — the typed error for a caller-supplied payload whose
// shape does not match what the tool requires.
//
// WHY THIS EXISTS
// ---------------
// The substrate's dispatch audit classifies every failed tool call with
// `internal.ClassifyError` (internal/core/tool_outcome.go). Before this
// type existed, a caller that sent `tags: "not-an-array"` produced a bare
// `fmt.Errorf`, whose text matched none of the classifier's cases, and the
// call was recorded as:
//
//	outcome_class = 'internal', outcome_code = 'unclassified'
//
// `internal` is the class that means "MPM could not attribute this". The
// live-runtime audit (docs/dev/live-runtime-health-audit-2026-10-28.md,
// PR #157) found 45 of 105 error rows in that class — every one of them
// caller noise. A class that is 43 % agent mistakes is a class in which a
// real substrate fault is invisible.
//
// The fix is a TYPED error rather than a wider net of substring matches.
// A substring rule over message bodies is exactly the failure mode the
// classifier's own contract warns against ("NEVER regex-match full message
// bodies wholesale"), and it cannot distinguish "the caller sent the wrong
// JSON type" from "the substrate said the word 'not found' in a sentence
// that has nothing to do with a missing artifact". A type can.
//
// THE CONTRACT
// ------------
//   - The caller-facing MESSAGE is unchanged from what the untyped error
//     produced. Agents that match on error text keep working; nothing
//     about the failure the caller sees has moved.
//   - The KIND selects a stable `outcome_code`. Codes are part of the
//     public vocabulary of the validation class (see OutcomeClassValidation
//     in tool_outcome.go) and must not be added casually.
//   - The FIELD names the offending field. It never carries the offending
//     VALUE: an outcome_code and an error message both end up in the audit
//     trail, and the audit trail is not a place for caller content.
//
// This mirrors the two typed-validation patterns already in the codebase:
// `ErrSensitiveContentBlocked` (a sentinel, recognised with errors.Is) and
// `*LifecycleEventRejection` (a struct, recognised with errors.As).

package internal

import "fmt"

// PayloadShapeKind identifies WHICH shape contract the caller violated.
// The kind maps one-to-one onto a stable outcome_code.
type PayloadShapeKind string

const (
	// PayloadShapeArray — a field declared as an array of strings
	// received something else (a scalar, an object), or an element of
	// that array was not a string.
	//
	// outcome_code: invalid_array_shape
	PayloadShapeArray PayloadShapeKind = "array"

	// PayloadShapeObject — a field declared as a JSON object received a
	// scalar, an array, or null.
	//
	// outcome_code: invalid_object_shape
	PayloadShapeObject PayloadShapeKind = "object"

	// PayloadShapeNumeric — a field declared as a number received a
	// non-numeric value.
	//
	// outcome_code: invalid_numeric_field
	PayloadShapeNumeric PayloadShapeKind = "numeric"

	// PayloadShapeEnvelope — the top-level {action, params} contract was
	// violated: the payload was null, carried fields outside the params
	// envelope, had no action, had a non-string action, or had a params
	// value that was not an object.
	//
	// This is its own kind because it is a different class of mistake
	// from a wrong field type: the host adapter put the parameters at the
	// wrong nesting level. An operator debugging that needs to see it
	// counted separately from "the value of `limit` was a string".
	//
	// outcome_code: invalid_params_envelope
	PayloadShapeEnvelope PayloadShapeKind = "envelope"

	// PayloadShapeType — a field declared as a scalar (string, boolean)
	// received a different JSON type.
	//
	// outcome_code: invalid_type
	PayloadShapeType PayloadShapeKind = "type"

	// PayloadShapeEnum — a field declared as an enumeration received a
	// value outside the closed set.
	//
	// outcome_code: invalid_enum_value
	PayloadShapeEnum PayloadShapeKind = "enum"
)

// OutcomeCode returns the stable outcome_code for this kind. The codes are
// bounded lowercase ASCII identifiers with single underscores, as the
// outcome-code contract requires.
func (k PayloadShapeKind) OutcomeCode() string {
	switch k {
	case PayloadShapeArray:
		return "invalid_array_shape"
	case PayloadShapeObject:
		return "invalid_object_shape"
	case PayloadShapeNumeric:
		return "invalid_numeric_field"
	case PayloadShapeEnvelope:
		return "invalid_params_envelope"
	case PayloadShapeType:
		return "invalid_type"
	case PayloadShapeEnum:
		return "invalid_enum_value"
	default:
		return "unclassified"
	}
}

// PayloadShapeError is the typed error a malformed caller payload produces.
//
// It is recognised by internal.ClassifyError through errors.As, so a
// handler may WRAP it (fmt.Errorf("...: %w", err)) and the classification
// still holds. That matters because several handlers wrap the parse
// helpers ("reinforce: %w", "weaken: %w", "set_weight: %w").
type PayloadShapeError struct {
	// Kind selects the outcome_code.
	Kind PayloadShapeKind

	// Field names the offending field or the envelope contract. It never
	// carries the offending value.
	Field string

	// Message is the caller-facing text. It is byte-identical to the
	// text the untyped error produced before this type existed, so no
	// agent's error handling moves.
	Message string
}

// Error implements the error interface.
func (e *PayloadShapeError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s: invalid payload shape (%s)", e.Field, e.Kind)
	}
	return e.Message
}

// OutcomeCode returns the stable outcome_code for this error.
func (e *PayloadShapeError) OutcomeCode() string {
	if e == nil {
		return "unclassified"
	}
	return e.Kind.OutcomeCode()
}

// NewPayloadShapeError builds a typed payload-shape error.
//
// The message argument is the caller-facing text and is preserved verbatim.
// Keeping the text at the call site (rather than deriving it here) is
// deliberate: each site has its own established phrasing, and the tests
// that pin those phrasings are the reason the text must not move.
func NewPayloadShapeError(kind PayloadShapeKind, field, message string) *PayloadShapeError {
	return &PayloadShapeError{Kind: kind, Field: field, Message: message}
}
