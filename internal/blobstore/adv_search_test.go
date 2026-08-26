package blobstore

// Stage 7 adversarial verification: blob Search state-machine boundaries.
// Covers chunk-boundary matches, unterminated lines, bounded snippets, and
// matches inside a maxBytes-truncated scan window (single-line spill JSON).
import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newSearchFixture(t *testing.T) *FilesystemBackend {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "blobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE blobs (
			id             TEXT PRIMARY KEY,
			source_tool    TEXT NOT NULL,
			source_call_id TEXT,
			session_id     TEXT,
			size_bytes     INTEGER NOT NULL,
			content_type   TEXT NOT NULL DEFAULT 'application/json',
			created_at     INTEGER NOT NULL,
			expires_at     INTEGER NOT NULL,
			checksum       TEXT
		)`); err != nil {
		t.Fatal(err)
	}
	bs, err := NewFilesystemBackend(db, t.TempDir(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return bs
}

func TestAdvSearchChunkBoundaries(t *testing.T) {
	bs := newSearchFixture(t)
	ctx := context.Background()

	const B = 32 * 1024 // scanner buffer size
	cases := []struct {
		name    string
		content string
		query   string
		wantMin int // minimum expected match count
	}{
		{
			name:    "match spans 32767/32768 boundary mid-line",
			content: strings.Repeat("A", 40000) + "NEEDLE" + strings.Repeat("B", 10) + "\n",
			query:   "NEEDLE",
			wantMin: 1,
		},
		{
			name:    "match entirely before boundary",
			content: strings.Repeat("x", 1000) + "\nNEEDLE\n" + strings.Repeat("y", B),
			query:   "NEEDLE",
			wantMin: 1,
		},
		{
			name:    "match starts exactly at 32K boundary",
			content: strings.Repeat("q", B) + "NEEDLE\n",
			query:   "NEEDLE",
			wantMin: 1,
		},
		{
			name:    "match ends exactly at boundary minus 1",
			content: strings.Repeat("q", B-6) + "NEEDLE\n" + strings.Repeat("z", 50),
			query:   "NEEDLE",
			wantMin: 1,
		},
		{
			name:    "single enormous line no newline (unterminated final line)",
			content: strings.Repeat("w", 70000) + "NEEDLE" + strings.Repeat("w", 100),
			query:   "NEEDLE",
			wantMin: 1,
		},
		{
			name:    "many short lines multiple matches",
			content: strings.Repeat("line NEEDLE here\n", 500),
			query:   "NEEDLE",
			wantMin: 100,
		},
		{
			name:    "empty lines interleaved",
			content: "\n\n\nNEEDLE\n\n\nNEEDLE\n\n",
			query:   "NEEDLE",
			wantMin: 2,
		},
		{
			name:    "unicode content match after boundary",
			content: strings.Repeat("é", B/2) + "\nÜnïcodé—NEEDLE—日本語\n",
			query:   "NEEDLE",
			wantMin: 1,
		},
		{
			name:    "1-byte blob",
			content: "Z",
			query:   "Z",
			wantMin: 1,
		},
		{
			name:    "match crossing maxBytes scan ceiling is not required but must not panic",
			content: strings.Repeat("n", 300000) + "\n",
			query:   "NEEDLE",
			wantMin: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := Metadata{SourceTool: "adv", SizeBytes: int64(len(tc.content)), ContentType: "text/plain"}
			id, err := bs.Put(ctx, strings.NewReader(tc.content), meta)
			if err != nil {
				t.Fatalf("put: %v", err)
			}
			matches, err := bs.Search(ctx, id.ID, SearchQuery{Query: tc.query, MaxMatches: 100})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(matches) < tc.wantMin {
				t.Fatalf("got %d matches, want >= %d (%+v)", len(matches), tc.wantMin, matches)
			}
			for _, m := range matches {
				if len(m.Snippet) > 240+3 {
					t.Errorf("snippet unbounded: %d bytes", len(m.Snippet))
				}
				if m.ByteOffset < 0 || int64(m.ByteOffset) >= meta.SizeBytes+1 {
					t.Errorf("byte offset out of range: %d", m.ByteOffset)
				}
			}
		})
	}
}

func TestAdvSnippetBoundedOnSpillShapedBlob(t *testing.T) {
	bs := newSearchFixture(t)
	ctx := context.Background()
	// single-line JSON spill-shaped payload with the query deep inside
	huge := `{"status":"spilled","preview":"` + strings.Repeat("p", 60000) + `"}` + "\n"
	meta := Metadata{SourceTool: "adv", SizeBytes: int64(len(huge)), ContentType: "application/json"}
	id, _ := bs.Put(ctx, strings.NewReader(huge), meta)
	matches, err := bs.Search(ctx, id.ID, SearchQuery{Query: "spilled"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("want 1 match got %d", len(matches))
	}
	if len(matches[0].Snippet) > 243 {
		t.Fatalf("snippet %d bytes would re-spill the search response", len(matches[0].Snippet))
	}
}

func TestAdvSearch_MatchInsideTruncatedWindow(t *testing.T) {
	bs := newSearchFixture(t)
	ctx := context.Background()
	// Single-line spill-shaped payload LARGER than the scan window; the
	// needle sits inside the window (~1KB in) with 190KB of payload after it. The old state machine flushed
	// the carry only on EOF, so a maxBytes-bounded scan silently returned
	// zero matches — exactly the default mpm_blob_search shape.
	huge := `{"count":1,"memories":[{"content":"` + strings.Repeat("X", 1000) + `NEEDLEMARKER` + strings.Repeat("X", 190000) + `"}]}` // no trailing newline
	meta := Metadata{SourceTool: "adv", SizeBytes: int64(len(huge)), ContentType: "application/json"}
	id, err := bs.Put(ctx, strings.NewReader(huge), meta)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := bs.Search(ctx, id.ID, SearchQuery{Query: "NEEDLEMARKER", MaxBytes: 50 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("match inside truncated window lost: got %d matches", len(matches))
	}
}
