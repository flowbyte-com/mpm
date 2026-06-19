package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallHelpers_AddEvidence_HappyPath(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'parser error observed')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:   "mem-1",
		ArtifactType: "memory",
		Type:         "reproduction",
		SourceGroup:  "test-rig-1",
		Strength:     0.85,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)
	assert.Contains(t, out, "success")
	assert.Equal(t, true, out["success"])
	assert.Contains(t, out, "confidence")
	conf, ok := out["confidence"].(float64)
	require.True(t, ok)
	assert.Greater(t, conf, 0.5, "positive evidence should raise confidence above initial 0.8")

	// Evidence row should exist.
	var n int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM evidence WHERE artifact_id = ?`, "mem-1").Scan(&n))
	assert.Equal(t, 1, n)
}

func TestCallHelpers_AddEvidence_RejectsInvalidType(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-2")
	require.NoError(t, err)

	_, err = dm.AddEvidence(EvidenceInput{
		ArtifactID:   "mem-2",
		ArtifactType: "memory",
		Type:         "bogus_type",
		SourceGroup:  "test",
		CreatedBy:    "test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid evidence type")
}

func TestCallHelpers_AddEvidence_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:   "",
		ArtifactType: "memory",
		Type:         "reproduction",
		SourceGroup:  "test",
		CreatedBy:    "test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}

func TestCallHelpers_ListEvidence_EmptyArtifact(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.ListEvidence("mem-1", "memory")
	require.NoError(t, err)
	assert.Contains(t, out, "evidence")
	items, ok := out["evidence"].([]map[string]interface{})
	require.True(t, ok)
	assert.Empty(t, items)
}

func TestCallHelpers_ListEvidence_FiltersByType(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO lessons (id, type, content, created) VALUES (?, 'insight', 'y', STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))`, 0, "les-1")
	require.NoError(t, err)
	require.NoError(t, AddEvidence(dm, EvidenceInput{ArtifactID: "mem-1", ArtifactType: "memory", Type: "observation", SourceGroup: "g", Strength: 0.4, CreatedBy: "t", CreatedAt: time.Now()}))
	require.NoError(t, AddEvidence(dm, EvidenceInput{ArtifactID: "les-1", ArtifactType: "lesson", Type: "observation", SourceGroup: "g", Strength: 0.4, CreatedBy: "t", CreatedAt: time.Now()}))

	memItems, err := dm.ListEvidence("mem-1", "memory")
	require.NoError(t, err)
	items, _ := memItems["evidence"].([]map[string]interface{})
	assert.Len(t, items, 1)
	assert.Equal(t, "mem-1", items[0]["artifact_id"])
}

func TestCallHelpers_ListEvidence_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ListEvidence("", "memory")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}
