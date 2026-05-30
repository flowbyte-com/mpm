package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

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
}

// DefaultHybridConfig returns sensible defaults.
func DefaultHybridConfig() HybridConfig {
	return HybridConfig{
		VectorWeight:      0.5,
		RetrievalThreshold: -3.0, // BM25 can be negative; threshold here is on combined score
		Limit:             15,
	}
}

// HybridResult is a memory with combined FTS5 + vector scores.
type HybridResult struct {
	ID                 string
	Content            string
	Collection         string
	Tags               string
	Metadata           string
	CreatedAt          string
	ReinforcementCount int
	Weight             int
	LastAccessedAt     *string
	ReferenceID        *string
	FTS5Score float64 // raw BM25
	VectorSimilarity  float64 // cosine similarity (0.0–1.0)
	CombinedScore     float64 // weighted blend
	Source string  // "fts5", "vector", "hybrid"
}

// HybridSearch performs a combined FTS5 + vector search.
// It queries FTS5 and vector spaces independently, then merges and re-ranks.
// FTS5 results that lack embeddings are still returned (graceful degradation).
// Vector results that FTS5 would rank higher are boosted.
func HybridSearch(dm *DatabaseManager, query string, collection string, cfg HybridConfig) ([]HybridResult, error) {
	if cfg.Limit <= 0 {
		cfg.Limit = 15
	}
	if cfg.VectorWeight < 0 || cfg.VectorWeight > 1 {
		cfg.VectorWeight = 0.5
	}

	// ── Step 1: FTS5 keyword search ──────────────────────────────────────────
	ftsResults, err := searchFTS5(dm.SQLDB(), query, collection, cfg.Limit*2)
	if err != nil {
		// Fall back to LIKE if FTS5 is unavailable
		ftsResults, err = searchLike(dm.SQLDB(), query, collection, cfg.Limit*2)
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
			vecResults, err = dm.VectorMatch(collection, vec, cfg.Limit*2)
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

		if combinedScore < cfg.RetrievalThreshold {
			continue
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
			VectorSimilarity:  vecSim,
			CombinedScore:     combinedScore,
			Source:             source,
		})
	}

	// Sort by combined score descending
	sort.Slice(combined, func(i, j int) bool {
		return combined[i].CombinedScore > combined[j].CombinedScore
	})

	if len(combined) > cfg.Limit {
		combined = combined[:cfg.Limit]
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
	CreatedAt          string
	ReinforcementCount int
	Weight             int
	LastAccessedAt     *string
	ReferenceID        *string
	Score              float64
}

type vecEntry struct {
	similarity float64
}

// VectorMatch is returned by DatabaseManager.VectorMatch.
type VectorMatch struct {
	ID string
	Content    string
	CreatedAt  string
	Similarity float64
}

// searchFTS5 runs FTS5 MATCH with bm25 scoring.
func searchFTS5(db *sql.DB, query, collection string, limit int) ([]ftsEntry, error) {
	sqlQuery := `
		SELECT m.id, m.content, m.collection, m.tags, m.metadata,
		       m.created_at, m.reinforcement_count, m.weight,
		       m.last_accessed_at, m.reference_id,
		       bm25(memories_fts) AS score
		FROM memories_fts
		JOIN memories m ON memories_fts.rowid = m.rowid
		WHERE memories_fts MATCH ? AND m.deleted_at IS NULL
		  AND (? = '' OR m.collection = ?)
		ORDER BY score
		LIMIT ?`
	rows, err := db.Query(sqlQuery, query, collection, collection, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFTSEntries(rows)
}

// searchLike is the FTS5 fallback using LIKE.
func searchLike(db *sql.DB, query, collection string, limit int) ([]ftsEntry, error) {
	likePat := "%" + strings.ReplaceAll(query, "%", "\\%") + "%"
	sqlQuery := `
		SELECT id, content, collection, tags, metadata,
		       created_at, reinforcement_count, weight,
		       last_accessed_at, reference_id,
		       0.0 AS score
		FROM memories
		WHERE content LIKE ? AND deleted_at IS NULL
		  AND (? = '' OR collection = ?)
		ORDER BY created_at DESC
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
		var nullableTags, nullableMetadata, nullableLastAccessed, nullableRefID sql.NullString
		if err := rows.Scan(&e.ID, &e.Content, &e.Collection, &nullableTags, &nullableMetadata,
			&e.CreatedAt, &e.ReinforcementCount, &e.Weight,
			&nullableLastAccessed, &nullableRefID, &e.Score); err != nil {
			continue
		}
		e.Tags = nullableTags.String
		e.Metadata = nullableMetadata.String
		if nullableLastAccessed.Valid {
			e.LastAccessedAt = &nullableLastAccessed.String
		}
		if nullableRefID.Valid {
			e.ReferenceID = &nullableRefID.String
		}
		results = append(results, e)
	}
	return results, rows.Err()
}

// VectorMatch searches the memories table for vector similarity.
// This uses the existing in-memory approach (JSON embedding column).
// DatabaseManager.VectorMatch is the method — this is a top-level wrapper.
func (dm *DatabaseManager) VectorMatch(collection string, queryEmbedding []float32, limit int) ([]VectorMatch, error) {
	if limit <= 0 {
		limit = 10
	}
	colClause := ""
	args := []interface{}{}
	if collection != "" {
		colClause = "AND collection = ?"
		args = append(args, collection)
	}

	rows, err := dm.SQLDB().Query(`
		SELECT id, content, created_at, embedding
		FROM memories
		WHERE embedding IS NOT NULL AND embedding != 'null' AND deleted_at IS NULL `+colClause,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []VectorMatch
	for rows.Next() {
		var id, content, createdAt, embeddingJSON string
		if err := rows.Scan(&id, &content, &createdAt, &embeddingJSON); err != nil {
			continue
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
