package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// embeddingBytes marshals a []float32 to JSON bytes for storage in the
// reference_chunks.embedding BLOB column. Matches the serialization
// memories.embedding uses so the two columns are interchangeable
// (one less thing for vector-search code to special-case).
func embeddingBytes(vec []float32) ([]byte, error) {
	if vec == nil {
		return nil, nil
	}
	return json.Marshal(vec)
}

// ==================== Reference Library: DatabaseManager methods ====================
//
// These methods are the single SQLite write surface for the reference
// library. Both go through dm.WithTx so they inherit:
//   - watchdog.jsonl telemetry via ExecTracked / DBNode
//   - automatic rollback on panic (defer-recover inside WithTx)
//   - consistent commit/rollback handling shared with every other
//     DatabaseManager multi-statement operation
//
// There is no separate ReferenceDB type or connection. Anything that
// wants to write a reference goes through AddReference / DeleteReference
// here — a single connection pool, a single set of transaction rules.

// AddReference upserts a reference document and its chunks, applying
// the chunk_hash diff so unchanged content is not re-written and orphan
// chunks from a previous version of the doc are deleted.
//
// For re-ingest of the same source to actually trigger the chunk diff
// (instead of just creating a duplicate doc), callers are expected to
// look up the existing doc by source_path first and reuse its id. The
// diff path is the only way to reuse the same doc across ingests with
// stable chunk-level identity.
//
// Diff semantics:
//   - chunks whose content_hash already exists for the doc_id are
//     treated as Unchanged and skipped (their id is preserved).
//   - chunks whose content_hash is new are Inserted.
//   - existing chunks whose content_hash is not in the new set are
//     Deleted (orphan cleanup).
//   - the doc row itself is always updated to reflect the latest
//     metadata (title, tags, content_hash, total_chunks, last_indexed)
//     via ON CONFLICT(id) DO UPDATE.
//
// Writes into file_path (the schema column name) from doc.SourcePath
// (the struct field name). Both names are kept; do not rename without a
// migration — legacy code reads SourcePath from the struct.
//
// Chunk inserts use ON CONFLICT(id) DO NOTHING so a partial-pipeline
// retry against an already-populated chunk table doesn't duplicate
// rows or fail on the primary-key constraint.
func (dm *DatabaseManager) AddReference(doc *ReferenceDoc, chunks []ReferenceChunk) error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("AddReference: database not initialized")
	}
	if doc == nil {
		return fmt.Errorf("AddReference: nil reference doc")
	}
	if doc.ContentHash == "" {
		doc.ContentHash = HashContent(doc.Content)
	}

	return dm.WithTx(func(node DBNode) error {
		tagsJSON, _ := MarshalJSON(doc.Tags)

		// Upsert the doc row. ON CONFLICT(id) DO UPDATE keeps metadata
		// fresh on re-ingest; callers that want strict one-shot
		// semantics must check existence first and pick a fresh id.
		_, err := node.ExecTracked(`
			INSERT INTO reference_docs
			(id, title, file_path, source_type, tags, content, content_hash,
			 import_reason, total_chunks, last_indexed, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				title = excluded.title,
				file_path = excluded.file_path,
				source_type = excluded.source_type,
				tags = excluded.tags,
				content = excluded.content,
				content_hash = excluded.content_hash,
				import_reason = excluded.import_reason,
				total_chunks = excluded.total_chunks,
				last_indexed = excluded.last_indexed
		`, 0,
			doc.ID, doc.Title, doc.SourcePath, doc.SourceType, tagsJSON,
			doc.Content, doc.ContentHash, doc.ImportReason,
			doc.TotalChunks, doc.LastIndexed, doc.Created,
		)
		if err != nil {
			return fmt.Errorf("add reference %q: %w", doc.ID, err)
		}

		// Chunk diff: classify new chunks against existing rows for
		// this doc, then apply the diff inside the same tx.
		diff, err := DiffChunks(dm.db, doc.ID, chunks)
		if err != nil {
			return fmt.Errorf("AddReference: diff chunks: %w", err)
		}

		for _, c := range diff.Inserted {
			if _, err := node.ExecTracked(`
				INSERT INTO reference_chunks
				(id, doc_id, chunk_index, section, content, source_path, content_hash)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(id) DO NOTHING
			`, 0,
				c.ID, c.DocID, c.ChunkIndex,
				c.Section, c.Content, c.SourcePath,
				ComputeChunkHash(c.Content),
			); err != nil {
				return fmt.Errorf("add chunk %q: %w", c.ID, err)
			}
		}
		// Updated chunks share the Inserted write path — same row,
		// different content. ON CONFLICT(id) DO UPDATE rewrites
		// content_hash AND clears the embedding column so
		// EmbedReferenceChunks picks the row up on its next pass.
		// Without the embedding reset, the row would carry a stale
		// vector for new content and the diff-based embedding bypass
		// would silently misroute searches.
		for _, c := range diff.Updated {
			if _, err := node.ExecTracked(`
				INSERT INTO reference_chunks
				(id, doc_id, chunk_index, section, content, source_path, content_hash)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(id) DO UPDATE SET
					content = excluded.content,
					content_hash = excluded.content_hash,
					section = excluded.section,
					chunk_index = excluded.chunk_index,
					embedding = NULL
			`, 0,
				c.ID, c.DocID, c.ChunkIndex,
				c.Section, c.Content, c.SourcePath,
				ComputeChunkHash(c.Content),
			); err != nil {
				return fmt.Errorf("update chunk %q: %w", c.ID, err)
			}
		}
		// Unchanged: skip entirely — the existing row is the truth.
		// Deleted: drop orphans whose content_hash is not in the new set.
		for _, orphanID := range diff.Deleted {
			if _, err := node.ExecTracked(
				`DELETE FROM reference_chunks WHERE id = ?`, 0, orphanID,
			); err != nil {
				return fmt.Errorf("delete orphan chunk %q: %w", orphanID, err)
			}
		}
		return nil
	})
}

// DeleteReference removes a reference, its chunks, and its audit rows
// (reference_interactions, admission_log) in a single transaction. The
// interactions and admission_log tables are NOT children of
// reference_chunks, so ON DELETE CASCADE alone does not cover the
// fan-out — without the transaction, a partial failure could leave
// orphan audit rows pointing at a missing parent doc.
func (dm *DatabaseManager) DeleteReference(id string) error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("DeleteReference: database not initialized")
	}
	return dm.WithTx(func(node DBNode) error {
		if _, err := node.ExecTracked(`DELETE FROM reference_chunks WHERE doc_id = ?`, 0, id); err != nil {
			return fmt.Errorf("delete reference chunks: %w", err)
		}
		if _, err := node.ExecTracked(`DELETE FROM reference_interactions WHERE doc_id = ?`, 0, id); err != nil {
			return fmt.Errorf("delete reference interactions: %w", err)
		}
		if _, err := node.ExecTracked(`DELETE FROM admission_log WHERE doc_id = ?`, 0, id); err != nil {
			return fmt.Errorf("delete admission log: %w", err)
		}
		if _, err := node.ExecTracked(`DELETE FROM reference_docs WHERE id = ?`, 0, id); err != nil {
			return fmt.Errorf("delete reference doc: %w", err)
		}
		return nil
	})
}

// EmbedReferenceChunks fills in NULL embeddings for chunks belonging
// to docID. Idempotent: chunks with non-NULL embeddings are skipped
// (they were embedded by a previous pass or by re-ingest of unchanged
// content). Per-chunk embedding uses EmbedText, which falls back to
// HashEmbed when no provider is configured — so this method always
// populates something, never returns per-chunk failure counts in
// production. Database-level errors (query / UPDATE) are returned as
// err; ctx cancellation is checked between chunks and aborts cleanly.
//
// Embedding is intentionally split from AddReference:
//   - AddReference's tx stays small and fast (chunk rows + diff logic
//     only); a slow embed call would block the ingest tx and bloat
//     the WAL.
//   - Embedding is retryable independently — a transient provider
//     failure does not roll back chunk inserts.
//   - Embedding is parallelizable at the caller level (loop across
//     docIDs, wrap in a worker pool) without rewriting AddReference.
//
// Why this works with the chunk_hash diff: AddReference only clears
// embedding on the Updated branch (content changed). The Unchanged
// branch skips the row entirely, so its existing embedding stays —
// that is the whole point of the diff-based embedding bypass. Re-
// ingesting an unchanged manual re-embeds zero chunks.
func (dm *DatabaseManager) EmbedReferenceChunks(ctx context.Context, docID string) (embedded int, failed int, err error) {
	if dm == nil || dm.db == nil {
		return 0, 0, fmt.Errorf("EmbedReferenceChunks: database not initialized")
	}
	if docID == "" {
		return 0, 0, fmt.Errorf("EmbedReferenceChunks: empty docID")
	}

	rows, err := dm.db.QueryContext(ctx,
		`SELECT id, content FROM reference_chunks WHERE doc_id = ? AND embedding IS NULL`, docID)
	if err != nil {
		return 0, 0, fmt.Errorf("EmbedReferenceChunks: query: %w", err)
	}
	type pending struct {
		id      string
		content string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if scanErr := rows.Scan(&p.id, &p.content); scanErr != nil {
			rows.Close()
			return embedded, failed, fmt.Errorf("EmbedReferenceChunks: scan: %w", scanErr)
		}
		batch = append(batch, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return embedded, failed, fmt.Errorf("EmbedReferenceChunks: rows.Err: %w", err)
	}
	rows.Close()

	for _, p := range batch {
		if ctx.Err() != nil {
			return embedded, failed, ctx.Err()
		}
		vec := EmbedText(p.content)
		bytes, marshalErr := embeddingBytes(vec)
		if marshalErr != nil {
			failed++
			continue
		}
		_, writeErr := dm.db.ExecContext(ctx,
			`UPDATE reference_chunks SET embedding = ? WHERE id = ?`, bytes, p.id)
		if writeErr != nil {
			failed++
			continue
		}
		embedded++
	}
	return embedded, failed, nil
}

// embedReferenceDoc is an internal helper that walks every chunk of
// every doc and embeds the ones with NULL embedding. Used by full-
// corpus backfill operations (e.g., after enabling embeddings for the
// first time on a populated library). Not on the ingest hot path.
func (dm *DatabaseManager) embedReferenceDoc(ctx context.Context, docID string) (int, int, error) {
	return dm.EmbedReferenceChunks(ctx, docID)
}

// referenceChunksNeedingEmbedding returns the chunk ids for a doc
// whose embedding column is NULL. Exposed for callers (CLI/MCP) that
// want to surface "X chunks awaiting embedding" without running the
// embedding themselves.
func referenceChunksNeedingEmbedding(db *sql.DB, docID string) ([]string, error) {
	if db == nil {
		return nil, fmt.Errorf("referenceChunksNeedingEmbedding: database not initialized")
	}
	rows, err := db.Query(
		`SELECT id FROM reference_chunks WHERE doc_id = ? AND embedding IS NULL`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

