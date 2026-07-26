// retrieval_ranker.go — Pluggable ranking interface for Adaptive Retrieval.
//
// Search queries LEFT JOIN retrieval_metadata so the stats are
// available to rankers, but DefaultRanker returns the FTS score
// unchanged — today's behavior is preserved bit-for-bit. Future
// rankers (reciprocal-rank blending, success-rate weighting, etc.)
// drop in here without touching the search path.
//
// CRITICAL: the FTS ORDER BY rank expression is owned by the SQL
// query, NOT the ranker. The ranker adjusts per-row scores AFTER the
// ORDER BY has already decided ordering. Mixing them would mean a
// future ranker silently re-orders results.

package internal

// RetrievalMetadata is a snapshot of retrieval stats for a single
// node. Decoupled from the retrieval_metadata table row so the
// ranker can be unit-tested without a database.
type RetrievalMetadata struct {
	NodeID          string
	NodeType        string
	ReuseCount      int
	LastRetrievedAt string // ISO-8601 string; empty when never retrieved
	SuccessCount    int
}

// RetrievalRanker combines an FTS score with retrieval metadata to
// produce the final ranking score. Pure function: no DB access, no
// side effects, deterministic for the same inputs.
type RetrievalRanker interface {
	Score(ftsScore float64, meta RetrievalMetadata) float64
}

// DefaultRanker returns the FTS score unchanged. Today's behavior
// preserved exactly. The interface exists so future implementations
// can plug in via setRanker() without search-path changes.
type DefaultRanker struct{}

// Score returns ftsScore verbatim. The meta argument is ignored.
func (DefaultRanker) Score(ftsScore float64, _ RetrievalMetadata) float64 {
	return ftsScore
}

// ranker is the package-level ranker singleton. Process-local; not
// persisted. Future personalization (per-agent ranker preference)
// would extend this with an agent-id key.
var ranker RetrievalRanker = DefaultRanker{}

// SetRanker replaces the global ranker. Used by tests and future
// personalization hooks. Returns the previous ranker so callers can
// restore it.
func SetRanker(r RetrievalRanker) RetrievalRanker {
	prev := ranker
	ranker = r
	return prev
}

// CurrentRanker returns the active ranker. The search path uses this
// to score individual rows after the SQL ORDER BY has already decided
// their order.
func CurrentRanker() RetrievalRanker {
	return ranker
}