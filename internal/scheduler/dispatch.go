// dispatch.go — atomic claim of notification-kind scheduled_wakes.
//
// The deadline-driven Run loop in scheduler.go calls these to drain
// notification-kind (and untagged, which looks the same as notification
// to Kind()) wakes whose target_time has elapsed.
//
// Why a separate file: the claim SQL is the keystone correctness
// invariant — it MUST be atomic against concurrent dispatchers
// (mpm-scheduler daemon, mpm call opportunistic fold, mpm-mcp
// opportunistic fold). All three call sites compete for the same
// rows; the UPDATE ... WHERE fired=0 ... RETURNING pattern is what
// prevents double-fire. Keeping it isolated lets us reason about the
// invariant in one place and verify it with focused tests in
// dispatch_test.go.
//
// What this file does NOT do:
//   - It does not execute a wake handler. The scheduler's Tick loop
//     dispatches system kinds (snapshot, gc, etc.). Notification kinds
//     have no in-process handler — they surface via read_wake_context
//     and the agent decides what to do on the next MCP call.
//   - It does not call MarkFired. The UPDATE inside the claim is itself
//     the fired-flip, so a successful claim means fired=1.
//
// The Substrate Defense Triad applies: the write here is the firing
// mutation, so we read it back implicitly via RETURNING (the row we
// just claimed is the one we got back).
package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// dispatchClaimNextAdHocWake atomically claims exactly one due
// notification-kind scheduled_wakes row, or returns (zero, false, nil)
// when no eligible row exists.
//
// Concurrency: this is the canonical cross-process dedup primitive.
// SQLite's UPDATE ... WHERE id = (SELECT ...) WHERE fired = 0 is atomic
// — when two dispatchers race on the same table, exactly one wins; the
// loser sees ErrNoRows because the inner SELECT's WHERE fired = 0
// filters out the row the winner just claimed.
//
// Eligible = WHERE fired = 0 AND target_time <= now AND kind IN
// ('notification', NULL='', MISSING). This matches the existing
// Kind() default (untagged wakes report kind = "notification") so the
// claim partition is precisely the complement of the system-kind set.
//
// The fired timestamp and a "dispatched_by = mpm-scheduler" metadata
// tag are stamped on success; the latter is the forensic trail for
// distinguishing scheduler-claimed rows from rows claimed by the
// opportunistic fold in mpm call / mpm-mcp.
//
// The single-row claim shape lets dispatchDrainAdHocWakes loop
// indefinitely (bounded only by capN) without holding any transaction.
// Each iteration is its own SQLite write — no transaction overhead.
func dispatchClaimNextAdHocWake(ctx context.Context, db *sql.DB, now time.Time) (Wake, bool, error) {
	nowUnix := now.Unix()
	var w Wake
	var metaJSON string
	err := db.QueryRowContext(ctx, `
		UPDATE scheduled_wakes
		SET fired = 1,
		    fired_at = ?,
		    metadata = json_set(COALESCE(metadata,'{}'), '$.dispatched_by', 'mpm-scheduler')
		WHERE id = (
			SELECT id FROM scheduled_wakes
			WHERE fired = 0
			  AND target_time <= ?
			  AND (
			    metadata IS NULL
			    OR json_extract(metadata, '$.kind') IS NULL
			    OR json_extract(metadata, '$.kind') = ''
			    OR json_extract(metadata, '$.kind') = 'notification'
			  )
			ORDER BY target_time ASC
			LIMIT 1
		)
		RETURNING id, target_time, reason, theory_id, recurring_rule,
		          created_by, created_at, metadata
	`, nowUnix, nowUnix).Scan(
		&w.ID, &w.TargetTime, &w.Reason, &w.TheoryID, &w.RecurringRule,
		&w.CreatedBy, &w.CreatedAt, &metaJSON,
	)
	if err == sql.ErrNoRows {
		return Wake{}, false, nil
	}
	if err != nil {
		return Wake{}, false, fmt.Errorf("claim ad-hoc wake: %w", err)
	}
	if metaJSON != "" {
		_ = json.Unmarshal([]byte(metaJSON), &w.Metadata)
	}
	return w, true, nil
}

// dispatchDrainAdHocWakes repeatedly claims due notification-kind
// wakes until the queue is empty or capN claims have happened in this
// call (default 100). Returns the number of wakes claimed.
//
// capN bounds a single drain pass so a runaway scheduler tick (e.g.
// after a long outage where many wakes accumulated) cannot starve
// other scheduler work. The cron-doesn't-block-maintenance floor
// pattern in scheduler.go's Run loop calls this from the deadline.C
// branch and from the post-tick cleanup.
//
// Cancellation: checks ctx.Err() between claims. A ctx cancellation
// mid-drain returns the partial count + ctx.Err().
//
// This function does NOT log per-wake; the caller is responsible for
// surfacing summary lines (Run already logs "ad-hoc drain executed
// wakes count=N").
func dispatchDrainAdHocWakes(ctx context.Context, db *sql.DB, now time.Time, capN int) (int, error) {
	if capN <= 0 {
		capN = 100
	}
	claimed := 0
	for claimed < capN {
		if cerr := ctx.Err(); cerr != nil {
			return claimed, fmt.Errorf("drain ad-hoc wakes: ctx cancelled: %w", cerr)
		}
		_, ok, err := dispatchClaimNextAdHocWake(ctx, db, now)
		if err != nil {
			return claimed, fmt.Errorf("drain ad-hoc wakes: %w", err)
		}
		if !ok {
			return claimed, nil
		}
		claimed++
	}
	return claimed, nil
}
