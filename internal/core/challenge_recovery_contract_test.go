// challenge_recovery_contract_test.go — issue #24 regressions.
//
// The defect: two production writers set `status=challenged` and reduced a
// memory's weight AND retrieval_priority through an inline
// `UPDATE shared.memories` that recorded no prior values and no theory
// reference. `mpm challenge restore` therefore refused those memories
// outright — the same "is not challenged" message an untouched memory gets —
// so a legitimate, operator-adjudicated verdict was permanently
// irreversible. Adjudication gates *who* may trigger a slash (issue #10);
// it never made the slash reversible.
//
// The contract pinned here, across all three writers:
//
//  1. A challenge records a baseline for every column it reduces, and only
//     for those columns.
//  2. The recorded baseline is the FIRST one. A repeated challenge on an
//     already-challenged memory must not overwrite it.
//  3. Metadata and the reduction are one statement, so the record of what
//     happened cannot drift from what happened.
//  4. Restore puts back exactly what was recorded, in the store the
//     challenge wrote to.
//  5. A challenge with no linked theory does not fabricate one, and
//     restores COMPLETELY rather than reporting a missing theory.
//  6. A challenged row with no recorded baseline is REFUSED with a message
//     distinguishable from "is not challenged" — never speculatively
//     reconstructed.
//
// Every test here runs against an isolated in-memory DatabaseManager
// (newTestDM / newTestDMWithShared). No live MPM database, workspace,
// service or scheduler is touched, and no historical row is migrated.
package internal

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── helpers ────────────────────────────────────────────────────────────

// issue24State is the persisted state a challenge is allowed to move.
type issue24State struct {
	weight            float64
	retrievalPriority float64
	confidence        float64
	metadata          string
}

func issue24Snapshot(t *testing.T, dm *DatabaseManager, table ChallengeTable, id string) issue24State {
	t.Helper()
	var s issue24State
	var meta *string
	err := dm.db.QueryRow(fmt.Sprintf(
		`SELECT COALESCE(weight,0), COALESCE(retrieval_priority,0), COALESCE(confidence,0), metadata FROM %s WHERE id = ?`,
		string(table)), id).Scan(&s.weight, &s.retrievalPriority, &s.confidence, &meta)
	require.NoError(t, err, "snapshot %s from %s", id, string(table))
	if meta != nil {
		s.metadata = *meta
	}
	return s
}

func issue24Meta(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	m := map[string]interface{}{}
	if raw == "" {
		return m
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &m))
	return m
}

// issue24SeedShared plants a shared.memories row with explicit columns, so
// a test can state the pre-challenge weight and retrieval_priority as
// literals rather than inheriting whatever the schema defaults happen to be.
func issue24SeedShared(t *testing.T, dm *DatabaseManager, id string, confidence, weight, retrievalPriority float64) {
	t.Helper()
	_, err := dm.db.Exec(`
		INSERT INTO shared.memories (id, collection, content, confidence, retrieval_priority, importance, weight)
		VALUES (?, 'memories', ?, ?, ?, ?, ?)`,
		id, id+" content", confidence, retrievalPriority, 0.5, weight)
	require.NoError(t, err, "seed shared memory %s", id)
}

// issue24ApplyAdjudicated runs the real `--apply` path on a decisive row and
// returns the queue id. Adjudication is what issue #10 requires before a
// row may penalise a memory, so the tests here exercise the path an
// operator legitimately reaches rather than a bypass.
func issue24ApplyAdjudicated(t *testing.T, dm *DatabaseManager, queueID int64) ResolutionDecision {
	t.Helper()
	row := rowFor(t, dm, queueID)
	dec, err := dm.ResolveOneContradiction(row, true)
	require.NoError(t, err, "an adjudicated decisive row must apply")
	return dec
}

// issue24EnqueueAdjudicated creates a contradiction row and adjudicates it.
// Adjudication is issue #10's precondition for a row to penalise anything,
// so every test that exercises a real slash goes through it rather than
// around it.
func issue24EnqueueAdjudicated(t *testing.T, dm *DatabaseManager, a, b string) int64 {
	t.Helper()
	id := enqueueWithSource(t, dm, a, b, similarityCandidateSource)
	require.NoError(t, dm.AdjudicateContradiction(id, "operator confirmed the negation"))
	return id
}

// ── writer 1: ChallengeMemory (the challenge surfaces) ────────────────

// TestIssue24_ChallengeMemory_RoundTrip is the baseline the other two
// writers are measured against. It must keep working exactly as it did:
// requirement 8 of #11 forbids regressing the healthy path.
func TestIssue24_ChallengeMemory_RoundTrip(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	memID := seedWeightedMemory(t, dm, "issue24 challenge-memory round trip", 9)
	before := issue24Snapshot(t, dm, ChallengeTableLocal, memID)

	err := dm.ChallengeMemory(memID, 2, "contradicted by the October runbook")
	require.NoError(t, err)

	mid := issue24Snapshot(t, dm, ChallengeTableLocal, memID)
	require.Equal(t, float64(7), mid.weight)
	midMeta := issue24Meta(t, mid.metadata)
	require.Equal(t, "challenged", midMeta["status"])
	require.Equal(t, float64(9), midMeta["challenged_prior_weight"])
	require.NotContains(t, midMeta, "challenged_prior_retrieval_priority",
		"the challenge surfaces do NOT reduce retrieval_priority — so recording a prior for it would claim a "+
			"change that did not happen, and a restore would then clobber an unrelated later adjustment")
	require.Equal(t, ChallengeViaTheory, midMeta["challenged_via"])

	res, err := dm.RestoreMemoryFromChallenge(memID)
	require.NoError(t, err)
	require.Equal(t, "local", res["memory_store"])
	require.Equal(t, true, res["weight_restored"])
	require.Equal(t, false, res["retrieval_priority_restored"],
		"ISSUE 24: a restore must not touch a column the challenge never reduced")

	after := issue24Snapshot(t, dm, ChallengeTableLocal, memID)
	assert.Equal(t, float64(9), after.weight)
	assert.Equal(t, before.retrievalPriority, after.retrievalPriority,
		"retrieval_priority must be exactly as it was — untouched by both the challenge and the restore")
	assert.NotContains(t, issue24Meta(t, after.metadata), "status")
}

// ── writer 2: applyResolution (`--apply` on a decisive row) ───────────

// TestIssue24_ApplyResolution_RoundTrip is the defect, end to end: adjudicate
// → apply → restore, asserting the pre-slash weight AND retrieval_priority
// come back and the status flag clears.
func TestIssue24_ApplyResolution_RoundTrip(t *testing.T) {
	dm := newTestDMWithShared(t)

	issue24SeedShared(t, dm, "issue24-winner", 0.95, 5, 0.90)
	issue24SeedShared(t, dm, "issue24-loser", 0.10, 7, 0.80)
	qid := issue24EnqueueAdjudicated(t, dm, "issue24-winner", "issue24-loser")
	before := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-loser")

	dec := issue24ApplyAdjudicated(t, dm, qid)
	require.Equal(t, "issue24-loser", dec.LoserID)

	mid := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-loser")
	midMeta := issue24Meta(t, mid.metadata)

	// Both columns really moved — otherwise this test proves nothing.
	require.Less(t, mid.weight, before.weight, "apply must reduce weight")
	require.Less(t, mid.retrievalPriority, before.retrievalPriority, "apply must reduce retrieval_priority")

	require.Equal(t, "challenged", midMeta["status"])
	require.Equal(t, float64(before.weight), midMeta["challenged_prior_weight"],
		"ISSUE 24: the pre-slash weight must be recorded, not reconstructed")
	require.Equal(t, before.retrievalPriority, midMeta["challenged_prior_retrieval_priority"],
		"ISSUE 24: the pre-slash retrieval_priority must be recorded — nothing else can put it back")
	require.Equal(t, ChallengeViaResolution, midMeta["challenged_via"])
	require.NotEmpty(t, midMeta["challenged_by_resolution"])
	require.NotContains(t, midMeta, "challenged_theory_id",
		"a resolution challenge has no theory to disprove; fabricating one would send a restore looking for a row that does not exist")

	res, err := dm.RestoreMemoryFromChallenge("issue24-loser")
	require.NoError(t, err, "ISSUE 24: a memory challenged by --apply must be restorable")
	assert.Equal(t, "shared", res["memory_store"])
	assert.Equal(t, true, res["weight_restored"])
	assert.Equal(t, true, res["retrieval_priority_restored"])
	assert.Equal(t, true, res["complete"])
	assert.Equal(t, TheoryResolutionNotApplicable, res["theory_resolution"],
		"a challenge with no linked theory is complete, not partial")
	assert.Equal(t, false, res["theory_resolved"])
	assert.Equal(t, false, res["confidence_reset"],
		"--apply never drops confidence, so a restore must not reset it")

	after := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-loser")
	assert.Equal(t, before.weight, after.weight, "weight must return to its exact pre-slash value")
	assert.Equal(t, before.retrievalPriority, after.retrievalPriority, "retrieval_priority must return to its exact pre-slash value")
	assert.Equal(t, before.confidence, after.confidence, "confidence must be untouched")

	afterMeta := issue24Meta(t, after.metadata)
	assert.NotContains(t, afterMeta, "status")
	assert.NotContains(t, afterMeta, "challenged_prior_weight")
	assert.NotContains(t, afterMeta, "challenged_prior_retrieval_priority")
	assert.Equal(t, true, afterMeta["restored_from_challenge"])
	assert.NotEmpty(t, afterMeta["challenged_by_resolution"], "provenance must survive as audit trail")
}

// ── writer 3: applyArbitrationResolution (operator closes the loop) ────

// TestIssue24_Arbitration_RoundTrip covers the more misleading of the two
// irreversible writers, because ResolveArbitrationTheory is the documented
// close-the-loop operator workflow and reads as a reversible decision.
func TestIssue24_Arbitration_RoundTrip(t *testing.T) {
	dm := newTestDMWithShared(t)

	issue24SeedShared(t, dm, "issue24-arbitrator-winner", 0.90, 30, 0.60)
	issue24SeedShared(t, dm, "issue24-arbitrator-loser", 0.30, 40, 0.75)

	res, _ := dm.db.Exec(`INSERT INTO shared.contradiction_log (memory_id_a, memory_id_b, evidence, similarity) VALUES ('issue24-arbitrator-winner','issue24-arbitrator-loser','close call',0.9)`)
	queueID, _ := res.LastInsertId()
	_, err := dm.db.Exec(`
		INSERT INTO shared.memories (id, collection, content, tags, dependencies, metadata, weight, retrieval_priority, importance, confidence)
		VALUES ('issue24-theory', 'theories', 'arbitration hypothesis', '["arbitration:contradiction:issue24"]', ?, ?, 1.0, 0.5, 0.5, 0.5)`,
		`["issue24-arbitrator-winner","issue24-arbitrator-loser"]`,
		fmt.Sprintf(`{"status":"pending","arbitration_for_queue_id":%d}`, queueID))
	require.NoError(t, err)

	before := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-arbitrator-loser")

	out, err := dm.ResolveArbitrationTheory("issue24-theory", "issue24-arbitrator-winner", "operator reviewed both claims")
	require.NoError(t, err)
	require.Equal(t, "issue24-arbitrator-loser", out["loser_id"])

	mid := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-arbitrator-loser")
	midMeta := issue24Meta(t, mid.metadata)
	require.Less(t, mid.weight, before.weight)
	require.Less(t, mid.retrievalPriority, before.retrievalPriority)
	require.Equal(t, float64(before.weight), midMeta["challenged_prior_weight"])
	require.Equal(t, before.retrievalPriority, midMeta["challenged_prior_retrieval_priority"])
	require.Equal(t, ChallengeViaArbitration, midMeta["challenged_via"])
	require.Equal(t, "issue24-theory", midMeta["arbitration_theory"])
	require.NotContains(t, midMeta, "challenged_theory_id",
		"the arbitration theory is already resolved by the operator; a restore must not go and disprove it")

	restoreRes, err := dm.RestoreMemoryFromChallenge("issue24-arbitrator-loser")
	require.NoError(t, err, "ISSUE 24: closing the loop must be undoable")
	assert.Equal(t, TheoryResolutionNotApplicable, restoreRes["theory_resolution"])
	assert.Equal(t, true, restoreRes["complete"])
	assert.Equal(t, true, restoreRes["weight_restored"])
	assert.Equal(t, true, restoreRes["retrieval_priority_restored"])

	after := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-arbitrator-loser")
	assert.Equal(t, before.weight, after.weight)
	assert.Equal(t, before.retrievalPriority, after.retrievalPriority)
	assert.NotContains(t, issue24Meta(t, after.metadata), "status")

	// The operator's verdict must survive a restore of the loser. A restore
	// that flipped the theory to "disproven" would report a contradiction
	// the operator explicitly rejected.
	var theoryStatus string
	require.NoError(t, dm.db.QueryRow(
		`SELECT json_extract(metadata,'$.status') FROM shared.memories WHERE id = 'issue24-theory'`).Scan(&theoryStatus))
	assert.Equal(t, "resolved", theoryStatus,
		"ISSUE 24: restoring the loser must not rewrite the operator's resolved verdict")
}

// ── theory-linked restore still follows the #11 contract ──────────────

// TestIssue24_TheoryLinkedRestore_ResolvesTheory pins that the new contract
// did not weaken #11: a theory-linked challenge still resolves its theory,
// and now also reports which store it read from.
func TestIssue24_TheoryLinkedRestore_ResolvesTheory(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	memID, theoryID := challengeTarget(t, dm, "issue24 theory linked restore")

	res, err := dm.RestoreMemoryFromChallenge(memID)
	require.NoError(t, err)
	assert.Equal(t, TheoryResolutionResolved, res["theory_resolution"])
	assert.Equal(t, int64(1), res["theory_rows_affected"])
	assert.Equal(t, true, res["complete"])
	assert.Equal(t, "local", res["memory_store"])
	assert.Equal(t, ChallengeViaTheory, res["challenge_via"])
	assert.Equal(t, "disproven", theoryMeta(t, dm, theoryID)["status"])
}

// ── repeated challenge does not overwrite the baseline ────────────────

// TestIssue24_RepeatedChallenge_KeepsOriginalBaseline: the baseline is
// where the memory stood before the FIRST challenge. Any later value
// already includes the first slash, so restoring to it would leave the
// memory permanently weaker than it ever was.
func TestIssue24_RepeatedChallenge_KeepsOriginalBaseline(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	memID := seedWeightedMemory(t, dm, "issue24 repeated challenge", 9)

	require.NoError(t, dm.ChallengeMemory(memID, 2, "first challenge"))
	afterFirst := issue24Meta(t, issue24Snapshot(t, dm, ChallengeTableLocal, memID).metadata)
	require.Equal(t, float64(9), afterFirst["challenged_prior_weight"])
	require.Equal(t, float64(7), issue24Snapshot(t, dm, ChallengeTableLocal, memID).weight)

	require.NoError(t, dm.ChallengeMemory(memID, 2, "second challenge"))
	afterSecond := issue24Meta(t, issue24Snapshot(t, dm, ChallengeTableLocal, memID).metadata)

	assert.Equal(t, float64(9), afterSecond["challenged_prior_weight"],
		"ISSUE 24: a repeated challenge must not move the restoration baseline")
	assert.Equal(t, float64(5), issue24Snapshot(t, dm, ChallengeTableLocal, memID).weight,
		"the reduction itself must still apply on every challenge")

	_, err := dm.RestoreMemoryFromChallenge(memID)
	require.NoError(t, err)
	assert.Equal(t, float64(9), issue24Snapshot(t, dm, ChallengeTableLocal, memID).weight,
		"restore must return the memory to where it stood before the FIRST challenge")
}

// TestIssue24_RepeatedResolutionSlash_KeepsBaseline is the same invariant on
// the writer that reduces two columns. Two adjudicated rows against the same
// loser slash it twice; the recorded baseline must remain the first value.
func TestIssue24_RepeatedResolutionSlash_KeepsBaseline(t *testing.T) {
	dm := newTestDMWithShared(t)

	issue24SeedShared(t, dm, "issue24-double-winner", 0.95, 5, 0.9)
	issue24SeedShared(t, dm, "issue24-double-loser", 0.10, 40, 0.80)

	first := issue24EnqueueAdjudicated(t, dm, "issue24-double-winner", "issue24-double-loser")
	issue24ApplyAdjudicated(t, dm, first)
	afterFirst := issue24Meta(t, issue24Snapshot(t, dm, ChallengeTableShared, "issue24-double-loser").metadata)
	require.Equal(t, float64(40), afterFirst["challenged_prior_weight"])
	require.Equal(t, float64(0.80), afterFirst["challenged_prior_retrieval_priority"])

	// A second, independent contradiction naming the same loser. The queue
	// keys on (a, b, detected_at), so detected_at is pinned to keep the two
	// rows distinct — this is a real second detection, not a re-read.
	res, err := dm.db.Exec(`
		INSERT INTO shared.contradiction_log
			(memory_id_a, memory_id_b, evidence, similarity, detected_at, detected_by)
		VALUES ('issue24-double-winner', 'issue24-double-loser', 'a second, distinct detection', 0.88,
		        '2026-10-14 00:00:01', 'similarity:candidate')`)
	require.NoError(t, err)
	second, _ := res.LastInsertId()
	require.NoError(t, dm.AdjudicateContradiction(second, "operator confirmed the negation again"))
	issue24ApplyAdjudicated(t, dm, second)

	afterSecond := issue24Meta(t, issue24Snapshot(t, dm, ChallengeTableShared, "issue24-double-loser").metadata)
	assert.Equal(t, float64(40), afterSecond["challenged_prior_weight"],
		"ISSUE 24: a second slash must not move the restoration baseline")
	assert.Equal(t, float64(0.80), afterSecond["challenged_prior_retrieval_priority"],
		"ISSUE 24: a second slash must not move the retrieval_priority baseline")

	_, err = dm.RestoreMemoryFromChallenge("issue24-double-loser")
	require.NoError(t, err)
	final := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-double-loser")
	assert.Equal(t, float64(40), final.weight)
	assert.Equal(t, float64(0.80), final.retrievalPriority)
}

// ── repeated restore ─────────────────────────────────────────────────

// TestIssue24_RepeatedRestore_IsSafeAndReported: the second restore must be
// refused with a message that says the memory is not challenged, must not
// drift the weight, and must not re-resolve the theory.
func TestIssue24_RepeatedRestore_IsSafeAndReported(t *testing.T) {
	const (
		issue24RepeatLoserWeight = 12.0
		issue24RepeatLoserRP     = 0.85
	)
	dm := newTestDMWithShared(t)
	issue24SeedShared(t, dm, "issue24-repeat-winner", 0.95, 5, 0.9)
	issue24SeedShared(t, dm, "issue24-repeat-loser", 0.10, issue24RepeatLoserWeight, issue24RepeatLoserRP)
	qid := issue24EnqueueAdjudicated(t, dm, "issue24-repeat-winner", "issue24-repeat-loser")
	issue24ApplyAdjudicated(t, dm, qid)

	before := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-repeat-loser")
	require.Less(t, before.weight, issue24RepeatLoserWeight,
		"setup: the apply path must have slashed the loser")
	require.Less(t, before.retrievalPriority, issue24RepeatLoserRP,
		"setup: the apply path must have reduced its retrieval priority")

	res, err := dm.RestoreMemoryFromChallenge("issue24-repeat-loser")
	require.NoError(t, err)
	require.Equal(t, true, res["success"])

	restored := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-repeat-loser")

	res2, err := dm.RestoreMemoryFromChallenge("issue24-repeat-loser")
	require.Error(t, err, "restoring an already-restored memory must fail loudly")
	assert.Nil(t, res2)
	assert.Contains(t, err.Error(), "not challenged")

	after := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-repeat-loser")
	assert.Equal(t, restored.weight, after.weight, "a refused repeat must not drift the weight")
	assert.Equal(t, restored.retrievalPriority, after.retrievalPriority)

	// The restore must land on the SEEDED weight, not on the post-challenge
	// one. Asserting against `before` would be asserting that a restore is
	// a no-op.
	assert.Equal(t, issue24RepeatLoserWeight, restored.weight,
		"ISSUE 24: the first restore returns the loser to its pre-challenge weight")
	assert.Equal(t, issue24RepeatLoserRP, restored.retrievalPriority,
		"ISSUE 24: the first restore returns the loser to its pre-challenge retrieval priority")
}

// ── partial failure rolls back completely ────────────────────────────

// TestIssue24_RestoreRollsBackOnTheoryHalf: the theory UPDATE runs first,
// so an injected failure in the memory half must roll it back. Otherwise a
// failed restore leaves a theory marked disproven — an adjudication that
// never completed.
func TestIssue24_RestoreRollsBackOnTheoryHalf(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	memID, theoryID := challengeTarget(t, dm, "issue24 restore rollback")

	require.Equal(t, TheoryReferenceIdentifier, classifyTheoryReference(memID),
		"setup: the memory id must be a generated hex id for safe trigger interpolation")
	_, err := dm.db.Exec(fmt.Sprintf(`
		CREATE TRIGGER issue24_block_restore
		BEFORE UPDATE ON memories
		WHEN NEW.id = '%s'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`, memID))
	require.NoError(t, err)

	res, err := dm.RestoreMemoryFromChallenge(memID)
	require.Error(t, err)
	assert.Nil(t, res)

	assert.Equal(t, "pending", theoryMeta(t, dm, theoryID)["status"],
		"ISSUE 24: a failed restore must not leave a theory marked disproven")
	meta := issue24Meta(t, issue24Snapshot(t, dm, ChallengeTableLocal, memID).metadata)
	assert.Equal(t, "challenged", meta["status"], "the memory must be left challenged")
	assert.Equal(t, float64(7), issue24Snapshot(t, dm, ChallengeTableLocal, memID).weight, "weight must be left slashed")
}

// TestIssue24_ChallengeRollsBackOnLaterFailure pins the other half of
// atomicity: when a step AFTER the challenge statement fails inside
// applyResolution's transaction, the challenge is rolled back too. The
// metadata and the slash are one statement, but the whole resolution is one
// transaction, and a partially-applied verdict is worse than none.
func TestIssue24_ChallengeRollsBackOnLaterFailure(t *testing.T) {
	dm := newTestDMWithShared(t)

	issue24SeedShared(t, dm, "issue24-atomic-winner", 0.95, 5, 0.9)
	issue24SeedShared(t, dm, "issue24-atomic-loser", 0.10, 9, 0.7)
	qid := issue24EnqueueAdjudicated(t, dm, "issue24-atomic-winner", "issue24-atomic-loser")

	before := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-atomic-loser")

	// Make the queue close — the step after the slash — fail.
	//
	// The trigger is created INSIDE the attached database and its table is
	// named unqualified: SQLite forbids a trigger from referring to objects
	// in another database by qualified name, and the trigger itself lives
	// in `shared`, so its unqualified `contradiction_log` resolves there.
	_, err := dm.db.Exec(fmt.Sprintf(`
		CREATE TRIGGER shared.issue24_block_queue_close
		BEFORE UPDATE ON contradiction_log
		WHEN NEW.id = %d
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`, qid))
	require.NoError(t, err)

	_, err = dm.ResolveOneContradiction(rowFor(t, dm, qid), true)
	require.Error(t, err, "setup: the injected failure must surface")

	after := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-atomic-loser")
	assert.Equal(t, before.metadata, after.metadata,
		"ISSUE 24: a failed resolution must leave the loser completely untouched")
	assert.Equal(t, before.weight, after.weight)
	assert.Equal(t, before.retrievalPriority, after.retrievalPriority)
	assert.NotContains(t, issue24Meta(t, after.metadata), "status")

	n := countQueueTableRows(t, dm, `SELECT COUNT(*) FROM shared.memories WHERE collection='resolutions'`)
	assert.Equal(t, 0, n, "the resolution memory must be rolled back with everything else")
}

// ── missing / invalid recovery metadata fails safely ─────────────────

// TestIssue24_HistoricalRowWithoutBaseline_IsRefusedNotGuessed is the
// historical shape the two Arc 1 writers produced before this fix: reduced
// columns, nothing recorded. There is no honest reconstruction — the memory
// may have been reinforced since — so the restore must refuse, say which
// case this is, and write nothing.
func TestIssue24_HistoricalRowWithoutBaseline_IsRefusedNotGuessed(t *testing.T) {
	dm := newTestDMWithShared(t)
	issue24SeedShared(t, dm, "issue24-legacy-loser", 0.10, 9, 0.7)

	// Exactly what applyResolution used to leave behind.
	_, err := dm.db.Exec(`UPDATE shared.memories SET metadata = ? WHERE id = 'issue24-legacy-loser'`,
		`{"status":"challenged","challenged_by_resolution":"resolution-issue24-legacy-loser-1","challenged_at":"2026-10-01T00:00:00Z"}`)
	require.NoError(t, err)
	before := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-legacy-loser")

	res, err := dm.RestoreMemoryFromChallenge("issue24-legacy-loser")
	require.Error(t, err, "ISSUE 24: an unrecoverable challenge must not report success")
	assert.Nil(t, res)

	assert.Contains(t, err.Error(), "cannot be reversed",
		"the error must say this is unrecoverable, not merely missing")
	assert.Contains(t, err.Error(), "resolution-issue24-legacy-loser-1",
		"the error must point at the resolution that caused it")
	assert.NotContains(t, err.Error(), "is not challenged",
		"ISSUE 24: an unrecoverable challenge must read differently from an absent one")

	after := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-legacy-loser")
	assert.Equal(t, before.metadata, after.metadata, "a refused restore must write nothing")
	assert.Equal(t, float64(9), after.weight)
}

// TestIssue24_UnchallengedMemory_IsDistinguishable: the other half of the
// same contract. A memory that was never challenged must not be reported as
// an unrecoverable challenge.
func TestIssue24_UnchallengedMemory_IsDistinguishable(t *testing.T) {
	dm := newTestDMWithShared(t)
	issue24SeedShared(t, dm, "issue24-never-challenged", 0.5, 5, 0.5)

	_, err := dm.RestoreMemoryFromChallenge("issue24-never-challenged")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not challenged")
	assert.NotContains(t, err.Error(), "cannot be reversed")
}

// TestIssue24_InvalidBaseline_FailsSafely: a baseline that is present but
// not a number must not be silently skipped (which would report a restore
// that only partly happened) nor coerced (which would write a value that
// never existed).
func TestIssue24_InvalidBaseline_FailsSafely(t *testing.T) {
	dm := newTestDMWithShared(t)
	issue24SeedShared(t, dm, "issue24-bad-baseline", 0.5, 5, 0.5)
	_, err := dm.db.Exec(`UPDATE shared.memories SET metadata = ? WHERE id = 'issue24-bad-baseline'`,
		`{"status":"challenged","challenged_prior_weight":"fairly high","challenged_prior_retrieval_priority":0.5}`)
	require.NoError(t, err)
	before := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-bad-baseline")

	_, err = dm.RestoreMemoryFromChallenge("issue24-bad-baseline")
	require.Error(t, err, "a non-numeric weight baseline must not be ignored")
	assert.Equal(t, before.metadata, issue24Snapshot(t, dm, ChallengeTableShared, "issue24-bad-baseline").metadata,
		"nothing may be written when the baseline cannot be trusted")
}

// ── #10's invariant still holds ───────────────────────────────────────

// TestIssue24_UnverifiedCandidate_StillCannotPenalise: #24 changed what an
// ALLOWED challenge writes. It must not have relaxed what decides whether a
// challenge is allowed. An unverified similarity candidate must still be
// refused outright and leave no challenge state whatsoever — including no
// baseline, which would otherwise leave a latent restore behind.
func TestIssue24_UnverifiedCandidate_StillCannotPenalise(t *testing.T) {
	dm := newTestDMWithShared(t)
	seedDecisivePair(t, dm, "issue24-cand-a", "issue24-cand-b")

	qid := enqueueWithSource(t, dm, "issue24-cand-a", "issue24-cand-b", similarityCandidateSource)
	before := snapshotMemory(t, dm, "issue24-cand-b")

	_, err := dm.ResolveOneContradiction(rowFor(t, dm, qid), true)
	require.Error(t, err, "ISSUE 10 INVARIANT: an unverified similarity candidate must still be refused")

	after := snapshotMemory(t, dm, "issue24-cand-b")
	assert.Equal(t, before.metadata, after.metadata)
	assert.Equal(t, before.weight, after.weight)
	assert.Equal(t, before.retrievalPriority, after.retrievalPriority)
	assert.NotContains(t, after.metadata, "challenged_prior_weight",
		"a refused candidate must not leave a restorable baseline behind")
	assert.True(t, queueRowIsUnresolved(t, dm, qid), "a refused row stays in the queue")
}

// ── historical rows are not migrated ──────────────────────────────────

// TestIssue24_HistoricalRows_AreNotMigrated: the fix is for future
// challenges. It must not rewrite rows already on disk — including the 41
// the retired auto-challenge defect left, whose remediation is a separate
// decision (issue #20). Bytes in, bytes out, across a full challenge and
// restore cycle performed on other memories.
func TestIssue24_HistoricalRows_AreNotMigrated(t *testing.T) {
	dm := newTestDMWithShared(t)

	// The three historical shapes.
	issue24SeedShared(t, dm, "issue24-legacy-resolution", 0.5, 4, 0.4)
	_, err := dm.db.Exec(`UPDATE shared.memories SET metadata = ? WHERE id = 'issue24-legacy-resolution'`,
		`{"status":"challenged","challenged_by_resolution":"resolution-legacy-1","challenged_at":"2026-08-01T00:00:00Z"}`)
	require.NoError(t, err)

	issue24SeedShared(t, dm, "issue24-legacy-arbitration", 0.5, 4, 0.4)
	_, err = dm.db.Exec(`UPDATE shared.memories SET metadata = ? WHERE id = 'issue24-legacy-arbitration'`,
		`{"status":"challenged","challenged_by_resolution":"resolution-legacy-2","challenged_at":"2026-08-02T00:00:00Z","challenged_via":"operator_arbitration","arbitration_theory":"arbitration-legacy-2"}`)
	require.NoError(t, err)

	legacyLocal := seedWeightedMemory(t, dm, "issue24 legacy auto-challenge row", 4)
	_, err = dm.db.Exec(`UPDATE memories SET metadata = ? WHERE id = ?`,
		`{"status":"challenged","challenged_theory_id":"contradicted by runbook revision 2026-08","challenged_prior_weight":9,"challenged_prior_confidence":0.7,"challenged_at":1754000000}`, legacyLocal)
	require.NoError(t, err)

	type row struct {
		table ChallengeTable
		id    string
	}
	historical := []row{
		{ChallengeTableShared, "issue24-legacy-resolution"},
		{ChallengeTableShared, "issue24-legacy-arbitration"},
		{ChallengeTableLocal, legacyLocal},
	}
	before := map[string]string{}
	for _, h := range historical {
		before[string(h.table)+h.id] = issue24Snapshot(t, dm, h.table, h.id).metadata
	}

	// Now exercise the new writers on OTHER memories.
	issue24SeedShared(t, dm, "issue24-live-winner", 0.95, 5, 0.9)
	issue24SeedShared(t, dm, "issue24-live-loser", 0.10, 9, 0.7)
	qid := issue24EnqueueAdjudicated(t, dm, "issue24-live-winner", "issue24-live-loser")
	issue24ApplyAdjudicated(t, dm, qid)
	_, err = dm.RestoreMemoryFromChallenge("issue24-live-loser")
	require.NoError(t, err)

	fresh := seedWeightedMemory(t, dm, "issue24 fresh local round trip", 9)
	_, err = dm.ChallengeMemoryWithTheory(fresh, "issue24 evidence")
	require.NoError(t, err)
	_, err = dm.RestoreMemoryFromChallenge(fresh)
	require.NoError(t, err)

	for _, h := range historical {
		after := issue24Snapshot(t, dm, h.table, h.id)
		assert.Equal(t, before[string(h.table)+h.id], after.metadata,
			"ISSUE 24: historical challenged rows must not be rewritten by unrelated challenge activity (%s)", h.id)
	}
}

// ── a challenge can never raise the memory's rank ────────────────────

// TestIssue24_NegativeSlash_IsRejected: `weight = MAX(1, weight - ?)` turns a
// negative slash into a weight BOOST, so a challenge carrying one would
// silently promote contested knowledge — the exact opposite of what it
// claims to do. The primitive refuses the value instead, and the docs claim
// that refusal, so it is pinned here rather than left to the SQL.
func TestIssue24_NegativeSlash_IsRejected(t *testing.T) {
	dm := newTestDMWithShared(t)
	issue24SeedShared(t, dm, "issue24-negative-winner", 0.95, 5, 0.9)
	issue24SeedShared(t, dm, "issue24-negative-loser", 0.10, 4, 0.4)

	before := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-negative-loser")

	tx, err := dm.db.Begin()
	require.NoError(t, err)

	_, err = applyChallengeStateInTx(tx, ChallengeTableShared, ChallengeSpec{
		MemoryID:               "issue24-negative-loser",
		SlashAmount:            -3,
		SlashRetrievalPriority: true,
		ChallengedAt:           1760000000,
		Fields:                 map[string]interface{}{"status": "challenged"},
	})
	require.Error(t, err, "a negative slash amount must be refused, not inverted into a boost")
	assert.Contains(t, err.Error(), "slash amount")
	// Release the connection before the next call: the shared ATTACH lives
	// on the pool connection setup used, and holding a transaction open
	// here would hand a later query a connection that has no `shared`.
	require.NoError(t, tx.Rollback())

	// And the public ChallengeMemory surface refuses it the same way.
	err = dm.ChallengeMemory("issue24-negative-loser", -3, "negative slash")
	require.Error(t, err)

	after := issue24Snapshot(t, dm, ChallengeTableShared, "issue24-negative-loser")
	assert.Equal(t, before.weight, after.weight, "ISSUE 24: a refused challenge must not move the weight")
	assert.Equal(t, before.retrievalPriority, after.retrievalPriority)
	assert.Equal(t, before.metadata, after.metadata, "and must not stamp challenge metadata either")
}
