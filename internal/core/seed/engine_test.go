// engine_test.go — tests for the seed engine.
//
// Pins three contracts:
//   1. First run creates the rows (one per seed.SeedDirectives entry).
//   2. Re-run is a no-op (idempotent): all rows in Skipped.
//   3. Operator's local edit is preserved (flagged as Drifted,
//      not overwritten).
//
// The tests use a fresh sqlite3 file via NewDatabaseManagerForDB so
// they never touch the workspace database. See wake_context_audit_test.go
// for the same pattern.
package seed_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/seed"
)

func newTestDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	return internal.NewTestDM(t)
}

func TestApplyDirectives_FirstRunCreatesAll(t *testing.T) {
	dm := newTestDM(t)

	summary, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)

	require.Equal(t, len(seed.SeedDirectives), len(summary.Created),
		"first run should create every seeded directive, got Created=%v", summary.Created)
	require.Equal(t, 0, len(summary.Skipped), "first run has nothing to skip")
	require.Equal(t, 0, len(summary.Updated), "first run has nothing drifted")

	// Verify the rows are actually present in the DB.
	for _, sd := range seed.SeedDirectives {
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
	first, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	require.Equal(t, len(seed.SeedDirectives), len(first.Created))

	// Second run on the same DB.
	second, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	require.Equal(t, 0, len(second.Created), "second run should not re-create")
	require.Equal(t, len(seed.SeedDirectives), len(second.Skipped),
		"second run should skip all existing rows, got Skipped=%v", second.Skipped)
	require.Equal(t, 0, len(second.Updated), "second run has no drift")
}

// TestApplyDirectives_RevivesSoftDeleted pins the restore contract:
// a soft-deleted baseline row still occupies the stable-id PRIMARY KEY,
// so INSERT OR IGNORE would silently no-op while the summary reports
// "Created" — a shredded baseline could never come back. Re-init must
// UNDELETE the row instead, making `mpm ops init directives` the
// documented recovery path after an accidental shred, and keeping the
// "standalone runtime is never directive-blind" guarantee.
func TestApplyDirectives_RevivesSoftDeleted(t *testing.T) {
	dm := newTestDM(t)

	// Seed, then soft-delete every baseline row (as a decay sweep or an
	// accidental shred would).
	first, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	require.Equal(t, len(seed.SeedDirectives), len(first.Created))
	_, err = dm.SQLDB().Exec(
		`UPDATE memories SET deleted_at = 1786610894 WHERE id IN (SELECT id FROM memories WHERE collection = 'directives')`,
	)
	require.NoError(t, err)

	// Re-init must revive every shredded baseline, not skip them.
	second, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	require.Equal(t, len(seed.SeedDirectives), len(second.Created),
		"re-init must revive every soft-deleted baseline, got Created=%v", second.Created)
	require.Equal(t, 0, len(second.Skipped), "no row may be skipped while its baseline is dead")

	var live int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'directives' AND deleted_at IS NULL`,
	).Scan(&live))
	require.Equal(t, len(seed.SeedDirectives), live, "all baseline directives must be live after revive")

	// Idempotent afterwards.
	third, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	require.Equal(t, 0, len(third.Created))
	require.Equal(t, len(seed.SeedDirectives), len(third.Skipped))
}

func TestApplyDirectives_OperatorEditPreserved(t *testing.T) {
	dm := newTestDM(t)

	// First run.
	_, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)

	// Operator edits the first seeded directive locally.
	if len(seed.SeedDirectives) == 0 {
		t.Fatal("seed.SeedDirectives is empty — nothing to test")
	}
	first := seed.SeedDirectives[0]
	_, err = dm.SQLDB().Exec(
		`UPDATE memories SET content = ? WHERE id = ?`,
		first.Content+"\n\n-- OPERATOR EDIT: tighter phrasing for this workspace.", first.StableID)
	require.NoError(t, err)

	// Re-run. The local edit must be preserved and flagged.
	summary, err := seed.ApplyDirectives(dm)
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

	_, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)

	// Every seeded row must have collection='directives' and
	// is_prime_directive=1 (the legacy read-path identifier).
	for _, sd := range seed.SeedDirectives {
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

	_, err := seed.ApplyDirectives(dm)
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

	require.Equal(t, len(seed.SeedDirectives), len(seen),
		"read_directives must return every seeded row, got %v", seen)
}

func TestContentHash_StableForSameInput(t *testing.T) {
	// Pin the hash shape: same content → same hash, different content
	// → different hash. If a future refactor changes the hash function
	// or the canonicalization, this catches it before the operator's
	// "Drifted" detection silently flips.
	a := seed.SeedDirectives[0].ContentHash()
	b := seed.SeedDirectives[0].ContentHash()
	require.Equal(t, a, b, "ContentHash must be deterministic for same input")

	// 64 hex chars = SHA-256.
	require.Equal(t, 64, len(a), "ContentHash must be 64 hex chars (SHA-256), got %d", len(a))
}

// TestSeedDirective_AllBaselinesAreGlobal pins the default scope on the
// baseline cognitive bootstrap. Every entry shipped in seed.SeedDirectives
// must be reachable by every agent framework — no framework-scoped
// directive in the baseline. If a future contributor adds a baseline
// directive without Scope="global", this catches it before alpha ships.
func TestSeedDirective_AllBaselinesAreGlobal(t *testing.T) {
	for _, sd := range seed.SeedDirectives {
		scope := sd.Scope
		if scope == "" {
			scope = "global"
		}
		require.Equal(t, "global", scope,
			"baseline directive %s must have Scope=\"global\" or unset (which defaults to global); got %q",
			sd.StableID, sd.Scope)
	}
}

// TestApplyDirectives_ScopeMaterialisedInMetadata pins that the
// seed engine writes metadata.scope correctly. ReadDirectivesForFramework
// relies on this JSON field for its WHERE clause; if insertSeedRow stops
// materialising scope, framework-scoped directives silently break.
func TestApplyDirectives_ScopeMaterialisedInMetadata(t *testing.T) {
	dm := newTestDM(t)

	_, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)

	rows, err := dm.SQLDB().Query(`
		SELECT id, json_extract(metadata, '$.scope') FROM memories
		WHERE (collection = 'directives' OR is_prime_directive = 1)
		  AND deleted_at IS NULL
		ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	for rows.Next() {
		var id string
		var scope *string
		require.NoError(t, rows.Scan(&id, &scope))
		require.NotNil(t, scope, "seeded directive %s must have metadata.scope set", id)
		require.Equal(t, "global", *scope,
			"seeded baseline directive %s must materialise scope=global in metadata, got %q",
			id, *scope)
	}
}

// TestApplyDirectives_FreshSeedPopulatesCreatedAt pins the writer
// contract: every seeded directive row must have a non-NULL
// created_at. The pre-2026-09-04 seed INSERT omitted created_at
// from its column list, and on databases whose memories.created_at
// column lacks the canonical DEFAULT clause (legacy installs that
// went through the column-affinity rebuild without inheriting the
// DEFAULT), the result was NULL — which crashed read_directives
// with `converting NULL to string is unsupported`.
//
// Regression target: insertSeedRow must provide created_at
// explicitly so the column invariant holds regardless of the
// underlying schema's DEFAULT clause. The corresponding legacy-row
// repair is covered by
// TestMigrateMemoriesCreatedAtBackfill_RepairsNullRows in the
// internal/core package.
func TestApplyDirectives_FreshSeedPopulatesCreatedAt(t *testing.T) {
	dm := newTestDM(t)

	_, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)

	for _, sd := range seed.SeedDirectives {
		var ca sql.NullInt64
		err := dm.SQLDB().QueryRow(
			`SELECT created_at FROM memories WHERE id = ? AND deleted_at IS NULL`, sd.StableID,
		).Scan(&ca)
		require.NoError(t, err, "expected row %s to exist after seed", sd.StableID)
		require.True(t, ca.Valid,
			"seeded directive %s must have a non-NULL created_at (regression: pre-fix seed path inserted NULL on databases without the canonical DEFAULT clause)",
			sd.StableID)
		require.Greater(t, ca.Int64, int64(0),
			"seeded directive %s created_at must be a positive Unix epoch, got %d",
			sd.StableID, ca.Int64)
	}
}