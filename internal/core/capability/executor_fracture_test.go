package capability

import (
	"context"
	"database/sql"
	"testing"
)

// =============================================================================
// executor_fracture_test.go — EX-7 sliding-window detector + §2.4.1 wake
//
// Pins the load-bearing contracts:
//
//   * FailureCountSince counts only exit_code != 0 rows within
//     the requested window — successes, exit-0 rows, and rows
//     outside the window are excluded.
//   * FailureCountSince returns 0 (not an error) for a capability
//     with no invocation history — the detector is a count,
//     not a fetch.
//   * checkFracture fires when the window crosses the 3-in-60s
//     threshold AND the capability is in StateDegraded.
//   * checkFracture is a no-op when the threshold is not crossed.
//   * checkFracture is a no-op when the capability is in any
//     state other than StateDegraded (the spec's matrix
//     restricts the fracture transition to degraded → fractured).
//   * checkFracture is a no-op on a missing capability.
//   * When the capability carries author_theory_id, the fracture
//     transition ALSO writes a row to epistemic_cascade_outbox
//     with downstream_artifact_id = the upstream theory. When
//     author_theory_id is NULL, no cascade row is written.
//   * Re-firing on an already-fractured capability is a no-op.
//
// All tests use newTestStore (in-memory SQLite) + seedCapability
// + direct INSERTs against capability_invocations. Time is
// controlled via the frozen clock so the sliding-window math
// is deterministic.
// =============================================================================

// mustInsertInvocation inserts one capability_invocations row
// at the given Unix-epoch second with the given exit code.
// Bypasses Store.RecordInvocation so tests can fast-forward
// time and write rows directly (Store.RecordInvocation uses
// the same Now() as everything else; we need explicit clock
// control).
func mustInsertInvocation(t *testing.T, db *sql.DB, capID string, at int64, exitCode int) {
	t.Helper()
	q := `INSERT INTO capability_invocations
	      (id, capability_id, invoked_at, exit_code, duration_ms, cascade_invalidated)
	      VALUES (?, ?, ?, ?, 100, 0)`
	id := "inv-" + capID + "-" + itoa(at) + "-" + itoa(int64(exitCode))
	if _, err := db.Exec(q, id, capID, at, exitCode); err != nil {
		t.Fatalf("mustInsertInvocation: %v", err)
	}
}

// itoa is a tiny strconv replacement so the test file stays
// import-light. Negative numbers aren't used here; the
// signature matches strconv.Itoa.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// countCascadeOutboxRows returns the number of rows in the
// epistemic_cascade_outbox table where downstream_artifact_id
// matches. Used by tests to assert the §2.4.1 cascade
// emission fired (or didn't).
func countCascadeOutboxRows(t *testing.T, store *Store, downstreamID string) int {
	t.Helper()
	var n int
	q := `SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE downstream_artifact_id = ?`
	row := store.dm.QueryRowTracked(q, downstreamID)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("countCascadeOutboxRows: %v", err)
	}
	return n
}

// seedCapabilityWithTheory inserts a capability row in the
// requested state WITH author_theory_id populated. Mirrors
// seedCapability (store_test.go) but populates the theory FK
// column so cascade-emission tests can exercise the §2.4.1
// wake path.
//
// author_theory_id has FOREIGN KEY ... REFERENCES memories(id),
// so the helper also inserts a stub memory row of
// collection='theories' to satisfy the constraint. The stub's
// content is the theoryID itself for forensic traceability.
func seedCapabilityWithTheory(t *testing.T, db *sql.DB, id, name string, state CapabilityState, theoryID string) {
	t.Helper()
	// Stub theory row — FK target. collection='theories' is
	// what the substrate uses for theory rows; the content
	// is the theoryID for debuggability.
	qTheory := `INSERT INTO memories (id, collection, content, created_at, updated_at, weight)
	             VALUES (?, 'theories', ?, ?, ?, 50)`
	if _, err := db.Exec(qTheory, theoryID, theoryID, 1700000000, 1700000000); err != nil {
		t.Fatalf("seedCapabilityWithTheory (theory stub): %v", err)
	}

	q := `INSERT INTO capabilities
	      (id, name, purpose, source_code, source_language, source_hash,
	       state, execution_domain, state_changed_at,
	       author_theory_id, author_agent,
	       probation_required_success_count, probation_max_failure_rate,
	       tags, created_at, updated_at, metadata)
	      VALUES (?, ?, 'test', 'echo', 'bash', 'deadbeef',
	              ?, 'sandbox', ?,
	              ?, '',
	              5, 0.10,
	              '[]', ?, ?, '{}')`
	if _, err := db.Exec(q, id, name, state, 1700000000,
		theoryID, 1700000000, 1700000000); err != nil {
		t.Fatalf("seedCapabilityWithTheory: %v", err)
	}
}

// =============================================================================
// FailureCountSince tests
// =============================================================================

// TestFailureCountSince_EmptyCapability: a capability with no
// invocation rows returns 0, not an error. The detector is
// a count, not a fetch.
func TestFailureCountSince_EmptyCapability(t *testing.T) {
	store, _, _ := newTestStore(t)
	ctx := context.Background()

	got, err := store.FailureCountSince(ctx, "nonexistent-cap", 0)
	if err != nil {
		t.Fatalf("FailureCountSince: %v", err)
	}
	if got != 0 {
		t.Fatalf("expected 0 for unknown capability, got %d", got)
	}
}

// TestFailureCountSince_CountsOnlyFailures: successes inside
// the window are excluded. exit_code=0 rows do not increment
// the failure count regardless of timestamp.
func TestFailureCountSince_CountsOnlyFailures(t *testing.T) {
	store, db, _ := newTestStore(t)
	seedCapability(t, db, "cap-x", "cap-x", StateActive, nil)
	ctx := context.Background()

	mustInsertInvocation(t, db, "cap-x", 1700000000, 1) // fail
	mustInsertInvocation(t, db, "cap-x", 1700000010, 0) // ok
	mustInsertInvocation(t, db, "cap-x", 1700000020, 2) // fail
	mustInsertInvocation(t, db, "cap-x", 1700000030, 0) // ok
	mustInsertInvocation(t, db, "cap-x", 1700000040, 0) // ok

	got, err := store.FailureCountSince(ctx, "cap-x", 0)
	if err != nil {
		t.Fatalf("FailureCountSince: %v", err)
	}
	if got != 2 {
		t.Fatalf("expected 2 failures (exit_code != 0), got %d", got)
	}
}

// TestFailureCountSince_RespectsWindow: rows older than the
// `since` cutoff are excluded. The window is inclusive at
// `since` (>=) and exclusive above.
func TestFailureCountSince_RespectsWindow(t *testing.T) {
	store, db, _ := newTestStore(t)
	seedCapability(t, db, "cap-x", "cap-x", StateActive, nil)
	ctx := context.Background()

	mustInsertInvocation(t, db, "cap-x", 1700000000, 1) // too old
	mustInsertInvocation(t, db, "cap-x", 1700000100, 1) // in window
	mustInsertInvocation(t, db, "cap-x", 1700000200, 1) // in window

	got, err := store.FailureCountSince(ctx, "cap-x", 1700000050)
	if err != nil {
		t.Fatalf("FailureCountSince: %v", err)
	}
	if got != 2 {
		t.Fatalf("expected 2 failures inside window (since=1700000050), got %d", got)
	}
}

// TestFailureCountSince_RejectsNegativeSince: since < 0 is a
// caller bug, surfaced as an error rather than silently
// returning 0.
func TestFailureCountSince_RejectsNegativeSince(t *testing.T) {
	store, _, _ := newTestStore(t)
	_, err := store.FailureCountSince(context.Background(), "cap-x", -1)
	if err == nil {
		t.Fatal("expected error for negative since, got nil")
	}
}

// TestFailureCountSince_RejectsEmptyID: empty capability ID
// is rejected.
func TestFailureCountSince_RejectsEmptyID(t *testing.T) {
	store, _, _ := newTestStore(t)
	_, err := store.FailureCountSince(context.Background(), "", 0)
	if err == nil {
		t.Fatal("expected error for empty capabilityID, got nil")
	}
}

// =============================================================================
// checkFracture tests
// =============================================================================

// TestCheckFracture_ThresholdNotMet: 2 failures in the window
// is below the 3-in-60s threshold. The detector must not
// transition the capability.
func TestCheckFracture_ThresholdNotMet(t *testing.T) {
	store, db, clk := newTestStore(t)
	seedCapability(t, db, "cap-x", "cap-x", StateDegraded, nil)
	ctx := context.Background()

	mustInsertInvocation(t, db, "cap-x", 1700000000, 1)
	mustInsertInvocation(t, db, "cap-x", 1700000010, 1)

	clk.store(1700000050) // 50s after the first failure; both inside window

	fired, err := store.checkFracture(ctx, "cap-x")
	if err != nil {
		t.Fatalf("checkFracture: %v", err)
	}
	if fired {
		t.Fatal("expected no fracture for 2-in-60s, got fired=true")
	}

	c, err := store.GetCapability("cap-x")
	if err != nil {
		t.Fatalf("GetCapability: %v", err)
	}
	if c.State != StateDegraded {
		t.Fatalf("expected state=degraded (unchanged), got %s", c.State)
	}
}

// TestCheckFracture_ThresholdMet_NoTheory: 3 failures in 60s
// AND state=degraded triggers the fracture transition. No
// author_theory_id → no cascade wake written.
func TestCheckFracture_ThresholdMet_NoTheory(t *testing.T) {
	store, db, clk := newTestStore(t)
	seedCapability(t, db, "cap-x", "cap-x", StateDegraded, nil)
	ctx := context.Background()

	mustInsertInvocation(t, db, "cap-x", 1700000000, 1)
	mustInsertInvocation(t, db, "cap-x", 1700000010, 1)
	mustInsertInvocation(t, db, "cap-x", 1700000020, 1)
	clk.store(1700000050) // 50s after the first failure; all inside window

	fired, err := store.checkFracture(ctx, "cap-x")
	if err != nil {
		t.Fatalf("checkFracture: %v", err)
	}
	if !fired {
		t.Fatal("expected fired=true for 3-in-60s from degraded, got false")
	}

	c, err := store.GetCapability("cap-x")
	if err != nil {
		t.Fatalf("GetCapability: %v", err)
	}
	if c.State != StateFractured {
		t.Fatalf("expected state=fractured, got %s", c.State)
	}

	// No author_theory_id → cascade outbox should be empty.
	if n := countCascadeOutboxRows(t, store, "cap-x"); n != 0 {
		t.Fatalf("expected 0 cascade rows for capability with no theory, got %d", n)
	}
}

// TestCheckFracture_ThresholdMet_WithTheory: 3 failures in
// 60s + state=degraded + author_theory_id set triggers both
// the fracture transition AND the §2.4.1 cascade wake.
func TestCheckFracture_ThresholdMet_WithTheory(t *testing.T) {
	store, db, clk := newTestStore(t)
	const theoryID = "theory-abc-123"
	seedCapabilityWithTheory(t, db, "cap-x", "cap-x", StateDegraded, theoryID)
	ctx := context.Background()

	mustInsertInvocation(t, db, "cap-x", 1700000000, 1)
	mustInsertInvocation(t, db, "cap-x", 1700000010, 1)
	mustInsertInvocation(t, db, "cap-x", 1700000020, 1)
	clk.store(1700000050)

	fired, err := store.checkFracture(ctx, "cap-x")
	if err != nil {
		t.Fatalf("checkFracture: %v", err)
	}
	if !fired {
		t.Fatal("expected fired=true for 3-in-60s, got false")
	}

	c, err := store.GetCapability("cap-x")
	if err != nil {
		t.Fatalf("GetCapability: %v", err)
	}
	if c.State != StateFractured {
		t.Fatalf("expected state=fractured, got %s", c.State)
	}

	// §2.4.1 cascade wake: one row in the outbox with
	// downstream_artifact_id = the theory id.
	if n := countCascadeOutboxRows(t, store, theoryID); n != 1 {
		t.Fatalf("expected 1 cascade row for theory %s, got %d", theoryID, n)
	}
}

// TestCheckFracture_ThresholdMet_NotDegraded: 3 failures in
// 60s but the capability is in StateActive. The detector must
// NOT transition — the spec's matrix restricts fracture to
// degraded → fractured; active must first be demoted via the
// soft threshold.
func TestCheckFracture_ThresholdMet_NotDegraded(t *testing.T) {
	store, db, clk := newTestStore(t)
	seedCapability(t, db, "cap-x", "cap-x", StateActive, nil)
	ctx := context.Background()

	mustInsertInvocation(t, db, "cap-x", 1700000000, 1)
	mustInsertInvocation(t, db, "cap-x", 1700000010, 1)
	mustInsertInvocation(t, db, "cap-x", 1700000020, 1)
	clk.store(1700000050)

	fired, err := store.checkFracture(ctx, "cap-x")
	if err != nil {
		t.Fatalf("checkFracture: %v", err)
	}
	if fired {
		t.Fatal("expected fired=false for active (not degraded), got true")
	}

	c, err := store.GetCapability("cap-x")
	if err != nil {
		t.Fatalf("GetCapability: %v", err)
	}
	if c.State != StateActive {
		t.Fatalf("expected state=active (unchanged), got %s", c.State)
	}
}

// TestCheckFracture_OutsideWindow: 3 failures but all outside
// the 60s window. The detector must not transition.
func TestCheckFracture_OutsideWindow(t *testing.T) {
	store, db, clk := newTestStore(t)
	seedCapability(t, db, "cap-x", "cap-x", StateDegraded, nil)
	ctx := context.Background()

	// Three failures spread across 200s — all outside the
	// 60s window when "now" = 1700000300.
	mustInsertInvocation(t, db, "cap-x", 1700000100, 1) // -200s from now
	mustInsertInvocation(t, db, "cap-x", 1700000150, 1) // -150s from now
	mustInsertInvocation(t, db, "cap-x", 1700000200, 1) // -100s from now
	clk.store(1700000300)

	fired, err := store.checkFracture(ctx, "cap-x")
	if err != nil {
		t.Fatalf("checkFracture: %v", err)
	}
	if fired {
		t.Fatal("expected fired=false for failures outside window, got true")
	}

	c, err := store.GetCapability("cap-x")
	if err != nil {
		t.Fatalf("GetCapability: %v", err)
	}
	if c.State != StateDegraded {
		t.Fatalf("expected state=degraded (unchanged), got %s", c.State)
	}
}

// TestCheckFracture_Idempotent: re-firing the detector on an
// already-fractured capability is a clean no-op.
func TestCheckFracture_Idempotent(t *testing.T) {
	store, db, clk := newTestStore(t)
	seedCapability(t, db, "cap-x", "cap-x", StateDegraded, nil)
	ctx := context.Background()

	mustInsertInvocation(t, db, "cap-x", 1700000000, 1)
	mustInsertInvocation(t, db, "cap-x", 1700000010, 1)
	mustInsertInvocation(t, db, "cap-x", 1700000020, 1)
	clk.store(1700000050)

	// First fire: should fracture.
	if _, err := store.checkFracture(ctx, "cap-x"); err != nil {
		t.Fatalf("first checkFracture: %v", err)
	}
	c, _ := store.GetCapability("cap-x")
	if c.State != StateFractured {
		t.Fatalf("after first fire: expected fractured, got %s", c.State)
	}

	// Second fire: no-op, no error.
	fired2, err := store.checkFracture(ctx, "cap-x")
	if err != nil {
		t.Fatalf("second checkFracture: %v", err)
	}
	if fired2 {
		t.Fatal("second checkFracture should be no-op, got fired=true")
	}
}

// TestCheckFracture_CapabilityMissing: detector on a missing
// capability returns (false, nil) — the row was deleted
// between Invoke and checkFracture.
func TestCheckFracture_CapabilityMissing(t *testing.T) {
	store, _, _ := newTestStore(t)
	ctx := context.Background()

	fired, err := store.checkFracture(ctx, "ghost-cap")
	if err != nil {
		t.Fatalf("checkFracture on missing: %v", err)
	}
	if fired {
		t.Fatal("expected fired=false for missing capability")
	}
}

// TestCheckFracture_NilStore: EX-1 test convenience — a nil
// Store must not panic. Returns (false, nil) so the test
// helpers can safely call it on partially-constructed stores.
func TestCheckFracture_NilStore(t *testing.T) {
	var s *Store
	fired, err := s.checkFracture(context.Background(), "cap-x")
	if err != nil {
		t.Fatalf("nil-store checkFracture: %v", err)
	}
	if fired {
		t.Fatal("nil-store should return fired=false")
	}
}

// =============================================================================
// Threshold / constants tests
// =============================================================================

// TestFractureConstantsMatchSpec: pins the public constants
// at the values the spec mandates (3 failures in 60s). If a
// future patch tunes them, this test will fail and force the
// reviewer to update the spec alongside the code.
func TestFractureConstantsMatchSpec(t *testing.T) {
	if FractureThreshold != 3 {
		t.Fatalf("FractureThreshold: spec requires 3, got %d", FractureThreshold)
	}
	if FractureWindowSeconds != 60 {
		t.Fatalf("FractureWindowSeconds: spec requires 60, got %d", FractureWindowSeconds)
	}
}