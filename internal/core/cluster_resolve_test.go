package internal

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func newClusterTestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	return NewTestDM(t)
}

// seedCluster inserts a synthetic audit_cluster_proposals row above
// the threshold so ActiveClusters() surfaces it. count=5 is arbitrary
// but >= ClusterThreshold (3) keeps the test fast and unambiguous.
func seedClusterResolve(t *testing.T, dm *DatabaseManager, key, component, message string) {
	t.Helper()
	_, err := dm.db.Exec(`
		INSERT INTO audit_cluster_proposals
			(cluster_key, component, message_hash, count, first_seen, last_seen, status)
		VALUES (?, ?, ?, 5, CAST(strftime('%s','now', '-1 day') AS INTEGER), CAST(strftime('%s','now') AS INTEGER), 'active')`,
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
		`UPDATE audit_cluster_proposals SET snooze_until = CAST(strftime('%s','now', '-1 hour') AS INTEGER) WHERE cluster_key = ?`,
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

// ─── AnnotateCluster tests ─────────────────────────────────────────────
//
// Annotations append forensic context to the audit log without
// mutating the cluster row. The state-changing verbs (resolve/snooze)
// and the append-only verb (annotate) are deliberately separate tools
// so the audit trail can't be back-doored.

func TestAnnotateCluster_WritesAuditRow(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "annotate:test", "annotate", "annotation cluster")

	if err := dm.AnnotateCluster("annotate:test", "root cause was DNS upstream", "post-mortem"); err != nil {
		t.Fatalf("annotate: %v", err)
	}

	var message string
	var ctx sql.NullString
	if err := dm.db.QueryRow(
		`SELECT message, context FROM system_audit_log WHERE component = 'cluster' LIMIT 1`,
	).Scan(&message, &ctx); err != nil {
		t.Fatalf("expected audit row, got: %v", err)
	}
	if !strings.Contains(message, "cluster annotated by agent:") {
		t.Errorf("audit message = %q, expected 'cluster annotated by agent:' prefix", message)
	}
	if !strings.Contains(message, "root cause was DNS upstream") {
		t.Errorf("annotation text not in message preview: %q", message)
	}
	if !ctx.Valid {
		t.Error("audit context should be JSON-populated")
	}
}

func TestAnnotateCluster_DoesNotMutateClusterRow(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "annotate:no-mutate", "annotate", "annotation cluster")

	// Capture the row before annotation.
	var statusBefore, snoozeBefore string
	var countBefore int
	dm.db.QueryRow(
		`SELECT status, IFNULL(snooze_until, ''), count FROM audit_cluster_proposals WHERE cluster_key = ?`,
		"annotate:no-mutate",
	).Scan(&statusBefore, &snoozeBefore, &countBefore)

	if err := dm.AnnotateCluster("annotate:no-mutate", "annotation text", "label"); err != nil {
		t.Fatal(err)
	}

	// Row must be byte-identical — annotations never touch state.
	var statusAfter, snoozeAfter string
	var countAfter int
	dm.db.QueryRow(
		`SELECT status, IFNULL(snooze_until, ''), count FROM audit_cluster_proposals WHERE cluster_key = ?`,
		"annotate:no-mutate",
	).Scan(&statusAfter, &snoozeAfter, &countAfter)

	if statusAfter != statusBefore {
		t.Errorf("status changed: %q → %q (annotation must not mutate)", statusBefore, statusAfter)
	}
	if snoozeAfter != snoozeBefore {
		t.Errorf("snooze_until changed: %q → %q", snoozeBefore, snoozeAfter)
	}
	if countAfter != countBefore {
		t.Errorf("count changed: %d → %d", countBefore, countAfter)
	}
}

func TestAnnotateCluster_OnResolvedCluster(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "annotate:resolved", "annotate", "annotation cluster")

	if err := dm.SetClusterStatus("annotate:resolved", ClusterStatusResolved, "", "initial resolution"); err != nil {
		t.Fatal(err)
	}

	// A week later: post-mortem on a resolved cluster.
	if err := dm.AnnotateCluster("annotate:resolved", "found the real cause: clock skew on relay", "post-mortem"); err != nil {
		t.Fatalf("annotate resolved cluster should work: %v", err)
	}

	// Cluster is STILL resolved (no reactivation back-door).
	var status string
	dm.db.QueryRow(`SELECT status FROM audit_cluster_proposals WHERE cluster_key = ?`, "annotate:resolved").Scan(&status)
	if status != ClusterStatusResolved {
		t.Errorf("status = %q, want resolved (annotation must not back-door reactivation)", status)
	}

	// The annotation IS in the audit log.
	var n int
	dm.db.QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE component = 'cluster' AND message LIKE 'cluster annotated by agent%'`,
	).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 annotation audit row, got %d", n)
	}
}

func TestAnnotateCluster_OnSnoozedCluster(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "annotate:snoozed", "annotate", "annotation cluster")

	if err := dm.SetClusterStatus("annotate:snoozed", ClusterStatusSnoozed, "24h", "noise today"); err != nil {
		t.Fatal(err)
	}

	if err := dm.AnnotateCluster("annotate:snoozed", "this is the daily cron that always fires", "refinement"); err != nil {
		t.Fatal(err)
	}

	var status string
	dm.db.QueryRow(`SELECT status FROM audit_cluster_proposals WHERE cluster_key = ?`, "annotate:snoozed").Scan(&status)
	if status != ClusterStatusSnoozed {
		t.Errorf("status = %q, want snoozed", status)
	}
}

func TestAnnotateCluster_MultipleAnnotations(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "annotate:multi", "annotate", "annotation cluster")

	// Three sequential annotations — each must get its own audit row.
	for _, note := range []string{
		"first observation",
		"second observation — refined cause",
		"third observation — confirmed",
	} {
		if err := dm.AnnotateCluster("annotate:multi", note, "iteration"); err != nil {
			t.Fatal(err)
		}
	}

	var n int
	dm.db.QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE component = 'cluster' AND message LIKE 'cluster annotated by agent%'`,
	).Scan(&n)
	if n != 3 {
		t.Errorf("expected 3 annotation audit rows, got %d", n)
	}
}

func TestAnnotateCluster_MessagePreviewTruncated(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "annotate:trunc", "annotate", "annotation cluster")

	longAnnotation := strings.Repeat("x", 500)
	if err := dm.AnnotateCluster("annotate:trunc", longAnnotation, ""); err != nil {
		t.Fatal(err)
	}

	var message string
	dm.db.QueryRow(
		`SELECT message FROM system_audit_log WHERE component = 'cluster' AND message LIKE 'cluster annotated by agent%'`,
	).Scan(&message)
	if !strings.Contains(message, "...") {
		t.Errorf("long annotation should be truncated with ellipsis, got: %q", message)
	}
	if strings.Contains(message, longAnnotation) {
		t.Errorf("full 500-char annotation should NOT be in the message preview")
	}
}

func TestAnnotateCluster_RejectsEmptyClusterKey(t *testing.T) {
	dm := newClusterTestDM(t)
	err := dm.AnnotateCluster("", "some annotation", "")
	if err == nil {
		t.Error("empty cluster_key should be rejected")
	}
	if !strings.Contains(err.Error(), "cluster_key required") {
		t.Errorf("error = %v", err)
	}
}

func TestAnnotateCluster_RejectsEmptyAnnotation(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "annotate:empty", "annotate", "annotation cluster")

	err := dm.AnnotateCluster("annotate:empty", "", "no-annotation")
	if err == nil {
		t.Error("empty annotation should be rejected")
	}
	if !strings.Contains(err.Error(), "annotation required") {
		t.Errorf("error = %v", err)
	}
}

func TestAnnotateCluster_RejectsUnknownCluster(t *testing.T) {
	dm := newClusterTestDM(t)
	err := dm.AnnotateCluster("does:not:exist", "some annotation", "")
	if err == nil {
		t.Error("unknown cluster should be rejected")
	}
	if !strings.Contains(err.Error(), "cluster not found") {
		t.Errorf("error = %v", err)
	}
}

func TestAnnotateCluster_NilDMDefensive(t *testing.T) {
	var nilDM *DatabaseManager
	err := nilDM.AnnotateCluster("x:y", "annotation", "")
	if err == nil {
		t.Error("nil DM should return error")
	}
}

func TestAnnotateCluster_PriorStatusInAuditContext(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "annotate:prior", "annotate", "annotation cluster")

	// Resolve first so prior_status='resolved'.
	if err := dm.SetClusterStatus("annotate:prior", ClusterStatusResolved, "", "init"); err != nil {
		t.Fatal(err)
	}

	if err := dm.AnnotateCluster("annotate:prior", "week-later insight", "post-mortem"); err != nil {
		t.Fatal(err)
	}

	var ctx sql.NullString
	dm.db.QueryRow(
		`SELECT context FROM system_audit_log WHERE component = 'cluster' AND message LIKE 'cluster annotated by agent%' ORDER BY id DESC LIMIT 1`,
	).Scan(&ctx)
	if !ctx.Valid {
		t.Fatal("expected JSON context")
	}
	// prior_status and prior_count must be in the audit context so
	// post-mortem readers can reconstruct what the cluster looked
	// like when annotated.
	if !strings.Contains(ctx.String, "resolved") {
		t.Errorf("audit context should include prior_status=resolved, got: %s", ctx.String)
	}
	if !strings.Contains(ctx.String, "prior_count") {
		t.Errorf("audit context should include prior_count, got: %s", ctx.String)
	}
	if !strings.Contains(ctx.String, "annotation") {
		t.Errorf("audit context should include the full annotation text, got: %s", ctx.String)
	}
}

func TestAnnotateCluster_ReasonOptional(t *testing.T) {
	dm := newClusterTestDM(t)
	seedClusterResolve(t, dm, "annotate:noreason", "annotate", "annotation cluster")

	// No reason field — must still succeed.
	if err := dm.AnnotateCluster("annotate:noreason", "no reason given", ""); err != nil {
		t.Errorf("annotation without reason should succeed: %v", err)
	}

	// Audit context includes reason (possibly empty).
	var ctx sql.NullString
	dm.db.QueryRow(
		`SELECT context FROM system_audit_log WHERE component = 'cluster' AND message LIKE 'cluster annotated by agent%' LIMIT 1`,
	).Scan(&ctx)
	if !ctx.Valid {
		t.Error("audit context should be JSON-populated even without reason")
	}
	if !strings.Contains(ctx.String, "reason") {
		t.Errorf("audit context should still include reason field (even if empty): %s", ctx.String)
	}
}
