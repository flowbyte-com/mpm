package internal

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tableExists returns whether the named table is present in the (main) schema.
// A PRAGMA on an unknown table returns zero rows rather than erroring, so we
// just count.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	row := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name)
	require.NoError(t, row.Scan(&n))
	return n > 0
}

// indexExists returns whether the named index is present in the (main) schema.
func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	row := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", name)
	require.NoError(t, row.Scan(&n))
	return n > 0
}

// sharedIndexExists returns whether the named index exists in the `shared`
// schema of an attached shared DB. We must query through the same *sql.DB
// that ran the ATTACH — opening a separate connection would not see the
// attached schema.
func sharedIndexExists(t *testing.T, dm *DatabaseManager, name string) bool {
	t.Helper()
	var n int
	row := dm.db.QueryRow(
		"SELECT COUNT(*) FROM shared.sqlite_master WHERE type = 'index' AND name = ?",
		name,
	)
	require.NoError(t, row.Scan(&n))
	return n > 0
}

// hermeticDatabaseManager opens a DatabaseManager rooted at t.TempDir() so
// the test cannot touch the real production DB under $MPM_WORKSPACE. The
// shared DB (when opted into) lives in the same temp dir; NewDatabaseManager
// falls through to GetMPMDir which honours MPM_WORKSPACE.
//
// MPM_WORKSPACE has three purposes here:
//  1. Stop the production /home/v/.openclaw/.../mpm.db from being migrated.
//  2. Keep every artifact (DB file, watchdog.jsonl, mirror.jsonl) inside
//     the test's t.TempDir, so cleanup is automatic.
//  3. Match the contract other hermetic tests (config_workspace_test.go)
//     already establish.
func hermeticDatabaseManager(t *testing.T) *DatabaseManager {
	t.Helper()
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	dm, err := NewDatabaseManager(workspace)
	require.NoError(t, err, "NewDatabaseManager(%q)", workspace)
	t.Cleanup(func() { dm.Close() })
	return dm
}

// TestSchema_EpistemicCascadeOutboxTable asserts that the canonical schema
// creates the cascade outbox table with the columns required by the design
// spec (see docs/superpowers/specs/2026-08-04-epistemic-cascades-design.md).
//
// Failure mode the test is designed to catch: someone removes the table
// from BaseTables, renames a column, or forgets to ship the migration. All
// of those are silent until the materializer hits a SQL error at runtime.
func TestSchema_EpistemicCascadeOutboxTable(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	require.True(t, tableExists(t, store.DB.DB, "epistemic_cascade_outbox"),
		"epistemic_cascade_outbox table must exist after initUnifiedSchema")

	cols := getTableColumns(t, store.DB.DB, "epistemic_cascade_outbox")
	required := []string{
		"id",
		"invalidation_event_id",
		"dead_artifact_id",
		"dead_artifact_type",
		"downstream_artifact_id",
		"downstream_artifact_type",
		"trigger_evidence_id", // nullable
		"cascade_depth",
		"reason",
		"status",
		"materialized_theory_id", // nullable
		"attempt_count",
		"next_retry_at",
		"terminal_error",
		"created_at",
		"updated_at",
	}
	for _, want := range required {
		assert.Contains(t, cols, want, "epistemic_cascade_outbox missing column %q", want)
	}
}

// TestSchema_EpistemicProvenanceTable asserts the canonical schema creates
// the provenance citation table with the columns required by the design
// (typed source/downstream IDs, event ID, and timestamps).
func TestSchema_EpistemicProvenanceTable(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	require.True(t, tableExists(t, store.DB.DB, "epistemic_provenance"),
		"epistemic_provenance table must exist after initUnifiedSchema")

	cols := getTableColumns(t, store.DB.DB, "epistemic_provenance")
	required := []string{
		"id",
		"source_id",
		"source_type",
		"downstream_id",
		"downstream_type",
		"event_id",
		"created_at",
	}
	for _, want := range required {
		assert.Contains(t, cols, want, "epistemic_provenance missing column %q", want)
	}
}

// TestSchema_CascadeOutboxStatusCheck asserts the status column has a CHECK
// constraint enforcing the four legal states from the design spec:
// pending, processing, materialized, failed.
//
// The check is part of the structural invariant: the materializer only
// claims rows where status='pending', and the dead-letter transition is
// status='failed'. Without the CHECK, a typo silently creates rows that
// the worker will never pick up.
func TestSchema_CascadeOutboxStatusCheck(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	// Insert a row with a clearly-invalid status. The CHECK must reject it.
	_, err := store.DB.Exec(`
		INSERT INTO epistemic_cascade_outbox
		    (id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
		     downstream_artifact_id, downstream_artifact_type, cascade_depth,
		     reason, status)
		VALUES ('row-x', 'evt-x', 'dead-x', 'theory', 'down-x', 'theory', 0, 'r', 'nonsense')
	`)
	require.Error(t, err, "epistemic_cascade_outbox.status must reject unsupported values via CHECK")
	assert.Contains(t, strings.ToLower(err.Error()), "check",
		"error should mention CHECK constraint, got %v", err)

	// Sanity: all four design states are accepted.
	for _, status := range []string{"pending", "processing", "materialized", "failed"} {
		_, err := store.DB.Exec(`
			INSERT INTO epistemic_cascade_outbox
			    (id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
			     downstream_artifact_id, downstream_artifact_type, cascade_depth,
			     reason, status)
			VALUES (?, 'evt-' || ?, 'dead-x', 'theory', 'down-x', 'theory', 0, 'r', ?)
		`, "row-"+status, status, status)
		require.NoError(t, err, "epistemic_cascade_outbox must accept status=%q", status)
	}
}

// TestSchema_CascadeOutboxUniqueKey asserts the unique-constraint that
// dedupes invalidation events against targets. From the design spec:
//
//	The uniqueness key is (dead_artifact_id, downstream_artifact_id, invalidation_event_id).
//
// Two intents with the same triple must be rejected; a re-managed intent
// (different ID, same triple) must collapse into one row.
func TestSchema_CascadeOutboxUniqueKey(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	// First row with the canonical triple.
	_, err := store.DB.Exec(`
		INSERT INTO epistemic_cascade_outbox
		    (id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
		     downstream_artifact_id, downstream_artifact_type, cascade_depth,
		     reason, status)
		VALUES ('row-1', 'evt-1', 'dead-A', 'theory', 'down-B', 'decision', 1, 'r', 'pending')
	`)
	require.NoError(t, err)

	// Same triple different id — must be rejected by the unique constraint.
	_, err = store.DB.Exec(`
		INSERT INTO epistemic_cascade_outbox
		    (id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
		     downstream_artifact_id, downstream_artifact_type, cascade_depth,
		     reason, status)
		VALUES ('row-2', 'evt-1', 'dead-A', 'theory', 'down-B', 'decision', 1, 'r', 'pending')
	`)
	require.Error(t, err, "epistemic_cascade_outbox must reject duplicate (dead, downstream, event) triple")

	// Different downstream → acceptable.
	_, err = store.DB.Exec(`
		INSERT INTO epistemic_cascade_outbox
		    (id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
		     downstream_artifact_id, downstream_artifact_type, cascade_depth,
		     reason, status)
		VALUES ('row-3', 'evt-1', 'dead-A', 'theory', 'down-C', 'decision', 1, 'r', 'pending')
	`)
	require.NoError(t, err, "different downstream_artifact_id must be accepted")

	// Different event_id → acceptable.
	_, err = store.DB.Exec(`
		INSERT INTO epistemic_cascade_outbox
		    (id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
		     downstream_artifact_id, downstream_artifact_type, cascade_depth,
		     reason, status)
		VALUES ('row-4', 'evt-2', 'dead-A', 'theory', 'down-B', 'decision', 1, 'r', 'pending')
	`)
	require.NoError(t, err, "different invalidation_event_id must be accepted")
}

// TestSchema_CascadeOutboxIndexesPresent asserts the named indexes are
// explicitly present (idempotent naming gives the audit trail in
// sqlite_master). Names follow the idx_<table>_<columns> convention used
// elsewhere in the schema.
func TestSchema_CascadeOutboxIndexesPresent(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	required := []string{
		"idx_epistemic_cascade_outbox_event",
		"idx_epistemic_cascade_outbox_dead",
		"idx_epistemic_cascade_outbox_status_retry",
	}
	for _, name := range required {
		assert.Truef(t, indexExists(t, store.DB.DB, name),
			"expected index %q on epistemic_cascade_outbox", name)
	}
}

// TestSchema_EpistemicProvenanceIndexesPresent asserts the indexes required by
// the provenance lookup paths: source_id (for dependency discovery) and
// downstream_id (for reverse lookups).
func TestSchema_EpistemicProvenanceIndexesPresent(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	required := []string{
		"idx_epistemic_provenance_source",
		"idx_epistemic_provenance_downstream",
	}
	for _, name := range required {
		assert.Truef(t, indexExists(t, store.DB.DB, name),
			"expected index %q on epistemic_provenance", name)
	}
}

// TestSchema_CascadeTablesTimestampsAreInteger asserts the timestamp
// columns on the new tables are INTEGER (Unix epoch seconds), matching
// the codebase-wide convention set by the timestamps_unified_v1 migration
// (see MigrationTimestamps in schema documentation).
func TestSchema_CascadeTablesTimestampsAreInteger(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	for _, c := range []struct{ table, column string }{
		{"epistemic_cascade_outbox", "created_at"},
		{"epistemic_cascade_outbox", "updated_at"},
		{"epistemic_cascade_outbox", "next_retry_at"},
		{"epistemic_provenance", "created_at"},
	} {
		rows, err := store.DB.DB.Query(fmt.Sprintf("PRAGMA table_info(%s)", c.table))
		require.NoError(t, err)
		var found bool
		for rows.Next() {
			var cid, cname, ctype string
			var notnull, pk int
			var dflt sql.NullString
			require.NoError(t, rows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk))
			if cname != c.column {
				continue
			}
			found = true
			assert.Truef(t, strings.EqualFold(ctype, "INTEGER") || strings.EqualFold(ctype, "INT"),
				"%s.%s should be INTEGER, got %q", c.table, c.column, ctype)
		}
		rows.Close()
		assert.True(t, found, "%s.%s column not found", c.table, c.column)
	}
}

// TestSchema_CascadeTablesIdempotentInit verifies that running
// DatabaseManager init twice on the same database does not error and does
// not duplicate tables/indexes. This is the property that makes the
// post-upgrade migration safe to re-run on every boot.
func TestSchema_CascadeTablesIdempotentInit(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	// Second init must be a clean no-op (CREATE TABLE IF NOT EXISTS,
	// CREATE INDEX IF NOT EXISTS, SafeMigrations skip-on-duplicate).
	require.NoError(t, dm.InitSchema(),
		"InitSchema must be idempotent — second run must succeed")

	// The tables still exist.
	for _, table := range []string{"epistemic_cascade_outbox", "epistemic_provenance"} {
		assert.True(t, tableExists(t, dm.db, table),
			"%s must still exist after second init", table)
	}
}

// TestSchema_CascadeTablesUpgradeFromPreCascadeSchema is the real
// upgrade-path test for this task: a database that pre-dates the
// cascade feature (no epistemic_* tables, no cascade indexes) is
// upgraded by re-running InitSchema, and is left with the cascade
// tables and indexes in place.
//
// Why this matters: TestSchema_CascadeTablesIdempotentInit is the
// re-run-with-no-change case. It would pass even if the schema were
// unconditionally CREATE TABLE (no IF NOT EXISTS guard), because the
// existing tables would already be there from a prior call. The
// pre-cascade-schema fixture is what proves the IF NOT EXISTS path
// actually applies — no rows to migrate, just CREATE TABLE /
// CREATE INDEX IF NOT EXISTS firing on first call after the upgrade.
func TestSchema_CascadeTablesUpgradeFromPreCascadeSchema(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "pre-cascade.db")
	sqlDB, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer sqlDB.Close()

	// Build a pre-cascade schema: every BaseTables statement EXCEPT
	// the cascade tables and cascade indexes. This is the shape a
	// database would have had before the 2026-08-04 schema flip.
	for _, ddl := range preCascadeBaseTables() {
		_, err := sqlDB.Exec(ddl)
		require.NoError(t, err, "pre-cascade DDL (first 80 chars): %s", truncateDDL(ddl, 80))
	}

	// Sanity: the cascade tables are NOT yet present.
	assert.False(t, tableExists(t, sqlDB, "epistemic_cascade_outbox"),
		"pre-cascade schema must not contain epistemic_cascade_outbox yet")
	assert.False(t, tableExists(t, sqlDB, "epistemic_provenance"),
		"pre-cascade schema must not contain epistemic_provenance yet")

	// Now run InitSchema — the canonical migration entry point.
	dm := NewDatabaseManagerForDB(sqlDB)
	require.NoError(t, dm.InitSchema(),
		"InitSchema must succeed against pre-cascade DB")

	// After init, both tables and all cascade indexes must be present.
	for _, table := range []string{"epistemic_cascade_outbox", "epistemic_provenance"} {
		assert.True(t, tableExists(t, sqlDB, table),
			"after upgrade, %s must exist", table)
	}
	for _, idx := range []string{
		"idx_epistemic_cascade_outbox_event",
		"idx_epistemic_cascade_outbox_dead",
		"idx_epistemic_cascade_outbox_status_retry",
		"idx_epistemic_provenance_source",
		"idx_epistemic_provenance_downstream",
	} {
		assert.True(t, indexExists(t, sqlDB, idx),
			"after upgrade, index %s must exist", idx)
	}

	// And the pre-existing tables must still be intact — the upgrade
	// path must not destroy data.
	for _, table := range []string{"memories", "evidence", "scheduled_wakes"} {
		assert.True(t, tableExists(t, sqlDB, table),
			"after upgrade, pre-cascade table %s must still exist", table)
	}
}

// preCascadeBaseTables returns the BaseTables slice with the cascade
// additions removed. It is the fixture for the upgrade-path test.
// Cascade additions are detected by table/index name substring so the
// test is robust against minor rearrangements of the schema slice.
func preCascadeBaseTables() []string {
	out := make([]string, 0, len(BaseTables))
	for _, ddl := range BaseTables {
		if strings.Contains(ddl, "epistemic_cascade_outbox") ||
			strings.Contains(ddl, "epistemic_provenance") {
			continue
		}
		out = append(out, ddl)
	}
	return out
}

// truncateDDL returns the first n bytes of s, or s itself if shorter.
func truncateDDL(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// TestSchema_CascadeTablesInSharedAttach verifies that a fresh shared
// database attached via DatabaseManager.attachShared also receives the
// new cascade tables. This is the cross-epistemology invariant — cascades
// must be writable from a shared DB if the substrate is shared.
//
// Note: the `shared` schema is only visible to the connection that ran
// ATTACH. We must query via dm.db directly rather than opening a fresh
// connection to the shared file.
//
// The shared DB lives in the same temp dir as MPM_WORKSPACE so cleanup is
// hermetic — no risk of bumping the production shared DB.
func TestSchema_CascadeTablesInSharedAttach(t *testing.T) {
	workspace := t.TempDir()
	sharedPath := filepath.Join(workspace, "shared.db")
	t.Setenv("MPM_WORKSPACE", workspace)
	t.Setenv("MPM_SHARED_DB", sharedPath)
	t.Setenv("MPM_SHARED_READONLY", "")

	dm, err := NewDatabaseManager(workspace)
	require.NoError(t, err)
	defer dm.Close()

	for _, table := range []string{"epistemic_cascade_outbox", "epistemic_provenance"} {
		var n int
		err := dm.db.QueryRow(
			"SELECT COUNT(*) FROM shared.sqlite_master WHERE type = 'table' AND name = ?",
			table,
		).Scan(&n)
		require.NoError(t, err, "probe shared schema for %s", table)
		assert.Equalf(t, 1, n, "shared.%s must exist after attach", table)
	}
}

// TestSchema_CascadeIndexesInSharedAttach verifies that the cascade
// indexes also propagate to the attached shared schema. The attach code
// must rewrite CREATE INDEX statements in addition to CREATE TABLE so
// the materializer's claim hot-path (status, next_retry_at) is served by
// an index in the shared DB.
//
// This is the test the original share-check was missing — the table
// presence test passes when only CREATE TABLE is propagated, but the
// materializer's working-set query degenerates to a full scan if the
// index never lands.
func TestSchema_CascadeIndexesInSharedAttach(t *testing.T) {
	workspace := t.TempDir()
	sharedPath := filepath.Join(workspace, "shared.db")
	t.Setenv("MPM_WORKSPACE", workspace)
	t.Setenv("MPM_SHARED_DB", sharedPath)
	t.Setenv("MPM_SHARED_READONLY", "")

	dm, err := NewDatabaseManager(workspace)
	require.NoError(t, err)
	defer dm.Close()

	required := []string{
		"idx_epistemic_cascade_outbox_event",
		"idx_epistemic_cascade_outbox_dead",
		"idx_epistemic_cascade_outbox_status_retry",
		"idx_epistemic_provenance_source",
		"idx_epistemic_provenance_downstream",
	}
	for _, name := range required {
		assert.Truef(t, sharedIndexExists(t, dm, name),
			"shared schema must include index %q", name)
	}
}
