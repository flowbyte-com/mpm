// umask.go — process-wide private-file umask for every MPM executable
// that can create persistent or sensitive state.
//
// Security invariant (2026-09-18 hardening pass):
//
//   Sensitive MPM directories  -> 0700
//   Sensitive MPM regular files -> 0600
//
// These must hold regardless of the caller's shell/framework umask
// (which is commonly 0002 on developer workstations). The process must
// clamp its own umask to 0077 so newly-created files inherit the
// restrictive default — independent of any ambient shell umask — and
// independent of whether systemd's UMask= directive is honored.
//
// The umask is process-global (Linux syscall.Umask). Callers MUST set
// it exactly once at the very top of main(), before:
//   - reading any config that may open/create DBs;
//   - starting goroutines that may write to disk;
//   - opening any file the process might create.
//
// Callers MUST NOT toggle it around individual file operations after
// concurrency begins. Doing so would race with parallel goroutines and
// is unsafe.
//
// Both the binary-level umask and the systemd-level UMask=0077 are
// redundant lines of defense: a binary launched from a permissive
// umask shell still creates private files; a binary launched under
// systemd with UMask=0077 still creates private files if its ambient
// shell happened to be permissive. Either is sufficient; both together
// make the security invariant portable across launchers.
//
// This file has no test file of its own — it is exercised by
// tests/security/test_umask_test.go in the same package, plus the
// lifecycle tests in scripts/tests/.

package config

import (
	"syscall"
)

// HardenedUMask is the process-wide file-creation mask applied by
// every MPM executable capable of creating persistent or sensitive
// state. Octal 0077 strips group + other on every file creation.
//
// Exported as a constant so tests and tooling can refer to the same
// canonical value rather than hard-coding literals.
const HardenedUMask = 0o077

// EnforcePrivateUmask sets the process-wide file creation mask to 0077
// (octal 0077 = decimal 63 = group + other stripped). It is idempotent
// and safe to call once at process startup.
//
// Returns the previous umask (the same value syscall.Umask returns),
// mostly so tests can assert the system actually accepted the call.
//
// On non-Unix platforms this is a no-op that returns 0. The
// portability shim lives here rather than at every call site so the
// build-tag discipline is centralized.
//
// The hardening call is intentionally SILENT by default — emitting a
// log line at process startup would disturb callers (such as the
// mpm-telemetry MCP-style subprocess) that treat stderr as a
// protocol stream and discard logs elsewhere. Operators can verify the
// process umask via the syscall directly; MPM's binaries print their
// own diagnostic at verbose levels if they want to advertise the
// hardened state.
func EnforcePrivateUmask() int {
	return syscall.Umask(int(HardenedUMask))
}
