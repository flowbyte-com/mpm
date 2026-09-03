package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	internal "github.com/flowbyte-com/mpm-core"
)

// =============================================================================
// forge_dedup.go — capability proposal dedup (spec §3.2 step 8)
//
// Before a proposal is inserted, the Forge computes an embedding
// for the proposed purpose and compares it against every
// non-retired, non-rolled-back capability's stored embedding. If
// any cosine similarity is >= threshold (default 0.92), the
// proposal is rejected with ErrDedupMatch — the agent is forced
// to either use the existing tool, or declare replaces_id to
// fork (which adds the existing tool to the skip-list and creates
// a new revision in the lineage chain).
//
// Why a fixed threshold (not adaptive):
//
//   * 0.92 was chosen because HashEmbed and Ollama nomic-embed
//     both place near-identical purpose statements above 0.95
//     and reworded duplicates in the 0.85-0.92 range. Below 0.92,
//     the proposals were either legitimately different OR had
//     materially different contracts.
//   * Operators can tune via the Forge's config; this constant
//     is the default and the value tests assume.
//
// The dedup is a single-shot comparison, not an indexed search.
// Capability corpora are small (dozens to low hundreds). Adding
// an ANN index would be premature; revisit when corpus > 10k.
// =============================================================================

// DedupResult is the outcome of one dedup check. Matched=false
// means no duplicate was found (or all matches were in the skip
// list); the proposal is free to proceed.
type DedupResult struct {
	Matched     bool
	MatchedID   string  // capability id of the duplicate (when Matched=true)
	MatchedName string  // human-readable name (for the rejection message)
	Similarity  float32 // cosine similarity in [-1, 1] of the best match
}

// DedupChecker is the interface; the Forge uses this so tests
// can inject a fake and avoid the embedding provider.
type DedupChecker interface {
	// Check returns the first match above the threshold, or an
	// empty DedupResult if no match. The skip list contains
	// capability IDs to exclude — used for the replaces_id
	// fork-flow bypass.
	Check(ctx context.Context, purpose string, skipCapabilityIDs []string) (DedupResult, error)
}

// DefaultDedup is the production dedup. It uses the package-level
// internal.EmbedText and scans the Store for non-retired
// capabilities to compare against.
//
// Tests inject a custom embedder to make the check deterministic.
type DefaultDedup struct {
	store     *Store
	embedder  func(string) ([]float32, error)
	threshold float32
}

// DefaultDedupConfig returns the production defaults. Threshold
// 0.92 is the spec §3.2 default; embedder is the package-level
// internal.EmbedText.
func DefaultDedupConfig() (func(string) ([]float32, error), float32) {
	return internal.EmbedText, 0.92
}

// NewDefaultDedup builds a DefaultDedup. Zero-value embedder or
// threshold fall back to the defaults.
func NewDefaultDedup(store *Store, embedder func(string) ([]float32, error), threshold float32) *DefaultDedup {
	defEmbedder, defThreshold := DefaultDedupConfig()
	if embedder == nil {
		embedder = defEmbedder
	}
	if threshold <= 0 {
		threshold = defThreshold
	}
	return &DefaultDedup{
		store:     store,
		embedder:  embedder,
		threshold: threshold,
	}
}

// Check runs the dedup. Returns Matched=false on no match.
// Returns Matched=true with the best match when similarity >= threshold.
// The skip list is honoured: any candidate ID in the list is
// excluded from the comparison (used for the replaces_id bypass
// — the agent has explicitly declared "I am forking this one").
func (d *DefaultDedup) Check(_ context.Context, purpose string, skipCapabilityIDs []string) (DedupResult, error) {
	if purpose == "" {
		return DedupResult{}, fmt.Errorf("capability: dedup: purpose is required")
	}
	if d.store == nil {
		return DedupResult{}, fmt.Errorf("capability: dedup: store is required")
	}

	// Build skip set for O(1) lookup.
	skip := make(map[string]bool, len(skipCapabilityIDs))
	for _, id := range skipCapabilityIDs {
		skip[id] = true
	}

	// Embed the proposed purpose.
	proposed, embedErr := d.embedder(purpose)
	if embedErr != nil || len(proposed) == 0 {
		// Embedding failed or returned empty. Treat as no match —
		// better to over-accept than to block every proposal when
		// the embedding service is down.
		return DedupResult{}, nil
	}

	// Pull all non-retired, non-rolled-back capability embeddings.
	refs, err := d.store.ListNonRetiredEmbeddings(0)
	if err != nil {
		return DedupResult{}, fmt.Errorf("capability: dedup: list: %w", err)
	}

	// Find the best match above threshold.
	best := DedupResult{}
	for _, ref := range refs {
		if skip[ref.ID] {
			continue
		}
		if len(ref.Embedding) == 0 {
			// No stored embedding — likely a pre-embedding
			// migration row. Skip rather than fail the check;
			// a tool with no embedding was never visible to
			// dedup anyway.
			continue
		}
		var stored []float32
		if err := json.Unmarshal(ref.Embedding, &stored); err != nil {
			// Malformed embedding in the DB. Skip this row;
			// failing the proposal because of a corrupt row
			// would be a denial-of-service vector.
			continue
		}
		sim := cosineSimilarity(proposed, stored)
		if sim >= d.threshold && sim > best.Similarity {
			best = DedupResult{
				Matched:     true,
				MatchedID:   ref.ID,
				MatchedName: ref.Name,
				Similarity:  sim,
			}
		}
	}
	return best, nil
}

// cosineSimilarity computes the cosine of the angle between two
// float32 vectors. Returns 0 if either vector is zero-length
// (the empty-vector case is the natural "no information" baseline;
// treating it as 0 similarity avoids spurious matches when one
// side failed to embed).
//
// Vectors of different lengths: pad the shorter with zeros. In
// practice the HashEmbed fallback is always 256 dims and Ollama
// nomic-embed is 768; we just use the shorter length's overlap
// which slightly underestimates similarity (conservative — better
// to miss a duplicate than false-positive a fork).
func cosineSimilarity(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var dot, magA, magB float64
	for i := 0; i < n; i++ {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		magA += ai * ai
		magB += bi * bi
	}
	if magA == 0 || magB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(magA) * math.Sqrt(magB)))
}

// =============================================================================
// FakeDedup — test-only DedupChecker.
// =============================================================================

// FakeDedup is a programmable DedupChecker. Tests preload the
// result for a given purpose and the Forge sees the canned
// answer instead of running the real check.
type FakeDedup struct {
	// Result is returned by every Check call. Tests mutate this
	// between calls to simulate different scenarios.
	Result DedupResult
	// Error is returned alongside Result. Use to simulate
	// infrastructure failures.
	Error error
	// Calls records every (purpose, skip) invocation for assertions.
	Calls []FakeDedupCall
}

// FakeDedupCall records one invocation.
type FakeDedupCall struct {
	Purpose     string
	SkipIDs     []string
}

// Check returns the canned Result/Error and records the call.
func (f *FakeDedup) Check(_ context.Context, purpose string, skip []string) (DedupResult, error) {
	cp := append([]string(nil), skip...)
	f.Calls = append(f.Calls, FakeDedupCall{Purpose: purpose, SkipIDs: cp})
	return f.Result, f.Error
}
