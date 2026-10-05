// wake_context_previous_session_test.go — pins the previous-session
// identity contract on the wake-context wire surface.
//
// WakeContextData.SessionPreviousID and SessionPreviousEndedAt have
// always existed on the public wire form but were left at their zero
// values. These tests make them truthful:
//
//   - previous == most recent ENDED session in the SAME identity
//     namespace as the current session, and distinct from it
//   - the id and the ended_at always come from the SAME handoff row
//   - the answer does not depend on handoff read state, so consuming a
//     handoff never erases the identity of the session that wrote it
//   - discovery is a projection: it allocates no identity, rotates
//     nothing, and writes no rows
//
// Every test is hermetic (t.TempDir() workspace + file-backed manager
// rooted there). No live MPM state is read or written.

package internal

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// insertSessionsRow writes one row into the dormant sessions table.
// That table is the LEGACY identity source: a workspace with no
// active.json resolves its current session from here.
func insertSessionsRow(t *testing.T, dm *DatabaseManager, sessionID string, createdAt int64) {
	t.Helper()
	// source_path and metadata are scanned into plain strings by
	// GetLastSession, so the row must carry values rather than NULL.
	_, err := dm.db.Exec(
		`INSERT INTO sessions (id, session_id, content, content_hash, source_path, metadata, created_at) VALUES (?, ?, 'legacy session body', ?, ?, '{}', ?)`,
		"sess-row-"+sessionID, sessionID, "hash-"+sessionID, "fixture:"+sessionID, createdAt)
	require.NoError(t, err, "insert sessions row %s", sessionID)
}

// --- core lifecycle reproduction -------------------------------------------

// The defect this whole file exists for, in the shape production
// produces it: lifecycle S1 ends and writes its handoff, the operator
// rotates into S2, and S2 wakes. The handoff carries everything needed
// to name S1 and the moment it ended, yet both previous-session fields
// were reporting "" and 0.
func TestWakeContext_PreviousSession_AfterLifecycleEndAndRotate(t *testing.T) {
	dm, _ := previousHandoffDM(t)

	// S1 ends: the handoff records the lifecycle identity that
	// EndSessionV2 allocated from this workspace's own active.json.
	h1, err := dm.EndSessionV2("", "host-s1", "", "s1 summary", HandoffClean, nil, nil)
	require.NoError(t, err)
	require.Equal(t, h1.MPMSessionID, CurrentMPMSessionID(), "S1 is the active identity")

	// Operator rotates into S2.
	s2 := RotateMPMSessionID()
	require.NotEqual(t, h1.MPMSessionID, s2, "rotation must produce a distinct lifecycle")

	wc, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)

	require.Equal(t, s2, wc.MPMSessionID)
	require.Equal(t, s2, wc.SessionCurrentID)
	require.Equal(t, h1.MPMSessionID, wc.SessionPreviousID,
		"the ended S1 lifecycle is the previous session")
	require.Equal(t, h1.EndedAt, wc.SessionPreviousEndedAt,
		"previous ended_at comes from the same handoff row as the id")

	// Legacy alias follows the current identity, exactly as before.
	require.Equal(t, s2, wc.SessionID)
}

// --- identity namespace -----------------------------------------------------

// The current identity is MPM-owned, so the previous identity must be
// MPM-owned too. A framework-owned or legacy id sitting in the handoff
// must not be promoted into the MPM namespace just because it is the
// newest row.
func TestWakeContext_PreviousSession_NeverMixesNamespaces(t *testing.T) {
	dm, root := previousHandoffDM(t)
	seedActiveMPMSession(t, root, "mpm-s2")

	t.Setenv("MPM_PROVENANCE_FRAMEWORK_SESSION_ID", "host-current-uuid")

	// Newest row: framework + legacy identity, no MPM identity.
	insertHandoffRow(t, dm, "h-legacy", "legacy-sess", "", "host-old-uuid", 1700000900, false)
	// Prior MPM lifecycle.
	insertHandoffRow(t, dm, "h-s1", "", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-s2", "", "mpm-s2", "", 1700000200, false)

	wc, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)

	require.Equal(t, "mpm-s1", wc.SessionPreviousID)
	require.NotEqual(t, "host-old-uuid", wc.SessionPreviousID,
		"framework_session_id must not leak into SessionPreviousID")
	require.NotEqual(t, "legacy-sess", wc.SessionPreviousID,
		"legacy session_id must not leak into SessionPreviousID")

	// FrameworkSessionID keeps its own field, unchanged.
	require.Equal(t, "host-current-uuid", wc.FrameworkSessionID)
	require.Equal(t, "mpm-s2", wc.SessionCurrentID)
}

// --- definition of "previous" ----------------------------------------------

// The newest handoff often belongs to the CURRENT lifecycle, because
// an operator may close a session out without rotating yet. Reporting
// that row as "previous" would tell the agent it is its own ancestor.
func TestWakeContext_PreviousSession_LatestHandoffIsCurrentIdentity(t *testing.T) {
	dm, root := previousHandoffDM(t)
	seedActiveMPMSession(t, root, "mpm-s2")

	insertHandoffRow(t, dm, "h-s1", "", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-s2", "", "mpm-s2", "", 1700000200, false)

	wc, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.Equal(t, "mpm-s2", wc.SessionCurrentID)
	require.Equal(t, "mpm-s1", wc.SessionPreviousID)
	require.NotEqual(t, wc.SessionCurrentID, wc.SessionPreviousID,
		"the current lifecycle is never its own previous session")
}

// Several closeouts inside ONE lifecycle are still one session. The
// previous identity is the distinct lifecycle, and its ended_at is the
// latest closeout within it.
func TestWakeContext_PreviousSession_MultipleCloseoutsOneLifecycle(t *testing.T) {
	dm, root := previousHandoffDM(t)
	seedActiveMPMSession(t, root, "mpm-s2")

	insertHandoffRow(t, dm, "h-s1-a", "", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-s1-b", "", "mpm-s1", "", 1700000150, false)
	insertHandoffRow(t, dm, "h-s1-c", "", "mpm-s1", "", 1700000180, false)
	insertHandoffRow(t, dm, "h-s2", "", "mpm-s2", "", 1700000200, false)

	wc, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.Equal(t, "mpm-s1", wc.SessionPreviousID)
	require.Equal(t, int64(1700000180), wc.SessionPreviousEndedAt,
		"ended_at is the latest closeout of the prior lifecycle, not its first")
}

// When every handoff belongs to the current lifecycle there is no
// previous session. Empty is the honest answer.
func TestWakeContext_PreviousSession_OnlyCurrentLifecycleHandoffs(t *testing.T) {
	dm, root := previousHandoffDM(t)
	seedActiveMPMSession(t, root, "mpm-s2")

	insertHandoffRow(t, dm, "h-s2-a", "", "mpm-s2", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-s2-b", "", "mpm-s2", "", 1700000200, false)

	wc, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.Equal(t, "mpm-s2", wc.SessionCurrentID)
	require.Empty(t, wc.SessionPreviousID)
	require.Zero(t, wc.SessionPreviousEndedAt)
}

// --- read-state independence ------------------------------------------------

// Identity must not be a function of delivery state. Deriving it from
// the unread handoff would erase the previous session's identity on the
// very wake that consumes its handoff — the second read would report a
// different previous session than the first.
func TestWakeContext_PreviousSession_ReadOnlyConsumeReadOnlyAgree(t *testing.T) {
	dm, root := previousHandoffDM(t)
	seedActiveMPMSession(t, root, "mpm-s2")

	insertHandoffRow(t, dm, "h-s1", "", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-s2", "", "mpm-s2", "", 1700000200, false)

	before, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.NotNil(t, before.LastHandoff, "the unread handoff is surfaced on the peek")

	during, err := dm.GatherWakeContext()
	require.NoError(t, err)

	after, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)

	// The only difference is the handoff delivery: the consuming read
	// marks the newest handoff read, so it is not surfaced again. The
	// next-oldest unread handoff is what a later peek legitimately sees.
	require.NotNil(t, during.LastHandoff, "consuming read delivers the handoff")
	require.Equal(t, "h-s2", during.LastHandoff.ID)
	require.NotNil(t, after.LastHandoff)
	require.NotEqual(t, "h-s2", after.LastHandoff.ID,
		"a consumed handoff is never re-delivered")
	require.Equal(t, "h-s1", after.LastHandoff.ID,
		"the next read surfaces the next-oldest unread handoff")

	// Previous identity is identical across all three.
	for name, wc := range map[string]WakeContextData{
		"before": before, "during": during, "after": after,
	} {
		require.Equal(t, "mpm-s1", wc.SessionPreviousID, "%s: previous identity", name)
		require.Equal(t, int64(1700000100), wc.SessionPreviousEndedAt, "%s: previous ended_at", name)
		require.Equal(t, "mpm-s2", wc.SessionCurrentID, "%s: current identity", name)
	}
}

// --- fresh / legacy compatibility matrix -----------------------------------

// The full matrix, one case per row. Each case asserts all five
// identity fields together so a namespace slip is visible in one place.
func TestWakeContext_PreviousSession_CompatibilityMatrix(t *testing.T) {
	cases := []struct {
		name string
		// setup mutates the fixture; want* are the expected fields.
		setup       func(t *testing.T, dm *DatabaseManager)
		wantCurrent string
		wantPrev    string
		wantPrevEnd int64
		wantMPM     string
		wantFW      string
	}{
		{
			name:  "fresh workspace, no session at all",
			setup: func(*testing.T, *DatabaseManager) {},
		},
		{
			name: "active MPM session, no handoffs",
			// active.json is seeded from wantMPM by the harness; the
			// point of this case is that no handoff rows exist.
			setup:       func(*testing.T, *DatabaseManager) {},
			wantCurrent: "mpm-solo", wantMPM: "mpm-solo",
		},
		{
			name: "previous MPM handoff + current MPM session",
			setup: func(t *testing.T, dm *DatabaseManager) {
				insertHandoffRow(t, dm, "h1", "", "mpm-s1", "", 1700000100, false)
				insertHandoffRow(t, dm, "h2", "", "mpm-s2", "", 1700000200, false)
			},
			wantCurrent: "mpm-s2", wantPrev: "mpm-s1", wantPrevEnd: 1700000100, wantMPM: "mpm-s2",
		},
		{
			name: "multiple previous MPM handoffs",
			setup: func(t *testing.T, dm *DatabaseManager) {
				insertHandoffRow(t, dm, "h1", "", "mpm-s1", "", 1700000100, false)
				insertHandoffRow(t, dm, "h2", "", "mpm-s2", "", 1700000200, false)
				insertHandoffRow(t, dm, "h3", "", "mpm-s3", "", 1700000300, false)
			},
			wantCurrent: "mpm-s3", wantPrev: "mpm-s2", wantPrevEnd: 1700000200, wantMPM: "mpm-s3",
		},
		{
			name: "latest handoff belongs to current MPM session",
			setup: func(t *testing.T, dm *DatabaseManager) {
				insertHandoffRow(t, dm, "h1", "", "mpm-s1", "", 1700000100, false)
				insertHandoffRow(t, dm, "h2", "", "mpm-s2", "", 1700000200, false)
				insertHandoffRow(t, dm, "h3", "", "mpm-s2", "", 1700000300, false)
			},
			wantCurrent: "mpm-s2", wantPrev: "mpm-s1", wantPrevEnd: 1700000100, wantMPM: "mpm-s2",
		},
		{
			name: "historical handoff with NULL mpm_session_id",
			setup: func(t *testing.T, dm *DatabaseManager) {
				insertHandoffRow(t, dm, "h-null", "legacy-only", "", "", 1700000900, false)
				insertHandoffRow(t, dm, "h1", "", "mpm-s1", "", 1700000100, false)
				insertHandoffRow(t, dm, "h2", "", "mpm-s2", "", 1700000200, false)
			},
			wantCurrent: "mpm-s2", wantPrev: "mpm-s1", wantPrevEnd: 1700000100, wantMPM: "mpm-s2",
		},
		{
			name: "legacy-only session_id mode",
			setup: func(t *testing.T, dm *DatabaseManager) {
				insertHandoffRow(t, dm, "h-a", "sess-a", "", "", 1700000100, false)
				insertHandoffRow(t, dm, "h-b", "sess-b", "", "", 1700000200, false)
				insertSessionsRow(t, dm, "sess-b", 1700000300)
			},
			wantCurrent: "sess-b", wantPrev: "sess-a", wantPrevEnd: 1700000100,
		},
		{
			name: "handoff with NULL legacy session_id but valid mpm_session_id",
			setup: func(t *testing.T, dm *DatabaseManager) {
				insertHandoffRow(t, dm, "h-nolegacy", "", "mpm-s1", "", 1700000100, false)
				insertHandoffRow(t, dm, "h2", "", "mpm-s2", "", 1700000200, false)
			},
			wantCurrent: "mpm-s2", wantPrev: "mpm-s1", wantPrevEnd: 1700000100, wantMPM: "mpm-s2",
		},
		{
			name: "framework_session_id present but no valid previous MPM identity",
			setup: func(t *testing.T, dm *DatabaseManager) {
				insertHandoffRow(t, dm, "h-fw", "", "", "host-uuid", 1700000100, false)
				insertHandoffRow(t, dm, "h2", "", "mpm-s2", "", 1700000200, false)
			},
			wantCurrent: "mpm-s2", wantMPM: "mpm-s2", wantFW: "host-current-uuid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm, root := previousHandoffDM(t)
			t.Setenv("MPM_PROVENANCE_FRAMEWORK_SESSION_ID", tc.wantFW)
			tc.setup(t, dm)
			// The active MPM identity is what makes SessionCurrentID an
			// MPM-owned id; a case with no wantMPM is legacy mode and
			// must leave active.json absent.
			if tc.wantMPM != "" {
				seedActiveMPMSession(t, root, tc.wantMPM)
			}

			wc, err := dm.GatherWakeContextReadOnly()
			require.NoError(t, err)

			require.Equal(t, tc.wantCurrent, wc.SessionCurrentID, "SessionCurrentID")
			require.Equal(t, tc.wantPrev, wc.SessionPreviousID, "SessionPreviousID")
			require.Equal(t, tc.wantPrevEnd, wc.SessionPreviousEndedAt, "SessionPreviousEndedAt")
			require.Equal(t, tc.wantMPM, wc.MPMSessionID, "MPMSessionID")
			require.Equal(t, tc.wantFW, wc.FrameworkSessionID, "FrameworkSessionID")
			require.Equal(t, wc.SessionCurrentID, wc.SessionID,
				"the legacy alias always mirrors the resolved current identity")
		})
	}
}

// A workspace whose only identity lives in the dormant sessions table
// has no MPM-owned identity, so the MPM namespace must stay empty
// rather than borrowing the legacy one.
func TestWakeContext_PreviousSession_LegacyModeDoesNotMintMPMIdentity(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h-a", "sess-a", "", "", 1700000100, false)
	insertHandoffRow(t, dm, "h-b", "sess-b", "", "", 1700000200, false)
	insertSessionsRow(t, dm, "sess-b", 1700000300)

	wc, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.Empty(t, wc.MPMSessionID, "legacy mode does not invent an MPM-owned identity")
	require.Equal(t, "sess-b", wc.SessionCurrentID)
	require.Equal(t, "sess-a", wc.SessionPreviousID)
}

// --- no allocation on read ---------------------------------------------------

// Gathering wake context is a read. It must not create active.json on a
// fresh workspace merely to have something to compare against, and it
// must not allocate or rotate an identity.
func TestWakeContext_PreviousSession_DoesNotAllocateOnFreshWorkspace(t *testing.T) {
	dm, root := previousHandoffDM(t)
	activePath := filepath.Join(root, "active.json")

	require.NoFileExists(t, activePath, "precondition: no active.json")

	for i := 0; i < 3; i++ {
		_, err := dm.GatherWakeContextReadOnly()
		require.NoError(t, err)
		require.NoFileExists(t, activePath,
			"wake context must not allocate a session identity on a fresh workspace")
	}
	require.Empty(t, CurrentMPMSessionID())

	_, err := dm.GatherWakeContext()
	require.NoError(t, err)
	require.NoFileExists(t, activePath,
		"the consuming path allocates no identity either")
}

// The previous-session lookup performs no writes of its own. On the
// consuming path the handoff read marker is the only permitted
// mutation, and it is not the previous-session lookup's doing.
func TestWakeContext_PreviousSession_LookupPerformsNoWrites(t *testing.T) {
	dm, root := previousHandoffDM(t)
	seedActiveMPMSession(t, root, "mpm-s2")

	insertHandoffRow(t, dm, "h1", "", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h2", "", "mpm-s2", "", 1700000200, false)

	var rowsBefore int
	require.NoError(t, dm.db.QueryRow(`SELECT COUNT(*) FROM session_handoffs`).Scan(&rowsBefore))

	// Read-only path: nothing at all may change.
	wc, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.Equal(t, "mpm-s1", wc.SessionPreviousID)

	var rowsAfter int
	require.NoError(t, dm.db.QueryRow(`SELECT COUNT(*) FROM session_handoffs`).Scan(&rowsAfter))
	require.Equal(t, rowsBefore, rowsAfter)

	var readRows int
	require.NoError(t, dm.db.QueryRow(`SELECT COUNT(*) FROM session_handoffs WHERE read_at IS NOT NULL`).Scan(&readRows))
	require.Zero(t, readRows, "the read-only path marks nothing read")
}

// --- wire-format and payload contracts --------------------------------------

// The wire version guard: these two fields already existed with a
// documented meaning, so making them non-empty is not a schema change.
// A consumer that handled the documented empty case still handles the
// populated one. No rename, no removal, no type change — the version
// stays put. This test fails if anyone bumps it to "fix" the change.
func TestWakeContext_PreviousSession_ContextVersionUnchanged(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertHandoffRow(t, dm, "h1", "", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h2", "", "mpm-s2", "", 1700000200, false)

	wc, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.Equal(t, "wake-context-v5", wc.ContextVersion,
		"populating pre-existing fields is not a non-additive wire change")
	require.Equal(t, wakeContextCurrentVersion, wc.ContextVersion,
		"ContextVersion must track the single declared constant")
}

// A populated identity is real bytes in an existing field. The payload
// must stay inside the cap, and a normal gather must not trip any
// truncation tier.
func TestWakeContext_PreviousSession_PayloadStaysWithinCap(t *testing.T) {
	require.Equal(t, 32*1024, MaxWakeContextBytes, "the cap is a contract, not a tunable")

	dm, root := previousHandoffDM(t)
	seedActiveMPMSession(t, root, "mpm-fedcba9876543210fedcba9876543210")
	insertHandoffRow(t, dm, "h1", "", "mpm-0123456789abcdef0123456789abcdef", "", 1700000100, false)
	insertHandoffRow(t, dm, "h2", "", "mpm-fedcba9876543210fedcba9876543210", "", 1700000200, false)

	wc, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.Equal(t, "mpm-0123456789abcdef0123456789abcdef", wc.SessionPreviousID)

	b, err := EnforceSizeLimit(&wc)
	require.NoError(t, err)
	require.LessOrEqual(t, len(b), MaxWakeContextBytes)
	require.False(t, wc.AvailableSkillsTruncated)
	require.False(t, wc.RecentActivityTruncated)
	require.False(t, wc.RecentTopicsTruncated)

	// The populated values are on the wire, not dropped by the cap.
	var wire WakeContextData
	require.NoError(t, json.Unmarshal(b, &wire))
	require.Equal(t, "mpm-0123456789abcdef0123456789abcdef", wire.SessionPreviousID)
	require.Equal(t, int64(1700000100), wire.SessionPreviousEndedAt)
}

// --- existing behaviour is untouched ---------------------------------------

// Everything the previous-session change could plausibly have disturbed
// about the rest of the identity surface, asserted together.
func TestWakeContext_PreviousSession_ExistingIdentityBehaviourUnchanged(t *testing.T) {
	dm, root := previousHandoffDM(t)
	seedActiveMPMSession(t, root, "mpm-s2")

	t.Setenv("MPM_PROVENANCE_FRAMEWORK_SESSION_ID", "host-abc")
	insertHandoffRow(t, dm, "h1", "", "mpm-s1", "", 1700000100, false)
	insertHandoffRow(t, dm, "h2", "", "mpm-s2", "", 1700000200, false)

	wc, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)

	require.Equal(t, "mpm-s2", wc.MPMSessionID)
	require.Equal(t, "mpm-s2", wc.SessionCurrentID)
	require.Equal(t, "mpm-s2", wc.SessionID, "legacy alias mirrors the MPM identity")
	require.Equal(t, "host-abc", wc.FrameworkSessionID)
	require.Equal(t, wakeContextCurrentVersion, wc.ContextVersion)

	// Non-nil empty slices survive. (RecentTopics and
	// RecentMilestones are pre-existingly nil on a fresh DB — the
	// invariant-3 re-init is overwritten by query helpers that return
	// nil. That is an unrelated, pre-existing inconsistency and is
	// deliberately not asserted on here.)
	require.NotNil(t, wc.RecentMemories)
	require.NotNil(t, wc.OverdueWakes)
	require.NotNil(t, wc.OpenWorks)
	require.NotNil(t, wc.CompletedWorks)
	require.NotNil(t, wc.RecentActivity)
	require.NotNil(t, wc.AvailableSkills)
	require.NotNil(t, wc.GlobalRules)

	// Handoff delivery is unchanged: the unread handoff still surfaces.
	require.NotNil(t, wc.LastHandoff)
	require.Equal(t, "h2", wc.LastHandoff.ID)
	_, err = dm.GetHandoffByID("h2")
	require.NoError(t, err, "the peeked handoff is still on the substrate")
}

// No trustworthy prior session means empty, never a synthesized
// timestamp. A previous ended_at is only ever a real handoff's
// ended_at — not the next session's start, and not sessions.created_at.
func TestWakeContext_PreviousSession_NoSynthesizedTimestamps(t *testing.T) {
	dm, _ := previousHandoffDM(t)
	insertSessionsRow(t, dm, "sess-solo", 1700000500)

	wc, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.Equal(t, "sess-solo", wc.SessionCurrentID)
	require.Empty(t, wc.SessionPreviousID)
	require.Zero(t, wc.SessionPreviousEndedAt,
		"sessions.created_at is a start timestamp, not an end — never reused as one")
}

// The lookup reports "no previous" through sql.ErrNoRows, matching the
// other handoff readers. A nil manager is a programming error.
func TestWakeContext_PreviousSession_NoRowsPathIsNotAnError(t *testing.T) {
	dm, _ := previousHandoffDM(t)

	_, err := dm.GetPreviousMPMSessionHandoff("mpm-anything")
	require.ErrorIs(t, err, sql.ErrNoRows)

	wc, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.Empty(t, wc.SessionPreviousID)
}
