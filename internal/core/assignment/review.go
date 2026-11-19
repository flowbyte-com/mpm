package assignment

import (
	"fmt"

	internal "github.com/flowbyte-com/mpm-core"
)

// RecordReviewResult records an INDEPENDENT REVIEW verdict.
//
// PR 1's state machine makes the SUBJECT's own row carry the verdict: an
// implementer travels `completed → review_passed`. Somebody must perform
// that transition, and the only party with the standing to do it is the
// independent reviewer — a separate assignment with role `reviewer`.
//
// THE TARGET IS FIXED BY THE DELEGATION. The reviewer's payload names the
// assignment it reviews:
//
//	{"review_target": "at-0123456789abcdef"}
//
// so a reviewer cannot be re-pointed at an arbitrary subject afterwards.
// The delegation that created it decided what it would judge.
//
// INDEPENDENCE. The subject must not be an ANCESTOR of the reviewer: a
// reviewer delegated BY the work it reviews is refused, because the
// subject is on the reviewer's parent chain. The walk terminates because
// the depth is immutable and therefore strictly decreasing.
//
// The verdict:
//
//   - `passed` moves the subject `completed → review_passed` and the
//     reviewer `in_progress → review_passed`: two transitions, two
//     ledger entries, one transaction;
//   - `changes_requested` leaves the subject where it is and moves the
//     reviewer `in_progress → completed`. The subject ALSO receives a
//     `note`, so a failed review is not invisible on the work it
//     judges — the one documented exception to "one event per
//     transition", and it is a note rather than a transition.
func (s *Store) RecordReviewResult(p ReviewResultParams) (*Assignment, error) {
	switch p.Verdict {
	case VerdictPassed, VerdictChangesRequested:
	default:
		return nil, fmt.Errorf("%w: the verdict must be %q or %q",
			ErrConstraint, VerdictPassed, VerdictChangesRequested)
	}

	var out *Assignment
	err := s.db.WithTx(func(node internal.DBNode) error {
		reviewer, err := getAssignmentTx(node, p.ReviewerID)
		if err != nil {
			return err
		}
		if reviewer.Role != RoleReviewer {
			return fmt.Errorf("%w: %s is a %s and does not review",
				ErrConstraint, reviewer.ID, reviewer.Role)
		}
		if p.Epoch != reviewer.ClaimEpoch {
			return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, reviewer.ID, reviewer.ClaimEpoch)
		}
		if reviewer.ClaimEpoch == 0 {
			return fmt.Errorf("%w: %s holds no claim", ErrConstraint, reviewer.ID)
		}
		if reviewer.State != StateInProgress {
			return fmt.Errorf("%w: %s is %s and is not reviewing",
				ErrIllegalTransition, reviewer.ID, reviewer.State)
		}

		targetID, _ := reviewer.Payload[PayloadKeyReviewTarget].(string)
		if targetID == "" {
			return fmt.Errorf(
				"%w: %s names no review target — its payload carries no %q",
				ErrConstraint, reviewer.ID, PayloadKeyReviewTarget)
		}
		subject, err := getAssignmentTx(node, targetID)
		if err != nil {
			return err
		}
		if subject.ID == reviewer.ID {
			return fmt.Errorf("%w: %s reviews itself", ErrConstraint, reviewer.ID)
		}
		if subject.Role != RoleImplementer {
			return fmt.Errorf("%w: %s is a %s and is not reviewable",
				ErrConstraint, subject.ID, subject.Role)
		}
		if subject.ProjectID != reviewer.ProjectID || subject.RootID != reviewer.RootID {
			return fmt.Errorf("%w: %s is not in the reviewer's tree", ErrConstraint, subject.ID)
		}
		ancestor, err := isAncestorOf(node, reviewer, subject.ID)
		if err != nil {
			return err
		}
		if ancestor {
			return fmt.Errorf(
				"%w: %s delegated %s, so it does not review its own work",
				ErrConstraint, reviewer.ParentID, reviewer.ID)
		}

		now := s.Now()

		if p.Verdict == VerdictPassed {
			if subject.State != StateCompleted {
				return fmt.Errorf("%w: %s is %s and has not completed the work",
					ErrIllegalTransition, subject.ID, subject.State)
			}
			// The SUBJECT moves first: the verdict is the subject's
			// transition, and the reviewer's own state change
			// records that the review is over.
			if err := s.transitionTx(node, transitionSpec{
				id:             subject.ID,
				presentedEpoch: subject.ClaimEpoch,
				// The subject is not written on the reviewer's
				// authority: it is written because the review
				// passed. Its epoch is its own, read in this
				// transaction, so the clause says "the subject did
				// not change under me".
				toState:        StateReviewPassed,
				eventType:      EventReviewPassed,
				actor:          p.Actor,
				note:           p.Note,
				eventPayload: map[string]interface{}{
					"reviewer_id": reviewer.ID,
					"verdict":     VerdictPassed,
				},
			}, now); err != nil {
				return err
			}
		} else {
			// The subject does NOT move, so the verdict would
			// otherwise leave no trace on the work it judges. The
			// note is that trace.
			//
			// THE REWORK IS A NEW ASSIGNMENT. The subject stays
			// 'completed' because the state machine has no path
			// back from 'completed' — PR 1 drew it as a forward
			// path, and PR 2 does not widen it. The rework is a
			// CHILD delegated from the subject, which is fresh and
			// walks the path from the beginning. That is the
			// honest shape of "the review asked for changes":
			// the work that was asked for is a new unit of work,
			// and the old one is left standing as the record of
			// what was tried.
			if err := s.injectFault("review-note"); err != nil {
				return err
			}
			if _, err := appendEvent(node, subject.ID, EventNote, "", "", p.Actor, nil,
				p.Note,
				map[string]interface{}{
					"reviewer_id": reviewer.ID,
					"verdict":     VerdictChangesRequested,
					"state":       subject.State,
				}, now); err != nil {
				return err
			}
		}

		toState := StateCompleted
		if p.Verdict == VerdictPassed {
			toState = StateReviewPassed
		}
		if err := s.transitionTx(node, transitionSpec{
			id:             reviewer.ID,
			presentedEpoch: p.Epoch,
			toState:        toState,
			eventType:      eventTypeForState(toState),
			actor:          p.Actor,
			note:           p.Note,
			eventPayload: map[string]interface{}{
				"subject_id": subject.ID,
				"verdict":    p.Verdict,
			},
		}, now); err != nil {
			return err
		}

		row, err := getAssignmentTx(node, reviewer.ID)
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// transitionTx is the in-transaction half of the transition helper.
//
// The service's other transitions run in their own transaction; the
// reviewer's verdict moves TWO assignments, so both moves and both ledger
// entries share one transaction, and neither can land without the other.
func (s *Store) transitionTx(node internal.DBNode, spec transitionSpec, now int64) error {
	current, err := getAssignmentTx(node, spec.id)
	if err != nil {
		return err
	}
	if spec.presentedEpoch != current.ClaimEpoch {
		return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, spec.id, current.ClaimEpoch)
	}
	if err := checkTransition(current.Role, current.State, spec.toState); err != nil {
		return fmt.Errorf("%w: %s", err, spec.id)
	}

	setClause, args := buildSetClause(spec.setColumns)
	args = append([]interface{}{spec.toState, now}, args...)
	// The WHERE clause carries the PRESENTED epoch: see the note in
	// lifecycle.go's transition.
	args = append(args, spec.id, spec.presentedEpoch)

	if err := s.injectFault("transition"); err != nil {
		return err
	}
	res, err := node.ExecTracked(
		`UPDATE agent_tasks SET state = ?, updated_at = ?`+setClause+
			` WHERE id = ? AND claim_epoch = ?`,
		5, args...)
	if err != nil {
		return storageRefusal(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("assignment: read the affected rows: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: %s moved while the write was in flight", ErrStaleClaim, spec.id)
	}

	if err := s.injectFault("transition-event"); err != nil {
		return err
	}
	epoch := current.ClaimEpoch
	actor := spec.actor
	if actor.Address == "" {
		actor = Actor{Address: current.Recipient, Kind: "agent"}
	}
	if _, err := appendEvent(node, spec.id, spec.eventType, current.State, spec.toState,
		actor, &epoch, spec.note, spec.eventPayload, now); err != nil {
		return err
	}
	return nil
}

// isAncestorOf reports whether candidateID is on the parent chain of the
// assignment.
//
// The walk terminates because the depth is immutable: the chain's depths
// strictly decrease, so it cannot be longer than the tree is deep.
func isAncestorOf(node internal.DBNode, from *Assignment, candidateID string) (bool, error) {
	current := from
	for current.ParentID != "" {
		if current.ParentID == candidateID {
			return true, nil
		}
		parent, err := getAssignmentTx(node, current.ParentID)
		if err != nil {
			return false, err
		}
		if parent.Depth >= current.Depth {
			// The depth is immutable, so this cannot happen; and
			// if it did, the chain is not strictly decreasing and
			// the walk would not terminate.
			return false, fmt.Errorf(
				"assignment: the delegation chain of %s does not decrease in depth", from.ID)
		}
		current = parent
	}
	return false, nil
}

// eventTypeForState maps a state to the ledger entry its transition
// writes.
func eventTypeForState(state string) string {
	switch state {
	case StateDelivered:
		return EventDelivered
	case StateAcknowledged:
		return EventAcknowledged
	case StateAccepted:
		return EventAccepted
	case StateInProgress:
		return EventStarted
	case StateCompleted:
		return EventCompleted
	case StateReviewPassed:
		return EventReviewPassed
	case StateAwaitingOperator:
		return EventAwaitingOperator
	case StateOperatorApproved:
		return EventOperatorApproved
	case StateCancelled:
		return EventCancelled
	default:
		return EventNote
	}
}
