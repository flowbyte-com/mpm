package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	mpminternal "mpm/internal"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpsConfidence_ParsesShowArgs(t *testing.T) {
	args := []string{"show", "--artifact", "mem-1"}
	payload, cmd, err := parseOpsConfidenceArgs(args)
	require.NoError(t, err)
	assert.Equal(t, "show", cmd)
	assert.Equal(t, "mem-1", payload["artifact_id"])
}

func TestOpsConfidence_ParsesRecomputeArgs(t *testing.T) {
	args := []string{"recompute", "--artifact", "mem-1"}
	_, cmd, err := parseOpsConfidenceArgs(args)
	require.NoError(t, err)
	assert.Equal(t, "recompute", cmd)
}

func TestOpsConfidence_RequiresSubcommand(t *testing.T) {
	_, _, err := parseOpsConfidenceArgs([]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subcommand")
}

func TestOpsConfidence_RejectsUnknownSubcommand(t *testing.T) {
	_, _, err := parseOpsConfidenceArgs([]string{"bogus", "--artifact", "x"})
	require.Error(t, err)
}

// TestGetConfidenceForArtifact_EndToEnd exercises the full path against a
// real temp DB: insert memory, add evidence, get the snapshot, assert.
func TestGetConfidenceForArtifact_EndToEnd(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	dm := mpminternal.NewDatabaseManagerForDB(db)
	require.NoError(t, dm.InitSchema())
	t.Cleanup(func() { dm.Close() })

	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES ('mem-1', 'memories', 'x')`, 0)
	require.NoError(t, err)

	require.NoError(t, mpminternal.AddEvidence(dm, mpminternal.EvidenceInput{
		ArtifactID: "mem-1", ArtifactType: "memory", Type: "test",
		SourceGroup: "x", Strength: 0.7, CreatedBy: "tester", CreatedAt: time.Now(),
	}))

	snap, err := mpminternal.GetConfidenceForArtifact(dm, "mem-1", "memory", 10)
	require.NoError(t, err)
	// Initial confidence for "memory" is 0.8; one positive evidence pushes
	// it modestly upward. The function should return a value in that range
	// (not zero, not unrelated). Tolerance is loose to avoid over-coupling
	// the test to the exact math.
	assert.InDelta(t, 0.8, snap.Confidence, 0.15, "confidence should be near initial after one positive evidence")
	assert.Equal(t, 1, snap.HistoryCount)
	require.Len(t, snap.History, 1)
	assert.Equal(t, "evidence_added", snap.History[0].Trigger)
}
