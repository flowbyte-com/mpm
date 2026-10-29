// scratchpad_write_budget_test.go — issue #164's audit, completed.
//
// WHY THIS FILE EXISTS
// --------------------
// Issue #164 listed three options. The first two (correct the two named
// write paths, and give the `retries=0` call sites a real budget) were
// discharged by PR #169. The THIRD — "audit every `retries=0` write call
// site and decide, per site, whether it genuinely cannot contend" — was
// discharged for the sites #169 named, but the audit itself was
// incomplete: two `retries=0` writes survived it.
//
// Both survivors are in this package:
//
//	internal/core/tools/handlers.go — handleFlushScratchpad
//	internal/core/tools/handlers.go — handleDiscardScratchpad
//
// Both write the `ephemeral_scratchpad` table, which lives in the
// substrate's own `mpm.db`. The scratchpad is a VOLATILE working thesis
// — it decays, it is discarded, it is not durable knowledge — but
// volatility is a property of the ARTIFACT, not of the WRITE. Nothing
// about these two statements makes them uncontendable: they are ordinary
// writes against a database that other processes write to concurrently.
//
// The audit's answer for both is therefore the same: they CAN contend,
// so they get the documented write budget.
//
// WHAT THIS FILE PROVES
// ---------------------
// 1. BEHAVIOUR. The flush path survives ordinary lock contention — the
//    write lands, exactly once, and the caller's error is nil. Before
//    the fix, the same scenario returned "database is locked" and the
//    scratchpad row was never written.
//
// The STRUCTURAL half of this audit — "no write in the substrate carries a
// zero retry budget unless an outer layer owns that busy class" — is
// `internal/core/tx_write_budget_guard_test.go`. It lives in the core
// package because it walks the whole substrate, this nested module
// included.
//
// THE METHOD
// ----------
// The same standard as #163/#169: a file-backed WAL database opened with
// the PRODUCTION DSN (`internal.SqliteWriteDSN`), and deterministic
// contention produced by a second pool holding the WAL write lock.
//
// WHEN THE LOCK IS RELEASED IS THE POINT. The holder must be released
// at the moment the writer has genuinely collided with the lock, and not
// before — otherwise the busy handler absorbs the contention and the
// write succeeds with no retry at all, which would make the test pass
// vacuously. The collision is detected through the retry counter: it
// moves only AFTER a failed write attempt.
//
// Disposable file-backed databases and isolated workspace roots only.

package tools

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// scratchpadWALDB opens a file-backed WAL database with the production
// DSN and returns a manager over it, plus a second pool on the same file
// that can be used to hold the WAL write lock.
//
// The harness mirrors internal/core/tx_busy_snapshot_test.go, which cannot
// be reused here: it lives in the `internal` package's test binary, and a
// `_test.go` file is not importable from a sibling package.
func scratchpadWALDB(t *testing.T) (*mpminternal.DatabaseManager, *sql.DB) {
	t.Helper()
	mpminternal.PinIsolatedWorkspace(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "mpm.db")

	db, err := sql.Open("sqlite3", mpminternal.SqliteWriteDSN(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatalf("PRAGMA journal_mode = WAL: %v", err)
	}

	dm := mpminternal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	// A dedicated probe table keeps the low-level holder off the
	// scratchpad table, so the only thing under test is the lock
	// behaviour and not the scratchpad's own constraints.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS scratchpad_probe (id TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatalf("create scratchpad_probe: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO scratchpad_probe (id, v) VALUES ('seed', 'x')`); err != nil {
		t.Fatalf("seed scratchpad_probe: %v", err)
	}

	competitor, err := sql.Open("sqlite3", mpminternal.SqliteWriteDSN(path))
	if err != nil {
		t.Fatalf("competitor sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = competitor.Close() })

	return dm, competitor
}

// holdScratchpadWALWriteLock takes the WAL write lock on the competitor
// pool and returns the release function.
func holdScratchpadWALWriteLock(t *testing.T, competitor *sql.DB) (release func()) {
	t.Helper()
	tx, err := competitor.Begin()
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO scratchpad_probe (id, v) VALUES ('holder', 'h')`); err != nil {
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

// runScratchpadUnderContention runs fn while a competing connection holds
// the WAL write lock, and returns fn's error.
//
// The release is scheduled, not awaited: the writer must experience the
// contention WHILE the lock is held, and the lock must be released while
// the writer is retrying.
func runScratchpadUnderContention(t *testing.T, dm *mpminternal.DatabaseManager, competitor *sql.DB, fn func() error) error {
	t.Helper()

	release := holdScratchpadWALWriteLock(t, competitor)
	defer release()

	errCh := make(chan error, 1)
	go func() { errCh <- fn() }()

	// Wait for the collision. The retry counter moves only after a
	// failed write attempt, so it is the signal that the writer is
	// provably inside the busy window.
	deadline := time.Now().Add(30 * time.Second)
	for dm.BusyRetryCount() == 0 {
		select {
		case err := <-errCh:
			// The write finished before any retry happened. That is
			// the pre-fix behaviour (the row was lost) or a
			// contention that never materialised. Either way the
			// caller reports the outcome; the release below is a
			// formality.
			release()
			return err
		default:
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	release()

	select {
	case err := <-errCh:
		return err
	case <-time.After(90 * time.Second):
		t.Fatal("the write did not complete")
		return nil
	}
}

// TestFlushScratchpadSurvivesOrdinaryContention is the behavioural half.
//
// BEFORE the fix, this exact scenario produced "flush scratchpad:
// database is locked" and the scratchpad row was never written. AFTER
// the fix, the write rides the statement-level retry budget and the row
// lands — exactly once.
func TestFlushScratchpadSurvivesOrdinaryContention(t *testing.T) {
	dm, competitor := scratchpadWALDB(t)

	err := runScratchpadUnderContention(t, dm, competitor, func() error {
		_, err := handleFlushScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
			"session_id": "sess-contention",
			"thesis":     "a thesis that must not be lost to a lock",
		})
		return err
	})
	if err != nil {
		t.Fatalf("the scratchpad write must survive ordinary contention: %v", err)
	}
	if dm.BusyRetryCount() == 0 {
		t.Fatal("the retry counter never moved: the contention never happened, so this test proves nothing")
	}

	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM ephemeral_scratchpad WHERE session_id = ?`, "sess-contention",
	).Scan(&n); err != nil {
		t.Fatalf("count scratchpad: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one scratchpad row, got %d", n)
	}
}

// TestDiscardScratchpadSurvivesOrdinaryContention covers the sibling
// path. A discard that fails leaves a stale row behind: the agent
// believes it cleared its working thesis, and the next wake context
// surfaces a thesis it thought it had discarded.
func TestDiscardScratchpadSurvivesOrdinaryContention(t *testing.T) {
	dm, competitor := scratchpadWALDB(t)

	if _, err := handleFlushScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"session_id": "sess-discard",
		"thesis":     "to be discarded",
	}); err != nil {
		t.Fatalf("seed scratchpad: %v", err)
	}

	err := runScratchpadUnderContention(t, dm, competitor, func() error {
		_, err := handleDiscardScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
			"session_id": "sess-discard",
		})
		return err
	})
	if err != nil {
		t.Fatalf("the scratchpad delete must survive ordinary contention: %v", err)
	}
	if dm.BusyRetryCount() == 0 {
		t.Fatal("the retry counter never moved: the contention never happened, so this test proves nothing")
	}

	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM ephemeral_scratchpad WHERE session_id = ?`, "sess-discard",
	).Scan(&n); err != nil {
		t.Fatalf("count scratchpad: %v", err)
	}
	if n != 0 {
		t.Fatalf("the scratchpad row survived the discard: %d row(s) remain", n)
	}
}
