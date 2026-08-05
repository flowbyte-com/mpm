package capability

import (
	"context"
	"errors"
	"testing"
)

// =============================================================================
// dispatcher_test.go — DomainDispatcher tests (EX-3.3)
//
// Covers the contract:
//
//   * StaticDispatcher.DriverFor returns the registered Driver
//     for each domain; returns ErrDispatcherUnknownDomain for
//     unmapped domains.
//   * SingleDriverDispatcher routes every domain to the same
//     Driver (EX-1 back-compat path).
//   * injectDriverContext stamps domain + metadata into ctx so
//     the Driver can read them via the typed keys in driver_bwrap.go.
// =============================================================================

// TestStaticDispatcher_RoutesByDomain confirms that the
// production dispatcher routes sandbox and restricted to their
// respective Drivers (in EX-3, both are BwrapDriver instances —
// one per domain is allowed; we test with a single instance here
// to keep the test focused on the routing logic).
func TestStaticDispatcher_RoutesByDomain(t *testing.T) {
	sandbox := &FakeDriver{Results: []*DriverResult{{ExitCode: 0}}}
	restricted := &FakeDriver{Results: []*DriverResult{{ExitCode: 0}}}

	disp := NewStaticDispatcher(map[ExecutionDomain]Driver{
		DomainSandbox:    sandbox,
		DomainRestricted: restricted,
	})

	for _, tc := range []struct {
		domain ExecutionDomain
		want   Driver
	}{
		{DomainSandbox, sandbox},
		{DomainRestricted, restricted},
	} {
		got, err := disp.DriverFor(&Capability{ExecutionDomain: tc.domain})
		if err != nil {
			t.Errorf("DriverFor(%s): %v", tc.domain, err)
		}
		if got != tc.want {
			t.Errorf("DriverFor(%s) = %v, want %v", tc.domain, got, tc.want)
		}
	}
}

// TestStaticDispatcher_UnknownDomainErrors confirms that a
// capability with an ExecutionDomain not in the map produces
// ErrDispatcherUnknownDomain. The Executor maps this to a
// driver-level (nil, err) — no telemetry, no fracture.
func TestStaticDispatcher_UnknownDomainErrors(t *testing.T) {
	disp := NewStaticDispatcher(map[ExecutionDomain]Driver{
		DomainSandbox: &FakeDriver{},
	})
	_, err := disp.DriverFor(&Capability{ExecutionDomain: DomainOperator})
	if !errors.Is(err, ErrDispatcherUnknownDomain) {
		t.Errorf("expected ErrDispatcherUnknownDomain, got: %v", err)
	}
}

// TestStaticDispatcher_NilCapabilityErrors is the defensive
// test for the nil cap pointer. The Executor guards against
// this earlier, but the Dispatcher should also be safe.
func TestStaticDispatcher_NilCapabilityErrors(t *testing.T) {
	disp := NewStaticDispatcher(map[ExecutionDomain]Driver{
		DomainSandbox: &FakeDriver{},
	})
	_, err := disp.DriverFor(nil)
	if err == nil {
		t.Error("expected error for nil capability")
	}
}

// TestStaticDispatcher_NilDriverForDomainErrors confirms that
// a registered-but-nil Driver surfaces as ErrDispatcherUnknownDomain
// (rather than a nil-pointer panic on the Driver.Run call).
func TestStaticDispatcher_NilDriverForDomainErrors(t *testing.T) {
	disp := NewStaticDispatcher(map[ExecutionDomain]Driver{
		DomainSandbox: nil, // explicitly nil
	})
	_, err := disp.DriverFor(&Capability{ExecutionDomain: DomainSandbox})
	if !errors.Is(err, ErrDispatcherUnknownDomain) {
		t.Errorf("expected ErrDispatcherUnknownDomain for nil Driver, got: %v", err)
	}
}

// TestSingleDriverDispatcher_AllDomainsRouteToSameDriver
// confirms EX-1 back-compat: the single-Driver Executor keeps
// working because NewExecutor wraps the Driver in a
// SingleDriverDispatcher that ignores the domain.
func TestSingleDriverDispatcher_AllDomainsRouteToSameDriver(t *testing.T) {
	drv := &FakeDriver{Results: []*DriverResult{{ExitCode: 0}}}
	disp := NewSingleDriverDispatcher(drv)

	for _, domain := range AllExecutionDomains() {
		got, err := disp.DriverFor(&Capability{ExecutionDomain: domain})
		if err != nil {
			t.Errorf("DriverFor(%s): %v", domain, err)
		}
		if got != drv {
			t.Errorf("DriverFor(%s) = %v, want single driver", domain, got)
		}
	}
}

// TestSingleDriverDispatcher_NilDriverErrors confirms the
// defensive nil-Driver check.
func TestSingleDriverDispatcher_NilDriverErrors(t *testing.T) {
	disp := NewSingleDriverDispatcher(nil)
	_, err := disp.DriverFor(&Capability{ExecutionDomain: DomainSandbox})
	if err == nil {
		t.Error("expected error for nil wrapped Driver")
	}
}

// TestInjectDriverContext_StampsDomainAndMetadata confirms
// that the Dispatcher's context injection works — the Driver
// can read the domain + metadata via the typed keys.
func TestInjectDriverContext_StampsDomainAndMetadata(t *testing.T) {
	meta := CapabilityMetadata{
		"project_dir":    "/srv/myapp",
		"allowed_paths": []interface{}{"/data/a", "/data/b"},
	}
	cap := &Capability{
		ExecutionDomain: DomainRestricted,
		Metadata:        meta,
	}
	ctx := injectDriverContext(context.Background(), cap)

	domain, ok := ctx.Value(driverDomainKey{}).(ExecutionDomain)
	if !ok {
		t.Fatal("domain not stamped into ctx")
	}
	if domain != DomainRestricted {
		t.Errorf("ctx domain = %s, want %s", domain, DomainRestricted)
	}

	gotMeta, ok := ctx.Value(driverMetaKey{}).(CapabilityMetadata)
	if !ok {
		t.Fatal("metadata not stamped into ctx")
	}
	if gotMeta["project_dir"] != "/srv/myapp" {
		t.Errorf("ctx metadata project_dir = %v, want /srv/myapp", gotMeta["project_dir"])
	}
}

// TestInjectDriverContext_NilCapabilitySafe confirms the
// nil-cap defensive path: no panic, no stamping.
func TestInjectDriverContext_NilCapabilitySafe(t *testing.T) {
	ctx := injectDriverContext(context.Background(), nil)
	if _, ok := ctx.Value(driverDomainKey{}).(ExecutionDomain); ok {
		t.Error("nil cap should not stamp a domain")
	}
}
