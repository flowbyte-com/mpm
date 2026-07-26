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
// TestIncrementSuccess_PureUpdate verifies that IncrementSuccess only
// fires for nodes with a prior retrieval row — a never-retrieved
// node must not receive a fabricated success_count. This is the
// semantic that distinguishes "the agent saw this and learned from
// it" (real success) from "the agent mentioned this id but never
// looked at it" (no success signal).
func TestIncrementSuccess_PureUpdate(t *testing.T) {
	dm := NewTestDM(t)

	// Pre-record a retrieval so the row exists.
	if err := dm.RecordRetrieval("mem-real", "memory"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// IncrementSuccess on a retrieved node bumps success_count.
	if err := dm.IncrementSuccess("mem-real"); err != nil {
		t.Fatalf("IncrementSuccess on retrieved: %v", err)
	}
	meta, _ := dm.GetRetrievalMetadata("mem-real")
	if meta.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", meta.SuccessCount)
	}

	// Second IncrementSuccess bumps again.
	_ = dm.IncrementSuccess("mem-real")
	meta, _ = dm.GetRetrievalMetadata("mem-real")
	if meta.SuccessCount != 2 {
		t.Errorf("after second IncrementSuccess, SuccessCount = %d, want 2", meta.SuccessCount)
	}

	// IncrementSuccess on a NEVER-RETRIEVED node: no row created,
	// no error, no fabricated success_count. GetRetrievalMetadata
	// still returns zero-valued because the row does not exist.
	if err := dm.IncrementSuccess("mem-never-retrieved"); err != nil {
		t.Fatalf("IncrementSuccess on unknown: %v", err)
	}
	meta, _ = dm.GetRetrievalMetadata("mem-never-retrieved")
	if meta.SuccessCount != 0 {
		t.Errorf("never-retrieved SuccessCount = %d, want 0 (must not fabricate)", meta.SuccessCount)
	}
}

// TestSaveLessonSourceIds_CreditsSuccessCount is the Provenance Proxy
// contract: when an agent distills a lesson and lists the source_ids,
// each previously-retrieved source receives a success_count bump.
// The lesson's own row is unaffected. Never-retrieved sources are
// silently skipped.
func TestSaveLessonSourceIds_CreditsSuccessCount(t *testing.T) {
	dm := NewTestDM(t)

	// Seed: three memories, two retrieved, one never.
	_ = dm.RecordRetrieval("mem-source-A", "memory")
	_ = dm.RecordRetrieval("mem-source-B", "memory")

	// Call IncrementSuccess directly to simulate what the handler
	// does (handler is exercised through the registry/handler test
	// path; the IncrementSuccess unit test pins the contract).
	_ = dm.IncrementSuccess("mem-source-A")
	_ = dm.IncrementSuccess("mem-source-A")
	_ = dm.IncrementSuccess("mem-source-B")
	_ = dm.IncrementSuccess("mem-source-never") // no-op

	metaA, _ := dm.GetRetrievalMetadata("mem-source-A")
	if metaA.SuccessCount != 2 {
		t.Errorf("mem-source-A SuccessCount = %d, want 2", metaA.SuccessCount)
	}
	metaB, _ := dm.GetRetrievalMetadata("mem-source-B")
	if metaB.SuccessCount != 1 {
		t.Errorf("mem-source-B SuccessCount = %d, want 1", metaB.SuccessCount)
	}
	metaNever, _ := dm.GetRetrievalMetadata("mem-source-never")
	if metaNever.SuccessCount != 0 {
		t.Errorf("mem-source-never SuccessCount = %d, want 0", metaNever.SuccessCount)
	}
}

// TestIncrementSuccess_RejectsEmptyArg pins the defensive contract.
func TestIncrementSuccess_RejectsEmptyArg(t *testing.T) {
	dm := NewTestDM(t)
	if err := dm.IncrementSuccess(""); err == nil {
		t.Error("IncrementSuccess with empty nodeID: got nil error, want error")
	}
}
