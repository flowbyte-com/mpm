// shred_staging_disclosure_test.go — pin the operator-visible disclosure
// for imported staging rows (Hermes review, priority 4).
//
// A shred removes the memory and its defined cascades. Imported content
// also lives in `raw_memories` after promotion; the substrate must say
// what happened to that copy, and must not pretend to know which
// pre-linkage rows belong to this memory.
package main

import (
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// seedRawAndPromote inserts a staging row, promotes it, and returns the
// memory id it became.
func seedRawAndPromote(t *testing.T, rawID, text string) string {
	t.Helper()
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB returned nil in the test workspace")
	}
	now := float64(time.Now().Unix())
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata,
		                           ingested_at, status, import_batch, updated_at)
		 VALUES (?, ?, 'openclaw', ?, ?, '{"weight":5}', ?, 'pending', 'cli-disclosure', ?)`,
		rawID, "src-"+rawID, "hash-"+rawID, text, now, now,
	); err != nil {
		t.Fatalf("seed raw memory: %v", err)
	}
	if _, err := dm.PromoteRawMemoryBatch("cli-disclosure", false); err != nil {
		t.Fatalf("promote: %v", err)
	}
	var memoryID string
	if err := dm.SQLDB().QueryRow(
		`SELECT memory_id FROM raw_memories WHERE id = ?`, rawID,
	).Scan(&memoryID); err != nil {
		t.Fatalf("read linkage: %v", err)
	}
	if memoryID == "" {
		t.Fatal("promotion did not record a memory linkage")
	}
	return memoryID
}

// TestMemoryShredCLI_DisclosesStagingRemovalAndUnlinkedRows pins both
// halves of the disclosure: linked rows are removed and counted, and
// unlinked rows are named as a residual copy that was deliberately left.
func TestMemoryShredCLI_DisclosesStagingRemovalAndUnlinkedRows(t *testing.T) {
	workspace(t)

	memoryID := seedRawAndPromote(t, "raw-cli-1", "linked staging copy")

	code, out := runCLI(t, "memory", "shred", memoryID)
	if code != 0 {
		t.Fatalf("shred failed: exit %d, output %q", code, out)
	}
	if !strings.Contains(out, "removed imported staging row(s) linked to this memory: 1") {
		t.Errorf("output does not disclose the linked staging removal:\n%s", out)
	}
	if !strings.Contains(out, "raw_memories.memory_id IS NULL") {
		t.Errorf("output does not disclose the unlinkable residual staging rows:\n%s", out)
	}

	dm := getDB()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM raw_memories WHERE id = 'raw-cli-1'`,
	).Scan(&n); err != nil {
		t.Fatalf("count raw row: %v", err)
	}
	if n != 0 {
		t.Errorf("linked staging row survived the shred")
	}
}

// TestMemoryShredCLI_LeavesUnlinkedStagingRowAndSaysSo is the
// counterpart: an unlinked row is NOT removed on a text-match guess,
// and the disclosure tells the operator it survives.
func TestMemoryShredCLI_LeavesUnlinkedStagingRowAndSaysSo(t *testing.T) {
	workspace(t)
	dm := getDB()

	now := float64(time.Now().Unix())
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata,
		                           ingested_at, status, updated_at)
		 VALUES ('raw-cli-legacy', 'src-legacy', 'openclaw', 'hash-legacy',
		         'legacy staging copy', '{"weight":5}', ?, 'approved', ?)`,
		now, now,
	); err != nil {
		t.Fatalf("seed unlinked raw memory: %v", err)
	}

	out, _, err := dm.SaveMemoryWithContext(
		"legacy staging copy", "memories", nil, 0, "", mpminternal.ActiveContext{},
	)
	if err != nil {
		t.Fatalf("save memory: %v", err)
	}
	memoryID, _ := out["id"].(string)

	code, cliOut := runCLI(t, "memory", "shred", memoryID)
	if code != 0 {
		t.Fatalf("shred failed: exit %d, output %q", code, cliOut)
	}
	if !strings.Contains(cliOut, "raw_memories.memory_id IS NULL") {
		t.Errorf("output does not disclose unlinkable staging rows:\n%s", cliOut)
	}
	if strings.Contains(cliOut, "removed imported staging row(s)") {
		t.Errorf("output claims a staging removal that did not happen:\n%s", cliOut)
	}

	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM raw_memories WHERE id = 'raw-cli-legacy'`,
	).Scan(&n); err != nil {
		t.Fatalf("count raw row: %v", err)
	}
	if n != 1 {
		t.Errorf("unlinked staging row was removed on a text-match guess")
	}
}
