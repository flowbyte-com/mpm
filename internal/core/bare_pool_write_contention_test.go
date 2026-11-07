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
