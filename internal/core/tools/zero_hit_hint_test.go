// internal/core/tools/zero_hit_hint_test.go — W-010 regression.
//
// Pins the contract: when mpm_memory query returns zero results and the
// query expands to more than one FTS5 term, the response carries a
// `hint` field explaining the implicit AND that caused the zero-hit. When
// the query returns hits, or yields a single FTS5 term, no hint is
// emitted.
//
// The W-010 tests below pin only WHEN a hint fires. What it SAYS was
// corrected later and is pinned by the W-014 tests at the foot of this
// file — the original text blamed "BM25 IDF underflow" and a threshold
// that never applied to lexical rows, and pointed agents at a knob
// that could not help.
//
// We test the pure helper directly — no DatabaseManager required.
package tools

import (
	"fmt"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
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
		{"alpha", false},             // single token
		{"  alpha  ", false},         // single token after trim
		{"alpha beta", true},         // two tokens
		{"alpha\tbeta\tgamma", true}, // tab-separated
		{"alpha\nbeta", true},        // newline-separated
		{"alpha   beta", true},       // multi-space
		{"", false},                  // empty
		{"   ", false},               // whitespace only
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

// ── W-014: the hint must describe the real mechanism ──────────────────
//
// The tests above pin WHEN a hint is emitted. These pin WHAT it says,
// which is where the original text was wrong in a way that sent agents
// to the wrong knob: it blamed "BM25 IDF underflow" and a "threshold
// underflow", neither of which exists in this retrieval path.
//
// A hint that is confidently wrong is worse than no hint: it ends the
// agent's investigation into the real cause and starts it down the wrong
// one instead.

// TestAttachZeroHitHint_DescribesImplicitANDNotThresholdUnderflow is the
// core correction.
//
// BM25 has no IDF underflow — IDF is a per-term weight, not a running
// total. And HybridSearch deliberately exempts lexical-only rows from
// RetrievalThreshold (BM25 is naturally negative, so a -3.0 floor would
// delete nearly every useful lexical hit), while the default -3.0
// cannot fire against the vector arm at all, whose combined score is
// cosine similarity in [0,1] scaled by a [0,1] weight. So "raise the
// threshold" was never advice that could help, and the hint asserted a
// mechanism the code does not contain.
func TestAttachZeroHitHint_DescribesImplicitANDNotThresholdUnderflow(t *testing.T) {
	resp := map[string]interface{}{}
	attachZeroHitHint(resp, "alpha beta gamma", 0)
	hint, _ := resp["hint"].(string)

	mustContain := []string{
		"implicit AND",  // the actual cause
		"match failure", // not a score filter
		"BM25",          // retained: ranked by BM25 on the lexical leg
	}
	for _, want := range mustContain {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q\n got: %s", want, hint)
		}
	}

	// The two claims that were false, asserted absent so a future edit
	// cannot reintroduce them.
	mustNotContain := []string{
		"underflow",      // no such mechanism in this path
		"IDF is",         // the old IDF-underflow framing
		"threshold even", // the old threshold-underflow framing
	}
	for _, unwanted := range mustNotContain {
		if strings.Contains(hint, unwanted) {
			t.Errorf("hint still contains the false claim %q\n got: %s", unwanted, hint)
		}
	}
}

// TestAttachZeroHitHint_CountsFTS5TermsNotWhitespaceTokens pins the
// trigger and the stated count against what FTS5 actually ANDs together.
//
// Whitespace tokens and FTS5 terms are different quantities. "lazy-start"
// is ONE whitespace token and TWO AND terms; "HTTP/1.1" is ONE whitespace
// token and THREE. Counting whitespace tokens therefore suppressed the
// hint on exactly the technical queries — the ones most likely to
// over-specify — and told the agent the wrong number to reduce from.
func TestAttachZeroHitHint_CountsFTS5TermsNotWhitespaceTokens(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantHint   bool
		wantCounts string
	}{
		{
			// Two AND terms behind one whitespace token: the old
			// whitespace-token count returned 1 and emitted no hint.
			name:  "hyphenated single whitespace token still hints",
			query: "lazy-start", wantHint: true,
		},
		{
			name: "slash splits into terms", query: "HTTP/1.1", wantHint: true,
		},
		{
			// Quoting does NOT create a phrase. BuildFTS5Query treats
			// double quotes as separators, so "lazy start" is tokenized
			// to lazy* start* and ANDed — the phrase a caller believes
			// they asked for was never expressed to FTS5. The hint must
			// say so rather than let the caller keep re-quoting.
			name: "quoted phrase is still an AND", query: `"lazy start"`, wantHint: true,
		},
		{
			// BuildFTS5Query returns "" for an all-separator query, so
			// there is nothing to reduce from and nothing to explain.
			name: "all separators yields no terms", query: `^"():*`, wantHint: false,
		},
		{
			// A bare apostrophe is dropped: unicode61 keeps apostrophes
			// only INSIDE words.
			name: "bare apostrophe yields no terms", query: "'", wantHint: false,
		},
		{
			name: "single term still suppressed", query: "configurator", wantHint: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := map[string]interface{}{}
			attachZeroHitHint(resp, tc.query, 0)
			hint, hasHint := resp["hint"].(string)
			if hasHint != tc.wantHint {
				t.Fatalf("query=%q: hasHint=%v, want %v", tc.query, hasHint, tc.wantHint)
			}
			if !tc.wantHint {
				return
			}
			// The stated term count must be the FTS5 term count, so an
			// agent reducing the query knows how many terms it has.
			terms := strings.Fields(mpminternal.BuildFTS5Query(tc.query))
			want := fmt.Sprintf("All %d FTS5 term(s)", len(terms))
			if !strings.Contains(hint, want) {
				t.Errorf("hint does not report the FTS5 term count %q\n got: %s", want, hint)
			}
		})
	}
}

// TestAttachZeroHitHint_NamesPrefixSemantics pins that the hint tells
// the caller terms are prefix-matched.
//
// `WordPress*` matches "wordpress", "wordpress_theme" and "wp"-
// independent variants, but NOT "Word" alone and NOT a substring. That
// asymmetry is why a "reasonable" paraphrase still returns zero, and it
// is invisible from the response.
func TestAttachZeroHitHint_NamesPrefixSemantics(t *testing.T) {
	resp := map[string]interface{}{}
	attachZeroHitHint(resp, "agentshell configurator", 0)
	hint, _ := resp["hint"].(string)
	for _, want := range []string{"PREFIX match", "substring"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q — the prefix-only match rule is the "+
				"other reason a plausible paraphrase returns zero\n got: %s", want, hint)
		}
	}
}
