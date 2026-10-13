// similarity_candidates_store.go — the ONLY path that persists candidates.
//
// Separation of concerns, stated plainly:
//
//   - similarity_candidates.go DISCOVERS candidates and writes nothing.
//   - this file PERSISTS them, and is reachable only from an explicitly
//     invoked operator command.
//
// Ordinary semantic retrieval calls neither. That is the whole guarantee:
// a read cannot add a row to the persistent queue, so a read cannot grow it
// without bound and a diagnostic command cannot leave a durable trace
// behind.
//
// The queue rows themselves are unchanged in shape. Existing
// shared.contradiction_log records are read, resolved and arbitrated exactly
// as before; nothing here migrates, reinterprets or deletes them. What
// changed is only WHO may add to the queue: not a retrieval, only an
// operator who asked.

package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// similarityCandidateSource is set on every row this file writes, so a
// reviewer can tell at a glance which queue items arrived from an explicit
// operator scan and which predate the split. Existing rows carry whatever
// detected_by they already had and are not touched.
const similarityCandidateSource = "similarity-scan"

// LoadSimilarityScanEntries reads the most recent live memories as scan
// inputs. `limit` caps the working set (0 = no cap); pairs are evaluated
// within that set only, so a smaller limit is a cheaper, shallower scan.
//
// The filters mirror the old Phase 4 batch fetch exactly: a memory with no
// embedding, a literal 'null' vector, or a synthetic hash vector cannot be
// compared meaningfully and is excluded rather than silently scored.
func (dm *DatabaseManager) LoadSimilarityScanEntries(limit int, schemaPrefix string) ([]ScanEntry, error) {
	if dm.db == nil {
		return nil, fmt.Errorf("LoadSimilarityScanEntries: dm.db is nil")
	}
	q := fmt.Sprintf(`
		SELECT id, embedding, content, COALESCE(metadata, '{}')
		FROM %smemories
		WHERE deleted_at IS NULL
		  AND embedding IS NOT NULL
		  AND embedding != 'null'
		  AND embedding_source != 'hash'
		ORDER BY created_at DESC
	`, schemaPrefix)
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := dm.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("LoadSimilarityScanEntries: %w", err)
	}
	defer rows.Close()

	var out []ScanEntry
	for rows.Next() {
		var id, embStr, content, meta string
		if err := rows.Scan(&id, &embStr, &content, &meta); err != nil {
			return nil, fmt.Errorf("LoadSimilarityScanEntries: scan: %w", err)
		}
		var emb []float32
		if embStr == "" || json.Unmarshal([]byte(embStr), &emb) != nil || len(emb) == 0 {
			continue
		}
		out = append(out, ScanEntry{
			ID:           id,
			Content:      content,
			Embedding:    emb,
			IsChallenged: memoryIsChallenged(meta),
			MetadataJSON: meta,
		})
	}
	return out, rows.Err()
}

// memoryIsChallenged reports whether the metadata carries the challenge flag.
// It READS pre-existing state and never sets it.
func memoryIsChallenged(metadataJSON string) bool {
	return strings.Contains(metadataJSON, "challenged")
}

// EnqueueSimilarityCandidates persists candidates to the shared queue.
//
// This is the explicit opt-in write. It is not called by HybridSearch, by
// recall, or by any diagnostic path.
//
// EnqueueContradiction is idempotent on the memory pair, so re-running a
// scan over the same corpus adds nothing the second time. That is what
// keeps a repeated operator scan from growing the queue without bound.
//
// Returns the number of rows ACTUALLY inserted. Candidates already queued
// are not counted, so a repeat run over unchanged data returns 0 rather than
// re-announcing last run's work as new. Callers show this number to an
// operator, and a number that counts writes it did not perform is worse than
// no number at all.
func (dm *DatabaseManager) EnqueueSimilarityCandidates(cands []SimilarityCandidate) (int, error) {
	if len(cands) == 0 {
		return 0, nil
	}
	inserted := 0
	for _, c := range cands {
		ev := ContradictionEvidence{
			MemoryA:    c.MemoryA,
			MemoryB:    c.MemoryB,
			Evidence:   c.Evidence,
			Similarity: c.Similarity,
			DetectedBy: similarityCandidateSource,
		}
		wrote, err := dm.EnqueueContradictionReport(ev)
		if err != nil {
			// Surface the shared-attachment failure rather than swallowing
			// it: a caller that asked for persistence and got silence would
			// reasonably conclude the queue was updated.
			if isSharedUnavailable(err) {
				return inserted, fmt.Errorf(
					"EnqueueSimilarityCandidates: shared database unavailable: %w", err)
			}
			return inserted, err
		}
		if wrote {
			inserted++
		}
	}
	return inserted, nil
}

// isSharedUnavailable reports whether err is the expected "no shared
// database attached" failure rather than a real write error.
func isSharedUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") ||
		strings.Contains(msg, "no such schema") ||
		strings.Contains(msg, "shared")
}

// CountContradictionQueue reports how many unresolved rows the queue holds.
// Used by the explicit review command to show before/after, and by tests
// that assert a retrieval wrote nothing.
func (dm *DatabaseManager) CountContradictionQueue() (int, error) {
	if dm.db == nil {
		return 0, fmt.Errorf("CountContradictionQueue: dm.db is nil")
	}
	var n int
	err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM shared.contradiction_log WHERE resolved_at IS NULL`).Scan(&n)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}
