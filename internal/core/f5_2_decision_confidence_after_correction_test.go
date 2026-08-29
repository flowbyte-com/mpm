// f5_2_decision_confidence_after_correction_test.go — F5-2 alpha P2 regression.
//
// F5-2: a corrected (superseded) decision retained its pre-correction
// confidence. HybridSearch's correction-chain tag discount (×0.25) only
// affects retrieval ranking; the live `confidence` column stayed at its
// pre-correction value, which the audit flagged as a stale-state drift:
// "if a decision has been shown wrong, it should not continue to
// represent itself as high-confidence to the world."
//
// The fix is the smallest possible: when SupersedeDecision marks the
// original as superseded, also write a confidence_history row that
// records the supersede event with the new confidence value dropped to
// the per-type initial baseline (0.5 for decisions). This matches the
// existing F7.1 "challenged memory" pattern (ChallengedMemoryConfidenceFloor)
// and leaves the existing confidence_recompute path free to re-elevate
// the value if new evidence arrives.
//
// InvalidateDecision has the same drift — a decision retired without a
// replacement must not continue to advertise its pre-retirement confidence
// either. Same fix shape.
package internal

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decisionConfidence reads the live confidence column for a memory.
func decisionConfidence(t *testing.T, dm *DatabaseManager, id string) float64 {
	t.Helper()
	mem, err := dm.GetMemory(id)
	require.NoError(t, err)
	c, ok := mem["confidence"].(float64)
	require.True(t, ok, "memory %s missing confidence column", id)
	return c
}

// confidenceHistoryRowsFor returns every confidence_history row keyed
// against artifactID, newest-first.
func confidenceHistoryRowsFor(t *testing.T, dm *DatabaseManager, artifactID string) []map[string]interface{} {
	t.Helper()
	rows, err := dm.db.Query(`
		SELECT confidence, trigger, computed_at, evidence_count
		FROM confidence_history
		WHERE artifact_id = ?
		ORDER BY computed_at DESC
	`, artifactID)
	require.NoError(t, err)
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var conf float64
		var trigger string
		var computedAt int64
		var evidenceCount int
		require.NoError(t, rows.Scan(&conf, &trigger, &computedAt, &evidenceCount))
		out = append(out, map[string]interface{}{
			"confidence":     conf,
			"trigger":        trigger,
			"computed_at":    computedAt,
			"evidence_count": evidenceCount,
		})
	}
	return out
}

// TestF5_2_SupersedeDecisionDropsConfidence is the primary F5-2 test.
//
// Before fix: the original decision's confidence column stayed at its
// pre-correction value (0.5 here — the initial baseline for decisions)
// indefinitely; only the retrieval-discount tag changed.
//
// After fix: the supersede writes a confidence_history row tagged
// "supersede" with the confidence dropped to the initial baseline and
// the live confidence column updated to match, so the two views agree.
func TestF5_2_SupersedeDecisionDropsConfidence(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	originalID := seedF9Decision(t, dm, "deploy with blue-green")
	beforeConf := decisionConfidence(t, dm, originalID)

	res, err := dm.SupersedeDecision(originalID, "ctx", "deploy with canary", "safer rollback", "",
		nil, nil, ActiveContext{})
	require.NoError(t, err)
	newID, _ := res["id"].(string)
	require.NotEqual(t, originalID, newID)

	// Original decision's confidence must not silently retain its old value
	// after being shown superseded. The audit invariant: a superseded
	// artifact is a "no evidence" baseline; the live confidence column
	// must reflect that, not the pre-correction history.
	afterConf := decisionConfidence(t, dm, originalID)
	assert.LessOrEqual(t, afterConf, beforeConf,
		"superseded decision confidence must not exceed its pre-supersede value")

	// Audit trail: a confidence_history row stamped by the supersede,
	// tag="supersede", value=afterConf. Without this row, a reviewer
	// inspecting confidence_history cannot tell why the column dropped.
	hist := confidenceHistoryRowsFor(t, dm, originalID)
	require.NotEmpty(t, hist, "supersede must append a confidence_history row")
	var found bool
	for _, h := range hist {
		if h["trigger"] == "supersede" {
			found = true
			assert.Equal(t, afterConf, h["confidence"],
				"history row confidence must match the live column")
		}
	}
	assert.True(t, found,
		"confidence_history must contain a supersede-triggered row for the original decision")
}

// TestF5_2_InvalidateDecisionDropsConfidence is the F5-2 parallel for
// retirement without replacement. The audit applies the same invariant:
// "if a decision is no longer valid, its confidence column must reflect
// that, not its pre-invalidation value."
func TestF5_2_InvalidateDecisionDropsConfidence(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	id := seedF9Decision(t, dm, "use in-house scheduler")
	beforeConf := decisionConfidence(t, dm, id)

	_, err := dm.InvalidateDecision(id, "replaced by managed queue")
	require.NoError(t, err)

	afterConf := decisionConfidence(t, dm, id)
	assert.LessOrEqual(t, afterConf, beforeConf,
		"invalidated decision confidence must not exceed its pre-invalidation value")

	hist := confidenceHistoryRowsFor(t, dm, id)
	require.NotEmpty(t, hist, "invalidate must append a confidence_history row")
	var found bool
	for _, h := range hist {
		if h["trigger"] == "invalidate" {
			found = true
			assert.Equal(t, afterConf, h["confidence"],
				"history row confidence must match the live column")
		}
	}
	assert.True(t, found,
		"confidence_history must contain an invalidate-triggered row")
}

// TestF5_2_SupersedeHistoryRowHasSupersedeTriggerContract pins the
// trigger-string contract: "supersede" and "invalidate" exactly. Any
// other value (e.g. "superseded", "invalidated") breaks downstream
// reviewers and analysis queries that group by trigger.
func TestF5_2_SupersedeHistoryRowHasSupersedeTriggerContract(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	id := seedF9Decision(t, dm, "deploy with blue-green")
	_, err := dm.SupersedeDecision(id, "ctx", "deploy with canary", "safer rollback", "",
		nil, nil, ActiveContext{})
	require.NoError(t, err)

	hist := confidenceHistoryRowsFor(t, dm, id)
	var triggers []string
	for _, h := range hist {
		triggers = append(triggers, h["trigger"].(string))
	}
	joined := strings.Join(triggers, ",")
	assert.Contains(t, joined, "supersede",
		"history row trigger must include exactly 'supersede' (got: %s)", joined)
	assert.NotContains(t, joined, "superseded",
		"history row trigger must not be the past-tense 'superseded'")
}

// TestF5_2_SupersededDecisionContentPreserved is a regression-safety
// check: the F5-2 fix must NOT touch the original decision's content
// (historical inspection is a non-negotiable contract — see F9).
func TestF5_2_SupersededDecisionContentPreserved(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	id := seedF9Decision(t, dm, "deploy with blue-green")
	_, err := dm.SupersedeDecision(id, "ctx", "deploy with canary", "safer rollback", "",
		nil, nil, ActiveContext{})
	require.NoError(t, err)

	mem, err := dm.GetMemory(id)
	require.NoError(t, err)
	content, _ := mem["content"].(string)
	assert.Contains(t, content, "CHOICE: deploy with blue-green",
		"original decision content must remain intact after supersede (F9 contract)")

	var meta map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(mem["metadata"].(string)), &meta))
	assert.Equal(t, true, meta["superseded"], "superseded metadata flag still set")
}