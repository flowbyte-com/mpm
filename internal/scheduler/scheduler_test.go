// Tests for the universal wake executor.
//
// These tests use an in-memory SQLite database to avoid coupling to
// filesystem state. They cover the architectural guarantees from
// decision 27d7b3c18199e098:
//
//   1. Default kind = notification (untagged wakes pass through)
//   2. Concurrent execution (parallel goroutines for system kinds)
//   3. Failure isolation (handler errors mark fired with last_error,
//      do not block other wakes)
//
// The schema bootstrap mirrors the mpm-core init path enough to let
// CheckPendingWakes / ScheduleWake / MarkFired all work.

package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const testSchema = `
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
CREATE INDEX IF NOT EXISTS idx_scheduled_wakes_due ON scheduled_wakes(fired, target_time);
CREATE TABLE scheduled_tasks (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    cron_expr TEXT NOT NULL,
    directive_id TEXT NOT NULL,
    status TEXT CHECK (status IN ('active', 'paused')),
    last_run_at INTEGER,
    next_run_at INTEGER NOT NULL,
    created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
);
CREATE INDEX idx_scheduled_tasks_poll ON scheduled_tasks(status, next_run_at);
`

// newTestScheduler returns a Scheduler backed by a temp file DB with the
// scheduled_wakes schema installed and a discard logger. We use a file
// (not :memory:) because WAL mode + :memory: gives each connection its
// own database, which breaks tests that share a *sql.DB across goroutines
// via concurrent MarkFired calls.
func newTestScheduler(t *testing.T) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(testSchema); err != nil {
		t.Fatalf("install schema: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	return &Scheduler{
		db:           db,
		dbPath:       path,
		log:          log,
		handlers:     make(map[string]HandlerFunc),
		tickHandlers: make(map[string]func(ctx context.Context) error),
		wakeCh:       make(chan struct{}, 1),
	}
}

// seedWake inserts a wake directly. Bypasses ScheduleWake so tests don't
// depend on the absolute/relative time parser.
func seedWake(t *testing.T, s *Scheduler, id string, targetTime time.Time, kind string) {
	t.Helper()
	metadata := `{}`
	if kind != "" {
		metadata = fmt.Sprintf(`{"kind":%q}`, kind)
	}
	_, err := s.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES (?, ?, ?, '', '', 0, 'test', ?)`,
		id, targetTime.Unix(), "test-wake "+id, metadata,
	)
	if err != nil {
		t.Fatalf("seed wake %s: %v", id, err)
	}
}

// TestWake_Kind verifies the dispatch key extraction including the
// notification default. The default IS the backward-compat guarantee:
// existing untagged wakes pass through to mpm-mcp's opportunistic fold.
func TestWake_Kind(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string]interface{}
		want     string
	}{
		{"no metadata", nil, "notification"},
		{"empty metadata", map[string]interface{}{}, "notification"},
		{"kind missing", map[string]interface{}{"label": "x"}, "notification"},
		{"kind empty string", map[string]interface{}{"kind": ""}, "notification"},
		{"kind snapshot", map[string]interface{}{"kind": "snapshot"}, "snapshot"},
		{"kind critic_audit", map[string]interface{}{"kind": "critic_audit"}, "critic_audit"},
		{"kind gc", map[string]interface{}{"kind": "gc"}, "gc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := Wake{Metadata: c.metadata}
			if got := w.Kind(); got != c.want {
				t.Errorf("Kind() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestQueryDueWakes verifies the query returns unfired, due wakes in
// chronological order. Notification kinds are not filtered here — that
// partition happens at the mpm-core CheckPendingWakes layer.
func TestQueryDueWakes(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()

	// Three wakes: one past, one now, one future.
	seedWake(t, s, "past", now.Add(-1*time.Hour), "snapshot")
	seedWake(t, s, "now", now, "snapshot")
	seedWake(t, s, "future", now.Add(1*time.Hour), "snapshot")
	// One already fired.
	seedWake(t, s, "fired-past", now.Add(-2*time.Hour), "snapshot")
	if _, err := s.db.Exec(`UPDATE scheduled_wakes SET fired = 1 WHERE id = 'fired-past'`); err != nil {
		t.Fatal(err)
	}

	got, err := s.QueryDueWakes(now)
	if err != nil {
		t.Fatalf("QueryDueWakes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d wakes, want 2 (past and now)", len(got))
	}
	if got[0].ID != "past" || got[1].ID != "now" {
		t.Errorf("order: got [%s, %s], want [past, now]", got[0].ID, got[1].ID)
	}
}

// TestTick_NotificationPassThrough verifies that Tick() never touches
// notification-kind wakes — they remain owned by the deadline-driven
// dispatch loop (internal/scheduler/dispatch.go + Run's deadlineTimer.C
// branch). The split is load-bearing:
//
//   - Tick handles only system kinds (snapshot, gc, critic_audit,
//     broadcast, drill, cascade_summary, cascade, cron).
//   - dispatchDrainAdHocWakes handles notification + untagged wakes.
//
// A wake must NEVER be claimed by both paths. The atomic UPDATE
// WHERE fired=0 invariant in dispatchClaimNextAdHocWake makes the
// claim idempotent if the safety-net opportunistic fold in mpm call /
// mpm-mcp later races on the same row.
func TestTick_NotificationPassThrough(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	seedWake(t, s, "notif-1", now.Add(-1*time.Minute), "") // empty kind = notification
	seedWake(t, s, "notif-2", now.Add(-1*time.Minute), "notification")

	executed, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if executed != 0 {
		t.Errorf("executed = %d, want 0 (notification wakes must not be executed by Tick)", executed)
	}

	// Both wakes should still be unfired — Tick left them for the
	// deadline-driven dispatch loop (or the opportunistic fold).
	for _, id := range []string{"notif-1", "notif-2"} {
		var fired int
		if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = ?`, id).Scan(&fired); err != nil {
			t.Fatal(err)
		}
		if fired != 0 {
			t.Errorf("%s fired = %d, want 0 (Tick must not claim notification wakes)", id, fired)
		}
	}
}

// TestTick_SystemKindExecuted verifies a system-kind wake is executed
// and marked fired on success.
func TestTick_SystemKindExecuted(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	seedWake(t, s, "snap-1", now.Add(-1*time.Minute), "snapshot")

	called := atomic.Int32{}
	s.Register("snapshot", func(ctx context.Context, w Wake) error {
		called.Add(1)
		return nil
	})

	executed, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if executed != 1 {
		t.Errorf("executed = %d, want 1", executed)
	}
	if called.Load() != 1 {
		t.Errorf("handler called %d times, want 1", called.Load())
	}

	var fired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = 'snap-1'`).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Errorf("fired = %d, want 1", fired)
	}
}

// TestTick_FailureIsolation verifies a handler that returns an error
// still marks the wake fired, sets last_error in metadata, and does NOT
// block subsequent wakes.
func TestTick_FailureIsolation(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	seedWake(t, s, "fails", now.Add(-1*time.Minute), "snapshot")
	seedWake(t, s, "succeeds", now.Add(-1*time.Minute), "snapshot")

	called := atomic.Int32{}
	s.Register("snapshot", func(ctx context.Context, w Wake) error {
		called.Add(1)
		if w.ID == "fails" {
			return errors.New("synthetic failure")
		}
		return nil
	})

	executed, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if executed != 1 {
		t.Errorf("executed = %d, want 1 (only succeeds counts)", executed)
	}
	if called.Load() != 2 {
		t.Errorf("handler called %d times, want 2 (both wakes attempted)", called.Load())
	}

	// Both should be marked fired; only the failing one should have last_error.
	for _, c := range []struct {
		id         string
		wantFired  int
		wantErrSub string
	}{
		{"fails", 1, "synthetic failure"},
		{"succeeds", 1, ""},
	} {
		var fired int
		var metadata string
		if err := s.db.QueryRow(`SELECT fired, metadata FROM scheduled_wakes WHERE id = ?`, c.id).Scan(&fired, &metadata); err != nil {
			t.Fatal(err)
		}
		if fired != c.wantFired {
			t.Errorf("%s fired = %d, want %d", c.id, fired, c.wantFired)
		}
		if c.wantErrSub != "" {
			if !contains(metadata, c.wantErrSub) {
				t.Errorf("%s metadata missing last_error=%q: %s", c.id, c.wantErrSub, metadata)
			}
		}
	}
}

// TestTick_ConcurrentExecution verifies independent system wakes fire
// in parallel goroutines, not serially. The SF2 race test failure mode
// was exactly serial execution.
func TestTick_ConcurrentExecution(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	const N = 5
	for i := 0; i < N; i++ {
		seedWake(t, s, fmt.Sprintf("snap-%d", i), now.Add(-1*time.Minute), "snapshot")
	}

	// Each handler sleeps 200ms. If serial, total = ~1s. If parallel, ~200ms.
	const handlerDelay = 200 * time.Millisecond
	s.Register("snapshot", func(ctx context.Context, w Wake) error {
		time.Sleep(handlerDelay)
		return nil
	})

	start := time.Now()
	executed, err := s.Tick(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if executed != N {
		t.Errorf("executed = %d, want %d", executed, N)
	}

	// Parallel: should be < N*handlerDelay/2 (some headroom for goroutine
	// startup overhead but well under serial).
	maxSerial := time.Duration(N) * handlerDelay
	if elapsed >= maxSerial {
		t.Errorf("elapsed = %v, expected < %v (parallel execution required)", elapsed, maxSerial)
	}
}

// TestTick_UnknownKind verifies a wake with a non-notification kind but
// no registered handler is logged and skipped (does not error the tick).
func TestTick_UnknownKind(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	seedWake(t, s, "weird", now.Add(-1*time.Minute), "future_kind")

	executed, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if executed != 0 {
		t.Errorf("executed = %d, want 0 (no handler registered)", executed)
	}

	// The wake stays unfired — leaving it for an eventual handler registration.
	var fired int
	if err := s.db.QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = 'weird'`).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Errorf("fired = %d, want 0 (unregistered kinds should not be marked)", fired)
	}
}

// TestRegister_Overwrite verifies re-registering a kind replaces the
// previous handler (used for test isolation and runtime swap).
func TestRegister_Overwrite(t *testing.T) {
	s := newTestScheduler(t)

	var first, second atomic.Int32
	s.Register("snapshot", func(ctx context.Context, w Wake) error { first.Add(1); return nil })
	s.Register("snapshot", func(ctx context.Context, w Wake) error { second.Add(1); return nil })

	now := time.Now()
	seedWake(t, s, "x", now.Add(-1*time.Minute), "snapshot")
	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	if first.Load() != 0 {
		t.Errorf("first handler called %d times, want 0 (should be replaced)", first.Load())
	}
	if second.Load() != 1 {
		t.Errorf("second handler called %d times, want 1", second.Load())
	}
}

// TestRun_ContextCancellation verifies the ticker loop stops cleanly
// when the context is cancelled.
func TestRun_ContextCancellation(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	seedWake(t, s, "x", now.Add(-1*time.Minute), "snapshot")

	s.Register("snapshot", func(ctx context.Context, w Wake) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// Use 1s interval (Run's minimum). Three ticks would take 3s; we
	// only need one or two. Cancel after 1.2s.
	go func() { done <- s.Run(ctx, 1*time.Second) }()

	time.Sleep(1200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop within 2s of context cancel")
	}
}

// TestAcquireLock_Singleton verifies two AcquireLock calls on the same
// path produce an error on the second (kernel flock prevents double-run).
func TestAcquireLock_Singleton(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	second, err := AcquireLock(path)
	if err == nil {
		_ = second.Close()
		t.Fatal("second AcquireLock succeeded; expected flock to block")
	}
	if !contains(err.Error(), "flock") {
		t.Errorf("error = %v, want one mentioning flock", err)
	}
}

// TestRotateSnapshots_KeepsN verifies rotation deletes the oldest entries
// when the count exceeds the keep threshold.
func TestRotateSnapshots_KeepsN(t *testing.T) {
	dir := t.TempDir()
	keep := 3

	// Create 5 fake snapshots with staggered mtimes.
	for i := 0; i < 5; i++ {
		path := filepath.Join(dir, fmt.Sprintf("mpm_pre_critic_%02d.db", i))
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Force mtime order: 0 oldest, 4 newest.
		mtime := time.Now().Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	if err := rotateSnapshots(dir, keep); err != nil {
		t.Fatalf("rotateSnapshots: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != keep {
		t.Errorf("after rotation: %d entries, want %d", len(entries), keep)
	}

	// The 3 kept should be 02, 03, 04 (newest).
	for want := 2; want < 2+keep; want++ {
		name := fmt.Sprintf("mpm_pre_critic_%02d.db", want)
		found := false
		for _, e := range entries {
			if e.Name() == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing kept snapshot %s", name)
		}
	}
}

// ── helpers ─────────────────────────────────────────────────────────────

func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestRegisterTickHandler_FiresOnEveryTick verifies a registered tick handler
// runs on every Tick, not just on ticks with wakes.
func TestRegisterTickHandler_FiresOnEveryTick(t *testing.T) {
	s := newTestScheduler(t)

	var count int
	var mu sync.Mutex
	s.RegisterTickHandler("test_counter", func(ctx context.Context) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	})

	for i := 0; i < 3; i++ {
		if _, err := s.Tick(context.Background()); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if count != 3 {
		t.Errorf("tick handler ran %d times, expected 3", count)
	}
}

// TestRegisterTickHandler_FailureDoesNotBlock verifies a handler that returns
// an error does not prevent subsequent tick handlers from running.
func TestRegisterTickHandler_FailureDoesNotBlock(t *testing.T) {
	s := newTestScheduler(t)

	var ranAfterFailure bool
	s.RegisterTickHandler("always_errors", func(ctx context.Context) error {
		return fmt.Errorf("boom")
	})
	s.RegisterTickHandler("second", func(ctx context.Context) error {
		ranAfterFailure = true
		return nil
	})

	_, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if !ranAfterFailure {
		t.Errorf("second tick handler did not run after first handler errored")
	}
}

// TestRegisterTickHandler_FiresEvenWithNoWakes is the critical idempotency
// test: tick handlers must fire even when there are no wakes (the wake
// dispatch returns early). The defer pattern in Tick makes this work — without
// it, the early return at len(wakes)==0 would skip tick dispatch entirely.
func TestRegisterTickHandler_FiresEvenWithNoWakes(t *testing.T) {
	s := newTestScheduler(t)

	var fired bool
	var mu sync.Mutex
	s.RegisterTickHandler("fires_on_idle", func(ctx context.Context) error {
		mu.Lock()
		fired = true
		mu.Unlock()
		return nil
	})

	// Tick with an empty outbox — the existing wake dispatch returns early,
	// but tick handlers must still fire.
	_, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !fired {
		t.Errorf("tick handler did not fire on idle tick")
	}
}

// avoid unused-import warning if sync becomes unreferenced in some builds
var _ = sync.Once{}

// TestCascadeSummaryHandler_LogsAndReturnsNil covers the cascade_summary
// wake kind (issue #5). The handler is a closure-captured no-op that
// emits one structured log line per wake and returns nil — the load-
// bearing assertions are:
//
//  1. nil error (so executeOne marks the wake fired without last_error)
//  2. the four metadata fields (materialized/failed/pending_after/
//     elapsed_ms) surface as separate log fields, not embedded JSON
func TestCascadeSummaryHandler_LogsAndReturnsNil(t *testing.T) {
	logger, buf, mu := captureLogger()
	h := NewCascadeSummaryHandler(logger)

	w := Wake{
		ID:         "cascade_summary_test_1",
		TargetTime: time.Now().Unix(),
		Reason:     "cascade tick summary",
		CreatedBy:  "cascade_drain",
		CreatedAt:  time.Now().Unix(),
		Metadata: map[string]interface{}{
			"kind":          "cascade_summary",
			"materialized":  float64(3),
			"failed":        float64(1),
			"pending_after": float64(7),
			"elapsed_ms":    float64(4521),
		},
	}
	if err := h(context.Background(), w); err != nil {
		t.Fatalf("CascadeSummaryHandler: unexpected error: %v", err)
	}

	logs := captureLogsFrom(buf, mu)
	if len(logs) == 0 {
		t.Fatal("CascadeSummaryHandler emitted no log lines")
	}

	var saw map[string]any
	for _, rec := range logs {
		if msg, _ := rec["msg"].(string); msg == "cascade summary" {
			saw = rec
			break
		}
	}
	if saw == nil {
		t.Fatalf("CascadeSummaryHandler log line not found; got: %v", logs)
	}
	// JSON unmarshals numbers as float64 — check the underlying value,
	// not the asserted Go type.
	mat, _ := saw["materialized"].(float64)
	fail, _ := saw["failed"].(float64)
	pa, _ := saw["pending_after"].(float64)
	em, _ := saw["elapsed_ms"].(float64)
	if mat != 3 || fail != 1 || pa != 7 || em != 4521 {
		t.Errorf("log line fields wrong: materialized=%v failed=%v pending_after=%v elapsed_ms=%v (want 3/1/7/4521)",
			mat, fail, pa, em)
	}
	if id, _ := saw["wake_id"].(string); id != w.ID {
		t.Errorf("log line wake_id = %q, want %q", id, w.ID)
	}
}

// TestScheduler_NotifyScheduleChanged_NonBlocking verifies that the
// in-process notification API does not deadlock when the channel is
// already full. Capacity-1 coalescing is the documented behaviour:
// a second signal during a pending wake-up must not block, must not
// queue, must simply be ignored.
func TestScheduler_NotifyScheduleChanged_NonBlocking(t *testing.T) {
	s := newTestScheduler(t)
	// Fill the channel directly.
	s.wakeCh <- struct{}{}
	// This send must not block, must not deadlock the test.
	done := make(chan struct{})
	go func() {
		s.NotifyScheduleChanged()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("NotifyScheduleChanged blocked when channel was full (capacity-1 coalescing invariant violated)")
	}
	// Drain the original signal; wakeCh now empty.
	<-s.wakeCh
}

// TestScheduler_NotifyScheduleChanged_NilReceiver verifies the
// defensive nil-check on wakeCh. A Scheduler constructed without
// going through New() (e.g. embedded test stub) must not panic when
// NotifyScheduleChanged is called.
func TestScheduler_NotifyScheduleChanged_NilReceiverSafe(t *testing.T) {
	s := &Scheduler{}
	s.NotifyScheduleChanged() // must not panic
}

// TestScheduler_computeEarliestDeadline verifies the deadline query
// folds both scheduled_wakes (unfired) and scheduled_tasks (active)
// into a single MIN() and respects the partial-index partitions:
// acknowledged (fired=1) wakes and paused tasks are excluded.
//
// The wake branch filters to notification-eligible kinds (matching
// dispatchClaimNextAdHocWake's WHERE clause) so the deadline reflects
// work the deadline-driven dispatcher can actually perform. Cron-kind
// and other system-kind rows are NOT the dispatcher's concern even
// when their target_time is past.
func TestScheduler_computeEarliestDeadline(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, s *Scheduler, now time.Time)
		wantOff time.Duration // 0 == empty (zero time)
	}{
		{
			name:  "empty database yields zero time",
			setup: func(t *testing.T, s *Scheduler, now time.Time) {},
		},
		{
			name: "wake only",
			setup: func(t *testing.T, s *Scheduler, now time.Time) {
				seedWake(t, s, "w1", now.Add(30*time.Second), "")
			},
			wantOff: 30 * time.Second,
		},
		{
			name: "active task only",
			setup: func(t *testing.T, s *Scheduler, now time.Time) {
				mustExec(t, s.db, `INSERT INTO scheduled_tasks (id, name, cron_expr, directive_id, status, next_run_at) VALUES ('t1','a','* * * * *','d1','active',?)`, now.Add(10*time.Second).Unix())
			},
			wantOff: 10 * time.Second,
		},
		{
			name: "wake + active task, task wins",
			setup: func(t *testing.T, s *Scheduler, now time.Time) {
				seedWake(t, s, "w1", now.Add(30*time.Second), "")
				mustExec(t, s.db, `INSERT INTO scheduled_tasks (id, name, cron_expr, directive_id, status, next_run_at) VALUES ('t1','a','* * * * *','d1','active',?)`, now.Add(5*time.Second).Unix())
			},
			wantOff: 5 * time.Second,
		},
		{
			name: "past notification wake yields negative offset",
			setup: func(t *testing.T, s *Scheduler, now time.Time) {
				seedWake(t, s, "w1", now.Add(-1*time.Minute), "")
			},
			wantOff: -1 * time.Minute,
		},
		{
			name: "acknowledged (fired=1) wake is excluded",
			setup: func(t *testing.T, s *Scheduler, now time.Time) {
				seedWake(t, s, "w1", now.Add(1*time.Second), "")
				mustExec(t, s.db, `UPDATE scheduled_wakes SET fired=1 WHERE id='w1'`)
			},
		},
		{
			name: "paused task is excluded",
			setup: func(t *testing.T, s *Scheduler, now time.Time) {
				mustExec(t, s.db, `INSERT INTO scheduled_tasks (id, name, cron_expr, directive_id, status, next_run_at) VALUES ('t1','a','* * * * *','d1','paused',?)`, now.Add(1*time.Second).Unix())
			},
		},
		{
			name: "past cron-kind wake does NOT pin deadline to zero (wedge regression)",
			setup: func(t *testing.T, s *Scheduler, now time.Time) {
				// A cron-kind wake with target_time in the past. Without the
				// notification-kind filter in computeEarliestDeadline, MIN(t)
				// would return this past timestamp, scheduleNextDeadline
				// clamps d=0, and Run enters a 0-delay select loop → 100% CPU
				// wedge with no dispatcher work to perform.
				mustExec(t, s.db, `INSERT INTO scheduled_wakes (id, target_time, reason, created_by, fired, metadata) VALUES ('cron-past',?, 'cron:test', 'mpm-scheduler', 0, ?)`,
					now.Add(-1*time.Hour).Unix(),
					`{"kind":"cron","task_id":"test"}`)
			},
			// No notification-eligible wake AND no scheduled_task → deadline zero.
			wantOff: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestScheduler(t)
			now := time.Now()
			c.setup(t, s, now)
			got, err := s.computeEarliestDeadline(context.Background(), now)
			if err != nil {
				t.Fatalf("compute: %v", err)
			}
			if c.wantOff == 0 {
				if !got.IsZero() {
					t.Errorf("got %v, want zero (empty)", got)
				}
				return
			}
			want := now.Add(c.wantOff)
			// Allow 1-second slack for unix-truncation between arg and result.
			if diff := got.Sub(want); diff > time.Second || diff < -time.Second {
				t.Errorf("got %v (Δ=%v from now), want ~%v (Δ=%v from now)", got, diff, want, c.wantOff)
			}
		})
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

// TestScheduler_FiresAtTargetTime is the keystone acceptance test
// from the original report. It verifies the deadline-driven loop
// dispatches a notification-kind wake at its target_time without
// waiting for the maintenance interval, and without any unrelated
// `mpm call` driving an opportunistic fold.
//
// This is the regression that motivated the change: previously, a
// wake scheduled for +500ms would sit unfired for up to 60s because
// nothing else ever asked the mpm call chokepoint to look at it.
//
// 2026-09-23 release-blocker repair: "dispatched" means
// dispatched_at is set; fired stays 0 so the wake remains pending for
// normal delivery.
func TestScheduler_FiresAtTargetTime(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	target := now.Add(500 * time.Millisecond)
	seedWake(t, s, "soon", target, "") // untagged = notification

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, 1*time.Second) }()

	// Wait up to 2s for the wake to be dispatched. The bounded
	// deadline timer should fire it within ~500ms + Jitter + drain
	// overhead.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("wake not dispatched within 2s (deadline-driven dispatch did not pick it up)")
		case <-time.After(50 * time.Millisecond):
			var dispatchedAt sql.NullInt64
			if err := s.db.QueryRow(
				`SELECT dispatched_at FROM scheduled_wakes WHERE id='soon'`,
			).Scan(&dispatchedAt); err != nil {
				cancel()
				<-done
				t.Fatal(err)
			}
			if dispatchedAt.Valid {
				cancel()
				return // PASS
			}
		}
	}
}

// TestScheduler_FiresMultipleDueWakesInOrder verifies the deadline-
// driven drain claims all currently-due wakes in a single iteration,
// in target_time ASC order (FIFO). The drain loop's cap=100 lets a
// single select-wake-up drain batches of accumulated work.
func TestScheduler_FiresMultipleDueWakesInOrder(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	const N = 5
	// Seed in REVERSE order; the drain must still claim in target_time order.
	for i := 0; i < N; i++ {
		seedWake(t, s, fmt.Sprintf("w%d", i), now.Add(time.Duration(i)*100*time.Millisecond), "")
	}
	// Wait for at least the first wake to come due.
	time.Sleep(150 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, 1*time.Second) }()

	// Wait up to 3s for ALL 5 to be claimed.
	// 2026-09-23 release-blocker repair: "claimed" means dispatched_at
	// is set; fired remains 0 until acknowledged by the user/agent.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			<-done
			var count int
			_ = s.db.QueryRow(
				`SELECT COUNT(*) FROM scheduled_wakes WHERE dispatched_at IS NOT NULL`,
			).Scan(&count)
			t.Fatalf("only %d/%d dispatched", count, N)
		case <-time.After(50 * time.Millisecond):
			var count int
			if err := s.db.QueryRow(
				`SELECT COUNT(*) FROM scheduled_wakes WHERE dispatched_at IS NOT NULL`,
			).Scan(&count); err != nil {
				cancel()
				<-done
				t.Fatal(err)
			}
			if count == N {
				cancel()
				return // PASS
			}
		}
	}
}

// TestScheduler_FoldsCronTasks verifies that scheduled_tasks rows
// (cron) participate in the deadline-driven loop. The maintenance
// tick (interval=1s in this test) calls ProcessScheduledTasks which
// finds rows WHERE next_run_at <= now. We verify the injected wake
// row (kind='cron', reason='cron:<task_id>') appears within a bounded
// window — the actual firing happens later in the handler
// (`hasHandler("cron")` returns whatever was registered, or false for
// this test which doesn't register one). What's under test here is:
// does the deadline-driven scheduler pick up the cron task on time?
func TestScheduler_FoldsCronTasks(t *testing.T) {
	s := newTestScheduler(t)
	now := time.Now()
	// A far-future wake to prove cron is the soonest pending deadline.
	seedWake(t, s, "w1", now.Add(1*time.Hour), "")
	// A cron task whose next_run_at is imminent.
	mustExec(t, s.db, `INSERT INTO scheduled_tasks (id, name, cron_expr, directive_id, status, next_run_at) VALUES ('t1','a-cron','* * * * *','d1','active',?)`, now.Add(800*time.Millisecond).Unix())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, 1*time.Second) }()

	// Wait up to 4s for the cron-driven wake row to be injected
	// (1s tick + 800ms slack + 2s headroom).
	deadline := time.After(4 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("cron task did not produce an injected wake row within 4s")
		case <-time.After(50 * time.Millisecond):
			var n int
			if err := s.db.QueryRow(
				`SELECT COUNT(*) FROM scheduled_wakes WHERE reason LIKE 'cron:%' AND created_by = ?`,
				"mpm-scheduler", // CronCreatedBy per internal/core/scheduled_tasks.go
			).Scan(&n); err != nil {
				cancel()
				<-done
				t.Fatal(err)
			}
			if n > 0 {
				cancel()
				return // PASS
			}
		}
	}
}

// TestScheduler_IdleDoesNotBusyLoop verifies that the deadline-driven
// loop does NOT spin when nothing is pending. With a 60s maintenance
// interval and an empty queue, computeEarliestDeadline returns zero,
// scheduleNextDeadline arms the timer for `interval` (60s), and the
// goroutine sleeps 60s.
//
// Asserting "no tick fired in 2.5s" is a proxy: at 60s interval, the
// maintenance ticker can fire at most 0-1 times in 2.5s. We assert
// tickCount == 0 because runOnce happens BEFORE Run's loop begins
// and is not part of tickCount.
func TestScheduler_IdleDoesNotBusyLoop(t *testing.T) {
	s := newTestScheduler(t)
	s.SetHeartbeat(0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, 60*time.Second) }()

	time.Sleep(2500 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("scheduler did not stop within 1s of ctx cancel")
	}

	if s.tickCount != 0 {
		t.Errorf("tickCount = %d, want 0 (no maintenance ticks should fire within 2.5s at 60s interval)", s.tickCount)
	}
}

// TestScheduler_DoesNotWedgeOnPastCronKindWake is the regression test
// for the 2026-09-04 100% CPU wedge. The reproduction:
//   - 3156 unfired scheduled_wakes rows accumulated over time, ALL of
//     them kind=cron (cron-injected wakes whose target_time is in the
//     past because ProcessScheduledTasks periodically re-arms them).
//   - 0 notification-eligible rows pending.
//   - computeEarliestDeadline's MIN(t) query, without the kind filter,
//     returned the soonest past cron-kind timestamp.
//   - scheduleNextDeadline clamped d=0 → timer fires immediately.
//   - select hits <-deadlineTimer.C → dispatchDrainAdHocWakes claims
//     nothing (no notification-eligible rows) → exit.
//   - select loops back, re-enters scheduleNextDeadline → d=0 again.
//   - Tight loop with no waiting → 100% CPU with no work.
//
// The fix: computeEarliestDeadline filters the wake branch to the
// kind-set dispatchClaimNextAdHocWake actually claims. With no
// notification-eligible rows AND no scheduled_tasks, the deadline is
// zero and the timer is armed for the full interval (60s).
func TestScheduler_DoesNotWedgeOnPastCronKindWake(t *testing.T) {
	s := newTestScheduler(t)
	s.SetHeartbeat(0)
	now := time.Now()

	// Many cron-kind wakes with target_time deep in the past — these
	// are the rows that piled up during cron re-arming cycles.
	for i := 0; i < 50; i++ {
		mustExec(t, s.db, `INSERT INTO scheduled_wakes (id, target_time, reason, created_by, fired, metadata) VALUES (?,?,?,?,0,?)`,
			fmt.Sprintf("cron-past-%d", i),
			now.Add(-time.Duration(i+1)*time.Minute).Unix(),
			"cron:epistemic-compaction",
			"mpm-scheduler",
			`{"kind":"cron","task_id":"epistemic-compaction"}`)
	}

	// Snapshot of computeEarliestDeadline — must NOT return a past timestamp.
	got, err := s.computeEarliestDeadline(context.Background(), now)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("computeEarliestDeadline = %v, want zero (no notification-eligible work pending)", got)
	}

	// End-to-end: run scheduler with 60s interval for 2s and verify
	// nothing in the loop spins. Before the fix this would burn 100%
	// CPU within milliseconds.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, 60*time.Second) }()
	time.Sleep(2000 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("scheduler did not stop within 1s of ctx cancel")
	}

	if s.tickCount != 0 {
		t.Errorf("tickCount = %d, want 0 (no maintenance ticks should fire within 2s at 60s interval)", s.tickCount)
	}
}

// TestScheduler_RestartRecoversPending verifies that an overdue wake
// is dispatched on (re)start without any unrelated mpm call. The
// deadline-driven loop arms the timer with d=0 when earliest is in
// the past, so the very first iteration of the select processes
// the overdue wake.
//
// 2026-09-23 release-blocker repair: "dispatched" now means
// dispatched_at is set; fired remains 0 so the wake is still pending
// for normal delivery via the fold / wake-context.
func TestScheduler_RestartRecoversPending(t *testing.T) {
	s := newTestScheduler(t)
	s.SetHeartbeat(0)
	now := time.Now()
	// An overdue wake.
	seedWake(t, s, "overdue", now.Add(-1*time.Second), "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, 60*time.Second) }()

	// Wait up to 2s for the scheduler to dispatch (the deadline
	// timer should fire on the very first iteration, since the only
	// pending deadline is in the past).
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("overdue wake did not get dispatched within 2s of scheduler start")
		case <-time.After(50 * time.Millisecond):
			var dispatchedAt sql.NullInt64
			if err := s.db.QueryRow(
				`SELECT dispatched_at FROM scheduled_wakes WHERE id='overdue'`,
			).Scan(&dispatchedAt); err != nil {
				cancel()
				<-done
				t.Fatal(err)
			}
			if dispatchedAt.Valid {
				cancel()
				return // PASS
			}
		}
	}
}

// TestScheduler_NotifyScheduleChanged_InterruptsWait verifies that
// an in-process wake mutation wakes Run() from its deadline sleep
// via the wakeCh. Scenario: schedule a far-future wake so the
// deadline timer parks for an hour; inject a now-due wake and
// NotifyScheduleChanged; the new earliest-deadline is "now" so the
// select hits the deadlineTimer.C branch on the next iteration.
//
// 2026-09-23 release-blocker repair: dispatch stamps dispatched_at,
// not fired=1. The test asserts the wake got dispatched (audit
// trail) within the deadline window.
func TestScheduler_NotifyScheduleChanged_InterruptsWait(t *testing.T) {
	s := newTestScheduler(t)
	s.SetHeartbeat(0)
	now := time.Now()
	// Far-future wake so the loop sleeps until interval (60s).
	seedWake(t, s, "far", now.Add(1*time.Hour), "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, 60*time.Second) }()

	// Give the scheduler a moment to enter its select.
	time.Sleep(200 * time.Millisecond)

	// Insert a now-due wake and signal.
	seedWake(t, s, "now-wake", now.Add(-100*time.Millisecond), "")
	s.NotifyScheduleChanged()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("now-wake did not get dispatched within 2s of NotifyScheduleChanged")
		case <-time.After(50 * time.Millisecond):
			var dispatchedAt sql.NullInt64
			if err := s.db.QueryRow(
				`SELECT dispatched_at FROM scheduled_wakes WHERE id='now-wake'`,
			).Scan(&dispatchedAt); err != nil {
				cancel()
				<-done
				t.Fatal(err)
			}
			if dispatchedAt.Valid {
				cancel()
				return // PASS
			}
		}
	}
}

// TestScheduler_ContextCancellation_CleanShutdown verifies that a
// context cancellation terminates Run() promptly and returns nil.
// Run() exits via the ctx.Done() case and stops its deadlineTimer.
func TestScheduler_ContextCancellation_CleanShutdown(t *testing.T) {
	s := newTestScheduler(t)
	s.SetHeartbeat(0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, 60*time.Second) }()

	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned err = %v, want nil", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not stop within 1s of ctx cancel")
	}
}
