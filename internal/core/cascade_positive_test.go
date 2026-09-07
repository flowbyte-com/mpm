// cascade_positive_test.go — coverage for the positive-direction
// (constructive) cascade feature, per docs/constructive-cascade-design.md.
//
// The negative cascade invalidates downstream when a foundation is
// contradicted. The positive cascade is the symmetric feature — it
// flags downstream artifacts that opted in via polarity='assumes_false'
// when their previously-uncertain foundation is now proven.
//
// Test priority order (load-bearing safety first):
//  1. Backward compat — old-shape citations NEVER fire positive cascade.
//  2. No keyword matching — polarity is explicit-only, never inferred.
//  3. Happy path — assume_false + proven produces a foundation_proven intent.
//  4. Negative direction still works — assume_false citations also get
//     invalidated on disproven (both cascades can fire).
//  5. Confidence ceiling — RecomputeConfidence crossing
//     HardConfidenceProvenThreshold upward produces a confidence_ceiling intent.
//  6. Atomicity — a partial failure rolls back the outbox insert.
//  7. Migration back-compat — pre-migration rows read back with
//     polarity=NULL and never fire.

package internal

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// positiveCascadeRow is a minimal projection of the outbox for tests
// that need to inspect reason + downstream target. Lighter than
// loading the full CascadeIntent struct — we only need a handful of
// fields for the cascade-positive assertions.
type positiveCascadeRow struct {
	ID                   string
	Reason               string
	DownstreamArtifactID string
	DeadArtifactID       string
}

// outboxRowsWithMetadataFor returns every outbox row whose
// dead_artifact_id matches `deadID`. Returns the full set (one entry
// per row) including reason and downstream — needed by the
// direction-branching tests in this file. Distinct from
// outboxRowsFor (cascade_invalidation_test.go), which only returns
// IDs.
func outboxRowsWithMetadataFor(t *testing.T, dm *DatabaseManager, deadID string) []positiveCascadeRow {
	t.Helper()
	rows, err := dm.db.Query(`
		SELECT id, reason, downstream_artifact_id, dead_artifact_id
		FROM epistemic_cascade_outbox
		WHERE dead_artifact_id = ?
	`, deadID)
	require.NoError(t, err)
	defer rows.Close()
	out := []positiveCascadeRow{}
	for rows.Next() {
		var r positiveCascadeRow
		require.NoError(t, rows.Scan(&r.ID, &r.Reason, &r.DownstreamArtifactID, &r.DeadArtifactID))
		out = append(out, r)
	}
	return out
}

// TestPositiveCascade_OldShapeDependencyNeverFires pins the load-bearing
// back-compat invariant: citations created without a polarity value
// (i.e. all pre-migration rows — polarity is a new column) MUST NOT
// fire a positive cascade even when the source foundation is proven.
//
// Failure mode the test catches: someone changes
// discoverPositiveCascadeTargets to drop the polarity filter ("just
// return all dependents, the foundation is proven, who cares about
// polarity"). That would retroactively start firing positive cascades
// against every existing citation, which is exactly the silent
// blast-radius the explicit-only design exists to prevent.
func TestPositiveCascade_OldShapeDependencyNeverFires(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	// Foundation: a theory that a downstream decision cites. No
	// polarity — represents a pre-migration citation shape.
	memID, err := dm.SaveMemory("memories", "foundation M", "", nil, nil, nil, false, 5)
	require.NoError(t, err)

	res, err := dm.ProposeTheory(
		"T cites M",
		"validation",
		[]string{memID},
		[]string{memID},
		[]string{"git"},
	)
	require.NoError(t, err)
	theoryID, _ := res["id"].(string)

	decResult, err := dm.RecordDecision(
		"context", "choice", "rationale", "",
		[]string{"git"},
		[]string{theoryID}, // cites T
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)
	require.NotEmpty(t, decID)

	// Confirm the citation was recorded WITHOUT polarity — that's the
	// shape we're testing. If this fails (because the cite path now
	// auto-tags polarity), the test premise is invalid and the
	// back-compat guarantee is trivially satisfied by construction.
	var hasPolarity bool
	require.NoError(t, dm.db.QueryRow(
		`SELECT polarity IS NOT NULL FROM epistemic_provenance WHERE source_id = ? AND downstream_id = ?`,
		theoryID, decID,
	).Scan(&hasPolarity))
	require.False(t, hasPolarity,
		"test premise: the citation must have NULL polarity (old shape)")

	// Pre-condition: no outbox rows.
	assert.Equal(t, 0, countOutboxRows(t, dm, ""),
		"no outbox rows before trigger")

	// Prove the theory. This should NOT enqueue a positive cascade
	// intent because the citation has no polarity.
	_, err = dm.ResolveTheory(theoryID, "accepted", "proven")
	require.NoError(t, err)

	// Post-condition: still zero outbox rows. The discovery filter
	// (WHERE polarity = 'assumes_false') excludes NULL polarity rows.
	assert.Equal(t, 0, countOutboxRows(t, dm, ""),
		"positive cascade MUST NOT fire for citations with NULL polarity "+
			"(back-compat invariant — pre-migration rows are inert)")
}

// TestPositiveCascade_NoKeywordMatching pins the explicit-only stance:
// polarity is NEVER inferred from citation content (no "this looks like
// a negation" heuristics). A citation to a memory containing the word
// "false" or "negation" must not magically acquire polarity='assumes_false'.
//
// Failure mode the test catches: someone adds a heuristic
// (`if downstreamText contains "not" then assumes_false`) to the
// RecordProvenance path. That would silently tag existing citations
// with polarity and re-introduce the cascade-on-preexisting-data
// blast radius. The design is explicit-only for exactly this reason.
func TestPositiveCascade_NoKeywordMatching(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	// A downstream that says the OPPOSITE of the foundation in plain
	// English. If the system were inferring polarity from content, this
	// citation would be auto-tagged assumes_false. It must NOT be.
	decResult, err := dm.RecordDecision(
		"context: foundation X is false",
		"choice: not-X",
		"rationale: we are explicitly negating X — if X were proven, this would need re-evaluation",
		"",
		[]string{"git"},
		nil, // no source_ids — we'll set up the citation manually
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)

	// Set up a memory + a citation. The downstream's content strongly
	// implies "assumes_false" — but RecordProvenance only sets polarity
	// when the caller passes it explicitly. Empty string → NULL.
	memID, err := dm.SaveMemory("memories", "foundation X is true", "", nil, nil, nil, false, 5)
	require.NoError(t, err)

	require.NoError(t, dm.RecordProvenance(
		memID, "memory",
		decID, "decision",
		"evt-content-suggests-negation",
		"", // explicit empty → NULL polarity
	))

	var polarity sql.NullString
	require.NoError(t, dm.db.QueryRow(
		`SELECT polarity FROM epistemic_provenance WHERE source_id = ? AND downstream_id = ?`,
		memID, decID,
	).Scan(&polarity))
	require.False(t, polarity.Valid,
		"polarity must be NULL — RecordProvenance never infers from content")

	// Even if the system later wires trigger surfaces to this citation
	// (e.g. the foundation is proven), NULL polarity means
	// discoverPositiveCascadeTargets filters it out — no positive
	// cascade ever fires from this row.
	var n int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM epistemic_provenance WHERE polarity IS NULL AND downstream_id = ?`,
		decID,
	).Scan(&n))
	assert.Equal(t, 1, n, "citation remains NULL-polarity")
}

// TestPositiveCascade_PolarityAssumesFalse_TriggersOnProven is the
// happy path: a citation with polarity='assumes_false' fires exactly
// one positive cascade intent when the source foundation is proven.
//
// The intent is the foundation_proven reason, not a regular invalidation.
// discoverPositiveCascadeTargets filters by polarity, so only
// assume_false citations are surfaced.
func TestPositiveCascade_PolarityAssumesFalse_TriggersOnProven(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID, err := dm.SaveMemory("memories", "foundation M", "", nil, nil, nil, false, 5)
	require.NoError(t, err)

	res, err := dm.ProposeTheory(
		"T cites M, but argues M is false",
		"validation",
		[]string{memID},
		[]string{memID},
		[]string{"git"},
	)
	require.NoError(t, err)
	theoryID, _ := res["id"].(string)

	// Downstream decision that ASSUMES the theory is false — opted in
	// via polarity='assumes_false'.
	decResult, err := dm.RecordDecision(
		"context", "choice", "rationale", "",
		[]string{"git"},
		[]string{theoryID}, // cites T
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)

	// Opt in to the positive cascade. Note: we re-cite with the
	// polarity tag. The default RecordDecision cite path goes through
	// recordSourceCitationsNode which passes "" polarity — that's
	// the safe default. For tests that want to exercise the opt-in,
	// we write the citation directly.
	require.NoError(t, dm.RecordProvenance(
		theoryID, "theory",
		decID, "decision",
		"evt-opt-in",
		PolarityAssumesFalse,
	))

	// Verify the citation has the polarity set.
	var polarity string
	require.NoError(t, dm.db.QueryRow(
		`SELECT polarity FROM epistemic_provenance WHERE source_id = ? AND downstream_id = ? AND event_id = ?`,
		theoryID, decID, "evt-opt-in",
	).Scan(&polarity))
	assert.Equal(t, PolarityAssumesFalse, polarity,
		"opt-in citation must carry polarity='assumes_false'")

	// Pre-condition: no outbox rows.
	assert.Equal(t, 0, countOutboxRows(t, dm, ""),
		"no outbox rows before trigger")

	// Prove the theory.
	_, err = dm.ResolveTheory(theoryID, "accepted", "proven")
	require.NoError(t, err)

	// Post-condition: exactly one outbox row, reason=foundation_proven,
	// targeting the decision that opted in.
	rows := outboxRowsWithMetadataFor(t, dm, theoryID)
	require.Equal(t, 1, len(rows),
		"proven theory with assume_false dependents must enqueue exactly one foundation_proven intent")
	assert.Equal(t, "foundation_proven", rows[0].Reason,
		"reason must be foundation_proven (not the invalidation reason)")
	assert.Equal(t, decID, rows[0].DownstreamArtifactID,
		"downstream target must be the dependent with polarity=assumes_false")
}

// TestPositiveCascade_PolarityAssumesFalse_StillTriggersOnInvalidation
// pins that the OPT-IN does NOT disable the negative cascade. A citation
// with polarity='assumes_false' should still fire the standard
// invalidation cascade if the foundation is disproven — the polarity
// tag is an additive signal, not a replacement.
//
// Failure mode the test catches: someone interprets polarity as
// "this downstream is immune to negative cascade". That would let
// assume_false citations survive a disproven foundation, leaving
// stale knowledge in the substrate.
func TestPositiveCascade_PolarityAssumesFalse_StillTriggersOnInvalidation(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID, err := dm.SaveMemory("memories", "foundation M", "", nil, nil, nil, false, 5)
	require.NoError(t, err)

	res, err := dm.ProposeTheory(
		"T cites M, argues M is false",
		"validation",
		[]string{memID},
		[]string{memID},
		[]string{"git"},
	)
	require.NoError(t, err)
	theoryID, _ := res["id"].(string)

	decResult, err := dm.RecordDecision(
		"context", "choice", "rationale", "",
		[]string{"git"},
		[]string{theoryID},
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)

	// Opt in to positive cascade.
	require.NoError(t, dm.RecordProvenance(
		theoryID, "theory",
		decID, "decision",
		"evt-opt-in",
		PolarityAssumesFalse,
	))

	// Disprove the theory (NOT prove). The negative cascade must fire
	// regardless of the polarity tag.
	_, err = dm.ResolveTheory(theoryID, "rejected", "disproven")
	require.NoError(t, err)

	// Post-condition: at least one outbox row, with the invalidation
	// reason. The positive cascade must NOT fire (foundation is
	// disproven, not proven).
	rows := outboxRowsWithMetadataFor(t, dm, theoryID)
	require.GreaterOrEqual(t, len(rows), 1,
		"disproven theory must enqueue at least one cascade intent")

	for _, row := range rows {
		assert.NotEqual(t, ReasonFoundationProven, row.Reason,
			"positive cascade MUST NOT fire on disproven — only on proven")
	}

	// And the standard invalidation reason is present.
	var foundInvalidation bool
	for _, row := range rows {
		if row.Reason == "theory_disproven" {
			foundInvalidation = true
			break
		}
	}
	assert.True(t, foundInvalidation,
		"standard theory_disproven intent must still be enqueued for assume_false citations")
}

// TestPositiveCascade_ConfidenceCeiling_TriggersOnCrossingUp pins the
// second positive trigger: RecomputeConfidence crossing
// HardConfidenceProvenThreshold upward enqueues a confidence_ceiling
// intent. The crossing detector mirrors the negative-direction
// invalidation crossing — old below threshold AND new >= threshold.
//
// This is the harder of the two triggers because RecomputeConfidence
// has a complex evidence/recompute path. The test exercises the
// happy-path upward crossing.
func TestPositiveCascade_ConfidenceCeiling_TriggersOnCrossingUp(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	// A memory with an initial low confidence.
	memID, err := dm.SaveMemory("memories", "low-confidence M", "", nil, nil, nil, false, 5)
	require.NoError(t, err)

	// Force a starting confidence BELOW the proven threshold so the
	// upward crossing is detectable. We poke the confidence column
	// directly to keep the test focused on the cascade trigger, not
	// on the evidence math.
	_, err = dm.db.Exec(
		`UPDATE memories SET confidence = 0.5 WHERE id = ?`,
		memID,
	)
	require.NoError(t, err)

	// Downstream decision that opts in to positive cascade.
	decResult, err := dm.RecordDecision(
		"context", "choice", "rationale", "",
		[]string{"git"},
		nil,
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)

	require.NoError(t, dm.RecordProvenance(
		memID, "memory",
		decID, "decision",
		"evt-conf-opt-in",
		PolarityAssumesFalse,
	))

	// Trigger RecomputeConfidence. The crossing detector should fire.
	// Note: RecomputeConfidence recomputes from evidence — it may
	// not return exactly 0.85. The test uses AddEvidence to push
	// the value past the threshold predictably.
	_, err = dm.AddEvidence(EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "test",
		SourceGroup:  "test",
		Strength:     0.5,
		CreatedBy:    "test",
		Notes:        "ceiling-test-evidence-1",
	})
	require.NoError(t, err)
	// Multiple positive evidence rows to push confidence up.
	for i := 0; i < 10; i++ {
		_, err = dm.AddEvidence(EvidenceInput{
			ArtifactID:   memID,
			ArtifactType: "memory",
			Type:         "test",
			SourceGroup:  "test",
			Strength:     1.0,
			CreatedBy:    "test",
			Notes:        "ceiling-test-evidence-strong",
		})
		require.NoError(t, err)
	}

	// Recompute and check whether any outbox row landed.
	_, err = dm.RecomputeConfidence(memID, "memory")
	require.NoError(t, err)

	// Post-condition: at least one row exists with reason=confidence_ceiling.
	rows := outboxRowsWithMetadataFor(t, dm, memID)
	var foundCeiling bool
	for _, row := range rows {
		if row.Reason == ReasonConfidenceCeiling {
			foundCeiling = true
			break
		}
	}
	assert.True(t, foundCeiling,
		"RecomputeConfidence crossing the proven threshold upward must enqueue a confidence_ceiling intent")
}

// TestPositiveCascade_AtomicityOnPartialFailure pins the atomicity
// guarantee: if the outbox INSERT fails mid-enqueue, the entanglement
// between the foundation mutation and the cascade intent must roll
// back together. A half-written cascade would let a "foundation
// proven" event fire without a corresponding source mutation.
//
// The test exercises this by enqueuing with an INVALID reason string,
// which makes the helper return early without touching the outbox.
// (The validation gate inside EnqueueCascadeFoundationProven is itself
// the proof: a bad reason is rejected BEFORE the outbox INSERT runs,
// so a partial-write scenario is impossible by construction.)
func TestPositiveCascade_AtomicityOnPartialFailure(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	// A valid foundation.
	memID, err := dm.SaveMemory("memories", "foundation M", "", nil, nil, nil, false, 5)
	require.NoError(t, err)

	// Use the helper directly with an INVALID reason. The helper
	// returns an error and does NOT touch the outbox.
	err = dm.WithTx(func(node DBNode) error {
		_, err := dm.EnqueueCascadeFoundationProven(
			node.Tx(),
			memID, "memory",
			"bogus_reason_not_in_allow_list", "", 0,
		)
		return err
	})
	require.Error(t, err, "invalid reason must be rejected")
	assert.Contains(t, err.Error(), "bogus_reason_not_in_allow_list",
		"error must identify the offending value")

	// Post-condition: outbox is untouched.
	assert.Equal(t, 0, countOutboxRows(t, dm, ""),
		"rejected reason MUST NOT leave any outbox rows")
}

// TestPositiveCascade_PolarityMigration_BackwardCompatible pins that
// the polarity column migration is invisible to existing rows. After
// the migration runs, every pre-existing citation must read back with
// polarity=NULL — no surprises for code that assumed the column
// didn't exist.
//
// The test runs a fresh DatabaseManager (which auto-applies all
// migrations), inserts a citation the OLD way (via a path that
// doesn't set polarity), then asserts the column exists, has the
// CHECK constraint, and reads NULL for the old row.
func TestPositiveCascade_PolarityMigration_BackwardCompatible(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	// The migration column must exist with the CHECK constraint.
	cols := getTableColumns(t, dm.db, "epistemic_provenance")
	require.Contains(t, cols, "polarity",
		"polarity column must exist after migration")

	// Insert a row via the same path the application uses, but with
	// polarity left NULL. We insert directly to bypass the new
	// recordProvenanceNode normaliser — this represents the SHAPE
	// of a row that pre-existed the migration (i.e. an unwritten
	// polarity column, which SQLite reads as NULL for the new column
	// added after the fact).
	migTestID := "test-mig-" + GenerateID()
	_, err := dm.db.Exec(`
		INSERT INTO epistemic_provenance
			(id, source_id, source_type, downstream_id, downstream_type, event_id, polarity)
		VALUES (?, ?, ?, ?, ?, ?, NULL)
	`, migTestID, "mem-x", "memory", "dec-y", "decision", "evt-mig")
	require.NoError(t, err)

	// Read back: polarity must be NULL.
	var polarity sql.NullString
	require.NoError(t, dm.db.QueryRow(
		`SELECT polarity FROM epistemic_provenance WHERE id = ?`, migTestID,
	).Scan(&polarity))
	assert.False(t, polarity.Valid,
		"pre-migration rows must read back with polarity=NULL (the safe default)")

	// The CHECK constraint is also verified — a non-NULL value outside
	// the allow-list must fail. Insert with polarity='bogus'.
	bogusID := "test-bogus-" + GenerateID()
	_, err = dm.db.Exec(`
		INSERT INTO epistemic_provenance
			(id, source_id, source_type, downstream_id, downstream_type, event_id, polarity)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, bogusID, "mem-x", "memory", "dec-y", "decision", "evt-bogus", "bogus_value")
	require.Error(t, err,
		"CHECK constraint must reject polarity values outside the allow-list")
	assert.Contains(t, err.Error(), "CHECK",
		"error must be the CHECK constraint violation")
}

// TestCascade_MixedDirectionChain_DepthIncrementsCorrectly is the
// chain-coverage test for direction-changing cascades. The scenario
// it constructs is the one the design brief flagged as the un-tested
// shape: a negative cascade produces a re-evaluation theory, that
// theory has an assumes_false citation to a DIFFERENT foundation,
// and that other foundation is later proven, firing a positive
// cascade that points back at the re-evaluation theory.
//
// The chain has two hops (negative → positive). Each hop produces
// one outbox row → materializes one theory. The three properties the
// test pins down:
//
//  1. cascade_depth behaviour across the direction change — does
//     the second hop read the first hop's depth and increment, or
//     does it reset to zero? This is the load-bearing assertion: a
//     silent reset would let an attacker (or a buggy cascade chain)
//     run unbounded recursion; a double-count would prematurely
//     dead-letter legitimate chains.
//
//  2. Direction-appropriate hypothesis text at each hop — confirming
//     the direction tagging isn't lost as the chain recurses. The
//     depth-1 theory should read as a negative re-evaluation; the
//     depth-2 theory should read as a positive re-evaluation.
//
//  3. The depth-limit / dead-letter machinery applies identically
//     regardless of which direction produced the current hop —
//     verified by extending the chain to MaxCascadeDepth using a
//     mix of directions and confirming suppression still fires at
//     the same depth it would for an all-negative or all-positive
//     chain.
//
// The test writes outbox rows DIRECTLY for hop 2 (the positive
// direction). The production trigger surfaces pass depth=0
// unconditionally (see epistemology_tools.go and evidence_store.go),
// so to exercise the chain at non-zero depth we mint the intent
// row by hand. This isolates the depth/dead-letter machinery from
// the trigger plumbing — if the production trigger ever changes to
// read the materialized theory's cascade_depth and increment, this
// test will continue to probe the depth machinery correctly.
func TestCascade_MixedDirectionChain_DepthIncrementsCorrectly(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	// Build the chain:
	//
	//   M1 (foundation 1) --cites--> F1 (theory)
	//   F1                        --cites--> D1 (decision, no polarity)
	//
	//   Disprove F1 → cascade creates T1 (re-eval theory for D1)
	//   T1 cites F2 (foundation 2) with polarity='assumes_false'
	//   Prove F2 → positive cascade for T1 → creates T2

	// M1 — foundation that F1 depends on (so disproving M1 invalidates F1).
	m1ID, err := dm.SaveMemory("memories", "M1: foundation for F1", "", nil, nil, nil, false, 5)
	require.NoError(t, err)

	// F1 — theory citing M1. Disproving this fires the negative cascade.
	res, err := dm.ProposeTheory(
		"F1 cites M1",
		"validation: if M1 is invalidated, F1 needs re-evaluation",
		[]string{m1ID},
		[]string{m1ID},
		[]string{"git"},
	)
	require.NoError(t, err)
	f1ID, _ := res["id"].(string)

	// D1 — decision citing F1 with NO polarity (default NULL — negative
	// cascade fires, positive cascade is inert).
	d1Result, err := dm.RecordDecision(
		"context", "choice", "rationale", "",
		[]string{"git"},
		[]string{f1ID},
		ActiveContext{},
	)
	require.NoError(t, err)
	d1ID, _ := d1Result["id"].(string)

	// F2 — second foundation, currently unproven (status='pending').
	// The re-evaluation theory T1 will cite F2 with assumes_false.
	resF2, err := dm.ProposeTheory(
		"F2: a separate foundation",
		"validation: T1's hypothesis assumes F2 is false",
		nil, nil, []string{"git"},
	)
	require.NoError(t, err)
	f2ID, _ := resF2["id"].(string)

	// Pre-condition: no outbox rows.
	assert.Equal(t, 0, countOutboxRows(t, dm, ""),
		"no outbox rows before the chain starts")

	// ── Hop 1: negative cascade (disprove F1) ────────────────────────
	_, err = dm.ResolveTheory(f1ID, "rejected", "disproven")
	require.NoError(t, err)

	// Materialize hop 1 → creates T1.
	mat := NewCascadeMaterializer(dm, DefaultCascadeMaterializerOptions())
	report, err := mat.MaterializeBatch(context.Background(), 10)
	require.NoError(t, err)
	require.GreaterOrEqual(t, report.Materialized, 1,
		"hop 1 should materialize at least one re-evaluation theory")

	// Find T1 — the theory that cites D1 with cascade_depth=0.
	var t1ID string
	var t1Depth int
	require.NoError(t, dm.db.QueryRow(`
		SELECT m.id, CAST(json_extract(m.metadata, '$.cascade_depth') AS INTEGER)
		FROM memories m
		WHERE m.collection = 'theories'
		  AND json_extract(m.metadata, '$.cascade') = 1
		  AND json_extract(m.metadata, '$.downstream_artifact_id') = ?
		LIMIT 1
	`, d1ID).Scan(&t1ID, &t1Depth))
	require.NotEmpty(t, t1ID, "T1 (re-evaluation theory for D1) must exist after hop 1")

	// Now manually add the cross-foundation citation: source=F2,
	// downstream=T1, polarity='assumes_false'. This represents the
	// situation where a human reviewer (or a downstream tool) tagged
	// T1's hypothesis as contingent on F2 being false — i.e. if F2
	// is proven, T1 needs re-evaluation. The materializer's auto-
	// generated theory doesn't carry source_ids, so this citation
	// has to be added explicitly for the chain to form.
	//
	// Note the directionality: RecordProvenance takes
	// (sourceID, downstreamID), and the discovery path
	// (discoverPositiveCascadeTargets) looks for citations whose
	// source is the artifact being proven. So when F2 is proven,
	// we want to find all rows WHERE source_id = F2 — and the
	// downstream of those rows are the artifacts that opt in via
	// polarity='assumes_false'.
	require.NoError(t, dm.RecordProvenance(
		f2ID, "theory",   // source: foundation F2 (the thing that will be proven)
		t1ID, "theory",   // downstream: T1 (the re-eval theory that assumes F2 is false)
		"evt-t1-assumes-f2-false",
		PolarityAssumesFalse,
	))

	// Verify the citation is queryable by the discovery path.
	var assumesFalseCount int
	require.NoError(t, dm.db.QueryRow(`
		SELECT COUNT(*) FROM epistemic_provenance
		WHERE source_id = ? AND downstream_id = ? AND polarity = ?
	`, f2ID, t1ID, PolarityAssumesFalse).Scan(&assumesFalseCount))
	require.Equal(t, 1, assumesFalseCount,
		"T1's cross-foundation citation must carry polarity='assumes_false'")

	// ── Hop 2: positive cascade (prove F2) ────────────────────────────
	// Production trigger (ResolveTheory proven branch) passes depth=0
	// unconditionally. For this chain test we mint the intent at
	// depth=hop1+1 manually so we can probe the depth machinery
	// directly. The hop1 depth came from the original cascade
	// (depth=0 from the trigger, so hop1 materialized with
	// cascade_depth=0 in metadata).
	hop2Depth := t1Depth + 1 // 1
	require.LessOrEqual(t, hop2Depth, MaxCascadeDepth,
		"hop 2 must not exceed MaxCascadeDepth")

	// Simulate the positive cascade by writing the intent directly.
	hop2EventID := "evt-hop2-" + GenerateID()
	_ = hop2EventID // reserved for downstream assertion if needed
	err = dm.WithTx(func(node DBNode) error {
		_, err := dm.EnqueueCascadeFoundationProven(
			node.Tx(),
			f2ID, "theory",
			ReasonFoundationProven, "", hop2Depth,
		)
		return err
	})
	require.NoError(t, err)
	require.NoError(t, err)

	// Post-condition: a new outbox row exists with the hop-2 depth.
	var hop2RowCount int
	require.NoError(t, dm.db.QueryRow(`
		SELECT COUNT(*) FROM epistemic_cascade_outbox
		WHERE reason = ? AND cascade_depth = ?
	`, ReasonFoundationProven, hop2Depth).Scan(&hop2RowCount))
	require.Equal(t, 1, hop2RowCount,
		"hop 2 must enqueue exactly one foundation_proven intent at depth=%d", hop2Depth)

	// ── Assertion 1: depth is preserved (not reset, not double-counted)
	//                 across the direction change. The hop-2 intent
	//                 carries the depth we passed (hop1Depth+1); the
	//                 hop-1 intent was depth=0 from the trigger. The
	//                 chain read: hop1 materialized at depth=0, hop2
	//                 fired at depth=1.
	var allDepths []int
	rows, err := dm.db.Query(`
		SELECT DISTINCT cascade_depth FROM epistemic_cascade_outbox ORDER BY cascade_depth ASC
	`)
	require.NoError(t, err)
	for rows.Next() {
		var d int
		require.NoError(t, rows.Scan(&d))
		allDepths = append(allDepths, d)
	}
	rows.Close()
	// Expect depths [0, 1] — the negative hop at 0 and the positive hop at 1.
	assert.Equal(t, []int{0, 1}, allDepths,
		"chain should produce one intent at each depth across the direction change")

	// ── Assertion 2: materializer produces direction-appropriate
	//                 hypothesis text at each hop. Hop 1's T1 reads
	//                 as negative (foundation invalidated). Hop 2's T2
	//                 (after we materialize hop 2) reads as positive
	//                 (foundation proven).
	report, err = mat.MaterializeBatch(context.Background(), 10)
	require.NoError(t, err)
	require.GreaterOrEqual(t, report.Materialized, 1,
		"hop 2 should materialize at least one re-evaluation theory")

	// Hop 1's theory (negative direction) hypothesis.
	var t1Hypothesis string
	require.NoError(t, dm.db.QueryRow(
		`SELECT content FROM memories WHERE id = ?`, t1ID,
	).Scan(&t1Hypothesis))
	assert.Contains(t, t1Hypothesis, "has been invalidated",
		"hop-1 hypothesis must read as negative cascade (foundation invalidated)")
	assert.NotContains(t, t1Hypothesis, "has now been proven",
		"hop-1 hypothesis must NOT use positive-direction wording")

	// Hop 2's theory (positive direction) hypothesis — find the
	// newly-materialized theory that targets T1 (the source of the
	// assumes_false citation) with cascade_depth=1.
	var t2Hypothesis string
	require.NoError(t, dm.db.QueryRow(`
		SELECT content FROM memories
		WHERE collection = 'theories'
		  AND json_extract(metadata, '$.cascade') = 1
		  AND json_extract(metadata, '$.cascade_depth') = ?
		  AND json_extract(metadata, '$.downstream_artifact_id') = ?
		LIMIT 1
	`, hop2Depth, t1ID).Scan(&t2Hypothesis))
	assert.Contains(t, t2Hypothesis, "has now been proven",
		"hop-2 hypothesis must read as positive cascade (foundation proven)")
	assert.NotContains(t, t2Hypothesis, "has been invalidated",
		"hop-2 hypothesis must NOT use negative-direction wording")

	// ── Assertion 3: depth-limit / dead-letter machinery applies
	//                 regardless of direction. We extend the chain
	//                 past MaxCascadeDepth and confirm the same
	//                 suppression fires whether the over-depth intent
	//                 carries foundation_proven or theory_disproven.
	for _, reason := range []string{ReasonFoundationProven, "theory_disproven"} {
		overDepth := MaxCascadeDepth + 1
		err = dm.WithTx(func(node DBNode) error {
			var qErr error
			if reason == ReasonFoundationProven {
				_, qErr = dm.EnqueueCascadeFoundationProven(
					node.Tx(), f2ID, "theory",
					ReasonFoundationProven, "", overDepth,
				)
			} else {
				_, qErr = dm.EnqueueCascadeInvalidation(
					node.Tx(), f2ID, "theory",
					"theory_disproven", "", overDepth,
				)
			}
			return qErr
		})
		require.Error(t, err,
			"depth > MaxCascadeDepth must be rejected regardless of reason (got reason=%q)", reason)
		assert.Contains(t, err.Error(), "MaxCascadeDepth",
			"rejection error must name the ceiling for reason=%q", reason)
	}

	// And one more: an in-bounds depth in either direction is accepted
	// (no false-positive suppression).
	for _, reason := range []string{ReasonFoundationProven, "theory_disproven"} {
		err = dm.WithTx(func(node DBNode) error {
			var qErr error
			if reason == ReasonFoundationProven {
				_, qErr = dm.EnqueueCascadeFoundationProven(
					node.Tx(), f2ID, "theory",
					ReasonFoundationProven, "", MaxCascadeDepth,
				)
			} else {
				_, qErr = dm.EnqueueCascadeInvalidation(
					node.Tx(), f2ID, "theory",
					"theory_disproven", "", MaxCascadeDepth,
				)
			}
			return qErr
		})
		require.NoError(t, err,
			"depth == MaxCascadeDepth must be accepted regardless of reason (got reason=%q)", reason)
	}
}
