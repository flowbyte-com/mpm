// compact_deferral_counter.go — per-process monotonic counter used to
// keep deferral batch ids unique within a millisecond.
//
// A batch id is an operator-facing grouping key. Two deferrals in the
// same millisecond would otherwise share an id, and "group rows by
// compaction_deferred_batch" would merge two unrelated refusals into
// one apparent group — the exact ambiguity the stable-batch-id property
// exists to prevent.

package internal

import "sync/atomic"

// deferralCounter is process-local and never persisted. Ids only need
// to be distinct within a process, because a batch id is minted once
// and written to the rows of that one deferral; a restart resets it
// harmlessly since the millisecond prefix also moves.
var deferralCounter atomic.Int64

func atomicDeferralCounter() int {
	return int(deferralCounter.Add(1) % 1000)
}
