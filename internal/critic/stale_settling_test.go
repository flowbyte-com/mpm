package critic

// Tests for StaleMemoryHunt's cumulative-scheduler-active settling.
//
// Two independent clocks, both required:
//
//	MaxAge         wall clock   — "is this stale?"
//	SettlingPeriod ACTIVE uptime — "has the system been up long enough
//	                                 to judge it fairly?"
//
// The historical defect this file pins: the settling cutoff used to be
// `a.CycleStart().Add(-settling)`, where CycleStart is time.Now() inside
// a ONE-SHOT process. That is identically `now - settling`, so any
// scheduler outage accrued settling credit. The in-code comment claimed
// the opposite. TestCriticSettling_OutageDoesNotSettle is the regression.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	mpmcore "github.com/flowbyte-com/mpm-core"
)

// quietLogger suppresses hunt logging so failure output stays readable.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// settlingDB builds a hermetic critic DB. criticTestSchema already
// carries memories, system_config, and the core-owned
// memory_settling_baselines relation this hunt reads.
func settlingDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settling.db")
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(criticTestSchema); err != nil {
		t.Fatalf("install schema: %v", err)
	}
	return db
}

// seedStaleMemory inserts a wall-old, challengeable-on-MaxAge memory.
func seedStaleMemory(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()
	if _, err := db.Exec(
		`INSERT INTO memories (id, collection, content, is_long_term, created_at, updated_at)
		 VALUES (?, 'memories', 'stale content', 0, ?, ?)`,
		id, old, old); err != nil {
		t.Fatalf("seed memory %s: %v", id, err)
	}
}

func setGlobalActive(t *testing.T, db *sql.DB, seconds int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT OR REPLACE INTO system_config (key, raw_json, content_hash) VALUES (?, ?, '')`,
		mpmcore.ActiveUptimeKey,
		`{"active_seconds":`+itoa(seconds)+`,"last_elapsed_ms":0,"updated_at":0}`); err != nil {
		t.Fatalf("set active uptime: %v", err)
	}
}

func setBaseline(t *testing.T, db *sql.DB, memoryID string, seconds int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT OR REPLACE INTO memory_settling_baselines (memory_id, baseline_active_seconds, captured_at)
		 VALUES (?, ?, 0)`, memoryID, seconds); err != nil {
		t.Fatalf("set baseline: %v", err)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// runStaleHunt executes the hunt against a minimal Audit.
func runStaleHunt(t *testing.T, db *sql.DB, settling time.Duration) ([]Finding, error) {
	t.Helper()
	log := quietLogger()
	a := &Audit{db: db, log: log, cli: &fakeCLI{}, cycle: 1, cycleStart: time.Now()}
	h := &StaleMemoryHunt{MaxAge: 30 * 24 * time.Hour, SettlingPeriod: settling}
	return h.Run(context.Background(), a)
}

// ── The historical defect ───────────────────────────────────────────────

// TestCriticSettling_OutageDoesNotSettle is the primary regression.
//
// A memory is wall-old enough for MaxAge, and 20h of scheduler downtime
// has elapsed. Only 6h of CUMULATIVE ACTIVE uptime exists. The old
// implementation compared against `now - settling`, so the outage
// counted and the memory was challenged. It must not be.
func TestCriticSettling_OutageDoesNotSettle(t *testing.T) {
	db := settlingDB(t)
	seedStaleMemory(t, db, "outage")

	// 6h of real active uptime has ever accumulated.
	setGlobalActive(t, db, 6*3600)
	// The memory was admitted when the counter stood at 0.
	setBaseline(t, db, "outage", 0)

	findings, err := runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("memory challenged with only 6h of active uptime; a 20h+ wall outage must not settle it (got %d findings)", len(findings))
	}
}

// TestCriticSettling_SettlesAfterEnoughActiveTime is the positive
// counterpart: once enough scheduler-active time has genuinely accrued,
// the same memory IS challengeable.
func TestCriticSettling_SettlesAfterEnoughActiveTime(t *testing.T) {
	db := settlingDB(t)
	seedStaleMemory(t, db, "settled")

	setGlobalActive(t, db, 12*3600)
	setBaseline(t, db, "settled", 0)

	findings, err := runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1 (12h active uptime satisfies the 12h settling period)", len(findings))
	}
}

// ── Boundary conditions (Section 15) ────────────────────────────────────

// TestCriticSettling_Boundary pins the INCLUSIVE comparator exactly.
// The same memory, with only the global counter varying.
func TestCriticSettling_Boundary(t *testing.T) {
	const settling = 12 * time.Hour
	const want = int64(12 * 3600)

	for _, tc := range []struct {
		name      string
		active    int64
		challenge bool
	}{
		{"one second below the period", want - 1, false},
		{"exactly the period", want, true},
		{"one second above the period", want + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := settlingDB(t)
			seedStaleMemory(t, db, "edge")
			setGlobalActive(t, db, tc.active)
			setBaseline(t, db, "edge", 0)

			findings, err := runStaleHunt(t, db, settling)
			if err != nil {
				t.Fatalf("hunt: %v", err)
			}
			got := len(findings) == 1
			if got != tc.challenge {
				t.Errorf("active=%d challengeable=%v, want %v", tc.active, got, tc.challenge)
			}
		})
	}
}

// ── Legacy and imported memories (Sections 7, 8) ────────────────────────

// TestCriticSettling_LegacyMemoryGetsNoHistoricalCredit pins the rollout
// policy: a 90-day-old memory with no baseline is NOT settled, no matter
// how much wall age it carries.
func TestCriticSettling_LegacyMemoryGetsNoHistoricalCredit(t *testing.T) {
	db := settlingDB(t)
	seedStaleMemory(t, db, "legacy")
	// A large amount of active uptime exists, but no baseline row: the
	// scheduler has never observed this memory.
	setGlobalActive(t, db, 999*3600)

	findings, err := runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("legacy memory challenged with no baseline; unknown residency must not read as settled")
	}
}

// TestCriticSettling_FreshImportWithOldTimestampsProtected pins that an
// import cannot bypass settling by carrying old timestamps. The memory is
// brand new to the system but its created_at/updated_at are ancient.
func TestCriticSettling_FreshImportWithOldTimestampsProtected(t *testing.T) {
	db := settlingDB(t)
	seedStaleMemory(t, db, "imported")

	// 30 minutes of active uptime, baseline stamped moments ago.
	setGlobalActive(t, db, 1800)
	setBaseline(t, db, "imported", 1500)

	findings, err := runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("freshly imported memory challenged after 5 minutes of active uptime; old timestamps must not bypass settling")
	}
}

// TestCriticSettling_HuntIsReadOnly pins that merely EVALUATING the hunt
// mutates nothing. In particular the Critic must not create a missing
// baseline for a memory it has no authority over.
func TestCriticSettling_HuntIsReadOnly(t *testing.T) {
	db := settlingDB(t)
	seedStaleMemory(t, db, "unobserved")
	setGlobalActive(t, db, 5000)

	if _, err := runStaleHunt(t, db, 12*time.Hour); err != nil {
		t.Fatalf("hunt: %v", err)
	}

	var baselines int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_settling_baselines`).Scan(&baselines); err != nil {
		t.Fatalf("count baselines: %v", err)
	}
	if baselines != 0 {
		t.Errorf("critic created %d baseline rows; StaleMemoryHunt must remain read-only", baselines)
	}
}

// TestCriticSettling_DifferentAdmissionTimes pins that settling is
// measured PER MEMORY from its own admission baseline, not from the
// global counter alone.
//
// Both memories are wall-stale and share the same global counter, so
// the ONLY thing distinguishing them is their baseline:
//   - "resident" admitted at 0     -> 12h of residency -> challengeable
//   - "newcomer" admitted at 8h    ->  4h of residency -> NOT challengeable
//
// A mutation that replaced the per-memory difference with the raw global
// total would challenge both, which is exactly what Probe D checks.
func TestCriticSettling_DifferentAdmissionTimes(t *testing.T) {
	db := settlingDB(t)
	seedStaleMemory(t, db, "resident")
	seedStaleMemory(t, db, "newcomer")

	setGlobalActive(t, db, 12*3600)
	setBaseline(t, db, "resident", 0)      // 12h of residency
	setBaseline(t, db, "newcomer", 8*3600) // 4h of residency

	findings, err := runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want exactly 1: only the memory admitted at 0 has 12h of residency", len(findings))
	}
	if id, _ := findings[0].Payload["memoryId"].(string); id != "resident" {
		t.Errorf("challenged %q, want \"resident\" (the 12h-resident memory)", id)
	}
}

// TestCriticSettling_RestartDoesNotExtendResidency pins that a scheduler
// restart contributes no settling credit. The global counter is already
// at 12h, but the memory was admitted when it stood at 0 AND the 20h
// wall gap since must not have moved it — so a memory admitted before
// the gap keeps exactly the residency the counter recorded.
func TestCriticSettling_RestartDoesNotExtendResidency(t *testing.T) {
	db := settlingDB(t)
	seedStaleMemory(t, db, "survivor")

	// Process A accrued 12h, then died. 20h of wall downtime passed.
	// The counter is unmoved by that gap.
	setGlobalActive(t, db, 12*3600)
	setBaseline(t, db, "survivor", 0)

	findings, err := runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1 (12h of recorded active uptime satisfies the period)", len(findings))
	}
}

// ── Reinforcement (Section 9) ───────────────────────────────────────────

// TestCriticSettling_ReinforcementDoesNotResetSettling pins that the
// baseline is admission residency, not a cooldown after reinforcement.
// Reinforcing a long-resident memory bumps updated_at (making it fresh
// for MaxAge) but must NOT extend its settling requirement — and once
// settled, a reinforcement that leaves updated_at old again must not
// make the memory unsettle.
func TestCriticSettling_ReinforcementDoesNotResetSettling(t *testing.T) {
	db := settlingDB(t)
	seedStaleMemory(t, db, "reinforced")

	// Admitted at 0; 20h of active uptime has since accrued.
	setGlobalActive(t, db, 20*3600)
	setBaseline(t, db, "reinforced", 0)

	// Reinforce: updated_at moves to now, created_at stays ancient.
	if _, err := db.Exec(
		`UPDATE memories SET updated_at = ? WHERE id = 'reinforced'`,
		time.Now().Unix()); err != nil {
		t.Fatalf("reinforce: %v", err)
	}
	findings, err := runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("reinforced memory challenged; MaxAge is wall-clock recency and a fresh updated_at must exclude it")
	}

	// Reinforcement did NOT rewrite the baseline.
	var baseline int64
	if err := db.QueryRow(
		`SELECT baseline_active_seconds FROM memory_settling_baselines WHERE memory_id = 'reinforced'`).
		Scan(&baseline); err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	if baseline != 0 {
		t.Errorf("baseline = %d after reinforcement, want 0 (baseline is immutable admission residency)", baseline)
	}

	// Age updated_at back into staleness: the memory is still settled,
	// because residency is unchanged.
	if _, err := db.Exec(
		`UPDATE memories SET updated_at = ? WHERE id = 'reinforced'`,
		time.Now().Add(-90*24*time.Hour).Unix()); err != nil {
		t.Fatalf("age updated_at: %v", err)
	}
	findings, err = runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt 2: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1 (a settled memory stays settled; reinforcement does not unsettle it)", len(findings))
	}
}

// ── Fail-closed on malformed global state (Section 10) ──────────────────

// TestCriticSettling_MalformedGlobalStateFailsClosed pins that corrupt
// active-time state produces NO challenges rather than permissive ones.
func TestCriticSettling_MalformedGlobalStateFailsClosed(t *testing.T) {
	for _, raw := range []string{
		`{"active_seconds":-1}`,
		`{"active_seconds":"3600"}`,
		`{"active_seconds":1.5}`,
		`{}`,
		`{"active_seconds":null}`,
	} {
		t.Run(raw, func(t *testing.T) {
			db := settlingDB(t)
			seedStaleMemory(t, db, "corrupt")
			if _, err := db.Exec(
				`INSERT OR REPLACE INTO system_config (key, raw_json, content_hash) VALUES (?, ?, '')`,
				mpmcore.ActiveUptimeKey, raw); err != nil {
				t.Fatalf("seed malformed: %v", err)
			}
			setBaseline(t, db, "corrupt", 0)

			findings, err := runStaleHunt(t, db, 12*time.Hour)
			if err == nil {
				t.Fatalf("expected an explicit error for malformed state %s, got nil", raw)
			}
			if len(findings) != 0 {
				t.Errorf("hunt returned %d findings despite malformed state; must fail closed", len(findings))
			}
		})
	}
}

// ── Dual-clock contract (Section 11) ───────────────────────────────────

// TestCriticSettling_MaxAgeRemainsWallClock pins that MaxAge is NOT
// converted to active uptime: a memory wall-old past MaxAge but with
// insufficient ACTIVE residency is excluded, and once active residency
// is sufficient it is included — the wall-clock age is unchanged across
// both observations.
func TestCriticSettling_MaxAgeRemainsWallClock(t *testing.T) {
	db := settlingDB(t)
	seedStaleMemory(t, db, "dual")

	// 6h of active uptime: wall-stale but not settled.
	setGlobalActive(t, db, 6*3600)
	setBaseline(t, db, "dual", 0)
	findings, err := runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("memory challenged at 6h active uptime; both gates must be required")
	}

	// The scheduler keeps running; the memory's wall age is unchanged.
	setGlobalActive(t, db, 13*3600)
	findings, err = runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt 2: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1 (wall-stale AND settled => challengeable)", len(findings))
	}
}

// TestCriticSettling_FreshMemoryNotMaxAgeStale pins the other half of the
// dual gate: a memory that is well settled but NOT wall-stale is
// excluded.
func TestCriticSettling_FreshMemoryNotMaxAgeStale(t *testing.T) {
	db := settlingDB(t)
	if _, err := db.Exec(
		`INSERT INTO memories (id, collection, content, is_long_term, created_at, updated_at)
		 VALUES ('fresh','memories','brand new',0,?,?)`,
		time.Now().Unix(), time.Now().Unix()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	setGlobalActive(t, db, 100*3600)
	setBaseline(t, db, "fresh", 0)

	findings, err := runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %d, want 0 (a non-wall-stale memory must never be challenged)", len(findings))
	}
}

// TestCriticSettling_SoftDeletedExcluded keeps the pre-existing soft
// delete guard working alongside the new join.
func TestCriticSettling_SoftDeletedExcluded(t *testing.T) {
	db := settlingDB(t)
	seedStaleMemory(t, db, "deleted")
	setGlobalActive(t, db, 100*3600)
	setBaseline(t, db, "deleted", 0)
	if _, err := db.Exec(
		`UPDATE memories SET deleted_at = ? WHERE id = 'deleted'`, time.Now().Unix()); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	findings, err := runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("soft-deleted memory challenged")
	}
}

// TestCriticSettling_LongTermExcluded keeps the is_long_term guard.
func TestCriticSettling_LongTermExcluded(t *testing.T) {
	db := settlingDB(t)
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()
	if _, err := db.Exec(
		`INSERT INTO memories (id, collection, content, is_long_term, created_at, updated_at)
		 VALUES ('ltm','memories','long term',1,?,?)`, old, old); err != nil {
		t.Fatalf("seed: %v", err)
	}
	setGlobalActive(t, db, 100*3600)
	setBaseline(t, db, "ltm", 0)

	findings, err := runStaleHunt(t, db, 12*time.Hour)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("long-term memory challenged; is_long_term guard regressed")
	}
}

// TestCriticSettling_DefaultSettlingPeriod pins the 12h default is
// interpreted as ACTIVE uptime, not wall clock.
func TestCriticSettling_DefaultSettlingPeriod(t *testing.T) {
	db := settlingDB(t)
	seedStaleMemory(t, db, "defaulted")
	// 11h of active uptime: under the DEFAULT 12h period.
	setGlobalActive(t, db, 11*3600)
	setBaseline(t, db, "defaulted", 0)

	log := quietLogger()
	a := &Audit{db: db, log: log, cli: &fakeCLI{}, cycle: 1, cycleStart: time.Now()}
	// SettlingPeriod left zero to exercise the default.
	findings, err := (&StaleMemoryHunt{MaxAge: 30 * 24 * time.Hour}).Run(context.Background(), a)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %d, want 0 under the default 12h active-uptime settling period", len(findings))
	}
}
