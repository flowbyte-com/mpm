package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const challengeWarning = "[Note: This memory is challenged — treat as unverified]"
const conceptDriftWarning = "[SYSTEM WARNING: This knowledge is under active Concept Drift investigation — treat as potentially obsolete]"

// HybridConfig controls how FTS5 and vector scores are blended.
type HybridConfig struct {
	// VectorWeight (0.0–1.0): how much to weight vector similarity.
	// 0.0 = pure FTS5,1.0 = pure vector.
	// Default0.5 = equal weight.
	VectorWeight float64

	// RetrievalThreshold: minimum combined score to return a result.
	// Lower = more results, stricter = fewer, higher-quality results.
	RetrievalThreshold float64

	// Limit: maximum results to return.
	Limit int

	// SchemaPrefix (since 2026-07-06): empty string targets the local
	// 'memories'/'memories_fts' tables; "shared." targets the shared-DB
	// equivalents via ATTACH. Set Origin at the same time so downstream
	// consumers can render results distinctly.
	//
	// Used by Phase 2d of docs/library/shared-epistemology.md (Multi-Agent Shared Epistemology)
	// to fuse local and shared-memory recall into one ranked stream.
	// threading the prefix through searchFTS5/searchLike/VectorMatch
	// keeps the search primitives schema-agnostic so the same code path
	// runs against both ATTACHed databases.
	SchemaPrefix string

	// Origin: "local" (default) or "shared". Stamped onto every
	// HybridResult the search returns. Distinct from Source below —
	// Source describes the SEARCH METHOD that produced the row
	// (fts5/vector/hybrid); Origin describes the DATA ORIGIN.
	// Two different concerns; overloading them is a silent-regression
	// trap (see WISHLIST Phase 2d for the rationale).
	Origin string
}

// safeSchemaPrefixPattern validates SQL-safe schema prefixes.
var safeSchemaPrefixPattern = regexp.MustCompile(`^$|^[a-zA-Z_][a-zA-Z0-9_]*\.$`)

// ValidateSchemaPrefix returns an error if the prefix contains unsafe chars.
func ValidateSchemaPrefix(prefix string) error {
	if !safeSchemaPrefixPattern.MatchString(prefix) {
		return fmt.Errorf("invalid schema prefix: %q must match %s", prefix, safeSchemaPrefixPattern.String())
	}
	return nil
}

// DefaultHybridConfig returns sensible defaults.
//
// RetrievalThreshold stays at -3.0 despite that value being unreachable
// for vector-only rows (see the threshold comment in HybridSearch). It was
// measured rather than assumed — see the calibration below.
//
// CALIBRATION (Phase A.3 corpus v2: 50 synthetic documents, 40 answerable
// + 20 unanswerable queries, nomic-embed-text 768-dim, top-10):
//
//	positive-query MRR@10                                0.9182
//	positive-query Recall@10                             0.9875
//	unanswerable queries still served vector candidates   20/20
//
// The two populations OVERLAP. Every unanswerable query still receives
// vector candidates under the current default, and no similarity floor
// separates them without destroying legitimate answerable queries
// (paraphrase queries among them):
//
//	cosine floor 0.70   rejects 18/20 negatives   but 10/40 positives
//	best gap policy     rejects 19/20 negatives   but 14/40 positives
//
// (60-query policy sweep across the calibration and held-out splits;
// bench/output/phase-a3/v2-nomic-eval/, tabulated in
// docs/ISSUE-15-PHASE-A-REPORT.md §A2.4. The earlier 22-query /
// all-minilm measurement reached the same conclusion and is recorded in
// docs/CLOSEOUT.md §4.1.)
//
// WHY THE THRESHOLD AND GAP POLICIES WERE REJECTED (Issue #15, 2026-10-21):
//
//   - A cosine floor of 0.70 removes 18 of 20 nonsense results but also
//     removes 10 of 40 legitimate answerable queries. Those 10 are not
//     marginal hits: 3 of the 10 are the corpus's explicitly labelled
//     paraphrase queries, and the other 7 are legitimate answerable
//     queries that carry no paraphrase label. The floor trades a
//     confidence-display problem for a recall problem, which is the
//     worse failure.
//   - The best gap-based policy (top1–top2 cosine gap < 0.05 OR top-1
//     cosine < 0.60) removes ONE more negative (19/20) while removing
//     14/40 positives (2 of the 14 are labelled paraphrase queries). It
//     is strictly worse on the axis that matters.
//   - The held-out split shows the same legitimate-answer losses, so
//     neither policy is merely overfit to the calibration split.
//
// The 9 labelled paraphrase queries are NOT a low-similarity band: their
// top-1 cosine spans 0.5733-0.8752, overlapping the non-paraphrase
// positives' 0.5957-0.9234. A floor therefore cuts across the paraphrase
// queries indiscriminately rather than isolating them, and most of the
// legitimate answers it removes carry no paraphrase label at all.
//
// The measured signals therefore do not support reliable automatic
// abstention at an acceptable legitimate-answer cost. There is NO new
// default similarity threshold and NO confidence score. The remedy is
// orthogonal to filtering: an honest retrieval contract that tells the
// caller the returned rows are RANKED CANDIDATES whose relevance has not
// been established (the retrieval_status field on the recall output). A
// caller that wants a floor should still set RetrievalThreshold above 0
// deliberately, knowing the rejected set contains legitimate answerable
// queries, paraphrase queries among them.
func DefaultHybridConfig() HybridConfig {
	return HybridConfig{
		VectorWeight: 0.5,
		// Unreachable for vector-only rows (similarity is 0–1). Kept at
		// the historical value rather than silently changing retrieval
		// behaviour; see the calibration comment above.
		RetrievalThreshold: -3.0,
		Limit:              15,
		SchemaPrefix:       "",
		Origin:             "local",
	}
}

// HybridResult is a memory with combined FTS5 + vector scores.
//
// CreatedAt is stored as INTEGER Unix-epoch seconds (see migration
// timestamps_unified_v1). LastAccessedAt is a pointer because the column
// is nullable.
type HybridResult struct {
	ID                 string
	Content            string
	Collection         string
	Tags               string
	Metadata           string
	CreatedAt          int64
	ReinforcementCount int
	Weight             float64
	LastAccessedAt     *int64
	ReferenceID        *string
	FTS5Score          float64 // raw BM25
	VectorSimilarity   float64 // cosine similarity (0.0–1.0)
	CombinedScore      float64 // weighted blend
	Source             string  // "fts5", "vector", "hybrid" — search method
	Origin             string  // "local" | "shared" — data origin (Phase 2d)
	IsChallenged       bool
	IsConceptDrift     bool
	ChallengedTheoryID string
}

// Retrieval status values for a hybrid (semantic) result set.
//
// The semantic ranker returns RANKED CANDIDATES, not verified answers. A
// caller must be able to tell "the ranker produced nothing" from "the
// ranker produced candidates whose relevance has not been established",
// because those two call for opposite responses — rephrase the query,
// versus treat the rows as leads to check.
//
// These values are deliberately NOT a confidence score. The Phase A.3
// measurements (docs/ISSUE-15-PHASE-A-REPORT.md §A2.4) show that no
// similarity or gap signal separates answerable from unanswerable queries
// at an acceptable legitimate-answer cost, so the contract reports what
// the ranker DID rather than guessing what it MEANT.
const (
	// RetrievalStatusNoCandidates means the ranker returned no rows at
	// all — neither the lexical nor the vector channel produced a
	// candidate.
	RetrievalStatusNoCandidates = "no_candidates"
	// RetrievalStatusCandidatesUnverified means rows were returned and
	// ranked, but no relevance was established for any of them. The
	// scores rank the rows; they do not assert that a row answers the
	// query.
	RetrievalStatusCandidatesUnverified = "candidates_unverified"
)

// HybridSearchOutcome carries the rows a hybrid search produced together
// with the status of the semantic (vector) channel.
//
// Why it exists: the vector channel used to fail silently. A provider
// that could not be reached, a provider that returned an empty vector, a
// vector scan that errored — all of them left the caller with "whatever
// the lexical channel produced" and no way to tell that apart from
// "the corpus has nothing near this query". The first is an
// infrastructure failure; the second is a result. Reporting them the
// same way is how a broken embedding service comes to look like an
// empty memory store.
//
// Rows holds exactly the rows HybridSearch returns, in the same order.
type HybridSearchOutcome struct {
	// Rows are the ranked results, identical to what HybridSearch returns.
	Rows []HybridResult

	// VectorChannelError is non-nil when an embedding provider was
	// configured but the semantic arm could not run: the provider
	// failed, returned an empty vector, or the vector scan errored.
	//
	// It is nil when the vector channel ran successfully, and also nil
	// when no embedding provider is configured (the null provider) —
	// that is a configuration state, not a failure, and the lexical
	// channel's result set is the complete answer for that run.
	VectorChannelError error
}

// HybridSearch performs a combined FTS5 + vector search.
// It queries FTS5 and vector spaces independently, then merges and re-ranks.
// FTS5 results that lack embeddings are still returned (graceful degradation).
// Vector results that FTS5 would rank higher are boosted.
//
// A vector-channel failure is NOT reported through this function's error
// return: the lexical channel may still have produced rows, and existing
// callers (the MCP memory-query tool, the federated local+shared scope
// search) depend on the historical behaviour of degrading to a lexical
// result set rather than failing the whole query.
//
// This legacy degradation is DELIBERATELY RETAINED (Issue #15 scope
// boundary, 2026-10-21). The three pre-existing callers —
// HybridSearchMemories, hybridSearchScopeLocal and hybridSearchScopeAll
// in internal/core/memory_tools.go — keep the historical behaviour: a
// vector-channel failure degrades to a lexical result set and is not
// surfaced as an error. Only the CLI semantic-recall path
// (cmd/mpm/recall.go) uses HybridSearchWithOutcome and turns a
// vector-channel failure into a hard error.
//
// Callers that must not present a degraded result set as a complete
// semantic answer should use HybridSearchWithOutcome and inspect
// VectorChannelError.
func HybridSearch(dm *DatabaseManager, query string, collection string, cfg HybridConfig) ([]HybridResult, error) {
	outcome, err := HybridSearchWithOutcome(dm, query, collection, cfg)
	if err != nil {
		return nil, err
	}
	return outcome.Rows, nil
}

// HybridSearchWithOutcome performs the hybrid search and additionally
// reports whether the semantic channel actually ran.
//
// The error return is reserved for a failure of the search itself (an
// invalid schema prefix, or both FTS5 and the LIKE fallback failing).
// A vector-channel failure is carried in
// HybridSearchOutcome.VectorChannelError so a caller can distinguish
// "no candidates" from "the semantic arm never ran".
func HybridSearchWithOutcome(dm *DatabaseManager, query string, collection string, cfg HybridConfig) (HybridSearchOutcome, error) {
	if err := ValidateSchemaPrefix(cfg.SchemaPrefix); err != nil {
		return HybridSearchOutcome{}, err
	}
	// 2026-09-05 audit remediation pass 2: cfg.Limit is no longer
	// silently coerced. Public callers route through parseLimitStrict
	// at the handler boundary; any non-positive value reaching here is
	// an honest caller request for "no results" (Limit=0) or an
	// internal caller contract violation. The handler-level gate
	// pins the public contract.
	if cfg.VectorWeight < 0 || cfg.VectorWeight > 1 {
		cfg.VectorWeight = 0.5
	}
	// Phase 2d (2026-07-06): default empty prefix + local origin.
	// Callers targeting the shared DB set both explicitly so the same
	// code path runs against either ATTACHed schema.
	if cfg.Origin == "" {
		cfg.Origin = "local"
	}

	// ── Step 1: FTS5 keyword search ──────────────────────────────────────────
	ftsResults, err := searchFTS5(dm.SQLDB(), query, collection, cfg.Limit*2, cfg.SchemaPrefix)
	if err != nil {
		// Fall back to LIKE if FTS5 is unavailable
		ftsResults, err = searchLike(dm.SQLDB(), query, collection, cfg.Limit*2, cfg.SchemaPrefix)
		if err != nil {
			return HybridSearchOutcome{}, fmt.Errorf("hybrid search: FTS5/LIKE failed: %w", err)
		}
	}

	// ── Step 2: Vector search ──────────────────────────────────────────────────
	//
	// Every failure mode of the semantic channel is captured rather than
	// swallowed. The rows are still returned (the lexical channel may
	// have produced some), but the caller is told the semantic arm did
	// not run — see HybridSearchOutcome.VectorChannelError.
	var vecResults []VectorMatch
	var vectorChannelErr error
	provider := DefaultEmbeddingConfig().Provider
	if provider.Name() != "null" {
		vec, embedErr := provider.Embed(query)
		switch {
		case embedErr != nil:
			vectorChannelErr = fmt.Errorf("embedding provider %q failed: %w", provider.Name(), embedErr)
		case len(vec) == 0:
			vectorChannelErr = fmt.Errorf("embedding provider %q returned an empty vector", provider.Name())
		default:
			matched, matchErr := dm.VectorMatch(collection, vec, cfg.Limit*2, cfg.SchemaPrefix)
			if matchErr != nil {
				vectorChannelErr = fmt.Errorf("vector channel failed: %w", matchErr)
			} else {
				// VecResults already sorted by similarity.
				vecResults = matched
			}
		}
	}

	// ── Step 3: Merge and re-rank ──────────────────────────────────────────────
	// Build maps for fast lookup
	ftsMap := make(map[string]ftsEntry)
	for _, r := range ftsResults {
		ftsMap[r.ID] = r
	}
	vecMap := make(map[string]vecEntry)
	for _, r := range vecResults {
		vecMap[r.ID] = vecEntry{similarity: r.Similarity}
	}

	// For vector-only candidates, the FTS5 row is absent so the merge
	// can't populate Weight/Collection/Tags/Metadata/IsChallenged/etc.
	// from the lexical entry. Batched SELECT fills the gap with one
	// query. Skipped entirely when there are no vector candidates.
	vectorOnlyMeta := loadVectorOnlyMeta(dm, vecResults, cfg.SchemaPrefix)

	// Union of all IDs
	allIDs := make(map[string]bool)
	for _, r := range ftsResults {
		allIDs[r.ID] = true
	}
	for _, r := range vecResults {
		allIDs[r.ID] = true
	}

	// Build combined results
	var combined []HybridResult
	for id := range allIDs {
		fts, ftsOK := ftsMap[id]
		vec, vecOK := vecMap[id]

		var ftsScore, vecSim, combinedScore float64
		var source string

		if ftsOK && vecOK {
			// Both — full hybrid
			ftsScore = fts.Score
			vecSim = vec.similarity
			combinedScore = hybridScore(fts.Score, vecSim, cfg.VectorWeight)
			source = "hybrid"
		} else if ftsOK {
			// FTS5 only. Normalise the lexical leg onto the same 0–1
			// scale as the hybrid blend so CombinedScore means the same
			// thing for every source. Previously this was
			// `fts.Score * (1-vecWeight)` — raw unbounded BM25 on a
			// negative-is-better scale, which made an FTS5 row's score
			// incomparable to a hybrid row's and put it at a wildly
			// different magnitude (e.g. -7.32 vs -0.16).
			//
			// There is no vector leg to blend in, so the normalised
			// lexical signal is scaled by (1-vecWeight) and nothing
			// else is added: an FTS5-only match is a partial match, and
			// leaving the vector weight's share unfilled keeps it below
			// a hybrid row that matched both signals.
			ftsScore = fts.Score
			combinedScore = normalizeBM25(fts.Score) * (1 - cfg.VectorWeight)
			source = "fts5"
		} else {
			// Vector only (no FTS5 match) — preserve as a candidate.
			// Previous behavior discarded these (continue), which silently
			// broke semantic-archaeology queries where paraphrased natural
			// language has zero token overlap with the corpus. With a real
			// embedding provider active, the vector signal is computed but
			// was being thrown away. Now vector-only candidates survive
			// the merge and rank by their cosine similarity, weighted by
			// VectorWeight so VectorWeight=1.0 yields pure vector retrieval.
			vecSim = vec.similarity
			combinedScore = vec.similarity * cfg.VectorWeight
			source = "vector"
		}

		// RetrievalThreshold applies to hybrid and vector-only candidates.
		// Both are now on a 0–1 "higher is better" scale (see
		// normalizeBM25), so a single threshold is meaningful across them.
		// FTS5-only rows are still exempt: their ordering comes from the
		// BM25 rank itself, and a relevance floor on a lexical-only path
		// discards legitimate exact-term matches.
		switch {
		case ftsOK && vecOK:
			if combinedScore < cfg.RetrievalThreshold {
				continue
			}
		case !ftsOK && vecOK:
			// Vector similarity is 0–1, so the shipped default of -3.0
			// could never fire here. Use a non-negative floor so callers who raise the
			// threshold above 0 can still exclude low-similarity noise.
			if combinedScore < cfg.RetrievalThreshold {
				continue
			}
		}

		isChallenged := false
		isConceptDrift := false
		challengedTheoryID := ""
		// Use whichever metadata source is available: FTS5 (full row)
		// or vectorOnlyMeta (batched fill for vector-only candidates).
		var metaJSON string
		if ftsOK {
			metaJSON = fts.Metadata
		} else if meta, ok := vectorOnlyMeta[id]; ok {
			metaJSON = meta.metadata
		}
		if metaJSON != "" {
			var meta map[string]interface{}
			if json.Unmarshal([]byte(metaJSON), &meta) == nil {
				// concept_drift: true from the idle-dream drift detector
				if cd, _ := meta["concept_drift"].(bool); cd {
					isConceptDrift = true
					isChallenged = true
				}
				// status == "challenged" from manual challenge_memory calls
				if status, _ := meta["status"].(string); status == "challenged" {
					isChallenged = true
				}
				if tid, _ := meta["challenged_theory_id"].(string); tid != "" {
					challengedTheoryID = tid
				}
			}
		}

		// Resolve display fields. FTS5 rows carry full content; vector-only
		// rows need their fields populated from the VectorMatch row (which
		// already returns Content/CreatedAt). The challenge/provenance
		// metadata is empty for vector-only candidates — that's acceptable
		// because vector-only retrieval doesn't have lexical context to
		// surface challenge status, and downstream code that needs metadata
		// re-fetches from the database by ID.
		var content, collection, tags, metadata string
		var createdAt int64
		var reinforcementCount int
		var weight float64
		var lastAccessedAt *int64
		var referenceID *string
		if ftsOK {
			content = fts.Content
			collection = fts.Collection
			tags = fts.Tags
			metadata = fts.Metadata
			createdAt = fts.CreatedAt
			reinforcementCount = fts.ReinforcementCount
			weight = fts.Weight
			lastAccessedAt = fts.LastAccessedAt
			referenceID = fts.ReferenceID
		} else {
			// Vector-only: pull all display fields from the batched
			// metadata lookup. This populates Weight, Collection, Tags,
			// ReinforcementCount, IsChallenged/IsConceptDrift signals —
			// none of which are returned by VectorMatch. Without this
			// enrichment, vector-only rows surface in callers like
			// `mpm recall --semantic` with Weight=0, Collection="",
			// CreatedAt=0, and no challenge/concept-drift visibility,
			// which silently misrepresents the memory's state.
			if meta, ok := vectorOnlyMeta[id]; ok {
				content = meta.content
				collection = meta.collection
				tags = meta.tags
				metadata = meta.metadata
				createdAt = meta.createdAt
				reinforcementCount = meta.reinforcementCount
				weight = meta.weight
			}
			// Vector-only doesn't carry VectorMatch's CreatedAt — the
			// batched lookup above is authoritative.
		}

		combined = append(combined, HybridResult{
			ID:                 id,
			Content:            content,
			Collection:         collection,
			Tags:               tags,
			Metadata:           metadata,
			CreatedAt:          createdAt,
			ReinforcementCount: reinforcementCount,
			Weight:             weight,
			LastAccessedAt:     lastAccessedAt,
			ReferenceID:        referenceID,
			FTS5Score:          ftsScore,
			VectorSimilarity:   vecSim,
			CombinedScore:      combinedScore,
			Source:             source,
			Origin:             cfg.Origin,
			IsChallenged:       isChallenged,
			IsConceptDrift:     isConceptDrift,
			ChallengedTheoryID: challengedTheoryID,
		})
	}

	// Sort: rank by source-appropriate signal.
	//   hybrid  → CombinedScore descending (higher = better blend)
	//   fts5    → FTS5Score ascending (more negative = better match)
	//   vector  → VectorSimilarity descending (higher = more similar)
	// Mixed-source ordering keeps hybrid above FTS5-only above vector-only
	// because vector confirmation beats text-only beats paraphrase-only.
	sort.Slice(combined, func(i, j int) bool {
		if combined[i].Source == combined[j].Source {
			switch combined[i].Source {
			case "fts5":
				return combined[i].FTS5Score < combined[j].FTS5Score
			case "vector":
				return combined[i].VectorSimilarity > combined[j].VectorSimilarity
			case "hybrid":
				return combined[i].CombinedScore > combined[j].CombinedScore
			}
		}
		// Mixed sources: prefer hybrid, then fts5, then vector.
		return sourceRank(combined[i].Source) > sourceRank(combined[j].Source)
	})
	if len(combined) > cfg.Limit {
		combined = combined[:cfg.Limit]
	}

	// ── Phase 4: REMOVED — similarity candidates are no longer discovered here ──
	//
	// This phase used to run three things over the top-15 results: a
	// ChallengeMemory write on each of three branches, and an
	// EnqueueContradiction insert on each of the same three branches.
	//
	// Both are gone, for one shared reason. Cosine similarity establishes
	// that two memories are RELATED. It does not establish that they
	// CONTRADICT each other, and similarity is highest exactly where
	// duplication is highest — so the scan fired hardest on near-identical
	// statements asserting the same claim. On the live store that had
	// challenged 40 distinct members of one 75-row cluster and pinned 30 of
	// them at the weight floor of 1.0.
	//
	// Discovery did not die with the writes; it moved somewhere that cannot
	// write. FindSimilarityCandidates (similarity_candidates.go) is a pure
	// function over ScanEntry values, and EnqueueSimilarityCandidates
	// (similarity_candidates_store.go) is the only persistence path, reached
	// only when an operator explicitly asks for a review scan.
	//
	// Consequences for this file, all intentional:
	//
	//   - Retrieval no longer performs a pairwise scan at all. It was work
	//     whose only output was a write, so removing the write removes the
	//     work.
	//   - Retrieval is now write-free on the semantic path. The narrowing
	//     invariant is now the stronger one: semantic retrieval adds no
	//     persistent record of any kind.
	//   - Ranking and result contents are untouched. Phase 4 ran after
	//     ranking was fixed and after IsChallenged was populated from
	//     pre-existing database state, so it never fed back into either.
	//
	// Tested in hybrid_search_nomutate_test.go and
	// similarity_candidates_test.go.

	// ── Phase 5: In-Memory Provenance Preamble + Warning Prepend ─────────
	// ── Phase 5b: Correction-chain discount ──────────────────────────────
	// Memories tagged "superseded" are intermediate steps in a correction
	// chain — they were correct at the time but later refined. The chain's
	// final reading is what users want. We discount superseded memories
	// (combined score * 0.25) so they don't outrank the final reading in
	// retrieval, while still surfacing them when the chain context is the
	// best match (e.g. someone explicitly asks about the iteration).
	//
	// The "superseded-by" tag (if present) is preserved in the metadata
	// for traceability — callers can fetch the full chain via the linked
	// supersession pointer.
	for i := range combined {
		if preamble := ProvenancePreamble(combined[i].Metadata); preamble != "" {
			combined[i].Content = preamble + "\n" + combined[i].Content
		}
		if combined[i].IsConceptDrift {
			combined[i].Content = conceptDriftWarning + "\n" + combined[i].Content
		} else if combined[i].IsChallenged {
			combined[i].Content = challengeWarning + "\n" + combined[i].Content
		}
		if isSupersededTagsString(combined[i].Tags) {
			combined[i].CombinedScore *= 0.25
		}
	}

	return HybridSearchOutcome{Rows: combined, VectorChannelError: vectorChannelErr}, nil
}

// normalizeBM25 maps an FTS5 bm25() score onto a 0–1 scale where
// HIGHER MEANS A BETTER MATCH.
//
// SQLite's bm25() is negative-is-better: the strongest match returns the
// most negative number. The earlier `score / (1 + |score|)` squash was
// sign-preserving, so it mapped the best lexical match (most negative)
// to the worst normalised value (near -1) — inverting the lexical signal
// inside the blend. A 22-query benchmark caught this: with bm25 -22.57
// ranked above bm25 -24.36 for the same query.
//
// Dividing by -1 first flips the sign so better matches are positive,
// then the squash compresses the unbounded magnitude into (0, 1):
//
//	bm25 -24.365 -> normFTS 0.961   (strongest match)
//	bm25  -8.797 -> normFTS 0.898
//	bm25  -0.300 -> normFTS 0.231   (weakest match)
//
// This is not a sigmoid — it is a monotone squash — so the comment
// claiming sigmoid normalisation was wrong as well.
func normalizeBM25(ftsScore float64) float64 {
	return -ftsScore / (1 + math.Abs(ftsScore))
}

// hybridScore blends FTS5 BM25 and vector cosine similarity.
//
// Both legs are on the same 0–1 "higher is better" scale, so the result
// is directly comparable across sources and against RetrievalThreshold:
//
//	combined = (1-vecWeight)*normalizeBM25(bm25) + vecWeight*cosine
func hybridScore(ftsScore float64, vecSim float64, vecWeight float64) float64 {
	return (1-vecWeight)*normalizeBM25(ftsScore) + vecWeight*vecSim
}

// sourceRank gives the cross-source ordering priority: hybrid > fts5 > vector.
// Higher value = more preferred when sorting mixed-source result sets.
func sourceRank(s string) int {
	switch s {
	case "hybrid":
		return 3
	case "fts5":
		return 2
	case "vector":
		return 1
	default:
		return 0
	}
}

// vecRowFor looks up a vector row by ID. Used to populate display fields
// (Content, CreatedAt) for vector-only candidates whose FTS5 entry is
// absent. Returns false when the row is missing (defensive — vecMap and
// vecResults are built from the same query, so this should not happen in
// practice).
func vecRowFor(id string, rows []VectorMatch) (VectorMatch, bool) {
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
	}
	return VectorMatch{}, false
}

// vectorOnlyMeta carries the metadata fields VectorMatch does not return.
// Populated by loadVectorOnlyMeta so the merge step can fully populate
// HybridResult fields for vector-only candidates without per-row queries.
type vectorOnlyMetaRow struct {
	content            string
	collection         string
	tags               string
	metadata           string
	createdAt          int64
	reinforcementCount int
	weight             float64
}

// loadVectorOnlyMeta issues a single batched SELECT for any vector-result
// IDs not covered by the FTS5 path. Returns an empty map when there are
// no vector candidates (the common NullProvider case) — the merge loop
// skips the lookup entirely in that case via the if ftsOK short-circuit.
//
// Cost: one indexed IN-list query against `memories`. Runs only when
// VectorMatch produced results, so NullProvider configurations pay zero.
func loadVectorOnlyMeta(dm *DatabaseManager, vecResults []VectorMatch, schemaPrefix string) map[string]vectorOnlyMetaRow {
	out := make(map[string]vectorOnlyMetaRow)
	if len(vecResults) == 0 {
		return out
	}
	placeholders := make([]string, len(vecResults))
	args := make([]interface{}, len(vecResults))
	for i, r := range vecResults {
		placeholders[i] = "?"
		args[i] = r.ID
	}
	memTable := schemaPrefix + "memories"
	query := `SELECT id, content, collection, tags, COALESCE(metadata, '{}'), created_at, reinforcement_count, weight FROM ` + memTable +
		` WHERE id IN (` + strings.Join(placeholders, ",") + `) AND deleted_at IS NULL` + MemoryExpireClause
	rows, err := dm.SQLDB().Query(query, args...)
	if err != nil {
		slog.Warn("loadVectorOnlyMeta: query failed", "err", err, "ids", args)
		// Defensive: if the batched lookup fails, vector-only rows get
		// zero-valued fields. The merge still completes; callers see
		// empty strings instead of an error. Better than swallowing
		// the whole search.
		return out
	}
	defer rows.Close()
	for rows.Next() {
		// tags and metadata are nullable JSON columns. Direct string
		// scanning panics on NULL rows with
		// "converting NULL to string is unsupported". The 2026-08-17
		// Substrate Defense Triad mandates NullString for any
		// nullable scalar — including JSON columns, which are TEXT
		// under the hood.
		var (
			id, content, collection string
			tags, metadata          sql.NullString
			createdAt               int64
			reinforcementCount      int
			weight                  sql.NullFloat64
		)
		if err := rows.Scan(&id, &content, &collection, &tags, &metadata, &createdAt, &reinforcementCount, &weight); err != nil {
			slog.Warn("loadVectorOnlyMeta: scan failed", "err", err)
			continue
		}
		tagsStr := ""
		if tags.Valid {
			tagsStr = tags.String
		}
		metaStr := ""
		if metadata.Valid {
			metaStr = metadata.String
		}
		weightVal := 0.0
		if weight.Valid {
			weightVal = weight.Float64
		}
		out[id] = vectorOnlyMetaRow{
			content:            content,
			collection:         collection,
			tags:               tagsStr,
			metadata:           metaStr,
			createdAt:          createdAt,
			reinforcementCount: reinforcementCount,
			weight:             weightVal,
		}
	}
	return out
}

// ── Internal helpers ─────────────────────────────────────────────────────────

type ftsEntry struct {
	ID                 string
	Content            string
	Collection         string
	Tags               string
	Metadata           string
	CreatedAt          int64
	ReinforcementCount int
	Weight             float64 // SQLite REAL — even though the schema declares INTEGER,
	// values like 1.5 are stored as REAL via type affinity,
	// so Scan must use a float destination.
	LastAccessedAt *int64
	ReferenceID    *string
	Score          float64
	// Retrieval metadata (Observability Layer, 2026-07-26).
	// Populated by a LEFT JOIN against retrieval_metadata; today these
	// fields are observability-only and do not influence Score or
	// ORDER BY. A future RetrievalRanker implementation may consult
	// these to blend a reuse-adjusted score per row.
	ReuseCount      int
	LastRetrievedAt *int64
	SuccessCount    int
}

type vecEntry struct {
	similarity float64
}

// VectorMatch is returned by DatabaseManager.VectorMatch.
//
// CreatedAt is stored as INTEGER Unix-epoch seconds (see migration
// timestamps_unified_v1).
type VectorMatch struct {
	ID         string
	Content    string
	CreatedAt  int64
	Similarity float64
}

// searchFTS5 runs FTS5 MATCH with bm25 scoring.
func searchFTS5(db *sql.DB, query, collection string, limit int, schemaPrefix string) ([]ftsEntry, error) {
	// schemaPrefix is either "" (local) or "shared." (ATTACHed shared DB).
	// Constants only — never user input — so direct concatenation is safe.
	// FTS5 bm25() requires the virtual table's full qualified name.
	ftsTable := schemaPrefix + "memories_fts"
	memTable := schemaPrefix + "memories"

	// FTS5 query construction (2026-07-23, builds on 2026-07-17 fix).
	// The 2026-07-17 fix wrapped the user query in double quotes to
	// dodge the hyphen-as-column-name parser quirk. That worked at the
	// syntax level but introduced a new silent bug: phrase queries
	// (`"lazy start"`) only match the literal two-word phrase, while
	// the porter unicode61 tokenizer stores "lazy-start" as the
	// separate tokens "lazy" and "start". Wrapping in quotes hid the
	// hyphen crash but traded it for silent zero-result queries.
	//
	// BuildFTS5Query tokenizes the query the same way the index does
	// (split on whitespace + hyphens + underscores + dots), applies
	// prefix wildcard per token, and joins with whitespace (FTS5
	// implicit AND). This makes hyphenated, multi-word, and partial
	// keyword queries all work via the same code path.
	safeQuery := BuildFTS5Query(query)
	if safeQuery == "" {
		// Empty query — FTS5 MATCH "" raises an error; return empty
		// so the caller can short-circuit.
		return nil, nil
	}
	sqlQuery := `
		SELECT m.id, m.content, m.collection, m.tags, m.metadata,
		       m.created_at, m.reinforcement_count, m.weight,
		       m.last_accessed_at, m.reference_id,
		       bm25(` + ftsTable + `) AS score,
		       COALESCE(rm.reuse_count, 0) AS reuse_count,
		       rm.last_retrieved_at AS last_retrieved_at,
		       COALESCE(rm.success_count, 0) AS success_count
		FROM ` + ftsTable + `
		JOIN ` + memTable + ` m ON ` + ftsTable + `.rowid = m.rowid
		LEFT JOIN retrieval_metadata rm ON rm.node_id = m.id
		WHERE ` + ftsTable + ` MATCH ? AND m.deleted_at IS NULL` + MemoryExpireClauseM + `
		  AND (? = '' OR m.collection = ?)
		ORDER BY score
		LIMIT ?`
	rows, err := db.Query(sqlQuery, safeQuery, collection, collection, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result, scanErr := scanFTSEntries(rows)
	if scanErr != nil {
		return nil, scanErr
	}
	return result, nil
}

// searchLike is the FTS5 fallback using LIKE.
func searchLike(db *sql.DB, query, collection string, limit int, schemaPrefix string) ([]ftsEntry, error) {
	likePat := "%" + strings.ReplaceAll(query, "%", "\\%") + "%"
	memTable := schemaPrefix + "memories"
	sqlQuery := `
		SELECT m.id, m.content, m.collection, m.tags, m.metadata,
		       m.created_at, m.reinforcement_count, m.weight,
		       m.last_accessed_at, m.reference_id,
		       0.0 AS score,
		       COALESCE(rm.reuse_count, 0) AS reuse_count,
		       rm.last_retrieved_at AS last_retrieved_at,
		       COALESCE(rm.success_count, 0) AS success_count
		FROM ` + memTable + ` m
		LEFT JOIN retrieval_metadata rm ON rm.node_id = m.id
		WHERE m.content LIKE ? AND m.deleted_at IS NULL` + MemoryExpireClause + `
		  AND (? = '' OR m.collection = ?)
		ORDER BY m.created_at DESC
		LIMIT ?`
	rows, err := db.Query(sqlQuery, likePat, collection, collection, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFTSEntries(rows)
}

func scanFTSEntries(rows *sql.Rows) ([]ftsEntry, error) {
	var results []ftsEntry
	for rows.Next() {
		var e ftsEntry
		var nullableTags, nullableMetadata, nullableRefID sql.NullString
		// Timestamp columns may be stored as INTEGER (unix epoch) or as
		// TEXT with DATETIME affinity. The mattn driver returns:
		//   INTEGER columns → driver.Value type int64
		//   TEXT/DATETIME columns → driver.Value type time.Time
		// sql.NullTime can't accept int64; sql.NullInt64 can't accept
		// time.Time. Scan into interface{} and detect the type at runtime
		// so both storage formats work without schema migrations.
		var createdAtRaw, lastAccessedRaw, lastRetrievedRaw interface{}
		if err := rows.Scan(&e.ID, &e.Content, &e.Collection, &nullableTags, &nullableMetadata,
			&createdAtRaw, &e.ReinforcementCount, &e.Weight,
			&lastAccessedRaw, &nullableRefID, &e.Score,
			&e.ReuseCount, &lastRetrievedRaw, &e.SuccessCount); err != nil {
			return nil, fmt.Errorf("scanning FTS entry row: %w", err)
		}
		if v, ok := timestampToUnix(createdAtRaw); ok {
			e.CreatedAt = v
		}
		e.Tags = nullableTags.String
		e.Metadata = nullableMetadata.String
		if v, ok := timestampToUnix(lastAccessedRaw); ok {
			e.LastAccessedAt = &v
		}
		if nullableRefID.Valid {
			e.ReferenceID = &nullableRefID.String
		}
		if v, ok := timestampToUnix(lastRetrievedRaw); ok {
			e.LastRetrievedAt = &v
		}
		results = append(results, e)
	}
	return results, rows.Err()
}

// timestampToUnix normalises whatever the mattn/go-sqlite3 driver hands
// back for a timestamp column. Returns (unixSeconds, true) for any
// usable value; (0, false) for NULL.
//
// Storage variants observed in the wild:
//   - INTEGER (unix epoch)        → driver.Value type int64
//   - TEXT in RFC3339 format      → driver.Value type string
//   - TEXT in 'YYYY-MM-DD HH:MM:SS' (DATETIME affinity) → time.Time
//
// All three reduce to a single int64 unix-second representation without
// the caller needing to know which storage format the database picked.
func timestampToUnix(v interface{}) (int64, bool) {
	if v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case time.Time:
		return t.Unix(), true
	case string:
		// RFC3339 first (more specific), then DATETIME format.
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed.Unix(), true
		}
		if parsed, err := time.Parse("2006-01-02 15:04:05", t); err == nil {
			return parsed.Unix(), true
		}
		// Try parsing as plain integer string (legacy INTEGER-as-string).
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return n, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// VectorMatch searches the memories table for vector similarity.
//
// Strategy:
//  1. If vector_clusters has rows (operator has run `mpm ops rebalance`),
//     use IVFSearch — candidate generation scans K centroids + N/K * probe_p
//     members instead of all N rows. ~250x reduction at typical N.
//  2. Otherwise (fresh DB, no rebalance yet), fall back to the brute-force
//     scan with the MPM_MAX_VECTOR_SCAN circuit breaker intact. The cap
//     protects against accidentally large scans during the bootstrap window
//     before the operator gets around to running rebalance.
//
// schemaPrefix (since Phase 2d, 2026-07-06): empty for local tables,
// "shared." for ATTACHed shared DB. Constrained to two values; safe
// to interpolate directly into the table reference.
func (dm *DatabaseManager) VectorMatch(collection string, queryEmbedding []float32, limit int, schemaPrefix string) ([]VectorMatch, error) {
	if err := ValidateSchemaPrefix(schemaPrefix); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}

	// Step 1: try IVFSearch. If clusters exist, this is the fast path.
	ivfResults, err := IVFSearch(dm.SQLDB(), queryEmbedding, collection,
		IVFConfig{ProbeP: DefaultIVFConfig().ProbeP, Limit: limit}, schemaPrefix)
	if err != nil {
		// IVF errored out (schema missing, malformed centroids, etc.)
		// — fall through to brute force rather than failing the query.
		slog.Warn("VectorMatch: IVFSearch failed, falling back to brute force",
			"error", err.Error())
	} else if ivfResults != nil {
		// IVF succeeded and returned candidates. We're done.
		return ivfResults, nil
	}
	// ivfResults == nil means no clusters exist — fall through to brute.

	// Step 2: brute-force fallback with circuit breaker.
	whereClauses := []string{"embedding IS NOT NULL", "embedding != 'null'", "deleted_at IS NULL" + MemoryExpireClause}
	args := []interface{}{}
	if collection != "" {
		whereClauses = append(whereClauses, "collection = ?")
		args = append(args, collection)
	}
	memTable := schemaPrefix + "memories"

	// Model compatibility. This is the check that dimension equality
	// cannot make: two models can share a dimension while producing
	// incomparable vector spaces, and the `len(dbEmbedding) !=
	// len(queryEmbedding)` guard below passes in exactly that case. The
	// cosine is then computed over unrelated spaces and returns a
	// plausible number in [-1,1] — which is worse than no score,
	// because a score above 0.85 makes HybridSearch call
	// ChallengeMemory() and MUTATE the memory.
	//
	// Rows with NULL embedding_model predate the 2026-10-13 migration
	// and cannot be attributed to any model, so they are excluded too:
	// unproven is not the same as compatible. `mpm ops
	// backfill-embeddings` selects unlabelled rows as pending and restores
	// them under the active model.
	activeModel := CurrentEmbeddingFingerprint().Model
	if activeModel != "" {
		whereClauses = append(whereClauses, "embedding_model = ?")
		args = append(args, activeModel)
	}

	maxScan := dm.GetConfigInt("vector.max_scan", 5000)
	if maxScan > 0 {
		countQuery := `SELECT COUNT(*) FROM ` + memTable + ` WHERE ` + strings.Join(whereClauses, " AND ")
		var rowCount int
		if err := dm.SQLDB().QueryRow(countQuery, args...).Scan(&rowCount); err != nil {
			return nil, fmt.Errorf("vector scan preflight count: %w", err)
		}
		if rowCount > maxScan {
			return nil, fmt.Errorf(
				"vector scan cap exceeded: %d rows in %s (cap=%d via MPM_MAX_VECTOR_SCAN). "+
					"Run `mpm ops rebalance` to enable IVF (drops scan to N/K * probe_p) or raise the cap.",
				rowCount, memTable, maxScan)
		}
	}

	whereSQL := strings.Join(whereClauses, " AND ")

	// Fail loudly, not silently. After the 2026-10-13 embedding_model
	// migration every pre-existing memory row is unlabelled, and the
	// compatibility filter above therefore excludes all of them. That is
	// the correct behaviour — unproven is not comparable — but the visible
	// symptom is "no memories found", which reads like an empty store and
	// sends the operator looking in entirely the wrong place.
	//
	// Guarded on `labelled == 0`, not merely `unlabelled > 0`: a partially
	// backfilled store has unlabelled rows but still retrieves fine, and
	// warning there would train the operator to ignore this message. It
	// fires only when the filter has actually emptied the candidate set.
	// The remedy is a re-embed, not a config change.
	if activeModel != "" {
		var labelled, unlabelled int
		if err := dm.SQLDB().QueryRow(
			`SELECT
				COALESCE(SUM(CASE WHEN embedding_model IS NOT NULL
				                   AND TRIM(embedding_model) <> '' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN embedding_model IS NULL
				                   OR TRIM(embedding_model) = '' THEN 1 ELSE 0 END), 0)
			 FROM `+memTable+` WHERE embedding IS NOT NULL AND embedding != 'null'`,
		).Scan(&labelled, &unlabelled); err == nil && unlabelled > 0 && labelled == 0 {
			slog.Warn("VectorMatch: every embedded memory is excluded by the "+
				"embedding-model compatibility filter",
				"active_model", activeModel,
				"unlabelled_rows", unlabelled,
				"remedy", "run `mpm ops backfill-embeddings` to re-embed "+
					"them under the active model and record the fingerprint")
		}
	}

	rows, err := dm.SQLDB().Query(`
		SELECT id, content, created_at, embedding, embedding_source
		FROM `+memTable+`
		WHERE `+whereSQL,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []VectorMatch
	for rows.Next() {
		var id, content, embeddingJSON, embeddingSource string
		var createdAt int64
		if err := rows.Scan(&id, &content, &createdAt, &embeddingJSON, &embeddingSource); err != nil {
			return nil, fmt.Errorf("scanning VectorMatch memory row: %w", err)
		}
		var dbEmbedding []float32
		if err := json.Unmarshal([]byte(embeddingJSON), &dbEmbedding); err != nil || len(dbEmbedding) != len(queryEmbedding) || embeddingSource == "hash" {
			continue
		}
		sim := cosineSimilarity(queryEmbedding, dbEmbedding)
		results = append(results, VectorMatch{
			ID:         id,
			Content:    content,
			CreatedAt:  createdAt,
			Similarity: float64(sim),
		})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, rows.Err()
}

// IsStructuralPrefix returns true when content starts with a template
// marker that dominates embedding similarity without carrying semantic
// signal (CHOICE:, ##, Fact:, etc.). When two memories share such a
// prefix, the embedding model returns a high cosine score based on
// template tokens rather than the underlying claim — the contradiction
// detector halves the score in that case to avoid false-positive flags.
//
// Extracted from hybrid_search.go so the predicate is testable in
// isolation. See scorer_discount_test.go for coverage.

// structuralPrefixes is the set of template markers that, when shared
// between two memories, inflate the cosine similarity score without
// adding semantic signal.
var structuralPrefixes = []string{
	"CHOICE:", "CHOICE :", "## ", "### ", "#### ",
	"Fact:", "Note:", "Decision:", "Update:",
	"TODO:", "FIXME:", "WARNING:", "ERROR:",
	"INFO:", "DEBUG:", "ISSUE:", "PR:", "RFC:",
	"v:", "V:", "USER:",
}

func IsStructuralPrefix(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 5 {
		return false
	}
	for _, p := range structuralPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// IsSuperseded returns true when a memory is tagged as an intermediate
// step in a correction chain. Superseded memories are still retrievable
// for audit and chain-context queries, but the hybrid search discounts
// their combined score so the chain's final reading surfaces first.
//
// Tag conventions:
//
//	"superseded"               — generic: this memory was refined by a later one
//	"superseded-by:<id>"      — points at the chain's final reading
//
// Generalized form covers any domain where a value gets refined over time
// (calibration corrections, API version migrations, schema evolution).
// The discount is intentionally aggressive (0.25x) so the final reading
// wins decisively in retrieval without outright hiding the intermediate.
func IsSuperseded(tags []string) bool {
	for _, t := range tags {
		if t == "superseded" || strings.HasPrefix(t, "superseded-by:") {
			return true
		}
	}
	return false
}

// isSupersededTagsString is the tags-as-string variant of IsSuperseded.
// HybridResult.Tags is a comma-separated string (not a slice), so we
// split here. Cheap — runs only on top candidates.
func isSupersededTagsString(tags string) bool {
	if tags == "" {
		return false
	}
	for _, t := range strings.Split(tags, ",") {
		t = strings.TrimSpace(t)
		if t == "superseded" || strings.HasPrefix(t, "superseded-by:") {
			return true
		}
	}
	return false
}
