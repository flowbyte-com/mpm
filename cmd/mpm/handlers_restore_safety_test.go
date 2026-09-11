// Regression tests for the 2026-09-11 T78 destructive-ordering bug.
//
// The pre-fix handleRestoreDB would:
//   1. Clear WAL/SHM siblings
//   2. Rename the live DB to .pre-restore
//   3. Validate the dump
//
// If validation rejected (e.g. unsafe statement), the original DB was
// stranded at .pre-restore with the canonical path empty. Subsequent
// `mpm` calls saw an empty or partial DB and the operator could not
// recover without manually moving the .pre-restore file back.
//
// Post-fix, validate-before-mutate: the live DB is only touched AFTER
// a fully validated dump is in hand. On any failure, the live DB is
// preserved byte-for-byte.

package main

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// populateTestDB writes a known dataset into the dm so the test has
// something concrete to verify (memories count, sha256 of the file, etc.).
func populateTestDB(t *testing.T, dm *mpminternal.DatabaseManager) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, deleted_at, created_at, updated_at, weight)
		VALUES
		  ('restore-safety-mem-1', 'memories', 'original row 1', NULL, 1700000000, 1700000000, 50),
		  ('restore-safety-mem-2', 'memories', 'original row 2', NULL, 1700000001, 1700000001, 75)
	`)
	require.NoError(t, err)
}

// sha256File returns the SHA-256 of the file at path, or "<missing>"
// if the file does not exist.
func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "<missing>"
		}
		require.NoError(t, err)
	}
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	require.NoError(t, err)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// restoreSafetyCountMemories returns the count of non-deleted memories
// in the dm. Named to avoid clashing with handlers_status.go's
// package-level `countMemories` helper.
func restoreSafetyCountMemories(t *testing.T, dm *mpminternal.DatabaseManager) int {
	t.Helper()
	var n int
	row := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`)
	require.NoError(t, row.Scan(&n))
	return n
}

// restoreSafetySentinelCount counts only the rows this test's setup
// populated — id LIKE 'restore-safety-%'. Used to insulate row-count
// assertions from baseline-seeded memory rows that schema init may
// insert alongside the canonical schema.
//
// Note: queries a fresh sql.DB on the file at dbPath rather than
// dm.SQLDB(). The dm's *sql.DB connection pool holds file handles
// from before the restore — the handler renames dbPath to .pre-restore
// and writes a new file at dbPath. dm.SQLDB() would be querying the
// stale inode (gone, in fact). A fresh connection reads the actual
// current file at dbPath.
func restoreSafetySentinelCountAtPath(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	require.NoError(t, err)
	defer db.Close()
	var n int
	row := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id LIKE 'restore-safety-%'`)
	require.NoError(t, row.Scan(&n))
	return n
}

// setupRestoreSafetyTest wires the dbManager singleton to a hermetic
// test DM backed by a real file in t.TempDir().
//
// Why not newTestDMForCmd (in-memory)? The handler operates on
// dm.DBPath() — it uses os.Rename, sql.Open, os.Remove on the file
// directly. An in-memory DM has no real file path and the handler
// would fail at the rename step. Mirrors the pattern in
// TestFlushWal_PassesForeignKeysPragma.
func setupRestoreSafetyTest(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmpDir)
	dm, err := mpminternal.NewDatabaseManager("")
	require.NoError(t, err)
	t.Cleanup(func() { _ = dm.Close() })
	require.NotEmpty(t, dm.DBPath(),
		"file-based DM must produce a non-empty DBPath — got empty "+
			"(workspace=%q). Without this, the handler's os.Rename would "+
			"silently fail.", tmpDir)

	// Consume sync.Once then swap in our test DM. Same pattern as
	// setupBackupSingletonTest.
	_ = getDB()
	prev := dbManager
	prevErr := dbManagerInitErr
	dbManager = dm
	dbManagerInitErr = nil
	t.Cleanup(func() {
		dbManager = prev
		dbManagerInitErr = prevErr
	})
	return dm
}

// driveStdin replaces os.Stdin with a pipe carrying "y\n" so the
// handler's user-confirmation prompt receives a yes. Restores
// os.Stdin on test cleanup. The handleRestoreDB fmt.Scanln reads
// from os.Stdin so this is the only way to drive it from a test.
func driveStdinYes(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })
	_, err = w.WriteString("y\n")
	require.NoError(t, err)
	require.NoError(t, w.Close())
}

// TestHandleRestoreDB_RejectionLeavesOriginalIntact exercises the core
// T78 regression. The validator rejects `bad_unknown_table.sql` because
// it INSERTs into a non-canonical table. Pre-fix, the live DB had been
// renamed to .pre-restore before validation ran; the canonical path
// was empty. Post-fix, validation runs first and the original DB
// remains byte-for-byte intact.
func TestHandleRestoreDB_RejectionLeavesOriginalIntact(t *testing.T) {
	dm := setupRestoreSafetyTest(t)
	dbPath := dm.DBPath()
	require.NotEmpty(t, dbPath)

	populateTestDB(t, dm)

	preHash := sha256File(t, dbPath)
	preCount := restoreSafetyCountMemories(t, dm)

	dumpPath := filepath.Join("testdata", "restore_db", "bad_unknown_table.sql")
	driveStdinYes(t)

	exit := handleRestoreDB([]string{"mpm", dumpPath})
	require.Equal(t, 1, exit, "validator should reject bad_unknown_table.sql")

	// CRITICAL: live DB must be byte-for-byte unchanged.
	postHash := sha256File(t, dbPath)
	require.Equal(t, preHash, postHash,
		"live DB file must be byte-for-byte unchanged after rejected restore — "+
			"pre-fix, the rename to .pre-restore happened BEFORE validation")

	postCount := restoreSafetyCountMemories(t, dm)
	require.Equal(t, preCount, postCount,
		"row count must be unchanged after rejected restore")

	// CRITICAL: `.pre-restore` must NOT exist (no destructive rename).
	preRestorePath := dbPath + ".pre-restore"
	if _, err := os.Stat(preRestorePath); err == nil {
		t.Fatalf("fresh .pre-restore file appeared at %s — destructive rename "+
			"happened before validation rejected the dump", preRestorePath)
	}
}

// TestHandleRestoreDB_MalformedDumpLeavesOriginalIntact covers the
// "validator passes but per-statement Exec fails" branch — the dump
// references a column that doesn't exist. Validator passes (statements
// target canonical table `memories`); per-statement Exec fails on the
// bogus column. The restoreFromBackup path catches this and moves the
// original back.
func TestHandleRestoreDB_MalformedDumpLeavesOriginalIntact(t *testing.T) {
	dm := setupRestoreSafetyTest(t)
	dbPath := dm.DBPath()
	require.NotEmpty(t, dbPath)

	populateTestDB(t, dm)
	preHash := sha256File(t, dbPath)
	preCount := restoreSafetyCountMemories(t, dm)

	malformedDump := `PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
INSERT INTO memories (id, collection, content, bogus_col, created_at, updated_at, weight)
VALUES ('restore-safety-mem-1', 'memories', 'attempted overwrite', 1, 1700000000, 1700000000, 1);
COMMIT;
`
	dumpPath := filepath.Join(t.TempDir(), "malformed.sql")
	require.NoError(t, os.WriteFile(dumpPath, []byte(malformedDump), 0o600))
	driveStdinYes(t)

	exit := handleRestoreDB([]string{"mpm", dumpPath})
	require.Equal(t, 1, exit, "malformed dump should fail")

	postHash := sha256File(t, dbPath)
	require.Equal(t, preHash, postHash,
		"live DB must be byte-for-byte unchanged after malformed dump failure")

	postCount := restoreSafetyCountMemories(t, dm)
	require.Equal(t, preCount, postCount,
		"row count must be unchanged after malformed dump failure")
}

// TestHandleRestoreDB_StatementFailureLeavesOriginalIntact covers the
// case where the validator passes but a downstream statement in the
// transaction fails — e.g. PRIMARY KEY constraint violation. The
// transaction rolls back; restoreFromBackup must move the original
// back to the canonical path.
func TestHandleRestoreDB_StatementFailureLeavesOriginalIntact(t *testing.T) {
	dm := setupRestoreSafetyTest(t)
	dbPath := dm.DBPath()
	require.NotEmpty(t, dbPath)

	populateTestDB(t, dm)
	preHash := sha256File(t, dbPath)
	preCount := restoreSafetyCountMemories(t, dm)

	duplicateKeyDump := `PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
INSERT INTO memories (id, collection, content, deleted_at, created_at, updated_at, weight)
VALUES ('restore-safety-mem-1', 'memories', 'duplicate row', NULL, 1700000000, 1700000000, 99);
COMMIT;
`
	dumpPath := filepath.Join(t.TempDir(), "duplicate.sql")
	require.NoError(t, os.WriteFile(dumpPath, []byte(duplicateKeyDump), 0o600))
	driveStdinYes(t)

	exit := handleRestoreDB([]string{"mpm", dumpPath})
	require.Equal(t, 1, exit, "duplicate-key dump should fail")

	postHash := sha256File(t, dbPath)
	require.Equal(t, preHash, postHash,
		"live DB must be byte-for-byte unchanged after statement failure")

	postCount := restoreSafetyCountMemories(t, dm)
	require.Equal(t, preCount, postCount,
		"row count must be unchanged after statement failure")
}

// TestHandleRestoreDB_SuccessOverwritesOriginal is the positive control:
// a valid dump that the validator accepts and that imports cleanly
// adds new content to the live DB. The validator only allows
// BEGIN/COMMIT/ROLLBACK/END, INSERT, CREATE (TABLE/TRIGGER/VIEW/INDEX),
// and PRAGMA — so the success path uses INSERT.
//
// We do NOT use DELETE here because the validator rejects it. The
// success-path semantics we verify are: handler returns 0, file still
// exists at canonical path, and the new content is present.
func TestHandleRestoreDB_SuccessOverwritesOriginal(t *testing.T) {
	dm := setupRestoreSafetyTest(t)
	dbPath := dm.DBPath()
	require.NotEmpty(t, dbPath)

	populateTestDB(t, dm)
	// After a successful restore, the dm's pre-populated rows live in
	// the OLD inode (now removed). The NEW file at dbPath starts empty
	// and gains exactly 1 row from the dump's INSERT. So
	// preSafetyCount on the new file is 0, and postSafetyCount is 1.
	preSafetyCount := 0

	successDump := `PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE IF NOT EXISTS memories (
  id TEXT PRIMARY KEY, collection TEXT NOT NULL, content TEXT NOT NULL,
  session_id TEXT, tags JSON, metadata JSON, embedding BLOB,
  created_at INTEGER NOT NULL, updated_at INTEGER,
  source_db TEXT, source_id TEXT, promoted_at REAL,
  weight REAL, affinity REAL, is_long_term INTEGER,
  search_dispatched_at INTEGER, last_accessed_unix INTEGER,
  expires_at INTEGER, deleted_at INTEGER
);
INSERT INTO memories (id, collection, content, created_at, updated_at, weight)
VALUES ('restore-safety-new', 'memories', 'fresh row from restore', 1700000010, 1700000010, 60);
COMMIT;
`
	dumpPath := filepath.Join(t.TempDir(), "success.sql")
	require.NoError(t, os.WriteFile(dumpPath, []byte(successDump), 0o600))
	driveStdinYes(t)

	exit := handleRestoreDB([]string{"mpm", dumpPath})
	require.Equal(t, 0, exit, "valid dump should succeed")

	// Successful restore: file still exists at canonical path, new
	// row is present alongside the originals.
	require.NotEmpty(t, sha256File(t, dbPath),
		"successful restore must leave the canonical DB file in place")
	postSafetyCount := restoreSafetySentinelCountAtPath(t, dbPath)
	require.Equal(t, preSafetyCount+1, postSafetyCount,
		"successful restore must add exactly 1 sentinel row to the live DB")

	// The pre-restore copy must NOT exist after a successful restore
	// (the cleanup path removes it).
	if _, err := os.Stat(dbPath + ".pre-restore"); err == nil {
		t.Fatalf(".pre-restore still exists after successful restore — "+
			"cleanup path failed to remove it")
	}
}