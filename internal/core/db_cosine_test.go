package internal

import "testing"

func TestCosineSimilarity_DimensionMismatchDoesNotPanic(t *testing.T) {
	a := make([]float32, 384)
	b := make([]float32, 256)
	for i := range a { a[i] = 1 }
	for i := range b { b[i] = 1 }

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
}
