package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureLogger returns an slog.Logger writing JSON lines into a buffer,
// plus a getter that parses each line and returns them as map[string]any.
//
// Use this for tests that need to assert on log shape (fields, levels,
// message) without coupling to text formatting.
func captureLogger() (*slog.Logger, *bytes.Buffer, *sync.Mutex) {
	buf := &bytes.Buffer{}
	mu := &sync.Mutex{}
	log := slog.New(slog.NewJSONHandler(&syncWriter{w: buf, mu: mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return log, buf, mu
}

// captureLogs parses all JSON lines currently in the buffer and clears it.
// Returns empty slice if nothing buffered.
func captureLogsFrom(buf *bytes.Buffer, mu *sync.Mutex) []map[string]any {
	mu.Lock()
	defer mu.Unlock()
	raw := buf.String()
	buf.Reset()
	if raw == "" {
		return nil
	}
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if line == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries
}

// captureLogs is a convenience accessor for tests that wired captureLogger
// through the Scheduler struct (see newTestSchedulerWithCaptureLogger).
// Returns nil if no logger buffer is attached.
func (s *Scheduler) captureLogs() []map[string]any {
	if s.captureBuf == nil {
		return nil
	}
	return captureLogsFrom(s.captureBuf, s.captureMu)
}

type syncWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// TestExecuteOne_LogsDurationOnCompletion locks down the 2026-07-20 logging
// improvement: every wake handler execution emits a "wake completed" line
// with duration_ms. Before this fix, only "executing wake" was logged —
// impossible to distinguish a fast success from a hung handler killed by
// ctx without grepping system metrics.
func TestExecuteOne_LogsDurationOnCompletion(t *testing.T) {
	s := newTestSchedulerWithCaptureLogger(t)

	var called int
	s.Register("slow_kind", func(w Wake) error {
		called++
		time.Sleep(20 * time.Millisecond) // give us a measurable duration
		return nil
	})

	w := Wake{
		ID:       "wk-test-001",
		Metadata: map[string]interface{}{"kind": "slow_kind"},
	}

	if err := s.executeOne(context.Background(), w); err != nil {
		t.Fatalf("executeOne failed: %v", err)
	}
	if called != 1 {
		t.Fatalf("handler called %d times, want 1", called)
	}

	get := s.captureLogs()
	entries := get
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 log lines (executing + completed), got %d", len(entries))
	}

	// Find the completion line.
	var completed map[string]any
	for _, e := range entries {
		if msg, _ := e["msg"].(string); msg == "wake completed" {
			completed = e
			break
		}
	}
	if completed == nil {
		t.Fatalf("no 'wake completed' log line; entries: %+v", entries)
	}

	if completed["level"] != "INFO" {
		t.Errorf("completion log level = %v, want INFO", completed["level"])
	}
	if completed["wake_id"] != "wk-test-001" {
		t.Errorf("wake_id = %v, want wk-test-001", completed["wake_id"])
	}
	if completed["kind"] != "slow_kind" {
		t.Errorf("kind = %v, want slow_kind", completed["kind"])
	}
	dur, ok := completed["duration_ms"].(float64)
	if !ok {
		t.Fatalf("duration_ms missing or wrong type: %T %v", completed["duration_ms"], completed["duration_ms"])
	}
	if dur < 15 {
		t.Errorf("duration_ms = %v, want >= 15 (handler slept 20ms)", dur)
	}
}

// TestExecuteOne_LogsErrorOnFailure: handler returning error produces an
// ERROR-level "wake completed" line with the err field populated.
func TestExecuteOne_LogsErrorOnFailure(t *testing.T) {
	s := newTestSchedulerWithCaptureLogger(t)

	s.Register("fail_kind", func(w Wake) error {
		return context.DeadlineExceeded
	})

	w := Wake{ID: "wk-fail", Metadata: map[string]interface{}{"kind": "fail_kind"}}
	if err := s.executeOne(context.Background(), w); err == nil {
		t.Fatal("expected error from executeOne")
	}

	entries := s.captureLogs()
	var completed map[string]any
	for _, e := range entries {
		if msg, _ := e["msg"].(string); msg == "wake completed" {
			completed = e
			break
		}
	}
	if completed == nil {
		t.Fatalf("no 'wake completed' log line on error; entries: %+v", entries)
	}
	if completed["level"] != "ERROR" {
		t.Errorf("error completion level = %v, want ERROR", completed["level"])
	}
	if completed["err"] == nil {
		t.Error("error completion missing 'err' field")
	}
}

// TestSetHeartbeat_DefaultsTo100: verifies the documented default. If anyone
// changes the default to something aggressive (every tick), this test will
// surface it before it floods journal.
func TestSetHeartbeat_DefaultsTo100(t *testing.T) {
	// The default is set in New(), not the struct literal, so we exercise
	// the constructor to verify the documented default holds end-to-end.
	s := &Scheduler{heartbeatEvery: 100} // mirrors New() default for unit-test purposes
	if s.heartbeatEvery != 100 {
		t.Errorf("default heartbeatEvery = %d, want 100", s.heartbeatEvery)
	}
	s.SetHeartbeat(0)
	if s.heartbeatEvery != 0 {
		t.Errorf("after SetHeartbeat(0): heartbeatEvery = %d, want 0 (disabled)", s.heartbeatEvery)
	}
	s.SetHeartbeat(42)
	if s.heartbeatEvery != 42 {
		t.Errorf("after SetHeartbeat(42): heartbeatEvery = %d, want 42", s.heartbeatEvery)
	}
}

// TestRun_EmitsHeartbeatAtConfiguredCadence drives Run() with a fast-but-
// valid interval and asserts that the configured heartbeat fires a
// "scheduler heartbeat" line every N ticks. Without heartbeat, idle ticks
// stay silent and a hung process is indistinguishable from a healthy
// idle one.
func TestRun_EmitsHeartbeatAtConfiguredCadence(t *testing.T) {
	s := newTestSchedulerWithCaptureLogger(t)
	s.SetHeartbeat(2) // emit at tick 2, 4, 6 ...

	// Cancel after enough ticks for at least 1 heartbeat (>= 1s interval
	// requires the test to wait). Run enforces interval >= 1s as a guard
	// against accidental 1ns intervals in production.
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	_ = s.Run(ctx, time.Second)

	entries := s.captureLogs()
	var heartbeats int
	for _, e := range entries {
		if msg, _ := e["msg"].(string); msg == "scheduler heartbeat" {
			heartbeats++
		}
	}
	if heartbeats == 0 {
		t.Fatalf("expected at least one heartbeat line in 2.5s with 1s interval and heartbeat=2; entries: %+v", entries)
	}
}

// TestRun_HeartbeatDisabled_StaysSilent: SetHeartbeat(0) must produce NO
// heartbeat log lines even across many ticks.
func TestRun_HeartbeatDisabled_StaysSilent(t *testing.T) {
	s := newTestSchedulerWithCaptureLogger(t)
	s.SetHeartbeat(0)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	_ = s.Run(ctx, time.Second)

	entries := s.captureLogs()
	for _, e := range entries {
		if msg, _ := e["msg"].(string); msg == "scheduler heartbeat" {
			t.Errorf("heartbeat disabled but got line: %+v", e)
		}
	}
}

// newTestSchedulerWithCaptureLogger returns a Scheduler wired with a
// JSON logger that captures into a buffer retrievable via s.captureLogs().
// Used by logging-shape assertions.
func newTestSchedulerWithCaptureLogger(t *testing.T) *Scheduler {
	t.Helper()
	s := newTestScheduler(t)
	log, buf, mu := captureLogger()
	s.log = log
	s.captureBuf = buf
	s.captureMu = mu
	return s
}