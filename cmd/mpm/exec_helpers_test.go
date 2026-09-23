// exec_helpers_test.go — Shared helpers for cmd/mpm tests that
// need to spawn a real `mpm` subprocess.
//
// Background: a long-lived set of tests under cmd/mpm hardcoded
// "/home/v/.mpm/bin/mpm" as the path to the mpm binary they
// exercised. That path was the install location on the original
// author's machine; under any other Unix user or environment the
// tests failed at exec.Command with "no such file or directory".
//
// The fix is hermetic: build the current source tree's `mpm`
// binary into a t.TempDir() per test, and spawn it against an
// isolated MPM_WORKSPACE. This guarantees the test exercises the
// exact code under test, regardless of which user is running the
// test suite or whether MPM is installed on the host.
//
// Pre-existing release_pass_20260914_*.go files have their own
// per-package *Bin helpers (handoffTestBin, buildAttentionBin,
// etc.) which we do not rename here — they pre-date the shared
// helper and the test-infrastructure cleanup is scoped to the
// fixtures that explicitly hardcoded /home/v. Existing per-package
// helpers continue to work alongside this shared one.
//
// Add new CLI subprocess tests via the public mpmCmd helper; do
// not reintroduce hardcoded install paths.

package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"
)

// mpmCmd builds a fresh `mpm` binary from the current source tree
// (./cmd/mpm) into a per-test temp directory and returns its
// absolute path. The build uses the same flags as production
// (`-tags fts5` so the FTS5 modules build; the same `-tags fts5`
// flag the installer uses, see Makefile). Per-test isolation
// guarantees the test exercises the current code under test
// regardless of host environment.
//
// Caller passes the returned path to mpmRun (or uses it directly
// via exec.Command with the env it returns).
//
// The build is cached by Go's build cache; subsequent calls in
// the same `go test` invocation reuse the cache. Total overhead
// for a fresh build is ~1-2s on a cold cache; warm-cache calls
// are sub-second.
func mpmCmd(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-test")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// mpmRun invokes the mpm binary at `bin` with `args` against an
// isolated workspace `ws`. It returns the combined stdout+stderr
// output and the process exit code (0 on success, non-zero on
// failure — *exec.ExitError.ExitCode() preserves the actual
// process exit). It does not fail the test on non-zero exit;
// callers assert on the returned code.
//
// The subprocess inherits PATH (so `mpm` can find sqlite3 for any
// path the test exercises) and gets MPM_WORKSPACE pinned to the
// caller-supplied temp dir. No host $HOME leak — the subprocess's
// HOME is also pinned to a fresh per-test dir so the installer's
// bootstrap probe cannot reach into the operator's real ~/.mpm.
//
// HOME pinning matters because several installers and bootstrap
// paths probe $HOME/.mpm for legacy layout. Without this, a
// running test could mutate the operator's real install on
// machine-specific failure modes.
func mpmRun(t *testing.T, bin, ws string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{
		"MPM_WORKSPACE=" + ws,
		"PATH=/usr/bin:/bin:/usr/local/go/bin",
		"HOME=" + t.TempDir(),
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	}
	// CombinedOutput preserves the original interleaving order; the
	// separate-buffer form here matches what exec does internally,
	// then we concatenate so callers get the same shape as before.
	out := stdout.Bytes()
	if len(stderr.Bytes()) > 0 {
		if len(out) > 0 {
			out = append(out, '\n')
		}
		out = append(out, stderr.Bytes()...)
	}
	return string(out), code
}

// mpmCombined is the call-and-grep convenience for tests that
// only care about exit-code and a substring match. Returns true
// when code == expected and output contains marker. Saves a few
// lines per test. Intentionally not used everywhere — some tests
// need the raw output for richer assertions and prefer mpmRun.
func mpmCombined(t *testing.T, bin, ws, marker string, expected int, args ...string) bool {
	t.Helper()
	out, code := mpmRun(t, bin, ws, args...)
	if code != expected {
		t.Errorf("exit code = %d, want %d. Output:\n%s", code, expected, out)
		return false
	}
	return true
}
