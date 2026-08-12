package main

// scheduler_health_test.go — coverage for the passive scheduler-health
// nudge that fires from main() before any command dispatch. The nudge
// reads ~/.mpm/run/scheduler.state (the cross-process health bridge the
// scheduler writes every tick) and emits a single discrete warning to
// stderr if the daemon is degraded. Silent on the healthy path.
//
// Mandatory coverage: each verdict (not_running, error, stalled, ok)
// and the corrupt-file silent path. The "emission is targeted only at
// degraded states" invariant is the load-bearing property — any
// regression that starts emitting on the healthy path is a noise-storm
// waiting to happen on every dev loop.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSchedulerState plants a fake state file under a per-test HOME
// and returns the path. Caller is responsible for HOME = test home.
func writeSchedulerState(t *testing.T, home string, body interface{}) {
	t.Helper()
	dir := filepath.Join(home, ".mpm", "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	var data []byte
	switch v := body.(type) {
	case string:
		data = []byte(v)
	case nil:
		// omit the file entirely
		return
	default:
		var err error
		data, err = json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal state: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "scheduler.state"), data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

func TestEmitSchedulerHealthWarning_NotRunning(t *testing.T) {
	home := t.TempDir()
	writeSchedulerState(t, home, nil) // no state file at all

	t.Setenv("HOME", home)
	var buf bytes.Buffer
	emitSchedulerHealthWarning(&buf)

	got := buf.String()
	if !strings.Contains(got, "not running") {
		t.Errorf("expected 'not running' warning, got: %q", got)
	}
	if !strings.Contains(got, "systemctl --user start mpm-scheduler") {
		t.Errorf("expected remediation hint, got: %q", got)
	}
}

func TestEmitSchedulerHealthWarning_Error(t *testing.T) {
	home := t.TempDir()
	writeSchedulerState(t, home, map[string]interface{}{
		"last_status":          "error",
		"last_error":           "connection refused: db locked",
		"last_tick_unix":       int64(0),
		"process_started_unix": int64(0),
	})

	t.Setenv("HOME", home)
	var buf bytes.Buffer
	emitSchedulerHealthWarning(&buf)

	got := buf.String()
	if !strings.Contains(got, "error") {
		t.Errorf("expected 'error' verdict, got: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("expected last_error detail in hint, got: %q", got)
	}
}

func TestEmitSchedulerHealthWarning_Stalled(t *testing.T) {
	home := t.TempDir()
	// 6 min ago — beyond the 5 min threshold.
	now := time.Now().Unix()
	writeSchedulerState(t, home, map[string]interface{}{
		"last_status":          "ok",
		"last_error":           "",
		"last_tick_unix":       now - 360,
		"process_started_unix": now - 360,
	})

	t.Setenv("HOME", home)
	var buf bytes.Buffer
	emitSchedulerHealthWarning(&buf)

	got := buf.String()
	if !strings.Contains(got, "stalled") {
		t.Errorf("expected 'stalled' verdict, got: %q", got)
	}
	if !strings.Contains(got, "6m ago") {
		t.Errorf("expected age hint (6m ago), got: %q", got)
	}
}

func TestEmitSchedulerHealthWarning_Healthy(t *testing.T) {
	home := t.TempDir()
	// 1 min ago — well within the 5 min threshold.
	now := time.Now().Unix()
	writeSchedulerState(t, home, map[string]interface{}{
		"last_status":          "ok",
		"last_error":           "",
		"last_tick_unix":       now - 60,
		"process_started_unix": now - 60,
	})

	t.Setenv("HOME", home)
	var buf bytes.Buffer
	emitSchedulerHealthWarning(&buf)

	// The load-bearing property: silent on the healthy path.
	if buf.Len() != 0 {
		t.Errorf("healthy state must emit silently, got: %q", buf.String())
	}
}

func TestEmitSchedulerHealthWarning_CorruptFile(t *testing.T) {
	home := t.TempDir()
	writeSchedulerState(t, home, "this is not json {{{")

	t.Setenv("HOME", home)
	var buf bytes.Buffer
	emitSchedulerHealthWarning(&buf)

	// Corrupt state is "unknown" — equal to not_running for nudge
	// purposes; but the corrupt-file path is intentionally silent
	// (the permcheck gate + `mpm status` surface it elsewhere).
	if buf.Len() != 0 {
		t.Errorf("corrupt state file must be silent, got: %q", buf.String())
	}
}

func TestEmitSchedulerHealthWarning_StallEdgeAtThreshold(t *testing.T) {
	// Exactly at the threshold boundary — should NOT emit (the
	// condition is `> threshold`, not `>= threshold`). Locks down
	// the operator-facing semantics so a future off-by-one tweak
	// can't push the healthy-path nudge into the noisy zone.
	home := t.TempDir()
	now := time.Now().Unix()
	writeSchedulerState(t, home, map[string]interface{}{
		"last_status":          "ok",
		"last_tick_unix":       now - int64(schedulerHealthStaleAfterSecs),
		"process_started_unix": now - int64(schedulerHealthStaleAfterSecs),
	})

	t.Setenv("HOME", home)
	var buf bytes.Buffer
	emitSchedulerHealthWarning(&buf)

	if buf.Len() != 0 {
		t.Errorf("at-threshold tick should be silent (>, not >=), got: %q", buf.String())
	}
}
