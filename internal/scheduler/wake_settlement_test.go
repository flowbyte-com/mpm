package scheduler

// wake_settlement_test.go — issue #178's scheduler correction.
//
// WHAT THIS FILE PROVES
// ---------------------
// The settlement of a system wake is the only record that the wake was
// DELIVERED. It used to be a bare pool write, so an ordinary lock
// contention lost it — and a lost settlement is not a lost artifact:
// the wake stays `fired = 0`, the due-wake query selects it again, and
// the handler RUNS A SECOND TIME.
//
// The tests below are in three groups:
//
//   1. THE REPRODUCTION and its REGRESSION, on a real WAL database with
//      the production connection settings: a bare settlement loses the
//      wake under contention, and the corrected settlement survives the
//      same contention.
//   2. THE DELIVERY SEMANTICS, per failure point: before execution,
//      during execution, after a successful execution, and during the
//      settlement.
//   3. THE RESIDUAL, characterised rather than hidden: a crash between
//      the handler completing and the settlement landing still
//      re-executes the wake, and this file says so in a test so the
//      residual cannot be forgotten.
//
// The database is the scheduler's own test harness: a file-backed WAL
// database with `_busy_timeout=5000`, which is the production DSN's
// contention behaviour.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// settleProbeSchema is a table the contention harness writes to. It
// exists so holding the write lock does not have to touch
// `scheduled_wakes` — the rows under test must be exactly the ones the
// test seeded.
const settleProbeSchema = `
CREATE TABLE IF NOT EXISTS settle_probe (id TEXT PRIMARY KEY, v TEXT NOT NULL);
`

// holdWALWriteLock opens a SECOND connection to the scheduler's database
// and holds the WAL write lock in an open transaction, so the
// scheduler's own writes meet ordinary lock contention.
//
// The competitor uses the production connection settings too, so the
// contention it produces is the contention production produces.
func holdWALWriteLock(t *testing.T, s *Scheduler, holdFor time.Duration) (release func()) {
	t.Helper()

	if _, err := s.db.Exec(settleProbeSchema); err != nil {
		t.Fatalf("create the contention probe table: %v", err)
	}

	competitor, err := sql.Open("sqlite3", s.dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open the competitor connection: %v", err)
	}
	t.Cleanup(func() { _ = competitor.Close() })

	tx, err := competitor.Begin()
	if err != nil {
		t.Fatalf("competitor begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO settle_probe (id, v) VALUES ('holder', 'h')`); err != nil {
		t.Fatalf("competitor insert: %v", err)
	}

	var released atomic.Bool
	release = func() {
		if released.Swap(true) {
			return
		}
		if err := tx.Commit(); err != nil {
			t.Errorf("competitor commit: %v", err)
		}
	}
	if holdFor > 0 {
		go func() {
			time.Sleep(holdFor)
			release()
		}()
	}
	t.Cleanup(release)
	return release
}

// wakeFiredState reads the delivery state of a wake.
func wakeFiredState(t *testing.T, s *Scheduler, id string) (fired int, lastError string) {
	t.Helper()
	if err := s.db.QueryRow(
		`SELECT fired, COALESCE(json_extract(metadata, '$.last_error'), '') FROM scheduled_wakes WHERE id = ?`,
		id,
	).Scan(&fired, &lastError); err != nil {
		t.Fatalf("read back wake %s: %v", id, err)
	}
	return fired, lastError
}

// TestWakeSettlement_ABareWriteLosesTheWakeAndTheGuardedOneSurvives is
// the reproduction AND its regression, under the SAME contention.
//
// Two settlements are started against the same wake, at the same moment,
// with the WAL write lock held by a competitor:
//
//   - the BARE write — the shape the scheduler used before this change:
//     a pool UPDATE with no statement-level retry budget;
//   - the GUARDED write — the corrected shape: the same UPDATE, carried
//     by the substrate's statement-level retry budget.
//
// The lock is held for LONGER than the production busy timeout (5000ms),
// so a single attempt cannot survive it — a retry that never had to
// retry proves nothing. The bare write therefore fails, and the guarded
// write has to RETRY to land.
//
// The bare write's failure is the reproduction: the wake stays
// `fired = 0`, so the due-wake query selects it again and the handler
// would run a second time.
func TestWakeSettlement_ABareWriteLosesTheWakeAndTheGuardedOneSurvives(t *testing.T) {
	s := newTestScheduler(t)
	seedWake(t, s, "wk-settle", time.Now().Add(-time.Hour), "gc")

	// The lock is held for longer than the production busy timeout, so
	// the first attempt of BOTH writes expires before the lock is
	// released.
	const hold = 6 * time.Second
	release := holdWALWriteLock(t, s, hold)
	defer release()

	// The guarded settlement runs concurrently with the bare one, so
	// both meet the same contention from the same starting point.
	guarded := make(chan error, 1)
	go func() {
		guarded <- s.MarkFired("wk-settle", "")
	}()

	// ── The bare shape: the previous implementation ──────────────
	_, bareErr := s.db.Exec(
		`UPDATE scheduled_wakes SET fired = 1, fired_at = ? WHERE id = ?`,
		time.Now().Unix(), "wk-settle",
	)
	if bareErr == nil {
		t.Fatalf("the bare settlement must fail under contention that outlasts the busy timeout; " +
			"it succeeded, so this test is not exercising the defect")
	}
	if !core.IsBusyError(bareErr) {
		t.Fatalf("expected an ordinary busy error from the bare settlement, got: %v", bareErr)
	}

	// ── The guarded shape: the corrected implementation ───────────
	select {
	case err := <-guarded:
		if err != nil {
			t.Fatalf("the guarded settlement must survive the contention the bare write lost to: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the guarded settlement never completed")
	}

	// The artifact of the defect, and the proof the fix removed it:
	// the wake is TERMINAL, so the due-wake query does not select it
	// again and the handler does not run a second time.
	fired, _ := wakeFiredState(t, s, "wk-settle")
	if fired != 1 {
		t.Fatalf("the guarded settlement must land; fired=%d", fired)
	}
	due, err := s.QueryDueWakes(time.Now())
	if err != nil {
		t.Fatalf("query due wakes: %v", err)
	}
	if containsWake(due, "wk-settle") {
		t.Fatalf("a settled wake must not be due again: the handler would run twice")
	}
}

// containsWake reports whether the wake list holds the given id.
func containsWake(wakes []Wake, id string) bool {
	for _, w := range wakes {
		if w.ID == id {
			return true
		}
	}
	return false
}

// TestWakeDelivery_AFailedHandlerSettlesTheWakeAsTerminal covers the
// failure DURING execution.
//
// The scheduler's contract is "failures surface but do not block other
// wakes": a handler that fails is recorded as processed-and-failed. The
// wake is settled WITH the error, so it does not run again.
//
// This is at-most-once for a failed handler — and it is the behaviour
// the settlement must preserve. A settlement that recorded the failure
// badly (or not at all) would turn a failed handler into an infinite
// retry loop.
func TestWakeDelivery_AFailedHandlerSettlesTheWakeAsTerminal(t *testing.T) {
	s := newTestScheduler(t)
	seedWake(t, s, "wk-failed", time.Now().Add(-time.Hour), "gc")

	s.Register("gc", func(ctx context.Context, w Wake) error {
		return errors.New("the handler failed")
	})

	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	fired, lastError := wakeFiredState(t, s, "wk-failed")
	if fired != 1 {
		t.Fatalf("a failed handler must still settle the wake, or the handler retries forever; fired=%d", fired)
	}
	if !strings.Contains(lastError, "the handler failed") {
		t.Errorf("the failure must be recorded on the wake; last_error=%q", lastError)
	}
	if due, err := s.QueryDueWakes(time.Now()); err != nil {
		t.Fatalf("query due wakes: %v", err)
	} else if containsWake(due, "wk-failed") {
		t.Fatalf("a failed-but-settled wake must not be due again")
	}
}

// TestWakeDelivery_AnAmbiguousCommitStillSettlesTheWakeAsTerminal
// covers the AMBIGUOUS COMMIT outcome.
//
// A handler whose own transaction commit failed ambiguously (the
// substrate marks that `core.ErrTxCommitAmbiguous` — the write may or
// may not be durable) reports an error. The scheduler's job is not to
// resolve the ambiguity — it cannot — but to make sure the WAKE is
// terminal, so the ambiguity is not resolved by running the handler
// again and doubling whatever the handler wrote.
//
// The wake is settled with the ambiguity recorded. The artifact inside
// the handler stays ambiguous, which is the honest outcome; the WAKE is
// not re-executed, which is the part the scheduler owns.
func TestWakeDelivery_AnAmbiguousCommitStillSettlesTheWakeAsTerminal(t *testing.T) {
	s := newTestScheduler(t)
	seedWake(t, s, "wk-ambiguous", time.Now().Add(-time.Hour), "gc")

	s.Register("gc", func(ctx context.Context, w Wake) error {
		return fmt.Errorf("the handler's commit was ambiguous: %w", core.ErrTxCommitAmbiguous)
	})

	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	fired, lastError := wakeFiredState(t, s, "wk-ambiguous")
	if fired != 1 {
		t.Fatalf("an ambiguous commit must still settle the wake, or the handler runs again and "+
			"may double its write; fired=%d", fired)
	}
	if !strings.Contains(lastError, "ambiguous") {
		t.Errorf("the ambiguity must be recorded on the wake; last_error=%q", lastError)
	}
	if due, err := s.QueryDueWakes(time.Now()); err != nil {
		t.Fatalf("query due wakes: %v", err)
	} else if containsWake(due, "wk-ambiguous") {
		t.Fatalf("an ambiguously-committed wake must not be due again")
	}
}

// TestWakeDelivery_ACancelledHandlerSettlesTheWakeAsTerminal covers
// CANCELLATION.
//
// `executeOne` returns when the context is cancelled, even though the
// handler goroutine may still be unwinding. The wake is settled with
// the cancellation recorded, so the cancellation does not become a
// re-execution on the next tick.
func TestWakeDelivery_ACancelledHandlerSettlesTheWakeAsTerminal(t *testing.T) {
	s := newTestScheduler(t)
	seedWake(t, s, "wk-cancelled", time.Now().Add(-time.Hour), "gc")

	started := make(chan struct{})
	s.Register("gc", func(ctx context.Context, w Wake) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	defer cancel()

	if _, err := s.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	fired, lastError := wakeFiredState(t, s, "wk-cancelled")
	if fired != 1 {
		t.Fatalf("a cancelled handler must still settle the wake, or the cancellation becomes a "+
			"re-execution; fired=%d", fired)
	}
	if !strings.Contains(strings.ToLower(lastError), "cancel") {
		t.Errorf("the cancellation must be recorded on the wake; last_error=%q", lastError)
	}
}

// TestWakeDelivery_ASettlementFailureIsReportedAndLeavesTheWakePending
// covers the failure DURING THE SETTLEMENT.
//
// The settlement is blocked deterministically — a trigger that refuses
// the UPDATE — so this is a settlement failure that is NOT contention.
// The wake stays pending, which means the handler WILL run again; and
// that is reported by Tick, in the same place every other tick failure
// is reported, rather than being logged and forgotten.
func TestWakeDelivery_ASettlementFailureIsReportedAndLeavesTheWakePending(t *testing.T) {
	s := newTestScheduler(t)
	seedWake(t, s, "wk-settle-fails", time.Now().Add(-time.Hour), "gc")

	if _, err := s.db.Exec(`
		CREATE TRIGGER block_settlement BEFORE UPDATE ON scheduled_wakes
		BEGIN SELECT RAISE(ABORT, 'settlement blocked'); END;
	`); err != nil {
		t.Fatalf("install the settlement blocker: %v", err)
	}

	var ran atomic.Int32
	s.Register("gc", func(ctx context.Context, w Wake) error {
		ran.Add(1)
		return nil
	})

	_, err := s.Tick(context.Background())
	if err == nil {
		t.Fatalf("a failed settlement must be reported by Tick: the wake is still pending and its " +
			"handler will run again, and that must not be a silent condition")
	}
	if !strings.Contains(err.Error(), "settlement") {
		t.Errorf("the reported error must name the settlement failure; got: %v", err)
	}

	fired, _ := wakeFiredState(t, s, "wk-settle-fails")
	if fired != 0 {
		t.Fatalf("a failed settlement must leave the wake pending; fired=%d", fired)
	}
	if got := ran.Load(); got != 1 {
		t.Fatalf("the handler must have run exactly once in this tick; ran=%d", got)
	}
}

// TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakePending is
// the DOCUMENTED RESIDUAL.
//
// This is the one duplicate-execution path this change does NOT close,
// and it is pinned here so it cannot be forgotten:
//
//   - the handler runs;
//   - the process dies before the settlement lands;
//   - the next process finds the wake still `fired = 0` and runs the
//     handler AGAIN.
//
// No retry budget can close it: the settlement is not what failed — the
// PROCESS died. Closing it needs a delivery-state redesign (a durable
// claim with a lease, plus handlers that are idempotent or carry their
// own deduplication), which is recorded in
// docs/dev/issue-178-corrections-phase-1-2026-10-29.md and is
// explicitly NOT claimed here.
//
// If a future change closes this window, THIS TEST FAILS, and that is
// the point: the residual must be removed deliberately, not by accident.
func TestWakeDelivery_ARestartBeforeSettlementLeavesTheWakePending(t *testing.T) {
	s := newTestScheduler(t)
	seedWake(t, s, "wk-crash", time.Now().Add(-time.Hour), "gc")

	var ran atomic.Int32
	s.Register("gc", func(ctx context.Context, w Wake) error {
		ran.Add(1)
		return nil
	})

	// The handler runs — and then the process "dies": no settlement.
	w := Wake{ID: "wk-crash", Metadata: map[string]interface{}{"kind": "gc"}}
	if err := s.executeOne(context.Background(), w); err != nil {
		t.Fatalf("execute the handler: %v", err)
	}
	if ran.Load() != 1 {
		t.Fatalf("the handler must have run; ran=%d", ran.Load())
	}

	fired, _ := wakeFiredState(t, s, "wk-crash")
	if fired != 0 {
		t.Fatalf("without a settlement the wake must still be pending; fired=%d", fired)
	}
	due, err := s.QueryDueWakes(time.Now())
	if err != nil {
		t.Fatalf("query due wakes: %v", err)
	}
	if !containsWake(due, "wk-crash") {
		t.Fatalf("the wake must still be due: this is the residual duplicate-execution path, and " +
			"this test exists to keep it visible")
	}
}

// TestWakeDelivery_TheSchedulerIsStillASingleton is the compatibility
// check for the singleton invariant the duplicate-execution analysis
// rests on.
//
// The delivery semantics here are "at-least-once across a crash" — not
// "exactly-once" — because a crash between the handler and the
// settlement re-executes the wake. That is only as safe as the
// singleton: with one scheduler, the re-execution is a retry of the
// same wake by the same host, not a concurrent second execution.
//
// The singleton is enforced by flock on a PID file. This test asserts
// the lock is still exclusive, because if it stopped being exclusive
// the delivery semantics would be worse than the residual above.
func TestWakeDelivery_TheSchedulerIsStillASingleton(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/scheduler.lock"

	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("acquire the singleton lock: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := AcquireLock(path)
	if err == nil {
		_ = second.Close()
		t.Fatalf("a second scheduler must not acquire the singleton lock while the first holds it")
	}
}
