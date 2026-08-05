package capability

import (
	"context"
	"fmt"
)

// =============================================================================
// forge_probation.go — probation completion tick (spec §3.5 / FG-10)
//
// Capabilities in state='probation' accumulate invocation telemetry
// until they meet the promotion criteria (success_count >=
// probation_required_success_count AND failure_rate <=
// probation_max_failure_rate). This file provides the periodic
// sweep that graduates eligible ones to state='active'.
//
// The tick is a SEPARATE concern from the Forge's Propose
// pipeline. Proposals go in at state='validated' (post-dry-run);
// moving them to probation is operator-initiated (out of scope
// for F-5). The tick's job is the probation → active transition,
// and only that transition.
//
// Tick semantics:
//
//   * At most one transition per (capability, tick) call. If the
//     transition fails, the capability stays in probation for the
//     next tick.
//   * Each transition is its own transaction (PromoteToActive's
//     existing atomic unit). A failure in one does not affect
//     others.
//   * The tick is idempotent — running it twice in a row on the
//     same data set yields the same result (the second run sees
//     no probation rows).
//
// F-9 (scheduler) wires this into a periodic handler.
// =============================================================================

// ProbationTickResult is the outcome of one CheckProbationCompletion
// call. PromotedIDs lists the capabilities that graduated to
// active this tick; SkippedIDs lists probation rows that were
// inspected but did not yet meet criteria (useful for the CLI
// to surface "3 more successes needed" hints without re-querying).
type ProbationTickResult struct {
	PromotedIDs []string
	SkippedIDs  []string
	Errors      []ProbationError
}

// ProbationError captures one failure to transition a specific
// capability. Returned in the Errors slice; the tick continues
// to other capabilities even if one fails.
type ProbationError struct {
	CapabilityID string
	Err          error
}

// CheckProbationCompletion scans every capability in state='probation',
// runs CheckProbationCriteria, and transitions the eligible ones
// to state='active' via PromoteToActive. Returns a result
// describing what happened, with errors captured per-capability
// rather than as a single error.
//
// The check is bounded by ctx. Cancellation is honoured between
// capabilities (so a long sweep can be aborted cleanly mid-way).
func (s *Store) CheckProbationCompletion(ctx context.Context) (*ProbationTickResult, error) {
	result := &ProbationTickResult{}

	// Step 1: enumerate probation rows. Read-only; no transaction.
	// The query excludes soft-deleted rows (defensive — a probation
	// row that was soft-deleted is not a candidate for promotion).
	rows, err := s.dm.QueryTracked(
		`SELECT id FROM capabilities
		 WHERE state = 'probation' AND deleted_at IS NULL
		 ORDER BY state_changed_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("capability: probation tick: list: %w", err)
	}
	var probationIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("capability: probation tick: scan: %w", err)
		}
		probationIDs = append(probationIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("capability: probation tick: rows: %w", err)
	}

	// Step 2: for each, attempt the transition. Per-capability
	// errors are collected in result.Errors; the tick continues.
	for _, id := range probationIDs {
		// Honour ctx cancellation between rows so a long sweep
		// can be aborted.
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}

		decision, err := s.PromoteToActive(id)
		if err != nil {
			// Two failure shapes:
			//   (a) "not eligible" — the criteria check
			//       returned not-eligible. We treat this as
			//       "skip" not "error"; the capability just
			//       needs more evidence.
			//   (b) anything else — true error. Capture it.
			if decision.Reason != "" && !decision.Eligible {
				result.SkippedIDs = append(result.SkippedIDs, id)
				continue
			}
			result.Errors = append(result.Errors, ProbationError{
				CapabilityID: id,
				Err:          err,
			})
			continue
		}
		if !decision.Eligible {
			result.SkippedIDs = append(result.SkippedIDs, id)
			continue
		}
		result.PromotedIDs = append(result.PromotedIDs, id)
	}
	return result, nil
}
