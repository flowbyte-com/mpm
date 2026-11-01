// handlers_update_test.go — contracts for the `mpm update` surface.
//
// The update command moves the installation to a newer release, so its
// failure modes are the dangerous ones: a help request must never
// change anything, an unknown flag must fail loudly, and delegation to
// the standalone updater must preserve arguments and exit codes.
//
// Hermeticity: every test runs the built binary with testenv.WithExtra,
// which pins MPM_WORKSPACE and HOME to temporary directories. The live
// installation is never touched.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runMpmUpdate(t *testing.T, bin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	return runMpmVersion(t, bin, env, args...)
}

// TestUpdateHelp_IsInert pins the help contract: --help, -h and
// `mpm help update` exit 0, print the page, and create nothing.
func TestUpdateHelp_IsInert(t *testing.T) {
	bin := buildStampedMpm(t)
	for _, args := range [][]string{
		{"update", "--help"},
		{"update", "-h"},
		{"help", "update"},
	} {
		stdout, stderr, exit := runMpmUpdate(t, bin, nil, args...)
		if exit != 0 {
			t.Errorf("mpm %s exited %d\nstdout=%s\nstderr=%s", strings.Join(args, " "), exit, stdout, stderr)
			continue
		}
		if !strings.Contains(stdout, "MPM · Update") {
			t.Errorf("mpm %s has no canonical heading:\n%s", strings.Join(args, " "), stdout)
		}
	}
}

// TestUpdateHelp_CreatesNothing verifies the help request leaves no
// release, staging, lock or history material behind.
func TestUpdateHelp_CreatesNothing(t *testing.T) {
	bin := buildStampedMpm(t)
	ws := t.TempDir()
	env := []string{"MPM_WORKSPACE=" + ws, "HOME=" + ws}
	stdout, _, exit := runMpmUpdate(t, bin, env, "update", "--help")
	if exit != 0 {
		t.Fatalf("help failed:\n%s", stdout)
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "releases" || e.Name() == "staging" || strings.HasPrefix(e.Name(), ".mpm-update") || e.Name() == "update-history.jsonl" {
			t.Errorf("help created %s", e.Name())
		}
	}
}

// TestUpdate_UnknownFlagFails pins the fail-loud contract.
func TestUpdate_UnknownFlagFails(t *testing.T) {
	bin := buildStampedMpm(t)
	stdout, stderr, exit := runMpmUpdate(t, bin, nil, "update", "--definitely-not-a-flag")
	if exit == 0 {
		t.Errorf("unknown flag exited 0:\n%s\n%s", stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, "definitely-not-a-flag") {
		t.Errorf("the rejection does not name the flag:\n%s\n%s", stdout, stderr)
	}
}

// fakeUpdateBin writes a shell script that records its argv and exits
// with a controlled code.
func fakeUpdateBin(t *testing.T, exitCode int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mpm-update")
	script := "#!/bin/sh\necho \"FAKE-UPDATE $@\"\nexit " + updateItoa(exitCode) + "\n"
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return p
}

func updateItoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// TestUpdate_DelegatesToStableBinary pins delegation: argv passes
// through and the exit code is preserved.
func TestUpdate_DelegatesToStableBinary(t *testing.T) {
	bin := buildStampedMpm(t)
	fake := fakeUpdateBin(t, 0)
	stdout, _, exit := runMpmUpdate(t, bin, []string{"MPM_UPDATE_BIN=" + fake}, "update", "--check")
	if exit != 0 {
		t.Fatalf("delegation failed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "FAKE-UPDATE --check") {
		t.Errorf("argv was not passed through:\n%s", stdout)
	}
}

// TestUpdate_DelegationPreservesExitCode pins that a failing updater is
// reported as a failure, not swallowed.
func TestUpdate_DelegationPreservesExitCode(t *testing.T) {
	bin := buildStampedMpm(t)
	fake := fakeUpdateBin(t, 3)
	_, _, exit := runMpmUpdate(t, bin, []string{"MPM_UPDATE_BIN=" + fake}, "update", "--check")
	if exit != 3 {
		t.Errorf("exit = %d, want 3", exit)
	}
}

// TestUpdate_BareDelegates pins the bare contract: `mpm update` with no
// flags runs the default update through the standalone binary.
func TestUpdate_BareDelegates(t *testing.T) {
	bin := buildStampedMpm(t)
	fake := fakeUpdateBin(t, 0)
	stdout, _, exit := runMpmUpdate(t, bin, []string{"MPM_UPDATE_BIN=" + fake}, "update")
	if exit != 0 {
		t.Fatalf("bare delegation failed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "FAKE-UPDATE") {
		t.Errorf("bare invocation was not delegated:\n%s", stdout)
	}
}

// TestUpdate_VersionRewriteBecomesTo pins the --version translation:
// the router rewrites --version to the literal "version" before
// dispatch, so the handler must translate it back to --to for the
// standalone binary.
func TestUpdate_VersionRewriteBecomesTo(t *testing.T) {
	bin := buildStampedMpm(t)
	fake := fakeUpdateBin(t, 0)
	stdout, _, exit := runMpmUpdate(t, bin, []string{"MPM_UPDATE_BIN=" + fake}, "update", "--version", "v9.9.9")
	if exit != 0 {
		t.Fatalf("delegation failed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "FAKE-UPDATE --to v9.9.9") {
		t.Errorf("--version was not translated to --to:\n%s", stdout)
	}
}

// TestUpdate_InProcessStatus pins the fallback path: with no
// standalone updater installed, --status runs in-process and reports
// valid JSON.
func TestUpdate_InProcessStatus(t *testing.T) {
	bin := buildStampedMpm(t)
	ws := t.TempDir()
	env := []string{"MPM_WORKSPACE=" + ws, "HOME=" + ws, "MPM_UPDATE_BIN=/nonexistent"}
	stdout, stderr, exit := runMpmUpdate(t, bin, env, "update", "--status", "--json")
	if exit != 0 {
		t.Fatalf("status failed:\n%s\n%s", stdout, stderr)
	}
	var got map[string]interface{}
	// The scheduler notice goes to stderr; stdout must be pure JSON.
	line := strings.TrimSpace(stdout)
	if !json.Valid([]byte(line)) {
		t.Fatalf("stdout is not valid JSON:\n%s", stdout)
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["active_rev"]; !ok {
		t.Errorf("status JSON has no active_rev:\n%s", stdout)
	}
}
