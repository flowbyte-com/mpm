package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// StateFilePath returns the canonical path to scheduler.state — the
// cross-process health bridge between this daemon and the mpm CLI's
// emitSchedulerHealthWarning (cmd/mpm/main.go).
//
// Resolution order:
//  1. $MPM_WORKSPACE/run/scheduler.state (canonical — set by the systemd unit)
//  2. $HOME/.mpm/run/scheduler.state (legacy / dev fallback)
//  3. /tmp/run/scheduler.state (last resort — log a warning)
//
// The CLI reads from $HOME/.mpm/run/scheduler.state. The daemon must
// write to the SAME path or the bridge is broken. With MPM_WORKSPACE
// unset on a non-standard layout the bridge silently degrades to a
// mismatch — surfacing 'stalled' on a healthy daemon. Always set
// MPM_WORKSPACE in the systemd unit (or rely on the HOME fallback).
func StateFilePath() string {
	if ws := os.Getenv("MPM_WORKSPACE"); ws != "" {
		return filepath.Join(ws, "run", "scheduler.state")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".mpm", "run", "scheduler.state")
	}
	return filepath.Join("/tmp", "run", "scheduler.state")
}

// schedulerState is the JSON shape written by persistState and read by
// cmd/mpm/main.go's emitSchedulerHealthWarning. Keep the field set in
// sync with both sides — a missing field on the reader is silently
// zero-valued, a missing field on the writer is silently omitted.
//
// Layout choices:
//   - unix epoch ints (not RFC3339 strings) for last_tick_unix /
//     process_started_unix — the CLI computes age = now - last_tick_unix
//     and human-friendly format is the CLI's job, not ours.
//   - tick_count is uint64 (matches the in-memory counter type).
//   - pid is int (matches os.Getpid() return type).
//   - last_status / last_error are always "ok" / "" on the happy path;
//     reserved for future error-surfacing extensions without a schema
//     migration.
type schedulerState struct {
	LastTickUnix       int64  `json:"last_tick_unix"`
	ProcessStartedUnix int64  `json:"process_started_unix"`
	TickCount          uint64 `json:"tick_count"`
	PID                int    `json:"pid"`
	LastStatus         string `json:"last_status"`
	LastError          string `json:"last_error"`
}

// stateTarget resolves the heartbeat file for this Scheduler. New()
// captures StateFilePath() into s.statePath; the empty-string fallback
// covers a Scheduler built as a struct literal (tests only) and keeps
// the pre-field resolution behaviour intact for that shape.
func (s *Scheduler) stateTarget() string {
	if s.statePath != "" {
		return s.statePath
	}
	return StateFilePath()
}

// persistState atomically writes the scheduler heartbeat to disk so the
// CLI's emitSchedulerHealthWarning can observe liveness across processes.
//
// The target is s.stateTarget() — the daemon's own path, captured at
// construction — rather than a fresh StateFilePath() call per tick, so
// a second Scheduler in the same process cannot overwrite the live
// daemon's heartbeat.
//
// Atomic write pattern: write to <target>.tmp, then os.Rename to
// <target>. Because both paths share the same filesystem (per POSIX
// rename(2) atomicity), a concurrent reader — the CLI, the mpm-opencode
// plugin — observes either the old valid state or the new valid state,
// never a half-written buffer. Direct os.WriteFile / os.OpenFile +
// O_TRUNC would race the reader's os.ReadFile and could surface a
// truncated or empty file mid-write. This pattern closes that race.
//
// The <target>.tmp sibling is part of the same artifact: it is created
// and renamed away on every successful tick. Any test that pins a
// scheduler's state path must pin the directory, not just the file.
//
// Write amp: ~130 bytes per tick. At default 60s interval that's
// negligible (well under 1 KB/min) — no throttling needed.
//
// Failure handling: every step logs a warning but does not propagate.
// The state file is observability, not critical path — a failing disk
// must not stall the wake executor. The CLI's 'stalled' verdict will
// surface the failure as a warning, which is exactly the operator
// signal we want.
func (s *Scheduler) persistState() {
	state := schedulerState{
		LastTickUnix:       time.Now().Unix(),
		ProcessStartedUnix: s.processStartedUnix,
		TickCount:          s.tickCount,
		PID:                os.Getpid(),
		LastStatus:         "ok",
		LastError:          "",
	}
	data, err := json.Marshal(state)
	if err != nil {
		s.log.Warn("state marshal failed", "err", err)
		return
	}

	target := s.stateTarget()
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.log.Warn("state dir create failed", "dir", dir, "err", err)
		return
	}

	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		s.log.Warn("state tmp write failed", "path", tmp, "err", err)
		return
	}

	if err := os.Rename(tmp, target); err != nil {
		s.log.Warn("state rename failed", "from", tmp, "to", target, "err", err)
		// Best-effort cleanup; the tmp will be overwritten on the next
		// successful tick write anyway.
		_ = os.Remove(tmp)
		return
	}
}
