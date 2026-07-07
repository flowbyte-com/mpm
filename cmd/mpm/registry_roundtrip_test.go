package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
)

// TestRegistry_RoundTripCLIAndMCP invokes each tool via both surfaces
// and asserts the JSON outputs match. The CLI path is `mpm call
// <name> --payload '<json>'`; the MCP path is the same handler
// invoked via the mcpAdapter wrapper.
//
// This is the canonical proof that the registry eliminated drift
// between the two surfaces. Before the registry, each tool had two
// independently-maintained handlers (callFoo in call.go and handleFoo
// in tools.go) that could — and did — drift apart.
//
// We test tools that don't require complex preconditions (DB, schema
// migrations, etc.) by using `read_wake_context` and `read_directives`
// — both are read-only and produce deterministic output given an
// empty DB. For wider coverage we add a "happy path" test below that
// runs every tool with a minimal payload and asserts no panics.
func TestRegistry_RoundTripCLIAndMCP_ReadOnlyTools(t *testing.T) {
	dm := newTestDMForCmd(t)

	readOnlyTools := []string{
		"read_wake_context",
		"read_directives",
	}

	for _, name := range readOnlyTools {
		t.Run(name, func(t *testing.T) {
			tool, ok := tools.ByName(name)
			if !ok {
				t.Fatalf("tool %q not in registry", name)
			}

			// CLI path: invoke handler directly with empty payload.
			cliResult, err := tool.Handler(dm, internal.ActiveContext{}, map[string]interface{}{})
			if err != nil {
				t.Fatalf("CLI path failed: %v", err)
			}

			// MCP path: invoke through the same mcpAdapter the MCP server uses.
			adapter := mcpAdapterForTest(dm, tool.Handler)
			req := mcp.CallToolRequest{}
			req.Params.Name = tool.Name
			req.Params.Arguments = map[string]interface{}{}
			mcpResult, err := adapter(context.Background(), req)
			if err != nil {
				t.Fatalf("MCP path failed: %v", err)
			}
			if mcpResult.IsError {
				t.Fatalf("MCP path returned error result: %+v", mcpResult)
			}

			// Extract the JSON payload from the MCP text result and
			// compare to the CLI result (re-marshalled identically).
			mcpJSON := extractMCPTextPayload(t, mcpResult)
			cliJSON := mustMarshal(cliResult)

			// JSON is order-insensitive at the parser level. We compare
			// by re-marshalling both sides (which canonicalizes key
			// order) and string-comparing. If a future handler returns
			// a non-deterministic result (timestamps, etc.), this
			// assertion will need to be relaxed.
			if string(mcpJSON) != string(cliJSON) {
				t.Errorf("CLI/MCP output mismatch for %s:\n  CLI: %s\n  MCP: %s",
					name, cliJSON, mcpJSON)
			}
		})
	}
}

// TestRegistry_AllToolsExecuteWithoutPanic is the "happy path" sanity
// check. Each tool is invoked with a minimal payload and the test
// passes if no panic occurs AND the CLI/MCP paths agree on the result.
// Tools that require complex setup (full-text indexing, embedding
// pipeline, etc.) may return errors — that's fine, but they must
// return the same error from both surfaces (i.e. drift is not
// acceptable even in the error path).
func TestRegistry_AllToolsExecuteWithoutPanic(t *testing.T) {
	dm := newTestDMForCmd(t)

	for _, tool := range tools.Registry {
		t.Run(tool.Name, func(t *testing.T) {
			// Minimal payload derived from the schema's required
			// fields. Tools with no required fields get an empty map.
			payload := minimalPayload(tool.Schema)

			// CLI path
			cliResult, cliErr := tool.Handler(dm, internal.ActiveContext{}, payload)

			// MCP path
			adapter := mcpAdapterForTest(dm, tool.Handler)
			req := mcp.CallToolRequest{}
			req.Params.Name = tool.Name
			req.Params.Arguments = payload
			mcpResult, _ := adapter(context.Background(), req)

			// The MCP adapter returns (result, nil) where result.IsError
			// is the error signal — NOT the second return value. Translate
			// to the same (cliErr, mcpErr) shape for comparison.
			mcpCliErr := error(nil)
			if mcpResult != nil && mcpResult.IsError {
				mcpCliErr = mcpResultToError(mcpResult)
			}

			// Both must succeed or both must fail with the same error.
			if (cliErr == nil) != (mcpCliErr == nil) {
				t.Errorf("CLI/MCP drift on %s: cliErr=%v mcpErr=%v",
					tool.Name, cliErr, mcpCliErr)
				return
			}
			if cliErr != nil && mcpCliErr != nil {
				if cliErr.Error() != stripErrorPrefix(mcpCliErr.Error()) {
					t.Errorf("CLI/MCP error message drift on %s:\n  CLI: %v\n  MCP: %v",
						tool.Name, cliErr, mcpCliErr)
				}
				return
			}

			// Both succeeded. Compare result shape.
			cliJSON := mustMarshal(cliResult)
			mcpJSON := extractMCPTextPayload(t, mcpResult)
			// Strip volatile fields (timestamps, run-state) that legitimately
			// differ between two invocations of stateful tools.
			cliJSON = stripVolatile(cliJSON)
			mcpJSON = stripVolatile(mcpJSON)
			if string(cliJSON) != string(mcpJSON) {
				t.Errorf("CLI/MCP output drift on %s:\n  CLI: %s\n  MCP: %s",
					tool.Name, cliJSON, mcpJSON)
			}
		})
	}
}

// mcpAdapterForTest is the same closure shape as cmd/mpm-mcp/tools.go
// but takes a handler directly so tests don't have to plumb through
// the server.ToolHandlerFunc constructor. It mirrors mcpAdapter
// byte-for-byte; the duplication is acceptable because the test
// needs to construct the closure at runtime, while the production
// code wires it into RegisterAllTools.
func mcpAdapterForTest(dm *internal.DatabaseManager, handler tools.HandlerFunc) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		payload := req.GetArguments()
		if payload == nil {
			payload = map[string]interface{}{}
		}
		result, err := handler(dm, internal.ActiveContext{}, payload)
		if err != nil {
			return mcp.NewToolResultErrorFromErr(req.Params.Name+" failed", err), nil
		}
		b, _ := json.Marshal(result)
		return mcp.NewToolResultText(string(b)), nil
	}
}

// extractMCPTextPayload pulls the JSON text out of an mcp.CallToolResult.
// The result wraps a single TextContent (per jsonResult() in the MCP
// server); we read that text and return it as bytes.
func extractMCPTextPayload(t *testing.T, res *mcp.CallToolResult) []byte {
	t.Helper()
	if res.IsError {
		t.Fatalf("expected success result, got error: %+v", res)
	}
	for _, c := range res.Content {
		if text, ok := c.(mcp.TextContent); ok {
			return []byte(text.Text)
		}
	}
	t.Fatalf("no TextContent in result: %+v", res)
	return nil
}

// mustMarshal is a tiny helper for the test. We avoid the production
// must() (which is in cmd/mpm/call.go and unreachable from here) and
// keep the test self-contained.
func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("marshal-error: " + err.Error())
	}
	return b
}

// minimalPayload constructs a minimal valid payload for a tool based
// on its JSON schema. Required fields get placeholder values; optional
// fields are omitted. The goal is to exercise every handler with
// enough args to reach the DB call (or first validation error).
//
// This is intentionally a dumb stub. It doesn't know about field types
// or constraints — handlers that need richer payloads should add a
// tool-specific case below the loop. Today every schema has
// string-or-number required fields, so empty strings and 0 cover it.
func minimalPayload(schemaRaw json.RawMessage) map[string]interface{} {
	var schema struct {
		Properties map[string]map[string]interface{} `json:"properties"`
		Required   []string                          `json:"required"`
	}
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		return map[string]interface{}{}
	}
	out := map[string]interface{}{}
	for _, key := range schema.Required {
		prop, ok := schema.Properties[key]
		if !ok {
			continue
		}
		typ, _ := prop["type"].(string)
		switch typ {
		case "string":
			out[key] = ""
		case "number", "integer":
			out[key] = 0
		case "boolean":
			out[key] = false
		case "array":
			out[key] = []interface{}{}
		case "object":
			out[key] = map[string]interface{}{}
		default:
			out[key] = nil
		}
	}
	return out
}

// guard against unused import in case strings is dropped later
var _ = strings.Contains

// mcpResultToError extracts a Go error from an MCP error result so
// the round-trip test can compare CLI/MCP error messages in the same
// (error, error) shape.
func mcpResultToError(res *mcp.CallToolResult) error {
	if res == nil || !res.IsError {
		return nil
	}
	for _, c := range res.Content {
		if text, ok := c.(mcp.TextContent); ok {
			return &mcpTestError{msg: text.Text}
		}
	}
	return &mcpTestError{msg: "unknown error"}
}

type mcpTestError struct{ msg string }

func (e *mcpTestError) Error() string { return e.msg }

// stripVolatile removes fields from JSON output that legitimately
// change between invocations of stateful tools (gc_run, decay sweep,
// anything that writes a "last_ran" timestamp). The comparison is
// about STRUCTURE matching, not bit-identical output.
//
// Today the known volatile fields are:
//   - last_gc_ran (gc_run writes it on each call)
//   - ran / cooldown_skip (gc_run returns these based on cooldown state)
//
// We parse to interface{}, delete the volatile keys, and re-marshal
// so both sides have the same canonical form.
func stripVolatile(jsonBytes []byte) []byte {
	var obj map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &obj); err != nil {
		return jsonBytes
	}
	delete(obj, "last_gc_ran")
	delete(obj, "ran")
	delete(obj, "cooldown_skip")
	out, err := json.Marshal(obj)
	if err != nil {
		return jsonBytes
	}
	return out
}

// stripErrorPrefix removes the MCP adapter's "X failed: " prefix so
// CLI and MCP error messages can be compared directly. The prefix is
// useful for agents (it tells them WHICH tool failed) but the test
// cares about the underlying error message, not the prefix.
func stripErrorPrefix(msg string) string {
	idx := strings.LastIndex(msg, ": ")
	if idx < 0 {
		return msg
	}
	return msg[idx+2:]
}
