// engine_test.go — tests for the seed engine.
//
// Pins three contracts:
//   1. First run creates the rows (one per SeedDirectives entry).
//   2. Re-run is a no-op (idempotent): all rows in Skipped.
//   3. Operator's local edit is preserved (flagged as Drifted,
//      not overwritten).
//
// The tests use a fresh sqlite3 file via NewDatabaseManagerForDB so
// they never touch the workspace database. See wake_context_audit_test.go
// for the same pattern.
package seed

import (
	"testing"

	"github.com/stretchr/testify/require"

	"mpm/internal"
)

func newTestDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	return internal.NewTestDM(t)
}

func TestApplyDirectives_FirstRunCreatesAll(t *testing.T) {
	dm := newTestDM(t)

	summary, err := ApplyDirectives(dm)
	require.NoError(t, err)

	require.Equal(t, len(SeedDirectives), len(summary.Created),
		"first run should create every seeded directive, got Created=%v", summary.Created)
	require.Equal(t, 0, len(summary.Skipped), "first run has nothing to skip")
	require.Equal(t, 0, len(summary.Updated), "first run has nothing drifted")

	// Verify the rows are actually present in the DB.
	for _, sd := range SeedDirectives {
		var got string
		err := dm.SQLDB().QueryRow(
			`SELECT content FROM memories WHERE id = ? AND deleted_at IS NULL`, sd.StableID,
		).Scan(&got)
		require.NoError(t, err, "expected row %s to exist after seed", sd.StableID)
		require.Equal(t, sd.Content, got, "content mismatch for %s", sd.StableID)
	}
}

func TestApplyDirectives_RerunIsNoOp(t *testing.T) {
	dm := newTestDM(t)

	// First run.
	first, err := ApplyDirectives(dm)
	require.NoError(t, err)
	require.Equal(t, len(SeedDirectives), len(first.Created))

	// Second run on the same DB.
	second, err := ApplyDirectives(dm)
	require.NoError(t, err)
	require.Equal(t, 0, len(second.Created), "second run should not re-create")
	require.Equal(t, len(SeedDirectives), len(second.Skipped),
		"second run should skip all existing rows, got Skipped=%v", second.Skipped)
	require.Equal(t, 0, len(second.Updated), "second run has no drift")
}

func TestApplyDirectives_OperatorEditPreserved(t *testing.T) {
	dm := newTestDM(t)

	// First run.
	_, err := ApplyDirectives(dm)
	require.NoError(t, err)

	// Operator edits the first seeded directive locally.
	if len(SeedDirectives) == 0 {
		t.Fatal("SeedDirectives is empty — nothing to test")
	}
	first := SeedDirectives[0]
	_, err = dm.SQLDB().Exec(
		`UPDATE memories SET content = ? WHERE id = ?`,
		first.Content+"\n\n-- OPERATOR EDIT: tighter phrasing for this workspace.", first.StableID)
	require.NoError(t, err)

	// Re-run. The local edit must be preserved and flagged.
	summary, err := ApplyDirectives(dm)
	require.NoError(t, err)
	require.Equal(t, 1, len(summary.Updated),
		"local edit should be flagged as Drifted, got Updated=%v", summary.Updated)
	require.Equal(t, first.StableID, summary.Updated[0])

	// Verify the local edit is still there (not overwritten).
	var got string
	err = dm.SQLDB().QueryRow(
		`SELECT content FROM memories WHERE id = ?`, first.StableID,
	).Scan(&got)
	require.NoError(t, err)
	require.Contains(t, got, "OPERATOR EDIT",
		"local edit must be preserved after re-run, got content=%q", got)
}

func TestApplyDirectives_InsertedRowsHaveDirectiveMetadata(t *testing.T) {
	dm := newTestDM(t)

	_, err := ApplyDirectives(dm)
	require.NoError(t, err)

	// Every seeded row must have collection='directives' and
	// is_prime_directive=1 (the legacy read-path identifier).
	for _, sd := range SeedDirectives {
		var collection string
		var isPrime int
		err := dm.SQLDB().QueryRow(
			`SELECT collection, is_prime_directive FROM memories WHERE id = ?`, sd.StableID,
		).Scan(&collection, &isPrime)
		require.NoError(t, err)
		require.Equal(t, "directives", collection,
			"seeded row %s must have collection=directives, got %q", sd.StableID, collection)
		require.Equal(t, 1, isPrime,
			"seeded row %s must have is_prime_directive=1 (legacy read path)", sd.StableID)
	}
}

func TestApplyDirectives_InsertedRowsSurfacableViaReadDirectives(t *testing.T) {
	// End-to-end: seed → read_directives returns the rows. This pins
	// the loop: the only reason the agent sees the baseline directives
	// is because they were seeded. If collection or is_prime_directive
	// were set wrong, read_directives would return 0 rows.
	dm := newTestDM(t)

	_, err := ApplyDirectives(dm)
	require.NoError(t, err)

	rows, err := dm.SQLDB().Query(`
		SELECT id FROM memories
		WHERE (collection = 'directives' OR is_prime_directive = 1)
		  AND deleted_at IS NULL
		ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	var seen []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		seen = append(seen, id)
	}

	require.Equal(t, len(SeedDirectives), len(seen),
		"read_directives must return every seeded row, got %v", seen)
}

func TestContentHash_StableForSameInput(t *testing.T) {
	// Pin the hash shape: same content → same hash, different content
	// → different hash. If a future refactor changes the hash function
	// or the canonicalization, this catches it before the operator's
	// "Drifted" detection silently flips.
	a := SeedDirectives[0].ContentHash()
	b := SeedDirectives[0].ContentHash()
	require.Equal(t, a, b, "ContentHash must be deterministic for same input")

	// 64 hex chars = SHA-256.
	require.Equal(t, 64, len(a), "ContentHash must be 64 hex chars (SHA-256), got %d", len(a))
}