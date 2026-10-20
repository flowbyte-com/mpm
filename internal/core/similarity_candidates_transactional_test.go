// similarity_candidates_transactional_test.go — coverage for the ATOMIC
// proposal transaction (issue #21, Phase B).
//
// The queue is a worklist a human will read. Everything this file pins is
// therefore about what the operator can BELIEVE:
//
//   - a successful transaction wrote what it says it wrote, and the
//     count is of COMMITTED rows;
//   - a refused or failed transaction wrote NOTHING, and says so;
//   - an uncertain outcome is reported as uncertain, never as success;
//   - a pair that is already queued is not queued again, even when the
//     table's own uniqueness constraint would not have caught it;
//   - a producer cannot smuggle an adjudication past the writer.
//
// The concurrency tests are the ones the schema cannot help with. The
// table's UNIQUE constraint is (memory_id_a, memory_id_b, detected_at),
// and detected_at has one-second resolution — so it is a same-second
// duplicate suppressor, NOT an unresolved-pair guarantee. Two proposals
// a second and a half apart have different timestamps and are both
// accepted by the constraint. The protection against that is the
// unresolved-pair check performed INSIDE the write transaction, and it
// is the thing these tests exist to pin.

package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ── helpers ────────────────────────────────────────────────────────

// candidateFor builds a candidate directly, so a test can drive the
// writer with inputs the discovery pass would never produce (a
// self-pair, an empty ID, a reversed pair).
func candidateFor(a, b string, sim float64) SimilarityCandidate {
	return SimilarityCandidate{
		MemoryA:    a,
		MemoryB:    b,
		Similarity: sim,
		Kind:       CandidateEqualTier,
		Evidence:   "test candidate",
	}
}

// queueRows reads the unresolved queue rows directly, oldest first.
func queueRows(t *testing.T, dm *DatabaseManager) []map[string]interface{} {
	t.Helper()
	rows, err := dm.LoadContradictionQueue(0)
	require.NoError(t, err)
	return rows
}

// unresolvedCount counts the unresolved rows in the queue.
func unresolvedCount(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	n, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	return n
}

// ── the happy paths ────────────────────────────────────────────────

// A fresh queue: every candidate in the batch is written, and the
// reported count is the number of COMMITTED rows.
func TestEnqueueSimilarityCandidatesTransactional_FreshQueue(t *testing.T) {
	dm := NewTestSharedDM(t)

	cands := []SimilarityCandidate{
		candidateFor("mem-a", "mem-b", 0.95),
		candidateFor("mem-c", "mem-d", 0.91),
	}

	res, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, CommitStateConfirmed, res.CommitState)
	require.Equal(t, "confirmed", res.CommitState.String())
	require.Equal(t, 2, res.Inserted)
	require.Equal(t, 2, res.Eligible)
	require.True(t, res.WroteTheNewRows)
	require.Zero(t, res.SkippedPending)
	require.Zero(t, res.SkippedDuplicate)
	require.False(t, res.WriteBudgetHit)

	require.Equal(t, 2, unresolvedCount(t, dm))
}

// A partially populated queue: candidates whose pair is already an
// unresolved row are skipped, and only the genuinely new pairs are
// written.
//
// The count matters as much as the rows. A writer that reported "5
// candidates proposed" when it wrote 2 would be reporting work it did
// not do, and an operator reading that on every scan of unchanged data
// would learn nothing from it.
func TestEnqueueSimilarityCandidatesTransactional_PartiallyPopulatedQueue(t *testing.T) {
	dm := NewTestSharedDM(t)

	// One pair is already queued.
	require.NoError(t, dm.EnqueueContradiction(ContradictionEvidence{
		MemoryA: "mem-a", MemoryB: "mem-b", Evidence: "pre-existing", Similarity: 0.9,
	}))
	require.Equal(t, 1, unresolvedCount(t, dm))

	cands := []SimilarityCandidate{
		candidateFor("mem-a", "mem-b", 0.95), // already pending
		candidateFor("mem-c", "mem-d", 0.91), // new
		candidateFor("mem-e", "mem-f", 0.93), // new
	}

	res, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, CommitStateConfirmed, res.CommitState)
	require.Equal(t, 2, res.Inserted,
		"only the two new pairs are written")
	require.Equal(t, 1, res.SkippedPending,
		"the already-queued pair is skipped, not re-inserted")
	require.Equal(t, 2, res.Eligible)
	require.Equal(t, 1, res.PendingUnresolved,
		"the transaction sees the pre-existing unresolved row in its own snapshot")

	require.Equal(t, 3, unresolvedCount(t, dm))
}

// An empty batch is a successful transaction that wrote nothing. That is
// a different statement from "failed", and the two must not be
// conflated.
func TestEnqueueSimilarityCandidatesTransactional_EmptyBatchSucceeds(t *testing.T) {
	dm := NewTestSharedDM(t)

	res, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), nil, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, CommitStateConfirmed, res.CommitState)
	require.Equal(t, 0, res.Inserted)
	require.False(t, res.WroteTheNewRows)
	require.Equal(t, 0, unresolvedCount(t, dm))
}

// A repeated scan over unchanged data writes nothing and reports zero.
//
// This is the property that stops a repeated operator scan from growing
// the queue without bound.
func TestEnqueueSimilarityCandidatesTransactional_RepeatedScanWritesNothing(t *testing.T) {
	dm := NewTestSharedDM(t)

	cands := []SimilarityCandidate{
		candidateFor("mem-a", "mem-b", 0.95),
		candidateFor("mem-c", "mem-d", 0.91),
	}

	first, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, 2, first.Inserted)
	afterFirst := unresolvedCount(t, dm)

	for i := 0; i < 3; i++ {
		again, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
		require.NoError(t, err)
		require.Equal(t, 0, again.Inserted,
			"a repeat scan over unchanged data must report zero new rows")
		require.Equal(t, CommitStateConfirmed, again.CommitState)
		require.Equal(t, 2, again.SkippedPending)
		require.False(t, again.WroteTheNewRows)
		require.Equal(t, afterFirst, unresolvedCount(t, dm),
			"the persistent queue must not grow on a repeat scan")
	}
}

// A pair that was RESOLVED may be proposed again, even when nothing
// about it has changed.
//
// This is preserved behaviour, and it is deliberate: a resolved pair is
// a historical record, not a permanent verdict, and the operator may want
// to look at it again. The queue's unresolved-pair rule is about what is
// OUTSTANDING, not about what has ever been seen.
//
// The re-proposal is made after the original row's second has passed,
// because the table's UNIQUE constraint includes a one-second-resolution
// detected_at. That constraint is a same-second duplicate suppressor,
// not an unresolved-pair guarantee — see the different-timestamp test
// below for the case that matters.
func TestEnqueueSimilarityCandidatesTransactional_ResolvedPairMayBeQueuedAgain(t *testing.T) {
	dm := NewTestSharedDM(t)

	cands := []SimilarityCandidate{candidateFor("mem-a", "mem-b", 0.95)}

	first, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, 1, first.Inserted)
	require.Equal(t, 1, unresolvedCount(t, dm))

	// Resolve the row: it leaves the unresolved set.
	_, err = dm.SQLDB().Exec(`UPDATE shared.contradiction_log SET resolved_at = CURRENT_TIMESTAMP
		WHERE memory_id_a = 'mem-a' AND memory_id_b = 'mem-b'`)
	require.NoError(t, err)
	require.Equal(t, 0, unresolvedCount(t, dm))

	// Cross a second boundary so the new row's detected_at cannot collide
	// with the resolved row's.
	time.Sleep(1100 * time.Millisecond)

	second, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, 1, second.Inserted,
		"a resolved pair is no longer outstanding and may be proposed again")
	require.Zero(t, second.SkippedPending)
	require.Zero(t, second.SkippedConstrained)
	require.Equal(t, 1, unresolvedCount(t, dm))
}

// A resolved pair re-proposed WITHIN THE SAME SECOND as its original
// detection is suppressed by the table's UNIQUE constraint.
//
// The pair is not pending — it is resolved — so the in-transaction
// pending check lets it through and it is eligible. The constraint is
// what stops the write, because the constraint's key includes the
// one-second-resolution detected_at and the two proposals land in the
// same second.
//
// The point of pinning this is the REPORTING, not the suppression. An
// operator seeing "eligible 1, inserted 0" with no further explanation
// has no way to tell a constraint artefact from a bug. The writer
// therefore counts the suppression separately.
func TestEnqueueSimilarityCandidatesTransactional_SameSecondRequeueIsCountedAsConstrained(t *testing.T) {
	dm := NewTestSharedDM(t)

	cands := []SimilarityCandidate{candidateFor("mem-a", "mem-b", 0.95)}

	first, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, 1, first.Inserted)

	_, err = dm.SQLDB().Exec(`UPDATE shared.contradiction_log SET resolved_at = CURRENT_TIMESTAMP
		WHERE memory_id_a = 'mem-a' AND memory_id_b = 'mem-b'`)
	require.NoError(t, err)

	// Same second: the constraint suppresses the re-queue.
	second, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, CommitStateConfirmed, second.CommitState)
	require.Equal(t, 1, second.Eligible, "the pair is eligible: it is not pending")
	require.Equal(t, 0, second.Inserted)
	require.Equal(t, 1, second.SkippedConstrained,
		"the suppression must be counted, so the report explains itself")
	require.Zero(t, second.SkippedPending)
	require.Equal(t, 0, unresolvedCount(t, dm))
}

// ── the write budget ───────────────────────────────────────────────

// An over-budget batch is refused BEFORE the first INSERT, and writes
// nothing.
//
// Inserting the first N rows and stopping would report a number that
// does not describe what the scan found, and would leave the queue in a
// state that depends on where the truncation happened to fall.
func TestEnqueueSimilarityCandidatesTransactional_WriteBudgetRefusesBeforeAnyInsert(t *testing.T) {
	dm := NewTestSharedDM(t)

	cands := make([]SimilarityCandidate, 0, 5)
	for i := 0; i < 5; i++ {
		cands = append(cands, candidateFor(
			fmt.Sprintf("mem-%02d", i*2), fmt.Sprintf("mem-%02d", i*2+1), 0.95))
	}

	res, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, 3)
	require.Error(t, err)
	var budgetErr *ErrWriteBudgetExceeded
	require.ErrorAs(t, err, &budgetErr)
	require.Equal(t, 5, budgetErr.Eligible)
	require.Equal(t, 3, budgetErr.WriteBudget)

	require.True(t, res.WriteBudgetHit)
	require.Equal(t, CommitStateRefused, res.CommitState,
		"a refused batch never opened a write transaction, so it never wrote")
	require.Equal(t, 0, res.Inserted)
	require.Equal(t, 5, res.Eligible)

	require.Equal(t, 0, unresolvedCount(t, dm),
		"the refusal must have written zero rows")
}

// The budget counts NEW insertions, not candidates found. A scan that
// found many candidates of which most were already queued is a small
// write.
func TestEnqueueSimilarityCandidatesTransactional_WriteBudgetCountsNewPairsOnly(t *testing.T) {
	dm := NewTestSharedDM(t)

	// Three pairs are already queued.
	for _, pair := range [][2]string{{"mem-a", "mem-b"}, {"mem-c", "mem-d"}, {"mem-e", "mem-f"}} {
		require.NoError(t, dm.EnqueueContradiction(ContradictionEvidence{
			MemoryA: pair[0], MemoryB: pair[1], Evidence: "pre-existing", Similarity: 0.9,
		}))
	}

	cands := []SimilarityCandidate{
		candidateFor("mem-a", "mem-b", 0.95),
		candidateFor("mem-c", "mem-d", 0.95),
		candidateFor("mem-e", "mem-f", 0.95),
		candidateFor("mem-g", "mem-h", 0.95),
		candidateFor("mem-i", "mem-j", 0.95),
	}

	res, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, 2)
	require.NoError(t, err, "two NEW pairs fit a budget of two")
	require.Equal(t, 2, res.Inserted)
	require.Equal(t, 3, res.SkippedPending)
	require.Equal(t, 2, res.Eligible)
	require.Equal(t, 5, unresolvedCount(t, dm))
}

// A budget of zero is refused rather than silently treated as
// unlimited.
//
// The refusal is reported as an INVALID BUDGET, not as an over-budget
// batch: the batch was never measured against a budget of zero, so
// reporting "0 new candidate pairs exceed the write budget of 0" would
// describe nothing at all. What the operator gets instead is the budget
// that was supplied and how many candidates were offered against it.
func TestEnqueueSimilarityCandidatesTransactional_ZeroBudgetIsRefused(t *testing.T) {
	dm := NewTestSharedDM(t)

	cands := []SimilarityCandidate{
		candidateFor("mem-a", "mem-b", 0.95),
		candidateFor("mem-c", "mem-d", 0.95),
	}

	res, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, 0)
	require.Error(t, err)
	require.True(t, res.WriteBudgetHit)
	require.Equal(t, 0, res.Inserted)
	require.Equal(t, 0, unresolvedCount(t, dm))
	require.Equal(t, CommitStateRefused, res.CommitState)

	var invalidErr *ErrInvalidWriteBudget
	require.ErrorAs(t, err, &invalidErr)
	require.Equal(t, 0, invalidErr.WriteBudget)
	require.Equal(t, 2, invalidErr.Candidates,
		"the refusal reports how many candidates were offered")
	require.NotContains(t, err.Error(), "exceed",
		"the message must not claim the batch was measured against a budget of zero")
}

// A negative budget is refused on the same grounds as a zero budget.
func TestEnqueueSimilarityCandidatesTransactional_NegativeBudgetIsRefused(t *testing.T) {
	dm := NewTestSharedDM(t)

	res, err := dm.EnqueueSimilarityCandidatesTransactional(
		context.Background(), []SimilarityCandidate{candidateFor("mem-a", "mem-b", 0.95)}, -5)
	require.Error(t, err)
	require.True(t, res.WriteBudgetHit)
	require.Equal(t, 0, res.Inserted)
	require.Equal(t, 0, unresolvedCount(t, dm))

	var invalidErr *ErrInvalidWriteBudget
	require.ErrorAs(t, err, &invalidErr)
	require.Equal(t, -5, invalidErr.WriteBudget)
	require.Equal(t, 1, invalidErr.Candidates)
}

// ── duplicates within the batch ────────────────────────────────────

// The same canonical pair may appear twice in a batch, in either order.
// It must be written once, and the duplicate must be counted as a
// duplicate rather than as a second insertion.
//
// The canonical form is LEXICOGRAPHIC, which is not the order the
// candidate layer produces: classifyPair's state_collision branch
// deliberately puts the flagged memory first. A writer that trusted the
// candidate's own order would treat (A,B) and (B,A) as two pairs.
func TestEnqueueSimilarityCandidatesTransactional_DuplicatePairInBatchWrittenOnce(t *testing.T) {
	dm := NewTestSharedDM(t)

	cands := []SimilarityCandidate{
		candidateFor("mem-a", "mem-b", 0.95),
		candidateFor("mem-b", "mem-a", 0.95), // same pair, reversed
	}

	res, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, 1, res.Inserted)
	require.Equal(t, 1, res.SkippedDuplicate)
	require.Equal(t, 1, res.Eligible)
	require.Equal(t, 1, unresolvedCount(t, dm))
}

// ── validation: malformed, reversed, forged ────────────────────────

// A self-pair is refused. A memory cannot be in conflict with itself,
// and the alternative is a row that only reveals its defect after a human
// has spent attention on it.
func TestEnqueueSimilarityCandidatesTransactional_SelfPairRefusesTheBatch(t *testing.T) {
	dm := NewTestSharedDM(t)

	cands := []SimilarityCandidate{
		candidateFor("mem-a", "mem-b", 0.95),
		candidateFor("mem-x", "mem-x", 0.99), // self-pair
	}

	res, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
	require.Error(t, err)
	var invalidErr *ErrInvalidCandidate
	require.ErrorAs(t, err, &invalidErr)
	require.Equal(t, "mem-x", invalidErr.MemoryA)
	var selfErr *ErrSelfContradiction
	require.ErrorAs(t, err, &selfErr)

	require.Equal(t, 0, res.Inserted)
	require.Equal(t, CommitStateRefused, res.CommitState,
		"a batch refused before any write is a REFUSAL, not a rollback: no attempt was made")
	require.False(t, res.WroteTheNewRows)
	require.Equal(t, 0, unresolvedCount(t, dm),
		"a batch containing an invalid candidate writes NOTHING, not the valid part")
}

// An empty memory ID is refused, on either side.
func TestEnqueueSimilarityCandidatesTransactional_EmptyMemoryIDRefusesTheBatch(t *testing.T) {
	dm := NewTestSharedDM(t)

	for _, cands := range [][]SimilarityCandidate{
		{candidateFor("", "mem-b", 0.95)},
		{candidateFor("mem-a", "", 0.95)},
	} {
		res, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
		require.Error(t, err)
		var invalidErr *ErrInvalidCandidate
		require.ErrorAs(t, err, &invalidErr)
		var missingErr *ErrMissingMemoryID
		require.ErrorAs(t, err, &missingErr)
		require.Equal(t, 0, res.Inserted)
		require.Equal(t, CommitStateRefused, res.CommitState,
			"a batch refused before any write is a REFUSAL, not a rollback")
	}
	require.Equal(t, 0, unresolvedCount(t, dm))
}

// A REVERSED pair is not malformed — it is the same pair. The writer
// canonicalises it, and the stored row is in canonical order.
func TestEnqueueSimilarityCandidatesTransactional_ReversedPairIsCanonicalised(t *testing.T) {
	dm := NewTestSharedDM(t)

	res, err := dm.EnqueueSimilarityCandidatesTransactional(
		context.Background(), []SimilarityCandidate{candidateFor("mem-z", "mem-a", 0.95)}, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, 1, res.Inserted)

	rows := queueRows(t, dm)
	require.Len(t, rows, 1)
	require.Equal(t, "mem-a", rows[0]["memory_id_a"], "the stored pair must be lexicographically ordered")
	require.Equal(t, "mem-z", rows[0]["memory_id_b"])
}

// A producer cannot smuggle an adjudication into a queue row.
//
// The `adjudicated:` segment is a record of an operator having read both
// memories and concluded the claims conflict. Creating a queue row is
// not that action, so a producer-supplied segment is unauthorised no
// matter what the producer claims. The writer strips it, so the row
// stays ineligible to penalise a memory.
func TestEnqueueSimilarityCandidatesTransactional_ForgedAdjudicationIsStripped(t *testing.T) {
	dm := NewTestSharedDM(t)

	// The candidate layer has no field for provenance — the writer
	// supplies `similarity-scan`. The forged-adjudication path is
	// therefore exercised at the writer boundary, where a producer's
	// `detected_by` reaches the row.
	wrote, err := dm.EnqueueContradictionReport(ContradictionEvidence{
		MemoryA:    "mem-a",
		MemoryB:    "mem-b",
		Evidence:   "similarity 0.95",
		Similarity: 0.95,
		DetectedBy: "similarity-scan|adjudicated:we definitely know these conflict",
	})
	require.NoError(t, err)
	require.True(t, wrote)

	rows := queueRows(t, dm)
	require.Len(t, rows, 1)
	require.NotContains(t, rows[0]["detected_by"], "adjudicated",
		"a producer-supplied adjudication must not survive into the queue")
	require.Equal(t, "similarity-scan", rows[0]["detected_by"])

	// The evidence JSON mirrors the column, and must not carry the
	// adjudication either.
	require.NotContains(t, rows[0]["evidence"], "adjudicated",
		"the evidence JSON mirrors the stored provenance and must be stripped too")

	// The same helper leaves a genuine `similarity-scan` alone.
	ev2 := ContradictionEvidence{
		MemoryA: "mem-a", MemoryB: "mem-b",
		DetectedBy: "similarity-scan|theory:some-theory-id",
	}
	require.NoError(t, validateAndNormaliseContradictionEvidence(&ev2))
	require.Equal(t, "similarity-scan|theory:some-theory-id", ev2.DetectedBy,
		"provenance other than adjudication must survive")
}

// A row written by the transactional path carries the right provenance
// and a well-formed evidence JSON, and the row is NOT eligible to
// penalise a memory.
func TestEnqueueSimilarityCandidatesTransactional_RowCarriesProvenanceAndEvidence(t *testing.T) {
	dm := NewTestSharedDM(t)

	res, err := dm.EnqueueSimilarityCandidatesTransactional(
		context.Background(), []SimilarityCandidate{candidateFor("mem-a", "mem-b", 0.95)}, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, 1, res.Inserted)

	rows := queueRows(t, dm)
	require.Len(t, rows, 1)
	require.Equal(t, similarityCandidateSource, rows[0]["detected_by"],
		"the detection provenance must say where the row came from")
	require.Contains(t, rows[0]["detected_by"], "similarity-scan")

	// The evidence column is the JSON mirror of the evidence struct.
	var ev struct {
		MemoryA    string  `json:"memory_a"`
		MemoryB    string  `json:"memory_b"`
		Evidence   string  `json:"evidence"`
		Similarity float64 `json:"similarity"`
		DetectedBy string  `json:"detected_by"`
	}
	raw, ok := rows[0]["evidence"].(string)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal([]byte(raw), &ev))
	require.Equal(t, "mem-a", ev.MemoryA)
	require.Equal(t, "mem-b", ev.MemoryB)
	require.InDelta(t, 0.95, ev.Similarity, 1e-9)
	require.Equal(t, similarityCandidateSource, ev.DetectedBy)

	// And the row is a REVIEW ITEM: the resolver must refuse to apply it.
	//
	// The verification gate runs before any state is read and before any
	// branch that can write, so the refusal is total — nothing is
	// penalised, and the row stays in the queue for an operator.
	_, resolveErr := dm.ResolveOneContradiction(rows[0], true)
	require.Error(t, resolveErr)
	var unverified *UnverifiedContradictionError
	require.ErrorAs(t, resolveErr, &unverified,
		"an unadjudicated candidate must not be eligible to penalise a memory")
	require.Equal(t, 1, unresolvedCount(t, dm),
		"the refused apply must not have resolved or removed the row")

	// A dry run is not gated — it exists so an operator can SEE the row.
	dec, dryErr := dm.ResolveOneContradiction(rows[0], false)
	require.NoError(t, dryErr)
	require.False(t, dec.Adjudicated)
	require.NotEmpty(t, dec.VerificationNote,
		"a dry run must still say why the row is not actionable")
}

// ── mid-insertion fault ────────────────────────────────────────────

// An insertion that fails part-way through rolls the WHOLE batch back.
//
// The fault is a real SQLite trigger, not a mock: the row it aborts is
// the second of two, so the first INSERT has already succeeded when the
// failure happens. If the rollback were incomplete, that first row would
// survive and the queue would hold a partial batch.
func TestEnqueueSimilarityCandidatesTransactional_MidInsertionFaultRollsBackEverything(t *testing.T) {
	dm := NewTestSharedDM(t)

	// A trigger that aborts the INSERT of the second pair.
	_, err := dm.SQLDB().Exec(`
		CREATE TRIGGER shared.trg_inject_insert_failure
		BEFORE INSERT ON contradiction_log
		WHEN NEW.memory_id_b = 'mem-d'
		BEGIN
			SELECT RAISE(ABORT, 'injected insertion failure');
		END`)
	require.NoError(t, err)

	cands := []SimilarityCandidate{
		candidateFor("mem-a", "mem-b", 0.95),
		candidateFor("mem-c", "mem-d", 0.95),
	}

	res, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
	require.Error(t, err)
	var insertionErr *ErrProposalInsertion
	require.ErrorAs(t, err, &insertionErr)
	require.Equal(t, "insert", insertionErr.Phase)

	require.Equal(t, CommitStateRolledBack, res.CommitState,
		"a failed insert whose rollback succeeded is a CONFIRMED rollback")
	require.Nil(t, res.RollbackError)
	require.Equal(t, 0, res.Inserted,
		"attempted inserts are not committed inserts")
	require.False(t, res.WroteTheNewRows)

	require.Equal(t, 0, unresolvedCount(t, dm),
		"the first INSERT must have been rolled back with the second")
}

// A count that describes an INSERT must not survive a rollback.
//
// The TransactionResult contract says the counts are of things that
// HAPPENED. The constraint suppression is counted so that "eligible 1,
// inserted 0" has an explanation — but when the transaction is rolled
// back, the INSERT that was suppressed left nothing behind, and reporting
// the suppression would describe an attempt rather than an outcome.
//
// Producing the suppressed INSERT needs the table's UNIQUE constraint to
// fire, and that constraint's key includes a one-second-resolution
// detected_at. So the pre-existing row and the proposal's INSERT have to
// land in the same wall-clock second. The attempt is repeated until the
// test can PROVE they did: if the clock crossed a second boundary, the
// first INSERT was not suppressed and the attempt would prove nothing,
// so it is discarded and retried.
func TestEnqueueSimilarityCandidatesTransactional_ConstrainedCountDoesNotSurviveARollback(t *testing.T) {
	dm := NewTestSharedDM(t)

	// The second candidate's INSERT aborts, so the transaction rolls back
	// AFTER the first candidate's INSERT has been attempted.
	_, err := dm.SQLDB().Exec(`
		CREATE TRIGGER shared.trg_inject_insert_failure
		BEFORE INSERT ON contradiction_log
		WHEN NEW.memory_id_b = 'mem-d'
		BEGIN
			SELECT RAISE(ABORT, 'injected insertion failure');
		END`)
	require.NoError(t, err)

	cands := []SimilarityCandidate{
		candidateFor("mem-a", "mem-b", 0.95),
		candidateFor("mem-c", "mem-d", 0.95),
	}

	var (
		res           TransactionResult
		proposalErr   error
		collisionSeen bool
	)
	for attempt := 0; attempt < 20 && !collisionSeen; attempt++ {
		// A RESOLVED row for the first pair, detected in the current
		// second. It is resolved, so the pair is not pending and the
		// candidate is eligible — the constraint, not the pending check,
		// is what will stop the INSERT.
		_, err = dm.SQLDB().Exec(`DELETE FROM shared.contradiction_log WHERE memory_id_a = 'mem-a'`)
		require.NoError(t, err)
		_, err = dm.SQLDB().Exec(`
			INSERT INTO shared.contradiction_log
				(memory_id_a, memory_id_b, evidence, similarity, detected_by, detected_at, resolved_at)
			VALUES ('mem-a', 'mem-b', 'pre-existing', 0.9, 'similarity-scan', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
		require.NoError(t, err)

		res, proposalErr = dm.EnqueueSimilarityCandidatesTransactional(
			context.Background(), cands, DefaultWriteBudget)
		require.Error(t, proposalErr)

		// Prove the collision happened: the setup row and the proposal's
		// INSERT must have carried the same detected_at. The clock only
		// moves forward, so if the current second still equals the setup
		// row's detected_at, the proposal's INSERT was in that second too.
		var setupAt, nowAt string
		require.NoError(t, dm.SQLDB().QueryRow(
			`SELECT detected_at FROM shared.contradiction_log WHERE memory_id_a = 'mem-a'`).Scan(&setupAt))
		require.NoError(t, dm.SQLDB().QueryRow(`SELECT datetime('now')`).Scan(&nowAt))
		collisionSeen = setupAt == nowAt
	}
	require.True(t, collisionSeen,
		"the setup row and the proposal's INSERT must land in the same second for the constraint to fire")

	var insertionErr *ErrProposalInsertion
	require.ErrorAs(t, proposalErr, &insertionErr)
	require.Equal(t, "insert", insertionErr.Phase)

	require.Equal(t, CommitStateRolledBack, res.CommitState)
	require.Equal(t, 0, res.Inserted)
	require.Zero(t, res.SkippedConstrained,
		"a count of INSERT outcomes must not survive the rollback that undid them")
	require.False(t, res.WroteTheNewRows)
	require.Equal(t, 0, unresolvedCount(t, dm))
}

// ── commit failure ─────────────────────────────────────────────────

// A COMMIT that fails is reported as uncertain, and the reported insert
// count is zero.
//
// This is the case where "we cannot prove it" is the only honest
// answer. The batch may or may not be in the queue, and the operator has
// to look.
func TestEnqueueSimilarityCandidatesTransactional_CommitFailureIsUncertain(t *testing.T) {
	dm := NewTestSharedDM(t)

	// The commit statement fails; the rollback that follows succeeds.
	setProposalTxFaultsForTest(
		func(context.Context, *sql.Conn) error { return errors.New("injected commit failure") },
		nil,
	)
	t.Cleanup(func() { setProposalTxFaultsForTest(nil, nil) })

	res, err := dm.EnqueueSimilarityCandidatesTransactional(
		context.Background(), []SimilarityCandidate{candidateFor("mem-a", "mem-b", 0.95)}, DefaultWriteBudget)
	require.Error(t, err)
	var insertionErr *ErrProposalInsertion
	require.ErrorAs(t, err, &insertionErr)
	require.Equal(t, "commit", insertionErr.Phase)

	require.Equal(t, CommitStateRolledBack, res.CommitState,
		"the rollback after the failed commit was confirmed, so nothing was written")
	require.Equal(t, 0, res.Inserted)
	require.False(t, res.WroteTheNewRows)
	require.Equal(t, 0, unresolvedCount(t, dm),
		"a confirmed rollback leaves the queue unchanged")
}

// A COMMIT that fails AND a ROLLBACK that fails leaves the outcome
// genuinely unknown, and the result says so.
//
// This is the case the contract exists for: an unconfirmed write must
// never be reported as a success, and a failed rollback must never be
// dressed up as a confirmed one.
func TestEnqueueSimilarityCandidatesTransactional_CommitAndRollbackFailureIsUncertain(t *testing.T) {
	dm := NewTestSharedDM(t)

	setProposalTxFaultsForTest(
		func(context.Context, *sql.Conn) error { return errors.New("injected commit failure") },
		func(context.Context, *sql.Conn) error { return errors.New("injected rollback failure") },
	)
	t.Cleanup(func() { setProposalTxFaultsForTest(nil, nil) })

	res, err := dm.EnqueueSimilarityCandidatesTransactional(
		context.Background(), []SimilarityCandidate{candidateFor("mem-a", "mem-b", 0.95)}, DefaultWriteBudget)
	require.Error(t, err)
	var insertionErr *ErrProposalInsertion
	require.ErrorAs(t, err, &insertionErr)
	require.Equal(t, "commit", insertionErr.Phase)

	require.Equal(t, CommitStateUncertain, res.CommitState,
		"a failed rollback leaves the outcome unknown")
	require.NotNil(t, res.RollbackError)
	require.Equal(t, 0, res.Inserted)
	require.False(t, res.WroteTheNewRows)
}

// A failed INSERT whose ROLLBACK also fails is likewise uncertain.
func TestEnqueueSimilarityCandidatesTransactional_InsertAndRollbackFailureIsUncertain(t *testing.T) {
	dm := NewTestSharedDM(t)

	_, err := dm.SQLDB().Exec(`
		CREATE TRIGGER shared.trg_inject_insert_failure_2
		BEFORE INSERT ON contradiction_log
		WHEN NEW.memory_id_b = 'mem-b'
		BEGIN
			SELECT RAISE(ABORT, 'injected insertion failure');
		END`)
	require.NoError(t, err)

	setProposalTxFaultsForTest(
		nil,
		func(context.Context, *sql.Conn) error { return errors.New("injected rollback failure") },
	)
	t.Cleanup(func() { setProposalTxFaultsForTest(nil, nil) })

	res, err := dm.EnqueueSimilarityCandidatesTransactional(
		context.Background(), []SimilarityCandidate{candidateFor("mem-a", "mem-b", 0.95)}, DefaultWriteBudget)
	require.Error(t, err)
	require.Equal(t, CommitStateUncertain, res.CommitState)
	require.NotNil(t, res.RollbackError)
	require.Equal(t, 0, res.Inserted)
}

// ── concurrency ────────────────────────────────────────────────────

// Two writers proposing the SAME pair at the same time must produce ONE
// row, not two.
//
// The uniqueness constraint cannot do this: detected_at has one-second
// resolution, so two proposals in the same second are caught by the
// constraint and two proposals a second apart are not. What catches both
// is the unresolved-pair check performed inside the write transaction,
// with BEGIN IMMEDIATE serialising the two writers against each other.
func TestEnqueueSimilarityCandidatesTransactional_ConcurrentWritersProduceOneRow(t *testing.T) {
	dm := NewTestSharedDM(t)

	const writers = 6
	var wg sync.WaitGroup
	results := make([]TransactionResult, writers)
	errs := make([]error, writers)

	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = dm.EnqueueSimilarityCandidatesTransactional(
				context.Background(),
				[]SimilarityCandidate{candidateFor("mem-a", "mem-b", 0.95)},
				DefaultWriteBudget,
			)
		}(i)
	}
	close(start)
	wg.Wait()

	inserted := 0
	for i := 0; i < writers; i++ {
		if errs[i] != nil {
			// A writer that could not take the lock is a legitimate
			// outcome; a writer that FAILED for any other reason is not.
			var busyErr *ErrDatabaseBusy
			require.ErrorAs(t, errs[i], &busyErr,
				"writer %d failed unexpectedly: %v", i, errs[i])
			continue
		}
		inserted += results[i].Inserted
	}

	require.Equal(t, 1, inserted,
		"exactly one writer may report the row as written")
	require.Equal(t, 1, unresolvedCount(t, dm),
		"exactly one queue row may exist for the pair")
}

// The same pair proposed with a DIFFERENT detected_at is still one row.
//
// This is the case the schema's UNIQUE constraint does NOT catch, and it
// is the reason the pending check is inside the transaction rather than a
// pre-flight SELECT. The pre-existing row here carries a timestamp a
// long way from "now", so the constraint would happily accept a second
// row for the same pair.
func TestEnqueueSimilarityCandidatesTransactional_DifferentTimestampDuplicateIsStillOneRow(t *testing.T) {
	dm := NewTestSharedDM(t)

	// A row whose detected_at is far from the current second.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO shared.contradiction_log
			(memory_id_a, memory_id_b, evidence, similarity, detected_by, detected_at)
		VALUES ('mem-a', 'mem-b', '{}', 0.9, 'similarity-scan', '2001-01-01 00:00:00')`)
	require.NoError(t, err)
	require.Equal(t, 1, unresolvedCount(t, dm))

	res, err := dm.EnqueueSimilarityCandidatesTransactional(
		context.Background(), []SimilarityCandidate{candidateFor("mem-a", "mem-b", 0.95)}, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, 0, res.Inserted,
		"a pair that is already outstanding is not written again, whatever the timestamp")
	require.Equal(t, 1, res.SkippedPending)
	require.Equal(t, 1, unresolvedCount(t, dm),
		"the UNIQUE constraint would have accepted a second row here; the in-transaction "+
			"pending check is what prevents it")
}

// Two proposals a second and a half apart — which is faster than an
// operator can type the command — must still produce one row.
//
// This is the real-world shape of the duplicate: the schema's constraint
// has one-second resolution, so the second proposal lands outside the
// constraint's window entirely.
func TestEnqueueSimilarityCandidatesTransactional_RepeatAcrossTimestampBoundaryIsOneRow(t *testing.T) {
	dm := NewTestSharedDM(t)

	cands := []SimilarityCandidate{candidateFor("mem-a", "mem-b", 0.95)}

	first, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, 1, first.Inserted)

	// Cross a second boundary so the two proposals cannot share a
	// detected_at value.
	time.Sleep(1100 * time.Millisecond)

	second, err := dm.EnqueueSimilarityCandidatesTransactional(context.Background(), cands, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, 0, second.Inserted)
	require.Equal(t, 1, second.SkippedPending)
	require.Equal(t, 1, unresolvedCount(t, dm))
}

// Lock contention: while another writer holds the write lock, a proposal
// is refused with ErrDatabaseBusy having written nothing.
//
// The refusal is transient by nature — another writer held the lock for
// the whole busy window — so it has its own error type and its own
// completion value.
func TestEnqueueSimilarityCandidatesTransactional_LockContentionIsReportedAsBusy(t *testing.T) {
	dm := NewTestSharedDM(t)

	// Take the write lock on a separate connection and hold it.
	lockConn, err := dm.SQLDB().Conn(context.Background())
	require.NoError(t, err)
	defer lockConn.Close()

	_, err = lockConn.ExecContext(context.Background(), "BEGIN IMMEDIATE")
	require.NoError(t, err)
	defer func() {
		_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK")
	}()

	start := time.Now()
	res, err := dm.EnqueueSimilarityCandidatesTransactional(
		context.Background(), []SimilarityCandidate{candidateFor("mem-a", "mem-b", 0.95)}, DefaultWriteBudget)
	elapsed := time.Since(start)

	require.Error(t, err)
	var busyErr *ErrDatabaseBusy
	require.ErrorAs(t, err, &busyErr)
	require.True(t, res.Busy)
	require.Equal(t, CommitStateRefused, res.CommitState)
	require.Equal(t, 0, res.Inserted)

	t.Logf("MEASURED lock contention refused after %s (busy window)", elapsed)
}

// ── the shared attachment ──────────────────────────────────────────

// The `shared` schema must be attached to the TRANSACTION'S OWN
// connection.
//
// SQLite's ATTACH is per-connection, not per-database. The production
// attach path issues ATTACH through the pooled *sql.DB, so the attachment
// lands on whichever pooled connection served that call — and a
// transaction on a DIFFERENT pooled connection would find no `shared`
// schema at all.
//
// This test cannot force a specific connection, but it does assert the
// property that matters: the transaction's connection has the shared
// schema, and the write lands in the shared database rather than in the
// local one.
func TestEnqueueSimilarityCandidatesTransactional_SharedIsAttachedOnTheTransactionConnection(t *testing.T) {
	dm := NewTestSharedDM(t)

	res, err := dm.EnqueueSimilarityCandidatesTransactional(
		context.Background(), []SimilarityCandidate{candidateFor("mem-a", "mem-b", 0.95)}, DefaultWriteBudget)
	require.NoError(t, err)
	require.Equal(t, 1, res.Inserted)

	// The row is in the SHARED database, not the local one.
	var sharedCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM shared.contradiction_log`).Scan(&sharedCount))
	require.Equal(t, 1, sharedCount)
}

// ── the read path agrees with the write path ────────────────────────

// The queue COUNT must not depend on which pooled connection serves it.
//
// SQLite attaches a database PER CONNECTION. MPM attaches the shared
// schema on exactly ONE connection at startup (attachShared in db.go),
// and the pool is deliberately not limited to one connection — see the
// note in db.go about why SetMaxOpenConns(1) deadlocks. A count that ran
// as a pooled query would therefore fail with "no such table:
// shared.contradiction_log" whenever the pool handed it a connection that
// was not the attached one.
//
// The count pins a connection and verifies the attachment, so it works in
// the state this test creates: every idle connection checked out, and the
// count forced onto a brand-new connection that carries no attachment.
func TestCountContradictionQueue_SucceedsOnAFreshPooledConnection(t *testing.T) {
	dm := NewTestSharedDM(t)

	require.NoError(t, dm.EnqueueContradiction(ContradictionEvidence{
		MemoryA: "mem-a", MemoryB: "mem-b", Evidence: "pre-existing", Similarity: 0.9,
	}))

	// Hold every idle connection the pool has. The count then has to open
	// a new one, and a new connection has no `shared` attachment.
	var held []*sql.Conn
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	for dm.db.Stats().Idle > 0 {
		c, err := dm.db.Conn(context.Background())
		require.NoError(t, err)
		held = append(held, c)
	}

	n, err := dm.CountContradictionQueue()
	require.NoError(t, err, "the count must attach the shared schema it needs")
	require.Equal(t, 1, n)
}

// The queue LOAD must survive the same state for the same reason.
//
// The load is what the review command reads. A load that failed whenever
// the pool served an unattached connection would abort the operator's
// review with an error that said the table does not exist — when it
// does, and the transaction that writes to it works fine.
func TestLoadContradictionQueue_SucceedsOnAFreshPooledConnection(t *testing.T) {
	dm := NewTestSharedDM(t)

	require.NoError(t, dm.EnqueueContradiction(ContradictionEvidence{
		MemoryA: "mem-a", MemoryB: "mem-b", Evidence: "pre-existing", Similarity: 0.9,
	}))

	var held []*sql.Conn
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	for dm.db.Stats().Idle > 0 {
		c, err := dm.db.Conn(context.Background())
		require.NoError(t, err)
		held = append(held, c)
	}

	rows, err := dm.LoadContradictionQueue(100)
	require.NoError(t, err, "the load must attach the shared schema it needs")
	require.Len(t, rows, 1)
	require.Equal(t, "mem-a", rows[0]["memory_id_a"])
}

// An install with no shared database has no queue. That is a
// configuration fact, and it must be reported as such — a typed error
// the caller can recognise — rather than as an anonymous driver failure.
func TestCountContradictionQueue_LocalOnlyInstallReportsTheQueueIsUnavailable(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)

	_, err := dm.CountContradictionQueue()
	require.Error(t, err)
	var unavailable *ErrSharedUnavailable
	require.ErrorAs(t, err, &unavailable,
		"a missing shared database must be a typed failure, not an anonymous one")
	require.Contains(t, strings.ToLower(err.Error()), "shared")
}

// The insert-phase message must not claim a rollback.
//
// At the point the error is constructed the rollback has not run yet —
// the rollback is attempted by the deferred cleanup, which runs after the
// error is built. A message that said "was rolled back" would assert
// something that was not yet true, and in the case that matters most it
// asserts it wrongly: the ROLLBACK itself failed, and the outcome is
// unknown.
//
// The message points at the commit state instead, which is the
// authoritative statement of what happened.
func TestErrProposalInsertion_InsertFailureDoesNotClaimARollback(t *testing.T) {
	err := &ErrProposalInsertion{Phase: "insert", Underlying: errors.New("disk I/O error")}

	msg := err.Error()
	require.Contains(t, msg, "proposal insertion failed")
	require.NotContains(t, msg, "was rolled back")
	require.Contains(t, msg, "commit state")
}

// The commit-phase message must not assert a specific outcome either.
//
// It used to say "the outcome is uncertain", which was true of the
// COMMIT phase but not of the batch: a ROLLBACK that succeeded after a
// failed COMMIT proves that the batch did not land. The error is built
// before that rollback runs, so it cannot know — and it must not guess.
func TestErrProposalInsertion_CommitFailureDoesNotAssertAnOutcome(t *testing.T) {
	err := &ErrProposalInsertion{Phase: "commit", Underlying: errors.New("disk I/O error")}

	msg := err.Error()
	require.Contains(t, msg, "proposal commit failed")
	require.NotContains(t, msg, "the outcome is uncertain")
	require.Contains(t, msg, "commit state")
}

// ── the original path still works ──────────────────────────────────

// The original per-row writer and the transactional writer must agree on
// validation. Both go through the same helper, so a candidate that the
// transactional path refuses is one the original path refuses too.
func TestValidateAndNormaliseContradictionEvidence_IsSharedByBothWriters(t *testing.T) {
	dm := NewTestSharedDM(t)

	for _, ev := range []ContradictionEvidence{
		{MemoryA: "", MemoryB: "mem-b"},
		{MemoryA: "mem-a", MemoryB: ""},
		{MemoryA: "mem-a", MemoryB: "mem-a"},
	} {
		_, err := dm.EnqueueContradictionReport(ev)
		require.Error(t, err, "the original writer must reject %+v", ev)
	}
	require.Equal(t, 0, unresolvedCount(t, dm))
}
