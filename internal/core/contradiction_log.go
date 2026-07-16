// contradiction_log.go — Arc 1 (Conflict Resolution) operational queue.
//
// Storage for the contradiction lifecycle: detection → triage → resolution.
// The table is the operator's source of truth; mirror.jsonl is the
// forensic detection trail (rotated independently).
//
// Split out of the original monolith (F-009). This file owns ONLY the
// queue itself — types, constants, and the two CRUD operations that
// touch the table directly:
//
//   - EnqueueContradiction  insert (idempotent on memory pair)
//   - LoadContradictionQueue operator-facing read for triage
//
// The decision logic lives in resolve.go; the operator-driven
// arbitration closure lives in arbitration.go. The three files
// together implement the Arc 1 resolution loop (detect → triage →
// resolve → remember).
//
// Three invariants this file guarantees:
//  1. A contradiction that lands in the queue is durable (the row is
//     committed in the same transaction that produced the detection
//     event, OR the detection failed and the row never lands — there
//     is no half-state).
//  2. The resolution command is idempotent: running it twice does not
//     double-slash the same memory. The partial index on
//     resolved_at IS NULL keeps the unresolved set bounded, and the
//     transaction wraps (slashing + resolution memory + queue update)
//     atomically.
//  3. The "verdict" (which memory won, which lost, why) lives in the
//     resolution memory (a row in shared.memories), NOT in this
//     table. The table only holds the pair and the queue state. The
//     verdict is the audit trail; the queue is the worklist.

package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
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
	// ResolutionMemoryID is the id of the resolution memory row
	// written into shared.memories by applyResolution. Populated
	// after the apply call returns. Used by Arc 2's auto-broadcast
	// to fan out the resolution event.
	ResolutionMemoryID string
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