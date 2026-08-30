// resolve_theory_param_naming_test.go — alpha-4.1.2 D-008/W-006 regression test.
//
// Audit finding: the auditor flagged that `mpm_theories resolve` accepted
// `newStatus` as the parameter name, while the canonical vocabulary used
// in propose+resolve human-facing contexts is `status`. The naming
// mismatch made the call surface inconsistent.
//
// Fix (alpha-4.1.1): handleResolveTheory now prefers the natural `status`
// key. `newStatus` is retained as a backward-compatible alias. If both
// are supplied with conflicting values, the call is rejected explicitly.
//
// This test pins the post-fix contract:
//
//   - `status: "proven"` (canonical) succeeds.
//   - `newStatus: "proven"` (legacy alias) succeeds.
//   - Both supplied with the same value succeeds.
//   - Both supplied with conflicting values rejects explicitly.
//   - Neither supplied rejects explicitly.

package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// proposePendingTheory creates a pending theory in dm and returns its id.
func proposePendingTheory(t *testing.T, dm *mpminternal.DatabaseManager) string {
	t.Helper()
	res, err := dm.ProposeTheory(
		"D-008 hypothesis",
		"D-008 validation criteria",
		nil, nil, []string{"alpha-4.1.2"},
	)
	require.NoError(t, err)
	id, _ := res["id"].(string)
	require.NotEmpty(t, id)
	return id
}

func TestHandleResolveTheory_StatusCanonical_WS(t *testing.T) {
	dm := f3_3NewDM(t)
	id := proposePendingTheory(t, dm)

	out, err := handleResolveTheory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"theoryId":   id,
		"conclusion": "D-008 canonical status key resolves",
		"status":     "proven",
	})
	require.NoError(t, err)
	require.NotNil(t, out)
}

func TestHandleResolveTheory_NewStatusAlias_WS(t *testing.T) {
	dm := f3_3NewDM(t)
	id := proposePendingTheory(t, dm)

	out, err := handleResolveTheory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"theoryId":   id,
		"conclusion": "D-008 newStatus alias still works",
		"newStatus":  "disproven",
	})
	require.NoError(t, err)
	require.NotNil(t, out)
}

func TestHandleResolveTheory_BothKeysSameValue_WS(t *testing.T) {
	dm := f3_3NewDM(t)
	id := proposePendingTheory(t, dm)

	out, err := handleResolveTheory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"theoryId":   id,
		"conclusion": "D-008 both keys same value",
		"status":     "proven",
		"newStatus":  "proven",
	})
	require.NoError(t, err)
	require.NotNil(t, out)
}

func TestHandleResolveTheory_BothKeysConflict_RejectsExplicitly_WS(t *testing.T) {
	dm := f3_3NewDM(t)
	id := proposePendingTheory(t, dm)

	_, err := handleResolveTheory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"theoryId":   id,
		"conclusion": "D-008 conflicting keys should reject",
		"status":     "proven",
		"newStatus":  "disproven",
	})
	require.Error(t, err, "conflicting status/newStatus must reject explicitly")
	// Error must name both keys so the author can see the conflict.
	msg := err.Error()
	assert.True(t, strings.Contains(msg, "status") && strings.Contains(msg, "newStatus"),
		"error must name both keys, got: %s", msg)
}

func TestHandleResolveTheory_NeitherKey_RejectsExplicitly_WS(t *testing.T) {
	dm := f3_3NewDM(t)
	id := proposePendingTheory(t, dm)

	_, err := handleResolveTheory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"theoryId":   id,
		"conclusion": "D-008 missing status/newStatus",
	})
	require.Error(t, err, "missing status/newStatus must reject explicitly")
	msg := err.Error()
	// Must hint at both keys so authors can recover.
	assert.Contains(t, msg, "status", "error must mention 'status' (canonical key)")
}

func TestHandleResolveTheory_BadValue_Rejects_WS(t *testing.T) {
	dm := f3_3NewDM(t)
	id := proposePendingTheory(t, dm)

	_, err := handleResolveTheory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"theoryId":   id,
		"conclusion": "D-008 invalid status value",
		"status":     "definitely-wrong",
	})
	require.Error(t, err, "non-proven/disproven status must reject")
}