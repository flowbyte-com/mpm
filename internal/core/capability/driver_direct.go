package capability

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// =============================================================================
// driver_direct.go — direct-execution driver (spec §4.1 trusted + operator)
//
// DirectDriver is the runtime complement to BwrapDriver for the
// `trusted` and `operator` execution domains. Where BwrapDriver
// isolates the capability in a bubblewrap namespace, DirectDriver
// runs the capability natively on the host with the executor's
// own permissions. This is intentional: trusted and operator
// domains are reached only after earned track record (trusted) or
// explicit operator approval (operator), so the host's full
// filesystem and network are the operator's decision — not the
// Executor's.
//
// Why a wrapper script for rlimits (instead of syscall.Setrlimit):
//
//   syscall.Setrlimit on the executor process races across
//   concurrent invocations — one goroutine's rlimit setting
//   clobbers another's, and resetting requires careful defer
//   ordering that still wouldn't be safe under -race. The
//   wrapper-script approach sets rlimits in a fresh forked
//   process (the wrapper) before exec'ing the interpreter, so
//   the limits apply only to the child and only for its
//   lifetime. No cross-invocation state, no platform-specific
//   SysProcAttr plumbing. Portable across any POSIX shell.
//
// Source delivery uses the same tempfile pattern as BwrapDriver
// (0600 perms, bind-mounted read-only in bwrap; here it's just
// referenced by absolute path). Same quoting-hazard defense, same
// debugger-can-cat-the-file ergonomics.
//
// Failure semantics:
//
//   * ctx cancellation → SIGKILL to the child's process group
//     (matches BwrapDriver's contract so EX-7 fracture detection
//     has uniform semantics across all four domains).
//   * child exit non-zero → capability-level result with
//     ExitCode preserved.
//   * child killed by ctx → capability-level ExitCode=-1 so EX-7
//     can distinguish "script ran and failed" from "killed for
//     taking too long."
//   * tempfile write failure → driver-level (nil, err).
//   * missing interpreter → driver-level (nil,
//     ErrDirectInterpreterMissing). Pre-flight checked via
//     os.Stat before spawning; otherwise the wrapper's exec
//     would surface as ExitCode=127 (collides with the
//     legitimate "script returned 127" outcome). We don't
//     fail-loud at construction because DirectDriver has no
//     external binary dependency to check (interpreters come
//     from SourceLanguage.Interpreter(), which is a host
//     prerequisite not a driver prerequisite).
//
// Spec: docs/architecture/capability-lifecycle.md §4.1 (trusted +
//      operator arg shape).
// =============================================================================

// ErrDirectInterpreterMissing is the runtime failure when the
// configured interpreter (SourceLanguage.Interpreter() or
// InterpreterOverride) does not exist on the host. Returns a
// driver-level (nil, err) so the Executor's Step 3b short-circuit
// doesn't record this as an invocation — a missing interpreter is
// a host-misconfiguration, not a capability bug.
var ErrDirectInterpreterMissing = errors.New("capability: DirectDriver: interpreter binary not found")

// DirectDriver runs capabilities natively (no namespace
// isolation). A single instance serves both `trusted` and
// `operator` domains — the operator-only metadata stamp check
// lives in Executor.Invoke (Step 3b), not in the Driver, because
// that gate is policy not execution.
//
// Fields:
//
//   InterpreterOverride: optional. When non-empty, replaces the
//     SourceLanguage.Interpreter() path. Tests inject shell
//     stubs here; production leaves this empty.
//
//   WaitDelay: grace period between ctx cancellation (SIGTERM)
//     and the SIGKILL sent by WaitDelay. Default 100ms. The
//     wrapper script exits on SIGTERM (it doesn't trap it), so
//     the inner interpreter receives SIGTERM and has this window
//     to flush stdout/stderr before the process group is
//     SIGKILLed.
//
//   Now: clock for StartedAt/FinishedAt timestamping. Production
//     uses time.Now; tests inject a frozen clock for
//     deterministic DurationMs assertions.
type DirectDriver struct {
	InterpreterOverride string
	WaitDelay           time.Duration
	Now                 func() time.Time
}

// NewDirectDriver returns a ready *DirectDriver. Unlike
// NewBwrapDriver there is no fail-loud check — direct execution
// has no external binary dependency (the interpreter comes from
// SourceLanguage.Interpreter() at Run time). If the interpreter
// is missing, Run surfaces the failure as a driver-level error.
func NewDirectDriver() *DirectDriver {
	return &DirectDriver{
		WaitDelay: 100 * time.Millisecond,
		Now:       time.Now,
	}
}

// DriverName implements NamedDriver. Always "direct" — canonical
// telemetry stamp; queries grouping failures by driver rely on it.
func (d *DirectDriver) DriverName() string { return "direct" }

// Run implements Driver. See file header for failure semantics.
//
// DriverRequest.Limits MUST be already-resolved (the Executor
// resolves the three-tier precedence before calling Run); the
// Driver reads limits but does not look at metadata.
//
// The driverName field of DriverRequest is informational; the
// DriverName() method on the struct is authoritative.
func (d *DirectDriver) Run(ctx context.Context, req DriverRequest) (*DriverResult, error) {
	// Step 1: write source to a tempfile with 0600 perms.
	// Same pattern as BwrapDriver.Run — the on-disk
	// representation matches the on-the-wire representation
	// (a debugger can cat the tempfile to see exactly what
	// ran). The file lives in /tmp because that's the only
	// path guaranteed writable across the four execution
	// domains.
	sourcePath, err := writeSourceTempfile(req.SourceCode)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(sourcePath) }()

	// Step 2: resolve interpreter. The override path is used
	// by tests (shell stubs at t.TempDir()); production code
	// leaves InterpreterOverride empty and gets the absolute
	// path from SourceLanguage.Interpreter().
	interpreter := d.resolveInterpreter(req)

	// Pre-flight: verify the interpreter exists before
	// writing the wrapper. Without this check, a missing
	// interpreter would surface as ExitCode=127 from the
	// wrapper script's exec, which collides with the
	// legitimate "script returned 127" outcome. Returning a
	// typed driver-level error here lets the Executor's
	// Step 3b short-circuit fire cleanly.
	if _, err := os.Stat(interpreter); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrDirectInterpreterMissing, interpreter, err)
	}

	// Step 3: write the wrapper script. The wrapper sets
	// rlimits (ulimit -v, ulimit -n) then execs the
	// interpreter with the source tempfile + caller args.
	// Why a wrapper instead of prlimit: ulimit in a forked
	// child is race-free across concurrent invocations and
	// doesn't touch the executor process's rlimits. See the
	// file header for the full rationale.
	wrapperPath, err := d.writeWrapper(req, interpreter, sourcePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(wrapperPath) }()

	// Step 4: spawn the wrapper as the child process. Setpgid
	// makes the wrapper (and the interpreter it execs) a new
	// process group leader; cmd.Cancel then signals the whole
	// group on ctx cancellation. This matches BwrapDriver's
	// kill semantics so EX-7 fracture detection sees the same
	// "killed by ctx" outcome across all four domains.
	startedAt := d.now()
	cmd := exec.CommandContext(ctx, wrapperPath, req.Args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative PID = process group. SIGKILL the wrapper
		// AND the interpreter it exec'd. We use SIGKILL
		// rather than SIGTERM because the wrapper doesn't
		// trap signals; WaitDelay's SIGTERM-grace window is
		// for letting the inner interpreter flush its
		// output, but if WaitDelay's timer fires before
		// that finishes, the inner process gets SIGKILL
		// anyway.
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = d.WaitDelay
	cmd.Env = buildChildEnv(req.Env)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("capability: DirectDriver: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("capability: DirectDriver: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("capability: DirectDriver: start: %w", err)
	}

	// Step 5: drain pipes in parallel. Same pattern as
	// BwrapDriver.Run — drainCapped caps each pipe at
	// req.Limits.MaxOutputBytes and reports the per-stream
	// truncation status via DriverResult.StdoutTruncated /
	// StderrTruncated.
	stdout, stdoutTruncated := drainCapped(stdoutPipe, req.Limits.MaxOutputBytes)
	stderr, stderrTruncated := drainCapped(stderrPipe, req.Limits.MaxOutputBytes)

	// Step 6: wait for the child. cmd.Wait closes the pipes
	// after the process exits, so the drainCapped calls above
	// will return promptly after Wait.
	waitErr := cmd.Wait()
	finishedAt := d.now()

	// Step 7: classify the result. Capability-level failures
	// (non-zero exit, timeout) become a populated result with
	// a meaningful ExitCode; driver-level failures (exec
	// problems, fork failures) become (nil, err).
	exitCode := 0
	if waitErr != nil {
		// ctx-cancelled timeout: surface as ExitCode=-1 so
		// EX-7 fracture detection can distinguish "the script
		// ran and returned non-zero" from "the script was
		// killed for taking too long." Both increment
		// failure_count, but the EX-7 sliding window cares
		// about the latter for fracture-event emission.
		if ctx.Err() != nil {
			exitCode = -1
		} else if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("capability: DirectDriver: wait: %w", waitErr)
		}
	}

	return &DriverResult{
		ExitCode:        exitCode,
		Stdout:          stdout,
		Stderr:          stderr,
		StdoutTruncated: stdoutTruncated,
		StderrTruncated: stderrTruncated,
		DurationMs:      finishedAt.Sub(startedAt).Milliseconds(),
	}, nil
}

// resolveInterpreter picks the interpreter path: override if
// set (tests), otherwise SourceLanguage.Interpreter()'s
// canonical absolute path. The result is passed to the wrapper
// script's exec line; if it doesn't exist, the exec will fail
// and Start() surfaces ErrDirectInterpreterMissing.
func (d *DirectDriver) resolveInterpreter(req DriverRequest) string {
	if d.InterpreterOverride != "" {
		return d.InterpreterOverride
	}
	return req.Language.Interpreter()
}

// now returns the current time. Pluggable for tests via d.Now.
func (d *DirectDriver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// writeWrapper writes a 2-line + boilerplate shell wrapper that
// sets the requested rlimits then execs the interpreter with
// the source tempfile + caller args. Returns the wrapper's path.
//
// The wrapper is given mode 0700 because /bin/sh won't exec a
// non-executable file. The file is deleted by the caller via
// the same defer pattern as the source tempfile.
//
// The wrapper self-snapshots to $WRAPPER_SNAPSHOT_FILE if set
// — a test-only feature that lets driver_direct_test.go verify
// the wrapper's contents without racing against its deletion.
// The cost is one /bin/sh `[ -n ... ]` check on every Run, which
// is negligible.
func (d *DirectDriver) writeWrapper(req DriverRequest, interpreter, sourcePath string) (string, error) {
	f, err := os.CreateTemp("", "mpm-cap-wrap-*.sh")
	if err != nil {
		return "", fmt.Errorf("capability: DirectDriver: wrapper tempfile: %w", err)
	}
	path := f.Name()
	if err := f.Chmod(0700); err != nil {
		f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("capability: DirectDriver: wrapper chmod: %w", err)
	}

	memKB := req.Limits.MaxMemoryMB * 1024 // MB → KB (ulimit -v takes KB on Linux)

	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString("# capability-invocation wrapper (auto-generated; do not edit)\n")
	// Self-snapshot for tests. /bin/sh evaluates the test in a
	// forked child; the cost is one stat + one open + one write,
	// only when WRAPPER_SNAPSHOT_FILE is non-empty (i.e., only
	// in tests).
	script.WriteString("if [ -n \"$WRAPPER_SNAPSHOT_FILE\" ]; then\n")
	script.WriteString("\tcat \"$0\" > \"$WRAPPER_SNAPSHOT_FILE\"\n")
	script.WriteString("fi\n")
	if memKB > 0 {
		script.WriteString(fmt.Sprintf("ulimit -v %d\n", memKB))
	}
	if req.Limits.MaxFDs > 0 {
		script.WriteString(fmt.Sprintf("ulimit -n %d\n", req.Limits.MaxFDs))
	}
	// Quote interpreter + source path so paths with spaces
	// (rare but possible) don't break the exec. Caller args
	// flow via "$@" to preserve caller-side quoting.
	script.WriteString(fmt.Sprintf("exec %s %q \"$@\"\n", interpreter, sourcePath))

	if _, err := f.WriteString(script.String()); err != nil {
		f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("capability: DirectDriver: wrapper write: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("capability: DirectDriver: wrapper close: %w", err)
	}
	return path, nil
}

// writeSourceTempfile writes source to a tempfile in /tmp with
// mode 0600 and returns the path. The caller is responsible for
// removing the file via defer (matches BwrapDriver's pattern;
// the file is consumed before Run returns so the unlink is
// safe).
//
// Factored out as a package-level helper rather than duplicated
// inline in BwrapDriver.Run / DirectDriver.Run so any future
// shared source-delivery logic (e.g., content scanning) lives
// in one place.
func writeSourceTempfile(source string) (string, error) {
	f, err := os.CreateTemp("", "mpm-cap-*.sh")
	if err != nil {
		return "", fmt.Errorf("capability: source tempfile: %w", err)
	}
	path := f.Name()
	if err := f.Chmod(0600); err != nil {
		f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("capability: source tempfile chmod: %w", err)
	}
	if _, err := f.WriteString(source); err != nil {
		f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("capability: source tempfile write: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("capability: source tempfile close: %w", err)
	}
	return path, nil
}

// buildChildEnv layers the caller's req.Env on top of the
// executor process's environment, with caller values winning on
// conflict. Returns the merged environment as a KEY=VALUE slice
// suitable for cmd.Env.
//
// Standard Unix semantics: a caller who wants to UNSET a parent
// env var can pass an empty string (we don't currently support
// that — but the operator-blessed trusted/operator code is
// expected to know which env vars it cares about).
//
// env vars without an '=' are skipped (rare but legal on some
// platforms; not useful to forward).
func buildChildEnv(callerEnv map[string]string) []string {
	// Start from parent env. Allocate capacity for parent + caller.
	parent := os.Environ()
	merged := make(map[string]string, len(parent)+len(callerEnv))
	for _, kv := range parent {
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			merged[kv[:idx]] = kv[idx+1:]
		}
	}
	// Layer caller env on top. Caller wins on key collision —
	// matches the standard semantics of execve() in shells
	// that use `env KEY=VAL ...`.
	for k, v := range callerEnv {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// Compile-time guard that DirectDriver satisfies Driver + NamedDriver.
var (
	_ Driver      = (*DirectDriver)(nil)
	_ NamedDriver = (*DirectDriver)(nil)
)

// =============================================================================
// Notes
// =============================================================================
//
// Why 0700 for the wrapper, 0600 for the source:
//
//   The source tempfile contains capability source code. 0600
//   (read/write owner only) is appropriate because the file is
//   read by the wrapper script (which runs as the executor's
//   user — the same UID).
//
//   The wrapper tempfile is exec'd by /bin/sh. /bin/sh refuses
//   to exec a file without the executable bit set, so 0700
//   (read/write/execute owner only) is required. We could use
//   0755 but the wrapper contains capability-specific paths and
//   limits; keeping it owner-only is the conservative choice.
//
// Why we don't chdir to /tmp:
//
//   BwrapDriver uses --chdir /tmp (sandbox) or --chdir
//   project_dir (restricted) because bwrap's namespace hides the
//   host filesystem. DirectDriver has no namespace, so the
//   capability inherits the executor process's cwd. This is
//   what trusted/operator code expects — operator-blessed
//   capabilities may legitimately rely on cwd-relative paths
//   (project-relative imports, local config files, etc.).
//
// Why we use the wrapper as the cmd target (not the interpreter):
//
//   The wrapper sets rlimits and then execs the interpreter. We
//   could alternatively set rlimits via syscall.SysProcAttr on
//   the interpreter cmd directly, but Go's SysProcAttr.Rlimit
//   field is Linux-only and the values are passed through opaquely.
//   The wrapper-script approach is portable, testable, and the
//   resulting process tree (wrapper → interpreter) is what the
//   process-group kill in cmd.Cancel wants anyway.
//
// Why we layer caller env on top of parent env:
//
//   Trusted/operator code is operator-blessed. The operator
//   expects the executor's environment (PATH, HOME, LANG,
//   SSH_AUTH_SOCK, DATABASE_URL, etc.) to be visible. Stripping
//   it would surprise every operator who relies on env-driven
//   workflows. Caller req.Env wins on conflict because that's
//   what the caller's caller (the agent, the operator's CLI)
//   asked for.
// =============================================================================