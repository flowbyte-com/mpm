// ops_propose_candidates_test.go — coverage for the one write path that may
// add to the persistent contradiction queue.
//
// runProposeCandidates was the only reachable writer of
// shared.contradiction_log with NO test exercising it. That matters more than
// the usual "untested function" concern, because this is the path an operator
// trusts to be the ONLY thing that grows the queue. The guarantee item 1
// rests on — "retrieval never adds a row, only an explicit operator command
// does" — is only as good as this function's behaviour under repetition, and
// repetition is exactly what no test was checking.
//
// What these pin:
//   - a scan finds above-threshold pairs and writes exactly one row per pair
//   - re-running over unchanged data writes NOTHING (the idempotency claim)
//   - shared-side memories are scanned, not just local ones
//   - a shared-only candidate is not proposed as a contradiction of itself
//   - the reported count is rows ACTUALLY inserted, not candidates found

package main

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

// seedProposeMemory inserts a live memory with an explicit vector. Explicit,
// rather than going through the store, so the test controls the exact geometry
// and can place memories above or below the candidate threshold on purpose.
func seedProposeMemory(t *testing.T, dm *mpminternal.DatabaseManager, schema, id, content string, emb []float32) {
	t.Helper()
	raw, err := json.Marshal(emb)
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(`
		INSERT INTO `+schema+`memories
			(id, collection, content, metadata, weight, created_at,
			 embedding, embedding_source, embedding_dimension, embedding_model)
		VALUES (?, 'memories', ?, '{}', 1, strftime('%s','now'), ?, 'provider', ?, ?)`,
		id, content, string(raw), len(emb),
		mpminternal.CurrentEmbeddingFingerprint().Model)
	require.NoError(t, err)
}

// The baseline: two near-identical claims are found, enqueued once, and the
// returned count is the number of writes performed.
func TestRunProposeCandidates_FindsAndEnqueues(t *testing.T) {
	dm := mpminternal.NewTestSharedDM(t)

	// Identical vectors => cosine 1.0, well above the candidate threshold.
	seedProposeMemory(t, dm, "", "p-a", "the deploy pipeline runs on tuesday", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "p-b", "the deploy pipeline runs on tuesdays", unitVector(1, 0, 0))

	before, err := dm.CountContradictionQueue()
	require.NoError(t, err)

	inserted, err := runProposeCandidates(dm, 0, false)
	require.NoError(t, err)
	require.Equal(t, 1, inserted)

	after, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, before+1, after,
		"exactly one queue row per pair; the count is rows written, not pairs seen")
}

// THE repeat-safety property. EnqueueContradictionReport is idempotent on the
// unresolved pair, but the underlying UNIQUE constraint includes a
// one-second-resolution detected_at, so idempotency is enforced by a pre-check
// rather than by the schema. A test that runs the command once proves nothing
// about that pre-check.
func TestRunProposeCandidates_RepeatWritesNothing(t *testing.T) {
	dm := mpminternal.NewTestSharedDM(t)

	seedProposeMemory(t, dm, "", "p-a", "the deploy pipeline runs on tuesday", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "p-b", "the deploy pipeline runs on tuesdays", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "p-c", "the deploy pipeline runs on wed", unitVector(1, 0, 0))

	first, err := runProposeCandidates(dm, 0, false)
	require.NoError(t, err)
	require.Positive(t, first)

	queueAfterFirst, err := dm.CountContradictionQueue()
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		again, err := runProposeCandidates(dm, 0, false)
		require.NoError(t, err)
		require.Equal(t, 0, again,
			"a repeat scan over unchanged data must report zero new rows, "+
				"not re-announce last run's candidates as new")

		now, err := dm.CountContradictionQueue()
		require.NoError(t, err)
		require.Equal(t, queueAfterFirst, now,
			"the persistent queue must not grow on a repeat scan")
	}
}

// A duplicate-heavy corpus is the realistic shape of a scan: many near-identical
// memories, all pairing with each other. Every pair is a distinct queue row and
// every pair is distinct, so the queue legitimately grows large in ONE run —
// and must then be exactly stable.
func TestRunProposeCandidates_DuplicateHeavyClusterIsStableAfterFirstRun(t *testing.T) {
	dm := mpminternal.NewTestSharedDM(t)

	for i := 0; i < 6; i++ {
		seedProposeMemory(t, dm, "", "dup-"+itoa(i),
			"the workspace must use pnpm for every install", unitVector(1, 0, 0))
	}

	first, err := runProposeCandidates(dm, 0, false)
	require.NoError(t, err)
	// Six memories produce C(6,2) = 15 above-threshold pairs.
	require.Equal(t, 15, first, "a fully duplicate cluster yields every pair once")

	afterFirst, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, 15, afterFirst)

	second, err := runProposeCandidates(dm, 0, false)
	require.NoError(t, err)
	require.Equal(t, 0, second)

	afterSecond, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, afterFirst, afterSecond,
		"15 candidates re-found must add 0 rows the second time")
}

// The federated scan. hybridSearchScope returns local ∪ shared to every
// reader, so a scan that read local only would leave the operator comparing a
// subset of what they can see — while enqueueing into the shared queue.
func TestRunProposeCandidates_ScansSharedSideToo(t *testing.T) {
	dm := mpminternal.NewTestSharedDM(t)

	// A pair living entirely on the shared side. Orthogonal to the local pair, so
	// the two cross-side cosines are 0 and only the two within-side pairs
	// clear the threshold. Without that separation every one of the C(4,2)=6
	// pairs would fire and the test would pass for the wrong reason.
	seedProposeMemory(t, dm, "shared.", "s-a", "shared claim one", unitVector(0, 1, 0))
	seedProposeMemory(t, dm, "shared.", "s-b", "shared claim two", unitVector(0, 1, 0))

	// A pair living entirely on the local side.
	seedProposeMemory(t, dm, "", "l-a", "local claim one", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "l-b", "local claim two", unitVector(1, 0, 0))

	inserted, err := runProposeCandidates(dm, 0, false)
	require.NoError(t, err)
	require.Equal(t, 2, inserted,
		"both the local and the shared pair must be found; a local-only scan "+
			"finds 1 and silently under-reports")
}

// A memory promoted to shared can remain in local. The two copies are the same
// claim with the same ID. FindSimilarityCandidates only skips index pairs
// (j > i), so without an ID dedupe the copies pair with each other and a
// memory is proposed as a contradiction of itself.
func TestRunProposeCandidates_SelfPairAcrossSchemasIsNotProposed(t *testing.T) {
	dm := mpminternal.NewTestSharedDM(t)

	seedProposeMemory(t, dm, "", "promoted", "the same claim on both sides", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "shared.", "promoted", "the same claim on both sides", unitVector(1, 0, 0))

	inserted, err := runProposeCandidates(dm, 0, false)
	require.NoError(t, err)
	require.Equal(t, 0, inserted,
		"one memory present in local and shared is ONE claim, not a pair")

	queue, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, 0, queue, "no row may be written for a memory against itself")
}

// An unattached shared DB is a normal configuration, not an error. The scan
// degrades to local-only rather than failing — matching hybridSearchScope.
func TestRunProposeCandidates_LocalOnlyInstallStillScans(t *testing.T) {
	dm := mpminternal.NewTestLocalOnlyDM(t)

	seedProposeMemory(t, dm, "", "l-a", "local claim one", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "l-b", "local claim two", unitVector(1, 0, 0))

	// The SCAN degrades: no shared schema is a normal configuration, not an
	// error, and the operator still gets a scan of what actually exists.
	entries, err := dm.LoadSimilarityScanEntriesFederated(0)
	require.NoError(t, err,
		"a missing shared schema must degrade, not abort the scan")
	require.Len(t, entries, 2)

	// The ENQUEUE cannot succeed: the queue is shared.contradiction_log, and
	// there is no shared schema to hold it. That must be reported as a
	// failure, not swallowed — an operator who asked for candidates and got
	// silence would reasonably conclude the queue was updated.
	inserted, err := runProposeCandidates(dm, 0, false)
	require.Error(t, err,
		"a local-only install cannot enqueue; the error must surface")
	require.Contains(t, strings.ToLower(err.Error()), "shared",
		"the error must name the reason, not just fail")
	require.Equal(t, 0, inserted)
}

// The limit is a per-source cost bound applied per side, exactly as
// hybridSearchScope applies its limit per side.
func TestRunProposeCandidates_LimitIsAppliedPerSide(t *testing.T) {
	dm := mpminternal.NewTestSharedDM(t)

	for i := 0; i < 4; i++ {
		seedProposeMemory(t, dm, "", "l-"+itoa(i),
			"local claim "+itoa(i), unitVector(1, 0, 0))
	}
	for i := 0; i < 4; i++ {
		seedProposeMemory(t, dm, "shared.", "s-"+itoa(i),
			"shared claim "+itoa(i), unitVector(1, 0, 0))
	}

	// limit=3 per side => 3 local + 3 shared = 6 entries.
	entries, err := dm.LoadSimilarityScanEntriesFederated(3)
	require.NoError(t, err)
	require.Len(t, entries, 6,
		"limit applies per side, so the union is up to 2x the limit")

	// No cap => everything on both sides.
	all, err := dm.LoadSimilarityScanEntriesFederated(0)
	require.NoError(t, err)
	require.Len(t, all, 8)
}

// The scan must refuse an injectable schema prefix rather than interpolating
// it into the statement — it is a WRITE path.
func TestLoadSimilarityScanEntries_RejectsUnsafeSchemaPrefix(t *testing.T) {
	dm := mpminternal.NewTestSharedDM(t)

	for _, prefix := range []string{
		"shared; DROP TABLE memories --",
		"shared.'",
		"shared..memories",
		"1shared.",
		"shared.memories.",
	} {
		_, err := dm.LoadSimilarityScanEntries(0, prefix)
		require.Error(t, err, "prefix %q must be rejected", prefix)
		require.Contains(t, err.Error(), "invalid schema prefix")
	}

	// The legitimate prefixes must still pass. "Shared." is accepted because a
	// mixed-case SQL identifier is legal — the pattern permits it by design,
	// and a test asserting otherwise would be arguing with the existing guard
	// rather than with the scan.
	for _, prefix := range []string{"", "shared.", "Shared."} {
		_, err := dm.LoadSimilarityScanEntries(0, prefix)
		require.NoError(t, err, "prefix %q is legitimate", prefix)
	}
}

// The JSON form is what a script consumes, so its counts must distinguish
// "candidates found" from "rows written". Conflating them is how an operator
// concludes a queue was populated when nothing was inserted.
func TestRunProposeCandidates_JSONDistinguishesFoundFromWritten(t *testing.T) {
	dm := mpminternal.NewTestSharedDM(t)

	seedProposeMemory(t, dm, "", "j-a", "claim one", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "j-b", "claim two", unitVector(1, 0, 0))

	var inserted int
	out := captureStdout(t, func() {
		var err error
		inserted, err = runProposeCandidates(dm, 0, true)
		require.NoError(t, err)
	})
	require.Equal(t, 1, inserted)

	require.Contains(t, out, `"scanned_memories":2`)
	require.Contains(t, out, `"candidates_found":1`)
	require.Contains(t, out, `"candidates_proposed":1`)

	// Second run: the candidate is found again but nothing is written.
	out = captureStdout(t, func() {
		var err error
		inserted, err = runProposeCandidates(dm, 0, true)
		require.NoError(t, err)
	})
	require.Equal(t, 0, inserted)
	require.Contains(t, out, `"candidates_found":1`)
	require.Contains(t, out, `"candidates_proposed":0`,
		"found and written must be separate numbers, even on a repeat run")
}

func unitVector(a, b, c float32) []float32 {
	n := float32(1)
	for _, x := range []float32{a, b, c} {
		n += x * x
	}
	return []float32{a / n, b / n, c / n}
}
