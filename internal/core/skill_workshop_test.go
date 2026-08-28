package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWorkshopClaimOrWait_FirstWriterWins(t *testing.T) {
	_ = NewTestDM(t)
	key := "test-key-" + fmt.Sprint(time.Now().UnixNano())

	// First claim — should be the writer.
	entry, isWriter, err := claimOrWait(context.Background(), key)
	if err != nil {
		t.Fatalf("first claimOrWait: %v", err)
	}
	if !isWriter {
		t.Fatal("first caller should be the writer")
	}

	// Publish the result and unblock any concurrent waiters.
	publishResult(entry, json.RawMessage(`{"outcome":"published"}`), nil)

	// Second claim with same key — entry.done is already closed,
	// so this should return immediately (isWriter=false).
	entry2, isWriter2, err := claimOrWait(context.Background(), key)
	if err != nil {
		t.Fatalf("second claimOrWait: %v", err)
	}
	if isWriter2 {
		t.Fatal("second caller should NOT be the writer")
	}
	if entry != entry2 {
		t.Fatal("second caller should observe the same entry as first")
	}

	// Verify the result was propagated.
	if string(entry2.response) != `{"outcome":"published"}` {
		t.Errorf("entry2.response = %q, want published", string(entry2.response))
	}
}

func TestValidateInput_ExceedsSizeLimits(t *testing.T) {
	huge := strings.Repeat("x", 50*1024+1) // task_context max is 50KB
	req := &WorkshopRequest{
		Mode: "form",
		Proposal: SkillProposal{Name: "foo", Version: "1.0.0"},
		TaskContext: huge,
	}
	_, _, err := validateInput(req)
	if err == nil {
		t.Fatal("expected error for oversized task_context, got nil")
	}
}

func TestValidateInput_TooManyMemoryIDs(t *testing.T) {
	ids := make([]string, 11)
	for i := range ids {
		ids[i] = fmt.Sprintf("mem-%d", i)
	}
	req := &WorkshopRequest{
		Mode: "form",
		Proposal: SkillProposal{Name: "foo", Version: "1.0.0"},
		Evidence: WorkshopEvidence{MemoryIDs: ids},
	}
	_, _, err := validateInput(req)
	if err == nil {
		t.Fatal("expected error for >10 memory_ids, got nil")
	}
}

func TestValidateInput_CleanRequest(t *testing.T) {
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2, Boundary: "procedure"},
		Proposal: SkillProposal{Name: "foo", Version: "1.0.0", WhenToUse: "doing a thing"},
		TaskContext: "small context",
	}
	_, _, err := validateInput(req)
	if err != nil {
		t.Fatalf("clean request: %v", err)
	}
}

func TestEvaluateDecisionModel_RejectsNonProcedure(t *testing.T) {
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2, Boundary: "fact"},
	}
	outcome, _, reason := evaluateDecisionModel(req)
	if outcome != DecisionRejected {
		t.Errorf("outcome = %v, want rejected", outcome)
	}
	if reason == "" {
		t.Errorf("reason empty")
	}
}

func TestEvaluateDecisionModel_RejectsLowScore(t *testing.T) {
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 1, NonObviousness: 1, Stability: 1, Leverage: 0, Boundary: "procedure"},
	}
	outcome, _, _ := evaluateDecisionModel(req)
	if outcome != DecisionRejected {
		t.Errorf("total=3 should reject, got %v", outcome)
	}
}

func TestEvaluateDecisionModel_CandidateMediumScore(t *testing.T) {
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 1, NonObviousness: 2, Stability: 1, Leverage: 1, Boundary: "procedure"},
	}
	outcome, _, _ := evaluateDecisionModel(req)
	if outcome != DecisionCandidate {
		t.Errorf("total=5 should candidate, got %v", outcome)
	}
}

func TestEvaluateDecisionModel_PublishedHighScore(t *testing.T) {
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2, Boundary: "procedure"},
	}
	outcome, _, _ := evaluateDecisionModel(req)
	if outcome != DecisionPublished {
		t.Errorf("total=8 procedure should publish, got %v", outcome)
	}
}

func TestEvaluateDecisionModel_RefineBumpsScores(t *testing.T) {
	// Spec §9 step 3: failure_recovery bumps reusability & non_obviousness.
	req := &WorkshopRequest{
		Mode:            "refine",
		FailureRecovery: "a recurring failure mode surfaced",
		DecisionModel:   DecisionModel{Reusability: 1, NonObviousness: 1, Stability: 2, Leverage: 2, Boundary: "procedure"},
	}
	outcome, dm, _ := evaluateDecisionModel(req)
	if outcome != DecisionPublished {
		t.Errorf("refine with bumped scores should publish, got %v", outcome)
	}
	if dm.Reusability < 2 {
		t.Errorf("refine did not bump reusability: %d", dm.Reusability)
	}
}

func TestCheckWhenToUse_Short(t *testing.T) {
	result := checkWhenToUse("foo", "form")
	if len(result.Warnings) == 0 {
		t.Error("expected warning for short when_to_use")
	}
}

func TestCheckWhenToUse_NoVerb(t *testing.T) {
	result := checkWhenToUse("a long phrase with nouns but no verb here at all", "form")
	found := false
	for _, w := range result.Warnings {
		if w == "when_to_use_no_verb" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected when_to_use_no_verb warning, got %v", result.Warnings)
	}
}

// TestCheckWhenToUse_EqualsName is replaced per brief bug note:
// The brief's TestCheckWhenToUse_EqualsName expects checkWhenToUse to
// produce a when_to_use_equals_name error, but checkWhenToUse has no
// name parameter. The orchestrator calls whenToUseEqualsName separately.
// This test verifies the actual helper function instead.
func TestWhenToUseEqualsName(t *testing.T) {
	if !whenToUseEqualsName("agentshell", "agentshell") {
		t.Error("expected whenToUseEqualsName to return true for equal strings")
	}
	if whenToUseEqualsName("agentshell", "different") {
		t.Error("expected whenToUseEqualsName to return false for different strings")
	}
	if whenToUseEqualsName("", "agentshell") {
		t.Error("expected whenToUseEqualsName to return false when proposed is empty")
	}
}

func TestCheckWhenToUse_Rich(t *testing.T) {
	// Comma-separated noun phrases, verb present, longer than 30 chars.
	rich := "reorganizing documentation, moving cross-references, updating READMEs"
	result := checkWhenToUse(rich, "form")
	if len(result.Warnings) != 0 || len(result.Errors) != 0 {
		t.Errorf("expected clean result, got warnings=%v errors=%v", result.Warnings, result.Errors)
	}
}

func TestDetectDuplicates_OverlapTriggersCandidate(t *testing.T) {
	dm := NewTestDM(t)
	// Seed an existing skill whose when_to_use overlaps heavily.
	existing := "---\nname: docs-cleanup\nversion: 1.0.0\nwhen_to_use: reorganizing documentation, moving cross-references, updating READMEs\n---\nbody"
	if _, err := dm.SaveSkill("docs-cleanup", "1.0.0", existing, "test", false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Propose a new skill with high overlap.
	proposal := SkillProposal{
		Name:      "docs-pass-2",
		Version:   "1.0.0",
		WhenToUse: "reorganizing documentation, moving cross-references, syncing READMEs and ARCHIVE",
	}
	result, err := detectDuplicates(dm, proposal, "")
	if err != nil {
		t.Fatalf("detectDuplicates: %v", err)
	}
	if len(result.CloseMatches) == 0 {
		t.Fatal("expected close match, got none")
	}
	if result.CloseMatches[0].OverlapScore < 0.6 {
		t.Errorf("overlap = %f, want >= 0.6", result.CloseMatches[0].OverlapScore)
	}
}

func TestDetectDuplicates_RefineExcludesSelf(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := dm.SaveSkill("docs-cleanup", "1.0.0",
		"---\nname: docs-cleanup\nversion: 1.0.0\nwhen_to_use: reorganizing documentation, moving cross-references\n---\nbody",
		"test", false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Refining docs-cleanup — must NOT flag itself.
	proposal := SkillProposal{
		Name:      "docs-cleanup",
		Version:   "1.1.0",
		WhenToUse: "reorganizing documentation, moving cross-references, plus new task",
	}
	result, err := detectDuplicates(dm, proposal, "docs-cleanup")
	if err != nil {
		t.Fatalf("detectDuplicates: %v", err)
	}
	for _, m := range result.CloseMatches {
		if m.Name == "docs-cleanup" {
			t.Errorf("refine must exclude self, but got match: %+v", m)
		}
	}
}

func TestIdentityCheck_NoExisting(t *testing.T) {
	dm := NewTestDM(t)
	proposal := SkillProposal{Name: "foo", Version: "1.0.0"}
	content := "---\nname: foo\nversion: 1.0.0\n---\nbody"
	outcome, existing, err := identityCheck(dm, proposal, content)
	if err != nil {
		t.Fatalf("identityCheck: %v", err)
	}
	if outcome != IdentityNone {
		t.Errorf("outcome = %v, want none", outcome)
	}
	if existing != nil {
		t.Errorf("existing should be nil for new skill")
	}
}

func TestIdentityCheck_SameContent(t *testing.T) {
	dm := NewTestDM(t)
	content := "---\nname: foo\nversion: 1.0.0\nwhen_to_use: doing, another\n---\nbody"
	if _, err := dm.SaveSkill("foo", "1.0.0", content, "test", false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	proposal := SkillProposal{Name: "foo", Version: "1.0.0"}
	outcome, existing, err := identityCheck(dm, proposal, content)
	if err != nil {
		t.Fatalf("identityCheck: %v", err)
	}
	if outcome != IdentitySame {
		t.Errorf("outcome = %v, want same", outcome)
	}
	if existing == nil || existing.ID != "skill:foo-v1.0.0" {
		t.Errorf("existing = %v, want skill:foo-v1.0.0", existing)
	}
}

func TestIdentityCheck_DifferentContent(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := dm.SaveSkill("foo", "1.0.0",
		"---\nname: foo\nversion: 1.0.0\n---\nbody v1", "test", false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	proposal := SkillProposal{Name: "foo", Version: "1.0.0"}
	contentV2 := "---\nname: foo\nversion: 1.0.0\n---\nbody v2 — different"
	outcome, _, err := identityCheck(dm, proposal, contentV2)
	if err != nil {
		t.Fatalf("identityCheck: %v", err)
	}
	if outcome != IdentityDifferent {
		t.Errorf("outcome = %v, want different", outcome)
	}
}

func TestBumpVersion_CorrectionPatch(t *testing.T) {
	got, err := bumpVersion("1.0.0", "correction")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "1.0.1" {
		t.Errorf("got %q, want 1.0.1", got)
	}
}

func TestBumpVersion_ExtensionMinor(t *testing.T) {
	got, err := bumpVersion("1.0.0", "extension")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "1.1.0" {
		t.Errorf("got %q, want 1.1.0", got)
	}
}

func TestBumpVersion_RestructuringMinor(t *testing.T) {
	got, err := bumpVersion("1.0.0", "restructuring")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "1.1.0" {
		t.Errorf("got %q, want 1.1.0", got)
	}
}

func TestBumpVersion_PurposeChangeMajor(t *testing.T) {
	got, err := bumpVersion("1.0.0", "purpose_change")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "2.0.0" {
		t.Errorf("got %q, want 2.0.0", got)
	}
}

func TestCheckVersionBump_MismatchReturnsError(t *testing.T) {
	err := checkVersionBump("2.0.0", "1.0.0", "correction")
	if err == nil {
		t.Fatal("expected version_bump_mismatch error")
	}
}

func TestCheckVersionBump_MatchPasses(t *testing.T) {
	if err := checkVersionBump("1.0.1", "1.0.0", "correction"); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestRunWorkshop_PublishedWorthyWorkflow(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2, Boundary: "procedure"},
		Proposal: SkillProposal{
			Name:        "new-skill",
			Version:     "1.0.0",
			Domain:      "test",
			Description: "a brand new skill",
			WhenToUse:   "doing a thing, doing another thing, plus a third",
			Steps:       []SkillStep{{Call: "step1"}, {Call: "step2"}},
		},
	}
	resp, err := RunWorkshop(dm, req)
	if err != nil {
		t.Fatalf("RunWorkshop: %v", err)
	}
	if resp.Outcome != OutcomePublished {
		t.Errorf("outcome = %v, want published", resp.Outcome)
	}
	if resp.SkillID != "skill:new-skill-v1.0.0" {
		t.Errorf("skill_id = %q, want skill:new-skill-v1.0.0", resp.SkillID)
	}
	// Read-back assertion: row exists in DB.
	if _, err := dm.ReadSkill("skill:new-skill-v1.0.0", ""); err != nil {
		t.Errorf("read-back failed: %v", err)
	}
}

func TestRunWorkshop_RejectsTrivialAction(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 0, NonObviousness: 0, Stability: 0, Leverage: 0, Boundary: "one_off"},
	}
	resp, err := RunWorkshop(dm, req)
	if err != nil {
		t.Fatalf("RunWorkshop: %v", err)
	}
	if resp.Outcome != OutcomeRejected {
		t.Errorf("outcome = %v, want rejected", resp.Outcome)
	}
	// No DB writes.
	var count int
	dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE collection='skills'`).Scan(&count)
	if count != 0 {
		t.Errorf("rejected workshop wrote to DB: count=%d", count)
	}
}

func TestRunWorkshop_CandidateSoftWarning(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 1, NonObviousness: 1, Stability: 1, Leverage: 1, Boundary: "procedure"},
		Proposal:      SkillProposal{Name: "soft-skill", Version: "1.0.0", WhenToUse: "doing a thing"},
	}
	resp, err := RunWorkshop(dm, req)
	if err != nil {
		t.Fatalf("RunWorkshop: %v", err)
	}
	if resp.Outcome != OutcomeCandidate {
		t.Errorf("outcome = %v, want candidate", resp.Outcome)
	}
	if resp.SavePayload == nil {
		t.Error("candidate must include save_payload")
	}
}

func TestRunWorkshop_DuplicateDetection(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := dm.SaveSkill("existing", "1.0.0",
		"---\nname: existing\nversion: 1.0.0\nwhen_to_use: reorganizing documentation, moving cross-references\n---\nbody",
		"test", false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2, Boundary: "procedure"},
		Proposal: SkillProposal{
			Name:      "dup-skill",
			Version:   "1.0.0",
			WhenToUse: "reorganizing documentation, moving cross-references, with extra stuff",
		},
	}
	resp, err := RunWorkshop(dm, req)
	if err != nil {
		t.Fatalf("RunWorkshop: %v", err)
	}
	if resp.Outcome != OutcomeCandidate {
		t.Errorf("outcome = %v, want candidate", resp.Outcome)
	}
	if len(resp.DuplicateCheck.CloseMatches) == 0 {
		t.Error("expected duplicate close_matches")
	}
}

func TestRunWorkshop_ValidationFailure(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2, Boundary: "procedure"},
		Proposal: SkillProposal{
			Name:        "scan-block",
			Version:     "1.0.0",
			WhenToUse:   "doing a thing, doing another, plus a third",
			Description: "AKIA1234567890123456",
		},
	}
	resp, err := RunWorkshop(dm, req)
	if err != nil {
		t.Fatalf("RunWorkshop: %v", err)
	}
	if resp.Outcome != OutcomeCandidate {
		t.Errorf("outcome = %v, want candidate (validation failure downgrades)", resp.Outcome)
	}
	if len(resp.Validation.Errors) == 0 {
		t.Error("expected validation.errors populated")
	}
}

func TestRunWorkshop_RefineExistingSkill(t *testing.T) {
	dm := NewTestDM(t)
	priorID, err := dm.SaveSkill("foo", "1.0.0",
		"---\nname: foo\nversion: 1.0.0\nwhen_to_use: doing, another, plus a third\n---\nv1",
		"test", false)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := &WorkshopRequest{
		Mode:            "refine",
		Intent:          "foo",
		ChangeType:      "extension",
		FailureRecovery: "recurring failure mode",
		DecisionModel:   DecisionModel{Reusability: 1, NonObviousness: 1, Stability: 2, Leverage: 2, Boundary: "procedure"},
		Proposal: SkillProposal{
			Name:        "foo",
			Version:     "1.1.0",
			WhenToUse:   "doing, another, plus a third, plus new task",
			Description: "foo extended",
		},
	}
	resp, err := RunWorkshop(dm, req)
	if err != nil {
		t.Fatalf("RunWorkshop: %v", err)
	}
	if resp.Outcome != OutcomePublished {
		t.Errorf("outcome = %v, want published", resp.Outcome)
	}
	// Prior version must be deprecated.
	prior, err := dm.ReadSkill(priorID, "")
	if err != nil {
		t.Fatalf("read prior: %v", err)
	}
	dep, _ := prior.Metadata["deprecated"].(bool)
	if !dep {
		t.Error("prior.Metadata.deprecated should be true")
	}
}

func TestRunWorkshop_Idempotence(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode:        "form",
		WorkshopKey: "stable-key-123",
		DecisionModel: DecisionModel{Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2, Boundary: "procedure"},
		Proposal:     SkillProposal{Name: "idem-skill", Version: "1.0.0", WhenToUse: "doing, another, plus a third", Description: "idempotent"},
	}
	resp1, _ := RunWorkshop(dm, req)
	if resp1.Outcome != OutcomePublished {
		t.Fatalf("first call: outcome=%v", resp1.Outcome)
	}
	resp2, _ := RunWorkshop(dm, req)
	if resp2.Outcome != OutcomePublished {
		t.Errorf("second call: outcome=%v (cache should dedupe)", resp2.Outcome)
	}
	if resp1.SkillID != resp2.SkillID {
		t.Errorf("cache must return same skill_id: %q vs %q", resp1.SkillID, resp2.SkillID)
	}
}

func TestRunWorkshop_IdentityCheckSameContent(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2, Boundary: "procedure"},
		Proposal:     SkillProposal{Name: "resilient-skill", Version: "1.0.0", WhenToUse: "doing, another, plus a third", Description: "x"},
	}
	resp1, _ := RunWorkshop(dm, req)
	if resp1.Outcome != OutcomePublished {
		t.Fatalf("first: %v", resp1.Outcome)
	}
	// Simulate cache loss: fresh workshop key.
	req.WorkshopKey = "different-key-after-restart"
	resp2, err := RunWorkshop(dm, req)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if resp2.Outcome != OutcomePublished {
		t.Errorf("post-restart same content: outcome=%v", resp2.Outcome)
	}
	if resp2.SkillID != resp1.SkillID {
		t.Errorf("identity check must return existing skill_id: %q vs %q", resp1.SkillID, resp2.SkillID)
	}
	// Only one row exists.
	var count int
	dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, resp1.SkillID).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 row for %s, got %d", resp1.SkillID, count)
	}
}

func TestRunWorkshop_IdentityCheckDifferentContent(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2, Boundary: "procedure"},
		Proposal:     SkillProposal{Name: "v-collide", Version: "1.0.0", WhenToUse: "doing, another, plus a third", Description: "first"},
	}
	resp1, _ := RunWorkshop(dm, req)
	if resp1.Outcome != OutcomePublished {
		t.Fatalf("first: %v", resp1.Outcome)
	}
	// Same (name, version) but different content.
	req.Proposal.Description = "second-different-content"
	resp2, err := RunWorkshop(dm, req)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if resp2.Outcome != OutcomeCandidate {
		t.Errorf("version_collision should return candidate, got %v", resp2.Outcome)
	}
	// No second row written.
	var count int
	dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE id LIKE 'skill:v-collide%'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 row for v-collide, got %d", count)
	}
}

func TestRunWorkshop_ValidateSkillNonMutating(t *testing.T) {
	dm := NewTestDM(t)
	// Count rows before.
	var before int
	dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE collection='skills'`).Scan(&before)
	skill := &Skill{
		Name:    "pure-validate",
		Version: "1.0.0",
		Frontmatter: SkillFrontmatter{
			Name:        "pure-validate",
			Version:     "1.0.0",
			WhenToUse:   "doing, another, plus a third",
			Description: "test",
		},
	}
	_, errs, err := dm.ValidateSkill(skill)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("unexpected errs: %v", errs)
	}
	var after int
	dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE collection='skills'`).Scan(&after)
	if after != before {
		t.Errorf("ValidateSkill wrote %d rows", after-before)
	}
}

func TestRunWorkshop_BoundedContext(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode:        "form",
		TaskContext: strings.Repeat("x", 50*1024+1), // exceeds limit
		DecisionModel: DecisionModel{Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2, Boundary: "procedure"},
		Proposal:     SkillProposal{Name: "bounded", Version: "1.0.0"},
	}
	resp, err := RunWorkshop(dm, req)
	if err != nil {
		t.Fatalf("RunWorkshop: %v", err)
	}
	if resp.Outcome != OutcomeRejected {
		t.Errorf("oversized input must reject, got %v", resp.Outcome)
	}
	if resp.Reason != "input_validation_failed" {
		t.Errorf("reason = %q, want input_validation_failed", resp.Reason)
	}
}

func TestRunWorkshop_RefineChangeTypeVersionBump(t *testing.T) {
	dm := NewTestDM(t)
	// Seed v1.0.0.
	if _, err := dm.SaveSkill("bumpy", "1.0.0",
		"---\nname: bumpy\nversion: 1.0.0\nwhen_to_use: doing, another, plus a third\n---\nv1",
		"test", false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Try a refine with change_type=correction but proposal.version=2.0.0.
	req := &WorkshopRequest{
		Mode:            "refine",
		Intent:          "bumpy",
		ChangeType:      "correction",
		FailureRecovery: "minor fix needed",
		DecisionModel:   DecisionModel{Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2, Boundary: "procedure"},
		Proposal: SkillProposal{
			Name:        "bumpy",
			Version:     "2.0.0", // wrong — correction should yield 1.0.1
			WhenToUse:   "doing, another, plus a third, fixed",
			Description: "bumpy v2",
		},
	}
	resp, err := RunWorkshop(dm, req)
	if err != nil {
		t.Fatalf("RunWorkshop: %v", err)
	}
	if resp.Outcome != OutcomeCandidate {
		t.Errorf("version_bump_mismatch should downgrade to candidate, got %v", resp.Outcome)
	}
	found := false
	for _, e := range resp.Validation.Errors {
		if e == "version_bump_mismatch" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected version_bump_mismatch error, got %v", resp.Validation.Errors)
	}
}

func TestRunWorkshop_PriorVersionReadability(t *testing.T) {
	dm := NewTestDM(t)
	priorID, _ := dm.SaveSkill("audit", "1.0.0",
		"---\nname: audit\nversion: 1.0.0\nwhen_to_use: doing, another, plus a third\n---\nv1 body",
		"test", false)
	req := &WorkshopRequest{
		Mode:       "refine",
		Intent:     "audit",
		ChangeType: "extension",
		DecisionModel: DecisionModel{Reusability: 1, NonObviousness: 1, Stability: 2, Leverage: 2, Boundary: "procedure"},
		Proposal: SkillProposal{
			Name:        "audit",
			Version:     "1.1.0",
			WhenToUse:   "doing, another, plus a third, plus extension",
			Description: "audit v1.1",
		},
	}
	resp, err := RunWorkshop(dm, req)
	if err != nil {
		t.Fatalf("RunWorkshop: %v", err)
	}
	if resp.Outcome != OutcomePublished {
		t.Errorf("outcome = %v, want published", resp.Outcome)
	}
	// Prior version must still be readable.
	prior, err := dm.ReadSkill(priorID, "")
	if err != nil {
		t.Fatalf("read prior: %v", err)
	}
	if prior.Version != "1.0.0" {
		t.Errorf("prior.Version = %q, want 1.0.0", prior.Version)
	}
}

