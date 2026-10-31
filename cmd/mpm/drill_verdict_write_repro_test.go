package main

// drill_verdict_write_repro_test.go — issue #178, phase 2.
//
// The drill verdict is the run's durable outcome. Losing the UPDATE
// leaves the run `running` forever. The error used to be discarded
// (`_, _ =`), so a busy failure was silent. These tests pin the fix:
// the verdict write carries the statement-level retry budget and its
// failure reaches the caller.

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func drillDBPath(ws string) string {
	return filepath.Join(ws, "src", "db", "mpm.db")
}

func openDrillTestDB(t *testing.T) (*mpminternal.DatabaseManager, *sql.DB) {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	raw, err := sql.Open("sqlite3", mpminternal.SqliteWriteDSN(drillDBPath(ws)))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	return dm, raw
}

func seedDrillRun(t *testing.T, raw *sql.DB, runID string) {
	t.Helper()
	if _, err := raw.Exec(
		`INSERT INTO drill_runs (id, drill_id, framework, session_id, status, started_at)
		 VALUES (?, 'd1', 'synthetic', 's1', 'running', ?)`,
		runID, time.Now().Unix()); err != nil {
		t.Fatalf("seed drill run: %v", err)
	}
}

func drillRunStatus(t *testing.T, raw *sql.DB, runID string) string {
	t.Helper()
	var status string
	if err := raw.QueryRow(`SELECT status FROM drill_runs WHERE id = ?`, runID).Scan(&status); err != nil {
		t.Fatalf("read drill status: %v", err)
	}
	return status
}

// TestRepro_DrillVerdictReportsFailure pins the dropped error: a
// verdict UPDATE that does not land must be returned, not discarded.
func TestRepro_DrillVerdictReportsFailure(t *testing.T) {
	dm, raw := openDrillTestDB(t)
	const runID = "repro-drill-verdict"
	seedDrillRun(t, raw, runID)

	if _, err := raw.Exec(
		`CREATE TRIGGER repro_block_verdict BEFORE UPDATE ON drill_runs
		 BEGIN SELECT RAISE(ABORT, 'verdict blocked'); END;`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = raw.Exec(`DROP TRIGGER IF EXISTS repro_block_verdict`) })

	if err := updateDrillRunVerdict(dm, runID, "passed", `{"passed":true}`, 1, 1); err == nil {
		t.Fatalf("a verdict that did not land must be reported: the run would stay `running` forever")
	}
	if got := drillRunStatus(t, raw, runID); got != "running" {
		t.Fatalf("the blocked verdict must not have applied, status = %q", got)
	}
}

// holdDrillWALWriteLock holds the WAL write lock on a competing
// connection with an uncommitted write, and returns the function that
// commits it. Same technique as the core contention harness.
func holdDrillWALWriteLock(t *testing.T, ws string) (release func()) {
	t.Helper()
	competitor, err := sql.Open("sqlite3", mpminternal.SqliteWriteDSN(drillDBPath(ws)))
	if err != nil {
		t.Fatalf("competitor open: %v", err)
	}
	t.Cleanup(func() { competitor.Close() })
	tx, err := competitor.Begin()
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO drill_runs (id, drill_id, framework, session_id, status, started_at)
		VALUES ('holder', 'd1', 'synthetic', 's1', 'running', 0)`); err != nil {
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

// waitForBusyRetry blocks until the statement-level retry engages.
func waitForBusyRetry(t *testing.T, dm *mpminternal.DatabaseManager) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if dm.BusyRetryCount() > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the statement-level retry to engage")
}

// TestRepro_DrillVerdictSurvivesContention pins the retry budget: the
// verdict write survives a contention window that outlasts the busy
// timeout.
func TestRepro_DrillVerdictSurvivesContention(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	raw, err := sql.Open("sqlite3", mpminternal.SqliteWriteDSN(drillDBPath(ws)))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { raw.Close() })
	const runID = "repro-drill-contention"
	seedDrillRun(t, raw, runID)

	release := holdDrillWALWriteLock(t, ws)
	defer release()

	done := make(chan error, 1)
	go func() {
		done <- updateDrillRunVerdict(dm, runID, "passed", `{"passed":true}`, 1, 1)
	}()

	waitForBusyRetry(t, dm)
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the verdict write must survive contention, got: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the verdict write never completed")
	}
	if got := drillRunStatus(t, raw, runID); got != "passed" {
		t.Fatalf("the verdict was lost under contention, status = %q", got)
	}
}
