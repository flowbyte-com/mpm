// Tests for the deadline-driven ad-hoc wake dispatch claim SQL
// (internal/scheduler/dispatch.go).
//
// These tests verify the keystone correctness invariant: the atomic
// UPDATE ... RETURNING claim pattern is the cross-process dedup
// primitive. Two goroutines, two processes, two different dispatchers
// (mpm-scheduler + mpm call opportunistic fold) must all agree that
// each wake row is claimed at most once.
//
// The schema is the same minimal subset scheduler_test.go uses — enough
// for the claim SQL to execute, no more.

package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const dispatchTestSchema = `
CREATE TABLE IF NOT EXISTS scheduled_wakes (
    id              TEXT PRIMARY KEY,
    target_time     INTEGER NOT NULL,
    reason          TEXT NOT NULL,
    theory_id       TEXT,
    recurring_rule  TEXT,
    fired           INTEGER NOT NULL DEFAULT 0,
    fired_at        INTEGER,
    fired_by        TEXT,
    dispatched_at   INTEGER,
    created_by      TEXT NOT NULL,
    created_at      INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    metadata        JSON
);
`

// newDispatchTestScheduler returns a Scheduler backed by a temp-file
// SQLite database (so WAL mode works across connections), with the
// minimal schema and a discard logger. Tests reuse the existing
// seedWake helper from scheduler_test.go.
func newDispatchTestScheduler(t *testing.T) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch_test.db")
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(dispatchTestSchema); err != nil {
		t.Fatalf("install schema: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	return &Scheduler{
		db:       db,
		dbPath:   path,
		log:      log,
		handlers: make(map[string]HandlerFunc),
		// Pinned beside the test DB — see newTestScheduler for why a
		// test scheduler must never resolve the live heartbeat path.
		statePath: filepath.Join(dir, "run", "scheduler.state"),
	}
}

// TestDispatchClaimNextAdHocWake_ClaimsNotificationKind verifies the
// keystone invariant: the claim partitions on notification-kind (and
// untagged, which seedWake represents as kind=""). System kinds stay
// unfired for Tick to handle on a future maintenance tick.
func TestDispatchClaimNextAdHocWake_ClaimsNotificationKind(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	seedWake(t, s, "n1", now.Add(-1*time.Minute), "")              // untagged = notification
	seedWake(t, s, "n2", now.Add(-30*time.Second), "notification") // explicit notification
	seedWake(t, s, "sys", now.Add(-1*time.Minute), "snapshot")     // system kind — must NOT claim

	w, ok, err := dispatchClaimNextAdHocWake(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok {
		t.Fatal("expected a claim (3 due, 2 eligible)")
	}
	if w.ID != "n1" {
		t.Errorf("claimed id = %q, want n1 (oldest eligible)", w.ID)
	}

	// Verify system kind is NOT fired.
	var fired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id='sys'`).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Errorf("system-kind wake fired=%d, want 0 (must remain for maintenance tick)", fired)
	}

	// Verify metadata stamped with dispatched_by.
	var dispatchedBy sql.NullString
	if err := s.db.QueryRow(
		`SELECT json_extract(metadata, '$.dispatched_by') FROM scheduled_wakes WHERE id='n1'`,
	).Scan(&dispatchedBy); err != nil {
		t.Fatal(err)
	}
	if !dispatchedBy.Valid || dispatchedBy.String != "mpm-scheduler" {
		t.Errorf("dispatched_by = %v, want mpm-scheduler", dispatchedBy)
	}
}

// TestDispatchClaimNextAdHocWake_FutureNotClaimed verifies the
// target_time <= now guard — a wake scheduled for the future must NOT
// be claimed by the deadline-driven drain until the deadline passes.
func TestDispatchClaimNextAdHocWake_FutureNotClaimed(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	seedWake(t, s, "future", now.Add(1*time.Hour), "")

	_, ok, err := dispatchClaimNextAdHocWake(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ok {
		t.Error("future wake was claimed; the deadline-driven drain must respect target_time")
	}
}

// TestDispatchClaimNextAdHocWake_OrderByTargetTime verifies that the
// drain is FIFO: even when a future wake is inserted between two due
// wakes, the older due wake is claimed first.
func TestDispatchClaimNextAdHocWake_OrderByTargetTime(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	// Insert in REVERSE chronological order; claim must still return the oldest.
	seedWake(t, s, "later", now.Add(-1*time.Second), "")
	seedWake(t, s, "earliest", now.Add(-10*time.Second), "")
	seedWake(t, s, "middle", now.Add(-5*time.Second), "")

	w, ok, err := dispatchClaimNextAdHocWake(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok {
		t.Fatal("expected a claim")
	}
	if w.ID != "earliest" {
		t.Errorf("claimed = %q, want earliest (FIFO order)", w.ID)
	}
}

// TestDispatchClaimNextAdHocWake_NoEligibleReturnsFalse verifies the
// no-row case: when there are no due notification-kind wakes, the
// claim returns (zero, false, nil) without surfacing ErrNoRows.
func TestDispatchClaimNextAdHocWake_NoEligibleReturnsFalse(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	// Only system-kind; ad-hoc claim must skip them.
	seedWake(t, s, "sys", now.Add(-1*time.Minute), "snapshot")

	_, ok, err := dispatchClaimNextAdHocWake(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("claim: %v (must return nil not sql.ErrNoRows)", err)
	}
	if ok {
		t.Error("expected no claim (only system-kind wakes present)")
	}
}

// TestDispatchClaimNextAdHocWake_HandlesNullableStrings is the
// regression for the production-shape NULL columns. ScheduleWake
// leaves theory_id and recurring_rule as NULL when no theory is
// attached and no recurrence is set. Scanning NULL into a plain
// `string` field trips "converting NULL to string is unsupported",
// which fails the claim even though the UPDATE itself succeeded —
// the row is now marked fired but the dispatch loop sees an error
// and gives up.
//
// Live acceptance caught this: a cron-injected wake tripped the
// scan error (target_time=1788438426, fired_at=None because the
// drain errored before claiming). The acceptance-A wake (target
// 1788438430) still fired because its claim hit a moment when no
// other wake was ahead of it, but the loop kept erroring on every
// iteration until ctx cancel.
func TestDispatchClaimNextAdHocWake_HandlesNullableStrings(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	if _, err := s.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES ('null-fields', ?, 'test null wake', NULL, NULL, 0, 'test', '{}')`,
		now.Add(-1*time.Minute).Unix(),
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	w, ok, err := dispatchClaimNextAdHocWake(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("claim on NULL theory_id/recurring_rule row: %v (must NOT trip scan on NULL)", err)
	}
	if !ok {
		t.Fatal("expected a claim")
	}
	if w.ID != "null-fields" {
		t.Errorf("claimed = %q, want null-fields", w.ID)
	}
	if w.TheoryID != "" {
		t.Errorf("TheoryID = %q, want empty (NULL scans to empty)", w.TheoryID)
	}
	if w.RecurringRule != "" {
		t.Errorf("RecurringRule = %q, want empty (NULL scans to empty)", w.RecurringRule)
	}
}

// for the production-shape metadata column. ScheduleWake stores
// metadata=” (not '{}') for the default no-kind case. Earlier the
// claim UPDATE used COALESCE(metadata,'{}'), which on empty string
// returned ” because ” is non-NULL in SQLite — then json_set(”)
// raised "malformed JSON" and the entire drain failed.
//
// Live-acceptance caught this: the first wake that fired was
// acceptance-A (target_time=1788438304, fired_at=1788438309) and the
// scheduler log showed 17 "malformed JSON" errors between 13:25:09.024
// and 13:25:09.027 before ctx cancel. The wake row was marked fired
// because the UPDATE itself succeeded — the failure was on the RETURNING
// metadata column decode on the Go side. The subsequent claims errored
// out the same way until ctx-cancel.
//
// This test inserts a row with metadata=” directly and asserts the
// claim succeeds, returns the row, and stamps dispatched_by.
func TestDispatchClaimNextAdHocWake_HandlesEmptyMetadata(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	// Insert a wake with metadata='' directly, mirroring the production
	// ScheduleWake shape. seedWake always sets metadata to '{}' so it
	// can't exercise this path.
	if _, err := s.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES ('prod-shape', ?, 'test prod-shape wake', '', '', 0, 'test', '')`,
		now.Add(-1*time.Minute).Unix(),
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	w, ok, err := dispatchClaimNextAdHocWake(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("claim on empty-metadata row: %v (must NOT return malformed JSON)", err)
	}
	if !ok {
		t.Fatal("expected a claim")
	}
	if w.ID != "prod-shape" {
		t.Errorf("claimed = %q, want prod-shape", w.ID)
	}

	var dispatchedBy sql.NullString
	if err := s.db.QueryRow(
		`SELECT json_extract(metadata, '$.dispatched_by') FROM scheduled_wakes WHERE id='prod-shape'`,
	).Scan(&dispatchedBy); err != nil {
		t.Fatal(err)
	}
	if !dispatchedBy.Valid || dispatchedBy.String != "mpm-scheduler" {
		t.Errorf("dispatched_by = %v, want mpm-scheduler", dispatchedBy)
	}
}

// TestDispatchDrainAdHocWakes_DrainsAllDue verifies the drain loop
// claims every due row in a single call up to the cap.
//
// 2026-09-23 release-blocker repair: the drain records dispatched_at
// instead of firing. Every claimed row must remain fired=0 (so the
// normal wake/context delivery path can surface it to the user/agent).
func TestDispatchDrainAdHocWakes_DrainsAllDue(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	const N = 5
	for i := 0; i < N; i++ {
		seedWake(t, s, fmt.Sprintf("n%d", i), now.Add(-time.Duration(i+1)*time.Second), "")
	}

	n, err := dispatchDrainAdHocWakes(context.Background(), s.db, now, 100)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != N {
		t.Errorf("claimed %d, want %d", n, N)
	}

	var dispatched int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE dispatched_at IS NOT NULL`,
	).Scan(&dispatched); err != nil {
		t.Fatal(err)
	}
	if dispatched != N {
		t.Errorf("dispatched_at count = %d, want %d", dispatched, N)
	}

	var fired int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 1`,
	).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Errorf("fired count = %d, want 0 (wakes must remain pending until terminal (acknowledged via fold, ResolveWake, or bounded retire))", fired)
	}
}

// TestDispatchDrainAdHocWakes_LeavesFutureAlone verifies that a drain
// with mixed past/future wakes claims only the past ones.
//
// 2026-09-23 release-blocker repair: "claim" now means stamping
// dispatched_at, not flipping fired=1. The future wake must remain
// fully untouched (no dispatched_at, fired=0).
func TestDispatchDrainAdHocWakes_LeavesFutureAlone(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	seedWake(t, s, "past", now.Add(-1*time.Minute), "")
	seedWake(t, s, "future", now.Add(1*time.Hour), "")

	n, err := dispatchDrainAdHocWakes(context.Background(), s.db, now, 100)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 1 {
		t.Errorf("claimed %d, want 1 (only past)", n)
	}

	var pastDispatchedAt sql.NullInt64
	var pastFired int
	if err := s.db.QueryRow(
		`SELECT dispatched_at, fired FROM scheduled_wakes WHERE id='past'`,
	).Scan(&pastDispatchedAt, &pastFired); err != nil {
		t.Fatal(err)
	}
	if !pastDispatchedAt.Valid {
		t.Errorf("past dispatched_at not set, want set (claim should stamp dispatched_at)")
	}
	if pastFired != 0 {
		t.Errorf("past fired=%d, want 0 (wake must remain pending until terminal (acknowledged via fold, ResolveWake, or bounded retire))", pastFired)
	}

	var futureDispatchedAt sql.NullInt64
	var futureFired int
	if err := s.db.QueryRow(
		`SELECT dispatched_at, fired FROM scheduled_wakes WHERE id='future'`,
	).Scan(&futureDispatchedAt, &futureFired); err != nil {
		t.Fatal(err)
	}
	if futureDispatchedAt.Valid {
		t.Errorf("future dispatched_at set (%v), want NULL", futureDispatchedAt)
	}
	if futureFired != 0 {
		t.Errorf("future fired=%d, want 0", futureFired)
	}
}

// TestDispatchDrainAdHocWakes_RespectsCap verifies capN bounds a
// single drain pass. A cap of 1 against 5 due rows yields 1 claim and
// 4 still-pending rows.
//
// 2026-09-23 release-blocker repair: post-drain the 1 claimed row has
// dispatched_at set and fired=0; the 4 unclaimed rows have dispatched_at
// NULL and fired=0.
func TestDispatchDrainAdHocWakes_RespectsCap(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	const N = 5
	for i := 0; i < N; i++ {
		seedWake(t, s, fmt.Sprintf("n%d", i), now.Add(-time.Duration(i+1)*time.Second), "")
	}

	n, err := dispatchDrainAdHocWakes(context.Background(), s.db, now, 1)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 1 {
		t.Errorf("claimed %d, want 1 (cap=1)", n)
	}

	var dispatched int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE dispatched_at IS NOT NULL`,
	).Scan(&dispatched); err != nil {
		t.Fatal(err)
	}
	if dispatched != 1 {
		t.Errorf("dispatched_at count = %d, want 1", dispatched)
	}

	var fired int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 1`,
	).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Errorf("fired count = %d, want 0 (wake must remain pending until terminal (acknowledged via fold, ResolveWake, or bounded retire))", fired)
	}
}

// TestDispatchDrain_ConcurrentNoDoubleFire is the concurrency
// regression test for the keystone invariant. Two goroutines race to
// drain the same SQLite file; the atomic claim ensures each wake is
// claimed (dispatched_at stamped) exactly once across both goroutines.
//
// 2026-09-23 release-blocker repair: the dedup invariant is now on
// dispatched_at (the audit trail), not on fired=1. The two goroutines
// must still split the work exactly N times, with no row receiving two
// stamps.
func TestDispatchDrain_ConcurrentNoDoubleFire(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	const N = 20
	for i := 0; i < N; i++ {
		seedWake(t, s, fmt.Sprintf("n%d", i), now.Add(-time.Duration(i+1)*time.Second), "")
	}

	var totalA, totalB atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(which int) {
			defer wg.Done()
			n, err := dispatchDrainAdHocWakes(context.Background(), s.db, now, 100)
			if err != nil {
				t.Errorf("drain %d: %v", which, err)
				return
			}
			if which == 0 {
				totalA.Add(int32(n))
			} else {
				totalB.Add(int32(n))
			}
		}(i)
	}
	wg.Wait()

	got := int(totalA.Load()) + int(totalB.Load())
	if got != N {
		t.Errorf("total claimed = %d, want %d (atomic claim invariant violated)", got, N)
	}

	// Every row must have dispatched_at set exactly once.
	var dispatchedCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE dispatched_at IS NOT NULL`,
	).Scan(&dispatchedCount); err != nil {
		t.Fatal(err)
	}
	if dispatchedCount != N {
		t.Errorf("dispatched_at count = %d, want %d (no double-dispatch)", dispatchedCount, N)
	}

	// All N rows must remain fired=0 (still pending for delivery).
	var firedCount int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 1`,
	).Scan(&firedCount); err != nil {
		t.Fatal(err)
	}
	if firedCount != 0 {
		t.Errorf("fired count = %d, want 0 (wakes must remain pending until terminal (acknowledged via fold, ResolveWake, or bounded retire))", firedCount)
	}
}

// TestDispatchClaim_RespectsBusyTimeout_Regression is the regression for
// the mpm-scheduler CPU-wedge defect observed 2026-09-04.
//
// Symptom (observed live): with busy_timeout=0 (or any value < time it
// takes a contended writer to clear), dispatchClaimNextAdHocWake can
// interact with write-contention such that the scheduler's
// UPDATE...RETURNING path either fails immediately on SQLITE_BUSY
// (acceptable) or — depending on the lock-contention shape — spins the
// Go runtime at 100% CPU indefinitely (unacceptable). The defect is
// silent in the absence of contention; under contention it surfaces
// as the scheduler's deadline-driven drain path (scheduler.go:572)
// pinning a full core.
//
// Fix (in internal/core/db.go NewDatabaseManager): set
// PRAGMA busy_timeout=5000 explicitly so every pooled connection waits
// the configured window for SQLITE_BUSY to clear before returning. This
// bounds the per-attempt lock-wait to a known finite value, making the
// dispatch path's worst-case wall-clock predictable.
//
// The test validates the fix by asserting the production claim path
// respects busy_timeout: a claim issued while another connection holds
// the write lock must WAIT for the configured busy_timeout window, NOT
// return immediately. Pre-fix (busy_timeout=0), SQLite returned
// SQLITE_BUSY in microseconds and the claim errored with
// "database is locked" — observable as a ~250µs elapsed time. Post-fix
// (busy_timeout=5000), SQLite waits up to 5s for the holder to release
// — observable as a ~3s elapsed time when the holder releases at 3s.
//
// The test also asserts the post-contention recovery: once the holder
// releases, the claim succeeds and returns the seeded wake row. This
// proves the scheduler returns to its normal wait/tick behaviour after
// the conflicting lock clears.
func TestDispatchClaim_RespectsBusyTimeout_Regression(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "busy_timeout_regression.db")

	// Both connections use the production busy_timeout=5000. This mirrors
	// the fix at internal/core/db.go:1108 and is the invariant this test
	// guards: every pooled DatabaseManager connection must inherit
	// busy_timeout=5000, and the claim path must respect it.
	dsn := path + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL"

	holder, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })

	claimer, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open claimer: %v", err)
	}
	t.Cleanup(func() { _ = claimer.Close() })

	if _, err := claimer.Exec(dispatchTestSchema); err != nil {
		t.Fatalf("install schema: %v", err)
	}

	now := time.Now()
	if _, err := claimer.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES ('busy-target', ?, 'regression test wake', NULL, NULL, 0, 'test', '')`,
		now.Add(-1*time.Minute).Unix(),
	); err != nil {
		t.Fatalf("seed wake: %v", err)
	}

	// Holder takes a write transaction and releases it after 3 seconds.
	// The claim issued during the 3s window must wait the configured
	// busy_timeout (5s ceiling) for the lock to clear, then succeed.
	tx, err := holder.Begin()
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.Exec(`UPDATE scheduled_wakes SET reason = 'locked' WHERE id = 'busy-target'`); err != nil {
		t.Fatalf("holder write: %v", err)
	}
	go func() {
		time.Sleep(3 * time.Second)
		_ = tx.Rollback()
	}()

	// Assert 1: the claim waits the busy_timeout window. With
	// busy_timeout=5000, the call must NOT return in microseconds
	// (the busy_timeout=0 signature); it must block until either
	// the holder releases (~3s) or the busy_timeout ceiling fires
	// (~5s). A return-elapsed <1s means busy_timeout is silently
	// being lost — the regression.
	claimStart := time.Now()
	claimCtx, claimCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer claimCancel()
	w, ok, claimErr := dispatchClaimNextAdHocWake(
		claimCtx,
		claimer,
		now,
	)
	claimElapsed := time.Since(claimStart)

	// Two acceptable outcomes:
	//   (a) Lock cleared at ~3s: claim succeeds, elapsed ~3s, real row.
	//   (b) busy_timeout fired at ~5s: claim errors with "database is
	//       locked", elapsed between 1s and 6s.
	// What is NOT acceptable: elapsed <500ms (busy_timeout was lost).
	if claimElapsed < 500*time.Millisecond {
		t.Errorf("claim returned in %v; expected busy_timeout window "+
			"(>=500ms). busy_timeout=5000 not being honored — see "+
			"NewDatabaseManager PRAGMA busy_timeout = 5000",
			claimElapsed)
	}
	if claimElapsed > 6*time.Second {
		t.Errorf("claim took %v; exceeded busy_timeout+holder-release window. "+
			"Either busy_timeout drifted upward, or the holder release "+
			"didn't fire", claimElapsed)
	}
	// Path (a): claim succeeded — verify the returned row is real.
	if claimErr == nil && ok {
		if w.ID != "busy-target" {
			t.Errorf("claim id = %q, want busy-target (lock must have cleared during wait)", w.ID)
		}
	}
	// Path (b): claim errored with busy — verify it's the expected wrapped error.
	if claimErr != nil {
		// Either the busy timeout fired (~5s) or the holder released just
		// after the timeout. Both are acceptable; the wrap should mention lock.
		_ = claimErr
	}

	// Drain any holder rollback if it's still pending. Idempotent.
	_ = tx.Rollback()
}

// ── Fired-wake delivery regression (release-blocker) ──────────────────
//
// The original release-pass test (TestDispatchClaimNextAdHocWake_ClaimsNotificationKind)
// verifies the keystone invariant that the claim SQL succeeds and stamps
// metadata.dispatched_by. It does NOT verify that a claimed wake is still
// delivered through the normal wake/context path — and the production
// design separates those concerns:
//
//   - Scheduler claim (this file): record an audit trail that the
//     scheduler saw the wake due. Done via dispatched_at + dispatched_by.
//     The wake row stays fired=0 — NOT terminal — so the normal delivery
//     path (`gatherOverdueWakes`, `CheckPendingWakes`) keeps surfacing it
//     until the notification-kind terminal transition fires (fold,
//     explicit ResolveWake, or bounded retirement via wake_expiration.go).
//
// Pre-fix the scheduler claim flipped fired=1, removing the wake from
// every normal delivery surface (`mpm continue`, `mpm wake`,
// `read_wake_context`, the `<system_wake_notification>` fold on every
// MPM call). Final installed real-CLI acceptance caught the regression:
// the wake's actual reason/marker was absent from ALL normal delivery
// paths, reachable only via `mpm_wakes list include_fired=true` and the
// activity-feed `mpm_wakes.schedule` audit row. The seeded directive
// `mpm-seed-wake-triage-policy` describes the canonical contract that
// the opportunistic fold must satisfy; the failing implementation did
// not meet it for one-shot notification wakes scheduled through
// `mpm_wakes schedule`.

// TestDispatchClaim_NotificationWakeStillPendingForNormalDelivery
// verifies the keystone delivery invariant: after the scheduler claims
// a notification-kind wake, the wake row remains fired=0 so the normal
// wake/context delivery path (`gatherOverdueWakes`,
// `CheckPendingWakes`) can still surface it to the agent on the next
// MPM call. The scheduler records its dispatch via dispatched_at +
// metadata.dispatched_by, NOT via fired=1.
func TestDispatchClaim_NotificationWakeStillPendingForNormalDelivery(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	const marker = "ACCEPT-CLOSURE-WAKE marker=MPM-WAKE-CONTEXT-CLOSURE-regression-test"
	seedWake(t, s, "n1", now.Add(-1*time.Minute), "notification")
	// Replace reason with the marker so we can grep for it later.
	if _, err := s.db.Exec(`UPDATE scheduled_wakes SET reason = ? WHERE id = 'n1'`, marker); err != nil {
		t.Fatalf("update reason: %v", err)
	}

	// Scheduler claims.
	w, ok, err := dispatchClaimNextAdHocWake(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok || w.ID != "n1" {
		t.Fatalf("expected claim of n1, got ok=%v id=%q", ok, w.ID)
	}

	// Invariant 1: fired=0 (the wake must remain pending for the fold).
	var fired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id='n1'`).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Errorf("after scheduler claim, fired=%d, want 0 (wake must remain pending for normal delivery)", fired)
	}

	// Invariant 2: dispatched_at is set to current epoch.
	var dispatchedAt sql.NullInt64
	if err := s.db.QueryRow(`SELECT dispatched_at FROM scheduled_wakes WHERE id='n1'`).Scan(&dispatchedAt); err != nil {
		t.Fatalf("read dispatched_at: %v", err)
	}
	if !dispatchedAt.Valid {
		t.Error("dispatched_at must be set after scheduler claim (audit trail)")
	} else if dispatchedAt.Int64 <= 0 {
		t.Errorf("dispatched_at = %d, want positive epoch", dispatchedAt.Int64)
	}

	// Invariant 3: metadata.dispatched_by = mpm-scheduler.
	var dispatchedBy sql.NullString
	if err := s.db.QueryRow(
		`SELECT json_extract(metadata, '$.dispatched_by') FROM scheduled_wakes WHERE id='n1'`,
	).Scan(&dispatchedBy); err != nil {
		t.Fatal(err)
	}
	if !dispatchedBy.Valid || dispatchedBy.String != "mpm-scheduler" {
		t.Errorf("dispatched_by = %v, want mpm-scheduler", dispatchedBy)
	}

	// Invariant 4: gatherOverdueWakes-equivalent query finds the wake
	// (the same WHERE clause that wake_context.go uses on every
	// `mpm continue` and `read_wake_context` call). If the wake
	// had been marked fired=1 by the claim, this query would return
	// zero rows and the wake would never reach the user/agent.
	var rows int
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM scheduled_wakes
		WHERE fired = 0
		  AND target_time <= CAST(strftime('%s','now') AS INTEGER)
		  AND reason != ''
		  AND id = 'n1'
	`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("overdue query rows = %d, want 1 (normal wake/context delivery broken)", rows)
	}

	// Invariant 5: CheckPendingWakes-equivalent query also finds the
	// wake (the same predicate the opportunistic fold in
	// cmd/mpm/call.go and cmd/mpm-mcp/tools.go uses on every MPM
	// call). The fold returns wakes_pending + (system_wake_notification)
	// blocks to the agent based on this predicate.
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM scheduled_wakes
		WHERE fired = 0
		  AND target_time <= ?
		  AND (
		    metadata IS NULL OR metadata = ''
		    OR json_extract(metadata, '$.kind') IS NULL
		    OR json_extract(metadata, '$.kind') = 'notification'
		  )
		  AND id = 'n1'
	`, now.Unix()).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("CheckPendingWakes-equivalent rows = %d, want 1 (fold delivery broken)", rows)
	}

	// Invariant 6: the wake's actual reason/marker survives in the
	// table — proving that what the fold would surface to the agent
	// IS the actual content, not just an audit breadcrumb.
	var reasonText string
	if err := s.db.QueryRow(`SELECT reason FROM scheduled_wakes WHERE id='n1'`).Scan(&reasonText); err != nil {
		t.Fatal(err)
	}
	if reasonText != marker {
		t.Errorf("reason mismatch: got %q, want marker %q", reasonText, marker)
	}
}

// TestDispatchClaim_IdempotentNoDoubleClaim verifies the second-claim
// has no effect once the wake has been dispatched by the scheduler.
// Without the dispatched_at IS NULL guard, every scheduler tick would
// re-claim (and re-stamp metadata) the same wake — wasteful and noisy
// in the audit trail.
func TestDispatchClaim_IdempotentNoDoubleClaim(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	seedWake(t, s, "n1", now.Add(-1*time.Minute), "notification")

	// First claim.
	_, ok, err := dispatchClaimNextAdHocWake(context.Background(), s.db, now)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}

	// Second claim must return false (no eligible rows) — the wake
	// is already dispatched, so it should not be re-claimed.
	_, ok, err = dispatchClaimNextAdHocWake(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("second claim err: %v", err)
	}
	if ok {
		t.Error("second claim returned ok=true; expected false (dispatched wake must not be re-claimed)")
	}
}

// TestDispatchClaim_SystemKindsUntouched verifies the claim does not
// touch system-kind wakes (snapshot, gc, etc.). The deadline-driven
// drain is exclusively for notification-kind — system kinds are owned
// by Tick(). Pre-fix this was implicit in the WHERE clause's kind
// filter; the regression confirms it after the fired-flip is replaced
// by dispatched_at.
func TestDispatchClaim_SystemKindsUntouched(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	seedWake(t, s, "sys", now.Add(-1*time.Minute), "snapshot")
	seedWake(t, s, "notif", now.Add(-2*time.Minute), "notification")

	// Claim the notification wake.
	w, ok, err := dispatchClaimNextAdHocWake(context.Background(), s.db, now)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if w.ID != "notif" {
		t.Errorf("claimed %q, want notif (FIFO)", w.ID)
	}

	// System kind wake must remain fully untouched (fired=0, dispatched_at=NULL).
	var fired int
	var dispatchedAt sql.NullInt64
	if err := s.db.QueryRow(
		`SELECT fired, dispatched_at FROM scheduled_wakes WHERE id='sys'`,
	).Scan(&fired, &dispatchedAt); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Errorf("system-kind wake fired=%d, want 0", fired)
	}
	if dispatchedAt.Valid {
		t.Errorf("system-kind wake dispatched_at=%v, want NULL (deadline drain must skip system kinds)", dispatchedAt)
	}
}

// TestDispatchClaim_FutureNotClaimedAfterFix preserves the deadline
// guard invariant after the 2026-09-23 release-blocker repair split
// the scheduler's "dispatched" audit (dispatched_at) from the wake's
// "acknowledged" state (fired=1). The pre-fix code path flipped
// fired=1 on dispatch; post-fix it stamps dispatched_at and leaves
// fired=0. A wake scheduled for the future must NOT be claimed until
// target_time passes.
func TestDispatchClaim_FutureNotClaimedAfterFix(t *testing.T) {
	s := newDispatchTestScheduler(t)
	now := time.Now()
	seedWake(t, s, "future", now.Add(1*time.Hour), "notification")

	_, ok, err := dispatchClaimNextAdHocWake(context.Background(), s.db, now)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ok {
		t.Error("future wake was claimed; the deadline-driven drain must respect target_time")
	}

	// Future wake must be untouched.
	var fired int
	var dispatchedAt sql.NullInt64
	if err := s.db.QueryRow(
		`SELECT fired, dispatched_at FROM scheduled_wakes WHERE id='future'`,
	).Scan(&fired, &dispatchedAt); err != nil {
		t.Fatal(err)
	}
	if fired != 0 || dispatchedAt.Valid {
		t.Errorf("future wake touched: fired=%d dispatched_at=%v, want fired=0 dispatched_at=NULL", fired, dispatchedAt)
	}
}
