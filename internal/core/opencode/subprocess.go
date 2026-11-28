package opencode

// subprocess.go — the ONE place MPM starts a framework child process.
//
// # WHAT THIS IS
//
// Ordinary subprocess lifecycle management: a deadline, cancellation
// that respects an earlier caller cancellation, a bounded capture, and
// termination of the owned process group when the run is terminated.
// It is NOT a security sandbox: a process that deliberately leaves its
// process group is outside this accounting, and MPM does not claim
// otherwise. Nothing unrelated is ever signalled — the group here is the
// one the child was placed in at start-up.
//
// # WHY THE GROUP MATTERS
//
// `exec.CommandContext` kills only the DIRECT child when the context is
// done. A descendant that inherits the child's stdout/stderr keeps the
// output pipe open, so `Wait` would block until that descendant exits —
// which for a stuck agent framework can be far beyond the deadline the
// dispatcher budgeted for its lease. So the child starts in its own
// process group (where the OS supports it), cancellation terminates that
// group, and `WaitDelay` bounds the pipe copy either way.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	// childWaitDelay bounds how long Wait may keep waiting for the
	// child's I/O pipes after the process (or its group) has been
	// terminated. It is also what bounds a NORMAL exit whose framework
	// left a descendant holding the pipe: the run returns bounded, and
	// nothing is signalled (the framework chose to leave it running).
	childWaitDelay = 2 * time.Second

	// defaultReadTimeout bounds the read-only commands (session lists,
	// agent lists, message streams). A read is a short query, not a run.
	defaultReadTimeout = 30 * time.Second

	// defaultChildTimeout is the fallback when a caller passes no
	// deadline. Nothing in MPM reaches it in practice: the adapter and
	// the dispatcher always configure one.
	defaultChildTimeout = 30 * time.Minute
)

// childResult is what one owned subprocess run produced.
//
// IT IS SAFE TO CARRY INTO ERRORS, LOGS AND PERSISTED RECORDS: it holds
// the classification, the exit status and a digest of the captured
// output — never the output text, which may contain anything the
// framework printed, including credentials.
type childResult struct {
	// stdout and stderr are the captured streams, each bounded.
	stdout string
	stderr string
	// exitCode is meaningful only when started and reaped.
	exitCode int
	// started says the process was created at all.
	started bool
	// terminated says THIS run ended because its deadline or the
	// caller's cancellation fired — the condition under which the
	// process group is terminated.
	terminated bool
	// cancelKind records WHY the run was terminated: "deadline" when the
	// run's own deadline fired, "caller" when the caller's context was
	// cancelled. Empty when the run was not terminated.
	cancelKind string
	// waitErr is the SAFE classification of a failed run.
	waitErr error
	// combinedBytes/combinedDigest describe the COMBINED captured
	// output, for evidence-style diagnostics.
	combinedBytes  int
	combinedDigest string
}

// exitCodeOf maps a wait outcome to an exit status where reliably
// available.
func (c *childResult) exitCodeOf(ps *os.ProcessState) int {
	if ps == nil {
		return 0
	}
	return ps.ExitCode()
}

// classify turns a wait error into the safe LaunchFailureClass
// vocabulary the adapter already uses.
func classifyChild(cr *childResult) (LaunchFailureClass, bool) {
	switch {
	case cr.cancelKind == "caller":
		return LaunchCancelled, true
	case cr.cancelKind == "deadline":
		return LaunchDeadlineExceeded, true
	case !cr.started:
		return LaunchStartFailed, true
	case cr.exitCode != 0:
		return LaunchExitFailure, true
	}
	return "", false
}

// childConfig is one owned subprocess invocation.
type childConfig struct {
	bin     string
	args    []string
	dir     string
	timeout time.Duration
}

// runChild starts the child in its own process group (where supported),
// runs it under the deadline (respecting any earlier cancellation of the
// caller's context), captures bounded output, reaps it, and — only when
// the run was TERMINATED — kills the group so a surviving descendant
// cannot outlive the accounting.
//
// The returned result is safe to persist; it carries no raw output.
func runChild(ctx context.Context, cfg childConfig) (*childResult, error) {
	if cfg.bin == "" {
		return nil, errors.New("subprocess: no binary")
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultChildTimeout
	}
	// WithTimeout derives from the caller's context: an earlier
	// cancellation or deadline wins, and this deadline is finite either
	// way.
	runCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, cfg.bin, cfg.args...)
	setProcessGroup(cmd)
	// The group, not just the direct child, is terminated on
	// cancellation. exec calls Cancel when the context is done.
	cmd.Cancel = func() error {
		return signalProcessGroup(cmd, syscallSIGTERM)
	}
	// Even after termination, a descendant can hold the output pipe;
	// WaitDelay bounds that copy so Wait cannot block on it.
	cmd.WaitDelay = childWaitDelay

	cmd.Env = os.Environ()
	cmd.Dir = cfg.dir

	var out, errBuf boundedBuffer
	out.limit = maxCapturedOutput
	errBuf.limit = maxCapturedOutput
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	waitErr := cmd.Run()
	cr := &childResult{
		stdout:     out.String(),
		stderr:     errBuf.String(),
		started:    cmd.ProcessState != nil,
		terminated: runCtx.Err() != nil,
	}
	if cr.terminated {
		// Which fired first: the run's own deadline, or the caller's
		// cancellation. Both terminate the group; the class names the
		// reason.
		if errors.Is(runCtx.Err(), context.Canceled) {
			cr.cancelKind = "caller"
		} else {
			cr.cancelKind = "deadline"
		}
	}
	if cr.started {
		cr.exitCode = cmd.ProcessState.ExitCode()
	}
	combined := cr.stdout + cr.stderr
	cr.combinedBytes = len(combined)
	if combined != "" {
		sum := sha256.Sum256([]byte(combined))
		cr.combinedDigest = fmt.Sprintf("%x", sum)
	}
	cr.waitErr = classifyChildWait(cr, waitErr, cfg.timeout)

	// TERMINATION CLEANUP. Only when THIS run was terminated: a normal
	// exit leaves the framework's own descendants alone.
	if cr.terminated && cr.started {
		_ = terminateProcessGroup(cmd)
	}
	return cr, nil
}

// classifyChildWait renders the SAFE wait classification. The message
// names the class, the exit status where reliably available, and the
// bounded-output digest — never the captured text.
func classifyChildWait(cr *childResult, waitErr error, timeout time.Duration) error {
	if waitErr == nil {
		return nil
	}
	var class LaunchFailureClass
	var detail string
	switch {
	case cr.terminated:
		class = LaunchDeadlineExceeded
		detail = fmt.Sprintf(
			"the command did not finish within its %s deadline and its process group was terminated", timeout)
	case !cr.started:
		class = LaunchStartFailed
		detail = "the command could not be started"
	default:
		class = LaunchExitFailure
		detail = fmt.Sprintf("the command exited unsuccessfully (exit %d)", cr.exitCode)
	}
	msg := fmt.Sprintf("%s: %s", class, detail)
	if cr.combinedBytes > 0 {
		msg += fmt.Sprintf("; captured %d bytes (sha256:%s); raw output is not included",
			cr.combinedBytes, cr.combinedDigest)
	}
	return fmt.Errorf("%w", errors.New(msg))
}

// exitStatus reports the exit status where reliably available, for
// diagnostics that already have a childResult.
func (c *childResult) exitStatus() string {
	if !c.started {
		return "not started"
	}
	return fmt.Sprintf("exit %d", c.exitCode)
}

// readCommandError is the SAFE error for a failed read command. It wraps
// the child classification; it carries no captured text.
type readCommandError struct {
	bin  string
	wait error
}

func (e *readCommandError) Error() string {
	return fmt.Sprintf("%s: %v", e.bin, e.wait)
}

func (e *readCommandError) Unwrap() error { return e.wait }

// readBinShort is the short binary name for a read error.
func readBinShort(bin string) string { return filepath.Base(bin) }

// maxCapturedOutput bounds the runtime output the adapter keeps.
const maxCapturedOutput = 1 << 20

// boundedBuffer keeps at most limit bytes and silently discards the rest.
// It is used where the output is only digested, never interpreted.
type boundedBuffer struct {
	buf   []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	room := b.limit - len(b.buf)
	if room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf }

func (b *boundedBuffer) String() string { return string(b.buf) }

// digestOf is the SHA-256 hex digest used by the safe diagnostics.
func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}
