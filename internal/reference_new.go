package internal

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/ledongthuc/pdf"
	"github.com/pkoukk/tiktoken-go"
	"golang.org/x/net/html"

	"mpm/internal/config"
)

// isUniqueConstraintError reports whether err is a SQLite UNIQUE/PRIMARY KEY
// constraint violation. The modernc.org/sqlite driver returns these errors
// with the message "constraint failed: UNIQUE constraint failed: <col>" or
// "constraint failed: PRIMARY KEY constraint failed: <col>". A substring
// match is sufficient — we only need to distinguish "already exists" from
// other write failures for the AddReference error path.
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "PRIMARY KEY constraint failed")
}

// ReferenceDoc represents a reference document in the database
type ReferenceDoc struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	SourcePath   string   `json:"source_path"`
	SourceType   string   `json:"source_type"`
	Tags         []string `json:"tags"`
	ImportReason string   `json:"import_reason"`
	Content      string   `json:"content"`
	ContentHash  string   `json:"content_hash"`
	TotalChunks  int      `json:"total_chunks"`
	LastIndexed  string   `json:"last_indexed"`
	Created      string   `json:"created"`
}

// ReferenceChunk represents a chunk of a reference document
type ReferenceChunk struct {
	ID         string `json:"id"`
	DocID      string `json:"doc_id"`
	ChunkIndex int    `json:"chunk_index"`
	Section    string `json:"section,omitempty"`
	Content    string `json:"content"`
	SourcePath string `json:"source_path"`
}

// ErrAlreadyExists is returned by AddReference when a document with the
// same primary-key id is already present. Callers who want update semantics
// should use UpdateReference explicitly — silent overwrite via INSERT OR
// REPLACE was the previous default and produced orphan chunks under the
// inline-Init() schema and confused audit history.
var ErrAlreadyExists = errors.New("reference document already exists")

// ReferenceDB manages reference documents and chunks in SQLite
type ReferenceDB struct {
	DatabasePath string
	db           *SQLiteConnection
}

// NewReferenceDB creates a new reference database
// Updated for new path structure: flowbyte/mpm/src/db/mpm.db (consolidated)
func NewReferenceDB(dbPath string) *ReferenceDB {
	if dbPath == "" {
		// Use the correct MPM database path
		dbPath = filepath.Join(config.GetMPMDir(), "src", "db", "mpm.db")
	}
	return &ReferenceDB{DatabasePath: dbPath}
}

// Init opens the connection and ensures the reference library schema exists.
// The CREATE TABLE statements come from schema.ReferenceTables and
// schema.ReferenceIndexes — the single source of truth also run by
// DatabaseManager.initUnifiedSchema() in production. Keeping both startup
// paths pointed at the same SQL slice eliminates the previous drift where
// ReferenceDB had a stale, schema-divergent inline subset (missing
// reference_interactions, no CASCADE on chunks, no content column).
func (db *ReferenceDB) Init() error {
	if err := os.MkdirAll(filepath.Dir(db.DatabasePath), 0755); err != nil {
		return err
	}
	conn, err := NewSQLiteConnection(db.DatabasePath)
	if err != nil {
		return err
	}
	db.db = conn
	for _, q := range ReferenceTables {
		if _, err := db.db.Exec(q); err != nil {
			return fmt.Errorf("reference schema failed: %w\nSQL: %s", err, q)
		}
	}
	for _, q := range ReferenceIndexes {
		if _, err := db.db.Exec(q); err != nil {
			return fmt.Errorf("reference index failed: %w\nSQL: %s", err, q)
		}
	}
	return nil
}

// AddReference inserts a new reference document. Returns ErrAlreadyExists
// if a document with the same id is already present — callers wanting
// update semantics must use UpdateReference explicitly. Plain INSERT
// replaces the previous INSERT OR REPLACE, which silently overwrote
// metadata and could orphan chunks under the old non-cascading schema.
//
// Writes into file_path (the schema column name) from doc.SourcePath
// (the struct field name). Both are kept; do not rename without a
// migration — legacy code reads SourcePath from the struct.
func (db *ReferenceDB) AddReference(doc *ReferenceDoc) error {
	if db == nil || db.db == nil {
		return fmt.Errorf("database not initialized")
	}
	if doc == nil {
		return fmt.Errorf("nil reference doc")
	}
	tagsJSON, _ := MarshalJSON(doc.Tags)
	res, err := db.db.Exec(`
		INSERT INTO reference_docs
		(id, title, file_path, source_path, source_type, tags, content, content_hash,
		 import_reason, total_chunks, last_indexed, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), CURRENT_TIMESTAMP))
	`,
		doc.ID, doc.Title, doc.SourcePath, doc.SourcePath, doc.SourceType,
		tagsJSON, doc.Content, doc.ContentHash, doc.ImportReason,
		doc.TotalChunks, doc.LastIndexed, doc.Created)
	if err != nil {
		if isUniqueConstraintError(err) {
			return fmt.Errorf("add reference %q: %w", doc.ID, ErrAlreadyExists)
		}
		return fmt.Errorf("add reference %q: %w", doc.ID, err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Defensive: some drivers return 0 rows on INSERT-OR-IGNORE-style
		// conflicts. With plain INSERT this should not happen, but guard
		// against driver quirks so we never lie about persistence.
		return fmt.Errorf("add reference %q: %w", doc.ID, ErrAlreadyExists)
	}
	return nil
}

// AddChunk inserts a chunk for a reference document. ON CONFLICT(id) DO
// NOTHING makes the operation idempotent: re-running an ingest pipeline
// against a partially-populated chunk table no longer duplicates rows or
// fails on the primary-key constraint.
func (db *ReferenceDB) AddChunk(chunk *ReferenceChunk) error {
	if db == nil || db.db == nil {
		return fmt.Errorf("database not initialized")
	}
	if chunk == nil {
		return fmt.Errorf("nil chunk")
	}
	_, err := db.db.Exec(`
		INSERT INTO reference_chunks
		(id, doc_id, chunk_index, section, content, source_path)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, chunk.ID, chunk.DocID, chunk.ChunkIndex, chunk.Section, chunk.Content, chunk.SourcePath)
	return err
}

// GetReference retrieves a reference by ID or file_path in a single query.
// The previous implementation made two round trips (one for the row, one
// for tags) and silently dropped the import_reason column. Selecting every
// column we need in one SELECT — including the tags JSON string and
// import_reason — fixes both: one round trip, no field loss.
//
// tags lives in the same row (stored as a JSON-encoded string in TEXT), so
// no join or aggregation is required. json_group_array would wrap the
// stored JSON in another array, double-escaping the inner quotes; a plain
// SELECT avoids that tax entirely.
func (db *ReferenceDB) GetReference(idOrPath string) (*ReferenceDoc, error) {
	if db == nil || db.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	var doc ReferenceDoc
	var tagsJSON sql.NullString
	err := db.db.QueryRow(`
		SELECT id, title, file_path, source_type, tags,
		       content, content_hash, import_reason,
		       total_chunks, last_indexed, created_at
		FROM reference_docs
		WHERE id = ? OR file_path = ?
		LIMIT 1
	`, idOrPath, idOrPath).Scan(
		&doc.ID, &doc.Title, &doc.SourcePath, &doc.SourceType, &tagsJSON,
		&doc.Content, &doc.ContentHash, &doc.ImportReason,
		&doc.TotalChunks, &doc.LastIndexed, &doc.Created,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("reference not found: %s", idOrPath)
	}
	if err != nil {
		return nil, fmt.Errorf("get reference %q: %w", idOrPath, err)
	}
	if tagsJSON.Valid && strings.TrimSpace(tagsJSON.String) != "" && tagsJSON.String != "null" {
		_ = UnmarshalJSON(tagsJSON.String, &doc.Tags)
	}
	return &doc, nil
}

// GetChunksByDocID retrieves all chunks for a document
func (db *ReferenceDB) GetChunksByDocID(docID string) ([]*ReferenceChunk, error) {
	rows, err := db.db.Query(`
		SELECT id, doc_id, chunk_index, section, content, source_path
		FROM reference_chunks 
		WHERE doc_id = ?
		ORDER BY chunk_index
	`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []*ReferenceChunk
	for rows.Next() {
		var chunk ReferenceChunk
		err := rows.Scan(&chunk.ID, &chunk.DocID, &chunk.ChunkIndex, &chunk.Section, &chunk.Content, &chunk.SourcePath)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, &chunk)
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return chunks, nil
}

// ListReferences lists all reference documents
func (db *ReferenceDB) ListReferences() ([]*ReferenceDoc, error) {
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	if db.db == nil {
		if err := db.Init(); err != nil {
			return nil, err
		}
	}

	rows, err := db.db.Query(`
		SELECT id, title, file_path, source_type, tags, content, content_hash,
		       import_reason, total_chunks, last_indexed, created_at
		FROM reference_docs
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []*ReferenceDoc
	for rows.Next() {
		var doc ReferenceDoc
		var tagsJSON string
		err := rows.Scan(
			&doc.ID, &doc.Title, &doc.SourcePath, &doc.SourceType, &tagsJSON,
			&doc.Content, &doc.ContentHash, &doc.ImportReason,
			&doc.TotalChunks, &doc.LastIndexed, &doc.Created,
		)
		if err != nil {
			return nil, err
		}
		if tagsJSON != "" {
			_ = UnmarshalJSON(tagsJSON, &doc.Tags)
		}
		docs = append(docs, &doc)
	}

	if err = rows.Err(); err != nil {
		return nil, err
	}

	return docs, nil
}

// DeleteReference removes a reference, its chunks, and its audit rows
// (reference_interactions, admission_log) in a single transaction. Without
// the transaction, a failure between the chunk delete and the doc delete
// would leave orphan chunks pointing at a missing parent row — even with
// ON DELETE CASCADE in the schema, the two deletes here span tables that
// are NOT linked by FK to one another (interactions and admission_log are
// not children of reference_chunks). Wrapping in a tx makes the whole
// fan-out atomic.
func (db *ReferenceDB) DeleteReference(id string) error {
	if db == nil || db.db == nil {
		return fmt.Errorf("database not initialized")
	}
	tx, err := db.db.Begin()
	if err != nil {
		return fmt.Errorf("delete reference: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.Exec(`DELETE FROM reference_chunks WHERE doc_id = ?`, id); err != nil {
		return fmt.Errorf("delete reference chunks: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM reference_interactions WHERE doc_id = ?`, id); err != nil {
		return fmt.Errorf("delete reference interactions: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM admission_log WHERE doc_id = ?`, id); err != nil {
		return fmt.Errorf("delete admission log: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM reference_docs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete reference doc: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete reference commit: %w", err)
	}
	committed = true
	return nil
}

// GetReferenceStats returns statistics about the reference library
func (db *ReferenceDB) GetReferenceStats() (map[string]interface{}, error) {
	stats := map[string]interface{}{
		"total_documents": 0,
		"total_chunks":    0,
	}

	var docCount, chunkCount int
	err := db.db.QueryRow(`SELECT COUNT(*) FROM reference_docs`).Scan(&docCount)
	if err != nil {
		return stats, err
	}
	err = db.db.QueryRow(`SELECT COUNT(*) FROM reference_chunks`).Scan(&chunkCount)
	if err != nil {
		return stats, err
	}

	stats["total_documents"] = docCount
	stats["total_chunks"] = chunkCount

	return stats, nil
}

// ParsePDF extracts raw text from a PDF file using a pure Go implementation.
// Eliminates the need for the external python pdfplumber dependency.
func ParsePDF(filePath string) (string, error) {
	f, r, err := pdf.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open PDF %s: %w", filePath, err)
	}
	defer f.Close()

	var buf bytes.Buffer
	b, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("failed to extract text from PDF: %w", err)
	}

	buf.ReadFrom(b)
	return buf.String(), nil
}

// ParseEPUB extracts text from an EPUB file by unzipping the archive
// and stripping HTML tags from its internal content files.
// Eliminates the need for the external Calibre ebook-convert tool.
func ParseEPUB(filePath string) (string, error) {
	r, err := zip.OpenReader(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open EPUB archive %s: %w", filePath, err)
	}
	defer r.Close()

	var extractedText strings.Builder

	// Iterate through the files in the zip archive
	for _, f := range r.File {
		// EPUB text content is primarily stored in HTML or XHTML files
		name := strings.ToLower(f.Name)
		if strings.HasSuffix(name, ".html") || strings.HasSuffix(name, ".xhtml") {
			rc, err := f.Open()
			if err != nil {
				// Log this if you have a logger, otherwise skip unreadable fragments
				continue
			}

			text := stripHTML(rc)
			if text != "" {
				extractedText.WriteString(text)
				extractedText.WriteString("\n\n") // Maintain some structural separation
			}
			rc.Close()
		}
	}

	if extractedText.Len() == 0 {
		return "", fmt.Errorf("no readable text content found in EPUB: %s", filePath)
	}

	return extractedText.String(), nil
}

// stripHTML is a lightweight helper to remove HTML tags and extract
// clean text from the EPUB's internal chapters for vectorization.
func stripHTML(r io.Reader) string {
	z := html.NewTokenizer(r)
	var text strings.Builder

	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			// End of file
			return text.String()
		case html.TextToken:
			t := z.Token()
			cleanText := strings.TrimSpace(t.Data)
			if len(cleanText) > 0 {
				text.WriteString(cleanText)
				text.WriteString(" ")
			}
		}
	}
}

// Section represents a markdown section
type Section struct {
	Section string
	Content string
}

// Chunk represents a text chunk
type Chunk struct {
	Index   int
	Section string
	Content string
}

// DetectSourceType detects the file type based on extension
func DetectSourceType(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".pdf":
		return "pdf"
	case ".epub":
		return "epub"
	case ".md":
		return "markdown"
	case ".txt":
		return "text"
	case ".json":
		return "json"
	case ".html", ".xhtml":
		return "html"
	default:
		return "unknown"
	}
}

// GenerateReferenceID generates a unique reference ID
func GenerateReferenceID() string {
	timestamp := time.Now().UnixNano()
	hash := sha256.Sum256([]byte(fmt.Sprintf("ref-%d-%d", timestamp, time.Now().UnixNano())))
	return hex.EncodeToString(hash[:])[:12]
}

// ParseMarkdownSections parses markdown content into sections
// Each ## or ### header starts a new section with the header text as name
func ParseMarkdownSections(content string) []Section {
	sections := []Section{}
	lines := strings.Split(content, "\n")
	var currentSection *Section

	for _, line := range lines {
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
			if currentSection != nil && currentSection.Content != "" {
				sections = append(sections, *currentSection)
			}
			title := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			title = strings.TrimSpace(strings.TrimPrefix(title, "### "))
			currentSection = &Section{
				Section: title,
				Content: "",
			}
		} else if currentSection != nil {
			currentSection.Content += line + "\n"
		}
	}

	if currentSection != nil && currentSection.Content != "" {
		sections = append(sections, *currentSection)
	}

	return sections
}

// ChunkReference chunks reference content into smaller pieces
// Prefers section-based chunks (markdown headers), falls back to word-boundary chunks
func ChunkReference(content string, chunkSize int) []Chunk {
	chunks := []Chunk{}

	// Try section-based chunking first
	sections := ParseMarkdownSections(content)
	if len(sections) > 1 {
		for i, sec := range sections {
			chunks = append(chunks, Chunk{
				Index:   i,
				Section: sec.Section,
				Content: strings.TrimSpace(sec.Content),
			})
		}
		return chunks
	}

	// Fall back to word-boundary chunking
	words := strings.Fields(content)
	var currentChunk strings.Builder
	idx := 0

	for _, word := range words {
		if currentChunk.Len()+len(word)+1 > chunkSize && currentChunk.Len() > 0 {
			chunks = append(chunks, Chunk{
				Index:   idx,
				Content: strings.TrimSpace(currentChunk.String()),
			})
			idx++
			currentChunk.Reset()
		}
		if currentChunk.Len() > 0 {
			currentChunk.WriteString(" ")
		}
		currentChunk.WriteString(word)
	}

	if currentChunk.Len() > 0 {
		chunks = append(chunks, Chunk{
			Index:   idx,
			Content: strings.TrimSpace(currentChunk.String()),
		})
	}

	return chunks
}

// tiktokenEnc caches the cl100k_base encoder after first successful load.
var (
	tiktokenEnc   *tiktoken.Tiktoken
	tiktokenErr   error
	tiktokenEncMu sync.Mutex
	tiktokenReady atomic.Bool // true once initialization succeeded
)

// getTiktokenEncoder returns a shared cl100k_base encoder.
// Initialization is retried on each call until it succeeds — useful for
// transient failures (e.g., library load races in goroutines).
// Subsequent calls after success return the cached encoder with no lock.
func getTiktokenEncoder() (*tiktoken.Tiktoken, error) {
	if tiktokenReady.Load() {
		return tiktokenEnc, tiktokenErr
	}
	tiktokenEncMu.Lock()
	defer tiktokenEncMu.Unlock()
	if tiktokenReady.Load() { // double-check after lock
		return tiktokenEnc, tiktokenErr
	}
	tiktokenEnc, tiktokenErr = tiktoken.GetEncoding("cl100k_base")
	if tiktokenErr == nil {
		tiktokenReady.Store(true)
	}
	return tiktokenEnc, tiktokenErr
}

// CountTokens returns the number of cl100k_base tokens in a string.
// Uses tiktoken for accurate LLM context window sizing.
// The encoder is cached after first use for performance.
func CountTokens(text string) (int, error) {
	encoder, err := getTiktokenEncoder()
	if err != nil {
		return 0, fmt.Errorf("failed to load tiktoken encoder: %w", err)
	}
	tokens := encoder.Encode(text, nil, nil)
	return len(tokens), nil
}

// ChunkByTokens chunks reference content by token count (not character count).
// Uses tiktoken cl100k_base encoding for accurate LLM context window sizing.
// chunkSize is the target token count per chunk (64-8192; 64-2048 recommended).
// The encoder is cached after first use for performance.
// Returns chunks with Content field containing the text, Index for ordering.
func ChunkByTokens(content string, chunkSize int) ([]Chunk, error) {
	if chunkSize <= 0 {
		chunkSize = 512
	}
	if chunkSize < 1 {
		chunkSize = 1
	}
	if chunkSize > 8192 {
		chunkSize = 8192
	}

	// Fast path: empty content returns nil before touching tiktoken
	if strings.TrimSpace(content) == "" {
		return nil, nil
	}

	encoder, err := getTiktokenEncoder()
	if err != nil {
		return nil, fmt.Errorf("failed to load tiktoken encoder: %w", err)
	}

	// Fast path: if total tokens <= chunkSize, return single chunk
	fullTokens := encoder.Encode(content, nil, nil)
	if len(fullTokens) <= chunkSize {
		return []Chunk{{Index: 0, Content: strings.TrimSpace(content)}}, nil
	}

	// Multi-chunk: batch encode once, iterate in chunkSize increments
	// Decode each chunk's token slice, then trim to nearest space to prevent mid-word slicing
	var chunks []Chunk
	for i := 0; i < len(fullTokens); i += chunkSize {
		end := i + chunkSize
		if end > len(fullTokens) {
			end = len(fullTokens)
		}

		// Decode token slice to text
		chunkText := encoder.Decode(fullTokens[i:end])

		// If not the last chunk, trim to nearest space to avoid mid-word breaks.
		// Use rune-level LastIndex to handle multi-byte UTF-8 characters safely.
		if end < len(fullTokens) {
			chunkRunes := []rune(chunkText)
			lastSpace := -1
			for j := len(chunkRunes) - 1; j >= 0; j-- {
				if unicode.IsSpace(chunkRunes[j]) {
					lastSpace = j
					break
				}
			}
			if lastSpace > 0 {
				chunkText = string(chunkRunes[:lastSpace])
			}
		}

		chunkText = strings.TrimSpace(chunkText)
		if chunkText != "" {
			chunks = append(chunks, Chunk{
				Index:   len(chunks),
				Content: chunkText,
			})
		}
	}

	return chunks, nil
}

// GetReferenceCount returns the number of reference documents
func (rdb *ReferenceDB) GetReferenceCount() (int, error) {
	if rdb.db == nil {
		return 0, fmt.Errorf("database not initialized")
	}
	var count int
	err := rdb.db.QueryRow(`SELECT COUNT(*) FROM reference_docs`).Scan(&count)
	return count, err
}

// GetReferenceCount returns the number of reference documents via ReferenceStore
func (rs *ReferenceStore) GetReferenceCount() (int, error) {
	if rs.MetadataDB == nil {
		return 0, fmt.Errorf("database not initialized")
	}
	return rs.MetadataDB.GetReferenceCount()
}

// HashContent generates a SHA256 hash of content for deduplication
func HashContent(content string) string {
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:])
}

// StripHTML removes HTML tags from content
func StripHTML(htmlContent string) string {
	r := strings.NewReader(htmlContent)
	z := html.NewTokenizer(r)
	var text strings.Builder

	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return text.String()
		case html.TextToken:
			t := z.Token()
			cleanText := strings.TrimSpace(t.Data)
			if len(cleanText) > 0 {
				text.WriteString(cleanText)
				text.WriteString(" ")
			}
		}
	}
}
