// Package dispatch is Continuity 001's bounded autonomous dispatcher.
//
// # WHAT THIS IS
//
// A COORDINATOR, not a runtime. It finds work that MPM already knows
// about, proves the work is authorised, claims it through the fenced claim
// API, and hands it to PR 3's OpenCode adapter. It never runs a model
// itself, and it never creates an assignment.
//
// MPM coordinates. OpenCode executes.
//
// # WHAT IT IS NOT
//
//   - It is not a second agent runtime. The only execution path is PR 3's
//     adapter, and the only runtime behind that adapter is OpenCode.
//   - It is not a daemon. It is a bounded `run once`, callable by an
//     operator or by a scheduler tick that has been explicitly enabled.
//   - It is not a root creator. A root is always operator-created; the
//     dispatcher cannot create one and cannot grant approval.
//   - It does not invent work. An assignment that is not eligible is
//     DEFERRED with a reason, never made eligible because the dispatcher
//     had nothing else to do.
//
// # THE TWO THINGS IT MUST NEVER DO
//
//  1. LAUNCH WORK THE OPERATOR DID NOT AUTHORISE. Every dispatch is
//     justified by the root's own permitted operations and eligible
//     recipients, and the service's own budget checks bind it.
//  2. DISPATCH THE SAME WORK TWICE. A session that might still be running
//     is a blocker, not an invitation.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/flowbyte-com/mpm-core/assignment"
)

// The reason codes the dispatcher reports.
//
// They are a CLOSED set, because an operator reads them and a report that
// can say anything says nothing. Every refused, deferred or completed
// dispatch carries exactly one of them.
const (
	// ReasonDisabled says the dispatcher is off. It is not an error: the
	// dispatcher is OFF unless an operator explicitly enables it.
	ReasonDisabled = "dispatch_disabled"

	// ReasonNoEligible says nothing was eligible. It is a normal
	// outcome, not a failure.
	ReasonNoEligible = "no_eligible_assignment"

	// ReasonBudgetExhausted says the tree's aggregate budget is spent.
	ReasonBudgetExhausted = "budget_exhausted"

	// ReasonLeaseHeld says a LIVE LEASE blocks the claim.
	//
	// The lease may be the dispatcher's own — left behind by a run that
	// died after the claim — or another holder's. Either way the answer
	// is the same: the work is not reclaimed while the lease lives, and
	// the detail says who holds it and until when. The dispatcher does
	// NOT try to recognise its own lease by the owner string: the
	// operator's one-shot and the scheduler's tick share that string,
	// and a second one would then resume an attempt the first one is
	// still working on.
	ReasonLeaseHeld = "lease_held"

	// ReasonLeaseInsufficient says the lease the dispatcher holds does
	// not outlive the runtime window, so the result could not be
	// recorded.
	//
	// It is NOT `lease_held`: that code says the work is under a lease
	// and must wait, and this one says the dispatcher HAS the work and
	// cannot do it safely. Two different facts, two different codes —
	// an operator reading "lease_held" would go looking for a holder
	// to wait for, and there is none.
	ReasonLeaseInsufficient = "lease_insufficient"

	// ReasonStaleEpoch says the claim epoch moved under the dispatcher.
	ReasonStaleEpoch = "stale_epoch"

	// ReasonSessionStillRunning says a recorded session may still be
	// working. It blocks redispatch into the same workspace.
	ReasonSessionStillRunning = "session_still_running"

	// ReasonRuntimeUnavailable says the runtime could not be reached.
	ReasonRuntimeUnavailable = "runtime_unavailable"

	// ReasonAmbiguous says the adapter could not determine whether the
	// runtime did the work. It is never folded into a success.
	ReasonAmbiguous = "ambiguous_execution"

	// ReasonReviewerUnavailable says the reviewer could not run: its
	// definition, its sandbox or its plugin is unavailable.
	ReasonReviewerUnavailable = "reviewer_unavailable"

	// ReasonRecoveryRequired says an outcome needs an operator: an
	// outstanding attempt whose effects cannot be determined.
	ReasonRecoveryRequired = "recovery_required"

	// ReasonOperatorGatePending says the work is waiting for the
	// operator's gate, not for the dispatcher.
	ReasonOperatorGatePending = "operator_gate_pending"

	// ReasonDispatched says the dispatcher handed the work to the
	// adapter. It is not a completion.
	ReasonDispatched = "dispatched"

	// ReasonCompleted says the assignment reached its terminal state.
	ReasonCompleted = "completed"

	// ReasonRefused says the work was refused by a rule.
	ReasonRefused = "refused"

	// ReasonDeferred says the work was not attempted this round and
	// should be considered again later.
	ReasonDeferred = "deferred"

	// ReasonFailed says the assignment failed in a way the ledger
	// records.
	ReasonFailed = "failed"
)

// Outcome is what happened to ONE assignment.
//
// It keeps the four things apart that the brief asks to be kept apart:
// the RUNTIME's report, the REVIEWER's evidence, the REVIEWER's verdict
// and the OPERATOR's approval are four separate facts, and this struct
// never collapses them.
type Outcome struct {
	// AssignmentID is the assignment the outcome is about.
	AssignmentID string `json:"assignment_id"`
	// RootID is the tree it belongs to.
	RootID string `json:"root_id"`
	// Role is implementer or reviewer.
	Role string `json:"role"`
	// Reason is the closed-set code.
	Reason string `json:"reason"`
	// Detail is a bounded human explanation. It never carries a
	// credential, a prompt or a transcript.
	Detail string `json:"detail,omitempty"`
	// AttemptID is the durable attempt, when there is one.
	AttemptID string `json:"attempt_id,omitempty"`
	// NativeSessionID is the runtime session, when there is one.
	NativeSessionID string `json:"native_session_id,omitempty"`
	// Epoch is the claim epoch the dispatcher held.
	Epoch int64 `json:"epoch,omitempty"`
	// RuntimeOutcome is the RUNTIME's own report. It is empty when the
	// runtime said nothing, and it is NOT the assignment's state.
	RuntimeOutcome string `json:"runtime_outcome,omitempty"`
	// AssignmentState is the assignment's state after the dispatch.
	AssignmentState string `json:"assignment_state,omitempty"`
	// ReviewedRevision and FindingsSHA256 are the REVIEWER's captured
	// evidence, when the dispatch was a review. They are the host's
	// capture, not a digest the reviewer supplied.
	ReviewedRevision string `json:"reviewed_revision,omitempty"`
	FindingsSHA256   string `json:"findings_sha256,omitempty"`
}

// Report is the machine-readable result of one dispatcher run.
//
// It is deliberately a VALUE: it carries no live runtime, no open session
// and no handle to anything that could be mistaken for a live system.
type Report struct {
	// Enabled says whether the dispatcher was enabled at all.
	Enabled bool `json:"enabled"`
	// Dispatched, Completed, Refused, Deferred and Ambiguous are the
	// counts by outcome.
	Dispatched int `json:"dispatched"`
	Completed  int `json:"completed"`
	Refused    int `json:"refused"`
	Deferred   int `json:"deferred"`
	Ambiguous  int `json:"ambiguous"`
	// Outcomes is the per-assignment detail, in the order the
	// dispatcher considered the assignments.
	Outcomes []Outcome `json:"outcomes"`
	// StartedAt and FinishedAt are unix seconds.
	StartedAt  int64 `json:"started_at"`
	FinishedAt int64 `json:"finished_at"`
}

// RuntimeFactory builds the runtime an assignment needs.
//
// The dispatcher does NOT know how OpenCode is configured; the caller
// supplies that. It is an interface so the tests can inject a fake and so
// the real boundary has exactly one implementation.
//
// The two return values are the two boundaries: a worker runtime for an
// implementer, and a review runtime for a reviewer. Exactly one is
// non-nil for any given role.
type RuntimeFactory func(ctx context.Context, a *assignment.Assignment, workspace string) (
	assignment.Runtime, assignment.ReviewRuntime, error)

// Engine is the dispatcher.
//
// It holds the assignment service, the operator's configuration and the
// factory that builds a runtime. It is safe for concurrent use: the
// concurrency budget lives in the SERVICE, not here.
type Engine struct {
	store   *assignment.Store
	cfg     Config
	factory RuntimeFactory
	now     func() time.Time
}

// NewEngine builds a dispatcher.
//
// A disabled configuration is ACCEPTED — that is the safe default — and a
// configuration that is enabled but INVALID is refused. A dispatcher that
// guesses at its own limits is worse than no dispatcher.
func NewEngine(store *assignment.Store, cfg Config, factory RuntimeFactory) (*Engine, error) {
	if store == nil {
		return nil, fmt.Errorf("dispatch: the dispatcher needs the assignment service")
	}
	if factory == nil {
		return nil, fmt.Errorf("dispatch: the dispatcher needs a runtime factory")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Engine{
		store:   store,
		cfg:     cfg,
		factory: factory,
		now:     func() time.Time { return time.Now().UTC() },
	}, nil
}

// Config returns the engine's configuration.
func (e *Engine) Config() Config { return e.cfg }

// Now returns the engine's clock.
func (e *Engine) Now() time.Time { return e.now() }

// RunOnce performs ONE bounded dispatch pass.
//
// It is bounded in every direction that matters: it dispatches at most
// `Concurrency` assignments, it never polls, and it never retries inside
// the call. A run that finds nothing returns a report that says so.
//
// The order of the run is fixed:
//
//  1. RECOVER, before dispatching anything. An outstanding attempt from a
//     previous run is reconciled or reported before new work starts, so a
//     crash cannot leave work half-done and unnoticed.
//  2. DISCOVER the eligible assignments, in a deterministic order.
//  3. For each, CLAIM it through the fenced claim API, then hand it to
//     PR 3's adapter.
//
// The dispatcher NEVER creates an assignment and NEVER grants approval.
func (e *Engine) RunOnce(ctx context.Context) (*Report, error) {
	report := &Report{
		Enabled:   e.cfg.Enabled,
		StartedAt: e.now().Unix(),
	}
	defer func() { report.FinishedAt = e.now().Unix() }()

	// OFF MEANS OFF. Not one row is read and not one runtime is built,
	// so a disabled dispatcher cannot accidentally start inference.
	if !e.cfg.Enabled {
		report.Outcomes = append(report.Outcomes, Outcome{
			Reason: ReasonDisabled,
			Detail: "autonomous dispatch is not enabled; no work was discovered and no runtime was built",
		})
		return report, nil
	}

	// 1. RECOVER.
	//
	// The recovery pass runs FIRST, so a crash cannot leave work
	// half-done and unnoticed. It reports only what it BLOCKED and what
	// it RECONCILED: an assignment it merely observed is left to the
	// dispatch path, so every assignment appears in the report once.
	recovery, blocked, err := e.recover(ctx)
	if err != nil {
		return report, err
	}
	report.Outcomes = append(report.Outcomes, recovery...)
	// THE RECOVERED COUNTS.
	//
	// A reconciled completion is a `completed` and NOT a `dispatched`:
	// this run did not hand the work to the adapter — the runtime had
	// already done it, and the recovery pass only recorded what the
	// runtime said. So `completed` can be larger than `dispatched` in
	// a run that only recovered, and that is the point of recovery:
	// the work finished even though nobody launched it this time.
	for _, o := range recovery {
		switch o.Reason {
		case ReasonCompleted:
			report.Completed++
		case ReasonAmbiguous:
			report.Ambiguous++
		case ReasonFailed:
			// The attempt failed. It was not dispatched by this run.
		default:
			report.Deferred++
		}
	}

	// 2. DISCOVER.
	candidates, err := e.store.DispatchCandidates(assignment.DispatchQuery{
		Limit: e.cfg.CandidateLimit,
	})
	if err != nil {
		return report, err
	}
	if len(candidates) == 0 && len(recovery) == 0 {
		report.Outcomes = append(report.Outcomes, Outcome{
			Reason: ReasonNoEligible,
			Detail: "no assignment is eligible for dispatch",
		})
		return report, nil
	}

	// 3. CLAIM AND DISPATCH.
	//
	// `attempts` counts every dispatch this run ATTEMPTED, whatever its
	// outcome, because a dispatch that FAILED still spent a claim and a
	// session: counting only the successes would let a run launch
	// without limit as long as every launch failed.
	attempts := 0
	for _, candidate := range candidates {
		// THE RECOVERY PASS ALREADY REFUSED THIS ONE.
		//
		// A session that may still be working blocks a second writer
		// into the same workspace, and the recovery pass established
		// that by asking the runtime. The dispatcher does not try
		// anyway: PR 3's own fence would refuse the launch, and a
		// refusal that costs a claim is worse than a clear report.
		//
		// It is NOT COUNTED AGAIN here: the recovery pass already
		// counted it as deferred.
		if _, ok := blocked[candidate.ID]; ok {
			continue
		}
		if attempts >= e.cfg.Concurrency {
			report.Deferred++
			report.Outcomes = append(report.Outcomes, Outcome{
				AssignmentID: candidate.ID,
				RootID:       candidate.RootID,
				Role:         candidate.Role,
				Reason:       ReasonDeferred,
				Detail:       "this run's concurrency limit is reached; the assignment is left for a later run",
			})
			continue
		}
		reason, detail := e.preflight(candidate)
		if reason != "" {
			if reason == ReasonDeferred {
				report.Deferred++
			} else {
				report.Refused++
			}
			report.Outcomes = append(report.Outcomes, Outcome{
				AssignmentID: candidate.ID,
				RootID:       candidate.RootID,
				Role:         candidate.Role,
				Reason:       reason,
				Detail:       detail,
			})
			continue
		}

		outcome, err := e.dispatchOne(ctx, candidate)
		if err != nil {
			return report, err
		}
		report.Outcomes = append(report.Outcomes, outcome)
		attempts++
		// THE COUNTS, AND WHAT EACH ONE MEANS.
		//
		//   dispatched  the dispatcher handed the work to the adapter
		//   completed  the assignment reached its terminal state
		//   refused    a rule said no; nothing was launched
		//   deferred   the work was not attempted this round
		//   ambiguous  the runtime's outcome could not be determined
		//
		// A claim that was REFUSED is not a dispatch: nothing was
		// launched, so it is refused and not dispatched.
		//
		// A FAILED attempt is a dispatch that did not complete: the
		// work WAS handed to the adapter, and it failed. It is not a
		// refusal — nothing refused it — so it is dispatched and
		// neither completed nor refused. The per-assignment reason
		// says `failed`, and that is where an operator reads it.
		switch outcome.Reason {
		case ReasonCompleted:
			report.Dispatched++
			report.Completed++
		case ReasonAmbiguous:
			report.Dispatched++
			report.Ambiguous++
		case ReasonFailed:
			report.Dispatched++
		case ReasonDispatched:
			report.Dispatched++
		case ReasonDeferred:
			report.Deferred++
		default:
			report.Refused++
		}
	}
	return report, nil
}

// preflight says why an assignment must not be dispatched this round.
//
// The order is the contract:
//
//  1. AUTHORISATION — the tree's own permitted operations and eligible
//     recipients. A refusal here is not "not yet": it is "not ever",
//     and waiting does not change it.
//  2. DEPENDENCIES — the work's own preconditions (a review's target,
//     a tree that has not been withdrawn).
//  3. THE DISPATCHER'S OWN POLICY — the attempt cap and the backoff,
//     which are what stop a repeatedly failing assignment from
//     monopolising the dispatcher.
//
// The checks that are the SERVICE's business — the budget, the lease,
// the epoch — are enforced by the writes the dispatcher goes on to make,
// so they are not re-implemented here.
func (e *Engine) preflight(a *assignment.Assignment) (string, string) {
	if a.Role != assignment.RoleImplementer && a.Role != assignment.RoleReviewer {
		return ReasonRefused, "the assignment has no execution path"
	}
	if reason, detail := e.authorisationReason(a); reason != "" {
		return reason, detail
	}
	if reason, detail := e.dependencyReason(a); reason != "" {
		return reason, detail
	}
	return e.deferReason(a)
}

// deferReason says why an assignment must not be dispatched this round.
//
// It is the dispatcher's OWN policy, and it is what stops a repeatedly
// failing assignment from monopolising the dispatcher: an assignment that
// has used up its dispatch attempts, or that failed too recently, is
// DEFERRED rather than retried forever.
func (e *Engine) deferReason(a *assignment.Assignment) (string, string) {
	attempts, err := e.store.AttemptsFor(a.ID)
	if err != nil {
		return ReasonDeferred, "the attempts could not be read: " + err.Error()
	}
	if e.cfg.MaxDispatchAttempts > 0 && len(attempts) >= e.cfg.MaxDispatchAttempts {
		return ReasonDeferred, fmt.Sprintf(
			"the assignment has used its dispatch attempts (%d of %d)",
			len(attempts), e.cfg.MaxDispatchAttempts)
	}
	if e.cfg.RetryBackoffSeconds > 0 && len(attempts) > 0 {
		last := attempts[len(attempts)-1]
		if last.LaunchState == assignment.SessionFailed && last.UpdatedAt > 0 {
			since := e.now().Unix() - last.UpdatedAt
			if since < int64(e.cfg.RetryBackoffSeconds) {
				return ReasonDeferred, fmt.Sprintf(
					"the assignment failed %ds ago and the backoff is %ds",
					since, int64(e.cfg.RetryBackoffSeconds))
			}
		}
	}
	return "", ""
}

// dispatchOne claims one assignment and hands it to the adapter.
//
// The ORDER is the contract: the claim comes FIRST, so the dispatcher
// holds fenced ownership before any session exists, and the adapter's own
// epoch fence then binds the launch to that claim.
func (e *Engine) dispatchOne(ctx context.Context, a *assignment.Assignment) (Outcome, error) {
	outcome := Outcome{
		AssignmentID: a.ID,
		RootID:       a.RootID,
		Role:         a.Role,
	}

	// THE TREE'S AUTHORISATION IS CHECKED BEFORE THE CLAIM.
	//
	// The budgets are NOT checked here: they are the SERVICE's business,
	// and the service enforces them atomically inside the claim's own
	// UPDATE. A dispatcher-side count would be a second, weaker copy of
	// a rule the write already enforces — and a copy that could be
	// crossed between the read and the write. So the dispatcher claims,
	// and the claim is what refuses a tree whose concurrency budget is
	// spent.

	actor := assignment.Actor{Address: a.Recipient, Kind: "agent"}

	// THE CLAIM IS ALWAYS TAKEN, AND A LIVE LEASE IS NEVER RECLAIMED.
	//
	// The dispatcher does not try to recognise "a lease I already hold"
	// by the OWNER STRING: two dispatchers — the operator's one-shot and
	// the scheduler's tick — share that string, and a second one would
	// then resume an attempt the first one is still working on, and
	// both would prompt the same recorded session.
	//
	// So the dispatcher always claims, and PR 2's compare-and-swap
	// decides: one claim per epoch, whoever asks first. A live lease is
	// REFUSED, and the assignment waits for the lease to expire — which
	// is the safe direction, because an expired lease is not evidence
	// that the old process stopped and a live lease is not evidence
	// that it did.
	var err error
	claimed, err := e.store.Claim(assignment.ClaimParams{
		ID:            a.ID,
		ExpectedEpoch: a.ClaimEpoch,
		Owner:         e.cfg.Owner,
		LeaseSeconds:  e.cfg.LeaseSeconds,
		Actor:         actor,
	})
	if err != nil {
		return e.classifyClaimRefusal(outcome, err), nil
	}
	outcome.Epoch = claimed.ClaimEpoch

	// THE ASSIGNMENT IS STARTED, because the dispatcher now holds the
	// claim and is the party that will do the work.
	//
	// The worker's path would start it itself; the REVIEW path requires
	// the assignment to be in_progress before it will run, so the
	// dispatcher starts it for both. Starting it here is the honest
	// order: the claim is taken, the work is declared in progress, and
	// only then does a session exist.
	if claimed.State == assignment.StateAccepted {
		started, serr := e.store.Start(assignment.FencedParams{
			ID:    claimed.ID,
			Epoch: claimed.ClaimEpoch,
			Actor: assignment.Actor{Address: claimed.Recipient, Kind: "agent"},
		})
		if serr != nil {
			outcome.Reason = classifyAdapterRefusal(serr)
			outcome.Detail = serr.Error()
			return outcome, nil
		}
		claimed = started
	}

	// THE LEASE, JUDGED AGAIN ON THE CLAIM THE RUN ACTUALLY HOLDS.
	//
	// The configuration already refuses a lease that cannot cover the
	// runtime window, so this can only fire if the store clamped the
	// lease. It is checked anyway: a launch that cannot be recorded is
	// the one failure this whole design exists to prevent.
	if !e.leaseCovers(claimed) {
		outcome.Reason = ReasonLeaseInsufficient
		outcome.Detail = fmt.Sprintf(
			"the lease ends at %d and the runtime window needs %ds of headroom",
			leaseExpiry(claimed), int64(e.cfg.RuntimeTimeoutSeconds))
		return outcome, nil
	}

	workspace := ""
	if claimed.Role != assignment.RoleReviewer {
		var werr error
		workspace, werr = e.workspaceFor(claimed)
		if werr != nil {
			outcome.Reason = ReasonRuntimeUnavailable
			outcome.Detail = werr.Error()
			return outcome, nil
		}
	}

	worker, reviewer, err := e.factory(ctx, claimed, workspace)
	if err != nil {
		outcome.Reason = ReasonRuntimeUnavailable
		outcome.Detail = err.Error()
		return outcome, nil
	}

	if claimed.Role == assignment.RoleReviewer {
		return e.dispatchReview(ctx, claimed, reviewer, outcome)
	}
	return e.dispatchWorker(ctx, claimed, worker, workspace, outcome)
}

// leaseCovers says the lease outlives the runtime window.
//
// The margin is deliberate: the runtime's own timeout plus a margin for
// the adapter to observe the outcome and write it down. Work that cannot
// fit inside the lease is refused, not started.
func (e *Engine) leaseCovers(a *assignment.Assignment) bool {
	if a.LeaseExpiresAt == nil {
		return false
	}
	need := int64(e.cfg.RuntimeTimeoutSeconds) + int64(e.cfg.LeaseMarginSeconds)
	return *a.LeaseExpiresAt-e.store.Now() >= need
}

// leaseExpiry reads an assignment's lease expiry, or zero.
func leaseExpiry(a *assignment.Assignment) int64 {
	if a.LeaseExpiresAt == nil {
		return 0
	}
	return *a.LeaseExpiresAt
}

// classifyClaimRefusal turns a claim refusal into a reason code.
//
// The claim API's refusals are already precise; this maps them onto the
// dispatcher's closed set so an operator reads one vocabulary.
func (e *Engine) classifyClaimRefusal(outcome Outcome, err error) Outcome {
	switch {
	case errors.Is(err, assignment.ErrBudgetExhausted):
		outcome.Reason = ReasonBudgetExhausted
	case errors.Is(err, assignment.ErrLeaseHeld), errors.Is(err, assignment.ErrAlreadyClaimed):
		outcome.Reason = ReasonLeaseHeld
	case errors.Is(err, assignment.ErrStaleClaim):
		outcome.Reason = ReasonStaleEpoch
	case errors.Is(err, assignment.ErrIllegalTransition):
		outcome.Reason = ReasonOperatorGatePending
	default:
		outcome.Reason = ReasonRefused
	}
	outcome.Detail = err.Error()
	return outcome
}
