// similarity_candidates_test.go — unit tests for the pure discovery function.
//
// FindSimilarityCandidates has no database, no embedding provider and no
// shared attachment, so everything here runs in microseconds against
// hand-built vectors. That is the point of the split: the classification logic
// can be tested exhaustively without arranging state, and "does discovery
// write?" is answered by construction rather than by an assertion.
//
// The database-touching half of the feature — that retrieval writes nothing
// and that the explicit path persists what discovery found — lives in
// hybrid_search_nomutate_test.go.

package internal

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// vec builds a unit vector from raw components, so a test can state the
// cosine it expects rather than computing it.
func vec(t *testing.T, comps ...float32) []float32 {
	t.Helper()
	n := 1.0
	for _, c := range comps {
		n += float64(c) * float64(c)
	}
	out := make([]float32, len(comps))
	for i, c := range comps {
		out[i] = c / float32(n)
	}
	return out
}

// ── threshold behaviour ─────────────────────────────────────────────

func TestFindSimilarityCandidates_BelowThresholdYieldsNothing(t *testing.T) {
	entries := []ScanEntry{
		{ID: "a", Content: "alpha", Embedding: vec(t, 1, 0, 0)},
		{ID: "b", Content: "beta", Embedding: vec(t, 0, 1, 0)}, // cosine 0
	}
	require.Empty(t, FindSimilarityCandidates(entries))
}

// The threshold is a boundary, and it must be inclusive: a pair sitting on
// 0.85 is worth a human's attention, a pair below it is not.
//
// Vectors (1,0) and (slope,1) have cosine slope/sqrt(1+slope^2), so
// slope = c/sqrt(1-c^2) produces a target cosine c. An epsilon either side
// keeps this off the knife edge, where floating-point rounding would decide
// the result rather than the code.
func TestFindSimilarityCandidates_ThresholdBoundary(t *testing.T) {
	const eps = 1e-4
	for _, tc := range []struct {
		name      string
		cosine    float64
		wantPairs int
	}{
		{"just above the threshold", ContradictionCandidateThreshold + eps, 1},
		{"exactly at the threshold", ContradictionCandidateThreshold, 1},
		{"just below the threshold", ContradictionCandidateThreshold - eps, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slope := float32(tc.cosine / math.Sqrt(1-tc.cosine*tc.cosine))
			require.Positive(t, slope)

			cands := FindSimilarityCandidates([]ScanEntry{
				{ID: "x", Content: "claim one", Embedding: vec(t, 1, 0)},
				{ID: "y", Content: "claim two", Embedding: vec(t, slope, 1)},
			})
			require.Len(t, cands, tc.wantPairs)
		})
	}
}

// ── structural-prefix discount ─────────────────────────────────────

// Two memories that both open with the same template marker share tokens the
// claims need not share. The cosine is inflated by that shared frame, so it
// is halved before thresholding.
func TestFindSimilarityCandidates_StructuralPrefixPairIsDiscounted(t *testing.T) {
	// Identical vectors: raw cosine 1.0. With both sides structural prefixes
	// the score halves to 0.5, below threshold, so no candidate is emitted.
	structural := []ScanEntry{
		{ID: "s1", Content: "CHOICE: use pnpm for the workspace", Embedding: vec(t, 1, 0)},
		{ID: "s2", Content: "CHOICE: use npm for the workspace", Embedding: vec(t, 1, 0)},
	}
	require.Empty(t, FindSimilarityCandidates(structural),
		"two template-structured memories must not pair on shared frame alone")

	// Same vectors, neither side structural: raw cosine 1.0, emitted.
	plain := []ScanEntry{
		{ID: "p1", Content: "the workspace uses pnpm", Embedding: vec(t, 1, 0)},
		{ID: "p2", Content: "the workspace uses npm", Embedding: vec(t, 1, 0)},
	}
	require.Len(t, FindSimilarityCandidates(plain), 1,
		"without the shared frame the same cosine must be surfaced")
}

// ── dimension and value guards ─────────────────────────────────────

// Truncation defect: a 768-dim and a 384-dim vector compared over their
// shared components can score exactly 1.0. Comparing only same-length pairs
// makes that unreachable.
func TestFindSimilarityCandidates_MismatchedDimensionsAreSkipped(t *testing.T) {
	entries := []ScanEntry{
		{ID: "long", Content: "768-dimensional", Embedding: make([]float32, 768)},
		{ID: "short", Content: "384-dimensional", Embedding: make([]float32, 384)},
	}
	for i := range entries[0].Embedding {
		entries[0].Embedding[i] = 1
	}
	for i := range entries[1].Embedding {
		entries[1].Embedding[i] = 1
	}
	require.Empty(t, FindSimilarityCandidates(entries),
		"vectors of different dimensionality must never be compared")
}

// A NaN makes every comparison false, so a `sim < threshold` guard alone
// would let the pair THROUGH rather than drop it.
func TestFindSimilarityCandidates_NonFiniteValuesAreSkipped(t *testing.T) {
	nan := float32(0)
	nan = nan / nan // NaN without importing math
	inf := float32(1)
	inf = inf * inf * inf * inf * inf // +Inf without importing math

	for _, bad := range []float32{nan, inf, -inf} {
		entries := []ScanEntry{
			{ID: "ok", Content: "well-formed", Embedding: vec(t, 1, 0)},
			{ID: "bad", Content: "malformed", Embedding: []float32{bad, 1}},
		}
		require.Empty(t, FindSimilarityCandidates(entries),
			"a non-finite component must drop the pair, not pass it")
	}
}

func TestFindSimilarityCandidates_EmptyAndNilEmbeddingsAreSkipped(t *testing.T) {
	entries := []ScanEntry{
		{ID: "a", Content: "no vector", Embedding: nil},
		{ID: "b", Content: "empty vector", Embedding: []float32{}},
		{ID: "c", Content: "real", Embedding: vec(t, 1, 0)},
		{ID: "d", Content: "also real", Embedding: vec(t, 1, 0)},
	}
	cands := FindSimilarityCandidates(entries)
	require.Len(t, cands, 1)
	require.ElementsMatch(t, []string{"c", "d"}, []string{cands[0].MemoryA, cands[0].MemoryB})
}

// ── classification ──────────────────────────────────────────────────

func TestFindSimilarityCandidates_ClassifiesAllThreeBranches(t *testing.T) {
	same := vec(t, 1, 0)
	cases := []struct {
		name     string
		metaA    string
		metaB    string
		chalA    bool
		chalB    bool
		wantKind CandidateKind
	}{
		{
			name:     "state_collision wins over tier difference",
			metaA:    `{"provenance":{"compute":"absolute"}}`,
			metaB:    `{"provenance":{"compute":"ephemeral"}}`,
			chalA:    true,
			wantKind: CandidateStateCollision,
		},
		{
			name:     "tier difference wins when neither is flagged",
			metaA:    `{"provenance":{"compute":"absolute"}}`,
			metaB:    `{"provenance":{"compute":"ephemeral"}}`,
			wantKind: CandidateProvenanceTier,
		},
		{
			name:     "equal tier, neither flagged",
			metaA:    `{"provenance":{"compute":"high"}}`,
			metaB:    `{"provenance":{"compute":"high"}}`,
			wantKind: CandidateEqualTier,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cands := FindSimilarityCandidates([]ScanEntry{
				{ID: "m-a", Content: "shared claim one", Embedding: same,
					MetadataJSON: tc.metaA, IsChallenged: tc.chalA},
				{ID: "m-b", Content: "shared claim two", Embedding: same,
					MetadataJSON: tc.metaB, IsChallenged: tc.chalB},
			})
			require.Len(t, cands, 1)
			require.Equal(t, tc.wantKind, cands[0].Kind)
		})
	}
}

// The state-collision branch must name the flagged memory FIRST, whatever
// the loop order was, so an operator sees the row that already carries state.
func TestFindSimilarityCandidates_StateCollisionNamesFlaggedMemoryFirst(t *testing.T) {
	// "z-flagged" sorts after "a-unflagged", so loop order and lexical order
	// disagree — the case a naive implementation gets wrong.
	cands := FindSimilarityCandidates([]ScanEntry{
		{ID: "a-unflagged", Content: "claim one", Embedding: vec(t, 1, 0)},
		{ID: "z-flagged", Content: "claim two", Embedding: vec(t, 1, 0),
			IsChallenged: true},
	})
	require.Len(t, cands, 1)
	require.Equal(t, "z-flagged", cands[0].MemoryA,
		"the flagged memory must occupy the first slot")
	require.Equal(t, "a-unflagged", cands[0].MemoryB)
}

func TestFindSimilarityCandidates_TierBranchNamesHigherTierFirst(t *testing.T) {
	cands := FindSimilarityCandidates([]ScanEntry{
		{ID: "a-ephemeral", Content: "low tier", Embedding: vec(t, 1, 0),
			MetadataJSON: `{"provenance":{"compute":"ephemeral"}}`},
		{ID: "z-absolute", Content: "high tier", Embedding: vec(t, 1, 0),
			MetadataJSON: `{"provenance":{"compute":"absolute"}}`},
	})
	require.Len(t, cands, 1)
	require.Equal(t, "z-absolute", cands[0].MemoryA,
		"the higher-tier memory must occupy the first slot")
}

func TestProvenanceTierFor_Ranking(t *testing.T) {
	require.Greater(t, provenanceTierFor("absolute"), provenanceTierFor("high"))
	require.Greater(t, provenanceTierFor("high"), provenanceTierFor("standard"))
	require.Greater(t, provenanceTierFor("standard"), provenanceTierFor("ephemeral"))
	require.Equal(t, provenanceTierFor("standard"), provenanceTierFor("nonsense"),
		"an unrecognised tier must default to standard, not rank highest")
	require.Equal(t, provenanceTierFor("standard"), provenanceTierFor(""))
}

// ── pair identity and determinism ───────────────────────────────────

// (A,B) and (B,A) must collapse to one queue key, so the same corpus scanned
// in reverse order produces the same candidate rather than a second one.
func TestFindSimilarityCandidates_PairIsOrderIndependent(t *testing.T) {
	forward := FindSimilarityCandidates([]ScanEntry{
		{ID: "b", Content: "second", Embedding: vec(t, 1, 0)},
		{ID: "a", Content: "first", Embedding: vec(t, 1, 0)},
	})
	reversed := FindSimilarityCandidates([]ScanEntry{
		{ID: "a", Content: "first", Embedding: vec(t, 1, 0)},
		{ID: "b", Content: "second", Embedding: vec(t, 1, 0)},
	})
	require.Len(t, forward, 1)
	require.Len(t, reversed, 1)
	require.Equal(t, forward[0].MemoryA, reversed[0].MemoryA)
	require.Equal(t, forward[0].MemoryB, reversed[0].MemoryB)
	require.InDelta(t, forward[0].Similarity, reversed[0].Similarity, 1e-12)
}

func TestFindSimilarityCandidates_IsDeterministic(t *testing.T) {
	entries := []ScanEntry{
		{ID: "d", Content: "fourth", Embedding: vec(t, 1, 0.05)},
		{ID: "a", Content: "first", Embedding: vec(t, 1, 0)},
		{ID: "c", Content: "third", Embedding: vec(t, 1, 0.03)},
		{ID: "b", Content: "second", Embedding: vec(t, 1, 0.01)},
	}
	first := FindSimilarityCandidates(entries)
	require.NotEmpty(t, first)
	for i := 0; i < 5; i++ {
		again := FindSimilarityCandidates(entries)
		require.Equal(t, first, again,
			"the same corpus must yield the same candidates, so a repeat scan is diffable")
	}
}

// ── language discipline ─────────────────────────────────────────────

// The single most important property of the evidence strings: they describe
// what was OBSERVED and never claim a contradiction was found. An operator
// reading the queue should not be told something the system cannot know.
func TestFindSimilarityCandidates_EvidenceNeverClaimsContradiction(t *testing.T) {
	entries := []ScanEntry{
		{ID: "b-flagged", Content: "same claim here", Embedding: vec(t, 1, 0),
			IsChallenged: true},
		{ID: "a-plain", Content: "same claim there", Embedding: vec(t, 1, 0)},
		{ID: "c-high", Content: "same claim again", Embedding: vec(t, 1, 0),
			MetadataJSON: `{"provenance":{"compute":"absolute"}}`},
		{ID: "d-low", Content: "and once more", Embedding: vec(t, 1, 0),
			MetadataJSON: `{"provenance":{"compute":"ephemeral"}}`},
	}
	cands := FindSimilarityCandidates(entries)
	require.NotEmpty(t, cands)
	for _, c := range cands {
		require.Contains(t, c.Evidence, "unverified similarity candidate")
		require.NotContains(t, strings.ToLower(c.Evidence), "contradict")
		require.NotContains(t, strings.ToLower(c.Evidence), "conflict")
	}
}

// A pair is only ever compared once, never with itself.
func TestFindSimilarityCandidates_NoSelfPairs(t *testing.T) {
	entries := []ScanEntry{
		{ID: "solo", Content: "alone", Embedding: vec(t, 1, 0)},
	}
	require.Empty(t, FindSimilarityCandidates(entries))
}

func TestFindSimilarityCandidates_NilInputIsSafe(t *testing.T) {
	require.Empty(t, FindSimilarityCandidates(nil))
	require.Empty(t, FindSimilarityCandidates([]ScanEntry{}))
}
