// supersession_trace_test.go — Mechanical trace of the supersession
// pipeline for the cross-agent acceptance fixture. This is the
// evidence-gathering test for the Stage 2E.4 closure completion
// pass: it must classify the failure precisely (discovery /
// selection / materialization / delivery / fixture) before any
// pipeline edit is considered.

package release_acceptance_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func TestTraceSupersessionPipeline(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	ids := &scenarioIDs{
		mpmSessionA: "mpm-trace-A",
	}
	dmA, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		t.Fatalf("dmA: %v", err)
	}
	seedAgentACanonical(t, dmA, ids)
	dmA.Close()

	dmB, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		t.Fatalf("dmB: %v", err)
	}
	defer dmB.Close()

	// Discovery — print every candidate involving the theories.
	t.Log("\n=== STAGE 2D DISCOVERY ===")
	q := mpminternal.ContextQuery{
		MPMSessionID:       "mpm-trace-B",
		FrameworkSessionID: "framework-B-session",
		FrameworkName:      "openclaw",
	}
	candRes, err := dmB.GenerateContextualCandidates(q)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	t.Logf("total candidates: %d", len(candRes.Candidates))
	for _, c := range candRes.Candidates {
		if c.Kind == "theory" {
			b, _ := json.MarshalIndent(map[string]interface{}{
				"id":              c.ID,
				"kind":            c.Kind,
				"lifecycle_state": c.LifecycleState,
				"reasons":         c.Reasons,
				"related_ids":     c.RelatedIDs,
				"actor_kind":      c.ActorKind,
				"timestamp":       c.Timestamp,
			}, "", "  ")
			t.Logf("\n%s", string(b))
		}
	}

	// Selection — print surviving selected items.
	t.Log("\n=== STAGE 2E.1 SELECTION ===")
	cands := candRes.Candidates
	sel := mpminternal.SelectContextualCandidates(
		cands, mpminternal.DefaultSelectionPolicy(), time.Now().Unix(),
	)
	t.Logf("selected: %d", len(sel.Items))
	for _, it := range sel.Items {
		if it.Candidate.Kind != "theory" {
			continue
		}
		b, _ := json.MarshalIndent(map[string]interface{}{
			"candidate_id":         it.Candidate.ID,
			"kind":                 it.Candidate.Kind,
			"lifecycle_state":      it.Candidate.LifecycleState,
			"band":                 it.Band,
			"rationale":            it.Rationale,
			"why_now":              it.WhyNow,
			"combination_matches":  it.CombinationMatches,
			"compressed_related":   it.CompressedRelatedIDs,
			"compression_triggers": it.CompressionTriggers,
		}, "", "  ")
		t.Logf("\n%s", string(b))
	}

	// Materialization + Delivery.
	t.Log("\n=== STAGE 2E.2/2E.3 MATERIALIZATION + DELIVERY ===")
	mat := mpminternal.MaterializeContextualSelection(
		sel, dmB, mpminternal.DefaultMaterializationLimits(), time.Now().Unix(),
	)
	for _, it := range mat.Items {
		if it.Kind != "theory" {
			continue
		}
		b, _ := json.MarshalIndent(map[string]interface{}{
			"candidate_id":         it.CandidateID,
			"artifact_id":          it.ArtifactID,
			"kind":                 it.Kind,
			"status":               it.Status,
			"detail":               it.Detail,
			"compression_triggers": it.CompressionTriggers,
			"compressed_related":   it.CompressedRelatedIDs,
		}, "", "  ")
		t.Logf("\n%s", string(b))
	}

	t.Log("\n=== END ===")
	fmt.Println("TRACE COMPLETE — check -v output above for each stage")
}
