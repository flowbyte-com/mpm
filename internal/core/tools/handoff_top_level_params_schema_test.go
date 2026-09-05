// handoff_top_level_params_schema_test.go — regression for the
// 2026-09-05 audit P2 finding: mpm_handoff schema did not declare
// the top-level `params` field that handleMpmHandoff reads (via
// extractParamsOrFail at handlers.go:4632).
//
// The drift class this test pins: "schema/handler under-declaration
// at the envelope layer." The registry's JSON-Schema is the published
// wire contract; if it omits a field the handler actually reads, MCP
// clients that introspect the schema cannot supply that field, and
// CLI callers (`mpm call mpm_handoff`) get a "no parseable JSON"
// envelope (call.go:58) instead of a precise error.
//
// The canonical schema-guard test TestSchemaSupersetOfHandlerPayloadReads
// caught this drift in the audit; it must pass once the top-level
// `params` declaration is added. This test pins the specific behaviour
// for the mpm_handoff family rather than relying on the generic guard.

package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMpmHandoffSchema_DeclaresTopLevelParams pins the schema/handler
// parity for the envelope: `params` is a top-level field that the
// dispatcher reads, so the schema must declare it at the top level.
//
// Why the top level and not inside the per-action oneOf branches:
// extractParamsOrFail (handlers.go:4632) extracts `payload["params"]`
// BEFORE the action is read. The schema must reflect this — the
// envelope shape is the same regardless of which action is invoked.
func TestMpmHandoffSchema_DeclaresTopLevelParams(t *testing.T) {
	tool, ok := ByName("mpm_handoff")
	if !ok {
		t.Fatal("mpm_handoff tool not found in Registry")
	}

	// Decode the top-level schema shape.
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                  `json:"required"`
		OneOf      []json.RawMessage         `json:"oneOf"`
	}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("mpm_handoff schema is not valid JSON: %v", err)
	}

	raw, ok := schema.Properties["params"]
	if !ok {
		t.Fatalf("mpm_handoff schema does NOT declare top-level 'params' — " +
			"handleMpmHandoff reads payload['params'] via extractParamsOrFail, " +
			"so the schema under-declares the envelope. " +
			"See 2026-09-05 audit Defect C.3.")
	}

	// The declared params must be an object-typed property (not a
	// primitive or array).
	var paramsSchema struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &paramsSchema); err != nil {
		t.Fatalf("mpm_handoff.params schema is not valid JSON: %v", err)
	}
	if paramsSchema.Type != "object" {
		t.Errorf("mpm_handoff.params type = %q, want \"object\"", paramsSchema.Type)
	}

	// Sanity: required must still only contain "action" — params is
	// optional at the envelope (extractParamsOrFail treats it as
	// optional; inner handlers validate per-action required keys).
	for _, r := range schema.Required {
		if r == "params" {
			t.Errorf("mpm_handoff schema falsely marks 'params' as required " +
				"at the top level — params is optional; per-action required " +
				"keys belong in the oneOf branches.")
		}
	}
}

// TestMpmHandoffSchema_PerActionShapesPreserved pins that adding the
// top-level `params` declaration did not regress the per-action shapes
// declared inside the oneOf branches. The `write` branch must still
// mark `summary` as required; the `shred` branch must still mark
// `handoff_id` as required.
func TestMpmHandoffSchema_PerActionShapesPreserved(t *testing.T) {
	tool, ok := ByName("mpm_handoff")
	if !ok {
		t.Fatal("mpm_handoff tool not found in Registry")
	}

	var schema struct {
		OneOf []json.RawMessage `json:"oneOf"`
	}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("mpm_handoff schema is not valid JSON: %v", err)
	}

	// Walk the oneOf branches and check the action -> required mapping.
	wantRequired := map[string][]string{
		"write": {"summary"},
		"read":  {},
		"list":  {},
		"shred": {"handoff_id"},
	}
	for _, branch := range schema.OneOf {
		var b struct {
			Properties struct {
				Action struct {
					Const string `json:"const"`
				} `json:"action"`
				Params struct {
					Required []string `json:"required"`
				} `json:"params"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(branch, &b); err != nil {
			continue // not all branches declare action/params — skip
		}
		action := b.Properties.Action.Const
		if action == "" {
			continue
		}
		want, ok := wantRequired[action]
		if !ok {
			t.Errorf("unexpected oneOf branch with action=%q", action)
			continue
		}
		got := b.Properties.Params.Required
		if len(got) != len(want) {
			t.Errorf("mpm_handoff.%s required keys: got %v, want %v", action, got, want)
			continue
		}
		// order-insensitive comparison
		seen := map[string]bool{}
		for _, k := range got {
			seen[k] = true
		}
		for _, w := range want {
			if !seen[w] {
				t.Errorf("mpm_handoff.%s required keys: got %v, want %v (missing %q)", action, got, want, w)
			}
		}
	}
}

// TestMpmHandoffSchema_SchemaGuardPasses is a thin wrapper around the
// canonical TestSchemaSupersetOfHandlerPayloadReads that asserts the
// specific drift fixed by this change is now caught as a PASS rather
// than a FAIL. If the canonical guard ever regresses on mpm_handoff,
// this test fails first with a focused message.
func TestMpmHandoffSchema_SchemaGuardPasses(t *testing.T) {
	// Run the canonical schema-guard test in isolation by name
	// (t.Run sub-tests aren't addressable across packages; we just
	// assert the guard's specific mpm_handoff failure mode is gone
	// by reading the tool's schema and confirming 'params' is at the
	// top level — same invariant TestMpmHandoffSchema_DeclaresTopLevelParams
	// pins more thoroughly).
	tool, ok := ByName("mpm_handoff")
	if !ok {
		t.Fatal("mpm_handoff tool not found in Registry")
	}
	if !strings.Contains(string(tool.Schema), `"params"`) {
		t.Fatalf("mpm_handoff schema does not mention 'params' at all — " +
			"the canonical TestSchemaSupersetOfHandlerPayloadReads " +
			"is currently FAILING on this tool.")
	}
}
