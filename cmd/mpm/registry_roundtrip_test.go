package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
// migrations, etc.) by using `mpm_context` with `read_wake_context` and
// `read_directives` actions — both are read-only and produce deterministic
// output given an empty DB. For wider coverage we add a "happy path" test
// below that runs every tool with a minimal payload and asserts no panics.
func TestRegistry_RoundTripCLIAndMCP_ReadOnlyTools(t *testing.T) {
	dm := newTestDMForCmd(t)

	// Use domain tools with read-only actions for the roundtrip test.
	type readOnlyCase struct {
		tool   string
		action string
		params map[string]interface{}
	}
	readOnlyTools := []readOnlyCase{
		{"mpm_context", "read_wake_context", map[string]interface{}{}},
		{"mpm_context", "read_directives", map[string]interface{}{}},
	}

	for _, tc := range readOnlyTools {
		t.Run(tc.tool+"_"+tc.action, func(t *testing.T) {
			tool, ok := tools.ByName(tc.tool)
			if !ok {
				t.Fatalf("tool %q not in registry", tc.tool)
			}

			payload := map[string]interface{}{
				"action": tc.action,
				"params": tc.params,
			}

			// CLI path: invoke handler directly with payload.
			cliResult, err := tool.Handler(dm, internal.ActiveContext{}, payload)
			if err != nil {
				t.Fatalf("CLI path failed: %v", err)
			}

			// MCP path: invoke through the same mcpAdapter the MCP server uses.
			adapter := mcpAdapterForTest(dm, tool.Handler)
			req := mcp.CallToolRequest{}
			req.Params.Name = tool.Name
			req.Params.Arguments = payload
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
			// order) and string-comparing.
			//
			// Volatile fields are stripped: audit_summary is a CUMULATIVE
			// count of system_audit_log entries, and since both calls
			// (CLI then MCP) write to that log on read_wake_context, the
			// second call always shows a higher count than the first.
			// That's a known property of the audit log, not a CLI/MCP
			// drift — strip it before comparing.
			cliJSON = stripVolatile(cliJSON)
			mcpJSON = stripVolatile(mcpJSON)
			if string(mcpJSON) != string(cliJSON) {
				t.Errorf("CLI/MCP output mismatch for %s_%s:\n  CLI: %s\n  MCP: %s",
					tc.tool, tc.action, cliJSON, mcpJSON)
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
// on its JSON schema. After the 2026-08-13 dispatcher hardening, the
// domain-tool contract is {action: <op>, params: {<op-specific>}}. The
// schema declares `action` as required (an enum of valid op strings)
// and `params` as a free-form object — so the minimal shape we build
// here is {action: <first enum value>, params: {}}. Inner handlers that
// need specific fields in params.<key> will return "X is required" and
// the test tolerates that as long as it doesn't panic, identical to
// the pre-fix shape.
//
// Tools that have NO schema (a few standalone tools like
// `explain_retrieval`) get an empty top-level payload. Their handlers
// are written defensively and tolerate missing-field errors at every
// level; that continues to work post-hardening because those handlers
// don't route through extractParamsOrFail.
func minimalPayload(schemaRaw json.RawMessage) map[string]interface{} {
	var schema struct {
		Properties map[string]map[string]interface{} `json:"properties"`
		Required   []string                          `json:"required"`
	}
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		return map[string]interface{}{}
	}

	// Resolve the action enum (if present) so the wrapping is valid
	// for domain tools. Default to "x" for tools without an enum.
	actionValue := "x"
	if actionProp, ok := schema.Properties["action"]; ok {
		if enum, ok := actionProp["enum"].([]interface{}); ok && len(enum) > 0 {
			if s, ok := enum[0].(string); ok {
				actionValue = s
			}
		}
	}

	// Only wrap if `action` is required — tools without `action` in
	// their required list (e.g. explain_retrieval) keep their flat shape.
	requiresAction := false
	for _, k := range schema.Required {
		if k == "action" {
			requiresAction = true
			break
		}
	}
	if !requiresAction {
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

	return map[string]interface{}{
		"action": actionValue,
		"params": map[string]interface{}{},
	}
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
//   - as_of / generated_at (wall-clock read per call, see below)
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
	// audit_summary is cumulative across calls (each call may write to
	// system_audit_log, so successive calls in the same test see a
	// higher count). Strip it so CLI/MCP comparisons are deterministic.
	delete(obj, "audit_summary")
	// as_of and generated_at are wall-clock unix seconds read at
	// assemble time (internal/core/wake_context.go, gatherWakeContext).
	// The CLI and MCP paths are invoked back-to-back, so the two
	// payloads agree EXCEPT when the pair straddles a whole-second
	// boundary — a genuine ~0.1% flake that made
	// TestRegistry_AllToolsExecuteWithoutPanic/mpm_context fail
	// intermittently with these two fields as the only difference.
	//
	// They belong in this function by its own stated contract ("anything
	// that writes a last_ran timestamp"); they were simply never added.
	// A CLI/MCP drift in how a wall-clock instant is read is not surface
	// drift, and stripping them does not weaken the parity claim: every
	// other field, nested structures included, is still compared.
	delete(obj, "as_of")
	delete(obj, "generated_at")
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

// TestRegistry_StripVolatileRemovesWallClockFields pins the volatile-field
// contract that the CLI/MCP parity comparison depends on.
//
// A payload field that is re-read from the wall clock on every call cannot
// be equal across two invocations that straddle a whole second. as_of and
// generated_at are exactly that (internal/core/wake_context.go,
// gatherWakeContext: `nowUnix := time.Now().Unix()`). Leaving them in the
// comparison made TestRegistry_AllToolsExecuteWithoutPanic/mpm_context fail
// intermittently on a ~0.1% boundary alignment, with those two fields as the
// ONLY difference between an otherwise-identical pair of payloads.
//
// This asserts the strip set directly, so the regression does not depend on
// winning a race to observe.
func TestRegistry_StripVolatileRemovesWallClockFields(t *testing.T) {
	payload := []byte(`{
		"as_of": 1790933126,
		"generated_at": 1790933126,
		"last_gc_ran": 1790933120,
		"ran": true,
		"cooldown_skip": true,
		"audit_summary": "12",
		"context_version": "wake-context-v5",
		"epistemic_pressure": {"ratio": 0, "threshold": 100}
	}`)

	got := string(stripVolatile(payload))

	for _, gone := range []string{"as_of", "generated_at", "last_gc_ran", "ran", "cooldown_skip", "audit_summary"} {
		if strings.Contains(got, `"`+gone+`"`) {
			t.Errorf("stripVolatile left volatile key %q in output: %s", gone, got)
		}
	}

	// Everything that is NOT volatile must survive, including nested
	// structure — stripping must not hollow out the comparison.
	for _, kept := range []string{"context_version", "wake-context-v5", "epistemic_pressure", "threshold"} {
		if !strings.Contains(got, kept) {
			t.Errorf("stripVolatile removed non-volatile content %q: %s", kept, got)
		}
	}
}

// TestRegistry_MCPParityHoldsAcrossSecondBoundary drives the real
// CLI/MCP comparison used by TestRegistry_AllToolsExecuteWithoutPanic across
// a forced whole-second boundary.
//
// The sleep is semantically required, not a timing crutch: the defect under
// test IS a wall-clock boundary, so a test that never crosses a boundary
// cannot observe it. The wait is to the next wall-clock second plus 20ms, so
// it costs at most ~1s and deterministically puts the two invocations in
// different seconds. Under the old stripVolatile this test failed every time;
// it now passes because the wall-clock fields are excluded from the
// comparison, which is the invariant the parity claim actually rests on.
func TestRegistry_MCPParityHoldsAcrossSecondBoundary(t *testing.T) {
	tool, ok := tools.ByName("mpm_context")
	if !ok {
		t.Fatalf("mpm_context not in registry")
	}
	dm := newTestDMForCmd(t)
	payload := minimalPayload(tool.Schema)

	adapter := mcpAdapterForTest(dm, tool.Handler)
	req := mcp.CallToolRequest{}
	req.Params.Name = tool.Name
	req.Params.Arguments = payload

	// CLI path.
	cliResult, cliErr := tool.Handler(dm, internal.ActiveContext{}, payload)
	if cliErr != nil {
		t.Fatalf("CLI path failed: %v", cliErr)
	}

	// Force the two invocations into different wall-clock seconds.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 20*time.Millisecond)))

	// MCP path.
	mcpResult, err := adapter(context.Background(), req)
	if err != nil {
		t.Fatalf("MCP path failed: %v", err)
	}

	cliJSON := string(stripVolatile(mustMarshal(cliResult)))
	mcpJSON := string(stripVolatile(extractMCPTextPayload(t, mcpResult)))
	if cliJSON != mcpJSON {
		t.Errorf("CLI/MCP drift across a second boundary:\n  CLI: %s\n  MCP: %s", cliJSON, mcpJSON)
	}
}

// TestRegistry_ParityComparisonCatchesSemanticDrift is the negative control
// for the wall-clock strip.
//
// TestRegistry_StripVolatileRemovesWallClockFields proves the helper does not
// delete the semantic keys. This proves the STRONGER property: the comparison
// the two parity tests actually perform still rejects a payload pair that
// differs in any of them. Without this, an over-broad strip — one that hollowed
// the payload out — would leave both existing tests green while the parity
// claim they exist to protect silently evaporated.
//
// The two subtests are deliberate opposites: drift confined to the wall-clock
// fields is ignored (that is the fix), drift in any ordinary semantic field is
// still caught (that is the guarantee).
func TestRegistry_ParityComparisonCatchesSemanticDrift(t *testing.T) {
	const base = `{
		"as_of": 1790933126,
		"generated_at": 1790933126,
		"context_version": "wake-context-v5",
		"epistemic_pressure": {"ratio": 0.62, "threshold": 100},
		"recent_activity": [{"kind": "session", "id": "s-1"}],
		"handoff": {"summary": "prior state", "session_id": "s-0"},
		"open_work": [{"id": "w-1", "state": "open"}],
		"directives": []
	}`

	// Control case: differing ONLY in the wall-clock fields must compare
	// equal after the strip. This is the behaviour the fix introduces.
	t.Run("wall_clock_only_drift_is_ignored", func(t *testing.T) {
		mutated := strings.Replace(base, "1790933126", "1790933127", 2)
		if mutated == base {
			t.Fatal("mutation did not apply — test is vacuous")
		}
		if got, want := string(stripVolatile([]byte(mutated))), string(stripVolatile([]byte(base))); got != want {
			t.Errorf("wall-clock-only drift was not ignored:\n got: %s\nwant: %s", got, want)
		}
	})

	// Each of these is an ordinary semantic field. The strip must leave
	// every one of them compared, so the parity check must still fail.
	for _, tc := range []struct{ name, from, to string }{
		{"context_version", `"wake-context-v5"`, `"wake-context-v6"`},
		{"epistemic_pressure.ratio", `"ratio": 0.62`, `"ratio": 0.63`},
		{"recent_activity[0].id", `"id": "s-1"`, `"id": "s-2"`},
		{"handoff.summary", `"prior state"`, `"different state"`},
		{"open_work[0].state", `"state": "open"`, `"state": "blocked"`},
		{"directives", `"directives": []`, `"directives": ["d-1"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.Replace(base, tc.from, tc.to, 1)
			if mutated == base {
				t.Fatalf("mutation %q did not apply — test is vacuous", tc.name)
			}
			if string(stripVolatile([]byte(mutated))) == string(stripVolatile([]byte(base))) {
				t.Errorf("semantic drift in %s survived the parity comparison — stripVolatile is too broad", tc.name)
			}
		})
	}
}
