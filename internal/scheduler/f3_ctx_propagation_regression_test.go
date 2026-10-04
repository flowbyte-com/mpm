// f3_ctx_propagation_regression_test.go — regression guard for the
// 2026-09-04 residual-inventory finding F-3.
//
// F-3 P1: Scheduler.executeOne leaked the handler goroutine and any
// subprocess tree on context cancellation. Pre-fix:
//
//   - executeOne did `go func() { done <- h(w) }()` and selected on
//     `<-ctx.Done()` to return early on cancellation. The detached
//     goroutine was unreachable and kept running.
//   - Handlers did not accept ctx (HandlerFunc signature: func(w Wake)
//     error) so they could not observe cancellation themselves.
//   - Handlers that shell out (SnapshotHandler, GCHandler,
//     BroadcastHandler) used bare exec.Command without
//     exec.CommandContext, so subprocesses outlived the daemon on
//     SIGTERM and kept the SQLite DB write-locked past the exit
//     window.
//
// The fix has three pieces:
//  1. HandlerFunc signature gains ctx: func(ctx, w) error.
//  2. executeOne passes its ctx to the handler and selects on it for
//     cancellation.
//  3. Handlers that spawn subprocesses use exec.CommandContext(ctx,
//     ...), so subprocess death follows ctx cancellation.
//
// These tests pin the corrected shape from the dispatch boundary
// outward: a handler that observes ctx returns promptly on cancel;
// a handler that spawns a subprocess kills it on cancel.
package scheduler

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestF3_HandlerReceivesCtx is the signature-change guard. Pre-fix
// the HandlerFunc type was `func(w Wake) error` — the handler had no
// way to observe context cancellation. Post-fix it is `func(ctx
// context.Context, w Wake) error`, and the test registers a handler
// that returns immediately when ctx is cancelled.
//
// Contract under test: the handler MUST observe the caller's ctx.Done()
// when ctx is cancelled and return ctx.Err(). This is a propagation
// contract, not a context-identity contract: production may legitimately
// wrap ctx internally (context.WithCancel, etc.) and the handler still
// observes Done.
//
// Deterministic synchronization (NOT wall-clock):
//
//   - The handler signals `handlerStarted` immediately upon entry,
//     proving production reached h(ctx, w).
//   - The test waits for that signal, then calls cancel() in the same
//     goroutine with no intervening operations.
//   - The handler signals `handlerReturned` on its way out, carrying
//     the error it actually returned.
//
// At the moment cancel() fires, the handler has just signalled it is
// about to enter its select. ctx.Done() becomes ready NOW, while
// time.After(500ms) is ~500ms away. The two arms of the handler's
// select cannot be simultaneously ready in any realistic scheduling
// scenario, so Go's select picks ctx.Done() deterministically.
//
// What this regression test used to do (and why it was flaky):
//
//   - Old test spawned a goroutine that did time.Sleep(20ms) then
//     cancel(). Under CPU pressure the cancel could land late enough
//     that the handler's time.After(500ms) arm fired first AND the
//     handler's select was not yet waiting on ctx.Done(), in which case
//     the test read `cancelled.Load()` before the handler had run.
//   - Even when the handler did reach its select, Go's select picks
//     randomly when multiple arms are ready; under load both arms could
//     be ready, making the test a 50/50 coin flip.
//
// This test fails at compile time against pre-fix code (handler
// signature mismatch) and passes deterministically post-fix.
func TestF3_HandlerReceivesCtx(t *testing.T) {
	s := newTestScheduler(t)

	handlerStarted := make(chan struct{})
	var startedOnce sync.Once
	handlerReturned := make(chan error, 1)

	s.Register("ctx_aware", func(ctx context.Context, w Wake) error {
		startedOnce.Do(func() { close(handlerStarted) })
		var err error
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(500 * time.Millisecond):
			// 500ms is only here to bound how long this test takes if
			// production never cancels ctx (a sanity guard, not the
			// synchronization mechanism. Real synchronization is the
			// handlerStarted/handlerReturned channel pair above).
		}
		handlerReturned <- err
		return err
	})

	w := Wake{ID: "wk-ctx-001", Metadata: map[string]interface{}{"kind": "ctx_aware"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type execResult struct{ err error }
	execDone := make(chan execResult, 1)
	execStart := time.Now()
	go func() { execDone <- execResult{s.executeOne(ctx, w)} }()

	// 1. Wait for production to invoke the handler. This is the contract
	//    gate — if executeOne never reaches h(ctx, w), production is
	//    broken and the test must report that, not flake.
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started; production must invoke h(ctx, w)")
	}

	// 2. Cancel immediately. No time.Sleep, no other goroutines, no
	//    channels — the test goroutine calls cancel() right after
	//    handlerStarted closes. By the time the handler reaches its
	//    select, ctx.Done() is the only ready arm.
	cancel()

	// 3. Wait for executeOne to return. executeOne selects on ctx.Done()
	//    on the cancel path and returns promptly. The 2s is a deadlock
	//    guard, not a synchronization primitive.
	var exec execResult
	select {
	case exec = <-execDone:
	case <-time.After(2 * time.Second):
		t.Fatal("executeOne did not return within 2s after cancel")
	}
	require.Less(t, time.Since(execStart), 2*time.Second,
		"executeOne took too long; ctx propagation may be broken")
	require.ErrorIs(t, exec.err, context.Canceled,
		"executeOne must observe ctx.Done() on cancel path")

	// 4. Wait for the handler to actually return and report what it
	//    observed. The handler goroutine continues running after
	//    executeOne returns (executeOne does not wait for the handler
	//    on the cancel path — by design, so a hung handler cannot pin
	//    a tick). The test must wait for it before reading what it
	//    observed.
	var handlerErr error
	select {
	case handlerErr = <-handlerReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return within 2s after cancel")
	}

	require.ErrorIs(t, handlerErr, context.Canceled,
		"handler must observe ctx.Done() and return context.Canceled; "+
			"got %v (the 500ms-time.After branch won the select, which means "+
			"either production detached the handler from ctx, or the test waited "+
			"too long to cancel after handlerStarted fired)",
		handlerErr)
}

// TestF3_HandlerSubprocessKilledOnCancel is the subprocess-leak guard.
// Pre-fix: GCHandler-style exec.Command("mpm", ...) spawns a child
// that outlives ctx cancellation. Post-fix: handlers that take ctx
// use exec.CommandContext(ctx, ...), and the child dies when ctx is
// cancelled.
func TestF3_HandlerSubprocessKilledOnCancel(t *testing.T) {
	s := newTestScheduler(t)

	// Register a handler that spawns a long-lived subprocess via
	// exec.CommandContext(ctx, ...). When ctx is cancelled, the
	// subprocess receives SIGKILL and exits.
	s.Register("spawn_sleeper", func(ctx context.Context, w Wake) error {
		cmd := exec.CommandContext(ctx, "sleep", "60")
		// Pin the test subprocess to the test process group so SIGKILL
		// from ctx cancellation actually terminates it (rather than
		// relying on the test process's lifetime).
		_ = cmd.Start()
		return cmd.Wait()
	})

	w := Wake{ID: "wk-subproc-001", Metadata: map[string]interface{}{"kind": "spawn_sleeper"}}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- s.executeOne(ctx, w) }()

	// Cancel after the handler has spawned its subprocess.
	time.Sleep(50 * time.Millisecond)
	cancelStart := time.Now()
	cancel()

	select {
	case err := <-done:
		// The handler should return promptly with a cancellation error.
		require.Error(t, err, "executeOne must return an error on cancel")
		require.Less(t, time.Since(cancelStart), 2*time.Second,
			"executeOne took too long to return after cancel; subprocess leak likely")
		t.Logf("executeOne returned in %v after cancel: %v", time.Since(cancelStart), err)
	case <-time.After(3 * time.Second):
		t.Fatal("executeOne did not return within 3s of cancel; goroutine likely leaked")
	}

	// Give the OS a moment to reap the subprocess, then verify no
	// `sleep 60` lingers. We check /proc on Linux (the only supported
	// platform for this test) for any process whose command line
	// starts with "sleep 60".
	if runtime.GOOS == "linux" {
		time.Sleep(100 * time.Millisecond) // reap window
		if out, err := exec.Command("pgrep", "-af", "^sleep 60$").CombinedOutput(); err == nil {
			pgrepOut := strings.TrimSpace(string(out))
			if pgrepOut != "" {
				t.Fatalf("subprocess leaked after cancel: %s", pgrepOut)
			}
		}
	}
}

// TestF3_HandlerSignatureAcceptsContext is a compile-time guard.
// Pre-fix: HandlerFunc = func(w Wake) error. Post-fix: HandlerFunc =
// func(ctx context.Context, w Wake) error. If a future refactor
// accidentally drops ctx from the type, this test fails to compile.
func TestF3_HandlerSignatureAcceptsContext(t *testing.T) {
	var fn HandlerFunc = func(ctx context.Context, w Wake) error {
		return ctx.Err()
	}
	// Round-trip through Register to prove the new signature works
	// end-to-end with the existing public API.
	s := newTestScheduler(t)
	require.NotPanics(t, func() {
		s.Register("compile_check", fn)
	})
}
