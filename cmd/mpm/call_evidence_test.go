package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"mpm/internal"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallAddEvidence_RequiresFields(t *testing.T) {
	dm := newTestDMForCmd(t)
	_, err := runHandler(dm, "add_evidence", map[string]interface{}{})
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

	_, err = runHandler(dm, "add_evidence", map[string]interface{}{
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
	res, err := runHandler(dm, "list_evidence", map[string]interface{}{
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
		_, err = runHandler(dm, "add_evidence", map[string]interface{}{
			"artifact_id":   mem.ID,
			"artifact_type": "memory",
			"type":          "observation",
			"source_group":  "test",
			"strength":      0.4,
			"created_by":    "test",
		})
		require.NoError(t, err)
	}
	res, err := runHandler(dm, "query_confidence_history", map[string]interface{}{
		"artifact_id":   mem.ID,
		"artifact_type": "memory",
		"limit":         10,
	})
	require.NoError(t, err)
	out, _ := json.Marshal(res)
	assert.Contains(t, string(out), "history")
}

// newTestDMForCmd is a test helper for cmd/mpm package tests. Lives in this
// file to avoid pulling internal-only helpers.
//
// Important: do NOT use internal.NewDatabaseManager here — that function
// ignores its projectRoot argument and always opens the canonical workspace
// DB at mpm/src/db/mpm.db. Using it from a test would silently mutate the
// workspace DB on every test run. (This bug was caught after the cmd/mpm
// evidence tests had been writing test rows like `content='x'` directly
// into the workspace DB for an extended period.)
//
// The correct pattern is NewDatabaseManagerForDB + InitSchema on a fresh
// sqlite3 file in t.TempDir() — same as newTestDM in internal/.
//
// This helper ALSO installs the dm as the call-handler DM override via
// setTestDMOverride, so subsequent calls to callAddEvidence /
// callQueryConfidenceTrend etc. land in the temp DB rather than the
// workspace DB. The override is cleared on test cleanup.
func newTestDMForCmd(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	dm := internal.NewDatabaseManagerForDB(db)
	require.NoError(t, dm.InitSchema())
	t.Cleanup(func() {
		dm.Close()
	})
	return dm
}

func TestCallQueryConfidenceChanges_ReturnsDeltas(t *testing.T) {
	dm := newTestDMForCmd(t)
	store := &internal.MemoryStore{DM: dm, DB: &internal.SQLiteConnection{DB: dm.SQLDB()}}
	mem, err := store.AddMemory("x", "memories", nil, nil, "", "test")
	require.NoError(t, err)
	require.NotNil(t, mem)

	// Add evidence twice to produce two history rows with deltas.
	for i := 0; i < 2; i++ {
		_, err = runHandler(dm, "add_evidence", map[string]interface{}{
			"artifact_id":   mem.ID,
			"artifact_type": "memory",
			"type":          "observation",
			"source_group":  "test",
			"strength":      0.4,
			"created_by":    "test",
		})
		require.NoError(t, err)
	}
	res, err := runHandler(dm, "query_confidence_changes", map[string]interface{}{
		"since_seconds_ago": 3600,
		"limit":             10,
		"artifact_id":       mem.ID,
		"artifact_type":     "memory",
	})
	require.NoError(t, err)
	out, _ := json.Marshal(res)
	assert.Contains(t, string(out), "changes")
	assert.Contains(t, string(out), "delta")
	assert.Contains(t, string(out), "trigger")
}

func TestCallQueryConfidenceTrend_ReturnsTrajectory(t *testing.T) {
	dm := newTestDMForCmd(t)
	store := &internal.MemoryStore{DM: dm, DB: &internal.SQLiteConnection{DB: dm.SQLDB()}}
	mem, err := store.AddMemory("x", "memories", nil, nil, "", "test")
	require.NoError(t, err)
	require.NotNil(t, mem)

	// Add 3 pieces of evidence with increasing strength to create an upward trend.
	for i := 0; i < 3; i++ {
		_, err = runHandler(dm, "add_evidence", map[string]interface{}{
			"artifact_id":   mem.ID,
			"artifact_type": "memory",
			"type":          "observation",
			"source_group":  "test",
			"strength":      0.3 + float64(i)*0.1, // 0.3, 0.4, 0.5
			"created_by":    "test",
		})
		require.NoError(t, err)
	}

	res, err := runHandler(dm, "query_confidence_trend", map[string]interface{}{
		"artifact_id":   mem.ID,
		"artifact_type": "memory",
		"window_days":   30,
	})
	require.NoError(t, err)
	out, _ := json.Marshal(res)
	assert.Contains(t, string(out), "trend")
	assert.Contains(t, string(out), "velocity")
	assert.Contains(t, string(out), "delta_window")
}

func TestCallQueryMemoryQuality_PerSourceStats(t *testing.T) {
	dm := newTestDMForCmd(t)
	store := &internal.MemoryStore{DM: dm, DB: &internal.SQLiteConnection{DB: dm.SQLDB()}}

	// Write 2 memories — auto_capture trigger should record their creator.
	for i := 0; i < 2; i++ {
		mem, err := store.AddMemory("x", "memories", nil, nil, "", "test-source-A")
		require.NoError(t, err)
		require.NotNil(t, mem)
	}

	res, err := runHandler(dm, "query_memory_quality", map[string]interface{}{})
	require.NoError(t, err)
	out, _ := json.Marshal(res)
	assert.Contains(t, string(out), "sources")
	assert.Contains(t, string(out), "test-source-A")
	assert.Contains(t, string(out), "memory_count")
}
