package internal

import (
	"fmt"
)

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
		// content_hash so future diffs see the new content.
		for _, c := range diff.Updated {
			if _, err := node.ExecTracked(`
				INSERT INTO reference_chunks
				(id, doc_id, chunk_index, section, content, source_path, content_hash)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(id) DO UPDATE SET
					content = excluded.content,
					content_hash = excluded.content_hash,
					section = excluded.section,
					chunk_index = excluded.chunk_index
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
