package internal

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"
	"golang.org/x/net/html"

	"mpm/internal/config"
)

// ReferenceDoc represents a reference document in the database
type ReferenceDoc struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	SourcePath   string   `json:"source_path"`
	SourceType   string   `json:"source_type"`
	Tags         []string `json:"tags"`
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

// ReferenceDB manages reference documents and chunks in SQLite
type ReferenceDB struct {
	DatabasePath string
	db           *SQLiteConnection
}

// NewReferenceDB creates a new reference database
// Updated for new path structure: workspace/src/db/mpm.db (consolidated)
func NewReferenceDB(dbPath string) *ReferenceDB {
	if dbPath == "" {
		// Use config.GetDBPath() for portable installations
		dbPath = config.GetDBPath("mpm")
	}
	return &ReferenceDB{DatabasePath: dbPath}
}

// Init initializes the database and creates tables
func (db *ReferenceDB) Init() error {
	err := os.MkdirAll(filepath.Dir(db.DatabasePath), 0755)
	if err != nil {
		return err
	}

	database, err := NewSQLiteConnection(db.DatabasePath)
	if err != nil {
		return err
	}
	db.db = database

	// Create reference_docs table
	_, err = db.db.Exec(`
		CREATE TABLE IF NOT EXISTS reference_docs (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			source_path TEXT NOT NULL,
			source_type TEXT,
			tags TEXT,
			total_chunks INTEGER DEFAULT 0,
			last_indexed TEXT,
			created TEXT NOT NULL
		)
	`)
	if err != nil {
		return err
	}

	// Create reference_chunks table
	_, err = db.db.Exec(`
		CREATE TABLE IF NOT EXISTS reference_chunks (
			id TEXT PRIMARY KEY,
			doc_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			section TEXT,
			content TEXT NOT NULL,
			source_path TEXT,
			FOREIGN KEY (doc_id) REFERENCES reference_docs(id)
		)
	`)
	if err != nil {
		return err
	}

	// Create index for doc_id
	_, err = db.db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_chunks_doc_id ON reference_chunks(doc_id)
	`)
	if err != nil {
		return err
	}

	return nil
}

// AddReference adds a reference document
func (db *ReferenceDB) AddReference(doc *ReferenceDoc) error {
	if db == nil || db.db == nil {
		return fmt.Errorf("database not initialized")
	}
	tagsJSON, _ := MarshalJSON(doc.Tags)
	_, err := db.db.Exec(`
		INSERT OR REPLACE INTO reference_docs 
		(id, title, source_path, source_type, tags, total_chunks, last_indexed, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, doc.ID, doc.Title, doc.SourcePath, doc.SourceType, tagsJSON, doc.TotalChunks, doc.LastIndexed, doc.Created)
	if err != nil {
		return err
	}
	return nil
}

// AddChunk adds a chunk to a reference document
func (db *ReferenceDB) AddChunk(chunk *ReferenceChunk) error {
	_, err := db.db.Exec(`
		INSERT INTO reference_chunks 
		(id, doc_id, chunk_index, section, content, source_path)
		VALUES (?, ?, ?, ?, ?, ?)
	`, chunk.ID, chunk.DocID, chunk.ChunkIndex, chunk.Section, chunk.Content, chunk.SourcePath)
	return err
}

// GetReference retrieves a reference by ID or path
func (db *ReferenceDB) GetReference(idOrPath string) (*ReferenceDoc, error) {
	var doc ReferenceDoc
	// Scan without tags first
	err := db.db.QueryRow(`
		SELECT id, title, source_path, source_type, total_chunks, last_indexed, created
		FROM reference_docs 
		WHERE id = ? OR source_path = ?
	`, idOrPath, idOrPath).Scan(
		&doc.ID, &doc.Title, &doc.SourcePath, &doc.SourceType, &doc.TotalChunks, &doc.LastIndexed, &doc.Created,
	)
	if err != nil {
		return nil, fmt.Errorf("reference not found: %v", err)
	}

	// Parse tags
	var tagsJSON string
	err = db.db.QueryRow(`SELECT tags FROM reference_docs WHERE id = ?`, doc.ID).Scan(&tagsJSON)
	if err == nil && tagsJSON != "" {
		_ = UnmarshalJSON(tagsJSON, &doc.Tags)
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
		SELECT id, title, source_path, source_type, tags, total_chunks, last_indexed, created
		FROM reference_docs
		ORDER BY created DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []*ReferenceDoc
	for rows.Next() {
		var doc ReferenceDoc
		var tagsJSON string
		err := rows.Scan(&doc.ID, &doc.Title, &doc.SourcePath, &doc.SourceType, &tagsJSON, &doc.TotalChunks, &doc.LastIndexed, &doc.Created)
		if err != nil {
			return nil, err
		}
		// Parse tags JSON
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

// DeleteReference removes a reference and all its chunks
func (db *ReferenceDB) DeleteReference(id string) error {
	_, err := db.db.Exec(`DELETE FROM reference_chunks WHERE doc_id = ?`, id)
	if err != nil {
		return err
	}
	_, err = db.db.Exec(`DELETE FROM reference_docs WHERE id = ?`, id)
	return err
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
func ParseMarkdownSections(content string) []Section {
	sections := []Section{}
	lines := strings.Split(content, "\n")
	var currentSection *Section

	for _, line := range lines {
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
			if currentSection != nil {
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

	if currentSection != nil {
		sections = append(sections, *currentSection)
	}

	return sections
}

// ChunkReference chunks reference content into smaller pieces
func ChunkReference(content string, chunkSize int) []Chunk {
	chunks := []Chunk{}
	words := strings.Fields(content)
	var currentChunk strings.Builder

	for _, word := range words {
		if currentChunk.Len()+len(word)+1 > chunkSize && currentChunk.Len() > 0 {
			chunks = append(chunks, Chunk{
				Content: strings.TrimSpace(currentChunk.String()),
			})
			currentChunk.Reset()
		}
		if currentChunk.Len() > 0 {
			currentChunk.WriteString(" ")
		}
		currentChunk.WriteString(word)
	}

	if currentChunk.Len() > 0 {
		chunks = append(chunks, Chunk{
			Content: strings.TrimSpace(currentChunk.String()),
		})
	}

	return chunks
}
