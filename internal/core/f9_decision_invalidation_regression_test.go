// f9_decision_invalidation_regression_test.go — F9 regression.
//
// Audit finding F9: decisions had NO invalidation path — stale decisions
// co-ranked with corrections and there was no way to mark knowledge as
// superseded. The fix adds SupersedeDecision/InvalidateDecision, which
// reuse the existing correction-chain tag contract ("superseded" /
// "superseded-by:<id>") that HybridSearch's Phase 5b discount (×0.25)
// already honours — so stale decisions stop outranking current knowledge
// while remaining fully inspectable.
package internal

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedF9Decision(t *testing.T, dm *DatabaseManager, choice string) string {
	t.Helper()
	res, err := dm.RecordDecision("ctx", choice, "rationale", "", nil, nil, ActiveContext{})
	require.NoError(t, err)
	id, _ := res["id"].(string)
	require.NotEmpty(t, id)
	return id
}

func TestF9_SupersedeMarksOriginalAndRecordsReplacement(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	originalID := seedF9Decision(t, dm, "deploy with blue-green")

	res, err := dm.SupersedeDecision(originalID, "ctx", "deploy with canary", "safer rollback", "",
		nil, nil, ActiveContext{})
	require.NoError(t, err)
	newID, _ := res["id"].(string)
	require.NotEmpty(t, newID)
	assert.NotEqual(t, originalID, newID)

	// Original: marked superseded in metadata AND tags, still inspectable.
	mem, err := dm.GetMemory(originalID)
	require.NoError(t, err)
	var meta map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(mem["metadata"].(string)), &meta))
	assert.Equal(t, true, meta["superseded"])
	assert.Equal(t, newID, meta["superseded_by"])

	tagsStr, _ := mem["tags"].(string)
	assert.Contains(t, tagsStr, "superseded")
	assert.Contains(t, tagsStr, "superseded-by:"+newID)

	// Content preserved — historical inspection intact.
	content, _ := mem["content"].(string)
	assert.Contains(t, content, "CHOICE: deploy with blue-green")
}

func TestF9_SupersedeChainsAndRejectsDoubleSupersede(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	d1 := seedF9Decision(t, dm, "sqlite journal=delete")
	res, err := dm.SupersedeDecision(d1, "", "sqlite journal=wal", "", "", nil, nil, ActiveContext{})
	require.NoError(t, err)
	d2, _ := res["id"].(string)

	// Correcting the correction.
	res, err = dm.SupersedeDecision(d2, "", "sqlite journal=wal+synchronous-full", "", "", nil, nil, ActiveContext{})
	require.NoError(t, err)
	d3, _ := res["id"].(string)

	// Chain pointers resolve to the current reading.
	mem1, _ := dm.GetMemory(d1)
	meta1 := map[string]interface{}{}
	json.Unmarshal([]byte(mem1["metadata"].(string)), &meta1)
	assert.Equal(t, d2, meta1["superseded_by"])
	mem2, _ := dm.GetMemory(d2)
	meta2 := map[string]interface{}{}
	json.Unmarshal([]byte(mem2["metadata"].(string)), &meta2)
	assert.Equal(t, d3, meta2["superseded_by"])
	mem3, _ := dm.GetMemory(d3)
	meta3 := map[string]interface{}{}
	json.Unmarshal([]byte(mem3["metadata"].(string)), &meta3)
	_, stillSuperseded := meta3["superseded"]
	assert.False(t, stillSuperseded, "current decision must not be flagged stale")

	// Double-supersede of the same original is rejected with a precise error.
	_, err = dm.SupersedeDecision(d1, "", "conflicting replacement", "", "", nil, nil, ActiveContext{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already superseded by "+d2)
}

func TestF9_SupersededDecisionDiscountedInRetrievalRanking(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	staleID := seedF9Decision(t, dm, "kafka-f9-marker partition count fixed at 4")
	currentID := seedF9Decision(t, dm, "kafka-f9-marker partition count autoscaled")

	// Before invalidation both are current; after superseding the stale one,
	// its retrieval score must be discounted below the current one.
	res, err := dm.SupersedeDecision(staleID, "", "unrelated placeholder", "", "", nil, nil, ActiveContext{})
	require.NoError(t, err)
	_ = res

	staleMem, _ := dm.GetMemory(staleID)
	currentMem, _ := dm.GetMemory(currentID)
	assert.True(t, isSupersededFromMapF9(staleMem), "superseded decision must carry the ranking-discount tag")
	assert.False(t, isSupersededFromMapF9(currentMem), "current decision must not be discounted")
}

// isSupersededFromMapF9 adapts the GetMemory map shape to IsSuperseded.
func isSupersededFromMapF9(mem map[string]interface{}) bool {
	if mem == nil {
		return false
	}
	tagsStr, _ := mem["tags"].(string)
	tags := make([]string, 0)
	for _, t := range splitComma(tagsStr) {
		tags = append(tags, t)
	}
	return IsSuperseded(tags)
}

func splitComma(s string) []string {
	out := make([]string, 0)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, trimSpace(s[start:i]))
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, trimSpace(s[start:]))
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func TestF9_InvalidateRetiresWithoutReplacement(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	id := seedF9Decision(t, dm, "use in-house scheduler")

	res, err := dm.InvalidateDecision(id, "replaced by managed queue")
	require.NoError(t, err)
	assert.Equal(t, true, res["invalidated"])

	mem, err := dm.GetMemory(id)
	require.NoError(t, err)
	var meta map[string]interface{}
	json.Unmarshal([]byte(mem["metadata"].(string)), &meta)
	assert.Equal(t, true, meta["invalidated"])
	assert.Equal(t, "replaced by managed queue", meta["invalid_reason"])
	assert.True(t, isSupersededFromMapF9(mem), "invalidated decision carries the discount tag")

	// History NOT deleted.
	content, _ := mem["content"].(string)
	assert.Contains(t, content, "use in-house scheduler")

	// Non-decision artifacts rejected.
	other, err := dm.SaveMemory("memories", "not a decision", "", nil, nil, nil, false, 1)
	require.NoError(t, err)
	_, err = dm.InvalidateDecision(other, "nope")
	assert.Error(t, err)
}
