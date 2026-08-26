// challenge_weight_regression_test.go — F11 alpha-blocker regression.
//
// Audit finding F11: `mpm_memory challenge` reported "weakened" but the
// stored weight INCREASED (5 → 7) on every reproduction. Root cause:
// ChallengeMemoryWithTheory passed slashAmount=-2 into
// ChallengeMemory, which computes `weight = MAX(1, weight - ?)` —
// subtracting a negative amount silently boosted disputed knowledge.
//
// The tests below inspect PERSISTED state directly (not presentation
// output) and cover: no weight inflation, restore-to-prior semantics,
// repeated challenge/restore cycles, varied initial weights, negative
// slash rejection, and ranking parity with an untouched clean memory.
package internal

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func persistedWeightAndMeta(t *testing.T, dm *DatabaseManager, id string) (int, map[string]interface{}) {
	t.Helper()
	var weight int
	var metaStr string
	err := dm.db.QueryRow(`SELECT COALESCE(weight,0), COALESCE(metadata,'{}') FROM memories WHERE id = ? AND deleted_at IS NULL`, id).
		Scan(&weight, &metaStr)
	require.NoError(t, err)
	meta := map[string]interface{}{}
	require.NoError(t, json.Unmarshal([]byte(metaStr), &meta))
	return weight, meta
}

func seedWeightedMemory(t *testing.T, dm *DatabaseManager, content string, weight int) string {
	t.Helper()
	id, err := dm.SaveMemory("memories", content, "", []string{"f11"}, nil, nil, false, weight)
	require.NoError(t, err)
	return id
}

// TestF11_ChallengeMustNotIncreaseWeight: the core inversion. Weight 5,
// challenged via the MCP/call surface (ChallengeMemoryWithTheory), must end
// up WEAKENED — never above 5.
func TestF11_ChallengeMustNotIncreaseWeight(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	for _, initial := range []int{5, 3, 7, 12} {
		// Unique content per case: F19 identity dedup makes an identical
		// re-save return the first row, which would conflate the cases.
		id := seedWeightedMemory(t, dm, fmt.Sprintf("deploy to prod via blue-green %d", initial), initial)
		res, err := dm.ChallengeMemoryWithTheory(id, "contradicted by ops runbook 2026-08")
		require.NoError(t, err)
		assert.Equal(t, "weakened", res["action"])

		weight, meta := persistedWeightAndMeta(t, dm, id)
		assert.Equal(t, "challenged", meta["status"], "initial=%d", initial)
		assert.LessOrEqual(t, weight, initial, "challenge must NEVER increase stored weight (initial=%d)", initial)
		assert.Equal(t, max(1, initial-2), weight, "challenge should weaken by exactly 2 (initial=%d)", initial)

		// Prior epistemic standing is recorded for restore.
		prior, ok := meta["challenged_prior_weight"].(float64)
		require.True(t, ok, "challenged_prior_weight must be recorded (initial=%d)")
		assert.Equal(t, float64(initial), prior)
	}
}

// TestF11_RestoreReturnsPriorEpistemicState: after a successful restore the
// memory's weight equals its pre-challenge value and no challenge keys remain.
func TestF11_RestoreReturnsPriorEpistemicState(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	id := seedWeightedMemory(t, dm, "cache TTL is 300s", 6)
	_, err := dm.ChallengeMemoryWithTheory(id, "TTL changed to 60s in config")
	require.NoError(t, err)

	wChallenged, meta := persistedWeightAndMeta(t, dm, id)
	assert.Equal(t, 4, wChallenged)

	// Restore through the same code path as `mpm challenge restore`:
	// resolve theory disproven + clear status + reset prior weight.
	theoryID, _ := meta["challenged_theory_id"].(string)
	require.NotEmpty(t, theoryID)
	restoreForTest(t, dm, id, theoryID)

	wRestored, metaAfter := persistedWeightAndMeta(t, dm, id)
	assert.Equal(t, 6, wRestored, "restore must return the pre-challenge weight")
	assert.NotContains(t, metaAfter, "status")
	assert.NotContains(t, metaAfter, "challenged_theory_id")
	assert.NotContains(t, metaAfter, "challenged_prior_weight")

	var theoryStatus string
	err = dm.db.QueryRow(`SELECT json_extract(metadata,'$.status') FROM memories WHERE id = ?`, theoryID).Scan(&theoryStatus)
	require.NoError(t, err)
	assert.Equal(t, "disproven", theoryStatus)
}

// TestF11_RepeatedChallengeRestoreCyclesStable: cycles must not drift the
// weight upward or downward across repetitions.
func TestF11_RepeatedChallengeRestoreCyclesStable(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	id := seedWeightedMemory(t, dm, "retry thrice then alert", 9)
	const original = 9

	for cycle := 0; cycle < 3; cycle++ {
		_, err := dm.ChallengeMemoryWithTheory(id, "cycle evidence")
		require.NoError(t, err)
		w, meta := persistedWeightAndMeta(t, dm, id)
		assert.Equal(t, original-2, w, "cycle %d: weakened weight must be deterministic", cycle)
		theoryID, _ := meta["challenged_theory_id"].(string)
		require.NotEmpty(t, theoryID)
		restoreForTest(t, dm, id, theoryID)
		wAfter, _ := persistedWeightAndMeta(t, dm, id)
		assert.Equal(t, original, wAfter, "cycle %d: restore must return to original weight", cycle)
	}
}

// TestF11_DisputedCannotOutrankCleanEquivalent: a challenged memory with the
// same base weight must not outrank an untouched clean memory in retrieval
// scoring purely because of the challenge.
func TestF11_DisputedCannotOutrankCleanEquivalent(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	cleanID := seedWeightedMemory(t, dm, "unique-f11-marker kafka partition rebalancing", 5)
	challengedID := seedWeightedMemory(t, dm, "unique-f11-marker kafka consumer lag", 5)

	_, err := dm.ChallengeMemoryWithTheory(challengedID, "stale advice")
	require.NoError(t, err)

	cleanW, _ := persistedWeightAndMeta(t, dm, cleanID)
	chalW, _ := persistedWeightAndMeta(t, dm, challengedID)
	assert.Greater(t, cleanW, chalW, "clean memory must outrank challenged equivalent at the persistence layer")

	// And the challenged one carries the retrieval-time dispute banner flag.
	_, meta := persistedWeightAndMeta(t, dm, challengedID)
	assert.Equal(t, "challenged", meta["status"])
}

// TestF11_NegativeSlashRejected: ChallengeMemory must reject a negative
// reduction loudly instead of silently boosting the disputed memory.
func TestF11_NegativeSlashRejected(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	id := seedWeightedMemory(t, dm, "negative slash target", 5)
	err := dm.ChallengeMemory(id, -2, "inverted amount must be rejected")
	assert.Error(t, err, "negative slashAmount must be rejected, not inverted into a boost")

	w, _ := persistedWeightAndMeta(t, dm, id)
	assert.Equal(t, 5, w, "rejected challenge must leave weight untouched")
}

// restoreForTest mirrors cmd/mpm handleChallengeRestore's transaction so the
// core-level tests exercise identical semantics without shelling out.
func restoreForTest(t *testing.T, dm *DatabaseManager, memID, theoryID string) {
	t.Helper()
	tx, err := dm.db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	resolveJSON, _ := json.Marshal(map[string]interface{}{"status": "disproven", "memory_id": nil})
	_, err = tx.Exec(`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ? AND deleted_at IS NULL`, string(resolveJSON), theoryID)
	require.NoError(t, err)

	clearJSON, _ := json.Marshal(map[string]interface{}{"status": nil, "challenged_theory_id": nil, "challenged_prior_weight": nil})
	// Weight restoration uses the recorded prior value:
	var prior int
	err = tx.QueryRow(`SELECT CAST(json_extract(metadata,'$.challenged_prior_weight') AS INTEGER) FROM memories WHERE id = ? AND deleted_at IS NULL`, memID).Scan(&prior)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?), weight = ? WHERE id = ? AND deleted_at IS NULL`, string(clearJSON), prior, memID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}
