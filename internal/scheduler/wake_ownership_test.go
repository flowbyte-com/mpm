// wake_ownership_test.go — the wake ownership boundary, from the
// scheduler's side.
//
// The wake queue has two owners. The agent owns the notification kinds
// (core.AgentOwnedWakeKinds); the scheduler owns everything else, and
// runs it through a registered handler.
//
// These tests are the scheduler's half of that boundary:
//
//   - the agent's fold must leave the scheduler's wakes alone, so the
//     handler still gets to run them;
//   - the scheduler's handler kinds must never be agent-owned kinds,
//     because a kind that is both would be executed twice.
//
// Every test uses the scheduler's disposable file-backed test database.
package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// TestSchedulerDispatchesADrillWakeTheAgentCheckLeftPending is the
// end-to-end proof of the boundary.
//
// The order is the point: the agent's fold runs FIRST — it runs on
// every CLI and MCP request, so it always gets there first — and the
// scheduler's tick runs after. If the fold retired the wake, the
// handler would never run and the drill would be silently lost.
func TestSchedulerDispatchesADrillWakeTheAgentCheckLeftPending(t *testing.T) {
	s := newTestScheduler(t)
	// The core DatabaseManager over the scheduler's own database, so
	// the fold is the REAL fold and not a re-implementation of it.
	dm := core.NewDatabaseManagerForDB(s.db)

	var handled int
	s.Register(SystemHandlerDrill, func(ctx context.Context, w Wake) error {
		handled++
		return nil
	})

	const drillID = "wk-ownership-drill"
	now := time.Now()
	seedWake(t, s, drillID, now.Add(-time.Minute), SystemHandlerDrill)

	// 1. The agent-facing wildcard check, exactly as the CLI and the
	//    MCP server run it on every request.
	due, err := dm.CheckPendingWakes(now, []string{"*"})
	if err != nil {
		t.Fatalf("the agent-facing wildcard check failed: %v", err)
	}
	for _, w := range due {
		if id, _ := w["id"].(string); id == drillID {
			t.Fatalf("the agent-facing wildcard check surfaced the scheduler's drill wake to the agent")
		}
	}

	// 2. The wake must still be pending, so the scheduler can run it.
	var fired int
	if err := s.db.QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE id = ?`, drillID,
	).Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 0 {
		t.Fatalf("the agent-facing check retired the scheduler's drill wake; its handler will never run")
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
		t.Errorf("the drill handler ran %d time(s), want 1 — the wake was retired before its handler could run it", handled)
	}
	if err := s.db.QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE id = ?`, drillID,
	).Scan(&fired); err != nil {
		t.Fatalf("read fired: %v", err)
	}
	if fired != 1 {
		t.Errorf("the drill wake is not terminal after its handler ran (fired=%d)", fired)
	}
}

// TestNoSystemHandlerKindIsAgentOwned pins the ownership invariant.
//
// A kind is either the agent's or the scheduler's, never both. A kind
// in both would be delivered and retired by the fold AND executed by a
// handler: the work would run twice.
func TestNoSystemHandlerKindIsAgentOwned(t *testing.T) {
	for _, kind := range SystemHandlerKinds() {
		if core.WakeKindAgentOwned(kind) {
			t.Errorf("the scheduler registers a handler for %q, but that kind is agent-owned: "+
				"the fold would deliver and retire the wake, and the handler would run it a second time",
				kind)
		}
	}
}

// TestRegisterSystemHandlersRegistersExactlyTheSystemHandlerKinds pins
// the registration against the kind list.
//
// The two are the daemon's contract: the list says which kinds the
// daemon dispatches, and the registration says which handlers it
// installs. If they drift, the ownership invariant is being asserted
// about a set the daemon does not actually use.
func TestRegisterSystemHandlersRegistersExactlyTheSystemHandlerKinds(t *testing.T) {
	s := newTestScheduler(t)
	RegisterSystemHandlers(s, SystemHandlerDeps{
		Workspace:    t.TempDir(),
		CriticDBPath: "test.db",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	s.mu.RLock()
	registered := make([]string, 0, len(s.handlers))
	for kind := range s.handlers {
		registered = append(registered, kind)
	}
	s.mu.RUnlock()
	sort.Strings(registered)

	want := SystemHandlerKinds()
	sort.Strings(want)

	if len(registered) != len(want) {
		t.Fatalf("the daemon registered %v, but SystemHandlerKinds says %v", registered, want)
	}
	for i := range want {
		if registered[i] != want[i] {
			t.Fatalf("the daemon registered %v, but SystemHandlerKinds says %v", registered, want)
		}
	}
}

// TestRegisterRefusesAnAgentOwnedKind proves the invariant is ENFORCED,
// not merely documented.
//
// A handler for an agent-owned kind is refused: the kind stays
// unhandled, and the refusal is in the daemon's log rather than
// silently producing a double execution.
func TestRegisterRefusesAnAgentOwnedKind(t *testing.T) {
	s := newTestScheduler(t)

	for _, kind := range core.AgentOwnedWakeKinds {
		s.Register(kind, func(ctx context.Context, w Wake) error { return nil })
		s.mu.RLock()
		_, present := s.handlers[kind]
		s.mu.RUnlock()
		if present {
			t.Errorf("a handler was registered for the agent-owned kind %q; "+
				"the fold would retire the wake before the handler could run it", kind)
		}
	}
}
