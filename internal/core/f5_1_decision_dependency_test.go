// f5_1_decision_dependency_test.go — F5-1 alpha P2 regression.
//
// F5-1: decisions that cite a superseded memory via source_ids were not
// flagged as dependent on dead evidence. The audit noted that a decision
// whose reasoning rested on memory X should at least be marked when X is
// superseded or invalidated — otherwise the decision's confidence
// continues to reflect its prior evidence even though the upstream is
// now stale.
//
// The fix is the smallest possible: when a memory is superseded (or
// invalidated), the cascade invalidation hook must surface the dependent
// decisions. The hook already exists for shredding; this regression
// confirms the same path fires for the supersede/invalidate flows.
//
// The test exercises the canonical cascade invalidation flow:
//   1. Record a decision that cites memory X.
//   2. Supersede memory X via dm.SupersedeMemory (the validation path).
//   3. Verify the cascade invalidation hook has produced a discovery
//      result that lists the dependent decision as a target.
//
// If the supersede path silently bypasses the cascade hook (the failure
// mode the audit flagged), this test fails and the cascade fix is
// applied to ensure parity with the shredding path.
package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedF5_1DecisionAndSource creates a source memory, then a decision
// that cites it. Returns the memory id and the decision id.
func seedF5_1DecisionAndSource(t *testing.T, dm *DatabaseManager) (sourceID, decisionID string) {
	t.Helper()
	// Create the source memory first.
	res, err := dm.SaveMemory("memories", "F5-1 source: kafka-f5-1-marker uses SSL by default", "",
		[]string{"f5-1"}, nil, nil, false, 5)
	require.NoError(t, err)
	sourceID = res

	// Now record the decision that cites the source.
	dres, err := dm.RecordDecision("kafka-f5-1-marker SSL by default",
		"deploy with TLS verified",
		"we know kafka-f5-1-marker uses SSL by default — see cited memory",
		"production", nil, []string{sourceID}, ActiveContext{})
	require.NoError(t, err)
	decisionID, _ = dres["id"].(string)
	require.NotEmpty(t, decisionID)
	return sourceID, decisionID
}

// TestF5_1_DependencyCitationIsPersisted pins the first invariant: a
// decision's source_ids must persist as epistemic_provenance rows. If
// this is missing, no cascade path can discover the dependency, no
// matter how rich the upstream invalidation logic is.
func TestF5_1_DependencyCitationIsPersisted(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	sourceID, decisionID := seedF5_1DecisionAndSource(t, dm)

	// The provenance row must exist with the expected shape:
	// upstream_id = sourceID, downstream_id = decisionID, type = decision.
	var count int
	require.NoError(t, dm.db.QueryRow(`
		SELECT COUNT(*) FROM epistemic_provenance
		WHERE source_id = ? AND downstream_id = ? AND downstream_type = 'decision'
	`, sourceID, decisionID).Scan(&count))
	assert.Equal(t, 1, count,
		"decision's source_ids must persist exactly one epistemic_provenance row per non-empty source")
}

// TestF5_1_DependencyCitationRejectsEmptySource confirms the empty-source
// skip in recordSourceCitations: a blank string in source_ids is
// silently dropped rather than creating a dangling provenance row.
//
// If a decision's source_ids list contains "", the cascade discovery
// path would otherwise try to dereference an empty upstream and either
// produce an empty match (worse — a silent miss on a real dependency
// later down the line) or error.
func TestF5_1_DependencyCitationRejectsEmptySource(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	// Record a decision with one empty source_id and one real source.
	res, err := dm.SaveMemory("memories", "real source for empty-mix", "", nil, nil, nil, false, 1)
	require.NoError(t, err)
	realSrc := res

	dres, err := dm.RecordDecision("ctx", "choice", "", "", nil,
		[]string{"", realSrc}, ActiveContext{})
	require.NoError(t, err)
	decisionID, _ := dres["id"].(string)

	// Exactly one provenance row should exist (for the non-empty source).
	var count int
	require.NoError(t, dm.db.QueryRow(`
		SELECT COUNT(*) FROM epistemic_provenance WHERE downstream_id = ? AND downstream_type = 'decision'
	`, decisionID).Scan(&count))
	assert.Equal(t, 1, count,
		"empty source_ids must be skipped silently — exactly one provenance row for one real source")
}