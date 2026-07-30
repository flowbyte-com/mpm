package internal

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallHelpers_AddEvidence_HappyPath(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'parser error observed')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:   "mem-1",
		ArtifactType: "memory",
		Type:         "reproduction",
		SourceGroup:  "test-rig-1",
		Strength:     0.85,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)
	assert.Contains(t, out, "success")
	assert.Equal(t, true, out["success"])
	assert.Contains(t, out, "confidence")
	conf, ok := out["confidence"].(float64)
	require.True(t, ok)
	assert.Greater(t, conf, 0.5, "positive evidence should raise confidence above initial 0.8")

	// Evidence row should exist.
	var n int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM evidence WHERE artifact_id = ?`, "mem-1").Scan(&n))
	assert.Equal(t, 1, n)
}

func TestCallHelpers_AddEvidence_RejectsInvalidType(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-2")
	require.NoError(t, err)

	_, err = dm.AddEvidence(EvidenceInput{
		ArtifactID:   "mem-2",
		ArtifactType: "memory",
		Type:         "bogus_type",
		SourceGroup:  "test",
		CreatedBy:    "test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid evidence type")
}

func TestCallHelpers_AddEvidence_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:   "",
		ArtifactType: "memory",
		Type:         "reproduction",
		SourceGroup:  "test",
		CreatedBy:    "test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}

func TestCallHelpers_ListEvidence_EmptyArtifact(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.ListEvidence("mem-1", "memory")
	require.NoError(t, err)
	assert.Contains(t, out, "evidence")
	items, ok := out["evidence"].([]map[string]interface{})
	require.True(t, ok)
	assert.Empty(t, items)
}

func TestCallHelpers_ListEvidence_FiltersByType(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO lessons (id, type, content, created) VALUES (?, 'insight', 'y', STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))`, 0, "les-1")
	require.NoError(t, err)
	require.NoError(t, AddEvidence(dm, EvidenceInput{ArtifactID: "mem-1", ArtifactType: "memory", Type: "observation", SourceGroup: "g", Strength: 0.4, CreatedBy: "t", CreatedAt: time.Now()}))
	require.NoError(t, AddEvidence(dm, EvidenceInput{ArtifactID: "les-1", ArtifactType: "lesson", Type: "observation", SourceGroup: "g", Strength: 0.4, CreatedBy: "t", CreatedAt: time.Now()}))

	memItems, err := dm.ListEvidence("mem-1", "memory")
	require.NoError(t, err)
	items, _ := memItems["evidence"].([]map[string]interface{})
	assert.Len(t, items, 1)
	assert.Equal(t, "mem-1", items[0]["artifact_id"])
}

func TestCallHelpers_ListEvidence_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ListEvidence("", "memory")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}

func TestCallHelpers_QueryConfidenceHistory_DefaultLimit(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	// Insert 60 history rows to exceed the default limit.
	for i := 0; i < 60; i++ {
		_, err := dm.ExecTracked(`INSERT INTO confidence_history (artifact_id, artifact_type, computed_at, confidence, evidence_count, trigger) VALUES (?, 'memory', ?, 0.5, 0, 'manual_recompute')`, 0, "mem-1", int64(1700000000+i))
		require.NoError(t, err)
	}

	out, err := dm.QueryConfidenceHistory("mem-1", "memory", 0)
	require.NoError(t, err)
	hist, ok := out["history"].([]map[string]interface{})
	require.True(t, ok)
	assert.Len(t, hist, 50, "default limit is 50")
}

func TestCallHelpers_QueryConfidenceHistory_CustomLimit(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		_, err := dm.ExecTracked(`INSERT INTO confidence_history (artifact_id, artifact_type, computed_at, confidence, evidence_count, trigger) VALUES (?, 'memory', ?, 0.5, 0, 'manual_recompute')`, 0, "mem-1", int64(1700000000+i))
		require.NoError(t, err)
	}

	out, err := dm.QueryConfidenceHistory("mem-1", "memory", 2)
	require.NoError(t, err)
	hist, _ := out["history"].([]map[string]interface{})
	assert.Len(t, hist, 2)
}

func TestCallHelpers_QueryConfidenceHistory_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.QueryConfidenceHistory("", "memory", 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}

func TestCallHelpers_QueryConfidenceChanges_SinceSecondsAgo(t *testing.T) {
	dm := newTestDM(t)
	out, err := dm.QueryConfidenceChanges(ConfidenceChangesFilter{
		Since: time.Now().Add(-1 * time.Hour),
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Contains(t, out, "changes")
	assert.Contains(t, out, "count")
}

func TestCallHelpers_QueryConfidenceChanges_FilterByArtifact(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.QueryConfidenceChanges(ConfidenceChangesFilter{
		ArtifactID:   "mem-1",
		ArtifactType: "memory",
		Limit:        10,
	})
	require.NoError(t, err)
	changes, _ := out["changes"].([]map[string]interface{})
	for _, c := range changes {
		assert.Equal(t, "mem-1", c["artifact_id"])
	}
}

func TestCallHelpers_QueryConfidenceTrend_HappyPath(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.QueryConfidenceTrend("mem-1", "memory", 30)
	require.NoError(t, err)
	assert.Contains(t, out, "trend")
}

func TestCallHelpers_QueryConfidenceTrend_DefaultWindow(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.QueryConfidenceTrend("mem-1", "memory", 0)
	require.NoError(t, err)
	_, ok := out["trend"]
	assert.True(t, ok)
}

func TestCallHelpers_QueryConfidenceTrend_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.QueryConfidenceTrend("", "memory", 30)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}

func TestCallHelpers_ShowConfidence_NestedHistoryShape(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO confidence_history (artifact_id, artifact_type, computed_at, confidence, evidence_count, trigger) VALUES (?, 'memory', 1700000000, 0.5, 0, 'manual_recompute')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.ShowConfidence("mem-1", "memory")
	require.NoError(t, err)
	assert.Contains(t, out, "current")
	_, hasCurrent := out["current"].(float64)
	assert.True(t, hasCurrent, "current must be a float64")
	hist, hasHist := out["history"].(map[string]interface{})
	assert.True(t, hasHist, "history must be a nested map (NOT a slice)")
	_, hasInnerHist := hist["history"].([]map[string]interface{})
	assert.True(t, hasInnerHist, "history.history must be the rows slice")
}

func TestCallHelpers_QueryMemoryQuality_ReturnsPerSource(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, metadata) VALUES (?, 'memories', 'x', ?)`, 0, "mem-1", `{"provenance":{"model":"gpt-4o"}}`)
	require.NoError(t, err)

	out, err := dm.QueryMemoryQuality()
	require.NoError(t, err)
	assert.Contains(t, out, "sources")
	assert.Contains(t, out, "count")
}

func TestCallHelpers_RecomputeConfidence_TriggersRecompute(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)
	require.NoError(t, AddEvidence(dm, EvidenceInput{ArtifactID: "mem-1", ArtifactType: "memory", Type: "reproduction", SourceGroup: "g", Strength: 0.85, CreatedBy: "t", CreatedAt: time.Now()}))

	out, err := dm.RecomputeConfidence("mem-1", "memory")
	require.NoError(t, err)
	// Returns the same shape as ShowConfidence.
	_, hasCurrent := out["current"].(float64)
	assert.True(t, hasCurrent)
	_, hasHist := out["history"].(map[string]interface{})
	assert.True(t, hasHist)
}

func TestCallHelpers_RecomputeConfidence_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.RecomputeConfidence("", "memory")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}

func TestCallHelpers_ExplainConfidence_ComponentBreakdown(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)
	require.NoError(t, AddEvidence(dm, EvidenceInput{ArtifactID: "mem-1", ArtifactType: "memory", Type: "reproduction", SourceGroup: "g", Strength: 0.85, CreatedBy: "t", CreatedAt: time.Now()}))

	out, err := dm.ExplainConfidence("mem-1", "memory")
	require.NoError(t, err)
	assert.Equal(t, true, out["success"])
	assert.Contains(t, out, "explanation")
	_, isMap := out["explanation"].(map[string]interface{})
	assert.True(t, isMap, "explanation must be a map (ConfidenceExplanation JSON shape)")
}

func TestCallHelpers_ExplainConfidence_RequiresArtifactID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExplainConfidence("", "memory")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact_id")
}

// ── Memory feedback / mutation tool tests ────────────────────────────────────
//
// These exercise the 7 DM methods backing the new shred_memory /
// reinforce_memory / weaken_memory / snooze_memory / set_memory_weight /
// patch_memory / promote_memory tools. Both the `mpm call <tool>` CLI
// and the MCP server route through these methods, so a single test of
// the DM layer covers both surfaces.

func TestCallHelpers_ShredMemory_HappyPath(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.ShredMemoryWithCascade("mem-1")
	require.NoError(t, err)
	assert.Equal(t, true, out["success"])
	assert.Equal(t, "mem-1", out["memory_id"])
	assert.Equal(t, true, out["shredded"])

	// Verify the row is actually gone.
	var n int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM memories WHERE id = ?`, "mem-1").Scan(&n))
	assert.Equal(t, 0, n)
}

func TestCallHelpers_ShredMemory_CascadesToChallengedTheory(t *testing.T) {
	dm := newTestDM(t)
	// Theory row that the memory challenged
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, tags) VALUES (?, 'theories', 'old theory', '[]')`, 0, "theory-1")
	require.NoError(t, err)
	// Memory that challenged the theory, with metadata linking them.
	// Provide all columns GetMemory scans (collection, content, session_id,
	// tags, metadata, created_at, weight, ...) so the read doesn't fail on
	// NULL-to-string conversion. Same defect in other tests below.
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, tags, metadata, weight) VALUES (?, 'memories', 'x', '[]', ?, 5)`, 0, "mem-1",
		`{"challenged_theory_id":"theory-1"}`)
	require.NoError(t, err)
	// Membership row
	_, err = dm.ExecTracked(`INSERT INTO topic_memberships (memory_id, topic_id, role) VALUES (?, ?, 'manual')`, 0, "mem-1", "topic-1")
	require.NoError(t, err)

	out, err := dm.ShredMemoryWithCascade("mem-1")
	require.NoError(t, err)
	assert.Equal(t, true, out["success"])
	assert.Equal(t, "theory-1", out["theory_purged"], "challenged theory must be cascade-purged")

	// Verify both rows are gone, and the membership row too.
	var memCount, theoryCount, memberCount int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM memories WHERE id = ?`, "mem-1").Scan(&memCount))
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM memories WHERE id = ?`, "theory-1").Scan(&theoryCount))
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM topic_memberships WHERE memory_id = ?`, "mem-1").Scan(&memberCount))
	assert.Equal(t, 0, memCount)
	assert.Equal(t, 0, theoryCount)
	assert.Equal(t, 0, memberCount, "topic_memberships must be cleaned up — orphan rows would mislead recall")
}

func TestCallHelpers_ShredMemory_RequiresID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ShredMemoryWithCascade("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "memory_id")
}

func TestCallHelpers_ReinforceMemory_HappyPath(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, reinforcement_count, weight) VALUES (?, 'memories', 'x', 2, 5)`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.ReinforceMemoryTool("mem-1", 3)
	require.NoError(t, err)
	assert.Equal(t, true, out["success"])
	assert.Equal(t, "mem-1", out["memory_id"])
	assert.Equal(t, 3, out["delta"])

	var count int
	require.NoError(t, dm.QueryRowTracked(`SELECT reinforcement_count FROM memories WHERE id = ?`, "mem-1").Scan(&count))
	assert.Equal(t, 5, count, "2 + 3 = 5")
}

func TestCallHelpers_ReinforceMemory_DefaultDelta(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, reinforcement_count) VALUES (?, 'memories', 'x', 0)`, 0, "mem-1")
	require.NoError(t, err)

	// delta=0 must default to 1 (not no-op)
	out, err := dm.ReinforceMemoryTool("mem-1", 0)
	require.NoError(t, err)
	assert.Equal(t, 1, out["delta"])
}

func TestCallHelpers_WeakenMemory_HonorsFloor(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, weight) VALUES (?, 'memories', 'x', 2)`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.WeakenMemoryTool("mem-1", 5)
	require.NoError(t, err)
	assert.Equal(t, -5, out["delta"])

	var w int
	require.NoError(t, dm.QueryRowTracked(`SELECT weight FROM memories WHERE id = ?`, "mem-1").Scan(&w))
	assert.GreaterOrEqual(t, w, 1, "weight floor at 1 — a typo can't drive weight negative")
}

func TestCallHelpers_SnoozeMemory_BumpsWeightAndTimestamp(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, weight, last_accessed_at) VALUES (?, 'memories', 'x', 5, '2020-01-01 00:00:00')`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.SnoozeMemory("mem-1", 3)
	require.NoError(t, err)
	assert.Equal(t, 3, out["days"])

	var weight int
	var accessed string
	require.NoError(t, dm.QueryRowTracked(`SELECT weight, last_accessed_at FROM memories WHERE id = ?`, "mem-1").Scan(&weight, &accessed))
	assert.Equal(t, 6, weight, "weight +1 (5 → 6)")
	assert.NotEqual(t, "2020-01-01 00:00:00", accessed, "last_accessed_at must advance")
}

func TestCallHelpers_SnoozeMemory_CapsAtLTMThreshold(t *testing.T) {
	dm := newTestDM(t)
	// weight=9 already — snooze should NOT push to 10 (would trigger LTM)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, weight) VALUES (?, 'memories', 'x', 9)`, 0, "mem-1")
	require.NoError(t, err)

	_, err = dm.SnoozeMemory("mem-1", 1)
	require.NoError(t, err)

	var weight int
	var isLTM int
	require.NoError(t, dm.QueryRowTracked(`SELECT weight, is_long_term FROM memories WHERE id = ?`, "mem-1").Scan(&weight, &isLTM))
	assert.Equal(t, 9, weight, "weight must cap at 9 — snooze must NEVER promote to LTM")
	assert.Equal(t, 0, isLTM, "snooze must not flip is_long_term")
}

func TestCallHelpers_SetMemoryWeight_ClampsAndUpdates(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, weight) VALUES (?, 'memories', 'x', 3)`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.SetMemoryWeight("mem-1", 7)
	require.NoError(t, err)
	assert.Equal(t, 7, out["weight"])

	var w int
	require.NoError(t, dm.QueryRowTracked(`SELECT weight FROM memories WHERE id = ?`, "mem-1").Scan(&w))
	assert.Equal(t, 7, w)
}

func TestCallHelpers_SetMemoryWeight_RejectsOutOfRange(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, "mem-1")
	require.NoError(t, err)

	_, err = dm.SetMemoryWeight("mem-1", -1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0-100")
	_, err = dm.SetMemoryWeight("mem-1", 101)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0-100")
}

func TestCallHelpers_SetMemoryWeight_NotFound(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.SetMemoryWeight("nonexistent", 5)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestCallHelpers_PatchMemory_MergesJSON(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, metadata) VALUES (?, 'memories', 'x', ?)`, 0, "mem-1",
		`{"existing_key":"keep_me","overwrite_me":"old"}`)
	require.NoError(t, err)

	out, err := dm.PatchMemoryMetadata("mem-1", `{"new_key":"added","overwrite_me":"new"}`)
	require.NoError(t, err)
	assert.Equal(t, true, out["success"])

	var metaStr string
	require.NoError(t, dm.QueryRowTracked(`SELECT metadata FROM memories WHERE id = ?`, "mem-1").Scan(&metaStr))
	var meta map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(metaStr), &meta))
	assert.Equal(t, "keep_me", meta["existing_key"], "merge: existing key must be preserved")
	assert.Equal(t, "added", meta["new_key"], "merge: new key must appear")
	assert.Equal(t, "new", meta["overwrite_me"], "merge: existing key in patch must overwrite")
}

func TestCallHelpers_PromoteMemory_SetsLTMFlags(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, weight, is_long_term, reinforcement_count) VALUES (?, 'memories', 'x', 3, 0, 1)`, 0, "mem-1")
	require.NoError(t, err)

	out, err := dm.PromoteMemory("mem-1")
	require.NoError(t, err)
	assert.Equal(t, true, out["success"])
	assert.Equal(t, 10, out["weight"])
	assert.Equal(t, true, out["is_long_term"])

	var weight int
	var isLTM int
	var count int
	require.NoError(t, dm.QueryRowTracked(`SELECT weight, is_long_term, reinforcement_count FROM memories WHERE id = ?`, "mem-1").Scan(&weight, &isLTM, &count))
	assert.Equal(t, 10, weight)
	assert.Equal(t, 1, isLTM)
	assert.Equal(t, 10, count, "1 + 9 = 10 — PromoteMemory must apply the +9 reinforcement")
}

// ── Workflow tool tests (Tier 2) ──────────────────────────────────────────────

func TestCallHelpers_ReviewMemories_RequiresSetup(t *testing.T) {
	dm := newTestDM(t)
	// Empty DB — review should return success with empty items, not error.
	out, err := dm.ReviewMemories(30, 20)
	require.NoError(t, err)
	assert.Equal(t, true, out["success"])
	assert.Equal(t, 0, out["count"])
	items, ok := out["items"].([]map[string]interface{})
	require.True(t, ok)
	assert.Empty(t, items)
}

func TestCallHelpers_ReviewMemories_DefaultsApplied(t *testing.T) {
	dm := newTestDM(t)
	out, err := dm.ReviewMemories(0, 0) // both invalid → defaults
	require.NoError(t, err)
	assert.Equal(t, 30, out["days"], "days<=0 must default to 30")
	assert.Equal(t, 20, out["limit"], "limit<=0 must default to 20")
}

func TestCallHelpers_ReviewMemories_FindsStaleLTM(t *testing.T) {
	dm := newTestDM(t)
	// Stale LTM memory: weight>=10, is_long_term=1, last_accessed 60 days ago.
	// GetSpacedReinforcementReview filters LTM/high-weight by last_accessed.
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, tags, weight, is_long_term, last_accessed_at) VALUES (?, 'memories', 'stale fact', '[]', 10, 1, CAST(strftime('%s','now', '-60 days') AS INTEGER))`, 0, "mem-stale")
	require.NoError(t, err)

	out, err := dm.ReviewMemories(30, 20)
	require.NoError(t, err)
	items, _ := out["items"].([]map[string]interface{})
	// The DM method should surface at least our stale LTM memory.
	foundStale := false
	for _, item := range items {
		if id, _ := item["id"].(string); id == "mem-stale" {
			foundStale = true
			break
		}
	}
	assert.True(t, foundStale, "stale LTM memory should appear in review results")
}

func TestCallHelpers_SynthesizeMemoryFor_RequiresID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.SynthesizeMemoryFor(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "memory_id")
}

func TestCallHelpers_SynthesizeMemoryFor_NotFound(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.SynthesizeMemoryFor(context.Background(), "nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ── GC tool tests (Tier 3) ───────────────────────────────────────────────────

func TestCallHelpers_RunGC_DryRunOnEmptyDB(t *testing.T) {
	dm := newTestDM(t)
	out, err := dm.RunGC(GCOptions{DryRun: true})
	require.NoError(t, err)
	assert.True(t, out.Ran, "fresh DB → cooldown claim succeeds → Ran=true")
	assert.False(t, out.CooldownSkip)
	assert.Equal(t, 0, out.Scanned)
	assert.Equal(t, 0, out.Updated, "dry run must not write")
	assert.Empty(t, out.DeadMemories)
}

func TestCallHelpers_RunGC_CooldownSkipOnSecondCall(t *testing.T) {
	dm := newTestDM(t)
	first, err := dm.RunGC(GCOptions{DryRun: true})
	require.NoError(t, err)
	require.True(t, first.Ran, "first call must claim")

	// Second call within 24h cooldown must be skipped.
	second, err := dm.RunGC(GCOptions{DryRun: true})
	require.NoError(t, err)
	assert.False(t, second.Ran, "second call within cooldown must not claim")
	assert.True(t, second.CooldownSkip, "second call must signal cooldown_skip")
	assert.NotNil(t, second.LastGCRan, "second call must report the last successful GC timestamp")
}

func TestCallHelpers_RunGC_DryRunIdentifiesDead(t *testing.T) {
	dm := newTestDM(t)
	// Memory: weight=5, last accessed 365 days ago → will decay below 0
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, tags, weight, last_accessed_at, created_at) VALUES (?, 'memories', 'old fact', '[]', 5, CAST(strftime('%s','now', '-365 days') AS INTEGER), CAST(strftime('%s','now', '-400 days') AS INTEGER))`, 0, "mem-old")
	require.NoError(t, err)
	// LTM memory: immune to decay (must NOT appear as dead)
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, tags, weight, is_long_term, last_accessed_at, created_at) VALUES (?, 'memories', 'permanent', '[]', 10, 1, CAST(strftime('%s','now', '-1000 days') AS INTEGER), CAST(strftime('%s','now', '-1000 days') AS INTEGER))`, 0, "mem-ltm")
	require.NoError(t, err)

	out, err := dm.RunGC(GCOptions{DryRun: true})
	require.NoError(t, err)
	assert.True(t, out.Ran)
	assert.GreaterOrEqual(t, out.Scanned, 2, "must scan both rows")
	assert.Equal(t, 0, out.Updated, "dry run writes nothing")

	// Dead previews must include the old non-LTM memory but NOT the LTM.
	foundOld, foundLTM := false, false
	for _, d := range out.DeadMemories {
		if id, _ := d["id"].(string); id == "mem-old" {
			foundOld = true
		}
		if id, _ := d["id"].(string); id == "mem-ltm" {
			foundLTM = true
		}
	}
	assert.True(t, foundOld, "decayed non-LTM memory should appear as dead")
	assert.False(t, foundLTM, "LTM memory must NEVER be flagged as dead")
}

func TestCallHelpers_RunGC_AppliesUpdatesWhenNotDryRun(t *testing.T) {
	dm := newTestDM(t)
	// High weight, very old — should decay but not die
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, tags, weight, last_accessed_at, created_at) VALUES (?, 'memories', 'mid-weight', '[]', 7, CAST(strftime('%s','now', '-30 days') AS INTEGER), CAST(strftime('%s','now', '-30 days') AS INTEGER))`, 0, "mem-mid")
	require.NoError(t, err)

	out, err := dm.RunGC(GCOptions{DryRun: false})
	require.NoError(t, err)
	assert.True(t, out.Ran)
	assert.GreaterOrEqual(t, out.Updated, 1, "non-dry-run must apply weight updates")

	// Verify the weight actually changed in the DB.
	var newWeight int
	require.NoError(t, dm.QueryRowTracked(`SELECT weight FROM memories WHERE id = ?`, "mem-mid").Scan(&newWeight))
	assert.Less(t, newWeight, 7, "weight must decrease after decay")
}
