// Regression test for the T27 5th-site scan failure: dm.ReinforceMemory
// and dm.WeakenMemory read back the post-state weight into an int
// destination. With a fractional weight value (REAL column), the
// Scan fails with `converting driver.Value type float64 to a int`.
//
// Pre-fix (this test, 2026-09-11): postWeight int64 → Scan fails on
// weight=11.5; the round-4 epistemology_tools.go fix missed this site.
//
// Post-fix: weight is scanned into float64; integer and fractional
// rows both succeed.

package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestReinforceMemory_FractionalWeightReadBack exercises the post-update
// read-back in dm.ReinforceMemory with a fractional weight value.
// Pre-fix this returned `sql: Scan error ... converting driver.Value
// type float64 ("11.5") to a int`. Post-fix it succeeds.
func TestReinforceMemory_FractionalWeightReadBack(t *testing.T) {
	dm := newTestDM(t)

	// Seed a memory with a fractional weight (REAL column).
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, weight, reinforcement_count, deleted_at, created_at, updated_at)
		VALUES ('reinforce-fractional-mem', 'memories', 'fractional reinforce', 11.5, 3, NULL, ?, ?)
	`, time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)

	// Pre-fix this returned a Scan error on weight=11.5 + delta gain.
	require.NoError(t, dm.ReinforceMemory("reinforce-fractional-mem", 5),
		"ReinforceMemory must accept fractional weight values — pre-fix, "+
			"the post-update read-back scanned weight as int and panicked "+
			"on any fractional value")

	// Read-back: weight should be MIN(weight + gain, 100). With
	// weightGain = (5+1)/2 = 3 → weight = MIN(11.5 + 3, 100) = 14.5.
	var postWeight float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT weight FROM memories WHERE id = ?`, "reinforce-fractional-mem",
	).Scan(&postWeight))
	require.InDelta(t, 14.5, postWeight, 0.001,
		"weight after reinforce should be 14.5; got %v", postWeight)
}

// TestWeakenMemory_FractionalWeightReadBack exercises the post-update
// read-back in dm.WeakenMemory with a fractional weight value. Same
// scan-target bug class as ReinforceMemory.
func TestWeakenMemory_FractionalWeightReadBack(t *testing.T) {
	dm := newTestDM(t)

	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, weight, reinforcement_count, deleted_at, created_at, updated_at)
		VALUES ('weaken-fractional-mem', 'memories', 'fractional weaken', 11.5, 3, NULL, ?, ?)
	`, time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)

	require.NoError(t, dm.WeakenMemory("weaken-fractional-mem", 5),
		"WeakenMemory must accept fractional weight values — pre-fix, "+
			"the post-update read-back scanned weight as int and panicked")

	// Verify the floor mechanism is reachable. Pre-fix the floor
	// check was unreachable because Scan failed first. Post-fix:
	// weight after weaken = MAX(11.5 - 3, 1) = 8.5.
	var postWeight float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT weight FROM memories WHERE id = ?`, "weaken-fractional-mem",
	).Scan(&postWeight))
	require.InDelta(t, 8.5, postWeight, 0.001)
}

// TestReinforceMemory_IntegerWeightStillWorks pins that the
// fractional-weight fix does not regress integer-valued rows.
func TestReinforceMemory_IntegerWeightStillWorks(t *testing.T) {
	dm := newTestDM(t)

	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, weight, reinforcement_count, deleted_at, created_at, updated_at)
		VALUES ('reinforce-integer-mem', 'memories', 'integer reinforce', 50, 0, NULL, ?, ?)
	`, time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)

	require.NoError(t, dm.ReinforceMemory("reinforce-integer-mem", 5))

	var postWeight float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT weight FROM memories WHERE id = ?`, "reinforce-integer-mem",
	).Scan(&postWeight))
	require.InDelta(t, 53.0, postWeight, 0.001)
}