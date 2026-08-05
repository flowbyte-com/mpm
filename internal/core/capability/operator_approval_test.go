// operator_approval_test.go — MarkOperatorApproved Store tests
//
// Pins the contract between `mpm capability grant-operator` and the
// executor's operator-domain gate (executor.go int64FromMeta):
//
//   * Stamping an active capability sets metadata.operator_approved_at
//     (int64 unix epoch) AND metadata.operator_approved_at_rfc3339
//     (human-readable string), AND writes a capability_events row
//     with event_type='operator_approval'.
//   * Stamping preserves existing metadata keys (the operator can
//     re-stamp without losing custom governance flags).
//   * Stamping a soft-deleted row returns ErrNotFound (the gate can't
//     approve what isn't there).
//   * Stamping a retired capability returns ErrOperatorApprovalRefused.
//   * Stamping a fractured capability returns ErrOperatorApprovalRefused.
//   * Idempotency: re-stamping refreshes the timestamp + event row.
//   * Stamping a non-existent id returns ErrNotFound.
//   * The capability_events row's metadata carries the RFC3339
//     timestamp + actor + reason (audit-trail fidelity).
//   * Empty actor defaults to "operator" (no panic on unset).
//   * Zero approvedAt uses Store.Now() (the frozen clock).
//
// Pattern: seedCapability inserts a row directly via SQL, then
// MarkOperatorApproved exercises the Store's tx + metadata-merge
// logic. We don't use InsertCapabilityProposal here because we
// want precise control over the starting state (some tests start
// in StateActive, some in StateRetired, etc.).
package capability

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// countOperatorApprovalEvents returns the count of capability_events
// rows with event_type='operator_approval' for the given capability id.
func countOperatorApprovalEvents(t *testing.T, db *sql.DB, capID string) int {
	t.Helper()
	var n int
	row := db.QueryRow(`SELECT COUNT(*) FROM capability_events
	                   WHERE capability_id = ? AND event_type = 'operator_approval'`, capID)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("countOperatorApprovalEvents: %v", err)
	}
	return n
}

// =============================================================================
// Happy path: stamping a StateActive capability produces the right
// metadata + event row.
// =============================================================================

func TestStore_MarkOperatorApproved_HappyPath(t *testing.T) {
	store, db, clk := newTestStore(t)

	// Seed an active capability.
	seedCapability(t, db, "cap-under-test", "under-test", StateActive, nil)

	// Pin the frozen clock to T=2026-08-05T17:42:01Z (unix epoch 1781211721).
	want := time.Date(2026, 8, 5, 17, 42, 1, 0, time.UTC)
	clk.store(want.Unix())

	got, err := store.MarkOperatorApproved("cap-under-test", "alice", want, "reviewed by sec-team")
	if err != nil {
		t.Fatalf("MarkOperatorApproved: %v", err)
	}
	if got == nil {
		t.Fatal("expected returned capability, got nil")
	}

	// 1. Metadata has operator_approved_at — JSON round-trip turns
	// int64 into float64 by default. The executor's int64FromMeta
	// handles that case (executor.go:216); what we care about
	// here is that the numeric value survived intact.
	if v, ok := got.Metadata["operator_approved_at"]; !ok {
		t.Fatalf("metadata.operator_approved_at not set; got %+v", got.Metadata)
	} else {
		// Use int64FromMeta to validate the gate-side contract.
		gotVal := int64FromMeta(got.Metadata, "operator_approved_at")
		if gotVal != want.Unix() {
			t.Errorf("operator_approved_at = %d, want %d (via int64FromMeta)",
				gotVal, want.Unix())
		}
		// And assert the float64 numeric form (the JSON-default
		// decode type) carries the exact value.
		if f, ok := v.(float64); ok {
			if int64(f) != want.Unix() {
				t.Errorf("operator_approved_at float64 = %v, want %v",
					f, want.Unix())
			}
		}
	}

	// 2. Metadata has operator_approved_at_rfc3339 = RFC3339 string.
	if v, ok := got.Metadata["operator_approved_at_rfc3339"]; !ok {
		t.Fatalf("metadata.operator_approved_at_rfc3339 not set")
	} else {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("operator_approved_at_rfc3339 type = %T, want string", v)
		}
		// Parse and compare — RFC3339 preserves zone, so round-trip
		// via time.Parse + Equal to tolerate formatting differences.
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("RFC3339 parse failed: %v (got %q)", err, s)
		}
		if !parsed.Equal(want) {
			t.Errorf("RFC3339 timestamp = %v, want %v", parsed, want)
		}
	}

	// 3. Metadata has operator_approved_by = "alice".
	if got.Metadata["operator_approved_by"] != "alice" {
		t.Errorf("operator_approved_by = %v, want alice",
			got.Metadata["operator_approved_by"])
	}

	// 4. Capability_events row exists with the right metadata.
	if n := countOperatorApprovalEvents(t, db, "cap-under-test"); n != 1 {
		t.Fatalf("capability_events count = %d, want 1", n)
	}
	var evMeta string
	if err := db.QueryRow(`SELECT metadata FROM capability_events
	                       WHERE capability_id = ? AND event_type = 'operator_approval'`,
		"cap-under-test").Scan(&evMeta); err != nil {
		t.Fatalf("event metadata read: %v", err)
	}
	for _, wantSub := range []string{
		`"operator_approved_at_rfc3339":"2026-08-05T17:42:01Z"`,
		`"operator_approved_by":"alice"`,
		`"reason":"reviewed by sec-team"`,
	} {
		if !strings.Contains(evMeta, wantSub) {
			t.Errorf("event metadata missing %q (got %q)", wantSub, evMeta)
		}
	}
}

// =============================================================================
// Existing metadata preservation: re-stamping must not lose custom keys.
// =============================================================================

func TestStore_MarkOperatorApproved_PreservesExistingMetadata(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Seed a row, then overwrite metadata with custom keys.
	seedCapability(t, db, "cap-under-test", "under-test", StateActive, nil)
	const customJSON = `{"owner":"platform-team","priority":"high"}`
	if _, err := db.Exec(`UPDATE capabilities SET metadata = ? WHERE id = ?`,
		customJSON, "cap-under-test"); err != nil {
		t.Fatalf("metadata update: %v", err)
	}

	want := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	got, err := store.MarkOperatorApproved("cap-under-test", "bob", want, "")
	if err != nil {
		t.Fatalf("MarkOperatorApproved: %v", err)
	}

	// Custom keys preserved.
	if got.Metadata["owner"] != "platform-team" {
		t.Errorf("metadata.owner = %v, want platform-team (must not be clobbered)",
			got.Metadata["owner"])
	}
	if got.Metadata["priority"] != "high" {
		t.Errorf("metadata.priority = %v, want high", got.Metadata["priority"])
	}
	// Stamp keys added.
	if got.Metadata["operator_approved_by"] != "bob" {
		t.Errorf("operator_approved_by = %v, want bob", got.Metadata["operator_approved_by"])
	}
}

// =============================================================================
// State guards: retired and fractured refuse with ErrOperatorApprovalRefused.
// =============================================================================

func TestStore_MarkOperatorApproved_RefusesRetiredState(t *testing.T) {
	store, db, _ := newTestStore(t)
	seedCapability(t, db, "cap-under-test", "under-test", StateRetired, nil)

	_, err := store.MarkOperatorApproved("cap-under-test", "alice", time.Time{}, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrOperatorApprovalRefused) {
		t.Fatalf("err = %v, want ErrOperatorApprovalRefused", err)
	}
	if !strings.Contains(err.Error(), "retired") {
		t.Errorf("error should name the state: %v", err)
	}

	// And no event row was written.
	if n := countOperatorApprovalEvents(t, db, "cap-under-test"); n != 0 {
		t.Errorf("event count = %d, want 0 (refused write)", n)
	}
}

func TestStore_MarkOperatorApproved_RefusesFracturedState(t *testing.T) {
	store, db, _ := newTestStore(t)
	seedCapability(t, db, "cap-under-test", "under-test", StateFractured, nil)

	_, err := store.MarkOperatorApproved("cap-under-test", "alice", time.Time{}, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrOperatorApprovalRefused) {
		t.Fatalf("err = %v, want ErrOperatorApprovalRefused", err)
	}
	if !strings.Contains(err.Error(), "fractured") {
		t.Errorf("error should name the state: %v", err)
	}
}

// =============================================================================
// Soft-deleted: ErrNotFound (no gate can approve a shredded row).
// =============================================================================

func TestStore_MarkOperatorApproved_SoftDeletedReturnsNotFound(t *testing.T) {
	store, db, _ := newTestStore(t)
	seedCapability(t, db, "cap-under-test", "under-test", StateActive, nil)
	if _, err := db.Exec(`UPDATE capabilities SET deleted_at = ? WHERE id = ?`,
		1700000099, "cap-under-test"); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	_, err := store.MarkOperatorApproved("cap-under-test", "alice", time.Time{}, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStore_MarkOperatorApproved_NonExistentReturnsNotFound(t *testing.T) {
	store, _, _ := newTestStore(t)
	_, err := store.MarkOperatorApproved("cap-does-not-exist", "alice", time.Time{}, "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// =============================================================================
// Idempotency: re-stamping refreshes the timestamp + event row.
// =============================================================================

func TestStore_MarkOperatorApproved_Idempotent_RestampRefreshesTimestamp(t *testing.T) {
	store, db, clk := newTestStore(t)
	seedCapability(t, db, "cap-under-test", "under-test", StateActive, nil)

	first := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	clk.store(first.Unix())
	if _, err := store.MarkOperatorApproved("cap-under-test", "alice", first, "first"); err != nil {
		t.Fatalf("first stamp: %v", err)
	}

	// Advance clock and re-stamp with a different actor + reason.
	second := time.Date(2026, 8, 6, 14, 30, 0, 0, time.UTC)
	clk.store(second.Unix())
	got, err := store.MarkOperatorApproved("cap-under-test", "carol", second, "second")
	if err != nil {
		t.Fatalf("second stamp: %v", err)
	}

	// Timestamp updated (via int64FromMeta to validate gate-side contract).
	if gotVal := int64FromMeta(got.Metadata, "operator_approved_at"); gotVal != second.Unix() {
		t.Errorf("operator_approved_at = %d, want %d (second.Unix())",
			gotVal, second.Unix())
	}
	if got.Metadata["operator_approved_by"] != "carol" {
		t.Errorf("operator_approved_by = %v, want carol", got.Metadata["operator_approved_by"])
	}

	// Two event rows now.
	if n := countOperatorApprovalEvents(t, db, "cap-under-test"); n != 2 {
		t.Errorf("event count = %d, want 2 (first + second)", n)
	}
}

// =============================================================================
// Default actor: empty actor defaults to "operator".
// =============================================================================

func TestStore_MarkOperatorApproved_EmptyActorDefaultsToOperator(t *testing.T) {
	store, db, _ := newTestStore(t)
	seedCapability(t, db, "cap-under-test", "under-test", StateActive, nil)

	got, err := store.MarkOperatorApproved("cap-under-test", "", time.Time{}, "")
	if err != nil {
		t.Fatalf("MarkOperatorApproved: %v", err)
	}
	if got.Metadata["operator_approved_by"] != "operator" {
		t.Errorf("operator_approved_by = %v, want operator (default)",
			got.Metadata["operator_approved_by"])
	}
}

// =============================================================================
// Zero approvedAt uses Store.Now() (frozen clock).
// =============================================================================

func TestStore_MarkOperatorApproved_ZeroApprovedAtUsesStoreClock(t *testing.T) {
	store, db, clk := newTestStore(t)
	seedCapability(t, db, "cap-under-test", "under-test", StateActive, nil)

	frozen := int64(1781211721) // 2026-08-05T17:42:01Z
	clk.store(frozen)

	got, err := store.MarkOperatorApproved("cap-under-test", "alice", time.Time{}, "")
	if err != nil {
		t.Fatalf("MarkOperatorApproved: %v", err)
	}
	if gotVal := int64FromMeta(got.Metadata, "operator_approved_at"); gotVal != frozen {
		t.Errorf("operator_approved_at = %d, want %d (frozen clock)",
			gotVal, frozen)
	}
}

// =============================================================================
// Empty id: refused with descriptive error (not a DB error).
// =============================================================================

func TestStore_MarkOperatorApproved_EmptyIDRefused(t *testing.T) {
	store, _, _ := newTestStore(t)
	_, err := store.MarkOperatorApproved("", "alice", time.Time{}, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "id is required") {
		t.Errorf("error should mention id requirement: %v", err)
	}
}
