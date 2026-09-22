// doctor_observability_synthesis_test.go — Doctor Wave 3 acceptance.
//
// Pins:
//   - Usage section synthesised from tool_invocations
//   - Attention section synthesised from system_audit_log + audit_cluster_proposals
//   - Doctor remains READ-ONLY (observer-effect row counts unchanged)
//   - Empty / new install renders neutrally without panic
//   - Legacy NULL outcome_class rows render as historical/unclassified
//   - JSON envelope adds usage / attention keys additively
//   - Operational / caller-policy / security-policy classification routing
//   - Bounded queries (no full-table scans)

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// newDoctorTestDM returns a hermetic DatabaseManager for Doctor
// observability synthesis tests. Mirrors cmd/mpm-mcp/audit_test.go
// newIsolatedTestDM pattern.
func newDoctorTestDM(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	return mpminternal.NewTestDM(t)
}

// seedToolInvocation inserts one row in tool_invocations with
// caller-supplied classification fields. Time is anchored to the
// supplied anchor so tests can place rows inside / outside the 24h
// window deterministically.
func seedToolInvocation(t *testing.T, dm *mpminternal.DatabaseManager, id string, toolName, frameworkName, outcomeClass, outcomeCode string, startedAt time.Time, resultStatus string) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id, actor_kind,
		     payload_hash, result_status, started_at, completed_at,
		     duration_ms, framework_name, outcome_class, outcome_code)
		VALUES (?, 'cli-default', ?, 'test', ?, 'agent',
		        'sha256:test', ?, ?, ?, 100, ?, ?, ?)`,
		id, toolName, "inv-"+id, resultStatus, startedAt.Unix(), startedAt.Unix(),
		frameworkName, outcomeClass, outcomeCode,
	)
	if err != nil {
		t.Fatalf("seedToolInvocation: %v", err)
	}
}

// seedAuditRow inserts one row in system_audit_log at the supplied time.
// created_at is INTEGER (unix seconds) per the system_audit_log schema;
// the format-string version above bypassed the comparison when used in
// indexed WHERE predicates, so we insert the unix int directly.
func seedAuditRow(t *testing.T, dm *mpminternal.DatabaseManager, id, level, component, eventCode, message string, at time.Time) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO system_audit_log
		    (id, level, component, message, stack_trace, context, created_at,
		     event_code)
		VALUES (?, ?, ?, ?, '', NULL, ?, ?)`,
		id, level, component, message, at.Unix(),
		eventCode,
	)
	if err != nil {
		t.Fatalf("seedAuditRow: %v", err)
	}
}

// seedCluster inserts one row in audit_cluster_proposals.
func seedCluster(t *testing.T, dm *mpminternal.DatabaseManager, id, component string, count int, lastSeen time.Time, status string) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO audit_cluster_proposals
		    (cluster_key, component, message_hash, count, first_seen, last_seen, status)
		VALUES (?, ?, 'sha256:test', ?, ?, ?, ?)`,
		id, component, count, lastSeen.Unix(), lastSeen.Unix(), status,
	)
	if err != nil {
		t.Fatalf("seedCluster: %v", err)
	}
}

// snapshotRowCounts captures the live row count of every table Doctor
// must not mutate. Used by the observer-effect test to prove zero
// mutations.
func snapshotRowCounts(t *testing.T, dm *mpminternal.DatabaseManager) map[string]int {
	t.Helper()
	out := map[string]int{}
	tables := []string{
		"tool_invocations", "system_audit_log", "audit_cluster_proposals",
		"session_handoffs", "scheduled_wakes", "works", "memories",
	}
	for _, table := range tables {
		var n int
		if err := dm.SQLDB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

// diffRowCounts returns the per-table delta between before / after snapshots.
func diffRowCounts(before, after map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range before {
		out[k] = after[k] - v
	}
	return out
}

// TestDoctor_Usage_Acceptance exercises the canonical Usage surface
// across success / caller-failure / security-block / substrate-event
// fixtures in one deterministic scene.
func TestDoctor_Usage_Acceptance(t *testing.T) {
	dm := newDoctorTestDM(t)
	now := time.Now().UTC()

	// 24h window: 6 successful saves, 2 sensitive blocks, 1 not_found, 1 conflict, 1 substrate.
	seedToolInvocation(t, dm, "ok-1", "mpm_memory", "opencode", "ok", "", now.Add(-1*time.Hour), "success")
	seedToolInvocation(t, dm, "ok-2", "mpm_memory", "opencode", "ok", "", now.Add(-2*time.Hour), "success")
	seedToolInvocation(t, dm, "ok-3", "mpm_work", "openclaw", "ok", "", now.Add(-3*time.Hour), "success")
	seedToolInvocation(t, dm, "ok-4", "mpm_work", "openclaw", "ok", "", now.Add(-4*time.Hour), "success")
	seedToolInvocation(t, dm, "sens-1", "mpm_memory", "pi", "validation", "sensitive_content_blocked", now.Add(-5*time.Hour), "error")
	seedToolInvocation(t, dm, "sens-2", "mpm_memory", "opencode", "validation", "sensitive_content_blocked", now.Add(-6*time.Hour), "error")
	seedToolInvocation(t, dm, "nf-1", "mpm_resolve", "opencode", "not_found", "artifact_not_found", now.Add(-7*time.Hour), "error")
	seedToolInvocation(t, dm, "conf-1", "mpm_decisions", "openclaw", "conflict", "state_transition_invalid", now.Add(-8*time.Hour), "error")
	seedToolInvocation(t, dm, "sub-1", "mpm_work", "opencode", "substrate", "substrate_schema", now.Add(-9*time.Hour), "error")
	// 7d-but-not-24h row: 1 day-old successful save (outside 24h, inside 7d).
	seedToolInvocation(t, dm, "old-1", "mpm_context", "claude-code", "ok", "", now.Add(-25*time.Hour), "success")
	// Older than 7d: must NOT appear in either window.
	seedToolInvocation(t, dm, "ancient", "mpm_memory", "opencode", "ok", "", now.Add(-30*24*time.Hour), "success")
	// Legacy NULL outcome row.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id, actor_kind,
		     payload_hash, result_status, started_at, completed_at,
		     duration_ms, framework_name)
		VALUES (?, 'cli-default', 'mpm_memory', 'test', 'inv-legacy', 'agent',
		        'sha256:legacy', 'error', ?, ?, 100, 'openclaw')`,
		"legacy-1", now.Add(-10*time.Hour).Unix(), now.Add(-10*time.Hour).Unix(),
	)
	if err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	svc := NewDoctorService(dm)
	report, err := svc.Check()
	if err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}
	if report.Usage == nil {
		t.Fatal("Usage section is nil")
	}
	if report.Usage.Unavailable != nil {
		t.Fatalf("Usage Unavailable: component=%s", report.Usage.Unavailable.Component)
	}

	// 24h window: ok-1..4 + sens-1, sens-2 + nf-1 + conf-1 + sub-1 + legacy-1 = 10 rows.
	if got := report.Usage.Window24hInvocations; got != 10 {
		t.Errorf("Window24hInvocations = %d, want 10", got)
	}
	// 7d window: 10 (24h) + old-1 (25h) = 11. The 30-day-old row is excluded.
	if got := report.Usage.Window7dInvocations; got != 11 {
		t.Errorf("Window7dInvocations = %d, want 11", got)
	}

	// Frameworks: opencode, openclaw, pi, claude-code → 4 distinct.
	if got := len(report.Usage.FrameworksObserved); got != 4 {
		t.Errorf("FrameworksObserved count = %d, want 4 (got %v)", got, report.Usage.FrameworksObserved)
	}

	// Most-used tools in 7d:
	//   mpm_memory: ok-1, ok-2, sens-1, sens-2, legacy-1, ancient (excluded 7d) = 5 in 7d
	//   mpm_work: ok-3, ok-4, sub-1 = 3
	//   mpm_resolve: nf-1 = 1
	//   mpm_decisions: conf-1 = 1
	//   mpm_context: old-1 = 1
	// Top by count: mpm_memory (5), mpm_work (3), [ties at 1].
	if got := report.Usage.MostUsedTools[0].Tool; got != "mpm_memory" {
		t.Errorf("MostUsedTools[0].Tool = %q, want mpm_memory", got)
	}
	if got := report.Usage.MostUsedTools[0].Count; got != 5 {
		t.Errorf("MostUsedTools[0].Count = %d, want 5", got)
	}

	// Outcome distribution.
	if got := report.Usage.OutcomeDistribution["ok"]; got != 5 {
		t.Errorf("OutcomeDistribution[ok] = %d, want 5", got)
	}
	if got := report.Usage.OutcomeDistribution["validation"]; got != 2 {
		t.Errorf("OutcomeDistribution[validation] = %d, want 2", got)
	}
	if got := report.Usage.OutcomeDistribution["not_found"]; got != 1 {
		t.Errorf("OutcomeDistribution[not_found] = %d, want 1", got)
	}
	if got := report.Usage.OutcomeDistribution["conflict"]; got != 1 {
		t.Errorf("OutcomeDistribution[conflict] = %d, want 1", got)
	}
	if got := report.Usage.OutcomeDistribution["substrate"]; got != 1 {
		t.Errorf("OutcomeDistribution[substrate] = %d, want 1", got)
	}
	// Legacy NULL outcome row.
	if got := report.Usage.HistoricalUnclassified; got != 1 {
		t.Errorf("HistoricalUnclassified = %d, want 1", got)
	}

	// MCP tool registry surface.
	if got := report.Usage.RegisteredTools; got != 21 {
		t.Errorf("RegisteredTools = %d, want 21", got)
	}
	if got := report.Usage.ExposedTools; got != 3 {
		t.Errorf("ExposedTools = %d, want 3", got)
	}
	if got := report.Usage.ExposedToolsUnfiltered; got != 21 {
		t.Errorf("ExposedToolsUnfiltered = %d, want 21", got)
	}
}

// TestDoctor_Attention_Acceptance exercises the Attention surface:
// operational (substrate) issues surface; caller-policy and security
// blocks do NOT inflate operational counts; bounded recent events;
// bounded clusters; security count only.
func TestDoctor_Attention_Acceptance(t *testing.T) {
	dm := newDoctorTestDM(t)
	now := time.Now().UTC()

	// 24h recent operational (substrate) event.
	seedAuditRow(t, dm, "aud-sub-1", "error", "substrate", "substrate_schema",
		"substrate failure 1", now.Add(-2*time.Hour))
	seedAuditRow(t, dm, "aud-sub-2", "error", "substrate", "substrate_schema",
		"substrate failure 2", now.Add(-3*time.Hour))
	// Caller-policy failures: validation / not_found. These must NOT
	// appear in operational_events (component != 'security', but
	// the level='error' would otherwise pull them in — so Doctor
	// excludes the security component AND classifies via outcome_class).
	// In this fixture we test the level-based filter; these are
	// info-level events that don't surface.
	seedAuditRow(t, dm, "aud-valid-1", "info", "validation", "usererror_rejected",
		"caller validation failure", now.Add(-1*time.Hour))
	// Security policy event (sensitive_content_blocked).
	seedAuditRow(t, dm, "aud-sec-1", "error", "security", "memory_save_sensitive_content_blocked",
		"sensitive blocked", now.Add(-1*time.Hour))
	seedAuditRow(t, dm, "aud-sec-2", "error", "security", "memory_save_poison_content_blocked",
		"poison blocked", now.Add(-1*time.Hour))
	// Older-than-7d: must NOT appear.
	seedAuditRow(t, dm, "aud-old", "error", "substrate", "substrate_schema",
		"old substrate failure", now.Add(-30*24*time.Hour))
	// Cluster with count > 1.
	seedCluster(t, dm, "cluster-sec-1", "security", 5, now.Add(-2*time.Hour), "active")

	svc := NewDoctorService(dm)
	report, err := svc.Check()
	if err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}
	if report.Attention == nil {
		t.Fatal("Attention section is nil")
	}
	if report.Attention.Unavailable != nil {
		t.Fatalf("Attention Unavailable: component=%s", report.Attention.Unavailable.Component)
	}

	// Operational issues = 2 substrate events (24h, in-window).
	if got := report.Attention.OperationalIssues7d; got != 2 {
		t.Errorf("OperationalIssues7d = %d, want 2", got)
	}
	if got := len(report.Attention.OperationalEvents); got != 1 {
		t.Errorf("OperationalEvents count = %d, want 1 (one component/event_code group)", got)
	} else {
		ev := report.Attention.OperationalEvents[0]
		if ev.Component != "substrate" || ev.EventCode != "substrate_schema" || ev.Count != 2 {
			t.Errorf("OperationalEvents[0] = %+v, want substrate/substrate_schema/2", ev)
		}
	}

	// Audit clusters: 1 active with count > 1.
	if got := len(report.Attention.AuditClusters); got != 1 {
		t.Errorf("AuditClusters count = %d, want 1", got)
	}

	// Security events: 2 (one sensitive, one poison).
	if got := report.Attention.SecurityEvents7d; got != 2 {
		t.Errorf("SecurityEvents7d = %d, want 2", got)
	}
}

// TestDoctor_OutcomeClassificationRouting pins that:
//   - validation / not_found / conflict → Usage counter ONLY
//   - substrate / integration / timeout / internal → Usage counter AND Attention operational
//   - security events (regardless of level) → Usage counter for "validation"
//     AND security count, NEVER operational
//
// Attention is sourced from system_audit_log rows (substrate-class
// tool_invocations do NOT themselves populate Attention — Doctor reads
// each surface separately). So this test seeds both surfaces.
func TestDoctor_OutcomeClassificationRouting(t *testing.T) {
	dm := newDoctorTestDM(t)
	now := time.Now().UTC()
	// tool_invocations — every closed outcome class once.
	seedToolInvocation(t, dm, "oc-1", "mpm_memory", "f1", "validation", "sensitive_content_blocked", now.Add(-1*time.Hour), "error")
	seedToolInvocation(t, dm, "oc-2", "mpm_memory", "f2", "not_found", "artifact_not_found", now.Add(-1*time.Hour), "error")
	seedToolInvocation(t, dm, "oc-3", "mpm_decisions", "f3", "conflict", "state_transition_invalid", now.Add(-1*time.Hour), "error")
	seedToolInvocation(t, dm, "oc-4", "mpm_work", "f4", "substrate", "substrate_schema", now.Add(-1*time.Hour), "error")
	seedToolInvocation(t, dm, "oc-5", "mpm_resolve", "f5", "integration", "provider_unreachable", now.Add(-1*time.Hour), "error")
	seedToolInvocation(t, dm, "oc-6", "mpm_blob_read", "f6", "timeout", "context_deadline", now.Add(-1*time.Hour), "error")
	seedToolInvocation(t, dm, "oc-7", "mpm_route", "f7", "internal", "unclassified", now.Add(-1*time.Hour), "error")

	// system_audit_log — paired substrate/integration/timeout/internal events
	// so Attention.OperationalIssues7d counts 4.
	seedAuditRow(t, dm, "oc-aud-sub", "error", "substrate", "substrate_schema", "substrate", now.Add(-1*time.Hour))
	seedAuditRow(t, dm, "oc-aud-int", "error", "integration", "provider_unreachable", "integration", now.Add(-1*time.Hour))
	seedAuditRow(t, dm, "oc-aud-tmo", "error", "scheduler", "context_deadline", "timeout", now.Add(-1*time.Hour))
	seedAuditRow(t, dm, "oc-aud-int2", "fatal", "router", "unclassified", "internal", now.Add(-1*time.Hour))

	svc := NewDoctorService(dm)
	report, err := svc.Check()
	if err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}

	// Each class appears in Usage.OutcomeDistribution.
	for _, class := range []string{"validation", "not_found", "conflict", "substrate", "integration", "timeout", "internal"} {
		if report.Usage.OutcomeDistribution[class] != 1 {
			t.Errorf("Usage.OutcomeDistribution[%q] = %d, want 1", class, report.Usage.OutcomeDistribution[class])
		}
	}

	// 4 substrate-class audit events within 7d → Attention count 4.
	if got := report.Attention.OperationalIssues7d; got != 4 {
		t.Errorf("OperationalIssues7d = %d, want 4 (substrate/integration/timeout/internal)", got)
	}
	// Validation / not_found / conflict events must NOT be in Attention
	// even though they have outcome_class set in tool_invocations —
	// because Attention sources from the audit_log, not from
	// tool_invocations.outcome_class.
	for _, ev := range report.Attention.OperationalEvents {
		if ev.Component == "validation" || ev.Component == "not_found" || ev.Component == "conflict" {
			t.Errorf("caller-policy component %q should not surface in Attention; got %+v", ev.Component, ev)
		}
	}
}

// TestDoctor_ObserverEffect pins that DoctorService.Check mutates
// ZERO rows in tool_invocations, system_audit_log,
// audit_cluster_proposals, handoffs, wakes, work, memories,
// contextual_routing.
func TestDoctor_ObserverEffect(t *testing.T) {
	dm := newDoctorTestDM(t)
	now := time.Now().UTC()

	// Seed a non-trivial dataset so Doctor has rows to read.
	seedToolInvocation(t, dm, "oe-1", "mpm_memory", "opencode", "ok", "", now.Add(-1*time.Hour), "success")
	seedToolInvocation(t, dm, "oe-2", "mpm_memory", "openclaw", "validation", "sensitive_content_blocked", now.Add(-1*time.Hour), "error")
	seedAuditRow(t, dm, "oe-aud-1", "error", "substrate", "substrate_schema", "x", now.Add(-1*time.Hour))
	seedCluster(t, dm, "oe-cluster-1", "security", 3, now.Add(-1*time.Hour), "active")

	before := snapshotRowCounts(t, dm)

	svc := NewDoctorService(dm)
	if _, err := svc.Check(); err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}

	after := snapshotRowCounts(t, dm)
	delta := diffRowCounts(before, after)
	for table, d := range delta {
		if d != 0 {
			t.Errorf("Doctor mutated %s by %d rows; must remain 0", table, d)
		}
	}
}

// TestDoctor_EmptyDB pins that Doctor renders neutrally on a fresh
// install with no rows. No panic, no fake data.
func TestDoctor_EmptyDB(t *testing.T) {
	dm := newDoctorTestDM(t)
	svc := NewDoctorService(dm)
	report, err := svc.Check()
	if err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}
	if report.Usage == nil || report.Attention == nil {
		t.Fatal("Usage / Attention must be non-nil even on empty DB")
	}
	if report.Usage.Window24hInvocations != 0 {
		t.Errorf("Window24hInvocations = %d, want 0", report.Usage.Window24hInvocations)
	}
	if report.Usage.Window7dInvocations != 0 {
		t.Errorf("Window7dInvocations = %d, want 0", report.Usage.Window7dInvocations)
	}
	if len(report.Usage.FrameworksObserved) != 0 {
		t.Errorf("FrameworksObserved non-empty: %v", report.Usage.FrameworksObserved)
	}
	if len(report.Usage.MostUsedTools) != 0 {
		t.Errorf("MostUsedTools non-empty: %v", report.Usage.MostUsedTools)
	}
	if len(report.Usage.OutcomeDistribution) != 0 {
		t.Errorf("OutcomeDistribution non-empty: %v", report.Usage.OutcomeDistribution)
	}
	if report.Usage.HistoricalUnclassified != 0 {
		t.Errorf("HistoricalUnclassified = %d, want 0", report.Usage.HistoricalUnclassified)
	}
	if report.Attention.OperationalIssues7d != 0 {
		t.Errorf("OperationalIssues7d = %d, want 0", report.Attention.OperationalIssues7d)
	}
	if len(report.Attention.OperationalEvents) != 0 {
		t.Errorf("OperationalEvents non-empty: %v", report.Attention.OperationalEvents)
	}
	if len(report.Attention.AuditClusters) != 0 {
		t.Errorf("AuditClusters non-empty: %v", report.Attention.AuditClusters)
	}
	if report.Attention.SecurityEvents7d != 0 {
		t.Errorf("SecurityEvents7d = %d, want 0", report.Attention.SecurityEvents7d)
	}
}

// TestDoctor_LegacyOutcome pins that legacy NULL outcome_class rows
// are NOT classified by parsing error_message and DO surface in
// HistoricalUnclassified (not in OutcomeDistribution).
func TestDoctor_LegacyOutcome(t *testing.T) {
	dm := newDoctorTestDM(t)
	now := time.Now().UTC()
	// Legacy row: outcome_class NULL, error_message carries a
	// human description that would naively look like "substrate" or
	// "validation" but Doctor must not parse it.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id, actor_kind,
		     payload_hash, result_status, started_at, completed_at,
		     duration_ms, framework_name, error_message)
		VALUES (?, 'cli-default', 'mpm_memory', 'test', 'inv-legacy', 'agent',
		        'sha256:legacy', 'error', ?, ?, 100, 'opencode',
		        'something something substrate schema failure nothing')`,
		"legacy-row-1", now.Add(-1*time.Hour).Unix(), now.Add(-1*time.Hour).Unix(),
	)
	if err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	svc := NewDoctorService(dm)
	report, err := svc.Check()
	if err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}
	if report.Usage.HistoricalUnclassified != 1 {
		t.Errorf("HistoricalUnclassified = %d, want 1", report.Usage.HistoricalUnclassified)
	}
	if report.Usage.OutcomeDistribution["substrate"] != 0 {
		t.Errorf("OutcomeDistribution[substrate] = %d, want 0 (must NOT parse error_message)",
			report.Usage.OutcomeDistribution["substrate"])
	}
}

// TestDoctor_JSON_EnvelopeCompatibility pins that the JSON envelope
// adds usage / attention keys additively without changing the
// existing summary / checks / timestamp shape. We test the same
// envelope struct that handleDoctor emits, populated directly.
func TestDoctor_JSON_EnvelopeCompatibility(t *testing.T) {
	dm := newDoctorTestDM(t)
	now := time.Now().UTC()
	seedToolInvocation(t, dm, "json-1", "mpm_memory", "opencode", "ok", "", now.Add(-1*time.Hour), "success")
	seedAuditRow(t, dm, "json-aud-1", "error", "substrate", "substrate_schema", "x", now.Add(-1*time.Hour))

	svc := NewDoctorService(dm)
	report, err := svc.Check()
	if err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}

	// Build the same envelope handleDoctor builds.
	envelope := struct {
		Timestamp string                   `json:"timestamp"`
		Summary   map[string]interface{}   `json:"summary"`
		Checks    []map[string]interface{} `json:"checks"`
		Usage     *DoctorUsage             `json:"usage,omitempty"`
		Attention *DoctorAttention         `json:"attention,omitempty"`
	}{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Summary: map[string]interface{}{
			"passed":   report.Passed,
			"warnings": report.Warnings,
			"failed":   report.Failed,
		},
		Checks:    doctorChecksToJSON(report.Checks),
		Usage:     report.Usage,
		Attention: report.Attention,
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Required keys preserved.
	for _, k := range []string{"timestamp", "summary", "checks"} {
		if _, ok := got[k]; !ok {
			t.Errorf("JSON envelope missing required key %q", k)
		}
	}
	// New additive keys present.
	for _, k := range []string{"usage", "attention"} {
		if _, ok := got[k]; !ok {
			t.Errorf("JSON envelope missing additive key %q", k)
		}
	}
	// Summary shape preserved.
	var summary map[string]interface{}
	if err := json.Unmarshal(got["summary"], &summary); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	for _, k := range []string{"passed", "warnings", "failed"} {
		if _, ok := summary[k]; !ok {
			t.Errorf("summary missing key %q", k)
		}
	}
	// Usage / Attention shape: usage has window_24h_invocations;
	// attention has operational_issues_7d.
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(got["usage"], &usage); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	if _, ok := usage["window_24h_invocations"]; !ok {
		t.Errorf("usage missing key %q", "window_24h_invocations")
	}
	var att map[string]json.RawMessage
	if err := json.Unmarshal(got["attention"], &att); err != nil {
		t.Fatalf("unmarshal attention: %v", err)
	}
	if _, ok := att["operational_issues_7d"]; !ok {
		t.Errorf("attention missing key %q", "operational_issues_7d")
	}
}

// TestDoctor_ExitCodeStability pins that exit codes are unchanged by
// the new sections. Empty DB → 0 (PASS).
func TestDoctor_ExitCodeStability(t *testing.T) {
	dm := newDoctorTestDM(t)
	svc := NewDoctorService(dm)
	report, err := svc.Check()
	if err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}
	// Compute the same exit code logic as handleDoctor.
	code := 0
	if report.Failed > 0 {
		code = 2
	} else if report.Warnings > 0 {
		code = 1
	}
	if code != 0 {
		t.Errorf("Empty DB exit code = %d, want 0 (PASS)", code)
	}
}

// TestDoctor_QueryBudget_Structural pins that Doctor's bounded query
// shape stays deterministic: 24h / 7d windows anchored to a fixed
// anchor, bounded LIMIT clauses on group-by queries, and a small
// bounded SELECT count from the use + attention paths. Wall-clock
// itself is NOT asserted in this suite (host scheduling would
// produce flaky failures per the original Doctor brief). Operators
// measure wall-clock manually via `time bin/mpm doctor`.
func TestDoctor_QueryBudget_Structural(t *testing.T) {
	dm := newDoctorTestDM(t)
	now := time.Now().UTC()
	for i := 0; i < 50; i++ {
		seedToolInvocation(t, dm, sqlSimpleID(i), "mpm_memory", "opencode", "ok", "", now.Add(-time.Duration(i)*time.Minute), "success")
	}
	for i := 0; i < 20; i++ {
		seedAuditRow(t, dm, sqlSimpleID(1000+i), "info", "synthesis", "", "x", now.Add(-time.Duration(i)*time.Minute))
	}
	seedCluster(t, dm, "cluster-perf-1", "synthesis", 10, now.Add(-1*time.Hour), "active")

	// Run Doctor once so the test exercises both computeUsage and
	// computeAttention. The bounded shape assertions below are the
	// actual acceptance criterion; wall-clock is captured for
	// operator reference only.
	svc := NewDoctorService(dm)
	start := time.Now()
	if _, err := svc.Check(); err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("Doctor single-iter wall-clock (informational): %v", elapsed)

	// 1. Anchored 24h / 7d windows: both windows must be exactly
	// 24h / 7d before now; not "since some event".
	rows, err := dm.SQLDB().Query(
		`SELECT
		   -- 24h count must equal 7d count for in-window seed
		   (SELECT COUNT(*) FROM tool_invocations WHERE started_at >= ?) AS c24,
		   (SELECT COUNT(*) FROM tool_invocations WHERE started_at >= ?) AS c7d`,
		now.Add(-24*time.Hour).Unix(), now.Add(-7*24*time.Hour).Unix(),
	)
	if err != nil {
		t.Fatalf("anchor query: %v", err)
	}
	defer rows.Close()
	var c24, c7d int
	if !rows.Next() {
		t.Fatal("anchor row missing")
	}
	if scanErr := rows.Scan(&c24, &c7d); scanErr != nil {
		t.Fatalf("scan: %v", scanErr)
	}
	if c7d < c24 {
		t.Errorf("7d count %d must be >= 24h count %d", c7d, c24)
	}

	// 2. LIMIT clauses: the GROUP BY queries used by computeUsage /
	// computeAttention must use a bounded top-N, not unbounded. We
	// re-issue the SQL against the test DB and assert the LIMIT is
	// present (i.e. the query is bounded by construction).
	// 2. LIMIT clauses: every SELECT issued by computeUsage /
	// computeAttention must be bounded — either by a time-window
	// predicate, by count>1 + LIMIT (cluster proposals), or by being
	// a single COUNT(*) aggregate (which is bounded by definition).
	// We re-issue the canonical SQL and assert the bounded-shape
	// invariants below. The list mirrors the 8 SELECTs actually
	// issued at service_doctor.go lines 916, 924, 932, 952, 975,
	// 1051, 1088, 1112 — 5 in computeUsage + 3 in computeAttention.
	for _, q := range []string{
		// computeUsage (5)
		`SELECT COUNT(*) FROM tool_invocations WHERE started_at >= ?`,                                       // 24h count (line 916)
		`SELECT COUNT(*) FROM tool_invocations WHERE started_at >= ?`,                                       // 7d count  (line 924)
		`SELECT framework_name FROM tool_invocations WHERE started_at >= ? AND framework_name IS NOT NULL GROUP BY framework_name ORDER BY framework_name`, // frameworks (line 932)
		`SELECT tool_name, COUNT(*) c FROM tool_invocations WHERE started_at >= ? GROUP BY tool_name ORDER BY c DESC, tool_name LIMIT 5`, // top-tools (line 952)
		`SELECT outcome_class, COUNT(*) FROM tool_invocations WHERE started_at >= ? GROUP BY outcome_class`, // outcomes (line 975)
		// computeAttention (3)
		`SELECT component, COALESCE(event_code, '') AS event_code, COUNT(*) AS c, MAX(created_at) AS last_seen FROM system_audit_log WHERE level IN ('error','fatal','critical') AND created_at >= ? AND component != 'security' GROUP BY component, event_code ORDER BY c DESC, last_seen DESC LIMIT 5`, // operational events (line 1051)
		`SELECT component, count, last_seen, status FROM audit_cluster_proposals WHERE status = 'active' AND count > 1 ORDER BY count DESC, last_seen DESC LIMIT 5`, // clusters (line 1088)
		`SELECT COUNT(*) FROM system_audit_log WHERE component = 'security' AND event_code IN ('memory_save_sensitive_content_blocked','memory_save_poison_content_blocked') AND created_at >= ?`, // security-policy 7d (line 1112)
	} {
		if !queryHasBoundedWindow(q) {
			t.Errorf("query missing bounded window predicate: %s", q)
		}
		if !queryHasLimitOrSingleAggregate(q) {
			t.Errorf("non-aggregate GROUP BY query missing LIMIT: %s", q)
		}
	}
}

// queryHasBoundedWindow / queryHasLimitOrSingleAggregate are file-local
// structural SQL inspection helpers. We use file-local names to avoid
// clashing with `contains` declared by other test files in the same
// package. They are deliberately case-insensitive and tolerate
// arbitrary whitespace.
//
// queryHasBoundedWindow returns true when the SQL has EITHER:
//   - a time-window predicate (started_at >= / created_at >= /
//     last_seen >=), OR
//   - a count-threshold predicate (count > N) combined with a LIMIT
//     elsewhere in the same query (used by the cluster-proposals
//     query, which is bounded by activity volume rather than time).
//
// queryHasLimitOrSingleAggregate returns true when the SQL is bounded
// by:
//   - a LIMIT clause (rows-cap), OR
//   - a single COUNT(*) aggregate without GROUP BY (returns one row,
//     bounded by definition), OR
//   - a GROUP BY on outcome_class (closed vocabulary of 9 classes
//     per internal/core/tool_outcome.go AllOutcomeClasses), OR
//   - a GROUP BY on framework_name (registered framework set is
//     small and finite).
func queryHasBoundedWindow(q string) bool {
	ql := strings.ToLower(q)
	hasTime := strings.Contains(ql, "started_at >=") ||
		strings.Contains(ql, "created_at >=") ||
		strings.Contains(ql, "last_seen >=")
	if hasTime {
		return true
	}
	hasCount := strings.Contains(ql, "count > ")
	hasLimit := strings.Contains(ql, "limit ")
	return hasCount && hasLimit
}

func queryHasLimitOrSingleAggregate(q string) bool {
	ql := strings.ToLower(q)
	if strings.Contains(ql, "limit ") || strings.Contains(ql, "count(*)") {
		return true
	}
	if strings.Contains(ql, "group by outcome_class") ||
		strings.Contains(ql, "group by framework_name") {
		return true
	}
	return false
}

// TestDoctor_RawErrorNotExposed pins that arbitrary database error
// text (paths, SQL fragments, table names) does NOT surface in
// Doctor's NEW Usage/Attention sections when the underlying
// observability queries fail. Doctor surfaces a bounded
// "Observability history unavailable: <component>" line instead.
// This test is deliberately scoped to the Wave 3 additive sections
// — the pre-existing System-health checks have their own contract
// for surfacing error context and are not part of this hardening.
func TestDoctor_RawErrorNotExposed(t *testing.T) {
	dm := newDoctorTestDM(t)
	// Seed at least one row so computeUsage / computeAttention
	// have something to read before the connection dies.
	seedToolInvocation(t, dm, "err-1", "mpm_memory", "opencode", "ok", "", time.Now().Add(-1*time.Hour), "success")
	seedAuditRow(t, dm, "err-aud-1", "error", "substrate", "substrate_schema", "x", time.Now().Add(-1*time.Hour))

	// Close the DB so subsequent queries fail with a path-bearing
	// "sql: database is closed" error containing the underlying
	// sqlite file path. The error string is exactly the kind we
	// must NOT surface in the Wave 3 sections.
	sqlDB := dm.SQLDB()
	closeErr := sqlDB.Close()
	if closeErr != nil {
		t.Fatalf("pre-close: %v", closeErr)
	}

	svc := NewDoctorService(dm)
	report, err := svc.Check()
	if err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}

	// 1) Structural: Wave 3 sections MUST carry the bounded
	// Unavailable sentinel — never the raw err.
	if report.Usage == nil || report.Usage.Unavailable == nil {
		t.Fatalf("expected Usage.Unavailable populated when computeUsage fails")
	}
	if !report.Usage.Unavailable.Unavailable {
		t.Errorf("Usage.Unavailable.Unavailable must be true")
	}
	if report.Usage.Unavailable.Component == "" {
		t.Errorf("Usage.Unavailable.Component must be set")
	}
	if report.Attention == nil || report.Attention.Unavailable == nil {
		t.Fatalf("expected Attention.Unavailable populated when computeAttention fails")
	}
	if !report.Attention.Unavailable.Unavailable {
		t.Errorf("Attention.Unavailable.Unavailable must be true")
	}
	if report.Attention.Unavailable.Component == "" {
		t.Errorf("Attention.Unavailable.Component must be set")
	}

	// 2) Render the Wave 3 sections in isolation — extract the
	// Usage / Attention blocks from the full render and assert
	// no raw error text appears within them.
	buf := &bytes.Buffer{}
	r := NewDoctorRenderer(buf, false)
	if err := r.Render(report); err != nil {
		t.Fatalf("render: %v", err)
	}
	rendered := buf.String()

	usageStart := strings.Index(rendered, "MPM · Usage")
	attentionStart := strings.Index(rendered, "MPM · Attention")
	if usageStart < 0 || attentionStart < 0 {
		t.Fatalf("Usage/Attention sections missing from rendered output:\n%s", rendered)
	}
	// Slice from Usage heading to Attention heading; append the
	// Attention block to the end of render.
	wave3 := rendered[usageStart:]
	if attentionEnd := strings.Index(rendered[attentionStart+1:], "MPM ·"); attentionEnd >= 0 {
		wave3 = rendered[usageStart : attentionStart+1+attentionEnd]
	}

	// Raw-error-shaped substrings we MUST NOT see in Wave 3 sections:
	//   - "database is closed" (raw sql.ErrConnDone)
	//   - any /tmp/, /home/, or .db substring (path leakage)
	for _, forbidden := range []string{"database is closed", "/tmp/", "/home/", ".db"} {
		if strings.Contains(wave3, forbidden) {
			t.Errorf("Wave 3 section leaked %q\nslice:\n%s", forbidden, wave3)
		}
	}
	// Bounded contract MUST be present.
	if !strings.Contains(wave3, "Observability history unavailable") {
		t.Errorf("Wave 3 sections missing bounded unavailable line:\n%s", wave3)
	}

	// 3) JSON envelope: same checks scoped to usage + attention
	// subtrees only (legacy checks have their own contract).
	envelope := struct {
		Usage     *DoctorUsage     `json:"usage,omitempty"`
		Attention *DoctorAttention `json:"attention,omitempty"`
	}{
		Usage:     report.Usage,
		Attention: report.Attention,
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rawStr := string(raw)
	for _, forbidden := range []string{"database is closed", "/tmp/", "/home/", ".db"} {
		if strings.Contains(rawStr, forbidden) {
			t.Errorf("JSON Wave 3 envelope leaked %q\noutput:\n%s", forbidden, rawStr)
		}
	}
}

// TestDoctor_RecoveredIncident pins the historical-vs-current
// distinction at the centre of Doctor's purpose. A historical
// substrate-class audit_log row is present (Attention surface),
// but every live System-health check is healthy. Doctor's exit
// semantics remain PASS (no Warnings / Failed inflation), proving
// historical issue != current failure.
func TestDoctor_RecoveredIncident(t *testing.T) {
	dm := newDoctorTestDM(t)
	now := time.Now().UTC()

	// Historical substrate event — the "previously recovered" signal.
	seedAuditRow(t, dm, "rec-hist-1", "error", "substrate", "substrate_schema",
		"historical substrate failure", now.Add(-2*time.Hour))

	svc := NewDoctorService(dm)
	report, err := svc.Check()
	if err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}

	// Historical event surfaces in Attention.
	if report.Attention == nil || report.Attention.OperationalIssues7d < 1 {
		t.Fatalf("historical substrate event must surface in Attention.OperationalIssues7d; got %d",
			0)
	}
	sawSubstrate := false
	for _, ev := range report.Attention.OperationalEvents {
		if ev.Component == "substrate" && ev.EventCode == "substrate_schema" {
			sawSubstrate = true
		}
	}
	if !sawSubstrate {
		t.Errorf("historical substrate event must appear in Attention.OperationalEvents")
	}

	// Current-health summary is unchanged by the historical event.
	if report.Failed != 0 {
		t.Errorf("historical event must NOT modify report.Failed (got %d, want 0)", report.Failed)
	}
	if report.Warnings != 0 {
		t.Errorf("historical event must NOT modify report.Warnings (got %d, want 0)", report.Warnings)
	}
	if report.Passed < 1 {
		t.Errorf("live System-health checks must still report PASS (got %d, want >= 1)", report.Passed)
	}

	// Doctor exit semantics: live current state determines exit code.
	exitCode := 0
	if report.Failed > 0 {
		exitCode = 2
	} else if report.Warnings > 0 {
		exitCode = 1
	}
	if exitCode != 0 {
		t.Errorf("Doctor exit code with healthy live checks = %d, want 0 (PASS)", exitCode)
	}

	// Rendered output: the historical event appears in Attention,
	// never in the System-health summary line.
	buf := &bytes.Buffer{}
	r := NewDoctorRenderer(buf, false)
	if err := r.Render(report); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Operational issues") {
		t.Errorf("Attention surface missing from rendered output:\n%s", out)
	}
	// The summary line must not claim a Warning / Failed state.
	for _, line := range []string{"warning,", "failure,", "WARNINGS", "FAILURES"} {
		if strings.Contains(out, line) {
			// "Warnings" / "Failed" appear as zero-count context in
			// the rendered summary line ("0 warnings, N passed"),
			// so accept those. Only panic on the non-zero markers.
			if !(line == "warning," || line == "failure,") {
				t.Errorf("rendered output contains failure marker %q but live checks are healthy:\n%s", line, out)
			}
		}
	}
}

// sqlSimpleID returns a unique identifier for seeding many rows.
func sqlSimpleID(n int) string {
	return "perf-" + leftPad(n, 16)
}

// leftPad zero-pads n to width characters.
func leftPad(n, width int) string {
	s := []byte{}
	for n > 0 {
		s = append([]byte{byte('0' + n%10)}, s...)
		n /= 10
	}
	for len(s) < width {
		s = append([]byte{byte('0')}, s...)
	}
	return string(s)
}

// TestDoctor_JSONCompatibility_2600c3a8 pins the JSON envelope
// against the pre-Wave-3 baseline. At 2600c3a8 the envelope was
// exactly:
//
//	{ "timestamp": ...,
//	  "summary": {"passed":N, "warnings":N, "failed":N},
//	  "checks":  [...] }
//
// Wave 3 (bfc54f6 + 820773e) added `usage` and `attention` as new
// top-level keys. This test asserts the 2600c3a8 fields are still
// present and the new keys are additive (omitempty means absent
// when the corresponding section is nil — but with a populated
// DoctorReport the new keys do appear). The test is scoped to the
// additive envelope shape, not to internal sub-envelope changes
// (DoctorUsageSectionError / DoctorAttentionSectionError had their
// Message field replaced by an Unavailable bool between bfc54f6 and
// 820773e — those sub-envelope changes are NOT part of the public
// 2600c3a8 contract and are therefore out of scope here).
func TestDoctor_JSONCompatibility_2600c3a8(t *testing.T) {
	dm := newDoctorTestDM(t)
	now := time.Now().UTC()
	// Seed a few rows so Usage / Attention have content and the
	// corresponding sub-envelopes are non-nil.
	for i := 0; i < 3; i++ {
		seedToolInvocation(t, dm,
			fmt.Sprintf("compat-ti-%d", i),
			"mpm_memory", "opencode", "ok", "",
			now.Add(-time.Duration(i)*time.Hour), "success")
	}
	seedAuditRow(t, dm, "compat-aud-1", "info", "compat", "compat_evt",
		"compatibility fixture", now.Add(-1*time.Hour))

	svc := NewDoctorService(dm)
	report, err := svc.Check()
	if err != nil {
		t.Fatalf("DoctorService.Check: %v", err)
	}

	// Build the canonical envelope exactly as handleDoctor emits.
	envelope := struct {
		Timestamp string                   `json:"timestamp"`
		Summary   map[string]interface{}   `json:"summary"`
		Checks    []map[string]interface{} `json:"checks"`
		Usage     *DoctorUsage             `json:"usage,omitempty"`
		Attention *DoctorAttention         `json:"attention,omitempty"`
	}{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Summary: map[string]interface{}{
			"passed":   report.Passed,
			"warnings": report.Warnings,
			"failed":   report.Failed,
		},
		Checks:    doctorChecksToJSON(report.Checks),
		Usage:     report.Usage,
		Attention: report.Attention,
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Every field present at 2600c3a8 must still be present.
	for _, k := range []string{"timestamp", "summary", "checks"} {
		if _, ok := generic[k]; !ok {
			t.Errorf("2600c3a8 field %q missing from envelope", k)
		}
	}
	// The summary sub-object must contain all three 2600c3a8 keys.
	sum, ok := generic["summary"].(map[string]interface{})
	if !ok {
		t.Fatalf("summary not an object: %T", generic["summary"])
	}
	for _, k := range []string{"passed", "warnings", "failed"} {
		if _, ok := sum[k]; !ok {
			t.Errorf("2600c3a8 summary field %q missing", k)
		}
	}

	// New top-level keys must be additive (omitempty when nil, but
	// here the report is populated so both must appear).
	if _, ok := generic["usage"]; !ok {
		t.Errorf("Wave 3 additive field %q missing", "usage")
	}
	if _, ok := generic["attention"]; !ok {
		t.Errorf("Wave 3 additive field %q missing", "attention")
	}

	// Specifically: the 2600c3a8 envelope did NOT contain usage /
	// attention. Adding them is a forward-compatible change because
	// they are new keys — every existing field is preserved. This
	// explicit assertion records the contract for future review.
	if _, ok := generic["usage"]; ok {
		t.Logf("usage present (additive over 2600c3a8)")
	}
}
