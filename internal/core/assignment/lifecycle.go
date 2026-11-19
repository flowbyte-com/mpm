package assignment

import (
	"fmt"
	"sort"
	"strings"

	internal "github.com/flowbyte-com/mpm-core"
)

// ── the one transition helper ──────────────────────────────────────────────
//
// Every state change in this package goes through here. That is what makes
// "the CLI and the API share one business-logic implementation" true in
// practice rather than in a comment: there is exactly one place that
// writes a state, and it writes the ledger entry in the same transaction.

// setColumns that a transition may write. The whitelist is explicit
// because the SET clause is built from it.
var settableColumns = map[string]bool{
	"awaiting_action":   true,
	"approval_action":   true,
	"approval_revision": true,
	"approval_source":   true,
	"approval_evidence": true,
	"approved_at":       true,
	"completed_at":      true,
}

// transitionSpec describes one state change.
type transitionSpec struct {
	// id is the assignment to move.
	id string
	// presentedEpoch is the caller's claim epoch. It must equal the
	// assignment's current claim_epoch: the write is
	// `WHERE id = ? AND claim_epoch = ?`, so a stale holder's write
	// matches NO ROW and the transition does not happen.
	presentedEpoch int64
	// toState is the new state.
	toState string
	// eventType is the ledger entry the transition writes.
	eventType string
	// actor is the attribution.
	actor Actor
	// note is the ledger note.
	note string
	// setColumns are the extra columns the transition writes.
	setColumns map[string]interface{}
	// eventPayload is the ledger entry's payload.
	eventPayload map[string]interface{}
	// requireClaim refuses the transition when the assignment holds no
	// claim. It is used by the transitions that mean "this work is
	// happening under a lease".
	requireClaim bool
}

// transition applies one state change and its ledger entry atomically.
//
// The two writes share a transaction, so there is no window in which the
// row moved and the ledger did not, and none in which the ledger moved and
// the row did not.
func (s *Store) transition(spec transitionSpec) (*Assignment, error) {
	var out *Assignment
	err := s.db.WithTx(func(node internal.DBNode) error {
		current, err := getAssignmentTx(node, spec.id)
		if err != nil {
			return err
		}
		if spec.presentedEpoch != current.ClaimEpoch {
			return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, spec.id, current.ClaimEpoch)
		}
		if spec.requireClaim && current.ClaimEpoch == 0 {
			return fmt.Errorf("%w: %s holds no claim", ErrConstraint, spec.id)
		}
		// THE PATH IS CHECKED HERE, before the write, so the caller
		// gets the API's own vocabulary rather than the schema's.
		// The schema refuses it independently.
		if err := checkTransition(current.Role, current.State, spec.toState); err != nil {
			return fmt.Errorf("%w: %s", err, spec.id)
		}

		setClause, args := buildSetClause(spec.setColumns)
		now := s.Now()
		args = append([]interface{}{spec.toState, now}, args...)
		// The WHERE clause carries the PRESENTED epoch, not the
		// row's current one. Binding the current epoch would make
		// the fence a tautology: the row was read inside this
		// transaction, so the clause would always match. The
		// presented epoch is the caller's claim about the world, and
		// it is what makes a stale holder's write match NO ROW.
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
			// The row moved under us: the epoch no longer matches.
			return fmt.Errorf("%w: %s moved while the write was in flight", ErrStaleClaim, spec.id)
		}

		if err := s.injectFault("transition-event"); err != nil {
			return err
		}
		var epoch *int64
		e := current.ClaimEpoch
		epoch = &e
		// The attribution defaults to the assignment's own recipient:
		// a structural fact the service knows, rather than a claim it
		// accepts. It is RECORDED either way, and it grants nothing.
		actor := spec.actor
		if actor.Address == "" {
			actor = Actor{Address: current.Recipient, Kind: "agent"}
		}
		if _, err := appendEvent(node, spec.id, spec.eventType, current.State, spec.toState,
			actor, epoch, spec.note, spec.eventPayload, now); err != nil {
			return err
		}

		row, err := getAssignmentTx(node, spec.id)
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

// buildSetClause renders the extra columns of a transition.
//
// The column names come from the whitelist, and the values are bound, so
// nothing a caller supplies reaches the SQL text.
func buildSetClause(cols map[string]interface{}) (string, []interface{}) {
	if len(cols) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(cols))
	for name := range cols {
		if !settableColumns[name] {
			// A programming error, not a caller error. Silently
			// dropping the column would write a transition that does
			// not carry what it was asked to carry.
			panic("assignment: transition set column " + name + " is not settable")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	args := make([]interface{}, 0, len(names))
	for _, name := range names {
		b.WriteString(", ")
		b.WriteString(name)
		b.WriteString(" = ?")
		args = append(args, cols[name])
	}
	return b.String(), args
}

// ── the transitions ───────────────────────────────────────────────────────

// Deliver hands an assignment to a runtime: `enqueued → delivered`.
//
// Delivery is NOT acceptance. An assignment that was delivered and never
// acknowledged is re-deliverable, not lost — which is why the delivery is
// a transition with its own ledger entry rather than a side effect of the
// row having been read.
func (s *Store) Deliver(p FencedParams) (*Assignment, error) {
	return s.transition(transitionSpec{
		id:             p.ID,
		presentedEpoch: p.Epoch,
		toState:        StateDelivered,
		eventType:      EventDelivered,
		actor:          p.Actor,
		note:           p.Note,
	})
}

// Acknowledge records that the recipient has taken the work:
// `delivered → acknowledged`.
//
// "I have it" is not "I will do it": acknowledgement and acceptance are
// distinct states, and the distinction is the point.
func (s *Store) Acknowledge(p FencedParams) (*Assignment, error) {
	return s.transition(transitionSpec{
		id:             p.ID,
		presentedEpoch: p.Epoch,
		toState:        StateAcknowledged,
		eventType:      EventAcknowledged,
		actor:          p.Actor,
		note:           p.Note,
	})
}

// Accept records the recipient's commitment: `acknowledged → accepted`.
func (s *Store) Accept(p FencedParams) (*Assignment, error) {
	return s.transition(transitionSpec{
		id:             p.ID,
		presentedEpoch: p.Epoch,
		toState:        StateAccepted,
		eventType:      EventAccepted,
		actor:          p.Actor,
		note:           p.Note,
	})
}

// Refuse records an explicit refusal.
//
// A refusal is not a failure and not a retry — it is the recipient
// declining, and the ledger says so. The event's payload carries
// `reason: refused`, so the ledger distinguishes a refusal from an
// ordinary cancellation even though both land in `cancelled`.
//
// The cancellation it produces is the universal one, so a refusal is
// legal from any live state: the schema's withdrawal rule is
// universal, and a narrower rule here would leave a recipient unable
// to decline work they have not yet acknowledged.
func (s *Store) Refuse(p FencedParams) (*Assignment, error) {
	return s.transition(transitionSpec{
		id:             p.ID,
		presentedEpoch: p.Epoch,
		toState:        StateCancelled,
		eventType:      EventCancelled,
		actor:          p.Actor,
		note:           p.Note,
		eventPayload:   map[string]interface{}{"reason": "refused"},
	})
}

// Start records that the work is under way: `accepted → in_progress`.
//
// It REQUIRES a claim: an assignment cannot be in progress without a
// lease, because there would be nothing to expire it.
func (s *Store) Start(p FencedParams) (*Assignment, error) {
	return s.transition(transitionSpec{
		id:             p.ID,
		presentedEpoch: p.Epoch,
		toState:        StateInProgress,
		eventType:      EventStarted,
		actor:          p.Actor,
		note:           p.Note,
		requireClaim:   true,
	})
}

// Complete records that the work is done: `in_progress → completed`.
//
// Completion is NOT approval and NOT a review verdict: it is the
// implementer's claim that the work is finished.
func (s *Store) Complete(p FencedParams) (*Assignment, error) {
	now := s.Now()
	return s.transition(transitionSpec{
		id:             p.ID,
		presentedEpoch: p.Epoch,
		toState:        StateCompleted,
		eventType:      EventCompleted,
		actor:          p.Actor,
		note:           p.Note,
		setColumns:     map[string]interface{}{"completed_at": now},
	})
}

// Cancel withdraws an assignment from any state.
//
// The cancellation is universal: legal from every state except itself,
// for every role. It is the implementer's withdrawal of the work, not a
// state-machine special case.
func (s *Store) Cancel(p CancelParams) (*Assignment, error) {
	return s.transition(transitionSpec{
		id:             p.ID,
		presentedEpoch: p.Epoch,
		toState:        StateCancelled,
		eventType:      EventCancelled,
		actor:          p.Actor,
		note:           p.Note,
	})
}

// Note appends a ledger note without changing the state.
//
// A note is not a transition, so it carries no from_state and no
// to_state: the ledger says what happened, not that the row moved.
func (s *Store) Note(p NoteParams) (*Assignment, error) {
	var out *Assignment
	err := s.db.WithTx(func(node internal.DBNode) error {
		// The row must exist: a note on an assignment that is not
		// there is a mistake, not a no-op.
		if _, err := getAssignmentTx(node, p.ID); err != nil {
			return err
		}
		now := s.Now()
		if err := s.injectFault("note"); err != nil {
			return err
		}
		if _, err := appendEvent(node, p.ID, EventNote, "", "", p.Actor, nil,
			p.Note, p.Payload, now); err != nil {
			return err
		}
		row, err := getAssignmentTx(node, p.ID)
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

// HandoffToOperator hands the work to the operator's gate:
// `review_passed → awaiting_operator`.
//
// AwaitingAction is the EXACT action the operator is being asked to
// authorise. It is written by this transition and by no other, so the
// record is attributable to the event that claims it — PR 1's trigger
// refuses a transition that does not change the field.
//
// The gate is a HAND-OFF, not a claim: the work is finished and the
// operator holds the decision.
func (s *Store) HandoffToOperator(p HandoffParams) (*Assignment, error) {
	if err := requireText("awaiting_action", p.AwaitingAction); err != nil {
		return nil, err
	}
	return s.transition(transitionSpec{
		id:             p.ID,
		presentedEpoch: p.Epoch,
		toState:        StateAwaitingOperator,
		eventType:      EventAwaitingOperator,
		actor:          p.Actor,
		note:           p.Note,
		setColumns:     map[string]interface{}{"awaiting_action": p.AwaitingAction},
		eventPayload:   map[string]interface{}{"awaiting_action": p.AwaitingAction},
	})
}

// RecordApproval records an EXTERNALLY VERIFIED approval:
// `awaiting_operator → operator_approved`.
//
// This is an operator operation and it requires an OperatorAuthority. It
// is absent from the tool registry, so no agent-facing action can reach
// it.
//
// MPM RECORDS the approval; it never manufactures or authenticates one.
// The action, the revision, the source and the evidence pointer are the
// operator's claims about what was authorised, and the row says so. The
// approval must name the SAME action the gate awaited — PR 1's trigger
// refuses it otherwise, and this method refuses it first with a clearer
// message.
//
// A change to the head revision after the approval was recorded leaves
// the approval STALE, and staleness is observable rather than implied.
//
// THE APPROVAL IS A FENCED WRITE, like every other state change here.
// The operator presents the claim epoch it read, and the write is
// `WHERE id = ? AND claim_epoch = ?` with the PRESENTED epoch. An
// approval therefore lands only on the revision of the assignment the
// operator actually read: if the assignment moved under the operator
// between the read and the write, the approval does not land and the
// operator must read it again.
//
// This is the same rule the rest of the package follows, and it is the
// one place where following it mattered most: the approval is the gate.
func (s *Store) RecordApproval(auth OperatorAuthority, p ApprovalParams) (*Assignment, error) {
	if auth == nil {
		return nil, fmt.Errorf("%w: recording an approval is an operator operation", ErrNotAuthorised)
	}
	actor := auth.Actor()

	if err := requireText("action", p.Action); err != nil {
		return nil, err
	}
	if err := requireText("revision", p.Revision); err != nil {
		return nil, err
	}
	if err := requireText("source", p.Source); err != nil {
		return nil, err
	}
	if err := requireText("evidence", p.Evidence); err != nil {
		return nil, err
	}

	var out *Assignment
	err := s.db.WithTx(func(node internal.DBNode) error {
		current, err := getAssignmentTx(node, p.ID)
		if err != nil {
			return err
		}
		if current.State != StateAwaitingOperator {
			return fmt.Errorf("%w: %s is %s and is not at the operator's gate",
				ErrIllegalTransition, p.ID, current.State)
		}
		// The fence. The operator presents the epoch it read; a
		// mismatch means the assignment moved under it, so the
		// approval was made against something else.
		if p.Epoch != current.ClaimEpoch {
			return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, p.ID, current.ClaimEpoch)
		}
		if err := checkTransition(current.Role, current.State, StateOperatorApproved); err != nil {
			return err
		}
		if current.AwaitingAction != p.Action {
			return fmt.Errorf(
				"%w: the gate awaits %q and the approval names %q",
				ErrConstraint, current.AwaitingAction, p.Action)
		}
		now := s.Now()

		if err := s.injectFault("approval"); err != nil {
			return err
		}
		res, err := node.ExecTracked(`
			UPDATE agent_tasks
			SET state = 'operator_approved', updated_at = ?,
			    approval_action = ?, approval_revision = ?, approval_source = ?,
			    approval_evidence = ?, approved_at = ?
			WHERE id = ? AND claim_epoch = ?`,
			5, now, p.Action, p.Revision, p.Source, p.Evidence, now,
			p.ID, p.Epoch)
		if err != nil {
			return storageRefusal(err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("assignment: read the affected rows: %w", err)
		}
		if affected != 1 {
			return fmt.Errorf("%w: %s moved while the write was in flight", ErrStaleClaim, p.ID)
		}

		if err := s.injectFault("approval-event"); err != nil {
			return err
		}
		if _, err := appendEvent(node, p.ID, EventOperatorApproved, current.State,
			StateOperatorApproved, actor, nil, p.Evidence,
			map[string]interface{}{
				"action":   p.Action,
				"revision": p.Revision,
				"source":   p.Source,
				"evidence": p.Evidence,
			}, now); err != nil {
			return err
		}

		row, err := getAssignmentTx(node, p.ID)
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
