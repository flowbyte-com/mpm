// scheduler_blocked_wake_test.go — the DISPATCH gate for blocked wake kinds.
//
// P2b (docs/dev/session-a-adjudication.md §6b.1): a wake whose kind is
// blocked must not be EXECUTED, however it got persisted.
//
// The producer-side gate (ScheduleWake, in the core module) covers wakes
// armed from now on. This gate covers the ones that ALREADY EXIST: created
// before the gate existed, by an older binary, by a restore from a dump, or
// by direct SQL. Both gates are deliberate: the producer gate cannot speak
// for rows that already exist, and the consumer gate cannot speak for
// producers that have not been taught.
//
// The refusal is an ERROR on purpose. The caller marks the wake fired with
// the reason in `last_error`, so a blocked wake is recorded as
// processed-and-refused — it does NOT retry forever — and the refusal is
// observable rather than silent.

package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestScheduler_RefusesToDispatchAlreadyPersistedBlockedWake is the
// already-persisted case: the row is seeded by DIRECT SQL, bypassing the
// producer gate entirely, exactly as a legacy row would have been.
func TestScheduler_RefusesToDispatchAlreadyPersistedBlockedWake(t *testing.T) {
	s := newTestScheduler(t)

	called := false
	s.Register("critic_audit", func(ctx context.Context, w Wake) error {
		called = true
		return nil
	})

	// Seeded directly, NOT through ScheduleWake: this is the row a
	// producer-side gate cannot prevent.
	seedWake(t, s, "wk-legacy-critic", time.Now().Add(-time.Hour), "critic_audit")

	executed, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if called {
		t.Fatal("the critic_audit handler RAN for an already-persisted blocked wake")
	}
	if executed != 0 {
		t.Errorf("Tick reported %d executed wake(s); a refused wake must not count as executed", executed)
	}

	// The refusal must be recorded, not swallowed.
	var fired int
	var lastError string
	if err := s.db.QueryRow(
		`SELECT fired, COALESCE(json_extract(metadata, '$.last_error'), '') FROM scheduled_wakes WHERE id = ?`,
		"wk-legacy-critic",
	).Scan(&fired, &lastError); err != nil {
		t.Fatalf("read back the refused wake: %v", err)
	}
	if fired != 1 {
		t.Errorf("the refused wake must be marked fired (so it does not retry forever); fired=%d", fired)
	}
	if !strings.Contains(lastError, "critic_audit") {
		t.Errorf("the refusal reason must name the blocked kind; last_error=%q", lastError)
	}
	if !strings.Contains(lastError, "blocked") {
		t.Errorf("the refusal reason must say the wake was blocked; last_error=%q", lastError)
	}
}

// TestScheduler_DispatchesEveryOtherKind is the regression guard for the
// dispatch gate: an ordinary system wake must still run. A gate that
// blocked too much would silently disable the scheduler.
func TestScheduler_DispatchesEveryOtherKind(t *testing.T) {
	s := newTestScheduler(t)

	called := false
	s.Register("snapshot", func(ctx context.Context, w Wake) error {
		called = true
		return nil
	})

	seedWake(t, s, "wk-ordinary", time.Now().Add(-time.Hour), "snapshot")

	executed, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !called {
		t.Fatal("an ordinary system wake did NOT dispatch; the gate blocked too much")
	}
	if executed != 1 {
		t.Errorf("Tick reported %d executed wake(s); want 1", executed)
	}
}

// TestScheduler_BlockedWakeIsNotClaimedByTheOpportunisticFold pins the
// third path. The opportunistic fold claims only notification-kind wakes
// (dispatch.go's claim SQL filters kind='notification'), so a blocked wake
// cannot be smuggled through that route either. If that filter ever
// changes, this test fails rather than the wake running unreviewed.
func TestScheduler_BlockedWakeIsNotClaimedByTheOpportunisticFold(t *testing.T) {
	s := newTestScheduler(t)

	seedWake(t, s, "wk-fold-attempt", time.Now().Add(-time.Hour), "critic_audit")

	wake, claimed, err := dispatchClaimNextAdHocWake(context.Background(), s.db, time.Now())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Fatalf("the opportunistic fold claimed a %q wake; the notification-only filter must not widen", wake.Kind())
	}

	// And the row must be untouched by the fold's claim.
	var dispatchedAt interface{}
	if err := s.db.QueryRow(
		`SELECT dispatched_at FROM scheduled_wakes WHERE id = ?`, "wk-fold-attempt",
	).Scan(&dispatchedAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if dispatchedAt != nil {
		t.Errorf("the fold stamped dispatched_at on a blocked wake: %v", dispatchedAt)
	}
}
