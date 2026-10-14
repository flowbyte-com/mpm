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
//  4. (added 2026-10-13, amended 2026-10-14) A row is a REVIEW ITEM, not
//     a finding. The rows this file receives from EnqueueSimilarityCandidates
//     are UNVERIFIED similarity candidates: two memories that scored high
//     on cosine, which shows they are related and not that their claims
//     conflict. Near-identical duplicates score at the top of that range, so
//     most candidates are duplicates.
//
//     The amendment (issue #10): this comment used to say an operator
//     upgrades a row "via --apply", which made the apply flag its own
//     justification — a batch --apply over 100 candidates adjudicated all
//     100 at once, which is the defect, not a remedy. Adjudication is now
//     its own explicit, per-row act (AdjudicateContradiction, in
//     contradiction_verification.go) that records the operator's reason on
//     detected_by. ResolveOneContradiction refuses to penalise any row
//     lacking that marker, whatever it was detected by.

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
	// Adjudicated reports whether an operator explicitly judged this
	// pair to be a genuine contradiction. Only an adjudicated row may
	// penalise a memory; see contradiction_verification.go. False for
	// unverified similarity candidates, for rows merely routed to
	// arbitration, and for any detected_by value this codebase does not
	// recognise — the check is affirmative, never inferred.
	Adjudicated bool
	// VerificationNote explains, for an unadjudicated row, why it is not
	// eligible to apply and what an operator can do about it. Empty when
	// Adjudicated is true.
	VerificationNote string
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
// with the same triple is a no-op.
//
// WHO CALLS THIS, as of 2026-10-13: no production path. The one caller that
// used to — HybridSearch's Phase 4 — was removed, because a retrieval must
// not persist a contradiction claim it cannot substantiate. The only
// production insert is EnqueueSimilarityCandidates (similarity_candidates_store.go),
// reached solely from `mpm ops resolve-contradictions --propose`. This
// wrapper is retained because it is part of the DatabaseManager surface and
// is exercised by the queue's own tests.
//
// The doc comment here used to say this was called from ChallengeMemoryAsync.
// That was wrong even when written: ChallengeMemoryAsync appends to
// mirror.jsonl and never touched this table. Correcting it here rather than
// leaving a caller that does not exist.
//
// The shared DB write is best-effort by contract: if it fails (shared DB not
// attached, write error, etc.) the caller decides whether that is fatal.
// EnqueueSimilarityCandidates treats an unattached shared DB as fatal,
// because a caller that asked to persist and got silence would conclude the
// queue was updated.
//
// Callers that need to know whether a row was actually added should use
// EnqueueContradictionReport; this wrapper discards the answer because its
// remaining callers have no use for it.
func (dm *DatabaseManager) EnqueueContradiction(ev ContradictionEvidence) error {
	_, err := dm.EnqueueContradictionReport(ev)
	return err
}

// EnqueueContradictionReport is EnqueueContradiction plus the one fact it
// cannot otherwise convey: whether the INSERT OR IGNORE actually inserted.
//
// The distinction is not cosmetic. A caller that reports "N candidates
// proposed" while re-proposing rows it did not write is reporting work it
// did not do, and an operator reading "78 candidates proposed" on every run
// of a scan over unchanged data learns nothing. With `inserted`, a repeat run
// reports 0, which is the truth.
//
// Returns (false, nil) when the pair is already queued. Errors are returned
// exactly as EnqueueContradiction returns them, so existing fail-soft
// behaviour is unchanged.
//
// # Why the explicit pre-check, when INSERT OR IGNORE already exists
//
// The table's uniqueness constraint is
//
//	UNIQUE(memory_id_a, memory_id_b, detected_at)
//
// and detected_at is CURRENT_TIMESTAMP, whose resolution is one second. So
// OR IGNORE suppresses a write only when the repeat lands inside the same
// second. An operator running a scan twice — two seconds apart, which is
// faster than typing the command — gets a second row for the same pair, and
// RowsAffected reports it as a write.
//
// Measured, before this check: two enqueues of an identical pair 1.1s apart
// both returned wrote=true and left 2 rows. That made "N candidates
// proposed" grow by N on every run of a scan over unchanged data, which is
// the opposite of what the count is for.
//
// The check is on the unresolved pair, matching the queue's own semantics
// (see the partial index on resolved_at IS NULL): a pair is either awaiting
// review or it is not. Once resolved, the pair may legitimately be queued
// again.
//
// This is a SELECT before an INSERT, so two concurrent enqueues of the same
// fresh pair can both insert. That is accepted deliberately: the queue is a
// review worklist, a duplicate there costs an operator one glance, and the
// alternative — a new UNIQUE index on the live shared schema — is a larger
// change than the defect warrants. The callers that matter are sequential
// operator invocations.
func (dm *DatabaseManager) EnqueueContradictionReport(ev ContradictionEvidence) (bool, error) {
	if dm.db == nil {
		return false, fmt.Errorf("EnqueueContradiction: dm.db is nil")
	}
	if ev.MemoryA == "" || ev.MemoryB == "" {
		return false, fmt.Errorf("EnqueueContradiction: both memory IDs required")
	}
	if ev.MemoryA == ev.MemoryB {
		return false, fmt.Errorf("EnqueueContradiction: a memory cannot contradict itself")
	}
	// Normalize the pair: sort lexicographically so (A,B) and (B,A)
	// produce the same dedup key. This protects against the "order
	// swapped at the call site" class of bug.
	if ev.MemoryA > ev.MemoryB {
		ev.MemoryA, ev.MemoryB = ev.MemoryB, ev.MemoryA
	}
	// An `adjudicated:` segment is a record of an operator having read both
	// memories. Creating a queue row is not that action, so an adjudication
	// segment supplied by a producer is unauthorised no matter what the
	// producer claims: verification is affirmative and never inferred from
	// text supplied by the party asking for the benefit of the doubt.
	// Strip it before it reaches either the column or the evidence JSON that
	// mirrors this struct, so AdjudicateContradiction remains the only way a
	// row becomes eligible to penalise a memory. See
	// stripForgedAdjudication for why this is enforced here rather than
	// trusted to the call graph.
	if cleaned, forged := stripForgedAdjudication(ev.DetectedBy); forged {
		ev.DetectedBy = cleaned
	}

	evJSON, _ := json.Marshal(ev)

	// The shared.contradiction_log table is created in attachShared.
	// If shared isn't attached, fail soft: the caller will log this
	// to watchdog and the mirror.jsonl entry is the fallback.
	var pending int
	if err := dm.db.QueryRow(`
		SELECT COUNT(*) FROM shared.contradiction_log
		WHERE memory_id_a = ? AND memory_id_b = ? AND resolved_at IS NULL
	`, ev.MemoryA, ev.MemoryB).Scan(&pending); err != nil {
		return false, fmt.Errorf("EnqueueContradiction: %w", err)
	}
	if pending > 0 {
		return false, nil
	}

	res, err := dm.db.Exec(`
		INSERT OR IGNORE INTO shared.contradiction_log
			(memory_id_a, memory_id_b, evidence, similarity, detected_by, detected_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, ev.MemoryA, ev.MemoryB, string(evJSON), ev.Similarity, ev.DetectedBy)
	if err != nil {
		return false, fmt.Errorf("EnqueueContradiction: %w", err)
	}
	// INSERT OR IGNORE reports 0 affected rows when the uniqueness constraint
	// suppressed the write. With the pre-check above this is a same-second
	// race, not the normal path. A RowsAffected error is not actionable here:
	// we would be reporting the write as unconfirmed while having no way to
	// distinguish success, so treat it as "not inserted" and let the caller's
	// own verification catch a real problem.
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil
	}
	return n > 0, nil
}

// LoadContradictionQueue returns the unresolved rows ordered by
// detection time (oldest first — operators want to clear stale items
// before fresh ones). Limit caps the batch size; 0 means "no cap"
// but in practice the CLI default is 100.
//
// Every row carries a "detected_by" key. It is the only thing that
// distinguishes an unverified similarity candidate from a row an operator
// adjudicated, so a caller that is about to ACT on a row — rather than
// merely list it — must pass it to ClassifyContradictionVerification
// first. ResolveOneContradiction does exactly that and refuses to apply
// an unadjudicated row; the check lives there so that every present and
// future caller inherits it rather than having to remember it.
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
			similarity                                 sql.NullFloat64
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
