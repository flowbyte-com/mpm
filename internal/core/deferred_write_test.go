// deferred_write_test.go — the substrate's deferred durable-write queue.
//
// WHY THIS FILE EXISTS.
//
// The queue carries one hard promise: an observational write handed to
// it is still a durable write, and it lands. The tests below pin that
// promise and the mechanism's failure modes.
//
// The most important of them is
// TestDeferredWriteQueue_WorkerSurvivesAnEmptyQueue. An earlier
// revision of the worker returned from its loop the moment the queue
// was empty. That killed the worker permanently — `started` stayed
// true, so it was never restarted — and every obligation enqueued
// after that point was queued but never written. Any flush then waited
// for an obligation that could never land. The bug was
// race-dependent and invisible in isolation: it only surfaced when an
// earlier test in the same package drained the queue first.

package internal

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestDeferredQueue returns a private queue plus its cleanup.
//
// The package-level queue is shared by every test in this package, so a
// test that closed it would break the tests that follow. A private
// queue keeps these tests independent.
func newTestDeferredQueue(t *testing.T) *deferredWriteQueue {
	t.Helper()
	q := &deferredWriteQueue{notify: make(chan struct{})}
	t.Cleanup(func() { q.close() })
	return q
}

// close stops this queue's worker and reports the obligations it could
// not land.
func (q *deferredWriteQueue) close() int {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return 0
	}
	q.closed = true
	pending := len(q.items)
	q.items = nil
	q.signalLocked()
	q.mu.Unlock()
	return pending
}

// flushCtx returns a bounded flush context for a test.
func flushCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// waitForDeferred waits for a condition to become true, or fails the
// test. It is named apart from the package's existing waitFor helper.
func waitForDeferred(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestDeferredWriteQueue_WorkerSurvivesAnEmptyQueue is the regression
// test for the worker-lifetime defect.
//
// The worker must IDLE on an empty queue, not exit. If it exits, the
// next obligation is queued but never written, and the flush below
// times out.
func TestDeferredWriteQueue_WorkerSurvivesAnEmptyQueue(t *testing.T) {
	q := newTestDeferredQueue(t)

	// First obligation: the worker picks it up.
	var first atomic.Int64
	if !q.deferWrite("test:first", func() error {
		first.Add(1)
		return nil
	}) {
		t.Fatal("first obligation was not accepted")
	}
	if err := q.flush(flushCtx(t)); err != nil {
		t.Fatalf("flush after the first obligation: %v", err)
	}
	if got := first.Load(); got != 1 {
		t.Fatalf("first obligation ran %d times, want 1", got)
	}

	// The queue is now EMPTY and the worker is idle. A second
	// obligation must still land: this is exactly the case the earlier
	// revision failed, because its worker had already exited.
	var second atomic.Int64
	if !q.deferWrite("test:second", func() error {
		second.Add(1)
		return nil
	}) {
		t.Fatal("second obligation was not accepted")
	}
	if err := q.flush(flushCtx(t)); err != nil {
		t.Fatalf("flush after the second obligation: %v", err)
	}
	if got := second.Load(); got != 1 {
		t.Fatalf("second obligation ran %d times, want 1", got)
	}
}

// TestDeferredWriteQueue_RetriesContendedObligationUntilItLands pins
// the central promise: a contended write is RETRIED, not dropped.
//
// The bare pool write this queue replaces had NO retry budget at all —
// under contention it lost its artifact outright. The deferred write
// keeps the obligation pending until it lands.
func TestDeferredWriteQueue_RetriesContendedObligationUntilItLands(t *testing.T) {
	q := newTestDeferredQueue(t)

	var attempts atomic.Int64
	landed := make(chan struct{})
	if !q.deferWrite("test:contended", func() error {
		if attempts.Add(1) < 3 {
			return errors.New("database is locked")
		}
		close(landed)
		return nil
	}) {
		t.Fatal("obligation was not accepted")
	}

	select {
	case <-landed:
	case <-time.After(30 * time.Second):
		t.Fatalf("contended obligation never landed (attempts=%d)", attempts.Load())
	}
	if err := q.flush(flushCtx(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

// TestDeferredWriteQueue_DropsUnretryableFailureWithoutBlockingTheQueue
// pins the clearable/unclearable split.
//
// A closed database is not a wait: retrying it cannot succeed, and
// retrying forever would strand every later obligation behind it.
func TestDeferredWriteQueue_DropsUnretryableFailureWithoutBlockingTheQueue(t *testing.T) {
	q := newTestDeferredQueue(t)

	var attempts atomic.Int64
	if !q.deferWrite("test:closed", func() error {
		attempts.Add(1)
		return errors.New("sql: database is closed")
	}) {
		t.Fatal("obligation was not accepted")
	}

	// A later obligation must still run.
	var later atomic.Int64
	if !q.deferWrite("test:later", func() error {
		later.Add(1)
		return nil
	}) {
		t.Fatal("later obligation was not accepted")
	}

	if err := q.flush(flushCtx(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := later.Load(); got != 1 {
		t.Fatalf("later obligation ran %d times, want 1", got)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("unretryable obligation was attempted %d times, want 1", got)
	}
}

// TestDeferredWriteQueue_FailedObligationDoesNotMonopoliseTheQueue pins
// the no-head-of-line-blocking property.
//
// A contended obligation is put back with a later next-attempt time, so
// the obligations behind it get their turn. The contended item must not
// be re-attempted ahead of an obligation that is already due.
//
// An attempt that is IN FLIGHT does hold the queue — but every deferred
// write is bounded by the substrate's own busy-timeout and retry
// budget, so no obligation can wedge the queue permanently. That bound
// is a property of the closure, not of the queue.
func TestDeferredWriteQueue_FailedObligationDoesNotMonopoliseTheQueue(t *testing.T) {
	q := newTestDeferredQueue(t)

	var mu sync.Mutex
	var order []string

	// The first obligation fails twice with the clearable class.
	if !q.deferWrite("test:contended", func() error {
		mu.Lock()
		order = append(order, "contended")
		n := len(order)
		mu.Unlock()
		if n <= 2 {
			return errors.New("database is locked")
		}
		return nil
	}) {
		t.Fatal("contended obligation was not accepted")
	}

	// The second obligation is due immediately and must get its turn
	// before the contended one is re-attempted.
	if !q.deferWrite("test:other", func() error {
		mu.Lock()
		order = append(order, "other")
		mu.Unlock()
		return nil
	}) {
		t.Fatal("other obligation was not accepted")
	}

	if err := q.flush(flushCtx(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()

	if len(got) < 3 {
		t.Fatalf("attempt order = %v, want at least 3 attempts", got)
	}
	if got[1] != "other" {
		t.Fatalf("attempt order = %v, want the second obligation to take its turn before the contended one is re-attempted", got)
	}
}

// TestDeferredWriteQueue_IsBounded pins the capacity bound.
//
// An unbounded queue turns a write-lock stall into a memory leak.
//
// The bound is checked against the number of PENDING obligations, so
// the test first parks the worker on a blocked obligation: otherwise
// the worker drains items while the test fills the queue and the
// capacity is reached at an unpredictable point.
func TestDeferredWriteQueue_IsBounded(t *testing.T) {
	q := newTestDeferredQueue(t)

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	blocked := func() error {
		<-block
		return nil
	}

	// One obligation occupies the worker; it cannot complete.
	q.deferWrite("test:blocked", blocked)
	waitForDeferred(t, "the worker to take the blocking obligation", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.inflight == 1 && len(q.items) == 0
	})

	// Fill the queue to exactly its capacity.
	for i := 0; i < deferredWriteQueueCapacity-1; i++ {
		if !q.deferWrite("test:blocked", blocked) {
			t.Fatalf("obligation %d was rejected before the capacity was reached", i)
		}
	}
	q.mu.Lock()
	pending := len(q.items)
	q.mu.Unlock()
	if pending != deferredWriteQueueCapacity-1 {
		t.Fatalf("pending obligations = %d, want %d", pending, deferredWriteQueueCapacity-1)
	}

	// The obligation that reaches the capacity is accepted...
	if !q.deferWrite("test:at-capacity", blocked) {
		t.Fatalf("the queue rejected the obligation that reaches its capacity of %d", deferredWriteQueueCapacity)
	}
	// ...and the one past it is refused.
	if q.deferWrite("test:overflow", func() error { return nil }) {
		t.Fatalf("the queue accepted an obligation past its capacity of %d", deferredWriteQueueCapacity)
	}
}

// TestDeferDurableWrite_GlobalQueueAcceptsAndFlushes pins the
// package-level entry point, which is what the substrate's callers use.
func TestDeferDurableWrite_GlobalQueueAcceptsAndFlushes(t *testing.T) {
	var ran atomic.Int64
	if !DeferDurableWrite("test:global", func() error {
		ran.Add(1)
		return nil
	}) {
		t.Fatal("the process-wide queue rejected the obligation")
	}
	if err := FlushDeferredWrites(flushCtx(t)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if ran.Load() != 1 {
		t.Fatalf("obligation ran %d times, want 1", ran.Load())
	}
}

// TestDeferDurableWrite_RejectsNilClosure pins the nil guard: a nil
// closure would panic on the worker goroutine.
func TestDeferDurableWrite_RejectsNilClosure(t *testing.T) {
	if DeferDurableWrite("test:nil", nil) {
		t.Fatal("a nil closure was accepted")
	}
}

// TestDeferredWriteQueue_CloseReportsPendingObligations pins the
// shutdown path: an obligation the process could not land is COUNTED,
// not silently swallowed.
func TestDeferredWriteQueue_CloseReportsPendingObligations(t *testing.T) {
	q := &deferredWriteQueue{notify: make(chan struct{})}

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	for i := 0; i < 3; i++ {
		q.deferWrite("test:pending", func() error {
			<-block
			return nil
		})
	}

	// Give the worker a moment to take one item into flight, so the
	// pending count is the items still queued.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
	}()
	wg.Wait()

	pending := q.close()
	if pending < 1 {
		t.Fatalf("close reported %d pending obligations, want at least 1", pending)
	}
	// A closed queue refuses new work.
	if q.deferWrite("test:after-close", func() error { return nil }) {
		t.Fatal("a closed queue accepted an obligation")
	}
}
