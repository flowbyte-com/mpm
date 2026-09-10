// f0910_regression_repair_test.go — 2026-09-10 regression repair tests
// for T45, T56, T78.
//
//   T45: `mpm evidence list` (bare form) must list evidence
//        without requiring --artifact. The filtered form
//        `--artifact <id>` continues to work.
//
//   T56: `mpm work item note <id> "text"` (positional) must work.
//        The flag form `--note "text"` continues to work. Missing
//        note fails cleanly.
//
//   T78: `mpm restore-db <external-path>` must accept a path
//        outside the database directory. The pre-fix behavior
//        rejected any external path. Safety checks still apply
//        (file existence, regular-file mode). The actual SQL
//        execution path depends on the canonical dump validator,
//        which is exercised separately.
//
// These tests run against a real binary built from the working tree
// via `make build`. They use a fresh tempdir workspace per case so
// destructive operations never touch the production substrate.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// findBin locates the freshly-built mpm binary. Tests that need to
// run the binary rely on this — the existing `TestHelp_NeverExecutesHandlers`
// pattern in cmd/mpm/help_regression_test.go is the precedent.
func findBin(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join("..", "..", "bin", "mpm")
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("bin/mpm not built (%v); run `make build` first", err)
	}
	return binPath
}

// runMpm invokes the binary with the supplied args in the
// supplied workspace. Returns the combined stdout+stderr text
// and the exit code. Inherits the test process's PATH so
// subprocesses like `sqlite3` (used by `mpm backup`) resolve
// correctly.
func runMpm(t *testing.T, bin, workspace string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v\n%s", args, err, out)
	}
	return string(out), code
}

// ── T45: evidence list ─────────────────────────────────────────────

// TestF0910_T45_EvidenceList_BareForm pins the bare
// `mpm evidence list` form. Pre-fix this returned
// `--artifact is required` and refused to run without a filter.
// Post-fix it lists available evidence using the documented
// default behavior.
func TestF0910_T45_EvidenceList_BareForm(t *testing.T) {
	bin := findBin(t)
	ws := t.TempDir()

	out, code := runMpm(t, bin, ws, "memory", "add", "t45-source")
	if code != 0 {
		t.Fatalf("seed add failed: %s", out)
	}
	memID := strings.Fields(out)[len(strings.Fields(out))-1]
	memID = strings.TrimPrefix(memID, ":")
	if memID == "" {
		t.Fatalf("could not extract memory id from: %s", out)
	}

	// Bare `mpm evidence list` must succeed.
	out, code = runMpm(t, bin, ws, "evidence", "list")
	if code != 0 {
		t.Errorf("bare `mpm evidence list` must succeed; got exit=%d out=%s", code, out)
	}
	if strings.Contains(out, "--artifact is required") {
		t.Errorf("bare `mpm evidence list` must not require --artifact; got: %s", out)
	}
	if !strings.Contains(out, `"success":true`) {
		t.Errorf("bare list response must be JSON envelope; got: %s", out)
	}
}

// TestF0910_T45_EvidenceList_FilteredForm pins the
// `--artifact <id>` filtered form continues to work after the
// bare-list regression repair.
func TestF0910_T45_EvidenceList_FilteredForm(t *testing.T) {
	bin := findBin(t)
	ws := t.TempDir()

	out, code := runMpm(t, bin, ws, "memory", "add", "t45-filter")
	if code != 0 {
		t.Fatalf("seed add failed: %s", out)
	}
	memID := strings.Fields(out)[len(strings.Fields(out))-1]
	memID = strings.TrimPrefix(memID, ":")

	out, code = runMpm(t, bin, ws, "evidence", "list", "--artifact", memID)
	if code != 0 {
		t.Errorf("filtered list must succeed; got exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, memID) {
		t.Errorf("filtered list response must reference the artifact id; got: %s", out)
	}
}

// ── T56: work item note positional/flag parity ────────────────────

// TestF0910_T56_WorkNote_PositionalForm pins the natural
// positional form. Pre-fix this returned `note is required`
// even though positional note text was previously accepted.
func TestF0910_T56_WorkNote_PositionalForm(t *testing.T) {
	bin := findBin(t)
	ws := t.TempDir()

	_, code := runMpm(t, bin, ws, "work", "item", "create", "t56 title", "t56 content")
	if code != 0 {
		t.Fatalf("seed create failed: exit=%d", code)
	}

	// Get work id via list (JSON).
	listOut, _ := runMpm(t, bin, ws, "call", "mpm_work",
		"--payload", `{"action":"list","params":{"limit":1}}`)
	wid := extractFirstID(t, listOut)

	// Positional note must succeed.
	out, code := runMpm(t, bin, ws, "work", "item", "note", wid, "positional note text")
	if code != 0 {
		t.Errorf("positional note must succeed; got exit=%d out=%s", code, out)
	}
	if strings.Contains(out, `"note is required`) {
		t.Errorf("positional note must not require --note flag; got: %s", out)
	}
	if !strings.Contains(out, `"note":"positional note text"`) {
		t.Errorf("positional note must be recorded; got: %s", out)
	}
}

// TestF0910_T56_WorkNote_FlagForm pins the `--note "text"` flag
// form continues to work after the regression repair.
func TestF0910_T56_WorkNote_FlagForm(t *testing.T) {
	bin := findBin(t)
	ws := t.TempDir()

	_, code := runMpm(t, bin, ws, "work", "item", "create", "t56 title", "t56 content")
	if code != 0 {
		t.Fatalf("seed create failed: exit=%d", code)
	}
	listOut, _ := runMpm(t, bin, ws, "call", "mpm_work",
		"--payload", `{"action":"list","params":{"limit":1}}`)
	wid := extractFirstID(t, listOut)

	out, code := runMpm(t, bin, ws, "work", "item", "note", wid, "--note", "flag note text")
	if code != 0 {
		t.Errorf("flag note must succeed; got exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, `"note":"flag note text"`) {
		t.Errorf("flag note must be recorded; got: %s", out)
	}
}

// TestF0910_T56_WorkNote_MissingNoteFails pins the clean validation
// error when the operator supplies no note text (neither positional
// nor --note).
func TestF0910_T56_WorkNote_MissingNoteFails(t *testing.T) {
	bin := findBin(t)
	ws := t.TempDir()

	_, _ = runMpm(t, bin, ws, "work", "item", "create", "t56 title", "t56 content")
	listOut, _ := runMpm(t, bin, ws, "call", "mpm_work",
		"--payload", `{"action":"list","params":{"limit":1}}`)
	wid := extractFirstID(t, listOut)

	out, code := runMpm(t, bin, ws, "work", "item", "note", wid)
	if code == 0 {
		t.Errorf("missing note must error; got exit=0 out=%s", out)
	}
	if !strings.Contains(out, "note is required") {
		t.Errorf("missing-note error must mention 'note is required'; got: %s", out)
	}
}

// ── T78: restore-db path validation ────────────────────────────────

// TestF0910_T78_RestoreDB_ExternalPathAcceptedByPathCheck pins
// the path-validation contract. Pre-fix this returned
// `Restore path must be inside the database directory ...` for
// any external path. Post-fix the path is accepted (the
// downstream canonical dump validator may then reject the
// content with its own distinct error — but that's a separate
// layer from the path check this test pins).
func TestF0910_T78_RestoreDB_ExternalPathAcceptedByPathCheck(t *testing.T) {
	bin := findBin(t)
	ws := t.TempDir()
	_ = ws

	// Create a minimal SQL dump file in /tmp (outside any workspace).
	dumpPath := filepath.Join(t.TempDir(), "external-dump.sql")
	if err := os.WriteFile(dumpPath, []byte("PRAGMA foreign_keys=OFF;\n"), 0644); err != nil {
		t.Fatalf("write dump: %v", err)
	}

	out, _ := runMpm(t, bin, ws, "restore-db", dumpPath)
	// We don't care about the exit code here — the canonical
	// dump validator may reject the minimal content with its own
	// error. We only care that the error is NOT the
	// "Restore path must be inside the database directory" path
	// rejection.
	if strings.Contains(out, "Restore path must be inside the database directory") {
		t.Errorf("external path must not be rejected by path check; got: %s", out)
	}
	if strings.Contains(out, "no such file or directory") &&
		!strings.Contains(out, dumpPath) {
		// Path-not-found error should mention the actual path so
		// operators can see what was attempted.
		t.Errorf("path-related error must reference the supplied path %q; got: %s", dumpPath, out)
	}
}

// TestF0910_T78_RestoreDB_NonexistentPathFails pins the safety
// check for a non-existent file. The error must mention the
// path and must NOT claim the path is wrong because it's
// outside the DB dir.
func TestF0910_T78_RestoreDB_NonexistentPathFails(t *testing.T) {
	bin := findBin(t)
	ws := t.TempDir()
	missing := filepath.Join(t.TempDir(), "nonexistent-dump.sql")

	out, code := runMpm(t, bin, ws, "restore-db", missing)
	if code == 0 {
		t.Errorf("nonexistent path must fail; got exit=0 out=%s", out)
	}
	if strings.Contains(out, "Restore path must be inside the database directory") {
		t.Errorf("nonexistent path must not trigger DB-dir restriction; got: %s", out)
	}
	if !strings.Contains(out, "Cannot read") && !strings.Contains(out, "no such file") {
		t.Errorf("nonexistent path error must indicate missing file; got: %s", out)
	}
}

// TestF0910_T78_RestoreDB_DirectoryNotFileFails pins the safety
// check that requires the path to be a regular file (not a
// directory).
func TestF0910_T78_RestoreDB_DirectoryNotFileFails(t *testing.T) {
	bin := findBin(t)
	ws := t.TempDir()
	dirAsBackup := t.TempDir() // a directory, not a file

	out, code := runMpm(t, bin, ws, "restore-db", dirAsBackup)
	if code == 0 {
		t.Errorf("directory-as-path must fail; got exit=0 out=%s", out)
	}
	if strings.Contains(out, "Restore path must be inside the database directory") {
		t.Errorf("directory-as-path must not trigger DB-dir restriction; got: %s", out)
	}
}

// TestF0910_T78_RestoreDB_InTreeBackupPathAccepted pins that
// the in-tree backup path continues to work after the path
// relaxation.
func TestF0910_T78_RestoreDB_InTreeBackupPathAccepted(t *testing.T) {
	bin := findBin(t)
	ws := t.TempDir()

	// Make a backup inside the workspace's DB dir.
	_, _ = runMpm(t, bin, ws, "memory", "add", "t78-in-tree-source")
	backupOut, _ := runMpm(t, bin, ws, "backup")
	// Extract path from "Backup written: <path>"
	inTree := extractBackupPath(t, backupOut)
	if !strings.HasPrefix(inTree, ws) {
		t.Fatalf("backup path %q should be inside workspace %q", inTree, ws)
	}

	// Path check must accept (the validator may still reject the
	// dump content, but the path must not be the failure cause).
	out, code := runMpm(t, bin, ws, "restore-db", inTree)
	if strings.Contains(out, "Restore path must be inside the database directory") {
		t.Errorf("in-tree backup path must still pass path check; got: %s", out)
	}
	_ = code // exit code not asserted — see above
}

// extractFirstID extracts the first "id":"<uuid>" pair from a
// JSON-encoded list response. Used by the work-note tests.
func extractFirstID(t *testing.T, jsonList string) string {
	t.Helper()
	idx := strings.Index(jsonList, `"id":"`)
	if idx < 0 {
		t.Fatalf("no id field in: %s", jsonList)
	}
	rest := jsonList[idx+len(`"id":"`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("malformed id field in: %s", jsonList)
	}
	return rest[:end]
}

// extractBackupPath extracts the backup path from a
// "Backup written: <path>" line. Used by T78.
func extractBackupPath(t *testing.T, backupOut string) string {
	t.Helper()
	const marker = "Backup written: "
	idx := strings.Index(backupOut, marker)
	if idx < 0 {
		t.Fatalf("no backup marker in: %s", backupOut)
	}
	rest := backupOut[idx+len(marker):]
	end := strings.IndexAny(rest, " \n\r\t")
	if end < 0 {
		return rest
	}
	return rest[:end]
}
