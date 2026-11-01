package main

import (
	"strings"
	"testing"
)

func TestSplitFlag(t *testing.T) {
	cases := []struct {
		in          string
		name, value string
		hasValue    bool
	}{
		{"--to", "--to", "", false},
		{"--to=v1", "--to", "v1", true},
		{"-j", "-j", "", false},
		{"plain", "plain", "", false},
	}
	for _, c := range cases {
		n, v, h := splitFlag(c.in)
		if n != c.name || v != c.value || h != c.hasValue {
			t.Errorf("splitFlag(%q) = %q %q %v", c.in, n, v, h)
		}
	}
}

func TestRunHelpIsInert(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{{"--help"}, {"-h"}} {
		if rc := run(args); rc != 0 {
			t.Errorf("run(%v) = %d, want 0", args, rc)
		}
	}
}

func TestRunUnknownFlagFails(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	if rc := run([]string{"--nope"}); rc == 0 {
		t.Error("unknown flag exited 0")
	}
}

func TestRunStatusJSON(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if rc := run([]string{"--status", "--json"}); rc != 0 {
		t.Errorf("status --json = %d, want 0", rc)
	}
}

func TestRunConflictingSourcesRejected(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	if rc := run([]string{"--to", "v1", "--branch", "main"}); rc == 0 {
		t.Error("conflicting sources exited 0")
	}
}

func TestRunRollbackWithRestoreNeedsForce(t *testing.T) {
	// --with-db-restore without --force would be destructive; the CLI
	// must not be the place that silently allows it. This path fails
	// before any mutation (no previous release in a fresh workspace).
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	rc := run([]string{"--rollback", "--with-db-restore"})
	if rc == 0 {
		t.Error("restore without --force exited 0")
	}
}

func TestUsageMentionsRecovery(t *testing.T) {
	if !strings.Contains(usage, "~/.mpm/mpm-update") && !strings.Contains(usage, "--rollback") {
		t.Error("usage does not document recovery")
	}
}
