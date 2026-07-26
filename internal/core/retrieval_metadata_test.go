// retrieval_metadata_test.go — Observability Layer contract pins.
//
// These tests verify that RecordRetrieval/RecordRetrievalSuccess/
// GetRetrievalMetadata behave correctly: UPSERT semantics on
// duplicate node_id, monotonic reuse_count, no-op on unknown node.
// They do NOT test search ranking (left untouched per the user's
// Phase 0 constraint: "Do NOT change the current search ranking").

package internal

import (
	"testing"
)

func TestRecordRetrieval_InsertAndIncrement(t *testing.T) {
	dm := NewTestDM(t)

	// First retrieval: row created with reuse_count=1.
	if err := dm.RecordRetrieval("mem-abc", "memory"); err != nil {
		t.Fatalf("first RecordRetrieval: %v", err)
	}
	meta, err := dm.GetRetrievalMetadata("mem-abc")
	if err != nil {
		t.Fatalf("GetRetrievalMetadata: %v", err)
	}
	if meta.ReuseCount != 1 {
		t.Errorf("first retrieval ReuseCount = %d, want 1", meta.ReuseCount)
	}
	if meta.LastRetrievedAt == "" {
		t.Error("first retrieval LastRetrievedAt is empty, want populated")
	}
	if meta.NodeType != "memory" {
		t.Errorf("NodeType = %q, want memory", meta.NodeType)
	}

	// Second retrieval: reuse_count incremented to 2.
	if err := dm.RecordRetrieval("mem-abc", "memory"); err != nil {
		t.Fatalf("second RecordRetrieval: %v", err)
	}
	meta, _ = dm.GetRetrievalMetadata("mem-abc")
	if meta.ReuseCount != 2 {
		t.Errorf("second retrieval ReuseCount = %d, want 2", meta.ReuseCount)
	}
}

func TestRecordRetrievalSuccess_IndependentCounter(t *testing.T) {
	dm := NewTestDM(t)

	// Three retrievals but only one success: reuse_count=3,
	// success_count=1.
	_ = dm.RecordRetrieval("mem-xyz", "memory")
	_ = dm.RecordRetrieval("mem-xyz", "memory")
	_ = dm.RecordRetrievalSuccess("mem-xyz", "memory")
	_ = dm.RecordRetrieval("mem-xyz", "memory")

	meta, _ := dm.GetRetrievalMetadata("mem-xyz")
	if meta.ReuseCount != 3 {
		t.Errorf("ReuseCount = %d, want 3", meta.ReuseCount)
	}
	if meta.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", meta.SuccessCount)
	}
}

func TestGetRetrievalMetadata_UnknownNodeReturnsZero(t *testing.T) {
	dm := NewTestDM(t)

	meta, err := dm.GetRetrievalMetadata("never-seen")
	if err != nil {
		t.Fatalf("GetRetrievalMetadata on unknown: %v", err)
	}
	if meta.ReuseCount != 0 {
		t.Errorf("ReuseCount = %d, want 0 for unknown node", meta.ReuseCount)
	}
	if meta.SuccessCount != 0 {
		t.Errorf("SuccessCount = %d, want 0 for unknown node", meta.SuccessCount)
	}
	if meta.LastRetrievedAt != "" {
		t.Errorf("LastRetrievedAt = %q, want empty for unknown node", meta.LastRetrievedAt)
	}
}

func TestRecordRetrieval_RejectsEmptyArgs(t *testing.T) {
	dm := NewTestDM(t)

	if err := dm.RecordRetrieval("", "memory"); err == nil {
		t.Error("RecordRetrieval with empty nodeID: got nil error, want error")
	}
	if err := dm.RecordRetrieval("mem-x", ""); err == nil {
		t.Error("RecordRetrieval with empty nodeType: got nil error, want error")
	}
}

func TestDefaultRanker_PreservesFtsScore(t *testing.T) {
	// Phase 3 invariant: DefaultRanker must return ftsScore unchanged.
	// If a future change accidentally blends metadata into the score,
	// this test catches it before any search path is affected.
	r := DefaultRanker{}
	cases := []float64{-1.5, 0.0, 0.5, 1.0, 100.0, -0.0001}
	for _, fts := range cases {
		got := r.Score(fts, RetrievalMetadata{ReuseCount: 99, SuccessCount: 42})
		if got != fts {
			t.Errorf("DefaultRanker.Score(%v) = %v, want %v (ranker must pass through)", fts, got, fts)
		}
	}
}

func TestSetRanker_ReplacesAndRestores(t *testing.T) {
	prev := CurrentRanker()
	defer SetRanker(prev) // restore at end

	// Custom ranker that returns ftsScore * 2 when reuse_count > 0.
	blended := blendedRanker{}
	SetRanker(blended)

	got := CurrentRanker().Score(1.0, RetrievalMetadata{ReuseCount: 5, SuccessCount: 0})
	want := 2.0
	if got != want {
		t.Errorf("blended.Score(1.0, reuse=5) = %v, want %v", got, want)
	}

	// Restore the previous (default) ranker and confirm pass-through.
	SetRanker(DefaultRanker{})
	got = CurrentRanker().Score(1.0, RetrievalMetadata{ReuseCount: 5, SuccessCount: 0})
	if got != 1.0 {
		t.Errorf("after restore, Score(1.0) = %v, want 1.0", got)
	}
}

// blendedRanker is a test-only ranker demonstrating that future
// implementations can blend reuse_count into the FTS score. It is
// not used in production paths — DefaultRanker is.
type blendedRanker struct{}

func (blendedRanker) Score(ftsScore float64, meta RetrievalMetadata) float64 {
	if meta.ReuseCount > 0 {
		return ftsScore * 2.0
	}
	return ftsScore
}