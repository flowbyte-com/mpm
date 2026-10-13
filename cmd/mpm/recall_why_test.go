// recall_why_test.go — pins that `mpm recall --why` reports the signals
// that actually rank a row.
//
// It previously printed:
//
//	reinforcement=N (×2 = X)  weight=N (×1.5 = Y)  recency=Z
//
// attributed in a comment to internal/hybrid_search.go. No comparator in
// the recall path reads reinforcement_count or weight — the benchmark
// neutralised both (weight=1, reinforcement_count=0 across all rows) and
// measured 0/8 orderings changed, byte-identical. That arithmetic lived
// only in computeScore, a 0-1 display value, and described nothing the
// ranker did. These tests fail if the fabricated arithmetic returns.
package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// TestNormalizeBM25ForDisplay_MatchesCore keeps the duplicated display
// helper honest against the ranking helper it mirrors.
func TestNormalizeBM25ForDisplay_MatchesCore(t *testing.T) {
	for _, s := range []float64{-24.365, -14.649, -8.797, -4.0, -1.5, -0.3, 0} {
		// The core implementation: -score / (1 + |score|)
		want := -s / (1 + math.Abs(s))
		if got := normalizeBM25ForDisplay(s); math.Abs(got-want) > 1e-9 {
			t.Errorf("normalizeBM25ForDisplay(%v) = %v, want %v", s, got, want)
		}
	}
}

// TestPrintProvenance_LexicalReportsBM25 — the lexical path must report
// the real ranking signal: the BM25 score and the rank it was given.
func TestPrintProvenance_LexicalReportsBM25(t *testing.T) {
	var buf bytes.Buffer
	e := recallEntry{
		id:          "abc12345",
		content:     "some content mentioning ecryptfs",
		createdAt:   1700000000,
		bm25:        -8.797,
		hasBM25:     true,
		lexicalRank: 2,
		source:      "lexical",
	}
	printProvenance(&buf, e, e.content)
	out := buf.String()

	if !strings.Contains(out, "bm25=-8.797") {
		t.Errorf("expected the real bm25 score in --why output, got:\n%s", out)
	}
	if !strings.Contains(out, "rank 2") {
		t.Errorf("expected the preserved bm25 rank in --why output, got:\n%s", out)
	}
}

// TestPrintProvenance_NoFabricatedFormula is the direct regression for
// the reported defect: the reinforcement/weight arithmetic must not
// come back, because nothing in the recall path computes it.
func TestPrintProvenance_NoFabricatedFormula(t *testing.T) {
	var buf bytes.Buffer
	e := recallEntry{
		id:                 "abc12345",
		content:            "some content",
		createdAt:          1700000000,
		reinforcementCount: 7,
		weight:             9,
		hasState:           true,
		bm25:               -5.0,
		hasBM25:            true,
		lexicalRank:        1,
		source:             "lexical",
	}
	printProvenance(&buf, e, e.content)
	out := buf.String()

	// The fabricated arithmetic was "reinforcement=7 (×2 = 14.0)" and
	// "weight=9 (×1.5 = 13.5)". Check for the arithmetic specifically,
	// not the bare field names — showing the state is fine, showing it
	// as a score contribution is not.
	for _, forbidden := range []string{"×2", "×1.5", "(×2 =", "(×1.5 ="} {
		if strings.Contains(out, forbidden) {
			t.Errorf("--why must not present fabricated score arithmetic %q, got:\n%s",
				forbidden, out)
		}
	}
	// State may still be shown, but must be labelled as non-ranking.
	if strings.Contains(out, "reinforcement=7") || strings.Contains(out, "weight=9") {
		if !strings.Contains(out, "not used for ranking") {
			t.Errorf("reinforcement/weight shown without a 'not used for ranking' "+
				"label, which re-implies they affect ranking:\n%s", out)
		}
	}
}

// TestPrintProvenance_LikeFallbackIsHonest — the LIKE path has no BM25,
// so it must say so rather than inventing a lexical score.
func TestPrintProvenance_LikeFallbackIsHonest(t *testing.T) {
	var buf bytes.Buffer
	e := recallEntry{
		id:        "abc12345",
		content:   "substring matched content",
		createdAt: 1700000000,
		source:    "lexical",
		// hasBM25 deliberately false — the LIKE fallback's NULL placeholder.
	}
	printProvenance(&buf, e, e.content)
	out := buf.String()

	if !strings.Contains(out, "LIKE fallback") {
		t.Errorf("expected the LIKE fallback to be identified in --why, got:\n%s", out)
	}
	if strings.Contains(out, "bm25=") {
		t.Errorf("LIKE fallback must not report a bm25 score it never computed, got:\n%s", out)
	}
}

// TestPrintProvenance_SemanticReportsPerChannelSignals — the semantic
// path reports the per-channel scores of the blend.
func TestPrintProvenance_SemanticReportsPerChannelSignals(t *testing.T) {
	var buf bytes.Buffer
	e := recallEntry{
		id:          "abc12345",
		content:     "hybrid matched content",
		createdAt:   1700000000,
		source:      "hybrid",
		vecSim:      0.607,
		hasVecSim:   true,
		combined:    0.412,
		hasComb:     true,
		bm25:        -8.797,
		hasBM25:     true,
		lexicalRank: 1,
	}
	printProvenance(&buf, e, e.content)
	out := buf.String()

	if !strings.Contains(out, "hybrid") {
		t.Errorf("expected the hybrid rationale in --why output, got:\n%s", out)
	}
	// rationaleForSource formats the combined score to 2 decimal places.
	if !strings.Contains(out, "0.41") {
		t.Errorf("expected the combined blend score in --why output, got:\n%s", out)
	}
}

// TestRationaleForSource_DescribesActualChannel guards against the
// pre-existing mismatch where a vector-only row was labelled "hybrid".
func TestRationaleForSource_DescribesActualChannel(t *testing.T) {
	if got := rationaleForSource("vector", 0.3, 0.44); !strings.Contains(got, "vector-only") {
		t.Errorf("vector row mislabelled: %q", got)
	}
	if got := rationaleForSource("fts5", 0.4, 0); !strings.Contains(got, "fts5") {
		t.Errorf("fts5 row mislabelled: %q", got)
	}
	if got := rationaleForSource("hybrid", 0.5, 0.5); !strings.Contains(got, "hybrid") {
		t.Errorf("hybrid row mislabelled: %q", got)
	}
}
