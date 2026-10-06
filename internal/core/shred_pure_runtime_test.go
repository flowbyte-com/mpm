package internal

// Pure-runtime shred semantics.
//
// shred_git_source_test.go pins the refusal that protects the COLOCATED
// layout, where the checkout at ~/.mpm means mode/ and persona/ are
// git-tracked source. That guard is retained.
//
// This file pins the other half, which becomes the normal case once the
// source checkout and the runtime root are physically separate: in a
// pure runtime workspace, mode/ and persona/ ARE runtime state. They are
// provisioned, operator-owned copies (see
// scripts/install_runtime_assets.py), not repository content. Two
// properties follow and both are asserted here:
//
//  1. The bulk shred is permitted. A guard that refuses everywhere would
//     make `mpm shred modes -f` permanently unusable for real
//     deployments, which is the opposite of the intended fix.
//
//  2. It touches ONLY the runtime workspace. The operator's source
//     checkout, which now lives elsewhere and still holds the canonical
//     definitions, must be completely untouched. This is the property
//     that makes the shred safe to expose again.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertNotInsideGitWorktree guards the premise of every test in this
// file: they all require a workspace that Git does not claim. If the
// ambient temp directory were ever inside a checkout, the tests would
// silently be exercising the refusal path instead of the runtime path,
// and would pass for the wrong reason.
func assertNotInsideGitWorktree(t *testing.T, dir string) {
	t.Helper()
	if isInsideGitWorktree(dir) {
		t.Fatalf(
			"precondition violated: %s is inside a Git worktree, so these tests would "+
				"exercise the refusal path rather than the pure-runtime path",
			dir,
		)
	}
}

func writeDefinition(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// newSplitWorkspace builds the post-migration world:
//
//	<tmp>/src/mpm/mode      the operator's source checkout (elsewhere)
//	<tmp>/home/.mpm/mode   the pure runtime root
//
// and returns both paths.
func newSplitWorkspace(t *testing.T) (runtimeRoot, sourceRoot string) {
	t.Helper()
	base := t.TempDir()
	runtimeRoot = filepath.Join(base, "home", ".mpm")
	sourceRoot = filepath.Join(base, "src", "mpm")
	for _, d := range []string{runtimeRoot, sourceRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	assertNotInsideGitWorktree(t, filepath.Join(runtimeRoot, "mode"))

	// Canonical definitions exist in BOTH places: provisioned runtime
	// copies plus the untouched source checkout. These are the REAL
	// stock definitions, copied from the repository, because a stub with
	// no patterns would let a routing assertion pass (or fail) for
	// reasons that have nothing to do with where the files were loaded
	// from. Provisioning copies is exactly what
	// scripts/install_runtime_assets.py does.
	repoRoot := filepath.Join("..", "..")
	for _, sub := range []string{"mode", "persona"} {
		copyTree(t, filepath.Join(repoRoot, sub), filepath.Join(sourceRoot, sub))
		copyTree(t, filepath.Join(repoRoot, sub), filepath.Join(runtimeRoot, sub))
	}
	return runtimeRoot, sourceRoot
}

// copyTree recursively copies a directory, preserving regular-file bytes.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			copyTree(t, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
	}
}

func TestModeRemoveAll_PermittedInPureRuntimeWorkspace(t *testing.T) {
	runtimeRoot, _ := newSplitWorkspace(t)
	modeDir := filepath.Join(runtimeRoot, "mode")

	mm := &ModeManager{Dir: modeDir, ActiveFile: filepath.Join(runtimeRoot, "active.json")}
	n, err := mm.RemoveAll()
	if err != nil {
		t.Fatalf("RemoveAll in a pure runtime workspace must be permitted, got: %v", err)
	}
	if n == 0 {
		t.Fatal("RemoveAll reported zero deletions in a non-empty runtime mode/ directory")
	}
	if _, err := os.Stat(modeDir); err == nil {
		entries, _ := os.ReadDir(modeDir)
		if len(entries) != 0 {
			t.Fatalf("runtime mode/ still holds %d entries after shred", len(entries))
		}
	}
}

func TestPersonaRemoveAll_PermittedInPureRuntimeWorkspace(t *testing.T) {
	runtimeRoot, _ := newSplitWorkspace(t)

	pm := &PersonaManager{
		Dir:        filepath.Join(runtimeRoot, "persona"),
		ActiveFile: filepath.Join(runtimeRoot, "active.json"),
	}
	n, err := pm.RemoveAll()
	if err != nil {
		t.Fatalf("RemoveAll in a pure runtime workspace must be permitted, got: %v", err)
	}
	if n == 0 {
		t.Fatal("RemoveAll reported zero deletions in a non-empty runtime persona/ directory")
	}
}

func TestModeRemoveAll_DoesNotTouchASeparateSourceCheckout(t *testing.T) {
	runtimeRoot, sourceRoot := newSplitWorkspace(t)
	sourceModeDir := filepath.Join(sourceRoot, "mode")

	before := snapshotDir(t, sourceModeDir)

	mm := &ModeManager{Dir: filepath.Join(runtimeRoot, "mode"), ActiveFile: filepath.Join(runtimeRoot, "active.json")}
	if _, err := mm.RemoveAll(); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	assertDirUnchanged(t, sourceModeDir, before, "source checkout mode/ must survive a runtime shred")
}

func TestPersonaRemoveAll_DoesNotTouchASeparateSourceCheckout(t *testing.T) {
	runtimeRoot, sourceRoot := newSplitWorkspace(t)
	sourcePersonaDir := filepath.Join(sourceRoot, "persona")

	before := snapshotDir(t, sourcePersonaDir)

	pm := &PersonaManager{
		Dir:        filepath.Join(runtimeRoot, "persona"),
		ActiveFile: filepath.Join(runtimeRoot, "active.json"),
	}
	if _, err := pm.RemoveAll(); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	assertDirUnchanged(t, sourcePersonaDir, before, "source checkout persona/ must survive a runtime shred")
}

func TestRuntimeDefinitionsAreTreatedAsRuntimeState(t *testing.T) {
	runtimeRoot, _ := newSplitWorkspace(t)
	modeDir := filepath.Join(runtimeRoot, "mode")

	// List must see the runtime copy. If the loader resolved anywhere
	// else it would come up empty here, which is the failure mode a
	// source/runtime split introduces.
	mm := &ModeManager{Dir: modeDir, ActiveFile: filepath.Join(runtimeRoot, "active.json")}
	modes, err := mm.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var names []string
	for _, m := range modes {
		names = append(names, m.Name)
	}
	if !containsName(names, "default") || !containsName(names, "architect") {
		t.Fatalf("runtime mode/ definitions not loaded; got %v", names)
	}
}

func TestRuntimeDefinitionListingExcludesDocumentation(t *testing.T) {
	runtimeRoot, _ := newSplitWorkspace(t)
	modeDir := filepath.Join(runtimeRoot, "mode")
	// The reconciler ships README.md alongside the definitions as
	// operator documentation for a directory the operator now owns.
	writeDefinition(t, modeDir, "README.md", "---\nname: README\npatterns: '.*'\n---\ndocs\n")

	mm := &ModeManager{Dir: modeDir, ActiveFile: filepath.Join(runtimeRoot, "active.json")}
	modes, err := mm.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, m := range modes {
		if strings.EqualFold(m.Name, "README") {
			t.Fatal("README.md became a selectable runtime mode definition")
		}
	}
}

func TestRouterLoadsDefinitionsFromPureRuntimeRoot(t *testing.T) {
	runtimeRoot, _ := newSplitWorkspace(t)

	r, err := NewRouter(runtimeRoot)
	if err != nil {
		t.Fatalf("NewRouter on a pure runtime root: %v", err)
	}
	report := r.Evaluate("please debug this failing concurrency test")
	if len(report.SelectedModes) == 0 && report.SelectedPersona == "" {
		t.Fatal("router matched nothing against provisioned runtime definitions")
	}
}

// -- helpers ---------------------------------------------------------------

func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func assertDirUnchanged(t *testing.T, dir string, before map[string]string, msg string) {
	t.Helper()
	after := snapshotDir(t, dir)
	if len(before) != len(after) {
		t.Fatalf("%s: entry count changed %d -> %d", msg, len(before), len(after))
	}
	for name, content := range before {
		got, ok := after[name]
		if !ok {
			t.Fatalf("%s: %s was deleted", msg, name)
		}
		if got != content {
			t.Fatalf("%s: %s was modified", msg, name)
		}
	}
}

func containsName(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
