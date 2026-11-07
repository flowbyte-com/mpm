// wake_resolve_ownership_test.go — the resolution side of the wake
// ownership boundary, from the scheduler's point of view.
//
// `mpm_wakes resolve` is the explicit, audited retirement path. Before
// the ownership correction it carried its own hand-written kind
// allowlist, which accepted `cascade_summary` — a kind the scheduler
// dispatches through a registered handler — while refusing `cron`,
// which is the agent's.
//
// The consequence was concrete: an agent could resolve a
// `cascade_summary` wake, the resolver set `fired=1`, and the
// scheduler's `QueryDueWakes` (which selects `WHERE fired = 0`) never
// saw the row again. The handler never ran.
//
// These tests are the scheduler's half of that boundary:
//
//   - a scheduler-owned wake the agent tried to resolve still reaches
//     its handler;
//   - the same wake resolves normally when nothing interfered, so the
//     first test is not passing for the wrong reason.
//
// Every test uses the scheduler's disposable file-backed test database.
package scheduler

import (
	"context"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// TestSchedulerDispatchesACascadeSummaryWakeTheAgentCouldNotResolve is
// the end-to-end proof.
//
// The order is the point. The agent's resolve attempt happens FIRST —
// it is an ordinary tool call the agent can make at any time — and the
// scheduler's tick runs after. If the resolve had succeeded, the wake
// would be terminal before the scheduler ever saw it, and the handler
// would never run.
func TestSchedulerDispatchesACascadeSummaryWakeTheAgentCouldNotResolve(t *testing.T) {
	s := newTestScheduler(t)
	// The core DatabaseManager over the scheduler's own database, so
	// the resolver is the REAL resolver and not a re-implementation.
	dm := core.NewDatabaseManagerForDB(s.db)

	var handled int
	// The scheduler owns `cascade_summary`: it has a registered handler.
	s.Register("cascade_summary", func(ctx context.Context, w Wake) error {
		handled++
		return nil
	})

	const wakeID = "wk-resolve-cascade-summary"
	now := time.Now()
	seedWake(t, s, wakeID, now.Add(-time.Minute), "cascade_summary")

	// 1. The agent tries to retire the wake.
	resolved, status, err := dm.ResolveWake(wakeID, "superseded", "")
	if err == nil {
		t.Fatalf("the agent must not resolve a scheduler-owned wake; "+
			"resolved=%v status=%q", resolved, status)
	}
	if resolved {
		t.Fatalf("the agent-facing resolve reported success on a scheduler-owned wake")
	}
	if status != "not_a_wake" {
		t.Errorf("refusal status = %q, want %q", status, "not_a_wake")
	}

	// 2. The wake must still be pending, so the scheduler can run it.
	var fired int
	if err := s.db.QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE id = ?`, wakeID,
	).Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 0 {
		t.Fatalf("the agent-facing resolve retired the scheduler's cascade_summary wake; "+
			"its handler will never run")
	}

	// 3. The scheduler's tick dispatches it.
	executed, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if executed != 1 {
		t.Errorf("the scheduler executed %d system wake(s), want 1", executed)
	}
	if handled != 1 {
		t.Errorf("the cascade_summary handler ran %d time(s), want 1 — "+
			"the wake was retired before its handler could run it", handled)
	}
	if err := s.db.QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE id = ?`, wakeID,
	).Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 1 {
		t.Errorf("the cascade_summary wake is not terminal after its handler ran (fired=%d)", fired)
	}
}

// TestSchedulerDispatchesACascadeSummaryWakeWithoutAnyResolveAttempt
// is the control.
//
// Without the resolver's intervention the same wake reaches the same
// handler exactly once. Without this control, the test above could
// pass for the wrong reason — e.g. if the scheduler never dispatched
// cascade_summary rows at all.
func TestSchedulerDispatchesACascadeSummaryWakeWithoutAnyResolveAttempt(t *testing.T) {
	s := newTestScheduler(t)

	var handled int
	s.Register("cascade_summary", func(ctx context.Context, w Wake) error {
		handled++
		return nil
	})

	const wakeID = "wk-resolve-cascade-summary-control"
	seedWake(t, s, wakeID, time.Now().Add(-time.Minute), "cascade_summary")

	executed, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if executed != 1 {
		t.Errorf("the scheduler executed %d system wake(s), want 1", executed)
	}
	if handled != 1 {
		t.Errorf("the cascade_summary handler ran %d time(s), want 1", handled)
	}
}

// TestSchedulerIgnoresAnAlreadyResolvedCronWake pins that the cron
// kind — agent-owned, and therefore now retirable by the agent — does
// not become scheduler work.
//
// A cron wake has no registered handler, so the scheduler must never
// dispatch it. If the agent resolves one, the scheduler must simply
// never see it.
func TestSchedulerIgnoresAnAlreadyResolvedCronWake(t *testing.T) {
	s := newTestScheduler(t)
	dm := core.NewDatabaseManagerForDB(s.db)

	const wakeID = "wk-resolve-cron"
	seedWake(t, s, wakeID, time.Now().Add(-time.Minute), "cron")

	// The agent resolves it — cron is the agent's own kind.
	resolved, status, err := dm.ResolveWake(wakeID, "reconciled", "")
	if err != nil {
		t.Fatalf("an agent-owned cron wake must be resolvable; status=%q err=%v", status, err)
	}
	if !resolved {
		t.Fatalf("an agent-owned cron wake must resolve; status=%q", status)
	}

	// The scheduler must not treat it as work.
	executed, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if executed != 0 {
		t.Errorf("the scheduler executed %d system wake(s), want 0 — a cron wake is the agent's", executed)
	}
}
