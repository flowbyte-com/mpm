package internal

import (
	"database/sql"
	"sync/atomic"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// migrationTestDBCounter increments per call to setupMigrationTestDB, giving
// each caller a uniquely-named shared-cache in-memory database. Same
// rationale as recall_test.go: bare ":memory:" gives each pooled connection
// its own private DB and silently loses schema state.
var migrationTestDBCounter int64

// setupMigrationTestDB opens a fresh in-memory SQLite database with the
// minimum schema needed to exercise MigrateDeletedAtToUnixEpoch: a `memories`
// table with a `deleted_at` column, and a `schema_migrations` table for the
// sentinel guard. Returns the *sql.DB; caller is responsible for Close().
func setupMigrationTestDB(t *testing.T) *sql.DB {
	t.Helper()

	n := atomic.AddInt64(&migrationTestDBCounter, 1)
	dsn := "file:migration-deleted-at-test-" + testItoa(n) + "?mode=memory&cache=shared"

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dsn, err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS memories (
		id TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		deleted_at INTEGER
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

// itoa avoids pulling in strconv for a test-only helper.
func testItoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// seedDeletedAt inserts a row with a deliberately chosen deleted_at value
// of the given SQLite-stored type. Pass nil for SQL NULL.
func seedDeletedAt(t *testing.T, db *sql.DB, id string, value any) {
	t.Helper()
	var val any = value
	if val == nil {
		_, err := db.Exec(`INSERT INTO memories (id, content, deleted_at) VALUES (?, 'seed', NULL)`, id)
		if err != nil {
			t.Fatalf("seed %s (NULL): %v", id, err)
		}
		return
	}
	_, err := db.Exec(`INSERT INTO memories (id, content, deleted_at) VALUES (?, 'seed', ?)`, id, val)
	if err != nil {
		t.Fatalf("seed %s (%v): %v", id, val, err)
	}
}

// readDeletedAt returns the deleted_at value and SQLite type for a row.
func readDeletedAt(t *testing.T, db *sql.DB, id string) (value sql.NullInt64, sqliteType string) {
	t.Helper()
	err := db.QueryRow(`SELECT deleted_at, typeof(deleted_at) FROM memories WHERE id = ?`, id).Scan(&value, &sqliteType)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return
}

// TestMigrateDeletedAtToUnixEpoch covers the four behaviors that the audit
// flagged for the deleted_at / expires_at format unification:
//   1. TEXT rows are converted to INTEGER Unix epoch
//   2. INTEGER rows are left untouched (typeof guard works)
//   3. NULL rows are untouched
//   4. The migration is idempotent (sentinel guard works)
func TestMigrateDeletedAtToUnixEpoch(t *testing.T) {
	db := setupMigrationTestDB(t)
	t.Cleanup(func() { db.Close() })

	// Pre-seed three rows covering the three storage states a real
	// database could present to the migration. The "int-row" case is
	// critical: it proves the typeof(deleted_at) = 'text' guard in the
	// migration query correctly skips already-correct rows.
	seedDeletedAt(t, db, "text-row", "2026-07-23 14:32:40") // text format
	seedDeletedAt(t, db, "int-row",  int64(1721743500))     // already correct
	seedDeletedAt(t, db, "null-row", nil)                   // never deleted

	if err := runMigration(t, db); err != nil {
		t.Fatalf("first migration: %v", err)
	}

	// 1. NULL row: untouched
	nullVal, nullType := readDeletedAt(t, db, "null-row")
	if nullVal.Valid {
		t.Errorf("null-row should still be NULL, got %v (type=%s)", nullVal, nullType)
	}

	// 2. TEXT row: converted to integer. Expected value computed via the
	// same SQL the migration runs, so the assertion isn't tied to a
	// hardcoded epoch constant that could drift.
	var expectedEpoch int64
	if err := db.QueryRow(
		`SELECT CAST(strftime('%s', '2026-07-23 14:32:40') AS INTEGER)`,
	).Scan(&expectedEpoch); err != nil {
		t.Fatalf("compute expected epoch: %v", err)
	}
	textVal, textType := readDeletedAt(t, db, "text-row")
	if textType != "integer" {
		t.Errorf("text-row type should be integer after migration, got %s", textType)
	}
	if !textVal.Valid || textVal.Int64 != expectedEpoch {
		t.Errorf("text-row should be %d after migration, got valid=%v val=%d",
			expectedEpoch, textVal.Valid, textVal.Int64)
	}

	// 3. INTEGER row: untouched. This proves the typeof guard worked.
	intVal, intType := readDeletedAt(t, db, "int-row")
	if intType != "integer" {
		t.Errorf("int-row type should remain integer, got %s", intType)
	}
	if !intVal.Valid || intVal.Int64 != 1721743500 {
		t.Errorf("int-row should be unchanged at 1721743500, got valid=%v val=%d",
			intVal.Valid, intVal.Int64)
	}

	// 4. Idempotent: a second call must be a no-op. We verify by running
	// it again and asserting the values are unchanged and the sentinel
	// is still set exactly once.
	if err := runMigration(t, db); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	textVal2, _ := readDeletedAt(t, db, "text-row")
	if textVal2.Int64 != textVal.Int64 {
		t.Errorf("second migration changed text-row: was %d, now %d", textVal.Int64, textVal2.Int64)
	}
	var sentinelCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = 'deleted_at_unified_v1'`,
	).Scan(&sentinelCount); err != nil {
		t.Fatalf("count sentinel: %v", err)
	}
	if sentinelCount != 1 {
		t.Errorf("sentinel row should exist exactly once, got %d", sentinelCount)
	}
}

// runMigration opens a transaction and calls the migration under test.
func runMigration(t *testing.T, db *sql.DB) error {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := MigrateDeletedAtToUnixEpoch(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
