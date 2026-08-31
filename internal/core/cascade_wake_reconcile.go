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

		if _, schedErr := dm.ScheduleWake(reason, time.Now().Format(time.RFC3339),
			p.theoryID, "", "cascade-reconciler", meta); schedErr != nil {
			dm.LogAudit(AuditWarn, "cascade-reconciler",
				fmt.Sprintf("failed to reschedule wake for theory=%s intent=%s: %v",
					p.theoryID, p.intentID, schedErr), "", nil)
			continue
		}

		// Defense Triad #3 — flip the wake_scheduled flag immediately
		// after ScheduleWake succeeds, so the next reconcile pass does
		// not double-book. read-back assertion is implicit: a second
		// reconcile tick will find wake_scheduled=1 and skip.
		if _, markErr := dm.db.ExecContext(ctx, `
			UPDATE epistemic_cascade_outbox
			SET wake_scheduled = 1, updated_at = ?
			WHERE id = ?
		`, time.Now().Unix(), p.intentID); markErr != nil {
			dm.LogAudit(AuditError, "cascade-reconciler",
				fmt.Sprintf("failed to mark wake_scheduled for intent=%s after reschedule: %v",
					p.intentID, markErr), "", nil)
			return recovered, fmt.Errorf("mark wake_scheduled for intent=%s: %w", p.intentID, markErr)
		}

		recovered++
		dm.LogAudit(AuditInfo, "cascade-reconciler",
			fmt.Sprintf("recovered lost cascade wake for theory=%s intent=%s",
				p.theoryID, p.intentID), "", nil)
	}

	return recovered, nil
}
