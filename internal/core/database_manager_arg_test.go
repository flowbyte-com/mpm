package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewDatabaseManager_HonoursProjectRoot locks down the contract fix from
// 2026-07-20: NewDatabaseManager(projectRoot) MUST use its argument when
// non-empty, not silently fall back to config.GetMPMDir(). The legacy
// behaviour caused a 3-attempt restart loop in mpm-scheduler because the
// system-level service passed /var/lib/mpm but the constructor silently
// dropped it in favour of ~/.mpm (which the daemon user couldn't write).
//
// Regression guard: if anyone reintroduces the silent-ignore behaviour,
// this test fails with a clear message pointing at the right line.
func TestNewDatabaseManager_HonoursProjectRoot(t *testing.T) {
	tmp := t.TempDir()

	// Isolate from any ambient MPM_WORKSPACE so the env-driven fallback
	// can't accidentally point at the production workspace.
	t.Setenv("MPM_WORKSPACE", "")
	t.Setenv("HOME", tmp)

	dm, err := NewDatabaseManager(tmp)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q) failed: %v", tmp, err)
	}
	defer dm.Close()

	want := filepath.Join(tmp, "src", "db", "mpm.db")
	if dm.dbPath != want {
		t.Fatalf("NewDatabaseManager ignored projectRoot argument:\n  got  dbPath=%q\n  want dbPath=%q\n"+
			"(regression: this is the silent-ignore bug that broke systemd User=v scheduler)",
			dm.dbPath, want)
	}

	// Belt-and-braces: the test tmpdir must actually contain a DB file
	// (not just an entry in /home/$USER/.mpm).
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected DB at %q, but file missing — database was created in wrong location: %v",
			want, err)
	}

	// And — the smoking-gun check — there must be NO db file created under
	// HOME/.mpm. If this assertion fires, the constructor is back to
	// silently dropping its argument.
	rogue := filepath.Join(tmp, ".mpm", "src", "db", "mpm.db")
	if _, err := os.Stat(rogue); err == nil {
		t.Fatalf("rogue DB at %q — NewDatabaseManager fell back to ~/.mpm instead of honouring its argument",
			rogue)
	}
}

// TestNewDatabaseManager_EmptyArg_FallsBackToGetMPMDir verifies the
// preservation guarantee: callers that pass "" (most tests, ad-hoc CLI
// invocations) still get the env-driven path via config.GetMPMDir(). This
// is the backward-compat half of the contract fix.
func TestNewDatabaseManager_EmptyArg_FallsBackToGetMPMDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)

	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager(\"\") failed: %v", err)
	}
	defer dm.Close()

	want := filepath.Join(tmp, "src", "db", "mpm.db")
	if dm.dbPath != want {
		t.Fatalf("empty-arg path should honour MPM_WORKSPACE:\n  got  %q\n  want %q",
			dm.dbPath, want)
	}
}

// TestNewDatabaseManager_UnwritableHome_NoSpuriousError covers the case
// where HOME points somewhere the daemon user can't write (the exact
// symptom from the mpm-scheduler restart loop). The contract fix must
// not regress into falling back to that unwritable HOME when the caller
// passed a valid projectRoot.
func TestNewDatabaseManager_UnwritableHome_NoSpuriousError(t *testing.T) {
	tmp := t.TempDir()
	// Point HOME at a path we cannot create — simulates the production
	// permission-denied condition without needing root.
	t.Setenv("HOME", "/nonexistent-readonly-path-mpm-test")
	t.Setenv("MPM_WORKSPACE", "")

	dm, err := NewDatabaseManager(tmp)
	if err != nil {
		// The bug we're guarding against: a permission-denied error
		// mentioning the bogus HOME path. The good path is success
		// because we passed a valid projectRoot.
		if strings.Contains(err.Error(), "/nonexistent-readonly-path-mpm-test") {
			t.Fatalf("regression: NewDatabaseManager fell back to HOME instead of honouring projectRoot: %v", err)
		}
		t.Fatalf("unexpected error: %v", err)
	}
	defer dm.Close()
}