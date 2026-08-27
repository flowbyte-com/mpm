package main

import (
	"os"
	"path/filepath"
)

// defaultTelemetrySocketPath returns the canonical socket path used by
// the mpm-telemetry daemon (serve) and its client subcommands (ping,
// observe, query, cost).
//
// F16 fix (2026-08-27): before this helper existed, serve.go defaulted
// to <workspace>/run/mpm-telemetry.sock and ping.go defaulted to
// <workspace>/runtime/mpm-telemetry.sock. The systemd unit pinned the
// "run" path. The default install bound the daemon to one path and
// dialed another — every `mpm-telemetry ping` failed with "no such
// file" until an operator noticed and set MPM_TELEMETRY_SOCKET.
//
// Canonical location: <workspace>/runtime/mpm-telemetry.sock
//
// MPM_TELEMETRY_SOCKET still wins when set, for operator overrides and
// tests. The helper does not cache — it reads MPM_WORKSPACE on every
// call so process reloads aren't required to pick up a new workspace.
func defaultTelemetrySocketPath() string {
	if p := os.Getenv("MPM_TELEMETRY_SOCKET"); p != "" {
		return p
	}
	ws := os.Getenv("MPM_WORKSPACE")
	if ws == "" {
		// Defensive: do not silently fall back to a hard-coded path.
		// Callers that need a default should set MPM_WORKSPACE; the
		// return-empty contract surfaces the misconfiguration to the
		// caller, who can decide whether to die (serve) or warn (ping).
		return ""
	}
	return filepath.Join(ws, "runtime", "mpm-telemetry.sock")
}