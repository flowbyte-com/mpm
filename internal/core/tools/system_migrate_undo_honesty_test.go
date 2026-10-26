// system_migrate_undo_honesty_test.go — pin that the agent-facing
// import-batch undo says what it actually did.
//
// The CLI undo (`mpm ingest --undo <batch>`) was made honest by the
// previous commit on this branch: the RowsAffected error is propagated,
// a zero-match undo is not reported as a removal, and the output states
// that memories already promoted from the batch are NOT removed.
//
// The MCP / tool undo of the same verb — `mpm_system action=migrate`
// with `undo_batch` — was left as it was:
//
//	_, _ = dm.SQLDB().Exec(
//	    `UPDATE raw_memories SET status='rejected', ... WHERE import_batch=? ...`,
//	    ...)
//
// The write error was discarded, the call reported success, and the
// result carried no count and no disclosure. This is the surface an
// agent actually uses, so it is the one that must not lie.
//
// The two surfaces also differ in what they DO: the CLI undo deletes
// the staging rows, the tool undo tombstones them (`status='rejected'`)
// so the audit trail survives. That difference is deliberate and stays —
// but the tool path now says which one it is.
package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestMigrate_UndoBatchWriteFailureIsNotSuccess pins the headline
// invariant: a failed undo write must be an error, not a success.
func TestMigrate_UndoBatchWriteFailureIsNotSuccess(t *testing.T) {
	dm := mpminternal.NewTestLocalOnlyDM(t)
	seedRawMemoriesBatch(t, dm, "batch-undo-failure", 2)

	// Hide the staging table so the undo UPDATE cannot run.
	if _, err := dm.SQLDB().Exec(`ALTER TABLE raw_memories RENAME TO raw_memories_unavailable_for_undo_test`); err != nil {
		t.Fatalf("hide raw_memories table: %v", err)
	}

	out, err := handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"confirm":    true,
		"undo_batch": "batch-undo-failure",
	})
	if err == nil {
		t.Fatalf("failed undo reported success (result %v); a write that did not "+
			"happen must never be reported as an undo", out)
	}
	if !strings.Contains(err.Error(), "undo") {
		t.Errorf("error must name the failed operation, got: %v", err)
	}
}

// TestMigrate_UndoBatchReportsWhatItReverted pins the reporting
// contract: the result must say how many staging rows were reverted and
// must disclose that promoted memories are not removed.
func TestMigrate_UndoBatchReportsWhatItReverted(t *testing.T) {
	dm := mpminternal.NewTestLocalOnlyDM(t)
	seedRawMemoriesBatch(t, dm, "batch-undo-report", 3)

	out, err := handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"confirm":    true,
		"undo_batch": "batch-undo-report",
	})
	if err != nil {
		t.Fatalf("undo failed: %v", err)
	}
	res, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("result is %T, want map[string]interface{}", out)
	}
	n, ok := res["rows_reverted"].(int)
	if !ok {
		t.Fatalf("result does not report rows_reverted (result %v)", res)
	}
	if n != 3 {
		t.Errorf("rows_reverted = %d, want 3 (the three seeded staging rows)", n)
	}
	note, _ := res["note"].(string)
	if !strings.Contains(strings.ToLower(note), "promoted") {
		t.Errorf("result must disclose that promoted memories are NOT removed, got note %q", note)
	}
	if !strings.Contains(strings.ToLower(note), "not removed") {
		t.Errorf("result note must say the promoted memories are not removed, got %q", note)
	}
}

// TestMigrate_UndoBatchZeroMatchIsNotARemoval pins the other half of
// the CLI contract: an undo that matched nothing must not be reported as
// a removal of zero rows.
func TestMigrate_UndoBatchZeroMatchIsNotARemoval(t *testing.T) {
	dm := mpminternal.NewTestLocalOnlyDM(t)

	out, err := handleMigrate(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"confirm":    true,
		"undo_batch": "batch-that-does-not-exist",
	})
	if err != nil {
		t.Fatalf("undo of a missing batch failed: %v", err)
	}
	res, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("result is %T, want map[string]interface{}", out)
	}
	n, ok := res["rows_reverted"].(int)
	if !ok {
		t.Fatalf("result does not report rows_reverted (result %v)", res)
	}
	if n != 0 {
		t.Errorf("rows_reverted = %d, want 0", n)
	}
}
