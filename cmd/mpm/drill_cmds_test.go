// drill_cmds_test.go — exercise the CLI dispatcher for `mpm drills`.
//
// Tests:
//   - findDrillPath: positive, negative, missing-dir
//   - parseDrillRunFlags: defaults, --no-compliant, --framework, --timeout
//   - handleDrillsList: enumerates installed drills
//
// The full run() path is already covered by TestDrillHandler_* in
// internal/scheduler plus the smoke-test the engineer runs before
// merging; re-running it here would just rebuild the mpm binary and
// shell-mpm-call sub-shells, which is more about integration than
// unit testing the CLI plumbing.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindDrillPath_MatchByID(t *testing.T) {
	ws := t.TempDir()
	drillsDir := filepath.Join(ws, DrillsDir)
	if err := os.MkdirAll(drillsDir, 0o755); err != nil {
		t.Fatalf("mkdir drills: %v", err)
	}
	// Filename deliberately differs from id (the contract is the id).
	yamlBody := []byte(`
id: lesson-persistence-001
description: x
framework: synthetic
prompt: do the thing
expect:
  tools_required: [mpm_lessons]
  sequence:
    - tool: mpm_lessons
      action: save
`)
	if err := os.WriteFile(filepath.Join(drillsDir, "lesson.yaml"), yamlBody, 0o644); err != nil {
		t.Fatalf("write drill: %v", err)
	}
	t.Setenv("MPM_WORKSPACE", ws)

	got, err := findDrillPath("lesson-persistence-001")
	if err != nil {
		t.Fatalf("findDrillPath: %v", err)
	}
	if filepath.Base(got) != "lesson.yaml" {
		t.Errorf("path = %q, want lesson.yaml", got)
	}
}

func TestFindDrillPath_NotFound(t *testing.T) {
	ws := t.TempDir()
	drillsDir := filepath.Join(ws, DrillsDir)
	if err := os.MkdirAll(drillsDir, 0o755); err != nil {
		t.Fatalf("mkdir drills: %v", err)
	}
	t.Setenv("MPM_WORKSPACE", ws)

	_, err := findDrillPath("does-not-exist")
	if err == nil {
		t.Fatal("expected error for missing drill")
	}
}

func TestFindDrillPath_NoDrillsDir(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	_, err := findDrillPath("anything")
	if err == nil {
		t.Fatal("expected error when drills/ directory is missing")
	}
}

func TestParseDrillRunFlags_Defaults(t *testing.T) {
	fw, compliant, timeout, rest := parseDrillRunFlags([]string{})
	if fw != "" || !compliant || timeout != 0 {
		t.Errorf("defaults: fw=%q compliant=%v timeout=%d rest=%v", fw, compliant, timeout, rest)
	}
}

func TestParseDrillRunFlags_AllThreeFlags(t *testing.T) {
	fw, compliant, timeout, rest := parseDrillRunFlags([]string{
		"--framework", "claude_code",
		"--no-compliant",
		"--timeout", "99",
		"trailing-arg",
	})
	if fw != "claude_code" {
		t.Errorf("framework = %q, want claude_code", fw)
	}
	if compliant {
		t.Error("expected compliant=false")
	}
	if timeout != 99 {
		t.Errorf("timeout = %d, want 99", timeout)
	}
	if len(rest) != 1 || rest[0] != "trailing-arg" {
		t.Errorf("rest = %v, want [trailing-arg]", rest)
	}
}

func TestParseDrillRunFlags_UnparseableTimeout(t *testing.T) {
	fw, compliant, timeout, rest := parseDrillRunFlags([]string{"--timeout", "not-a-number"})
	if fw != "" || !compliant || timeout != 0 {
		t.Errorf("garbage timeout: fw=%q compliant=%v timeout=%d rest=%v", fw, compliant, timeout, rest)
	}
	if len(rest) != 0 {
		t.Errorf("garbage timeout should leave rest empty, got %v", rest)
	}
}

func TestHandleDrillsList_Empty(t *testing.T) {
	ws := t.TempDir()
	drillsDir := filepath.Join(ws, DrillsDir)
	if err := os.MkdirAll(drillsDir, 0o755); err != nil {
		t.Fatalf("mkdir drills: %v", err)
	}
	t.Setenv("MPM_WORKSPACE", ws)

	if rc := handleDrillsList([]string{}); rc != 0 {
		t.Errorf("empty list rc = %d, want 0", rc)
	}
}

func TestHandleDrillsList_WithDrills(t *testing.T) {
	ws := t.TempDir()
	drillsDir := filepath.Join(ws, DrillsDir)
	if err := os.MkdirAll(drillsDir, 0o755); err != nil {
		t.Fatalf("mkdir drills: %v", err)
	}
	yamlBody := []byte(`
id: drill-x
description: x
framework: synthetic
prompt: p
expect:
  tools_required: [t]
  sequence:
    - tool: t
      action: a
`)
	if err := os.WriteFile(filepath.Join(drillsDir, "x.yaml"), yamlBody, 0o644); err != nil {
		t.Fatalf("write drill: %v", err)
	}
	t.Setenv("MPM_WORKSPACE", ws)

	if rc := handleDrillsList([]string{}); rc != 0 {
		t.Errorf("list rc = %d, want 0", rc)
	}
}

func TestHandleDrills_UnknownSubcommand(t *testing.T) {
	rc := handleDrills([]string{"drills", "frobnicate"})
	if rc == 0 {
		t.Error("unknown subcommand should non-zero exit")
	}
}

func TestHandleDrills_NoSubcommand(t *testing.T) {
	if rc := handleDrills([]string{"drills"}); rc != 0 {
		t.Errorf("no subcommand rc = %d, want 0 (help)", rc)
	}
}
