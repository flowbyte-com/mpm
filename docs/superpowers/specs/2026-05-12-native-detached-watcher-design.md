# Native Detached Watcher — Design Spec

**Date:** 2026-05-12
**Status:** Draft — awaiting approval

---

## Overview

Replace the fire-and-forget `nohup mpm watch start &` pattern with native self-spawn. When the user runs `mpm watch start`, the CLI spawns a detached child process that runs the watcher goroutine, then exits immediately. A PID file enables `stop` and `status` to find and communicate with the detached watcher without a Unix socket.

---

## PID File

**Location:** `{MPM_DATA_DIR}/watch.pid` (falls back to `~/.mpm/watch.pid`)

**Format:** Plain text, single line — the PID as a decimal integer.

**Lifecycle:**
- Written by the child process on startup (after successful goroutine init)
- Deleted by the child process on graceful shutdown
- Deleted by `stop` if the process is already dead (stale PID file)

---

## Watch Start — Self-Spawn Flow

```
User runs: mpm watch start
    │
    ▼
handleWatchStart (no --bg flag)
    │
    ├─► Re-invoke: exec.Command(os.Executable(), "watch", "start", "--bg")
    │   └─► cmd.Start()  →  child process spawned
    │
    └─► Parent prints: "🚀 Watcher started in background (PID {pid})"
        └─► os.Exit(0)  →  terminal returned to user

─────────────────────────────────────────────────

Child runs: mpm watch start --bg
    │
    ▼
handleWatchStart (--bg flag present)
    │
    ├─► Lazy-init WorkerPool + DatabaseManager
    ├─► Write PID to watch.pid
    ├─► Setup signal.Notify(os.Interrupt, SIGTERM) → graceful shutdown
    ├─► Start watcher goroutine + external DB pollers (startWatchGoroutine)
    └─► Block: select{}  →  process stays alive until signal or crash
```

**On graceful shutdown (SIGTERM / Interrupt):**
1. Call `stopWatchGoroutine()` — cancels `watcherCtx`, drains pool
2. Delete `watch.pid`
3. Exit 0

**On crash:** `watch.pid` is not deleted. `stop` and `status` treat a stale PID file as "not running."

---

## Watch Stop

```
User runs: mpm watch stop
    │
    ▼
handleWatchStop
    │
    ├─► Read PID from watch.pid
    │       └─► If file missing → "Watcher is not running."
    │
    ├─► os.FindProcess(pid)
    │       └─► If process not found → delete PID file, print "Watcher is not running."
    │
    └─► process.Signal(os.Interrupt)   ← standard Go, cross-platform
            ├─► Success → print "Watcher stopped."
            └─► Err     → delete PID file, print error
```

---

## Watch Status

```
User runs: mpm watch status
    │
    ▼
handleWatchStatus
    │
    ├─► Read PID from watch.pid
    │       └─► If file missing → "Watcher is not running."
    │
    ├─► os.FindProcess(pid) + signal 0 check
    │       └─► If process dead → delete PID file, print "Watcher is not running."
    │
    └─► Print running state with PID and event processed count
```

---

## Edge Cases

| Scenario | Behavior |
|----------|----------|
| `watch start` when already running | Parent reads `watch.pid` → `os.FindProcess` + signal 0 → if alive, print "Watcher is already running (PID X)" and exit without spawning |
| `watch stop` when not running | Reads missing/stale PID file → prints "not running", no error exit |
| `watch start --bg` run directly by user | Behaves as child (no re-spawn) — blocks normally |
| Process crashes (no graceful exit) | `watch.pid` left behind → next `stop`/`status` cleans it up |
| `watch stop` → process already dead | `os.FindProcess` succeeds (dead process), signal fails → clean up PID file |
| Permission denied writing PID file | Propagate error, do not start watcher |

---

## Files Changed

| File | Change |
|------|--------|
| `cmd/mpm/handlers.go` | `handleWatchStart` (add --bg branch), `handleWatchStop`, `handleWatchStatus` (new), signal handler in child |
| `cmd/mpm/router.go` | Register `--bg` flag for `watch start` subcommand |

---

## Security Considerations

- PID file in a user-writable directory — no special permissions needed
- Child process is the same binary, same user — no privilege escalation
- No socket exposed — no network attack surface from this feature