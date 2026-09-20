// recent_activity_session_identity_filter_test.go — Stage 2C.1
// multi-session filter test. Seeds four invocations across two MPM
// sessions × three framework sessions and verifies each filter
// dimension returns the correct set without cross-field fallback.

package internal

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRecentActivity_FilterIdentityDimensions is the matrix test
// from Stage 2C.1 step 20:
//
//   MPM session M1:
//     OpenClaw framework session F1 -> events A/B
//     OpenClaw framework session F2 -> event C
//
//   MPM session M2:
//     OpenClaw F3 -> event D
//
// Asserts:
//   filter mpm_session_id=M1        -> A,B,C
//   filter framework_session_id=F1  -> A,B
//   filter mpm_session_id=M2        -> D
//   filter session_id=<legacy>      uses only legacy column
//   no cross-field fallback
func TestRecentActivity_FilterIdentityDimensions(t *testing.T) {
	dm := NewTestDM(t)

	// MPM session M1
	seedInvocationIdentity(t, dm, "evt-A", "p-1", "M1", "F1", "inv-A", "openclaw", 1700000900)
	seedInvocationIdentity(t, dm, "evt-B", "p-2", "M1", "F1", "inv-B", "openclaw", 1700000901)
	seedInvocationIdentity(t, dm, "evt-C", "p-3", "M1", "F2", "inv-C", "openclaw", 1700000902)
	// MPM session M2
	seedInvocationIdentity(t, dm, "evt-D", "p-4", "M2", "F3", "inv-D", "openclaw", 1700000903)

	// Filter mpm_session_id=M1 → A, B, C
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, MPMSessionID: "M1",
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 3,
		"filter by mpm_session_id=M1 must surface events A, B, C")

	gotIDs := map[string]bool{}
	for _, ev := range res.Events {
		gotIDs[ev.ID] = true
		require.Equal(t, "M1", ev.MPMSessionID)
	}
	require.True(t, gotIDs["evt-A"] && gotIDs["evt-B"] && gotIDs["evt-C"],
		"M1 filter must include A, B, C")
	require.False(t, gotIDs["evt-D"],
		"M1 filter must NOT include D")

	// Filter framework_session_id=F1 → A, B
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, FrameworkSessionID: "F1",
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 2,
		"filter by framework_session_id=F1 must surface events A, B")

	gotIDs = map[string]bool{}
	for _, ev := range res.Events {
		gotIDs[ev.ID] = true
		require.Equal(t, "F1", ev.FrameworkSessionID)
	}
	require.True(t, gotIDs["evt-A"] && gotIDs["evt-B"])
	require.False(t, gotIDs["evt-C"] || gotIDs["evt-D"],
		"F1 filter must not include C (F2) or D (F3)")

	// Filter mpm_session_id=M2 → D
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, MPMSessionID: "M2",
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 1, "filter by mpm_session_id=M2 must surface only D")
	require.Equal(t, "evt-D", res.Events[0].ID)

	// Filter session_id=p-2 → only B (legacy column, no cross-field fallback)
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, SessionID: "p-2",
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 1, "filter by legacy session_id returns only that process row")
	require.Equal(t, "evt-B", res.Events[0].ID)
	require.Equal(t, "M1", res.Events[0].MPMSessionID,
		"row returned by session_id=p-2 still carries its mpm_session_id (independent columns)")

	// Filter session_id=p-1 → only A
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, SessionID: "p-1",
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	require.Equal(t, "evt-A", res.Events[0].ID)

	// Combined filter: MPM=M1 AND framework_session_id=F2 → only C.
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, MPMSessionID: "M1", FrameworkSessionID: "F2",
	})
	require.NoError(t, err)
	require.Len(t, res.Events, 1,
		"combined MPM + framework filter narrows to a single event")
	require.Equal(t, "evt-C", res.Events[0].ID)

	// Negative case: a non-existent mpm_session_id returns no events.
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, MPMSessionID: "M-NONEXISTENT",
	})
	require.NoError(t, err)
	require.Empty(t, res.Events,
		"non-existent mpm_session_id must not produce false matches")

	// Negative case: a non-existent framework_session_id returns no events.
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit: 50, FrameworkSessionID: "F-NONEXISTENT",
	})
	require.NoError(t, err)
	require.Empty(t, res.Events,
		"non-existent framework_session_id must not produce false matches")
}
