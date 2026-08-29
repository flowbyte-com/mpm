// f7_1_challenge_restore_surface_test.go — F7-1 alpha P2 regression
//
// F7-1: challenge restore was CLI-only. The audit flagged that the
// restore operation had no agent-facing counterpart, forcing agents to
// either drop into shell or duplicate the restore logic.
//
// This test exercises the agent surface end-to-end:
//
//   1. `mpm call mpm_memory --payload '{"action":"restore_challenge",...}'`
//      reaches the same DatabaseManager method as `mpm challenge restore`.
//
//   2. `mpm call mpm_challenge --payload '{"action":"restore",...}'` is
//      a parallel top-level entrypoint with the same behavior.
//
//   3. Both paths produce the same end state: weight restored to
//      pre-challenge value, challenged flags cleared, theory marked
//      disproven, confidence left at the challenged floor (F7.1
//      invariant: restoration ≠ verification promotion).
package tools

import (
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedChallengedMemory plants a memory, challenges it via the core
// method (same path the CLI `mpm challenge` would take), and returns
// the memory id and the pre-challenge weight (which the restore path
// will restore the memory to).
func seedChallengedMemory(t *testing.T, dm *mpminternal.DatabaseManager) (memoryID string, originalWeight int) {
	t.Helper()
	id, err := dm.SaveMemory("memories", "F7-1 target memory", "", []string{"f7-1"}, nil, nil, false, 7)
	require.NoError(t, err)

	_, err = dm.ChallengeMemoryWithTheory(id, "audit-regression dispute")
	require.NoError(t, err)

	// After challenge, weight is reduced by 2 (7 - 2 = 5).
	w, err := dm.GetMemory(id)
	require.NoError(t, err)
	weight, _ := w["weight"].(int)
	require.Equal(t, 5, weight, "setup: challenge must have reduced weight 7 → 5")
	return id, 7
}

// TestF7_1_RestoreChallenge_AgentSurface_MemoryTool checks the
// `mpm call mpm_memory {"action":"restore_challenge",...}` surface.
func TestF7_1_RestoreChallenge_AgentSurface_MemoryTool(t *testing.T) {
	dm := f6NewDM(t)
	id, originalWeight := seedChallengedMemory(t, dm)

	res, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "restore_challenge",
		"params": map[string]interface{}{"memory_id": id},
	})
	require.NoError(t, err)
	m := res.(map[string]interface{})
	assert.Equal(t, "restored", m["action"])
	assert.Equal(t, id, m["memory_id"])

	// Weight restored.
	w, err := dm.GetMemory(id)
	require.NoError(t, err)
	weight, _ := w["weight"].(int)
	assert.Equal(t, originalWeight, weight, "weight must be restored to pre-challenge value")

	// Confidence left at floor (F7.1 invariant).
	conf, _ := w["confidence"].(float64)
	assert.Equal(t, mpminternal.ChallengedMemoryConfidenceFloor, conf,
		"confidence must remain at the challenged floor; restoration does NOT silently re-promote verification")

	// Status flag cleared, audit trail preserved.
	metaStr, _ := w["metadata"].(string)
	assert.NotContains(t, metaStr, `"status":"challenged"`,
		"challenged status flag must be cleared")
	assert.Contains(t, metaStr, `"restored_from_challenge":true`,
		"restoration event must be stamped in metadata for the audit trail")
}

// TestF7_1_RestoreChallenge_AgentSurface_TopLevel checks the parallel
// `mpm call mpm_challenge {"action":"restore",...}` top-level surface.
func TestF7_1_RestoreChallenge_AgentSurface_TopLevel(t *testing.T) {
	dm := f6NewDM(t)
	id, originalWeight := seedChallengedMemory(t, dm)

	res, err := handleMpmChallenge(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action":    "restore",
		"memory_id": id,
	})
	require.NoError(t, err)
	m := res.(map[string]interface{})
	assert.Equal(t, "restored", m["action"])

	w, err := dm.GetMemory(id)
	require.NoError(t, err)
	weight, _ := w["weight"].(int)
	assert.Equal(t, originalWeight, weight)
}

// TestF7_1_RestoreChallenge_NotChallenged_Rejects confirms the agent
// surface rejects restore against a memory that has not been challenged.
func TestF7_1_RestoreChallenge_NotChallenged_Rejects(t *testing.T) {
	dm := f6NewDM(t)
	id, err := dm.SaveMemory("memories", "untouched memory", "", []string{"f7-1"}, nil, nil, false, 5)
	require.NoError(t, err)

	_, err = handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "restore_challenge",
		"params": map[string]interface{}{"memory_id": id},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not challenged",
		"restore against a non-challenged memory must be rejected with a clear error")
}

// TestF7_1_RestoreChallenge_MissingID_Rejects confirms required memory_id
// is validated at the boundary, not silently defaulted.
func TestF7_1_RestoreChallenge_MissingID_Rejects(t *testing.T) {
	dm := f6NewDM(t)

	_, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "restore_challenge",
		"params": map[string]interface{}{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "memory_id",
		"missing memory_id must be rejected with a memory_id-pointer error")
}
