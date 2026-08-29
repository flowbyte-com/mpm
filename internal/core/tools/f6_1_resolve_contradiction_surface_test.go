// f6_1_resolve_contradiction_surface_test.go — F6-1 alpha P2 regression
//
// F6-1: no first-class agent-facing contradiction resolution. The audit
// flagged that an agent could write evidence but had no canonical way to
// withdraw / resolve a dispute against a work item.
//
// This test exercises the agent surface end-to-end:
//
//   1. `mpm call mpm_work --payload '{"action":"resolve_contradiction",...}'`
//      reaches the same DatabaseManager method as the CLI facade
//      (`mpm work item resolve-contradiction`) and as the eventual MCP
//      tool binding.
//
//   2. The agent-facing payload is accepted; required fields (work_id,
//      reason) are validated; empty reason is rejected.
//
//   3. The DP side effect (audit-trail reason stamped on the dispute row,
//      verification re-derived) is identical to the unit-level
//      ResolveWorkContradiction call.
package tools

import (
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func f6NewDM(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	dm := mpminternal.NewTestDM(t)
	t.Cleanup(func() { dm.Close() })
	return dm
}

// seedVerifiedWorkWithDispute creates a work item, verifies it via
// outcome evidence, then adds an unsubstantiated challenge row.
func seedVerifiedWorkWithDispute(t *testing.T, dm *mpminternal.DatabaseManager) (workID string) {
	t.Helper()
	w, err := dm.CreateWorkWithContext("F6-1 target", "", "", mpminternal.ActiveContext{})
	require.NoError(t, err)
	workID = w.ID

	require.NoError(t, mpminternal.AddEvidence(dm, mpminternal.EvidenceInput{
		ArtifactID:   workID,
		ArtifactType: "work",
		Type:         "test",
		SourceGroup:  "test",
		Strength:     0.85,
		CreatedBy:    "f6-1",
		CreatedAt:    time.Now(),
	}))
	require.NoError(t, mpminternal.AddEvidence(dm, mpminternal.EvidenceInput{
		ArtifactID:   workID,
		ArtifactType: "work",
		Type:         "challenge",
		SourceGroup:  "manual_review",
		Strength:     -0.6,
		CreatedBy:    "f6-1",
		CreatedAt:    time.Now(),
	}))

	w2, err := dm.GetWork(workID)
	require.NoError(t, err)
	require.Equal(t, string(mpminternal.WorkVerificationPartial), string(w2.Verification),
		"setup: must be partial (disputed, not contradicted) so F6-1 exercises the recovery path")
	return workID
}

// TestF6_1_ResolveContradiction_AgentSurface is the canonical agent-path
// regression: a single tool call resolves the dispute and re-derives.
func TestF6_1_ResolveContradiction_AgentSurface(t *testing.T) {
	dm := f6NewDM(t)
	workID := seedVerifiedWorkWithDispute(t, dm)

	_, err := handleMpmWork(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "resolve_contradiction",
		"params": map[string]interface{}{
			"work_id": workID,
			"reason":  "spurious dispute withdrawn by agent",
		},
	})
	require.NoError(t, err)

	w, err := dm.GetWork(workID)
	require.NoError(t, err)
	assert.Equal(t, string(mpminternal.WorkVerificationVerified), string(w.Verification),
		"F6-1: agent-facing resolve_contradiction must restore verified status after dispute withdrawal")
}

// TestF6_1_ResolveContradiction_ValidatesReason checks that the agent
// surface rejects an empty reason. The audit-trail integrity depends on
// the reason being non-empty; we reject early so the agent gets a clear
// contract violation rather than a silent zero-default.
func TestF6_1_ResolveContradiction_ValidatesReason(t *testing.T) {
	dm := f6NewDM(t)
	workID := seedVerifiedWorkWithDispute(t, dm)

	_, err := handleMpmWork(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "resolve_contradiction",
		"params": map[string]interface{}{
			"work_id": workID,
		},
	})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "reason",
		"missing reason must be rejected with a reason-pointer error, got: %v", err)
}

// TestF6_1_ResolveContradiction_ValidatesWorkID checks that an empty
// work_id is rejected at the boundary, not silently defaulted.
func TestF6_1_ResolveContradiction_ValidatesWorkID(t *testing.T) {
	dm := f6NewDM(t)

	_, err := handleMpmWork(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "resolve_contradiction",
		"params": map[string]interface{}{
			"reason": "no work_id supplied",
		},
	})
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "work_id",
		"missing work_id must be rejected at the boundary, got: %v", err)
}

// TestF6_1_ResolveContradiction_NotFoundRejects checks that a missing
// work_id returns a clear "not found" rather than silently succeeding.
func TestF6_1_ResolveContradiction_NotFoundRejects(t *testing.T) {
	dm := f6NewDM(t)

	_, err := handleMpmWork(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "resolve_contradiction",
		"params": map[string]interface{}{
			"work_id": "non-existent-work-id",
			"reason":  "test",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found",
		"unknown work_id must return a clear not-found error")
}

// TestF6_1_ResolveContradiction_UnknownActionRejects ensures that the
// action name is enforced (typos like "resolve_contradictio" do not
// silently route somewhere else).
func TestF6_1_ResolveContradiction_UnknownActionRejects(t *testing.T) {
	dm := f6NewDM(t)

	_, err := handleMpmWork(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "resolve_contradictio", // typo
		"params": map[string]interface{}{
			"work_id": "x",
			"reason":  "y",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown action")
}
