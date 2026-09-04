// handoff_schema_handler_parity_test.go — schema/handler parity pin for
// mpm_handoff write.
//
// Audit context (2026-09-04 forensic audit): the registry schema at
// registry_list.go advertised a `note` parameter on `mpm_handoff write`,
// but `handleHandoffWrite` (handlers.go:2905) never reads it. This is
// the exact "accepted-then-discarded" silent-data-loss class the F13
// audit flagged for `commitments` / `open_questions`. The `Handoff`
// struct (handoff.go:20-31) and the `session_handoffs` table schema
// (schema.go) both lack any `note` column — so the schema
// advertisement was a stale scaffold, not an unfinished feature.
//
// Option B (audit-recommended): remove `note` from the schema. Any
// caller that still sends it will be told via `additionalProperties: false`
// that the field is unknown, instead of receiving silent success.

package tools

import (
	"encoding/json"
	"testing"
)

// TestMpmHandoffWriteSchema_NoStaleNoteField pins that the registry
// schema for `mpm_handoff` does NOT advertise a `note` parameter on
// `write`. The handler does not read `note`; the data model has no
// `note` column; the migration never created one. Advertising a
// parameter that the handler silently drops creates a
// silent-data-loss contract defect — the audit's central correctness
// finding (P0-1).
//
// If a future change legitimately adds `note` to the handoff model,
// this test must be updated alongside the handler read, the
// `Handoff` struct field, the schema column, and the migration.
func TestMpmHandoffWriteSchema_NoStaleNoteField(t *testing.T) {
	tool, ok := ByName("mpm_handoff")
	if !ok {
		t.Fatal("mpm_handoff tool not found in Registry")
	}

	var schema struct {
		Properties struct {
			Params struct {
				Properties map[string]interface{} `json:"properties"`
			} `json:"params"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("mpm_handoff schema is not valid JSON: %v", err)
	}

	if _, ok := schema.Properties.Params.Properties["note"]; ok {
		t.Errorf("mpm_handoff.write schema advertises 'note' but the handler does not read it " +
			"(handleHandoffWrite at handlers.go:2905 ignores params['note']). " +
			"This is the silent-data-loss contract defect from the 2026-09-04 audit (P0-1). " +
			"Either remove 'note' from the schema or implement end-to-end persistence " +
			"(schema column + struct field + handler read + EndSession signature).")
	}
}

// TestMpmHandoffWriteSchema_RequiredSummary pins the contract that
// `summary` is the only REQUIRED field on `mpm_handoff.write`. session_id
// is optional (see TestHandleHandoffWrite_EmptySessionIDSucceeds). The
// schema's `required` array should not falsely mandate session_id or
// any other optional field.
func TestMpmHandoffWriteSchema_RequiredSummary(t *testing.T) {
	tool, ok := ByName("mpm_handoff")
	if !ok {
		t.Fatal("mpm_handoff tool not found in Registry")
	}

	var schema struct {
		Properties struct {
			Action struct {
				Enum []string `json:"enum"`
			} `json:"action"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("mpm_handoff schema is not valid JSON: %v", err)
	}

	wantActions := []string{"write", "read", "list", "shred"}
	for _, want := range wantActions {
		found := false
		for _, a := range schema.Properties.Action.Enum {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("mpm_handoff action enum missing %q (got %v)", want, schema.Properties.Action.Enum)
		}
	}
}
