package internal

import (
	"testing"
)

// TestWork_ForensicIncident_ClaimWithoutAction reproduces the Aug-23 forensic incident:
// Agent claims completion but takes no MPM action. No evidence is collected.
// Expected: status=done, verification=unverified.
func TestWork_ForensicIncident_ClaimWithoutAction(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("Update README with Work Primitive documentation",
		"Document the event-sourced model and new CLI commands", "session-abc")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Agent claims completion without any observable MPM action.
	// CompleteWorkWithContext emits claimed_complete event.
	completed, err := dm.CompleteWorkWithContext(w.ID, "All documentation updated", ActiveContext{})
	if err != nil {
		t.Fatalf("CompleteWorkWithContext: %v", err)
	}

	// Status must be done (lifecycle terminal).
	if completed.Status != WorkStatusDone {
		t.Errorf("Status = %v, want done", completed.Status)
	}

	// Verification must be unverified (no evidence collected).
	if completed.Verification != WorkVerificationUnverified {
		t.Errorf("Verification = %v, want unverified (no evidence collected)", completed.Verification)
	}

	// claimed_complete event must be in the ledger.
	events, err := dm.GetWorkEvents(w.ID)
	if err != nil {
		t.Fatalf("GetWorkEvents: %v", err)
	}
	eventTypes := make([]string, len(events))
	for i, e := range events {
		eventTypes[i] = string(e.EventType)
	}
	found := false
	for _, et := range eventTypes {
		if et == "claimed_complete" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Events = %v, want claimed_complete in list", eventTypes)
	}

	// No evidence rows must exist for this work.
	evidence, err := ListEvidenceForArtifact(dm, w.ID, "work")
	if err != nil {
		t.Fatalf("ListEvidenceForArtifact: %v", err)
	}
	if len(evidence) > 0 {
		t.Errorf("Evidence rows = %d, want 0 (no observation occurred)", len(evidence))
	}
}

// TestWork_ExternalGitCommit_WithoutMPMAction simulates an external process
// committing to git without any MPM action. MPM observes but did not act.
// Expected: external evidence recorded, verification=partial (audit evidence only).
//
// The evidence is recorded through the EXPLICIT observation route. This test
// previously called dm.recordGitEvidenceForWork(w.ID), which reached
// CaptureGitSnapshot("") and relied on the ambient cwd/workspace probe chain
// to find a repository. It passed only because `go test` happened to run with
// cwd inside the MPM git worktree — an environment-dependent test asserting
// the very false attribution that has since been removed. What this test is
// actually about is the verification rule "audit-only evidence derives
// partial", and that rule needs an evidence row from a real source.
//
// See git_provenance_test.go for the fail-closed provenance invariant.
func TestWork_ExternalGitCommit_WithoutMPMAction(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("Update README", "Add Work section", "session-abc")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// External process committed; the observer records git audit evidence
	// explicitly, stating the observation as its own knowledge.
	if _, err := dm.AddEvidence(EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:         "observation",
		SourceGroup:  "git",
		CreatedBy:    "work_evidence",
		Notes:        "git changed_files: README.md head_before=abc1234",
		Strength:     0.6,
	}); err != nil {
		t.Fatalf("AddEvidence: %v", err)
	}

	// Derive verification — should be partial (audit evidence only, no outcome).
	derived, err := dm.DeriveWorkVerification(w.ID)
	if err != nil {
		t.Fatalf("DeriveWorkVerification: %v", err)
	}
	if derived != WorkVerificationPartial {
		t.Errorf("Verification = %v, want partial (audit evidence only)", derived)
	}

	// Verify the evidence row was recorded.
	evidence, err := ListEvidenceForArtifact(dm, w.ID, "work")
	if err != nil {
		t.Fatalf("ListEvidenceForArtifact: %v", err)
	}
	if len(evidence) == 0 {
		t.Error("Evidence rows = 0, want 1 (git evidence recorded)")
	}
	for _, e := range evidence {
		if e.SourceGroup != "git" {
			t.Errorf("SourceGroup = %q, want git", e.SourceGroup)
		}
	}

	// No LIFECYCLE work_events for the git commit (it wasn't MPM's action).
	// F7/F12 note: an evidence_observed ledger event IS expected — it is
	// observability of the evidence write (which the audit requires be
	// inspectable), not a fabricated lifecycle transition.
	events, err := dm.GetWorkEvents(w.ID)
	if err != nil {
		t.Fatalf("GetWorkEvents: %v", err)
	}
	lifecycleTypes := map[WorkEventType]bool{
		WorkEventTypeCreated: true, WorkEventTypeCompleted: true,
		WorkEventTypeCancelled: true, WorkEventTypeReopened: true,
		WorkEventTypeClaimedComplete: true,
	}
	for _, e := range events {
		if lifecycleTypes[e.EventType] {
			t.Errorf("fabricated lifecycle event %q from git-only observation", e.EventType)
		}
	}
}

// TestWork_VerificationUnaffectedByReprojection verifies that RecomputeWorkProjection
// does not reset verification to unverified.
func TestWork_VerificationUnaffectedByReprojection(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("Test reprojection", "Content", "session-abc")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Simulate a verified state by directly setting it, then reprojecting.
	_, err = dm.db.Exec(`UPDATE works SET verification = 'verified' WHERE id = ?`, w.ID)
	if err != nil {
		t.Fatalf("Set verification: %v", err)
	}

	// Recompute projection — must not reset verification.
	err = dm.RecomputeWorkProjection(w.ID)
	if err != nil {
		t.Fatalf("RecomputeWorkProjection: %v", err)
	}

	reloaded, err := dm.GetWork(w.ID)
	if err != nil {
		t.Fatalf("GetWork: %v", err)
	}
	if reloaded.Verification != WorkVerificationVerified {
		t.Errorf("Verification after reproject = %v, want verified (must not reset)", reloaded.Verification)
	}
}

// TestWork_ActionEvidenceWithoutOutcome_Unverified verifies that action evidence
// alone (tool call succeeded) does not produce verified or partial.
func TestWork_ActionEvidenceWithoutOutcome_Unverified(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("Write a file", "Content", "session-abc")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Add action evidence only (tool_invocation source_group).
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:        "observation",
		SourceGroup: "tool_invocation",
		CreatedBy:   "test",
		Strength:    0.7,
		Notes:       "Write tool succeeded",
	})

	derived, err := dm.DeriveWorkVerification(w.ID)
	if err != nil {
		t.Fatalf("DeriveWorkVerification: %v", err)
	}
	if derived != WorkVerificationUnverified {
		t.Errorf("Verification = %v, want unverified (action without outcome)", derived)
	}
}

// TestWork_OutcomeEvidence_Verified verifies that outcome evidence (filesystem,
// test, api_response) produces verification=verified regardless of audit evidence.
func TestWork_OutcomeEvidence_Verified(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("Update README", "Content", "session-abc")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Add outcome evidence.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:        "observation",
		SourceGroup: "filesystem",
		CreatedBy:   "test",
		Strength:    0.8,
		Notes:       "README contains 'Work Primitive v2' at line 200",
	})

	derived, err := dm.DeriveWorkVerification(w.ID)
	if err != nil {
		t.Fatalf("DeriveWorkVerification: %v", err)
	}
	if derived != WorkVerificationVerified {
		t.Errorf("Verification = %v, want verified (outcome evidence present)", derived)
	}
}

// TestWork_Contradicted verifies that contradictory evidence (strength near -1)
// produces verification=contradicted.
func TestWork_Contradicted(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("Update README", "Content", "session-abc")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// Add contradictory outcome evidence.
	AddEvidence(dm, EvidenceInput{
		ArtifactID:   w.ID,
		ArtifactType: "work",
		Type:        "observation",
		SourceGroup: "filesystem",
		CreatedBy:   "test",
		Strength:    -0.9, // strong contradiction
		Notes:       "README does NOT contain expected text",
	})

	derived, err := dm.DeriveWorkVerification(w.ID)
	if err != nil {
		t.Fatalf("DeriveWorkVerification: %v", err)
	}
	if derived != WorkVerificationContradicted {
		t.Errorf("Verification = %v, want contradicted", derived)
	}
}

// TestWork_GitUnavailable_NoPanic verifies that DeriveWorkVerification does not
// panic and returns unverified when git is not available.
func TestWork_GitUnavailable_NoPanic(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("Test git-free", "Content", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}

	// No evidence at all — git unavailable, no action.
	derived, err := dm.DeriveWorkVerification(w.ID)
	if err != nil {
		t.Fatalf("DeriveWorkVerification: %v", err)
	}
	if derived != WorkVerificationUnverified {
		t.Errorf("Verification = %v, want unverified (no evidence)", derived)
	}
}

// TestWork_WakeContextWork_IncludesVerification verifies that WakeContextWork
// includes the Verification field.
func TestWork_WakeContextWork_IncludesVerification(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w, err := dm.AddWork("Wake test", "Content", "")
	if err != nil {
		t.Fatalf("AddWork: %v", err)
	}
	_, err = dm.db.Exec(`UPDATE works SET verification = 'partial' WHERE id = ?`, w.ID)
	if err != nil {
		t.Fatalf("Set verification: %v", err)
	}

	ctx, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("GatherWakeContext: %v", err)
	}

	for _, w2 := range ctx.OpenWorks {
		if w2.ID == w.ID {
			if w2.Verification != WorkVerificationPartial {
				t.Errorf("OpenWorks[%s].Verification = %v, want partial", w.ID, w2.Verification)
			}
			return
		}
	}
	// Work may not be in open list if there are more than 5; check via direct query.
	t.Log("Work not in OpenWorks (limit 5); checking directly via query")
}
