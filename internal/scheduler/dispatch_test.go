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
// metadata='' (not '{}') for the default no-kind case. Earlier the
// claim UPDATE used COALESCE(metadata,'{}'), which on empty string
// returned '' because '' is non-NULL in SQLite — then json_set('')
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
// This test inserts a row with metadata='' directly and asserts the
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

	var fired int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes WHERE fired=1`).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired != N {
		t.Errorf("fired count = %d, want %d", fired, N)
	}
}

// TestDispatchDrainAdHocWakes_LeavesFutureAlone verifies that a drain
// with mixed past/future wakes claims only the past ones.
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

	var pastFired, futureFired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id='past'`).Scan(&pastFired); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id='future'`).Scan(&futureFired); err != nil {
		t.Fatal(err)
	}
	if pastFired != 1 {
		t.Errorf("past fired=%d, want 1", pastFired)
	}
	if futureFired != 0 {
		t.Errorf("future fired=%d, want 0", futureFired)
	}
}

// TestDispatchDrainAdHocWakes_RespectsCap verifies capN bounds a
// single drain pass. A cap of 1 against 5 due rows yields 1 claim and
// 4 still-pending rows.
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

	var fired int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes WHERE fired=1`).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Errorf("fired count = %d, want 1", fired)
	}
}

// TestDispatchDrain_ConcurrentNoDoubleFire is the concurrency
// regression test for the keystone invariant. Two goroutines race to
// drain the same SQLite file; the atomic claim ensures each wake is
// fired exactly once across both goroutines.
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

	var firedCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes WHERE fired=1`).Scan(&firedCount); err != nil {
		t.Fatal(err)
	}
	if firedCount != N {
		t.Errorf("fired count = %d, want %d (no double-fire)", firedCount, N)
	}
}
