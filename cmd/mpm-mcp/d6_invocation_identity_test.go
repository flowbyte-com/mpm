// cmd/mpm-mcp/d6_invocation_identity_test.go — D6 regression coverage at
// the MCP TRANSPORT boundary.
//
// THE GAP D6 FOUND HERE: the mcpAdapter mints a per-call invocation id and
// hands it to the handler as the second argument, so the handler and the
// audit row share one correlation id. The contextAdapter — the closure
// behind the model-facing `mpm_context` tool — IGNORED that argument and
// used the captured process-level context instead.
//
// The captured context has no per-call invocation id. So every
// `mpm_context write_handoff` — a CONTINUITY-WRITING action, and one of the
// hunt's recording vocabulary — produced a handoff whose
// artifact_provenance row carried invocation_id NULL: the artifact existed
// and could not be joined back to the call that wrote it.
//
// These tests use disposable, file-backed databases and isolated workspace
// roots.

package main

import (
	"path/filepath"
	"testing"

	core "github.com/flowbyte-com/mpm-core"
)

// d6MCPTestDM opens a disposable, file-backed DatabaseManager inside an
// isolated workspace root.
func d6MCPTestDM(t *testing.T) *core.DatabaseManager {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("HOME", ws)
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", filepath.Join(ws, "src", "db", "mpm.db"))

	dm, err := core.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return dm
}

// TestD6_ContextAdapterForwardsThePerCallInvocation pins the transport
// fix: the adapter must forward the PER-CALL ActiveContext, so a handoff
// written through `mpm_context write_handoff` carries the invocation id
// of the call that wrote it.
func TestD6_ContextAdapterForwardsThePerCallInvocation(t *testing.T) {
	dm := d6MCPTestDM(t)

	// The captured (process-level) context — NO invocation id. This is
	// what the adapter used to forward.
	captured := core.ActiveContext{SessionID: "ses_d6"}
	adapter := contextAdapter(dm, captured)

	// The per-call context — minted by mcpAdapter for this dispatch.
	perCall := core.ActiveContext{
		SessionID:    "ses_d6",
		InvocationID: "inv_mcp_1",
	}

	result, err := adapter(nil, perCall, map[string]interface{}{
		"action": "write_handoff",
		"params": map[string]interface{}{
			"summary": "a handoff written through the MCP context adapter",
			"state":   "clean",
		},
	})
	if err != nil {
		t.Fatalf("write_handoff dispatch failed: %v", err)
	}
	if result == nil {
		t.Fatal("write_handoff dispatch returned nil result")
	}

	env, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected result shape: %T", result)
	}
	handoffID, _ := env["handoff_id"].(string)
	if handoffID == "" {
		t.Fatalf("no handoff id in result: %+v", env)
	}

	// The handoff's provenance row must carry the PER-CALL invocation id.
	var inv string
	if err := dm.SQLDB().QueryRow(`
		SELECT COALESCE(invocation_id, '')
		FROM artifact_provenance WHERE artifact_id = ? AND artifact_type = 'handoff'`,
		handoffID).Scan(&inv); err != nil {
		t.Fatalf("read handoff provenance: %v", err)
	}
	if inv != "inv_mcp_1" {
		t.Errorf("handoff provenance invocation_id = %q, want %q — the adapter "+
			"dropped the per-call invocation identity", inv, "inv_mcp_1")
	}
}

// TestD6_ContextAdapterStillWorksWithoutAPerCallContext is the
// compatibility guard: an adapter invoked without a per-call context must
// not regress.
func TestD6_ContextAdapterStillWorksWithoutAPerCallContext(t *testing.T) {
	dm := d6MCPTestDM(t)

	adapter := contextAdapter(dm, core.ActiveContext{SessionID: "ses_d6_none"})

	result, err := adapter(nil, core.ActiveContext{SessionID: "ses_d6_none"}, map[string]interface{}{
		"action": "write_handoff",
		"params": map[string]interface{}{
			"summary": "a handoff written without a per-call context",
			"state":   "clean",
		},
	})
	if err != nil {
		t.Fatalf("write_handoff dispatch failed: %v", err)
	}
	if result == nil {
		t.Fatal("write_handoff dispatch returned nil result")
	}
}
