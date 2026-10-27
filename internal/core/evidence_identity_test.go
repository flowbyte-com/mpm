package internal

// evidence_identity_test.go — the regression suite for the D1/D5 correction.
//
// THE DEFECTS.
//
//   D1 — a duplicate evidence submission created a second row and raised the
//        artifact's confidence twice (measured: 0.5 → 0.6224593183800414 →
//        0.7310585651678245).
//   D5 — a repeated delivery of ONE observation was counted as independent
//        corroboration, because the substrate could not tell a delivery from
//        an observation.
//
// THE CORRECTION.
//
// The evidence row's id is derived from the observation's identity and the
// table's PRIMARY KEY enforces "at most one stored row per observation per
// artifact". See internal/core/evidence_identity.go for the model.
//
// These tests were promoted out of the `m5contract`-tagged suite when the
// correction landed: they are ordinary regression tests now, and they run in
// every green gate. The obsolete characterization tests that pinned the
// defective behaviour were retired in the same change.
//
// ISOLATION.
//
// Every test uses the shared M5 fixtures (m5_trust_helpers_test.go), which
// pin HOME, MPM_WORKSPACE and MPM_DB_PATH to disposable locations and open a
// disposable file-backed database at the canonical <workspace>/src/db/mpm.db
// path. No test here can reach an operator's live database.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// evidenceIdentityTestObservation is the canonical observation used by the
// delivery tests: one observation, submitted N times.
func evidenceIdentityTestObservation(artifactID, notes string) EvidenceInput {
	return EvidenceInput{
		ArtifactID:         artifactID,
		ArtifactType:       "theory",
		Type:               "observation",
		SourceGroup:        "tool_invocation",
		Strength:           0.5,
		IndependenceFactor: 1.0,
		CreatedBy:          "critic:probe",
		Notes:              notes,
	}
}

// evidenceIdentityRowCount counts the evidence rows recorded for an artifact.
func evidenceIdentityRowCount(t *testing.T, dm *DatabaseManager, artifactID string) int {
	t.Helper()
	return m5ScalarInt(t, dm.SQLDB(),
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ?`, artifactID)
}

// evidenceIdentityHistoryCount counts the confidence_history rows the ledger
// holds for an artifact with a given trigger.
func evidenceIdentityHistoryCount(t *testing.T, dm *DatabaseManager, artifactID, trigger string) int {
	t.Helper()
	return m5ScalarInt(t, dm.SQLDB(),
		`SELECT COUNT(*) FROM confidence_history
		  WHERE artifact_id = ? AND trigger = ?`, artifactID, trigger)
}

// ── 1. a repeated delivery does not inflate confidence ─────────────────────

// TestEvidenceIdentity_RepeatedDeliveryDoesNotInflateConfidence is the
// promoted D1 contract.
//
// Two byte-identical deliveries of ONE observation must leave the artifact's
// confidence where one delivery left it, and must leave exactly one evidence
// row. Before the correction this produced two rows and two confidence
// increases (0.5 → 0.6224593183800414 → 0.7310585651678245).
func TestEvidenceIdentity_RepeatedDeliveryDoesNotInflateConfidence(t *testing.T) {
	dm := m5NewDM(t)
	id := m5ProposePendingTheory(t, dm,
		"Scheduler heartbeat rename is not atomic across filesystems.",
		[]string{"evidence-identity", "repeated-delivery"})

	before := m5Confidence(t, dm, id)

	in := evidenceIdentityTestObservation(id, "source observation O-1")

	first, err := AddEvidenceWithOutcome(dm, in)
	if err != nil {
		t.Fatalf("first AddEvidence: %v", err)
	}
	afterFirst := m5Confidence(t, dm, id)

	if first.Duplicate {
		t.Errorf("the first delivery of an observation must not be reported as a replay")
	}

	second, err := AddEvidenceWithOutcome(dm, in)
	if err != nil {
		t.Fatalf("second AddEvidence (identical): %v", err)
	}
	afterSecond := m5Confidence(t, dm, id)

	if !second.Duplicate {
		t.Errorf("the second identical delivery must be reported as a replay")
	}
	if second.EvidenceID != first.EvidenceID {
		t.Errorf("a replay must land on the same evidence row: first=%q second=%q",
			first.EvidenceID, second.EvidenceID)
	}

	if rows := evidenceIdentityRowCount(t, dm, id); rows != 1 {
		t.Errorf("expected exactly 1 evidence row after two identical deliveries, got %d", rows)
	}
	if diff := afterSecond - afterFirst; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("expected the repeated delivery not to change confidence: %.9f -> %.9f",
			afterFirst, afterSecond)
	}
	if diff := afterFirst - before; diff <= 0 {
		t.Errorf("expected the FIRST delivery to raise confidence: %.9f -> %.9f",
			before, afterFirst)
	}
}

// ── 2. a retry preserves idempotency ──────────────────────────────────────

// TestEvidenceIdentity_RetryPreservesIdempotency proves the correction is
// durable, not in-process.
//
// A retry is the same delivery arriving again — after a crash, after a
// restart, after a collector re-reads its queue. The test closes the database
// and opens a FRESH manager on the same workspace, so the second delivery is
// made by a different process against the same on-disk state.
//
// Nothing may be added: no second row, no second confidence increase, and no
// second confidence_history row.
func TestEvidenceIdentity_RetryPreservesIdempotency(t *testing.T) {
	ws := m5IsolatedWorkspace(t)

	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}

	id := m5ProposePendingTheory(t, dm,
		"A retried observation must not count twice.",
		[]string{"evidence-identity", "retry"})

	in := evidenceIdentityTestObservation(id, "observation O-1")
	if _, err := AddEvidenceWithOutcome(dm, in); err != nil {
		t.Fatalf("first AddEvidence: %v", err)
	}
	afterFirst := m5Confidence(t, dm, id)
	historyAfterFirst := evidenceIdentityHistoryCount(t, dm, id, "evidence_added")

	if err := dm.Close(); err != nil {
		t.Fatalf("close dm: %v", err)
	}

	// A fresh manager on the same workspace: the restart.
	dm2, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("reopen NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm2.Close() })

	retry, err := AddEvidenceWithOutcome(dm2, in)
	if err != nil {
		t.Fatalf("retried AddEvidence: %v", err)
	}
	if !retry.Duplicate {
		t.Errorf("a retry after a restart must be recognised as a replay")
	}

	if rows := evidenceIdentityRowCount(t, dm2, id); rows != 1 {
		t.Errorf("expected exactly 1 evidence row after a retry across a restart, got %d", rows)
	}
	afterRetry := m5Confidence(t, dm2, id)
	if diff := afterRetry - afterFirst; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("expected a retry not to change confidence: %.9f -> %.9f",
			afterFirst, afterRetry)
	}
	if got := evidenceIdentityHistoryCount(t, dm2, id, "evidence_added"); got != historyAfterFirst {
		t.Errorf("expected a retry to append no confidence_history row: %d -> %d",
			historyAfterFirst, got)
	}
}

// ── 3. distinct independent evidence still raises confidence ──────────────

// TestEvidenceIdentity_DistinctIndependentEvidenceStillRaisesConfidence is
// the other half of D5: the correction must not flatten corroboration.
//
// Two DIFFERENT observations must produce two rows and two contributions —
// the second must still be worth something.
func TestEvidenceIdentity_DistinctIndependentEvidenceStillRaisesConfidence(t *testing.T) {
	dm := m5NewDM(t)
	id := m5ProposePendingTheory(t, dm,
		"Two distinct observations must read as independent corroboration.",
		[]string{"evidence-identity", "distinct"})

	before := m5Confidence(t, dm, id)

	if _, err := AddEvidenceWithOutcome(dm, evidenceIdentityTestObservation(id, "observation O-1")); err != nil {
		t.Fatalf("AddEvidence (O-1): %v", err)
	}
	afterFirst := m5Confidence(t, dm, id)

	if _, err := AddEvidenceWithOutcome(dm, evidenceIdentityTestObservation(id, "observation O-2")); err != nil {
		t.Fatalf("AddEvidence (O-2): %v", err)
	}
	afterSecond := m5Confidence(t, dm, id)

	if rows := evidenceIdentityRowCount(t, dm, id); rows != 2 {
		t.Errorf("expected 2 evidence rows for 2 distinct observations, got %d", rows)
	}
	if !(afterFirst > before) {
		t.Errorf("expected the first observation to raise confidence: %.9f -> %.9f",
			before, afterFirst)
	}
	if !(afterSecond > afterFirst) {
		t.Errorf("expected a second DISTINCT observation to raise confidence further: %.9f -> %.9f",
			afterFirst, afterSecond)
	}
}

// ── 4. identical text from independently identified observations ──────────

// TestEvidenceIdentity_IdenticalTextFromIdentifiedObservationsIsNotCollapsed
// pins the distinction the correction is built on.
//
// Two INDEPENDENT observations may carry identical text. What separates them
// is their identity, and an explicitly identified observation keeps its own
// identity even when its text matches another's exactly.
//
// So: same text + DIFFERENT observation keys = two observations. Same text +
// the SAME observation key = one observation.
func TestEvidenceIdentity_IdenticalTextFromIdentifiedObservationsIsNotCollapsed(t *testing.T) {
	dm := m5NewDM(t)
	id := m5ProposePendingTheory(t, dm,
		"Two independent observations may read the same.",
		[]string{"evidence-identity", "identified"})

	const identicalText = "the scheduler heartbeat rename is not atomic"

	first := evidenceIdentityTestObservation(id, identicalText)
	first.ObservationKey = "observation-A"

	second := evidenceIdentityTestObservation(id, identicalText)
	second.ObservationKey = "observation-B"

	before := m5Confidence(t, dm, id)

	out1, err := AddEvidenceWithOutcome(dm, first)
	if err != nil {
		t.Fatalf("AddEvidence (observation-A): %v", err)
	}
	afterFirst := m5Confidence(t, dm, id)

	out2, err := AddEvidenceWithOutcome(dm, second)
	if err != nil {
		t.Fatalf("AddEvidence (observation-B): %v", err)
	}
	afterSecond := m5Confidence(t, dm, id)

	if out2.Duplicate {
		t.Errorf("two observations with DIFFERENT identity keys must not be collapsed")
	}
	if out1.EvidenceID == out2.EvidenceID {
		t.Errorf("two observations with different keys must land on different rows: %q",
			out1.EvidenceID)
	}
	if rows := evidenceIdentityRowCount(t, dm, id); rows != 2 {
		t.Errorf("expected 2 evidence rows for 2 independently identified observations, got %d", rows)
	}
	if !(afterSecond > afterFirst) {
		t.Errorf("expected a second identified observation to raise confidence: %.9f -> %.9f",
			afterFirst, afterSecond)
	}
	if !(afterFirst > before) {
		t.Errorf("expected the first observation to raise confidence: %.9f -> %.9f",
			before, afterFirst)
	}

	// And the same key delivered again IS the same observation.
	replay, err := AddEvidenceWithOutcome(dm, first)
	if err != nil {
		t.Fatalf("AddEvidence (observation-A again): %v", err)
	}
	if !replay.Duplicate {
		t.Errorf("the same observation key delivered again must be a replay")
	}
	if rows := evidenceIdentityRowCount(t, dm, id); rows != 2 {
		t.Errorf("expected the replay to add no row, got %d rows", rows)
	}
}

// ── 5. conflicting evidence retains its intended influence ────────────────

// TestEvidenceIdentity_ConflictingEvidenceRetainsItsInfluence pins that the
// correction did not blunt the other direction.
//
// A contradicting observation is a DIFFERENT observation, and it must still
// pull the confidence down. Two observations that disagree are not a
// duplicate: they are a dispute.
func TestEvidenceIdentity_ConflictingEvidenceRetainsItsInfluence(t *testing.T) {
	dm := m5NewDM(t)
	id := m5ProposePendingTheory(t, dm,
		"A contradicting observation must still count against the claim.",
		[]string{"evidence-identity", "conflict"})

	positive := evidenceIdentityTestObservation(id, "reproduced on three machines")

	if _, err := AddEvidenceWithOutcome(dm, positive); err != nil {
		t.Fatalf("AddEvidence (positive): %v", err)
	}
	afterPositive := m5Confidence(t, dm, id)

	conflict := EvidenceInput{
		ArtifactID:         id,
		ArtifactType:       "theory",
		Type:               "challenge",
		SourceGroup:        "git",
		Strength:           -0.6,
		IndependenceFactor: 1.0,
		CreatedBy:          "critic:probe",
		Notes:              "reproduced on two of three machines",
	}
	if _, err := AddEvidenceWithOutcome(dm, conflict); err != nil {
		t.Fatalf("AddEvidence (conflict): %v", err)
	}
	afterConflict := m5Confidence(t, dm, id)

	if rows := evidenceIdentityRowCount(t, dm, id); rows != 2 {
		t.Errorf("expected the contradicting observation to be its own row, got %d rows", rows)
	}
	if !(afterConflict < afterPositive) {
		t.Errorf("expected a contradicting observation to lower confidence: %.9f -> %.9f",
			afterPositive, afterConflict)
	}

	// The contradicting row is still visible to the audit surfaces.
	rows, err := ListEvidenceForArtifact(dm, id, "theory")
	if err != nil {
		t.Fatalf("ListEvidenceForArtifact: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 evidence rows in the audit listing, got %d", len(rows))
	}
	var sawChallenge bool
	for _, r := range rows {
		if r.Type == "challenge" {
			sawChallenge = true
		}
	}
	if !sawChallenge {
		t.Errorf("the contradicting observation must remain visible in the evidence listing")
	}
}

// ── 6. repeated critic cycles do not manufacture corroboration ───────────

// TestEvidenceIdentity_RepeatedCriticCyclesDoNotManufactureCorroboration is
// the promoted D5 contract, in the shape the defect actually took.
//
// A critic that re-evaluates the same pending proposal on every cycle
// re-submits the SAME observation. Five cycles must produce one observation,
// one contribution, and one confidence_history row — not five of each.
func TestEvidenceIdentity_RepeatedCriticCyclesDoNotManufactureCorroboration(t *testing.T) {
	dm := m5NewDM(t)
	id := m5ProposePendingTheory(t, dm,
		"A repeated critic cycle must not read as independent corroboration.",
		[]string{"evidence-identity", "critic-cycles"})

	in := evidenceIdentityTestObservation(id, "observation O-1")

	var afterFirst float64
	for cycle := 1; cycle <= 5; cycle++ {
		out, err := AddEvidenceWithOutcome(dm, in)
		if err != nil {
			t.Fatalf("cycle %d: AddEvidence: %v", cycle, err)
		}
		if cycle == 1 {
			if out.Duplicate {
				t.Errorf("cycle 1 must record a new observation")
			}
			afterFirst = m5Confidence(t, dm, id)
			continue
		}
		if !out.Duplicate {
			t.Errorf("cycle %d must be recognised as a replay of cycle 1", cycle)
		}
		conf := m5Confidence(t, dm, id)
		if diff := conf - afterFirst; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("cycle %d changed confidence: %.9f -> %.9f", cycle, afterFirst, conf)
		}
	}

	if rows := evidenceIdentityRowCount(t, dm, id); rows != 1 {
		t.Errorf("expected 1 evidence row after 5 critic cycles, got %d", rows)
	}
	if got := evidenceIdentityHistoryCount(t, dm, id, "evidence_added"); got != 1 {
		t.Errorf("expected exactly 1 confidence_history row for 5 critic cycles, got %d", got)
	}
}

// ── 7. an invalid or ambiguous identity fails safely ─────────────────────

// TestEvidenceIdentity_InvalidIdentityFailsSafely pins that a malformed
// observation identity is refused BEFORE anything is written.
//
// A key that is whitespace, padded, control-bearing or oversized cannot
// identify an observation unambiguously, and a key that is silently
// normalized would merge two callers' observations. Both are refused, with
// zero partial state.
func TestEvidenceIdentity_InvalidIdentityFailsSafely(t *testing.T) {
	dm := m5NewDM(t)
	id := m5ProposePendingTheory(t, dm,
		"An unusable observation identity must be refused.",
		[]string{"evidence-identity", "invalid"})

	before := m5Confidence(t, dm, id)

	longKey := strings.Repeat("k", EvidenceObservationKeyMaxChars+1)
	for name, key := range map[string]string{
		"whitespace only":     "   ",
		"leading whitespace":  " obs-1",
		"trailing whitespace": "obs-1 ",
		"control character":   "obs-1\x07",
		"too long":            longKey,
	} {
		in := evidenceIdentityTestObservation(id, "observation O-1")
		in.ObservationKey = key
		if _, err := AddEvidenceWithOutcome(dm, in); err == nil {
			t.Errorf("an observation key that is %s must be refused", name)
		} else if !strings.Contains(err.Error(), "observation key") {
			t.Errorf("the error for a %s key must name the field: %v", name, err)
		}
	}

	if rows := evidenceIdentityRowCount(t, dm, id); rows != 0 {
		t.Errorf("a refused identity must leave no evidence row, got %d", rows)
	}
	if diff := m5Confidence(t, dm, id) - before; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("a refused identity must not change confidence: %.9f -> %.9f",
			before, m5Confidence(t, dm, id))
	}
}

// TestEvidenceIdentity_ContradictingIdentityIsRefused pins the other
// fail-safe case: a key that is already recorded, delivered with DIFFERENT
// content.
//
// That is not a replay — it is two different claims wearing one identity.
// The substrate refuses it rather than silently keeping one of the two, and
// it leaves no partial state behind.
func TestEvidenceIdentity_ContradictingIdentityIsRefused(t *testing.T) {
	dm := m5NewDM(t)
	id := m5ProposePendingTheory(t, dm,
		"One identity cannot carry two different claims.",
		[]string{"evidence-identity", "conflict"})

	first := evidenceIdentityTestObservation(id, "observation O-1")
	first.ObservationKey = "observation-A"
	if _, err := AddEvidenceWithOutcome(dm, first); err != nil {
		t.Fatalf("AddEvidence (observation-A): %v", err)
	}
	afterFirst := m5Confidence(t, dm, id)

	second := evidenceIdentityTestObservation(id, "a DIFFERENT observation")
	second.ObservationKey = "observation-A"
	if _, err := AddEvidenceWithOutcome(dm, second); err == nil {
		t.Errorf("expected a contradicting delivery under the same identity to be refused")
	}

	if rows := evidenceIdentityRowCount(t, dm, id); rows != 1 {
		t.Errorf("expected the refused delivery to add no row, got %d", rows)
	}
	if diff := m5Confidence(t, dm, id) - afterFirst; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("the refused delivery must not change confidence: %.9f -> %.9f",
			afterFirst, m5Confidence(t, dm, id))
	}
}

// ── 8. existing legitimate evidence remains accessible ────────────────────

// TestEvidenceIdentity_ExistingEvidenceRemainsAccessible pins that the
// correction did not orphan or hide anything.
//
// Legacy rows — written before the correction, with the random ids the old
// code minted — are still listed, still counted, and still carry their
// stored values. The correction is additive: no row is rewritten, no id
// changes, no historical evidence becomes unreachable.
func TestEvidenceIdentity_ExistingEvidenceRemainsAccessible(t *testing.T) {
	dm := m5NewDM(t)

	const legacyArtifact = "legacy-evidence-artifact"
	if _, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'legacy claim')`,
		0, legacyArtifact); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	// A legacy row, exactly as the old code wrote it: a random 16-char id
	// and no observation identity.
	const legacyID = "0123456789abcdef"
	if _, err := dm.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group,
		                      strength, independence_factor, created_by, created_at, notes)
		VALUES (?, ?, 'memory', 'observation', 'filesystem', 0.4, 1.0, 'legacy', ?, 'an old observation')
	`, 0, legacyID, legacyArtifact, time.Now().Unix()); err != nil {
		t.Fatalf("seed legacy evidence: %v", err)
	}

	rows, err := ListEvidenceForArtifact(dm, legacyArtifact, "memory")
	if err != nil {
		t.Fatalf("ListEvidenceForArtifact: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the legacy evidence row to remain visible, got %d rows", len(rows))
	}
	if rows[0].ID != legacyID {
		t.Errorf("the legacy row's id must not change: got %q want %q", rows[0].ID, legacyID)
	}
	if rows[0].Notes != "an old observation" {
		t.Errorf("the legacy row's notes must be preserved: %q", rows[0].Notes)
	}

	// It still counts: the recompute sees it.
	if err := RecomputeConfidence(dm, legacyArtifact, "memory", RecomputeReasonManual); err != nil {
		t.Fatalf("RecomputeConfidence: %v", err)
	}
	conf := m5Confidence(t, dm, legacyArtifact)
	if conf <= 0.8 {
		t.Errorf("expected the legacy evidence to still raise confidence above the 0.8 "+
			"initial value, got %.9f", conf)
	}

	// And a NEW observation on the same artifact lands beside it, with its
	// own derived identity.
	in := EvidenceInput{
		ArtifactID:         legacyArtifact,
		ArtifactType:       "memory",
		Type:               "observation",
		SourceGroup:        "filesystem",
		Strength:           0.4,
		IndependenceFactor: 1.0,
		CreatedBy:          "tester",
		Notes:              "a new observation",
	}
	out, err := AddEvidenceWithOutcome(dm, in)
	if err != nil {
		t.Fatalf("AddEvidence: %v", err)
	}
	if out.Duplicate {
		t.Errorf("a new observation must not be reported as a replay")
	}
	if !strings.HasPrefix(out.EvidenceID, evidenceRowIDPrefix) {
		t.Errorf("a new observation must land on a derived row id (prefix %q), got %q",
			evidenceRowIDPrefix, out.EvidenceID)
	}
	rows, err = ListEvidenceForArtifact(dm, legacyArtifact, "memory")
	if err != nil {
		t.Fatalf("ListEvidenceForArtifact: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("expected the legacy row and the new row to coexist, got %d rows", len(rows))
	}
}

// TestEvidenceIdentity_LegacyDuplicatesKeepTheirHistoricalWeight pins the
// historical limitation explicitly.
//
// Rows written BEFORE the correction carry no observation identity, and the
// correction does not rewrite history: they keep counting exactly as they
// did. A database that already holds duplicated evidence keeps its existing
// confidence values until an operator acts — no automatic recalculation is
// performed, because lowering a stored confidence is a policy decision, not
// a side effect of a bug fix.
func TestEvidenceIdentity_LegacyDuplicatesKeepTheirHistoricalWeight(t *testing.T) {
	dm := m5NewDM(t)

	const artifact = "legacy-duplicate-artifact"
	if _, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'legacy claim')`,
		0, artifact); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	for i, id := range []string{"legacydup00000001", "legacydup00000002"} {
		if _, err := dm.ExecTracked(`
			INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group,
			                      strength, independence_factor, created_by, created_at, notes)
			VALUES (?, ?, 'memory', 'observation', 'filesystem', 0.4, 1.0, 'legacy', ?, 'the same old observation')
		`, 0, id, artifact, time.Now().Unix()); err != nil {
			t.Fatalf("seed legacy evidence %d: %v", i, err)
		}
	}

	if err := RecomputeConfidence(dm, artifact, "memory", RecomputeReasonManual); err != nil {
		t.Fatalf("RecomputeConfidence: %v", err)
	}
	conf := m5Confidence(t, dm, artifact)

	// Two legacy rows of 0.4 each: the confidence reflects BOTH, exactly as
	// it did before the correction.
	single := 0.8 + 0.4
	if !(conf > 0.85) {
		t.Errorf("expected the two legacy rows to keep contributing as they did before the "+
			"correction (a single 0.4 row would land near %.2f), got %.9f", single, conf)
	}
}

// ── 9. confidence recalculation is stable across repeated execution ───────

// TestEvidenceIdentity_ConfidenceRecalculationIsStable pins that the derived
// value is a function of the evidence SET, not of how many times it was
// computed.
//
// The same evidence recomputed ten times must give the same number, and a
// replay must not append to the ledger.
//
// The tolerance is not zero because the confidence carries a time-decay term
// evaluated against the wall clock: recomputes a few milliseconds apart move
// the value by ~1e-11 (the same reason the reference-URL isolation test uses
// 1e-9). What is under test is that the recomputation is stable — not that
// the clock stands still.
func TestEvidenceIdentity_ConfidenceRecalculationIsStable(t *testing.T) {
	dm := m5NewDM(t)
	id := m5ProposePendingTheory(t, dm,
		"The derived confidence must be a function of the evidence set.",
		[]string{"evidence-identity", "stable"})

	in := evidenceIdentityTestObservation(id, "observation O-1")
	if _, err := AddEvidenceWithOutcome(dm, in); err != nil {
		t.Fatalf("AddEvidence: %v", err)
	}

	var first float64
	for i := 0; i < 10; i++ {
		if err := RecomputeConfidence(dm, id, "theory", RecomputeReasonManual); err != nil {
			t.Fatalf("RecomputeConfidence (round %d): %v", i, err)
		}
		conf := m5Confidence(t, dm, id)
		if i == 0 {
			first = conf
			continue
		}
		if diff := conf - first; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("recompute %d changed the confidence: %.15f -> %.15f", i, first, conf)
		}
	}

	// The stored value and the explanation agree — the reasoning trace
	// reads the same evidence set the recompute did.
	exp, err := ExplainConfidence(dm, id, "theory")
	if err != nil {
		t.Fatalf("ExplainConfidence: %v", err)
	}
	if diff := exp.Confidence - first; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("the explanation and the stored confidence disagree: %.15f vs %.15f",
			exp.Confidence, first)
	}
}

// ── the identity itself ───────────────────────────────────────────────────

// TestEvidenceIdentity_DerivedRowIDIsStableAndPrefixed pins the shape of the
// storage identity: the same observation always lands on the same row, the
// id is recognisable as derived, and a legacy id is never reused.
func TestEvidenceIdentity_DerivedRowIDIsStableAndPrefixed(t *testing.T) {
	dm := m5NewDM(t)
	id := m5ProposePendingTheory(t, dm,
		"The observation identity is the row identity.",
		[]string{"evidence-identity", "identity"})

	in := evidenceIdentityTestObservation(id, "observation O-1")
	first, err := AddEvidenceWithOutcome(dm, in)
	if err != nil {
		t.Fatalf("AddEvidence: %v", err)
	}
	if !strings.HasPrefix(first.EvidenceID, evidenceRowIDPrefix) {
		t.Errorf("a derived evidence id must carry the %q prefix, got %q",
			evidenceRowIDPrefix, first.EvidenceID)
	}

	// The identity is a pure function of the observation: deriving it twice
	// from the same submission gives the same id.
	identityA, err := deriveObservationIdentity(in)
	if err != nil {
		t.Fatalf("deriveObservationIdentity: %v", err)
	}
	identityB, err := deriveObservationIdentity(in)
	if err != nil {
		t.Fatalf("deriveObservationIdentity: %v", err)
	}
	if identityA.RowID != identityB.RowID || identityA.RowID != first.EvidenceID {
		t.Errorf("the derived identity must be stable: %q, %q, %q",
			identityA.RowID, identityB.RowID, first.EvidenceID)
	}
	if identityA.Mode != EvidenceObservationBestEffort {
		t.Errorf("a submission without a key must use the best_effort mode, got %q", identityA.Mode)
	}

	// A keyed submission is in strict mode, and its key decides the id.
	keyed := in
	keyed.ObservationKey = "observation-A"
	keyedIdentity, err := deriveObservationIdentity(keyed)
	if err != nil {
		t.Fatalf("deriveObservationIdentity (keyed): %v", err)
	}
	if keyedIdentity.Mode != EvidenceObservationStrict {
		t.Errorf("a keyed submission must use the strict mode, got %q", keyedIdentity.Mode)
	}
	if keyedIdentity.RowID == identityA.RowID {
		t.Errorf("a keyed observation must not share the identity of an unkeyed one")
	}

	// The stored id is the derived one, and it is what the audit surfaces
	// return.
	var storedID string
	if err := dm.QueryRowTracked(
		`SELECT id FROM evidence WHERE artifact_id = ?`, id).Scan(&storedID); err != nil {
		t.Fatalf("read stored id: %v", err)
	}
	if storedID != first.EvidenceID {
		t.Errorf("the stored row id must be the derived identity: %q vs %q",
			storedID, first.EvidenceID)
	}
}

// TestEvidenceIdentity_ReplayRecordsAMissingReferenceURL pins the one write a
// replay may cause.
//
// A reference URL is provenance about a source, and it is provably
// confidence-neutral. A replay that carries one the stored row does not have
// records it, without adding a row and without moving the confidence.
func TestEvidenceIdentity_ReplayRecordsAMissingReferenceURL(t *testing.T) {
	dm := m5NewDM(t)
	id := m5ProposePendingTheory(t, dm,
		"A replay must not lose the reference it carries.",
		[]string{"evidence-identity", "reference"})

	in := evidenceIdentityTestObservation(id, "observation O-1")
	if _, err := AddEvidenceWithOutcome(dm, in); err != nil {
		t.Fatalf("AddEvidence: %v", err)
	}
	afterFirst := m5Confidence(t, dm, id)

	withRef := in
	withRef.ReferenceURL = "https://example.com/spec"

	replay, err := AddEvidenceWithOutcome(dm, withRef)
	if err != nil {
		t.Fatalf("AddEvidence (replay with reference): %v", err)
	}
	if !replay.Duplicate {
		t.Errorf("the replay must still be recognised as a replay")
	}
	if rows := evidenceIdentityRowCount(t, dm, id); rows != 1 {
		t.Errorf("the replay must not add a row, got %d", rows)
	}

	var ref string
	if err := dm.QueryRowTracked(
		`SELECT reference_url FROM evidence WHERE artifact_id = ?`, id).Scan(&ref); err != nil {
		t.Fatalf("read reference_url: %v", err)
	}
	if ref != withRef.ReferenceURL {
		t.Errorf("the replay's reference must be recorded on the stored row: %q", ref)
	}
	if diff := m5Confidence(t, dm, id) - afterFirst; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("recording a reference must not move the confidence: %.9f -> %.9f",
			afterFirst, m5Confidence(t, dm, id))
	}
}

// TestEvidenceIdentity_ReplayDoesNotResurrectANeutralizedObservation pins the
// boundary between "idempotent" and "resurrecting".
//
// The challenge path (F7.1) deliberately expires a memory's evidence rows.
// A replay of one of those observations must not bring it back to life: the
// stored row stays authoritative, and the observation stays neutralized.
func TestEvidenceIdentity_ReplayDoesNotResurrectANeutralizedObservation(t *testing.T) {
	dm := m5NewDM(t)

	const artifact = "neutralized-evidence-artifact"
	if _, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'a claim under challenge')`,
		0, artifact); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	in := EvidenceInput{
		ArtifactID:         artifact,
		ArtifactType:       "memory",
		Type:               "observation",
		SourceGroup:        "filesystem",
		Strength:           0.4,
		IndependenceFactor: 1.0,
		CreatedBy:          "tester",
		Notes:              "observation O-1",
	}
	if _, err := AddEvidenceWithOutcome(dm, in); err != nil {
		t.Fatalf("AddEvidence: %v", err)
	}
	afterFirst := m5Confidence(t, dm, artifact)

	// The challenge path neutralizes the row.
	if _, err := dm.ExecTracked(
		`UPDATE evidence SET expires_at = ? WHERE artifact_id = ? AND expires_at IS NULL`,
		0, time.Now().Unix(), artifact); err != nil {
		t.Fatalf("neutralize evidence: %v", err)
	}
	if err := RecomputeConfidence(dm, artifact, "memory", RecomputeReasonManual); err != nil {
		t.Fatalf("RecomputeConfidence: %v", err)
	}
	afterNeutralized := m5Confidence(t, dm, artifact)
	if !(afterNeutralized < afterFirst) {
		t.Errorf("expected the neutralized observation to stop counting: %.9f -> %.9f",
			afterFirst, afterNeutralized)
	}

	// A replay must not resurrect it.
	if _, err := AddEvidenceWithOutcome(dm, in); err != nil {
		t.Fatalf("AddEvidence (replay): %v", err)
	}
	if err := RecomputeConfidence(dm, artifact, "memory", RecomputeReasonManual); err != nil {
		t.Fatalf("RecomputeConfidence: %v", err)
	}
	afterReplay := m5Confidence(t, dm, artifact)
	if diff := afterReplay - afterNeutralized; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("a replay must not resurrect a neutralized observation: %.9f -> %.9f",
			afterNeutralized, afterReplay)
	}
}

// TestEvidenceIdentity_WorkspacePathIsTheCanonicalOne is a cheap guard that
// the fixture really is a disposable file-backed database at the canonical
// workspace path — the isolation the whole suite depends on.
func TestEvidenceIdentity_WorkspacePathIsTheCanonicalOne(t *testing.T) {
	ws := m5IsolatedWorkspace(t)
	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })

	var mainPath string
	rows, err := dm.QueryTracked(`PRAGMA database_list`)
	if err != nil {
		t.Fatalf("PRAGMA database_list: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, path string
		if err := rows.Scan(&seq, &name, &path); err != nil {
			t.Fatalf("scan database_list: %v", err)
		}
		if name == "main" {
			mainPath = path
		}
	}
	if mainPath != filepath.Join(ws, "src", "db", "mpm.db") {
		t.Errorf("the disposable database must live at the canonical workspace path: %q",
			mainPath)
	}
}
