// testhelpers_workspace_guard_test.go — the test workspace guard.
//
// PinIsolatedWorkspace used to TRUST a preset MPM_WORKSPACE: a
// non-empty value was returned as-is, unexamined. That let an INHERITED
// environment variable aim a test at the operator's production
// workspace — `$HOME/.mpm`, where `active.json`, the cross-process lock,
// `toxicphrases.txt` and the database itself live.
//
// The guard now VERIFIES a preset workspace before it is used, and
// refuses one that resolves to the production workspace or to anything
// inside it.
//
// Every test here uses a DISPOSABLE fake HOME. The operator's real
// `$HOME/.mpm` is never referenced, never created and never written.

package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// p3GuardFakeHome installs a disposable fake HOME and returns it.
//
// The fake home stands in for the operator's home directory: the
// production workspace for the duration of the test is
// `<fake home>/.mpm`, which is a directory the test creates and owns.
func p3GuardFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// The ambient workspace starts unset, so each case states its own.
	t.Setenv("MPM_WORKSPACE", "")
	return home
}

// p3GuardFakeProduction creates the fake production workspace — the
// `<fake home>/.mpm` directory — and returns it.
func p3GuardFakeProduction(t *testing.T, home string) string {
	t.Helper()
	prod := filepath.Join(home, ".mpm")
	if err := os.MkdirAll(prod, 0o700); err != nil {
		t.Fatalf("create the fake production workspace: %v", err)
	}
	return prod
}

// p3GuardDirEntries lists the names of the entries directly inside dir.
func p3GuardDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestValidateTestWorkspaceRefusesTheProductionWorkspace covers the
// equality case: the workspace IS the production workspace.
func TestValidateTestWorkspaceRefusesTheProductionWorkspace(t *testing.T) {
	home := p3GuardFakeHome(t)
	prod := p3GuardFakeProduction(t, home)

	err := ValidateTestWorkspace(prod)
	if err == nil {
		t.Fatalf("the production workspace %q was accepted", prod)
	}
	if !strings.Contains(err.Error(), "production workspace") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestValidateTestWorkspaceRefusesAChildOfTheProductionWorkspace covers
// the containment case: a directory INSIDE the production workspace.
//
// A child is just as dangerous as the production workspace itself — it
// is the same directory tree, and a write into it is a write into the
// operator's installation.
func TestValidateTestWorkspaceRefusesAChildOfTheProductionWorkspace(t *testing.T) {
	home := p3GuardFakeHome(t)
	prod := p3GuardFakeProduction(t, home)

	child := filepath.Join(prod, "nested")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatalf("create the child: %v", err)
	}

	if err := ValidateTestWorkspace(child); err == nil {
		t.Fatalf("the child of the production workspace %q was accepted", child)
	}
}

// TestValidateTestWorkspaceRefusesADotDotPathIntoTheProductionWorkspace
// covers the `..` case: a path that only REACHES the production
// workspace after the segments are resolved.
//
// The comparison is made on the resolved path, so spelling the target
// differently does not get past it.
func TestValidateTestWorkspaceRefusesADotDotPathIntoTheProductionWorkspace(t *testing.T) {
	home := p3GuardFakeHome(t)
	prod := p3GuardFakeProduction(t, home)

	// <fake home>/.mpm/../.mpm — resolves to the production workspace.
	dotDot := filepath.Join(prod, "..", ".mpm")
	if err := ValidateTestWorkspace(dotDot); err == nil {
		t.Fatalf("the `..` path %q into the production workspace was accepted", dotDot)
	}

	// <fake home>/scratch/../.mmp — same target, a different spelling.
	scratch := filepath.Join(home, "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatalf("create the scratch directory: %v", err)
	}
	viaScratch := filepath.Join(scratch, "..", ".mpm")
	if err := ValidateTestWorkspace(viaScratch); err == nil {
		t.Fatalf("the `..` path %q into the production workspace was accepted", viaScratch)
	}
}

// TestValidateTestWorkspaceRefusesASymlinkIntoTheProductionWorkspace
// covers the symlink case: a path that is not the production workspace
// by name, but IS it by target.
//
// The symlink is resolved before the comparison, so an alias does not
// get past the guard.
func TestValidateTestWorkspaceRefusesASymlinkIntoTheProductionWorkspace(t *testing.T) {
	home := p3GuardFakeHome(t)
	prod := p3GuardFakeProduction(t, home)

	alias := filepath.Join(t.TempDir(), "looks-harmless")
	if err := os.Symlink(prod, alias); err != nil {
		t.Fatalf("create the symlink: %v", err)
	}
	if err := ValidateTestWorkspace(alias); err == nil {
		t.Fatalf("the symlink %q into the production workspace was accepted", alias)
	}

	// A symlink to a CHILD of the production workspace is refused too.
	child := filepath.Join(prod, "nested")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatalf("create the child: %v", err)
	}
	childAlias := filepath.Join(t.TempDir(), "child-alias")
	if err := os.Symlink(child, childAlias); err != nil {
		t.Fatalf("create the child symlink: %v", err)
	}
	if err := ValidateTestWorkspace(childAlias); err == nil {
		t.Fatalf("the symlink %q into a child of the production workspace was accepted", childAlias)
	}
}

// TestValidateTestWorkspaceRefusesAnEmptyWorkspace pins the empty case.
//
// An empty MPM_WORKSPACE means "no workspace given", which the
// production path resolves to `$HOME/.mpm`. It is therefore as unsafe
// as naming the production workspace directly.
func TestValidateTestWorkspaceRefusesAnEmptyWorkspace(t *testing.T) {
	p3GuardFakeHome(t)
	if err := ValidateTestWorkspace(""); err == nil {
		t.Fatal("the empty workspace was accepted")
	}
	if err := ValidateTestWorkspace("   "); err == nil {
		t.Fatal("the blank workspace was accepted")
	}
}

// TestValidateTestWorkspaceAcceptsASafeWorkspace pins that the guard
// does not forbid deliberate pinning: a disposable workspace is
// accepted, whether or not it exists yet.
//
// A workspace that does not exist yet is legitimate — several tests pin
// one and let the substrate create it — so the guard resolves the
// longest existing ancestor rather than refusing outright.
func TestValidateTestWorkspaceAcceptsASafeWorkspace(t *testing.T) {
	home := p3GuardFakeHome(t)

	safe := filepath.Join(t.TempDir(), "workspace")
	if err := ValidateTestWorkspace(safe); err != nil {
		t.Errorf("a safe workspace was refused: %v", err)
	}

	// A sibling whose name merely SHARES a prefix with the production
	// workspace is outside it. A string-prefix comparison would get
	// this wrong, in the dangerous direction.
	sibling := filepath.Join(home, ".mpm-backup")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatalf("create the sibling: %v", err)
	}
	if err := ValidateTestWorkspace(sibling); err != nil {
		t.Errorf("a sibling of the production workspace was refused: %v", err)
	}
}

// TestValidateTestWorkspaceHandlesRelativePaths pins that a relative
// path is resolved against the working directory before the comparison,
// so `..` cannot smuggle a path past the guard.
func TestValidateTestWorkspaceHandlesRelativePaths(t *testing.T) {
	home := p3GuardFakeHome(t)
	p3GuardFakeProduction(t, home)

	// A relative path that walks into the production workspace.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Chdir(home)
	defer t.Chdir(cwd)

	if err := ValidateTestWorkspace(".mpm"); err == nil {
		t.Fatal("the relative path \".mpm\" into the production workspace was accepted")
	}
	if err := ValidateTestWorkspace(filepath.Join("scratch", "..", ".mpm")); err == nil {
		t.Fatal("the relative path with `..` into the production workspace was accepted")
	}
}

// TestPinIsolatedWorkspacePinsADisposableWorkspaceWhenUnset pins the
// positive half: with no ambient workspace, the helper pins one the
// test owns.
func TestPinIsolatedWorkspacePinsADisposableWorkspaceWhenUnset(t *testing.T) {
	home := p3GuardFakeHome(t)
	prod := p3GuardFakeProduction(t, home)

	if err := pinIsolatedWorkspace(t); err != nil {
		t.Fatalf("pinIsolatedWorkspace: %v", err)
	}

	pinned := os.Getenv("MPM_WORKSPACE")
	if pinned == "" {
		t.Fatal("no workspace was pinned")
	}
	if pinned == prod {
		t.Fatalf("the pinned workspace is the production workspace %q", pinned)
	}
	if err := ValidateTestWorkspace(pinned); err != nil {
		t.Errorf("the pinned workspace is not safe: %v", err)
	}
	// It is a directory that exists, and it is NOT inside the fake
	// production workspace.
	if st, err := os.Stat(pinned); err != nil {
		t.Errorf("the pinned workspace does not exist: %v", err)
	} else if !st.IsDir() {
		t.Errorf("the pinned workspace is not a directory: %s", pinned)
	}
	if strings.HasPrefix(pinned, prod+string(filepath.Separator)) {
		t.Errorf("the pinned workspace %q is inside the production workspace %q", pinned, prod)
	}
}

// TestPinIsolatedWorkspaceKeepsAnExplicitlyPinnedSafeWorkspace pins
// that a deliberate, safe pin is still respected — the guard exists to
// stop an INHERITED variable from aiming a test at production, not to
// forbid deliberate pinning.
func TestPinIsolatedWorkspaceKeepsAnExplicitlyPinnedSafeWorkspace(t *testing.T) {
	home := p3GuardFakeHome(t)
	p3GuardFakeProduction(t, home)

	safe := filepath.Join(t.TempDir(), "pinned-workspace")
	t.Setenv("MPM_WORKSPACE", safe)

	if err := pinIsolatedWorkspace(t); err != nil {
		t.Fatalf("pinIsolatedWorkspace: %v", err)
	}
	if got := os.Getenv("MPM_WORKSPACE"); got != safe {
		t.Errorf("the pinned workspace was replaced: MPM_WORKSPACE = %q, want %q", got, safe)
	}
}

// TestPinIsolatedWorkspaceRefusesTheProductionWorkspace pins the
// GUARD's refusal — the decision `PinIsolatedWorkspace` makes — and
// pins that the refusal happens BEFORE anything is written.
//
// The fake production workspace must still be empty after the refusal:
// the guard refuses the workspace, it does not clean up after it.
func TestPinIsolatedWorkspaceRefusesTheProductionWorkspace(t *testing.T) {
	home := p3GuardFakeHome(t)
	prod := p3GuardFakeProduction(t, home)

	t.Setenv("MPM_WORKSPACE", prod)

	err := pinIsolatedWorkspace(t)
	if err == nil {
		t.Fatal("the guard accepted the production workspace")
	}
	if !strings.Contains(err.Error(), "production workspace") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// Nothing was created in the refused workspace.
	if names := p3GuardDirEntries(t, prod); len(names) != 0 {
		t.Errorf("the refused workspace gained %v before the refusal", names)
	}
}

// TestPinIsolatedWorkspaceRefusesBeforeAnyWorkspaceScopedFileIsCreated
// is the end-to-end shape of the defect: the guard refuses, and the
// scanner — which is what writes `toxicphrases.txt` — never runs.
//
// It pins the ordering that matters. A guard that refuses AFTER the
// workspace has been touched would already have leaked.
func TestPinIsolatedWorkspaceRefusesBeforeAnyWorkspaceScopedFileIsCreated(t *testing.T) {
	home := p3GuardFakeHome(t)
	prod := p3GuardFakeProduction(t, home)

	t.Setenv("MPM_WORKSPACE", prod)

	if err := pinIsolatedWorkspace(t); err == nil {
		t.Fatal("the guard accepted the production workspace")
	}

	// The file the scanner seeds on first use must not exist.
	phrases := filepath.Join(prod, "toxicphrases.txt")
	if _, err := os.Stat(phrases); err == nil {
		t.Errorf("the refused workspace gained %s", phrases)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected error stat %s: %v", phrases, err)
	}
	if names := p3GuardDirEntries(t, prod); len(names) != 0 {
		t.Errorf("the refused workspace gained %v before the refusal", names)
	}
}
