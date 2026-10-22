// scheduler_lifecycle_test.go — the Run goroutine's test lifecycle.
//
// The scheduler's Run loop runs in a goroutine that the test starts.
// Context cancellation only *signals* that goroutine; it does not wait
// for it. A Run goroutine that is still alive when the test function
// returns keeps using the scheduler's SQLite files under t.TempDir(),
// and the framework's cleanup then fails with
//
//	TempDir RemoveAll cleanup: unlinkat .../001: directory not empty
//
// That is the failure CI run 39757594086 recorded for
// TestScheduler_FiresAtTargetTime: the orphaned goroutine's cancelled
// deadline query logged "compute earliest deadline failed: context
// canceled" *after* the test that started it had already been reported
// as failed.
//
// These tests pin the two halves of the correction: cancellation alone
// does not join, and stopScheduler does.

package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestScheduler_CancelDoesNotWaitForRunGoroutine pins the mechanism
// behind the flake: cancelling the context is not a join.
//
// The Run goroutine is parked inside a tick handler, so it cannot
// observe ctx.Done() at all. After cancel() returns, the goroutine is
// provably still running — which is exactly the state a test returns
// in when it cancels and does not wait.
func TestScheduler_CancelDoesNotWaitForRunGoroutine(t *testing.T) {
	s := newTestScheduler(t)
	s.SetHeartbeat(0)

	var once sync.Once
	entered := make(chan struct{})
	release := make(chan struct{})
	s.RegisterTickHandler("blocker", func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, 1*time.Second) }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("tick handler never ran; Run is not calling tick handlers")
	}

	// The Run goroutine is now parked inside the tick handler.
	cancel()

	select {
	case <-done:
		t.Fatal("Run returned while the tick handler was still blocked; " +
			"cancellation is expected to signal, not to join")
	case <-time.After(200 * time.Millisecond):
		// Expected: Run is still running after cancel() returned.
	}

	// Release the handler so Run can finish, then join it properly.
	close(release)
	stopScheduler(t, cancel, done)
}

// TestScheduler_StopSchedulerWaitsForRunGoroutine pins the correction:
// once stopScheduler returns, the Run goroutine has exited.
//
// This is the invariant the corrected success paths depend on — the
// goroutine must be gone before the test function returns, so that
// t.TempDir() cleanup cannot race with it.
func TestScheduler_StopSchedulerWaitsForRunGoroutine(t *testing.T) {
	s := newTestScheduler(t)
	s.SetHeartbeat(0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- s.Run(ctx, 1*time.Second)
	}()

	// Let Run reach its select loop before stopping it.
	time.Sleep(200 * time.Millisecond)

	stopScheduler(t, cancel, done)

	// stopScheduler's guarantee is that Run() has returned — which is
	// exactly what receiving from done proves. The goroutine wrapper
	// may still be unwinding after its send completes, so wait for it
	// with a bounded deadline: an unbounded wait would hide a real
	// leak, and a bare non-blocking check would race with the wrapper.
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the Run goroutine did not finish after stopScheduler returned")
	}
}
