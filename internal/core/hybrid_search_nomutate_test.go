// hybrid_search_nomutate_test.go — regression net for the 2026-10-13 change
// that removed automatic ChallengeMemory writes from HybridSearch Phase 4.
//
// What changed
//
// Phase 4 compared pairwise cosine similarity to a hardcoded 0.85 and, on
// all three of its branches, called dm.ChallengeMemory — reducing a
// memory's weight as a side effect of a read. Cosine similarity
// establishes that two memories are RELATED; it does not establish that
// they contradict each other. Three consequences were observed:
//
//   - The equal-tier branch fired on scanSet[i] by rank position alone.
//   - Identical and near-identical statements tripped every branch, since
//     similarity is maximal exactly where duplication is maximal.
//   - On the live store, 40 distinct members of a single 75-row
//     near-identical cluster had been challenged for asserting the SAME
//     claim; 30 sat pinned at the weight floor of 1.0.
//
// The invariant these tests pin:
//
//	Semantic similarity must never automatically cause
//	ChallengeMemory to reduce memory weight.
//
// It is deliberately NOT "semantic recall is read-only". EnqueueContradiction
// is still a database write into shared.contradiction_log, and CLI recall
// applies independent access reinforcement. See TestNoShared_FailsSafe.
//
// Every test here uses an isolated temporary database. Nothing touches the
// live store.

package internal

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// ── deterministic embedding provider ────────────────────────────────

// stubProvider returns a fixed vector per text, so tests can control
// pairwise cosine exactly instead of depending on a live model.
type stubProvider struct{ vecs map[string][]float32 }

func (s stubProvider) Embed(text string) ([]float32, error) {
	if v, ok := s.vecs[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil
}
func (s stubProvider) Name() string { return "stub" }

// unitVec builds a 3-dim unit vector from a raw triple, normalising it so
// the cosine between two such vectors is well defined.
func unitVec(a, b, c float32) []float32 {
	n := float32(1.0)
	l := a*a + b*b + c*c
	if l > 0 {
		n = float32(sqrt32(l))
	}
	return []float32{a / n, b / n, c / n}
}

func sqrt32(v float32) float32 {
	if v <= 0 {
		return 0
	}
	// Newton's method; avoids pulling math into the helper surface.
	x := v
	for i := 0; i < 12; i++ {
		x = 0.5 * (x + v/x)
	}
	return x
}

// installStubProvider wires a deterministic provider into the process-wide
// embedding config and returns a restore func.
func installStubProvider(t *testing.T, vecs map[string][]float32) {
	t.Helper()
	cfg := &EmbeddingConfig{
		Source:       EmbeddingSourceProfile,
		ProviderName: "stub",
		Provider:     stubProvider{vecs: vecs},
		Status:       EmbeddingStatusConfigured,
	}
	prev := SetEmbedConfigForTest(cfg)
	t.Cleanup(func() { SetEmbedConfigForTest(prev) })
}

// ── fixtures ────────────────────────────────────────────────────────

// seedNomutateMemory inserts a memory with a caller-supplied vector and metadata.
// Named distinctly from broadcast_test.go's seedMemory to avoid a package-level clash.
func seedNomutateMemory(t *testing.T, dm *DatabaseManager, id, content, metadata string, weight float64, vec []float32) {
	t.Helper()
	emb, err := json.Marshal(vec)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`
		INSERT INTO memories (id, collection, content, metadata, weight, created_at,
		                       embedding, embedding_source, embedding_dimension, embedding_model)
		VALUES (?, 'memories', ?, ?, ?, strftime('%s','now'), ?, 'provider', ?, ?)`,
		0, id, content, metadata, weight, string(emb), len(vec), CurrentEmbeddingFingerprint().Model)
	require.NoError(t, err, "seed memory %s", id)
}

func readWeight(t *testing.T, dm *DatabaseManager, id string) float64 {
	t.Helper()
	var w float64
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT weight FROM memories WHERE id = ?`, id).Scan(&w))
	return w
}

func readStatus(t *testing.T, dm *DatabaseManager, id string) (status, theoryID string, priorWeight float64, hasPrior bool) {
	t.Helper()
	var meta string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT metadata FROM memories WHERE id = ?`, id).Scan(&meta))
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(meta), &m))
	status, _ = m["status"].(string)
	theoryID, _ = m["challenged_theory_id"].(string)
	if pw, ok := m["challenged_prior_weight"].(float64); ok {
		priorWeight, hasPrior = pw, true
	}
	return
}

func countQueue(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM shared.contradiction_log WHERE resolved_at IS NULL`).Scan(&n))
	return n
}

// pair returns two near-parallel 3-dim vectors whose cosine exceeds 0.85,
// which is what Phase 4's gate requires to reach any branch.
func pair() ([]float32, []float32) {
	return unitVec(1, 0, 0), unitVec(1, 0.1, 0)
}

// ── 1. identical memories do not lose weight ────────────────────────

// Identical content is the maximal-similarity case. Before the change this
// tripped the equal-tier branch and slashed scanSet[i] by 1.
func TestNoAutoChallenge_IdenticalMemoriesKeepWeight(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	const c = "the deployment pipeline runs on tuesday mornings"
	a, b := pair()
	installStubProvider(t, map[string][]float32{"deploy": a, c: b})

	seedNomutateMemory(t, dm, "identical-a", c, "{}", 7, a)
	seedNomutateMemory(t, dm, "identical-b", c, "{}", 7, b)

	cfg := DefaultHybridConfig()
	cfg.Limit = 10
	_, err := HybridSearch(dm, "deploy", "memories", cfg)
	require.NoError(t, err)

	require.Equal(t, float64(7), readWeight(t, dm, "identical-a"),
		"identical memory must not lose weight from retrieval")
	require.Equal(t, float64(7), readWeight(t, dm, "identical-b"),
		"identical memory must not lose weight from retrieval")
}

// ── 2. near-duplicates do not lose weight ───────────────────────────

// Differ by a single token. This is the shape that produced the 40 spurious
// challenges in the live uid cluster.
func TestNoAutoChallenge_NearDuplicatesKeepWeight(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	a, b := pair()
	installStubProvider(t, map[string][]float32{"duplicate": a})

	seedNomutateMemory(t, dm, "dup-a", "square root of divided by zero x 0 attempt uid=1", "{}", 9, a)
	seedNomutateMemory(t, dm, "dup-b", "square root of divided by zero x 0 attempt uid=2", "{}", 9, b)

	cfg := DefaultHybridConfig()
	cfg.Limit = 10
	_, err := HybridSearch(dm, "duplicate", "memories", cfg)
	require.NoError(t, err)

	require.Equal(t, float64(9), readWeight(t, dm, "dup-a"),
		"near-duplicate must not lose weight")
	require.Equal(t, float64(9), readWeight(t, dm, "dup-b"),
		"near-duplicate must not lose weight")
}

// ── 3. genuine contradictions are proposed, not penalised ───────────

// A real logical conflict (path A vs path B). The pair must be recorded for
// operator review while BOTH memories keep their weight: MPM has no
// contradiction test, so it cannot know which side is wrong.
func TestNoAutoChallenge_ContradictionProposedButNotPenalised(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	a, b := pair()
	installStubProvider(t, map[string][]float32{"lifecycle": a})

	seedNomutateMemory(t, dm, "path-a", "lifecycle decision: choose lifecycle path A", "{}", 5, a)
	seedNomutateMemory(t, dm, "path-b", "lifecycle decision: choose lifecycle path B", "{}", 5, b)

	cfg := DefaultHybridConfig()
	cfg.Limit = 10
	_, err := HybridSearch(dm, "lifecycle", "memories", cfg)
	require.NoError(t, err)

	require.Equal(t, float64(5), readWeight(t, dm, "path-a"),
		"a genuine contradiction must not auto-penalise either side")
	require.Equal(t, float64(5), readWeight(t, dm, "path-b"),
		"a genuine contradiction must not auto-penalise either side")

	require.Positive(t, countQueue(t, dm),
		"the conflicting pair must be proposed for operator review")
}

// ── 4. all three former branches still enqueue ──────────────────────

// Each branch is exercised for its proposal, since removing the write must
// not silently remove the detection signal.
func TestNoAutoChallenge_AllThreeBranchesStillPropose(t *testing.T) {
	t.Run("state_collision", func(t *testing.T) {
		dm := newTestDMWithShared(t)
		defer dm.Close()

		a, b := pair()
		installStubProvider(t, map[string][]float32{"collision": a})

		seedNomutateMemory(t, dm, "ch-1", "collision witness alpha record", "{}", 4, a)
		seedNomutateMemory(t, dm, "ch-2", "collision witness beta record", "{}", 4, b)
		// Mark one pre-challenged to select the state-collision branch.
		_, err := dm.ExecTracked(`UPDATE memories SET metadata = ? WHERE id = ?`,
			0, `{"status":"challenged","challenged_theory_id":"pre-existing"}`, "ch-1")
		require.NoError(t, err)

		cfg := DefaultHybridConfig()
		cfg.Limit = 10
		_, err = HybridSearch(dm, "collision", "memories", cfg)
		require.NoError(t, err)

		require.Positive(t, countQueue(t, dm), "state-collision branch must propose")
		require.Equal(t, float64(4), readWeight(t, dm, "ch-2"),
			"state-collision branch must not slash the unchallenged side")
	})

	t.Run("provenance_tier", func(t *testing.T) {
		dm := newTestDMWithShared(t)
		defer dm.Close()

		a, b := pair()
		installStubProvider(t, map[string][]float32{"tier": a})

		// Different compute tiers select the provenance branch.
		seedNomutateMemory(t, dm, "tier-hi", "tiered provenance record human authored",
			`{"provenance":{"compute":"human"}}`, 6, a)
		seedNomutateMemory(t, dm, "tier-lo", "tiered provenance record model authored",
			`{"provenance":{"compute":"ephemeral"}}`, 6, b)

		cfg := DefaultHybridConfig()
		cfg.Limit = 10
		_, err := HybridSearch(dm, "tier", "memories", cfg)
		require.NoError(t, err)

		require.Positive(t, countQueue(t, dm), "provenance branch must propose")
		require.Equal(t, float64(6), readWeight(t, dm, "tier-lo"),
			"provenance branch must not slash the losing tier")
		require.Equal(t, float64(6), readWeight(t, dm, "tier-hi"),
			"provenance branch must not slash the winning tier")
	})

	t.Run("equal_tier", func(t *testing.T) {
		dm := newTestDMWithShared(t)
		defer dm.Close()

		a, b := pair()
		installStubProvider(t, map[string][]float32{"equal": a})

		seedNomutateMemory(t, dm, "eq-1", "equal tier first statement", "{}", 3, a)
		seedNomutateMemory(t, dm, "eq-2", "equal tier second statement", "{}", 3, b)

		cfg := DefaultHybridConfig()
		cfg.Limit = 10
		_, err := HybridSearch(dm, "equal", "memories", cfg)
		require.NoError(t, err)

		require.Positive(t, countQueue(t, dm), "equal-tier branch must propose")
		require.Equal(t, float64(3), readWeight(t, dm, "eq-1"),
			"equal-tier branch must not slash by rank position")
	})
}

// ── 5. repeated retrieval does not grow the queue without bound ─────

func TestNoAutoChallenge_RepeatedRetrievalQueueIsBounded(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	a, b := pair()
	installStubProvider(t, map[string][]float32{"repeat": a})

	seedNomutateMemory(t, dm, "rep-1", "repeated retrieval alpha record", "{}", 5, a)
	seedNomutateMemory(t, dm, "rep-2", "repeated retrieval beta record", "{}", 5, b)

	cfg := DefaultHybridConfig()
	cfg.Limit = 10

	_, err := HybridSearch(dm, "repeat", "memories", cfg)
	require.NoError(t, err)
	afterFirst := countQueue(t, dm)
	require.Positive(t, afterFirst, "first retrieval should propose")

	for i := 0; i < 5; i++ {
		_, err = HybridSearch(dm, "repeat", "memories", cfg)
		require.NoError(t, err)
	}
	require.Equal(t, afterFirst, countQueue(t, dm),
		"repeated retrieval of the same pair must not enqueue duplicates")
}

// ── 6. diagnostics cannot penalise ──────────────────────────────────

// handleMpmRetrievalDiagnose reaches HybridSearch via HybridSearchMemories.
// A diagnostic must be observational.
func TestNoAutoChallenge_DiagnosticsDoNotMutate(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	a, b := pair()
	installStubProvider(t, map[string][]float32{"diagnose": a})

	seedNomutateMemory(t, dm, "diag-1", "diagnostic probe alpha record", "{}", 8, a)
	seedNomutateMemory(t, dm, "diag-2", "diagnostic probe beta record", "{}", 8, b)

	before := map[string]float64{
		"diag-1": readWeight(t, dm, "diag-1"),
		"diag-2": readWeight(t, dm, "diag-2"),
	}

	_, err := dm.HybridSearchMemories("diagnose", "memories", 10, "local")
	require.NoError(t, err)

	for id, w := range before {
		require.Equal(t, w, readWeight(t, dm, id),
			"retrieval diagnostics must not change weight for %s", id)
	}
}

// ── 7. the explicit operator path still works ───────────────────────

// The sanctioned challenge path is unchanged: an explicit, audited call
// still reduces weight and records restoration state.
func TestExplicitChallengeMemory_StillWrites(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	seedNomutateMemory(t, dm, "explicit-1", "an explicitly challenged memory", "{}", 10,
		unitVec(0, 0, 1))

	require.NoError(t, dm.ChallengeMemory("explicit-1", 2, "operator-initiated challenge"))

	require.Equal(t, float64(8), readWeight(t, dm, "explicit-1"),
		"explicit challenge must still reduce weight")
	status, theoryID, priorWeight, hasPrior := readStatus(t, dm, "explicit-1")
	require.Equal(t, "challenged", status)
	require.NotEmpty(t, theoryID)
	require.True(t, hasPrior, "restoration info must be recorded")
	require.Equal(t, float64(10), priorWeight,
		"challenged_prior_weight must preserve the pre-challenge weight")
}

// ── 8. pre-existing challenged memories are untouched ───────────────

// The change must not retroactively alter state written before it.
func TestNoAutoChallenge_ExistingChallengedStatePreserved(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	seedNomutateMemory(t, dm, "legacy-1", "a memory challenged before the change", "{}", 1,
		unitVec(0, 0, 1))

	// Simulate the pre-change write: challenged with recorded prior state.
	_, err := dm.ExecTracked(`UPDATE memories SET metadata = ? WHERE id = ?`,
		0, `{"status":"challenged","challenged_theory_id":"legacy collision",`+
			`"challenged_prior_weight":4,"challenged_prior_confidence":0.5}`, "legacy-1")
	require.NoError(t, err)

	a, b := pair()
	installStubProvider(t, map[string][]float32{"legacy": a})
	seedNomutateMemory(t, dm, "legacy-2", "a nearby memory sharing the legacy vocabulary", "{}", 6, b)

	cfg := DefaultHybridConfig()
	cfg.Limit = 10
	_, err = HybridSearch(dm, "legacy", "memories", cfg)
	require.NoError(t, err)

	require.Equal(t, float64(1), readWeight(t, dm, "legacy-1"),
		"an already-challenged memory must not be re-slashed by retrieval")
	status, theoryID, priorWeight, hasPrior := readStatus(t, dm, "legacy-1")
	require.Equal(t, "challenged", status)
	require.Equal(t, "legacy collision", theoryID, "prior challenge note must be preserved")
	require.True(t, hasPrior, "restoration info must survive retrieval")
	require.Equal(t, float64(4), priorWeight)
}

// ── 9. ranking and results are unchanged ────────────────────────────

// The removal must be invisible to retrieval semantics: identical ordering,
// identical scores, identical result set.
func TestNoAutoChallenge_RankingUnchanged(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	vecs := map[string][]float32{}
	for i, c := range []string{
		"ranking probe first statement",
		"ranking probe second statement",
		"ranking probe third statement",
		"ranking probe fourth statement",
	} {
		vecs[c] = unitVec(float32(i+1), 1, 1)
	}
	installStubProvider(t, map[string][]float32{"ranking": unitVec(3, 1, 1)})

	for i, c := range []string{
		"ranking probe first statement",
		"ranking probe second statement",
		"ranking probe third statement",
		"ranking probe fourth statement",
	} {
		seedNomutateMemory(t, dm, fmt.Sprintf("rank-%d", i), c, "{}", 5, vecs[c])
	}

	cfg := DefaultHybridConfig()
	cfg.Limit = 10

	res, err := HybridSearch(dm, "ranking", "memories", cfg)
	require.NoError(t, err)
	require.NotEmpty(t, res)

	ids := make([]string, len(res))
	for i, r := range res {
		ids[i] = r.ID
	}
	scores := make([]float64, len(res))
	for i, r := range res {
		scores[i] = r.CombinedScore
	}

	// Re-run: the second pass must not be perturbed by any side effect of
	// the first.
	res2, err := HybridSearch(dm, "ranking", "memories", cfg)
	require.NoError(t, err)
	require.Equal(t, len(res), len(res2), "result count must be stable across runs")
	for i := range res2 {
		require.Equal(t, ids[i], res2[i].ID, "ordering must be stable")
		require.InDelta(t, scores[i], res2[i].CombinedScore, 1e-9,
			"scores must be stable; a weight write leaking into scoring would show here")
	}
}

// ── 10. missing shared DB fails safe ────────────────────────────────

// Without shared attached, EnqueueContradiction fails soft. The memory must
// not be mutated: losing a review item is the intended failure mode, and
// penalising a memory would not be.
func TestNoShared_FailsSafeWithoutMutation(t *testing.T) {
	dm := newTestDM(t) // no shared DB attached
	defer dm.Close()

	a, b := pair()
	installStubProvider(t, map[string][]float32{"unattached": a})

	seedNomutateMemory(t, dm, "nos-1", "unattached shared probe alpha record", "{}", 5, a)
	seedNomutateMemory(t, dm, "nos-2", "unattached shared probe beta record", "{}", 5, b)

	cfg := DefaultHybridConfig()
	cfg.Limit = 10
	_, err := HybridSearch(dm, "unattached", "memories", cfg)
	require.NoError(t, err, "proposal failure must not fail retrieval")

	require.Equal(t, float64(5), readWeight(t, dm, "nos-1"),
		"an unattached shared DB must not cause a weight mutation")
	require.Equal(t, float64(5), readWeight(t, dm, "nos-2"),
		"an unattached shared DB must not cause a weight mutation")
}
