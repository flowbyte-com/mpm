// t64_why_resolution_test.go — T64 regression for `mpm why <id>`
// artifact resolution across every supported kind.
//
// Audit finding T64: `mpm why <lesson-id>` regressed when the schema
// migration moved lesson rows from the `lessons` collection of the
// `memories` table into a dedicated `lessons_base` table (via
// migrateLessonsToView + INSTEAD OF triggers). The pre-fix
// service_why.detectKind only probed `memories WHERE collection=?`,
// so every lesson id returned "no artifact found ... probed 9
// standard collections + works" even though the row was trivially
// resolvable in lessons_base.
//
// A related regression: service_why.fetchWork declared `verification`
// and `session_id` as raw `string`, so a work with NULL verification
// (a perfectly legal state) failed Scan with
// `converting NULL to string is unsupported`, and detectKind's
// `continue` swallowed the error as a "not a work" miss — same
// operator-visible symptom ("no artifact found") for a different
// underlying cause.
//
// The fix: lessons_base direct probe via fetchLesson, and
// sql.NullString for nullable work columns. This test pins both.

package main

import (
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestT64_WhyResolvesMemoryArtifact pins the round-5 happy path —
// a memory-collection id must still resolve as kind=memory.
func TestT64_WhyResolvesMemoryArtifact(t *testing.T) {
	dm := newTestDMForCmd(t)

	// AddMemory uses the memory store path with the production schema
	// and provenance stamping. The returned id is what an operator
	// would paste into `mpm why <id>`.
	mem, err := (&mpminternal.MemoryStore{DM: dm, DB: &mpminternal.SQLiteConnection{DB: dm.SQLDB()}}).
		AddMemory("t64 memory content", "memories", nil, nil, "", "test")
	require.NoError(t, err)
	require.NotNil(t, mem)

	svc := NewWhyService(dm)
	require.NotNil(t, svc)
	report, err := svc.Explain(mem.ID)
	require.NoError(t, err)
	assert.Empty(t, report.SkipReason, "memory artifact must resolve")
	assert.Equal(t, "memory", report.ArtifactKind)
}

// TestT64_WhyResolvesLessonArtifact is the core T64 regression. The
// schema's migrateLessonsToView migration moved lessons into a
// dedicated `lessons_base` table; the pre-fix why probe never looked
// there. This test inserts directly into lessons_base and asserts the
// probe resolves.
func TestT64_WhyResolvesLessonArtifact(t *testing.T) {
	dm := newTestDMForCmd(t)

	_, err := dm.SQLDB().Exec(`
		INSERT INTO lessons_base (id, type, content, tags, reinforcement_count, created,
		                          retrieval_priority, importance, confidence)
		VALUES ('t64-lesson-id', 'insight',
		        'T64 regression: lesson content in dedicated table',
		        '["t64","regression"]', 1, '2026-09-11T00:00:00Z',
		        0.5, 0.5, 0.7)
	`)
	require.NoError(t, err)

	svc := NewWhyService(dm)
	require.NotNil(t, svc)
	report, err := svc.Explain("t64-lesson-id")
	require.NoError(t, err)
	assert.Empty(t, report.SkipReason, "lesson artifact must resolve")
	assert.Equal(t, "lesson", report.ArtifactKind,
		"lesson id must surface kind=lesson (not 'no artifact found')")
	require.NotNil(t, report.Identity)
	assert.Contains(t, report.Identity.Content,
		"T64 regression: lesson content in dedicated table")
}

// TestT64_WhyResolvesDecisionArtifact pins the decision-collection
// memory path. Decisions live in `memories` with collection='decisions',
// so the existing memories-table probe finds them.
func TestT64_WhyResolvesDecisionArtifact(t *testing.T) {
	dm := newTestDMForCmd(t)

	mem, err := (&mpminternal.MemoryStore{DM: dm, DB: &mpminternal.SQLiteConnection{DB: dm.SQLDB()}}).
		AddMemory("t64 decision content", "decisions", nil, nil, "", "test")
	require.NoError(t, err)
	require.NotNil(t, mem)

	svc := NewWhyService(dm)
	require.NotNil(t, svc)
	report, err := svc.Explain(mem.ID)
	require.NoError(t, err)
	assert.Empty(t, report.SkipReason)
	assert.Equal(t, "decision", report.ArtifactKind)
}

// TestT64_WhyResolvesTheoryArtifact mirrors the decision test for
// the theories collection.
func TestT64_WhyResolvesTheoryArtifact(t *testing.T) {
	dm := newTestDMForCmd(t)

	mem, err := (&mpminternal.MemoryStore{DM: dm, DB: &mpminternal.SQLiteConnection{DB: dm.SQLDB()}}).
		AddMemory("t64 theory content", "theories", nil, nil, "", "test")
	require.NoError(t, err)
	require.NotNil(t, mem)

	svc := NewWhyService(dm)
	require.NotNil(t, svc)
	report, err := svc.Explain(mem.ID)
	require.NoError(t, err)
	assert.Empty(t, report.SkipReason)
	assert.Equal(t, "theory", report.ArtifactKind)
}

// TestT64_WhyResolvesWorkArtifactWithNullSessionID is the
// fetchWork null-safety regression. The pre-fix scan declared
// `verification` and `session_id` as raw `string`, which failed
// Scan with `converting NULL to string is unsupported` when
// either column was NULL — and detectKind's `continue` swallowed
// that as a probe miss, surfacing the same "no artifact found"
// the operator sees for legitimate non-works.
//
// The current canonical schema makes `verification` non-null with a
// default of 'unverified', but `session_id` remains nullable for
// works that haven't been linked to a session. This test exercises
// the session_id null path (which is the realistic case) and
// asserts the why probe resolves. The verification=null path is
// pinned by the same scan logic — sql.NullString accepts both
// — so one regression test covers both columns.
func TestT64_WhyResolvesWorkArtifactWithNullSessionID(t *testing.T) {
	dm := newTestDMForCmd(t)

	// session_id is omitted entirely → stored as NULL.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO works (id, title, content, status, created_at, updated_at)
		VALUES ('t64-work-null-sessid', 'T64 work with null session_id',
		        'work body content', 'open',
		        CAST(strftime('%s','now') AS INTEGER),
		        CAST(strftime('%s','now') AS INTEGER))
	`)
	require.NoError(t, err)

	svc := NewWhyService(dm)
	require.NotNil(t, svc)
	report, err := svc.Explain("t64-work-null-sessid")
	require.NoError(t, err)
	assert.Empty(t, report.SkipReason,
		"work with NULL session_id must still resolve (regression: pre-fix Scan failed on NULL session_id and detectKind swallowed it as a miss)")
	assert.Equal(t, "work", report.ArtifactKind)
}

// TestT64_WhyNotFoundForUnknownID pins the legitimate miss path.
// A non-existent id must still return the SkipReason with a clear
// message — the T64 fix must not regress "not found" into "always
// resolved".
func TestT64_WhyNotFoundForUnknownID(t *testing.T) {
	dm := newTestDMForCmd(t)

	svc := NewWhyService(dm)
	require.NotNil(t, svc)
	report, err := svc.Explain("definitely-not-a-real-id-xyz")
	require.NoError(t, err)
	assert.NotEmpty(t, report.SkipReason,
		"unknown id must produce a SkipReason — the operator needs to see 'no artifact found'")
	assert.Contains(t, report.SkipReason, "no artifact found")
}
