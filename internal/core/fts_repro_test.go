// fts_repro_test.go — reproduces the two pre-existing SearchReferences FTS-path
// findings from the alpha-final pass. Runs against a hermetic in-memory DB
// so it has no state leakage.
//
// The findings to verify:
//   (a) `ORDER BY bm25(references_fts)` raises "no such column: references_fts"
//       because bm25() is only in scope inside the FTS5 query that produces
//       the rowset, not on a subquery like `SELECT rowid FROM references_fts`.
//   (b) `WHERE id IN (SELECT rowid FROM references_fts ...)` returns 0 rows
//       because reference_docs.id is TEXT and references_fts.rowid is INTEGER
//       — the type mismatch causes the lookup to always miss.
package internal

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// setupFtsReproDB creates a hermetic DB with the FTS schema mirrored from
// internal/core/db.go (lines 2388-2463). Self-contained so the reproduction
// is independent of the full MPM schema.
func setupFtsReproDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:?_pragma=fts5(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	stmts := []string{
		`CREATE TABLE reference_docs (
			id TEXT PRIMARY KEY,
			title TEXT,
			file_path TEXT,
			source_type TEXT,
			content TEXT,
			tags TEXT,
			total_chunks INTEGER,
			last_indexed TEXT,
			created_at TEXT
		)`,
		`CREATE VIRTUAL TABLE references_fts USING fts5(title, content, tags, tokenize='porter unicode61')`,
		`CREATE TRIGGER references_ai AFTER INSERT ON reference_docs BEGIN
			INSERT INTO references_fts(rowid, title, content, tags) VALUES (new.rowid, new.title, new.content, new.tags);
		END`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v\nstmt: %s", err, s)
		}
	}
	// Seed one reference with a unique sentinel token.
	if _, err := db.Exec(
		`INSERT INTO reference_docs (id, title, content, tags, last_indexed, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"ref-fts-sentinel", "ALPHA-REFERENCE-FTS-2026",
		"unique sentinel content for FTS reproduction",
		"[]", "1735689600", "2026-08-29",
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return db
}

// TestFtsRepro_BugA_Bm25OutsideScope confirms finding (a):
// the production query pattern at web_db.go:1035 uses
//
//   ORDER BY bm25(references_fts)
//
// after a subquery — bm25() is only defined inside the FTS5 virtual table
// itself. Running the exact production query string against a hermetic DB
// MUST raise "no such column: references_fts" — the reproduction proves
// the bug is real, not a test artifact.
func TestFtsRepro_BugA_Bm25OutsideScope(t *testing.T) {
	db := setupFtsReproDB(t)

	const productionQuery = `SELECT id, title FROM reference_docs
		WHERE id IN (SELECT rowid FROM references_fts WHERE references_fts MATCH ?)
		ORDER BY bm25(references_fts) LIMIT ?`

	_, err := db.Query(productionQuery, `"ALPHA-REFERENCE-FTS-2026"*`, 20)
	if err == nil {
		t.Fatal("expected error from bm25() outside FTS5 query scope, got nil — bug was already fixed")
	}
	if !strings.Contains(err.Error(), "no such column") {
		t.Fatalf("expected 'no such column' error, got: %v", err)
	}
	t.Logf("finding (a) reproduced: %v", err)
}

// TestFtsRepro_BugB_IdRowidTypeMismatch confirms finding (b):
// the production WHERE clause `id IN (SELECT rowid FROM references_fts)`
// joins reference_docs.id (TEXT) with references_fts.rowid (INTEGER).
// Even WITHOUT the bm25 issue, this type mismatch means no row matches
// — `id = <integer>` against a TEXT column is always false.
func TestFtsRepro_BugB_IdRowidTypeMismatch(t *testing.T) {
	db := setupFtsReproDB(t)

	// Get the FTS-side rowid that the trigger assigned.
	var ftsRowid int64
	if err := db.QueryRow(`SELECT rowid FROM references_fts LIMIT 1`).Scan(&ftsRowid); err != nil {
		t.Fatalf("get fts rowid: %v", err)
	}
	t.Logf("FTS rowid = %d (integer), reference_docs.id = %q (text)", ftsRowid, "ref-fts-sentinel")

	// Direct probe: the production query (without bm25) should return the row.
	rows, err := db.Query(
		`SELECT id FROM reference_docs WHERE id IN (SELECT rowid FROM references_fts WHERE references_fts MATCH ?)`,
		`"ALPHA-REFERENCE-FTS-2026"*`,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if count != 0 {
		t.Fatalf("expected 0 rows (text/int mismatch), got %d — bug was already fixed", count)
	}
	t.Logf("finding (b) reproduced: WHERE id IN (SELECT rowid FROM fts) returned 0 rows")
}
