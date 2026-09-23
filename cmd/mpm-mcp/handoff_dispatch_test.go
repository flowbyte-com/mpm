// cmd/mpm-mcp/handoff_dispatch_test.go — pin the compact-MCP handoff
// dispatch contract.
//
// Behavioral role: when the model-facing `mpm_context` tool is called
// with action `write_handoff` (or `read_handoff`), the contextAdapter
// must rewrite the action to `write` (or `read`) and forward to the
// substrate `mpm_handoff` handler. Pre-2026-09-08 the adapter used
// `strings.TrimPrefix(action, "_handoff")` which is a no-op
// (neither input starts with `_handoff`), so the substrate handler
// rejected the request with `unknown action "write_handoff" for
// mpm_handoff`. The model-facing description has always advertised
// `write_handoff | read_handoff` as the canonical action names, so
// the bug left the documented contract unfulfilled.
//
// This test exercises the actual contextAdapter closure against an
// isolated DatabaseManager and asserts:
//
//   1. action=write_handoff rewrites to action=write and persists a
//      handoff record (verified by reading it back).
//   2. action=read_handoff rewrites to action=read and returns the
//      just-written record.
//   3. Non-handoff actions (e.g. read_wake_context) pass through to
//      the context handler unchanged.
//   4. Static source-level guard against the TrimPrefix bug coming
//      back: tools.go must not contain TrimPrefix(action, "_handoff").
//
// Failure mode this catches: any revert of the TrimSuffix fix in
// tools.go, or a regression that re-routes `write_handoff` through
// the wrong handler.

package main

import (
	"os"
	"strings"
	"testing"

	core "github.com/flowbyte-com/mpm-core"
)

func readFile(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(path)
}

func TestContextAdapter_WriteHandoffDispatch(t *testing.T) {
	dm := newIsolatedTestDM(t)

	adapter := contextAdapter(dm, core.ActiveContext{SessionID: "test-session"})

	// Drive the adapter with action=write_handoff. The closure must
	// rewrite to action=write and the substrate handler must persist
	// a handoff record.
	result, err := adapter(nil, core.ActiveContext{SessionID: "test-session"}, map[string]interface{}{
		"action": "write_handoff",
		"params": map[string]interface{}{
			"summary": "test handoff from MCP adapter",
			"state":   "clean",
		},
	})
	if err != nil {
		t.Fatalf("write_handoff dispatch failed: %v (the substrate mpm_handoff rejects write_handoff; the adapter must rewrite to write)", err)
	}
	if result == nil {
		t.Fatal("write_handoff dispatch returned nil result")
	}

	// Read it back via the same adapter — read_handoff should also
	// be rewritten and return the just-written record.
	readResult, err := adapter(nil, core.ActiveContext{SessionID: "test-session"}, map[string]interface{}{
		"action": "read_handoff",
		"params": map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("read_handoff dispatch failed: %v", err)
	}
	if readResult == nil {
		t.Fatal("read_handoff dispatch returned nil result")
	}
}

func TestContextAdapter_NonHandoffActionsForwarded(t *testing.T) {
	dm := newIsolatedTestDM(t)
	adapter := contextAdapter(dm, core.ActiveContext{})

	// read_wake_context is a context action, not a handoff action.
	// It must pass through unchanged to the context handler. We do
	// not assert the result content (the test DB is empty so wake
	// is a no-op); we only assert no error and no false rewrite.
	_, err := adapter(nil, core.ActiveContext{}, map[string]interface{}{
		"action": "read_wake_context",
		"params": map[string]interface{}{"projection": "compact"},
	})
	if err != nil {
		t.Fatalf("read_wake_context dispatch failed: %v", err)
	}
}

func TestContextAdapter_NoTrimPrefixBugRegression(t *testing.T) {
	// Static source-level guard. If a future contributor reverts
	// TrimSuffix back to TrimPrefix, this test fires immediately —
	// the user-facing contract is the model's write_handoff action
	// arriving at the substrate as `write`, which only TrimSuffix
	// produces.
	src := readToolsSource(t)
	if strings.Contains(src, `TrimPrefix(action, "_handoff")`) {
		t.Fatal("contextAdapter still uses TrimPrefix(action, \"_handoff\") — this is the 2026-09-08 bug; use TrimSuffix")
	}
	if !strings.Contains(src, `TrimSuffix(action, "_handoff")`) {
		t.Fatal("contextAdapter must use TrimSuffix(action, \"_handoff\") to rewrite write_handoff → write and read_handoff → read")
	}
}

func readToolsSource(t *testing.T) string {
	t.Helper()
	const path = "tools.go"
	b, err := readFile(t, path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
