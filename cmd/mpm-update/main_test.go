package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm/internal/update"
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

// ---------------------------------------------------------------------
// Dispatch: a BARE invocation is the default update workflow
// ---------------------------------------------------------------------
//
// The defect these pin: `run` returned the usage text for len(args) == 0,
// so BOTH `mpm-update` and `mpm update` (which delegates with no args)
// printed help and exited 0 instead of updating. Help is documented as
// something you ASK for (--help), never something a bare invocation gets.

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

// stubWorkflow replaces the update workflow and reports whether it ran and
// with what options. Restored automatically at test end.
func stubWorkflow(t *testing.T) (*bool, *update.Options) {
	t.Helper()
	called := false
	var got update.Options
	orig := updateRunner
	updateRunner = func(_ context.Context, o update.Options, _ bool) int {
		called = true
		got = o
		return 0
	}
	t.Cleanup(func() { updateRunner = orig })
	return &called, &got
}

func TestRunBareInvocationRunsTheDefaultUpdate(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("HOME", ws)
	called, got := stubWorkflow(t)

	var rc int
	out := captureStdout(t, func() { rc = run(nil) })

	if rc != 0 {
		t.Fatalf("run(nil) = %d, want 0", rc)
	}
	if !*called {
		t.Fatalf("a bare `mpm-update` did not reach the update workflow; it printed:\n%s", out)
	}
	if strings.Contains(out, "Usage:") {
		t.Errorf("a bare `mpm-update` printed the usage banner instead of updating:\n%s", out)
	}
	// The implicit default must stay the stable release. `main` is a
	// development branch and must only be selected explicitly.
	if got.WantBranch != "" {
		t.Errorf("implicit default selected branch %q; the default source must be the latest stable release", got.WantBranch)
	}
	if got.WantVersion != "" || got.WantCommit != "" || got.WantLocal != "" {
		t.Errorf("implicit default pinned a source: %+v", got)
	}
	if got.CheckOnly || got.DryRun || got.Rollback {
		t.Errorf("bare invocation set a read-only/destructive mode: %+v", got)
	}
}

func TestRunHelpNeverRunsTheWorkflow(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			ws := t.TempDir()
			t.Setenv("MPM_WORKSPACE", ws)
			t.Setenv("HOME", ws)
			called, _ := stubWorkflow(t)

			var rc int
			out := captureStdout(t, func() { rc = run(args) })

			if rc != 0 {
				t.Errorf("run(%v) = %d, want 0", args, rc)
			}
			if *called {
				t.Errorf("run(%v) ran the update workflow; --help must never update", args)
			}
			if !strings.Contains(out, "Usage:") {
				t.Errorf("run(%v) did not print usage", args)
			}
			// Help must not create anything either.
			if entries, err := os.ReadDir(ws); err == nil && len(entries) != 0 {
				t.Errorf("help created files in the workspace: %v", entries)
			}
		})
	}
}

func TestRunReadOnlyModesKeepTheirSemantics(t *testing.T) {
	// --status is the only mode that takes no action flag and needs no
	// network; it must still route to status, not to the workflow.
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	called, _ := stubWorkflow(t)

	var rc int
	captureStdout(t, func() { rc = run([]string{"--status"}) })
	if rc != 0 {
		t.Fatalf("--status = %d, want 0", rc)
	}
	if *called {
		t.Error("--status reached the update workflow")
	}
}
