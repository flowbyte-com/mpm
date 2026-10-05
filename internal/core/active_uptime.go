package internal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Cumulative ACTIVE scheduler uptime — the clock the Critic's
// SettlingPeriod is measured in.
//
// WHY THIS EXISTS. StaleMemoryHunt originally computed its settling
// cutoff as `a.CycleStart().Add(-settling)`, where CycleStart is
// time.Now() captured inside the one-shot mpm-critic process. That made
// the cutoff identically `now - settling`, i.e. a pure wall-clock age
// that carried no information beyond MaxAge. A scheduler outage of any
// length therefore accrued settling credit, so a memory could be
// challenged after a fraction of the intended residency. The in-code
// comment claimed the opposite ("a 13h daemon outage does NOT
// accumulate against the settling period"); it was false.
//
// WHAT "ACTIVE" MEANS. Active time is the monotonic elapsed duration
// for which the persistent scheduler PROCESS has been alive. It is
// accumulated by the scheduler itself — the only process whose uptime
// is meaningful — and read by the Critic. It is deliberately NOT wall
// clock:
//
//   - restart after an 8h outage contributes 0, because the new process
//     starts at elapsed≈0 and only observes its own monotonic span;
//   - a forward or backward wall-clock jump (NTP step, suspend/resume)
//     contributes nothing, because no wall delta is ever consulted;
//   - a long-running handler in the same process counts as active,
//     because the process genuinely remained alive;
//   - a crash loses the unpersisted tail between ticks, which is a
//     conservative UNDERCOUNT and is never reconstructed from wall time.
//
// OWNERSHIP. The scheduler accrues; the Critic reads. Neither creates
// this state — core owns it, like every other canonical table.

const (
	// ActiveUptimeKey is the system_config key holding the cumulative
	// active-seconds scalar. system_config is the canonical bounded
	// operational-state surface (see cascade_drain.max_intents_per_tick
	// and the Critic's own critic_cycle row for precedent).
	ActiveUptimeKey = "scheduler_active_uptime"
)

// activeUptimeState is the stored shape. active_seconds is the semantic
// source of truth; every other field is diagnostic only and MUST NOT
// participate in settling arithmetic.
type activeUptimeState struct {
	ActiveSeconds int64 `json:"active_seconds"`
	LastElapsedMs int64 `json:"last_elapsed_ms"`
	UpdatedAt     int64 `json:"updated_at"`
}

// ErrActiveUptimeMalformed reports durable active-time state that this
// package refuses to silently repair.
//
// The failure mode being prevented: a corrupt or negative
// active_seconds that gets reset to zero would retroactively make every
// memory look freshly settled, while a silently repaired value would
// hide a bug that may already have granted or denied challenges. Both
// are worse than an explicit, actionable error.
var ErrActiveUptimeMalformed = errors.New("core: scheduler active uptime state is malformed")

// AccrueActiveUptime atomically adds deltaSeconds to the durable
// cumulative active total and returns the new total.
//
// deltaSeconds is a MONOTONIC PROCESS ELAPSED quantity supplied by the
// caller (scheduler); this function never reads a wall clock to decide
// how much to add. Negative deltas are clamped to zero so a backwards
// source can never decrement the counter.
//
// ATOMICITY: one statement. On first run (absent row) it bootstraps at
// deltaSeconds; thereafter it adds via json arithmetic under a WHERE
// guard, so concurrent schedulers serialize on the write lock and can
// never lose an update.
//
// FAIL CLOSED: if the stored state is not a JSON object whose
// active_seconds is a non-negative integer, the DO UPDATE's WHERE is
// false, no row is written, and RETURNING yields nothing — the caller
// gets ErrActiveUptimeMalformed and the corrupt row is left EXACTLY as
// found. The guard is inside the statement rather than a post-read check
// so a rejected claim does not mutate state as a side effect of
// detecting corruption.
func AccrueActiveUptime(ctx context.Context, db *sql.DB, deltaSeconds int64, observedElapsedMs int64) (int64, error) {
	if deltaSeconds < 0 {
		// A backwards elapsed source must never decrement the counter.
		deltaSeconds = 0
	}

	stmt := `
INSERT INTO system_config (key, raw_json, content_hash)
VALUES (?,
        json_object('active_seconds', ?,
                    'last_elapsed_ms', ?,
                    'updated_at', CAST(strftime('%s','now') AS INTEGER)),
        '')
ON CONFLICT(key) DO UPDATE SET
	raw_json = json_set(
		system_config.raw_json,
		'$.active_seconds',
			json_extract(system_config.raw_json, '$.active_seconds') + ?,
		'$.last_elapsed_ms', ?,
		'$.updated_at', CAST(strftime('%s','now') AS INTEGER)
	)
WHERE json_type(system_config.raw_json, '$.active_seconds') = 'integer'
  AND json_extract(system_config.raw_json, '$.active_seconds') >= 0
RETURNING json_extract(raw_json, '$.active_seconds')`

	var total int64
	err := db.QueryRowContext(ctx, stmt,
		ActiveUptimeKey, deltaSeconds, observedElapsedMs,
		deltaSeconds, observedElapsedMs,
	).Scan(&total)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w (key %q): refusing to reset active_seconds to zero, because that "+
			"would retroactively make every memory look freshly settled. Inspect or repair the row manually",
			ErrActiveUptimeMalformed, ActiveUptimeKey)
	}
	if err != nil {
		// SQLite raises its own "malformed JSON" error while evaluating
		// the WHERE guard when the stored row is not valid JSON. That is
		// a corrupt owned row, so it must surface as the same fail-closed
		// error as a type mismatch — not as a generic query failure that
		// invites a caller to treat it as transient and retry.
		if strings.Contains(err.Error(), "malformed JSON") {
			return 0, fmt.Errorf("%w (key %q): stored raw_json is not valid JSON",
				ErrActiveUptimeMalformed, ActiveUptimeKey)
		}
		return 0, fmt.Errorf("core: accrue active uptime: %w", err)
	}
	return total, nil
}

// ReadActiveUptime returns the durable cumulative active-seconds total.
//
// An ABSENT row is a legitimate first-boot state and reads as 0 — no
// scheduler has ever accrued time yet. A PRESENT but malformed row is
// an error, never a silent zero, for the same reason the accrual guard
// fails closed.
func ReadActiveUptime(ctx context.Context, db *sql.DB) (int64, error) {
	var raw string
	err := db.QueryRowContext(ctx,
		`SELECT raw_json FROM system_config WHERE key = ?`, ActiveUptimeKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("core: read active uptime: %w", err)
	}

	// Validate with SQLite's own json_type rather than a Go decoder, so
	// the READ path and the accrual WHERE guard agree on exactly what
	// "a non-negative integer" means. A Go decoder with json.Number
	// accepts the JSON string "3600"; json_type reports 'text' and
	// rejects it. Sharing one definition is what keeps the read path
	// from being more permissive than the write path — a divergence
	// there would let a value be readable but un-accruable.
	//
	// The value scans through `any`, not an integer type: json_extract
	// yields NULL for a missing/null path, an int64 for integers, and a
	// float64 for JSON reals. Scanning a float64 into sql.NullInt64
	// fails in the DRIVER with a conversion error before any of our own
	// checks run, which would surface a malformed row as a generic read
	// failure. `any` accepts every shape so the json_type check below is
	// the sole authority on validity.
	var jsonType string
	var value any
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(json_type(?, '$.active_seconds'), ''), json_extract(?, '$.active_seconds')`,
		raw, raw).Scan(&jsonType, &value)
	if err != nil {
		if strings.Contains(err.Error(), "malformed JSON") {
			return 0, fmt.Errorf("%w (key %q): raw_json=%q is not valid JSON",
				ErrActiveUptimeMalformed, ActiveUptimeKey, raw)
		}
		return 0, fmt.Errorf("core: read active uptime: %w", err)
	}
	if jsonType != "integer" {
		return 0, fmt.Errorf("%w (key %q): active_seconds must be a non-negative integer, got json_type=%q in raw_json=%q",
			ErrActiveUptimeMalformed, ActiveUptimeKey, jsonType, raw)
	}
	n, ok := value.(int64)
	if !ok || n < 0 {
		return 0, fmt.Errorf("%w (key %q): active_seconds=%v is not a non-negative integer in raw_json=%q",
			ErrActiveUptimeMalformed, ActiveUptimeKey, value, raw)
	}
	return n, nil
}

// CaptureMissingSettlingBaselines stamps a settling baseline for every
// live memory that does not yet have one, using currentActiveSeconds as
// the anchor, and returns how many rows were inserted.
//
// IDEMPOTENT and safe under concurrency: INSERT ... ON CONFLICT DO
// NOTHING, so a memory inserted between the scan and the insert simply
// waits for the next tick. It can never produce a duplicate row, a
// negative settling age, or lost credit.
//
// IMMUTABILITY: there is deliberately NO update path. A baseline records
// admission residency; reinforcement, weight adjustment, recall, and
// challenge resolution must not restart SettlingPeriod, so nothing here
// ever rewrites an existing row.
//
// This is the ONLY writer of memory_settling_baselines. The Critic
// never calls it — a read-only evaluation that mutated per-memory state
// would make the hunt's cost depend on how often it runs.
func CaptureMissingSettlingBaselines(ctx context.Context, db *sql.DB, currentActiveSeconds int64) (int64, error) {
	if currentActiveSeconds < 0 {
		return 0, fmt.Errorf("core: capture settling baselines: negative active uptime %d", currentActiveSeconds)
	}

	// Bounded scan: memories are processed in id order in batches so a
	// large database does not hold one long write transaction.
	const batch = 500
	var total int64
	for {
		res, err := db.ExecContext(ctx, `
INSERT INTO memory_settling_baselines (memory_id, baseline_active_seconds, captured_at)
SELECT m.id, ?, CAST(strftime('%s','now') AS INTEGER)
FROM memories m
WHERE m.deleted_at IS NULL
  AND NOT EXISTS (
	SELECT 1 FROM memory_settling_baselines b WHERE b.memory_id = m.id
  )
ORDER BY m.id
LIMIT ?`, currentActiveSeconds, batch)
		if err != nil {
			return total, fmt.Errorf("core: capture settling baselines: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("core: capture settling baselines rows affected: %w", err)
		}
		total += n
		if n < batch {
			return total, nil
		}
	}
}

// SettlingAgeSeconds returns the cumulative ACTIVE residency of a memory
// relative to the supplied current active total, and whether the memory
// has a baseline at all.
//
// A memory with no baseline is reported settled=false: it was never
// observed by a scheduler, so its residency is unknown, and unknown must
// not be treated as sufficient. The returned value is clamped at zero so
// a stale baseline can never produce a negative age.
func SettlingAgeSeconds(ctx context.Context, db *sql.DB, memoryID string, currentActiveSeconds int64) (age int64, hasBaseline bool, err error) {
	var baseline int64
	err = db.QueryRowContext(ctx,
		`SELECT baseline_active_seconds FROM memory_settling_baselines WHERE memory_id = ?`,
		memoryID).Scan(&baseline)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("core: read settling baseline: %w", err)
	}
	if baseline < 0 {
		return 0, false, fmt.Errorf("core: settling baseline for %q is negative (%d)", memoryID, baseline)
	}
	age = currentActiveSeconds - baseline
	if age < 0 {
		// Defensive: current < baseline would mean the counter went
		// backwards, which the accrual guard prevents. Clamp rather than
		// let a negative difference read as enormous eligibility.
		age = 0
	}
	return age, true, nil
}

// ActiveUptimeFromElapsed converts a monotonic process elapsed duration
// into the integer seconds to accrue, given what has already been
// accrued for THIS process.
//
// This is the pure accounting boundary the scheduler drives and tests
// synthesize: given processStart/lastElapsed bookkeeping, it yields the
// delta for one tick. It exists separately from AccrueActiveUptime so
// the arithmetic is testable without a database or a sleep.
//
//	elapsed  = monotonic seconds since this process started
//	delta    = max(0, elapsed - lastElapsed)
//
// A process that restarts begins with lastElapsed = 0 and elapsed ≈ 0,
// so any wall-clock gap between processes contributes nothing.
func ActiveUptimeFromElapsed(processElapsed time.Duration, lastElapsed time.Duration) int64 {
	d := processElapsed - lastElapsed
	if d <= 0 {
		return 0
	}
	return int64(d / time.Second)
}
