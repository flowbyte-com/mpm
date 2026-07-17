package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateStaging_DedupSkipsExistingContent verifies the dedup
// mechanism: re-running migrate on the same source skips facts already
// in the memories table (matched via content_hash).
func TestMigrateStaging_DedupSkipsExistingContent(t *testing.T) {
	dm := NewTestDM(t)

	// Write a real source file so IngestFromMarkdownFile can read it.
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "dedup-test.md")
	mustWrite(t, srcPath, `## Test

First fact here for the dedup test.

## Other

Second fact here also for the dedup test.`)

	// First run: stage and commit 2 facts
	stats1, err := dm.IngestFromMarkdownFile(srcPath, "test_dedup_1", false)
	if err != nil {
		t.Fatalf("first ingest failed: %v", err)
	}
	if stats1.RowsStaged != 2 {
		t.Fatalf("expected 2 staged, got %d", stats1.RowsStaged)
	}

	n, err := dm.PromoteRawMemoryBatch("test_dedup_1", false)
	if err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 promoted, got %d", n)
	}

	// Second run on the same source: should skip both
	stats2, err := dm.IngestFromMarkdownFile(srcPath, "test_dedup_2", false)
	if err != nil {
		t.Fatalf("second ingest failed: %v", err)
	}
	if stats2.RowsStaged != 0 {
		t.Errorf("expected 0 staged (dedup), got %d", stats2.RowsStaged)
	}
	if stats2.RowsSkipped != 2 {
		t.Errorf("expected 2 skipped, got %d", stats2.RowsSkipped)
	}
}

// TestMigrateStaging_RejectsSensitiveContent verifies the security scanner
// integration: facts containing obvious secret patterns are rejected.
func TestMigrateStaging_RejectsSensitiveContent(t *testing.T) {
	dm := NewTestDM(t)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "scanner-test.md")
	mustWrite(t, srcPath, `## Test

API token: abc123supersecret should be rejected by the scanner.

## Safe

This content is fine and should promote normally.`)

	_, err := dm.IngestFromMarkdownFile(srcPath, "test_scanner", false)
	if err != nil {
		t.Fatalf("ingest failed: %v", err)
	}

	n, err := dm.PromoteRawMemoryBatch("test_scanner", false)
	if err != nil {
		t.Fatalf("commit batch error: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 promoted (sensitive one rejected), got %d", n)
	}

	var rejected int
	dm.db.QueryRow(
		`SELECT COUNT(*) FROM raw_memories WHERE import_batch='test_scanner' AND status='rejected'`,
	).Scan(&rejected)
	if rejected != 1 {
		t.Errorf("expected 1 rejected row, got %d", rejected)
	}
}

// TestPromoteRawMemoryBatch_EmptyBatchReturnsZero verifies the promote
// function handles empty batches gracefully.
func TestPromoteRawMemoryBatch_EmptyBatchReturnsZero(t *testing.T) {
	dm := NewTestDM(t)
	n, err := dm.PromoteRawMemoryBatch("nonexistent_batch", false)
	if err != nil {
		t.Fatalf("promote failed: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 promoted for nonexistent batch, got %d", n)
	}
}

// TestPromoteRawMemoryBatch_DryRun verifies dry-run mode reports what
// would be promoted without actually writing to memories.
func TestPromoteRawMemoryBatch_DryRun(t *testing.T) {
	dm := NewTestDM(t)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "dryrun-test.md")
	mustWrite(t, srcPath, `## Test

Fact one here.
§
Fact two here.`)

	_, err := dm.IngestFromMarkdownFile(srcPath, "test_dryrun", false)
	if err != nil {
		t.Fatalf("ingest failed: %v", err)
	}

	n, err := dm.PromoteRawMemoryBatch("test_dryrun", true)
	if err != nil {
		t.Fatalf("dry-run promote failed: %v", err)
	}
	if n != 2 {
		t.Errorf("expected dry-run report of 2, got %d", n)
	}

	var approved int
	dm.db.QueryRow(
		`SELECT COUNT(*) FROM raw_memories WHERE import_batch='test_dryrun' AND status='approved'`,
	).Scan(&approved)
	if approved != 0 {
		t.Errorf("dry-run should NOT mark rows approved; got %d approved", approved)
	}

	var pending int
	dm.db.QueryRow(
		`SELECT COUNT(*) FROM raw_memories WHERE import_batch='test_dryrun' AND status='pending'`,
	).Scan(&pending)
	if pending != 2 {
		t.Errorf("expected 2 pending rows after dry-run, got %d", pending)
	}
}

// TestMigrateFromMigratedTagAlwaysPresent verifies the 'migrated' tag
// is added to every promoted memory so we can find migrations later.
func TestMigrateFromMigratedTagAlwaysPresent(t *testing.T) {
	dm := NewTestDM(t)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "tag-test.md")
	mustWrite(t, srcPath, `## Test

A fact with the migrated tag.`)

	_, _ = dm.IngestFromMarkdownFile(srcPath, "test_tags", false)
	_, _ = dm.PromoteRawMemoryBatch("test_tags", false)

	var found bool
	rows, _ := dm.db.Query(`SELECT tags FROM memories WHERE 'from-migration:test_tags' IN (SELECT value FROM json_each(tags))`)
	defer rows.Close()
	for rows.Next() {
		var tagsJSON string
		rows.Scan(&tagsJSON)
		if strings.Contains(tagsJSON, "migrated") {
			found = true
		}
	}
	if !found {
		t.Error("expected 'migrated' tag on promoted memory")
	}
}

// TestJSONMigrate_EndToEnd covers the full JSON migration path with dedup.
func TestJSONMigrate_EndToEnd(t *testing.T) {
	dm := NewTestDM(t)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "smoke.json")
	mustWrite(t, srcPath, `[
		{"content": "JSON fact one for the end-to-end smoke.", "tags": ["test"], "weight": 8},
		{"content": "JSON fact two for the end-to-end smoke.", "tags": ["test"]}
	]`)

	stats, err := dm.IngestFromJsonFile(srcPath, "json_e2e", false)
	if err != nil {
		t.Fatalf("json ingest failed: %v", err)
	}
	if stats.RowsStaged != 2 {
		t.Fatalf("expected 2 staged, got %d", stats.RowsStaged)
	}

	n, err := dm.PromoteRawMemoryBatch("json_e2e", false)
	if err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 promoted, got %d", n)
	}

	// Re-run for dedup
	stats2, err := dm.IngestFromJsonFile(srcPath, "json_e2e_2", false)
	if err != nil {
		t.Fatalf("second json ingest failed: %v", err)
	}
	if stats2.RowsSkipped != 2 {
		t.Errorf("expected 2 skipped on rerun, got %d", stats2.RowsSkipped)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}