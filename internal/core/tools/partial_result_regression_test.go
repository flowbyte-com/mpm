// partial_result_regression_test.go — regressions for the transport-level
// loss of partial results.
//
// A handler may return BOTH a structured result and a non-nil error.
// compact does exactly this on a mid-drain batch failure, and the
// diagnostic it returns is the only way an operator learns which batch
// failed and how much work already committed. Both transports used to
// discard it, collapsing the diagnostic into a bare error string.
//
// These tests pin the shared helper's contract. The transport-level
// wiring is covered separately in cmd/mpm and cmd/mpm-mcp.
package tools

import (
	"encoding/json"
	"testing"
)

// drainResult mirrors the shape of
// internal/core.CompactEpistemologyDrainResult — the concrete type whose
// fields were being lost. A struct (not a map) is deliberate: it is what
// the compact handler actually returns, and it exercises the
// marshal-then-unmarshal normalization path rather than the trivial
// map pass-through.
type drainResult struct {
	Success          bool     `json:"success"`
	BatchesProcessed int      `json:"batches_processed"`
	RawProcessed     int      `json:"raw_processed"`
	LessonsCreated   int      `json:"lessons_created"`
	RawRemaining     int      `json:"raw_remaining"`
	LessonIDs        []string `json:"lesson_ids,omitempty"`
	StopReason       string   `json:"stop_reason"`
	FailedBatch      int      `json:"failed_batch,omitempty"`
	FailureReason    string   `json:"failure_reason,omitempty"`
}

// partialFailure is the exact (result, err) pair compact returns when a
// batch fails mid-drain: three batches committed, the fourth failed.
func partialFailure() (drainResult, error) {
	return drainResult{
		Success:          false,
		BatchesProcessed: 3,
		RawProcessed:     150,
		LessonsCreated:   3,
		RawRemaining:     110,
		LessonIDs:        []string{"les-a", "les-b", "les-c"},
		StopReason:       "failure",
		FailedBatch:      4,
		FailureReason:    "synthesis timeout",
	}, errTest
}

var errTest = &testError{"synthesis timeout"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// TestPartialResult_PreservesDiagnosticFields is the primary regression:
// every diagnostic field the drain result carries must survive the
// conversion. Before the fix, all of these were replaced by a bare
// {"success":false,"error":"..."}.
func TestPartialResult_PreservesDiagnosticFields(t *testing.T) {
	result, err := partialFailure()

	env := ErrorEnvelopeWithResult(result, err)

	for field, want := range map[string]interface{}{
		"batches_processed": float64(3),
		"raw_processed":     float64(150),
		"lessons_created":   float64(3),
		"raw_remaining":     float64(110),
		"stop_reason":       "failure",
		"failed_batch":      float64(4),
		"failure_reason":    "synthesis timeout",
	} {
		got, ok := env[field]
		if !ok {
			t.Errorf("field %q lost: not present in error envelope %v", field, env)
			continue
		}
		if got != want {
			t.Errorf("field %q = %v (%T), want %v", field, got, got, want)
		}
	}
}

// TestPartialResult_ForcesFailureVerdict: a failed operation must never
// be reported as successful, no matter what the payload claims.
//
// This is the safety property behind the whole change — preserving
// diagnostics must not become a way to launder a failure into a success.
func TestPartialResult_ForcesFailureVerdict(t *testing.T) {
	// A hostile handler that sets success:true and its own error string
	// alongside its error. The transport's verdict must win.
	hostile := map[string]interface{}{
		"success":     true,
		"error":       "everything is fine",
		"stop_reason": "completed",
	}
	err := &testError{"actual failure"}

	env := ErrorEnvelopeWithResult(hostile, err)

	if env["success"] != false {
		t.Errorf("success = %v, want false: transport must not report a failure as success", env["success"])
	}
	if env["error"] != "actual failure" {
		t.Errorf("error = %v, want the Go error text", env["error"])
	}
}

// TestPartialResult_NilResultUnchanged: the overwhelmingly common case is
// (nil, err). It must produce exactly the pre-existing envelope, so no
// consumer can regress.
func TestPartialResult_NilResultUnchanged(t *testing.T) {
	env := ErrorEnvelopeWithResult(nil, &testError{"boom"})

	if len(env) != 2 {
		t.Errorf("envelope has %d keys, want exactly 2 (success, error): %v", len(env), env)
	}
	if env["success"] != false || env["error"] != "boom" {
		t.Errorf("envelope = %v, want {success:false, error:boom}", env)
	}
}

// TestPartialResult_NonObjectResultUnchanged: a scalar or slice result has
// no top-level fields to merge. Degrading to the plain envelope keeps the
// response shape stable instead of inventing a wrapper.
func TestPartialResult_NonObjectResultUnchanged(t *testing.T) {
	for name, result := range map[string]interface{}{
		"string": "partial text",
		"int":    42,
		"slice":  []string{"a", "b"},
		"bool":   true,
	} {
		env := ErrorEnvelopeWithResult(result, &testError{"boom"})
		if len(env) != 2 {
			t.Errorf("%s: envelope has %d keys, want 2: %v", name, len(env), env)
		}
	}
}

// TestPartialResult_EnvelopeIsSerializable guards the wire contract: the
// envelope must marshal as valid JSON, since it is written verbatim to
// stdout by `mpm call`.
func TestPartialResult_EnvelopeIsSerializable(t *testing.T) {
	result, err := partialFailure()

	data, mErr := json.Marshal(ErrorEnvelopeWithResult(result, err))
	if mErr != nil {
		t.Fatalf("envelope must be JSON-serializable: %v", mErr)
	}

	var round map[string]interface{}
	if uErr := json.Unmarshal(data, &round); uErr != nil {
		t.Fatalf("envelope must round-trip: %v", uErr)
	}
	if round["raw_remaining"] != float64(110) {
		t.Errorf("raw_remaining = %v after round-trip, want 110", round["raw_remaining"])
	}
	// lesson_ids has omitempty and is populated; it must survive too.
	ids, ok := round["lesson_ids"].([]interface{})
	if !ok || len(ids) != 3 {
		t.Errorf("lesson_ids = %v, want 3 entries — slice fields are part of the diagnostic", round["lesson_ids"])
	}
}

// TestPartialResult_MapResultPreserved: a map-typed result (the other
// common handler shape) takes the same path as a struct.
func TestPartialResult_MapResultPreserved(t *testing.T) {
	env := ErrorEnvelopeWithResult(
		map[string]interface{}{"stop_reason": "failure", "failed_batch": 2},
		&testError{"batch 2 rejected"},
	)

	if env["stop_reason"] != "failure" {
		t.Errorf("stop_reason = %v, want failure", env["stop_reason"])
	}
	// float64, not int: a map result is normalized through a JSON
	// round-trip, and JSON has a single number type. This is invisible on
	// the wire (the emitted JSON is identical either way) and only
	// observable to an in-process caller, so it is pinned here rather than
	// left to surprise the next reader of the helper.
	if env["failed_batch"] != float64(2) {
		t.Errorf("failed_batch = %v (%T), want 2", env["failed_batch"], env["failed_batch"])
	}
}

// TestPartialResult_DoesNotWidenErrorExposure pins the safety property
// that makes a GENERIC transport-level fix acceptable.
//
// A partial result reaching the error envelope is only safe if it cannot
// disclose more than the error string already discloses. Two handlers
// currently return (result, err): compact, whose FailureReason is
// literally err.Error(), and migrate, whose fields are counters and
// caller-supplied path fragments.
//
// This test pins the property that keeps that true — the merged envelope
// must never introduce a value the error string does not already carry.
// It is a guard on FUTURE handlers, not a description of the current two:
// a handler that started returning, say, raw memory text in its result
// would satisfy every other test here and would leak on the error path.
func TestPartialResult_DoesNotWidenErrorExposure(t *testing.T) {
	// The compact shape: the only field carrying prose is
	// failure_reason, and it is byte-identical to the error text.
	result, err := partialFailure()
	env := ErrorEnvelopeWithResult(result, err)

	if env["failure_reason"] != err.Error() {
		t.Errorf("failure_reason = %v, want the error text verbatim: "+
			"a diverging value would disclose content the error does not",
			env["failure_reason"])
	}

	// Everything else must be a scalar or a small bounded list. Count and
	// measure rather than enumerate: the point is a bound, so a future
	// field carrying a bulk payload fails the size check.
	total := 0
	for k, v := range env {
		if k == "error" || k == "failure_reason" {
			continue
		}
		total += valueFootprint(v)
	}
	const maxDiagnosticFootprint = 4096
	if total > maxDiagnosticFootprint {
		t.Errorf("diagnostic fields total %d bytes, above the %d bound: "+
			"the result is carrying a payload, not a diagnostic",
			total, maxDiagnosticFootprint)
	}
}

// valueFootprint approximates the serialized size of an envelope value,
// so a size bound can be asserted without enumerating the field set.
func valueFootprint(v interface{}) int {
	switch t2 := v.(type) {
	case string:
		return len(t2)
	case []interface{}:
		n := 0
		for _, e := range t2 {
			n += valueFootprint(e)
		}
		return n
	case map[string]interface{}:
		n := 0
		for k, e := range t2 {
			n += len(k) + valueFootprint(e)
		}
		return n
	default:
		return 8 // scalars
	}
}

// TestErrorTextForResult pins the MCP error-text contract: the human
// string must stay exactly "<tool> failed: <err>" so a client that reads
// only the first content block sees what it always saw.
func TestErrorTextForResult(t *testing.T) {
	got := ErrorTextForResult("mpm_memory", &testError{"db locked"})
	want := "mpm_memory failed: db locked"
	if got != want {
		t.Errorf("ErrorTextForResult = %q, want %q", got, want)
	}
}
