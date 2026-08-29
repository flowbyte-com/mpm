// f15_2_workshop_error_propagation_test.go — F15-2 alpha-final regression.
//
// F15-2: F15-1 added hard input-validation errors to validateInput
// (mode mismatch, size cap, axis-range violations, unsupported
// boundary). RunWorkshop caught the error but SWALLOWED it —
// resp.Outcome was set to OutcomeRejected and resp.Reason was
// "input_validation_failed", but the function returned (resp, nil).
// From the registry / MCP / `mpm call` caller's perspective, a
// structurally-invalid request looked like a successful call with
// a "rejected" outcome, which contradicts the alpha audit's
// "do not silently coerce invalid inputs" rule.
//
// The corrected contract: RunWorkshop returns (resp, err) where
// err is the validateInput error. The response is still populated
// with Outcome=rejected so the UI layer can render the rejection
// reason, but err != nil ensures no caller can accidentally treat
// a malformed request as a successful outcome.
//
// This test pins the loud-fail contract at three layers:
//   1. validateInput (the F15-1 layer, already covered).
//   2. RunWorkshop (this fix).
//   3. Surface parity through handleWorkshopSkill — covered in
//      tools/f_alpha_surface_parity_test.go.
package internal

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunWorkshop_PropagatesValidateInputError is the core F15-2
// regression: out-of-range axis score must produce err != nil from
// RunWorkshop, not just Outcome=rejected inside a successful
// response.
//
// Pre-fix: err=nil, Outcome=OutcomeRejected.
// Post-fix: err contains "out of range", Outcome=OutcomeRejected,
// resp.Validation.Errors is populated for downstream display.
func TestRunWorkshop_PropagatesValidateInputError(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode: "form",
		// Reusability=999 is WAY outside the 0..5 range — F15-1
		// hard-rejects at validateInput. F15-2 ensures the
		// rejection propagates as err from RunWorkshop.
		DecisionModel: DecisionModel{
			Reusability:    999,
			NonObviousness: 2,
			Stability:      2,
			Leverage:       2,
			Boundary:       "procedure",
		},
		Proposal: SkillProposal{
			Name:        "f152-test",
			Version:     "1.0.0",
			Description: "test skill for F15-2 propagation",
			WhenToUse:   "Use this skill when testing F15-2 propagation",
			Steps:       []SkillStep{{Call: "do the thing"}},
		},
	}
	resp, err := RunWorkshop(dm, req)
	require.Error(t, err,
		"F15-2: out-of-range axis score must produce err != nil from RunWorkshop (silent-swallow regression)")
	assert.True(t,
		strings.Contains(err.Error(), "out of range") || strings.Contains(err.Error(), "reusability"),
		"F15-2: error must identify the offending axis, got: %v", err)
	assert.Equal(t, OutcomeRejected, resp.Outcome,
		"F15-2: response Outcome must still be rejected for downstream UI")
	assert.Equal(t, "input_validation_failed", resp.Reason,
		"F15-2: response Reason must be set so the UI can render the rejection")

	// No DB writes — a rejected request must not leak into the skills table.
	var count int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection='skills' AND id LIKE 'f152%'`,
	).Scan(&count))
	assert.Equal(t, 0, count, "F15-2: rejected workshop must not write to skills table")
}

// TestRunWorkshop_PropagatesUnsupportedBoundaryError confirms the
// boundary enum rejection also propagates as err (not just Outcome).
// Same silent-swallow class as the axis-range case.
func TestRunWorkshop_PropagatesUnsupportedBoundaryError(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{
			Reusability:    2,
			NonObviousness: 2,
			Stability:      2,
			Leverage:       2,
			Boundary:       "one_off", // not in {procedure, judgment, knowledge}
		},
		Proposal: SkillProposal{
			Name:        "f152-boundary",
			Version:     "1.0.0",
			Description: "test skill for boundary rejection",
			WhenToUse:   "Use this skill when testing boundary rejection",
			Steps:       []SkillStep{{Call: "do the thing"}},
		},
	}
	resp, err := RunWorkshop(dm, req)
	require.Error(t, err,
		"F15-2: unsupported boundary must produce err != nil (silent-swallow regression)")
	assert.True(t,
		strings.Contains(err.Error(), "boundary"),
		"F15-2: error must name the boundary field, got: %v", err)
	assert.Equal(t, OutcomeRejected, resp.Outcome)
}

// TestRunWorkshop_PropagatesSizeCapError confirms the size-cap
// rejection path also propagates as err.
func TestRunWorkshop_PropagatesSizeCapError(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode:        "form",
		TaskContext: strings.Repeat("x", 50*1024+1), // > 51200 byte cap
		DecisionModel: DecisionModel{
			Reusability:    2,
			NonObviousness: 2,
			Stability:      2,
			Leverage:       2,
			Boundary:       "procedure",
		},
		Proposal: SkillProposal{
			Name:        "f152-size",
			Version:     "1.0.0",
			Description: "test skill for size cap",
			WhenToUse:   "Use this skill when testing size cap rejection",
			Steps:       []SkillStep{{Call: "do the thing"}},
		},
	}
	resp, err := RunWorkshop(dm, req)
	require.Error(t, err,
		"F15-2: oversized input must produce err != nil (silent-swallow regression)")
	assert.True(t,
		strings.Contains(err.Error(), "task_context"),
		"F15-2: error must name the offending field, got: %v", err)
	assert.Equal(t, OutcomeRejected, resp.Outcome)
}

// TestRunWorkshop_SoftValidationStillReturnsNilError pins that the
// soft-validation downgrade path (secret scanner, missing proposal
// fields) is unchanged. F15-2 only hardens the err-propagation path;
// soft warnings continue to surface as Outcome=candidate with
// err=nil, preserving the existing contract for in-pipeline callers.
//
// This is the negative-control test that proves the fix didn't
// over-correct: secret scanner hit → err=nil, Outcome=candidate.
func TestRunWorkshop_SoftValidationStillReturnsNilError(t *testing.T) {
	dm := NewTestDM(t)
	req := &WorkshopRequest{
		Mode: "form",
		DecisionModel: DecisionModel{
			Reusability:    2,
			NonObviousness: 2,
			Stability:      2,
			Leverage:       2,
			Boundary:       "procedure",
		},
		Proposal: SkillProposal{
			Name:        "f152-soft",
			Version:     "1.0.0",
			Description: "AKIA1234567890123456", // secret scanner hit — soft fail
			WhenToUse:   "doing a thing, doing another, plus a third",
			Steps:       []SkillStep{{Call: "do the thing"}},
		},
	}
	resp, err := RunWorkshop(dm, req)
	// Soft failure: err=nil, Outcome=candidate (existing contract).
	assert.NoError(t, err, "F15-2: secret-scanner hit must remain a soft failure (err=nil)")
	assert.Equal(t, OutcomeCandidate, resp.Outcome,
		"F15-2: secret-scanner hit must downgrade to candidate, not reject")
}
