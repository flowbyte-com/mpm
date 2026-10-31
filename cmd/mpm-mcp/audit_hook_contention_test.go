package main

// audit_hook_contention_test.go — issue #178, phase 2 (audit ledger,
// F6, MCP path).
//
// recordToolInvocation runs on every MCP tools/call dispatch. A busy
// failure used to lose the row with only a warning. The write stays
// best-effort (a warning, never a blocked call), but it now carries
// the statement-level retry budget. This pins that the row survives a
// contention window that outlasts the busy timeout.

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

func holdMCPWALWriteLock(t *testing.T, ws string) (release func()) {
	t.Helper()
	competitor, err := sql.Open("sqlite3", core.SqliteWriteDSN(ws+"/src/db/mpm.db"))
	if err != nil {
		t.Fatalf("competitor open: %v", err)
	}
	t.Cleanup(func() { competitor.Close() })
	tx, err := competitor.Begin()
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO tool_invocations (id, session_id, tool_name, action, invocation_id, actor_kind, framework_name, payload_hash, result_status, started_at, completed_at, duration_ms) VALUES ('holder', 's', 't', 'a', 'i', 'agent', 'f', 'h', 'success', 0, 0, 0)`); err != nil {
		t.Fatalf("holder insert: %v", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if err := tx.Commit(); err != nil {
				t.Errorf("holder commit: %v", err)
			}
		})
	}
}

func waitForMCPBusyRetry(t *testing.T, dm *core.DatabaseManager) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if dm.BusyRetryCount() > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the audit retry to engage")
}

// TestRepro_MCPRecordToolInvocationSurvivesContention pins the MCP
// dispatch audit row: it must land despite contention, because the
// drill scorer and recent-activity feed are built from this table.
func TestRepro_MCPRecordToolInvocationSurvivesContention(t *testing.T) {
	dm := newIsolatedTestDM(t)
	ws := dm.DBPath()
	ws = ws[:len(ws)-len("/src/db/mpm.db")]

	release := holdMCPWALWriteLock(t, ws)
	defer release()

	payload := map[string]interface{}{"action": "repro"}
	now := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		recordToolInvocation(dm,
			core.ActiveContext{SessionID: "repro-session"},
			"repro_tool", payload,
			now, now, "repro", "success", nil)
	}()

	waitForMCPBusyRetry(t, dm)
	release()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("recordToolInvocation never completed")
	}

	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM tool_invocations WHERE tool_name = 'repro_tool'`).Scan(&n); err != nil {
		t.Fatalf("counting the audit row: %v", err)
	}
	if n != 1 {
		t.Fatalf("the MCP dispatch audit row was lost under contention")
	}
}
