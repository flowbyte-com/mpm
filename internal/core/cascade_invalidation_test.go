// cascade_invalidation_test.go — coverage for the explicit invalidation
// paths (Task 4 of the 2026-08-04 epistemic cascade plan).
//
// Three invalidation triggers are wired into the cascade outbox:
//
//  1. Disproven theory resolution — `ResolveTheory` with status="disproven"
//     enqueues one cascade intent per downstream decision/theory that cited
//     the disproven theory (via explicit `dependencies` JSON or typed
//     `epistemic_provenance` citations). Status="proven" and re-resolutions
//     of an already-disproven theory are NO-OPS at the outbox level.
//
//  2. Memory shred — both `ShredMemoryWithCascade` (the lesson-aware
//     wrapper used by `mpm call shred_memory`) and `ShredMemory` (the
//     standalone wrapper that fires stale-foundation wakes) enqueue one
//     cascade intent per downstream dependent inside the same tx as the
//     root DELETE. The lesson path remains non-cascading: lessons do not
//     have reasoning dependents.
//
//  3. Hard confidence invalidation — `RecomputeConfidence` enqueues a
//     cascade intent only when the artifact's confidence CROSSES below
//     the defined threshold. An ordinary decrease (from 0.7 to 0.3, both
//     above the threshold) is a NO-OP. An artifact already below the
//     threshold that drops further is also a NO-OP (the cross is the
//     event, not the absolute value).
//
// All three share atomicity: the root mutation and the cascade intents
// commit together. A failure on the outbox INSERT rolls back the root
// state change so the substrate never sees a "the memory is gone but no
// cascade was recorded" divergence.

package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countOutboxRows is a helper for tests asserting on outbox state.
// Returns the number of rows whose invalidation_event_id matches `eventID`.
// Passing eventID="" returns all rows.
func countOutboxRows(t *testing.T, dm *DatabaseManager, eventID string) int {
	t.Helper()
	var n int
	var err error
	if eventID == "" {
		err = dm.db.QueryRow(`SELECT COUNT(*) FROM epistemic_cascade_outbox`).Scan(&n)
	} else {
		err = dm.db.QueryRow(
			`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE invalidation_event_id = ?`,
			eventID,
		).Scan(&n)
	}
	require.NoError(t, err)
	return n
}

// outboxRowsFor returns all rows in the outbox whose dead_artifact_id
// matches the supplied id. Used to assert that a specific invalidation
// produced the expected number of intents.
func outboxRowsFor(t *testing.T, dm *DatabaseManager, deadID string) []string {
	t.Helper()
	rows, err := dm.db.Query(`
		SELECT id FROM epistemic_cascade_outbox WHERE dead_artifact_id = ?
	`, deadID)
	require.NoError(t, err)
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	return out
}

// ── ResolveTheory invalidation path ─────────────────────────────────────────

// TestInvalidation_DisproveTheoryEnqueuesIntents is the central test for
// the disprove path: a downstream decision that cited the theory via
// source_ids must surface as one cascade intent when the theory is
// resolved as disproven. The intent, the theory's status update, and the
// +1 reinforcement all commit inside one transaction.
func TestInvalidation_DisproveTheoryEnqueuesIntents(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	// Foundation memory M (the thing the theory depends on).
	memID, err := dm.SaveMemory(
		"memories",
		"foundation memory for disprove cascade",
		"", nil, nil, nil, false, 5,
	)
	require.NoError(t, err)

	// Theory T that depends on M (explicit dependencies JSON).
	res, err := dm.ProposeTheory(
		"T depends on M being true",
		"if M is shredded, T should be re-evaluated",
		[]string{memID},
		[]string{memID}, // source_ids also references M
		[]string{"git"},
	)
	require.NoError(t, err)
	theoryID, _ := res["id"].(string)
	require.NotEmpty(t, theoryID)

	// Downstream decision D that cites T via source_ids.
	decResult, err := dm.RecordDecision(
		"decision context",
		"decision choice",
		"rationale",
		"",
		[]string{"git"},
		[]string{theoryID}, // source_ids cites the theory
		ActiveContext{},
	)
	require.NoError(t, err)
	decID, _ := decResult["id"].(string)
	require.NotEmpty(t, decID)

	// Sanity: no outbox rows yet.
	assert.Equal(t, 0, countOutboxRows(t, dm, ""),
		"no outbox rows should exist before invalidation")

	// Resolve the theory as disproven.
	resolveResult, err := dm.ResolveTheory(theoryID, "rejected", "disproven")
	require.NoError(t, err)
	require.NotNil(t, resolveResult)
	assert.Equal(t, "disproven", resolveResult["status"])

	// Post-condition: the outbox has one intent targeting the decision.
	rows := outboxRowsFor(t, dm, theoryID)
	require.Equal(t, 1, len(rows),
		"disprove should enqueue exactly one cascade intent for the downstream decision")

	// Verify the intent's metadata round-trips.
	var deadType, downType, reason string
	require.NoError(t, dm.db.QueryRow(`
		SELECT dead_artifact_type, downstream_artifact_type, reason
		FROM epistemic_cascade_outbox WHERE dead_artifact_id = ?
	`, theoryID).Scan(&deadType, &downType, &reason))
	assert.Equal(t, "theory", deadType)
	assert.Equal(t, "decision", downType)
	assert.NotEmpty(t, reason, "reason field should be populated")

	// The decision itself is unchanged (cascade is an INTENT, not a
	// mutation). Materialization is the materializer's job (later task).
	decMem, err := dm.GetMemory(decID)
	require.NoError(t, err)
	assert.NotNil(t, decMem, "decision should still exist after disprove")
}

// TestInvalidation_ProvenTheoryDoesNotEnqueue pins the proven path:
// resolving as proven does NOT enqueue cascade intents. The brief
// ("only a transition to disproven creates an invalidation event") is
// load-bearing — a proven theory is still load-bearing, not invalidated.
func TestInvalidation_ProvenTheoryDoesNotEnqueue(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID, err := dm.SaveMemory("memories", "M", "", nil, nil, nil, false, 5)
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

	// Downstream decision cites T.
	decResult, err := dm.RecordDecision(
		"ctx", "choice", "rationale", "",
		[]string{"git"},
		[]string{theoryID},
		ActiveContext{},
	)
	require.NoError(t, err)
	_, _ = decResult["id"]

	// Resolve as PROVEN — must NOT enqueue.
	_, err = dm.ResolveTheory(theoryID, "confirmed", "proven")
	require.NoError(t, err)

	assert.Equal(t, 0, countOutboxRows(t, dm, ""),
		"proven resolution must not enqueue cascade intents")
}

// TestInvalidation_RepeatedDisproveDoesNotReEnqueue pins the
// idempotency contract: re-resolving a theory that is already disproven
// is a no-op at the outbox level. The first call enqueues; the second
// does not (the schema's UNIQUE constraint would collapse it anyway, but
// the brief asks us not to even attempt the enqueue).
func TestInvalidation_RepeatedDisproveDoesNotReEnqueue(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID, err := dm.SaveMemory("memories", "M", "", nil, nil, nil, false, 5)
	require.NoError(t, err)

	res, err := dm.ProposeTheory(
		"T", "v", []string{memID}, []string{memID}, []string{"git"},
	)
	require.NoError(t, err)
	theoryID, _ := res["id"].(string)

	_, err = dm.RecordDecision("ctx", "choice", "rationale", "",
		[]string{"git"}, []string{theoryID}, ActiveContext{})
	require.NoError(t, err)

	// First disprove: enqueues one intent.
	_, err = dm.ResolveTheory(theoryID, "rejected", "disproven")
	require.NoError(t, err)
	assert.Equal(t, 1, countOutboxRows(t, dm, ""),
		"first disprove should enqueue one intent")

	// Second disprove: must NOT add another intent.
	// Note: the current ResolveTheory implementation does not prevent
	// repeated disprove calls — but the brief asks us to make the
	// invalidation hook idempotent on the source state, not on the
	// schema. We test the invalidation hook contract directly.
	_, err = dm.ResolveTheory(theoryID, "rejected", "disproven")
	require.NoError(t, err, "repeated disprove should not error")
	assert.Equal(t, 1, countOutboxRows(t, dm, ""),
		"second disprove should NOT enqueue another cascade intent (idempotent)")
}

// ── Shred invalidation path ─────────────────────────────────────────────────

// TestInvalidation_ShredMemoryEnqueuesIntents pins the memory-path
// shred wrapper: a memory that two distinct downstream reasoning
// artifacts depend on must produce exactly one cascade intent per
// dependent when shredded via ShredMemoryWithCascade.
//
// Setup: foundation memory M, theory T (depends on M via both
// `dependencies` JSON and `source_ids`), and decision D (cites M
// directly via `source_ids`). When M is shredded, the cascade
// discovery surfaces BOTH T and D — T via both dependency
// surfaces (deduped), D via its source_id citation — yielding
// exactly two outbox intents. The tight Equal(2) catches a
// partial failure where one discovery path silently regresses
// (e.g., a future patch breaks the dependencies JSON scan but
// the provenance scan still works; an Equal(2) assertion fails
// the test rather than passing with a count of 1).
func TestInvalidation_ShredMemoryEnqueuesIntents(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID, err := dm.SaveMemory(
		"memories",
		"foundation memory for shred cascade",
		"", nil, nil, nil, false, 5,
	)
	require.NoError(t, err)

	// Theory T depends on M (both via dependencies JSON AND via
	// source_ids — both discovery paths surface T, but the
	// discovery helper dedupes by artifact_id so T counts as
	// one target).
	res, err := dm.ProposeTheory(
		"T depends on M",
		"validation",
		[]string{memID},
		[]string{memID},
		[]string{"git"},
	)
	require.NoError(t, err)
	_, _ = res["id"]

	// Decision D cites M directly via source_ids (no dependencies
	// JSON edge — a different downstream reasoning artifact than
	// T, surfaced via a different discovery path).
	_, err = dm.RecordDecision("ctx", "choice", "rationale", "",
		[]string{"git"}, []string{memID}, ActiveContext{})
	require.NoError(t, err)

	// Sanity: no intents yet.
	assert.Equal(t, 0, countOutboxRows(t, dm, ""))

	// Shred the memory.
	shredResult, err := dm.ShredMemoryWithCascade(memID)
	require.NoError(t, err)
	require.NotNil(t, shredResult)
	assert.True(t, shredResult["success"].(bool))

	// Post-condition: exactly two intents exist — one targeting T
	// (theory), one targeting D (decision). The dedup key on
	// (dead, downstream, event) collapses any duplicates within
	// the same invalidation event.
	rows := outboxRowsFor(t, dm, memID)
	assert.Equal(t, 2, len(rows),
		"shred should enqueue exactly one cascade intent per downstream dependent (theory + decision)")

	// Verify both downstream types are represented.
	types := map[string]int{}
	for _, id := range rows {
		var downType string
		require.NoError(t, dm.db.QueryRow(
			`SELECT downstream_artifact_type FROM epistemic_cascade_outbox WHERE id = ?`, id,
		).Scan(&downType))
		types[downType]++
	}
	assert.Equal(t, 1, types["theory"], "one intent for the dependent theory")
	assert.Equal(t, 1, types["decision"], "one intent for the dependent decision")

	// Verify the dead artifact type is "memory".
	var deadType string
	require.NoError(t, dm.db.QueryRow(`
		SELECT DISTINCT dead_artifact_type FROM epistemic_cascade_outbox WHERE dead_artifact_id = ?
	`, memID).Scan(&deadType))
	assert.Equal(t, "memory", deadType)
}

// TestInvalidation_ShredMemoryLessonDoesNotEnqueue pins the lesson
// path: lesson shreds must NOT enqueue cascade intents. The cascade
// materializer only targets decision/theory downstreams; lessons have
// no reasoning dependents (they are immutable observations).
func TestInvalidation_ShredMemoryLessonDoesNotEnqueue(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	// Add a lesson (no source_ids wiring required — lessons never
	// surface as cascade targets).
	lesson, err := dm.AddLesson(
		"cascade-test lesson",
		LessonTypeInsight,
		[]string{"git"},
		"test",
	)
	require.NoError(t, err)

	_, err = dm.ShredMemoryWithCascade(lesson.ID)
	require.NoError(t, err)

	assert.Equal(t, 0, countOutboxRows(t, dm, ""),
		"shredding a lesson must NOT enqueue cascade intents")
}

// TestInvalidation_ShredMemoryStandaloneWrapperEnqueues covers the
// alternative shred entry point (`ShredMemory` in web_db.go, used by
// the CLI's `mpm shred` command). It must also enqueue intents so the
// two paths cannot drift.
func TestInvalidation_ShredMemoryStandaloneWrapperEnqueues(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID, err := dm.SaveMemory(
		"memories", "M for standalone shred", "", nil, nil, nil, false, 5,
	)
	require.NoError(t, err)

	res, err := dm.ProposeTheory(
		"T depends on M",
		"v",
		[]string{memID},
		[]string{memID},
		[]string{"git"},
	)
	require.NoError(t, err)
	_, _ = res["id"]

	// Standalone wrapper (CLI path).
	err = dm.ShredMemory(memID)
	require.NoError(t, err)

	rows := outboxRowsFor(t, dm, memID)
	assert.GreaterOrEqual(t, len(rows), 1,
		"standalone ShredMemory must also enqueue cascade intents")
}

// ── Hard confidence threshold invalidation path ────────────────────────────

// TestInvalidation_HardConfidenceCrossingEnqueues pins the hard
// threshold transition: when a recompute causes the artifact's
// confidence to CROSS below the defined threshold, exactly one cascade
// intent is enqueued per downstream dependent.
func TestInvalidation_HardConfidenceCrossingEnqueues(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	// Foundation memory M with a healthy initial confidence (above
	// the threshold).
	memID, err := dm.SaveMemory(
		"memories", "M foundation for hard-confidence test",
		"", nil, nil, nil, false, 5,
	)
	require.NoError(t, err)

	// Downstream theory that depends on M.
	res, err := dm.ProposeTheory(
		"T depends on M",
		"v",
		[]string{memID},
		[]string{memID},
		[]string{"git"},
	)
	require.NoError(t, err)
	_, _ = res["id"]

	// Sanity: initial confidence of M is above the threshold.
	var initialConf float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, memID,
	).Scan(&initialConf))
	assert.Greater(t, initialConf, HardConfidenceInvalidationThreshold,
		"initial confidence should be above the hard threshold")

	// Inject enough strong NEGATIVE evidence to drive confidence below
	// the hard threshold. The evidence engine handles this without
	// cascading itself (recompute is the trigger surface). Evidence
	// strength is constrained to [-1.0, 1.0] by a CHECK constraint;
	// one -1.0 row only drops a memory from 0.8 → ~0.6 (still above
	// the 0.3 hard threshold), so we add multiple strong rows to push
	// the artifact below the floor. Three rows gets us to ~0.18.
	for i := 0; i < 3; i++ {
		require.NoError(t, AddEvidence(dm, EvidenceInput{
			ArtifactID:         memID,
			ArtifactType:       "memory",
			Type:               "challenge",
			SourceGroup:        "git",
			Strength:           -1.0,
			IndependenceFactor: 1.0,
			CreatedBy:          "test",
			CreatedAt:          time.Now(),
		}))
	}

	// Recompute — this is the production trigger surface.
	require.NoError(t, RecomputeConfidence(dm, memID, "memory", RecomputeReasonEvidenceAdded))

	// Sanity: confidence is now below the threshold.
	var newConf float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, memID,
	).Scan(&newConf))
	assert.Less(t, newConf, HardConfidenceInvalidationThreshold,
		"confidence should have crossed below the hard threshold")

	// Post-condition: at least one cascade intent exists for M.
	rows := outboxRowsFor(t, dm, memID)
	assert.GreaterOrEqual(t, len(rows), 1,
		"crossing the hard confidence threshold must enqueue a cascade intent")
}

// TestInvalidation_OrdinaryConfidenceDecreaseDoesNotEnqueue pins the
// non-cascade path: a confidence decrease that stays ABOVE the hard
// threshold must not enqueue anything. The brief explicitly calls out
// that ordinary weakening is NOT a cascade trigger.
func TestInvalidation_OrdinaryConfidenceDecreaseDoesNotEnqueue(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID, err := dm.SaveMemory(
		"memories", "M foundation for ordinary weaken test",
		"", nil, nil, nil, false, 5,
	)
	require.NoError(t, err)

	// A downstream theory so the foundation has dependents (the
	// cascade materializer would have something to materialize).
	_, err = dm.ProposeTheory(
		"T depends on M",
		"v",
		[]string{memID},
		[]string{memID},
		[]string{"git"},
	)
	require.NoError(t, err)

	// Inject a small negative evidence row — this should weaken
	// confidence but NOT cross the hard threshold.
	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:         memID,
		ArtifactType:       "memory",
		Type:               "observation",
		SourceGroup:        "git",
		Strength:           -0.3, // small negative (not strong enough to cross threshold)
		IndependenceFactor: 1.0,
		CreatedBy:          "test",
		CreatedAt:          time.Now(),
	}))

	require.NoError(t, RecomputeConfidence(dm, memID, "memory", RecomputeReasonEvidenceAdded))

	// Sanity: confidence is still above the threshold.
	var newConf float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, memID,
	).Scan(&newConf))
	assert.Greater(t, newConf, HardConfidenceInvalidationThreshold,
		"ordinary weakening should leave confidence above the hard threshold")

	// Post-condition: no cascade intent was enqueued.
	assert.Equal(t, 0, countOutboxRows(t, dm, ""),
		"ordinary confidence decrease must not enqueue cascade intents")
}

// TestInvalidation_NoCrossingOnRepeatedRecomputeBelowThreshold pins the
// "crossing is the event" semantic: once an artifact is below the hard
// threshold, further recomputes (which leave it below) must not
// re-enqueue intents. The schema's UNIQUE constraint would collapse
// duplicates anyway, but the brief asks us to detect the transition
// state and skip the enqueue entirely.
//
// Both recomputes run inside WithTx (matching the production
// AddEvidence path) so the cascade hook fires when the node is in
// a transaction; the second recompute sees old=below-threshold +
// new=below-threshold and skips the enqueue.
func TestInvalidation_NoCrossingOnRepeatedRecomputeBelowThreshold(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID, err := dm.SaveMemory(
		"memories", "M for repeated-recompute test",
		"", nil, nil, nil, false, 5,
	)
	require.NoError(t, err)

	// Downstream theory that depends on M — gives the cascade
	// discovery something to surface when the foundation crosses.
	res, err := dm.ProposeTheory(
		"T depends on M",
		"v",
		[]string{memID},
		[]string{memID},
		[]string{"git"},
	)
	require.NoError(t, err)
	_, _ = res["id"]

	// Drive confidence below threshold with three strong challenges
	// inside WithTx (matches production AddEvidence path).
	for i := 0; i < 3; i++ {
		require.NoError(t, AddEvidence(dm, EvidenceInput{
			ArtifactID:         memID,
			ArtifactType:       "memory",
			Type:               "challenge",
			SourceGroup:        "git",
			Strength:           -1.0,
			IndependenceFactor: 1.0,
			CreatedBy:          "test",
			CreatedAt:          time.Now(),
		}))
	}

	// One intent should exist (the AddEvidence path wraps the recompute
	// in WithTx so the cascade hook fires when confidence crosses).
	require.Equal(t, 1, countOutboxRows(t, dm, ""),
		"first crossing should produce one cascade intent")

	// A second recompute (no new evidence) inside WithTx must NOT add
	// another intent — old is below threshold, so the crossing detector
	// returns false.
	require.NoError(t, dm.WithTx(func(node DBNode) error {
		return RecomputeConfidence(node, memID, "memory", RecomputeReasonDecayTick)
	}))
	assert.Equal(t, 1, countOutboxRows(t, dm, ""),
		"recompute without crossing must not enqueue another intent")
}

// ── Atomicity: root mutation + outbox intents commit together ───────────────

// TestInvalidation_OutboxFailureRollsBackRootMutation pins the
// atomicity contract: if the outbox INSERT fails (simulated by
// injecting a failure on the shared-write path), the root mutation
// (e.g., theory status change) must be rolled back. The substrate must
// never see "the memory is gone but no cascade was recorded" divergence.
func TestInvalidation_OutboxFailureRollsBackRootMutation(t *testing.T) {
	dm := hermeticDatabaseManager(t)

	memID, err := dm.SaveMemory("memories", "M", "", nil, nil, nil, false, 5)
	require.NoError(t, err)
	res, err := dm.ProposeTheory(
		"T", "v", []string{memID}, []string{memID}, []string{"git"},
	)
	require.NoError(t, err)
	theoryID, _ := res["id"].(string)
	_, err = dm.RecordDecision("ctx", "choice", "rationale", "",
		[]string{"git"}, []string{theoryID}, ActiveContext{})
	require.NoError(t, err)

	// Snapshot pre-state: theory is pending.
	statusBefore, foundBefore, err := dm.lookupTheoryStatus(theoryID)
	require.NoError(t, err)
	require.True(t, foundBefore, "theory should be found pre-disprove")
	assert.Equal(t, "pending", statusBefore)

	// Rename the outbox table so the local INSERT fails mid-tx. This
	// simulates a production failure where the outbox table is missing
	// (schema drift between local and shared).
	_, err = dm.db.Exec(`ALTER TABLE epistemic_cascade_outbox RENAME TO epistemic_cascade_outbox_BROKEN`)
	require.NoError(t, err, "rename outbox to force failure")

	// Attempt to disprove — this MUST fail (cascade INSERT inside the
	// transaction will fail).
	_, err = dm.ResolveTheory(theoryID, "rejected", "disproven")
	require.Error(t, err, "ResolveTheory must fail when outbox insert fails")

	// Restore the outbox so the post-conditions can be checked.
	_, err = dm.db.Exec(`ALTER TABLE epistemic_cascade_outbox_BROKEN RENAME TO epistemic_cascade_outbox`)
	require.NoError(t, err)

	// Post-condition: the theory's status is unchanged (pending).
	statusAfter, foundAfter, err := dm.lookupTheoryStatus(theoryID)
	require.NoError(t, err)
	require.True(t, foundAfter, "theory should still exist after rollback")
	assert.Equal(t, "pending", statusAfter,
		"theory status must be rolled back when cascade outbox insert fails")
}
