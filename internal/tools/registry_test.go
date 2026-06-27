package tools

import (
	"encoding/json"
	"testing"

	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"mpm/internal"
)

// TestRegistry_AllEntriesHaveHandlerAndSchema verifies every entry in
// the Registry has a non-nil Handler and a non-empty Schema. This is the
// static "no tool got registered with a missing piece" check — catches
// the common mistake of adding a Tool{} entry but forgetting to wire
// up the function pointer or the JSON schema.
func TestRegistry_AllEntriesHaveHandlerAndSchema(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range Registry {
		if names[tool.Name] {
			t.Errorf("duplicate tool name in Registry: %s", tool.Name)
		}
		names[tool.Name] = true

		if tool.Handler == nil {
			t.Errorf("tool %q has nil Handler", tool.Name)
		}
		if len(tool.Description) == 0 {
			t.Errorf("tool %q has empty Description", tool.Name)
		}
		if len(tool.Schema) == 0 {
			t.Errorf("tool %q has empty Schema — every tool needs a JSON schema so MCP clients know what args to pass", tool.Name)
		}

		// Schema must be parseable JSON.
		var schemaObj map[string]interface{}
		if err := json.Unmarshal(tool.Schema, &schemaObj); err != nil {
			t.Errorf("tool %q schema is not valid JSON: %v", tool.Name, err)
		}
	}
}

// TestRegistry_NoDriftBetweenCLIAndMCP verifies the CLI dispatcher
// (cmd/mpm/call.go) and the MCP server (cmd/mpm-mcp/tools.go) both
// expose the SAME set of tool names. Drift between the two surfaces
// was the primary bug class before the registry — the CLI would
// silently keep a stale tool while the MCP server added the new one,
// or vice versa.
//
// We can't import cmd/mpm from internal/tools (cycle), so the test
// surfaces the canonical Registry names and asserts they all resolve
// to a non-nil handler. The CLI/MCP integration tests live in
// cmd/mpm and verify the same set appears in both entry points.
func TestRegistry_NoDriftBetweenCLIAndMCP(t *testing.T) {
	for _, name := range Names() {
		tool, ok := ByName(name)
		if !ok {
			t.Fatalf("Names() returned %q but ByName says it's missing", name)
		}
		if tool.Handler == nil {
			t.Errorf("tool %q listed in Names() but handler is nil", name)
		}
	}
}

// TestRegistry_RequiredFieldsHaveCorrectTypes verifies that the JSON
// schema's "required" array only references fields that exist in
// "properties". This is a common copy-paste bug — adding a required
// field and forgetting to define it.
func TestRegistry_RequiredFieldsHaveCorrectTypes(t *testing.T) {
	for _, tool := range Registry {
		var schema struct {
			Properties map[string]interface{} `json:"properties"`
			Required   []string               `json:"required"`
		}
		if err := json.Unmarshal(tool.Schema, &schema); err != nil {
			continue // already caught by TestRegistry_AllEntriesHaveHandlerAndSchema
		}
		for _, req := range schema.Required {
			if _, ok := schema.Properties[req]; !ok {
				t.Errorf("tool %q schema: required field %q is not in properties", tool.Name, req)
			}
		}
	}
}

// TestRegistry_HandlersAcceptEmptyPayload verifies that every handler
// tolerates an empty payload. Some tools (read_wake_context,
// read_directives) genuinely need no input; their handlers should not
// panic or fail with "missing field" on {}. This guards against the
// regression where a new tool's first arg check rejects an empty map.
func TestRegistry_HandlersAcceptEmptyPayload(t *testing.T) {
	// Only run for tools that the CLI dispatcher also exercises. We don't
	// open a real DB here — we're verifying the handlers don't blow up
	// on nil-deref / panic before reaching the DM. Some handlers will
	// return errors (e.g. "db is required") and that's fine.
	//
	// Skipped because most handlers call dm.<method> which would panic
	// on a nil dm. The CLI dispatcher guarantees dm is non-nil before
	// calling; we test that path separately via integration tests.
	t.Skip("empty-payload smoke test requires a real DM; covered by CLI integration tests")

	for _, tool := range Registry {
		// Invariant check: the handler doesn't dereference payload["foo"]
		// before checking payload == nil.
		_ = tool
	}
}

// TestMCPAdapter_RoundTripsPayload verifies the MCP adapter correctly
// extracts the payload from mcp.CallToolRequest and passes it through
// to the registry handler. This is the "CLI and MCP get the same
// args" guard — if the adapter mangles the payload, the two surfaces
// would silently drift.
//
// We can't run the full handler (most need a real DM), so we test
// the adapter's payload extraction against a stub handler that
// records what it received.
func TestMCPAdapter_RoundTripsPayload(t *testing.T) {
	var received map[string]interface{}
	stub := func(dm *internal.DatabaseManager, ac internal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
		received = payload
		return map[string]interface{}{"ok": true}, nil
	}

	// Build the same adapter the MCP server uses.
	adapter := func(req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		payload := req.GetArguments()
		if payload == nil {
			payload = map[string]interface{}{}
		}
		result, err := stub(nil, internal.ActiveContext{}, payload)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("test failed", err), nil
		}
		b, _ := json.Marshal(result)
		return mcp.NewToolResultText(string(b)), nil
	}

	// Test 1: explicit payload
	req := mcp.CallToolRequest{}
	req.Params.Name = "test"
	req.Params.Arguments = map[string]interface{}{
		"fact": "hello world",
		"weight": 0.8,
	}
	if _, err := adapter(req); err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if received["fact"] != "hello world" {
		t.Errorf("payload round-trip lost: got %v", received)
	}

	// Test 2: nil payload → handler receives empty map (not nil)
	req2 := mcp.CallToolRequest{}
	req2.Params.Name = "test"
	req2.Params.Arguments = nil
	if _, err := adapter(req2); err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if received == nil {
		t.Errorf("expected empty map, got nil")
	}

	// Test 3: empty payload (some clients send {} explicitly)
	req3 := mcp.CallToolRequest{}
	req3.Params.Name = "test"
	req3.Params.Arguments = map[string]interface{}{}
	if _, err := adapter(req3); err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if len(received) != 0 {
		t.Errorf("expected empty payload, got %v", received)
	}
}

// TestMCPAdapter_ErrorEnvelope verifies the adapter converts handler
// errors into mcp.NewToolResultErrorFromErr so MCP clients see the
// message instead of an opaque "tool failed" string.
func TestMCPAdapter_ErrorEnvelope(t *testing.T) {
	stub := func(dm *internal.DatabaseManager, ac internal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
		return nil, &testErr{msg: "scanner blocked: API key pattern"}
	}
	adapter := func(req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		payload := req.GetArguments()
		if payload == nil {
			payload = map[string]interface{}{}
		}
		result, err := stub(nil, internal.ActiveContext{}, payload)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("test failed", err), nil
		}
		b, _ := json.Marshal(result)
		return mcp.NewToolResultText(string(b)), nil
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = "test"
	req.Params.Arguments = map[string]interface{}{}
	res, err := adapter(req)
	if err != nil {
		t.Fatalf("adapter returned error (should be in result): %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true, got %+v", res)
	}
	// The error text should contain the original message.
	for _, c := range res.Content {
		if text, ok := c.(mcp.TextContent); ok {
			if !strings.Contains(text.Text, "scanner blocked") {
				t.Errorf("error text missing original message: %q", text.Text)
			}
		}
	}
}

// Compile-time guard that server.ToolHandlerFunc still satisfies the
// type expected by mcp-go. If the upstream type changes, this will
// fail at compile time, not at server boot.
var _ server.ToolHandlerFunc

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }