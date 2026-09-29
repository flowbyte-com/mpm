// Package internal — test helpers (test-only, exported so cmd/mpm/*_test.go
// and internal/tools/*_test.go can share them).
//
// This file is NOT a _test.go file because test helpers must be importable
// from sibling packages (cmd/mpm, internal/tools). The single sql.Open call
// below is whitelisted in TestDatabaseManagerIsOnlyOwnerOfSqlOpen with a
// justifying comment.
package internal

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// testDBCounter is incremented per call to NewTestDM, giving every test
// a uniquely-named shared-cache in-memory database. The DSN form
// `file:<unique>?mode=memory&cache=shared` is the same one already used
// by changelog_mcp_test.go / call_log_to_changelog_test.go /
// changelog_synthesis_test.go — keeps the whole codebase on one DSN
// strategy.
//
// Why cache=shared matters: bare ":memory:" gives each pooled connection
// its OWN private DB. If the schema is initialised on conn A and a query
// later lands on conn B, conn B sees an empty database. cache=shared makes
// every connection to the same `file:NAME` see the same in-memory store.
var testDBCounter int64

// NewTestDM returns a hermetic DatabaseManager backed by an in-memory SQLite
// database. The DB lives only for the duration of the test (registered with
// t.Cleanup) and is isolated from every other test that runs in the same
// `go test` invocation — no cross-test interference, no prod-DB pollution,
// no leftover rows in ~/.mpm/src/db/mpm.db from a test run.
//
// Use this everywhere tests previously constructed a tempfile via
// `t.TempDir() + sql.Open("sqlite3", filepath.Join(tmp, "X.db"))`. The
// consolidation is deliberate: nine near-identical helpers existed before
// this, all drifting in lockstep until one of them broke (the same drift
// pattern as the WakeContextData handler-copy bug caught on 2026-07-06).
// One helper, one DSN strategy, no drift surface.
//
// Memory store callers that need both DM and DB on the same store can do:
//
//	dm := NewTestDM(t)
//	store := &MemoryStore{DM: dm, DB: &SQLiteConnection{DB: dm.SQLDB()}}
//
// (matching the pattern in cmd/mpm/call_evidence_test.go).
func NewTestDM(t *testing.T) *DatabaseManager {
	t.Helper()

	// Unique name per test prevents the shared cache from being shared
	// across tests in the same process — each test gets its own in-memory
	// namespace, then it's garbage-collected when the last connection closes.
	// The mattn/go-sqlite3 DSN semantics: every sql.Open call against the
	// same `file:NAME?mode=memory&cache=shared` joins the same in-memory
	// cache; closing the last reference releases the memory.
	n := atomic.AddInt64(&testDBCounter, 1)
	dsn := fmt.Sprintf("file:mpm-memtest-%d?mode=memory&cache=shared", n)

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("NewTestDM: sql.Open(%q): %v", dsn, err)
	}

	dm := NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		// Close the half-initialised db before failing so we don't leak it.
		db.Close()
		t.Fatalf("NewTestDM: InitSchema: %v", err)
	}

	t.Cleanup(func() {
		// Closing the *sql.DB releases the last reference to the shared
		// cache, letting SQLite reclaim the memory immediately rather than
		// waiting for GC.
		dm.Close()
	})
	return dm
}

// NewTestStoreOnDM returns a MemoryStore bound to an existing hermetic
// DatabaseManager — the sanctioned way for a test to build a
// MemoryStore.
//
// It exists because the obvious alternative, `NewMemoryStore("")`, is
// not a test constructor at all. `NewMemoryStore(_ string)` discards
// its path argument and always resolves `config.GetMPMDir()`, so every
// call points the store at the operator's real database. A store built
// that way is inert only until something calls InitSQLite(), which
// GetByID does automatically when s.DB == nil (memory.go:1185) — at
// which point the full DDL, including four FTS5 virtual tables, is
// written to ~/.mpm/src/db/mpm.db. The repo-wide AST guard in
// test_db_safety_test.go fails any test file that calls it.
//
// Callers that need a MemoryStore with no DatabaseManager behind it
// (to exercise InitSQLite's own DDL) should build the struct literal
// with an explicit SQLiteDBPath under t.TempDir() instead — see
// newTestStore in schema_foundation_test.go.
func NewTestStoreOnDM(t *testing.T, dm *DatabaseManager) *MemoryStore {
	t.Helper()
	if dm == nil {
		t.Fatal("NewTestStoreOnDM: nil DatabaseManager")
	}
	return &MemoryStore{
		DM:           dm,
		DB:           &SQLiteConnection{DB: dm.SQLDB()},
		SQLiteDBPath: dm.DBPath(),
		Collections:  []string{},
	}
}

// NewTestSharedDM opens a hermetic DatabaseManager with a local tmpfile
// DB and an ATTACHed shared tmpfile DB — both rooted at t.TempDir().
//
// Replaces the previous pattern of using NewDatabaseManager("") with
// only MPM_SHARED_DB set, which left the local DB pointed at the real
// workspace (~/.mpm/src/db/mpm.db). Every `make test` run polluted the
// workspace with ~22 leftover rows from TestHandlePromoteToGlobal_* and
// TestPromoteToGlobal_*.
//
// Fix: MPM_WORKSPACE redirects the local DB to a tmpdir. NewDatabaseManager
// derives its dbPath from config.GetMPMDir(), which honours MPM_WORKSPACE.
// The shared DB lives next to it in the same tmpdir. Both DBs (and the
// watchdog.jsonl that NewDatabaseManager derives from dbPath) are
// cleaned up via t.Cleanup. The previous pattern also wrote watchdog
// entries to ~/.mpm/src/db/watchdog.jsonl on every test call, which
// contributed to that file's unbounded growth.
//
// For tests that need to verify the "shared DB not attached" path
// (RecordGlobalRule / PromoteToGlobal refuse to fall through to local
// without a shared DB), see NewTestLocalOnlyDM.
func NewTestSharedDM(t *testing.T) *DatabaseManager {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmpDir)
	t.Setenv("MPM_SHARED_DB", filepath.Join(tmpDir, "shared.db"))

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewTestSharedDM: NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	return dm
}

// NewTestLocalOnlyDM opens a hermetic DatabaseManager with a local
// tmpfile DB and NO shared DB attached. For tests that exercise the
// "shared DB not configured" path. Setting MPM_WORKSPACE keeps these
// tests from polluting the workspace the way the previous pattern did
// (the previous code used NewDatabaseManager("") directly, which
// always opens ~/.mpm/src/db/mpm.db regardless of intent).
func NewTestLocalOnlyDM(t *testing.T) *DatabaseManager {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmpDir)
	t.Setenv("MPM_SHARED_DB", "")

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewTestLocalOnlyDM: NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	return dm
}

// OpenLegacyMemoriesDB builds a raw *sql.DB whose `memories` table
// mirrors the pre-2026-09-04 shape: created_at is nullable,
// INTEGER-affinity, no DEFAULT. schema_migrations is also created
// (empty) so a migration can record its sentinel.
//
// Used by tests that need to exercise the legacy DB path — the
// canonical BaseTables DDL now declares created_at NOT NULL, so any
// test that needs to simulate a NULL row or a pre-fix schema has to
// bypass NewTestDM (which would apply the modern shape). Returns
// the raw *sql.DB so callers can drive migration functions
// directly without going through DatabaseManager.
//
// Not a NewTestDM replacement — use NewTestDM whenever you don't
// specifically need the legacy shape.
func OpenLegacyMemoriesDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:legacy-memories-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("OpenLegacyMemoriesDB: sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Legacy-shape memories table: created_at nullable, no DEFAULT,
	// no NOT NULL. Mirrors the pre-fix d4cfbfa outcome on a database
	// that never inherited the canonical BaseTables DEFAULT clause.
	_, err = db.Exec(`
		CREATE TABLE memories (
			id TEXT PRIMARY KEY,
			collection TEXT,
			content TEXT,
			session_id TEXT,
			tags TEXT,
			metadata TEXT,
			embedding BLOB,
			weight REAL,
			confidence REAL,
			retrieval_priority REAL,
			importance REAL,
			created_at INTEGER,
			updated_at INTEGER,
			deleted_at INTEGER,
			reference_id TEXT,
			content_hash TEXT,
			is_prime_directive INTEGER,
			toxicity_score REAL,
			is_long_term INTEGER,
			reinforcement_count INTEGER,
			last_accessed_at INTEGER,
			expires_at INTEGER,
			source_db TEXT,
			source_id TEXT,
			promoted_at REAL,
			is_global INTEGER
		);
		CREATE TABLE schema_migrations (
			id TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		);
	`)
	if err != nil {
		t.Fatalf("OpenLegacyMemoriesDB: create legacy schema: %v", err)
	}
	return db
}
