package internal

// bare_pool_write_contention_test.go — issue #178, first remediation
// slice: the contention reproduction.
//
// WHAT THIS FILE PROVES
// ---------------------
// A BARE pool write loses its artifact when the contention outlasts
// SQLite's busy timeout: the statement returns "database is locked" and
// the row is simply not there. The TRACKED route — ExecTracked with the
// substrate's budget of 5 — survives the same contention and lands the
// write.
//
// The reproduction is deliberately built on DISPOSABLE databases: a
// temporary directory, a temporary SQLite file, two independent
// connections to that file. It never opens the operator's ~/.mpm and it
// never touches a running service.
//
// WHY busy_timeout IS SET SHORT HERE
// ----------------------------------
// The production DSN asks SQLite to wait five seconds for a lock
// (sqliteWriteDSN → _busy_timeout=5000). A test that reproduced the
// production behaviour literally would have to hold the lock for longer
// than five seconds, which makes the test slow without making it more
// convincing: the defect is that the write has NO retry, not that the
// timeout is five seconds. The timeout here is 100 milliseconds, so the
// contention outlasts it quickly and the reproduction runs in
// milliseconds. The RETRY POLICY under test — "an ordinary SQLITE_BUSY
// is a wait, and the budget decides how long the substrate waits it
// out" — is identical at either timeout.
//
// WHY THE LOCK IS RELEASED FROM A SEPARATE GOROUTINE
// --------------------------------------------------
// The bare write must fail while the lock is HELD (that is the defect),
// and the tracked write must succeed after the lock is RELEASED (that is
// the fix). Releasing from a goroutine is what lets the same test show
// both, on the same database, with the same contention.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// contentionDSN opens a disposable SQLite file with a SHORT busy timeout.
//
// _busy_timeout=100 makes the contention outlast the driver's own wait
// in ~100ms, so a bare write fails fast and the test stays fast.
func contentionDSN(t *testing.T, dir, name string) string {
	t.Helper()
	return fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=100&_foreign_keys=1",
		filepath.Join(dir, name))
}

// holdWriteLock takes the SQLite write lock on db and returns the
// function that releases it.
//
// BEGIN IMMEDIATE acquires the write lock at the start of the
// transaction, so the lock is held for the whole transaction rather
// than only for the duration of the INSERT.
//
// The lock is taken by INSERTING a row, so the lock table must exist.
// It is created here, in the disposable database, so the test never
// depends on any pre-existing schema.
func holdWriteLock(t *testing.T, db *sql.DB) (release func()) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS contention_lock (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create the lock table: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin the lock-holding transaction: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO contention_lock (id) VALUES ('holder')`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("take the write lock: %v", err)
	}
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		if err := tx.Commit(); err != nil {
			t.Errorf("release the write lock: %v", err)
		}
	}
}

// TestBarePoolWriteLosesItsArtifactToContention is the reproduction.
//
// It asserts BOTH halves of the defect class:
//
//  1. the bare pool write (`db.Exec`) fails while the lock is held, and
//     the row it was supposed to write is NOT there — the artifact is
//     lost;
//  2. the tracked route (`execTracked(db, query, 5, ...)`) waits the
//     contention out and the row IS there.
//
// Both halves run against the same disposable database and the same
// held lock, so the difference between them is the retry policy and
// nothing else.
func TestBarePoolWriteLosesItsArtifactToContention(t *testing.T) {
	dir := t.TempDir()

	// Two INDEPENDENT pools over the same file: the pool holds the
	// write lock, the other pool is the writer that contends with it.
	holder, err := sql.Open("sqlite3", contentionDSN(t, dir, "contention.db"))
	if err != nil {
		t.Fatalf("open the lock holder: %v", err)
	}
	defer holder.Close()
	if _, err := holder.Exec(`CREATE TABLE artifacts (id TEXT PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatalf("create the artifacts table: %v", err)
	}

	writer, err := sql.Open("sqlite3", contentionDSN(t, dir, "contention.db"))
	if err != nil {
		t.Fatalf("open the contending writer: %v", err)
	}
	defer writer.Close()
	// One connection, so the writer cannot quietly serialise itself.
	writer.SetMaxOpenConns(1)

	release := holdWriteLock(t, holder)

	// --- 1. the bare pool write loses the artifact -------------------
	if _, err := writer.Exec(`INSERT INTO artifacts (id, body) VALUES ('bare', 'lost')`); err == nil {
		t.Fatal("the bare pool write SUCCEEDED while the write lock was held — " +
			"the contention did not reproduce, so this test proves nothing")
	} else if !isBusyError(err) {
		t.Fatalf("the bare pool write failed with %v, which is not a lock contention", err)
	}
	var body string
	err = writer.QueryRow(`SELECT body FROM artifacts WHERE id = 'bare'`).Scan(&body)
	if err == nil {
		t.Fatalf("the bare pool write reported failure but the row IS there (body=%q)", body)
	}
	if err != sql.ErrNoRows {
		t.Fatalf("reading the bare row back: %v", err)
	}

	// --- 2. the tracked route survives the same contention ------------
	// The lock is released while the tracked write is in flight, so the
	// tracked write has to WAIT IT OUT — that is the whole difference.
	done := make(chan error, 1)
	go func() {
		out := execTracked(writer, `INSERT INTO artifacts (id, body) VALUES ('tracked', 'kept')`, 5, nil)
		done <- out.Err
	}()

	// Give the tracked write time to fail at least one attempt against
	// the held lock, so the success below is the result of a RETRY and
	// not of a write that never contended.
	time.Sleep(300 * time.Millisecond)
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the tracked write did not survive the contention: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the tracked write never returned")
	}

	var tracked string
	if err := writer.QueryRow(`SELECT body FROM artifacts WHERE id = 'tracked'`).Scan(&tracked); err != nil {
		t.Fatalf("the tracked write reported success but the row is not there: %v", err)
	}
	if tracked != "kept" {
		t.Fatalf("the tracked row has body %q, want %q", tracked, "kept")
	}
}

// TestConvertedMemoryWriteSurvivesContention drives the CONVERTED
// production path — DatabaseManager.SetMemoryWeight, one of the twenty
// sites this slice moved onto ExecTracked — under the same contention.
//
// It is the end-to-end half of the reproduction: the first test proves
// the POLICY, this one proves that the converted code path USES it.
//
// The DatabaseManager is built over a disposable file with a short busy
// timeout, so the contention outlasts the driver's wait and the budget
// has to do the work.
func TestConvertedMemoryWriteSurvivesContention(t *testing.T) {
	dir := t.TempDir()
	dsn := contentionDSN(t, dir, "memory-contention.db")

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open the disposable database: %v", err)
	}
	defer db.Close()
	dm := NewDatabaseManagerForDB(db)
	defer dm.Close()
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("init the disposable schema: %v", err)
	}

	// A second, independent connection to the SAME file holds the write
	// lock while the converted write contends with it.
	holder, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open the lock holder: %v", err)
	}
	defer holder.Close()

	// Seed a memory to act on.
	const memoryID = "contention-memory"
	if _, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, created_at, weight) VALUES (?, 'memories', 'seed', 0, 1)`,
		5, memoryID); err != nil {
		t.Fatalf("seed the memory: %v", err)
	}

	release := holdWriteLock(t, holder)

	done := make(chan error, 1)
	go func() {
		_, err := dm.SetMemoryWeight(memoryID, 42)
		done <- err
	}()

	time.Sleep(300 * time.Millisecond)
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the converted memory write did not survive the contention: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the converted memory write never returned")
	}

	var weight float64
	if err := dm.db.QueryRow(`SELECT weight FROM memories WHERE id = ?`, memoryID).Scan(&weight); err != nil {
		t.Fatalf("read the weight back: %v", err)
	}
	if weight != 42 {
		t.Fatalf("the weight is %v, want 42 — the write did not land", weight)
	}
}

// TestPoolMemExecerWaitsOutContention is the RUNTIME half of the
// adapter's contract.
//
// The AST guard in shared_primitive_budget_guard_test.go says the
// adapter's Exec routes through the substrate's statement-level retry
// policy. That is a claim about the CODE. This test is the claim
// about the BEHAVIOUR: the adapter really does wait ordinary
// contention out and land its write, where a bare pool write of the
// same statement loses the artifact.
//
// It exists because an AST assertion alone cannot prove that the
// policy it routes through is worth routing through. The review of
// 2026-11-06 (finding M2) showed that the adapter's assertion could
// be switched off without anything failing; this test fails if the
// adapter stops routing through the tracked policy in a way that
// changes what happens to the write.
//
// The two halves run against the SAME disposable database and the SAME
// held lock, so the difference between them is the retry policy and
// nothing else:
//
//  1. a BARE pool write of the same statement fails while the lock is
//     held, and the row is not there; and
//  2. the same statement through poolMemExecer succeeds after the
//     lock is released.
func TestPoolMemExecerWaitsOutContention(t *testing.T) {
	dir := t.TempDir()
	dsn := contentionDSN(t, dir, "adapter-contention.db")

	holder, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open the lock holder: %v", err)
	}
	defer holder.Close()
	if _, err := holder.Exec(`CREATE TABLE artifacts (id TEXT PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatalf("create the artifacts table: %v", err)
	}

	writer, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open the contending writer: %v", err)
	}
	defer writer.Close()
	// One connection, so the writer cannot quietly serialise itself.
	writer.SetMaxOpenConns(1)

	release := holdWriteLock(t, holder)

	// --- 1. the bare pool write loses the artifact -------------------
	const bareStatement = `INSERT INTO artifacts (id, body) VALUES ('bare-adapter', 'lost')`
	if _, err := writer.Exec(bareStatement); err == nil {
		t.Fatal("the bare pool write SUCCEEDED while the write lock was held — " +
			"the contention did not reproduce, so this test proves nothing")
	} else if !isBusyError(err) {
		t.Fatalf("the bare pool write failed with %v, which is not a lock contention", err)
	}
	var body string
	err = writer.QueryRow(`SELECT body FROM artifacts WHERE id = 'bare-adapter'`).Scan(&body)
	if err == nil {
		t.Fatalf("the bare pool write reported failure but the row IS there (body=%q)", body)
	}
	if err != sql.ErrNoRows {
		t.Fatalf("reading the bare row back: %v", err)
	}

	// --- 2. the adapter's write survives the same contention ---------
	// The lock is released while the adapter's write is in flight, so
	// the adapter has to WAIT IT OUT — that is the whole difference.
	executor := poolMemExecer{db: writer, retries: 5}
	done := make(chan error, 1)
	go func() {
		_, err := executor.Exec(`INSERT INTO artifacts (id, body) VALUES ('budgeted-adapter', 'kept')`)
		done <- err
	}()

	// Give the adapter's write time to fail at least one attempt
	// against the held lock, so the success below is the result of a
	// RETRY and not of a write that never contended.
	time.Sleep(300 * time.Millisecond)
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the adapter's write did not survive the contention: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the adapter's write never returned")
	}

	var kept string
	if err := writer.QueryRow(`SELECT body FROM artifacts WHERE id = 'budgeted-adapter'`).Scan(&kept); err != nil {
		t.Fatalf("the adapter's write reported success but the row is not there: %v", err)
	}
	if kept != "kept" {
		t.Fatalf("the adapter's row has body %q, want %q", kept, "kept")
	}
}

// ---------------------------------------------------------------------------
// Issue #178, SECOND remediation slice: the durable user-state writes.
// ---------------------------------------------------------------------------
//
// The slice covers the durable state of a session, its handoffs, its work
// items and its topic links — the writes whose loss is a loss of USER
// state, not of a diagnostic.
//
// The harness above proves the POLICY (a bare pool write loses its
// artifact; the tracked route waits the contention out). The tests below
// prove the POLICY IS WIRED INTO THE PRODUCTION CODE, one production call
// per converted site: each site is driven through its real DatabaseManager
// method while a SECOND connection holds the SQLite write lock.
//
// WHY THE LOCK IS RELEASED ON A RETRY, NOT ON A CLOCK
// ---------------------------------------------------
// The first slice released the lock after a fixed sleep. That is a race:
// the release has to land between one attempt's busy timeout and the next
// attempt's, and a loaded machine can miss the window — the write then
// fails for the wrong reason and the test proves nothing.
//
// These tests release the lock the moment the manager has demonstrably
// ABSORBED A RETRY (dm.BusyRetryCount() moves). The retry counter is
// incremented by ExecTracked after a failed attempt and before the
// backoff sleep, so its movement proves that the write met the held lock,
// lost the first attempt, and is on its way round again. The release
// therefore lands inside the backoff window, where the next attempt is
// guaranteed to find the lock free.
//
// If the write returns BEFORE any retry is observed, it took the BARE
// path: it failed on its first attempt against the held lock and the
// artifact was lost. That is the defect, and the test says so.

// slice2Site is one converted site: the rows it needs, the production call
// it makes, and the durable state it must leave behind.
type slice2Site struct {
	// name identifies the site in the subtest name.
	name string
	// seed inserts the rows the production call operates on. It runs
	// BEFORE the write lock is taken.
	seed func(t *testing.T, dm *DatabaseManager, db *sql.DB)
	// invoke makes the production call. It runs on a goroutine, so it
	// must RETURN the error rather than fail the test itself.
	invoke func(dm *DatabaseManager) error
	// verify asserts the durable state landed.
	verify func(t *testing.T, dm *DatabaseManager, db *sql.DB)
}

// slice2Sites returns the converted sites of the second slice.
//
// Each case gets its own closure variables (the generated work id), so
// the list must be BUILT per test rather than shared as a package
// variable.
func slice2Sites() []slice2Site {
	var addedWorkID string

	return []slice2Site{
		{
			// The session summary — the statement issue #178 names as
			// the shape's example.
			name: "UpdateSessionSummary",
			seed: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				// The production function writes sessions.summary. The
				// CURRENT schema does not carry that column — a
				// separate defect, recorded in the slice-2 record and
				// filed as its own follow-up rather than smuggled in
				// here. The reproduction adds the column to the
				// DISPOSABLE database so the production write path can
				// be exercised end to end; the tree's own schema is
				// untouched.
				if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN summary TEXT`); err != nil {
					t.Fatalf("add sessions.summary to the disposable database: %v", err)
				}
				if _, err := db.Exec(
					`INSERT INTO sessions (id, session_id, content, content_hash) VALUES ('s-slice2', 'sess-slice2', 'body', 'hash-slice2')`,
				); err != nil {
					t.Fatalf("seed the session row: %v", err)
				}
			},
			invoke: func(dm *DatabaseManager) error {
				return dm.UpdateSessionSummary("sess-slice2", "the summary that must survive")
			},
			verify: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				var summary string
				if err := db.QueryRow(`SELECT summary FROM sessions WHERE session_id = 'sess-slice2'`).Scan(&summary); err != nil {
					t.Fatalf("read the session summary back: %v", err)
				}
				if summary != "the summary that must survive" {
					t.Fatalf("the session summary is %q — the write did not land", summary)
				}
			},
		},
		{
			name: "AddMemoryToTopic",
			seed: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				if _, err := db.Exec(
					`INSERT INTO topics (id, name, description) VALUES ('t-slice2', 'Slice Two Topic', '')`,
				); err != nil {
					t.Fatalf("seed the topic row: %v", err)
				}
				if _, err := db.Exec(
					`INSERT INTO memories (id, collection, content, created_at, weight) VALUES ('m-slice2', 'memories', 'seed', 0, 1)`,
				); err != nil {
					t.Fatalf("seed the memory row: %v", err)
				}
			},
			invoke: func(dm *DatabaseManager) error {
				return dm.AddMemoryToTopic("m-slice2", "t-slice2", "manual")
			},
			verify: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				var role string
				if err := db.QueryRow(
					`SELECT role FROM topic_memberships WHERE memory_id = 'm-slice2' AND topic_id = 't-slice2'`,
				).Scan(&role); err != nil {
					t.Fatalf("read the topic membership back: %v", err)
				}
				if role != "manual" {
					t.Fatalf("the topic membership role is %q, want %q — the write did not land", role, "manual")
				}
			},
		},
		{
			name: "RemoveMemoryFromTopic",
			seed: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				if _, err := db.Exec(
					`INSERT INTO topics (id, name, description) VALUES ('t-slice2', 'Slice Two Topic', '')`,
				); err != nil {
					t.Fatalf("seed the topic row: %v", err)
				}
				if _, err := db.Exec(
					`INSERT INTO memories (id, collection, content, created_at, weight) VALUES ('m-slice2', 'memories', 'seed', 0, 1)`,
				); err != nil {
					t.Fatalf("seed the memory row: %v", err)
				}
				if _, err := db.Exec(
					`INSERT INTO topic_memberships (memory_id, topic_id, created_at, role) VALUES ('m-slice2', 't-slice2', 1, 'manual')`,
				); err != nil {
					t.Fatalf("seed the topic membership row: %v", err)
				}
			},
			invoke: func(dm *DatabaseManager) error {
				removed, err := dm.RemoveMemoryFromTopic("m-slice2", "t-slice2")
				if err != nil {
					return err
				}
				if !removed {
					return fmt.Errorf("RemoveMemoryFromTopic reported the membership as absent")
				}
				return nil
			},
			verify: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				var n int
				if err := db.QueryRow(
					`SELECT COUNT(*) FROM topic_memberships WHERE memory_id = 'm-slice2' AND topic_id = 't-slice2'`,
				).Scan(&n); err != nil {
					t.Fatalf("count the topic memberships back: %v", err)
				}
				if n != 0 {
					t.Fatalf("the topic membership is still there (%d rows) — the delete did not land", n)
				}
			},
		},
		{
			name: "MarkHandoffRead",
			seed: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				if _, err := db.Exec(
					`INSERT INTO session_handoffs (id, session_id, ended_at, ended_state, summary, created_at) VALUES ('h-slice2', 'sess-slice2', 1, 'clean', 'a summary', 1)`,
				); err != nil {
					t.Fatalf("seed the handoff row: %v", err)
				}
			},
			invoke: func(dm *DatabaseManager) error {
				return dm.MarkHandoffRead("h-slice2", "reader-slice2")
			},
			verify: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				var readBy string
				var readAt sql.NullInt64
				if err := db.QueryRow(`SELECT read_by, read_at FROM session_handoffs WHERE id = 'h-slice2'`).Scan(&readBy, &readAt); err != nil {
					t.Fatalf("read the handoff back: %v", err)
				}
				if readBy != "reader-slice2" {
					t.Fatalf("the handoff read_by is %q, want %q — the write did not land", readBy, "reader-slice2")
				}
				if !readAt.Valid || readAt.Int64 <= 0 {
					t.Fatalf("the handoff read_at is %v — the write did not land", readAt)
				}
			},
		},
		{
			name: "DeleteHandoff",
			seed: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				if _, err := db.Exec(
					`INSERT INTO session_handoffs (id, session_id, ended_at, ended_state, summary, created_at) VALUES ('h-slice2', 'sess-slice2', 1, 'clean', 'a summary', 1)`,
				); err != nil {
					t.Fatalf("seed the handoff row: %v", err)
				}
			},
			invoke: func(dm *DatabaseManager) error {
				n, err := dm.DeleteHandoff("h-slice2")
				if err != nil {
					return err
				}
				if n != 1 {
					return fmt.Errorf("DeleteHandoff reported %d rows deleted, want 1", n)
				}
				return nil
			},
			verify: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				var n int
				if err := db.QueryRow(`SELECT COUNT(*) FROM session_handoffs WHERE id = 'h-slice2'`).Scan(&n); err != nil {
					t.Fatalf("count the handoff rows back: %v", err)
				}
				if n != 0 {
					t.Fatalf("the handoff row is still there — the delete did not land")
				}
			},
		},
		{
			name: "AddWork",
			seed: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {},
			invoke: func(dm *DatabaseManager) error {
				w, err := dm.AddWork("Slice two work item", "A work item whose creation must survive contention", "")
				if err != nil {
					return err
				}
				addedWorkID = w.ID
				return nil
			},
			verify: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				if addedWorkID == "" {
					t.Fatal("the work item was never created — the invoke step did not run")
				}
				var title string
				if err := db.QueryRow(`SELECT title FROM works WHERE id = ?`, addedWorkID).Scan(&title); err != nil {
					t.Fatalf("read the work item back: %v", err)
				}
				if title != "Slice two work item" {
					t.Fatalf("the work item title is %q — the write did not land", title)
				}
			},
		},
		{
			name: "CompleteWork",
			seed: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				if _, err := db.Exec(
					`INSERT INTO works (id, title, content, status, created_at, updated_at) VALUES ('w-status-slice2', 'Status work', 'body', 'open', 0, 0)`,
				); err != nil {
					t.Fatalf("seed the work row: %v", err)
				}
			},
			invoke: func(dm *DatabaseManager) error {
				w, err := dm.CompleteWork("w-status-slice2")
				if err != nil {
					return err
				}
				if w.Status != WorkStatusDone {
					return fmt.Errorf("CompleteWork returned status %q, want %q", w.Status, WorkStatusDone)
				}
				return nil
			},
			verify: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				var status string
				if err := db.QueryRow(`SELECT status FROM works WHERE id = 'w-status-slice2'`).Scan(&status); err != nil {
					t.Fatalf("read the work status back: %v", err)
				}
				if status != string(WorkStatusDone) {
					t.Fatalf("the work status is %q, want %q — the write did not land", status, WorkStatusDone)
				}
			},
		},
		{
			name: "DeriveWorkVerification",
			seed: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				// Seeded with a verification value the derive does NOT
				// produce, so "the column still holds the seeded value"
				// is distinguishable from "the write landed".
				if _, err := db.Exec(
					`INSERT INTO works (id, title, content, status, created_at, updated_at, verification) VALUES ('w-verify-slice2', 'Verify work', 'body', 'open', 0, 0, 'partial')`,
				); err != nil {
					t.Fatalf("seed the work row: %v", err)
				}
			},
			invoke: func(dm *DatabaseManager) error {
				v, err := dm.DeriveWorkVerification("w-verify-slice2")
				if err != nil {
					return err
				}
				if v != WorkVerificationUnverified {
					return fmt.Errorf("DeriveWorkVerification returned %q, want %q", v, WorkVerificationUnverified)
				}
				return nil
			},
			verify: func(t *testing.T, dm *DatabaseManager, db *sql.DB) {
				var verification string
				if err := db.QueryRow(`SELECT verification FROM works WHERE id = 'w-verify-slice2'`).Scan(&verification); err != nil {
					t.Fatalf("read the verification back: %v", err)
				}
				if verification != string(WorkVerificationUnverified) {
					t.Fatalf("the verification is %q, want %q — the write did not land",
						verification, WorkVerificationUnverified)
				}
			},
		},
	}
}

// TestConvertedDurableUserStateWritesSurviveContention drives each
// converted site of the second slice through its PRODUCTION call while a
// second connection holds the SQLite write lock.
//
// The assertion has two halves, and both matter:
//
//  1. the production call returns no error, and
//  2. the durable state it was supposed to write is THERE.
//
// The second half is what makes this a durability test rather than an
// error-handling test: a write that reported success but left nothing
// behind would fail half 2.
func TestConvertedDurableUserStateWritesSurviveContention(t *testing.T) {
	for _, tc := range slice2Sites() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dsn := contentionDSN(t, dir, "slice2.db")

			db, err := sql.Open("sqlite3", dsn)
			if err != nil {
				t.Fatalf("open the disposable database: %v", err)
			}
			defer db.Close()
			dm := NewDatabaseManagerForDB(db)
			defer dm.Close()
			if err := dm.InitSchema(); err != nil {
				t.Fatalf("init the disposable schema: %v", err)
			}

			// InitSchema sets the PRODUCTION busy window on the pool's
			// connections (`PRAGMA busy_timeout = 5000`,
			// internal/core/db.go:2934). The harness deliberately uses a
			// SHORT window instead — see the file header: the defect is
			// that the write has NO retry, not that the window is five
			// seconds, and a short window keeps the reproduction in
			// milliseconds.
			//
			// The pool is pinned to one connection first, because
			// `PRAGMA busy_timeout` is a PER-CONNECTION setting and a
			// pragma issued through a multi-connection pool would only
			// reach whichever connection it happened to draw. Any
			// connection opened afterwards gets the short window from
			// the DSN anyway (contentionDSN sets _busy_timeout=100).
			db.SetMaxOpenConns(1)
			if _, err := db.Exec(`PRAGMA busy_timeout = 100`); err != nil {
				t.Fatalf("shorten the busy window on the disposable pool: %v", err)
			}

			// A second, INDEPENDENT pool over the same file holds the
			// write lock while the production write contends with it.
			holder, err := sql.Open("sqlite3", dsn)
			if err != nil {
				t.Fatalf("open the lock holder: %v", err)
			}
			defer holder.Close()

			tc.seed(t, dm, db)

			baseline := dm.BusyRetryCount()
			release := holdWriteLock(t, holder)

			done := make(chan error, 1)
			go func() { done <- tc.invoke(dm) }()

			// Wait until the write has demonstrably ABSORBED A RETRY
			// (the tracked shape) or has RETURNED (the bare shape, which
			// failed on its first attempt and lost the artifact).
			var early error
			returned := false
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				if dm.BusyRetryCount() > baseline {
					break
				}
				select {
				case e := <-done:
					returned, early = true, e
				default:
				}
				if returned {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}

			release()

			if returned {
				t.Fatalf("the production call returned WITHOUT absorbing a retry — "+
					"it took the bare path and lost the artifact: %v", early)
			}

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the converted write did not survive the contention: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the converted write never returned")
			}

			tc.verify(t, dm, db)
		})
	}
}
