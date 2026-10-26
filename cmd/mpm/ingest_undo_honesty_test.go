// ingest_undo_honesty_test.go — pin what `mpm ingest --undo` actually
// does (Hermes review, priority 5: a successful response when the
// underlying operation did less than the verb implies).
//
// Undo clears STAGING rows for a batch. It cannot remove memories already
// promoted from that batch (promotion is not tracked per batch), and the
// old output neither said so nor distinguished "removed N rows" from
// "matched nothing".
package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// captureUndoErrors redirects usererror output for this test. Defined
// here rather than shared so the helper does not collide with the GC
// reporting tests' capture helper.
func captureUndoErrors(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	usererror.SetWriter(&buf)
	t.Cleanup(func() { usererror.SetWriter(nil) })
	return &buf
}

// activeContextForUndoTest is the neutral caller identity for a save the
// test performs itself; no provenance is under test here.
func activeContextForUndoTest() mpminternal.ActiveContext {
	return mpminternal.ActiveContext{}
}

// seedStagingBatch inserts one staging row for the given batch.
func seedStagingBatch(t *testing.T, rawID, text, batch string) {
	t.Helper()
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB returned nil")
	}
	now := float64(time.Now().Unix())
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata,
		                           ingested_at, status, import_batch, updated_at)
		 VALUES (?, ?, 'openclaw', ?, ?, '{"weight":5}', ?, 'pending', ?, ?)`,
		rawID, "src-"+rawID, "hash-"+rawID, text, now, batch, now,
	); err != nil {
		t.Fatalf("seed staging row %s: %v", rawID, err)
	}
}

// TestIngestUndo_DisclosesThatPromotedMemoriesRemain pins the honesty
// fix: undo removes staging rows and says, in the success output, that
// promoted memories survive.
func TestIngestUndo_DisclosesThatPromotedMemoriesRemain(t *testing.T) {
	workspace(t)
	captureUndoErrors(t)

	seedStagingBatch(t, "raw-undo-1", "undo-disclosure-marker", "batch-undo")
	dm := getDB()
	// A memory already promoted from this batch. The undo test does not
	// depend on the staging->memory linkage: it asserts that a promoted
	// memory survives the staging sweep and that the output says so.
	saved, _, err := dm.SaveMemoryWithContext(
		"undo-disclosure-marker", "memories", nil, 0, "", activeContextForUndoTest(),
	)
	if err != nil {
		t.Fatalf("save promoted memory: %v", err)
	}
	memoryID, _ := saved["id"].(string)
	if memoryID == "" {
		t.Fatalf("save returned no id: %v", saved)
	}

	code, out := runCLI(t, "ingest", "--undo", "batch-undo")
	if code != 0 {
		t.Fatalf("undo failed: exit %d, output %q", code, out)
	}
	if !strings.Contains(out, "Removed 1 staging entries") {
		t.Errorf("undo did not report the staging removal: %q", out)
	}
	// The limit must be stated, not left in a source comment.
	if !strings.Contains(out, "already promoted from this batch are NOT removed") {
		t.Errorf("undo does not disclose that promoted memories survive: %q", out)
	}

	// The promoted memory is still there — the disclosure is true.
	if _, err := dm.GetMemory(memoryID); err != nil {
		t.Errorf("promoted memory disappeared during staging undo: %v", err)
	}
}

// TestIngestUndo_NoMatchIsNotReportedAsRemoval pins the second half: an
// undo that matches nothing must not print "Removed 0 entries" as a
// success-shaped result.
func TestIngestUndo_NoMatchIsNotReportedAsRemoval(t *testing.T) {
	workspace(t)
	errs := captureUndoErrors(t)

	code, out := runCLI(t, "ingest", "--undo", "batch-that-does-not-exist")
	if code != 0 {
		t.Fatalf("no-op undo exited %d: %q", code, out)
	}
	if strings.Contains(out, "Removed 0 entries") {
		t.Errorf("no-op undo printed a removal line: %q", out)
	}
	if !strings.Contains(errs.String(), "No staging rows matched batch") {
		t.Errorf("no-op undo did not say nothing matched; stderr buffer: %q", errs.String())
	}
}
