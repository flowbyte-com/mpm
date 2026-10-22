// schema_scope_test.go — the schema-scope distinction between the
// REGISTERED schema and the native COMPACT MCP wrapper's schema.
//
// `mpm_help show <tool>` returns the tool's REGISTERED schema: the
// registry entry's own `Tool.Schema`, which is what the `mpm call`
// path and the runtime dispatcher validate a payload against. It is not
// the schema the native compact MCP wrapper for the same tool
// publishes.
//
// The distinction is not academic. The compact `mpm_context` wrapper
// accepts `write_handoff` and `read_handoff` — the session-continuity
// actions, which its adapter rewrites and routes to the `mpm_handoff`
// handler. The registered `mpm_context` entry's action enum does not
// contain them, and the registered handler rejects them.
//
// The risk these tests guard against is documentation drift in the
// opposite direction: describing the registered schema as "the schema
// the MCP surface publishes" would overstate the parity and would
// mislead an agent that consults discovery before choosing an action.
// The remedy is to state the distinction accurately — NOT to change a
// tool's schema, and not to build a second schema catalogue.
//
// **Brittleness notice.** The compact-wrapper assertions read what a
// client actually receives: the production registration is replayed
// against a fresh server and the published schema is read back through
// `s.ListTools()`, exactly as an MCP client would see it. The source
// may be reorganised in any way that preserves semantics.

package main

import (
	"encoding/json"
	"strings"
	"testing"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
)

// handoffActions are the two transport-specific actions the COMPACT
// mpm_context wrapper serves and the REGISTERED mpm_context entry does
// not.
var handoffActions = []string{"write_handoff", "read_handoff"}

// liveCompactMpmContextActionDescription returns the action property's
// description as the production MCP server publishes it.
//
// The production registration (registerCompactMpmContext) is replayed
// against a fresh server and the registered schema is read back — the
// same shape the compact-distinction regression test uses for the
// description.
func liveCompactMpmContextActionDescription(t *testing.T) string {
	t.Helper()

	s := buildMCPServerCore()
	registerCompactMpmContext(s, nil, core.ActiveContext{})

	for _, st := range s.ListTools() {
		if st.Tool.Name != "mpm_context" {
			continue
		}
		action, ok := st.Tool.InputSchema.Properties["action"].(map[string]any)
		if !ok {
			t.Fatalf("the published mpm_context schema has no action property: %#v",
				st.Tool.InputSchema.Properties)
		}
		desc, _ := action["description"].(string)
		return desc
	}

	t.Fatal("mpm_context was not registered on the server; the production registration may have been lost")
	return ""
}

// registeredActionEnum returns the action enum of a registry entry's
// own schema.
func registeredActionEnum(t *testing.T, toolName string) []string {
	t.Helper()

	entry, ok := tools.ByName(toolName)
	if !ok {
		t.Fatalf("%s is not registered", toolName)
	}
	var schema struct {
		Properties struct {
			Action struct {
				Enum []string `json:"enum"`
			} `json:"action"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(entry.Schema, &schema); err != nil {
		t.Fatalf("the registered schema for %s is not a JSON object: %v", toolName, err)
	}
	return schema.Properties.Action.Enum
}

// TestSchemaScope_TheCompactMpmContextPublishesTransportSpecificActions
// pins that the native compact wrapper really does publish the
// handoff actions.
//
// Without this, "the compact wrapper may expose transport-specific
// actions" would be an unverified claim in the documentation.
func TestSchemaScope_TheCompactMpmContextPublishesTransportSpecificActions(t *testing.T) {
	desc := liveCompactMpmContextActionDescription(t)

	for _, action := range handoffActions {
		if !strings.Contains(desc, action) {
			t.Errorf("the published compact mpm_context action vocabulary does not name %q:\n%s",
				action, desc)
		}
	}
}

// TestSchemaScope_TheRegisteredMpmContextDoesNotServeTheHandoffActions
// pins the other half of the distinction: the REGISTERED mpm_context
// entry neither advertises nor serves the handoff actions.
//
// This is the half that makes the documentation correction necessary.
// The registered handler rejects them, so an agent that read the
// registered schema and then called the registered tool is told the
// truth; an agent on the compact surface is served by a different
// adapter and a different schema.
func TestSchemaScope_TheRegisteredMpmContextDoesNotServeTheHandoffActions(t *testing.T) {
	enum := registeredActionEnum(t, "mpm_context")
	if len(enum) == 0 {
		t.Fatal("the registered mpm_context schema has no action enum")
	}

	for _, action := range handoffActions {
		for _, candidate := range enum {
			if candidate == action {
				t.Errorf("the registered mpm_context action enum contains %q; the registered handler does not serve it",
					action)
			}
		}
	}

	// The registered handler must reject them too. A schema that merely
	// omitted them would leave the behaviour unproven.
	entry, ok := tools.ByName("mpm_context")
	if !ok {
		t.Fatal("mpm_context is not registered")
	}
	for _, action := range handoffActions {
		_, err := entry.Handler(nil, core.ActiveContext{}, map[string]any{"action": action})
		if err == nil {
			t.Errorf("the registered mpm_context handler accepted action=%q; it must reject the transport-specific actions",
				action)
		}
	}
}

// TestSchemaScope_DiscoveryReturnsTheRegisteredSchema pins that
// `mpm_help show` reports the REGISTERED schema — and therefore the
// CLI / substrate vocabulary — rather than the compact wrapper's.
//
// This is the contract the documentation states. If discovery ever
// started reporting the compact schema, the CLI would be advertising an
// action vocabulary it does not enforce.
func TestSchemaScope_DiscoveryReturnsTheRegisteredSchema(t *testing.T) {
	res, err := tools.Help("show", "mpm_context")
	if err != nil {
		t.Fatalf("mpm_help show mpm_context: %v", err)
	}

	schema, ok := res["input_schema"].(map[string]interface{})
	if !ok {
		t.Fatalf("input_schema is %T, want a JSON object", res["input_schema"])
	}

	entry, ok := tools.ByName("mpm_context")
	if !ok {
		t.Fatal("mpm_context is not registered")
	}
	var want map[string]interface{}
	if err := json.Unmarshal(entry.Schema, &want); err != nil {
		t.Fatalf("the registered schema for mpm_context is not a JSON object: %v", err)
	}
	gotJSON, _ := json.Marshal(schema)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("discovery does not return the registered schema\n got: %s\nwant: %s", gotJSON, wantJSON)
	}

	// The registered schema's action enum must not carry the handoff
	// actions: discovery reports the CLI / substrate vocabulary, and the
	// CLI path rejects those actions.
	props, _ := schema["properties"].(map[string]interface{})
	action, _ := props["action"].(map[string]interface{})
	enum, _ := action["enum"].([]interface{})
	for _, candidate := range enum {
		for _, handoff := range handoffActions {
			if candidate == handoff {
				t.Errorf("the discovered schema's action enum contains %q; discovery reports the registered (CLI) vocabulary",
					handoff)
			}
		}
	}
}
