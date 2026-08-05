package capability

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	internal "github.com/flowbyte-com/mpm-core"
)

// =============================================================================
// store_lifecycle.go — named state transition methods
//
// Every public method here is one named business operation. Each one:
//   1. Pre-flight reads the current row outside the transaction (so we
//      can return a clean ErrInvalidTransition without partial writes)
//   2. Validates the transition is legal via CapabilityState.CanTransitionTo
//   3. Opens a transaction, updates state, writes the event row
//   4. Commits (or rolls back on any error)
//
// The cascading methods that involve lineage walks (ExecuteRollbackWithShatter)
// live in store_cascade.go. This file is the simple-case surface.
// =============================================================================

// PromotionDecision bundles the probation criteria check result so the
// caller can surface a useful error message (or, in the CLI, a hint
// like "3 more successes needed").
type PromotionDecision struct {
	Eligible                bool
	SuccessesSoFar          int
	RequiredSuccesses       int
	FailureRate             float64
	FailureRateCeiling      float64
	Reason                  string
}

// CheckProbationCriteria evaluates whether a probation capability has
// met its promotion criteria. Read-only; does NOT mutate state.
// Spec §1.3 row 4:
//
//	`probation → active` requires:
//	  success_count >= probation_required_success_count
//	  failure_rate  <= probation_max_failure_rate
//
// The failure rate is computed as failure_count / (success_count + failure_count),
// rounded to 4 decimal places. A capability with zero invocations is
// not eligible (you cannot prove something with no evidence).
func CheckProbationCriteria(c *Capability) PromotionDecision {
	if c == nil {
		return PromotionDecision{Eligible: false, Reason: "nil capability"}
	}
	required := c.ProbationRequiredSuccessCount
	if required <= 0 {
		required = 5
	}
	ceiling := c.ProbationMaxFailureRate
	if ceiling <= 0 {
		ceiling = 0.10
	}
	total := c.SuccessCount + c.FailureCount
	if total == 0 {
		return PromotionDecision{
			Eligible:           false,
			SuccessesSoFar:     c.SuccessCount,
			RequiredSuccesses:  required,
			FailureRate:        0,
			FailureRateCeiling: ceiling,
			Reason:             "no invocations recorded yet",
		}
	}
	rate := float64(c.FailureCount) / float64(total)
	if c.SuccessCount < required {
		return PromotionDecision{
			Eligible:           false,
			SuccessesSoFar:     c.SuccessCount,
			RequiredSuccesses:  required,
			FailureRate:        rate,
			FailureRateCeiling: ceiling,
			Reason: fmt.Sprintf("only %d/%d successes observed",
				c.SuccessCount, required),
		}
	}
	if rate > ceiling {
		return PromotionDecision{
			Eligible:           false,
			SuccessesSoFar:     c.SuccessCount,
			RequiredSuccesses:  required,
			FailureRate:        rate,
			FailureRateCeiling: ceiling,
			Reason: fmt.Sprintf("failure rate %.4f exceeds ceiling %.4f",
				rate, ceiling),
		}
	}
	return PromotionDecision{
		Eligible:           true,
		SuccessesSoFar:     c.SuccessCount,
		RequiredSuccesses:  required,
		FailureRate:        rate,
		FailureRateCeiling: ceiling,
	}
}

// PromoteToActive moves a capability from probation to active and
// retires the predecessor (if any) in the same transaction. This is
// the load-bearing "new revision goes live" transition of spec §3.5.
//
// Steps:
//   1. Read current row; refuse if not in state='probation'
//   2. Evaluate CheckProbationCriteria; refuse if not eligible
//   3. UPDATE this row: state='active', promoted_at=now
//   4. If created_from_id is set and the predecessor is currently
//      active: retire it (state='retired', superseded_by_id=this.id)
//      and write a retirement event
//   5. INSERT promotion event for this row
//
// Returns the PromotionDecision so the CLI can surface "needs N more
// successes" hints when the call is rejected.
func (s *Store) PromoteToActive(id string) (PromotionDecision, error) {
	var decision PromotionDecision

	err := s.withTx(func(n internal.DBNode) error {
		// 1. Read current row inside the transaction so we get a
		// consistent snapshot (no TOCTOU between state check and UPDATE).
		c, err := s.getCapabilityTx(n, id)
		if err != nil {
			return err
		}
		if !c.State.CanTransitionTo(StateActive) {
			return fmt.Errorf("%w: %s → active", ErrInvalidTransition, c.State)
		}

		// 2. Probation criteria check (only meaningful from probation).
		// Operators can override via the lifecycle methods that bypass
		// the criteria (not exposed yet — left for operator tooling).
		if c.State == StateProbation {
			decision = CheckProbationCriteria(c)
			if !decision.Eligible {
				return fmt.Errorf("capability: not eligible for promotion: %s", decision.Reason)
			}
		}

		now := s.Now()

		// 3. UPDATE this row.
		if _, err := n.ExecTracked(
			`UPDATE capabilities
			 SET state = 'active',
			     state_changed_at = ?,
			     promoted_at = ?,
			     updated_at = ?
			 WHERE id = ? AND state = 'probation'`,
			3, now, now, now, id,
		); err != nil {
			return wrapDBError("PromoteToActive", err)
		}

		// 4. Retire predecessor (spec §3.5).
		if c.CreatedFromID != nil {
			if err := s.retirePredecessorTx(n, *c.CreatedFromID, id, now); err != nil {
				return err
			}
		}

		// 5. INSERT promotion event for this row.
		return s.insertTransitionEventTx(n, EventPromotion, id, c.State, StateActive, "scheduler", "criteria_met", nil)
	})

	if err != nil {
		return decision, err
	}
	return decision, nil
}

// EnterProbation moves a capability from validated to probation. If
// the capability has a created_from_id pointing at an active predecessor,
// this method sets the predecessor's superseded_by_id (shadow flag from
// spec §3.5 — the predecessor stays active until the successor reaches
// active, at which point PromoteToActive retires it).
func (s *Store) EnterProbation(id string, actor string) error {
	return s.withTx(func(n internal.DBNode) error {
		c, err := s.getCapabilityTx(n, id)
		if err != nil {
			return err
		}
		if !c.State.CanTransitionTo(StateProbation) {
			return fmt.Errorf("%w: %s → probation", ErrInvalidTransition, c.State)
		}
		now := s.Now()
		if _, err := n.ExecTracked(
			`UPDATE capabilities SET state = 'probation', state_changed_at = ?, updated_at = ?
			 WHERE id = ? AND state = 'validated'`,
			3, now, now, id,
		); err != nil {
			return wrapDBError("EnterProbation", err)
		}
		// Shadow flag the predecessor (does not change its state).
		if c.CreatedFromID != nil {
			if _, err := n.ExecTracked(
				`UPDATE capabilities
				 SET superseded_by_id = ?, updated_at = ?
				 WHERE id = ? AND state = 'active' AND superseded_by_id IS NULL`,
				3, id, now, *c.CreatedFromID,
			); err != nil {
				return wrapDBError("EnterProbation: shadow predecessor", err)
			}
		}
		return s.insertTransitionEventTx(n, EventPromotion, id, c.State, StateProbation, actor, "operator_approval", nil)
	})
}

// MarkValidated moves a capability from linted to validated after the
// dry-run exit 0 inside bwrap (spec §4). Used by the Forge pipeline
// after a successful dry-run.
func (s *Store) MarkValidated(id string) error {
	return s.simpleTransition(id, StateLinted, StateValidated, EventPromotion, "scheduler", "dry_run_ok", nil)
}

// MarkDegraded moves an active capability to degraded when the soft
// failure-rate threshold is breached. Spec §1.4: default 0.20. The
// capability remains callable (IsCallable is true for degraded).
func (s *Store) MarkDegraded(id, reason string) error {
	return s.simpleTransition(id, StateActive, StateDegraded, EventDemotion, "scheduler", reason, nil)
}

// Recover moves a degraded capability back to active after the
// failure rate has stayed below the soft threshold for the recovery
// window (spec §1.3 row 8: 5 consecutive successes).
func (s *Store) Recover(id string) error {
	return s.simpleTransition(id, StateDegraded, StateActive, EventPromotion, "scheduler", "recovery", nil)
}

// MarkNeedsRevision moves active→needs_revision on operator flag.
// Attaches the operator's note to metadata.operator_notes per spec
// §1.3 row 10.
func (s *Store) MarkNeedsRevision(id, operatorNote string) error {
	meta := EventMetadata{"operator_notes": operatorNote}
	if operatorNote == "" {
		meta = nil
	}
	return s.simpleTransition(id, StateActive, StateNeedsRevision, EventDemotion, "operator", "operator_flag", meta)
}

// MarkFractured moves a degraded capability to fractured after the
// 3-failures-in-60s cluster threshold. Emits a fracture event with
// cluster evidence in metadata (window_seconds, failure_count, stderr).
//
// Spec §1.3 row 9 mentions writing to epistemic_cascade_outbox when
// author_theory_id is set; that integration is the scheduler handler's
// responsibility (it observes new fracture events and triggers the
// cascade write). The Store's job here is to record the transition.
func (s *Store) MarkFractured(id, reason, stderr string, windowSec int) error {
	meta := EventMetadata{
		"fracture_reason": reason,
		"window_seconds":  windowSec,
	}
	if stderr != "" {
		meta["failure_trace"] = stderr
	}
	err := s.simpleTransition(id, StateDegraded, StateFractured, EventFracture, "scheduler", reason, meta)
	if err != nil {
		return err
	}
	// Also stamp last_failure_stderr on the capability row itself so
	// `mpm skill show <id>` can surface it without joining events.
	return s.withTx(func(n internal.DBNode) error {
		now := s.Now()
		_, err := n.ExecTracked(
			`UPDATE capabilities SET last_failure_stderr = ?, last_failure_at = ?, updated_at = ?
			 WHERE id = ?`, 3, stderr, now, now, id)
		return wrapDBError("MarkFractured:stamp", err)
	})
}

// Retire is the explicit "* → retired" transition from spec §1.3.
// Used by operator tooling and by the scheduler when a capability is
// being permanently abandoned (different from rolled_back which is
// reserved for observation-window breach — see store_cascade.go).
//
// Can target any non-terminal state; refusal of self-retirement of
// already-retired rows happens via ErrNotFound from the lookup.
func (s *Store) Retire(id, reason string) error {
	return s.simpleTransitionAny(id, StateRetired, EventRetirement, "operator", reason, nil)
}

// RecordInvocationOutcome updates the success/failure counters and
// latency for a capability. Does NOT change state — the transition
// methods decide that based on the new metrics.
//
// Returns the updated Capability row so callers can decide whether a
// transition is now warranted (e.g. switch to degraded if failure
// rate crossed soft threshold).
func (s *Store) RecordInvocationOutcome(id string, success bool, latencyMs int64, stderr string) (*Capability, error) {
	var updated *Capability
	err := s.withTx(func(n internal.DBNode) error {
		c, err := s.getCapabilityTx(n, id)
		if err != nil {
			return err
		}
		now := s.Now()

		// Compute new metrics. Latency uses a running mean weighted by
		// the existing avg_latency_ms (recency weight = 1 / new total).
		total := int64(c.SuccessCount + c.FailureCount)
		newTotal := total + 1
		newAvg := c.AvgLatencyMs
		if total > 0 {
			newAvg = (c.AvgLatencyMs*float64(total) + float64(latencyMs)) / float64(newTotal)
		} else {
			newAvg = float64(latencyMs)
		}

		var succDelta, failDelta int
		var stderrPtr *string
		if success {
			succDelta = 1
		} else {
			failDelta = 1
			if stderr != "" {
				stderrPtr = &stderr
			}
		}

		q := `UPDATE capabilities
		      SET success_count = success_count + ?,
		          failure_count = failure_count + ?,
		          avg_latency_ms = ?,
		          last_invoked_at = ?,
		          last_failure_at = CASE WHEN ? THEN last_failure_at ELSE ? END,
		          last_failure_stderr = COALESCE(?, last_failure_stderr),
		          updated_at = ?
		      WHERE id = ?`
		if _, err := n.ExecTracked(q, 3,
			succDelta, failDelta, newAvg, now,
			success, now, stderrPtr, now, id,
		); err != nil {
			return wrapDBError("RecordInvocationOutcome", err)
		}

		// Re-read for the caller.
		updated, err = s.getCapabilityTx(n, id)
		return err
	})
	return updated, err
}

// =============================================================================
// internal helpers
// =============================================================================

// simpleTransition is the canonical "read → validate → update → event"
// pattern for transitions that don't have per-transition side effects.
// Use named methods (MarkDegraded, Recover, etc.) at call sites; this
// helper keeps the logic in one place.
func (s *Store) simpleTransition(
	id string,
	from, to CapabilityState,
	eventType EventType,
	actor, reason string,
	meta EventMetadata,
) error {
	return s.withTx(func(n internal.DBNode) error {
		c, err := s.getCapabilityTx(n, id)
		if err != nil {
			return err
		}
		if c.State != from {
			return fmt.Errorf("%w: cannot %s from %s (expected %s)",
				ErrInvalidTransition, to, c.State, from)
		}
		if !c.State.CanTransitionTo(to) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, c.State, to)
		}
		now := s.Now()
		q := fmt.Sprintf(
			`UPDATE capabilities SET state = ?, state_changed_at = ?, updated_at = ?
			 WHERE id = ? AND state = ?`)
		if _, err := n.ExecTracked(q, 3, to, now, now, id, from); err != nil {
			return wrapDBError("simpleTransition", err)
		}
		fromState := c.State
		return s.insertTransitionEventTx(n, eventType, id, fromState, to, actor, reason, meta)
	})
}

// simpleTransitionAny is like simpleTransition but accepts any from-state
// that CanTransitionTo(to) approves. Used by Retire, which leverages
// the wildcard "* → retired" rule.
func (s *Store) simpleTransitionAny(
	id string,
	to CapabilityState,
	eventType EventType,
	actor, reason string,
	meta EventMetadata,
) error {
	return s.withTx(func(n internal.DBNode) error {
		c, err := s.getCapabilityTx(n, id)
		if err != nil {
			return err
		}
		if !c.State.CanTransitionTo(to) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, c.State, to)
		}
		now := s.Now()
		q := fmt.Sprintf(
			`UPDATE capabilities SET state = ?, state_changed_at = ?, updated_at = ?
			 WHERE id = ?`)
		if _, err := n.ExecTracked(q, 3, to, now, now, id); err != nil {
			return wrapDBError("simpleTransitionAny", err)
		}
		fromState := c.State
		return s.insertTransitionEventTx(n, eventType, id, fromState, to, actor, reason, meta)
	})
}

// retirePredecessor moves the predecessor from active to retired and
// stamps superseded_by_id, in the same transaction as the successor's
// promotion. Refuses if the predecessor is not in state='active' (it
// might have been fractured or retired already — that's an audit
// signal, not a silent skip).
func (s *Store) retirePredecessorTx(
	n internal.DBNode,
	predecessorID, successorID string,
	now int64,
) error {
	res, err := n.ExecTracked(
		`UPDATE capabilities
		 SET state = 'retired', state_changed_at = ?, updated_at = ?,
		     superseded_by_id = ?
		 WHERE id = ? AND state = 'active'`,
		3, now, now, successorID, predecessorID,
	)
	if err != nil {
		return wrapDBError("retirePredecessorTx", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Predecessor was not in active state. Spec §5.2.1 escalation
		// path is reserved for the rollback cascade; for the promotion
		// case we surface a distinct error so the operator can
		// investigate (a fractured predecessor should not be silently
		// shadowed by a promotion).
		return fmt.Errorf("%w: predecessor %s not in active state",
			ErrPredecessorBroken, predecessorID)
	}
	meta := EventMetadata{"reason": "superseded_by_revision"}
	return s.insertTransitionEventTx(n, EventRetirement, predecessorID, StateActive, StateRetired, "scheduler", "superseded_by_revision", meta)
}

// getCapabilityTx reads a capability row inside a transaction (so it
// participates in the tx's snapshot semantics). Returns ErrNotFound
// if the row does not exist or is soft-deleted.
func (s *Store) getCapabilityTx(n internal.DBNode, id string) (*Capability, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("capability: getCapabilityTx: id is required")
	}
	row := n.QueryRowTracked(selectCapabilityBase+` WHERE id = ? AND deleted_at IS NULL`, id)
	c, err := scanCapability(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// insertTransitionEventTx writes one synthetic event row. Centralized
// because the schema's NOT NULL constraints on actor/event_type and the
// JSON metadata shape are easy to get wrong by hand at call sites.
// Uses s.Now() so event timestamps honour the Store's injected clock.
func (s *Store) insertTransitionEventTx(
	n internal.DBNode,
	eventType EventType,
	capabilityID string,
	from, to CapabilityState,
	actor, reason string,
	meta EventMetadata,
) error {
	if err := eventType.Validate(); err != nil {
		return fmt.Errorf("capability: insertTransitionEventTx: %w", err)
	}
	id := uuid.New().String()
	q := `INSERT INTO capability_events
	      (id, capability_id, event_type, occurred_at, actor,
	       from_state, to_state, reason, metadata)
	      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := n.ExecTracked(q, 3,
		id, capabilityID, eventType, s.Now(), actor,
		string(from), string(to), reason, meta,
	)
	return wrapDBError("insertTransitionEventTx", err)
}
