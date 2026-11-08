// retrieval_metadata_deferred.go — the non-blocking form of the
// retrieval telemetry write.
//
// WHY THIS EXISTS.
//
// The retrieval telemetry write is OBSERVATIONAL BY CONTRACT. Its own
// file header says so:
//
//	Errors are returned for caller inspection but the typical contract
//	is fire-and-forget: the user-facing retrieval must not fail because
//	telemetry write failed. Callers should log and continue.
//
// But "fire-and-forget" was never true of the COST. The write was
// issued on the caller's goroutine, on the shared pool, where every
// connection carries a 5 s busy-handler timeout. Under a held write
// lock each telemetry write therefore cost ~5 s of the agent's wall
// clock before it could even fail.
//
// Measured on one MCP wake request: FIVE telemetry writes — recent
// memories, recent milestones, theories, decisions, lessons — cost
// 25.04 s of the request's 124.47 s, and produced no rows.
//
// Two facts make the fix possible without a second connection pool
// (which the substrate forbids — there is exactly one DatabaseManager
// pool, and every write goes through it):
//
//  1. A context deadline CANNOT cut SQLite's busy-handler wait. The
//     driver's interrupt path does not abort a busy sleep; a bounded
//     context still waits the full busy_timeout. (Measured: 50 ms and
//     250 ms deadlines both returned after ~5.0 s.)
//
//  2. The busy-handler wait is therefore unavoidable for any write
//     attempted ON THE CALLER'S GOROUTINE. The only way to keep it off
//     the agent's critical path is to move the write off that
//     goroutine.
//
// So the write moves to the deferred queue. That is not a downgrade:
// the write still happens, it is simply retried until it lands instead
// of being lost on the first contended attempt. The bare pool write
// this replaces had NO retry budget at all — under contention it lost
// its artifact outright, which is the defect class issue #178 exists
// to close.
//
// The accounting rule is unchanged: retrieval_metadata counts a node
// once per retrieval, and the deferred write is still one UPSERT per
// retrieval. Deferring changes WHEN the counter moves, not how much it
// moves.

package internal

// RecordRetrievalDeferred records that a node was retrieved, without
// blocking the caller.
//
// It returns true when the obligation was accepted by the deferred
// queue. A false return means the write was NOT queued and the caller
// owns that failure.
//
// The obligation is pending until the worker lands it.
func (dm *DatabaseManager) RecordRetrievalDeferred(nodeID, nodeType string) bool {
	if nodeID == "" || nodeType == "" {
		// Same contract as RecordRetrieval: an empty id or type is a
		// caller error, not a telemetry event. Report it the same way.
		return false
	}
	return DeferDurableWrite("retrieval_metadata", func() error {
		return dm.RecordRetrieval(nodeID, nodeType)
	})
}
