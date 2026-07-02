package tools

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// ---------------------------------------------------------------------------
// list_active_clusters handler tests
// ---------------------------------------------------------------------------

// newTestIsolatedDM opens a fresh sqlite3 file in t.TempDir() and runs
// the canonical MPM schema via NewDatabaseManagerForDB + InitSchema.
// Unlike newTestSharedDM this does NOT attach the workspace MPM_SHARED_DB,
// so tests using it cannot pollute the workspace database or read
// state from prior tests. Use this for tests that need a clean slate.
func newTestIsolatedDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "cluster-test.db")
	db, err := sql.Open("sqlite3", tmp)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	return dm
}

// seedClusterRow inserts a row into audit_cluster_proposals directly.
// INSERT OR REPLACE (UPSERT) so tests that re-run or share state via
// the shared DB don't trip the PRIMARY KEY constraint.
func seedClusterRow(t *testing.T, dm *internal.DatabaseManager, clusterKey, component, messageHash, status string, count int, firstSeen, lastSeen string) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT OR REPLACE INTO audit_cluster_proposals
		    (cluster_key, component, message_hash, count, first_seen, last_seen, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		clusterKey, component, messageHash, count, firstSeen, lastSeen, status)
	if err != nil {
		t.Fatalf("seedClusterRow: %v", err)
	}
}

// seedTheoryForCluster inserts a theories row whose content references
// the given cluster_key. Used to test the "known" classification.
func seedTheoryForCluster(t *testing.T, dm *internal.DatabaseManager, id, content, status string) {
	t.Helper()
	meta := fmt.Sprintf(`{"status":"%s"}`, status)
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata)
		VALUES (?, 'theories', ?, '[]', ?)`,
		id, content, meta)
	if err != nil {
		t.Fatalf("seedTheoryForCluster: %v", err)
	}
}

func TestHandleListActiveClusters_EmptyReturnsEmptyArrays(t *testing.T) {
	dm := newTestIsolatedDM(t)

	result, err := handleListActiveClusters(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("handleListActiveClusters: %v", err)
	}
	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", result)
	}
	if m["success"] != true {
		t.Errorf("expected success=true, got %v", m["success"])
	}
	// Both buckets must be empty slices (not nil), so JSON encodes as [].
	known, ok := m["known_clusters"].([]internal.ClusterProposal)
	if !ok {
		t.Fatalf("expected known_clusters to be []ClusterProposal, got %T", m["known_clusters"])
	}
	if len(known) != 0 {
		t.Errorf("expected 0 known clusters, got %d", len(known))
	}
	unknown, ok := m["unknown_clusters"].([]internal.ClusterProposal)
	if !ok {
		t.Fatalf("expected unknown_clusters to be []ClusterProposal, got %T", m["unknown_clusters"])
	}
	if len(unknown) != 0 {
		t.Errorf("expected 0 unknown clusters, got %d", len(unknown))
	}
	counts := m["count"].(map[string]int)
	if counts["known"] != 0 || counts["unknown"] != 0 {
		t.Errorf("expected counts both zero, got %+v", counts)
	}
}

func TestHandleListActiveClusters_BucketsKnownVsUnknown(t *testing.T) {
	dm := newTestIsolatedDM(t)
	knownKey := "relay:abc123def456abc123def456abc12345"
	unknownKey := "storage:def456abc123def456abc123def456ab"
	now := "2026-07-02 14:00:00"

	seedClusterRow(t, dm, knownKey, "relay", "abc123def456abc123def456abc12345", "active", 5, now, now)
	seedClusterRow(t, dm, unknownKey, "storage", "def456abc123def456abc123def456ab", "active", 4, now, now)
	// Reference the known cluster_key in a pending theory's content.
	seedTheoryForCluster(t, dm, "th-1",
		fmt.Sprintf("HYPOTHESIS: relay cluster %s is a misconfigured retry loop", knownKey),
		"pending")

	result, err := handleListActiveClusters(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("handleListActiveClusters: %v", err)
	}
	m := result.(map[string]interface{})

	known := m["known_clusters"].([]internal.ClusterProposal)
	unknown := m["unknown_clusters"].([]internal.ClusterProposal)

	if len(known) != 1 {
		t.Fatalf("expected 1 known cluster, got %d", len(known))
	}
	if known[0].Key != knownKey {
		t.Errorf("known cluster key mismatch: got %q want %q", known[0].Key, knownKey)
	}
	if !known[0].Known {
		t.Error("known cluster should have Known=true")
	}

	if len(unknown) != 1 {
		t.Fatalf("expected 1 unknown cluster, got %d", len(unknown))
	}
	if unknown[0].Key != unknownKey {
		t.Errorf("unknown cluster key mismatch: got %q want %q", unknown[0].Key, unknownKey)
	}
	if unknown[0].Known {
		t.Error("unknown cluster should have Known=false")
	}

	counts := m["count"].(map[string]int)
	if counts["known"] != 1 || counts["unknown"] != 1 {
		t.Errorf("count mismatch: got %+v", counts)
	}
}

func TestHandleListActiveClusters_BelowThresholdExcluded(t *testing.T) {
	dm := newTestIsolatedDM(t)
	now := "2026-07-02 14:00:00"
	// count=2 is below ClusterThreshold=3.
	seedClusterRow(t, dm, "relay:abc123def456abc123def456abc12345", "relay",
		"abc123def456abc123def456abc12345", "active", 2, now, now)

	result, err := handleListActiveClusters(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("handleListActiveClusters: %v", err)
	}
	m := result.(map[string]interface{})
	unknown := m["unknown_clusters"].([]internal.ClusterProposal)
	if len(unknown) != 0 {
		t.Errorf("cluster below threshold should be excluded, got %d", len(unknown))
	}
}

func TestHandleListActiveClusters_FutureSnoozeExcluded(t *testing.T) {
	dm := newTestIsolatedDM(t)
	now := "2026-07-02 14:00:00"
	key := "relay:abc123def456abc123def456abc12345"
	seedClusterRow(t, dm, key, "relay", "abc123def456abc123def456abc12345",
		"snoozed", 5, now, now)
	_, err := dm.SQLDB().Exec(`UPDATE audit_cluster_proposals SET snooze_until = ? WHERE cluster_key = ?`,
		"2026-12-31 00:00:00", key)
	if err != nil {
		t.Fatal(err)
	}

	result, err := handleListActiveClusters(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("handleListActiveClusters: %v", err)
	}
	m := result.(map[string]interface{})
	unknown := m["unknown_clusters"].([]internal.ClusterProposal)
	if len(unknown) != 0 {
		t.Errorf("future-snoozed cluster should be excluded, got %d", len(unknown))
	}
}

// TestHandleListActiveClusters_RegistryEntryWired pins that the tool
// is registered. If a future refactor removes it from the Registry
// slice, this test fails before the agent loses access.
func TestHandleListActiveClusters_RegistryEntryWired(t *testing.T) {
	var found bool
	for _, tool := range Registry {
		if tool.Name == "list_active_clusters" {
			found = true
			if tool.Handler == nil {
				t.Error("list_active_clusters registry entry has nil Handler")
			}
			if tool.Description == "" {
				t.Error("list_active_clusters registry entry has empty Description")
			}
			if len(tool.Schema) == 0 {
				t.Error("list_active_clusters registry entry has empty Schema")
			}
			break
		}
	}
	if !found {
		t.Error("list_active_clusters not in Registry — agent will not see this tool")
	}
}
