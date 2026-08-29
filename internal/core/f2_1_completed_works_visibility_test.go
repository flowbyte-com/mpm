// f2_1_completed_works_visibility_test.go — F2-1 alpha P2 regression.
//
// F2-1: completed (status='done') work had no wake-context surface. The
// audit found that a fresh agent session could re-orient against open
// work via OpenWorks, but had no "what did I just ship" view of recently
// completed work. The fix added gatherCompletedWorks (limit 5,
// completed_at DESC), but the surface itself was not covered by a
// regression test — the audit specifically flagged this as missing.
//
// This test pins three contracts:
//   1. Completed work IS visible in wake context (the basic visibility).
//   2. Ordering is completed_at DESC, not created_at — a work item
//      completed yesterday outranks one completed last week.
//   3. Cancelled work is excluded — the comment on the production code
//      says cancelled items "belong to a different cognitive channel"
//      and must not pollute the completed-works view.
package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedDoneAndCancelledWorks creates one done, one cancelled, and one
// open work. Returns the IDs in that order.
func seedDoneAndCancelledWorks(t *testing.T, dm *DatabaseManager) (doneID, cancelledID, openID string) {
	t.Helper()
	now := time.Now()

	dw, err := dm.AddWork("done work f2-1", "", "")
	require.NoError(t, err)
	_, err = dm.CompleteWork(dw.ID)
	require.NoError(t, err)
	doneID = dw.ID

	cw, err := dm.AddWork("cancelled work f2-1", "", "")
	require.NoError(t, err)
	_, err = dm.CancelWork(cw.ID)
	require.NoError(t, err)
	cancelledID = cw.ID

	ow, err := dm.AddWork("open work f2-1", "", "")
	require.NoError(t, err)
	openID = ow.ID

	// Sanity: at least one full second has passed across the seed
	// (touches may be sub-second on fast machines; the ordering assertion
	// only needs recency, not millisecond precision).
	_ = now
	return doneID, cancelledID, openID
}

// TestF2_1_CompletedWorkVisibleInWakeContext is the primary F2-1
// visibility test. After completing a work, gatherCompletedWorks must
// return it — and the recent completion must outrank the cancelled
// item, which must NOT appear in the result set at all.
func TestF2_1_CompletedWorkVisibleInWakeContext(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	doneID, cancelledID, _ := seedDoneAndCancelledWorks(t, dm)

	completed := dm.gatherCompletedWorks()
	require.NotEmpty(t, completed,
		"a completed work must surface in the wake context completed_works list")

	var foundDone bool
	for _, w := range completed {
		if w.ID == doneID {
			foundDone = true
		}
		// Cancelled must NEVER appear in completed_works.
		assert.NotEqual(t, cancelledID, w.ID,
			"cancelled work must not surface as 'completed' (different cognitive channel)")
	}
	assert.True(t, foundDone,
		"the completed work must be present in gatherCompletedWorks output")
}

// TestF2_1_CompletedWorksOrderedByCompletedAtNotCreatedAt pins the
// ordering contract. If a work item was created a long time ago but
// only completed recently, it must still rank higher than an older
// completion.
func TestF2_1_CompletedWorksOrderedByCompletedAtNotCreatedAt(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	now := time.Now()

	// Old creation, recent completion — must rank first.
	oldDone, err := dm.AddWork("old creation, recent completion", "", "")
	require.NoError(t, err)
	// Backdate the created_at to 90 days ago so the
	// created_at-ascending code path would put it last.
	_, err = dm.db.Exec(`UPDATE works SET created_at = ? WHERE id = ?`,
		now.Add(-90*24*time.Hour).Unix(), oldDone.ID)
	require.NoError(t, err)
	_, err = dm.CompleteWork(oldDone.ID)
	require.NoError(t, err)
	// CompleteWork sets completed_at=now(). Re-pin it explicitly so the
	// tiebreak is not subject to test-machine timing.
	_, err = dm.db.Exec(`UPDATE works SET completed_at = ? WHERE id = ?`,
		now.Unix(), oldDone.ID)
	require.NoError(t, err)

	// Recent creation, but explicitly backdated completed_at to 2h ago
	// so the recent-but-older completion ranks second.
	recentDone, err := dm.AddWork("recent creation, older completion", "", "")
	require.NoError(t, err)
	_, err = dm.CompleteWork(recentDone.ID)
	require.NoError(t, err)
	_, err = dm.db.Exec(`UPDATE works SET completed_at = ? WHERE id = ?`,
		now.Add(-2*time.Hour).Unix(), recentDone.ID)
	require.NoError(t, err)

	completed := dm.gatherCompletedWorks()
	require.Len(t, completed, 2, "both completions must be visible (limit=5)")

	// The old-but-recently-completed work must rank first; the recent-
	// but-completed-2h-ago work must rank second.
	assert.Equal(t, oldDone.ID, completed[0].ID,
		"ordering must be completed_at DESC, not created_at DESC")
	assert.Equal(t, recentDone.ID, completed[1].ID,
		"the older completion must rank below the recent one")
}

// TestF2_1_CompletedWorksRespectsLimit ensures the 5-item bound is
// honored when more than 5 works are completed.
func TestF2_1_CompletedWorksRespectsLimit(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	// Create 8 completed works with explicit completed_at offsets so the
	// ordering is deterministic (sub-second completions on a fast machine
	// would tie on completed_at and the updated_at-DESC tiebreak would
	// pick a different order than seed order).
	ids := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		w, err := dm.AddWork("completed work", "", "")
		require.NoError(t, err)
		_, err = dm.CompleteWork(w.ID)
		require.NoError(t, err)
		// Backdate completed_at so each subsequent completion is
		// strictly newer (or equal-and-then-resolved by updated_at).
		_, err = dm.db.Exec(`UPDATE works SET completed_at = ? WHERE id = ?`,
			time.Now().Add(time.Duration(i-7)*time.Hour).Unix(), w.ID)
		require.NoError(t, err)
		ids = append(ids, w.ID)
	}

	completed := dm.gatherCompletedWorks()
	assert.LessOrEqual(t, len(completed), 5,
		"gatherCompletedWorks must cap at 5 items (the wake-context bound)")
	require.NotEmpty(t, completed)

	// The LAST five ids (offsets 3..7, the most recent 5 by completed_at)
	// must appear. ids is in seed order; ids[len-5:] are the ones whose
	// completed_at offset was -3h..0h (most recent).
	mostRecentFive := map[string]bool{}
	for _, id := range ids[len(ids)-5:] {
		mostRecentFive[id] = true
	}
	for _, w := range completed {
		if _, ok := mostRecentFive[w.ID]; ok {
			delete(mostRecentFive, w.ID)
		}
	}
	assert.Empty(t, mostRecentFive,
		"the 5 most recently completed works must all appear in the bound")

	// The first three ids (offsets 0..2, the OLDEST completions) must be
	// evicted by the limit.
	oldestThree := map[string]bool{}
	for _, id := range ids[:3] {
		oldestThree[id] = true
	}
	for _, w := range completed {
		if _, ok := oldestThree[w.ID]; ok {
			assert.Fail(t, "evicted work surfaced in result set",
				"work %s (older completion) should have been evicted by the 5-item limit", w.ID)
		}
	}
}

// TestF2_1_EmptyWhenNoCompletedWorks confirms the slice is non-nil but
// empty when no work has been completed. Wake context invariant 3
// (non-nil empty slice on no data) must hold.
func TestF2_1_EmptyWhenNoCompletedWorks(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	completed := dm.gatherCompletedWorks()
	assert.NotNil(t, completed,
		"gatherCompletedWorks must return a non-nil slice even when empty")
	assert.Empty(t, completed,
		"no completions = empty slice")
}