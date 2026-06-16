package internal

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidenceStore_AddEvidenceAndRecompute(t *testing.T) {
	dm := newTestDM(t)

	// Insert a memory, then add positive evidence.
	memID := "mem-1"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'parser error observed')`, 0, memID)
	require.NoError(t, err)

	err = AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "reproduction",
		SourceGroup:  "test-rig-1",
		Strength:     0.85,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)

	// After evidence: confidence should have moved up from initial 0.8.
	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, memID).Scan(&conf))
	assert.Greater(t, conf, 0.8, "positive evidence should raise confidence above initial 0.8")

	// History should have at least one row.
	var historyCount int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ?`, memID).Scan(&historyCount))
	assert.GreaterOrEqual(t, historyCount, 1)
}

func TestEvidenceStore_NegativeEvidenceLowersConfidence(t *testing.T) {
	dm := newTestDM(t)

	memID := "mem-2"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'uncertain claim')`, 0, memID)
	require.NoError(t, err)

	err = AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "challenge",
		SourceGroup:  "reviewer-1",
		Strength:     -0.6,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)

	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, memID).Scan(&conf))
	assert.Less(t, conf, 0.8, "challenge evidence should lower confidence below initial")
}

func TestEvidenceStore_TriggerReasonRecordedInHistory(t *testing.T) {
	dm := newTestDM(t)

	memID := "mem-3"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)

	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "observation",
		Strength:     0.4,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	}))

	var trigger string
	require.NoError(t, dm.QueryRowTracked(
		`SELECT trigger FROM confidence_history WHERE artifact_id = ? ORDER BY computed_at DESC LIMIT 1`, memID,
	).Scan(&trigger))
	assert.Equal(t, "evidence_added", trigger)
}

func TestEvidenceStore_RecomputeManual(t *testing.T) {
	dm := newTestDM(t)

	memID := "mem-4"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)

	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "test",
		Strength:     0.7,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	}))

	// Manual recompute with the same evidence should produce a history row
	// with trigger='manual_recompute'.
	require.NoError(t, RecomputeConfidence(dm, memID, "memory", RecomputeReasonManual))

	var manualCount int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ? AND trigger = 'manual_recompute'`, memID,
	).Scan(&manualCount))
	assert.GreaterOrEqual(t, manualCount, 1)
}

// newTestDM creates a DatabaseManager on a temp DB and returns it. The
// DatabaseManager is closed via t.Cleanup. Follows the freshDB pattern from
// isolation_test.go so we don't write to the real workspace DB.
func newTestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	dm := NewDatabaseManagerForDB(db)
	require.NoError(t, dm.InitSchema())
	t.Cleanup(func() { dm.Close() })
	return dm
}
