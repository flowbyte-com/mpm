// challenge_restore_accuracy_test.go — issue #11 regressions.
//
// Issue #11: `mpm challenge restore` could report complete success when the
// associated theory resolution updated ZERO rows. The theory UPDATE was
// executed without reading its RowsAffected; a challenged_theory_id that
// matched nothing raised no error, the transaction committed, and the
// returned map asserted `"theory_status":"disproven"` unconditionally. The
// CLI then printed "✓ Theory <id> resolved as disproven" for a resolution
// that never happened.
//
// These tests pin the corrected contract:
//   - what the core reports is what the database did (RowsAffected);
//   - a stale or non-identifier theory reference degrades to a truthful
//     partial result, never to a false resolution;
//   - a non-theory row, or a theory belonging to a DIFFERENT memory, is
//     never stamped `disproven` (no speculative matching, no collateral
//     damage to unrelated theories);
//   - the two halves stay atomic: a failure in the memory half rolls back
//     the theory half.
//
// Every test runs against an isolated in-memory database (NewTestDM). No
// live MPM database, workspace, or service is touched.
package internal

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// challengeTarget seeds a memory at weight 9 and challenges it through the
// production path (ChallengeMemoryWithTheory), returning the memory id and
// the id of the theory that challenged it.
func challengeTarget(t *testing.T, dm *DatabaseManager, content string) (memID, theoryID string) {
	t.Helper()
	memID = seedWeightedMemory(t, dm, content, 9)
	_, err := dm.ChallengeMemoryWithTheory(memID, "contradicted by newer measurement")
	require.NoError(t, err)

	_, meta := persistedWeightAndMeta(t, dm, memID)
	theoryID, _ = meta["challenged_theory_id"].(string)
	require.NotEmpty(t, theoryID, "setup: ChallengeMemoryWithTheory must link the real theory id")
	return memID, theoryID
}

// theoryMeta reads a theory row's metadata.
func theoryMeta(t *testing.T, dm *DatabaseManager, id string) map[string]interface{} {
	t.Helper()
	var metaStr string
	err := dm.db.QueryRow(`SELECT COALESCE(metadata,'{}') FROM memories WHERE id = ?`, id).Scan(&metaStr)
	require.NoError(t, err)
	meta := map[string]interface{}{}
	require.NoError(t, json.Unmarshal([]byte(metaStr), &meta))
	return meta
}

// TestIssue11_RestoreWithValidMetadata_ResolvesTheory: the healthy path.
// A real theory created by ChallengeMemoryWithTheory carries both
// collection='theories' and metadata.challenged_memory_id, so exactly one
// row matches and the restore reports a genuine resolution.
func TestIssue11_RestoreWithValidMetadata_ResolvesTheory(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	memID, theoryID := challengeTarget(t, dm, "issue11 healthy restore path")
	require.Equal(t, float64(7), persistedWeightOnly(t, dm, memID), "setup: challenge must have weakened 9 → 7")

	res, err := dm.RestoreMemoryFromChallenge(memID)
	require.NoError(t, err)

	assert.Equal(t, true, res["theory_resolved"], "a matched theory row must be reported as resolved")
	assert.Equal(t, true, res["complete"], "nothing is missing, so the restore is complete")
	assert.Equal(t, TheoryResolutionResolved, res["theory_resolution"])
	assert.Equal(t, "disproven", res["theory_status"])
	assert.Equal(t, int64(1), res["theory_rows_affected"])
	assert.Equal(t, TheoryReferenceIdentifier, res["theory_reference"])

	// Persisted state, not just the returned map.
	assert.Equal(t, "disproven", theoryMeta(t, dm, theoryID)["status"])
	w, meta := persistedWeightAndMeta(t, dm, memID)
	assert.Equal(t, float64(9), w, "pre-challenge weight must be restored")
	assert.NotContains(t, meta, "status")
	assert.NotContains(t, meta, "challenged_theory_id")
	assert.Equal(t, true, meta["restored_from_challenge"])
}

// TestIssue11_RestoreWithMissingTheory_ReportsNotResolved: the referenced
// theory row does not exist. Before the fix this returned
// theory_status="disproven" with no error.
func TestIssue11_RestoreWithMissingTheory_ReportsNotResolved(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	memID, _ := challengeTarget(t, dm, "issue11 theory row deleted")
	// Soft-delete the theory, as a retention sweep would.
	_, err := dm.db.Exec(`UPDATE memories SET deleted_at = unixepoch() WHERE id IN (SELECT id FROM memories WHERE collection='theories')`)
	require.NoError(t, err)

	res, err := dm.RestoreMemoryFromChallenge(memID)
	require.NoError(t, err, "a missing theory is a partial result, not a hard failure")

	assert.Equal(t, false, res["theory_resolved"],
		"ISSUE 11 REGRESSION: a missing theory must not be reported as resolved")
	assert.Equal(t, false, res["complete"])
	assert.Equal(t, TheoryResolutionNotFound, res["theory_resolution"])
	assert.Equal(t, int64(0), res["theory_rows_affected"])
	assert.Empty(t, res["theory_status"],
		"ISSUE 11 REGRESSION: theory_status must not assert a status for a row that was never located")
	assert.NotEmpty(t, res["theory_warning"], "an incomplete restore must say why")

	// The memory half still ran — that is the point of partial semantics.
	w, meta := persistedWeightAndMeta(t, dm, memID)
	assert.Equal(t, float64(9), w)
	assert.NotContains(t, meta, "challenged_theory_id")
}

// TestIssue11_RestoreWithFreeTextReference_RestoresMemoryFlagsLegacyShape:
// rows written by the retired auto-challenge defect store a human-readable
// reason in challenged_theory_id, because ChallengeMemory writes its
// `evidence` argument there. There is no theory row to resolve. The restore
// must still repair the memory — refusing would leave it unrestorable — but
// must not claim a resolution.
func TestIssue11_RestoreWithFreeTextReference_RestoresMemoryFlagsLegacyShape(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	memID := seedWeightedMemory(t, dm, "issue11 legacy free text reference", 9)
	// ChallengeMemory (not ...WithTheory) is the legacy writer: it puts the
	// raw evidence string into challenged_theory_id.
	require.NoError(t, dm.ChallengeMemory(memID, 2, "contradicted by runbook revision 2026-10"))

	_, meta := persistedWeightAndMeta(t, dm, memID)
	stored, _ := meta["challenged_theory_id"].(string)
	require.Equal(t, "contradicted by runbook revision 2026-10", stored,
		"setup: ChallengeMemory must store the raw evidence string")

	res, err := dm.RestoreMemoryFromChallenge(memID)
	require.NoError(t, err)

	assert.Equal(t, TheoryReferenceFreeText, res["theory_reference"],
		"prose in challenged_theory_id must be labelled free_text, not treated as an id")
	assert.Equal(t, false, res["theory_resolved"])
	assert.Equal(t, false, res["complete"])
	assert.Equal(t, TheoryResolutionNotFound, res["theory_resolution"])

	// The recoverable half is still recovered.
	w, after := persistedWeightAndMeta(t, dm, memID)
	assert.Equal(t, float64(9), w, "legacy rows must still be weight-restorable")
	assert.NotContains(t, after, "challenged_theory_id")
	assert.NotContains(t, after, "status")
}

// TestIssue11_FreeTextCollidingWithRealMemory_DoesNotStampDisproven:
// requirement "do not silently mark unrelated theories resolved". A
// free-text reference is arbitrary prose, and prose can collide with an
// ordinary memory's id. Before the guard was added the UPDATE had no
// collection filter, so it would have stamped status=disproven onto an
// unrelated memory — permanently, since nothing clears that key.
func TestIssue11_FreeTextCollidingWithRealMemory_DoesNotStampDisproven(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	// An ordinary memory whose id is exactly the string that will appear in
	// challenged_theory_id.
	const collide = "contradicted by runbook revision 2026-10"
	victimID, err := dm.SaveMemory("memories", "an entirely unrelated memory", "", []string{"issue11"}, nil, nil, false, 5)
	require.NoError(t, err)
	// Force the id so the collision is exact rather than incidental.
	_, err = dm.db.Exec(`UPDATE memories SET id = ? WHERE id = ?`, collide, victimID)
	require.NoError(t, err)

	challengedID := seedWeightedMemory(t, dm, "issue11 collision challenger", 9)
	require.NoError(t, dm.ChallengeMemory(challengedID, 2, collide))

	_, err = dm.RestoreMemoryFromChallenge(challengedID)
	require.NoError(t, err)

	victim := theoryMeta(t, dm, collide)
	assert.NotContains(t, victim, "status",
		"ISSUE 11 REGRESSION: an unrelated memory was stamped by a colliding free-text reference")

	var victimCollection string
	require.NoError(t, dm.db.QueryRow(`SELECT collection FROM memories WHERE id = ?`, collide).Scan(&victimCollection))
	assert.Equal(t, "memories", victimCollection,
		"setup sanity: the victim is an ordinary memory, not a theory")
}

// TestIssue11_TheoryOfAnotherMemory_IsNotResolved: a reference that names a
// REAL theory, but one that challenged a different memory, must not be
// resolved. Marking it disproven would resolve a contradiction the operator
// never adjudicated.
func TestIssue11_TheoryOfAnotherMemory_IsNotResolved(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	_, foreignTheory := challengeTarget(t, dm, "issue11 owner of the foreign theory")
	victimID, _ := challengeTarget(t, dm, "issue11 victim of a borrowed reference")

	// Repoint the victim's reference at the other memory's theory — a stale
	// or mis-patched link.
	_, err := dm.db.Exec(
		`UPDATE memories SET metadata = json_set(COALESCE(metadata,'{}'), '$.challenged_theory_id', ?) WHERE id = ?`,
		foreignTheory, victimID)
	require.NoError(t, err)

	res, err := dm.RestoreMemoryFromChallenge(victimID)
	require.NoError(t, err)

	assert.Equal(t, false, res["theory_resolved"],
		"a theory that challenged a different memory must not be resolved by this restore")
	assert.Equal(t, int64(0), res["theory_rows_affected"])
	assert.Equal(t, "pending", theoryMeta(t, dm, foreignTheory)["status"],
		"the unrelated theory must be left exactly as it was")

	w, _ := persistedWeightAndMeta(t, dm, victimID)
	assert.Equal(t, float64(9), w, "the memory itself is still restored")
}

// TestIssue11_RestoreIsAtomic_RollbackOnMemoryHalfFailure: the theory
// UPDATE runs first, so a failure in the memory half must roll the theory
// resolution back. Otherwise a failed restore leaves a theory marked
// disproven — an adjudication that never completed.
func TestIssue11_RestoreIsAtomic_RollbackOnMemoryHalfFailure(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	memID, theoryID := challengeTarget(t, dm, "issue11 rollback target")

	// Abort the memory-half UPDATE only. The theory-half UPDATE targets the
	// theory's own id, so a WHEN clause on the memory id makes the failure
	// land in the second statement — after the theory was already updated
	// inside the transaction.
	//
	// SQLite cannot bind parameters in a CREATE TRIGGER body, so the id is
	// interpolated. It comes from GenerateID, so it is 16 hex chars; assert
	// that rather than assuming it, so a future change to id generation
	// cannot turn this into a syntax error or an injection.
	require.Equal(t, TheoryReferenceIdentifier, classifyTheoryReference(memID),
		"setup: the memory id must be a generated hex id for safe interpolation")
	_, err := dm.db.Exec(fmt.Sprintf(`
		CREATE TRIGGER issue11_block_memory_half
		BEFORE UPDATE ON memories
		WHEN NEW.id = '%s'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`, memID))
	require.NoError(t, err)

	res, err := dm.RestoreMemoryFromChallenge(memID)
	require.Error(t, err, "an aborted UPDATE must surface as an error, not as a silent no-op")

	// No result map may accompany a failed restore.
	assert.Nil(t, res)

	assert.Equal(t, "pending", theoryMeta(t, dm, theoryID)["status"],
		"ISSUE 11 REGRESSION: the theory resolution must roll back with the memory half")
	_, meta := persistedWeightAndMeta(t, dm, memID)
	assert.Equal(t, "challenged", meta["status"], "the memory must be left untouched on rollback")
	assert.Equal(t, float64(7), persistedWeightOnly(t, dm, memID), "weight must be left slashed")
}

// TestIssue11_RepeatedRestore_IsIdempotentAndNonDrifting: restore twice.
// The second call must fail loudly (the memory is no longer challenged)
// rather than re-resolving the theory or drifting the weight. Also
// re-challenging after a restore must behave exactly like the first cycle.
func TestIssue11_RepeatedRestore_IsIdempotentAndNonDrifting(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	memID, _ := challengeTarget(t, dm, "issue11 repeated restore")

	res, err := dm.RestoreMemoryFromChallenge(memID)
	require.NoError(t, err)
	assert.Equal(t, true, res["theory_resolved"])
	assert.Equal(t, float64(9), persistedWeightOnly(t, dm, memID))

	// Second restore: the metadata no longer carries challenged_theory_id.
	_, err = dm.RestoreMemoryFromChallenge(memID)
	require.Error(t, err, "restoring an already-restored memory must say so, not no-op silently")
	assert.Contains(t, err.Error(), "not challenged")

	assert.Equal(t, float64(9), persistedWeightOnly(t, dm, memID),
		"a rejected repeat must not drift the weight")

	// A fresh challenge/restore cycle returns to the same baseline.
	for cycle := 0; cycle < 3; cycle++ {
		_, err := dm.ChallengeMemoryWithTheory(memID, "issue11 re-challenge")
		require.NoError(t, err)
		assert.Equal(t, float64(7), persistedWeightOnly(t, dm, memID), "cycle %d", cycle)

		res, err := dm.RestoreMemoryFromChallenge(memID)
		require.NoError(t, err)
		assert.Equal(t, true, res["theory_resolved"], "cycle %d: a fresh theory must resolve", cycle)
		assert.Equal(t, float64(9), persistedWeightOnly(t, dm, memID), "cycle %d: no drift", cycle)
	}
}

// TestClassifyTheoryReference pins the lexical classifier. It is a LABEL
// for reporting only — it never gates whether a theory is resolved — so its
// contract is exactly "does this look like a GenerateID value".
func TestClassifyTheoryReference(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"generated id", "0123456789abcdef", TheoryReferenceIdentifier},
		{"uppercase hex id", "0123456789ABCDEF", TheoryReferenceIdentifier},
		{"legacy prose", "contradicted by runbook revision 2026-10", TheoryReferenceFreeText},
		{"arbitration id", "arbitration-a-b-1758000000000000000", TheoryReferenceFreeText},
		{"empty", "", TheoryReferenceFreeText},
		{"sixteen non-hex chars", "zzzzzzzzzzzzzzzz", TheoryReferenceFreeText},
		{"fifteen hex chars", "0123456789abcde", TheoryReferenceFreeText},
		{"seventeen hex chars", "0123456789abcdef0", TheoryReferenceFreeText},
		{"padded id", "  0123456789abcdef  ", TheoryReferenceIdentifier},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyTheoryReference(tc.input))
		})
	}
}

// persistedWeightOnly is a narrow read helper for weight-only assertions.
func persistedWeightOnly(t *testing.T, dm *DatabaseManager, id string) float64 {
	t.Helper()
	w, _ := persistedWeightAndMeta(t, dm, id)
	return w
}
