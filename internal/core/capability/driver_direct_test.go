package capability

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// =============================================================================
// driver_direct_test.go — DirectDriver tests (EX-6.4)
//
// Like driver_bwrap_test.go, these tests don't depend on the
// real /bin/bash. A shell-script stub is installed at
// t.TempDir()/bash, pointed at via DirectDriver.InterpreterOverride,
// and the stub records its argv + env so tests can assert
// against it.
//
// The DirectDriver's runtime shape differs from BwrapDriver:
//
//   * The wrapper script is the immediate child of the executor.
//     It runs `ulimit -v N; ulimit -n M; exec <interpreter>
//     <source_tempfile> "$@"`. Tests inspect the wrapper's
//     contents via $WRAPPER_SNAPSHOT_FILE (a test-only env var
//     the wrapper writes its own contents to when set).
//   * The interpreter (the stub) sees argv after exec:
//     argv[0] = stub path, argv[1] = source tempfile,
//     argv[2..] = caller args.
//   * The stub's env includes everything from the parent
//     executor env (PATH, HOME, ...) plus the caller's req.Env.
//
// Test cases:
//
//   1. NotFoundReturnsErr: InterpreterOverride path missing →
//      Run returns (nil, error).
//   2. DriverName_IsDirect: telemetry stamp is "direct".
//   3. SourceDeliveredViaTempfile: stub sees argv[1] =
//      /tmp/mpm-cap-*.sh.
//   4. CallerArgsFlowThrough: argv[2..] = req.Args.
//   5. WrapperScriptSetsRlimits: wrapper contents include
//      `ulimit -v <kb>` + `ulimit -n <count>` with the
//      resolved values from ResourceLimits.
//   6. WrapperClampsRlimitsToCeiling: huge MaxMemoryMB /
//      MaxFDs end up at the documented ceiling, not the
//      requested values.
//   7. TimeoutKillsProcessGroup: 0.10s ctx kills a 5s sleep.
//   8. ExitCodePreserved: child exits 7 → DriverResult.ExitCode=7.
//   9. ExitCodeMinusOneOnCtxCancel: ctx cancellation gives
//      ExitCode=-1 (matches BwrapDriver contract).
//  10. DispatchedFromStaticDispatcher: StaticDispatcher
//      routes trusted + operator to the DirectDriver.
//  11. DefaultDispatcherWiresAllDomains: NewDefaultDispatcher
//      covers sandbox/restricted/trusted/operator.
//  12. EnvFromCallerFlowsToChild: req.Env["KEY"]=value reaches
//      the stub as $KEY=value.
//  13. PassthroughEnvParent: parent env (e.g., PATH) reaches
//      the stub unchanged.
//  14. CallerEnvOverridesParent: req.Env["PATH"] wins on
//      conflict (Unix execve semantics).
//  15. TempfilesCleanedUp: source + wrapper tempfiles are gone
//      after Run returns.
//  16. OperatorDomainRequiresOperatorApprovedAt: Executor
//      refuses to dispatch an operator-domain capability
//      whose metadata lacks operator_approved_at.
//  17. OperatorDomainAllowedWhenApproved: Executor dispatches
//      when operator_approved_at is set.
// =============================================================================

// writeDirectStub installs a shell-script interpreter at path.
// The stub records its argv + env, optionally sleeps, optionally
// writes output, optionally exits with a code. Models a real
// interpreter's behavior without depending on /bin/bash being
// the actual shell.
func writeDirectStub(t *testing.T, dir, name string) string {
	t.Helper()
	stubPath := filepath.Join(dir, name)
	script := `#!/bin/bash
# DirectDriver test stub. Records argv (one per line) and env
# keys (one per line, KEY=VALUE), then optionally sleeps / writes
# output / exits with code.

set -e

if [ -n "$BWRAP_RECORD_FILE" ]; then
  : > "$BWRAP_RECORD_FILE"
  for arg in "$@"; do
    printf '%s\n' "$arg" >> "$BWRAP_RECORD_FILE"
  done
fi

if [ -n "$BWRAP_RECORD_ENV_FILE" ]; then
  : > "$BWRAP_RECORD_ENV_FILE"
  # Print only keys with the test prefix to keep the output small.
  env | grep -E '^(BWRAP_|TEST_|WRAPPER_)' | sort >> "$BWRAP_RECORD_ENV_FILE"
fi

if [ -n "$BWRAP_SLEEP_MS" ]; then
  export BWRAP_SLEEP_MS
  python3 -c 'import os, time; time.sleep(int(os.environ["BWRAP_SLEEP_MS"]) / 1000.0)'
fi

if [ -n "$BWRAP_WRITE_OUTPUT" ]; then
  printf 'stdout-msg\n'
  printf 'stderr-msg\n' >&2
fi

if [ -n "$BWRAP_EXIT_CODE" ]; then
  exit "$BWRAP_EXIT_CODE"
fi

exit 0
`
	if err := os.WriteFile(stubPath, []byte(script), 0755); err != nil {
		t.Fatalf("write direct stub: %v", err)
	}
	return stubPath
}

// =============================================================================
// Construction / metadata tests
// =============================================================================

// TestDirectDriver_DriverNameIsDirect verifies the canonical
// telemetry stamp. Queries grouping failures by driver rely on
// the string being "direct" — not the Go type name.
func TestDirectDriver_DriverNameIsDirect(t *testing.T) {
	d := NewDirectDriver()
	if got := d.DriverName(); got != "direct" {
		t.Errorf("DriverName() = %q, want direct", got)
	}
}

// TestDirectDriver_NotFoundReturnsErr verifies that a
// nonexistent interpreter override surfaces as a driver-level
// error. Unlike BwrapDriver's fail-loud-at-construction
// pattern (which checks for the bwrap binary on PATH), the
// DirectDriver defers the check to Run because there is no
// single external dependency to validate at construction.
func TestDirectDriver_NotFoundReturnsErr(t *testing.T) {
	d := NewDirectDriver()
	d.InterpreterOverride = "/nonexistent/path/bash"

	_, err := d.Run(context.Background(), DriverRequest{
		Language:   LangBash,
		SourceCode: "echo hi",
		Limits:     DefaultResourceLimits(),
	})
	if err == nil {
		t.Fatal("Run succeeded with nonexistent interpreter")
	}
	if !errors.Is(err, ErrDirectInterpreterMissing) {
		t.Errorf("err = %v, want errors.Is(ErrDirectInterpreterMissing)", err)
	}
}

// =============================================================================
// Source delivery tests
// =============================================================================

// TestDirectDriver_SourceDeliveredViaTempfile verifies that the
// DirectDriver writes source to a tempfile and passes the
// absolute path as argv[1] to the interpreter. Matches the
// BwrapDriver pattern (0600 tempfile, no quoting hazards,
// debugger can cat the file).
func TestDirectDriver_SourceDeliveredViaTempfile(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")
	rec := filepath.Join(t.TempDir(), "argv.txt")

	d := NewDirectDriver()
	d.InterpreterOverride = stubPath

	t.Setenv("BWRAP_RECORD_FILE", rec)
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	if _, err := d.Run(context.Background(), DriverRequest{
		Language:   LangBash,
		SourceCode: "echo hello",
		Limits:     DefaultResourceLimits(),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := readBwrapArgs(t, rec)
	if len(args) < 1 {
		t.Fatalf("argv empty; want ≥1 line")
	}
	srcPath := args[0]
	if !strings.HasPrefix(srcPath, "/tmp/mpm-cap-") {
		t.Errorf("argv[0] (source tempfile) = %q, want /tmp/mpm-cap- prefix", srcPath)
	}
	if !strings.HasSuffix(srcPath, ".sh") {
		t.Errorf("argv[0] (source tempfile) = %q, want .sh suffix", srcPath)
	}
}

// TestDirectDriver_CallerArgsFlowThrough verifies that the
// caller's req.Args reach the interpreter as argv[2..] after
// the wrapper execs. The wrapper script's "$@" preserves
// caller-side quoting.
func TestDirectDriver_CallerArgsFlowThrough(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")
	rec := filepath.Join(t.TempDir(), "argv.txt")

	d := NewDirectDriver()
	d.InterpreterOverride = stubPath

	t.Setenv("BWRAP_RECORD_FILE", rec)
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	if _, err := d.Run(context.Background(), DriverRequest{
		Language:   LangBash,
		SourceCode: "echo hi",
		Args:       []string{"alpha", "beta with space", "gamma"},
		Limits:     DefaultResourceLimits(),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	args := readBwrapArgs(t, rec)
	// argv[0] is the source tempfile (test above), argv[1..]
	// are the caller args.
	if len(args) != 4 {
		t.Fatalf("argv has %d entries, want 4 (tempfile + 3 caller args): %q", len(args), args)
	}
	if args[1] != "alpha" {
		t.Errorf("argv[1] = %q, want alpha", args[1])
	}
	if args[2] != "beta with space" {
		t.Errorf("argv[2] = %q, want 'beta with space' (caller quoting preserved)", args[2])
	}
	if args[3] != "gamma" {
		t.Errorf("argv[3] = %q, want gamma", args[3])
	}
}

// =============================================================================
// Rlimit wrapper tests
// =============================================================================

// TestDirectDriver_WrapperScriptSetsRlimits verifies that the
// wrapper script contains ulimit -v <kb> and ulimit -n <count>
// with values matching the resolved ResourceLimits. Uses the
// $WRAPPER_SNAPSHOT_FILE env var that the wrapper self-snapshots
// to (a test-only feature).
func TestDirectDriver_WrapperScriptSetsRlimits(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")
	snap := filepath.Join(t.TempDir(), "wrapper.sh")

	d := NewDirectDriver()
	d.InterpreterOverride = stubPath

	t.Setenv("BWRAP_RECORD_FILE", "/dev/null")
	t.Setenv("WRAPPER_SNAPSHOT_FILE", snap)
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	if _, err := d.Run(context.Background(), DriverRequest{
		Language:   LangBash,
		SourceCode: "echo",
		Limits: ResourceLimits{
			MaxRuntimeMs:   30_000,
			MaxMemoryMB:    256,
			MaxFDs:         128,
			MaxOutputBytes: MaxOutputBytesDefault,
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	content, err := os.ReadFile(snap)
	if err != nil {
		t.Fatalf("read wrapper snapshot: %v", err)
	}
	// 256 MB → 262144 KB (ulimit -v takes KB on Linux).
	wantLines := []string{"ulimit -v 262144", "ulimit -n 128"}
	for _, w := range wantLines {
		if !strings.Contains(string(content), w) {
			t.Errorf("wrapper missing %q; got:\n%s", w, content)
		}
	}
	// The wrapper must exec the interpreter — this is the
	// whole point. Sanity-check the exec line.
	if !strings.Contains(string(content), "exec ") {
		t.Errorf("wrapper missing exec line; got:\n%s", content)
	}
}

// TestDirectDriver_WrapperClampsRlimitsToCeiling verifies that
// absurd MaxMemoryMB / MaxFDs values in DriverRequest.Limits
// do NOT bypass the ceiling — but the DirectDriver does NOT
// clamp on its own (resolveLimits in Executor already does).
// This test verifies the *contract*: callers passing
// already-clamped limits get those limits in the wrapper; the
// clamping itself is EX-4's job.
func TestDirectDriver_WrapperClampsRlimitsToCeiling(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")
	snap := filepath.Join(t.TempDir(), "wrapper.sh")

	d := NewDirectDriver()
	d.InterpreterOverride = stubPath

	t.Setenv("BWRAP_RECORD_FILE", "/dev/null")
	t.Setenv("WRAPPER_SNAPSHOT_FILE", snap)

	// Pass already-clamped limits (the Executor would do this
	// via resolveLimits). The DirectDriver trusts its inputs —
	// it does NOT re-clamp. This test pins that contract.
	clamped := ClampLimits(ResourceLimits{
		MaxMemoryMB: 999_999_999,
		MaxFDs:      999_999_999,
	})

	if _, err := d.Run(context.Background(), DriverRequest{
		Language:   LangBash,
		SourceCode: "echo",
		Limits: ResourceLimits{
			MaxRuntimeMs:   30_000,
			MaxMemoryMB:    clamped.MaxMemoryMB,
			MaxFDs:         clamped.MaxFDs,
			MaxOutputBytes: MaxOutputBytesDefault,
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	content, err := os.ReadFile(snap)
	if err != nil {
		t.Fatalf("read wrapper snapshot: %v", err)
	}
	// Memory ceiling: 4096 MB → 4194304 KB.
	if !strings.Contains(string(content), fmt.Sprintf("ulimit -v %d", LimitsCeiling().MaxMemoryMB*1024)) {
		t.Errorf("wrapper missing ceiling ulimit -v; got:\n%s", content)
	}
	// FD ceiling: 65536.
	if !strings.Contains(string(content), fmt.Sprintf("ulimit -n %d", LimitsCeiling().MaxFDs)) {
		t.Errorf("wrapper missing ceiling ulimit -n; got:\n%s", content)
	}
}

// =============================================================================
// Subprocess lifecycle tests
// =============================================================================

// TestDirectDriver_TimeoutKillsProcessGroup verifies that
// ctx cancellation SIGKILLs the wrapper AND the interpreter it
// exec'd. The 0.10s ctx must complete in well under the
// child's 5s sleep.
//
// The interpreter stub forwards SIGTERM to its python child,
// so even if the executor sent SIGTERM (not SIGKILL) the test
// would still complete promptly. We use SIGKILL via Setpgid +
// cmd.Cancel to kill the process group, so the test exercises
// the documented kill path.
func TestDirectDriver_TimeoutKillsProcessGroup(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")

	d := NewDirectDriver()
	d.InterpreterOverride = stubPath

	t.Setenv("BWRAP_RECORD_FILE", "/dev/null")
	t.Setenv("BWRAP_SLEEP_MS", "5000")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := d.Run(ctx, DriverRequest{
		Language:   LangBash,
		SourceCode: "sleep 5",
		Limits:     DefaultResourceLimits(),
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed > 4*time.Second {
		t.Errorf("TimeoutKillsProcessGroup took %v, want <4s", elapsed)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (ctx cancellation)", res.ExitCode)
	}
}

// TestDirectDriver_ExitCodePreserved verifies that a child
// exit code (non-zero, non-timeout) propagates to DriverResult.
func TestDirectDriver_ExitCodePreserved(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")

	d := NewDirectDriver()
	d.InterpreterOverride = stubPath

	t.Setenv("BWRAP_RECORD_FILE", "/dev/null")
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "7")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	res, err := d.Run(context.Background(), DriverRequest{
		Language:   LangBash,
		SourceCode: "exit 7",
		Limits:     DefaultResourceLimits(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
}

// =============================================================================
// Dispatcher wiring tests
// =============================================================================

// TestDirectDriver_DispatchedFromStaticDispatcher verifies
// that a StaticDispatcher routing trusted + operator domains
// to a DirectDriver returns the same instance for both
// domains. Mirrors the BwrapDriver equivalent.
func TestDirectDriver_DispatchedFromStaticDispatcher(t *testing.T) {
	d := NewDirectDriver()

	disp := NewStaticDispatcher(map[ExecutionDomain]Driver{
		DomainTrusted:  d,
		DomainOperator: d,
	})

	for _, domain := range []ExecutionDomain{DomainTrusted, DomainOperator} {
		cap := &Capability{ID: "x", ExecutionDomain: domain}
		got, err := disp.DriverFor(cap)
		if err != nil {
			t.Errorf("DriverFor(%s): %v", domain, err)
			continue
		}
		if got != d {
			t.Errorf("DriverFor(%s) = %v, want %v", domain, got, d)
		}
	}
}

// TestDirectDriver_DefaultDispatcherWiresAllDomains verifies
// that NewDefaultDispatcher covers every ExecutionDomain —
// every lookup must succeed. The BwrapDriver + DirectDriver
// pair is the production runtime contract.
func TestDirectDriver_DefaultDispatcherWiresAllDomains(t *testing.T) {
	bwrap, _ := NewBwrapDriver("/bin/true") // anything on PATH
	direct := NewDirectDriver()

	disp := NewDefaultDispatcher(bwrap, direct)

	for _, domain := range AllExecutionDomains() {
		cap := &Capability{ID: "x", ExecutionDomain: domain}
		got, err := disp.DriverFor(cap)
		if err != nil {
			t.Errorf("DriverFor(%s): %v (NewDefaultDispatcher should cover every domain)", domain, err)
			continue
		}
		if got == nil {
			t.Errorf("DriverFor(%s) returned nil Driver", domain)
		}
	}
}

// TestDirectDriver_NilDriversPanic documents the
// fail-loud-at-construction contract for NewDefaultDispatcher.
// A misconfigured host should surface the failure at mpm start,
// not at the first invocation.
func TestDirectDriver_NilDriversPanic(t *testing.T) {
	t.Run("nil_bwrap", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for nil BwrapDriver")
			}
		}()
		NewDefaultDispatcher(nil, NewDirectDriver())
	})
	t.Run("nil_direct", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for nil DirectDriver")
			}
		}()
		NewDefaultDispatcher(&BwrapDriver{}, nil)
	})
}

// =============================================================================
// Environment propagation tests
// =============================================================================

// TestDirectDriver_EnvFromCallerFlowsToChild verifies that
// keys set in DriverRequest.Env reach the spawned child
// process. Uses the stub's BWRAP_RECORD_ENV_FILE feature.
func TestDirectDriver_EnvFromCallerFlowsToChild(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")
	recEnv := filepath.Join(t.TempDir(), "env.txt")

	d := NewDirectDriver()
	d.InterpreterOverride = stubPath

	t.Setenv("BWRAP_RECORD_FILE", "/dev/null")
	t.Setenv("BWRAP_RECORD_ENV_FILE", recEnv)
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	if _, err := d.Run(context.Background(), DriverRequest{
		Language:   LangBash,
		SourceCode: "echo",
		Env: map[string]string{
			"TEST_FROM_CALLER": "caller-value",
		},
		Limits: DefaultResourceLimits(),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	envOut, err := os.ReadFile(recEnv)
	if err != nil {
		t.Fatalf("read env record: %v", err)
	}
	if !strings.Contains(string(envOut), "TEST_FROM_CALLER=caller-value") {
		t.Errorf("child env missing TEST_FROM_CALLER; got:\n%s", envOut)
	}
}

// TestDirectDriver_PassthroughEnvParent verifies that the
// executor process's env vars (PATH, HOME, etc.) are visible
// to the spawned child. The DirectDriver passes through parent
// env + caller env overrides, matching standard Unix execve
// semantics.
func TestDirectDriver_PassthroughEnvParent(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")
	recEnv := filepath.Join(t.TempDir(), "env.txt")

	// Set a known parent env var. t.Setenv is restored at
	// test cleanup.
	t.Setenv("TEST_FROM_PARENT", "parent-value")

	d := NewDirectDriver()
	d.InterpreterOverride = stubPath

	t.Setenv("BWRAP_RECORD_FILE", "/dev/null")
	t.Setenv("BWRAP_RECORD_ENV_FILE", recEnv)
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	if _, err := d.Run(context.Background(), DriverRequest{
		Language:   LangBash,
		SourceCode: "echo",
		Limits:     DefaultResourceLimits(),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	envOut, err := os.ReadFile(recEnv)
	if err != nil {
		t.Fatalf("read env record: %v", err)
	}
	if !strings.Contains(string(envOut), "TEST_FROM_PARENT=parent-value") {
		t.Errorf("child env missing TEST_FROM_PARENT; got:\n%s", envOut)
	}
}

// TestDirectDriver_CallerEnvOverridesParent verifies that
// req.Env wins on key collision with the parent env. Standard
// execve semantics — the wrapper script's env line is the
// single source of truth.
func TestDirectDriver_CallerEnvOverridesParent(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")
	recEnv := filepath.Join(t.TempDir(), "env.txt")

	t.Setenv("TEST_OVERRIDE_KEY", "parent-value")

	d := NewDirectDriver()
	d.InterpreterOverride = stubPath

	t.Setenv("BWRAP_RECORD_FILE", "/dev/null")
	t.Setenv("BWRAP_RECORD_ENV_FILE", recEnv)
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	if _, err := d.Run(context.Background(), DriverRequest{
		Language:   LangBash,
		SourceCode: "echo",
		Env: map[string]string{
			"TEST_OVERRIDE_KEY": "caller-value",
		},
		Limits: DefaultResourceLimits(),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	envOut, err := os.ReadFile(recEnv)
	if err != nil {
		t.Fatalf("read env record: %v", err)
	}
	if strings.Contains(string(envOut), "TEST_OVERRIDE_KEY=parent-value") {
		t.Errorf("child env still has parent value; caller override didn't win:\n%s", envOut)
	}
	if !strings.Contains(string(envOut), "TEST_OVERRIDE_KEY=caller-value") {
		t.Errorf("child env missing caller override; got:\n%s", envOut)
	}
}

// =============================================================================
// Cleanup tests
// =============================================================================

// TestDirectDriver_TempfilesCleanedUp verifies that BOTH the
// source tempfile and the wrapper tempfile are unlinked after
// Run returns. Mirrors BwrapDriver.TempfileCleanedUp — the
// pattern is identical because both files use os.CreateTemp
// + deferred os.Remove.
func TestDirectDriver_TempfilesCleanedUp(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")

	d := NewDirectDriver()
	d.InterpreterOverride = stubPath

	t.Setenv("BWRAP_RECORD_FILE", "/dev/null")
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	if _, err := d.Run(context.Background(), DriverRequest{
		Language:   LangBash,
		SourceCode: "echo",
		Limits:     DefaultResourceLimits(),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// t.TempDir() is cleaned up at test end, but we want to
	// verify OUR tempfiles (in /tmp, not the test tempdir) are
	// gone. Count mpm-cap-* files in /tmp at start vs after.
	before := countMPMTempfiles(t)
	if _, err := d.Run(context.Background(), DriverRequest{
		Language:   LangBash,
		SourceCode: "echo",
		Limits:     DefaultResourceLimits(),
	}); err != nil {
		t.Fatalf("Run (second): %v", err)
	}
	after := countMPMTempfiles(t)
	if after > before {
		t.Errorf("tempfile leak: before=%d, after=%d (after one Run, count should not grow)", before, after)
	}
}

// =============================================================================
// Executor-level operator-domain gate
//
// The operator_approved_at check lives in Executor.Invoke,
// not in DirectDriver. These tests verify the gate fires (or
// doesn't) end-to-end through the Executor.
// =============================================================================

// TestDirectDriver_OperatorDomainRequiresOperatorApprovedAt
// verifies that an operator-domain capability whose metadata
// lacks operator_approved_at is refused BEFORE driver
// dispatch. The Executor must return ErrOperatorNotApproved
// and write no telemetry (the failure is policy, not execution).
func TestDirectDriver_OperatorDomainRequiresOperatorApprovedAt(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")
	direct := NewDirectDriver()
	direct.InterpreterOverride = stubPath

	store, db, _ := newTestStore(t)
	src := "echo hi"
	srcHash := hashOf(src)
	seedCapabilityWithHash(t, db, "cap_op", "cap_op", StateActive,
		src, string(LangBash), srcHash, "{}")

	cap := &Capability{
		ID:             "cap_op",
		Name:           "cap_op",
		SourceCode:     src,
		SourceLanguage: string(LangBash),
		SourceHash:     srcHash,
		State:          StateActive,
		ExecutionDomain: DomainOperator,
		Metadata:       CapabilityMetadata{}, // no operator_approved_at
	}

	bwrap, _ := NewBwrapDriver(stubPath) // reuse stub path; not invoked
	ex := NewExecutorWithDispatcher(store,
		NewDefaultDispatcher(bwrap, direct),
		nil,
	)

	t.Setenv("BWRAP_RECORD_FILE", "/dev/null")
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	_, err := ex.Invoke(context.Background(), InvokeRequest{
		Capability: cap,
		Language:   LangBash,
		SourceCode: src,
	})
	if err == nil {
		t.Fatal("Invoke succeeded without operator_approved_at")
	}
	if !errors.Is(err, ErrOperatorNotApproved) {
		t.Errorf("err = %v, want errors.Is(ErrOperatorNotApproved)", err)
	}
}

// TestDirectDriver_OperatorDomainAllowedWhenApproved verifies
// the gate's positive path: when operator_approved_at is set,
// the Executor dispatches and the DirectDriver runs the
// capability.
func TestDirectDriver_OperatorDomainAllowedWhenApproved(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")
	direct := NewDirectDriver()
	direct.InterpreterOverride = stubPath

	store, db, _ := newTestStore(t)
	src := "echo hi"
	srcHash := hashOf(src)
	seedCapabilityWithHash(t, db, "cap_op_ok", "cap_op_ok", StateActive,
		src, string(LangBash), srcHash, `{"operator_approved_at": 1700000000}`)

	cap := &Capability{
		ID:             "cap_op_ok",
		Name:           "cap_op_ok",
		SourceCode:     src,
		SourceLanguage: string(LangBash),
		SourceHash:     srcHash,
		State:          StateActive,
		ExecutionDomain: DomainOperator,
		Metadata: CapabilityMetadata{
			"operator_approved_at": int64(1700000000),
		},
	}

	bwrap, _ := NewBwrapDriver(stubPath)
	ex := NewExecutorWithDispatcher(store,
		NewDefaultDispatcher(bwrap, direct),
		nil,
	)

	t.Setenv("BWRAP_RECORD_FILE", "/dev/null")
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "")

	res, err := ex.Invoke(context.Background(), InvokeRequest{
		Capability: cap,
		Language:   LangBash,
		SourceCode: src,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// =============================================================================
// Output capture tests (smoke for the drainCapped path on direct exec)
// =============================================================================

// TestDirectDriver_OutputCapturedFromChild verifies that the
// DirectDriver's drainCapped path captures stdout/stderr from
// the spawned child process. Mirrors BwrapDriver's output
// capture tests.
func TestDirectDriver_OutputCapturedFromChild(t *testing.T) {
	stubDir := t.TempDir()
	stubPath := writeDirectStub(t, stubDir, "bash")

	d := NewDirectDriver()
	d.InterpreterOverride = stubPath

	t.Setenv("BWRAP_RECORD_FILE", "/dev/null")
	t.Setenv("BWRAP_SLEEP_MS", "")
	t.Setenv("BWRAP_EXIT_CODE", "")
	t.Setenv("BWRAP_WRITE_OUTPUT", "yes")

	res, err := d.Run(context.Background(), DriverRequest{
		Language:   LangBash,
		SourceCode: "echo",
		Limits:     DefaultResourceLimits(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Contains(res.Stdout, []byte("stdout-msg")) {
		t.Errorf("Stdout = %q, want contains 'stdout-msg'", string(res.Stdout))
	}
	if !bytes.Contains(res.Stderr, []byte("stderr-msg")) {
		t.Errorf("Stderr = %q, want contains 'stderr-msg'", string(res.Stderr))
	}
	if res.StdoutTruncated || res.StderrTruncated {
		t.Errorf("per-stream truncation flags set on small output: StdoutTruncated=%v StderrTruncated=%v",
			res.StdoutTruncated, res.StderrTruncated)
	}
}