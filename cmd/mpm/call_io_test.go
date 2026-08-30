// call_io_test.go — regression tests for the alpha-4 D-004/W-004
// machine-clean-output contract.
//
// Contract:
//   - `mpm call <tool> --payload <json>` writes the result envelope to
//     stdout and routes operational INFO logs to io.Discard by default.
//   - stderr carries only true error diagnostics (no INFO/Warn noise
//     from the blob-store wiring, audit system, or migration probe).
//   - `MPM_VERBOSE=1` restores the operator-facing diagnostic stream.
//
// These tests exec the freshly-built `mpm` binary directly — the
// handler-level tests can't observe the stderr/stdout split because
// the dispatcher's stdout/stderr are process-bound.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mpmBin is the freshly-built mpm binary used by the IO tests. It is
// populated by the shared TestMain in cmd/mpm/recall_test.go (which
// builds once per package run) — building it again per-test would
// add ~10s to the suite for no benefit.
var mpmBin string

// buildMPMBinForIOTests is invoked by the package TestMain. It returns
// the path to a freshly-built mpm binary, or empty string on failure
// (the IO tests will be skipped in that case).
//
// The build runs from the parent directory of cmd/mpm/ because that's
// where `./cmd/mpm` resolves correctly under `go build`. `go test
// ./cmd/mpm/` runs with cwd = cmd/mpm, so a relative path of
// `./cmd/mpm` would resolve to cmd/mpm/cmd/mpm and fail.
func buildMPMBinForIOTests() string {
	tmp, err := os.MkdirTemp("", "mpm-call-io-bin-")
	if err != nil {
		return ""
	}
	bin := filepath.Join(tmp, "mpm")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, "./cmd/mpm")
	// chdir to the parent directory so `./cmd/mpm` resolves to the
	// cmd/mpm package directory.
	repoRoot, err := os.Getwd()
	if err != nil {
		return ""
	}
	cmd.Dir = filepath.Dir(filepath.Dir(repoRoot))
	cmd.Env = append(os.Environ(), "CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "buildMPMBinForIOTests failed: %v\n%s\n", err, out)
		return ""
	}
	return bin
}

// callMPM runs the freshly-built mpm binary with the given args and
// returns stdout, stderr, and the exit error.
func callMPM(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	if mpmBin == "" {
		t.Skip("mpm binary not built (see TestMain); skipping IO regression")
	}
	cmd := exec.Command(mpmBin, args...)
	// Build a clean env: inherit PATH and HOME-related entries from
	// os.Environ(), override HOME to a temp dir for workspace isolation,
	// set MPM_SCHEDULER_DISABLED=1 to silence the passive nudge, and
	// ensure MPM_VERBOSE is unset so the discard path is exercised.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"MPM_SCHEDULER_DISABLED=1",
	}
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err = cmd.Run()
	return so.String(), se.String(), err
}

// TestCallSuccess_StderrCleanOnHappyPath is the alpha-4 D-004 pin:
// a successful mpm_memory query must NOT leak operational INFO into
// stderr. Pre-fix this leaked "migrateLessonsToView" probe warnings
// and "alpha-3 schema fully applied" Info lines — even on a clean
// query. Post-fix stderr should be empty (or contain only true errors,
// which a successful query never has).
func TestCallSuccess_StderrCleanOnHappyPath(t *testing.T) {
	stdout, stderr, err := callMPM(t,
		"call", "mpm_memory",
		"--payload", `{"action":"query","params":{"query":"d004-clean-output","limit":3}}`,
	)
	if err != nil {
		t.Fatalf("call failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	// stdout must be valid JSON (the result envelope).
	var env map[string]interface{}
	if jerr := json.Unmarshal([]byte(stdout), &env); jerr != nil {
		t.Fatalf("stdout is not JSON: %v\nstdout=%s", jerr, stdout)
	}
	// stderr should NOT carry operational INFO from mpm itself.
	if strings.Contains(stderr, "migrateLessonsToView") {
		t.Errorf("D-004 regression: stderr leaked migration probe noise: %s", stderr)
	}
	if strings.Contains(stderr, "alpha-3 schema fully applied") {
		t.Errorf("D-004 regression: stderr leaked schema migration INFO: %s", stderr)
	}
	if strings.Contains(stderr, "artifact_provenance") {
		t.Errorf("D-004 regression: stderr leaked artifact_provenance INFO: %s", stderr)
	}
}

// TestCallValidationFailure_EnvelopeOnStdout_ExitNonZero covers the
// failure path: a payload missing required fields should produce a
// structured error envelope on stdout (parseable by machine callers)
// and exit non-zero. Stderr MAY carry a human-readable diagnostic —
// that's the one case where stderr is allowed to be non-empty.
func TestCallValidationFailure_EnvelopeOnStdout_ExitNonZero(t *testing.T) {
	stdout, _, err := callMPM(t,
		"call", "mpm_memory",
		"--payload", `{"action":"query","params":{}}`,
	)
	if err == nil {
		t.Fatalf("expected non-zero exit on empty payload, got success\nstdout=%s", stdout)
	}
	var env map[string]interface{}
	if jerr := json.Unmarshal([]byte(stdout), &env); jerr != nil {
		t.Fatalf("validation-failure stdout is not JSON: %v\nstdout=%s", jerr, stdout)
	}
	if _, has := env["error"]; !has {
		t.Errorf("validation-failure envelope missing error field: %s", stdout)
	}
}

// TestCallVerboseFlag_RestoresDiagnostics covers the MPM_VERBOSE=1
// escape hatch: with the env var set, operational INFO is restored
// to stderr so operators can debug wire-up failures. The test asserts
// the escape hatch doesn't break the happy path AND that it produces
// some diagnostic output (we expect at least the alpha-3 schema INFO
// line on a fresh DB).
func TestCallVerboseFlag_RestoresDiagnostics(t *testing.T) {
	if mpmBin == "" {
		t.Skip("mpm binary not built (see TestMain); skipping IO regression")
	}
	cmd := exec.Command(mpmBin,
		"call", "mpm_memory",
		"--payload", `{"action":"query","params":{"query":"d004-verbose","limit":3}}`,
	)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"MPM_VERBOSE=1",
		"MPM_SCHEDULER_DISABLED=1",
	}
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	if err := cmd.Run(); err != nil {
		t.Fatalf("MPM_VERBOSE=1 broke the happy path: %v\nstdout=%s\nstderr=%s", err, so.String(), se.String())
	}
	var env map[string]interface{}
	if jerr := json.Unmarshal([]byte(so.String()), &env); jerr != nil {
		t.Fatalf("stdout is not JSON under MPM_VERBOSE=1: %v\nstdout=%s", jerr, so.String())
	}
	// Under verbose mode, stderr should carry operational INFO — at
	// minimum the alpha-3 schema migration line on a fresh DB.
	if !strings.Contains(se.String(), "alpha-3 schema") {
		t.Errorf("MPM_VERBOSE=1 did not restore diagnostic stream: stderr=%s", se.String())
	}
}
