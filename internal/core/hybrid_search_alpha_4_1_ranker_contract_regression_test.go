package internal

// Alpha-4.1 F-002 regression: pin the actual ranking contract.
//
// Per the alpha-4.1 audit, the public README at one point described
// ranking as `score = (reinforcement_count × 2) + (weight × 1.5) +
// recency_bonus`, but the live `DefaultRanker` returns FTS5 BM25 +
// cosine similarity only. The audit instruction is explicit: do not
// change ranking behavior to satisfy docs — instead pin the live
// contract so future changes have to update this test.
//
// This test is the single source of truth for "what does the current
// ranker do?" until a future PR introduces a weighted ranker.

import (
	"math"
	"testing"
)

// TestF002_Ranker_DoesNotConsumeWeightOrReinforcement verifies that two
// memories with the same FTS5 BM25 match score but different
// weight/reinforcement_count land in the SAME rank order. If a future
// PR adds a weighted ranker, this test fails and the PR must update
// the contract documentation as well.
func TestF002_Ranker_DoesNotConsumeWeightOrReinforcement(t *testing.T) {
	dm := NewTestDM(t)
	// Two memories with identical content (so FTS5 BM25 is comparable)
	// but radically different weight and reinforcement_count.
	const tag = "f002-rank-contract"
	mustExec(t, dm, `INSERT INTO memories (id, collection, content, tags, metadata, created_at, weight, reinforcement_count) VALUES (?, 'memory', ?, '[]', '{}', ?, ?, ?)`,
		"f002-low-"+tag, tag+" low weight low reinforcement", 1000, 0.0, 0)
	mustExec(t, dm, `INSERT INTO memories (id, collection, content, tags, metadata, created_at, weight, reinforcement_count) VALUES (?, 'memory', ?, '[]', '{}', ?, ?, ?)`,
		"f002-high-"+tag, tag+" high weight high reinforcement", 1000, 100.0, 1000)

	hits, err := dm.HybridSearchMemories(tag, "", 10, "local")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("want ≥2 hits, got %d", len(hits))
	}
	// Read scores back as float64 (HybridResult.CombinedScore is
	// float64). The two rows must have IDENTICAL combined_score
	// within float epsilon — weight/reinforcement must not have moved
	// either row up or down.
	scoreLow := hits[0]["combined_score"]
	scoreHigh := hits[1]["combined_score"]
	sfLow, okL := scoreLow.(float64)
	sfHigh, okH := scoreHigh.(float64)
	if !okL || !okH {
		t.Fatalf("combined_score not float64: low=%T high=%T", scoreLow, scoreHigh)
	}
	if math.Abs(sfLow-sfHigh) > 1e-9 {
		t.Errorf("combined_score differs by %g; weight/reinforcement must not influence rank. low=%g high=%g",
			math.Abs(sfLow-sfHigh), sfLow, sfHigh)
	}
}

// TestF002_Ranker_ReturnsBM25AndCosine sanity-checks that the
// combined_score carries a non-trivial BM25 contribution by feeding
// an obvious lexical hit and asserting the score is non-zero.
func TestF002_Ranker_ReturnsBM25AndCosine(t *testing.T) {
	dm := NewTestDM(t)
	const tag = "f002-bm25-present"
	mustExec(t, dm, `INSERT INTO memories (id, collection, content, tags, metadata, created_at, weight, reinforcement_count) VALUES (?, 'memory', ?, '[]', '{}', ?, ?, ?)`,
		"f002-bm25-"+tag, "unique-token-"+tag+" appears in the body", 1000, 1.0, 1)

	hits, err := dm.HybridSearchMemories("unique-token-"+tag, "", 5, "local")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("want ≥1 hit for unique token, got 0")
	}
	score, ok := hits[0]["combined_score"].(float64)
	if !ok {
		t.Fatalf("combined_score not float64: %T", hits[0]["combined_score"])
	}
	if score == 0 {
		t.Errorf("combined_score should be non-zero for obvious BM25 match; got 0")
	}
}

func mustExec(t *testing.T, dm *DatabaseManager, query string, args ...interface{}) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(query, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}
