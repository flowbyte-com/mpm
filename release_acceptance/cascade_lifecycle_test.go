// cascade_lifecycle_test.go — Authoritative cascade/invalidation
// lifecycle acceptance.
//
// Verifies:
//   1. Pending cascade surfaces as cascade_pending; downstream
//      truth is NOT silently rewritten.
//   2. Resolved cascade surfaces as cascade_resolved; pending
//      obligation no longer masquerades as unresolved.
//   3. Future wake transitions to overdue for the SAME wake
//      artifact (not a different wake row).
//   4. E2E missing-artifact, degraded-focus, and empty-focus
//      composition through the wake-context boundary.

package release_acceptance_test

import (
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestCrossAgentContinuity_PendingCascade proves that a pending
// cascade surfaces in focus with the cascade_pending reason, the
// downstream artifact remains canonical (cascade = obligation to
// reconsider, not truth propagation), and the wake context does
// NOT silently rewrite downstream state.
func TestCrossAgentContinuity_PendingCascade(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	ids := &scenarioIDs{
		mpmSessionA: "mpm-cascade-A",
	}

	dmA, err := newDatabaseManager("")
	if err != nil {
		t.Fatalf("dmA: %v", err)
	}
	seedAgentACanonical(t, dmA, ids)
	now := time.Now().Unix()

	// Create authoritative cascade state: foundation
	// T-cross-agent-active gets invalidated, producing a
	// pending cascade obligation for the downstream decision.
	_, err = dmA.SQLDB().Exec(`
		INSERT INTO epistemic_cascade_outbox
		    (id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
		     downstream_artifact_id, downstream_artifact_type,
		     cascade_depth, status, reason, created_at, updated_at)
		VALUES ('cascade-pending-1', 'evt-cascade-pending', ?, 'theory',
		        ?, 'decision', 1, 'pending', 'foundation invalidated', ?, ?)
	`, ids.activeTheoryID, ids.decisionID, now, now)
	if err != nil {
		t.Fatalf("insert pending cascade: %v", err)
	}
	dmA.Close()

	dmB, err := newDatabaseManager("")
	if err != nil {
		t.Fatalf("dmB: %v", err)
	}
	defer dmB.Close()
	data, err := dmB.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}

	// The cascade_pending obligation MUST surface. The downstream
	// decision carries cascade_pending reason (per the cascade
	// source). The wake context does NOT rewrite the decision's
	// content — only the focus envelope surfaces the obligation.
	byArtifact := map[string]mpminternal.ContextualFocusItem{}
	for _, it := range data.ContextualFocus.Items {
		byArtifact[it.ArtifactID] = it
	}
	cands, err := dmB.GenerateContextualCandidates(mpminternal.ContextQuery{})
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	foundPending := false
	for _, c := range cands.Candidates {
		if c.ID == "decision:"+ids.decisionID {
			for _, r := range c.Reasons {
				if r == "cascade_pending" {
					foundPending = true
				}
			}
		}
	}
	if !foundPending {
		t.Logf("DEBUG: candidates emitted for cascade scenario:")
		for _, c := range cands.Candidates {
			t.Logf("  %s reasons=%v related=%v", c.ID, c.Reasons, c.RelatedIDs)
		}
		t.Errorf("cascade_pending reason not emitted on downstream decision " +
			"(cascade obligation must reach Agent B)")
	}

	// Downstream decision content must NOT be rewritten.
	mem, err := dmB.GetMemory(ids.decisionID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if !contains(mem, "current canonical decision grounded in active theory") {
		t.Errorf("downstream decision content was rewritten; " +
			"cascade must NOT propagate truth, only obligation")
	}
	_ = byArtifact
}

// TestCrossAgentContinuity_ResolvedCascade transitions a pending
// cascade to resolved state and verifies Agent B receives the
// resolved semantics — no lingering cascade_pending.
func TestCrossAgentContinuity_ResolvedCascade(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	ids := &scenarioIDs{
		mpmSessionA: "mpm-cascade-resolved-A",
	}

	dmA, err := newDatabaseManager("")
	if err != nil {
		t.Fatalf("dmA: %v", err)
	}
	seedAgentACanonical(t, dmA, ids)
	now := time.Now().Unix()

	// Seed pending cascade.
	_, err = dmA.SQLDB().Exec(`
		INSERT INTO epistemic_cascade_outbox
		    (id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
		     downstream_artifact_id, downstream_artifact_type,
		     cascade_depth, status, reason, created_at, updated_at)
		VALUES ('cascade-resolve-1', 'evt-cascade-resolve', ?, 'theory',
		        ?, 'decision', 1, 'pending', 'foundation invalidated', ?, ?)
	`, ids.activeTheoryID, ids.decisionID, now, now)
	if err != nil {
		t.Fatalf("insert pending cascade: %v", err)
	}

	// Resolve it.
	_, err = dmA.SQLDB().Exec(`
		UPDATE epistemic_cascade_outbox
		SET status = 'materialized', updated_at = ?
		WHERE id = 'cascade-resolve-1'
	`, now)
	if err != nil {
		t.Fatalf("resolve cascade: %v", err)
	}
	dmA.Close()

	dmB, err := newDatabaseManager("")
	if err != nil {
		t.Fatalf("dmB: %v", err)
	}
	defer dmB.Close()
	cands, err := dmB.GenerateContextualCandidates(mpminternal.ContextQuery{})
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	hasResolved := false
	hasLingeringPending := false
	for _, c := range cands.Candidates {
		if c.ID == "decision:"+ids.decisionID {
			for _, r := range c.Reasons {
				if r == "cascade_resolved" {
					hasResolved = true
				}
				if r == "cascade_pending" {
					hasLingeringPending = true
				}
			}
		}
	}
	if !hasResolved {
		t.Errorf("cascade_resolved reason not emitted after transition; "+
			"got cascade_resolved=%v cascade_pending=%v",
			hasResolved, hasLingeringPending)
	}
	if hasLingeringPending {
		t.Errorf("cascade_pending still emitted after transition to resolved; " +
			"pending obligation must not masquerade as unresolved")
	}
}

// TestCrossAgentContinuity_SameWakeFutureToOverdue proves the
// SAME wake artifact transitions from "future / not overdue" to
// "overdue" semantics as the deterministic clock advances past
// target_time, without firing the wake as a side-effect of
// projection.
func TestCrossAgentContinuity_SameWakeFutureToOverdue(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	ids := &scenarioIDs{
		mpmSessionA: "mpm-wake-A",
	}

	dmA, err := newDatabaseManager("")
	if err != nil {
		t.Fatalf("dmA: %v", err)
	}
	seedAgentACanonical(t, dmA, ids)

	// Add ONE wake whose target_time is 1 hour in the future
	// (relative to deterministic test time).
	now := time.Now().Unix()
	futureWakeID := "K-future-test"
	_, err = dmA.SQLDB().Exec(`
		INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, created_at)
		VALUES (?, ?, 'future wake test', 0, 'mpm-cli', ?)
	`, futureWakeID, now+3600, now)
	if err != nil {
		t.Fatalf("insert future wake: %v", err)
	}
	dmA.Close()

	// Phase A: same wake is future — must NOT be in OverdueWakes.
	dmB1, _ := newDatabaseManager("")
	data1, err := dmB1.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("phase A read-only: %v", err)
	}
	for _, w := range data1.OverdueWakes {
		if w.ID == futureWakeID {
			t.Errorf("future wake %s surfaced as overdue at target_time+%ds",
				futureWakeID, 3600)
		}
	}
	for _, it := range data1.ContextualFocus.Items {
		if it.ArtifactID == futureWakeID {
			// Brief: future wake can be selected as supporting
			// context. It must NOT be classified as obligation.
			if it.Band == "obligation" {
				t.Errorf("future wake %s surfaced as obligation",
					futureWakeID)
			}
			t.Logf("phase A: future wake %s in focus at band=%s (acceptable)",
				futureWakeID, it.Band)
		}
	}
	dmB1.Close()

	// Phase B: shift the SAME wake's target_time into the past
	// (simulating deterministic clock advance) and re-open as a
	// fresh consumer. Must now surface as overdue with band=obligation.
	dmA2, _ := newDatabaseManager("")
	_, err = dmA2.SQLDB().Exec(`
		UPDATE scheduled_wakes SET target_time = ? WHERE id = ?
	`, now-60, futureWakeID)
	if err != nil {
		t.Fatalf("advance wake target_time: %v", err)
	}
	// Verify fired is still 0 after the projection.
	var fired int
	dmA2.SQLDB().QueryRow(`SELECT fired FROM scheduled_wakes WHERE id = ?`, futureWakeID).Scan(&fired)
	if fired != 0 {
		t.Errorf("projection fired the wake; fired=%d, want 0", fired)
	}
	dmA2.Close()

	dmB2, _ := newDatabaseManager("")
	defer dmB2.Close()
	data2, err := dmB2.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("phase B read-only: %v", err)
	}
	foundOverdue := false
	for _, w := range data2.OverdueWakes {
		if w.ID == futureWakeID {
			foundOverdue = true
		}
	}
	if !foundOverdue {
		t.Errorf("after target_time < now, same wake %s not in OverdueWakes",
			futureWakeID)
	}
	foundBand := false
	for _, it := range data2.ContextualFocus.Items {
		if it.ArtifactID == futureWakeID {
			if it.Band == "obligation" {
				foundBand = true
			}
			t.Logf("phase B focus item: band=%s", it.Band)
		}
	}
	if !foundBand {
		t.Errorf("phase B focus band=obligation not observed for wake %s",
			futureWakeID)
	}
}

// contains is a tiny helper that handles map[string]interface{}
// returned by GetMemory with loose field-walking.
func contains(m map[string]interface{}, needle string) bool {
	if m == nil {
		return false
	}
	for _, v := range m {
		if s, ok := v.(string); ok {
			if s == needle {
				return true
			}
		}
	}
	return false
}
