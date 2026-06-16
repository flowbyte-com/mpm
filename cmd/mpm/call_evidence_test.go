package main

import (
	"encoding/json"
	"testing"

	"mpm/internal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallAddEvidence_RequiresFields(t *testing.T) {
	_, err := callAddEvidence(map[string]interface{}{})
	require.Error(t, err)
}

func TestCallAddEvidence_RoutesToStore(t *testing.T) {
	// Set up a DatabaseManager, insert a memory via the production path
	// (MemoryStore.AddMemory) so the initial confidence is set, then call
	// callAddEvidence and verify confidence moved.
	dm := newTestDMForCmd(t)
	store := &internal.MemoryStore{DM: dm, DB: &internal.SQLiteConnection{DB: dm.SQLDB()}}
	mem, err := store.AddMemory("x", "memories", nil, nil, "", "test")
	require.NoError(t, err)
	require.NotNil(t, mem)

	_, err = callAddEvidence(map[string]interface{}{
		"artifact_id":   mem.ID,
		"artifact_type": "memory",
		"type":          "reproduction",
		"source_group":  "test",
		"strength":      0.85,
		"created_by":    "test",
	})
	require.NoError(t, err)

	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, mem.ID).Scan(&conf))
	assert.Greater(t, conf, 0.8, "adding positive evidence should raise confidence above initial 0.8")
}

func TestCallListEvidence_EmptyResultIsObject(t *testing.T) {
	dm := newTestDMForCmd(t)
	_ = dm // helper exists to mirror the other tests' setup; not strictly needed here
	res, err := callListEvidence(map[string]interface{}{
		"artifact_id":   "nonexistent",
		"artifact_type": "memory",
	})
	require.NoError(t, err)
	// res should be a map with an "evidence" key (possibly empty list)
	out, _ := json.Marshal(res)
	assert.Contains(t, string(out), "evidence")
}

func TestCallQueryConfidenceHistory_ReturnsTimeline(t *testing.T) {
	dm := newTestDMForCmd(t)
	// Use the production AddMemory path so the memory has its initial confidence.
	store := &internal.MemoryStore{DM: dm, DB: &internal.SQLiteConnection{DB: dm.SQLDB()}}
	mem, err := store.AddMemory("x", "memories", nil, nil, "", "test")
	require.NoError(t, err)
	require.NotNil(t, mem)

	// Add evidence twice to produce two history rows.
	for i := 0; i < 2; i++ {
		_, err = callAddEvidence(map[string]interface{}{
			"artifact_id":   mem.ID,
			"artifact_type": "memory",
			"type":          "observation",
			"source_group":  "test",
			"strength":      0.4,
			"created_by":    "test",
		})
		require.NoError(t, err)
	}
	res, err := callQueryConfidenceHistory(map[string]interface{}{
		"artifact_id":   mem.ID,
		"artifact_type": "memory",
		"limit":         10,
	})
	require.NoError(t, err)
	out, _ := json.Marshal(res)
	assert.Contains(t, string(out), "history")
}

// newTestDMForCmd is a test helper for cmd/mpm package tests. Lives in this
// file to avoid pulling internal-only helpers. Opens a fresh DatabaseManager
// on a tmpdir DB (does NOT touch the workspace DB).
func newTestDMForCmd(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	tmpDir := t.TempDir()
	dm, err := internal.NewDatabaseManager(tmpDir + "/test.db")
	require.NoError(t, err)
	t.Cleanup(func() { dm.Close() })
	return dm
}
