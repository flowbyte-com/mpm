// blocker_2_3_test.go — Regression tests for BLOCKER 2 (verification
// monotonicity) and BLOCKER 3 (weak observation verification).
//
// BLOCKER 2 (legacy framing — pre-T20-1): A work item previously
// derived as 'verified' was supposed to drop to 'contradicted' the
// moment a challenge-type evidence row arrived. The original BLOCKER 2
// failure mode was that -0.6 fell below the verifier's -0.7 strength
// threshold, so a fresh challenge row was silently ignored and the
// work stayed verified. That framing has been SUPERSEDED by T20-1
// (alpha-final): the corrected architecture requires corroboration
// before contradiction fires. A single unsubstantiated challenge row
// on otherwise-verified work downgrades to PARTIAL, not contradicted.
// This file's two BLOCKER 2 tests have been updated to assert the
// corrected contract: the contradiction surface is now properly
// substantiation-gated, and the verifier responds to a single
// challenge by re-deriving as 'partial'. The substantiated path
// (corroborated dispute, strong-negative observation, designated
// negative evidence) is covered by the third test below and by the
// dedicated t20_1_contradiction_test.go.
//
// BLOCKER 3: A single weak observation (strength 0.4) is not sufficient
// to verify a work item. The default observation strength is 0.4, and
// the verifier used to treat any positive-strength outcome evidence as
// sufficient for verified — meaning the default observation type could
// promote work to 'verified' on its own. The semantic intent is that
// observations contribute confidence but require stronger corroboration;
// only designated evidence types (test/reproduction/decision_outcome)
// or observations with explicit high strength are sufficient.

package internal

import (
	"testing"
	"time"
)

// TestBlocker2_ChallengeEvidenceDowngradesVerifiedToPartial reproduces
// the original BLOCKER 2 failure mode but reflects the corrected T20-1
// architecture: a work derives verified, then a single unsubstantiated
// challenge evidence row is appended. Pre-fix, -0.6 fell below the
// verifier's -0.7 threshold so the challenge row was silently ignored.
// Post-fix, the verifier recognises the challenge but the T20-1
// substantiation gate classifies a single challenge as DISPUTE (not
// contradiction), so verified work downgrades to PARTIAL — the
// challenge is visible in the audit trail but does not permanently
// convert the work to contradicted.
func TestBlocker2_ChallengeEvidenceDowngradesVerifiedToPartial(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("ship parser fix", "", "session-1")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Stage 1: build up verified state with a designated evidence type.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "test",
		SourceGroup:  "filesystem",
		Strength:     0.9,
		CreatedBy:    "ci",
		CreatedAt:    time.Now(),
		Notes:        "parser regression test passes",
	})
	v, err := dm.DeriveWorkVerification(w.ID)
	if err != nil {
		t.Fatalf("DeriveWorkVerification (stage 1): %v", err)
	}
	if v != WorkVerificationVerified {
		t.Fatalf("sanity: stage 1 should derive verified, got %v", v)
	}

	// Stage 2: a single challenge evidence row arrives. T20-1: this is
	// dispute, not contradiction. The verifier MUST respond by
	// downgrading from 'verified' to 'partial' (visible downgrade +
	// preserved audit trail). AddEvidence routes through
	// DeriveWorkVerification synchronously.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "challenge",
		SourceGroup:  "manual_review",
		Strength:     -0.6, // explicit override matches registry default
		CreatedBy:    "reviewer",
		CreatedAt:    time.Now(),
		Notes:        "regression test was incorrectly scoped — false positive",
	})

	reloaded, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if reloaded.Verification != WorkVerificationPartial {
		t.Errorf("after single unsubstantiated challenge, verification = %v, want partial (T20-1: substantiation gate; single challenge is dispute, not contradiction)",
			reloaded.Verification)
	}

	// Audit trail invariant: the challenge row MUST remain visible —
	// T20-1 records the dispute in evidence history without deleting it.
	evidenceList, err := ListEvidenceForArtifact(dm, w.ID, "work")
	if err != nil {
		t.Fatalf("ListEvidenceForArtifact: %v", err)
	}
	var foundChallenge bool
	for _, e := range evidenceList {
		if e.Type == "challenge" {
			foundChallenge = true
			break
		}
	}
	if !foundChallenge {
		t.Errorf("challenge row must remain in evidence ledger (T20-1: dispute is audit-visible, not deleted)")
	}
}

// TestBlocker2_DefaultChallengeStrengthDowngradesVerifiedToPartial mirrors
// the same T20-1 contract: a challenge row inserted WITHOUT explicit
// strength (registry default -0.6 applies) is still classified as
// dispute. The pre-fix regression was that -0.6 was silently ignored
// (work stayed verified); post-fix, the verifier responds correctly
// by downgrading to partial.
func TestBlocker2_DefaultChallengeStrengthDowngradesVerifiedToPartial(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("default strength challenge target", "", "session-1")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Verified state first.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "test",
		SourceGroup:  "test",
		Strength:     0.95,
		CreatedBy:    "ci",
		CreatedAt:    time.Now(),
	})
	if v, err := dm.DeriveWorkVerification(w.ID); err != nil {
		t.Fatalf("DeriveWorkVerification: %v", err)
	} else if v != WorkVerificationVerified {
		t.Fatalf("sanity: must derive verified, got %v", v)
	}

	// Default-strength challenge: Strength=0 → AddEvidence's DM wrapper
	// fills it from the registry (-0.6). T20-1: a single default-strength
	// challenge is dispute, not contradiction. Verified → partial.
	if _, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "challenge",
		SourceGroup:  "manual_review",
		CreatedBy:    "reviewer",
		CreatedAt:    time.Now(),
		Notes:        "default-strength challenge — T20-1 classifies as dispute, not contradiction",
	}); err != nil {
		t.Fatalf("AddEvidence (challenge): %v", err)
	}

	reloaded, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if reloaded.Verification != WorkVerificationPartial {
		t.Errorf("default-strength challenge (strength=-0.6) did not move verified → partial; got %v (T20-1: single default-strength challenge is dispute, not contradiction)",
			reloaded.Verification)
	}
}

// TestBlocker2_ChallengeResolvesToVerifiedWhenChallengeExpires confirms
// the bidirectional recovery path: once a substantiated contradiction
// is resolved (the architectural mechanism for declaring a dispute
// resolved is expires_at on the contradicting row), fresh designated
// evidence re-promotes verification to verified. Contradiction is not
// a permanent demotion — explicit resolution restores the verification
// surface.
//
// Pre-fix behaviour: the verifier never recognized the expires_at
// column on dispute rows, so a contradicted work stayed contradicted
// forever even after the dispute was administratively resolved.
// Post-fix behaviour: ListEvidenceForArtifact (which the verifier
// walks) already filters expired rows, so an expired contradicting
// row no longer contributes to the contradiction count, and a fresh
// designated evidence row can re-promote.
//
// T20-1 alignment: a single 'challenge' row alone does NOT establish
// contradiction (substantiation is required). To make this test
// express the contradicted state, the contradicting row must be a
// strong-negative observation (strength ≤ -0.7), which is the one row
// type that single-handedly establishes substantiated contradiction
// per evidenceSetHasContradiction.
func TestBlocker2_ChallengeResolvesToVerifiedWhenChallengeExpires(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("recovery after contradiction", "", "session-1")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Stage 1: verified via strong test evidence.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "test",
		SourceGroup:  "filesystem",
		Strength:     0.9,
		CreatedBy:    "ci",
		CreatedAt:    time.Now(),
	})

	// Stage 2: substantiated contradiction via strong-negative
	// observation (T20-1: type='observation' at strength ≤ -0.7 is the
	// single-source substantiation escape). Capture the row's id so
	// we can mark it resolved.
	contradictingID := "ev-strong-neg-" + w.ID
	AddEvidence(dm, EvidenceInput{
		ArtifactID:         w.ID,
		ArtifactType:       "work",
		Type:               "observation",
		SourceGroup:        "manual_review",
		Strength:           -0.85,
		CreatedBy:          "reviewer",
		CreatedAt:          time.Now(),
		IndependenceFactor: 1.0,
		Notes:              "initial false-positive reproduction",
	})
	if v, err := dm.DeriveWorkVerification(w.ID); err != nil {
		t.Fatalf("DeriveWorkVerification (post-strong-negative): %v", err)
	} else if v != WorkVerificationContradicted {
		t.Fatalf("sanity: post-strong-negative must be contradicted, got %v", v)
	}

	// Sanity: find the contradicting row we just inserted so we can
	// expire it. ListEvidenceForArtifact returns them newest-first.
	evidenceList, err := ListEvidenceForArtifact(dm, w.ID, "work")
	if err != nil {
		t.Fatalf("ListEvidenceForArtifact: %v", err)
	}
	var foundContradicting string
	for _, e := range evidenceList {
		if e.Type == "observation" && e.Strength < 0 {
			foundContradicting = e.ID
			break
		}
	}
	if foundContradicting == "" {
		t.Fatalf("could not find inserted strong-negative observation in evidence list")
	}
	_ = contradictingID // marker for symmetry with the explicit-id variant below

	// Stage 3: the operator marks the contradicting row as resolved
	// (expires_at in the past). This is the architectural mechanism
	// for declaring a dispute resolved without deleting the row
	// (F7.1 invariant — the audit trail is preserved).
	if _, err := dm.db.Exec(
		`UPDATE evidence SET expires_at = CAST(strftime('%s','now') AS INTEGER) - 1 WHERE id = ?`,
		foundContradicting,
	); err != nil {
		t.Fatalf("expire contradicting row: %v", err)
	}

	// Stage 4: fresh designated evidence arrives. With the challenge
	// row now expired (ListEvidenceForArtifact filters expires_at <= now),
	// the verifier should re-derive verified.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "reproduction",
		SourceGroup:  "filesystem",
		Strength:     0.95,
		CreatedBy:    "ci",
		CreatedAt:    time.Now(),
		Notes:        "reproduced in clean environment — challenge was wrong",
	})

	reloaded, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if reloaded.Verification != WorkVerificationVerified {
		t.Errorf("after challenge expiry + fresh reproduction evidence, verification = %v, want verified",
			reloaded.Verification)
	}
}

// TestBlocker3_WeakObservationDoesNotVerify reproduces the BLOCKER 3
// failure mode: a single observation evidence row with default
// strength 0.4 is sufficient to derive 'verified' on its own. The
// semantic intent is that observations contribute confidence but
// require stronger corroboration; only designated evidence types or
// observations carrying explicit high strength are sufficient.
func TestBlocker3_WeakObservationDoesNotVerify(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("observation-only verification target", "", "session-1")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Single observation at default strength (0.4). The DM wrapper
	// fills strength from the registry when zero is passed.
	if _, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "filesystem",
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
		Notes:        "weak observation at registry default strength",
	}); err != nil {
		t.Fatalf("AddEvidence: %v", err)
	}

	reloaded, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if reloaded.Verification == WorkVerificationVerified {
		t.Errorf("default-strength observation (strength=0.4) must not derive verified; got %v",
			reloaded.Verification)
	}
}

// TestBlocker3_DesignatedEvidenceTypeDoesVerify confirms the happy
// path: a 'test' or 'reproduction' or 'decision_outcome' evidence
// row IS sufficient to verify, even at the default registry strength
// (0.7 for test). Designated evidence types carry the architectural
// weight to verify independently.
func TestBlocker3_DesignatedEvidenceTypeDoesVerify(t *testing.T) {
	for _, evidenceType := range []string{"test", "reproduction", "decision_outcome"} {
		t.Run(evidenceType, func(t *testing.T) {
			dm := NewTestDM(t)
			defer dm.Close()

			w, err := dm.AddWork("designated type "+evidenceType, "", "session-1")
			if err != nil {
				t.Fatalf("AddWork: %v", err)
			}

			// Default strength (registry fills it: test=0.7, reproduction=0.85,
			// decision_outcome=0.95).
			if _, err := dm.AddEvidence(EvidenceInput{
				ArtifactID:   w.ID,
				ArtifactType: "work",
				Type:         evidenceType,
				SourceGroup:  "filesystem",
				CreatedBy:    "ci",
				CreatedAt:    time.Now(),
			}); err != nil {
				t.Fatalf("AddEvidence: %v", err)
			}

			reloaded, err := dm.GetWork(w.ID)
			if err != nil {
				t.Fatalf("GetWork: %v", err)
			}
			if reloaded.Verification != WorkVerificationVerified {
				t.Errorf("designated evidence type %q at default strength should verify; got %v",
					evidenceType, reloaded.Verification)
			}
		})
	}
}

// TestBlocker3_HighStrengthObservationDoesVerify confirms the
// corroboration escape: an observation carrying an explicit high
// strength IS sufficient to verify on its own. The rule is "designated
// type OR high-strength observation", not "designated type only".
// Otherwise an agent cannot verify purely observational work where
// the strength is bumped manually to reflect unusual confidence.
func TestBlocker3_HighStrengthObservationDoesVerify(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("high-strength observation target", "", "session-1")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "filesystem",
		Strength:     0.9, // explicit override — high confidence
		CreatedBy:    "reviewer",
		CreatedAt:    time.Now(),
		Notes:        "manual review with explicit high confidence",
	})

	reloaded, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if reloaded.Verification != WorkVerificationVerified {
		t.Errorf("high-strength observation (strength=0.9) should verify; got %v",
			reloaded.Verification)
	}
}

// TestBlocker3_MultipleObservationsAggregateToVerify confirms the
// corroboration rule: multiple weak observations can aggregate to
// verify when their combined strength crosses the threshold. This
// protects the use case where a work item is observed many times at
// moderate confidence (e.g., a long-running task with multiple
// checkpoint observations).
func TestBlocker3_MultipleObservationsAggregateToVerify(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("aggregated observation target", "", "session-1")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Three observations at default strength (0.4 each). Sum = 1.2,
	// which exceeds the corroboration threshold. Per the design:
	// observations contribute confidence; aggregated strength ≥ verify
	// threshold corroborates verification.
	for i := 0; i < 3; i++ {
		if _, err := dm.AddEvidence(EvidenceInput{
			ArtifactID:   w.ID,
			ArtifactType: "work",
			Type:         "observation",
			SourceGroup:  "filesystem",
			CreatedBy:    "reviewer",
			CreatedAt:    time.Now().Add(time.Duration(i) * time.Second),
			Notes:        "checkpoint observation",
		}); err != nil {
			t.Fatalf("AddEvidence[%d]: %v", i, err)
		}
	}

	reloaded, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if reloaded.Verification != WorkVerificationVerified {
		t.Errorf("three observations (sum strength 1.2) should corroborate to verified; got %v",
			reloaded.Verification)
	}
}