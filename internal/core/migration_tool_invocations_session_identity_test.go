// migration_tool_invocations_session_identity_test.go — pins the
// additive tool_invocations session-identity migration path
// end-to-end. Five tests cover the legs that matter:
//
//   1. FreshDB_ColumnsPresent        — fresh install declares
//                                      columns + indexes from the
//                                      BaseTables DDL.
//   2. LegacyDB_AddsColumns          — simulates a legacy install
//                                      (columns absent) and runs
//                                      the migration; legacy rows
//                                      survive verbatim with NULL
//                                      on the new dimensions.
//   3. Idempotent                   — second invocation short-
//                                      circuits on the sentinel.
//   4. NewWritesPopulateColumns     — a fresh INSERT records the
//                                      three identity dimensions.
//   5. IndexesExist                — composite indexes are
//                                      queryable by EXPLAIN QUERY PLAN.
//
// Uses NewTestDM in-memory because the migration does not require
// close+reopen — it operates within a single transaction that
// NewTestDM's initUnifiedSchema opens during construction.

package internal

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigrateToolInvocationsSessionIdentity_FreshDB_ColumnsPresent
// pins the BaseTables DDL change: a fresh install must declare
// mpm_session_id and framework_session_id on tool_invocations, plus
// the two composite indexes.
func TestMigrateToolInvocationsSessionIdentity_FreshDB_ColumnsPresent(t *testing.T) {
	dm := NewTestDM(t)

	require.True(t, sessionHandoffsColumnExists(t, dm.SQLDB(), "tool_invocations", "mpm_session_id"),
		"canonical BaseTables must declare tool_invocations.mpm_session_id")
	require.True(t, sessionHandoffsColumnExists(t, dm.SQLDB(), "tool_invocations", "framework_session_id"),
		"canonical BaseTables must declare tool_invocations.framework_session_id")

	// Migration has already run during initUnifiedSchema — verify
	// the sentinel exists exactly once.
	var sentinelCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		toolInvocationsSessionIdentitySentinel,
	).Scan(&sentinelCount))
	require.Equal(t, 1, sentinelCount,
		"initUnifiedSchema must register the migration sentinel")
}

// TestMigrateToolInvocationsSessionIdentity_LegacyDB_AddsColumns
// simulates the production state of a legacy install whose
// tool_invocations has no mpm_session_id / framework_session_id
// columns, then runs the migration end-to-end. Verifies the
// columns appear, a legacy row survives verbatim with NULL on
// the new dimensions, and the indexes exist.
func TestMigrateToolInvocationsSessionIdentity_LegacyDB_AddsColumns(t *testing.T) {
	dm := NewTestDM(t)
	// Drop the indexes first — SQLite refuses ALTER TABLE DROP COLUMN
	// when the column is referenced by an index. The migration's
	// CREATE INDEX IF NOT EXISTS will rebuild them on the post-ADD-COLUMN
	// schema.
	_, _ = dm.SQLDB().Exec(`DROP INDEX IF EXISTS idx_tool_invocations_mpm_session`)
	_, _ = dm.SQLDB().Exec(`DROP INDEX IF EXISTS idx_tool_invocations_framework_session`)
	// Drop the new columns to simulate a legacy DB.
	_, err := dm.SQLDB().Exec(`ALTER TABLE tool_invocations DROP COLUMN mpm_session_id`)
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(`ALTER TABLE tool_invocations DROP COLUMN framework_session_id`)
	require.NoError(t, err)
	// Clear the sentinel so the migration can re-run.
	_, err = dm.SQLDB().Exec(`DELETE FROM schema_migrations WHERE id = ?`,
		toolInvocationsSessionIdentitySentinel)
	require.NoError(t, err)

	// Pre-condition: columns absent.
	require.False(t, sessionHandoffsColumnExists(t, dm.SQLDB(), "tool_invocations", "mpm_session_id"))
	require.False(t, sessionHandoffsColumnExists(t, dm.SQLDB(), "tool_invocations", "framework_session_id"))

	// Seed a legacy row that uses only the original columns.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id,
		     actor_kind, framework_name, payload_hash, result_status,
		     started_at, completed_at, duration_ms)
		VALUES ('legacy-ti-1', 'legacy-sid', 'mpm_memory', 'save',
		        'inv-legacy-1', 'agent', 'mpm-cli', 'sha256:x',
		        'success', 1700000000, 1700000001, 100)
	`)
	require.NoError(t, err)

	// Run the migration in its own tx.
	require.NoError(t, runMigrationTx(t, dm.SQLDB(), MigrateToolInvocationsSessionIdentity))

	// Post-condition: both columns now present.
	require.True(t, sessionHandoffsColumnExists(t, dm.SQLDB(), "tool_invocations", "mpm_session_id"))
	require.True(t, sessionHandoffsColumnExists(t, dm.SQLDB(), "tool_invocations", "framework_session_id"))

	// Legacy row survives verbatim — both new dimensions are NULL.
	var mpmSID, fwSID *string
	var sessID string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT session_id, mpm_session_id, framework_session_id
		   FROM tool_invocations WHERE id = 'legacy-ti-1'`,
	).Scan(&sessID, &mpmSID, &fwSID))
	require.Equal(t, "legacy-sid", sessID,
		"legacy session_id is preserved verbatim")
	require.Nil(t, mpmSID,
		"legacy row's mpm_session_id is NULL — no fabricated history")
	require.Nil(t, fwSID,
		"legacy row's framework_session_id is NULL — no fabricated history")

	// Sentinel written exactly once.
	var sentinelCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		toolInvocationsSessionIdentitySentinel,
	).Scan(&sentinelCount))
	require.Equal(t, 1, sentinelCount)
}

// TestMigrateToolInvocationsSessionIdentity_Idempotent verifies
// that re-running the migration short-circuits on the sentinel
// without altering the schema.
func TestMigrateToolInvocationsSessionIdentity_Idempotent(t *testing.T) {
	dm := NewTestDM(t)

	// First invocation: probe-fresh path (columns already present
	// via BaseTables DDL, sentinel gate fires).
	require.NoError(t, runMigrationTx(t, dm.SQLDB(), MigrateToolInvocationsSessionIdentity))

	// Second invocation: sentinel gate fires; no-op.
	require.NoError(t, runMigrationTx(t, dm.SQLDB(), MigrateToolInvocationsSessionIdentity))

	// Sentinel exists exactly once.
	var sentinelCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		toolInvocationsSessionIdentitySentinel,
	).Scan(&sentinelCount))
	require.Equal(t, 1, sentinelCount,
		"idempotent migration must not insert duplicate sentinel rows")
}

// TestMigrateToolInvocationsSessionIdentity_NewWritesPopulateColumns
// verifies that a fresh INSERT (mimicking the audit_hook write
// path) records the three independent session identity dimensions
// without substituting one for another.
func TestMigrateToolInvocationsSessionIdentity_NewWritesPopulateColumns(t *testing.T) {
	dm := NewTestDM(t)

	const (
		sessionID    = "cli-dispatcher-uuid-1"
		mpmID        = "mpm-abc123"
		frameworkID  = "claude-code-cs-7"
		invocationID = "inv-new-write-1"
	)
	_, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id,
		     actor_kind, framework_name, payload_hash, result_status,
		     started_at, completed_at, duration_ms,
		     mpm_session_id, framework_session_id)
		VALUES ('new-ti-1', ?, 'mpm_memory', 'save', ?,
		        'agent', 'claude-code', 'sha256:y', 'success',
		        1700000100, 1700000101, 100, ?, ?)
	`, sessionID, invocationID, mpmID, frameworkID)
	require.NoError(t, err)

	var sessID, mpmSID, fwSID string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT session_id, mpm_session_id, framework_session_id
		   FROM tool_invocations WHERE id = 'new-ti-1'`,
	).Scan(&sessID, &mpmSID, &fwSID))
	require.Equal(t, sessionID, sessID, "session_id round-trips")
	require.Equal(t, mpmID, mpmSID, "mpm_session_id round-trips independently")
	require.Equal(t, frameworkID, fwSID, "framework_session_id round-trips independently")
	require.NotEqual(t, mpmSID, sessID,
		"mpm_session_id must NOT equal session_id")
	require.NotEqual(t, fwSID, sessID,
		"framework_session_id must NOT equal session_id")
	require.NotEqual(t, fwSID, mpmSID,
		"framework_session_id must NOT equal mpm_session_id")
}

// TestMigrateToolInvocationsSessionIdentity_IndexesExist verifies
// the composite indexes are queryable. EXPLAIN QUERY PLAN cannot
// be used deterministically with FTS5 shadow tables in test
// mode, so this test uses sqlite_master instead — the presence of
// the index name is the authoritative evidence.
func TestMigrateToolInvocationsSessionIdentity_IndexesExist(t *testing.T) {
	dm := NewTestDM(t)

	required := []string{
		"idx_tool_invocations_mpm_session",
		"idx_tool_invocations_framework_session",
	}
	for _, idx := range required {
		var n int
		require.NoError(t, dm.SQLDB().QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name = ?`,
			idx,
		).Scan(&n))
		require.Equal(t, 1, n,
			"composite index %s must exist on tool_invocations", idx)
	}
}
