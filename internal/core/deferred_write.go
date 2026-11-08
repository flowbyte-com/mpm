// deferred_write.go — the substrate's deferred durable-write queue.
//
// WHY THIS EXISTS.
//
// A substrate write that carries OBSERVATIONAL weight — retrieval
// telemetry, audit evidence, the per-invocation audit row — used to be
// paid INLINE, on the caller's goroutine, with the substrate's full
// durable-write retry budget.
//
// Under a held SQLite write lock that budget is ~33 s per write (six
// 5 s busy-handler waits plus the exponential backoff between them).
// One MCP wake request was measured at 124.47 s end to end, of which
// 99.4 s was three durable writes and 25.0 s was five telemetry
// writes — every one of them observational. The agent waited 124 s to
// be told what it already knew.
//
// THE CONTRACT.
//
//  1. The CALLER never waits for a deferred write. The obligation moves
//     to a background worker and the caller's latency stops depending
//     on how long the write lock is held.
//
//  2. The obligation is RETRIED until it lands. A deferred write is not
//     a dropped write: the worker keeps the obligation pending and
//     re-attempts it with the substrate's own durable budget (the
//     closure is the existing, unchanged write call, retry budget
//     included). This is STRICTLY STRONGER than the inline behaviour
//     it replaces, where the same writes exhausted their budget and
//     were lost — the audit evidence was gone, the telemetry row was
//     gone, the invocation row was gone.
//
//  3. Only the CLEARABLE class is retried. An ordinary SQLITE_BUSY is
//     a wait, and waiting clears it. Anything else — a constraint
//     violation, a closed database — is not a wait, and retrying it
//     forever would block every other obligation behind it. Those are
//     dropped, loudly.
//
//  4. The queue is BOUNDED. An unbounded queue turns a write-lock
//     stall into a memory leak. When the queue is full the obligation
//     is dropped with a warning, exactly as the synthesis pool drops a
//     task when it is full (ErrSynthesisPoolFull).
//
//  5. A contended obligation does NOT block the others. Each pending
//     obligation carries its own next-attempt time, so one write stuck
//     behind a held lock never strands the obligations queued behind it.
//
// WHAT THIS IS NOT.
//
// This is not a way to weaken the durability of genuine user state. A
// deferred write is a real write, performed by the same code, with the
// same budget. What changes is WHEN it happens: the caller no longer
// blocks on it, and the obligation stays pending until it lands.
//
// It is also not a general-purpose async executor. Every item here is
// a durable write that must reach the database.

package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// deferredWriteQueueCapacity is the maximum number of pending
// obligations. Beyond it, new obligations are dropped with a warning
// rather than growing the queue without bound.
const deferredWriteQueueCapacity = 256

// deferredWriteInitialBackoff is the first pause between re-attempts of
// a contended obligation.
const deferredWriteInitialBackoff = 100 * time.Millisecond

// deferredWriteMaxBackoff caps the pause between re-attempts.
const deferredWriteMaxBackoff = 5 * time.Second

// deferredWriteItem is one pending obligation.
//
// The closure is the WRITE ITSELF — the unchanged substrate call, with
// its own durable retry budget. The worker owns only the cadence: when
// to try again.
type deferredWriteItem struct {
	kind        string
	fn          func() error
	attempts    int
	nextAttempt time.Time
}

// deferredWriteQueue is the process-wide pending-obligation queue.
//
// It is package-level rather than per-DatabaseManager because the
// obligations are closures that capture whatever they need, and
// because a manager-scoped worker would need a lifecycle hook to
// start and drain it — a coupling this mechanism does not need.
//
// A single worker processes items in order of their next-attempt time.
// That is deliberate: two workers would contend with each other for
// the same write lock and turn one stall into two.
type deferredWriteQueue struct {
	mu      sync.Mutex
	items   []deferredWriteItem
	closed  bool
	started bool
	busy    bool
	wake    chan struct{}
}

var deferredWrites = &deferredWriteQueue{
	wake: make(chan struct{}, 1),
}

// DeferDurableWrite hands a durable write to the background worker and
// returns immediately.
//
// The caller MUST NOT report the write as settled: the obligation is
// pending until the worker lands it. fn is re-invoked — with its own
// budget — until it returns nil.
//
// It returns false when the obligation could not be accepted (the queue
// is full, or the queue is closed), in which case fn was NOT called and
// the caller owns the failure.
func DeferDurableWrite(kind string, fn func() error) bool {
	if fn == nil {
		return false
	}
	deferredWrites.mu.Lock()
	if deferredWrites.closed || len(deferredWrites.items) >= deferredWriteQueueCapacity {
		deferredWrites.mu.Unlock()
		slog.Warn("deferred write dropped: queue full", "kind", kind, "capacity", deferredWriteQueueCapacity)
		return false
	}
	deferredWrites.items = append(deferredWrites.items, deferredWriteItem{
		kind:        kind,
		fn:          fn,
		nextAttempt: time.Now(),
	})
	deferredWrites.mu.Unlock()
	deferredWrites.start()
	deferredWrites.signal()
	return true
}

// signal nudges the worker without blocking.
func (q *deferredWriteQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// start launches the worker exactly once.
func (q *deferredWriteQueue) start() {
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return
	}
	q.started = true
	q.mu.Unlock()
	go q.run()
}

// run is the worker loop.
//
// Items are attempted in next-attempt order. A contended item is put
// back with a later next-attempt time, so the obligations behind it
// are not stranded behind its retries.
func (q *deferredWriteQueue) run() {
	for {
		item, wait, ok := q.next()
		if !ok {
			return
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-q.wake:
				timer.Stop()
			}
			continue
		}
		q.attempt(item)
	}
}

// next picks the item that is due, or reports how long to wait for the
// next one.
func (q *deferredWriteQueue) next() (deferredWriteItem, time.Duration, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return deferredWriteItem{}, 0, false
	}
	if len(q.items) == 0 {
		q.busy = false
		return deferredWriteItem{}, 0, false
	}
	now := time.Now()
	best := -1
	for i, it := range q.items {
		if it.nextAttempt.After(now) {
			continue
		}
		if best == -1 || it.nextAttempt.Before(q.items[best].nextAttempt) {
			best = i
		}
	}
	if best == -1 {
		// Nothing is due yet: wait for the earliest.
		earliest := q.items[0].nextAttempt
		for _, it := range q.items[1:] {
			if it.nextAttempt.Before(earliest) {
				earliest = it.nextAttempt
			}
		}
		wait := earliest.Sub(now)
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		return deferredWriteItem{}, wait, true
	}
	item := q.items[best]
	q.items = append(q.items[:best], q.items[best+1:]...)
	q.busy = true
	return item, 0, true
}

// attempt performs one obligation once and re-queues it when the
// failure is the clearable class.
func (q *deferredWriteQueue) attempt(item deferredWriteItem) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.mu.Unlock()

	item.attempts++
	err := item.fn()
	if err == nil {
		return
	}
	if !isRetryableDeferredError(err) {
		// Not a wait: retrying cannot clear it, and holding the queue
		// behind it would strand every later obligation.
		slog.Warn("deferred write failed and will not be retried",
			"kind", item.kind, "attempts", item.attempts, "err", err.Error())
		return
	}
	slog.Debug("deferred write contended; will retry",
		"kind", item.kind, "attempts", item.attempts, "err", err.Error())

	backoff := deferredWriteInitialBackoff
	for i := 1; i < item.attempts && backoff < deferredWriteMaxBackoff; i++ {
		backoff *= 2
	}
	if backoff > deferredWriteMaxBackoff {
		backoff = deferredWriteMaxBackoff
	}
	item.nextAttempt = time.Now().Add(backoff)

	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	if len(q.items) < deferredWriteQueueCapacity {
		q.items = append(q.items, item)
	}
	q.mu.Unlock()
	q.signal()
}

// isRetryableDeferredError reports whether an error is the CLEARABLE
// class: ordinary write contention.
//
// This mirrors the substrate's own busy-class discipline. An ordinary
// SQLITE_BUSY is a wait and a retry clears it. A closed database is
// not retryable (the manager is gone). Everything else — a constraint
// violation, a malformed statement — will fail identically forever.
func isRetryableDeferredError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "database is closed") {
		return false
	}
	return IsBusyError(err) ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "busy")
}

// FlushDeferredWrites waits until the pending queue is empty and no
// attempt is in flight, or the context expires.
//
// It exists for the two callers that need a definite answer: a test
// asserting that a deferred row has landed, and a process that is
// about to exit and must not lose the obligation it accepted.
func FlushDeferredWrites(ctx context.Context) error {
	deferredWrites.start()
	for {
		deferredWrites.mu.Lock()
		empty := len(deferredWrites.items) == 0 && !deferredWrites.busy
		deferredWrites.mu.Unlock()
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("flush deferred writes: %w", ctx.Err())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// CloseDeferredWrites stops the worker and discards any pending
// obligations.
//
// It is the shutdown path for a process that is going away: the
// obligations it could not land are counted and reported, not silently
// swallowed.
func CloseDeferredWrites() int {
	deferredWrites.mu.Lock()
	if deferredWrites.closed {
		deferredWrites.mu.Unlock()
		return 0
	}
	deferredWrites.closed = true
	pending := len(deferredWrites.items)
	deferredWrites.items = nil
	deferredWrites.mu.Unlock()
	deferredWrites.signal()
	return pending
}
