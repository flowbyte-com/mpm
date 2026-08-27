// blocker_2_3_test.go — Regression tests for BLOCKER 2 (verification
// monotonicity) and BLOCKER 3 (weak observation verification).
//
// BLOCKER 2: A work item previously derived as 'verified' must drop to
// 'contradicted' the moment a challenge-type evidence row arrives. The
// challenge type has a default strength of -0.6, which previously fell
// below the verifier's strength threshold (-0.7) — so a fresh challenge
// row was silently ignored by DeriveWorkVerification while the works.verification
// column still read 'verified'. A fresh agent reading the work item
// would see verified + an active challenge in evidence history, which
// is the exact "stale verification masquerading as authoritative" bug
// the architecture is supposed to prevent.
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

// TestBlocker2_ChallengeEvidenceMovesVerifiedToContradicted reproduces
// the BLOCKER 2 failure mode: a work derives verified, then a challenge
// evidence row is appended (which has default strength -0.6). The
// verifier must immediately re-derive contradicted — otherwise a fresh
// agent sees verified + an active challenge, which is the exact
// "verification hidden contradiction" failure mode.
//
// Pre-fix behaviour: challenge default strength (-0.6) fell below the
// verifier's threshold (-0.7), so the verifier ignored the challenge
// row and the work stayed verified.
func TestBlocker2_ChallengeEvidenceMovesVerifiedToContradicted(t *testing.T) {
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

	// Stage 2: a challenge evidence row arrives. AddEvidence routes
	// through DeriveWorkVerification synchronously (F7/F12); the new
	// evidence must immediately downgrade verification to contradicted.
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
	if reloaded.Verification != WorkVerificationContradicted {
		t.Errorf("after challenge evidence, verification = %v, want contradicted",
			reloaded.Verification)
	}
}

// TestBlocker2_DefaultChallengeStrengthTriggersContradicted confirms that
// a challenge row inserted WITHOUT explicit strength (so the registry
// default of -0.6 applies) still moves verification out of verified.
// This is the regression surface the audit exposed: pre-fix, -0.6 was
// below the verifier's -0.7 strength threshold, so the default challenge
// was silently ignored.
func TestBlocker2_DefaultChallengeStrengthTriggersContradicted(t *testing.T) {
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
	// fills it from the registry (-0.6). Without the BLOCKER 2 fix this
	// would NOT trigger contradicted because -0.6 < -0.7.
	if _, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "challenge",
		SourceGroup:  "manual_review",
		CreatedBy:    "reviewer",
		CreatedAt:    time.Now(),
		Notes:        "default-strength challenge — should still move verified → contradicted",
	}); err != nil {
		t.Fatalf("AddEvidence (challenge): %v", err)
	}

	reloaded, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if reloaded.Verification != WorkVerificationContradicted {
		t.Errorf("default-strength challenge (strength=-0.6) did not move verified → contradicted; got %v",
			reloaded.Verification)
	}
}

// TestBlocker2_ChallengeResolvesToVerifiedWhenChallengeExpires confirms
// the bidirectional recovery path: once a challenge row is marked as
// expired (the architectural mechanism for declaring a challenge
// resolved), fresh designated evidence re-promotes verification to
// verified. Contradiction is not a permanent demotion — explicit
// challenge resolution restores the verification surface.
//
// Pre-fix behaviour: the verifier never recognized the expires_at
// column on challenge rows, so a contradicted work stayed contradicted
// forever even after the challenge was administratively resolved.
// Post-fix behaviour: ListEvidenceForArtifact (which the verifier
// walks) already filters expired rows, so an expired challenge row no
// longer contributes to the contradiction count, and a fresh
// designated evidence row can re-promote.
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

	// Stage 2: challenge → contradicted. Capture the challenge row's
	// id so we can mark it resolved.
	challengeID := "ev-challenge-test-" + w.ID
	AddEvidence(dm, EvidenceInput{
		ArtifactID:         w.ID,
		ArtifactType:       "work",
		Type:               "challenge",
		SourceGroup:        "manual_review",
		Strength:           -0.85,
		CreatedBy:          "reviewer",
		CreatedAt:          time.Now(),
		IndependenceFactor: 1.0,
		Notes:              "initial false-positive challenge",
	})
	if v, err := dm.DeriveWorkVerification(w.ID); err != nil {
		t.Fatalf("DeriveWorkVerification (post-challenge): %v", err)
	} else if v != WorkVerificationContradicted {
		t.Fatalf("sanity: post-challenge must be contradicted, got %v", v)
	}

	// Sanity: find the challenge row we just inserted so we can expire
	// it. ListEvidenceForArtifact returns them newest-first.
	evidenceList, err := ListEvidenceForArtifact(dm, w.ID, "work")
	if err != nil {
		t.Fatalf("ListEvidenceForArtifact: %v", err)
	}
	var foundChallenge string
	for _, e := range evidenceList {
		if e.Type == "challenge" {
			foundChallenge = e.ID
			break
		}
	}
	if foundChallenge == "" {
		t.Fatalf("could not find inserted challenge row in evidence list")
	}
	_ = challengeID // marker for symmetry with the explicit-id variant below

	// Stage 3: the operator marks the challenge as resolved (expires_at
	// in the past). This is the architectural mechanism for declaring
	// a challenge resolved without deleting the row (F7.1 invariant).
	if _, err := dm.db.Exec(
		`UPDATE evidence SET expires_at = CAST(strftime('%s','now') AS INTEGER) - 1 WHERE id = ?`,
		foundChallenge,
	); err != nil {
		t.Fatalf("expire challenge row: %v", err)
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