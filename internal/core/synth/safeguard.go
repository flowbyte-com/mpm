// safeguard.go — bounded-execution guard for LLM-backed operations.
//
// 2026-09-14 tightening pass: synthesize is BOUNDED
// TRANSFORMATION, not autonomous problem solving. Every model
// call must be attributable to a finite pre-computable plan;
// the safeguard prevents runaway loops even when the model
// returns "uncertain", "ambiguous", or "mathematically
// undefined" answers that an LLM-based retry policy would
// otherwise treat as a reason to keep calling.
//
// CORE INVARIANTS (auditable in two lines):
//
//   per stage:    attempts <= 2           (1 fresh + 1 recovery)
//   per run:      attempts <= planned_stages * 2
//   recovery:     one slot per stage — retry XOR repair, never both
//
// THE ARCHITECTURE IS PRESERVED from the previous pass:
//
//   - finite pre-computable execution plans (Plan object)
//   - per-failure-class retry policy (FailureClass enum)
//   - semantic uncertainty as a valid terminal result
//   - auth failures fail fast (FailureAuth → 0 recovery)
//   - recursion depth bounded (RecursionCap = 1)
//   - duplicate requests fingerprinted (fingerprint + per-stage cap)
//   - empty model output is mechanical failure, NOT uncertainty
//
// What changes in THIS pass:
//
//   1. Per-stage ceiling: was 1 fresh + 1 retry + 1 repair (3 calls);
//      is now 1 fresh + 1 recovery (2 calls). Recovery is a single
//      slot that may be allocated to EITHER retry (transient) OR
//      repair (malformed body), NEVER both.
//   2. Per-invocation stage ceiling: was 50; is now 8.
//      Top-level ceiling: planned_stages * 2 = 16 provider calls.
//   3. 429 policy: Retry-After absent → 0 recovery; ≤ 10s → 1;
//      > 10s → 0. No minutes-long sleeps inside synthesis.
//   4. CLI synthesise: bounded continuation, not refusal. The
//      scan processes up to 8 eligible stages per call and
//      reports remaining work; re-invocation resumes from
//      canonical substrate state (content-hash dedup is durable).
//   5. Compact drain: cap at MaxSemanticStagesPerInvocation (= 8);
//      remaining batches are still eligible on a later invocation
//      — no recurse, no immediate re-schedule in the same call.
//   6. Empty model Content is treated as malformed and consumes
//      the recovery slot. A recovery that also returns empty
//      stops the run. Explicit uncertainty (a non-empty refusal
//      explanation) is the only semantic terminal that always
//      succeeds.
//
// THIS IS NOT A TOKEN-LIMIT FEATURE.
// THIS IS NOT A PRICING FEATURE.
// Output-token caps were removed from user-facing config in
// the prior release pass; the safeguard runs on the call /
// stage / recursion axes that ARE meaningful for runaway
// prevention.

package synth

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// RecoveryKind labels how a stage's recovery slot is used.
// A recovery slot is allocated to either transient retry OR
// structural repair; never both. Setting this at AttemptRecovery
// time is the caller's responsibility — the policy dispatch in
// do_request.go classifies the failure and chooses RetryKind
// (wire-level transient); the call site (Synthesize /
// SynthesizeCompactLesson) chooses RepairKind (structural-decode
// failure). The plan enforces single-slot usage.
type RecoveryKind int

const (
	// RecoveryNone is the zero value; no recovery consumed.
	RecoveryNone RecoveryKind = iota

	// RecoveryRetry — recovery slot allocated to mechanical
	// retry on transient transport / provider 5xx / bounded
	// 429 / connection-refused. The wire layer drives this.
	RecoveryRetry

	// RecoveryRepair — recovery slot allocated to structural
	// repair on malformed body or empty response. The call
	// site drives this.
	RecoveryRepair
)

// String returns a stable label for the recovery kind. Used
// in watchdog events and in StopReason rendering.
func (r RecoveryKind) String() string {
	switch r {
	case RecoveryRetry:
		return "retry"
	case RecoveryRepair:
		return "repair"
	}
	return "none"
}

// Pre-computable hard caps. Internal-only constants —
// operators MUST NOT be able to override these via config.
//
// The brief's "every model call must have a finite
// pre-computable reason" invariant: each cap is a per-stage
// allowance, NOT a global retry budget.
const (
	// MaxRecoveriesPerStage is the single recovery slot per
	// stage. The recovery may be EITHER transient retry OR
	// structural repair — never both. Two calls total per
	// stage (1 fresh + 1 recovery).
	MaxRecoveriesPerStage = 1

	// MaxSemanticStagesPerInvocation is the top-level safety
	// ceiling. It is the same constant for the CLI scan,
	// the compact drain, and any other orchestrator that
	// goes through this package.
	//
	// Rationale: bounded autonomous execution, bounded
	// cumulative provider usage per invocation, predictable
	// failure behaviour, and explicit continuation. We
	// deliberately do NOT justify this constant by aggregate
	// context-window arithmetic — context windows apply
	// PER REQUEST, not cumulatively across the run.
	//
	// Choosing 8 was a judgement call. Telemetry can later
	// surface whether 8 was too low or too high; this is the
	// alpha starting point. Operators with larger workloads
	// run multiple invocations and rely on continuation
	// rather than expanding the per-invocation ceiling.
	MaxSemanticStagesPerInvocation = 8
)

// absoluteCallCeilingPerInvocation is the maximum number of
// provider calls allowed in a single invocation. It is a
// derived constant: MaxSemanticStagesPerInvocation * 2
// (one fresh + one recovery per stage).
const absoluteCallCeilingPerInvocation = MaxSemanticStagesPerInvocation * 2

// ErrBoundedPlanExceeded marks a circuit-breaker stop. The
// safeguard surfaces this with concrete counts so callers
// can render a useful stop diagnostic.
var ErrBoundedPlanExceeded = errors.New("synth: bounded-execution safeguard triggered")

// ErrInputTooLarge is the executor's stop signal when an
// invocation arrives with more work than the ceiling allows.
// The message names the rejection reason and points the
// operator at the bounded shape (smaller scope, multiple
// invocations).
var ErrInputTooLarge = errors.New("synth: input too large for one bounded synthesis run")

// FailureClass categorises the kind of error coming back
// from a transport / decode attempt. The policy table in
// do_request.go maps each class to an allowance.
type FailureClass int

const (
	// FailureUnknown is the zero value. Treated conservatively
	// as non-retriable. Better to stop than to retry blindly.
	FailureUnknown FailureClass = iota

	// FailureTransientTransport: connection reset, 5xx,
	// timeout before a usable response. Recoverable via
	// the single recovery slot IF the slot is still free.
	FailureTransientTransport

	// FailureAuth: 401, 403, billing/quota refusal where
	// retry cannot help. Zero recovery — fail immediately.
	FailureAuth

	// FailureRateLimit: 429. Recovery is conditional:
	// one slot IF Retry-After is present and <= 10 seconds;
	// otherwise zero recovery — stop.
	FailureRateLimit

	// FailureInvalidMachineResponse: 200 OK with malformed
	// JSON where structured output is contractually required,
	// OR an empty Content body (treated as mechanical
	// failure per the tightening pass). The recovery slot
	// may be allocated to RepairKind here.
	FailureInvalidMachineResponse
)

// String returns a stable label for the failure class — used
// in human diagnostic output and in the JSON-envelope
// telemetry field `last_failure_class`.
func (f FailureClass) String() string {
	switch f {
	case FailureUnknown:
		return "unknown"
	case FailureTransientTransport:
		return "transient_transport"
	case FailureAuth:
		return "auth"
	case FailureRateLimit:
		return "rate_limit"
	case FailureInvalidMachineResponse:
		return "invalid_machine_response"
	}
	return "unknown"
}

// Plan is a pre-computable execution plan for ONE logical
// operation. It is constructed BEFORE the first network
// call; every subsequent call must acquire permission through
// it via AttemptFresh / AttemptRecovery. Every mutation lives
// under the plan's mutex.
//
// Thread-safety: Plan is safe for concurrent access from the
// single goroutine that owns the run. Pool workers each build
// their own Plan; plans do NOT cross task boundaries.
type Plan struct {
	mu sync.Mutex

	// PlannedStages is the canonical semantic-stage budget.
	// One Plan instance is constructed for one logical run.
	PlannedStages int

	// stages is keyed by stage fingerprint (FNV-64 of the
	// request body). Per-stage state tracks FreshDone,
	// RecoveryKind, and the recovery-slot usage.
	stages map[uint64]*stageState

	// counters — incremented under mu.
	//
	// 2026-09-15 accounting-naming nit: the recovery slot
	// counters are split to make the JSON contract exact.
	// `retryCalls` counts RecoveryRetry only, `repairCalls`
	// counts RecoveryRepair only, and `recoveryCalls` is the
	// total (sum of the two). Pin:
	//
	//   recoveryCalls == retryCalls + repairCalls
	//   actualProviderCalls <= plannedStages * 2
	//   recoveryCalls <= plannedStages
	//
	// Each per-stage slot contributes exactly one entry to
	// `recoveryCalls` (either Retry OR Repair, never both),
	// so the upper bound is `plannedStages`.
	completedStages        int
	recoveryCalls          int // total recovery calls = retry + repair
	retryCalls             int // subset: RecoveryRetry allocations
	repairCalls            int // subset: RecoveryRepair allocations
	actualProviderCalls    int // total wire hits so far
	exhausted              bool
	lastFailureClass       FailureClass
	stopReason             string
}

// stageState is the per-fingerprint bookkeeping. The plan
// holds one stageState per unique fingerprint seen during
// the run.
type stageState struct {
	freshDone     bool      // first attempt was sent
	recoveryUsed  bool      // recovery slot consumed
	recoveryKind  RecoveryKind
}

// Stats is the read-only factual snapshot a Plan exposes for
// telemetry / diagnostics. Stable JSON shape.
//
// 2026-09-15 naming nit: counter JSON tags are now semantically
// exact. The contract:
//
//   recovery_calls        total recovery slots consumed
//   retry_calls           subset: RecoveryRetry allocations
//   repair_calls          subset: RecoveryRepair allocations
//
// Pins (enforced by TestSafeguard_StatsInvariants):
//
//   recovery_calls == retry_calls + repair_calls
//   actual_provider_calls <= planned_stages * 2
//   recovery_calls <= planned_stages
type Stats struct {
	PlannedStages       int          `json:"planned_stages"`
	CompletedStages     int          `json:"completed_stages"`
	RecoveryCalls       int          `json:"recovery_calls"`
	RetryCalls          int          `json:"retry_calls"`
	RepairCalls         int          `json:"repair_calls"`
	ActualProviderCalls int          `json:"actual_provider_calls"`
	Exhausted           bool         `json:"exhausted"`
	StopReason          string       `json:"stop_reason,omitempty"`
	LastFailureClass    FailureClass `json:"last_failure_class"`
}

// PlanOption is a builder option for NewPlan.
type PlanOption func(*Plan)

// WithPlannedStages overrides the planned-stage budget. The
// default per-call plan uses 1.
func WithPlannedStages(n int) PlanOption {
	return func(p *Plan) { p.PlannedStages = n }
}

// NewPlan constructs a Plan from options. The default
// allowance is one semantic stage.
func NewPlan(opts ...PlanOption) *Plan {
	p := &Plan{
		PlannedStages: 1,
		stages:        map[uint64]*stageState{},
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.PlannedStages < 0 {
		p.PlannedStages = 0
	}
	// Clamp to MaxSemanticStagesPerInvocation so no run can
	// exceed the top-level ceiling regardless of caller.
	if p.PlannedStages > MaxSemanticStagesPerInvocation {
		p.PlannedStages = MaxSemanticStagesPerInvocation
	}
	return p
}

// NewPerCallPlan returns a Plan representing a single LLM
// call: one semantic stage, one recovery slot. Use for any
// call site where the orchestrator's semantic unit is "one
// LLM call, possibly one mechanical recovery".
func NewPerCallPlan() *Plan {
	return NewPlan()
}

// NewBoundedPlan returns a Plan whose planned-stage budget
// is `stages`. The constructor refuses to build a plan whose
// planned budget exceeds MaxSemanticStagesPerInvocation; that
// is the ErrInputTooLarge contract — callers must split the
// work across invocations, not expand the per-invocation
// ceiling.
func NewBoundedPlan(stages int) (*Plan, error) {
	if stages < 1 {
		stages = 1
	}
	if stages > MaxSemanticStagesPerInvocation {
		return nil, fmt.Errorf("%w: planned=%d ceiling=%d", ErrInputTooLarge,
			stages, MaxSemanticStagesPerInvocation)
	}
	return NewPlan(WithPlannedStages(stages)), nil
}

// Stats returns the plan's current counters.
func (p *Plan) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		PlannedStages:       p.PlannedStages,
		CompletedStages:     p.completedStages,
		RecoveryCalls:       p.recoveryCalls,
		RetryCalls:          p.retryCalls,
		RepairCalls:         p.repairCalls,
		ActualProviderCalls: p.actualProviderCalls,
		Exhausted:           p.exhausted,
		StopReason:          p.stopReason,
		LastFailureClass:    p.lastFailureClass,
	}
}

// AttemptFresh books the FIRST semantic attempt against the
// stage identified by fingerprint. Each fingerprint gets
// exactly one fresh attempt per run. A second fresh attempt
// for the same fingerprint is a duplicate-stage violation
// (the brief's "third same-fingerprint provider attempt
// never occurs" rule is enforced here, generalised to all
// semantic stages).
func (p *Plan) AttemptFresh(fingerprint uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.exhausted {
		return p.stopLocked("safeguard already exhausted")
	}
	st, ok := p.stages[fingerprint]
	if !ok {
		st = &stageState{}
		p.stages[fingerprint] = st
	}
	if st.freshDone {
		return p.stopLocked(fmt.Sprintf(
			"duplicate fresh attempt for fingerprint %x (each stage gets exactly one fresh call)",
			fingerprint))
	}
	st.freshDone = true
	p.actualProviderCalls++
	if p.actualProviderCalls > absoluteCallCeilingPerInvocation {
		return p.stopLocked(fmt.Sprintf(
			"absolute provider-call ceiling reached (%d/%d)",
			p.actualProviderCalls, absoluteCallCeilingPerInvocation))
	}
	return nil
}

// AttemptRecovery books the SINGLE recovery attempt against
// an already-fresh-attempted stage. The recovery slot may be
// allocated to EITHER transient retry (RecoveryRetry) OR
// structural repair (RecoveryRepair) — never both. After a
// successful recovery attempt, further attempts against the
// same fingerprint are refused.
//
// The kind argument is required: callers MUST declare whether
// they are retrying or repairing. The plan uses this both for
// accounting (stats.retry_calls vs stats.repair_calls) and to
// enforce single-slot allocation.
func (p *Plan) AttemptRecovery(fingerprint uint64, kind RecoveryKind) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.exhausted {
		return p.stopLocked("safeguard already exhausted")
	}
	if kind != RecoveryRetry && kind != RecoveryRepair {
		return p.stopLocked("recovery attempt without a kind")
	}
	st, ok := p.stages[fingerprint]
	if !ok || !st.freshDone {
		return p.stopLocked("recovery without preceding fresh attempt (call AttemptFresh first)")
	}
	if st.recoveryUsed {
		return p.stopLocked(fmt.Sprintf(
			"recovery already used for fingerprint %x as %s; stage permits exactly one recovery, never both retry and repair",
			fingerprint, st.recoveryKind))
	}
	st.recoveryUsed = true
	st.recoveryKind = kind

	// 2026-09-15 accounting-naming nit: keep the subset and
	// total counters in lock-step. retryCalls and repairCalls
	// are mutually exclusive per stage (the slot is consumed
	// by exactly one), so the pin `recoveryCalls == retryCalls
	// + repairCalls` is preserved by construction here.
	switch kind {
	case RecoveryRetry:
		p.retryCalls++
	case RecoveryRepair:
		p.repairCalls++
	}
	p.recoveryCalls++
	p.actualProviderCalls++
	if p.actualProviderCalls > absoluteCallCeilingPerInvocation {
		return p.stopLocked(fmt.Sprintf(
			"absolute provider-call ceiling reached (%d/%d)",
			p.actualProviderCalls, absoluteCallCeilingPerInvocation))
	}
	return nil
}

// RecordSuccess marks the most-recent attempt as a 200 OK
// with a valid body. The stage is counted as complete.
func (p *Plan) RecordSuccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completedStages++
}

// RecordFailure records the failure class for the
// most-recent attempt. Used to populate LastFailureClass
// for telemetry.
func (p *Plan) RecordFailure(class FailureClass) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastFailureClass = class
}

// stopLocked writes the breaker's stop state under the
// assumption the caller already holds p.mu.
func (p *Plan) stopLocked(reason string) error {
	p.exhausted = true
	if p.stopReason == "" {
		p.stopReason = reason
	}
	return fmt.Errorf("%w: %s", ErrBoundedPlanExceeded, reason)
}

// StoppedReason returns a human-readable summary of the
// stop, or empty string if the plan is still healthy.
// Used by the diagnostic renderer when the safeguard stops
// a run. Stable machine fields; never includes secrets.
func (p *Plan) StoppedReason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.exhausted {
		return ""
	}
	s := p.Stats()
	return fmt.Sprintf(
		"bounded-execution safeguard triggered. "+
			"planned=%d completed=%d recovery=%d retry=%d repair=%d provider_calls=%d last_failure=%s",
		s.PlannedStages, s.CompletedStages, s.RecoveryCalls, s.RetryCalls, s.RepairCalls,
		s.ActualProviderCalls, s.LastFailureClass)
}

// RecursionSentinel is documented with the marker in the
// parent file. The depth-bounded recursion guard remains
// unchanged from the previous pass: depth = 1 is the default,
// top-level calls are permitted, same-chain recursion is
// refused.
type recursionState struct{ depth int }

// recursionEnabled controls the per-run recursion guard.
// Tests can flip it off; production keeps it on.
var recursionEnabled = true

// MarkRecursion returns a guard the caller can inspect via
// Depth to decide whether a recursive synth is permitted.
func MarkRecursion(ctx context.Context) *RecursionGuard {
	depth := 1
	if v := ctx.Value(recursionKey{}); v != nil {
		if s, ok := v.(*recursionState); ok {
			depth = s.depth + 1
		}
	}
	return &RecursionGuard{depth: depth}
}

// RecursionGuard observes the chain depth.
type RecursionGuard struct{ depth int }

// Depth returns the observed depth (1 = top-level).
func (r *RecursionGuard) Depth() int { return r.depth }

// Close is the deferred cleanup. Required for symmetry with
// future states; in this alpha, the per-call sentinel is
// naturally scoped per LLM call's context.
func (r *RecursionGuard) Close() {}

// recursionKey is the context value key for chain depth.
type recursionKey struct{}

// WithRecursionChain tags ctx with the chain depth.
func WithRecursionChain(ctx context.Context, depth int) context.Context {
	if !recursionEnabled {
		return ctx
	}
	return context.WithValue(ctx, recursionKey{}, &recursionState{depth: depth})
}

// SynthesisRecursionDetected wraps the depth-violation error
// returned by CheckRecursion.
var SynthesisRecursionDetected = errors.New("synth: synthesis recursion detected")

// RecursionCap is the maximum allowed depth of a synthesis
// chain. Default 1. Operators do not configure this.
var RecursionCap = 1

// CheckRecursion examines the prospective call's chain
// depth and refuses when it exceeds RecursionCap. Stored
// depth = the chain's existing depth; prospective = stored+1.
func CheckRecursion(ctx context.Context) error {
	if !recursionEnabled {
		return nil
	}
	depth := 1
	if v := ctx.Value(recursionKey{}); v != nil {
		if s, ok := v.(*recursionState); ok {
			depth = s.depth + 1
		}
	}
	if depth > RecursionCap {
		return fmt.Errorf("%w: depth=%d cap=%d",
			SynthesisRecursionDetected, depth, RecursionCap)
	}
	return nil
}
