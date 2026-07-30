// reference_tools.go — DM methods for ingesting reference documents
// (PDF, EPUB, HTML, MD, TXT) into the SQLite reference_docs /
// reference_chunks tables.
//
// Two methods:
//   - AddReferenceFromFile: simple entry point (no tags, no reason, default chunk size)
//   - AddReferenceFromFileWith: full entry point with tags, reason, custom chunk size
//
// All file parsing lives here (parseReferenceFile) so the dispatch
// logic — by extension — stays in one place. Both the CLI handler
// (handleRefAdd) and the MCP add_reference tool route through these
// methods, so neither surface can drift in extension support.
package internal

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// AddReferenceFromFile reads a file from disk, parses it by extension,
// chunks the content, and inserts the resulting document + chunks into
// the unified SQLite store. This is the same pipeline the CLI uses
// (mpm kb reference add) — routing both surfaces through one code path
// fixes the prior bug where the MCP add path wrote to the legacy JSON
// store while search read from the chunked SQLite store, leaving MCP
// additions invisible to MCP searches.
//
// Supports: .pdf, .epub, .html/.xhtml, .txt, .md. Unknown extensions are
// treated as plain text. Parse failures short-circuit with a wrapped error.
//
// tags and reason are optional. chunkSize is clamped to the same 64–2048
// range ChunkByTokens enforces internally; 0 or negative falls back to
// the 512 default.
func (dm *DatabaseManager) AddReferenceFromFile(filepath, title string) (map[string]interface{}, error) {
	return dm.AddReferenceFromFileWith(filepath, title, nil, "", 0)
}

// AddReferenceFromFileWith is the extended entry point. Use this when
// callers want to attach tags, an import_reason, or a non-default chunk
// size. The two-argument AddReferenceFromFile delegates here with zero
// values so existing callers (MCP) stay byte-compatible.
func (dm *DatabaseManager) AddReferenceFromFileWith(filepath, title string, tags []string, reason string, chunkSize int) (map[string]interface{}, error) {
	content, err := parseReferenceFile(filepath)
	if err != nil {
		return nil, err
	}
	if title == "" {
		title = filepathBase(filepath)
	}

	chunks, err := ChunkByTokens(content, chunkSize)
	if err != nil {
		return nil, fmt.Errorf("chunk reference: %w", err)
	}

	// Look up an existing doc by source path so re-ingest reuses the
	// doc id. Without this every re-ingest would create a fresh doc
	// (different id) and the chunk_hash diff would never fire — each
	// ingest would land in a new doc row, the old one orphaned. The
	// content_hash comparison below also lets us short-circuit when
	// the file is byte-identical to the last ingest.
	contentHash := HashContent(content)
	existing, err := FindReferenceBySourcePath(dm.db, filepath)
	if err != nil {
		return nil, fmt.Errorf("find existing reference: %w", err)
	}
	if existing != nil && existing.ContentHash == contentHash {
		// Same source, same bytes — nothing to do. Caller already has
		// the doc id; return success with the existing stats.
		return map[string]interface{}{
			"success":      true,
			"id":           existing.ID,
			"title":        existing.Title,
			"total_chunks": existing.TotalChunks,
			"unchanged":    true,
		}, nil
	}

	now := strconv.FormatInt(time.Now().Unix(), 10)
	docID := GenerateID()
	if existing != nil {
		// Source path seen before but content changed: reuse the id so
		// AddReference's chunk diff can replace chunks in place.
		docID = existing.ID
	}
	doc := &ReferenceDoc{
		ID:           docID,
		Title:        title,
		SourcePath:   filepath,
		SourceType:   DetectSourceType(filepath),
		Tags:         tags,
		ImportReason: reason,
		Content:      content,
		ContentHash:  contentHash,
		TotalChunks:  len(chunks),
		LastIndexed:  now,
		Created:      now,
	}

	refChunks := make([]ReferenceChunk, len(chunks))
	for i, c := range chunks {
		refChunks[i] = ReferenceChunk{
			ID:         ComputeChunkID(docID, c.Index, contentHash), // stable across re-ingest of same content
			DocID:      docID,
			ChunkIndex: c.Index,
			Section:    c.Section,
			Content:    c.Content,
			SourcePath: filepath,
		}
	}

	if err := dm.AddReference(doc, refChunks); err != nil {
		return nil, fmt.Errorf("add reference: %w", err)
	}

	// Embed chunks in a separate phase after the chunk-insert tx has
	// committed. Embedding failures are best-effort: a transient
	// provider outage leaves embedding NULL on the affected rows, and
	// a follow-up embed pass (or re-ingest) fills them in. We do NOT
	// fail the ingest call on embedding errors — the chunk rows are
	// already durable, and the operator can retry embedding later.
	embedded, _, _ := dm.EmbedReferenceChunks(context.Background(), docID)

	return map[string]interface{}{
		"success":      true,
		"id":           doc.ID,
		"title":        doc.Title,
		"total_chunks": doc.TotalChunks,
		"embedded":     embedded,
	}, nil
}

// parseReferenceFile reads a file and returns its extracted text content.
// Dispatches by extension: PDF/EPUB use the dedicated parsers, HTML is
// stripped to text, everything else is read as bytes. Errors from the
// underlying parsers are wrapped so callers see a clear failure path.
//
// Mirrors the dispatch logic in cmd/mpm/simple_cmds.go handleRefAdd;
// both surfaces must agree on what counts as supported content. Keep
// them in sync — if a new extension is added here, add it there too.
func parseReferenceFile(filepath string) (string, error) {
	ext := strings.ToLower(filepathExt(filepath))
	switch ext {
	case ".pdf":
		text, err := ParsePDF(filepath)
		if err != nil {
			return "", fmt.Errorf("parse pdf %q: %w", filepath, err)
		}
		return text, nil
	case ".epub":
		text, err := ParseEPUB(filepath)
		if err != nil {
			return "", fmt.Errorf("parse epub %q: %w", filepath, err)
		}
		return text, nil
	case ".html", ".xhtml":
		data, err := os.ReadFile(filepath)
		if err != nil {
			return "", fmt.Errorf("read %q: %w", filepath, err)
		}
		return StripHTML(string(data)), nil
	case ".txt", ".md":
		data, err := os.ReadFile(filepath)
		if err != nil {
			return "", fmt.Errorf("read %q: %w", filepath, err)
		}
		return string(data), nil
	default:
		// Best-effort: read as bytes. Reject obvious binary noise by
		// surfacing the read error; otherwise accept the raw text.
		data, err := os.ReadFile(filepath)
		if err != nil {
			return "", fmt.Errorf("unsupported file type %q: %w", ext, err)
		}
		return string(data), nil
	}
}