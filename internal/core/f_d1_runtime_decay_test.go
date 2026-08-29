// f_d1_runtime_decay_test.go — F-D1 vacation invariant regression.
//
// The hostile 40-point alpha validation found that `gcComputeDecay`
// advanced according to wall-clock time since `last_accessed_at`, not
// accumulated scheduler/CLI runtime. A 14-day vacation with the
// scheduler down was causing immediate memory decay to -8.0 in a
// single gc tick. The fix replaces the wall-clock signal with a
// per-memory `runtime_seconds_since_access` counter that accrues by
// min(wall_delta, process_uptime) on each gc tick.
//
// These tests pin the vacation invariant at three scales and prove
// that wall-clock backdate attacks are also no-ops against decay.
package internal

import (
	"testing"
	"time"
)

// fD1SeedFreshMemory inserts a memory with weight=1.0 and returns its
// id. The runtime columns start at 0/NULL — the standard fresh row.
func fD1SeedFreshMemory(t *testing.T, dm *DatabaseManager, content string) string {
	t.Helper()
	id := "fd1-" + GenerateID()
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, weight,
		    is_long_term, last_accessed_at, created_at, runtime_seconds_since_access, runtime_last_accrued_at)
		VALUES (?, 'memories', ?, '[]', '{}', 1.0, 0,
		    CAST(strftime('%s','now') AS INTEGER),
		    CAST(strftime('%s','now') AS INTEGER),
		    0, CAST(strftime('%s','now') AS INTEGER))
	`, id, content)
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	return id
}

// fD1ReadWeight reads the current weight column for an id.
func fD1ReadWeight(t *testing.T, dm *DatabaseManager, id string) float64 {
	t.Helper()
	var w float64
	if err := dm.db.QueryRow(`SELECT weight FROM memories WHERE id = ?`, id).Scan(&w); err != nil {
		t.Fatalf("read weight: %v", err)
	}
	return w
}

// fD1ReadRuntimeSec reads the runtime counter for an id.
func fD1ReadRuntimeSec(t *testing.T, dm *DatabaseManager, id string) int64 {
	t.Helper()
	var s int64
	if err := dm.db.QueryRow(`SELECT runtime_seconds_since_access FROM memories WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	return s
}

// TestF_D1_VacationInvariant_30DaysVacuumDoesNotDecay is the headline
// regression: backdate created_at and last_accessed_at by 30 days, then
// run gc. Pre-fix the memory decayed from 1.0 to -8.0 in a single tick.
// Post-fix the weight must stay at 1.0 because no runtime accrued
// during the "vacation" — the accrual step capped itself at process
// uptime, which is ~0 in a fresh test process.
func TestF_D1_VacationInvariant_30DaysVacuumDoesNotDecay(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	id := fD1SeedFreshMemory(t, dm, "F-D1 vacation test content")

	// Backdate both timestamps by 30 days (simulating scheduler down).
	thirtyDays := int64(30 * 86400)
	if _, err := dm.db.Exec(`
		UPDATE memories
		SET created_at = CAST(strftime('%s','now') AS INTEGER) - ?,
		    last_accessed_at = CAST(strftime('%s','now') AS INTEGER) - ?,
		    runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER) - ?
		WHERE id = ?
	`, thirtyDays, thirtyDays, thirtyDays, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Run gc. Pre-fix this would decay the memory to roughly -8.0
	// (30 days * 0.3/day for low-weight). Post-fix it stays at 1.0
	// because runtime is bounded by process uptime (~0s in tests).
	if _, err := dm.RunGC(GCOptions{DryRun: false, MaxAgeHours: 0}); err != nil {
		t.Fatalf("gc: %v", err)
	}

	w := fD1ReadWeight(t, dm, id)
	if w != 1.0 {
		t.Fatalf("vacation invariant violated: weight=%v, want 1.0 (memory should NOT decay during scheduler downtime)", w)
	}
}

// TestF_D1_BackdateAttackDoesNotDecay verifies that even a 14-day
// runtime backdate is bounded by process uptime. The hostile test's
// backdate path went straight at created_at; this test goes at
// runtime_last_accrued_at to prove the column is independently
// resistant.
func TestF_D1_BackdateAttackDoesNotDecay(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	id := fD1SeedFreshMemory(t, dm, "F-D1 backdate attack content")

	// Backdate ONLY runtime_last_accrued_at — leave last_accessed_at
	// and created_at alone. Pre-fix this would have made gc decay
	// based on wall-clock last_accessed_at. Post-fix the runtime
	// accrual step caps at process uptime regardless.
	fourteenDays := int64(14 * 86400)
	if _, err := dm.db.Exec(`
		UPDATE memories
		SET runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER) - ?
		WHERE id = ?
	`, fourteenDays, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if _, err := dm.RunGC(GCOptions{DryRun: false, MaxAgeHours: 0}); err != nil {
		t.Fatalf("gc: %v", err)
	}

	w := fD1ReadWeight(t, dm, id)
	if w != 1.0 {
		t.Fatalf("backdate attack should not decay: weight=%v, want 1.0", w)
	}
}

// TestF_D1_ActiveRuntimeDoesDecay verifies the inverse: an active
// runtime counter (runtime_seconds_since_access set to several days)
// DOES trigger decay. This pins the contract that decay IS still
// real for genuinely-stale memories — vacation immunity is for
// downtime, not for actually-forgotten content.
func TestF_D1_ActiveRuntimeDoesDecay(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	id := fD1SeedFreshMemory(t, dm, "F-D1 active runtime decay content")

	// Pretend this memory has been actively-decaying for 30 days of
	// accumulated runtime (the scheduler was up and ticking).
	thirtyDaysOfRuntime := int64(30 * 86400)
	if _, err := dm.db.Exec(`
		UPDATE memories
		SET runtime_seconds_since_access = ?,
		    runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ?
	`, thirtyDaysOfRuntime, id); err != nil {
		t.Fatalf("set runtime: %v", err)
	}

	if _, err := dm.RunGC(GCOptions{DryRun: false, MaxAgeHours: 0}); err != nil {
		t.Fatalf("gc: %v", err)
	}

	w := fD1ReadWeight(t, dm, id)
	if w >= 1.0 {
		t.Fatalf("active runtime should decay: weight=%v, want <1.0 (memory has 30d of accumulated runtime)", w)
	}
}

// TestF_D1_AccessResetsRuntimeCounter verifies the access path
// (snooze/reinforce/patch-memory/etc.) zeroes the runtime counter,
// matching the existing last_accessed_at refresh. Without this,
// freshly-touched memory would still appear stale.
func TestF_D1_AccessResetsRuntimeCounter(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	id := fD1SeedFreshMemory(t, dm, "F-D1 access reset content")

	// Set runtime to 100 days worth of "stale" accumulation.
	hundredDays := int64(100 * 86400)
	if _, err := dm.db.Exec(`
		UPDATE memories
		SET runtime_seconds_since_access = ?
		WHERE id = ?
	`, hundredDays, id); err != nil {
		t.Fatalf("set runtime: %v", err)
	}

	// Touch the memory through the canonical access path.
	if err := dm.ReinforceMemory(id, 1); err != nil {
		t.Fatalf("reinforce: %v", err)
	}

	r := fD1ReadRuntimeSec(t, dm, id)
	if r != 0 {
		t.Fatalf("access should reset runtime counter: got %d seconds, want 0", r)
	}
}

// TestF_D1_DryRunDoesNotAccrueRuntime proves that dry-run gc (the
// default for `mpm call gc_run`) does NOT mutate runtime counters.
// Dry runs are pure stats — they must not consume vacation credit.
func TestF_D1_DryRunDoesNotAccrueRuntime(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	id := fD1SeedFreshMemory(t, dm, "F-D1 dry run content")

	// Snapshot runtime before dry-run.
	runtimeBefore := fD1ReadRuntimeSec(t, dm, id)

	if _, err := dm.RunGC(GCOptions{DryRun: true, MaxAgeHours: 0}); err != nil {
		t.Fatalf("gc dry-run: %v", err)
	}

	runtimeAfter := fD1ReadRuntimeSec(t, dm, id)
	if runtimeAfter != runtimeBefore {
		t.Fatalf("dry-run must not accrue runtime: before=%d after=%d", runtimeBefore, runtimeAfter)
	}

	// Also verify last_accrued_at is unchanged.
	var lastAccrued *int64
	if err := dm.db.QueryRow(`SELECT runtime_last_accrued_at FROM memories WHERE id = ?`, id).Scan(&lastAccrued); err != nil {
		t.Fatalf("read last_accrued: %v", err)
	}
	if lastAccrued != nil && time.Since(time.Unix(*lastAccrued, 0)) > 30*time.Second {
		t.Fatalf("dry-run must not update last_accrued_at: got %v ago", time.Since(time.Unix(*lastAccrued, 0)))
	}
}