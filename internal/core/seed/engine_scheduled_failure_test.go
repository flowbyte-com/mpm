// engine_scheduled_failure_test.go — failure-path contract for the
// scheduled epistemic-compaction reflex. The architectural claim:
//
//   "Compaction failure should NOT crash the schedule. The next
//    tick should fire and the agent should see a new wake with
//    the directive_id."
//
// This test pins the schedule's survival under compaction failure
// WITHOUT adding any new safety logic. The proof is structural:
// ProcessScheduledTasks is a single transaction that (a) injects
// the wake and (b) rolls over next_run_at. A failed compact call
// downstream of the wake cannot roll either of those back — there
// is no shared transaction, no rollback path, no "mark as done"
// that the scheduler would honor. The next tick fires regardless
// because next_run_at was already rolled forward at injection
// time.
//
// Test plan:
//   1. Seed directive + task.
//   2. Backdate → process → assert wake #1 injected, next_run_at
//      rolled forward.
//   3. SIMULATE FAILURE: do nothing. (In production, the agent
//      reads the wake, calls mpm_system.compact, compact returns
//      failure. None of those steps touch scheduled_tasks.)
//   4. Backdate next_run_at to past again — this models the next
//      cron iteration arriving while the previous wake's compact
//      call has not (yet) succeeded.
//   5. Process → assert wake #2 injected. Same directive_id.
//   6. next_run_at rolled forward again.
//   7. Assert exactly two distinct wake rows exist for the same
//      task_id, proving the schedule did not dedupe or disable
//      itself after the prior failure.
package seed_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/seed"
)

// TestEpistemicCompaction_ScheduleSurvivesCompactionFailure verifies
// that the schedule keeps ticking even when the agent's compact call
// (downstream of the wake) returns failure. The schedule is a wake
// injector — its contract is "deliver the directive, do not act on
// it" — and this test proves a downstream failure has no path to
// disable the next tick.
func TestEpistemicCompaction_ScheduleSurvivesCompactionFailure(t *testing.T) {
	dm := newTestDM(t)

	// Production boot order.
	_, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	_, err = dm.SeedBaselineScheduledTasks()
	require.NoError(t, err)

	// Tick #1: backdate → process → wake #1.
	require.NoError(t, backdateTask(dm, "epistemic-compaction"))
	processed, err := core.ProcessScheduledTasks(dm.SQLDB())
	require.NoError(t, err)
	require.Equal(t, 1, processed, "tick #1 must inject exactly one wake")

	wakeID1 := latestWakeID(t, dm)
	require.NotEmpty(t, wakeID1, "tick #1 must have an injected wake row")

	// Capture the rolled-forward next_run_at from tick #1.
	next1 := nextRunAt(t, dm, "epistemic-compaction")
	require.Greater(t, next1, time.Now().Unix()-1,
		"tick #1 must roll next_run_at forward")

	// SIMULATE FAILURE between ticks. In production this is the
	// gap between the wake being delivered and the agent's
	// compact call returning. We do nothing here — and that is
	// the point. The schedule has no opinion on compact success
	// or failure. It only owns next_run_at.

	// Tick #2: backdate again to model the next cron iteration
	// arriving while compaction is still failing or has failed.
	// This is the production scenario: a 60s daemon tick, a task
	// whose next_run_at is in the past, gets re-injected. There
	// is NO call-site dedupe, NO "previous wake still pending"
	// check, NO failure-aware suppression. The scheduler is
	// dumb on purpose.
	require.NoError(t, backdateTask(dm, "epistemic-compaction"))
	processed, err = core.ProcessScheduledTasks(dm.SQLDB())
	require.NoError(t, err)
	require.Equal(t, 1, processed,
		"tick #2 must inject a fresh wake even though the prior wake's compact (if any) returned failure")

	wakeID2 := latestWakeID(t, dm)
	require.NotEmpty(t, wakeID2)
	require.NotEqual(t, wakeID1, wakeID2,
		"tick #2 must produce a DISTINCT wake id; dedupe would mean the schedule disabled itself after a failure")

	// Both wakes must carry the same directive_id so the agent's
	// triage logic (per mpm-seed-wake-triage-policy) sees the same
	// directive on the retry.
	require.Equal(t, "mpm-seed-epistemic-compaction-policy",
		latestWakeDirectiveID(t, dm))
	require.Equal(t, "mpm-seed-epistemic-compaction-policy",
		earliestWakeDirectiveID(t, dm),
		"the older wake (from tick #1) must also carry the directive_id; "+
			"the schedule did not corrupt the prior wake row when the failure happened")

	// next_run_at was rolled forward a second time. Either to the
	// SAME future instant (if cron math is deterministic on the
	// same wall-clock day) or strictly later. Either way, no
	// rollback, no zero, no stuck-in-the-past.
	next2 := nextRunAt(t, dm, "epistemic-compaction")
	require.GreaterOrEqual(t, next2, next1,
		"next_run_at must not regress after tick #2")

	// Exactly two wake rows for this task. If the test sees three
	// or more, ProcessScheduledTasks double-fired. If it sees one,
	// the failure path suppressed the second tick. Both are bugs.
	var wakeCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes
		 WHERE reason = 'cron:epistemic-compaction'`,
	).Scan(&wakeCount))
	require.Equal(t, 2, wakeCount,
		"exactly two wakes expected (one per tick), got %d", wakeCount)
}

// backdateTask sets next_run_at to one second before now so the
// next ProcessScheduledTasks call treats the row as due.
func backdateTask(dm *core.DatabaseManager, taskID string) error {
	_, err := dm.SQLDB().Exec(`
		UPDATE scheduled_tasks
		SET next_run_at = CAST(strftime('%s','now') AS INTEGER) - 1
		WHERE id = ?`, taskID)
	return err
}

// latestWakeID returns the id of the most recently created wake row
// for the epistemic-compaction task.
func latestWakeID(t *testing.T, dm *core.DatabaseManager) string {
	t.Helper()
	var id string
	require.NoError(t, dm.SQLDB().QueryRow(`
		SELECT id FROM scheduled_wakes
		WHERE reason = 'cron:epistemic-compaction'
		ORDER BY created_at DESC LIMIT 1`,
	).Scan(&id))
	return id
}

// earliestWakeID returns the id of the oldest wake row.
func earliestWakeID(t *testing.T, dm *core.DatabaseManager) string {
	t.Helper()
	var id string
	require.NoError(t, dm.SQLDB().QueryRow(`
		SELECT id FROM scheduled_wakes
		WHERE reason = 'cron:epistemic-compaction'
		ORDER BY created_at ASC LIMIT 1`,
	).Scan(&id))
	return id
}

// latestWakeDirectiveID returns the directive_id from the most
// recently created wake's metadata.
func latestWakeDirectiveID(t *testing.T, dm *core.DatabaseManager) string {
	t.Helper()
	return wakeDirectiveID(t, dm, "DESC")
}

// earliestWakeDirectiveID returns the directive_id from the
// oldest wake's metadata.
func earliestWakeDirectiveID(t *testing.T, dm *core.DatabaseManager) string {
	t.Helper()
	return wakeDirectiveID(t, dm, "ASC")
}

func wakeDirectiveID(t *testing.T, dm *core.DatabaseManager, order string) string {
	t.Helper()
	var metadata string
	require.NoError(t, dm.SQLDB().QueryRow(`
		SELECT metadata FROM scheduled_wakes
		WHERE reason = 'cron:epistemic-compaction'
		ORDER BY created_at `+order+` LIMIT 1`,
	).Scan(&metadata))
	require.Contains(t, metadata, `"directive_id":"mpm-seed-epistemic-compaction-policy"`,
		"wake metadata must carry directive_id, got %q", metadata)
	return "mpm-seed-epistemic-compaction-policy"
}

// nextRunAt returns the next_run_at for a task as a Unix epoch.
func nextRunAt(t *testing.T, dm *core.DatabaseManager, taskID string) int64 {
	t.Helper()
	var next int64
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT next_run_at FROM scheduled_tasks WHERE id = ?`, taskID,
	).Scan(&next))
	return next
}

// silence unused-helper warnings if the file is later trimmed.
var _ = earliestWakeID