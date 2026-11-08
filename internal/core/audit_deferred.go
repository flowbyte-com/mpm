// audit_deferred.go — the non-blocking form of the system audit log.
//
// WHY THIS EXISTS.
//
// An audit write is OBSERVATIONAL: it records what happened, it does
// not make it happen. But it used to be paid INLINE, with the
// substrate's full durable-write retry budget.
//
// That is catastrophic in the one place where an audit write is most
// likely: recording a FAILURE. A write that failed because the
// database was locked was followed by an audit write that failed for
// the same reason — and the audit write spent a SECOND full durable
// budget (~33 s under a held write lock) trying to record the first
// failure, and then lost the evidence too.
//
// Measured on one MCP wake request under a held write lock: the
// failure-path audit cost 33.14 s and produced NO row.
//
// LogAuditDeferred removes the caller-side cost while making the
// evidence STRONGER, not weaker: the obligation is queued and retried
// until it lands, where the inline write exhausted its budget and
// dropped the row on the floor.
//
// The stack trace is captured on the CALLING goroutine. Capturing it in
// the worker would record the worker's stack, which is the same for
// every deferred audit and therefore worthless as evidence.

package internal

import (
	"runtime/debug"
)

// LogAuditDeferred records an audit event without blocking the caller.
//
// It returns true when the obligation was accepted by the deferred
// queue, and false when it was not (the queue is full or closed). A
// false return means the evidence was NOT queued: the caller owns that
// failure and should report it however it reports its own failures.
//
// The obligation is pending until the worker lands it. A caller must
// not report the audit as recorded before the queue accepts it.
func (dm *DatabaseManager) LogAuditDeferred(level AuditLevel, component, message, stack string, ctx AuditContext) bool {
	if dm == nil || dm.db == nil {
		return false
	}
	// Capture the stack HERE, on the caller's goroutine. The worker
	// would capture its own, which is the same for every deferred
	// audit and therefore useless as evidence.
	if stack == "" {
		stack = string(debug.Stack())
	}
	return DeferDurableWrite("audit:"+component, func() error {
		return dm.LogAuditErr(level, component, message, stack, ctx)
	})
}
