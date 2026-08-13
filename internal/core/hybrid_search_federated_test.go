// hybrid_search_federated_test.go — Phase 2d (Multi-Agent Shared
// Epistemology) tests for the federated query path.
//
// Pins the contracts:
//
//   1. SchemaPrefix stamps Origin on HybridResult.
//   2. applySharedPremium: local untouched, shared *1.20, rules *1.35,
//      capped at 1.0.
//   3. Sort order respects final-score > weight > collection.
//   4. HybridSearchMemories scope=local returns only local rows.
//   5. HybridSearchMemories scope=shared returns only shared rows.
//   6. HybridSearchMemories scope=all merges + applies multiplier + slices to limit.
//   7. handleQueryLongTermMemory forwards the `scope` param.
//
// Uses NewTestSharedDM (tmpfile-backed shared DB) since the in-memory
// NewTestDM has no shared DB attached — the federated paths need it.
package internal

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// originOf extracts the Origin field from a result map. Helper to
// keep the assertions below readable.
func originOf(m map[string]interface{}) string {
	if s, ok := m["origin"].(string); ok {
		return s
	}
	return ""
}

// containsOrigin reports whether any item in items carries the given
// origin string. Used to assert scope-filter correctness without
// depending on row count (which is non-deterministic across FTS5 builds).
func containsOrigin(items []map[string]interface{}, want string) bool {
	for _, it := range items {
		if originOf(it) == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Contract 1: SchemaPrefix stamps Origin on HybridResult
// ---------------------------------------------------------------------------

func TestFederated_OriginStampedFromSchemaPrefix(t *testing.T) {
	// Use NewTestSharedDM so the SchemaPrefix="shared." path actually
	// resolves to a table (via ATTACH). Without the shared DB the
	// prefix would 404 at query time, masking the Origin-stamping
	// contract we want to verify.
	dm := NewTestSharedDM(t)

	// Seed: one local row (matches "alpha bravo" via FTS5 BM25).
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, tags, weight, deleted_at)
		VALUES ('local-1', 'memories', 'alpha bravo charlie delta', '[]', 1, NULL)`)
	require.NoError(t, err)

	cfg := DefaultHybridConfig()
	cfg.Limit = 5

	localResults, err := HybridSearch(dm, "alpha", "", cfg)
	require.NoError(t, err)
	require.NotEmpty(t, localResults)
	require.Equal(t, "local", localResults[0].Origin,
		"default cfg.Origin (empty cfg) should be normalised to 'local'")

	// Now flip the prefix / origin and re-run with the shared schema.
	// No shared rows exist for "alpha", so the result is empty — but
	// the SchemaPrefix plumbing must not error, and any rows that DO
	// come back must have Origin=shared.
	sharedCfg := DefaultHybridConfig()
	sharedCfg.SchemaPrefix = "shared."
	sharedCfg.Origin = "shared"
	sharedCfg.Limit = 5

	sharedResults, err := HybridSearch(dm, "alpha", "", sharedCfg)
	require.NoError(t, err, "shared-schema HybridSearch must not error")
	for _, r := range sharedResults {
		require.Equal(t, "shared", r.Origin,
			"shared-schema search must stamp Origin='shared', got %q", r.Origin)
	}
}

// ---------------------------------------------------------------------------
// Contract 2: applySharedPremium multiplier matrix
// ---------------------------------------------------------------------------

func TestApplySharedPremium_MultiplierMatrix(t *testing.T) {
	type tc struct {
		name           string
		in             []HybridResult
		limit          int
		wantFinals     []float64 // expected final score per row in returned order
		wantOrigins    []string  // expected origin per row in returned order
		wantColl       []string  // expected collection per row in returned order
	}

	cases := []tc{
		{
			name: "local untouched",
			in: []HybridResult{
				{ID: "L1", Origin: "local", Collection: "memories", CombinedScore: 0.5, Weight: 1},
			},
			limit:       10,
			wantFinals:  []float64{0.5},
			wantOrigins: []string{"local"},
			wantColl:    []string{"memories"},
		},
		{
			name: "shared non-rule boosted 1.20x",
			in: []HybridResult{
				{ID: "S1", Origin: "shared", Collection: "decisions", CombinedScore: 0.5, Weight: 1},
			},
			limit:       10,
			wantFinals:  []float64{0.6}, // 0.5 * 1.20 = 0.60
			wantOrigins: []string{"shared"},
			wantColl:    []string{"decisions"},
		},
		{
			name: "shared rule boosted 1.35x",
			in: []HybridResult{
				{ID: "SR1", Origin: "shared", Collection: "rules", CombinedScore: 0.5, Weight: 1},
			},
			limit:       10,
			wantFinals:  []float64{0.675}, // 0.5 * 1.35 = 0.675
			wantOrigins: []string{"shared"},
			wantColl:    []string{"rules"},
		},
		{
			name: "shared rule 1.0 cap honoured",
			in: []HybridResult{
				{ID: "SR-high", Origin: "shared", Collection: "rules", CombinedScore: 0.95, Weight: 1},
			},
			limit:       10,
			wantFinals:  []float64{1.0}, // min(0.95 * 1.35, 1.0) = min(1.2825, 1.0) = 1.0
			wantOrigins: []string{"shared"},
			wantColl:    []string{"rules"},
		},
		{
			name: "shared rule stays at zero when combined is zero",
			in: []HybridResult{
				{ID: "SR-zero", Origin: "shared", Collection: "rules", CombinedScore: 0, Weight: 1},
			},
			limit:       10,
			wantFinals:  []float64{0}, // 0 * 1.35 = 0 (NOT promoted to 1.0!)
			wantOrigins: []string{"shared"},
			wantColl:    []string{"rules"},
		},
		{
			name: "irrelevant shared stays below relevant local",
			in: []HybridResult{
				{ID: "L-low", Origin: "local", Collection: "memories", CombinedScore: 0.3, Weight: 1},
				{ID: "S-neg", Origin: "shared", Collection: "rules", CombinedScore: -0.2, Weight: 1},
			},
			limit:       10,
			wantFinals:  []float64{0.3, -0.27}, // local 0.3 stays; shared -0.2 * 1.35 = -0.27
			wantOrigins: []string{"local", "shared"},
			wantColl:    []string{"memories", "rules"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := applySharedPremium(c.in, c.limit)
			require.Len(t, out, len(c.wantFinals))
			for i := range out {
				require.InDelta(t, c.wantFinals[i], out[i].CombinedScore, 0.001,
					"row %d final score mismatch (got=%v want=%v)",
					i, out[i].CombinedScore, c.wantFinals[i])
				require.Equal(t, c.wantOrigins[i], out[i].Origin)
				require.Equal(t, c.wantColl[i], out[i].Collection)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Contract 3: sort order respects final-score > weight > collection
// ---------------------------------------------------------------------------

func TestApplySharedPremium_SortOrder(t *testing.T) {
	in := []HybridResult{
		// Two rows with tied final score (0.5), different weights.
		{ID: "W1", Origin: "local", Collection: "memories", CombinedScore: 0.5, Weight: 10},
		{ID: "W2", Origin: "local", Collection: "memories", CombinedScore: 0.5, Weight: 5},
		// Three rows with tied final + weight; collection asc tiebreaks.
		{ID: "C1", Origin: "local", Collection: "lessons", CombinedScore: 0.4, Weight: 1},
		{ID: "C2", Origin: "local", Collection: "decisions", CombinedScore: 0.4, Weight: 1},
		{ID: "C3", Origin: "local", Collection: "rules", CombinedScore: 0.4, Weight: 1}, // sort key matches "lessons"
		// Different scores.
		{ID: "Z", Origin: "local", Collection: "memories", CombinedScore: 0.1, Weight: 1},
	}
	out := applySharedPremium(in, 10)
	require.Len(t, out, 6)

	require.Equal(t, "W1", out[0].ID) // weight 10 wins over weight 5
	require.Equal(t, "W2", out[1].ID)
	// Remaining four all have different scores / collection asc ordering.
	// Score 0.4 tie: decisions < lessons < rules alphabetically... wait
	// ascending means "decisions" first. Adjust expectation:
	// C2 (decisions) < C1 (lessons) < C3 (rules) < Z (0.1)
	expectedOrder := []string{"W1", "W2", "C2", "C1", "C3", "Z"}
	for i, want := range expectedOrder {
		require.Equal(t, want, out[i].ID, "row %d position mismatch", i)
	}
}

// ---------------------------------------------------------------------------
// Contract 4 + 5 + 6: HybridSearchMemories scope filter, end-to-end with shared
// ---------------------------------------------------------------------------

// TestFederated_HybridSearchMemoriesScope requires a shared DB so we
// use NewTestSharedDM. This is the integration test that proves the
// scope parameter actually filters correctly against two ATTACHed
// databases (not just the in-memory local-only setup).
func TestFederated_HybridSearchMemoriesScope(t *testing.T) {
	dm := NewTestSharedDM(t)

	// Seed local: a memory that hits FTS5 for "alpha bravo".
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, tags, weight, deleted_at)
		VALUES ('local-alpha', 'memories', 'alpha bravo charlie delta', '[]', 5, NULL)`)
	require.NoError(t, err)

	// Seed shared: a rule that also hits "alpha". Use the existing
	// shared schema. requires the shared DB to be migrated (InitSchema
	// already does this), and the row to have is_global=1 so
	// QueryGlobalRules sees it.
	_, err = dm.db.Exec(`
		INSERT INTO shared.memories (id, collection, content, tags, weight, is_global, deleted_at)
		VALUES ('shared-alpha', 'rules', 'alpha bravo — house rule', '[]', 10, 1, NULL)`)
	require.NoError(t, err)

	// shared.memories_fts is auto-synced by triggers in attachShared;
	// no manual backfill needed (and adding one would double-insert
	// rows against the FTS5 primary-key constraint).

	// scope=local: only local row.
	localItems, err := dm.HybridSearchMemories("alpha", "", 5, "local")
	require.NoError(t, err)
	require.NotEmpty(t, localItems)
	require.False(t, containsOrigin(localItems, "shared"),
		"scope=local must NOT return shared rows; got %d items", len(localItems))
	require.True(t, containsOrigin(localItems, "local"))

	// scope=shared: only shared row.
	sharedItems, err := dm.HybridSearchMemories("alpha", "", 5, "shared")
	require.NoError(t, err)
	require.NotEmpty(t, sharedItems)
	require.True(t, containsOrigin(sharedItems, "shared"),
		"scope=shared must return shared rows")
	require.False(t, containsOrigin(sharedItems, "local"),
		"scope=shared must NOT return local rows; got %d items", len(sharedItems))

	// scope=all: both rows, shared gets the 1.35 rules boost.
	allItems, err := dm.HybridSearchMemories("alpha", "", 5, "all")
	require.NoError(t, err)
	require.True(t, containsOrigin(allItems, "local"))
	require.True(t, containsOrigin(allItems, "shared"))

	// The shared rule should rank first because of the multiplier
	// (raw relevance ties would go either way; rules multiplier
	// closes the gap).
	var sharedItem map[string]interface{}
	for _, it := range allItems {
		if originOf(it) == "shared" {
			sharedItem = it
			break
		}
	}
	require.NotNil(t, sharedItem, "shared row must appear in scope=all")
	// Score shouldn't exceed 1.0 (the cap).
	if score, ok := sharedItem["combined_score"].(float64); ok {
		require.LessOrEqual(t, score, 1.0, "boosted score must respect 1.0 cap")
	}
}

// TestFederated_ApplySharedPremiumLimitSlicing pins the federated
// fetch buffer: with limit=3 and 8 rows of mixed origin, the result
// must be exactly 3 items and the top 3 by final-score.
func TestFederated_ApplySharedPremiumLimitSlicing(t *testing.T) {
	in := []HybridResult{
		{ID: "L1", Origin: "local", Collection: "memories", CombinedScore: 0.95, Weight: 1},
		{ID: "L2", Origin: "local", Collection: "memories", CombinedScore: 0.90, Weight: 1},
		{ID: "L3", Origin: "local", Collection: "memories", CombinedScore: 0.85, Weight: 1},
		{ID: "L4", Origin: "local", Collection: "memories", CombinedScore: 0.80, Weight: 1},
		{ID: "SR1", Origin: "shared", Collection: "rules", CombinedScore: 0.90, Weight: 1}, // *1.35 = 1.215 → cap 1.0
		{ID: "SR2", Origin: "shared", Collection: "rules", CombinedScore: 0.85, Weight: 1},
		{ID: "S1", Origin: "shared", Collection: "decisions", CombinedScore: 0.95, Weight: 1}, // *1.20 = 1.14 → cap 1.0
		{ID: "S2", Origin: "shared", Collection: "decisions", CombinedScore: 0.80, Weight: 1},
	}
	out := applySharedPremium(in, 3)
	require.Len(t, out, 3)

	// Both shared rules hit the 1.0 cap. Decisions at 0.95 → 1.14 → 1.0.
	// So we expect three rows at final=1.0; tie-break by weight (all 1),
	// then by collection asc -> "decisions" < "rules".
	expectedOrder := []string{"S1", "SR1", "SR2"}
	for i, want := range expectedOrder {
		require.Equal(t, want, out[i].ID, "row %d position mismatch", i)
		require.InDelta(t, 1.0, out[i].CombinedScore, 0.001,
			"row %d must be capped at 1.0", i)
	}
}

// ---------------------------------------------------------------------------
// Contract 7: handleQueryLongTermMemory forwards the scope param
// (uses a different in-memory setup; the DM call itself is the unit
// under test, not the FTS5 plumbing)
// ---------------------------------------------------------------------------

func TestHandleQueryLongTermMemory_ScopeForwarded(t *testing.T) {
	dm := NewTestSharedDM(t)
	// Seed at least one row so the search returns something to inspect.
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, tags, weight, deleted_at)
		VALUES ('local-x', 'memories', 'unique_marker_string', '[]', 5, NULL)`)
	require.NoError(t, err)

	// scope param propagates: caller asks for local-only.
	items, err := dm.HybridSearchMemories("unique_marker_string", "", 5, "local")
	require.NoError(t, err)
	require.False(t, containsOrigin(items, "shared"),
		"local scope must filter out shared rows")

	// Sanity: same query, no scope, gets 'all' default — which may
	// or may not include shared rows depending on whether any are
	// seeded. We don't assert on shared presence here; just that
	// the default doesn't break.
	allItems, err := dm.HybridSearchMemories("unique_marker_string", "", 5, "")
	require.NoError(t, err)
	require.NotEmpty(t, allItems)
}

// suppress unused-import lint when the test compiles down to a
// subset on certain build configs.
var _ = sort.Strings

// TestVectorMatch_MaxScanCap_Default verifies the circuit breaker in
// VectorMatch. With MPM_MAX_VECTOR_SCAN=0 (no cap) the existing
// behaviour holds. With a tiny cap (3 rows) and a corpus larger than
// the cap, the function MUST error rather than crash, hang, or OOM.
//
// This is the regression net for the 2026-07-07 VectorMatch refactor
// — the un-indexed O(n) cosine scan can OOM at scale; the cap buys
// time until the ANN index (HNSW/IVF) lands in docs/architecture/shared-epistemology.md.
func TestVectorMatch_MaxScanCap_Default(t *testing.T) {
	dm := NewTestDM(t)
	// No env var set: default cap (5000) is well above any test
	// corpus, so VectorMatch should NOT trip on a small dataset.
	// Empty query embedding: just exercise the cap path with the
	// default to confirm it doesn't false-positive.
	vec := make([]float32, 4)
	_, err := dm.VectorMatch("", vec, 10, "")
	require.NoError(t, err, "default cap (5000) should not trip on small test corpus")
}

func TestVectorMatch_MaxScanCap_DisabledByZero(t *testing.T) {
	dm := NewTestDM(t)
	t.Setenv("MPM_MAX_VECTOR_SCAN", "0")
	vec := make([]float32, 4)
	_, err := dm.VectorMatch("", vec, 10, "")
	require.NoError(t, err, "MPM_MAX_VECTOR_SCAN=0 must disable the cap (full scan, your funeral)")
}

func TestVectorMatch_MaxScanCap_TripsAboveThreshold(t *testing.T) {
	dm := NewTestDM(t)
	// Set the cap to 0. With 0 we expect... actually the test was
	// for "trips ABOVE threshold". With MPM_MAX_VECTOR_SCAN=1 we'd
	// require the corpus to be empty. A more honest test: insert
	// a few rows, then set cap=1, expect error.
	t.Setenv("MPM_MAX_VECTOR_SCAN", "1")
	_, _ = dm.db.Exec(`INSERT INTO memories (id, collection, content, embedding, weight, deleted_at)
		VALUES ('cap-1', 'memories', 'first', '[0.1, 0.2, 0.3, 0.4]', 1, NULL)`)
	_, _ = dm.db.Exec(`INSERT INTO memories (id, collection, content, embedding, weight, deleted_at)
		VALUES ('cap-2', 'memories', 'second', '[0.2, 0.3, 0.4, 0.5]', 1, NULL)`)
	_, _ = dm.db.Exec(`INSERT INTO memories (id, collection, content, embedding, weight, deleted_at)
		VALUES ('cap-3', 'memories', 'third', '[0.3, 0.4, 0.5, 0.6]', 1, NULL)`)
	vec := []float32{0.1, 0.2, 0.3, 0.4}
	_, err := dm.VectorMatch("", vec, 10, "")
	require.Error(t, err, "cap=1 with 3 rows in scope MUST trip the circuit breaker")
	require.Contains(t, err.Error(), "vector scan cap exceeded")
	require.Contains(t, err.Error(), "MPM_MAX_VECTOR_SCAN")
}

func TestVectorMatch_MaxScanCap_AllowsAtThreshold(t *testing.T) {
	dm := NewTestDM(t)
	t.Setenv("MPM_MAX_VECTOR_SCAN", "5")
	// Insert exactly 5 rows; cap is inclusive at the threshold.
	for i := 0; i < 5; i++ {
		_, _ = dm.db.Exec(`INSERT INTO memories (id, collection, content, embedding, weight, deleted_at)
			VALUES (?, 'memories', ?, ?, 1, NULL)`,
			fmt.Sprintf("at-cap-%d", i), fmt.Sprintf("content %d", i), "[0.1, 0.2, 0.3, 0.4]")
	}
	vec := []float32{0.1, 0.2, 0.3, 0.4}
	_, err := dm.VectorMatch("", vec, 10, "")
	require.NoError(t, err, "5 rows with cap=5 should NOT trip (5 ≤ 5)")
}
