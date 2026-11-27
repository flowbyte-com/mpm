package dispatch

import (
	"fmt"

	"github.com/flowbyte-com/mpm-core/assignment"
)

// The authorisation checks.
//
// # WHAT AUTHORISATION MEANS HERE
//
// The OPERATOR creates a root and states what it permits: the operations
// the tree may perform and the recipients it may address. Every dispatch
// is justified by that statement, and the dispatcher checks it BEFORE it
// takes a claim — because a claim on work that cannot be authorised is a
// lease spent on nothing.
//
// # THE CHILD INHERITS
//
// A child carries no list of its own: the schema says so, and the root's
// list is the bound. So the check reads the ROOT, exactly as PR 3's
// adapter does. The check is not duplicated for the child — it is the
// same list.

// authorisationReason says why the tree does not authorise this
// dispatch, or "" when it does.
func (e *Engine) authorisationReason(a *assignment.Assignment) (string, string) {
	root, err := e.store.Get(a.RootID)
	if err != nil {
		return ReasonRefused, "the root could not be read: " + err.Error()
	}

	// THE RECIPIENT MUST BE ONE THE ROOT ALLOWS.
	//
	// The delegation already refuses a recipient the root does not
	// list, so this is a second, independent check: the dispatcher's
	// decision to hand work to somebody is its own, and it does not
	// rely on a check that happened somewhere else.
	if len(root.EligibleRecipients) > 0 && !containsString(root.EligibleRecipients, a.Recipient) {
		return ReasonRefused, fmt.Sprintf(
			"the root does not list %q among its eligible recipients", a.Recipient)
	}

	// THE OPERATIONS THE DISPATCH PERFORMS MUST BE PERMITTED.
	//
	// The dispatcher claims; the adapter starts and completes. A tree
	// that permits none of them cannot be executed, and saying so
	// before the claim is cheaper and clearer than saying it after.
	for _, op := range []string{"claim", "start", "complete"} {
		if !containsString(root.PermittedOperations, op) {
			return ReasonRefused, fmt.Sprintf(
				"the root does not permit the %q operation", op)
		}
	}
	return "", ""
}

// dependencyReason says why this assignment's own DEPENDENCIES are not
// met, or "" when they are.
//
// # WHAT A DEPENDENCY IS HERE
//
// A reviewer depends on the work it reviews: the delegation that created
// it fixed the target, and a reviewer with no target has nothing to do.
// An implementer depends on its TREE: a child whose root has been
// cancelled is work nobody asked for any more.
//
// The dependency is checked BEFORE the claim, so a dispatch that cannot
// run never takes a lease.
func (e *Engine) dependencyReason(a *assignment.Assignment) (string, string) {
	switch a.Role {
	case assignment.RoleReviewer:
		target, _ := a.Payload[assignment.PayloadKeyReviewTarget].(string)
		if target == "" {
			return ReasonRefused, "the review names no target, so there is nothing to review"
		}
		subject, err := e.store.Get(target)
		if err != nil {
			return ReasonRefused, "the review target could not be read: " + err.Error()
		}
		if subject.Role != assignment.RoleImplementer {
			return ReasonRefused, fmt.Sprintf(
				"the review target %s is a %s, and only an implementer's work is reviewable",
				subject.ID, subject.Role)
		}
		// T5: no early review at dispatch either. A reviewer whose
		// subject has not completed would spend a runtime session only
		// to fail at verdict time.
		if subject.State != assignment.StateCompleted {
			return ReasonRefused, fmt.Sprintf(
				"the review target %s is %s and has not completed the work",
				subject.ID, subject.State)
		}
		return "", ""

	case assignment.RoleImplementer:
		// THE TREE MUST STILL BE ALIVE. A cancelled root means the
		// operator withdrew the whole work order.
		root, err := e.store.Get(a.RootID)
		if err != nil {
			return ReasonRefused, "the root could not be read: " + err.Error()
		}
		if root.State == assignment.StateCancelled {
			return ReasonRefused, "the root was cancelled, so its work was withdrawn"
		}
		return "", ""

	default:
		return ReasonRefused, "the assignment has no execution path"
	}
}

// containsString reports whether the list holds the value.
func containsString(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}
