package internal

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDetectConceptDriftCycle_FiresTheory tests the full end-to-end drift
// detection path: a high-confidence memory that accumulates challenge evidence
// should cause DetectConceptDriftCycle to propose a pending theory.
func TestDetectConceptDriftCycle_FiresTheory(t *testing.T) {
	dm := newTestDM(t)
	worker := &IdleConsolidationWorker{db: dm, logger: slog.Default()}

	memID := "drift-test-1"

	// Step 1: Insert a memory with high initial confidence.
	// confidence = 0.92 → log-odds = ln(0.92/0.08) ≈ 2.44
	_, err := dm.ExecTracked(`
		INSERT INTO memories (id, collection, content, confidence, deleted_at)
		VALUES (?, 'memories', 'Redis is the exclusive caching layer for the widget pipeline', ?, NULL)
	`, 0, memID, 0.92)
	require.NoError(t, err)

	// Step 2: Insert a peak confidence_history row so PeakConfidence CTE
	// returns 0.92 (not the current 0.92 which will be lower after challenges).
	histPeakID := GenerateID()
	_, err = dm.ExecTracked(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES (?, ?, 'memory', ?, ?, 1, 'evidence_added')
	`, 0, histPeakID, memID, 0.92, time.Now().Add(-30*24*time.Hour).Unix())
	require.NoError(t, err)

	// Step 3: Add challenge evidence — 3 lifetime, 2 recent.
	// Recent challenges (last 3 days) drive recent_challenges >= 2.
	// Lifetime challenges (2 months old) bring lifetime_challenges to 3.
	// Each challenge has strength -0.6 (the registry default for type='challenge').
	// Net effect should push confidence below 0.60 given the decay formula.
	now := time.Now()
	recentWindow := now.Add(-3 * 24 * time.Hour).Unix() // 3 days ago
	oldWindow := now.Add(-60 * 24 * time.Hour).Unix()  // 60 days ago

	challenges := []struct {
		id        string
		createdAt int64
	}{
		{GenerateID(), recentWindow}, // recent challenge 1
		{GenerateID(), recentWindow}, // recent challenge 2
		{GenerateID(), oldWindow},    // old challenge 3 (lifetime only)
	}
	for _, c := range challenges {
		_, err = dm.ExecTracked(`
			INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, independence_factor, created_by, created_at)
			VALUES (?, ?, 'memory', 'challenge', 'drift-test', -0.6, 1.0, 'test-agent', ?)
		`, 0, c.id, memID, c.createdAt)
		require.NoError(t, err)
	}

	// Step 4: Verify current confidence is below threshold before running cycle.
	var currentConf float64
	err = dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, memID).Scan(&currentConf)
	require.NoError(t, err)
	// Recompute manually so the row is current before DetectConceptDriftCycle reads it.
	require.NoError(t, RecomputeConfidence(dm, memID, "memory", RecomputeReasonEvidenceAdded))
	err = dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, memID).Scan(&currentConf)
	require.NoError(t, err)
	assert.Less(t, currentConf, 0.60, "confidence should be driven below 0.60 by challenge evidence")

	// Step 5: Run the drift detection cycle.
	worker.DetectConceptDriftCycle()

	// Step 6: Verify a theory was proposed.
	var theoryCount int
	err = dm.QueryRowTracked(`
		SELECT COUNT(*) FROM memories WHERE collection = 'theories' AND deleted_at IS NULL
	`).Scan(&theoryCount)
	require.NoError(t, err)
	assert.Equal(t, 1, theoryCount, "exactly one concept-drift theory should be proposed")

	// Step 7: Verify theory content and metadata.
	var theoryID, theoryContent string
	var theoryTags []byte
	var theoryMeta []byte
	err = dm.QueryRowTracked(`
		SELECT id, content, tags, metadata FROM memories WHERE collection = 'theories'
	`).Scan(&theoryID, &theoryContent, &theoryTags, &theoryMeta)
	require.NoError(t, err)
	assert.Contains(t, theoryContent, "Concept drift detected")
	assert.Contains(t, theoryContent, memID)
	assert.Contains(t, theoryContent, "0.92") // peak confidence
	assert.Contains(t, theoryContent, "0.44") // current (approximately — computed from challenges)

	var meta map[string]interface{}
	err = json.Unmarshal(theoryMeta, &meta)
	require.NoError(t, err)
	assert.True(t, meta["concept_drift"].(bool))
	assert.Equal(t, memID, meta["source_artifact"])
	assert.Equal(t, 0.92, meta["peak_confidence"])
	assert.Equal(t, float64(3), meta["lifetime_challenges"])
	assert.Equal(t, float64(2), meta["recent_challenges"])

	// Step 8: Verify tags include concept-drift markers.
	var tags []string
	err = json.Unmarshal(theoryTags, &tags)
	require.NoError(t, err)
	assert.Contains(t, tags, "concept-drift")
	assert.Contains(t, tags, "auto-generated")
}

// TestDetectConceptDriftCycle_SkipsHealthyArtifacts verifies that a memory
// with no or few challenges does NOT produce a drift theory.
func TestDetectConceptDriftCycle_SkipsHealthyArtifacts(t *testing.T) {
	dm := newTestDM(t)
	worker := &IdleConsolidationWorker{db: dm, logger: slog.Default()}

	memID := "healthy-test-1"
	_, err := dm.ExecTracked(`
		INSERT INTO memories (id, collection, content, confidence, deleted_at)
		VALUES (?, 'memories', 'Healthy memory with no challenges', 0.80, NULL)
	`, 0, memID)
	require.NoError(t, err)

	// Add only 1 challenge — below the lifetime >= 3 threshold.
	_, err = dm.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at)
		VALUES (?, ?, 'memory', 'challenge', 'test', -0.6, 'test', ?)
	`, 0, GenerateID(), memID, time.Now().Add(-1*24*time.Hour).Unix())
	require.NoError(t, err)

	worker.DetectConceptDriftCycle()

	var count int
	err = dm.QueryRowTracked(`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "healthy artifact should not trigger a drift theory")
}

// TestDetectConceptDriftCycle_DedupSkipsDuplicateRun verifies that a second
// call to DetectConceptDriftCycle within the same interval does not produce
// a duplicate theory (in-process dedup + SQLite-native dedup).
func TestDetectConceptDriftCycle_DedupSkipsDuplicateRun(t *testing.T) {
	dm := newTestDM(t)
	worker := &IdleConsolidationWorker{db: dm, logger: slog.Default()}

	memID := "dedup-test-1"
	_, err := dm.ExecTracked(`
		INSERT INTO memories (id, collection, content, confidence, deleted_at)
		VALUES (?, 'memories', 'Dedup test memory', ?, NULL)
	`, 0, memID, 0.92)
	require.NoError(t, err)

	// Peak history row.
	_, err = dm.ExecTracked(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES (?, ?, 'memory', 0.92, ?, 1, 'evidence_added')
	`, 0, GenerateID(), memID, time.Now().Add(-30*24*time.Hour).Unix())
	require.NoError(t, err)

	// Add sufficient challenges.
	now := time.Now()
	for _, ts := range []int64{
		now.Add(-3 * 24 * time.Hour).Unix(),
		now.Add(-3 * 24 * time.Hour).Unix(),
		now.Add(-60 * 24 * time.Hour).Unix(),
	} {
		_, err = dm.ExecTracked(`
			INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at)
			VALUES (?, ?, 'memory', 'challenge', 'drift-test', -0.6, 'test', ?)
		`, 0, GenerateID(), memID, ts)
		require.NoError(t, err)
	}

	require.NoError(t, RecomputeConfidence(dm, memID, "memory", RecomputeReasonEvidenceAdded))

	// First run — should produce a theory.
	worker.DetectConceptDriftCycle()

	var count1 int
	err = dm.QueryRowTracked(`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`).Scan(&count1)
	require.NoError(t, err)
	assert.Equal(t, 1, count1)

	// Second run — in-process dedup should suppress.
	worker.DetectConceptDriftCycle()

	var count2 int
	err = dm.QueryRowTracked(`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`).Scan(&count2)
	require.NoError(t, err)
	assert.Equal(t, 1, count2, "second run should not create a duplicate theory")
}
