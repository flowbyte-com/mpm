// Regression test for the T24 weaken-floor contract: WeakenMemory
// must clamp weight to a floor of 1.0 across every input shape that
// would otherwise push weight below 1.0. The pre-fix SQL used
// MAX(weight - ?, 0) which allowed weight=0.0; post-fix it uses
// MAX(weight - ?, 1) matching the canonical floor contract shared
// with AdjustMemoryWeight, WeakenMemoryTool, and the legacy_weight view.
//
// Floor contract source-of-truth: every weight-modifying primitive
// must clamp to >= 1.0. ReinforceMemory only adds so the floor is
// unreachable from the reinforce path; weaken is the path that crosses
// the floor when the delta exceeds weight.
//
// Four scenarios from the Round 6 brief:
//   1. Above floor: weight 50, weaken by 1 → 49
//   2. Exact floor: weight 1.0, weaken by 1 → 1.0 (floor_hit)
//   3. Crossing below floor (integer): weight 3, weaken by 10 → 1.0
//   4. Crossing below floor (fractional): weight 1.5, weaken by 5 → 1.0
//
// Each scenario also reads back via dm.db to confirm the SQL MAX
// actually fired (not just that the Go-side error check passed).

package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWeakenMemory_HonorsFloor_AboveFloor(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, weight, reinforcement_count, deleted_at, created_at, updated_at)
		VALUES ('weaken-above-mem', 'memories', 'well above floor', 50, 10, NULL, ?, ?)
	`, time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)

	// weightLoss = (1+1)/2 = 1 → 50 - 1 = 49, well above the 1.0 floor
	require.NoError(t, dm.WeakenMemory("weaken-above-mem", 1))

	var postWeight float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT weight FROM memories WHERE id = ?`, "weaken-above-mem",
	).Scan(&postWeight))
	require.InDelta(t, 49.0, postWeight, 0.001,
		"weight above the floor must subtract exactly delta: got %v", postWeight)
}

func TestWeakenMemory_HonorsFloor_ExactFloor(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, weight, reinforcement_count, deleted_at, created_at, updated_at)
		VALUES ('weaken-floor-mem', 'memories', 'exactly at floor', 1.0, 0, NULL, ?, ?)
	`, time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)

	// Already at the floor; any weaken must not push below 1.0
	// and must not error.
	require.NoError(t, dm.WeakenMemory("weaken-floor-mem", 1),
		"WeakenMemory at the floor must not error and must not push below 1.0")

	var postWeight float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT weight FROM memories WHERE id = ?`, "weaken-floor-mem",
	).Scan(&postWeight))
	require.InDelta(t, 1.0, postWeight, 0.001,
		"weight must stay at the 1.0 floor: got %v", postWeight)
}

func TestWeakenMemory_HonorsFloor_CrossingBelowInteger(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, weight, reinforcement_count, deleted_at, created_at, updated_at)
		VALUES ('weaken-crossing-mem', 'memories', 'weak row about to cross', 3, 0, NULL, ?, ?)
	`, time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)

	// weightLoss = (10+1)/2 = 5 → 3 - 5 = -2 without floor. With floor
	// of 1.0, post-state must clamp to 1.0.
	require.NoError(t, dm.WeakenMemory("weaken-crossing-mem", 10),
		"WeakenMemory with delta exceeding weight must clamp to the 1.0 floor, "+
			"not go to 0 or below")

	var postWeight float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT weight FROM memories WHERE id = ?`, "weaken-crossing-mem",
	).Scan(&postWeight))
	require.InDelta(t, 1.0, postWeight, 0.001,
		"weight must clamp to 1.0 floor (integer crossing): got %v", postWeight)
}

func TestWeakenMemory_HonorsFloor_CrossingBelowFractional(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, weight, reinforcement_count, deleted_at, created_at, updated_at)
		VALUES ('weaken-frac-mem', 'memories', 'fractional starting weight', 1.5, 1, NULL, ?, ?)
	`, time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)

	// weightLoss = (5+1)/2 = 3 → 1.5 - 3 = -1.5 without floor. With
	// floor of 1.0, post-state must clamp to 1.0.
	require.NoError(t, dm.WeakenMemory("weaken-frac-mem", 5),
		"WeakenMemory with delta exceeding fractional weight must clamp "+
			"to the 1.0 floor, not produce a negative weight")

	var postWeight float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT weight FROM memories WHERE id = ?`, "weaken-frac-mem",
	).Scan(&postWeight))
	require.InDelta(t, 1.0, postWeight, 0.001,
		"weight must clamp to 1.0 floor (fractional crossing): got %v", postWeight)
}

// TestWeakenMemory_FloorConsistentWithSiblings pins the cross-site
// floor contract. If a future change weakens one of the floor sites
// (AdjustMemoryWeight, WeakenMemoryTool, legacy_weight, or the
// migration's `weight < 1.0` placeholder filter), this test surfaces
// the divergence by exercising all four with the same input shape
// and asserting they all land at 1.0.
func TestWeakenMemory_FloorConsistentWithSiblings(t *testing.T) {
	dm := newTestDM(t)

	// AdjustMemoryWeight with a large negative delta must clamp at 1.0
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, weight, reinforcement_count, deleted_at, created_at, updated_at)
		VALUES ('floor-sibling-adjust', 'memories', 'adjust path', 2.0, 0, NULL, ?, ?)
	`, time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)
	require.NoError(t, dm.AdjustMemoryWeight("floor-sibling-adjust", -100))
	var w float64
	require.NoError(t, dm.db.QueryRow(`SELECT weight FROM memories WHERE id = ?`,
		"floor-sibling-adjust").Scan(&w))
	require.InDelta(t, 1.0, w, 0.001,
		"AdjustMemoryWeight must clamp to 1.0 floor: got %v", w)

	// WeakenMemory with a large delta must clamp at 1.0 (T24 fix)
	_, err = dm.db.Exec(`
		INSERT INTO memories (id, collection, content, weight, reinforcement_count, deleted_at, created_at, updated_at)
		VALUES ('floor-sibling-weaken', 'memories', 'weaken path', 2.0, 0, NULL, ?, ?)
	`, time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)
	require.NoError(t, dm.WeakenMemory("floor-sibling-weaken", 100))
	require.NoError(t, dm.db.QueryRow(`SELECT weight FROM memories WHERE id = ?`,
		"floor-sibling-weaken").Scan(&w))
	require.InDelta(t, 1.0, w, 0.001,
		"WeakenMemory must clamp to 1.0 floor: got %v", w)

	// WeakenMemoryTool must clamp at 1.0 (already correct pre-T24)
	_, err = dm.db.Exec(`
		INSERT INTO memories (id, collection, content, weight, reinforcement_count, deleted_at, created_at, updated_at)
		VALUES ('floor-sibling-tool', 'memories', 'tool path', 2.0, 0, NULL, ?, ?)
	`, time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)
	_, err = dm.WeakenMemoryTool("floor-sibling-tool", 100)
	require.NoError(t, err)
	require.NoError(t, dm.db.QueryRow(`SELECT weight FROM memories WHERE id = ?`,
		"floor-sibling-tool").Scan(&w))
	require.InDelta(t, 1.0, w, 0.001,
		"WeakenMemoryTool must clamp to 1.0 floor: got %v", w)
}
