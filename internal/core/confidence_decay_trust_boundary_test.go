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

// addAssumesFalseDownstream registers a downstream artifact that cites
// the source with polarity='assumes_false' — the opt-in that makes the
// positive-direction cascade reach it.
//
// Without this fixture the cascade discovers no target and enqueues
// nothing, so an assertion on the outbox would pass VACUOUSLY: it
// would say "no intent was created" whether or not the crossing fired.
func addAssumesFalseDownstream(t *testing.T, dm *DatabaseManager, sourceID, downstreamID string) {
	t.Helper()
	insertDecayMemoryEpoch(t, dm, downstreamID, "decisions",
		decayFixtureNow.Add(-10*24*time.Hour), InitialConfidence("decision"))
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO epistemic_provenance (id, source_id, source_type, downstream_id,
		                                 downstream_type, event_id, polarity, created_at)
		VALUES (?, ?, 'memory', ?, 'decision', ?, 'assumes_false', ?)`,
		"prov-"+downstreamID, sourceID, downstreamID, "evt-"+downstreamID, decayFixtureNow.Unix(),
	); err != nil {
		t.Fatalf("insert provenance: %v", err)
	}
}

// countCascadeIntents counts the cascade intents the substrate enqueued
// for a source artifact.
func countCascadeIntents(t *testing.T, dm *DatabaseManager, sourceID string) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE dead_artifact_id = ?`, sourceID,
	).Scan(&n); err != nil {
		t.Fatalf("count cascade intents: %v", err)
	}
	return n
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
// A maintenance pass must never be the thing that declares a
// foundation proven.
//
// The test carries its own NEGATIVE CONTROL: the same fixture, driven by
// fresh evidence, DOES produce a cascade intent. That is what makes the
// zero-intent assertion meaningful — without it, the assertion would
// hold for the wrong reason (no downstream target) and would prove
// nothing.
func TestConfidenceDecay_RestoredMemoryIsNotDeclaredProven(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-10 * 24 * time.Hour)
	restoredMemoryFixture(t, dm, "mem-restored-cascade", created)
	addAssumesFalseDownstream(t, dm, "mem-restored-cascade", "dec-restore-downstream")

	if _, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10}); err != nil {
		t.Fatalf("run decay: %v", err)
	}
	if n := countCascadeIntents(t, dm, "mem-restored-cascade"); n != 0 {
		t.Errorf("the decay pass enqueued %d cascade intent(s) for a challenge-restored "+
			"memory; a maintenance pass must never declare a foundation proven", n)
	}

	// NEGATIVE CONTROL: the fixture is live. Fresh evidence raises the
	// same artifact past the proven threshold, and the cascade reaches
	// the downstream artifact.
	if err := AddEvidence(dm, EvidenceInput{
		ArtifactID:         "mem-restored-cascade",
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
	if n := countCascadeIntents(t, dm, "mem-restored-cascade"); n == 0 {
		t.Error("the negative control produced no cascade intent: the fixture does not " +
			"reach the cascade, so the zero-intent assertion above proves nothing")
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

	// The challenge neutralises the evidence by setting expires_at to
	// NOW, at one-second granularity, and the loader treats a row as
	// expired only once expires_at < now. A recompute in the SAME
	// second as the challenge therefore still sees the neutralised
	// evidence, and the derived value depends on the sub-second gap
	// between the two. Pin the expiry into the past so the fixture
	// states one thing: the ledger is empty, and the formula derives
	// the INITIAL confidence from it.
	if _, err := dm.SQLDB().Exec(
		`UPDATE evidence SET expires_at = 1 WHERE artifact_id = 'mem-manual-challenged'`); err != nil {
		t.Fatalf("pin the evidence expiry: %v", err)
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

// supersededDecisionFixture builds the state the F5-2 supersede path
// produces: a decision whose confidence was dropped to
// DecisionInitialConfidence, whose metadata carries the supersede
// marker, and whose evidence ledger is empty.
//
// It drives the REAL path — dm.SupersedeDecision — so the fixture
// cannot drift from what production writes.
func supersededDecisionFixture(t *testing.T, dm *DatabaseManager, id string, createdAt time.Time) {
	t.Helper()

	insertDecayMemoryEpoch(t, dm, id, "decisions", createdAt, InitialConfidence("decision"))

	if _, err := dm.SupersedeDecision(id, "ctx", "the new choice", "because", "", nil, nil, ActiveContext{}); err != nil {
		t.Fatalf("supersede: %v", err)
	}
}

// TestConfidenceDecay_DecayPassDoesNotPromoteASupersededDecision pins
// the F5-2 half of the trust boundary.
//
// A superseded decision falls back to DecisionInitialConfidence: "the
// artifact is at 'no evidence' until fresh evidence arrives" — the same
// contract as the F7.1 challenged memory. The live installation has
// exactly such a row: a decision stored at 0.5 with an empty evidence
// ledger, whose confidence_history carries one `supersede` row with
// evidence_count = 0.
//
// The formula derives ~0.6 for it (the per-type initial confidence,
// barely decayed), so a maintenance pass that applied the formula
// verbatim would RAISE the demoted decision back to 0.6 — undoing a
// recorded supersede, with no evidence added, within one cadence window
// of a deploy.
func TestConfidenceDecay_DecayPassDoesNotPromoteASupersededDecision(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-2 * 24 * time.Hour)
	supersededDecisionFixture(t, dm, "dec-superseded-1", created)

	before, _ := storedDecayConfidence(t, dm, "dec-superseded-1")
	if math.Abs(before-DecisionInitialConfidence) > 1e-9 {
		t.Fatalf("precondition: superseded confidence = %v, want the demotion value %v",
			before, DecisionInitialConfidence)
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}

	after, _ := storedDecayConfidence(t, dm, "dec-superseded-1")
	if after > before+1e-9 {
		t.Errorf("superseded decision confidence = %.12f after the decay pass, want it "+
			"unchanged at the demotion value %.12f; the pass must not re-elevate a "+
			"superseded decision, and no evidence was added (decay result %+v)",
			after, before, res)
	}
}

// TestConfidenceDecay_ManualRecomputeDoesNotRaiseASupersededDecision
// pins the same invariant on the operator's manual recompute.
//
// `mpm ops confidence recompute` is a re-derivation from evidence. On a
// superseded decision with an empty ledger it derives the initial
// confidence and undoes the supersede. Fresh evidence is what should
// undo it — and an evidence event is exempt from the guard, so that
// path still works.
func TestConfidenceDecay_ManualRecomputeDoesNotRaiseASupersededDecision(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-2 * 24 * time.Hour)
	supersededDecisionFixture(t, dm, "dec-superseded-2", created)

	before, _ := storedDecayConfidence(t, dm, "dec-superseded-2")

	if err := RecomputeConfidence(dm, "dec-superseded-2", "decision", RecomputeReasonManual); err != nil {
		t.Fatalf("manual recompute: %v", err)
	}

	after, _ := storedDecayConfidence(t, dm, "dec-superseded-2")
	if after > before+1e-9 {
		t.Errorf("superseded decision confidence = %.12f after a manual recompute, want it "+
			"unchanged at the demotion value %.12f — a recompute is a re-derivation from "+
			"evidence, not an un-supersede", after, before)
	}
}

// TestConfidenceDecay_ManualRecomputeMayRaiseANonDemotedArtifact pins
// the OTHER side of the guard: it must not become a blanket freeze.
//
// The operator's recompute is the substrate's repair tool: an artifact
// whose cached value disagrees with what its evidence supports is
// exactly what the operator is trying to fix. The guard therefore
// clamps a raise only where the stored value is a trust-boundary
// decision (a demotion) or where there is no live evidence to justify
// the raise.
func TestConfidenceDecay_ManualRecomputeMayRaiseANonDemotedArtifact(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-10 * 24 * time.Hour)
	insertDecayMemoryEpoch(t, dm, "mem-plain-raise", "memories", created, 0.5)

	// Live evidence that supports a higher value than the stored one.
	if err := AddEvidence(dm, EvidenceInput{
		ArtifactID:         "mem-plain-raise",
		ArtifactType:       "memory",
		Type:               "observation",
		SourceGroup:        "manual_review",
		Strength:           1.0,
		IndependenceFactor: 1.0,
		CreatedBy:          "reviewer",
		CreatedAt:          decayFixtureNow.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("add evidence: %v", err)
	}
	// The evidence event set the value the formula derives, so the
	// cached value is no longer stale. Put it back to a stale-low value
	// to model the state the operator is repairing.
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET confidence = 0.5 WHERE id = 'mem-plain-raise'`); err != nil {
		t.Fatalf("reset confidence: %v", err)
	}

	if err := RecomputeConfidence(dm, "mem-plain-raise", "memory", RecomputeReasonManual); err != nil {
		t.Fatalf("manual recompute: %v", err)
	}

	after, _ := storedDecayConfidence(t, dm, "mem-plain-raise")
	if after <= 0.5+1e-9 {
		t.Errorf("confidence = %.12f after a manual recompute, want it above the stale "+
			"%.12f — the guard must not freeze a value that live evidence supports", after, 0.5)
	}
}

// TestConfidenceDecay_PassDoesNotPromoteAnArtifactWithNoLiveEvidence
// pins clause 2 of the guard: the maintenance pass may not promote an
// artifact that has no live evidence.
//
// The formula's value for an evidence-less artifact is the
// ABSENCE-of-evidence baseline — the per-type initial confidence, decayed
// from the artifact's creation. That is a statement about the artifact's
// mere existence, not about support. Writing it over a stored value that
// sits below it is a promotion, and an unattended background pass may not
// promote.
//
// The fixture is deliberately NOT a demotion: there is no challenge, no
// restore, no supersede, no invalidate. It is an artifact whose stored
// value happens to sit below the baseline, so the test isolates clause 2
// rather than clause 1.
func TestConfidenceDecay_PassDoesNotPromoteAnArtifactWithNoLiveEvidence(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-10 * 24 * time.Hour)
	insertDecayMemoryEpoch(t, dm, "mem-no-evidence", "memories", created, 0.5)

	// The formula's value for this artifact is HIGHER than the stored
	// one, so a pass that applied the formula verbatim would raise it.
	derived := computeConfidence("memory", nil, decayFixtureNow, created, 0.005)
	if derived <= 0.5 {
		t.Fatalf("precondition: the formula derives %.12f, want it above the stored 0.5 "+
			"so the promotion is detectable", derived)
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}

	after, _ := storedDecayConfidence(t, dm, "mem-no-evidence")
	if after > 0.5+1e-9 {
		t.Errorf("confidence = %.12f after the decay pass, want it unchanged at 0.5; the "+
			"pass must not promote an artifact with no live evidence — the formula's value "+
			"for such an artifact is the absence-of-evidence baseline, not support "+
			"(decay result %+v)", after, res)
	}
}

// TestConfidenceDecay_ManualRecomputeMayRepairAnArtifactWithNoLiveEvidence
// pins the SCOPE of clause 2: it constrains the unattended maintenance
// pass, not the operator's repair command.
//
// `mpm ops confidence recompute` exists to bring a cached value back
// into agreement with the formula. An artifact whose cached value
// disagrees with what the formula derives is exactly what the operator is
// trying to fix, so the command must still be able to raise it.
//
// The distinction is deliberate and is the difference between an
// unattended job and a supervised command: maintenance may not promote,
// but an operator may repair — and neither may undo a recorded demotion.
func TestConfidenceDecay_ManualRecomputeMayRepairAnArtifactWithNoLiveEvidence(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-10 * 24 * time.Hour)
	insertDecayMemoryEpoch(t, dm, "mem-repair", "memories", created, 0.5)

	if err := RecomputeConfidence(dm, "mem-repair", "memory", RecomputeReasonManual); err != nil {
		t.Fatalf("manual recompute: %v", err)
	}

	after, _ := storedDecayConfidence(t, dm, "mem-repair")
	want := computeConfidence("memory", nil, time.Now(), created, 0.005)
	if after <= 0.5+1e-9 {
		t.Errorf("confidence = %.12f after a manual recompute, want it above the stale "+
			"0.5 — the operator's repair command must still be able to bring a cached "+
			"value back into agreement with the formula", after)
	}
	if math.Abs(after-want) > 1e-3 {
		t.Errorf("confidence = %.12f, want the formula's %.12f", after, want)
	}
}

// TestConfidenceDecay_PassNeverDeclaresAFoundationProven pins the
// coupling hazard the maintenance pass must stay out of.
//
// "This foundation is now solid" is a statement about EVIDENCE. The
// upward crossing of HardConfidenceProvenThreshold enqueues the
// foundation_proven cascade, which surfaces every downstream artifact
// that was running on the assumption that this foundation was uncertain.
//
// A background maintenance pass has no business emitting that event: it
// creates no evidence, it only notices that a cached value moved. Letting
// it do so would let unattended maintenance declare a theory proven —
// the automatic approval of an uncertain finding, and a coupling to the
// critic-arming surface that maintenance has no business with.
//
// The fixture is the one shape that would cross the threshold: live
// evidence that supports a value above the threshold, against a cached
// value below it. The pass MAY reconcile that value — the evidence is
// real — but it must not declare the foundation proven.
//
// The negative control is the same fixture driven by the operator's
// manual recompute, which is not the maintenance pass and therefore does
// emit the event. It is what makes the zero-intent assertion mean
// something.
func TestConfidenceDecay_PassNeverDeclaresAFoundationProven(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-10 * 24 * time.Hour)

	// The source the maintenance pass touches.
	insertDecayMemoryEpoch(t, dm, "mem-foundation-pass", "memories", created, 0.5)
	addAssumesFalseDownstream(t, dm, "mem-foundation-pass", "dec-foundation-pass")
	// The identical source the operator's recompute touches.
	insertDecayMemoryEpoch(t, dm, "mem-foundation-manual", "memories", created, 0.5)
	addAssumesFalseDownstream(t, dm, "mem-foundation-manual", "dec-foundation-manual")

	// Live evidence, inserted directly so the cached values stay stale
	// — the state these recomputes are meant to reconcile.
	for _, id := range []string{"mem-foundation-pass", "mem-foundation-manual"} {
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group,
			                      strength, independence_factor, created_by, created_at, expires_at, notes, reference_url)
			VALUES (?, ?, 'memory', 'observation', 'manual_review',
			        1.0, 1.0, 'reviewer', ?, NULL, NULL, NULL)`,
			"ev-"+id, id, decayFixtureNow.Add(-time.Hour).Unix(),
		); err != nil {
			t.Fatalf("insert evidence: %v", err)
		}
	}

	// NEGATIVE CONTROL: a transactional recompute that is NOT the
	// maintenance pass does emit the event. This proves the fixture
	// reaches the cascade at all.
	//
	// It has to be transactional: the cascade hook only fires when the
	// recompute runs inside a transaction, and the maintenance pass is
	// the only non-evidence recompute that does.
	if err := dm.WithTx(func(node DBNode) error {
		_, err := recomputeConfidenceAt(node, "mem-foundation-manual", "memory", RecomputeReasonManual, decayFixtureNow, false)
		return err
	}); err != nil {
		t.Fatalf("transactional manual recompute: %v", err)
	}
	if n := countCascadeIntents(t, dm, "mem-foundation-manual"); n == 0 {
		t.Fatal("the negative control produced no cascade intent: the fixture does not " +
			"reach the cascade, so the assertion below would prove nothing")
	}

	// The maintenance pass, on the identical fixture.
	if _, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10}); err != nil {
		t.Fatalf("run decay: %v", err)
	}
	if n := countCascadeIntents(t, dm, "mem-foundation-pass"); n != 0 {
		t.Errorf("the decay pass enqueued %d foundation_proven intent(s); maintenance "+
			"must never declare a foundation proven", n)
	}
}
