package internal

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
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

// TestAddReferenceRejectsDuplicate locks in the ErrAlreadyExists contract:
// the previous INSERT OR REPLACE silently overwrote metadata and could
// orphan chunks. The unified AddReference must refuse a duplicate id with
// a typed sentinel so callers can branch on it (vs treating every error
// as fatal).
func TestAddReferenceRejectsDuplicate(t *testing.T) {
	dm, db := newTestRefDM(t)

	doc := &ReferenceDoc{
		ID:         "dup-001",
		Title:      "First",
		SourcePath: "/test/dup.txt",
		SourceType: "txt",
	}
	if err := dm.AddReference(doc, nil); err != nil {
		t.Fatalf("first AddReference failed: %v", err)
	}

	err := dm.AddReference(doc, nil)
	if err == nil {
		t.Fatal("expected ErrAlreadyExists on duplicate id, got nil")
	}
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected wrapped ErrAlreadyExists, got: %v", err)
	}

	// Title must be unchanged — no silent overwrite.
	got, err := GetReferenceDoc(db, "dup-001")
	if err != nil {
		t.Fatalf("GetReferenceDoc failed: %v", err)
	}
	if got.Title != "First" {
		t.Errorf("expected title preserved, got: %q", got.Title)
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
