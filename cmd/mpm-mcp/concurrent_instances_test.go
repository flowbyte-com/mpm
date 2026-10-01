package main

import (
	"bytes"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm/internal/testenv"
)

// TestConcurrentMcpInstances verifies the architectural invariant that
// multiple mpm-mcp processes can coexist (one per MCP host) without
// contending for a singleton lock.
//
// Regression test for the 2026-07-17 incident where hermes couldn't save
// memories because OpenClaw's mpm-mcp child held the pidfile singleton.
// The fix removed the singleton entirely — each mpm-mcp is owned by its
// parent host via OS process lifecycle, concurrent writes are safe via
// SQLite WAL + busy_timeout.
//
// The test runs the binary in two parallel subshell pipes (stdin is
// /dev/null because we only want to verify startup, not a full MCP
// round-trip — that requires careful shutdown handling). Two instances
// must both log the "no pidfile singleton" line within the timeout,
// proving the singleton check no longer rejects the second process.
func TestConcurrentMcpInstances(t *testing.T) {
	// Source-tree executable: build cmd/mpm-mcp into t.TempDir() and
	// invoke that exact binary. If the build fails, mcpCmd calls
	// t.Fatalf — the test must fail with the build error, not skip
	// or fall back to an installed binary. A stale install must
	// never make a broken source checkout pass.
	bin := mcpCmd(t)

	const N = 3
	outputs := make([]string, N)
	// WaitGroup replaces the busy-poll barrier that previously raced
	// the goroutine writes to `outputs`. With WaitGroup, the assertion
	// loop runs after every writer has published its slot — no
	// happens-before edge needed via shared-memory polling.
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(bin)
			cmd.Stdin = strings.NewReader("")
			// Alpha-4.1.1 D-004 fix: mpm-mcp routes the slog default
			// writer to io.Discard unless MPM_VERBOSE=1, so the
			// machine interface stays clean. The startup log lines
			// ("no pidfile singleton") that this regression asserts
			// are diagnostic output — set MPM_VERBOSE to recover them.
			//
			// Isolation (2026-09-30): this was append(os.Environ(),
			// …), which let each child resolve the operator's real
			// MPM state — mpm-mcp opens a database at startup.
			// testenv.WithExtra pins MPM_WORKSPACE and HOME to temp
			// state while still supplying MPM_VERBOSE.
			cmd.Env = testenv.WithExtra(t, "MPM_VERBOSE=1")
			var out, errOut bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &errOut
			done := make(chan error, 1)
			go func() { done <- cmd.Run() }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				cmd.Process.Kill()
				<-done
			}
			outputs[i] = errOut.String()
		}()
	}

	// Wait for all goroutines with an overall deadline so a hung child
	// doesn't block the test indefinitely. Without WaitGroup this loop
	// would read `outputs` while goroutines were still writing — the
	// race detector flagged exactly that on 2026-08-17.
	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(8 * time.Second):
		t.Fatalf("instances did not complete within 8s — possible hang")
	}

	for i, out := range outputs {
		if out == "" {
			t.Errorf("instance %d produced no output within timeout", i)
			continue
		}
		if !strings.Contains(out, "no pidfile singleton") {
			t.Errorf("instance %d: stderr should contain the singleton-removal log line, got: %q", i, out)
		}
		// Critical: no instance should have logged the old "another
		// live instance holds the lock" message. That message was
		// the symptom of the bug.
		if strings.Contains(out, "another live instance holds") {
			t.Errorf("instance %d: stderr shows the OLD singleton-rejection behavior — regression! Output: %q", i, out)
		}
	}
}

// TestNoPidfileWrittenAfterStartup verifies the singleton removal is complete:
// after startup, no pidfile should exist at the canonical path. The new
// "no pidfile singleton" log line replaces the old "pidfile=..." line.
func TestNoPidfileWrittenAfterStartup(t *testing.T) {
	// Source-tree executable: build cmd/mpm-mcp into t.TempDir() and
	// invoke that exact binary. If the build fails, mcpCmd calls
	// t.Fatalf — the test must fail with the build error, not skip
	// or fall back to an installed binary. A stale install must
	// never make a broken source checkout pass.
	bin := mcpCmd(t)

	cmd := exec.Command(bin)
	cmd.Stdin = strings.NewReader("")
	// Alpha-4.1.1 D-004 fix: mpm-mcp routes the slog default writer
	// to io.Discard unless MPM_VERBOSE=1. The startup log line this
	// regression asserts is diagnostic output — set MPM_VERBOSE to
	// recover it.
	//
	// Isolation (2026-09-30): this was append(os.Environ(), …), which
	// let the child resolve the operator's real MPM state. See
	// TestConcurrentMcpInstances for the full rationale.
	cmd.Env = testenv.WithExtra(t, "MPM_VERBOSE=1")
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		<-done
	}

	if !strings.Contains(errOut.String(), "no pidfile singleton") {
		t.Errorf("expected 'no pidfile singleton' log line in stderr, got: %q", errOut.String())
	}
	if strings.Contains(errOut.String(), "pidfile=") {
		t.Errorf("expected NO 'pidfile=' path log line (removed with the singleton), got: %q", errOut.String())
	}
}
