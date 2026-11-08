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
//  2. An ACCEPTED obligation is RETRIED until it lands. A deferred
//     write is not a dropped write: the worker keeps the obligation
//     pending and re-attempts it with the substrate's own durable
//     budget (the closure is the existing, unchanged write call, retry
//     budget included). This is STRICTLY STRONGER than the inline
//     behaviour it replaces, where the same writes exhausted their
//     budget and were lost — the audit evidence was gone, the telemetry
//     row was gone, the invocation row was gone.
//
//  3. An accepted obligation is NEVER dropped by the queue's own
//     capacity management. The capacity accounting counts the
//     obligation BEING WRITTEN as well as the ones waiting, so a new
//     producer can never take the slot an in-flight obligation needs
//     for its retry. This is the property that was missing, and its
//     absence broke promise 2 exactly when the queue was saturated.
//
//  4. Only the CLEARABLE class is retried. An ordinary SQLITE_BUSY is
//     a wait, and waiting clears it. Anything else — a constraint
//     violation, a closed database — is not a wait, and retrying it
//     forever would block every other obligation behind it. Those are
//     dropped, loudly, and COUNTED.
//
//  5. The queue is BOUNDED. An unbounded queue turns a write-lock
//     stall into a memory leak. The bound covers pending AND in-flight
//     obligations together, so it is a bound on the memory the queue
//     is responsible for.
//
//  6. EVIDENCE and TELEMETRY are different classes, and they are
//     admitted differently. Telemetry is best-effort BY CONTRACT — the
//     retrieval layer's own header says so. Audit evidence is not:
//     losing it loses the record of what happened. So a slice of the
//     queue's capacity is reserved for evidence, and telemetry may not
//     consume it. Telemetry that cannot be admitted is refused,
//     loudly; that is the contract it was always written under.
//
//  7. A FAILED obligation does not monopolise the queue. A contended
//     item is put back with a later next-attempt time, so the
//     obligations behind it get their turn instead of being stranded
//     behind its retries. An attempt that is IN FLIGHT does hold the
//     queue — but every deferred write is bounded by the substrate's
//     own busy-timeout and retry budget (~5 s for a bare write, ~33 s
//     for a budgeted one), so no obligation can wedge the queue.
//
//  8. The worker LIVES as long as the queue does. It idles when there
//     is nothing to do and it is woken by the next obligation. It exits
//     only when the queue is closed.
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
//
// IT IS NOT CRASH-DURABLE.
//
// The queue is MEMORY-ONLY. Nothing is persisted, so a SIGKILL — or any
// process death that does not run the shutdown path — loses every
// pending obligation, and there is no way to recover them afterwards.
//
// That is acceptable for telemetry, which is losable by contract. It is
// NOT acceptable for audit evidence, and this file does not pretend
// otherwise: see docs/SPEC.md §10 and
// docs/dev/issue-178-observational-write-latency-2026-11-08.md §7 for
// what the evidence class does and does not guarantee, and for the
// separate mechanism that closing the crash-durability gap would
// require.

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

// DeferredWriteClass says what a deferred write is WORTH.
//
// The class decides two things: whether the queue's capacity is
// reserved for it, and whether its loss is a contract violation or an
// accepted risk.
type DeferredWriteClass int

const (
	// DeferredWriteTelemetry is BEST-EFFORT telemetry.
	//
	// The retrieval counters are telemetry by their own contract: the
	// user-facing retrieval must not fail because a telemetry write
	// failed. Losing one is a recorded, counted event, not a broken
	// promise.
	DeferredWriteTelemetry DeferredWriteClass = iota

	// DeferredWriteEvidence is ESSENTIAL audit evidence.
	//
	// These rows record what happened — a failure, an invocation. Their
	// loss is a loss of evidence, and the queue's capacity accounting
	// reserves room for them so that telemetry cannot crowd them out.
	DeferredWriteEvidence
)

// String renders the class for a log line.
func (c DeferredWriteClass) String() string {
	if c == DeferredWriteEvidence {
		return "evidence"
	}
	return "telemetry"
}

// deferredWriteQueueCapacity is the maximum number of obligations the
// queue is responsible for at once — the ones WAITING plus the one
// BEING WRITTEN.
//
// Counting the in-flight obligation is what makes promise 3 hold: the
// slot an in-flight obligation needs for its retry is reserved while
// it is in flight, so a new producer cannot take it.
const deferredWriteQueueCapacity = 256

// deferredWriteTelemetryCapacity is the maximum number of TELEMETRY
// obligations the queue will hold.
//
// The difference between it and deferredWriteQueueCapacity is the
// EVIDENCE RESERVE: telemetry may not consume it, so a burst of
// telemetry cannot crowd out the audit evidence the queue exists to
// protect. 192 + 64 = 256.
//
// The reserve is a promise about ADMISSION, not a hard ceiling on the
// telemetry count: an obligation that was already accepted keeps its
// slot through its retries. The ceiling on the telemetry count is
// therefore deferredWriteTelemetryCapacity, and the ceiling on the
// total is deferredWriteQueueCapacity.
const deferredWriteTelemetryCapacity = 192

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
	class       DeferredWriteClass
	fn          func() error
	attempts    int
	nextAttempt time.Time
}

// DeferredWriteStats is the queue's accounting.
//
// It distinguishes the four outcomes that matter, which are NOT the
// same thing:
//
//   - Landed          the obligation was written successfully.
//   - Accepted        the queue took responsibility for the obligation.
//   - Rejected*       the obligation was NEVER accepted (the queue was
//     full or closed). The caller was told, at the moment of refusal.
//   - Dropped*        the obligation WAS accepted and then lost. This
//     is the only outcome that breaks the queue's promise, and it is
//     always counted and reported.
type DeferredWriteStats struct {
	// Accepted is the number of obligations the queue took responsibility
	// for.
	Accepted int64
	// Landed is the number of obligations that were written successfully.
	Landed int64
	// RejectedFull is the number of obligations refused at enqueue time
	// because the queue was full. They were never accepted.
	RejectedFull int64
	// RejectedClosed is the number of obligations refused at enqueue
	// time because the queue was closed. They were never accepted.
	RejectedClosed int64
	// DroppedTerminal is the number of ACCEPTED obligations that failed
	// with a failure that retrying cannot clear.
	DroppedTerminal int64
	// DroppedClosed is the number of ACCEPTED obligations that were
	// discarded because the queue was closed before they landed.
	DroppedClosed int64
	// LostEvidence is the subset of the losses that were EVIDENCE
	// obligations — audit records. Losing one breaks a promise.
	LostEvidence int64
	// LostTelemetry is the subset of the losses that were TELEMETRY
	// obligations. Losing one is allowed by the telemetry contract.
	LostTelemetry int64
	// Pending is the number of obligations waiting when the snapshot was
	// taken.
	Pending int
	// InFlight is the number of obligations being written when the
	// snapshot was taken.
	InFlight int
}

// Lost returns the number of obligations that were ACCEPTED and then
// lost. It is the number that breaks the queue's promise.
func (s DeferredWriteStats) Lost() int64 {
	return s.DroppedTerminal + s.DroppedClosed
}

// deferredWriteQueue is the process-wide pending-obligation queue.
//
// It is package-level rather than per-DatabaseManager because the
// obligations are closures that capture whatever they need, and
// because a manager-scoped worker would need a lifecycle hook to
// start and drain it — a coupling this mechanism does not need.
//
// A single worker processes items in order of their next-attempt time.
// That is deliberate: two workers would contend with each other for the
// same write lock and turn one stall into two.
type deferredWriteQueue struct {
	mu      sync.Mutex
	items   []deferredWriteItem
	closed  bool
	started bool
	// inflight counts the obligations currently being written. The
	// queue is not drained until it is zero AND no item is pending.
	inflight int
	// inflightClass is the class of the in-flight obligation, so the
	// telemetry accounting can count it.
	inflightClass DeferredWriteClass
	// notify is closed and replaced whenever the queue changes, so a
	// wake-up that arrives while the worker is busy is never lost.
	notify chan struct{}
	// counts is the queue's accounting. It is cumulative for the life of
	// the queue.
	counts DeferredWriteStats
	// unreportedEvidenceLoss counts the EVIDENCE losses that no flush has
	// reported yet. A flush reports them once and resets the counter, so
	// a caller that flushes learns about an evidence loss exactly once,
	// and an evidence loss is never silently absorbed by a later flush.
	//
	// TELEMETRY losses are deliberately NOT counted here: telemetry is
	// losable by its own contract, so a lost counter must not fail a
	// caller that is waiting for audit evidence. They are counted in the
	// cumulative statistics and logged loudly.
	unreportedEvidenceLoss int64
}

var deferredWrites = &deferredWriteQueue{
	notify: make(chan struct{}),
}

// DeferDurableWrite hands a durable write to the background worker and
// returns immediately.
//
// It returns false when the obligation could NOT be accepted (the queue
// is full, or the queue is closed), in which case fn was NOT called and
// the caller owns the failure. A caller that ignores the false return
// is claiming a write was queued when it was not.
//
// The caller MUST NOT report the write as settled: the obligation is
// pending until the worker lands it. fn is re-invoked — with its own
// budget — until it returns nil.
//
// The class says what the obligation is worth. It is not decoration:
// it decides whether the queue's capacity is reserved for it.
func DeferDurableWrite(class DeferredWriteClass, kind string, fn func() error) bool {
	if fn == nil {
		return false
	}
	return deferredWrites.deferWrite(class, kind, fn)
}

// deferWrite is DeferDurableWrite for one specific queue.
//
// The package-level function is a thin wrapper over this one, so a test
// can exercise a private queue instead of the process-wide one — the
// process-wide queue is shared by every test in the package, and a
// test that closes it would break the tests that follow.
func (q *deferredWriteQueue) deferWrite(class DeferredWriteClass, kind string, fn func() error) bool {
	if fn == nil {
		return false
	}
	q.mu.Lock()
	if q.closed {
		q.counts.RejectedClosed++
		q.mu.Unlock()
		slog.Warn("deferred write rejected: the queue is closed",
			"kind", kind, "class", class.String())
		return false
	}
	// THE CAPACITY CHECK COUNTS THE IN-FLIGHT OBLIGATION.
	//
	// An in-flight obligation may need to be re-queued when its attempt
	// fails with the clearable class, and that re-queue MUST succeed:
	// the obligation was accepted, and promise 2 says an accepted
	// obligation is retried until it lands.
	//
	// If a NEW producer could take the in-flight slot, that promise
	// would break exactly when the queue is saturated — which is
	// precisely when it matters. So the in-flight slot is RESERVED: a
	// new obligation is refused (loudly, and counted) while it is
	// held.
	//
	// The bound is therefore on the obligations the queue is
	// RESPONSIBLE FOR — waiting plus in-flight — which is also the
	// bound on the memory it holds.
	if q.totalObligationsLocked() >= deferredWriteQueueCapacity {
		q.counts.RejectedFull++
		q.mu.Unlock()
		slog.Warn("deferred write rejected: the queue is full",
			"kind", kind, "class", class.String(),
			"capacity", deferredWriteQueueCapacity,
			"obligation", "not accepted, not written, not retried")
		return false
	}
	// TELEMETRY MAY NOT CONSUME THE EVIDENCE RESERVE.
	//
	// Telemetry is losable by contract; audit evidence is not. A burst
	// of telemetry — five rows per wake request — must not be able to
	// consume the whole queue and starve the evidence the queue exists
	// to protect.
	if class == DeferredWriteTelemetry &&
		q.telemetryObligationsLocked() >= deferredWriteTelemetryCapacity {
		q.counts.RejectedFull++
		q.mu.Unlock()
		slog.Warn("deferred write rejected: the telemetry capacity is reached",
			"kind", kind, "class", class.String(),
			"telemetry_capacity", deferredWriteTelemetryCapacity,
			"evidence_reserve", deferredWriteQueueCapacity-deferredWriteTelemetryCapacity,
			"obligation", "not accepted, not written, not retried")
		return false
	}
	q.items = append(q.items, deferredWriteItem{
		kind:        kind,
		class:       class,
		fn:          fn,
		nextAttempt: time.Now(),
	})
	q.counts.Accepted++
	q.signalLocked()
	q.mu.Unlock()
	q.start()
	return true
}

// totalObligationsLocked returns the number of obligations the queue is
// responsible for: the ones waiting plus the one being written.
//
// The caller must hold q.mu.
func (q *deferredWriteQueue) totalObligationsLocked() int {
	return len(q.items) + q.inflight
}

// telemetryObligationsLocked returns the number of TELEMETRY
// obligations the queue is responsible for.
//
// The caller must hold q.mu.
func (q *deferredWriteQueue) telemetryObligationsLocked() int {
	n := 0
	for _, it := range q.items {
		if it.class == DeferredWriteTelemetry {
			n++
		}
	}
	if q.inflight > 0 && q.inflightClass == DeferredWriteTelemetry {
		n++
	}
	return n
}

// signalLocked wakes the worker. The caller must hold q.mu.
//
// The notify channel is closed and REPLACED rather than sent to. A
// buffered send would drop the wake-up when the worker was busy, and
// the worker would then sleep through an obligation it was supposed to
// be waiting for.
func (q *deferredWriteQueue) signalLocked() {
	close(q.notify)
	q.notify = make(chan struct{})
}

// start launches the worker exactly once.
//
// It is safe to call after the worker has idled: the worker only exits
// when the queue is closed, and a closed queue refuses new work.
func (q *deferredWriteQueue) start() {
	q.mu.Lock()
	if q.started || q.closed {
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
//
// The loop exits ONLY when the queue is closed. An empty queue is an
// IDLE state, not a terminal one: the worker waits for the next
// obligation. (An earlier revision returned from run() on an empty
// queue, which killed the worker permanently the first time the queue
// drained — every later obligation was then queued but never written,
// and any flush waited for it until its deadline.)
func (q *deferredWriteQueue) run() {
	for {
		item, wait, notify, ok := q.next()
		if !ok {
			return
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-notify:
				timer.Stop()
			}
			continue
		}
		q.attempt(item)
	}
}

// next picks the item that is due, or reports how long to wait for the
// next one.
//
// The returned channel is the queue's current notify channel: it is
// closed when the queue changes, so the caller can wait on it without
// missing a wake-up that raced with the wait.
func (q *deferredWriteQueue) next() (deferredWriteItem, time.Duration, <-chan struct{}, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return deferredWriteItem{}, 0, nil, false
	}
	if len(q.items) == 0 {
		// Nothing pending: idle until the next obligation arrives.
		return deferredWriteItem{}, time.Hour, q.notify, true
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
		return deferredWriteItem{}, wait, q.notify, true
	}
	item := q.items[best]
	q.items = append(q.items[:best], q.items[best+1:]...)
	q.inflight++
	q.inflightClass = item.class
	return item, 0, q.notify, true
}

// attempt performs one obligation once and re-queues it when the
// failure is the clearable class.
//
// The diagnostics here are the queue's REPORT on its own behaviour, so
// each one must describe what actually happened. In particular: a
// "will retry" message is emitted ONLY when a retry has actually been
// scheduled, and a loss is reported as a loss, with a count.
func (q *deferredWriteQueue) attempt(item deferredWriteItem) {
	item.attempts++
	err := item.fn()

	var (
		requeued bool
		dropped  bool
	)
	q.mu.Lock()
	q.inflight--
	q.inflightClass = DeferredWriteTelemetry
	switch {
	case err == nil:
		q.counts.Landed++
	case isRetryableDeferredError(err) && q.closed:
		// The queue is closed, so there is no worker left to retry it.
		// This is a TERMINAL loss, not a scheduled retry, and it is
		// counted as one.
		q.counts.DroppedClosed++
		q.recordLossLocked(item.class)
		dropped = true
	case isRetryableDeferredError(err):
		// THE RE-QUEUE ALWAYS SUCCEEDS.
		//
		// The capacity accounting counted this obligation as in-flight
		// while it was being attempted, so the slot it needs is
		// already reserved: moving it from in-flight back to pending
		// leaves the total unchanged, and the total is bounded by
		// deferredWriteQueueCapacity.
		//
		// An earlier revision re-queued behind a `len(items) < capacity`
		// guard and DROPPED the obligation when the guard failed — the
		// guard could not fire under the old accounting, but under the
		// corrected accounting it is exactly the guard that would have
		// broken promise 3. The re-queue is unconditional here, and
		// the invariant above is what makes that safe.
		item.nextAttempt = time.Now().Add(deferredWriteBackoff(item.attempts))
		q.items = append(q.items, item)
		q.signalLocked()
		requeued = true
	default:
		// Not a wait: retrying cannot clear it, and holding the queue
		// behind it would strand every later obligation.
		q.counts.DroppedTerminal++
		q.recordLossLocked(item.class)
		dropped = true
	}
	q.mu.Unlock()

	switch {
	case err == nil:
		return
	case requeued:
		slog.Debug("deferred write contended; a retry is scheduled",
			"kind", item.kind, "class", item.class.String(),
			"attempts", item.attempts, "err", err.Error())
	case dropped && q.isClosed():
		slog.Warn("deferred write dropped: the queue was closed before it could be retried",
			"kind", item.kind, "class", item.class.String(),
			"attempts", item.attempts, "err", err.Error())
	default:
		slog.Warn("deferred write failed and will not be retried",
			"kind", item.kind, "class", item.class.String(),
			"attempts", item.attempts, "err", err.Error())
	}
}

// deferredWriteBackoff returns the pause before the next attempt of a
// contended obligation.
func deferredWriteBackoff(attempts int) time.Duration {
	backoff := deferredWriteInitialBackoff
	for i := 1; i < attempts && backoff < deferredWriteMaxBackoff; i++ {
		backoff *= 2
	}
	if backoff > deferredWriteMaxBackoff {
		backoff = deferredWriteMaxBackoff
	}
	return backoff
}

// recordLossLocked records a terminal loss against its class.
//
// The caller must hold q.mu.
//
// An EVIDENCE loss is one the queue's promise does not cover: the audit
// record of what happened is gone. It is counted, logged, and reported
// to the next flush. A TELEMETRY loss is allowed by the telemetry
// contract; it is counted and logged, and it does not fail a caller.
func (q *deferredWriteQueue) recordLossLocked(class DeferredWriteClass) {
	if class == DeferredWriteEvidence {
		q.counts.LostEvidence++
		q.unreportedEvidenceLoss++
		return
	}
	q.counts.LostTelemetry++
}

// isClosed reports whether the queue has been closed.
func (q *deferredWriteQueue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
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
//
// A nil error means every accepted EVIDENCE obligation has landed. If an
// accepted audit record was lost — a terminal failure, or the queue
// closing underneath it — the flush reports that instead of a bare
// success, so a caller that treats a nil flush as "the evidence is
// there" is not told a falsehood.
//
// A loss is reported exactly once: the flush resets the unreported-loss
// counter after reporting it, so a later flush does not repeat a loss
// the caller has already been told about, and a loss is never silently
// absorbed.
//
// A TELEMETRY loss does NOT fail the flush. Telemetry is losable by its
// own contract, and a caller waiting for audit evidence must not be
// failed by a lost reuse counter. It is counted in the queue's
// statistics and logged loudly.
//
// An obligation REFUSED at enqueue time is not reported here: it was
// never accepted, it was reported to the caller at the moment of
// refusal, and it is counted in the queue's stats.
func FlushDeferredWrites(ctx context.Context) error {
	return deferredWrites.flush(ctx)
}

// flush waits until this queue is empty and no attempt is in flight, or
// the context expires.
func (q *deferredWriteQueue) flush(ctx context.Context) error {
	q.start()
	for {
		q.mu.Lock()
		drained := len(q.items) == 0 && q.inflight == 0
		q.mu.Unlock()
		if drained {
			// The drain is complete, so every obligation accepted
			// before it has either landed or been terminally dropped.
			q.mu.Lock()
			lost := q.unreportedEvidenceLoss
			q.unreportedEvidenceLoss = 0
			q.mu.Unlock()
			if lost > 0 {
				return fmt.Errorf("flush deferred writes: %d accepted EVIDENCE obligation(s) were lost and did not land", lost)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("flush deferred writes: %w", ctx.Err())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// CloseDeferredWrites stops the worker and reports what it could not
// land.
//
// It is the shutdown path for a process that is going away: the
// obligations it could not land are COUNTED and reported, not silently
// swallowed.
//
// The returned stats are the queue's accounting. Pending is the number
// of obligations that were discarded by this call; Lost is the number
// of accepted obligations that have been lost over the queue's life.
func CloseDeferredWrites() DeferredWriteStats {
	return deferredWrites.close()
}

// close stops this queue's worker and reports the obligations it could
// not land.
func (q *deferredWriteQueue) close() DeferredWriteStats {
	q.mu.Lock()
	if q.closed {
		stats := q.statsLocked()
		q.mu.Unlock()
		return stats
	}
	q.closed = true
	pending := len(q.items)
	var pendingEvidence, pendingTelemetry int
	for _, it := range q.items {
		if it.class == DeferredWriteEvidence {
			pendingEvidence++
		} else {
			pendingTelemetry++
		}
	}
	q.items = nil
	q.counts.DroppedClosed += int64(pending)
	q.counts.LostEvidence += int64(pendingEvidence)
	q.counts.LostTelemetry += int64(pendingTelemetry)
	q.unreportedEvidenceLoss += int64(pendingEvidence)
	q.signalLocked()
	stats := q.counts
	// The obligations this call discarded. `stats.Pending` would be zero
	// — the queue was just emptied — so the discarded count is reported
	// explicitly: it is the number the operator needs.
	stats.Pending = pending
	stats.InFlight = q.inflight
	q.mu.Unlock()
	if pending > 0 {
		slog.Warn("deferred write obligations were discarded at close",
			"discarded", pending,
			"accepted", stats.Accepted,
			"landed", stats.Landed,
			"lost_total", stats.Lost())
	}
	return stats
}

// DeferredWriteStats returns the process-wide queue's accounting.
//
// It is the operator's window into what the queue did: how much it
// accepted, how much landed, how much it refused, and how much it
// lost. The last number is the one that breaks the queue's promise.
func DeferredWriteStatsSnapshot() DeferredWriteStats {
	return deferredWrites.stats()
}

// stats returns the queue's accounting.
func (q *deferredWriteQueue) stats() DeferredWriteStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.statsLocked()
}

// statsLocked returns the queue's accounting.
//
// The caller must hold q.mu.
func (q *deferredWriteQueue) statsLocked() DeferredWriteStats {
	stats := q.counts
	stats.Pending = len(q.items)
	stats.InFlight = q.inflight
	return stats
}
