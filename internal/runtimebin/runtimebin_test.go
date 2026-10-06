package runtimebin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A non-standard PREFIX install is the reason sibling resolution exists at
// all: `make install PREFIX=/opt/mpm` puts mpm, mpm-critic and mpm-telemetry
// side by side somewhere other than ~/.mpm/bin. These tests use a fake prefix
// under t.TempDir() and set HOME to an unrelated empty directory, so nothing
// here can pass by accident on a ~/.mpm layout.

func writeFakeBinary(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// fakePrefix builds /tmp/.../custom/bin/{mpm,mpm-critic,mpm-telemetry} and
// returns the bin directory.
func fakePrefix(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "custom", "bin")
	for _, name := range []string{"mpm", "mpm-critic", "mpm-telemetry"} {
		writeFakeBinary(t, filepath.Join(bin, name), 0o755)
	}
	return bin
}

func TestResolve_SiblingInCustomPrefix(t *testing.T) {
	bin := fakePrefix(t)
	// HOME deliberately points at an empty directory: no ~/.mpm layout exists.
	t.Setenv("HOME", t.TempDir())
	t.Setenv(BinEnv, "")

	for _, self := range []string{
		filepath.Join(bin, "mpm-critic"),
		filepath.Join(bin, "mpm-telemetry"),
	} {
		got, err := (&Resolver{SelfPath: self}).Resolve()
		if err != nil {
			t.Fatalf("Resolve from %s: %v", self, err)
		}
		want := filepath.Join(bin, "mpm")
		if got != want {
			t.Errorf("Resolve from %s = %q, want %q", self, got, want)
		}
	}
}

// TestResolve_SiblingFollowsSymlinkedPrefix covers an install reached through
// a symlinked bin directory, which is common under versioned prefixes.
func TestResolve_SiblingFollowsSymlinkedPrefix(t *testing.T) {
	bin := fakePrefix(t)
	link := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(bin, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv(BinEnv, "")

	self := filepath.Join(link, "mpm-critic")
	got, err := (&Resolver{SelfPath: self}).Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if filepath.Base(got) != "mpm" {
		t.Errorf("Resolve = %q, want a sibling named mpm", got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Resolve = %q, want an absolute path", got)
	}
}

// TestResolve_Precedence pins the documented order: explicit beats MPM_BIN,
// which beats sibling.
func TestResolve_Precedence(t *testing.T) {
	bin := fakePrefix(t)
	envBin := writeFakeBinary(t, filepath.Join(t.TempDir(), "from-env", "mpm"), 0o755)
	explicit := writeFakeBinary(t, filepath.Join(t.TempDir(), "from-flag", "mpm"), 0o755)
	self := filepath.Join(bin, "mpm-critic")

	got, err := (&Resolver{Explicit: explicit, SelfPath: self, Env: []string{BinEnv + "=" + envBin}}).Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != explicit {
		t.Errorf("Resolve = %q, want the explicit path %q", got, explicit)
	}

	got, err = (&Resolver{SelfPath: self, Env: []string{BinEnv + "=" + envBin}}).Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != envBin {
		t.Errorf("Resolve = %q, want MPM_BIN %q", got, envBin)
	}

	got, err = (&Resolver{SelfPath: self, Env: []string{BinEnv + "="}}).Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(bin, "mpm"); got != want {
		t.Errorf("Resolve = %q, want the sibling %q", got, want)
	}
}

// TestResolve_ConfiguredCandidateFailureDoesNotFallThrough pins that a
// configured-but-broken binary is an error rather than a reason to quietly
// substitute a different one. An operator who named a binary and got it wrong
// needs to find out.
func TestResolve_ConfiguredCandidateFailureDoesNotFallThrough(t *testing.T) {
	bin := fakePrefix(t)
	self := filepath.Join(bin, "mpm-critic")

	missing := filepath.Join(t.TempDir(), "gone", "mpm")
	_, err := (&Resolver{SelfPath: self, Env: []string{BinEnv + "=" + missing}}).Resolve()
	if err == nil {
		t.Fatal("Resolve succeeded with a nonexistent MPM_BIN")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name %q", err, missing)
	}
}

// TestResolve_NonExecutableSiblingIsNotUsed pins that an unusable sibling
// does not get executed, and that the failure is reported rather than
// silently degrading to something else.
func TestResolve_NonExecutableSiblingIsNotUsed(t *testing.T) {
	dir := t.TempDir()
	writeFakeBinary(t, filepath.Join(dir, "mpm"), 0o644) // not +x
	self := writeFakeBinary(t, filepath.Join(dir, "mpm-critic"), 0o755)

	t.Setenv(BinEnv, "")
	got, err := (&Resolver{SelfPath: self}).Resolve()
	if err == nil {
		t.Fatalf("Resolve = %q, want an error for a non-executable sibling", got)
	}
	if !strings.Contains(err.Error(), BinEnv) {
		t.Errorf("error = %v, want it to name %s as the fix", err, BinEnv)
	}
}

func TestResolve_RejectsBareName(t *testing.T) {
	t.Setenv(BinEnv, "")
	_, err := (&Resolver{Explicit: BinName}).Resolve()
	if err == nil {
		t.Fatal("Resolve accepted the bare name \"mpm\"")
	}
	if !strings.Contains(err.Error(), "bare name") {
		t.Errorf("error = %v, want it to explain that a bare name defers to PATH", err)
	}
}

func TestResolve_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := (&Resolver{Explicit: dir}).Resolve()
	if err == nil {
		t.Fatal("Resolve accepted a directory")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error = %v, want it to say the path is a directory", err)
	}
}

// TestResolve_FailureMessageNamesEveryOption keeps a misconfiguration
// actionable: the error must say what was tried and what to set.
func TestResolve_FailureMessageNamesEveryOption(t *testing.T) {
	dir := t.TempDir()
	self := writeFakeBinary(t, filepath.Join(dir, "mpm-critic"), 0o755)

	_, err := (&Resolver{SelfPath: self, Env: []string{}}).Resolve()
	if err == nil {
		t.Fatal("Resolve succeeded with no candidates")
	}
	for _, want := range []string{BinEnv, "--mpm", "PATH"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}
