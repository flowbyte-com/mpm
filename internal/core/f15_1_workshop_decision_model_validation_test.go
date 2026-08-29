// f15_1_workshop_decision_model_validation_test.go — F15-1 alpha-final
// regression.
//
// F15-1: the Skill Workshop decision-model contract specifies a 0-5
// scoring range per axis (reusability, non_obviousness, stability,
// leverage), but validateInput never enforced the range. A caller
// could send reusability=999 with the rest=0 and the runner would
// compute total=999 → "decision_total_high" → publish. The audit's
// constraint is "do not silently coerce invalid inputs" — this is
// the same class of bug as F12-1.
//
// The corrected contract: validateInput rejects out-of-range axis
// scores at the validation stage (before the decision-model stage
// even runs), with a clear error referencing the offending axis.
// The agent caller must supply scores in 0..5.
//
// Boundary is also a free-form string but the published set is
// restricted to {"procedure", "judgment", "knowledge"} — anything
// else is rejected by evaluateDecisionModel with a generic
// "non-procedure boundary" message. F15-1 also pins that the
// boundary must be one of the documented values, surfaced as a
// hard error rather than silently defaulting.
package internal

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validWorkshopRequest returns a minimal-form-mode request that
// passes all shape checks — only the decision_model / boundary
// fields are modified by the F15-1 tests.
func validWorkshopRequest() *WorkshopRequest {
	return &WorkshopRequest{
		Mode:       "form",
		Intent:     "test intent",
		ChangeType: "purpose_change",
		DecisionModel: DecisionModel{
			Reusability:    2,
			NonObviousness: 2,
			Stability:      2,
			Leverage:       2,
			Boundary:       "procedure",
		},
		Proposal: SkillProposal{
			Name:        "test-skill",
			Version:     "1.0.0",
			Domain:      "test",
			Description: "a test skill",
			WhenToUse:   "Use this skill when you need to test things",
			Steps:       []SkillStep{{Call: "do the thing"}},
		},
	}
}

// TestF15_1_OutOfRangeReusabilityIsRejected confirms that a
// decision_model axis score outside the documented 0-5 range is
// rejected at validation time, not silently published. Pre-fix:
// 999 → total=999 → published. Post-fix: rejected with a clear
// axis-naming error.
func TestF15_1_OutOfRangeReusabilityIsRejected(t *testing.T) {
	req := validWorkshopRequest()
	req.DecisionModel.Reusability = 999 // WAY out of range

	_, _, err := validateInput(req)
	require.Error(t, err,
		"reusability=999 must be rejected at validation; pre-fix this slipped through to published")
	errMsg := strings.ToLower(err.Error())
	assert.True(t,
		strings.Contains(errMsg, "reusability") || strings.Contains(errMsg, "axis") || strings.Contains(errMsg, "range"),
		"error must identify the offending axis (reusability/axis/range), got: %s", err.Error())
}

// TestF15_1_NegativeAxisScoreIsRejected is the symmetric check:
// a negative score (caller accidentally sent -1) must also be
// rejected. Pre-fix: total could be negative-low → rejected at
// the decision-model stage with a generic message, but this
// masks the structural bug.
func TestF15_1_NegativeAxisScoreIsRejected(t *testing.T) {
	req := validWorkshopRequest()
	req.DecisionModel.Stability = -3

	_, _, err := validateInput(req)
	require.Error(t, err,
		"negative stability score must be rejected at validation")
}

// TestF15_1_ValidBoundaryAccepted is the regression-safety check:
// a valid 0-5 score set + boundary="procedure" still passes
// validateInput cleanly.
func TestF15_1_ValidBoundaryAccepted(t *testing.T) {
	req := validWorkshopRequest()
	// Each axis is at the upper end of 0-5.
	req.DecisionModel.Reusability = 5
	req.DecisionModel.NonObviousness = 5
	req.DecisionModel.Stability = 5
	req.DecisionModel.Leverage = 5
	req.DecisionModel.Boundary = "procedure"

	warnings, _, err := validateInput(req)
	require.NoError(t, err, "valid 0-5 range + procedure boundary must pass validation")
	assert.Empty(t, warnings, "valid request should produce no warnings")
}

// TestF15_1_UnsupportedBoundaryRejected pins the boundary enum.
// The published set per spec is procedure | judgment | knowledge;
// anything else must be rejected. Pre-fix: only "procedure" was
// the publishable value, but evaluateDecisionModel rejected other
// values with a generic "non-procedure boundary" message that
// didn't tell the caller what the valid set was.
func TestF15_1_UnsupportedBoundaryRejected(t *testing.T) {
	req := validWorkshopRequest()
	req.DecisionModel.Boundary = "frobnicated" // not a documented boundary value

	_, _, err := validateInput(req)
	require.Error(t, err,
		"unsupported boundary value must be rejected at validation")
	errMsg := strings.ToLower(err.Error())
	assert.True(t,
		strings.Contains(errMsg, "boundary") || strings.Contains(errMsg, "procedure"),
		"error must name the boundary field, got: %s", err.Error())
}

// TestF15_1_RangeBoundaryAxesAccepted pins the inclusive range:
// 0 and 5 must both be accepted.
func TestF15_1_RangeBoundaryAxesAccepted(t *testing.T) {
	for _, axis := range []int{0, 5} {
		req := validWorkshopRequest()
		req.DecisionModel.Reusability = axis
		_, _, err := validateInput(req)
		require.NoError(t, err, "axis=%d (range endpoint) must be accepted", axis)
	}
}

// TestF15_1_JustOutsideRangeRejected confirms that values just
// outside the range (6 and -1) are rejected — the boundary
// endpoints are inclusive.
func TestF15_1_JustOutsideRangeRejected(t *testing.T) {
	for axis, name := range map[int]string{6: "reusability=6", -1: "stability=-1"} {
		req := validWorkshopRequest()
		if axis == 6 {
			req.DecisionModel.Reusability = axis
		} else {
			req.DecisionModel.Stability = axis
		}
		_, _, err := validateInput(req)
		require.Error(t, err, "%s must be rejected (just outside the 0..5 range)", name)
	}
}