package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression test for the 2026-08-13 audit finding: handleBackup and
// handleRestoreDB used to call dm.Close() on the package-level singleton
// (dbManager) right after extracting its DBPath(). Because dbManager is
// initialised exactly once via sync.Once, every subsequent CLI command in
// the same process received the permanently-closed handle. A single
// `mpm backup` or `mpm restore-db` invocation broke every later command.
//
// Post-fix: both handlers extract what they need from the singleton but
// leave it open. This test enforces that invariant — after invoking each
// handler, the singleton must still serve queries.

// setupBackupSingletonTest wires the dbManager singleton to a hermetic test
// DM (same pattern as handlers_memory_add_test.go::setupMemoryAddTest) and
// returns it so the test can query back to verify the singleton survives.
func setupBackupSingletonTest(t *testing.T) {
	t.Helper()
	dm := newTestDMForCmd(t)
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	// Consume sync.Once before swapping in the test DM, mirroring
	// setupMemoryAddTest's pattern.
	_ = getDB()
	prev := dbManager
	prevErr := dbManagerInitErr
	dbManager = dm
	dbManagerInitErr = nil
	t.Cleanup(func() {
		dbManager = prev
		dbManagerInitErr = prevErr
	})
}

// TestHandleBackup_LeavesSingletonAlive is the core regression. Pre-fix,
// handleBackup called dm.Close() on the singleton at the top of the
// function — the very first thing it did. The test invokes handleBackup
// with an explicit output path (so it doesn't try to write into the
// canonical workspace), then asserts that the same singleton returned by
// getDB() can still execute a SELECT. A closed *sql.DB returns
// "sql: database is closed" from any operation; the test asserts against
// that exact string to make the failure mode obvious.
func TestHandleBackup_LeavesSingletonAlive(t *testing.T) {
	setupBackupSingletonTest(t)
	dm := getDBConcrete()
	require.NotNil(t, dm)

	// handleBackup invokes `sqlite3 .dump` — if the CLI is missing it
	// returns 1 but does NOT close the singleton (post-fix). Either
	// outcome is fine for this regression test; the singleton must
	// survive either way.
	outPath := filepath.Join(t.TempDir(), "backup.sql")
	_ = handleBackup([]string{"mpm", "backup", outPath})

	// Critical assertion: singleton still works.
	var n int
	row := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories`)
	err := row.Scan(&n)
	require.NoError(t, err,
		"singleton must remain queryable after handleBackup — "+
			"pre-fix, dm.Close() left it permanently broken")
}

// TestHandleRestoreDB_LeavesSingletonAlive mirrors the backup regression
// for the restore path. handleRestoreDB does NOT actually need to open the
// dump file successfully for this test — the bug was in the prefix
// (extracting dbDir/dbPath from the singleton), not the dump execution.
func TestHandleRestoreDB_LeavesSingletonAlive(t *testing.T) {
	setupBackupSingletonTest(t)
	dm := getDBConcrete()
	require.NotNil(t, dm)

	// Build a minimal valid dump in the DB directory (the handler
	// canonicalises and validates the path is inside dbDir before
	// attempting to read).
	dumpPath := filepath.Join(t.TempDir(), "restore.sql")
	require.NoError(t, os.WriteFile(dumpPath, []byte("CREATE TABLE IF NOT EXISTS dummy(x);"), 0o600))

	// handleRestoreDB may fail at the user-confirmation prompt (we
	// don't simulate stdin), but the critical path is the prefix that
	// used to call dm.Close(). After the fix, that prefix must not
	// touch the singleton's lifetime.
	_ = handleRestoreDB([]string{"mpm", "restore-db", dumpPath})

	// Critical assertion: singleton still works.
	var n int
	row := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories`)
	err := row.Scan(&n)
	require.NoError(t, err,
		"singleton must remain queryable after handleRestoreDB — "+
			"pre-fix, dm.Close() left it permanently broken")
}

// TestFlushWal_PassesForeignKeysPragma verifies the second audit finding:
// the pre-H-4 flushWal opened its own transient connection with bare
// `dbPath` (no DSN query params), leaving foreign_keys=OFF. With FK
// enforcement off, a WAL checkpoint could silently drop rows that
// violate FK constraints.
//
// Post-H-4 flushWal does not open its own connection: it routes through
// the singleton DatabaseManager (which holds LOCK_SH as part of the
// maintenance lease). The DSN-side FK guard is therefore exercised
// separately against SqliteWriteDSN below — the helper used by every
// non-DM write path that still opens its own connection.
func TestFlushWal_PassesForeignKeysPragma(t *testing.T) {
	// Use a file-based DM (not NewTestDM which is in-memory) so DBPath()
	// returns a real path — the empty-path guard returns ":memory:",
	// which has no FK pragma and would mask the regression we're
	// checking for.
	tmpDir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmpDir)
	dm, err := mpminternal.NewDatabaseManager("")
	require.NoError(t, err)
	t.Cleanup(func() { _ = dm.Close() })
	require.NotEmpty(t, dm.DBPath(),
		"file-based DM must produce a non-empty DMPath — got %q "+
			"(workspace=%q)", dm.DBPath(), config.GetMPMDir())

	// Post-H-4 flushWal takes the DM (CoreDB interface) and runs the
	// checkpoint on the singleton's *sql.DB. The LOCK_SH held by the
	// DM is what blocks a concurrent restore-db / shred-database from
	// racing the dump. The transient sql.Open path that previously
	// bypassed the lease has been removed.
	require.NoError(t, flushWal(dm))

	// Open a fresh connection using the same exported helper flushWal
	// USED TO use internally, and read the pragma to confirm the DSN
	// still enables FK. SqliteWriteDSN remains the single source of
	// truth for FK=ON for any write path that opens its own connection.
	db, err := sql.Open("sqlite3", mpminternal.SqliteWriteDSN(dm.DBPath()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var fk int
	require.NoError(t, db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk))
	assert.Equal(t, 1, fk,
		"SqliteWriteDSN must enable foreign_keys=ON — "+
			"pre-fix, FK enforcement was off and any write path "+
			"that opens its own connection could silently drop "+
			"FK-violating rows")
}

// TestSqliteWriteDSN_EmptyPathReturnsMemory exercises the defensive guard
// added by the 2026-08-14 audit. mattn/go-sqlite3 interprets a DSN
// starting with '?' as "create a file with that literal name", so an
// unguarded empty path silently creates a file named "?_foreign_keys=1".
// SqliteWriteDSN("") must instead return ":memory:".
func TestSqliteWriteDSN_EmptyPathReturnsMemory(t *testing.T) {
	assert.Equal(t, ":memory:", mpminternal.SqliteWriteDSN(""),
		"empty path must map to :memory: — otherwise mattn creates a "+
			"literal '?_foreign_keys=1' file in the cwd")
}
