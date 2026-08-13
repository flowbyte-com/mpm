package capability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// =============================================================================
// executor.go — capability invocation runtime (spec §4)
//
// The Executor is the single Go function that turns a (capability, args,
// env) triple into a (exit_code, stdout, stderr, duration_ms) result.
// Every invocation — agent, scheduler, CLI, MCP — flows through it.
//
// The Executor enforces the load-bearing policy:
//
//   1. Liveness: the capability row must be in a callable state AND not
//      superseded by a newer revision.
//   2. source_hash verification: sha256 of the live source code must
//      equal capabilities.source_hash. Mismatch fractures the
//      capability (runtime tamper) and refuses the invocation.
//   3. Domain → driver selection (EX-3 fills in the dispatch table;
//      EX-1 invokes through the Invoker interface so the dispatch
//      table is a swappable dependency).
//   4. Resource limits resolution: request.Limits overrides
//      capability.metadata overrides hardcoded defaults.
//   5. Timeout / rlimit application (EX-4 fills in the per-driver
//      implementation; the limits struct is stable).
//   6. Invocation telemetry write — FAIL-FAST. A telemetry write
//      failure returns ErrTelemetryFailed regardless of execution
//      success. An unrecorded action is worse than a failed action.
//   7. Counter update: success_count / failure_count /
//      last_invoked_at updated in the same transaction as the
//      telemetry row, so the two never disagree.
//   8. Fracture detection (EX-7): 3 timeout/OOM failures within a
//      60-second window flips the capability to state='fractured'
//      and emits a §2.4.1 wake.
//
// Architecture:
//
//   Invoker interface (this file)        — the public boundary. One method.
//   Driver  interface (this file)        — the lowest-level primitive. One method.
//   Executor (this file)                 — owns the Invoker surface, wraps a Driver.
//   BwrapDriver / DirectDriver (EX-3/EX-6) — the real Driver implementations.
//
// The Invoker/Driver split is deliberate. The Invoker surface is
// policy-aware (it knows about capabilities, state machines, source
// hashes, telemetry). The Driver surface is policy-free (it knows
// how to run a string under isolation and return the result). Tests
// can mock at either layer — most EX-1 tests use a FakeDriver; the
// Executor itself gets separately verified with a stub Invoker.
//
// Spec: docs/architecture/capability-lifecycle.md §4
// =============================================================================

// SourceLanguage is the typed source-language identifier. The Forge
// already restricts proposals to {bash, python, jq}; the Executor
// re-validates at invoke time because the Driver selects the
// interpreter based on this value, and a typo in the CLI handler
// ("bash " with a trailing space) would silently dispatch to the
// wrong interpreter. A named type with Validate() makes the type
// system carry the invariant past the proposal boundary.
type SourceLanguage string

const (
	LangBash   SourceLanguage = "bash"
	LangPython SourceLanguage = "python"
	LangJQ     SourceLanguage = "jq"
)

// Validate returns nil if l is one of the three recognized languages.
// Called at the Executor boundary to defend against CLI/MCP bugs
// that might pass an empty or mistyped language string.
func (l SourceLanguage) Validate() error {
	switch l {
	case LangBash, LangPython, LangJQ:
		return nil
	}
	return fmt.Errorf("capability: unknown source language %q (must be bash, python, or jq)", string(l))
}

// Interpreter returns the absolute path to the interpreter binary
// for this language. Used by both Drivers and the dry-run path
// (forge_dryrun.go). Centralizing the path here keeps the
// interpreter selection in one place.
func (l SourceLanguage) Interpreter() string {
	switch l {
	case LangBash:
		return "/bin/bash"
	case LangPython:
		return "/usr/bin/python3"
	case LangJQ:
		return "/usr/bin/jq"
	}
	// Validate() should have caught this; defensive zero value.
	return ""
}

// AllSourceLanguages returns every recognized language. Mirrors
// ExecutionDomain.AllExecutionDomains() — kept separate so the
// language taxonomy can diverge from the domain taxonomy.
func AllSourceLanguages() []SourceLanguage {
	return []SourceLanguage{LangBash, LangPython, LangJQ}
}

// ResourceLimits is the per-invocation ceiling. Resolved in this
// order (first non-zero wins):
//
//   1. req.Limits           — caller-supplied override (CLI flag, MCP arg)
//   2. capability.metadata  — proposal-stamped default
//   3. hardcoded fallback   — see DefaultResourceLimits()
//
// All fields use 0 to mean "use the next-priority source." Callers
// that genuinely want "unlimited" should pass math.MaxInt64 (the
// Executor will clamp against platform limits where appropriate;
// EX-4 fills in the clamping rules).
type ResourceLimits struct {
	MaxRuntimeMs   int64 // wall clock; 0 = use metadata, then default
	MaxMemoryMB    int64 // bwrap --rlimit-as; 0 = use metadata, then default
	MaxFDs         int64 // bwrap --rlimit-nofile; 0 = use metadata, then default
	MaxOutputBytes int64 // stdout+stderr ceiling; 0 = use metadata, then default
}

// DefaultResourceLimits returns the hardcoded fallback applied
// when neither the request nor the capability metadata specifies
// a limit. Values mirror spec §4.3:
//
//   MaxRuntimeMs   = 30_000   (30 seconds)
//   MaxMemoryMB    = 512      (half a gigabyte)
//   MaxFDs         = 256      (POSIX soft limit)
//   MaxOutputBytes = MaxOutputBytesDefault (16 MiB)
func DefaultResourceLimits() ResourceLimits {
	return ResourceLimits{
		MaxRuntimeMs:   30_000,
		MaxMemoryMB:    512,
		MaxFDs:         256,
		MaxOutputBytes: MaxOutputBytesDefault,
	}
}

// resolveLimits implements the three-tier precedence:
//
//	req.Limits > metadata > DefaultResourceLimits
//
// metadata is the capability's CapabilityMetadata JSON blob. Known
// keys (string-encoded integers):
//
//	metadata["max_runtime_ms"]
//	metadata["max_memory_mb"]
//	metadata["max_fds"]
//	metadata["max_output_bytes"]
//
// Unknown keys are ignored (forward-compatible: a future
// metadata field doesn't break old Executors).
//
// After resolution, ClampLimits applies the documented safety
// ceilings (limits.go) so a malicious or typo'd metadata
// payload cannot request a 1-petabyte memory cap. The clamp
// is the single insertion point — every Driver receives
// pre-clamped limits.
//
// Returns a fully-resolved ResourceLimits — every field non-zero
// AND within the documented ceiling. The Executor never has
// to think about "is this zero? use the next tier" or "is
// this absurd? clamp it" once it has the resolved value.
func resolveLimits(req ResourceLimits, meta CapabilityMetadata) ResourceLimits {
	out := req
	if out.MaxRuntimeMs == 0 {
		out.MaxRuntimeMs = int64FromMeta(meta, "max_runtime_ms")
	}
	if out.MaxRuntimeMs == 0 {
		out.MaxRuntimeMs = DefaultResourceLimits().MaxRuntimeMs
	}
	if out.MaxMemoryMB == 0 {
		out.MaxMemoryMB = int64FromMeta(meta, "max_memory_mb")
	}
	if out.MaxMemoryMB == 0 {
		out.MaxMemoryMB = DefaultResourceLimits().MaxMemoryMB
	}
	if out.MaxFDs == 0 {
		out.MaxFDs = int64FromMeta(meta, "max_fds")
	}
	if out.MaxFDs == 0 {
		out.MaxFDs = DefaultResourceLimits().MaxFDs
	}
	if out.MaxOutputBytes == 0 {
		out.MaxOutputBytes = int64FromMeta(meta, "max_output_bytes")
	}
	if out.MaxOutputBytes == 0 {
		out.MaxOutputBytes = DefaultResourceLimits().MaxOutputBytes
	}
	return ClampLimits(out)
}

// int64FromMeta reads a known-int metadata key. Returns 0 if the
// key is absent or not parseable as int64. Treating "not parseable"
// as "absent" is intentional — a future metadata format that
// stringifies durations doesn't break old Executors; they fall
// through to the next tier.
func int64FromMeta(m CapabilityMetadata, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64: // JSON unmarshal may surface numbers as float64
		return int64(n)
	}
	return 0
}

// InvokeRequest is the input to Executor.Invoke. Bundled into a
// struct (rather than positional arguments) so the parameter list
// stays readable as fields grow, and adding a new field (e.g.,
// TraceID for distributed tracing) is a non-breaking change to
// the Invoker interface.
type InvokeRequest struct {
	Capability *Capability    // the row (state, source_hash, domain, metadata)
	Language   SourceLanguage // bash | python | jq; validated at boundary
	SourceCode string         // live source, must hash-equal Capability.SourceHash
	Args       []string       // positional argv
	Env        map[string]string
	Limits     ResourceLimits // per-invocation ceiling; 0 = use metadata/defaults
}

// InvokeResult is the output of Executor.Invoke. Distinct from
// DriverResult because:
//
//   * DriverResult has raw []byte; InvokeResult has string.
//   * InvokeResult has Truncated flag; DriverResult does not.
//   * InvokeResult has DurationMs as the resolved wall-clock
//     measurement; DriverResult has it as a raw observation.
type InvokeResult struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	DurationMs int64
	Truncated  bool // true if stdout OR stderr hit the 16MB cap
}

// DriverRequest is what the Executor hands to a Driver. Deliberately
// driver-shaped: no Capability, no state machine, no telemetry —
// just "run this source under isolation with these args/limits."
// One Driver per isolation model (BwrapDriver, DirectDriver).
type DriverRequest struct {
	Language   SourceLanguage
	SourceCode string
	Args       []string
	Env        map[string]string
	Limits     ResourceLimits // already-resolved (no metadata lookup)
	DriverName string        // "bwrap" | "direct" | "fake"; stamped into telemetry
}

// DriverResult is what a Driver returns. Err is reserved for
// infrastructure failures (binary missing, fork() failed, I/O on
// the pipe pair failed). A non-zero ExitCode is NOT an Err — that's
// a normal "the script ran and returned non-zero" outcome, and
// surfaces as a successful InvokeResult with ExitCode preserved.
//
// StdoutTruncated and StderrTruncated are per-stream flags
// reported by the Driver after its cap-aware read. The
// Executor ORs them to produce InvokeResult.Truncated. The
// per-stream distinction matters for forensics: "stdout hit
// the cap but stderr didn't" is a different failure pattern
// from "stderr overflowed" and EX-7's fracture detection
// queries benefit from being able to filter on it.
type DriverResult struct {
	ExitCode         int
	Stdout           []byte
	Stderr           []byte
	StdoutTruncated  bool
	StderrTruncated  bool
	DurationMs       int64
	Err              error
}

// Driver is the lowest-level execution primitive. EX-3 fills in
// BwrapDriver (sandbox + restricted); EX-6 fills in DirectDriver
// (trusted + operator). Tests inject FakeDriver.
type Driver interface {
	Run(ctx context.Context, req DriverRequest) (*DriverResult, error)
}

// NamedDriver is the optional interface a Driver implements to
// self-identify in telemetry. The Executor's driverName() method
// type-asserts to NamedDriver first; implementations that don't
// satisfy it fall back to a type-name lookup. Canonical names:
//
//	"bwrap"   — BwrapDriver (EX-3, sandbox + restricted domains)
//	"direct"  — DirectDriver (EX-6, trusted + operator domains)
//	"fake"    — FakeDriver (tests)
//
// The name lands in capability_invocations.invocation_context
// and powers "do failures correlate with a specific driver?"
// queries without joins.
type NamedDriver interface {
	Driver
	DriverName() string
}

// Invoker is the public boundary every caller uses. The Executor
// implements it; tests can also implement it directly to bypass
// all policy and verify a single handler's call shape.
type Invoker interface {
	Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error)
}

// Executor is the canonical Invoker implementation. Construct
// with NewExecutor (single-Driver, EX-1 mode) or
// NewExecutorWithDispatcher (domain-aware, EX-3+ mode). The
// returned value is safe for concurrent use (no per-call state
// beyond the embedded references).
type Executor struct {
	store      *Store
	driver     Driver // legacy single-Driver path; nil when dispatcher is set
	dispatcher DomainDispatcher // EX-3 domain-aware path; nil when single-Driver
	clock      Clock
}

// NewExecutor builds an Executor with a single Driver for all
// domains. Retained for EX-1/EX-2 tests that don't need
// domain-aware dispatch. Production code should use
// NewExecutorWithDispatcher.
//
// A nil clock falls back to realClock. A nil driver is a
// programmer error and panics — the Executor cannot dispatch
// invocations without one.
func NewExecutor(store *Store, driver Driver, clock Clock) *Executor {
	if store == nil {
		panic("capability: NewExecutor: store is nil")
	}
	if driver == nil {
		panic("capability: NewExecutor: driver is nil")
	}
	if clock == nil {
		clock = realClock
	}
	return &Executor{
		store:      store,
		driver:     driver,
		dispatcher: NewSingleDriverDispatcher(driver),
		clock:      clock,
	}
}

// NewExecutorWithDispatcher builds an Executor that routes
// invocations to Drivers by ExecutionDomain (sandbox → Bwrap,
// trusted → Direct, etc.). This is the production constructor
// from EX-3 onward.
//
// A nil dispatcher falls back to a no-domain-routing path
// equivalent to NewExecutor with no Driver — every invocation
// will fail with ErrDispatcherUnknownDomain. A non-nil clock
// is required; nil panics so misconfiguration fails at
// construction time, not at the first invocation.
func NewExecutorWithDispatcher(store *Store, dispatcher DomainDispatcher, clock Clock) *Executor {
	if store == nil {
		panic("capability: NewExecutorWithDispatcher: store is nil")
	}
	if dispatcher == nil {
		panic("capability: NewExecutorWithDispatcher: dispatcher is nil")
	}
	if clock == nil {
		clock = realClock
	}
	return &Executor{
		store:      store,
		dispatcher: dispatcher,
		clock:      clock,
	}
}

// Now returns the Executor's wall-clock as Unix-epoch seconds.
// Mirrors Store.Now() so audit fields written by the Executor
// (telemetry rows) use the same time source as the Store's own
// audit fields (state transitions, events). One clock, one truth.
func (e *Executor) Now() int64 { return e.clock().Unix() }

// Invoke runs the §4 pipeline. Returns *InvokeResult on success,
// or one of the typed errors:
//
//   ErrNotLive              — capability is in a non-callable state or superseded
//   ErrSourceHashMismatch   — live source code does not hash-equal Capability.SourceHash
//                             (also triggers a fracture transition)
//   ErrOperatorNotApproved  — execution_domain='operator' without metadata.operator_approved_at
//   ErrSourceLanguageUnknown — Language.Validate() failed (defensive: Forge already gates this)
//   ErrTelemetryFailed      — capability_invocations row write failed
//
// The error return is reserved for two distinct shapes:
//
//   (a) Capability-level failures — the script ran and returned
//       non-zero. These surface as *InvokeResult with ExitCode
//       preserved; the caller decides whether to treat them as
//       "expected non-zero" (e.g., grep found nothing) or
//       "unexpected failure" (e.g., the script crashed).
//
//   (b) Driver-level failures — the binary was missing, the
//       sandbox couldn't start, the FakeDriver queue was
//       exhausted. These surface as (nil, error). They are NOT
//       recorded in capability_invocations because they are
//       not invocations — they are substrate-level failures
//       that happened to a specific call site. Recording them
//       would conflate "the capability is broken" with "the
//       driver infrastructure is broken," which is precisely
//       the distinction EX-7's fracture detection depends on.
//
// This is the policy-heavy method. Steps 1, 2, 4, 6, 7, 8 live
// here. Step 3 (driver dispatch) lives here too but is trivial —
// the domain-to-driver mapping is a switch on req.Capability.ExecutionDomain.
// EX-3 / EX-6 add the real BwrapDriver / DirectDriver behind the
// Driver interface; this method never has to change.
func (e *Executor) Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error) {
	// Step 0: defensive validation. None of these should fire in
	// production (the Forge gates at propose time), but the
	// Executor is the last line of defense before a subprocess
	// is spawned, so the invariants are re-checked here.
	if req.Capability == nil {
		return nil, fmt.Errorf("capability: executor: Capability is nil")
	}
	if err := req.Language.Validate(); err != nil {
		return nil, err
	}
	if req.SourceCode == "" {
		return nil, fmt.Errorf("capability: executor: SourceCode is empty")
	}

	// Step 1: Liveness. The capability must be in a callable
	// state AND not superseded. State.IsCallable() handles the
	// state half; the superseded_by_id check handles the
	// lineage half. Both must hold.
	if !req.Capability.State.IsCallable() {
		return nil, fmt.Errorf("%w: state=%s", ErrNotLive, req.Capability.State)
	}
	if req.Capability.SupersededByID != nil {
		return nil, fmt.Errorf("%w: superseded_by_id=%s", ErrNotLive, *req.Capability.SupersededByID)
	}

	// Step 2: source_hash verification. SHA-256 of the live
	// source code (provided by the caller — typically loaded
	// from disk or a payload) must equal the hash recorded at
	// proposal time. Mismatch means the runtime has been
	// tampered with (or the caller passed the wrong source),
	// and the capability fractures immediately.
	sum := sha256.Sum256([]byte(req.SourceCode))
	gotHash := hex.EncodeToString(sum[:])
	if gotHash != req.Capability.SourceHash {
		// Fracture the capability. This is a side effect, but
		// it's the only safe response to a tamper signal —
		// refusing the call without fracturing leaves a known-
		// compromised capability in the active set. The fracture
		// is best-effort: if the Store write fails, we still
		// return ErrSourceHashMismatch (the caller learns the
		// invocation was refused), but the wake emission is
		// tied to the Store write's success.
		_ = e.store.FractureCapability(req.Capability.ID, "source_hash_mismatch")
		return nil, fmt.Errorf("%w: expected=%s got=%s",
			ErrSourceHashMismatch, req.Capability.SourceHash, gotHash)
	}

	// Step 4 (resource limits): resolve the three-tier
	// precedence (request > metadata > defaults). The Driver
	// gets the fully-resolved limits — it never has to do its
	// own metadata lookup.
	limits := resolveLimits(req.Limits, req.Capability.Metadata)

	// Step 3 (operator approval gate): operator-domain
	// capabilities require metadata.operator_approved_at to be
	// present. The check happens BEFORE driver dispatch so a
	// missing approval never even spawns a process. The key is
	// a unix-epoch-seconds int; non-numeric values are treated
	// as missing (defensive against hand-edited metadata).
	if req.Capability.ExecutionDomain == DomainOperator {
		if int64FromMeta(req.Capability.Metadata, "operator_approved_at") == 0 {
			return nil, fmt.Errorf("%w: capability %s",
				ErrOperatorNotApproved, req.Capability.Name)
		}
	}

	// Step 5: build a timeout-bounded context. The Driver
	// inherits this context; if MaxRuntimeMs elapses, the
	// Driver must kill the child process. EX-4 fills in the
	// per-Driver implementation; for EX-1, the context is the
	// only enforcement.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(limits.MaxRuntimeMs)*time.Millisecond)
	defer cancel()

	// Step 3 (driver dispatch): the domain → driver selection.
	// EX-3 wires the DomainDispatcher; for back-compat with
	// EX-1/EX-2's single-Driver Executor, the dispatcher is
	// always non-nil (NewExecutor wraps the single Driver in
	// a SingleDriverDispatcher). The Dispatcher also injects
	// the domain + metadata into the context so the Driver
	// can build its argv without seeing the full Capability.
	//
	// StartedAt is captured BEFORE the dispatch so the
	// telemetry row's invoked_at reflects when the Executor
	// began running the capability (not when telemetry was
	// finally written — those can differ by retry backoff).
	startedAt := e.Now()
	drvReq := DriverRequest{
		Language:   req.Language,
		SourceCode: req.SourceCode,
		Args:       req.Args,
		Env:        req.Env,
		Limits:     limits,
		DriverName: e.driverNameFor(req.Capability),
	}
	drvRes, drvErr := e.dispatchAndRun(runCtx, req.Capability, drvReq)

	// Step 3b: driver-level failure short-circuit. A non-nil
	// drvErr means the driver could not run the capability at
	// all (binary missing, sandbox failed to start, FakeDriver
	// queue exhausted, fork() failed, etc.). We return
	// (nil, drvErr) immediately and DO NOT write telemetry —
	// this is not an invocation, it is an infrastructure
	// failure that happens to a specific call site. Recording
	// it in capability_invocations would conflate "the
	// capability's script is broken" with "the substrate
	// can't run anything right now," which is precisely the
	// distinction EX-7's fracture detection depends on.
	if drvErr != nil {
		return nil, drvErr
	}
	if drvRes == nil {
		// Defensive: a Driver that returns (nil, nil) is a
		// contract violation. Treat as driver-level failure
		// for the same reason — we have no result to record.
		return nil, fmt.Errorf("capability: executor: driver returned (nil, nil)")
	}

	// Step 6: build the result shape. From here on, the
	// capability DID run; a non-zero ExitCode is a legitimate
	// "the script ran and returned non-zero" outcome.
	exitCode := drvRes.ExitCode
	stdout := drvRes.Stdout
	stderr := drvRes.Stderr
	duration := drvRes.DurationMs

	// Step 5b: output truncation. The Driver owns the
	// cap-aware read (drainCapped in output.go) and reports
	// the per-stream truncation status. The Executor's
	// job here is to OR them for the single-flag
	// InvokeResult.Truncated and pass the per-stream flags
	// forward into the telemetry payload (EX-7 fracture
	// detection queries filter on them).
	//
	// We do NOT re-truncate the raw bytes here — the Driver
	// has already capped them at limits.MaxOutputBytes via
	// drainCapped. The Executor's only truncation concern
	// is "should I report Truncated=true to the caller?"
	stdoutTruncated := drvRes.StdoutTruncated
	stderrTruncated := drvRes.StderrTruncated
	truncated := stdoutTruncated || stderrTruncated

	result := &InvokeResult{
		ExitCode:   exitCode,
		Stdout:     string(stdout),
		Stderr:     string(stderr),
		DurationMs: duration,
		Truncated:  truncated,
	}

	// Step 6: telemetry write. FAIL-FAST. This is the
	// auditable trail — if we can't record what happened, we
	// don't claim it happened. Store.RecordInvocation runs
	// inside its own retry loop (4 attempts with exponential
	// backoff on SQLITE_BUSY; honors ctx between every
	// attempt and every backoff sleep). On any write
	// failure — exhausted retry budget, non-transient error,
	// or caller-supplied ctx cancellation — it returns
	// ErrTelemetryFailed wrapped with the underlying cause.
	finishedAt := e.Now()
	payload := &TelemetryPayload{
		CapabilityID: req.Capability.ID,
		Language:     req.Language,
		StartedAt:    startedAt,
		FinishedAt:   finishedAt,
		ExitCode:     exitCode,
		DurationMs:   duration,
		Stdout:       stdout,
		Stderr:       stderr,
		Truncated:    truncated,
		ArgsHash:     HashArgs(req.Args),
		EnvKeys:      SortedEnvKeys(req.Env),
		DriverName:   e.driverNameFor(req.Capability),
	}
	if err := e.store.RecordInvocation(ctx, payload); err != nil {
		return nil, err
	}

	// Step 7: counter update was folded into RecordInvocation
	// (same transaction as the telemetry row) so success_count
	// / failure_count never disagree with capability_invocations.

	// Step 8: fracture detection (EX-7). After the telemetry
	// row is durably written, check the sliding-window failure
	// count; if it crosses the 3-in-60s threshold AND the
	// capability is in StateDegraded (per spec §1.3 row 9),
	// MarkFracture transitions to StateFractured AND emits the
	// §2.4.1 cascade wake when author_theory_id is set.
	//
	// Best-effort: a checkFracture failure does NOT mask the
	// successful telemetry-backed result. The next failure in
	// the window will re-trigger the detector.
	if err := e.checkFracture(ctx, req.Capability.ID); err != nil {
		// Swallowed by design — see Store.checkFracture doc.
		_ = err
	}

	return result, nil
}

// dispatchAndRun resolves the Driver for this capability's
// ExecutionDomain via the Dispatcher, injects the domain +
// metadata into the context, and dispatches. Returns the
// driver's result plus a driver-level error (or nil).
//
// Dispatcher lookups that fail (unknown domain, nil Driver)
// surface as driver-level (nil, err) — no telemetry, no
// fracture — because the failure is a configuration bug in
// the Executor's wiring, not a capability problem.
func (e *Executor) dispatchAndRun(ctx context.Context, cap *Capability, drvReq DriverRequest) (*DriverResult, error) {
	if e.dispatcher == nil {
		return nil, fmt.Errorf("capability: Executor: dispatcher is nil (use NewExecutor or NewExecutorWithDispatcher)")
	}
	drv, err := e.dispatcher.DriverFor(cap)
	if err != nil {
		return nil, err
	}
	injected := injectDriverContext(ctx, cap)
	return drv.Run(injected, drvReq)
}

// driverNameFor returns the canonical name of the Driver that
// will run `cap`. Defaults to "unknown" if the Dispatcher
// can't resolve a Driver (the Invoke path will fail loudly
// elsewhere; this is just the telemetry stamp).
//
// The Dispatcher is the source of truth — even for the
// single-Driver case, NewExecutor wraps the Driver in a
// SingleDriverDispatcher so the lookup path is uniform.
func (e *Executor) driverNameFor(cap *Capability) string {
	if e == nil || e.dispatcher == nil {
		return "unknown"
	}
	drv, err := e.dispatcher.DriverFor(cap)
	if err != nil || drv == nil {
		return "unknown"
	}
	if n, ok := drv.(interface{ DriverName() string }); ok {
		return n.DriverName()
	}
	switch drv.(type) {
	case *FakeDriver:
		return "fake"
	}
	return "unknown"
}

// recordInvocation is retained for the EX-1 stub body that
// lives in executor_helpers.go. EX-2 calls Store.RecordInvocation
// directly from Invoke (see Step 6 above); this method is now
// unused by the production path and exists only so the helper
// stub compiles. Removed in EX-3 when the helper file goes away.

// checkFracture is the EX-7 hook. Delegates to
// Store.checkFracture (the canonical sliding-window detector
// + §2.4.1 cascade emission). Lives as an Executor method so
// the call site in Invoke stays uniform across the test suite
// (some tests override the hook; some override the Store
// directly).
//
// Errors are surfaced (not swallowed) so the production call
// site can log them via the executor's own audit path; the
// Invoke call site swallows them because the telemetry row is
// already durably written and a missed fracture is recoverable.
func (e *Executor) checkFracture(ctx context.Context, capID string) error {
	if e == nil || e.store == nil {
		return nil
	}
	_, err := e.store.checkFracture(ctx, capID)
	return err
}