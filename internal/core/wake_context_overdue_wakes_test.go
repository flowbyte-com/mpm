package internal

// Tests for the wake-context overdue-wakes surface (added 2026-08-13).
//
// Background: read_wake_context was structurally blind to scheduled_wakes
// — the entire scheduled_wakes table was missing from GatherWakeContext,
// not just filtered by kind. This test file pins the contract for the
// patch that wires gatherOverdueWakes into the wake-context surface so
// the gap cannot silently regress.
//
// Tests follow the per-domain pattern established by
// wake_context_audit_test.go / wake_context_milestones_test.go /
// wake_context_epistemic_pressure_test.go: hermetic in-memory DB via
// NewTestDM, direct SQL seeds via dm.db.Exec, contract assertions via
// require.

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedWake inserts a scheduled_wakes row with explicit fired / target_time
// / metadata so tests can pin every input dimension independently. Pass
// fired=1 to model an already-dispatched wake; pass reason="" to model
// the empty-reason defensive-skip branch. Schema enforces reason
// NOT NULL but does NOT enforce non-empty — the gather query's
// `reason != ''` clause is the real defensive guard.
func seedWake(t *testing.T, dm *DatabaseManager, id string, targetTime int64, fired int, kind string, reason string) {
	t.Helper()
	var meta string
	if kind == "" {
		meta = "null"
	} else {
		meta = fmt.Sprintf(`{"kind":"%s"}`, kind)
	}
	_, err := dm.db.Exec(`
		INSERT INTO scheduled_wakes (id, target_time, reason, fired, fired_at, created_by, metadata)
		VALUES (?, ?, ?, ?, NULL, 'test', ?)`,
		id, targetTime, reason, fired, meta)
	require.NoError(t, err)
}

func TestGatherOverdueWakes_IncludesDueUnfired(t *testing.T) {
	dm := NewTestDM(t)
	now := dm.nowUnix(t)
	seedWake(t, dm, "wk-due", now-3600, 0, "reminder", "Re-evaluate X")

	got := dm.gatherOverdueWakes()
	require.Len(t, got, 1)
	require.Equal(t, "wk-due", got[0].ID)
	require.Equal(t, "reminder", got[0].Kind)
	require.Equal(t, int64(3600), got[0].OverdueSecs)
}

func TestGatherOverdueWakes_SkipsFired(t *testing.T) {
	dm := NewTestDM(t)
	now := dm.nowUnix(t)
	seedWake(t, dm, "wk-fired", now-3600, 1, "reminder", "Already fired")
	seedWake(t, dm, "wk-due", now-3600, 0, "reminder", "Still due")

	got := dm.gatherOverdueWakes()
	require.Len(t, got, 1, "fired wake must not surface; unfired due wake must")
	require.Equal(t, "wk-due", got[0].ID)
}

func TestGatherOverdueWakes_SkipsFutureDated(t *testing.T) {
	dm := NewTestDM(t)
	now := dm.nowUnix(t)
	seedWake(t, dm, "wk-future", now+3600, 0, "reminder", "Not yet due")

	got := dm.gatherOverdueWakes()
	require.Empty(t, got, "future-dated wake must not surface even if fired=0")
}

func TestGatherOverdueWakes_OrderedMostOverdueFirst(t *testing.T) {
	dm := NewTestDM(t)
	now := dm.nowUnix(t)
	seedWake(t, dm, "wk-1h", now-3600, 0, "reminder", "One hour overdue")
	seedWake(t, dm, "wk-1d", now-86400, 0, "reminder", "One day overdue")
	seedWake(t, dm, "wk-1m", now-60, 0, "reminder", "One minute overdue")

	got := dm.gatherOverdueWakes()
	require.Len(t, got, 3)
	require.Equal(t, "wk-1d", got[0].ID, "most-overdue first")
	require.Equal(t, "wk-1h", got[1].ID)
	require.Equal(t, "wk-1m", got[2].ID, "least-overdue last")
}

func TestGatherOverdueWakes_CapsAt5(t *testing.T) {
	dm := NewTestDM(t)
	now := dm.nowUnix(t)
	// Seed 7 rows with increasing overdue times — i=0 is least overdue,
	// i=6 is most overdue. gatherOverdueWakes returns the 5 most-overdue,
	// so the result must be (i=6, 5, 4, 3, 2) — i=0 and i=1 are dropped
	// by the cap.
	for i := 0; i < 7; i++ {
		seedWake(t, dm, fmt.Sprintf("wk-cap-%d", i), now-int64(3600*(i+1)), 0, "reminder",
			fmt.Sprintf("Row %d", i))
	}

	got := dm.gatherOverdueWakes()
	require.Len(t, got, 5, "cap must be 5 to keep wake-context payload bounded")
	require.Equal(t, "wk-cap-6", got[0].ID, "most-overdue must be first")
	require.Equal(t, "wk-cap-2", got[4].ID, "5th-most-overdue is the cutoff")

	// Assert the two dropped rows are NOT present.
	seen := map[string]bool{}
	for _, w := range got {
		seen[w.ID] = true
	}
	require.False(t, seen["wk-cap-0"], "least-overdue row must be dropped by the cap")
	require.False(t, seen["wk-cap-1"], "second-least overdue row must be dropped by the cap")
}

func TestGatherOverdueWakes_SkipsEmptyReason(t *testing.T) {
	dm := NewTestDM(t)
	now := dm.nowUnix(t)
	seedWake(t, dm, "wk-empty", now-3600, 0, "reminder", "")  // defensive skip
	seedWake(t, dm, "wk-good", now-3600, 0, "reminder", "Real reason")

	got := dm.gatherOverdueWakes()
	require.Len(t, got, 1)
	require.Equal(t, "wk-good", got[0].ID)
}

func TestGatherOverdueWakes_KindExtractedFromMetadata(t *testing.T) {
	dm := NewTestDM(t)
	now := dm.nowUnix(t)
	seedWake(t, dm, "wk-drill", now-3600, 0, "drill", "Drill wake")
	seedWake(t, dm, "wk-task", now-3600, 0, "task", "Task wake")
	seedWake(t, dm, "wk-no-kind", now-3600, 0, "", "Kind missing")

	got := dm.gatherOverdueWakes()
	require.Len(t, got, 3)
	kindByID := map[string]string{}
	for _, w := range got {
		kindByID[w.ID] = w.Kind
	}
	require.Equal(t, "drill", kindByID["wk-drill"])
	require.Equal(t, "task", kindByID["wk-task"])
	require.Equal(t, "", kindByID["wk-no-kind"], "missing kind renders as empty string, not panic")
}

func TestGatherOverdueWakes_EmptyResultForFreshQueue(t *testing.T) {
	dm := NewTestDM(t)
	got := dm.gatherOverdueWakes()
	require.Empty(t, got, "empty scheduled_wakes queue → empty overdue surface")
	require.NotNil(t, got, "must be a valid empty slice, not nil-sentinel — see JSON contract")
}

func TestGatherOverdueWakes_NegativeOverdueClampedToZero(t *testing.T) {
	dm := NewTestDM(t)
	// Force a synthetic overdue_secs negative by inserting a wake with
	// target_time slightly in the future, then patching the gather query's
	// own filter would defeat the test. Instead we trust the unit-level
	// gather filter and verify clamp behavior at the renderer level
	// separately. Here we just assert that a row with target_time=now
	// produces overdue_secs=0 (boundary).
	now := dm.nowUnix(t)
	seedWake(t, dm, "wk-now", now, 0, "reminder", "Due right now")

	got := dm.gatherOverdueWakes()
	require.Len(t, got, 1)
	require.Equal(t, int64(0), got[0].OverdueSecs, "boundary target_time=now → overdue=0 (not negative)")
}

func TestHumanizeOverdueSecs_CoarseBucketing(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{0, "0s"},
		{45, "45s"},
		{60, "1m"},
		{3599, "59m"},
		{3600, "1h"},
		{86399, "23h"},
		{86400, "1d"},
		{259200, "3d"},
		{-10, "0s"}, // defensive clamp
	}
	for _, tc := range cases {
		got := humanizeOverdueSecs(tc.secs)
		require.Equal(t, tc.want, got, fmt.Sprintf("input=%d", tc.secs))
	}
}

func TestGatherWakeContext_IncludesOverdueWakesInPayload(t *testing.T) {
	dm := NewTestDM(t)
	now := dm.nowUnix(t)
	seedWake(t, dm, "wk-cross", now-3600, 0, "reminder", "Cross-boundary test")

	data, err := dm.GatherWakeContext()
	require.NoError(t, err)
	require.Len(t, data.OverdueWakes, 1, "GatherWakeContext must wire through gatherOverdueWakes")
	require.Equal(t, "wk-cross", data.OverdueWakes[0].ID)

	rendered := formatWakeContext(data)
	require.Contains(t, rendered, "**Overdue Wakes (1, capped at 5):**",
		"renderer must surface overdue wakes with the explicit cap note")
	require.Contains(t, rendered, "[reminder]",
		"renderer must show the kind tag")
	require.Contains(t, rendered, "wk-cross",
		"renderer must show the wake id")
}

// nowUnix is a tiny helper that returns the current Unix-seconds from
// the test DB host so target_time math is always relative to the same
// "now" the SQL query uses. Cheaper and cleaner than running a
// strftime in setup.
func (dm *DatabaseManager) nowUnix(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, dm.db.QueryRow(`SELECT CAST(strftime('%s','now') AS INTEGER)`).Scan(&n))
	return n
}
