// maintain_error_reporting_test.go — `mpm ops maintain` must not report a
// failed maintenance operation as a success.
//
// WHY THIS FILE EXISTS.
//
// The handler used to return 1 only when `RunSelfMaintenance` itself returned
// an error — and that function discarded every operation's error, so the CLI
// printed "✅ Self-maintenance complete" and exited 0 while decay, pruning or
// consolidation had not run.
//
// These tests drive the REAL router and the REAL built-in CLI handler, and
// assert the exit status and the banner, because those are the two things an
// operator (or a cron wrapper) actually keys off.

package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// maintWorkspace initialises a disposable workspace through the real CLI and
// returns its database path.
func maintWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	resetDBSingleton(t)

	if code, out := runCLI(t, "status"); code != 0 {
		t.Fatalf("initialise the workspace: exit %d\n%s", code, out)
	}
	return filepath.Join(ws, "src", "db", "mpm.db")
}

// seedAgedMemory inserts one decay-eligible memory.
func seedAgedMemory(t *testing.T, dbPath, id string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open workspace db: %v", err)
	}
	defer db.Close()
	old := int64(0)
	if err := db.QueryRow(`SELECT CAST(strftime('%s','now','-120 days') AS INTEGER)`).Scan(&old); err != nil {
		t.Fatalf("compute the old timestamp: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO memories (id, collection, content, tags, weight, metadata, created_at, updated_at)
		VALUES (?, 'memories', 'aged maintenance fixture', '[]', 10, '{}', ?, ?)`,
		id, old, old); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// TestMaintain_SuccessExitsZero is the control: a clean workspace must still
// print the success banner and exit 0.
func TestMaintain_SuccessExitsZero(t *testing.T) {
	dbPath := maintWorkspace(t)
	seedAgedMemory(t, dbPath, "cli-decay-a")

	var code int
	var out string
	userOut := captureUserError(t, func() int {
		code, out = runCLI(t, "ops", "maintain")
		return code
	})
	out += userOut
	if code != 0 {
		t.Fatalf("a clean maintenance exited %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "Self-maintenance complete") {
		t.Errorf("the success banner is missing:\n%s", out)
	}
	if strings.Contains(out, "WITH ERRORS") {
		t.Errorf("a clean maintenance reported errors:\n%s", out)
	}
}

// TestMaintain_FailedDecayExitsNonZeroAndKeepsTheReport is the regression: a
// failed decay must produce a non-zero exit, name the failed operation, and
// still print the statistics that ARE true.
func TestMaintain_FailedDecayExitsNonZeroAndKeepsTheReport(t *testing.T) {
	dbPath := maintWorkspace(t)
	seedAgedMemory(t, dbPath, "cli-decay-a")

	// Make every weight update abort. The trigger is a deterministic
	// injection: no clock, no contention, no sleeps.
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open workspace db: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER cli_decay_fail BEFORE UPDATE OF weight ON memories
		BEGIN SELECT RAISE(ABORT, 'injected CLI decay failure'); END;`); err != nil {
		db.Close()
		t.Fatalf("create the trigger: %v", err)
	}
	db.Close()

	var code int
	var out string
	userOut := captureUserError(t, func() int {
		code, out = runCLI(t, "ops", "maintain")
		return code
	})
	out += userOut

	if code != 1 {
		t.Fatalf("a failed maintenance exited %d, want 1\n%s", code, out)
	}
	if strings.Contains(out, "✅ Self-maintenance complete") {
		t.Errorf("the CLI printed the success banner for a failed run:\n%s", out)
	}
	if !strings.Contains(out, "WITH ERRORS") {
		t.Errorf("the CLI did not mark the run as failed:\n%s", out)
	}
	if !strings.Contains(out, "decay_weights") {
		t.Errorf("the CLI did not name the failed operation:\n%s", out)
	}
	if !strings.Contains(out, "injected CLI decay failure") {
		t.Errorf("the CLI did not surface the underlying cause:\n%s", out)
	}
	// Partial statistics are still reported: the other operations ran.
	for _, stat := range []string{"Decayed weights:", "Pruned total:", "Never accessed:"} {
		if !strings.Contains(out, stat) {
			t.Errorf("the partial report is missing %q:\n%s", stat, out)
		}
	}
}
