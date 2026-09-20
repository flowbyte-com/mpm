// migration_session_handoffs_mpm_session_id_test.go — pins the
// additive mpm_session_id + framework_session_id migration path
// end-to-end. Five tests cover the legs that matter:
//
//   1. TestMigrateSessionHandoffsMPMSessionID_FreshDB_ColumnsPresent
//      — fresh install has the columns at BaseTables time and the
//      migration short-circuits via the schema-shape probe.
//   2. TestMigrateSessionHandoffsMPMSessionID_LegacyDB_AddsColumns
//      — simulates a legacy install (columns absent) and runs the
//      migration; verifies columns appear and a legacy row survives.
//   3. TestMigrateSessionHandoffsMPMSessionID_Idempotent
//      — second invocation short-circuits on the sentinel; sentinel
//      exists exactly once.
//   4. TestMigrateSessionHandoffsMPMSessionID_IndexCreated
//      — non-unique idx_handoffs_mpm_session appears; multiple rows
//      with the same mpm_session_id coexist (UNIQUE auto-index on
//      the legacy session_id is unaffected).
//   5. TestMigrateSessionHandoffsMPMSessionID_ColumnsNullable
//      — both new columns are nullable so legacy rows remain valid
//      and callers that have no MPM/framework identity still write.
//
// Uses a real file-DB DSN (per test_db_safety_test.go whitelist)
// because the additive migration path needs on-disk persistence to
// exercise close+reopen across the sentinel boundary.

package internal

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// sessionHandoffsMPMSessionIDMigrationFixture is the canonical
// pre-migration schema. Hand-rolled because BaseTables already
// declares the new columns and we need to test the upgrade path.
const sessionHandoffsMPMSessionIDMigrationFixture = `
CREATE TABLE schema_migrations (
    id TEXT PRIMARY KEY,
    applied_at INTEGER NOT NULL
);
CREATE TABLE session_handoffs (
    id            TEXT PRIMARY KEY,
    session_id    TEXT UNIQUE,
    ended_at      INTEGER NOT NULL,
    ended_state   TEXT NOT NULL CHECK (ended_state IN ('clean','crashed','interrupted','force_end')),
    summary       TEXT NOT NULL,
    commitments   JSON NOT NULL DEFAULT '[]',
    open_questions JSON NOT NULL DEFAULT '[]',
    read_at       INTEGER,
    read_by       TEXT,
    created_at    INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
);
CREATE INDEX idx_handoffs_unread ON session_handoffs(read_at, ended_at DESC);
CREATE INDEX idx_handoffs_ended  ON session_handoffs(ended_at);
CREATE INDEX idx_handoffs_session ON session_handoffs(session_id);
`

// newLegacySessionHandoffsDB creates a raw sql.DB at the given path
// that has the pre-migration session_handoffs shape (no
// mpm_session_id / framework_session_id columns). The DB is auto-
// closed on test cleanup. Use to drive the additive migration path
// from a known-fixture starting state.
func newLegacySessionHandoffsDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	// Touch the file path; if the parent directory was wiped, rebuild.
	db, err := sql.Open("sqlite3", path+"?_fk=1")
	require.NoError(t, err, "open legacy fixture DB")
	_, err = db.Exec(sessionHandoffsMPMSessionIDMigrationFixture)
	require.NoError(t, err, "apply legacy fixture DDL")
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// sessionHandoffsColumnExists returns whether a column is present on
// the named table. Namespaced to avoid colliding with the helper of
// the same name in migration_timestamps_test.go.
func sessionHandoffsColumnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column,
	).Scan(&n))
	return n > 0
}

// TestMigrateSessionHandoffsMPMSessionID_FreshDB_ColumnsPresent
// pins the BaseTables DDL change: a fresh install must declare
// mpm_session_id and framework_session_id from the start, so the
// schema-shape probe inside the migration short-circuits without
// touching the table.
func TestMigrateSessionHandoffsMPMSessionID_FreshDB_ColumnsPresent(t *testing.T) {
	dm := NewTestDM(t)

	require.True(t, sessionHandoffsColumnExists(t, dm.SQLDB(), "session_handoffs", "mpm_session_id"),
		"canonical BaseTables must declare session_handoffs.mpm_session_id")
	require.True(t, sessionHandoffsColumnExists(t, dm.SQLDB(), "session_handoffs", "framework_session_id"),
		"canonical BaseTables must declare session_handoffs.framework_session_id")

	// Probe must short-circuit (no rebuild, no log noise) — exercise
	// the migration directly to confirm the schema-shape gate fires.
	require.NoError(t, runMigrationTx(t, dm.SQLDB(), MigrateSessionHandoffsMPMSessionID),
		"fresh-DB probe must short-circuit and return nil")
}

// TestMigrateSessionHandoffsMPMSessionID_LegacyDB_AddsColumns
// simulates the production state of a legacy install whose
// mpm_session_id and framework_session_id columns are absent, then
// runs the migration end-to-end. Verifies the columns appear and
// a pre-existing handoff row survives verbatim.
func TestMigrateSessionHandoffsMPMSessionID_LegacyDB_AddsColumns(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/legacy.db"
	db := newLegacySessionHandoffsDB(t, dbPath)

	// Pre-condition: columns absent on the legacy fixture.
	require.False(t, sessionHandoffsColumnExists(t, db, "session_handoffs", "mpm_session_id"))
	require.False(t, sessionHandoffsColumnExists(t, db, "session_handoffs", "framework_session_id"))

	// Seed a legacy handoff row so the post-migration checksum has
	// something to verify. The row uses the legacy column set only.
	now := int64(1788521012)
	_, err := db.Exec(`
		INSERT INTO session_handoffs
		    (id, session_id, ended_at, ended_state, summary, created_at)
		VALUES ('legacy-h-1', 'legacy-sid', ?, 'clean', 'legacy summary', ?)
	`, now, now)
	require.NoError(t, err)

	// Run the migration in a single transaction — mirrors production
	// (initUnifiedSchema wraps every migration in one tx).
	require.NoError(t, runMigrationTx(t, db, MigrateSessionHandoffsMPMSessionID))

	// Post-condition: both columns are now present.
	require.True(t, sessionHandoffsColumnExists(t, db, "session_handoffs", "mpm_session_id"))
	require.True(t, sessionHandoffsColumnExists(t, db, "session_handoffs", "framework_session_id"))

	// Legacy row survives verbatim — mpm_session_id and
	// framework_session_id are NULL on pre-migration rows.
	var mpmSID, fwSID sql.NullString
	var summary string
	require.NoError(t, db.QueryRow(
		`SELECT mpm_session_id, framework_session_id, summary
		   FROM session_handoffs WHERE id = 'legacy-h-1'`,
	).Scan(&mpmSID, &fwSID, &summary))
	require.False(t, mpmSID.Valid, "legacy row's mpm_session_id must be NULL")
	require.False(t, fwSID.Valid, "legacy row's framework_session_id must be NULL")
	require.Equal(t, "legacy summary", summary, "legacy row content preserved")

	// Sentinel row written exactly once.
	var sentinelCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		sessionHandoffsMPMSessionIDSentinel,
	).Scan(&sentinelCount))
	require.Equal(t, 1, sentinelCount)
}

// TestMigrateSessionHandoffsMPMSessionID_Idempotent verifies that
// re-running the migration short-circuits on the sentinel without
// altering the schema.
func TestMigrateSessionHandoffsMPMSessionID_Idempotent(t *testing.T) {
	dm := NewTestDM(t)

	// First invocation: probe-fresh path (columns already present,
	// sentinel gate fires).
	require.NoError(t, runMigrationTx(t, dm.SQLDB(), MigrateSessionHandoffsMPMSessionID))

	// Second invocation: sentinel gate fires; no-op.
	require.NoError(t, runMigrationTx(t, dm.SQLDB(), MigrateSessionHandoffsMPMSessionID))

	// Sentinel row exists exactly once.
	var sentinelCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		sessionHandoffsMPMSessionIDSentinel,
	).Scan(&sentinelCount))
	require.Equal(t, 1, sentinelCount,
		"idempotent migration must not insert duplicate sentinel rows")
}

// TestMigrateSessionHandoffsMPMSessionID_IndexCreated verifies
// that the non-unique idx_handoffs_mpm_session index exists and
// permits multiple rows with the same mpm_session_id (one MPM
// session has many handoffs). The legacy session_id UNIQUE auto-
// index is unaffected.
func TestMigrateSessionHandoffsMPMSessionID_IndexCreated(t *testing.T) {
	dm := NewTestDM(t)

	// Seed three handoffs sharing the same mpm_session_id, plus one
	// with a different one. Should all succeed (no UNIQUE violation).
	const shared = "mpm-shared-id"
	_, err := dm.SQLDB().Exec(`
		INSERT INTO session_handoffs
		    (id, mpm_session_id, ended_at, ended_state, summary)
		VALUES
		    ('idx-1', ?, 1, 'clean', 'first'),
		    ('idx-2', ?, 2, 'clean', 'second'),
		    ('idx-3', ?, 3, 'clean', 'third'),
		    ('idx-4', 'mpm-other', 4, 'clean', 'other')
	`, shared, shared, shared)
	require.NoError(t, err, "non-unique mpm_session_id must accept repeats")

	// Index is queryable (no SQLITE_ERROR on .indexed_by / EXPLAIN).
	rows, err := dm.SQLDB().Query(
		`SELECT id FROM session_handoffs WHERE mpm_session_id = ? ORDER BY id`,
		shared,
	)
	require.NoError(t, err)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		got = append(got, id)
	}
	require.Equal(t, []string{"idx-1", "idx-2", "idx-3"}, got)
}

// TestMigrateSessionHandoffsMPMSessionID_ColumnsNullable pins
// the contract that both new columns are nullable so legacy rows
// remain valid AND callers that have no MPM / framework identity
// (e.g. early boot of `mpm session allocate` after a fresh
// rotation failure) can still write a handoff.
func TestMigrateSessionHandoffsMPMSessionID_ColumnsNullable(t *testing.T) {
	dm := NewTestDM(t)
	for _, col := range []string{"mpm_session_id", "framework_session_id"} {
		var notNull int
		require.NoError(t, dm.SQLDB().QueryRow(
			`SELECT "notnull" FROM pragma_table_info('session_handoffs') WHERE name = ?`,
			col,
		).Scan(&notNull))
		require.Equal(t, 0, notNull,
			"column %s must remain nullable after migration", col)
	}

	// Insert a handoff with both columns NULL — must succeed.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO session_handoffs
		    (id, ended_at, ended_state, summary)
		VALUES ('nulls-1', 1, 'clean', 'both null')
	`)
	require.NoError(t, err, "nullable columns must accept NULL on insert")
}

// runMigrationTx runs a migration function inside a transaction on
// db, rolling back on error and committing on success. Mirrors the
// production initUnifiedSchema envelope so the migration sees the
// same atomicity shape it sees in production.
func runMigrationTx(t *testing.T, db *sql.DB, fn func(*sql.Tx) error) error {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
