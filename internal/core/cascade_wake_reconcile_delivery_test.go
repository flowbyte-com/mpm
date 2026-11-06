// cascade_wake_reconcile_delivery_test.go — the reconciler's
// compensation must not destroy a delivery.
//
// THE DEFECT THIS FILE PINS.
//
// internal/core/cascade_wake_reconcile.go's deleteOrphanWake ran
//
//	DELETE FROM scheduled_wakes WHERE id = ?
//
// with NO fired-state guard. The reconciler creates a wake and then
// claims the outbox row that owns it; when it loses that claim it
// deletes the wake it created.
//
// The wake it creates is immediately due, so the agent's opportunistic
// fold can select, settle (fired=1, fired_at) and PRESENT it between
// the reconciler's ScheduleWake and its claim UPDATE. The unguarded
// DELETE then removed the SETTLED row: the delivery had already
// happened — the agent holds the wake — and the substrate's record of
// it, including the fired/fired_at pair that `list_wakes
// --include-fired` surfaces as the forensic view of a delivery, was
// destroyed.
//
// The reconciler owns a ROW it created. It does not own a DELIVERY
// that already happened.
//
// WHAT THIS FILE DOES NOT CLAIM.
//
// It does not claim that a cascade obligation is delivered exactly
// once. The reconciler's schedule-then-claim order leaves a narrow
// window in which the same obligation is delivered twice (see
// internal/core/cascade_materializer.go and the MEDIUM-2 note in
// docs/SPEC.md). That window is a Stage B/C concern — a claim with a
// lease — and this file pins only what it can pin deterministically:
// that a delivered row is not deleted, and that a genuinely orphaned
// PENDING row is still cleaned up.
//
// Every test uses a disposable in-memory database (NewTestDM) and
// never touches the operator's.
package internal

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seedCascadeOutboxRow inserts an outbox row that the reconciler will
// pick up: status='materialized' and wake_scheduled=0 is exactly the
// state the H-3 recovery pass scans for.
func seedCascadeOutboxRow(t *testing.T, dm *DatabaseManager, id, theoryID, eventID string) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO epistemic_cascade_outbox
			(id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
			 downstream_artifact_id, downstream_artifact_type, status,
			 materialized_theory_id, wake_scheduled)
		VALUES (?, ?, ?, 'lesson', ?, 'theory', 'materialized', ?, 0)
	`, id, eventID, "dead-"+id, "down-"+id, theoryID)
	require.NoError(t, err, "seed cascade outbox row %s", id)
}

// cascadeWakeRowCount counts the wake rows whose id matches a prefix.
func cascadeWakeRowCount(t *testing.T, dm *DatabaseManager, prefix string) int {
	t.Helper()
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE id LIKE ?`, prefix+"%",
	).Scan(&n))
	return n
}

// TestDeleteOrphanWake_PreservesADeliveredWake is the headline
// regression: the reviewer's exact probe, against the REAL function.
//
// Pre-fix: the delivered row is DELETED and the count is 0.
func TestDeleteOrphanWake_PreservesADeliveredWake(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const wakeID = "wk-reconcile-delivered"
	_, err := dm.SQLDB().Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, 'cascade: theory th-1 requires review', 0, 'cascade-reconciler', ?)`,
		wakeID, now.Add(-time.Minute).Unix(),
		`{"kind":"cascade","theory_id":"th-1","invalidation_event_id":"ev-1"}`,
	)
	require.NoError(t, err)

	// The agent's fold settles the wake and presents it.
	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, due, 1, "the default fold must deliver the cascade wake")
	require.Equal(t, wakeID, due[0]["id"])

	var fired int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE id = ?`, wakeID,
	).Scan(&fired))
	require.Equal(t, 1, fired, "the delivered wake must be settled")

	// The reconciler now loses the claim and compensates.
	deleteOrphanWake(dm, map[string]interface{}{"id": wakeID})

	// The delivery record MUST survive.
	var count int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE id = ?`, wakeID,
	).Scan(&count))
	require.Equal(t, 1, count,
		"the reconciler deleted a DELIVERED wake: the agent already holds it and the "+
			"substrate's record of the delivery must survive")
}

// TestDeleteOrphanWake_RemovesAGenuinelyOrphanedPendingWake pins the
// legitimate cleanup: the reconciler's compensation must still work.
//
// A wake the reconciler created and lost the claim on is pending, and
// it IS an orphan — nobody owns it and it would be delivered as a
// duplicate obligation. It must be removed.
func TestDeleteOrphanWake_RemovesAGenuinelyOrphanedPendingWake(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	const wakeID = "wk-reconcile-orphan"
	_, err := dm.SQLDB().Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, 'cascade: theory th-2 requires review', 0, 'cascade-reconciler', ?)`,
		wakeID, now.Add(-time.Minute).Unix(),
		`{"kind":"cascade","theory_id":"th-2","invalidation_event_id":"ev-2"}`,
	)
	require.NoError(t, err)

	deleteOrphanWake(dm, map[string]interface{}{"id": wakeID})

	var count int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE id = ?`, wakeID,
	).Scan(&count))
	require.Equal(t, 0, count,
		"a genuinely orphaned PENDING wake must still be cleaned up: it is a duplicate "+
			"of an obligation another pass already owns")
}

// TestDeleteOrphanWake_IgnoresAnEmptyResult pins the input guard.
func TestDeleteOrphanWake_IgnoresAnEmptyResult(t *testing.T) {
	dm := NewTestDM(t)

	// None of these name a wake. None may panic, and none may delete
	// anything.
	deleteOrphanWake(dm, nil)
	deleteOrphanWake(dm, map[string]interface{}{})
	deleteOrphanWake(dm, map[string]interface{}{"id": ""})
	deleteOrphanWake(dm, map[string]interface{}{"id": 42})

	var count int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes`,
	).Scan(&count))
	require.Equal(t, 0, count)
}

// TestReconcileUnscheduledCascadeWakes_RecoversAnUnscheduledWake is
// the reconciler's normal job. It must still work: the H-3 recovery
// path is the reason the file exists.
func TestReconcileUnscheduledCascadeWakes_RecoversAnUnscheduledWake(t *testing.T) {
	dm := NewTestDM(t)
	seedCascadeOutboxRow(t, dm, "intent-recover", "th-recover", "ev-recover")

	recovered, err := ReconcileUnscheduledCascadeWakes(t.Context(), dm)
	require.NoError(t, err)
	require.Equal(t, 1, recovered, "the reconciler must recover the unscheduled wake")
	require.Equal(t, 1, cascadeWakeRowCount(t, dm, "wk-"),
		"the reconciler must have created exactly one wake")

	// The outbox row is now booked.
	var scheduled int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT wake_scheduled FROM epistemic_cascade_outbox WHERE id = 'intent-recover'`,
	).Scan(&scheduled))
	require.Equal(t, 1, scheduled, "the outbox row must be booked")
}

// TestReconcileUnscheduledCascadeWakes_DeliveredWakeSurvivesALostClaim
// is the END-TO-END reproduction of the reviewer's trigger (b): two
// overlapping reconciler passes over the same outbox row, where the
// losing pass's compensation targets a wake the fold has already
// delivered.
//
// The trigger reproduces the interleaving deterministically:
//
//   - BEFORE the losing pass's claim UPDATE, it settles the wake the
//     reconciler just created (the fold winning the race), and
//   - RAISE(IGNORE) skips the claim itself, so the UPDATE affects 0
//     rows and the reconciler takes its "lost the race" path and
//     compensates by deleting the wake it created.
//
// Pre-fix: that compensation deletes the DELIVERED wake.
func TestReconcileUnscheduledCascadeWakes_DeliveredWakeSurvivesALostClaim(t *testing.T) {
	dm := NewTestDM(t)
	seedCascadeOutboxRow(t, dm, "intent-lost-claim", "th-lost", "ev-lost")

	// The interleaving trigger.
	_, err := dm.SQLDB().Exec(`
		CREATE TRIGGER simulate_fold_wins_the_race
		BEFORE UPDATE ON epistemic_cascade_outbox
		WHEN OLD.id = 'intent-lost-claim'
		BEGIN
			UPDATE scheduled_wakes
			   SET fired = 1, fired_at = CAST(strftime('%s','now') AS INTEGER)
			 WHERE id = (
				SELECT id FROM scheduled_wakes
				 WHERE created_by = 'cascade-reconciler'
				 ORDER BY created_at DESC, id DESC
				 LIMIT 1);
			SELECT RAISE(IGNORE);
		END;
	`)
	require.NoError(t, err, "install the interleaving trigger")

	recovered, err := ReconcileUnscheduledCascadeWakes(t.Context(), dm)
	require.NoError(t, err)
	require.Equal(t, 0, recovered, "the pass lost the claim, so it recovered nothing")

	// The wake the reconciler created was settled by the simulated
	// fold and must still be there.
	var count, fired int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE created_by = 'cascade-reconciler'`,
	).Scan(&count))
	require.Equal(t, 1, count,
		"the reconciler deleted a wake that had already been delivered: the agent "+
			"holds it and the substrate's delivery record must survive")

	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE created_by = 'cascade-reconciler'`,
	).Scan(&fired))
	require.Equal(t, 1, fired, "the delivered wake must remain settled")
}

// TestReconcileUnscheduledCascadeWakes_ReclaimsAPendingDuplicate is
// the legitimate half of the compensation, at the reconciler level.
//
// The losing pass created a duplicate wake that nobody delivered. It
// is pending, it IS an orphan, and it must be removed — otherwise the
// duplicate obligation reaches the agent.
func TestReconcileUnscheduledCascadeWakes_ReclaimsAPendingDuplicate(t *testing.T) {
	dm := NewTestDM(t)
	seedCascadeOutboxRow(t, dm, "intent-dup-pending", "th-dup", "ev-dup")

	// Same interleaving as the delivered case, but the trigger does
	// NOT settle the wake: the duplicate stays pending, so the
	// compensation must remove it.
	_, err := dm.SQLDB().Exec(`
		CREATE TRIGGER simulate_lost_claim
		BEFORE UPDATE ON epistemic_cascade_outbox
		WHEN OLD.id = 'intent-dup-pending'
		BEGIN
			SELECT RAISE(IGNORE);
		END;
	`)
	require.NoError(t, err, "install the lost-claim trigger")

	recovered, err := ReconcileUnscheduledCascadeWakes(t.Context(), dm)
	require.NoError(t, err)
	require.Equal(t, 0, recovered)

	var count int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE created_by = 'cascade-reconciler'`,
	).Scan(&count))
	require.Equal(t, 0, count,
		"a duplicate wake that nobody delivered is an orphan and must be reclaimed")
}

// TestCascadeReconcile_ConcurrentDeliveryAndReconciliation runs the
// fold and the reconciler against the same database at the same time.
//
// The invariant under contention is narrow and it is the one that
// matters: a wake the fold DELIVERED keeps its row. The reconciler
// may reclaim a wake that was never delivered, and the fold may
// deliver a wake the reconciler created — both are fine. What must
// never happen is the loss of a delivery record.
//
// It also pins the two properties the reconciler must not break while
// racing the fold: it must not book the same outbox row twice (a
// persistent reconciliation loop), and it must not accumulate wakes
// without bound (one wake per outbox row, at most).
func TestCascadeReconcile_ConcurrentDeliveryAndReconciliation(t *testing.T) {
	dm := NewTestDM(t)

	const iterations = 40
	for i := 0; i < iterations; i++ {
		id := fmt.Sprintf("intent-race-%02d", i)
		seedCascadeOutboxRow(t, dm, id, "th-race-"+id, "ev-race-"+id)
	}

	var mu sync.Mutex
	deliveredCount := map[string]int{}

	var wg sync.WaitGroup
	wg.Add(2)

	// The agent's fold, delivering continuously.
	go func() {
		defer wg.Done()
		for j := 0; j < 12; j++ {
			due, _ := dm.CheckPendingWakes(time.Now(), nil)
			mu.Lock()
			for _, w := range due {
				if id, ok := w["id"].(string); ok {
					deliveredCount[id]++
				}
			}
			mu.Unlock()
		}
	}()

	// The reconciler, recovering continuously.
	go func() {
		defer wg.Done()
		for j := 0; j < 4; j++ {
			_, _ = ReconcileUnscheduledCascadeWakes(t.Context(), dm)
		}
	}()

	wg.Wait()

	// Every wake the fold delivered must STILL BE THERE, settled.
	mu.Lock()
	delivered := make(map[string]int, len(deliveredCount))
	for id, n := range deliveredCount {
		delivered[id] = n
	}
	mu.Unlock()

	require.NotEmpty(t, delivered, "the race must have delivered at least one wake")
	for id, n := range delivered {
		require.Equal(t, 1, n,
			"the fold delivered the wake %s more than once", id)

		var fired int
		err := dm.SQLDB().QueryRow(
			`SELECT fired FROM scheduled_wakes WHERE id = ?`, id,
		).Scan(&fired)
		if err != nil {
			t.Fatalf("the DELIVERED wake %s was DELETED: the reconciler destroyed a "+
				"delivery record: %v", id, err)
		}
		require.Equal(t, 1, fired, "the delivered wake %s must remain settled", id)
	}

	// No persistent reconciliation loop: every outbox row the
	// reconciler saw is booked exactly once.
	var unbooked int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE wake_scheduled = 0`,
	).Scan(&unbooked))
	require.Equal(t, 0, unbooked,
		"every recovered outbox row must be booked: an unbooked row is a permanent "+
			"reconciliation loop")

	// No unbounded orphan accumulation: at most one wake per outbox
	// row survives (a duplicate the reconciler created and lost the
	// claim on is reclaimed).
	var wakeRows, outboxRows int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes`,
	).Scan(&wakeRows))
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox`,
	).Scan(&outboxRows))
	require.LessOrEqual(t, wakeRows, outboxRows,
		"the reconciler must not accumulate wakes without bound: %d wakes for %d outbox rows",
		wakeRows, outboxRows)
}
