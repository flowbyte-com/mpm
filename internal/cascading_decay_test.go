package internal

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cascading_decay_test.go — Integration test for the cascading-decay gap.
//
// Architecture fact (verified by reading internal/evidence_store.go and
// internal/confidence.go): confidence is derived from evidence rows that
// point TO an artifact (artifact_id, artifact_type), not FROM it. A
// theory's supporting evidence references the theory itself, not the
// memories the theory's reasoning depends on.
//
// Implication: when a memory a theory depends on is deleted, the
// theory's confidence is unchanged. The theory continues to claim
// whatever it claimed, with the same confidence, even though one of
// its foundations has rotted.
//
// This is a documented architectural gap — NOT a bug. The substrate
// doesn't track forward dependencies (theory → memory). The
// reconciliation tool that would detect this state is a future
// deliverable. Until then, agents must surface and resolve
// "orphaned" theories through explicit recall + review, not via
// automatic confidence adjustment (which would violate the
// "explicit mutation" invariant the architecture enforces).
//
// The test below pins current behavior as a regression net. When
// the reconciliation tool lands, the assertion at the end will
// need to change — and that's the point.

// TestCascadingDecay_TheoryConfidenceUnchangedWhenUnderlyingMemoryDeleted
// is the forcing function for the cascading-decay gap.
//
// Sequence:
//  1. Insert a memory M with specific content.
//  2. Create a theory T whose hypothesis explicitly depends on M.
//  3. Add positive evidence to T (strength 0.8) — T's confidence rises
//     above the initial 0.5 because of the evidence.
//  4. Verify T's confidence reflects the evidence (sanity check that
//     AddEvidence + RecomputeConfidence actually do their job).
//  5. Soft-delete M (DeleteMemory sets deleted_at).
//  6. Recompute T's confidence from scratch.
//  7. Assert: T's confidence is unchanged.
//
// Step 7 is the gap. The proper fix would either (a) detect that T
// depends on M via content/metadata parsing, or (b) implement a
// periodic reconciliation that flags T as a candidate for review.
// Neither exists today, so the test pins the silent drift.
func TestCascadingDecay_TheoryConfidenceUnchangedWhenUnderlyingMemoryDeleted(t *testing.T) {
	dm := newTestDM(t)

	// Step 1: insert a memory M with deterministic content.
	memID, err := dm.SaveMemory(
		"memories",
		"Cascading decay test: M is a foundational fact about shell quoting.",
		"", // session_id
		[]string{"test", "cascading-decay"},
		nil, nil,
		false, // is_long_term
		5,     // weight
	)
	require.NoError(t, err)
	require.NotEmpty(t, memID)

	// Step 2: create a theory T whose hypothesis explicitly cites M's id.
	// The validation_criteria text mentions memID — that's the only place
	// the dependency is recorded in the current schema.
	hypothesis := fmt.Sprintf("T depends on memory %s being true", memID)
	res, err := dm.ProposeTheory(hypothesis, "if M is shredded, T should be re-evaluated", []string{"test", "cascading-decay"})
	require.NoError(t, err)
	theoryID, _ := res["id"].(string)
	require.NotEmpty(t, theoryID)

	// Verify T's initial confidence = 0.5 (per InitialConfidence("theory")).
	var initialConf float64
	require.NoError(t, dm.QueryRowTracked(
		`SELECT confidence FROM memories WHERE id = ? AND collection = 'theories'`, theoryID,
	).Scan(&initialConf))
	assert.InDelta(t, 0.5, initialConf, 1e-9, "theory initial confidence should be 0.5")

	// Step 3: add positive evidence to T (strength 0.8).
	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:         theoryID,
		ArtifactType:       "theory",
		Type:               "observation",
		SourceGroup:        "test-cascading-decay",
		Strength:           0.8,
		IndependenceFactor: 1.0,
		CreatedBy:          "808",
		CreatedAt:          time.Now(),
		Notes:              "Positive evidence from a confirming observation.",
	}))

	// Step 4: recompute T's confidence and verify it rose above 0.5.
	require.NoError(t, RecomputeConfidence(dm, theoryID, "theory", RecomputeReasonManual))
	var confAfterEvidence float64
	require.NoError(t, dm.QueryRowTracked(
		`SELECT confidence FROM memories WHERE id = ?`, theoryID,
	).Scan(&confAfterEvidence))
	assert.Greater(t, confAfterEvidence, 0.5,
		"positive evidence (0.8) should raise T's confidence above initial 0.5 (got %v)", confAfterEvidence)

	// Step 5: soft-delete M (the memory T depends on).
	// DeleteMemory is the production entry point — sets deleted_at + metadata.is_deleted.
	store := &MemoryStore{DM: dm, DB: &SQLiteConnection{DB: dm.SQLDB()}}
	require.NoError(t, store.DeleteMemory(memID, "memories"))

	// Sanity check: M is now soft-deleted (active queries exclude it).
	var memDeletedAt *string
	require.NoError(t, dm.QueryRowTracked(
		`SELECT deleted_at FROM memories WHERE id = ?`, memID,
	).Scan(&memDeletedAt))
	require.NotNil(t, memDeletedAt, "memory M should be soft-deleted after DeleteMemory")

	// Step 6: recompute T's confidence from scratch.
	require.NoError(t, RecomputeConfidence(dm, theoryID, "theory", RecomputeReasonManual))

	// Step 7: THE GAP. T's confidence is unchanged because the evidence
	// model has no concept of "this theory depends on this memory." T's
	// evidence rows still exist and still reference T; they don't know
	// M has been shredded. The proper fix is an explicit reconciliation
	// tool — until then, this assertion pins the silent drift.
	var confAfterMemDeleted float64
	require.NoError(t, dm.QueryRowTracked(
		`SELECT confidence FROM memories WHERE id = ?`, theoryID,
	).Scan(&confAfterMemDeleted))

	assert.InDelta(t, confAfterEvidence, confAfterMemDeleted, 1e-9,
		"CASCADING DECAY GAP: theory confidence unchanged after underlying memory deleted "+
			"(was %v, still %v). The evidence model has no concept of forward dependencies. "+
			"Until reconcile_theories is built, this drift is silent — the theory continues to "+
			"claim whatever it claimed with unchanged confidence, even though a foundation has rotted.",
		confAfterEvidence, confAfterMemDeleted)
}