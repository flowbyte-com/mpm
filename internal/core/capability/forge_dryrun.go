package capability

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// =============================================================================
// forge_dryrun.go — capability dry-run sandbox (spec §3.2 step 9)
//
// Dry-run is the final check before a proposal is inserted. It
// runs the source code in a bwrap sandbox and asserts that the
// process exits 0 within the timeout. The sandbox is the only
// thing standing between a malicious or buggy proposal and a
// live capability row.
//
// Why bwrap (not Docker, not firejail, not chroot):
//
//   * Available on every mainstream Linux distro (apt: bubblewrap,
//     dnf: bwrap, pacman: bubblewrap). One tool, one dependency.
//   * Namespace-based, not full-VM. The proposal runs in
//     unshared-pid + unshare-net + read-only root + tmpfs for
//     /tmp and /var/tmp. No filesystem writes leak to the host.
//   * No daemon. bwrap execs and waits; the Forge just shells
//     out to it.
//   * `--die-with-parent` ensures a runaway proposal cannot
//     outlive the Forge invocation.
//
// What this file provides:
//
//   * DryRunner interface — pluggable for tests
//   * BwrapDryRunner — production implementation (shells out)
//   * FakeDryRunner — test implementation
//
// F-6 will provide the orchestration that consumes a DryRunner
// to run proposed capabilities as part of the Forge pipeline.
// =============================================================================

// DryRunResult is the outcome of one dry-run attempt. Pass=true
// means the source executed cleanly (exit 0 within timeout).
// The Forge rejects any proposal whose DryRunResult.Pass=false.
type DryRunResult struct {
	Pass    bool
	Reason  string // short failure reason (first stderr line, or "timeout")
	Stderr  string // full stderr for the rejection detail
	Stdout  string // for diagnostics; not used by the Forge
	Elapsed time.Duration
}

// DryRunner is the interface. The Forge uses this so tests can
// inject a fake and avoid the bwrap dependency.
type DryRunner interface {
	// Run executes the source under the configured sandbox. The
	// implementation MUST honour ctx cancellation — a runaway
	// proposal must not be able to lock up the Forge.
	//
	// The source is written to a temp file; the sandbox mounts
	// it read-only along with the language interpreter. The
	// source is NEVER passed as a command-line argument (which
	// would be subject to shell escaping bugs).
	Run(ctx context.Context, language, source string) (DryRunResult, error)
}

// BwrapConfig lets the operator tune the bwrap invocation. The
// defaults are spec §3.2 step 9: read-only root, tmpfs /tmp and
// /var/tmp, unshared pid + net, die-with-parent.
type BwrapConfig struct {
	BwrapBinary      string        // default: "bwrap"
	BashBinary       string        // default: "/bin/bash"
	PythonBinary     string        // default: "/usr/bin/python3"
	JqBinary         string        // default: "/usr/bin/jq"
	Timeout          time.Duration // default: 30s
	AllowNetAccess   bool          // default: false (unshare-net)
	MountReadOnlyRoot bool         // default: true
}

// DefaultBwrapConfig returns the spec §3.2 defaults. Operators
// can override AllowNetAccess for the trusted-domain case where
// the proposal needs to call an API.
func DefaultBwrapConfig() BwrapConfig {
	return BwrapConfig{
		BwrapBinary:       "bwrap",
		BashBinary:        "/bin/bash",
		PythonBinary:      "/usr/bin/python3",
		JqBinary:          "/usr/bin/jq",
		Timeout:           30 * time.Second,
		AllowNetAccess:    false,
		MountReadOnlyRoot: true,
	}
}

// BwrapDryRunner is the production DryRunner. It writes the
// source to a temp file, then shells out to bwrap with the
// appropriate language interpreter.
//
// The bwrap invocation is constructed with --die-with-parent so
// that a runaway proposal cannot outlive the Forge. We use
// --unshare-pid --unshare-net for namespace isolation; the
// proposal sees a fresh /proc and has no network.
//
// Note: an installed bwrap is a precondition. If it's missing,
// every Run returns an infrastructure error — the Forge must
// treat that as a hard reject (fail-closed for toolchain).
type BwrapDryRunner struct {
	cfg BwrapConfig
}

// NewBwrapDryRunner builds a BwrapDryRunner with the given
// config. A zero-value config is filled in with DefaultBwrapConfig().
func NewBwrapDryRunner(cfg BwrapConfig) *BwrapDryRunner {
	def := DefaultBwrapConfig()
	if cfg.BwrapBinary == "" {
		cfg.BwrapBinary = def.BwrapBinary
	}
	if cfg.BashBinary == "" {
		cfg.BashBinary = def.BashBinary
	}
	if cfg.PythonBinary == "" {
		cfg.PythonBinary = def.PythonBinary
	}
	if cfg.JqBinary == "" {
		cfg.JqBinary = def.JqBinary
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = def.Timeout
	}
	// Boolean fields: zero value is false, but the spec defaults
	// are (MountReadOnlyRoot=true, AllowNetAccess=false). Apply
	// the defaults only when the caller hasn't set them. The
	// "caller hasn't set them" signal is harder for booleans —
	// we use a sentinel convention: a zero-value BwrapConfig
	// means "apply all defaults". If the caller wants to opt
	// out, they can pass a BwrapConfig{AllowNetAccess: true}
	// explicitly. The mount-read-only default is applied
	// because it's the more secure choice.
	if !cfg.MountReadOnlyRoot {
		cfg.MountReadOnlyRoot = def.MountReadOnlyRoot
	}
	// AllowNetAccess is allowed to be either value; we don't
	// override it. (If you really want to default it, pass
	// BwrapConfig{AllowNetAccess: false} explicitly.)
	return &BwrapDryRunner{cfg: cfg}
}

// Run writes the source to a temp file, invokes bwrap with the
// appropriate interpreter, and returns the outcome. The temp
// file is removed on every exit path.
func (r *BwrapDryRunner) Run(ctx context.Context, language, source string) (DryRunResult, error) {
	c, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	// Write source to a temp file. The pattern includes the
	// language suffix so the file is recognisable if it leaks
	// (e.g. bwrap is missing and the file is left behind).
	suffix := "." + language
	tmpFile, err := os.CreateTemp("", "mpm-dryrun-*"+suffix)
	if err != nil {
		return DryRunResult{}, fmt.Errorf("capability: dryrun: create temp: %w", err)
	}
	defer os.Remove(tmpFile.Name()) //nolint:errcheck

	if _, err := tmpFile.WriteString(source); err != nil {
		tmpFile.Close()
		return DryRunResult{}, fmt.Errorf("capability: dryrun: write temp: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return DryRunResult{}, fmt.Errorf("capability: dryrun: close temp: %w", err)
	}

	// Build the bwrap command. The argv order matters: --ro-bind
	// and --tmpfs MUST come before -- to set up the namespace;
	// the interpreter and source MUST come after --.
	interp, interpArgs, err := r.interpreterFor(language, tmpFile.Name())
	if err != nil {
		return DryRunResult{}, err
	}

	args := []string{}
	if r.cfg.MountReadOnlyRoot {
		args = append(args, "--ro-bind", "/", "/")
	}
	args = append(args,
		"--tmpfs", "/tmp",
		"--tmpfs", "/var/tmp",
		"--dev", "/dev",
		"--proc", "/proc",
		"--unshare-pid",
		"--die-with-parent",
	)
	if !r.cfg.AllowNetAccess {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--", interp)
	args = append(args, interpArgs...)

	start := time.Now()
	cmd := exec.CommandContext(c, r.cfg.BwrapBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	elapsed := time.Since(start)

	// Context cancellation → timeout. Surface as a distinct
	// reason so the rejection is "proposal exceeded time limit"
	// not "proposal exited 1".
	if c.Err() == context.DeadlineExceeded {
		return DryRunResult{
			Pass:    false,
			Reason:  "timeout",
			Stderr:  stderr.String(),
			Stdout:  stdout.String(),
			Elapsed: elapsed,
		}, nil
	}

	if err == nil {
		return DryRunResult{
			Pass:    true,
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
			Elapsed: elapsed,
		}, nil
	}

	// Non-zero exit. First stderr line is the most useful signal.
	reason := firstLine(stderr.String())
	if reason == "" {
		reason = fmt.Sprintf("exit %v", err)
	}
	return DryRunResult{
		Pass:    false,
		Reason:  reason,
		Stderr:  stderr.String(),
		Stdout:  stdout.String(),
		Elapsed: elapsed,
	}, nil
}

// interpreterFor returns the interpreter path and any args to
// pass it, for the given language. Bash and python take the
// source file as a positional arg. Jq takes -f to read from a
// file; the input is null (`echo -n null | bwrap ... -- jq -f
// <source>` would work, but bwrap's stdin comes from outside
// the namespace — easier to pass `null` as a literal argv).
func (r *BwrapDryRunner) interpreterFor(language, sourcePath string) (string, []string, error) {
	switch language {
	case "bash":
		return r.cfg.BashBinary, []string{sourcePath}, nil
	case "python":
		return r.cfg.PythonBinary, []string{sourcePath}, nil
	case "jq":
		// jq -f <file> reads the filter from a file. We pass
		// "null" as a positional arg to give jq a deterministic
		// input (`jq` on its own would read from stdin, but in
		// the bwrap namespace stdin is detached — null as a
		// literal is more reproducible than relying on a tty).
		return r.cfg.JqBinary, []string{"-f", sourcePath, "null"}, nil
	default:
		return "", nil, fmt.Errorf("capability: dryrun: unknown language %q", language)
	}
}

// =============================================================================
// FakeDryRunner — test-only DryRunner.
// =============================================================================

// FakeDryRunner is a programmable DryRunner for tests. Every Run
// returns the next pre-canned result/error from the queues. If
// the queues are empty, returns Pass=true (the "everything is
// fine" default).
type FakeDryRunner struct {
	// Results is the queue of canned results. Run consumes from
	// the front; if empty, returns Pass=true.
	Results []DryRunResult
	// Errors is the queue of infrastructure errors. Run consumes
	// from the front; if empty, returns nil error.
	Errors []error
	// Calls records every (language, source) tuple for assertions.
	Calls []FakeDryRunnerCall
}

// FakeDryRunnerCall records one invocation.
type FakeDryRunnerCall struct {
	Language string
	Source   string
}

// Run returns the next pre-canned result/error and records the call.
func (f *FakeDryRunner) Run(_ context.Context, language, source string) (DryRunResult, error) {
	f.Calls = append(f.Calls, FakeDryRunnerCall{Language: language, Source: source})
	var result DryRunResult
	if len(f.Results) > 0 {
		result = f.Results[0]
		f.Results = f.Results[1:]
	} else {
		result = DryRunResult{Pass: true}
	}
	var err error
	if len(f.Errors) > 0 {
		err = f.Errors[0]
		f.Errors = f.Errors[1:]
	}
	return result, err
}
