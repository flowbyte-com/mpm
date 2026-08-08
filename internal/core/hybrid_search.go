package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strings"
)

const challengeWarning     = "[Note: This memory is challenged — treat as unverified]"
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
	// Used by Phase 2d of WISHLIST.md (Multi-Agent Shared Epistemology)
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
func DefaultHybridConfig() HybridConfig {
	return HybridConfig{
		VectorWeight:       0.5,
		RetrievalThreshold: -3.0, // BM25 can be negative; threshold here is on combined score
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

// HybridSearch performs a combined FTS5 + vector search.
// It queries FTS5 and vector spaces independently, then merges and re-ranks.
// FTS5 results that lack embeddings are still returned (graceful degradation).
// Vector results that FTS5 would rank higher are boosted.
func HybridSearch(dm *DatabaseManager, query string, collection string, cfg HybridConfig) ([]HybridResult, error) {
	if err := ValidateSchemaPrefix(cfg.SchemaPrefix); err != nil {
		return nil, err
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 15
	}
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
			return nil, fmt.Errorf("hybrid search: FTS5/LIKE failed: %w", err)
		}
	}

	// ── Step 2: Vector search ──────────────────────────────────────────────────
	var vecResults []VectorMatch
	provider := DefaultEmbeddingConfig().Provider
	if provider.Name() != "null" {
		vec, err := provider.Embed(query)
		if err == nil && len(vec) > 0 {
			vecResults, err = dm.VectorMatch(collection, vec, cfg.Limit*2, cfg.SchemaPrefix)
			// VecResults already sorted by similarity; ignore error and continue
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
			// FTS5 only
			ftsScore = fts.Score
			combinedScore = fts.Score * (1 - cfg.VectorWeight)
			source = "fts5"
		} else {
			// Vector only (no FTS5 match) — rare, skip unless we want vector-only mode
			continue
		}

		// RetrievalThreshold is calibrated for hybrid (FTS5+vector) combined
		// scores, which land in roughly 0–1 range. BM25 is naturally unbounded
		// negative, so for FTS5-only matches the same threshold would filter
		// out nearly everything useful. Trust the BM25 ordering for FTS5-only
		// paths; only apply the threshold to true hybrid scores.
		if ftsOK && vecOK && combinedScore < cfg.RetrievalThreshold {
			continue
		}

		isChallenged := false
		isConceptDrift := false
		challengedTheoryID := ""
		if fts.Metadata != "" {
			var meta map[string]interface{}
			if json.Unmarshal([]byte(fts.Metadata), &meta) == nil {
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

		combined = append(combined, HybridResult{
			ID:                 id,
			Content:            fts.Content,
			Collection:         fts.Collection,
			Tags:               fts.Tags,
			Metadata:           fts.Metadata,
			CreatedAt:          fts.CreatedAt,
			ReinforcementCount: fts.ReinforcementCount,
			Weight:             fts.Weight,
			LastAccessedAt:     fts.LastAccessedAt,
			ReferenceID:        fts.ReferenceID,
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

	// Sort by combined score descending
	sort.Slice(combined, func(i, j int) bool {
		return combined[i].CombinedScore > combined[j].CombinedScore
	})

	if len(combined) > cfg.Limit {
		combined = combined[:cfg.Limit]
	}

	// ── Phase 4: Contradiction Detection (Cognitive Immune System) ─────
	// Limit to top 15 candidates for O(1) pairwise evaluation (C(15,2) = 105)
	// Uses provenance-based tiebreaker rules when a collision is detected.
	scanLimit := 15
	if len(combined) < scanLimit {
		scanLimit = len(combined)
	}
	scanSet := combined[:scanLimit]

	// Load embeddings + provenance for pairwise cosine similarity
	type candidateInfo struct {
		embedding    []float32
		content      string
		isChallenged bool
		metadataJSON string
	}

	// isStructuralPrefix returns true when content starts with a template
	// marker that dominates embedding similarity without carrying semantic
	// signal. The CHOICE:/## /Fact:/Note:/Decision: family are how decision
	// records, log entries, and section headings start — when two unrelated
	// memories share the same prefix, the embedding model returns a high
	// cosine score based on the template tokens, not the underlying claim.
	// Halving the similarity score in this case prevents the contradiction
	// detector from false-flagging unrelated decisions/logs/sections as
	// semantic collisions. Added 2026-07-17 after the c0d7c5807 vs
	// 077e9b207aa6be1e false positive (both started with "CHOICE: CHOICE:",
	// cosine=0.88, unrelated content).

	// Phase 2d: build the contradiction lookup once with the schema prefix.
	// INSERT INTO shared.memories for shared DBs; bare `memories` for local.
	contradictionTable := cfg.SchemaPrefix + "memories"
	candMap := make(map[string]candidateInfo, scanLimit)

	// Batch fetch embeddings + content + metadata in a single query
	if scanLimit > 0 {
		placeholders := make([]string, scanLimit)
		args := make([]interface{}, scanLimit)
		for i, c := range scanSet {
			placeholders[i] = "?"
			args[i] = c.ID
		}
		rows, err := dm.SQLDB().Query(
			fmt.Sprintf(`SELECT id, embedding, content, COALESCE(metadata, '{}') FROM %s WHERE id IN (%s) AND embedding IS NOT NULL AND embedding != 'null'`, contradictionTable, strings.Join(placeholders, ",")),
			args...,
		)
		if err == nil {
			for rows.Next() {
				var id, embStr, contentStr, metaStr string
				if err := rows.Scan(&id, &embStr, &contentStr, &metaStr); err == nil && embStr != "" {
					var emb []float32
					if json.Unmarshal([]byte(embStr), &emb) == nil && len(emb) > 0 {
						candMap[id] = candidateInfo{embedding: emb, content: contentStr, isChallenged: false, metadataJSON: metaStr}
					}
				}
			}
			rows.Close()
		}
	}

	// Backfill isChallenged from scanSet (since we didn't include it in the batch query)
	for _, c := range scanSet {
		if info, ok := candMap[c.ID]; ok {
			info.isChallenged = c.IsChallenged
			candMap[c.ID] = info
		}
	}

	// Provenance tier priority for tiebreaker: absolute > high > standard > ephemeral
	provenanceTier := func(compute string) int {
		switch compute {
		case "absolute":
			return 4
		case "high":
			return 3
		case "standard":
			return 2
		case "ephemeral":
			return 1
		default:
			return 2 // default to standard
		}
	}

	// Pairwise O(1) contradiction scan
	for i := 0; i < scanLimit; i++ {
		ci, ciOK := candMap[scanSet[i].ID]
		if !ciOK || len(ci.embedding) == 0 {
			continue
		}
		for j := i + 1; j < scanLimit; j++ {
			cj, cjOK := candMap[scanSet[j].ID]
			if !cjOK || len(cj.embedding) == 0 {
				continue
			}
			sim := cosineSimilarity(ci.embedding, cj.embedding)
			// Structural-prefix discount: if both candidates start with the same
			// template marker (e.g. "CHOICE: CHOICE:") the cosine score is
			// inflated by shared tokens. Halve it before the threshold check.
			if IsStructuralPrefix(ci.content) && IsStructuralPrefix(cj.content) {
				sim = sim * 0.5
			}
			if sim < 0.85 {
				continue
			}

			// Extract provenance compute for both memories
			ciCompute := extractProvenanceCompute(ci.metadataJSON)
			cjCompute := extractProvenanceCompute(cj.metadataJSON)
			ciTier := provenanceTier(ciCompute)
			cjTier := provenanceTier(cjCompute)

			// State collision: one challenged, one not — challenge the unchallenged one.
			// Previously used ChallengeMemoryAsync (fire-and-forget, no DB write). Fixed:
			// uses synchronous ChallengeMemory so the DB is actually updated.
			if ci.isChallenged != cj.isChallenged {
				var challengedID, unchallengedID string
				if ci.isChallenged {
					challengedID = scanSet[i].ID
					unchallengedID = scanSet[j].ID
				} else {
					challengedID = scanSet[j].ID
					unchallengedID = scanSet[i].ID
				}
				evidence := fmt.Sprintf(
					"semantic collision (cosine=%.2f) between challenged memory %s and unchallenged memory %s",
					sim, challengedID, unchallengedID)
				dm.ChallengeMemory(unchallengedID, 1, evidence)
				// Arc 1: enqueue in the shared contradiction queue
				// so the operator can resolve via `mpm ops
				// resolve-contradictions`. Best-effort: if the
				// shared DB isn't attached, the slash still
				// landed; the queue insert is a no-op.
				_ = dm.EnqueueContradiction(ContradictionEvidence{
					MemoryA:    challengedID,
					MemoryB:    unchallengedID,
					Evidence:   evidence,
					Similarity: float64(sim),
				})
				continue
			}

			// Provenance-based tiebreaker: different tiers
			if ciTier != cjTier {
				var winnerID, loserID string
				var loserCompute string
				var slashAmount int
				if ciTier > cjTier {
					winnerID = scanSet[i].ID
					loserID = scanSet[j].ID
					loserCompute = cjCompute
				} else {
					winnerID = scanSet[j].ID
					loserID = scanSet[i].ID
					loserCompute = ciCompute
				}
				// Human vs Model: slash -5
				// High vs Ephemeral: slash -3
				if loserCompute == "ephemeral" {
					slashAmount = 3
				} else {
					slashAmount = 5
				}
				evidence := fmt.Sprintf(
					"provenance collision (cosine=%.2f): %s (tier=%d) vs %s (tier=%d) — resolved in favor of %s",
					sim, scanSet[i].ID, ciTier, scanSet[j].ID, cjTier, winnerID)
				dm.ChallengeMemory(loserID, slashAmount, evidence)
				_ = dm.EnqueueContradiction(ContradictionEvidence{
					MemoryA:    winnerID,
					MemoryB:    loserID,
					Evidence:   evidence,
					Similarity: float64(sim),
				})
				continue
			}

			// Equal tier — both memories have equal provenance weight and neither is
			// challenged. Mark the first as challenged to signal unresolved dispute.
			// (scanSet[i] is the earlier-in-list member of the unordered pair since j > i.)
			if !cj.isChallenged {
				evidence := fmt.Sprintf(
					"unresolved state collision (cosine=%.2f) between equal-tier memories %s and %s — pending manual review",
					sim, scanSet[i].ID, scanSet[j].ID)
				dm.ChallengeMemory(scanSet[i].ID, 1, evidence)
				_ = dm.EnqueueContradiction(ContradictionEvidence{
					MemoryA:    scanSet[i].ID,
					MemoryB:    scanSet[j].ID,
					Evidence:   evidence,
					Similarity: float64(sim),
				})
			}
		}
	}

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

	return combined, nil
}

// hybridScore blends FTS5 BM25 and vector cosine similarity.
// FTS5 scores are unbounded; normalize to 0–1 range using sigmoid.
// Vector similarity is already 0–1 cosine.
func hybridScore(ftsScore float64, vecSim float64, vecWeight float64) float64 {
	// Sigmoid normalization for BM25: maps unbounded scores to ~0–1
	// Using a simple tanh-based squash: score / (1 + |score|)
	normFTS := ftsScore / (1 + math.Abs(ftsScore))

	// Blend: combined = (1-vecWeight)*normFTS + vecWeight*vecSim
	return (1-vecWeight)*normFTS + vecWeight*vecSim
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
	LastAccessedAt     *int64
	ReferenceID        *string
	Score              float64
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
	return scanFTSEntries(rows)
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
		var nullableLastAccessed, nullableMetaLastRetrieved sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Content, &e.Collection, &nullableTags, &nullableMetadata,
			&e.CreatedAt, &e.ReinforcementCount, &e.Weight,
			&nullableLastAccessed, &nullableRefID, &e.Score,
			&e.ReuseCount, &nullableMetaLastRetrieved, &e.SuccessCount); err != nil {
			return nil, fmt.Errorf("scanning FTS entry row: %w", err)
		}
		e.Tags = nullableTags.String
		e.Metadata = nullableMetadata.String
		if nullableLastAccessed.Valid {
			v := nullableLastAccessed.Int64
			e.LastAccessedAt = &v
		}
		if nullableRefID.Valid {
			e.ReferenceID = &nullableRefID.String
		}
		if nullableMetaLastRetrieved.Valid {
			v := nullableMetaLastRetrieved.Int64
			e.LastRetrievedAt = &v
		}
		results = append(results, e)
	}
	return results, rows.Err()
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
	rows, err := dm.SQLDB().Query(`
		SELECT id, content, created_at, embedding
		FROM `+memTable+`
		WHERE `+whereSQL,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []VectorMatch
	for rows.Next() {
		var id, content, embeddingJSON string
		var createdAt int64
		if err := rows.Scan(&id, &content, &createdAt, &embeddingJSON); err != nil {
			return nil, fmt.Errorf("scanning VectorMatch memory row: %w", err)
		}
		var dbEmbedding []float32
		if err := json.Unmarshal([]byte(embeddingJSON), &dbEmbedding); err != nil || len(dbEmbedding) != len(queryEmbedding) {
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
//   "superseded"               — generic: this memory was refined by a later one
//   "superseded-by:<id>"      — points at the chain's final reading
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
