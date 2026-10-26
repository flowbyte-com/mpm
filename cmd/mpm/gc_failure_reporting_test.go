// gc_failure_reporting_test.go — pin that EVERY failing maintenance
// path in `mpm ops gc` is reported and makes the command exit non-zero.
//
// The previous corrective commit on this branch fixed the `--purge`
// sweep and the retention prunes. The same defect class — a failed
// operation reported as a clean run — survived on three more paths:
//
//  1. The weight-decay batch. This IS the maintenance pass: the batch
//     applies every computed weight delta in one transaction. When the
//     batch failed the handler rolled the transaction back, printed the
//     error, and let the command exit 0 — "the store is clean" for a
//     pass in which no weight changed at all.
//  2. The runtime accrual. It is the INPUT to the decay computation, so
//     a failure means the weights computed below were derived from
//     stale accumulators. It was a `usererror.Warn(... continuing)`.
//  3. The `--shred-negative` read. A failed query fell through to
//     "No negative-weight memories found." with exit 0 — a failed READ
//     reported as a successful empty scan.
//
// A fourth path was worse than a mis-reported exit code: the memory
// SELECT reported its error and then called `rows.Close()` on a nil
// *sql.Rows, which is a nil-pointer dereference — a panic in the middle
// of the maintenance pass.
//
// Each test injects a failure with a SQLite trigger, so the failure is
// confined to exactly one statement and the other paths still run.
package main

import (
	"strings"
	"testing"
)

// seedOrdinaryMemory inserts one ordinary (non-directive) memory that
// the decay pass WILL change, so the weight-decay batch issues an
// UPDATE for it.
//
// The decay is driven by `runtime_seconds_since_access`, and the batch
// skips a row whose computed weight equals its current weight. The
// seeded row therefore carries ten days of accumulated runtime and a
// `runtime_last_accrued_at` in the past, so the runtime accrual adds to
// the counter instead of resetting it to zero.
func seedOrdinaryMemory(t *testing.T, id string) {
	t.Helper()
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB returned nil")
	}
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, weight, runtime_seconds_since_access,
		                       runtime_last_accrued_at, created_at, updated_at)
		 VALUES (?, 'memories', ?, 3, 864000, CAST(strftime('%s','now') AS INTEGER) - 60,
		         CAST(strftime('%s','now') AS INTEGER) - 86400, CAST(strftime('%s','now') AS INTEGER))`,
		id, "an ordinary memory that decays",
	); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
}

// TestGC_WeightDecayBatchFailureExitsNonZero pins the primary sweep: a
// rolled-back weight-decay batch must not report a clean run.
func TestGC_WeightDecayBatchFailureExitsNonZero(t *testing.T) {
	workspace(t)
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB returned nil")
	}
	seedOrdinaryMemory(t, "gc-batch-failure-memory")

	// Abort only the weight UPDATE. The trigger fires for UPDATEs that
	// name the weight column, so the runtime accrual and the review
	// soft-delete (which set other columns) are untouched.
	if _, err := dm.SQLDB().Exec(`
		CREATE TRIGGER gc_injected_weight_failure BEFORE UPDATE OF weight ON memories
		BEGIN SELECT RAISE(ABORT, 'injected weight-update failure'); END;
	`); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}

	errs := captureUserErrors(t)
	code, out := runCLI(t, "ops", "gc")

	if code == 0 {
		t.Fatalf("gc with a failed weight-decay batch exited 0 (output %q); "+
			"a sweep in which no weight changed must not report success", out)
	}
	got := errs.String()
	if !strings.Contains(got, "weight decay") {
		t.Errorf("failed weight-decay batch was not reported to the operator; stderr buffer: %q", got)
	}
	if !strings.Contains(got, "maintenance operation(s) failed") {
		t.Errorf("gc did not summarise the degraded run; stderr buffer: %q", got)
	}
}

// TestGC_RuntimeAccrualFailureIsReported pins the decay input: a failed
// runtime accrual means the weights were computed from stale
// accumulators, so it must be visible and must fail the run.
func TestGC_RuntimeAccrualFailureIsReported(t *testing.T) {
	workspace(t)
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB returned nil")
	}
	seedOrdinaryMemory(t, "gc-accrual-failure-memory")

	// Abort only the runtime-accrual UPDATE.
	if _, err := dm.SQLDB().Exec(`
		CREATE TRIGGER gc_injected_accrual_failure BEFORE UPDATE OF runtime_seconds_since_access ON memories
		BEGIN SELECT RAISE(ABORT, 'injected runtime-accrual failure'); END;
	`); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}

	errs := captureUserErrors(t)
	code, out := runCLI(t, "ops", "gc")

	if code == 0 {
		t.Fatalf("gc with a failed runtime accrual exited 0 (output %q)", out)
	}
	got := errs.String()
	if !strings.Contains(got, "runtime accrual") {
		t.Errorf("failed runtime accrual was not reported to the operator; stderr buffer: %q", got)
	}
}

// TestGC_ShredNegativeReadFailureExitsNonZero pins the destructive
// sweep's read: a failed query must not be reported as "no negative
// weight memories found".
func TestGC_ShredNegativeReadFailureExitsNonZero(t *testing.T) {
	workspace(t)
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB returned nil")
	}
	if _, err := dm.SQLDB().Exec(`ALTER TABLE memories RENAME TO memories_unavailable_for_shred_negative_test`); err != nil {
		t.Fatalf("hide memories table: %v", err)
	}

	errs := captureUserErrors(t)
	code, out := runCLI(t, "ops", "gc", "--shred-negative")

	if code == 0 {
		t.Fatalf("gc --shred-negative with a failed read exited 0 (output %q)", out)
	}
	if strings.Contains(out, "No negative-weight memories found.") {
		t.Errorf("failed read was reported as an empty scan: %q", out)
	}
	if !strings.Contains(errs.String(), "negative-weight") {
		t.Errorf("failed read produced no operator-visible error; stderr buffer: %q", errs.String())
	}
}

// TestGC_MemoryReadFailureExitsNonZeroWithoutPanic pins the memory
// SELECT. The previous shape reported the error and then called
// `rows.Close()` on a nil *sql.Rows — a nil-pointer dereference, i.e. a
// panic in the middle of the maintenance pass.
func TestGC_MemoryReadFailureExitsNonZeroWithoutPanic(t *testing.T) {
	workspace(t)
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB returned nil")
	}
	if _, err := dm.SQLDB().Exec(`ALTER TABLE memories RENAME TO memories_unavailable_for_read_test`); err != nil {
		t.Fatalf("hide memories table: %v", err)
	}

	errs := captureUserErrors(t)
	code, out := runCLI(t, "ops", "gc")

	if code == 0 {
		t.Fatalf("gc with a failed memory read exited 0 (output %q)", out)
	}
	if !strings.Contains(errs.String(), "could not read the memory set") {
		t.Errorf("failed memory read produced no operator-visible error; stderr buffer: %q", errs.String())
	}
}
