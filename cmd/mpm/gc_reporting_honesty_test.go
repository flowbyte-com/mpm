// gc_reporting_honesty_test.go — pin that `mpm ops gc` reports what it
// actually did (Hermes review, priority 5).
//
// Two defects are covered:
//
//  1. `--purge` logged the DELETE error and then called RowsAffected on
//     the nil Result (panic), and on a non-panicking path printed
//     "Purged 0" and exited 0 — a failed destructive sweep reported as a
//     successful no-op.
//  2. Maintenance prunes (audit log, handoffs, cascade outbox) and the
//     `--review` soft-deletes reported failures only through slog, so
//     `mpm ops gc` exited 0 while a requested sweep had done nothing.
//     The review path also printed the number of rows ATTEMPTED as if
//     they had all been soft-deleted.
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/usererror"
)

// captureUserErrors redirects the CLI's user-facing error stream and
// returns the buffer + a restore hook.
func captureUserErrors(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	usererror.SetWriter(&buf)
	t.Cleanup(func() { usererror.SetWriter(nil) })
	return &buf
}

// TestGC_PurgeFailureIsNotSuccess pins the destructive-sweep contract:
// a failed `--purge` must not panic, must not print a fake count, and
// must exit non-zero.
func TestGC_PurgeFailureIsNotSuccess(t *testing.T) {
	workspace(t)
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB returned nil")
	}
	// Make the purge DELETE fail for a reason the handler cannot paper
	// over. The cooldown claim touches only system_config, so the failure
	// lands exactly where the defect was.
	if _, err := dm.SQLDB().Exec(`ALTER TABLE memories RENAME TO memories_unavailable_for_purge_test`); err != nil {
		t.Fatalf("hide memories table: %v", err)
	}

	errs := captureUserErrors(t)
	code, out := runCLI(t, "ops", "gc", "--purge")

	if code == 0 {
		t.Fatalf("failed purge exited 0 (output %q); a destructive sweep that did nothing must not report success", out)
	}
	if strings.Contains(out, "Purged 0") {
		t.Errorf("failed purge printed a fabricated row count: %q", out)
	}
	if !strings.Contains(errs.String(), "purge") {
		t.Errorf("failed purge produced no operator-visible error; stderr buffer: %q", errs.String())
	}
}

// TestGC_PruneFailureExitsNonZero pins the maintenance-reporting
// contract: a requested retention sweep that fails is visible and the
// command exits non-zero, instead of a clean-looking exit 0.
func TestGC_PruneFailureExitsNonZero(t *testing.T) {
	workspace(t)
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB returned nil")
	}
	// Drop one retention target. The prune then fails with "no such
	// table", which used to be swallowed into a slog.Warn line.
	if _, err := dm.SQLDB().Exec(`DROP TABLE session_handoffs`); err != nil {
		t.Fatalf("drop session_handoffs: %v", err)
	}

	errs := captureUserErrors(t)
	code, out := runCLI(t, "ops", "gc")

	if code == 0 {
		t.Fatalf("gc with a failed prune exited 0 (output %q)", out)
	}
	got := errs.String()
	if !strings.Contains(got, "session handoffs prune failed") {
		t.Errorf("failed handoff prune was not reported to the operator; stderr buffer: %q", got)
	}
	if !strings.Contains(got, "maintenance operation(s) failed") {
		t.Errorf("gc did not summarise the degraded run; stderr buffer: %q", got)
	}
}

// TestGC_CleanRunStillSucceeds guards against over-correction: a normal
// maintenance pass must still exit 0 and report zero maintenance
// failures in its JSON envelope.
func TestGC_CleanRunStillSucceeds(t *testing.T) {
	workspace(t)
	captureUserErrors(t)

	code, out := runCLI(t, "ops", "gc", "--json")
	if code != 0 {
		t.Fatalf("clean gc run exited %d: %q", code, out)
	}
	if !strings.Contains(out, `"maintenance_failures":0`) {
		t.Errorf("clean gc JSON does not report zero maintenance failures: %q", out)
	}
}
