package internal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The canonical MPM layout is `git clone … ~/.mpm` — the checkout IS the
// workspace root. `mode/` and `persona/` are therefore git-tracked source
// directories that sit at the runtime root, and bulk-shredding them would
// delete tracked files and dirty the worktree.
//
// These tests pin the refusal. Detection asks Git directly rather than
// testing for a `.git` directory, because a linked worktree (and a
// submodule) carries `.git` as a *file*.

func writeModeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("---\n---\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("git %v unavailable/failed in this environment: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepo creates a real Git checkout at dir — required so the guard is
// exercised against Git's own answer, not a hand-made marker.
func initRepo(t *testing.T, dir string) {
	t.Helper()
	gitOut(t, dir, "init", "-q", ".")
	gitOut(t, dir, "config", "user.email", "test@example.invalid")
	gitOut(t, dir, "config", "user.name", "Test")
}

func TestModeRemoveAll_RefusesInNormalGitCheckout(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	modeDir := filepath.Join(root, "mode")
	writeModeFiles(t, modeDir, "default.md", "architect.md", "README.md")
	gitOut(t, root, "add", "-A")
	gitOut(t, root, "commit", "-qm", "tracked modes")

	mm := &ModeManager{Dir: modeDir, ActiveFile: filepath.Join(root, "active.json")}
	n, err := mm.RemoveAll()
	if err == nil {
		t.Fatal("RemoveAll succeeded inside a Git checkout; want refusal (would delete tracked source)")
	}
	if !strings.Contains(err.Error(), "refusing to shred modes") {
		t.Errorf("error = %q, want it to explain the refusal", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0 on refusal", n)
	}
	// The critical assertion: nothing was deleted.
	if _, err := os.Stat(filepath.Join(modeDir, "default.md")); err != nil {
		t.Error("default.md was deleted — the guard did not prevent destruction")
	}
	dirty := gitOut(t, root, "status", "--porcelain")
	if strings.TrimSpace(dirty) != "" {
		t.Errorf("worktree dirtied by shred:\n%s", dirty)
	}
}

func TestModeRemoveAll_RefusesWhenGitIsAFile(t *testing.T) {
	// Linked-worktree / submodule shape: `.git` is a regular file holding a
	// `gitdir:` pointer, NOT a directory. A `.git`-is-a-dir check would miss
	// this and delete tracked source.
	root := t.TempDir()
	initRepo(t, root)
	realMode := filepath.Join(root, "mode")
	writeModeFiles(t, realMode, "default.md")
	gitOut(t, root, "add", "-A")
	gitOut(t, root, "commit", "-qm", "tracked modes")

	// Build a genuine linked worktree, then assert `.git` really is a file.
	wt := filepath.Join(t.TempDir(), "wt")
	gitOut(t, root, "worktree", "add", "-q", wt, "-b", "wt-branch")
	info, err := os.Lstat(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatalf("lstat .git: %v", err)
	}
	if info.IsDir() {
		t.Skip("environment produced a .git directory; this test targets the file form")
	}

	mm := &ModeManager{
		Dir:        filepath.Join(wt, "mode"),
		ActiveFile: filepath.Join(wt, "active.json"),
	}
	if _, err := mm.RemoveAll(); err == nil {
		t.Error("RemoveAll succeeded in a linked worktree; want refusal")
	} else if !strings.Contains(err.Error(), "refusing to shred modes") {
		t.Errorf("error = %q, want the refusal message", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "mode", "default.md")); err != nil {
		t.Error("default.md deleted in a linked worktree — source destroyed")
	}
}

func TestModeRemoveAll_PreservedInNonGitWorkspace(t *testing.T) {
	// An install prefix that is not a checkout holds only runtime state;
	// the original bulk-delete behaviour must be preserved there.
	root := t.TempDir()
	modeDir := filepath.Join(root, "mode")
	writeModeFiles(t, modeDir, "a.md", "b.md", "notes.txt")

	mm := &ModeManager{Dir: modeDir, ActiveFile: filepath.Join(root, "active.json")}
	n, err := mm.RemoveAll()
	if err != nil {
		t.Fatalf("RemoveAll in a non-Git workspace: %v (behaviour must be preserved)", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (only .md files)", n)
	}
	if _, err := os.Stat(filepath.Join(modeDir, "a.md")); !os.IsNotExist(err) {
		t.Error("a.md survived a non-Git bulk delete")
	}
	if _, err := os.Stat(filepath.Join(modeDir, "notes.txt")); err != nil {
		t.Error("notes.txt was deleted; only .md files are in scope")
	}
}

func TestPersonaRemoveAll_RefusesInGitCheckout(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	personaDir := filepath.Join(root, "persona")
	writeModeFiles(t, personaDir, "default.md", "critic.md")
	gitOut(t, root, "add", "-A")
	gitOut(t, root, "commit", "-qm", "tracked personas")

	pm := &PersonaManager{Dir: personaDir, ActiveFile: filepath.Join(root, "active.json")}
	n, err := pm.RemoveAll()
	if err == nil {
		t.Fatal("PersonaManager.RemoveAll succeeded in a Git checkout; want refusal")
	}
	if !strings.Contains(err.Error(), "refusing to shred personas") {
		t.Errorf("error = %q, want the refusal message", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(personaDir, "default.md")); err != nil {
		t.Error("default.md deleted — the guard did not prevent destruction")
	}
}

func TestPersonaRemoveAll_PreservedInNonGitWorkspace(t *testing.T) {
	root := t.TempDir()
	personaDir := filepath.Join(root, "persona")
	writeModeFiles(t, personaDir, "x.md", "y.md")

	pm := &PersonaManager{Dir: personaDir, ActiveFile: filepath.Join(root, "active.json")}
	n, err := pm.RemoveAll()
	if err != nil {
		t.Fatalf("RemoveAll in a non-Git workspace: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

// The refusal must not contradict AddMode, which already declines to
// create these files because they are managed directly in the directory.
func TestAddModeStillDeclinesInGitCheckout(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	modeDir := filepath.Join(root, "mode")
	writeModeFiles(t, modeDir, "default.md")

	mm := &ModeManager{Dir: modeDir, ActiveFile: filepath.Join(root, "active.json")}
	if err := mm.AddMode("brand-new"); err == nil {
		t.Error("AddMode succeeded; it must continue to decline (source-owned files)")
	}
	if _, err := os.Stat(filepath.Join(modeDir, "brand-new.md")); !os.IsNotExist(err) {
		t.Error("AddMode created a file; source-owned invariant broken")
	}
}

func TestIsInsideGitWorktree(t *testing.T) {
	inside := t.TempDir()
	initRepo(t, inside)
	if !isInsideGitWorktree(inside) {
		t.Error("a real Git checkout reported as not-inside-worktree")
	}
	// Nested subdirectory must also resolve (Git walks up to the root).
	nested := filepath.Join(inside, "mode", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if !isInsideGitWorktree(nested) {
		t.Error("a nested dir inside a checkout reported as not-inside-worktree")
	}
	outside := t.TempDir()
	if isInsideGitWorktree(outside) {
		t.Error("a non-Git temp dir reported as inside a worktree")
	}
	if isInsideGitWorktree("") {
		t.Error("empty dir reported as inside a worktree")
	}
	if isInsideGitWorktree(filepath.Join(t.TempDir(), "does", "not", "exist")) {
		t.Error("nonexistent path reported as inside a worktree")
	}
}
