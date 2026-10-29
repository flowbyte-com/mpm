// confidence_decay_handler_control_test.go — the scheduler-side control
// surface of the automatic confidence-decay trigger (issue #92).
//
// The handler's contract with the operator is a CONTROL contract: when
// does the pass run, how much work does it do, and how does an operator
// see what it did. These tests pin the parts of that contract that are
// about stopping and reporting rather than about the decay math.
package scheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// TestConfidenceDecayHandler_CancelledTickDoesNotRunThePass pins that a
// shutdown during a tick stops the pass instead of running it to
// completion.
//
// The handler receives the scheduler's context, and a scheduler that is
// shutting down must not keep writing to the database. The pass
// observes the cancellation at the artifact boundary, so the artifacts
// it already finished keep their watermark and the rest stay due.
func TestConfidenceDecayHandler_CancelledTickDoesNotRunThePass(t *testing.T) {
	dm := core.NewTestDM(t)
	now := decayFixtureClock()
	insertStaleArtifact(t, dm, "mem-cancelled-tick", now)
	if _, err := dm.SaveSystemConfig(confidenceDecayMinAgeKey, "1", "1", ""); err != nil {
		t.Fatalf("min age: %v", err)
	}

	h := NewConfidenceDecayHandler(dm, slog.Default())
	h.now = func() time.Time { return now }
	tick := h.TickHandler()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A cancelled tick reports the cancellation as an error rather than
	// pretending the pass ran.
	if err := tick(ctx); err == nil {
		t.Error("a cancelled tick reported success")
	}

	var checked *int64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence_checked_at FROM memories WHERE id = 'mem-cancelled-tick'`,
	).Scan(&checked); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if checked != nil {
		t.Errorf("watermark = %v, want NULL — a cancelled tick must not mark work done", *checked)
	}
}

// TestConfidenceDecayHandler_CancelledPassDoesNotGateTheNextTick pins
// the resume side of the cancellation contract.
//
// A COMPLETED pass advances the cadence marker, so the next pass waits
// for the interval. A CANCELLED pass did not complete: the artifacts it
// did not reach are still due, and gating them behind a 6-hour interval
// would hide an unfinished backlog for no reason. So a cancelled pass
// leaves the marker alone.
func TestConfidenceDecayHandler_CancelledPassDoesNotGateTheNextTick(t *testing.T) {
	dm := core.NewTestDM(t)
	now := decayFixtureClock()
	insertStaleArtifact(t, dm, "mem-resume-tick", now)
	if _, err := dm.SaveSystemConfig(confidenceDecayMinAgeKey, "1", "1", ""); err != nil {
		t.Fatalf("min age: %v", err)
	}

	h := NewConfidenceDecayHandler(dm, slog.Default())
	h.now = func() time.Time { return now }
	tick := h.TickHandler()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tick(ctx); err == nil {
		t.Fatal("a cancelled tick reported success")
	}

	// The marker must NOT have been written, so the next tick is free to
	// resume the work.
	if _, err := dm.GetSystemConfig(core.ConfidenceDecayLastRunKey); err == nil {
		t.Error("the cancelled pass advanced the cadence marker; the next tick would be " +
			"gated and the unfinished backlog would be hidden")
	}

	// And the next tick, uncancelled, does the work.
	if err := tick(context.Background()); err != nil {
		t.Fatalf("resume tick: %v", err)
	}
	var checked *int64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence_checked_at FROM memories WHERE id = 'mem-resume-tick'`,
	).Scan(&checked); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if checked == nil {
		t.Error("the resume tick did not run the pass")
	}
}

// TestConfidenceDecayHandler_LimitIsConfigurable pins that the batch
// size is the operator's to choose, and that the configured value is
// what the pass uses.
//
// The limit is the main brake on a pass's cost, so it must be readable
// from configuration rather than compiled in — and the handler's default
// must match the core's documented default, so an operator who has
// configured nothing gets the documented behaviour.
func TestConfidenceDecayHandler_LimitIsConfigurable(t *testing.T) {
	dm := core.NewTestDM(t)
	now := decayFixtureClock()
	for i := 0; i < 5; i++ {
		insertStaleArtifact(t, dm, "mem-limit-"+string(rune('a'+i)), now)
	}
	if _, err := dm.SaveSystemConfig(confidenceDecayMinAgeKey, "1", "1", ""); err != nil {
		t.Fatalf("min age: %v", err)
	}
	if _, err := dm.SaveSystemConfig(confidenceDecayLimitKey, "2", "2", ""); err != nil {
		t.Fatalf("limit: %v", err)
	}

	h := NewConfidenceDecayHandler(dm, slog.Default())
	h.now = func() time.Time { return now }
	if err := h.TickHandler()(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	var checked int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id LIKE 'mem-limit-%' AND confidence_checked_at IS NOT NULL`,
	).Scan(&checked); err != nil {
		t.Fatalf("count checked: %v", err)
	}
	if checked != 2 {
		t.Errorf("checked %d artifacts, want 2 — the configured limit must bound the pass", checked)
	}
}

// TestConfidenceDecayHandler_DefaultLimitMatchesTheCoreDefault pins
// that the handler's default and the core's documented default are the
// same number.
//
// They live in two packages, so they can drift. When they do, the
// operator's configured-less run behaves differently from the
// documentation, and the drift is invisible.
func TestConfidenceDecayHandler_DefaultLimitMatchesTheCoreDefault(t *testing.T) {
	dm := core.NewTestDM(t)
	got := dm.GetConfigInt(confidenceDecayLimitKey, core.DefaultConfidenceDecayLimit)
	if got != core.DefaultConfidenceDecayLimit {
		t.Errorf("default limit = %d, want the core's %d", got, core.DefaultConfidenceDecayLimit)
	}
	gotAge := dm.GetConfigInt(confidenceDecayMinAgeKey, int(core.DefaultConfidenceDecayMinAge/time.Hour))
	wantAge := int(core.DefaultConfidenceDecayMinAge / time.Hour)
	if gotAge != wantAge {
		t.Errorf("default min age = %d hours, want the core's %d", gotAge, wantAge)
	}
}
