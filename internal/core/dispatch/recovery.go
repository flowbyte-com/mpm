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
// It returns one outcome per ASSIGNMENT it had something to say about,
// and the set of assignments it BLOCKED. An assignment it recovered
// silently is not reported — the normal dispatch path will pick the
// assignment up.
//
// # ONE ASSIGNMENT, ONE OUTCOME
//
// An assignment can have MORE THAN ONE outstanding attempt: PR 3's
// reconcile creates a new attempt when the epoch advances, and the older
// one stays outstanding. EVERY attempt is inspected — an older session
// may still be working, and that is a blocker — but the assignment is
// reported ONCE, because a report that says the same thing twice is a
// report an operator has to reconcile by hand.
//
// # THE ORDER OF THE QUESTIONS
//
//  1. What does the runtime say about the recorded session? That answer
//     comes FIRST, because it is EVIDENCE, and the epoch arithmetic is
//     not.
//  2. Is the attempt's epoch the assignment's current epoch?
//  3. What can be done through the existing fenced paths?
func (e *Engine) recover(ctx context.Context) ([]Outcome, map[string]Outcome, error) {
	outstanding, err := e.store.OutstandingAttempts()
	if err != nil {
		return nil, nil, err
	}
	if len(outstanding) == 0 {
		return nil, nil, nil
	}

	out := []Outcome{}
	blocked := map[string]Outcome{}
	for _, g := range groupByAssignment(outstanding) {
		outcome, isBlocked, rerr := e.recoverAssignment(ctx, g)
		if rerr != nil {
			return out, blocked, rerr
		}
		if isBlocked {
			blocked[g.assignment.ID] = outcome
		}
		if outcome.Reason != "" {
			out = append(out, outcome)
		}
	}
	return out, blocked, nil
}

// assignmentGroup is one assignment's outstanding attempts.
//
// The attempts are in the order the store returned them: oldest first.
type assignmentGroup struct {
	assignment *assignment.Assignment
	attempts   []*assignment.OutstandingAttempt
}

// groupByAssignment groups the outstanding attempts by assignment.
//
// The ORDER of the groups is the order the assignments first appeared
// in, so the recovery pass is deterministic: two runs over the same rows
// consider them in the same order.
func groupByAssignment(outstanding []*assignment.OutstandingAttempt) []assignmentGroup {
	order := []string{}
	byID := map[string]*assignmentGroup{}
	for _, o := range outstanding {
		if o.Assignment == nil {
			continue
		}
		g, ok := byID[o.Assignment.ID]
		if !ok {
			g = &assignmentGroup{assignment: o.Assignment}
			byID[o.Assignment.ID] = g
			order = append(order, o.Assignment.ID)
		}
		g.attempts = append(g.attempts, o)
	}
	out := make([]assignmentGroup, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

// recoverAssignment reconciles ONE assignment's outstanding attempts.
//
// It returns the outcome for the assignment — empty when the assignment
// has nothing to say and the dispatch path should pick it up — and
// whether the assignment is BLOCKED.
//
// # EVERY ATTEMPT IS PROCESSED, AND THE BLOCKER WINS THE REPORT
//
// The loop does NOT stop at the first blocker. An assignment can carry
// an older attempt whose session may still be working AND a newer
// attempt at the current epoch whose outcome nobody recorded — and the
// newer one's record is DURABLE WORK that must not be lost because the
// older one is a blocker. So every attempt is inspected and every
// reconcilable outcome is recorded, and the REPORT says one thing: the
// blocker, because that is what the operator has to deal with.
//
// Stopping at the first blocker would lose the newer record. That is
// not a reporting nicety: the reconciliation IS the write that records
// a runtime's outcome, and a write that never happens is work lost.
func (e *Engine) recoverAssignment(ctx context.Context, g assignmentGroup) (
	Outcome, bool, error) {

	// THE BLOCKER is what the operator has to deal with. THE REPORTED
	// outcome is the first thing the pass had to say about the
	// assignment — recorded or refused — and `wasRecorded` says whether a
	// WRITE happened, because a refusal must never be described as a
	// reconciliation.
	var blocker *Outcome
	var reported *Outcome
	var wasRecorded bool

	for _, o := range g.attempts {
		outcome := Outcome{
			AssignmentID:    g.assignment.ID,
			RootID:          g.assignment.RootID,
			Role:            g.assignment.Role,
			AttemptID:       o.Attempt.ID,
			NativeSessionID: o.Attempt.NativeSessionID,
			Epoch:           o.Attempt.ClaimEpoch,
		}

		// WHAT DOES THE RUNTIME SAY? FIRST, because it is evidence.
		state, reason, detail := e.inspect(ctx, g.assignment, o.Attempt.NativeSessionID)
		if reason != "" {
			// THE RUNTIME COULD NOT BE ASKED. That is the UNKNOWN case,
			// and it is the one that blocks: a session that cannot be
			// inspected may still be working, and a second writer cannot
			// be ruled out.
			outcome.Reason = reason
			outcome.Detail = detail
			// NO NOTE FOR A CANCELLED RUN. The operator stopped it, or
			// their deadline expired, and a note that says "recovery
			// required" for a deliberate stop would train the operator
			// to ignore the notes that matter.
			if ctx.Err() == nil {
				if err := e.noteRecovery(o, outcome); err != nil {
					return outcome, false, err
				}
			}
			if blocker == nil {
				blocker = &outcome
			}
			// THE NEXT ATTEMPT IS STILL INSPECTED: this one is a
			// blocker, and a blocker is not a reason to lose another
			// attempt's record.
			continue
		}

		// THE SESSION IS STILL RUNNING.
		//
		// It may or may not be — the runtime knows it has not reported
		// an outcome, and that is the whole of the evidence. It blocks:
		// a second writer into the same workspace is exactly the
		// duplicate this record exists to prevent.
		if state != nil && state.Outcome == "" {
			outcome.Reason = ReasonSessionStillRunning
			outcome.Detail = fmt.Sprintf(
				"the session %s has not reported an outcome, so it may still be working "+
					"and a second writer cannot be ruled out", o.Attempt.NativeSessionID)
			drained, recorded, derr := e.drain(ctx, o, outcome)
			if derr != nil {
				return outcome, false, derr
			}
			// The drain may have RECORDED the outcome while it waited,
			// and then it is a completion and not a blocker.
			if drained.Reason == ReasonRecoveryRequired ||
				drained.Reason == ReasonRuntimeUnavailable ||
				drained.Reason == ReasonSessionStillRunning {
				if blocker == nil {
					blocker = &drained
				}
				continue
			}
			if reported == nil {
				reported = &drained
				wasRecorded = recorded
			}
			continue
		}

		// A TERMINAL OUTCOME, AT THE ASSIGNMENT'S CURRENT EPOCH.
		//
		// It is reconciled through the existing fenced path — PR 3's
		// adapter, which records what the runtime said and nothing
		// else.
		//
		// An outcome from an OLDER epoch is NOT reconciled: that
		// attempt's writes are fenced out, so nobody can record it, and
		// the work has to run again. That is PR 3's at-least-once
		// choice, and the dispatch path makes it — the assignment is
		// left alone here so it appears in the report exactly once.
		if state != nil && state.Outcome != "" &&
			o.Attempt.ClaimEpoch == g.assignment.ClaimEpoch {
			outcome.RuntimeOutcome = state.Outcome
			result, recorded, rerr := e.reconcile(ctx, g.assignment, outcome)
			if rerr != nil {
				return outcome, false, rerr
			}
			if reported == nil {
				reported = &result
				wasRecorded = recorded
			}
			continue
		}

		// EVERYTHING ELSE IS LEFT TO THE DISPATCH PATH.
		//
		// The session does not exist, or it belongs to an older claim.
		// Nothing was created, or nothing can be recorded, so there is
		// nothing to reconcile and nothing to block. The assignment is
		// reported by the dispatch path, which claims it — and a live
		// lease there is what says the work is still with somebody.
		//
		// THE NEXT ATTEMPT IS STILL INSPECTED: an older attempt's
		// session may be gone while the current one is still working,
		// and that is a blocker too.
	}

	// THE BLOCKER WINS THE REPORT, AND THE RECORDING IS MENTIONED.
	//
	// The operator has to deal with the blocker, so that is what the
	// report says — and it says what else was RECORDED, because a
	// reconciliation that is not visible is a write nobody can audit.
	//
	// ONLY A RECORDING IS MENTIONED AS ONE. A refusal wrote nothing, and
	// a report that says "was reconciled" about a refusal is asserting a
	// durable record that does not exist.
	if blocker != nil {
		if reported != nil && wasRecorded {
			blocker.Detail = fmt.Sprintf(
				"%s; the attempt at epoch %d was reconciled through the existing fenced path",
				blocker.Detail, reported.Epoch)
		}
		return *blocker, true, nil
	}
	if reported != nil {
		return *reported, false, nil
	}
	return Outcome{}, false, nil
}

// inspect asks the runtime what it knows about a session.
//
// It returns the state, or a reason code when the runtime could not be
// asked. The three answers are kept apart, because they mean different
// things: ABSENT is safe, UNREACHABLE is ambiguous, and RUNNING is a
// blocker.
//
// # THE WORKSPACE IS BUILT HERE, NOT BY THE CALLER
//
// The real factory REFUSES a worker runtime with no authorised
// workspace, because a runtime with no workspace is a runtime that could
// run anywhere. So the inspection path builds the assignment's workspace
// exactly as the dispatch path does — the same directory, from the same
// configured root. An inspection that handed the factory an empty
// workspace would fail on every implementer, which would silently turn
// the whole recovery pass into "the runtime is unavailable".
func (e *Engine) inspect(ctx context.Context, a *assignment.Assignment,
	sessionID string) (*oc.SessionState, string, string) {

	// A CANCELLED RUN IS NOT AN UNREACHABLE RUNTIME.
	//
	// The inspection is a command, and a command on a cancelled context
	// fails — so without this check a cancelled run would be reported as
	// "the runtime could not be asked", which is a lie about the runtime
	// and a decision the operator would act on wrongly.
	if err := ctx.Err(); err != nil {
		return nil, ReasonRecoveryRequired,
			"the run was cancelled before the session could be inspected"
	}
	workspace := ""
	if a.Role != assignment.RoleReviewer {
		var werr error
		workspace, werr = e.workspaceFor(a)
		if werr != nil {
			return nil, ReasonRuntimeUnavailable, werr.Error()
		}
	}
	worker, reviewer, err := e.factory(ctx, a, workspace)
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
	outcome Outcome) (Outcome, bool, error) {

	// THE WORKSPACE IS BUILT FOR THE IMPLEMENTER, AND ONLY FOR THE
	// IMPLEMENTER — the same rule the dispatch path follows. A reviewer
	// has no worktree of its own: it examines the configured
	// repository, and asking for a worktree would create a directory
	// nobody works in.
	workspace := ""
	if a.Role != assignment.RoleReviewer {
		var werr error
		workspace, werr = e.workspaceFor(a)
		if werr != nil {
			outcome.Reason = ReasonRuntimeUnavailable
			outcome.Detail = werr.Error()
			return outcome, false, nil
		}
	}
	worker, reviewer, err := e.factory(ctx, a, workspace)
	if err != nil {
		outcome.Reason = ReasonRuntimeUnavailable
		outcome.Detail = err.Error()
		return outcome, false, nil
	}
	if a.Role == assignment.RoleReviewer {
		if reviewer == nil {
			outcome.Reason = ReasonReviewerUnavailable
			outcome.Detail = "the reviewer runtime was not built"
			return outcome, false, nil
		}
		// The review path reconciles through its own execution, which
		// refuses to run a SECOND reviewer over work that may already
		// have been reviewed.
		outcome.Reason = ReasonRecoveryRequired
		outcome.Detail = "the review session reported a terminal outcome; the review " +
			"path owns the recording of it, and it refuses to run a second reviewer " +
			"while an attempt is outstanding"
		return outcome, false, nil
	}
	if worker == nil {
		outcome.Reason = ReasonRuntimeUnavailable
		outcome.Detail = "the worker runtime was not built"
		return outcome, false, nil
	}
	// RECORD ONLY.
	//
	// The adapter decides for itself whether to record, resume or mint a
	// NEW attempt and launch it. The recovery pass wants the first and
	// must never have the third: a launch from the recovery path is a
	// second writer into a workspace whose earlier session may still be
	// working, and it is the one thing this package exists to prevent.
	//
	// So the adapter is told to record only, and it REFUSES to resume or
	// to launch when it cannot record. The refusal is loud.
	res, err := e.store.Execute(ctx, worker, assignment.ExecuteParams{
		ID:          a.ID,
		Epoch:       a.ClaimEpoch,
		Actor:       assignment.Actor{Address: a.Recipient, Kind: "agent"},
		Agent:       e.cfg.WorkerAgent,
		Workspace:   workspace,
		WorkspaceID: a.ID,
		Model:       e.cfg.Model,
		Title:       "assignment " + a.ID,
		RecordOnly:  true,
	})
	if err != nil {
		outcome.Reason = classifyAdapterRefusal(err)
		outcome.Detail = err.Error()
		// A REFUSAL IS NOT A RECONCILIATION — UNLESS SOMETHING WAS
		// WRITTEN. The adapter can record the runtime's outcome and
		// then fail to move the ASSIGNMENT, and then a write DID
		// happen. Discarding the result would under-claim it, and the
		// report's audit trail would lose a record that is in the
		// ledger.
		return outcome, res != nil && res.Reconciled, nil
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
	// `Reconciled` says the adapter RECORDED an observed outcome rather
	// than launching anything. It is what tells the report whether a
	// write happened.
	return outcome, res.Reconciled, nil
}

// drain waits, bounded, for a running session to report a terminal
// outcome.
//
// This is the narrowest provable drain: it observes and it waits. It does
// not stop the session, because there is no verified host operation that
// safely does that, and it does not assume the session will finish. When
// the window closes, the attempt is reported as needing an operator.
func (e *Engine) drain(ctx context.Context, o *assignment.OutstandingAttempt,
	outcome Outcome) (Outcome, bool, error) {

	if e.cfg.DrainTimeoutSeconds <= 0 || e.cfg.DrainPollIntervalSeconds <= 0 {
		// NO NOTE FOR A CANCELLED RUN. This is the branch the DEFAULT
		// configuration takes — the drain window ships disabled — so it
		// is the one a cancelled run is most likely to reach.
		if ctx.Err() == nil {
			if err := e.noteRecovery(o, outcome); err != nil {
				return outcome, false, err
			}
		}
		outcome.Reason = ReasonRecoveryRequired
		outcome.Detail = "the recorded session is still running and no drain window is " +
			"configured, so the operator has to deal with it"
		return outcome, false, nil
	}

	// THE DRAIN WINDOW IS REAL TIME, NOT THE INJECTED CLOCK.
	//
	// The injected clock governs the LEASE — when a claim expires and
	// when an epoch moves. A drain is a WAIT on an external system, and
	// a wait bounded by a clock that never advances is a wait that never
	// ends. So the window is a context timeout, which is real time, and
	// the caller can cut it short by cancelling the context.
	waitCtx, cancel := context.WithTimeout(ctx,
		time.Duration(e.cfg.DrainTimeoutSeconds)*time.Second)
	defer cancel()
	for {
		state, reason, detail := e.inspect(ctx, o.Assignment, o.Attempt.NativeSessionID)
		if reason != "" {
			// THE CALLER CANCELLED, OR THE RUNTIME WENT AWAY.
			//
			// They are different facts, and the cancellation is checked
			// FIRST: an inspection that fails because the context was
			// cancelled is not a runtime that went away, and telling the
			// operator the runtime vanished when the operator stopped the
			// run would be a lie.
			if ctx.Err() != nil {
				outcome.Reason = ReasonRecoveryRequired
				outcome.Detail = "the drain was interrupted before the session reported an outcome"
			} else {
				outcome.Reason = reason
				outcome.Detail = detail
			}
			// THE SESSION'S EFFECTS ARE UNKNOWN either way, so the
			// operator has to be told — UNLESS the run was cancelled, in
			// which case a note would train the operator to ignore the
			// notes that matter.
			if ctx.Err() == nil {
				if err := e.noteRecovery(o, outcome); err != nil {
					return outcome, false, err
				}
			}
			return outcome, false, nil
		}
		if state != nil && state.Outcome != "" {
			outcome.RuntimeOutcome = state.Outcome
			return e.reconcile(ctx, o.Assignment, outcome)
		}
		select {
		case <-waitCtx.Done():
			// TWO ENDINGS, AND THEY ARE NOT THE SAME FACT.
			//
			// The WINDOW CLOSING is the drain's own end: the session was
			// still running when the window shut, and its effects are
			// unknown. The CALLER CANCELLING is somebody stopping the
			// run on purpose, and the operator is the one who did it —
			// so the message says what happened instead of blaming the
			// window.
			outcome.Reason = ReasonRecoveryRequired
			if ctx.Err() != nil {
				outcome.Detail = "the drain was interrupted before the session reported an outcome"
			} else {
				outcome.Detail = fmt.Sprintf(
					"the recorded session was still running after a %ds drain window, so its "+
						"effects are unknown and it blocks another writer into the same workspace",
					e.cfg.DrainTimeoutSeconds)
			}
			// NO NOTE FOR A CANCELLED RUN.
			if ctx.Err() == nil {
				if err := e.noteRecovery(o, outcome); err != nil {
					return outcome, false, err
				}
			}
			return outcome, false, nil
		case <-time.After(time.Duration(e.cfg.DrainPollIntervalSeconds) * time.Second):
		}
	}
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
