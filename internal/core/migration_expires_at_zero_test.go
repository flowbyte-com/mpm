package internal

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigrateExpiresAtZeroToNull pins the legacy-sentinel normalization:
// rows with expires_at = 0 (in any storage form — INTEGER 0, REAL 0.0,
// TEXT '0') become NULL so the standard `expires_at IS NULL OR > now`
// TTL read predicates surface them, and PruneExpired can no longer treat
// them as expired. Real TTL epochs and NULL rows are untouched.
func TestMigrateExpiresAtZeroToNull(t *testing.T) {
	db := setupMigrationTestDB(t)
	defer db.Close()

	if _, err := db.Exec(`
		INSERT INTO memories (id, content, expires_at) VALUES
			('zero-int',   'legacy INTEGER 0',     0),
			('zero-real',  'legacy REAL 0.0',      0.0),
			('zero-text',  'legacy TEXT ''0''',    '0'),
			('epoch',      'real past TTL',        1000),
			('future',     'real future TTL',      4102444800),
			('null',       'no TTL',               NULL)`); err != nil {
		t.Fatalf("insert fixtures: %v", err)
	}

	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, MigrateExpiresAtZeroToNull(tx))
	require.NoError(t, tx.Commit())

	var nullCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE expires_at IS NULL`,
	).Scan(&nullCount))
	require.Equal(t, 4, nullCount, "all legacy zeros must normalize to NULL; real epochs stay set")

	var zeroCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE expires_at = 0`,
	).Scan(&zeroCount))
	require.Equal(t, 0, zeroCount, "no row may retain the legacy zero sentinel")

	// The migrated rows must satisfy the canonical TTL read predicate.
	var visible int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE (expires_at IS NULL OR expires_at > strftime('%s','now')) AND id IN ('zero-int','zero-real','zero-text')`,
	).Scan(&visible))
	require.Equal(t, 3, visible, "every previously-zero row must surface under the standard TTL read predicate")

	// A real past TTL must remain prune-able.
	var pruneable int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE expires_at IS NOT NULL AND expires_at < strftime('%s','now')`,
	).Scan(&pruneable))
	require.Equal(t, 1, pruneable, "a real past TTL stays expired and prune-able")
}

// TestMigrateExpiresAtZeroToNull_Idempotent pins the sentinel guard: a
// second run is a no-op (no conversion, no re-record).
func TestMigrateExpiresAtZeroToNull_Idempotent(t *testing.T) {
	db := setupMigrationTestDB(t)
	defer db.Close()

	if _, err := db.Exec(`
		INSERT INTO memories (id, content, expires_at) VALUES
			('zero-int', 'legacy INTEGER 0', 0),
			('null',     'no TTL',           NULL)`); err != nil {
		t.Fatalf("insert fixtures: %v", err)
	}

	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, MigrateExpiresAtZeroToNull(tx))
	require.NoError(t, tx.Commit())

	// Second run on the same DB: sentinel present, no-op.
	tx2, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, MigrateExpiresAtZeroToNull(tx2))
	require.NoError(t, tx2.Commit())

	var nullCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE expires_at IS NULL`,
	).Scan(&nullCount))
	require.Equal(t, 2, nullCount, "idempotent: state unchanged after re-run")

	var sentinel int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = 'expires_at_zero_normalized_v1'`,
	).Scan(&sentinel))
	require.Equal(t, 1, sentinel)
}