// internal/core/tools/zero_hit_hint_test.go — W-010 regression.
//
// Pins the contract: when mpm_memory query returns zero results and the
// query has multiple whitespace-separated tokens, the response carries
// a `hint` field explaining the BM25 IDF underflow. When the query
// returns hits, or is single-token, no hint is emitted.
//
// We test the pure helper directly — no DatabaseManager required.
package tools

import (
	"strings"
	"testing"
)

// TestAttachZeroHitHint_MultiTokenZeroHits is the happy path of W-010:
// count=0 + multi-token → hint emitted.
func TestAttachZeroHitHint_MultiTokenZeroHits(t *testing.T) {
	resp := map[string]interface{}{}
	attachZeroHitHint(resp, "alpha beta gamma", 0)
	hint, ok := resp["hint"].(string)
	if !ok {
		t.Fatal("hint not present for zero-hit multi-token query")
	}
	if !strings.Contains(hint, "BM25") {
		t.Errorf("hint missing BM25 explanation; got %q", hint)
	}
}

// TestAttachZeroHitHint_SingleTokenZeroHits: with a single token the
// hint is suppressed — the audit's framing is "multi-token queries
// underflow", and a single token's no-hits case has no actionable
// advice (try a different single word, which the agent will do anyway).
func TestAttachZeroHitHint_SingleTokenZeroHits(t *testing.T) {
	resp := map[string]interface{}{}
	attachZeroHitHint(resp, "alpha", 0)
	if _, ok := resp["hint"]; ok {
		t.Errorf("hint emitted for single-token zero-hit query; want suppressed")
	}
}

// TestAttachZeroHitHint_NonZeroHits: when the query returns any hits,
// no hint is emitted — the agent already has results to work with.
func TestAttachZeroHitHint_NonZeroHits(t *testing.T) {
	for _, count := range []int{1, 3, 10} {
		resp := map[string]interface{}{}
		attachZeroHitHint(resp, "alpha beta gamma", count)
		if _, ok := resp["hint"]; ok {
			t.Errorf("hint emitted for count=%d; want suppressed", count)
		}
	}
}

// TestAttachZeroHitHint_WhitespaceEdgeCases: leading/trailing/multi
// whitespace is collapsed by strings.Fields, so the token-count logic
// is robust to realistic input shapes.
func TestAttachZeroHitHint_WhitespaceEdgeCases(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"alpha", false},                       // single token
		{"  alpha  ", false},                   // single token after trim
		{"alpha beta", true},                   // two tokens
		{"alpha\tbeta\tgamma", true},           // tab-separated
		{"alpha\nbeta", true},                  // newline-separated
		{"alpha   beta", true},                 // multi-space
		{"", false},                            // empty
		{"   ", false},                         // whitespace only
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			resp := map[string]interface{}{}
			attachZeroHitHint(resp, tc.query, 0)
			_, hasHint := resp["hint"]
			if hasHint != tc.want {
				t.Errorf("query=%q: hasHint=%v, want %v", tc.query, hasHint, tc.want)
			}
		})
	}
}