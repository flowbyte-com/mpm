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
