package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NotZero(t, h.CreatedAt)
	require.NotZero(t, h.EndedAt)

	// Read it back via the helper
	got, err := dm.GetHandoffBySessionID("session-test-1")
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
	require.NotZero(t, firstEndedAt)

	// Second call — must NOT error. Must overwrite.
	second, err := dm.EndSession(sessionID, "round 2: did 20 more minutes of substantive work", HandoffClean,
		[]string{"commit A", "commit B"}, []string{"open question"})
	require.NoError(t, err, "upsert must not error on duplicate session_id")

	// (c) id stable
	require.Equal(t, firstID, second.ID, "id should be stable across upserts")

	// (d) created_at moves forward on upsert (so a stale row's age
	// reflects the latest closeout, not its first incarnation)
	require.GreaterOrEqual(t, second.CreatedAt, firstEndedAt,
		"created_at should move forward to (or past) the first call's ended_at, got first=%v second=%v",
		firstEndedAt, second.CreatedAt)

	// (b) summary is the NEW value (last writer wins)
	require.Equal(t, "round 2: did 20 more minutes of substantive work", second.Summary)

	// (e) ended_at moved forward (or at least didn't go backward)
	require.GreaterOrEqual(t, second.EndedAt, firstEndedAt,
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
	_, err = dm.db.Exec(`UPDATE session_handoffs SET read_at = CAST(strftime('%s','now') AS INTEGER), read_by = 'agent' WHERE id = ?`, first.ID)
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
// empty summary and nil DB. (Empty session_id is intentionally NOT
// rejected — see TestHandoff_EndSession_EmptySessionIDCoexists for the
// new "external session IDs are optional" contract.)
func TestHandoff_EndSession_RejectsBadInputs(t *testing.T) {
	dm := newHandoffTestDM(t)

	_, err := dm.EndSession("sid", "", HandoffClean, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "summary is required")

	// Nil DB
	var nilDM *DatabaseManager
	_, err = nilDM.EndSession("sid", "summary", HandoffClean, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "db not initialized")
}

// newHandoffTestDM returns a hermetic in-memory DatabaseManager. All test
// files in this package share the same NewTestDM helper now (see
// internal/testhelpers.go) — no copy-paste drift surface per file.
func newHandoffTestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	return NewTestDM(t)
}

// TestHandoff_DeleteHandoff_RemovesRow pins the happy path: write a
// handoff, delete it by id, verify the row is gone and a follow-up
// lookup returns sql.ErrNoRows.
//
// Closes MPM-GAP-SHRED-HANDOFF-2026-08-19: before DeleteHandoff, the
// only way to remove a handoff was direct SQL. Tests and integration
// smoke scripts had to bypass the supported interface.
func TestHandoff_DeleteHandoff_RemovesRow(t *testing.T) {
	dm := newHandoffTestDM(t)

	h, err := dm.EndSession(
		"session-shred-1",
		"round 1 — to be shredded",
		HandoffClean,
		[]string{"will be deleted"},
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, h)
	require.NotEmpty(t, h.ID)

	n, err := dm.DeleteHandoff(h.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "exactly one row deleted")

	// Lookup must now return sql.ErrNoRows — the row is truly gone.
	_, err = dm.GetHandoffByID(h.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestHandoff_DeleteHandoff_Idempotent pins the contract that
// re-shredding an unknown id is a no-op (0 rows, no error). The handler
// surface uses this to report shredded=false cleanly when the caller
// passes a stale id.
func TestHandoff_DeleteHandoff_Idempotent(t *testing.T) {
	dm := newHandoffTestDM(t)

	n, err := dm.DeleteHandoff("never-existed")
	require.NoError(t, err)
	require.Equal(t, int64(0), n)

	// Empty id is rejected up front (programming error, not a real shred).
	_, err = dm.DeleteHandoff("")
	require.Error(t, err)
	require.Contains(t, err.Error(), "id is required")

	// Nil DB rejects up front (same shape as EndSession's nil-DB guard).
	var nilDM *DatabaseManager
	_, err = nilDM.DeleteHandoff("any")
	require.Error(t, err)
	require.Contains(t, err.Error(), "db not initialized")
}

// TestHandoff_GetLatestUnreadHandoff_SameSecond_TieBreaker pins the
// secondary sort: when multiple handoffs share the same ended_at
// timestamp (SQLite INTEGER Unix-epoch resolves to the second), the
// selection MUST be deterministic. Without `id DESC` as a
// tie-breaker the LIMIT 1 selection is non-deterministic — wake
// context would surface arbitrary session's commitments under load.
//
// Caveat on real IDs: GenerateID() produces SHA256 prefixes (see
// internal/core/db.go:3430). Those are NOT chronologically sortable —
// the tie-breaker enforces determinism, not "most recently inserted
// wins." For true insertion-order, `rowid DESC` is the right answer.
// This test uses synthetic ids that DO sort by insertion order so
// the user's stated fix is exercised end-to-end.
//
// MPM-BUG-HANDOFF-SAME-SECOND-TIE-2026-08-27.
func TestHandoff_GetLatestUnreadHandoff_SameSecond_TieBreaker(t *testing.T) {
	dm := newHandoffTestDM(t)
	// Three handoffs in chronological order, all stamped at the same
	// Unix-epoch second. id values are chosen so alphabetical DESC
	// matches insertion DESC (id-a < id-b < id-c).
	_, err := dm.db.Exec(`INSERT INTO session_handoffs (id, session_id, ended_at, ended_state, summary) VALUES ('id-a', 'sess-a', 1700000000, 'clean', 'first')`)
	require.NoError(t, err)
	_, err = dm.db.Exec(`INSERT INTO session_handoffs (id, session_id, ended_at, ended_state, summary) VALUES ('id-b', 'sess-b', 1700000000, 'clean', 'second')`)
	require.NoError(t, err)
	_, err = dm.db.Exec(`INSERT INTO session_handoffs (id, session_id, ended_at, ended_state, summary) VALUES ('id-c', 'sess-c', 1700000000, 'clean', 'third')`)
	require.NoError(t, err)

	got, err := dm.GetLatestUnreadHandoff()
	require.NoError(t, err)
	require.NotNil(t, got)
	// ORDER BY ended_at DESC, id DESC picks id-c (highest id). Pre-fix
	// the LIMIT 1 selection was non-deterministic.
	assert.Equal(t, "id-c", got.ID)
	assert.Equal(t, "third", got.Summary)
}

// TestHandoff_GetLatestHandoff_SameSecond_TieBreaker pins the same
// property for the read-state-agnostic GetLatestHandoff query.
func TestHandoff_GetLatestHandoff_SameSecond_TieBreaker(t *testing.T) {
	dm := newHandoffTestDM(t)
	_, err := dm.db.Exec(`INSERT INTO session_handoffs (id, session_id, ended_at, ended_state, summary, read_at) VALUES ('x-1', 'sess-1', 1700000000, 'clean', 'first', 1)`)
	require.NoError(t, err)
	_, err = dm.db.Exec(`INSERT INTO session_handoffs (id, session_id, ended_at, ended_state, summary, read_at) VALUES ('x-2', 'sess-2', 1700000000, 'clean', 'second', 1)`)
	require.NoError(t, err)
	_, err = dm.db.Exec(`INSERT INTO session_handoffs (id, session_id, ended_at, ended_state, summary, read_at) VALUES ('x-3', 'sess-3', 1700000000, 'clean', 'third', 1)`)
	require.NoError(t, err)

	got, err := dm.GetLatestHandoff()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "x-3", got.ID)
}

// TestHandoff_ListHandoffs_SameSecond_TieBreaker pins deterministic
// ordering on the bulk listing path too. Pre-fix, list ordering across
// same-second rows was undefined.
func TestHandoff_ListHandoffs_SameSecond_TieBreaker(t *testing.T) {
	dm := newHandoffTestDM(t)
	for _, id := range []string{"row-a", "row-b", "row-c"} {
		_, err := dm.db.Exec(`INSERT INTO session_handoffs (id, session_id, ended_at, ended_state, summary) VALUES (?, ?, 1700000000, 'clean', ?)`, id, "sess-"+id, id)
		require.NoError(t, err)
	}

	got, err := dm.ListHandoffs(10, false)
	require.NoError(t, err)
	require.Len(t, got, 3)
	// ORDER BY ended_at DESC, id DESC: row-c > row-b > row-a.
	assert.Equal(t, "row-c", got[0].ID)
	assert.Equal(t, "row-b", got[1].ID)
	assert.Equal(t, "row-a", got[2].ID)
}
