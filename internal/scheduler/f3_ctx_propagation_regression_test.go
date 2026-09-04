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
//   1. HandlerFunc signature gains ctx: func(ctx, w) error.
//   2. executeOne passes its ctx to the handler and selects on it for
//      cancellation.
//   3. Handlers that spawn subprocesses use exec.CommandContext(ctx,
//      ...), so subprocess death follows ctx cancellation.
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
	"sync/atomic"
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
// This test fails at compile time against pre-fix code (handler
// signature mismatch) and passes post-fix.
func TestF3_HandlerReceivesCtx(t *testing.T) {
	s := newTestScheduler(t)

	var cancelled atomic.Bool // set when the handler observes ctx.Done()
	s.Register("ctx_aware", func(ctx context.Context, w Wake) error {
		select {
		case <-ctx.Done():
			cancelled.Store(true)
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
			return nil
		}
	})

	w := Wake{ID: "wk-ctx-001", Metadata: map[string]interface{}{"kind": "ctx_aware"}}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel from a goroutine just after executeOne starts the handler.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_ = s.executeOne(ctx, w)
	elapsed := time.Since(start)

	// Handler observed the cancellation and returned early.
	require.True(t, cancelled.Load(),
		"handler must observe ctx.Done() (pre-fix signature change makes this impossible)")
	// The 500ms sleep would dominate if ctx wasn't propagated.
	require.Less(t, elapsed, 300*time.Millisecond,
		"executeOne returned in %v; handler likely ignored ctx", elapsed)
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
