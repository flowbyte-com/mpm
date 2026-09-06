package main

// Test helpers for the alpha-4.1 memory-search --json regression
// suite. Kept in a separate file so the main test file stays focused
// on the behavior contract.

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	mpminternal "github.com/flowbyte-com/mpm-core"
)

// capturedRespond is the in-memory sink used by captureRespond so the
// tests can read what handleMemorySearch would have printed. We swap
// the package-level writer (respondOutput / respondError) for the
// duration of the test.
var (
	respondMu       sync.Mutex
	respondCaptured *capturedWriter
)

// capturedWriter wraps bytes.Buffer so the respond() shim can write to
// it without exposing the buffer to every callsite.
type capturedWriter struct {
	buf bytes.Buffer
}

func (c *capturedWriter) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c *capturedWriter) String() string              { return c.buf.String() }

// captureRespond runs fn while capturing what respond() writes. Returns
// the combined stdout/stderr string. Restores the real writers on exit.
func captureRespond(t *testing.T, fn func() int) string {
	t.Helper()
	respondMu.Lock()
	defer respondMu.Unlock()

	prevStdout := os.Stdout
	prevStderr := os.Stderr

	captured := &capturedWriter{}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	os.Stderr = w

	done := make(chan string, 1)
	go func() {
		var all bytes.Buffer
		io.Copy(&all, r)
		done <- all.String()
	}()

	code := fn()

	w.Close()
	out := <-done

	os.Stdout = prevStdout
	os.Stderr = prevStderr
	_ = captured // captureRespond owns the writer swap directly via os.Pipe
	_ = code     // return value of the handler is irrelevant to the captured output
	return out
}

// saveArgv / restoreArgv are placeholders so tests don't break the
// global os.Args. The current handlers do not read os.Args directly
// (they get args from the dispatcher), so the helpers are no-ops.
func saveArgv(argv []string) []string {
	prev := make([]string, len(os.Args))
	copy(prev, os.Args)
	os.Args = argv
	return prev
}

func restoreArgv(prev []string) {
	os.Args = prev
}

// mkMemoryForTest seeds one memory row directly via SQL so the test
// doesn't depend on session/foreign-key state from the runtime test
// harness. The idHint / tag / content triple is what the search
// query will hit against. The seed is fully idempotent: the INSERT
// is guarded with OR IGNORE so a previous run's row is tolerated, the
// UPDATE refreshes content/tags/created_at, and the explicit DELETE
// + INSERT into memories_fts guarantees the FTS5 index reflects the
// latest content even when the rowid changes between runs (because
// INSERT OR IGNORE picks an existing rowid for repeated ids).
func mkMemoryForTest(t *testing.T, store *mpminternal.MemoryStore, idHint, content, tag string) {
	t.Helper()
	if store == nil {
		t.Skip("memory store not initialized (see TestMain)")
	}
	db := store.DB
	if db == nil {
		t.Skip("memory store has no DB handle")
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO memories (id, collection, content, tags, metadata, created_at, weight) VALUES (?, 'memories', ?, ?, ?, strftime('%s','now'), 1.0)`,
		idHint, content, "[\""+tag+"\"]", `{"test_origin":"f005"}`,
	); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if _, err := db.Exec(`UPDATE memories SET created_at = strftime('%s','now'), content = ?, tags = ?, weight = 1.0 WHERE id = ? AND collection = 'memories'`,
		content, "[\""+tag+"\"]", idHint,
	); err != nil {
		t.Fatalf("update memory: %v", err)
	}
	// Force-sync the FTS5 index to match the row we just (re)wrote.
	// The AFTER INSERT / AFTER UPDATE triggers normally handle this,
	// but in tests that share a database with prior runs the rowid
	// may not change (INSERT OR IGNORE keeps the existing rowid), so
	// the trigger's DELETE+INSERT sequence runs but the visible index
	// entry can stay stale. Explicit DELETE+INSERT is cheap and
	// deterministic.
	if _, err := db.Exec(`DELETE FROM memories_fts WHERE rowid IN (SELECT rowid FROM memories WHERE id = ? AND collection = 'memories')`, idHint); err != nil {
		t.Fatalf("clear fts: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
		SELECT rowid, content, collection, session_id, tags FROM memories WHERE id = ? AND collection = 'memories'`, idHint); err != nil {
		t.Fatalf("sync fts: %v", err)
	}
}

// callMPMForMemSearch shells out to the actual mpm binary with a
// fresh temp MPM_WORKSPACE so the test doesn't pollute the workspace.
// The DB at <workspace>/src/db/mpm.db is opened and seeded by the
// caller (via mkMemoryForTestInWorkspace) so the subprocess sees the
// row when it queries.
func callMPMForMemSearch(t *testing.T, workspace string, args ...string) (string, string, error) {
	t.Helper()
	if mpmBin == "" {
		return "", "", fmt.Errorf("mpm binary not built")
	}
	cmd := exec.Command(mpmBin, args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"MPM_WORKSPACE=" + workspace,
		"MPM_SCHEDULER_DISABLED=1",
	}
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	return so.String(), se.String(), err
}

// mkMemoryForTestInWorkspace seeds a memory row directly into the
// production-shaped database at <workspace>/src/db/mpm.db so the
// `mpm memory search` subprocess can find it. The seed mirrors the
// in-process helper but is hermetic — every run creates a fresh DB
// at the workspace path.
func mkMemoryForTestInWorkspace(t *testing.T, workspace, idHint, content, tag string) {
	t.Helper()
	dbDir := filepath.Join(workspace, "src", "db")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	dbPath := filepath.Join(dbDir, "mpm.db")
	dsn := "file:" + dbPath + "?_journal_mode=WAL&_busy_timeout=5000&_fk=true&cache=shared"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open workspace db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Apply the minimal schema needed for the search test. The
	// production binary will re-create the schema when it boots
	// against the same DB, but we need it to exist now so the seed
	// INSERT and FTS5 trigger are valid.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS memories (
			id TEXT PRIMARY KEY,
			collection TEXT NOT NULL,
			content TEXT NOT NULL,
			session_id TEXT,
			tags TEXT,
			metadata TEXT,
			embedding BLOB,
			created_at INTEGER,
			updated_at INTEGER,
			deleted_at INTEGER,
			weight REAL DEFAULT 1.0,
			reinforcement_count INTEGER DEFAULT 0,
			reference_id TEXT,
			retrieval_priority REAL DEFAULT 0.5,
			importance REAL DEFAULT 0.5,
			confidence REAL DEFAULT 0.5,
			content_hash TEXT
		)`); err != nil {
		t.Fatalf("create memories: %v", err)
	}
	if _, err := db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content, collection, session_id UNINDEXED, tags, tokenize='porter unicode61')`); err != nil {
		t.Fatalf("create fts: %v", err)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO memories (id, collection, content, tags, metadata, created_at, weight) VALUES (?, 'memories', ?, ?, ?, strftime('%s','now'), 1.0)`,
		idHint, content, "[\""+tag+"\"]", `{"test_origin":"f005-subproc"}`,
	); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO memories_fts(rowid, content, collection, session_id, tags)
		SELECT rowid, content, collection, session_id, tags FROM memories WHERE id = ?`, idHint); err != nil {
		t.Fatalf("seed fts: %v", err)
	}
}

// stripJSONFlag was the test-side mirror of the production
// stripMemoryFlagToken. Stage S2 of the CLI refactor (2026-09-06)
// migrated the production scrubber to the canonical ExtractJSONFlag
// helper. The test-side mirror was dead code (no callers in the test
// corpus) and has been removed as part of the duplicate-extraction
// cleanup. See cli_args_json.go for the canonical --json contract
// and cli_args_json_test.go for the test coverage that replaces this
// pin.
