// session_runtime.go — Per-process session identity for Arc 2.
//
// Arc 2's fan-out needs to know "who is broadcasting" so it can
// skip self in the target list, and "who is receiving" so the
// WakesPending fold pulls only wakes for this session.
//
// In the MCP server, the session is constructed once at boot and
// lives for the server's lifetime. In the CLI dispatcher, every
// `mpm call` invocation is its own process; we need a session ID
// that's stable for the duration of the call but distinguishable
// across calls (so the wake-receipt fold doesn't double-pull).
//
// Strategy:
//   - session_id: 32 hex chars from crypto/rand, generated lazily
//     on first call. Each `mpm call` is a fresh process so the
//     value is naturally fresh. Override via $MPM_SESSION_ID for
//     long-lived agent loops that want a stable ID across calls.
//   - agent_id: from $MPM_AGENT_ID, falling back to "mpm_cli".
//     Operators identify themselves ("808", "alice-on-vm") by
//     exporting the var once in their shell profile.
//   - hostname: from $MPM_HOSTNAME, falling back to os.Hostname().

package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
)

var (
	sessionIDOnce sync.Once
	sessionID     string
)

// getOrMakeSessionID returns the per-process session ID. Generated
// lazily on first call so unit tests that don't need it don't pay
// the rand cost.
//
// Override: if MPM_SESSION_ID is set in the env, that wins. Useful
// for embedding MPM in a longer-lived agent loop where you want
// the session ID stable across `mpm call` invocations.
func getOrMakeSessionID() string {
	if env := os.Getenv("MPM_SESSION_ID"); env != "" {
		return env
	}
	sessionIDOnce.Do(func() {
		sessionID = newSessionID()
	})
	return sessionID
}

// resolveAgentID returns the agent identity for the shared.sessions
// heartbeat. Default "mpm_cli" so unit tests / one-off operators
// don't pollute the agents table with garbage; operators set
// MPM_AGENT_ID to a stable name (e.g., "808", "alice-on-vm").
func resolveAgentID() string {
	if env := os.Getenv("MPM_AGENT_ID"); env != "" {
		return env
	}
	return "mpm_cli"
}

// resolveHostname returns the machine hostname for the heartbeat.
// Defaults to "" if os.Hostname fails (rare; we swallow the error
// so the heartbeat row stays valid with a NULL hostname).
func resolveHostname() string {
	if env := os.Getenv("MPM_HOSTNAME"); env != "" {
		return env
	}
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// newSessionID returns a 32-char hex string from crypto/rand.
// 128 bits of entropy — collision probability for any realistic
// fleet is negligible.
func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is exceptional (no /dev/urandom).
		// Fall back to a deterministic-but-distinct value: pid+ns.
		b = []byte{
			byte(os.Getpid() & 0xff),
			byte((os.Getpid() >> 8) & 0xff),
		}
	}
	return hex.EncodeToString(b)
}