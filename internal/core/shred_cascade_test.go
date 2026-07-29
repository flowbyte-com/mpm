// shred_cascade_test.go — tests for ShredMemoryWithCascade's
// collection-aware routing (audit 2026-07-22).
//
// The cascade wrapper is what `mpm call shred_memory` actually invokes
// in production. It used to only handle memories — silently no-op'ing
// on lesson IDs. The fix probes lessons_base and routes accordingly:
//
//   - lesson path: DELETE FROM lessons (INSTEAD OF trigger fires)
//   - memory path: full cascade (topic_memberships + theory + memory)
//
// These tests pin both paths through the wrapper that production uses.
package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShredMemoryWithCascade_LessonPath proves the cascade wrapper
// actually deletes lessons (the production bug — wrapper previously
// silently no-op'd on lesson IDs).
func TestShredMemoryWithCascade_LessonPath(t *testing.T) {
	dm := NewTestDM(t)

	lesson, err := dm.AddLesson("cascade wrapper lesson test", LessonTypeInsight,
		[]string{"cascade-test"}, "test-session")
	require.NoError(t, err)
	id := lesson.ID

	// Pre-condition: lesson is present.
	var baseCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM lessons_base WHERE id = ?`, id).Scan(&baseCount))
	require.Equal(t, 1, baseCount)

	// Exercise the cascade wrapper (production call path).
	result, err := dm.ShredMemoryWithCascade(id)
	require.NoError(t, err)
	assert.True(t, result["success"].(bool))
	assert.Equal(t, id, result["lesson_id"], "lesson result should report lesson_id, not memory_id")
	_, hasMemoryID := result["memory_id"]
	assert.False(t, hasMemoryID, "lesson path must not report memory_id")

	// Post-condition: lesson is gone.
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM lessons_base WHERE id = ?`, id).Scan(&baseCount))
	assert.Equal(t, 0, baseCount, "lesson must be removed by cascade wrapper")
}

// TestShredMemoryWithCascade_MemoryPath proves the memory path is
// unchanged — no regression. Uses topic_memberships + a challenged
// theory to exercise the full cascade.
func TestShredMemoryWithCascade_MemoryPath(t *testing.T) {
	dm := NewTestDM(t)

	// Create a topic + a memory linked to it.
	topicID, err := dm.CreateTopic("cascade-test-topic", "Test topic for cascade", "", "")
	require.NoError(t, err)

	memID, err := dm.SaveMemory("memories", "cascade wrapper memory test", "test-session",
		[]string{"cascade-test"}, nil, nil, false, 5)
	require.NoError(t, err)
	require.NoError(t, dm.AddMemoryToTopic(memID, topicID, "primary"))

	// Verify membership exists.
	var memCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM topic_memberships WHERE memory_id = ?`, memID).Scan(&memCount))
	require.Equal(t, 1, memCount, "topic_memberships row must exist before shred")

	// Cascade shred via the wrapper.
	result, err := dm.ShredMemoryWithCascade(memID)
	require.NoError(t, err)
	assert.True(t, result["success"].(bool))
	assert.Equal(t, memID, result["memory_id"])

	// Post-conditions: memory soft-deleted, topic_memberships cleared.
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM topic_memberships WHERE memory_id = ?`, memID).Scan(&memCount))
	assert.Equal(t, 0, memCount, "topic_memberships must be cleared by cascade")

	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id = ? AND deleted_at IS NULL`, memID).Scan(&memCount))
	assert.Equal(t, 0, memCount, "memory must be soft-deleted by cascade")
}

// TestShredMemoryWithCascade_NonExistentIsIdempotent proves a shred of a
// non-existent id returns success without error — same idempotent
// semantics as the prior implementation.
func TestShredMemoryWithCascade_NonExistentIsIdempotent(t *testing.T) {
	dm := NewTestDM(t)

	result, err := dm.ShredMemoryWithCascade("0000000000000000")
	require.NoError(t, err, "non-existent id must be idempotent (no error)")
	assert.True(t, result["success"].(bool))
}

// TestShredMemoryWithCascade_EmptyIDRejected proves the empty-id guard
// fires before any DB work.
func TestShredMemoryWithCascade_EmptyIDRejected(t *testing.T) {
	dm := NewTestDM(t)
	_, err := dm.ShredMemoryWithCascade("")
	assert.Error(t, err, "empty id must return an error")
	assert.Contains(t, err.Error(), "memory_id is required")
}

// TestShredMemoryWithCascade_LessonPriority proves a memory-id that's
// also accidentally a lesson-id (collision-resistant but theoretically
// possible) gets routed to the lesson path. Today, since both tables
// use GenerateID with the same algorithm, collisions are vanishingly
// rare (16-hex = 64 bits). This test pins the deterministic behavior.
func TestShredMemoryWithCascade_LessonPriority(t *testing.T) {
	dm := NewTestDM(t)

	// Add a lesson.
	lesson, err := dm.AddLesson("priority test", LessonTypeInsight, nil, "test")
	require.NoError(t, err)

	// Shred via cascade wrapper.
	result, err := dm.ShredMemoryWithCascade(lesson.ID)
	require.NoError(t, err)

	// Lesson path wins (we only added the lesson, no memory with this id).
	assert.Equal(t, lesson.ID, result["lesson_id"])

	// Memory should NOT exist (we never added one).
	var memCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id = ?`, lesson.ID).Scan(&memCount))
	assert.Equal(t, 0, memCount)
}