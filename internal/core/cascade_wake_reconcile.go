// cascade_wake_reconcile.go — recovery pass for lost cascade wakes.
//
// Post-M3 audit H-3 (2026-08-31): the cascade materializer's pre-fix
// scheduleCascadeWake was fire-and-forget. A crash between
// markMaterialized (which flips status to 'materialized') and the
// ScheduleWake insert lost the wake silently. The H-3 fix added a
// wake_scheduled column to the outbox and a markWakeScheduled helper
// that flips the flag after a successful booking.
//
// This file owns the recovery path: ReconcileUnscheduledCascadeWakes
// scans for rows where status='materialized' AND wake_scheduled=0 and
// re-schedules the wake. The caller is mpm-scheduler, which invokes
// the function on a 60-second tick (a 5s base tick * 12 = 60s).
//
// Idempotency: each recovered row's wake_scheduled flag flips to 1
// before the next reconcile scan, so re-running never double-books.
// The recovery loop uses LIMIT 100 per pass to bound work and avoid
// holding a long-running transaction.
package internal

import (
	"context"
	"fmt"
	"time"
)

// ReconcileUnscheduledCascadeWakes scans the cascade outbox for rows
// that have been marked materialized but whose wake_scheduled flag is
// still 0, and re-schedules the cascade wake for each. Returns the
// number of wakes successfully recovered.
//
// Concurrency model: the SELECT-then-UPDATE pattern is racy on its
// own (two passes can SELECT the same row before either commits the
// flag flip, causing double-booking). The fix is to schedule the wake
// FIRST and atomically claim the row via an UPDATE with
// `WHERE wake_scheduled = 0`. The UPDATE returns RowsAffected=1 only
// if THIS pass won the race; if another pass already flipped the
// flag, the UPDATE affects 0 rows and the duplicate wake this pass
// created is removed to keep wake count bounded.
//
// The removal is conditional on the duplicate still being PENDING
// (see deleteOrphanWake). A duplicate the agent's fold has already
// settled is not removed — it is a delivery record, and the fact
// that the obligation was delivered twice is logged rather than
// papered over.
//
// The schedule-first order is deliberate and is NOT changed here. The
// other order (claim, then schedule) would close the duplicate window
// but open a LOSS window instead: a crash between the claim and the
// schedule would leave the outbox row flagged wake_scheduled=1 with
// no wake, and the reconciler would never retry it. Trading a
// duplicate for a lost obligation is not an improvement; the durable
// fix is a claim with a lease, which belongs to the delivery-state
// work (Stage B/C), not to this pass.
//
// The function is idempotent: re-running on the same database will
// recover zero rows once the prior pass has flushed its flag writes.
// On a wake-scheduling failure for a single row, the loop logs and
// continues — the next tick will retry the row.
//
// Wiring: mpm-scheduler invokes this function on a 60s cadence via
// cmd/mpm-scheduler/main.go. The function is also safe to call
// directly from any consumer that holds a *DatabaseManager.
func ReconcileUnscheduledCascadeWakes(ctx context.Context, dm *DatabaseManager) (int, error) {
	rows, err := dm.db.QueryContext(ctx, `
		SELECT id, materialized_theory_id, invalidation_event_id
		FROM epistemic_cascade_outbox
		WHERE status = 'materialized' AND wake_scheduled = 0
		ORDER BY updated_at ASC
		LIMIT 100
	`)
	if err != nil {
		return 0, fmt.Errorf("query unscheduled cascade wakes: %w", err)
	}
	defer rows.Close()

	type pendingRow struct {
		intentID            string
		theoryID            string
		invalidationEventID string
	}
	var pending []pendingRow
	for rows.Next() {
		var p pendingRow
		if scanErr := rows.Scan(&p.intentID, &p.theoryID, &p.invalidationEventID); scanErr != nil {
			return 0, fmt.Errorf("scan unscheduled cascade wake row: %w", scanErr)
		}
		pending = append(pending, p)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return 0, fmt.Errorf("iterate unscheduled cascade wake rows: %w", rowsErr)
	}
	rows.Close()

	recovered := 0
	for _, p := range pending {
		if err := ctx.Err(); err != nil {
			return recovered, err
		}

		reason := fmt.Sprintf("cascade-reconcile: theory %s requires review — downstream of invalidation %s (recovered from unscheduled state)",
			p.theoryID, p.invalidationEventID)
		meta := map[string]interface{}{
			"kind":                  "cascade",
			"theory_id":             p.theoryID,
			"invalidation_event_id": p.invalidationEventID,
			"source":                "cascade-reconciler",
		}

		wakeResult, schedErr := dm.ScheduleWake(reason, time.Now().Format(time.RFC3339),
			p.theoryID, "cascade-reconciler", meta)
		if schedErr != nil {
			dm.LogAudit(AuditWarn, "cascade-reconciler",
				fmt.Sprintf("failed to reschedule wake for theory=%s intent=%s: %v",
					p.theoryID, p.intentID, schedErr), "", nil)
			continue
		}

		// Atomic claim via conditional UPDATE. If another reconcile
		// pass already flipped wake_scheduled=1 between our SELECT and
		// this UPDATE, RowsAffected=0 and the duplicate wake we just
		// scheduled must be reclaimed to keep wake count bounded.
		//
		// The reclaim is conditional on the duplicate still being
		// PENDING: a duplicate the agent's fold already settled is a
		// delivery record and is left in place (deleteOrphanWake
		// logs the difference).
		res, claimErr := dm.db.ExecContext(ctx, `
			UPDATE epistemic_cascade_outbox
			SET wake_scheduled = 1, updated_at = ?
			WHERE id = ? AND wake_scheduled = 0
		`, time.Now().Unix(), p.intentID)
		if claimErr != nil {
			// Roll back the just-scheduled wake so we don't leave a
			// orphan wake for a row that another pass already owns.
			deleteOrphanWake(dm, wakeResult)
			dm.LogAudit(AuditError, "cascade-reconciler",
				fmt.Sprintf("failed to atomically claim wake_scheduled for intent=%s: %v",
					p.intentID, claimErr), "", nil)
			return recovered, fmt.Errorf("atomic claim wake_scheduled for intent=%s: %w", p.intentID, claimErr)
		}
		affected, raErr := res.RowsAffected()
		if raErr != nil {
			deleteOrphanWake(dm, wakeResult)
			return recovered, fmt.Errorf("rows-affected for claim: %w", raErr)
		}
		if affected == 0 {
			// Lost the race: another pass already flipped the flag.
			// Reclaim the duplicate wake we just scheduled — unless
			// the fold already delivered it, in which case
			// deleteOrphanWake preserves the delivery record and says
			// so.
			deleteOrphanWake(dm, wakeResult)
			dm.LogAudit(AuditInfo, "cascade-reconciler",
				fmt.Sprintf("lost race for intent=%s theory=%s — duplicate wake reclaimed",
					p.intentID, p.theoryID), "", nil)
			continue
		}

		recovered++
		dm.LogAudit(AuditInfo, "cascade-reconciler",
			fmt.Sprintf("recovered lost cascade wake for theory=%s intent=%s",
				p.theoryID, p.intentID), "", nil)
	}

	return recovered, nil
}

// deleteOrphanWake removes a wake that was scheduled but lost the
// race to own its outbox row. The wakeResult map is the return value
// of dm.ScheduleWake (contains "id"). Best-effort: a delete failure
// is logged but does not surface to the caller.
//
// THE DELETE IS A COMPARE-AND-SET ON THE PENDING STATE.
//
// The row is removed only if it is STILL PENDING (fired = 0). The
// `AND fired = 0` guard is load-bearing:
//
//   - The wake this reconciler just created is immediately due, so
//     the agent's opportunistic fold can select, settle (fired=1,
//     fired_at) and PRESENT it between this reconciler's
//     ScheduleWake and its claim UPDATE.
//   - An unguarded DELETE removed the settled row too. The delivery
//     had already happened — the agent holds the wake — and the
//     substrate's copy of it, including the fired/fired_at pair that
//     `list_wakes --include-fired` surfaces as the forensic record
//     of a delivery, was destroyed.
//
// The reconciler owns a ROW it created; it does not own a DELIVERY
// that already happened. A row that is no longer pending is not an
// orphan, and the two cases are logged differently so an operator can
// tell them apart: a deleted orphan is a cleanup, a skipped settled
// row is evidence that the obligation was delivered TWICE (the
// reconciler created a duplicate wake, and the fold delivered it
// before this pass could reclaim it).
func deleteOrphanWake(dm *DatabaseManager, wakeResult map[string]interface{}) {
	wakeID, ok := wakeResult["id"].(string)
	if !ok || wakeID == "" {
		return
	}
	res, err := dm.ExecTracked(
		`DELETE FROM scheduled_wakes WHERE id = ? AND fired = 0`, 5, wakeID,
	)
	if err != nil {
		dm.LogAudit(AuditWarn, "cascade-reconciler",
			fmt.Sprintf("failed to delete orphan wake id=%s: %v", wakeID, err), "", nil)
		return
	}
	affected, raErr := res.RowsAffected()
	if raErr != nil {
		dm.LogAudit(AuditWarn, "cascade-reconciler",
			fmt.Sprintf("delete orphan wake id=%s: rows-affected: %v", wakeID, raErr), "", nil)
		return
	}
	if affected == 0 {
		// The row is gone, or it is no longer pending. A settled row
		// is NOT an orphan — it was delivered, and its row is the
		// delivery's audit record. Say so rather than letting the
		// absence read as a silent cleanup.
		//
		// The duplicate-delivery reading is the one to look at: the
		// reconciler's compensation for a lost claim cannot reclaim a
		// wake the fold already delivered, so the agent may hold the
		// same obligation twice. That is visible here rather than
		// silent.
		dm.LogAudit(AuditInfo, "cascade-reconciler",
			fmt.Sprintf("orphan wake id=%s was already settled (fired=1) — delivery record preserved; "+
				"the obligation may have been delivered twice", wakeID), "", nil)
		return
	}
	dm.LogAudit(AuditInfo, "cascade-reconciler",
		fmt.Sprintf("deleted orphan wake id=%s (still pending)", wakeID), "", nil)
}
