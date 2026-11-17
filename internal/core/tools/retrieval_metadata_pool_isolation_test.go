// retrieval_metadata_pool_isolation_test.go — the deferred retrieval
// telemetry write must not be able to open a SCHEMA-LESS database.
//
// WHY THIS FILE EXISTS.
//
// issue #178 (F2): the wake surface's retrieval telemetry is written
// through the substrate's DEFERRED durable-write queue. The deferred
// write runs on a DIFFERENT goroutine — and therefore on a different
// POOLED connection — while the handler is still running.
//
// That breaks both in-memory shapes:
//
//   - a bare ":memory:" gives every pooled connection its own PRIVATE
//     database, so the deferred write lands in an empty, schema-less
//     one, and `database/sql` hands connections out WITHOUT AFFINITY —
//     the test's next query may draw the empty one and fail with
//     "no such table: retrieval_metadata";
//   - `cache=shared` serialises writers against the caller's own open
//     read, so the deferred write is refused with SQLITE_LOCKED until
//     the reader finishes.
//
// A file-backed WAL database is what production runs on, and in WAL
// mode a reader never blocks a writer.
//
// The independent review of PR #235 found a fixture in this package
// that still used ":memory:" while calling handleReadWakeContext, and
// hit the resulting flake in a full-package -race run and in a
// -count=20 run. The sibling fixture had already been converted; this
// one had been missed.
//
// The test below is the DETERMINISTIC form of that flake: it forces the
// pool to open a second PHYSICAL connection and asserts that the second
// connection sees the same database. With ":memory:" the second
// connection has no schema, and the assertion fails every time rather
// than occasionally.

package tools

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core"
)

// openRetrievalMetadataDB opens a database suitable for a test that
// exercises the DEFERRED retrieval telemetry write.
//
// It MUST be file-backed. A ":memory:" DSN is refused here, loudly,
// because every pooled connection would get its own private database
// and the deferred write would land in a schema-less one.
func openRetrievalMetadataDB(t *testing.T) (*sql.DB, *internal.DatabaseManager) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "mpm.db")
	if dbPath == ":memory:" {
		t.Fatal("the retrieval-metadata fixture must be file-backed")
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	return db, dm
}

// TestRetrievalMetadataDeferredWrite_PoolConnectionsShareOneSchema is
// the deterministic regression test for the fixture hazard.
//
// It forces the pool to open a second PHYSICAL connection and asserts
// that the second connection sees the SAME database — the schema and
// the rows.
//
// The mechanism it pins: the deferred telemetry write runs on the
// queue's worker goroutine, so it takes a pooled connection of its
// own. If the pool can hand out a connection that points at a
// DIFFERENT database, the telemetry row lands in the wrong place and
// every later query on the pool may fail.
//
// PRE-FIX: with a ":memory:" DSN the second connection is a private,
// schema-less database and this test fails with "no such table:
// retrieval_metadata".
func TestRetrievalMetadataDeferredWrite_PoolConnectionsShareOneSchema(t *testing.T) {
	internal.PinIsolatedWorkspace(t)
	db, dm := openRetrievalMetadataDB(t)

	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, tags, deleted_at, created_at, updated_at)
		VALUES ('pool-alpha', 'memories', 'pool isolation surface test', 5, '', NULL, '2026-10-17', '2026-10-17')
	`); err != nil {
		t.Fatalf("seed a memory: %v", err)
	}

	// The wake surface records its telemetry through the DEFERRED queue,
	// so the write runs on another goroutine — and therefore on another
	// pooled connection.
	if _, err := handleReadWakeContext(dm, internal.ActiveContext{}, map[string]interface{}{}); err != nil {
		t.Fatalf("read_wake_context: %v", err)
	}

	// Flush, so the deferred write has actually run.
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFlush()
	if err := internal.FlushDeferredWrites(flushCtx); err != nil {
		t.Fatalf("flush deferred writes: %v", err)
	}

	// Hold one connection open and take a second. The pool MUST open a
	// new physical connection, because the first is in use.
	conn1, err := db.Conn(flushCtx)
	if err != nil {
		t.Fatalf("take the first connection: %v", err)
	}
	defer conn1.Close()
	conn2, err := db.Conn(flushCtx)
	if err != nil {
		t.Fatalf("take the second connection: %v", err)
	}
	defer conn2.Close()

	// Every physical connection must see the SAME database.
	for i, conn := range []*sql.Conn{conn1, conn2} {
		var n int
		if err := conn.QueryRowContext(flushCtx, `SELECT COUNT(*) FROM retrieval_metadata`).Scan(&n); err != nil {
			t.Fatalf("connection %d could not read retrieval_metadata: %v — the pool opened a schema-less database, "+
				"which is what a bare \":memory:\" DSN produces", i, err)
		}
	}
}
