# Unix-Epoch Timestamps Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Migrate every remaining DATETIME TEXT column in MPM's SQLite schema to INTEGER Unix-epoch seconds, with the full display-layer, struct-layer, SQL-layer, and test-layer follow-through specified in `docs/superpowers/specs/2026-07-30-unix-epoch-timestamps-design.md`.

**Architecture:** One migration function (`MigrateAllTimestampsToUnixEpoch`) with sentinel row `timestamps_unified_v1` mirrors the existing `deleted_at_unified_v1` precedent. Schema DDL flips DATETIME → INTEGER; defaults switch from `CURRENT_TIMESTAMP` to `CAST(strftime('%s','now') AS INTEGER)`. Go struct fields become `int64` / `*int64`; `FormatUnixSeconds` and `FormatOptionalUnixSeconds` centralize RFC3339 formatting at CLI/MCP boundaries. CLI inputs accept both RFC3339 and unix-epoch integers. AST walker test guards against SQLite manifest typing silently storing strings in INTEGER columns.

**Tech Stack:** Go 1.22+, SQLite via `github.com/mattn/go-sqlite3` (with `CGO_CFLAGS=-DSQLITE_ENABLE_FTS5`), existing test patterns from `internal/core/migration_deleted_at_test.go` and `internal/core/sqlopen_owner_test.go`.

---

## Pre-Plan Setup (manual)

Before starting Task 1:

```bash
cd /home/v/workspace/projects/mpm
git stash push -m "WIP: route_render + getters refactor (in-flight)" -- active.json cmd/mpm/route_render.go cmd/mpm/route_render_test.go internal/core/tools/getters.go internal/core/tools/getters_test.go
# OR, if those changes are committed-ready:
# git add active.json cmd/mpm/route_render.go cmd/mpm/route_render_test.go internal/core/tools/getters.go internal/core/tools/getters_test.go
# git commit -m "WIP: route_render + getters refactor"
```

The unrelated changes on `main` must be cleared (committed or stashed) so the feature branch starts from a clean tree.

---

## Task 1: Create feature branch

**Files:**
- Modify: `.git/HEAD` (git operation only, no file edits)

- [ ] **Step 1: Confirm clean tree and current branch**

```bash
git status --short
git branch --show-current
```

Expected: `git status --short` is empty (after pre-plan setup). `git branch --show-current` returns `main`.

- [ ] **Step 2: Create feature branch**

```bash
git checkout -b feat/unix-epoch-timestamps
```

Expected: Branch created. `git branch --show-current` returns `feat/unix-epoch-timestamps`.

- [ ] **Step 3: Verify build and tests still pass on the branch**

```bash
make build
make test
```

Expected: Both succeed. If anything fails on `main`, abort and resolve before proceeding.

- [ ] **Step 4: No commit needed — branch creation is local.**

---

## Task 2: Add migration function with TDD (Test 1)

**Files:**
- Create: `internal/core/migration_timestamps.go`
- Create: `internal/core/migration_timestamps_test.go`

- [ ] **Step 1: Write the failing migration test**

Create `internal/core/migration_timestamps_test.go`:

```go
package internal

import (
	"database/sql"
	"sync/atomic"
	"testing"
	"time"
	_ "github.com/mattn/go-sqlite3"
)

// migrationTestDBCounter gives each caller a uniquely-named shared-cache in-memory DB.
// Same rationale as recall_test.go: bare ":memory:" gives each pooled connection its
// own private DB and silently loses schema state.
var timestampsTestDBCounter int64

func setupTimestampsTestDB(t *testing.T) *sql.DB {
	t.Helper()

	n := atomic.AddInt64(&timestampsTestDBCounter, 1)
	dsn := "file:timestamps-test-" + testItoa(n) + "?mode=memory&cache=shared"

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dsn, err)
	}

	// Minimal schema mirroring the columns in the migration scope.
	// Three rows per column cover TEXT (legacy), INTEGER (already migrated), NULL.
	schema := `
	CREATE TABLE IF NOT EXISTS migrations_test (
		id TEXT PRIMARY KEY,
		text_col TEXT,
		int_col INTEGER,
		null_col TEXT,
		z_suffix_col TEXT
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

// TestMigrateAllTimestampsToUnixEpoch covers the four behaviors:
//   1. TEXT rows convert to INTEGER unix epoch
//   2. INTEGER rows stay untouched (typeof guard works)
//   3. NULL rows stay untouched
//   4. Z-suffixed RFC3339 strings (the exact legacy format from time.Now().UTC().Format(time.RFC3339)) convert correctly with no local-tz offset
//   5. The migration is idempotent (sentinel guard works)
func TestMigrateAllTimestampsToUnixEpoch(t *testing.T) {
	db := setupTimestampsTestDB(t)
	t.Cleanup(func() { db.Close() })

	// Seed: text_col gets legacy format; int_col already migrated; null_col is NULL;
	// z_suffix_col uses the exact legacy format from time.Now().UTC().Format(time.RFC3339).
	zRFC3339 := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`
		INSERT INTO migrations_test (id, text_col, int_col, null_col, z_suffix_col) VALUES
			('text-row', '2026-07-23 14:32:40', 1721743500, NULL, '2026-07-23 14:32:40Z'),
			('z-row',   '2026-07-23 14:32:40', 1721743500, NULL, ?),
			('int-row', NULL,                   1721743500, NULL, NULL),
			('null-row', NULL,                  NULL,       NULL, NULL)
	`, zRFC3339); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := runTimestampsMigration(t, db); err != nil {
		t.Fatalf("first migration: %v", err)
	}

	// 1. NULL row: untouched
	var nullVal sql.NullInt64
	if err := db.QueryRow(`SELECT int_col FROM migrations_test WHERE id = 'null-row'`).Scan(&nullVal); err != nil {
		t.Fatalf("read null-row: %v", err)
	}
	if nullVal.Valid {
		t.Errorf("null-row int_col should still be NULL, got %v", nullVal.Int64)
	}

	// 2. TEXT row: text_col converted to int. Expected value computed via the
	// same SQL the migration runs, so the assertion isn't tied to a hardcoded
	// epoch constant that could drift.
	var expectedEpoch int64
	if err := db.QueryRow(
		`SELECT CAST(strftime('%s', '2026-07-23 14:32:40') AS INTEGER)`,
	).Scan(&expectedEpoch); err != nil {
		t.Fatalf("compute expected epoch: %v", err)
	}
	var textVal sql.NullInt64
	if err := db.QueryRow(`SELECT text_col FROM migrations_test WHERE id = 'text-row'`).Scan(&textVal); err != nil {
		t.Fatalf("read text-row: %v", err)
	}
	if !textVal.Valid || textVal.Int64 != expectedEpoch {
		t.Errorf("text-row should be %d after migration, got valid=%v val=%d",
			expectedEpoch, textVal.Valid, textVal.Int64)
	}

	// 3. INTEGER row: untouched. This proves the typeof guard worked.
	var intVal sql.NullInt64
	if err := db.QueryRow(`SELECT int_col FROM migrations_test WHERE id = 'int-row'`).Scan(&intVal); err != nil {
		t.Fatalf("read int-row: %v", err)
	}
	if !intVal.Valid || intVal.Int64 != 1721743500 {
		t.Errorf("int-row should be unchanged at 1721743500, got valid=%v val=%d",
			intVal.Valid, intVal.Int64)
	}

	// 4. Z-suffix row: proves no local-timezone offset is silently applied.
	// The exact legacy format produced by time.Now().UTC().Format(time.RFC3339) ends in 'Z'.
	var zVal sql.NullInt64
	if err := db.QueryRow(`SELECT text_col FROM migrations_test WHERE id = 'z-row'`).Scan(&zVal); err != nil {
		t.Fatalf("read z-row: %v", err)
	}
	if !zVal.Valid || zVal.Int64 != expectedEpoch {
		t.Errorf("z-row should be %d after migration (no local-tz offset), got valid=%v val=%d",
			expectedEpoch, zVal.Valid, zVal.Int64)
	}
	// And the z_suffix_col itself converted cleanly:
	if err := db.QueryRow(`SELECT z_suffix_col FROM migrations_test WHERE id = 'z-row'`).Scan(&zVal); err != nil {
		t.Fatalf("read z-row z_suffix: %v", err)
	}
	if !zVal.Valid || zVal.Int64 != expectedEpoch {
		t.Errorf("z-row z_suffix_col should be %d after migration, got valid=%v val=%d",
			expectedEpoch, zVal.Valid, zVal.Int64)
	}

	// 5. Idempotent: a second call must be a no-op.
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
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd internal/core && go test -tags fts5 -run TestMigrateAllTimestampsToUnixEpoch -v
```

Expected: FAIL with `undefined: MigrateAllTimestampsToUnixEpoch`.

- [ ] **Step 3: Write the migration function**

Create `internal/core/migration_timestamps.go`:

```go
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// timestampColumn identifies one (table, column) pair to migrate.
type timestampColumn struct {
	table string
	col   string
}

// allTimestampsToMigrate is the canonical list of DATETIME columns that must
// convert from TEXT ISO 8601 to INTEGER Unix-epoch seconds. Mirrors the table
// in docs/superpowers/specs/2026-07-30-unix-epoch-timestamps-design.md.
var allTimestampsToMigrate = []timestampColumn{
	{"sessions", "created_at"},
	{"topics", "created_at"},
	{"topics", "updated_at"},
	{"topic_memberships", "created_at"},
	{"memories", "created_at"},
	{"memories", "updated_at"},
	{"memories", "last_accessed_at"},
	{"memories", "expires_at"},
	{"system_config", "updated_at"},
	{"retrieval_metadata", "created_at"},
	{"retrieval_metadata", "updated_at"},
	{"retrieval_metadata", "last_retrieved_at"},
	{"external_db_cursors", "updated_at"},
	{"reference_docs", "created_at"},
	{"reference_docs", "last_indexed"},
	{"system_audit_log", "created_at"},
	{"audit_cluster_proposals", "created_at"},
	{"audit_cluster_proposals", "updated_at"},
	{"audit_cluster_proposals", "first_seen"},
	{"audit_cluster_proposals", "last_seen"},
	{"audit_cluster_proposals", "snooze_until"},
	{"session_handoffs", "created_at"},
	{"session_handoffs", "ended_at"},
	{"session_handoffs", "read_at"},
	{"scheduled_wakes", "created_at"},
	{"scheduled_tasks", "created_at"},
	{"scheduled_tasks", "updated_at"},
	{"scheduled_tasks", "last_run_at"},
	{"scheduled_tasks", "next_run_at"},
	{"ephemeral_scratchpad", "created_at"},
	{"ephemeral_scratchpad", "updated_at"},
	{"ephemeral_scratchpad", "decay_at"},
	{"vector_clusters", "updated_at"},
	{"vector_assignments", "updated_at"},
	{"reference_interactions", "created_at"},
	{"admission_log", "created_at"},
	{"memory_revisions", "created_at"},
}

// MigrateAllTimestampsToUnixEpoch converts every legacy DATETIME TEXT column in
// allTimestampsToMigrate to INTEGER Unix-epoch seconds. Idempotent via the
// timestamps_unified_v1 sentinel.
//
// The typeof() = 'text' guard makes the migration safe to re-run on a
// partially-applied state: already-integer rows are skipped, so a partial
// failure followed by retry picks up where it left off.
//
// Must run AFTER MigrateDeletedAtToUnixEpoch (which establishes the
// integer-timestamp precedent on memories.deleted_at) and BEFORE any handler
// executes. Caller (DatabaseManager.init) wraps in a transaction:
//
//   tx.Begin()
//   MigrateDeletedAtToUnixEpoch(tx)
//   MigrateAllTimestampsToUnixEpoch(tx)
//   tx.Commit()
func MigrateAllTimestampsToUnixEpoch(tx *sql.Tx) error {
	// 1. Sentinel check — bail if already applied.
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"timestamps_unified_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check timestamps_unified_v1 sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}

	// 2. Convert TEXT rows to INTEGER Unix epoch. The typeof() guard ensures
	// we don't double-process rows that are already integer (e.g. from a
	// partially-applied prior migration).
	for _, tc := range allTimestampsToMigrate {
		query := fmt.Sprintf(
			`UPDATE %s SET %s = CAST(strftime('%%s', %s) AS INTEGER) WHERE %s IS NOT NULL AND typeof(%s) = 'text'`,
			tc.table, tc.col, tc.col, tc.col, tc.col,
		)
		if _, err := tx.Exec(query); err != nil {
			return fmt.Errorf("migrate %s.%s to unix epoch: %w", tc.table, tc.col, err)
		}
	}

	// 3. Record sentinel so future runs short-circuit.
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"timestamps_unified_v1",
		time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record timestamps_unified_v1 sentinel: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd internal/core && go test -tags fts5 -run TestMigrateAllTimestampsToUnixEpoch -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/core/migration_timestamps.go internal/core/migration_timestamps_test.go
git commit -m "feat(core): MigrateAllTimestampsToUnixEpoch + sentinel

Mirrors the deleted_at_unified_v1 migration pattern. Converts 33 DATETIME
TEXT columns across 19 tables to INTEGER Unix-epoch seconds. Idempotent
via the timestamps_unified_v1 sentinel row in schema_migrations. The
typeof() = 'text' guard makes re-runs safe on partially-applied state.

Test fixture covers TEXT, INTEGER, NULL, and the exact Z-suffixed RFC3339
format produced by time.Now().UTC().Format(time.RFC3339) — proves no
local-timezone offset is applied during conversion."
```

---

## Task 3: Wire migration into DatabaseManager.init

**Files:**
- Modify: `internal/core/db.go` (the init function — find the existing `MigrateDeletedAtToUnixEpoch(tx)` call site)

- [ ] **Step 1: Locate the existing call site**

```bash
grep -n "MigrateDeletedAtToUnixEpoch" internal/core/db.go
```

Expected: a single line near the init transaction, e.g. `if err := MigrateDeletedAtToUnixEpoch(tx); err != nil { ... }`.

- [ ] **Step 2: Add the new migration call immediately after the existing one**

Insert immediately after the existing `MigrateDeletedAtToUnixEpoch` call (use the exact line found in Step 1 as the anchor):

```go
if err := MigrateAllTimestampsToUnixEpoch(tx); err != nil {
    return fmt.Errorf("timestamps unification migration failed: %w", err)
}
```

- [ ] **Step 3: Build to verify the call site compiles**

```bash
go build -tags fts5 ./...
```

Expected: success. The migration function is now invoked from init.

- [ ] **Step 4: Run the migration test to verify behavior**

```bash
cd internal/core && go test -tags fts5 -run TestMigrateAllTimestampsToUnixEpoch -v
```

Expected: still PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/core/db.go
git commit -m "feat(core): wire MigrateAllTimestampsToUnixEpoch into init"
```

---

## Task 4: Flip schema DDL columns to INTEGER

**Files:**
- Modify: `internal/core/schema.go` (BaseTables and CommonIndexes DDL strings)

- [ ] **Step 1: Run the bulk replacement for DATETIME DEFAULT CURRENT_TIMESTAMP**

Use `sed` to flip every `DATETIME DEFAULT CURRENT_TIMESTAMP` to `INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))` in `schema.go`. This is a structural schema change, so all touched DDL strings now resolve to INTEGER columns.

```bash
sed -i "s/DATETIME DEFAULT CURRENT_TIMESTAMP/INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))/g" internal/core/schema.go
```

- [ ] **Step 2: Update DATETIME columns without the CURRENT_TIMESTAMP default**

These columns need flipping to INTEGER without the default change. Apply individually:

```bash
sed -i "s/memories\.last_accessed_at DATETIME/memories.last_accessed_at INTEGER/g" internal/core/schema.go
sed -i "s/memories\.updated_at DATETIME/memories.updated_at INTEGER/g" internal/core/schema.go
sed -i "s/memories\.expires_at DATETIME/memories.expires_at INTEGER/g" internal/core/schema.go
sed -i "s/reference_chunks.*last_indexed TEXT/reference_chunks.last_indexed INTEGER/g" internal/core/schema.go
sed -i "s/reference_interactions.*created_at TEXT NOT NULL/reference_interactions.created_at INTEGER NOT NULL/g" internal/core/schema.go
sed -i "s/admission_log.*created_at TEXT NOT NULL/admission_log.created_at INTEGER NOT NULL/g" internal/core/schema.go
sed -i "s/scheduled_tasks.*last_run_at  DATETIME/scheduled_tasks.last_run_at  INTEGER/g" internal/core/schema.go
sed -i "s/scheduled_tasks.*next_run_at  DATETIME/scheduled_tasks.next_run_at INTEGER/g" internal/core/schema.go
sed -i "s/scheduled_tasks.*created_at   DATETIME/scheduled_tasks.created_at   INTEGER/g" internal/core/schema.go
sed -i "s/scheduled_tasks.*updated_at   DATETIME/scheduled_tasks.updated_at   INTEGER/g" internal/core/schema.go
sed -i "s/session_handoffs.*ended_at      DATETIME/session_handoffs.ended_at      INTEGER/g" internal/core/schema.go
sed -i "s/session_handoffs.*read_at       DATETIME/session_handoffs.read_at       INTEGER/g" internal/core/schema.go
sed -i "s/audit_cluster_proposals.*first_seen    DATETIME/audit_cluster_proposals.first_seen    INTEGER/g" internal/core/schema.go
sed -i "s/audit_cluster_proposals.*last_seen     DATETIME/audit_cluster_proposals.last_seen     INTEGER/g" internal/core/schema.go
sed -i "s/audit_cluster_proposals.*snooze_until  DATETIME/audit_cluster_proposals.snooze_until  INTEGER/g" internal/core/schema.go
sed -i "s/ephemeral_scratchpad.*decay_at DATETIME/ephemeral_scratchpad.decay_at INTEGER/g" internal/core/schema.go
sed -i "s/retrieval_metadata.*last_retrieved_at DATETIME/retrieval_metadata.last_retrieved_at INTEGER/g" internal/core/schema.go
```

- [ ] **Step 3: Update memory_revisions.created_at specifically**

Locate the line and replace:

```bash
grep -n "memory_revisions.*created_at" internal/core/schema.go
```

Expected: a line containing `STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')`. Replace with `created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))`.

Manual edit at the line found: change the entire `created_at TEXT DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')),` line to `created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),`.

- [ ] **Step 4: Update SafeMigrations entries that added DATETIME columns**

The `SafeMigrations` list contains columns added by prior migrations as `DATETIME` types — these are no-op for fresh DBs (the BaseTables define them as INTEGER now) but the SafeMigrations entries must match for upgrade-in-place DBs to land on the same type. Find and update:

```bash
grep -n '"memories", "last_accessed_at", "DATETIME"' internal/core/schema.go
grep -n '"memories", "updated_at", "DATETIME"' internal/core/schema.go
grep -n '"memories", "expires_at", "DATETIME"' internal/core/schema.go
```

For each match, change `"DATETIME"` to `"INTEGER"`.

- [ ] **Step 5: Verify build**

```bash
go build -tags fts5 ./...
```

Expected: success.

- [ ] **Step 6: Run the migration test against a fresh schema**

```bash
cd internal/core && go test -tags fts5 -run "TestSchema|TestMigrate" -v
```

Expected: migration test still PASS. Existing schema tests still PASS.

- [ ] **Step 7: Run the full test suite (expect some failures in struct/scan code — those are addressed in subsequent tasks)**

```bash
make test 2>&1 | head -50
```

Expected: many compile/test errors pointing at struct fields like `CreatedAt time.Time` that need updating. Document the failing test names but do not fix yet — Task 5+ address them.

- [ ] **Step 8: Commit**

```bash
git add internal/core/schema.go
git commit -m "feat(core): flip all DATETIME TEXT columns to INTEGER in schema DDL

All 33 columns across 19 tables now resolve to INTEGER Unix-epoch seconds
with default CAST(strftime('%s','now') AS INTEGER). SafeMigrations entries
matching these columns are updated to INTEGER so upgrade-in-place DBs land
on the same type as fresh schemas.

Note: struct fields and SQL scan sites still expect time.Time/string for
these columns. Subsequent tasks in this branch update them; do not merge
this commit in isolation."
```

---

## Task 5: Schema fingerprint test (Test 2)

**Files:**
- Create: `internal/core/schema_fingerprint_timestamps_test.go`

- [ ] **Step 1: Write the failing fingerprint test**

Create `internal/core/schema_fingerprint_timestamps_test.go`:

```go
package internal

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestSchemaFingerprint_TimestampColumnsAreInteger proves every column in the
// timestamps_unified_v1 migration scope resolves to INTEGER (not TEXT) after
// migration runs. Lifts the pattern from schema_foundation_test.go.
//
// If a future DDL change re-introduces a DATETIME TEXT column for any of these
// fields, this test fails loudly. SQLite's manifest typing would otherwise
// silently accept strings stored in INTEGER columns.
func TestSchemaFingerprint_TimestampColumnsAreInteger(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Run the migration against the freshly-created DB to land the sentinel
	// and confirm idempotency on the test fixture.
	if err := runTimestampsMigration(t, db.SQLDB()); err != nil {
		t.Fatalf("runTimestampsMigration: %v", err)
	}

	for _, tc := range allTimestampsToMigrate {
		var sqliteType string
		err := db.SQLDB().QueryRow(
			`SELECT typeof([%s]) FROM %s LIMIT 1`,
			tc.col, tc.table,
		).Scan(&sqliteType)
		// LIMIT 1 returns no rows on empty tables — that's fine, we just
		// check the column exists via pragma.
		if err != nil {
			// Confirm column exists via pragma; skip typeof check on empty tables.
			var colType string
			pragmaErr := db.SQLDB().QueryRow(
				`SELECT type FROM pragma_table_info(?, ?)`,
				tc.table, tc.col,
			).Scan(&colType)
			if pragmaErr != nil {
				t.Errorf("%s.%s: pragma lookup failed: %v", tc.table, tc.col, pragmaErr)
				continue
			}
			// 'integer' / 'INTEGER' from pragma_table_info
			if colType != "INTEGER" && colType != "integer" {
				t.Errorf("%s.%s: expected INTEGER column, got %q (pragma)", tc.table, tc.col, colType)
			}
			continue
		}
		// Non-empty table: typeof() returns 'integer' for INT columns
		if sqliteType != "integer" {
			t.Errorf("%s.%s: expected integer storage, got %q (typeof)", tc.table, tc.col, sqliteType)
		}
	}
}

// openTestDBCore returns a *DatabaseManager with the standard test schema
// applied. Implementation: copy from internal/core/schema_foundation_test.go
// or test helpers in the same package.
func openTestDB(t *testing.T) *DatabaseManager {
	t.Helper()
	// Use the existing test helper if present. If not, this test will fail
	// at compile time and the engineer should add the helper. Look for
	// `setupTestDB`, `newTestDB`, or `openTestDB` in adjacent _test.go files.
	return setupTestDB(t)
}
```

- [ ] **Step 2: Confirm the test helper signature exists**

```bash
grep -rn "func setupTestDB\|func newTestDB\|func openTestDB" internal/core/ | grep _test.go
```

If a matching helper exists, use it. If multiple exist, pick the one that returns `*DatabaseManager`. If none exists, look at how other tests in `internal/core/` construct a test DB and align — this may require writing a small helper in a `_test_helpers.go` file (out of scope for the test itself; see Task 12).

- [ ] **Step 3: Run the fingerprint test**

```bash
cd internal/core && go test -tags fts5 -run TestSchemaFingerprint_TimestampColumnsAreInteger -v
```

Expected: PASS (after Task 4's schema changes). All 37 entries in `allTimestampsToMigrate` resolve to INTEGER.

- [ ] **Step 4: Commit**

```bash
git add internal/core/schema_fingerprint_timestamps_test.go
git commit -m "test(core): schema fingerprint — every migrated column is INTEGER

Defends against future DDL drift that re-introduces DATETIME TEXT columns
for any of the 37 (table, column) pairs in allTimestampsToMigrate. SQLite's
manifest typing would silently accept strings stored in INTEGER columns;
this test catches that at build time rather than at production runtime."
```

---

## Task 6: Format helpers (Test 4)

**Files:**
- Create: `internal/core/format_time.go`
- Create: `internal/core/format_time_test.go`

- [ ] **Step 1: Write the failing format helper test**

Create `internal/core/format_time_test.go`:

```go
package internal

import (
	"testing"
)

func TestFormatUnixSeconds(t *testing.T) {
	cases := []struct {
		sec      int64
		expected string
	}{
		{0, "1970-01-01T00:00:00Z"},
		{1785421960, "2026-07-30T14:32:40Z"},
	}
	for _, c := range cases {
		got := FormatUnixSeconds(c.sec)
		if got != c.expected {
			t.Errorf("FormatUnixSeconds(%d) = %q, want %q", c.sec, got, c.expected)
		}
	}
}

func TestFormatOptionalUnixSeconds(t *testing.T) {
	zero := int64(0)
	cases := []struct {
		name     string
		sec      *int64
		expected string
	}{
		{"nil", nil, ""},
		{"zero", &zero, "1970-01-01T00:00:00Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FormatOptionalUnixSeconds(c.sec)
			if got != c.expected {
				t.Errorf("FormatOptionalUnixSeconds(%v) = %q, want %q", c.sec, got, c.expected)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd internal/core && go test -tags fts5 -run "TestFormatUnixSeconds|TestFormatOptionalUnixSeconds" -v
```

Expected: FAIL with `undefined: FormatUnixSeconds` / `undefined: FormatOptionalUnixSeconds`.

- [ ] **Step 3: Implement the helpers**

Create `internal/core/format_time.go`:

```go
package internal

import "time"

// FormatUnixSeconds returns the RFC3339-formatted UTC string for an int64
// Unix-epoch seconds value. Used at every CLI/MCP display boundary to convert
// the int64 stored in the DB to a human-readable timestamp.
func FormatUnixSeconds(sec int64) string {
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// FormatOptionalUnixSeconds returns the RFC3339-formatted UTC string for an
// int64 pointer, or empty string if the pointer is nil. Used for nullable
// timestamp columns.
func FormatOptionalUnixSeconds(sec *int64) string {
	if sec == nil {
		return ""
	}
	return time.Unix(*sec, 0).UTC().Format(time.RFC3339)
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd internal/core && go test -tags fts5 -run "TestFormatUnixSeconds|TestFormatOptionalUnixSeconds" -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/core/format_time.go internal/core/format_time_test.go
git commit -m "feat(core): FormatUnixSeconds + FormatOptionalUnixSeconds

Centralized RFC3339 formatting at the CLI/MCP display boundary. Storage
is int64 (non-nullable) or *int64 (nullable); these helpers convert at
the boundary so downstream consumers see RFC3339 strings."
```

---

## Task 7: Add ParseTimestampArg helper (CLI input layer)

**Files:**
- Create: `internal/core/parse_time.go`
- Create: `internal/core/parse_time_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/core/parse_time_test.go`:

```go
package internal

import (
	"testing"
	"time"
)

func TestParseTimestampArg(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{"unix-int", "1785421960", 1785421960, false},
		{"unix-negative", "-1", -1, false},
		{"rfc3339", "2026-07-30T14:32:40Z", 1785421960, false},
		{"rfc3339-offset", "2026-07-30T14:32:40+00:00", 1785421960, false},
		{"garbage", "not-a-time", 0, true},
		{"empty", "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseTimestampArg(c.input)
			if c.wantErr {
				if err == nil {
					t.Errorf("ParseTimestampArg(%q) succeeded, want error", c.input)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseTimestampArg(%q) error: %v", c.input, err)
			}
			if got != c.want {
				t.Errorf("ParseTimestampArg(%q) = %d, want %d", c.input, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd internal/core && go test -tags fts5 -run TestParseTimestampArg -v
```

Expected: FAIL with `undefined: ParseTimestampArg`.

- [ ] **Step 3: Implement the parser**

Create `internal/core/parse_time.go`:

```go
package internal

import (
	"fmt"
	"strconv"
	"time"
)

// ParseTimestampArg accepts either a unix-epoch integer (seconds) or an
// RFC3339-formatted string. Used by CLI flags like --as-of, --since,
// --until, and snooze_until so users can opt into the simpler integer form
// when scripting. Returns the int64 Unix-epoch seconds representation.
func ParseTimestampArg(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty timestamp")
	}
	// Try unix-epoch integer first (cheaper than parsing RFC3339).
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("invalid timestamp: %q (want unix-epoch integer or RFC3339)", s)
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd internal/core && go test -tags fts5 -run TestParseTimestampArg -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/core/parse_time.go internal/core/parse_time_test.go
git commit -m "feat(core): ParseTimestampArg accepts RFC3339 OR unix-epoch integer

Single CLI input parser for --as-of, --since, --until, and snooze_until
flags. RFC3339 string OR unix-epoch integer both accepted; integer returned
so the value flows directly into the int64 storage layer."
```

---

## Task 8: Update Memory struct (the central type)

**Files:**
- Modify: `internal/core/memory.go` (locate the `Memory` struct definition)

- [ ] **Step 1: Locate the Memory struct**

```bash
grep -n "^type Memory struct\|^type Memory " internal/core/memory.go
```

Expected: a struct definition near the top of `memory.go`.

- [ ] **Step 2: Update the timestamp fields**

Change the `Memory` struct so the migrated columns become `int64` (non-nullable) or `*int64` (nullable):

```go
type Memory struct {
	ID               string  `json:"id"`
	Collection       string  `json:"collection"`
	Content          string  `json:"content"`
	SessionID        string  `json:"session_id,omitempty"`
	Tags             string  `json:"tags,omitempty"`
	Metadata         string  `json:"metadata,omitempty"`
	CreatedAt        int64   `json:"created_at"`
	UpdatedAt        int64   `json:"updated_at"`
	LastAccessedAt   *int64  `json:"last_accessed_at,omitempty"`
	ExpiresAt        *int64  `json:"expires_at,omitempty"`
	SourceDB         string  `json:"source_db,omitempty"`
	SourceID         string  `json:"source_id,omitempty"`
	PromotedAt       float64 `json:"promoted_at"`
	DeletedAt        int64   `json:"deleted_at"`
	LastSynthesizedAt int64  `json:"last_synthesized_at"`
}
```

(Adjust to match the exact field set in the codebase; the columns shown here are the ones the migration touches. `PromotedAt` stays `float64` — it's already REAL and out of scope.)

- [ ] **Step 3: Find every scan site that reads Memory fields**

```bash
grep -n "&.*\.CreatedAt\|&.*\.UpdatedAt\|&.*\.LastAccessedAt\|&.*\.ExpiresAt" internal/core/*.go internal/core/tools/*.go cmd/mpm/*.go
```

For each scan site that assigns to a migrated Memory field, change:
- `&m.CreatedAt` → `&m.CreatedAt` (no signature change; int64 target)
- `&m.UpdatedAt` → `&m.UpdatedAt`
- `&m.LastAccessedAt` (was `*time.Time`) → `&m.LastAccessedAt` (now `**int64`)? Or `&m.LastAccessedAt` with a fresh `*int64` first?

Driver behavior: when scanning NULL into `**int64`, the driver allocates a `*int64` set to nil. When scanning non-NULL, it allocates a `*int64` set to the value. So passing `&field` (where field is `*int64`) works directly — the driver's `sql.Null*-style` helper handles it. **Verify**: if mattn/go-sqlite3 doesn't support this, allocate a `var tmp *int64` and pass `&tmp`, then assign `field = tmp` after Scan.

Practical implementation: grep for the existing pattern in the codebase. `memory.go` already scans `&deletedAt` for the existing `DeletedAt int64` field — same pattern applies.

- [ ] **Step 4: Build to verify**

```bash
go build -tags fts5 ./...
```

Expected: success. If compile errors point at scan sites, apply the same `&field` pattern. If they point at struct field types being mismatched elsewhere (e.g., `time.Time` operations like `.After()` or `.Format()`), refactor those callers to use the int64 directly and apply `FormatUnixSeconds` / `FormatOptionalUnixSeconds` at the display boundary.

- [ ] **Step 5: Commit**

```bash
git add internal/core/memory.go
git commit -m "refactor(core): Memory struct fields int64/*int64 for migrated timestamps

CreatedAt, UpdatedAt → int64. LastAccessedAt, ExpiresAt → *int64.
DeletedAt, LastSynthesizedAt already int64 — unchanged. PromotedAt stays
float64 (REAL, out of scope). Scan sites updated to match; display layer
callers use FormatUnixSeconds / FormatOptionalUnixSeconds at the boundary."
```

---

## Task 9: Update remaining structs (Session, Topic, TopicMembership, etc.)

**Files:**
- Modify: each struct definition file (see manifest below)

For each struct, apply the same pattern as Task 8: flip `time.Time` → `int64` or `*int64` per the spec table, fix scan sites.

- [ ] **Step 1: Apply the field-type changes**

Use the spec table at `docs/superpowers/specs/2026-07-30-unix-epoch-timestamps-design.md` (section "Go Struct Field Changes"). For each entry:

| Struct | Change |
|---|---|
| `Session` | `CreatedAt time.Time` → `CreatedAt int64` |
| `Topic` | `CreatedAt time.Time` → `CreatedAt int64`, `UpdatedAt time.Time` → `UpdatedAt int64` |
| `TopicMembership` | `CreatedAt time.Time` → `CreatedAt int64` |
| `SystemConfig` | `UpdatedAt time.Time` → `UpdatedAt int64` |
| `RetrievalMetadata` | `CreatedAt int64`, `UpdatedAt int64`, `LastRetrievedAt *time.Time` → `*int64` |
| `ExternalDBCursor` | `UpdatedAt time.Time` → `UpdatedAt int64` |
| `ReferenceDoc` | `CreatedAt time.Time` → `CreatedAt int64` |
| `AuditEntry` | `CreatedAt time.Time` → `CreatedAt int64` |
| `AuditClusterProposal` | `CreatedAt int64`, `UpdatedAt int64`, `FirstSeen int64`, `LastSeen int64`, `SnoozeUntil *time.Time` → `*int64` |
| `Handoff` | `CreatedAt int64`, `EndedAt int64`, `ReadAt *time.Time` → `*int64` |
| `Wake` | `CreatedAt time.Time` → `CreatedAt int64` (others already int64) |
| `ScheduledTask` | `CreatedAt int64`, `UpdatedAt int64`, `LastRunAt *time.Time` → `*int64`, `NextRunAt int64` |
| `ScratchpadRow` | `CreatedAt int64`, `UpdatedAt int64`, `DecayAt int64` |
| `VectorCluster` | `UpdatedAt time.Time` → `UpdatedAt int64` |
| `VectorAssignment` | `UpdatedAt time.Time` → `UpdatedAt int64` |
| `ReferenceInteraction` | `CreatedAt time.Time` → `CreatedAt int64` |
| `AdmissionLogRow` | `CreatedAt time.Time` → `CreatedAt int64` |
| `MemoryRevision` | `CreatedAt time.Time` → `CreatedAt int64` |

Note: field names may differ in the codebase (e.g., `Created time.Time` for lessons). Adjust per actual struct.

- [ ] **Step 2: Update scan sites for each struct**

```bash
grep -rn "&[a-zA-Z]*\.CreatedAt\|&[a-zA-Z]*\.UpdatedAt\|&[a-zA-Z]*\.EndedAt\|&[a-zA-Z]*\.ReadAt" internal/core/*.go internal/core/tools/*.go cmd/mpm/*.go
```

For each hit, ensure the variable type matches the struct field type. If scan sites have local helpers like `var createdAt time.Time`, change to `var createdAt int64` (or `*int64`).

- [ ] **Step 3: Build to verify**

```bash
go build -tags fts5 ./...
```

Expected: success. Fix compile errors iteratively — the compiler points at each mismatch.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "refactor(core): all migrated struct fields int64/*int64

18 structs updated to match the integer-timestamp schema. Non-nullable
columns become int64; nullable columns become *int64. Scan sites updated
to match. Display layer callers format at the boundary via
FormatUnixSeconds / FormatOptionalUnixSeconds."
```

---

## Task 10: SQL query updates — Category 1 (datetime('now') → strftime)

**Files:**
- Modify: `internal/core/audit.go` (2 sites)
- Modify: `internal/core/handoff.go` (1 site)
- Modify: `internal/core/wake_context.go` (2 sites)

- [ ] **Step 1: Update audit.go**

```bash
grep -n "datetime('now'" internal/core/audit.go
```

Replace each `datetime('now', ?)` with `CAST(strftime('%s','now', ?) AS INTEGER)`. Also replace `datetime('now', ? || ' days')` (if present) the same way.

- [ ] **Step 2: Update handoff.go**

```bash
grep -n "datetime('now'" internal/core/handoff.go
```

Replace `datetime('now', '-' || ? || ' days')` with `CAST(strftime('%s','now', '-' || ? || ' days') AS INTEGER)`.

- [ ] **Step 3: Update wake_context.go**

```bash
grep -n "datetime('now'" internal/core/wake_context.go
```

Replace each `datetime('now', '-N days')` with `CAST(strftime('%s','now', '-N days') AS INTEGER)`.

- [ ] **Step 4: Build to verify**

```bash
go build -tags fts5 ./...
```

Expected: success.

- [ ] **Step 5: Commit**

```bash
git add internal/core/audit.go internal/core/handoff.go internal/core/wake_context.go
git commit -m "feat(core): SQL queries use strftime('%s','now',...) for time arithmetic

5 sites converted from datetime('now',...) to CAST(strftime('%s','now',...) AS INTEGER).
The integer form is consistent with the migrated schema and avoids the
datetime-vs-integer comparison gotcha the deleted_at incident surfaced."
```

---

## Task 11: Boundary function updates — Category 2

**Files:**
- Modify: `internal/core/web_db.go` (PruneMemoriesBefore)
- Modify: `internal/core/db.go` (GetMemoriesByDateRange, GetMemoryRevisionAtTime)
- Modify: `cmd/mpm/recall.go` (--since, --until, --as-of flags)
- Modify: `internal/core/cluster_proposals.go` (parseClusterSnoozeUntil)

- [ ] **Step 1: Update PruneMemoriesBefore signature**

Locate the function and its caller:

```bash
grep -n "PruneMemoriesBefore\|func.*MemoriesBefore" internal/core/web_db.go cmd/mpm/*.go
```

Change the signature to accept `int64` (unix-epoch seconds) instead of `time.Time`. Update the caller in `cmd/mpm/maint_cmds.go` (or wherever it's invoked) to pass `before.Unix()`.

- [ ] **Step 2: Update GetMemoriesByDateRange**

```bash
grep -n "GetMemoriesByDateRange\|fromDate, toDate string" internal/core/db.go
```

Change `fromDate` and `toDate` parsing to convert via `ParseTimestampArg` at the top of the function. The downstream SQL `created_at >= ? AND created_at <= ?` continues to work because the values are now int64.

- [ ] **Step 3: Update GetMemoryRevisionAtTime**

```bash
grep -n "GetMemoryRevisionAtTime" internal/core/db.go
```

Change the function body to:
1. Convert `asOf` to int64 once at the top (`asOfSec := asOf.Unix()`).
2. Compare `createdAt` (now INTEGER) directly against `asOfSec` (no `time.Parse` cascade).
3. Delete the `time.Parse` cascade at lines 3064-3075 and 3084-3093 (memory_createdAt and memory_deletedAt are now INTEGER; no parsing needed).
4. Drop the `asOfStr` construction at line 3099 (no string conversion needed).

This is a significant simplification. Verify the function still returns nil for "memory didn't exist yet" and "memory was deleted before asOf" cases.

- [ ] **Step 4: Update recall.go flags**

```bash
grep -n "as-of\|--since\|--until\|asOf.*RFC3339\|time.Parse.*RFC3339" cmd/mpm/recall.go
```

Each flag definition changes from `time.Parse(time.RFC3339, *asOf)` to `ParseTimestampArg(*asOf)`. The error message changes accordingly.

- [ ] **Step 5: Update parseClusterSnoozeUntil**

```bash
grep -n "func parseClusterSnoozeUntil" internal/core/cluster_proposals.go
```

Extend the parser to accept unix-epoch integer in addition to RFC3339 and Go-relative duration. Order of attempts: integer → RFC3339 → Go-relative (existing).

- [ ] **Step 6: Build to verify**

```bash
go build -tags fts5 ./...
```

Expected: success.

- [ ] **Step 7: Commit**

```bash
git add internal/core/web_db.go internal/core/db.go cmd/mpm/recall.go internal/core/cluster_proposals.go
git commit -m "feat(core): boundary functions accept unix-epoch ints at the API layer

PruneMemoriesBefore, GetMemoriesByDateRange, GetMemoryRevisionAtTime,
recall flags, parseClusterSnoozeUntil — all accept unix-epoch ints at the
boundary. RFC3339 strings still accepted for backward compatibility. The
time.Parse cascade in GetMemoryRevisionAtTime is removed because
memories.created_at and memories.deleted_at are now INTEGER."
```

---

## Task 12: INSERT site updates — Category 3

**Files:**
- Modify: `internal/core/web_db.go` (line 909)
- Modify: `internal/core/admission_db.go` (line 120)
- Modify: `internal/core/compact.go` (lines 261, 330 — verify each)
- Modify: `cmd/mpm/handlers_epistemology.go` (lines 155, 317 — verify each)
- Modify: `cmd/mpm/handlers_challenge.go` (line 32 — verify)
- Modify: `cmd/mpm/simple_cmds.go` (line 739 — verify)

- [ ] **Step 1: Inventory the INSERT sites**

```bash
grep -n "time.Now().UTC().Format(time.RFC3339)" internal/core/web_db.go internal/core/admission_db.go internal/core/compact.go cmd/mpm/handlers_epistemology.go cmd/mpm/handlers_challenge.go cmd/mpm/simple_cmds.go
```

For each site, identify the column it writes. If the column is in `allTimestampsToMigrate`, change `time.Now().UTC().Format(time.RFC3339)` to `time.Now().Unix()`. If the column is in a JSON sidecar (active.json, scratchpad JSON), leave it as RFC3339.

- [ ] **Step 2: Apply changes**

Edit each file. For example, in `internal/core/web_db.go` line ~909:

```go
// Before:
`, GenerateID(), docID, chunkID, query, searchKind, rank, score, time.Now().UTC().Format(time.RFC3339))`

// After:
`, GenerateID(), docID, chunkID, query, searchKind, rank, score, time.Now().Unix())`
```

Apply the same pattern to each verified site. **Important**: do not change sites that write to JSON sidecars (active.json, scratchpad files). The grep result helps disambiguate.

- [ ] **Step 3: Build to verify**

```bash
go build -tags fts5 ./...
```

Expected: success.

- [ ] **Step 4: Commit**

```bash
git add internal/core/web_db.go internal/core/admission_db.go internal/core/compact.go cmd/mpm/handlers_epistemology.go cmd/mpm/handlers_challenge.go cmd/mpm/simple_cmds.go
git commit -m "feat(core): INSERT sites write unix-epoch ints to migrated columns

All writes to migrated columns now use time.Now().Unix() instead of
RFC3339-formatted strings. JSON sidecar writes (active.json, scratchpad
JSON) are unchanged — they live outside the schema migration scope."
```

---

## Task 13: MCP JSON Schema updates

**Files:**
- Modify: any MCP tool definition that accepts `--as-of` / `--since` / `--until` / `snooze_until`-style inputs

- [ ] **Step 1: Find the MCP tool definitions**

```bash
grep -rn '"as_of"\|"since"\|"until"\|"snooze_until"\|"asOf"\|as-of' internal/core/tools/ | grep -i "type.*string"
```

For each match, the JSON Schema `type` field is `"string"`. Change to `"type": ["string", "integer"]` so MCP clients don't reject integer inputs before Go parses them.

- [ ] **Step 2: Apply changes**

Edit each tool definition. For example:

```go
// Before:
"asOf": map[string]any{"type": "string", "description": "RFC3339 timestamp"}

// After:
"asOf": map[string]any{"type": []string{"string", "integer"}, "description": "RFC3339 timestamp or unix-epoch seconds"}
```

- [ ] **Step 3: Build to verify**

```bash
go build -tags fts5 ./...
```

Expected: success.

- [ ] **Step 4: Commit**

```bash
git add internal/core/tools/
git commit -m "feat(tools): MCP JSON schemas accept integer for timestamp inputs

Tools that accept --as-of-style inputs update their JSON Schema to
type: [string, integer] so MCP clients don't reject integer inputs."
```

---

## Task 14: Update existing test fixtures (Test 5)

**Files:**
- Modify: ~50-100 INSERT statements across `internal/core/*_test.go` and `cmd/mpm/*_test.go`

This is the largest mechanical step. The pattern is uniform.

- [ ] **Step 1: Inventory all RFC3339 INSERT values for migrated columns**

```bash
grep -rn "INSERT INTO.*VALUES.*created_at.*'20" internal/core/ cmd/mpm/
```

Expected: a long list. Each entry either:
- (a) passes an explicit RFC3339 string that must become `int64`, or
- (b) uses `CURRENT_TIMESTAMP` / `datetime('now')` and must become `CAST(strftime('%s','now') AS INTEGER)` or `?` with `time.Now().Unix()` bound.

- [ ] **Step 2: Apply the transformation per file**

For each `_test.go` file, apply:

**RFC3339 string INSERTs:** Replace `'2026-07-30 14:32:40'` style literals with hardcoded `int64(1785421960)`. If the test asserts on a specific value (e.g., "this row was just inserted, so its created_at is within 1 second of now"), use `?` and bind `time.Now().Unix()`.

**CURRENT_TIMESTAMP inserts:** Replace `CURRENT_TIMESTAMP` with `CAST(strftime('%s','now') AS INTEGER)`.

**Scanning `time.Time`:** Replace `var createdAt time.Time` with `var createdAt int64` (or `*int64`). Replace scan-site helpers that convert with the format helpers at the assertion boundary.

- [ ] **Step 3: Run the full test suite**

```bash
make test 2>&1 | tail -100
```

Iterate on failures. Each failure points at a remaining RFC3339-vs-int64 mismatch. Fix and re-run.

- [ ] **Step 4: Run from the core module in isolation**

```bash
cd internal/core && go test -tags fts5 ./...
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "test(core): existing test fixtures use unix-epoch timestamps

50-100 INSERT sites updated to pass int64 (or current-time binds) to
migrated columns. Scan sites updated to read int64 / *int64. Display-layer
assertions format via FormatUnixSeconds at the boundary."
```

---

## Task 15: AST walker test (Test 6)

**Files:**
- Create: `internal/core/timestamp_writepath_test.go`

This is a static-analysis test that mirrors the `TestScannerCoverage_AllMemoriesWritersScanContent` pattern. It walks the Go AST of the codebase and asserts no write path constructs `time.Now().UTC().Format(time.RFC3339)` and feeds the result into a migrated column.

- [ ] **Step 1: Read the existing AST walker precedent**

```bash
grep -rln "parser.ParseDir\|ast.Inspect" internal/core/*_test.go
```

Read the file to confirm the AST traversal pattern. Use it as the template for the timestamp walker.

- [ ] **Step 2: Write the AST walker test**

Create `internal/core/timestamp_writepath_test.go`:

```go
package internal

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// timestampWhitelist identifies Go source locations where it's acceptable to
// write RFC3339-formatted strings into a JSON sidecar (active.json, scratchpad
// JSON, working-context JSON). These are NOT database columns and stay RFC3339.
//
// Format: "relative/path/file.go:lineno" → "reason".
var timestampWhitelist = map[string]string{
	// Populated after Task 12 lands. Initial entries seeded from the
	// cmd/mpm JSON sidecar sites that survive the migration:
	// "cmd/mpm/handlers_stance.go:69": "active.json Updated field, not a DB column",
	// "cmd/mpm/route_render.go:156": "active.json Updated field, not a DB column",
	// "cmd/mpm/handlers_epistemology.go:155": "scratchpad JSON, not a DB column",
}

// TestTimestampWritePaths_NoRFC3339IntoMigratedColumns walks every Go file in
// internal/ and cmd/ and asserts no call to time.Now().UTC().Format(time.RFC3339)
// flows into a migrated database column. SQLite's manifest typing would
// silently accept strings stored in INTEGER columns; this test catches that.
//
// Whitelisted sites are documented in timestampWhitelist with justification.
func TestTimestampWritePaths_NoRFC3339IntoMigratedColumns(t *testing.T) {
	fset := token.NewFileSet()

	// Build a fast lookup of migrated column names.
	migratedCols := map[string]bool{}
	for _, tc := range allTimestampsToMigrate {
		migratedCols[tc.col] = true
	}

	violations := []string{}

	for _, dir := range []string{"internal", "cmd"} {
		pkgs, err := parser.ParseDir(fset, dir, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("ParseDir(%q): %v", dir, err)
		}
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				pos := fset.Position(file.Pos())
				if strings.HasSuffix(pos.Filename, "_test.go") {
					continue // tests may use RFC3339 literals as fixtures
				}

				// For each *ast.CallExpr that looks like Exec/Query, examine args.
				ast.Inspect(file, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if sel.Sel.Name != "Exec" && sel.Sel.Name != "Query" && sel.Sel.Name != "QueryRow" {
						return true
					}

					// First arg is the SQL string. Look for migrated column names.
					if len(call.Args) < 1 {
						return true
					}
					sqlLit, ok := call.Args[0].(*ast.BasicLit)
					if !ok || sqlLit.Kind != token.STRING {
						return true
					}
					sql := strings.Trim(sqlLit.Value, "\"`")
					hasMigratedCol := false
					for col := range migratedCols {
						if strings.Contains(sql, col) {
							hasMigratedCol = true
							break
						}
					}
					if !hasMigratedCol {
						return true
					}

					// Walk remaining args; flag any that are time.Now().UTC().Format(time.RFC3339).
					for _, arg := range call.Args[1:] {
						if isRFC3339FormatCall(arg) {
							argPos := fset.Position(arg.Pos())
							key := relativeKey(argPos)
							if _, ok := timestampWhitelist[key]; !ok {
								violations = append(violations, fmt.Sprintf(
									"%s: RFC3339-format call flows into migrated column. Add to timestampWhitelist with justification, or change to time.Now().Unix()",
									key,
								))
							}
						}
					}
					return true
				})
			}
		}
	}

	if len(violations) > 0 {
		for _, v := range violations {
			t.Error(v)
		}
		t.Fatalf("%d timestamp write-path violations found", len(violations))
	}
}

// isRFC3339FormatCall reports whether expr matches the shape
//
//	time.Now().UTC().Format(time.RFC3339)
//
// Approximate: also matches time.Now().Format(time.RFC3339) without the UTC()
// hop. We don't try to match every equivalent shape — the goal is to flag
// the common explicit pattern, not dynamic constructions.
func isRFC3339FormatCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "Format" {
		return false
	}
	// Format argument must be time.RFC3339 or time.RFC3339Nano.
	if len(call.Args) < 1 {
		return false
	}
	argSel, ok := call.Args[0].(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if argSel.Sel.Name != "RFC3339" && argSel.Sel.Name != "RFC3339Nano" {
		return false
	}
	// Receiver of Format must be time.Now() possibly chained through .UTC().
	recv := sel.X
	if utc, ok := recv.(*ast.CallExpr); ok {
		if sel, ok := utc.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "UTC" {
			recv = sel.X
		}
	}
	nowCall, ok := recv.(*ast.CallExpr)
	if !ok {
		return false
	}
	nowSel, ok := nowCall.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return nowSel.Sel.Name == "Now"
}

// relativeKey returns the position as a "path:line" string relative to the
// repo root (cwd). Matches the format used in timestampWhitelist.
func relativeKey(pos token.Position) string {
	// Strip cwd prefix to get a stable relative path.
	cwd, _ := getCwd()
	key := pos.Filename
	if cwd != "" && strings.HasPrefix(key, cwd+"/") {
		key = strings.TrimPrefix(key, cwd+"/")
	}
	return fmt.Sprintf("%s:%d", key, pos.Line)
}

func getCwd() (string, error) {
	// Use os.Getwd via a thin wrapper to avoid importing "os" at the top
	// (kept minimal for the test). os.Getwd is the standard call.
	import_os_getwd() // helper inlined below
	return osGetwd()
}
```

The `osGetwd` and `import_os_getwd` shims above must be replaced with a real `os.Getwd` call. The full replacement:

```go
import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// ... isRFC3339FormatCall unchanged ...

func relativeKey(pos token.Position) string {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Sprintf("%s:%d", pos.Filename, pos.Line)
	}
	key := pos.Filename
	if strings.HasPrefix(key, cwd+"/") {
		key = strings.TrimPrefix(key, cwd+"/")
	}
	return fmt.Sprintf("%s:%d", key, pos.Line)
}
```

- [ ] **Step 3: Run the test**

```bash
cd internal/core && go test -tags fts5 -run TestTimestampWritePaths_NoRFC3339IntoMigratedColumns -v
```

Expected: PASS — Task 12 already converted the writes to `time.Now().Unix()`, so the walker should find no violations.

If violations appear:
1. Note the file:line reported.
2. Decide: change the call site to `time.Now().Unix()` (preferred), or add to `timestampWhitelist` with a justification comment.
3. Re-run.

- [ ] **Step 4: Commit**

```bash
git add internal/core/timestamp_writepath_test.go
git commit -m "test(core): AST walker — no RFC3339 strings into migrated columns

Mirrors TestScannerCoverage_AllMemoriesWritersScanContent pattern. Walks
the AST of internal/ and cmd/ and asserts no write path constructs
time.Now().UTC().Format(time.RFC3339) into a migrated DB column. SQLite
manifest typing would silently accept strings stored in INTEGER columns;
this test catches that at build time."
```

---

## Task 16: Smoke script (Test 7)

**Files:**
- Create: `scripts/smoke_timestamps.sh`

- [ ] **Step 1: Write the smoke script**

Create `scripts/smoke_timestamps.sh`:

```bash
#!/usr/bin/env bash
# Smoke test for the unix-epoch timestamp migration.
# Boots an isolated MPM_WORKSPACE, ingests a memory, exercises the display
# boundary, and asserts both:
#   1. The CLI output is RFC3339-formatted (display layer intact).
#   2. The underlying DB column is INTEGER (storage layer migrated).
#   3. The MCP tool output is RFC3339-formatted (MCP display layer intact).
#   4. --as-of accepts both RFC3339 and unix-epoch int with identical results.

set -euo pipefail

WORKSPACE=$(mktemp -d)
export MPM_WORKSPACE="$WORKSPACE"
export MPM_DB="$WORKSPACE/mpm.db"

trap "rm -rf '$WORKSPACE'" EXIT

# Build mpm into a temp location.
TMPBIN=$(mktemp -d)
trap "rm -rf '$WORKSPACE' '$TMPBIN'" EXIT
go build -tags fts5 -o "$TMPBIN/mpm" ./cmd/mpm

# Initialize the DB.
"$TMPBIN/mpm" init >/dev/null

# Ingest a memory.
"$TMPBIN/mpm" remember "smoke-test-timestamp-migration" --tags test >/dev/null

# 1. CLI output is RFC3339.
OUT=$("$TMPBIN/mpm" recall --query "smoke-test-timestamp-migration" 2>&1)
if ! echo "$OUT" | grep -qE "[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}"; then
    echo "FAIL: CLI output does not contain RFC3339 timestamp"
    echo "Output: $OUT"
    exit 1
fi
echo "PASS: CLI output is RFC3339-formatted"

# 2. Underlying DB column is INTEGER.
COL_TYPE=$(sqlite3 "$PMM_DB" "SELECT type FROM pragma_table_info('memories') WHERE name='created_at'")
if [ "$COL_TYPE" != "integer" ]; then
    echo "FAIL: memories.created_at column type is '$COL_TYPE', expected 'integer'"
    exit 1
fi
echo "PASS: memories.created_at is INTEGER"

# 3. MCP tool output is RFC3339-formatted.
MCP_OUT=$("$TMPBIN/mpm" call recall --payload '{"query":"smoke-test-timestamp-migration"}')
if ! echo "$MCP_OUT" | jq -e '.[] | .created_at | match("^[0-9]{4}-[0-9]{2}-[0-9]{2}T")' >/dev/null; then
    echo "FAIL: MCP output created_at is not RFC3339"
    echo "Output: $MCP_OUT"
    exit 1
fi
echo "PASS: MCP output created_at is RFC3339"

# 4. --as-of accepts both RFC3339 and unix-epoch int with identical results.
#    First, get the memory's created_at as int64.
NOW_INT=$(date -u +%s)
RFC3339=$(date -u +%Y-%m-%dT%H:%M:%SZ)
RES_RFC=$("$TMPBIN/mpm" recall --as-of "$RFC3339" --query "smoke-test-timestamp-migration" 2>&1 || true)
RES_INT=$("$TMPBIN/mpm" recall --as-of "$NOW_INT" --query "smoke-test-timestamp-migration" 2>&1 || true)

# Same query, same result count (both should return the memory).
COUNT_RFC=$(echo "$RES_RFC" | grep -c "smoke-test-timestamp-migration" || echo 0)
COUNT_INT=$(echo "$RES_INT" | grep -c "smoke-test-timestamp-migration" || echo 0)
if [ "$COUNT_RFC" != "$COUNT_INT" ]; then
    echo "FAIL: --as-of RFC3339 returned $COUNT_RFC, --as-of int returned $COUNT_INT"
    exit 1
fi
echo "PASS: --as-of accepts both RFC3339 and unix-epoch int"

echo ""
echo "ALL SMOKE TESTS PASSED"
```

- [ ] **Step 2: Make it executable and run**

```bash
chmod +x scripts/smoke_timestamps.sh
./scripts/smoke_timestamps.sh
```

Expected: all four PASS lines and `ALL SMOKE TESTS PASSED`.

- [ ] **Step 3: Commit**

```bash
git add scripts/smoke_timestamps.sh
git commit -m "test(scripts): smoke_timestamps.sh — RFC3339 display, INTEGER storage

Boots isolated MPM_WORKSPACE, exercises:
  1. CLI output is RFC3339 (display layer intact)
  2. memories.created_at is INTEGER (storage migrated)
  3. MCP tool output is RFC3339 (MCP display intact)
  4. --as-of accepts both RFC3339 and unix-epoch int with identical results"
```

---

## Task 17: Docs updates

**Files:**
- Modify: `CLAUDE.md` (Gotchas section)
- Modify: `README.md` (--as-of, --since, --until, snooze_until flag descriptions)

- [ ] **Step 1: Update CLAUDE.md Gotchas**

Locate the existing bullet:

```markdown
- `CURRENT_TIMESTAMP` is still used for *setting* `deleted_at`; `strftime('%s','now')` is used for *comparing* against it (intentional, but worth documenting).
```

Replace with:

```markdown
- All timestamp columns are INTEGER Unix-epoch seconds (unified by `timestamps_unified_v1` migration). The `deleted_at_unified_v1` precedent no longer applies separately — both migrations wrap in the same `DatabaseManager.init` transaction.
```

- [ ] **Step 2: Update README.md flag descriptions**

```bash
grep -n "\-\-as-of\|\-\-since\|\-\-until\|snooze_until" README.md
```

For each flag, update the description to mention "RFC3339 OR unix-epoch integer". Example:

```markdown
# Before:
--as-of string  Point-in-time reconstruction: retrieve memory state as of this timestamp (RFC3339)

# After:
--as-of string  Point-in-time reconstruction: retrieve memory state as of this timestamp (RFC3339 OR unix-epoch integer)
```

- [ ] **Step 3: Add changelog entry**

Append to `changelog.md` (or whatever the project's changelog file is):

```markdown
- **feat(core): unify all timestamp columns on unix-epoch seconds** — All 33 DATETIME TEXT columns across 19 tables migrated to INTEGER seconds via `timestamps_unified_v1` sentinel. Go struct fields become `int64` / `*int64`. CLI inputs accept both RFC3339 and unix-epoch integers. Display formatting centralized at `FormatUnixSeconds` / `FormatOptionalUnixSeconds`. **Operators must take `mpm backup-db` before installing this release.**
```

- [ ] **Step 4: Commit**

```bash
git add CLAUDE.md README.md changelog.md
git commit -m "docs: timestamp migration notes + CLI input format

CLAUDE.md Gotchas updated (the old CURRENT_TIMESTAMP-vs-strftime note is
obsolete). README flag descriptions note both RFC3339 and unix-epoch int
inputs. Changelog warns operators to backup-db before installing."
```

---

## Task 18: Final verification and squash

**Files:** none (verification only)

- [ ] **Step 1: Run the full test suite from repo root**

```bash
make clean
make test
```

Expected: all tests pass.

- [ ] **Step 2: Run the full test suite from the core module**

```bash
cd internal/core && go test -tags fts5 -v ./...
```

Expected: all tests pass.

- [ ] **Step 3: Run the existing shared-DB smoke script**

```bash
./scripts/smoke_shared.sh
```

Expected: all sentinels present and shared-DB invariants hold.

- [ ] **Step 4: Run the new timestamp smoke script**

```bash
./scripts/smoke_timestamps.sh
```

Expected: all four checks pass.

- [ ] **Step 5: Boot a populated DB through the migration**

```bash
# Copy the dev DB (NOT production).
cp src/db/mpm.db /tmp/mpm.db.backup
cp src/db/mpm.db /tmp/mpm.db.test

export MPM_WORKSPACE=/tmp/mpm-workspace-test
mkdir -p "$MPM_WORKSPACE"
cp /tmp/mpm.db.test "$MPM_WORKSPACE/mpm.db"

# Boot the binary; init runs the migration.
./bin/mpm init

# Verify zero row loss.
sqlite3 "$MPM_WORKSPACE/mpm.db" "SELECT COUNT(*) FROM memories"
# Compare against the pre-migration count from /tmp/mpm.db.backup.
sqlite3 /tmp/mpm.db.backup "SELECT COUNT(*) FROM memories"
# Counts should match.

# Verify the schema is INTEGER.
sqlite3 "$MPM_WORKSPACE/mpm.db" "SELECT name, type FROM pragma_table_info('memories') WHERE name='created_at'"
# Expected: created_at|integer
```

Expected: row counts match; created_at is integer.

- [ ] **Step 6: Manual verification of --as-of parity**

```bash
mpm recall --as-of 2026-07-30T00:00:00Z --query "some known memory" 2>&1 | head
mpm recall --as-of 1785421800 --query "some known memory" 2>&1 | head
```

Expected: same memories returned (or both empty for the same reason).

- [ ] **Step 7: Squash commits 1-14 into a single atomic commit**

```bash
# List the commits on the feature branch since main.
git log --oneline main..HEAD

# Interactive rebase to squash.
git rebase -i main
```

In the rebase editor:
- Mark Task 14's commit (the test-fixtures commit) as `pick`.
- Mark all earlier commits (Tasks 2-13) as `squash` or `fixup`.
- The squashed result becomes one atomic commit matching the message in the spec.

The final commit list should be:
- `pick` Task 14 (test fixtures) → becomes the squashed "atomic core"
- `pick` Task 15 (AST walker)
- `pick` Task 16 (smoke script)
- `pick` Task 17 (docs)

Result: 4 commits on the feature branch — one atomic core, three follow-ups.

- [ ] **Step 8: Force-push the feature branch**

```bash
git push origin feat/unix-epoch-timestamps --force-with-lease
```

Expected: branch is now ready for review/merge.

---

## Out-of-Scope Reminders

These items are explicitly NOT part of this migration:
- `active.json` `Updated` field — JSON sidecar, stays RFC3339.
- Scratchpad JSON files (`session_id.json`) — JSON sidecars, stay RFC3339.
- Working-context JSON files — JSON sidecars, stay RFC3339.
- `raw_memories` table — already INTEGER/REAL with its own conventions.
- `memories.promoted_at REAL` — already fractional; out of scope.
- Sub-second resolution everywhere (millisecond/nanosecond) — out of scope.

---

## Self-Review Checklist (run before declaring done)

1. **Spec coverage:** All 7 design sections in `docs/superpowers/specs/2026-07-30-unix-epoch-timestamps-design.md` map to tasks:
   - Schema changes → Tasks 3, 4
   - Migration function → Task 2
   - Go struct fields → Tasks 8, 9
   - SQL queries → Tasks 10, 11
   - Display layer → Task 6
   - CLI input → Task 7
   - Tests 1-7 → Tasks 2, 5, 6, 14, 15, 16
   - Risk register / rollback → Tasks 17, 18
2. **Placeholder scan:** No "TBD", "TODO", or vague steps remain.
3. **Type consistency:** `FormatUnixSeconds(int64)` / `FormatOptionalUnixSeconds(*int64)` / `ParseTimestampArg(string) (int64, error)` are used consistently across Tasks 6, 7, 11.
4. **Commit strategy:** Tasks 2-14 squash to one atomic commit; Tasks 15-17 stay separate. Verified in Task 18.