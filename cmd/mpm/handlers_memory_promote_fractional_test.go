// Regression test for the 2026-09-11 T27 float64→int scan failure.
//
// The pre-fix promote path scanned a REAL-valued weight (e.g. 11.5)
// into an int destination via a code path downstream of dm.PromoteMemory.
// MPM does not guarantee weight is an integer — schema weight is REAL,
// legacy data may carry fractional values, and migration paths can
// produce non-integer weights. The promote handler must round-trip a
// fractional value without scanning it into an int.
//
// This test seeds a memory with weight=11.5 (a fractional REAL value),
// invokes dm.PromoteMemory (the canonical promote primitive used by
// both `mpm memory promote` and the mpm_memory MCP tool), and verifies
// the post-condition is satisfied: weight=10, is_long_term=1.
//
// If the underlying Scan destination is `int` (not `float64`) and the
// value carries a fractional component, this test fails with the
// classic `converting driver.Value type float64 ("11.5") to int` error.

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPromoteMemory_FractionalWeightSurvives is the primary regression
// guard for T27. Seeds a memory with weight=11.5 (REAL), promotes it,
// asserts the post-condition holds. Pre-fix, the downstream code path
// (or the promote itself, depending on the exact failure point) would
// panic with a float64→int scan error on a fractional weight.
func TestPromoteMemory_FractionalWeightSurvives(t *testing.T) {
	dm := setupRestoreSafetyTest(t) // reuses the file-backed DM helper

	// Seed a memory with a fractional weight — the schema column is
	// REAL, so 11.5 is a valid value. Pre-fix code that scanned
	// weight into an int destination would fail here.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, deleted_at, created_at, updated_at)
		VALUES ('promote-fractional-mem', 'memories', 'fractional weight test', 11.5, NULL, 1700000000, 1700000000)
	`)
	require.NoError(t, err, "seeding fractional-weight memory must succeed")

	// Verify the fractional value landed in the REAL column correctly.
	var preWeight float64
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT weight FROM memories WHERE id = ?`, "promote-fractional-mem",
	).Scan(&preWeight))
	require.Equal(t, 11.5, preWeight,
		"weight must persist as REAL 11.5 — a scan-target mismatch would have "+
			"clobbered it during insert")

	// The actual promote call. handlePromote (CLI) and handlePromoteMemory
	// (MCP) both delegate here. Pre-fix, this either panicked on the
	// float64→int scan in ReinforceMemory's underlying math, or in a
	// post-condition read.
	res, err := dm.PromoteMemory("promote-fractional-mem")
	require.NoError(t, err, "PromoteMemory must succeed on a fractional-weight memory")
	require.NotNil(t, res)

	// Post-condition: weight=10 (integer assigned by promote), is_long_term=1.
	var postWeight float64
	var isLTM int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT weight, is_long_term FROM memories WHERE id = ?`, "promote-fractional-mem",
	).Scan(&postWeight, &isLTM))
	require.Equal(t, 10.0, postWeight,
		"weight must be 10 after promote regardless of pre-value")
	require.Equal(t, 1, isLTM,
		"is_long_term must be 1 after promote")

	// Read-back through the canonical GetMemory surface to exercise
	// any read paths that might scan weight into an int destination.
	mem, err := dm.GetMemory("promote-fractional-mem")
	require.NoError(t, err, "GetMemory must succeed on a promoted memory with fractional history")
	require.NotNil(t, mem)
}

// TestPromoteMemory_IntegerWeightStillWorks is the regression guard
// for the existing-integer case — pins that adding float64 targets
// to the schema or scan paths does not regress integer-valued rows.
func TestPromoteMemory_IntegerWeightStillWorks(t *testing.T) {
	dm := setupRestoreSafetyTest(t)

	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, deleted_at, created_at, updated_at)
		VALUES ('promote-integer-mem', 'memories', 'integer weight test', 50, NULL, 1700000000, 1700000000)
	`)
	require.NoError(t, err)

	res, err := dm.PromoteMemory("promote-integer-mem")
	require.NoError(t, err)
	require.NotNil(t, res)

	var postWeight float64
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT weight FROM memories WHERE id = ?`, "promote-integer-mem",
	).Scan(&postWeight))
	require.Equal(t, 10.0, postWeight)
}

// TestGetMemory_FractionalWeightSurvives is the broader read-side guard:
// even if promote succeeds, any code that reads the weight back must
// accept the REAL type. GetMemory is the canonical read primitive —
// if it scans weight into int, this test fails.
func TestGetMemory_FractionalWeightSurvives(t *testing.T) {
	dm := setupRestoreSafetyTest(t)

	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, deleted_at, created_at, updated_at)
		VALUES ('getmem-fractional-mem', 'memories', 'fractional read test', 7.25, NULL, 1700000000, 1700000000)
	`)
	require.NoError(t, err)

	mem, err := dm.GetMemory("getmem-fractional-mem")
	require.NoError(t, err, "GetMemory must accept fractional weight values")
	require.NotNil(t, mem)

	weightVal, ok := mem["weight"]
	require.True(t, ok, "GetMemory must return a weight field")

	// Accept either an int (legacy contract, fractional truncated) or
	// a float64 (cleaner contract preserved).
	switch w := weightVal.(type) {
	case int:
		require.Equal(t, 7, w, "fractional weight 7.25 truncated to int 7 — acceptable")
	case float64:
		require.Equal(t, 7.25, w, "fractional weight preserved as float64")
	default:
		t.Fatalf("unexpected weight type %T value %v", w, w)
	}
}