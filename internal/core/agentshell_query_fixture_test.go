// agentshell_query_fixture_test.go — W-014, end to end.
//
// The reported failure was concrete: an agent searched MPM for
// "AgentShell WordPress theme MCP configurator" and got zero results,
// even though MPM held relevant memories. This file reproduces that
// query against a representative fixture under both retrieval
// configurations, and pins the two things the fix relies on:
//
//   - In a lexical-only configuration the zero-hit is real, and the
//     response now SAYS WHY (implicit AND, one term matches nothing)
//     instead of blaming BM25 IDF underflow or a threshold that never
//     applied to lexical rows.
//   - In a semantic-capable configuration the same query returns rows,
//     and every one of them is labelled as having come from the vector
//     arm with no lexical match — so a caller can see that "5 results"
//     and "5 relevant results" are not the same claim.
//
// Everything is hermetic: an in-memory database seeded through the
// production write path, and a deterministic hashing embedder instead of
// a model. The existing hybrid-search vector tests require a real Ollama
// endpoint and SKIP when one is absent, which means the semantic path
// they cover is routinely not executed at all — including in CI. The
// failure mode being fixed is exactly one of "the semantic arm ran and
// its provenance is invisible", so a test that skips when the arm is
// unconfigured cannot pin it.

package internal

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/require"
)

// agentShellQuery is the verbatim query from the report.
const agentShellQuery = "AgentShell WordPress theme MCP configurator"

// fixtureMemories is a representative corpus: memories that genuinely
// describe AgentShell work and genuinely mention AgentShell, MCP,
// configurator and theme — but never the word "WordPress".
//
// This is the shape of the real corpus, which is why the failure is
// confusing: the memories ARE there. Only one query term is absent.
var fixtureMemories = []struct {
	id      string
	content string
}{
	{"mcp-tool-inventory", "The AgentShell MCP surface exposes 50+ tools; use the registry rather than raw REST."},
	{"configurator-transactional-editing", "AgentShell configurator edits run inside begin_transaction / commit_transaction."},
	{"theme-zone-tokens", "AgentShell theme zones are themed through CSS variables on :root."},
	{"mcp-daemon-architecture", "agentshell-mcp-daemon proxies stdio JSON-RPC to the WordPress-less plugin transport."},
	{"registration-bilateral", "Widget registration is bilateral: file definitions merged with config entries."},
}

// seedAgentShellFixture writes fixtureMemories through SaveMemoryWithContext.
//
// The production write path is required, not incidental: a raw INSERT
// never fires the memories_fts triggers, so every row would be invisible
// to the lexical arm and the lexical assertions would pass vacuously.
func seedAgentShellFixture(t *testing.T, dm *DatabaseManager) {
	t.Helper()
	for _, m := range fixtureMemories {
		_, mem, err := dm.SaveMemoryWithContext(
			m.content, "memories", []string{"agentshell"}, 0, "", ActiveContext{},
		)
		require.NoError(t, err, "seed %s", m.id)
		require.NotNil(t, mem)
	}
}

// ── deterministic embedder ────────────────────────────────────────────

const fixtureEmbedDims = 64

// hashingEmbedder is a deterministic bag-of-words embedder: each token
// is hashed to a bucket and counted. Cosine similarity under it
// approximates token overlap, which is exactly the property the fixture
// needs — a query sharing most of a memory's terms scores near it, and
// an unrelated query scores near nothing.
//
// Its real virtue is that it is the same on every machine and needs no
// model, so the semantic assertions execute everywhere instead of
// skipping.
type hashingEmbedder struct{}

func (hashingEmbedder) Name() string { return "fixture:hashing-v1" }

func (hashingEmbedder) Embed(text string) ([]float32, error) {
	vec := make([]float32, fixtureEmbedDims)
	tokens := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, tok := range tokens {
		sum := 0
		for _, c := range tok {
			sum = sum*31 + int(c)
		}
		vec[((sum%fixtureEmbedDims)+fixtureEmbedDims)%fixtureEmbedDims]++
	}
	return vec, nil
}

// pinFixtureEmbedder installs the hashing embedder as the default
// embedding config and seeds an embedding column for every fixture row.
//
// Seeding the stored vectors through the SAME embedder that embeds the
// query is what makes the match real rather than asserted: the fixture
// cannot drift out of sync with itself the way a hand-written vector
// can.
func pinFixtureEmbedder(t *testing.T, dm *DatabaseManager) {
	t.Helper()

	prev := SetEmbedConfigForTest(&EmbeddingConfig{
		Provider:     hashingEmbedder{},
		ProviderName: "fixture:hashing-v1",
	})
	t.Cleanup(func() {
		if prev != nil {
			SetEmbedConfigForTest(prev)
			return
		}
		ResetEmbedConfigForTest()
	})

	for _, m := range fixtureMemories {
		vec, err := hashingEmbedder{}.Embed(m.content)
		require.NoError(t, err)
		embJSON, err := json.Marshal(vec)
		require.NoError(t, err)
		// Matched on content, not on the fixture's id: SaveMemoryWithContext
		// generates the real id, and reaching for the raw column is the
		// only way to seed a vector for a row written through the
		// production path.
		res, err := dm.ExecTracked(
			`UPDATE memories SET embedding = ? WHERE content = ?`,
			0, string(embJSON), m.content)
		require.NoError(t, err)
		n, err := res.RowsAffected()
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "seed embedding for %s", m.id)
	}
}

// ── the two configurations ────────────────────────────────────────────

// TestAgentShellQuery_LexicalOnlyIsZeroAndExplainsItself pins the
// lexical-only half.
//
// Every term except "WordPress" matches the fixture, so the query
// returns nothing purely because BuildFTS5Query joins the terms with a
// space and FTS5 ANDs them. This is a MATCH failure, not a scoring one.
func TestAgentShellQuery_LexicalOnlyIsZeroAndExplainsItself(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	// Pin the disabled case explicitly rather than inheriting whatever
	// the host has configured: this test asserts lexical-only
	// behaviour, and a developer with OllAMA running would otherwise
	// get a different result on the same commit.
	prev := SetEmbedConfigForTest(&EmbeddingConfig{
		Provider: NullProvider{}, ProviderName: "null",
	})
	defer func() {
		if prev != nil {
			SetEmbedConfigForTest(prev)
			return
		}
		ResetEmbedConfigForTest()
	}()

	seedAgentShellFixture(t, dm)

	hits, err := dm.HybridSearchMemories(agentShellQuery, "memories", 5, "local")
	require.NoError(t, err)
	require.Empty(t, hits,
		"the fixture must genuinely have no row containing all five terms — "+
			"if this fails, a fixture memory grew the word WordPress and the "+
			"zero-hit below is no longer testing implicit AND")

	// The term that kills it. "WordPress" tokenizes to `WordPress*`,
	// which matches no row, so the AND cannot be satisfied.
	//
	// Case is preserved by BuildFTS5Query — it does not lowercase,
	// relying on the unicode61 indexer to fold case at match time. The
	// terms are asserted verbatim because the set of AND-ed terms is
	// the premise of the whole test.
	terms := strings.Fields(BuildFTS5Query(agentShellQuery))
	require.Equal(t, []string{"AgentShell*", "WordPress*", "theme*", "MCP*", "configurator*"},
		terms, "tokenization is the premise of this test")

	// Reducing to terms that all match recovers the memories. This is
	// the action the corrected hint recommends, so it must be true.
	recovered, err := dm.HybridSearchMemories("AgentShell configurator", "memories", 5, "local")
	require.NoError(t, err)
	require.NotEmpty(t, recovered,
		"a reduced query must hit; if this is empty the hint's advice is wrong")
}

// TestAgentShellQuery_SemanticOnlyRowsAreLabelled pins the semantic
// half: the same query returns rows, and every one of them is marked as
// having come from the vector arm with no lexical match.
//
// The labelling is the fix. These rows are nearest-neighbours, not
// matches; a caller told only "5 results" would reasonably treat them as
// equivalent to five term matches, which is precisely how an unrelated
// memory gets acted on as if it answered the question.
func TestAgentShellQuery_SemanticOnlyRowsAreLabelled(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	seedAgentShellFixture(t, dm)
	pinFixtureEmbedder(t, dm)

	hits, err := dm.HybridSearchMemories(agentShellQuery, "memories", 5, "local")
	require.NoError(t, err)
	require.NotEmpty(t, hits,
		"with a semantic-capable configuration the query must return rows; "+
			"a zero here means the vector arm never ran")

	for _, h := range hits {
		src, _ := h["retrieval_source"].(string)
		require.Equal(t, "vector", src,
			"row %v carries retrieval_source=%q; with no lexical match the "+
				"only honest label is \"vector\" — labelling these as a match "+
				"is the defect being fixed", h["id"], src)
	}
}

// TestHybridSearchMemories_LexicalHitsAreLabelledLexically is the
// negative control for the test above.
//
// Without it, "every row says vector" would pass just as well if the
// field were hardcoded to "vector". A lexical match must be labelled as
// one.
func TestHybridSearchMemories_LexicalHitsAreLabelledLexically(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	prev := SetEmbedConfigForTest(&EmbeddingConfig{
		Provider: NullProvider{}, ProviderName: "null",
	})
	defer func() {
		if prev != nil {
			SetEmbedConfigForTest(prev)
			return
		}
		ResetEmbedConfigForTest()
	}()

	seedAgentShellFixture(t, dm)

	// "configurator" is present in two fixture rows.
	hits, err := dm.HybridSearchMemories("configurator", "memories", 5, "local")
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	for _, h := range hits {
		src, _ := h["retrieval_source"].(string)
		require.Equal(t, "fts5", src,
			"row %v was matched lexically and must be labelled fts5, got %q",
			h["id"], src)
	}
}
