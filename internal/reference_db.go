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

// AddReference inserts a reference document and all its chunks in a
// single transaction. Returns ErrAlreadyExists if a document with the
// same id is already present — callers wanting update semantics must
// delete and re-insert. Plain INSERT replaces the previous INSERT OR
// REPLACE default, which silently overwrote metadata and could orphan
// chunks.
//
// Writes into file_path (the schema column name) from doc.SourcePath
// (the struct field name). Both names are kept; do not rename without a
// migration — legacy code reads SourcePath from the struct.
//
// Chunk inserts use ON CONFLICT(id) DO NOTHING so a partial-pipeline
// retry against an already-populated chunk table doesn't duplicate
// rows or fail on the primary-key constraint. With the unified write
// surface, a retry that includes a duplicate *doc* id still fails the
// whole tx before any chunk is written — the chunk-level guard handles
// the rare case where the doc insert succeeds but the tx is replayed
// (e.g., a connection-pool retry).
func (dm *DatabaseManager) AddReference(doc *ReferenceDoc, chunks []ReferenceChunk) error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("AddReference: database not initialized")
	}
	if doc == nil {
		return fmt.Errorf("AddReference: nil reference doc")
	}
	return dm.WithTx(func(node DBNode) error {
		tagsJSON, _ := MarshalJSON(doc.Tags)
		if _, err := node.ExecTracked(`
			INSERT INTO reference_docs
			(id, title, file_path, source_type, tags, content, content_hash,
			 import_reason, total_chunks, last_indexed, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, 0,
			doc.ID, doc.Title, doc.SourcePath, doc.SourceType, tagsJSON,
			doc.Content, doc.ContentHash, doc.ImportReason,
			doc.TotalChunks, doc.LastIndexed, doc.Created,
		); err != nil {
			if isUniqueConstraintError(err) {
				return fmt.Errorf("add reference %q: %w", doc.ID, ErrAlreadyExists)
			}
			return fmt.Errorf("add reference %q: %w", doc.ID, err)
		}

		for _, chunk := range chunks {
			if _, err := node.ExecTracked(`
				INSERT INTO reference_chunks
				(id, doc_id, chunk_index, section, content, source_path)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT(id) DO NOTHING
			`, 0,
				chunk.ID, chunk.DocID, chunk.ChunkIndex,
				chunk.Section, chunk.Content, chunk.SourcePath,
			); err != nil {
				return fmt.Errorf("add chunk %q: %w", chunk.ID, err)
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
