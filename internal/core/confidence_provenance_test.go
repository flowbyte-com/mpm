// confidence_provenance_test.go — pin the semantic contract of
// `source_group = 'auto_capture'` evidence.
//
// # The defect this file pins
//
// `memory_source_evidence_ai` (db.go) writes one evidence row for every
// memory INSERT, attributed to the writer. That row exists to answer
// "which model/source created this memory?" — it is provenance
// attribution, not an independent observation that the memory is true.
// Nothing about saving a claim makes the claim more likely to be
// correct, and a claim must not be able to raise its own confidence
// merely by existing.
//
// Two defects followed from treating it as scoring evidence:
//
//  1. The trigger wrote the row but never recomputed, so the stored
//     confidence (InitialConfidence) diverged from the confidence the
//     documented formula derives from the stored evidence rows. Any
//     later recompute (after real evidence was added) silently banked
//     the save-time self-attribution as confirmation.
//  2. `ExplainConfidence` counted the auto row as a contributor, so the
//     reasoning trace disagreed with the stored value before any
//     recompute ran.
//
// # The contract these tests enforce
//
//   - auto_capture evidence is retained (provenance queries keep
//     working) but excluded from the confidence computation.
//   - A freshly saved memory's stored confidence equals the confidence
//     derived from its scoring evidence (which is none).
//   - Recomputing a memory that only has auto_capture evidence does not
//     move its confidence and records evidence_count = 0.
//   - The explanation agrees with the stored value and names no
//     auto_capture contributor.
package internal

import (
	"math"
	"testing"
	"time"
)

// autoCaptureRows returns the auto_capture evidence rows for an artifact.
func autoCaptureRows(t *testing.T, dm *DatabaseManager, artifactID string) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND source_group = 'auto_capture'`,
		artifactID,
	).Scan(&n); err != nil {
		t.Fatalf("count auto_capture rows: %v", err)
	}
	return n
}

// storedConfidence reads the live confidence column.
func storedConfidence(t *testing.T, dm *DatabaseManager, artifactID string) float64 {
	t.Helper()
	var conf float64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, artifactID,
	).Scan(&conf); err != nil {
		t.Fatalf("read stored confidence: %v", err)
	}
	return conf
}

// saveProvenanceMemory writes a memory through the canonical save path
// (the one `mpm_memory action=save` and `mpm memory add` use) so the
// memory_source_evidence_ai trigger fires exactly as it does in
// production.
func saveProvenanceMemory(t *testing.T, dm *DatabaseManager, fact string) string {
	t.Helper()
	out, _, err := dm.SaveMemoryWithContext(
		fact, "memories", nil, 0, "",
		ActiveContext{Agent: "confidence-provenance-test", Model: "test-model", FrameworkName: "test"},
	)
	if err != nil {
		t.Fatalf("save memory: %v", err)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("save returned no id: %v", out)
	}
	return id
}

// TestAutoCaptureEvidence_IsRetainedButDoesNotScore pins the core
// contract: the trigger fires, the provenance row survives, and it
// contributes nothing to confidence.
func TestAutoCaptureEvidence_IsRetainedButDoesNotScore(t *testing.T) {
	dm := NewTestDM(t)

	id := saveProvenanceMemory(t, dm, "provenance-only evidence must not raise confidence")

	// The trigger did fire — provenance attribution is intact.
	if n := autoCaptureRows(t, dm, id); n != 1 {
		t.Fatalf("auto_capture evidence rows = %d, want 1 (trigger must still attribute the write)", n)
	}

	// A brand-new memory with no scoring evidence must sit exactly at
	// the documented initial confidence.
	initial := InitialConfidence("memory")
	if got := storedConfidence(t, dm, id); math.Abs(got-initial) > 1e-9 {
		t.Errorf("stored confidence = %v, want initial %v", got, initial)
	}

	// Recompute must not move it. Before the fix the auto_capture row
	// added +0.5 log-odds here (≈0.868 for a memory).
	if _, err := dm.RecomputeConfidence(id, "memory"); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if got := storedConfidence(t, dm, id); math.Abs(got-initial) > 1e-6 {
		t.Errorf("recompute on provenance-only memory moved confidence to %v; want it to stay at %v", got, initial)
	}

	// The confidence history row must record that zero scoring evidence
	// pieces were used, so the audit trail agrees with the computation.
	var evidenceCount int
	if err := dm.SQLDB().QueryRow(
		`SELECT evidence_count FROM confidence_history
		 WHERE artifact_id = ? ORDER BY computed_at DESC, rowid DESC LIMIT 1`,
		id,
	).Scan(&evidenceCount); err != nil {
		t.Fatalf("read confidence_history: %v", err)
	}
	if evidenceCount != 0 {
		t.Errorf("confidence_history.evidence_count = %d, want 0 (auto_capture is not scoring evidence)", evidenceCount)
	}
}

// TestExplainConfidence_AgreesWithStoredAndOmitsProvenance pins the
// explanation surface: it must reproduce the stored value and must not
// present the save-time attribution row as a contributor.
func TestExplainConfidence_AgreesWithStoredAndOmitsProvenance(t *testing.T) {
	dm := NewTestDM(t)

	id := saveProvenanceMemory(t, dm, "explanation must agree with the stored confidence")

	exp, err := dm.ExplainConfidence(id, "memory")
	if err != nil {
		t.Fatalf("explain confidence: %v", err)
	}
	explanation, _ := exp["explanation"].(map[string]interface{})
	if explanation == nil {
		t.Fatalf("explanation missing from response: %v", exp)
	}

	explained, _ := explanation["confidence"].(float64)
	stored := storedConfidence(t, dm, id)
	if math.Abs(explained-stored) > 1e-6 {
		t.Errorf("explanation confidence = %v, stored = %v; the trace must agree with the artifact", explained, stored)
	}

	components := make([]interface{}, 0)
	for _, key := range []string{"positive_evidence", "negative_evidence"} {
		if list, ok := explanation[key].([]interface{}); ok {
			components = append(components, list...)
		}
	}
	for _, raw := range components {
		c, _ := raw.(map[string]interface{})
		if sg, _ := c["source_group"].(string); sg == "auto_capture" {
			t.Errorf("explanation lists auto_capture as a confidence contributor: %v", c)
		}
	}
}

// TestRecomputeConfidence_RealEvidenceStillScores guards against
// over-correction: excluding provenance must not disable the formula.
// A real observation must still move confidence, and the result must
// match the pure formula evaluated on that observation alone.
func TestRecomputeConfidence_RealEvidenceStillScores(t *testing.T) {
	dm := NewTestDM(t)

	id := saveProvenanceMemory(t, dm, "real evidence must still move confidence")

	if _, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:         id,
		ArtifactType:       "memory",
		Type:               "observation",
		SourceGroup:        "manual_review",
		Strength:           1.0,
		IndependenceFactor: 1.0,
		CreatedBy:          "confidence-provenance-test",
		CreatedAt:          time.Now(),
	}); err != nil {
		t.Fatalf("add evidence: %v", err)
	}

	// Expected: the pure formula over one fresh strength=1.0 observation
	// with the memory's initial confidence as the prior. auto_capture
	// contributes nothing.
	initial := InitialConfidence("memory")
	want := sigmoid(math.Log(initial/(1-initial)) + 1.0)

	got := storedConfidence(t, dm, id)
	if math.Abs(got-want) > 2e-3 {
		t.Errorf("confidence after one real observation = %v, want ≈%v", got, want)
	}
	if got <= initial {
		t.Errorf("confidence after a strong real observation = %v, want > initial %v", got, initial)
	}
}
