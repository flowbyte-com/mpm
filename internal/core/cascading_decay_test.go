package internal

import (
	"database/sql"
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
	res, err := dm.ProposeTheory(hypothesis, "if M is shredded, T should be re-evaluated", nil, nil, []string{"test", "cascading-decay"})
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
		SourceGroup:        "test",
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
// TestStaleFoundationWake_FiresOnDeclaredDependencyRemoval is the positive
// counterpart to the forcing function above. With the dependencies JSON
// column in place, a theory that EXPLICITLY declares a dependency
// triggers a stale-foundation wake when that dependency is soft-deleted.
//
// This is the visible proof that Gap #2 is closed: forward dependency
// edges are now part of the schema, and the substrate emits a wake
// when the edge breaks. The agent decides what to do with the wake
// (re-evaluate the theory, mark it disproven, dismiss).
//
// Sequence:
//  1. Insert memory M.
//  2. Propose theory T with explicit dependencies=[M.id].
//  3. Soft-delete M.
//  4. Assert: a wake exists with theory_id=T.id and
//     metadata.missing_artifact_id=M.id.
func TestStaleFoundationWake_FiresOnDeclaredDependencyRemoval(t *testing.T) {
	dm := newTestDM(t)

	// Step 1: foundational memory M.
	memID, err := dm.SaveMemory(
		"memories",
		"Stale foundation test: M is a foundational fact.",
		"", nil, nil, nil,
		false, 5,
	)
	require.NoError(t, err)

	// Step 2: theory T that explicitly declares M as a dependency.
	res, err := dm.ProposeTheory(
		"T depends on M",
		"if M is shredded, T is orphaned",
		[]string{memID}, // <-- the new dependencies parameter
		nil, // source_ids
		[]string{"test", "stale-foundation"},
	)
	require.NoError(t, err)
	theoryID, _ := res["id"].(string)
	require.NotEmpty(t, theoryID)

	// Verify dependencies column was persisted.
	var depsJSON sql.NullString
	require.NoError(t, dm.QueryRowTracked(
		`SELECT dependencies FROM memories WHERE id = ?`, theoryID,
	).Scan(&depsJSON))
	require.True(t, depsJSON.Valid, "dependencies column should be populated")
	require.Contains(t, depsJSON.String, memID, "dependencies JSON should contain M's id")

	// Step 3: soft-delete M via the production path.
	store := &MemoryStore{DM: dm, DB: &SQLiteConnection{DB: dm.SQLDB()}}
	require.NoError(t, store.DeleteMemory(memID, "memories"))

	// Step 4: assert the stale-foundation wake fired with the right payload.
	var wakeCount int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE theory_id = ?`, theoryID,
	).Scan(&wakeCount))
	require.Equal(t, 1, wakeCount, "exactly one stale-foundation wake should fire for the dependent theory")

	// Verify the wake carries structured metadata for programmatic consumers.
	rows, err := dm.SQLDB().Query(`
		SELECT reason, metadata FROM scheduled_wakes WHERE theory_id = ?
	`, theoryID)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next(), "expected at least one row from the wake query")

	var reason, metaJSON string
	require.NoError(t, rows.Scan(&reason, &metaJSON))
	require.Contains(t, reason, memID, "wake reason should mention the missing artifact")
	require.Contains(t, reason, theoryID, "wake reason should mention the dependent theory")
	require.Contains(t, metaJSON, `"type":"stale_foundation"`, "metadata should carry structured type field")
	require.Contains(t, metaJSON, `"theory_id":"`+theoryID+`"`, "metadata should carry the dependent theory id")
	require.Contains(t, metaJSON, `"missing_artifact_id":"`+memID+`"`, "metadata should carry the missing artifact id")
}

// TestStaleFoundationWake_NoWakeForUnrelatedDelete confirms the hook
// is scoped: deleting a memory that NO theory depends on fires zero
// wakes. The reconcile must be a no-op for the common case.
func TestStaleFoundationWake_NoWakeForUnrelatedDelete(t *testing.T) {
	dm := newTestDM(t)

	// Memory M1, theory T1 that depends on M1.
	mem1ID, err := dm.SaveMemory("memories", "M1 is foundational.", "", nil, nil, nil, false, 5)
	require.NoError(t, err)
	_, err = dm.ProposeTheory("T1 depends on M1", "", []string{mem1ID}, nil, []string{"test"})
	require.NoError(t, err)

	// Unrelated memory M2 — no theory depends on it.
	mem2ID, err := dm.SaveMemory("memories", "M2 is unrelated.", "", nil, nil, nil, false, 5)
	require.NoError(t, err)

	store := &MemoryStore{DM: dm, DB: &SQLiteConnection{DB: dm.SQLDB()}}
	require.NoError(t, store.DeleteMemory(mem2ID, "memories"))

	// No wake should have fired for T1 (T1's dependency M1 is still alive).
	var wakeCount int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM scheduled_wakes`,
	).Scan(&wakeCount))
	assert.Equal(t, 0, wakeCount, "deleting an unrelated memory should fire zero wakes")
}

// TestStaleFoundationWake_TheoryWithNoDependenciesStillClean confirms
// the new column doesn't break the legacy path: theories with no
// declared dependencies (the historical default) behave exactly as
// before — soft-delete of any memory fires zero wakes for them.
func TestStaleFoundationWake_TheoryWithNoDependenciesStillClean(t *testing.T) {
	dm := newTestDM(t)

	memID, err := dm.SaveMemory("memories", "M is a fact.", "", nil, nil, nil, false, 5)
	require.NoError(t, err)

	// Theory with NO declared dependencies (legacy-style propose).
	_, err = dm.ProposeTheory("T makes no dependency claims", "", nil, nil, []string{"test"})
	require.NoError(t, err)

	store := &MemoryStore{DM: dm, DB: &SQLiteConnection{DB: dm.SQLDB()}}
	require.NoError(t, store.DeleteMemory(memID, "memories"))

	var wakeCount int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM scheduled_wakes`,
	).Scan(&wakeCount))
	assert.Equal(t, 0, wakeCount, "theory with no declared deps should fire no wakes on unrelated delete")
}
