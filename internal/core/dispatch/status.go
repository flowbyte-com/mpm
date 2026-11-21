package dispatch

import (
	"fmt"

	"github.com/flowbyte-com/mpm-core/assignment"
)

// The operator's view of the dispatcher's work.
//
// # WHY A STATUS AND NOT A REPORT
//
// A dispatch REPORT says what one run did. A STATUS says what the
// dispatcher COULD do, without doing it: what is pending, what is
// blocked, and what needs an operator.
//
// The status is a READ. It builds no runtime, creates no session and
// writes nothing, so it is safe to run at any time — including when the
// dispatcher is disabled, which is exactly when an operator most wants
// to see what enabling it would do.

// StatusItem is one assignment in the status.
type StatusItem struct {
	// AssignmentID is the assignment.
	AssignmentID string `json:"assignment_id"`
	// RootID is the tree it belongs to.
	RootID string `json:"root_id"`
	// Role is implementer or reviewer.
	Role string `json:"role"`
	// Recipient is who the work is addressed to.
	Recipient string `json:"recipient"`
	// Title is the assignment's title.
	Title string `json:"title"`
	// State is the assignment's state.
	State string `json:"state"`
	// Owner is the lease holder, when there is one.
	Owner string `json:"owner,omitempty"`
	// Epoch is the current claim epoch.
	Epoch int64 `json:"epoch,omitempty"`
	// LeaseExpiresAt is the lease's end, in unix seconds.
	LeaseExpiresAt int64 `json:"lease_expires_at,omitempty"`
	// Attempts is the number of attempts the assignment has consumed.
	Attempts int `json:"attempts"`
	// SessionID is the native session of an outstanding attempt.
	SessionID string `json:"session_id,omitempty"`
	// Reason is the closed-set code that puts the item in its bucket.
	Reason string `json:"reason,omitempty"`
	// Detail is the bounded human explanation.
	Detail string `json:"detail,omitempty"`
}

// StatusReport is the operator's view.
type StatusReport struct {
	// Enabled says whether the dispatcher would run at all.
	Enabled bool `json:"enabled"`
	// Pending is the work the dispatcher could pick up.
	Pending []StatusItem `json:"pending"`
	// Blocked is the work it cannot pick up, and why.
	Blocked []StatusItem `json:"blocked"`
	// Recoverable is the interrupted work an operator has to look at.
	Recoverable []StatusItem `json:"recoverable"`
	// Budgets is the budget picture of every tree involved.
	Budgets []assignment.BudgetStatus `json:"budgets"`
	// GeneratedAt is the moment the status was taken, in unix seconds.
	GeneratedAt int64 `json:"generated_at"`
}

// Status reads the dispatcher's view of the work.
//
// It takes the configuration rather than an engine, because the status
// must work when the dispatcher is DISABLED — that is the state an
// operator is in before they enable it.
func Status(store *assignment.Store, cfg Config) (*StatusReport, error) {
	now := store.Now()
	out := &StatusReport{
		Enabled:     cfg.Enabled,
		Pending:     []StatusItem{},
		Blocked:     []StatusItem{},
		Recoverable: []StatusItem{},
		Budgets:     []assignment.BudgetStatus{},
		GeneratedAt: now,
	}

	// THE INTERRUPTED WORK FIRST. An outstanding attempt is the thing an
	// operator has to look at, and it also BLOCKS the assignment it
	// belongs to.
	outstanding, err := store.OutstandingAttempts()
	if err != nil {
		return nil, err
	}
	blockedByAttempt := map[string]*assignment.OutstandingAttempt{}
	for _, o := range outstanding {
		if o.Assignment == nil {
			continue
		}
		item := statusItem(o.Assignment)
		item.Attempts = attemptCount(o)
		item.SessionID = o.Attempt.NativeSessionID
		item.Reason = ReasonRecoveryRequired
		item.Detail = fmt.Sprintf(
			"attempt %s is %s at epoch %d; its effects cannot be determined here",
			o.Attempt.ID, o.Attempt.LaunchState, o.Attempt.ClaimEpoch)
		out.Recoverable = append(out.Recoverable, item)
		blockedByAttempt[o.Assignment.ID] = o
	}

	candidates, err := store.DispatchCandidates(assignment.DispatchQuery{
		States: []string{assignment.StateAccepted, assignment.StateInProgress},
		Limit:  statusCandidateLimit,
	})
	if err != nil {
		return nil, err
	}

	roots := map[string]bool{}
	for _, c := range candidates {
		roots[c.RootID] = true
		item := statusItem(c)
		attempts, err := store.AttemptsFor(c.ID)
		if err != nil {
			return nil, err
		}
		item.Attempts = len(attempts)

		switch {
		case blockedByAttempt[c.ID] != nil:
			item.Reason = ReasonSessionStillRunning
			item.Detail = "an earlier attempt is outstanding, so a second writer cannot be ruled out"
			out.Blocked = append(out.Blocked, item)

		case liveLeaseHeldByOther(c, cfg.Owner, now):
			item.Reason = ReasonLeaseHeld
			item.Detail = "another holder holds a live lease on this assignment"
			out.Blocked = append(out.Blocked, item)

		default:
			item.Reason = ReasonNoEligible
			item.Detail = "the assignment is eligible for dispatch"
			out.Pending = append(out.Pending, item)
		}
	}

	for rootID := range roots {
		b, err := store.BudgetStatus(rootID)
		if err != nil {
			return nil, err
		}
		out.Budgets = append(out.Budgets, b)
	}
	return out, nil
}

// statusCandidateLimit bounds the status read.
//
// The status is a report, not an exhaustive dump: a tree with a thousand
// assignments shows its first thousand, which is enough to see whether
// the dispatcher has anything to do.
const statusCandidateLimit = 200

// statusItem projects an assignment into the status shape.
func statusItem(a *assignment.Assignment) StatusItem {
	item := StatusItem{
		AssignmentID: a.ID,
		RootID:       a.RootID,
		Role:         a.Role,
		Recipient:    a.Recipient,
		Title:        a.Title,
		State:        a.State,
		Owner:        a.Owner,
		Epoch:        a.ClaimEpoch,
	}
	if a.LeaseExpiresAt != nil {
		item.LeaseExpiresAt = *a.LeaseExpiresAt
	}
	return item
}

// attemptCount is the number of attempts the assignment has consumed.
//
// The attempt records are numbered from one, so the outstanding
// attempt's own number IS the count — and it is what tells an operator
// whether this is the first interruption or the tenth.
func attemptCount(o *assignment.OutstandingAttempt) int {
	if o.Attempt.Attempt < 1 {
		return 1
	}
	return int(o.Attempt.Attempt)
}

// liveLeaseHeldByOther says the assignment is leased to somebody who is
// not this dispatcher.
//
// A lease the DISPATCHER holds is not a blocker: the dispatcher can
// resume its own work with the epoch it already holds. A lease held by
// anybody else is a blocker, and the dispatcher never takes it.
func liveLeaseHeldByOther(a *assignment.Assignment, owner string, now int64) bool {
	if a.ClaimEpoch == 0 || a.LeaseExpiresAt == nil {
		return false
	}
	if *a.LeaseExpiresAt <= now {
		return false
	}
	return a.Owner != owner
}
