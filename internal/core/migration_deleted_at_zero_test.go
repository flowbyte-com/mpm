package internal

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigrateDeletedAtZeroToNull pins the legacy-sentinel normalization:
// rows with deleted_at = 0 (in any storage form — INTEGER 0, REAL 0.0,
// TEXT '0') become NULL so the standard `deleted_at IS NULL` read
// predicates surface them. Real soft-delete epochs and NULL rows are
// untouched.
func TestMigrateDeletedAtZeroToNull(t *testing.T) {
	db := setupMigrationTestDB(t)
	defer db.Close()

	if _, err := db.Exec(`
		INSERT INTO memories (id, content, deleted_at) VALUES
			('zero-int',   'legacy INTEGER 0',     0),
			('zero-real',  'legacy REAL 0.0',      0.0),
			('zero-text',  'legacy TEXT ''0''',    '0'),
			('epoch',      'real soft-delete',     1786610894),
			('null',       'already normalized',   NULL)`); err != nil {
		t.Fatalf("insert fixtures: %v", err)
	}

	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, MigrateDeletedAtZeroToNull(tx))
	require.NoError(t, tx.Commit())

	var nullCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`,
	).Scan(&nullCount))
	require.Equal(t, 4, nullCount, "all legacy zeros must normalize to NULL; the epoch row stays deleted")

	var zeroCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE deleted_at = 0`,
	).Scan(&zeroCount))
	require.Equal(t, 0, zeroCount, "no row may retain the legacy zero sentinel")

	// The migrated rows must satisfy the canonical read predicate.
	var visible int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND id IN ('zero-int','zero-real','zero-text')`,
	).Scan(&visible))
	require.Equal(t, 3, visible, "every previously-zero row must surface under the standard IS NULL read predicate")

	// The epoch row must stay hidden from live reads.
	var epochLive int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND id = 'epoch'`,
	).Scan(&epochLive))
	require.Equal(t, 0, epochLive, "a real soft-delete must never be resurrected")
}

// TestMigrateDeletedAtZeroToNull_Idempotent pins the sentinel guard: a
// second run is a no-op (no conversion, no re-record).
func TestMigrateDeletedAtZeroToNull_Idempotent(t *testing.T) {
	db := setupMigrationTestDB(t)
	defer db.Close()

	if _, err := db.Exec(`
		INSERT INTO memories (id, content, deleted_at) VALUES
			('zero-int', 'legacy INTEGER 0', 0),
			('null',     'already normalized', NULL)`); err != nil {
		t.Fatalf("insert fixtures: %v", err)
	}

	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, MigrateDeletedAtZeroToNull(tx))
	require.NoError(t, tx.Commit())

	// Second run on the same DB: sentinel present, no-op.
	tx2, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, MigrateDeletedAtZeroToNull(tx2))
	require.NoError(t, tx2.Commit())

	var nullCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`,
	).Scan(&nullCount))
	require.Equal(t, 2, nullCount, "idempotent: state unchanged after re-run")

	var sentinel int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = 'deleted_at_zero_normalized_v1'`,
	).Scan(&sentinel))
	require.Equal(t, 1, sentinel)
}