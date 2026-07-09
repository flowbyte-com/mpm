// cmd/mpm-mcp — Native Go MCP server for MPM.
//
// Exposes MPM reasoning primitives to MCP clients (Claude Code, etc.)
// over stdio. Replaces the Python wrapper at .claude/mpm-mcp/server.py.
//
// Tools are no longer defined here — they live in internal/tools. This
// file is the thin MCP adapter: it iterates tools.Registry, builds the
// MCP Tool list (description + JSON schema), and dispatches incoming
// MCP requests to the unified handlers via a single adapter closure.
//
// Adding a new tool is now: (1) write handleFoo in internal/tools, (2)
// add a Tool entry to internal/tools/registry_list.go. The MCP server
// picks it up automatically — no edits here.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"time"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
)

const emptyWakeContext = "Wake context is empty. Ready for context."

// RegisterAllTools registers every entry in tools.Registry on the given
// MCP server. The "route" tool is special: its handler closes over the
// router, which is constructed at server boot. We build it last with
// the live router instance instead of using the registry stub.
//
// This replaces the previous 35-line s.AddTool(...) block plus 30
// handle*() adapter functions — both have been moved to the registry
// or the single mcpAdapter closure below.
func RegisterAllTools(s *server.MCPServer, dm *core.DatabaseManager, ac core.ActiveContext, router *core.Router) {
	for _, tool := range tools.Registry {
		if tool.Name == "route" {
			continue // registered below with the live router closure
		}
		s.AddTool(
			mcp.NewToolWithRawSchema(tool.Name, tool.Description, tool.Schema),
			mcpAdapter(dm, ac, tool.Handler),
		)
	}

	// route: needs the live *Router instance, not the registry stub.
	// The MCP server constructs the router once at boot from mode/*.md
	// and persona/*.md files; routing per-request is pure string
	// matching with zero parsing overhead.
	s.AddTool(
		mcp.NewTool("route",
			mcp.WithDescription(
				"Evaluate a user prompt and auto-select the best-matching MPM mode(s) "+
					"and persona. Modes use threshold filtering (multiple can activate); "+
					"personas use max-pooling (only the highest scorer wins, if any beats threshold 1). "+
					"Patterns are pre-compiled at server boot. Anti-patterns penalize false positives. "+
					"Returns a full diagnostic report with scores and triggers per component."),
			mcp.WithString("prompt",
				mcp.Required(),
				mcp.Description("The user prompt or message to route."),
			),
		),
		makeRouteHandler(router),
	)
}

// mcpAdapter wraps a registry HandlerFunc as an MCP server.ToolHandlerFunc.
//
// MCP requests arrive as mcp.CallToolRequest with arguments extracted via
// req.GetArguments() (a map[string]interface{}). The registry Handler
// already takes that shape directly — we just need to:
//   - convert errors to mcp.NewToolResultErrorFromErr
//   - convert the result to a JSON text result
//   - opportunistically fold any due scheduled_wakes into the response
//
// No arg-rewriting, no type assertions, no per-tool boilerplate. The
// 30+ previous handle*() functions collapsed to this single closure.
func mcpAdapter(dm *core.DatabaseManager, ac core.ActiveContext, handler tools.HandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		payload := req.GetArguments()
		if payload == nil {
			payload = map[string]interface{}{}
		}
		result, err := handler(dm, ac, payload)
		if err != nil {
			return mcp.NewToolResultErrorFromErr(req.Params.Name+" failed", err), nil
		}

		// Opportunistic wake fold (Phase 5a): check for due wakes and append
		// a visually distinct XML notification block. Uses the raw JSON text
		// for the primary content so any client that doesn't understand the
		// wake block still gets clean machine-readable output.
		var jsonText string
		if resultMap, ok := result.(map[string]interface{}); ok {
			b, jErr := json.Marshal(resultMap)
			if jErr != nil {
				jsonText = fmt.Sprintf("%q", fmt.Sprintf("%v", result))
			} else {
				jsonText = string(b)
			}
		} else {
			b, jErr := json.Marshal(result)
			if jErr != nil {
				jsonText = fmt.Sprintf("%q", fmt.Sprintf("%v", result))
			} else {
				jsonText = string(b)
			}
		}

		// Build the content array: [wake notification?, json result]
		//
		// Block order matters: the wake notification (if any) is prepended
		// as Block 1 so it is the first thing the LLM reads, never lost
		// to truncation or "lost in the middle" syndrome when a tool
		// returns a large payload. The JSON tool result is Block 2.
		content := []mcp.Content{
			mcp.TextContent{
				Type: mcp.ContentTypeText,
				Text: jsonText,
			},
		}

		if due, dErr := dm.CheckPendingWakes(time.Now()); dErr == nil && len(due) > 0 {
			notification := core.FormatWakeNotification(due)
			// Prepend the notification as Block 1 so it is the very
			// first content the LLM sees. Build a new slice rather than
			// inserting at index 0 to keep the code obvious.
			prepended := make([]mcp.Content, 0, len(content)+1)
			prepended = append(prepended, mcp.TextContent{
				Type: mcp.ContentTypeText,
				Text: notification,
			})
			prepended = append(prepended, content...)
			content = prepended
		}

		return &mcp.CallToolResult{Content: content}, nil
	}
}

// makeRouteHandler is the only per-tool MCP handler that survives —
// route closes over the *Router (constructed once at server boot),
// which is not in the registry because the registry has no router
// reference. Every other tool uses the generic mcpAdapter.
func makeRouteHandler(router *core.Router) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		prompt, _ := req.GetArguments()["prompt"].(string)
		if prompt == "" {
			return mcp.NewToolResultError("prompt is required"), nil
		}
		return jsonResult(router.Evaluate(prompt)), nil
	}
}

// jsonResult marshals v to JSON and wraps it in an mcp text result.
// Errors during marshalling fall back to a quoted string so the
// handler still returns a useful response.
func jsonResult(v interface{}) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("%q", fmt.Sprintf("%v", v)))
	}
	return mcp.NewToolResultText(string(b))
}

// ── Arg helpers ────────────────────────────────────────────────────────────

// parseNum extracts a numeric value from a JSON-decoded arg.
// Defaults to def when the value is missing or the wrong type.
// Accepts float64, int, int64 (covers every JSON number encoding
// the MCP server is likely to see).
func parseNum(v interface{}, def float64) float64 {
	if v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return def
}

// stringArg returns the string value of v (empty string if not a string).
func stringArg(v interface{}) string {
	s, _ := v.(string)
	return s
}

// defaultString returns s (if non-string) or defaultStr.
func defaultString(v interface{}, defaultStr string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return defaultStr
}

// parseStringSliceArg accepts a string (comma-separated) or []interface{}
// (each must be a string). Returns nil for any other shape or empty
// input. The opencode plugin emits arrays; many clients flatten to CSV
// when the tool schema is a string — accept both shapes.
func parseStringSliceArg(v interface{}) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		parts := strings.Split(t, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	}
	return nil
}