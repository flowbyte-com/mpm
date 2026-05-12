---
name: mpm-reboot-bug-fix
description: Fixed daemon restart bug where mpm restart caused daemon to die instead of restarting
type: project
---

# MPM Daemon Restart Bug Fix (2026-04-03)

## Problem
`mpm restart` caused the daemon to die completely instead of restarting.

## Root Cause
The `handleReboot` function called `listener.Close()` in a goroutine before `syscall.Exec`. This caused the accept loop to break, `main()` to return, and the Go runtime to terminate BEFORE `execve` could run. The goroutine never got a chance to execute the process replacement.

## Fix
Removed `listener.Close()` and `os.Remove(sockPath)` from the reboot goroutine. The new process inherits the socket fd via execve but cleans it up in `becomeDaemon()` via `os.Remove(sockPath)` before rebinding.

Key insight: don't close resources in a goroutine before execve — execve replaces the process atomically, but Go's runtime can still terminate the process if main() returns.

## Files Changed
- `cmd/mpm/main.go`: handleReboot() — simplified execve approach

## Verification
- Daemon survives `mpm restart` repeatedly
- PID changes on each restart (proving fresh process)
- Uptime resets to 1s after restart
