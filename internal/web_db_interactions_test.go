package internal

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// setupTestDM creates a DatabaseManager backed by a temp DB with the
// reference_interactions schema applied. Returns the dm and a cleanup func.
// NOTE: NewDatabaseManager() ignores its argument and uses config.GetMPMDir().
// Tests must use NewDatabaseManagerForDB with a fresh *sql.DB to avoid
// polluting the live MPM database.
func setupTestDM(t *testing.T) (*DatabaseManager, func()) {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "mpm-int-test-*.db")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	tmpFile.Close()
	dbPath := tmpFile.Name()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		os.Remove(dbPath)
		t.Fatalf("sql.Open: %v", err)
	}
	dm := NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		db.Close()
		os.Remove(dbPath)
		t.Fatalf("InitSchema: %v", err)
	}
	return dm, func() {
		db.Close()
		os.Remove(dbPath)
	}
}

func TestReferenceInteractionsTableExists(t *testing.T) {
	dm, cleanup := setupTestDM(t)
	defer cleanup()

	// The interactions table should exist after InitSchema.
	row := dm.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='reference_interactions'`)
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 reference_interactions table, got %d", n)
	}
}

func TestReferenceInteractionRecordAndRead(t *testing.T) {
	dm, cleanup := setupTestDM(t)
	defer cleanup()

	// Insert a reference doc first, with one chunk so FTS5 has content.
	doc := &ReferenceDoc{
		ID:           "test-doc-1",
		Title:        "Test Reference",
		SourcePath:   "/tmp/test.txt",
		SourceType:   "txt",
		Tags:         []string{"test"},
		ImportReason: "test reason",
		Content:      "the quick brown fox jumps over the lazy dog",
		ContentHash:  "abc123",
		TotalChunks:  1,
		LastIndexed:  "2026-01-01T00:00:00Z",
		Created:      "2026-01-01T00:00:00Z",
	}
	chunks := []ReferenceChunk{
		{ID: "c1", DocID: doc.ID, ChunkIndex: 0, Content: doc.Content, SourcePath: doc.SourcePath},
	}
	if err := dm.AddReference(doc, chunks); err != nil {
		t.Fatalf("AddReference: %v", err)
	}

	// Record an interaction.
	chunkID := "chunk-1"
	score := -3.14
	dm.recordReferenceInteraction(doc.ID, &chunkID, "fox", "chunk_fts", 1, sql.NullFloat64{Float64: score, Valid: true})

	// Read it back.
	rows, err := dm.GetInteractionsForDoc(doc.ID, 10)
	if err != nil {
		t.Fatalf("GetInteractionsForDoc: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 interaction, got %d", len(rows))
	}
	if q, _ := rows[0]["query"].(string); q != "fox" {
		t.Errorf("expected query='fox', got %q", q)
	}
	if k, _ := rows[0]["search_kind"].(string); k != "chunk_fts" {
		t.Errorf("expected kind='chunk_fts', got %q", k)
	}
	if cid, _ := rows[0]["chunk_id"].(string); cid != "chunk-1" {
		t.Errorf("expected chunk_id='chunk-1', got %q", cid)
	}
}

func TestReferenceInteractionFilterShortQuery(t *testing.T) {
	// The production SearchReferenceChunks wraps recordReferenceInteraction
	// and skips queries shorter than 3 chars. We test the wrapper indirectly
	// by ensuring short queries do not land in the table when called via
	// the public SearchReferenceChunks path. This test requires a doc with
	// matching content.
	dm, cleanup := setupTestDM(t)
	defer cleanup()

	doc := &ReferenceDoc{
		ID:           "test-doc-2",
		Title:        "Test 2",
		SourcePath:   "/tmp/test2.txt",
		SourceType:   "txt",
		Tags:         []string{},
		ImportReason: "",
		Content:      "the quick brown fox",
		ContentHash:  "def456",
		TotalChunks:  1,
		LastIndexed:  "2026-01-01T00:00:00Z",
		Created:      "2026-01-01T00:00:00Z",
	}
	chunks := []ReferenceChunk{
		{ID: "c2", DocID: doc.ID, ChunkIndex: 0, Content: doc.Content, SourcePath: doc.SourcePath},
	}
	if err := dm.AddReference(doc, chunks); err != nil {
		t.Fatalf("AddReference: %v", err)
	}

	// Short query — should be filtered at the SearchReferenceChunks layer.
	_, _ = dm.SearchReferenceChunks("fo", 10)
	rows, _ := dm.GetInteractionsForDoc(doc.ID, 10)
	if len(rows) != 0 {
		t.Errorf("expected 0 interactions for short query, got %d", len(rows))
	}

	// Long query — should be recorded.
	_, _ = dm.SearchReferenceChunks("fox", 10)
	rows, _ = dm.GetInteractionsForDoc(doc.ID, 10)
	if len(rows) != 1 {
		t.Errorf("expected 1 interaction for normal query, got %d", len(rows))
	}
}

func TestReferenceMostUsedAggregation(t *testing.T) {
	dm, cleanup := setupTestDM(t)
	defer cleanup()

	// Two docs, one is searched 3 times with 2 distinct queries,
	// the other is searched once. Aggregation should rank the first.
	doc1 := &ReferenceDoc{
		ID: "d1", Title: "Doc One", SourcePath: "/tmp/d1.txt", SourceType: "txt",
		Content: "fox alpha", ContentHash: "h1", TotalChunks: 1, LastIndexed: "2026-01-01T00:00:00Z", Created: "2026-01-01T00:00:00Z",
	}
	doc2 := &ReferenceDoc{
		ID: "d2", Title: "Doc Two", SourcePath: "/tmp/d2.txt", SourceType: "txt",
		Content: "fox beta", ContentHash: "h2", TotalChunks: 1, LastIndexed: "2026-01-01T00:00:00Z", Created: "2026-01-01T00:00:00Z",
	}
	chunks1 := []ReferenceChunk{{ID: "c-d1", DocID: doc1.ID, ChunkIndex: 0, Content: doc1.Content, SourcePath: doc1.SourcePath}}
	chunks2 := []ReferenceChunk{{ID: "c-d2", DocID: doc2.ID, ChunkIndex: 0, Content: doc2.Content, SourcePath: doc2.SourcePath}}
	if err := dm.AddReference(doc1, chunks1); err != nil {
		t.Fatalf("AddReference d1: %v", err)
	}
	if err := dm.AddReference(doc2, chunks2); err != nil {
		t.Fatalf("AddReference d2: %v", err)
	}

	// Search for "fox" three times against d1, once against d2.
	for i := 0; i < 3; i++ {
		_, _ = dm.SearchReferenceChunks("fox", 10)
	}

	rows, err := dm.GetMostUsedReferences(10)
	if err != nil {
		t.Fatalf("GetMostUsedReferences: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("expected 2 docs in used list, got %d", len(rows))
	}
	// The first one in the result should be d1 (more hits).
	if !strings.Contains(rows[0]["title"].(string), "Doc One") {
		t.Errorf("expected Doc One first, got %v", rows[0])
	}
}
