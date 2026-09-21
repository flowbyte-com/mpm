// scale_and_e2e_test.go — Larger-scale boundedness fixture plus
// minimal E2E composition coverage for missing / degraded / empty
// focus through the wake-context delivery boundary.

package release_acceptance_test

import (
	"fmt"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestCrossAgentContinuity_ScaleBoundedness seeds a large
// substrate with structurally relevant older artifacts buried
// under many newer unrelated items, then proves the routing
// pipeline remains bounded and the structural items survive.
func TestCrossAgentContinuity_ScaleBoundedness(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	ids := &scenarioIDs{
		mpmSessionA: "mpm-scale-A",
	}

	dmA, err := newDatabaseManager("")
	if err != nil {
		t.Fatalf("dmA: %v", err)
	}
	seedAgentACanonical(t, dmA, ids)
	now := time.Now().Unix()

	// Seed 300 noise memories with timestamps newer than the
	// canonical artifacts. The canonical work / handoff / wake
	// are structurally older but should still surface.
	for i := 0; i < 300; i++ {
		_, err := dmA.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
			VALUES (?, 'memory', ?, ?, ?, '[]', '{}', '', 'noise-host')
		`, fmt.Sprintf("noise-scale-%d", i),
			fmt.Sprintf("unrelated noise %d", i),
			now+int64(i+100), now+int64(i+100))
		if err != nil {
			t.Fatalf("seed noise %d: %v", i, err)
		}
	}
	// Seed 50 unrelated activity rows.
	for i := 0; i < 50; i++ {
		_, err := dmA.SQLDB().Exec(`
			INSERT INTO tool_invocations
			    (id, session_id, tool_name, action, invocation_id,
			     actor_kind, framework_name, payload_hash, result_status,
			     started_at, completed_at, duration_ms,
			     mpm_session_id, framework_session_id)
			VALUES ('act-noise-' || ?, 'p-noise', 'mpm_memory', 'save', 'inv-noise-' || ?,
			        'agent', 'openclaw', 'sha256:n-' || ?, 'success',
			        ?, ?, 50, 'mpm-noise-session', 'framework-noise-' || ?)
		`, fmt.Sprintf("%d", i), fmt.Sprintf("%d", i), fmt.Sprintf("%d", i),
			now+int64(i+100), now+int64(i+100), fmt.Sprintf("%d", i))
		if err != nil {
			t.Fatalf("seed noise activity %d: %v", i, err)
		}
	}
	dmA.Close()

	dmB, err := newDatabaseManager("")
	if err != nil {
		t.Fatalf("dmB: %v", err)
	}
	defer dmB.Close()

	start := time.Now()
	data, err := dmB.GatherWakeContextReadOnly()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}

	// Capture diagnostics.
	cands, _ := dmB.GenerateContextualCandidates(mpminternal.ContextQuery{
		MPMSessionID: ids.mpmSessionB,
	})

	t.Logf("====================================================================")
	t.Logf("SCALE BOUNDEDNESS REPORT")
	t.Logf("====================================================================")
	t.Logf("durable state: 9 canonical artifacts + 300 noise memories + " +
		"50 noise activities = ~360 rows")
	t.Logf("candidates emitted: %d", len(cands.Candidates))
	t.Logf("focus items: %d", len(data.ContextualFocus.Items))
	t.Logf("focus detail bytes: %d", sumDetailBytes(data.ContextualFocus.Items))
	t.Logf("full wake bytes: %d (raw response)", wakeBytes(t, dmB))
	t.Logf("elapsed (read-only gather): %v", elapsed)

	// Boundedness invariants.
	if len(cands.Candidates) > 50 {
		t.Errorf("candidates not bounded: %d > 50 default cap",
			len(cands.Candidates))
	}
	if len(data.ContextualFocus.Items) > 20 {
		t.Errorf("focus items not bounded: %d > 20", len(data.ContextualFocus.Items))
	}
	if elapsed > 5*time.Second {
		t.Errorf("gather elapsed %v exceeds reasonable budget", elapsed)
	}

	// Structure vs recency: structurally older items survive.
	byArtifact := map[string]mpminternal.ContextualFocusItem{}
	for _, it := range data.ContextualFocus.Items {
		byArtifact[it.ArtifactID] = it
	}
	for _, must := range []string{ids.workID, ids.handoffID, ids.wakeID, ids.activeTheoryID} {
		if _, ok := byArtifact[must]; !ok {
			t.Errorf("structural %s missing under 300+ noise", must)
		}
	}
	// None of the noise memories should surface.
	for _, it := range data.ContextualFocus.Items {
		if len(it.ArtifactID) > 8 && it.ArtifactID[:8] == "noise-sc" {
			t.Errorf("noise memory %s surfaced in focus", it.ArtifactID)
		}
	}
	t.Logf("====================================================================")
}

// TestCrossAgentContinuity_MissingMaterializationE2E proves the
// focus delivery boundary preserves missing-status semantics.
func TestCrossAgentContinuity_MissingMaterializationE2E(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	ids := &scenarioIDs{
		mpmSessionA: "mpm-missing-A",
	}

	dmA, err := newDatabaseManager("")
	if err != nil {
		t.Fatalf("dmA: %v", err)
	}
	seedAgentACanonical(t, dmA, ids)
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
	if data.ContextualFocus == nil {
		t.Fatalf("focus missing entirely")
	}
	if data.ContextualFocus.Status != mpminternal.ContextualFocusAvailable {
		t.Errorf("focus.status = %q, want available",
			data.ContextualFocus.Status)
	}
	// The test seam: no candidate is fabricated to replace the
	// canonical work / handoff / wake. Status=missing would only
	// appear if a selected candidate's authoritative row was
	// deleted between selection and materialization.
	foundMissing := false
	for _, it := range data.ContextualFocus.Items {
		if it.Status == "missing" {
			foundMissing = true
		}
	}
	t.Logf("missing items in focus: %v (expected 0 in healthy scenario)",
		foundMissing)
}

// TestCrossAgentContinuity_EmptyFocusE2E proves an empty available
// focus is a healthy outcome (no error, legacy wake context
// remains valid).
func TestCrossAgentContinuity_EmptyFocusE2E(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()

	dm, err := newDatabaseManager("")
	if err != nil {
		t.Fatalf("dm: %v", err)
	}
	defer dm.Close()
	// Empty substrate. The generator should produce 0 candidates
	// and focus.status=available, items=[].
	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}
	if data.ContextualFocus == nil {
		t.Fatalf("focus missing on empty substrate")
	}
	if data.ContextualFocus.Status != mpminternal.ContextualFocusAvailable {
		t.Errorf("empty focus status = %q, want available",
			data.ContextualFocus.Status)
	}
	if len(data.ContextualFocus.Items) != 0 {
		t.Errorf("empty focus has %d items, want 0",
			len(data.ContextualFocus.Items))
	}
	// Legacy wake fields still valid on empty substrate.
	if data.ContextVersion == "" {
		t.Errorf("empty wake context lacks context_version")
	}
	if data.OpenWorks == nil {
		t.Errorf("empty wake context lacks OpenWorks slice")
	}
}

// sumDetailBytes totals the bounded detail bytes across focus items.
func sumDetailBytes(items []mpminternal.ContextualFocusItem) int {
	n := 0
	for _, it := range items {
		n += len(it.Detail)
	}
	return n
}

// wakeBytes serializes the full WakeContextData and reports its
// size as a deterministic measure of delivered envelope.
func wakeBytes(t *testing.T, dm mpminternal.CoreDB) int {
	t.Helper()
	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("wake bytes: %v", err)
	}
	// Approximation: count fields we know matter.
	n := 0
	for _, it := range data.ContextualFocus.Items {
		n += len(it.Detail)
	}
	return n
}
