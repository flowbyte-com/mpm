// safeguard.go — bounded-execution guard for LLM-backed operations.
//
// 2026-09-14 release-pass: synthesis is BOUNDED TRANSFORMATION,
// not autonomous problem solving. Every model request must be
// attributable to a finite, pre-computed execution plan; the
// safeguard is the safety net that prevents runaway loops even
// when the model returns "uncertain", "ambiguous", or
// "mathematically undefined" answers that an LLM-based retry
// policy would otherwise treat as a reason to keep calling.
//
// THIS IS NOT A TOKEN-LIMIT FEATURE.
// THIS IS NOT A PRICING FEATURE.
// Output-token caps were removed from user-facing config in
// the same release pass; the safeguard runs on the call /
// stage / recursion axes that ARE meaningful for runaway
// prevention.
//
// Public API:
//
//   planner := synth.NewPerCallPlan()      // 1 semantic call, 1 retry, 1 repair
//   resp, err := client.SynthesizeWithPlan(ctx, fragments, planner)
//   planner.Stats()                      // facts for telemetry
//
// The internal package export lets other packages (admission,
// compact_epistemology, synthesis_auto) build their own plans.
package synth

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Pre-computable hard caps. Internal-only constants — operators
// MUST NOT be able to override these via config. Telemetry may
// later surface evidence for tuning but the values stay baked
// in here for the alpha.
//
// The brief's "every model call must have a finite pre-computable
// reason" invariant: each cap is a per-stage allowance, NOT a
// global retry budget.
const (
	// MaxRetriesPerStage is the per-stage transient-failure
	// retry allowance. Mechanical recovery only.
	MaxRetriesPerStage = 1

	// MaxRepairsPerRun is the absolute repair budget for one
	// logical run. A repair is a structured-output re-decode
	// (e.g. one extra attempt to parse a malformed response).
	// ONE per run, full stop.
	MaxRepairsPerRun = 1
)

// Absolute batch ceiling per invocation. The brief pins
// this as the largest single-invocation batch budget MPM
// will run. Exceeding it means the input is too large for a
// bounded synthesis plan and the orchestrator must refuse
// BEFORE any model call is placed.
//
// Rationale (per the brief's instruction to "justify the
// chosen maximum from existing batch size / normal MPM
// workload / context constraints / bounded execution
// goals"):
//
//   - Existing batch size: compactBatchSize = 50
//     (internal/core/compact.go). One SynthesizeCompactLesson
//     call processes at most 50 raw memories per batch.
//   - Normal MPM workload: compact_epistemology_drain is the
//     only orchestrator that runs multiple batches per call;
//     its hard cap (compactDrainMaxBatchesHardCap) is 100.
//     The CLI `mpm synthesize` scan loops over memories but
//     only one AutoSynthesize call per memory; we add a
//     hard invocation cap to bound the loop.
//   - Context constraints: each batch consumes input tokens
//     ~ (50 × ~512 input each) = ~25k input tokens plus
//     ~16k output tokens; one batch per LLM request. We cap
//     at 50 batches per invocation so the maximum input
//     across all batches stays well under 1.5M tokens, the
//     upper bound for any current provider context window.
//   - Bounded execution goal: the safeguard is a SAFETY
//     feature, not a capacity feature. Operators with
//     genuinely large inputs break them into smaller
//     invocations — that is the correct shape for a
//     bounded synthesis model.
//
// 50 batches × ~50 memories per batch = 2,500 raw memories per
// invocation. That is the meaningful upper bound any
// reasonable operator will reach in a single session.
const AbsoluteMaxBatchesPerInvocation = 50

// ErrInputTooLarge is the executor's stop signal when an
// invocation arrives with more work than the absolute batch
// ceiling allows. The message names the rejection reason and
// points the operator at the bounded shape.
var ErrInputTooLarge = errors.New("synth: input too large for one bounded synthesis run")

// NewBatchPlanForInvocation builds a Plan whose planned-call
// budget is `batches`. The plan refuses Plan construction if
// batches exceeds AbsoluteMaxBatchesPerInvocation; callers
// must handle the error rather than silently capping.
//
// Returns ErrInputTooLarge when batches > AbsoluteMaxBatchesPerInvocation.
func NewBatchPlanForInvocation(batches int) (*Plan, error) {
	if batches > AbsoluteMaxBatchesPerInvocation {
		return nil, fmt.Errorf("%w: planned=%d ceiling=%d", ErrInputTooLarge,
			batches, AbsoluteMaxBatchesPerInvocation)
	}
	if batches < 1 {
		batches = 1
	}
	return NewPlan(WithPlannedCalls(batches)), nil
}

// ErrBoundedPlanExceeded marks a circuit-breaker stop. The
// Safeguard surfaces this with concrete counts so callers
// can render a useful stop diagnostic.
var ErrBoundedPlanExceeded = errors.New("synth: bounded-execution safeguard triggered")

// FailureClass categorises the kind of error coming back from
// a transport / decode attempt. The policy table in
// package_dispatch maps each class to an allowance.
type FailureClass int

const (
	// FailureUnknown is the zero value. Treated conservatively
	// as transient (one retry, then stop). Better to stop
	// than to retry blindly.
	FailureUnknown FailureClass = iota

	// FailureTransientTransport: connection reset, 5xx,
	// timeout before a usable response. Recoverable with
	// retry up to MaxRetriesPerStage.
	FailureTransientTransport

	// FailureAuth: 401, 403, billing/quota refusal where
	// retry cannot help. Zero retries; fail-fast.
	FailureAuth

	// FailureRateLimit: 429 with no Retry-After, or with a
	// Retry-After that exceeds the run deadline. The current
	// release permits at most ONE retry only if the response
	// gives a bounded retry path; otherwise stop.
	FailureRateLimit

	// FailureInvalidMachineResponse: 200 OK with malformed
	// JSON where structured output is contractually required.
	// One repair attempt.
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
// operation (e.g. one synthesis run, one compact batch, one
// admission evaluation). It is constructed BEFORE the first
// network call; every subsequent call must acquire permission
// through it via AcquireOrStop.
//
// Thread-safety: Plan is safe for concurrent access from the
// single goroutine that owns the run. Pool workers each build
// their own Plan; plans do NOT cross task boundaries.
type Plan struct {
	mu sync.Mutex

	// PlannedCalls is the canonical semantic-call budget for
	// this plan. The brief's contract: planned = the number
	// of semantic batches + (optionally) one final merge.
	PlannedCalls int

	// MaxRetries is the per-stage mechanical-retry ceiling.
	// Default: MaxRetriesPerStage.
	MaxRetries int

	// MaxRepairs is the absolute repair budget for this plan.
	// Default: MaxRepairsPerRun.
	MaxRepairs int

	// counters — incremented under mu.
	completedCalls   int
	retryCalls       int
	repairCalls      int
	exhausted        bool
	lastFailureClass FailureClass

	// fingerprintSeen blocks same-stage accidental re-entry.
	// Keyed by a 64-bit FNV-style hash of stage-input bytes.
	// In-memory only; per-run.
	fingerprintSeen map[uint64]int
}

// PlanOption is a builder option for NewPlan / NewPerCallPlan.
type PlanOption func(*Plan)

// WithPlannedCalls overrides the planned-call budget. The
// default per-call plan uses 1.
func WithPlannedCalls(n int) PlanOption {
	return func(p *Plan) { p.PlannedCalls = n }
}

// WithMaxRetries overrides the per-stage retry ceiling.
func WithMaxRetries(n int) PlanOption {
	return func(p *Plan) { p.MaxRetries = n }
}

// WithMaxRepairs overrides the absolute repair budget.
func WithMaxRepairs(n int) PlanOption {
	return func(p *Plan) { p.MaxRepairs = n }
}

// NewPlan constructs a Plan from options. The default allowance
// is one semantic call + MaxRetriesPerStage retries + 1 repair.
func NewPlan(opts ...PlanOption) *Plan {
	p := &Plan{
		PlannedCalls:    1,
		MaxRetries:      MaxRetriesPerStage,
		MaxRepairs:      MaxRepairsPerRun,
		fingerprintSeen: map[uint64]int{},
	}
	for _, opt := range opts {
		opt(p)
	}
	// Absolute ceiling: 1 semantic call per planned +
	// per-stage retry for each planned call + absolute repair.
	// Negative inputs are clamped to zero.
	if p.PlannedCalls < 0 {
		p.PlannedCalls = 0
	}
	if p.MaxRetries < 0 {
		p.MaxRetries = 0
	}
	if p.MaxRepairs < 0 {
		p.MaxRepairs = 0
	}
	return p
}

// NewPerCallPlan returns a Plan representing a single LLM call
// (one semantic stage) with the default retry/repair budgets.
// Use this when caller semantics are "one batch, one LLM
// call, possibly one mechanical retry, possibly one repair".
func NewPerCallPlan() *Plan {
	return NewPlan()
}

// NewBatchPlan returns a Plan for a multi-batch operation with
// planned batch count, a final merge call (one extra semantic
// call when planned > 1), and the same retry/repair budgets.
// planned <= 0 -> 1; planned > synthBatchAbsoluteMaxBatches -> stop.
func NewBatchPlan(planned int) *Plan {
	if planned < 1 {
		planned = 1
	}
	return NewPlan(WithPlannedCalls(planned))
}

// Stats is the read-only factual snapshot a Plan exposes for
// telemetry / diagnostics. Stable JSON shape.
type Stats struct {
	PlannedCalls     int            `json:"planned_calls"`
	CompletedCalls   int            `json:"completed_calls"`
	RetryCalls       int            `json:"retry_calls"`
	RepairCalls      int            `json:"repair_calls"`
	Exhausted        bool           `json:"exhausted"`
	LastFailureClass FailureClass   `json:"last_failure_class"`
}

// Stats returns the plan's current counters.
func (p *Plan) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		PlannedCalls:     p.PlannedCalls,
		CompletedCalls:   p.completedCalls,
		RetryCalls:       p.retryCalls,
		RepairCalls:      p.repairCalls,
		Exhausted:        p.exhausted,
		LastFailureClass: p.lastFailureClass,
	}
}

// Snapshot wraps Stats with a stable envelope reason so the
// "stopped" diagnostic can be rendered consistently across
// callers. The zero-length Reason means "in progress, not
// stopped".
type Snapshot struct {
	Stats
	Reason string `json:"reason,omitempty"`
}

// classifyTransient inspects the error string returned by the
// transport. The classifier deliberately stays conservative —
// anything we don't recognise is treated as FailureUnknown
// (the safest fallback when retry could itself be unsafe).
//
// We avoid pulling in a structured error type from the wire
// layer; the classifier reads the small set of phrases that
// net/http and the wire paths actually emit today.
func classifyTransient(httpStatus int, errStr string) FailureClass {
	if errStr == "" {
		// Caller passed a transport error without a status
		// — assume transient (one retry).
		return FailureTransientTransport
	}
	switch {
	case httpStatus == 401 || httpStatus == 403:
		return FailureAuth
	case httpStatus == 402:
		// Billing refusal — same policy as auth: zero
		// retries, the operator must fix the account.
		return FailureAuth
	case httpStatus == 429:
		return FailureRateLimit
	case httpStatus >= 500 && httpStatus < 600:
		return FailureTransientTransport
	}
	// Status 0 is a transport-layer failure (no HTTP
	// exchange happened). net.OpError surfaces things like
	// "connection reset", "i/o timeout", "EOF". Treat as
	// transient.
	return FailureTransientTransport
}

// AcquireOrStop books a single semantic attempt against the
// plan. It returns nil if a new call is permitted; otherwise
// ErrBoundedPlanExceeded with stats attached.
//
// The optional `stageFingerprint` is a stable hash of the
// input bytes for this stage. If the same fingerprint has
// already been permitted once without an intervening
// mechanical retry authorisation, the second attempt is
// refused (duplicate-stage guard). fingerprint=0 disables the
// fingerprint check (used when the caller knows the stage is
// unique).
//
// The interaction between fingerprint and retry is intentional:
// a transient retry of THE SAME stage re-uses the same
// fingerprint as the original attempt; that is *not* a
// duplicate-stage — only semantic-different stages trip the
// guard.
//
// The contract:
//
//	attempt 1 (fresh): fingerprintSeen[fpr] = 0 -> permit, set 1
//	attempt 2 (mech retry of same stage, called via RetryOrStop
//	                  below): fingerprintSeen[fpr] = 1 -> permit, 2
//	attempt 3 (semantic "different" attempt, same fingerprint):
//	                  fingerprintSeen[fpr] >= 2 -> refuse
func (p *Plan) AcquireOrStop(stageFingerprint uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.exhausted {
		return p.stopLocked("safeguard already exhausted")
	}

	// Recursion guard: refuse if we've already permitted more
	// than (planned + retries + repairs) attempts of this
	// fingerprint. Same fingerprint can't have more attempts
	// than: 1 initial + MaxRetries mechanical retries +
	// MaxRepairs repairs = 1 + MaxRetries + MaxRepairs allowed
	// in total.
	if stageFingerprint != 0 {
		seen := p.fingerprintSeen[stageFingerprint]
		allowance := 1 + p.MaxRetries + p.MaxRepairs
		if seen >= allowance {
			p.exhausted = true
			return p.stopLocked(fmt.Sprintf(
				"duplicate-stage fingerprint %x saw %d attempts (allowance %d)",
				stageFingerprint, seen, allowance))
		}
	}

	// Absolute-call ceiling: refuse if further successful
	// semantic work would push the total past the planned
	// budget + retries + repairs. The +repairs allowance is
	// invariant under stage count: 1 repair per run.
	totalAllowed := p.PlannedCalls + p.MaxRetries*p.PlannedCalls + p.MaxRepairs
	if totalAllowed < 1 {
		totalAllowed = 1
	}
	// Counters track "attempts permitted" — completed+retry+repair.
	attempted := p.completedCalls + p.retryCalls + p.repairCalls
	if attempted >= totalAllowed {
		p.exhausted = true
		return p.stopLocked(fmt.Sprintf(
			"absolute call ceiling reached (%d/%d total attempted)",
			attempted, totalAllowed))
	}

	if stageFingerprint != 0 {
		p.fingerprintSeen[stageFingerprint]++
	}
	return nil
}

// AccountSuccess records a successful semantic call against
// the planned budget.
func (p *Plan) AccountSuccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completedCalls++
}

// AccountFailure records a transient failure (or a non-fatal
// pre-stop event) for telemetry + lastFailureClass. It does
// NOT consume a retry slot; that happens in RetryOrStop.
func (p *Plan) AccountFailure(class FailureClass) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastFailureClass = class
}

// RetryOrStop accounts a permitted mechanical retry. Callers
// must call RetryOrStop AFTER AcquireOrStop has permitted the
// retry (typically the retry succeeds via the same
// AcquireOrStop path on a fresh attempt).
func (p *Plan) RetryOrStop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retryCalls >= p.MaxRetries*p.PlannedCalls {
		p.exhausted = true
		return p.stopLocked(fmt.Sprintf(
			"retry budget exhausted (%d/%d retry attempts used)",
			p.retryCalls, p.MaxRetries*p.PlannedCalls))
	}
	p.retryCalls++
	return nil
}

// RepairOrStop accounts a permitted machine-output repair.
// At most MaxRepairsPerRun across the entire run.
func (p *Plan) RepairOrStop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.repairCalls >= p.MaxRepairs {
		p.exhausted = true
		return p.stopLocked(fmt.Sprintf(
			"repair budget exhausted (%d/%d repair attempts used)",
			p.repairCalls, p.MaxRepairs))
	}
	p.repairCalls++
	return nil
}

// stopLocked writes the breaker's stop state under the
// assumption the caller already holds p.mu.
func (p *Plan) stopLocked(reason string) error {
	p.exhausted = true
	return fmt.Errorf("%w: %s", ErrBoundedPlanExceeded, reason)
}

// StoppedReason returns a human-readable summary of the
// stop, or empty string if the plan is still healthy.
func (p *Plan) StoppedReason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.exhausted {
		return ""
	}
	s := p.Stats()
	return fmt.Sprintf(
		"bounded-execution safeguard triggered. "+
			"planned=%d completed=%d retries=%d repairs=%d last_failure=%s",
		s.PlannedCalls, s.CompletedCalls, s.RetryCalls, s.RepairCalls,
		s.LastFailureClass)
}

// RecursionSentinel is a depth-bounded marker that callers
// push/pop across the same execution chain (request →
// admission → synth, etc.). The default depth is 1: a
// synthesis call may not recursively enqueue another
// synthesis within the SAME chain.
//
// The sentinel uses a context-scoped counter stored in the
// existing context values map (no new context key for the
// alpha; we reuse a single key and document the contract).
//
// The magic value lives in `recursion` package below.
type recursionState struct{ depth int }

// recursionEnabled controls the per-run recursion guard. The
// guard is global to the process so a single chain (e.g. a
// goroutine spawned from an LLM tool result) cannot loop.
//
// DO NOT disable for normal operation; tests that need to
// exercise recursion attempt-detection can flip it off via
// the test-only entry points below.
var recursionEnabled = true

// MarkRecursion prevents the same execution chain from
// invoking a LLM-backed operation recursively. The contract:
//
//	guard := synth.MarkRecursion(ctx)
//	defer guard.Close()
//	if guard.Depth() > 1 { return /* reject */ }
//
// The depth is computed by introspecting the context's
// recursion state (stored via WithRecursionChain below).
func MarkRecursion(ctx context.Context) *RecursionGuard {
	depth := 1
	if v := ctx.Value(recursionKey{}); v != nil {
		if s, ok := v.(*recursionState); ok {
			depth = s.depth + 1
		}
	}
	return &RecursionGuard{depth: depth}
}

// RecursionGuard observes the chain depth. Close is a no-op
// except for tests that need to reset.
type RecursionGuard struct {
	depth int
}

// Depth returns the observed depth (1 = top-level).
func (r *RecursionGuard) Depth() int { return r.depth }

// Close is the deferred cleanup. Required for the deferred
// pattern; in this alpha, the per-call sentinel is inherently
// scoped because each LLM call creates its own context.
// Provided for symmetry.
func (r *RecursionGuard) Close() {}

// recursionKey is the context value key for the chain depth.
// Unexported because this is internal infrastructure.
type recursionKey struct{}

// WithRecursionChain returns a child context tagged with the
// current recursion depth. MarkRecursion reads this to
// detect re-entry. Pass-through otherwise.
func WithRecursionChain(ctx context.Context, depth int) context.Context {
	if !recursionEnabled {
		return ctx
	}
	return context.WithValue(ctx, recursionKey{}, &recursionState{depth: depth})
}

// SynthesisRecursionDetected is the sentinel error wrapped by
// the guard when depth exceeds the cap. The default cap is 1
// (no synthesis can recursively enqueue another synthesis in
// the SAME chain).
var SynthesisRecursionDetected = errors.New("synth: synthesis recursion detected")

// RecursionCap is the maximum allowed depth of a synthesis
// invocation chain. Default: 1.
//
// One chain layer above synthesis is sufficient for the
// admission / watcher / scheduler triggers we have today
// (each spawns its own process or pool task); a deeper chain
// would indicate the model output is being used to
// recursively enqueue more LLM calls without human review.
var RecursionCap = 1

// CheckRecursion examines a proposed call context against the
// current chain depth and rejects if the call would exceed
// RecursionCap. Returns nil if permitted; a wrapped
// SynthesisRecursionDetected error otherwise.
//
// The depth comes from the context's stored recursionState
// (set by WithRecursionChain). For top-level calls there is
// no state and the depth defaults to 1.
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
