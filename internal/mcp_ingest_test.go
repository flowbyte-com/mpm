package internal

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// freshReferenceDM returns an isolated DatabaseManager wired to a fresh
// sqlite file in t.TempDir(). Mirrors the helper in reliability_sprint_test.go
// but does not depend on the synthesis worker schema — keeps this test
// focused on the reference ingestion + search contract.
func freshReferenceDM(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "mcp_ingest_test.db")
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	dm := NewDatabaseManagerForDB(db)
	require.NoError(t, dm.InitSchema())
	return dm
}

// TestMCPAddReferenceIsSearchable locks in the fix for the bug where
// DatabaseManager.AddReferenceFromFile wrote to the legacy JSON store
// while SearchReferenceChunks queried the chunked SQLite store — making
// MCP-ingested documents invisible to MCP search.
//
// The contract: anything that goes through AddReferenceFromFile (and its
// extension AddReferenceFromFileWith) MUST be retrievable via
// SearchReferenceChunks on the same DatabaseManager. Regression here
// means a future refactor has reintroduced the dual-store divergence.
func TestMCPAddReferenceIsSearchable(t *testing.T) {
	dm := freshReferenceDM(t)
	defer dm.Close()

	// Write a recognisable content sample to disk. The query term is
	// "zylophlox" — a nonsense word chosen to be unique across runs and
	// unlikely to collide with any other test fixture.
	const marker = "zylophlox"
	body := strings.Join([]string{
		"Chapter One — Discovery.",
		"",
		"The expedition uncovered a " + marker + " mineral beneath the third ridge.",
		"It hummed faintly when the wind shifted, and the cartographer made note of it.",
		"",
		"Chapter Two — Departure.",
		"",
		"They packed the specimens carefully and set out before the storm arrived.",
	}, "\n")
	src := filepath.Join(t.TempDir(), "discovery.md")
	require.NoError(t, os.WriteFile(src, []byte(body), 0o644))

	// Ingest through the public MCP-shaped entry point.
	res, err := dm.AddReferenceFromFile(src, "Discovery Expedition")
	require.NoError(t, err)
	require.Equal(t, true, res["success"])
	docID, _ := res["id"].(string)
	require.NotEmpty(t, docID, "AddReferenceFromFile returned no doc id")
	chunks, _ := res["total_chunks"].(int)
	require.Greater(t, chunks, 0, "expected at least one chunk")

	// Search via the same DatabaseManager. With FTS5 unavailable in this
	// test driver the LIKE fallback must still surface the chunk whose
	// content contains the marker.
	hits, err := dm.SearchReferenceChunks(marker, 10)
	require.NoError(t, err)
	require.NotEmpty(t, hits, "marker %q not found — MCP add path is not visible to search", marker)

	var foundDocID string
	for _, h := range hits {
		if id, ok := h["doc_id"].(string); ok && id == docID {
			foundDocID = id
			break
		}
	}
	assert.Equal(t, docID, foundDocID, "search hit must belong to the doc just ingested")
}

// TestMCPAddReferenceWithTagsAndReason exercises the extended entry point
// to confirm tags and import_reason survive the chunked-SQLite write path.
// The legacy JSON store accepted both as fields on the in-memory struct;
// the unified path must persist them in the reference_docs columns.
func TestMCPAddReferenceWithTagsAndReason(t *testing.T) {
	dm := freshReferenceDM(t)
	defer dm.Close()

	src := filepath.Join(t.TempDir(), "annotated.md")
	require.NoError(t, os.WriteFile(src, []byte("first observation about quendrelite crystals\n"), 0o644))

	res, err := dm.AddReferenceFromFileWith(
		src,
		"Annotated Sample",
		[]string{"geology", "expedition"},
		"admitted: parallels prior expedition notes on resonance",
		128,
	)
	require.NoError(t, err)
	docID, _ := res["id"].(string)

	got, err := dm.GetReference(docID)
	require.NoError(t, err)
	assert.Equal(t, "admitted: parallels prior expedition notes on resonance", got["import_reason"])
	assert.Contains(t, got["tags"], "geology")
	assert.Contains(t, got["tags"], "expedition")
	assert.Greater(t, got["total_chunks"].(int), 0)
}

// TestParseReferenceFileDispatch confirms the extension switch handles the
// four supported formats plus the plain-text fallback. The dispatch logic
// lives in parseReferenceFile (unexported) and must agree with the CLI
// handleRefAdd parser — if you add a new branch here, add it there too.
func TestParseReferenceFileDispatch(t *testing.T) {
	cases := []struct {
		ext         string
		wantNonEmpty bool
		wantErr     bool
	}{
		{".txt", true, false},
		{".md", true, false},
		{".html", true, false},
		{".unknown", true, false}, // best-effort plain text fallback
		{".pdf", false, true},     // nonexistent PDF file
	}

	for _, tc := range cases {
		t.Run(tc.ext, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "sample"+tc.ext)
			require.NoError(t, os.WriteFile(src, []byte("hello world"), 0o644))

			out, err := parseReferenceFile(src)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.wantNonEmpty {
				require.NotEmpty(t, out)
			}
		})
	}
}