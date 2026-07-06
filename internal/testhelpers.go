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
