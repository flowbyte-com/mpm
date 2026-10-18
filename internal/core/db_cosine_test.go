package internal

import (
	"math"
	"testing"
)

// makeEmbedding builds a deterministic unit-ish vector of the given
// dimension, so similarity expectations are exact rather than
// approximate.
func makeEmbedding(dim int, seed float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = seed + float32(i)
	}
	return v
}

func TestCosineSimilarity_DimensionMismatchDoesNotPanic(t *testing.T) {
	a := make([]float32, 384)
	b := make([]float32, 256)
	for i := range a {
		a[i] = 1
	}
	for i := range b {
		b[i] = 1
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("cosineSimilarity panicked on dimension mismatch: %v", r)
		}
	}()
	_ = cosineSimilarity(a, b) // must not panic
}

func TestCosineSimilarity_Empty(t *testing.T) {
	if got := cosineSimilarity(nil, []float32{1, 2}); got != 0 {
		t.Errorf("cosineSimilarity(nil, ...) = %f, want 0", got)
	}
	if got := cosineSimilarity([]float32{1, 2}, nil); got != 0 {
		t.Errorf("cosineSimilarity(..., nil) = %f, want 0", got)
	}
	if got := cosineSimilarity(nil, nil); got != 0 {
		t.Errorf("cosineSimilarity(nil, nil) = %f, want 0", got)
	}
}

// TestCosineSimilarity_MatchingDimensionsPreservesResults pins that the
// guard changed nothing for the normal case. Before this fix these exact
// inputs produced the same numbers by truncation; if a future edit
// "fixes" this by, say, zero-padding the shorter vector, this fails.
func TestCosineSimilarity_MatchingDimensionsPreservesResults(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float32
	}{
		{"identical", []float32{1, 2, 3}, []float32{1, 2, 3}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
		{"known 3-2-1", []float32{1, 2, 3}, []float32{4, 5, 6}, 0.9746318},
		{"scale invariant", []float32{1, 2, 3}, []float32{10, 20, 30}, 1},
		{"384 vs 384 identical", makeEmbedding(384, 1), makeEmbedding(384, 1), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cosineSimilarity(tc.a, tc.b)
			if math.Abs(float64(got-tc.want)) > 1e-6 {
				t.Errorf("cosineSimilarity = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCosineSimilarity_DimensionMismatchIsRejected is the regression
// this file exists for.
//
// A 768-dim and a 384-dim vector were previously compared over their
// first 384 components and returned a number in [-1,1]. The value is
// meaningless — it describes a partial dot product of two unrelated
// embedding spaces — but callers act on it. HybridSearch's contradiction
// scan proposes a pair for operator review when a candidate pair scores
// at or above 0.85, so a meaningless maximum would put a fabricated
// dispute in front of a human.
//
// The test uses two vectors that agree perfectly on their shared
// prefix, which is the worst case: truncation returned 1.0, the
// maximum, and would have been read as "these memories are near
// duplicates".
func TestCosineSimilarity_DimensionMismatchIsRejected(t *testing.T) {
	long := make([]float32, 768)
	for i := range long {
		long[i] = float32(i%7) + 1
	}
	short := make([]float32, 384)
	copy(short, long[:384]) // agrees perfectly on the shared prefix

	// Sanity: the prefix really is an identical vector, so a
	// truncating implementation would score this a perfect 1.0.
	if got := cosineSimilarity(short, short); math.Abs(float64(got)-1) > 1e-6 {
		t.Fatalf("precondition failed: identical 384 vectors scored %v, want 1", got)
	}

	for _, tc := range []struct {
		name string
		a, b []float32
	}{
		{"384 vs 768", short, long},
		{"768 vs 384", long, short},
		{"384 vs 385", short, append(append([]float32{}, short...), 1)},
		{"1 vs 2", []float32{1}, []float32{1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cosineSimilarity(tc.a, tc.b); got != 0 {
				t.Errorf("cosineSimilarity(%d-dim, %d-dim) = %v, want 0 — "+
					"mismatched dimensions are not comparable and must not be "+
					"scored by truncation",
					len(tc.a), len(tc.b), got)
			}
		})
	}
}

func TestCosineSimilarity_NonFiniteIsRejected(t *testing.T) {
	nan := float32(math.NaN())
	inf := float32(math.Inf(1))
	ninf := float32(math.Inf(-1))

	tests := []struct {
		name string
		a, b []float32
	}{
		{"NaN in a", []float32{nan, 1, 1}, []float32{1, 1, 1}},
		{"NaN in b", []float32{1, 1, 1}, []float32{nan, 1, 1}},
		{"+Inf in a", []float32{inf, 1, 1}, []float32{1, 1, 1}},
		{"-Inf in b", []float32{1, 1, 1}, []float32{ninf, 1, 1}},
		{"NaN only", []float32{nan}, []float32{nan}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cosineSimilarity(tc.a, tc.b)
			if math.IsNaN(float64(got)) {
				t.Errorf("cosineSimilarity with non-finite input returned NaN — "+
					"a NaN score compares false against every threshold, so it "+
					"would slip past a `sim < 0.85` guard")
			}
			if got != 0 {
				t.Errorf("cosineSimilarity with non-finite input = %v, want 0", got)
			}
		})
	}
}

// TestCosineSimilarity_ZeroNormStillReturnsZero pins that the all-zero
// vector keeps its pre-existing answer. An all-zero vector is a VALID
// pair (equal dims, finite) — it is cosineSimilarity, not
// validEmbeddingPair, that returns 0 for it, because the angle is
// undefined.
func TestCosineSimilarity_ZeroNormStillReturnsZero(t *testing.T) {
	zero := make([]float32, 8)
	if got := cosineSimilarity(zero, makeEmbedding(8, 1)); got != 0 {
		t.Errorf("cosineSimilarity(zero, v) = %v, want 0", got)
	}
	if got := cosineSimilarity(zero, zero); got != 0 {
		t.Errorf("cosineSimilarity(zero, zero) = %v, want 0", got)
	}
	if !validEmbeddingPair(zero, zero) {
		t.Error("an all-zero vector is a well-formed pair; only its angle is undefined")
	}
}

func TestValidEmbeddingPair(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want bool
	}{
		{"equal dims", []float32{1, 2}, []float32{3, 4}, true},
		{"mismatched dims", []float32{1, 2}, []float32{1, 2, 3}, false},
		{"empty a", nil, []float32{1}, false},
		{"empty b", []float32{1}, nil, false},
		{"both empty", nil, nil, false},
		{"NaN", []float32{float32(math.NaN())}, []float32{1}, false},
		{"Inf", []float32{1}, []float32{float32(math.Inf(1))}, false},
		{"negative values fine", []float32{-1, -2}, []float32{-3, -4}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validEmbeddingPair(tc.a, tc.b); got != tc.want {
				t.Errorf("validEmbeddingPair = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCosineSimilarityFloat32_MatchesCosineSimilarity pins that the two
// cosine implementations in this package agree. They were separate
// copies that both truncated, and had already drifted (one looped to
// min(len), the other to len(a) with a min() guard), so a caller could
// get different answers from each depending on which path it took.
func TestCosineSimilarityFloat32_MatchesCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
	}{
		{"identical", []float32{1, 2, 3}, []float32{1, 2, 3}},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}},
		{"scale invariant", []float32{1, 2, 3}, []float32{10, 20, 30}},
		{"mismatched dims", []float32{1, 2, 3}, []float32{1, 2}},
		{"empty", nil, []float32{1}},
		{"NaN", []float32{float32(math.NaN()), 1}, []float32{1, 1}},
		{"zero norm", make([]float32, 4), []float32{1, 2, 3, 4}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := cosineSimilarity(tc.a, tc.b)
			b := cosineSimilarityFloat32(tc.a, tc.b)
			if a != b {
				t.Errorf("cosineSimilarity=%v but cosineSimilarityFloat32=%v", a, b)
			}
		})
	}
}