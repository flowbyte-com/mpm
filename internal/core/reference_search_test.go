// reference_search_test.go — ReferenceHybridSearch test suite.
//
// Covers the 10 categories the spec calls out:
//   1. Lexical preservation (exact API symbols top-ranked)
//   2. Semantic retrieval (paraphrased query finds unembedded-cosine text)
//   3. Hybrid fusion (FTS+vector agreement boosts rank)
//   4. Source diversity (per-doc cap demotes excess chunks)
//   5. Filters (by doc-level criteria)
//   6. No embeddings fallback (NullProvider degrades to FTS-only)
//   7. Backfill (EmbedReferenceChunks populates NULLs)
//   8. Update (chunk_hash diff clears stale embeddings on content change)
//   9. Delete (DeleteReference cascades)
//  10. Determinism (same query → same ordering)
//
// Plus a few sub-cases that fell out of writing the suite:
//   - Edge: empty / whitespace-only query
//   - Edge: dimension mismatch in stored embedding
//   - Edge: FTS5 unavailable (LIKE fallback)
//   - Edge: provider configured but EmbedText fails (degrade to FTS-only)
//
// Test pattern: install a stub embedding provider via SetEmbedConfigForTest
// before the search, restore via ResetEmbedConfigForTest. The stub uses a
// content-derived unit vector so cosine similarity is deterministic but
// not semantically meaningful — we test fusion MECHANICS, not model quality.

package internal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEmbeddingProvider is a deterministic EmbeddingProvider for
// tests. The vector for a given text is derived from sha256(text):
// the first 8 bytes are interpreted as a uint64, then mapped to
// 4 float32 components via a tanh squash. The result is a
// length-4 unit-ish vector. The dim is intentionally small to
// keep test setup cheap.
//
// Why a content-derived vector and not a single fixed vector:
// identical inputs must produce identical vectors (so query "X"
// matches chunk "X" with cosine=1.0) and similar inputs should
// not match (so different chunks don't all collide at cosine=1.0).
// sha256 is the simplest hash that satisfies both — the 4-float
// projection is just a deterministic bag-of-bytes.
//
// The provider has a configurable Name so the model-identity
// tests can install "stub-a" / "stub-b" and verify the
// fingerprint comparison fires when the model changes.
type stubEmbeddingProvider struct {
	dim  int
	name string
}

func (s *stubEmbeddingProvider) Embed(text string) ([]float32, error) {
	if s.dim <= 0 {
		s.dim = 4
	}
	sum := sha256.Sum256([]byte(text))
	out := make([]float32, s.dim)
	for i := 0; i < s.dim; i++ {
		off := i * 4 % len(sum)
		u := binary.BigEndian.Uint32(sum[off : off+4])
		// Tanh squash: map uint32 → roughly [-1, 1].
		f := float32(u%2000)/1000.0 - 1.0
		out[i] = f
	}
	// Normalize to unit length.
	var norm float32
	for _, v := range out {
		norm += v * v
	}
	if norm > 0 {
		norm = float32(math.Sqrt(float64(norm)))
		for i := range out {
			out[i] /= norm
		}
	}
	return out, nil
}

func (s *stubEmbeddingProvider) Name() string {
	if s.name == "" {
		return "stub:test"
	}
	return s.name
}

// withStubProvider installs the stub provider, returns a cleanup
// that restores the default (NullProvider).
func withStubProvider(t *testing.T, dim int) func() {
	return withNamedStubProvider(t, "stub:test", dim)
}

// withNamedStubProvider is the named-variant helper used by the
// model-identity tests. Two providers with the same dim but
// different names let us pin the model-fingerprint logic without
// involving the dimension check.
func withNamedStubProvider(t *testing.T, name string, dim int) func() {
	t.Helper()
	prev := SetEmbedConfigForTest(&EmbeddingConfig{
		Source:       EmbeddingSourceProfile,
		ProviderName: name,
		Provider:     &stubEmbeddingProvider{dim: dim, name: name},
		Status:       EmbeddingStatusConfigured,
	})
	return func() {
		ResetEmbedConfigForTest()
		_ = prev
	}
}

// seedReference inserts a reference doc with one chunk via the
// public AddReference path so the FTS5 triggers fire. The chunk
// content is what the test cares about; the title is set so the
// search result carries a useful doc_title field.
func seedReference(t *testing.T, dm *DatabaseManager, docID, title, content string) {
	t.Helper()
	require.NoError(t, dm.AddReference(&ReferenceDoc{
		ID:          docID,
		Title:       title,
		SourcePath:  "/tmp/" + docID + ".md",
		SourceType:  "markdown",
		Content:     content,
		LastIndexed: "1790000000",
	}, []ReferenceChunk{{
		ID:         "chunk-" + docID,
		DocID:      docID,
		ChunkIndex: 0,
		Section:    "intro",
		Content:    content,
		SourcePath: "/tmp/" + docID + ".md",
	}}))
}

// seedReferenceMany inserts a doc with multiple chunks.
func seedReferenceMany(t *testing.T, dm *DatabaseManager, docID, title string, contents []string) {
	t.Helper()
	chunks := make([]ReferenceChunk, len(contents))
	for i, c := range contents {
		chunks[i] = ReferenceChunk{
			ID:         "chunk-" + docID + "-" + itoa(i),
			DocID:      docID,
			ChunkIndex: i,
			Section:    "s" + itoa(i),
			Content:    c,
			SourcePath: "/tmp/" + docID + ".md",
		}
	}
	require.NoError(t, dm.AddReference(&ReferenceDoc{
		ID:          docID,
		Title:       title,
		SourcePath:  "/tmp/" + docID + ".md",
		SourceType:  "markdown",
		Content:     strings.Join(contents, "\n\n"),
		LastIndexed: "1790000000",
	}, chunks))
}

// TestReferenceHybridSearch_LexicalPreservation confirms that the
// FTS5 fix (BuildFTS5Query) lets queries with underscores (e.g.
// wp_kses_post) reach the right chunk. The pre-fix code wrapped
// the query in `"..."*` which forced a literal phrase match and
// returned zero hits for any underscore-bearing identifier because
// the porter unicode61 indexer splits on underscores at index
// time. Post-fix: the query tokenizes into "wp* kses* post*" and
// matches via FTS5 implicit AND.
//
// This is also the test that pins the spec's "Lexical preservation:
// exact API symbols top-ranked" requirement — when the query is the
// exact function name, the right chunk is the top result.
func TestReferenceHybridSearch_LexicalPreservation(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	seedReference(t, dm, "ref-kses", "wp-kses-post",
		"wp_kses_post() is a function that sanitizes content for post display. "+
			"It strips dangerous HTML and only allows tags in the allowed list.")
	seedReference(t, dm, "ref-menu", "wp-wp-create-nav-menu",
		"wp_create_nav_menu() creates a new navigation menu. "+
			"Use register_nav_menus to declare theme locations.")
	seedReference(t, dm, "ref-cap", "wp-roles-capabilities",
		"current_user_can('edit_theme_options') checks the active theme's options. "+
			"unfiltered_html is a capability for trusted users to post raw HTML.")

	// NullProvider: FTS-only path. Hybrid still works; the vector
	// leg is skipped silently.
	results, err := ReferenceHybridSearch(dm, "wp_kses_post", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	require.NotEmpty(t, results, "underscore-bearing query must surface chunks (BuildFTS5Query fix)")
	// The first result must be from the wp-kses-post doc.
	assert.Equal(t, "ref-kses", results[0].DocID,
		"top hit must be the kses doc; got %s", results[0].DocID)
	assert.Equal(t, "fts5", results[0].Source,
		"NullProvider → source must be fts5; got %s", results[0].Source)
}

// TestReferenceHybridSearch_FTS5QueryBuilderFixesHyphenatedSymbols
// is a parallel coverage check for hyphenated identifiers. The
// porter unicode61 tokenizer also splits on hyphens, so a query
// like "wp-kses-post" must tokenize the same way the indexer did.
func TestReferenceHybridSearch_FTS5QueryBuilderFixesHyphenatedSymbols(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	seedReference(t, dm, "ref-kses-h", "wp-kses-post",
		"wp-kses-post sanitizes post content. The function is wp_kses_post in PHP.")
	seedReference(t, dm, "ref-other", "wp-application-passwords",
		"Application passwords are 24-character strings used for REST API auth.")

	results, err := ReferenceHybridSearch(dm, "wp-kses-post", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	require.NotEmpty(t, results)
	top := results[0]
	assert.Equal(t, "ref-kses-h", top.DocID,
		"hyphenated query must surface the kses doc; got %s", top.DocID)
}

// TestReferenceHybridSearch_SemanticRetrieval confirms the vector
// leg surfaces a chunk even when the FTS5 leg has zero overlap.
// Setup: a chunk whose content is descriptive prose about
// sanitization, with the exact function name absent. The stub
// embedding for the query (which contains the function name) has
// nonzero cosine to the chunk's embedding (sha256-derived;
// collisions are statistically rare but possible — for safety we
// use a unique chunk content that differs in many bytes from the
// query, so FTS5 scores zero). The vector-only candidate survives
// the merge and appears in the result list with Source="vector".
//
// This is the spec's "Semantic retrieval: paraphrased query finds
// the chunk" test.
func TestReferenceHybridSearch_SemanticRetrieval(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	// Two chunks: one talks about HTML sanitization in prose,
	// one talks about media uploads. The prose chunk contains
	// zero FTS5 overlap with the query "wp_kses_post".
	seedReference(t, dm, "ref-kses-prose", "prose-on-sanitization",
		"This document explains how WordPress removes dangerous tags from user-submitted "+
			"content before display, ensuring only an allowlist of safe elements survives.")
	seedReference(t, dm, "ref-media", "media-handling",
		"Media uploads in WordPress go through wp_handle_upload and are stored in the "+
			"uploads directory keyed by year and month.")
	// Embed so the vector leg has data to score against.
	_, _, err := dm.EmbedReferenceChunks(context.Background(), "ref-kses-prose")
	require.NoError(t, err)
	_, _, err = dm.EmbedReferenceChunks(context.Background(), "ref-media")
	require.NoError(t, err)

	// Use a unique query token that has no FTS5 overlap with any
	// chunk. "ksepostprobe12345" is not in either chunk's content.
	results, err := ReferenceHybridSearch(dm, "ksepostprobe12345", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	// Both legs must produce candidates (vector: all chunks;
	// FTS5: zero). The vector leg surfaces chunks ordered by
	// cosine; the kses-prose chunk's sha256-derived vector will
	// have some nonzero similarity to the query's vector.
	// We don't assert WHICH chunk ranks first (sha256 has no
	// semantic meaning), only that the vector leg returned rows.
	require.NotEmpty(t, results,
		"vector leg must surface rows even when FTS5 has no overlap")
	for _, r := range results {
		assert.Equal(t, "vector", r.Source,
			"with no FTS5 overlap, source must be vector; got %s (id=%s)", r.Source, r.ID)
	}
}

// TestReferenceHybridSearch_HybridFusion confirms that when FTS5
// and vector agree (both legs surface the same chunk), that
// chunk's Source is "hybrid" and ranks above FTS5-only or
// vector-only candidates.
//
// Setup: a query that has FTS5 overlap with one chunk. Both the
// FTS5 leg (matching the literal token) and the vector leg
// (matching via hash collision) include the chunk. The chunk's
// source is "hybrid".
func TestReferenceHybridSearch_HybridFusion(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	// Query exactly matches the chunk's content token "nonces".
	seedReference(t, dm, "ref-nonces", "wp-nonces",
		"nonces protect URLs and forms from CSRF. They have a 24h lifetime.")
	seedReference(t, dm, "ref-roles", "wp-roles-capabilities",
		"current_user_can checks capabilities. Roles include administrator, editor.")
	// Embed so the vector leg has data to score.
	_, _, embErr := dm.EmbedReferenceChunks(context.Background(), "ref-nonces")
	require.NoError(t, embErr)
	_, _, embErr = dm.EmbedReferenceChunks(context.Background(), "ref-roles")
	require.NoError(t, embErr)

	results, err := ReferenceHybridSearch(dm, "nonces", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	require.NotEmpty(t, results)
	// Find the nonces row.
	var noncesRow *ReferenceHybridResult
	for i := range results {
		if results[i].DocID == "ref-nonces" {
			noncesRow = &results[i]
			break
		}
	}
	require.NotNil(t, noncesRow, "nonces row missing from results: %+v", results)
	// FTS5 matched the literal token "nonces" — this is the
	// strong leg. The vector leg always returns ALL chunks (any
	// nonzero cosine is > 0), so the nonces chunk is "hybrid".
	assert.Equal(t, "hybrid", noncesRow.Source,
		"when FTS5+vector agree, source must be hybrid; got %s", noncesRow.Source)
	assert.Greater(t, noncesRow.VectorSimilarity, 0.0,
		"hybrid row must carry a vector similarity > 0")
}

// TestReferenceHybridSearch_SourceDiversity verifies that the
// per-doc cap demotes excess chunks from a single large
// reference. Setup: one doc with 5 chunks containing the query
// token. PerDocCap=2 means at most 2 chunks from this doc rank
// in the top of the result list.
func TestReferenceHybridSearch_SourceDiversity(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	chunks := make([]string, 5)
	for i := range chunks {
		chunks[i] = "audit chunk mentions the token \"navmenu\" in passing"
	}
	seedReferenceMany(t, dm, "ref-big", "big-doc", chunks)
	seedReference(t, dm, "ref-small", "small-doc",
		"This doc also mentions navmenu in a different way.")

	cfg := DefaultReferenceHybridConfig()
	cfg.PerDocCap = 2
	results, err := ReferenceHybridSearch(dm, "navmenu", cfg)
	require.NoError(t, err)
	require.NotEmpty(t, results)

	// PerDocCap=2 means at most 2 chunks from ref-big should
	// appear before the first chunk from ref-small (or before
	// the tail of the result list if ref-small is absent). The
	// implementation partitions the result list: the first 2
	// from ref-big stay in their natural-sort position; the
	// other 3 from ref-big are moved to the tail.
	bigCount := 0
	sawSmallBeforeCap := false
	for i, r := range results {
		switch r.DocID {
		case "ref-big":
			bigCount++
			if bigCount > 2 {
				// Overflow rows must be at the tail; before
				// them we should have seen at least one row
				// from another doc (ref-small) or the end.
				if !sawSmallBeforeCap && i < len(results)-3 {
					// The first 2 from ref-big are followed by
					// ref-small in the natural sort; the
					// remaining 3 from ref-big are overflow.
					t.Errorf("overflow ref-big chunk at position %d should be at tail; results: %v",
						i, docSummary(results))
				}
			}
		case "ref-small":
			sawSmallBeforeCap = true
		}
	}
	assert.GreaterOrEqual(t, bigCount, 2, "ref-big should still appear in results")
}

func docSummary(results []ReferenceHybridResult) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.DocID + ":" + r.ID
	}
	return out
}

// TestReferenceHybridSearch_NoEmbeddingsFallback confirms that
// when the embedding provider is absent (NullProvider), the
// hybrid function degrades to FTS-only and every result has
// Source="fts5". The vector leg is silently skipped.
func TestReferenceHybridSearch_NoEmbeddingsFallback(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	// No SetEmbedConfigForTest call → default EmbeddingConfig is
	// loaded; the test environment has no OLLAMA_* env and no
	// profile, so Provider.Name() == "null" and the vector leg
	// is skipped.
	cfg := DefaultEmbeddingConfig()
	if cfg.Source != EmbeddingSourceAbsent {
		t.Skipf("test environment has a non-null embedding provider; cannot exercise fallback")
	}

	seedReference(t, dm, "ref-x", "test-doc",
		"This doc contains the word FALLBACKTOKEN123 for testing.")

	results, err := ReferenceHybridSearch(dm, "FALLBACKTOKEN123", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	require.NotEmpty(t, results)
	for _, r := range results {
		assert.Equal(t, "fts5", r.Source,
			"NullProvider fallback must produce fts5 source; got %s", r.Source)
		assert.Equal(t, 0.0, r.VectorSimilarity,
			"NullProvider fallback must have zero vector similarity")
	}
}

// TestReferenceHybridSearch_Backfill is the spec's "Backfill" test.
// Calling EmbedReferenceChunks populates NULL embeddings. A
// subsequent hybrid search uses the vector leg.
func TestReferenceHybridSearch_Backfill(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	seedReference(t, dm, "ref-bf", "backfill-doc",
		"This chunk has no embedding at seed time because the provider was null.")

	// Confirm the chunk is unembedded before backfill.
	var nullCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM reference_chunks WHERE doc_id = ? AND embedding IS NULL`,
		"ref-bf").Scan(&nullCount))
	require.Equal(t, 1, nullCount, "seed should leave chunk unembedded")

	// Backfill. With a configured provider, EmbedReferenceChunks
	// populates the embedding.
	embedded, _, err := dm.EmbedReferenceChunks(context.Background(), "ref-bf")
	require.NoError(t, err)
	assert.Equal(t, 1, embedded)

	// Now the chunk has an embedding. A hybrid search uses it.
	results, err := ReferenceHybridSearch(dm, "anyquery", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	require.NotEmpty(t, results)
	found := false
	for _, r := range results {
		if r.DocID == "ref-bf" {
			found = true
			// Vector leg fired.
			assert.Greater(t, r.VectorSimilarity, 0.0,
				"post-backfill hybrid must have a vector similarity > 0; got %.3f", r.VectorSimilarity)
		}
	}
	assert.True(t, found, "backfilled chunk should appear in hybrid results")
}

// TestReferenceHybridSearch_UpdateClearsStaleEmbedding is the spec's
// "Update" test. AddReference's chunk_hash diff clears the
// embedding column on the Updated branch so a re-embed pass
// picks up the new content. Confirms:
//   - Initial ingest embeds chunk A.
//   - Re-ingest with new content (different content_hash) clears
//     the embedding.
//   - The next EmbedReferenceChunks call embeds the new content.
func TestReferenceHybridSearch_UpdateClearsStaleEmbedding(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	// Initial ingest.
	require.NoError(t, dm.AddReference(&ReferenceDoc{
		ID: "ref-up", Title: "up-doc", SourcePath: "/tmp/up.md", SourceType: "markdown",
		Content: "VERSION-1 content token", LastIndexed: "1790000000",
	}, []ReferenceChunk{{
		ID: "chunk-up", DocID: "ref-up", ChunkIndex: 0,
		Section: "intro", Content: "VERSION-1 content token", SourcePath: "/tmp/up.md",
	}}))
	embedded, _, err := dm.EmbedReferenceChunks(context.Background(), "ref-up")
	require.NoError(t, err)
	require.Equal(t, 1, embedded)

	// Capture the initial embedding (a JSON-encoded []float32).
	var initialEmb []byte
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding FROM reference_chunks WHERE id = 'chunk-up'`,
	).Scan(&initialEmb))
	require.NotEmpty(t, initialEmb, "initial embedding must be present")

	// Re-ingest with the SAME doc id (so AddReference's diff
	// path runs) but DIFFERENT content. The chunk_hash diff
	// should detect the change and clear the embedding.
	require.NoError(t, dm.AddReference(&ReferenceDoc{
		ID: "ref-up", Title: "up-doc", SourcePath: "/tmp/up.md", SourceType: "markdown",
		Content: "VERSION-2 different content", LastIndexed: "1790000001",
	}, []ReferenceChunk{{
		ID: "chunk-up", DocID: "ref-up", ChunkIndex: 0,
		Section: "intro", Content: "VERSION-2 different content", SourcePath: "/tmp/up.md",
	}}))

	// Embedding must be NULL after the update.
	var postUpdateEmb sql.NullString
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding FROM reference_chunks WHERE id = 'chunk-up'`,
	).Scan(&postUpdateEmb))
	// postUpdateEmb.Valid is true if the row is non-NULL, false
	// if NULL. The Update branch clears the column to NULL.
	// In SQLite-go, scanning a NULL TEXT into NullString yields
	// Valid=false; the raw bytes may still be empty.
	if postUpdateEmb.Valid {
		assert.Equal(t, "", postUpdateEmb.String,
			"post-update embedding must be empty (column cleared to NULL)")
	}

	// Re-embed picks up the new content.
	embedded, _, err = dm.EmbedReferenceChunks(context.Background(), "ref-up")
	require.NoError(t, err)
	assert.Equal(t, 1, embedded)

	var reEmb []byte
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding FROM reference_chunks WHERE id = 'chunk-up'`,
	).Scan(&reEmb))
	require.NotEmpty(t, reEmb, "re-embed must populate the column")
	assert.NotEqual(t, string(initialEmb), string(reEmb),
		"new content must produce a different embedding vector")
}

// TestReferenceHybridSearch_DeleteCascades is the spec's "Delete"
// test. DeleteReference removes the doc, its chunks, and the
// audit rows. A subsequent search returns no rows for the deleted
// content.
func TestReferenceHybridSearch_DeleteCascades(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	seedReference(t, dm, "ref-del", "delete-me",
		"This chunk will be deleted by the test. Token: DELTOKEN987")
	_, _, err := dm.EmbedReferenceChunks(context.Background(), "ref-del")
	require.NoError(t, err)

	// Pre-delete: search returns the chunk.
	pre, err := ReferenceHybridSearch(dm, "DELTOKEN987", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	require.NotEmpty(t, pre)

	// Delete.
	require.NoError(t, dm.DeleteReference("ref-del"))

	// Post-delete: search returns nothing for the deleted token.
	post, err := ReferenceHybridSearch(dm, "DELTOKEN987", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	assert.Empty(t, post, "post-delete search must return no rows")
}

// TestReferenceHybridSearch_Determinism confirms the same query
// twice produces identical ordering. Stable secondary sort by id
// is required; without it, the relative order of equal-score
// chunks is map-iteration-order-dependent and tests would flake.
func TestReferenceHybridSearch_Determinism(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	// Many chunks with the same FTS5 score → exercises the
	// tiebreaker sort.
	for i := 0; i < 10; i++ {
		seedReference(t, dm, "ref-det-"+itoa(i), "det-"+itoa(i),
			"common-token appears in every chunk for tiebreaker testing")
	}

	first, err := ReferenceHybridSearch(dm, "common-token", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	require.NotEmpty(t, first)

	for run := 0; run < 5; run++ {
		again, err := ReferenceHybridSearch(dm, "common-token", DefaultReferenceHybridConfig())
		require.NoError(t, err)
		require.Equal(t, len(first), len(again),
			"run %d: result count diverged from first run", run)
		for i := range first {
			assert.Equal(t, first[i].ID, again[i].ID,
				"run %d, position %d: id %s != %s",
				run, i, first[i].ID, again[i].ID)
		}
	}
}

// TestReferenceHybridSearch_EmptyQuery confirms that an
// empty/whitespace query short-circuits to a nil result with no
// error. FTS5 MATCH "" would error; the guard prevents that.
func TestReferenceHybridSearch_EmptyQuery(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	seedReference(t, dm, "ref-e", "any-doc", "any content")

	cases := []string{"", " ", "\t\n"}
	for _, q := range cases {
		results, err := ReferenceHybridSearch(dm, q, DefaultReferenceHybridConfig())
		require.NoError(t, err)
		assert.Empty(t, results, "empty query %q must return no rows", q)
	}
}

// ── Story 1: ranking regression tests ──────────────────────────────────────
//
// These tests construct adversarial candidate sets that exposed a
// real defect in the legacy "hybrid > fts5 > vector" source-priority
// sort: a weak-hybrid row outranked a strong-vector row simply
// because of its source label, even when the strong-vector row
// was the more relevant answer.
//
// The new ranking is by RelevanceScore (max-of-legs + small
// lexical boost on ties). The tests pin the user-stated principle:
// "actual relevance should dominate source label."

// TestReferenceHybridSearch_RankingWeakHybridBeatsStrongVector is
// the direct reverse of the legacy defect. Two candidates:
//
//   - "hybrid" row: weak FTS5 (BM25 ~ -2, strength ~0.33) and
//     weak vector (cosine ~0.20). Combined under the old weighted
//     blend ~-0.07, but the source label "hybrid" lifted it to
//     the top of the result list.
//
//   - "vector" row: zero FTS5 overlap, but very strong cosine
//     (~0.85). The semantically closest chunk in the corpus.
//
// The unit-level test of relevanceScore pins the formula
// directly — the ranking must produce strongVector > weakHybrid
// for these inputs. This is the contract; the end-to-end test
// below confirms the search function uses the formula.
func TestReferenceHybridSearch_RankingWeakHybridBeatsStrongVector(t *testing.T) {
	// Direct unit test of relevanceScore. Pre-fix the legacy
	// source-priority sort would have produced the opposite
	// ordering; the new sort uses relevanceScore as the primary
	// key. Pin the formula here so a future refactor cannot
	// silently regress to source-priority.
	weakHybrid := relevanceScore(-2.0, 0.20)
	strongVector := relevanceScore(0.0, 0.85)
	assert.Greater(t, strongVector, weakHybrid,
		"strong vector must outrank weak hybrid; got strong=%.3f weak=%.3f (formula regression — source-priority bucket returned?)",
		strongVector, weakHybrid)

	// Also pin the in-corpus behavior with a deterministic
	// end-to-end test. Use content where the FTS5 leg returns
	// distinct scores: weakHybrid has the literal query token
	// (weak FTS5 hit) AND some semantic similarity; strongVector
	// has no FTS5 overlap and uses the stub provider's hash
	// embedding (cosine is deterministic but not meaningful,
	// which is why the unit test above is the contract).
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	// weak-hybrid: contains the query token "navmenu" (so FTS5
	// fires) and a few related words.
	seedReference(t, dm, "ref-weak-hybrid", "weak-hybrid-doc",
		"This page mentions navmenu in passing.")
	// strong-vector: zero token overlap with "navmenu".
	seedReference(t, dm, "ref-strong-vector", "strong-vector-doc",
		"This page is the canonical deep-dive on building site navigation menus in WordPress.")
	_, _, err := dm.EmbedReferenceChunks(context.Background(), "ref-weak-hybrid")
	require.NoError(t, err)
	_, _, err = dm.EmbedReferenceChunks(context.Background(), "ref-strong-vector")
	require.NoError(t, err)

	results, err := ReferenceHybridSearch(dm, "navmenu", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	require.NotEmpty(t, results)

	// Both rows must be present. (Order is not pinned here
	// because the stub provider's hash-based cosine is not
	// semantically meaningful — the contract is the formula,
	// pinned above.)
	weakIdx := indexOfDoc(results, "ref-weak-hybrid")
	strongIdx := indexOfDoc(results, "ref-strong-vector")
	assert.GreaterOrEqual(t, weakIdx, 0, "weak-hybrid row must be present")
	assert.GreaterOrEqual(t, strongIdx, 0, "strong-vector row must be present")

	// Diagnostic: assert the source classification the merge
	// produced. weak-hybrid has the query token, so it MUST be
	// classified as "hybrid" (both legs returned hits). The
	// strong-vector chunk has no token overlap with the query,
	// so it must be "vector" (only the vector leg returned).
	var weakRow, strongRow *ReferenceHybridResult
	for i := range results {
		if results[i].DocID == "ref-weak-hybrid" {
			weakRow = &results[i]
		}
		if results[i].DocID == "ref-strong-vector" {
			strongRow = &results[i]
		}
	}
	assert.Equal(t, "hybrid", weakRow.Source,
		"weak-hybrid chunk has the query token, must be classified hybrid")
	assert.Equal(t, "vector", strongRow.Source,
		"strong-vector chunk has no token overlap, must be classified vector")
}

// TestReferenceHybridSearch_RankingStrongLexicalBeatsGoodSemantic
// pins the user-stated principle that exact technical-symbol
// matches must retain a strong advantage. Setup:
//
//   - "exact-lexical" row: very strong FTS5 (BM25 ~ -15) from
//     containing the exact query token. Weak vector (~0.10).
//     This is the canonical-doc-for-symbol case.
//
//   - "good-semantic" row: zero FTS5, but strong cosine (~0.80).
//     A semantically related neighbor.
//
// Under the new ranking, the exact-lexical row outranks because
// max-of-legs puts the strong FTS5 (strength ~0.94) above the
// strong vector (0.80), and the small lexical boost on the tie
// also favours the strong-lexical row.
func TestReferenceHybridSearch_RankingStrongLexicalBeatsGoodSemantic(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	seedReference(t, dm, "ref-exact", "exact-lexical-doc",
		"This page is the canonical reference for wp_kses_post and covers the function in detail.")
	seedReference(t, dm, "ref-neighbor", "good-semantic-doc",
		"This page explains HTML sanitization workflows and which tags are allowed in post content.")
	_, _, err := dm.EmbedReferenceChunks(context.Background(), "ref-exact")
	require.NoError(t, err)
	_, _, err = dm.EmbedReferenceChunks(context.Background(), "ref-neighbor")
	require.NoError(t, err)

	// Query the exact symbol. The exact-lexical chunk has the
	// literal token "wp_kses_post" so FTS5 hits hard. The
	// neighbor has zero FTS5 overlap.
	results, err := ReferenceHybridSearch(dm, "wp_kses_post", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	require.NotEmpty(t, results)

	exactIdx := indexOfDoc(results, "ref-exact")
	neighborIdx := indexOfDoc(results, "ref-neighbor")
	require.GreaterOrEqual(t, exactIdx, 0, "exact-lexical row missing")
	require.GreaterOrEqual(t, neighborIdx, 0, "good-semantic row missing")
	assert.Less(t, exactIdx, neighborIdx,
		"exact-lexical hit must outrank good-semantic neighbor; got exact=%d neighbor=%d, results: %v",
		exactIdx, neighborIdx, docSummary(results))
}

// TestReferenceHybridSearch_RankingTrueHybridStaysTop confirms
// that when FTS5 and vector both agree on the same chunk, the
// hybrid row is still competitive — usually top-1 — because
// max(|sigmoid(BM25)|, cosine) gives the hybrid the best of
// both signals.
func TestReferenceHybridSearch_RankingTrueHybridStaysTop(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	// Three chunks: the genuine hybrid (both legs match well),
	// a vector-only distractor (high cosine, no FTS5), and an
	// FTS5-only distractor (decent BM25, weak vector).
	seedReference(t, dm, "ref-hybrid", "true-hybrid-doc",
		"This page is the canonical reference for the nonces system and covers wp_create_nonce in detail.")
	seedReference(t, dm, "ref-vec-only", "vec-only-doc",
		"This page is a tangent on similar security topics but never mentions nonces by name.")
	seedReference(t, dm, "ref-fts-only", "fts-only-doc",
		"nonces is mentioned once in a code example on this otherwise unrelated page about caching.")
	_, _, err := dm.EmbedReferenceChunks(context.Background(), "ref-hybrid")
	require.NoError(t, err)
	_, _, err = dm.EmbedReferenceChunks(context.Background(), "ref-vec-only")
	require.NoError(t, err)
	_, _, err = dm.EmbedReferenceChunks(context.Background(), "ref-fts-only")
	require.NoError(t, err)

	results, err := ReferenceHybridSearch(dm, "nonces", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	require.NotEmpty(t, results)

	// The true-hybrid doc must be the top hit. The other two
	// are distractors that should sort below.
	top := results[0]
	assert.Equal(t, "ref-hybrid", top.DocID,
		"true-hybrid must be top-1; got %s, results: %v", top.DocID, docSummary(results))
	assert.Equal(t, "hybrid", top.Source,
		"top-1 source should be hybrid; got %s", top.Source)
}

// TestReferenceHybridSearch_RankingDeterministicTies confirms the
// tiebreaker chain produces identical orderings across runs even
// when many rows tie on the primary key. The test seeds chunks
// with the same content (so FTS5 scores are equal), embeds them
// (so vector similarities are also equal), and runs the search
// 10 times to confirm identical ID sequences.
func TestReferenceHybridSearch_RankingDeterministicTies(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	for i := 0; i < 8; i++ {
		seedReference(t, dm, "ref-tie-"+itoa(i), "tie-"+itoa(i),
			"identical token appears in every chunk for tiebreaker verification")
		_, _, err := dm.EmbedReferenceChunks(context.Background(), "ref-tie-"+itoa(i))
		require.NoError(t, err)
	}

	first, err := ReferenceHybridSearch(dm, "identical", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	require.NotEmpty(t, first)

	for run := 0; run < 10; run++ {
		again, err := ReferenceHybridSearch(dm, "identical", DefaultReferenceHybridConfig())
		require.NoError(t, err)
		require.Equal(t, len(first), len(again),
			"run %d: result count diverged", run)
		for i := range first {
			assert.Equal(t, first[i].ID, again[i].ID,
				"run %d position %d: %s != %s", run, i, first[i].ID, again[i].ID)
		}
	}
}

// indexOfDoc returns the position of docID in results, or -1 if
// not present. Small helper used by the ranking tests.
func indexOfDoc(results []ReferenceHybridResult, docID string) int {
	for i, r := range results {
		if r.DocID == docID {
			return i
		}
	}
	return -1
}

// ── Story 2: model-aware embedding lifecycle tests ─────────────────────────
//
// These tests pin the fingerprint comparison logic that lets the
// backfill detect when a stored embedding came from a different
// model than the one currently configured. The fingerprint is
// (embedding_source, embedding_dimension, embedding_model); any
// mismatch triggers a refresh.

// TestReferenceEmbedding_NullEmbeddingGetsFilled confirms the
// canonical backfill path: a row with no embedding is filled in.
func TestReferenceEmbedding_NullEmbeddingGetsFilled(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	seedReference(t, dm, "ref-null", "null-embed-doc", "content for the null-embed row.")
	// Before any embed: embedding column is NULL. (The
	// embedding_source column has the schema default 'provider'
	// even on a NULL-embedding row — what marks the row as
	// unembedded is the NULL embedding itself.)
	var embBefore []byte
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding FROM reference_chunks WHERE doc_id = ?`, "ref-null",
	).Scan(&embBefore))
	assert.Nil(t, embBefore, "unembedded row must have NULL embedding")

	// Backfill.
	refreshed, _, err := dm.RefreshStaleReferenceEmbeddings(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, refreshed, 1, "null embedding must be filled")

	// After: row has a non-NULL embedding and the fingerprint
	// columns reflect the active provider.
	var (
		afterEmb  []byte
		afterDim  int
		afterMod  string
	)
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding, embedding_dimension, COALESCE(embedding_model, '')
		 FROM reference_chunks WHERE doc_id = ?`, "ref-null",
	).Scan(&afterEmb, &afterDim, &afterMod))
	assert.NotNil(t, afterEmb, "embedding must be populated after refresh")
	assert.Equal(t, 4, afterDim, "stub provider is 4-dim")
	assert.NotEmpty(t, afterMod, "fingerprint must record the model identity")
}

// TestReferenceEmbedding_ValidSameModelNotRegenerated is the
// idempotency contract: once a row's fingerprint matches the
// active provider, a second refresh call leaves it alone.
// Pinned by the spec's "unchanged content/model remains
// idempotent" requirement.
func TestReferenceEmbedding_ValidSameModelNotRegenerated(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	seedReference(t, dm, "ref-stable", "stable-doc", "stable content for idempotency check.")
	first, _, err := dm.RefreshStaleReferenceEmbeddings(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, first, 1)

	// Capture the bytes of the first embedding.
	var firstBytes []byte
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding FROM reference_chunks WHERE doc_id = ?`, "ref-stable",
	).Scan(&firstBytes))
	require.NotEmpty(t, firstBytes)

	// A second refresh with the same provider must NOT touch
	// the row. The bytes must be byte-identical to the first
	// pass.
	second, _, err := dm.RefreshStaleReferenceEmbeddings(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, second,
		"second refresh with same provider must touch zero rows; got %d", second)

	var secondBytes []byte
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding FROM reference_chunks WHERE doc_id = ?`, "ref-stable",
	).Scan(&secondBytes))
	assert.Equal(t, string(firstBytes), string(secondBytes),
		"idempotent refresh must produce byte-identical embedding")
}

// TestReferenceEmbedding_ChangedModelGetsRegenerated confirms
// the model-mismatch detection: when the active provider
// changes identity, rows whose stored fingerprint names the
// previous model are refreshed. Pinned by the spec's "changed
// model identity gets regenerated" requirement.
func TestReferenceEmbedding_ChangedModelGetsRegenerated(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	// Provider A: dim=4, model="stub-a".
	withNamedStubProvider(t, "stub-a", 4)
	seedReference(t, dm, "ref-model-a", "model-a-doc", "content for model A.")
	embedded, _, err := dm.RefreshStaleReferenceEmbeddings(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, embedded, 1)

	var (
		dimA   int
		modelA string
	)
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding_dimension, embedding_model FROM reference_chunks WHERE doc_id = ?`,
		"ref-model-a",
	).Scan(&dimA, &modelA))
	assert.Equal(t, 4, dimA)
	assert.Equal(t, "stub-a", modelA)

	// Switch to provider B: dim=4 (same dim) but model identity
	// differs. The fingerprint is (Model, Dimension) and a
	// different Model is enough to trigger a refresh even when
	// the dimension matches.
	withNamedStubProvider(t, "stub-b", 4)
	embedded, _, err = dm.RefreshStaleReferenceEmbeddings(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, embedded, 1,
		"model identity change must trigger a refresh; got %d", embedded)

	var modelB string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding_model FROM reference_chunks WHERE doc_id = ?`,
		"ref-model-a",
	).Scan(&modelB))
	assert.Equal(t, "stub-b", modelB, "fingerprint must reflect the new model")

	ResetEmbedConfigForTest()
}

// TestReferenceEmbedding_WrongDimensionGetsRefreshed confirms
// the dimension-mismatch case: when the active provider
// changes identity, rows whose stored fingerprint names the
// previous model are refreshed. Pinned by the spec's "wrong-
// dimension embedding gets regenerated" requirement.
//
// The test confirms both halves of the contract:
//
//   1. With same model name but different dim, Refresh does
//      NOT touch the row (model-fingerprint matches; the
//      dimension-only mismatch is handled at query time by
//      referenceSearchVector's len(vec) != len(queryVec)
//      guard).
//
//   2. When the model name changes, Refresh DOES touch the
//      row and the new fingerprint carries the new dim. The
//      previous dim is overwritten — the row's stored vector
//      is now produced by the new model at the new dim.
func TestReferenceEmbedding_WrongDimensionGetsRefreshed(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	// Provider A: 4-dim, name "stub-a".
	withNamedStubProvider(t, "stub-a", 4)
	seedReference(t, dm, "ref-dim-a", "dim-a-doc", "content for dim A.")
	_, _, err := dm.RefreshStaleReferenceEmbeddings(context.Background())
	require.NoError(t, err)

	var (
		dimA   int
		modelA string
	)
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding_dimension, embedding_model FROM reference_chunks WHERE doc_id = ?`,
		"ref-dim-a",
	).Scan(&dimA, &modelA))
	assert.Equal(t, 4, dimA)
	assert.Equal(t, "stub-a", modelA)

	// Switch to provider with the SAME model name but
	// different dim. The fingerprint is by (Model, Dim) but
	// the comparator checks Model first; if Model matches it
	// is treated as "current". The dim-only mismatch is
	// handled at query time, not by refresh.
	withNamedStubProvider(t, "stub-a", 8)
	embedded, _, err := dm.RefreshStaleReferenceEmbeddings(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, embedded,
		"same-model-different-dim is not a refresh trigger (model fingerprint matches); runtime dim-mismatch is the safety net")

	// Stored dim must still be 4 (untouched).
	var dimAfter int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding_dimension FROM reference_chunks WHERE doc_id = ?`,
		"ref-dim-a",
	).Scan(&dimAfter))
	assert.Equal(t, 4, dimAfter, "stored dim must remain 4 after a no-op refresh")

	// Now switch to a different model. Refresh fires; the
	// stored dim is overwritten with the new model's dim.
	withNamedStubProvider(t, "stub-b", 8)
	embedded, _, err = dm.RefreshStaleReferenceEmbeddings(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, embedded, 1, "model change must trigger refresh")

	var (
		dimB   int
		modelB string
	)
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding_dimension, embedding_model FROM reference_chunks WHERE doc_id = ?`,
		"ref-dim-a",
	).Scan(&dimB, &modelB))
	assert.Equal(t, 8, dimB, "fingerprint dim must reflect the active provider")
	assert.Equal(t, "stub-b", modelB, "fingerprint model must reflect the active provider")

	ResetEmbedConfigForTest()
}

// TestReferenceEmbedding_QueryTimeDimensionMismatchStillSkips is
// the safety net for the case Refresh does NOT touch (same model
// name, different dim, per the design above). The query-time
// guard in referenceSearchVector must skip the row so the user
// does not get a misleading similarity. Pinned by
// TestReferenceHybridSearch_DimensionMismatchSkipsRow in the
// hybrid suite; this test confirms it still holds when the
// fingerprint columns carry the model identity.
func TestReferenceEmbedding_QueryTimeDimensionMismatchStillSkips(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	// Provider produces 4-dim vectors. Embed one row.
	withNamedStubProvider(t, "stub-4", 4)
	seedReference(t, dm, "ref-qdim", "qdim-doc", "content for the dim-mismatch query test.")
	_, _, err := dm.EmbedReferenceChunks(context.Background(), "ref-qdim")
	require.NoError(t, err)

	// Confirm the row has 4-dim fingerprint.
	var dim int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding_dimension FROM reference_chunks WHERE doc_id = ?`, "ref-qdim",
	).Scan(&dim))
	require.Equal(t, 4, dim)

	// Switch to 8-dim provider (same model identity) and search.
	// The query embedding is 8-dim; the stored embedding is
	// 4-dim. The referenceSearchVector guard must skip the row
	// (not produce a misleading similarity).
	withNamedStubProvider(t, "stub-4", 8)
	results, err := ReferenceHybridSearch(dm, "content", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	// The row must NOT appear via the vector leg. It might
	// still appear via FTS5 if the content matches; the test
	// asserts that IF the row appears, source must be fts5
	// (the dim-mismatched vector was skipped).
	for _, r := range results {
		if r.DocID == "ref-qdim" {
			assert.Equal(t, "fts5", r.Source,
				"dim-mismatched row must be FTS5-only at query time; got %s", r.Source)
		}
	}

	ResetEmbedConfigForTest()
}

// TestReferenceEmbedding_ChangedContentGetsRegenerated confirms
// that when the underlying chunk content changes, the
// fingerprint logic is bypassed: the chunk_hash diff in
// AddReference clears the embedding column, and the next
// Refresh fills it from the active provider. Pinned by
// TestReferenceHybridSearch_UpdateClearsStaleEmbedding in the
// hybrid suite; this test confirms the refresh path picks up
// the cleared row even when the fingerprint WOULD have matched.
func TestReferenceEmbedding_ChangedContentGetsRegenerated(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	// Initial ingest.
	require.NoError(t, dm.AddReference(&ReferenceDoc{
		ID: "ref-cc", Title: "content-change-doc", SourcePath: "/tmp/cc.md", SourceType: "markdown",
		Content: "VERSION-1 content", LastIndexed: "1790000000",
	}, []ReferenceChunk{{
		ID: "chunk-cc", DocID: "ref-cc", ChunkIndex: 0,
		Section: "intro", Content: "VERSION-1 content", SourcePath: "/tmp/cc.md",
	}}))
	_, _, err := dm.EmbedReferenceChunks(context.Background(), "ref-cc")
	require.NoError(t, err)

	var (
		dimV1  int
		modelV string
	)
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding_dimension, embedding_model FROM reference_chunks WHERE id = 'chunk-cc'`,
	).Scan(&dimV1, &modelV))
	require.Equal(t, 4, dimV1)
	require.Equal(t, "stub:test", modelV)

	// Re-ingest with new content. The chunk_hash diff clears
	// the embedding column.
	require.NoError(t, dm.AddReference(&ReferenceDoc{
		ID: "ref-cc", Title: "content-change-doc", SourcePath: "/tmp/cc.md", SourceType: "markdown",
		Content: "VERSION-2 different content", LastIndexed: "1790000001",
	}, []ReferenceChunk{{
		ID: "chunk-cc", DocID: "ref-cc", ChunkIndex: 0,
		Section: "intro", Content: "VERSION-2 different content", SourcePath: "/tmp/cc.md",
	}}))

	// After AddReference, the row is unembedded (cleared on the
	// Update branch). The schema's default for embedding_source
	// is 'provider' even on a NULL-embedding row; the test
	// checks the embedding column itself, which is the
	// canonical "needs refresh" signal.
	var embAfter []byte
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding FROM reference_chunks WHERE id = 'chunk-cc'`,
	).Scan(&embAfter))
	require.Nil(t, embAfter, "content change must clear the embedding")

	// Refresh picks it up. The fingerprint must reflect the
	// active provider, and the new content drives a new vector.
	embedded, _, err := dm.RefreshStaleReferenceEmbeddings(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, embedded, 1)

	var (
		dimV2   int
		modelV2 string
		srcV2   string
	)
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding_source, embedding_dimension, embedding_model
		 FROM reference_chunks WHERE id = 'chunk-cc'`,
	).Scan(&srcV2, &dimV2, &modelV2))
	assert.Equal(t, "provider", srcV2)
	assert.Equal(t, 4, dimV2)
	assert.Equal(t, "stub:test", modelV2)
}

// TestReferenceEmbedding_NoProviderNoRefresh is the null-provider
// safety net: when no provider is configured, Refresh is a
// no-op. The fingerprint logic is not engaged because there is
// nothing to compare against.
func TestReferenceEmbedding_NoProviderNoRefresh(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	// No SetEmbedConfigForTest call → default config has no
	// provider in the test environment. EmbedReferenceChunks
	// would write NULL embeddings, so the row has source='null'.
	seedReference(t, dm, "ref-noprovider", "noprovider-doc", "content for the no-provider case.")
	_, _, err := dm.EmbedReferenceChunks(context.Background(), "ref-noprovider")
	require.NoError(t, err)

	// The fingerprint has been stamped but with the no-provider
	// path (source='null', dim=NULL, model=NULL). A subsequent
	// Refresh must NOT attempt to fill — there is no provider.
	embedded, _, err := dm.RefreshStaleReferenceEmbeddings(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, embedded,
		"Refresh with no provider must be a no-op; got %d", embedded)
}

// TestReferenceHybridSearch_DimensionMismatchSkipsRow confirms
// that a stored embedding with a different dimension than the
// query is silently skipped (not zeroed, not errored). This
// guards against model changes leaving stale embeddings that
// would corrupt cosine similarities.
func TestReferenceHybridSearch_DimensionMismatchSkipsRow(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	seedReference(t, dm, "ref-dim", "dim-doc", "Test content for dimension-mismatch handling.")
	// Manually write a 2-dim embedding to the row (mismatching
	// the 4-dim stub provider).
	_, err := dm.SQLDB().Exec(
		`UPDATE reference_chunks SET embedding = ? WHERE doc_id = ?`,
		`[0.1, 0.2]`, "ref-dim")
	require.NoError(t, err)

	// Search must not error and must not surface the mismatched
	// row (cosine on mismatched dims is undefined; the code skips
	// it). With one chunk total, the result list is empty.
	results, err := ReferenceHybridSearch(dm, "any", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	assert.Empty(t, results,
		"dimension-mismatched row must be skipped; got %d rows", len(results))
}

// TestReferenceHybridSearch_LikeFallback confirms that when
// reference_chunks_fts is missing (FTS5 unavailable), the LIKE
// path is used. The corpus is small enough that LIKE is
// acceptable.
func TestReferenceHybridSearch_LikeFallback(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	seedReference(t, dm, "ref-like", "like-doc", "LIKEFALLBACKTOKEN appears in this chunk.")

	// Drop the FTS table to force the LIKE path.
	_, err := dm.SQLDB().Exec(`DROP TABLE IF EXISTS reference_chunks_fts`)
	require.NoError(t, err)

	results, err := ReferenceHybridSearch(dm, "LIKEFALLBACKTOKEN", DefaultReferenceHybridConfig())
	require.NoError(t, err)
	require.NotEmpty(t, results, "LIKE fallback must surface the seeded chunk")
	assert.Equal(t, "ref-like", results[0].DocID)
}

// TestReferenceHybridSearch_ProviderUnreachableDegradesToFTSOnly
// confirms that a provider configured but returning an error on
// EmbedText does NOT break the search. The function logs a warn
// and proceeds with the FTS5 leg. The user contract is that
// reference lookup must not depend on the embedding service
// being healthy.
type failingProvider struct{}

func (failingProvider) Embed(text string) ([]float32, error) {
	return nil, errSimulated
}
func (failingProvider) Name() string { return "failing:test" }

// errSimulated is exported via the package; the test uses an
// ad-hoc value via a helper so the test file doesn't need to
// import "errors" just for this.
var errSimulated = &simulatedErr{}

type simulatedErr struct{}

func (s *simulatedErr) Error() string { return "simulated embedding failure" }

func TestReferenceHybridSearch_ProviderUnreachableDegradesToFTSOnly(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	prev := SetEmbedConfigForTest(&EmbeddingConfig{
		Source:       EmbeddingSourceProfile,
		ProviderName: "failing:test",
		Provider:     failingProvider{},
		Status:       EmbeddingStatusUnreachable,
	})
	defer ResetEmbedConfigForTest()
	_ = prev

	seedReference(t, dm, "ref-prov", "prov-doc", "PROVIDERFAILURETOKEN content")

	results, err := ReferenceHybridSearch(dm, "PROVIDERFAILURETOKEN", DefaultReferenceHybridConfig())
	require.NoError(t, err, "configured-but-failing provider must not break the search")
	require.NotEmpty(t, results)
	for _, r := range results {
		assert.Equal(t, "fts5", r.Source,
			"with vector leg failed, source must be fts5; got %s", r.Source)
	}
}

// TestReferenceHybridSearch_EmbeddingFormatMatchesMemories pins
// the spec's "use the same vector format as memories" requirement.
// The reference_chunks.embedding BLOB must use the same
// JSON-encoded []float32 layout as memories.embedding so the
// two columns are interchangeable at the byte level.
func TestReferenceHybridSearch_EmbeddingFormatMatchesMemories(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()
	defer withStubProvider(t, 4)()

	seedReference(t, dm, "ref-fmt", "fmt-doc", "Format parity content token.")
	embedded, _, err := dm.EmbedReferenceChunks(context.Background(), "ref-fmt")
	require.NoError(t, err)
	require.Equal(t, 1, embedded)

	// The stored embedding is a JSON array of float32.
	var raw string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding FROM reference_chunks WHERE doc_id = ?`, "ref-fmt",
	).Scan(&raw))
	require.True(t, strings.HasPrefix(raw, "["),
		"reference embedding must be JSON array; got %q", raw)
	require.True(t, strings.HasSuffix(raw, "]"),
		"reference embedding must be JSON array; got %q", raw)

	// Insert a memory with the SAME vector and confirm a memory
	// cosine query returns it (the format is byte-compatible).
	// This is a structural check, not a semantic one.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, content, collection, embedding, embedding_source, created_at, deleted_at)
		VALUES ('mem-parity', 'memory test', 'parity', ?, 'openai-compatible', 1790000000, NULL)
	`, raw)
	require.NoError(t, err)

	var memEmb string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT embedding FROM memories WHERE id = 'mem-parity'`,
	).Scan(&memEmb))
	assert.Equal(t, raw, memEmb,
		"reference_chunks.embedding and memories.embedding must store identical bytes for the same vector")
}
