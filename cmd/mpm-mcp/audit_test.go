// audit_test.go — pin the MCP audit-hook contract.
//
// Behavioral role: every MCP tools/call routed through mcpAdapter must
// leave a row in tool_invocations with framework_name='mcp'. Without
// this, MCP-only frameworks (Hermes is the primary one) are invisible
// to the compatibility matrix.
//
// The test exercises recordToolInvocation directly rather than going
// through the full JSON-RPC subprocess — that's a separate integration
// test gated on `make build`. The contract (table shape, framework_name
// pinning) is what matters; the dispatch path that calls this helper is
// wired in tools.go:91.

package main

import (
	"path/filepath"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// newIsolatedTestDM returns a DatabaseManager rooted in a temp workspace
// so tests don't bleed into the user's real ~/.mpm. We can't import
// internal/testutil from a separate module — cmd/mpm's tests run inside
// the main module and inherit it; cmd/mpm-mcp runs as its own binary.
func newIsolatedTestDM(t *testing.T) *core.DatabaseManager {
	t.Helper()
	workspace := t.TempDir()
	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", workspace, err)
	}
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	// Sanity: the audit table must be present after schema init.
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='tool_invocations'`,
	).Scan(&n); err != nil {
		t.Fatalf("probe tool_invocations: %v", err)
	}
	if n != 1 {
		t.Fatalf("tool_invocations not in schema; check schema.go BaseTables")
	}
	t.Cleanup(func() { _ = dm.Close() })
	return dm
}

func TestRecordToolInvocation_WritesMCPRow(t *testing.T) {
	dm := newIsolatedTestDM(t)

	payload := map[string]interface{}{"action": "health_check"}
	started := time.Now()
	completed := started.Add(10 * time.Millisecond)

	recordToolInvocation(dm,
		core.ActiveContext{SessionID: "mcp-test-session"},
		"mpm_system", payload,
		started, completed, "health_check", "success", nil)

	var frameworkName, sessionID, toolName, resultStatus, actorKind string
	row := dm.SQLDB().QueryRow(`SELECT framework_name, session_id, tool_name, result_status, actor_kind FROM tool_invocations ORDER BY rowid DESC LIMIT 1`)
	if err := row.Scan(&frameworkName, &sessionID, &toolName, &resultStatus, &actorKind); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if frameworkName != "mcp" {
		t.Errorf("framework_name = %q, want mcp (drill matrix groups on this)", frameworkName)
	}
	if sessionID != "mcp-test-session" {
		t.Errorf("session_id = %q, want mcp-test-session", sessionID)
	}
	if toolName != "mpm_system" {
		t.Errorf("tool_name = %q, want mpm_system", toolName)
	}
	if resultStatus != "success" {
		t.Errorf("result_status = %q, want success", resultStatus)
	}
	if actorKind != "agent" {
		t.Errorf("actor_kind = %q, want agent", actorKind)
	}
}

func TestRecordToolInvocation_ErrorPath(t *testing.T) {
	dm := newIsolatedTestDM(t)

	started := time.Now()
	completed := started.Add(5 * time.Millisecond)
	boom := errorString("intentional audit row error")

	recordToolInvocation(dm,
		core.ActiveContext{SessionID: "mcp-err-session"},
		"mpm_context", map[string]interface{}{"action": "route"},
		started, completed, "route", "error", boom)

	var status, errMsg string
	row := dm.SQLDB().QueryRow(`SELECT result_status, error_message FROM tool_invocations WHERE session_id = 'mcp-err-session'`)
	if err := row.Scan(&status, &errMsg); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "error" {
		t.Errorf("result_status = %q, want error", status)
	}
	if errMsg != boom.Error() {
		t.Errorf("error_message = %q, want %q", errMsg, boom.Error())
	}
}

// Audit row should still land even when dm is nil (panic-isolation contract).
func TestRecordToolInvocation_NilDMSafe(t *testing.T) {
	recordToolInvocation(nil, core.ActiveContext{}, "x", nil, time.Now(), time.Now(),
		"a", "success", nil)
	// No assertion needed — passing means it didn't panic.
}

// errorString is a tiny error helper so we don't pull in errors/fmt.
type errorString string

func (e errorString) Error() string { return string(e) }

// _ = filepath keeps the import alive on platforms where t.TempDir paths
// don't surface the workspace layout we want.
var _ = filepath.Join
