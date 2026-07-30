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
	if meta.LastRetrievedAt == nil {
		t.Error("first retrieval LastRetrievedAt is nil, want populated")
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
	if meta.LastRetrievedAt != nil {
		t.Errorf("LastRetrievedAt = %v, want nil for unknown node", *meta.LastRetrievedAt)
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
// TestIncrementSuccess_UpsertBehavior pins the INSERT ... ON CONFLICT
// DO UPDATE semantic. The 2026-07-26 spec change moved from pure
// UPDATE to UPSERT so the Provenance Proxy can credit nodes that the
// agent cites from prior-session memory without having surfaced them
// via the read hooks in this turn. A citation is a legitimate
// signal of utility even when the current session hasn't read the
// node.
func TestIncrementSuccess_UpsertBehavior(t *testing.T) {
	dm := NewTestDM(t)

	// Pre-record a retrieval so the row exists.
	if err := dm.RecordRetrieval("mem-real", "memory"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// IncrementSuccess on a retrieved node bumps success_count.
	if err := dm.IncrementSuccess("mem-real", "memory"); err != nil {
		t.Fatalf("IncrementSuccess on retrieved: %v", err)
	}
	meta, _ := dm.GetRetrievalMetadata("mem-real")
	if meta.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", meta.SuccessCount)
	}

	// Second IncrementSuccess bumps again.
	_ = dm.IncrementSuccess("mem-real", "memory")
	meta, _ = dm.GetRetrievalMetadata("mem-real")
	if meta.SuccessCount != 2 {
		t.Errorf("after second IncrementSuccess, SuccessCount = %d, want 2", meta.SuccessCount)
	}

	// IncrementSuccess on a NEVER-RETRIEVED node: row is created
	// with success_count=1, reuse_count=0, last_retrieved_at=NULL.
	// This is the key semantic shift from the prior UPDATE-only
	// implementation — the agent may legitimately cite a node from
	// prior-session memory without having read it this turn.
	if err := dm.IncrementSuccess("mem-never-retrieved", "memory"); err != nil {
		t.Fatalf("IncrementSuccess on unknown: %v", err)
	}
	meta, _ = dm.GetRetrievalMetadata("mem-never-retrieved")
	if meta.SuccessCount != 1 {
		t.Errorf("never-retrieved SuccessCount = %d, want 1 (UPSERT inserts first-time citations)", meta.SuccessCount)
	}
	if meta.ReuseCount != 0 {
		t.Errorf("never-retrieved ReuseCount = %d, want 0", meta.ReuseCount)
	}
	if meta.LastRetrievedAt != nil {
		t.Errorf("never-retrieved LastRetrievedAt = %v, want nil (never retrieved)", *meta.LastRetrievedAt)
	}
	if meta.NodeType != "memory" {
		t.Errorf("never-retrieved NodeType = %q, want memory", meta.NodeType)
	}

	// A second IncrementSuccess on the new row increments.
	_ = dm.IncrementSuccess("mem-never-retrieved", "memory")
	meta, _ = dm.GetRetrievalMetadata("mem-never-retrieved")
	if meta.SuccessCount != 2 {
		t.Errorf("after second IncrementSuccess on first-time row, SuccessCount = %d, want 2", meta.SuccessCount)
	}
}

// TestIncrementSuccess_DefaultsNodeType pins the defensive contract
// that an empty node_type falls back to "memory" — the schema's
// NOT NULL constraint requires a value, and an unknown id format
// must not block the citation path.
func TestIncrementSuccess_DefaultsNodeType(t *testing.T) {
	dm := NewTestDM(t)

	if err := dm.IncrementSuccess("some-uuid", ""); err != nil {
		t.Fatalf("IncrementSuccess with empty type: %v", err)
	}
	meta, _ := dm.GetRetrievalMetadata("some-uuid")
	if meta.NodeType != "memory" {
		t.Errorf("NodeType = %q, want memory (default for empty type)", meta.NodeType)
	}
}

// TestSaveLessonSourceIds_CreditsSuccessCount is the Provenance Proxy
// contract: when an agent distills a lesson and lists the source_ids,
// each source receives a success_count bump. Previously-retrieved
// sources see their existing count incremented; never-retrieved
// sources get a fresh row with success_count=1.
//
// The handler invokes IncrementSuccess per source_id with the
// inferred node_type; this test exercises the same path through
// IncrementSuccess directly so the contract is pinned without
// taking on the full handler/MCP test burden.
func TestSaveLessonSourceIds_CreditsSuccessCount(t *testing.T) {
	dm := NewTestDM(t)

	// Seed: three memories, two retrieved, one never.
	_ = dm.RecordRetrieval("mem-source-A", "memory")
	_ = dm.RecordRetrieval("mem-source-B", "memory")

	// Simulate the handler's per-source-id iteration.
	_ = dm.IncrementSuccess("mem-source-A", "memory")
	_ = dm.IncrementSuccess("mem-source-A", "memory")
	_ = dm.IncrementSuccess("mem-source-B", "memory")
	_ = dm.IncrementSuccess("mem-source-never", "memory") // first-time citation

	metaA, _ := dm.GetRetrievalMetadata("mem-source-A")
	if metaA.SuccessCount != 2 {
		t.Errorf("mem-source-A SuccessCount = %d, want 2", metaA.SuccessCount)
	}
	metaB, _ := dm.GetRetrievalMetadata("mem-source-B")
	if metaB.SuccessCount != 1 {
		t.Errorf("mem-source-B SuccessCount = %d, want 1", metaB.SuccessCount)
	}
	metaNever, _ := dm.GetRetrievalMetadata("mem-source-never")
	if metaNever.SuccessCount != 1 {
		t.Errorf("mem-source-never SuccessCount = %d, want 1 (UPSERT inserts first-time citation)", metaNever.SuccessCount)
	}
}

// TestIncrementSuccess_RejectsEmptyArg pins the defensive contract.
func TestIncrementSuccess_RejectsEmptyArg(t *testing.T) {
	dm := NewTestDM(t)
	if err := dm.IncrementSuccess("", "memory"); err == nil {
		t.Error("IncrementSuccess with empty nodeID: got nil error, want error")
	}
}
