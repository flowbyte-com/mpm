// f20_1_search_references_fts_regression_test.go — F20-1 alpha-final regression.
//
// F20-1: SearchReferences had two pre-existing FTS-path bugs that made the
// FTS5 branch effectively dead for alpha users:
//
//   (a) `ORDER BY bm25(references_fts)` raised
//       "no such column: references_fts" — bm25() is an FTS5 auxiliary
//       function only in scope inside the FTS5 virtual table query
//       itself; using it on a subquery like `SELECT rowid FROM fts`
//       is a SQLite syntax error.
//
//   (b) `WHERE id IN (SELECT rowid FROM references_fts)` returned 0
//       rows even on a perfect match. reference_docs.id is TEXT and
//       references_fts.rowid is INTEGER — the type mismatch made the
//       lookup always miss. This was independent of F19-1 (which only
//       added the freshness field to the row map).
//
// The previous fix that landed (a brittle workaround that dropped the FTS
// table at test time) only masked bug (a) for the projection contract.
// A real alpha user invoking `mpm call mpm_references search` got an
// error (a) or empty results (b) — discovery was broken.
//
// The correction: use a CTE that wraps the FTS5 match so bm25() is in
// scope, and join reference_docs on its implicit rowid (the column the
// references_ai trigger uses when populating references_fts.rowid).
// This mirrors the proven pattern from SearchReferenceChunks
// (web_db.go:1096), which never had either bug.
//
// The F19-1 freshness projection contract is preserved unchanged —
// the row map builder below the query is untouched.
package internal

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedReferenceForFts inserts one reference via the public AddReference path
// so the references_ai trigger (db.go:2456) populates references_fts
// correctly. This is the only way to exercise the FTS branch.
func seedReferenceForFts(t *testing.T, dm *DatabaseManager, doc *ReferenceDoc) {
	t.Helper()
	require.NoError(t, dm.AddReference(doc, nil))
}

// TestF20_1_SearchReferences_FTSBranchReturnsHits confirms the FTS5
// branch (when references_fts exists and is populated) actually
// returns rows. Pre-fix: error (a) or 0 rows (b). Post-fix: rows
// returned with the same shape as the LIKE-fallback branch.
func TestF20_1_SearchReferences_FTSBranchReturnsHits(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	uniqueSentinel := "ALPHA-REFERENCE-FTS-2026"
	seedReferenceForFts(t, dm, &ReferenceDoc{
		ID:          "ref-f201-sentinel",
		Title:       uniqueSentinel,
		SourcePath:  "/tmp/f201-sentinel.md",
		SourceType:  "markdown",
		Content:     "Unique sentinel content for F20-1 reproduction.",
		LastIndexed: fmt.Sprintf("%d", time.Now().Unix()),
	})

	// Verify the FTS table actually has the row (via the trigger).
	var ftsCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM references_fts WHERE references_fts MATCH ?`,
		`"`+uniqueSentinel+`"*`,
	).Scan(&ftsCount))
	require.Equal(t, 1, ftsCount, "FTS row missing — trigger didn't fire")

	// Pre-fix: this query returned 0 rows (bug b) OR errored (bug a).
	// Post-fix: returns 1 row with the unique sentinel.
	results, err := dm.SearchReferences(uniqueSentinel, 20)
	require.NoError(t, err, "SearchReferences must not error")
	require.NotEmpty(t, results, "FTS branch must surface the seeded row")

	var found map[string]interface{}
	for _, r := range results {
		if id, _ := r["id"].(string); id == "ref-f201-sentinel" {
			found = r
			break
		}
	}
	require.NotNil(t, found, "seeded row must appear in FTS results: %+v", results)

	// Freshness projection contract (F19-1) must still hold — the
	// row map builder is unchanged, so this is just a regression
	// guard that the FTS fix didn't drop the freshness field.
	freshness, ok := found["freshness"].(string)
	require.True(t, ok, "freshness field missing or wrong type — F19-1 regression")
	require.NotEmpty(t, freshness, "freshness field empty — F19-1 regression")
	assert.Equal(t, "current", freshness,
		"freshly indexed ref should classify as current; got %q", freshness)
}

// TestF20_1_SearchReferences_RankingPrefersTitleMatch confirms the
// bm25() ordering works (bug a's fix). Two refs with the sentinel in
// different positions: title-match should rank higher than body-only-match.
func TestF20_1_SearchReferences_RankingPrefersTitleMatch(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	// Title match.
	seedReferenceForFts(t, dm, &ReferenceDoc{
		ID:          "ref-f201-title",
		Title:       "FINDME-F20-1 title hit",
		SourcePath:  "/tmp/f201-title.md",
		SourceType:  "markdown",
		Content:     "irrelevant body",
		LastIndexed: fmt.Sprintf("%d", time.Now().Unix()),
	})
	// Body only — same query, different rank expected.
	seedReferenceForFts(t, dm, &ReferenceDoc{
		ID:          "ref-f201-body",
		Title:       "unrelated heading",
		SourcePath:  "/tmp/f201-body.md",
		SourceType:  "markdown",
		Content:     "this body mentions FINDME-F20-1 once",
		LastIndexed: fmt.Sprintf("%d", time.Now().Unix()),
	})

	results, err := dm.SearchReferences("FINDME-F20-1", 20)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(results), 2, "both seeded rows must appear")

	// Find the position of each.
	pos := map[string]int{}
	for i, r := range results {
		if id, _ := r["id"].(string); id == "ref-f201-title" || id == "ref-f201-body" {
			pos[id] = i
		}
	}
	require.Contains(t, pos, "ref-f201-title")
	require.Contains(t, pos, "ref-f201-body")

	// Title match should rank before body match (bm25 lower = better).
	assert.Less(t, pos["ref-f201-title"], pos["ref-f201-body"],
		"title-match must rank before body-match (bm25 ordering broken); got positions %+v", pos)
}

// TestF20_1_SearchReferences_LikeFallbackUnchanged confirms the LIKE
// branch still works (sanity: the FTS fix didn't regress the simpler
// path). SearchReferences uses LIKE when references_fts is missing.
func TestF20_1_SearchReferences_LikeFallbackUnchanged(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	seedReferenceForFts(t, dm, &ReferenceDoc{
		ID:          "ref-f201-like",
		Title:       "FINDME-LIKE-F20-1",
		SourcePath:  "/tmp/f201-like.md",
		SourceType:  "markdown",
		LastIndexed: fmt.Sprintf("%d", time.Now().Unix()),
	})

	// Force LIKE path by dropping the FTS table.
	_, _ = dm.SQLDB().Exec(`DROP TABLE IF EXISTS references_fts`)

	results, err := dm.SearchReferences("FINDME-LIKE-F20-1", 20)
	require.NoError(t, err)
	require.NotEmpty(t, results, "LIKE fallback must surface the seeded row")

	found := false
	for _, r := range results {
		if id, _ := r["id"].(string); id == "ref-f201-like" {
			found = true
			// Freshness projection must still hold on the LIKE path.
			if freshness, _ := r["freshness"].(string); freshness == "" {
				t.Error("freshness missing on LIKE fallback")
			}
		}
	}
	assert.True(t, found, "seeded row must appear in LIKE fallback results")
}

// TestF20_1_SearchReferences_NoSilentCoercion confirms that a query
// matching NOTHING (zero hits) returns an empty slice, not an error
// and not a synthetic placeholder. Pre-fix bug (b) returned 0 rows
// silently even when matches existed — the opposite silent failure.
// Post-fix: zero matches → empty slice + nil error. False success
// is the silent-coercion class the audit forbids.
func TestF20_1_SearchReferences_NoSilentCoercion(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	seedReferenceForFts(t, dm, &ReferenceDoc{
		ID:          "ref-f201-noop",
		Title:       "Some unrelated title",
		SourcePath:  "/tmp/f201-noop.md",
		SourceType:  "markdown",
		Content:     "no overlap with the query sentinel",
		LastIndexed: fmt.Sprintf("%d", time.Now().Unix()),
	})

	results, err := dm.SearchReferences("ZZZ-NEVER-MATCHES-12345", 20)
	require.NoError(t, err, "zero-hit query must not error")
	assert.Empty(t, results, "zero-hit query must return empty slice, got %d rows", len(results))

	// Every returned row (none here, but the contract) must carry freshness.
	for i, r := range results {
		freshness, _ := r["freshness"].(string)
		if freshness == "" {
			t.Errorf("row %d: freshness field empty", i)
		}
	}
}

// TestF20_1_SearchReferences_QueryWithSpecialChars confirms the FTS
// fix survives query strings with double quotes (which must be escaped
// to avoid breaking the MATCH expression). Pre-fix would crash on
// queries like `mpm call mpm_references search 'a " b'`.
func TestF20_1_SearchReferences_QueryWithSpecialChars(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	seedReferenceForFts(t, dm, &ReferenceDoc{
		ID:          "ref-f201-quote",
		Title:       "QuoteEdge F20-1",
		SourcePath:  "/tmp/f201-quote.md",
		SourceType:  "markdown",
		Content:     "QuoteEdge body",
		LastIndexed: fmt.Sprintf("%d", time.Now().Unix()),
	})

	// Insert a literal double-quote in the query — must be escaped.
	results, err := dm.SearchReferences(`QuoteEdge "quoted"`, 20)
	require.NoError(t, err, "query with double-quote must not error — escaping bug")
	// No assertion on result count: zero hits → nil slice in Go is
	// acceptable. The contract pinned here is "must not error" —
	// pre-fix the FTS query could panic on unescaped quotes.
	_ = results
}

// helper used by the above tests.
func init() {
	// Avoid unused-import lint failure on `strings` in helpers.
	_ = strings.Contains
}
