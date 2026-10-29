// outcome_payload_shape_audit_test.go — the end-to-end evidence for issue
// #159: a malformed payload is recorded in the dispatch audit as a
// VALIDATION failure, and the historical rows are left exactly as they
// were.
//
// WHY THIS FILE IS AT THE CLI LAYER
// ---------------------------------
// internal/core/tools/payload_shape_tool_entry_test.go proves the tools
// produce typed errors. internal/core/tool_outcome_payload_shape_test.go
// proves the classifier maps them correctly. Neither touches the audit
// trail.
//
// This file closes the loop through the real write path — the same
// recordToolInvocation both `mpm call` and the MCP adapter use — so the
// claim "the live trail will stop filling up with caller noise" is
// demonstrated rather than asserted.
//
// The second half is the historical-record guarantee: the classification is
// computed at WRITE time, so the 45 rows already in the live trail keep the
// classification they were written with. Nothing backfills them.

package main

import (
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestAudit_MalformedPayloadIsRecordedAsValidation is the end-to-end proof.
//
// Before this change, this exact call wrote:
//
//	outcome_class = 'internal', outcome_code = 'unclassified'
//
// which is the class the live audit found 43 % full of agent noise.
func TestAudit_MalformedPayloadIsRecordedAsValidation(t *testing.T) {
	dm := newTestDMForCmd(t)

	// mpm_memory.query with a string `limit`.
	_, err := runHandler(dm, "mpm_memory", map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": "anything",
			"limit": "ten",
		},
	})
	if err == nil {
		t.Fatal("the malformed payload was accepted")
	}

	var class, code string
	row := dm.SQLDB().QueryRow(`
		SELECT outcome_class, outcome_code FROM tool_invocations
		WHERE tool_name = 'mpm_memory' ORDER BY started_at DESC LIMIT 1`)
	if err := row.Scan(&class, &code); err != nil {
		t.Fatalf("scan audit row: %v", err)
	}
	if class != string(mpminternal.OutcomeClassValidation) {
		t.Errorf("outcome_class = %q, want %q", class, mpminternal.OutcomeClassValidation)
	}
	if code != "invalid_numeric_field" {
		t.Errorf("outcome_code = %q, want invalid_numeric_field", code)
	}
}

// TestAudit_EnvelopeAndArrayFamiliesAreRecordedAsValidation covers the two
// other shape families the live audit measured, at the audit layer.
func TestAudit_EnvelopeAndArrayFamiliesAreRecordedAsValidation(t *testing.T) {
	dm := newTestDMForCmd(t)

	cases := []struct {
		name    string
		tool    string
		payload map[string]interface{}
		want    string
	}{
		{
			name: "array",
			tool: "mpm_decisions",
			payload: map[string]interface{}{
				"action": "record",
				"params": map[string]interface{}{
					"choice": "a decision",
					"tags":   "not-an-array",
				},
			},
			want: "invalid_array_shape",
		},
		{
			name: "envelope",
			tool: "mpm_memory",
			payload: map[string]interface{}{
				"action": "query",
				"query":  "anything",
			},
			want: "invalid_params_envelope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runHandler(dm, tc.tool, tc.payload)
			if err == nil {
				t.Fatal("the malformed payload was accepted")
			}
			var class, code string
			row := dm.SQLDB().QueryRow(`
				SELECT outcome_class, outcome_code FROM tool_invocations
				WHERE tool_name = ? ORDER BY started_at DESC LIMIT 1`, tc.tool)
			if err := row.Scan(&class, &code); err != nil {
				t.Fatalf("scan audit row: %v", err)
			}
			if class != string(mpminternal.OutcomeClassValidation) {
				t.Errorf("outcome_class = %q, want %q", class, mpminternal.OutcomeClassValidation)
			}
			if code != tc.want {
				t.Errorf("outcome_code = %q, want %q", code, tc.want)
			}
		})
	}
}

// TestAudit_HistoricalRowsAreNotRewritten is the historical-record
// guarantee.
//
// The 45 rows already in the live trail were written with
// internal/unclassified. The classification is computed at write time and
// nothing backfills it, so those rows keep the classification they were
// written with. This test writes a row in exactly that historical shape,
// runs a new invocation, and asserts the old row is untouched.
func TestAudit_HistoricalRowsAreNotRewritten(t *testing.T) {
	dm := newTestDMForCmd(t)

	// A row in the shape the live trail already contains.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id,
		     actor_kind, framework_name, payload_hash, result_status,
		     started_at, completed_at, duration_ms, error_message,
		     outcome_class, outcome_code)
		VALUES ('historical-row', 'old-session', 'mpm_memory', 'query', 'old-invocation',
		        'agent', 'opencode', 'deadbeef', 'error',
		        1700000000, 1700000001, 1,
		        'tags must be an array of strings, got string',
		        'internal', 'unclassified')`); err != nil {
		t.Fatalf("insert historical row: %v", err)
	}

	// A new invocation, which classifies correctly.
	if _, err := runHandler(dm, "mpm_memory", map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": "anything",
			"limit": "ten",
		},
	}); err == nil {
		t.Fatal("the malformed payload was accepted")
	}

	// The historical row still says what it always said.
	var class, code string
	row := dm.SQLDB().QueryRow(`
		SELECT outcome_class, outcome_code FROM tool_invocations WHERE id = 'historical-row'`)
	if err := row.Scan(&class, &code); err != nil {
		t.Fatalf("scan historical row: %v", err)
	}
	if class != "internal" || code != "unclassified" {
		t.Errorf("the historical row was rewritten to %s/%s — the classification "+
			"is computed at write time and historical records must not move", class, code)
	}
}
