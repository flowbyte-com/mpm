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
	"errors"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
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
//
// Ownership contract: this test asserts on the EXACT subprocess it
// created, not on the system-wide population of `sleep 60` processes.
// A global pgrep / proc scan is wrong: a developer's other tooling,
// editor, or sibling CI step can legitimately spawn a process named
// "sleep 60" and create a false-positive leak. The right signal is
// the captured child PID + the handler-completed signal from
// cmd.Wait().
func TestF3_HandlerSubprocessKilledOnCancel(t *testing.T) {
	s := newTestScheduler(t)

	// Channels for deterministic synchronization (NO wall-clock
	// coordination beyond the cancel path):
	//   - handlerEntered: closes when the handler goroutine starts;
	//     proves production reached h(ctx, w).
	//   - handlerReturned: receives the handler's exit error; closing
	//     proves cmd.Wait() returned, which guarantees the child has
	//     been reaped by the OS (Go's os/exec contract).
	//   - childPID:        receives the spawned child PID as the
	//     first thing the handler does after Start().
	var (
		handlerEntered  = make(chan struct{})
		childPID        = make(chan int, 1)
		handlerReturned = make(chan error, 1)
		enteredOnce     sync.Once
	)

	s.Register("spawn_sleeper", func(ctx context.Context, w Wake) error {
		enteredOnce.Do(func() { close(handlerEntered) })
		cmd := exec.CommandContext(ctx, "sleep", "60")
		if err := cmd.Start(); err != nil {
			handlerReturned <- err
			return err
		}
		// Publish the PID for the test to assert on. Buffer-1 send
		// so we never block even if the test has already moved on.
		childPID <- cmd.Process.Pid
		waitErr := cmd.Wait()
		handlerReturned <- waitErr
		return waitErr
	})

	w := Wake{ID: "wk-subproc-001", Metadata: map[string]interface{}{"kind": "spawn_sleeper"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	execDone := make(chan error, 1)
	go func() { execDone <- s.executeOne(ctx, w) }()

	// 1. Wait for the handler to start AND publish the child PID.
	//    Both are required before we cancel — we want to assert on
	//    the exact child that this invocation of the test created.
	var pid int
	select {
	case <-handlerEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started; production must invoke h(ctx, w)")
	}
	select {
	case pid = <-childPID:
	case <-time.After(2 * time.Second):
		t.Fatal("handler started but did not publish child PID")
	}

	// 2. Cancel. The handler's exec.CommandContext will SIGKILL the
	//    child, and cmd.Wait() will return with a signal-killed error.
	cancelStart := time.Now()
	cancel()

	// 3. executeOne must return promptly on the cancel path
	//    (it selects on ctx.Done() and does NOT wait for the handler
	//    goroutine — by design, so a hung handler cannot pin a tick).
	select {
	case err := <-execDone:
		require.Error(t, err, "executeOne must return an error on cancel")
		require.Less(t, time.Since(cancelStart), 2*time.Second,
			"executeOne took too long to return after cancel; ctx propagation likely broken")
		t.Logf("executeOne returned in %v after cancel: %v", time.Since(cancelStart), err)
	case <-time.After(3 * time.Second):
		t.Fatal("executeOne did not return within 3s of cancel; goroutine likely leaked")
	}

	// 4. Wait for the handler goroutine itself to return. This is the
	//    KEY synchronization: cmd.Wait() returning is Go's os/exec
	//    guarantee that the child has been reaped. We must not assert
	//    on PID liveness before this fires, or we race against the
	//    OS's process-reaper. The 5s ceiling is a deadlock guard, not
	//    a synchronization primitive — the real synchronization is
	//    handlerReturned closing.
	select {
	case handlerErr := <-handlerReturned:
		// We expect a non-nil error ("signal: killed" or
		// "context canceled" depending on Go version). We do not
		// require.ErrorIs here because the exact wording of the
		// Wait() error is platform/version-specific; the only
		// contract we pin is that Wait() returned (which proves
		// the child is reaped).
		_ = handlerErr
	case <-time.After(5 * time.Second):
		t.Fatalf("handler did not return within 5s of cancel; "+
			"cmd.Wait() blocked — the spawned child (pid=%d) was not "+
			"reaped, indicating a real production regression (NOT a "+
			"test-only flake). Inspect the child with: ps -p %d",
			pid, pid)
	}

	// 5. Defense-in-depth: even though cmd.Wait() has reaped the
	//    child, double-check the exact PID is gone. We use
	//    syscall.Kill(pid, 0) which returns ESRCH if the process
	//    does not exist, and 0 if it does (it does NOT send a
	//    signal). This is scoped to OUR pid, never a global scan.
	//
	//    Skip on non-linux: Windows process liveness via signal 0
	//    is unreliable and this scheduler is linux-only.
	if runtime.GOOS == "linux" {
		if err := syscall.Kill(pid, 0); err == nil {
			t.Fatalf("child pid=%d is still alive after handler returned "+
				"from cmd.Wait(); exec.CommandContext did not actually "+
				"terminate the child on ctx cancel", pid)
		} else if !errors.Is(err, syscall.ESRCH) {
			// EPERM is "process exists, you can't signal it" —
			// that still proves the process is alive, which is a
			// leak. Anything other than ESRCH is a failure.
			t.Fatalf("unexpected error from kill(%d, 0): %v "+
				"(want ESRCH; EPERM would also indicate the process is alive)",
				pid, err)
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
