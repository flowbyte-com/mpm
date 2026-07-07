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