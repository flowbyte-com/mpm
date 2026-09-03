package tools

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core"
)

// newTestSharedDM opens a hermetic local+shared tmpfile DM via the
// internal test helper. Wrapper kept so callers in this package don't
// all need to know about the internal package name.
func newTestSharedDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	return internal.NewTestSharedDM(t)
}

// TestHandleRecordGlobalRule_RejectsMissingConfirm verifies the
// operator gate: the handler refuses the call when confirm=true is
// not present. Without this gate, agents could autonomously write
// house rules — the original concern from docs/archive/shared-epistemology.md Phase 3.
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

	// Insert a local memory to promote. The id is unique-per-run so the
	// test never collides with prior runs (and never relies on stale
	// rows in the workspace DB, which the prior version of this test
	// accidentally did via a hardcoded "local-promote-001" lookup).
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
		"memory_id": localPromoteID,
		"confirm":   true,
	})
	if err != nil {
		t.Fatalf("handlePromoteToGlobal: %v", err)
	}
	m := result.(map[string]interface{})
	if m["local_id"] != localPromoteID {
		t.Errorf("local_id mismatch: got %v, want %s", m["local_id"], localPromoteID)
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

// newTestIsolatedDM opens a hermetic in-memory DatabaseManager with the
// canonical MPM schema. Unlike newTestSharedDM this does NOT attach the
// workspace MPM_SHARED_DB, so tests using it cannot pollute the workspace
// database or read state from prior tests. Use this for tests that need
// a clean slate.
func newTestIsolatedDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	return internal.NewTestDM(t)
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

// TestHandleListActiveClusters_RegistryEntryWired pins that the
// aggregator that hosts this action is registered. After the
// 2026-08-11 aggregator redesign, list_active_clusters is now
// reached via mpm_system action=`list_clusters`. The test asserts
// the aggregator exists; the action enum is documented in the
// tool's Description.
func TestHandleListActiveClusters_RegistryEntryWired(t *testing.T) {
	var found bool
	for _, tool := range Registry {
		if tool.Name == "mpm_system" {
			found = true
			if tool.Handler == nil {
				t.Error("mpm_system registry entry has nil Handler")
			}
			if tool.Description == "" {
				t.Error("mpm_system registry entry has empty Description")
			}
			if !strings.Contains(tool.Description, "list_clusters") {
				t.Errorf("mpm_system description must mention `list_clusters` action (got: %q)", tool.Description)
			}
			if len(tool.Schema) == 0 {
				t.Error("mpm_system registry entry has empty Schema")
			}
			break
		}
	}
	if !found {
		t.Error("mpm_system not in Registry — list_active_clusters dispatch path is broken")
	}
}

// TestHandleCommitMilestone_RegistryEntryWired pins the aggregator
// that hosts this action. After the 2026-08-11 aggregator redesign,
// commit_milestone is now reached via mpm_memory action=`commit_milestone`.
func TestHandleCommitMilestone_RegistryEntryWired(t *testing.T) {
	var entry *Tool
	for i, tool := range Registry {
		if tool.Name == "mpm_memory" {
			entry = &Registry[i]
			break
		}
	}
	if entry == nil {
		t.Fatal("mpm_memory not in Registry — commit_milestone dispatch path is broken")
	}
	if entry.Handler == nil {
		t.Error("mpm_memory registry entry has nil Handler")
	}
	if !strings.Contains(entry.Description, "commit_milestone") {
		t.Errorf("mpm_memory description must mention `commit_milestone` action (got: %q)", entry.Description)
	}
}

// TestHandleCommitMilestone_RequiresSummary pins the floor that
// `summary` must be non-empty. Trimmed further by the length test below.
func TestHandleCommitMilestone_RequiresSummary(t *testing.T) {
	dm := newTestSharedDM(t)
	_, err := handleCommitMilestone(dm, internal.ActiveContext{}, map[string]interface{}{
		// summary intentionally omitted
	})
	if err == nil {
		t.Fatal("expected error when summary missing, got nil")
	}
	if !strings.Contains(err.Error(), "summary is required") {
		t.Errorf("expected 'summary is required' error, got: %v", err)
	}
}

// TestHandleCommitMilestone_RejectsShortSummary pins the 50-char
// minimum. The threshold encodes "could another agent defend this
// claim from the summary alone?" — a 20-char string like "shipped
// scratchpad" fails that test.
func TestHandleCommitMilestone_RejectsShortSummary(t *testing.T) {
	dm := newTestSharedDM(t)
	short := strings.Repeat("a", MinMilestoneSummaryChars-1)
	_, err := handleCommitMilestone(dm, internal.ActiveContext{}, map[string]interface{}{
		"summary": short,
	})
	if err == nil {
		t.Fatal("expected error for summary < 50 chars, got nil")
	}
	if !strings.Contains(err.Error(), "must be at least 50") {
		t.Errorf("expected length-floor error, got: %v", err)
	}
	// Just over the floor should succeed (validates boundary).
	justOver := strings.Repeat("a", MinMilestoneSummaryChars)
	_, err = handleCommitMilestone(dm, internal.ActiveContext{}, map[string]interface{}{
		"summary": justOver,
	})
	if err != nil {
		t.Fatalf("expected success for summary == %d chars, got: %v", MinMilestoneSummaryChars, err)
	}
}

// TestHandleCommitMilestone_DefaultFlavorIsShipped verifies that
// omitting `flavor` defaults to "shipped". The taxonomy is the whole
// point — silent fallback to "insight" or "" would dilute the signal.
func TestHandleCommitMilestone_DefaultFlavorIsShipped(t *testing.T) {
	dm := newTestSharedDM(t)
	summary := "Closed the morning's MPM mechanical cleanup arc: handoff UPSERT fix + MCP schema gap + stderr allowlist reconciliation + README."
	out, err := handleCommitMilestone(dm, internal.ActiveContext{}, map[string]interface{}{
		"summary": summary,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out)
	}
	tags, _ := m["tags"].([]string)
	wantTag := "type:milestone-shipped"
	found := false
	for _, t := range tags {
		if t == wantTag {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected tag %q in %v, got none", wantTag, tags)
	}
}

// TestHandleCommitMilestone_ExplicitFlavor verifies the taxonomy gate
// can be overridden explicitly. "insight" milestones are durable
// learnings (architectural rules, anti-patterns) — different from work
// shipped.
func TestHandleCommitMilestone_ExplicitFlavor(t *testing.T) {
	dm := newTestSharedDM(t)
	summary := "Lesson learned: an UPSERT's read_at-preserve-on-overwrite is a silent shadow-write; the row is unreadable to the read-side filter for the entire lifetime of the first read. UPSERT must reset read state."
	out, err := handleCommitMilestone(dm, internal.ActiveContext{}, map[string]interface{}{
		"summary": summary,
		"flavor":  "insight",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, _ := out.(map[string]interface{})
	tags, _ := m["tags"].([]string)
	wantTag := "type:milestone-insight"
	found := false
	for _, t := range tags {
		if t == wantTag {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected tag %q in %v, got none", wantTag, tags)
	}
}

// TestHandleCommitMilestone_InvalidFlavor pins the enum. Anything
// outside shipped / insight is rejected — typos here would create
// un-aggregable tags the wake-context query wouldn't surface.
func TestHandleCommitMilestone_InvalidFlavor(t *testing.T) {
	dm := newTestSharedDM(t)
	summary := strings.Repeat("x", MinMilestoneSummaryChars+10)
	_, err := handleCommitMilestone(dm, internal.ActiveContext{}, map[string]interface{}{
		"summary": summary,
		"flavor":  "sh1pped", // typo
	})
	if err == nil {
		t.Fatal("expected error for invalid flavor, got nil")
	}
	if !strings.Contains(err.Error(), "flavor must be") {
		t.Errorf("expected enum rejection error, got: %v", err)
	}
}

// TestHandleCommitMilestone_DoublePrefixDedupe pins the dedupe path —
// a caller who pre-tags with the canonical form should not get two
// copies in the stored tag list. The dedupe is what keeps the wake-
// context query symmetric across all milestone tags.
func TestHandleCommitMilestone_DoublePrefixDedupe(t *testing.T) {
	dm := newTestSharedDM(t)
	summary := strings.Repeat("y", MinMilestoneSummaryChars+10)
	out, err := handleCommitMilestone(dm, internal.ActiveContext{}, map[string]interface{}{
		"summary": summary,
		"flavor":  "shipped",
		"tags":    "type:milestone-shipped,custom-tag",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, _ := out.(map[string]interface{})
	tags, _ := m["tags"].([]string)
	count := 0
	for _, t := range tags {
		if t == "type:milestone-shipped" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 occurrence of type:milestone-shipped, got %d (tags=%v)", count, tags)
	}
	// Caller-supplied custom-tag must be preserved.
	hasCustom := false
	for _, t := range tags {
		if t == "custom-tag" {
			hasCustom = true
			break
		}
	}
	if !hasCustom {
		t.Errorf("expected custom-tag in %v, missing", tags)
	}
}

// TestHandleCommitMilestone_PersistsThroughWakeContext verifies the
// full path: commit_milestone writes a memory tagged type:milestone-*,
// and the wake-context gather picks it up inside the 30d window. This
// is the integration test the schema-guard test cannot see — the
// handler and the wake-context query must agree on the tag anchor.
//
// The full path is exercised in internal/wake_context_milestones_test.go
// where `dm.recentMilestones` is in scope. Here in `tools/` we limit
// the assertion to what's accessible: that the memory row is persisted
// with the canonical tag, queryable from the same DB the wake-context
// gather reads from.
func TestHandleCommitMilestone_PersistsWithCanonicalTag(t *testing.T) {
	dm := newTestSharedDM(t)
	summary := "Closing the wake-context narrative-arc gap: added Recent Milestones block (5 strategic slots) carved out of the Recent Memories tactical envelope (10->5); budget held constant."
	out, err := handleCommitMilestone(dm, internal.ActiveContext{}, map[string]interface{}{
		"summary": summary,
		"flavor":  "shipped",
	})
	if err != nil {
		t.Fatalf("commit_milestone failed: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out)
	}
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatal("expected memory id in output, got empty")
	}
	// Confirm the row exists with the canonical tag by querying the
	// DB directly. The wake-context gather uses the same tag anchor
	// (verified in internal/wake_context_milestones_test.go).
	var storedTags string
	err = dm.SQLDB().QueryRow(`SELECT tags FROM memories WHERE id = ?`, id).Scan(&storedTags)
	if err != nil {
		t.Fatalf("expected row to exist: %v", err)
	}
	if !strings.Contains(storedTags, "type:milestone-shipped") {
		t.Errorf("milestone row missing canonical tag, tags column: %s", storedTags)
	}
}

// TestHandleReadWakeContext_IncludesRecentMilestones pins the wire
// contract: a milestone written via commit_milestone must surface in
// the read_wake_context response under the recent_milestones key.
//
// This catches the drift pattern where WakeContextData (the internal
// struct) gains a new field but handleReadWakeContext (the handler
// that maps struct → JSON response map) silently omits it. The two
// surfaces had drifted on every other field addition prior to this
// fix; this test pins the contract going forward.
func TestHandleReadWakeContext_IncludesRecentMilestones(t *testing.T) {
	// Use a hermetic in-memory DB instead of newTestSharedDM so the test
	// is independent of prod-DB state and independent of test-ordering
	// races with sibling commit_milestone tests. The newTestSharedDM
	// helper opens the production DB (mpmPath is hardcoded in
	// NewDatabaseManager); without hermetic isolation, the LIMIT 5
	// window can exclude our row when other tests commit in the same
	// second.
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	// Active state lives in ~/.mpm/active.json (read by readActiveState
	// during GatherWakeContext). For the in-memory test we don't care
	// about it; ignore any read errors.
	summary := "Closing the wake-context narrative-arc gap at the handler layer too: handler copy must match struct, otherwise the wire contract drifts silently — wake context milestone regression test."
	out0, err := handleCommitMilestone(dm, internal.ActiveContext{}, map[string]interface{}{
		"summary": summary,
		"flavor":  "shipped",
	})
	if err != nil {
		t.Fatalf("commit_milestone failed: %v", err)
	}
	m0, _ := out0.(map[string]interface{})
	myID, _ := m0["id"].(string)
	if myID == "" {
		t.Fatal("commit_milestone did not return a memory id")
	}

	out, err := handleReadWakeContext(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("read_wake_context failed: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out)
	}
	rawMilestones, exists := m["recent_milestones"]
	if !exists {
		t.Fatal("recent_milestones key missing from read_wake_context response — handler copy drifted from WakeContextData struct")
	}
	refs, ok := rawMilestones.([]map[string]interface{})
	if !ok {
		t.Fatalf("expected recent_milestones to be []map[string]interface{}, got %T", rawMilestones)
	}
	if len(refs) != 1 {
		t.Fatalf("expected exactly 1 milestone in recent_milestones (hermetic DB), got %d: %+v", len(refs), refs)
	}
	// Wire contract (updated 2026-08-25): milestone refs carry
	// id/summary/pointer/created_at, matching WakeContextMemory's JSON
	// tags ("summary", not the pre-split "content") and mirroring the
	// recent_memories ref shape so both lists share one contract.
	if refs[0]["summary"] != summary {
		t.Errorf("milestone summary drift:\n got: %s\nwant: %s", refs[0]["summary"], summary)
	}
	if refs[0]["pointer"] != "mpm://memory/"+myID {
		t.Errorf("milestone pointer drift: got %v, want mpm://memory/%s", refs[0]["pointer"], myID)
	}
	if refs[0]["id"] != myID {
		t.Errorf("milestone id drift: got %s, want %s", refs[0]["id"], myID)
	}
}

// TestHandleReadWakeContext_IncludesOverdueWakes pins the wire
// contract: a scheduled_wake with fired=0 and a past target_time must
// surface in the read_wake_context response under the overdue_wakes
// key.
//
// Companion to the wake_context_overdue_wakes_test.go tests in
// internal/core — those pin the SQL/logic at the gather layer, this
// test pins the handler copy. The pattern is identical to
// TestHandleReadWakeContext_IncludesRecentMilestones: same drift
// pattern (struct gains a field, handler drops it on the wire), same
// hermetic in-memory DB, same field-presence check.
//
// 2026-08-13 bootstrap: the patch that adds overdue_wakes to the
// wake-context primer was prompted by the silent failure where an
// overdue reminder wake sat 4d13h past its target without any
// surface surfacing it. handleReadWakeContext builds the response
// map by hand — any new WakeContextData field has to be explicitly
// projected, otherwise the wire contract drifts silently.
func TestHandleReadWakeContext_IncludesOverdueWakes(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Seed a single overdue wake (fired=0, target_time in the past) and a
	// future-dated wake that must NOT surface.
	now := time.Now().Unix()
	past := now - 7200   // 2 hours overdue
	future := now + 3600 // 1 hour in the future

	seedOverdueWake := func(id string, target int64, fired int, kind, reason string) {
		var meta any
		if kind == "" {
			meta = nil
		} else {
			meta = fmt.Sprintf(`{"kind":%q}`, kind)
		}
		_, err := db.Exec(`
			INSERT INTO scheduled_wakes (id, target_time, reason, fired, fired_at, created_by, metadata)
			VALUES (?, ?, ?, ?, NULL, 'test', ?)`,
			id, target, reason, fired, meta)
		if err != nil {
			t.Fatalf("seed wake %s: %v", id, err)
		}
	}
	seedOverdueWake("wk-overdue", past, 0, "reminder", "Should surface in overdue_wakes")
	seedOverdueWake("wk-fired", past, 1, "reminder", "Already fired — must not surface")
	seedOverdueWake("wk-future", future, 0, "reminder", "Future-dated — must not surface")

	out, err := handleReadWakeContext(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("read_wake_context: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out)
	}

	rawOverdue, exists := m["overdue_wakes"]
	if !exists {
		t.Fatal("overdue_wakes key missing from read_wake_context response — handler copy drifted from WakeContextData struct")
	}
	refs, ok := rawOverdue.([]map[string]interface{})
	if !ok {
		t.Fatalf("expected overdue_wakes to be []map[string]interface{}, got %T", rawOverdue)
	}
	if len(refs) != 1 {
		t.Fatalf("expected exactly 1 row in overdue_wakes (one past+fired=0 row), got %d: %+v", len(refs), refs)
	}
	got := refs[0]
	if got["id"] != "wk-overdue" {
		t.Errorf("overdue_wakes id drift: got %v, want wk-overdue", got["id"])
	}
	if got["kind"] != "reminder" {
		t.Errorf("overdue_wakes kind drift: got %v, want reminder", got["kind"])
	}
	// overdue_secs should be positive (target_time was 2h ago).
	if secs, ok := got["overdue_secs"].(float64); !ok || secs <= 0 {
		t.Errorf("overdue_secs drift: got %v (%T), want positive float64", got["overdue_secs"], got["overdue_secs"])
	}
	if reason, _ := got["reason"].(string); reason != "Should surface in overdue_wakes" {
		t.Errorf("overdue_wakes reason drift: got %q, want %q", reason, "Should surface in overdue_wakes")
	}

	// Also assert the field is ALWAYS present even when there's nothing
	// overdue. This catches the omitempty-style silent omission at the
	// handler layer specifically (cf. the underlying struct field which
	// we already pinned in wake_context_overdue_wakes_test.go).
	cleanDB, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open clean db: %v", err)
	}
	t.Cleanup(func() { cleanDB.Close() })
	cleanDM := internal.NewDatabaseManagerForDB(cleanDB)
	if err := cleanDM.InitSchema(); err != nil {
		t.Fatalf("init clean schema: %v", err)
	}
	out2, err := handleReadWakeContext(cleanDM, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("read_wake_context (clean): %v", err)
	}
	m2 := out2.(map[string]interface{})
	if _, present := m2["overdue_wakes"]; !present {
		t.Fatal("overdue_wakes key absent on clean DB — handler must always surface the field, even as []")
	}
}

// TestHandleReadWakeContext_IncludesEpistemicPressure pins the
// proprioception contract at the handler boundary: every
// read_wake_context response must surface an `epistemic_pressure`
// block with raw_count, lesson_count, ratio, threshold, and exceeded.
// The handler is the wire-format layer; if it drops the field,
// the agent loses its cognitive-load signal without any error.
//
// This is the structural companion to TestEpistemicPressure_* in
// internal/core — those tests pin the SQL/logic, this test pins
// the handler copy. Pattern matches the recent_milestones regression
// test above: same hermetic in-memory DB, same field-presence check.
func TestHandleReadWakeContext_IncludesEpistemicPressure(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	out, err := handleReadWakeContext(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("read_wake_context: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out)
	}
	epRaw, exists := m["epistemic_pressure"]
	if !exists {
		t.Fatal("epistemic_pressure key missing from read_wake_context response — proprioception contract broken")
	}
	ep, ok := epRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("epistemic_pressure should be a map, got %T", epRaw)
	}
	for _, k := range []string{"raw_count", "lesson_count", "ratio", "threshold", "exceeded"} {
		if _, present := ep[k]; !present {
			t.Errorf("epistemic_pressure missing sub-field %q", k)
		}
	}
	// Cold-start: ratio must be 0.0 (not NaN, not +Inf) when lesson_count
	// is 0. Catches a regression in the divide-by-zero guard.
	if r, _ := ep["ratio"].(float64); r != 0.0 {
		t.Errorf("cold-start ratio: got %v, want 0.0 (divide-by-zero guard)", r)
	}
	if thr, _ := ep["threshold"].(float64); int(thr) != 100 {
		t.Errorf("default threshold: got %v, want 100", thr)
	}
}

// TestHandleReadWakeContext_IncludesOpenWorks pins the wire-format
// contract: works with status=open must surface in the read_wake_context
// response under the open_works key. This test covers both populated and
// empty cases. The open_works field was added to WakeContextData but
// was absent from the handler's JSON serialization map — a silent drift
// that caused the JSON wire format to omit pending work items while the
// system-prompt format (which calls ReadWakeContext directly) rendered
// them correctly.
func TestHandleReadWakeContext_IncludesOpenWorks(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Case 1: empty — open_works key must be present (not omitted)
	out, err := handleReadWakeContext(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("read_wake_context: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out)
	}
	rawWorks, exists := m["open_works"]
	if !exists {
		t.Fatal("open_works key missing from read_wake_context response — handler copy drifted from WakeContextData struct")
	}
	works, ok := rawWorks.([]internal.WakeContextWork)
	if !ok {
		// WakeContextWork is not []interface{} so this cast may fail;
		// try the generic interface{} slice path
		worksIF, ok := rawWorks.([]interface{})
		if !ok {
			t.Fatalf("expected open_works to be []WakeContextWork or []interface{}, got %T", rawWorks)
		}
		if len(worksIF) != 0 {
			t.Fatalf("expected empty open_works, got %d items", len(worksIF))
		}
	} else {
		if len(works) != 0 {
			t.Fatalf("expected empty open_works, got %d items", len(works))
		}
	}

	// Case 2: populated — create a work item and verify it appears
	w, err := dm.AddWork("Audit findings need fixing", "Close the open_works JSON drift", "session-openworks-test")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	out2, err := handleReadWakeContext(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("read_wake_context after AddWork: %v", err)
	}
	m2, ok := out2.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out2)
	}
	rawWorks2, exists2 := m2["open_works"]
	if !exists2 {
		t.Fatal("open_works key missing from response after AddWork")
	}
	works2, ok := rawWorks2.([]internal.WakeContextWork)
	if !ok {
		works2IF, ok := rawWorks2.([]interface{})
		if !ok {
			t.Fatalf("expected []WakeContextWork, got %T", rawWorks2)
		}
		if len(works2IF) != 1 {
			t.Fatalf("expected 1 work item, got %d", len(works2IF))
		}
		// At least verify it's a non-empty slice
	} else {
		if len(works2) != 1 {
			t.Fatalf("expected 1 work item in open_works, got %d", len(works2))
		}
		if works2[0].ID != w.ID {
			t.Errorf("open_works[0].ID: got %s, want %s", works2[0].ID, w.ID)
		}
		if works2[0].Verification != internal.WorkVerificationUnverified {
			t.Errorf("new work verification: got %s, want unverified", works2[0].Verification)
		}
	}
}

// TestHandleReadWakeContext_IncludesCompletedWorks pins the wire-format
// contract: works with status=done must surface in the read_wake_context
// response under the completed_works key. RECOMMENDED 10 had populated
// WakeContextData.CompletedWorks but no public surface (CLI or MCP)
// actually rendered it — making this the "dead projection" regression
// fix. The pattern mirrors TestHandleReadWakeContext_IncludesOpenWorks:
// prove the empty case still emits the key (so callers can branch on
// presence, not absence) and prove the populated case emits each item
// with the right id and pointer.
func TestHandleReadWakeContext_IncludesCompletedWorks(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Case 1: empty — completed_works key must be present (not omitted)
	out, err := handleReadWakeContext(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("read_wake_context: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out)
	}
	rawWorks, exists := m["completed_works"]
	if !exists {
		t.Fatal("completed_works key missing from read_wake_context response — handler copy drifted from WakeContextData struct")
	}
	if _, ok := rawWorks.([]internal.WakeContextWork); !ok {
		t.Fatalf("completed_works should be []WakeContextWork, got %T", rawWorks)
	}

	// Case 2: populated — create a work, complete it, verify it surfaces
	w, err := dm.AddWork("RECOMMENDED 10 dead data fix", "Completed work must reach wake-context consumers", "completed-works-test")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}
	if _, err := dm.CompleteWork(w.ID); err != nil {
		t.Fatalf("CompleteWork: %v", err)
	}

	out2, err := handleReadWakeContext(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("read_wake_context after CompleteWork: %v", err)
	}
	m2, ok := out2.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out2)
	}
	rawWorks2, exists2 := m2["completed_works"]
	if !exists2 {
		t.Fatal("completed_works key missing from response after CompleteWork")
	}
	works2, ok := rawWorks2.([]internal.WakeContextWork)
	if !ok {
		t.Fatalf("expected []WakeContextWork, got %T", rawWorks2)
	}
	if len(works2) != 1 {
		t.Fatalf("expected 1 completed work, got %d", len(works2))
	}
	if works2[0].ID != w.ID {
		t.Errorf("completed_works[0].ID: got %s, want %s", works2[0].ID, w.ID)
	}
	if works2[0].Pointer != "mpm://work/"+w.ID {
		t.Errorf("completed_works[0].Pointer: got %s, want mpm://work/%s", works2[0].Pointer, w.ID)
	}
	if works2[0].Title == "" {
		t.Error("completed_works[0].Title: empty")
	}

	// Case 3: active vs completed are distinct — a work in 'open' status
	// must not appear in completed_works.
	wOpen, err := dm.AddWork("Still open", "Should not appear in completed_works", "completed-works-test")
	if err != nil {
		t.Fatalf("AddWork (open): %v", err)
	}
	out3, err := handleReadWakeContext(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("read_wake_context after AddWork (open): %v", err)
	}
	m3 := out3.(map[string]interface{})
	rawWorks3 := m3["completed_works"].([]internal.WakeContextWork)
	for _, item := range rawWorks3 {
		if item.ID == wOpen.ID {
			t.Errorf("open work %s leaked into completed_works", wOpen.ID)
		}
	}
}

// ── health_check tool ──────────────────────────────────────────────────

// TestHandleHealthCheck_PassesThrough verifies the tool-layer wrapper
// invokes DM.HealthCheck() and returns its payload. Pins that the
// registered tool is just a thin shim over the DM method.
func TestHandleHealthCheck_PassesThrough(t *testing.T) {
	dm := newTestSharedDM(t)
	out, err := handleHealthCheck(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("handleHealthCheck: %v", err)
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", out)
	}
	if okVal, _ := m["ok"].(bool); !okVal {
		t.Errorf("ok: got false, want true; payload=%+v", m)
	}
	if _, present := m["busy_retries"]; !present {
		t.Error("busy_retries missing from handler payload")
	}
}

// TestHandleHealthCheck_ReflectsState pins that the handler reflects
// domain state — adding a memory bumps memories_active. The boot path
// seeds the four constitutional directives into file-backed DMs, so
// the assertion is a delta over the baseline rather than an absolute.
func TestHandleHealthCheck_ReflectsState(t *testing.T) {
	dm := newTestSharedDM(t)
	var baseline int64
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`,
	).Scan(&baseline); err != nil {
		t.Fatalf("baseline count: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, deleted_at, created_at, updated_at)
		VALUES ('mem-hc-handler-1', 'memories', 'handler test', NULL, '2026-07-06', '2026-07-06')
	`); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	out, err := handleHealthCheck(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("handleHealthCheck: %v", err)
	}
	m := out.(map[string]interface{})
	if n, _ := m["memories_active"].(int64); n != baseline+1 {
		t.Errorf("memories_active: got %d, want %d", n, baseline+1)
	}
}

// ── annotate_cluster ──────────────────────────────────────────────────

// ── mpm_memory domain dispatcher ─────────────────────────────────────

// TestMpmMemoryDispatch routes each action through the unified dispatcher
// and verifies it reaches the underlying handler (no "unknown action" error).
// Actions that require specific DB state or params are tested with the
// minimum viable payload.
func TestMpmMemoryDispatch(t *testing.T) {
	dm := newTestSharedDM(t)

	// Seed a memory for actions that need an existing row.
	seedID := fmt.Sprintf("mem-dispatch-%d", time.Now().UnixNano())
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, deleted_at, created_at, updated_at)
		VALUES (?, 'memories', 'dispatch test fact', 5, NULL, '2026-08-11', '2026-08-11')
	`, seedID)
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	tests := []struct {
		name    string
		action  string
		params  map[string]interface{}
		wantErr bool
		errMsg  string
	}{
		{
			name:   "save routes to handleSaveToMemory",
			action: "save",
			params: map[string]interface{}{
				"fact": "dispatch save test",
			},
		},
		{
			name:   "query routes to handleQueryLongTermMemory",
			action: "query",
			params: map[string]interface{}{
				"query": "dispatch",
			},
		},
		{
			name:   "shred routes to handleShredMemory",
			action: "shred",
			params: map[string]interface{}{
				"memory_id": seedID,
			},
		},
		{
			name:   "reinforce routes to handleReinforceMemory",
			action: "reinforce",
			params: map[string]interface{}{
				"memory_id": seedID,
				"delta":     1,
			},
		},
		{
			name:   "weaken routes to handleWeakenMemory",
			action: "weaken",
			params: map[string]interface{}{
				"memory_id": seedID,
				"delta":     1,
			},
		},
		{
			name:   "snooze routes to handleSnoozeMemory",
			action: "snooze",
			params: map[string]interface{}{
				"memory_id": seedID,
				"days":      1,
			},
		},
		{
			name:   "set_weight routes to handleSetMemoryWeight",
			action: "set_weight",
			params: map[string]interface{}{
				"memory_id": seedID,
				"weight":    10,
			},
		},
		{
			name:   "promote routes to handlePromoteMemory",
			action: "promote",
			params: map[string]interface{}{
				"memory_id": seedID,
			},
		},
		{
			name:   "review routes to handleReviewMemories",
			action: "review",
			params: map[string]interface{}{},
		},
		{
			name:   "challenge routes to handleChallengeMemory with normalization",
			action: "challenge",
			params: map[string]interface{}{
				"memory_id": seedID,
				"evidence":  "contradictory finding",
			},
		},
		{
			name:   "commit_milestone routes to handleCommitMilestone",
			action: "commit_milestone",
			params: map[string]interface{}{
				"summary": "Shipped the unified domain dispatcher for mpm_memory with normalized casing across all actions",
				"flavor":  "shipped",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := map[string]interface{}{
				"action": tt.action,
				"params": tt.params,
			}
			_, err := handleMpmMemory(dm, internal.ActiveContext{}, payload)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errMsg)
				} else if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got: %v", tt.errMsg, err)
				}
				return
			}
			if err != nil {
				// "unknown action" errors are dispatch failures; other errors
				// mean the dispatcher routed correctly but the handler rejected
				// bad input — that's fine for this test.
				if strings.Contains(err.Error(), "unknown action") {
					t.Errorf("dispatch failed (unknown action): %v", err)
				}
			}
		})
	}
}

// TestMpmMemoryUnknownAction verifies the dispatcher returns a descriptive
// error with the valid action list when given an unrecognized action.
func TestMpmMemoryUnknownAction(t *testing.T) {
	dm := newTestSharedDM(t)
	payload := map[string]interface{}{
		"action": "nonexistent_action",
		"params": map[string]interface{}{},
	}
	_, err := handleMpmMemory(dm, internal.ActiveContext{}, payload)
	if err == nil {
		t.Fatal("expected error for unknown action, got nil")
	}
	if !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("error should mention 'unknown action', got: %v", err)
	}
	if !strings.Contains(err.Error(), "nonexistent_action") {
		t.Errorf("error should mention the bad action name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Valid actions include") {
		t.Errorf("error should list valid actions, got: %v", err)
	}
}

// TestMpmMemoryChallengeNormalization verifies that the challenge action
// normalizes memory_id (snake_case) to memoryId (camelCase) for the
// underlying handler.
func TestMpmMemoryChallengeNormalization(t *testing.T) {
	dm := newTestSharedDM(t)

	// Seed a memory to challenge.
	normID := fmt.Sprintf("mem-challenge-norm-%d", time.Now().UnixNano())
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, deleted_at, created_at, updated_at)
		VALUES (?, 'memories', 'challengeable fact', 5, NULL, '2026-08-11', '2026-08-11')
	`, normID)
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	// Send memory_id (snake_case) — the dispatcher must normalize to memoryId.
	payload := map[string]interface{}{
		"action": "challenge",
		"params": map[string]interface{}{
			"memory_id": normID,
			"evidence":  "new contradictory evidence",
		},
	}
	_, err = handleMpmMemory(dm, internal.ActiveContext{}, payload)
	if err != nil {
		// The handler may fail for other reasons (e.g., no theory table),
		// but it must NOT fail with "memoryId is required" — that would
		// mean normalization didn't happen.
		if strings.Contains(err.Error(), "memoryId is required") {
			t.Errorf("normalization failed: memoryId not passed through: %v", err)
		}
	}
}

// TestMpmMemoryMissingParamsContract verifies the alpha-4.1.1 D-006
// envelope contract: missing `params` is OPTIONAL (returns empty map so
// actions like `mpm_decisions action=list` can be invoked without
// `params:{}` boilerplate), but a wrong-typed `params` (e.g. nil,
// string, number) still hard-fails with the canonical type-mismatch
// error.
//
// Pre-D-006 contract (regressed, fixed): `params` was strictly required
// and a missing key returned "missing required field `params`" with
// a Go error. Post-D-006 contract: missing key is silently treated as
// an empty map; only wrong types fail loudly. The test below pins both
// halves of the new contract.
func TestMpmMemoryMissingParamsContract(t *testing.T) {
	dm := newTestSharedDM(t)

	// Missing params key → no error, treated as empty map. Use a safe
	// action (review with default filter) so the call succeeds end-to-end.
	_, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "review",
	})
	if err != nil {
		t.Fatalf("missing params must be tolerated (D-006): got error %v", err)
	}

	// params=nil (present but nil-valued) → loud failure naming the type mismatch.
	_, err = handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": nil,
	})
	if err == nil {
		t.Fatal("expected loud failure for params: nil, got nil — silent-drop regression")
	}
	if !strings.Contains(err.Error(), "must be an object") {
		t.Fatalf("expected type-mismatch message, got: %v", err)
	}
}

// ── All-domain dispatch tests ───────────────────────────────────────

// TestAllDomainDispatchers routes one safe action per domain through each
// unified dispatcher, verifying: (1) the dispatcher doesn't panic,
// (2) unknown actions return descriptive errors, and (3) nil params
// are handled gracefully.
func TestAllDomainDispatchers(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := internal.ActiveContext{}

	type domainTest struct {
		name    string
		handler func(internal.CoreDB, internal.ActiveContext, map[string]interface{}) (interface{}, error)
		action  string
		params  map[string]interface{}
	}

	tests := []domainTest{
		// mpm_handoff
		{"handoff/list", handleMpmHandoff, "list", map[string]interface{}{}},
		{"handoff/read", handleMpmHandoff, "read", map[string]interface{}{}},
		// session/shred_handoff: requires a real handoff id; seed in the
		// dedicated TestShredHandoff_HappyPath test. The dispatch smoke
		// here just verifies the action is recognised (no unknown-action
		// error) — the underlying DeleteHandoff returns an error if id is
		// missing, which is the documented contract.
		{"handoff/shred", handleMpmHandoff, "shred", map[string]interface{}{"id": "nonexistent"}},
		// mpm_wakes
		{"wakes/list", handleMpmWakes, "list", map[string]interface{}{}},
		{"wakes/list_tasks", handleMpmWakes, "list_tasks", map[string]interface{}{}},
		{"wakes/digest", handleMpmWakes, "digest", map[string]interface{}{}},
		// mpm_theories
		{"theories/propose", handleMpmTheories, "propose", map[string]interface{}{"hypothesis": "test hypothesis for dispatch"}},
		// mpm_lessons
		{"lessons/list", handleMpmLessons, "list", map[string]interface{}{}},
		{"lessons/search", handleMpmLessons, "search", map[string]interface{}{"query": "test"}},
		// mpm_decisions
		{"decisions/record", handleMpmDecisions, "record", map[string]interface{}{"context": "test context", "choice": "test choice", "rationale": "test rationale"}},
		// mpm_topics
		{"topics/search", handleMpmTopics, "search", map[string]interface{}{"query": "test"}},
		// mpm_references
		{"references/list", handleMpmReferences, "list", map[string]interface{}{}},
		// mpm_evidence
		{"evidence/list", handleMpmEvidence, "list", map[string]interface{}{"artifact_id": "nonexistent"}},
		// mpm_confidence
		{"confidence/quality", handleMpmConfidence, "quality", map[string]interface{}{}},
		{"confidence/show", handleMpmConfidence, "show", map[string]interface{}{"artifact_id": "nonexistent"}},
		// mpm_skills
		{"skills/list", handleMpmSkills, "list", map[string]interface{}{}},
		// mpm_context
		{"context/read_wake_context", handleMpmContext, "read_wake_context", map[string]interface{}{}},
		{"context/read_directives", handleMpmContext, "read_directives", map[string]interface{}{}},
		{"context/query_global_rules", handleMpmContext, "query_global_rules", map[string]interface{}{}},
		// mpm_system
		{"system/health_check", handleMpmSystem, "health_check", map[string]interface{}{}},
		{"system/list_clusters", handleMpmSystem, "list_clusters", map[string]interface{}{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := map[string]interface{}{
				"action": tt.action,
				"params": tt.params,
			}
			_, err := tt.handler(dm, ac, payload)
			if err != nil && strings.Contains(err.Error(), "unknown action") {
				t.Errorf("dispatch failed (unknown action): %v", err)
			}
		})
	}
}

// TestAllDomainUnknownActions verifies each dispatcher returns a
// descriptive error for unrecognized actions.
func TestAllDomainUnknownActions(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := internal.ActiveContext{}

	type dispatcherInfo struct {
		name    string
		handler func(internal.CoreDB, internal.ActiveContext, map[string]interface{}) (interface{}, error)
	}

	dispatchers := []dispatcherInfo{
		{"mpm_handoff", handleMpmHandoff},
		{"mpm_scratchpad", handleMpmScratchpad},
		{"mpm_wakes", handleMpmWakes},
		{"mpm_theories", handleMpmTheories},
		{"mpm_lessons", handleMpmLessons},
		{"mpm_decisions", handleMpmDecisions},
		{"mpm_topics", handleMpmTopics},
		{"mpm_references", handleMpmReferences},
		{"mpm_evidence", handleMpmEvidence},
		{"mpm_confidence", handleMpmConfidence},
		{"mpm_skills", handleMpmSkills},
		{"mpm_context", handleMpmContext},
		{"mpm_system", handleMpmSystem},
	}

	for _, d := range dispatchers {
		t.Run(d.name, func(t *testing.T) {
			payload := map[string]interface{}{
				"action": "bogus_action",
				"params": map[string]interface{}{},
			}
			_, err := d.handler(dm, ac, payload)
			if err == nil {
				t.Errorf("%s: expected error for bogus action, got nil", d.name)
				return
			}
			if !strings.Contains(err.Error(), "unknown action") {
				t.Errorf("%s: error should mention 'unknown action', got: %v", d.name, err)
			}
			if !strings.Contains(err.Error(), "bogus_action") {
				t.Errorf("%s: error should mention the bad action name, got: %v", d.name, err)
			}
			if !strings.Contains(err.Error(), "Valid actions include") {
				t.Errorf("%s: error should list valid actions, got: %v", d.name, err)
			}
		})
	}
}

// TestAllDomainNilParams verifies each dispatcher tolerates missing
// params (alpha-4.1.1 D-006) and rejects wrong-typed params (params=nil).
//
// Contract split (D-006):
//   - missing key   → tolerated, treated as empty map (no error)
//   - params=nil    → loud "must be an object" failure
//
// Pre-D-006 contract (now superseded): missing key was a loud
// "missing required field `params`" error. The 2026-08-13 hardening
// pass codified that strict contract; D-006 relaxed it because no
// downstream consumer actually required the explicit envelope.
func TestAllDomainNilParams(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := internal.ActiveContext{}

	type dispatcherInfo struct {
		name    string
		handler func(internal.CoreDB, internal.ActiveContext, map[string]interface{}) (interface{}, error)
		action  string
	}

	// Each entry picks the action most likely to tolerate an empty params
	// map (read-only list / show / query). The D-006 contract being pinned
	// here is the **envelope layer**: `extractParamsOrFail` must accept
	// missing or wrong-typed params without crashing, regardless of whether
	// the inner action then complains about its own required fields.
	// We therefore choose actions whose dispatch does not gate on any
	// inner required field — otherwise the test would conflate envelope
	// tolerance (D-006) with inner-field validation (a separate contract).
	dispatchers := []dispatcherInfo{
		{"mpm_handoff", handleMpmHandoff, "list"},
		{"mpm_wakes", handleMpmWakes, "list"},
		{"mpm_theories", handleMpmTheories, "resolve"}, // falls through to inner validation; envelope-only check
		{"mpm_lessons", handleMpmLessons, "list"},
		{"mpm_decisions", handleMpmDecisions, "list"}, // list accepts empty params (status/limit are optional)
		{"mpm_topics", handleMpmTopics, "create"},     // requires name; envelope-only check
		{"mpm_references", handleMpmReferences, "list"},
		{"mpm_evidence", handleMpmEvidence, "source_groups"}, // no inner required field
		{"mpm_confidence", handleMpmConfidence, "quality"},
		{"mpm_skills", handleMpmSkills, "list"},
		{"mpm_context", handleMpmContext, "read_wake_context"},
		{"mpm_system", handleMpmSystem, "health_check"},
	}

	for _, d := range dispatchers {
		t.Run(d.name+"_missing_params", func(t *testing.T) {
			// D-006: missing `params` is tolerated (returns empty map).
			// The handler may still error on inner required fields — that
			// is a separate contract. What we pin here is: the error must
			// NOT be the envelope-level "missing required field `params`"
			// shape that pre-D-006 codepath produced.
			_, err := d.handler(dm, ac, map[string]interface{}{
				"action": d.action,
			})
			if err != nil {
				if strings.Contains(err.Error(), "missing required field `params`") {
					t.Fatalf("%s: missing params must be tolerated (D-006): got envelope error %v", d.name, err)
				}
				// Inner-field validation errors (e.g. "name is required",
				// "theoryId is required") are acceptable — the envelope
				// contract is preserved.
			}

			// params=nil (present but nil-valued) → loud failure for type mismatch.
			_, err = d.handler(dm, ac, map[string]interface{}{
				"action": d.action,
				"params": nil,
			})
			if err == nil {
				t.Fatalf("%s: expected loud failure for params: nil, got nil — silent-drop regression", d.name)
			}
			if !strings.Contains(err.Error(), "must be an object") {
				t.Fatalf("%s: expected type-mismatch message, got: %v", d.name, err)
			}
		})
	}
}

// ── 2026-08-13 hardening regression tests ────────────────────────────
//
// Two specific failure modes from the silent-promotion-by-edge-case
// archaeology. Both were catching the day through the SQLite cross-check
// rule, but never reaching the Go test suite — so a future agent could
// have re-introduced either shape without any test failing. These tests
// pin the loud-fail contract and the db_path surface to disk.

func TestExtractParamsOrFail_LoudFailureTopLevelFact(t *testing.T) {
	dm, ac := newTestSharedDM(t), internal.ActiveContext{Agent: "test", SessionID: "test"}

	// Pre-fix shape: {"action":"save","fact":"..."} — top-level `fact`
	// outside the params envelope. Pre-fix this silently dropped the
	// fact, called handleSaveToMemory with empty params, and returned
	// "fact is required" — far away from the source of the bug.
	// Post-fix extractParamsOrFail catches the top-level `fact` and
	// returns a descriptive schema error naming the offending field.
	_, err := handleMpmMemory(dm, ac, map[string]interface{}{
		"action": "save",
		"fact":   "I should be inside params.fact, not at the top level",
	})
	if err == nil {
		t.Fatal("expected loud failure for top-level `fact`, got nil")
	}
	if !strings.Contains(err.Error(), "outside the params envelope") ||
		!strings.Contains(err.Error(), "fact") ||
		!strings.Contains(err.Error(), "mpm_memory") {
		t.Fatalf("expected schema-error message naming mpm_memory + `fact`, got: %v", err)
	}
}

func TestExtractParamsOrFail_LoudFailureTopLevelLeakAcrossAllDomainTools(t *testing.T) {
	dm, ac := newTestSharedDM(t), internal.ActiveContext{Agent: "test", SessionID: "test"}

	// Same regression as the all-loop above, but this one specifically
	// tests top-level field leakage (the original failing pattern
	// discovered at scope=all) across every dispatcher with the new
	// "outside the params envelope" message.
	tools := []struct {
		name    string
		handler func(internal.CoreDB, internal.ActiveContext, map[string]interface{}) (interface{}, error)
	}{
		{"mpm_memory", handleMpmMemory},
		{"mpm_handoff", handleMpmHandoff},
		{"mpm_scratchpad", handleMpmScratchpad},
		{"mpm_wakes", handleMpmWakes},
		{"mpm_theories", handleMpmTheories},
		{"mpm_lessons", handleMpmLessons},
		{"mpm_decisions", handleMpmDecisions},
		{"mpm_topics", handleMpmTopics},
		{"mpm_references", handleMpmReferences},
		{"mpm_evidence", handleMpmEvidence},
		{"mpm_confidence", handleMpmConfidence},
		{"mpm_skills", handleMpmSkills},
		{"mpm_context", handleMpmContext},
		{"mpm_system", handleMpmSystem},
	}
	for _, tcase := range tools {
		t.Run(tcase.name, func(t *testing.T) {
			_, err := tcase.handler(dm, ac, map[string]interface{}{
				"action":            "anything",
				"top_level_leak":    "should fail with schema error",
			})
			if err == nil {
				t.Fatalf("%s: expected loud failure for top-level leak, got nil", tcase.name)
			}
			if !strings.Contains(err.Error(), tcase.name) {
				t.Fatalf("%s: error message should name the tool, got: %v", tcase.name, err)
			}
			if !strings.Contains(err.Error(), "outside the params envelope") {
				t.Fatalf("%s: expected 'outside the params envelope' message, got: %v", tcase.name, err)
			}
		})
	}
}

func TestHealthCheck_DbPathSurface(t *testing.T) {
	dm, ac := newTestSharedDM(t), internal.ActiveContext{Agent: "test", SessionID: "test"}

	// The 2026-08-13 hardening exposed db_path / db_path_raw /
	// shared_attached / shared_path in health_check so integration
	// plugins can assert host-pinned path invariants at boot. Verify
	// they are present (non-empty strings for db_path on an opened db).
	res, err := handleMpmSystem(dm, ac, map[string]interface{}{
		"action": "health_check",
		"params": map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("health_check failed: %v", err)
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map response, got %T", res)
	}
	for _, key := range []string{"db_path", "db_path_raw", "shared_attached", "shared_path"} {
		if _, present := m[key]; !present {
			t.Errorf("health_check response missing key %q", key)
		}
	}
	if dbp, _ := m["db_path"].(string); dbp == "" {
		t.Errorf("expected non-empty db_path, got: %v", m["db_path"])
	}
}

// TestShredHandoff_ByIdempotentBySessionID verifies the full lifecycle
// and the idempotency contract closed by MPM-GAP-SHRED-HANDOFF-2026-08-19:
// shredding by session_id after the row is already gone must return the
// same no-op shape as the by-id path (shredded=false, rows_deleted=0)
// instead of a validation error.
func TestShredHandoff_ByIdempotentBySessionID(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := internal.ActiveContext{Agent: "test", SessionID: "test"}

	seed, err := dm.EndSession("test_probe_session_999", "Ephemeral probe handoff", internal.HandoffClean, nil, nil)
	if err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	first, err := handleMpmHandoff(dm, ac, map[string]interface{}{
		"action": "shred",
		"params": map[string]interface{}{"session_id": "test_probe_session_999"},
	})
	if err != nil {
		t.Fatalf("first shred by session_id: %v", err)
	}
	m, ok := first.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", first)
	}
	if m["shredded"] != true || m["rows_deleted"] != int64(1) {
		t.Fatalf("first shred: want shredded=true rows_deleted=1, got %v", m)
	}

	second, err := handleMpmHandoff(dm, ac, map[string]interface{}{
		"action": "shred",
		"params": map[string]interface{}{"session_id": "test_probe_session_999"},
	})
	if err != nil {
		t.Fatalf("second shred by session_id must be a no-op, got error: %v", err)
	}
	m, ok = second.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", second)
	}
	if m["shredded"] != false || m["rows_deleted"] != int64(0) {
		t.Errorf("second shred: want shredded=false rows_deleted=0, got %v", m)
	}

	if _, err := dm.GetHandoffByID(seed.ID); err != sql.ErrNoRows {
		t.Errorf("handoff %s still readable after shred, err=%v", seed.ID, err)
	}
}

// TestScrubChallengeIdentifier_PublicSurfaces covers the RECOMMENDED 11
// fix: the scrubber must remove CHALLENGED_MEMORY_ID and
// CHALLENGED_AT_NANO regardless of where they appear in the stored
// challenge content. The previous implementation assumed a specific
// ordering (CHALLENGED_AT_NANO immediately after CHALLENGED_MEMORY_ID)
// and silently leaked the timestamp when the real stored layout placed
// it between EVIDENCE and ORIGINAL_CONTENT.
//
// The test exercises three layouts that all exist in practice:
//
//	Layout A: id, nano, evidence, original   (legacy / what old scrubber assumed)
//	Layout B: id, evidence, nano, original   (current stored layout)
//	Layout C: evidence, nano, original       (id missing edge)
//
// plus regressions:
//   - non-challenge content is returned verbatim
//   - benign content mentioning CHALLENGED_MEMORY_ID inside an EVIDENCE
//     line is NOT destroyed
//   - the full public wake projection (handleReadWakeContext) shows no
//     forbidden internal field — this is the user-visible contract.
func TestScrubChallengeIdentifier_PublicSurfaces(t *testing.T) {
	// Layout B is the actual stored layout per
	// internal/core/epistemology_tools.go theoryContent template.
	const layoutB = "CHALLENGED_MEMORY_ID: abc-123\nEVIDENCE: counterexample\nCHALLENGED_AT_NANO: 1787816542845373355\nORIGINAL_CONTENT: theory text"

	tests := []struct {
		name         string
		input        string
		wantContains []string
		wantOmit     []string
	}{
		{
			name:         "LayoutA_id_nano_evidence_original",
			input:        "CHALLENGED_MEMORY_ID: a-1\nCHALLENGED_AT_NANO: 42\nEVIDENCE: e\nORIGINAL_CONTENT: c",
			wantContains: []string{"(atomic challenge)", "EVIDENCE", "ORIGINAL_CONTENT"},
			wantOmit:     []string{"CHALLENGED_MEMORY_ID", "CHALLENGED_AT_NANO"},
		},
		{
			name:         "LayoutB_id_evidence_nano_original_current_stored",
			input:        layoutB,
			wantContains: []string{"(atomic challenge)", "EVIDENCE", "ORIGINAL_CONTENT", "counterexample", "theory text"},
			wantOmit:     []string{"CHALLENGED_MEMORY_ID", "CHALLENGED_AT_NANO", "abc-123", "1787816542845373355"},
		},
		{
			name:         "LayoutC_nano_only_no_id",
			input:        "EVIDENCE: e\nCHALLENGED_AT_NANO: 42\nORIGINAL_CONTENT: c",
			wantContains: []string{"EVIDENCE", "ORIGINAL_CONTENT"},
			wantOmit:     []string{"CHALLENGED_AT_NANO", "42"},
		},
		{
			name:         "non_challenge_passthrough",
			input:        "This is just a regular memory about caching strategies.",
			wantContains: []string{"caching strategies"},
			wantOmit:     []string{"(atomic challenge)"},
		},
		{
			name:         "benign_prose_mentioning_CHALLENGED_MEMORY_ID_inside_EVIDENCE_line",
			input:        "EVIDENCE: the user pointed out that CHALLENGED_MEMORY_ID was leaked in the previous version",
			wantContains: []string{"CHALLENGED_MEMORY_ID was leaked"}, // inside EVIDENCE — must be preserved
			wantOmit:     []string{},
		},
		{
			name:         "id_at_end",
			input:        "EVIDENCE: e\nORIGINAL_CONTENT: c\nCHALLENGED_MEMORY_ID: a-1",
			wantContains: []string{"EVIDENCE", "ORIGINAL_CONTENT"},
			wantOmit:     []string{"CHALLENGED_MEMORY_ID", "a-1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubChallengeIdentifier(tc.input)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q\ninput:  %q\noutput: %q", want, tc.input, got)
				}
			}
			for _, forbidden := range tc.wantOmit {
				if strings.Contains(got, forbidden) {
					t.Errorf("output leaked forbidden %q\ninput:  %q\noutput: %q", forbidden, tc.input, got)
				}
			}
		})
	}
}

// TestHandleReadWakeContext_NoChallengeMetadataLeak is the public-
// surface integration test for the scrubber fix. It writes a memory
// whose content matches the actual stored challenge layout and
// confirms that the JSON wire payload returned by read_wake_context
// contains neither CHALLENGED_MEMORY_ID nor CHALLENGED_AT_NANO. This
// is the contract the agent sees — the helper-level test above proves
// the algorithm; this one proves the wire.
func TestHandleReadWakeContext_NoChallengeMetadataLeak(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Mirror the actual stored layout from
	// internal/core/epistemology_tools.go: theoryContent template.
	challengeContent := "CHALLENGED_MEMORY_ID: deadbeef-1234\n" +
		"EVIDENCE: counterexample finding\n" +
		"CHALLENGED_AT_NANO: 1787816542845373355\n" +
		"ORIGINAL_CONTENT: the contested claim"
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, tags, created_at) VALUES (?, 'theories', ?, '[]', ?)`,
		"theory-challenge-leak", challengeContent, time.Now().Unix(),
	); err != nil {
		t.Fatalf("insert challenge theory: %v", err)
	}

	out, err := handleReadWakeContext(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("read_wake_context: %v", err)
	}
	// Marshal the full result to JSON to mirror what an MCP client
	// actually receives on the wire.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload := string(raw)
	if strings.Contains(payload, "CHALLENGED_MEMORY_ID") {
		t.Errorf("CHALLENGED_MEMORY_ID leaked into wake payload: %s", payload)
	}
	if strings.Contains(payload, "CHALLENGED_AT_NANO") {
		t.Errorf("CHALLENGED_AT_NANO leaked into wake payload: %s", payload)
	}
	if strings.Contains(payload, "1787816542845373355") {
		t.Errorf("internal timestamp leaked into wake payload: %s", payload)
	}
	if strings.Contains(payload, "deadbeef-1234") {
		t.Errorf("internal memory id leaked into wake payload: %s", payload)
	}
	// EVIDENCE / ORIGINAL_CONTENT should still be visible.
	if !strings.Contains(payload, "counterexample") {
		t.Errorf("EVIDENCE content lost during scrub: %s", payload)
	}
	if !strings.Contains(payload, "contested claim") {
		t.Errorf("ORIGINAL_CONTENT lost during scrub: %s", payload)
	}
}

// TestHandleListEvidence_RejectsEmptyArtifactID pins the handler-layer
// validation: missing artifact_id must error at the delivery layer, not
// silently return an empty array. Without this, agents cannot distinguish
// "no evidence exists" from "I forgot to pass the id."
func TestHandleListEvidence_RejectsEmptyArtifactID(t *testing.T) {
	dm := newTestSharedDM(t)
	_, err := handleListEvidence(dm, internal.ActiveContext{}, map[string]interface{}{
		"artifact_type": "memory",
	})
	if err == nil {
		t.Fatal("expected error when artifact_id is empty")
	}
	if !strings.Contains(err.Error(), "artifact_id is required") {
		t.Errorf("error must mention artifact_id, got: %v", err)
	}
}

// TestHandleListEvidence_RejectsEmptyArtifactType pins strict validation
// on artifact_type: silently defaulting to "memory" was arbitrary and
// statistically wrong for the majority of artifact types (theory, lesson,
// decision, skill, work). The schema defines artifact_type as an enum of
// six distinct values — masking a missing value with "memory" destroys
// contract integrity. MPM-BUG-LIST-EVIDENCE-DEAF-2026-08-27.
func TestHandleListEvidence_RejectsEmptyArtifactType(t *testing.T) {
	dm := newTestSharedDM(t)
	_, err := handleListEvidence(dm, internal.ActiveContext{}, map[string]interface{}{
		"artifact_id": "mem-1",
	})
	if err == nil {
		t.Fatal("expected error when artifact_type is empty")
	}
	if !strings.Contains(err.Error(), "artifact_type is required") {
		t.Errorf("error must mention artifact_type, got: %v", err)
	}
}

// TestHandleListEvidence_HappyPath verifies the handler still returns
// the evidence map when both required fields are supplied. Regression
// guard against the validation change accidentally breaking the
// supported call shape.
func TestHandleListEvidence_HappyPath(t *testing.T) {
	dm := newTestSharedDM(t)
	_, _ = dm.SQLDB().Exec(`INSERT INTO memories (id, collection, content) VALUES ('mem-hp', 'memories', 'x')`)
	_, err := handleListEvidence(dm, internal.ActiveContext{}, map[string]interface{}{
		"artifact_id":   "mem-hp",
		"artifact_type": "memory",
	})
	if err != nil {
		t.Fatalf("expected success with valid payload, got: %v", err)
	}
}

// stubProviderOK is a test double that returns a fixed embedding vector.
type stubProviderOK struct{}

func (stubProviderOK) Embed(text string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3, 0.4}, nil
}
func (stubProviderOK) Name() string { return "stub-ok" }

// stubProviderFail is a test double that always returns an error.
type stubProviderFail struct{ err error }

func (p stubProviderFail) Embed(text string) ([]float32, error) { return nil, p.err }
func (p stubProviderFail) Name() string                        { return "stub-fail" }

// TestHandleSaveToMemory_EmbeddingAvailable verifies that when the embedding
// provider is reachable, the response is pure success with no embedding_status field.
func TestHandleSaveToMemory_EmbeddingAvailable(t *testing.T) {
	dm := newTestIsolatedDM(t)
	cfg := &internal.EmbeddingConfig{
		Source:       internal.EmbeddingSourceProfile,
		ProviderName: "stub-ok",
		Provider:     stubProviderOK{},
		Status:       internal.EmbeddingStatusConfigured,
	}
	prev := internal.SetEmbedConfigForTest(cfg)
	defer internal.ResetEmbedConfigForTest()
	_ = prev // restored by defer

	result, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": "test embedding available",
			"tags": []interface{}{"embed-test"},
		},
	})
	if err != nil {
		t.Fatalf("handleMpmMemory save failed: %v", err)
	}
	res := result.(map[string]interface{})

	if memID, ok := res["memory_id"].(string); !ok || memID == "" {
		t.Errorf("memory_id missing or empty: %v", res["memory_id"])
	}
	if pers, ok := res["memory_persisted"].(bool); !ok || !pers {
		t.Errorf("memory_persisted should be true: %v", res["memory_persisted"])
	}
	// No embedding_status field when provider is available (spec §4.4 Case 3)
	if _, hasStatus := res["embedding_status"]; hasStatus {
		t.Errorf("embedding_status should not be present when provider is available: %v", res["embedding_status"])
	}
}

// TestHandleSaveToMemory_EmbeddingDisabled verifies that when embeddings are
// intentionally disabled, the response carries embedding_status="disabled" and
// backfill_required=false.
func TestHandleSaveToMemory_EmbeddingDisabled(t *testing.T) {
	dm := newTestIsolatedDM(t)
	cfg := &internal.EmbeddingConfig{
		Source:                internal.EmbeddingSourceDisabled,
		ProviderName:          "null",
		IntentionallyDisabled: true,
		Status:                internal.EmbeddingStatusNull,
	}
	prev := internal.SetEmbedConfigForTest(cfg)
	defer internal.ResetEmbedConfigForTest()
	_ = prev

	result, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": "test embedding disabled",
			"tags": []interface{}{"embed-test"},
		},
	})
	if err != nil {
		t.Fatalf("handleMpmMemory save failed: %v", err)
	}
	res := result.(map[string]interface{})

	if memID, ok := res["memory_id"].(string); !ok || memID == "" {
		t.Errorf("memory_id missing or empty: %v", res["memory_id"])
	}
	if pers, ok := res["memory_persisted"].(bool); !ok || !pers {
		t.Errorf("memory_persisted should be true: %v", res["memory_persisted"])
	}
	if status, ok := res["embedding_status"].(string); !ok || status != "disabled" {
		t.Errorf("embedding_status should be 'disabled': %v", res["embedding_status"])
	}
	if backfill, ok := res["backfill_required"].(bool); !ok || backfill {
		t.Errorf("backfill_required should be false: %v", res["backfill_required"])
	}
}

// TestHandleSaveToMemory_EmbeddingUnreachable verifies that when the configured
// embedding provider is unreachable, the response is the §4.4 structured error
// shape: memory_persisted=true, embedding_status="unavailable",
// backfill_required=true, error=..., memory_id=....
func TestHandleSaveToMemory_EmbeddingUnreachable(t *testing.T) {
	dm := newTestIsolatedDM(t)
	cfg := &internal.EmbeddingConfig{
		Source:       internal.EmbeddingSourceProfile,
		ProviderName: "stub-fail",
		Provider:     stubProviderFail{err: fmt.Errorf("connection refused")},
		Status:       internal.EmbeddingStatusUnreachable,
	}
	prev := internal.SetEmbedConfigForTest(cfg)
	defer internal.ResetEmbedConfigForTest()
	_ = prev

	result, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": "test embedding unreachable",
			"tags": []interface{}{"embed-test"},
		},
	})
	if err != nil {
		t.Fatalf("handleMpmMemory save should not return error for unreachable provider, got: %v", err)
	}
	res := result.(map[string]interface{})

	if memID, ok := res["memory_id"].(string); !ok || memID == "" {
		t.Errorf("memory_id missing or empty: %v", res["memory_id"])
	}
	if pers, ok := res["memory_persisted"].(bool); !ok || !pers {
		t.Errorf("memory_persisted should be true: %v", res["memory_persisted"])
	}
	if status, ok := res["embedding_status"].(string); !ok || status != "unavailable" {
		t.Errorf("embedding_status should be 'unavailable': %v", res["embedding_status"])
	}
	if backfill, ok := res["backfill_required"].(bool); !ok || !backfill {
		t.Errorf("backfill_required should be true: %v", res["backfill_required"])
	}
	errMsg, hasErr := res["error"].(string)
	if !hasErr || errMsg == "" {
		t.Errorf("error field should be non-empty: %v", res["error"])
	}
	if hasErr && !strings.Contains(errMsg, "unreachable") {
		t.Errorf("error should mention 'unreachable': %v", errMsg)
	}
}
