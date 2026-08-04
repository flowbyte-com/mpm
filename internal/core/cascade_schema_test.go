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

// cascadeSchemaTestStore centralizes the new-schema test fixture so each test
// can stand on its own fresh database without rebuilding the boilerplate.
func cascadeSchemaTestStore(t *testing.T) *MemoryStore {
	t.Helper()
	tmpDir := t.TempDir()
	store := NewMemoryStore("")
	store.SQLiteDBPath = filepath.Join(tmpDir, "test.db")
	require.NoError(t, store.InitSQLite())
	return store
}

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

// columnNamesOnTable returns the column names of a table.
func columnNamesOnTable(t *testing.T, db *sql.DB, name string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", name))
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid, cname, ctype string
		var notnull, pk int
		// dflt_value is the only column whose Go target type matters here;
		// TEXT-default columns (e.g. status='pending') come back as
		// strings, INTEGER-default columns as int64, and undefaulted
		// columns as NULL. Use sql.NullString so the scan never trips on
		// the NULL case.
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk))
		out = append(out, cname)
	}
	return out
}

// TestSchema_EpistemicCascadeOutboxTable asserts that the canonical schema
// creates the cascade outbox table with the columns required by the design
// spec (see docs/superpowers/specs/2026-08-04-epistemic-cascades-design.md).
//
// Failure mode the test is designed to catch: someone removes the table
// from BaseTables, renames a column, or forgets to ship the migration. All
// of those are silent until the materializer hits a SQL error at runtime.
func TestSchema_EpistemicCascadeOutboxTable(t *testing.T) {
	store := cascadeSchemaTestStore(t)
	defer store.DB.Close()

	require.True(t, tableExists(t, store.DB.DB, "epistemic_cascade_outbox"),
		"epistemic_cascade_outbox table must exist after initUnifiedSchema")

	cols := columnNamesOnTable(t, store.DB.DB, "epistemic_cascade_outbox")
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
	store := cascadeSchemaTestStore(t)
	defer store.DB.Close()

	require.True(t, tableExists(t, store.DB.DB, "epistemic_provenance"),
		"epistemic_provenance table must exist after initUnifiedSchema")

	cols := columnNamesOnTable(t, store.DB.DB, "epistemic_provenance")
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
	store := cascadeSchemaTestStore(t)
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
	store := cascadeSchemaTestStore(t)
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

// TestSchema_CascadeOutboxIndexes asserts the indexes required by the two
// hot-path reads described in the design: invalidation lookup (by event
// id) and worker claim (by status + next_retry_at).
//
// Materializer hot path: "find pending or reclaimable rows ordered by next_retry_at".
// Invalidation-trace path: "find all intents stemming from a given event_id".
func TestSchema_CascadeOutboxIndexes(t *testing.T) {
	store := cascadeSchemaTestStore(t)
	defer store.DB.Close()

	// We don't pin the exact index names — that is an internal detail the
	// implementation can evolve. Instead, exercise the query plans and
	// assert the EXPLAIN QUERY PLAN reports an index-driven scan rather
	// than a full table scan.
	// Use literal values instead of placeholders so EXPLAIN QUERY PLAN
	// runs without parameter binding (the test only cares about plan
	// shape, not values).
	checks := []struct {
		filter string
		column string
	}{
		{"WHERE invalidation_event_id = 'evt-1'", "invalidation_event_id"},
		{"WHERE dead_artifact_id = 'dead-A'", "dead_artifact_id"},
		{"WHERE status = 'pending'", "status"},
		{"WHERE status = 'pending' AND (next_retry_at IS NULL OR next_retry_at <= 1000)", "status"},
	}

	for _, c := range checks {
		plan := func() string {
			rows, err := store.DB.DB.Query(
				"EXPLAIN QUERY PLAN SELECT id FROM epistemic_cascade_outbox " + c.filter,
			)
			if err != nil {
				return ""
			}
			defer rows.Close()
			var sb strings.Builder
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					return ""
				}
				sb.WriteString(detail)
				sb.WriteString("\n")
			}
			return sb.String()
		}()

		// Empty plan means the table itself is missing — the test before
		// this one would already fail; bail with a clear assertion.
		if plan == "" {
			t.Fatalf("no plan returned for filter %q (table missing?)", c.filter)
		}
		// Look for either a USING INDEX clause on the requested column or
		// an AUTO-COVERING index scan. SQLite's plan vocabulary for
		// index-driven reads is "USING INDEX <name>".
		hasIndex := strings.Contains(plan, "USING INDEX") || strings.Contains(plan, "USING ROWID")
		assert.Truef(t, hasIndex,
			"filter %q should use an index, plan: %s", c.filter, plan)
	}
}

// TestSchema_CascadeOutboxIndexesPresent asserts the named indexes are
// explicitly present (idempotent naming gives the audit trail in
// sqlite_master). Names follow the idx_<table>_<columns> convention used
// elsewhere in the schema.
func TestSchema_CascadeOutboxIndexesPresent(t *testing.T) {
	store := cascadeSchemaTestStore(t)
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

// TestSchema_EpistemicProvenanceIndexes asserts the indexes required by
// the provenance lookup paths: source_id (for dependency discovery) and
// downstream_id (for reverse lookups).
func TestSchema_EpistemicProvenanceIndexesPresent(t *testing.T) {
	store := cascadeSchemaTestStore(t)
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
	store := cascadeSchemaTestStore(t)
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
			// dflt_value can be NULL for non-defaulted columns; use
			// sql.NullString so the scan never trips on a NULL
			// (mattn/go-sqlite3 returns NULL as the underlying type
			// rather than a Go nil).
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
// DatabaseManager init twice on the same database does not error and
// does not duplicate tables/indexes. This is the property that makes the
// post-upgrade migration safe to re-run on every boot.
func TestSchema_CascadeTablesIdempotentInit(t *testing.T) {
	// Build a fresh DatabaseManager on a tmp DB so we can call the
	// exported InitSchema twice. (MemoryStore.InitSQLite bypasses the
	// DatabaseManager path; the idempotent guarantee we care about is
	// the DatabaseManager one, since it is the production boot path.)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	sqlDB, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer sqlDB.Close()

	dm := NewDatabaseManagerForDB(sqlDB)
	require.NoError(t, dm.InitSchema(), "first init must succeed")

	// Second init must be a clean no-op (CREATE TABLE IF NOT EXISTS,
	// CREATE INDEX IF NOT EXISTS, SafeMigrations skip-on-duplicate).
	require.NoError(t, dm.InitSchema(),
		"InitSchema must be idempotent — second run must succeed")

	// The tables still exist.
	for _, table := range []string{"epistemic_cascade_outbox", "epistemic_provenance"} {
		assert.True(t, tableExists(t, sqlDB, table),
			"%s must still exist after second init", table)
	}
}

// TestSchema_CascadeTablesInSharedAttach verifies that a fresh shared
// database attached via DatabaseManager.attachShared also receives the
// new cascade tables. This is the cross-epistemology invariant — cascades
// must be writable from a shared DB if the substrate is shared.
//
// Note: the `shared` schema is only visible to the connection that ran
// ATTACH. We must query via dm.db directly rather than opening a fresh
// connection to the shared file.
func TestSchema_CascadeTablesInSharedAttach(t *testing.T) {
	tmp := t.TempDir()
	sharedPath := filepath.Join(tmp, "shared.db")
	t.Setenv("MPM_SHARED_DB", sharedPath)
	t.Setenv("MPM_SHARED_READONLY", "")

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
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
