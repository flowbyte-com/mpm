// system_migrate_confirm_regression_test.go — Pass 6 defect C.20.
//
// The 2026-09-05 audit found that mpm_system.migrate could be invoked
// without any explicit authorization. The previous shape required
// from_path / commit_batch / undo_batch but did not require a
// confirmation gate — an agent could call the handler with arbitrary
// parameters and have it execute a bulk database write.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_system --payload '{"action":"migrate","params":{"commit_batch":"some-existing-batch","dry_run":true}}'
//     # wanted: error requiring confirm=true
//     # actual: success; PromoteRawMemoryBatch ran with dry_run=true
//
// Canonical contract (per audit + the existing record_global_rule
// convention at handlers.go:3371):
//
//   confirm omitted    → ERROR: migrate requires confirm=true
//   confirm = false    → ERROR: migrate requires confirm=true
//   confirm = true     → migration executes
//   confirm = "true"   → ERROR (string is not a boolean)
//   confirm = 1        → ERROR (number is not a boolean)
//   confirm = null     → ERROR (null is not a boolean)
//
// The fix mirrors the record_global_rule convention. Only the boolean
// literal `true` authorises execution; every other shape is rejected
// before any DB call. Because both the CLI dispatcher
// (cmd/mpm/call.go handleCall) and the MCP adapter
// (cmd/mpm-mcp/tools.go mcpAdapter) feed the same payload shape into
// handleMpmSystem, a single handler-level validation closes the
// safety invariant for every public path.

package tools

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// nowUnix returns the current unix epoch in seconds as an int64.
// Mirrors the row.created_at/updated_at format used by the rest
// of the codebase.
func nowUnix() int64 { return time.Now().Unix() }

// intStr converts a non-negative int into its decimal string form.
// Used to build stable test ids (the schema requires [a-z0-9-]).
func intStr(n int) string { return strconv.Itoa(n) }

// TestMigrate_OmittedConfirmRejected pins the headline invariant:
// the migrate handler refuses any invocation without explicit
// `confirm=true`.
func TestMigrate_OmittedConfirmRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	srcPath := writeMigrateSourceFile(t, "## Test\n\nfact one for the confirm test.\n")

	_, err := handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"from_path": srcPath,
	})
	if err == nil {
		t.Fatal("omitted confirm must error; migration must not run")
	}
	if !strings.Contains(err.Error(), "confirm=true") {
		t.Errorf("error must mention 'confirm=true', got: %v", err)
	}
}

// TestMigrate_ConfirmFalseRejected pins: explicit `confirm=false`
// also rejects. The handler must not accept `false` as a substitute
// for the missing gate.
func TestMigrate_ConfirmFalseRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	srcPath := writeMigrateSourceFile(t, "## Test\n\nfact two for the confirm test.\n")

	_, err := handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"from_path": srcPath,
		"confirm":   false,
	})
	if err == nil {
		t.Fatal("confirm=false must error; migration must not run")
	}
	if !strings.Contains(err.Error(), "confirm=true") {
		t.Errorf("error must mention 'confirm=true', got: %v", err)
	}
}

// TestMigrate_NonBooleanConfirmRejected pins: string "true",
// numeric 1, and null must all be rejected. Only the boolean
// literal `true` authorises execution — strings and numbers are not
// silently coerced.
func TestMigrate_NonBooleanConfirmRejected(t *testing.T) {
	dm := newTestSharedDM(t)
	srcPath := writeMigrateSourceFile(t, "## Test\n\nfact three for the confirm test.\n")

	cases := []struct {
		name      string
		confirm   interface{}
		wantSubstr string
	}{
		{"string_true", "true", "confirm=true"},
		{"string_FALSE", "FALSE", "confirm=true"},
		{"numeric_one", 1, "confirm=true"},
		{"numeric_int", int64(1), "confirm=true"},
		{"null", nil, "confirm=true"},
		{"array", []interface{}{true}, "confirm=true"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"from_path": srcPath,
				"confirm":   c.confirm,
			})
			if err == nil {
				t.Fatalf("confirm=%v (%T) must error", c.confirm, c.confirm)
			}
			if !strings.Contains(err.Error(), c.wantSubstr) {
				t.Errorf("error must mention %q, got: %v", c.wantSubstr, err)
			}
		})
	}
}

// TestMigrate_OmittedConfirm_DoesNotMutateRawMemories pins the
// safety invariant: an unauthorised invocation does NOT touch
// raw_memories. Uses the commit_batch path (the most direct
// promoter) as a sentinel — a non-empty staged batch is the only
// observable side effect of a successful migrate, so a snapshot
// before/after a rejected call must be identical.
func TestMigrate_OmittedConfirm_DoesNotMutateRawMemories(t *testing.T) {
	dm := newTestSharedDM(t)

	// Seed a controlled batch: directly INSERT into raw_memories so
	// the migrate action has a real batch to "promote" if it were
	// to run. We then prove the rejected call does not promote any
	// of these rows.
	seedRawMemoriesBatch(t, dm, "test-batch-no-mutate", 3)

	// Baseline: the batch is present and all rows are pending.
	assertRawBatchState(t, dm, "test-batch-no-mutate", 3, "pending")

	// Omitted confirm.
	_, err := handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"commit_batch": "test-batch-no-mutate",
	})
	if err == nil {
		t.Fatal("omitted confirm must error")
	}

	// Confirm=false.
	_, err = handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"commit_batch": "test-batch-no-mutate",
		"confirm":      false,
	})
	if err == nil {
		t.Fatal("confirm=false must error")
	}

	// String "true" — should be rejected.
	_, err = handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"commit_batch": "test-batch-no-mutate",
		"confirm":      "true",
	})
	if err == nil {
		t.Fatal("confirm='true' string must error")
	}

	// Numeric 1 — should be rejected.
	_, err = handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"commit_batch": "test-batch-no-mutate",
		"confirm":      1,
	})
	if err == nil {
		t.Fatal("confirm=1 numeric must error")
	}

	// After all rejected calls: rows must still be pending (no
	// promotion happened).
	assertRawBatchState(t, dm, "test-batch-no-mutate", 3, "pending")
}

// TestMigrate_ConfirmTrue_PromotesBatch pins the success path:
// with explicit `confirm=true`, the handler runs the migration
// (the same PromoteRawMemoryBatch path it ran before the fix,
// just guarded now).
func TestMigrate_ConfirmTrue_PromotesBatch(t *testing.T) {
	dm := newTestSharedDM(t)

	seedRawMemoriesBatch(t, dm, "test-batch-confirm-true", 2)

	// Pre: rows pending.
	assertRawBatchState(t, dm, "test-batch-confirm-true", 2, "pending")

	res, err := handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"commit_batch": "test-batch-confirm-true",
		"confirm":      true,
	})
	if err != nil {
		t.Fatalf("confirm=true with valid batch must succeed: %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["action"] != "promote" {
		t.Errorf("expected action=promote, got %v", m["action"])
	}
	if promoted, _ := m["rows_promoted"].(int); promoted != 2 {
		t.Errorf("expected 2 rows promoted, got %d", promoted)
	}

	// Post: PromoteRawMemoryBatch keeps the raw_memories row but
	// transitions it from 'pending' to 'approved'. Sentinel: a
	// rejected call would have left them as 'pending'.
	row := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM raw_memories WHERE import_batch = ? AND status = 'approved'`, "test-batch-confirm-true")
	var approved int
	if err := row.Scan(&approved); err != nil {
		t.Fatalf("count approved rows: %v", err)
	}
	if approved != 2 {
		t.Errorf("expected 2 rows transitioned to 'approved', got %d", approved)
	}
}

// TestMigrate_ConfirmTrue_DryRunHonoured pins the existing
// dry_run contract: explicit confirm=true still respects dry_run.
// Promotion only runs when !dry_run.
func TestMigrate_ConfirmTrue_DryRunHonoured(t *testing.T) {
	dm := newTestSharedDM(t)

	seedRawMemoriesBatch(t, dm, "test-batch-dryrun", 2)

	res, err := handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"commit_batch": "test-batch-dryrun",
		"dry_run":      true,
		"confirm":      true,
	})
	if err != nil {
		t.Fatalf("confirm=true + dry_run must succeed: %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["action"] != "promote" {
		t.Errorf("expected action=promote, got %v", m["action"])
	}

	// Post: rows still pending (dry_run suppressed promotion).
	assertRawBatchState(t, dm, "test-batch-dryrun", 2, "pending")
}

// TestMigrate_ConfirmTrue_UndoBatchWorks pins: explicit confirm=true
// also authorises the undo_batch path (a non-trivial UPDATE that
// tombstones pending/approved rows).
func TestMigrate_ConfirmTrue_UndoBatchWorks(t *testing.T) {
	dm := newTestSharedDM(t)

	seedRawMemoriesBatch(t, dm, "test-batch-undo", 2)
	assertRawBatchState(t, dm, "test-batch-undo", 2, "pending")

	res, err := handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"undo_batch": "test-batch-undo",
		"confirm":    true,
	})
	if err != nil {
		t.Fatalf("confirm=true + undo_batch must succeed: %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["action"] != "undo" {
		t.Errorf("expected action=undo, got %v", m["action"])
	}

	// Post: rows tombstoned (status='rejected', llm_notes set).
	row := dm.SQLDB().QueryRow(`SELECT status FROM raw_memories WHERE import_batch = ? LIMIT 1`, "test-batch-undo")
	var status string
	if err := row.Scan(&status); err != nil {
		t.Fatalf("scan status: %v", err)
	}
	if status != "rejected" {
		t.Errorf("expected status=rejected after undo, got %q", status)
	}
}

// ── Helpers ────────────────────────────────────────────────────────

// writeMigrateSourceFile writes a small markdown migration source
// file to a per-test tmp dir and returns the path. handleMigrate
// needs a real file path to construct a batch id.
func writeMigrateSourceFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "migrate-source.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write migrate source: %v", err)
	}
	return path
}

// seedRawMemoriesBatch inserts N pending rows for the given import
// batch. The migrate commit_batch path promotes these. Tests use
// this as the observable sentinel: if confirmation is bypassed,
// the rows transition from "pending" to "promoted" (deleted from
// raw_memories).
func seedRawMemoriesBatch(t *testing.T, dm *mpminternal.DatabaseManager, batch string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := batch + "-row-" + intStr(i)
		_, err := dm.SQLDB().Exec(`
			INSERT INTO raw_memories (
				id, source_id, source_db, content_hash, text,
				metadata, ingested_at, status, llm_notes,
				import_batch, updated_at
			) VALUES (?, ?, 'test', ?, ?, '{}', ?, 'pending', '', ?, ?)
		`, id, id, "hash-"+id, "text for "+id, nowUnix(), batch, nowUnix())
		if err != nil {
			t.Fatalf("seed raw_memories row %d: %v", i, err)
		}
	}
}

// assertRawBatchState asserts the raw_memories table contains
// `expectedCount` rows for the given batch, all with `expectedStatus`.
func assertRawBatchState(t *testing.T, dm *mpminternal.DatabaseManager, batch string, expectedCount int, expectedStatus string) {
	t.Helper()
	row := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM raw_memories WHERE import_batch = ?`, batch)
	var count int
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count raw_memories for batch %q: %v", batch, err)
	}
	if count != expectedCount {
		t.Fatalf("batch %q: expected %d rows, got %d", batch, expectedCount, count)
	}
	if expectedStatus == "" {
		return
	}
	row = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM raw_memories WHERE import_batch = ? AND status != ?`, batch, expectedStatus)
	var mismatched int
	if err := row.Scan(&mismatched); err != nil {
		t.Fatalf("count non-%s rows for batch %q: %v", expectedStatus, batch, err)
	}
	if mismatched != 0 {
		t.Errorf("batch %q: %d rows have status != %q", batch, mismatched, expectedStatus)
	}
}
