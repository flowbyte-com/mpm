// handlers_provenance_test.go — tests for `mpm provenance` CLI surface.
package main

import (
	"sync/atomic"
	"testing"
)

var provSeq uint32

// testID returns a unique ID per test invocation. Safe for parallel runs because
// t.Name() is unique per test, and the per-call atomic sequence differentiates
// multiple calls within the same test.
func testID(t *testing.T) string {
	seq := atomic.AddUint32(&provSeq, 1)
	return t.Name() + "-id-" + uitoa(seq)
}

func uitoa(n uint32) string {
	if n == 0 {
		return "0"
	}
	var result []byte
	for n > 0 {
		result = append([]byte{byte('0' + n%10)}, result...)
		n /= 10
	}
	return string(result)
}

func TestHandleProvenance_MemoryWithoutProvenance(t *testing.T) {
	dm := getDB()
	if dm == nil {
		t.Skip("no test DB (getDB returned nil)")
	}
	exit := handleProvenance([]string{"mem-nonexistent-provenance-test"})
	if exit == 0 {
		t.Errorf("expected non-zero exit for missing provenance, got 0")
	}
}

func TestHandleProvenance_MemoryWithProvenance(t *testing.T) {
	dm := getDB()
	if dm == nil {
		t.Skip("no test DB (getDB returned nil)")
	}

	memID := testID(t)
	provID := testID(t)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, created_at, weight, confidence)
		 VALUES (?, 'memories', 'test', CAST(strftime('%s','now') AS INTEGER), 1, 0.5)`,
		memID,
	); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO artifact_provenance (id, artifact_id, artifact_type, actor_kind, framework_name, model_name, thinking_level, schema_version, created_at)
		 VALUES (?, ?, 'memory', 'agent', 'mpm_cli', 'sonnet', 'high', 'v1', CAST(strftime('%s','now') AS INTEGER))`,
		provID, memID,
	); err != nil {
		t.Fatalf("seed prov: %v", err)
	}
	t.Cleanup(func() {
		dm.SQLDB().Exec(`DELETE FROM artifact_provenance WHERE id = ?`, provID)
		dm.SQLDB().Exec(`DELETE FROM memories WHERE id = ?`, memID)
	})

	exit := handleProvenance([]string{memID, "--json"})
	if exit != 0 {
		t.Fatalf("handleProvenance exit = %d, want 0", exit)
	}
}

func TestHandleProvenance_Inspect(t *testing.T) {
	dm := getDB()
	if dm == nil {
		t.Skip("no test DB (getDB returned nil)")
	}

	invID := testID(t)
	memIDs := make([]string, 0, 2)
	provIDs := make([]string, 0, 2)
	for range []int{0, 1} {
		memID := testID(t)
		provID := testID(t)
		memIDs = append(memIDs, memID)
		provIDs = append(provIDs, provID)
		if _, err := dm.SQLDB().Exec(
			`INSERT INTO memories (id, collection, content, created_at, weight, confidence)
			 VALUES (?, 'memories', 'test', CAST(strftime('%s','now') AS INTEGER), 1, 0.5)`,
			memID,
		); err != nil {
			t.Fatalf("seed memory: %v", err)
		}
		if _, err := dm.SQLDB().Exec(
			`INSERT INTO artifact_provenance (id, artifact_id, artifact_type, actor_kind, invocation_id, schema_version, created_at)
			 VALUES (?, ?, 'memory', 'agent', ?, 'v1', CAST(strftime('%s','now') AS INTEGER))`,
			provID, memID, invID,
		); err != nil {
			t.Fatalf("seed prov: %v", err)
		}
	}
	t.Cleanup(func() {
		for i := range memIDs {
			dm.SQLDB().Exec(`DELETE FROM artifact_provenance WHERE id = ?`, provIDs[i])
			dm.SQLDB().Exec(`DELETE FROM memories WHERE id = ?`, memIDs[i])
		}
	})

	exit := handleProvenanceInspect([]string{"--invocation", invID})
	if exit != 0 {
		t.Fatalf("handleProvenanceInspect exit = %d, want 0", exit)
	}
}

func TestHandleProvenance_ModelYield(t *testing.T) {
	dm := getDB()
	if dm == nil {
		t.Skip("no test DB (getDB returned nil)")
	}

	// Empty database — should return zero counts, not error.
	exit := handleProvenanceModelYield([]string{})
	if exit != 0 {
		t.Fatalf("handleProvenanceModelYield exit = %d, want 0", exit)
	}
}
