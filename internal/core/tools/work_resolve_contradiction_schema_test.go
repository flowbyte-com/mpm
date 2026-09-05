// work_resolve_contradiction_schema_test.go — Pass 3 defect C.10.
//
// The 2026-09-05 audit found the mpm_work.resolve_contradiction
// schema did NOT declare `reason` in properties and did NOT mark it
// as required — even though handleResolveContradictionWork at
// work_handlers.go:343-345 enforces it:
//
//   if reason == "" {
//       return nil, fmt.Errorf("reason is required for resolve_contradiction (audit trail)")
//   }
//
// An MCP client introspecting the schema would not know reason is
// required, and the schema_superset_of_handler_payload_reads guard
// test does not flag it because the AST extractor doesn't see the
// indirect read via the `reason, _ := p["reason"].(string)` pattern
// (the same pattern that was missed in pass 1).
//
// Canonical contract (per the handler):
//
//   work_id  required
//   reason   required (audit trail)
//   winner_id, winner_outcome optional
//
// Fix: declare `reason` in the schema's properties (so consumers
// see the field name) AND add it to `required` (so consumers see
// the constraint). The handler-side enforcement is already correct;
// this commit only corrects the published schema.

package tools

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestWorkResolveContradictionSchema_DeclaresReason pins that the
// schema's per-action shape for resolve_contradiction includes the
// `reason` field in its properties, with `required: [work_id, reason]`.
func TestWorkResolveContradictionSchema_DeclaresReason(t *testing.T) {
	tool, ok := ByName("mpm_work")
	if !ok {
		t.Fatal("mpm_work tool not found in Registry")
	}

	var schema struct {
		OneOf []json.RawMessage `json:"oneOf"`
	}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("mpm_work schema is not valid JSON: %v", err)
	}

	// Find the resolve_contradiction branch.
	var rcProps struct {
		Properties struct {
			Action struct {
				Const string `json:"const"`
			} `json:"action"`
			Params struct {
				Type       string                    `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                  `json:"required"`
			} `json:"params"`
		} `json:"properties"`
	}

	found := false
	for _, branch := range schema.OneOf {
		var b struct {
			Properties struct {
				Action struct {
					Const string `json:"const"`
				} `json:"action"`
				Params struct {
					Type       string                    `json:"type"`
					Properties map[string]json.RawMessage `json:"properties"`
					Required   []string                  `json:"required"`
				} `json:"params"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(branch, &b); err != nil {
			continue
		}
		if b.Properties.Action.Const != "resolve_contradiction" {
			continue
		}
		// Found the right branch.
		if _, ok := b.Properties.Params.Properties["reason"]; !ok {
			t.Errorf("resolve_contradiction schema does not declare 'reason' in properties — " +
				"consumers introspecting the schema cannot supply the audit-trail field")
		}
		required := b.Properties.Params.Required
		hasReason := false
		for _, r := range required {
			if r == "reason" {
				hasReason = true
				break
			}
		}
		if !hasReason {
			t.Errorf("resolve_contradiction schema required = %v, must include 'reason'", required)
		}
		found = true
		break
	}
	if !found {
		t.Fatalf("no oneOf branch with action=resolve_contradiction found")
	}

	// Suppress unused-variable lint on rcProps — the literal struct
	// shape is documented for readability.
	_ = rcProps
	_ = mpminternal.ActiveContext{}
}

// TestWorkResolveContradictionHandler_RequiresReason pins the
// handler-side enforcement that drives the schema fix: omitting
// reason errors loudly rather than silently mutating state. This is
// the contract that the schema must now advertise.
func TestWorkResolveContradictionHandler_RequiresReason(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := defaultACForPatch()

	_, err := handleMpmWork(dm, ac, map[string]interface{}{
		"action": "resolve_contradiction",
		"params": map[string]interface{}{
			"work_id": "nonexistent",
			// reason absent → must error, must NOT mutate state
		},
	})
	if err == nil {
		t.Fatalf("resolve_contradiction without reason must error")
	}
	if !strings.Contains(err.Error(), "reason") {
		t.Errorf("error must mention 'reason', got: %v", err)
	}
}

// helper: minimal ActiveContext for the tools package — kept local
// to avoid name collision with handlers_test.go's defaultACForTools.
func mpminternalActiveContextForTools() struct{ SessionID, Agent, Model string } {
	return struct{ SessionID, Agent, Model string }{
		SessionID: "resolve-contradiction-schema-test",
		Agent:     "audit",
		Model:     "audit-m",
	}
}
