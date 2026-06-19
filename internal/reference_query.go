package internal

import (
	"database/sql"
	"fmt"
	"strings"
)

// ==================== Reference Library: Free-Function Read API ====================
//
// The reference write surface lives on *DatabaseManager (AddReference,
// DeleteReference) and runs through dm.db — a single connection shared with
// the rest of MPM. The read surface intentionally lives as free functions
// taking *sql.DB instead of on the manager: reads are single-statement,
// don't need transactions, and a free-function API matches the existing
// pattern (ShredMemory, DeleteByID, ShredSession, ShredTopic).
//
// Schema is owned by DatabaseManager.InitSchema() — callers must construct
// a DatabaseManager (or call InitSchema on a NewDatabaseManagerForDB) before
// invoking these. See internal/schema.go for ReferenceTables /
// ReferenceIndexes, the single source of truth also run by the manager.

// GetReferenceDoc retrieves a single reference by id or file_path in one
// query. The previous two-query path on the legacy ReferenceDB silently
// dropped import_reason; selecting every column we need in one SELECT
// fixes that. tags lives in the same row (JSON-encoded TEXT) so no join
// is required — json_group_array would wrap the stored JSON in another
// array, double-escaping the inner quotes.
func GetReferenceDoc(db *sql.DB, idOrPath string) (*ReferenceDoc, error) {
	if db == nil {
		return nil, fmt.Errorf("GetReferenceDoc: database not initialized")
	}
	var doc ReferenceDoc
	var tagsJSON sql.NullString
	err := db.QueryRow(`
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

// ListReferenceDocs returns every reference, newest first.
func ListReferenceDocs(db *sql.DB) ([]*ReferenceDoc, error) {
	if db == nil {
		return nil, fmt.Errorf("ListReferenceDocs: database not initialized")
	}
	rows, err := db.Query(`
		SELECT id, title, file_path, source_type, tags, content, content_hash,
		       import_reason, total_chunks, last_indexed, created_at
		FROM reference_docs
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list references: %w", err)
	}
	defer rows.Close()

	var docs []*ReferenceDoc
	for rows.Next() {
		var doc ReferenceDoc
		var tagsJSON string
		if err := rows.Scan(
			&doc.ID, &doc.Title, &doc.SourcePath, &doc.SourceType, &tagsJSON,
			&doc.Content, &doc.ContentHash, &doc.ImportReason,
			&doc.TotalChunks, &doc.LastIndexed, &doc.Created,
		); err != nil {
			return nil, err
		}
		if tagsJSON != "" {
			_ = UnmarshalJSON(tagsJSON, &doc.Tags)
		}
		docs = append(docs, &doc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return docs, nil
}

// GetReferenceChunksByDocID returns the chunks for a document in
// chunk_index order. Used by ingest pipelines and tests.
func GetReferenceChunksByDocID(db *sql.DB, docID string) ([]*ReferenceChunk, error) {
	if db == nil {
		return nil, fmt.Errorf("GetReferenceChunksByDocID: database not initialized")
	}
	rows, err := db.Query(`
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
		if err := rows.Scan(
			&chunk.ID, &chunk.DocID, &chunk.ChunkIndex,
			&chunk.Section, &chunk.Content, &chunk.SourcePath,
		); err != nil {
			return nil, err
		}
		chunks = append(chunks, &chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return chunks, nil
}

// CountReferences returns the number of reference documents.
func CountReferences(db *sql.DB) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("CountReferences: database not initialized")
	}
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM reference_docs`).Scan(&n)
	return n, err
}

// GetReferenceStats returns document and chunk counts as a map. The map
// shape matches the legacy ReferenceDB.GetReferenceStats so UI code
// consuming "total_documents" / "total_chunks" continues to work.
func GetReferenceStats(db *sql.DB) (map[string]interface{}, error) {
	stats := map[string]interface{}{
		"total_documents": 0,
		"total_chunks":    0,
	}
	if db == nil {
		return stats, fmt.Errorf("GetReferenceStats: database not initialized")
	}
	var docCount, chunkCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM reference_docs`).Scan(&docCount); err != nil {
		return stats, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM reference_chunks`).Scan(&chunkCount); err != nil {
		return stats, err
	}
	stats["total_documents"] = docCount
	stats["total_chunks"] = chunkCount
	return stats, nil
}
