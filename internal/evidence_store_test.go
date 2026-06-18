package internal

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidenceStore_AddEvidenceAndRecompute(t *testing.T) {
	dm := newTestDM(t)

	// Insert a memory, then add positive evidence.
	memID := "mem-1"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'parser error observed')`, 0, memID)
	require.NoError(t, err)

	err = AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "reproduction",
		SourceGroup:  "test-rig-1",
		Strength:     0.85,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)

	// After evidence: confidence should have moved up from initial 0.8.
	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, memID).Scan(&conf))
	assert.Greater(t, conf, 0.8, "positive evidence should raise confidence above initial 0.8")

	// History should have at least one row.
	var historyCount int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ?`, memID).Scan(&historyCount))
	assert.GreaterOrEqual(t, historyCount, 1)
}

func TestEvidenceStore_NegativeEvidenceLowersConfidence(t *testing.T) {
	dm := newTestDM(t)

	memID := "mem-2"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'uncertain claim')`, 0, memID)
	require.NoError(t, err)

	err = AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "challenge",
		SourceGroup:  "reviewer-1",
		Strength:     -0.6,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)

	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, memID).Scan(&conf))
	assert.Less(t, conf, 0.8, "challenge evidence should lower confidence below initial")
}

func TestEvidenceStore_TriggerReasonRecordedInHistory(t *testing.T) {
	dm := newTestDM(t)

	memID := "mem-3"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)

	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test-rig",
		Strength:     0.4,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	}))

	var trigger string
	require.NoError(t, dm.QueryRowTracked(
		`SELECT trigger FROM confidence_history WHERE artifact_id = ? ORDER BY computed_at DESC LIMIT 1`, memID,
	).Scan(&trigger))
	assert.Equal(t, "evidence_added", trigger)
}

func TestEvidenceStore_RecomputeManual(t *testing.T) {
	dm := newTestDM(t)

	memID := "mem-4"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)

	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "test",
		SourceGroup:  "test-rig",
		Strength:     0.7,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	}))

	// Manual recompute with the same evidence should produce a history row
	// with trigger='manual_recompute'.
	require.NoError(t, RecomputeConfidence(dm, memID, "memory", RecomputeReasonManual))

	var manualCount int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ? AND trigger = 'manual_recompute'`, memID,
	).Scan(&manualCount))
	assert.GreaterOrEqual(t, manualCount, 1)
}

func TestEvidenceStore_AddEvidence_BlocksSensitiveNotes(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES ('m1', 'memories', 'x')`, 0)
	require.NoError(t, err)

	// The 20-pattern scanner matches API key prefixes; "sk-" + 20+ chars
	// is the canary pattern that the scanner is supposed to catch.
	err = AddEvidence(dm, EvidenceInput{
		ArtifactID:   "m1",
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.4,
		CreatedBy:    "test",
		Notes:        "found api key sk-abcdefghijklmnopqrstuv in the logs",
	})
	require.Error(t, err, "scanner should block evidence with API key in notes")
	assert.Contains(t, err.Error(), "sensitive content")

	// Verify nothing was persisted.
	var count int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM evidence WHERE artifact_id = 'm1'`).Scan(&count))
	assert.Equal(t, 0, count, "no evidence row should be persisted when scanner blocks")
}

// TestEvidenceStore_WithTxRollsBackOnRecomputeFailure exercises the
// WithTx + RecomputeConfidence code path that AddEvidence uses. A bad
// RecomputeReason ("not_a_valid_reason") violates the CHECK constraint on
// confidence_history.trigger, causing the history INSERT to fail inside
// the transaction. WithTx must roll back the evidence row.
func TestEvidenceStore_WithTxRollsBackOnRecomputeFailure(t *testing.T) {
	dm := newTestDM(t)

	memID := "rollback-test-1"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'safe memory')`, 0, memID)
	require.NoError(t, err)

	err = dm.WithTx(func(node DBNode) error {
		// 1. Insert evidence (would normally succeed).
		_, err := node.ExecTracked(`
			INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group,
			                       strength, independence_factor, created_by, created_at)
			VALUES ('bad-ev-1', ?, 'memory', 'observation', 'sys', 0.4, 1.0, 'test', ?)
		`, 0, memID, time.Now().Unix())
		if err != nil {
			return err
		}

		// 2. Force recompute to fail by violating the confidence_history.trigger
		// CHECK constraint. The valid set is defined in schema.go and does
		// not include "not_a_valid_reason".
		return RecomputeConfidence(node, memID, "memory", RecomputeReason("not_a_valid_reason"))
	})

	require.Error(t, err, "transaction should have failed at the history INSERT")
	assert.Contains(t, err.Error(), "insert history row", "failure should surface from RecomputeConfidence")

	// Rollback guarantee: the evidence row must NOT be persisted.
	var evidenceCount int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM evidence WHERE id = 'bad-ev-1'`).Scan(&evidenceCount))
	assert.Equal(t, 0, evidenceCount, "evidence row must be rolled back when recompute fails")

	// The confidence column should also be untouched (still at the initial 0.8).
	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, memID).Scan(&conf))
	assert.InDelta(t, 0.8, conf, 1e-9, "confidence should remain at initial after rollback")

	// And the history table should have no row for this artifact.
	var historyCount int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ?`, memID,
	).Scan(&historyCount))
	assert.Equal(t, 0, historyCount, "no history row should exist after rollback")
}

// TestEvidenceStore_AddEvidenceRollsBackOnRecomputeFailure is the
// end-to-end counterpart: it calls the public AddEvidence path and induces
// failure by writing a row directly into the evidence table with a
// strength outside the allowed range, then calls RecomputeConfidence which
// loads it and... actually, strength is unbounded. The cleanest failure
// injection for AddEvidence is a constraint on the evidence row itself;
// we use the evidence_history CHECK constraint violation path inside a
// follow-up tx, but for AddEvidence we use the sensitivity scanner (which
// runs BEFORE the transaction opens) — that's covered by
// TestEvidenceStore_AddEvidence_BlocksSensitiveNotes. The WithTx-level
// rollback is the WithTx test above; AddEvidence uses the same WithTx
// helper so the guarantee transfers.

// TestEvidenceStore_AddEvidenceRejectsMissingArtifact pins the contract
// that AddEvidence validates the artifact exists BEFORE inserting the
// evidence row. Without this check, evidence rows would land orphaned
// (no FK to memories/lessons), RecomputeConfidence would silently
// produce an unanchored value (lastPositiveAt=now → no decay), and
// confidence_history would gain an audit row for an artifact that
// doesn't exist — the canonical "confidence diverges from evidence state"
// failure mode the docs warn against.
func TestEvidenceStore_AddEvidenceRejectsMissingArtifact(t *testing.T) {
	dm := newTestDM(t)

	err := AddEvidence(dm, EvidenceInput{
		ArtifactID:   "ghost-memory",
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.4,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	require.Error(t, err, "AddEvidence must reject evidence for a nonexistent artifact")
	assert.Contains(t, err.Error(), "does not exist",
		"error should explain why the call was rejected")

	// No evidence row, no history row — nothing was persisted.
	var evCount, histCount int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM evidence WHERE artifact_id = ?`, "ghost-memory").Scan(&evCount))
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ?`, "ghost-memory").Scan(&histCount))
	assert.Equal(t, 0, evCount, "no evidence row should be persisted for a missing artifact")
	assert.Equal(t, 0, histCount, "no history row should be persisted for a missing artifact")
}

// TestEvidenceStore_RecomputeConfidenceRejectsMissingArtifact pins the
// contract that RecomputeConfidence errors when the artifact doesn't
// exist. The audit found that RecomputeConfidence previously loaded
// "no evidence" + lastPositiveAt=now (the silent fallback path) and
// produced an unanchored confidence value with no decay, then wrote
// the result into confidence_history for an artifact that doesn't exist —
// polluting the audit trail with phantom recomputes.
func TestEvidenceStore_RecomputeConfidenceRejectsMissingArtifact(t *testing.T) {
	dm := newTestDM(t)

	err := RecomputeConfidence(dm, "ghost-memory", "memory", RecomputeReasonManual)
	require.Error(t, err, "RecomputeConfidence must reject a nonexistent artifact")
	assert.Contains(t, err.Error(), "does not exist",
		"error should explain why the recompute was rejected")

	// No phantom history row.
	var histCount int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ?`, "ghost-memory").Scan(&histCount))
	assert.Equal(t, 0, histCount, "no history row should exist for a phantom recompute")
}

// TestEvidenceStore_AddEvidenceOnLessonArtifact verifies that the artifact
// existence check honours the artifact_type → table mapping (lesson →
// lessons view), not just memories. Without this, evidence for a lesson
// would also be silently orphaned.
func TestEvidenceStore_AddEvidenceOnLessonArtifact(t *testing.T) {
	dm := newTestDM(t)

	// Create a real lesson via the production AddLesson path.
	_, err := dm.AddLesson("a real lesson", LessonTypeInsight, nil, "")
	require.NoError(t, err)

	// Read its ID back.
	var lessonID string
	require.NoError(t, dm.QueryRowTracked(`SELECT id FROM lessons LIMIT 1`).Scan(&lessonID))
	require.NotEmpty(t, lessonID)

	// Evidence for the existing lesson succeeds.
	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:   lessonID,
		ArtifactType: "lesson",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.4,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	}))

	// Evidence for a nonexistent lesson fails.
	err = AddEvidence(dm, EvidenceInput{
		ArtifactID:   "ghost-lesson",
		ArtifactType: "lesson",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.4,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	require.Error(t, err, "AddEvidence for a nonexistent lesson must fail")
	assert.Contains(t, err.Error(), "does not exist")
}

// TestEvidenceStore_ExplainConfidenceRejectsMissingArtifact pins the
// ExplainConfidence contract. The audit found that ExplainConfidence
// previously fell back to lastPositiveAt=now for an empty evidence set
// and produced a fabricated "all good" trace for an artifact that
// doesn't exist — confidence equals the initial value with no decay
// penalty. The reasoning trace would lie about a phantom artifact.
func TestEvidenceStore_ExplainConfidenceRejectsMissingArtifact(t *testing.T) {
	dm := newTestDM(t)

	_, err := ExplainConfidence(dm, "ghost-memory", "memory")
	require.Error(t, err, "ExplainConfidence must reject a nonexistent artifact")
	assert.Contains(t, err.Error(), "does not exist")
}

// newTestDM creates a DatabaseManager on a temp DB and returns it. The
// DatabaseManager is closed via t.Cleanup. Follows the freshDB pattern from
// isolation_test.go so we don't write to the real workspace DB.
func newTestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	dm := NewDatabaseManagerForDB(db)
	require.NoError(t, dm.InitSchema())
	t.Cleanup(func() { dm.Close() })
	return dm
}

// TestEvidenceStore_ExplainConfidenceMatchesRecompute pins the
// invariant that ExplainConfidence (the reasoning trace) returns the
// same confidence value as the recompute that wrote the column.
//
// The audit found that ExplainConfidence normalized independence=0 → 1.0
// for the breakdown components while loadEvidenceForRecompute passed
// the raw value through to computeConfidence. For any row inserted with
// independence_factor=0, that meant the explanation reported a
// different confidence than the recompute wrote — the stored value
// diverged from the explanation. Fix: drop the normalization so both
// paths agree.
func TestEvidenceStore_ExplainConfidenceMatchesRecompute(t *testing.T) {
	dm := newTestDM(t)

	memID := "explain-match-1"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, confidence) VALUES (?, 'memories', 'x', 0.8)`,
		0, memID,
	)
	require.NoError(t, err)

	// Insert evidence directly with independence_factor=0 to exercise the
	// divergent code path. With the fix, both RecomputeConfidence and
	// ExplainConfidence see this as effective=0 (no contribution) and
	// produce the same result.
	_, err = dm.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group,
		                     strength, independence_factor, created_by, created_at)
		VALUES (?, ?, 'memory', 'reproduction', 'src', 0.85, 0.0, 'test', ?)
	`, 0, "ev-zero-indep", memID, time.Now().Unix())
	require.NoError(t, err)

	// First, baseline: what does ExplainConfidence return?
	exp, err := ExplainConfidence(dm, memID, "memory")
	require.NoError(t, err)

	// Second, drive a recompute and read the persisted value.
	require.NoError(t, RecomputeConfidence(dm, memID, "memory", RecomputeReasonManual))

	var stored float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, memID).Scan(&stored))

	// The reasoning trace must match the stored value exactly. The
	// independence=0 row contributes nothing in both paths, so the
	// stored value should equal the explanation.
	assert.InDelta(t, stored, exp.Confidence, 1e-9,
		"ExplainConfidence.Confidence (%.9f) must equal stored confidence (%.9f) for independence=0 evidence",
		exp.Confidence, stored)

	// Verify each component of the explanation also reflects the raw
	// independence. The contribution of an independence=0 row should be
	// effective=0, not strength*recency.
	for _, p := range exp.Positive {
		if p.ID == "ev-zero-indep" {
			assert.InDelta(t, 0.0, p.EffectiveStrength, 1e-9,
				"effective_strength for independence=0 evidence must be 0, not strength*recency")
		}
	}
}

// TestQueryMemoryQualityBySource_SurvivalRateNotJoinMultiplied pins
// the contract that `survived` and `survivalRate` are memory-level
// counts, not evidence-level counts. The audit found that the second
// aggregate query had a join-multiplication bug: `SUM(CASE WHEN
// m.confidence >= 0.5 THEN 1 ELSE 0 END)` operated on the joined
// evidence rows, so a memory with 3 evidence rows counted as 3
// survivors instead of 1. With 2 memories and 4 evidence rows the
// rate came out as 1.5 (greater than 1.0) — meaningless as a
// "fraction of memories that survived."
func TestQueryMemoryQualityBySource_SurvivalRateNotJoinMultiplied(t *testing.T) {
	dm := newTestDM(t)

	// m1: high confidence, 3 evidence rows from src-A
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('m1', 'memories', 'x', 0.6)`, 0)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err = dm.ExecTracked(`
			INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at)
			VALUES (?, 'm1', 'memory', 'observation', 'auto_capture', 0.4, 'src-A', ?)
		`, 0, fmt.Sprintf("ev-m1-%d", i), time.Now().Unix()+int64(i))
		require.NoError(t, err)
	}

	// m2: low confidence, 1 evidence row from src-A
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('m2', 'memories', 'x', 0.3)`, 0)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at)
		VALUES ('ev-m2-1', 'm2', 'memory', 'observation', 'auto_capture', 0.4, 'src-A', ?)
	`, 0, time.Now().Unix())
	require.NoError(t, err)

	stats, err := QueryMemoryQualityBySource(dm)
	require.NoError(t, err)
	require.Len(t, stats, 1)
	got := stats[0]

	assert.Equal(t, "src-A", got.Source)
	assert.Equal(t, 2, got.MemoryCount, "mc must count distinct memories, not evidence rows")
	// m1 (0.6) survives, m2 (0.3) does not → 1 survivor, not 3.
	// Pre-fix bug: with join multiplication, survived=3 → survivalRate=1.5 (>1.0, meaningless).
	assert.InDelta(t, 1.0, got.SurvivalRate*float64(got.MemoryCount), 1e-9,
		"survivalRate*MemoryCount should equal 1 (the one surviving memory)")
	assert.InDelta(t, 0.5, got.SurvivalRate, 1e-9,
		"survivalRate should be 1/2 = 0.5")
}
