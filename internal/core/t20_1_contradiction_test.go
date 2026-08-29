// t20_1_contradiction_test.go — T20-1 alpha P1 regression
//
// T20-1: A single unsubstantiated challenge evidence row currently converts
// verified work into "contradicted" with NO corroboration requirement and
// NO recovery path. The audit fix must:
//
//  1. Require corroboration for established contradiction (parallel to
//     verification: aggregated negative observation strength ≤ -1.0 OR
//     single strong negative observation (≤ -0.7) OR designated verifier
//     type with negative strength).
//
//  2. A single type='challenge' row at default strength (-0.6) alone is a
//     "dispute", NOT an "established contradiction" — it must downgrade
//     verified to 'partial' (or any non-contradicted state) so the audit
//     flag is visible without forcing the work into an irreversible
//     terminal state.
//
//  3. Add a recovery path: after the dispute is resolved (the challenge
//     evidence is withdrawn/neutralized or fresh outcome evidence arrives
//     to outweigh the dispute), verification must re-derive to verified.
//
//  4. Historical evidence remains auditable — challenge rows are not
//     deleted; they are marked withdrawn via expires_at or a metadata
//     field so the audit trail is reconstructable.
//
// This file holds the regression tests that lock the invariant down.
package internal

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addChallengeEvidence seeds a single type='challenge' evidence row at the
// registry default strength (-0.6). This is the audit scenario: a lone
// unsubstantiated challenge.
func addChallengeEvidence(t *testing.T, dm *DatabaseManager, workID string) {
	t.Helper()
	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:   workID,
		ArtifactType: "work",
		Type:         "challenge",
		SourceGroup:  "manual_review",
		Strength:     -0.6,
		CreatedBy:    "t20-1-test",
		CreatedAt:    time.Now(),
	}))
}

// TestT20_1_SingleUnsubstantiatedChallengeDoesNotContradict is the core
// invariant: a lone challenge row at default strength (-0.6) on a
// previously-verified work item must NOT flip verification to
// 'contradicted'. The work must surface the dispute but not be permanently
// converted to "established contradiction" by a single unsubstantiated
// challenge.
func TestT20_1_SingleUnsubstantiatedChallengeDoesNotContradict(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	workID := seedOpenWork(t, dm, "T20-1 verified → disputed (not contradicted)")

	// Seed an outcome + designated verifier pair so the work verifies.
	addWorkEvidence(t, dm, workID, "filesystem", 0.8)
	addWorkEvidence(t, dm, workID, "test", 0.85)
	assert.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, workID),
		"setup: must start verified")

	// Lone unsubstantiated challenge row → must NOT contradict.
	addChallengeEvidence(t, dm, workID)
	v := workVerification(t, dm, workID)
	assert.NotEqual(t, string(WorkVerificationContradicted), v,
		"T20-1 REGRESSION: a single unsubstantiated challenge row must NOT establish "+
			"contradiction (was: flipped verified → contradicted, "+
			"permanent, no recovery). Got %q, want anything-but-contradicted.", v)
}

// TestT20_1_StrongNegativeObservationStillContradicts preserves the F12
// invariant: a strong negative observation (≤ -0.7) IS substantiated by
// its own weight, so verified → contradicted remains correct.
func TestT20_1_StrongNegativeObservationStillContradicts(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	workID := seedOpenWork(t, dm, "T20-1 strong negative still contradicts")

	addWorkEvidence(t, dm, workID, "filesystem", 0.8)
	addWorkEvidence(t, dm, workID, "test", 0.85)
	require.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, workID))

	// Strong negative observation: substantiated single source.
	addWorkEvidence(t, dm, workID, "manual_review", -0.85)
	assert.Equal(t, string(WorkVerificationContradicted), workVerification(t, dm, workID),
		"a strong negative observation (≤ -0.7) must still establish contradiction")
}

// TestT20_1_CorroboratedChallengeDoesContradict: two challenge rows
// (aggregated negative ≤ -1.0) corroborate each other and establish
// contradiction.
func TestT20_1_CorroboratedChallengeDoesContradict(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	workID := seedOpenWork(t, dm, "T20-1 corroborated challenge contradicts")

	addWorkEvidence(t, dm, workID, "filesystem", 0.8)
	addWorkEvidence(t, dm, workID, "test", 0.85)
	require.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, workID))

	// Two challenge rows: -0.6 + -0.6 = -1.2, crosses the negative
	// corroboration threshold (parallel to positive corroborationSum=1.0).
	addChallengeEvidence(t, dm, workID)
	addChallengeEvidence(t, dm, workID)
	assert.Equal(t, string(WorkVerificationContradicted), workVerification(t, dm, workID),
		"two corroborating challenge rows (aggregated ≤ -1.0) must establish contradiction")
}

// TestT20_1_RecoveryAfterResolve: after the dispute is resolved (the
// challenge row is withdrawn / neutralized), verification must re-derive
// back to verified. This proves the contradiction is NOT permanent.
func TestT20_1_RecoveryAfterResolve(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	workID := seedOpenWork(t, dm, "T20-1 recovery after resolve")

	// Verified baseline.
	addWorkEvidence(t, dm, workID, "filesystem", 0.8)
	addWorkEvidence(t, dm, workID, "test", 0.85)
	require.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, workID))

	// Lone dispute (downgraded, not contradicted).
	addChallengeEvidence(t, dm, workID)
	require.NotEqual(t, string(WorkVerificationContradicted), workVerification(t, dm, workID),
		"setup: must be downgraded (not contradicted) before resolve")

	// Resolve-contradiction: withdraw the challenge evidence so it no
	// longer counts toward the verification decision.
	require.NoError(t, dm.ResolveWorkContradiction(workID, "manual_review: spurious dispute"))

	// After resolution, the verification must re-derive to verified.
	assert.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, workID),
		"after ResolveWorkContradiction withdraws the dispute, verification must restore")
}

// TestT20_1_ResolveIsAuditable: resolution must NOT delete evidence rows
// (historical audit trail preserved). Rows are neutralized via expires_at
// or a withdrawal flag so they remain in the table for forensic review.
func TestT20_1_ResolveIsAuditable(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	workID := seedOpenWork(t, dm, "T20-1 auditability")
	addWorkEvidence(t, dm, workID, "filesystem", 0.8)
	addChallengeEvidence(t, dm, workID)
	challengeID := mustLastEvidenceID(t, dm, workID, "challenge")

	require.NoError(t, dm.ResolveWorkContradiction(workID, "auditability"))

	// Row still exists in the table (not deleted).
	var count int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM evidence WHERE id = ?`, challengeID).Scan(&count))
	assert.Equal(t, 1, count, "challenge row must remain in the table for audit trail")

	// But expires_at must be set so it no longer counts in derivation.
	var expiresAt sql.NullInt64
	require.NoError(t, dm.db.QueryRow(
		`SELECT expires_at FROM evidence WHERE id = ?`, challengeID).Scan(&expiresAt))
	assert.True(t, expiresAt.Valid, "resolved challenge row must carry expires_at (neutralized, not deleted)")
	assert.NotZero(t, expiresAt.Int64, "expires_at must be a non-zero unix epoch")
}

func mustLastEvidenceID(t *testing.T, dm *DatabaseManager, workID, evidenceType string) string {
	t.Helper()
	var id string
	require.NoError(t, dm.db.QueryRow(
		`SELECT id FROM evidence WHERE artifact_id = ? AND type = ? ORDER BY created_at DESC LIMIT 1`,
		workID, evidenceType).Scan(&id))
	require.NotEmpty(t, id, "expected an evidence row of type %q for work %q", evidenceType, workID)
	return id
}
