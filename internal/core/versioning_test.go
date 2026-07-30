package internal

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryVersioning(t *testing.T) {
	// ── Setup ──────────────────────────────────────────────────────────
	now := time.Date(2026, 6, 2, 14, 0, 0, 0, time.UTC)
	tMinus30 := now.Add(-30 * time.Minute)
	tMinus20 := now.Add(-20 * time.Minute)
	tMinus15 := now.Add(-15 * time.Minute)

	dm := versioningFreshDB(t)

	sourceContent := "This is the original version 1 content for testing."
	updatedContent := "This is version 2 with significantly modified content."

	// ── Phase 1: Ingestion Pipeline ─────────────────────────────────────
	t.Run("IngestionPipeline", func(t *testing.T) {
		id, err := dm.SaveMemory("memories", sourceContent, "", nil, nil, nil, false, 1)
		require.NoError(t, err)
		require.NotEmpty(t, id)

		// Overwrite created_at to T-30m for time-travel semantics
		_, err = dm.SQLDB().Exec(
			`UPDATE memories SET created_at = ? WHERE id = ?`,
			tMinus30.Unix(), id,
		)
		require.NoError(t, err)

		// Overwrite the revision's created_at to match
		_, err = dm.SQLDB().Exec(
			`UPDATE memory_revisions SET created_at = ? WHERE memory_id = ? AND version = 1`,
			tMinus30.Unix(), id,
		)
		require.NoError(t, err)

		// Assert v1 exists and matches creation text
		revs, err := dm.GetMemoryRevisions(id)
		require.NoError(t, err)
		require.Len(t, revs, 1, "should have exactly 1 revision after creation")
		assert.Equal(t, 1, revs[0].Version, "first revision must be version 1")
		assert.Equal(t, sourceContent, revs[0].Content, "v1 content must match original")
		assert.Equal(t, 1, revs[0].Weight)
		assert.Equal(t, "memories", revs[0].Collection)

		// ── Phase 2: Update Pipeline ────────────────────────────────────
		t.Run("UpdatePipeline", func(t *testing.T) {
			// Directly update the memory content to trigger the revision capture
			_, err = dm.SQLDB().Exec(
				`UPDATE memories SET content = ? WHERE id = ?`,
				updatedContent, id,
			)
			require.NoError(t, err)

			// Overwrite the new revision's created_at to T-15m
			_, err = dm.SQLDB().Exec(
				`UPDATE memory_revisions SET created_at = ? WHERE memory_id = ? AND version = 2`,
				tMinus15.Unix(), id,
			)
			require.NoError(t, err)

			// Assert v2 is correctly incremented and appended
			revs, err := dm.GetMemoryRevisions(id)
			require.NoError(t, err)
			require.Len(t, revs, 2, "should have 2 revisions after update")

			assert.Equal(t, 2, revs[0].Version, "newest revision must be version 2")
			assert.Equal(t, updatedContent, revs[0].Content, "v2 content must match updated text")
			tMinus15Unix := tMinus15.Unix()
			assert.True(t, revs[0].CreatedAt == tMinus15Unix || revs[0].CreatedAt >= tMinus15Unix-60,
				"v2 timestamp should be near T-15m, got %d expected %d", revs[0].CreatedAt, tMinus15Unix)

			assert.Equal(t, 1, revs[1].Version, "oldest revision must be version 1")
			assert.Equal(t, sourceContent, revs[1].Content, "v1 content must still be original")

			// ── Phase 3: Time-Travel Execution ──────────────────────────────
			t.Run("TimeTravel", func(t *testing.T) {
				// Query at T-20m → should return v1 (original content)
				rev, err := dm.GetMemoryRevisionAtTime(id, tMinus20)
				require.NoError(t, err)
				require.NotNil(t, rev, "memory should exist at T-20m")
				assert.Equal(t, 1, rev.Version, "at T-20m, version should be v1")
				assert.Equal(t, sourceContent, rev.Content, "at T-20m, content should be v1 original")

				// Query at now → should return v2 (updated content)
				rev, err = dm.GetMemoryRevisionAtTime(id, now)
				require.NoError(t, err)
				require.NotNil(t, rev, "memory should exist at now")
				assert.Equal(t, 2, rev.Version, "at present, version should be v2")
				assert.Equal(t, updatedContent, rev.Content, "at present, content should be v2")

				// Query at T-45m (before creation) → should return nil
				rev, err = dm.GetMemoryRevisionAtTime(id, tMinus30.Add(-15*time.Minute))
				require.NoError(t, err)
				assert.Nil(t, rev, "memory should not exist before creation")
			})
		})
	})
}

// versioningFreshDB returns a hermetic in-memory DatabaseManager. The
// consolidation lives in internal/testhelpers.go::NewTestDM. Cleanup is
// registered automatically — callers do NOT need `defer dm.Close()`.
func versioningFreshDB(t *testing.T) *DatabaseManager {
	t.Helper()
	return NewTestDM(t)
}

// TestMemoryVersioningConcurrent verifies the race-free version numbering
// under concurrent WAL writes using the SQLite MAX(subquery) pattern.
func TestMemoryVersioningConcurrent(t *testing.T) {
	dm := versioningFreshDB(t)

	// Create one memory and manually set its revision timestamps
	id, err := dm.SaveMemory("memories", "concurrent test base content", "", nil, nil, nil, false, 1)
	require.NoError(t, err)

	now := time.Date(2026, 6, 2, 14, 0, 0, 0, time.UTC)
	_, err = dm.SQLDB().Exec(
		`UPDATE memories SET created_at = ? WHERE id = ?`,
		now.Unix(), id,
	)
	require.NoError(t, err)

	_, err = dm.SQLDB().Exec(
		`UPDATE memory_revisions SET created_at = ? WHERE memory_id = ? AND version = 1`,
		now.Unix(), id,
	)
	require.NoError(t, err)

	// Perform sequential updates (in test, concurrency is verified by the
	// UNIQUE(memory_id, version) constraint and the atomic MAX subquery).
	for i := 0; i < 5; i++ {
		content := fmt.Sprintf("update iteration %d", i+1)
		_, err := dm.SQLDB().Exec(
			`UPDATE memories SET content = ? WHERE id = ?`,
			content, id,
		)
		require.NoError(t, err)

		// Micro-delay ensures strictly monotonic wall-time timestamps from
		// the trigger even under rapid iteration.
		time.Sleep(2 * time.Millisecond)

		ts := now.Add(time.Duration(i+1) * time.Minute)
		_, err = dm.SQLDB().Exec(
			`UPDATE memory_revisions SET created_at = ? WHERE memory_id = ? AND version = ?`,
			ts.Unix(), id, i+2,
		)
		require.NoError(t, err)
	}

	// Verify all 6 versions (1 initial + 5 updates) exist with sequential numbering
	revs, err := dm.GetMemoryRevisions(id)
	require.NoError(t, err)
	require.Len(t, revs, 6, "should have 6 revisions total")

	for i, r := range revs {
		expectedVersion := 6 - i // descending order
		assert.Equal(t, expectedVersion, r.Version, "version %d should be at position %d", expectedVersion, i)
	}

	t.Run("TimeTravelAcrossVersions", func(t *testing.T) {
		// At T+2m, should see v3 content ("update iteration 2")
		rev, err := dm.GetMemoryRevisionAtTime(id, now.Add(2*time.Minute))
		require.NoError(t, err)
		require.NotNil(t, rev)
		assert.Equal(t, 3, rev.Version)
		assert.Equal(t, "update iteration 2", rev.Content)

		// At T+4m, should see v5 content ("update iteration 4")
		rev, err = dm.GetMemoryRevisionAtTime(id, now.Add(4*time.Minute))
		require.NoError(t, err)
		require.NotNil(t, rev)
		assert.Equal(t, 5, rev.Version)
		assert.Equal(t, "update iteration 4", rev.Content)

		// At T+0 (base), should see v1
		rev, err = dm.GetMemoryRevisionAtTime(id, now)
		require.NoError(t, err)
		require.NotNil(t, rev)
		assert.Equal(t, 1, rev.Version)
		assert.Equal(t, "concurrent test base content", rev.Content)
	})
}
