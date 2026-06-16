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

func TestEvidenceList_ParsesArgs(t *testing.T) {
	args := []string{"--artifact", "mem-1"}
	payload, err := parseEvidenceListArgs(args)
	require.NoError(t, err)
	assert.Equal(t, "mem-1", payload["artifact_id"])
}

func TestEvidenceList_RequiresArtifact(t *testing.T) {
	_, err := parseEvidenceListArgs([]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--artifact")
}

// TestEvidenceList_EndToEnd exercises the full path: real temp DB, add evidence,
// list it, assert the row comes back. Closes the production-path coverage gap.
func TestEvidenceList_EndToEnd(t *testing.T) {
	// Build a real DatabaseManager on a temp file.
	tmp := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	dm := mpminternal.NewDatabaseManagerForDB(db)
	require.NoError(t, dm.InitSchema())
	t.Cleanup(func() { dm.Close() })

	// Insert a memory to attach evidence to.
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES ('mem-1', 'memories', 'x')`, 0)
	require.NoError(t, err)

	// Add two pieces of evidence.
	require.NoError(t, mpminternal.AddEvidence(dm, mpminternal.EvidenceInput{
		ArtifactID: "mem-1", ArtifactType: "memory", Type: "observation",
		SourceGroup: "test", Strength: 0.4, CreatedBy: "tester", CreatedAt: time.Now(),
	}))
	require.NoError(t, mpminternal.AddEvidence(dm, mpminternal.EvidenceInput{
		ArtifactID: "mem-1", ArtifactType: "memory", Type: "test",
		SourceGroup: "test", Strength: 0.7, CreatedBy: "tester", CreatedAt: time.Now(),
	}))

	// List and assert.
	rows, err := mpminternal.ListEvidenceForArtifact(dm, "mem-1", "memory")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	// Ordered by created_at asc; we just check both types are present.
	types := []string{rows[0].Type, rows[1].Type}
	assert.Contains(t, types, "observation")
	assert.Contains(t, types, "test")
}
