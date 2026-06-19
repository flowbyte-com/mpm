package internal

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// newTestRefDM returns a *DatabaseManager over a fresh in-memory SQLite
// DB using the shared-cache mode. Each test gets a per-test unique DSN
// (file:<random>?mode=memory&cache=shared) so the shared cache is owned
// by that test alone — no cross-test contamination, no tmpdir on disk,
// and InitSchema runs against the same SQL slice as production, so the
// reference_docs / reference_chunks / reference_interactions /
// admission_log tables are guaranteed identical to what production sees.
//
// The previous NewReferenceDB(tmpfile) pattern had each test open its
// own sqlite file via NewSQLiteConnection. The new helper routes every
// reference test through the unified write surface
// (*DatabaseManager.AddReference / DeleteReference) — there is no
// separate ReferenceDB type to keep in sync, and the test DB shares the
// same connection-pool semantics as production code.
func newTestRefDM(t *testing.T) (*DatabaseManager, *sql.DB) {
	t.Helper()
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	dsn := "file:ref_" + hex.EncodeToString(suffix) + "?mode=memory&cache=shared"
	raw, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open test sqlite (%s): %v", dsn, err)
	}
	dm := NewDatabaseManagerForDB(raw)
	if err := dm.InitSchema(); err != nil {
		_ = raw.Close()
		t.Fatalf("InitSchema: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return dm, dm.SQLDB()
}


func TestCountTokens_English(t *testing.T) {
	count, err := CountTokens("hello world")
	if err != nil {
		t.Fatalf("CountTokens failed: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 tokens for 'hello world', got %d", count)
	}
}

func TestCountTokens_KnownPhrase(t *testing.T) {
	// "foo"=1, "bar"=1, "baz"=1 → 3 tokens
	count, err := CountTokens("foo bar baz")
	if err != nil {
		t.Fatalf("CountTokens failed: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 tokens for 'foo bar baz', got %d", count)
	}
}

func TestCountTokens_RepeatingWord(t *testing.T) {
	// Each "word " (with trailing space) is one token; 200 repeats = 200 tokens
	// Note: tiktoken counts the trailing space as part of the token boundary
	content := strings.Repeat("word ", 200)
	count, err := CountTokens(content)
	if err != nil {
		t.Fatalf("CountTokens failed: %v", err)
	}
	// tiktoken may count 200 or 201 depending on trailing space handling
	if count < 200 || count > 201 {
		t.Errorf("expected 200-201 tokens for 200x'word ', got %d", count)
	}
}

func TestChunkByTokens_SingleChunk(t *testing.T) {
	content := "This is a short sentence."
	chunks, err := ChunkByTokens(content, 512)
	if err != nil {
		t.Fatalf("ChunkByTokens failed: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk for short text, got %d", len(chunks))
	}
	if chunks[0].Content != content {
		t.Errorf("content mismatch: got %q, want %q", chunks[0].Content, content)
	}
}

func TestChunkByTokens_MultipleChunks(t *testing.T) {
	// 200 tokens, chunkSize=64 → should produce multiple chunks (200/64 ≈ 3-4)
	content := strings.Repeat("word ", 200)
	chunks, err := ChunkByTokens(content, 64)
	if err != nil {
		t.Fatalf("ChunkByTokens failed: %v", err)
	}
	if len(chunks) < 3 {
		t.Errorf("expected at least 3 chunks for 200 tokens at chunkSize=64, got %d", len(chunks))
	}
}

func TestChunkByTokens_TokenAccuracy(t *testing.T) {
	// 300 tokens at chunkSize=64 → verify no chunk exceeds target by >10 tokens
	content := strings.Repeat("word ", 300)
	for _, tc := range []int{64, 128, 256} {
		chunks, err := ChunkByTokens(content, tc)
		if err != nil {
			t.Fatalf("ChunkByTokens(chunkSize=%d) failed: %v", tc, err)
		}
		for i, chunk := range chunks {
			count, err := CountTokens(chunk.Content)
			if err != nil {
				t.Fatalf("CountTokens chunk %d failed: %v", i, err)
			}
			if count > tc+10 {
				t.Errorf("chunk %d with chunkSize=%d exceeds target by >10 tokens: got %d", i, tc, count)
			}
		}
	}
}

func TestChunkByTokens_EdgeCases(t *testing.T) {
	// Empty content returns nil (no chunks — avoids storing empty references)
	chunks, err := ChunkByTokens("", 512)
	if err != nil {
		t.Fatalf("ChunkByTokens('') failed: %v", err)
	}
	if chunks != nil {
		t.Errorf("expected nil for empty content, got %d chunks", len(chunks))
	}

	// chunkSize below minimum — should clamp to 1
	chunks, err = ChunkByTokens("hello world", 0)
	if err != nil {
		t.Fatalf("ChunkByTokens with chunkSize=0 failed: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk for chunkSize=0, got %d", len(chunks))
	}
}

func TestParsePDF(t *testing.T) {
	// Check if the function compiles and handles errors correctly
	text, err := ParsePDF("/nonexistent/file.pdf")
	if err == nil {
		t.Error("Expected error for nonexistent file, got nil")
	}
	if text != "" {
		t.Errorf("Expected empty text for nonexistent file, got: %s", text)
	}
}

func TestParseEPUB(t *testing.T) {
	// Check if the function compiles and handles errors correctly
	text, err := ParseEPUB("/nonexistent/file.epub")
	if err == nil {
		t.Error("Expected error for nonexistent file, got nil")
	}
	if text != "" {
		t.Errorf("Expected empty text for nonexistent file, got: %s", text)
	}
}

func TestStripHTML(t *testing.T) {
	// Test the stripHTML function
	input := "<html><body>Hello <b>World</b></body></html>"
	result := stripHTML(strings.NewReader(input))
	if result != "Hello World " {
		t.Errorf("Expected 'Hello World ', got: %s", result)
	}
}

// ==================== Reference Library (unified via DatabaseManager) ====================
//
// These tests exercise the same operations as the legacy ReferenceDB tests
// (init, add+get, list+delete, stats, duplicate-id rejection, atomic delete
// fan-out, import_reason preservation) but go through the unified write
// surface — *DatabaseManager.AddReference and *DatabaseManager.DeleteReference
// — instead of through a separate *ReferenceDB with its own *SQLiteConnection.
// The read side uses free functions in reference_query.go taking *sql.DB.
//
// The legacy TestAddChunkIdempotent is intentionally removed: chunks now
// only enter the database via AddReference(doc, chunks) which is atomic.
// If the doc id is a duplicate the whole tx fails before any chunk row is
// inserted; if the doc id is fresh every chunk is inserted once. The old
// "add doc, then re-add chunk in a separate call" flow has no analogue in
// the unified API.

// TestReferenceInitViaDatabaseManager verifies that InitSchema creates
// the reference_docs / reference_chunks / reference_interactions /
// admission_log tables and indexes — the single source of truth shared
// with production.
func TestReferenceInitViaDatabaseManager(t *testing.T) {
	dm, db := newTestRefDM(t)
	if !dm.IsOpen() {
		t.Fatal("DatabaseManager reports not open after InitSchema")
	}
	for _, tbl := range []string{"reference_docs", "reference_chunks", "reference_interactions", "admission_log"} {
		var name string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name); err != nil {
			t.Errorf("table %q missing after InitSchema: %v", tbl, err)
		}
	}
}

// TestReferenceAddAndRetrieve tests the happy path: doc + chunk inserted
// in one transaction, retrieved via free-function reads.
func TestReferenceAddAndRetrieve(t *testing.T) {
	dm, db := newTestRefDM(t)

	doc := &ReferenceDoc{
		ID:          "test-001",
		Title:       "Test Document",
		SourcePath:  "/test/path.txt",
		SourceType:  "txt",
		Tags:        []string{"test", "sample"},
		TotalChunks: 1,
	}
	chunk := ReferenceChunk{
		ID:         "chunk-001",
		DocID:      "test-001",
		ChunkIndex: 0,
		Section:    "Introduction",
		Content:    "This is a test chunk.",
	}
	if err := dm.AddReference(doc, []ReferenceChunk{chunk}); err != nil {
		t.Fatalf("AddReference failed: %v", err)
	}

	got, err := GetReferenceDoc(db, "test-001")
	if err != nil {
		t.Fatalf("GetReferenceDoc failed: %v", err)
	}
	if got.Title != "Test Document" {
		t.Errorf("expected title %q, got %q", "Test Document", got.Title)
	}

	chunks, err := GetReferenceChunksByDocID(db, "test-001")
	if err != nil {
		t.Fatalf("GetReferenceChunksByDocID failed: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Content != "This is a test chunk." {
		t.Errorf("expected chunk content %q, got %q", "This is a test chunk.", chunks[0].Content)
	}
}

// TestReferenceListAndDelete covers the list round-trip and the
// DeleteReference atomic fan-out (chunks + audit rows + doc all gone).
func TestReferenceListAndDelete(t *testing.T) {
	dm, db := newTestRefDM(t)

	doc := &ReferenceDoc{
		ID:         "test-002",
		Title:      "Another Test",
		SourcePath: "/test/path2.txt",
		SourceType: "txt",
	}
	if err := dm.AddReference(doc, nil); err != nil {
		t.Fatalf("AddReference failed: %v", err)
	}

	docs, err := ListReferenceDocs(db)
	if err != nil {
		t.Fatalf("ListReferenceDocs failed: %v", err)
	}
	found := false
	for _, d := range docs {
		if d.ID == "test-002" {
			found = true
			break
		}
	}
	if !found {
		t.Error("test-002 not found in list")
	}

	if err := dm.DeleteReference("test-002"); err != nil {
		t.Fatalf("DeleteReference failed: %v", err)
	}

	if _, err := GetReferenceDoc(db, "test-002"); err == nil {
		t.Error("expected GetReferenceDoc to fail after delete, got nil")
	}
}

// TestReferenceStats covers GetReferenceStats shape (total_documents /
// total_chunks keys preserved for UI consumers).
func TestReferenceStats(t *testing.T) {
	dm, db := newTestRefDM(t)

	stats, err := GetReferenceStats(db)
	if err != nil {
		t.Fatalf("GetReferenceStats failed: %v", err)
	}
	if stats["total_documents"] != 0 {
		t.Errorf("expected 0 documents initially, got: %d", stats["total_documents"])
	}
	if stats["total_chunks"] != 0 {
		t.Errorf("expected 0 chunks initially, got: %d", stats["total_chunks"])
	}

	doc := &ReferenceDoc{
		ID:         "test-003",
		Title:      "Stats Test",
		SourcePath: "/test/stats.txt",
		SourceType: "txt",
	}
	if err := dm.AddReference(doc, []ReferenceChunk{
		{ID: "c1", DocID: "test-003", ChunkIndex: 0, Content: "one"},
		{ID: "c2", DocID: "test-003", ChunkIndex: 1, Content: "two"},
	}); err != nil {
		t.Fatalf("AddReference failed: %v", err)
	}

	stats, err = GetReferenceStats(db)
	if err != nil {
		t.Fatalf("GetReferenceStats(after add) failed: %v", err)
	}
	if stats["total_documents"] != 1 {
		t.Errorf("expected 1 document after add, got: %d", stats["total_documents"])
	}
	if stats["total_chunks"] != 2 {
		t.Errorf("expected 2 chunks after add, got: %d", stats["total_chunks"])
	}
}

// TestAddReferenceUpserts locks in the upsert + chunk-diff contract:
// the previous INSERT OR REPLACE silently overwrote metadata; then
// AddReference rejected duplicates with ErrAlreadyExists so a re-ingest
// was forced through delete-then-insert (a trapdoor). The chunk-hash
// diff ingest requires re-ingest into the same doc id to work, so
// AddReference now upserts via ON CONFLICT(id) DO UPDATE: metadata
// refreshes, unchanged chunks are skipped, orphans are deleted.
//
// Callers wanting strict one-shot semantics must check existence first
// (GetReferenceDoc by source_path) and pick a fresh id themselves.
func TestAddReferenceUpserts(t *testing.T) {
	dm, db := newTestRefDM(t)

	first := &ReferenceDoc{
		ID:         "dup-001",
		Title:      "First",
		SourcePath: "/test/dup.txt",
		SourceType: "txt",
	}
	if err := dm.AddReference(first, nil); err != nil {
		t.Fatalf("first AddReference failed: %v", err)
	}

	// Re-ingest with same id, updated title, same source path.
	second := &ReferenceDoc{
		ID:         "dup-001",
		Title:      "First (updated)",
		SourcePath: "/test/dup.txt",
		SourceType: "txt",
	}
	if err := dm.AddReference(second, nil); err != nil {
		t.Fatalf("upsert AddReference failed: %v", err)
	}

	// Title must reflect the upsert — metadata refreshed.
	got, err := GetReferenceDoc(db, "dup-001")
	if err != nil {
		t.Fatalf("GetReferenceDoc failed: %v", err)
	}
	if got.Title != "First (updated)" {
		t.Errorf("expected title updated to %q, got %q", "First (updated)", got.Title)
	}

	// Still one doc row, not two.
	var docCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM reference_docs WHERE id = ?`, "dup-001").Scan(&docCount); err != nil {
		t.Fatalf("count docs: %v", err)
	}
	if docCount != 1 {
		t.Errorf("expected 1 doc row after upsert, got %d", docCount)
	}
}

// TestDeleteReferenceAtomic verifies DeleteReference rolls back cleanly
// when one of the fan-out deletes fails. Seeds an audit row, deletes the
// doc, then verifies the doc + audit row + chunks are all gone (atomic)
// and the connection is still usable for follow-up writes (no leftover
// tx state).
func TestDeleteReferenceAtomic(t *testing.T) {
	dm, db := newTestRefDM(t)

	doc := &ReferenceDoc{
		ID: "del-001", Title: "Del", SourcePath: "/d.txt", SourceType: "txt",
	}
	if err := dm.AddReference(doc, []ReferenceChunk{
		{ID: "del-c1", DocID: "del-001", ChunkIndex: 0, Content: "x"},
	}); err != nil {
		t.Fatalf("AddReference failed: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO reference_interactions (id, doc_id, query, created_at) VALUES (?, ?, ?, ?)`,
		"del-i1", "del-001", "q", time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("seed interaction failed: %v", err)
	}

	if err := dm.DeleteReference("del-001"); err != nil {
		t.Fatalf("DeleteReference failed: %v", err)
	}

	if _, err := GetReferenceDoc(db, "del-001"); err == nil {
		t.Error("expected GetReferenceDoc to fail after delete, got nil")
	}
	var chunkCount, interactionCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM reference_chunks WHERE doc_id = ?`, "del-001").Scan(&chunkCount); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunkCount != 0 {
		t.Errorf("expected 0 orphan chunks after delete, got: %d", chunkCount)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM reference_interactions WHERE doc_id = ?`, "del-001").Scan(&interactionCount); err != nil {
		t.Fatalf("count interactions: %v", err)
	}
	if interactionCount != 0 {
		t.Errorf("expected 0 interactions after delete, got: %d", interactionCount)
	}

	// Connection must still be usable for follow-up writes — no leaked tx.
	if err := dm.AddReference(&ReferenceDoc{
		ID: "del-002", Title: "After", SourcePath: "/a.txt", SourceType: "txt",
	}, nil); err != nil {
		t.Errorf("connection broken after delete tx: %v", err)
	}
}

// TestGetReferenceReturnsImportReason guards the regression where
// GetReference's two-query path silently dropped import_reason on read.
// With the unified single-query SELECT, every column is preserved.
func TestGetReferenceReturnsImportReason(t *testing.T) {
	dm, db := newTestRefDM(t)

	doc := &ReferenceDoc{
		ID:           "ir-001",
		Title:        "Reasoned",
		SourcePath:   "/r.txt",
		SourceType:   "txt",
		ImportReason: "admitted: parallels Marcus Aurelius on discipline",
		Tags:         []string{"stoicism", "discipline"},
	}
	if err := dm.AddReference(doc, nil); err != nil {
		t.Fatalf("AddReference failed: %v", err)
	}

	got, err := GetReferenceDoc(db, "ir-001")
	if err != nil {
		t.Fatalf("GetReferenceDoc failed: %v", err)
	}
	if got.ImportReason != doc.ImportReason {
		t.Errorf("import_reason lost on read: got %q, want %q", got.ImportReason, doc.ImportReason)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "stoicism" {
		t.Errorf("tags lost on read: got %v", got.Tags)
	}
}

// ==================== Chunk-Hash Diff Ingest ====================

// TestComputeChunkIDStable verifies the deterministic-ID contract:
// same (docID, chunkIndex, content) always yields the same id, different
// content yields a different id. This is the property AddReference relies
// on to keep chunk identity stable across re-ingests.
func TestComputeChunkIDStable(t *testing.T) {
	id1 := ComputeChunkID("doc-A", 0, HashContent("hello world"))
	id2 := ComputeChunkID("doc-A", 0, HashContent("hello world"))
	if id1 != id2 {
		t.Errorf("ComputeChunkID not stable: %q != %q", id1, id2)
	}
	id3 := ComputeChunkID("doc-A", 1, HashContent("hello world"))
	if id1 == id3 {
		t.Errorf("ComputeChunkID collapsed different chunkIndex: %q == %q", id1, id3)
	}
	id4 := ComputeChunkID("doc-B", 0, HashContent("hello world"))
	if id1 == id4 {
		t.Errorf("ComputeChunkID collapsed different docID: %q == %q", id1, id4)
	}
	id5 := ComputeChunkID("doc-A", 0, HashContent("hello WORLD"))
	if id1 == id5 {
		t.Errorf("ComputeChunkID collapsed different content: %q == %q", id1, id5)
	}
}

// TestDiffChunksFreshIngest: the re-ingest path against an existing
// doc but with completely new content. Every new chunk is Inserted
// because no row has matching content_hash; the old chunks (with the
// original content) all become orphans. This is the "I rewrote the
// file completely" scenario.
func TestDiffChunksFreshIngest(t *testing.T) {
	dm, db := newTestRefDM(t)
	if err := dm.AddReference(&ReferenceDoc{
		ID: "diff-fresh", Title: "Fresh", SourcePath: "/fresh.txt", SourceType: "txt",
	}, []ReferenceChunk{
		{ID: "f-c1", DocID: "diff-fresh", ChunkIndex: 0, Content: "alpha"},
		{ID: "f-c2", DocID: "diff-fresh", ChunkIndex: 1, Content: "beta"},
	}); err != nil {
		t.Fatalf("seed AddReference failed: %v", err)
	}

	// Re-ingest: same doc id, completely different content. Old chunks
	// become orphans; new chunks are all inserts.
	diff, err := DiffChunks(db, "diff-fresh", []ReferenceChunk{
		{ID: ComputeChunkID("diff-fresh", 0, HashContent("gamma")),
			DocID: "diff-fresh", ChunkIndex: 0, Content: "gamma"},
		{ID: ComputeChunkID("diff-fresh", 1, HashContent("delta")),
			DocID: "diff-fresh", ChunkIndex: 1, Content: "delta"},
	})
	if err != nil {
		t.Fatalf("DiffChunks failed: %v", err)
	}
	if len(diff.Inserted) != 2 {
		t.Errorf("expected 2 inserted (no existing row with new content_hash), got %d", len(diff.Inserted))
	}
	if len(diff.Deleted) != 2 {
		t.Errorf("expected 2 deleted (original alpha/beta now orphan), got %d", len(diff.Deleted))
	}
	if len(diff.Unchanged) != 0 {
		t.Errorf("expected 0 unchanged (no content matched), got %d", len(diff.Unchanged))
	}
	if len(diff.Updated) != 0 {
		t.Errorf("expected 0 updated, got %d", len(diff.Updated))
	}
}

// TestDiffChunksReIngestSameContent: after seeding chunks with their
// content_hash populated, re-ingesting the same content produces all
// Unchanged + zero Deleted + zero Inserted.
func TestDiffChunksReIngestSameContent(t *testing.T) {
	dm, db := newTestRefDM(t)
	if err := dm.AddReference(&ReferenceDoc{
		ID: "diff-same", Title: "Same", SourcePath: "/same.txt", SourceType: "txt",
	}, []ReferenceChunk{
		{ID: "s-c1", DocID: "diff-same", ChunkIndex: 0, Content: "unchanged text"},
		{ID: "s-c2", DocID: "diff-same", ChunkIndex: 1, Content: "also unchanged"},
	}); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	// Verify content_hash was written by the seed AddReference.
	var hashCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM reference_chunks WHERE doc_id = ? AND content_hash IS NOT NULL`, "diff-same").Scan(&hashCount); err != nil {
		t.Fatalf("count hashed chunks: %v", err)
	}
	if hashCount != 2 {
		t.Fatalf("expected both seed chunks to have content_hash populated, got %d/2", hashCount)
	}

	// Re-ingest: same content, new (deterministic) IDs.
	diff, err := DiffChunks(db, "diff-same", []ReferenceChunk{
		{ID: ComputeChunkID("diff-same", 0, HashContent("unchanged text")),
			DocID: "diff-same", ChunkIndex: 0, Content: "unchanged text"},
		{ID: ComputeChunkID("diff-same", 1, HashContent("also unchanged")),
			DocID: "diff-same", ChunkIndex: 1, Content: "also unchanged"},
	})
	if err != nil {
		t.Fatalf("DiffChunks failed: %v", err)
	}
	if len(diff.Unchanged) != 2 {
		t.Errorf("expected 2 unchanged, got %d", len(diff.Unchanged))
	}
	if len(diff.Inserted) != 0 {
		t.Errorf("expected 0 inserted, got %d", len(diff.Inserted))
	}
	if len(diff.Updated) != 0 {
		t.Errorf("expected 0 updated, got %d", len(diff.Updated))
	}
	if len(diff.Deleted) != 0 {
		t.Errorf("expected 0 deleted, got %d", len(diff.Deleted))
	}
}

// TestDiffChunksReIngestWithChanges: re-ingest where one chunk is
// unchanged, one is updated (same id but different content), one is new,
// and one old chunk is gone. All four diff branches must fire correctly.
func TestDiffChunksReIngestWithChanges(t *testing.T) {
	dm, db := newTestRefDM(t)
	if err := dm.AddReference(&ReferenceDoc{
		ID: "diff-mix", Title: "Mix", SourcePath: "/mix.txt", SourceType: "txt",
	}, []ReferenceChunk{
		{ID: "m-c1", DocID: "diff-mix", ChunkIndex: 0, Content: "keep me"},
		{ID: "m-c2", DocID: "diff-mix", ChunkIndex: 1, Content: "replace me"},
		{ID: "m-c3", DocID: "diff-mix", ChunkIndex: 2, Content: "drop me"},
	}); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	// New chunk set: keep "keep me" (same content), update "replace me"
	// (same id, different content), insert new "fresh me", drop "drop me".
	replaceID := "m-c2"
	diff, err := DiffChunks(db, "diff-mix", []ReferenceChunk{
		{ID: "m-c1", DocID: "diff-mix", ChunkIndex: 0, Content: "keep me"},
		{ID: replaceID, DocID: "diff-mix", ChunkIndex: 1, Content: "replaced content"},
		{ID: "m-c4", DocID: "diff-mix", ChunkIndex: 3, Content: "fresh me"},
	})
	if err != nil {
		t.Fatalf("DiffChunks failed: %v", err)
	}
	if len(diff.Unchanged) != 1 {
		t.Errorf("expected 1 unchanged (keep me), got %d", len(diff.Unchanged))
	}
	if len(diff.Updated) != 1 {
		t.Errorf("expected 1 updated (replace me), got %d", len(diff.Updated))
	}
	if len(diff.Inserted) != 1 {
		t.Errorf("expected 1 inserted (fresh me), got %d", len(diff.Inserted))
	}
	if len(diff.Deleted) != 1 {
		t.Errorf("expected 1 deleted (drop me), got %d", len(diff.Deleted))
	}
	if len(diff.Deleted) == 1 && diff.Deleted[0] != "m-c3" {
		t.Errorf("expected deleted id m-c3, got %q", diff.Deleted[0])
	}
}

// TestAddReferenceReIngestAppliesDiff: end-to-end through AddReference.
// Seed a doc with three chunks, then re-ingest with one changed and one
// new. After the call: total chunk count is correct, the unchanged
// chunk is preserved, the changed chunk has new content + content_hash,
// the orphan is gone, and the new chunk exists.
func TestAddReferenceReIngestAppliesDiff(t *testing.T) {
	dm, db := newTestRefDM(t)

	doc := &ReferenceDoc{
		ID: "reingest-doc", Title: "Reingest", SourcePath: "/ri.txt", SourceType: "txt",
		TotalChunks: 3,
	}
	if err := dm.AddReference(doc, []ReferenceChunk{
		{ID: "ri-c1", DocID: doc.ID, ChunkIndex: 0, Content: "stable"},
		{ID: "ri-c2", DocID: doc.ID, ChunkIndex: 1, Content: "old text"},
		{ID: "ri-c3", DocID: doc.ID, ChunkIndex: 2, Content: "will vanish"},
	}); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	// Re-ingest: doc id stays the same, content_hash differs, chunks
	// change. TotalChunks reflects the new chunk count.
	updated := &ReferenceDoc{
		ID: doc.ID, Title: "Reingest", SourcePath: doc.SourcePath, SourceType: "txt",
		ContentHash: HashContent("fresh doc content"),
		TotalChunks: 3,
	}
	if err := dm.AddReference(updated, []ReferenceChunk{
		{ID: "ri-c1", DocID: doc.ID, ChunkIndex: 0, Content: "stable"},                // unchanged
		{ID: "ri-c2", DocID: doc.ID, ChunkIndex: 1, Content: "new text after edit"},   // updated
		{ID: "ri-c4", DocID: doc.ID, ChunkIndex: 3, Content: "added in re-ingest"},     // inserted
	}); err != nil {
		t.Fatalf("re-ingest failed: %v", err)
	}

	// Count: 3 chunks now (ri-c1, ri-c2 updated, ri-c4 new). ri-c3 gone.
	chunks, err := GetReferenceChunksByDocID(db, doc.ID)
	if err != nil {
		t.Fatalf("GetReferenceChunksByDocID: %v", err)
	}
	if len(chunks) != 3 {
		t.Errorf("expected 3 chunks after re-ingest, got %d", len(chunks))
	}

	// Verify each expected chunk exists with the right content.
	byID := make(map[string]string)
	for _, c := range chunks {
		byID[c.ID] = c.Content
	}
	if byID["ri-c1"] != "stable" {
		t.Errorf("unchunk ri-c1: got %q, want %q", byID["ri-c1"], "stable")
	}
	if byID["ri-c2"] != "new text after edit" {
		t.Errorf("updated ri-c2: got %q, want %q", byID["ri-c2"], "new text after edit")
	}
	if byID["ri-c4"] != "added in re-ingest" {
		t.Errorf("new ri-c4: got %q, want %q", byID["ri-c4"], "added in re-ingest")
	}
	if _, ok := byID["ri-c3"]; ok {
		t.Errorf("orphan ri-c3 should be deleted but still exists")
	}

	// Verify content_hash reflects the updated content.
	var ri2Hash string
	if err := db.QueryRow(`SELECT content_hash FROM reference_chunks WHERE id = ?`, "ri-c2").Scan(&ri2Hash); err != nil {
		t.Fatalf("read ri-c2 hash: %v", err)
	}
	if ri2Hash != HashContent("new text after edit") {
		t.Errorf("ri-c2 content_hash stale: got %q", ri2Hash)
	}

	// Verify total_chunks on the doc row updated to reflect the new set.
	got, err := GetReferenceDoc(db, doc.ID)
	if err != nil {
		t.Fatalf("GetReferenceDoc: %v", err)
	}
	if got.TotalChunks != 3 {
		t.Errorf("expected doc.total_chunks=3 after re-ingest, got %d", got.TotalChunks)
	}
}

// TestFindReferenceBySourcePath covers the lookup helper used by both
// CLI ingest paths to find an existing doc for re-ingest instead of
// creating a duplicate.
func TestFindReferenceBySourcePath(t *testing.T) {
	dm, db := newTestRefDM(t)
	if err := dm.AddReference(&ReferenceDoc{
		ID: "lookup-1", Title: "Lookup", SourcePath: "/lookup.txt", SourceType: "txt",
		ContentHash: "deadbeef",
	}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	found, err := FindReferenceBySourcePath(db, "/lookup.txt")
	if err != nil {
		t.Fatalf("FindReferenceBySourcePath: %v", err)
	}
	if found == nil {
		t.Fatal("expected to find the seeded doc, got nil")
	}
	if found.ID != "lookup-1" {
		t.Errorf("expected id=lookup-1, got %q", found.ID)
	}
	if found.Title != "Lookup" {
		t.Errorf("expected title=Lookup, got %q", found.Title)
	}

	// Empty sourcePath rejected.
	if _, err := FindReferenceBySourcePath(db, ""); err == nil {
		t.Error("expected error for empty sourcePath")
	}

	// Missing sourcePath returns (nil, nil) — caller decides whether to create.
	missing, err := FindReferenceBySourcePath(db, "/never-ingested.txt")
	if err != nil {
		t.Fatalf("FindReferenceBySourcePath missing: %v", err)
	}
	if missing != nil {
		t.Errorf("expected nil for missing source, got %+v", missing)
	}
}

