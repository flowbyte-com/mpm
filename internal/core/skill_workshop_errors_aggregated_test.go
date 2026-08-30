// skill_workshop_errors_aggregated_test.go — alpha-4.1.2 D-010/W-008 regression test.
//
// Audit finding: the auditor flagged that `mpm skill workshop` validation
// surfaced only the FIRST error from a multi-error payload, leaving the
// author to fix-and-retry until the next error appeared. This is the
// same class of silent-fragmentation the alpha-4 W-005 fix addressed for
// the broader skills save path.
//
// Fix (alpha-4 W-005 + alpha-4.1.2 D-010): the workshop pipeline already
// aggregates validation errors into Validation.Errors []string (see
// skill_workshop.go:684). RunWorkshop returns the full WorkshopResponse
// with all errors collected. The handler prints the JSON response so
// every error reaches the operator.
//
// This test pins the post-fix contract: when a WorkshopRequest triggers
// multiple validation failures (both scanner_secret AND step shape), the
// response must surface ALL of them, not just the first.

package internal

import (
	"strings"
	"testing"
)

// TestRunWorkshop_AggregatesMultipleErrors pins that the workshop
// returns ALL validation errors in a single response, not the
// pre-W-005 behaviour of "first error only".
//
// We trigger two distinct error classes:
//   - scanner_secret: the Description contains an AWS-style access key.
//   - scanner_step:   the Steps list contains a malformed entry.
//
// The post-fix response must include BOTH errors so the author sees the
// full picture in a single round-trip.
func TestRunWorkshop_AggregatesMultipleErrors(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{
			Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2,
			Boundary: "procedure",
		},
		Proposal: SkillProposal{
			Name:        "scan-block",
			Version:     "1.0.0",
			WhenToUse:   "doing a thing, doing another, plus a third",
			Description: "AKIA1234567890123456 — leaks AWS key",
			Steps: []SkillStep{
				{Call: "good_call"},
				{Call: ""}, // empty call — D-004 violation
			},
		},
	}
	resp, err := RunWorkshop(dm, req)
	if err != nil {
		t.Fatalf("RunWorkshop: %v", err)
	}
	if resp.Outcome != OutcomeCandidate {
		t.Errorf("outcome = %v, want candidate (validation failure downgrades)", resp.Outcome)
	}
	if len(resp.Validation.Errors) < 2 {
		t.Fatalf("validation.errors = %v, want at least 2 entries (aggregation contract)",
			resp.Validation.Errors)
	}

	// Both error classes must be present.
	hasSecret := false
	hasStep := false
	for _, e := range resp.Validation.Errors {
		if strings.Contains(e, "scanner_secret") || strings.Contains(strings.ToLower(e), "sensitive") {
			hasSecret = true
		}
		if strings.Contains(e, "step[") {
			hasStep = true
		}
	}
	if !hasSecret {
		t.Errorf("expected scanner_secret error in aggregated list, got %v", resp.Validation.Errors)
	}
	if !hasStep {
		t.Errorf("expected step[N] error in aggregated list, got %v", resp.Validation.Errors)
	}
}

// TestRunWorkshop_OutcomeIsCandidateOnAnyError pins the contract that
// ANY validation error downgrades the outcome to candidate (not
// published), regardless of how many errors the aggregation collects.
func TestRunWorkshop_OutcomeIsCandidateOnAnyError(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{
			Reusability: 2, NonObviousness: 2, Stability: 2, Leverage: 2,
			Boundary: "procedure",
		},
		Proposal: SkillProposal{
			Name:        "any-error-candidate",
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
		t.Errorf("outcome = %v, want candidate", resp.Outcome)
	}
	if resp.Validation.Status != "failed" {
		t.Errorf("validation.status = %q, want failed", resp.Validation.Status)
	}
}