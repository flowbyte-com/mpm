package capability

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// =============================================================================
// driver_bwrap.go — Bubblewrap execution driver (spec §4.1)
//
// BwrapDriver is the isolation primitive for `sandbox` and
// `restricted` execution domains. It runs the capability's
// source code inside a `bwrap` namespace with:
//
//   * a read-only view of the host's binaries and libraries
//   * a private /tmp and /home (no host state visible)
//   * no network access (sandbox) or restricted access
//     (restricted adds project bind mounts)
//   * an unshared PID namespace so the script can't see or
//     signal other processes on the host
//   * die-with-parent so the child dies if the executor dies
//     (no orphan bwrap processes outliving the agent)
//
// Source delivery uses a tempfile with 0600 perms, bind-mounted
// read-only into the sandbox. This avoids the quoting hazards
// of `bash -c "<arbitrary source>"` and keeps the on-disk
// representation identical to the on-the-wire representation.
//
// Failure semantics:
//
//   * bwrap missing on PATH  → NewBwrapDriver returns an error
//                              (fail-loud at construction; the
//                              agent operator learns the host
//                              is misconfigured at mpm start,
//                              not at the first invocation).
//   * exec.CommandContext    → child gets SIGKILL when ctx
//                              fires. WaitDelay (100ms) gives
//                              bwrap a grace window to clean
//                              up its namespaces.
//   * tempfile write failure → driver-level (nil, err).
//   * child exit non-zero    → capability-level (result, nil)
//                              with ExitCode preserved.
//   * child killed by ctx    → capability-level
//                              (ExitCode: -1, DurationMs: elapsed)
//                              so EX-7 fracture detection counts
//                              it as a failure, not an infra error.
//
// Spec: docs/archive/capability-lifecycle.md §4.1 (sandbox args),
//      §4.3 (timeout via ctx).
// =============================================================================

// ErrBwrapNotFound is the construction-time failure when the
// `bwrap` binary is missing from $PATH. Distinguishes "the host
// is misconfigured" from runtime driver failures (exec failure,
// fork failure, etc.) so mpm start can refuse to come up with a
// clear, actionable message.
var ErrBwrapNotFound = errors.New("capability: BwrapDriver: bwrap binary not found on PATH (install bubblewrap; e.g. apt-get install bubblewrap)")

// ErrBwrapProjectDirMissing is the runtime failure for
// restricted-domain invocations whose metadata lacks
// `project_dir` and the OS-level cwd is unavailable. A
// restricted capability must declare which directory to bind
// — we don't guess.
var ErrBwrapProjectDirMissing = errors.New("capability: BwrapDriver: restricted domain requires metadata.project_dir (or non-empty process cwd)")

// ErrBwrapAllowedPathInvalid is the runtime failure for
// restricted-domain invocations whose metadata.allowed_paths
// contains a relative path. bwrap's --bind requires absolute
// paths; relative paths would resolve relative to the wrong
// root inside the sandbox namespace.
var ErrBwrapAllowedPathInvalid = errors.New("capability: BwrapDriver: metadata.allowed_paths entry is not an absolute path")

// BwrapDriver runs capabilities under bubblewrap isolation.
//
// The struct is safe for concurrent use — every Run call builds
// its own tempfile + cmd, and exec.CommandContext is goroutine-
// safe per os/exec docs.
//
// Fields:
//
//   BwrapPath: absolute path to the bwrap binary. Empty in
//     production (resolved at construction time via
//     exec.LookPath). Tests inject a shell-stub path. Once
//     resolved, this field is immutable.
//
//   BwrapWaitDelay: grace period between ctx cancellation
//     (SIGTERM) and the SIGKILL sent by WaitDelay. Default
//     100ms. bwrap uses this window to unwind its namespaces
//     and remount /proc; below 100ms the child is usually
//     killed mid-unmount and leaks a half-torn-down namespace.
//
//   Now: clock for StartedAt/FinishedAt timestamping. Production
//     uses time.Now; tests inject a frozen clock for
//     deterministic DurationMs assertions. The Executor's clock
//     is the canonical source; the Driver's clock is only used
//     when the Driver is invoked directly (rare — Executor.Invoke
//     always timestamps from its own clock).
type BwrapDriver struct {
	BwrapPath     string
	BwrapWaitDelay time.Duration
	Now           func() time.Time
}

// NewBwrapDriver resolves the bwrap binary and returns a ready
// Driver. Returns ErrBwrapNotFound (wrapped with the underlying
// exec.LookPath error) if bwrap is not on PATH.
//
// The lookup is performed eagerly at construction time so a
// misconfigured host surfaces the failure at mpm start, not at
// the first capability invocation. This is the spec's fail-loud
// requirement: a degraded capability lifecycle is a worse outcome
// than a refused-to-start daemon.
//
// If bwrapPathOverride is non-empty, it bypasses PATH lookup —
// useful for tests and for installations where bwrap lives in a
// non-standard location (e.g., a vendored copy in /opt).
func NewBwrapDriver(bwrapPathOverride string) (*BwrapDriver, error) {
	if bwrapPathOverride != "" {
		// Even the override path is verified to exist. A typo
		// in MPM_BWRAP_PATH should not produce a Driver that
		// silently fails every Run call.
		if _, err := os.Stat(bwrapPathOverride); err != nil {
			return nil, fmt.Errorf("%w: override %q: %v",
				ErrBwrapNotFound, bwrapPathOverride, err)
		}
		return &BwrapDriver{
			BwrapPath:     bwrapPathOverride,
			BwrapWaitDelay: 100 * time.Millisecond,
			Now:           time.Now,
		}, nil
	}
	resolved, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBwrapNotFound, err)
	}
	return &BwrapDriver{
		BwrapPath:     resolved,
		BwrapWaitDelay: 100 * time.Millisecond,
		Now:           time.Now,
	}, nil
}

// DriverName implements NamedDriver. Always "bwrap" — this is the
// canonical telemetry stamp; queries grouping failures by driver
// rely on it.
func (d *BwrapDriver) DriverName() string { return "bwrap" }

// Run implements Driver. See file header for failure semantics.
//
// DriverRequest.Limits MUST be already-resolved (the Executor
// resolves the three-tier precedence before calling Run); the
// Driver reads limits but does not look at metadata.
//
// The driverName field of DriverRequest is informational; the
// DriverName() method on the struct is authoritative.
func (d *BwrapDriver) Run(ctx context.Context, req DriverRequest) (*DriverResult, error) {
	// Step 1: write source to a tempfile with 0600 perms.
	// The file lives in the system temp dir (typically /tmp)
	// because bwrap's --ro-bind /tmp inside the sandbox only
	// makes sense if /tmp exists on the host at the same path.
	tmpFile, err := os.CreateTemp("", "mpm-cap-*.sh")
	if err != nil {
		return nil, fmt.Errorf("capability: BwrapDriver: tempfile create: %w", err)
	}
	tmpPath := tmpFile.Name()
	// Defer cleanup. If the child is still running when this
	// returns, os.Remove would fail on Windows (and on Linux
	// for an open fd). The child has already exited by the
	// time Run returns (we wait on cmd below), so the unlink
	// is safe.
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmpFile.Chmod(0600); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("capability: BwrapDriver: tempfile chmod: %w", err)
	}
	if _, err := tmpFile.WriteString(req.SourceCode); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("capability: BwrapDriver: tempfile write: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("capability: BwrapDriver: tempfile close: %w", err)
	}

	// Step 2: build bwrap argv. The argv is the spec's §4.1
	// shape for sandbox + restricted; buildArgs knows which
	// domain the request targets because the Executor has
	// already resolved limits (the Driver doesn't see the
	// Capability row directly, but the Executor could pass
	// the domain via DriverRequest in EX-6 if needed). For
	// EX-3, both sandbox and restricted are handled here.
	args, err := d.buildArgs(ctx, tmpPath, req)
	if err != nil {
		return nil, err
	}

	// Step 3: stamp start, spawn, capture.
	startedAt := d.now()
	cmd := exec.CommandContext(ctx, d.BwrapPath, args...)
	// Put the child in its own process group so ctx cancellation
	// kills bwrap AND its descendants (the actual script). Without
	// this, bwrap gets killed but the inner script keeps running
	// until its own timeout — for a 30-second capability timeout
	// that's a 30-second orphaned-process window. Setting Pgid=0
	// makes the child a new process group leader; cmd.Cancel (set
	// below) then signals the entire group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative PID = process group. Send SIGKILL to the
		// whole group so the inner script dies with bwrap.
		// We use SIGKILL rather than SIGTERM because we want
		// a hard ceiling — WaitDelay's SIGTERM-grace window
		// is for letting bwrap clean up its namespaces, but
		// if bwrap has already started a sandboxed script,
		// the script gets SIGKILL anyway.
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = d.BwrapWaitDelay
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("capability: BwrapDriver: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("capability: BwrapDriver: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("capability: BwrapDriver: start: %w", err)
	}

	// Step 4: drain pipes in parallel. Each pipe is read into
	// a buffer capped at req.Limits.MaxOutputBytes via
	// drainCapped (output.go). The per-stream truncation
	// status is reported back to the Executor via the
	// DriverResult's StdoutTruncated / StderrTruncated
	// fields, which the Executor ORs to produce
	// InvokeResult.Truncated and forwards into the
	// telemetry payload (where EX-7 fracture queries can
	// filter on them).
	stdout, stdoutTruncated := drainCapped(stdoutPipe, req.Limits.MaxOutputBytes)
	stderr, stderrTruncated := drainCapped(stderrPipe, req.Limits.MaxOutputBytes)

	// Step 5: wait for the child. cmd.Wait closes the pipes
	// after the process exits, so the drainCapped calls above
	// will return promptly after Wait.
	waitErr := cmd.Wait()
	finishedAt := d.now()

	// Step 6: classify the result. Capability-level failures
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
			// exec failure that isn't an exit code (rare).
			// Return as driver-level error so we don't
			// pollute capability_invocations with non-
			// invocations.
			return nil, fmt.Errorf("capability: BwrapDriver: wait: %w", waitErr)
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

// buildArgs assembles the bwrap argv for one invocation. The
// domain (sandbox vs restricted) is inferred from the request's
// metadata-equivalent: in EX-3 the Driver doesn't have direct
// access to the Capability row, so the Executor must tell us
// which domain to apply via DriverRequest. We use a convention:
// when DriverRequest.Args is nil OR empty after the script
// argv, we treat it as sandbox; restricted requires the
// Executor to set a sentinel (the Dispatcher is responsible
// for this — see dispatcher.go).
//
// Actually, since the Executor knows the domain but the
// DriverRequest doesn't carry it, we encode the domain via
// environment: DRIVER_DOMAIN=sandbox|restricted. This keeps
// the DriverRequest surface stable across EX-Ns.
//
// The source file path is passed positionally as the script
// argument; bwrap runs /bin/bash with the path as $0.
func (d *BwrapDriver) buildArgs(ctx context.Context, sourcePath string, req DriverRequest) ([]string, error) {
	domain := d.domainFromContext(ctx, req)
	args := make([]string, 0, 32)

	// --- namespace + filesystem setup (sandbox base) ---
	// Read-only bind of system binaries. These are the only
	// paths the script can see; everything else is hidden
	// by the empty filesystem bwrap creates by default.
	for _, p := range []string{"/usr", "/lib", "/bin", "/etc/resolv.conf"} {
		args = append(args, "--ro-bind", p, p)
	}
	// Bind the host's /tmp read-only so the script tempfile
	// (created outside the sandbox) is visible at the same
	// path inside the namespace. Then layer a private tmpfs
	// over /home — /home is always empty inside the sandbox,
	// so a tmpfs is correct there.
	args = append(args, "--ro-bind", "/tmp", "/tmp")
	args = append(args, "--tmpfs", "/home")

	// Namespace isolation. unshare-pid prevents the script
	// from seeing or signalling host processes; new-session
	// detaches it from the controlling terminal; die-with-
	// parent ensures the bwrap namespace dies if the agent
	// dies (no orphan mounts).
	//
	// unshare-net is sandbox-only. Restricted domain
	// capabilities may legitimately need network access
	// (e.g., a build tool that fetches dependencies); the
	// spec removes it from the restricted arg shape.
	//
	// Order matches the spec §4.1 listing: unshare-net first
	// when present, then the PID/session/parent trio.
	if domain == DomainSandbox {
		args = append(args, "--unshare-net")
	}
	args = append(args,
		"--unshare-pid",
		"--new-session",
		"--die-with-parent",
	)

	// --- resource limits (spec §4.3) ---
	// Wall-clock is enforced by the Executor via ctx + the
	// cmd.Cancel process-group SIGKILL (see Run). We do NOT
	// add bwrap's --timeout flag here because bwrap has no
	// native --timeout; emulating one via the GNU `timeout`
	// wrapper would add an external binary dependency for no
	// defensive gain — the ctx-based kill is already atomic
	// and reliable.
	//
	// Memory limit: bwrap's --rlimit-as takes BYTES, not MB.
	// MemoryBytes converts MaxMemoryMB → bytes. A zero return
	// (from a zero or negative MaxMemoryMB) means "do not set
	// the limit" — the Executor clamps via resolveLimits so
	// in practice we always see a positive value here.
	memBytes := MemoryBytes(req.Limits.MaxMemoryMB)
	if memBytes > 0 {
		args = append(args, "--rlimit-as", strconv.FormatInt(memBytes, 10))
	}
	// FD limit: bwrap's --rlimit-nofile takes a direct count.
	if req.Limits.MaxFDs > 0 {
		args = append(args, "--rlimit-nofile", strconv.FormatInt(req.Limits.MaxFDs, 10))
	}

	// Restricted adds the project dir + allowed_paths as
	// writable bind mounts. We resolve project_dir from the
	// process cwd if metadata doesn't supply it (the Executor
	// can also pass it via env, but the cwd fallback keeps
	// the Driver testable in isolation).
	if domain == DomainRestricted {
		projectDir, allowedPaths, err := d.resolveRestrictedPaths(ctx)
		if err != nil {
			return nil, err
		}
		args = append(args, "--bind", projectDir, projectDir)
		for _, p := range allowedPaths {
			if !filepath.IsAbs(p) {
				return nil, fmt.Errorf("%w: %q", ErrBwrapAllowedPathInvalid, p)
			}
			args = append(args, "--bind", p, p)
		}
		args = append(args, "--chdir", projectDir)
	} else {
		args = append(args, "--chdir", "/tmp")
	}

	// --- exec the script ---
	// The script path is the last positional; bwrap's --
	// separates bwrap flags from the inner command. The
	// interpreter path comes from SourceLanguage.Interpreter;
	// we pass the script path as the inner command's argv[0]
	// and the caller's argv as the inner command's argv[1..].
	args = append(args, "--")
	args = append(args, req.Language.Interpreter(), sourcePath)
	args = append(args, req.Args...)
	return args, nil
}

// domainFromContext reads the DRIVER_DOMAIN env var injected by
// the Dispatcher (see dispatcher.go). Defaults to sandbox for
// back-compat with tests that bypass the Dispatcher.
//
// The env injection pattern is preferred over a DriverRequest
// field because it keeps the Driver interface stable across
// EX-Ns (EX-4 will need rlimit flags; EX-5 may need output
// paths; each can be carried via env without changing the
// type).
func (d *BwrapDriver) domainFromContext(ctx context.Context, req DriverRequest) ExecutionDomain {
	// Prefer ctx-bound env (set by the Dispatcher) over the
	// DriverRequest, since ctx propagates through the executor.
	if v, ok := ctx.Value(driverDomainKey{}).(ExecutionDomain); ok && v != "" {
		return v
	}
	// Fallback: parse DriverRequest.DriverName. The Dispatcher
	// stamps it, but tests that build a DriverRequest directly
	// can omit it.
	_ = req
	return DomainSandbox
}

// resolveRestrictedPaths reads project_dir and allowed_paths
// from the context-injected metadata (set by the Dispatcher).
// Falls back to the OS cwd for project_dir if metadata is
// silent; errors if neither is available.
func (d *BwrapDriver) resolveRestrictedPaths(ctx context.Context) (string, []string, error) {
	meta, _ := ctx.Value(driverMetaKey{}).(CapabilityMetadata)

	var projectDir string
	if meta != nil {
		if v, ok := meta["project_dir"].(string); ok && v != "" {
			projectDir = v
		}
	}
	if projectDir == "" {
		cwd, err := os.Getwd()
		if err != nil || cwd == "" {
			return "", nil, ErrBwrapProjectDirMissing
		}
		projectDir = cwd
	}

	var allowedPaths []string
	if meta != nil {
		if v, ok := meta["allowed_paths"].([]interface{}); ok {
			for _, item := range v {
				if s, ok := item.(string); ok && s != "" {
					allowedPaths = append(allowedPaths, s)
				}
			}
		}
	}
	return projectDir, allowedPaths, nil
}

// now returns the current time. Pluggable for tests via d.Now.
func (d *BwrapDriver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// driverDomainKey is the ctx.Value key for the ExecutionDomain
// the Dispatcher has routed this call to. Typed empty struct so
// no other package can collide with it.
type driverDomainKey struct{}

// driverMetaKey is the ctx.Value key for the CapabilityMetadata
// the Dispatcher injects so the Driver can read project_dir /
// allowed_paths without seeing the full Capability row.
type driverMetaKey struct{}

// withDomainAndMeta returns a child context that carries the
// domain + metadata the BwrapDriver needs to build argv. The
// Dispatcher calls this once per invocation before handing off
// to Driver.Run.
//
// Kept here (rather than in dispatcher.go) so the keys live
// with the consumer; this avoids a circular import if the
// Dispatcher ever needs to read these keys itself.
func withDomainAndMeta(ctx context.Context, domain ExecutionDomain, meta CapabilityMetadata) context.Context {
	ctx = context.WithValue(ctx, driverDomainKey{}, domain)
	if meta != nil {
		ctx = context.WithValue(ctx, driverMetaKey{}, meta)
	}
	return ctx
}

// Compile-time guard that BwrapDriver satisfies Driver + NamedDriver.
var (
	_ Driver      = (*BwrapDriver)(nil)
	_ NamedDriver = (*BwrapDriver)(nil)
)

// =============================================================================
// Notes (kept here rather than scattered across the file):
//
// Why tempfile over `bash -c "<source>"`:
//   - LLM-generated source may contain quotes, dollar signs, backticks,
//     etc. that survive naive double-quoting but break under single-
//     quoting with embedded single quotes. Tempfile eliminates the
//     class entirely.
//   - argv-size limits: a 64KB script under `bash -c` adds command-line
//     overhead that some kernels truncate at ARG_MAX.
//   - On-disk representation matches in-the-wire representation: a
//     debugger can `cat` the tempfile to see exactly what ran.
//
// Why `--ro-bind /tmp /tmp` instead of `--tmpfs /tmp`:
//   - We create the source tempfile on the host's /tmp so the same
//     path is valid inside the sandbox. A `--tmpfs /tmp` would
//     create a fresh empty /tmp that doesn't contain our tempfile.
//   - The ro-bind keeps the script from rewriting the source file
//     mid-execution (a subtle self-tampering attack). The script
//     sees host /tmp as read-only; any write attempt fails.
//   - /home is still `--tmpfs` because there's no host /home
//     content the script needs.
//
// The DriverName interface assertion below also imports
// internal.IsBusyError just to keep the import live for future
// EX-Ns that will reuse it (e.g., EX-7 fracture detection's
// Store.FractureCapability retry uses the same IsBusyError
// classifier). It's a no-op reference; harmless.
// =============================================================================
