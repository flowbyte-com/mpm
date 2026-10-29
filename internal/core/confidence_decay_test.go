// confidence_decay_test.go — pin the bounded confidence-decay pass
// (issue #92).
//
// The defect: computeConfidence includes time decay, but the stored
// confidence column only moved when an evidence event happened to
// recompute it. A stale artifact therefore kept a too-high snapshot
// forever. These tests use a fixed clock so the decay term is
// deterministic, and they assert the contract the maintenance pass
// promises: bounded work, restartability, no write storms, no
// unchanged-history growth, and inherited cascade semantics.
package internal

import (
	"math"
	"strings"
	"testing"
	"time"
)

// decayFixtureNow is the fixed clock shared by every test in this file.
var decayFixtureNow = time.Unix(2_000_000_000, 0).UTC()

// insertDecayMemory writes one memories-backed artifact with an explicit
// created_at and stored confidence.
func insertDecayMemory(t *testing.T, dm *DatabaseManager, id, collection string, createdAt time.Time, confidence float64) {
	t.Helper()
	ts := createdAt.UTC().Format(time.RFC3339)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, weight, confidence, created_at, updated_at)
		 VALUES (?, ?, ?, 5, ?, ?, ?)`,
		id, collection, "content for "+id, confidence, ts, ts,
	); err != nil {
		t.Fatalf("insert memory %s: %v", id, err)
	}
}

// storedDecayConfidence reads the stored confidence and watermark.
func storedDecayConfidence(t *testing.T, dm *DatabaseManager, id string) (float64, *int64) {
	t.Helper()
	var conf float64
	var checked *int64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence, confidence_checked_at FROM memories WHERE id = ?`, id,
	).Scan(&conf, &checked); err != nil {
		t.Fatalf("read confidence %s: %v", id, err)
	}
	return conf, checked
}

func decayHistoryCount(t *testing.T, dm *DatabaseManager, id string) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("count history %s: %v", id, err)
	}
	return n
}

// TestConfidenceDecay_StaleArtifactLosesConfidence is the core
// regression: an artifact with no evidence and no recent recompute must
// lose the confidence the formula says it has lost.
func TestConfidenceDecay_StaleArtifactLosesConfidence(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemory(t, dm, "mem-decay-1", "memories", created, InitialConfidence("memory"))

	before, watermark := storedDecayConfidence(t, dm, "mem-decay-1")
	if watermark != nil {
		t.Fatalf("watermark should start NULL, got %v", *watermark)
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}
	if res.Due != 1 || res.Scanned != 1 || res.Changed != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want due=1 scanned=1 changed=1 failed=0", res)
	}

	want := computeConfidence("memory", nil, decayFixtureNow, created, 0.005)
	after, watermark := storedDecayConfidence(t, dm, "mem-decay-1")
	if math.Abs(after-want) > 1e-9 {
		t.Errorf("stored confidence = %.12f, want %.12f (the formula's decayed value)", after, want)
	}
	if !(after < before) {
		t.Errorf("stale confidence did not decay: before %.6f after %.6f", before, after)
	}
	if watermark == nil || *watermark != decayFixtureNow.Unix() {
		t.Errorf("watermark = %v, want %d", watermark, decayFixtureNow.Unix())
	}

	// The recompute goes through the authoritative path: one decay_tick
	// history row, with the artifact's real evidence count.
	var trigger string
	var evidenceCount int
	if err := dm.SQLDB().QueryRow(
		`SELECT trigger, evidence_count FROM confidence_history
		 WHERE artifact_id = 'mem-decay-1' ORDER BY computed_at DESC, rowid DESC LIMIT 1`,
	).Scan(&trigger, &evidenceCount); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if trigger != string(RecomputeReasonDecayTick) {
		t.Errorf("history trigger = %q, want %q", trigger, RecomputeReasonDecayTick)
	}
	if evidenceCount != 0 {
		t.Errorf("evidence_count = %d, want 0", evidenceCount)
	}
}

// TestConfidenceDecay_FreshArtifactIsNotTouched pins the MinAge gate: a
// just-saved artifact is not "decayed" the moment it exists.
func TestConfidenceDecay_FreshArtifactIsNotTouched(t *testing.T) {
	dm := NewTestDM(t)
	insertDecayMemory(t, dm, "mem-fresh", "memories", decayFixtureNow.Add(-time.Hour), InitialConfidence("memory"))

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}
	if res.Due != 0 || res.Scanned != 0 {
		t.Fatalf("fresh artifact selected: %+v", res)
	}
	conf, watermark := storedDecayConfidence(t, dm, "mem-fresh")
	if conf != InitialConfidence("memory") || watermark != nil {
		t.Errorf("fresh artifact modified: conf=%v watermark=%v", conf, watermark)
	}
}

// TestConfidenceDecay_RepeatedRunIsIdempotent pins restartability: once
// a pass has advanced the watermark, an immediate re-run does no work.
func TestConfidenceDecay_RepeatedRunIsIdempotent(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemory(t, dm, "mem-repeat", "memories", created, InitialConfidence("memory"))

	if _, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	historyAfterFirst := decayHistoryCount(t, dm, "mem-repeat")
	confAfterFirst, _ := storedDecayConfidence(t, dm, "mem-repeat")

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow.Add(time.Minute), Limit: 10})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Due != 0 || res.Scanned != 0 {
		t.Fatalf("second run did work: %+v", res)
	}
	confAfterSecond, _ := storedDecayConfidence(t, dm, "mem-repeat")
	if confAfterSecond != confAfterFirst {
		t.Errorf("second run changed confidence: %v -> %v", confAfterFirst, confAfterSecond)
	}
	if got := decayHistoryCount(t, dm, "mem-repeat"); got != historyAfterFirst {
		t.Errorf("second run appended history: %d -> %d", historyAfterFirst, got)
	}
}

// TestConfidenceDecay_BoundedByLimit pins the per-invocation boundary and
// the deferred backlog report.
func TestConfidenceDecay_BoundedByLimit(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	for _, id := range []string{"mem-bound-a", "mem-bound-b", "mem-bound-c"} {
		insertDecayMemory(t, dm, id, "memories", created, InitialConfidence("memory"))
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 2})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res.Due != 3 || res.Scanned != 2 || res.Deferred != 1 || res.Changed != 2 {
		t.Fatalf("first run = %+v, want due=3 scanned=2 deferred=1 changed=2", res)
	}

	res, err = dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow.Add(time.Minute), Limit: 2})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Due != 1 || res.Scanned != 1 || res.Deferred != 0 || res.Changed != 1 {
		t.Fatalf("second run = %+v, want due=1 scanned=1 deferred=0 changed=1", res)
	}

	res, err = dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow.Add(2 * time.Minute), Limit: 2})
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if res.Due != 0 {
		t.Errorf("third run still has backlog: %+v", res)
	}
}

// TestConfidenceDecay_UnchangedResultWritesNoHistory pins the
// no-write-storm rule: when the derived value matches the stored value,
// the watermark advances but no confidence_history row is appended.
func TestConfidenceDecay_UnchangedResultWritesNoHistory(t *testing.T) {
	dm := NewTestDM(t)

	// A decision's initial log-odds are log(0.6/0.4) = 0.4054651; at
	// λ=0.001/day, that is exactly 405.4651 days of decay, which derives
	// 0.5. Storing 0.5 makes the pass a no-op computation.
	secondsFloat := 405.4651081081644 * 24 * 3600
	seconds := int64(secondsFloat)
	created := decayFixtureNow.Add(-time.Duration(seconds) * time.Second)
	insertDecayMemory(t, dm, "dec-unchanged", "decisions", created, 0.5)

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}
	if res.Scanned != 1 || res.Unchanged != 1 || res.Changed != 0 {
		t.Fatalf("result = %+v, want scanned=1 unchanged=1 changed=0", res)
	}
	if n := decayHistoryCount(t, dm, "dec-unchanged"); n != 0 {
		t.Errorf("unchanged artifact appended %d history rows", n)
	}
	if _, watermark := storedDecayConfidence(t, dm, "dec-unchanged"); watermark == nil {
		t.Error("unchanged artifact did not advance its watermark; it would be re-selected forever")
	}
}

// TestConfidenceDecay_PartialFailureRetriesOnlyTheFailedArtifact pins
// the interrupted-pass contract using an injected database error.
func TestConfidenceDecay_PartialFailureRetriesOnlyTheFailedArtifact(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemory(t, dm, "mem-ok", "memories", created, InitialConfidence("memory"))
	insertDecayMemory(t, dm, "mem-fail", "memories", created, InitialConfidence("memory"))

	if _, err := dm.SQLDB().Exec(`
		CREATE TRIGGER inject_conf_update_failure
		BEFORE UPDATE OF confidence ON memories
		WHEN old.id = 'mem-fail'
		BEGIN SELECT RAISE(ABORT, 'injected confidence write failure'); END;
	`); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}
	if res.Failed != 1 || res.Changed != 1 || len(res.Failures) != 1 {
		t.Fatalf("result = %+v, want failed=1 changed=1 with one failure message", res)
	}
	if !strings.Contains(res.Failures[0], "mem-fail") {
		t.Errorf("failure does not name the artifact: %q", res.Failures[0])
	}
	// The successful artifact committed; the failed one did not advance
	// its watermark, so the next pass retries exactly it.
	if _, wm := storedDecayConfidence(t, dm, "mem-ok"); wm == nil {
		t.Error("successful artifact watermark not advanced")
	}
	if conf, wm := storedDecayConfidence(t, dm, "mem-fail"); wm != nil || conf != InitialConfidence("memory") {
		t.Errorf("failed artifact was partially written: conf=%v watermark=%v", conf, wm)
	}

	if _, err := dm.SQLDB().Exec(`DROP TRIGGER inject_conf_update_failure`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	res, err = dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow.Add(time.Minute), Limit: 10})
	if err != nil {
		t.Fatalf("retry run: %v", err)
	}
	if res.Due != 1 || res.Scanned != 1 || res.Changed != 1 || res.Failed != 0 {
		t.Fatalf("retry = %+v, want only the previously failed artifact", res)
	}
}

// TestConfidenceDecay_IncludesEvidenceAddedBetweenRuns pins that a pass
// never overwrites a fresher evidence-derived value with a stale one:
// evidence added between runs is part of the next computation.
func TestConfidenceDecay_IncludesEvidenceAddedBetweenRuns(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemory(t, dm, "mem-ev-a", "memories", created, InitialConfidence("memory"))
	insertDecayMemory(t, dm, "mem-ev-b", "memories", created, InitialConfidence("memory"))

	if _, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 1}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Add strong evidence to the artifact the bounded first pass did not
	// reach. AddEvidence recomputes immediately; the next decay pass must
	// include it rather than reverting to a decay-only value.
	if _, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:         "mem-ev-b",
		ArtifactType:       "memory",
		Type:               "observation",
		SourceGroup:        "manual_review",
		Strength:           1.0,
		IndependenceFactor: 1.0,
		CreatedBy:          "decay-test",
		CreatedAt:          decayFixtureNow.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("add evidence: %v", err)
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow.Add(time.Minute), Limit: 10})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Due != 1 || res.Scanned != 1 {
		t.Fatalf("second run = %+v, want the one remaining artifact", res)
	}
	got, _ := storedDecayConfidence(t, dm, "mem-ev-b")
	want := computeConfidence("memory", []evidenceInput{{
		Strength: 1.0, Independence: 1.0, CreatedAt: decayFixtureNow.Add(-time.Hour),
	}}, decayFixtureNow.Add(time.Minute), decayFixtureNow.Add(-time.Hour), 0.005)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("confidence after evidence = %.12f, want %.12f", got, want)
	}
}

// TestConfidenceDecay_DryRunWritesNothing pins the preview contract.
func TestConfidenceDecay_DryRunWritesNothing(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemory(t, dm, "mem-dry", "memories", created, InitialConfidence("memory"))

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.Due != 1 || res.Scanned != 1 || res.Changed != 1 || res.DryRun != true {
		t.Fatalf("dry run = %+v, want due=1 scanned=1 changed=1 dry_run", res)
	}
	conf, watermark := storedDecayConfidence(t, dm, "mem-dry")
	if conf != InitialConfidence("memory") || watermark != nil {
		t.Errorf("dry run wrote state: conf=%v watermark=%v", conf, watermark)
	}
	if n := decayHistoryCount(t, dm, "mem-dry"); n != 0 {
		t.Errorf("dry run wrote %d history rows", n)
	}
}

// TestConfidenceDecay_SkipsNonArtifactRows pins the participant set:
// directives, soft-deleted rows, retired global rules and non-artifact
// collections are never decayed.
func TestConfidenceDecay_SkipsNonArtifactRows(t *testing.T) {
	dm := NewTestDM(t)
	old := decayFixtureNow.Add(-200 * 24 * time.Hour)

	insertDecayMemory(t, dm, "mem-directive", "directives", old, InitialConfidence("memory"))
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET is_prime_directive = 1 WHERE id = 'mem-directive'`); err != nil {
		t.Fatalf("mark directive: %v", err)
	}
	insertDecayMemory(t, dm, "mem-deleted", "memories", old, InitialConfidence("memory"))
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET deleted_at = ? WHERE id = 'mem-deleted'`, decayFixtureNow.Unix()); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	insertDecayMemory(t, dm, "mem-retired", "memories", old, InitialConfidence("memory"))
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET retired_at = ? WHERE id = 'mem-retired'`, decayFixtureNow.Unix()); err != nil {
		t.Fatalf("retire: %v", err)
	}
	insertDecayMemory(t, dm, "mem-other-collection", "notes", old, InitialConfidence("memory"))

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}
	if res.Due != 0 || res.Scanned != 0 {
		t.Fatalf("non-artifact rows were selected: %+v", res)
	}
}

// TestConfidenceDecay_DirectivesAreExcludedByTheCanonicalPredicate pins
// the participant set against the CANONICAL directive predicate.
//
// A row is a directive when EITHER identifier is set:
// collection='directives' (the canonical path) or is_prime_directive=1
// (the legacy column-based path). The participant query used to spell
// that exclusion inline, and it happened to be correct — but only by
// coincidence: a row with collection='directives' and
// is_prime_directive=0 was excluded by the COLLECTION ALLOW-LIST, not
// by the directive predicate.
//
// This test exercises all four combinations:
//
//	collection='directives', is_prime_directive=0  -> excluded
//	collection='directives', is_prime_directive=1  -> excluded
//	collection='memories',    is_prime_directive=1  -> excluded
//	collection='memories',    is_prime_directive=0  -> A PARTICIPANT
//
// The last row is the control: it is the same shape as the third, with
// the directive marker removed, and it MUST be selected. Without it,
// "the directive rows were excluded" could be true because nothing was
// selected at all.
//
// # WHAT THIS TEST DOES AND DOES NOT PROVE
//
// It pins the BEHAVIOUR: every directive shape is excluded and the
// non-directive row is not. The previous coverage marked a directive
// with BOTH identifiers, so neither identifier was exercised on its
// own.
//
// It does NOT distinguish the canonical clause from an equivalent
// inline copy — and it could not. The two are semantically identical
// today, and that is the point: the change from the inline copy to
// DirectiveExclusionClause buys the absence of a DRIFT RISK, not a
// behaviour difference. No behavioural test can prove the absence of a
// drift risk; what it can do is pin today's behaviour, so that if the
// two ever stop agreeing, this test says so.
func TestConfidenceDecay_DirectivesAreExcludedByTheCanonicalPredicate(t *testing.T) {
	dm := NewTestDM(t)
	old := decayFixtureNow.Add(-200 * 24 * time.Hour)

	cases := []struct {
		id           string
		collection   string
		primeFlag    int
		wantSelected bool
	}{
		{"mem-dir-canonical-only", "directives", 0, false},
		{"mem-dir-both", "directives", 1, false},
		{"mem-dir-legacy-only", "memories", 1, false},
		{"mem-not-a-directive", "memories", 0, true},
	}
	for _, tc := range cases {
		insertDecayMemory(t, dm, tc.id, tc.collection, old, InitialConfidence("memory"))
		if _, err := dm.SQLDB().Exec(
			`UPDATE memories SET is_prime_directive = ? WHERE id = ?`, tc.primeFlag, tc.id); err != nil {
			t.Fatalf("mark %s: %v", tc.id, err)
		}
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}

	if res.Due != 1 || res.Scanned != 1 {
		t.Fatalf("participant set = %d due / %d scanned, want 1 / 1 — exactly the "+
			"one row that is not a directive (result %+v)", res.Due, res.Scanned, res)
	}

	// And the control row is the one that was touched.
	var checked int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id LIKE 'mem-%' AND confidence_checked_at IS NOT NULL`,
	).Scan(&checked); err != nil {
		t.Fatalf("count checked: %v", err)
	}
	if checked != 1 {
		t.Errorf("checked %d row(s), want 1 — the three directive rows must not be "+
			"recomputed, and the control row must be", checked)
	}
}

// TestConfidenceDecay_BacklogMatchesDue pins the diagnostics helper.
func TestConfidenceDecay_BacklogMatchesDue(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemory(t, dm, "mem-backlog", "memories", created, InitialConfidence("memory"))

	n, err := dm.ConfidenceDecayBacklog(decayFixtureNow, 24*time.Hour)
	if err != nil {
		t.Fatalf("backlog: %v", err)
	}
	if n != 1 {
		t.Errorf("backlog = %d, want 1", n)
	}
}
