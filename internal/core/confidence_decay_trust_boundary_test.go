package internal

// confidence_decay_trust_boundary_test.go — the decay pass against the
// artifacts whose confidence was set by a TRUST-BOUNDARY action rather
// than by the confidence formula.
//
// THE TRUST BOUNDARY
// ------------------
// F7.1 is explicit about the challenge cycle:
//
//   - a challenged memory's confidence is PINNED to
//     ChallengedMemoryConfidenceFloor (0.5), and its evidence rows are
//     expired, so fresh evidence is the only thing that can re-elevate it;
//   - restoration does NOT silently re-promote the pre-challenge value —
//     it resets to the floor.
//
// The decay pass already refuses to touch a memory whose metadata status
// is 'challenged' (confidence_decay_production_shape_test.go). But the
// RESTORE clears that status — deliberately, so the memory can be worked
// with again — and leaves the floor in place. That makes the restored
// memory a PARTICIPANT of the decay pass, and the pass recomputes it from
// an empty (all-expired) evidence set.
//
// Two ways that goes wrong:
//
//   - a RECENT restored memory: the formula derives ~0.8 from an empty
//     evidence set, so the pass RAISES the floor value to near-maximal
//     confidence, with no evidence added. That is a promotion of an
//     uncertain artifact, which is precisely what F7.1 forbids;
//   - an OLD restored memory: the formula derives well below the floor,
//     so the pass erodes the floor the restore deliberately set.
//
// The invariant the pass must hold is stated in the existing test:
// the pass must not move a challenge-pinned confidence "neither up ...
// nor down" — with ONE exception, which is the whole point of F7.1:
// FRESH EVIDENCE re-elevates. The guard is therefore asymmetric, and
// this file pins both halves.
//
// The same guard covers the OPERATOR's manual recompute
// (`mpm ops confidence recompute`), which had the identical hole: a
// recompute of a challenged memory derived the initial confidence from
// the empty evidence set and overwrote the floor.

import (
	"math"
	"testing"
	"time"
)

// restoredMemoryFixture builds the state the real challenge→restore
// cycle produces: a memory whose evidence has been neutralised, whose
// confidence sits at the F7.1 floor, and whose metadata carries the
// restore marker but no 'challenged' status.
//
// It drives the REAL paths — ChallengeMemory then
// RestoreMemoryFromChallenge — so the fixture cannot drift from what
// production writes.
func restoredMemoryFixture(t *testing.T, dm *DatabaseManager, id string, createdAt time.Time) {
	t.Helper()

	insertDecayMemoryEpoch(t, dm, id, "memories", createdAt, InitialConfidence("memory"))

	if err := AddEvidence(dm, EvidenceInput{
		ArtifactID:   id,
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.6,
		CreatedBy:    "trust-boundary-test",
	}); err != nil {
		t.Fatalf("add evidence: %v", err)
	}

	if err := dm.ChallengeMemory(id, 1, "theory-under-test"); err != nil {
		t.Fatalf("challenge: %v", err)
	}

	// The challenge must have produced the challenged state, or the
	// fixture is not testing what it claims to.
	challenged, _ := storedDecayConfidence(t, dm, id)
	if math.Abs(challenged-ChallengedMemoryConfidenceFloor) > 1e-9 {
		t.Fatalf("precondition: challenged confidence = %v, want the floor %v",
			challenged, ChallengedMemoryConfidenceFloor)
	}

	if _, err := dm.RestoreMemoryFromChallenge(id); err != nil {
		t.Fatalf("restore: %v", err)
	}

	restored, _ := storedDecayConfidence(t, dm, id)
	if math.Abs(restored-ChallengedMemoryConfidenceFloor) > 1e-9 {
		t.Fatalf("precondition: restored confidence = %v, want the floor %v",
			restored, ChallengedMemoryConfidenceFloor)
	}

	// The restore MUST have cleared the challenged status, otherwise this
	// fixture is just the already-covered challenged case.
	var status string
	if err := dm.SQLDB().QueryRow(
		`SELECT COALESCE(json_extract(metadata, '$.status'), '') FROM memories WHERE id = ?`, id,
	).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "" {
		t.Fatalf("precondition: status = %q, want it cleared by the restore", status)
	}
}

// TestConfidenceDecay_RestoredMemoryKeepsItsFloor is the probe that
// found the defect: a restored memory's confidence must not move.
//
// A recent restore is the sharpest case, because the formula derives a
// HIGHER value than the floor from an empty evidence set — so the pass
// would PROMOTE an artifact that has no valid evidence and was
// explicitly challenged and restored.
func TestConfidenceDecay_RestoredMemoryKeepsItsFloor(t *testing.T) {
	dm := NewTestDM(t)

	// Ten days old: young enough that the formula's derived value is
	// above the floor, so a promotion is detectable.
	created := decayFixtureNow.Add(-10 * 24 * time.Hour)
	restoredMemoryFixture(t, dm, "mem-restored-1", created)

	before, _ := storedDecayConfidence(t, dm, "mem-restored-1")

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}

	after, _ := storedDecayConfidence(t, dm, "mem-restored-1")
	if math.Abs(after-before) > 1e-6 {
		t.Errorf("restored memory confidence = %.12f after the decay pass, want it "+
			"unchanged at the F7.1 floor %.12f; the pass must not re-elevate (or erode) "+
			"a challenge-restored artifact, and no evidence was added (decay result %+v)",
			after, ChallengedMemoryConfidenceFloor, res)
	}
	if res.Changed != 0 {
		t.Errorf("decay result = %+v, want changed=0 — nothing about the artifact moved", res)
	}
}

// TestConfidenceDecay_RestoredMemoryIsNotDeclaredProven is the cascade
// half of the promotion hazard.
//
// The positive cascade fires when a recompute crosses UPWARD past
// HardConfidenceProvenThreshold (0.8): the substrate then tells the
// materializer that this foundation is now solid, and the artifacts
// that were running on the assumption that it was uncertain are
// re-examined as still valid.
//
// A maintenance pass must never be the thing that declares a founda-
// tion proven. The artifact below is the same restored memory as the
// test above — before the fix the pass promoted it to ~0.78, and a
// memory restored within a day of its creation would have been derived
// at exactly 0.8, which crosses the threshold.
//
// The assertion is about the OUTCOME, not about the number: no cascade
// intent may exist for this artifact.
func TestConfidenceDecay_RestoredMemoryIsNotDeclaredProven(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-10 * 24 * time.Hour)
	restoredMemoryFixture(t, dm, "mem-restored-cascade", created)

	if _, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10}); err != nil {
		t.Fatalf("run decay: %v", err)
	}

	var intents int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE dead_artifact_id = 'mem-restored-cascade'`,
	).Scan(&intents); err != nil {
		t.Fatalf("count cascade intents: %v", err)
	}
	if intents != 0 {
		t.Errorf("the decay pass enqueued %d cascade intent(s) for a challenge-restored "+
			"memory; a maintenance pass must never declare a foundation proven", intents)
	}
}

// TestConfidenceDecay_DryRunAgreesWithThePassOnAPinnedArtifact pins the
// dry-run contract for the guard specifically.
//
// A preview that reported "changed" for an artifact the real pass would
// leave alone would tell an operator the pass would move values that it
// cannot move — and the operator would be looking at a wrong backlog.
func TestConfidenceDecay_DryRunAgreesWithThePassOnAPinnedArtifact(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-10 * 24 * time.Hour)
	restoredMemoryFixture(t, dm, "mem-restored-dry", created)

	dry, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Changed != 0 || dry.Unchanged != 1 {
		t.Errorf("dry run = %+v, want changed=0 unchanged=1 — the preview must apply the "+
			"same floor guard the pass applies", dry)
	}

	real, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("real run: %v", err)
	}
	if dry.Changed != real.Changed || dry.Unchanged != real.Unchanged {
		t.Errorf("dry run = %+v, real pass = %+v — the preview must match the pass", dry, real)
	}
}

// TestConfidenceDecay_OldRestoredMemoryMayStillDecay pins the
// ASYMMETRY of the guard.
//
// The guard clamps RAISES only. A decay that moves the value DOWN is
// still allowed, because lower confidence is the conservative direction
// and the formula's whole purpose is to move it down.
//
// So an old restored memory — whose formula value is far below the
// floor — is still decayed. The floor is not a permanent pin; it is the
// value the restore sets, and time still applies to it.
//
// This is the asymmetry that makes the guard a guard rather than a
// freeze: freezing the value would reintroduce exactly the divergence
// issue #92 is about, for the whole restored population.
func TestConfidenceDecay_OldRestoredMemoryMayStillDecay(t *testing.T) {
	dm := NewTestDM(t)

	// 200 days old: the formula's value is far below the floor.
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	restoredMemoryFixture(t, dm, "mem-restored-old", created)

	before, _ := storedDecayConfidence(t, dm, "mem-restored-old")

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}
	if res.Changed != 1 {
		t.Errorf("decay result = %+v, want changed=1 — the formula's value is below the "+
			"floor, so the decay still applies", res)
	}

	after, _ := storedDecayConfidence(t, dm, "mem-restored-old")
	if !(after < before) {
		t.Errorf("restored memory confidence = %.12f, want it below the floor %.12f — "+
			"the decay must still apply", after, before)
	}
	// And it must be the formula's value, not something in between.
	ev, lastPositiveAt, err := loadEvidenceForRecompute(dm, "mem-restored-old", "memory", decayFixtureNow)
	if err != nil {
		t.Fatalf("load evidence: %v", err)
	}
	want := computeConfidence("memory", ev, decayFixtureNow, lastPositiveAt, 0.005)
	if math.Abs(after-want) > 1e-9 {
		t.Errorf("confidence = %.12f, want the formula's %.12f", after, want)
	}
}

// TestConfidenceDecay_FreshEvidenceReElevatesAChallengedMemory pins the
// EXEMPTION, and it is the most important assertion in this file.
//
// F7.1 says fresh evidence is what re-elevates a challenge-pinned
// memory. If the guard blocked that, the challenge cycle would be
// permanent — the memory could never be trusted again, no matter how
// much independent evidence arrived.
//
// The evidence path must therefore be exempt from the guard.
func TestConfidenceDecay_FreshEvidenceReElevatesAChallengedMemory(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-10 * 24 * time.Hour)
	insertDecayMemoryEpoch(t, dm, "mem-challenged-recover", "memories", created, InitialConfidence("memory"))

	if err := AddEvidence(dm, EvidenceInput{
		ArtifactID:   "mem-challenged-recover",
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.6,
		CreatedBy:    "trust-boundary-test",
	}); err != nil {
		t.Fatalf("add evidence: %v", err)
	}
	if err := dm.ChallengeMemory("mem-challenged-recover", 1, "theory-under-test"); err != nil {
		t.Fatalf("challenge: %v", err)
	}
	floor, _ := storedDecayConfidence(t, dm, "mem-challenged-recover")
	if math.Abs(floor-ChallengedMemoryConfidenceFloor) > 1e-9 {
		t.Fatalf("precondition: challenged confidence = %v, want the floor %v",
			floor, ChallengedMemoryConfidenceFloor)
	}

	// Fresh, independent evidence arrives. The memory's evidence was
	// neutralised by the challenge, so this is genuinely new evidence —
	// the one thing F7.1 says may re-elevate the memory.
	if err := AddEvidence(dm, EvidenceInput{
		ArtifactID:         "mem-challenged-recover",
		ArtifactType:       "memory",
		Type:               "observation",
		SourceGroup:        "manual_review",
		Strength:           1.0,
		IndependenceFactor: 1.0,
		CreatedBy:          "reviewer",
		CreatedAt:          decayFixtureNow.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("add fresh evidence: %v", err)
	}

	after, _ := storedDecayConfidence(t, dm, "mem-challenged-recover")
	if after <= ChallengedMemoryConfidenceFloor+1e-9 {
		t.Errorf("challenged memory confidence = %.12f after fresh evidence, want it "+
			"above the floor %.12f — fresh evidence is what re-elevates a challenged "+
			"memory, and the guard must not block it", after, ChallengedMemoryConfidenceFloor)
	}
}

// TestConfidenceDecay_ManualRecomputeDoesNotRaiseAChallengedMemory
// pins the second path the guard covers.
//
// `mpm ops confidence recompute` is the operator's explicit "recompute
// this artifact" command. On a challenged memory it derived the initial
// confidence from the empty evidence set and OVERWROTE the floor —
// silently reversing a challenge the substrate had recorded.
//
// A recompute is a re-derivation from evidence; it is not an
// un-challenge. The operator's intent when they recompute a challenged
// memory is to see what the evidence supports, not to have the challenge
// quietly undone.
func TestConfidenceDecay_ManualRecomputeDoesNotRaiseAChallengedMemory(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemoryEpoch(t, dm, "mem-manual-challenged", "memories", created, InitialConfidence("memory"))

	if err := AddEvidence(dm, EvidenceInput{
		ArtifactID:   "mem-manual-challenged",
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.6,
		CreatedBy:    "trust-boundary-test",
	}); err != nil {
		t.Fatalf("add evidence: %v", err)
	}
	if err := dm.ChallengeMemory("mem-manual-challenged", 1, "theory-under-test"); err != nil {
		t.Fatalf("challenge: %v", err)
	}
	floor, _ := storedDecayConfidence(t, dm, "mem-manual-challenged")

	if err := RecomputeConfidence(dm, "mem-manual-challenged", "memory", RecomputeReasonManual); err != nil {
		t.Fatalf("manual recompute: %v", err)
	}

	after, _ := storedDecayConfidence(t, dm, "mem-manual-challenged")
	if math.Abs(after-floor) > 1e-9 {
		t.Errorf("challenged memory confidence = %.12f after a manual recompute, want it "+
			"unchanged at the floor %.12f — a recompute is a re-derivation from evidence, "+
			"not an un-challenge", after, floor)
	}
}
