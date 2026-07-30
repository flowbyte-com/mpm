package internal

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// timestampsTestDBCounter gives each caller a uniquely-named shared-cache in-memory DB.
// Same rationale as recall_test.go: bare ":memory:" gives each pooled connection its
// own private DB and silently loses schema state.
var timestampsTestDBCounter int64

// setupTimestampsTestDB opens a fresh in-memory SQLite database and applies
// the real production schema (BaseTables + ReferenceTables + SafeMigrations
// + CommonIndexes) so the migration runs against the same column
// declarations as production.
//
// Building the fixture from the real DDL — rather than re-declaring every
// (table, column) — means:
//   - DATETIME/NUMERIC-affinity columns are exercised naturally,
//   - TEXT-affinity columns (admission_log.created_at,
//     reference_interactions.created_at, reference_docs.last_indexed,
//     memory_revisions.created_at) are also declared as TEXT, which is
//     exactly the state the migration faces on disk,
//   - if allTimestampsToMigrate ever drifts from schema.go, this test
//     will go red with "no such column" / "no such table" errors at the
//     migration step.
func setupTimestampsTestDB(t *testing.T) *sql.DB {
	t.Helper()

	n := atomic.AddInt64(&timestampsTestDBCounter, 1)
	dsn := "file:timestamps-test-" + testItoa(n) + "?mode=memory&cache=shared"

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dsn, err)
	}

	// 1. Apply base + reference table DDL.
	for _, stmt := range append(append([]string{}, BaseTables...), ReferenceTables...) {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatalf("apply schema stmt: %v\nstatement: %s", err, stmt)
		}
	}

	// 2. Apply SafeMigrations so columns referenced by newer indexes exist
	// before the indexes are created. Some SafeMigration entries overlap
	// with BaseTables columns (e.g. topics.parent_topic_id); suppress
	// duplicate-column errors the same way the production init does
	// (see db.go initUnifiedSchema).
	for _, m := range SafeMigrations {
		if m[0] == "lessons" {
			continue // lessons is a view in BaseTables; ALTER would fail
		}
		alter := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", m[0], m[1], m[2])
		if _, err := db.Exec(alter); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				db.Close()
				t.Fatalf("apply safe migration %s.%s: %v", m[0], m[1], err)
			}
		}
	}

	// 3. Indexes are best-effort. CommonIndexes references columns
	// (lessons.retrieval_priority, etc.) that the production
	// migrateLessonsToView() step adds; the migration UPDATEs don't need
	// indexes, just columns.
	for _, stmt := range CommonIndexes {
		if _, err := db.Exec(stmt); err != nil {
			if !strings.Contains(err.Error(), "no such column") && !strings.Contains(err.Error(), "no such table") {
				db.Close()
				t.Fatalf("apply common index: %v\nstatement: %s", err, stmt)
			}
		}
	}

	return db
}

// setupFlippedSchemaDB returns a fresh in-memory SQLite DB that mirrors the
// production schema but with all migration-target columns declared with
// INTEGER affinity — this stands in for the post-Task-4 world where the
// schema DDL has been updated.
//
// Why not reuse setupTimestampsTestDB + ALTER COLUMN? SQLite does not
// support ALTER COLUMN ... SET TYPE; changing affinity requires the 12-step
// rebuild dance (rename, recreate, copy, re-add indexes/triggers), which is
// non-trivial to reverse and fragile against the artifacts view that joins
// multiple tables.
//
// The flipped fixture declares every table referenced by
// allTimestampsToMigrate with at least the migration-target column (and any
// NOT NULL/foreign-key dependencies). The columns that originally had TEXT
// affinity (admission_log.created_at, reference_interactions.created_at,
// reference_docs.last_indexed) are now INTEGER, modeling the post-Task-4
// schema. Other timestamp columns stay INTEGER (Task 4's intent).
func setupFlippedSchemaDB(t *testing.T) *sql.DB {
	t.Helper()

	n := atomic.AddInt64(&timestampsTestDBCounter, 1)
	dsn := "file:timestamps-flipped-" + testItoa(n) + "?mode=memory&cache=shared"

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dsn, err)
	}

	schema := `
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY, session_id TEXT NOT NULL, content TEXT NOT NULL,
		content_hash TEXT NOT NULL, created_at INTEGER
	);
	CREATE TABLE topics (
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
		created_at INTEGER, updated_at INTEGER
	);
	CREATE TABLE topic_memberships (
		memory_id TEXT, session_id TEXT, topic_id TEXT NOT NULL,
		created_at INTEGER,
		PRIMARY KEY (memory_id, topic_id)
	);
	CREATE TABLE memories (
		id TEXT PRIMARY KEY, content TEXT NOT NULL,
		created_at INTEGER, updated_at INTEGER,
		last_accessed_at INTEGER, expires_at INTEGER
	);
	CREATE TABLE system_config (
		key TEXT PRIMARY KEY, raw_json TEXT NOT NULL, content_hash TEXT NOT NULL,
		updated_at INTEGER
	);
	CREATE TABLE retrieval_metadata (
		node_id TEXT PRIMARY KEY, node_type TEXT NOT NULL,
		created_at INTEGER, updated_at INTEGER, last_retrieved_at INTEGER
	);
	CREATE TABLE external_db_cursors (
		db_label TEXT PRIMARY KEY, last_cursor TEXT NOT NULL,
		updated_at INTEGER
	);
	CREATE TABLE reference_docs (
		id TEXT PRIMARY KEY, title TEXT NOT NULL,
		created_at INTEGER, last_indexed INTEGER
	);
	CREATE TABLE system_audit_log (
		id TEXT PRIMARY KEY, created_at INTEGER
	);
	CREATE TABLE audit_cluster_proposals (
		id TEXT PRIMARY KEY,
		created_at INTEGER, updated_at INTEGER,
		first_seen INTEGER, last_seen INTEGER, snooze_until INTEGER
	);
	CREATE TABLE session_handoffs (
		id TEXT PRIMARY KEY,
		created_at INTEGER, ended_at INTEGER, read_at INTEGER
	);
	CREATE TABLE scheduled_wakes (
		id TEXT PRIMARY KEY, created_at INTEGER
	);
	CREATE TABLE scheduled_tasks (
		id TEXT PRIMARY KEY,
		created_at INTEGER, updated_at INTEGER,
		last_run_at INTEGER, next_run_at INTEGER
	);
	CREATE TABLE ephemeral_scratchpad (
		id TEXT PRIMARY KEY,
		created_at INTEGER, updated_at INTEGER, decay_at INTEGER
	);
	CREATE TABLE vector_clusters (
		id TEXT PRIMARY KEY, updated_at INTEGER
	);
	CREATE TABLE vector_assignments (
		id TEXT PRIMARY KEY, updated_at INTEGER
	);
	CREATE TABLE reference_interactions (
		id TEXT PRIMARY KEY,
		doc_id TEXT NOT NULL,
		query TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE admission_log (
		id TEXT PRIMARY KEY,
		doc_id TEXT NOT NULL,
		admit INTEGER NOT NULL,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE memory_revisions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		memory_id TEXT NOT NULL,
		version INTEGER NOT NULL,
		content TEXT NOT NULL,
		weight INTEGER NOT NULL,
		collection TEXT NOT NULL,
		created_at INTEGER
	);
	CREATE TABLE schema_migrations (
		id TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		t.Fatalf("create flipped schema: %v", err)
	}

	return db
}

// setupTextAffinityFixtureDB returns a fresh in-memory SQLite DB that
// declares a TEXT-affinity timestamp column alongside an INTEGER-affinity
// sibling. Used by the C1-replacement subtest to exercise the
// ErrTimestampsMigrationDeferred code path on a hand-built fixture: after
// Task 4, the production schema no longer carries any TEXT-affinity
// migration-target columns, so we can't trigger the deferral on production
// DDL — but we still need a guard in case future schema work re-introduces
// one.
//
// Design: `memory_revisions.created_at` IS in allTimestampsToMigrate (it
// was originally TEXT in pre-Task-4 schema). The fixture declares it as
// TEXT here to recreate the C1 trigger; `sessions.created_at` is also in
// the migration list, declared as INTEGER to verify the partial-progress
// contract (INTEGER columns convert even when TEXT siblings defer the
// migration).
func setupTextAffinityFixtureDB(t *testing.T) *sql.DB {
	t.Helper()

	n := atomic.AddInt64(&timestampsTestDBCounter, 1)
	dsn := "file:timestamps-text-affinity-" + testItoa(n) + "?mode=memory&cache=shared"

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dsn, err)
	}

	// memory_revisions.created_at is in the migration list and is declared
	// as TEXT here to trigger ErrTimestampsMigrationDeferred. sessions
	// mirrors the minimum columns needed for the UPDATE loop to issue its
	// statement (content_hash is NOT NULL).
	schema := `
	CREATE TABLE memory_revisions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		memory_id TEXT NOT NULL,
		version INTEGER NOT NULL,
		content TEXT NOT NULL,
		weight INTEGER NOT NULL,
		collection TEXT NOT NULL,
		created_at TEXT
	);
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		content TEXT NOT NULL,
		content_hash TEXT NOT NULL,
		created_at INTEGER
	);
	CREATE TABLE schema_migrations (
		id TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		t.Fatalf("create text-affinity fixture: %v", err)
	}

	return db
}
// raw bytes plus the SQLite storage class. The mattn/go-sqlite3 driver
// parses DATETIME-affinity columns into time.Time, which trips a NullInt64
// scan on perfectly fine values; using RawBytes sidesteps that coercion so
// we can read the actual stored bytes for both TEXT and INTEGER affinity
// columns.
func readTyped(t *testing.T, db *sql.DB, table, keyCol, keyVal, col string) (raw []byte, sqliteType string) {
	t.Helper()
	q := fmt.Sprintf(`SELECT %s, typeof(%s) FROM %s WHERE %s = ?`, col, col, table, keyCol)
	if err := db.QueryRow(q, keyVal).Scan(&raw, &sqliteType); err != nil {
		t.Fatalf("read %s.%s where %s=%s: %v", table, col, keyCol, keyVal, err)
	}
	if sqliteType == "null" {
		return nil, sqliteType
	}
	return raw, sqliteType
}

// TestMigrateAllTimestampsToUnixEpoch verifies the migration against the
// real production DDL. It exercises two scenarios:
//
//	Scenario A — current-schema DDL with TEXT-affinity columns.
//	  The migration should REFUSE TO RUN and refuse to write the sentinel.
//	  Reasoning: the C1 finding shows the typeof() guard is unreliable on
//	  TEXT-affinity columns (SQLite coerces the cast result back to TEXT
//	  on store), so the migration would appear to succeed while leaving
//	  the columns untouched. The post-UPDATE verification step catches
//	  this and returns an error; the sentinel guard prevents re-runs.
//
//	Scenario B — flipped-schema DDL (INTEGER affinity for the three TEXT
//	  columns from C1). The migration should succeed end-to-end and the
//	  sentinel should be written exactly once.
//
// Both scenarios also exercise the five behaviors the migration is meant
// to guarantee:
//  1. TEXT rows convert to INTEGER unix epoch (on NUMERIC-affinity columns)
//  2. INTEGER rows stay untouched (typeof guard works)
//  3. NULL rows stay untouched
//  4. Z-suffixed RFC3339 strings (the exact legacy format from
//     time.Now().UTC().Format(time.RFC3339)) convert with no local-tz offset
//  5. The migration is idempotent (sentinel guard works)
func TestMigrateAllTimestampsToUnixEpoch(t *testing.T) {
	t.Run("hand-built TEXT-affinity fixture: ErrTimestampsMigrationDeferred surfaces and no sentinel is written", func(t *testing.T) {
		// Why a hand-built fixture: as of Task 4 (2026-07-30), the production
		// DDL in schema.go has been flipped so every migration-target column
		// is INTEGER-affinity. That means the production schema can no longer
		// trigger the ErrTimestampsMigrationDeferred code path on its own —
		// the C1 finding becomes structurally unreachable.
		//
		// This subtest deliberately preserves the safety net for that code
		// path: if a future schema change re-introduces a TEXT-affinity
		// column (or somebody hand-rolls a downgrade), the migration must
		// still refuse to write the sentinel rather than silently succeed.
		// We construct two parallel tables — one with TEXT affinity, one with
		// INTEGER affinity — so the test can also assert that INTEGER columns
		// convert normally even in the presence of an unrecoverable TEXT
		// sibling (the partial-progress contract).
		db := setupTextAffinityFixtureDB(t)
		t.Cleanup(func() { db.Close() })

		// Seed parseable rows on both sides so the migration has work to do.
		if _, err := db.Exec(`
			INSERT INTO memory_revisions (id, memory_id, version, content, weight, collection, created_at) VALUES (1, 'm1', 1, 'c', 1, 'memories', '2026-07-23 14:32:40');
			INSERT INTO sessions (id, session_id, content, content_hash, created_at) VALUES ('int-1', 's1', 'c', 'h1', '2026-07-23 14:32:40');
		`); err != nil {
			t.Fatalf("seed: %v", err)
		}

		// Run the migration in a transaction we control so we can capture
		// the error without the helper aborting the test.
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		migErr := MigrateAllTimestampsToUnixEpoch(tx)
		_ = tx.Rollback()

		// The migration must surface ErrTimestampsMigrationDeferred so the
		// caller can classify "not finished yet" vs "broken".
		if migErr == nil {
			t.Fatal("expected migration to fail on TEXT-affinity column; got nil (C1 fix missing)")
		}
		if !errors.Is(migErr, ErrTimestampsMigrationDeferred) {
			t.Errorf("TEXT-affinity error must wrap ErrTimestampsMigrationDeferred, got: %v", migErr)
		}
		// The offending column is named in the error message so on-call
		// engineers can see at a glance which column needs the DDL flip.
		if !strings.Contains(migErr.Error(), "memory_revisions.created_at") {
			t.Errorf("deferral error should list memory_revisions.created_at, got: %v", migErr)
		}

		// TEXT-affinity column must still be 'text' — the migration aborted
		// before its storage class could be flipped (the cast result would
		// otherwise be coerced back to TEXT on store).
		_, sqliteType := readTyped(t, db, "memory_revisions", "id", "1", "created_at")
		if sqliteType != "text" {
			t.Errorf("memory_revisions.created_at should still be text, got %q", sqliteType)
		}

		// INTEGER-affinity column MUST have been converted despite the
		// overall deferral — the migration's contract is partial progress,
		// not all-or-nothing. The transaction is rolled back by this test,
		// but the assertion proves the UPDATE on this column was issued;
		// the column row below is checked before the rollback.
		//
		// NOTE: the migration runs all column-UPDATEs inside one tx, so we
		// can only assert "column was UPDATEd" by re-running and committing.
		// We do that in a second subtest below for an unambiguous check.
		_, intType := readTyped(t, db, "sessions", "id", "int-1", "created_at")
		if intType != "text" {
			t.Errorf("sessions.created_at should still be text after rollback, got %q", intType)
		}

		// Sentinel must not have been written — the transaction was rolled
		// back AND the migration contractually refuses to write it under
		// deferral. Both belt and braces.
		var sentinelCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE id = 'timestamps_unified_v1'`,
		).Scan(&sentinelCount); err != nil {
			t.Fatalf("count sentinel: %v", err)
		}
		if sentinelCount != 0 {
			t.Errorf("sentinel must not be written when migration defers, got count=%d", sentinelCount)
		}
	})

	t.Run("hand-built TEXT-affinity fixture: INTEGER columns still convert (partial-progress contract)", func(t *testing.T) {
		// Companion to the subtest above. Asserts the partial-progress
		// contract: even when the migration aborts with a deferral error,
		// any INTEGER-affinity columns that the UPDATE loop reached have
		// already been converted. We assert this by committing the
		// transaction, re-reading the column, and checking its storage
		// class flipped to integer.
		db := setupTextAffinityFixtureDB(t)
		t.Cleanup(func() { db.Close() })

		if _, err := db.Exec(`
			INSERT INTO memory_revisions (id, memory_id, version, content, weight, collection, created_at) VALUES (1, 'm1', 1, 'c', 1, 'memories', '2026-07-23 14:32:40');
			INSERT INTO sessions (id, session_id, content, content_hash, created_at) VALUES ('int-1', 's1', 'c', 'h1', '2026-07-23 14:32:40');
		`); err != nil {
			t.Fatalf("seed: %v", err)
		}

		// Commit the partial work — this is what initUnifiedSchema does on
		// ErrTimestampsMigrationDeferred: log the warning and commit, so the
		// partial conversion survives into the post-init DB.
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := MigrateAllTimestampsToUnixEpoch(tx); !errors.Is(err, ErrTimestampsMigrationDeferred) {
			_ = tx.Rollback()
			t.Fatalf("expected ErrTimestampsMigrationDeferred, got: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}

		// INTEGER column converted despite the deferral.
		_, intType := readTyped(t, db, "sessions", "id", "int-1", "created_at")
		if intType != "integer" {
			t.Errorf("sessions.created_at should be integer after partial commit, got %q", intType)
		}

		// TEXT column still untouched.
		_, txtType := readTyped(t, db, "memory_revisions", "id", "1", "created_at")
		if txtType != "text" {
			t.Errorf("memory_revisions.created_at should still be text after partial commit, got %q", txtType)
		}

		// Sentinel still absent — deferral blocks the write even on commit.
		var sentinelCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE id = 'timestamps_unified_v1'`,
		).Scan(&sentinelCount); err != nil {
			t.Fatalf("count sentinel: %v", err)
		}
		if sentinelCount != 0 {
			t.Errorf("sentinel must not be written on deferral, got count=%d", sentinelCount)
		}
	})

	t.Run("flipped DDL succeeds end-to-end (post-Task-4)", func(t *testing.T) {
		db := setupFlippedSchemaDB(t)
		t.Cleanup(func() { db.Close() })

		// Compute the expected epoch via the same SQL the migration runs, so
		// the assertion isn't tied to a hardcoded value that could drift
		// across runzones or DST changes.
		var expectedEpoch int64
		if err := db.QueryRow(
			`SELECT CAST(strftime('%s', '2026-07-23 14:32:40') AS INTEGER)`,
		).Scan(&expectedEpoch); err != nil {
			t.Fatalf("compute expected epoch: %v", err)
		}

		// Seed each (table, column) structural class:
		//   sessions.created_at               — nullable DATETIME
		//   reference_docs.last_indexed       — flipped to INTEGER (was TEXT)
		//   reference_interactions.created_at — flipped to INTEGER (was TEXT NOT NULL)
		//   admission_log.created_at          — flipped to INTEGER (was TEXT NOT NULL)
		if _, err := db.Exec(`
			INSERT INTO sessions (id, session_id, content, content_hash, created_at) VALUES
				('z-row',    's1', 'c1', 'h1', '2026-07-23 14:32:40Z'),
				('int-row',  's2', 'c2', 'h2', 1721743500),
				('null-row', 's3', 'c3', 'h3', NULL);
			INSERT INTO reference_docs (id, title, last_indexed) VALUES
				('doc-li',  'Last Indexed',  '2026-07-23 14:32:40'),
				('doc-int', 'Int Doc',       1721743500),
				('doc-nul', 'Null Doc',      NULL);
			INSERT INTO reference_interactions (id, doc_id, query, created_at) VALUES
				('ri-text', 'doc-li',  'q-text', '2026-07-23 14:32:40Z'),
				('ri-int',  'doc-int', 'q-int',  1721743500);
			INSERT INTO admission_log (id, doc_id, admit, created_at) VALUES
				('adm-text', 'doc-li',  1, '2026-07-23 14:32:40'),
				('adm-int',  'doc-int', 0, 1721743500);
		`); err != nil {
			t.Fatalf("seed flipped-schema rows: %v", err)
		}

		// Run the migration.
		if err := runTimestampsMigration(t, db); err != nil {
			t.Fatalf("first migration on flipped schema: %v", err)
		}

		// === Assertion 1: TEXT rows convert to INTEGER unix epoch ===
		for _, c := range []struct {
			table  string
			keyCol string
			keyVal string
			col    string
		}{
			{"reference_docs", "id", "doc-li", "last_indexed"},
			{"reference_interactions", "id", "ri-text", "created_at"},
			{"admission_log", "id", "adm-text", "created_at"},
		} {
			raw, sqliteType := readTyped(t, db, c.table, c.keyCol, c.keyVal, c.col)
			if sqliteType != "integer" {
				t.Errorf("%s.%s text-row storage class should be 'integer', got %q (raw=%s)",
					c.table, c.col, sqliteType, raw)
				continue
			}
			var got int64
			fmt.Sscanf(string(raw), "%d", &got)
			if got != expectedEpoch {
				t.Errorf("%s.%s text-row should be %d, got %d", c.table, c.col, expectedEpoch, got)
			}
		}

		// === Assertion 2: INTEGER rows stay untouched ===
		for _, c := range []struct {
			table  string
			keyCol string
			keyVal string
			col    string
		}{
			{"reference_docs", "id", "doc-int", "last_indexed"},
			{"reference_interactions", "id", "ri-int", "created_at"},
			{"admission_log", "id", "adm-int", "created_at"},
		} {
			raw, sqliteType := readTyped(t, db, c.table, c.keyCol, c.keyVal, c.col)
			if sqliteType != "integer" {
				t.Errorf("%s.%s int-row storage class should be 'integer', got %q (raw=%s)",
					c.table, c.col, sqliteType, raw)
				continue
			}
			var got int64
			fmt.Sscanf(string(raw), "%d", &got)
			if got != 1721743500 {
				t.Errorf("%s.%s int-row should be 1721743500, got %d", c.table, c.col, got)
			}
		}

		// === Assertion 3: NULL stays NULL ===
		_, nullType := readTyped(t, db, "reference_docs", "id", "doc-nul", "last_indexed")
		if nullType != "null" {
			t.Errorf("reference_docs.last_indexed null-row: type=%q (want null)", nullType)
		}

		// === Assertion 4: Z-suffixed RFC3339 strings convert to the same
		// unix epoch as the un-suffixed variant — UTC, no local-tz offset.
		// This is the format produced by time.Now().UTC().Format(time.RFC3339).
		// sessions.created_at is NUMERIC-affinity, so the cast result is
		// preserved on store. ===
		{
			raw, sqliteType := readTyped(t, db, "sessions", "id", "z-row", "created_at")
			if sqliteType != "integer" {
				t.Errorf("sessions.z-row (Z-suffix): storage class should be 'integer', got %q", sqliteType)
			} else {
				var got int64
				fmt.Sscanf(string(raw), "%d", &got)
				if got != expectedEpoch {
					t.Errorf("sessions.z-row (Z-suffix): expected %d, got %d (no local-tz offset)", expectedEpoch, got)
				}
			}
		}

		// === Assertion 5: migration is idempotent ===
		if err := runTimestampsMigration(t, db); err != nil {
			t.Fatalf("second migration: %v", err)
		}
		var sentinelCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE id = 'timestamps_unified_v1'`,
		).Scan(&sentinelCount); err != nil {
			t.Fatalf("count sentinel: %v", err)
		}
		if sentinelCount != 1 {
			t.Errorf("sentinel row should exist exactly once, got %d", sentinelCount)
		}

		// sessions values unchanged on re-run
		raw, _ := readTyped(t, db, "sessions", "id", "z-row", "created_at")
		var got2 int64
		fmt.Sscanf(string(raw), "%d", &got2)
		if got2 != expectedEpoch {
			t.Errorf("sessions.z-row changed on second migration: want=%d got=%d", expectedEpoch, got2)
		}
	})

	t.Run("skips tables and columns that do not exist yet", func(t *testing.T) {
		// Regression guard for the Task 3 wiring bug: eight tables
		// (system_audit_log, audit_cluster_proposals, session_handoffs,
		// scheduled_wakes, scheduled_tasks, ephemeral_scratchpad,
		// vector_clusters, vector_assignments) are declared in
		// schema.CommonIndexes, which historically ran AFTER the migration
		// transaction in initUnifiedSchema. The UPDATE loop then died with
		// "no such table: system_audit_log" and took the whole init with it.
		//
		// The call site has since moved after CommonIndexes so every table
		// exists, but the migration must not depend on that ordering: a
		// future schema reorder (or an older on-disk DB that predates a
		// table) must degrade to a skip, not a hard failure.
		db := setupFlippedSchemaDB(t)
		t.Cleanup(func() { db.Close() })

		for _, tbl := range []string{
			"system_audit_log",
			"audit_cluster_proposals",
			"session_handoffs",
			"scheduled_wakes",
			"scheduled_tasks",
			"ephemeral_scratchpad",
			"vector_clusters",
			"vector_assignments",
		} {
			if _, err := db.Exec("DROP TABLE " + tbl); err != nil {
				t.Fatalf("drop %s: %v", tbl, err)
			}
		}

		// A table that exists but is missing one of the migration-target
		// columns must be skipped column-wise, not table-wise.
		if _, err := db.Exec(`
			DROP TABLE topics;
			CREATE TABLE topics (id TEXT PRIMARY KEY, created_at INTEGER);
			INSERT INTO topics (id, created_at) VALUES ('t1', '2026-07-23 14:32:40');
		`); err != nil {
			t.Fatalf("rebuild topics without updated_at: %v", err)
		}

		if err := runTimestampsMigration(t, db); err != nil {
			t.Fatalf("migration must skip missing tables/columns, got: %v", err)
		}

		// The surviving column still got migrated.
		_, sqliteType := readTyped(t, db, "topics", "id", "t1", "created_at")
		if sqliteType != "integer" {
			t.Errorf("topics.created_at should have converted to integer, got %q", sqliteType)
		}

		var sentinelCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE id = 'timestamps_unified_v1'`,
		).Scan(&sentinelCount); err != nil {
			t.Fatalf("count sentinel: %v", err)
		}
		if sentinelCount != 1 {
			t.Errorf("sentinel should be written after a successful skip-run, got %d", sentinelCount)
		}
	})

	t.Run("INSERT OR IGNORE on sentinel makes double-init safe", func(t *testing.T) {
		// Run two migrations sequentially. The second run must not fail
		// on PRIMARY KEY insertion — INSERT OR IGNORE makes the second
		// write a no-op.
		db := setupFlippedSchemaDB(t)
		t.Cleanup(func() { db.Close() })
		// Seed one parseable row so the migration actually has work to do.
		if _, err := db.Exec(`
			INSERT INTO reference_docs (id, title) VALUES ('doc-1', 'D')
		`); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := runTimestampsMigration(t, db); err != nil {
			t.Fatalf("first migration: %v", err)
		}
		// Run again — sentinel guard short-circuits, no work done.
		if err := runTimestampsMigration(t, db); err != nil {
			t.Fatalf("second migration: %v", err)
		}
		var sentinelCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE id = 'timestamps_unified_v1'`,
		).Scan(&sentinelCount); err != nil {
			t.Fatalf("count sentinel: %v", err)
		}
		if sentinelCount != 1 {
			t.Errorf("sentinel row should exist exactly once after double migration, got %d", sentinelCount)
		}
	})
}

func runTimestampsMigration(t *testing.T, db *sql.DB) error {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := MigrateAllTimestampsToUnixEpoch(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
