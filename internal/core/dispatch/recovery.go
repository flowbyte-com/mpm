package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/flowbyte-com/mpm-core/assignment"
	oc "github.com/flowbyte-com/mpm-core/opencode"
)

// The recovery pass.
//
// # WHAT RECOVERY IS
//
// A dispatcher run can be interrupted: the process dies between the claim
// and the launch, between the launch and the record, or during the run.
// The next run has to find that work and deal with it BEFORE it starts
// anything new — otherwise the interruption is silently duplicated.
//
// # THE ORDER OF THE QUESTIONS
//
//  1. What attempt is outstanding, and which session did it record?
//  2. Is the assignment's claim epoch still the attempt's epoch?
//  3. What does the runtime say about that session — running, terminal,
//     absent, or unreachable?
//  4. What can be done through the EXISTING fenced paths?
//
// # WHAT IS NEVER DONE
//
//   - A live lease is never reclaimed. PR 2's Claim refuses a live lease
//     and PR 4 does not work around it.
//   - An outcome is never INVENTED. A runtime that cannot be asked is
//     AMBIGUOUS, and an ambiguous attempt stays ambiguous until a human
//     or a later observation resolves it.
//   - A session is never "stopped". There is no verified host operation
//     that safely ends an OpenCode session, and `session.abort` is not
//     one MPM may depend on. So a session that may still be working is a
//     BLOCKER, and the dispatcher says so rather than pretending.
//
// # THE DRAIN
//
// The one thing a dispatcher CAN do about a running session is WAIT for it
// to report a terminal outcome, and then reconcile that outcome through
// the existing fenced path. That is the drain: bounded, observable, and
// it ends either in a reconciled record or in a refusal.

// recover reconciles the attempts a previous run left behind.
//
// It returns one outcome per outstanding attempt it had something to say
// about. An attempt it recovered silently is not reported — the normal
// dispatch path will pick the assignment up.
func (e *Engine) recover(ctx context.Context) ([]Outcome, error) {
	outstanding, err := e.store.OutstandingAttempts()
	if err != nil {
		return nil, err
	}
	if len(outstanding) == 0 {
		return nil, nil
	}

	out := []Outcome{}
	for _, o := range outstanding {
		if o.Assignment == nil {
			continue
		}
		outcome := Outcome{
			AssignmentID:    o.Assignment.ID,
			RootID:          o.Assignment.RootID,
			Role:            o.Assignment.Role,
			AttemptID:       o.Attempt.ID,
			NativeSessionID: o.Attempt.NativeSessionID,
			Epoch:           o.Attempt.ClaimEpoch,
		}

		// AN OLDER EPOCH IS A BLOCKER.
		//
		// The attempt's writes are fenced on the epoch, so an attempt
		// from an older claim can never record its own result, and the
		// session it started may still be working. PR 3's own reconciler
		// refuses this case; the dispatcher surfaces it rather than
		// working around it.
		if o.Attempt.ClaimEpoch != o.Assignment.ClaimEpoch {
			outcome.Reason = ReasonSessionStillRunning
			outcome.Detail = fmt.Sprintf(
				"the attempt is at epoch %d and the assignment is at epoch %d, so the "+
					"session may still be working and its effects cannot be fenced from "+
					"here", o.Attempt.ClaimEpoch, o.Assignment.ClaimEpoch)
			if err := e.noteRecovery(o, outcome); err != nil {
				return out, err
			}
			out = append(out, outcome)
			continue
		}

		// WHAT DOES THE RUNTIME SAY?
		state, reason, detail := e.inspect(ctx, o.Assignment, o.Attempt.NativeSessionID)
		if reason != "" {
			outcome.Reason = reason
			outcome.Detail = detail
			if reason == ReasonRecoveryRequired {
				if err := e.noteRecovery(o, outcome); err != nil {
					return out, err
				}
			}
			out = append(out, outcome)
			continue
		}

		switch {
		case state == nil:
			// THE RUNTIME HAS NEVER HEARD OF IT. Nothing was created, so
			// reusing the recorded id cannot duplicate anything. The
			// normal dispatch path resumes it.
			outcome.Reason = ReasonDeferred
			outcome.Detail = "the runtime has no record of the session, so the recorded " +
				"id can be reused safely; the assignment will be dispatched normally"
			out = append(out, outcome)

		case state.Outcome != "":
			// A TERMINAL OUTCOME. It is reconciled through the existing
			// fenced path — PR 3's adapter, which records what the
			// runtime said and nothing else.
			outcome.RuntimeOutcome = state.Outcome
			reconciled, rerr := e.reconcile(ctx, o.Assignment, outcome)
			if rerr != nil {
				return out, rerr
			}
			out = append(out, reconciled)

		default:
			// STILL RUNNING. The drain: wait, bounded, for the session to
			// report a terminal outcome.
			outcome.Reason = ReasonSessionStillRunning
			outcome.Detail = "the recorded session is still running"
			drained, derr := e.drain(ctx, o, outcome)
			if derr != nil {
				return out, derr
			}
			out = append(out, drained)
		}
	}
	return out, nil
}

// inspect asks the runtime what it knows about a session.
//
// It returns the state, or a reason code when the runtime could not be
// asked. The three answers are kept apart, because they mean different
// things: ABSENT is safe, UNREACHABLE is ambiguous, and RUNNING is a
// blocker.
func (e *Engine) inspect(ctx context.Context, a *assignment.Assignment,
	sessionID string) (*oc.SessionState, string, string) {

	worker, reviewer, err := e.factory(ctx, a, "")
	if err != nil {
		return nil, ReasonRuntimeUnavailable, err.Error()
	}
	var inspector interface {
		Inspect(ctx context.Context, sessionID string) (*oc.SessionState, error)
	}
	if a.Role == assignment.RoleReviewer {
		if reviewer == nil {
			return nil, ReasonReviewerUnavailable, "the reviewer runtime was not built"
		}
		inspector = reviewer
	} else {
		if worker == nil {
			return nil, ReasonRuntimeUnavailable, "the worker runtime was not built"
		}
		inspector = worker
	}
	if sessionID == "" {
		return nil, ReasonRecoveryRequired, "the attempt recorded no session id, so there is nothing to inspect"
	}
	state, err := inspector.Inspect(ctx, sessionID)
	if err != nil {
		if errors.Is(err, oc.ErrSessionNotFound) {
			return nil, "", ""
		}
		return nil, ReasonRuntimeUnavailable, "the runtime could not be asked about the session: " + err.Error()
	}
	return state, "", ""
}

// reconcile records an observed terminal outcome through the existing
// fenced path.
//
// The dispatcher does NOT write the outcome itself. PR 3's adapter is the
// only thing that records a runtime's report, and it does so with the
// attempt's epoch fence. The dispatcher supplies the runtime and lets the
// adapter do what it already does.
func (e *Engine) reconcile(ctx context.Context, a *assignment.Assignment,
	outcome Outcome) (Outcome, error) {

	workspace, werr := e.workspaceFor(a)
	if werr != nil {
		outcome.Reason = ReasonRuntimeUnavailable
		outcome.Detail = werr.Error()
		return outcome, nil
	}
	worker, reviewer, err := e.factory(ctx, a, workspace)
	if err != nil {
		outcome.Reason = ReasonRuntimeUnavailable
		outcome.Detail = err.Error()
		return outcome, nil
	}
	if a.Role == assignment.RoleReviewer {
		if reviewer == nil {
			outcome.Reason = ReasonReviewerUnavailable
			outcome.Detail = "the reviewer runtime was not built"
			return outcome, nil
		}
		// The review path reconciles through its own execution, which
		// refuses to run a SECOND reviewer over work that may already
		// have been reviewed.
		outcome.Reason = ReasonRecoveryRequired
		outcome.Detail = "the review session reported a terminal outcome; the review " +
			"path owns the recording of it, and it refuses to run a second reviewer " +
			"while an attempt is outstanding"
		return outcome, nil
	}
	if worker == nil {
		outcome.Reason = ReasonRuntimeUnavailable
		outcome.Detail = "the worker runtime was not built"
		return outcome, nil
	}
	res, err := e.store.Execute(ctx, worker, assignment.ExecuteParams{
		ID:          a.ID,
		Epoch:       a.ClaimEpoch,
		Actor:       assignment.Actor{Address: a.Recipient, Kind: "agent"},
		Agent:       e.cfg.WorkerAgent,
		Workspace:   workspace,
		WorkspaceID: a.ID,
		Model:       e.cfg.Model,
		Title:       "assignment " + a.ID,
	})
	if err != nil {
		outcome.Reason = classifyAdapterRefusal(err)
		outcome.Detail = err.Error()
		return outcome, nil
	}
	outcome.RuntimeOutcome = res.RuntimeOutcome
	if res.Attempt != nil {
		outcome.AttemptID = res.Attempt.ID
		outcome.NativeSessionID = res.Attempt.NativeSessionID
	}
	if res.Assignment != nil {
		outcome.AssignmentState = res.Assignment.State
	}
	outcome.Reason = completionReason(res.LaunchState, res.RuntimeOutcome, res.Assignment)
	return outcome, nil
}

// drain waits, bounded, for a running session to report a terminal
// outcome.
//
// This is the narrowest provable drain: it observes and it waits. It does
// not stop the session, because there is no verified host operation that
// safely does that, and it does not assume the session will finish. When
// the window closes, the attempt is reported as needing an operator.
func (e *Engine) drain(ctx context.Context, o *assignment.OutstandingAttempt,
	outcome Outcome) (Outcome, error) {

	if e.cfg.DrainTimeoutSeconds <= 0 || e.cfg.DrainPollIntervalSeconds <= 0 {
		if err := e.noteRecovery(o, outcome); err != nil {
			return outcome, err
		}
		outcome.Reason = ReasonRecoveryRequired
		outcome.Detail = "the recorded session is still running and no drain window is " +
			"configured, so the operator has to deal with it"
		return outcome, nil
	}

	deadline := e.now().Add(time.Duration(e.cfg.DrainTimeoutSeconds) * time.Second)
	for {
		state, reason, detail := e.inspect(ctx, o.Assignment, o.Attempt.NativeSessionID)
		if reason != "" {
			outcome.Reason = reason
			outcome.Detail = detail
			return outcome, nil
		}
		if state != nil && state.Outcome != "" {
			outcome.RuntimeOutcome = state.Outcome
			return e.reconcile(ctx, o.Assignment, outcome)
		}
		if !e.now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			outcome.Reason = ReasonRecoveryRequired
			outcome.Detail = "the drain was interrupted before the session reported an outcome"
			return outcome, nil
		case <-time.After(time.Duration(e.cfg.DrainPollIntervalSeconds) * time.Second):
		}
	}

	outcome.Reason = ReasonRecoveryRequired
	outcome.Detail = fmt.Sprintf(
		"the recorded session was still running after a %ds drain window, so its effects "+
			"are unknown and it blocks another writer into the same workspace",
		e.cfg.DrainTimeoutSeconds)
	if err := e.noteRecovery(o, outcome); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// noteRecovery records the recovery state where an operator will see it.
//
// The assignment's ledger is durable and the operator reads it, so a note
// is where a state that has no assignment-level transition goes. The
// assignment is NOT moved: `in_progress` cannot reach `awaiting_operator`
// on the role paths, and inventing a transition to carry this would be a
// change to PR 1's contract.
//
// The note carries the session id and the reason, and NOTHING else: no
// transcript, no prompt, no credential.
func (e *Engine) noteRecovery(o *assignment.OutstandingAttempt, outcome Outcome) error {
	note := fmt.Sprintf(
		"recovery required: attempt %s recorded session %s at epoch %d — %s",
		o.Attempt.ID, o.Attempt.NativeSessionID, o.Attempt.ClaimEpoch, outcome.Reason)
	_, err := e.store.Note(assignment.NoteParams{
		ID:    o.Assignment.ID,
		Actor: assignment.Actor{Address: e.cfg.Owner, Kind: "agent"},
		Note:  note,
		Payload: map[string]interface{}{
			"recovery_required": true,
			"attempt_id":        o.Attempt.ID,
			"native_session_id": o.Attempt.NativeSessionID,
			"claim_epoch":       o.Attempt.ClaimEpoch,
			"reason":            outcome.Reason,
		},
	})
	if err != nil {
		return fmt.Errorf("dispatch: record the recovery state on %s: %w", o.Assignment.ID, err)
	}
	return nil
}
