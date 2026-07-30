// retrieval_metadata.go — Observability Layer for Adaptive Retrieval.
//
// Records every retrieval of a cognitive node (Memory / Lesson /
// Decision / Theory / Skill) into the retrieval_metadata table. Called
// from MCP handlers and wake_context after a successful read so the
// agent never has to manage these stats explicitly.
//
// Errors are returned for caller inspection but the typical contract
// is fire-and-forget: the user-facing retrieval must not fail because
// telemetry write failed. Callers should log and continue.
//
// CRITICAL: this layer is observability only. It does NOT alter the
// FTS ranking, the cognitive object schemas, or any existing search
// behavior. DefaultRanker in retrieval_ranker.go returns ftsScore
// unchanged, preserving today's exact behavior bit-for-bit.

package internal

import (
	"fmt"
)

// RecordRetrieval increments the reuse_count for the given node and
// updates last_retrieved_at. Upserts via ON CONFLICT so the first
// retrieval of a node creates the row.
//
// Timestamps are INTEGER Unix-epoch seconds (see migration
// timestamps_unified_v1); CURRENT_TIMESTAMP is replaced with
// CAST(strftime('%s','now') AS INTEGER).
func (dm *DatabaseManager) RecordRetrieval(nodeID, nodeType string) error {
	if nodeID == "" {
		return fmt.Errorf("RecordRetrieval: node_id is empty")
	}
	if nodeType == "" {
		return fmt.Errorf("RecordRetrieval: node_type is empty")
	}
	_, err := dm.db.Exec(`
		INSERT INTO retrieval_metadata (node_id, node_type, reuse_count, last_retrieved_at, success_count)
		VALUES (?, ?, 1, CAST(strftime('%s','now') AS INTEGER), 0)
		ON CONFLICT(node_id) DO UPDATE SET
			reuse_count = retrieval_metadata.reuse_count + 1,
			last_retrieved_at = CAST(strftime('%s','now') AS INTEGER),
			updated_at = CAST(strftime('%s','now') AS INTEGER)
	`, nodeID, nodeType)
	if err != nil {
		return fmt.Errorf("RecordRetrieval(%q, %q): %w", nodeID, nodeType, err)
	}
	return nil
}

// RecordRetrievalSuccess bumps success_count for a node. Separate from
// RecordRetrieval because "surfaced" and "useful" are different signals
// — a node can be retrieved 100 times and only succeed (be reinforced,
// promoted, committed as evidence) twice. Callers wire this into the
// success path (challenge resolution, save_lesson-from-mem, etc.) when
// the success signal is unambiguous; default is to leave success_count
// at 0 unless a future call site proves it.
func (dm *DatabaseManager) RecordRetrievalSuccess(nodeID, nodeType string) error {
	if nodeID == "" {
		return fmt.Errorf("RecordRetrievalSuccess: node_id is empty")
	}
	if nodeType == "" {
		return fmt.Errorf("RecordRetrievalSuccess: node_type is empty")
	}
	_, err := dm.db.Exec(`
		INSERT INTO retrieval_metadata (node_id, node_type, reuse_count, last_retrieved_at, success_count)
		VALUES (?, ?, 0, NULL, 1)
		ON CONFLICT(node_id) DO UPDATE SET
			success_count = retrieval_metadata.success_count + 1,
			updated_at = CAST(strftime('%s','now') AS INTEGER)
	`, nodeID, nodeType)
	if err != nil {
		return fmt.Errorf("RecordRetrievalSuccess(%q, %q): %w", nodeID, nodeType, err)
	}
	return nil
}

// IncrementSuccess bumps success_count by 1 for a node via INSERT
// ... ON CONFLICT DO UPDATE. Used by the save_lesson handler to
// credit source_ids when an agent distils a lesson from prior
// retrievals.
//
// The UPSERT form (not pure UPDATE) accommodates citations for
// nodes that haven't been retrieved via the read hooks in this
// turn: an agent may legitimately cite a node by id from prior-
// session memory without having surfaced it via ReadSkill /
// query_long_term_memory / etc. Pure UPDATE would silently skip
// those citations; UPSERT records them as first-time successes.
//
// nodeType is the cognitive-object type for the citation (memory,
// lesson, skill, decision, theory). When the caller doesn't know
// the type, pass "memory" — the schema's NOT NULL constraint
// requires a value, and the alternative is fabricating a sentinel
// like "unknown" that pollutes the telemetry taxonomy.
func (dm *DatabaseManager) IncrementSuccess(nodeID, nodeType string) error {
	if nodeID == "" {
		return fmt.Errorf("IncrementSuccess: node_id is empty")
	}
	if nodeType == "" {
		nodeType = "memory"
	}
	_, err := dm.db.Exec(`
		INSERT INTO retrieval_metadata (node_id, node_type, reuse_count, last_retrieved_at, success_count)
		VALUES (?, ?, 0, NULL, 1)
		ON CONFLICT(node_id) DO UPDATE SET
			success_count = retrieval_metadata.success_count + 1,
			updated_at = CAST(strftime('%s','now') AS INTEGER)
	`, nodeID, nodeType)
	if err != nil {
		return fmt.Errorf("IncrementSuccess(%q, %q): %w", nodeID, nodeType, err)
	}
	return nil
}

// GetRetrievalMetadata fetches the metadata row for a node, returning
// a zero-valued RetrievalMetadata when the row does not exist. Used by
// the explain_retrieval MCP tool to render per-node diagnostics.
func (dm *DatabaseManager) GetRetrievalMetadata(nodeID string) (RetrievalMetadata, error) {
	out := RetrievalMetadata{NodeID: nodeID}
	if nodeID == "" {
		return out, nil
	}
	var (
		nodeType                            string
		reuseCount, successCount            int
		lastRetrievedAt                     *int64
	)
	err := dm.db.QueryRow(`
		SELECT node_type, reuse_count, last_retrieved_at, success_count
		FROM retrieval_metadata
		WHERE node_id = ?
	`, nodeID).Scan(&nodeType, &reuseCount, &lastRetrievedAt, &successCount)
	if err != nil {
		// sql.ErrNoRows -> the zero-value is the right answer.
		// We don't surface "not found" as an error because the
		// explain_retrieval tool's contract is "show what we know",
		// and a never-retrieved node is a legitimate result.
		return out, nil
	}
	out.NodeType = nodeType
	out.ReuseCount = reuseCount
	out.SuccessCount = successCount
	if lastRetrievedAt != nil {
		out.LastRetrievedAt = lastRetrievedAt
	}
	return out, nil
}