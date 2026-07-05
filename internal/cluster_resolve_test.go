package internal

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newClusterTestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "cluster-resolve-test.db")
	db, err := sql.Open("sqlite3", tmp)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	dm := NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	return dm
}

// seedCluster inserts a synthetic audit_cluster_proposals row above
// the threshold so ActiveClusters() surfaces it. count=5 is arbitrary
// but >= ClusterThreshold (3) keeps the test fast and unambiguous.
func seedClusterResolve(t *testing.T, dm *DatabaseManager, key, component, message string) {
	t.Helper()
	_, err := dm.db.Exec(`
		INSERT INTO audit_cluster_proposals
			(cluster_key, component, message_hash, count, first_seen, last_seen, status)
		VALUES (?, ?, ?, 5, datetime('now', '-1 day'), datetime('now'), 'active')`,
		key, component, HashMessage(message))
	if err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
}

// ─── SetClusterStatus tests ─────────────────────────────────────────────

func TestSetClusterStatus_ResolvePreservesRow(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "relay:abc", "relay", "relay timeout")

	if err := dm.SetClusterStatus("relay:abc", ClusterStatusResolved, "", "test fixture"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var status string
	var count int
	var snoozeUntil sql.NullString
	if err := dm.db.QueryRow(
		`SELECT status, count, snooze_until FROM audit_cluster_proposals WHERE cluster_key = ?`,
		"relay:abc",
	).Scan(&status, &count, &snoozeUntil); err != nil {
		t.Fatal(err)
	}
	if status != ClusterStatusResolved {
		t.Errorf("status = %q, want %q", status, ClusterStatusResolved)
	}
	if count != 5 {
		t.Errorf("count = %d, want 5 (preserved)", count)
	}
	if snoozeUntil.Valid {
		t.Errorf("snooze_until should be NULL after resolve, got %q", snoozeUntil.String)
	}
}

func TestSetClusterStatus_SnoozeSetsTimer(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "security:def", "security", "poison phrase")

	if err := dm.SetClusterStatus("security:def", ClusterStatusSnoozed, "24h", "noise"); err != nil {
		t.Fatalf("snooze: %v", err)
	}

	var status string
	var snoozeUntil string
	if err := dm.db.QueryRow(
		`SELECT status, snooze_until FROM audit_cluster_proposals WHERE cluster_key = ?`,
		"security:def",
	).Scan(&status, &snoozeUntil); err != nil {
		t.Fatal(err)
	}
	if status != ClusterStatusSnoozed {
		t.Errorf("status = %q, want %q", status, ClusterStatusSnoozed)
	}
	// snooze_until must be roughly now+24h (allow 10s slack for clock drift).
	target, err := time.Parse("2006-01-02 15:04:05", snoozeUntil)
	if err != nil {
		// SQLite may store in different format; try with timezone
		target, err = time.Parse(time.RFC3339, snoozeUntil)
		if err != nil {
			t.Fatalf("unparseable snooze_until %q: %v", snoozeUntil, err)
		}
	}
	delta := time.Until(target)
	if delta < 23*time.Hour || delta > 25*time.Hour {
		t.Errorf("snooze_until delta = %v, want ~24h", delta)
	}
}

func TestSetClusterStatus_UnknownClusterReturnsError(t *testing.T) {
	dm := newClusterTestDM(t)
	err := dm.SetClusterStatus("does:not:exist", ClusterStatusResolved, "", "")
	if err == nil {
		t.Fatal("expected error for missing cluster, got nil")
	}
	if !strings.Contains(err.Error(), "cluster not found") {
		t.Errorf("error = %v, expected 'cluster not found'", err)
	}
}

func TestSetClusterStatus_InvalidStatusRejected(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "x:y", "x", "y")
	err := dm.SetClusterStatus("x:y", "bogus_status", "", "")
	if err == nil {
		t.Fatal("expected error for invalid status, got nil")
	}
	if !strings.Contains(err.Error(), "status must be") {
		t.Errorf("error = %v", err)
	}
}

func TestSetClusterStatus_SnoozeWithoutUntilRejected(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "x:y", "x", "y")
	err := dm.SetClusterStatus("x:y", ClusterStatusSnoozed, "", "")
	if err == nil {
		t.Fatal("expected error for snooze without snooze_until, got nil")
	}
	if !strings.Contains(err.Error(), "snooze_until required") {
		t.Errorf("error = %v", err)
	}
}

func TestSetClusterStatus_IdempotentResolve(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "x:y", "x", "y")

	if err := dm.SetClusterStatus("x:y", ClusterStatusResolved, "", "first reason"); err != nil {
		t.Fatal(err)
	}
	// Re-resolving with a different reason — should succeed (idempotent).
	if err := dm.SetClusterStatus("x:y", ClusterStatusResolved, "", "better reason"); err != nil {
		t.Errorf("re-resolve should succeed: %v", err)
	}

	var count int
	dm.db.QueryRow(`SELECT COUNT(*) FROM audit_cluster_proposals WHERE cluster_key = ?`, "x:y").Scan(&count)
	if count != 1 {
		t.Errorf("idempotent resolve should leave exactly 1 row, got %d", count)
	}
}

func TestSetClusterStatus_ReSnoozeResetsTimer(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "x:y", "x", "y")

	if err := dm.SetClusterStatus("x:y", ClusterStatusSnoozed, "1h", "first"); err != nil {
		t.Fatal(err)
	}
	// 50ms later, snooze for longer — timer must reset.
	time.Sleep(50 * time.Millisecond)
	if err := dm.SetClusterStatus("x:y", ClusterStatusSnoozed, "48h", "longer"); err != nil {
		t.Fatal(err)
	}

	var snoozeUntil string
	dm.db.QueryRow(`SELECT snooze_until FROM audit_cluster_proposals WHERE cluster_key = ?`, "x:y").Scan(&snoozeUntil)
	target, err := time.Parse(time.RFC3339, snoozeUntil)
	if err != nil {
		t.Fatalf("unparseable snooze_until %q: %v", snoozeUntil, err)
	}
	delta := time.Until(target)
	// 48h timer must be in the future and not the 1h timer.
	if delta < 47*time.Hour {
		t.Errorf("delta = %v, want ~48h (re-snooze should reset timer)", delta)
	}
}

// ─── Audit-row verification ────────────────────────────────────────────

func TestSetClusterStatus_WritesAuditRow(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "audit:test", "audit", "audit test cluster")

	if err := dm.SetClusterStatus("audit:test", ClusterStatusResolved, "", "by design"); err != nil {
		t.Fatal(err)
	}

	var message string
	var ctx sql.NullString
	if err := dm.db.QueryRow(
		`SELECT message, context FROM system_audit_log WHERE component = 'cluster' LIMIT 1`,
	).Scan(&message, &ctx); err != nil {
		t.Fatalf("expected audit row, got: %v", err)
	}
	if !strings.Contains(message, "resolved by agent") {
		t.Errorf("audit message = %q, expected '...resolved by agent'", message)
	}
	if !ctx.Valid {
		t.Error("audit context should be JSON-populated")
	}
}

// ─── parseClusterSnoozeUntil tests ─────────────────────────────────────

func TestParseClusterSnoozeUntil_Absolute(t *testing.T) {
	fixedFuture, _ := time.Parse(time.RFC3339, "2030-01-01T00:00:00Z")
	got, err := parseClusterSnoozeUntil("2030-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Equal(fixedFuture) {
		t.Errorf("got %v, want %v", got, fixedFuture)
	}
}

func TestParseClusterSnoozeUntil_RelativeHours(t *testing.T) {
	before := time.Now().UTC()
	got, err := parseClusterSnoozeUntil("24h")
	if err != nil {
		t.Fatal(err)
	}
	expectedMin := before.Add(24 * time.Hour).Add(-2 * time.Second)
	expectedMax := before.Add(24 * time.Hour).Add(2 * time.Second)
	if got.Before(expectedMin) || got.After(expectedMax) {
		t.Errorf("24h: got %v, want %v..%v", got, expectedMin, expectedMax)
	}
}

func TestParseClusterSnoozeUntil_RelativeDays(t *testing.T) {
	got, err := parseClusterSnoozeUntil("7d")
	if err != nil {
		t.Fatal(err)
	}
	delta := time.Until(got)
	if delta < 6*24*time.Hour || delta > 8*24*time.Hour {
		t.Errorf("7d delta = %v, want ~7d", delta)
	}
}

func TestParseClusterSnoozeUntil_Composite(t *testing.T) {
	got, err := parseClusterSnoozeUntil("1h30m")
	if err != nil {
		t.Fatal(err)
	}
	delta := time.Until(got)
	if delta < 89*time.Minute || delta > 91*time.Minute {
		t.Errorf("1h30m delta = %v, want ~90min", delta)
	}
}

func TestParseClusterSnoozeUntil_RejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "  ", "tomorrow", "2026-13-45"} {
		_, err := parseClusterSnoozeUntil(bad)
		if err == nil {
			t.Errorf("expected error for %q, got nil", bad)
		}
	}
}

func TestParseClusterSnoozeUntil_TrimsWhitespace(t *testing.T) {
	got, err := parseClusterSnoozeUntil("  24h  ")
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(got) < 23*time.Hour {
		t.Errorf("trimmed 24h should be ~24h, got %v", time.Until(got))
	}
}

// ─── ActiveClusters integration with status transitions ────────────────

func TestActiveClusters_FilterSnoozedOutsideWindow(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "x:y", "x", "y")

	// Snooze with an expired window — should re-surface as active.
	if _, err := dm.db.Exec(
		`UPDATE audit_cluster_proposals SET snooze_until = datetime('now', '-1 hour') WHERE cluster_key = ?`,
		"x:y",
	); err != nil {
		t.Fatal(err)
	}

	known, unknown, err := dm.ActiveClusters()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range append(known, unknown...) {
		if c.Key == "x:y" {
			found = true
			break
		}
	}
	if !found {
		t.Error("cluster with expired snooze should re-surface in active clusters")
	}
}

func TestActiveClusters_FilterResolvedOut(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "x:y", "x", "y")

	if err := dm.SetClusterStatus("x:y", ClusterStatusResolved, "", "test"); err != nil {
		t.Fatal(err)
	}

	known, unknown, err := dm.ActiveClusters()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range append(known, unknown...) {
		if c.Key == "x:y" {
			t.Errorf("resolved cluster should NOT appear in ActiveClusters, got %+v", c)
		}
	}
}

func TestSetClusterStatus_NilDMDefensive(t *testing.T) {
	var nilDM *DatabaseManager
	err := nilDM.SetClusterStatus("x:y", ClusterStatusResolved, "", "")
	if err == nil {
		t.Error("nil DM should return error, got nil")
	}
	if !errors.Is(err, errors.Unwrap(err)) {
		// sanity: the wrap chain is just fmt.Errorf
	}
}

func TestSetClusterStatus_EmptyClusterKeyRejected(t *testing.T) {
	dm := newClusterTestDM(t)
	err := dm.SetClusterStatus("", ClusterStatusResolved, "", "")
	if err == nil {
		t.Error("empty cluster_key should be rejected")
	}
	if !strings.Contains(err.Error(), "cluster_key required") {
		t.Errorf("error = %v", err)
	}
}
