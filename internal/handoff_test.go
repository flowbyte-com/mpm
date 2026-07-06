package internal

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "github.com/mattn/go-sqlite3"
	"database/sql"
)

// TestHandoff_EndSession_InsertsAndReadsBack pins the basic
// happy path: one handoff for a fresh session is written, readable, and
// has both id and timestamps populated.
func TestHandoff_EndSession_InsertsAndReadsBack(t *testing.T) {
	dm := newHandoffTestDM(t)

	h, err := dm.EndSession(
		"session-test-1",
		"Round 1: did things",
		HandoffClean,
		[]string{"commit A", "commit B"},
		[]string{"open question X"},
	)
	require.NoError(t, err)
	require.NotNil(t, h)
	require.NotEmpty(t, h.ID)
	require.Equal(t, "session-test-1", h.SessionID)
	require.Equal(t, HandoffClean, h.EndedState)
	require.Equal(t, "Round 1: did things", h.Summary)
	require.Equal(t, []string{"commit A", "commit B"}, h.Commitments)
	require.Equal(t, []string{"open question X"}, h.OpenQuestions)
	require.False(t, h.CreatedAt.IsZero())
	require.False(t, h.EndedAt.IsZero())

	// Read it back via the helper
	got, err := dm.getHandoffBySessionID("session-test-1")
	require.NoError(t, err)
	require.Equal(t, h.ID, got.ID, "id should match between write and read-back")
	require.Equal(t, h.Summary, got.Summary)
}

// TestHandoff_EndSession_UpsertOverwrites is the core regression test
// for decision be61de1c4ef2ff4a. Before the upsert, calling EndSession
// twice with the same session_id produced a UNIQUE constraint failure
// and a warn-level audit row. The new contract is: second call silently
// overwrites the first, returning the same persisted id.
//
// Verifies:
//   (a) second call returns no error (no UNIQUE blow-up)
//   (b) summary is the new value (last writer wins)
//   (c) id is stable across upserts (external refs stay valid)
//   (d) created_at moves forward on upsert (table reader sees the
//       most recent closeout as a fresh row, not a 12-day-old stale
//       timestamp from the row's first incarnation — see bug
//       2026-07-06 where agent:main:main closeouts were invisible
//       to wake context because read_at + created_at carried over)
//   (e) ended_at does move forward on the second call
//   (f) only ONE row exists in the table (upsert, not append)
func TestHandoff_EndSession_UpsertOverwrites(t *testing.T) {
	dm := newHandoffTestDM(t)
	sessionID := "session-upsert-1"

	first, err := dm.EndSession(sessionID, "round 1: started work", HandoffClean,
		[]string{"started"}, nil)
	require.NoError(t, err)
	firstID := first.ID
	firstEndedAt := first.EndedAt
	require.False(t, firstEndedAt.IsZero())

	// Second call — must NOT error. Must overwrite.
	second, err := dm.EndSession(sessionID, "round 2: did 20 more minutes of substantive work", HandoffClean,
		[]string{"commit A", "commit B"}, []string{"open question"})
	require.NoError(t, err, "upsert must not error on duplicate session_id")

	// (c) id stable
	require.Equal(t, firstID, second.ID, "id should be stable across upserts")

	// (d) created_at moves forward on upsert (so a stale row's age
	// reflects the latest closeout, not its first incarnation)
	require.True(t, !second.CreatedAt.Before(firstEndedAt),
		"created_at should move forward to (or past) the first call's ended_at, got first=%v second=%v",
		firstEndedAt, second.CreatedAt)

	// (b) summary is the NEW value (last writer wins)
	require.Equal(t, "round 2: did 20 more minutes of substantive work", second.Summary)

	// (e) ended_at moved forward (or at least didn't go backward)
	require.True(t, !second.EndedAt.Before(firstEndedAt),
		"ended_at should move forward, got first=%v second=%v",
		firstEndedAt, second.EndedAt)

	// commitments + open_questions overwritten
	require.Equal(t, []string{"commit A", "commit B"}, second.Commitments)
	require.Equal(t, []string{"open question"}, second.OpenQuestions)

	// (f) only one row exists
	var rowCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM session_handoffs WHERE session_id = ?`, sessionID,
	).Scan(&rowCount))
	require.Equal(t, 1, rowCount, "upsert must not append")
}

// TestHandoff_EndSession_UpsertResetsReadAt pins the 2026-07-06
// fix. The UPSERT MUST reset read_at + read_by to NULL on every
// overwrite, because a handoff with new content is, by definition,
// unread. Otherwise a long-lived session_id whose first handoff was
// consumed weeks ago silently shadows every subsequent closeout —
// wake context's `WHERE read_at IS NULL` filter would skip the new
// content as "already read" and return no handoff at all.
//
// Regression scenario: agent:main:main session_id has been alive
// since 2026-06-23; its first handoff was read on day 1; every
// subsequent closeout overwrote the same row but kept the stale
// read_at, so 12 days of work were invisible to wake.
func TestHandoff_EndSession_UpsertResetsReadAt(t *testing.T) {
	dm := newHandoffTestDM(t)
	sessionID := "session-upsert-readat"

	first, err := dm.EndSession(sessionID, "v1", HandoffClean, nil, nil)
	require.NoError(t, err)

	// Manually mark as read (simulating wake consumption)
	_, err = dm.db.Exec(`UPDATE session_handoffs SET read_at = datetime('now'), read_by = 'agent' WHERE id = ?`, first.ID)
	require.NoError(t, err)

	// Sanity: row IS read before upsert
	row := dm.db.QueryRow(`SELECT read_at, read_by FROM session_handoffs WHERE session_id = ?`, sessionID)
	var readAt sql.NullString
	var readBy sql.NullString
	require.NoError(t, row.Scan(&readAt, &readBy))
	require.True(t, readAt.Valid, "pre-upsert: read_at should be set")
	require.True(t, readBy.Valid && readBy.String == "agent", "pre-upsert: read_by should be 'agent'")

	// Upsert — must reset read_at + read_by
	second, err := dm.EndSession(sessionID, "v2", HandoffClean, nil, nil)
	require.NoError(t, err)

	row = dm.db.QueryRow(`SELECT read_at, read_by FROM session_handoffs WHERE session_id = ?`, sessionID)
	require.NoError(t, row.Scan(&readAt, &readBy))
	require.False(t, readAt.Valid, "post-upsert: read_at should be NULL so wake surfaces the new content")
	require.False(t, readBy.Valid, "post-upsert: read_by should be NULL")
	require.NotNil(t, second)

	// And: GetLatestUnreadHandoff should now find this row (the
	// bug was that it returned ErrNoRows because read_at was
	// sticky across upserts)
	latest, err := dm.GetLatestUnreadHandoff()
	require.NoError(t, err)
	require.NotNil(t, latest)
	require.Equal(t, sessionID, latest.SessionID)
	require.Equal(t, "v2", latest.Summary, "wake should see the NEW summary, not v1")
}

// TestHandoff_EndSession_EmptySlicesSerializeAsBrackets verifies the
// "ensure non-nil slices so JSON encoding produces [] not null"
// invariant. The default in schema.go is '[]', so even if the column
// were empty the schema would catch it — but the Go side must also
// produce a valid non-nil slice.
func TestHandoff_EndSession_EmptySlicesSerializeAsBrackets(t *testing.T) {
	dm := newHandoffTestDM(t)
	h, err := dm.EndSession("session-empty", "summary", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, h.Commitments)
	require.NotNil(t, h.OpenQuestions)
	require.Len(t, h.Commitments, 0)
	require.Len(t, h.OpenQuestions, 0)
}

// TestHandoff_EndSession_RejectsBadInputs covers the validation paths:
// empty session_id, empty summary, nil DB.
func TestHandoff_EndSession_RejectsBadInputs(t *testing.T) {
	dm := newHandoffTestDM(t)

	_, err := dm.EndSession("", "summary", HandoffClean, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "session_id is required")

	_, err = dm.EndSession("sid", "", HandoffClean, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "summary is required")

	// Nil DB
	var nilDM *DatabaseManager
	_, err = nilDM.EndSession("sid", "summary", HandoffClean, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "db not initialized")
}

// newHandoffTestDM is a copy of newTestDM from evidence_store_test.go
// kept local to this file so the test is self-contained.
func newHandoffTestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	dm := NewDatabaseManagerForDB(db)
	require.NoError(t, dm.InitSchema())
	t.Cleanup(func() { dm.Close() })
	return dm
}
