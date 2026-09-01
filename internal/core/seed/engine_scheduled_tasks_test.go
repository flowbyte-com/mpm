// engine_scheduled_tasks_test.go — tests for the Baseline Cognitive
// Bootstrap for scheduled tasks (SeedScheduledTasks /
// seedBaselineScheduledTasks).
//
// Pins the contract:
//
//   1. First run creates the canonical row (id=epistemic-compaction,
//      cron=0 3 * * *, directive_id=mpm-seed-epistemic-compaction-policy,
//      status=active).
//   2. Re-run is a no-op: the existing row is in Skipped, its custom
//      cron / name / directive_id / status are preserved verbatim.
//   3. Operator's local edit is preserved: a manual `mpm tasks upsert`
//      that pauses the task stays paused across re-runs.
//   4. If the named directive is not seeded, the task is in the
//      Missing bucket (not Created, not Skipped). The operator can
//      run `mpm ops init directives` first and re-run.
//
// The tests use a fresh sqlite3 file via NewTestDM so they never
// touch the workspace database. They explicitly call
// seedBaselineDirectives before seedBaselineScheduledTasks to mirror
// the production boot order in NewDatabaseManager.
package seed_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/flowbyte-com/mpm-core/seed"
)

// TestSeedBaselineScheduledTasks_FirstRunCreates pins the canonical
// row shape on first run. Catches drift if a future contributor
// changes the seed's StableID, CronExpr, DirectiveID, or Status.
func TestSeedBaselineScheduledTasks_FirstRunCreates(t *testing.T) {
	dm := newTestDM(t)
	// Production boot order: directives first, then tasks. Mirrors
	// the order in NewDatabaseManager.
	_, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	summary, err := dm.SeedBaselineScheduledTasks()
	require.NoError(t, err)

	require.Equal(t, len(seed.SeedScheduledTasks), len(summary.Created),
		"first run should create every seeded task, got Created=%v", summary.Created)
	require.Equal(t, 0, len(summary.Skipped), "first run has nothing to skip")
	require.Equal(t, 0, len(summary.Missing), "first run with directives seeded should have no missing")

	// Verify the canonical row is present and well-formed.
	for _, st := range seed.SeedScheduledTasks {
		var cron, name, directiveID, status string
		var nextRun int64
		err := dm.SQLDB().QueryRow(
			`SELECT cron_expr, name, directive_id, status, next_run_at
			 FROM scheduled_tasks WHERE id = ?`, st.StableID,
		).Scan(&cron, &name, &directiveID, &status, &nextRun)
		require.NoError(t, err, "expected row %s to exist after seed", st.StableID)
		require.Equal(t, st.CronExpr, cron,
			"cron_expr mismatch for %s, got %q want %q", st.StableID, cron, st.CronExpr)
		require.Equal(t, st.Name, name,
			"name mismatch for %s, got %q want %q", st.StableID, name, st.Name)
		require.Equal(t, st.DirectiveID, directiveID,
			"directive_id mismatch for %s, got %q want %q", st.StableID, directiveID, st.DirectiveID)
		require.Equal(t, st.Status, status,
			"status mismatch for %s, got %q want %q", st.StableID, status, st.Status)
		require.Greater(t, nextRun, int64(0), "next_run_at must be a positive Unix timestamp")
	}
}

// TestSeedBaselineScheduledTasks_RerunIsNoOp pins the idempotency
// contract. Re-running on a DB that already has the canonical row
// must NOT overwrite it and must report it as Skipped.
func TestSeedBaselineScheduledTasks_RerunIsNoOp(t *testing.T) {
	dm := newTestDM(t)
	_, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)

	first, err := dm.SeedBaselineScheduledTasks()
	require.NoError(t, err)
	require.Equal(t, len(seed.SeedScheduledTasks), len(first.Created))

	// Re-run on the same DB.
	second, err := dm.SeedBaselineScheduledTasks()
	require.NoError(t, err)
	require.Equal(t, 0, len(second.Created), "second run should not re-create")
	require.Equal(t, len(seed.SeedScheduledTasks), len(second.Skipped),
		"second run should skip all existing rows, got Skipped=%v", second.Skipped)
	require.Equal(t, 0, len(second.Missing), "second run should have no missing")
}

// TestSeedBaselineScheduledTasks_OperatorCustomizationPreserved
// pins the "no auto-noise" voice: a manual edit (e.g. the operator
// changed the cron to "0 */4 * * *" for high ingest, or paused the
// task) MUST be preserved across re-runs. The seed is a starting
// configuration, not a sync target.
func TestSeedBaselineScheduledTasks_OperatorCustomizationPreserved(t *testing.T) {
	dm := newTestDM(t)
	_, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	_, err = dm.SeedBaselineScheduledTasks()
	require.NoError(t, err)

	if len(seed.SeedScheduledTasks) == 0 {
		t.Fatal("seed.SeedScheduledTasks is empty — nothing to test")
	}
	canonical := seed.SeedScheduledTasks[0]

	// Operator upserts the same task with a different cron, name,
	// directive_id, and a paused status. This simulates "operator
	// customised the canonical task for their workspace".
	customCron := "0 */4 * * *"
	customName := "Operator-customized compaction (4h cadence)"
	customDirective := canonical.DirectiveID
	_, err = dm.SQLDB().Exec(`
		UPDATE scheduled_tasks
		SET cron_expr = ?, name = ?, directive_id = ?, status = 'paused'
		WHERE id = ?`,
		customCron, customName, customDirective, canonical.StableID)
	require.NoError(t, err)

	// Re-run the seed.
	summary, err := dm.SeedBaselineScheduledTasks()
	require.NoError(t, err)
	require.Equal(t, 1, len(summary.Skipped),
		"operator's custom task must be skipped, not re-upserted, got Skipped=%v", summary.Skipped)
	require.Equal(t, canonical.StableID, summary.Skipped[0])

	// Verify the operator's values are still in the row.
	var gotCron, gotName, gotStatus string
	err = dm.SQLDB().QueryRow(
		`SELECT cron_expr, name, status FROM scheduled_tasks WHERE id = ?`, canonical.StableID,
	).Scan(&gotCron, &gotName, &gotStatus)
	require.NoError(t, err)
	require.Equal(t, customCron, gotCron, "operator's cron must be preserved")
	require.Equal(t, customName, gotName, "operator's name must be preserved")
	require.Equal(t, "paused", gotStatus, "operator's paused status must be preserved")
}

// TestSeedBaselineScheduledTasks_MissingDirectiveNotCreated pins
// the FK-validation contract: if the canonical directive is absent,
// the task is NOT inserted. The apply loop must not silently create
// a task with a dangling directive_id reference — that would
// surface only at wake time, which is too late. The task lands in
// the Missing bucket instead, and the operator can run
// `mpm ops init directives` first and re-run.
//
// We simulate the missing-directive case by seeding the task
// registry WITHOUT first seeding directives.
func TestSeedBaselineScheduledTasks_MissingDirectiveNotCreated(t *testing.T) {
	dm := newTestDM(t)
	// Deliberately skip ApplyDirectives — the directive does not
	// exist. The task seed must detect this and put the task in
	// the Missing bucket rather than creating a dangling row.
	summary, err := dm.SeedBaselineScheduledTasks()
	require.NoError(t, err)
	require.Equal(t, 0, len(summary.Created),
		"no task may be created when its directive is missing, got Created=%v", summary.Created)
	require.Equal(t, 0, len(summary.Skipped))
	require.Equal(t, len(seed.SeedScheduledTasks), len(summary.Missing),
		"every task whose directive is missing must be in Missing, got Missing=%v", summary.Missing)

	// The DB must be empty.
	var rowCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_tasks`,
	).Scan(&rowCount))
	require.Equal(t, 0, rowCount, "no task rows may exist when directives are missing")
}

// TestSeedBaselineScheduledTasks_DirectiveThenTaskOrder pins the
// production boot order: directives first, then tasks. After both
// runs, the task row exists AND the directive row exists. This is
// the canonical sequence NewDatabaseManager uses.
func TestSeedBaselineScheduledTasks_DirectiveThenTaskOrder(t *testing.T) {
	dm := newTestDM(t)

	// Step 1: seed directives (the production boot does this
	// first).
	dSummary, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	require.Equal(t, len(seed.SeedDirectives), len(dSummary.Created),
		"directives must seed first")

	// Step 2: seed scheduled tasks. With directives present, the
	// task seed must succeed.
	tSummary, err := dm.SeedBaselineScheduledTasks()
	require.NoError(t, err)
	require.Equal(t, len(seed.SeedScheduledTasks), len(tSummary.Created),
		"tasks must seed successfully when directives are present, got Created=%v", tSummary.Created)
	require.Equal(t, 0, len(tSummary.Missing))

	// Verify the directive row exists.
	for _, sd := range seed.SeedDirectives {
		var got string
		require.NoError(t, dm.SQLDB().QueryRow(
			`SELECT id FROM memories WHERE id = ? AND deleted_at IS NULL`, sd.StableID,
		).Scan(&got))
	}
	// Verify the task rows exist.
	for _, st := range seed.SeedScheduledTasks {
		var got string
		require.NoError(t, dm.SQLDB().QueryRow(
			`SELECT id FROM scheduled_tasks WHERE id = ?`, st.StableID,
		).Scan(&got))
	}
}
