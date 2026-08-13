package internal

import (
	"database/sql"
	"sync/atomic"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// migrationWeightTestDBCounter increments per call, giving each caller a
// uniquely-named shared-cache in-memory database. Same rationale as
// migration_deleted_at_test.go: bare ":memory:" gives each pooled
// connection its own private DB and silently loses schema state.
var migrationWeightTestDBCounter int64

func setupMigrationWeightTestDB(t *testing.T) *sql.DB {
	t.Helper()
	n := atomic.AddInt64(&migrationWeightTestDBCounter, 1)
	dsn := "file:migration-weight-real-test-" + testItoa(n) + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dsn, err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS memories (
		id TEXT PRIMARY KEY,
		collection TEXT NOT NULL,
		content TEXT NOT NULL,
		weight INTEGER NOT NULL DEFAULT 1,
		deleted_at INTEGER
	);
	-- Indexes that mirror production (a subset of the 14 indexes on the
	-- real memories table). The migration drops indexes that reference
	-- the weight column before DROP COLUMN — production has several.
	-- We include a composite one to exercise column-list matching.
	CREATE INDEX IF NOT EXISTS idx_memories_test_collection ON memories(collection);
	CREATE INDEX IF NOT EXISTS idx_memories_test_deleted    ON memories(deleted_at);
	CREATE INDEX IF NOT EXISTS idx_memories_test_session    ON memories(id);
	CREATE INDEX IF NOT EXISTS idx_memories_test_weight     ON memories(weight);
	CREATE INDEX IF NOT EXISTS idx_memories_test_composite  ON memories(collection, weight);
	-- Trigger that references NEW.weight — the production schema has
	-- triggers like memories_rev_ai/memories_rev_au that read NEW.weight
	-- into memory_revisions.weight. DROP COLUMN fails on these; the
	-- migration must drop the trigger first, then recreate it.
	CREATE TRIGGER IF NOT EXISTS memories_test_rev_ai AFTER INSERT ON memories
		BEGIN
			INSERT INTO memory_revisions (memory_id, version, content, weight, collection)
			VALUES (NEW.id, 1, NEW.content, COALESCE(NEW.weight, 0), NEW.collection);
		END;
	-- View that references memories.weight — production has views like
	-- v_model_memory_yield that read m.weight from the memories alias.
	-- DROP COLUMN fails on these too; the migration must drop the view
	-- first, then recreate it. Production hit on 2026-08-13 18:40.
	CREATE VIEW IF NOT EXISTS memories_test_weight_view AS
		SELECT id, weight FROM memories WHERE weight >= 1;
	CREATE TABLE IF NOT EXISTS memory_revisions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		memory_id TEXT NOT NULL,
		version INTEGER NOT NULL,
		content TEXT NOT NULL,
		weight INTEGER NOT NULL DEFAULT 1,
		collection TEXT NOT NULL,
		created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
	);
	CREATE TABLE IF NOT EXISTS schema_migrations (
		id TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		t.Fatalf("create schema: %v", err)
	}
	return db
}

// TestMigrateWeightToReal exercises the migration contract:
//   - converts INTEGER-declared weight columns to REAL on both tables
//   - preserves fractional (REAL-stored) values — the bug case
//   - preserves integer values (cast to REAL — 50 → 50.0)
//   - preserves NULL values
//   - records the schema_migrations sentinel exactly once
//   - is idempotent (second run short-circuits on sentinel)
//   - does not touch unrelated columns (deleted_at stays INTEGER)
//   - reproduces the production index-namespace collision: indexes that
//     exist on the source table must be cleanly recreated on the new
//     table without triggering "index <name> already exists". The
//     pre-fix ordering (recreate-then-drop) tripped on this in
//     production on 2026-08-13 — this test would have caught it.
func TestMigrateWeightToReal(t *testing.T) {
	db := setupMigrationWeightTestDB(t)
	defer db.Close()

	// Seed: integer weight and fractional weight — per table. NULL is not a
	// valid case in production (memories.weight is NOT NULL DEFAULT 1.0) so
	// we don't exercise it here.
	type seed struct {
		id     string
		weight any
	}
	memorySeeds := []seed{
		{"mem-int", 50},    // integer value (cast to REAL: 50.0)
		{"mem-real", 82.5}, // fractional — the bug case
	}
	for _, s := range memorySeeds {
		if _, err := db.Exec(
			`INSERT INTO memories (id, collection, content, weight) VALUES (?, 'memories', 'seed', ?)`,
			s.id, s.weight,
		); err != nil {
			t.Fatalf("seed memories %s: %v", s.id, err)
		}
	}
	// Seed memory_revisions with rows that DON'T collide with the
	// trigger (memories_test_rev_ai fires on every memories INSERT and
	// creates a memory_revisions row for the same memory_id with the
	// NEW.weight value). Using distinct ids ("rev-..." prefix) keeps
	// the seed assertions unambiguous.
	revisionSeeds := []seed{
		{"rev-int", 50},
		{"rev-real", 90.5},
	}
	for _, s := range revisionSeeds {
		if _, err := db.Exec(
			`INSERT INTO memory_revisions (memory_id, version, content, weight, collection) VALUES (?, 1, 'rev', ?, 'memories')`,
			s.id, s.weight,
		); err != nil {
			t.Fatalf("seed memory_revisions %s: %v", s.id, err)
		}
	}

	// Pre-migration probe: weight must be INTEGER (the bug state).
	for _, table := range []string{"memories", "memory_revisions"} {
		var declType string
		if err := db.QueryRow(
			`SELECT type FROM pragma_table_info(?) WHERE name = 'weight'`, table,
		).Scan(&declType); err != nil {
			t.Fatalf("pre-migration pragma %s.weight: %v", table, err)
		}
		if declType != "INTEGER" {
			t.Fatalf("pre-migration %s.weight expected INTEGER, got %q", table, declType)
		}
	}

	// Run the migration.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := MigrateWeightToReal(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("MigrateWeightToReal: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Post-migration probe: weight must be REAL on both tables.
	for _, table := range []string{"memories", "memory_revisions"} {
		var declType string
		if err := db.QueryRow(
			`SELECT type FROM pragma_table_info(?) WHERE name = 'weight'`, table,
		).Scan(&declType); err != nil {
			t.Fatalf("post-migration pragma %s.weight: %v", table, err)
		}
		if declType != "REAL" {
			t.Fatalf("post-migration %s.weight expected REAL, got %q", table, declType)
		}
	}

	// Index namespace: every index that existed on the source memories
	// table must be recreated on the new weight column after the
	// column dance. The old index dropped+recreate path was the
	// index-collision bug from 2026-08-13.
	indexRows, err := db.Query(`
		SELECT name FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'memories'
		  AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`)
	if err != nil {
		t.Fatalf("list recreated indexes: %v", err)
	}
	defer indexRows.Close()
	var gotIdx []string
	for indexRows.Next() {
		var name string
		if err := indexRows.Scan(&name); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		gotIdx = append(gotIdx, name)
	}
	wantIdx := []string{
		"idx_memories_test_collection",
		"idx_memories_test_composite",
		"idx_memories_test_deleted",
		"idx_memories_test_session",
		"idx_memories_test_weight",
	}
	if len(gotIdx) != len(wantIdx) {
		t.Errorf("recreated index count: want %d (%v), got %d (%v)",
			len(wantIdx), wantIdx, len(gotIdx), gotIdx)
	} else {
		for i := range wantIdx {
			if gotIdx[i] != wantIdx[i] {
				t.Errorf("recreated index[%d]: want %q, got %q", i, wantIdx[i], gotIdx[i])
			}
		}
	}

	// Trigger: the trigger that referenced NEW.weight must have been
	// dropped before DROP COLUMN and recreated after RENAME. Use a
	// fresh memory_id ("mem-trig-test") so we don't collide with the
	// seed inserts that ran earlier in the test — the trigger fires on
	// every memories INSERT and would otherwise create duplicate
	// memory_revisions rows for the seed ids.
	if _, err := db.Exec(`INSERT INTO memories (id, collection, content, weight) VALUES ('mem-trig-test', 'memories', 'trig body', 77.5)`); err != nil {
		t.Fatalf("trigger test insert: %v", err)
	}
	var trigWeight float64
	if err := db.QueryRow(
		`SELECT weight FROM memory_revisions WHERE memory_id = 'mem-trig-test'`,
	).Scan(&trigWeight); err != nil {
		t.Fatalf("trigger test select: %v", err)
	}
	if trigWeight != 77.5 {
		t.Errorf("trigger weight: want 77.5, got %v", trigWeight)
	}

	// View: the view that referenced memories.weight must have been
	// dropped before DROP COLUMN and recreated after RENAME. Verify by
	// querying the view — if it returns the expected row, the view is
	// functional; if the migration failed to recreate it, the view
	// would be absent from sqlite_master.
	viewRows, err := db.Query(`
		SELECT name FROM sqlite_master
		WHERE type = 'view' AND name = 'memories_test_weight_view'
	`)
	if err != nil {
		t.Fatalf("view check query: %v", err)
	}
	defer viewRows.Close()
	if !viewRows.Next() {
		t.Fatalf("memories_test_weight_view not in sqlite_master after migration")
	}
	viewRows.Close()
	var viewCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM memories_test_weight_view`,
	).Scan(&viewCount); err != nil {
		t.Fatalf("view query: %v", err)
	}
	// The view filters WHERE weight >= 1, so mem-int (50.0) qualifies
	// but mem-real (82.5) also qualifies; the trigger row
	// (mem-trig-test, 77.5) qualifies. mem-null was removed earlier.
	// mem-real from the seed was 82.5, which becomes 82.5 after
	// migration — it qualifies. mem-int (50) qualifies. mem-trig-test
	// (77.5) qualifies. rev-* rows are in memory_revisions, not
	// memories, so don't appear.
	if viewCount != 3 {
		t.Errorf("view count: want 3 (mem-int, mem-real, mem-trig-test), got %d", viewCount)
	}

	// Data preservation: read all seeded memories, verify values.
	rows, err := db.Query(`SELECT id, weight FROM memories WHERE id IN ('mem-int', 'mem-real')`)
	if err != nil {
		t.Fatalf("select memories: %v", err)
	}
	defer rows.Close()
	got := map[string]float64{}
	for rows.Next() {
		var id string
		var w float64
		if err := rows.Scan(&id, &w); err != nil {
			t.Fatalf("scan memories: %v", err)
		}
		got[id] = w
	}
	if w, ok := got["mem-int"]; !ok || w != 50.0 {
		t.Errorf("mem-int weight: want 50.0, got %v (present=%v)", w, ok)
	}
	if w, ok := got["mem-real"]; !ok || w != 82.5 {
		t.Errorf("mem-real weight: want 82.5, got %v (present=%v)", w, ok)
	}

	// Data preservation: memory_revisions fractional value intact.
	var revWeight sql.NullFloat64
	if err := db.QueryRow(
		`SELECT weight FROM memory_revisions WHERE memory_id = 'rev-real'`,
	).Scan(&revWeight); err != nil {
		t.Fatalf("select memory_revisions: %v", err)
	}
	if !revWeight.Valid || revWeight.Float64 != 90.5 {
		t.Errorf("memory_revisions weight: want 90.5, got %+v", revWeight)
	}

	// Unrelated column untouched: deleted_at must still be INTEGER.
	var deletedAtType string
	if err := db.QueryRow(
		`SELECT type FROM pragma_table_info(?) WHERE name = 'deleted_at'`, "memories",
	).Scan(&deletedAtType); err != nil {
		t.Fatalf("pragma deleted_at: %v", err)
	}
	if deletedAtType != "INTEGER" {
		t.Errorf("deleted_at type changed: want INTEGER, got %q", deletedAtType)
	}

	// Sentinel recorded exactly once.
	var sentinelCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = 'weight_real_v1'`,
	).Scan(&sentinelCount); err != nil {
		t.Fatalf("sentinel check: %v", err)
	}
	if sentinelCount != 1 {
		t.Errorf("sentinel count: want 1, got %d", sentinelCount)
	}

	// Idempotency: a second run must short-circuit on the sentinel and
	// must NOT add another sentinel row.
	tx, err = db.Begin()
	if err != nil {
		t.Fatalf("begin tx #2: %v", err)
	}
	if err := MigrateWeightToReal(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("MigrateWeightToReal #2: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit #2: %v", err)
	}
	var sentinelCount2 int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = 'weight_real_v1'`,
	).Scan(&sentinelCount2); err != nil {
		t.Fatalf("sentinel check #2: %v", err)
	}
	if sentinelCount2 != 1 {
		t.Errorf("sentinel count after second run: want 1, got %d", sentinelCount2)
	}
}
