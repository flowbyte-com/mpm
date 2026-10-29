// payload_shape_tool_entry_test.go — the payload-shape contract, exercised
// through the REAL tool entry points (issue #159).
//
// WHY THIS FILE IS SEPARATE FROM THE CLASSIFIER TESTS
// ---------------------------------------------------
// internal/core/tool_outcome_payload_shape_test.go proves the CLASSIFIER
// maps typed errors to the right class and code. This file proves the other
// half: that the tools actually PRODUCE those typed errors, at the entry
// points callers use, for every shape family — and that a well-formed
// payload still succeeds.
//
// The distinction matters because the classifier cannot fix a producer that
// does not produce. A payload-shape error that still arrives as a bare
// fmt.Errorf is still internal/unclassified, however good the classifier is.
//
// Each case below goes through the tool's own handler with the payload
// shape a caller would send, then asserts the classification the dispatch
// audit would record.
//
// Disposable file-backed databases and isolated environment roots only.
package tools

import (
	"errors"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// payloadShapeTestDM opens a disposable, file-backed DatabaseManager inside
// an isolated workspace root.
func payloadShapeTestDM(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("HOME", ws)
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", ws+"/src/db/mpm.db")

	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return dm
}

// assertValidation asserts that the error a tool returned is classified the
// way the dispatch audit will record it.
func assertValidation(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the malformed payload was accepted — it must be rejected")
	}
	class, code := mpminternal.ClassifyError(err)
	if class != mpminternal.OutcomeClassValidation {
		t.Fatalf("class = %q, want %q (error: %v)", class, mpminternal.OutcomeClassValidation, err)
	}
	if code != wantCode {
		t.Fatalf("code = %q, want %q (error: %v)", code, wantCode, err)
	}
}

// ── The four shape families, through real entry points ─────────────────────

func TestPayloadShape_ArrayFamilyThroughToolEntryPoint(t *testing.T) {
	dm := payloadShapeTestDM(t)
	ac := mpminternal.ActiveContext{}

	// mpm_decisions.record: `tags` must be an array of strings.
	_, err := handleMpmDecisions(dm, ac, map[string]interface{}{
		"action": "record",
		"params": map[string]interface{}{
			"choice": "a decision",
			"tags":   "not-an-array",
		},
	})
	assertValidation(t, err, "invalid_array_shape")

	// The element-level variant is the same family.
	_, err = handleMpmDecisions(dm, ac, map[string]interface{}{
		"action": "record",
		"params": map[string]interface{}{
			"choice": "a decision",
			"tags":   []interface{}{"ok", 42},
		},
	})
	assertValidation(t, err, "invalid_array_shape")
}

func TestPayloadShape_ObjectFamilyThroughToolEntryPoint(t *testing.T) {
	dm := payloadShapeTestDM(t)
	ac := mpminternal.ActiveContext{}

	// mpm_memory.patch: `patch` must be a JSON object.
	_, err := handleMpmMemory(dm, ac, map[string]interface{}{
		"action": "patch",
		"params": map[string]interface{}{
			"memory_id": "does-not-matter",
			"patch":     nil,
		},
	})
	assertValidation(t, err, "invalid_object_shape")

	// A non-object patch is the same family.
	_, err = handleMpmMemory(dm, ac, map[string]interface{}{
		"action": "patch",
		"params": map[string]interface{}{
			"memory_id": "does-not-matter",
			"patch":     "not-an-object",
		},
	})
	assertValidation(t, err, "invalid_object_shape")
}

func TestPayloadShape_NumericFieldFamilyThroughToolEntryPoint(t *testing.T) {
	dm := payloadShapeTestDM(t)
	ac := mpminternal.ActiveContext{}

	// mpm_memory.query: `limit` must be a number.
	_, err := handleMpmMemory(dm, ac, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": "anything",
			"limit": "ten",
		},
	})
	assertValidation(t, err, "invalid_numeric_field")
}

func TestPayloadShape_EnvelopeFamilyThroughToolEntryPoint(t *testing.T) {
	dm := payloadShapeTestDM(t)
	ac := mpminternal.ActiveContext{}

	// A parameter at the TOP LEVEL instead of inside the params envelope.
	// This is the shape a host adapter produces when it nests wrongly, and
	// it is its own family: the fix is structural, not a value correction.
	_, err := handleMpmMemory(dm, ac, map[string]interface{}{
		"action": "query",
		"query":  "anything",
	})
	assertValidation(t, err, "invalid_params_envelope")

	// `params` present but not an object.
	_, err = handleMpmMemory(dm, ac, map[string]interface{}{
		"action": "query",
		"params": "not-an-object",
	})
	assertValidation(t, err, "invalid_params_envelope")
}

// ── The handoff ended_state enum, at the tool boundary ────────────────────

func TestPayloadShape_HandoffEndedStateEnumAtToolBoundary(t *testing.T) {
	dm := payloadShapeTestDM(t)
	ac := mpminternal.ActiveContext{}

	_, err := handleMpmHandoff(dm, ac, map[string]interface{}{
		"action": "write",
		"params": map[string]interface{}{
			"summary": "a summary",
			"state":   "bogus",
		},
	})
	assertValidation(t, err, "invalid_enum_value")

	// The error names the four permitted values, so the caller does not
	// have to go and look them up.
	if err == nil {
		t.Fatal("no error")
	}
	for _, allowed := range mpminternal.HandoffEndedStates {
		if !strings.Contains(err.Error(), allowed) {
			t.Errorf("error %q does not name the permitted value %q", err.Error(), allowed)
		}
	}

	// And the four permitted values are accepted.
	for _, allowed := range mpminternal.HandoffEndedStates {
		res, err := handleMpmHandoff(dm, ac, map[string]interface{}{
			"action": "write",
			"params": map[string]interface{}{
				"summary": "a summary for " + allowed,
				"state":   allowed,
			},
		})
		if err != nil {
			t.Errorf("state %q was rejected: %v", allowed, err)
			continue
		}
		if res == nil {
			t.Errorf("state %q produced no result", allowed)
		}
	}
}

// ── Non-vacuity: the fix does not break well-formed payloads ──────────────

// A validator that rejects everything would satisfy every assertion above.
// These cases pin the other half: the SAME tools, with the SAME fields,
// still accept the shapes they are supposed to accept.
func TestPayloadShape_ValidPayloadsStillSucceed(t *testing.T) {
	dm := payloadShapeTestDM(t)
	ac := mpminternal.ActiveContext{}

	// mpm_decisions.record with a proper string array.
	res, err := handleMpmDecisions(dm, ac, map[string]interface{}{
		"action": "record",
		"params": map[string]interface{}{
			"choice": "a decision",
			"tags":   []interface{}{"one", "two"},
		},
	})
	if err != nil {
		t.Fatalf("a valid tags array was rejected: %v", err)
	}
	if res == nil {
		t.Fatal("a valid record produced no result")
	}

	// mpm_memory.save with a numeric weight.
	res, err = handleMpmMemory(dm, ac, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact":   "a memory",
			"weight": 2.5,
		},
	})
	if err != nil {
		t.Fatalf("a valid weight was rejected: %v", err)
	}
	if res == nil {
		t.Fatal("a valid save produced no result")
	}

	// mpm_memory.query with a numeric limit.
	res, err = handleMpmMemory(dm, ac, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": "memory",
			"limit": 5,
		},
	})
	if err != nil {
		t.Fatalf("a valid limit was rejected: %v", err)
	}
	if res == nil {
		t.Fatal("a valid query produced no result")
	}
}

// The typed error must not change the message a caller sees. Several
// existing tests pin these strings exactly; this one pins the ones the
// live audit actually recorded, so a future reword is a deliberate act.
func TestPayloadShape_CallerFacingMessagesAreUnchanged(t *testing.T) {
	dm := payloadShapeTestDM(t)
	ac := mpminternal.ActiveContext{}

	_, err := handleMpmDecisions(dm, ac, map[string]interface{}{
		"action": "record",
		"params": map[string]interface{}{
			"choice": "a decision",
			"tags":   "not-an-array",
		},
	})
	if err == nil {
		t.Fatal("no error")
	}
	if got, want := err.Error(), "tags must be an array of strings, got string"; got != want {
		t.Errorf("message = %q, want %q — the typed error must not reword what the caller sees", got, want)
	}

	_, err = handleMpmMemory(dm, ac, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": "anything",
			"limit": "ten",
		},
	})
	if err == nil {
		t.Fatal("no error")
	}
	if got, want := err.Error(), "field `limit` must be a number (float64/int), got string"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// The error must reach the dispatch audit as a TYPED error, so both
// transports (CLI and MCP) classify it identically. Both audit hooks call
// the same core.ClassifyError; this asserts the value they would receive.
func TestPayloadShape_TheErrorTheAuditWouldSeeIsClassified(t *testing.T) {
	dm := payloadShapeTestDM(t)
	ac := mpminternal.ActiveContext{}

	_, err := handleMpmMemory(dm, ac, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": "anything",
			"limit": "ten",
		},
	})
	if err == nil {
		t.Fatal("no error")
	}
	// The audit hooks compute the class from the error VALUE, not from a
	// re-parse of the message. Assert the value itself carries the type.
	var shape *mpminternal.PayloadShapeError
	if !errors.As(err, &shape) {
		t.Fatalf("the tool returned %T, not a *PayloadShapeError — the audit "+
			"would fall back to message text", err)
	}
	if shape.Field != "limit" {
		t.Errorf("shape.Field = %q, want %q", shape.Field, "limit")
	}
	if shape.OutcomeCode() != "invalid_numeric_field" {
		t.Errorf("shape.OutcomeCode() = %q, want invalid_numeric_field", shape.OutcomeCode())
	}
	// The field never carries the value: an outcome_code and an error
	// message both land in the audit trail.
	if strings.Contains(shape.Field, "ten") {
		t.Errorf("shape.Field %q carries the offending VALUE", shape.Field)
	}
}
