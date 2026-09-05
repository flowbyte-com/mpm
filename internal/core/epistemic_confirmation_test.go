// epistemic_confirmation_test.go — Regression tests for the
// LogChangelogEntryWithConfirmations flow (docs/epistemic-confirmation.md).
//
// Three properties pin the contract:
//
//	(a) a confirming event measurably raises confidence on the target
//	    artifact (proves the in-tx evidence + RecomputeConfidence path
//	    actually moves the artifact's confidence column upward)
//
//	(b) confirmation does NOT accidentally cross-fire with the
//	    invalidation-cascade path (the assertion in
//	    docs/epistemic-confirmation.md that "positive-strength evidence
//	    on a confidence=0.7-default lesson can never drop below the
//	    cascade threshold"; this test pins it from both sides — the
//	    epistemic_cascade_outbox stays empty after a confirmation, and
//	    the RecomputeConfidence hard-confidence hook does not enqueue)
//
//	(c) confirmation requires an explicit, non-inferred assertion —
//	    naming a lesson in confirms_lesson_id must work; not naming it
//	    must NOT, and a keyword-only match (e.g. a commit message that
//	    happens to mention "fts5") must not silently trigger a
//	    confirmation on the FTS5 lesson
//
// None of these tests should pass by string-matching commit message
// content to lesson body content. The relationship is asserted
// explicitly by the caller.
package internal

import (
	"strings"
	"testing"
)

// TestEpistemicConfirmation_RaisesConfidence proves (a): a confirming
// event measurably raises confidence on a target artifact.
//
// Baseline: a fresh lesson starts at confidence 0.7 (per
// InitialConfidence("lesson") at confidence.go:24). After
// LogChangelogEntryWithConfirmations fires an evidence row of type
// "reproduction" (default strength 0.85) against the lesson, the
// confidence must rise above 0.7.
//
// The "rise" threshold is intentionally >0.7 (not ==0.85) because
// the recompute formula applies per-artifact-type decay anchor and
// the existing default evidence set may already include a
// positive-evidence anchor. The strict assertion is "confidence >
// 0.7" — the value before any confirmation. The weaker "confidence
// after >= confidence before" is the lower bound that proves the
// monotonicity the design depends on.
func TestEpistemicConfirmation_RaisesConfidence(t *testing.T) {
	dm := NewTestDM(t)

	// Seed a lesson.
	const probeContent = "epistemic-confirmation probe lesson"
	if _, _, err := dm.SaveLesson(probeContent, "insight", []string{"epistemic-confirmation-probe"}); err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	listed, err := dm.ListLessons("")
	if err != nil {
		t.Fatalf("list lessons: %v", err)
	}
	var foundID string
	for _, l := range listed {
		if l.Content == probeContent {
			foundID = l.ID
			break
		}
	}
	if foundID == "" {
		t.Fatalf("seeded lesson not found in list")
	}

	// Baseline confidence.
	var before float64
	if err := dm.QueryRowTracked(`SELECT confidence FROM lessons WHERE id = ?`, foundID).Scan(&before); err != nil {
		t.Fatalf("read baseline confidence: %v", err)
	}
	if before != 0.7 {
		t.Fatalf("baseline confidence = %f, want 0.7 (InitialConfidence('lesson'))", before)
	}

	// Fire the confirmation.
	const fakeCommit = "0123456789abcdef0123456789abcdef01234567"
	if _, err := dm.LogChangelogEntryWithConfirmations(
		"epistemic-confirmation probe commit",
		fakeCommit,
		nil,
		[]ConfirmationSpec{{ArtifactID: foundID, ArtifactType: "lesson"}},
	); err != nil {
		t.Fatalf("LogChangelogEntryWithConfirmations: %v", err)
	}

	// Read confidence back.
	var after float64
	if err := dm.QueryRowTracked(`SELECT confidence FROM lessons WHERE id = ?`, foundID).Scan(&after); err != nil {
		t.Fatalf("read post-confirmation confidence: %v", err)
	}
	if !(after > before) {
		t.Errorf("confidence did not rise: before=%f after=%f (the +0.85 reproduction row + recompute should measurably raise a 0.7-default lesson)", before, after)
	}

	// Sanity: confidence_history row exists with trigger='evidence_added'.
	var histCount int
	if err := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ? AND artifact_type = 'lesson' AND trigger = 'evidence_added'`,
		foundID,
	).Scan(&histCount); err != nil {
		t.Fatalf("count history rows: %v", err)
	}
	if histCount == 0 {
		t.Errorf("confidence_history missing evidence_added row for the confirmed lesson")
	}
}

// TestEpistemicConfirmation_DoesNotTriggerCascade proves (b):
// confirmation must not enqueue any cascade invalidation intent.
// docs/epistemic-confirmation.md explicitly states this is the
// expected behavior (positive-strength evidence on a 0.7-default
// lesson cannot cross the 0.3 cascade threshold), and this test pins
// it from the storage side — the cascade outbox must stay empty.
func TestEpistemicConfirmation_DoesNotTriggerCascade(t *testing.T) {
	dm := NewTestDM(t)

	const probeContent = "epistemic-confirmation no-cascade probe"
	if _, _, err := dm.SaveLesson(probeContent, "insight", []string{"epistemic-confirmation-no-cascade"}); err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	listed, err := dm.ListLessons("")
	if err != nil {
		t.Fatalf("list lessons: %v", err)
	}
	var foundID string
	for _, l := range listed {
		if l.Content == probeContent {
			foundID = l.ID
			break
		}
	}
	if foundID == "" {
		t.Fatalf("seeded lesson not found in list")
	}

	const fakeCommit = "fedcba9876543210fedcba9876543210fedcba98"
	if _, err := dm.LogChangelogEntryWithConfirmations(
		"no-cascade probe",
		fakeCommit,
		nil,
		[]ConfirmationSpec{{ArtifactID: foundID, ArtifactType: "lesson"}},
	); err != nil {
		t.Fatalf("LogChangelogEntryWithConfirmations: %v", err)
	}

	// Cascade outbox must be empty. The cascade system only fires on
	// threshold crossings; confirmation cannot cross that floor.
	var cascadeCount int
	if err := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE dead_artifact_id = ? AND dead_artifact_type = 'lesson'`,
		foundID,
	).Scan(&cascadeCount); err != nil {
		t.Fatalf("count cascade outbox rows: %v", err)
	}
	if cascadeCount != 0 {
		t.Errorf("cascade outbox has %d rows for the confirmed lesson; expected 0 (confirmation cannot cross the cascade threshold)", cascadeCount)
	}

	// Sanity: confidence must still be above 0.3 (the cascade floor)
	// — the entire reason confirmation cannot trigger cascade.
	var conf float64
	if err := dm.QueryRowTracked(`SELECT confidence FROM lessons WHERE id = ?`, foundID).Scan(&conf); err != nil {
		t.Fatalf("read confidence: %v", err)
	}
	if conf < HardConfidenceInvalidationThreshold {
		t.Errorf("confidence dropped below cascade threshold: conf=%f threshold=%f (confirmation must keep confidence >= %f)",
			conf, HardConfidenceInvalidationThreshold, HardConfidenceInvalidationThreshold)
	}
}

// TestEpistemicConfirmation_RequiresExplicitAssertion proves (c):
// the relationship is asserted by the caller, not inferred. A commit
// whose fact text happens to contain "fts5" must NOT auto-confirm
// the FTS5-flag lesson. The only path to a confirmation row is
// confirms_lesson_id=<id>.
//
// The negative case (no confirms_lesson_id) is the existing
// LogChangelogEntry path — it must still produce a changelog memory
// and NO evidence row, even when the fact text mentions every
// lesson in the system.
func TestEpistemicConfirmation_RequiresExplicitAssertion(t *testing.T) {
	dm := NewTestDM(t)

	const probeContent = "epistemic-confirmation explicit-only probe"
	if _, _, err := dm.SaveLesson(probeContent, "insight", []string{"epistemic-confirmation-explicit-only"}); err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	listed, err := dm.ListLessons("")
	if err != nil {
		t.Fatalf("list lessons: %v", err)
	}
	var foundID string
	for _, l := range listed {
		if l.Content == probeContent {
			foundID = l.ID
			break
		}
	}
	if foundID == "" {
		t.Fatalf("seeded lesson not found in list")
	}

	// Baseline confidence (so we can prove the no-confirmation path
	// doesn't move it).
	var before float64
	if err := dm.QueryRowTracked(`SELECT confidence FROM lessons WHERE id = ?`, foundID).Scan(&before); err != nil {
		t.Fatalf("read baseline confidence: %v", err)
	}

	// Negative case: log a changelog that mentions every relevant
	// keyword, but assert NO confirmations. The existing
	// LogChangelogEntry path must be taken (no confirmation
	// evidence row should land). This is the regression case: a
	// caller who didn't know to pass confirms_lesson_id must NOT
	// get a silent auto-reinforcement.
	const fakeCommit = "1111222233334444555566667777888899990000"
	matchingFact := "epistemic-confirmation explicit-only probe fts5 sqlite " + foundID + " reproduction decision theory"
	if _, err := dm.LogChangelogEntry(matchingFact, fakeCommit, nil); err != nil {
		t.Fatalf("LogChangelogEntry (no confirmations): %v", err)
	}

	// Confidence must NOT have changed (no evidence was added).
	var afterNoConfirm float64
	if err := dm.QueryRowTracked(`SELECT confidence FROM lessons WHERE id = ?`, foundID).Scan(&afterNoConfirm); err != nil {
		t.Fatalf("read post-no-confirm confidence: %v", err)
	}
	if afterNoConfirm != before {
		t.Errorf("confidence changed without explicit assertion: before=%f after=%f (keyword-matching must NOT trigger confirmation)",
			before, afterNoConfirm)
	}

	// Evidence row count for this lesson must still be zero
	// (LogChangelogEntry alone never wrote evidence).
	var evCount int
	if err := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND artifact_type = 'lesson'`,
		foundID,
	).Scan(&evCount); err != nil {
		t.Fatalf("count evidence rows: %v", err)
	}
	if evCount != 0 {
		t.Errorf("found %d evidence rows for the lesson without explicit confirmation assertion", evCount)
	}

	// Sanity: the second call WITH explicit assertion DOES produce
	// one. This proves the negative-case silence isn't a broken
	// confirmation path — it's the explicit-only contract.
	const fakeCommit2 = "9999000088887777666655554444333322221111"
	if _, err := dm.LogChangelogEntryWithConfirmations(
		matchingFact,
		fakeCommit2,
		nil,
		[]ConfirmationSpec{{ArtifactID: foundID, ArtifactType: "lesson"}},
	); err != nil {
		t.Fatalf("LogChangelogEntryWithConfirmations (explicit): %v", err)
	}
	var evCount2 int
	if err := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND artifact_type = 'lesson'`,
		foundID,
	).Scan(&evCount2); err != nil {
		t.Fatalf("count evidence rows after explicit: %v", err)
	}
	if evCount2 != 1 {
		t.Errorf("explicit assertion produced %d evidence rows, want 1", evCount2)
	}
}

// TestEpistemicConfirmation_AtomicityRollback proves that a
// confirmation targeting a non-existent artifact rolls back the
// entire transaction (changelog memory AND any prior confirmations
// in the same call). Per docs/epistemic-confirmation.md §"Failure
// handling", invalid input rolls back the entire transaction; this
// test pins atomicity from the storage side.
func TestEpistemicConfirmation_AtomicityRollback(t *testing.T) {
	dm := NewTestDM(t)

	// Seed a real lesson so the FIRST confirmation would succeed.
	const realProbeContent = "epistemic-confirmation atomic real probe"
	if _, _, err := dm.SaveLesson(realProbeContent, "insight", []string{"epistemic-confirmation-atomic-real"}); err != nil {
		t.Fatalf("seed real lesson: %v", err)
	}
	listed, err := dm.ListLessons("")
	if err != nil {
		t.Fatalf("list lessons: %v", err)
	}
	var foundReal string
	for _, l := range listed {
		if l.Content == realProbeContent {
			foundReal = l.ID
			break
		}
	}
	if foundReal == "" {
		t.Fatalf("real lesson not found")
	}

	// Baseline state: no evidence on the real lesson.
	var evBefore int
	if err := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND artifact_type = 'lesson'`,
		foundReal,
	).Scan(&evBefore); err != nil {
		t.Fatalf("count pre-confirm evidence: %v", err)
	}
	if evBefore != 0 {
		t.Fatalf("baseline evidence count = %d, want 0", evBefore)
	}

	// Mixed confirmation batch: first target is real, second target
	// is non-existent. The second must roll back the first AND the
	// changelog memory.
	const fakeCommit = "aaaabbbbccccddddeeeeffffaaaabbbbccccdddd"
	_, err = dm.LogChangelogEntryWithConfirmations(
		"atomicity probe commit",
		fakeCommit,
		nil,
		[]ConfirmationSpec{
			{ArtifactID: foundReal, ArtifactType: "lesson"},
			{ArtifactID: "lesson-does-not-exist", ArtifactType: "lesson"},
		},
	)
	if err == nil {
		t.Fatalf("expected error for non-existent confirmation target; got nil")
	}
	if !strings.Contains(err.Error(), "lesson-does-not-exist") {
		t.Errorf("error %q does not name the bad artifact id", err)
	}

	// Atomicity: no changelog memory, no evidence rows for the
	// real lesson. The entire transaction rolled back.
	var memCount int
	if err := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM memories WHERE content LIKE ?`,
		"%"+fakeCommit+"%",
	).Scan(&memCount); err != nil {
		t.Fatalf("count changelog memories: %v", err)
	}
	if memCount != 0 {
		t.Errorf("changelog memory landed despite rollback: %d rows", memCount)
	}

	var evAfter int
	if err := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND artifact_type = 'lesson'`,
		foundReal,
	).Scan(&evAfter); err != nil {
		t.Fatalf("count post-rollback evidence: %v", err)
	}
	if evAfter != 0 {
		t.Errorf("evidence row landed despite rollback: %d rows for the real lesson", evAfter)
	}
}