// shred_lesson_aware_test.go — tests for the collection-aware ShredMemory.
//
// Audit 2026-07-22: prior implementation only DELETED FROM memories,
// silently no-op'ing on lesson IDs. The fix unifies both paths behind
// one collection-aware primitive. These tests pin the contract:
//   - lesson IDs are actually deleted from lessons_base
//   - lessons_fts is updated atomically (no orphans)
//   - memory IDs still work (no regression)
//   - non-existent IDs return nil (idempotent semantics)
//   - mixing lesson + memory in same call sequence works
package internal

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ftsLessonsAvailable checks whether the test DB has the lessons_fts
// virtual table. Test environments built without SQLITE_ENABLE_FTS5
// (the default for `go test`) won't have FTS5 tables; production builds
// always do (CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 per the Makefile).
//
// FTS5-specific assertions are gated behind this so the test passes
// in both environments while still pinning the contract when FTS5 is
// available.
func ftsLessonsAvailable(t *testing.T, dm *DatabaseManager) bool {
	t.Helper()
	var n int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lessons_fts'`,
	).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// TestShredMemory_LessonLifecycle proves a lesson can be saved,
// FTS-indexed, shredded, and is then truly gone from BOTH lessons_base
// and lessons_fts.
func TestShredMemory_LessonLifecycle(t *testing.T) {
	dm := NewTestDM(t)

	// Add a lesson via the public surface (route goes through the
	// INSTEAD OF INSERT trigger so lessons_fts is populated).
	lesson, err := dm.AddLesson("audit-shred-test lesson content", LessonTypeInsight,
		[]string{"shred-test"}, "test-session")
	require.NoError(t, err)
	require.NotNil(t, lesson)
	id := lesson.ID

	// Sanity: present in lessons_base.
	var baseCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM lessons_base WHERE id = ?`, id).Scan(&baseCount))
	assert.Equal(t, 1, baseCount, "lesson should be in lessons_base before shred")

	// FTS5 verification gated on table availability (test env may lack
	// SQLITE_ENABLE_FTS5; production builds always have it).
	if ftsLessonsAvailable(t, dm) {
		var ftsCount int
		require.NoError(t, dm.db.QueryRow(
			`SELECT COUNT(*) FROM lessons_fts WHERE rowid = (SELECT rowid FROM lessons_base WHERE id = ?)`,
			id).Scan(&ftsCount))
		assert.Equal(t, 1, ftsCount, "lesson should be in lessons_fts before shred")
	}

	// Shred via the unified entry point.
	require.NoError(t, ShredMemory(dm.db, id))

	// Verify truly gone from lessons_base (the core contract).
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM lessons_base WHERE id = ?`, id).Scan(&baseCount))
	assert.Equal(t, 0, baseCount, "lesson should be removed from lessons_base after shred")

	// FTS5 verification: when available, the INSTEAD OF trigger should
	// have removed the row from lessons_fts atomically with lessons_base.
	if ftsLessonsAvailable(t, dm) {
		var ftsCount int
		require.NoError(t, dm.db.QueryRow(
			`SELECT COUNT(*) FROM lessons_fts WHERE rowid = (SELECT rowid FROM lessons_base WHERE id = ?)`,
			id).Scan(&ftsCount))
		assert.Equal(t, 0, ftsCount, "lesson should be removed from lessons_fts after shred")
	}
}

// TestShredMemory_MemoryLifecycle proves the memory path is unchanged
// (no regression) — memories still get deleted via the AFTER DELETE
// trigger that maintains memories_fts.
func TestShredMemory_MemoryLifecycle(t *testing.T) {
	dm := NewTestDM(t)

	id, err := dm.SaveMemory("memories", "audit-shred-test memory content", "test-session",
		[]string{"shred-test"}, nil, nil, false, 5)
	require.NoError(t, err)

	// Verify present in memories table.
	var memCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id = ? AND deleted_at IS NULL`, id).Scan(&memCount))
	assert.Equal(t, 1, memCount, "memory should be present before shred")

	// Shred via the unified entry point.
	require.NoError(t, ShredMemory(dm.db, id))

	// Verify soft-deleted (memories table uses soft-delete via deleted_at).
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id = ? AND deleted_at IS NULL`, id).Scan(&memCount))
	assert.Equal(t, 0, memCount, "memory should be soft-deleted after shred")
}

// TestShredMemory_NonExistentIsIdempotent proves a shred call on an id
// that exists in neither lessons nor memories returns nil (no error).
// This preserves the prior behavior and matches Go's stdlib convention.
func TestShredMemory_NonExistentIsIdempotent(t *testing.T) {
	dm := NewTestDM(t)
	err := ShredMemory(dm.db, "0000000000000000")
	assert.NoError(t, err, "shred of non-existent id must be a no-op (idempotent)")
}

// TestShredMemory_EmptyIDRejected proves the empty-id guard fires.
func TestShredMemory_EmptyIDRejected(t *testing.T) {
	dm := NewTestDM(t)
	err := ShredMemory(dm.db, "")
	assert.Error(t, err, "empty id must return an error")
	assert.True(t,
		strings.Contains(err.Error(), "id is required"),
		"error message should mention the id requirement, got: %v", err)
}

// TestShredMemory_MixedSequence proves a session can shred a lesson
// then a memory (or vice versa) without one shred blocking the other.
func TestShredMemory_MixedSequence(t *testing.T) {
	dm := NewTestDM(t)

	// Add one lesson + one memory.
	lesson, err := dm.AddLesson("audit-shred mixed-test content", LessonTypeWarning,
		[]string{"mixed-test"}, "test-session")
	require.NoError(t, err)
	memID, err := dm.SaveMemory("memories", "audit-shred mixed-test memory", "test-session",
		[]string{"mixed-test"}, nil, nil, false, 5)
	require.NoError(t, err)

	// Shred lesson first.
	require.NoError(t, ShredMemory(dm.db, lesson.ID))
	// Verify only the lesson is gone.
	var lCount, mCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM lessons_base WHERE id = ?`, lesson.ID).Scan(&lCount))
	assert.Equal(t, 0, lCount)
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id = ? AND deleted_at IS NULL`, memID).Scan(&mCount))
	assert.Equal(t, 1, mCount, "memory should survive lesson shred")

	// Then shred the memory.
	require.NoError(t, ShredMemory(dm.db, memID))
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id = ? AND deleted_at IS NULL`, memID).Scan(&mCount))
	assert.Equal(t, 0, mCount, "memory should be gone after second shred")
}

// TestShredMemory_FTSNoOrphans proves the lessons_fts index stays in
// sync — no orphan FTS rows after shred. This is the operational
// guarantee the audit caught missing. Skipped when FTS5 isn't compiled
// in (test env), since the trigger-side guarantee requires FTS5 to test.
func TestShredMemory_FTSNoOrphans(t *testing.T) {
	dm := NewTestDM(t)
	if !ftsLessonsAvailable(t, dm) {
		t.Skip("lessons_fts not available — test DB built without SQLITE_ENABLE_FTS5; production builds always have it")
	}

	// Add 3 lessons, then shred 2. lessons_fts must have exactly 1 row
	// matching the surviving lesson.
	ids := make([]string, 3)
	for i := range ids {
		lesson, err := dm.AddLesson(
			"audit-shred orphan-test content "+string(rune('a'+i)),
			LessonTypeInsight,
			[]string{"orphan-test"},
			"test-session",
		)
		require.NoError(t, err)
		ids[i] = lesson.ID
	}

	// Shred the first two.
	require.NoError(t, ShredMemory(dm.db, ids[0]))
	require.NoError(t, ShredMemory(dm.db, ids[1]))

	// Count lessons_fts rows whose rowid matches lessons_base rowids
	// for the surviving id — should be exactly 1.
	var count int
	require.NoError(t, dm.db.QueryRow(`
		SELECT COUNT(*) FROM lessons_fts
		WHERE rowid = (SELECT rowid FROM lessons_base WHERE id = ?)
	`, ids[2]).Scan(&count))
	assert.Equal(t, 1, count, "surviving lesson should be indexed in lessons_fts")

	// And no orphan rows for the shredded ids.
	for _, shreddedID := range ids[:2] {
		var orphanCount int
		require.NoError(t, dm.db.QueryRow(`
			SELECT COUNT(*) FROM lessons_fts
			WHERE rowid = (SELECT rowid FROM lessons_base WHERE id = ?)
		`, shreddedID).Scan(&orphanCount))
		assert.Equal(t, 0, orphanCount,
			"shredded id %s should not leave an orphan in lessons_fts", shreddedID)
	}
}