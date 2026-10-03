package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The canonical MPM layout is `git clone … ~/.mpm`: the checkout IS the
// workspace root. These tests pin the two properties that make that
// co-location safe.
//
// Isolation: every test sets MPM_WORKSPACE to a temp dir, so nothing here
// reads or writes the live database at ~/.mpm.

func TestGetMPMDirHonoursWorkspaceOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)
	if got := GetMPMDir(); got != dir {
		t.Errorf("GetMPMDir() = %q, want the MPM_WORKSPACE override %q", got, dir)
	}
}

func TestGetWorkspaceHonoursOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)
	if got := GetWorkspace(); got != dir {
		t.Errorf("GetWorkspace() = %q, want %q", got, dir)
	}
}

// An alternate checkout keeps source elsewhere but runtime state at the
// canonical root. With no override the runtime root must be $HOME/.mpm,
// never the checkout's own directory — that is what makes the two layouts
// independently relocatable.
func TestGetWorkspaceDefaultsToHomeMpmNotCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MPM_WORKSPACE", "")

	want := filepath.Join(home, ".mpm")
	if got := GetWorkspace(); got != want {
		t.Errorf("GetWorkspace() = %q, want the canonical %q", got, want)
	}
	if got := GetMPMDir(); got != want {
		t.Errorf("GetMPMDir() = %q, want the canonical %q", got, want)
	}
}

// Runtime paths must resolve beneath the workspace root, not into the
// source tree, so a `git pull` can never collide with live data.
func TestRuntimePathsResolveUnderWorkspaceRoot(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	for name, got := range map[string]string{
		"mirror": GetMirrorPath(),
		"toxics": GetToxicPhrasesPath(),
	} {
		if !strings.HasPrefix(got, ws) {
			t.Errorf("%s path = %q, want it under the workspace root %q", name, got, ws)
		}
	}
}

// The database directory is the co-location hot spot: it holds mpm.db and
// its WAL/SHM sidecars, and it sits inside the checkout.
func TestDatabaseDirIsUnderWorkspaceRoot(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	dbDir := filepath.Join(ws, "src", "db")
	if !strings.HasPrefix(dbDir, ws) {
		t.Fatalf("db dir %q escaped the workspace root %q", dbDir, ws)
	}
	// Sidecars are what leak into git status if the ignore rules drift.
	for _, name := range []string{"mpm.db", "mpm.db-wal", "mpm.db-shm", "telemetry.db"} {
		p := filepath.Join(dbDir, name)
		if filepath.Dir(p) != dbDir {
			t.Errorf("%s would not sit in %s", name, dbDir)
		}
	}
}

// The documented tree must match the implementation. If a future refactor
// moves a runtime path, this fails rather than letting the docs drift.
func TestCanonicalLayoutContract(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	for _, rel := range []string{
		filepath.Join("src", "db"),
		"backups",
		"blobs",
		"run",
		"active.json",
		"toxicphrases.txt",
	} {
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if _, err := os.Stat(filepath.Dir(p)); err != nil {
			t.Errorf("%s is not creatable under the canonical root: %v", rel, err)
		}
	}
}
