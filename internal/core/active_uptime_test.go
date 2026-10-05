package internal

// Tests for cumulative scheduler-ACTIVE uptime accounting and the
// per-memory settling baselines that anchor to it.
//
// Every test here is hermetic (NewTestDM → isolated in-memory SQLite
// with the full canonical schema) and drives SYNTHETIC elapsed
// durations. None of them sleeps, and none reads a wall clock to decide
// how much uptime to accrue — that separation is the whole point of the
// monotonic model, so a test that conflated the two could not detect the
// defect it exists to prevent.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// seedMemoryForSettling inserts a minimal live memory row.
func seedMemoryForSettling(t *testing.T, dm *DatabaseManager, id string, createdAt, updatedAt int64) {
	t.Helper()
	_, err := dm.SQLDB().ExecContext(context.Background(),
		`INSERT INTO memories (id, collection, content, is_long_term, created_at, updated_at)
		 VALUES (?, 'memories', 'settling test content', 0, ?, ?)`,
		id, createdAt, updatedAt)
	if err != nil {
		t.Fatalf("seed memory %s: %v", id, err)
	}
}

func setActiveUptimeRaw(t *testing.T, dm *DatabaseManager, raw string) {
	t.Helper()
	_, err := dm.SQLDB().ExecContext(context.Background(),
		`INSERT OR REPLACE INTO system_config (key, raw_json, content_hash) VALUES (?, ?, '')`,
		ActiveUptimeKey, raw)
	if err != nil {
		t.Fatalf("seed raw active uptime: %v", err)
	}
}

// ── Pure arithmetic boundary (Section 3 / 13) ─────────────────────────────

// TestActiveUptimeFromElapsed pins the pure accounting helper that turns
// process-monotonic elapsed time into an accrual delta. Synthetic values
// only — no clock, no database, no sleep.
func TestActiveUptimeFromElapsed(t *testing.T) {
	const h = time.Hour
	for _, tc := range []struct {
		name        string
		elapsed     time.Duration
		lastElapsed time.Duration
		want        int64
	}{
		{"first tick of a fresh process", 0, 0, 0},
		{"sub-second first tick truncates to zero", 500 * time.Millisecond, 0, 0},
		{"one minute of process life", time.Minute, 0, 60},
		{"exactly 12h continuous", 12 * h, 0, 12 * 3600},
		{"11h59m59s continuous", 11*h + 59*time.Minute + 59*time.Second, 0, 11*3600 + 59*60 + 59},
		{"12h00m01s continuous", 12*h + time.Second, 0, 12*3600 + 1},
		{"incremental tick contributes only its own slice", 12 * h, 11 * h, 3600},
		{"repeated tick with no elapsed time contributes zero", 12 * h, 12 * h, 0},
		// A backwards elapsed source must never decrement the counter.
		{"elapsed behind lastElapsed clamps to zero", 5 * h, 6 * h, 0},
		{"negative elapsed clamps to zero", -time.Hour, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ActiveUptimeFromElapsed(tc.elapsed, tc.lastElapsed); got != tc.want {
				t.Errorf("ActiveUptimeFromElapsed(%v, %v) = %d, want %d",
					tc.elapsed, tc.lastElapsed, got, tc.want)
			}
		})
	}
}

// ── Global durable counter ───────────────────────────────────────────────

// TestAccrueActiveUptime_BootstrapsFromAbsentRow: an absent row is a
// legitimate first-boot state, not corruption.
func TestAccrueActiveUptime_BootstrapsFromAbsentRow(t *testing.T) {
	dm := NewTestDM(t)
	ctx := context.Background()

	got, err := ReadActiveUptime(ctx, dm.SQLDB())
	if err != nil {
		t.Fatalf("absent row must read as 0, got err: %v", err)
	}
	if got != 0 {
		t.Fatalf("absent row read as %d, want 0", got)
	}

	total, err := AccrueActiveUptime(ctx, dm.SQLDB(), 3600, 3600000)
	if err != nil {
		t.Fatalf("bootstrap accrue: %v", err)
	}
	if total != 3600 {
		t.Errorf("bootstrap total = %d, want 3600", total)
	}
}

// TestAccrueActiveUtime_AccumulatesAcrossCalls pins that repeated
// accruals within one logical process sum correctly.
func TestAccrueActiveUptime_AccumulatesAcrossCalls(t *testing.T) {
	dm := NewTestDM(t)
	ctx := context.Background()

	// Two hours in process A, then ten more in "process A continued".
	for _, delta := range []int64{2 * 3600, 10 * 3600} {
		if _, err := AccrueActiveUptime(ctx, dm.SQLDB(), delta, 0); err != nil {
			t.Fatalf("accrue %d: %v", delta, err)
		}
	}
	got, err := ReadActiveUptime(ctx, dm.SQLDB())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := int64(12 * 3600); got != want {
		t.Errorf("total = %d, want %d", got, want)
	}
}

// TestAccrueActiveUptime_IgnoresWallGapAcrossRestart is the crash/outage
// case: a fresh process contributes only its OWN monotonic span. The
// model expresses this by the caller passing its own elapsed; here we pin
// that a new process starting at elapsed≈0 adds nothing regardless of
// how much wall time has passed since the last write.
func TestAccrueActiveUptime_IgnoresWallGapAcrossRestart(t *testing.T) {
	dm := NewTestDM(t)
	ctx := context.Background()

	// Process A accrues 2h.
	if _, err := AccrueActiveUptime(ctx, dm.SQLDB(), 2*3600, 7200000); err != nil {
		t.Fatalf("process A: %v", err)
	}
	// 20h of wall downtime elapses — nothing writes it.
	// Process B starts; its own elapsed is 10h.
	total, err := AccrueActiveUptime(ctx, dm.SQLDB(), 10*3600, 36000000)
	if err != nil {
		t.Fatalf("process B: %v", err)
	}
	if want := int64(12 * 3600); total != want {
		t.Errorf("total after restart = %d, want %d (the 20h gap must contribute zero)", total, want)
	}
}

// TestAccrueActiveUptime_ClampsNegativeDelta pins that a backwards
// elapsed source cannot decrement the counter.
func TestAccrueActiveUptime_ClampsNegativeDelta(t *testing.T) {
	dm := NewTestDM(t)
	ctx := context.Background()

	if _, err := AccrueActiveUptime(ctx, dm.SQLDB(), 3600, 3600000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	total, err := AccrueActiveUptime(ctx, dm.SQLDB(), -500, 0)
	if err != nil {
		t.Fatalf("negative delta: %v", err)
	}
	if total != 3600 {
		t.Errorf("total = %d, want 3600 (negative delta must not decrement)", total)
	}
}

// TestAccrueActiveUptime_MalformedFailsClosed pins the compatibility
// matrix. Malformed owned state must NEVER be reset to zero — doing so
// would retroactively make every memory look freshly settled — and must
// never be silently repaired.
func TestAccrueActiveUptime_MalformedFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"missing field", `{}`},
		{"json null", `{"active_seconds":null}`},
		{"numeric string", `{"active_seconds":"3600"}`},
		{"real", `{"active_seconds":1.5}`},
		{"negative", `{"active_seconds":-1}`},
		{"large negative", `{"active_seconds":-99999}`},
		{"boolean", `{"active_seconds":true}`},
		{"not an object", `[3600]`},
		{"unparseable", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dm := NewTestDM(t)
			ctx := context.Background()
			setActiveUptimeRaw(t, dm, tc.raw)

			if _, err := AccrueActiveUptime(ctx, dm.SQLDB(), 3600, 3600000); !errors.Is(err, ErrActiveUptimeMalformed) {
				t.Fatalf("accrue over %s: err = %v, want ErrActiveUptimeMalformed", tc.raw, err)
			}
			if _, err := ReadActiveUptime(ctx, dm.SQLDB()); !errors.Is(err, ErrActiveUptimeMalformed) {
				t.Fatalf("read over %s: err = %v, want ErrActiveUptimeMalformed", tc.raw, err)
			}

			// The corrupt row must be left EXACTLY as found.
			var got string
			if err := dm.SQLDB().QueryRowContext(ctx,
				`SELECT raw_json FROM system_config WHERE key = ?`, ActiveUptimeKey).Scan(&got); err != nil {
				t.Fatalf("reread: %v", err)
			}
			if got != tc.raw {
				t.Errorf("malformed state was rewritten: %q -> %q", tc.raw, got)
			}
		})
	}
}

// ── Per-memory settling baselines ────────────────────────────────────────

// TestCaptureBaselines_StampsExistingMemoriesWithCurrentTotal pins the
// legacy-memory policy: existing memories get their baseline from the
// CURRENT counter, never backdated from wall age. They therefore receive
// zero known historical credit.
func TestCaptureBaselines_StampsExistingMemoriesWithCurrentTotal(t *testing.T) {
	dm := NewTestDM(t)
	ctx := context.Background()
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()
	seedMemoryForSettling(t, dm, "legacy", old, old)

	// The counter already sits at 5h of real active time.
	if _, err := AccrueActiveUptime(ctx, dm.SQLDB(), 5*3600, 0); err != nil {
		t.Fatalf("accrue: %v", err)
	}
	n, err := CaptureMissingSettlingBaselines(ctx, dm.SQLDB(), 5*3600)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if n != 1 {
		t.Fatalf("captured %d baselines, want 1", n)
	}

	age, ok, err := SettlingAgeSeconds(ctx, dm.SQLDB(), "legacy", 5*3600)
	if err != nil || !ok {
		t.Fatalf("baseline missing: ok=%v err=%v", ok, err)
	}
	if age != 0 {
		t.Errorf("settling age = %d, want 0 (a 90-day-old memory must get NO historical credit)", age)
	}
}

// TestCaptureBaselines_Idempotent pins that repeated capture never
// duplicates a row and never rewrites an existing baseline.
func TestCaptureBaselines_Idempotent(t *testing.T) {
	dm := NewTestDM(t)
	ctx := context.Background()
	seedMemoryForSettling(t, dm, "m1", 1000, 1000)

	if _, err := CaptureMissingSettlingBaselines(ctx, dm.SQLDB(), 100); err != nil {
		t.Fatalf("first capture: %v", err)
	}
	n, err := CaptureMissingSettlingBaselines(ctx, dm.SQLDB(), 99999)
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if n != 0 {
		t.Errorf("second capture inserted %d rows, want 0", n)
	}

	var count int
	if err := dm.SQLDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_settling_baselines WHERE memory_id = 'm1'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("baseline row count = %d, want 1", count)
	}
	age, _, err := SettlingAgeSeconds(ctx, dm.SQLDB(), "m1", 99999)
	if err != nil {
		t.Fatalf("age: %v", err)
	}
	if want := int64(99999 - 100); age != want {
		t.Errorf("age = %d, want %d (baseline must be immutable, not rewritten to 99999)", age, want)
	}
}

// TestCaptureBaselines_DistinctAdmissionTimes: two memories admitted at
// different scheduler times hold different baselines, so the older
// active resident settles first.
func TestCaptureBaselines_DistinctAdmissionTimes(t *testing.T) {
	dm := NewTestDM(t)
	ctx := context.Background()
	seedMemoryForSettling(t, dm, "older", 1000, 1000)

	if _, err := CaptureMissingSettlingBaselines(ctx, dm.SQLDB(), 100); err != nil {
		t.Fatalf("capture older: %v", err)
	}
	seedMemoryForSettling(t, dm, "newer", 2000, 2000)
	if _, err := CaptureMissingSettlingBaselines(ctx, dm.SQLDB(), 300); err != nil {
		t.Fatalf("capture newer: %v", err)
	}

	olderAge, _, err := SettlingAgeSeconds(ctx, dm.SQLDB(), "older", 300)
	if err != nil {
		t.Fatalf("older age: %v", err)
	}
	newerAge, _, err := SettlingAgeSeconds(ctx, dm.SQLDB(), "newer", 300)
	if err != nil {
		t.Fatalf("newer age: %v", err)
	}
	if olderAge != 200 || newerAge != 0 {
		t.Fatalf("ages: older=%d newer=%d, want 200 and 0", olderAge, newerAge)
	}
}

// TestCaptureBaselines_ExcludesSoftDeleted pins that a soft-deleted
// memory does not receive a new baseline, while an existing baseline
// SURVIVES a soft-delete round trip (admission is not re-granted).
func TestCaptureBaselines_ExcludesSoftDeleted(t *testing.T) {
	dm := NewTestDM(t)
	ctx := context.Background()
	seedMemoryForSettling(t, dm, "soft", 1000, 1000)
	seedMemoryForSettling(t, dm, "gone", 1000, 1000)

	if _, err := CaptureMissingSettlingBaselines(ctx, dm.SQLDB(), 10); err != nil {
		t.Fatalf("initial capture: %v", err)
	}
	if _, err := dm.SQLDB().ExecContext(ctx,
		`UPDATE memories SET deleted_at = ? WHERE id = 'gone'`, time.Now().Unix()); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	// A newly soft-deleted memory must not acquire a baseline.
	seedMemoryForSettling(t, dm, "gone2", 1000, 1000)
	if _, err := dm.SQLDB().ExecContext(ctx,
		`UPDATE memories SET deleted_at = ? WHERE id = 'gone2'`, time.Now().Unix()); err != nil {
		t.Fatalf("soft delete gone2: %v", err)
	}
	if _, err := CaptureMissingSettlingBaselines(ctx, dm.SQLDB(), 500); err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if _, ok, _ := SettlingAgeSeconds(ctx, dm.SQLDB(), "gone2", 500); ok {
		t.Errorf("soft-deleted memory received a baseline")
	}

	// Soft delete / restore preserves the admission baseline.
	if _, err := dm.SQLDB().ExecContext(ctx,
		`UPDATE memories SET deleted_at = NULL WHERE id = 'soft'`); err != nil {
		t.Fatalf("restore: %v", err)
	}
	age, ok, err := SettlingAgeSeconds(ctx, dm.SQLDB(), "soft", 500)
	if err != nil || !ok {
		t.Fatalf("baseline lost across soft-delete round trip: ok=%v err=%v", ok, err)
	}
	if age != 490 {
		t.Errorf("age after restore = %d, want 490 (baseline preserved, not re-stamped)", age)
	}
}

// TestCaptureBaselines_HardDeleteCascades pins the FK contract.
//
// Uses NewTestLocalOnlyDM rather than NewTestDM: cascade enforcement
// needs the production DSN's _foreign_keys=1, which the in-memory test
// DSN does not set. Using NewTestDM here would silently pass with FK
// enforcement OFF, proving nothing — the same trap foreign_keys_test.go
// documents.
func TestCaptureBaselines_HardDeleteCascades(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	ctx := context.Background()
	seedMemoryForSettling(t, dm, "hard", 1000, 1000)
	if _, err := CaptureMissingSettlingBaselines(ctx, dm.SQLDB(), 10); err != nil {
		t.Fatalf("capture: %v", err)
	}

	// Guard the guard: prove FK enforcement is actually live, so a pass
	// below means the cascade fired rather than that FKs were off.
	var fk int
	if err := dm.SQLDB().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys pragma: %v", err)
	}
	if fk != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, want 1; this test cannot observe the cascade otherwise", fk)
	}

	if _, err := dm.SQLDB().ExecContext(ctx, `DELETE FROM memories WHERE id = 'hard'`); err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	var count int
	if err := dm.SQLDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_settling_baselines WHERE memory_id = 'hard'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("baseline rows after hard delete = %d, want 0 (ON DELETE CASCADE)", count)
	}
}

// TestCaptureBaselines_Batched pins the bounded batch loop drains a set
// larger than one batch without dropping or duplicating rows.
func TestCaptureBaselines_Batched(t *testing.T) {
	dm := NewTestDM(t)
	ctx := context.Background()
	const n = 1200
	for i := 0; i < n; i++ {
		seedMemoryForSettling(t, dm, "bulk-"+string(rune('a'+i%26))+timeKey(i), 1000, 1000)
	}
	total, err := CaptureMissingSettlingBaselines(ctx, dm.SQLDB(), 42)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if total != n {
		t.Errorf("captured %d, want %d", total, n)
	}
	var count int
	if err := dm.SQLDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_settling_baselines`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != n {
		t.Errorf("baseline rows = %d, want %d", count, n)
	}
}

func timeKey(i int) string {
	return "-" + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10))
}

// TestSettlingAgeSeconds_MissingBaseline pins the "unknown is not
// settled" rule.
func TestSettlingAgeSeconds_MissingBaseline(t *testing.T) {
	dm := NewTestDM(t)
	ctx := context.Background()
	seedMemoryForSettling(t, dm, "unseen", 1000, 1000)

	age, ok, err := SettlingAgeSeconds(ctx, dm.SQLDB(), "unseen", 999999)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Errorf("hasBaseline = true for a memory the scheduler never observed")
	}
	if age != 0 {
		t.Errorf("age = %d, want 0 when no baseline exists", age)
	}
}

// TestSettlingAgeSeconds_ClampsNegativeDifference pins that a baseline
// ahead of the counter can never produce a negative age.
func TestSettlingAgeSeconds_ClampsNegativeDifference(t *testing.T) {
	dm := NewTestDM(t)
	ctx := context.Background()
	seedMemoryForSettling(t, dm, "ahead", 1000, 1000)
	if _, err := CaptureMissingSettlingBaselines(ctx, dm.SQLDB(), 5000); err != nil {
		t.Fatalf("capture: %v", err)
	}
	age, ok, err := SettlingAgeSeconds(ctx, dm.SQLDB(), "ahead", 100)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if age != 0 {
		t.Errorf("age = %d, want 0 (negative difference clamped)", age)
	}
}

// TestActiveUptimeStateShape pins the durable representation so a future
// change cannot silently alter the stored format that guards the
// malformed-state policy.
func TestActiveUptimeStateShape(t *testing.T) {
	dm := NewTestDM(t)
	ctx := context.Background()
	if _, err := AccrueActiveUptime(ctx, dm.SQLDB(), 1234, 5678); err != nil {
		t.Fatalf("accrue: %v", err)
	}
	var raw string
	if err := dm.SQLDB().QueryRowContext(ctx,
		`SELECT raw_json FROM system_config WHERE key = ?`, ActiveUptimeKey).Scan(&raw); err != nil {
		t.Fatalf("read: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		t.Fatalf("stored state is not JSON: %v", err)
	}
	if probe["active_seconds"] != float64(1234) {
		t.Errorf("active_seconds = %v, want 1234", probe["active_seconds"])
	}
	if probe["last_elapsed_ms"] != float64(5678) {
		t.Errorf("last_elapsed_ms = %v, want 5678 (diagnostic field)", probe["last_elapsed_ms"])
	}
	if _, ok := probe["updated_at"]; !ok {
		t.Errorf("updated_at missing from stored state: %s", raw)
	}
}
