package internal

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"mpm/internal/config"
	"mpm/internal/synth"
	"github.com/stretchr/testify/require"
)

// openLifecycleTestDB opens an in-memory SQLite database for lifecycle tests.
func openLifecycleTestDB(t *testing.T) *DatabaseManager {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	dm := NewDatabaseManagerForDB(db)
	err = dm.InitSchema()
	require.NoError(t, err)
	return dm
}

// openLifecycleFileDB opens a temp-file SQLite database for integration tests
// that need a real path (e.g. RunLifecycleDecayAndArchival's isolated connection).
func openLifecycleFileDB(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := t.TempDir() + "/lifecycle_test.db"
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	db.Exec("PRAGMA journal_mode = WAL")
	db.Exec("PRAGMA busy_timeout = 5000")
	dm := NewDatabaseManagerForDB(db)
	dm.dbPath = tmp // ensure RunLifecycleDecayAndArchival can open an isolated connection
	err = dm.InitSchema()
	require.NoError(t, err)
	return dm
}

// setUpdatedAt directly updates the updated_at column to a past timestamp.
func setUpdatedAt(t *testing.T, dm *DatabaseManager, id string, pastDays int) {
	t.Helper()
	_, err := dm.SQLDB().Exec(fmt.Sprintf(
		`UPDATE memories SET updated_at = DATETIME('now', '-%d days') WHERE id = ?`,
		pastDays), id)
	require.NoError(t, err)
}

// setMemoryCreatedAt sets the created_at column to simulate memory age.
func setMemoryCreatedAt(t *testing.T, dm *DatabaseManager, id string, pastDays int) {
	t.Helper()
	_, err := dm.SQLDB().Exec(fmt.Sprintf(
		`UPDATE memories SET created_at = DATETIME('now', '-%d days') WHERE id = ?`,
		pastDays), id)
	require.NoError(t, err)
}

func TestLifecycleDecay(t *testing.T) {
	t.Run("TestMemoryWeightDecay", func(t *testing.T) {
		dm := openLifecycleTestDB(t)
		defer dm.Close()

		id, err := dm.SaveMemory("memories", "test decay memory", "sess_1", nil, nil, nil, true, 10)
		require.NoError(t, err)
		require.NotEmpty(t, id)

		setUpdatedAt(t, dm, id, 14)

		policies := map[string]DecayPolicy{
			"memories": {DecayPercent: 1.0, Floor: 1},
		}
		count, err := dm.DecayWeights(policies, 7)
		require.NoError(t, err)
		require.Equal(t, 1, count)

		var newWeight int
		err = dm.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id = ?`, id).Scan(&newWeight)
		require.NoError(t, err)
		require.Equal(t, 9, newWeight, "expected weight 10 -> 9 after 1%% exponential step-down")

		var updatedAt string
		err = dm.SQLDB().QueryRow(`SELECT updated_at FROM memories WHERE id = ?`, id).Scan(&updatedAt)
		require.NoError(t, err)
		require.NotEmpty(t, updatedAt)

		setUpdatedAt(t, dm, id, 14)
		count, err = dm.DecayWeights(policies, 7)
		require.NoError(t, err)
		require.Equal(t, 1, count)

		err = dm.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id = ?`, id).Scan(&newWeight)
		require.NoError(t, err)
		require.GreaterOrEqual(t, newWeight, 1, "weight must never descend below 1")
	})

	t.Run("TestMemoryArchivalAndPruning", func(t *testing.T) {
		dm := openLifecycleTestDB(t)
		defer dm.Close()

		id, err := dm.SaveMemory("memories", "test archival memory", "sess_1", nil, nil, nil, false, 1)
		require.NoError(t, err)
		require.NotEmpty(t, id)

		setUpdatedAt(t, dm, id, 45)
		setMemoryCreatedAt(t, dm, id, 45)

		count, err := dm.ArchiveStaleMemories(30)
		require.NoError(t, err)
		require.Equal(t, 1, count)

		var deletedAt *string
		err = dm.SQLDB().QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id).Scan(&deletedAt)
		require.NoError(t, err)
		require.NotNil(t, deletedAt)
		require.NotEqual(t, "", *deletedAt)

		var revCount int
		err = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memory_revisions WHERE memory_id = ?`, id).Scan(&revCount)
		require.NoError(t, err)
		require.GreaterOrEqual(t, revCount, 1)

		var revContent string
		err = dm.SQLDB().QueryRow(`SELECT content FROM memory_revisions WHERE memory_id = ? ORDER BY version DESC LIMIT 1`, id).Scan(&revContent)
		require.NoError(t, err)
		require.Equal(t, "test archival memory", revContent)
	})

	t.Run("TestMemoryStaysBelowArchivalThreshold", func(t *testing.T) {
		dm := openLifecycleTestDB(t)
		defer dm.Close()

		id, err := dm.SaveMemory("memories", "keep this memory", "sess_1", nil, nil, nil, false, 2)
		require.NoError(t, err)

		setUpdatedAt(t, dm, id, 45)
		setMemoryCreatedAt(t, dm, id, 45)

		count, err := dm.ArchiveStaleMemories(30)
		require.NoError(t, err)
		require.Equal(t, 0, count)

		var deletedAt *string
		err = dm.SQLDB().QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id).Scan(&deletedAt)
		require.NoError(t, err)
		require.Nil(t, deletedAt)
	})

	t.Run("TestDecaySkipsNonLTMMemories", func(t *testing.T) {
		dm := openLifecycleTestDB(t)
		defer dm.Close()

		id, err := dm.SaveMemory("memories", "non-ltm decay", "sess_1", nil, nil, nil, false, 10)
		require.NoError(t, err)

		setUpdatedAt(t, dm, id, 14)

		policies := map[string]DecayPolicy{
			"memories": {DecayPercent: 1.0, Floor: 1},
		}
		count, err := dm.DecayWeights(policies, 7)
		require.NoError(t, err)
		require.Equal(t, 0, count)

		var weight int
		err = dm.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id = ?`, id).Scan(&weight)
		require.NoError(t, err)
		require.Equal(t, 10, weight)
	})

	t.Run("TestRunLifecycleDecayAndArchivalIntegration", func(t *testing.T) {
		dm := openLifecycleFileDB(t)
		defer dm.Close()

		ltmID, err := dm.SaveMemory("memories", "ltm decay candidate", "sess_1", nil, nil, nil, true, 10)
		require.NoError(t, err)

		archID, err := dm.SaveMemory("memories", "archival candidate", "sess_1", nil, nil, nil, false, 1)
		require.NoError(t, err)

		setUpdatedAt(t, dm, ltmID, 14)
		setUpdatedAt(t, dm, archID, 45)
		setMemoryCreatedAt(t, dm, archID, 45)

		err = dm.RunLifecycleDecayAndArchival(1.0, 30)
		require.NoError(t, err)

		var ltmWeight int
		err = dm.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id = ?`, ltmID).Scan(&ltmWeight)
		require.NoError(t, err)
		require.Less(t, ltmWeight, 10)

		var deletedAt *string
		err = dm.SQLDB().QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, archID).Scan(&deletedAt)
		require.NoError(t, err)
		require.NotNil(t, deletedAt)
		require.NotEmpty(t, *deletedAt)

		err = dm.RunLifecycleDecayAndArchival(1.0, 30)
		require.NoError(t, err)

		err = dm.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id = ?`, ltmID).Scan(&ltmWeight)
		require.NoError(t, err)
		require.GreaterOrEqual(t, ltmWeight, 1)
	})
}

func TestLifecycleDecayHeartbeatWiring(t *testing.T) {
	dm := openLifecycleFileDB(t)
	defer dm.Close()

	synth := &mockSynthClient{succeedResult: &synth.SynthResult{Content: "mock"}}
	dlqTick := make(chan time.Time, 5)
	worker := NewSynthesisWorkerForTest(dm, synth, 2, dlqTick, []config.SynthVendor{
		{Name: "mock", Model: "mock", APIKey: "test", BaseURL: "http://localhost:0"},
	})
	worker.Start()

	id, err := dm.SaveMemory("memories", "heartbeat decay", "sess_1", nil, nil, nil, true, 10)
	require.NoError(t, err)
	setUpdatedAt(t, dm, id, 14)

	dlqTick <- time.Now()
	time.Sleep(1000 * time.Millisecond)

	var weight int
	err = dm.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id = ?`, id).Scan(&weight)
	require.NoError(t, err)
	require.Less(t, weight, 10)
	require.GreaterOrEqual(t, weight, 1)

	worker.Stop()
}
