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
//   - drops out of the wakes_overdue query (WHERE fired = 0 ...),
//   - drops out of the wakes_overdue doctor counter,
//   - remains in scheduled_wakes for forensic / audit purposes
//     (mpm list_wakes --include-fired still returns it),
//   - gets a stable identifier in metadata so an operator can prove
//     the row was swept, not lost.
//
// Retired rows are functionally equivalent to a fold or ResolveWake
// terminal transition for notification-kind wakes: fired=1 means the
// wake is no longer pending for normal delivery. The sweep is the
// bounded-retirement fallback for notification wakes that never reach
// the fold because no MPM call arrived in time. It is the third
// legitimate notification-kind terminal path (alongside the fold
// and explicit ResolveWake). See scheduler.go and wake_tools.go for
// the canonical fired=1 / terminal invariant.
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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	core "github.com/flowbyte-com/mpm-core"
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
// CONCURRENCY. The candidate SELECT runs outside the retirement
// transaction, so another writer can retire a candidate between the
// SELECT and the UPDATE. The UPDATE therefore carries an `AND
// fired = 0` guard and is a compare-and-set on the retirement flag:
// it retires a wake only if the wake is still pending. A wake that
// was retired in the meantime is SKIPPED — the sweep does not
// overwrite the other writer's `fired_at` or metadata, which for a
// scheduler settlement is the only record of when the handler
// actually finished. A skipped candidate is not counted as retired
// and does not abort the batch.
//
// The function is exposed for direct invocation by tests and for
// future CLI / on-demand paths. The tick handler
// (NotificationExpirationTickHandler) is the production caller.
func SweepOverdueNotificationWakes(ctx context.Context, db *sql.DB, now time.Time) (int, []core.WakeMetadataCorruption, error) {
	// 1. Compute the cutoff. We compare against unix-epoch seconds
	//    to match the integer storage class of target_time.
	cutoff := now.Add(-NotificationRetentionWindow).Unix()

	// 2. Identify candidates. TWO bounded queries, for TWO populations.
	//
	//    (a) THE RETIREMENT CANDIDATES: the healthy overdue rows. The
	//        kind filter is intentionally permissive: untagged wakes
	//        default to "notification" via Wake.Kind(), so a missing
	//        or NULL json_extract result is also a candidate. An
	//        explicit system kind (e.g. snapshot) is NOT a candidate —
	//        those are dispatched eagerly and self-mark fired.
	//
	//        The filter is core.WakeNotificationKindFilter, which
	//        reads the kind through an always-valid-JSON extraction.
	//        json_extract() RAISES on malformed JSON, so an unguarded
	//        call aborts the whole candidate SELECT on a single corrupt
	//        metadata row — and a corrupt row would then block the
	//        retention of EVERY healthy overdue row behind it, forever.
	//        The always-valid form raises on nothing. It also carries
	//        no dependence on SQL evaluation order, which an OR-chain
	//        of json_extract() calls would.
	//
	//        The corrupt rows are EXCLUDED from this query, and the
	//        exclusion is not cosmetic. A corrupt row is never
	//        retired, so letting it consume one of the sweep's
	//        bounded slots would starve the healthy rows behind it:
	//        with as many corrupt rows as the limit, the sweep would
	//        retire NOTHING. That is the very "one bad row stops the
	//        sweep" class this defect is about, one level up.
	//
	//    (b) THE CORRUPT ROWS: reported, never touched. They are
	//        fetched by their own bounded query so that the
	//        diagnostic survives the exclusion above, and so that a
	//        corrupt row never consumes the retirement capacity.
	candidateQuery := `
		SELECT id, metadata
		FROM scheduled_wakes
		WHERE fired = 0
		  AND target_time < ?
		  AND ` + core.WakeNotificationKindFilter + `
		  AND NOT ` + core.WakeMetadataIsCorrupt + `
		ORDER BY target_time ASC
		LIMIT ?
	`
	corruptQuery := `
		SELECT id, metadata
		FROM scheduled_wakes
		WHERE fired = 0
		  AND target_time < ?
		  AND ` + core.WakeMetadataIsCorrupt + `
		ORDER BY target_time ASC
		LIMIT ?
	`
	rejected, err := sweepCorruptRows(ctx, db, corruptQuery, cutoff, NotificationRetentionLimit,
		core.WakeSurfaceNotificationExpiration)
	if err != nil {
		return 0, nil, err
	}

	rows, err := db.QueryContext(ctx, candidateQuery, cutoff, NotificationRetentionLimit)
	if err != nil {
		return 0, nil, fmt.Errorf("sweep: query candidates: %w", err)
	}
	type candidate struct {
		id       string
		metadata sql.NullString
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.metadata); err != nil {
			rows.Close()
			return 0, nil, fmt.Errorf("sweep: scan candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, nil, fmt.Errorf("sweep: iterate candidates: %w", err)
	}
	rows.Close()

	if len(candidates) == 0 && len(rejected) == 0 {
		return 0, nil, nil
	}

	// 3. Retire each candidate inside a single transaction. The
	//    row-level read-back assertion is the Substrate Defense
	//    Triad rule: an UPDATE that reports nil error is not the
	//    same as a row that actually flipped to fired=1. We
	//    re-read each row post-UPDATE to confirm the state
	//    change took effect.
	//
	//    PER-ROW ISOLATION. Each row's work runs inside a SAVEPOINT
	//    (retireSweepRow), so a failure on one row rolls back that row
	//    alone and leaves the rows already retired in this batch
	//    intact. The batch is committed with whatever succeeded and
	//    the per-row failures are returned as an error. Before this,
	//    one failing row aborted the whole transaction and undid
	//    every retirement the sweep had already made — the "one bad row
	//    stops the sweep" class this defect is about.
	nowUnix := now.Unix()
	retired := 0
	var failures []error
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("sweep: begin tx: %w", err)
	}
	defer tx.Rollback() // safe no-op after Commit

	retirer := &sweepRetirer{tx: tx, label: "sweep"}
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return retired, rejected, fmt.Errorf("sweep: ctx cancelled mid-batch: %w", err)
		}

		// A CORRUPT ROW IS REJECTED, NOT RETIRED.
		//
		// A row whose metadata is present, non-empty and not a
		// parseable JSON document is owned by NO partition. Retiring
		// it here would report a scheduler-owned row under the
		// NOTIFICATION reason — a wrong audit trail — and destroy
		// the only copy of the bytes that name its owner.
		//
		// The candidate query already excludes corrupt rows, so this
		// check is the TOCTOU GUARD: a row corrupted between the
		// candidate query and this loop is rejected here instead of
		// being retired.
		if core.WakeMetadataIsCorruptValue(c.metadata) {
			rejected = appendSweepRejection(rejected, core.WakeMetadataCorruption{
				WakeID:   c.id,
				Metadata: c.metadata.String,
				Surface:  core.WakeSurfaceNotificationExpiration,
			})
			continue
		}

		ok, rowErr := retirer.retire(ctx, c.id, ExpirationReasonValue, nowUnix)
		if rowErr != nil {
			failures = append(failures, rowErr)
			continue
		}
		if ok {
			retired++
		}
	}

	if err := tx.Commit(); err != nil {
		return retired, rejected, fmt.Errorf("sweep: commit: %w", err)
	}
	if len(failures) > 0 {
		return retired, rejected, fmt.Errorf("sweep: %d row(s) failed and were skipped; "+
			"the rows that succeeded were retired: %w", len(failures), errors.Join(failures...))
	}
	return retired, rejected, nil
}

// NotificationExpirationTickHandler is the production entry point
// the scheduler tick loop calls. It binds SweepOverdueNotificationWakes
// to the current wall clock and surfaces a structured log line on
// every tick that retires at least one wake.
//
// Errors are returned (and logged by the caller) so a transient
// SQLite BUSY does not silently disable the sweep — the next tick
// will retry.
func NotificationExpirationTickHandler(ctx context.Context, db *sql.DB, log *slog.Logger) func(ctx context.Context) error {
	if log == nil {
		log = slog.Default()
	}
	// The reporter remembers what it has already logged. This sweep
	// runs on EVERY tick (about once a minute), so a corrupt row that
	// is never repaired would otherwise produce one log line per
	// minute, forever.
	reporter := &wakeCorruptionReporter{}
	return func(ctx context.Context) error {
		retired, rejected, err := SweepOverdueNotificationWakes(ctx, db, time.Now())
		for _, c := range reporter.newOnes(rejected) {
			log.Error("wake metadata is corrupt — row rejected and PRESERVED",
				"wake_id", c.WakeID,
				"surface", c.Surface,
				"metadata", c.Metadata,
			)
		}
		if err != nil {
			return fmt.Errorf("notification-expiration sweep: %w", err)
		}
		if retired > 0 && log != nil {
			log.Debug("notification-expiration sweep retired wakes", "count", retired)
		}
		return nil
	}
}

// sweepCorruptRows returns the corrupt rows the sweep is rejecting.
//
// The rows are read by a bounded query of their own, so they never
// consume the sweep's retirement capacity: a corrupt row is never
// retired, so letting it occupy one of the bounded candidate slots
// would starve the healthy rows behind it.
//
// The list is capped at limit rows. The wakes_corrupt_metadata health
// counter carries the true count, so a backlog of corrupt rows stays
// visible even when the sweep reports only its first page.
func sweepCorruptRows(ctx context.Context, db *sql.DB, query string, cutoff int64, limit int, surface string) ([]core.WakeMetadataCorruption, error) {
	rows, err := db.QueryContext(ctx, query, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("sweep: query corrupt rows: %w", err)
	}
	defer rows.Close()
	var out []core.WakeMetadataCorruption
	for rows.Next() {
		var (
			id       string
			metadata sql.NullString
		)
		if err := rows.Scan(&id, &metadata); err != nil {
			return nil, fmt.Errorf("sweep: scan corrupt row: %w", err)
		}
		out = append(out, core.WakeMetadataCorruption{
			WakeID:   id,
			Metadata: metadata.String,
			Surface:  surface,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sweep: iterate corrupt rows: %w", err)
	}
	return out, nil
}

// appendSweepRejection adds a rejection unless the row is already
// recorded. The corrupt rows are fetched by their own query and may
// also be met in the retirement loop, so the same row must not be
// reported twice.
func appendSweepRejection(list []core.WakeMetadataCorruption, c core.WakeMetadataCorruption) []core.WakeMetadataCorruption {
	for _, existing := range list {
		if existing.WakeID == c.WakeID {
			return list
		}
	}
	return append(list, c)
}

// wakeCorruptionReporter reports wake-metadata corruption to the log
// ONCE per distinct row.
//
// The notification sweep runs on EVERY scheduler tick (about once a
// minute), so a corrupt row that is never repaired would otherwise
// produce one log line per minute, forever. The reporter remembers
// what it has already reported and reports only what is NEW. A row
// that is repaired (or removed) drops out of the reported set, so a
// later recurrence is reported again.
//
// The reporter is a log-volume boundary, not a security boundary: the
// diagnostic itself is per-row and complete — the sweep returns EVERY
// rejection it made, and the wakes_corrupt_metadata health counter
// counts every corrupt row at any time.
type wakeCorruptionReporter struct {
	mu       sync.Mutex
	reported map[string]struct{}
}

// newOnes returns the corruptions that have not been reported yet and
// records them.
func (r *wakeCorruptionReporter) newOnes(corruptions []core.WakeMetadataCorruption) []core.WakeMetadataCorruption {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reported == nil {
		r.reported = make(map[string]struct{})
	}
	present := make(map[string]struct{}, len(corruptions))
	var fresh []core.WakeMetadataCorruption
	for _, c := range corruptions {
		key := c.WakeID + "\x00" + c.Metadata
		present[key] = struct{}{}
		if _, seen := r.reported[key]; !seen {
			fresh = append(fresh, c)
		}
	}
	for key := range r.reported {
		if _, ok := present[key]; !ok {
			delete(r.reported, key)
		}
	}
	for _, c := range fresh {
		r.reported[c.WakeID+"\x00"+c.Metadata] = struct{}{}
	}
	return fresh
}

// sweepRetirer retires rows for one retention sweep, one row at a
// time, with PER-ROW ISOLATION.
//
// Each row's work runs inside a SAVEPOINT. A failure on THIS row
// rolls back to the savepoint and leaves every row already retired in
// the batch intact. Without the savepoint the sweep's single
// transaction would undo the whole batch on the first per-row failure
// — the "one bad row stops the sweep" class this defect is about.
//
// The `AND fired = 0` guard in the UPDATE is the compare-and-set that
// makes the retirement safe against a concurrent settlement (#226): a
// wake another writer already retired is SKIPPED, not overwritten, so
// the other writer's `fired_at` and metadata survive.
type sweepRetirer struct {
	tx    *sql.Tx
	label string
	seq   int
}

// retire retires one candidate row and asserts the retirement.
//
// It returns true when this call retired the row, false when the row
// was skipped (the race was lost — nothing of ours to assert), and an
// error when the row failed. On an error the row's work has been
// rolled back and the batch is still usable.
func (r *sweepRetirer) retire(ctx context.Context, id, reason string, nowUnix int64) (bool, error) {
	r.seq++
	// A unique savepoint name per row: SQLite stacks a savepoint onto
	// an existing one with the same name, so a name that was not
	// released would silently accumulate.
	savepoint := fmt.Sprintf("wake_sweep_row_%d", r.seq)
	if _, err := r.tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		return false, fmt.Errorf("%s: savepoint %s: %w", r.label, id, err)
	}

	// fail rolls THIS row back and leaves the batch intact.
	fail := func(err error) (bool, error) {
		if _, rbErr := r.tx.ExecContext(ctx, "ROLLBACK TO "+savepoint); rbErr != nil {
			return false, fmt.Errorf("%s: rollback %s: %w (original failure: %v)",
				r.label, id, rbErr, err)
		}
		if _, relErr := r.tx.ExecContext(ctx, "RELEASE "+savepoint); relErr != nil {
			return false, fmt.Errorf("%s: release %s: %w (original failure: %v)",
				r.label, id, relErr, err)
		}
		return false, err
	}

	updateQuery := `
		UPDATE scheduled_wakes
		SET fired = 1,
		    fired_at = ?,
		    metadata = json_set(
		      ` + core.WakeMetadataJSON + `,
		      '$.' || ? || '.reason', ?,
		      '$.' || ? || '.at', ?,
		      '$.' || ? || '.target_time', (
		        SELECT target_time FROM scheduled_wakes WHERE id = ?
		      )
		    )
		WHERE id = ? AND fired = 0
	`
	res, err := r.tx.ExecContext(ctx, updateQuery,
		nowUnix,
		ExpirationMetadataKey, reason,
		ExpirationMetadataKey, nowUnix,
		ExpirationMetadataKey, id,
		id,
	)
	if err != nil {
		return fail(fmt.Errorf("%s: update %s: %w", r.label, id, err))
	}

	// RowsAffected: 0 means the candidate was no longer retirable — it
	// was retired by another writer between the candidate SELECT and
	// this UPDATE, or it was removed.
	//
	// This is the "lost the race" outcome, and the substrate's own
	// CAS idiom treats it as a benign no-op rather than an error: the
	// wake resolver returns "already_resolved" when its guarded UPDATE
	// affects zero rows. Treating it as an error here would abort the
	// whole batch — the deferred Rollback would undo every candidate this
	// sweep had already retired, so one concurrent settlement would
	// cancel the entire sweep.
	//
	// A row this sweep retired is still asserted below with the full
	// read-back. A skipped row is not asserted: this sweep did not
	// write it, so there is nothing of ours to verify.
	rowsAffected, raErr := res.RowsAffected()
	if raErr != nil {
		return fail(fmt.Errorf("%s: rows-affected %s: %w", r.label, id, raErr))
	}
	if rowsAffected == 0 {
		if _, err := r.tx.ExecContext(ctx, "RELEASE "+savepoint); err != nil {
			return false, fmt.Errorf("%s: release %s: %w", r.label, id, err)
		}
		return false, nil
	}

	var (
		fired    int
		firedAt  int64
		metadata string
	)
	readBack := r.tx.QueryRowContext(ctx,
		`SELECT fired, fired_at, COALESCE(metadata, '{}') FROM scheduled_wakes WHERE id = ?`,
		id,
	)
	if err := readBack.Scan(&fired, &firedAt, &metadata); err != nil {
		return fail(fmt.Errorf("%s: read-back %s: %w", r.label, id, err))
	}
	if fired != 1 {
		return fail(fmt.Errorf("%s: read-back %s: fired=%d, want 1", r.label, id, fired))
	}
	if firedAt != nowUnix {
		return fail(fmt.Errorf("%s: read-back %s: fired_at=%d, want %d", r.label, id, firedAt, nowUnix))
	}
	// Audit note must be present and carry the right reason.
	note := extractMetadataString(metadata, ExpirationMetadataKey+".reason")
	if note != reason {
		return fail(fmt.Errorf("%s: read-back %s: missing %s.reason=%q (got %q)",
			r.label, id, ExpirationMetadataKey, reason, note))
	}

	if _, err := r.tx.ExecContext(ctx, "RELEASE "+savepoint); err != nil {
		return false, fmt.Errorf("%s: release %s: %w", r.label, id, err)
	}
	return true, nil
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
// Diagnostic contract for `mpm doctor` / `mpm status`. The cron-wake
// retention is intentionally visible to operators via the
// CronRetentionStatus struct surfaced under the Scheduler check. The
// contract:
//
//	Phase=startup_stabilization: uptime < 2*CronRetentionCadence (2h).
//	    Recent cron wakes are expected retained. Backlog is the normal
//	    pool; first ACTUAL retirement occurs at T+2h (see startup
//	    calibration below).
//	Phase=steady: uptime >= 2h. Pending cron pool pulses between ~0
//	    (just after a sweep) and ~60 (just before the next sweep).
//	    Approximately one retention-window of recent cron rows is the
//	    steady-state contract — NOT a fixed count.
//
// Startup calibration sequence (T=process_started_unix):
//
//	T+0h  runOnce-equivalent: handler runs immediately at startup.
//	    state.lastSweepUnix=0 → gate `0 > 0` is false → sweep runs.
//	    No rows eligible (just inserted). lastSweepUnix advances.
//	T+1h  next gate-pass: handler runs. Diff=3600s. Cutoff=T+0h.
//	    Row 0 has target_time EXACTLY equal to cutoff. Strict-<
//	    excludes it. Sweep retires 0.
//	T+2h  next gate-pass: handler runs. Cutoff=T+1h. Rows 0..59 are
//	    all < cutoff. Sweep retires ~60 (the first ACTUAL retirement).
//	T+3h+ normal hourly cadence: each sweep retires ~60 rows.
//
// See TestCronRetention_StartupCalibrationSequence in
// cron_retention_test.go for the regression test that pins this
// pattern.
//
// If the cron-retention diagnostic surfaces a backlog that exceeds
// the recoverable cadence/capacity (CronRetentionCatchUpThreshold=240),
// the doctor escalates to WARN/FAIL — see cmd/mpm/service_doctor.go
// cronRetentionToStatus for the classification ladder.
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
		    AND `+core.WakeCronKindFilter+`
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
// CONCURRENCY. As in SweepOverdueNotificationWakes: the candidate
// SELECT runs outside the retirement transaction, so the retirement
// UPDATE carries an `AND fired = 0` guard and is a compare-and-set on
// the retirement flag. A candidate retired by another writer in the
// meantime is SKIPPED — not overwritten, not counted, and it does not
// abort the batch.
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
func SweepOverdueCronWakes(ctx context.Context, db *sql.DB, now time.Time, maxRows int) (int, []core.WakeMetadataCorruption, error) {
	if maxRows <= 0 || maxRows > CronRetentionCatchUpLimit {
		maxRows = CronRetentionCatchUpLimit
	}

	cutoff := now.Add(-CronRetentionWindow).Unix()

	// The candidate filter matches kind='cron' ONLY, so a corrupt row
	// — which reads as untagged — is not a candidate here. The explicit
	// exclusion states the policy at the query rather than relying on
	// that accident, so the sweep cannot start retiring corrupt rows
	// if the kind filter is ever widened.
	candidateQuery := `
		SELECT id, metadata
		FROM scheduled_wakes
		WHERE fired = 0
		  AND target_time < ?
		  AND ` + core.WakeCronKindFilter + `
		  AND NOT ` + core.WakeMetadataIsCorrupt + `
		ORDER BY target_time ASC
		LIMIT ?
	`
	rows, err := db.QueryContext(ctx, candidateQuery, cutoff, maxRows)
	if err != nil {
		return 0, nil, fmt.Errorf("cron sweep: query candidates: %w", err)
	}
	type candidate struct {
		id       string
		metadata sql.NullString
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.metadata); err != nil {
			rows.Close()
			return 0, nil, fmt.Errorf("cron sweep: scan candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, nil, fmt.Errorf("cron sweep: iterate candidates: %w", err)
	}
	rows.Close()

	if len(candidates) == 0 {
		return 0, nil, nil
	}

	nowUnix := now.Unix()
	retired := 0
	var rejected []core.WakeMetadataCorruption
	var failures []error
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("cron sweep: begin tx: %w", err)
	}
	defer tx.Rollback()

	retirer := &sweepRetirer{tx: tx, label: "cron sweep"}
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return retired, rejected, fmt.Errorf("cron sweep: ctx cancelled mid-batch: %w", err)
		}

		// A CORRUPT ROW IS REJECTED, NOT RETIRED. The candidate
		// filter already excludes one (a corrupt row reads as
		// untagged, not as cron), but the rejection is made here too
		// so that the policy holds even if the filter changes — and so
		// that the row's bytes are reported rather than silently
		// skipped.
		if core.WakeMetadataIsCorruptValue(c.metadata) {
			rejected = append(rejected, core.WakeMetadataCorruption{
				WakeID:   c.id,
				Metadata: c.metadata.String,
				Surface:  core.WakeSurfaceCronRetention,
			})
			continue
		}

		ok, rowErr := retirer.retire(ctx, c.id, CronRetentionReasonValue, nowUnix)
		if rowErr != nil {
			failures = append(failures, rowErr)
			continue
		}
		if ok {
			retired++
		}
	}

	if err := tx.Commit(); err != nil {
		return retired, rejected, fmt.Errorf("cron sweep: commit: %w", err)
	}
	if len(failures) > 0 {
		return retired, rejected, fmt.Errorf("cron sweep: %d row(s) failed and were skipped; "+
			"the rows that succeeded were retired: %w", len(failures), errors.Join(failures...))
	}
	return retired, rejected, nil
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
	// The reporter remembers what it has already logged, so a corrupt
	// row that is never repaired cannot turn the hourly sweep into a
	// log flood.
	reporter := &wakeCorruptionReporter{}
	return func(ctx context.Context) error {
		now := time.Now()
		nowUnix := now.Unix()
		if state.lastSweepUnix > 0 && nowUnix-state.lastSweepUnix < int64(CronRetentionCadence.Seconds()) {
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

		retired, rejected, err := SweepOverdueCronWakes(ctx, db, now, cap)
		for _, c := range reporter.newOnes(rejected) {
			if log != nil {
				log.Error("wake metadata is corrupt — row rejected and PRESERVED",
					"wake_id", c.WakeID,
					"surface", c.Surface,
					"metadata", c.Metadata,
				)
			}
		}
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
