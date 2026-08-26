// f18_wake_handoff_regression_test.go — F18 regression.
//
// Audit finding F18: `read_wake_context format=system-prompt` omitted
// last_handoff. Root cause was NOT the prose renderer (it renders
// d.LastHandoff) — it was CONSUMPTION BY SIDE EFFECT: `mpm status`
// (dashboard) and `mpm continue` called GatherWakeContext() for their own
// rendering, which marks the latest unread handoff read. By the time the
// agent asked for its wake context, no unread handoff remained.
package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestF18_ReadOnlyGatherPeeksWithoutConsuming(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	_, err := dm.EndSession("sess-f18-a", "continuity summary A", "clean", nil, nil)
	require.NoError(t, err)

	// Presentation gather: handoff must be present AND still unread after.
	data, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.NotNil(t, data.LastHandoff, "read-only wake must include the unread handoff")
	assert.Equal(t, "continuity summary A", data.LastHandoff.Summary)

	stillUnread, err := dm.GetLatestUnreadHandoff()
	require.NoError(t, err)
	require.NotNil(t, stillUnread, "read-only gather must NOT consume the handoff")
}

func TestF18_ConsumeSemanticsPreservedForWakeReads(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	_, err := dm.EndSession("sess-f18-b", "continuity summary B", "clean", nil, nil)
	require.NoError(t, err)

	first, err := dm.GatherWakeContext()
	require.NoError(t, err)
	require.NotNil(t, first.LastHandoff)

	// Read-once: a second consuming wake does not re-deliver.
	second, err := dm.GatherWakeContext()
	require.NoError(t, err)
	assert.Nil(t, second.LastHandoff, "consuming wake must deliver the handoff exactly once")
}

func TestF18_SystemPromptFormatIncludesLastHandoff(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	_, err := dm.EndSession("sess-f18-c", "the richest continuity field survived", "interrupted",
		[]string{"ship the fix"}, []string{"why did the test flake?"})
	require.NoError(t, err)

	prose, err := dm.ReadWakeContext()
	require.NoError(t, err)
	assert.Contains(t, prose, "Previous Session Handoff")
	assert.Contains(t, prose, "the richest continuity field survived")
	// F13: open questions/commitments round-trip into the prose projection.
	assert.Contains(t, prose, "ship the fix")
	assert.Contains(t, prose, "why did the test flake?")

	// Absent handoff data is represented cleanly (no handoff block, no crash).
	dm2 := newTestDM(t)
	defer dm2.Close()
	empty, err := dm2.ReadWakeContext()
	require.NoError(t, err)
	assert.NotContains(t, empty, "Previous Session Handoff")
}

func TestF18_DashboardStyleReadDoesNotStarveAgentWake(t *testing.T) {
	// The exact audited sequence: presentation surface runs first,
	// agent wake runs second and must STILL see the handoff.
	dm := newTestDM(t)
	defer dm.Close()

	_, err := dm.EndSession("sess-f18-d", "dashboard must not eat me", "clean", nil, nil)
	require.NoError(t, err)

	// What cmd/mpm main.go dashboard now does.
	dashData, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	require.NotNil(t, dashData.LastHandoff)

	// What the agent's read_wake_context does.
	wakeData, err := dm.GatherWakeContext()
	require.NoError(t, err)
	require.NotNil(t, wakeData.LastHandoff, "agent wake starved by presentation surface (F18)")
	assert.Equal(t, "dashboard must not eat me", wakeData.LastHandoff.Summary)
}
