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
//	ChallengeMemory to reduce memory weight, and must never
//	cause a read to persist a contradiction claim.
//
// This IS now "semantic retrieval writes nothing". The 2026-10-13 change
// removed the ChallengeMemory calls (removal of auto-challenge) and then, in
// the same day, removed the EnqueueContradiction calls from the retrieval
// path (moved to an explicit --propose). Both were writes hanging off a read.
//
// What is deliberately still NOT covered here: CLI recall applies access
// reinforcement (+0.5 per returned row) as a separate, unrelated mechanism.
// That is not similarity-driven and is not pinned here.
//
// Every test here uses an isolated temporary database. Nothing touches the
// live store.

package internal

import (
	"encoding/json"
	"fmt"
	"strings"
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

// challengedIDs returns the sorted set of memory IDs whose metadata
// carries a challenge flag. Used to assert that a code path ADDED none,
// which is distinct from asserting the set is empty — several fixtures
// seed a pre-existing flag deliberately.
func challengedIDs(t *testing.T, dm *DatabaseManager) []string {
	t.Helper()
	rows, err := dm.SQLDB().Query(
		`SELECT id FROM memories WHERE COALESCE(metadata,'') LIKE '%challenged%' ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
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

// ── 3. genuine contradictions are NOT penalised, and not auto-queued ──

// A real logical conflict (path A vs path B).
//
// This test originally asserted the pair was automatically queued for
// review. It no longer is, and that is the point of the 2026-10-13 change:
// even a pair that really does conflict must not be persisted by a READ.
// MPM cannot establish logical contradiction from cosine similarity, so
// auto-filing a "contradiction" asserted a fact the system does not have.
//
// The pair is still discoverable — by the explicit path, below — and
// neither side is ever penalised.
func TestNoAutoChallenge_ContradictionNotPenalisedAndNotAutoQueued(t *testing.T) {
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

	// The read queued nothing, however genuine the conflict looks.
	require.Zero(t, countQueue(t, dm),
		"a read must not persist a contradiction claim it cannot substantiate")

	// The pair remains discoverable on the explicit path.
	entries, err := dm.LoadSimilarityScanEntries(0, "")
	require.NoError(t, err)
	cands := FindSimilarityCandidates(entries)
	require.Len(t, cands, 1, "explicit scan still surfaces the conflicting pair")
	require.ElementsMatch(t, []string{"path-a", "path-b"},
		[]string{cands[0].MemoryA, cands[0].MemoryB})
	require.Contains(t, cands[0].Evidence, "unverified similarity candidate")
}

// ── 4. all three former branches still enqueue ──────────────────────

// Each branch is exercised for its proposal, since removing the write must
// not silently remove the detection signal.
// TestNoAutoEnqueue_RetrievalNeverProposes_ExplicitScanStillFindsAllThree
// pins the 2026-10-13 contract split, which is the whole point of moving
// discovery out of HybridSearch:
//
//	retrieval must not write, but discovery must not die with it.
//
// The previous version of this test asserted the OPPOSITE for the queue —
// that HybridSearch enqueued on all three branches. That assertion encoded
// the behaviour this change removes, so it is inverted rather than deleted:
// each branch is now checked twice, once for "retrieval wrote nothing" and
// once for "the explicit path still finds the pair".
func TestNoAutoEnqueue_RetrievalNeverProposes_ExplicitScanStillFindsAllThree(t *testing.T) {
	// each case seeds a pair that lands on one of the three branches.
	cases := []struct {
		name     string
		query    string
		seed     func(t *testing.T, dm *DatabaseManager, a, b []float32)
		wantKind CandidateKind
		wantIDs  []string
	}{
		{
			name:  "state_collision",
			query: "collision",
			seed: func(t *testing.T, dm *DatabaseManager, a, b []float32) {
				seedNomutateMemory(t, dm, "ch-1", "collision witness alpha record", "{}", 4, a)
				seedNomutateMemory(t, dm, "ch-2", "collision witness beta record", "{}", 4, b)
				_, err := dm.ExecTracked(`UPDATE memories SET metadata = ? WHERE id = ?`,
					0, `{"status":"challenged","challenged_theory_id":"pre-existing"}`, "ch-1")
				require.NoError(t, err)
			},
			wantKind: CandidateStateCollision,
			wantIDs:  []string{"ch-1", "ch-2"},
		},
		{
			name:  "provenance_tier",
			query: "tier",
			seed: func(t *testing.T, dm *DatabaseManager, a, b []float32) {
				seedNomutateMemory(t, dm, "tier-hi", "tiered provenance record human authored",
					`{"provenance":{"compute":"human"}}`, 6, a)
				seedNomutateMemory(t, dm, "tier-lo", "tiered provenance record model authored",
					`{"provenance":{"compute":"ephemeral"}}`, 6, b)
			},
			wantKind: CandidateProvenanceTier,
			wantIDs:  []string{"tier-hi", "tier-lo"},
		},
		{
			name:  "equal_tier",
			query: "equal",
			seed: func(t *testing.T, dm *DatabaseManager, a, b []float32) {
				seedNomutateMemory(t, dm, "eq-1", "equal tier first statement", "{}", 3, a)
				seedNomutateMemory(t, dm, "eq-2", "equal tier second statement", "{}", 3, b)
			},
			wantKind: CandidateEqualTier,
			wantIDs:  []string{"eq-1", "eq-2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := newTestDMWithShared(t)
			defer dm.Close()

			a, b := pair()
			installStubProvider(t, map[string][]float32{tc.query: a})
			tc.seed(t, dm, a, b)
			challengedBefore := challengedIDs(t, dm)

			cfg := DefaultHybridConfig()
			cfg.Limit = 10
			_, err := HybridSearch(dm, tc.query, "memories", cfg)
			require.NoError(t, err)

			// 1. Retrieval wrote nothing. Not "fewer than before" —
			//    nothing at all, which is the new guarantee.
			require.Zero(t, countQueue(t, dm),
				"semantic retrieval must not add rows to the persistent queue")

			// 2. Discovery survived the move. The explicit path finds the
			//    same pair the removed scan would have.
			entries, err := dm.LoadSimilarityScanEntries(0, "")
			require.NoError(t, err)
			cands := FindSimilarityCandidates(entries)
			require.Len(t, cands, 1, "explicit scan must still find the %s pair", tc.name)
			require.Equal(t, tc.wantKind, cands[0].Kind)
			require.ElementsMatch(t, tc.wantIDs, []string{cands[0].MemoryA, cands[0].MemoryB})

			// 3. And the candidate is labelled as unverified, not as a
			//    contradiction that was found.
			require.Contains(t, cands[0].Evidence, "unverified similarity candidate")

			// 4. Enqueuing is a separate, explicit step.
			n, err := dm.EnqueueSimilarityCandidates(cands)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			require.Equal(t, 1, countQueue(t, dm))

			// 5. No memory's challenge state changed. Snapshot rather than
			//    assert "none are challenged": the state_collision case
			//    deliberately seeds a PRE-EXISTING flag, and the guarantee
			//    is that nothing was ADDED, not that the column is empty.
			require.Equal(t, challengedBefore, challengedIDs(t, dm),
				"neither retrieval nor candidate generation may change any memory's challenge state")
		})
	}
}

// ── 5. repeated retrieval does not grow the queue without bound ─────

// Two properties, both required, and the second is now the stronger one.
//
// Originally this test asserted that the FIRST retrieval queued one row and
// that repeats added no duplicates. The first half is no longer true and
// must not be: a read queues nothing at all. Boundedness is therefore
// asserted twice over — once for retrieval, which writes nothing, and once
// for the explicit scan, which relies on pair-level idempotence.
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
	require.Zero(t, countQueue(t, dm), "the first retrieval must not queue anything")

	for i := 0; i < 5; i++ {
		_, err = HybridSearch(dm, "repeat", "memories", cfg)
		require.NoError(t, err)
	}
	require.Zero(t, countQueue(t, dm),
		"repeated retrieval must not grow the queue, even not by one row")

	// The explicit path IS allowed to write, and is idempotent: running the
	// same scan repeatedly must not duplicate rows. This is what keeps an
	// operator re-running a review from flooding the worklist.
	entries, err := dm.LoadSimilarityScanEntries(0, "")
	require.NoError(t, err)
	cands := FindSimilarityCandidates(entries)
	require.Len(t, cands, 1)

	n, err := dm.EnqueueSimilarityCandidates(cands)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	afterFirst := countQueue(t, dm)
	require.Positive(t, afterFirst)

	for i := 0; i < 5; i++ {
		_, err := dm.EnqueueSimilarityCandidates(FindSimilarityCandidates(entries))
		require.NoError(t, err)
	}
	require.Equal(t, afterFirst, countQueue(t, dm),
		"repeated explicit scans of the same pair must not enqueue duplicates")
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

// ── 10. missing shared DB does not break retrieval ──────────────────

// Without shared attached, retrieval has nothing to enqueue and so nothing
// to fail on: the code that needed the attachment has been removed from this
// path. The memory must not be mutated, and retrieval must still succeed.
func TestNoShared_RetrievalSucceedsAndLeavesMemoryIntact(t *testing.T) {
	dm := newTestDM(t) // no shared DB attached
	defer dm.Close()

	a, b := pair()
	installStubProvider(t, map[string][]float32{"unattached": a})

	seedNomutateMemory(t, dm, "nos-1", "unattached shared probe alpha record", "{}", 5, a)
	seedNomutateMemory(t, dm, "nos-2", "unattached shared probe beta record", "{}", 5, b)

	cfg := DefaultHybridConfig()
	cfg.Limit = 10
	_, err := HybridSearch(dm, "unattached", "memories", cfg)
	require.NoError(t, err, "retrieval must not depend on the shared attachment")

	require.Equal(t, float64(5), readWeight(t, dm, "nos-1"),
		"an unattached shared DB must not cause a weight mutation")
	require.Equal(t, float64(5), readWeight(t, dm, "nos-2"),
		"an unattached shared DB must not cause a weight mutation")

	// The explicit path DOES need shared, and says so rather than reporting a
	// silent success. This matters: a caller that asked to persist candidates
	// and received no error would conclude the queue was updated.
	_, err = dm.EnqueueSimilarityCandidates(FindSimilarityCandidates([]ScanEntry{
		{ID: "x", Content: "unattached probe", Embedding: a},
		{ID: "y", Content: "unattached probe", Embedding: b},
	}))
	require.Error(t, err, "persisting candidates without shared must report failure")
}

// ── 11. a duplicate-heavy cluster is the worst case ─────────────────

// This is the shape that caused the original damage: many near-identical
// memories, all asserting the SAME claim. Every pair scores above threshold,
// so the old code had the maximum number of opportunities to slash weight.
//
// It is asserted as a whole-corpus property rather than pairwise, because
// that is the claim that matters: N members of a duplicate cluster come out of
// retrieval with exactly the weight they went in with.
func TestNoAutoChallenge_DuplicateHeavyClusterUntouchedByRepeatedRetrieval(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	// Every cluster member shares one vector: identical content, maximal
	// pairwise cosine, so every pair clears the threshold.
	const cluster = "square root of divided by zero x 0 attempt uid="
	vec := unitVec(1, 0.02, 0)
	installStubProvider(t, map[string][]float32{cluster: vec})

	const n = 75
	before := map[string]float64{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("uid-%02d", i)
		before[id] = 5
		seedNomutateMemory(t, dm, id, fmt.Sprintf("%s%d", cluster, i), "{}", 5, vec)
	}

	cfg := DefaultHybridConfig()
	cfg.Limit = 100

	// Repeated retrieval: the cluster is queried several times, because the
	// old code compounded — a member already challenged in pass one was
	// reslashed in pass two.
	for pass := 0; pass < 3; pass++ {
		_, err := HybridSearch(dm, cluster, "memories", cfg)
		require.NoError(t, err)
	}

	for id, w := range before {
		require.Equal(t, w, readWeight(t, dm, id),
			"%s must keep its weight across repeated retrieval of a duplicate cluster", id)
	}
	require.Empty(t, challengedIDs(t, dm),
		"a duplicate cluster must not be treated as 75 contradictions")
	require.Zero(t, countQueue(t, dm),
		"a read must not queue any of the 2775 pairs in this cluster")

	// The cluster IS discoverable explicitly — and the count is reported
	// honestly as unverified candidates, not as contradictions.
	entries, err := dm.LoadSimilarityScanEntries(0, "")
	require.NoError(t, err)
	cands := FindSimilarityCandidates(entries)
	require.Len(t, cands, n*(n-1)/2, "every pair in an identical cluster is a candidate")
	for _, c := range cands {
		require.Contains(t, c.Evidence, "unverified similarity candidate")
		require.NotContains(t, strings.ToLower(c.Evidence), "contradict",
			"evidence must never claim a contradiction was established")
		require.Equal(t, CandidateEqualTier, c.Kind)
	}
}

// ── 12. explicit candidate generation writes exactly what it found ───

// The sanctioned write path, covered directly rather than through the CLI, so
// a failure here localises to this file and not to argument parsing.
func TestEnqueueSimilarityCandidates_WritesOnlyWhatDiscoveryFound(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	a, b := pair()
	installStubProvider(t, map[string][]float32{"explicit": a})

	seedNomutateMemory(t, dm, "exp-1", "explicit generation alpha record", "{}", 5, a)
	seedNomutateMemory(t, dm, "exp-2", "explicit generation beta record", "{}", 5, b)
	// Below threshold: must never appear.
	seedNomutateMemory(t, dm, "exp-far", "an unrelated orthogonal statement",
		"{}", 5, unitVec(0, 1, 0))

	entries, err := dm.LoadSimilarityScanEntries(0, "")
	require.NoError(t, err)
	cands := FindSimilarityCandidates(entries)
	require.Len(t, cands, 1, "only the above-threshold pair is a candidate")
	require.Equal(t, CandidateEqualTier, cands[0].Kind)

	n, err := dm.EnqueueSimilarityCandidates(cands)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, countQueue(t, dm))

	// Re-running finds nothing NEW, and reports that honestly rather than
	// re-reporting the existing row as fresh work.
	n, err = dm.EnqueueSimilarityCandidates(FindSimilarityCandidates(entries))
	require.NoError(t, err)
	require.Zero(t, n, "an already-queued pair must not be reported as new")
	require.Equal(t, 1, countQueue(t, dm))

	// Nothing was enqueued twice, and the below-threshold memory is absent.
	rows, err := dm.SQLDB().Query(
		`SELECT memory_id_a, memory_id_b, detected_by FROM shared.contradiction_log`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var aID, bID, by string
		require.NoError(t, rows.Scan(&aID, &bID, &by))
		require.ElementsMatch(t, []string{"exp-1", "exp-2"}, []string{aID, bID})
		require.Equal(t, similarityCandidateSource, by,
			"rows written by the scan must be attributable to the scan")
	}

	// Weight is untouched by the enqueue path: queueing a review item is not
	// a verdict and must not act like one.
	require.Equal(t, float64(5), readWeight(t, dm, "exp-1"))
	require.Equal(t, float64(5), readWeight(t, dm, "exp-2"))
	require.Empty(t, challengedIDs(t, dm))
}

// ── 13. retrieval writes NOTHING at all ─────────────────────────────

// The strongest form of the guarantee: rather than checking the columns we
// know about, this snapshots every row of the corpus and the whole shared
// queue, runs retrieval, and requires the two snapshots to be byte-identical.
//
// A future regression that adds a new column write will fail here even if no
// existing assertion was written to catch it.
func TestRetrieval_WritesNothingToAnyStore(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()

	seedNomutateMemory(t, dm, "ro-1", "read-only probe alpha record uid=1", "{}", 5, unitVec(1, 0, 0))
	seedNomutateMemory(t, dm, "ro-2", "read-only probe beta record uid=2", "{}", 5, unitVec(1, 0.1, 0))
	// One memory carrying pre-existing challenge state, which the snapshot
	// must preserve rather than "helpfully" clear.
	_, err := dm.ExecTracked(`UPDATE memories SET metadata = ? WHERE id = ?`,
		0, `{"status":"challenged","challenged_theory_id":"pre-existing"}`, "ro-2")
	require.NoError(t, err)

	installStubProvider(t, map[string][]float32{"read-only": unitVec(1, 0, 0)})

	before := snapshotCorpus(t, dm)
	queueBefore := snapshotQueue(t, dm)

	cfg := DefaultHybridConfig()
	cfg.Limit = 10
	for i := 0; i < 3; i++ {
		res, err := HybridSearch(dm, "read-only", "memories", cfg)
		require.NoError(t, err)
		require.NotEmpty(t, res, "retrieval must still return results")
	}

	require.Equal(t, before, snapshotCorpus(t, dm),
		"retrieval must not alter a single field of any memory")
	require.Equal(t, queueBefore, snapshotQueue(t, dm),
		"retrieval must not touch the contradiction queue")

	// The same guarantee for the diagnostic entry point, which reaches the
	// same code from a different caller.
	_, err = dm.HybridSearchMemories("read-only", "memories", 10, "local")
	require.NoError(t, err)
	require.Equal(t, before, snapshotCorpus(t, dm),
		"retrieval diagnostics must not alter any memory field")
	require.Equal(t, queueBefore, snapshotQueue(t, dm),
		"retrieval diagnostics must not touch the contradiction queue")
}

// snapshotCorpus renders every memory row as a sorted slice of strings so
// two snapshots can be compared with require.Equal without depending on
// column order or driver-specific types.
func snapshotCorpus(t *testing.T, dm *DatabaseManager) []string {
	t.Helper()
	rows, err := dm.SQLDB().Query(
		`SELECT id, content, COALESCE(metadata,'{}'), weight, COALESCE(embedding,''),
		        COALESCE(deleted_at,''), COALESCE(last_accessed_at,''),
		        COALESCE(runtime_seconds_since_access,0), COALESCE(runtime_last_accrued_at,0)
		 FROM memories ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id, content, meta, emb, deleted, accessed string
		var weight float64
		var secsSince, lastAccrued int64
		require.NoError(t, rows.Scan(&id, &content, &meta, &weight, &emb,
			&deleted, &accessed, &secsSince, &lastAccrued))
		out = append(out, strings.Join([]string{
			id, content, meta, fmt.Sprintf("%g", weight), emb, deleted, accessed,
			fmt.Sprintf("%d", secsSince), fmt.Sprintf("%d", lastAccrued),
		}, "\x1f"))
	}
	require.NoError(t, rows.Err())
	return out
}

func snapshotQueue(t *testing.T, dm *DatabaseManager) []string {
	t.Helper()
	rows, err := dm.SQLDB().Query(
		`SELECT id, memory_id_a, memory_id_b, COALESCE(detected_by,''),
		        COALESCE(resolved_at,''), COALESCE(similarity,0)
		 FROM shared.contradiction_log ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id, a, b, by, resolved string
		var sim float64
		require.NoError(t, rows.Scan(&id, &a, &b, &by, &resolved, &sim))
		out = append(out, strings.Join([]string{
			id, a, b, by, resolved, fmt.Sprintf("%g", sim),
		}, "\x1f"))
	}
	require.NoError(t, rows.Err())
	return out
}
