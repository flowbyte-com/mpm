// f6_f7_f12_evidence_regression_test.go — regressions for audit findings
// F6 (unknown source_group silently accepted), F7 (post-completion outcome
// evidence requires re-completion) and F12 (post-hoc contradiction does not
// transition verified → contradicted).
package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedOpenWork(t *testing.T, dm *DatabaseManager, title string) string {
	t.Helper()
	w, err := dm.AddWork(title, "", "")
	require.NoError(t, err)
	return w.ID
}

func workVerification(t *testing.T, dm *DatabaseManager, workID string) string {
	t.Helper()
	var v string
	require.NoError(t, dm.db.QueryRow(`SELECT verification FROM works WHERE id = ?`, workID).Scan(&v))
	return v
}

func addWorkEvidence(t *testing.T, dm *DatabaseManager, workID, sourceGroup string, strength float64) {
	t.Helper()
	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:   workID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  sourceGroup,
		Strength:     strength,
		CreatedBy:    "f7-f12-test",
		CreatedAt:    time.Now(),
	}))
}

// ── F6 ───────────────────────────────────────────────────────────────────

func TestF6_UnknownSourceGroupRejectedWithNoPartialState(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	workID := seedOpenWork(t, dm, "f6 target")

	err := AddEvidence(dm, EvidenceInput{
		ArtifactID:   workID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "my-custom-vibe", // unrecognized
		Strength:     0.9,
		CreatedBy:    "f6",
		CreatedAt:    time.Now(),
	})
	require.Error(t, err, "unknown source_group must be rejected")
	assert.Contains(t, err.Error(), "invalid source_group")
	assert.Contains(t, err.Error(), "my-custom-vibe", "error must echo the offending value")
	assert.Contains(t, err.Error(), "filesystem", "error must enumerate accepted values")

	// No partial state: nothing persisted.
	var n int
	require.NoError(t, dm.db.QueryRow(`SELECT COUNT(*) FROM evidence WHERE artifact_id = ?`, workID).Scan(&n))
	assert.Zero(t, n, "rejected evidence must leave no evidence row")

	// Missing and malformed values are rejected too.
	for _, bad := range []string{"", "GIT", "test ", "git;--"} {
		err := AddEvidence(dm, EvidenceInput{
			ArtifactID: workID, ArtifactType: "work", Type: "observation",
			SourceGroup: bad, Strength: 0.5, CreatedBy: "f6", CreatedAt: time.Now(),
		})
		assert.Error(t, err, "source_group %q must be rejected", bad)
	}
}

func TestF6_AllRegistryValuesAcceptedAndDiscoverable(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	for _, sg := range []string{"filesystem", "test", "api_response", "manual_review",
		"git", "ci", "external", "tool_invocation", "api_call", "process"} {
		assert.True(t, ValidEvidenceSourceGroup(sg), "%q must be valid", sg)
	}
	// Class partitioning is exhaustive over the registry.
	total := len(EvidenceSourceGroupOfClass(SourceGroupClassOutcome)) +
		len(EvidenceSourceGroupOfClass(SourceGroupClassAudit)) +
		len(EvidenceSourceGroupOfClass(SourceGroupClassAction))
	assert.Equal(t, len(evidenceSourceGroupRegistry), total)
}

// ── F7/F12 ───────────────────────────────────────────────────────────────

func TestF7_PostCompletionOutcomeEvidenceUpdatesVerification(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	workID := seedOpenWork(t, dm, "ship the parser fix")

	// Complete with only git audit evidence → partial.
	_, err := dm.CompleteWork(workID)
	require.NoError(t, err)
	addWorkEvidence(t, dm, workID, "git", 0.6)
	v, err := dm.DeriveWorkVerification(workID)
	require.NoError(t, err)
	assert.Equal(t, string(WorkVerificationPartial), string(v))

	// Post-completion OUTCOME evidence arrives → verified WITHOUT re-running
	// complete. This is the F7 reproduction.
	addWorkEvidence(t, dm, workID, "test", 0.9)
	assert.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, workID),
		"outcome evidence added after completion must upgrade derived verification")
}

func TestF12_PostHocContradictionTransitionsVerifiedToContradicted(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	workID := seedOpenWork(t, dm, "enable aggressive caching")

	addWorkEvidence(t, dm, workID, "filesystem", 0.8)
	assert.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, workID))

	// Post-hoc contradiction (strong negative strength) — the audited gap:
	// verification must transition automatically.
	addWorkEvidence(t, dm, workID, "manual_review", -0.95)
	assert.Equal(t, string(WorkVerificationContradicted), workVerification(t, dm, workID),
		"a post-hoc contradiction must demote a verified work automatically")

	// The evidence_observed ledger event is inspectable (dormant vocabulary
	// now exercised).
	var events int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM work_events WHERE work_id = ? AND event_type = 'evidence_observed'`,
		workID).Scan(&events))
	assert.GreaterOrEqual(t, events, 2, "each evidence write should append an evidence_observed event")
}

func TestF12_ReopenEvidenceChangeRecomputeIdempotent(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	workID := seedOpenWork(t, dm, "redeploy service")

	addWorkEvidence(t, dm, workID, "api_response", 0.85)
	assert.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, workID))

	// Cancel → evidence changes keep derivation current. F8.1 invariant:
	// a cancelled work MUST NOT be verified regardless of evidence
	// pattern. Cancellation locks verification BELOW verified.
	_, err := dm.CancelWork(workID)
	require.NoError(t, err)
	assert.Equal(t, string(WorkVerificationUnverified), workVerification(t, dm, workID),
		"cancelled work with prior verified evidence must downgrade to unverified (F8.1)")

	// Reopen → evidence-based derivation applies because status is 'open' again.
	_, err = dm.ReopenWorkWithContext(workID, ActiveContext{})
	require.NoError(t, err)
	assert.Equal(t, string(WorkVerificationVerified), workVerification(t, dm, workID),
		"reopened work with outcome evidence must re-derive to verified")

	// Cancel again, then add action evidence. Action evidence on a
	// cancelled work cannot promote verification above unverified — the
	// lifecycle gate keeps it locked.
	_, err = dm.CancelWork(workID)
	require.NoError(t, err)
	addWorkEvidence(t, dm, workID, "tool_invocation", 0.5) // action-only
	assert.Equal(t, string(WorkVerificationUnverified), workVerification(t, dm, workID),
		"cancelled work must stay unverified under any non-contradictory evidence (F8.1)")

	// Contradiction still wins over everything (lifecycle consistency).
	addWorkEvidence(t, dm, workID, "external", -0.9)
	assert.Equal(t, string(WorkVerificationContradicted), workVerification(t, dm, workID))

	// Idempotent recompute: deriving twice changes nothing.
	first, err := dm.DeriveWorkVerification(workID)
	require.NoError(t, err)
	second, err := dm.DeriveWorkVerification(workID)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
	assert.Equal(t, WorkVerificationContradicted, first)
}
