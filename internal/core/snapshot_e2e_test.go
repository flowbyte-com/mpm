// snapshot_e2e_test.go — end-to-end: record tool call → save memory →
// verify the row carries an _epistemic_snapshot block.

package internal

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestSaveMemoryWithContextAndSnapshot_InjectsBlock is the integration
// contract: when a WrapperContext is supplied, the saved memory row's
// metadata column contains a populated _epistemic_snapshot block. This
// is the entire reason the snapshot path exists; if it breaks, every
// memory saved via the MCP path loses its provenance/audit context.
func TestSaveMemoryWithContextAndSnapshot_InjectsBlock(t *testing.T) {
	dm := NewTestDM(t)
	t.Cleanup(func() { dm.Close() })
	ResetForTest()

	// Seed a RecentTool record so the resolver has something to stamp.
	rec := &ToolCallRecord{
		ToolName:   "read_file",
		URI:        "/tmp/example.md",
		CalledAt:   time.Now().Add(-2 * time.Second),
		ResultHash: "abc123def456",
	}
	GlobalToolBuffer().Record("e2e-session", rec)

	wc := &WrapperContext{
		AgentID:         "main",
		SessionID:       "e2e-session",
		Model:           "minimax-portal/MiniMax-M3",
		RecentTool:      GlobalToolBuffer().Head("e2e-session"),
		ConfidenceBand:  "high",
		ReasoningDepth:  "deep",
	}
	ac := ActiveContext{
		Mode:    "audit",
		Persona: "artisan",
		Agent:   "main",
		Model:   "minimax-portal/MiniMax-M3",
	}

	out, mem, err := dm.SaveMemoryWithContextAndSnapshot(
		"test fact for snapshot injection",
		"memories", nil, 0.5, "", ac, wc,
	)
	if err != nil {
		t.Fatalf("SaveMemoryWithContextAndSnapshot: %v", err)
	}
	if mem == nil {
		t.Fatal("returned Memory is nil")
	}
	if out["id"] != mem.ID {
		t.Errorf("result.id %v != mem.ID %s", out["id"], mem.ID)
	}

	// Pull the row's metadata back out and verify the snapshot landed.
	var rawMeta string
	err = dm.SQLDB().QueryRow(
		`SELECT metadata FROM memories WHERE id = ?`, mem.ID,
	).Scan(&rawMeta)
	if err != nil {
		t.Fatalf("read back metadata: %v", err)
	}

	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(rawMeta), &meta); err != nil {
		t.Fatalf("metadata not valid JSON: %v", err)
	}

	snap, ok := meta["_epistemic_snapshot"].(map[string]interface{})
	if !ok {
		t.Fatalf("_epistemic_snapshot missing from metadata: %v", meta)
	}

	// Pin the must-have fields.
	if snap["schema_version"] != SnapshotSchemaVersion {
		t.Errorf("schema_version: want %s, got %v", SnapshotSchemaVersion, snap["schema_version"])
	}

	creator, ok := snap["creator"].(map[string]interface{})
	if !ok {
		t.Fatalf("creator block missing")
	}
	if creator["agent_id"] != "main" {
		t.Errorf("creator.agent_id: want main, got %v", creator["agent_id"])
	}
	if creator["session_id"] != "e2e-session" {
		t.Errorf("creator.session_id: want e2e-session, got %v", creator["session_id"])
	}

	exec, ok := snap["execution"].(map[string]interface{})
	if !ok {
		t.Fatalf("execution block missing")
	}
	if exec["confidence_band"] != "high" {
		t.Errorf("execution.confidence_band: want high, got %v", exec["confidence_band"])
	}
	if exec["reasoning_depth"] != "deep" {
		t.Errorf("execution.reasoning_depth: want deep, got %v", exec["reasoning_depth"])
	}
	if exec["captured_at"] == nil {
		t.Error("execution.captured_at missing")
	}

	prov, ok := snap["provenance"].(map[string]interface{})
	if !ok {
		t.Fatalf("provenance block missing — RecentTool was provided, snapshot should reflect it")
	}
	if prov["source_tool"] != "read_file" {
		t.Errorf("provenance.source_tool: want read_file, got %v", prov["source_tool"])
	}
	if prov["uri"] != "/tmp/example.md" {
		t.Errorf("provenance.uri: want /tmp/example.md, got %v", prov["uri"])
	}
	if w, ok := prov["observation_window_ms"].(float64); !ok || w <= 0 || w > 60000 {
		t.Errorf("provenance.observation_window_ms out of range: %v", prov["observation_window_ms"])
	}
}

// TestSaveMemoryWithContext_NoWrapperContext_NoSnapshot pins backward
// compat: the legacy save path (nil WrapperContext) must NOT stamp a
// snapshot. Legacy callers and CLI invocations continue working
// unchanged.
func TestSaveMemoryWithContext_NoWrapperContext_NoSnapshot(t *testing.T) {
	dm := NewTestDM(t)
	t.Cleanup(func() { dm.Close() })
	ResetForTest()

	ac := ActiveContext{Mode: "audit", Persona: "artisan"}

	out, mem, err := dm.SaveMemoryWithContext(
		"legacy save — no snapshot expected",
		"memories", nil, 0.5, "", ac,
	)
	if err != nil {
		t.Fatalf("SaveMemoryWithContext: %v", err)
	}

	var rawMeta string
	if err := dm.SQLDB().QueryRow(
		`SELECT metadata FROM memories WHERE id = ?`, mem.ID,
	).Scan(&rawMeta); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if strings.Contains(rawMeta, "_epistemic_snapshot") {
		t.Errorf("legacy path stamped a snapshot — backward compat broken: %s", rawMeta)
	}

	// But the legacy provenance block must still be there (DecayWeights contract).
	if !strings.Contains(rawMeta, `"provenance"`) {
		t.Error("legacy metadata.provenance block missing — DecayWeights contract broken")
	}

	_ = out
}

// TestSaveMemoryWithContextAndSnapshot_NilRecentToolOmitsProvenance —
// when the buffer is empty (no preceding tool), provenance is omitted
// but the other blocks still land. This is the "pure reasoning save"
// case: the agent saved a memory without observing anything first.
func TestSaveMemoryWithContextAndSnapshot_NilRecentToolOmitsProvenance(t *testing.T) {
	dm := NewTestDM(t)
	t.Cleanup(func() { dm.Close() })
	ResetForTest()

	wc := &WrapperContext{
		AgentID:    "main",
		SessionID:  "reasoning-session",
		Model:      "minimax-portal/MiniMax-M3",
		RecentTool: nil, // pure reasoning — no observation
	}
	ac := ActiveContext{Mode: "audit", Persona: "artisan", Agent: "main"}

	_, mem, err := dm.SaveMemoryWithContextAndSnapshot(
		"thought without observation",
		"memories", nil, 0.5, "", ac, wc,
	)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	var rawMeta string
	if err := dm.SQLDB().QueryRow(
		`SELECT metadata FROM memories WHERE id = ?`, mem.ID,
	).Scan(&rawMeta); err != nil {
		t.Fatalf("read back: %v", err)
	}

	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(rawMeta), &meta); err != nil {
		t.Fatalf("metadata JSON: %v", err)
	}
	snap := meta["_epistemic_snapshot"].(map[string]interface{})

	if snap["provenance"] != nil {
		t.Errorf("expected provenance omitted when RecentTool nil, got %v", snap["provenance"])
	}
	// Creator and execution should still be present.
	if snap["creator"] == nil {
		t.Error("creator missing — should still be stamped even without provenance")
	}
	if snap["execution"] == nil {
		t.Error("execution missing — should still be stamped even without provenance")
	}
}

// TestSaveMemoryWithContextAndSnapshot_StaleToolDropsProvenance — when
// RecentTool is older than the 60s window, provenance is dropped (not
// weakened). Other blocks still land.
func TestSaveMemoryWithContextAndSnapshot_StaleToolDropsProvenance(t *testing.T) {
	dm := NewTestDM(t)
	t.Cleanup(func() { dm.Close() })
	ResetForTest()

	staleRec := &ToolCallRecord{
		ToolName: "read_file",
		URI:      "/tmp/old.md",
		CalledAt: time.Now().Add(-5 * time.Minute), // well past 60s
	}
	GlobalToolBuffer().Record("stale-session", staleRec)

	wc := &WrapperContext{
		AgentID:    "main",
		SessionID:  "stale-session",
		Model:      "minimax-portal/MiniMax-M3",
		RecentTool: GlobalToolBuffer().Head("stale-session"),
	}
	ac := ActiveContext{Agent: "main"}

	_, mem, err := dm.SaveMemoryWithContextAndSnapshot(
		"stale observation should drop",
		"memories", nil, 0.5, "", ac, wc,
	)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	var rawMeta string
	if err := dm.SQLDB().QueryRow(
		`SELECT metadata FROM memories WHERE id = ?`, mem.ID,
	).Scan(&rawMeta); err != nil {
		t.Fatalf("read back: %v", err)
	}

	var meta map[string]interface{}
	json.Unmarshal([]byte(rawMeta), &meta)
	snap := meta["_epistemic_snapshot"].(map[string]interface{})

	if snap["provenance"] != nil {
		t.Errorf("stale tool should be dropped (not stamped with window_ms > 60s), got %v", snap["provenance"])
	}
}