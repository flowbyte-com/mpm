package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `mpm shred modes` / `shred personas` in bulk must never delete git-tracked
// source. Under the canonical layout the checkout IS the workspace root
// (`git clone … ~/.mpm`), so mode/ and persona/ are tracked directories that
// happen to sit at the runtime root.
//
// These tests call the handlers directly, which is the boundary the guard
// lives on. Note that the CLI confirmation flag does not currently reach
// them: router.parseFlags (router.go:500) consumes `-f`/`--force` from argv
// and re-publishes it as `MPM_FORCE=1`, which handleRm reads but the shred
// handlers do not — so `mpm shred modes -f` prints its "use -f to confirm"
// warning and exits 1. That flag bug is pre-existing and tracked separately;
// it is why the bulk-delete path is currently unreachable from the shipped
// CLI, and therefore why the underlying RemoveAll() guard is latent rather
// than actively exploited. The guard still has to be here: it is the only
// thing standing between a fixed flag handler and silent deletion of
// repository source.
//
// Isolation: MPM_WORKSPACE is pinned to t.TempDir() and a real throwaway Git
// repository is created inside it. Nothing here touches the live checkout or
// the production database.

func newShredWorkspace(t *testing.T) string {
	t.Helper()
	// t.TempDir() can land under the repository being tested, in which case
	// it is already inside a Git worktree. Create a subdirectory so the
	// test controls its own Git state exactly.
	ws := filepath.Join(t.TempDir(), "mpm-workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	t.Setenv("MPM_WORKSPACE", ws)

	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v\n%s", err, out)
		}
	}
	return ws
}

func seedTrackedMarkdown(t *testing.T, ws, dir string, names ...string) {
	t.Helper()
	full := filepath.Join(ws, dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(full, n), []byte("---\nname: x\n---\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "seed"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func assertFileSurvives(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("tracked source %s was deleted by shred: %v", path, err)
	}
}

func TestShredModesRefusesToDeleteTrackedSource(t *testing.T) {
	ws := newShredWorkspace(t)
	seedTrackedMarkdown(t, ws, "mode", "default.md", "architect.md", "README.md")

	// The handler is destructive and returns a non-zero exit code on failure;
	// what matters here is that the files survive.
	_ = handleShredModes([]string{"--force"})

	assertFileSurvives(t, filepath.Join(ws, "mode", "default.md"))
	assertFileSurvives(t, filepath.Join(ws, "mode", "architect.md"))
	assertFileSurvives(t, filepath.Join(ws, "mode", "README.md"))

	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = ws
	if out, err := cmd.CombinedOutput(); err == nil && strings.TrimSpace(string(out)) != "" {
		t.Errorf("shred modes dirtied the worktree:\n%s", out)
	}
}

func TestShredPersonasRefusesToDeleteTrackedSource(t *testing.T) {
	ws := newShredWorkspace(t)
	seedTrackedMarkdown(t, ws, "persona", "default.md", "critic.md")

	_ = handleShredPersonas([]string{"--force"})

	assertFileSurvives(t, filepath.Join(ws, "persona", "default.md"))
	assertFileSurvives(t, filepath.Join(ws, "persona", "critic.md"))
}

// The guard must not disable the feature where it is safe: a workspace that
// is not a checkout still bulk-deletes mode files.
func TestShredModesStillWorksInNonGitWorkspace(t *testing.T) {
	// A plain runtime workspace with no version control at all. The
	// directory is created outside any repository, so `git rev-parse`
	// cannot succeed here even by walking up.
	ws := filepath.Join(t.TempDir(), "mpm-plain")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("MPM_WORKSPACE", ws)
	modeDir := filepath.Join(ws, "mode")
	if err := os.MkdirAll(modeDir, 0o755); err != nil {
		t.Fatalf("mkdir mode: %v", err)
	}
	for _, n := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(modeDir, n), []byte("---\n---\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}

	_ = handleShredModes([]string{"--force"})

	for _, n := range []string{"a.md", "b.md"} {
		if _, err := os.Stat(filepath.Join(modeDir, n)); !os.IsNotExist(err) {
			t.Errorf("%s survived a legitimate non-Git bulk delete; the guard is over-broad", n)
		}
	}
}
