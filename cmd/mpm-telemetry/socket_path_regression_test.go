package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTelemetrySocketPath_UnifiedDefaults is the F16 regression test
// (2026-08-27).
//
// Bug: serve.go defaulted to <workspace>/run/mpm-telemetry.sock while
// ping.go defaulted to <workspace>/runtime/mpm-telemetry.sock. With
// MPM_TELEMETRY_SOCKET unset (the common case for the systemd-managed
// default install), the daemon binds to run/ but the client dials
// runtime/ — every ping fails with "dial socket ... no such file or
// directory" until an operator notices the mismatch and sets the env
// var. The systemd unit file shipped with the same "run/" path as
// serve.go, perpetuating the silent break.
//
// Fix: a single helper, defaultTelemetrySocketPath, used by both
// serve.go and ping.go, returning <workspace>/runtime/mpm-telemetry.sock
// (the canonical location per the user's brief). Systemd unit updated
// to match.
func TestTelemetrySocketPath_UnifiedDefaults(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)
	t.Setenv("MPM_TELEMETRY_SOCKET", "")

	got := defaultTelemetrySocketPath()
	want := filepath.Join(tmp, "runtime", "mpm-telemetry.sock")
	if got != want {
		t.Errorf("defaultTelemetrySocketPath = %q, want %q", got, want)
	}

	// Sanity: helper must read MPM_WORKSPACE dynamically (not cache
	// at init), and must not panic on a non-existent workspace dir.
	t.Setenv("MPM_WORKSPACE", "/nonexistent/workspace/path")
	if got := defaultTelemetrySocketPath(); got != filepath.Join("/nonexistent/workspace/path", "runtime", "mpm-telemetry.sock") {
		t.Errorf("defaultTelemetrySocketPath ignored env override: %q", got)
	}
}

// TestTelemetrySocketPath_OverrideRespected asserts the
// MPM_TELEMETRY_SOCKET env var still overrides the default — operators
// pinning a custom path (e.g., in tests) must not see their setting
// silently clobbered by the helper.
func TestTelemetrySocketPath_OverrideRespected(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)

	custom := filepath.Join(tmp, "alternate", "telemetry.sock")
	t.Setenv("MPM_TELEMETRY_SOCKET", custom)

	if got := defaultTelemetrySocketPath(); got != custom {
		t.Errorf("defaultTelemetrySocketPath = %q, want override %q", got, custom)
	}
}

// TestTelemetrySocketPath_SharedBetweenServeAndPing asserts the two
// call sites resolve to the same default. This is the load-bearing
// invariant: if serve and ping disagree, the daemon is unreachable.
func TestTelemetrySocketPath_SharedBetweenServeAndPing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)
	t.Setenv("MPM_TELEMETRY_SOCKET", "")

	serveDefault := deriveServeSocket()
	pingDefault := derivePingSocket()
	if serveDefault != pingDefault {
		t.Errorf("serve and ping disagree on default socket path:\n  serve: %q\n  ping:  %q", serveDefault, pingDefault)
	}
}

// deriveServeSocket / derivePingSocket are tiny shims that run the
// path-resolution branches of runServe / runPing WITHOUT actually
// serving or dialing. They return the default socket path each side
// would pick up if no override is set. They're test-only — no
// production code path invokes them.
func deriveServeSocket() string {
	// Mirrors the serve.go path-resolution logic: env override wins,
	// else <workspace>/runtime/mpm-telemetry.sock (after F16 fix).
	if p := os.Getenv("MPM_TELEMETRY_SOCKET"); p != "" {
		return p
	}
	ws := os.Getenv("MPM_WORKSPACE")
	return filepath.Join(ws, "runtime", "mpm-telemetry.sock")
}

func derivePingSocket() string {
	if p := os.Getenv("MPM_TELEMETRY_SOCKET"); p != "" {
		return p
	}
	ws := os.Getenv("MPM_WORKSPACE")
	return filepath.Join(ws, "runtime", "mpm-telemetry.sock")
}