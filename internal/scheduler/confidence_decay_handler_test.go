// confidence_decay_handler_test.go — pin the automatic trigger for
// issue #92: the pass runs on a cadence, not on every tick, and it can
// be disabled without touching code.
package scheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// insertStaleArtifact writes one memories-backed artifact whose last
// confidence check is old enough to be due.
func insertStaleArtifact(t *testing.T, dm *core.DatabaseManager, id string, now time.Time) {
	t.Helper()
	ts := now.Add(-200 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, weight, confidence, created_at, updated_at)
		 VALUES (?, 'memories', 'stale claim', 5, 0.8, ?, ?)`,
		id, ts, ts,
	); err != nil {
		t.Fatalf("insert artifact %s: %v", id, err)
	}
}

func decayFixtureClock() time.Time {
	return time.Unix(2_000_000_000, 0).UTC()
}

// TestConfidenceDecayHandler_RunsThenGatesByInterval pins the cadence:
// the first tick runs the pass, the next tick inside interval_hours is a
// no-op, and a tick after the interval runs again.
func TestConfidenceDecayHandler_RunsThenGatesByInterval(t *testing.T) {
	dm := core.NewTestDM(t)
	now := decayFixtureClock()
	insertStaleArtifact(t, dm, "mem-cadence", now)
	// Make the artifact due after a few hours, so the test measures the
	// cadence gate rather than the artifact-age gate.
	if _, err := dm.SaveSystemConfig(confidenceDecayMinAgeKey, "1", "1", ""); err != nil {
		t.Fatalf("min age: %v", err)
	}

	h := NewConfidenceDecayHandler(dm, slog.Default())
	h.now = func() time.Time { return now }
	tick := h.TickHandler()

	if err := tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	var checked *int64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence_checked_at FROM memories WHERE id = 'mem-cadence'`,
	).Scan(&checked); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if checked == nil {
		t.Fatal("first tick did not run the decay pass")
	}
	firstWatermark := *checked

	// One hour later: inside the default 6h interval → no run.
	now = now.Add(time.Hour)
	h.now = func() time.Time { return now }
	if err := tick(context.Background()); err != nil {
		t.Fatalf("gated tick: %v", err)
	}
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence_checked_at FROM memories WHERE id = 'mem-cadence'`,
	).Scan(&checked); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if checked == nil || *checked != firstWatermark {
		t.Fatalf("gated tick ran the pass again: watermark %v -> %v", firstWatermark, checked)
	}

	// Seven hours later: the marker has aged out → the pass runs again.
	now = now.Add(7 * time.Hour)
	h.now = func() time.Time { return now }
	if err := tick(context.Background()); err != nil {
		t.Fatalf("second scheduled tick: %v", err)
	}
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence_checked_at FROM memories WHERE id = 'mem-cadence'`,
	).Scan(&checked); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if checked == nil || *checked == firstWatermark {
		t.Fatalf("scheduled tick did not advance the watermark: %v", checked)
	}
}

// TestConfidenceDecayHandler_DisabledByConfig pins the off switch.
func TestConfidenceDecayHandler_DisabledByConfig(t *testing.T) {
	dm := core.NewTestDM(t)
	now := decayFixtureClock()
	insertStaleArtifact(t, dm, "mem-disabled", now)
	if _, err := dm.SaveSystemConfig(confidenceDecayEnabledKey, "0", "0", ""); err != nil {
		t.Fatalf("disable: %v", err)
	}

	h := NewConfidenceDecayHandler(dm, slog.Default())
	h.now = func() time.Time { return now }
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("disabled tick: %v", err)
	}
	var checked *int64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence_checked_at FROM memories WHERE id = 'mem-disabled'`,
	).Scan(&checked); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if checked != nil {
		t.Error("disabled handler still ran the pass")
	}
}

// TestConfidenceDecayHandler_PartialFailureIsReportedAndThrottled pins
// that a per-artifact failure surfaces as an error (visible in the
// scheduler log) while the cadence marker still advances, so the
// scheduler does not retry on every 60s tick.
func TestConfidenceDecayHandler_PartialFailureIsReportedAndThrottled(t *testing.T) {
	dm := core.NewTestDM(t)
	now := decayFixtureClock()
	insertStaleArtifact(t, dm, "mem-fail", now)
	if _, err := dm.SQLDB().Exec(`
		CREATE TRIGGER inject_handler_decay_failure
		BEFORE UPDATE OF confidence ON memories
		WHEN old.id = 'mem-fail'
		BEGIN SELECT RAISE(ABORT, 'injected'); END;
	`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}

	h := NewConfidenceDecayHandler(dm, slog.Default())
	h.now = func() time.Time { return now }
	if err := h.TickHandler()(context.Background()); err == nil {
		t.Fatal("partial failure was reported as success")
	}
	if _, err := dm.GetSystemConfig(core.ConfidenceDecayLastRunKey); err != nil {
		t.Fatalf("cadence marker was not written after a partial pass: %v", err)
	}
}
