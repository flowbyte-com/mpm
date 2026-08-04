// backfill_snapshots_test.go — end-to-end verification of the
// legacy-memory migration path.

package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// seedLegacyMemory inserts a memory row that mimics the pre-resolver
// era: no _epistemic_snapshot in metadata, an optional session_id, and
// an optional legacy metadata.provenance block from which creator
// fields can be derived.
func seedLegacyMemory(t *testing.T, dm *DatabaseManager, id, sessionID, agentID, model string, withProvenance bool) {
	t.Helper()

	var metaJSON string
	if withProvenance {
		prov := map[string]interface{}{
			"source":  "agent",
			"model":   model,
			"compute": "relative",
			"agent":   agentID,
		}
		meta := map[string]interface{}{
			"provenance": prov,
		}
		b, _ := json.Marshal(meta)
		metaJSON = string(b)
	} else {
		metaJSON = `{}`
	}

	var sessArg interface{} = nil
	if sessionID != "" {
		sessArg = sessionID
	}

	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, session_id, tags, metadata, created_at, updated_at)
		VALUES (?, 'memories', ?, ?, '[]', ?, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
	`, id, "test content for "+id, sessArg, metaJSON)
	if err != nil {
		t.Fatalf("seedLegacyMemory: %v", err)
	}
}

func readMetadata(t *testing.T, dm *DatabaseManager, id string) map[string]interface{} {
	t.Helper()
	var raw string
	if err := dm.SQLDB().QueryRow(`SELECT metadata FROM memories WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("metadata not valid JSON: %v", err)
	}
	return meta
}

func TestBackfillSnapshots_StampsCreatorFromProvenance(t *testing.T) {
	dm := NewTestDM(t)
	t.Cleanup(func() { dm.Close() })

	seedLegacyMemory(t, dm, "mem-creator-1", "sess-1", "main", "minimax-portal/MiniMax-M3", true)

	report, err := BackfillSnapshots(context.Background(), dm, BackfillOptions{BatchSize: 100})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.Candidates != 1 {
		t.Errorf("candidates: want 1, got %d", report.Candidates)
	}
	if report.Backfilled != 1 {
		t.Errorf("backfilled: want 1, got %d", report.Backfilled)
	}

	meta := readMetadata(t, dm, "mem-creator-1")
	snap, ok := meta["_epistemic_snapshot"].(map[string]interface{})
	if !ok {
		t.Fatalf("_epistemic_snapshot missing from stamped metadata")
	}
	if snap["schema_version"] != SnapshotSchemaVersion {
		t.Errorf("schema_version: want %s, got %v", SnapshotSchemaVersion, snap["schema_version"])
	}
	creator, ok := snap["creator"].(map[string]interface{})
	if !ok {
		t.Fatalf("creator missing")
	}
	if creator["agent_id"] != "main" {
		t.Errorf("creator.agent_id: want main, got %v", creator["agent_id"])
	}
	if creator["session_id"] != "sess-1" {
		t.Errorf("creator.session_id: want sess-1, got %v", creator["session_id"])
	}
	if creator["model"] != "minimax-portal/MiniMax-M3" {
		t.Errorf("creator.model: got %v", creator["model"])
	}
}

func TestBackfillSnapshots_PartialWithoutSessionID(t *testing.T) {
	dm := NewTestDM(t)
	t.Cleanup(func() { dm.Close() })

	// No session_id, no provenance block. Backfill should still
	// succeed but mark the row as partial.
	seedLegacyMemory(t, dm, "mem-no-session", "", "", "", false)

	report, err := BackfillSnapshots(context.Background(), dm, BackfillOptions{BatchSize: 100})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.Backfilled != 1 {
		t.Errorf("backfilled: want 1, got %d", report.Backfilled)
	}
	if report.Partial != 1 {
		t.Errorf("partial: want 1, got %d", report.Partial)
	}

	meta := readMetadata(t, dm, "mem-no-session")
	snap := meta["_epistemic_snapshot"].(map[string]interface{})
	creator := snap["creator"].(map[string]interface{})
	if creator["agent_id"] != "" {
		t.Errorf("creator.agent_id: want empty, got %v", creator["agent_id"])
	}
	if creator["session_id"] != "" {
		t.Errorf("creator.session_id: want empty, got %v", creator["session_id"])
	}
}

func TestBackfillSnapshots_SkipsAlreadyStamped(t *testing.T) {
	dm := NewTestDM(t)
	t.Cleanup(func() { dm.Close() })

	// Pre-stamped memory — should be skipped, not double-stamped.
	snap := &EpistemicSnapshot{
		SchemaVersion: SnapshotSchemaVersion,
		Creator:       &CreatorContext{AgentID: "existing", SessionID: "sess-existing"},
	}
	meta := map[string]interface{}{
		"_epistemic_snapshot": map[string]interface{}{
			"schema_version": snap.SchemaVersion,
			"creator":        map[string]interface{}{"agent_id": "existing", "session_id": "sess-existing"},
		},
	}
	metaBytes, _ := json.Marshal(meta)
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, session_id, tags, metadata, created_at, updated_at)
		VALUES ('mem-existing', 'memories', 'existing content', 'sess-existing', '[]', ?, 0, 0)
	`, string(metaBytes))
	if err != nil {
		t.Fatalf("seed existing: %v", err)
	}

	report, err := BackfillSnapshots(context.Background(), dm, BackfillOptions{BatchSize: 100})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.Candidates != 0 {
		t.Errorf("candidates: want 0 (pre-stamped excluded), got %d", report.Candidates)
	}
	if report.Backfilled != 0 {
		t.Errorf("backfilled: want 0, got %d", report.Backfilled)
	}
}

func TestBackfillSnapshots_StampsValidationFromEvidence(t *testing.T) {
	dm := NewTestDM(t)
	t.Cleanup(func() { dm.Close() })

	seedLegacyMemory(t, dm, "mem-with-evidence", "sess-2", "main", "minimax-portal/MiniMax-M3", true)

	// Add a positive-strength evidence row — backfill should pick it up.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group,
		                      strength, independence_factor, created_by, created_at, expires_at, notes)
		VALUES (?, ?, 'memory', 'test', 'manual',
		        0.8, 1.0, 'tester', CAST(strftime('%s','now') AS INTEGER), NULL, 'backfill test')
	`, "ev-pos-1", "mem-with-evidence")
	if err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	report, err := BackfillSnapshots(context.Background(), dm, BackfillOptions{BatchSize: 100})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.Backfilled != 1 {
		t.Fatalf("backfilled: want 1, got %d", report.Backfilled)
	}

	meta := readMetadata(t, dm, "mem-with-evidence")
	snap := meta["_epistemic_snapshot"].(map[string]interface{})
	validation, ok := snap["validation"].(map[string]interface{})
	if !ok {
		t.Fatalf("validation missing")
	}
	if validation["status"] != "corroborated" {
		t.Errorf("validation.status: want corroborated, got %v", validation["status"])
	}
	if v, _ := validation["evidence_count"].(float64); int(v) != 1 {
		t.Errorf("validation.evidence_count: want 1, got %v", validation["evidence_count"])
	}
	if validation["trigger_evidence_id"] != "ev-pos-1" {
		t.Errorf("validation.trigger_evidence_id: want ev-pos-1, got %v", validation["trigger_evidence_id"])
	}
	if validation["last_validated_at"] == nil {
		t.Error("validation.last_validated_at missing")
	}
}

func TestBackfillSnapshots_ContradictedFromChallenge(t *testing.T) {
	dm := NewTestDM(t)
	t.Cleanup(func() { dm.Close() })

	seedLegacyMemory(t, dm, "mem-challenged", "sess-3", "main", "minimax-portal/MiniMax-M3", true)

	// Mix positive + challenge. Challenge wins (most recent with
	// non-zero strength supersedes per the same logic as
	// computeValidationState).
	_, err := dm.SQLDB().Exec(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group,
		                      strength, independence_factor, created_by, created_at, expires_at, notes)
		VALUES
		  ('ev-pos', ?, 'memory', 'test', 'manual', 0.7, 1.0, 'tester', CAST(strftime('%s','now', '-1 hour') AS INTEGER), NULL, ''),
		  ('ev-chal', ?, 'memory', 'challenge', 'manual', -1.0, 1.0, 'tester', CAST(strftime('%s','now') AS INTEGER), NULL, 'rebuttal')
	`, "mem-challenged", "mem-challenged")
	if err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	report, err := BackfillSnapshots(context.Background(), dm, BackfillOptions{BatchSize: 100})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.Backfilled != 1 {
		t.Fatalf("backfilled: want 1, got %d", report.Backfilled)
	}

	meta := readMetadata(t, dm, "mem-challenged")
	snap := meta["_epistemic_snapshot"].(map[string]interface{})
	validation := snap["validation"].(map[string]interface{})
	if validation["status"] != "contradicted" {
		t.Errorf("validation.status: want contradicted, got %v", validation["status"])
	}
}

func TestBackfillSnapshots_DryRunDoesNotWrite(t *testing.T) {
	dm := NewTestDM(t)
	t.Cleanup(func() { dm.Close() })

	seedLegacyMemory(t, dm, "mem-dryrun", "sess-4", "main", "minimax-portal/MiniMax-M3", true)

	report, err := BackfillSnapshots(context.Background(), dm, BackfillOptions{
		BatchSize: 100,
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.Candidates != 1 {
		t.Errorf("candidates: want 1, got %d", report.Candidates)
	}
	if report.Previewed != 1 {
		t.Errorf("previewed: want 1, got %d", report.Previewed)
	}
	if report.Backfilled != 0 {
		t.Errorf("backfilled: want 0 (dry-run), got %d", report.Backfilled)
	}

	// Verify the row's metadata is unchanged.
	var raw string
	if err := dm.SQLDB().QueryRow(`SELECT metadata FROM memories WHERE id = ?`, "mem-dryrun").Scan(&raw); err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(raw, "_epistemic_snapshot") {
		t.Errorf("dry-run wrote to the database — metadata now contains snapshot: %s", raw)
	}
}

func TestBackfillSnapshots_BatchSizeCeiling(t *testing.T) {
	dm := NewTestDM(t)
	t.Cleanup(func() { dm.Close() })

	_, err := BackfillSnapshots(context.Background(), dm, BackfillOptions{BatchSize: 50000})
	if err == nil {
		t.Fatal("expected error for batch size > safety ceiling")
	}
	if !strings.Contains(err.Error(), "safety ceiling") {
		t.Errorf("error message: want safety ceiling, got %v", err)
	}
}

func TestBackfillSnapshots_ChunkedWithMultipleBatches(t *testing.T) {
	dm := NewTestDM(t)
	t.Cleanup(func() { dm.Close() })

	// Seed 12 memories. BatchSize 5 → 3 batches.
	for i := 0; i < 12; i++ {
		id := "mem-batch-" + leftPad(i, 2)
		seedLegacyMemory(t, dm, id, "sess", "main", "model", true)
	}

	start := time.Now()
	report, err := BackfillSnapshots(context.Background(), dm, BackfillOptions{BatchSize: 5})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}

	if report.Backfilled != 12 {
		t.Errorf("backfilled: want 12, got %d", report.Backfilled)
	}
	// 3 batches × 50ms yield = 150ms minimum wall-clock. This is the
	// cost of protecting concurrent writers from starvation.
	if elapsed < 150*time.Millisecond {
		t.Errorf("expected >= 150ms elapsed (3 yields × 50ms); got %v", elapsed)
	}
	if report.BatchesRun != 3 {
		t.Errorf("batches_run: want 3, got %d", report.BatchesRun)
	}
}

func leftPad(n, width int) string {
	return fmt.Sprintf("%0*d", width, n)
}