package internal

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHybridSearch_VectorOnlyCandidatePreserved is the regression net for the
// 2026-08-26 merge fix: when a real embedding provider is active and the
// query has zero FTS5 token overlap with the corpus, the vector-only
// candidate MUST survive the merge and be returned by HybridSearch. Before
// the fix the merge step at hybrid_search.go:184-187 discarded every
// FTS5-absent row, even when VectorMatch had a high-similarity hit.
func TestHybridSearch_VectorOnlyCandidatePreserved(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	// Paraphrased query has zero token overlap with the corpus content.
	const query = "why does lifting a dispute on a memory not restore trust"

	// Seed one target row. The content uses corpus vocabulary tokens, but
	// the query doesn't — that's the whole point. The vector match must
	// still surface the row.
	const target = "vonly-target"
	const targetContent = "challenge restoration evidence neutralization confidence floor"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, metadata, created_at, weight) VALUES (?, 'memories', ?, '{}', strftime('%s','now'), 5)`,
		0, target, targetContent)
	require.NoError(t, err)

	// Embed the target row so the vector path has a stored embedding
	// to match against.
	provider := DefaultEmbeddingConfig().Provider
	if provider.Name() == "null" {
		t.Skip("requires a real embedding provider (set OLLAMA_ENDPOINT)")
	}
	vec, err := provider.Embed(targetContent)
	require.NoError(t, err)
	embJSON, err := json.Marshal(vec)
	require.NoError(t, err)
	_, err = dm.ExecTracked(
		`UPDATE memories SET embedding = ? WHERE id = ?`, 0, string(embJSON), target)
	require.NoError(t, err)

	// Run HybridSearch. With any VectorWeight > 0 the target must appear.
	cfg := DefaultHybridConfig()
	cfg.VectorWeight = 0.5
	cfg.Limit = 10
	results, err := HybridSearch(dm, query, "memories", cfg)
	require.NoError(t, err)
	require.NotEmpty(t, results, "vector-only candidate MUST be returned after the merge fix")
	require.Equal(t, target, results[0].ID, "target must rank #1")
	require.Equal(t, "vector", results[0].Source, "source must report vector")
	require.Greater(t, results[0].VectorSimilarity, 0.0, "vector similarity must be populated")
}

// TestHybridSearch_VectorOnlyAtVWOne returns a vector-only candidate
// when VectorWeight=1.0 (pure vector retrieval). Before the fix the
// merge dropped FTS5-absent rows unconditionally, so VW=1.0 also
// returned 0 results for paraphrased queries.
func TestHybridSearch_VectorOnlyAtVWOne(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	provider := DefaultEmbeddingConfig().Provider
	if provider.Name() == "null" {
		t.Skip("requires a real embedding provider (set OLLAMA_ENDPOINT)")
	}

	const targetContent = "challenge restoration evidence neutralization confidence floor"
	const target = "vw1-target"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, metadata, created_at, weight) VALUES (?, 'memories', ?, '{}', strftime('%s','now'), 5)`,
		0, target, targetContent)
	require.NoError(t, err)
	vec, err := provider.Embed(targetContent)
	require.NoError(t, err)
	embJSON, _ := json.Marshal(vec)
	_, err = dm.ExecTracked(
		`UPDATE memories SET embedding = ? WHERE id = ?`, 0, string(embJSON), target)
	require.NoError(t, err)

	cfg := DefaultHybridConfig()
	cfg.VectorWeight = 1.0
	cfg.Limit = 10

	// Query has zero FTS5 overlap.
	results, err := HybridSearch(dm, "an abandoned memory should not regain trust automatically",
		"memories", cfg)
	require.NoError(t, err)
	require.NotEmpty(t, results, "VW=1.0 MUST surface vector-only candidates")
	require.Equal(t, target, results[0].ID)
	require.Equal(t, "vector", results[0].Source)
}

// TestHybridSearch_BM25OnlyStillWorks confirms the existing BM25-only
// behavior is preserved when FTS5 actually returns hits. Vector-only
// candidates are now preserved too, but they should not displace a
// strong BM25 hit at the top of the ranking.
func TestHybridSearch_BM25OnlyStillWorks(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	provider := DefaultEmbeddingConfig().Provider
	hasRealProvider := provider.Name() != "null"

	// Strong FTS5 hit: query token "challenge" appears directly.
	const ftsHit = "bm25-strong"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, metadata, created_at, weight) VALUES (?, 'memories', 'challenge challenge challenge restoration', '{}', strftime('%s','now'), 5)`,
		0, ftsHit)
	require.NoError(t, err)

	// Embed it so the vector path also matches (full hybrid).
	if hasRealProvider {
		vec, err := provider.Embed("challenge challenge challenge restoration")
		require.NoError(t, err)
		embJSON, _ := json.Marshal(vec)
		_, err = dm.ExecTracked(
			`UPDATE memories SET embedding = ? WHERE id = ?`, 0, string(embJSON), ftsHit)
		require.NoError(t, err)
	}

	// FTS5-only candidate: no embedding, no vector signal — single token match.
	const ftsOnly = "bm25-fts-only"
	_, err = dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, metadata, created_at, weight) VALUES (?, 'memories', 'challenge something completely unrelated', '{}', strftime('%s','now'), 5)`,
		0, ftsOnly)
	require.NoError(t, err)

	// VW=0.0 with FTS5 hit "challenge" — strong BM25 row must rank first.
	cfg := DefaultHybridConfig()
	cfg.VectorWeight = 0.0
	cfg.Limit = 10
	results, err := HybridSearch(dm, "challenge", "memories", cfg)
	require.NoError(t, err)
	require.NotEmpty(t, results)
	require.Equal(t, ftsHit, results[0].ID,
		"FTS5-strong hit must rank #1 under VW=0; got %v",
		idsOfHybrid(results))
	// Source is "fts5" when no real embedding provider is active, or
	// "hybrid" when one is — both are correct because the FTS5 hit is
	// preserved in both cases. The bug under test was that the vector-only
	// candidate would displace this row; what we want to assert is that
	// this row WINS, not the Source label.
	require.Contains(t, []string{"fts5", "hybrid"}, results[0].Source,
		"VW=0 with FTS5 hit must produce source=fts5 or hybrid (got %q)",
		results[0].Source)
}

// TestHybridSearch_BothChannelsMerge confirms the union behavior: a row
// returned by BOTH FTS5 and vector is reported as Source=hybrid and
// gets a combined score, not just one of the channels.
func TestHybridSearch_BothChannelsMerge(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	provider := DefaultEmbeddingConfig().Provider
	if provider.Name() == "null" {
		t.Skip("requires a real embedding provider (set OLLAMA_ENDPOINT)")
	}

	const content = "cancellation verification lifecycle coupling invariant"
	const row = "both-channels"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, metadata, created_at, weight) VALUES (?, 'memories', ?, '{}', strftime('%s','now'), 5)`,
		0, row, content)
	require.NoError(t, err)
	vec, err := provider.Embed(content)
	require.NoError(t, err)
	embJSON, _ := json.Marshal(vec)
	_, err = dm.ExecTracked(
		`UPDATE memories SET embedding = ? WHERE id = ?`, 0, string(embJSON), row)
	require.NoError(t, err)

	cfg := DefaultHybridConfig()
	cfg.VectorWeight = 0.5
	cfg.Limit = 10
	// Query that hits BOTH channels: token "cancellation" matches FTS5
	// AND the embedding is semantically close.
	results, err := HybridSearch(dm, "cancellation verification invariant", "memories", cfg)
	require.NoError(t, err)
	require.NotEmpty(t, results)
	require.Equal(t, row, results[0].ID)
	require.Equal(t, "hybrid", results[0].Source,
		"row matched by both FTS5 and vector must report Source=hybrid")
	require.NotZero(t, results[0].FTS5Score, "FTS5 score must be populated for hybrid")
	require.NotZero(t, results[0].VectorSimilarity, "vector similarity must be populated for hybrid")
	require.NotZero(t, results[0].CombinedScore, "combined score must be populated for hybrid")
}

// TestHybridSearch_VectorOnlyThresholdFilter confirms the threshold
// filter applies to vector-only candidates. Raising RetrievalThreshold
// above the candidate's similarity must drop the candidate.
func TestHybridSearch_VectorOnlyThresholdFilter(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	provider := DefaultEmbeddingConfig().Provider
	if provider.Name() == "null" {
		t.Skip("requires a real embedding provider (set OLLAMA_ENDPOINT)")
	}

	const content = "challenge restoration evidence neutralization confidence floor"
	const target = "thresh-target"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content, metadata, created_at, weight) VALUES (?, 'memories', ?, '{}', strftime('%s','now'), 5)`,
		0, target, content)
	require.NoError(t, err)
	vec, err := provider.Embed(content)
	require.NoError(t, err)
	embJSON, _ := json.Marshal(vec)
	_, err = dm.ExecTracked(
		`UPDATE memories SET embedding = ? WHERE id = ?`, 0, string(embJSON), target)
	require.NoError(t, err)

	// Query with paraphrase (zero FTS5 overlap).
	cfg := DefaultHybridConfig()
	cfg.VectorWeight = 0.5
	cfg.Limit = 10

	// Low threshold: vector-only target survives.
	cfg.RetrievalThreshold = -3.0
	results, err := HybridSearch(dm, "abandoned task should not be marked verified",
		"memories", cfg)
	require.NoError(t, err)
	require.NotEmpty(t, results, "low threshold must keep vector-only target")

	// High threshold: vector similarity (~0.3-0.8 depending on model) is
	// below a threshold of 0.95, so the candidate should be filtered.
	cfg.RetrievalThreshold = 0.95
	results2, err := HybridSearch(dm, "abandoned task should not be marked verified",
		"memories", cfg)
	require.NoError(t, err)
	require.Empty(t, results2, "threshold > similarity must filter vector-only candidate")
}

// idsOfHybrid is a small helper for test failure messages.
func idsOfHybrid(rs []HybridResult) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}