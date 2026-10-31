package main

// audit_hook_contention_test.go — issue #178, phase 2 (audit ledger,
// F6, CLI path).
//
// recordToolInvocation runs on every `mpm call` dispatch. A busy
// failure used to lose the row with only a warning. The write stays
// best-effort (a warning, never a blocked call), but it now carries
// the statement-level retry budget. This pins that the row survives a
// contention window that outlasts the busy timeout.

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func openAuditTestDB(t *testing.T) (mpminternal.CoreDB, *sql.DB, string) {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	raw, err := sql.Open("sqlite3", mpminternal.SqliteWriteDSN(filepath.Join(ws, "src", "db", "mpm.db")))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	return dm, raw, ws
}

func holdAuditWALWriteLock(t *testing.T, ws string) (release func()) {
	t.Helper()
	competitor, err := sql.Open("sqlite3", mpminternal.SqliteWriteDSN(filepath.Join(ws, "src", "db", "mpm.db")))
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

func waitForAuditRetry(t *testing.T, dm mpminternal.CoreDB) {
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

// TestRepro_RecordToolInvocationSurvivesContention pins the CLI
// dispatch audit row: it must land despite contention, because the
// drill scorer and recent-activity feed are built from this table.
func TestRepro_RecordToolInvocationSurvivesContention(t *testing.T) {
	dm, raw, ws := openAuditTestDB(t)
	release := holdAuditWALWriteLock(t, ws)
	defer release()

	now := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		recordToolInvocation(dm, mpminternal.ActiveContext{},
			"repro-tool", map[string]interface{}{"action": "repro"},
			now, now, "repro", "success", nil)
	}()

	waitForAuditRetry(t, dm)
	release()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("recordToolInvocation never completed")
	}

	var n int
	if err := raw.QueryRow(
		`SELECT COUNT(*) FROM tool_invocations WHERE tool_name = 'repro-tool'`).Scan(&n); err != nil {
		t.Fatalf("counting the audit row: %v", err)
	}
	if n != 1 {
		t.Fatalf("the dispatch audit row was lost under contention")
	}
}
