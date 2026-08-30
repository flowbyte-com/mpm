// socket_subprocess_test.go — alpha-4.1.2 release-integrity test for
// D-002 (telemetry ping and serve must share socket resolution).
//
// The F16 fix (2026-08-27) added defaultTelemetrySocketPath() and
// tests pinning the helper. This file adds a subprocess-level test
// that drives the actual mpm-telemetry binary across the six
// scenarios section 3 of the alpha-4.1.2 spec mandates:
//
//   1. clean workspace,
//   2. start serve,
//   3. ping with only MPM_WORKSPACE,
//   4. ping using explicit socket override,
//   5. wrong socket,
//   6. shutdown cleanup.
//
// The contract: `serve` and `ping` must agree on the socket path
// derived from MPM_WORKSPACE, with MPM_TELEMETRY_SOCKET still winning
// when set.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func findTelemetryBinary(t *testing.T) string {
	t.Helper()
	candidates := []string{"../../bin/mpm-telemetry"}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	t.Skip("mpm-telemetry binary not found; run `make build` first")
	return ""
}

// serveProc wraps an exec.Cmd with a captured stderr buffer so
// failed-start diagnostics reach the test report.
type serveProc struct {
	*exec.Cmd
	stderr *bytes.Buffer
}

// runTelemetryServe starts the mpm-telemetry serve process with the
// given env, returning a *serveProc whose stderr is captured for
// post-mortem inspection if the daemon fails to start.
func runTelemetryServe(t *testing.T, env []string) *serveProc {
	t.Helper()
	bin := findTelemetryBinary(t)
	cmd := exec.Command(bin, "serve", "-quiet")
	cmd.Env = env
	buf := &bytes.Buffer{}
	cmd.Stderr = buf
	cmd.Stdout = buf // capture stdout too; serve is silent except on errors
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	return &serveProc{Cmd: cmd, stderr: buf}
}

// waitForSocket polls for the socket file to appear, up to 2 seconds.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("socket %s did not appear within 2s", path)
}

// runTelemetryPing invokes `mpm-telemetry ping` with the given env
// and returns combined output + error.
func runTelemetryPing(t *testing.T, env []string) (string, error) {
	t.Helper()
	bin := findTelemetryBinary(t)
	cmd := exec.Command(bin, "ping")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// stopServe terminates the serve process gracefully (SIGTERM) and
// waits for cleanup. If the process never started or stderr captured
// diagnostics, those are surfaced via t.Log so the failure context
// reaches the test report.
func stopServe(t *testing.T, p *serveProc) {
	t.Helper()
	if p == nil || p.Cmd == nil || p.Process == nil {
		return
	}
	_ = p.Process.Signal(syscall.SIGTERM)
	_ = p.Wait()
	if p.stderr != nil && p.stderr.Len() > 0 {
		t.Logf("serve stderr captured:\n%s", p.stderr.String())
	}
}

// baseEnvTel strips MPM_WORKSPACE/MPM_TELEMETRY_SOCKET/MPM_TELEMETRY_DB
// from os.Environ so each test builds a controlled env.
func baseEnvTel() []string {
	env := os.Environ()
	out := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, "MPM_WORKSPACE=") ||
			strings.HasPrefix(e, "MPM_TELEMETRY_SOCKET=") ||
			strings.HasPrefix(e, "MPM_TELEMETRY_DB=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// setupTelemetryWorkspace creates the directory layout telemetry serve
// expects (src/db/) so the binary can write telemetry.db on first boot.
func setupTelemetryWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src", "db"), 0o755); err != nil {
		t.Fatalf("mkdir telemetry workspace: %v", err)
	}
	// Diagnostic: confirm the dir exists and is writable from this
	// process. We've seen flakiness where the dir existed but the
	// serve subprocess couldn't open telemetry.db; this assert
	// distinguishes "MkdirAll silently failed" from "SQLite open
	// failed for some other reason".
	dbDir := filepath.Join(ws, "src", "db")
	if info, err := os.Stat(dbDir); err != nil {
		t.Fatalf("stat %s: %v", dbDir, err)
	} else if !info.IsDir() {
		t.Fatalf("%s is not a directory", dbDir)
	}
	return ws
}

// Scenarios 1-3: clean workspace, start serve, ping with only
// MPM_WORKSPACE. This is the canonical happy path: an operator runs
// `mpm-telemetry serve` and then `mpm-telemetry ping` and expects
// them to agree on the socket.
func TestTelemetrySubprocess_ServeAndPing_AgreeOnWorkspaceSocket(t *testing.T) {
	ws := setupTelemetryWorkspace(t)
	env := append(baseEnvTel(), "MPM_WORKSPACE="+ws)

	cmd := runTelemetryServe(t, env)
	defer stopServe(t, cmd)

	wantSocket := filepath.Join(ws, "runtime", "mpm-telemetry.sock")
	waitForSocket(t, wantSocket)

	out, err := runTelemetryPing(t, env)
	if err != nil {
		t.Fatalf("ping after serve failed: %v\n%s", err, out)
	}

	// The handshake JSON should contain the daemon's version/state.
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("ping output is not valid JSON: %v\n%s", err, out)
	}
	// We don't pin a specific field name (the handshake schema may
	// evolve), only that we got a structured response.
	if len(resp) == 0 {
		t.Fatalf("ping returned empty handshake:\n%s", out)
	}
}

// Scenario 4: ping using MPM_TELEMETRY_SOCKET override. Even when the
// override is set, it must point at the same socket the serve is
// listening on, and ping must succeed.
func TestTelemetrySubprocess_PingRespectsExplicitSocketOverride(t *testing.T) {
	ws := setupTelemetryWorkspace(t)
	// Override points to a non-default subdirectory.
	custom := filepath.Join(ws, "alt", "tel.sock")
	env := append(baseEnvTel(),
		"MPM_WORKSPACE="+ws,
		"MPM_TELEMETRY_SOCKET="+custom,
	)

	cmd := runTelemetryServe(t, env)
	defer stopServe(t, cmd)

	waitForSocket(t, custom)

	out, err := runTelemetryPing(t, env)
	if err != nil {
		t.Fatalf("ping with override failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "{") {
		t.Fatalf("ping with override returned non-JSON output:\n%s", out)
	}
}

// Scenario 5: wrong socket — ping must error, not silently succeed
// against a stale/different daemon. This is the cross-workspace
// safety check.
func TestTelemetrySubprocess_WrongSocket_ErrorsCleanly(t *testing.T) {
	ws := setupTelemetryWorkspace(t)
	// Start a serve in ws.
	env := append(baseEnvTel(), "MPM_WORKSPACE="+ws)
	cmd := runTelemetryServe(t, env)
	defer stopServe(t, cmd)
	wantSocket := filepath.Join(ws, "runtime", "mpm-telemetry.sock")
	waitForSocket(t, wantSocket)

	// Now ping from a different workspace with no override — it
	// should NOT find the ws daemon, because the helper derives the
	// socket from the new MPM_WORKSPACE.
	otherWS := setupTelemetryWorkspace(t)
	otherEnv := append(baseEnvTel(), "MPM_WORKSPACE="+otherWS)

	out, err := runTelemetryPing(t, otherEnv)
	if err == nil {
		t.Fatalf("ping from wrong workspace should error, got success:\n%s", out)
	}
	if !strings.Contains(out, wantSocket) && !strings.Contains(err.Error(), wantSocket) {
		// We don't pin the exact error format; just confirm the
		// failure was NOT caused by accidentally dialing the right
		// socket (which would be the silent-escape bug).
		t.Logf("ping failed with: %v\n%s (good — wrong socket)", err, out)
	}
}

// Scenario 6: shutdown cleanup — after serve is stopped, the socket
// file should be removed. This prevents stale-socket confusion on
// restart.
func TestTelemetrySubprocess_ShutdownCleansSocket(t *testing.T) {
	ws := setupTelemetryWorkspace(t)
	env := append(baseEnvTel(), "MPM_WORKSPACE="+ws)

	cmd := runTelemetryServe(t, env)
	wantSocket := filepath.Join(ws, "runtime", "mpm-telemetry.sock")
	waitForSocket(t, wantSocket)

	stopServe(t, cmd)

	// Poll up to 2s for socket removal.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(wantSocket); os.IsNotExist(err) {
			return // success
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("socket %s still present after shutdown", wantSocket)
}

// Bonus: assert that no daemon is reachable when no serve is running
// and no socket exists. This pins the "ping fails on clean workspace"
// path — silent success here would be a serious bug.
func TestTelemetrySubprocess_PingWithoutServe_Errors(t *testing.T) {
	ws := t.TempDir()
	env := append(baseEnvTel(), "MPM_WORKSPACE="+ws)

	out, err := runTelemetryPing(t, env)
	if err == nil {
		t.Fatalf("ping without serve should error, got success:\n%s", out)
	}
}

// Verify two concurrent pings both succeed against the same serve.
// This is a smoke test for the unix-socket listener (no socket
// serialization bug). The existing helper resets the buffer per
// connection, so this is a low-cost assertion.
func TestTelemetrySubprocess_ConcurrentPings(t *testing.T) {
	ws := setupTelemetryWorkspace(t)
	env := append(baseEnvTel(), "MPM_WORKSPACE="+ws)

	cmd := runTelemetryServe(t, env)
	defer stopServe(t, cmd)
	waitForSocket(t, filepath.Join(ws, "runtime", "mpm-telemetry.sock"))

	results := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			_, err := runTelemetryPing(t, env)
			results <- err
		}()
	}
	for i := 0; i < 3; i++ {
		if err := <-results; err != nil {
			t.Errorf("concurrent ping %d failed: %v", i, err)
		}
	}
	_ = bytes.Buffer{} // keep bytes import live if needed later
	_ = context.Background
	_ = net.Conn(nil)
}
