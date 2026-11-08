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
//
// The second most important group is the capacity tests. They pin the
// property that an ACCEPTED obligation is never lost by the queue's own
// capacity management — the property whose absence was the blocking
// finding in the independent review of PR #235.
//
// EVERY test below uses deterministic concurrency controls (channels and
// condition polls), never timing-dependent sleeps. A test that pins a
// durability promise must not be able to fail for a reason that has
// nothing to do with the promise.

package internal

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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
//
// The cleanup calls the PRODUCTION close method, not a local copy: the
// counting API must be the code that runs in production, and a test
// that exercised a copy of it would prove nothing about the real one.
func newTestDeferredQueue(t *testing.T) *deferredWriteQueue {
	t.Helper()
	q := &deferredWriteQueue{notify: make(chan struct{})}
	t.Cleanup(func() { q.close() })
	return q
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
//
// It polls a STATE, not a clock: the condition is the thing under test,
// so the wait is bounded by the test's own deadline and not by a
// guessed sleep.
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
	if !q.deferWrite(DeferredWriteTelemetry, "test:first", func() error {
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
	if !q.deferWrite(DeferredWriteTelemetry, "test:second", func() error {
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
	if !q.deferWrite(DeferredWriteTelemetry, "test:contended", func() error {
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
//
// It also pins the CLASS contract: an unretryable EVIDENCE obligation is
// a loss a flush must report, and an unretryable TELEMETRY obligation
// is a loss the telemetry contract allows — it is counted and logged,
// but it does not fail a caller that is waiting for audit evidence.
func TestDeferredWriteQueue_DropsUnretryableFailureWithoutBlockingTheQueue(t *testing.T) {
	q := newTestDeferredQueue(t)

	var attempts atomic.Int64
	if !q.deferWrite(DeferredWriteEvidence, "test:closed", func() error {
		attempts.Add(1)
		return errors.New("sql: database is closed")
	}) {
		t.Fatal("obligation was not accepted")
	}

	// A later obligation must still run.
	var later atomic.Int64
	if !q.deferWrite(DeferredWriteTelemetry, "test:later", func() error {
		later.Add(1)
		return nil
	}) {
		t.Fatal("later obligation was not accepted")
	}

	err := q.flush(flushCtx(t))
	if err == nil {
		t.Fatal("the flush reported success, but an accepted EVIDENCE obligation was lost")
	}
	if !strings.Contains(err.Error(), "lost") {
		t.Fatalf("the flush error does not report the loss: %v", err)
	}
	if got := later.Load(); got != 1 {
		t.Fatalf("later obligation ran %d times, want 1", got)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("unretryable obligation was attempted %d times, want 1", got)
	}
	if got := q.stats().LostEvidence; got != 1 {
		t.Fatalf("lost evidence obligations = %d, want 1", got)
	}
}

// TestDeferredWriteQueue_TelemetryLossDoesNotFailTheFlush pins the other
// half of the class contract.
//
// Telemetry is losable BY CONTRACT — the retrieval layer's own header
// says the user-facing retrieval must not fail because a telemetry write
// failed. So a lost telemetry obligation must not fail a caller that is
// waiting for audit evidence.
//
// It is still counted and still logged. Only the flush's error signal
// is reserved for the class whose loss breaks a promise.
func TestDeferredWriteQueue_TelemetryLossDoesNotFailTheFlush(t *testing.T) {
	q := newTestDeferredQueue(t)

	if !q.deferWrite(DeferredWriteTelemetry, "test:terminal", func() error {
		return errors.New("UNIQUE constraint failed: not retryable")
	}) {
		t.Fatal("obligation was not accepted")
	}

	if err := q.flush(flushCtx(t)); err != nil {
		t.Fatalf("the flush failed because of a lost TELEMETRY obligation: %v", err)
	}
	stats := q.stats()
	if stats.LostTelemetry != 1 {
		t.Fatalf("lost telemetry obligations = %d, want 1 — a telemetry loss is still counted", stats.LostTelemetry)
	}
	if stats.LostEvidence != 0 {
		t.Fatalf("lost evidence obligations = %d, want 0", stats.LostEvidence)
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
	if !q.deferWrite(DeferredWriteTelemetry, "test:contended", func() error {
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
	if !q.deferWrite(DeferredWriteTelemetry, "test:other", func() error {
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
// The bound is checked against the obligations the queue is
// RESPONSIBLE FOR — the ones waiting plus the one being written — so
// the test first parks the worker on a blocked obligation: otherwise
// the worker drains items while the test fills the queue and the
// capacity is reached at an unpredictable point.
func TestDeferredWriteQueue_IsBounded(t *testing.T) {
	q := newTestDeferredQueue(t)

	block := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	t.Cleanup(release)

	started := make(chan struct{})
	// One obligation occupies the worker; it cannot complete.
	q.deferWrite(DeferredWriteEvidence, "test:blocked", func() error {
		close(started)
		<-block
		return nil
	})
	<-started

	// Fill the queue to exactly its capacity. The in-flight obligation's
	// slot is RESERVED, so the queue accepts one fewer pending
	// obligation than its capacity and refuses the one past that.
	for i := 0; i < deferredWriteQueueCapacity-1; i++ {
		if !q.deferWrite(DeferredWriteEvidence, "test:blocked", func() error {
			<-block
			return nil
		}) {
			t.Fatalf("obligation %d was rejected before the capacity was reached", i)
		}
	}
	q.mu.Lock()
	pending := len(q.items)
	inflight := q.inflight
	q.mu.Unlock()
	if pending != deferredWriteQueueCapacity-1 {
		t.Fatalf("pending obligations = %d, want %d", pending, deferredWriteQueueCapacity-1)
	}
	if pending+inflight != deferredWriteQueueCapacity {
		t.Fatalf("the queue holds %d obligation(s) (%d pending + %d in flight), want %d",
			pending+inflight, pending, inflight, deferredWriteQueueCapacity)
	}

	// The obligation that reaches the capacity is refused — the
	// in-flight obligation's slot is reserved for its retry.
	if q.deferWrite(DeferredWriteEvidence, "test:overflow", func() error { return nil }) {
		t.Fatalf("the queue accepted an obligation past its capacity of %d", deferredWriteQueueCapacity)
	}
}

// TestDeferredWriteQueue_ProducersCannotConsumeCapacityReservedForInFlightRetries
// pins the accounting rule that makes the re-queue safe.
//
// An in-flight obligation may need to be re-queued when its attempt
// fails with the clearable class, and that re-queue MUST succeed: the
// obligation was accepted, and an accepted obligation is retried until
// it lands.
//
// If a new producer could take the in-flight slot, that promise would
// break exactly when the queue is saturated — which is precisely when
// it matters. So the in-flight slot is RESERVED, and the total the
// queue is responsible for never exceeds its capacity.
func TestDeferredWriteQueue_ProducersCannotConsumeCapacityReservedForInFlightRetries(t *testing.T) {
	q := newTestDeferredQueue(t)

	block := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	t.Cleanup(release)

	started := make(chan struct{})
	q.deferWrite(DeferredWriteEvidence, "test:blocked", func() error {
		close(started)
		<-block
		return nil
	})
	<-started

	// Fill until the queue refuses. The worker is parked, so nothing
	// drains and the fill point is deterministic.
	for i := 0; i < 4*deferredWriteQueueCapacity; i++ {
		if !q.deferWrite(DeferredWriteEvidence, "test:filler", func() error { return nil }) {
			break
		}
	}

	q.mu.Lock()
	pending := len(q.items)
	inflight := q.inflight
	q.mu.Unlock()

	if pending+inflight > deferredWriteQueueCapacity {
		t.Fatalf("the queue is holding %d obligation(s) (%d pending + %d in flight), past its capacity of %d — "+
			"a producer consumed the slot reserved for an in-flight retry",
			pending+inflight, pending, inflight, deferredWriteQueueCapacity)
	}
	if pending != deferredWriteQueueCapacity-1 {
		t.Fatalf("pending obligations = %d, want %d — the in-flight obligation's slot must be reserved",
			pending, deferredWriteQueueCapacity-1)
	}
}

// TestDeferredWriteQueue_AcceptedObligationIsNotLostWhenTheQueueIsFull
// is the regression test for the blocking finding of the independent
// review.
//
// The defect it pins: when the queue was full and an in-flight
// obligation failed with the clearable class, the re-queue was skipped
// behind a capacity guard and the obligation was DISCARDED — silently,
// with no warning, no count and no report. The queue had accepted it
// and told the caller it was accepted.
//
// The fix is the accounting: the in-flight slot is reserved, so the
// re-queue always has somewhere to go.
//
// The test is deterministic. The in-flight obligation blocks on a
// channel the test controls; the queue is filled until it refuses; the
// obligation is then released and fails with the clearable class. It
// MUST be retried, and it MUST run exactly twice — once failing, once
// landing. A third attempt would mean the retry bookkeeping duplicated
// the obligation.
func TestDeferredWriteQueue_AcceptedObligationIsNotLostWhenTheQueueIsFull(t *testing.T) {
	q := newTestDeferredQueue(t)

	var attempts atomic.Int64
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFn)

	started := make(chan struct{})
	landed := make(chan struct{})
	if !q.deferWrite(DeferredWriteEvidence, "test:contended", func() error {
		n := attempts.Add(1)
		if n == 1 {
			close(started)
			<-release
			return errors.New("database is locked")
		}
		close(landed)
		return nil
	}) {
		t.Fatal("the obligation was not accepted")
	}
	<-started

	// Fill the queue until it refuses. The worker is parked on the
	// contended obligation, so nothing drains.
	for i := 0; i < 4*deferredWriteQueueCapacity; i++ {
		if !q.deferWrite(DeferredWriteEvidence, "test:filler", func() error { return nil }) {
			break
		}
	}

	// The in-flight obligation now fails with the clearable class, with
	// the queue at its capacity.
	releaseFn()

	select {
	case <-landed:
	case <-time.After(30 * time.Second):
		t.Fatalf("the ACCEPTED obligation was LOST: it ran %d time(s) and was never retried; "+
			"the queue's stats report %d lost obligation(s)",
			attempts.Load(), q.stats().Lost())
	}

	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want exactly 2 (one failure, one retry) — a third attempt would mean the "+
			"retry bookkeeping duplicated the obligation", got)
	}
	if lost := q.stats().Lost(); lost != 0 {
		t.Fatalf("the queue reported %d lost obligation(s); an accepted obligation must not be lost", lost)
	}
}

// TestDeferredWriteQueue_TelemetryCannotConsumeTheEvidenceReserve pins
// the class separation.
//
// Telemetry is best-effort BY CONTRACT. Audit evidence is not: losing
// it loses the record of what happened. So a slice of the queue's
// capacity is reserved for evidence, and telemetry may not consume it.
//
// A burst of telemetry — five rows per wake request — must not be able
// to starve the evidence the queue exists to protect.
func TestDeferredWriteQueue_TelemetryCannotConsumeTheEvidenceReserve(t *testing.T) {
	q := newTestDeferredQueue(t)

	block := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	t.Cleanup(release)

	started := make(chan struct{})
	q.deferWrite(DeferredWriteEvidence, "test:blocked", func() error {
		close(started)
		<-block
		return nil
	})
	<-started

	// Fill with TELEMETRY. It stops at its own ceiling, which is below
	// the total capacity.
	telemetryAccepted := 0
	for i := 0; i < 4*deferredWriteQueueCapacity; i++ {
		if !q.deferWrite(DeferredWriteTelemetry, "test:telemetry", func() error { return nil }) {
			break
		}
		telemetryAccepted++
	}
	if telemetryAccepted > deferredWriteTelemetryCapacity {
		t.Fatalf("the queue accepted %d telemetry obligation(s), past the telemetry ceiling of %d",
			telemetryAccepted, deferredWriteTelemetryCapacity)
	}

	// EVIDENCE must still be accepted: the telemetry ceiling, not the
	// total capacity, is the binding constraint.
	evidenceAccepted := 0
	for i := 0; i < 4*deferredWriteQueueCapacity; i++ {
		if !q.deferWrite(DeferredWriteEvidence, "test:evidence", func() error { return nil }) {
			break
		}
		evidenceAccepted++
	}
	if evidenceAccepted == 0 {
		t.Fatalf("the queue refused an EVIDENCE obligation while only telemetry had filled it "+
			"(%d telemetry accepted, ceiling %d, total capacity %d)",
			telemetryAccepted, deferredWriteTelemetryCapacity, deferredWriteQueueCapacity)
	}

	// The total bound still holds.
	q.mu.Lock()
	total := len(q.items) + q.inflight
	q.mu.Unlock()
	if total > deferredWriteQueueCapacity {
		t.Fatalf("the queue is holding %d obligation(s), past its capacity of %d", total, deferredWriteQueueCapacity)
	}
}

// TestDeferredWriteQueue_RejectedObligationIsCountedAndNeverRun pins
// the refusal contract.
//
// An obligation the queue REFUSES was never accepted: it must not be
// run, and it must be counted as refused — not as lost, because it was
// never the queue's responsibility.
func TestDeferredWriteQueue_RejectedObligationIsCountedAndNeverRun(t *testing.T) {
	q := newTestDeferredQueue(t)

	block := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	t.Cleanup(release)

	started := make(chan struct{})
	q.deferWrite(DeferredWriteEvidence, "test:blocked", func() error {
		close(started)
		<-block
		return nil
	})
	<-started

	for i := 0; i < 4*deferredWriteQueueCapacity; i++ {
		if !q.deferWrite(DeferredWriteEvidence, "test:filler", func() error { return nil }) {
			break
		}
	}

	var ran atomic.Int64
	rejectedBefore := q.stats().RejectedFull
	if q.deferWrite(DeferredWriteEvidence, "test:refused", func() error {
		ran.Add(1)
		return nil
	}) {
		t.Fatal("the queue accepted an obligation past its capacity")
	}
	if got := ran.Load(); got != 0 {
		t.Fatalf("the refused obligation ran %d time(s); a refused obligation must never be written", got)
	}

	stats := q.stats()
	if stats.RejectedFull != rejectedBefore+1 {
		t.Fatalf("rejected obligations went from %d to %d, want exactly one more refusal",
			rejectedBefore, stats.RejectedFull)
	}
	if stats.Lost() != 0 {
		t.Fatalf("the queue reported %d lost obligation(s); a REFUSED obligation was never accepted, so it is not lost",
			stats.Lost())
	}
	if stats.Accepted != stats.Landed+stats.Lost()+int64(stats.Pending)+int64(stats.InFlight) {
		t.Fatalf("the accounting does not balance: accepted=%d landed=%d lost=%d pending=%d in_flight=%d",
			stats.Accepted, stats.Landed, stats.Lost(), stats.Pending, stats.InFlight)
	}
}

// TestDeferredWriteQueue_FlushReportsAnAcceptedObligationThatWasLost
// pins the flush's signal.
//
// A flush that returns nil must mean "everything that was accepted has
// landed". If an accepted obligation was lost while the queue drained,
// the flush reports that instead of a bare success — a caller that
// treats a nil flush as "the row is there" must not be told a
// falsehood.
func TestDeferredWriteQueue_FlushReportsAnAcceptedObligationThatWasLost(t *testing.T) {
	q := newTestDeferredQueue(t)

	if !q.deferWrite(DeferredWriteEvidence, "test:terminal", func() error {
		return errors.New("UNIQUE constraint failed: not retryable")
	}) {
		t.Fatal("obligation was not accepted")
	}

	err := q.flush(flushCtx(t))
	if err == nil {
		t.Fatal("the flush reported success even though an accepted obligation was lost")
	}
	if !strings.Contains(err.Error(), "lost") {
		t.Fatalf("the flush error does not report the loss: %v", err)
	}
	if got := q.stats().DroppedTerminal; got != 1 {
		t.Fatalf("dropped terminal obligations = %d, want 1", got)
	}
}

// capturingHandler records the message of every slog record.
type capturingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) contains(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// captureLogs installs a capturing slog handler for the duration of a
// test and returns it.
func captureLogs(t *testing.T) *capturingHandler {
	t.Helper()
	h := &capturingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return h
}

// TestDeferredWriteQueue_RetryDiagnosticMatchesTheActualOutcome pins
// the diagnostic's truthfulness.
//
// The queue's log lines are its REPORT on its own behaviour. A line
// that promises a retry must be backed by a retry that is actually
// scheduled, and a loss must be reported as a loss.
//
// The defect it pins: a retryable failure that arrived after the queue
// was closed took the "will retry" branch — the queue logged a promise
// it could not keep, and dropped the obligation instead.
func TestDeferredWriteQueue_RetryDiagnosticMatchesTheActualOutcome(t *testing.T) {
	// CASE 1: the queue is OPEN. The failure is retryable, so a retry
	// IS scheduled and the obligation lands.
	logs := captureLogs(t)
	q := newTestDeferredQueue(t)

	var attempts atomic.Int64
	landed := make(chan struct{})
	if !q.deferWrite(DeferredWriteTelemetry, "test:contended", func() error {
		if attempts.Add(1) == 1 {
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
		t.Fatalf("the contended obligation never landed (attempts=%d)", attempts.Load())
	}
	if !logs.contains("a retry is scheduled") {
		t.Fatalf("the queue did not report the scheduled retry; captured: %v", logs.msgs)
	}
	if logs.contains("will not be retried") {
		t.Fatalf("the queue reported a terminal failure for an obligation it retried; captured: %v", logs.msgs)
	}

	// CASE 2: the queue is CLOSED. The same failure cannot be retried,
	// so the queue must say so — and must NOT promise a retry.
	logs2 := captureLogs(t)
	q2 := newTestDeferredQueue(t)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFn)

	started := make(chan struct{})
	q2.deferWrite(DeferredWriteTelemetry, "test:contended", func() error {
		close(started)
		<-release
		return errors.New("database is locked")
	})
	<-started

	// Close the queue while the obligation is in flight, then let the
	// attempt fail.
	q2.close()
	releaseFn()

	waitForDeferred(t, "the in-flight attempt to finish", func() bool {
		q2.mu.Lock()
		defer q2.mu.Unlock()
		return q2.inflight == 0
	})

	if logs2.contains("a retry is scheduled") {
		t.Fatalf("the queue promised a retry it did not schedule; captured: %v", logs2.msgs)
	}
	if !logs2.contains("dropped") {
		t.Fatalf("the queue did not report the loss; captured: %v", logs2.msgs)
	}
}

// TestDeferDurableWrite_GlobalQueueAcceptsAndFlushes pins the
// package-level entry point, which is what the substrate's callers use.
func TestDeferDurableWrite_GlobalQueueAcceptsAndFlushes(t *testing.T) {
	var ran atomic.Int64
	if !DeferDurableWrite(DeferredWriteTelemetry, "test:global", func() error {
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
	if DeferDurableWrite(DeferredWriteTelemetry, "test:nil", nil) {
		t.Fatal("a nil closure was accepted")
	}
}

// TestDeferredWriteQueue_CloseReportsPendingObligations pins the
// shutdown path: an obligation the process could not land is COUNTED,
// not silently swallowed.
//
// It calls the PRODUCTION close method — the same one the MCP server's
// shutdown path calls — so the counting API that production uses is the
// one under test.
func TestDeferredWriteQueue_CloseReportsPendingObligations(t *testing.T) {
	q := &deferredWriteQueue{notify: make(chan struct{})}

	block := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	t.Cleanup(release)

	started := make(chan struct{})
	for i := 0; i < 3; i++ {
		q.deferWrite(DeferredWriteTelemetry, "test:pending", func() error {
			select {
			case <-started:
			default:
				close(started)
			}
			<-block
			return nil
		})
	}
	<-started

	stats := q.close()
	if stats.Pending < 1 {
		t.Fatalf("close reported %d pending obligations, want at least 1", stats.Pending)
	}
	if stats.DroppedClosed < 1 {
		t.Fatalf("close reported %d discarded obligation(s), want at least 1", stats.DroppedClosed)
	}
	if stats.Lost() < 1 {
		t.Fatalf("close reported %d lost obligation(s), want at least 1", stats.Lost())
	}
	// A closed queue refuses new work.
	if q.deferWrite(DeferredWriteTelemetry, "test:after-close", func() error { return nil }) {
		t.Fatal("a closed queue accepted an obligation")
	}
}
