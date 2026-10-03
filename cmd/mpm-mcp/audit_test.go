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
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// newIsolatedTestDM returns a DatabaseManager rooted in a temp workspace
// so tests don't bleed into the user's real ~/.mpm. We can't import
// internal/testutil from a separate module — cmd/mpm's tests run inside
// the main module and inherit it; cmd/mpm-mcp runs as its own binary.
//
// Two different roots have to be pinned, and isolating only the first is
// a silent leak. `workspace` above roots the DATABASE — and with it
// active.json, since a DatabaseManager resolves its own lifecycle
// identity beside its own database. MPM_WORKSPACE roots everything that
// still resolves through config.GetMPMDir(), most visibly
// toxicphrases.txt, which any memory save's poison scanner will
// generate. An already-set MPM_WORKSPACE is left alone.
func newIsolatedTestDM(t *testing.T) *core.DatabaseManager {
	t.Helper()
	workspace := t.TempDir()
	if os.Getenv("MPM_WORKSPACE") == "" {
		t.Setenv("MPM_WORKSPACE", workspace)
	}
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

// TestRecordToolInvocation_MCPFourColumnsAndAbsentSession pins the
// OBSERVABILITY FOUNDATION MCP-side contract: every tool_invocations
// row written by recordToolInvocation carries the four identity
// columns (invocation_id, mpm_session_id, framework_name,
// framework_session_id) populated from the ActiveContext, OR NULL
// when the caller did not supply a value (absent-native-session
// case — e.g. Pi/Hermes without hooks). Empty strings are NEVER
// synthesised across columns; the persisted NULL is the canonical
// "absent" state.
func TestRecordToolInvocation_MCPFourColumnsAndAbsentSession(t *testing.T) {
	dm := newIsolatedTestDM(t)

	// Case A: full ActiveContext (framework supplies native session).
	started := time.Now().Add(-20 * time.Millisecond)
	completed := time.Now()
	const (
		wantInvocation = "inv-mcp-fixture"
		wantMPMSession = "mpm-mcp-fixture"
		wantFwName     = "opencode-mcp-fixture"
		wantFwSession  = "opencode-mcp-sess-fixture"
	)
	acFull := core.ActiveContext{
		SessionID:          "mcp-full",
		InvocationID:       wantInvocation,
		MPMSessionID:       wantMPMSession,
		FrameworkName:      wantFwName,
		FrameworkSessionID: wantFwSession,
	}
	recordToolInvocation(dm, acFull,
		"mpm_system", map[string]interface{}{"action": "health_check"},
		started, completed, "health_check", "success", nil)

	row := dm.SQLDB().QueryRow(`
		SELECT invocation_id, mpm_session_id, framework_name, framework_session_id, framework_session_id IS NULL
		FROM tool_invocations
		WHERE invocation_id = ?
		ORDER BY rowid DESC LIMIT 1`, wantInvocation)
	var gotInv, gotMPMS, gotFw, gotFwS string
	var gotFwSIsNull int
	if err := row.Scan(&gotInv, &gotMPMS, &gotFw, &gotFwS, &gotFwSIsNull); err != nil {
		t.Fatalf("scan full AC: %v", err)
	}
	if gotInv != wantInvocation {
		t.Errorf("invocation_id = %q, want %q", gotInv, wantInvocation)
	}
	if gotMPMS != wantMPMSession {
		t.Errorf("mpm_session_id = %q, want %q", gotMPMS, wantMPMSession)
	}
	if gotFw != wantFwName {
		t.Errorf("framework_name = %q, want %q", gotFw, wantFwName)
	}
	if gotFwS != wantFwSession {
		t.Errorf("framework_session_id = %q, want %q", gotFwS, wantFwSession)
	}
	if gotFwSIsNull != 0 {
		t.Errorf("framework_session_id IS NULL = %d, want 0", gotFwSIsNull)
	}

	// Case B: absent-native-session — ActiveContext has empty
	// FrameworkSessionID. Persisted column must be SQL NULL, NOT
	// the empty string (empty-string == NULL invariant).
	acAbsent := core.ActiveContext{
		SessionID:     "mcp-absent",
		InvocationID:  "inv-mcp-absent",
		MPMSessionID:  "mpm-mcp-absent",
		FrameworkName: "pi-no-hooks",
		// FrameworkSessionID intentionally empty.
	}
	recordToolInvocation(dm, acAbsent,
		"mpm_memory", map[string]interface{}{"action": "show"},
		started, completed, "show", "success", nil)

	row = dm.SQLDB().QueryRow(`
		SELECT framework_session_id, framework_session_id IS NULL
		FROM tool_invocations
		WHERE invocation_id = ?`, "inv-mcp-absent")
	var gotFwS2 sql.NullString
	var gotFwS2IsNull int
	if err := row.Scan(&gotFwS2, &gotFwS2IsNull); err != nil {
		t.Fatalf("scan absent AC: %v", err)
	}
	if gotFwS2IsNull != 1 {
		t.Errorf("framework_session_id IS NULL = %d, want 1 (absent-native-session must be NULL, not empty)", gotFwS2IsNull)
	}
	if gotFwS2.Valid {
		t.Errorf("framework_session_id = %q, want NULL (absent-native-session case)", gotFwS2.String)
	}
}

// errorString is a tiny error helper so we don't pull in errors/fmt.
type errorString string

func (e errorString) Error() string { return string(e) }

// _ = filepath keeps the import alive on platforms where t.TempDir paths
// don't surface the workspace layout we want.
var _ = filepath.Join
