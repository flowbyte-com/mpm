package internal

import (
	"math"
	"testing"
)

// TestNormalizeBM25_HigherIsBetter pins the correction to the BM25 sign
// inversion.
//
// SQLite's bm25() is negative-is-better. The original implementation
// used `score / (1 + |score|)`, which is sign-PRESERVING: it mapped the
// strongest match (most negative) to the worst normalised value. The
// benchmark caught this concretely — for one query, bm25 -22.57 was
// ranked above bm25 -24.36.
func TestNormalizeBM25_HigherIsBetter(t *testing.T) {
	tests := []struct {
		name string
		bm25 float64
		want float64
	}{
		{"strongest match normalises near 1", -24.365, 0.9605},
		{"moderate match", -8.797, 0.8979},
		{"weak match", -0.300, 0.2308},
		{"near zero", 0, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeBM25(tc.bm25)
			if math.Abs(got-tc.want) > 0.001 {
				t.Errorf("normalizeBM25(%v) = %.4f, want %.4f",
					tc.bm25, got, tc.want)
			}
		})
	}
}

// TestNormalizeBM25_MonotonicInMatchQuality is the regression that
// actually failed. A better lexical match must never normalise to a
// worse value, for any input.
func TestNormalizeBM25_MonotonicInMatchQuality(t *testing.T) {
	// Ascending = progressively better matches (more negative bm25).
	scores := []float64{0.1, -0.5, -1.5, -4.0, -9.0, -14.649, -24.365, -30.0}

	prev := math.Inf(-1)
	for _, s := range scores {
		got := normalizeBM25(s)
		if got <= prev {
			t.Errorf("non-monotonic: normalizeBM25(%v)=%.4f is not greater "+
				"than the previous better-scoring row's %.4f", s, got, prev)
		}
		prev = got
	}
}

// TestNormalizeBM25_AlwaysNonNegative guards the property the blend
// depends on: every leg contributes a non-negative amount, so a lexical
// match can only ever ADD to the blend.
func TestNormalizeBM25_AlwaysNonNegative(t *testing.T) {
	// Negative bm25 is the only form SQLite returns in practice.
	for _, s := range []float64{-0.001, -0.5, -1, -5, -50, -1000} {
		if got := normalizeBM25(s); got < 0 {
			t.Errorf("normalizeBM25(%v) = %v, want >= 0", s, got)
		}
	}
}

// TestHybridScore_LexicalLegNotInverted is the end-to-end property: for
// two rows with identical vector similarity, the row with the stronger
// lexical match must score higher.
func TestHybridScore_LexicalLegNotInverted(t *testing.T) {
	const vecSim = 0.5
	const vw = 0.5

	stronger := hybridScore(-24.365, vecSim, vw) // better lexical match
	weaker := hybridScore(-8.797, vecSim, vw)    // worse lexical match

	if stronger <= weaker {
		t.Errorf("lexical leg inverted: stronger match scored %.4f, "+
			"weaker scored %.4f", stronger, weaker)
	}
}

// TestHybridScore_LegsShareOneScale verifies both legs are on a
// comparable 0-1 scale, which is what makes CombinedScore mean the same
// thing across sources and makes RetrievalThreshold coherent.
func TestHybridScore_LegsShareOneScale(t *testing.T) {
	// With VectorWeight 0.5, a perfect lexical + perfect vector match
	// should land at ~1.0, not overflow or go negative.
	got := hybridScore(-100, 1.0, 0.5)
	if got < 0.99 || got > 1.0 {
		t.Errorf("perfect-both-legs score = %.4f, want ~1.0", got)
	}

	// A total mismatch must land near 0, not negative. This is the
	// change in kind from the old behaviour, which returned -0.164 for
	// a strong match because the lexical leg dominated negatively.
	none := hybridScore(-0.001, 0.0, 0.5)
	if none < 0 || none > 0.01 {
		t.Errorf("no-match score = %.4f, want ~0.0", none)
	}
}

// TestHybridScore_AlwaysNonNegative is the invariant the threshold
// depends on: a hybrid result must never score below zero, or the
// -3.0 default could filter a genuine match.
func TestHybridScore_AlwaysNonNegative(t *testing.T) {
	for _, bm25 := range []float64{-0.001, -1, -5, -20, -100} {
		for _, sim := range []float64{0.0, 0.25, 0.5, 0.75, 1.0} {
			for _, vw := range []float64{0.0, 0.25, 0.5, 0.75, 1.0} {
				if got := hybridScore(bm25, sim, vw); got < 0 {
					t.Errorf("hybridScore(%v, %v, %v) = %v, want >= 0",
						bm25, sim, vw, got)
				}
			}
		}
	}
}
