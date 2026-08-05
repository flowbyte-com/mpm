package capability

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestProbationTick_NoProbationRows(t *testing.T) {
	store, _, _ := newTestStore(t)

	res, err := store.CheckProbationCompletion(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(res.PromotedIDs) != 0 {
		t.Errorf("expected no promotions, got %v", res.PromotedIDs)
	}
	if len(res.SkippedIDs) != 0 {
		t.Errorf("expected no skips, got %v", res.SkippedIDs)
	}
	if len(res.Errors) != 0 {
		t.Errorf("expected no errors, got %v", res.Errors)
	}
}

func TestProbationTick_PromotesEligible(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Seed a capability in probation with enough successful
	// invocations and a low failure rate. The criteria should
	// pass and the tick should promote it to active.
	seedProbationRow(t, db, "cap_a", 5, 0, 5, 0.10) // 5/5 success, rate 0
	res, err := store.CheckProbationCompletion(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(res.PromotedIDs) != 1 || res.PromotedIDs[0] != "cap_a" {
		t.Errorf("expected cap_a promoted, got: %+v", res)
	}

	// Verify state in the DB.
	var state string
	if err := db.QueryRow(`SELECT state FROM capabilities WHERE id = ?`, "cap_a").Scan(&state); err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != "active" {
		t.Errorf("state = %q, want active", state)
	}
}

func TestProbationTick_SkipsNotEligible(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Probation cap with 2 successes, 1 failure. Required: 5.
	// Rate: 1/3 = 0.33, ceiling 0.10. Both criteria fail.
	seedProbationRow(t, db, "cap_b", 2, 1, 5, 0.10)
	res, err := store.CheckProbationCompletion(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(res.PromotedIDs) != 0 {
		t.Errorf("expected no promotions, got: %+v", res.PromotedIDs)
	}
	if len(res.SkippedIDs) != 1 || res.SkippedIDs[0] != "cap_b" {
		t.Errorf("expected cap_b skipped, got: %+v", res)
	}

	// Verify state unchanged.
	var state string
	if err := db.QueryRow(`SELECT state FROM capabilities WHERE id = ?`, "cap_b").Scan(&state); err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != "probation" {
		t.Errorf("state should remain probation, got %q", state)
	}
}

func TestProbationTick_PromotesMultiple(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Three probation caps: one eligible, one with too few
	// successes, one with too-high failure rate. The tick
	// should promote only the first.
	seedProbationRow(t, db, "cap_eligible", 6, 0, 5, 0.10) // passes
	seedProbationRow(t, db, "cap_few", 2, 0, 5, 0.10)     // not enough
	seedProbationRow(t, db, "cap_rate", 5, 5, 5, 0.10)    // 50% failure

	res, err := store.CheckProbationCompletion(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(res.PromotedIDs) != 1 || res.PromotedIDs[0] != "cap_eligible" {
		t.Errorf("expected only cap_eligible promoted, got: %+v", res)
	}
	if len(res.SkippedIDs) != 2 {
		t.Errorf("expected 2 skipped, got %d: %+v", len(res.SkippedIDs), res.SkippedIDs)
	}
}

func TestProbationTick_IgnoresNonProbationStates(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Seed caps in every other state with the same metrics.
	// The tick should NOT touch them.
	for _, st := range []CapabilityState{
		StateDraft, StateLinted, StateValidated, StateActive,
		StateRetired, StateRolledBack, StateNeedsRevision, StateFractured,
	} {
		seedProbationRowWithState(t, db, "cap_"+string(st), string(st), 10, 0, 5, 0.10)
	}

	res, err := store.CheckProbationCompletion(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(res.PromotedIDs) != 0 {
		t.Errorf("expected no promotions, got: %+v", res)
	}
}

func TestProbationTick_HonorsContextCancellation(t *testing.T) {
	store, db, _ := newTestStore(t)
	// Seed 50 eligible probation rows. Cancel the context
	// before the tick — the tick should return ctx.Err()
	// without crashing.
	for i := 0; i < 50; i++ {
		id := "cap_" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		seedProbationRow(t, db, id, 5, 0, 5, 0.10)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	_, err := store.CheckProbationCompletion(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

// seedProbationRow inserts a capability directly in state='probation'
// with the given metrics. Mirrors seedCapability but adds the
// probation fields with explicit values.
func seedProbationRow(t *testing.T, db *sql.DB, id string, success, failure, required int, maxRate float64) {
	t.Helper()
	q := `INSERT INTO capabilities
	      (id, name, purpose, source_code, source_language, source_hash,
	       state, execution_domain, state_changed_at,
	       author_agent, created_from_id,
	       probation_required_success_count, probation_max_failure_rate,
	       success_count, failure_count,
	       tags, created_at, updated_at, metadata)
	      VALUES (?, ?, 'test', 'echo', 'bash', 'deadbeef',
	              'probation', 'sandbox', 1700000000,
	              '', NULL,
	              ?, ?,
	              ?, ?,
	              '[]', 1700000000, 1700000000, '{}')`
	if _, err := db.Exec(q, id, id, required, maxRate, success, failure); err != nil {
		t.Fatalf("seedProbationRow: %v", err)
	}
}

// seedProbationRowWithState is the variant for the "ignore
// non-probation states" test. The state is parameterised.
func seedProbationRowWithState(t *testing.T, db *sql.DB, id, state string, success, failure, required int, maxRate float64) {
	t.Helper()
	q := `INSERT INTO capabilities
	      (id, name, purpose, source_code, source_language, source_hash,
	       state, execution_domain, state_changed_at,
	       author_agent, created_from_id,
	       probation_required_success_count, probation_max_failure_rate,
	       success_count, failure_count,
	       tags, created_at, updated_at, metadata)
	      VALUES (?, ?, 'test', 'echo', 'bash', 'deadbeef',
	              ?, 'sandbox', 1700000000,
	              '', NULL,
	              ?, ?,
	              ?, ?,
	              '[]', 1700000000, 1700000000, '{}')`
	if _, err := db.Exec(q, id, id, state, required, maxRate, success, failure); err != nil {
		t.Fatalf("seedProbationRowWithState: %v", err)
	}
}
