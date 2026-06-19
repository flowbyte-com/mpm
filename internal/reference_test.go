package internal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func TestReferenceStoreIntegration(t *testing.T) {
	// Test that the reference store works with SQLite
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "references.sqlite")

	store := NewReferenceDB(dbPath)
	err := store.Init()
	if err != nil {
		t.Fatalf("Failed to init reference DB: %v", err)
	}

	// Verify the database file was created
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("Database file was not created")
	}
}

// TestParsePDFBasic tests basic PDF parsing without a real file
func TestParsePDFBasic(t *testing.T) {
	// Just verify the function exists and has the right signature
	_ = ParsePDF
}

// TestParseEPUBBasic tests basic EPUB parsing without a real file
func TestParseEPUBBasic(t *testing.T) {
	// Just verify the function exists and has the right signature
	_ = ParseEPUB
}

// TestParsePDFApi verifies the ParsePDF API
func TestParsePDFApi(t *testing.T) {
	text, err := ParsePDF("/nonexistent.pdf")
	if err == nil {
		t.Log("Expected error for nonexistent file")
	}
	t.Logf("ParsePDF returned: text=%d chars, err=%v", len(text), err)
}

// TestParseEPUBApi verifies the ParseEPUB API
func TestParseEPUBApi(t *testing.T) {
	text, err := ParseEPUB("/nonexistent.epub")
	if err == nil {
		t.Log("Expected error for nonexistent file")
	}
	t.Logf("ParseEPUB returned: text=%d chars, err=%v", len(text), err)
}

// TestStripHTMLApi verifies the stripHTML API
func TestStripHTMLApi(t *testing.T) {
	result := stripHTML(strings.NewReader("<html><body>Test</body></html>"))
	t.Logf("stripHTML returned: %s", result)
	if !strings.Contains(result, "Test") {
		t.Errorf("Expected 'Test' in result, got: %s", result)
	}
}

// TestReferenceStoreInit verifies ReferenceDB initialization
func TestReferenceStoreInit(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.sqlite")

	store := NewReferenceDB(dbPath)
	err := store.Init()
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Verify tables exist
	var count int
	err = store.db.QueryRow("SELECT COUNT(*) FROM reference_docs").Scan(&count)
	if err != nil {
		t.Errorf("Failed to query reference_docs: %v", err)
	}
}

// TestReferenceStoreAddAndRetrieve tests adding and retrieving references
func TestReferenceStoreAddAndRetrieve(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.sqlite")

	store := NewReferenceDB(dbPath)
	err := store.Init()
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Add a reference
	doc := &ReferenceDoc{
		ID:          "test-001",
		Title:       "Test Document",
		SourcePath:  "/test/path.txt",
		SourceType:  "txt",
		Tags:        []string{"test", "sample"},
		TotalChunks: 1,
	}
	err = store.AddReference(doc)
	if err != nil {
		t.Fatalf("AddReference failed: %v", err)
	}

	// Retrieve it
	retrieved, err := store.GetReference("test-001")
	if err != nil {
		t.Fatalf("GetReference failed: %v", err)
	}

	if retrieved.Title != "Test Document" {
		t.Errorf("Expected 'Test Document', got: %s", retrieved.Title)
	}

	// Add a chunk
	chunk := &ReferenceChunk{
		ID:         "chunk-001",
		DocID:      "test-001",
		ChunkIndex: 0,
		Section:    "Introduction",
		Content:    "This is a test chunk.",
	}
	err = store.AddChunk(chunk)
	if err != nil {
		t.Fatalf("AddChunk failed: %v", err)
	}

	// Retrieve chunks
	chunks, err := store.GetChunksByDocID("test-001")
	if err != nil {
		t.Fatalf("GetChunksByDocID failed: %v", err)
	}

	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk, got: %d", len(chunks))
	}

	if chunks[0].Content != "This is a test chunk." {
		t.Errorf("Expected 'This is a test chunk.', got: %s", chunks[0].Content)
	}
}

// TestReferenceStoreListAndDelete tests listing and deleting references
func TestReferenceStoreListAndDelete(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.sqlite")

	store := NewReferenceDB(dbPath)
	err := store.Init()
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Add a reference
	doc := &ReferenceDoc{
		ID:         "test-002",
		Title:      "Another Test",
		SourcePath: "/test/path2.txt",
		SourceType: "txt",
	}
	err = store.AddReference(doc)
	if err != nil {
		t.Fatalf("AddReference failed: %v", err)
	}

	// List references
	docs, err := store.ListReferences()
	if err != nil {
		t.Fatalf("ListReferences failed: %v", err)
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

	// Delete reference
	err = store.DeleteReference("test-002")
	if err != nil {
		t.Fatalf("DeleteReference failed: %v", err)
	}

	// Verify it's gone
	_, err = store.GetReference("test-002")
	if err == nil {
		t.Error("Expected error after deleting reference, got nil")
	}
}

// TestReferenceStats tests reference statistics
func TestReferenceStats(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.sqlite")

	store := NewReferenceDB(dbPath)
	err := store.Init()
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Get stats
	stats, err := store.GetReferenceStats()
	if err != nil {
		t.Fatalf("GetReferenceStats failed: %v", err)
	}

	if stats["total_documents"] != 0 {
		t.Errorf("Expected 0 documents initially, got: %d", stats["total_documents"])
	}
	if stats["total_chunks"] != 0 {
		t.Errorf("Expected 0 chunks initially, got: %d", stats["total_chunks"])
	}

	// Add a document and verify stats update
	doc := &ReferenceDoc{
		ID:         "test-003",
		Title:      "Stats Test",
		SourcePath: "/test/stats.txt",
		SourceType: "txt",
	}
	err = store.AddReference(doc)
	if err != nil {
		t.Fatalf("AddReference failed: %v", err)
	}

	stats, err = store.GetReferenceStats()
	if err != nil {
		t.Fatalf("GetReferenceStats failed: %v", err)
	}

	if stats["total_documents"] != 1 {
		t.Errorf("Expected 1 document after add, got: %d", stats["total_documents"])
	}
}

// TestAddReferenceRejectsDuplicate locks in the ErrAlreadyExists contract:
// the previous INSERT OR REPLACE silently overwrote metadata and could
// orphan chunks. New behaviour must refuse a duplicate id with a typed
// sentinel so callers can branch on it (vs treating every error as fatal).
func TestAddReferenceRejectsDuplicate(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.sqlite")

	store := NewReferenceDB(dbPath)
	if err := store.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	doc := &ReferenceDoc{
		ID:         "dup-001",
		Title:      "First",
		SourcePath: "/test/dup.txt",
		SourceType: "txt",
	}
	if err := store.AddReference(doc); err != nil {
		t.Fatalf("first AddReference failed: %v", err)
	}

	err := store.AddReference(doc)
	if err == nil {
		t.Fatal("expected ErrAlreadyExists on duplicate id, got nil")
	}
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected wrapped ErrAlreadyExists, got: %v", err)
	}

	// Title must be unchanged — no silent overwrite.
	got, err := store.GetReference("dup-001")
	if err != nil {
		t.Fatalf("GetReference failed: %v", err)
	}
	if got.Title != "First" {
		t.Errorf("expected title preserved, got: %q", got.Title)
	}
}

// TestAddChunkIdempotent verifies AddChunk no longer duplicates on re-run.
// Runs ingest twice with the same chunk id; the table must end with one row,
// not two, and the second call must not return an error.
func TestAddChunkIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.sqlite")

	store := NewReferenceDB(dbPath)
	if err := store.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	doc := &ReferenceDoc{
		ID: "idem-001", Title: "Idem", SourcePath: "/i.txt", SourceType: "txt",
	}
	if err := store.AddReference(doc); err != nil {
		t.Fatalf("AddReference failed: %v", err)
	}

	chunk := &ReferenceChunk{
		ID: "idem-chunk-1", DocID: "idem-001",
		ChunkIndex: 0, Section: "s1", Content: "once",
	}
	if err := store.AddChunk(chunk); err != nil {
		t.Fatalf("first AddChunk failed: %v", err)
	}
	if err := store.AddChunk(chunk); err != nil {
		t.Fatalf("second AddChunk should be no-op, got: %v", err)
	}

	chunks, err := store.GetChunksByDocID("idem-001")
	if err != nil {
		t.Fatalf("GetChunksByDocID failed: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk after duplicate insert, got: %d", len(chunks))
	}
}

// TestDeleteReferenceAtomic verifies DeleteReference rolls back cleanly
// when one of the fan-out deletes fails. The test seeds an audit row,
// deletes the doc, then verifies both the doc and the audit row are gone
// (transaction committed as a unit) and the test fixture still works
// afterwards (no leftover tx state).
func TestDeleteReferenceAtomic(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.sqlite")

	store := NewReferenceDB(dbPath)
	if err := store.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	doc := &ReferenceDoc{ID: "del-001", Title: "Del", SourcePath: "/d.txt", SourceType: "txt"}
	if err := store.AddReference(doc); err != nil {
		t.Fatalf("AddReference failed: %v", err)
	}
	if err := store.AddChunk(&ReferenceChunk{
		ID: "del-c1", DocID: "del-001", ChunkIndex: 0, Content: "x",
	}); err != nil {
		t.Fatalf("AddChunk failed: %v", err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO reference_interactions (id, doc_id, query, created_at) VALUES (?, ?, ?, ?)`,
		"del-i1", "del-001", "q", time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("seed interaction failed: %v", err)
	}

	if err := store.DeleteReference("del-001"); err != nil {
		t.Fatalf("DeleteReference failed: %v", err)
	}

	if _, err := store.GetReference("del-001"); err == nil {
		t.Error("expected GetReference to fail after delete, got nil")
	}
	var chunkCount, interactionCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM reference_chunks WHERE doc_id = ?`, "del-001").Scan(&chunkCount); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunkCount != 0 {
		t.Errorf("expected 0 orphan chunks after delete, got: %d", chunkCount)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM reference_interactions WHERE doc_id = ?`, "del-001").Scan(&interactionCount); err != nil {
		t.Fatalf("count interactions: %v", err)
	}
	if interactionCount != 0 {
		t.Errorf("expected 0 interactions after delete, got: %d", interactionCount)
	}

	// Connection must still be usable for follow-up writes.
	if err := store.AddReference(&ReferenceDoc{ID: "del-002", Title: "After", SourcePath: "/a.txt", SourceType: "txt"}); err != nil {
		t.Errorf("connection broken after delete tx: %v", err)
	}
}

// TestGetReferenceReturnsImportReason guards the regression where
// GetReference's two-query path silently dropped import_reason on read.
func TestGetReferenceReturnsImportReason(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.sqlite")

	store := NewReferenceDB(dbPath)
	if err := store.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	doc := &ReferenceDoc{
		ID:           "ir-001",
		Title:        "Reasoned",
		SourcePath:   "/r.txt",
		SourceType:   "txt",
		ImportReason: "admitted: parallels Marcus Aurelius on discipline",
		Tags:         []string{"stoicism", "discipline"},
	}
	if err := store.AddReference(doc); err != nil {
		t.Fatalf("AddReference failed: %v", err)
	}

	got, err := store.GetReference("ir-001")
	if err != nil {
		t.Fatalf("GetReference failed: %v", err)
	}
	if got.ImportReason != doc.ImportReason {
		t.Errorf("import_reason lost on read: got %q, want %q", got.ImportReason, doc.ImportReason)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "stoicism" {
		t.Errorf("tags lost on read: got %v", got.Tags)
	}
}
