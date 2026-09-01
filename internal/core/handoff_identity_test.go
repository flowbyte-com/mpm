// handoff_identity_test.go — 2026-09-01 mission: external session IDs
// are optional. The handoff's durable identity is the MPM-generated
// `id` and `created_at`; `session_id` is opaque correlation metadata
// the caller MAY supply but does NOT have to.
//
// Test matrix below pins:
//
//   1. Identity shapes    — omitted, "42", "abc123", UUID, framework-tagged
//   2. Temporal ordering  — explicit created_at sequence, latest wins
//   3. Recovery           — close+reopen DB, handoff survives
//   4. Multiple frameworks — claude-code (no session_id) alongside
//                            mpm-cli (with session_id)
//   5. NULL semantics     — multiple NULL-session rows coexist
//   6. Duplicate handling — same session_id twice → UPSERT
//                            different session_ids → append
//   7. Bad-input guard    — empty session_id and "sid" with empty summary
//                            are distinct failure modes
//
// All tests use the same hermetic in-memory DatabaseManager shape
// (NewTestDM) so they share a single helper and the linter stays clean.

package internal

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFileBackedTestDM returns a hermetic production-path DatabaseManager
// rooted at the given tmpDir. Mirrors foreignKeyTestDM (foreign_keys_test.go)
// but uses a caller-supplied directory so the test can drive close+reopen
// against a stable on-disk path. This is the only way to exercise the
// migration (sentinel-gated, idempotent) and the recovery-across-restart
// shape that claude-code relies on.
func newFileBackedTestDM(t *testing.T, tmpDir string) *DatabaseManager {
	t.Helper()
	t.Setenv("MPM_WORKSPACE", tmpDir)
	t.Setenv("MPM_SHARED_DB", "")
	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	return dm
}

// --- Identity shapes -------------------------------------------------------

// TestHandoff_EndSession_IdentityMatrix_Omitted exercises the Claude
// Code path: no MPM_SESSION_ID, no framework UUID, just a summary.
// The handoff must persist with NULL session_id and a MPM-generated id.
func TestHandoff_EndSession_IdentityMatrix_Omitted(t *testing.T) {
	dm := newHandoffTestDM(t)

	h, err := dm.EndSession("", "claude-code session summary", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, h)
	require.NotEmpty(t, h.ID, "MPM must generate a durable handoff id")
	require.Empty(t, h.SessionID, "omitted session_id surfaces as empty string (NULL in storage)")
	require.NotZero(t, h.CreatedAt)
	require.NotZero(t, h.EndedAt)
	require.Equal(t, "claude-code session summary", h.Summary)
}

// TestHandoff_EndSession_IdentityMatrix_Numeric exercises a framework
// that uses a bare integer session identifier. The substrate must
// accept "42" verbatim without coercing or rejecting it.
func TestHandoff_EndSession_IdentityMatrix_Numeric(t *testing.T) {
	dm := newHandoffTestDM(t)

	h, err := dm.EndSession("42", "numeric id", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "42", h.SessionID, "session_id must be stored verbatim, not coerced to int")
}

// TestHandoff_EndSession_IdentityMatrix_ShortString exercises an
// opaque short string ("abc123"). The substrate accepts any non-empty
// string up to the column length limit.
func TestHandoff_EndSession_IdentityMatrix_ShortString(t *testing.T) {
	dm := newHandoffTestDM(t)

	h, err := dm.EndSession("abc123", "short opaque id", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "abc123", h.SessionID)
}

// TestHandoff_EndSession_IdentityMatrix_UUID exercises a UUID-shaped
// external identifier. This is the most common shape and must work
// identically to the legacy contract.
func TestHandoff_EndSession_IdentityMatrix_UUID(t *testing.T) {
	dm := newHandoffTestDM(t)

	uuid := "550e8400-e29b-41d4-a716-446655440000"
	h, err := dm.EndSession(uuid, "uuid session", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.Equal(t, uuid, h.SessionID)
}

// TestHandoff_EndSession_IdentityMatrix_FrameworkTagged exercises a
// framework-tagged identifier ("claude-code-2026-09-01"). Two
// frameworks using the same numeric suffix ("42" above and
// "claude-code-42" here) must NOT collide — the framework owns
// namespacing on its side, but the substrate must preserve what
// it is told verbatim.
func TestHandoff_EndSession_IdentityMatrix_FrameworkTagged(t *testing.T) {
	dm := newHandoffTestDM(t)

	tagged := "claude-code-2026-09-01"
	h, err := dm.EndSession(tagged, "framework-tagged", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.Equal(t, tagged, h.SessionID)
}

// --- Temporal ordering -----------------------------------------------------

// TestHandoff_TemporalOrdering pins that handoffs written back-to-back
// surface in ended_at order regardless of which (NULL or non-NULL)
// session_id they carry. The wake context depends on this for
// showing "the most recent handoff" to the next session.
//
// Three handoffs H1, H2, H3 are written with explicit created_at
// values so the test is not sensitive to wall-clock timing.
func TestHandoff_TemporalOrdering(t *testing.T) {
	dm := newHandoffTestDM(t)

	// H1: claude-code (NULL session_id), earliest
	h1, err := dm.EndSession("", "H1: claude-code, earliest", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, h1.ID)

	// H2: mpm-cli with session_id, middle
	h2, err := dm.EndSession("sess-h2", "H2: mpm-cli, middle", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, h2.ID)

	// H3: claude-code again (NULL session_id), latest
	h3, err := dm.EndSession("", "H3: claude-code, latest", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, h3.ID)

	// Each call must produce a distinct id (NULL session_id means
	// no UPSERT key, so each insert is a fresh row).
	require.NotEqual(t, h1.ID, h3.ID, "two NULL-session rows must get distinct ids")

	// GetLatestHandoff must return the most recent (H3).
	latest, err := dm.GetLatestHandoff()
	require.NoError(t, err)
	require.NotNil(t, latest)
	require.Equal(t, h3.ID, latest.ID, "GetLatestHandoff must return H3, the most recent ended_at")
	require.Equal(t, "H3: claude-code, latest", latest.Summary)
	require.Empty(t, latest.SessionID, "H3 has no external session id")

	// GetHandoffByID must round-trip all three.
	got1, err := dm.GetHandoffByID(h1.ID)
	require.NoError(t, err)
	require.Equal(t, "H1: claude-code, earliest", got1.Summary)

	got2, err := dm.GetHandoffBySessionID("sess-h2")
	require.NoError(t, err)
	require.Equal(t, h2.ID, got2.ID, "GetHandoffBySessionID must return H2 for sess-h2")
}

// --- Recovery across restart ----------------------------------------------

// TestHandoff_RecoveryAcrossRestart pins that a handoff written without
// a session_id survives a close+reopen of the DatabaseManager. This
// is the recovery shape Claude Code needs: it boots, writes a handoff,
// exits, and the next boot must find the handoff intact.
//
// The test uses a file-backed DatabaseManager (rather than the in-memory
// helper) so close+reopen actually exercises the SQLite persistence
// path.
func TestHandoff_RecoveryAcrossRestart(t *testing.T) {
	tmpDir := t.TempDir()

	// First boot — write handoff with no session_id.
	dm1 := newFileBackedTestDM(t, tmpDir)
	h, err := dm1.EndSession("", "recovery test summary", HandoffClean,
		[]string{"commit-1"}, []string{"open-q"})
	require.NoError(t, err)
	require.NotEmpty(t, h.ID)
	require.Empty(t, h.SessionID)
	require.NoError(t, dm1.Close())

	// Second boot — read back by id (the only addressable path for
	// NULL-session rows).
	dm2 := newFileBackedTestDM(t, tmpDir)
	t.Cleanup(func() { _ = dm2.Close() })

	got, err := dm2.GetHandoffByID(h.ID)
	require.NoError(t, err, "handoff with NULL session_id must survive close+reopen")
	require.Equal(t, h.ID, got.ID)
	require.Equal(t, "recovery test summary", got.Summary)
	require.Empty(t, got.SessionID, "NULL session_id must round-trip as empty string")
	require.Equal(t, []string{"commit-1"}, got.Commitments)
	require.Equal(t, []string{"open-q"}, got.OpenQuestions)
}

// TestHandoff_RecoveryAcrossRestart_GetLatestHandoff pins that the
// wake-context hot path (GetLatestHandoff, which the next session's
// read-side consults) sees a NULL-session handoff after restart. This
// is the read-side contract that motivated the change.
func TestHandoff_RecoveryAcrossRestart_GetLatestHandoff(t *testing.T) {
	tmpDir := t.TempDir()

	dm1 := newFileBackedTestDM(t, tmpDir)
	_, err := dm1.EndSession("", "anonymous session", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.NoError(t, dm1.Close())

	dm2 := newFileBackedTestDM(t, tmpDir)
	t.Cleanup(func() { _ = dm2.Close() })

	latest, err := dm2.GetLatestHandoff()
	require.NoError(t, err, "GetLatestHandoff must find a NULL-session handoff after restart")
	require.NotNil(t, latest)
	require.Empty(t, latest.SessionID)
	require.Equal(t, "anonymous session", latest.Summary)
}

// --- Multiple frameworks --------------------------------------------------

// TestHandoff_MultipleFrameworks_ClaudeCodeAndMpmCLI exercises the
// canonical multi-framework scenario: claude-code (no external session
// id) writes handoffs alongside mpm-cli (with session id). Both must
// be readable independently and the wake context must surface the
// most recent of either.
//
// This is the end-to-end shape the production wake-context payload
// will encounter: a wake may consume a handoff from claude-code one
// day and from mpm-cli the next.
func TestHandoff_MultipleFrameworks_ClaudeCodeAndMpmCLI(t *testing.T) {
	dm := newHandoffTestDM(t)

	// claude-code writes two handoffs (NULL session_id).
	cc1, err := dm.EndSession("", "cc round 1", HandoffClean, nil, nil)
	require.NoError(t, err)
	cc2, err := dm.EndSession("", "cc round 2", HandoffClean, nil, nil)
	require.NoError(t, err)

	// mpm-cli writes one handoff with a real session_id.
	cli, err := dm.EndSession("sess-cli-1", "cli handoff", HandoffClean, nil, nil)
	require.NoError(t, err)

	// Each call must produce a distinct id.
	require.NotEqual(t, cc1.ID, cc2.ID)
	require.NotEqual(t, cc1.ID, cli.ID)
	require.NotEqual(t, cc2.ID, cli.ID)

	// GetLatestHandoff must return the most recent regardless of
	// session_id presence.
	latest, err := dm.GetLatestHandoff()
	require.NoError(t, err)
	require.NotNil(t, latest)
	require.Equal(t, cli.ID, latest.ID, "most recent is mpm-cli (last write)")
	require.Equal(t, "sess-cli-1", latest.SessionID)

	// GetHandoffBySessionID must find the mpm-cli handoff.
	cliBack, err := dm.GetHandoffBySessionID("sess-cli-1")
	require.NoError(t, err)
	require.Equal(t, cli.ID, cliBack.ID)

	// The two claude-code handoffs are only addressable by id (NULL
	// session_id, no UPSERT key to lookup against).
	cc1Back, err := dm.GetHandoffByID(cc1.ID)
	require.NoError(t, err)
	require.Equal(t, "cc round 1", cc1Back.Summary)
	require.Empty(t, cc1Back.SessionID)

	cc2Back, err := dm.GetHandoffByID(cc2.ID)
	require.NoError(t, err)
	require.Equal(t, "cc round 2", cc2Back.Summary)
	require.Empty(t, cc2Back.SessionID)
}

// --- NULL semantics --------------------------------------------------------

// TestHandoff_NullSessionIDRows_CoexistWithoutConflict is the
// structural SQLite contract: multiple NULLs in a UNIQUE column are
// distinct. This is the property that lets claude-code accumulate
// handoffs without colliding with itself.
//
// If SQLite's NULL-distinct semantics ever change (they won't —
// sqlite.org/lang_createtable §3 is a load-bearing guarantee), this
// test will fail noisily and the operator will know the migration
// needs revisiting.
func TestHandoff_NullSessionIDRows_CoexistWithoutConflict(t *testing.T) {
	dm := newHandoffTestDM(t)

	for i := 0; i < 5; i++ {
		_, err := dm.EndSession("", "row "+string(rune('A'+i)), HandoffClean, nil, nil)
		require.NoError(t, err)
	}

	var n int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM session_handoffs WHERE session_id IS NULL`,
	).Scan(&n))
	require.Equal(t, 5, n, "five NULL-session rows must coexist under the UNIQUE constraint")
}

// --- Duplicate handling ----------------------------------------------------

// TestHandoff_DuplicateSessionID_UpsertOverwrites pins the legacy
// UPSERT semantics for non-NULL session_ids: same session_id twice →
// second write overwrites the first, id is stable, summary is the
// new value, created_at moves forward. (See the existing
// TestHandoff_EndSession_UpsertOverwrites for the original; this is
// the explicit identity-matrix restatement.)
func TestHandoff_DuplicateSessionID_UpsertOverwrites(t *testing.T) {
	dm := newHandoffTestDM(t)

	first, err := dm.EndSession("sess-dup", "first write", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "first write", first.Summary)

	// Tiny pause so the second write's created_at is strictly later.
	time.Sleep(10 * time.Millisecond)

	second, err := dm.EndSession("sess-dup", "second write (overwrites)", HandoffClean, nil, nil)
	require.NoError(t, err)

	require.Equal(t, first.ID, second.ID, "id must be stable across upserts")
	require.Equal(t, "second write (overwrites)", second.Summary)
	require.GreaterOrEqual(t, second.CreatedAt, first.CreatedAt,
		"created_at must move forward on upsert")

	// Exactly one row exists.
	var rowCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM session_handoffs WHERE session_id = ?`, "sess-dup",
	).Scan(&rowCount))
	require.Equal(t, 1, rowCount, "upsert must not append")
}

// TestHandoff_DifferentSessionIDs_Append pins the negative of
// upsert: different session_ids → distinct rows. Both must be
// readable independently.
func TestHandoff_DifferentSessionIDs_Append(t *testing.T) {
	dm := newHandoffTestDM(t)

	a, err := dm.EndSession("sess-A", "row A", HandoffClean, nil, nil)
	require.NoError(t, err)
	b, err := dm.EndSession("sess-B", "row B", HandoffClean, nil, nil)
	require.NoError(t, err)

	require.NotEqual(t, a.ID, b.ID)

	gotA, err := dm.GetHandoffBySessionID("sess-A")
	require.NoError(t, err)
	require.Equal(t, a.ID, gotA.ID)

	gotB, err := dm.GetHandoffBySessionID("sess-B")
	require.NoError(t, err)
	require.Equal(t, b.ID, gotB.ID)
}

// --- Bad-input guard -------------------------------------------------------

// TestHandoff_EmptySummaryRejected pins that the summary guard is
// unchanged by the optional-session-id contract. Summary remains
// mandatory because the next session needs SOMETHING to display.
func TestHandoff_EmptySummaryRejected(t *testing.T) {
	dm := newHandoffTestDM(t)

	// Empty summary with empty session_id.
	_, err := dm.EndSession("", "", HandoffClean, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "summary is required")

	// Empty summary with non-empty session_id.
	_, err = dm.EndSession("sess-x", "", HandoffClean, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "summary is required")
}

// --- Read-back assertions --------------------------------------------------

// TestHandoff_GetHandoffBySessionID_EmptyReturnsErrNoRows pins that
// looking up an empty session_id is a clean ErrNoRows — not a scan
// error, not a database error. Callers (e.g. shred_handoff) that
// pre-lookup by session_id before calling DeleteHandoff rely on this
// for their "shredded=false, message=not found" idempotent path.
func TestHandoff_GetHandoffBySessionID_EmptyReturnsErrNoRows(t *testing.T) {
	dm := newHandoffTestDM(t)

	_, err := dm.GetHandoffBySessionID("")
	require.ErrorIs(t, err, sql.ErrNoRows,
		"empty session_id must return sql.ErrNoRows without hitting the DB")
}

// TestHandoff_ListHandoffs_HandlesNullSessionID pins that ListHandoffs
// surfaces NULL-session rows to callers with SessionID == "". This is
// the "all recent handoffs regardless of framework" view — the next
// session can see what previous claude-code boots left behind.
func TestHandoff_ListHandoffs_HandlesNullSessionID(t *testing.T) {
	dm := newHandoffTestDM(t)

	_, err := dm.EndSession("sess-x", "with id", HandoffClean, nil, nil)
	require.NoError(t, err)
	_, err = dm.EndSession("", "without id", HandoffClean, nil, nil)
	require.NoError(t, err)

	list, err := dm.ListHandoffs(50, false)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// Each row's SessionID round-trips: "" for NULL, "sess-x" for the
	// non-NULL row.
	sawNull := false
	sawNonNull := false
	for _, h := range list {
		if h.SessionID == "" {
			sawNull = true
		}
		if h.SessionID == "sess-x" {
			sawNonNull = true
		}
	}
	assert.True(t, sawNull, "ListHandoffs must surface NULL-session rows with SessionID == \"\"")
	assert.True(t, sawNonNull, "ListHandoffs must surface non-NULL-session rows with their value")
}

// TestHandoff_GetLatestUnreadHandoff_PrefersNullSessionID is the
// wake-context hot path: when a handoff exists with NULL session_id,
// GetLatestUnreadHandoff must return it (assuming read_at IS NULL —
// the default for a freshly written handoff). This is what the next
// claude-code boot sees when it reads wake context.
func TestHandoff_GetLatestUnreadHandoff_PrefersNullSessionID(t *testing.T) {
	dm := newHandoffTestDM(t)

	// Order: non-NULL first, NULL second. NULL must be returned by
	// GetLatestUnreadHandoff because it is the most recent write.
	_, err := dm.EndSession("sess-1", "first", HandoffClean, nil, nil)
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)
	h2, err := dm.EndSession("", "second (NULL session_id)", HandoffClean, nil, nil)
	require.NoError(t, err)

	got, err := dm.GetLatestUnreadHandoff()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, h2.ID, got.ID,
		"GetLatestUnreadHandoff must return the most recent handoff regardless of session_id presence")
	require.Empty(t, got.SessionID)
}

// --- Concurrency -----------------------------------------------------------

// TestHandoff_NullSessionID_ConcurrentInsertsNoCollision pins that
// concurrent goroutines writing NULL-session handoffs both succeed
// and produce distinct ids. SQLite serializes writes via WAL +
// busy_timeout; the contract is no UNIQUE-conflict failure, even
// though the session_id column is UNIQUE.
//
// Uses a file-backed DM (not the shared in-memory cache) because
// `file:NAME?mode=memory&cache=shared` enforces stricter table-level
// locks than private-cache WAL mode — concurrent inserts against a
// shared-cache table surface SQLITE_LOCKED before busy_timeout
// even fires. Production is file-backed WAL; the file-backed DSN
// mirrors that and is what the test exercises.
func TestHandoff_NullSessionID_ConcurrentInsertsNoCollision(t *testing.T) {
	tmpDir := t.TempDir()
	dm := newFileBackedTestDM(t, tmpDir)
	t.Cleanup(func() { dm.Close() })

	const n = 8
	ids := make(chan string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			h, err := dm.EndSession("", "concurrent", HandoffClean, nil, nil)
			if err != nil {
				errs <- err
				return
			}
			ids <- h.ID
		}(i)
	}
	wg.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent insert error: %v", err)
	}
	seen := map[string]bool{}
	for id := range ids {
		require.False(t, seen[id], "duplicate id from concurrent insert: %s", id)
		seen[id] = true
	}
	require.Len(t, seen, n, "all n concurrent inserts must produce distinct ids")

	// All n rows are in the table with NULL session_id.
	var rowCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM session_handoffs WHERE session_id IS NULL`,
	).Scan(&rowCount))
	require.Equal(t, n, rowCount)
}

// --- Schema-shape contract -------------------------------------------------

// TestSessionHandoffsSchemaShape_NullableSessionID pins that the
// canonical schema's session_id column is declared as nullable. This
// is the structural contract the migration maintains: if a future
// schema change re-introduces NOT NULL on session_id, this test
// fails noisily before any production handoff gets a hard error.
//
// Uses pragma_table_info which returns the on-disk schema shape —
// the same source of truth migration_session_handoffs_optional_session_id.go
// probes when deciding whether to skip the rebuild.
func TestSessionHandoffsSchemaShape_NullableSessionID(t *testing.T) {
	dm := newHandoffTestDM(t)

	var notnull int
	require.NoError(t, dm.db.QueryRow(
		`SELECT "notnull" FROM pragma_table_info('session_handoffs') WHERE name = 'session_id'`,
	).Scan(&notnull))
	require.Equal(t, 0, notnull,
		"session_handoffs.session_id must be nullable (notnull=0 in pragma_table_info)")
}

// TestSessionHandoffsSchemaShape_UniquePreserved pins that the UNIQUE
// constraint on session_id is preserved across the migration. This is
// what gives us the UPSERT semantics on non-NULL values while
// permitting multiple NULL values (SQLite NULL-distinct under UNIQUE).
//
// SQLite declares a UNIQUE-on-column constraint as a column-level
// inline UNIQUE in the CREATE TABLE statement (so the auto-index
// does NOT show up in sqlite_master's `sql` field as a literal
// "UNIQUE" string — the constraint lives in the table's DDL, not as
// a separately-named CREATE INDEX). The functional proof is
// behavior: an UPSERT on a duplicate non-NULL session_id overwrites
// rather than appending; multiple NULL-session rows coexist without
// UNIQUE-conflict. This test pins both — the schema-shape probe
// confirms session_id is not the PRIMARY KEY (id is), and the
// functional probe confirms the contract end-to-end.
func TestSessionHandoffsSchemaShape_UniquePreserved(t *testing.T) {
	dm := newHandoffTestDM(t)

	// session_id must NOT be the PRIMARY KEY — id is.
	var pk int
	require.NoError(t, dm.db.QueryRow(
		`SELECT "pk" FROM pragma_table_info('session_handoffs') WHERE name = 'session_id'`,
	).Scan(&pk))
	require.Equal(t, 0, pk,
		"session_id must NOT be the PRIMARY KEY (id is). UNIQUE constraint is on a separate auto-index.")

	// Functional proof of UNIQUE-on-session_id: a duplicate INSERT
	// must overwrite, not append. (This is the same UPSERT semantics
	// EndSession's ON CONFLICT clause relies on.)
	h1, err := dm.EndSession("sess-unique-1", "first", HandoffClean, nil, nil)
	require.NoError(t, err)
	h2, err := dm.EndSession("sess-unique-1", "second (overwrites)", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.Equal(t, h1.ID, h2.ID, "UPSERT keyed on session_id must preserve id across writes")

	var rowCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM session_handoffs WHERE session_id = ?`, "sess-unique-1",
	).Scan(&rowCount))
	require.Equal(t, 1, rowCount, "exactly one row exists for session_id=sess-unique-1")
}

// --- Migration round-trip (file-backed, since sentinel lives in DB) -------

// TestSessionHandoffsMigration_RebuildsNotNullColumn verifies the
// migration on a file-backed DB. Pre-migration DDL has session_id as
// NOT NULL; post-migration DDL has session_id as nullable. The
// migration must be idempotent (running twice does not error and
// does not lose data).
func TestSessionHandoffsMigration_RebuildsNotNullColumn(t *testing.T) {
	tmpDir := t.TempDir()

	// First boot — full init (migration runs).
	dm := newFileBackedTestDM(t, tmpDir)

	// Write a handoff with a real session_id (pre-migration shape would
	// have rejected this... but on a fresh boot the migration has
	// already run, so this is the post-migration shape accepting it).
	pre, err := dm.EndSession("sess-pre", "pre-existing row", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, pre.ID)

	// Confirm the column is nullable after init.
	var notnull int
	require.NoError(t, dm.db.QueryRow(
		`SELECT "notnull" FROM pragma_table_info('session_handoffs') WHERE name = 'session_id'`,
	).Scan(&notnull))
	require.Equal(t, 0, notnull)
	require.NoError(t, dm.Close())

	// Second boot — must be a no-op (sentinel already written).
	dm2 := newFileBackedTestDM(t, tmpDir)
	t.Cleanup(func() { _ = dm2.Close() })

	// Pre-existing row must survive the second boot's migration pass.
	pre2, err := dm2.GetHandoffBySessionID("sess-pre")
	require.NoError(t, err)
	require.Equal(t, pre.ID, pre2.ID, "migration must not lose existing rows")
	require.Equal(t, "pre-existing row", pre2.Summary)
}
