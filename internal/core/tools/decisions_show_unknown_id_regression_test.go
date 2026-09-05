// decisions_show_unknown_id_regression_test.go — Residual pass §I-C.14.
//
// The 2026-09-05 audit found mpm_decisions.show leaked the raw SQL
// error from the underlying query for an unknown decision id. The
// previous shape propagated "sql: no rows in result set" verbatim
// through the public tool response, exposing implementation detail
// (table name, query shape) and making CLI/MCP error handling
// unstable across database runtime versions.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_decisions --payload '{"action":"show","params":{"id":"dec-nonexistent"}}'
//     # wanted: error mentioning 'decision not found'
//     # actual: error containing 'sql: no rows in result set'
//
// Canonical contract:
//
//   id omitted/empty      → ERROR: show decision: id is required
//   id = valid existing   → success, decision envelope
//   id = unknown          → ERROR: decision not found: <id>
//   (no SQL internals leak)

package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestDecisionsShow_KnownReturnsEnvelope pins the positive path:
// a recorded decision surfaces with the canonical envelope.
func TestDecisionsShow_KnownReturnsEnvelope(t *testing.T) {
	dm := newTestSharedDM(t)

	// Record a decision.
	res, err := handleMpmDecisions(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "record",
		"params": map[string]interface{}{
			"choice": "test-decision-known",
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	m, _ := res.(map[string]interface{})
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatal("seed returned no id")
	}

	// Show by id.
	showRes, err := handleMpmDecisions(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "show",
		"params": map[string]interface{}{"id": id},
	})
	if err != nil {
		t.Fatalf("show known: %v", err)
	}
	sm, _ := showRes.(map[string]interface{})
	if sm["success"] != true {
		t.Errorf("expected success=true, got %v", sm["success"])
	}
}

// TestDecisionsShow_UnknownIDReturnsNotFound pins the headline
// §I-C.14 invariant. The unknown-id path must:
//   - return an error (not success)
//   - mention 'not found'
//   - NOT mention any SQL internals ('sql:', 'no rows', table name,
//     query text)
func TestDecisionsShow_UnknownIDReturnsNotFound(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmDecisions(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "show",
		"params": map[string]interface{}{"id": "dec-nonexistent-12345"},
	})
	if err == nil {
		t.Fatal("unknown id must error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error must mention 'not found', got: %v", err)
	}
	for _, leak := range []string{"sql:", "no rows", "result set", "memories", "SELECT"} {
		if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(leak)) {
			t.Errorf("error must not leak SQL internals (%q): %v", leak, err)
		}
	}
}

// TestDecisionsShow_EmptyIDRejected pins the existing pre-check:
// empty/missing id errors before any DM call.
func TestDecisionsShow_EmptyIDRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, empty := range []interface{}{nil, ""} {
		params := map[string]interface{}{"action": "show"}
		p := map[string]interface{}{}
		if empty != nil {
			p["id"] = empty
		}
		params["params"] = p
		_, err := handleMpmDecisions(dm, mpminternal.ActiveContext{}, params)
		if err == nil {
			t.Errorf("empty id must error (val=%v)", empty)
		}
	}
}

// TestDecisionsShow_NullIDReturnsNotFound pins: a JSON null id
// (different from absent) follows the empty-id path.
func TestDecisionsShow_NullIDReturnsNotFound(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleMpmDecisions(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "show",
		"params": map[string]interface{}{"id": nil},
	})
	if err == nil {
		t.Fatal("null id must error")
	}
	if !strings.Contains(err.Error(), "id is required") &&
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("error must mention 'id is required' or 'not found', got: %v", err)
	}
}
