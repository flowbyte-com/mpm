package tools

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"testing"

	"mpm/internal"
)

// newTestDM opens a fresh test DM with a temp DB. Caller must Close().
func newTestSharedDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("MPM_SHARED_DB", filepath.Join(tmp, "shared.db"))
	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	return dm
}

// TestHandleRecordGlobalRule_RejectsMissingConfirm verifies the
// operator gate: the handler refuses the call when confirm=true is
// not present. Without this gate, agents could autonomously write
// house rules — the original concern from WISHLIST.md Phase 3.
func TestHandleRecordGlobalRule_RejectsMissingConfirm(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleRecordGlobalRule(dm, internal.ActiveContext{}, map[string]interface{}{
		"fact": "test rule",
		// confirm intentionally omitted
	})
	if err == nil {
		t.Fatal("expected error when confirm missing, got nil")
	}
	if !strings.Contains(err.Error(), "confirm=true") {
		t.Errorf("error message should mention confirm=true, got: %v", err)
	}
}

// TestHandleRecordGlobalRule_HappyPath verifies the full round-trip:
// handler accepts confirm=true, writes to shared DB, returns id.
func TestHandleRecordGlobalRule_HappyPath(t *testing.T) {
	dm := newTestSharedDM(t)

	result, err := handleRecordGlobalRule(dm, internal.ActiveContext{}, map[string]interface{}{
		"fact":       "Always quote shell variables",
		"tags":       "shell,safety",
		"weight":     10,
		"provenance": "operator-test",
		"confirm":    true,
	})
	if err != nil {
		t.Fatalf("handleRecordGlobalRule: %v", err)
	}
	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", result)
	}
	if m["id"] == "" || m["id"] == nil {
		t.Errorf("expected non-empty id, got %v", m["id"])
	}
	if m["source"] != "shared" {
		t.Errorf("expected source=shared, got %v", m["source"])
	}
}

// TestHandlePromoteToGlobal_RejectsMissingConfirm verifies the same
// operator gate for promotion.
func TestHandlePromoteToGlobal_RejectsMissingConfirm(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handlePromoteToGlobal(dm, internal.ActiveContext{}, map[string]interface{}{
		"memory_id": "any-id",
		// confirm intentionally omitted
	})
	if err == nil {
		t.Fatal("expected error when confirm missing, got nil")
	}
	if !strings.Contains(err.Error(), "confirm=true") {
		t.Errorf("error message should mention confirm=true, got: %v", err)
	}
}

// TestHandlePromoteToGlobal_HappyPath verifies promotion round-trip.
func TestHandlePromoteToGlobal_HappyPath(t *testing.T) {
	dm := newTestSharedDM(t)

	// Insert a local memory to promote. The id is unique-per-run to
	// avoid collision with prior runs (the workspace DB persists
	// across tests).
	localPromoteID := fmt.Sprintf("local-promote-%d", time.Now().UnixNano())
	_, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, weight, deleted_at, created_at, updated_at)
		 VALUES (?, 'memories', 'a tip worth promoting', 7, NULL, '2026-06-26', '2026-06-26')`,
		localPromoteID,
	)
	if err != nil {
		t.Fatalf("insert local: %v", err)
	}

	result, err := handlePromoteToGlobal(dm, internal.ActiveContext{}, map[string]interface{}{
		"memory_id": "local-promote-001",
		"confirm":   true,
	})
	if err != nil {
		t.Fatalf("handlePromoteToGlobal: %v", err)
	}
	m := result.(map[string]interface{})
	if m["local_id"] != "local-promote-001" {
		t.Errorf("local_id mismatch: %v", m["local_id"])
	}
	if m["shared_id"] == "" || m["shared_id"] == nil {
		t.Errorf("shared_id empty: %v", m["shared_id"])
	}
}


// ── Phase 5a: scheduled_wakes handler tests ───────────────────────────

func TestHandleScheduleWake_HappyPath(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	result, err := handleScheduleWake(dm, internal.ActiveContext{}, map[string]interface{}{
		"reason":      "check WC2026 R32 result for theory 7383f1572d73bc9a",
		"target_time": "24h",
		"theory_id":   "7383f1572d73bc9a",
	})
	if err != nil {
		t.Fatalf("handleScheduleWake: %v", err)
	}
	m := result.(map[string]interface{})
	if m["id"] == nil || m["id"] == "" {
		t.Error("expected non-empty id")
	}
	if m["theory_id"] != "7383f1572d73bc9a" {
		t.Errorf("theory_id: got %v, want 7383f1572d73bc9a", m["theory_id"])
	}
	tt, _ := m["target_time"].(int64)
	if tt < time.Now().Unix()+3600 || tt > time.Now().Unix()+90000 {
		t.Errorf("target_time out of expected 24h window: got %d, now=%d", tt, time.Now().Unix())
	}
}

func TestHandleScheduleWake_RejectsMissing(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	if _, err := handleScheduleWake(dm, internal.ActiveContext{}, map[string]interface{}{
		"target_time": "24h",
	}); err == nil {
		t.Error("expected error when reason missing")
	}
	if _, err := handleScheduleWake(dm, internal.ActiveContext{}, map[string]interface{}{
		"reason": "test",
	}); err == nil {
		t.Error("expected error when target_time missing")
	}
}

func TestHandleScheduleWake_FoldsDueWakesInline(t *testing.T) {
	// When schedule_wake is called with a past target_time, the wake is
	// due immediately. The opportunistic fold means schedule_wake ITSELF
	// surfaces the wake in its own response (WakesPending block), not
	// requiring a separate check_wakes call. This is the architecture: every
	// write call also reads due writes. Verified here by checking the
	// direct return value of handleScheduleWake.
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	past := time.Now().Add(-1 * time.Minute).Unix()
	res, err := handleScheduleWake(dm, internal.ActiveContext{}, map[string]interface{}{
		"reason":      "due now",
		"target_time": fmt.Sprintf("%d", past),
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	m := res.(map[string]interface{})
	pending, ok := m["WakesPending"].([]map[string]interface{})
	if !ok {
		t.Fatalf("WakesPending missing or wrong type: %T", m["WakesPending"])
	}
	if len(pending) != 1 {
		t.Errorf("expected 1 pending wake surfaced inline, got %d", len(pending))
	}
	if pending[0]["reason"] != "due now" {
		t.Errorf("pending wake reason: got %v, want 'due now'", pending[0]["reason"])
	}

	// Subsequent check_wakes should return 0 (the wake was already fired
	// by the inline fold).
	res2, err := handleCheckWakes(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	m2 := res2.(map[string]interface{})
	if count, _ := m2["WakesPendingCount"].(int); count != 0 {
		t.Errorf("post-fold check: got %d wakes, want 0 (already fired inline)", count)
	}
}

func TestHandleCheckWakes_ExposesFutureWakeAfterBackdate(t *testing.T) {
	// Schedule a wake with a future target, then backdate it via SQL to
	// simulate time passing. handleCheckWakes should then surface it on
	// the next call — the agent does not need to wait for the daemon
	// (because there is no daemon).
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	future := time.Now().Add(2 * time.Hour).Unix()
	res, err := handleScheduleWake(dm, internal.ActiveContext{}, map[string]interface{}{
		"reason":      "future wake",
		"target_time": fmt.Sprintf("%d", future),
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	m := res.(map[string]interface{})
	if id, _ := m["id"].(string); id == "" {
		t.Fatal("schedule_wake returned no id")
	}
	// Should NOT surface now (target is 2h in future).
	if count, _ := m["WakesPendingCount"].(int); count != 0 {
		t.Errorf("future wake surfaced too early: got %d pending, want 0", count)
	}

	// Backdate to the past via SQL
	_, err = dm.SQLDB().Exec(`UPDATE scheduled_wakes SET target_time = ?`, time.Now().Add(-1*time.Minute).Unix())
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}

	res2, err := handleCheckWakes(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	m2 := res2.(map[string]interface{})
	if count, _ := m2["WakesPendingCount"].(int); count != 1 {
		t.Errorf("after backdate: got %d pending, want 1", count)
	}
}

func TestHandleListWakes_DefaultsAndFilters(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`DELETE FROM scheduled_wakes`)

	// Schedule TWO future wakes so the opportunistic fold does not fire
	// either of them. The list test then verifies filter semantics in
	// isolation; the past-target behavior is covered by the fold test.
	future1 := time.Now().Add(2 * time.Hour).Unix()
	future2 := time.Now().Add(4 * time.Hour).Unix()
	for _, w := range []struct{ reason, t string }{
		{"future wake 1", fmt.Sprintf("%d", future1)},
		{"future wake 2", fmt.Sprintf("%d", future2)},
	} {
		if _, err := handleScheduleWake(dm, internal.ActiveContext{}, map[string]interface{}{
			"reason":      w.reason,
			"target_time": w.t,
		}); err != nil {
			t.Fatalf("schedule %s: %v", w.reason, err)
		}
	}

	// Default: pending only (both unfired)
	res, err := handleListWakes(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	m := res.(map[string]interface{})
	if c, _ := m["count"].(int); c != 2 {
		t.Errorf("default list count: got %d, want 2", c)
	}

	// Backdate wake 1 to past via SQL
	_, err = dm.SQLDB().Exec(`UPDATE scheduled_wakes SET target_time = ? WHERE reason = ?`,
		time.Now().Add(-30*time.Minute).Unix(), "future wake 1")
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// overdue_only: past wake only (1)
	res, err = handleListWakes(dm, internal.ActiveContext{}, map[string]interface{}{
		"overdue_only": true,
	})
	if err != nil {
		t.Fatalf("list overdue: %v", err)
	}
	m = res.(map[string]interface{})
	if c, _ := m["count"].(int); c != 1 {
		t.Errorf("overdue_only count: got %d, want 1", c)
	}

	// include_fired=true with overdue_only=true: past wake is overdue AND
	// unfired (we did not fire it; only backdated). So count should still
	// be 1.
	res, err = handleListWakes(dm, internal.ActiveContext{}, map[string]interface{}{
		"include_fired": true,
		"overdue_only":  true,
	})
	if err != nil {
		t.Fatalf("list include+overdue: %v", err)
	}
	m = res.(map[string]interface{})
	if c, _ := m["count"].(int); c != 1 {
		t.Errorf("include_fired+overdue_only count: got %d, want 1 (past wake still unfired)", c)
	}
}
