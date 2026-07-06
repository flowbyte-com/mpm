// audit_info_test.go — focused tests for the AuditInfo level addition
// and its supporting migration.
//
// Pins five contracts:
//
//   1. AuditInfo events land in system_audit_log.
//   2. AuditInfo events do NOT trigger cluster upsert (gate).
//   3. Existing warn/error/fatal cluster behaviour is unchanged (regression).
//   4. AuditLevel validation accepts the new 'info' value.
//   5. migrateAuditLevelConstraint is idempotent and preserves rows.
//
// Tests use NewTestDM (in-memory, hermetic) so they never touch the
// workspace database. Migration test (#5) explicitly recreates the
// audit_log table with the OLD CHECK constraint before invoking the
// migration, simulating an existing pre-AuditInfo database.
package internal

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Contract 1: AuditInfo events land in system_audit_log
// ---------------------------------------------------------------------------

func TestLogAudit_InfoRowsAreWritten(t *testing.T) {
	dm := NewTestDM(t)

	dm.LogAudit(AuditInfo, "epistemology", "shred_memory mem-test-1", "",
		AuditContext{"memory_id": "mem-test-1", "reason": "smoke"})

	rows, err := dm.db.Query(
		`SELECT level, component, message FROM system_audit_log
		 WHERE component = 'epistemology'`)
	require.NoError(t, err)
	defer rows.Close()

	var found bool
	for rows.Next() {
		var lvl, comp, msg string
		require.NoError(t, rows.Scan(&lvl, &comp, &msg))
		if msg == "shred_memory mem-test-1" {
			require.Equal(t, "info", lvl, "AuditInfo must round-trip as 'info'")
			require.Equal(t, "epistemology", comp)
			found = true
		}
	}
	require.True(t, found, "AuditInfo row must appear in system_audit_log")
}

// ---------------------------------------------------------------------------
// Contract 2: AuditInfo events do NOT trigger cluster upsert
// (the gating rule that keeps deliberate ops out of the anomaly ledger)
// ---------------------------------------------------------------------------

func TestLogAudit_InfoRowsDoNotCluster(t *testing.T) {
	dm := NewTestDM(t)

	// Hammer a single message enough times to clear the cluster
	// threshold (ClusterThreshold=3) — if the gate is broken, this
	// would produce an active cluster row.
	for i := 0; i < 5; i++ {
		dm.LogAudit(AuditInfo, "epistemology", "shred_memory mem-X", "",
			AuditContext{"memory_id": "mem-X"})
	}

	// Audit rows were written (5 of them)...
	var infoCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE level = 'info' AND component = 'epistemology'`,
	).Scan(&infoCount))
	require.Equal(t, 5, infoCount, "all 5 Info rows must persist in system_audit_log")

	// ...but no cluster row was created.
	var clusterCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM audit_cluster_proposals WHERE component = 'epistemology'`,
	).Scan(&clusterCount))
	require.Equal(t, 0, clusterCount,
		"AuditInfo events must NOT upsert audit_cluster_proposals — that's the gate")
}

// ---------------------------------------------------------------------------
// Contract 3: existing warn/error/fatal cluster behaviour is unchanged
// ---------------------------------------------------------------------------

func TestLogAudit_WarnRowsStillCluster(t *testing.T) {
	dm := NewTestDM(t)

	// Three identical warns exceed ClusterThreshold=3 — must cluster.
	for i := 0; i < 4; i++ {
		dm.LogAudit(AuditWarn, "cluster", "test warn message", "",
			AuditContext{"i": i})
	}

	var clusterCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM audit_cluster_proposals WHERE component = 'cluster'`,
	).Scan(&clusterCount))
	require.Equal(t, 1, clusterCount,
		"4 identical warns must produce exactly 1 cluster row (regression)")
}

// And Error rows continue to write both audit and cluster side-effects.
func TestLogAudit_ErrorRowsStillWriteAndCluster(t *testing.T) {
	dm := NewTestDM(t)

	dm.LogAudit(AuditError, "security", "test error path", "",
		AuditContext{"why": "regression"})

	var auditCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE level = 'error'`,
	).Scan(&auditCount))
	require.Equal(t, 1, auditCount, "Error rows must persist in system_audit_log")

	var clusterCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM audit_cluster_proposals WHERE component = 'security'`,
	).Scan(&clusterCount))
	require.Equal(t, 1, clusterCount, "Error rows must still upsert cluster (regression)")
}

// ---------------------------------------------------------------------------
// Contract 4: AuditLevel validation accepts the new 'info' value
// ---------------------------------------------------------------------------

func TestLogAudit_InvalidLevelsRejected(t *testing.T) {
	dm := NewTestDM(t)

	// "debug" is invalid — must be silently dropped (consistent with
	// the existing contract: invalid levels log to stderr and skip).
	dm.LogAudit(AuditLevel("debug"), "epistemology", "should not appear", "", nil)
	dm.LogAudit(AuditLevel(""), "epistemology", "empty level", "", nil)

	var n int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM system_audit_log`,
	).Scan(&n))
	require.Equal(t, 0, n, "invalid AuditLevel values must not insert rows")
}

// ---------------------------------------------------------------------------
// Contract 5a: migrateAuditLevelConstraint is idempotent
// ---------------------------------------------------------------------------

func TestMigrateAuditLevelConstraint_Idempotent(t *testing.T) {
	dm := NewTestDM(t)

	// Already migrated (InitSchema applied the new CREATE TABLE). Run
	// the migration again — must be a clean no-op.
	require.NoError(t, dm.migrateAuditLevelConstraint())

	// Confirm the schema still admits 'info'.
	dm.LogAudit(AuditInfo, "epistemology", "after second migration", "", nil)
	var n int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE level = 'info'`,
	).Scan(&n))
	require.Equal(t, 1, n, "idempotent migration must preserve 'info' capability")
}

// ---------------------------------------------------------------------------
// Contract 5b: migrateAuditLevelConstraint preserves existing rows
// ---------------------------------------------------------------------------

func TestMigrateAuditLevelConstraint_PreservesRowsAndAddsInfoCapability(t *testing.T) {
	dm := NewTestDM(t)

	// Snapshot existing rows (none yet, but count for delta verification).
	preCount := 0
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM system_audit_log`,
	).Scan(&preCount))

	// Insert some warn/error rows that MUST survive the migration.
	_, err := dm.db.Exec(`
		INSERT INTO system_audit_log (level, component, message, context, stack_trace, created_at)
		VALUES
		  ('warn',  'epistemology', 'pre-migration warn',  '{"k":"v"}', 'old-stack-A', '2026-07-01 12:00:00'),
		  ('error', 'security',     'pre-migration error', '{"k":"v"}', 'old-stack-B', '2026-07-01 12:01:00'),
		  ('fatal', 'runtime',      'pre-migration fatal', '{"k":"v"}', 'old-stack-C', '2026-07-01 12:02:00')`)
	require.NoError(t, err)

	// Force the table back to the OLD constraint (simulating a database
	// created before the AuditInfo addition). DROP + RENAME recreates
	// the exact pre-migration shape. Indexes are intentionally dropped
	// here too — they'll be rebuilt by CommonIndexes on the next
	// initUnifiedSchema, mirroring what happens in production.
	_, err = dm.db.Exec(`DROP TABLE system_audit_log`)
	require.NoError(t, err)
	_, err = dm.db.Exec(`
		CREATE TABLE system_audit_log (
			id          TEXT PRIMARY KEY,
			level       TEXT NOT NULL CHECK (level IN ('warn','error','fatal')),
			component   TEXT NOT NULL,
			message     TEXT NOT NULL,
			stack_trace TEXT,
			context     JSON,
			created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
		)`)
	require.NoError(t, err)
	// The 3 rows above were dropped along with the table. Re-insert them
	// so the migration actually has something to preserve.
	_, err = dm.db.Exec(`
		INSERT INTO system_audit_log (level, component, message, context, stack_trace, created_at)
		VALUES
		  ('warn',  'epistemology', 'pre-migration warn',  '{"k":"v"}', 'old-stack-A', '2026-07-01 12:00:00'),
		  ('error', 'security',     'pre-migration error', '{"k":"v"}', 'old-stack-B', '2026-07-01 12:01:00'),
		  ('fatal', 'runtime',      'pre-migration fatal', '{"k":"v"}', 'old-stack-C', '2026-07-01 12:02:00')`)
	require.NoError(t, err)

	// Sanity: the OLD table rejects 'info'. Proves we set up the
	// pre-migration state correctly.
	_, err = dm.db.Exec(`
		INSERT INTO system_audit_log (level, component, message)
		VALUES ('info', 'epistemology', 'would fail')`)
	require.Error(t, err, "OLD constraint must reject 'info' (otherwise we didn't set up the test right)")
	require.True(t, strings.Contains(err.Error(), "CHECK") || strings.Contains(err.Error(), "constraint"),
		"failure must mention the CHECK constraint; got %v", err)

	// Run the migration.
	require.NoError(t, dm.migrateAuditLevelConstraint())

	// Post-migration: 3 original rows preserved (compare by message).
	rows, err := dm.db.Query(
		`SELECT level, message FROM system_audit_log
		 WHERE message LIKE 'pre-migration %' ORDER BY created_at`)
	require.NoError(t, err)
	defer rows.Close()
	type row struct{ level, msg string }
	var preserved []row
	for rows.Next() {
		var r row
		require.NoError(t, rows.Scan(&r.level, &r.msg))
		preserved = append(preserved, r)
	}
	require.Len(t, preserved, 3, "all 3 pre-migration rows must survive")
	require.Equal(t, "warn", preserved[0].level)
	require.Equal(t, "error", preserved[1].level)
	require.Equal(t, "fatal", preserved[2].level)

	// Post-migration: 'info' is now insertable via LogAudit (proves
	// the new constraint is live).
	dm.LogAudit(AuditInfo, "epistemology", "post-migration info", "",
		AuditContext{"smoke": true})
	var n int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE level = 'info'`,
	).Scan(&n))
	require.Equal(t, 1, n, "post-migration: AuditInfo rows must be insertable")
}

// ---------------------------------------------------------------------------
// Contract 5c: migrateAuditLevelConstraint handles missing-table case
// (fresh install — CREATE TABLE IF NOT EXISTS has not yet run)
// ---------------------------------------------------------------------------

func TestMigrateAuditLevelConstraint_NoOpWhenTableMissing(t *testing.T) {
	dm := NewTestDM(t)

	// Drop the table (simulating a state where InitSchema hasn't run yet).
	_, err := dm.db.Exec(`DROP TABLE system_audit_log`)
	require.NoError(t, err)

	// Migration must return cleanly without error.
	require.NoError(t, dm.migrateAuditLevelConstraint(),
		"missing-table case must be a clean no-op, not a fatal")
}
