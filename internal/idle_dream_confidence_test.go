package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdleDream_ConfidenceDecayCycle_RecomputesStaleArtifacts(t *testing.T) {
	dm := newTestDM(t)

	// Insert a memory and add positive evidence, then backdate the evidence
	// so it counts as stale.
	memID := "stale-1"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)
	evID := GenerateID()
	_, err = dm.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at)
		VALUES (?, ?, 'memory', 'reproduction', 'x', 0.85, 'test', ?)
	`, 0, evID, memID, time.Now().Add(-200*24*time.Hour).Unix()) // 200 days old
	require.NoError(t, err)

	worker := &IdleConsolidationWorker{db: dm}
	ran, err := worker.ConfidenceDecayCycle()
	require.NoError(t, err)
	assert.Greater(t, ran, 0, "stale artifact should be recomputed")

	var decayCount int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ? AND trigger = 'decay_tick'`, memID,
	).Scan(&decayCount))
	assert.GreaterOrEqual(t, decayCount, 1, "decay_tick history row should be present")
}

func TestIdleDream_ConfidenceDecayCycle_SkipsRecentArtifacts(t *testing.T) {
	dm := newTestDM(t)

	memID := "fresh-1"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)
	evID := GenerateID()
	_, err = dm.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at)
		VALUES (?, ?, 'memory', 'reproduction', 'x', 0.85, 'test', ?)
	`, 0, evID, memID, time.Now().Unix()) // brand new
	require.NoError(t, err)

	worker := &IdleConsolidationWorker{db: dm}
	ran, err := worker.ConfidenceDecayCycle()
	require.NoError(t, err)
	assert.Equal(t, 0, ran, "fresh artifact should be skipped")
}

// TestIdleDream_ConfidenceDecayCycle_PreservesTheoryEvidence pins the
// contract that the decay cycle recomputes theories and decisions
// under their correct artifact_type, not as "memory".
//
// The audit found that the cycle's candidate query hardcoded
// artifact_type='memory' for every row in the memories table. Theories
// and decisions live in the memories table but their evidence rows
// have artifact_type='theory' / 'decision'. A decay recompute under
// the wrong type would find zero evidence, anchor lastPositiveAt to
// the artifact's created_at, and reset the confidence to the initial
// value with full decay — silently wiping the contributions of every
// piece of evidence the theory or decision ever accumulated.
func TestIdleDream_ConfidenceDecayCycle_PreservesTheoryEvidence(t *testing.T) {
	dm := newTestDM(t)

	// Theory t1 with strong reproduction evidence, 30 days old. The
	// 30-day window is short enough that the theory's fast decay rate
	// (λ=0.02) doesn't dominate the evidence contribution; we want to
	// assert that the evidence actually participates in the math.
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'theories', 'x')`, 0, "t1")
	require.NoError(t, err)
	_, err = dm.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at)
		VALUES (?, ?, 'theory', 'reproduction', 'src', 0.85, 'test', ?)
	`, 0, "ev-t1", "t1", time.Now().Add(-30*24*time.Hour).Unix())
	require.NoError(t, err)

	worker := &IdleConsolidationWorker{db: dm}
	ran, err := worker.ConfidenceDecayCycle()
	require.NoError(t, err)
	assert.Greater(t, ran, 0, "the stale theory should be recomputed")

	// Read the recomputed confidence. With the fix, the evidence row
	// (artifact_type='theory') matches the candidate's artifact_type,
	// so the recompute honors the evidence. The evidence is 30 days
	// old; recency=0.86 and decay penalty=0.6 leave the evidence
	// contribution ≈0.73 in log-odds, lifting confidence above the
	// initial 0.5. Without the fix, the evidence is filtered out by
	// artifact_type mismatch and confidence would land at 0.5 with
	// decay from the artifact's creation time.
	var confAfter float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = 't1'`).Scan(&confAfter))
	assert.Greater(t, confAfter, 0.5,
		"theory confidence after decay recompute should reflect the strong reproduction evidence, not land at the initial 0.5")

	// Sanity: the recompute actually ran and wrote a history row tagged
	// decay_tick for the THEORY artifact_type (not memory). The fix's
	// CASE expression maps collection='theories' → 'theory'.
	var histCount int
	require.NoError(t, dm.QueryRowTracked(`
		SELECT COUNT(*) FROM confidence_history
		WHERE artifact_id = 't1' AND artifact_type = 'theory' AND trigger = 'decay_tick'
	`).Scan(&histCount))
	assert.GreaterOrEqual(t, histCount, 1,
		"decay_tick history row should be tagged with the correct artifact_type='theory'")
}

// TestIdleDream_ConfidenceDecayCycle_PreservesDecisionEvidence is the
// decision-collection counterpart to the theory test above. Same
// root cause, different artifact_type — both pre-fix were silently
// stripped of evidence on every decay tick.
func TestIdleDream_ConfidenceDecayCycle_PreservesDecisionEvidence(t *testing.T) {
	dm := newTestDM(t)

	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'decisions', 'x')`, 0, "d1")
	require.NoError(t, err)
	_, err = dm.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at)
		VALUES (?, ?, 'decision', 'decision_outcome', 'src', 0.95, 'test', ?)
	`, 0, "ev-d1", "d1", time.Now().Add(-200*24*time.Hour).Unix())
	require.NoError(t, err)

	worker := &IdleConsolidationWorker{db: dm}
	ran, err := worker.ConfidenceDecayCycle()
	require.NoError(t, err)
	assert.Greater(t, ran, 0, "the stale decision should be recomputed")

	var confAfter float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = 'd1'`).Scan(&confAfter))
	assert.Greater(t, confAfter, 0.6,
		"decision confidence after decay recompute should reflect the decision_outcome evidence, not land at the initial 0.6")

	var histCount int
	require.NoError(t, dm.QueryRowTracked(`
		SELECT COUNT(*) FROM confidence_history
		WHERE artifact_id = 'd1' AND artifact_type = 'decision' AND trigger = 'decay_tick'
	`).Scan(&histCount))
	assert.GreaterOrEqual(t, histCount, 1,
		"decay_tick history row should be tagged with the correct artifact_type='decision'")
}
