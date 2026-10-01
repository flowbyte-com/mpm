// call_partial_result_envelope_test.go — regression for the loss of
// partial results at the `mpm call` transport.
//
// A handler may return a structured result ALONGSIDE a non-nil error.
// The compact action does exactly this on a mid-drain batch failure,
// and its diagnostic (batches_processed, raw_remaining, failed_batch,
// failure_reason) is the only way an operator learns which batch failed
// and how much work already committed.
//
// The transport used to collapse all of that into
// {"success":false,"error":"..."} — the information existed at the point
// of loss and was discarded afterwards.
//
// The helper's own contract is pinned in
// internal/core/tools/partial_result_regression_test.go. This file pins
// the CLI WIRING: that handleCall routes the (result, err) pair through
// the helper rather than through a hand-rolled two-key literal, and that
// the stdout envelope still satisfies the documented `mpm call` contract.

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/tools"
)

// TestErrorEnvelope_PreservesDiagnosticOnStdout is the contract test for
// the CLI surface: a partial result must reach stdout, and the envelope
// must remain valid JSON on a single line.
func TestErrorEnvelope_PreservesDiagnosticOnStdout(t *testing.T) {
	result := map[string]interface{}{
		"success":           false,
		"batches_processed": 3,
		"raw_processed":     150,
		"lessons_created":   3,
		"raw_remaining":     110,
		"stop_reason":       "failure",
		"failed_batch":      4,
		"failure_reason":    "synthesis timeout",
	}

	var out strings.Builder
	writeEnvelope(&out, tools.ErrorEnvelopeWithResult(result, errTimeout))

	// The envelope must be ONE line of JSON: adapters parse stdout line
	// by line, and a pretty-printed envelope would break them.
	line := out.String()
	if strings.Count(strings.TrimRight(line, "\n"), "\n") != 0 {
		t.Fatalf("envelope must be a single line, got:\n%s", line)
	}

	var env map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimRight(line, "\n")), &env); err != nil {
		t.Fatalf("stdout envelope must be parseable JSON: %v\noutput: %s", err, line)
	}

	for field, want := range map[string]interface{}{
		"batches_processed": float64(3),
		"raw_remaining":     float64(110),
		"stop_reason":       "failure",
		"failed_batch":      float64(4),
		"failure_reason":    "synthesis timeout",
	} {
		if got := env[field]; got != want {
			t.Errorf("field %q = %v, want %v — the partial diagnostic was lost at the transport", field, got, want)
		}
	}
}

// TestErrorEnvelope_NoResultIsUnchanged pins the compatibility half: the
// common (nil, err) case must still produce exactly the documented
// two-key envelope. If this changes, every existing adapter that keys on
// {"success":false,"error":...} is affected.
func TestErrorEnvelope_NoResultIsUnchanged(t *testing.T) {
	var out strings.Builder
	writeEnvelope(&out, tools.ErrorEnvelopeWithResult(nil, errTimeout))

	// Byte-level equality with the literal envelope the transport wrote
	// before partial-result preservation existed. Shape equality would not
	// be enough: a shell adapter doing an exact-match comparison on the
	// whole line would break on a reordering or an added key, and
	// json.Marshal emits map keys in sorted order, so the comparison is
	// stable rather than incidental.
	const want = `{"error":"synthesis timeout","success":false}` + "\n"
	if out.String() != want {
		t.Errorf("nil-result envelope changed.\n got: %s\nwant: %s", out.String(), want)
	}

	var env map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimRight(out.String(), "\n")), &env); err != nil {
		t.Fatalf("envelope must be parseable JSON: %v", err)
	}
	if len(env) != 2 {
		t.Errorf("envelope has %d keys %v, want exactly 2 (success, error)", len(env), env)
	}
	if env["success"] != false {
		t.Errorf("success = %v, want false", env["success"])
	}
	if env["error"] != errTimeout.Error() {
		t.Errorf("error = %v, want %q", env["error"], errTimeout.Error())
	}
}

// TestErrorEnvelope_FailedCallStillFails pins the safety property at the
// CLI surface: preserving a diagnostic must never make a failed call look
// successful. success stays false.
func TestErrorEnvelope_FailedCallStillFails(t *testing.T) {
	result := map[string]interface{}{"success": true, "stop_reason": "completed"}

	var out strings.Builder
	writeEnvelope(&out, tools.ErrorEnvelopeWithResult(result, errTimeout))

	var env map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimRight(out.String(), "\n")), &env); err != nil {
		t.Fatalf("envelope must be parseable JSON: %v", err)
	}
	if env["success"] != false {
		t.Errorf("success = %v, want false: a failed call must never be reported as successful", env["success"])
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "synthesis timeout" }

var errTimeout = timeoutErr{}
