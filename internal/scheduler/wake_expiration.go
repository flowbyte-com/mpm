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
