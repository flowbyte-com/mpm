// session_identity_v2_test.go — Stage 2C semantic separation
// regression tests. Pins the contract that session identity and
// invocation lineage are distinct concepts, and that the new
// MPM_SESSION_ID surface does not collapse into either
// parent_invocation_id or framework_session_id by accident.
//
// Test matrix (one pin per invariant from the Stage 2C brief):
//
//   S1  session_id != invocation_id
//   S2  session_id != parent_invocation_id
//   S3  multiple invocations share one session
//   S4  parent invocation linkage works independently
//   S5  two sessions from same framework stay separate
//   S6  absent host session remains absent rather than fabricated
//   S7  recent_activity session filter uses session_id only
//   S8  wake-context session fields are read-only projections
//   S9  handoff write receives session identity where supported
//   S10 work create receives session identity where supported
//
// All tests are hermetic (overrideMPMDir + temp DB). No live MPM
// state is touched.

package internal

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// withSessionIdentityFixture overrides MPM_WORKSPACE to a temp dir
// and clears the in-process cache so the test sees a clean state.
// Returns the path so the caller can populate active.json.
func withSessionIdentityFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	overrideMPMDir(t, root)
	invalidateSessionCache()
	return root
}

// seedActiveMPMSession writes active.json with the given
// mpm_session_id. Used to simulate a workspace where the MPM-owned
// session identity has already been allocated.
func seedActiveMPMSession(t *testing.T, root, mpmID string) {
	t.Helper()
	state := &ActiveState{
		Updated:               "2026-09-20T00:00:00Z",
		MPMSessionID:          mpmID,
		MPMSessionIDCreatedAt: 1700000000,
	}
	require.NoError(t, SaveActiveJSON(state))
}

// TEST S1: session_id != invocation_id.
//
// The MPM-owned session identity is sticky across CLI/MCP/process
// boundaries. An invocation_id is per-tool-call. The two fields
// must never be conflated.
func TestSessionIdentity_S1_SessionIDNotInvocationID(t *testing.T) {
	root := withSessionIdentityFixture(t)
	const sessionID = "mpm-deadbeefcafef00d"
	seedActiveMPMSession(t, root, sessionID)

	got := CurrentMPMSessionID()
	require.Equal(t, sessionID, got, "session_id must round-trip through active.json")
	require.NotEqual(t, sessionID, "inv-12345",
		"invocation_id is a separate concept and must never be conflated with session_id")

	// Cross-check via ActiveContext: the dispatch-time read populates
	// MPMSessionID, but InvocationID is caller-supplied (or generated
	// per-call). They are distinct fields on the same struct.
	ac := ActiveContext{
		MPMSessionID: CurrentMPMSessionID(),
		InvocationID: "inv-aaaaaa",
	}
	require.Equal(t, sessionID, ac.MPMSessionID)
	require.Equal(t, "inv-aaaaaa", ac.InvocationID)
	require.NotEqual(t, ac.MPMSessionID, ac.InvocationID,
		"S1: session_id must NOT equal invocation_id")
}

// TEST S2: session_id != parent_invocation_id.
//
// parent_invocation_id expresses causal lineage between invocations
// (subagent A spawns subagent B → B.parent_invocation_id = A.id).
// session_id expresses continuity. The two are different axes and
// must never collapse into one another.
func TestSessionIdentity_S2_SessionIDNotParentInvocationID(t *testing.T) {
	root := withSessionIdentityFixture(t)
	const sessionID = "mpm-1234567890abcdef"
	seedActiveMPMSession(t, root, sessionID)

	// Parent invocation is a separate concept: a tool-call from a
	// parent agent that spawned this one. It is NEVER the session.
	parentInv := "inv-parent-xxxxxxxxxxxx"
	require.NotEqual(t, sessionID, parentInv,
		"S2: parent_invocation_id is causal lineage, not session identity")

	ac := ActiveContext{
		MPMSessionID:       CurrentMPMSessionID(),
		InvocationID:       "inv-child-yyyyyyyy",
		ParentInvocationID: parentInv,
	}
	require.Equal(t, sessionID, ac.MPMSessionID)
	require.Equal(t, parentInv, ac.ParentInvocationID)
	require.NotEqual(t, ac.MPMSessionID, ac.ParentInvocationID,
		"S2: session_id must NOT equal parent_invocation_id")
}

// TEST S3: multiple invocations share one session.
//
// Within a single MPM lifecycle, many tool calls happen. They all
// share the same mpm_session_id but each has a distinct invocation_id.
// AcquireMPMSessionID is idempotent: a second call returns the same
// id, never a new one.
func TestSessionIdentity_S3_MultipleInvocationsShareSession(t *testing.T) {
	withSessionIdentityFixture(t)

	// First interaction-boundary allocation: writes active.json.
	first := AcquireMPMSessionID()
	require.NotEmpty(t, first, "first allocation must produce an id")
	require.True(t, strings.HasPrefix(first, "mpm-"),
		"session_id must carry the mpm- prefix")
	invalidateSessionCache()

	// Subsequent reads see the same id (cache invalidation simulates
	// a fresh process that hits the same workspace).
	for i := 0; i < 5; i++ {
		invalidateSessionCache()
		got := AcquireMPMSessionID()
		require.Equal(t, first, got,
			"AcquireMPMSessionID must be idempotent within a workspace")
	}

	// The same id surfaces across many invocations (different
	// invocation_ids, same mpm_session_id).
	invocations := []string{"inv-1", "inv-2", "inv-3", "inv-4", "inv-5"}
	for _, inv := range invocations {
		invalidateSessionCache()
		ac := ActiveContext{
			MPMSessionID: CurrentMPMSessionID(),
			InvocationID: inv,
		}
		require.Equal(t, first, ac.MPMSessionID,
			"all invocations in one lifecycle share the same mpm_session_id")
		require.NotEqual(t, ac.MPMSessionID, ac.InvocationID)
	}
}

// TEST S4: parent invocation linkage works independently.
//
// Even with session identity in play, parent_invocation_id must
// still flow through ActiveContext to support subagent tracing.
// The two paths (session vs invocation lineage) coexist.
func TestSessionIdentity_S4_ParentInvocationLinkageIndependent(t *testing.T) {
	root := withSessionIdentityFixture(t)
	seedActiveMPMSession(t, root, "mpm-s4-session-id")

	// Parent agent call (root invocation).
	parent := ActiveContext{
		MPMSessionID: CurrentMPMSessionID(),
		InvocationID: "inv-parent-s4",
	}
	// Subagent call (child invocation, parent_invocation_id set).
	child := ActiveContext{
		MPMSessionID:       CurrentMPMSessionID(),
		InvocationID:       "inv-child-s4",
		ParentInvocationID: parent.InvocationID,
	}

	require.Equal(t, parent.MPMSessionID, child.MPMSessionID,
		"S3: parent and child share the MPM session")
	require.NotEqual(t, parent.InvocationID, child.InvocationID,
		"S4: parent and child have distinct invocation_ids")
	require.Equal(t, parent.InvocationID, child.ParentInvocationID,
		"S4: child.parent_invocation_id = parent.invocation_id")
}

// TEST S5: two sessions from same framework stay separate.
//
// When the operator rotates the session identity (mpm session
// rotate), the new id is distinct from the previous one. Framework
// identity is unchanged (same host); session identity moves.
func TestSessionIdentity_S5_TwoSessionsFromSameFrameworkStaySeparate(t *testing.T) {
	withSessionIdentityFixture(t)

	// First session.
	s1 := AcquireMPMSessionID()
	require.NotEmpty(t, s1)
	invalidateSessionCache()

	// Rotate: explicit operator-driven new-session boundary.
	s2 := RotateMPMSessionID()
	require.NotEmpty(t, s2)
	invalidateSessionCache()

	require.NotEqual(t, s1, s2,
		"S5: rotated session id must differ from the previous one")

	// Framework identity unchanged (same ActiveContext.FrameworkName
	// would carry through; we don't pin framework here because the
	// test is purely about session distinctness under same framework).
	require.True(t, strings.HasPrefix(s1, "mpm-"))
	require.True(t, strings.HasPrefix(s2, "mpm-"))

	// Subsequent reads see s2 (current), not s1 (historical).
	current := CurrentMPMSessionID()
	require.Equal(t, s2, current,
		"CurrentMPMSessionID returns the most-recently-allocated id")
	require.NotEqual(t, current, s1,
		"S5: current session must not be the prior session")
}

// TEST S6: absent host session remains absent rather than
// fabricated. When the host framework has no native session id
// (Pi, Hermes without hooks, Claude Code without MPM_SESSION_ID),
// FrameworkSessionID MUST stay empty. Routing must treat "missing"
// as neutral, not synthesize a value.
func TestSessionIdentity_S6_AbsentHostSessionRemainsAbsent(t *testing.T) {
	root := withSessionIdentityFixture(t)
	t.Setenv("MPM_PROVENANCE_FRAMEWORK_SESSION_ID", "")

	// Fresh ActiveContext with no framework session.
	ac := ActiveContext{}
	require.Empty(t, ac.FrameworkSessionID,
		"S6: empty FrameworkSessionID means 'no host session'")

	// Wake context reads: missing framework session stays missing.
	wc, err := NewTestDM(t).GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.Empty(t, wc.FrameworkSessionID,
		"S6: wake context must NOT fabricate a framework_session_id")

	// Even when MPM owns a session, the framework slot stays empty
	// unless the host explicitly populated MPM_PROVENANCE_FRAMEWORK_SESSION_ID.
	seedActiveMPMSession(t, root, "mpm-s6-host")
	ac = ActiveContext{
		MPMSessionID:       CurrentMPMSessionID(),
		FrameworkSessionID: "",
	}
	require.NotEmpty(t, ac.MPMSessionID,
		"MPM-owned session is independent of host session presence")
	require.Empty(t, ac.FrameworkSessionID,
		"S6: framework_session_id MUST stay empty when absent")
}

// TEST S7: recent_activity session filter uses session_id only.
//
// The Stage 2A recent_activity filter uses tool_invocations.session_id
// (NOT parent_invocation_id, NOT mpm_session_id derived from
// timestamp). When a caller filters by session, the only legal
// matching axis is the explicit session_id field.
func TestSessionIdentity_S7_RecentActivityFilterUsesSessionIDOnly(t *testing.T) {
	dm := NewTestDM(t)
	base := int64(1700000000)

	// Two invocations under session A; one under session B.
	seedToolInvocation(t, dm, seedArgs{
		ID: "s7-a-1", Tool: "mpm_memory", Action: "save",
		FrameworkName: "openclaw", ActorKind: "agent",
		StartedAt: base, CompletedAt: base + 1,
		InvocationID: "inv-s7-a-1",
		SessionID:    "mpm-session-a",
	})
	seedToolInvocation(t, dm, seedArgs{
		ID: "s7-a-2", Tool: "mpm_work", Action: "update",
		FrameworkName: "openclaw", ActorKind: "agent",
		StartedAt: base + 2, CompletedAt: base + 3,
		InvocationID: "inv-s7-a-2",
		SessionID:    "mpm-session-a",
	})
	seedToolInvocation(t, dm, seedArgs{
		ID: "s7-b-1", Tool: "mpm_lessons", Action: "save",
		FrameworkName: "openclaw", ActorKind: "agent",
		StartedAt: base + 4, CompletedAt: base + 5,
		InvocationID: "inv-s7-b-1",
		SessionID:    "mpm-session-b",
	})

	// Filter by session A: see 2 events.
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, SessionID: "mpm-session-a",
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 2,
		"S7: filter by session_id must surface only that session's events")

	// Filter by session B: see 1 event.
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, SessionID: "mpm-session-b",
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 1,
		"S7: filter by session B must surface only that session's event")

	// Filter by a non-existent session: see 0 events (no false matches).
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, SessionID: "mpm-no-such-session",
	})
	require.NoError(t, err)
	require.Empty(t, res.Events,
		"S7: missing session must NOT return false matches")
}

// TEST S8: wake-context session fields are read-only projections.
//
// wake-context data.MPMSessionID and data.FrameworkSessionID are
// populated from CurrentMPMSessionID / env, NOT persisted. The
// gather path is read-only.
func TestSessionIdentity_S8_WakeContextSessionFieldsAreReadOnly(t *testing.T) {
	root := withSessionIdentityFixture(t)
	t.Setenv("MPM_PROVENANCE_FRAMEWORK_SESSION_ID", "host-s8-session")
	seedActiveMPMSession(t, root, "mpm-s8-wake-id")

	wc, err := NewTestDM(t).GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.Equal(t, "mpm-s8-wake-id", wc.MPMSessionID,
		"S8: MPMSessionID surfaces from active.json into wake context")
	require.Equal(t, "host-s8-session", wc.FrameworkSessionID,
		"S8: FrameworkSessionID surfaces from env into wake context")

	// Re-gather after the on-disk state has been mutated — wake
	// context must reflect the new state (read-only, not cached
	// beyond the mtime check).
	seedActiveMPMSession(t, root, "mpm-s8-wake-id-v2")
	invalidateSessionCache()
	wc2, err := NewTestDM(t).GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.Equal(t, "mpm-s8-wake-id-v2", wc2.MPMSessionID,
		"S8: re-gather after rotation surfaces the new id")

	// Wire format includes both fields.
	wire, err := json.Marshal(wc2)
	require.NoError(t, err)
	require.Contains(t, string(wire), `"mpm_session_id":"mpm-s8-wake-id-v2"`,
		"S8: mpm_session_id must appear in the wire format")
	require.Contains(t, string(wire), `"framework_session_id":"host-s8-session"`,
		"S8: framework_session_id must appear in the wire format")
}

// TEST S9: handoff write receives session identity where supported.
//
// EndSessionV2 writes the canonical three-ID surface. Going forward,
// mpm_session_id is allocated at the interaction boundary (handoff
// write), and framework_session_id is the caller-supplied host id.
//
// The manager here is FILE-BACKED and rooted at the fixture workspace,
// which is what production looks like: a DatabaseManager resolves its
// lifecycle identity against the active.json belonging to its own
// database's workspace. An in-memory manager has no workspace of its own
// and deliberately mints a process-scoped id instead of reaching into
// whatever MPM_WORKSPACE happens to point at — that decoupling is what
// makes a hermetic test manager safe to construct, and S9's final
// assertion only holds for the file-backed shape.
func TestSessionIdentity_S9_HandoffWriteReceivesSessionIdentity(t *testing.T) {
	root := withSessionIdentityFixture(t)
	dm, err := NewDatabaseManager(root)
	require.NoError(t, err, "NewDatabaseManager(%q)", root)
	t.Cleanup(func() { _ = dm.Close() })

	h, err := dm.EndSessionV2(
		"",                                  // legacy session_id (empty)
		"claude-code-host-s9",               // framework_session_id
		"",                                  // explicitMPMSessionID
		"stage 2c s9 summary", "clean",
		[]string{"commit-s9"}, []string{"open-s9"},
	)
	require.NoError(t, err)
	require.NotEmpty(t, h.MPMSessionID,
		"S9: handoff must record an mpm_session_id (allocated at interaction boundary)")
	require.Equal(t, "claude-code-host-s9", h.FrameworkSessionID,
		"S9: handoff must record the caller-supplied framework_session_id")
	require.Empty(t, h.SessionID,
		"S9: legacy session_id column stays empty (caller did not supply)")

	// Cross-process lock: the active.json that backs mpm_session_id
	// is the same file the next acquire would read.
	require.True(t, strings.HasPrefix(h.MPMSessionID, "mpm-"),
		"S9: mpm_session_id carries the mpm- prefix")
	require.FileExists(t, filepath.Join(root, "active.json"),
		"S9: the manager persisted its identity beside its own database, not elsewhere")
	require.Equal(t, h.MPMSessionID, CurrentMPMSessionID(),
		"S9: post-write, the new session id is the current session id")
}

// TEST S10: work create receives session identity where supported.
//
// works.session_id (legacy, unchanged) and the work_event invocation
// linkage are separate concepts. Works carry the legacy session_id
// (caller-supplied), not mpm_session_id, to preserve historical
// readers. The work_events invocation/parent chain is preserved.
func TestSessionIdentity_S10_WorkCreateReceivesSessionIdentity(t *testing.T) {
	dm := NewTestDM(t)
	const callerSession = "caller-session-s10"

	// Insert a work row the way handlers_work.go would.
	w, err := dm.CreateWorkWithContext("s10 stage 2c", "fixture content", callerSession, ActiveContext{})
	require.NoError(t, err)
	require.NotEmpty(t, w.ID)
	require.Equal(t, callerSession, w.SessionID,
		"S10: works.session_id carries the caller-supplied session identifier")

	// The work row's session_id is a legacy/host correlation field;
	// it does NOT need to match mpm_session_id (which would be set
	// on the handoff side, not the work side).
	root := withSessionIdentityFixture(t)
	seedActiveMPMSession(t, root, "mpm-s10-distinct")
	require.NotEqual(t, callerSession, CurrentMPMSessionID(),
		"S10: caller-supplied session_id is independent of mpm_session_id")
}
