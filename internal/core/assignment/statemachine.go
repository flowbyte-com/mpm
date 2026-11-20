package assignment

import "fmt"

// ── The role-aware path ───────────────────────────────────────────────────
//
// The schema states the path in one trigger, and the schema is the
// backstop: a write that is not on the path is refused by the database
// whatever the API does.
//
// But a refusal that arrives as "refused by the storage: agent_tasks:
// illegal state transition for this role" is a poor answer to the
// question the caller actually asked — "may I do this?" — and it is a
// poor answer to a REVIEWER, who has to read a diff to find out that
// the API never checked.
//
// So the service states the path too, and states it BEFORE the write.
// The two statements are the same statement: the API refuses the
// illegal transition with a precise error, and the schema refuses it
// independently. The service's check is not the schema's replacement —
// it is the schema's first reader.
//
// The paths are:
//
//	implementer: enqueued → delivered → acknowledged → accepted →
//	            in_progress → completed → review_passed →
//	            awaiting_operator → operator_approved
//	reviewer:    enqueued → delivered → acknowledged → accepted →
//	            in_progress → completed | review_passed
//
// and 'cancelled' is the UNIVERSAL withdrawal: legal from every state
// except itself, for every role.

// implementerPath is the implementer's path: a chain, one step forward.
var implementerPath = map[string][]string{
	StateEnqueued:         {StateDelivered},
	StateDelivered:        {StateAcknowledged},
	StateAcknowledged:     {StateAccepted},
	StateAccepted:         {StateInProgress},
	StateInProgress:       {StateCompleted},
	StateCompleted:        {StateReviewPassed},
	StateReviewPassed:     {StateAwaitingOperator},
	StateAwaitingOperator: {StateOperatorApproved},
}

// reviewerPath is the reviewer's path: the same chain, BRANCHING at the
// end. A reviewer's assignment ends at 'completed' when the review asked
// for changes, and at 'review_passed' when it passed.
var reviewerPath = map[string][]string{
	StateEnqueued:     {StateDelivered},
	StateDelivered:    {StateAcknowledged},
	StateAcknowledged: {StateAccepted},
	StateAccepted:     {StateInProgress},
	StateInProgress:   {StateCompleted, StateReviewPassed},
}

// legalTransition reports whether an assignment of the given role may
// move from one state to another.
func legalTransition(role, from, to string) bool {
	// The universal withdrawal.
	if to == StateCancelled {
		return from != StateCancelled
	}
	var path map[string][]string
	switch role {
	case RoleImplementer:
		path = implementerPath
	case RoleReviewer:
		path = reviewerPath
	default:
		// An unknown role has no path, so nothing is legal. The
		// schema refuses the row outright; this keeps the service
		// from being the more permissive of the two.
		return false
	}
	for _, next := range path[from] {
		if next == to {
			return true
		}
	}
	return false
}

// checkTransition refuses an illegal transition with the API's own
// vocabulary.
//
// The message names the role, the state it is in and the state it was
// asked to reach, because "illegal state transition" alone does not tell
// a caller what the legal next step is.
func checkTransition(role, from, to string) error {
	if legalTransition(role, from, to) {
		return nil
	}
	if to == StateCancelled {
		return fmt.Errorf("%w: %s is already %s and cannot be cancelled again",
			ErrIllegalTransition, from, to)
	}
	return fmt.Errorf("%w: a %s cannot go from %s to %s",
		ErrIllegalTransition, role, from, to)
}
