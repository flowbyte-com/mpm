// confidence_maintenance.go — bounded, restartable confidence decay.
//
// # Why this exists
//
// computeConfidence models time decay (λ · days since the last positive
// evidence), but the stored `memories.confidence` column only changes
// when something calls RecomputeConfidence. Before this file, nothing
// did so on a schedule: an artifact with no new evidence kept its
// snapshot value forever, so the column disagreed with what the formula
// (and ExplainConfidence) would derive. Issue #92.
//
// # The contract
//
//   - Only artifacts whose last confidence check is older than MinAge
//     participate. A freshly written artifact is not "decayed" the moment
//     it is saved; its evidence path already set the value.
//   - Work per invocation is bounded by Limit. `Due` and `Deferred` are
//     reported so an operator can see the backlog instead of a partial
//     pass silently looking complete.
//   - Each artifact is recomputed and its watermark advanced in ONE
//     transaction. A failure rolls that artifact back, leaves the
//     watermark untouched (so the next run retries it), and does not
//     prevent the rest of the batch from making progress.
//   - Unchanged results do not append confidence_history rows. The
//     watermark is what stops the pass re-selecting them, not a ledger
//     entry that records nothing.
//   - The recompute goes through the same authoritative path as an
//     evidence event (recomputeConfidenceAt), inside a transaction, so
//     cascade crossings keep their existing semantics: a decay that
//     crosses HardConfidenceInvalidationThreshold enqueues the same
//     confidence_floor intent an evidence-driven recompute would.
//   - Concurrency: DatabaseManager runs in WAL mode. A per-artifact
//     deferred transaction that reads evidence and then writes takes a
//     read snapshot; if an ordinary evidence write commits in between,
//     the write upgrade fails with SQLITE_BUSY (snapshot) instead of
//     overwriting the newer evidence-derived value. That artifact is
//     reported as failed and retried on the next pass.
//
// # Scope
//
// Participants are the memories-backed artifact kinds: memory, theory
// and decision (collection "memories" / "theories" / "decisions", the
// default collection, and legacy rows with no collection). Directives
// (standing policy), retired rows and soft-deleted rows are excluded.
// Lessons (lessons_base) are deliberately NOT part of this pass: their
// confidence column is not consulted by any gate, and adding a second
// watermark table would double the surface of a first, bounded step.
// Extending the pass to lessons is a follow-up, not a silent omission —
// the CLI and scheduler report exactly which kinds they cover.
package internal

import (
	"fmt"
	"math"
	"time"
)

const (
	// DefaultConfidenceDecayLimit bounds the artifacts recomputed by one
	// invocation.
	DefaultConfidenceDecayLimit = 100
	// MaxConfidenceDecayLimit caps a caller-supplied limit. A single pass
	// is a maintenance step, not a full-store rewrite.
	MaxConfidenceDecayLimit = 1000
	// DefaultConfidenceDecayMinAge keeps brand-new artifacts out of the
	// pass. Evidence may legitimately arrive minutes after the save.
	DefaultConfidenceDecayMinAge = 24 * time.Hour
	// ConfidenceDecayLastRunKey is the system_config key the scheduler
	// handler uses to enforce its cadence.
	ConfidenceDecayLastRunKey = "confidence_decay_last_run"
	// ConfidenceDecayHistoryTrigger is the confidence_history trigger
	// value written by this pass. It is already part of the schema CHECK
	// vocabulary (migration_confidence_history_check_widening.go).
	ConfidenceDecayHistoryTrigger = RecomputeReasonDecayTick
)

// ConfidenceDecayOptions controls one maintenance pass. Zero values take
// the documented defaults; Now is injectable for deterministic tests.
type ConfidenceDecayOptions struct {
	Now    time.Time
	Limit  int
	MinAge time.Duration
	DryRun bool
}

// ConfidenceDecayResult is the machine-readable report of one pass.
type ConfidenceDecayResult struct {
	DryRun    bool     `json:"dry_run"`
	Due       int      `json:"due"`
	Scanned   int      `json:"scanned"`
	Changed   int      `json:"changed"`
	Unchanged int      `json:"unchanged"`
	Failed    int      `json:"failed"`
	Deferred  int      `json:"deferred"`
	Failures  []string `json:"failures,omitempty"`
}

type confidenceDecayCandidate struct {
	id           string
	artifactType string
}

// confidenceDecayDueSelect is the memories-backed artifact population
// whose last confidence check is due. "Last check" is the maintenance
// watermark when present, otherwise the artifact's creation time, so a
// never-checked row becomes due once it is older than MinAge — not
// immediately. The CASE on typeof() handles both the TEXT timestamps
// written by tool saves and legacy INTEGER epochs.
//
// Both the count and the page query read this one definition, so the
// reported backlog cannot drift from the rows the pass actually scans.
//
// Terminal states are EXCLUDED from the participant set:
//
//   - status='challenged' — a challenge pins the memory's confidence to
//     ChallengedMemoryConfidenceFloor and expires its evidence, so that
//     fresh evidence is the only thing that can re-elevate it (F7.1).
//     Recomputing such a memory from an empty evidence set derives the
//     INITIAL confidence, which would silently REVERSE the challenge.
//   - status='proven' — a theory the operator has explicitly proven. Its
//     cached confidence decaying to the invalidation threshold would
//     enqueue a confidence_floor cascade that invalidates every
//     downstream artifact citing a theory the operator declared sound.
//
// The json_extract is guarded by json_valid because json_extract raises
// "malformed JSON" on a non-JSON metadata value, which would abort the
// whole participant query. The trigger that writes metadata uses the
// same guard (internal/core/db.go).
const confidenceDecayDueSelect = `
SELECT id, collection, last_check FROM (
	SELECT id, collection,
	       CASE
	         WHEN confidence_checked_at IS NOT NULL THEN confidence_checked_at
	         WHEN typeof(created_at) IN ('integer','real') THEN CAST(created_at AS INTEGER)
	         ELSE COALESCE(CAST(strftime('%s', created_at) AS INTEGER), 0)
	       END AS last_check
	FROM memories
	WHERE deleted_at IS NULL
	  AND retired_at IS NULL
	  AND COALESCE(is_prime_directive, 0) = 0
	  AND COALESCE(collection, '') IN ('', 'memories', 'theories', 'decisions')
	  AND confidence IS NOT NULL
	  AND COALESCE(
	        CASE WHEN json_valid(metadata) THEN json_extract(metadata, '$.status') END,
	        ''
	      ) NOT IN ('challenged', 'proven')
)`

const confidenceDecayDueWhere = ` WHERE last_check <= ?`

// RunConfidenceDecay applies time decay to a bounded batch of due
// artifacts. It is safe to re-run: the watermark advances only on a
// committed artifact, so a completed pass is a no-op until MinAge
// elapses, and a failed pass retries exactly the artifacts it did not
// finish.
func (dm *DatabaseManager) RunConfidenceDecay(opts ConfidenceDecayOptions) (ConfidenceDecayResult, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultConfidenceDecayLimit
	}
	if limit > MaxConfidenceDecayLimit {
		limit = MaxConfidenceDecayLimit
	}
	minAge := opts.MinAge
	if minAge <= 0 {
		minAge = DefaultConfidenceDecayMinAge
	}
	cutoff := now.Add(-minAge).Unix()

	res := ConfidenceDecayResult{DryRun: opts.DryRun}

	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM (`+confidenceDecayDueSelect+confidenceDecayDueWhere+`)`,
		cutoff,
	).Scan(&res.Due); err != nil {
		return res, fmt.Errorf("confidence decay: count due artifacts: %w", err)
	}

	candidates, err := dm.loadConfidenceDecayCandidates(cutoff, limit)
	if err != nil {
		return res, err
	}
	res.Scanned = len(candidates)
	if res.Due > res.Scanned {
		res.Deferred = res.Due - res.Scanned
	}

	for _, c := range candidates {
		var changed bool
		if opts.DryRun {
			changed, err = dm.confidenceDecayWouldChange(c, now)
		} else {
			changed, err = dm.applyConfidenceDecay(c, now)
		}
		if err != nil {
			res.Failed++
			if len(res.Failures) < 20 {
				res.Failures = append(res.Failures, fmt.Sprintf("%s/%s: %v", c.artifactType, c.id, err))
			}
			continue
		}
		if changed {
			res.Changed++
		} else {
			res.Unchanged++
		}
	}
	return res, nil
}

func (dm *DatabaseManager) loadConfidenceDecayCandidates(cutoff int64, limit int) ([]confidenceDecayCandidate, error) {
	rows, err := dm.db.Query(
		confidenceDecayDueSelect+confidenceDecayDueWhere+` ORDER BY last_check ASC, id ASC LIMIT ?`,
		cutoff, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("confidence decay: select due artifacts: %w", err)
	}
	defer rows.Close()

	var out []confidenceDecayCandidate
	for rows.Next() {
		var id, collection string
		var lastCheck int64
		if err := rows.Scan(&id, &collection, &lastCheck); err != nil {
			return nil, fmt.Errorf("confidence decay: scan due artifact: %w", err)
		}
		out = append(out, confidenceDecayCandidate{
			id:           id,
			artifactType: artifactTypeFromCollection(collection),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("confidence decay: iterate due artifacts: %w", err)
	}
	return out, nil
}

// applyConfidenceDecay recomputes one artifact and advances its
// watermark in a single transaction. WithTx retries a busy Begin, and
// the callback's DBNode makes the recompute's confidence write, history
// append and cascade enqueue atomic with the watermark update.
func (dm *DatabaseManager) applyConfidenceDecay(c confidenceDecayCandidate, now time.Time) (bool, error) {
	var changed bool
	err := dm.WithTx(func(node DBNode) error {
		var err error
		changed, err = recomputeConfidenceAt(node, c.id, c.artifactType, ConfidenceDecayHistoryTrigger, now, true)
		if err != nil {
			return err
		}
		// The watermark advances even when the value did not change: that
		// is what keeps the next pass from re-selecting this artifact. It
		// is a maintenance fact ("checked at T"), not a derived value.
		// retries=5 (issue #164): the documented ExecTracked write budget.
		// The recompute above does not write when the confidence did not
		// change, so this watermark advance is sometimes the transaction's
		// first write — and therefore sometimes the statement that collides.
		if _, err := node.ExecTracked(
			`UPDATE memories SET confidence_checked_at = ? WHERE id = ?`, 5, now.Unix(), c.id,
		); err != nil {
			return fmt.Errorf("advance watermark: %w", err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// confidenceDecayWouldChange reports whether a pass at `now` would move
// the stored value, without writing anything. It mirrors the real
// computation (same evidence loader, same formula, same clock) instead
// of calling ExplainConfidence, whose internal clock is wall time.
func (dm *DatabaseManager) confidenceDecayWouldChange(c confidenceDecayCandidate, now time.Time) (bool, error) {
	ev, lastPositiveAt, err := loadEvidenceForRecompute(dm, c.id, c.artifactType, now)
	if err != nil {
		return false, err
	}
	derived := computeConfidence(c.artifactType, ev, now, lastPositiveAt, 0.005)
	stored, ok := readArtifactConfidence(dm, c.id, c.artifactType)
	if !ok {
		return false, fmt.Errorf("no stored confidence")
	}
	return math.Abs(derived-stored) >= 1e-6, nil
}

// ConfidenceDecayBacklog returns the number of artifacts currently due
// for a decay pass, without changing anything. Used by diagnostics.
func (dm *DatabaseManager) ConfidenceDecayBacklog(now time.Time, minAge time.Duration) (int, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if minAge <= 0 {
		minAge = DefaultConfidenceDecayMinAge
	}
	var n int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM (`+confidenceDecayDueSelect+confidenceDecayDueWhere+`)`,
		now.Add(-minAge).Unix(),
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("confidence decay backlog: %w", err)
	}
	return n, nil
}
