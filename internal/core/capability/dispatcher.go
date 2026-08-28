package capability

import (
	"context"
	"fmt"
)

// =============================================================================
// dispatcher.go — domain → driver routing (spec §4 / EX-3)
//
// The Executor is policy-aware (state machine, source-hash,
// telemetry). It does NOT know how to run a sandbox vs a
// trusted capability — that's the Driver's job. But it DOES
// need to pick which Driver to use based on the Capability's
// ExecutionDomain.
//
// DomainDispatcher is the seam. One method: DriverFor(domain)
// returns the Driver configured for that domain, or an error
// if the domain has no Driver registered (which surfaces as a
// driver-level failure in the Executor — see Invoke).
//
// Two implementations land in this codebase:
//
//   * StaticDispatcher — a fixed map[domain]Driver, built at
//     Executor construction time. Production usage via
//     NewDefaultDispatcher (canonical wiring) or directly
//     (test fixtures).
//   * SingleDriverDispatcher — for EX-1/EX-2 tests that didn't
//     need dispatch because they always passed a single Driver.
//     Retained for back-compat with the existing test suite.
//
// The Executor accepts a Dispatcher via NewExecutorWithDispatcher.
// NewExecutor (single-Driver) remains for tests that don't care
// about domain routing.
//
// The Dispatcher also injects the domain + capability metadata
// into the context the Driver receives. This is how BwrapDriver
// knows to apply restricted-domain bind mounts without seeing
// the full Capability row (which would be a layering violation
// — Drivers are policy-free primitives).
//
// Spec: docs/archive/capability-lifecycle.md §4.1 (sandbox vs
//      restricted arg shape).
// =============================================================================

// DomainDispatcher routes an ExecutionDomain to its Driver.
// Implementations are expected to be safe for concurrent use
// (the Executor invokes them once per Invoke call, and Invoke
// is called concurrently by the CLI/MCP/scheduler paths).
type DomainDispatcher interface {
	// DriverFor returns the Driver that should run capabilities
	// in the given domain. The Capability parameter is passed
	// so a Dispatcher can make metadata-driven routing decisions
	// (e.g., "this restricted capability declares no
	// allowed_paths — refuse it before it reaches the Driver").
	// Returning a non-nil error short-circuits the Invoke
	// pipeline as a driver-level failure.
	DriverFor(cap *Capability) (Driver, error)
}

// StaticDispatcher is the production DomainDispatcher. Maps
// each registered domain to a fixed Driver. Domains without a
// registered Driver produce ErrDispatcherUnknownDomain.
//
// The map is set at construction time and treated as immutable
// thereafter — concurrent reads are safe without locking. If a
// caller needs to mutate the map (e.g., test fixtures), they
// should construct a fresh StaticDispatcher.
type StaticDispatcher struct {
	drivers map[ExecutionDomain]Driver
}

// NewStaticDispatcher builds a Dispatcher from the given
// domain→driver map. Missing entries cause DriverFor to return
// ErrDispatcherUnknownDomain at lookup time, so an empty map
// is technically legal but useless.
//
// Typical wiring (production) uses NewDefaultDispatcher — this
// constructor is for tests that want a non-default map (e.g.,
// routing all domains to a FakeDriver).
func NewStaticDispatcher(drivers map[ExecutionDomain]Driver) *StaticDispatcher {
	// Copy the map so a caller-side mutation post-construction
	// doesn't race with DriverFor reads.
	cp := make(map[ExecutionDomain]Driver, len(drivers))
	for k, v := range drivers {
		cp[k] = v
	}
	return &StaticDispatcher{drivers: cp}
}

// NewDefaultDispatcher builds the production DomainDispatcher
// wiring: a single BwrapDriver covers sandbox + restricted
// (same binary, different arg shape); a single DirectDriver
// covers trusted + operator (same binary, the operator-only
// metadata stamp check lives in Executor.Invoke). This is the
// canonical map for EX-3/EX-6 production callers — every
// ExecutionDomain is registered, every lookup succeeds.
//
// Passing the same Driver instance to two domains is intentional:
// the Driver is policy-free (no per-call state beyond the
// request), so sharing is safe and avoids two goroutines spawning
// two independent tempfiles / wrappers for what is logically
// the same runtime.
//
// A nil BwrapDriver or DirectDriver panics — both are
// load-bearing; a misconfigured host should surface the failure
// at construction time, not at the first invocation.
func NewDefaultDispatcher(bwrap *BwrapDriver, direct *DirectDriver) *StaticDispatcher {
	if bwrap == nil {
		panic("capability: NewDefaultDispatcher: BwrapDriver is nil")
	}
	if direct == nil {
		panic("capability: NewDefaultDispatcher: DirectDriver is nil")
	}
	return NewStaticDispatcher(map[ExecutionDomain]Driver{
		DomainSandbox:    bwrap,
		DomainRestricted: bwrap,
		DomainTrusted:    direct,
		DomainOperator:   direct,
	})
}

// DriverFor implements DomainDispatcher.
func (d *StaticDispatcher) DriverFor(cap *Capability) (Driver, error) {
	if cap == nil {
		return nil, fmt.Errorf("capability: StaticDispatcher: capability is nil")
	}
	drv, ok := d.drivers[cap.ExecutionDomain]
	if !ok {
		return nil, fmt.Errorf("%w: domain=%s", ErrDispatcherUnknownDomain, cap.ExecutionDomain)
	}
	if drv == nil {
		return nil, fmt.Errorf("%w: domain=%s has nil Driver", ErrDispatcherUnknownDomain, cap.ExecutionDomain)
	}
	return drv, nil
}

// SingleDriverDispatcher adapts a single Driver to the
// DomainDispatcher interface. Used internally by NewExecutor
// so EX-1/EX-2 tests that pass a single Driver keep working
// unchanged — they get the same domain-agnostic behavior the
// EX-1 design documented.
//
// All domains route to the same Driver. The Dispatcher does
// NOT verify domain compatibility — that's the Executor /
// Driver's job (BwrapDriver errors on trusted/operator;
// DirectDriver errors on sandbox/restricted; those checks are
// the Driver's contract).
type SingleDriverDispatcher struct {
	driver Driver
}

// NewSingleDriverDispatcher wraps a single Driver as a
// DomainDispatcher. Convenience for tests + the single-Driver
// Executor code path.
func NewSingleDriverDispatcher(drv Driver) *SingleDriverDispatcher {
	return &SingleDriverDispatcher{driver: drv}
}

// DriverFor implements DomainDispatcher. Always returns the
// wrapped Driver regardless of domain.
func (d *SingleDriverDispatcher) DriverFor(cap *Capability) (Driver, error) {
	if d.driver == nil {
		return nil, fmt.Errorf("capability: SingleDriverDispatcher: wrapped Driver is nil")
	}
	return d.driver, nil
}

// ErrDispatcherUnknownDomain is the lookup miss. Surfaces when
// a capability has an ExecutionDomain the Dispatcher has no
// Driver registered for (e.g., EX-3 ships without a DirectDriver
// so trusted/operator domains return this error). The Executor
// maps it to a driver-level (nil, err) result — no telemetry,
// no fracture — because the failure is a configuration bug,
// not a capability bug.
var ErrDispatcherUnknownDomain = fmt.Errorf("capability: dispatcher has no Driver for ExecutionDomain")

// =============================================================================
// Context injection helpers
//
// These live in dispatcher.go (not driver_bwrap.go) because
// the Dispatcher is the call site that knows which domain +
// metadata a particular Driver needs. The Drivers consume
// the injected context values via the typed keys defined in
// driver_bwrap.go (driverDomainKey{}, driverMetaKey{}).
//
// Keeping the keys in driver_bwrap.go and the injection here
// produces a clean dependency direction:
//
//   dispatcher.go  ──writes──▶  context.Context  ──reads──▶  driver_bwrap.go
//
// The Dispatcher depends on the keys' existence (compile-time
// reference); the Driver depends on the keys' content (runtime
// lookup). No circular imports.
// =============================================================================

// injectDriverContext returns a child context carrying the
// domain + metadata the Driver needs to build its argv. Called
// once per Invoke, right before the Driver.Run call.
//
// Pulled out as a package-level helper (not a method on
// StaticDispatcher) because SingleDriverDispatcher uses it too
// — and because the Driver interface shouldn't have to know
// the Dispatcher exists.
func injectDriverContext(ctx context.Context, cap *Capability) context.Context {
	if cap == nil {
		return ctx
	}
	return withDomainAndMeta(ctx, cap.ExecutionDomain, cap.Metadata)
}

// Compile-time guard that SingleDriverDispatcher satisfies
// DomainDispatcher (StaticDispatcher is checked implicitly
// via NewStaticDispatcher → DriverFor).
var _ DomainDispatcher = (*SingleDriverDispatcher)(nil)
var _ DomainDispatcher = (*StaticDispatcher)(nil)
