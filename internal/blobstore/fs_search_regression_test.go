package blobstore

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBlobSearch_SingleLineBlob is a D4 follow-up regression test (2026-08-25).
//
// Bug: the chunked scanner only attempted matching when a '\n' fell inside
// the current 32 KB chunk. A blob with NO newlines — i.e. every spilled
// single-line JSON payload, the primary thing an agent searches — produced
// zero matches regardless of query. The final unterminated line was also
// dropped entirely for multi-line blobs.
func TestBlobSearch_SingleLineBlob(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	createBlobsTable(t, db)

	fs, err := NewFilesystemBackend(db, t.TempDir(), 24*60*60*1000) // 24h TTL
	require.NoError(t, err)

	// 40 KB single-line JSON-shaped payload (spill envelope shape).
	big := `{"content":"` + strings.Repeat("Y", 40000) + `","status":"spilled"}`
	ptr, err := fs.Put(context.Background(), strings.NewReader(big), Metadata{
		SourceTool: "test", ContentType: "application/json", SizeBytes: int64(len(big)),
	})
	require.NoError(t, err)

	matches, err := fs.Search(context.Background(), ptr.ID, SearchQuery{Query: "YYYY"})
	require.NoError(t, err)
	assert.NotEmpty(t, matches, "substring in a single-line blob must match")

	// Snippet must be bounded so the search response itself cannot spill.
	if len(matches) > 0 {
		assert.LessOrEqual(t, len(matches[0].Snippet), 300,
			"snippet must stay bounded on very long lines")
	}

	// Regex on single line too.
	rx, err := fs.Search(context.Background(), ptr.ID, SearchQuery{Query: `"status":"spilled"`, Regex: true})
	require.NoError(t, err)
	assert.NotEmpty(t, rx, "regex must match on single-line blob")
}

// TestBlobSearch_LineSpanningChunkBoundary covers the second half of the
// same defect: a matching line that crosses the internal 32 KB read-chunk
// boundary was silently never matched.
func TestBlobSearch_LineSpanningChunkBoundary(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	createBlobsTable(t, db)

	fs, err := NewFilesystemBackend(db, t.TempDir(), 24*60*60*1000)
	require.NoError(t, err)

	// Pad to just past the 32 KB chunk edge, then place the needle so it
	// straddles the boundary.
	padding := strings.Repeat("x", 32*1024-10)
	content := padding + "\nNEEDLE_" + strings.Repeat("z", 40) + "_END\nafter\n"
	ptr, err := fs.Put(context.Background(), strings.NewReader(content), Metadata{
		SourceTool: "test", ContentType: "text/plain", SizeBytes: int64(len(content)),
	})
	require.NoError(t, err)

	matches, err := fs.Search(context.Background(), ptr.ID, SearchQuery{Query: "NEEDLE_"})
	require.NoError(t, err)
	require.Len(t, matches, 1, "line spanning the chunk boundary must be matched")
	assert.Equal(t, 2, matches[0].LineNo)

	// Line numbers and offsets stay correct across many lines after a long carry.
	m2, err := fs.Search(context.Background(), ptr.ID, SearchQuery{Query: "after"})
	require.NoError(t, err)
	require.Len(t, m2, 1)
	assert.Equal(t, 3, m2[0].LineNo)
}

// TestBlobSearch_ManyLinesBeyondOneChunk ensures aggregate correctness when
// content spans multiple chunks with matches in each (no duplicated or lost
// lines).
func TestBlobSearch_ManyLinesBeyondOneChunk(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	createBlobsTable(t, db)

	fs, err := NewFilesystemBackend(db, t.TempDir(), 24*60*60*1000)
	require.NoError(t, err)

	var b strings.Builder
	total := 4000 // ~40KB at ~10 bytes/line
	for i := 0; i < total; i++ {
		fmt.Fprintf(&b, "hit-%04d\n", i)
	}
	ptr, err := fs.Put(context.Background(), strings.NewReader(b.String()), Metadata{
		SourceTool: "test", ContentType: "text/plain", SizeBytes: int64(b.Len()),
	})
	require.NoError(t, err)

	matches, err := fs.Search(context.Background(), ptr.ID, SearchQuery{Query: "hit-", MaxMatches: 100})
	require.NoError(t, err)
	assert.Equal(t, 100, len(matches), "max_matches caps results")
	// No duplicates: first match should be hit-0000.
	assert.Contains(t, matches[0].Snippet, "hit-0000")
}
