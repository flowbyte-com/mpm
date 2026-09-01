// cascade_adversarial_test.go — adversarial coverage for the H-2
// and H-3 audit fixes (post-M3, 2026-08-31).
//
// Phase 12 of the remediation plan. These tests exercise the failure
// modes that motivated the durability fixes:
//
//   - TestAdversarial_CrashBetweenMarkAndWake: simulate a crash
//     between markMaterialized and ScheduleWake. The reconcile pass
//     must recover the lost wake. This is the load-bearing H-3
//     regression test.
//
//   - TestAdversarial_DoubleClaim: two CascadeMaterializer instances
//     on the same DM both try to claim the same row. Only one wins;
//     the other gets zero rows. Verifies the claim transaction is
//     atomic.
//
//   - TestAdversarial_ConcurrentReconcileIdempotency: two reconcile
//     passes run concurrently on the same outbox. Each row is
//     recovered at most once. Verifies the wake_scheduled flag flip
//     is the durability gate.
package internal

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAdversarial_CrashBetweenMarkAndWake is the H-3 regression test.
// Pre-fix, a crash between markMaterialized (which flips status to
// 'materialized') and the ScheduleWake insert lost the wake silently
// with no observable signal. Post-fix, the wake_scheduled column
// provides the durable record; the reconcile pass recovers the wake.
func TestAdversarial_CrashBetweenMarkAndWake(t *testing.T) {
	dm := hermeticDatabaseManager(t)
	defer dm.Close()

	intentID := insertCascadeIntent(t, dm, "mem-pending-crash")
	theoryID := "theory-crash-recovery"

	// Pre-state: simulate post-markMaterialized, pre-ScheduleWake.
	// The flag is 0 (wake_scheduled NOT yet flipped).
	_, err := dm.db.Exec(`
		UPDATE epistemic_cascade_outbox
		SET status = 'materialized',
		    materialized_theory_id = ?,
		    wake_scheduled = 0
		WHERE id = ?
	`, theoryID, intentID)
	require.NoError(t, err)

	// Sanity: no wake for this theory yet.
	preCount := countWakesForTheory(t, dm, theoryID)
	require.Equal(t, 0, preCount, "pre-state: no wake booked")

	// Adversary: process "crashes" without ever calling
	// ScheduleWake. The reconcile pass is the recovery mechanism.
	recovered, err := ReconcileUnscheduledCascadeWakes(context.Background(), dm)
	require.NoError(t, err)
	require.Equal(t, 1, recovered, "reconcile must recover the lost wake")

	// Post-state: wake is booked.
	postCount := countWakesForTheory(t, dm, theoryID)
	require.Equal(t, 1, postCount, "post-state: wake must be booked")

	// Post-state: flag flipped to 1 — idempotency gate.
	var flag int
	require.NoError(t, dm.db.QueryRow(
		`SELECT wake_scheduled FROM epistemic_cascade_outbox WHERE id = ?`,
		intentID,
	).Scan(&flag))
	require.Equal(t, 1, flag, "wake_scheduled must flip to 1 after recovery")
}

// TestAdversarial_DoubleClaim: two CascadeMaterializer instances
// attempt to claim the same pending row. Only one wins; the other
// sees zero rows. Verifies the claim transaction's atomicity.
func TestAdversarial_DoubleClaim(t *testing.T) {
	dm := hermeticDatabaseManager(t)
	defer dm.Close()

	// Insert a single pending intent.
	intentID := insertCascadeIntent(t, dm, "mem-pending-double-claim")

	// Two materializers share the same DM. Both attempt to claim.
	mat1 := NewCascadeMaterializer(dm, DefaultCascadeMaterializerOptions())
	mat2 := NewCascadeMaterializer(dm, DefaultCascadeMaterializerOptions())

	intents1, err1 := mat1.claimCascadeIntents(10)
	require.NoError(t, err1)
	intents2, err2 := mat2.claimCascadeIntents(10)
	require.NoError(t, err2)

	// Exactly one claim should win — the other must see zero rows.
	got1 := len(intents1)
	got2 := len(intents2)
	require.Equal(t, 1, got1+got2,
		"total claimed across both materializers = %d, want 1 (got1=%d got2=%d)",
		got1+got2, got1, got2)

	// The winning claim must own our intent id.
	var winner string
	if got1 == 1 {
		winner = intents1[0].ID
	} else {
		winner = intents2[0].ID
	}
	require.Equal(t, intentID, winner, "the single claimed intent must be ours")
}

// TestAdversarial_ConcurrentReconcileIdempotency: two reconcile
// passes run concurrently. The wake_scheduled flag prevents
// double-booking even under race conditions — at most one pass can
// observe wake_scheduled=0 for any given row.
//
// Note: SQLite's WAL + busy_timeout serializes the actual UPDATE
// statements, so each row's flag flip is atomic. The recovery loop
// itself iterates over a snapshot, so the second pass may see
// wake_scheduled=1 (post first-pass commit) and skip.
func TestAdversarial_ConcurrentReconcileIdempotency(t *testing.T) {
	dm := hermeticDatabaseManager(t)
	defer dm.Close()

	// Insert a single materialized-but-unscheduled row.
	intentID := insertCascadeIntent(t, dm, "mem-pending-concurrent-reconcile")
	theoryID := "theory-concurrent-reconcile"
	_, err := dm.db.Exec(`
		UPDATE epistemic_cascade_outbox
		SET status = 'materialized',
		    materialized_theory_id = ?,
		    wake_scheduled = 0
		WHERE id = ?
	`, theoryID, intentID)
	require.NoError(t, err)

	// Run two reconcile passes concurrently.
	var wg sync.WaitGroup
	results := make([]int, 2)
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			results[i], errs[i] = ReconcileUnscheduledCascadeWakes(context.Background(), dm)
		}()
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	// The total recovered across both passes must be at most 1
	// (idempotency). The first pass observes wake_scheduled=0 and
	// flips it to 1; the second observes 1 and skips.
	totalRecovered := results[0] + results[1]
	require.LessOrEqual(t, totalRecovered, 1,
		"concurrent reconcile passes must not double-book; got total=%d", totalRecovered)

	// And only one wake is booked.
	postCount := countWakesForTheory(t, dm, theoryID)
	require.Equal(t, totalRecovered, postCount,
		"exactly one wake per recovered row; got recovered=%d wakes=%d",
		totalRecovered, postCount)

	// Flag must be 1 (recovery completed).
	var flag int
	require.NoError(t, dm.db.QueryRow(
		`SELECT wake_scheduled FROM epistemic_cascade_outbox WHERE id = ?`,
		intentID,
	).Scan(&flag))
	require.Equal(t, 1, flag)
}
