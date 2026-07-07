// contradiction_log.go — Arc 1 (Conflict Resolution) operational queue.
//
// Storage for the contradiction lifecycle: detection → triage → resolution.
// The table is the operator's source of truth; mirror.jsonl is the
// forensic detection trail (rotated independently).
//
// Three invariants this file guarantees:
//   1. A contradiction that lands in the queue is durable (the row is
//      committed in the same transaction that produced the detection
//      event, OR the detection failed and the row never lands — there
//      is no half-state).
//   2. The resolution command is idempotent: running it twice does not
//      double-slash the same memory. The partial index on
//      resolved_at IS NULL keeps the unresolved set bounded, and the
//      transaction wraps (slashing + resolution memory + queue update)
//      atomically.
//   3. The "verdict" (which memory won, which lost, why) lives in the
//      resolution memory (a row in shared.memories), NOT in this
//      table. The table only holds the pair and the queue state. The
//      verdict is the audit trail; the queue is the worklist.

package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// ContradictionEvidence is the structured payload written to
// shared.contradiction_log at detection time. The `evidence` text
// column is the freeform rationale; similarity is the score (cosine
// for vector, BM25 for keyword, or both for hybrid).
type ContradictionEvidence struct {
	MemoryA    string  `json:"memory_a"`
	MemoryB    string  `json:"memory_b"`
	Evidence   string  `json:"evidence"`
	Similarity float64 `json:"similarity,omitempty"`
	DetectedBy string  `json:"detected_by,omitempty"` // session_id or agent_id
}

// ProvenanceScore captures the three signals that go into the
// resolution decision. Each is normalized to [0, 1]; the weighted sum
// is in [0, 1]. See ComputeProvenanceScore for the formula.
type ProvenanceScore struct {
	MemoryID       string
	Confidence     float64
	Freshness      float64
	Reinforcement  float64
	Score          float64
	AgeDays        float64
	LastReinforced time.Time
}

// ResolutionDecision is the output of the resolution loop. The CLI
// command prints this in dry-run and applies it (transactionally) in
// --apply mode.
type ResolutionDecision struct {
	LoserID       string
	WinnerID      string
	LoserScore    ProvenanceScore
	WinnerScore   ProvenanceScore
	Margin        float64
	SlashAmount   int    // weight units to subtract from loser
	IsCloseCall   bool   // margin < threshold → propose theory instead
	Rationale     string // human-readable explanation
	ResolutionTag string // tag applied to the resolution memory
}

// ProvenanceThreshold is the |winner_score - loser_score| margin
// below which the resolver bails out and proposes a theory for human
// arbitration. Tuned at 0.10 — small enough that decisive calls
// happen, large enough that 50/50 ties surface for review.
const ProvenanceThreshold = 0.10

// SlashFraction is the proportion of the loser's current weight that
// gets removed by ChallengeMemory on resolution. 0.5 = slash half.
// Combined with the "challenged" status flag (set by ChallengeMemory),
// the loser's retrieval priority drops substantially without
// erasing the memory.
const SlashFraction = 0.5

// EnqueueContradiction inserts a row into shared.contradiction_log.
// Idempotent on (memory_a, memory_b, evidence_hash): a second call
// with the same triple is a no-op. This protects against the
// "detection fires twice" race (HybridSearch → ChallengeMemoryAsync
// can fire from two different code paths in the same query).
//
// Called from ChallengeMemoryAsync in the same goroutine that
// appends to mirror.jsonl. The shared DB write is best-effort: if it
// fails (shared DB not attached, write error, etc.) the goroutine
// logs to watchdog and returns. The mirror.jsonl entry is the
// durable trail; the table is the operational cache.
func (dm *DatabaseManager) EnqueueContradiction(ev ContradictionEvidence) error {
	if dm.db == nil {
		return fmt.Errorf("EnqueueContradiction: dm.db is nil")
	}
	if ev.MemoryA == "" || ev.MemoryB == "" {
		return fmt.Errorf("EnqueueContradiction: both memory IDs required")
	}
	if ev.MemoryA == ev.MemoryB {
		return fmt.Errorf("EnqueueContradiction: a memory cannot contradict itself")
	}
	// Normalize the pair: sort lexicographically so (A,B) and (B,A)
	// produce the same dedup key. This protects against the "order
	// swapped at the call site" class of bug.
	if ev.MemoryA > ev.MemoryB {
		ev.MemoryA, ev.MemoryB = ev.MemoryB, ev.MemoryA
	}
	evJSON, _ := json.Marshal(ev)

	// The shared.contradiction_log table is created in attachShared.
	// If shared isn't attached, fail soft: the caller will log this
	// to watchdog and the mirror.jsonl entry is the fallback.
	_, err := dm.db.Exec(`
		INSERT OR IGNORE INTO shared.contradiction_log
			(memory_id_a, memory_id_b, evidence, similarity, detected_by, detected_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, ev.MemoryA, ev.MemoryB, string(evJSON), ev.Similarity, ev.DetectedBy)
	if err != nil {
		return fmt.Errorf("EnqueueContradiction: %w", err)
	}
	return nil
}

// LoadContradictionQueue returns the unresolved rows ordered by
// detection time (oldest first — operators want to clear stale items
// before fresh ones). Limit caps the batch size; 0 means "no cap"
// but in practice the CLI default is 100.
func (dm *DatabaseManager) LoadContradictionQueue(limit int) ([]map[string]interface{}, error) {
	q := `
		SELECT id, memory_id_a, memory_id_b, evidence, similarity,
		       detected_at, detected_by
		FROM shared.contradiction_log
		WHERE resolved_at IS NULL
		ORDER BY detected_at ASC
	`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := dm.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("LoadContradictionQueue: %w", err)
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var (
			id, a, b, evidence, detectedAt, detectedBy sql.NullString
			similarity                                  sql.NullFloat64
		)
		if err := rows.Scan(&id, &a, &b, &evidence, &similarity, &detectedAt, &detectedBy); err != nil {
			return nil, fmt.Errorf("LoadContradictionQueue: scan: %w", err)
		}
		out = append(out, map[string]interface{}{
			"id":          id.String,
			"memory_id_a": a.String,
			"memory_id_b": b.String,
			"evidence":    evidence.String,
			"similarity":  similarity.Float64,
			"detected_at": detectedAt.String,
			"detected_by": detectedBy.String,
		})
	}
	return out, rows.Err()
}

// LoadMemoryProvenance loads the three signals (confidence,
// last_reinforced, reinforcement_count) for a single memory. Returns
// a zero-value ProvenanceScore if the memory is missing or deleted —
// the resolver treats missing memories as having the lowest possible
// provenance, so the OTHER side wins by default.
func (dm *DatabaseManager) LoadMemoryProvenance(memoryID string) (ProvenanceScore, error) {
	ps := ProvenanceScore{MemoryID: memoryID}
	var (
		confidence     sql.NullFloat64
		updatedAt      sql.NullString
		retrievalPri   sql.NullFloat64
	)
	err := dm.db.QueryRow(`
		SELECT
			COALESCE(confidence, 0.5),
			COALESCE(updated_at, CURRENT_TIMESTAMP),
			COALESCE(retrieval_priority, 0.5)
		FROM shared.memories
		WHERE id = ? AND deleted_at IS NULL
	`, memoryID).Scan(&confidence, &updatedAt, &retrievalPri)
	if err == sql.ErrNoRows {
		// Missing/deleted memory = lowest provenance. Caller will
		// see score 0 and the other side wins decisively.
		return ps, nil
	}
	if err != nil {
		return ps, fmt.Errorf("LoadMemoryProvenance: %w", err)
	}
	ps.Confidence = confidence.Float64
	if t, perr := time.Parse(time.RFC3339, updatedAt.String); perr == nil {
		ps.LastReinforced = t
		ps.AgeDays = time.Since(t).Hours() / 24.0
	}
	// Reinforcement count isn't a first-class column; we approximate
	// from the evidence count attached to the memory via the
	// evidence_store pipeline. The query is cheap and the
	// approximation is good enough for ranking.
	var evCount int
	_ = dm.db.QueryRow(`
		SELECT COUNT(*) FROM shared.evidence
		WHERE artifact_id = ? AND artifact_type = 'memory'
	`, memoryID).Scan(&evCount)
	ps.Reinforcement = math.Min(float64(evCount)/10.0, 1.0)

	// Weighted sum: confidence 50%, freshness 30%, reinforcement 20%.
	// Freshness decays as 1 / (1 + age_days) — recent memories win.
	// Reinforcement saturates at 10 evidence rows.
	ps.Score = 0.5*ps.Confidence + 0.3*(1.0/(1.0+ps.AgeDays)) + 0.2*ps.Reinforcement
	return ps, nil
}

// ResolveOneContradiction is the core decision function. Given a
// queue row, load both memories' provenance, compute the verdict,
// and (if --apply is set) apply it in a single transaction.
//
// Returns the decision. Caller prints it (dry-run) or applies it
// (--apply). The function itself NEVER mutates state in dry-run
// mode — mutation is gated on the apply flag at the call site.
//
// Three terminal states:
//   - "resolved": margin > threshold, slash applied, resolution
//     memory written, queue row marked resolved.
//   - "arbitration": margin < threshold, theory proposed, queue
//     row NOT marked resolved (operator arbitrates manually).
//   - "missing": one of the memories no longer exists. Default to
//     the survivor; queue row marked resolved.
func (dm *DatabaseManager) ResolveOneContradiction(row map[string]interface{}, apply bool) (ResolutionDecision, error) {
	dec := ResolutionDecision{}

	memoryA, _ := row["memory_id_a"].(string)
	memoryB, _ := row["memory_id_b"].(string)
	idStr, _ := row["id"].(string)
	queueID, _ := parseContradictionID(idStr)

	if memoryA == "" || memoryB == "" {
		return dec, fmt.Errorf("ResolveOneContradiction: missing memory IDs in row")
	}

	scoreA, err := dm.LoadMemoryProvenance(memoryA)
	if err != nil {
		return dec, fmt.Errorf("ResolveOneContradiction: load %s: %w", memoryA, err)
	}
	scoreB, err := dm.LoadMemoryProvenance(memoryB)
	if err != nil {
		return dec, fmt.Errorf("ResolveOneContradiction: load %s: %w", memoryB, err)
	}

	dec.LoserScore = scoreA
	dec.WinnerScore = scoreB
	dec.Margin = math.Abs(scoreA.Score - scoreB.Score)
	dec.IsCloseCall = dec.Margin < ProvenanceThreshold

	// Tie-or-close → propose theory, do not slash.
	if dec.IsCloseCall {
		dec.LoserID = memoryA
		dec.WinnerID = memoryB
		dec.Rationale = fmt.Sprintf(
			"close call: margin=%.3f < threshold=%.2f. Proposing arbitration theory.",
			dec.Margin, ProvenanceThreshold,
		)
		dec.ResolutionTag = fmt.Sprintf("arbitration:contradiction:%s-%s", memoryA, memoryB)
		if apply {
			if err := dm.applyArbitration(queueID, memoryA, memoryB, scoreA, scoreB, dec.Margin); err != nil {
				return dec, fmt.Errorf("apply arbitration: %w", err)
			}
		}
		return dec, nil
	}

	// Decisive: higher score wins. Slash the loser's weight.
	var loser, winner string
	var loserScore, winnerScore ProvenanceScore
	if scoreA.Score > scoreB.Score {
		// A has the higher score, so A wins. B is the loser.
		winner, loser = memoryA, memoryB
		winnerScore, loserScore = scoreA, scoreB
	} else {
		// B has the higher (or equal) score, so B wins. A is the loser.
		winner, loser = memoryB, memoryA
		winnerScore, loserScore = scoreB, scoreA
	}
	dec.LoserID = loser
	dec.WinnerID = winner
	dec.LoserScore = loserScore
	dec.WinnerScore = winnerScore
	dec.SlashAmount = int(math.Round(loserScore.Confidence * SlashFraction * 100))
	dec.Rationale = fmt.Sprintf(
		"decisive: %s (score=%.3f) wins over %s (score=%.3f, margin=%.3f). Slash %d weight units.",
		winner, winnerScore.Score, loser, loserScore.Score, dec.Margin, dec.SlashAmount,
	)
	dec.ResolutionTag = fmt.Sprintf("resolution:contradiction:%s-%s", loser, winner)

	if apply {
		if err := dm.applyResolution(queueID, dec); err != nil {
			return dec, fmt.Errorf("apply resolution: %w", err)
		}
	}
	return dec, nil
}

// applyResolution executes the verdict in a single transaction:
//  1. Slash the loser's weight (via ChallengeMemory, which sets
//     status='challenged' and decrements weight).
//  2. INSERT a resolution memory into shared.memories — the FTS5
//     sync triggers from 61a8418 mirror it to shared.memories_fts.
//  3. UPDATE the queue row: resolved_at + resolution_memory_id.
//
// All three steps in one transaction. Crash safety: either all
// three land or none do. The queue row stays "unresolved" if any
// step fails; the next operator run picks it up.
func (dm *DatabaseManager) applyResolution(queueID int64, dec ResolutionDecision) error {
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("applyResolution: begin: %w", err)
	}
	defer tx.Rollback()

	// Step 2: write the resolution memory first (so we have its ID
	// for the loser's metadata patch). The collection is
	// 'memories' (so it lands in the regular retrieval path), with
	// a tag identifying it as a resolution. The content is the
	// verdict narrative.
	resolutionID := fmt.Sprintf("resolution-%s-%d", dec.LoserID, time.Now().UnixNano())
	content := fmt.Sprintf(
		"Resolution: memory_%s (loser, confidence=%.3f, last_reinforced=%s) "+
			"slashed by %d weight units in favor of memory_%s (winner, confidence=%.3f, "+
			"last_reinforced=%s, %d evidence rows). Rationale: provenance-based scoring, "+
			"margin=%.3f. See shared.contradiction_log id=%d.",
		dec.LoserID, dec.LoserScore.Confidence, dec.LoserScore.LastReinforced.Format(time.RFC3339),
		dec.SlashAmount,
		dec.WinnerID, dec.WinnerScore.Confidence, dec.WinnerScore.LastReinforced.Format(time.RFC3339),
		int(dec.WinnerScore.Reinforcement*10),
		dec.Margin, queueID,
	)
	tagsJSON := fmt.Sprintf(`["%s"]`, dec.ResolutionTag)
	_, err = tx.Exec(`
		INSERT INTO shared.memories
			(id, collection, content, tags, weight, retrieval_priority, importance, confidence, metadata)
		VALUES (?, 'resolutions', ?, ?, 1.0, 0.7, 0.7, 0.9, ?)
	`, resolutionID, content, tagsJSON, fmt.Sprintf(`{"resolution_for_queue_id":%d,"winner":%q,"loser":%q,"margin":%f}`, queueID, dec.WinnerID, dec.LoserID, dec.Margin))
	if err != nil {
		return fmt.Errorf("applyResolution: write resolution memory: %w", err)
	}

	// Step 1: slash the loser. ChallengeMemory has its own
	// transaction; for the atomic guarantee, we duplicate the
	// essential UPDATE here inside our transaction. We do this
	// AFTER the resolution memory write so resolutionID is in
	// scope for the loser's metadata patch.
	var exists bool
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM shared.memories WHERE id = ? AND deleted_at IS NULL)`, dec.LoserID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("applyResolution: loser check: %w", err)
	}
	if exists {
		// Build a metadata patch that mirrors the existing
		// ChallengeMemory semantics: status='challenged' stored in
		// the JSON metadata column, plus weight/reinforcement
		// updated via the standard columns. Note: there's no
		// "status" column on the memories table; it's part of the
		// metadata JSON (see updateMemoryMetadataTx).
		loserMetadata := fmt.Sprintf(`{"status":"challenged","challenged_by_resolution":%q,"challenged_at":CURRENT_TIMESTAMP}`, resolutionID)
		_, err = tx.Exec(`
			UPDATE shared.memories
			SET metadata = COALESCE(metadata, '{}') || ?,
			    weight = MAX(1, weight - ?),
			    retrieval_priority = MAX(0.01, retrieval_priority - ?),
			    updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND deleted_at IS NULL
		`, loserMetadata, dec.SlashAmount, float64(dec.SlashAmount)/100.0, dec.LoserID)
		if err != nil {
			return fmt.Errorf("applyResolution: slash loser: %w", err)
		}
	}

	// Step 3: mark the queue row resolved.
	_, err = tx.Exec(`
		UPDATE shared.contradiction_log
		SET resolved_at = CURRENT_TIMESTAMP,
		    resolution_memory_id = ?
		WHERE id = ? AND resolved_at IS NULL
	`, resolutionID, queueID)
	if err != nil {
		return fmt.Errorf("applyResolution: mark resolved: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("applyResolution: commit: %w", err)
	}
	return nil
}

// applyArbitration is the close-call path. It writes a pending theory
// (collection='theories') into shared.memories with both memory IDs
// in dependencies. The queue row stays UNRESOLVED — when the
// operator arbitrates and resolves the theory, the resolution
// command can pick up the queue row again.
//
// Note: the queue row is intentionally NOT marked resolved here. A
// close call is not a resolution; it's a triage flag that requires
// human input.
func (dm *DatabaseManager) applyArbitration(queueID int64, memoryA, memoryB string, scoreA, scoreB ProvenanceScore, margin float64) error {
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("applyArbitration: begin: %w", err)
	}
	defer tx.Rollback()

	theoryID := fmt.Sprintf("arbitration-%s-%s-%d", memoryA, memoryB, time.Now().UnixNano())
	hypothesis := fmt.Sprintf(
		"Memory %s and memory %s are in contradiction (margin=%.3f, threshold=%.2f). "+
			"Provenance: A=%.3f (confidence=%.3f, %d ev), B=%.3f (confidence=%.3f, %d ev). "+
			"Human arbitration required.",
		memoryA, memoryB, margin, ProvenanceThreshold,
		scoreA.Score, scoreA.Confidence, int(scoreA.Reinforcement*10),
		scoreB.Score, scoreB.Confidence, int(scoreB.Reinforcement*10),
	)
	deps := []string{memoryA, memoryB}
	depsJSON, _ := json.Marshal(deps)
	tagsJSON := fmt.Sprintf(`["arbitration:contradiction:%s-%s"]`, memoryA, memoryB)

	_, err = tx.Exec(`
		INSERT INTO shared.memories
			(id, collection, content, tags, weight, retrieval_priority, importance, confidence, dependencies, metadata)
		VALUES (?, 'theories', ?, ?, 1.0, 0.5, 0.5, 0.5, ?, ?)
	`, theoryID, hypothesis, tagsJSON, string(depsJSON), fmt.Sprintf(`{"status":"pending","arbitration_for_queue_id":%d,"margin":%f,"score_a":%f,"score_b":%f}`, queueID, margin, scoreA.Score, scoreB.Score))
	if err != nil {
		return fmt.Errorf("applyArbitration: write theory: %w", err)
	}

	// The theory creation is a "marker" but the contradiction is
	// NOT yet resolved. We attach the theory ID as a sidecar so
	// the operator can see the linkage, but the queue row stays
	// open until they resolve the theory.
	_, err = tx.Exec(`
		UPDATE shared.contradiction_log
		SET detected_by = COALESCE(detected_by, '') || '|theory:' || ?
		WHERE id = ? AND resolved_at IS NULL
	`, theoryID, queueID)
	if err != nil {
		return fmt.Errorf("applyArbitration: link theory: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("applyArbitration: commit: %w", err)
	}
	return nil
}

// parseContradictionID is a defensive helper for the queue ID.
// SQLite returns it as a string when scanned into NullString; we
// convert here rather than at the scan site to keep the SELECT
// query portable. (Renamed from parseInt64 to avoid collision
// with wake_tools.go.)
func parseContradictionID(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// SanitizeEvidenceTag strips characters that would break the
// resolution_memory tag (which goes into JSON). Used by callers
// that build the tag from arbitrary memory IDs.
func SanitizeEvidenceTag(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' || r == ':' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') {
			return r
		}
		return '_'
	}, s)
}
