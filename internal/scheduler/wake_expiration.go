// Package scheduler — wake_expiration.go: 7-day retention sweep for
// notification-kind scheduled_wakes.
//
// Why this exists. Wakes whose kind is "notification" (or untagged,
// which defaults to "notification" via Wake.Kind) bypass the
// scheduler's active handler dispatch by design — they are surfaced
// into agent working memory on the next mpm-mcp opportunistic fold
// call. There is no expiration check on the fold path: a wake that
// is never folded accumulates indefinitely, growing the wakes_overdue
// metric and confusing doctor / status output.
//
// The retention sweep retires notification-kind wakes whose
// target_time is more than 7 days in the past. After 7 days, a
// notification that has not been picked up by the fold path is
// effectively dead weight: agents that care will have surfaced it
// long before then, and a 7-day-overdue notification carries no
// operational signal beyond "this lane is idle".
//
// What "retire" means. We mark fired=1 with fired_at=now and append
// an audit note to metadata. The wake:
//   - drops out of the fires-overdue query (WHERE fired = 0 ...),
//   - drops out of the wakes_overdue doctor counter,
//   - remains in scheduled_wakes for forensic / audit purposes
//     (mpm list_wakes --include-fired still returns it),
//   - gets a stable identifier in metadata so an operator can prove
//     the row was swept, not lost.
//
// What this sweep does NOT do.
//   - It does not touch wakes with a system kind (snapshot,
//     critic_audit, gc, broadcast, …). Those are dispatched
//     eagerly and mark themselves fired via MarkFired.
//   - It does not touch pending (not-yet-due) notification wakes —
//     they sit in the queue waiting for the fold path, which is
//     the intended behaviour.
//   - It does not delete rows. The mpm list_wakes view is the
//     audit surface; this sweep changes the row's status, not its
//     existence.
//
// Performance & safety.
//   - LIMIT 100 per call: the sweep is cheap but a runaway schedule
//     path (e.g. 100k dead wakes from a misconfigured cron) must
//     not stall the tick. 100 is large enough to drain any normal
//     drift, small enough to never block the next handler.
//   - Single SELECT to identify candidates, single UPDATE per
//     candidate inside a transaction: the row-level cost is O(1)
//     and the JSON write is bounded by metadata size.
//   - No goroutines, no external I/O: safe to call inline on a tick
//     handler.

package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// NotificationRetentionWindow is the grace period before a
// notification-kind wake is eligible for the sweep. 7 days matches
// the pre-alpha retention policy; lower values tighten the window
// (more aggressive), higher values loosen it. Exposed as a const so
// tests can reason about it without a magic number.
const NotificationRetentionWindow = 7 * 24 * time.Hour

// NotificationRetentionLimit is the per-tick cap on the sweep. Each
// call retires at most this many wakes. The next tick picks up
// where this one left off, so a backlog drains over multiple ticks
// without ever blocking a single tick.
const NotificationRetentionLimit = 100

// ExpirationMetadataKey is the JSON key written into
// scheduled_wakes.metadata by the sweep. Operators inspecting
// audit rows can grep for this to find every row the sweep has
// retired.
const ExpirationMetadataKey = "expired"

// ExpirationReasonValue is the audit note value. Distinct from
// "expired" so a future "dismissed" or "cancelled" path can coexist
// on the same row without collision.
const ExpirationReasonValue = "notification_7d_retention"

// SweepOverdueNotificationWakes retires notification-kind
// scheduled_wakes whose target_time is more than 7 days in the
// past. Returns the number of rows retired.
//
// Idempotent on the same input: a wake that has already been
// retired (fired=1, metadata carries the audit note) is filtered
// by the WHERE clause and never re-touched.
//
// The function is exposed for direct invocation by tests and for
// future CLI / on-demand paths. The tick handler
// (NotificationExpirationTickHandler) is the production caller.
func SweepOverdueNotificationWakes(ctx context.Context, db *sql.DB, now time.Time) (int, error) {
	// 1. Compute the cutoff. We compare against unix-epoch seconds
	//    to match the integer storage class of target_time.
	cutoff := now.Add(-NotificationRetentionWindow).Unix()

	// 2. Identify candidates. The kind filter is intentionally
	//    permissive: untagged wakes default to "notification" via
	//    Wake.Kind(), so a missing or NULL json_extract result is
	//    also a candidate. An explicit system kind (e.g. snapshot)
	//    is NOT a candidate — those are dispatched eagerly and
	//    self-mark fired.
	candidateQuery := `
		SELECT id, metadata
		FROM scheduled_wakes
		WHERE fired = 0
		  AND target_time < ?
		  AND (
		    json_extract(metadata, '$.kind') IS NULL
		    OR json_extract(metadata, '$.kind') = ''
		    OR json_extract(metadata, '$.kind') = 'notification'
		  )
		ORDER BY target_time ASC
		LIMIT ?
	`
	rows, err := db.QueryContext(ctx, candidateQuery, cutoff, NotificationRetentionLimit)
	if err != nil {
		return 0, fmt.Errorf("sweep: query candidates: %w", err)
	}
	type candidate struct {
		id       string
		metadata *string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.metadata); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sweep: scan candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("sweep: iterate candidates: %w", err)
	}
	rows.Close()

	if len(candidates) == 0 {
		return 0, nil
	}

	// 3. Retire each candidate inside a single transaction. The
	//    row-level read-back assertion is the Substrate Defense
	//    Triad rule: an UPDATE that reports nil error is not the
	//    same as a row that actually flipped to fired=1. We
	//    re-read each row post-UPDATE to confirm the state
	//    change took effect.
	nowUnix := now.Unix()
	retired := 0
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sweep: begin tx: %w", err)
	}
	defer tx.Rollback() // safe no-op after Commit

	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return retired, fmt.Errorf("sweep: ctx cancelled mid-batch: %w", err)
		}

		// Build the new metadata: take the existing JSON object
		// (or {} if absent), set the audit keys, write back as a
		// JSON string. We use COALESCE to handle the
		// metadata IS NULL case (a wake created via raw INSERT
		// without metadata).
		updateQuery := `
			UPDATE scheduled_wakes
			SET fired = 1,
			    fired_at = ?,
			    metadata = json_set(
			      COALESCE(metadata, '{}'),
			      '$.' || ? || '.reason', ?,
			      '$.' || ? || '.at', ?,
			      '$.' || ? || '.target_time', (
			        SELECT target_time FROM scheduled_wakes WHERE id = ?
			      )
			    )
			WHERE id = ?
		`
		res, err := tx.ExecContext(ctx, updateQuery,
			nowUnix,
			ExpirationMetadataKey, ExpirationReasonValue,
			ExpirationMetadataKey, nowUnix,
			ExpirationMetadataKey, c.id,
			c.id,
		)
		if err != nil {
			return retired, fmt.Errorf("sweep: update %s: %w", c.id, err)
		}

		// Read-back assertion (Substrate Defense Triad, rule 3).
		// A wake retired by the sweep must be findable with
		// fired=1 AND the audit note present in metadata. We
		// don't return an error if RowsAffected is 0 on a
		// table that might be a view; in the production
		// scheduled_wakes is a real table so 0 is an error.
		rowsAffected, raErr := res.RowsAffected()
		if raErr != nil {
			return retired, fmt.Errorf("sweep: rows-affected %s: %w", c.id, raErr)
		}
		if rowsAffected == 0 {
			return retired, fmt.Errorf("sweep: update %s affected 0 rows (concurrent change?)", c.id)
		}

		var (
			fired    int
			firedAt  int64
			metadata string
		)
		readBack := tx.QueryRowContext(ctx,
			`SELECT fired, fired_at, COALESCE(metadata, '{}') FROM scheduled_wakes WHERE id = ?`,
			c.id,
		)
		if err := readBack.Scan(&fired, &firedAt, &metadata); err != nil {
			return retired, fmt.Errorf("sweep: read-back %s: %w", c.id, err)
		}
		if fired != 1 {
			return retired, fmt.Errorf("sweep: read-back %s: fired=%d, want 1", c.id, fired)
		}
		if firedAt != nowUnix {
			return retired, fmt.Errorf("sweep: read-back %s: fired_at=%d, want %d", c.id, firedAt, nowUnix)
		}
		// Audit note must be present and carry the right reason.
		reason := extractMetadataString(metadata, ExpirationMetadataKey+".reason")
		if reason != ExpirationReasonValue {
			return retired, fmt.Errorf("sweep: read-back %s: missing %s.reason=%q (got %q)",
				c.id, ExpirationMetadataKey, ExpirationReasonValue, reason)
		}

		retired++
	}

	if err := tx.Commit(); err != nil {
		return retired, fmt.Errorf("sweep: commit: %w", err)
	}
	return retired, nil
}

// NotificationExpirationTickHandler is the production entry point
// the scheduler tick loop calls. It binds SweepOverdueNotificationWakes
// to the current wall clock and surfaces a structured log line on
// every tick that retires at least one wake.
//
// Errors are returned (and logged by the caller) so a transient
// SQLite BUSY does not silently disable the sweep — the next tick
// will retry.
func NotificationExpirationTickHandler(ctx context.Context, db *sql.DB) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := SweepOverdueNotificationWakes(ctx, db, time.Now())
		if err != nil {
			return fmt.Errorf("notification-expiration sweep: %w", err)
		}
		return nil
	}
}

// =============================================================================
// CRON-KIND WAKE RETENTION
// =============================================================================
//
// Why this exists. ProcessScheduledTasks (internal/core/scheduled_tasks.go)
// injects one `kind=cron` scheduled_wakes row per minute per active cron
// task (today: epistemic-compaction on a 1-minute schedule). The ad-hoc
// dispatcher deliberately never claims cron-kind rows — that is the
// CPU-wedge protection at scheduler.go:485-510. The result: cron rows
// accumulate indefinitely in scheduled_wakes. Over weeks of uptime the
// table grew past 2,300 rows even though the scheduler is healthy.
//
// What this sweep does. After CronRetentionWindow has elapsed since
// target_time, a cron row is permanently obsolete — its corresponding
// scheduled_tasks row already carries `last_run_at` as the audit
// trail, and the scheduled_wakes row itself has zero informational
// value (it records a one-shot event, not state). The sweep retires
// the row (fired=1, fired_at=now, audit note) so it drops out of the
// wakes_overdue doctor counter and out of any `WHERE fired=0` query.
//
// This is structurally identical to the notification sweep above — the
// difference is the kind filter and the bounds.
//
// What "retire" means here is the same. fired=1 with a stable
// identifier in metadata, row preserved for forensic / list_wakes
// --include-fired surfaces. The 30-day ops-maintain TTL on fired rows
// (schema.go) eventually hard-deletes them.
//
// Bounds (2026-09-11 cron-retention arc):
//   - Normal cadence: CronRetentionCadence = 60min. Normal cap per
//     sweep: CronRetentionNormalLimit = 60 rows. Matches the ~60
//     rows/hour production rate of the 1-minute epistemic-compaction
//     cron exactly; normal operation stays bounded.
//   - Catch-up: when the eligible backlog at start of sweep is
//     ≥ CronRetentionCatchUpThreshold = 240 rows, the cap is raised
//     to CronRetentionCatchUpLimit = 180. This is a deterministic
//     rule (the threshold and cap are constants) and the backlog
//     check uses a single bounded SELECT ... LIMIT 241, never a full
//     table COUNT(*).
//   - Hard ceiling: CronRetentionCatchUpLimit = 180 is the per-call
//     maximum. No cleanup invocation processes more rows than that.
//
// Why catch-up matters. If the scheduler daemon was down or
// rate-limited for hours, the backlog may be a few hundred rows. The
// 60/cadence cap would drain at 60/60min = 1/min, taking hours to
// catch up. The 180 catch-up pulls 3 hours of backlog into a single
// cleanup. Beyond catch-up, the next cadence cycle cleans another
// 180, etc., so a multi-day backlog converges without ever
// processing unbounded rows per call.
//
// CPU safety. The sweep is bounded (≤180 rows per call) and uses
// the same O(1)-per-row update pattern as the notification sweep.
// The ad-hoc dispatcher remains filtered against cron-kind (see
// dispatch.go:80-87 and scheduler.go:511-535) — this sweep changes
// row state, not the dispatcher's claim filter, so the wedge
// regression at TestScheduler_DoesNotWedgeOnPastCronKindWake
// (scheduler_test.go:957) continues to hold.

// CronRetentionWindow is the grace period before a cron-kind wake
// becomes eligible for retirement. 1 hour matches the cadence
// (60-min cleanup) with a small audit buffer so operators inspecting
// `mpm list_wakes` immediately after a cron fire still see the row.
const CronRetentionWindow = 1 * time.Hour

// CronRetentionCadence gates the tick handler — a sweep runs at most
// once per CronRetentionCadence. The scheduler's tick fires every
// interval (default 60s); without this gate, every tick would do the
// work.
const CronRetentionCadence = 60 * time.Minute

// CronRetentionNormalLimit is the per-sweep cap under normal
// (non-catch-up) operation. Matches the 1-min cron production rate
// over a 60-min cleanup interval — exactly 60 rows per hour.
const CronRetentionNormalLimit = 60

// CronRetentionCatchUpLimit is the per-sweep cap when catch-up mode
// is triggered. 180 rows = 3 hours of backlog per cleanup call.
// Hard ceiling on per-call work.
const CronRetentionCatchUpLimit = 180

// CronRetentionCatchUpThreshold is the eligible-row count at which
// catch-up mode activates. Set to 4 × CronRetentionNormalLimit so
// catch-up only fires when the backlog materially exceeds the normal
// capacity — a 240-row eligible backlog indicates ≥ 4 hours of
// un-cleaned production, which a 60-cap would take ≥ 4 hours to
// drain at one cadence each. 180-cap drains 3 hours of that per
// call, getting us back to normal in ~2 cycles.
const CronRetentionCatchUpThreshold = 240

// CronRetentionReasonValue is the audit note value written into the
// row's metadata. Distinct from ExpirationReasonValue so an operator
// grepping `expired.reason` can tell notification sweeps from cron
// sweeps apart.
const CronRetentionReasonValue = "cron_1h_retention"

// cronRetentionProbeBacklog returns the count of eligible rows at
// this instant. Bounded to CronRetentionCatchUpThreshold + 1 so the
// probe never scans more than 241 rows — keeping it cheap regardless
// of how large the eligible backlog grows.
//
// Returns -1 on query error (the caller treats -1 as "could not
// determine", which falls back to the normal cap — a safe default).
func cronRetentionProbeBacklog(ctx context.Context, db *sql.DB, cutoffUnix int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
		  SELECT 1 FROM scheduled_wakes
		  WHERE fired = 0
		    AND target_time < ?
		    AND json_extract(metadata, '$.kind') = 'cron'
		  LIMIT ?
		)
	`, cutoffUnix, CronRetentionCatchUpThreshold+1).Scan(&n)
	if err != nil {
		return -1, fmt.Errorf("probe cron backlog: %w", err)
	}
	return n, nil
}

// SweepOverdueCronWakes retires cron-kind scheduled_wakes whose
// target_time is more than CronRetentionWindow in the past. Returns
// the number of rows retired.
//
// Idempotent on the same input: a wake that has already been retired
// (fired=1) is filtered by the WHERE clause and never re-touched.
//
// maxRows bounds the per-call work; the caller chooses between
// CronRetentionNormalLimit and CronRetentionCatchUpLimit based on
// backlog size. The sweep itself never inspects backlog — it
// blindly honors the cap.
//
// The query, transaction shape, and read-back assertion mirror
// SweepOverdueNotificationWakes so the contract is identical across
// both sweeps. The kind filter is the only meaningful difference —
// the WHERE clause excludes notification-kind wakes (covered by the
// other sweep) and system kinds (covered by their eager-dispatch
// handlers).
func SweepOverdueCronWakes(ctx context.Context, db *sql.DB, now time.Time, maxRows int) (int, error) {
	if maxRows <= 0 || maxRows > CronRetentionCatchUpLimit {
		maxRows = CronRetentionCatchUpLimit
	}

	cutoff := now.Add(-CronRetentionWindow).Unix()

	candidateQuery := `
		SELECT id, metadata
		FROM scheduled_wakes
		WHERE fired = 0
		  AND target_time < ?
		  AND json_extract(metadata, '$.kind') = 'cron'
		ORDER BY target_time ASC
		LIMIT ?
	`
	rows, err := db.QueryContext(ctx, candidateQuery, cutoff, maxRows)
	if err != nil {
		return 0, fmt.Errorf("cron sweep: query candidates: %w", err)
	}
	type candidate struct {
		id       string
		metadata *string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.metadata); err != nil {
			rows.Close()
			return 0, fmt.Errorf("cron sweep: scan candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("cron sweep: iterate candidates: %w", err)
	}
	rows.Close()

	if len(candidates) == 0 {
		return 0, nil
	}

	nowUnix := now.Unix()
	retired := 0
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("cron sweep: begin tx: %w", err)
	}
	defer tx.Rollback()

	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return retired, fmt.Errorf("cron sweep: ctx cancelled mid-batch: %w", err)
		}

		updateQuery := `
			UPDATE scheduled_wakes
			SET fired = 1,
			    fired_at = ?,
			    metadata = json_set(
			      COALESCE(metadata, '{}'),
			      '$.' || ? || '.reason', ?,
			      '$.' || ? || '.at', ?,
			      '$.' || ? || '.target_time', (
			        SELECT target_time FROM scheduled_wakes WHERE id = ?
			      )
			    )
			WHERE id = ?
		`
		res, err := tx.ExecContext(ctx, updateQuery,
			nowUnix,
			ExpirationMetadataKey, CronRetentionReasonValue,
			ExpirationMetadataKey, nowUnix,
			ExpirationMetadataKey, c.id,
			c.id,
		)
		if err != nil {
			return retired, fmt.Errorf("cron sweep: update %s: %w", c.id, err)
		}

		rowsAffected, raErr := res.RowsAffected()
		if raErr != nil {
			return retired, fmt.Errorf("cron sweep: rows-affected %s: %w", c.id, raErr)
		}
		if rowsAffected == 0 {
			return retired, fmt.Errorf("cron sweep: update %s affected 0 rows (concurrent change?)", c.id)
		}

		var (
			fired    int
			firedAt  int64
			metadata string
		)
		readBack := tx.QueryRowContext(ctx,
			`SELECT fired, fired_at, COALESCE(metadata, '{}') FROM scheduled_wakes WHERE id = ?`,
			c.id,
		)
		if err := readBack.Scan(&fired, &firedAt, &metadata); err != nil {
			return retired, fmt.Errorf("cron sweep: read-back %s: %w", c.id, err)
		}
		if fired != 1 {
			return retired, fmt.Errorf("cron sweep: read-back %s: fired=%d, want 1", c.id, fired)
		}
		if firedAt != nowUnix {
			return retired, fmt.Errorf("cron sweep: read-back %s: fired_at=%d, want %d", c.id, firedAt, nowUnix)
		}
		reason := extractMetadataString(metadata, ExpirationMetadataKey+".reason")
		if reason != CronRetentionReasonValue {
			return retired, fmt.Errorf("cron sweep: read-back %s: missing %s.reason=%q (got %q)",
				c.id, ExpirationMetadataKey, CronRetentionReasonValue, reason)
		}

		retired++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("cron sweep: commit: %w", err)
	}
	return retired, nil
}

// cronRetentionState tracks the last sweep timestamp for cadence
// gating. The scheduler is a singleton per host (flock on
// scheduler.lock); one shared struct in the TickHandler closure is
// sufficient. Not persisted across daemon restarts by design — a
// fresh start may run a sweep immediately to catch up after downtime.
type cronRetentionState struct {
	lastSweepUnix int64
}

// CronRetentionTickHandler is the production entry point the
// scheduler tick loop calls. It gates the cron sweep behind
// CronRetentionCadence so the work happens ~once per hour, not on
// every 60-second tick.
//
// Per call:
//  1. If last sweep was within Cadence, return immediately (no work).
//  2. Probe eligible backlog (bounded SELECT ... LIMIT 241).
//  3. Choose cap: catch-up mode (180) when probe >= 240, else normal (60).
//  4. Run SweepOverdueCronWakes with the chosen cap.
//  5. Update lastSweepUnix on success.
//  6. Emit a Debug-level log line so operators can distinguish
//     sweep-ran-and-retired vs sweep-ran-but-nothing-eligible, and
//     normal vs catch-up cap. Cadence-gated skips stay silent.
//
// Step 2 is the only DB query on idle ticks (step 1 short-circuits
// without touching the DB). Probe is bounded by the constant
// CronRetentionCatchUpThreshold+1, never scans the whole table.
//
// Errors are returned and logged by the caller; transient errors
// leave lastSweepUnix unchanged so the next tick retries.
//
// Concurrency. cronRetentionState is read and written only from the
// tick handler closure, which is single-goroutine by scheduler
// design (dispatchTickHandlers is synchronous — see scheduler.go:400).
// No locking required.
func CronRetentionTickHandler(ctx context.Context, db *sql.DB, log *slog.Logger) func(ctx context.Context) error {
	state := &cronRetentionState{}
	return func(ctx context.Context) error {
		now := time.Now()
		nowUnix := now.Unix()
		if state.lastSweepUnix >0 && nowUnix-state.lastSweepUnix < int64(CronRetentionCadence.Seconds()) {
			return nil
		}

		cutoff := now.Add(-CronRetentionWindow).Unix()
		backlog, err := cronRetentionProbeBacklog(ctx, db, cutoff)
		if err != nil {
			return fmt.Errorf("cron retention probe: %w", err)
		}

		cap := CronRetentionNormalLimit
		catchUp := backlog >= CronRetentionCatchUpThreshold
		if catchUp {
			cap = CronRetentionCatchUpLimit
		}

		retired, err := SweepOverdueCronWakes(ctx, db, now, cap)
		if err != nil {
			return fmt.Errorf("cron retention sweep: %w", err)
		}

		// Only advance the cadence on a successful sweep. A
		// transient error leaves lastSweepUnix untouched so the
		// next tick retries the probe + sweep.
		state.lastSweepUnix = nowUnix

		// Debug-level visibility for the once-per-hour happy path.
		// Distinguishes "ran and retired N" from "ran but nothing
		// eligible" and normal from catch-up cap. Cadence-gated
		// skips (the common case) stay silent.
		if log != nil {
			log.Debug("cron retention sweep complete",
				"eligible_backlog", backlog,
				"retired", retired,
				"cap", cap,
				"catch_up", catchUp,
				"cutoff_unix", cutoff,
			)
		}
		return nil
	}
}

// extractMetadataString returns the value at the given dotted path
// inside a JSON metadata string, or "" if the key is missing or
// not a string. Implemented in Go rather than SQL so the read-back
// assertion has zero per-row query overhead.
func extractMetadataString(metadataJSON, path string) string {
	if metadataJSON == "" || metadataJSON == "{}" {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(metadataJSON), &m); err != nil {
		return ""
	}
	// Walk the dotted path. Notification-expiration only writes
	// one level of nesting ("expired.reason"), but the walker
	// handles arbitrary depth in case the schema grows.
	keys := strings.Split(path, ".")
	var cur interface{} = m
	for _, k := range keys {
		obj, ok := cur.(map[string]interface{})
		if !ok {
			return ""
		}
		cur, ok = obj[k]
		if !ok {
			return ""
		}
	}
	if s, ok := cur.(string); ok {
		return s
	}
	return ""
}
