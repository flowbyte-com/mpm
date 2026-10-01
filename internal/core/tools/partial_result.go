// PartialResult converts a (result, err) pair returned by a handler into
// the transport's error representation, preserving whatever structured
// diagnostic the result carries.
//
// # Why this exists
//
// A HandlerFunc returns (interface{}, error). MPM's contract is that a
// handler may return BOTH a meaningful partial result and a non-nil
// error — see internal/core/compact.go, where a mid-drain batch failure
// returns the full CompactEpistemologyDrainResult (batches_processed,
// raw_processed, lessons_created, raw_remaining, stop_reason, failed_batch,
// failure_reason) alongside the error. The point of that result is that
// the operator can see WHICH batch failed and HOW MUCH work already
// committed.
//
// Both transports used to discard it:
//
//	cmd/mpm/call.go:       writeEnvelope(..., {"success":false,"error":...})
//	cmd/mpm-mcp/tools.go:  mcp.NewToolResultErrorFromErr(name+" failed", err)
//
// which collapsed a rich partial diagnostic into a bare string. This is
// a presentation-layer defect: the information exists at the point of
// loss and is thrown away afterwards.
//
// # What it does NOT do
//
// This does not make a failed operation look successful. `success` stays
// false, the exit code stays 1, and the MCP result stays IsError=true.
// Only the diagnostic payload is added alongside.
//
// A handler that returns (nil, err) is unaffected — there is no partial
// result to preserve, and the envelope is exactly what it was before.
// That is the common case and the reason this is safe to apply to every
// action rather than special-casing compact.
package tools

import (
	"encoding/json"
	"fmt"
)

// ErrorEnvelopeWithResult builds the error envelope for a handler that
// returned both a result and an error.
//
// When result is nil, or is not a JSON object, it degrades to the plain
// {"success":false,"error":...} envelope — byte-identical to the
// pre-existing behaviour, so no existing consumer can regress.
//
// When result IS a JSON object, its fields are merged into the envelope
// and the reserved keys are then forced:
//
//	success — always false; a failed operation is never reported as
//	          successful, regardless of what the payload claims
//	error   — always the Go error text
//
// Forcing after the merge is deliberate. A handler that sets its own
// "success":true or "error" field must not be able to overwrite the
// transport's verdict about the call that produced it.
//
// The reserved keys are documented rather than silently merged so that
// the two conflicting cases are visible at the call site.
func ErrorEnvelopeWithResult(result interface{}, err error) map[string]interface{} {
	envelope := map[string]interface{}{}

	// Start from the partial result when it is an object. A struct such
	// as CompactEpistemologyDrainResult marshals to an object, which is
	// the shape every partial diagnostic in this codebase takes.
	if obj, ok := resultObject(result); ok {
		for k, v := range obj {
			envelope[k] = v
		}
	}

	// Reserved keys are written last, after the merge above.
	envelope["success"] = false
	if err != nil {
		envelope["error"] = err.Error()
	}
	return envelope
}

// resultObject normalizes a handler result into a JSON object, or
// reports that it is not one.
//
// A nil result, a scalar, or a slice is not an envelope: there is
// nothing meaningful to merge at the top level, and inventing one would
// change the shape of every existing error response. Only objects pass.
//
// Normalization goes through JSON, which is also what makes struct-typed
// results (the common case — compact returns a typed struct) mergeable
// with map-typed ones using a single code path. One consequence is that
// numeric values in a map-typed result emerge as float64, since JSON has
// a single number type. That is invisible on the wire — the emitted JSON
// is byte-identical — and only observable to an in-process caller.
func resultObject(result interface{}) (map[string]interface{}, bool) {
	if result == nil {
		return nil, false
	}
	// A typed nil pointer (e.g. (*T)(nil)) is non-nil as an interface but
	// marshals to null, which resultObject must reject rather than treat
	// as an object.
	data, mErr := json.Marshal(result)
	if mErr != nil {
		return nil, false
	}
	var obj map[string]interface{}
	if uErr := json.Unmarshal(data, &obj); uErr != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// HasDiagnosticFields reports whether an error envelope built by
// ErrorEnvelopeWithResult carries any field beyond the two the error
// text already states ("success" and "error").
//
// A transport that emits a separate structured block uses this to
// suppress an empty one, so the overwhelmingly common (nil, err) path
// adds no noise to the response.
//
// The test is for the PRESENCE of extra keys, not for len(env) > 2: a
// result consisting only of reserved keys yields a two-key envelope whose
// values the transport has since overwritten, and that envelope does
// carry information — the corrected verdict — worth emitting.
func HasDiagnosticFields(envelope map[string]interface{}) bool {
	for k := range envelope {
		if k != "success" && k != "error" {
			return true
		}
	}
	return false
}

// ErrorTextForResult renders the human-readable error string used by the
// MCP transport, which carries its message as text rather than as a
// structured field.
//
// The partial result is not folded into this string: the MCP response
// carries the structured payload as a separate content block (see
// ErrorContentWithResult), so duplicating it here would make the error
// message unreadable without adding information.
func ErrorTextForResult(toolName string, err error) string {
	if err == nil {
		return fmt.Sprintf("%s failed", toolName)
	}
	return fmt.Sprintf("%s failed: %v", toolName, err)
}
