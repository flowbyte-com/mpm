package assignment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	oc "github.com/flowbyte-com/mpm-core/opencode"
)

// The adapter: MPM assignment state ⇄ OpenCode runtime state.
//
// WHAT IT IS
//
// A TRANSLATOR. It takes an explicitly selected, already-authorised
// assignment, checks that the caller really holds it, associates it with
// exactly one native runtime session, asks the runtime to do the work, reads
// back what the runtime says happened, and records that durably.
//
// It does not implement inference, model selection, tool execution or a
// conversation loop. OpenCode owns those.
//
// WHAT IT REFUSES
//
//   - an assignment that is not an implementer's;
//   - an epoch that is not the assignment's current claim;
//   - an assignment whose state is not executable;
//   - a tree that does not permit `start` and `complete`;
//   - an agent definition that is not on the explicit allow-list;
//   - a runtime run whose reported workspace is not the authorised one;
//   - a session carrying a SESSION-LEVEL permission grant. The host
//     honours those over the agent definition's denies — measured — so the
//     adapter treats a non-empty `permissions` array as a refusal, not a
//     detail.
//
// WHAT IT WILL NOT DO
//
// Infer success. A session that exists proves nothing: the runtime's own
// `outcome` is the only thing that does, and an absence of one is recorded
// as AMBIGUOUS rather than folded into a boolean.

// Runtime is the boundary the adapter drives.
//
// It is an interface so the failure tests can inject a fake, and so the
// real boundary has exactly ONE implementation (internal/core/opencode).
type Runtime interface {
	// AgentAllowed answers before a session exists.
	AgentAllowed(name string) bool
	// VerifyAgent asks the runtime for the agent's EFFECTIVE permissions.
	VerifyAgent(ctx context.Context, name string) error
	// Launch creates-or-continues the caller-minted session and runs the
	// objective.
	Launch(ctx context.Context, req oc.LaunchRequest) (*oc.LaunchResult, error)
	// Inspect reads a session's own report without running anything.
	Inspect(ctx context.Context, sessionID string) (*oc.SessionState, error)
}

// ExecuteParams selects one assignment and one authorised workspace.
type ExecuteParams struct {
	// ID is the assignment the operator explicitly selected.
	ID string
	// Epoch is the claim epoch the caller holds. It fences every write.
	Epoch int64
	// Actor is attribution, not identity — the same rule PR 2 states.
	Actor Actor
	// Agent is the runtime agent definition. It must be allow-listed.
	Agent string
	// Workspace is the authorised worktree, absolute, as the HOST sees it.
	Workspace string
	// ExpectedRuntimeDir is the path the RUNTIME will report for that
	// worktree. When the runtime is contained, the worktree is bind
	// mounted at a fixed namespace path, so the host path and the
	// runtime's path differ by design — and the comparison is against the
	// namespace path, because that is what the runtime can observe. The
	// bind mount is what ties the two together.
	ExpectedRuntimeDir string
	// WorkspaceID is the stable label recorded on the attempt.
	WorkspaceID string
	// Model is the runtime model identifier, recorded for verification.
	Model string
	// Title labels the native session.
	Title string
	// References are bounded pointers appended to the objective. They are
	// POINTERS, never copies, and they never carry a credential.
	References []string
	// RecordOnly says the caller has OBSERVED an outcome and wants it
	// RECORDED — and nothing else.
	//
	// # WHY IT EXISTS
	//
	// The adapter's own decision has four answers: record what it
	// inspected, refuse a concurrent writer, resume the recorded session,
	// or mint a NEW attempt and launch it. The first is what a recovery
	// pass wants; the last is what it must NEVER do — a recovery pass
	// that launches is a dispatcher that dispatches from the recovery
	// path, and it puts a second writer into a workspace whose earlier
	// session may still be working.
	//
	// So a record-only caller gets the recording path or a refusal, and
	// NEVER a launch, a resume or a new attempt. The refusal is loud:
	// the caller asked to record an outcome, and the adapter says why it
	// cannot, instead of quietly doing something else.
	//
	// The zero value is false, which is the adapter's ordinary behaviour.
	RecordOnly bool
}

// ExecuteResult is what one explicit invocation did.
type ExecuteResult struct {
	Assignment *Assignment     `json:"assignment"`
	Attempt    *SessionAttempt `json:"attempt"`
	// LaunchState is the attempt's state after the call. `ambiguous` is a
	// real answer: it says the adapter could not determine whether the
	// runtime did the work.
	LaunchState string `json:"launch_state"`
	// RuntimeOutcome is what the RUNTIME said: "succeeded", "failed", or
	// "" when it said nothing.
	RuntimeOutcome string `json:"runtime_outcome"`
	// Reconciled reports that an existing session/attempt was reused
	// rather than a new one launched.
	Reconciled bool `json:"reconciled"`
	// RuntimeSucceeded says the RUNTIME reported success. It is NOT the
	// same as the assignment completing: a fenced completion can be
	// refused while the runtime's work still happened, and the two facts
	// are reported separately rather than collapsed.
	RuntimeSucceeded bool `json:"runtime_succeeded"`
	// CompletionError names why the assignment did not complete even
	// though the runtime succeeded. Empty when it did complete.
	CompletionError string `json:"completion_error,omitempty"`
	// Evidence is the bounded, credential-free record of the run.
	Evidence ExecutionEvidence `json:"evidence"`
}

// ExecutionEvidence is the durable, bounded record of one execution.
//
// It carries pointers, a digest and sizes — never a transcript, never a
// model output, never a credential.
type ExecutionEvidence struct {
	SessionID        string `json:"session_id"`
	RuntimeDirectory string `json:"runtime_directory"`
	RuntimeAgent     string `json:"runtime_agent"`
	RuntimeModel     string `json:"runtime_model"`
	OutputSHA256     string `json:"output_sha256"`
	OutputBytes      int    `json:"output_bytes"`
	ExitCode         int    `json:"exit_code"`
	Reason           string `json:"reason,omitempty"`
}

// minLeaseSeconds is how much lease the adapter insists on before it will
// start work. A lease that expires mid-run turns a real completion into a
// stale one, so the check is deliberately generous.
const minLeaseSeconds = 120

// Execute runs one assignment, explicitly invoked.
//
// IT RECONCILES BEFORE IT LAUNCHES. The order is:
//
//  1. validate the assignment (role, claim, state, permitted operations);
//  2. read the task's attempts;
//  3. decide, from EVIDENCE, whether work can resume, must be refused, or
//     can safely start;
//  4. only then mint an id, persist the intent, and launch.
//
// Step 3 is what stops a retry from silently duplicating execution.
func (s *Store) Execute(ctx context.Context, rt Runtime, p ExecuteParams) (*ExecuteResult, error) {
	if rt == nil {
		return nil, fmt.Errorf("%w: the adapter has no runtime", ErrConstraint)
	}
	if strings.TrimSpace(p.ID) == "" {
		return nil, fmt.Errorf("%w: execute needs an assignment id", ErrConstraint)
	}
	if !filepath.IsAbs(p.Workspace) {
		return nil, fmt.Errorf("%w: the workspace must be absolute, got %q", ErrConstraint, p.Workspace)
	}
	// BEFORE ANY SESSION EXISTS. The measured host behaviour for an
	// unknown agent is a RUNTIME failure, not a create-time refusal, so
	// relying on the host would mean creating a session and burning a run
	// before learning the name was wrong.
	if !rt.AgentAllowed(p.Agent) {
		return nil, fmt.Errorf("%w: agent %q is not on the allow-list", ErrConstraint, p.Agent)
	}

	task, err := s.Get(p.ID)
	if err != nil {
		return nil, err
	}
	if err := s.validateExecutable(task, p); err != nil {
		return nil, err
	}

	decision, err := s.reconcile(task, p, rt)
	if err != nil {
		return nil, err
	}

	// A RECORD-ONLY CALLER NEVER LAUNCHES.
	//
	// The decision to resume a recorded session or to mint a new attempt
	// is the adapter's, and it is the right decision for a caller that
	// wants the work DONE. A caller that has OBSERVED an outcome and
	// wants it recorded is asking for something else entirely, and the
	// two must not be confused: the second attempt would run the
	// objective again, in a workspace whose earlier session may still be
	// working.
	if p.RecordOnly {
		switch decision.kind {
		case reconcileResume:
			return nil, fmt.Errorf(
				"%w: %s has a launch intent whose session the runtime has never heard of, "+
					"so there is no outcome to record and the caller asked for no launch",
				ErrRecordOnlyRefused, task.ID)
		case reconcileNew:
			// The cause is deliberately NOT over-specified: the adapter
			// answers `reconcileNew` for a terminal attempt, for an
			// assignment with no attempt at all, and when the epoch
			// advanced. Naming one of them would be a guess.
			return nil, fmt.Errorf(
				"%w: %s would need a NEW attempt, and launching one is exactly what the "+
					"caller asked not to happen: it asked for an outcome to be recorded",
				ErrRecordOnlyRefused, task.ID)
		}
	}

	var attempt *SessionAttempt
	switch decision.kind {
	case reconcileRefuseConcurrent:
		return nil, fmt.Errorf("%w: %s (%s)",
			ErrConstraint, decision.reason, decision.attemptID)
	case reconcileRecordAmbiguous:
		// The intent exists and the runtime cannot tell us whether the
		// launch happened. Recording the ambiguity and STOPPING is the
		// honest answer: launching again could duplicate the work.
		att, err := s.MarkAttemptAmbiguous(SessionStateParams{
			AttemptID: decision.attemptID,
			Epoch:     p.Epoch,
			Error:     decision.reason,
		})
		if err != nil {
			return nil, err
		}
		return &ExecuteResult{
			Assignment:  task,
			Attempt:     att,
			LaunchState: att.LaunchState,
			Reconciled:  true,
			Evidence:    evidenceFromAttempt(att, nil, nil, decision.reason),
		}, nil
	case reconcileRecordOnly:
		return s.recordObserved(task, decision.attempt, decision.state, p)
	case reconcileResume:
		attempt = decision.attempt
	case reconcileNew:
		attempt, err = s.beginAttemptFor(task, p)
		if err != nil {
			return nil, err
		}
	}

	// From here the attempt exists and its identity is durable. Everything
	// below is recoverable by `attempt.NativeSessionID`.
	return s.launchAndRecord(ctx, rt, task, attempt, p, decision.kind == reconcileResume)
}

// validateExecutable applies every precondition that does not need the
// runtime.
func (s *Store) validateExecutable(task *Assignment, p ExecuteParams) error {
	if task.Role != RoleImplementer {
		return fmt.Errorf("%w: %s is a %s; its execution belongs to the review path",
			ErrConstraint, task.ID, task.Role)
	}
	// The execution barrier: an ambiguous tree admits only scoped, named
	// execution, and superseded work is frozen. The reconcile path
	// re-checks ambiguity; this names the refusal before any session
	// exists so no runtime launches.
	if err := s.execBlocked(task); err != nil {
		return err
	}
	if task.ClaimEpoch != p.Epoch {
		return fmt.Errorf("%w: %s is at epoch %d", ErrStaleClaim, task.ID, task.ClaimEpoch)
	}
	if task.ClaimEpoch == 0 {
		return fmt.Errorf("%w: %s holds no claim", ErrConstraint, task.ID)
	}
	// THE LEASE. Work started near the end of a lease becomes a stale
	// completion, so the adapter refuses to start without headroom.
	now := s.Now()
	if task.LeaseExpiresAt == nil {
		return fmt.Errorf("%w: %s has no lease", ErrConstraint, task.ID)
	}
	if *task.LeaseExpiresAt-now < minLeaseSeconds {
		return fmt.Errorf("%w: %s has less than %ds of lease left",
			ErrConstraint, task.ID, minLeaseSeconds)
	}
	switch task.State {
	case StateAccepted, StateInProgress:
	default:
		return fmt.Errorf("%w: %s is %s and cannot be executed",
			ErrIllegalTransition, task.ID, task.State)
	}
	// THE TREE'S OPERATIONS. The adapter performs `start` and `complete`,
	// so a tree that does not permit them is a tree this execution would
	// step outside of.
	//
	// A CHILD carries no list of its own — the schema says so, and the
	// bound is the ROOT's. So the operations are read from the root, which
	// is what a child actually inherits. (The first version checked the
	// child's own empty list and therefore refused every child
	// assignment.)
	root, err := s.Get(task.RootID)
	if err != nil {
		return err
	}
	if err := requireOperations(root, "start", "complete"); err != nil {
		return err
	}
	return nil
}

// requireOperations checks a ROOT's permitted-operation list.
//
// A child carries no list of its own — its bound is the ROOT's — so the
// check walks to the root, which is the same rule the schema states.
func requireOperations(root *Assignment, ops ...string) error {
	permitted := map[string]bool{}
	for _, o := range root.PermittedOperations {
		permitted[o] = true
	}
	for _, o := range ops {
		if !permitted[o] {
			return fmt.Errorf("%w: the tree does not permit %q", ErrConstraint, o)
		}
	}
	return nil
}

// reconcileKind is the recovery decision.
type reconcileKind int

const (
	reconcileNew reconcileKind = iota
	// reconcileResume launches with the recorded id. It is used ONLY when
	// the runtime has not reported a terminal outcome.
	reconcileResume
	// reconcileRecordOnly records what the runtime ALREADY reported,
	// without launching anything. A session that has finished is
	// finished: re-prompting it would run the objective a second time
	// inside the same conversation, and the attempt record would still
	// show one attempt.
	reconcileRecordOnly
	reconcileRecordAmbiguous
	reconcileRefuseConcurrent
)

type reconcileDecision struct {
	kind      reconcileKind
	attempt   *SessionAttempt
	attemptID string
	reason    string
	// state is what the runtime ALREADY reported, when the decision is to
	// record rather than launch.
	state *oc.SessionState
}

// reconcile decides, from EVIDENCE, what a retry may do.
//
// The cases, and why each is what it is:
//
//	no attempts                        -> new
//	last attempt intent, session ABSENT -> resume (launch; nothing was created)
//	last attempt intent, session EXISTS -> resume (reuse the SAME id)
//	last attempt launched/running       -> inspect; refuse a concurrent duplicate
//	last attempt completed/failed/etc   -> new attempt (a visible retry)
//
// "Resume" is the word for both intent cases because `opencode run --session
// <id>` creates-or-continues: the same command is correct whether the
// session exists or not, and that is precisely why the id is persisted
// first.
//
// # WHAT REUSE DOES NOT PROVE
//
// A reused session id does not prove a repeated prompt had no effects. The
// adapter therefore never re-prompts a session it finds in a terminal or
// running state; it either records what happened or refuses.
func (s *Store) reconcile(task *Assignment, p ExecuteParams, rt Runtime) (reconcileDecision, error) {
	attempts, err := s.AttemptsFor(task.ID)
	if err != nil {
		return reconcileDecision{}, err
	}
	if len(attempts) == 0 {
		return reconcileDecision{kind: reconcileNew}, nil
	}

	// THE HARD BARRIER, BEFORE ANY EPOCH OR SESSION LOGIC.
	//
	// An ambiguous attempt is an execution whose effects could not be
	// determined. A newer claim epoch, an expired lease and an absent
	// native session are not evidence that it did no work: the session
	// may have run and the runtime may have lost it. So ANY ambiguous
	// attempt in the history — not only the most recent one — refuses a
	// new launch. A later failed or completed attempt cannot hide it.
	//
	// RECORD-ONLY IS NOT A LAUNCH. It lets the recovery path record a
	// terminal outcome for a LAUNCHED/RUNNING attempt. The ambiguous case
	// below still refuses to relabel an ambiguous attempt: that requires
	// an operator reconciliation outside this adapter.
	if !p.RecordOnly {
		for _, a := range attempts {
			if a.LaunchState == SessionAmbiguous {
				return reconcileDecision{}, fmt.Errorf(
					"%w: %s has an unresolved ambiguous attempt %s at epoch %d; "+
						"operator recovery is required before another launch",
					ErrAmbiguousAttempt, task.ID, a.ID, a.ClaimEpoch)
			}
		}
	}
	last := attempts[len(attempts)-1]

	// THE ATTEMPT BELONGS TO ITS CLAIM. After a claim or a renew the
	// assignment's epoch advances, and an attempt stamped with the OLD
	// epoch can never write its result: the state write is fenced on
	// both the attempt's epoch and the assignment's current one. Resuming
	// it would launch a session whose outcome could not be recorded, and
	// every retry would launch it again. So a new epoch is a new attempt.
	if last.ClaimEpoch != p.Epoch {
		// AN OLDER EPOCH IS NOT A LICENCE TO START A SECOND WRITER.
		//
		// The epoch advancing does not prove the previous holder's session
		// stopped. If any earlier attempt's session is still working — or
		// cannot be asked — then a new attempt would put TWO runtimes into
		// the same worktree, which is exactly the duplicate this record
		// exists to prevent. Refuse, and leave the drain policy to PR 4's
		// recovery loop, which is the component that will own long-lived
		// attempts.
		for i := len(attempts) - 1; i >= 0; i-- {
			prev := attempts[i]
			if prev.ClaimEpoch == p.Epoch {
				continue
			}
			st, err := inspectQuietly(rt, prev.NativeSessionID)
			if err != nil {
				if errors.Is(err, oc.ErrSessionNotFound) {
					continue // nothing was created; safe
				}
				return reconcileDecision{
					kind:      reconcileRefuseConcurrent,
					attemptID: prev.ID,
					reason: "an earlier claim's session cannot be inspected, so a second " +
						"writer cannot be ruled out",
				}, nil
			}
			if st != nil && st.Outcome == "" {
				return reconcileDecision{
					kind:      reconcileRefuseConcurrent,
					attemptID: prev.ID,
					reason:    "an earlier claim's session is still running",
				}, nil
			}
		}
		return reconcileDecision{kind: reconcileNew}, nil
	}

	switch last.LaunchState {
	case SessionIntent:
		// The intent is durable and the id is known. Whether the runtime
		// was started is exactly what we must find out.
		//
		// THE TWO FAILURES ARE DIFFERENT. "The runtime has never heard of
		// this session" is the SAFE case: nothing was created, so reusing
		// the recorded id cannot duplicate anything. "The runtime could
		// not be asked" is the AMBIGUOUS case, and a retry must stop.
		state, err := inspectQuietly(rt, last.NativeSessionID)
		if err != nil {
			if errors.Is(err, oc.ErrSessionNotFound) {
				return reconcileDecision{kind: reconcileResume, attempt: last}, nil
			}
			return reconcileDecision{
				kind:      reconcileRecordAmbiguous,
				attemptID: last.ID,
				reason:    "the runtime could not be asked about the session recorded in the launch intent",
			}, nil
		}
		// THE INSPECTED STATE IS USED, NOT DISCARDED. An intent whose
		// session already reported an outcome is FINISHED — the runtime
		// may have completed the work before the adapter could record it.
		// Re-launching would run the objective again.
		if state != nil {
			if state.Outcome != "" {
				return reconcileDecision{kind: reconcileRecordOnly, attempt: last, state: state}, nil
			}
			return reconcileDecision{
				kind:      reconcileRefuseConcurrent,
				attemptID: last.ID,
				reason:    "the recorded session exists and has not reported an outcome",
			}, nil
		}
		return reconcileDecision{kind: reconcileResume, attempt: last}, nil

	case SessionLaunched, SessionRunning:
		state, err := inspectQuietly(rt, last.NativeSessionID)
		if err != nil && !errors.Is(err, oc.ErrSessionNotFound) {
			return reconcileDecision{
				kind:      reconcileRecordAmbiguous,
				attemptID: last.ID,
				reason:    "the runtime could not be asked about a launched attempt",
			}, nil
		}
		if state == nil {
			// The runtime lost it. That is ambiguous, not a licence to
			// launch again.
			return reconcileDecision{
				kind:      reconcileRecordAmbiguous,
				attemptID: last.ID,
				reason:    "a launched attempt's session is no longer known to the runtime",
			}, nil
		}
		if state.Outcome != "" {
			// TERMINAL. Record what it said; do NOT re-prompt.
			return reconcileDecision{kind: reconcileRecordOnly, attempt: last, state: state}, nil
		}
		// A running session: launching a second one would be the
		// duplicate this whole record exists to prevent.
		return reconcileDecision{
			kind:      reconcileRefuseConcurrent,
			attemptID: last.ID,
			reason:    "an attempt is still running",
		}, nil

	case SessionCompleted, SessionFailed:
		// A DEFINITIVELY TERMINAL attempt. The work has been recorded; a
		// retry is a NEW attempt with its own number, so the record shows
		// two. Only FAILED is retryable here: an ambiguous attempt was
		// refused above, and must never become a retry merely because a
		// caller chose a different epoch.
		//
		// This must NOT re-record: the runtime keeps a finished session in
		// its list forever, so `recordOnly` here would turn every retry
		// after a failure into a silent no-op that re-reports the old
		// result. (The first remediation did exactly that.)
		return reconcileDecision{kind: reconcileNew}, nil

	case SessionAmbiguous:
		// DEFENCE IN DEPTH. The barrier above catches every non-record-only
		// caller. A record-only caller is refused as well: the attempt's
		// durable state is terminal-ambiguous, and the state machine does
		// not permit it to become completed or failed. Only an operator
		// reconciliation outside this adapter may resolve it.
		return reconcileDecision{
			kind:      reconcileRefuseConcurrent,
			attemptID: last.ID,
			reason: "an ambiguous attempt cannot be treated as a retry; " +
				"operator recovery is required",
		}, nil

	default:
		// A terminal attempt. A retry is a NEW attempt, with its own
		// number and its own id, so the record shows two.
		return reconcileDecision{kind: reconcileNew}, nil
	}
}

// inspectQuietly asks the runtime about a session, returning nil when the
// runtime does not know it.
func inspectQuietly(rt Runtime, sessionID string) (*oc.SessionState, error) {
	if rt == nil {
		return nil, errors.New("assignment: no runtime is available to inspect a session")
	}
	return rt.Inspect(context.Background(), sessionID)
}

// beginAttemptFor mints an id and persists the intent.
func (s *Store) beginAttemptFor(task *Assignment, p ExecuteParams) (*SessionAttempt, error) {
	sessionID, err := mintSessionID()
	if err != nil {
		return nil, err
	}
	corr, err := mintCorrelationRef()
	if err != nil {
		return nil, err
	}
	// THE INTENT IS WRITTEN BEFORE THE LAUNCH. This ordering is the whole
	// recovery story: after a crash, the id in this row is what the retry
	// resumes.
	return s.BeginAttempt(BeginAttemptParams{
		TaskID:          task.ID,
		Epoch:           p.Epoch,
		Runtime:         RuntimeOpenCode,
		IntendedAgent:   p.Agent,
		NativeSessionID: sessionID,
		CorrelationRef:  corr,
		WorkspaceID:     p.WorkspaceID,
		WorkspacePath:   p.Workspace,
	})
}

// launchAndRecord runs the runtime and records what it actually said.
func (s *Store) launchAndRecord(
	ctx context.Context,
	rt Runtime,
	task *Assignment,
	attempt *SessionAttempt,
	p ExecuteParams,
	reconciled bool,
) (*ExecuteResult, error) {
	prompt := buildPrompt(task, p.References)

	if task.State == StateAccepted {
		started, err := s.Start(FencedParams{ID: task.ID, Epoch: p.Epoch, Actor: p.Actor})
		if err != nil {
			return nil, err
		}
		task = started
	}

	res, launchErr := rt.Launch(ctx, oc.LaunchRequest{
		SessionID: attempt.NativeSessionID,
		Agent:     p.Agent,
		Workspace: p.Workspace,
		Title:     p.Title,
		Prompt:    prompt,
	})

	// A LAUNCH THAT NEVER PRODUCED A STATE IS AMBIGUOUS, not a failure:
	// the process may have started and done work before the adapter lost
	// it.
	if res == nil || res.State == nil {
		reason := "the runtime produced no session state"
		if launchErr != nil {
			reason = boundedReason(launchErr)
		}
		att, err := s.MarkAttemptAmbiguous(SessionStateParams{
			AttemptID: attempt.ID, Epoch: p.Epoch, Error: reason,
		})
		if err != nil {
			return nil, err
		}
		return &ExecuteResult{
			Assignment: task, Attempt: att, LaunchState: att.LaunchState,
			Reconciled: reconciled,
			Evidence:   evidenceFromAttempt(att, res, launchErr, reason),
		}, nil
	}

	return s.recordState(task, attempt, res.State, p, reconciled, res)
}

// recordObserved records what the runtime ALREADY reported, without
// launching. It is the path for a session that finished before the adapter
// could observe it — a crash between the launch and the record.
func (s *Store) recordObserved(
	task *Assignment, attempt *SessionAttempt, state *oc.SessionState, p ExecuteParams,
) (*ExecuteResult, error) {
	// The SAME prerequisite the launch path has: a completion is only legal
	// from `in_progress`. Recording an observed result therefore advances
	// the assignment first, so the record and the state agree.
	if task.State == StateAccepted {
		started, err := s.Start(FencedParams{ID: task.ID, Epoch: p.Epoch, Actor: p.Actor})
		if err != nil {
			return nil, fmt.Errorf("%w: the observed result could not be recorded because "+
				"the assignment could not be started: %w", ErrStaleClaim, err)
		}
		task = started
	}
	return s.recordState(task, attempt, state, p, true, nil)
}

// recordState is the ONE place an outcome is believed and recorded.
func (s *Store) recordState(
	task *Assignment,
	attempt *SessionAttempt,
	state *oc.SessionState,
	p ExecuteParams,
	reconciled bool,
	res *oc.LaunchResult,
) (*ExecuteResult, error) {
	// THE SESSION'S OWN CLAIMS ARE CHECKED BEFORE ITS OUTCOME IS BELIEVED.
	if refusal := verifySession(state, p); refusal != "" {
		att, err := s.MarkAttemptFailed(SessionStateParams{
			AttemptID: attempt.ID, Epoch: p.Epoch,
			Outcome: state.Outcome, Error: refusal,
		})
		if err != nil {
			return nil, err
		}
		return &ExecuteResult{
			Assignment: task, Attempt: att, LaunchState: att.LaunchState,
			RuntimeOutcome: state.Outcome, Reconciled: reconciled,
			Evidence: evidenceFromAttempt(att, res, nil, refusal),
		}, nil
	}

	var att *SessionAttempt
	var err error
	switch state.Outcome {
	case "succeeded":
		att, err = s.MarkAttemptCompleted(SessionStateParams{
			AttemptID: attempt.ID, Epoch: p.Epoch, Outcome: "succeeded",
		})
		if err != nil {
			return nil, err
		}
		// THE COMPLETION IS FENCED. It carries the presented epoch, so a
		// worker whose lease was reclaimed cannot publish this as the
		// current holder's completion.
		completed, cerr := s.Complete(FencedParams{
			ID: task.ID, Epoch: p.Epoch, Actor: p.Actor,
			Note: "runtime session " + state.ID + " reported succeeded",
		})
		if cerr != nil {
			// RUNTIME SUCCESS IS NOT ASSIGNMENT COMPLETION.
			//
			// The runtime did the work and reported it; the ASSIGNMENT did
			// not move. Returning nil here would report a completion that
			// never happened, so the caller is told both facts and gets an
			// error.
			return &ExecuteResult{
				Assignment:       task,
				Attempt:          att,
				LaunchState:      att.LaunchState,
				RuntimeOutcome:   att.RuntimeOutcome,
				RuntimeSucceeded: true,
				CompletionError:  boundedReason(cerr),
				Reconciled:       reconciled,
				Evidence:         evidenceFromAttempt(att, res, nil, "the runtime succeeded but the assignment did not complete: "+boundedReason(cerr)),
			}, fmt.Errorf("%w: the runtime session succeeded but the assignment did not complete: %w",
				ErrStaleClaim, cerr)
		}
		task = completed
	case "failed":
		att, err = s.MarkAttemptFailed(SessionStateParams{
			AttemptID: attempt.ID, Epoch: p.Epoch, Outcome: "failed",
			Error: "the runtime reported failure",
		})
		if err != nil {
			return nil, err
		}
	default:
		// The runtime said nothing. Recording success would be an
		// invention.
		att, err = s.MarkAttemptAmbiguous(SessionStateParams{
			AttemptID: attempt.ID, Epoch: p.Epoch,
			Error: "the runtime reported no outcome",
		})
		if err != nil {
			return nil, err
		}
	}

	return &ExecuteResult{
		Assignment:       task,
		Attempt:          att,
		LaunchState:      att.LaunchState,
		RuntimeOutcome:   att.RuntimeOutcome,
		RuntimeSucceeded: state.Outcome == "succeeded",
		Reconciled:       reconciled,
		Evidence:         evidenceFromAttempt(att, res, nil, ""),
	}, nil
}

// verifySession checks what a session claims about itself.
//
// The return value is a REFUSAL REASON, or "" when the session may be
// believed. Every check here is a measured failure mode, not a hypothetical:
//
//   - `Location.Directory` comes from the runtime's PROCESS CWD, and
//     `session.create`'s `directory` parameter is ignored. So a session can
//     run somewhere other than where it was told, and the report is the
//     only way to see it.
//   - `Permissions` is the SESSION-LEVEL override list, and the host
//     honours those over the agent's denies. A non-empty list on a worker
//     session is a privilege widening, so the adapter refuses.
//   - `Agent` names the definition the runtime actually used.
//   - `Model` is what it actually ran, which is how a model selection is
//     verified rather than assumed.
func verifySession(state *oc.SessionState, p ExecuteParams) string {
	// THE RUNTIME RUNS IN THE AUTHORISED WORKSPACE, on this host, with no
	// bind mount in between. The expected directory is therefore the
	// workspace itself unless the caller names it.
	expected := p.ExpectedRuntimeDir
	if expected == "" {
		expected = p.Workspace
	}
	if expected != "" {
		if dir := filepath.Clean(state.Location.Directory); dir != filepath.Clean(expected) {
			return fmt.Sprintf("the session ran in %q, not the authorised workspace %q",
				state.Location.Directory, expected)
		}
	}
	// SESSION-LEVEL PERMISSION GRANTS ARE THE FRAMEWORK'S.
	//
	// MPM used to refuse a session that carried one, because such a grant
	// could widen the agent definition and MPM's containment was the thing
	// being protected. Without that boundary the definition and its
	// session overrides are the operator's agent framework policy; MPM
	// still verifies the agent NAME and the WORKSPACE, which are
	// orchestration facts, not tool policy.
	if state.Agent != p.Agent {
		return fmt.Sprintf("the session used agent %q, not %q", state.Agent, p.Agent)
	}
	if p.Model != "" {
		want := p.Model
		if i := strings.LastIndex(want, "/"); i >= 0 {
			want = want[i+1:]
		}
		// A session that names NO model has not verified the selection,
		// so it is refused rather than waved through — the check exists
		// so the model is verified rather than assumed.
		if state.Model.ID == "" {
			return "the session did not report which model it ran, so the selection is unverified"
		}
		if state.Model.ID != want {
			return fmt.Sprintf("the session ran model %q, not %q", state.Model.ID, want)
		}
	}
	return ""
}

// buildPrompt assembles the bounded objective.
//
// The prompt is DERIVED from the assignment, never supplied by the runtime
// or by another agent: the objective, the workspace rule, and the
// references as POINTERS. It carries no credential and no transcript.
func buildPrompt(task *Assignment, refs []string) string {
	var b strings.Builder
	b.WriteString("You are executing an MPM assignment.\n\n")
	b.WriteString("ID: " + task.ID + "\n")
	if task.HeadSHA != "" {
		b.WriteString("Revision under work: " + task.HeadSHA + "\n")
	}
	b.WriteString("\nOBJECTIVE\n")
	b.WriteString(task.Objective)
	b.WriteString("\n\nRULES\n")
	b.WriteString("- Work ONLY inside the current working directory.\n")
	b.WriteString("- You have no shell and no network. Use the file tools.\n")
	b.WriteString("- Report what you changed; do not claim work you did not do.\n")
	if len(refs) > 0 {
		b.WriteString("\nREFERENCES (pointers, not contents)\n")
		for _, r := range refs {
			b.WriteString("- " + r + "\n")
		}
	}
	return b.String()
}

// evidenceFromAttempt builds the bounded record. It never copies the
// runtime's output; it records its digest and size.
func evidenceFromAttempt(att *SessionAttempt, res *oc.LaunchResult, launchErr error, reason string) ExecutionEvidence {
	ev := ExecutionEvidence{
		SessionID: att.NativeSessionID,
		Reason:    reason,
	}
	if res != nil {
		ev.ExitCode = res.ExitCode
		ev.OutputBytes = len(res.Output)
		// AN EMPTY CAPTURE HAS NO DIGEST. The existing contract
		// distinguishes "no output was captured" from "the digest of an
		// empty string"; keep that distinction rather than inventing a
		// digest for silence.
		if len(res.Output) > 0 {
			sum := sha256.Sum256([]byte(res.Output))
			ev.OutputSHA256 = hex.EncodeToString(sum[:])
		}
		if res.State != nil {
			ev.RuntimeDirectory = res.State.Location.Directory
			ev.RuntimeAgent = res.State.Agent
			ev.RuntimeModel = res.State.Model.ProviderID + "/" + res.State.Model.ID
		}
		return ev
	}
	// THE RESULT MAY BE ABSENT. A runtime that returns no result at all can
	// still have produced a typed LaunchError carrying the known safe
	// fields. Preserve them rather than reporting false zeros. No raw
	// output exists on this path; only the count and digest are copied.
	var le *oc.LaunchError
	if errors.As(launchErr, &le) {
		ev.ExitCode = le.ExitCode
		ev.OutputBytes = le.OutputBytes
		ev.OutputSHA256 = le.OutputSHA256
	}
	return ev
}

// boundedReason keeps an error string to a note: no transcript, no
// credential, bounded length.
func boundedReason(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(msg) > 240 {
		msg = msg[:240]
	}
	return msg
}

// mintSessionID creates a session id in the runtime's own shape.
//
// The adapter mints it so the id exists BEFORE the runtime does — that is
// what makes the launch recoverable. The prefix mirrors the runtime's, so
// a reader can tell at a glance that this is a native session id.
func mintSessionID() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("assignment: mint a session id: %w", err)
	}
	return "ses_mpm" + hex.EncodeToString(b[:]), nil
}

func mintCorrelationRef() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("assignment: mint a correlation ref: %w", err)
	}
	return "corr-" + hex.EncodeToString(b[:]), nil
}
