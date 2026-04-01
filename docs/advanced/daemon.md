# Daemon Architecture

> **Note:** The daemon feature is currently **experimental** and requires manual building from source.

MPM can run as a persistent Unix socket daemon, enabling:
- **Stateful sessions** — daemon maintains uptime, active task counts
- **Structured JSON logging** — machine-readable audit trail
- **Subprocess isolation** — Ctrl+C only kills the subprocess, not the daemon
- **Concurrent command handling** — multiple `mpm` invocations share one daemon

---

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────┐
│                        mpm client                           │
│  $ mpm status                                              │
└─────────────────────┬───────────────────────────────────────┘
                      │ connect to /run/user/uid/mpm.sock
                      ▼
┌─────────────────────────────────────────────────────────────┐
│                    mpm daemon                               │
│                                                             │
│  ┌─────────────┐    ┌──────────────┐    ┌───────────────┐  │
│  │   status    │    │  stop        │    │  logs         │  │
│  │   meta-cmd  │    │  meta-cmd    │    │  meta-cmd     │  │
│  └─────────────┘    └──────────────┘    └───────────────┘  │
│                                                             │
│  ┌─────────────────────────────────────────────────────┐   │
│  │         executeCommandWithStream()                  │   │
│  │  ┌─────────────────────────────────────────────┐    │   │
│  │  │ Subprocess (Setpgid: true)                  │    │   │
│  │  │  └── mpm sync --workspace=...              │    │   │
│  │  └─────────────────────────────────────────────┘    │   │
│  └─────────────────────────────────────────────────────┘   │
│                                                             │
│  ┌─────────────┐    ┌──────────────┐    ┌───────────────┐  │
│  │ daemon.log  │    │ webhookChan  │    │  activeTasks  │  │
│  │ (JSON file) │    │  (buffered)  │    │  (atomic)     │  │
│  └─────────────┘    └──────────────┘    └───────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

---

## Socket Communication

### Socket Path Resolution

The daemon uses a **per-user socket path** to prevent cross-user collisions:

| Priority | Path | Notes |
|----------|------|-------|
| 1 | `$XDG_RUNTIME_DIR/mpm.sock` | Linux/BSD standard (e.g., `/run/user/1000/mpm.sock`) |
| 2 | `~/.mpm/mpm.sock` | Portable fallback |
| 3 | `/tmp/mpm.sock` | Last resort (shared in multi-user systems) |

```go
func socketPath() string {
    if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
        return filepath.Join(xdg, "mpm.sock")
    }
    if home, err := os.UserHomeDir(); err == nil {
        return filepath.Join(home, ".mpm", "mpm.sock")
    }
    return "/tmp/mpm.sock"
}
```

### Message Protocol

Communication uses JSON over Unix socket:

```json
// Client → Daemon (request)
{"args":["status"],"output":"","error":"","exit_code":0,"done":false}

// Daemon → Client (response)
{"args":null,"output":"{\"pid\":12345,...}","error":"","exit_code":0,"done":true}
```

---

## Process Isolation

### The `Setpgid` Pattern

When executing subprocesses, the daemon uses `Setpgid: true` to spawn each command in its **own process group**:

```go
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
```

**Benefit:** Pressing Ctrl+C in the terminal only kills the subprocess, not the daemon itself.

```
┌────────────────────────────────────────────────────────────┐
│ Terminal                                                   │
│  $ mpm sync    ───►  Daemon (pid 1234)                    │
│       │                      │                            │
│       │           ┌──────────┴──────────┐                │
│       ▼           ▼                      ▼                │
│   subprocess   subprocess            subprocess           │
│   (pgid 1235)  (pgid 1236)           (pgid 1237)          │
│                                                        │
│  Ctrl+C sends SIGINT to process group 1235 only         │
└────────────────────────────────────────────────────────────┘
```

### Graceful Shutdown

On `SIGINT`/`SIGTERM`/`SIGHUP`:

1. **Close listener** — stop accepting new connections
2. **Kill process group** — `syscall.Kill(-daemonPid, SIGTERM)`
3. **Remove socket file** — clean up `/run/user/uid/mpm.sock`
4. **Flush logs** — ensure no log data is lost

---

## Pre-Flight Health Check

Every daemon startup (including reboot) runs a diagnostic suite to validate the environment before accepting connections.

### Diagnostic Suite

| Check | Description | Auto-Repair |
|-------|-------------|-------------|
| **Lockfile Cleanup** | Removes stale lockfiles from previous crash | Yes |
| **Directory Check** | Ensures all required directories exist | Yes (creates) |
| **Database Integrity** | Validates SQLite with `PRAGMA integrity_check` | No |
| **Persona Validation** | Verifies persona directory and default persona exists | No |
| **Permissions Check** | Tests R/W access on critical paths | No |

### Go/No-Go Decision

| Condition | Action |
|-----------|--------|
| **Critical Failure** (database corrupted, persona missing) | Abort startup, print error to stderr |
| **Minor Issue** (missing directories, stale lockfiles) | Auto-repair, log WARN, proceed |
| **All Checks Pass** | Log INFO, proceed |

### Startup Output

**Success:**
```
[preflight] Lockfile Cleanup: OK (0ms)
[preflight] Directory Check: OK (1ms)
[preflight] Database Integrity: OK (15ms)
[preflight] Persona Validation: OK (2ms)
[preflight] Permissions Check: OK (0ms)
Daemon started on /run/user/1000/mpm.sock
```

**Critical Failure:**
```
✗ Pre-flight health check FAILED:

  [Error] Database Integrity: Database corruption detected
         Database: /workspace/src/db/mpm.db (2.4 MB)
         Integrity check result: errors found

Daemon aborted startup. Please fix the above issues.
```

### Telemetry

Pre-flight status is included in:
- **Heartbeat payload**: `preflight_status` field
- **Status response**: `preflight_status` field in JSON

```json
{
  "event": "heartbeat",
  "status": "healthy",
  "preflight_status": "Passed",
  "uptime": "2h34m",
  "memory_usage_kb": 8192
}
```

### Performance

Checks are optimized for speed:
- Parallel where possible
- Database opens in read-only mode
- Target: **<100ms** total for typical installations

---

## Lifecycle Commands

The daemon intercepts lifecycle commands for graceful shutdown and restart:

| Command | Aliases | Description |
|---------|---------|-------------|
| `mpm shutdown` | `stop` | Graceful daemon shutdown with session save |
| `mpm reboot` | `restart` | Graceful daemon restart (saves session first) |
| `mpm stop --force` | `stop -f` | Immediate shutdown (skip session save) |
| `mpm reboot --force` | `restart -f` | Immediate restart (skip session save) |

### Shutdown Sequence

```
$ mpm shutdown
  🔐 [1/3] Saving session...
  ⏹  [2/3] Terminating daemon...
  ✅ [3/3] Daemon stopped.
```

**With `--force`:**
```
$ mpm shutdown --force
  ⏹  [2/3] Terminating daemon...
  ✅ [3/3] Daemon stopped.
```

### Reboot Sequence

```
$ mpm reboot
  🔐 [1/3] Saving session...
  🔄 [2/3] Restarting daemon...
  ✅ [3/3] Daemon restarted.
```

**With `--force`:**
```
$ mpm reboot --force
  🔄 [2/3] Restarting daemon...
  ✅ [3/3] Daemon restarted.
```

### Session Auto-Save

Both `shutdown` and `reboot` automatically trigger `mpm ss` (session save) before terminating, **unless** `--force` is specified. This ensures all memory and persona state is persisted to disk.

---

## Doctor Command

The `mpm doctor` command provides a comprehensive diagnostic utility that can run independently of the daemon:

```
$ mpm doctor
$ mpm doctor --fix    # Attempt auto-repairs
```

### Diagnostic Suite

| Check | PASS | WARN | FAIL |
|-------|------|------|------|
| **System Information** | OS/Arch detected | Running as root | - |
| **Environment Variables** | MPM_WORKSPACE set | Not configured | - |
| **Workspace Structure** | All directories exist | - | Missing directories |
| **Socket Directory** | Writable | - | Not writable |
| **Database Integrity** | Valid, readable | No DB (first run) | Corrupted, read-only |
| **Webhook Connectivity** | HTTP 2xx response | Unreachable | - |
| **External Dependencies** | Tool found | Not found (optional) | - |
| **Daemon Status** | Daemon responsive | - | No response |

### Output Format

```
  ▸ System Information

    [PASS] Operating System
          linux/amd64
          Go Version: go1.21.0

    [WARN] User Permissions
          Running as UID 0 (not recommended)

  ▸ Environment Variables

    [PASS] MPM_WORKSPACE
          /home/user/.openclaw/workspace/projects/mpm

─────────────────────────────────────────────────────────────

  ▸ Summary

    Total Checks:  12
    ● Passed:  10
    ● Warnings: 2

  !  All critical checks passed. Review warnings above.
```

### --fix Flag

When run with `--fix`, doctor attempts to:
- Re-apply `chmod 755` to socket directory
- Re-apply `chmod 755` to database directory

### Standalone Operation

Doctor is designed to run **even if the daemon is not running**. This helps diagnose why the daemon won't start.

```
$ mpm doctor
✗ mpm Daemon is not currently active.
Running diagnostics anyway...

  ▸ Workspace Structure

    [FAIL] mode Directory
          Missing: /workspace/mode
```

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        Lifecycle Worker                         │
│                                                                 │
│  lifecycleChan ← chan *LifecycleOp                             │
│        │                                                        │
│        ├── "shutdown" → handleShutdown(force, conn)            │
│        │       ├── [1/3] flushSession()                        │
│        │       ├── [2/3] handleExit()                         │
│        │       └── [3/3] os.Exit(0)                           │
│        │                                                        │
│        └── "reboot" → handleReboot(force, conn)               │
│                ├── [1/3] flushSession()                        │
│                ├── [2/3] isRebooting=true                      │
│                ├── [3/3] executeReboot() → ForkExec           │
│                        └── Parent: handleExit()                 │
│                        └── Child: becomes new daemon           │
└─────────────────────────────────────────────────────────────────┘
```

### Reboot Atomicity

The reboot sequence ensures no "socket in use" race condition:

1. **Socket removed first** — `executeReboot()` removes `/run/user/uid/mpm.sock` before spawning child
2. **Child inherits state** — new process sees no socket, promotes itself to daemon
3. **Atomic handoff** — parent exits only after child has successfully bound the socket

---

## Meta-Commands

The daemon intercepts certain commands internally instead of spawning a subprocess:

| Command | Handler | Description |
|---------|---------|-------------|
| `mpm status` | `handleStatus()` | Returns daemon PID, uptime, active workers, queue depth, socket path |
| `mpm status --ping` | `handleStatus()` | Triggers manual heartbeat pulse |
| `mpm logs` | `handleLogsRequest()` | Streams daemon.json.log entries |

### Daemon Status Response

```json
{
  "pid": 12345,
  "uptime": "2h 15m 30s",
  "active_workers": 2,
  "queued_tasks": 1,
  "max_workers": 3,
  "total_tasks": 142,
  "socket": "/run/user/1000/mpm.sock"
}
```

**Human-readable output:**
```
  ┌─────────────────────────────────────────────┐
  │  MPM Daemon Status                           │
  ├─────────────────────────────────────────────┤
  │  PID:          12345                        │
  │  Uptime:       2h 15m 30s                    │
  │  Active Workers: 2                           │
  │  Queued Tasks:  1                           │
  │  Max Workers:   3                            │
  │  Total Tasks:   142                          │
  │  Socket:       /run/user/1000/mpm.sock       │
  └─────────────────────────────────────────────┘
```

---

## Worker Pool

The daemon uses a **worker pool** to limit concurrent task execution and manage load.

### Configuration

| Setting | Default | Environment Variable |
|---------|---------|---------------------|
| Max workers | 3 | `MPM_MAX_WORKERS` |
| Queue size | max(5, max_workers × 2) | — |
| Queue timeout | 30 minutes | — |

### Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        mpm daemon                               │
│                                                                 │
│  ┌─────────────┐                                                 │
│  │ taskQueue  │ ← Buffered channel (capacity: max_workers * 2)  │
│  │  (FIFO)    │                                                 │
│  └─────┬──────┘                                                 │
│        │ queueDispatcher()                                       │
│        ▼                                                        │
│  ┌────────────┴─────────────┴───────────┴──────────────────┐   │
│  │  Worker 1  │  Worker 2  │  Worker 3  │  ...             │   │
│  │  (running) │  (running) │  (queued)  │                  │   │
│  └────────────┴─────────────┴───────────┴──────────────────┘   │
│                                                                 │
│  Meta-commands bypass the queue entirely                        │
│  (status, logs, shutdown, reboot)                              │
└─────────────────────────────────────────────────────────────────┘
```

### Command Classification

| Type | Commands | Behavior |
|------|----------|----------|
| **Meta-commands** | `status`, `logs`, `help` | Execute immediately, bypass queue |
| **Lifecycle commands** | `shutdown`, `stop`, `reboot`, `restart` | Execute immediately, bypass queue |
| **Work commands** | `sync`, `shred`, `search`, etc. | Subject to worker pool limit |

### Queue Behavior

When all workers are busy:

```bash
$ mpm sync
All workers busy. Task queued at position #1.
Worker acquired. Executing sync...
```

**Queue response (JSON):**
```json
{
  "output": "All workers busy. Task queued at position #1.\n",
  "exit_code": 0,
  "done": false
}
```

When queue is full:
```json
{
  "output": "Queue is full. Please try again later.\n",
  "error": "queue full",
  "exit_code": 1,
  "done": true
}
```

### Queue Timeout

Tasks that sit in the queue for more than **30 minutes** are automatically cancelled:

```
[WARN] [queue] (pid:12345) Task task-1234567890-56789 timed out in queue
```

Client receives:
```json
{
  "output": "Task timed out after 30m0s in queue\n",
  "error": "queue timeout",
  "exit_code": 1,
  "done": true
}
```

### Graceful Shutdown

On `mpm stop`:
1. **Queue cleared** — all pending tasks rejected immediately
2. **Active workers finish** — currently running tasks allowed to complete
3. **Listener closed** — no new connections accepted
4. **Process group killed** — any straggling subprocesses terminated

---

## Structured JSON Logging

### Log File Location

```
~/.mpm/daemon.json.log   (or $XDG_RUNTIME_DIR/daemon.json.log)
```

### Log Entry Schema

```json
{
  "ts": "2026-03-29T08:21:00Z",
  "lvl": "INFO",
  "cmd": "sync",
  "pid": 12345,
  "msg": "Started sync",
  "dur_ms": null
}
```

| Field | Type | Description |
|-------|------|-------------|
| `ts` | string | RFC3339 timestamp |
| `lvl` | string | Log level: DEBUG, INFO, WARN, ERROR |
| `cmd` | string | Command name |
| `pid` | int | Subprocess PID (0 for daemon-level events) |
| `msg` | string | Human-readable message |
| `dur_ms` | int64? | Duration in milliseconds (optional) |

### Log Levels

| Level | Usage | Webhook |
|-------|-------|---------|
| DEBUG | Verbose internal events | No |
| INFO | Task start/complete | No |
| WARN | Recoverable issues | **Yes** |
| ERROR | Failures, exceptions | **Yes** |
| heartbeat | Health ping (24h interval) | **Yes** |
| manual_pulse | Immediate ping (`mpm status --ping`) | **Yes** |

### Log Rotation

- **Trigger:** When log file exceeds 10MB
- **Behavior:** Current log renamed to `daemon.json.log.YYYYMMDD-HHMMSS.old`
- **New file:** Fresh `daemon.json.log` created

### Reading Logs

```bash
# Human-readable format
mpm logs

# Raw JSON (machine-readable)
mpm logs --json

# Filter by level
mpm logs --level=error
```

**Human-readable output:**
```
[2026-03-29T08:21:00] INFO  [sync] (pid:12345) Started sync
[2026-03-29T08:21:05] INFO  [sync] (pid:12345) Completed sync (5000ms)
[2026-03-29T08:22:00] ERROR [sync] (pid:12346) Command failed: ...
```

---

## Thread Safety

| Component | Mechanism | Purpose |
|-----------|-----------|---------|
| `activeWorkers` | `atomic.Int64` | Thread-safe running worker counter |
| `queuedTasks` | `atomic.Int64` | Thread-safe queued task counter |
| `totalTasks` | `atomic.Int64` | Thread-safe total task counter |
| `logWriter` | `sync.Mutex` | Protect buffered writes |
| `webhookChan` | buffered channel | Async dispatch without blocking |
| `rateLimitCount` | `sync.Mutex` | Thread-safe rate counter |
| `cleanupSync` | `sync.Once` | Ensure cleanup runs exactly once |
| `queueMap` | `queueMapMutex` | Protect queue tracking map |

---

## Environment Variables

| Variable | Purpose |
|----------|---------|
| `MPM_WORKSPACE` | Override workspace path |
| `MPM_DIRECT=1` | Internal: bypass socket, run command directly |
| `MPM_WEBHOOK_URL` | Enable webhook notifications |
| `XDG_RUNTIME_DIR` | Socket/log directory (if set) |

---

## Heartbeat System

The heartbeat system proves the daemon is still alive by sending periodic health pings to the webhook endpoint.

### Heartbeat Payload

```json
{
  "event": "heartbeat",
  "status": "healthy",
  "uptime": "3d 4h 20m",
  "total_tasks_processed": 142,
  "memory_usage_kb": 8192,
  "timestamp": "2026-03-29T08:00:00Z"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `event` | string | Always `"heartbeat"` |
| `status` | string | Always `"healthy"` |
| `uptime` | string | Human-readable uptime (e.g., "3d 4h 20m") |
| `total_tasks_processed` | int64 | Commands handled since daemon start |
| `memory_usage_kb` | int64 | Current RSS memory in KB |
| `timestamp` | string | RFC3339 timestamp |

### Scheduling

| Setting | Value |
|---------|-------|
| Default interval | 24 hours |
| Jitter | ±5 minutes (random, prevents thundering herd) |
| First heartbeat | Initial interval + random jitter |

### Manual Pulse (`mpm status --ping`)

Test the webhook connection immediately:

```bash
$ mpm status --ping
✓ Manual pulse sent to webhook
```

**Manual Pulse Payload:**
```json
{
  "event": "manual_pulse",
  "type": "status_ping",
  "uptime": "5m 30s",
  "memory_kb": 8192,
  "timestamp": "2026-03-29T08:05:30Z"
}
```

### Resilience

- **Non-blocking**: Heartbeat goroutine never blocks `sync`/`shred` tasks
- **Failure logging**: If webhook POST fails, error logged to `daemon.json.log`
- **Retry**: 1 retry on 5xx errors with 500ms delay
- **Thread-safe**: `totalTasks` uses `sync/atomic`

---

## Related

- [Webhook System](webhook.md) — External notification dispatch
- [Quick Start](../getting-started/quick-start.md) — Setup and installation
