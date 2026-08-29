package internal

// f71_f81_cancellation_challenge_test.go — F7.1 / F8.1 P1-blocker regression
// tests.
//
// F7.1 (challenge-restoration): Challenge must not silently destroy history;
// restoration must not silently create verification.
//
// F8.1 (cancel-implies-verification): Cancellation must not produce or
// promote verification. Cancelled work MUST NOT surface as verified under
// any evidence pattern — the audit found that a cancelled work could
// satisfy a "successfully completed and verified" query because
// verification was decoupled from lifecycle status.
//
// Each test below reproduces a specific failure mode and asserts the
// post-fix invariants. Tests are kept hermetic (in-memory DM) so they
// never touch the workspace DB.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── F8.1 — Cancel/Verification Coupling ─────────────────────────────────

// TestF81_CancelledWorkVerificationCannotBeVerified reproduces the core
// failure mode: a work item with verified outcome evidence is then
// cancelled. Verification MUST immediately drop below 'verified' — the
// lifecycle-status gate in DeriveWorkVerification is structural and cannot
// be bypassed.
func TestF81_CancelledWorkVerificationCannotBeVerified(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("deploy service v2", "ship to prod", "session-1")
	require.NoError(t, err)

	// Add outcome evidence — should derive verified while open/done.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "filesystem",
		Strength:     0.9,
		CreatedBy:    "deploy-bot",
		CreatedAt:    time.Now(),
		Notes:        "binary deployed to /opt/svc/v2",
	})
	v, err := dm.DeriveWorkVerification(w.ID)
	require.NoError(t, err)
	require.Equal(t, WorkVerificationVerified, v,
		"sanity: open work with outcome evidence derives verified")

	// Cancel via the event-sourced path.
	_, err = dm.CancelWorkWithContext(w.ID, "deployment rolled back", ActiveContext{})
	require.NoError(t, err)

	// F8.1 invariant: verification MUST NOT be verified after cancellation.
	// Re-derive to test the gate (CancelWorkWithContext also re-derives,
	// but the explicit DeriveWorkVerification must also enforce the gate).
	v, err = dm.DeriveWorkVerification(w.ID)
	require.NoError(t, err)
	assert.NotEqual(t, WorkVerificationVerified, v,
		"cancelled work must NOT be verified (F8.1)")
	assert.Equal(t, WorkVerificationUnverified, v,
		"cancelled work with non-contradictory evidence must be unverified")
}

// TestF81_CancelledWorkWithPostCancelOutcomeEvidence reproduces the
// advanced failure: outcome evidence is added AFTER cancellation. A naive
// implementation would re-derive to 'verified' because outcome evidence
// alone is sufficient. The lifecycle gate MUST reject this.
func TestF81_CancelledWorkWithPostCancelOutcomeEvidence(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("ship parser fix", "fix the regex", "session-1")
	require.NoError(t, err)

	// Cancel first (no prior evidence).
	_, err = dm.CancelWorkWithContext(w.ID, "decided not to ship", ActiveContext{})
	require.NoError(t, err)

	// Now add outcome evidence. Naive implementation: derives 'verified'.
	// F8.1: cancelled status must short-circuit to 'unverified'.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "filesystem",
		Strength:     0.95,
		CreatedBy:    "ci",
		CreatedAt:    time.Now(),
		Notes:        "fix landed in mainline",
	})

	got := workVerification(t, dm, w.ID)
	assert.NotEqual(t, string(WorkVerificationVerified), got,
		"cancelled work must NEVER be verified, even if outcome evidence arrives post-cancel (F8.1)")
	assert.Equal(t, string(WorkVerificationUnverified), got,
		"cancelled work with post-cancel outcome evidence must be unverified")
}

// TestF81_CancelledWorkContradictedStillContradicted verifies the
// contradicted branch of the gate: cancellation locks below verified, but
// contradictory evidence still surfaces as 'contradicted' (the explicit
// failure is still auditable).
func TestF81_CancelledWorkContradictedStillContradicted(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("enable aggressive caching", "perf experiment", "session-1")
	require.NoError(t, err)

	// Add contradictory evidence before cancel.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "manual_review",
		Strength:     -0.95,
		CreatedBy:    "reviewer",
		CreatedAt:    time.Now(),
		Notes:        "cache hit ratio dropped from 80% to 12%",
	})

	_, err = dm.CancelWorkWithContext(w.ID, "rolled back", ActiveContext{})
	require.NoError(t, err)

	got := workVerification(t, dm, w.ID)
	assert.Equal(t, string(WorkVerificationContradicted), got,
		"cancelled work with contradictory evidence must still be contradicted")
}

// TestF81_CancelIsIdempotentForVerification confirms that calling cancel
// twice does not silently re-execute: the first cancel succeeds, the
// second is rejected by the F-B1 state machine (cancelled → cancelled is
// forbidden). The verification state at the end is whatever the FIRST
// cancel derived — there is no "second cancel" to race against because
// the state machine blocks it.
func TestF81_CancelIsIdempotentForVerification(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("retry test", "", "session-1")
	require.NoError(t, err)

	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "api_response",
		Strength:     0.9,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})

	// First cancel: succeeds.
	_, err = dm.CancelWorkWithContext(w.ID, "first", ActiveContext{})
	require.NoError(t, err)

	// Second cancel: rejected by F-B1. The state machine forbids
	// cancelled → cancelled; the test now pins that the substrate
	// surfaces this rather than silently no-op'ing.
	_, err = dm.CancelWorkWithContext(w.ID, "second", ActiveContext{})
	require.Error(t, err, "second cancel must be rejected per F-B1 state machine")
	assert.Contains(t, err.Error(), "invalid transition")

	v := workVerification(t, dm, w.ID)
	assert.Equal(t, string(WorkVerificationUnverified), v,
		"verification reflects the FIRST cancel; no second cancel ran")
}

// TestF81_OpenWorkReachesVerifiedAfterCancelThenReopen verifies the
// reopen path: a cancelled work that is reopened becomes eligible for
// verification promotion again. The reopen call must re-derive
// verification so the work reflects its fresh open state.
func TestF81_OpenWorkReachesVerifiedAfterCancelThenReopen(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("retry after fix", "", "session-1")
	require.NoError(t, err)

	// Outcome evidence BEFORE cancel — would normally derive verified.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.9,
		CreatedBy:    "ci",
		CreatedAt:    time.Now(),
	})

	_, err = dm.CancelWorkWithContext(w.ID, "first attempt failed", ActiveContext{})
	require.NoError(t, err)
	assert.Equal(t, string(WorkVerificationUnverified), workVerification(t, dm, w.ID),
		"cancelled work with prior outcome evidence must be unverified")

	// Reopen — verification must re-derive. Outcome evidence from BEFORE
	// cancel is still in the table (it's the audit trail) but with
	// status='open', the gate no longer blocks promotion.
	_, err = dm.ReopenWorkWithContext(w.ID, ActiveContext{})
	require.NoError(t, err)

	// Add fresh outcome evidence so we know we're not just observing
	// stale state.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.95,
		CreatedBy:    "ci",
		CreatedAt:    time.Now(),
	})

	assert.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, w.ID),
		"reopened work with fresh outcome evidence must reach verified")
}

// TestF81_CompleteKeepsVerifiedWhenOutcomeEvidencePresent confirms the
// "happy path" is unbroken: a work that's completed (not cancelled) with
// outcome evidence must still reach verified.
func TestF81_CompleteKeepsVerifiedWhenOutcomeEvidencePresent(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("happy path", "", "session-1")
	require.NoError(t, err)

	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "filesystem",
		Strength:     0.9,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})

	_, err = dm.CompleteWorkWithContext(w.ID, "all good", ActiveContext{})
	require.NoError(t, err)
	assert.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, w.ID),
		"completed work with outcome evidence must remain verified (F8.1 happy path)")
}

// TestF81_LegacyCancelWorkPathCouplesVerification confirms the non-
// event-sourced CancelWork also keeps verification in lockstep.
func TestF81_LegacyCancelWorkPathCouplesVerification(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("legacy cancel test", "", "session-1")
	require.NoError(t, err)

	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "filesystem",
		Strength:     0.9,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})

	// Use the legacy direct cancel path.
	_, err = dm.CancelWork(w.ID)
	require.NoError(t, err)

	assert.NotEqual(t, string(WorkVerificationVerified), workVerification(t, dm, w.ID),
		"legacy CancelWork must also keep verification coupled to status (F8.1)")
}

// TestF81_CancelAfterCompleteDowngrades verifies: a work that was
// completed and verified is then cancelled (e.g., rollback after
// verification). Verification must downgrade below verified.
func TestF81_CancelAfterCompleteDowngrades(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("cancel after complete", "", "session-1")
	require.NoError(t, err)

	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "filesystem",
		Strength:     0.9,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})

	_, err = dm.CompleteWorkWithContext(w.ID, "done", ActiveContext{})
	require.NoError(t, err)
	require.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, w.ID))

	// F-B1: done → cancelled is FORBIDDEN. Operators must explicitly
	// reopen the work before cancelling it. The two-step pattern is the
	// rollback-in-prod workflow: complete → reopen → cancel. The state
	// machine prevents silent cancel-after-complete, which F8.1 had
	// silently allowed as a no-op (the bypass this commit fixes).
	_, err = dm.CancelWorkWithContext(w.ID, "skip reopen", ActiveContext{})
	require.Error(t, err, "done → cancelled must be rejected; reopen first")
	assert.Contains(t, err.Error(), "invalid transition")

	// Reopen (done → open is allowed), then cancel.
	_, err = dm.UpdateWorkWithContext(w.ID, "", "", string(WorkStatusOpen), ActiveContext{})
	require.NoError(t, err)

	_, err = dm.CancelWorkWithContext(w.ID, "rolled back after reopen", ActiveContext{})
	require.NoError(t, err)

	assert.NotEqual(t, string(WorkVerificationVerified), workVerification(t, dm, w.ID),
		"cancel after complete-then-reopen must downgrade verification (F8.1)")
}

// ── F7.1 — Challenge/Restoration ───────────────────────────────────────

// TestF71_ChallengeNeutralizesEvidenceAndDropsConfidence confirms the
// new F7.1 invariant: when a memory is challenged, existing evidence is
// neutralized (expires_at = now), and the memory's confidence is dropped
// to the floor. The pre-challenge value is preserved in metadata for
// audit but the live confidence reflects the challenged state.
func TestF71_ChallengeNeutralizesEvidenceAndDropsConfidence(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	store := newMemoryStore(dm)
	mem, err := store.AddMemory("auth flow", "memories", nil, nil, "", "test")
	require.NoError(t, err)

	// Add positive evidence that pushes confidence above the floor.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   mem.ID,
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.9,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	// Sanity: confidence was bumped by AddEvidence's recompute.
	var beforeConf float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, mem.ID).Scan(&beforeConf))
	require.Greater(t, beforeConf, ChallengedMemoryConfidenceFloor,
		"sanity: pre-challenge confidence should exceed the floor")

	// Challenge the memory.
	require.NoError(t, dm.ChallengeMemory(mem.ID, 2, "outdated"))

	// F7.1: confidence MUST be at the floor after challenge.
	var afterConf float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, mem.ID).Scan(&afterConf))
	assert.Equal(t, ChallengedMemoryConfidenceFloor, afterConf,
		"challenged memory's confidence must be at the F7.1 floor")

	// F7.1: evidence rows are neutralized (expires_at set).
	var activeCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND artifact_type = 'memory' AND expires_at IS NULL`,
		mem.ID).Scan(&activeCount))
	assert.Zero(t, activeCount,
		"all evidence rows must be neutralized during challenge (F7.1)")

	// Audit trail: prior_confidence and challenged_at are stamped.
	var metaStr string
	require.NoError(t, dm.db.QueryRow(
		`SELECT metadata FROM memories WHERE id = ?`, mem.ID).Scan(&metaStr))
	assert.Contains(t, metaStr, "challenged_at",
		"challenge must stamp challenged_at for audit (F7.1)")
	assert.Contains(t, metaStr, "challenged_prior_confidence",
		"challenge must capture prior_confidence for forensic (F7.1)")
}

// TestF71_RestoreDoesNotPromoteConfidence confirms: restoration reverts
// the weight and clears the operational flags, but does NOT silently
// re-elevate confidence to its pre-challenge value. Fresh evidence is
// required.
func TestF71_RestoreDoesNotPromoteConfidence(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	store := newMemoryStore(dm)
	mem, err := store.AddMemory("restore target", "memories", nil, nil, "", "test")
	require.NoError(t, err)

	AddEvidence(dm, EvidenceInput{
		ArtifactID:   mem.ID,
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.95,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	var beforeConf float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, mem.ID).Scan(&beforeConf))
	require.Greater(t, beforeConf, ChallengedMemoryConfidenceFloor)

	require.NoError(t, dm.ChallengeMemory(mem.ID, 1, "test"))

	// We need a theory row for the restore path; the CLI version creates
	// one inside the same transaction. For the in-memory direct test, we
	// simulate the restore by directly applying the same patch+weight
	// semantics the CLI handler applies.
	require.NoError(t, simulateRestoreMemory(dm, mem.ID))

	var afterRestoreConf float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, mem.ID).Scan(&afterRestoreConf))

	assert.Equal(t, ChallengedMemoryConfidenceFloor, afterRestoreConf,
		"restored memory must remain at the F7.1 floor — restoration ≠ verification (F7.1)")
	assert.Less(t, afterRestoreConf, beforeConf,
		"restored confidence must be strictly less than the pre-challenge value")

	// Audit trail: restored_from_challenge / restored_at are stamped.
	var metaStr string
	require.NoError(t, dm.db.QueryRow(
		`SELECT metadata FROM memories WHERE id = ?`, mem.ID).Scan(&metaStr))
	assert.Contains(t, metaStr, "restored_from_challenge",
		"restoration must stamp restored_from_challenge for audit (F7.1)")
	assert.Contains(t, metaStr, "challenged_prior_confidence",
		"prior_confidence must be KEPT in metadata for the forensic trail")
}

// TestF71_NewEvidenceAfterRestoreReElevatesConfidence confirms the
// happy path: after a challenge/restore cycle, fresh evidence DOES
// re-elevate confidence. The F7.1 invariant is that restoration alone
// doesn't promote — but a legitimate new evidence event does.
func TestF71_NewEvidenceAfterRestoreReElevatesConfidence(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	store := newMemoryStore(dm)
	mem, err := store.AddMemory("re-elevation target", "memories", nil, nil, "", "test")
	require.NoError(t, err)

	AddEvidence(dm, EvidenceInput{
		ArtifactID:   mem.ID,
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.9,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})

	require.NoError(t, dm.ChallengeMemory(mem.ID, 1, "test"))
	require.NoError(t, simulateRestoreMemory(dm, mem.ID))

	// Fresh evidence after restore.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   mem.ID,
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.95,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})

	var conf float64
	require.NoError(t, dm.db.QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, mem.ID).Scan(&conf))
	assert.Greater(t, conf, ChallengedMemoryConfidenceFloor,
		"fresh evidence after restore must re-elevate confidence (F7.1 happy path)")
}

// TestF71_ChallengeEvidenceAuditTrailIsPreserved confirms the evidence
// rows remain in the table after challenge (not deleted) so the audit
// trail is reconstructable.
func TestF71_ChallengeEvidenceAuditTrailIsPreserved(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	store := newMemoryStore(dm)
	mem, err := store.AddMemory("audit trail target", "memories", nil, nil, "", "test")
	require.NoError(t, err)

	// Snapshot the row count before challenge.
	var beforeCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND artifact_type = 'memory'`,
		mem.ID).Scan(&beforeCount))

	AddEvidence(dm, EvidenceInput{
		ArtifactID:   mem.ID,
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.9,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})

	require.NoError(t, dm.ChallengeMemory(mem.ID, 1, "test"))

	// Evidence rows are still present (audit trail intact) — the count
	// after challenge must equal the count before challenge + the new
	// evidence row that was added. F7.1 invariant: no row is deleted.
	var afterCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND artifact_type = 'memory'`,
		mem.ID).Scan(&afterCount))
	assert.Equal(t, beforeCount+1, afterCount,
		"challenge must NOT delete evidence rows — audit trail preserved (F7.1)")

	// But all evidence rows are expired — loadEvidenceForRecompute must
	// skip them. This is the live recompute invariant: the challenged
	// memory cannot derive a high confidence from prior evidence.
	var activeCount int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ? AND artifact_type = 'memory'
		 AND (expires_at IS NULL OR expires_at > CAST(strftime('%s','now') AS INTEGER))`,
		mem.ID).Scan(&activeCount))
	assert.Zero(t, activeCount,
		"neutralized evidence must be inactive for recompute (F7.1)")
}

// TestF71_RepeatedChallengeRestoreCyclesStable confirms: repeated
// challenge/restore cycles converge to a stable state, not a slow drift.
// F7.1 invariant: history is preserved, but the live confidence stays at
// the floor until fresh evidence arrives.
func TestF71_RepeatedChallengeRestoreCyclesStable(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	store := newMemoryStore(dm)
	mem, err := store.AddMemory("cycle target", "memories", nil, nil, "", "test")
	require.NoError(t, err)

	AddEvidence(dm, EvidenceInput{
		ArtifactID:   mem.ID,
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.9,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})

	for i := 0; i < 3; i++ {
		require.NoError(t, dm.ChallengeMemory(mem.ID, 1, "cycle"))
		require.NoError(t, simulateRestoreMemory(dm, mem.ID))
		var conf float64
		require.NoError(t, dm.db.QueryRow(
			`SELECT confidence FROM memories WHERE id = ?`, mem.ID).Scan(&conf))
		assert.Equal(t, ChallengedMemoryConfidenceFloor, conf,
			"cycle %d: confidence must remain at the floor (F7.1)", i)
	}
}

// TestF81_AdversarialRunCancelVerifyRestoreSequence reproduces the
// adversarial event sequence from the audit brief:
//
//	RUN → CANCEL → VERIFY → RESTORE → VERIFY
//
// The system must apply its state/causality rules and reject, ignore, or
// record invalid transitions. The result MUST NEVER accidentally become
// verified merely because a verification event arrived after a cancelled
// state.
func TestF81_AdversarialRunCancelVerifyRestoreSequence(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("adversarial sequence", "", "session-1")
	require.NoError(t, err)

	// RUN: outcome evidence arrives while open.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "filesystem",
		Strength:     0.9,
		CreatedBy:    "ci",
		CreatedAt:    time.Now(),
	})
	require.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, w.ID),
		"sanity: outcome evidence on open work derives verified")

	// CANCEL
	_, err = dm.CancelWorkWithContext(w.ID, "rolled back", ActiveContext{})
	require.NoError(t, err)
	assert.Equal(t, string(WorkVerificationUnverified), workVerification(t, dm, w.ID),
		"after CANCEL: must drop below verified (F8.1)")

	// VERIFY (attempts to verify via a fresh evidence event)
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "api_response",
		Strength:     0.95,
		CreatedBy:    "monitor",
		CreatedAt:    time.Now(),
	})
	assert.NotEqual(t, string(WorkVerificationVerified), workVerification(t, dm, w.ID),
		"VERIFY after CANCEL must NOT silently promote verification (F8.1)")

	// RESTORE (reopen via the lifecycle status — explicit user reactivation)
	_, err = dm.ReopenWorkWithContext(w.ID, ActiveContext{})
	require.NoError(t, err)
	// After RESTORE, fresh VERIFY events should now be eligible to verify.
	assert.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, w.ID),
		"after RESTORE (reopen), evidence-based derivation is allowed again")

	// VERIFY (final event in sequence) — must remain stable
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.95,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	assert.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, w.ID),
		"after final VERIFY on a restored work, status remains verified")
}

// TestF71_AdversarialChallengeRestoreVerifyRace reproduces the audit
// brief's adversarial challenge sequence: the system must NOT silently
// allow restored state to masquerade as verified.
func TestF71_AdversarialChallengeRestoreVerifyRace(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	store := newMemoryStore(dm)
	mem, err := store.AddMemory("challenge restore race", "memories", nil, nil, "", "test")
	require.NoError(t, err)

	// CHALLENGE — high-confidence memory, evidence neutralized.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   mem.ID,
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.95,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	var beforeConf float64
	require.NoError(t, dm.db.QueryRow(`SELECT confidence FROM memories WHERE id = ?`, mem.ID).Scan(&beforeConf))
	require.Greater(t, beforeConf, ChallengedMemoryConfidenceFloor)

	require.NoError(t, dm.ChallengeMemory(mem.ID, 2, "outdated"))
	require.Equal(t, ChallengedMemoryConfidenceFloor, readConfidence(t, dm, mem.ID))

	// RESTORE — weight goes back, but confidence stays at floor.
	require.NoError(t, simulateRestoreMemory(dm, mem.ID))
	require.Equal(t, ChallengedMemoryConfidenceFloor, readConfidence(t, dm, mem.ID),
		"after RESTORE: confidence must stay at the F7.1 floor")

	// VERIFY (post-restore evidence) — must re-elevate.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   mem.ID,
		ArtifactType: "memory",
		Type:         "observation",
		SourceGroup:  "test",
		Strength:     0.95,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	var afterConf float64
	require.NoError(t, dm.db.QueryRow(`SELECT confidence FROM memories WHERE id = ?`, mem.ID).Scan(&afterConf))
	assert.Greater(t, afterConf, ChallengedMemoryConfidenceFloor,
		"VERIFY (fresh evidence after restore) must re-elevate confidence")

	// The forensic trail must be preserved across the entire sequence.
	var metaStr string
	require.NoError(t, dm.db.QueryRow(`SELECT metadata FROM memories WHERE id = ?`, mem.ID).Scan(&metaStr))
	assert.Contains(t, metaStr, "challenged_prior_confidence",
		"forensic trail: prior_confidence must persist through restore (F7.1)")
	assert.Contains(t, metaStr, "restored_from_challenge",
		"forensic trail: restored_from_challenge must persist (F7.1)")
}

func readConfidence(t *testing.T, dm *DatabaseManager, id string) float64 {
	t.Helper()
	var c float64
	require.NoError(t, dm.db.QueryRow(`SELECT confidence FROM memories WHERE id = ?`, id).Scan(&c))
	return c
}

// newMemoryStore constructs a MemoryStore wired to the in-memory DM.
// Mirrors the cmd/mpm/call_evidence_test.go pattern.
func newMemoryStore(dm *DatabaseManager) *MemoryStore {
	return &MemoryStore{DM: dm, DB: &SQLiteConnection{DB: dm.SQLDB()}}
}

// simulateRestoreMemory replays the operations the runChallengeRestore
// CLI handler performs, so unit tests in the core package can exercise
// the F7.1 invariants without depending on the cmd/mpm surface.
//
// It is intentionally a thin shim: it reads the challenged metadata,
// stamps the audit trail, and resets confidence to the floor. Weight is
// restored from challenged_prior_weight.
func simulateRestoreMemory(dm *DatabaseManager, id string) error {
	tx, err := dm.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Read prior weight and challenge theory id from metadata.
	var metaStr string
	if err := tx.QueryRow(`SELECT metadata FROM memories WHERE id = ? AND deleted_at IS NULL`, id).Scan(&metaStr); err != nil {
		return err
	}
	var meta map[string]interface{}
	if metaStr != "" {
		if err := json.Unmarshal([]byte(metaStr), &meta); err != nil {
			return err
		}
	}
	priorWeight, hasPrior := meta["challenged_prior_weight"].(float64)
	if !hasPrior {
		priorWeight = 1
	}

	// Apply the same patch semantics as runChallengeRestore: clear the
	// operational flags, stamp audit trail, restore weight, leave
	// confidence at the floor.
	patch := map[string]interface{}{
		"status":                  nil,
		"challenged_theory_id":    nil,
		"challenged_prior_weight": nil,
		"restored_from_challenge": true,
		"restored_at":             time.Now().Unix(),
	}
	patchJSON, _ := jsonMarshal(patch)
	if _, err := tx.Exec(
		`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?), weight = ?, confidence = ? WHERE id = ? AND deleted_at IS NULL`,
		string(patchJSON), int(priorWeight), ChallengedMemoryConfidenceFloor, id,
	); err != nil {
		return err
	}
	return tx.Commit()
}