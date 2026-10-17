// contradiction_queue_idempotency_test.go — the queue must not grow on a
// repeat of work it already holds.
//
// The defect these pin: the uniqueness constraint is
// UNIQUE(memory_id_a, memory_id_b, detected_at), and detected_at is
// CURRENT_TIMESTAMP with one-second resolution. OR IGNORE therefore suppressed
// a duplicate only when the repeat landed inside the same second. Two
// enqueues of an identical pair 1.1s apart both reported wrote=true and left
// two rows.
//
// It matters because the returned count is shown to an operator. A scan over
// unchanged data re-announcing every candidate as new teaches the reader
// nothing and, worse, makes the queue grow without bound.

package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func queueRowCount(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM shared.contradiction_log`).Scan(&n))
	return n
}

func pairEvidence(a, b string) ContradictionEvidence {
	return ContradictionEvidence{
		MemoryA:    a,
		MemoryB:    b,
		Evidence:   "unverified similarity candidate (cosine=0.99) between " + a + " and " + b,
		Similarity: 0.99,
		DetectedBy: similarityCandidateSource,
	}
}

// The headline case: two identical enqueues separated by more than the
// timestamp's resolution must produce one row, and the second must report
// that it wrote nothing.
func TestEnqueueContradictionReport_RepeatAcrossSecondsIsNotANewWrite(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	ev := pairEvidence("exp-1", "exp-2")

	wrote, err := dm.EnqueueContradictionReport(ev)
	require.NoError(t, err)
	require.True(t, wrote, "the first enqueue of an unseen pair must report a write")
	require.Equal(t, 1, queueRowCount(t, dm))

	// CURRENT_TIMESTAMP resolves to whole seconds. Sleeping past that boundary
	// is the whole point: without it the two calls could share a timestamp and
	// OR IGNORE alone would have hidden the bug.
	time.Sleep(1100 * time.Millisecond)

	wrote, err = dm.EnqueueContradictionReport(ev)
	require.NoError(t, err)
	require.False(t, wrote,
		"re-enqueueing an already-queued pair must report no write; reporting one "+
			"makes a repeated scan announce unchanged work as new")
	require.Equal(t, 1, queueRowCount(t, dm),
		"the queue must hold one row per pair, not one per invocation")
}

// Argument order is normalised at the call site, so (A,B) and (B,A) are the
// same pair and must not produce two rows.
func TestEnqueueContradictionReport_PairOrderIsNormalised(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	first := pairEvidence("exp-a", "exp-b")
	_, err := dm.EnqueueContradictionReport(first)
	require.NoError(t, err)
	require.Equal(t, 1, queueRowCount(t, dm))

	time.Sleep(1100 * time.Millisecond)

	swapped := pairEvidence("exp-b", "exp-a")
	wrote, err := dm.EnqueueContradictionReport(swapped)
	require.NoError(t, err)
	require.False(t, wrote, "(A,B) and (B,A) are one pair")
	require.Equal(t, 1, queueRowCount(t, dm))
}

// Evidence drifts between scans — the cosine in the string moves — but the
// pair is still the same review item. Deduplicating on pair+evidence would
// let a drifting score re-queue an already-queued pair.
func TestEnqueueContradictionReport_DriftedEvidenceIsStillTheSamePair(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	_, err := dm.EnqueueContradictionReport(pairEvidence("exp-1", "exp-2"))
	require.NoError(t, err)
	require.Equal(t, 1, queueRowCount(t, dm))

	time.Sleep(1100 * time.Millisecond)

	drifted := pairEvidence("exp-1", "exp-2")
	drifted.Evidence = "unverified similarity candidate (cosine=0.87) between exp-1 and exp-2"
	drifted.Similarity = 0.87

	wrote, err := dm.EnqueueContradictionReport(drifted)
	require.NoError(t, err)
	require.False(t, wrote,
		"a changed score for an already-queued pair is not a new review item")
	require.Equal(t, 1, queueRowCount(t, dm))
}

// Once a pair is resolved it may legitimately be queued again. Deduping on
// the pair alone, with no regard to resolution, would make that impossible.
func TestEnqueueContradictionReport_ResolvedPairCanBeQueuedAgain(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	_, err := dm.EnqueueContradictionReport(pairEvidence("exp-1", "exp-2"))
	require.NoError(t, err)
	require.Equal(t, 1, queueRowCount(t, dm))

	_, err = dm.SQLDB().Exec(
		`UPDATE shared.contradiction_log SET resolved_at = CURRENT_TIMESTAMP
		 WHERE memory_id_a = 'exp-1' AND memory_id_b = 'exp-2'`)
	require.NoError(t, err)

	time.Sleep(1100 * time.Millisecond)

	wrote, err := dm.EnqueueContradictionReport(pairEvidence("exp-1", "exp-2"))
	require.NoError(t, err)
	require.True(t, wrote,
		"a resolved pair has left the review worklist; re-flagging it is a new item")
	require.Equal(t, 2, queueRowCount(t, dm))
}

// Distinct pairs are not deduplicated against each other.
func TestEnqueueContradictionReport_DifferentPairsBothPersist(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	_, err := dm.EnqueueContradictionReport(pairEvidence("exp-1", "exp-2"))
	require.NoError(t, err)

	time.Sleep(1100 * time.Millisecond)

	wrote, err := dm.EnqueueContradictionReport(pairEvidence("exp-2", "exp-3"))
	require.NoError(t, err)
	require.True(t, wrote, "a different pair is a different review item")
	require.Equal(t, 2, queueRowCount(t, dm))
}

// The end-to-end shape: run the same scan twice over unchanged data. The
// second run must report zero inserted and leave the queue untouched.
func TestEnqueueSimilarityCandidates_SecondRunOverUnchangedDataInsertsNothing(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	cands := []SimilarityCandidate{
		{MemoryA: "exp-1", MemoryB: "exp-2", Similarity: 0.99, Kind: CandidateEqualTier},
		{MemoryA: "exp-3", MemoryB: "exp-4", Similarity: 0.97, Kind: CandidateStateCollision},
	}

	inserted, err := dm.EnqueueSimilarityCandidates(cands)
	require.NoError(t, err)
	require.Equal(t, 2, inserted)
	require.Equal(t, 2, queueRowCount(t, dm))

	time.Sleep(1100 * time.Millisecond)

	inserted, err = dm.EnqueueSimilarityCandidates(cands)
	require.NoError(t, err)
	require.Equal(t, 0, inserted,
		"a repeat scan over unchanged data must report 0 inserted, not 2")
	require.Equal(t, 2, queueRowCount(t, dm),
		"the queue must not grow when the same findings are re-proposed")
}
