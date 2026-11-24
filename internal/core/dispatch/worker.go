package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/flowbyte-com/mpm-core/assignment"
	oc "github.com/flowbyte-com/mpm-core/opencode"
)

// The two dispatch paths.
//
// They are SEPARATE on purpose. An implementer runs in the worker's
// containment with the worker's agent definition; a reviewer runs in the
// reviewer's own sandbox with the pinned `mpm-reviewer` definition. The
// reviewer NEVER runs in the worker's context, and the implementer never
// gets to record a review's outcome.

// dispatchWorker hands an implementer to PR 3's adapter.
//
// The adapter does the work: it reconciles, it mints the session id, it
// persists the intent, it launches, and it records what the runtime said.
// The dispatcher adds no execution path of its own.
func (e *Engine) dispatchWorker(ctx context.Context, claimed *assignment.Assignment,
	worker assignment.Runtime, workspace string, outcome Outcome) (Outcome, error) {

	if worker == nil {
		outcome.Reason = ReasonRuntimeUnavailable
		outcome.Detail = "the worker runtime was not built"
		return outcome, nil
	}

	res, err := e.store.Execute(ctx, worker, assignment.ExecuteParams{
		ID:          claimed.ID,
		Epoch:       claimed.ClaimEpoch,
		Actor:       assignment.Actor{Address: claimed.Recipient, Kind: "agent"},
		Agent:       e.cfg.WorkerAgent,
		Workspace:   workspace,
		WorkspaceID: claimed.ID,
		Model:       e.cfg.Model,
		Title:       "assignment " + claimed.ID,
	})
	if err != nil {
		outcome.Reason = classifyAdapterRefusal(err)
		outcome.Detail = err.Error()
		return outcome, nil
	}
	if res.Attempt != nil {
		outcome.AttemptID = res.Attempt.ID
		outcome.NativeSessionID = res.Attempt.NativeSessionID
		outcome.AssignmentState = res.Attempt.LaunchState
	}
	outcome.RuntimeOutcome = res.RuntimeOutcome
	if res.Assignment != nil {
		outcome.AssignmentState = res.Assignment.State
	}
	outcome.Reason = completionReason(res.LaunchState, res.RuntimeOutcome, res.Assignment)
	return outcome, nil
}

// dispatchReview hands a reviewer to PR 3's review path.
//
// The review path is the ONLY way a reviewer runs. It keeps the pinned
// definition, the default-deny permissions, the `sandbox_exec` boundary,
// the exact-revision snapshot, the host-captured findings, and the
// separation between the reviewer's EVIDENCE, the reviewer's VERDICT and
// the operator's APPROVAL.
func (e *Engine) dispatchReview(ctx context.Context, claimed *assignment.Assignment,
	reviewer assignment.ReviewRuntime, outcome Outcome) (Outcome, error) {

	if reviewer == nil {
		outcome.Reason = ReasonReviewerUnavailable
		outcome.Detail = "the reviewer runtime was not built"
		return outcome, nil
	}

	// THE EXACT REVISION IS PART OF THE AUTHORISATION. Without it the
	// review runtime would accept whatever the review checkout happens
	// to contain and the verdict would bind to that HEAD.
	expected := claimed.HeadSHA
	if expected == "" {
		if targetID, _ := claimed.Payload[assignment.PayloadKeyReviewTarget].(string); targetID != "" {
			if subject, err := e.store.Get(targetID); err == nil {
				expected = subject.HeadSHA
			}
		}
	}
	if expected == "" {
		outcome.Reason = ReasonReviewerUnavailable
		outcome.Detail = "the review names no exact revision to check; set the reviewer's head SHA"
		return outcome, nil
	}
	res, err := e.store.ExecuteReview(ctx, reviewer, assignment.ExecuteReviewParams{
		ID:               claimed.ID,
		Epoch:            claimed.ClaimEpoch,
		Actor:            assignment.Actor{Address: claimed.Recipient, Kind: "agent"},
		Brief:            e.briefFor(claimed),
		Title:            "review of " + claimed.ID,
		ExpectedRevision: expected,
		// The model the factory configured is the model the review was
		// authorised to use. A result from another one is refused.
		ExpectedModel: e.cfg.ReviewerModel,
	})
	if err != nil {
		outcome.Reason = classifyReviewRefusal(err)
		outcome.Detail = err.Error()
		return outcome, nil
	}
	if res != nil {
		outcome.NativeSessionID = res.SessionID
	}
	if res.Assignment != nil {
		outcome.AssignmentState = res.Assignment.State
	}
	outcome.RuntimeOutcome = res.RuntimeOutcome
	outcome.ReviewedRevision = res.ReviewedRevision
	outcome.FindingsSHA256 = res.FindingsSHA256
	outcome.Reason = ReasonCompleted
	return outcome, nil
}

// briefFor writes the reviewer's brief.
//
// It is BOUNDED and it names the subject and the revision, and nothing
// else: a brief is not a place for a transcript, and the dispatcher's
// brief must not become a channel for the implementer's content.
func (e *Engine) briefFor(a *assignment.Assignment) string {
	subject, _ := a.Payload[assignment.PayloadKeyReviewTarget].(string)
	brief := fmt.Sprintf(
		"Review the work of assignment %s independently. Report what is wrong with it, "+
			"with file and line references. End with exactly one final line, without "+
			"backticks: VERDICT: APPROVED or VERDICT: CHANGES REQUESTED.", subject)
	if len(brief) > maxBriefBytes {
		brief = brief[:maxBriefBytes]
	}
	return brief
}

// maxBriefBytes bounds a review brief.
const maxBriefBytes = 2000

// workspaceFor is the authorised worktree of one assignment.
//
// The dispatcher does not CHOOSE a workspace: the operator configures the
// root it lives under, and the worktree is the assignment's own directory
// inside it. That is what makes two runs of the same assignment find the
// same tree — a retry must not find a different one.
//
// The provisioner is HOST-side: it creates the Git worktree from the
// assignment's base revision and verifies an existing one before reuse.
// The worker only ever sees the checked-out files.
func (e *Engine) workspaceFor(a *assignment.Assignment) (string, error) {
	if e.provisioner == nil {
		return "", fmt.Errorf("dispatch: no workspace provisioner is configured")
	}
	return e.provisioner.Ensure(a)
}

// workspaceForExisting is the recovery path's workspace: it verifies one
// that already exists and NEVER creates one.
//
// An outstanding attempt implies a launch, and a launch implies the
// worktree existed before the session started. A missing worktree is a
// fact a human must explain; creating one while a lease or an older
// session may still be live would be a second writer's workspace.
func (e *Engine) workspaceForExisting(a *assignment.Assignment) (string, error) {
	if e.provisioner == nil {
		return "", fmt.Errorf("dispatch: no workspace provisioner is configured")
	}
	return e.provisioner.Reuse(a)
}

// classifyAdapterRefusal maps an adapter refusal onto a reason code.
func classifyAdapterRefusal(err error) string {
	switch {
	case errors.Is(err, assignment.ErrBudgetExhausted):
		return ReasonBudgetExhausted
	case errors.Is(err, assignment.ErrStaleClaim):
		return ReasonStaleEpoch
	case errors.Is(err, assignment.ErrLeaseHeld), errors.Is(err, assignment.ErrAlreadyClaimed):
		return ReasonLeaseHeld
	case errors.Is(err, oc.ErrReviewerUnavailable):
		return ReasonReviewerUnavailable
	case errors.Is(err, assignment.ErrReviewVerdict):
		return ReasonRecoveryRequired
	case errors.Is(err, assignment.ErrIllegalTransition):
		return ReasonOperatorGatePending
	case errors.Is(err, assignment.ErrRecordOnlyRefused):
		// The adapter could not record an observed outcome, and the
		// caller asked for no launch. The attempt stays OUTSTANDING and
		// the work is not lost: a later pass may dispatch it normally.
		// So it is a DEFERRAL — "nothing was launched, and the work will
		// be considered again" — and not a refusal.
		return ReasonDeferred
	default:
		return ReasonRefused
	}
}

// classifyReviewRefusal maps a review refusal onto a reason code.
func classifyReviewRefusal(err error) string {
	switch {
	// RECOVERY IS CHECKED FIRST. A post-inference failure can also wrap
	// ErrStaleClaim (the reviewer's epoch moved during the model run) or
	// ErrConstraint (the transaction failed). The attempt is already
	// AMBIGUOUS, so reporting "stale epoch" or "refused" would invite a
	// retry the ambiguity check will refuse.
	case errors.Is(err, oc.ErrReviewIncomplete),
		errors.Is(err, assignment.ErrReviewAmbiguous),
		errors.Is(err, assignment.ErrReviewVerdict):
		return ReasonRecoveryRequired
	case errors.Is(err, assignment.ErrBudgetExhausted):
		return ReasonBudgetExhausted
	case errors.Is(err, assignment.ErrStaleClaim):
		return ReasonStaleEpoch
	case errors.Is(err, oc.ErrReviewerUnavailable):
		return ReasonReviewerUnavailable
	case errors.Is(err, assignment.ErrRecordOnlyRefused):
		// The adapter could not record an observed outcome, and the
		// caller asked for no launch. The attempt stays OUTSTANDING and
		// the work is not lost: a later pass may dispatch it normally.
		// So it is a DEFERRAL — "nothing was launched, and the work will
		// be considered again" — and not a refusal.
		return ReasonDeferred
	case errors.Is(err, assignment.ErrConstraint):
		// A refused review is most often a refusal to launch a SECOND
		// reviewer, which is a duplicate, not a malformed request.
		return ReasonRefused
	case errors.Is(err, assignment.ErrIllegalTransition):
		return ReasonOperatorGatePending
	default:
		return ReasonRefused
	}
}

// completionReason says how a dispatch ended.
//
// RUNTIME SUCCESS, ASSIGNMENT COMPLETION, REVIEW EVIDENCE, REVIEW VERDICT
// and OPERATOR APPROVAL are five different facts. This function reports
// what the DISPATCH observed and never says more than it saw.
func completionReason(launchState, runtimeOutcome string, a *assignment.Assignment) string {
	switch launchState {
	case assignment.SessionAmbiguous:
		return ReasonAmbiguous
	case assignment.SessionCompleted:
		if a != nil && a.State == assignment.StateCompleted {
			return ReasonCompleted
		}
		// The runtime reported a terminal outcome but the assignment did
		// not complete: the two facts are separate, and the report says
		// the runtime succeeded without claiming the assignment finished.
		return ReasonDispatched
	case assignment.SessionFailed:
		return ReasonFailed
	default:
		return ReasonDispatched
	}
}
