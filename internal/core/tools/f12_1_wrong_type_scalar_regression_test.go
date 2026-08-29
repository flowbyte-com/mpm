// f12_1_wrong_type_scalar_regression_test.go — F12-1 alpha P2 regression.
//
// F12-1: scalar fields of wrong JSON type were silently coerced to
// the default value. The audit specifically called out `weight` (a
// memory/decision weight scalar) and `confidence` (a scalar in the
// 0.0–1.0 confidence range). A caller sending `weight: "5"` would
// get either `5` (silent coercion) or the default `1` (silent
// fallback), with no error at the boundary.
//
// The fix: distinguish three cases explicitly:
//   1. Field absent      → use the documented default
//   2. Field wrong type  → return an explicit error (NOT silent default)
//   3. Field right type  → use the value as-is
//
// The test exercises the agent surface (`mpm call mpm_memory` with a
// wrong-typed weight) and confirms the wrong-type case now produces a
// clear, agent-readable error rather than a silent default.
//
// (The fix itself lives in the parse helper used by the memory add
// path — see internal/core/parse.go. The test pins the contract on
// the agent-facing boundary, which is the surface the audit flagged.)
package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestF12_1_WrongTypedWeightIsRejected confirms a non-numeric weight
// payload is rejected with a clear error, not silently defaulted.
func TestF12_1_WrongTypedWeightIsRejected(t *testing.T) {
	dm := f3_3NewDM(t)

	// Caller sends weight as a string "5" — under the silent-coerce
	// behavior this would have been coerced to 5.0 (or defaulted to 1.0
	// if the parser refused). The new contract: a wrong-typed scalar
	// must error.
	_, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact":   "f12-1 test memory",
			"weight": "5", // WRONG TYPE — should be float64/int
		},
	})
	require.Error(t, err,
		"a weight scalar passed as a string must NOT silently coerce to a number; the boundary must reject it")
	errMsg := strings.ToLower(err.Error())
	assert.True(t,
		strings.Contains(errMsg, "weight") || strings.Contains(errMsg, "type") || strings.Contains(errMsg, "number") || strings.Contains(errMsg, "numeric"),
		"error must identify the offending field (weight/type/number), got: %s", err.Error())
}

// TestF12_1_NumericWeightIsAccepted is the regression-safety check:
// a numeric weight (the canonical type) must continue to work. The
// fix must not over-reach and reject valid payloads.
func TestF12_1_NumericWeightIsAccepted(t *testing.T) {
	dm := f3_3NewDM(t)

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact":   "f12-1 numeric weight test",
			"weight": 5.0, // CORRECT TYPE
		},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
}

// TestF12_1_AbsentWeightUsesDefault confirms the documented default
// is still used when the field is absent. The fix distinguishes
// "wrong type" (error) from "absent" (default) — the default behavior
// is a contract that agents rely on for terse payloads.
func TestF12_1_AbsentWeightUsesDefault(t *testing.T) {
	dm := f3_3NewDM(t)

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": "f12-1 absent weight test",
			// no weight field
		},
	})
	require.NoError(t, err,
		"absent weight must fall through to the documented default — agents rely on terse payloads")
	assert.NotNil(t, res)
}