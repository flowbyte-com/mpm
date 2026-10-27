// wake_tools_blocked_kind_test.go — the ARMING gate for blocked wake kinds.
//
// P2b (docs/dev/session-a-adjudication.md §6b.1): the critic's automatic
// path must not be ARMED while its automatic trust behaviour is unsafe.
//
// The producer-side gate lives in ScheduleWake, the DB-layer producer, so
// that every producer is covered by one check rather than by each caller's
// own diligence. These tests pin that placement — including the fact that
// the producer gate alone is NOT sufficient, which is why the dispatch gate
// in internal/scheduler exists.
//
// Every test pins an isolated HOME, MPM_WORKSPACE and MPM_DB_PATH and opens
// a disposable file-backed database, so the production path resolution is
// exercised rather than bypassed, and the live database is never touched.

package internal

import (
	"strings"
	"testing"
)

// TestScheduleWake_RefusesBlockedCriticAuditKind pins the arming gate: a
// critic_audit wake cannot be created through the producer.
func TestScheduleWake_RefusesBlockedCriticAuditKind(t *testing.T) {
	dm := NewTestDM(t)

	_, err := dm.ScheduleWake("arm the critic", "2026-10-27T00:00:00Z", "", "test", map[string]interface{}{
		"kind": BlockedWakeKindCriticAudit,
	})
	if err == nil {
		t.Fatal("ScheduleWake armed a critic_audit wake; the arming gate did not fire")
	}
	if !strings.Contains(err.Error(), BlockedWakeKindCriticAudit) {
		t.Errorf("the refusal must name the blocked kind, got: %v", err)
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("the refusal must say the wake was blocked, got: %v", err)
	}

	// Nothing may be persisted. A refused wake must not leave a row that
	// the dispatch gate would later have to clean up.
	var count int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE json_extract(metadata, '$.kind') = ?`,
		BlockedWakeKindCriticAudit,
	).Scan(&count); err != nil {
		t.Fatalf("count critic_audit wakes: %v", err)
	}
	if count != 0 {
		t.Errorf("a refused wake was persisted anyway: %d row(s) with kind=%s", count, BlockedWakeKindCriticAudit)
	}
}

// TestScheduleWake_AllowsEveryOtherKind is the regression guard for the
// gate: the notification path and every other kind must still arm normally.
// A gate that blocked too much would silently disable the wake queue.
func TestScheduleWake_AllowsEveryOtherKind(t *testing.T) {
	dm := NewTestDM(t)

	for _, kind := range []string{"notification", "cascade", "snapshot", "gc", "drill", "broadcast"} {
		out, err := dm.ScheduleWake("arm "+kind, "2026-10-27T00:00:00Z", "", "test", map[string]interface{}{
			"kind": kind,
		})
		if err != nil {
			t.Errorf("ScheduleWake refused the ordinary kind %q: %v", kind, err)
			continue
		}
		if out == nil {
			t.Errorf("ScheduleWake returned no receipt for kind %q", kind)
		}
	}

	// The absent-kind default must still work — it is the most common
	// producer path and it must not be caught by the gate.
	if _, err := dm.ScheduleWake("arm with no kind", "2026-10-27T00:00:00Z", "", "test", nil); err != nil {
		t.Errorf("ScheduleWake refused a wake with no kind (the notification default): %v", err)
	}
}

// TestScheduleWake_BlockedKindIsCheckedInTheProducerNotTheCaller pins
// WHERE the gate lives, and demonstrates why the dispatch gate is also
// needed: a row persisted by any path that bypasses ScheduleWake (direct
// SQL, an older binary, a restore from a dump) is NOT prevented by the
// producer gate.
func TestScheduleWake_BlockedKindIsCheckedInTheProducerNotTheCaller(t *testing.T) {
	dm := NewTestDM(t)

	// Simulate a row that already exists: inserted directly, bypassing the
	// producer. This is the "already-persisted wake" case.
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES ('wk-already-persisted', 0, 'armed before the gate existed', 0, 'legacy',
		         json_object('kind', ?))`,
		BlockedWakeKindCriticAudit,
	); err != nil {
		t.Fatalf("seed an already-persisted wake: %v", err)
	}

	// The producer gate does not retroactively touch it — by design. The
	// row is still there, and it is the DISPATCH gate's job to refuse it.
	var count int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE id = 'wk-already-persisted'`,
	).Scan(&count); err != nil {
		t.Fatalf("count the seeded wake: %v", err)
	}
	if count != 1 {
		t.Fatalf("the seeded wake was not persisted; the fixture is wrong")
	}

	// And the producer refuses to arm ANOTHER one of the same kind, so
	// the gate is genuinely producer-side rather than a one-off.
	if _, err := dm.ScheduleWake("arm again", "2026-10-27T00:00:00Z", "", "test", map[string]interface{}{
		"kind": BlockedWakeKindCriticAudit,
	}); err == nil {
		t.Fatal("the producer armed a second critic_audit wake")
	}
}

// TestWakeKindBlocked_PredicateIsExact pins the predicate: only the
// blocked kind is blocked, and the comparison is exact (no prefix, no
// case-folding, no trimming). A loose comparison would let a near-miss
// kind through, or block a legitimate one.
func TestWakeKindBlocked_PredicateIsExact(t *testing.T) {
	if !WakeKindBlocked(BlockedWakeKindCriticAudit) {
		t.Errorf("%s must be blocked", BlockedWakeKindCriticAudit)
	}
	for _, kind := range []string{
		"", "notification", "Critic_Audit", "critic_audit ", " critic_audit",
		"critic_audits", "critic", "audit", "snapshot", "gc",
	} {
		if WakeKindBlocked(kind) {
			t.Errorf("kind %q must NOT be blocked", kind)
		}
	}
}
