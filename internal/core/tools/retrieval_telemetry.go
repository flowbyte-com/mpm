// retrieval_telemetry.go — the wake path's non-blocking retrieval
// telemetry.
//
// WHY THIS FILE EXISTS.
//
// Every node the wake context surfaces is counted as "retrieved" in
// `retrieval_metadata`. That telemetry is OBSERVATIONAL BY CONTRACT —
// the retrieval layer's own header says the write is fire-and-forget
// and "must not fail because telemetry write failed".
//
// But it was paid INLINE, on the agent's goroutine, on the shared pool
// where every connection carries a 5 s busy-handler timeout. Under a
// held write lock each telemetry write cost ~5 s of the agent's wall
// clock before it could even fail.
//
// Measured on one MCP wake request under a held write lock: the FIVE
// telemetry writes (one per surfaced memory) cost 25.04 s of the
// request's 124.47 s, and produced no rows.
//
// The fix moves the write off the caller's goroutine. It is not a
// downgrade: the deferred write is retried until it lands, where the
// bare pool write it replaces had NO retry budget and lost its artifact
// on the first contended attempt.
//
// The accounting is unchanged: one UPSERT per surfaced node, so
// `reuse_count` still moves by exactly one per retrieval.
//
// The other RecordRetrieval call sites in this file are NOT changed.
// They sit on paths that are not the wake path, and their callers
// still read the row back synchronously.

package tools

import (
	"log/slog"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// recordRetrievalAsync queues the retrieval-telemetry write for a node
// the wake context surfaced, without blocking the caller.
//
// The obligation stays pending until the worker lands it.
//
// The refusal is HANDLED, not discarded (issue #178, F2 remediation). A
// false return means the write was NOT queued — the queue was full or
// closed — and it will not happen at all.
//
// Telemetry is BEST-EFFORT BY CONTRACT: the retrieval layer's own
// header says the user-facing retrieval must not fail because a
// telemetry write failed. So a refusal is logged and the retrieval
// carries on. It is not reported as a success, and it is not papered
// over with a synchronous write — under the saturation that causes the
// refusal, a synchronous write would cost the agent a full busy-handler
// wait and would fail for the same reason.
//
// The caller is not told, because a caller of a fire-and-forget
// telemetry write has nothing to do with the outcome; the refusal is
// counted by the queue and reported in its statistics.
func recordRetrievalAsync(dm mpminternal.CoreDB, nodeID, nodeType string) {
	if dm == nil || nodeID == "" {
		return
	}
	accepted := dm.RecordRetrievalDeferred(nodeID, nodeType)
	if !accepted {
		slog.Debug("the retrieval telemetry write was refused by the deferred queue",
			"node_id", nodeID,
			"node_type", nodeType,
			"class", "telemetry",
			"consequence", "the retrieval is not counted; telemetry is best-effort by contract")
	}
}
