package internal

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// schemaDriftTestDBCounter gives each caller a uniquely-named shared-cache
// in-memory DB. Bare ":memory:" gives each pooled connection its own private
// DB and silently loses schema state — see recall_test.go for the same fix.
var schemaDriftTestDBCounter int64

// setupLastAccessedDriftDB opens a fresh in-memory DB with the
// minimum DDL required to exercise the memories.last_accessed_at
// migration: a `memories` table with INTEGER last_accessed_at (matching
// the production schema) and the `schema_migrations` sentinel table.
func setupLastAccessedDriftDB(t *testing.T) *sql.DB {
	t.Helper()

	n := atomic.AddInt64(&schemaDriftTestDBCounter, 1)
	dsn := "file:last-accessed-drift-" + testItoa(n) + "?mode=memory&cache=shared"

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dsn, err)
	}

	schema := `
	CREATE TABLE memories (
		id TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		created_at INTEGER,
		updated_at INTEGER,
		last_accessed_at INTEGER
	);
	CREATE TABLE schema_migrations (
		id TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		t.Fatalf("apply drift-test schema: %v", err)
	}
	return db
}

// runLastAccessedDriftMigration invokes the focused migration inside
// a transaction the way DatabaseManager.initUnifiedSchema does in
// production.
func runLastAccessedDriftMigration(t *testing.T, db *sql.DB) error {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := MigrateLastAccessedAtToUnixEpoch(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// TestLastAccessedAtDriftMigration_NormalizesTextToInteger is the
// primary regression guard for the column-type drift bug. It seeds a
// row whose last_accessed_at is a SQLite ISO TEXT timestamp (the
// value CURRENT_TIMESTAMP produces), runs the migration, and asserts:
//
//  1. The value is now stored under the "integer" storage class
//     (typeof() == 'integer') — so subsequent int64 scans succeed.
//  2. The value equals the expected Unix-epoch seconds for that
//     timestamp — so the conversion is correct, not just a
//     best-effort to_integer coercion.
//  3. Re-running the migration is a no-op (sentinel guard works).
//  4. NULL rows are not modified (the WHERE clause correctly
//     excludes them).
func TestLastAccessedAtDriftMigration_NormalizesTextToInteger(t *testing.T) {
	db := setupLastAccessedDriftDB(t)
	t.Cleanup(func() { db.Close() })

	// Seed three rows: one ISO TEXT, one Z-suffixed ISO TEXT (UTC
	// form), and one NULL. Each one is a representative
	// contamination from the legacy CURRENT_TIMESTAMP write paths.
	rows := []struct {
		id    string
		value interface{}
	}{
		{"iso-row", "2026-08-15 14:32:40"},
		{"z-row", "2026-08-15 14:32:40Z"},
		{"null-row", nil},
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO memories (id, content, last_accessed_at) VALUES (?, ?, ?)`,
			r.id, "seed "+r.id, r.value,
		); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}

	// Pre-migration sanity: ISO and Z rows are stored as TEXT.
	for _, r := range []string{"iso-row", "z-row"} {
		_, sqliteType := readTyped(t, db, "memories", "id", r, "last_accessed_at")
		if sqliteType != "text" {
			t.Fatalf("pre-migration: %s should be stored as text, got %q", r, sqliteType)
		}
	}

	if err := runLastAccessedDriftMigration(t, db); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// 1 & 2: ISO row converts to the correct Unix-epoch second.
	expected := time.Date(2026, 8, 15, 14, 32, 40, 0, time.UTC).Unix()
	for _, r := range []string{"iso-row", "z-row"} {
		raw, sqliteType := readTyped(t, db, "memories", "id", r, "last_accessed_at")
		if sqliteType != "integer" {
			t.Errorf("%s: storage class should be 'integer' after migration, got %q (raw=%q)",
				r, sqliteType, string(raw))
			continue
		}
		var got int64
		if _, err := fmt.Sscanf(string(raw), "%d", &got); err != nil {
			t.Errorf("%s: parse integer: %v", r, err)
			continue
		}
		if got != expected {
			t.Errorf("%s: expected unix epoch %d, got %d", r, expected, got)
		}
	}

	// 3: sentinel row written exactly once.
	var sentinelCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = 'last_accessed_at_drift_v1'`,
	).Scan(&sentinelCount); err != nil {
		t.Fatalf("count sentinel: %v", err)
	}
	if sentinelCount != 1 {
		t.Errorf("sentinel row should exist exactly once, got %d", sentinelCount)
	}

	// 4: NULL row untouched.
	raw, sqliteType := readTyped(t, db, "memories", "id", "null-row", "last_accessed_at")
	if sqliteType != "null" {
		t.Errorf("null-row: expected storage class 'null', got %q (raw=%q)", sqliteType, string(raw))
	}
	if len(raw) != 0 {
		t.Errorf("null-row: expected empty raw bytes, got %q", string(raw))
	}

	// 5: idempotent — re-running is a no-op, no error.
	if err := runLastAccessedDriftMigration(t, db); err != nil {
		t.Errorf("re-running migration should be a no-op, got: %v", err)
	}
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = 'last_accessed_at_drift_v1'`,
	).Scan(&sentinelCount); err != nil {
		t.Fatalf("count sentinel after re-run: %v", err)
	}
	if sentinelCount != 1 {
		t.Errorf("sentinel should still exist exactly once after re-run, got %d", sentinelCount)
	}
}

// TestLastAccessedAtDriftMigration_LeavesJunkUntouched ensures the
// strftime() guard protects rows whose TEXT value can't be parsed
// (e.g. a stray 'invalid' literal). Silently coercing those to NULL
// would mask data-quality bugs; leaving them in place forces the
// doctor / GC sweep to surface them.
func TestLastAccessedAtDriftMigration_LeavesJunkUntouched(t *testing.T) {
	db := setupLastAccessedDriftDB(t)
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(`
		INSERT INTO memories (id, content, last_accessed_at)
		VALUES ('junk-row', 'seed', 'not-a-timestamp')
	`); err != nil {
		t.Fatalf("seed junk-row: %v", err)
	}

	if err := runLastAccessedDriftMigration(t, db); err != nil {
		t.Fatalf("migration should succeed even with junk rows: %v", err)
	}

	// Junk row remains TEXT (unchanged) — the strftime() guard
	// returns NULL for unparseable values, so the WHERE clause
	// filters the row out and the UPDATE never touches it.
	raw, sqliteType := readTyped(t, db, "memories", "id", "junk-row", "last_accessed_at")
	if sqliteType != "text" {
		t.Errorf("junk-row: should remain 'text' (strftime guard skips unparseable), got %q (raw=%q)",
			sqliteType, string(raw))
	}
	if string(raw) != "not-a-timestamp" {
		t.Errorf("junk-row: expected unchanged value 'not-a-timestamp', got %q", string(raw))
	}
}

// integerTimestampColumns is the canonical set of columns on the
// `memories` table that are typed INTEGER and must therefore be
// written with CAST(strftime('%s','now') AS INTEGER) rather than
// CURRENT_TIMESTAMP. Mirrors the column-type declaration in
// schema.go and the SafeMigrations list.
var integerTimestampColumns = []string{
	"last_accessed_at",
	"created_at",
	"updated_at",
	"expires_at",
}

// currentTimestampWriteRe matches `last_accessed_at = CURRENT_TIMESTAMP`
// (or any other integer-timestamp column) anywhere in a Go source
// file. The static scan iterates every .go file under internal/core
// and asserts no production write path uses CURRENT_TIMESTAMP on an
// integer column — the regression guard against the column-type
// drift bug class.
//
// Match shape: <word_boundary> <col_name> <whitespace> = <whitespace>
// CURRENT_TIMESTAMP. The word boundary prevents the regex from
// matching inside identifiers (e.g. "old_last_accessed_at ="). The
// pattern fires for both the multi-line form (column name at start
// of line, after SET) and the inline form (column name in the
// middle of a longer SQL statement).
var currentTimestampWriteRe = regexp.MustCompile(
	`\b([a-zA-Z_][a-zA-Z0-9_]*)\s*=\s*CURRENT_TIMESTAMP\b`,
)

// TestNoCurrentTimestampWritesToIntegerColumns statically scans every
// production .go file under internal/core/ and asserts that no write
// statement assigns CURRENT_TIMESTAMP to a column typed as INTEGER
// in the memories table. This is the structural guard against the
// column-type drift bug class — when a future contributor adds a
// new write path, this test fails before the bug ships.
//
// Out of scope (intentionally not scanned):
//   - Files under internal/core/ that target the shared DB
//     (broadcast.go, contradiction) — those tables declare TEXT
//     timestamps by design and CURRENT_TIMESTAMP is correct there.
//   - Test files (_test.go) — they're allowed to seed TEXT data
//     intentionally to exercise the migration.
//
// If the test is too restrictive, narrow the file list rather than
// relaxing the regex: every integer timestamp write in production
// MUST use CAST(strftime('%s','now') AS INTEGER).
func TestNoCurrentTimestampWritesToIntegerColumns(t *testing.T) {
	// Files where shared-DB (TEXT) writes legitimately use
	// CURRENT_TIMESTAMP. Excluded from the scan so the test
	// only fires on integer-column writes.
	sharedTextFiles := map[string]bool{
		"broadcast.go":               true,
		"broadcast_schema_patch.go":  true,
		"contradiction_schema_patch.go": true,
	}

	// Walk internal/core/ for .go files. Skip _test.go and shared-DB
	// writers.
	//
	// The test runs from internal/core/ (this package's module), so
	// the parent directory holds the project root and the target
	// directory is reachable as ../internal/core — except the
	// internal/core directory IS the current package, so we walk it
	// directly. We use the test source file's location as the
	// authoritative reference and resolve the package dir from it.
	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	var offenders []string

	entries, err := os.ReadDir(packageDir)
	if err != nil {
		t.Fatalf("read %s: %v", packageDir, err)
	}

	integerCols := make(map[string]bool, len(integerTimestampColumns))
	for _, c := range integerTimestampColumns {
		integerCols[c] = true
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if sharedTextFiles[name] {
			continue
		}

		path := filepath.Join(packageDir, name)
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		scanner := bufio.NewScanner(f)
		// Long lines (some SQL strings exceed the default 64K cap).
		scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)

		for scanner.Scan() {
			line := scanner.Text()
			// Skip comment lines so the regex doesn't fire on
			// documentation that mentions CURRENT_TIMESTAMP
			// in the explanatory text.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") {
				continue
			}
			m := currentTimestampWriteRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			col := strings.TrimSpace(m[1])
			if !integerCols[col] {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s: %s", name, strings.TrimSpace(line)))
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			t.Fatalf("scan %s: %v", path, err)
		}
	}

	if len(offenders) > 0 {
		t.Fatalf(
			"production code assigns CURRENT_TIMESTAMP to an INTEGER timestamp column "+
				"in %d place(s). Use CAST(strftime('%%s','now') AS INTEGER) instead:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "),
		)
	}
}

// createTableDatetimeRe matches any inline column declaration that
// types a (presumed integer-timestamp) column as DATETIME or DATE.
// Catches both the multi-line form (column at start of line, in a
// CREATE TABLE body) and the inline form (column mid-line in a
// longer DDL string).
//
// Match shape: <word_boundary> <col_name> <whitespace> <DATETIME|DATE>
// The word boundary prevents the regex from matching inside
// identifiers (e.g. "old_created_at DATETIME" is not a violation;
// "created_at DATETIME" is).
var createTableDatetimeRe = regexp.MustCompile(
	`(?i)\b([a-zA-Z_][a-zA-Z0-9_]*)\s+(DATETIME|DATE)\b`,
)

// TestNoCreateTableDatetimeForIntegerColumns is the static-scan
// CREATE-side companion to TestNoCurrentTimestampWritesToIntegerColumns.
// Together they form the full guard for the schema-timestamp-class
// problem:
//
//   - TestNoCurrentTimestampWritesToIntegerColumns prevents new
//     drift by failing the build if any write path assigns
//     CURRENT_TIMESTAMP to an INTEGER timestamp column.
//
//   - TestNoCreateTableDatetimeForIntegerColumns (this test) prevents
//     legacy-shape reintroduction by failing the build if any
//     CREATE TABLE declaration types an integer-timestamp column as
//     DATETIME or DATE.
//
// The shipped migration (migration_memories_affinity_rebuild.go)
// is the cleanup for legacy on-disk databases that already have
// such columns. This test ensures future contributors don't re-
// introduce the drift class at the DDL layer.
//
// Out of scope (intentionally not scanned):
//   - Files under internal/core/ that legitimately declare DATETIME
//     columns (broadcast_schema_patch.go — shared.bcast_* uses
//     DATETIME by design; legacy_weight view — historical)
//   - Test files (_test.go) — allowed to seed TEXT/DATETIME data
//     intentionally to exercise the migration.
func TestNoCreateTableDatetimeForIntegerColumns(t *testing.T) {
	// Files where DATETIME-shape declarations are legitimate.
	legitimateTextFiles := map[string]bool{
		"broadcast.go":                  true,
		"broadcast_schema_patch.go":     true,
		"contradiction_schema_patch.go": true,
		"migration_memories_affinity_rebuild.go": true, // references DATETIME in comments / legacy-shape fixtures
	}

	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	integerCols := make(map[string]bool, len(integerTimestampColumns))
	for _, c := range integerTimestampColumns {
		integerCols[c] = true
	}
	// Also include deleted_at — part of the rebuild target list,
	// should be INTEGER in the canonical schema.
	integerCols["deleted_at"] = true

	entries, err := os.ReadDir(packageDir)
	if err != nil {
		t.Fatalf("read %s: %v", packageDir, err)
	}

	var offenders []string

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if legitimateTextFiles[name] {
			continue
		}

		path := filepath.Join(packageDir, name)
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)

		// We only fire when the line appears inside a CREATE TABLE
		// block. Track that with a depth counter that opens on
		// `CREATE TABLE` and closes on the matching `)`. Simpler
		// approach: just look for lines that mention both
		// `CREATE TABLE` and one of the integer columns in the
		// preceding 20 lines. Even simpler: trigger on the column
		// declaration itself regardless of context, and let the
		// file-exclusion list carry the precision.
		//
		// The looser approach (column declaration only) is what's
		// used here. False positives are caught by the file list.
		for scanner.Scan() {
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") {
				continue
			}
			m := createTableDatetimeRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			col := strings.TrimSpace(m[1])
			if !integerCols[col] {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s: %s", name, strings.TrimSpace(line)))
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			t.Fatalf("scan %s: %v", path, err)
		}
	}

	if len(offenders) > 0 {
		t.Fatalf(
			"production code declares an integer-timestamp column as DATETIME/DATE "+
				"in %d place(s). Use INTEGER instead:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "),
		)
	}
}
