// recall_semantic_contract_test.go — the honest retrieval contract for
// `mpm recall --semantic`.
//
// What was wrong
//
// The semantic path answered two different questions with the same
// output. "There is nothing near this query" and "the semantic arm never
// ran" both produced "No memories found for: <query>" with exit code 0 —
// and under --json they produced a plain-text line where the contract
// promised a JSON envelope, so a machine consumer could not parse the
// result at all. The rows that WERE returned carried no statement about
// what they were: a vector near-neighbour and a verified answer looked
// identical on the wire, and the ranking score read like a confidence.
//
// What is pinned here
//
//  1. An empty semantic recall is valid JSON, exit 0, zero count, an
//     empty ARRAY (never null, never absent), and an explicit status.
//  2. A non-empty semantic recall keeps the same documents in the same
//     order, reports the real count, and says the candidates are
//     unverified.
//  3. A nonsense query does not make the interface claim the returned
//     documents answer the question.
//  4. A legitimate paraphrase is still returned — no threshold was
//     introduced behind the contract.
//  5. The lexical path, the --json alias, collection scoping and the
//     token budget behave exactly as before.
//  6. An embedding-provider failure is an ERROR, never a successful
//     empty result.
//
// Every fixture is hermetic: a disposable workspace (t.TempDir), a
// deterministic stub embedding provider, and no network access.

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

// ── deterministic stub embedding provider ────────────────────────────

// semanticStubModel is the provider name the stub reports. It doubles as
// the embedding_model fingerprint stamped on the seeded rows, because
// VectorMatch filters on the ACTIVE provider's fingerprint: a row labelled
// with any other model is excluded as incomparable.
const semanticStubModel = "stub-semantic-contract"

// semanticStubProvider maps text to a fixed vector so the cosine between
// a query and a stored memory is exactly what a test intends. No live
// model, no network, no dependence on the operator's embedding config.
type semanticStubProvider struct {
	vecs map[string][]float32
	// fail, when set, makes every Embed call fail. It is how the
	// embedding-provider-failure contract is exercised.
	fail error
}

func (p semanticStubProvider) Embed(text string) ([]float32, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	if v, ok := p.vecs[text]; ok {
		return v, nil
	}
	// Unknown text still gets a vector of the SAME dimension — VectorMatch
	// skips any stored row whose length differs from the query vector, so a
	// dimension mismatch would silently empty the result set.
	return p.vecs["*"], nil
}

func (p semanticStubProvider) Name() string { return semanticStubModel }

// semanticVec builds a 3-dim unit vector from a raw triple so the cosine
// between two such vectors is well defined.
func semanticVec(a, b, c float32) []float32 {
	n := float32(math.Sqrt(float64(a*a + b*b + c*c)))
	return []float32{a / n, b / n, c / n}
}

// installSemanticStub wires the deterministic provider into the
// process-wide embedding config and restores the previous one afterwards.
func installSemanticStub(t *testing.T, p semanticStubProvider) {
	t.Helper()
	cfg := &mpminternal.EmbeddingConfig{
		Source:       mpminternal.EmbeddingSourceProfile,
		ProviderName: semanticStubModel,
		Provider:     p,
		Status:       mpminternal.EmbeddingStatusConfigured,
	}
	prev := mpminternal.SetEmbedConfigForTest(cfg)
	t.Cleanup(func() { mpminternal.SetEmbedConfigForTest(prev) })
}

// seedSemanticMemory inserts a memory carrying a stored vector, labelled
// with the active provider's fingerprint so the vector arm can retrieve it.
//
// IDs are kept at 8 characters or fewer because the CLI shortens them in
// the JSON envelope (shortID), and the contract tests compare the
// envelope's ids with the ranker's.
func seedSemanticMemory(t *testing.T, dm *mpminternal.DatabaseManager, collection, id, content string, vec []float32) {
	t.Helper()
	emb, err := json.Marshal(vec)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`
		INSERT INTO memories (id, collection, content, metadata, weight, created_at,
		                       embedding, embedding_source, embedding_dimension, embedding_model)
		VALUES (?, ?, ?, '{}', 1, strftime('%s','now'), ?, 'provider', ?, ?)`,
		0, id, collection, content, string(emb), len(vec), semanticStubModel)
	require.NoError(t, err, "seed memory %s", id)
}

// semanticEnvelope is the JSON contract `mpm recall --semantic --json`
// emits. Decoding into a struct with typed fields is what proves the
// envelope is a real object with the promised field types.
type semanticEnvelope struct {
	Query      string `json:"query"`
	SearchMode string `json:"search_mode"`
	Count      int    `json:"count"`
	Status     string `json:"retrieval_status"`
	Memories   []struct {
		ID               string  `json:"id"`
		Content          string  `json:"content"`
		Score            float64 `json:"score"`
		VectorSimilarity float64 `json:"vector_similarity"`
		Source           string  `json:"source"`
	} `json:"memories"`
}

// decodeSemanticEnvelope parses the --json output of the semantic path.
func decodeSemanticEnvelope(t *testing.T, out string) semanticEnvelope {
	t.Helper()
	var env semanticEnvelope
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &env),
		"semantic recall must emit parseable JSON; got: %s", out)
	return env
}

// ── the corpus used by the contract tests ────────────────────────────

// The near vector and the far vector are orthogonal, so a query mapped to
// "near" has cosine 1.0 with the paraphrase memory and 0.0 with the
// unrelated one.
var (
	semanticNear = semanticVec(1, 0, 0)
	semanticFar  = semanticVec(0, 0, 1)
)

// semanticParaphraseQuery has ZERO token overlap with the memory it must
// retrieve — that is what makes it a paraphrase and not a keyword match.
const semanticParaphraseQuery = "why do oversized attachments fail to upload"

// semanticParaphraseContent deliberately shares no token with the query.
const semanticParaphraseContent = "pipeline handling of bulky payloads"

// semanticNonsenseQuery is unrelated to everything in the corpus.
const semanticNonsenseQuery = "quarterly revenue forecast variance"

// semanticCorpus seeds three memories: a paraphrase target, an unrelated
// memory, and a mid-similarity memory.
func semanticCorpus(t *testing.T, dm *mpminternal.DatabaseManager) {
	t.Helper()
	installSemanticStub(t, semanticStubProvider{
		vecs: map[string][]float32{
			semanticParaphraseQuery: semanticNear,
			semanticNonsenseQuery:   semanticNear,
			"*":                     semanticFar,
		},
	})
	seedSemanticMemory(t, dm, "memories", "sempara", semanticParaphraseContent, semanticNear)
	seedSemanticMemory(t, dm, "memories", "semunrel", "quarterly revenue variance reporting", semanticFar)
	seedSemanticMemory(t, dm, "memories", "semmid", "checkpoint retention policy window", semanticVec(0.6, 0.8, 0))
}

// ── 1. empty semantic recall ─────────────────────────────────────────

// An empty semantic recall must be a valid JSON envelope with a zero
// count, an empty ARRAY and an unambiguous status — and it must exit 0.
//
// Before the fix the empty path printed a plain-text line even under
// --json, so a machine consumer could not parse the result at all, and
// the plain text said "No memories found", which asserts something about
// the corpus that a ranked-candidate search never established.
func TestRecallSemantic_EmptyResultIsValidJSONWithExplicitStatus(t *testing.T) {
	recallWorkspace(t)
	installSemanticStub(t, semanticStubProvider{
		vecs: map[string][]float32{"*": semanticFar},
	})

	code, out, errOut := captureRecall(t, "recall", "--semantic", "--json", "a query nothing matches")
	require.Equal(t, 0, code, "an empty recall is a successful recall: %s (stderr: %s)", out, errOut)

	env := decodeSemanticEnvelope(t, out)
	require.Equal(t, 0, env.Count, "count must report zero for an empty result")
	require.NotNil(t, env.Memories, "memories must be an array, not null")
	require.Empty(t, env.Memories)
	require.Equal(t, mpminternal.RetrievalStatusNoCandidates, env.Status,
		"an empty result must say so explicitly")
	require.Equal(t, "hybrid", env.SearchMode, "search_mode keeps its existing meaning")

	require.Contains(t, out, `"memories":[]`,
		"the empty array must be rendered as an array, not as null or an absent key")
	require.NotContains(t, out, "No memories found",
		"the JSON path must not emit plain text")
	require.NotContains(t, out, "No candidates returned",
		"the JSON path must not emit plain text")
}

// The -j alias must produce the same envelope as --json, including the
// new status field. The alias is long-standing and load-bearing for
// scripts.
func TestRecallSemantic_EmptyResultHonoursTheJSONAlias(t *testing.T) {
	recallWorkspace(t)
	installSemanticStub(t, semanticStubProvider{
		vecs: map[string][]float32{"*": semanticFar},
	})

	code, out, errOut := captureRecall(t, "recall", "--semantic", "-j", "a query nothing matches")
	require.Equal(t, 0, code, "stderr: %s", errOut)
	env := decodeSemanticEnvelope(t, out)
	require.Equal(t, mpminternal.RetrievalStatusNoCandidates, env.Status)
	require.Equal(t, 0, env.Count)
}

// ── 2. non-empty semantic recall ─────────────────────────────────────

// A non-empty semantic recall must keep the same documents in the same
// order, report the real count, and say the candidates are unverified.
func TestRecallSemantic_NonEmptyResultReportsCountAndUnverifiedStatus(t *testing.T) {
	dm := recallWorkspace(t)
	semanticCorpus(t, dm)

	code, out, errOut := captureRecall(t, "recall", "--semantic", "--json", semanticParaphraseQuery)
	require.Equal(t, 0, code, "stderr: %s", errOut)

	env := decodeSemanticEnvelope(t, out)
	require.NotEmpty(t, env.Memories, "the corpus has three candidates")
	require.Equal(t, len(env.Memories), env.Count,
		"count must be the number of returned entries")
	require.Equal(t, mpminternal.RetrievalStatusCandidatesUnverified, env.Status,
		"returned rows are ranked candidates whose relevance is not established")

	// The documents and their order must be exactly what the ranker
	// produced — the contract adds fields, it does not re-rank.
	cfg := mpminternal.DefaultHybridConfig()
	cfg.Limit = 15
	cfg.VectorWeight = 0.5
	expected, err := mpminternal.HybridSearch(dm, semanticParaphraseQuery, "memories", cfg)
	require.NoError(t, err)
	require.Len(t, expected, len(env.Memories),
		"the envelope must carry every ranked row, in the ranked order")
	for i, want := range expected {
		require.Equal(t, want.ID, env.Memories[i].ID,
			"row %d must keep the ranker's order", i)
	}
}

// ── 3. nonsense queries ─────────────────────────────────────────────

// A nonsense query that still returns rows must not make the interface
// claim the rows answer the question. This is the defect the issue is
// about: the old envelope said nothing about what the rows were, and the
// score read like a confidence.
//
// The check is structural — the envelope must not carry a field that
// asserts answerability or a calibrated probability.
func TestRecallSemantic_NonsenseQueryDoesNotAssertAnswerability(t *testing.T) {
	dm := recallWorkspace(t)
	semanticCorpus(t, dm)

	code, out, errOut := captureRecall(t, "recall", "--semantic", "--json", semanticNonsenseQuery)
	require.Equal(t, 0, code, "stderr: %s", errOut)

	env := decodeSemanticEnvelope(t, out)
	require.Equal(t, mpminternal.RetrievalStatusCandidatesUnverified, env.Status,
		"a nonsense query that still returns rows must not be reported as answered")

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &raw))
	for _, forbidden := range []string{
		"confidence", "probability", "answerability", "verified", "is_answer",
		"answered", "relevance_established", "answer",
	} {
		require.NotContains(t, raw, forbidden,
			"the envelope must not carry a field that asserts the rows answer the question")
	}
}

// The human-readable semantic output must say the same thing the JSON
// envelope says: these are ranked candidates, not verified answers.
func TestRecallSemantic_HumanOutputFramesResultsAsUnverifiedCandidates(t *testing.T) {
	dm := recallWorkspace(t)
	semanticCorpus(t, dm)

	code, out, errOut := captureRecall(t, "recall", "--semantic", semanticParaphraseQuery)
	require.Equal(t, 0, code, "stderr: %s", errOut)

	require.Contains(t, out, "Ranked candidates",
		"the human output must frame the results as ranked candidates")
	require.Contains(t, out, "relevance not established",
		"the human output must say relevance was not established")
	require.NotContains(t, out, "verified answer",
		"the human output must not claim the rows are verified answers")
}

// An empty human-readable semantic recall says "no candidates", not "no
// memories" — the search returns ranked candidates, and "no memories
// found" asserts something about the corpus that was never established.
func TestRecallSemantic_HumanEmptyOutputDoesNotClaimMemoriesAreAbsent(t *testing.T) {
	recallWorkspace(t)
	installSemanticStub(t, semanticStubProvider{
		vecs: map[string][]float32{"*": semanticFar},
	})

	code, out, errOut := captureRecall(t, "recall", "--semantic", "a query nothing matches")
	require.Equal(t, 0, code, "stderr: %s", errOut)
	require.Contains(t, out, "No candidates returned for:",
		"an empty semantic recall reports that no candidates were returned")
	require.NotContains(t, out, "No memories found",
		"an empty semantic recall must not assert that no memories exist")
}

// ── 4. legitimate paraphrases ────────────────────────────────────────

// The contract must not smuggle a threshold back in. A legitimate
// paraphrase — no token overlap with the query — must still be returned,
// because the Phase A.3 measurements showed that every similarity floor
// costs more legitimate paraphrases than it removes nonsense queries.
func TestRecallSemantic_ParaphraseResultsRemainAvailable(t *testing.T) {
	dm := recallWorkspace(t)
	semanticCorpus(t, dm)

	code, out, errOut := captureRecall(t, "recall", "--semantic", "--json", semanticParaphraseQuery)
	require.Equal(t, 0, code, "stderr: %s", errOut)

	env := decodeSemanticEnvelope(t, out)
	require.NotEmpty(t, env.Memories)
	require.Equal(t, "sempara", env.Memories[0].ID,
		"the paraphrase target must be retrieved and must rank first")
	require.Positive(t, env.Memories[0].VectorSimilarity,
		"the paraphrase must be retrieved through the vector channel")
}

// ── 5. compatibility with the existing behaviour ─────────────────────

// The lexical path's envelope is NOT part of the new contract: it says
// "memories", and it matched on the query's terms. It must keep working
// exactly as before — same fields, no retrieval_status.
func TestRecallSemantic_LexicalRecallEnvelopeIsUnchanged(t *testing.T) {
	dm := recallWorkspace(t)
	seedRecallWords(t, dm, "alpha", "beta")

	code, out, errOut := captureRecall(t, "recall", "--json", "flagordertoken")
	require.Equal(t, 0, code, "stderr: %s", errOut)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &raw))
	require.Contains(t, raw, "query")
	require.Contains(t, raw, "memories")
	require.NotContains(t, raw, "retrieval_status",
		"the retrieval_status contract is scoped to the semantic path")

	var payload struct {
		Memories []json.RawMessage `json:"memories"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &payload))
	require.Len(t, payload.Memories, 2, "lexical recall must still return its rows")
}

// Collection scoping must keep working on the semantic path.
func TestRecallSemantic_CollectionScopingIsUnchanged(t *testing.T) {
	dm := recallWorkspace(t)
	semanticCorpus(t, dm)
	seedSemanticMemory(t, dm, "other-collection", "semother",
		semanticParaphraseContent, semanticNear)

	code, out, errOut := captureRecall(t, "recall", "--semantic", "--json",
		"--collection", "other-collection", semanticParaphraseQuery)
	require.Equal(t, 0, code, "stderr: %s", errOut)
	env := decodeSemanticEnvelope(t, out)
	require.Len(t, env.Memories, 1)
	require.Equal(t, "semother", env.Memories[0].ID,
		"--collection must scope the semantic result set")
	require.Equal(t, 1, env.Count)

	// The default collection must not see the other collection's rows.
	_, defOut, _ := captureRecall(t, "recall", "--semantic", "--json", semanticParaphraseQuery)
	defEnv := decodeSemanticEnvelope(t, defOut)
	for _, m := range defEnv.Memories {
		require.NotEqual(t, "semother", m.ID,
			"a row from another collection must not appear in the default scope")
	}
}

// The token budget must keep truncating the lexical result set exactly as
// before. It is deliberately NOT applied on the semantic path: dropping
// rows there would remove legitimate results, which the contract forbids.
func TestRecallSemantic_TokenBudgetBehaviourIsUnchanged(t *testing.T) {
	dm := recallWorkspace(t)
	seedRecallWords(t, dm, "alpha")

	code, out, errOut := captureRecall(t, "recall", "--json", "--token-budget", "1", "flagordertoken")
	require.Equal(t, 0, code, "stderr: %s", errOut)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &raw))
	require.Contains(t, raw, "truncated",
		"the lexical token budget must still truncate and say so")
	require.Equal(t, "true", string(raw["truncated"]))
}

// ── 6. embedding-provider failure ────────────────────────────────────

// An embedding-provider failure must be an ERROR. Before the fix it was
// swallowed inside the hybrid search, so a broken provider produced
// "No memories found" with exit code 0 — indistinguishable from a corpus
// that genuinely holds nothing near the query.
//
// The failure must be reported even when the lexical channel DID produce
// rows: a degraded result set must not be presented as a complete
// semantic answer.
func TestRecallSemantic_EmbeddingProviderFailureIsAnError(t *testing.T) {
	dm := recallWorkspace(t)
	seedRecallWords(t, dm, "alpha") // the lexical channel HAS a row
	installSemanticStub(t, semanticStubProvider{
		vecs: map[string][]float32{"*": semanticNear},
		fail: fmt.Errorf("connection refused"),
	})

	code, out, errOut := captureRecall(t, "recall", "--semantic", "--json", "flagordertoken")
	require.NotEqual(t, 0, code,
		"an embedding-provider failure must not exit 0: stdout=%s", out)
	require.Contains(t, errOut, "connection refused",
		"the error must name the underlying failure")
	require.Contains(t, strings.ToLower(errOut), "semantic search failed",
		"the error must say which stage failed")
	require.NotContains(t, out, `"retrieval_status"`,
		"a failed search must not emit a successful retrieval envelope")
	require.NotContains(t, out, `"memories"`,
		"a failed search must not emit result JSON")
}

// The same must hold when the lexical channel has nothing either: a
// provider failure is still an error, never a "no candidates" result.
func TestRecallSemantic_EmbeddingProviderFailureIsNotAnEmptyResult(t *testing.T) {
	recallWorkspace(t)
	installSemanticStub(t, semanticStubProvider{
		vecs: map[string][]float32{"*": semanticNear},
		fail: fmt.Errorf("dial tcp 127.0.0.1:11434: connect: connection refused"),
	})

	code, out, errOut := captureRecall(t, "recall", "--semantic", "--json", "a query nothing matches")
	require.NotEqual(t, 0, code,
		"a provider failure must not be reported as an empty recall: stdout=%s", out)
	require.NotContains(t, out, "no_candidates",
		"a provider failure must never be reported as 'no candidates returned'")
	require.Contains(t, strings.ToLower(errOut), "semantic search failed")
}
