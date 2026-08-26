package tools

import (
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
)

// TestWorkComplete_ResponseReflectsDerivedVerification is the D3 regression
// test (2026-08-25).
//
// Bug: handleCompleteWork / handleCancelWork returned the Work snapshot
// captured by CompleteWorkWithContext — BEFORE DeriveWorkVerification ran —
// so the response reported the stale pre-derivation verification value while
// the persisted row held the correct derived one.
//
// Invariant: lifecycle mutation and verification derivation remain
// orthogonal, but a command response must reflect the state produced by that
// command. This test asserts the RESPONSE value, then cross-checks it against
// a fresh show (persisted state) — they must agree.
func TestWorkComplete_ResponseReflectsDerivedVerification(t *testing.T) {
	dm := newTestSharedDM(t)

	// Create work.
	res, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{"title": "D3 stale payload probe"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	workID := res.(map[string]interface{})["id"].(string)

	// Outcome evidence (source_group=test → hasOutcome → verified per
	// DeriveWorkVerification's documented decision rules). Added BEFORE
	// complete so the derivation performed by complete itself must yield
	// "verified".
	if err := internal.AddEvidence(typeAssertDBM(dm), internal.EvidenceInput{
		ArtifactID:         workID,
		ArtifactType:       "work",
		Type:               "test",
		SourceGroup:        "test",
		Strength:           0.9,
		IndependenceFactor: 1.0,
		CreatedBy:          "d3-regression",
	}); err != nil {
		t.Fatalf("AddEvidence: %v", err)
	}

	// Complete — inspect the RESPONSE, not just persisted state.
	res, err = handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "complete",
		"params": map[string]interface{}{"work_id": workID},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	m := res.(map[string]interface{})
	respVerification, _ := m["verification"].(internal.WorkVerification)
	if string(respVerification) != "verified" {
		t.Errorf("complete response verification = %q, want %q (stale pre-derivation payload)",
			string(respVerification), "verified")
	}
	if workStatus(m, "status") != "done" {
		t.Errorf("complete response status = %q, want done", workStatus(m, "status"))
	}

	// Cross-check: response must agree with persisted state.
	showRes, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "show",
		"params": map[string]interface{}{"work_id": workID},
	})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	persisted := showRes.(map[string]interface{})["verification"].(internal.WorkVerification)
	if string(persisted) != string(respVerification) {
		t.Errorf("response/persistence divergence: response=%q persisted=%q",
			string(respVerification), string(persisted))
	}
}

// TestWorkCancel_ResponseReflectsDerivedVerification mirrors the D3 check for
// the cancel path (git audit evidence alone → partial per documented rules;
// with no prior outcome evidence the derivation must be visible in the
// cancel response rather than the stale snapshot).
func TestWorkCancel_ResponseReflectsDerivedVerification(t *testing.T) {
	dm := newTestSharedDM(t)

	res, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "create",
		"params": map[string]interface{}{"title": "D3 cancel probe"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	workID := res.(map[string]interface{})["id"].(string)

	res, err = handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "cancel",
		"params": map[string]interface{}{"work_id": workID},
	})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	m := res.(map[string]interface{})
	if workStatus(m, "status") != "cancelled" {
		t.Errorf("cancel response status = %q, want cancelled", workStatus(m, "status"))
	}
	// Git evidence recorded by cancel itself is audit-only → partial.
	respVerification, _ := m["verification"].(internal.WorkVerification)
	if !strings.EqualFold(string(respVerification), "partial") {
		t.Errorf("cancel response verification = %q, want partial (git-only audit evidence)", string(respVerification))
	}
}
