// hybrid_search_tiebreak_test.go — regression tests for the shared-scope
// weight tie-break that the benchmark found unreachable.
//
// The comparator in applySharedPremium used to early-return whenever both
// rows were FTS5-only:
//
//	if iIsFTS && jIsFTS {
//	    return out[i].r.FTS5Score < out[j].r.FTS5Score
//	}
//	...
//	if out[i].r.Weight != out[j].r.Weight {   // never reached
//	    return out[i].r.Weight > out[j].r.Weight
//	}
//
// That made the weight tie-break dead code for exactly the local-vs-shared
// comparison it existed to serve, since the shared-scope premium produces
// many rows inside a single BM25 band.
//
// The fix keeps BM25 primary and falls through to the tie-breaks only
// when the BM25 scores are genuinely equal. These tests pin both halves:
// relevance still wins when it differs, and weight now breaks true ties.
package internal

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestApplySharedPremium_FTS5WeightTieBreakIsReachable is the direct
// regression: two FTS5-only rows with identical BM25 but different
// weights. Before the fix, order was whatever the input happened to be.
func TestApplySharedPremium_FTS5WeightTieBreakIsReachable(t *testing.T) {
	in := []HybridResult{
		{ID: "low", Origin: "local", Collection: "memories", Source: "fts5",
			FTS5Score: -5.0, CombinedScore: 0.5, Weight: 1},
		{ID: "high", Origin: "local", Collection: "memories", Source: "fts5",
			FTS5Score: -5.0, CombinedScore: 0.5, Weight: 9},
	}

	out := applySharedPremium(in, 10)
	require.Len(t, out, 2)
	require.Equal(t, "high", out[0].ID,
		"equal-BM25 FTS5 rows must be tie-broken by weight descending; "+
			"the early return made this unreachable")
	require.Equal(t, "low", out[1].ID)
}

// TestApplySharedPremium_FTS5RelevanceStillPrimary guards the other
// half of the fix: raising weight must NOT let a worse lexical match
// outrank a better one. Relevance stays primary.
func TestApplySharedPremium_FTS5RelevanceStillPrimary(t *testing.T) {
	in := []HybridResult{
		// Better lexical match (-9 is a stronger hit than -5), low weight.
		{ID: "strong", Origin: "local", Collection: "memories", Source: "fts5",
			FTS5Score: -9.0, CombinedScore: 0.9, Weight: 1},
		// Worse lexical match, but very high weight.
		{ID: "weak", Origin: "local", Collection: "memories", Source: "fts5",
			FTS5Score: -2.0, CombinedScore: 0.2, Weight: 100},
	}

	out := applySharedPremium(in, 10)
	require.Len(t, out, 2)
	require.Equal(t, "strong", out[0].ID,
		"a stronger BM25 match must outrank a weaker one regardless of weight")
	require.Equal(t, "weak", out[1].ID)
}

// TestApplySharedPremium_FTS5CollectionTieBreak confirms the
// collection tie-break is reachable too, when both BM25 and weight tie.
func TestApplySharedPremium_FTS5CollectionTieBreak(t *testing.T) {
	in := []HybridResult{
		{ID: "rules", Origin: "local", Collection: "rules", Source: "fts5",
			FTS5Score: -5.0, CombinedScore: 0.5, Weight: 3},
		{ID: "lessons", Origin: "local", Collection: "lessons", Source: "fts5",
			FTS5Score: -5.0, CombinedScore: 0.5, Weight: 3},
	}

	out := applySharedPremium(in, 10)
	require.Len(t, out, 2)
	require.Equal(t, "lessons", out[0].ID,
		"equal BM25 and weight must fall through to collection ascending")
	require.Equal(t, "rules", out[1].ID)
}

// TestApplySharedPremium_MixedSourcesUnaffected confirms the fix did not
// disturb the hybrid/vector path, which sorts on `final`.
func TestApplySharedPremium_MixedSourcesUnaffected(t *testing.T) {
	in := []HybridResult{
		{ID: "vec-low-score", Origin: "local", Collection: "memories", Source: "vector",
			CombinedScore: 0.2, Weight: 100},
		{ID: "hyb-high-score", Origin: "local", Collection: "memories", Source: "hybrid",
			CombinedScore: 0.8, Weight: 1},
	}

	out := applySharedPremium(in, 10)
	require.Len(t, out, 2)
	require.Equal(t, "hyb-high-score", out[0].ID,
		"relevance must remain primary for non-FTS5 rows")
}
