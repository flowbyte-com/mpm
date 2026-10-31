package internal

// startup_ddl_contention_repro_test.go — issue #178, phase 2:
// startup DDL and migration backfills.
//
// Startup migrations run once per boot under the schema-init lock, are
// single-statement and idempotent — but they had no statement-level
// retry, so a concurrent writer during a boot migration could leave a
// missing column, a missing index, or an un-backfilled row with only a
// warning. This pins that the whole InitSchema path survives a
// contention window that outlasts the busy timeout.
//
// Multi-statement trigger DDL is deliberately out of scope here: a
// retry can compound a partial multi-statement Exec, so those want a
// transaction, not a budget (the lessons-view sequence already runs in
// one).

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRepro_InitSchemaSurvivesContention pins that a boot migration
// survives lock contention: every single-statement DDL and backfill in
// the startup path carries a retry budget.
func TestRepro_InitSchemaSurvivesContention(t *testing.T) {
	PinIsolatedWorkspace(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "mpm.db")
	db, err := sql.Open("sqlite3", SqliteWriteDSN(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE tx_probe (id TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatalf("seed tx_probe: %v", err)
	}
	dm := NewDatabaseManagerForDB(db)

	competitor, err := sql.Open("sqlite3", SqliteWriteDSN(path))
	if err != nil {
		t.Fatalf("competitor open: %v", err)
	}
	t.Cleanup(func() { _ = competitor.Close() })
	tx, err := competitor.Begin()
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO tx_probe (id, v) VALUES ('holder', 'h')`); err != nil {
		t.Fatalf("holder insert: %v", err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if err := tx.Commit(); err != nil {
				t.Errorf("holder commit: %v", err)
			}
		})
	}
	defer release()

	done := make(chan error, 1)
	go func() {
		done <- dm.InitSchema()
	}()

	// Hold past the busy timeout: a bare migration DDL fails at 5s.
	time.Sleep(6 * time.Second)
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("InitSchema must survive contention, got: %v", err)
		}
	case <-time.After(120 * time.Second):
		t.Fatalf("InitSchema never completed")
	}

	// The schema the migrations promise must actually be there.
	for _, table := range []string{"memories", "system_audit_log", "scheduled_tasks", "session_handoffs"} {
		var n int
		if err := dm.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&n); err != nil {
			t.Fatalf("probing %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("table %s missing after a contended InitSchema", table)
		}
	}
}
