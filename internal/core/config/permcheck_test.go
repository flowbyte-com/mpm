// permcheck_test.go — pin the auto-heal contract: 0700 (no-op),
// 0755 (auto-heal succeeds), too-wide + non-owned (returns
// ErrDirPermsTooWide), missing path, file-not-directory.
//
// The "non-owned" case requires root to chown a test dir to a
// different user; that's not practical in CI. The chmod attempt
// itself doubles as the ownership check, so the test for that path
// lives in code review rather than in the test suite.

package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestAssertUserDirPerms0700_NonExistentPath(t *testing.T) {
	err := AssertUserDirPerms0700("/nonexistent/path/that/should/not/exist/mpm-permcheck-xyz")
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
	if !strings.Contains(err.Error(), "stat mpm dir") {
		t.Errorf("error should mention stat failure; got %v", err)
	}
	// Should NOT be the perms-too-wide error — the path just doesn't exist.
	if errors.Is(err, ErrDirPermsTooWide) {
		t.Errorf("nonexistent path shouldn't surface as perms-too-wide; got %v", err)
	}
}

func TestAssertUserDirPerms0700_PathIsFile(t *testing.T) {
	f, err := os.CreateTemp("", "mpm-permcheck-*")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer os.Remove(f.Name())
	f.Close()

	err = AssertUserDirPerms0700(f.Name())
	if err == nil {
		t.Fatal("expected error for file path passed as dir")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error should mention not-a-directory; got %v", err)
	}
}

func TestAssertUserDirPerms0700_Already0700(t *testing.T) {
	d := t.TempDir()
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatalf("chmod setup: %v", err)
	}

	if err := AssertUserDirPerms0700(d); err != nil {
		t.Errorf("expected nil for compliant dir, got %v", err)
	}
}

func TestAssertUserDirPerms0700_StricterThan0700(t *testing.T) {
	// 0o600 / 0o500 on a directory would be unusual, but the
	// contract is "no wider than 0700" — anything tighter is fine.
	d := t.TempDir()
	// 0o500 is r-x for owner only. If the fs doesn't support it
	// (some don't allow stripping execute from a dir you own),
	// skip rather than fail.
	if err := os.Chmod(d, 0o500); err != nil {
		t.Skipf("can't chmod dir to 0500 on this fs: %v", err)
	}
	if err := AssertUserDirPerms0700(d); err != nil {
		t.Errorf("tighter-than-0700 dir should be considered compliant, got %v", err)
	}
}

func TestAssertUserDirPerms0700_AutoHealSucceeds(t *testing.T) {
	d := t.TempDir()
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatalf("chmod setup: %v", err)
	}

	if err := AssertUserDirPerms0700(d); err != nil {
		t.Fatalf("auto-heal should have succeeded (we own the tempdir); got %v", err)
	}

	info, err := os.Stat(d)
	if err != nil {
		t.Fatalf("stat after heal: %v", err)
	}
	got := info.Mode().Perm()
	if got > 0o700 {
		t.Errorf("perms after auto-heal: want <= 0700, got %04o", got)
	}
}

func TestAssertUserDirPerms0700_AutoHealVariousWidenedModes(t *testing.T) {
	// 0755, 0775, 0777 — all should auto-heal to 0700.
	for _, start := range []os.FileMode{0o755, 0o775, 0o777} {
		t.Run("", func(t *testing.T) {
			d := t.TempDir()
			if err := os.Chmod(d, start); err != nil {
				t.Fatalf("chmod setup to %04o: %v", start, err)
			}
			if err := AssertUserDirPerms0700(d); err != nil {
				t.Fatalf("auto-heal from %04o should have succeeded; got %v", start, err)
			}
			info, _ := os.Stat(d)
			if info.Mode().Perm() > 0o700 {
				t.Errorf("perms after heal from %04o: got %04o", start, info.Mode().Perm())
			}
		})
	}
}