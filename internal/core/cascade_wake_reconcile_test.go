// cascade_wake_reconcile_test.go — recovery pass coverage for H-3
// (post-M3 audit, 2026-08-31).
//
// The fix: ReconcileUnscheduledCascadeWakes scans the cascade outbox
// for rows with status='materialized' AND wake_scheduled=0 and
// re-schedules the wake. The wake_scheduled flag then flips to 1
// so a subsequent reconcile pass is idempotent.
//
// Tests:
//   - TestReconcileUnscheduledCascadeWakes_RecoversLostWake: pre-state
//     matches the H-3 failure mode (materialized but wake_scheduled=0);
//     post-state has the wake in scheduled_wakes and the flag at 1.
//   - TestReconcileUnscheduledCascadeWakes_AlreadyScheduled: a row with
//     wake_scheduled=1 is *not* re-scheduled (recovered=0).
//   - TestReconcileUnscheduledCascadeWakes_PendingRowSkipped: a row that
//     has not yet been materialized (status='pending') is skipped.
//   - TestReconcileUnscheduledCascadeWakes_FailedRowSkipped: a row in
//     terminal dead-letter state (status='failed') is skipped.
package internal

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReconcileUnscheduledCascadeWakes_RecoversLostWake is the load-
// bearing regression for the H-3 audit finding. Pre-fix, a crash
// between markMaterialized (which the H-2 fix made durable via
// read-back) and ScheduleWake left a "materialized but unscheduled"
// outbox row that no process would revisit. The reconcile pass closes
// that gap.
func TestReconcileUnscheduledCascadeWakes_RecoversLostWake(t *testing.T) {
	dm := hermeticDatabaseManager(t)
	defer dm.Close()

	intentID := insertCascadeIntent(t, dm, "mem-pending-h3")
	theoryID := "theory-recovered-h3"

	// Simulate the post-markMaterialized, pre-ScheduleWake crash window.
	_, err := dm.db.Exec(`
		UPDATE epistemic_cascade_outbox
		SET status = 'materialized',
		    materialized_theory_id = ?,
		    wake_scheduled = 0
		WHERE id = ?
	`, theoryID, intentID)
	require.NoError(t, err)

	// Sanity: no wake exists yet for this theory.
	preCount := countWakesForTheory(t, dm, theoryID)
	require.Equal(t, 0, preCount, "pre-state: no wake should exist")

	recovered, err := ReconcileUnscheduledCascadeWakes(context.Background(), dm)
	require.NoError(t, err, "reconcile pass must surface errors")
	require.Equal(t, 1, recovered, "exactly one wake should be recovered")

	// Post-state: wake exists in scheduled_wakes.
	postCount := countWakesForTheory(t, dm, theoryID)
	require.Equal(t, 1, postCount, "post-state: wake must be booked")

	// Post-state: flag flipped to 1 (idempotency).
	var flag int
	err = dm.db.QueryRow(
		`SELECT wake_scheduled FROM epistemic_cascade_outbox WHERE id = ?`,
		intentID,
	).Scan(&flag)
	require.NoError(t, err)
	require.Equal(t, 1, flag, "wake_scheduled flag must flip to 1 after recovery")
}

// TestReconcileUnscheduledCascadeWakes_AlreadyScheduled verifies the
// idempotency guarantee: a row that already has wake_scheduled=1 is
// not re-scheduled on a subsequent pass.
func TestReconcileUnscheduledCascadeWakes_AlreadyScheduled(t *testing.T) {
	dm := hermeticDatabaseManager(t)
	defer dm.Close()

	intentID := insertCascadeIntent(t, dm, "mem-pending-h3-already")
	theoryID := "theory-already-h3"

	_, err := dm.db.Exec(`
		UPDATE epistemic_cascade_outbox
		SET status = 'materialized',
		    materialized_theory_id = ?,
		    wake_scheduled = 1
		WHERE id = ?
	`, theoryID, intentID)
	require.NoError(t, err)

	recovered, err := ReconcileUnscheduledCascadeWakes(context.Background(), dm)
	require.NoError(t, err)
	require.Equal(t, 0, recovered, "already-scheduled rows must be skipped")

	// Verify no wake was created (count remains 0).
	postCount := countWakesForTheory(t, dm, theoryID)
	require.Equal(t, 0, postCount, "no wake should be created for already-scheduled rows")
}

// TestReconcileUnscheduledCascadeWakes_PendingRowSkipped verifies the
// pre-materialization filter: a row that has not yet been marked
// materialized must NOT be reconciled (its wake is the materializer's
// job, not the reconciler's).
func TestReconcileUnscheduledCascadeWakes_PendingRowSkipped(t *testing.T) {
	dm := hermeticDatabaseManager(t)
	defer dm.Close()

	intentID := insertCascadeIntent(t, dm, "mem-pending-h3-pending")
	// Row is 'pending' by default from insertCascadeIntent.
	// wake_scheduled defaults to 0 via schema.
	theoryID := "theory-pending-skipped-h3"

	recovered, err := ReconcileUnscheduledCascadeWakes(context.Background(), dm)
	require.NoError(t, err)
	require.Equal(t, 0, recovered, "pending rows must be skipped")

	// Confirm: the row is unchanged and no wake exists.
	var status string
	var flag int
	err = dm.db.QueryRow(
		`SELECT status, wake_scheduled FROM epistemic_cascade_outbox WHERE id = ?`,
		intentID,
	).Scan(&status, &flag)
	require.NoError(t, err)
	require.Equal(t, "pending", status)
	require.Equal(t, 0, flag)
	postCount := countWakesForTheory(t, dm, theoryID)
	require.Equal(t, 0, postCount, "no wake should be created for pending rows")
}

// TestReconcileUnscheduledCascadeWakes_FailedRowSkipped verifies the
// terminal-state filter: a row in dead-letter (status='failed') must
// NOT be reconciled — there is no theory to wake for.
func TestReconcileUnscheduledCascadeWakes_FailedRowSkipped(t *testing.T) {
	dm := hermeticDatabaseManager(t)
	defer dm.Close()

	intentID := insertCascadeIntent(t, dm, "mem-pending-h3-failed")
	_, err := dm.db.Exec(
		`UPDATE epistemic_cascade_outbox SET status = 'failed', wake_scheduled = 0 WHERE id = ?`,
		intentID,
	)
	require.NoError(t, err)

	recovered, err := ReconcileUnscheduledCascadeWakes(context.Background(), dm)
	require.NoError(t, err)
	require.Equal(t, 0, recovered, "failed rows must be skipped")
}

// countWakesForTheory returns the number of scheduled_wakes rows tied
// to the given theory_id. Isolated here so the test file is self-
// contained.
func countWakesForTheory(t *testing.T, dm *DatabaseManager, theoryID string) int {
	t.Helper()
	var n int
	err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE theory_id = ?`,
		theoryID,
	).Scan(&n)
	require.NoError(t, err)
	return n
}
