package internal

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/ledongthuc/pdf"
	"github.com/pkoukk/tiktoken-go"
	"golang.org/x/net/html"
)

// ==================== Reference Library: Types ====================
//
// Write surface: methods on *DatabaseManager (AddReference, DeleteReference).
// Read surface: free functions in reference_query.go taking *sql.DB.
// Schema: schema.ReferenceTables / schema.ReferenceIndexes, run by
// DatabaseManager.InitSchema(). There is no separate ReferenceDB struct
// — every reference write goes through the unified connection so two
// surfaces can never diverge.

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

// ErrAlreadyExists is returned by DatabaseManager.AddReference when a
// document with the same primary-key id is already present. Callers who
// want update semantics must delete and re-insert. The previous
// INSERT OR REPLACE default silently overwrote metadata and could orphan
// chunks under the old non-cascading schema.
var ErrAlreadyExists = errors.New("reference document already exists")

// ==================== Reference Library: File Parsers ====================

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

// GenerateReferenceID generates a unique reference ID.
func GenerateReferenceID() string {
	timestamp := time.Now().UnixNano()
	h := sha256.Sum256([]byte(fmt.Sprintf("ref-%d-%d", timestamp, time.Now().UnixNano())))
	return hex.EncodeToString(h[:])[:12]
}

// HashContent generates a SHA256 hash of content for deduplication
func HashContent(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// ==================== Reference Library: Chunking ====================

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
