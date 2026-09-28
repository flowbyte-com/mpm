// reference_search.go — hybrid lexical + semantic retrieval for
// reference_chunks.
//
// Mirrors the design of memory HybridSearch (internal/core/
// hybrid_search.go) but targets the reference library. The substrate
// already has every primitive this needs:
//
//   - BuildFTS5Query (internal/core/fts5_query.go) — the canonical
//     FTS5 MATCH expression builder. Tokenizes on underscores and
//     hyphens the same way the porter unicode61 indexer does, so
//     queries like "wp_kses_post" split into "wp* kses* post*" instead
//     of crashing on a hyphen-as-column-name parser error or returning
//     zero results for a literal phrase the index never stored.
//
//   - EmbedText (internal/core/embeddings.go) — the canonical embed
//     entry point. Returns (nil, nil) for NullProvider/Disabled/
//     Absent; returns (nil, err) only when the provider is configured
//     but unreachable. Both branches are handled below.
//
//   - cosineSimilarity (internal/core/db.go:3890) — the canonical
//     []float32 → float32 cosine primitive, identical signature to
//     the memory path.
//
//   - reference_chunks.embedding (BLOB, JSON-encoded []float32) —
//     shares its serialization format with memories.embedding via the
//     shared embeddingBytes helper (internal/core/reference_db.go:13),
//     so the two columns are interchangeable at the byte level.
//
// The embedding column is nullable: NULL means "not yet embedded".
// ReferenceHybridSearch tolerates partial embedding — vector-only
// candidates (chunks that have embeddings but no FTS5 hit) surface in
// the result list as Source="vector", and FTS5-only candidates
// surface as Source="fts5". Both branches survive a NullProvider
// configuration, which is the user-stated requirement: reference
// lookup must NEVER be dependent on an external embedding service
// being healthy.
//
// Ranking algorithm: weighted score fusion using the same sigmoid
// BM25 + linear blend that memory hybrid uses (hybridScore, this
// file's referenceHybridScore). Reciprocal Rank Fusion (RRF) was
// considered and rejected:
//
//   1. The existing memory substrate already uses weighted fusion; a
//      different fusion for references would silently diverge for
//      agents that retrieve from both surfaces.
//   2. The corpus is small (33 docs, ~250 chunks). RRF's main win —
//      resilience to score-scale mismatch — does not bite here
//      because the sigmoid normalization in hybridScore already
//      projects BM25 into [0, 1].
//   3. Maintainability: a single fusion function across the
//      substrate keeps the operator mental model uniform.
//
// Source diversity: applyPerDocCap demotes excess chunks from the
// same doc so a single large reference does not dominate the top
// of a result list. This is a simple post-merge pass — one count
// per doc, demoted rows still appear later in the list.

package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
)

// ReferenceHybridConfig controls how the lexical and semantic legs
// of reference retrieval are blended. Defaults via
// DefaultReferenceHybridConfig().
type ReferenceHybridConfig struct {
	// VectorWeight ∈ [0.0, 1.0]. 0.0 = pure FTS5, 1.0 = pure vector.
	// Default 0.5 = equal weight. The same convention memory
	// HybridConfig uses.
	VectorWeight float64

	// Limit: maximum results to return. Default 20.
	Limit int

	// PerDocCap: max chunks per doc in the result list. 0 disables
	// the cap. Default 3 — keeps a single large reference from
	// dominating the top of the ranking.
	PerDocCap int
}

// DefaultReferenceHybridConfig returns the canonical defaults.
func DefaultReferenceHybridConfig() ReferenceHybridConfig {
	return ReferenceHybridConfig{
		VectorWeight: 0.5,
		Limit:        20,
		PerDocCap:    3,
	}
}

// ReferenceHybridResult is one chunk hit with combined lexical and
// semantic scores. Field semantics match memory HybridResult so
// consumers can use the same rendering code.
type ReferenceHybridResult struct {
	ID               string  // chunk id
	DocID            string  // parent doc id
	ChunkIndex       int     // position within the doc
	Section          string  // chunk section heading (may be empty)
	Content          string  // chunk body
	DocTitle         string  // parent doc title (joined for display)
	FTS5Score        float64 // raw BM25 (more negative = better), 0 if no FTS5 hit
	VectorSimilarity float64 // cosine in [0, 1], 0 if no vector hit
	CombinedScore    float64 // weighted blend
	Source           string  // "fts5" | "vector" | "hybrid"
}

// ReferenceHybridSearch runs hybrid lexical + semantic retrieval over
// reference_chunks. Falls back gracefully when the embedding provider
// is absent/disabled/unreachable — the FTS5 leg always runs and
// unembedded chunks participate as FTS5-only candidates.
//
// The corpus is small enough for a brute-force vector scan (no IVF
// clustering yet). When the corpus grows past a few thousand chunks
// the memory IVFSearch path becomes relevant; for now the simple scan
// is correct, fast, and avoids an entire class of cluster-rebalance
// concerns.
func ReferenceHybridSearch(dm *DatabaseManager, query string, cfg ReferenceHybridConfig) ([]ReferenceHybridResult, error) {
	if dm == nil || dm.SQLDB() == nil {
		return nil, fmt.Errorf("ReferenceHybridSearch: database not initialized")
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if cfg.VectorWeight < 0 || cfg.VectorWeight > 1 {
		cfg.VectorWeight = 0.5
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 20
	}
	if cfg.PerDocCap < 0 {
		cfg.PerDocCap = 0
	}

	// Step 1: lexical leg. Always runs.
	ftsResults, err := referenceSearchFTS5(dm.SQLDB(), query, cfg.Limit*2)
	if err != nil {
		// FTS5 is unavailable (no reference_chunks_fts table) — fall
		// back to LIKE. The reference corpus is small enough that LIKE
		// is acceptable; the canonical substrate path keeps the search
		// callable when an FTS5 table is missing.
		ftsResults, err = referenceSearchLike(dm.SQLDB(), query, cfg.Limit*2)
		if err != nil {
			return nil, fmt.Errorf("reference hybrid: FTS5/LIKE failed: %w", err)
		}
	}

	// Step 2: semantic leg. Runs only when a non-null embedding
	// provider is configured. EmbedText returns (nil, nil) for the
	// NullProvider/Disabled/Absent cases — the user-stated constraint
	// is that reference lookup must not depend on an external
	// embedding service being healthy, so a missing provider is a
	// silent no-op, not an error.
	var vecResults []referenceVecHit
	provider := DefaultEmbeddingConfig().Provider
	if provider.Name() != "null" {
		queryVec, err := provider.Embed(query)
		if err != nil {
			// Configured but unreachable: log and degrade. The FTS5
			// leg still produced results; the search completes with
			// lexical-only candidates rather than failing the call.
			slog.Warn("ReferenceHybridSearch: EmbedText failed, falling back to FTS-only",
				"err", err, "provider", provider.Name())
		} else if len(queryVec) > 0 {
			vecResults, err = referenceSearchVector(dm.SQLDB(), queryVec, cfg.Limit*2)
			if err != nil {
				slog.Warn("ReferenceHybridSearch: vector scan failed, falling back to FTS-only",
					"err", err)
			}
		}
	}

	// Step 3: merge.
	results := mergeReferenceResults(dm.SQLDB(), ftsResults, vecResults, cfg.VectorWeight)

	// Step 4: natural sort. Source-priority first (hybrid > fts5
	// > vector), then the within-source signal (FTS5 BM25
	// ascending = better; vector similarity descending = better;
	// combined descending = better). Stable so equal-score
	// chunks preserve their merge-time order, which is
	// deterministic (sorted chunk IDs) per the Determinism
	// contract.
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Source != results[j].Source {
			return sourceRank(results[i].Source) > sourceRank(results[j].Source)
		}
		switch results[i].Source {
		case "fts5":
			return results[i].FTS5Score < results[j].FTS5Score
		case "vector":
			return results[i].VectorSimilarity > results[j].VectorSimilarity
		case "hybrid":
			return results[i].CombinedScore > results[j].CombinedScore
		}
		return false
	})

	// Step 5: source-diversity cap. Runs AFTER the natural sort
	// so the partition walks the actually-ordered list. The
	// first N chunks per doc keep their position; subsequent
	// chunks for the same doc are moved to the tail, preserving
	// their natural-sort order within the tail. PerDocCap=0
	// disables the cap.
	if cfg.PerDocCap > 0 {
		applyPerDocCap(results, cfg.PerDocCap)
	}

	if len(results) > cfg.Limit {
		results = results[:cfg.Limit]
	}
	return results, nil
}

// referenceSearchFTS5 runs FTS5 MATCH against reference_chunks_fts
// using the canonical BuildFTS5Query builder. The previous path
// (web_db.go:1188) wrapped the query in `"..."*` which forced a
// literal-phrase match; that pattern returns zero hits for any
// identifier containing underscores (e.g. wp_kses_post) because the
// porter unicode61 tokenizer splits those into separate tokens at
// index time. Switching to BuildFTS5Query tokenizes the query the
// same way the indexer did, with a `*` prefix per token — the FTS5
// implicit AND then matches any chunk containing any of the tokens.
// This is a correctness fix, not a behavior change for queries that
// already worked.
func referenceSearchFTS5(db *sql.DB, query string, limit int) ([]referenceFTSToMerge, error) {
	ftsQuery := BuildFTS5Query(query)
	if ftsQuery == "" {
		return nil, nil
	}
	// CTE pattern: bm25() is only in scope inside the FTS5 virtual
	// table query, so the score subquery wraps the FTS5 match. The
	// reference_chunks_ai trigger (db.go:2936) populates
	// reference_chunks_fts.rowid = new.rowid from reference_chunks'
	// implicit rowid, so the join must use rowid, NOT the TEXT id
	// column. Mirrors the proven pattern from SearchReferenceChunks
	// (web_db.go:1188).
	rows, err := db.Query(`
		WITH scores AS (
			SELECT rowid, bm25(reference_chunks_fts) AS s
			FROM reference_chunks_fts
			WHERE reference_chunks_fts MATCH ?
		)
		SELECT rc.id, rc.doc_id, rc.chunk_index, rc.section, rc.content,
		       rd.title, scores.s
		FROM reference_chunks rc
		JOIN scores ON rc.rowid = scores.rowid
		JOIN reference_docs rd ON rc.doc_id = rd.id
		ORDER BY scores.s
		LIMIT ?
	`, ftsQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []referenceFTSToMerge
	for rows.Next() {
		var hit referenceFTSToMerge
		if err := rows.Scan(&hit.ID, &hit.DocID, &hit.ChunkIndex, &hit.Section,
			&hit.Content, &hit.DocTitle, &hit.Score); err != nil {
			return nil, fmt.Errorf("referenceSearchFTS5: scan: %w", err)
		}
		out = append(out, hit)
	}
	return out, rows.Err()
}

// referenceSearchLike is the FTS5 fallback using LIKE. Used when
// reference_chunks_fts is missing (corpus before migration, or
// migration that failed mid-flight). LIKE is slow on large
// corpora; the corpus today is small enough that this is
// acceptable.
func referenceSearchLike(db *sql.DB, query string, limit int) ([]referenceFTSToMerge, error) {
	pat := "%" + strings.ReplaceAll(query, "%", "\\%") + "%"
	rows, err := db.Query(`
		SELECT rc.id, rc.doc_id, rc.chunk_index, rc.section, rc.content, rd.title, 0.0
		FROM reference_chunks rc
		JOIN reference_docs rd ON rc.doc_id = rd.id
		WHERE rc.content LIKE ?
		ORDER BY rc.chunk_index
		LIMIT ?
	`, pat, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []referenceFTSToMerge
	for rows.Next() {
		var hit referenceFTSToMerge
		if err := rows.Scan(&hit.ID, &hit.DocID, &hit.ChunkIndex, &hit.Section,
			&hit.Content, &hit.DocTitle, &hit.Score); err != nil {
			return nil, fmt.Errorf("referenceSearchLike: scan: %w", err)
		}
		out = append(out, hit)
	}
	return out, rows.Err()
}

// referenceSearchVector brute-force scans reference_chunks.embedding
// and returns the top-N by cosine similarity. Corpus is small
// (~250 chunks today) so an IVF index is not yet justified; the
// memory VectorMatch path has the same threshold before activating
// IVFSearch (vector.max_scan / MPM_MAX_VECTOR_SCAN).
//
// The embedding BLOB is JSON-encoded []float32 — same format as
// memories.embedding — so the unmarshal step is byte-compatible
// with the memory vector search.
func referenceSearchVector(db *sql.DB, queryVec []float32, limit int) ([]referenceVecHit, error) {
	rows, err := db.Query(`
		SELECT id, doc_id, chunk_index, section, content, embedding
		FROM reference_chunks
		WHERE embedding IS NOT NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []referenceVecHit
	for rows.Next() {
		var (
			id, docID, content string
			chunkIndex         int
			section            sql.NullString
			embRaw             []byte
		)
		// Scan the embedding BLOB directly into []byte. The
		// previous code scanned into sql.NullString, which works
		// in some driver versions but can silently produce an
		// empty string on others — leading to all rows being
		// skipped and the vector leg returning nothing. The
		// []byte scan is the canonical way to read a BLOB column.
		if err := rows.Scan(&id, &docID, &chunkIndex, &section, &content, &embRaw); err != nil {
			return nil, fmt.Errorf("referenceSearchVector: scan: %w", err)
		}
		if len(embRaw) == 0 {
			continue
		}
		var vec []float32
		if err := json.Unmarshal(embRaw, &vec); err != nil {
			// Corrupt blob — skip. Log so an operator can find
			// it if it becomes systemic.
			slog.Warn("referenceSearchVector: unmarshal failed, skipping row",
				"id", id, "err", err)
			continue
		}
		if len(vec) != len(queryVec) {
			// Dimension mismatch — the chunk was embedded by a
			// different model than the one currently configured.
			// Skip rather than producing a misleading similarity
			// score; the operator can re-embed with the current
			// model. This is the most common cause of "vector
			// similarity = 0 for every row" in practice.
			continue
		}
		sim := cosineSimilarity(queryVec, vec)
		out = append(out, referenceVecHit{
			ID:         id,
			DocID:      docID,
			ChunkIndex: chunkIndex,
			Section:    section.String,
			Content:    content,
			Similarity: float64(sim),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Similarity > out[j].Similarity
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, rows.Err()
}

// referenceFTSToMerge is the internal projection used by the merge
// step. Distinct from ReferenceHybridResult so the FTS5 scan
// doesn't need to fabricate zero-valued vector fields.
type referenceFTSToMerge struct {
	ID         string
	DocID      string
	ChunkIndex int
	Section    string
	Content    string
	DocTitle   string
	Score      float64
}

// referenceVecHit is the internal projection of a single vector
// match. DocTitle is filled in by the merge step (batched lookup)
// so the vector scan doesn't need to JOIN.
type referenceVecHit struct {
	ID         string
	DocID      string
	ChunkIndex int
	Section    string
	Content    string
	Similarity float64
}

// mergeReferenceResults fuses FTS5 + vector candidates by chunk ID.
// Union semantics: any chunk that appears in either leg is in the
// merged set. Source is "hybrid" when both legs agree, "fts5" when
// only the lexical leg matched, "vector" when only the semantic
// leg matched. FTS5-score normalization (sigmoid) + linear blend
// produces CombinedScore in roughly [0, 1].
//
// DocTitle is filled in for vector-only candidates via a single
// batched lookup so the merge step is one query, not N.
func mergeReferenceResults(db *sql.DB, fts []referenceFTSToMerge, vec []referenceVecHit, vecWeight float64) []ReferenceHybridResult {
	ftsByID := make(map[string]referenceFTSToMerge, len(fts))
	for _, h := range fts {
		ftsByID[h.ID] = h
	}
	vecByID := make(map[string]referenceVecHit, len(vec))
	for _, h := range vec {
		vecByID[h.ID] = h
	}
	all := make(map[string]bool, len(fts)+len(vec))
	for id := range ftsByID {
		all[id] = true
	}
	for id := range vecByID {
		all[id] = true
	}
	// Determinism: collect IDs into a sorted slice so the merge
	// loop visits chunks in a stable order. Without this, Go's
	// randomized map iteration order makes the final result list
	// non-deterministic for chunks that share the same source
	// category and same score. Pinned by the Determinism test.
	allIDs := make([]string, 0, len(all))
	for id := range all {
		allIDs = append(allIDs, id)
	}
	sort.Strings(allIDs)

	// DocTitle is missing for vector-only candidates. Batch-fetch
	// it in a single query so the merge doesn't regress to N+1.
	titles := batchFetchReferenceDocTitles(db, ftsByID, vecByID)

	out := make([]ReferenceHybridResult, 0, len(all))
	for _, id := range allIDs {
		ftsi, ftsOK := ftsByID[id]
		veci, vecOK := vecByID[id]
		var (
			docID, section, content, docTitle string
			chunkIndex                        int
			ftsScore, vecSim, combined        float64
			source                            string
		)
		switch {
		case ftsOK && vecOK:
			docID = ftsi.DocID
			chunkIndex = ftsi.ChunkIndex
			section = ftsi.Section
			content = ftsi.Content
			docTitle = ftsi.DocTitle
			ftsScore = ftsi.Score
			vecSim = veci.Similarity
			combined = referenceHybridScore(ftsScore, vecSim, vecWeight)
			source = "hybrid"
		case ftsOK:
			docID = ftsi.DocID
			chunkIndex = ftsi.ChunkIndex
			section = ftsi.Section
			content = ftsi.Content
			docTitle = ftsi.DocTitle
			ftsScore = ftsi.Score
			// FTS5-only: combined score is the FTS5 component of
			// the hybrid blend. This makes a pure-lexical hit
			// rank-comparable to a hybrid hit.
			combined = ftsScore / (1+math.Abs(ftsScore)) * (1 - vecWeight)
			source = "fts5"
		default: // vecOK
			docID = veci.DocID
			chunkIndex = veci.ChunkIndex
			section = veci.Section
			content = veci.Content
			docTitle = titles[veci.DocID]
			vecSim = veci.Similarity
			// Vector-only: weighted by VectorWeight so a pure
			// semantic hit scales to the same range as a hybrid
			// hit.
			combined = vecSim * vecWeight
			source = "vector"
		}
		out = append(out, ReferenceHybridResult{
			ID:               id,
			DocID:            docID,
			ChunkIndex:       chunkIndex,
			Section:          section,
			Content:          content,
			DocTitle:         docTitle,
			FTS5Score:        ftsScore,
			VectorSimilarity: vecSim,
			CombinedScore:    combined,
			Source:           source,
		})
	}
	return out
}

// batchFetchReferenceDocTitles returns a map[docID]title covering
// every doc referenced by the FTS or vector candidates. FTS hits
// already carry the title, so the lookup is only needed for
// vector-only doc IDs.
//
// db may be nil — in that case the FTS-derived titles are still
// returned (vector-only candidates fall through with empty title
// strings, which the renderer already tolerates). The nil path
// is exercised by the test suite when it constructs a synthetic
// search result without a real connection.
func batchFetchReferenceDocTitles(db *sql.DB, ftsByID map[string]referenceFTSToMerge, vecByID map[string]referenceVecHit) map[string]string {
	known := make(map[string]string)
	for _, h := range ftsByID {
		if h.DocTitle != "" {
			known[h.DocID] = h.DocTitle
		}
	}
	if db == nil {
		return known
	}
	missing := make(map[string]bool)
	for _, h := range vecByID {
		if _, ok := known[h.DocID]; !ok {
			missing[h.DocID] = true
		}
	}
	if len(missing) == 0 {
		return known
	}
	ids := make([]string, 0, len(missing))
	for id := range missing {
		ids = append(ids, id)
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	// NoSQL injection vector: doc IDs are server-generated
	// (GenerateID) and never user input, but the prepared-statement
	// shape is preserved regardless.
	rows, err := db.Query(
		`SELECT id, title FROM reference_docs WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		slog.Warn("batchFetchReferenceDocTitles: query failed", "err", err)
		return known
	}
	defer rows.Close()
	for rows.Next() {
		var docID, title string
		if err := rows.Scan(&docID, &title); err != nil {
			slog.Warn("batchFetchReferenceDocTitles: scan failed", "err", err)
			continue
		}
		known[docID] = title
	}
	return known
}

// referenceHybridScore blends FTS5 BM25 and vector cosine similarity.
// FTS5 scores are unbounded (negative is better); normalize to
// [0, 1] via the same sigmoid (`x / (1 + |x|)`) that memory
// hybridScore uses. Vector cosine is already in [0, 1].
// Combined = (1 - vecWeight) * normFTS + vecWeight * vecSim.
//
// Re-exported as a package-level function so a future caller (e.g.
// a CLI flag controlling blend weight) can recompute the score
// without re-running the search.
func referenceHybridScore(ftsScore float64, vecSim float64, vecWeight float64) float64 {
	normFTS := ftsScore / (1 + math.Abs(ftsScore))
	return (1-vecWeight)*normFTS + vecWeight*vecSim
}

// applyPerDocCap partitions the result list so the first `cap`
// chunks per doc retain their position, and any subsequent
// chunks for the same doc are moved to the tail. The natural
// sort is preserved within each partition. The partition is
// stable: overflow rows that were earlier in the natural sort
// stay earlier in the overflow tail (so a "stronger" overflow
// row still beats a "weaker" one).
//
// Why partition rather than score-penalty demotion: BM25 scores
// are negative; halving a negative CombinedScore makes it
// LESS negative, which is a boost, not a demotion. A partition
// is sign-agnostic and is what the spec actually wants — the
// top of the result list should not be monopolised by one doc,
// but overflow rows are still served, just later.
func applyPerDocCap(results []ReferenceHybridResult, cap int) {
	if cap <= 0 || len(results) == 0 {
		return
	}
	counts := make(map[string]int, len(results))
	keep := make([]ReferenceHybridResult, 0, len(results))
	overflow := make([]ReferenceHybridResult, 0, len(results))
	for _, r := range results {
		c := counts[r.DocID]
		if c < cap {
			counts[r.DocID] = c + 1
			keep = append(keep, r)
		} else {
			overflow = append(overflow, r)
		}
	}
	copy(results, keep)
	copy(results[len(keep):], overflow)
}

// SearchReferenceChunksHybrid is the public-facing adapter. It runs
// ReferenceHybridSearch and reshapes the result into the
// []map[string]interface{} the existing CLI / MCP consumers expect,
// plus the new hybrid-aware fields (combined_score, vector_similarity,
// source). Existing fields (id, doc_id, chunk_index, section, content,
// doc_title) are preserved so the CLI renderer doesn't need to
// change. New fields are additive — downstream consumers that don't
// look for them are unaffected.
func (dm *DatabaseManager) SearchReferenceChunksHybrid(query string, limit int) ([]map[string]interface{}, error) {
	cfg := DefaultReferenceHybridConfig()
	if limit > 0 {
		cfg.Limit = limit
	}
	results, err := ReferenceHybridSearch(dm, query, cfg)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(results))
	for _, r := range results {
		out = append(out, map[string]interface{}{
			"id":               r.ID,
			"doc_id":           r.DocID,
			"doc_title":        r.DocTitle,
			"chunk_index":      r.ChunkIndex,
			"section":          r.Section,
			"content":          r.Content,
			"fts5_score":       r.FTS5Score,
			"vector_similarity": r.VectorSimilarity,
			"combined_score":   r.CombinedScore,
			"source":           r.Source,
		})
	}
	return out, nil
}
