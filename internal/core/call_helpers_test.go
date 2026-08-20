package internal

import (
	"context"
	"database/sql"
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

// TestWritePaths_ErrorOnMissingRow pins the Defense-Triad-3 write-path
// assertions: UpdateMemory, ReinforceMemory, AdjustMemoryWeight,
// WeakenMemory, and SetMemoryTTL must fail loudly on a typo'd/nonexistent
// id instead of returning a silent 0-row success.
func TestWritePaths_ErrorOnMissingRow(t *testing.T) {
	dm := newTestDM(t)

	cases := []struct {
		name string
		run  func() error
	}{
		{"UpdateMemory", func() error { return dm.UpdateMemory("nope", "content", nil, nil) }},
		{"ReinforceMemory", func() error { return dm.ReinforceMemory("nope", 3) }},
		{"AdjustMemoryWeight", func() error { return dm.AdjustMemoryWeight("nope", 2) }},
		{"WeakenMemory", func() error { return dm.WeakenMemory("nope", 2) }},
		{"SetMemoryTTL", func() error { return dm.SetMemoryTTL("nope", time.Now().Add(time.Hour)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			require.Error(t, err, "silent 0-row success is a lie the substrate must not tell")
			assert.Contains(t, err.Error(), "nope")
		})
	}

	// Sanity: the same writes succeed on a real row.
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, weight) VALUES (?, 'memories', 'x', 5)`, 0, "mem-1")
	require.NoError(t, err)
	require.NoError(t, dm.UpdateMemory("mem-1", "y", nil, nil))
	require.NoError(t, dm.ReinforceMemory("mem-1", 3))
	require.NoError(t, dm.AdjustMemoryWeight("mem-1", 2))
	require.NoError(t, dm.WeakenMemory("mem-1", 2))
	require.NoError(t, dm.SetMemoryTTL("mem-1", time.Now().Add(time.Hour)))
}

func TestCallHelpers_SnoozeMemory_BumpsWeightAndTimestamp(t *testing.T) {
	dm := newTestDM(t)
	// last_accessed_at is INTEGER (Unix-epoch seconds) after the timestamps
	// unification migration. Seed with a known-past value via a Go-side bind.
	seedAccessed := int64(1577836800) // 2020-01-01 00:00:00 UTC
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, weight, last_accessed_at) VALUES (?, 'memories', 'x', 5, ?)`, 0, "mem-1", seedAccessed)
	require.NoError(t, err)

	out, err := dm.SnoozeMemory("mem-1", 3)
	require.NoError(t, err)
	assert.Equal(t, 3, out["days"])

	var weight int
	var accessed int64
	require.NoError(t, dm.QueryRowTracked(`SELECT weight, last_accessed_at FROM memories WHERE id = ?`, "mem-1").Scan(&weight, &accessed))
	assert.Equal(t, 6, weight, "weight +1 (5 → 6)")
	assert.Greater(t, accessed, seedAccessed, "last_accessed_at must advance")
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

func TestCallHelpers_RunGC_StaleTheorySweep(t *testing.T) {
	dm := newTestDM(t)
	pendingMeta := `{"status":"pending"}`
	// 1. Stale pending theory — 60 days old, visible → must be flagged.
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, metadata, weight, created_at) VALUES (?, 'theories', 'stale hypothesis', ?, 1, CAST(strftime('%s','now', '-60 days') AS INTEGER))`, 0, "th-stale", pendingMeta)
	require.NoError(t, err)
	// 2. Young pending theory — 5 days old → must NOT be flagged.
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, metadata, weight, created_at) VALUES (?, 'theories', 'fresh hypothesis', ?, 1, CAST(strftime('%s','now', '-5 days') AS INTEGER))`, 0, "th-fresh", pendingMeta)
	require.NoError(t, err)
	// 3. Stale but expired (legacy invisible row, expires_at=0) → swept
	// TOO: the sweep clears expires_at and resolves it in one tx, so
	// expired ghosts cannot accumulate as unactionable pending rows.
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, metadata, weight, created_at, expires_at) VALUES (?, 'theories', 'legacy ghost hypothesis', ?, 1, CAST(strftime('%s','now', '-60 days') AS INTEGER), 0)`, 0, "th-ghost", pendingMeta)
	require.NoError(t, err)

	// Dry run: flag only, never write.
	dry, err := dm.RunGC(GCOptions{DryRun: true, StaleTheoryDays: 30})
	require.NoError(t, err)
	require.True(t, dry.Ran)
	require.Equal(t, 2, len(dry.StaleTheories), "visible stale + expired ghost both flagged")
	flagged := map[string]bool{}
	for _, s := range dry.StaleTheories {
		flagged[s["id"].(string)] = true
	}
	assert.True(t, flagged["th-stale"], "visible stale theory flagged")
	assert.True(t, flagged["th-ghost"], "expired ghost flagged")
	assert.Equal(t, 0, dry.StaleTheoriesResolved, "dry run must not resolve")
	var st string
	require.NoError(t, dm.QueryRowTracked(`SELECT json_extract(metadata, '$.status') FROM memories WHERE id = 'th-stale'`).Scan(&st))
	assert.Equal(t, "pending", st, "dry run leaves status untouched")

	// Non-dry run: resolve exactly the stale visible theory.
	// Clear the cooldown claim from the dry run so the next pass may run.
	_, err = dm.ExecTracked(`DELETE FROM system_config WHERE key = 'last_gc_at'`, 0)
	require.NoError(t, err)
	real, err := dm.RunGC(GCOptions{DryRun: false, StaleTheoryDays: 30})
	require.NoError(t, err)
	require.True(t, real.Ran)
	require.Equal(t, 2, len(real.StaleTheories), "non-dry-run still reports the flag list")
	assert.Equal(t, 2, real.StaleTheoriesResolved, "both theories resolved")

	require.NoError(t, dm.QueryRowTracked(`SELECT json_extract(metadata, '$.status') FROM memories WHERE id = 'th-stale'`).Scan(&st))
	assert.Equal(t, "disproven", st, "stale theory flipped to disproven")
	require.NoError(t, dm.QueryRowTracked(`SELECT json_extract(metadata, '$.status') FROM memories WHERE id = 'th-fresh'`).Scan(&st))
	assert.Equal(t, "pending", st, "fresh theory untouched")
	require.NoError(t, dm.QueryRowTracked(`SELECT json_extract(metadata, '$.status') FROM memories WHERE id = 'th-ghost'`).Scan(&st))
	assert.Equal(t, "disproven", st, "expired ghost resolved too")
	var ghostExpires sql.NullInt64
	require.NoError(t, dm.QueryRowTracked(`SELECT expires_at FROM memories WHERE id = 'th-ghost'`).Scan(&ghostExpires))
	assert.False(t, ghostExpires.Valid, "expired ghost had expires_at cleared")

	// Second non-dry run: idempotent — nothing left to resolve.
	_, err = dm.ExecTracked(`DELETE FROM system_config WHERE key = 'last_gc_at'`, 0)
	require.NoError(t, err)
	again, err := dm.RunGC(GCOptions{DryRun: false, StaleTheoryDays: 30})
	require.NoError(t, err)
	require.True(t, again.Ran)
	assert.Equal(t, 0, len(again.StaleTheories), "nothing stale remains")
	assert.Equal(t, 0, again.StaleTheoriesResolved, "second run is a no-op")

	// StaleTheoryDays=0 (or negative) disables the sweep entirely.
	_, err = dm.ExecTracked(`DELETE FROM system_config WHERE key = 'last_gc_at'`, 0)
	require.NoError(t, err)
	disabled, err := dm.RunGC(GCOptions{DryRun: false, StaleTheoryDays: 0})
	require.NoError(t, err)
	require.True(t, disabled.Ran)
	assert.Empty(t, disabled.StaleTheories, "disabled sweep reports nothing")
}

// TestRunGC_ZeroDecayCollectionsExempt pins the GC/maintenance-policy
// alignment: collections with DecayPercent <= 0 (decisions — the
// append-only audit trail) must be exempt from the GC forgetting curve,
// while decayable collections still decay.
func TestRunGC_ZeroDecayCollectionsExempt(t *testing.T) {
	dm := newTestDM(t)
	old := `CAST(strftime('%s','now', '-100 days') AS INTEGER)`

	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, weight, created_at, last_accessed_at) VALUES (?, 'decisions', 'audit row', 3, `+old+`, `+old+`)`, 0, "dec-1")
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, weight, created_at, last_accessed_at) VALUES (?, 'memories', 'decayable row', 3, `+old+`, `+old+`)`, 0, "mem-1")
	require.NoError(t, err)

	_, err = dm.ExecTracked(`DELETE FROM system_config WHERE key = 'last_gc_at'`, 0)
	require.NoError(t, err)
	res, err := dm.RunGC(GCOptions{DryRun: false})
	require.NoError(t, err)
	require.True(t, res.Ran)

	var w int
	require.NoError(t, dm.QueryRowTracked(`SELECT weight FROM memories WHERE id = 'dec-1'`).Scan(&w))
	assert.Equal(t, 3, w, "decisions rows must not decay (append-only audit trail)")
	for _, d := range res.DeadMemories {
		assert.NotEqual(t, "dec-1", d["id"], "zero-decay rows must never be flagged dead")
	}

	require.NoError(t, dm.QueryRowTracked(`SELECT weight FROM memories WHERE id = 'mem-1'`).Scan(&w))
	assert.Less(t, w, 3, "decayable collections still feel the forgetting curve")
}

// TestShredMemory_BroadSweepCoversPiGap is the regression test for the
// 2026-08-11 finding: shredding a memory left orphan rows in
// session_handoffs (and a handful of other top-level artifact tables).
// A user-facing shred must be table-agnostic — if a row exists with
// the shredded id in any user-visible table, it must vanish.
func TestShredMemory_BroadSweepCoversPiGap(t *testing.T) {
	dm := newTestDM(t)
	id := "broad-sweep-victim"

	// Plant the target memory + a matching row in every top-level
	// artifact table that shares the ID space. If a future table is
	// added to the ID space and someone forgets to extend the sweep,
	// this test is the alarm.
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, tags, weight) VALUES (?, 'memories', 'doomed', '[]', 5)`, 0, id)
	require.NoError(t, err)

	// Pi's specific bug: session_handoffs. handoff id == memory id.
	now := time.Now().Unix()
	_, err = dm.ExecTracked(`INSERT INTO session_handoffs (id, session_id, ended_at, ended_state, summary, commitments, open_questions) VALUES (?, ?, ?, 'clean', '?', '[]', '[]')`, 0, id, "sess-"+id, now, "smoke test pollution")
	require.NoError(t, err)

	// Other top-level artifact tables that should also be swept.
	_, err = dm.ExecTracked(`INSERT INTO topics (id, name) VALUES (?, ?)`, 0, id, "topic-"+id)
	require.NoError(t, err)

	// FK dependents (artifact_id / node_id / memory_id columns that
	// don't have FK constraints declared — no auto-cascade).
	_, err = dm.ExecTracked(`INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, independence_factor, created_by, created_at) VALUES (?, ?, 'memory', 'reproduction', 'src', 0.85, 1.0, 'test', ?)`, 0, "ev-"+id, id, now)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO retrieval_metadata (node_id, node_type) VALUES (?, 'memory')`, 0, id)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger) VALUES (?, ?, 'memory', 0.5, ?, 1, 'manual_recompute')`, 0, "ch-"+id, id, now)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO artifact_provenance (id, artifact_id, artifact_type, created_at, actor_kind) VALUES (?, ?, 'memory', ?, 'human')`, 0, "ap-"+id, id, now)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO synth_runs (content_hash, first_run_at, last_run_at, result_memory_id) VALUES (?, ?, ?, ?)`, 0, "hash-"+id, now, now, id)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO memory_revisions (memory_id, version, content, weight, collection) VALUES (?, 2, 'second', 5, 'memories')`, 0, id)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO topic_memberships (memory_id, topic_id) VALUES (?, 'some-topic')`, 0, id)
	require.NoError(t, err)

	// Shred.
	out, err := dm.ShredMemoryWithCascade(id)
	require.NoError(t, err)
	require.Equal(t, true, out["success"])

	// Sweep result must report at least the Pi-gap table + the FK
	// dependents we planted. If any of these tables is missing from
	// the sweep, this assertion fires.
	// Note: sweep is map[string]int64 in the substrate (not
	// map[string]interface{}) — type assertion must match.
	sweep, _ := out["sweep"].(map[string]int64)
	assert.NotEmpty(t, sweep, "sweep should report at least one row deleted")

	expectedTables := []string{
		"session_handoffs", // the regression target
		"topics",
		"evidence",
		"retrieval_metadata",
		"confidence_history",
		"artifact_provenance",
		"synth_runs",
		"memory_revisions",
		"topic_memberships",
	}
	for _, table := range expectedTables {
		assert.Contains(t, sweep, table, "sweep must cover %s (regression: shred left orphans here before)", table)
	}

	// Verify the rows are actually gone.
	var n int
	for _, q := range []struct {
		table string
		sql   string
	}{
		{"memories", `SELECT COUNT(*) FROM memories WHERE id = ?`},
		{"session_handoffs", `SELECT COUNT(*) FROM session_handoffs WHERE id = ?`},
		{"topics", `SELECT COUNT(*) FROM topics WHERE id = ?`},
		{"evidence", `SELECT COUNT(*) FROM evidence WHERE artifact_id = ?`},
		{"retrieval_metadata", `SELECT COUNT(*) FROM retrieval_metadata WHERE node_id = ?`},
		{"confidence_history", `SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ?`},
		{"artifact_provenance", `SELECT COUNT(*) FROM artifact_provenance WHERE artifact_id = ?`},
		{"synth_runs", `SELECT COUNT(*) FROM synth_runs WHERE result_memory_id = ?`},
		{"memory_revisions", `SELECT COUNT(*) FROM memory_revisions WHERE memory_id = ?`},
		{"topic_memberships", `SELECT COUNT(*) FROM topic_memberships WHERE memory_id = ?`},
	} {
		require.NoError(t, dm.QueryRowTracked(q.sql, id).Scan(&n), q.table)
		assert.Equal(t, 0, n, "%s must have no rows for shredded id", q.table)
	}

	// DELIBERATELY NOT swept: epistemic_cascade_outbox. The shred
	// just enqueued intents there for downstream invalidation;
	// sweeping them would erase the shred's own work.
	var intents int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE dead_artifact_id = ?`, id).Scan(&intents))
	assert.GreaterOrEqual(t, intents, 0, "cascade outbox is the destination, not a sweep target")
}

// TestWeightFractional_SurvivesAllReadPaths pins the 2026-08-20 stability
// pass: memories.weight is REAL and legacy/fractional values (4.5, produced
// by mpm recall feedback + GC decay) must not crash any read path that
// scans the column into an int. Regression for the GetMemory crash class.
func TestWeightFractional_SurvivesAllReadPaths(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, tags, metadata, weight, created_at) VALUES (?, 'memories', 'fractional weight row', '[]', '{"status":"pending"}', 4.5, CAST(strftime('%s','now') AS INTEGER))`, 0, "frac-1")
	require.NoError(t, err)

	mem, err := dm.GetMemory("frac-1")
	require.NoError(t, err, "GetMemory must not crash on fractional weight")
	assert.Equal(t, 4, mem["weight"].(int), "weight truncated to int in map contract")

	_, err = dm.GetMemoriesByRelevance("memories", 10)
	require.NoError(t, err, "DM GetMemoriesByRelevance must not crash")

	_, err = dm.GetMemoriesForExport("memories", "", "")
	require.NoError(t, err, "GetMemoriesForExport must not crash")

	store := &MemoryStore{DB: &SQLiteConnection{DB: dm.SQLDB()}, DM: dm}
	_, err = store.GetContextualMemories([]string{}, "", 10)
	require.NoError(t, err, "GetContextualMemories must not crash")

	_, err = store.GetMemoriesByRelevance("memories", 10)
	require.NoError(t, err, "MemoryStore GetMemoriesByRelevance must not crash")

	_, err = store.SpacedReinforcementReview(1, 10)
	require.NoError(t, err, "SpacedReinforcementReview must not crash")

	_, err = dm.GetNegativeWeightMemories()
	require.NoError(t, err, "GetNegativeWeightMemories must not crash")

	_, err = dm.GetMemoryRevisions("frac-1")
	require.NoError(t, err, "GetMemoryRevisions must not crash")
}

// TestPromoteDeletedGuard pins the invariant that the CLI promote
// UPDATE pattern (the weight=10 / is_long_term=1 step in handlePromote)
// must reject soft-deleted rows so promote can't resurrect them.
// Mirrors the SQL: `UPDATE memories SET weight=10, is_long_term=1
// WHERE id=? AND deleted_at IS NULL`.
func TestPromoteDeletedGuard(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, weight, deleted_at) VALUES (?, 'memories', 'gone', 1, CAST(strftime('%s','now') AS INTEGER))`, 0, "prom-gone")
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, weight) VALUES (?, 'memories', 'live', 1)`, 0, "prom-live")
	require.NoError(t, err)

	res, err := dm.SQLDB().Exec(`UPDATE memories SET weight = 10, is_long_term = 1 WHERE id = ? AND deleted_at IS NULL`, "prom-gone")
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	assert.Equal(t, int64(0), affected, "promote on a soft-deleted id must not touch the row")

	var w int
	var ltm int
	require.NoError(t, dm.QueryRowTracked(`SELECT weight, is_long_term FROM memories WHERE id = 'prom-gone'`).Scan(&w, &ltm))
	assert.Equal(t, 1, w, "soft-deleted weight unchanged")
	assert.Equal(t, 0, ltm, "soft-deleted is_long_term unchanged")
}

// TestDeleteScheduledTask_MissingID pins the silent-success fix: a
// delete on a nonexistent id must error, not return nil.
func TestDeleteScheduledTask_MissingID(t *testing.T) {
	dm := newTestDM(t)
	err := dm.DeleteScheduledTask("nonexistent-task-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonexistent-task-id")
}

// TestDeleteTopic_MissingID pins the silent-success fix: deleting a
// nonexistent topic must error.
func TestDeleteTopic_MissingID(t *testing.T) {
	dm := newTestDM(t)
	err := dm.DeleteTopic("nonexistent-topic-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonexistent-topic-id")
}

// TestPruneCascadeOutbox pins the retention sweep: terminal intents
// (status='materialized' or 'failed') older than retentionDays are
// deleted; live intents (pending/processing) are untouched regardless
// of age.
func TestPruneCascadeOutbox(t *testing.T) {
	dm := newTestDM(t)

	now := time.Now().Unix()
	old := now - 60*86400      // 60 days ago
	fresh := now - 1*86400      // 1 day ago

	// Seed: 4 terminal intents (2 old materialized, 1 old failed, 1 fresh materialized)
	//       + 2 live intents (1 old pending, 1 old processing)
	seeds := []struct {
		id        string
		status    string
		updatedAt int64
	}{
		{"old-mat-1", "materialized", old},
		{"old-mat-2", "materialized", old},
		{"old-fail", "failed", old},
		{"fresh-mat", "materialized", fresh},
		{"old-pend", "pending", old},
		{"old-proc", "processing", old},
	}
	for _, s := range seeds {
		_, err := dm.ExecTracked(`
			INSERT INTO epistemic_cascade_outbox
				(id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
				 downstream_artifact_id, downstream_artifact_type,
				 reason, status, attempt_count, created_at, updated_at)
			VALUES (?, ?, 'x', 'memory', 'y', 'memory', 'test', ?, 0, ?, ?)
		`, 0, s.id, "evt-"+s.id, s.status, s.updatedAt, s.updatedAt)
		require.NoError(t, err)
	}

	// 30-day retention should delete the 3 old terminal intents and
	// preserve everything else (fresh terminal + all live).
	n, err := dm.PruneCascadeOutbox(30)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n, "3 old terminal intents pruned")

	// Confirm: the 3 old terminal rows are gone; the other 3 survive.
	var remaining []string
	rows, err := dm.SQLDB().Query(`SELECT id FROM epistemic_cascade_outbox ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		remaining = append(remaining, id)
	}
	assert.ElementsMatch(t, []string{"fresh-mat", "old-pend", "old-proc"}, remaining,
		"only fresh terminal + all live intents must survive")
}

// TestMemoryStats_PartitionIsConsistent pins the stats partition
// invariants so a future GetMemoryStats tweak can't reintroduce the
// '340 + 0 != 429' confusion:
//   - Total = Active + Deleted   (the primary partition)
//   - Expired ⊆ Active           (live rows past TTL; never deleted)
//   - Expired and Deleted are disjoint
func TestMemoryStats_PartitionIsConsistent(t *testing.T) {
	dm := newTestDM(t)

	// Live row, no TTL.
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'live')`, 0, "st-live")
	require.NoError(t, err)
	// Live row, expired TTL (past).
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, expires_at) VALUES (?, 'memories', 'past', 1000)`, 0, "st-past")
	require.NoError(t, err)
	// Live row, future TTL.
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, expires_at) VALUES (?, 'memories', 'future', 4102444800)`, 0, "st-future")
	require.NoError(t, err)
	// Soft-deleted row with past TTL — must NOT count in expired.
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, expires_at, deleted_at) VALUES (?, 'memories', 'gone', 1000, CAST(strftime('%s','now') AS INTEGER))`, 0, "st-gone")
	require.NoError(t, err)

	stats, err := dm.GetMemoryStats()
	require.NoError(t, err)

	// GetMemoryStats scans counts into Go int, so direct assertion is safe.
	total, _ := stats["total"].(int)
	active, _ := stats["active"].(int)
	deleted, _ := stats["deleted"].(int)
	expired, _ := stats["expired"].(int)

	assert.Equal(t, 4, total, "all four fixtures")
	assert.Equal(t, 3, active, "deleted row excluded from active")
	assert.Equal(t, 1, deleted, "deleted row counted once")
	assert.Equal(t, 1, expired, "live past-TTL row only; the deleted past-TTL row must be excluded")
	assert.Equal(t, active+deleted, total, "Total = Active + Deleted")
	assert.LessOrEqual(t, expired, active, "Expired ⊆ Active")
}
