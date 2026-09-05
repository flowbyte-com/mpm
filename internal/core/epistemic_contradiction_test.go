// epistemic_contradiction_test.go — Regression tests for the
// negative-direction counterpart of LogChangelogEntryWithConfirmations.
//
// Per docs/epistemic-confirmation.md §"Cascade cross-fire asymmetry",
// the confirmation path and the contradiction path are deliberately
// NOT symmetric at the cascade-invalidation hook:
//
//   - Confirmation: positive evidence (default +0.85) on a 0.7-default
//     lesson cannot drop confidence below the 0.3 cascade floor, so
//     the cascade hook is unreachable from this path.
//
//   - Contradiction: negative evidence (default -0.6) lowers confidence
//     toward the 0.3 floor; a strong contradiction (multiple challenge
//     rows, or an override-strength challenge) can cross the floor and
//     legitimately enqueue cascade intents for downstream dependents.
//     The cascade hook is reachable from this path by design.
//
// Five tests pin the contract for the negative direction. The first,
// third, and fifth mirror the confirmation suite. The second and
// fourth are deliberately different — they prove the cascade asymmetry
// works correctly in both directions (fires on crossing, doesn't fire
// when staying above threshold).
package internal

import (
	"strings"
	"testing"
)

// TestEpistemicContradiction_LowersConfidence mirrors the confirmation
// suite's (a) test for the negative direction: a contradicting event
// measurably lowers confidence on the target artifact.
//
// Baseline: a fresh lesson starts at confidence 0.7 (per
// InitialConfidence("lesson") at confidence.go:24). After
// LogChangelogEntryWithAssertions fires an evidence row of type
// "challenge" (default strength -0.6) against the lesson, the
// confidence must drop below 0.7.
func TestEpistemicContradiction_LowersConfidence(t *testing.T) {
	dm := NewTestDM(t)

	const probeContent = "epistemic-contradiction probe lesson"
	if _, _, err := dm.SaveLesson(probeContent, "insight", []string{"epistemic-contradiction-probe"}); err != nil {
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

	// Fire the contradiction.
	const fakeCommit = "33334444555566667777888899990000aaaabbbb"
	if _, err := dm.LogChangelogEntryWithAssertions(
		"epistemic-contradiction probe commit",
		fakeCommit,
		nil,
		nil,
		[]ContradictionSpec{{ArtifactID: foundID, ArtifactType: "lesson"}},
	); err != nil {
		t.Fatalf("LogChangelogEntryWithAssertions: %v", err)
	}

	// Read confidence back.
	var after float64
	if err := dm.QueryRowTracked(`SELECT confidence FROM lessons WHERE id = ?`, foundID).Scan(&after); err != nil {
		t.Fatalf("read post-contradiction confidence: %v", err)
	}
	if !(after < before) {
		t.Errorf("confidence did not drop: before=%f after=%f (the -0.6 challenge row + recompute should measurably lower a 0.7-default lesson)", before, after)
	}
	// Sanity: confidence must remain above the cascade floor
	// (a single default-strength challenge at -0.6 drops 0.7 → ~0.56,
	// which is well above 0.3). This sanity check pins that the
	// normal-strength contradiction doesn't accidentally cascade.
	if after < HardConfidenceInvalidationThreshold {
		t.Errorf("single contradiction dropped confidence below cascade floor: after=%f threshold=%f (a single default-strength challenge must NOT cascade)",
			after, HardConfidenceInvalidationThreshold)
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
		t.Errorf("confidence_history missing evidence_added row for the contradicted lesson")
	}
}

// TestEpistemicContradiction_TriggersCascadeWhenCrossingThreshold is
// the deliberate opposite of confirmation's cascade test. It
// constructs a case where contradiction DOES push confidence below
// 0.3 and confirms the cascade outbox gets a row.
//
// The realistic shape: a single contradiction at default strength
// (-0.6) drops 0.7 → ~0.56 (above 0.3, no cascade). Three
// contradictions at default strength cumulatively drop
// log-odds by -1.8, which crosses the cascade floor and legitimately
// enqueues cascade intents. This is the intended behavior — the
// contradiction path does NOT suppress cascade firing, because doing
// so would silently hide the user's intent when they assert "this
// artifact is wrong."
//
// The cascade outbox only gets a row when the dead artifact has at
// least one downstream dependent (a decision or theory citing it
// via `dependencies` JSON or `epistemic_provenance`). We seed a
// dependent theory here so the cascade machinery actually has a
// target to enqueue an intent for.
func TestEpistemicContradiction_TriggersCascadeWhenCrossingThreshold(t *testing.T) {
	dm := NewTestDM(t)

	const probeContent = "epistemic-contradiction cascades probe"
	if _, _, err := dm.SaveLesson(probeContent, "insight", []string{"epistemic-contradiction-cascades"}); err != nil {
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

	// Seed a downstream theory that cites the lesson via
	// `dependencies`. Without a downstream target, the cascade
	// machinery correctly writes zero outbox rows — see
	// docs/archive/epistemic-cascades.md §"Dependency discovery".
	// We need at least one dependent so the cascade enqueue has
	// somewhere to land.
	const theoryContent = "theory depending on the contradicted lesson"
	_, err = dm.ProposeTheory(
		theoryContent,
		"if the contradicted lesson is invalidated, this theory needs re-evaluation",
		[]string{foundID},
		nil, // no source_ids
		[]string{"epistemic-contradiction-cascades"},
	)
	if err != nil {
		t.Fatalf("seed dependent theory: %v", err)
	}

	// Fire three contradictions of the same artifact in one call.
	// Each adds -0.6 to log-odds; cumulatively -1.8, dropping
	// confidence well below the 0.3 floor.
	const fakeCommit = "ccccdddd33334444555566667777888899990000"
	if _, err := dm.LogChangelogEntryWithAssertions(
		"epistemic-contradiction cascades probe commit",
		fakeCommit,
		nil,
		nil,
		[]ContradictionSpec{
			{ArtifactID: foundID, ArtifactType: "lesson"},
			{ArtifactID: foundID, ArtifactType: "lesson"},
			{ArtifactID: foundID, ArtifactType: "lesson"},
		},
	); err != nil {
		t.Fatalf("LogChangelogEntryWithAssertions: %v", err)
	}

	// Cascade outbox MUST have a row for the contradicted lesson
	// pointing at the dependent theory as downstream. The crossing
	// event (oldConf >= 0.3 AND newConf < 0.3) fires on the 3rd
	// challenge per evidence_store.go:328-340, and the discovery
	// step finds the dependent theory above.
	var cascadeCount int
	if err := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE dead_artifact_id = ? AND dead_artifact_type = 'lesson' AND reason = 'confidence_floor'`,
		foundID,
	).Scan(&cascadeCount); err != nil {
		t.Fatalf("count cascade outbox rows: %v", err)
	}
	if cascadeCount == 0 {
		t.Errorf("cascade outbox has 0 rows for the contradicted lesson; expected ≥1 (the contradiction crossed the 0.3 threshold and the dependent theory should have been enqueued per evidence_store.go:328-340 + cascade_outbox.go:EnqueueCascadeIntents)")
	}

	// Sanity: confidence MUST be below 0.3 — the entire reason the
	// cascade fired.
	var conf float64
	if err := dm.QueryRowTracked(`SELECT confidence FROM lessons WHERE id = ?`, foundID).Scan(&conf); err != nil {
		t.Fatalf("read confidence: %v", err)
	}
	if conf >= HardConfidenceInvalidationThreshold {
		t.Errorf("confidence did not cross below cascade threshold: conf=%f threshold=%f (contradiction strength was insufficient — test setup is wrong)",
			conf, HardConfidenceInvalidationThreshold)
	}
}

// TestEpistemicContradiction_DoesNotTriggerCascadeWhenStayingAboveThreshold
// confirms ordinary contradictions that don't cross the line behave
// like ordinary confidence decreases — per docs/archive/epistemic-cascades.md,
// only threshold crossings trigger cascades.
//
// This test is also the symmetric counterpart of the confirmation
// test of the same name. The assertion is the same: cascade outbox
// must stay empty when confidence stays above the floor.
func TestEpistemicContradiction_DoesNotTriggerCascadeWhenStayingAboveThreshold(t *testing.T) {
	dm := NewTestDM(t)

	const probeContent = "epistemic-contradiction no-cascade probe"
	if _, _, err := dm.SaveLesson(probeContent, "insight", []string{"epistemic-contradiction-no-cascade"}); err != nil {
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

	// Single contradiction at default strength. Drops 0.7 → ~0.56
	// (above 0.3). No cascade expected.
	const fakeCommit = "ddddeeee33334444555566667777888899990000"
	if _, err := dm.LogChangelogEntryWithAssertions(
		"no-cascade contradiction probe",
		fakeCommit,
		nil,
		nil,
		[]ContradictionSpec{{ArtifactID: foundID, ArtifactType: "lesson"}},
	); err != nil {
		t.Fatalf("LogChangelogEntryWithAssertions: %v", err)
	}

	var cascadeCount int
	if err := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE dead_artifact_id = ? AND dead_artifact_type = 'lesson'`,
		foundID,
	).Scan(&cascadeCount); err != nil {
		t.Fatalf("count cascade outbox rows: %v", err)
	}
	if cascadeCount != 0 {
		t.Errorf("cascade outbox has %d rows for the contradicted lesson; expected 0 (single default-strength challenge at -0.6 cannot cross 0.3 from 0.7)", cascadeCount)
	}

	// Sanity: confidence must still be above 0.3.
	var conf float64
	if err := dm.QueryRowTracked(`SELECT confidence FROM lessons WHERE id = ?`, foundID).Scan(&conf); err != nil {
		t.Fatalf("read confidence: %v", err)
	}
	if conf < HardConfidenceInvalidationThreshold {
		t.Errorf("single contradiction dropped confidence below cascade threshold: conf=%f threshold=%f (test setup is wrong — single challenge should NOT cross)",
			conf, HardConfidenceInvalidationThreshold)
	}
}

// TestEpistemicContradiction_RequiresExplicitAssertion is the same
// negative-case discipline as the confirmation suite: the relationship
// must be asserted by the caller, not inferred. A commit whose fact
// text happens to contain keywords matching the lesson content must
// NOT auto-contradict the lesson.
//
// The negative case (no contradicts_*_id) routes to the legacy
// LogChangelogEntry path — no contradiction evidence row should
// land, even when the fact text mentions the lesson id and challenge-
// adjacent vocabulary.
func TestEpistemicContradiction_RequiresExplicitAssertion(t *testing.T) {
	dm := NewTestDM(t)

	const probeContent = "epistemic-contradiction explicit-only probe"
	if _, _, err := dm.SaveLesson(probeContent, "insight", []string{"epistemic-contradiction-explicit-only"}); err != nil {
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

	// Negative case: log a changelog that mentions every relevant
	// keyword, but assert NO contradictions. LogChangelogEntry (the
	// legacy path) must be taken — no contradiction evidence row
	// should land, no confidence drop.
	const fakeCommit = "eeefffff33334444555566667777888899990000"
	matchingFact := "epistemic-contradiction explicit-only probe challenge wrong " + foundID + " invalidated disproven"
	if _, err := dm.LogChangelogEntry(matchingFact, fakeCommit, nil); err != nil {
		t.Fatalf("LogChangelogEntry (no assertions): %v", err)
	}

	// Confidence must NOT have changed (no evidence was added).
	var afterNoAssert float64
	if err := dm.QueryRowTracked(`SELECT confidence FROM lessons WHERE id = ?`, foundID).Scan(&afterNoAssert); err != nil {
		t.Fatalf("read post-no-assert confidence: %v", err)
	}
	if afterNoAssert != before {
		t.Errorf("confidence changed without explicit assertion: before=%f after=%f (keyword-matching must NOT trigger contradiction)",
			before, afterNoAssert)
	}

	// Evidence row count for this lesson must still be zero.
	var evCount int
	if err := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND artifact_type = 'lesson'`,
		foundID,
	).Scan(&evCount); err != nil {
		t.Fatalf("count evidence rows: %v", err)
	}
	if evCount != 0 {
		t.Errorf("found %d evidence rows for the lesson without explicit assertion", evCount)
	}

	// Sanity: explicit assertion DOES produce one row.
	const fakeCommit2 = "ffff000033334444555566667777888899990000"
	if _, err := dm.LogChangelogEntryWithAssertions(
		matchingFact,
		fakeCommit2,
		nil,
		nil,
		[]ContradictionSpec{{ArtifactID: foundID, ArtifactType: "lesson"}},
	); err != nil {
		t.Fatalf("LogChangelogEntryWithAssertions (explicit): %v", err)
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

// TestEpistemicContradiction_AtomicityRollback pins atomicity for the
// contradiction path. A batch with a real lesson + a non-existent
// target must roll back the changelog memory AND any prior valid
// contradictions in the same call. Same shape as the confirmation
// version, but exercises the negative direction.
func TestEpistemicContradiction_AtomicityRollback(t *testing.T) {
	dm := NewTestDM(t)

	// Seed a real lesson so the FIRST contradiction would succeed.
	const realProbeContent = "epistemic-contradiction atomic real probe"
	if _, _, err := dm.SaveLesson(realProbeContent, "insight", []string{"epistemic-contradiction-atomic-real"}); err != nil {
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

	// Mixed batch: first contradiction is real (would succeed),
	// second is non-existent (must trigger rollback of the first).
	const fakeCommit = "9999aaaabbbbccccddddeeeeffff000011112222"
	_, err = dm.LogChangelogEntryWithAssertions(
		"atomicity contradiction probe commit",
		fakeCommit,
		nil,
		nil,
		[]ContradictionSpec{
			{ArtifactID: foundReal, ArtifactType: "lesson"},
			{ArtifactID: "lesson-does-not-exist", ArtifactType: "lesson"},
		},
	)
	if err == nil {
		t.Fatalf("expected error for non-existent contradiction target; got nil")
	}
	if !strings.Contains(err.Error(), "lesson-does-not-exist") {
		t.Errorf("error %q does not name the bad artifact id", err)
	}

	// Atomicity: no changelog memory, no evidence rows for the
	// real lesson, no cascade outbox rows (the contradiction
	// wasn't allowed to land either).
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

	var cascadeAfter int
	if err := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE dead_artifact_id = ?`,
		foundReal,
	).Scan(&cascadeAfter); err != nil {
		t.Fatalf("count cascade outbox rows: %v", err)
	}
	if cascadeAfter != 0 {
		t.Errorf("cascade outbox has %d rows despite rollback", cascadeAfter)
	}
}