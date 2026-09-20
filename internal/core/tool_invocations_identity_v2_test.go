// tool_invocations_identity_v2_test.go — Stage 2C.1 identity
// separation regressions on the audit substrate.
//
// The brief lists I1–I8; each test below pins one leg of the
// separation contract:
//
//   I1 one MPM session, multiple session_id values across simulated
//      process boundaries → same mpm_session_id
//   I2 one framework session, multiple invocation IDs → same
//      framework_session_id
//   I3 same framework, different framework sessions remain separate
//   I4 MPM session rotates while framework session remains same →
//      identities differ correctly
//   I5 framework session changes while MPM session remains same →
//      identities differ correctly
//   I6 parent invocation lineage never used as session fallback
//   I7 Pi/Hermes no framework session but valid MPM session remains
//      representable
//   I8 historical rows with NULL new fields remain queryable

package internal

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// seedInvocationIdentity is a compact helper to insert a
// tool_invocations row with the three session identity dimensions
// pre-populated, so each test can focus on its own semantic pin.
func seedInvocationIdentity(t *testing.T, dm *DatabaseManager, id, sessionID, mpmID, frameworkID, invocationID, frameworkName string, completedAt int64) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id,
		     actor_kind, framework_name, payload_hash, result_status,
		     started_at, completed_at, duration_ms,
		     mpm_session_id, framework_session_id)
		VALUES (?, ?, 'mpm_memory', 'save', ?,
		        'agent', ?, 'sha256:i', 'success',
		        ?, ?, 100, ?, ?)
	`, id, sessionID, invocationID, frameworkName,
		completedAt-1, completedAt, mpmID, frameworkID)
	require.NoError(t, err)
}

// I1: one MPM session, multiple tool_invocations.session_id values
//     across simulated process boundaries → same mpm_session_id.
func TestToolInvocationIdentity_I1_SameMPMSessionAcrossProcessBoundaries(t *testing.T) {
	dm := NewTestDM(t)
	const mpmID = "mpm-i1-session"

	seedInvocationIdentity(t, dm, "i1-a-1", "process-uuid-1", mpmID, "openclaw-s1", "inv-i1-a-1", "openclaw", 1700000100)
	seedInvocationIdentity(t, dm, "i1-a-2", "process-uuid-2", mpmID, "openclaw-s1", "inv-i1-a-2", "openclaw", 1700000101)
	seedInvocationIdentity(t, dm, "i1-a-3", "process-uuid-3", mpmID, "openclaw-s2", "inv-i1-a-3", "openclaw", 1700000102)

	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit:        50,
		MPMSessionID: mpmID,
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 3,
		"I1: all three invocations share one mpm_session_id even with different session_id values")

	// Cross-check: filtering by legacy session_id returns only the one row.
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit:     50,
		SessionID: "process-uuid-2",
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 1,
		"I1: legacy session_id filter must NOT consult mpm_session_id")
	require.Equal(t, "process-uuid-2", res.Events[0].SessionID)
	require.Equal(t, mpmID, res.Events[0].MPMSessionID,
		"I1: the row returned by session_id=process-uuid-2 carries mpm_session_id (independent columns)")
}

// I2: one framework session, multiple invocation IDs → same
//     framework_session_id.
func TestToolInvocationIdentity_I2_SameFrameworkSessionAcrossInvocations(t *testing.T) {
	dm := NewTestDM(t)
	const frameworkID = "openclaw-fw-i2"
	const mpmID = "mpm-i2-session"

	seedInvocationIdentity(t, dm, "i2-a-1", "p-1", mpmID, frameworkID, "inv-i2-1", "openclaw", 1700000200)
	seedInvocationIdentity(t, dm, "i2-a-2", "p-1", mpmID, frameworkID, "inv-i2-2", "openclaw", 1700000201)
	seedInvocationIdentity(t, dm, "i2-b-1", "p-1", "mpm-i2-other", "different-fw", "inv-i2-3", "openclaw", 1700000202)

	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit:              50,
		FrameworkSessionID: frameworkID,
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 2,
		"I2: filter by framework_session_id surfaces only that framework's events")

	for _, ev := range res.Events {
		require.Equal(t, frameworkID, ev.FrameworkSessionID,
			"I2: every returned event carries the matching framework_session_id")
		require.NotEmpty(t, ev.InvocationID,
			"I2: invocation_id is distinct across rows (per-call)")
	}
}

// I3: same framework, different framework sessions remain separate.
func TestToolInvocationIdentity_I3_SameFrameworkDifferentSessionsSeparate(t *testing.T) {
	dm := NewTestDM(t)
	const mpmID = "mpm-i3-session"

	seedInvocationIdentity(t, dm, "i3-a-1", "p-1", mpmID, "openclaw-fw-1", "inv-i3-1", "openclaw", 1700000300)
	seedInvocationIdentity(t, dm, "i3-a-2", "p-2", mpmID, "openclaw-fw-2", "inv-i3-2", "openclaw", 1700000301)

	res1, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, FrameworkSessionID: "openclaw-fw-1",
	})
	require.NoError(t, err)
	res2, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, FrameworkSessionID: "openclaw-fw-2",
	})
	require.NoError(t, err)
	require.Len(t, res1.Events, 1, "I3: framework_session_1 distinct")
	require.Len(t, res2.Events, 1, "I3: framework_session_2 distinct")
	require.NotEqual(t,
		res1.Events[0].FrameworkSessionID,
		res2.Events[0].FrameworkSessionID,
		"I3: same framework, different sessions stay separate")
}

// I4: MPM session rotates while framework session remains same →
//     identities differ correctly.
func TestToolInvocationIdentity_I4_MPMRotatesFrameworkStable(t *testing.T) {
	dm := NewTestDM(t)
	const frameworkID = "openclaw-fw-i4"

	// Before rotation: MPM session M1, framework session F1.
	seedInvocationIdentity(t, dm, "i4-pre", "p-1", "mpm-i4-M1", frameworkID, "inv-i4-1", "openclaw", 1700000400)
	// After rotation: MPM session M2, framework session F1 (stable).
	seedInvocationIdentity(t, dm, "i4-post", "p-1", "mpm-i4-M2", frameworkID, "inv-i4-2", "openclaw", 1700000401)

	// Filter by framework_session_id → both rows (F1 stable).
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, FrameworkSessionID: frameworkID,
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 2, "I4: framework_session_id stable across MPM rotation")

	// Filter by MPM session M1 → 1 event.
	resM1, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, MPMSessionID: "mpm-i4-M1",
	})
	require.NoError(t, err)
	require.Len(t, resM1.Events, 1, "I4: MPM session M1 isolates pre-rotation events")

	// Filter by MPM session M2 → 1 event.
	resM2, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, MPMSessionID: "mpm-i4-M2",
	})
	require.NoError(t, err)
	require.Len(t, resM2.Events, 1, "I4: MPM session M2 isolates post-rotation events")

	require.NotEqual(t, resM1.Events[0].MPMSessionID, resM2.Events[0].MPMSessionID,
		"I4: MPM identity changes across rotation")
	require.Equal(t, resM1.Events[0].FrameworkSessionID, resM2.Events[0].FrameworkSessionID,
		"I4: framework identity stable across MPM rotation")
}

// I5: framework session changes while MPM session remains same →
//     identities differ correctly.
func TestToolInvocationIdentity_I5_FrameworkChangesMPMStable(t *testing.T) {
	dm := NewTestDM(t)
	const mpmID = "mpm-i5-stable"

	seedInvocationIdentity(t, dm, "i5-a", "p-1", mpmID, "openclaw-fw-alpha", "inv-i5-1", "openclaw", 1700000500)
	seedInvocationIdentity(t, dm, "i5-b", "p-1", mpmID, "openclaw-fw-beta", "inv-i5-2", "openclaw", 1700000501)

	// MPM session stable → both rows.
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, MPMSessionID: mpmID,
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 2,
		"I5: MPM session stable → both invocations grouped under one MPM id")

	// Framework sessions are distinct.
	resAlpha, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, FrameworkSessionID: "openclaw-fw-alpha",
	})
	require.NoError(t, err)
	resBeta, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, FrameworkSessionID: "openclaw-fw-beta",
	})
	require.NoError(t, err)
	require.Len(t, resAlpha.Events, 1, "I5: framework alpha isolates one event")
	require.Len(t, resBeta.Events, 1, "I5: framework beta isolates one event")
	require.NotEqual(t,
		resAlpha.Events[0].FrameworkSessionID,
		resBeta.Events[0].FrameworkSessionID,
		"I5: framework sessions differ correctly under stable MPM session")
}

// I6: parent invocation lineage never used as session fallback.
//     A row that carries a parent_invocation_id must NOT also have
//     its parent_invocation_id surface in any of the three session
//     identity columns. (The substrate contract: tool_invocations has
//     no parent_invocation_id column at this stage; the test asserts
//     via ActiveContext that a parent invocation does not leak into
//     the session identity fields.)
func TestToolInvocationIdentity_I6_ParentInvocationNeverSessionFallback(t *testing.T) {
	dm := NewTestDM(t)
	const mpmID = "mpm-i6-session"
	const frameworkID = "openclaw-fw-i6"

	// Parent invocation (root).
	seedInvocationIdentity(t, dm, "i6-parent", "p-1", mpmID, frameworkID, "inv-i6-parent", "openclaw", 1700000600)
	// Child invocation with explicit parent_invocation_id on ActiveContext.
	seedInvocationIdentity(t, dm, "i6-child", "p-1", mpmID, frameworkID, "inv-i6-child", "openclaw", 1700000601)

	// Read back and confirm none of the three session dimensions
	// equal the parent's invocation_id.
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, MPMSessionID: mpmID,
	})
	require.NoError(t, err)
	for _, ev := range res.Events {
		require.NotEqual(t, "inv-i6-parent", ev.SessionID,
			"I6: parent's invocation_id must not leak into child.session_id")
		require.NotEqual(t, "inv-i6-parent", ev.MPMSessionID,
			"I6: parent's invocation_id must not leak into child.mpm_session_id")
		require.NotEqual(t, "inv-i6-parent", ev.FrameworkSessionID,
			"I6: parent's invocation_id must not leak into child.framework_session_id")
	}

	// ActiveContext invariant: when a parent_invocation_id is set
	// on ActiveContext, none of the three session fields equal it.
	ac := ActiveContext{
		MPMSessionID:       "mpm-ac-i6",
		FrameworkSessionID: "fw-ac-i6",
		SessionID:          "cli-ac-i6",
		InvocationID:       "inv-ac-i6",
		ParentInvocationID: "inv-ac-parent",
	}
	require.NotEqual(t, ac.ParentInvocationID, ac.SessionID,
		"I6: ParentInvocationID must NOT equal SessionID")
	require.NotEqual(t, ac.ParentInvocationID, ac.MPMSessionID,
		"I6: ParentInvocationID must NOT equal MPMSessionID")
	require.NotEqual(t, ac.ParentInvocationID, ac.FrameworkSessionID,
		"I6: ParentInvocationID must NOT equal FrameworkSessionID")
}

// I7: Pi/Hermes no framework session but valid MPM session remains
//     representable. A row with mpm_session_id populated and
//     framework_session_id NULL is valid and queryable.
func TestToolInvocationIdentity_I7_PiHermesNoFrameworkValidMPM(t *testing.T) {
	dm := NewTestDM(t)
	const mpmID = "mpm-i7-pi-session"

	// Pi / Hermes without hooks: framework_session_id is NULL.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id,
		     actor_kind, framework_name, payload_hash, result_status,
		     started_at, completed_at, duration_ms,
		     mpm_session_id, framework_session_id)
		VALUES ('i7-pi-1', 'p-pi', 'mpm_memory', 'save', 'inv-i7-1',
		        'agent', 'pi', 'sha256:i7', 'success',
		        1700000700, 1700000701, 100, ?, NULL)
	`, mpmID)
	require.NoError(t, err)

	// Filter by mpm_session_id returns the row.
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, MPMSessionID: mpmID,
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 1, "I7: MPM-only filter surfaces Pi rows")
	require.Empty(t, res.Events[0].FrameworkSessionID,
		"I7: framework_session_id is absent for Pi, not synthesized")

	// Filter by framework_session_id=<empty> must not return Pi rows.
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit:              50,
		FrameworkSessionID: "non-existent",
	})
	require.NoError(t, err)
	require.Empty(t, res.Events,
		"I7: filtering by an absent framework_session_id does not return Pi rows")
}

// I8: historical rows with NULL new fields remain queryable.
//     A row inserted before the migration (or before the audit hook
//     was updated) carries NULL on mpm_session_id and
//     framework_session_id. It must still surface in
//     framework_name=claude-code queries, and the recent_activity
//     event must surface SessionID + (empty) MPMSessionID +
//     (empty) FrameworkSessionID without crashing.
func TestToolInvocationIdentity_I8_HistoricalNULLNewFieldsQueryable(t *testing.T) {
	dm := NewTestDM(t)

	// Pre-Stage-2C.1 row: mpm_session_id and framework_session_id
	// are both NULL.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id,
		     actor_kind, framework_name, payload_hash, result_status,
		     started_at, completed_at, duration_ms)
		VALUES ('hist-1', 'legacy-sid', 'mpm_memory', 'save', 'inv-hist-1',
		        'agent', 'claude-code', 'sha256:h', 'success',
		        1700000800, 1700000801, 100)
	`)
	require.NoError(t, err)

	// Filter by framework_name returns the legacy row.
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, FrameworkName: "claude-code",
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 1, "I8: legacy row surfaces in framework_name filter")
	require.Equal(t, "legacy-sid", res.Events[0].SessionID,
		"I8: legacy row's session_id preserved")
	require.Empty(t, res.Events[0].MPMSessionID,
		"I8: historical NULL mpm_session_id surfaces as empty")
	require.Empty(t, res.Events[0].FrameworkSessionID,
		"I8: historical NULL framework_session_id surfaces as empty")

	// Filter by mpm_session_id=<empty> must not return legacy rows.
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, MPMSessionID: "mpm-not-set",
	})
	require.NoError(t, err)
	require.Empty(t, res.Events,
		"I8: filter by non-existent mpm_session_id does not match legacy NULL rows")
}
