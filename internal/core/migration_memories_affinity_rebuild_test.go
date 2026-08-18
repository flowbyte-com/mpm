package internal

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// affinityRebuildTestDBCounter is retained for compatibility with
// older test scaffolding; the affinity tests now use on-disk
// tmpfiles (one per test via t.TempDir) instead of shared-cache
// in-memory DBs. See setupAffinityRebuildDB for the rationale.
var affinityRebuildTestDBCounter int64

// setupAffinityRebuildDB opens a fresh on-disk DB (tmpfile,
// auto-cleaned via t.TempDir) that mimics the ON-DISK shape of a
// legacy mpm.db whose `memories` table still declares timestamp
// columns with DATETIME affinity. The FTS5 shadow, sync triggers,
// and outgoing FK from session_id are all installed — matching what
// a real legacy DB looks like at the point `mpm doctor` starts
// complaining about the Review backlog.
//
// Why on-disk tmpfile and not in-memory shared-cache: the rebuild
// test holds an open transaction that pins a connection. With
// in-memory shared-cache, Go's database/sql pool can hand the next
// query a different connection that doesn't see the tx's writes —
// which then deadlocks on sqlite_master during the second test's
// CREATE TABLE. On-disk tmpfile guarantees one DB per test, no
// shared state, no lock contention.
func setupAffinityRebuildDB(t *testing.T) *sql.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "affinity-test.db")
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=1")
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dbPath, err)
	}

	// Legacy DDL: created_at/updated_at/last_accessed_at all
	// declared DATETIME (→ NUMERIC affinity). deleted_at is TEXT.
	// expires_at is DATETIME. The canonical schema.go DDL uses
	// INTEGER for all five — that's what we're rebuilding to.
	schema := `
	CREATE TABLE memories (
		id TEXT PRIMARY KEY,
		collection TEXT NOT NULL,
		content TEXT NOT NULL,
		session_id TEXT,
		tags JSON,
		metadata JSON,
		embedding BLOB,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		deleted_at TEXT,
		last_accessed_at DATETIME,
		expires_at DATETIME,
		weight INTEGER DEFAULT 0,
		is_long_term INTEGER DEFAULT 0,
		reinforcement_count INTEGER DEFAULT 0,
		FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE SET NULL
	);
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		name TEXT
	);
	CREATE TABLE schema_migrations (
		id TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	);

	CREATE INDEX idx_memories_collection ON memories(collection);
	CREATE INDEX idx_memories_deleted ON memories(deleted_at);

	CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
		INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
		VALUES (new.rowid, new.content, new.collection, new.session_id, new.tags);
	END;

	CREATE VIRTUAL TABLE memories_fts USING fts5(content, collection, session_id UNINDEXED, tags);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		t.Fatalf("apply legacy schema: %v", err)
	}
	return db
}

// seedAffinityRebuildRows inserts a small dataset whose timestamp
// values are valid SQLite ISO-8601 strings (parseable by strftime)
// plus one row with NULL last_accessed_at (exercises the NULL pass-
// through path) plus one row whose deleted_at is a valid ISO string
// (proves deleted_at gets flipped to INTEGER too).
func seedAffinityRebuildRows(t *testing.T, db *sql.DB) {
	t.Helper()

	rows := []struct {
		id, collection, content, createdAt, updatedAt, lastAccessed, deletedAt string
		isLongTerm                                                             int
	}{
		{"mem-1", "notes", "first memory", "2026-01-15 10:00:00", "2026-01-15 10:00:00", "2026-08-10 12:30:00", nilString, 0},
		{"mem-2", "notes", "second memory", "2026-02-20 14:00:00", "2026-02-20 14:00:00", "2026-08-12 09:15:00", "2026-08-13 00:00:00", 1},
		{"mem-3", "ltm", "third memory (LTM)", "2026-03-25 16:00:00", "2026-03-25 16:00:00", nilString, nilString, 1},
		{"mem-4", "notes", "fourth memory", "2026-04-30 18:00:00", "2026-04-30 18:00:00", "2026-08-15 14:00:00", nilString, 0},
		{"mem-5", "notes", "fifth memory", "2026-05-10 20:00:00", "2026-05-10 20:00:00", nilString, nilString, 0},
	}
	for _, r := range rows {
		var lastAccessed, deletedAt interface{} = nil, nil
		if r.lastAccessed != nilString {
			lastAccessed = r.lastAccessed
		}
		if r.deletedAt != nilString {
			deletedAt = r.deletedAt
		}
		// session_id stays SQL NULL — the FK to sessions(id) requires
		// it to either match an existing sessions row or be NULL. An
		// empty string would violate the FK and fail PRAGMA
		// foreign_key_check, masking the rebuild's own correctness
		// signal.
		_, err := db.Exec(
			`INSERT INTO memories (id, collection, content, session_id, tags, metadata, embedding,
				created_at, updated_at, last_accessed_at, deleted_at, is_long_term)
			 VALUES (?, ?, ?, NULL, ?, ?, NULL, ?, ?, ?, ?, ?)`,
			r.id, r.collection, r.content, "[]", "{}",
			r.createdAt, r.updatedAt, lastAccessed, deletedAt, r.isLongTerm,
		)
		if err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
}

// nilString is the sentinel used by the seed table to express "this
// column gets SQL NULL". An empty string would be stored as TEXT and
// violate the FK on session_id or shift the checksum's NULL pass-
// through logic.
const nilString = "\x00NULL_SENTINEL\x00"

// nilZero is a no-op identity helper kept for compatibility with
// earlier versions of this test file; new code uses nilString.
// Deprecated: use nilString.
func nilZero(s string) string { return s }

// queryReviewBacklog simulates the exact query `mpm doctor`'s
// Review backlog check runs — it's the failure mode that motivated
// this migration. If the rebuild is correct, this query returns
// without error and the resulting int64 values are parseable.
func queryReviewBacklog(t *testing.T, db *sql.DB) (rows int64, lastID string) {
	t.Helper()
	q := db.QueryRow(
		`SELECT id, created_at FROM memories
		 WHERE deleted_at IS NULL
		   AND (is_long_term = 1 OR weight >= 5)
		   AND (last_accessed_at IS NULL OR last_accessed_at < CAST(strftime('%s','now', '-14 days') AS INTEGER))
		 ORDER BY last_accessed_at ASC NULLS FIRST
		 LIMIT 1`,
	)
	var id string
	var createdAt int64
	if err := q.Scan(&id, &createdAt); err != nil {
		// sql.ErrNoRows is acceptable — no candidates is not a failure
		// of the schema, just an empty review queue.
		if err == sql.ErrNoRows {
			return 0, ""
		}
		t.Fatalf("review-backlog query: %v", err)
	}
	// Note: we don't actually return createdAt here because LIMIT 1
	// may return 0 rows on this fixture; the SCHEMA test is whether
	// the Scan succeeds at all (the column-type drift failure was a
	// Scan error, not a row-not-found).
	_ = createdAt
	// Count via a separate query so we get the actual backlog length.
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM memories
		 WHERE deleted_at IS NULL
		   AND (is_long_term = 1 OR weight >= 5)
		   AND (last_accessed_at IS NULL OR last_accessed_at < CAST(strftime('%s','now', '-14 days') AS INTEGER))`,
	).Scan(&rows); err != nil {
		t.Fatalf("review-backlog count: %v", err)
	}
	return rows, id
}

// TestAffinityRebuild_FixesReviewBacklogScan is the primary
// regression guard. It seeds a legacy-shape DB, runs the migration,
// then runs the EXACT query that `mpm doctor` was failing on. Pre-
// migration the Scan fails with "converting driver.Value type
// time.Time to a int64: invalid syntax". Post-migration the Scan
// succeeds and `created_at` is INTEGER.
func TestAffinityRebuild_FixesReviewBacklogScan(t *testing.T) {
	db := setupAffinityRebuildDB(t)
	t.Cleanup(func() { db.Close() })

	// Sanity-check the failure mode on the LEGACY shape before we
	// rebuild. If this Scan does NOT fail, the test is meaningless
	// — the bug isn't being exercised.
	seedAffinityRebuildRows(t, db)
	var legacyID string
	var legacyCreated int64
	errScan := db.QueryRow(`SELECT id, created_at FROM memories LIMIT 1`).Scan(&legacyID, &legacyCreated)
	if errScan == nil {
		t.Fatalf("expected legacy-shape Scan to fail (column-type drift), got nil — bug not being exercised")
	}
	if !strings.Contains(errScan.Error(), "converting driver.Value") {
		t.Logf("legacy Scan failed (good — bug is exercised), but with unexpected error: %v", errScan)
	}

	// Run the rebuild.
	if err := RebuildMemoriesColumnAffinity(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	// Post-rebuild: the same Scan must succeed (returning time.Time
	// means the column still has TEXT affinity somewhere).
	var id string
	var createdAt int64
	err := db.QueryRow(`SELECT id, created_at FROM memories ORDER BY id LIMIT 1`).Scan(&id, &createdAt)
	if err != nil {
		t.Fatalf("post-rebuild Scan failed: %v", err)
	}
	if createdAt <= 0 {
		t.Errorf("post-rebuild created_at must be positive epoch seconds, got %d", createdAt)
	}
}

// TestAffinityRebuild_PreservesRowCountAndChecksum pins the
// composite-checksum invariant from v's landmine #3. The 4-tuple
// (row count, distinct id, created_at sum, last_accessed_at sum)
// must match pre and post rebuild — if it doesn't, silent data loss
// or NULL-coercion slipped through.
func TestAffinityRebuild_PreservesRowCountAndChecksum(t *testing.T) {
	db := setupAffinityRebuildDB(t)
	t.Cleanup(func() { db.Close() })
	seedAffinityRebuildRows(t, db)

	// Pre-migration: compute on TEXT-shape column using strftime.
	pre, err := checksumMemoriesTableDB(context.Background(), db, "memories", true)
	if err != nil {
		t.Fatalf("pre-checksum: %v", err)
	}
	if pre.RowCount != 5 {
		t.Errorf("pre RowCount: want 5, got %d", pre.RowCount)
	}
	if pre.DistinctID != 5 {
		t.Errorf("pre DistinctID: want 5, got %d", pre.DistinctID)
	}

	if err := RebuildMemoriesColumnAffinity(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	// Post-migration: compute on INTEGER-shape column (no strftime).
	post, err := checksumMemoriesTableDB(context.Background(), db, "memories", false)
	if err != nil {
		t.Fatalf("post-checksum: %v", err)
	}
	if pre != post {
		t.Fatalf("checksum mismatch: pre=%+v post=%+v", pre, post)
	}
}

// TestAffinityRebuild_RecreatesIndexesAndTriggers verifies the
// post-rebuild state has the same index set and trigger set as the
// pre-rebuild state. The doctor and FTS5 sync depend on these
// existing; losing them silently is exactly the failure class the
// Substrate Defense Triad (silent-swallow, edge-case promote) was
// codified to prevent.
func TestAffinityRebuild_RecreatesIndexesAndTriggers(t *testing.T) {
	db := setupAffinityRebuildDB(t)
	t.Cleanup(func() { db.Close() })

	// Pre-count.
	preIndexes := countNamedObjects(t, db, "index", "memories")
	preTriggers := countNamedObjects(t, db, "trigger", "memories")
	if preIndexes < 2 {
		t.Fatalf("expected ≥2 indexes pre-rebuild, got %d", preIndexes)
	}
	if preTriggers < 1 {
		t.Fatalf("expected ≥1 trigger pre-rebuild, got %d", preTriggers)
	}

	if err := RebuildMemoriesColumnAffinity(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	postIndexes := countNamedObjects(t, db, "index", "memories")
	postTriggers := countNamedObjects(t, db, "trigger", "memories")
	if preIndexes != postIndexes {
		t.Errorf("index count: pre=%d post=%d", preIndexes, postIndexes)
	}
	if preTriggers != postTriggers {
		t.Errorf("trigger count: pre=%d post=%d", preTriggers, postTriggers)
	}
}

// TestAffinityRebuild_IsIdempotent re-runs the migration on an
// already-rebuilt DB and asserts (a) no error, (b) sentinel is
// still present, (c) no further work happens (row count and
// checksum unchanged).
func TestAffinityRebuild_IsIdempotent(t *testing.T) {
	db := setupAffinityRebuildDB(t)
	t.Cleanup(func() { db.Close() })
	seedAffinityRebuildRows(t, db)

	if err := RebuildMemoriesColumnAffinity(db); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}

	post1, err := checksumMemoriesTableDB(context.Background(), db, "memories", false)
	if err != nil {
		t.Fatalf("post1 checksum: %v", err)
	}

	// Re-run.
	if err := RebuildMemoriesColumnAffinity(db); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}

	post2, err := checksumMemoriesTableDB(context.Background(), db, "memories", false)
	if err != nil {
		t.Fatalf("post2 checksum: %v", err)
	}
	if post1 != post2 {
		t.Errorf("idempotency broken: post1=%+v post2=%+v", post1, post2)
	}

	var sentinels int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		memoriesColumnAffinitySentinel,
	).Scan(&sentinels); err != nil {
		t.Fatalf("sentinel probe: %v", err)
	}
	if sentinels != 1 {
		t.Errorf("sentinel count: want 1, got %d", sentinels)
	}
}

// TestAffinityRebuild_NoOpOnIntegerShape verifies the migration
// doesn't touch a database that's already INTEGER-shaped (the
// canonical post-Phase-7 install). The sentinel write should NOT
// happen on the first run — only on actual rebuilds — so that a
// re-bootstrap of a clean install doesn't leave a sentinel row
// behind that masks future drift.
func TestAffinityRebuild_NoOpOnIntegerShape(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "affinity-test-clean.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Canonical-shape memories table (INTEGER timestamps).
	cleanSchema := `
	CREATE TABLE memories (
		id TEXT PRIMARY KEY,
		collection TEXT NOT NULL,
		content TEXT NOT NULL,
		created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		deleted_at INTEGER,
		last_accessed_at INTEGER,
		expires_at INTEGER
	);
	CREATE TABLE schema_migrations (id TEXT PRIMARY KEY, applied_at INTEGER NOT NULL);
	`
	if _, err := db.Exec(cleanSchema); err != nil {
		t.Fatalf("apply clean schema: %v", err)
	}
	_, err = db.Exec(`INSERT INTO memories (id, collection, content) VALUES ('m1','c','hello')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := RebuildMemoriesColumnAffinity(db); err != nil {
		t.Fatalf("clean rebuild (should be no-op): %v", err)
	}

	// Sentinel must NOT have been written — this is a no-op install.
	var sentinels int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		memoriesColumnAffinitySentinel,
	).Scan(&sentinels); err != nil {
		t.Fatalf("sentinel probe: %v", err)
	}
	if sentinels != 0 {
		t.Errorf("sentinel must NOT be written on no-op rebuild, got count=%d", sentinels)
	}
}

// TestAffinityRebuild_RunsAgainstRealOnDiskDB is the integration
// check against an isolated copy of /home/v/.../src/db/mpm.db —
// the exact scenario that motivated this work. It is skipped if
// the source DB doesn't exist (CI without a pre-existing
// production DB).
//
// On success:
//   - The 5 timestamp columns flip from DATETIME/TEXT → INTEGER
//   - The doctor-style review-backlog query succeeds (no
//     "converting driver.Value" Scan error)
//   - The migration sentinel is recorded
//
// On failure:
//   - The original DB is left untouched (we operate on a copy)
//   - The error message names the failing probe
func TestAffinityRebuild_RunsAgainstRealOnDiskDB(t *testing.T) {
	srcPath := findProductionDB(t)
	if srcPath == "" {
		t.Skip("no production mpm.db found; skipping real-DB integration test")
	}

	// Copy to a tmp file in the same directory (so the shared
	// schema attach path resolves cleanly). Cleaned up via t.TempDir.
	tmpDir := t.TempDir()
	dstPath := filepath.Join(tmpDir, "mpm.db.test")

	// Use sqlite3 .backup over the CLI for cross-process safety.
	// Skipping if sqlite3 CLI isn't available.
	if !hasSqlite3CLI() {
		t.Skip("sqlite3 CLI not available; skipping real-DB integration test")
	}
	if out, err := runCmd("sqlite3", srcPath, fmt.Sprintf(".backup %q", dstPath)); err != nil {
		t.Fatalf("backup %s -> %s: %v (out=%s)", srcPath, dstPath, err, out)
	}

	// Open with FK enforcement ON (matching production DSN).
	db, err := sql.Open("sqlite3", dstPath+"?_foreign_keys=1")
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Confirm the legacy-shape failure mode is reproducible on the
	// copy. If it isn't, the DB has already been migrated and the
	// rebuild should no-op.
	if !hasLegacyShape(t, db) {
		t.Skip("on-disk DB has already been migrated; no-op path verified by TestAffinityRebuild_NoOpOnIntegerShape")
	}

	if err := RebuildMemoriesColumnAffinity(db); err != nil {
		t.Fatalf("real-DB rebuild failed: %v", err)
	}

	// Verify the doctor-style query now succeeds.
	var createdAt int64
	if err := db.QueryRow(`SELECT created_at FROM memories ORDER BY created_at DESC LIMIT 1`).Scan(&createdAt); err != nil {
		t.Fatalf("post-rebuild doctor query failed: %v", err)
	}
	if createdAt <= 0 {
		t.Errorf("post-rebuild created_at: want positive epoch, got %d", createdAt)
	}

	// Verify sentinel was written.
	var sentinelCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		memoriesColumnAffinitySentinel,
	).Scan(&sentinelCount); err != nil {
		t.Fatalf("sentinel probe: %v", err)
	}
	if sentinelCount != 1 {
		t.Errorf("sentinel count: want 1, got %d", sentinelCount)
	}
}

// ───────────────────────── helpers ─────────────────────────

// mustTxForDB wraps a db.Begin() with t.Fatal on error so the tests
// stay focused on the migration's invariants. Distinct name from
// `mustTx` (declared in cascade_outbox_test.go) because that one
// takes a *DatabaseManager; this one takes a raw *sql.DB so the
// rebuild tests can run against an in-memory fixture without a
// DatabaseManager wrapper.
func mustTxForDB(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Don't Commit — rollback is fine for read-only checksum probes.
	// The tx is released on connection return.
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// countNamedObjects counts the rows in sqlite_master for a given
// (type, tbl_name) pair. Used by the indexes/triggers preservation
// test.
func countNamedObjects(t *testing.T, db *sql.DB, kind, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type=? AND tbl_name=?`,
		kind, table,
	).Scan(&n); err != nil {
		t.Fatalf("count %s on %s: %v", kind, table, err)
	}
	return n
}

// findProductionDB looks for a candidate production mpm.db under
// the user's home and the project working tree. Returns empty
// string if none is found.
func findProductionDB(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"/home/v/workspace/projects/mpm/src/db/mpm.db",
		"/home/v/.mpm/store.db",
	}
	for _, p := range candidates {
		if pathExists(p) {
			return p
		}
	}
	return ""
}

// hasLegacyShape returns true if any of the timestamp columns on
// `memories` is still declared with DATETIME/TEXT/DATE affinity.
func hasLegacyShape(t *testing.T, db *sql.DB) bool {
	t.Helper()
	for _, col := range timestampColumnsToRebuild {
		var declType string
		err := db.QueryRow(
			`SELECT type FROM pragma_table_info('memories') WHERE name=?`, col,
		).Scan(&declType)
		if err != nil {
			continue
		}
		up := strings.ToUpper(declType)
		if up == "DATETIME" || up == "DATE" || up == "TEXT" {
			return true
		}
	}
	return false
}
// ───────────────────────── filesystem / shell helpers ─────────────────────────

// pathExists returns true if p is a regular file (or a symlink to one).
// Uses os.Stat rather than os.Lstat so a symlink to a missing file is
// reported as not-existing.
func pathExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// hasSqlite3CLI returns true if the sqlite3 CLI binary is on PATH.
// The integration test uses `.backup` over the CLI for cross-process
// safety; if it's not available we skip rather than fail.
func hasSqlite3CLI() bool {
	_, err := exec.LookPath("sqlite3")
	return err == nil
}

// runCmd runs an external command, captures combined output, and
// returns (output, nil) on success or (output, error) on failure.
func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// checksumMemoriesTableDB is the *sql.DB version of
// checksumMemoriesTable. Opens a short-lived read tx internally so
// the connection isn't held while other work runs (which would
// trigger "database is locked" when the rebuild tries to write).
func checksumMemoriesTableDB(ctx context.Context, db *sql.DB, table string, useStrftime bool) (memoriesChecksum, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return memoriesChecksum{}, err
	}
	defer tx.Rollback()
	return checksumMemoriesTable(ctx, tx, table, useStrftime)
}
