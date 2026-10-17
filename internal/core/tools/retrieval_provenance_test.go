// retrieval_provenance_test.go — W-014: retrieval mode and fallback
// must be visible to the caller.
//
// The defect this pins: `mpm_memory query` returned rows with no
// statement of how they were found. A caller holding five memories could
// not tell whether all five matched the query's terms or all five were
// embedding near-neighbours, and could not tell whether the semantic arm
// was configured at all. Those two failures are indistinguishable from
// each other and from "the memories do not exist", which is the whole
// failure mode: an agent that cannot distinguish a match from a
// near-neighbour will treat both as a match.
//
// Everything here is a pure-function test over the row maps, so no
// database is required. The end-to-end shape (a real query producing
// vector-only rows against a real corpus) is covered in
// internal/core/agentshell_query_fixture_test.go.

package tools

import (
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// fakeEmbeddingProvider is a deterministic stand-in for a real embedder.
//
// Its only jobs are to report a non-"null" name (so the provenance block
// reports mode=hybrid) and to be installable without a network or a
// local model. Similarity quality is irrelevant here — these tests
// assert what the response SAYS about the retrieval path, not whether
// the embeddings are good.
type fakeEmbeddingProvider struct{ name string }

func (f fakeEmbeddingProvider) Embed(string) ([]float32, error) { return nil, nil }
func (f fakeEmbeddingProvider) Name() string                    { return f.name }

// pinEmbeddingProvider installs a provider for the duration of the test.
//
// SetEmbedConfigForTest is a process-global, so these tests must not
// call t.Parallel — two of them swapping the provider underneath each
// other would make the assertions meaningless in both directions.
func pinEmbeddingProvider(t *testing.T, name string) {
	t.Helper()
	prev := mpminternal.SetEmbedConfigForTest(&mpminternal.EmbeddingConfig{
		Provider:     fakeEmbeddingProvider{name: name},
		ProviderName: name,
	})
	t.Cleanup(func() {
		if prev != nil {
			mpminternal.SetEmbedConfigForTest(prev)
			return
		}
		mpminternal.ResetEmbedConfigForTest()
	})
}

// pinNullProvider is pinEmbeddingProvider for the disabled case.
func pinNullProvider(t *testing.T) {
	t.Helper()
	pinEmbeddingProvider(t, "null")
}

func memRows(sources ...string) []map[string]interface{} {
	rows := make([]map[string]interface{}, 0, len(sources))
	for _, s := range sources {
		rows = append(rows, map[string]interface{}{"id": "x", "retrieval_source": s})
	}
	return rows
}

// TestRetrievalProvenance_ModeFollowsEmbeddingConfig pins that `mode`
// reports whether the semantic arm was available, not merely whether it
// happened to contribute.
//
// This distinction is the point of the field. "No embedding provider
// configured" and "provider configured, nothing matched" both yield a
// lexical-only result set, but the caller's response is completely
// different: the first says "fix your configuration", the second says
// "rephrase the query". Inferring mode from the rows would collapse
// them and re-create the original ambiguity.
func TestRetrievalProvenance_ModeFollowsEmbeddingConfig(t *testing.T) {
	tests := []struct {
		provider string
		wantMode string
	}{
		{"null", "lexical_only"},
		{"ollama:all-minilm", "hybrid"},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			pinEmbeddingProvider(t, tc.provider)
			got := retrievalProvenanceFor(memRows("fts5"))
			if got.Mode != tc.wantMode {
				t.Errorf("mode = %q, want %q (provider %q)",
					got.Mode, tc.wantMode, tc.provider)
			}
		})
	}
}

// TestRetrievalProvenance_SemanticOnly pins the field that carries the
// original defect.
//
// semantic_only is true only when EVERY returned row came from the
// vector arm with no lexical match — exactly the situation where the
// results may be unrelated and should not be acted on as if they were
// matches.
func TestRetrievalProvenance_SemanticOnly(t *testing.T) {
	pinEmbeddingProvider(t, "ollama:all-minilm")

	tests := []struct {
		name    string
		sources []string
		want    bool
	}{
		{"all vector", []string{"vector", "vector"}, true},
		{"one vector", []string{"vector"}, true},
		{"any lexical row clears the flag", []string{"vector", "fts5"}, false},
		{"hybrid counts as lexical", []string{"hybrid"}, false},
		{"all lexical", []string{"fts5", "lexical"}, false},
		// An empty result set has no rows that could be semantic, so the
		// flag is false rather than vacuously true. Asserting true here
		// would tell a caller "everything you got was a guess" about
		// nothing at all.
		{"empty", nil, false},
		// An unrecognised channel is treated as non-lexical, so an
		// unknown source never overstates confidence.
		{"unknown channel is non-lexical", []string{"mystery"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := retrievalProvenanceFor(memRows(tc.sources...))
			if got.SemanticOnly != tc.want {
				t.Errorf("semantic_only = %v, want %v for sources %v",
					got.SemanticOnly, tc.want, tc.sources)
			}
		})
	}
}

// TestRetrievalProvenance_CountsSources pins the per-channel tally and
// its tolerance for rows that predate the field.
//
// Rows with no retrieval_source are the full-text fallback path
// (fallbackLocal in memory_tools.go), which never sets HybridResult.
// Source. Treating an absent value as lexical keeps the count honest
// instead of inventing a channel, and keeps semantic_only from being
// asserted because of a missing field.
func TestRetrievalProvenance_CountsSources(t *testing.T) {
	pinEmbeddingProvider(t, "ollama:all-minilm")

	got := retrievalProvenanceFor([]map[string]interface{}{
		{"id": "a", "retrieval_source": "hybrid"},
		{"id": "b", "retrieval_source": "hybrid"},
		{"id": "c", "retrieval_source": "fts5"},
		{"id": "d", "retrieval_source": "vector"},
		{"id": "e"}, // fallback path: no retrieval_source
	})

	want := map[string]int{"hybrid": 2, "fts5": 1, "vector": 1, "lexical": 1}
	if len(got.Sources) != len(want) {
		t.Fatalf("sources = %v, want %v", got.Sources, want)
	}
	for k, v := range want {
		if got.Sources[k] != v {
			t.Errorf("sources[%q] = %d, want %d", k, got.Sources[k], v)
		}
	}
	if got.SemanticOnly {
		t.Error("semantic_only = true, want false (rows carry lexical matches)")
	}
}

// TestRetrievalProvenance_EchoesScoringConfig pins that the block
// carries the scoring configuration in force.
//
// A caller who reads an unexpected result ordering needs to tell a
// ranking choice (weight, threshold) from a data problem. Without these
// two values the only way to find out is to read the source.
func TestRetrievalProvenance_EchoesScoringConfig(t *testing.T) {
	pinNullProvider(t)

	got := retrievalProvenanceFor(memRows("fts5"))
	defaults := mpminternal.DefaultHybridConfig()
	if got.VectorWeight != defaults.VectorWeight {
		t.Errorf("vector_weight = %v, want the default %v",
			got.VectorWeight, defaults.VectorWeight)
	}
	if got.RetrievalThreshold != defaults.RetrievalThreshold {
		t.Errorf("retrieval_threshold = %v, want the default %v",
			got.RetrievalThreshold, defaults.RetrievalThreshold)
	}
}
