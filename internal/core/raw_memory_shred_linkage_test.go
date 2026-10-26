// raw_memory_shred_linkage_test.go — pin the residual-copy contract for
// imported staging rows (Hermes review, priority 4).
//
// # The defect this file pins
//
// Imported content lands in `raw_memories` and is later promoted into
// `memories` by PromoteRawMemoryBatch. Nothing recorded which promoted
// memory a staging row became, so ShredMemoryWithCascade could only
// delete the memory row and its defined cascades — the staging row kept
// the full text forever. An operator who shredded a memory could still
// find its content in `raw_memories`, with no signal that this copy
// existed.
//
// The fix is a durable source linkage (`raw_memories.memory_id`), not
// text matching: a shred removes exactly the staging row that produced
// this memory. Rows promoted before the linkage existed cannot be
// attributed, and are deliberately left alone — deleting them because
// their text happens to match would destroy unrelated imports.
package internal

import (
	"testing"
	"time"
)

// seedRawMemory inserts one pending staging row and returns its id.
func seedRawMemory(t *testing.T, dm *DatabaseManager, id, text string) {
	t.Helper()
	now := float64(time.Now().Unix())
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata,
		                           ingested_at, status, import_batch, updated_at)
		 VALUES (?, ?, 'openclaw', ?, ?, '{"weight":5}', ?, 'pending', 'batch-linkage', ?)`,
		id, "src-"+id, "hash-"+id, text, now, now,
	); err != nil {
		t.Fatalf("seed raw memory %s: %v", id, err)
	}
}

func rawRowCount(t *testing.T, dm *DatabaseManager, id string) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM raw_memories WHERE id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("count raw row %s: %v", id, err)
	}
	return n
}

// TestPromoteRawMemory_RecordsSourceLinkage pins the durable link the
// shred path needs: the staging row must name the memory it became.
func TestPromoteRawMemory_RecordsSourceLinkage(t *testing.T) {
	dm := NewTestDM(t)
	seedRawMemory(t, dm, "raw-link-1", "the imported claim that becomes a memory")

	promoted, err := dm.PromoteRawMemoryBatch("batch-linkage", false)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted != 1 {
		t.Fatalf("promoted = %d, want 1", promoted)
	}

	var memoryID string
	if err := dm.SQLDB().QueryRow(
		`SELECT COALESCE(memory_id, '') FROM raw_memories WHERE id = 'raw-link-1'`,
	).Scan(&memoryID); err != nil {
		t.Fatalf("read linkage: %v", err)
	}
	if memoryID == "" {
		t.Fatal("raw_memories.memory_id is empty after promotion; the shred path cannot attribute the staging row")
	}
	if _, err := dm.GetMemory(memoryID); err != nil {
		t.Fatalf("linked memory %s is not readable: %v", memoryID, err)
	}
}

// TestShredMemory_RemovesLinkedStagingRow pins the residual-copy fix:
// after a shred, the staged text is no longer discoverable.
func TestShredMemory_RemovesLinkedStagingRow(t *testing.T) {
	dm := NewTestDM(t)
	seedRawMemory(t, dm, "raw-shred-1", "shredded-import-secret-marker")

	if _, err := dm.PromoteRawMemoryBatch("batch-linkage", false); err != nil {
		t.Fatalf("promote: %v", err)
	}
	var memoryID string
	if err := dm.SQLDB().QueryRow(
		`SELECT memory_id FROM raw_memories WHERE id = 'raw-shred-1'`,
	).Scan(&memoryID); err != nil {
		t.Fatalf("read linkage: %v", err)
	}
	if memoryID == "" {
		t.Fatal("no linkage recorded")
	}

	if _, err := dm.ShredMemoryWithCascade(memoryID); err != nil {
		t.Fatalf("shred: %v", err)
	}

	if n := rawRowCount(t, dm, "raw-shred-1"); n != 0 {
		t.Errorf("raw staging row survived the shred (%d rows); its text remains discoverable", n)
	}
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM raw_memories WHERE text LIKE '%shredded-import-secret-marker%'`,
	).Scan(&n); err != nil {
		t.Fatalf("scan raw text: %v", err)
	}
	if n != 0 {
		t.Errorf("%d raw staging row(s) still carry the shredded text", n)
	}
}

// TestShredMemory_LeavesUnlinkedStagingRowsAlone guards against the
// tempting over-fix: a staging row that predates the linkage cannot be
// attributed to this memory, and its text matching must not be treated
// as proof. Deleting by text would destroy unrelated imports.
func TestShredMemory_LeavesUnlinkedStagingRowsAlone(t *testing.T) {
	dm := NewTestDM(t)
	seedRawMemory(t, dm, "raw-legacy-1", "text that happens to match a memory")

	// Simulate a pre-linkage promotion: the memory exists, the staging
	// row has no memory_id (the column is NULL).
	out, _, err := dm.SaveMemoryWithContext(
		"text that happens to match a memory", "memories", nil, 0, "", ActiveContext{},
	)
	if err != nil {
		t.Fatalf("save memory: %v", err)
	}
	memoryID, _ := out["id"].(string)
	if memoryID == "" {
		t.Fatalf("save returned no id: %v", out)
	}

	if _, err := dm.ShredMemoryWithCascade(memoryID); err != nil {
		t.Fatalf("shred: %v", err)
	}

	if n := rawRowCount(t, dm, "raw-legacy-1"); n != 1 {
		t.Errorf("unlinked staging row was removed (%d rows); text matching must never drive deletion", n)
	}
}
