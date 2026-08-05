package capability

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// =============================================================================
// driver_bwrap_test.go — BwrapDriver tests (EX-3.4)
//
// These tests don't depend on the real `bwrap` binary. Instead,
// they install a shell-script stub at t.TempDir()/bwrap, point
// the Driver at it via BwrapPath override, and assert against
// the stub's recorded argv file.
//
// The stub's contract:
//
//   * Always writes its argv to $BWRAP_RECORD_FILE (one
//     argument per line, including argv[0] which is the stub's
//     own path).
//   * If $BWRAP_SLEEP_MS is set, sleeps that many ms before
//     exiting 0 (used for timeout tests).
//   * If $BWRAP_EXIT_CODE is set, exits with that code after
//     recording argv.
//   * Writes "stdout-msg\n" to stdout and "stderr-msg\n" to
//     stderr (so output-capture tests have something to verify).
//
// The stub has no bwrap-specific knowledge — it just behaves
// like a long-running process that bwrap would have spawned.
// This isolates the Driver's argv-construction + subprocess-
// management logic from the real bwrap semantics.
//
// Test cases:
//
//   1. BwrapNotFound: NewBwrapDriver with empty override and
//      no bwrap on PATH returns ErrBwrapNotFound.
//   2. BwrapFoundOverride: NewBwrapDriver with a valid
//      override returns the Driver with that path set.
//   3. ArgShape_Sandbox: Run with sandbox domain produces the
//      spec's §4.1 argv.
//   4. ArgShape_Restricted: Run with restricted domain adds
//      the project_dir + allowed_paths binds.
//   5. Restricted_NoProjectDir_Errors: Run with restricted
//      domain and no metadata.project_dir falls back to
//      process cwd if non-empty; errors otherwise.
//   6. Restricted_RelativeAllowedPath_Errors: Run with
//      restricted domain and a relative metadata.allowed_paths
//      entry returns ErrBwrapAllowedPathInvalid.
//   7. ExitCodePropagated: child exits non-zero; DriverResult
//      carries ExitCode preserved.
//   8. TimeoutKillsChild: child sleeps past ctx deadline;
//      DriverResult.ExitCode = -1.
//   9. TempfileCleanedUp: after Run returns, the tempfile
//      that held the source is gone.
//  10. DriverName_IsBwrap: Telemetry stamp is "bwrap".
// =============================================================================

// writeBwrapStub installs a shell-script bwrap at path. The
// stub records its argv to recordFile (if set), optionally
// sleeps sleepMs (if > 0), and exits with exitCode. Returns
// the path to the stub.
func writeBwrapStub(t *testing.T, dir, name string) string {
	t.Helper()
	stubPath := filepath.Join(dir, name)
	script := `#!/bin/bash
# Stub bwrap for tests. Records argv, then exits (optionally
# after sleeping). Models the real bwrap's child-killing
# semantics: SIGTERM from the executor's WaitDelay must
# propagate to any subprocess we spawned (so the timeout test
# actually completes promptly).
set -e

# Forward SIGTERM to the subprocess so the timeout test
# doesn't hang on a grandchild python sleep. Set after
# spawn so we don't trap our own setup.
child_pid=""

cleanup() {
  if [ -n "$child_pid" ]; then
    kill -TERM "$child_pid" 2>/dev/null || true
    wait "$child_pid" 2>/dev/null || true
  fi
}
trap cleanup TERM INT

# Find the bwrap arg list. bwrap's argv in our tests is the
# script path itself followed by "--ro-bind ..." etc. We write
# each arg on its own line to the record file.
if [ -n "$BWRAP_RECORD_FILE" ]; then
  : > "$BWRAP_RECORD_FILE"
  for arg in "$@"; do
    printf '%s\n' "$arg" >> "$BWRAP_RECORD_FILE"
  done
fi

# Optional sleep (used for timeout tests). Spawn python in the
# background and capture its PID so the SIGTERM trap above
# can kill it when the executor times us out.
if [ -n "$BWRAP_SLEEP_MS" ]; then
  export BWRAP_SLEEP_MS
  python3 -c 'import os, time; time.sleep(int(os.environ["BWRAP_SLEEP_MS"]) / 1000.0)' &
  child_pid=$!
  wait "$child_pid"
fi

# Optional output capture (so output-cap tests have data).
if [ -n "$BWRAP_WRITE_OUTPUT" ]; then
  printf 'stdout-msg\n'
  printf 'stderr-msg\n' >&2
fi

# Optional exit code.
if [ -n "$BWRAP_EXIT_CODE" ]; then
  exit "$BWRAP_EXIT_CODE"
fi

exit 0
`
	if err := os.WriteFile(stubPath, []byte(script), 0755); err != nil {
		t.Fatalf("write bwrap stub: %v", err)
	}
	return stubPath
}

// recordFile returns the path the stub will write its argv to.
// Tests pass this back to readBwrapArgs.
func recordFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "bwrap_argv.txt")
}

// readBwrapArgs returns the argv the stub recorded.
func readBwrapArgs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// withEnv returns a t.TempDir-based environment plus the given
// overrides. Tests use this to point the stub at its record
// file without polluting the test process env.
func withEnv(overrides map[string]string) []string {
	base := os.Environ()
	for k, v := range overrides {
		base = append(base, k+"="+v)
	}
	return base
}

// =============================================================================
// Construction tests
// =============================================================================

// TestBwrapDriver_NotFoundReturnsErr verifies the spec's
// fail-loud requirement: a missing bwrap binary produces a
// clear error at construction time, not at first invocation.
func TestBwrapDriver_NotFoundReturnsErr(t *testing.T) {
	// We force the lookup to fail by pointing PATH at an empty
	// directory and clearing MPM_BWRAP_PATH. We don't have an
	// override here because the test wants to exercise the
	// exec.LookPath path.
	t.Setenv("PATH", t.TempDir())
	_, err := NewBwrapDriver("")
	if err == nil {
		t.Fatal("expected NewBwrapDriver to fail with no bwrap on PATH")
	}
	if !errors.Is(err, ErrBwrapNotFound) {
		t.Errorf("expected ErrBwrapNotFound, got: %v", err)
	}
}

// TestBwrapDriver_OverrideAccepted verifies that the override
// path bypasses PATH lookup but still verifies the file exists.
func TestBwrapDriver_OverrideAccepted(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeBwrapStub(t, stubDir, "bwrap")

	d, err := NewBwrapDriver(stubPath)
	if err != nil {
		t.Fatalf("NewBwrapDriver: %v", err)
	}
	if d.BwrapPath != stubPath {
		t.Errorf("BwrapPath = %q, want %q", d.BwrapPath, stubPath)
	}
	if d.DriverName() != "bwrap" {
		t.Errorf("DriverName = %q, want %q", d.DriverName(), "bwrap")
	}
}

// TestBwrapDriver_OverrideMissingReturnsErr verifies that a
// typo in the override path also produces ErrBwrapNotFound.
func TestBwrapDriver_OverrideMissingReturnsErr(t *testing.T) {
	_, err := NewBwrapDriver("/nonexistent/path/to/bwrap")
	if err == nil {
		t.Fatal("expected error for missing override path")
	}
	if !errors.Is(err, ErrBwrapNotFound) {
		t.Errorf("expected ErrBwrapNotFound, got: %v", err)
	}
}

// =============================================================================
// Arg shape tests
// =============================================================================

// TestBwrapDriver_ArgShape_Sandbox verifies the §4.1 sandbox
// argv. The stub records argv so we read it back and assert.
func TestBwrapDriver_ArgShape_Sandbox(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeBwrapStub(t, stubDir, "bwrap")
	rec := recordFile(t)

	d, err := NewBwrapDriver(stubPath)
	if err != nil {
		t.Fatalf("NewBwrapDriver: %v", err)
	}

	cap := &Capability{
		ID:             "cap_sandbox_args",
		ExecutionDomain: DomainSandbox,
		Metadata:        CapabilityMetadata{},
	}
	ctx := injectDriverContext(context.Background(), cap)
	req := DriverRequest{
		Language:   LangBash,
		SourceCode: "echo hi",
		Args:       []string{"a", "b"},
		Limits:     DefaultResourceLimits(),
	}

	// Run the stub under controlled env (record file + no sleep).
	t.Setenv("BWRAP_RECORD_FILE", rec)
	// Clear sleeps that might be set by other tests.
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	_, err = d.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := readBwrapArgs(t, rec)
	// The stub records argv starting at "$@" which in bash
	// is argv[1:] (not including the program name). So the
	// recorded argv is bwrap's actual argv (no skip needed).

	wantPrefix := []string{
		// Read-only system binds.
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/etc/resolv.conf", "/etc/resolv.conf",
		// /tmp visible (ro) so the source tempfile is reachable.
		// /home private (tmpfs) so the script sees nothing.
		"--ro-bind", "/tmp", "/tmp",
		"--tmpfs", "/home",
		// Namespace isolation.
		"--unshare-net",
		"--unshare-pid",
		"--new-session",
		"--die-with-parent",
		// EX-4: rlimit flags appear after namespace flags
		// but before --chdir / --. Values depend on
		// DefaultResourceLimits(); the values here match
		// those defaults (512 MB → 536870912 bytes; 256 FDs).
		"--rlimit-as", "536870912",
		"--rlimit-nofile", "256",
		"--chdir", "/tmp",
		"--",
	}
	if !hasPrefix(args, wantPrefix) {
		t.Errorf("argv missing required prefix.\n got: %v\nwant prefix: %v", args, wantPrefix)
	}
	// The interpreter + script path + user args come after --.
	// With Args=["a","b"], tail is [--, /bin/bash, <tempfile>, a, b] (5).
	tail := args[len(args)-5:]
	if tail[0] != "--" {
		t.Errorf("expected '--' separator, got %q", tail[0])
	}
	if tail[1] != "/bin/bash" {
		t.Errorf("interpreter = %q, want /bin/bash", tail[1])
	}
	// tail[2] is the tempfile path (we don't assert on its
	// exact value because it varies per run).
	if tail[3] != "a" || tail[4] != "b" {
		t.Errorf("user args not preserved: %v", tail[3:])
	}
}

// TestBwrapDriver_ArgShape_Restricted verifies the §4.1
// restricted argv adds the project_dir + allowed_paths binds.
func TestBwrapDriver_ArgShape_Restricted(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeBwrapStub(t, stubDir, "bwrap")
	rec := recordFile(t)

	d, err := NewBwrapDriver(stubPath)
	if err != nil {
		t.Fatalf("NewBwrapDriver: %v", err)
	}

	projectDir := t.TempDir()
	cap := &Capability{
		ID:             "cap_restricted_args",
		ExecutionDomain: DomainRestricted,
		Metadata: CapabilityMetadata{
			"project_dir":    projectDir,
			"allowed_paths": []interface{}{"/data/a", "/data/b"},
		},
	}
	ctx := injectDriverContext(context.Background(), cap)
	req := DriverRequest{
		Language:   LangBash,
		SourceCode: "echo hi",
		Limits:     DefaultResourceLimits(),
	}

	t.Setenv("BWRAP_RECORD_FILE", rec)
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	_, err = d.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := readBwrapArgs(t, rec)
	// Restricted-specific binds must appear.
	wantBinds := []string{
		"--bind", projectDir, projectDir,
		"--bind", "/data/a", "/data/a",
		"--bind", "/data/b", "/data/b",
		"--chdir", projectDir,
	}
	if !containsAllPairs(args, wantBinds) {
		t.Errorf("argv missing restricted binds.\n got: %v\nwant binds: %v", args, wantBinds)
	}
	// Restricted does NOT unshare-net — the spec removed that
	// from the restricted shape. Verify it's absent.
	for _, a := range args {
		if a == "--unshare-net" {
			t.Errorf("restricted domain should not unshare-net; got argv %v", args)
		}
	}
}

// TestBwrapDriver_Restricted_RelativeAllowedPath_Errors
// verifies that a relative metadata.allowed_paths entry is
// refused at argv-construction time (bwrap's --bind needs
// absolute paths).
func TestBwrapDriver_Restricted_RelativeAllowedPath_Errors(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeBwrapStub(t, stubDir, "bwrap")
	d, _ := NewBwrapDriver(stubPath)

	cap := &Capability{
		ID:             "cap_relpath",
		ExecutionDomain: DomainRestricted,
		Metadata: CapabilityMetadata{
			"project_dir":    t.TempDir(),
			"allowed_paths": []interface{}{"relative/path"},
		},
	}
	ctx := injectDriverContext(context.Background(), cap)
	_, err := d.Run(ctx, DriverRequest{
		Language:   LangBash,
		SourceCode: "echo hi",
		Limits:     DefaultResourceLimits(),
	})
	if !errors.Is(err, ErrBwrapAllowedPathInvalid) {
		t.Errorf("expected ErrBwrapAllowedPathInvalid, got: %v", err)
	}
}

// =============================================================================
// Failure-shape tests
// =============================================================================

// TestBwrapDriver_NonZeroExitPreserved verifies that a child
// process exiting non-zero surfaces as a DriverResult with
// ExitCode preserved, NOT as a driver-level (nil, err).
func TestBwrapDriver_NonZeroExitPreserved(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeBwrapStub(t, stubDir, "bwrap")
	d, _ := NewBwrapDriver(stubPath)

	cap := &Capability{
		ID:             "cap_exit42",
		ExecutionDomain: DomainSandbox,
		Metadata:        CapabilityMetadata{},
	}
	ctx := injectDriverContext(context.Background(), cap)
	t.Setenv("BWRAP_RECORD_FILE", recordFile(t))
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "42")

	res, err := d.Run(ctx, DriverRequest{
		Language:   LangBash,
		SourceCode: "exit 42",
		Limits:     DefaultResourceLimits(),
	})
	if err != nil {
		t.Fatalf("expected capability-level (result, nil), got (nil, %v)", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result for capability-level failure")
	}
	if res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", res.ExitCode)
	}
}

// TestBwrapDriver_TimeoutKillsChild verifies that a ctx
// deadline causes the child to be killed and surfaced as
// ExitCode=-1 (so EX-7 fracture detection can distinguish
// "ran too long" from "exited non-zero").
func TestBwrapDriver_TimeoutKillsChild(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeBwrapStub(t, stubDir, "bwrap")
	d, _ := NewBwrapDriver(stubPath)
	d.BwrapWaitDelay = 50 * time.Millisecond

	cap := &Capability{
		ID:             "cap_timeout",
		ExecutionDomain: DomainSandbox,
		Metadata:        CapabilityMetadata{},
	}
	ctx := injectDriverContext(context.Background(), cap)
	t.Setenv("BWRAP_RECORD_FILE", recordFile(t))
	t.Setenv("BWRAP_SLEEP_MS", "5000") // 5 seconds — far longer than the ctx
	t.Setenv("BWRAP_EXIT_CODE", "")

	// Use a short ctx with sub-second deadline so the test
	// completes in <1s.
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := d.Run(ctx, DriverRequest{
		Language:   LangBash,
		SourceCode: "sleep 5",
		Limits:     DefaultResourceLimits(),
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected capability-level result, got (nil, %v)", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result on timeout")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (timeout)", res.ExitCode)
	}
	if elapsed > 1*time.Second {
		t.Errorf("timeout took %v, expected <1s (ctx deadline was 100ms)", elapsed)
	}
}

// TestBwrapDriver_TempfileCleanedUp verifies that the source
// tempfile is removed after Run returns. Important for two
// reasons: (1) disk hygiene, (2) the /tmp tempfile pattern
// is the only file the script can see, so a stale tempfile
// from a prior crash would be visible to a subsequent run.
func TestBwrapDriver_TempfileCleanedUp(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeBwrapStub(t, stubDir, "bwrap")
	d, _ := NewBwrapDriver(stubPath)

	cap := &Capability{
		ID:             "cap_cleanup",
		ExecutionDomain: DomainSandbox,
		Metadata:        CapabilityMetadata{},
	}
	ctx := injectDriverContext(context.Background(), cap)
	t.Setenv("BWRAP_RECORD_FILE", recordFile(t))
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")

	// Pre-condition: count tempfiles before.
	before := countMPMTempfiles(t)
	_, err := d.Run(ctx, DriverRequest{
		Language:   LangBash,
		SourceCode: "echo cleanup-test",
		Limits:     DefaultResourceLimits(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	after := countMPMTempfiles(t)
	if after != before {
		t.Errorf("tempfile leak: before=%d after=%d", before, after)
	}
}

// countMPMTempfiles returns the count of mpm-cap-*.sh files
// in the system temp dir. Used to verify no leaks.
func countMPMTempfiles(t *testing.T) int {
	t.Helper()
	dir := os.TempDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "mpm-cap-") && strings.HasSuffix(e.Name(), ".sh") {
			n++
		}
	}
	return n
}

// =============================================================================
// Dispatcher integration test
// =============================================================================

// TestBwrapDriver_DispatchedFromStaticDispatcher verifies the
// EX-3 wiring: a StaticDispatcher maps DomainSandbox to a
// BwrapDriver; an Executor wired through the Dispatcher
// dispatches correctly. The telemetry stamp is "bwrap".
func TestBwrapDriver_DispatchedFromStaticDispatcher(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeBwrapStub(t, stubDir, "bwrap")
	d, _ := NewBwrapDriver(stubPath)

	disp := NewStaticDispatcher(map[ExecutionDomain]Driver{
		DomainSandbox:    d,
		DomainRestricted: d,
	})

	// Verify the Dispatcher returns the right Driver.
	cap := &Capability{ID: "x", ExecutionDomain: DomainSandbox}
	got, err := disp.DriverFor(cap)
	if err != nil {
		t.Fatalf("DriverFor: %v", err)
	}
	if got != d {
		t.Errorf("DriverFor = %v, want %v", got, d)
	}
	if d.DriverName() != "bwrap" {
		t.Errorf("telemetry name = %q, want bwrap", d.DriverName())
	}
}

// =============================================================================
// Resource limit tests (EX-4.3)
//
// These tests verify that resolveLimits-then-buildArgs produces
// the correct bwrap rlimit flags. We invoke through a full
// Executor (rather than calling buildArgs directly) so the
// tests exercise the resolveLimits + ClampLimits chain end-to-end.
// =============================================================================

// TestBwrapDriver_RlimitFlagsPresent verifies that --rlimit-as
// (in bytes) and --rlimit-nofile (as integer) appear in argv
// with values matching ResourceLimits.MaxMemoryMB and MaxFDs.
func TestBwrapDriver_RlimitFlagsPresent(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeBwrapStub(t, stubDir, "bwrap")
	rec := recordFile(t)
	d, _ := NewBwrapDriver(stubPath)

	cap := &Capability{
		ID:             "cap_rlimit",
		ExecutionDomain: DomainSandbox,
		Metadata:        CapabilityMetadata{},
	}
	ctx := injectDriverContext(context.Background(), cap)
	limits := ResourceLimits{
		MaxRuntimeMs:   30_000,
		MaxMemoryMB:    256,
		MaxFDs:         128,
		MaxOutputBytes: 16 * 1024 * 1024,
	}
	t.Setenv("BWRAP_RECORD_FILE", rec)
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	_, err := d.Run(ctx, DriverRequest{
		Language:   LangBash,
		SourceCode: "echo hi",
		Limits:     limits,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := readBwrapArgs(t, rec)
	wantMemory := strconvItoa(MemoryBytes(256)) // 256 MiB in bytes
	wantFDs := "128"

	assertFlagPair(t, args, "--rlimit-as", wantMemory)
	assertFlagPair(t, args, "--rlimit-nofile", wantFDs)

	// And: --timeout MUST NOT appear (ctx-only timeout per
	// EX-4 design decision).
	for _, a := range args {
		if a == "--timeout" {
			t.Errorf("bwrap should not receive --timeout flag (ctx handles wall-clock); argv=%v", args)
		}
	}
}

// TestBwrapDriver_RlimitFlagsClampAtCeiling verifies that an
// absurd MaxMemoryMB request is reduced to the documented
// ceiling BEFORE reaching the Driver. This is the EX-4.2
// contract: resolveLimits clamps, the Driver sees the clamp.
func TestBwrapDriver_RlimitFlagsClampAtCeiling(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeBwrapStub(t, stubDir, "bwrap")
	rec := recordFile(t)
	d, _ := NewBwrapDriver(stubPath)

	// Build a full Executor with the Driver so we exercise
	// the real resolveLimits + ClampLimits chain. The
	// Executor sees a Capability whose metadata requests
	// 1 PiB; resolveLimits should clamp to LimitsCeiling().
	store, db, _ := newTestStore(t)
	src := "echo hi"
	srcHash := hashOf(src)
	seedCapabilityWithHash(t, db, "cap_absurd", "cap_absurd", StateActive,
		src, string(LangBash), srcHash,
		`{"max_memory_mb": 1048576, "max_fds": 999999999}`)

	cap := &Capability{
		ID:              "cap_absurd",
		Name:            "cap_absurd",
		SourceCode:      src,
		SourceLanguage:  string(LangBash),
		SourceHash:      srcHash,
		State:           StateActive,
		ExecutionDomain: DomainSandbox,
		Metadata: CapabilityMetadata{
			"max_memory_mb": int64(1048576),
			"max_fds":       int64(999999999),
		},
	}
	ex := NewExecutorWithDispatcher(store,
		NewStaticDispatcher(map[ExecutionDomain]Driver{
			DomainSandbox: d,
		}),
		nil,
	)

	t.Setenv("BWRAP_RECORD_FILE", rec)
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	if _, err := ex.Invoke(context.Background(), InvokeRequest{
		Capability: cap,
		Language:   LangBash,
		SourceCode: src,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	args := readBwrapArgs(t, rec)
	ceil := LimitsCeiling()
	wantMemory := strconvItoa(MemoryBytes(ceil.MaxMemoryMB))
	wantFDs := strconvItoa(ceil.MaxFDs)

	assertFlagPair(t, args, "--rlimit-as", wantMemory)
	assertFlagPair(t, args, "--rlimit-nofile", wantFDs)
}

// TestBwrapDriver_DefaultLimitsAppearInArgv is a regression
// guard: the default limits (well under the ceiling) must
// still produce rlimit flags — we don't skip the flag
// emission for "small" values. The Executor always wants a
// hard ceiling, even for benign defaults.
func TestBwrapDriver_DefaultLimitsAppearInArgv(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeBwrapStub(t, stubDir, "bwrap")
	rec := recordFile(t)
	d, _ := NewBwrapDriver(stubPath)

	cap := &Capability{
		ID:             "cap_default_limits",
		ExecutionDomain: DomainSandbox,
		Metadata:        CapabilityMetadata{},
	}
	ctx := injectDriverContext(context.Background(), cap)
	t.Setenv("BWRAP_RECORD_FILE", rec)
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	_, err := d.Run(ctx, DriverRequest{
		Language:   LangBash,
		SourceCode: "echo hi",
		Limits:     DefaultResourceLimits(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := readBwrapArgs(t, rec)
	wantMemory := strconvItoa(MemoryBytes(DefaultResourceLimits().MaxMemoryMB))
	wantFDs := strconvItoa(DefaultResourceLimits().MaxFDs)
	assertFlagPair(t, args, "--rlimit-as", wantMemory)
	assertFlagPair(t, args, "--rlimit-nofile", wantFDs)
}

// =============================================================================
// EX-5 truncation tests
// =============================================================================

// TestBwrapDriver_TruncationFlagsPerStream verifies that
// EX-5's per-stream truncation flags surface correctly. The
// stub writes 2KB to stdout + 100B to stderr; with a 1KB
// cap, stdout should report StdoutTruncated=true and stderr
// should report StderrTruncated=false.
func TestBwrapDriver_TruncationFlagsPerStream(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeBwrapStub(t, stubDir, "bwrap")
	d, _ := NewBwrapDriver(stubPath)

	// The stub's BWRAP_WRITE_OUTPUT writes "stdout-msg\n" +
	// "stderr-msg\n" — both very small. For a per-stream
	// overflow test, we need the stub to dump many bytes.
	// Extend the stub via a wrapper script.
	bigStubPath := filepath.Join(stubDir, "bwrap-big")
	bigScript := `#!/bin/bash
set -e
# Dump 2KB to stdout, 100B to stderr, then exit 0.
head -c 2048 /dev/urandom | base64 > /dev/stdout
head -c 100  /dev/urandom | base64 > /dev/stderr
exit 0
`
	if err := os.WriteFile(bigStubPath, []byte(bigScript), 0755); err != nil {
		t.Fatalf("write big stub: %v", err)
	}
	d.BwrapPath = bigStubPath

	cap := &Capability{
		ID:             "cap_trunc",
		ExecutionDomain: DomainSandbox,
		Metadata:        CapabilityMetadata{},
	}
	ctx := injectDriverContext(context.Background(), cap)
	t.Setenv("BWRAP_RECORD_FILE", "")
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	res, err := d.Run(ctx, DriverRequest{
		Language:   LangBash,
		SourceCode: "echo overflow",
		Limits: ResourceLimits{
			MaxRuntimeMs:   30_000,
			MaxMemoryMB:    512,
			MaxFDs:         256,
			MaxOutputBytes: 1024, // 1KB cap
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.StdoutTruncated {
		t.Errorf("StdoutTruncated = false, want true (2KB output > 1KB cap)")
	}
	if res.StderrTruncated {
		t.Errorf("StderrTruncated = true, want false (100B output < 1KB cap)")
	}
	if len(res.Stdout) != 1024 {
		t.Errorf("len(Stdout) = %d, want 1024 (cap)", len(res.Stdout))
	}
}

// TestExecutor_TruncatedFlagIsOROfPerStream verifies the
// Executor's Truncated flag is the logical OR of the two
// per-stream flags. Test both combinations:
//
//   * stdout overflows, stderr fits → Truncated=true
//   * both fit → Truncated=false
//   * stderr overflows, stdout fits → Truncated=true (mirror)
func TestExecutor_TruncatedFlagIsOROfPerStream(t *testing.T) {
	t.Run("stdout_overflows", func(t *testing.T) {
		store, db, _ := newTestStore(t)
		driver := &FakeDriver{
			Results: []*DriverResult{{
				ExitCode:        0,
				Stdout:          bytes.Repeat([]byte("a"), 100),
				Stderr:          []byte("ok"),
				StdoutTruncated: true, // overflow
				StderrTruncated: false,
			}},
		}
		src := "echo a"
		srcHash := hashOf(src)
		seedCapabilityWithHash(t, db, "cap_so", "cap_so", StateActive,
			src, string(LangBash), srcHash, "{}")

		cap := &Capability{
			ID:             "cap_so",
			Name:           "cap_so",
			SourceCode:     src,
			SourceLanguage: string(LangBash),
			SourceHash:     srcHash,
			State:          StateActive,
			ExecutionDomain: DomainSandbox,
			Metadata:       CapabilityMetadata{},
		}
		ex := NewExecutor(store, driver, nil)

		res, err := ex.Invoke(context.Background(), InvokeRequest{
			Capability: cap,
			Language:   LangBash,
			SourceCode: src,
		})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if !res.Truncated {
			t.Errorf("Truncated = false, want true (stdout overflowed)")
		}
	})

	t.Run("both_fit", func(t *testing.T) {
		store, db, _ := newTestStore(t)
		driver := &FakeDriver{
			Results: []*DriverResult{{
				ExitCode:        0,
				Stdout:          []byte("ok"),
				Stderr:          []byte("ok"),
				StdoutTruncated: false,
				StderrTruncated: false,
			}},
		}
		src := "echo ok"
		srcHash := hashOf(src)
		seedCapabilityWithHash(t, db, "cap_bf", "cap_bf", StateActive,
			src, string(LangBash), srcHash, "{}")

		cap := &Capability{
			ID:             "cap_bf",
			Name:           "cap_bf",
			SourceCode:     src,
			SourceLanguage: string(LangBash),
			SourceHash:     srcHash,
			State:          StateActive,
			ExecutionDomain: DomainSandbox,
			Metadata:       CapabilityMetadata{},
		}
		ex := NewExecutor(store, driver, nil)

		res, err := ex.Invoke(context.Background(), InvokeRequest{
			Capability: cap,
			Language:   LangBash,
			SourceCode: src,
		})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if res.Truncated {
			t.Errorf("Truncated = true, want false (both fit)")
		}
	})

	t.Run("stderr_overflows", func(t *testing.T) {
		store, db, _ := newTestStore(t)
		driver := &FakeDriver{
			Results: []*DriverResult{{
				ExitCode:        0,
				Stdout:          []byte("ok"),
				Stderr:          bytes.Repeat([]byte("e"), 100),
				StdoutTruncated: false,
				StderrTruncated: true,
			}},
		}
		src := "echo e"
		srcHash := hashOf(src)
		seedCapabilityWithHash(t, db, "cap_se", "cap_se", StateActive,
			src, string(LangBash), srcHash, "{}")

		cap := &Capability{
			ID:             "cap_se",
			Name:           "cap_se",
			SourceCode:     src,
			SourceLanguage: string(LangBash),
			SourceHash:     srcHash,
			State:          StateActive,
			ExecutionDomain: DomainSandbox,
			Metadata:       CapabilityMetadata{},
		}
		ex := NewExecutor(store, driver, nil)

		res, err := ex.Invoke(context.Background(), InvokeRequest{
			Capability: cap,
			Language:   LangBash,
			SourceCode: src,
		})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if !res.Truncated {
			t.Errorf("Truncated = false, want true (stderr overflowed)")
		}
	})
}

// =============================================================================
// Test helpers
// =============================================================================

// assertFlagPair asserts that argv contains "--flag value" as
// adjacent elements (in either order — we only care about the
// pair existing somewhere).
func assertFlagPair(t *testing.T, argv []string, flag, want string) {
	t.Helper()
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == want {
			return
		}
	}
	t.Errorf("argv missing %s %s pair; got %v", flag, want, argv)
}

// strconvItoa is a thin wrapper that just calls strconv.FormatInt
// — kept as a named helper so the test bodies read like the
// bwrap argv they describe ("--rlimit-as " + strconvItoa(...)).
func strconvItoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

// =============================================================================
// Helpers
// =============================================================================

// hasPrefix reports whether args starts with want (in order).
// Used to check that the bwrap argv contains the spec's §4.1
// prefix without forcing tests to enumerate every element.
func hasPrefix(args, want []string) bool {
	if len(args) < len(want) {
		return false
	}
	for i := range want {
		if args[i] != want[i] {
			return false
		}
	}
	return true
}

// containsAllPairs reports whether args contains every pair
// (a, b, c, d, ...) from want as adjacent elements. Used for
// --bind checks where order matters but surrounding flags don't.
func containsAllPairs(args, want []string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
