package main

import (
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidenceList_ParsesArgs(t *testing.T) {
	args := []string{"--artifact", "mem-1"}
	payload, err := parseEvidenceListArgs(args)
	require.NoError(t, err)
	assert.Equal(t, "mem-1", payload["artifact_id"])
}

// TestEvidenceList_BareArgs_ParseSuccessfully pins the 2026-09-10
// T45 regression repair: bare `mpm evidence list` (no --artifact)
// is now valid. The parser returns an empty `artifact_id` and the
// handler dispatches to ListEvidence (unfiltered). Previously this
// returned `--artifact is required`; the smoke probe flagged that
// as a regression and it has been restored.
func TestEvidenceList_BareArgs_ParseSuccessfully(t *testing.T) {
	payload, err := parseEvidenceListArgs([]string{})
	require.NoError(t, err)
	assert.Equal(t, "", payload["artifact_id"])
}

// TestEvidenceList_EndToEnd exercises the full path: in-memory DatabaseManager
// via internal.NewTestDM, add evidence, list it, assert the row comes back.
// Closes the production-path coverage gap.
func TestEvidenceList_EndToEnd(t *testing.T) {
	dm := mpminternal.NewTestDM(t)

	// Insert a memory to attach evidence to.
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES ('mem-1', 'memories', 'x')`, 0)
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
