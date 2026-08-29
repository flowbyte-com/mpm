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
// the cancel path: the response must reflect DeriveWorkVerification's
// current result, not the pre-derivation snapshot.
//
// Note (F8.1 architectural change, commit 55226a2): the original assertion
// expected "partial" because pre-F8.1, the cancel handler auto-recorded git
// audit evidence that promoted verification to "partial". The F8.1 fix
// made the lifecycle gate STRUCTURAL: a cancelled work item's verification
// is locked BELOW "verified" regardless of evidence pattern. Audit-only
// evidence on a cancelled item yields "unverified" (NOT "partial"). A
// challenge evidence row attached before or after cancellation still
// surfaces as "contradicted". See internal/core/db.go:5295-5314.
//
// This test now asserts the F8.1 invariant: the cancel response must
// reflect the locked "unverified" state, and the response must agree
// with persisted state (the D3 invariant the test was designed to defend).
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
	// F8.1: cancellation locks verification below verified. With no
	// evidence attached, the only correct response is "unverified".
	// "partial" is wrong because audit-only evidence (the only kind
	// the cancel handler records) must not promote a cancelled item.
	respVerification, _ := m["verification"].(internal.WorkVerification)
	if !strings.EqualFold(string(respVerification), "unverified") {
		t.Errorf("cancel response verification = %q, want %q (F8.1 lifecycle gate: cancel locks below verified)",
			string(respVerification), "unverified")
	}
	// F8.1 boundary check: the response must never claim "verified" or
	// "partial" on a cancelled item, regardless of evidence pattern.
	if v := strings.ToLower(string(respVerification)); v == "verified" || v == "partial" {
		t.Errorf("cancel response verification = %q violates F8.1 (must be unverified or contradicted for cancelled work)", v)
	}

	// D3 cross-check: response must agree with persisted state.
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

// TestWorkCancel_F81LifecycleGate covers the F8.1 invariant that motivated
// this fix: cancellation must downgrade verification to "unverified" even
// when prior outcome evidence would otherwise yield "verified". The gate
// is structural and cannot be bypassed by a follow-up evidence write.
//
// Regression scenarios:
//   1. Fresh work → cancel → "unverified" (the case above, also covered).
//   2. Verified work → cancel → "unverified" (F8.1 demote on lifecycle change).
//   3. Work + challenge evidence → cancel → "contradicted" (contradiction
//      still surfaces as the most informative verdict, not as "unverified").
//   4. Cancel does not corrupt evidence history (entries remain in the
//      evidence ledger regardless of lifecycle state).
//   5. Lifecycle status is independent of verification state — the cancel
//      response must carry status="cancelled" AND the F8.1-locked
//      verification, never "cancelled" implying a derivation result
//      that contradicts the gate.
func TestWorkCancel_F81LifecycleGate(t *testing.T) {
	t.Run("verified_work_cancel_downgrades_to_unverified", func(t *testing.T) {
		dm := newTestSharedDM(t)

		// Create + verify by attaching outcome evidence + completing.
		res, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "create",
			"params": map[string]interface{}{"title": "F8.1 verified-then-cancel"},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		workID := res.(map[string]interface{})["id"].(string)

		if err := internal.AddEvidence(typeAssertDBM(dm), internal.EvidenceInput{
			ArtifactID:         workID,
			ArtifactType:       "work",
			Type:               "test",
			SourceGroup:        "test",
			Strength:           0.9,
			IndependenceFactor: 1.0,
			CreatedBy:          "f81-downgrade-test",
		}); err != nil {
			t.Fatalf("AddEvidence: %v", err)
		}
		if _, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "complete",
			"params": map[string]interface{}{"work_id": workID},
		}); err != nil {
			t.Fatalf("complete: %v", err)
		}
		// Sanity: complete produces "verified" (regression for the D3 fix).
		showRes, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "show",
			"params": map[string]interface{}{"work_id": workID},
		})
		if err != nil {
			t.Fatalf("show post-complete: %v", err)
		}
		if v := showRes.(map[string]interface{})["verification"].(internal.WorkVerification); !strings.EqualFold(string(v), "verified") {
			t.Fatalf("precondition: expected verified after complete, got %q", string(v))
		}

		// Now cancel a VERIFIED item — F8.1 must downgrade.
		cancelRes, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "cancel",
			"params": map[string]interface{}{"work_id": workID, "note": "F8.1 demote check"},
		})
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		m := cancelRes.(map[string]interface{})
		if workStatus(m, "status") != "cancelled" {
			t.Errorf("status = %q, want cancelled", workStatus(m, "status"))
		}
		respVerification, _ := m["verification"].(internal.WorkVerification)
		if !strings.EqualFold(string(respVerification), "unverified") {
			t.Errorf("F8.1 demote on cancel: verification = %q, want %q (a verified item cancelled must NOT remain verified)",
				string(respVerification), "unverified")
		}
	})

	t.Run("challenged_work_cancel_surfaces_contradicted", func(t *testing.T) {
		dm := newTestSharedDM(t)

		res, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "create",
			"params": map[string]interface{}{"title": "F8.1 challenged-cancel"},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		workID := res.(map[string]interface{})["id"].(string)

		// T20-1: a single challenge row is unsubstantiated dispute, NOT
		// substantiated contradiction. evidenceSetHasContradiction
		// requires corroboration (cumulative strength ≤ -1.0). To make
		// this test express the F8.1 contract — "contradiction surfaces
		// even on cancelled work" — the dispute must be corroborated:
		// attach TWO challenge rows at the default -0.6 strength, totaling
		// -1.2, which crosses negativeCorroborationSum.
		for i := 0; i < 2; i++ {
			if err := internal.AddEvidence(typeAssertDBM(dm), internal.EvidenceInput{
				ArtifactID:         workID,
				ArtifactType:       "work",
				Type:               "challenge",
				SourceGroup:        "test",
				Strength:           -0.6,
				IndependenceFactor: 1.0,
				CreatedBy:          "f81-challenge-test",
			}); err != nil {
				t.Fatalf("AddEvidence(challenge %d): %v", i, err)
			}
		}

		cancelRes, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "cancel",
			"params": map[string]interface{}{"work_id": workID},
		})
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		m := cancelRes.(map[string]interface{})
		if workStatus(m, "status") != "cancelled" {
			t.Errorf("status = %q, want cancelled", workStatus(m, "status"))
		}
		respVerification, _ := m["verification"].(internal.WorkVerification)
		if !strings.EqualFold(string(respVerification), "contradicted") {
			t.Errorf("challenged + cancelled: verification = %q, want %q (corroborated contradiction must surface even on cancelled work)",
				string(respVerification), "contradicted")
		}
	})

	t.Run("cancel_does_not_corrupt_evidence_history", func(t *testing.T) {
		dm := newTestSharedDM(t)

		res, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "create",
			"params": map[string]interface{}{"title": "F8.1 evidence-history"},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		workID := res.(map[string]interface{})["id"].(string)

		// Add two evidence rows, then cancel.
		for _, input := range []internal.EvidenceInput{
			{ArtifactID: workID, ArtifactType: "work", Type: "test", SourceGroup: "test", Strength: 0.9, IndependenceFactor: 1.0, CreatedBy: "history-test"},
			{ArtifactID: workID, ArtifactType: "work", Type: "observation", SourceGroup: "manual_review", Strength: 0.8, IndependenceFactor: 1.0, CreatedBy: "history-test"},
		} {
			if err := internal.AddEvidence(typeAssertDBM(dm), input); err != nil {
				t.Fatalf("AddEvidence: %v", err)
			}
		}

		if _, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "cancel",
			"params": map[string]interface{}{"work_id": workID},
		}); err != nil {
			t.Fatalf("cancel: %v", err)
		}

		// Evidence must still be in the ledger. Cancellation is a
		// lifecycle transition, not an evidence purge.
		evidence, err := internal.ListEvidenceForArtifact(dm, workID, "work")
		if err != nil {
			t.Fatalf("ListEvidenceForArtifact: %v", err)
		}
		if len(evidence) != 2 {
			t.Errorf("evidence count after cancel = %d, want 2 (cancel must not purge history)", len(evidence))
		}
	})

	// T20-1 (alpha-final): single unsubstantiated challenge is DISPUTE,
	// not contradiction. On a cancelled work, the F8.1 lifecycle gate
	// locks verification at "unverified" — the challenge row remains in
	// the audit ledger (visible via ListEvidenceForArtifact) but does
	// NOT promote verification to "contradicted" without corroboration.
	// This pins the alpha-final architecture: corroborated dispute
	// surfaces as contradicted (test above); uncorroborated dispute
	// surfaces as unverified (this test). The same single row would
	// also yield "unverified" on an OPEN work — the corroboration gate
	// is lifecycle-independent.
	t.Run("single_unsubstantiated_challenge_on_cancelled_yields_unverified", func(t *testing.T) {
		dm := newTestSharedDM(t)

		res, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "create",
			"params": map[string]interface{}{"title": "T20-1 single-challenge-cancel"},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		workID := res.(map[string]interface{})["id"].(string)

		// ONE challenge row at default strength — substantiation fails
		// (-0.6 > negativeCorroborationSum of -1.0). This is the case
		// the alpha audit specifically flagged as "unsubstantiated
		// challenge permanently corrupting verified work" — it must NOT
		// promote verification on a cancelled work either.
		if err := internal.AddEvidence(typeAssertDBM(dm), internal.EvidenceInput{
			ArtifactID:         workID,
			ArtifactType:       "work",
			Type:               "challenge",
			SourceGroup:        "test",
			Strength:           -0.6,
			IndependenceFactor: 1.0,
			CreatedBy:          "t201-single-challenge",
		}); err != nil {
			t.Fatalf("AddEvidence(challenge): %v", err)
		}

		cancelRes, err := handleMpmWork(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "cancel",
			"params": map[string]interface{}{"work_id": workID},
		})
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		m := cancelRes.(map[string]interface{})
		if workStatus(m, "status") != "cancelled" {
			t.Errorf("status = %q, want cancelled", workStatus(m, "status"))
		}
		respVerification, _ := m["verification"].(internal.WorkVerification)
		if strings.EqualFold(string(respVerification), "contradicted") {
			t.Errorf("single unsubstantiated challenge on cancelled work: verification = %q, want NOT contradicted (T20-1: substantiation required; one -0.6 challenge alone must not flip cancelled work to contradicted)",
				string(respVerification))
		}
		// Evidence must remain visible — the dispute is recorded as audit
		// history; the lifecycle gate is what blocks the state transition.
		evidence, err := internal.ListEvidenceForArtifact(dm, workID, "work")
		if err != nil {
			t.Fatalf("ListEvidenceForArtifact: %v", err)
		}
		if len(evidence) != 1 {
			t.Errorf("challenge row must persist in evidence ledger after cancel, got count=%d", len(evidence))
		}
	})
}
