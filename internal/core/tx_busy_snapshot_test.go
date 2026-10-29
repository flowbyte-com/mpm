package internal

// Tests for issue #158 — the stale SQLite WAL snapshot
// (SQLITE_BUSY_SNAPSHOT) and the ineffective statement-level retry inside
// txNode.ExecTracked.
//
// BACKGROUND
// ----------
// MPM's WithTx opens a DEFERRED transaction. A deferred transaction takes a
// READ snapshot at its first read and acquires the write lock at its first
// write. If another connection commits in between, the write cannot upgrade:
// SQLite returns SQLITE_BUSY_SNAPSHOT (primary code SQLITE_BUSY, extended
// code 517).
//
// SQLITE_BUSY_SNAPSHOT is NOT a lock wait. SQLite does not invoke the busy
// handler for it, so busy_timeout is never consulted, and it shares BOTH its
// primary code and its message ("database is locked") with ordinary
// contention. The pre-fix code classified it as ordinary busy and retried
// the statement INSIDE the stale transaction five times — which can never
// succeed — burning 3100 ms of backoff before failing with the same error.
//
// The 2026-10-28 live-runtime audit found exactly this in production:
// `mpm_decisions record` failed with "database is locked" after 3104 ms and
// 3110 ms. The backoff schedule for five retries sums to 3100 ms, so every
// attempt had returned in under a millisecond — the BUSY_SNAPSHOT signature.
//
// THE FIX
// -------
//   - IsBusySnapshotError distinguishes the snapshot conflict by its
//     EXTENDED code, which is the only thing that tells it apart.
//   - txNode.ExecTracked no longer retries a snapshot conflict in place.
//   - WithTx restarts the whole transaction on a snapshot conflict: the
//     abandoned transaction is rolled back and the callback is re-run
//     against a fresh one, which takes a fresh snapshot.
//
// Every test that asserts a conflict was produced also asserts the
// mechanism that produced it, so a test cannot pass by being vacuous.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

// execBackoffSum is the total sleep of the statement-level retry schedule
// (100 + 200 + 400 + 800 + 1600 ms). The live watchdog recorded durations of
// 3104 ms and 3110 ms for the two production failures — the elapsed time was
// almost entirely backoff, which is how the audit identified the failure as
// BUSY_SNAPSHOT rather than lock contention.
const execBackoffSum = 3100 * time.Millisecond

// newWALTestDM returns a DatabaseManager over a TEMPORARY FILE database in
// WAL mode, opened with the PRODUCTION DSN, plus a second pool on the SAME
// file to act as a competing writer.
//
// WAL is not optional here: SQLITE_BUSY_SNAPSHOT only exists in WAL mode.
// The production DSN is used so busy_timeout=5000 and foreign_keys are
// exactly what production runs with — a test with different lock semantics
// would not reproduce the live failure.
//
// The competing writer needs its OWN pool: a transaction on the same pool
// occupies one connection, and the competing commit must come from a
// DIFFERENT connection to invalidate the first transaction's snapshot.
func newWALTestDM(t *testing.T) (*DatabaseManager, *sql.DB) {
	t.Helper()
	PinIsolatedWorkspace(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "mpm.db")

	db, err := sql.Open("sqlite3", SqliteWriteDSN(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatalf("PRAGMA journal_mode = WAL: %v", err)
	}

	dm := NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	// A dedicated probe table keeps the low-level tests off the memories
	// table's NOT NULL columns and FTS triggers, so the only thing under
	// test is the lock behaviour.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tx_probe (id TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatalf("create tx_probe: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tx_probe (id, v) VALUES ('seed', 'x')`); err != nil {
		t.Fatalf("seed tx_probe: %v", err)
	}

	competitor, err := sql.Open("sqlite3", SqliteWriteDSN(path))
	if err != nil {
		t.Fatalf("competitor sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = competitor.Close() })

	return dm, competitor
}

// describeSQLErr renders the driver's primary and extended codes, which is
// the only way to tell SQLITE_BUSY from SQLITE_BUSY_SNAPSHOT — they share a
// primary code AND a message.
func describeSQLErr(err error) string {
	if err == nil {
		return "nil"
	}
	var se sqlite3.Error
	if errors.As(err, &se) {
		return fmt.Sprintf("%v (primary=%d extended=%d)", err, se.Code, se.ExtendedCode)
	}
	return fmt.Sprintf("%v (%T)", err, err)
}

// holdWALWriteLock takes the WAL write lock on the competing connection with
// an UNCOMMITTED write, and returns the function that commits it.
//
// A held write lock is what turns a later writer's INSERT into a lock
// conflict. Releasing it (the commit) is what makes the WAL ADVANCE, and
// that advance is what invalidates the other transaction's read snapshot.
//
// The commit MUST happen after the other transaction has taken its snapshot,
// otherwise the snapshot already includes the commit and no conflict occurs.
func holdWALWriteLock(t *testing.T, competitor *sql.DB) (release func()) {
	t.Helper()
	tx, err := competitor.Begin()
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO tx_probe (id, v) VALUES ('holder', 'h')`); err != nil {
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

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, what)
}

// TestSnapshotHarness_ProducesARealConflict is the NEGATIVE CONTROL for the
// tests below: without the competing commit, the same read-then-write
// sequence succeeds. It proves the failures the other tests observe are
// caused by the competing commit and not by anything incidental in the
// harness.
func TestSnapshotHarness_ProducesARealConflict(t *testing.T) {
	dm, _ := newWALTestDM(t)

	tx, err := dm.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	node := &txNode{tx: tx, dm: dm}
	var n int
	if err := node.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&n); err != nil {
		t.Fatalf("snapshot read: %v", err)
	}

	// NO competing commit here.
	if _, err := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('writer', 'z')`, 5); err != nil {
		t.Fatalf("without a competing commit the write must succeed; got %s", describeSQLErr(err))
	}
}

// TestExecTracked_SnapshotConflictIsSQLiteBusySnapshotNotContention is the
// deterministic reproduction of the live failure recorded in issue #158.
//
// It asserts that the error is specifically SQLITE_BUSY_SNAPSHOT (extended
// code 517) and NOT ordinary lock contention (extended code 5). The two
// share a primary code and a message, so the extended code is the only
// thing that distinguishes them — and the distinction is what decides
// whether waiting can help.
func TestExecTracked_SnapshotConflictIsSQLiteBusySnapshotNotContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	tx, err := dm.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	node := &txNode{tx: tx, dm: dm}

	// The READ establishes the transaction's read snapshot.
	var n int
	if err := node.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&n); err != nil {
		t.Fatalf("snapshot read: %v", err)
	}

	// The competing writer commits AFTER the snapshot and BEFORE the write.
	// This is the exact interleaving that produces SQLITE_BUSY_SNAPSHOT.
	if _, err := competitor.Exec(`INSERT INTO tx_probe (id, v) VALUES ('competitor', 'y')`); err != nil {
		t.Fatalf("competitor commit: %v", err)
	}

	_, execErr := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('writer', 'z')`, 5)
	if execErr == nil {
		t.Fatal("the write succeeded; the harness did not produce a stale snapshot")
	}
	if !IsBusySnapshotError(execErr) {
		t.Fatalf("expected SQLITE_BUSY_SNAPSHOT (extended 517), got %s", describeSQLErr(execErr))
	}
	// A snapshot conflict IS still a busy error — the existing
	// IsBusyError-based retry paths must keep seeing it.
	if !IsBusyError(execErr) {
		t.Fatalf("IsBusyError must remain true for a snapshot conflict; got %s", describeSQLErr(execErr))
	}
}

// TestExecTracked_OrdinaryContentionIsNotASnapshotConflict is the mirror
// image of the test above: ordinary lock contention — another connection
// holding the write lock, not yet committed — must NOT be classified as a
// snapshot conflict.
//
// It matters because the fix routes the two classes to different remedies.
// Ordinary contention is transient and waitable, so the statement-level
// retry is still correct for it. A snapshot conflict is not, so it must be
// escalated to the transaction level. Classifying one as the other would
// either reintroduce the futile retry or abandon a retry that works.
func TestExecTracked_OrdinaryContentionIsNotASnapshotConflict(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	release := holdWALWriteLock(t, competitor)
	defer release()

	tx, err := dm.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	node := &txNode{tx: tx, dm: dm}
	var n int
	if err := node.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&n); err != nil {
		t.Fatalf("snapshot read: %v", err)
	}

	// The holder has NOT committed, so the WAL has not advanced: this is a
	// lock conflict, not a stale snapshot.
	_, execErr := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('writer', 'z')`, 0)
	if execErr == nil {
		t.Fatal("expected the write to conflict with the holder's lock")
	}
	if IsBusySnapshotError(execErr) {
		t.Fatalf("ordinary contention must not be classified as a snapshot conflict: %s", describeSQLErr(execErr))
	}
	if !IsBusyError(execErr) {
		t.Fatalf("ordinary contention must still be classified as busy: %s", describeSQLErr(execErr))
	}
}

// TestExecTracked_SnapshotConflictIsNotRetriedInPlace asserts the futile
// backoff burn is gone.
//
// Before the fix, ExecTracked retried the statement five times INSIDE the
// stale transaction and failed after 3100 ms — the exact duration the live
// watchdog recorded (3104 ms and 3110 ms). The retry could never succeed:
// the snapshot belongs to the transaction, and the transaction's snapshot does
// not change between attempts.
func TestExecTracked_SnapshotConflictIsNotRetriedInPlace(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	tx, err := dm.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	node := &txNode{tx: tx, dm: dm}
	var n int
	if err := node.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&n); err != nil {
		t.Fatalf("snapshot read: %v", err)
	}
	if _, err := competitor.Exec(`INSERT INTO tx_probe (id, v) VALUES ('competitor', 'y')`); err != nil {
		t.Fatalf("competitor commit: %v", err)
	}

	start := time.Now()
	_, execErr := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('writer', 'z')`, 5)
	elapsed := time.Since(start)

	if execErr == nil {
		t.Fatal("expected the write to fail with a stale snapshot")
	}
	if !IsBusySnapshotError(execErr) {
		t.Fatalf("expected SQLITE_BUSY_SNAPSHOT, got %s", describeSQLErr(execErr))
	}
	// The whole point: the statement must NOT have slept through the
	// 3100 ms backoff schedule. Half of it is a generous bound — the
	// pre-fix behaviour was execBackoffSum (3100 ms) plus the SQL work.
	if elapsed >= execBackoffSum/2 {
		t.Fatalf("the statement-level retry still burns the backoff budget: %v (backoff sum is %v)", elapsed, execBackoffSum)
	}
}

// TestIsBusySnapshotError_ClassifiesTheExtendedCode is the unit-level
// contract for the new classifier.
func TestIsBusySnapshotError_ClassifiesTheExtendedCode(t *testing.T) {
	if IsBusySnapshotError(nil) {
		t.Fatal("nil is not a snapshot conflict")
	}
	if IsBusySnapshotError(errors.New("database is locked")) {
		t.Fatal("a plain error carrying the busy message is not a snapshot conflict")
	}
	// A wrapped driver error must still be recognised — the error travels
	// up through fmt.Errorf("%w") on the way out of the write paths.
	snapshot := sqlite3.Error{Code: sqlite3.ErrBusy, ExtendedCode: sqlite3.ErrBusySnapshot}
	if !IsBusySnapshotError(snapshot) {
		t.Fatal("a bare SQLITE_BUSY_SNAPSHOT was not recognised")
	}
	if !IsBusySnapshotError(fmt.Errorf("record decision: %w", snapshot)) {
		t.Fatal("a wrapped SQLITE_BUSY_SNAPSHOT was not recognised")
	}
	// Ordinary contention must not match.
	plain := sqlite3.Error{Code: sqlite3.ErrBusy, ExtendedCode: sqlite3.ErrBusy.Extend(0)}
	if IsBusySnapshotError(plain) {
		t.Fatal("ordinary SQLITE_BUSY must not be classified as a snapshot conflict")
	}
}

// TestWithTx_RestartsTheTransactionOnAStaleSnapshot is the core fix test:
// the write must SUCCEED, and the transaction must have been restarted to
// achieve it.
func TestWithTx_RestartsTheTransactionOnAStaleSnapshot(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	attempts := 0
	err := dm.WithTx(func(node DBNode) error {
		attempts++
		// The READ that establishes the transaction's snapshot.
		var n int
		if err := node.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&n); err != nil {
			return err
		}
		if attempts == 1 {
			// The competing commit lands between this transaction's read
			// and its write.
			if _, err := competitor.Exec(`INSERT INTO tx_probe (id, v) VALUES ('competitor', 'y')`); err != nil {
				return err
			}
		}
		_, err := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('writer', 'z')`, 5)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx must recover from a stale snapshot; got %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected exactly one restart (2 attempts), got %d", attempts)
	}

	var c int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM tx_probe WHERE id = 'writer'`).Scan(&c); err != nil {
		t.Fatalf("count writer rows: %v", err)
	}
	if c != 1 {
		t.Fatalf("expected the restarted transaction's write to land exactly once, got %d rows", c)
	}
	// The abandoned first attempt must have left nothing behind.
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM tx_probe WHERE id = 'competitor'`).Scan(&c); err != nil {
		t.Fatalf("count competitor rows: %v", err)
	}
	if c != 1 {
		t.Fatalf("expected the competitor's committed row to survive, got %d", c)
	}
}

// TestWithTx_SnapshotRestartIsBounded asserts the restart budget is
// BOUNDED and that the error surfaces when it is exhausted. An unbounded
// restart would turn a contended write into an infinite loop.
func TestWithTx_SnapshotRestartIsBounded(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	attempts := 0
	err := dm.WithTx(func(node DBNode) error {
		attempts++
		var n int
		if err := node.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&n); err != nil {
			return err
		}
		// EVERY attempt conflicts: the competing commit lands inside every
		// transaction's read-then-write window.
		if _, cErr := competitor.Exec(`INSERT INTO tx_probe (id, v) VALUES (?, 'y')`, fmt.Sprintf("c%d", attempts)); cErr != nil {
			return cErr
		}
		_, execErr := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('writer', 'z')`, 5)
		return execErr
	})
	if err == nil {
		t.Fatal("expected the bounded restart to give up and return the error")
	}
	if !IsBusySnapshotError(err) {
		t.Fatalf("expected the snapshot conflict to surface, got %s", describeSQLErr(err))
	}
	if attempts != TxSnapshotMaxAttempts {
		t.Fatalf("expected the restart budget to be %d attempts, got %d", TxSnapshotMaxAttempts, attempts)
	}

	// Every abandoned attempt rolled back: no 'writer' row was persisted.
	var c int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM tx_probe WHERE id = 'writer'`).Scan(&c); err != nil {
		t.Fatalf("count writer rows: %v", err)
	}
	if c != 0 {
		t.Fatalf("an abandoned transaction must not persist its write; found %d rows", c)
	}
}

// TestWithTx_NonBusyCallbackErrorIsNotRetried asserts the restart is
// NARROW: only a stale snapshot triggers it. A caller's own error must be
// returned on the first attempt, unchanged.
func TestWithTx_NonBusyCallbackErrorIsNotRetried(t *testing.T) {
	dm, _ := newWALTestDM(t)

	attempts := 0
	sentinel := errors.New("caller error")
	err := dm.WithTx(func(node DBNode) error {
		attempts++
		if _, execErr := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('x', 'y')`, 0); execErr != nil {
			return execErr
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the caller's error to surface unchanged, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("a non-busy error must not be retried; attempts = %d", attempts)
	}

	// The write inside the failed transaction must have rolled back.
	var c int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM tx_probe WHERE id = 'x'`).Scan(&c); err != nil {
		t.Fatalf("count x rows: %v", err)
	}
	if c != 0 {
		t.Fatalf("the failed transaction must have rolled back; found %d rows", c)
	}
}

// TestWithTx_CancelledCallbackIsNotRetried asserts a cancelled callback is
// never restarted: re-running it would ignore the caller's cancellation.
func TestWithTx_CancelledCallbackIsNotRetried(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	attempts := 0
	err := dm.WithTx(func(node DBNode) error {
		attempts++
		var n int
		if err := node.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&n); err != nil {
			return err
		}
		if _, cErr := competitor.Exec(`INSERT INTO tx_probe (id, v) VALUES ('competitor', 'y')`); cErr != nil {
			return cErr
		}
		_, execErr := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('writer', 'z')`, 5)
		if execErr == nil {
			return nil
		}
		// A real snapshot conflict AND a cancellation: the cancellation
		// must win.
		return fmt.Errorf("%w: %w", context.Canceled, execErr)
	})
	if err == nil {
		t.Fatal("expected the cancelled callback's error to surface")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled to be preserved, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("a cancelled callback must not be retried; attempts = %d", attempts)
	}
}

// TestWithTx_CallbackErrorRollsBackEverything asserts the transaction's
// atomicity survives the restart machinery: a write followed by an error
// leaves nothing behind.
func TestWithTx_CallbackErrorRollsBackEverything(t *testing.T) {
	dm, _ := newWALTestDM(t)

	err := dm.WithTx(func(node DBNode) error {
		if _, execErr := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('first', '1')`, 0); execErr != nil {
			return execErr
		}
		if _, execErr := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('second', '2')`, 0); execErr != nil {
			return execErr
		}
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected the callback's error to surface")
	}
	var c int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM tx_probe WHERE id IN ('first','second')`).Scan(&c); err != nil {
		t.Fatalf("count: %v", err)
	}
	if c != 0 {
		t.Fatalf("a failed transaction must persist nothing; found %d rows", c)
	}
}

// TestWithTx_PanicRollsBackAndReraises asserts the panic contract survives
// the restart machinery: the transaction is rolled back and the panic is
// re-raised for the caller to recover.
func TestWithTx_PanicRollsBackAndReraises(t *testing.T) {
	dm, _ := newWALTestDM(t)

	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("the panic was swallowed")
			}
		}()
		_ = dm.WithTx(func(node DBNode) error {
			if _, execErr := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('panicked', 'p')`, 0); execErr != nil {
				return execErr
			}
			panic("kaboom")
		})
	}()

	var c int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM tx_probe WHERE id = 'panicked'`).Scan(&c); err != nil {
		t.Fatalf("count: %v", err)
	}
	if c != 0 {
		t.Fatalf("a panicking transaction must roll back; found %d rows", c)
	}
}

// TestWithTx_ConcurrentIdenticalWritesConvergeOnOneRow asserts the
// deduplication invariant under contention: two writers that race on the
// same content must produce exactly ONE row, and both must report the same
// id.
//
// The two writers are released together, so both take their read snapshot
// before either writes. That is the race the dedup probe exists for — and
// the race that produced the live SQLITE_BUSY_SNAPSHOT failures.
func TestWithTx_ConcurrentIdenticalWritesConvergeOnOneRow(t *testing.T) {
	dm, _ := newWALTestDM(t)

	const writers = 2
	barrier := make(chan struct{})
	// ready counts the writers that have taken their read snapshot. The
	// test waits for all of them before releasing the barrier, so both
	// writers hold a snapshot before either writes — the race the dedup
	// probe exists for.
	var ready sync.WaitGroup
	ready.Add(writers)
	var wg sync.WaitGroup
	ids := make([]string, writers)
	errs := make([]error, writers)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			attempts := 0
			errs[n] = dm.WithTx(func(node DBNode) error {
				attempts++
				// Establish this transaction's read snapshot FIRST — the
				// same thing the production dedup probe does.
				var n2 int
				if err := node.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&n2); err != nil {
					return err
				}
				if attempts == 1 {
					ready.Done()
					<-barrier
				}
				id, err := dm.SaveMemoryNode(node, "memories", "identical content for the dedup race", "", nil, nil, nil, false, 1, "", "0.5", "0.5", "")
				ids[n] = id
				return err
			})
		}(i)
	}

	// Release both writers once they are all inside their transaction.
	ready.Wait()
	close(barrier)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d failed: %v", i, err)
		}
	}
	if ids[0] == "" || ids[1] == "" {
		t.Fatalf("both writers must report an id; got %q and %q", ids[0], ids[1])
	}
	if ids[0] != ids[1] {
		t.Fatalf("both writers must converge on the same id; got %q and %q", ids[0], ids[1])
	}

	var c int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE content = ?`, "identical content for the dedup race").Scan(&c); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if c != 1 {
		t.Fatalf("expected exactly one row for the identical content, got %d", c)
	}
}

// TestProductionWritePrimitivesSurviveAStaleSnapshot exercises the REAL
// production write primitives — decision, lesson, memory, theory and
// evidence — against a deterministically-produced stale snapshot.
//
// THE RECIPE
// ----------
// Each case runs the production write primitive INSIDE a transaction whose
// read snapshot this test deliberately invalidates:
//
//	writer:  BEGIN (deferred)
//	writer:  READ                       <- the snapshot is taken here
//	test:    competing writer COMMITS    <- the WAL advances
//	writer:  WRITE                      <- SQLITE_BUSY_SNAPSHOT
//
// The competing commit happens inside the callback, immediately after the
// writer's read, so the ordering is guaranteed rather than timing-dependent.
// It is the same interleaving the live failure took: `mpm_decisions record`
// read (the dedup probe), then wrote (the INSERT into memories).
//
// The callback is replayed when the transaction restarts, so each case's
// write must be replay-safe — which is exactly the property the production
// primitives have: they generate their id inside the write, so the abandoned
// attempt leaves nothing behind.
func TestProductionWritePrimitivesSurviveAStaleSnapshot(t *testing.T) {
	const content = "a fact recorded while the snapshot went stale"

	cases := []struct {
		name string
		// write performs the production write primitive on the transaction.
		write func(t *testing.T, dm *DatabaseManager, node DBNode) error
		// count counts the rows the write should have produced.
		count func(t *testing.T, dm *DatabaseManager) int
		// setup prepares whatever the write primitive needs to exist
		// beforehand. It runs BEFORE the transaction, so it cannot
		// interfere with the snapshot.
		setup func(t *testing.T, dm *DatabaseManager)
		// transactional records whether the PRODUCTION API for this write
		// type runs inside a WithTx transaction. The non-transactional ones
		// cannot be bitten by the stale-snapshot defect at all, but their
		// primitive is still verified here because other callers do run it
		// transactionally.
		transactional bool
	}{
		{
			name: "decision",
			write: func(t *testing.T, dm *DatabaseManager, node DBNode) error {
				meta := map[string]interface{}{"choice": "c", "rationale": "r"}
				id, err := dm.SaveMemoryNodeWithInvocation(node, "decisions", content, "", []string{"t"}, meta, nil, false, 1, "", "0.5", "0.5", "", ActiveContext{})
				if err != nil {
					return err
				}
				return dm.recordSourceCitationsNode(node, nil, id, "decision")
			},
			count: func(t *testing.T, dm *DatabaseManager) int {
				return countProbeRows(t, dm, `SELECT COUNT(*) FROM memories WHERE collection = 'decisions'`)
			},
			transactional: true,
		},
		{
			name: "theory",
			write: func(t *testing.T, dm *DatabaseManager, node DBNode) error {
				meta := map[string]interface{}{"hypothesis": "h", "validation_criteria": "c"}
				id, err := dm.SaveMemoryNodeWithInvocation(node, "theories", content, "", []string{"t"}, meta, nil, false, 1, "", "0.5", "0.5", "", ActiveContext{})
				if err != nil {
					return err
				}
				_, err = node.ExecTracked(`UPDATE memories SET dependencies = ? WHERE id = ? AND collection = 'theories'`, 0, `["dep"]`, id)
				return err
			},
			count: func(t *testing.T, dm *DatabaseManager) int {
				return countProbeRows(t, dm, `SELECT COUNT(*) FROM memories WHERE collection = 'theories'`)
			},
			transactional: true,
		},
		{
			name: "memory",
			write: func(t *testing.T, dm *DatabaseManager, node DBNode) error {
				_, err := dm.SaveMemoryNode(node, "memories", content, "", []string{"t"}, nil, nil, false, 1, "", "0.5", "0.5", "")
				return err
			},
			count: func(t *testing.T, dm *DatabaseManager) int {
				return countProbeRows(t, dm, `SELECT COUNT(*) FROM memories WHERE collection = 'memories'`)
			},
			transactional: false,
		},
		{
			name: "lesson",
			write: func(t *testing.T, dm *DatabaseManager, node DBNode) error {
				_, err := dm.SaveMemoryNode(node, "lessons", content, "", []string{"t"}, nil, nil, false, 1, "", "0.5", "0.5", "")
				return err
			},
			count: func(t *testing.T, dm *DatabaseManager) int {
				return countProbeRows(t, dm, `SELECT COUNT(*) FROM memories WHERE collection = 'lessons'`)
			},
			transactional: false,
		},
		{
			name: "evidence",
			write: func(t *testing.T, dm *DatabaseManager, node DBNode) error {
				in := EvidenceInput{
					ArtifactID:         "artifact-under-contention",
					ArtifactType:       "memory",
					Type:               "observation",
					SourceGroup:        "group-1",
					CreatedBy:          "tester",
					IndependenceFactor: 1.0,
				}
				_, err := addEvidenceInTx(node, in, nil)
				return err
			},
			count: func(t *testing.T, dm *DatabaseManager) int {
				return countProbeRows(t, dm, `SELECT COUNT(*) FROM evidence`)
			},
			setup: func(t *testing.T, dm *DatabaseManager) {
				// The evidence write recomputes the artifact's confidence,
				// so the artifact must exist.
				if _, err := dm.db.Exec(`INSERT INTO memories (id, collection, content, created_at) VALUES ('artifact-under-contention', 'memories', 'the artifact', ?)`,
					time.Now().Unix()); err != nil {
					t.Fatalf("seed artifact: %v", err)
				}
			},
			transactional: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dm, competitor := newWALTestDM(t)
			if tc.setup != nil {
				tc.setup(t, dm)
			}

			attempts := 0
			err := dm.WithTx(func(node DBNode) error {
				attempts++
				// The READ that establishes this transaction's snapshot.
				var n int
				if err := node.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&n); err != nil {
					return err
				}
				if attempts == 1 {
					// The competing commit lands between this transaction's
					// read and its write.
					if _, cErr := competitor.Exec(`INSERT INTO tx_probe (id, v) VALUES ('competitor', 'y')`); cErr != nil {
						return cErr
					}
				}
				return tc.write(t, dm, node)
			})
			if err != nil {
				t.Fatalf("%s write failed under a stale snapshot: %v", tc.name, err)
			}
			if attempts != 2 {
				t.Fatalf("%s: expected the transaction to be restarted once (2 attempts), got %d", tc.name, attempts)
			}
			if n := tc.count(t, dm); n != 1 {
				t.Fatalf("%s: expected exactly one row, got %d", tc.name, n)
			}
			// The abandoned first attempt must have left nothing behind.
			if n := countProbeRows(t, dm, `SELECT COUNT(*) FROM tx_probe WHERE id = 'competitor'`); n != 1 {
				t.Fatalf("%s: the competitor's row must survive, got %d", tc.name, n)
			}
		})
	}
}

// TestNonTransactionalWritePathsAreNotSnapshotVulnerable records WHICH
// production APIs can be bitten by the stale-snapshot defect at all.
//
// A write that does not run inside a transaction takes no read snapshot, so
// it cannot hit SQLITE_BUSY_SNAPSHOT — it is immune to the #158 defect by
// construction. This test pins that classification so a future change that
// moves one of these paths inside a transaction (or out of one) is a
// deliberate act rather than an accident.
//
// The lesson path carries a SEPARATE weakness — it is a bare, unretried
// `db.Exec`, so ordinary lock contention is fatal to it. That is a
// different defect class from #158 and is reported separately rather than
// fixed here; see the PR's "adjacent findings" section.
func TestNonTransactionalWritePathsAreNotSnapshotVulnerable(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	// A competing writer commits while the non-transactional path is
	// between its dedup read and its write.
	release := holdWALWriteLock(t, competitor)

	writeErr := make(chan error, 1)
	go func() {
		_, _, err := dm.SaveMemoryWithContext("a fact recorded under contention", "memories", []string{"t"}, 1, "", ActiveContext{})
		writeErr <- err
	}()

	// The memory path's INSERT conflicts with the holder's lock and is
	// retried in place by the standalone ExecTracked. Release the holder
	// once the retry counter has moved, so the retry finds the lock free.
	waitFor(t, 15*time.Second, "the writer to hit the write lock", func() bool {
		return dm.BusyRetryCount() > 0
	})
	release()

	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("the non-transactional memory write must survive ordinary contention: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the non-transactional memory write did not complete")
	}

	if n := countProbeRows(t, dm, `SELECT COUNT(*) FROM memories WHERE collection = 'memories'`); n != 1 {
		t.Fatalf("expected exactly one memory row, got %d", n)
	}
}

// countProbeRows runs a single-row COUNT query against the manager.
func countProbeRows(t *testing.T, dm *DatabaseManager, query string) int {
	t.Helper()
	var n int
	if err := dm.db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// TestWatchdogRecordsASnapshotConflictAsContentionNotFault asserts the
// observability half of the fix.
//
// Before it, the two production snapshot conflicts were recorded in
// watchdog.jsonl as `reason=sql_error, level=error` with a 3.1 s duration —
// which is exactly what made them read as substrate faults in the live-runtime
// audit. They are contention events that the transaction layer recovers
// from, so they must be recorded as such.
func TestWatchdogRecordsASnapshotConflictAsContentionNotFault(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	// The callback is replayed when the transaction is restarted, so it must
	// be replay-safe: the id is derived from the attempt number, and the
	// abandoned attempt's row is rolled back with its transaction.
	attempts := 0
	err := dm.WithTx(func(node DBNode) error {
		attempts++
		var n int
		if err := node.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&n); err != nil {
			return err
		}
		if attempts == 1 {
			if _, cErr := competitor.Exec(`INSERT INTO tx_probe (id, v) VALUES ('competitor', 'y')`); cErr != nil {
				return cErr
			}
		}
		_, execErr := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES (?, 'z')`, 5, fmt.Sprintf("writer-%d", attempts))
		return execErr
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected the transaction to be restarted once, got %d attempts", attempts)
	}

	rows := readWatchdogRecords(t, dm)
	var snapshot, sqlErr int
	for _, rec := range rows {
		switch rec["reason"] {
		case watchdogReasonBusySnapshot:
			snapshot++
		case watchdogReasonError:
			sqlErr++
		}
	}
	if snapshot == 0 {
		t.Fatalf("expected a %q watchdog record for the snapshot conflict; got %d records: %v", watchdogReasonBusySnapshot, len(rows), rows)
	}
	if sqlErr != 0 {
		t.Fatalf("a recovered snapshot conflict must not be recorded as %q; got %d", watchdogReasonError, sqlErr)
	}
	for _, rec := range rows {
		if rec["reason"] == watchdogReasonBusySnapshot {
			if rec["level"] != watchdogLevelWarn {
				t.Fatalf("a snapshot conflict is contention, not a fault: level = %v, want %v", rec["level"], watchdogLevelWarn)
			}
		}
	}
}

// readWatchdogRecords reads the manager's watchdog.jsonl and returns the
// decoded records.
func readWatchdogRecords(t *testing.T, dm *DatabaseManager) []map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(dm.watchdogPath)
	if err != nil {
		t.Fatalf("read watchdog %s: %v", dm.watchdogPath, err)
	}
	var out []map[string]interface{}
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) == 0 {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode watchdog line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}
