package scheduler

// Tests for cumulative scheduler-ACTIVE uptime accrual and its
// interaction with memory settling baselines.
//
// The clock is injected, so every scenario runs with SYNTHETIC elapsed
// time. Nothing here sleeps or waits on wall time — the whole point of
// the monotonic model is that wall time is not the clock, and a test
// that consulted it could not distinguish a correct implementation from
// a wall-clock one.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// fakeClock is a controllable monotonic process clock. Tests advance it
// explicitly; nothing derives elapsed time from the wall clock.
type fakeClock struct{ at time.Duration }

func (c *fakeClock) elapsed() time.Duration { return c.at }

func (c *fakeClock) advance(d time.Duration) { c.at += d }

// schedulerUptimeDB builds a hermetic scheduler-bound *sql.DB with the
// canonical schema, isolated under t.TempDir. Foreign keys are ON so the
// baseline cascade behaves as it does in production.
func schedulerUptimeDB(t *testing.T) *sql.DB {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_SHARED_DB", "")
	dm, err := core.NewDatabaseManager(filepath.Join(ws, "db", "mpm.db"))
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	return dm.SQLDB()
}

func seedMemoryForSettling(t *testing.T, db *sql.DB, id string, createdAt, updatedAt int64) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO memories (id, collection, content, is_long_term, created_at, updated_at)
		 VALUES (?, 'memories', 'settling test', 0, ?, ?)`, id, createdAt, updatedAt)
	if err != nil {
		t.Fatalf("seed memory %s: %v", id, err)
	}
}

func readActive(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	got, err := core.ReadActiveUptime(context.Background(), db)
	if err != nil {
		t.Fatalf("read active uptime: %v", err)
	}
	return got
}

// TestAccruer_AccumulatesProcessElapsed pins that a continuously running
// scheduler accrues exactly its own monotonic elapsed time.
func TestAccruer_AccumulatesProcessElapsed(t *testing.T) {
	db := schedulerUptimeDB(t)
	ctx := context.Background()
	clk := &fakeClock{}
	acc := newActiveUptimeAccruer(clk)

	for _, d := range []time.Duration{time.Hour, time.Hour, time.Hour} {
		clk.advance(d)
		if _, err := acc.Accrue(ctx, db); err != nil {
			t.Fatalf("accrue: %v", err)
		}
	}
	if got, want := readActive(t, db), int64(3*3600); got != want {
		t.Errorf("active = %d, want %d", got, want)
	}
}

// TestAccruer_RestartContributesNoWallGap is the central outage property:
// process A's uptime plus process B's uptime, with the wall-clock gap
// between them contributing NOTHING.
func TestAccruer_RestartContributesNoWallGap(t *testing.T) {
	db := schedulerUptimeDB(t)
	ctx := context.Background()

	// Process A lives 2h.
	clkA := &fakeClock{}
	accA := newActiveUptimeAccruer(clkA)
	clkA.advance(2 * time.Hour)
	if _, err := accA.Accrue(ctx, db); err != nil {
		t.Fatalf("process A: %v", err)
	}

	// Process A dies. 20h of wall time pass. Nothing is written.

	// Process B starts fresh: elapsed restarts at 0.
	clkB := &fakeClock{}
	accB := newActiveUptimeAccruer(clkB)
	if got := readActive(t, db); got != 2*3600 {
		t.Fatalf("after A: active = %d, want %d", got, 2*3600)
	}
	// B's first tick, immediately after start, contributes nothing.
	if _, err := accB.Accrue(ctx, db); err != nil {
		t.Fatalf("process B first tick: %v", err)
	}
	// B then lives 10h.
	clkB.advance(10 * time.Hour)
	if _, err := accB.Accrue(ctx, db); err != nil {
		t.Fatalf("process B: %v", err)
	}

	if got, want := readActive(t, db), int64(12*3600); got != want {
		t.Errorf("active = %d, want %d (2h + 10h; the 20h wall gap must contribute 0)", got, want)
	}
}

// TestAccruer_UnpersistedTailIsLost pins that a crash loses the tail
// since the last persisted tick and never backfills it.
func TestAccruer_UnpersistedTailIsLost(t *testing.T) {
	db := schedulerUptimeDB(t)
	ctx := context.Background()

	clk := &fakeClock{}
	acc := newActiveUptimeAccruer(clk)
	clk.advance(time.Hour)
	if _, err := acc.Accrue(ctx, db); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// The process runs another 30 minutes and CRASHES before accruing.
	clk.advance(30 * time.Minute)

	// A new process starts and contributes only its own span.
	clk2 := &fakeClock{}
	acc2 := newActiveUptimeAccruer(clk2)
	clk2.advance(10 * time.Minute)
	if _, err := acc2.Accrue(ctx, db); err != nil {
		t.Fatalf("post-crash: %v", err)
	}

	if got, want := readActive(t, db), int64((70*time.Minute)/time.Second); got != want {
		t.Errorf("active = %d, want %d (the 30m unpersisted tail must be lost, not reconstructed)", got, want)
	}
}

// TestAccruer_DoesNotDecrementOnBackwardsClock pins that an elapsed
// source moving backwards cannot decrement the durable counter.
func TestAccruer_DoesNotDecrementOnBackwardsClock(t *testing.T) {
	db := schedulerUptimeDB(t)
	ctx := context.Background()
	clk := &fakeClock{}
	acc := newActiveUptimeAccruer(clk)

	clk.advance(2 * time.Hour)
	if _, err := acc.Accrue(ctx, db); err != nil {
		t.Fatalf("forward: %v", err)
	}
	// Elapsed source jumps backwards (the analogue of a wall-clock jump
	// reaching a monotonic-independent reader).
	clk.at = time.Hour
	if _, err := acc.Accrue(ctx, db); err != nil {
		t.Fatalf("backwards: %v", err)
	}
	if got, want := readActive(t, db), int64(2*3600); got != want {
		t.Errorf("active = %d, want %d (a backwards clock must never decrement)", got, want)
	}
}

// TestAccruer_CapturesMissingBaselinesAtCurrentTotal pins the population
// ordering: baselines anchor to the POST-accrual total, so a memory is
// never credited for time before the scheduler observed it.
func TestAccruer_CapturesMissingBaselinesAtCurrentTotal(t *testing.T) {
	db := schedulerUptimeDB(t)
	ctx := context.Background()
	seedMemoryForSettling(t, db, "admitted", 1000, 1000)

	clk := &fakeClock{}
	acc := newActiveUptimeAccruer(clk)
	clk.advance(5 * time.Hour)
	total, err := acc.Accrue(ctx, db)
	if err != nil {
		t.Fatalf("accrue: %v", err)
	}

	var baseline int64
	if err := db.QueryRowContext(ctx,
		`SELECT baseline_active_seconds FROM memory_settling_baselines WHERE memory_id = 'admitted'`).
		Scan(&baseline); err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	if baseline != total {
		t.Errorf("baseline = %d, want %d (must anchor to the post-accrual total)", baseline, total)
	}

	age, _, err := core.SettlingAgeSeconds(ctx, db, "admitted", total)
	if err != nil {
		t.Fatalf("age: %v", err)
	}
	if age != 0 {
		t.Errorf("age = %d, want 0 for a memory first observed on this very tick", age)
	}
}

// TestAccruer_DistinctAdmissionTimesSettleInOrder pins that two memories
// admitted at different scheduler times hold different baselines.
func TestAccruer_DistinctAdmissionTimesSettleInOrder(t *testing.T) {
	db := schedulerUptimeDB(t)
	ctx := context.Background()
	clk := &fakeClock{}
	acc := newActiveUptimeAccruer(clk)

	seedMemoryForSettling(t, db, "first", 1000, 1000)
	clk.advance(time.Hour)
	if _, err := acc.Accrue(ctx, db); err != nil {
		t.Fatalf("tick 1: %v", err)
	}

	seedMemoryForSettling(t, db, "second", 2000, 2000)
	clk.advance(time.Hour)
	if _, err := acc.Accrue(ctx, db); err != nil {
		t.Fatalf("tick 2: %v", err)
	}

	// Baselines anchor to the POST-accrual total of the tick that first
	// observed each memory: "first" at 3600, "second" at 7200.
	total := readActive(t, db)
	if total != 2*3600 {
		t.Fatalf("total = %d, want %d", total, 2*3600)
	}
	firstAge, _, err := core.SettlingAgeSeconds(ctx, db, "first", total)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	secondAge, _, err := core.SettlingAgeSeconds(ctx, db, "second", total)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if firstAge != 3600 || secondAge != 0 {
		t.Errorf("ages: first=%d second=%d, want %d and %d (first was admitted one active-hour earlier)",
			firstAge, secondAge, 3600, 0)
	}
}

// TestAccruer_BaselineSurvivesRestart pins that a baseline remains valid
// against the CUMULATIVE counter after a restart — it is anchored to the
// global total, not to a per-process quantity.
func TestAccruer_BaselineSurvivesRestart(t *testing.T) {
	db := schedulerUptimeDB(t)
	ctx := context.Background()
	seedMemoryForSettling(t, db, "resident", 1000, 1000)

	clkA := &fakeClock{}
	accA := newActiveUptimeAccruer(clkA)
	clkA.advance(6 * time.Hour)
	if _, err := accA.Accrue(ctx, db); err != nil {
		t.Fatalf("A: %v", err)
	}

	// Restart with a long wall gap.
	clkB := &fakeClock{}
	accB := newActiveUptimeAccruer(clkB)
	clkB.advance(6 * time.Hour)
	if _, err := accB.Accrue(ctx, db); err != nil {
		t.Fatalf("B: %v", err)
	}

	total := readActive(t, db)
	if want := int64(12 * 3600); total != want {
		t.Fatalf("total = %d, want %d", total, want)
	}
	// The memory was seeded before any accrual, so process A's first tick
	// stamped it at the post-accrual total of 21600. Its residency is
	// therefore 43200-21600 = 21600 — the 6h process B contributed, not
	// the full cumulative total. This is the property that distinguishes
	// a per-memory baseline from reading the global total directly.
	age, ok, err := core.SettlingAgeSeconds(ctx, db, "resident", total)
	if err != nil || !ok {
		t.Fatalf("baseline lost across restart: ok=%v err=%v", ok, err)
	}
	if want := int64(6 * 3600); age != want {
		t.Errorf("age = %d, want %d (baseline anchored at 21600, not reset across restart)", age, want)
	}
}

// TestAccruer_FailClosedOnMalformedGlobalState pins that corrupt owned
// state stops accrual rather than being reset, and that the corrupt row
// is left untouched.
func TestAccruer_FailClosedOnMalformedGlobalState(t *testing.T) {
	db := schedulerUptimeDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO system_config (key, raw_json, content_hash) VALUES (?, '{"active_seconds":-5}', '')`,
		core.ActiveUptimeKey); err != nil {
		t.Fatalf("seed malformed: %v", err)
	}

	clk := &fakeClock{}
	acc := newActiveUptimeAccruer(clk)
	clk.advance(time.Hour)
	if _, err := acc.Accrue(ctx, db); err == nil {
		t.Fatal("expected accrual to fail closed on malformed state, got nil")
	}

	var raw string
	if err := db.QueryRowContext(ctx,
		`SELECT raw_json FROM system_config WHERE key = ?`, core.ActiveUptimeKey).Scan(&raw); err != nil {
		t.Fatalf("reread: %v", err)
	}
	if raw != `{"active_seconds":-5}` {
		t.Errorf("malformed state was rewritten: %q", raw)
	}
}

// TestAccruer_RetriesUnaccruedIntervalAfterFailure pins that a failed
// accrual does not advance lastElapsed — the unaccrued interval is
// retried rather than silently dropped.
func TestAccruer_RetriesUnaccruedIntervalAfterFailure(t *testing.T) {
	db := schedulerUptimeDB(t)
	ctx := context.Background()
	// Malformed state makes accrual fail.
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO system_config (key, raw_json, content_hash) VALUES (?, '{"active_seconds":"nope"}', '')`,
		core.ActiveUptimeKey); err != nil {
		t.Fatalf("seed: %v", err)
	}

	clk := &fakeClock{}
	acc := newActiveUptimeAccruer(clk)
	clk.advance(time.Hour)
	if _, err := acc.Accrue(ctx, db); err == nil {
		t.Fatal("expected failure")
	}

	// Repair the row, then let one more hour pass.
	if _, err := db.ExecContext(ctx,
		`UPDATE system_config SET raw_json = '{"active_seconds":0}' WHERE key = ?`,
		core.ActiveUptimeKey); err != nil {
		t.Fatalf("repair: %v", err)
	}
	clk.advance(time.Hour)
	if _, err := acc.Accrue(ctx, db); err != nil {
		t.Fatalf("post-repair: %v", err)
	}
	// Both hours are credited: the failed tick's interval was retried.
	if got, want := readActive(t, db), int64(2*3600); got != want {
		t.Errorf("active = %d, want %d (the failed tick's interval must be retried, not dropped)", got, want)
	}
}

// TestTick_AccruesBeforeDispatchingWakes pins the ordering requirement:
// the active counter and baselines are updated at the START of Tick,
// before any wake is queried or dispatched.
func TestTick_AccruesBeforeDispatchingWakes(t *testing.T) {
	db := schedulerUptimeDB(t)
	ctx := context.Background()
	seedMemoryForSettling(t, db, "observed", 1000, 1000)

	s, err := New(db, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Inject a controllable clock so the tick accrues a known span.
	clk := &fakeClock{at: 3 * time.Hour}
	s.activeUptime = newActiveUptimeAccruer(clk)

	// Register a handler that observes the state DURING dispatch — it
	// must already see the accrued counter and the populated baseline.
	var (
		sawActive   int64
		sawBaseline sql.NullInt64
	)
	s.Register("critic_audit", func(context.Context, Wake) error {
		sawActive, _ = core.ReadActiveUptime(ctx, db)
		return db.QueryRowContext(ctx,
			`SELECT baseline_active_seconds FROM memory_settling_baselines WHERE memory_id = 'observed'`).
			Scan(&sawBaseline)
	})

	due := time.Now().Unix()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO scheduled_wakes (id, reason, target_time, fired, created_by, metadata)
		 VALUES ('w1','test',?,0,'test','{"kind":"critic_audit"}')`, due); err != nil {
		t.Fatalf("seed wake: %v", err)
	}

	if _, err := s.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if sawActive != 3*3600 {
		t.Errorf("handler observed active = %d, want %d (accrual must precede dispatch)", sawActive, 3*3600)
	}
	if !sawBaseline.Valid || sawBaseline.Int64 != 3*3600 {
		t.Errorf("handler observed baseline = %v, want 10800 (baselines must precede dispatch)", sawBaseline)
	}
}

// TestTick_AccrualFailureDoesNotBlockWakes pins that a tick which cannot
// account for uptime still delivers ordinary wake processing.
func TestTick_AccrualFailureDoesNotBlockWakes(t *testing.T) {
	db := schedulerUptimeDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO system_config (key, raw_json, content_hash) VALUES (?, '{"active_seconds":{}}', '')`,
		core.ActiveUptimeKey); err != nil {
		t.Fatalf("seed malformed: %v", err)
	}

	s, err := New(db, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clk := &fakeClock{at: time.Hour}
	s.activeUptime = newActiveUptimeAccruer(clk)

	var handled bool
	s.Register("critic_audit", func(context.Context, Wake) error {
		handled = true
		return nil
	})
	if _, err := db.ExecContext(ctx,
		`INSERT INTO scheduled_wakes (id, reason, target_time, fired, created_by, metadata)
		 VALUES ('w2','test',?,0,'test','{"kind":"critic_audit"}')`,
		time.Now().Unix()); err != nil {
		t.Fatalf("seed wake: %v", err)
	}

	if _, err := s.Tick(ctx); err != nil {
		t.Fatalf("a failed accrual must not fail the tick: %v", err)
	}
	if !handled {
		t.Error("wake handler did not run despite accrual failure")
	}
}
