package renderers_test

import (
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/orchestration"
	"github.com/flowbyte-com/mpm-core/renderers"
)

// goldenCase tests the renderer's output against an expected literal.
// Adding a new case is the natural extension when the layout
// evolves — renderers stay correct by golden tests.
type goldenCase struct {
	name     string
	results  []orchestration.ReviewResult
	wantSub  string // substring the output MUST contain
	wantMiss string // substring the output MUST NOT contain
}

func TestFormatReviewsMarkdown(t *testing.T) {
	cases := []goldenCase{
		{
			name:    "single success",
			results: successCase("memory", "default", "openai", "gpt-4o", 240, "Great work."),
			wantSub: "## Review from `memory`",
		},
		{
			name:    "single failure",
			results: failureCase("critic", "default", "openai", "gpt-4o", 12, "timeout exceeded"),
			wantSub: "## Review from `critic` — ❌ failed",
		},
		{
			name: "empty result returns placeholder",
			results: []orchestration.ReviewResult{{
				Component: "memory",
			}},
			wantSub:  "empty response",
			wantMiss: "no errors",
		},
		{
			name: "metadata always present even on failure",
			results: []orchestration.ReviewResult{
				{
					Component:  "reviewer",
					Profile:    "review",
					Provider:   "anthropic",
					Model:      "claude-3-5",
					DurationMS: 4200,
					Error:      "rate limit exceeded",
				},
			},
			wantSub: "**Duration:** 4200ms",
		},
		{
			name:     "header shows count",
			results:  threeSuccesses("a", "b", "c"),
			wantSub:  "Reviews (3 components)",
		},
		{
			name:    "header pluralizes correctly",
			results: successCase(
				"single", "default", "openai", "gpt-4o", 100, "ok",
			),
			wantSub: "Reviews (1 component)", // no plural
		},
		{
			name:     "section divider between results",
			results:  twoSuccesses("first", "second"),
			wantSub:  "---",
		},
		{
			name: "error message in fenced block",
			results: []orchestration.ReviewResult{{
				Component: "memory",
				Error:     "stack trace line 1\nstack trace line 2",
			}},
			wantSub: "```",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderers.FormatReviewsMarkdown(tc.results)
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("output missing %q\n---\n%s", tc.wantSub, got)
			}
			if tc.wantMiss != "" && strings.Contains(got, tc.wantMiss) {
				t.Errorf("output unexpectedly contained %q\n---\n%s", tc.wantMiss, got)
			}
		})
	}
}

// successCase returns a single-element slice of success ReviewResult.
// Slice (not single value) so multiple cases can be combined
// cleanly via the threeSuccesses / twoSuccesses helpers below.
func successCase(component, profile, provider, model string, durMS int, body string) []orchestration.ReviewResult {
	return []orchestration.ReviewResult{{
		Component:  component,
		Profile:    profile,
		Provider:   provider,
		Model:      model,
		DurationMS: durMS,
		Response:   body,
	}}
}

// failureCase returns a single-element slice of failed ReviewResult.
func failureCase(component, profile, provider, model string, durMS int, errMsg string) []orchestration.ReviewResult {
	return []orchestration.ReviewResult{{
		Component:  component,
		Profile:    profile,
		Provider:   provider,
		Model:      model,
		DurationMS: durMS,
		Error:      errMsg,
	}}
}

// threeSuccesses is the multi-component fixture used by cases
// that test the renderer with more than one result. Built by
// building a slice directly because Go's append doesn't accept a
// slice of T (without spread ...) as the variadic argument — and
// nesting append(append(...)) chains is brittle.
func threeSuccesses(componentA, componentB, componentC string) []orchestration.ReviewResult {
	return []orchestration.ReviewResult{
		{Component: componentA, Profile: "default", Provider: "test", Model: "m", DurationMS: 100, Response: "x"},
		{Component: componentB, Profile: "default", Provider: "test", Model: "m", DurationMS: 100, Response: "y"},
		{Component: componentC, Profile: "default", Provider: "test", Model: "m", DurationMS: 100, Response: "z"},
	}
}

// twoSuccesses is the 2-component variant.
func twoSuccesses(componentA, componentB string) []orchestration.ReviewResult {
	return []orchestration.ReviewResult{
		{Component: componentA, Profile: "default", Provider: "openai", Model: "gpt-4o", DurationMS: 100, Response: "A"},
		{Component: componentB, Profile: "default", Provider: "openai", Model: "gpt-4o", DurationMS: 100, Response: "B"},
	}
}
