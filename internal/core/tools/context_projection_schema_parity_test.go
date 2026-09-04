// context_projection_schema_parity_test.go — schema/handler parity pin for
// mpm_context read_wake_context's `projection` parameter.
//
// Audit context (2026-09-04 forensic audit): the registry schema for
// `mpm_context` did not document the `projection` parameter that
// handleReadWakeContext at handlers.go:1798 reads:
//
//	if projection, _ := params["projection"].(string); projection == "compact" {
//	    return handleReadWakeContextCompact(dm)
//	}
//
// The handler is the source of truth and gates a meaningful behavior
// difference (compact envelope vs full WakeContextData). Because the
// schema didn't advertise `projection`, an agent reading the schema
// had no way to know the parameter existed.
//
// Fix: document the wake-context projection enum (`compact`) honestly
// in the schema. The schema currently has `additionalProperties: true`,
// so adding the property is purely a documentation disclosure — it
// does not tighten validation.

package tools

import (
	"encoding/json"
	"testing"
)

// TestMpmContextSchema_ProjectsCompactProjection pins that the
// `mpm_context` schema documents the `projection` parameter on
// `read_wake_context`. If the handler ever stops reading it (the
// audit's central correctness finding: schema/handler parity), this
// test will need to be updated alongside the handler change.
//
// Currently the handler reads only `compact`. Other canonical
// projection values (`summary`, `full`) belong to mpm_memory query,
// not mpm_context wake context — the wake-context behavior is
// binary: compact OR full (the default branch). The schema should
// reflect the *actual* enum read by the handler, not the
// cross-tool projection vocabulary.
func TestMpmContextSchema_ProjectsCompactProjection(t *testing.T) {
	tool, ok := ByName("mpm_context")
	if !ok {
		t.Fatal("mpm_context tool not found in Registry")
	}

	var schema struct {
		Properties struct {
			Params struct {
				Properties map[string]struct {
					Type string   `json:"type"`
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"params"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("mpm_context schema is not valid JSON: %v", err)
	}

	proj, ok := schema.Properties.Params.Properties["projection"]
	if !ok {
		t.Errorf("mpm_context schema does not advertise 'projection' parameter, "+
			"but handleReadWakeContext (handlers.go:1798) reads params['projection'] "+
			"and gates the compact-vs-full envelope shape on it. This is the "+
			"schema/handler parity defect from the 2026-09-04 audit (P0-2). "+
			"Add 'projection' to the mpm_context.params.properties schema.")
		return
	}

	// Compact is the only wake-context-specific projection value —
	// any other value falls through to the default (full) branch.
	// Honest enum = ["compact"].
	if proj.Type != "string" {
		t.Errorf("mpm_context.projection.type = %q, want \"string\"", proj.Type)
	}
	foundCompact := false
	for _, e := range proj.Enum {
		if e == "compact" {
			foundCompact = true
			break
		}
	}
	if !foundCompact {
		t.Errorf("mpm_context.projection.enum = %v, want it to include \"compact\" "+
			"(the only wake-context projection value the handler reads)", proj.Enum)
	}
}
