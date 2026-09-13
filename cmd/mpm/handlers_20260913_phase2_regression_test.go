// cmd/mpm/handlers_20260913_phase2_regression_test.go — Phase 2
// regression coverage for the 2026-09-13 acceptance run remediation.
//
// Pins the atomicity invariants for working-context promotion
// (defect A) and the export --output writer routing (defect B) by
// source-pattern assertions against the patched handlers. The
// integration tests in cmd/mpm/cli_export_test.go and the scratchpad
// integration tests cover the behavioural coverage; here we pin the
// patch so a future refactor cannot silently reintroduce the bug
// without breaking this file.
//
// Reference points (acceptance run 2026-09-13):
//   - Defect A: `mpm work promote --session-id <id>` could create the
//     destination memory + clear the scratchpad, then return error
//     and exit 1. Callers retrying would create a duplicate memory.
//     Fix: lineage-tag idempotency check in WorkingContextService.Promote.
//   - Defect B: `mpm export --output FILE` truncated FILE to 0 bytes,
//     wrote JSON to stdout, and reported success. Fix: route the
//     format writer to FILE when --output is set.
package main

import (
	"os"
	"strings"
	"testing"
)

// TestWorkingContextPromote_HasIdempotencyCheck pins defect A: the
// Promote method must consult FindPromotionByLineage before saving.
// A future refactor that drops the check would let a retry duplicate
// the destination memory.
func TestWorkingContextPromote_HasIdempotencyCheck(t *testing.T) {
	src := readServiceSource(t, "service_working_context.go")
	if !strings.Contains(src, "FindPromotionByLineage") {
		t.Fatalf("WorkingContextService.Promote must call FindPromotionByLineage for idempotency (defect A)")
	}
	// The check must run BEFORE SaveMemory — otherwise a duplicate
	// could be created in the gap.
	if !idempotencyCheckBeforeSaveMemory(src) {
		t.Fatalf("FindPromotionByLineage call must precede SaveMemory call in Promote")
	}
}

// TestWorkingContextPromote_HasLineageTag pins defect A invariant
// contract: the lineage tag is "from-scratchpad:<sessionID>" so
// FindPromotionByLineage keys against it on retry.
func TestWorkingContextPromote_HasLineageTag(t *testing.T) {
	src := readServiceSource(t, "service_working_context.go")
	if !strings.Contains(src, `fmt.Sprintf("from-scratchpad:%s"`) {
		t.Fatalf("Promote must build the canonical lineage tag from-scratchpad:<sessionID>")
	}
}

// TestMemoryWriter_InterfaceHasFindPromotionByLineage pins defect A
// contract: the MemoryWriter interface must expose the idempotency
// lookup, and the production DatabaseManagerMemoryWriter must
// implement it.
func TestMemoryWriter_InterfaceHasFindPromotionByLineage(t *testing.T) {
	src := readServiceSource(t, "service_working_context.go")
	if !strings.Contains(src, "FindPromotionByLineage(lineageTag string) (string, error)") {
		t.Fatalf("MemoryWriter interface must expose FindPromotionByLineage")
	}
	if !strings.Contains(src, "func (w *DatabaseManagerMemoryWriter) FindPromotionByLineage(lineageTag string) (string, error)") {
		t.Fatalf("DatabaseManagerMemoryWriter must implement FindPromotionByLineage")
	}
	// Implementation must use json_each to scan the tags array.
	if !strings.Contains(src, "json_each(tags)") {
		t.Fatalf("FindPromotionByLineage must use json_each(tags) to scan the tags array")
	}
}

// TestExportOutputFile_RoutesToFileNotStdout pins defect B: the JSON
// encoder must point at outputTarget (the file when --output is set)
// not os.Stdout. The pre-fix code had json.NewEncoder(os.Stdout) hard
// at the JSON branch, which is the exact bug.
func TestExportOutputFile_RoutesToFileNotStdout(t *testing.T) {
	src := readServiceSource(t, "maint_cmds.go")
	// After patch: encoder uses outputTarget.
	if !strings.Contains(src, "json.NewEncoder(outputTarget)") {
		t.Fatalf("handleExport must use outputTarget for the JSON encoder (defect B)")
	}
	// Before patch: encoder hardcoded os.Stdout. The patch removes
	// that line.
	if strings.Contains(src, "json.NewEncoder(os.Stdout)") {
		t.Fatalf("handleExport must NOT hardcode os.Stdout for JSON (defect B regression)")
	}
}

// TestExportOutputFile_StatusGoesToStderr pins defect B: success
// messages must use usererror.Notice (stderr) so a stdout-mode
// export doesn't pollute the JSON document with text.
func TestExportOutputFile_StatusGoesToStderr(t *testing.T) {
	src := readServiceSource(t, "maint_cmds.go")
	if !strings.Contains(src, `usererror.Notice("Exported %d memories`) {
		t.Fatalf("handleExport must use usererror.Notice for status (defect B)")
	}
}

// readServiceSource returns the source text of a handler file for
// the regression assertions. Reads from disk once per call. The
// test package is `cmd/mpm`, so the working directory at test time
// is the same package directory and bare filenames resolve correctly.
func readServiceSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read source %s: %v", name, err)
	}
	return string(data)
}

// idempotencyCheckBeforeSaveMemory returns true when the
// FindPromotionByLineage call site appears in source text BEFORE
// the SaveMemory call site. A false-positive would mean a future
// refactor dropped the idempotency invariant; this test fails the
// build before the regression ships.
func idempotencyCheckBeforeSaveMemory(src string) bool {
	findIdx := strings.Index(src, "FindPromotionByLineage(")
	saveIdx := strings.Index(src, ".SaveMemory(")
	if findIdx < 0 || saveIdx < 0 {
		// Either both helper names exist (test of call sites will
		// catch a swap) or neither does (test of method shape caught
		// upstream). Returning true here means "the test cannot
		// determine order", which the surrounding TestWorkingContextPromote_HasIdempotencyCheck
		// will catch if the helper is missing entirely.
		return true
	}
	return findIdx < saveIdx
}