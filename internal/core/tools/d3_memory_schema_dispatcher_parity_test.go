// d3_memory_schema_dispatcher_parity_test.go — schema/dispatcher parity
// pin for mpm_memory (D3 audit, 2026-09-04).
//
// Audit claim: "MCP memory read/show ergonomics" — agents had no clean
// way to retrieve a specific memory by id. The dispatcher ALREADY
// supports mpm_memory show (W-003, handleShowMemory at handlers.go:641)
// and mpm_memory restore_challenge (F7-1, handleRestoreChallengeMemory
// at handlers.go:790); live-tested today via:
//
//   $ mpm call mpm_memory show --payload '{"action":"show","params":{"id":"<id>"}}'
//
// So the ergonomic defect is closed at the dispatch layer.
//
// The remaining drift is the JSON-Schema enum at registry_list.go:33,
// which lists 13 actions but the dispatcher accepts 15. Strict MCP
// hosts that validate against the schema enum would reject
// `action="show"` and `action="restore_challenge"` as "not in enum",
// even though the dispatcher routes them correctly. This pins the
// discovery surface so the drift cannot reappear silently.
//
// If a future change adds another dispatcher action to mpm_memory,
// this test fails until BOTH the dispatcher and the schema enum are
// updated together — same lock-in pattern as
// handoff_schema_handler_parity_test.go for mpm_handoff.
//
// Direction of check: dispatcher (source of truth) → schema enum
// (advertised). The reverse direction (adapter call sites → schema
// enum) is covered by adapter_schema_guard_test.go; we don't FTS-query
// our own schema here.
package tools

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// mpmMemoryDispatcherActions is the canonical list of actions accepted
// by `handleMpmMemory` (handlers.go:4660-4703). Each entry MUST also
// appear in the schema enum at registry_list.go:33. Update BOTH
// together when adding a new action; this test will catch a one-sided
// change.
var mpmMemoryDispatcherActions = []string{
	"save",
	"query",
	"show", // W-003: handleShowMemory
	"shred",
	"reinforce",
	"weaken",
	"snooze",
	"set_weight",
	"patch",
	"promote",
	"review",
	"synthesize",
	"challenge",
	"restore_challenge", // F7-1: handleRestoreChallengeMemory
	"commit_milestone",
}

// schemaEnum parses the canonical Registry entry for a named tool and
// returns its action enum slice. Returns nil if the tool is missing
// or its action field has no enum.
func schemaEnum(t *testing.T, toolName string) []string {
	t.Helper()
	tool, ok := ByName(toolName)
	if !ok {
		t.Fatalf("tool %q not in Registry", toolName)
	}
	var schema struct {
		Properties struct {
			Action struct {
				Enum []string `json:"enum"`
			} `json:"action"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("%s schema not valid JSON: %v", toolName, err)
	}
	return schema.Properties.Action.Enum
}

// TestD3_MpmMemorySchemaAdvertisesAllDispatcherActions pins that the
// canonical mpm_memory schema enum at registry_list.go:33 includes
// every action the dispatcher accepts in handleMpmMemory. If a future
// edit adds a dispatcher case without updating the schema enum, this
// fails first — preventing the discovery-silent drift the audit
// surfaced in 2026-09-04.
//
// Live verification: the audit-level "agents have no clean way to
// retrieve a specific memory" claim is closed because the dispatcher
// routes `action="show"`; this test pins that the schema also
// ADVERTISES the action so MCP discovery / strict validation / CLI
// help stay in sync with runtime capability.
func TestD3_MpmMemorySchemaAdvertisesAllDispatcherActions(t *testing.T) {
	enum := schemaEnum(t, "mpm_memory")
	if enum == nil {
		t.Fatal("mpm_memory schema has no action enum — fix the schema, not this test")
	}

	enumSet := map[string]bool{}
	for _, e := range enum {
		enumSet[e] = true
	}

	var missing []string
	for _, want := range mpmMemoryDispatcherActions {
		if !enumSet[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		t.Errorf("D3 schema/dispatcher parity drift: mpm_memory schema enum at "+
			"registry_list.go:33 is missing dispatcher-accepted actions %v. "+
			"Either remove the action from handleMpmMemory (handlers.go:4660) or "+
			"add the action to the schema enum. Silent drift here = "+
			"strict-MCP-host validation rejects actions that the dispatcher "+
			"actually supports. Audit surfaced this on 2026-09-04.",
			missing)
	}
}

// TestD3_MpmMemoryDispatcherRejectsUnknownActionWithCanonicalList is the
// inverse lock: when the dispatcher is given a bogus action, the
// resulting error message must list the SAME actions the schema enum
// advertises. If a future edit updates only one side, this fails along
// with TestD3_MpmMemorySchemaAdvertisesAllDispatcherActions — together
// they form a bidirectional pin.
//
// The dispatcher body's `default` case at handlers.go:4702 names the
// canonical valid-actions list verbatim. We pin that list length and
// contents match the schema enum exactly, modulo whitespace.
func TestD3_MpmMemoryDispatcherRejectsUnknownActionWithCanonicalList(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	_, err := handleMpmMemory(dm, ac, map[string]interface{}{
		"action": "bogus-d3-probe-action",
	})
	if err == nil {
		t.Fatal("handleMpmMemory with bogus action returned nil error")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "unknown action") {
		t.Fatalf("dispatcher error must start with 'unknown action'; got: %q", msg)
	}

	// Extract the post-clause actions list and compare to the schema enum.
	// The dispatcher formats: "unknown action %q for mpm_memory. Valid actions include A, B, C".
	idx := strings.Index(msg, "Valid actions include")
	if idx < 0 {
		t.Fatalf("dispatcher error must contain 'Valid actions include'; got: %q", msg)
	}
	tail := msg[idx+len("Valid actions include"):]
	parts := strings.Split(tail, ",")
	listedInError := map[string]bool{}
	for _, p := range parts {
		cleaned := strings.TrimSpace(p)
		if cleaned == "" {
			continue
		}
		listedInError[cleaned] = true
	}

	// Every action the dispatcher advertises in its error message must
	// also be in the schema enum. Together with
	// TestD3_MpmMemorySchemaAdvertisesAllDispatcherActions (which asserts
	// the inverse: every dispatcher case is in the enum), the surface
	// is locked both ways.
	enum := schemaEnum(t, "mpm_memory")
	enumSet := map[string]bool{}
	for _, e := range enum {
		enumSet[e] = true
	}
	var mismatched []string
	for a := range listedInError {
		if !enumSet[a] {
			mismatched = append(mismatched, a)
		}
	}
	if len(mismatched) > 0 {
		t.Errorf("D3 parity drift: mpm_memory dispatcher error names actions %v that are "+
			"missing from the schema enum (registry_list.go:33). Update the schema to match "+
			"the dispatcher's canonical valid-actions list.",
			mismatched)
	}
}
