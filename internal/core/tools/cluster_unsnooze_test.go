package tools

import (
	"database/sql"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// Part 2B (2026-09-06): public surface for cluster unsnooze.
//
// Proves:
//   1. active -> snooze -> snoozed -> unsnooze -> active round-trip
//   2. invalid cluster id errors at the boundary
//   3. already-active cluster is silent success
//   4. already-snoozed cluster reactivates (the primary use case)
//   5. resolved cluster is rejected (terminal state)
//   6. persisted status + snooze_until after operation
//   7. existing snooze_until=0s caller path still works (compatibility)

func TestHandleUnsnoozeCluster_RoundTrip(t *testing.T) {
	dm := newTestDMForTools(t)
	key := seedClusterProposal(t, dm, "router:abc", "active")

	assertClusterStatus(t, dm, key, "active")

	// Snooze.
	if _, err := handleSnoozeCluster(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"cluster_key":  key,
		"snooze_until": "1h",
	}); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	assertClusterStatus(t, dm, key, "snoozed")

	// Unsnooze.
	res, err := handleUnsnoozeCluster(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"cluster_key": key,
		"reason":      "false alarm after inspection",
	})
	if err != nil {
		t.Fatalf("unsnooze: %v", err)
	}
	m := res.(map[string]interface{})
	if m["success"] != true {
		t.Errorf("expected success=true, got %v", m["success"])
	}
	if m["status"] != "active" {
		t.Errorf("expected status=active, got %v", m["status"])
	}
	assertClusterStatus(t, dm, key, "active")

	// snooze_until must be cleared.
	if got := getClusterSnoozeUntil(t, dm, key); got != "" {
		t.Errorf("expected snooze_until NULL after unsnooze, got %q", got)
	}
}

func TestHandleUnsnoozeCluster_AlreadyActiveSilentSuccess(t *testing.T) {
	dm := newTestDMForTools(t)
	key := seedClusterProposal(t, dm, "router:def", "active")
	assertClusterStatus(t, dm, key, "active")

	res, err := handleUnsnoozeCluster(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"cluster_key": key,
	})
	if err != nil {
		t.Fatalf("unsnooze on active cluster: %v", err)
	}
	m := res.(map[string]interface{})
	if m["success"] != true {
		t.Errorf("expected success=true, got %v", m["success"])
	}
	if m["status"] != "active" {
		t.Errorf("expected status=active, got %v", m["status"])
	}
}

func TestHandleUnsnoozeCluster_UnknownClusterErrors(t *testing.T) {
	dm := newTestDMForTools(t)

	_, err := handleUnsnoozeCluster(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"cluster_key": "router:nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for unknown cluster_key")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

func TestHandleUnsnoozeCluster_ResolvedClusterRejected(t *testing.T) {
	dm := newTestDMForTools(t)
	key := seedClusterProposal(t, dm, "router:resolved-test", "active")

	if _, err := handleResolveCluster(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"cluster_key": key,
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	assertClusterStatus(t, dm, key, "resolved")

	_, err := handleUnsnoozeCluster(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"cluster_key": key,
	})
	if err == nil {
		t.Fatal("expected error when unsnoozing a resolved cluster")
	}
	if !strings.Contains(err.Error(), "resolved") {
		t.Errorf("error should mention 'resolved', got: %v", err)
	}
}

func TestHandleUnsnoozeCluster_RejectsMissingClusterKey(t *testing.T) {
	dm := newTestDMForTools(t)

	_, err := handleUnsnoozeCluster(dm, mpminternal.ActiveContext{}, map[string]interface{}{})
	if err == nil {
		t.Fatal("expected error for missing cluster_key")
	}
	if !strings.Contains(err.Error(), "cluster_key is required") {
		t.Errorf("error should mention cluster_key, got: %v", err)
	}
}

// TestHandleSnoozeCluster_ZeroDurationStillWorks pins the existing
// `snooze_until=0s` caller path. The Part 2B change must not regress
// this — it is the documented compatibility path until callers migrate
// to unsnooze_cluster.
func TestHandleSnoozeCluster_ZeroDurationStillWorks(t *testing.T) {
	dm := newTestDMForTools(t)
	key := seedClusterProposal(t, dm, "router:zero-dur", "active")

	if _, err := handleSnoozeCluster(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"cluster_key":  key,
		"snooze_until": "0s",
	}); err != nil {
		t.Fatalf("snooze_until=0s should still work: %v", err)
	}
	assertClusterStatus(t, dm, key, "snoozed")
	if got := getClusterSnoozeUntil(t, dm, key); got == "" {
		t.Errorf("expected snooze_until to be set (even if past), got empty")
	}
}

// helpers ────────────────────────────────────────────────────────────────

func seedClusterProposal(t *testing.T, dm *mpminternal.DatabaseManager, key string, status string) string {
	t.Helper()
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO audit_cluster_proposals (cluster_key, component, message_hash, status, count, first_seen, last_seen)
		 VALUES (?, 'router', 'hash', ?, 1, strftime('%s','now'), strftime('%s','now'))`,
		key, status,
	); err != nil {
		t.Fatalf("seed cluster proposal: %v", err)
	}
	return key
}

func assertClusterStatus(t *testing.T, dm *mpminternal.DatabaseManager, key string, want string) {
	t.Helper()
	var got string
	if err := dm.SQLDB().QueryRow(
		`SELECT status FROM audit_cluster_proposals WHERE cluster_key = ?`, key,
	).Scan(&got); err != nil {
		t.Fatalf("read status for %s: %v", key, err)
	}
	if got != want {
		t.Errorf("cluster %s status: want %q, got %q", key, want, got)
	}
}

func getClusterSnoozeUntil(t *testing.T, dm *mpminternal.DatabaseManager, key string) string {
	t.Helper()
	var v sql.NullString
	if err := dm.SQLDB().QueryRow(
		`SELECT snooze_until FROM audit_cluster_proposals WHERE cluster_key = ?`, key,
	).Scan(&v); err != nil {
		t.Fatalf("read snooze_until for %s: %v", key, err)
	}
	if !v.Valid {
		return ""
	}
	return v.String
}

// Ensure sql import is referenced (kept for future probe-style helpers).
var _ = sql.ErrNoRows
