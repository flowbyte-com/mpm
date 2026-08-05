package capability

import (
	"context"
	"errors"
	"sync"
)

// =============================================================================
// driver_fake.go — FakeDriver test double
//
// The FakeDriver is the canonical test primitive for the Driver
// interface. Tests pre-load Results and Errors queues; each Run
// call dequeues one entry from each queue. When a queue is
// exhausted, Run returns ErrFakeExhausted — a loud failure that
// surfaces the first time a test forgets to enqueue a fixture.
//
// Why loud failure matters:
//
//   A FakeDriver that silently returns {ExitCode: 0, Stdout: nil,
//   Stderr: nil} when its queue runs dry turns every test that
//   forgets one more fixture into a green test that exercises
//   nothing. The test passes because the assertion is "result is
//   not nil" and the fake quietly produced a zero value. The bug
//   surfaces hours later in production when the real driver
//   behaves differently.
//
//   Loud failure at the first missing fixture is strictly
//   better: the test fails immediately with a stack trace that
//   points at the call site that drained the queue.
//
// Queue semantics:
//
//   Errors is dequeued before Results (an explicit error short-
//   circuits any result). A nil entry in Errors is a no-op (used
//   when a test wants to skip error injection but keep the
//   queue-length alignment). When both queues are exhausted, the
//   run returns ErrFakeExhausted.
//
// Concurrency:
//
//   The FakeDriver records Calls and dequeues queues under a
//   mutex. Tests that share a FakeDriver across goroutines (rare
//   but possible in scheduler-style tests) won't race.
// =============================================================================

// ErrFakeExhausted is the sentinel returned by FakeDriver.Run when
// both Results and Errors queues are drained. Tests assert
// errors.Is(err, ErrFakeExhausted) when verifying "we ran out of
// fixtures" — typically as a guard against accidentally
// under-specifying a test.
var ErrFakeExhausted = errors.New("capability: FakeDriver: queue exhausted")

// FakeDriver is the test double for the Driver interface. See
// the file header for queue semantics and the rationale behind
// ErrFakeExhausted.
type FakeDriver struct {
	mu sync.Mutex

	// Results is the queue of canned DriverResults. Each Run
	// call dequeues one entry from the head. Populated by tests
	// before invoking the Executor.
	Results []*DriverResult

	// Errors is the queue of canned errors. Each Run call
	// dequeues one entry; a non-nil entry causes Run to return
	// that error immediately (the corresponding Results entry,
	// if any, is consumed on the same call so queues stay
	// aligned).
	Errors []error

	// Calls records every DriverRequest the FakeDriver has
	// received. Tests assert on len(Calls), on specific fields
	// of Calls[i], or on the order of calls (when the Executor
	// is expected to invoke the Driver multiple times).
	Calls []DriverRequest

	// Hook is an optional pre-Run callback. Tests that need to
	// inspect the request mid-flight (e.g., to assert that a
	// specific argument was passed) set Hook; it runs under the
	// mutex, so it must not call back into the FakeDriver.
	Hook func(req DriverRequest)
}

// Run implements the Driver interface. See file header for
// queue semantics. Returns ErrFakeExhausted when both queues
// are drained.
func (f *FakeDriver) Run(ctx context.Context, req DriverRequest) (*DriverResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Record the call regardless of what we return. Tests
	// asserting "the Driver was never invoked" check len(Calls)
	// after the test; recording before any return path means a
	// test that catches a returned error still sees the call.
	f.Calls = append(f.Calls, req)

	// Optional hook for mid-flight inspection. Runs under the
	// mutex; must not call back into the FakeDriver (would deadlock).
	if f.Hook != nil {
		f.Hook(req)
	}

	// Drain the Errors queue first. A nil entry is a no-op
	// (queue alignment marker); a non-nil entry causes Run to
	// return that error and skip the Results queue.
	if len(f.Errors) > 0 {
		err := f.Errors[0]
		f.Errors = f.Errors[1:]
		if err != nil {
			// Drain the matching Results entry so queues
			// stay length-aligned for the next call.
			if len(f.Results) > 0 {
				f.Results = f.Results[1:]
			}
			return nil, err
		}
	}

	// Drain the Results queue.
	if len(f.Results) == 0 {
		return nil, ErrFakeExhausted
	}
	r := f.Results[0]
	f.Results = f.Results[1:]
	if r == nil {
		return nil, ErrFakeExhausted
	}
	return r, nil
}

// FakeInvoker is the test double for the Invoker interface.
// Most EX-N tests use FakeDriver instead (they exercise the
// Executor's policy through a real Executor + a stubbed
// Driver). FakeInvoker is for tests that need to bypass the
// Executor entirely — e.g., a CLI handler test that just wants
// to verify "Invoke was called with these args."
type FakeInvoker struct {
	mu sync.Mutex

	// Result is the canned InvokeResult returned by every Invoke call.
	Result *InvokeResult

	// Err is the canned error returned by every Invoke call.
	// A non-nil Err causes Invoke to return (nil, Err) and skip
	// Result.
	Err error

	// Calls records every InvokeRequest. Tests assert on the
	// captured requests.
	Calls []InvokeRequest
}

// Invoke implements the Invoker interface.
func (f *FakeInvoker) Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, req)
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Result, nil
}

// DriverName implements the NamedDriver interface so FakeDriver
// self-identifies in telemetry as "fake". Tests that want to
// verify the DriverName gets stamped into the TelemetryPayload
// rely on this — invoking through FakeDriver produces
// TelemetryPayload.DriverName == "fake".
func (f *FakeDriver) DriverName() string { return "fake" }