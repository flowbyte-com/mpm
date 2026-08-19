// engine_skills_test.go — Task 15 of the skills-layer plan.
//
// Pins three contracts for seed.ApplySkills:
//   1. First run creates the rows (one per seed.SeedSkills entry).
//   2. Re-run is a no-op (idempotent): all rows in Skipped.
//   3. Operator's local edit to the seeded row's content is preserved,
//      not overwritten — flagged in `Updated` for visibility.

package seed_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/flowbyte-com/mpm-core/seed"
)

func TestApplySkills_FirstRunCreatesAll(t *testing.T) {
	dm := newTestDM(t)

	summary, err := seed.ApplySkills(dm)
	require.NoError(t, err)

	require.Equal(t, len(seed.SeedSkills), len(summary.Created),
		"first run should create every seeded skill, got Created=%v", summary.Created)
	require.Equal(t, 0, len(summary.Skipped), "first run has nothing to skip")
	require.Equal(t, 0, len(summary.Updated), "first run has nothing drifted")

	// Verify the rows are actually present in the DB at the deterministic
	// id that ReadSkill/ListSkills resolve against.
	for _, s := range seed.SeedSkills {
		savedID, err := s.SavedID()
		require.NoError(t, err)
		var got string
		require.NoError(t, dm.SQLDB().QueryRow(
			`SELECT id FROM memories WHERE id = ? AND deleted_at IS NULL`, savedID,
		).Scan(&got), "seeded skill %q should exist with id %q", s.Name, savedID)
	}
}

func TestApplySkills_IdempotentOnSecondRun(t *testing.T) {
	dm := newTestDM(t)

	first, err := seed.ApplySkills(dm)
	require.NoError(t, err)
	require.NotZero(t, len(first.Created))

	second, err := seed.ApplySkills(dm)
	require.NoError(t, err)

	require.Equal(t, 0, len(second.Created), "second run must create zero rows")
	require.Equal(t, len(first.Created), len(second.Skipped),
		"second run must skip exactly the rows it created on the first run")
	require.Equal(t, 0, len(second.Updated), "second run has nothing drifted")
}

func TestApplySkills_DriftDetected(t *testing.T) {
	dm := newTestDM(t)

	first, err := seed.ApplySkills(dm)
	require.NoError(t, err)
	require.NotZero(t, len(first.Created))

	// Mutate one seeded skill's content in place — operator's local edit.
	savedID, err := seed.SeedSkills[0].SavedID()
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(
		`UPDATE memories SET content = content || '

[locally edited]' WHERE id = ?`, savedID)
	require.NoError(t, err)

	second, err := seed.ApplySkills(dm)
	require.NoError(t, err)

	require.NotZero(t, len(second.Updated),
		"drift must be surfaced in Updated; got summary=%+v", second)
	require.Equal(t, 0, len(second.Created),
		"second run after drift should not create new rows")
}

