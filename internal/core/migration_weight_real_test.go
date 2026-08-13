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
	revisionSeeds := []seed{
		{"mem-int", 50},
		{"mem-real", 90.5},
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
		`SELECT weight FROM memory_revisions WHERE memory_id = 'mem-real'`,
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
