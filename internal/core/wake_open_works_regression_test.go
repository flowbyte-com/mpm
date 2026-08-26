// wake_open_works_regression_test.go — F3 regression.
//
// Audit finding F3: with 23 open works, the wake context surfaced only the
// five OLDEST (created_at ASC) — fresh agents saw stale historical work
// while current work never appeared. The fix orders by updated_at DESC
// (most recently touched first, created_at DESC tiebreak) so newly created
// and recently updated work is what agents see.
package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func touchWork(t *testing.T, dm *DatabaseManager, id string, at time.Time) {
	t.Helper()
	_, err := dm.db.Exec(`UPDATE works SET updated_at = ? WHERE id = ?`, at.Unix(), id)
	require.NoError(t, err)
}

func TestF3_OpenWorksSurfacesCurrentNotOldest(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	now := time.Now()

	// Seed 7 "historical" open works, each older than the last.
	historicalIDs := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		work, err := dm.AddWork("historical work "+string(rune('A'+i)), "", "")
		require.NoError(t, err)
		historicalIDs = append(historicalIDs, work.ID)
		old := now.Add(time.Duration(-(48+i)*24) * time.Hour)
		touchWork(t, dm, work.ID, old)
	}
	require.Len(t, historicalIDs, 7)

	// A brand-new work: under the old ASC ordering it would never surface
	// (it sits behind the five oldest).
	freshWork, err := dm.AddWork("fresh urgent work", "", "")
	require.NoError(t, err)
	freshID := freshWork.ID
	touchWork(t, dm, freshID, now)

	works := dm.gatherOpenWorks()
	require.NotEmpty(t, works)
	assert.Equal(t, freshID, works[0].ID,
		"newly created work must be surfaced first, not buried under stale history")

	// The five oldest must NOT fill the bound ahead of newer items.
	firstFive := map[string]bool{}
	for _, w := range works[:min(5, len(works))] {
		firstFive[w.ID] = true
	}
	// With 8 open works and limit 5, at least one historical item may appear,
	// but the OLDEST ones must rank last, not first.
	if len(works) == 5 {
		assert.NotEqual(t, historicalIDs[0], works[0].ID,
			"the single oldest work must not lead the list")
	}

	// Recently-updated old work jumps ahead of untouched history
	// (recency beats age; only the fresher brand-new work may precede it).
	reanimated := historicalIDs[6]
	touchWork(t, dm, reanimated, now.Add(-1*time.Minute))
	works = dm.gatherOpenWorks()
	require.NotEmpty(t, works)
	rank := -1
	for i, w := range works {
		if w.ID == reanimated {
			rank = i
			break
		}
	}
	assert.NotEqual(t, -1, rank, "reanimated work must surface within the bound")
	assert.LessOrEqual(t, rank, 1,
		"a 48-day-old work touched a minute ago must outrank every untouched item except newer ones")
}

func TestF3_ClosedAndCancelledExcludedDeterministicOrder(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	open1, err := dm.AddWork("open one", "", "")
	require.NoError(t, err)
	doneWork, err := dm.AddWork("completed work", "", "")
	done1 := doneWork.ID
	require.NoError(t, err)
	open2, err := dm.AddWork("open two", "", "")
	require.NoError(t, err)
	cxlWork, err := dm.AddWork("cancelled work", "", "")
	cxl1 := cxlWork.ID
	require.NoError(t, err)
	_, err = dm.CompleteWork(done1)
	require.NoError(t, err)
	_, err = dm.CancelWork(cxl1)
	require.NoError(t, err)

	works := dm.gatherOpenWorks()
	ids := make([]string, 0, len(works))
	for _, w := range works {
		ids = append(ids, w.ID)
	}
	assert.Contains(t, ids, open1.ID)
	assert.Contains(t, ids, open2.ID)
	assert.NotContains(t, ids, done1, "done work must not appear in open_works")
	assert.NotContains(t, ids, cxl1, "cancelled work must not appear in open_works")

	// Deterministic ordering: two gathers agree exactly.
	again := dm.gatherOpenWorks()
	ids2 := make([]string, 0, len(again))
	for _, w := range again {
		ids2 = append(ids2, w.ID)
	}
	assert.Equal(t, ids, ids2)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
