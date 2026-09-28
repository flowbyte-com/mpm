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
//
// CombinedScore and RelevanceScore are intentionally distinct:
//
//   - CombinedScore is the legacy weighted blend
//     (sigmoid(BM25) * (1 - VectorWeight) + cosine * VectorWeight).
//     It is a diagnostic — useful for showing "what would a
//     weighted blend look like?" — but it is NOT used for the
//     final sort.
//
//   - RelevanceScore is the comparable relevance in [0, 1] that
//     the final sort actually uses. See relevanceScore() for the
//     exact formula. Splitting the two makes the ranking intent
//     explicit and lets the JSON renderer expose both.
type ReferenceHybridResult struct {
	ID               string  // chunk id
	DocID            string  // parent doc id
	ChunkIndex       int     // position within the doc
	Section          string  // chunk section heading (may be empty)
	Content          string  // chunk body
	DocTitle         string  // parent doc title (joined for display)
	FTS5Score        float64 // raw BM25 (more negative = better), 0 if no FTS5 hit
	VectorSimilarity float64 // cosine in [0, 1], 0 if no vector hit
	CombinedScore    float64 // diagnostic: weighted blend, NOT used for sort
	RelevanceScore   float64 // the comparable relevance used for the final sort
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

	// Step 4: relevance-based sort. The previous implementation
	// used a source-priority bucket (hybrid > fts5 > vector)
	// before comparing within-source scores. That created a real
	// defect: a weak-hybrid row (low FTS5 + low vector) would
	// outrank a strong-vector row (zero FTS5, high cosine) purely
	// because the source label said "hybrid". The user's stated
	// principle is "actual relevance should dominate source
	// label", so the sort key is now a comparable RelevanceScore
	// in [0, 1].
	//
	// Tiebreakers, in order:
	//   1. Stronger FTS5 leg (|sigmoid(BM25)|). This preserves
	//      exact-symbol dominance: a chunk that has the literal
	//      query token outranks a chunk that is merely a close
	//      semantic neighbor, even when both end up with similar
	//      RelevanceScore. Pinned by
	//      Ranking_StrongLexicalBeatsGoodSemantic.
	//   2. Stronger vector leg. When FTS5 strengths tie too, the
	//      chunk that is semantically closer wins.
	//   3. Chunk ID ascending. Deterministic absolute tiebreak;
	//      no map-iteration-order leakage. Pinned by the
	//      Determinism test.
	sort.SliceStable(results, func(i, j int) bool {
		ri, rj := results[i].RelevanceScore, results[j].RelevanceScore
		if ri != rj {
			return ri > rj
		}
		// Tiebreaker 1: stronger FTS5 leg wins (preserves
		// exact-symbol dominance on relevance ties).
		fi := ftsStrength(results[i].FTS5Score)
		fj := ftsStrength(results[j].FTS5Score)
		if fi != fj {
			return fi > fj
		}
		// Tiebreaker 2: stronger vector leg wins.
		if results[i].VectorSimilarity != results[j].VectorSimilarity {
			return results[i].VectorSimilarity > results[j].VectorSimilarity
		}
		// Tiebreaker 3: chunk ID for absolute determinism.
		return results[i].ID < results[j].ID
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
// (chunk content) and references_fts (doc title + content + tags),
// merging the two result sets with deduplication by chunk_id.
//
// The two indexes serve different roles:
//
//   - reference_chunks_fts covers chunk content (section, content).
//     This is the primary retrieval surface and was the only one
//     used before 2026-09-28.
//
//   - references_fts covers the doc-level metadata: title, full
//     content (denormalized for legacy reasons), and the tags
//     JSON array. Tag matches are how an agent retrieves a doc
//     whose body has no exact token overlap with the query —
//     e.g. "edit_theme_options" matches the
//     wp-roles-capabilities.md tags column even though the body
//     has no exact token of that name.
//
// The doc-level hits are mapped to their top-ranked chunk (one
// chunk per matched doc) so they appear in the chunk-level
// result list. Doc-level BM25 scores are scaled by a small
// factor (less than 1.0) so chunk-content matches — which are
// the more specific signal — outrank tag-only matches when both
// exist. The scaling is documented inline; the constant
// (0.7) is a deliberately conservative starting point that
// keeps "exact technical-symbol queries that have body
// matches" anchored on the body matches, while letting
// tag-only matches surface for queries that would otherwise
// return zero rows.
//
// CTE pattern: bm25() is only in scope inside the FTS5 virtual
// table query, so each score subquery wraps the FTS5 match.
// The reference_chunks_ai trigger (db.go:2936) populates
// reference_chunks_fts.rowid = new.rowid from reference_chunks'
// implicit rowid; the references_ai trigger (db.go:2932) does
// the same for reference_docs. Both joins use rowid.
func referenceSearchFTS5(db *sql.DB, query string, limit int) ([]referenceFTSToMerge, error) {
	ftsQuery := BuildFTS5Query(query)
	if ftsQuery == "" {
		return nil, nil
	}

	// docLevelBaseBoost is the flat negative offset added to
	// every doc-level tag MATCH score. The MATCH expression
	// is column-qualified to the tags column of references_fts
	// so the boost only applies to docs whose frontmatter tags
	// include the query tokens. That restriction is essential:
	// without it, body token matches inside the doc would
	// inherit the boost and the boost would over-apply to
	// unrelated docs that happen to mention the words.
	//
	// The boost reflects the principle that a tag match is a
	// strong, specific signal for exact-symbol queries — a
	// doc whose frontmatter tags include "edit_theme_options"
	// is, by construction, the canonical doc for that
	// capability, even if its body has no exact-token overlap.
	// The -8.0 floor makes a tag match competitive with any
	// body match the corpus produces (empirically body matches
	// on the test queries top out around BM25 = -6 to -7).
	//
	// For natural-language queries (no exact token in tags),
	// the tag MATCH returns zero rows, so the boost has no
	// effect — body matches remain authoritative there.
	const docLevelBaseBoost = -8.0

	// tagMatch restricts the doc-level search to the tags
	// column of references_fts. SQLite FTS5 column-qualified
	// MATCH syntax is "column:term"; we prefix the ftsQuery
	// (which is "edit* theme* options*"-style) with "tags:" so
	// only docs whose tags include any query token fire.
	tagMatch := "tags:" + ftsQuery

	rows, err := db.Query(`
		WITH chunk_scores AS (
			SELECT rowid, bm25(reference_chunks_fts) AS s
			FROM reference_chunks_fts
			WHERE reference_chunks_fts MATCH ?
		),
		doc_scores AS (
			SELECT rowid, bm25(references_fts) + ? AS s
			FROM references_fts
			WHERE references_fts MATCH ?
		),
		doc_chunk_ranked AS (
			-- Pick the first chunk of each matched doc. The
			-- bm25() FTS5 function is not callable from a
			-- regular SQL expression (it is only in scope
			-- inside an FTS5 MATCH query), so we cannot use
			-- it as the rank() ORDER BY. The choice of which
			-- chunk to surface is therefore deterministic on
			-- chunk_index ASC. The actual doc-level score
			-- (ds.s) is the SAME for all rows of the same doc;
			-- what changes between rows is which chunk is
			-- shown to the user. Picking chunk_index=0
			-- (the first chunk of the doc) is the predictable
			-- choice and matches what an agent would scroll
			-- to first.
			SELECT rc.id AS chunk_id, rc.doc_id, rc.chunk_index,
			       rc.section, rc.content, rd.title,
			       ds.s AS doc_score,
			       rank() OVER (PARTITION BY rc.doc_id ORDER BY rc.chunk_index ASC) AS rk
			FROM doc_scores ds
			JOIN reference_docs rd ON rd.rowid = ds.rowid
			JOIN reference_chunks rc ON rc.doc_id = rd.id
		)
		SELECT id, doc_id, chunk_index, section, content, title, s
		FROM (
			-- Doc-level (tag match) FIRST. When the same chunk_id
			-- appears in both legs, the map-based merge in Go
			-- keeps the LAST entry seen; ordering the doc-level
			-- branch first means its more-negative score survives
			-- the overwrite when the chunk is also a chunk-content
			-- match. Without this, the chunk-content BM25
			-- (typically -3 to -7 on multi-token body matches)
			-- would clobber the doc-level score with boost
			-- (~-12 for tag matches), and the tag match would
			-- not get the relevance boost the user expects.
			SELECT chunk_id AS id, doc_id, chunk_index, section, content,
			       title, doc_score AS s
			FROM doc_chunk_ranked
			WHERE rk = 1
			UNION ALL
			SELECT rc.id, rc.doc_id, rc.chunk_index, rc.section, rc.content,
			       rd.title, cs.s
			FROM chunk_scores cs
			JOIN reference_chunks rc ON rc.rowid = cs.rowid
			JOIN reference_docs rd ON rc.doc_id = rd.id
		)
		ORDER BY s
		LIMIT ?
	`, ftsQuery, docLevelBaseBoost, tagMatch, limit)
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
	// Skip hash-source rows: those are legacy 256-dim placeholder
	// embeddings from a fallback path that was never used for
	// references (the references corpus is too new to have any),
	// but the filter mirrors memories' safety net so a future
	// fallback that writes hash embeddings cannot corrupt the
	// semantic ranking. See memories.embedding_source = 'hash' in
	// internal/core/hybrid_search.go:390 for the same convention.
	rows, err := db.Query(`
		SELECT id, doc_id, chunk_index, section, content, embedding
		FROM reference_chunks
		WHERE embedding IS NOT NULL
		  AND (embedding_source IS NULL OR embedding_source != 'hash')
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
		// When the same chunk appears in both the chunk-content
		// CTE and the doc-level CTE, the MIN BM25 is the more
		// relevant signal. Doc-level hits include the -8.0
		// tag-match boost and are typically more negative than
		// chunk-content BM25; taking the MIN keeps the strongest
		// evidence. Equality (same chunk in both legs, same
		// score) leaves the first-seen value in the map.
		if existing, ok := ftsByID[h.ID]; ok && existing.Score < h.Score {
			continue
		}
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
			RelevanceScore:   relevanceScore(ftsScore, vecSim),
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
//
// This score is the diagnostic CombinedScore on ReferenceHybridResult
// — useful for "what would a weighted blend look like?" reporting.
// It is NOT the final sort key. The sort uses RelevanceScore
// (see relevanceScore) so a high-vector row is not buried by the
// source-label bucket the legacy implementation used.
func referenceHybridScore(ftsScore float64, vecSim float64, vecWeight float64) float64 {
	normFTS := ftsScore / (1 + math.Abs(ftsScore))
	return (1-vecWeight)*normFTS + vecWeight*vecSim
}

// ftsStrength returns |sigmoid(BM25)| in [0, 1]. Used as the
// RelevanceScore tiebreaker so a row with strong lexical evidence
// outranks a row with comparable but purely-semantic evidence. The
// absolute value matters (not the signed sigmoid) because the
// sort compares strengths, not signed scores.
//
// Always returns 0 when ftsScore is 0 (no FTS5 hit at all).
func ftsStrength(ftsScore float64) float64 {
	if ftsScore == 0 {
		return 0
	}
	s := ftsScore / (1 + math.Abs(ftsScore))
	if s < 0 {
		return -s
	}
	return s
}

// strongLexicalThreshold is the BM25 below which a row counts as
// having a "strong lexical match". Used by relevanceScore to give
// exact-symbol queries a small advantage on relevance ties. The
// threshold is deliberately conservative: -5 corresponds to "two
// or more tokens matched well", which is what most technical-
// symbol queries reach when the chunk is the right answer.
const strongLexicalThreshold = -5.0

// relevanceScore is the final sort key. Range is roughly [0, 1].
//
// Formula:
//
//	fts = |sigmoid(ftsScore)|     // 0..1
//	vec = cosine                  // 0..1
//	score = max(fts, vec) + 0.05 * (fts <= strongLexicalThreshold)
//
// Why max-of-legs, not weighted-blend: a weighted blend buries a
// chunk that has only one strong leg. For example, an FTS5-only
// chunk with BM25=-15 has CombinedScore = -0.47, but a vector-only
// chunk with cosine=0.7 has CombinedScore = 0.35. The vector-only
// chunk would outrank the exact-symbol hit despite the exact-symbol
// hit being the more specific match. The max formulation fixes
// this without a source-priority bucket: a strong-FTS5 row has
// RelevanceScore near 0.94; a strong-vector row has RelevanceScore
// near 0.7. The exact-symbol row wins.
//
// The 0.05 lexical boost is a tiebreaker nudge, not a primary
// signal. On relevance ties (two rows with the same max-of-legs
// score), a row with strong lexical evidence gets a small
// preference — matching the user's principle that "exact
// technical-symbol matches must still retain a strong advantage
// where justified".
func relevanceScore(ftsScore, vecSim float64) float64 {
	fts := ftsStrength(ftsScore)
	score := math.Max(fts, vecSim)
	if ftsScore <= strongLexicalThreshold {
		score += 0.05
	}
	// Clamp to [0, 1] so the score is comparable across rows.
	// The boost can push score slightly above 1 in degenerate
	// cases (vec=1.0 AND strong FTS5); clamping keeps the
	// diagnostic field bounded and avoids surprising consumers
	// that read it as a probability.
	if score > 1.0 {
		score = 1.0
	}
	return score
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
