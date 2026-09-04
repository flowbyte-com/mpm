// migration_memories_created_at_not_null_test.go — pins the
// schema-level enforcement of the "created_at is mandatory"
// invariant. Four tests, all exercising the same migration but
// from different angles so a regression in any single leg of the
// flow surfaces with a precise error.
//
//   1. TestEnforceMemoriesCreatedAtNotNull_FreshDBIsAlreadyNotNull
//      — proves the canonical BaseTables DDL now declares the
//      column NOT NULL, so a fresh install never has to run the
//      rebuild at all (and the migration short-circuits via the
//      schema-shape probe).
//   2. TestEnforceMemoriesCreatedAtNotNull_LegacyDBRebuildsNotNull
//      — simulates a legacy install whose created_at is nullable,
//      runs the migration, verifies the schema is rebuilt with
//      NOT NULL and all rows preserved.
//   3. TestEnforceMemoriesCreatedAtNotNull_RefusesIfNullRowsExist
//      — defense-in-depth: the migration refuses to start a
//      rebuild if any row still has NULL created_at, surfacing a
//      precise error message instead of a generic NOT NULL
//      violation mid-rebuild.
//   4. TestEnforceMemoriesCreatedAtNotNull_Idempotent
//      — second invocation short-circuits on the sentinel without
//      touching the schema, and the sentinel row exists exactly
//      once.
//
// Sister tests: TestApplyDirectives_FreshSeedPopulatesCreatedAt
// (seed/engine_test.go) and TestMigrateMemoriesCreatedAtBackfill_RepairsNullRows
// (directive_tools_test.go) pin the writer-path and legacy-row
// repair contracts. This file pins the schema-level contract that
// makes those two durable.
package internal

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// TestEnforceMemoriesCreatedAtNotNull_FreshDBIsAlreadyNotNull
// pins the BaseTables DDL change: a fresh install must declare
// `created_at` with NOT NULL from the start, so the schema-shape
// probe inside EnforceMemoriesCreatedAtNotNull short-circuits
// without touching the table.
func TestEnforceMemoriesCreatedAtNotNull_FreshDBIsAlreadyNotNull(t *testing.T) {
	dm := NewTestDM(t)

	var notNull int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT "notnull" FROM pragma_table_info('memories') WHERE name='created_at'`,
	).Scan(&notNull))
	require.Equal(t, 1, notNull,
		"canonical BaseTables must declare memories.created_at as NOT NULL")

	// Probe must short-circuit (no rebuild, no log noise).
	require.NoError(t, EnforceMemoriesCreatedAtNotNull(dm.SQLDB()),
		"fresh-DB probe must short-circuit and return nil")

	// Schema unchanged.
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT "notnull" FROM pragma_table_info('memories') WHERE name='created_at'`,
	).Scan(&notNull))
	require.Equal(t, 1, notNull)
}

// TestEnforceMemoriesCreatedAtNotNull_LegacyDBRebuildsNotNull
// simulates the production state of a legacy install whose
// `created_at` is nullable, then runs the migration end-to-end.
// Uses a raw sql.DB so we control the table shape directly —
// NewTestDM would have already applied the BaseTables fix and
// short-circuited the migration, which is the wrong fixture for
// this test.
func TestEnforceMemoriesCreatedAtNotNull_LegacyDBRebuildsNotNull(t *testing.T) {
	db := OpenLegacyMemoriesDB(t)
	var notNullBefore int
	require.NoError(t, db.QueryRow(
		`SELECT "notnull" FROM pragma_table_info('memories') WHERE name='created_at'`,
	).Scan(&notNullBefore))
	require.Equal(t, 0, notNullBefore,
		"legacy fixture must start with created_at declared NULLable")

	// Seed a row so the post-rebuild checksum has something to verify.
	now := int64(1788521012)
	_, err := db.Exec(`
		INSERT INTO memories
		    (id, collection, content, created_at, updated_at)
		VALUES ('legacy-row-1', 'directives', 'legacy directive', ?, ?)
	`, now, now)
	require.NoError(t, err)

	// Run the migration.
	require.NoError(t, EnforceMemoriesCreatedAtNotNull(db))

	// Post-condition: column is now NOT NULL.
	var notNullAfter int
	require.NoError(t, db.QueryRow(
		`SELECT "notnull" FROM pragma_table_info('memories') WHERE name='created_at'`,
	).Scan(&notNullAfter))
	require.Equal(t, 1, notNullAfter,
		"rebuild must flip memories.created_at to NOT NULL")

	// Row preserved with its created_at intact.
	var got int64
	require.NoError(t, db.QueryRow(
		`SELECT created_at FROM memories WHERE id='legacy-row-1'`,
	).Scan(&got))
	require.Equal(t, now, got, "row's created_at must survive the rebuild byte-for-byte")

	// Indexes preserved — the rebuild re-creates the canonical
	// idx_memories_collection_deleted_created index on the
	// renamed table. PRAGMA-equivalent: count rows in the index
	// backing metadata, or simply probe the schema_migrations
	// sentinel as a coarse smoke test that the rebuild ran.
	var sentinelCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id='created_at_not_null_v1'`,
	).Scan(&sentinelCount))
	require.Equal(t, 1, sentinelCount,
		"sentinel must be recorded exactly once after rebuild")

	// Reader invariant: read_directives succeeds against the
	// rebuilt table.
	var directiveCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection='directives' AND deleted_at IS NULL`,
	).Scan(&directiveCount))
	require.Equal(t, 1, directiveCount,
		"the legacy directive row must surface through the read path after rebuild")
}

// TestEnforceMemoriesCreatedAtNotNull_RefusesIfNullRowsExist
// pins the defense-in-depth precondition check. A rebuild that
// proceeded with NULL rows in the table would fail loudly mid-way
// through the INSERT...SELECT with a generic NOT NULL violation
// — the operator gets a confusing error message and has to figure
// out that they need to run the backfill migration first. The
// precondition check surfaces a precise error instead.
func TestEnforceMemoriesCreatedAtNotNull_RefusesIfNullRowsExist(t *testing.T) {
	db := OpenLegacyMemoriesDB(t)
	// defect shape. Simulates an install that ran the legacy code
	// path without ever applying the 2026-09-04 backfill migration.
	_, err := db.Exec(`
		INSERT INTO memories
		    (id, collection, content, created_at, updated_at)
		VALUES
		    ('null-row-1', 'directives', 'null created_at row', NULL, NULL),
		    ('good-row-1', 'directives', 'good created_at row', 1788521012, 1788521012)
	`)
	require.NoError(t, err)

	// Migration must refuse with a precise error message that
	// points the operator at the backfill migration.
	err = EnforceMemoriesCreatedAtNotNull(db)
	require.Error(t, err,
		"migration must refuse to run a NOT NULL rebuild over a table that still has NULL rows")
	require.Contains(t, err.Error(), "MigrateMemoriesCreatedAtBackfill",
		"error message must point the operator at the canonical repair migration")

	// Schema must NOT have been touched (tx was rolled back).
	var notNullAfter int
	require.NoError(t, db.QueryRow(
		`SELECT "notnull" FROM pragma_table_info('memories') WHERE name='created_at'`,
	).Scan(&notNullAfter))
	require.Equal(t, 0, notNullAfter,
		"refused migration must leave the schema unchanged (tx rolled back)")
}

// TestEnforceMemoriesCreatedAtNotNull_Idempotent pins the
// sentinel-guarded no-op behaviour. A second invocation must
// short-circuit on the schema_migrations row without touching the
// table, and the sentinel row must exist exactly once.
func TestEnforceMemoriesCreatedAtNotNull_Idempotent(t *testing.T) {
	db := OpenLegacyMemoriesDB(t)
	_, err := db.Exec(`
		INSERT INTO memories
		    (id, collection, content, created_at, updated_at)
		VALUES ('idempotent-row-1', 'directives', 'row', 1788521012, 1788521012)
	`)
	require.NoError(t, err)
	require.NoError(t, EnforceMemoriesCreatedAtNotNull(db))

	// Capture pre-second-call row count so we can verify the
	// second call didn't touch the table.
	var rowCountBefore int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&rowCountBefore))

	// Second invocation — must no-op.
	require.NoError(t, EnforceMemoriesCreatedAtNotNull(db))

	// Schema and rows unchanged.
	var rowCountAfter int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&rowCountAfter))
	require.Equal(t, rowCountBefore, rowCountAfter,
		"idempotent re-run must not touch the row set")

	var sentinelCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id='created_at_not_null_v1'`,
	).Scan(&sentinelCount))
	require.Equal(t, 1, sentinelCount,
		"sentinel row must exist exactly once after first run + idempotent re-run")
}

// All four tests use OpenLegacyMemoriesDB (testhelpers.go) for the
// rebuild/refusal/idempotency tests, and NewTestDM for the
// fresh-DB-already-NotNull probe. The helper is package-shared so
// sister tests in directive_tools_test.go can use the same fixture
// when they need to simulate the pre-fix schema.