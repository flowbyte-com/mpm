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

// FindReferenceBySourcePath returns the reference doc whose file_path
// matches sourcePath exactly, or (nil, nil) if no match. Used by ingest
// paths that want to re-ingest into the same doc instead of creating a
// duplicate — the chunk_hash diff requires stable doc identity across
// ingests to actually fire.
//
// file_path is not currently UNIQUE in the schema (legacy rows predate
// the unification), so the LIMIT 1 picks whichever the index returns
// first. In practice a single source path maps to one doc — the
// caller is expected to use this helper as part of the ingest flow
// where they own the source path key.
func FindReferenceBySourcePath(db *sql.DB, sourcePath string) (*ReferenceDoc, error) {
	if db == nil {
		return nil, fmt.Errorf("FindReferenceBySourcePath: database not initialized")
	}
	if sourcePath == "" {
		return nil, fmt.Errorf("FindReferenceBySourcePath: empty sourcePath")
	}
	var doc ReferenceDoc
	var tagsJSON sql.NullString
	err := db.QueryRow(`
		SELECT id, title, file_path, source_type, tags,
		       content, content_hash, import_reason,
		       total_chunks, last_indexed, created_at
		FROM reference_docs
		WHERE file_path = ?
		LIMIT 1
	`, sourcePath).Scan(
		&doc.ID, &doc.Title, &doc.SourcePath, &doc.SourceType, &tagsJSON,
		&doc.Content, &doc.ContentHash, &doc.ImportReason,
		&doc.TotalChunks, &doc.LastIndexed, &doc.Created,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("FindReferenceBySourcePath %q: %w", sourcePath, err)
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

// ==================== Reference Library: Chunk Diff ====================
//
// Re-ingesting a reference doc that has not changed would otherwise
// rewrite every chunk row — wasted IO, audit-log noise (each chunk
// upsert bumps reference_interactions.chunk_id references), and
// pointless downstream work when per-chunk embedding is added later.
//
// DiffChunks classifies new chunks against the rows currently stored
// for the given doc_id, using content_hash as the identity key (NOT
// the chunk id — old chunks had random ids so id-matching would
// always classify them as different). Callers (AddReference) apply
// the diff: insert the Inserted set, delete the Deleted orphans, and
// leave the Unchanged set alone.

// ChunkDiff is the result of comparing a set of new chunks against the
// rows currently stored for a single document.
//
// Inserted: new chunks to write (no existing row with the same
// content_hash for this doc_id).
// Updated:  new chunks whose content_hash matches an existing row's
// content_hash but whose id differs — kept in Inserted for the caller
// to write, classified here only for accounting.
// Unchanged: new chunks whose content_hash matches an existing row's
// id (the typical re-ingest path for unchanged content).
// Deleted:  existing rows whose content_hash is not in the new set.
// The caller removes them so they do not orphan.
type ChunkDiff struct {
	Inserted  []ReferenceChunk
	Updated   []ReferenceChunk
	Unchanged []ReferenceChunk
	Deleted   []string // chunk ids to remove
}

// DiffChunks classifies newChunks against the rows currently stored for
// docID. Caller applies the diff (insert + delete) atomically —
// DiffChunks does not mutate the database. content_hash is the
// identity key, so the diff is robust to chunk-id churn (existing
// rows with random ids will not match the deterministic ids callers
// may now generate — but their content hashes still match if the
// content is unchanged).
func DiffChunks(db *sql.DB, docID string, newChunks []ReferenceChunk) (*ChunkDiff, error) {
	if db == nil {
		return nil, fmt.Errorf("DiffChunks: database not initialized")
	}
	diff := &ChunkDiff{
		Inserted:  []ReferenceChunk{},
		Updated:   []ReferenceChunk{},
		Unchanged: []ReferenceChunk{},
		Deleted:   []string{},
	}

	// Read existing (id, content_hash) for the doc. Missing content_hash
	// (rows from before the migration) cannot be matched by content, so
	// they fall into the "always insert / never delete" path — the
	// migration backfill is the operator's responsibility.
	rows, err := db.Query(
		`SELECT id, content_hash FROM reference_chunks WHERE doc_id = ?`, docID,
	)
	if err != nil {
		return nil, fmt.Errorf("DiffChunks: query existing: %w", err)
	}
	existingByHash := make(map[string]string) // content_hash -> chunk id
	existingIDs := make(map[string]bool)      // all ids (for orphan detection)
	for rows.Next() {
		var id, hash sql.NullString
		if err := rows.Scan(&id, &hash); err != nil {
			rows.Close()
			return nil, fmt.Errorf("DiffChunks: scan: %w", err)
		}
		if !id.Valid {
			continue
		}
		existingIDs[id.String] = true
		if hash.Valid && hash.String != "" {
			existingByHash[hash.String] = id.String
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("DiffChunks: rows.Err: %w", err)
	}
	rows.Close()

	// Track which existing ids are still referenced by new chunks so we
	// can detect orphans (existing ids not touched by any new chunk).
	referencedExisting := make(map[string]bool)

	for _, newChunk := range newChunks {
		newHash := ComputeChunkHash(newChunk.Content)
		if existingID, ok := existingByHash[newHash]; ok {
			// Content matches an existing row — unchanged.
			referencedExisting[existingID] = true
			unchangedChunk := newChunk
			unchangedChunk.ID = existingID
			unchangedChunk.ContentHash = newHash
			diff.Unchanged = append(diff.Unchanged, unchangedChunk)
			continue
		}
		// No matching existing row by content hash — this is a new
		// chunk. Caller writes it. If a row exists by id but with a
		// different (or NULL) content_hash, we count it as Updated for
		// accounting but the caller still inserts it via ON CONFLICT.
		if newChunk.ID != "" && existingIDs[newChunk.ID] {
			referencedExisting[newChunk.ID] = true
			updatedChunk := newChunk
			updatedChunk.ContentHash = newHash
			diff.Updated = append(diff.Updated, updatedChunk)
			continue
		}
		insertedChunk := newChunk
		insertedChunk.ContentHash = newHash
		diff.Inserted = append(diff.Inserted, insertedChunk)
	}

	// Orphans: existing rows whose id is not referenced by any new chunk.
	for id := range existingIDs {
		if !referencedExisting[id] {
			diff.Deleted = append(diff.Deleted, id)
		}
	}

	return diff, nil
}

// DeleteOrphanChunks removes the given chunk ids from reference_chunks.
// Used by AddReference to apply the Deleted half of a ChunkDiff. Splits
// into a batched delete to keep individual statements small even for
// large reference libraries (e.g., a 1000-chunk manual rewriting
// 800 chunks).
func DeleteOrphanChunks(db *sql.DB, chunkIDs []string) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("DeleteOrphanChunks: database not initialized")
	}
	if len(chunkIDs) == 0 {
		return 0, nil
	}
	deleted := 0
	for _, id := range chunkIDs {
		res, err := db.Exec(`DELETE FROM reference_chunks WHERE id = ?`, id)
		if err != nil {
			return deleted, fmt.Errorf("DeleteOrphanChunks: delete %q: %w", id, err)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
	}
	return deleted, nil
}

