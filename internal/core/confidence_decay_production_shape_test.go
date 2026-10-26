// confidence_decay_production_shape_test.go — pin the decay pass against
// the shapes production actually writes, and against the artifacts whose
// confidence is pinned by a challenge.
//
// Two defects this file pins.
//
// # 1. The decay anchor could not read a production `created_at`
//
// `memories.created_at` is `INTEGER NOT NULL DEFAULT
// (CAST(strftime('%s','now') AS INTEGER))` (internal/core/schema.go)
// and the production write path passes an integer epoch
// (internal/core/db.go). The anchor reader, however, scanned that
// column into a `string` and parsed it with a helper that accepts only
// RFC3339 and "2006-01-02 15:04:05". An integer epoch is neither, so
// the parse failed, the anchor fell back to "now", `tDays` was 0, and
// NO DECAY WAS EVER APPLIED to an artifact without positive evidence.
//
// The existing decay tests insert `created_at` as an RFC3339 TEXT
// value — a shape production never writes — so the whole population
// this pass exists to move was invisible to them.
//
// # 2. The decay pass destroyed the challenge floor
//
// `ChallengeMemory` pins a challenged memory's confidence to
// `ChallengedMemoryConfidenceFloor` (0.5) and expires every one of its
// evidence rows, so that fresh evidence is the only thing that can
// re-elevate it (F7.1). The decay pass then recomputed that memory
// from an empty evidence set, derived the INITIAL confidence, and
// overwrote the floor — silently, within a day, with no evidence added.
//
// The challenge mechanism is the substrate's primary defence against a
// known-wrong memory. A maintenance pass that quietly reverses it is
// worse than not having the pass at all.
package internal

import (
	"math"
	"testing"
	"time"
)

// insertDecayMemoryEpoch writes one memories-backed artifact with an
// INTEGER epoch `created_at` — the shape the production write path
// uses (internal/core/db.go, saveMemoryRow).
func insertDecayMemoryEpoch(t *testing.T, dm *DatabaseManager, id, collection string, createdAt time.Time, confidence float64) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, metadata, weight, confidence, created_at, updated_at)
		 VALUES (?, ?, ?, '{}', 5, ?, ?, ?)`,
		id, collection, "content for "+id, confidence, createdAt.Unix(), createdAt.Unix(),
	); err != nil {
		t.Fatalf("insert memory %s: %v", id, err)
	}
}

// TestConfidenceDecay_IntegerEpochCreatedAtDecays pins defect 1: an
// artifact whose `created_at` is an integer epoch must lose the
// confidence the formula says it has lost.
//
// Before the fix the anchor could not parse the epoch, `tDays` was 0
// and the pass reported "unchanged" for every such artifact.
func TestConfidenceDecay_IntegerEpochCreatedAtDecays(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemoryEpoch(t, dm, "mem-epoch-1", "memories", created, InitialConfidence("memory"))

	before, _ := storedDecayConfidence(t, dm, "mem-epoch-1")
	if math.Abs(before-InitialConfidence("memory")) > 1e-9 {
		t.Fatalf("precondition: stored confidence = %v, want the initial %v",
			before, InitialConfidence("memory"))
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}
	if res.Due != 1 || res.Scanned != 1 || res.Changed != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want due=1 scanned=1 changed=1 failed=0", res)
	}

	after, watermark := storedDecayConfidence(t, dm, "mem-epoch-1")
	if watermark == nil {
		t.Errorf("watermark was not advanced")
	}
	if math.Abs(after-before) < 1e-6 {
		t.Errorf("confidence did not move: %v -> %v; the decay term was not applied "+
			"to an artifact whose created_at is an integer epoch", before, after)
	}
	// The formula: sigmoid(log(0.8/0.2) - 0.01 * 200).
	want := computeConfidence("memory", nil, decayFixtureNow, created, 0.005)
	if math.Abs(after-want) > 1e-6 {
		t.Errorf("confidence = %v, want the formula's %v", after, want)
	}
}

// TestConfidenceDecay_ChallengedMemoryKeepsItsFloor pins defect 2: a
// challenged memory's confidence is pinned to the challenge floor, and
// the maintenance pass must not move it — neither up (which would
// reverse the challenge) nor down (which would erode a floor the
// challenge deliberately set).
func TestConfidenceDecay_ChallengedMemoryKeepsItsFloor(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemoryEpoch(t, dm, "mem-challenged-1", "memories", created, InitialConfidence("memory"))

	// Give it a positive evidence row so the challenge has something to
	// neutralize, then challenge it.
	if err := AddEvidence(dm, EvidenceInput{
		ArtifactID:   "mem-challenged-1",
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.6,
		CreatedBy:    "decay-test",
	}); err != nil {
		t.Fatalf("add evidence: %v", err)
	}
	if err := dm.ChallengeMemory("mem-challenged-1", 1, "theory-under-test"); err != nil {
		t.Fatalf("challenge: %v", err)
	}

	floor, _ := storedDecayConfidence(t, dm, "mem-challenged-1")
	if math.Abs(floor-ChallengedMemoryConfidenceFloor) > 1e-9 {
		t.Fatalf("precondition: challenged confidence = %v, want the floor %v",
			floor, ChallengedMemoryConfidenceFloor)
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}

	after, _ := storedDecayConfidence(t, dm, "mem-challenged-1")
	if math.Abs(after-ChallengedMemoryConfidenceFloor) > 1e-6 {
		t.Errorf("challenged memory confidence = %v after the decay pass, want it "+
			"unchanged at the challenge floor %v; the pass must not re-elevate a "+
			"challenged memory (decay result %+v)", after, ChallengedMemoryConfidenceFloor, res)
	}
}

// TestConfidenceDecay_ProvenTheoryIsNotDecayed pins the same invariant
// for a theory the operator has explicitly proven.
//
// A proven theory is a terminal, human-asserted state. Letting the
// decay pass walk its cached confidence down to the invalidation
// threshold would enqueue a `confidence_floor` cascade that invalidates
// every downstream artifact citing a theory the operator declared
// sound — the maintenance pass would undo the operator's decision.
func TestConfidenceDecay_ProvenTheoryIsNotDecayed(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, metadata, weight, confidence, created_at, updated_at)
		 VALUES (?, 'theories', ?, '{"status":"proven"}', 5, ?, ?, ?)`,
		"theory-proven-1", "a proven theory", InitialConfidence("theory"),
		created.Unix(), created.Unix(),
	); err != nil {
		t.Fatalf("insert theory: %v", err)
	}

	before, _ := storedDecayConfidence(t, dm, "theory-proven-1")

	if _, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10}); err != nil {
		t.Fatalf("run decay: %v", err)
	}

	after, _ := storedDecayConfidence(t, dm, "theory-proven-1")
	if math.Abs(after-before) > 1e-6 {
		t.Errorf("proven theory confidence = %v after the decay pass, want it "+
			"unchanged at %v", after, before)
	}
}
