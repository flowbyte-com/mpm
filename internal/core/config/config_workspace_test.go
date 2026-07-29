package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGetMPMDir_DefaultsToHomeMpm locks down the post-2026-07-21
// architecture: when MPM_WORKSPACE is unset, GetMPMDir returns $HOME/.mpm.
//
// The legacy behaviour (CWD fallback) was the proximate cause of the
// ghost-DB incident at /home/v/workspace/src/db/mpm.db. A database client
// that probes $CWD creates a blank file when it guesses wrong, so the
// default must follow the user, not the directory.
func TestGetMPMDir_DefaultsToHomeMpm(t *testing.T) {
	// Save and unset env so the default branch runs.
	origWS, hadWS := os.LookupEnv("MPM_WORKSPACE")
	t.Cleanup(func() {
		if hadWS {
			_ = os.Setenv("MPM_WORKSPACE", origWS)
		} else {
			_ = os.Unsetenv("MPM_WORKSPACE")
		}
	})
	if err := os.Unsetenv("MPM_WORKSPACE"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}

	// Stage a fake HOME so we can assert the deterministic path. Save real
	// HOME so the cleanup restores it for other tests.
	origHome, hadHome := os.LookupEnv("HOME")
	t.Cleanup(func() {
		if hadHome {
			_ = os.Setenv("HOME", origHome)
		} else {
			_ = os.Unsetenv("HOME")
		}
	})
	fakeHome := t.TempDir()
	if err := os.Setenv("HOME", fakeHome); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}

	got := GetMPMDir()
	want := filepath.Join(fakeHome, ".mpm")
	if got != want {
		t.Errorf("GetMPMDir() = %q, want %q", got, want)
	}
}

// TestGetMPMDir_NoCWDProbing is the regression guard. We cd into a temp
// dir that contains a writable src/db/mpm.db-shaped decoy; the resolver
// must NOT return the CWD-relative path.
func TestGetMPMDir_NoCWDProbing(t *testing.T) {
	origWS, hadWS := os.LookupEnv("MPM_WORKSPACE")
	t.Cleanup(func() {
		if hadWS {
			_ = os.Setenv("MPM_WORKSPACE", origWS)
		} else {
			_ = os.Unsetenv("MPM_WORKSPACE")
		}
	})
	if err := os.Unsetenv("MPM_WORKSPACE"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}

	origHome, hadHome := os.LookupEnv("HOME")
	t.Cleanup(func() {
		if hadHome {
			_ = os.Setenv("HOME", origHome)
		} else {
			_ = os.Unsetenv("HOME")
		}
	})
	fakeHome := t.TempDir()
	if err := os.Setenv("HOME", fakeHome); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}

	// Stage a CWD-resident decoy that LOOKS like a valid MPM DB location.
	decoyWD := t.TempDir()
	mustMkdir(t, filepath.Join(decoyWD, "src", "db"))
	mustWriteFile(t, filepath.Join(decoyWD, "src", "db", "mpm.db"), "decoy")

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(decoyWD); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	got := GetMPMDir()
	want := filepath.Join(fakeHome, ".mpm")
	if got != want {
		t.Fatalf("GetMPMDir() returned CWD-influenced path: got %q, want %q", got, want)
	}
	// Belt-and-braces: the returned path must NOT match the decoy.
	if got == filepath.Join(decoyWD, "src", "db", "mpm.db") {
		t.Fatalf("GetMPMDir() leaked the CWD-relative decoy path")
	}
}

// TestGetWorkspace_DefaultsToHomeMpm mirrors TestGetMPMDir_DefaultsToHomeMpm
// for the workspace resolver. Same architectural invariant: home wins when
// MPM_WORKSPACE is unset.
func TestGetWorkspace_DefaultsToHomeMpm(t *testing.T) {
	origWS, hadWS := os.LookupEnv("MPM_WORKSPACE")
	t.Cleanup(func() {
		if hadWS {
			_ = os.Setenv("MPM_WORKSPACE", origWS)
		} else {
			_ = os.Unsetenv("MPM_WORKSPACE")
		}
	})
	if err := os.Unsetenv("MPM_WORKSPACE"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}

	origHome, hadHome := os.LookupEnv("HOME")
	t.Cleanup(func() {
		if hadHome {
			_ = os.Setenv("HOME", origHome)
		} else {
			_ = os.Unsetenv("HOME")
		}
	})
	fakeHome := t.TempDir()
	if err := os.Setenv("HOME", fakeHome); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}

	got := GetWorkspace()
	want := filepath.Join(fakeHome, ".mpm")
	if got != want {
		t.Errorf("GetWorkspace() = %q, want %q", got, want)
	}
}

// TestGetMPMDir_WorkspaceOverride confirms the explicit-override path is
// untouched by the home-default change. Operators must be able to point
// MPM at a non-home location (CI, container, tests, multi-tenant) by
// setting MPM_WORKSPACE — that escape hatch is load-bearing.
func TestGetMPMDir_WorkspaceOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)

	got := GetMPMDir()
	if got != tmp {
		t.Errorf("GetMPMDir() = %q, want %q (MPM_WORKSPACE override)", got, tmp)
	}
}

// helpers — duplicated locally to avoid pulling in test helpers across
// packages. Keep in lockstep with cmd/mpm route_render_test.go helpers.
func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
