// handoff_previous_test.go — pins the read-only "previous handoff for
// this identity domain" lookup that wake context projects its
// previous-session fields from.
//
// The selection contract under test:
//
//	previous = the most recent ENDED handoff whose identity belongs to
//	           the SAME namespace as the caller's current identity and
//	           is DISTINCT from that current identity
//
// Ordering is ended_at DESC, rowid DESC — the same deterministic pair
// every other handoff query uses, because GenerateID() produces
// SHA256 prefixes that are not chronologically sortable.
//
// Every case is hermetic (t.TempDir() workspace + temp file DB). No
// live MPM state is read or written.

package internal

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// previousHandoffDM returns a hermetic file-backed DatabaseManager
// rooted at a fresh temp workspace, with MPM_WORKSPACE pinned to that
// same root so the manager's own active.json and the ambient identity
// resolver agree — the production shape.
func previousHandoffDM(t *testing.T) (*DatabaseManager, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("MPM_WORKSPACE", root)
	t.Setenv("MPM_SHARED_DB", "")
	invalidateSessionCache()
	t.Cleanup(invalidateSessionCache)

	dm, err := NewDatabaseManager(root)
	require.NoError(t, err, "NewDatabaseManager(%q)", root)
	t.Cleanup(func() { _ = dm.Close() })
	return dm, root
}

// insertHandoffRow writes one session_handoffs row with fully pinned
// identity columns and timestamps. Direct INSERT (rather than
// EndSessionV2) is required because the production writer stamps
// ended_at = now and always allocates an mpm_session_id; the matrix
// needs NULL columns and exact timestamp ordering.
func insertHandoffRow(t *testing.T, dm *DatabaseManager, id, legacySID, mpmSID, fwSID string, endedAt int64, read bool) {
	t.Helper()
	var readAt any
	if read {
		readAt = endedAt
	}
	_, err := dm.db.Exec(`
		INSERT INTO session_handoffs
			(id, session_id, mpm_session_id, framework_session_id, ended_at, ended_state, summary, commitments, open_questions, read_at, read_by, created_at)
		VALUES (?, ?, ?, ?, ?, 'clean', ?, '[]', '[]', ?, ?, ?)`,
		id, nullableIfEmpty(legacySID), nullableIfEmpty(mpmSID), nullableIfEmpty(fwSID),
		endedAt, "handoff "+id, readAt, nullableIfEmpty("reader"), endedAt)
	require.NoError(t, err, "insert handoff %s", id)
}

func nullableIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- GetPreviousMPMSessionHandoff ------------------------------------------

// A fresh substrate has no prior lifecycle, so there is no previous
// identity to project.
func TestGetPreviousMPMSessionHandoff_NoHandoffs(t *testing.T) {
	dm, _ := previousHandoffDM(t)

	_, err := dm.GetPreviousMPMSessionHandoff("mpm-current")
	require.ErrorIs(t, err, sql.ErrNoRows,
		"no handoffs at all must yield ErrNoRows, not a synthesized identity")
}

// The current lifecycle's own closeout is NOT its previous session. A
// single handoff belonging to the current identity leaves nothing
// behind to point at.
func TestGetPreviousMPMSessionHandoff_OnlyCurrentIdentity(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-current", "", "mpm-current", "", 1700000200, false)

	_, err := dm.GetPreviousMPMSessionHandoff("mpm-current")
	require.ErrorIs(t, err, sql.ErrNoRows,
		"a handoff belonging to the current identity must not be reported as previous")
}

// Baseline: one ended prior lifecycle, one current lifecycle.
func TestGetPreviousMPMSessionHandoff_SinglePriorIdentity(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-s1", "", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-s2", "", "mpm-s2", "", 1700000200, false)

	got, err := dm.GetPreviousMPMSessionHandoff("mpm-s2")
	require.NoError(t, err)
	require.Equal(t, "mpm-s1", got.MPMSessionID)
	require.Equal(t, int64(1700000100), got.EndedAt)
	require.Equal(t, "h-s1", got.ID,
		"the ID and the ended_at must come from the SAME row")
}

// Multiple prior lifecycles: the most recent DISTINCT one wins, even
// though the newest row overall belongs to the current identity.
func TestGetPreviousMPMSessionHandoff_SkipsNewestCurrentRow(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-s1", "", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-s2", "", "mpm-s2", "", 1700000200, false)
	insertHandoffRow(t, dm, "h-s3", "", "mpm-s3", "", 1700000300, false)

	got, err := dm.GetPreviousMPMSessionHandoff("mpm-s2")
	require.NoError(t, err)
	require.Equal(t, "mpm-s3", got.MPMSessionID,
		"newest handoff overall is s3; it is the prior identity relative to s2")
	require.Equal(t, int64(1700000300), got.EndedAt)
}

// session_handoffs.mpm_session_id is NOT unique: one MPM lifecycle may
// write several closeouts (retry, crash-restart, subagent flush). Those
// are one prior SESSION, and the previous ended_at is the latest
// closeout within it — not the earliest, and not a second session.
func TestGetPreviousMPMSessionHandoff_MultipleCloseoutsOneLifecycle(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-s1-a", "", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-s1-b", "", "mpm-s1", "", 1700000150, false)
	insertHandoffRow(t, dm, "h-s2", "", "mpm-s2", "", 1700000200, false)

	got, err := dm.GetPreviousMPMSessionHandoff("mpm-s2")
	require.NoError(t, err)
	require.Equal(t, "mpm-s1", got.MPMSessionID)
	require.Equal(t, int64(1700000150), got.EndedAt,
		"previous ended_at is the LATEST closeout within the prior lifecycle")
}

// Handoffs written before the identity columns existed carry NULL. They
// cannot be attributed to an MPM lifecycle, so they must not be
// projected as an MPM-owned previous identity.
func TestGetPreviousMPMSessionHandoff_IgnoresNullMPMIdentity(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-legacy", "legacy-sess", "", "", 1700000300, false)
	insertHandoffRow(t, dm, "h-s1", "", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-s2", "", "mpm-s2", "", 1700000200, false)

	got, err := dm.GetPreviousMPMSessionHandoff("mpm-s2")
	require.NoError(t, err)
	require.Equal(t, "mpm-s1", got.MPMSessionID,
		"a NULL-mpm_session_id handoff is not an MPM-owned identity")
}

// The framework-owned id is a separate namespace. Even when it is the
// only thing a historical row carries, it must never surface as an
// MPM-owned previous identity.
func TestGetPreviousMPMSessionHandoff_IgnoresFrameworkIdentity(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-fw", "", "", "host-uuid-1", 1700000300, false)
	insertHandoffRow(t, dm, "h-s1", "", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-s2", "", "mpm-s2", "", 1700000200, false)

	got, err := dm.GetPreviousMPMSessionHandoff("mpm-s2")
	require.NoError(t, err)
	require.Equal(t, "mpm-s1", got.MPMSessionID)
	require.NotEqual(t, "host-uuid-1", got.MPMSessionID,
		"framework_session_id must never leak into the MPM identity namespace")
}

// Read state must not affect identity discovery. A consumed handoff is
// still the previous session; discovering it must not depend on whether
// the handoff was already delivered.
func TestGetPreviousMPMSessionHandoff_ReadStateIndependent(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-s1", "", "mpm-s1", "", 1700000100, true)
	insertHandoffRow(t, dm, "h-s2", "", "mpm-s2", "", 1700000200, false)

	got, err := dm.GetPreviousMPMSessionHandoff("mpm-s2")
	require.NoError(t, err)
	require.Equal(t, "mpm-s1", got.MPMSessionID)
}

// Timestamps resolve to the second, so a tie needs a secondary sort.
// rowid is strictly monotonic on INSERT and captures true insertion
// order; the generated id is a SHA256 prefix and is NOT sortable by
// time, so the tie-break must be rowid.
func TestGetPreviousMPMSessionHandoff_SameSecondTieBreakIsRowid(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	// Insertion order deliberately differs from id order, so a lexical
	// id sort would pick the wrong row.
	insertHandoffRow(t, dm, "zzz-first", "", "mpm-a", "", 1700000000, false)
	insertHandoffRow(t, dm, "aaa-second", "", "mpm-b", "", 1700000000, false)
	insertHandoffRow(t, dm, "mmm-current", "", "mpm-c", "", 1700000000, false)

	got, err := dm.GetPreviousMPMSessionHandoff("mpm-c")
	require.NoError(t, err)
	require.Equal(t, "mpm-b", got.MPMSessionID,
		"same-second tie must resolve to the later INSERT, not the greater id")
}

// With no current identity there is no distinctness to establish, so
// the lookup declines rather than inventing continuity.
func TestGetPreviousMPMSessionHandoff_EmptyCurrentYieldsNoPrevious(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-s1", "", "mpm-s1", "", 1700000100, false)

	_, err := dm.GetPreviousMPMSessionHandoff("")
	require.ErrorIs(t, err, sql.ErrNoRows,
		"an empty current identity must not be treated as 'everything is previous'")
}

// --- GetPreviousLegacySessionHandoff ---------------------------------------

// Legacy mode: identity lives in the nullable session_id column.
func TestGetPreviousLegacySessionHandoff_LegacyNamespace(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-a", "sess-a", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-b", "sess-b", "mpm-s2", "", 1700000200, false)

	got, err := dm.GetPreviousLegacySessionHandoff("sess-b")
	require.NoError(t, err)
	require.Equal(t, "sess-a", got.SessionID)
	require.Equal(t, int64(1700000100), got.EndedAt)
}

// Even in legacy mode, a row with NULL session_id cannot be attributed
// to a legacy identity — including when it is the only candidate.
func TestGetPreviousLegacySessionHandoff_IgnoresNullLegacyIdentity(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-null", "", "mpm-s1", "", 1700000300, false)
	insertHandoffRow(t, dm, "h-a", "sess-a", "", "", 1700000100, false)

	got, err := dm.GetPreviousLegacySessionHandoff("sess-b")
	require.NoError(t, err)
	require.Equal(t, "sess-a", got.SessionID,
		"a NULL-session_id handoff is not a legacy identity")
}

// The two namespaces are disjoint lookups. Each evaluates distinctness
// against its OWN column, proven with one shared current string that
// appears in exactly one column of each row:
//
//	h-old: legacy="other",  mpm="shared"   (older)
//	h-new: legacy="shared", mpm="other"   (newer)
//
// MPM lookup("shared") must return h-NEW — the row whose legacy column
// matches. Filtering on the legacy column would have excluded it. The
// legacy lookup("shared") must return h-OLD — the row whose MPM column
// matches. Filtering on the MPM column would have excluded it. Each
// result is the opposite row from the one a crossed lookup would give.
func TestGetPreviousHandoff_NamespacesDoNotCross(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-old", "other", "shared", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-new", "shared", "other", "", 1700000200, false)

	mpmGot, err := dm.GetPreviousMPMSessionHandoff("shared")
	require.NoError(t, err)
	require.Equal(t, "other", mpmGot.MPMSessionID,
		"MPM lookup must filter on mpm_session_id, not the legacy column")
	require.Equal(t, int64(1700000200), mpmGot.EndedAt,
		"the returned id and ended_at must describe the same row")

	legacyGot, err := dm.GetPreviousLegacySessionHandoff("shared")
	require.NoError(t, err)
	require.Equal(t, "other", legacyGot.SessionID,
		"legacy lookup must filter on session_id, not the MPM column")
	require.Equal(t, int64(1700000100), legacyGot.EndedAt,
		"the returned id and ended_at must describe the same row")
}

// --- purity ----------------------------------------------------------------

// The lookup is a projection: it must not allocate a session identity,
// rotate one, or write any row. Discovering the previous session is
// a read.
func TestGetPreviousHandoff_LookupDoesNotMutate(t *testing.T) {
	dm, root := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-s1", "", "mpm-s1", "", 1700000100, false)

	var before int
	require.NoError(t, dm.db.QueryRow(`SELECT COUNT(*) FROM session_handoffs`).Scan(&before))

	_, err := dm.GetPreviousMPMSessionHandoff("mpm-s2")
	require.NoError(t, err)

	var after int
	require.NoError(t, dm.db.QueryRow(`SELECT COUNT(*) FROM session_handoffs`).Scan(&after))
	require.Equal(t, before, after, "lookup must not write handoff rows")

	// No identity was allocated: active.json still does not exist.
	require.NoFileExists(t, filepath.Join(root, "active.json"),
		"previous-session discovery must not allocate an MPM session identity")
	require.Empty(t, CurrentMPMSessionID())
}

// A nil manager is a programming error, not a substrate condition;
// the helper must report it rather than panic.
func TestGetPreviousHandoff_NilManager(t *testing.T) {
	var dm *DatabaseManager
	_, err := dm.GetPreviousMPMSessionHandoff("mpm-s2")
	require.Error(t, err)
	require.False(t, errors.Is(err, sql.ErrNoRows))
}
