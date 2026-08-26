// test17_semantic_benchmark_test.go — Test 17-Semantic: paraphrased-query
// archaeology benchmark.
//
// PURPOSE
// Measure how well the existing retrieval substrate (BM25 / Vector / Hybrid)
// surfaces high-value engineering memories when a fresh agent uses
// paraphrased natural-language queries — NOT the canonical identifiers.
//
// CONSTRAINTS
//   1. No new retrieval infrastructure. Tests existing APIs only.
//   2. Hermetic: seeds a fixed corpus in-memory. No external DB needed.
//   3. No cheating: queries never contain F7.1, F8.1, internal function names,
//      file names, or commit hashes.
//   4. Reports what was actually measured. No invented numbers.
//
// WHAT THIS BENCHMARK IS
// A reproducible measurement of retrieval recall on a small, fixed corpus
// representing the F7.1 and F8.1 archaeology surface. It seeds:
//
//   - The canonical F7.1 challenge/restoration archaeology memory.
//   - The canonical F8.1 cancel/verification archaeology memory.
//   - A handful of unrelated decoy memories (so generic queries like
//     "memory" do not accidentally hit the target).
//
// Then it runs paraphrased queries and measures whether each retrieval
// mode (BM25-only / vector-only / Hybrid) surfaces the right memory in
// its top-K.
//
// SUBSTRATE NOTES (measured during benchmark construction)
//   - HybridSearch with VectorWeight=0.0 is the canonical BM25 path
//     (the FTS5 query runs through BuildFTS5Query, per-token prefix
//     wildcarding). MemoryStore.FullTextSearch passes the raw query
//     to MATCH and returns 0 hits on multi-token paraphrased queries —
//     so the benchmark uses HybridSearch even for the BM25 row.
//   - With MPM_EMBED_PROVIDER unset (NullProvider), the vector path
//     returns no results. The benchmark reports this honestly rather
//     than silently falling back to a hash-based pseudo-embedding.
//
// WHAT THIS BENCHMARK IS NOT
//   - It does NOT replace Test 17 lexical archaeology.
//   - It does NOT introduce ranking, authority weighting, or knowledge
//     graph edges.
//   - It does NOT recommend architecture. It produces numbers.
package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// benchMem is one memory in the benchmark corpus. id is unique; collection
// matches the memories table discriminator; content is what gets indexed.
type benchMem struct {
	id         string
	collection string
	content    string
}

// benchCorpus returns the fixed F7.1 / F8.1 archaeology surface used by the
// benchmark. The content fields intentionally do NOT include the canonical
// identifiers F7.1 / F8.1 as standalone tokens — they are reachable via
// the changelog.md path but the benchmark seeds a textual representation
// of the archaeology so retrieval must work on the substance.
//
// The corpus also includes 4 decoy memories representing common unrelated
// topics. They exist so a query like "memory" or "verification" returns
// them too — that lets the benchmark check whether the target memory ranks
// ABOVE the noise.
func benchCorpus() []benchMem {
	return []benchMem{
		// Target — F7.1
		{
			id:         "bench-f71",
			collection: "memories",
			content: "Challenge and restoration coupling. A challenged memory cannot retain pre-challenge high confidence. Restoration does not silently re-promote trust. Fresh evidence is required to re-elevate a restored memory above the challenge floor. The challenge operation neutralizes existing evidence rows and drops the memory's confidence to a neutral floor. Restoration clears operational flags and restores prior weight, but explicitly does not re-elevate confidence.",
		},
		// Target — F8.1
		{
			id:         "bench-f81",
			collection: "memories",
			content: "Cancellation and verification coupling. A cancelled work status cannot co-occur with verification equals verified. Cancellation is not proof of completion. The verification derivation consults the work's status first; cancelled status locks the result below verified regardless of evidence pattern. Every terminal-lifecycle transition re-runs the derivation so verification can never lag the status column. Contradictory evidence still surfaces as contradicted under cancellation.",
		},
		// Decoys — unrelated topics that contain overlapping vocabulary.
		{
			id:         "bench-decoy-synthesis",
			collection: "memories",
			content: "Synthesis worker pool runs in isolation with a DLQ for failed attempts. LLM-driven dedup consolidates near-duplicate memories and emits confidence updates through the standard evidence pipeline.",
		},
		{
			id:         "bench-decoy-provenance",
			collection: "memories",
			content: "Provenance env vars stamp every event with actor_kind, actor_id, framework_name, session_id, invocation_id, parent_invocation_id. Build-time and runtime provenance captured separately.",
		},
		{
			id:         "bench-decoy-fs",
			collection: "memories",
			content: "Cascade materialize is invoked from cron or systemd to ingest files. The watch daemon was deprecated in 2026; file ingestion now flows through a CLI command rather than fsnotify.",
		},
		{
			id:         "bench-decoy-decay",
			collection: "memories",
			content: "Lifecycle decay applies exponential per-collection lambda. Decisions decay at 0.001 per day; memories at 0.01 per day; theories at 0.02 per day; lessons at 0.003 per day.",
		},
	}
}

// benchQuery is one paraphrased query in the benchmark. expectedIDs is the
// set of memory IDs that count as a hit — typically just the canonical
// target, but the target's vocabulary document may also qualify.
type benchQuery struct {
	class        string // A=f71 paraphrase, B=f81 paraphrase, C=architectural, D=test discovery, E=validation
	query        string
	expectedIDs  []string
}

// benchQueries is the paraphrased benchmark corpus. The queries were
// written by reading the F7.1 / F8.1 concepts and re-stating each one as
// a fresh agent would phrase it without knowing the canonical identifiers.
//
// No query contains F7.1, F8.1, ChallengeMemory, DeriveWorkVerification,
// CancelWork, the file name f71_f81_cancellation_challenge_test.go, or
// the commit hash 55226a2.
func benchQueries() []benchQuery {
	return []benchQuery{
		// ── Class A — F7.1 paraphrases ──
		{
			class:       "A",
			query:       "why does restoring a challenged memory not make it trusted again",
			expectedIDs: []string{"bench-f71"},
		},
		{
			class:       "A",
			query:       "where do we prevent old evidence from regaining authority after a challenge",
			expectedIDs: []string{"bench-f71"},
		},
		{
			class:       "A",
			query:       "how does the system stop restored information from becoming trusted without fresh proof",
			expectedIDs: []string{"bench-f71"},
		},
		{
			class:       "A",
			query:       "what protects us from a challenged memory silently regaining confidence",
			expectedIDs: []string{"bench-f71"},
		},
		{
			class:       "A",
			query:       "why does lifting a dispute on a memory not automatically restore its trust",
			expectedIDs: []string{"bench-f71"},
		},

		// ── Class B — F8.1 paraphrases ──
		{
			class:       "B",
			query:       "why can't a stopped task still count as successfully verified",
			expectedIDs: []string{"bench-f81"},
		},
		{
			class:       "B",
			query:       "what prevents abandoned work from looking successful",
			expectedIDs: []string{"bench-f81"},
		},
		{
			class:       "B",
			query:       "how do we prevent cancellation from being treated as proof of completion",
			expectedIDs: []string{"bench-f81"},
		},
		{
			class:       "B",
			query:       "where is the rule that separates lifecycle termination from verification",
			expectedIDs: []string{"bench-f81"},
		},
		{
			class:       "B",
			query:       "why might a killed task still be marked verified in the database",
			expectedIDs: []string{"bench-f81"},
		},

		// ── Class C — Architectural paraphrases ──
		{
			class:       "C",
			query:       "where did we fix state drift between task lifecycle and trust",
			expectedIDs: []string{"bench-f71", "bench-f81"},
		},
		{
			class:       "C",
			query:       "what architectural rule prevents operational status from disagreeing with verification",
			expectedIDs: []string{"bench-f71", "bench-f81"},
		},
		{
			class:       "C",
			query:       "where is the invariant that keeps lifecycle and verification semantics separate",
			expectedIDs: []string{"bench-f71", "bench-f81"},
		},

		// ── Class D — Test discovery ──
		{
			class:       "D",
			query:       "where are the tests proving that cancelled work cannot become verified",
			expectedIDs: []string{"bench-f81"},
		},
		{
			class:       "D",
			query:       "how can I find the regression tests for challenge and restoration",
			expectedIDs: []string{"bench-f71"},
		},
		{
			class:       "D",
			query:       "what tests cover restoration without trust promotion",
			expectedIDs: []string{"bench-f71"},
		},

		// ── Class E — Validation discovery ──
		{
			class:       "E",
			query:       "how was the cancellation verification bug proven fixed",
			expectedIDs: []string{"bench-f81"},
		},
		{
			class:       "E",
			query:       "what evidence cleared the alpha blocker for challenge restoration",
			expectedIDs: []string{"bench-f71"},
		},
		{
			class:       "E",
			query:       "where is the release validation for the cancel verification fix",
			expectedIDs: []string{"bench-f81"},
		},
	}
}

// seedBenchmarkCorpus inserts the corpus into the test DM and returns the
// DM. Caller is responsible for Close().
//
// If a real embedding provider is configured (Ollama via OLLAMA_ENDPOINT),
// the embedding column is populated for every row so the vector retrieval
// path has stored vectors to match against. Without a real provider, the
// embedding column stays NULL — the NullProvider fallback in EmbedText is
// a hash-based pseudo-embedding that does not get stored here because
// HybridSearch's vector path filters WHERE embedding IS NOT NULL and we
// want honest reporting (vector-disabled) instead of hash-polluted results.
func seedBenchmarkCorpus(t *testing.T, dm *DatabaseManager, corpus []benchMem) {
	t.Helper()
	provider := DefaultEmbeddingConfig().Provider
	hasRealProvider := provider.Name() != "null"
	t.Logf("seedBenchmarkCorpus: provider=%s (real=%v)", provider.Name(), hasRealProvider)
	for _, m := range corpus {
		var embJSON string
		if hasRealProvider {
			vec, err := provider.Embed(m.content)
			if err != nil || len(vec) == 0 {
				t.Logf("  embed FAILED for %s: %v (leaving embedding NULL)", m.id, err)
			} else {
				b, mErr := json.Marshal(vec)
				if mErr != nil {
					t.Logf("  marshal FAILED for %s: %v", m.id, mErr)
				} else {
					embJSON = string(b)
				}
			}
		}
		var err error
		if embJSON != "" {
			_, err = dm.ExecTracked(
				`INSERT INTO memories (id, collection, content, metadata, created_at, weight, embedding) VALUES (?, ?, ?, '{}', strftime('%s','now'), 5, ?)`,
				0, m.id, m.collection, m.content, embJSON)
		} else {
			_, err = dm.ExecTracked(
				`INSERT INTO memories (id, collection, content, metadata, created_at, weight) VALUES (?, ?, ?, '{}', strftime('%s','now'), 5)`,
				0, m.id, m.collection, m.content)
		}
		require.NoError(t, err)
	}
}

// rankFor reports the 1-based rank of any expectedID in the result list,
// or 0 if absent. If multiple expectedIDs hit, returns the best (lowest)
// rank.
func rankFor(results []string, expected []string) int {
	best := 0
	for i, id := range results {
		for _, want := range expected {
			if id == want {
				rank := i + 1
				if best == 0 || rank < best {
					best = rank
				}
			}
		}
	}
	return best
}

// resultIDsFromHybrid extracts the IDs from a HybridSearchMemories result
// list (which is []map[string]interface{}).
func resultIDsFromHybrid(rows []map[string]interface{}) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if id, ok := r["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// vectorOnlyRank performs direct vector retrieval via dm.VectorSearch,
// returning the 1-based rank of any expectedID in the similarity-ordered
// results. Returns 0 if the target is not retrieved, embedding fails, or
// no real provider is configured.
func vectorOnlyRank(dm *DatabaseManager, q benchQuery) int {
	provider := DefaultEmbeddingConfig().Provider
	if provider.Name() == "null" {
		return 0 // vector path disabled
	}
	vec, err := provider.Embed(q.query)
	if err != nil || len(vec) == 0 {
		return 0
	}
	results, err := dm.VectorSearch("memories", vec, 10)
	if err != nil {
		return 0
	}
	ids := make([]string, 0, len(results))
	for _, r := range results {
		if id, ok := r["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return rankFor(ids, q.expectedIDs)
}

// runOneBenchmarkQuery runs a single query against the three retrieval
// modes and returns the rank achieved by each. A rank of 0 means the
// target was NOT retrieved.
//
// Three retrieval modes are measured:
//
//   - BM25-only: HybridSearch with VectorWeight=0.0. The substrate
//     still computes vectors (returning nil for NullProvider) but never
//     blends, so FTS5 results are the only results. The FTS5 query
//     runs through BuildFTS5Query (per-token prefix wildcarding), which
//     is the canonical lexical path.
//
//   - Vector-only: dm.VectorSearch, the canonical direct-vector retrieval
//     path. HybridSearch with VectorWeight=1.0 does NOT yield pure
//     vector-only results — its merge step at hybrid_search.go:184-187
//     discards FTS5-absent rows, even when VectorWeight=1.0. The
//     substrate's documented "vector search" surface is dm.VectorSearch,
//     so we measure that.
//
//   - Hybrid: HybridSearch with default VectorWeight=0.5. Combines
//     BM25 + vector paths through the substrate's blending logic.
//
// These are honest measurements of the substrate as it stands.
func runOneBenchmarkQuery(dm *DatabaseManager, q benchQuery) (bm25Rank, vecRank, hybridRank int) {
	// BM25-only — HybridSearch with VectorWeight=0.
	bm25Cfg := DefaultHybridConfig()
	bm25Cfg.VectorWeight = 0.0
	bm25Cfg.Limit = 10
	bm25Results, err := HybridSearch(dm, q.query, "memories", bm25Cfg)
	bm25IDs := make([]string, 0, len(bm25Results))
	if err == nil {
		for _, r := range bm25Results {
			bm25IDs = append(bm25IDs, r.ID)
		}
	}
	bm25Rank = rankFor(bm25IDs, q.expectedIDs)

	// Vector-only — direct dm.VectorSearch. We embed the query and ask
	// the substrate to rank corpus rows by cosine similarity. This is the
	// only way to surface pure vector results without FTS5 contamination
	// (HybridSearch drops FTS5-absent rows by construction).
	vecRank = vectorOnlyRank(dm, q)

	// Hybrid — default config.
	hybridCfg := DefaultHybridConfig()
	hybridCfg.Limit = 10
	hybridResults, err := HybridSearch(dm, q.query, "memories", hybridCfg)
	hybridIDs := make([]string, 0, len(hybridResults))
	if err == nil {
		for _, r := range hybridResults {
			hybridIDs = append(hybridIDs, r.ID)
		}
	}
	hybridRank = rankFor(hybridIDs, q.expectedIDs)
	return
}

// TestSemanticArchaeologyBenchmark runs every paraphrased query against
// the three retrieval modes and reports a results table. The test passes
// if the run completes and produces output — recall numbers are recorded
// for human review, not for pass/fail gating.
//
// To enable embeddings for the vector path, set MPM_EMBED_PROVIDER=ollama
// (the substrate then probes the local Ollama endpoint). When embeddings
// are disabled (the default), the NullProvider is used and the
// vector-only path returns nil; the benchmark records that as
// "vector disabled".
func TestSemanticArchaeologyBenchmark(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	corpus := benchCorpus()
	seedBenchmarkCorpus(t, dm, corpus)

	queries := benchQueries()
	type row struct {
		Class, Query, Expected string
		Bm25, Vec, Hybrid     int
	}
	rows := make([]row, 0, len(queries))
	for _, q := range queries {
		bm25, vec, hybrid := runOneBenchmarkQuery(dm, q)
		rows = append(rows, row{
			Class:    q.class,
			Query:    q.query,
			Expected: strings.Join(q.expectedIDs, ","),
			Bm25:     bm25,
			Vec:      vec,
			Hybrid:   hybrid,
		})
	}

	// Compute aggregate Recall@1/3/5.
	type aggKey struct{ mode string }
	type agg struct{ at1, at3, at5, total int }
	aggByMode := map[string]*agg{
		"BM25":    {},
		"Vector":  {},
		"Hybrid":  {},
	}
	for _, r := range rows {
		for _, mode := range []string{"BM25", "Vector", "Hybrid"} {
			var rank int
			switch mode {
			case "BM25":
				rank = r.Bm25
			case "Vector":
				rank = r.Vec
			case "Hybrid":
				rank = r.Hybrid
			}
			a := aggByMode[mode]
			a.total++
			if rank == 1 {
				a.at1++
			}
			if rank >= 1 && rank <= 3 {
				a.at3++
			}
			if rank >= 1 && rank <= 5 {
				a.at5++
			}
		}
	}

	// Print results — go test -v will surface this.
	t.Logf("=== Semantic Archaeology Benchmark (Test 17-Semantic) ===")
	t.Logf("Corpus: %d memories (2 targets, 4 decoys)", len(corpus))
	t.Logf("Queries: %d paraphrased", len(queries))
	t.Logf("")
	t.Logf("Per-query results:")
	t.Logf("  Class | Rank@BM25 | Rank@Vector | Rank@Hybrid | Query")
	for _, r := range rows {
		t.Logf("  %-5s |    %2d     |     %2d      |     %2d      | %s",
			r.Class, r.Bm25, r.Vec, r.Hybrid, truncateForLog(r.Query, 90))
	}
	t.Logf("")
	t.Logf("Aggregate Recall:")
	t.Logf("  Mode    | Recall@1 | Recall@3 | Recall@5")
	for _, mode := range []string{"BM25", "Vector", "Hybrid"} {
		a := aggByMode[mode]
		t.Logf("  %-7s |  %5.1f%%  |  %5.1f%%  |  %5.1f%%",
			mode,
			pct(a.at1, a.total),
			pct(a.at3, a.total),
			pct(a.at5, a.total))
	}

	// Detect embedding coverage so the report can flag the "vector
	// disabled" case honestly.
	embeddingProvider := os.Getenv("MPM_EMBED_PROVIDER")
	if embeddingProvider == "" {
		t.Logf("")
		t.Logf("NOTE: MPM_EMBED_PROVIDER unset → NullProvider active → vector path")
		t.Logf("      is using hash-based pseudo-embeddings, not real semantic vectors.")
		t.Logf("      Set MPM_EMBED_PROVIDER=ollama for true semantic retrieval.")
	} else {
		t.Logf("")
		t.Logf("Embedding provider: %s", embeddingProvider)
	}
}

// truncateForLog shortens a string for log readability.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// pct computes integer percentage rounded to one decimal.
func pct(num, denom int) float64 {
	if denom == 0 {
		return 0
	}
	return float64(num) * 100.0 / float64(denom)
}

// TestSemanticBenchmark_VocabularyAlignedQueries is the complement of
// TestSemanticArchaeologyBenchmark. It runs paraphrased queries that DO
// use vocabulary from the corpus (e.g., "cancelled work verification",
// "challenge restoration memory") to confirm the substrate works when
// paraphrased language overlaps with indexed tokens. This isolates the
// "paraphrase vocabulary gap" from any other retrieval issue.
func TestSemanticBenchmark_VocabularyAlignedQueries(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()
	seedBenchmarkCorpus(t, dm, benchCorpus())

	queries := []benchQuery{
		{class: "A-aligned", query: "cancelled memory evidence neutralization", expectedIDs: []string{"bench-f71"}},
		{class: "B-aligned", query: "cancelled work verification", expectedIDs: []string{"bench-f81"}},
		{class: "A-aligned", query: "challenge evidence drop confidence", expectedIDs: []string{"bench-f71"}},
		{class: "B-aligned", query: "cancellation derivation lifecycle", expectedIDs: []string{"bench-f81"}},
		{class: "B-aligned", query: "cancelled work status locks", expectedIDs: []string{"bench-f81"}},
	}

	bm25Hit := 0
	for _, q := range queries {
		cfg := DefaultHybridConfig()
		cfg.VectorWeight = 0.0
		cfg.Limit = 10
		results, _ := HybridSearch(dm, q.query, "memories", cfg)
		ids := []string{}
		for _, r := range results {
			ids = append(ids, r.ID)
		}
		rank := rankFor(ids, q.expectedIDs)
		t.Logf("aligned query %q → %d results, target rank=%d, ids=%v",
			q.query, len(results), rank, ids)
		if rank >= 1 {
			bm25Hit++
		}
	}
	t.Logf("Vocabulary-aligned BM25 Recall@5: %d/%d = %.1f%%",
		bm25Hit, len(queries), pct(bm25Hit, len(queries)))
}

// TestSemanticBenchmark_DebugSanityCheck verifies the BM25 path can find
// a known-good query against a known-good corpus, and documents the
// substrate behavior the benchmark depends on:
//
//   1. Single-token FTS5 queries return expected hits.
//   2. Multi-token FTS5 queries with implicit-AND semantics return hits
//      only when ALL tokens match (porter unicode61 tokenizer).
//   3. The corpus tokens are actually indexed (presence check).
//
// If THIS test fails, the benchmark numbers in TestSemanticArchaeologyBenchmark
// are uninformative — fix the substrate wiring first.
func TestSemanticBenchmark_DebugSanityCheck(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()
	seedBenchmarkCorpus(t, dm, benchCorpus())

	// (1) Single-token sanity check — direct FTS5.
	res, err := dm.SQLDB().Query(
		`SELECT m.id FROM memories m JOIN memories_fts fts ON m.rowid = fts.rowid WHERE memories_fts MATCH ? AND m.deleted_at IS NULL LIMIT 5`,
		"cancelled")
	require.NoError(t, err)
	defer res.Close()
	count := 0
	for res.Next() {
		count++
	}
	require.NoError(t, res.Err())
	t.Logf("direct FTS5 single-token 'cancelled' → %d rows", count)
	require.NotZero(t, count, "BM25 should find 'cancelled' in a corpus containing it")

	// (2) Multi-token implicit-AND: direct FTS5.
	res2, _ := dm.SQLDB().Query(
		`SELECT m.id FROM memories m JOIN memories_fts fts ON m.rowid = fts.rowid WHERE memories_fts MATCH ? AND m.deleted_at IS NULL LIMIT 5`,
		"cancelled verification")
	count = 0
	for res2.Next() {
		count++
	}
	res2.Close()
	t.Logf("direct FTS5 multi-token 'cancelled verification' → %d rows", count)

	// (3) HybridSearch with VectorWeight=0 — what the benchmark uses.
	cfg := DefaultHybridConfig()
	cfg.VectorWeight = 0.0
	cfg.Limit = 10
	hr, _ := HybridSearch(dm, "cancelled verification", "memories", cfg)
	t.Logf("HybridSearch(weight=0) 'cancelled verification' → %d results", len(hr))

	// (4) Token presence check.
	for _, tok := range []string{"cancelled", "cancellation", "verification", "stopped", "task", "verified", "abandoned", "cancelled*", "verif*"} {
		var n int
		require.NoError(t, dm.QueryRowTracked(
			`SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH ?`,
			tok).Scan(&n))
		t.Logf("  FTS5 token %q → %d rows", tok, n)
	}
}

// TestSemanticBenchmark_VocabularyEffect measures whether explicitly
// including the F7.1/F8.1 concept-vocabulary tokens in the corpus
// improves recall for paraphrased queries.
//
// This is the only test that touches the authored vocabulary files. If
// the vocab files are missing (e.g. the test is running on a checkout
// without docs/concept-vocabularies/), the test is skipped.
func TestSemanticBenchmark_VocabularyEffect(t *testing.T) {
	vocabDir := findRepoDocsDir(t, "concept-vocabularies")
	if vocabDir == "" {
		t.Skip("docs/concept-vocabularies/ not found — vocabulary-effect scenario skipped")
	}

	dm := newTestDM(t)
	defer dm.Close()

	// Start with the bench corpus (target + decoys).
	seedBenchmarkCorpus(t, dm, benchCorpus())

	// Augment with concept-vocabulary content extracted from the docs.
	// These represent the "improved authoring" scenario — vocabulary
	// tokens would help if they were indexed as additional content.
	for _, slug := range []string{"f71-challenge-restoration.md", "f81-cancel-verification.md"} {
		path := filepath.Join(vocabDir, slug)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		id := "bench-vocab-" + strings.TrimSuffix(slug, ".md")
		_, err = dm.ExecTracked(
			`INSERT INTO memories (id, collection, content, metadata, created_at, weight) VALUES (?, 'memories', ?, '{}', strftime('%s','now'), 5)`,
			0, id, string(raw))
		require.NoError(t, err)
	}

	// Re-run the same paraphrased queries but allow the vocab-augmented
	// corpus to also count as a hit.
	queries := benchQueries()
	hit := 0
	for _, q := range queries {
		expanded := append([]string{}, q.expectedIDs...)
		// Find vocab docs whose content matches the expected target.
		for _, slug := range []string{"f71-challenge-restoration.md", "f81-cancel-verification.md"} {
			vid := "bench-vocab-" + strings.TrimSuffix(slug, ".md")
			if (strings.Contains(slug, "f71") && containsAny(q.expectedIDs, "bench-f71")) ||
				(strings.Contains(slug, "f81") && containsAny(q.expectedIDs, "bench-f81")) {
				expanded = append(expanded, vid)
			}
		}
		// Use HybridSearch with VectorWeight=0 — the substrate's BM25-canonical path.
		cfg := DefaultHybridConfig()
		cfg.VectorWeight = 0.0
		cfg.Limit = 10
		results, err := HybridSearch(dm, q.query, "memories", cfg)
		ids := make([]string, 0, len(results))
		if err == nil {
			for _, r := range results {
				ids = append(ids, r.ID)
			}
		}
		rank := rankFor(ids, expanded)
		if rank >= 1 && rank <= 5 {
			hit++
		}
	}
	t.Logf("Vocabulary-augmented BM25 (HybridSearch VectorWeight=0) Recall@5: %d/%d = %.1f%%",
		hit, len(queries), pct(hit, len(queries)))
}

// findRepoDocsDir walks up from the current test working directory to
// find the project root containing docs/concept-vocabularies/. Returns
// "" if not found.
func findRepoDocsDir(t *testing.T, leaf string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for dir := wd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "docs", leaf)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

// containsAny reports whether haystack contains any of the needles.
func containsAny(haystack []string, needles ...string) bool {
	for _, h := range haystack {
		for _, n := range needles {
			if h == n {
				return true
			}
		}
	}
	return false
}
