package internal

// Admission database queries: candidate discovery + outcome recording.
//
// These two methods were previously defined in internal/web_db.go (deleted
// during the plugin → MCP migration). They are distinct operational queries
// that belong with the admission flow, not the web interface, so they live
// in this dedicated file rather than being merged into db.go (which is
// already 1,100+ lines and getting unwieldy).

import (
	"encoding/json"
	"time"
)

// FindAdmissionCandidates returns up to `limit` reference chunks that have
// been retrieved frequently enough to warrant memory admission. The
// triggering thresholds (3+ hits across 2+ distinct queries) match the
// admission decision 9aa0ee2c6de8492a.
//
// Candidates whose chunk content already exists in a recent memory revision
// (within 7 days) are excluded — those are already admitted, no need to
// re-admit. The 7-day window matches the decay cycle of recently-admitted
// memories so the same chunk doesn't reappear immediately after admission.
func (dm *DatabaseManager) FindAdmissionCandidates(limit int) ([]*AdmissionCandidate, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := dm.db.Query(`
		SELECT
			i.chunk_id,
			r.id,
			r.title,
			r.import_reason,
			rc.content,
			count(i.id) as hits,
			count(DISTINCT i.query) as distinct_queries
		FROM reference_interactions i
		JOIN reference_docs r ON r.id = i.doc_id
		JOIN reference_chunks rc ON rc.id = i.chunk_id
		WHERE i.chunk_id IS NOT NULL
		  AND NOT EXISTS (
		    SELECT 1 FROM memory_revisions mr
		    JOIN memories m ON m.id = mr.memory_id
		    WHERE mr.content LIKE '%' || substr(rc.content, 1, 80) || '%'
		      AND mr.created_at > datetime('now', '-7 days')
		  )
		GROUP BY i.chunk_id
		HAVING count(i.id) >= 3 AND count(DISTINCT i.query) >= 2
		ORDER BY count(i.id) DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AdmissionCandidate{}
	for rows.Next() {
		var chunkID, docID, title, importReason, content string
		var hits, distinctQueries int
		if err := rows.Scan(&chunkID, &docID, &title, &importReason, &content, &hits, &distinctQueries); err != nil {
			continue
		}
		candidate := &AdmissionCandidate{
			DocID:           docID,
			DocTitle:        title,
			ChunkID:         chunkID,
			ChunkContent:    content,
			ImportReason:    importReason,
			HitCount:        hits,
			DistinctQueries: distinctQueries,
		}
		// Pull recent queries that surfaced this chunk.
		qRows, err := dm.db.Query(`
			SELECT DISTINCT query FROM reference_interactions
			WHERE chunk_id = ?
			ORDER BY created_at DESC
			LIMIT 10
		`, chunkID)
		if err == nil {
			for qRows.Next() {
				var q string
				if err := qRows.Scan(&q); err == nil {
					candidate.RecentQueries = append(candidate.RecentQueries, q)
				}
			}
			qRows.Close()
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

// RecordAdmissionOutcome writes the result of an admission evaluation to
// the audit trail. On admit, the caller is expected to have already
// written the memory; this records the chain. On reject, this is the
// only record that the candidate was evaluated.
//
// Stored in a sibling table `admission_log` so it does not pollute
// reference_interactions (which is the retrieval audit, not the
// admission audit). The two have different semantics and different
// consumers.
func (dm *DatabaseManager) RecordAdmissionOutcome(candidate *AdmissionCandidate, result *admitResult, admissionModel string) error {
	chainJSON, _ := json.Marshal(result.Justification)
	_, err := dm.db.Exec(`
		INSERT INTO admission_log (
			id, doc_id, chunk_id, admit, content, confidence, reason,
			justification, admission_model, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		GenerateID(),
		candidate.DocID,
		candidate.ChunkID,
		result.Admit,
		result.Content,
		result.Confidence,
		result.Reason,
		string(chainJSON),
		admissionModel,
		time.Now().UTC().Format(time.RFC3339),
	)
	return err
}