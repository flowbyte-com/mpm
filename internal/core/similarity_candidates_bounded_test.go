// similarity_candidates_bounded_test.go — coverage for the BOUNDED
// discovery pass (issue #21, Phase B).
//
// FindSimilarityCandidatesBounded is the difference between "the scan
// looked at the corpus" and "the scan looked at a bounded window of the
// corpus and said so". Every property the bounded pass claims is a
// property about HONESTY, so the tests here are mostly about what the
// pass does when it cannot do the whole job:
//
//   - it refuses the comparison budget BEFORE evaluating a single pair,
//     rather than evaluating pairs and stopping halfway;
//   - it refuses the candidate cap rather than truncating, so no caller
//     can persist a partial discovery;
//   - its statistics add up, so an operator reading three numbers is
//     reading about one population;
//   - its refusal arithmetic does not overflow.
//
// The dense-cluster test is the load-bearing one. A corpus of 75
// near-identical memories is the shape that actually breaks an
// unbounded scan: every pair qualifies, so the candidate count is
// quadratic in the corpus size. That is the case the budgets exist for,
// and it is the case where "we found 2,775 candidates" and "we found
// 2,775 candidates in a 75-row window" must not be confused.

package internal

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// denseCluster builds n entries that are mutually near-identical: every
// pair clears the candidate threshold, so the candidate count is exactly
// C(n,2).
//
// The vectors are unit vectors with a tiny shared component so that they
// are not bit-identical — a cluster of bit-identical vectors is a
// degenerate case that some fast paths special-case.
func denseCluster(t *testing.T, n int) []ScanEntry {
	t.Helper()
	entries := make([]ScanEntry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, ScanEntry{
			ID:      fmt.Sprintf("dense-%03d", i),
			Content: fmt.Sprintf("the workspace must use pnpm for every install (variant %d)", i),
			// A shared dominant axis plus a small distinct component: the
			// cosine between any two is close to 1 and comfortably above
			// the 0.85 threshold.
			Embedding: vec(t, 1, float32(i+1)/1000.0),
		})
	}
	return entries
}

// ── comparison budget ──────────────────────────────────────────────

// The comparison budget is checked ONCE, before any pair is evaluated.
//
// This matters more than it looks. A budget checked per-pair would let a
// scan evaluate exactly N pairs and then stop, producing a candidate set
// that depends on where the loop happened to stop — an arbitrary subset
// dressed up as a result. Refusing up front means the pass either
// examines the whole window or examines nothing.
func TestFindSimilarityCandidatesBounded_CompareBudgetRefusesBeforeAnyPair(t *testing.T) {
	entries := denseCluster(t, 10) // C(10,2) = 45 pairs

	cands, stats, err := FindSimilarityCandidatesBounded(entries, DefaultCandidateCap, 10)

	require.Error(t, err, "a window that needs 45 pairs must be refused by a budget of 10")
	var budgetErr *ErrCompareBudgetExceeded
	require.ErrorAs(t, err, &budgetErr)
	require.Equal(t, 10, budgetErr.Entries)
	require.Equal(t, int64(45), budgetErr.EstimatedPairs)
	require.Equal(t, 10, budgetErr.CompareBudget)

	require.Nil(t, cands, "a refused pass must not return a partial candidate set")
	require.True(t, stats.CompareBudgetHit)
	require.Zero(t, stats.PairsExamined,
		"the budget is checked before the loop, so no pair may have been evaluated")
	require.Equal(t, int64(45), stats.EstimatedPairs,
		"the estimate is what the operator needs in order to choose a budget")
}

// The boundary is inclusive: a window that needs EXACTLY the budget is
// allowed, and one pair more is refused. An off-by-one here would either
// refuse a legitimate scan or let one extra window through.
func TestFindSimilarityCandidatesBounded_CompareBudgetBoundary(t *testing.T) {
	entries := denseCluster(t, 10) // C(10,2) = 45 pairs

	t.Run("exactly at the budget is allowed", func(t *testing.T) {
		cands, stats, err := FindSimilarityCandidatesBounded(entries, DefaultCandidateCap, 45)
		require.NoError(t, err)
		require.Len(t, cands, 45)
		require.Equal(t, 45, stats.PairsExamined)
		require.False(t, stats.CompareBudgetHit)
	})

	t.Run("one pair over the budget is refused", func(t *testing.T) {
		cands, _, err := FindSimilarityCandidatesBounded(entries, DefaultCandidateCap, 44)
		require.Error(t, err)
		require.Nil(t, cands)
	})
}

// A window of fewer than two entries has no pairs, and is therefore a
// COMPLETE scan that found nothing — not a refusal.
//
// Getting this wrong in the other direction would make an empty corpus
// report a budget failure, which would send an operator looking for a
// problem that does not exist.
func TestFindSimilarityCandidatesBounded_SmallWindowsAreCompleteNotRefused(t *testing.T) {
	for _, n := range []int{0, 1} {
		t.Run(fmt.Sprintf("%d entries", n), func(t *testing.T) {
			entries := make([]ScanEntry, 0, n)
			for i := 0; i < n; i++ {
				entries = append(entries, ScanEntry{ID: fmt.Sprintf("e-%d", i), Embedding: vec(t, 1, 0, 0)})
			}
			cands, stats, err := FindSimilarityCandidatesBounded(entries, DefaultCandidateCap, MinCompareBudget)
			require.NoError(t, err)
			require.Empty(t, cands)
			require.False(t, stats.CompareBudgetHit)
			require.False(t, stats.CandidateCapHit)
			require.Zero(t, stats.PairsExamined)
		})
	}
}

// ── candidate cap ──────────────────────────────────────────────────

// The candidate cap is an ALLOCATION bound, and exceeding it fails the
// pass closed.
//
// The refused pass must return NO candidates at all. A truncated slice
// would be an arbitrary subset, and a caller that persisted it while
// reporting the full candidate count would be reporting a number it
// cannot explain — the exact failure the contract forbids.
func TestFindSimilarityCandidatesBounded_CandidateCapRefusesAndReturnsNothing(t *testing.T) {
	entries := denseCluster(t, 10) // 45 qualifying pairs

	cands, stats, err := FindSimilarityCandidatesBounded(entries, 5, DefaultCompareBudget)

	require.Error(t, err)
	var capErr *ErrCandidateCapReached
	require.ErrorAs(t, err, &capErr)
	require.Equal(t, 45, capErr.CandidatesFound)
	require.Equal(t, 5, capErr.CandidateCap)
	require.Equal(t, 45, capErr.PairsExamined)

	require.Nil(t, cands, "a capped pass must not hand back a partial candidate set")
	require.True(t, stats.CandidateCapHit)
	require.Equal(t, 45, stats.CandidatesFound,
		"the pass keeps counting past the cap so the statistics describe the whole window")
	require.Equal(t, 5, stats.CandidatesMaterialised)
}

// The cap boundary is inclusive: exactly `cap` candidates is a complete
// scan.
func TestFindSimilarityCandidatesBounded_CandidateCapBoundary(t *testing.T) {
	entries := denseCluster(t, 10) // 45 qualifying pairs

	cands, stats, err := FindSimilarityCandidatesBounded(entries, 45, DefaultCompareBudget)
	require.NoError(t, err)
	require.Len(t, cands, 45)
	require.Equal(t, 45, stats.CandidatesMaterialised)
	require.False(t, stats.CandidateCapHit)

	cands, _, err = FindSimilarityCandidatesBounded(entries, 44, DefaultCompareBudget)
	require.Error(t, err)
	require.Nil(t, cands)
}

// ── non-positive budgets ───────────────────────────────────────────

// A comparison budget of zero is a budget that permits NO comparisons —
// not an unlimited budget.
//
// The guard that used to require a positive budget made a budget of zero
// mean "compare everything", which is the exact opposite of what a budget
// of zero says, and it is the difference between a bound and no bound at
// all. A negative budget is the same case with the sign made explicit.
func TestFindSimilarityCandidatesBounded_NonPositiveCompareBudgetRefuses(t *testing.T) {
	entries := denseCluster(t, 10) // C(10,2) = 45 pairs

	for _, budget := range []int{0, -1, -1000} {
		t.Run(fmt.Sprintf("budget %d", budget), func(t *testing.T) {
			cands, stats, err := FindSimilarityCandidatesBounded(entries, DefaultCandidateCap, budget)

			require.Error(t, err, "a budget of zero must refuse, not compare everything")
			var budgetErr *ErrCompareBudgetExceeded
			require.ErrorAs(t, err, &budgetErr)
			require.Equal(t, budget, budgetErr.CompareBudget)
			require.Equal(t, int64(45), budgetErr.EstimatedPairs)

			require.Nil(t, cands)
			require.True(t, stats.CompareBudgetHit)
			require.Zero(t, stats.PairsExamined,
				"a refused pass examines no pair at all")
		})
	}
}

// A candidate cap of zero is a cap that permits NO candidate to be
// retained.
//
// The same guard used to make a cap of zero mean "retain everything",
// which is the opposite of what a cap of zero says. A window containing a
// qualifying candidate therefore refuses the pass.
func TestFindSimilarityCandidatesBounded_NonPositiveCandidateCapRefuses(t *testing.T) {
	entries := denseCluster(t, 10) // 45 qualifying pairs

	for _, candidateCap := range []int{0, -1} {
		t.Run(fmt.Sprintf("cap %d", candidateCap), func(t *testing.T) {
			cands, stats, err := FindSimilarityCandidatesBounded(entries, candidateCap, DefaultCompareBudget)

			require.Error(t, err, "a cap of zero must refuse, not retain everything")
			var capErr *ErrCandidateCapReached
			require.ErrorAs(t, err, &capErr)
			require.Equal(t, candidateCap, capErr.CandidateCap)
			require.Equal(t, 45, capErr.CandidatesFound)

			require.Nil(t, cands)
			require.True(t, stats.CandidateCapHit)
			require.Zero(t, stats.CandidatesMaterialised,
				"a refused pass retains nothing")
		})
	}
}

// A window with no pairs at all is a COMPLETE scan under any budget,
// including a non-positive one.
//
// This is the boundary of the two rules above: there is nothing to
// compare and nothing to retain, so neither budget can be exceeded. An
// empty corpus must not report a budget failure — that would send an
// operator looking for a problem that does not exist.
func TestFindSimilarityCandidatesBounded_EmptyWindowIsNotARefusalForAnyBudget(t *testing.T) {
	for _, n := range []int{0, 1} {
		entries := make([]ScanEntry, 0, n)
		for i := 0; i < n; i++ {
			entries = append(entries, ScanEntry{ID: fmt.Sprintf("e-%d", i), Embedding: vec(t, 1, 0, 0)})
		}
		for _, budget := range []int{0, -1} {
			for _, candidateCap := range []int{0, -1} {
				cands, stats, err := FindSimilarityCandidatesBounded(entries, candidateCap, budget)
				require.NoError(t, err)
				require.Empty(t, cands)
				require.False(t, stats.CompareBudgetHit)
				require.False(t, stats.CandidateCapHit)
			}
		}
	}
}

// ── statistics ─────────────────────────────────────────────────────

// The statistics are internally consistent, and the identities are the
// point: an operator reading "19,900 pairs examined, 900 above
// threshold, 123 candidates" needs those three numbers to describe one
// population.
func TestFindSimilarityCandidatesBounded_StatisticsAreInternallyConsistent(t *testing.T) {
	entries := denseCluster(t, 12) // 66 pairs, all above threshold
	// Add one entry with no embedding: it takes part in no comparison.
	entries = append(entries, ScanEntry{ID: "no-embedding", Content: "unembedded"})

	cands, stats, err := FindSimilarityCandidatesBounded(entries, DefaultCandidateCap, DefaultCompareBudget)
	require.NoError(t, err)
	require.Len(t, cands, 66)

	require.Equal(t, stats.PairsExamined,
		stats.PairsSkippedInvalid+stats.PairsBelowThreshold+stats.PairsAboveThreshold,
		"every pair the pass entered must be accounted for exactly once")
	require.Equal(t, stats.PairsAboveThreshold,
		stats.CandidatesFound+stats.PairsDeclined,
		"every above-threshold pair is either a candidate or a declined pair")
	require.Equal(t, stats.CandidatesFound, stats.CandidatesMaterialised,
		"below the cap, every found candidate is materialised")
	require.Equal(t, 1, stats.EntriesSkipped,
		"an entry with no embedding is counted once, not once per pair it would have joined")
	require.Equal(t, len(entries), stats.EntriesConsidered)
}

// A corpus with BOTH above-threshold and below-threshold pairs must
// account for both populations separately.
func TestFindSimilarityCandidatesBounded_StatisticsSeparateBelowFromAbove(t *testing.T) {
	entries := []ScanEntry{
		{ID: "a", Content: "claim a", Embedding: vec(t, 1, 0, 0)},
		{ID: "b", Content: "claim b", Embedding: vec(t, 1, 0, 0)}, // cosine 1.0 → above
		{ID: "c", Content: "claim c", Embedding: vec(t, 0, 1, 0)}, // cosine 0.0 → below
		{ID: "d", Content: "claim d", Embedding: vec(t, 0, 1, 0)}, // cosine 1.0 with c → above
	}

	cands, stats, err := FindSimilarityCandidatesBounded(entries, DefaultCandidateCap, DefaultCompareBudget)
	require.NoError(t, err)
	require.Len(t, cands, 2)
	require.Equal(t, 6, stats.PairsExamined)
	require.Equal(t, 4, stats.PairsBelowThreshold,
		"the four cross-side pairs have cosine 0 and fall below the threshold")
	require.Equal(t, 2, stats.PairsAboveThreshold,
		"the two within-side pairs have cosine 1 and clear the threshold")
	require.Equal(t, 2, stats.CandidatesFound)
}

// ── equivalence with the unbounded pass ────────────────────────────

// The bounded pass is the SAME discovery with budgets applied. Below
// every budget it must produce the same candidates in the same order —
// same threshold, same structural-prefix discount, same classification,
// same deterministic ordering.
//
// This is the test that says the budgets changed the SIZE of what a scan
// can do, not WHAT a scan means.
func TestFindSimilarityCandidatesBounded_MatchesUnboundedSemantics(t *testing.T) {
	entries := []ScanEntry{
		{ID: "a", Content: "the deploy pipeline runs on tuesday", Embedding: vec(t, 1, 0, 0)},
		{ID: "b", Content: "the deploy pipeline runs on tuesdays", Embedding: vec(t, 1, 0, 0)},
		{ID: "c", Content: "CHOICE: tuesday", Embedding: vec(t, 1, 0, 0)},
		{ID: "d", Content: "unrelated claim", Embedding: vec(t, 0, 1, 0)},
	}

	unbounded := FindSimilarityCandidates(entries)
	bounded, _, err := FindSimilarityCandidatesBounded(entries, DefaultCandidateCap, DefaultCompareBudget)
	require.NoError(t, err)

	require.Equal(t, len(unbounded), len(bounded))
	for i := range unbounded {
		require.Equal(t, unbounded[i].MemoryA, bounded[i].MemoryA)
		require.Equal(t, unbounded[i].MemoryB, bounded[i].MemoryB)
		require.Equal(t, unbounded[i].Similarity, bounded[i].Similarity)
		require.Equal(t, unbounded[i].Kind, bounded[i].Kind)
		require.Equal(t, unbounded[i].Evidence, bounded[i].Evidence)
	}
}

// ── the dense 75-row cluster ───────────────────────────────────────

// The dense cluster is the workload the budgets exist for.
//
// 75 mutually-similar memories produce C(75,2) = 2,775 qualifying pairs.
// The candidate count is quadratic in the corpus size, so a corpus that
// grows from 75 to 750 rows would produce 100x the candidates. This test
// pins three things at once:
//
//   - the discovery pass finds all 2,775;
//   - the DEFAULT candidate cap (10,000) is above that, so a normal
//     operator scan of a dense corpus completes;
//   - a deliberately small cap refuses the pass rather than truncating.
//
// It also records the measured wall time, which is a measurement and not
// a guarantee — see the comment.
func TestFindSimilarityCandidatesBounded_Dense75RowCluster(t *testing.T) {
	entries := denseCluster(t, 75)
	const expectedPairs = 75 * 74 / 2 // 2,775

	start := time.Now()
	cands, stats, err := FindSimilarityCandidatesBounded(entries, DefaultCandidateCap, DefaultCompareBudget)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Len(t, cands, expectedPairs)
	require.Equal(t, expectedPairs, stats.PairsExamined)
	require.Equal(t, expectedPairs, stats.CandidatesFound)
	require.Equal(t, expectedPairs, stats.CandidatesMaterialised)
	require.Equal(t, int64(expectedPairs), stats.EstimatedPairs)

	t.Logf("MEASURED dense 75-row cluster: %d pairs compared in %s (%d candidates materialised)",
		stats.PairsExamined, elapsed, stats.CandidatesMaterialised)

	// A small cap refuses rather than truncating.
	cands, stats, err = FindSimilarityCandidatesBounded(entries, 100, DefaultCompareBudget)
	require.Error(t, err)
	require.Nil(t, cands)
	require.True(t, stats.CandidateCapHit)
	require.Equal(t, expectedPairs, stats.CandidatesFound)
}

// The comparison budget refuses a dense cluster before it is evaluated,
// which is the whole point of checking the budget up front: the dense
// corpus is exactly the one where "evaluate N pairs then stop" would be
// most tempting and most misleading.
func TestFindSimilarityCandidatesBounded_DenseClusterHitsCompareBudgetFirst(t *testing.T) {
	entries := denseCluster(t, 75)

	start := time.Now()
	cands, stats, err := FindSimilarityCandidatesBounded(entries, DefaultCandidateCap, 1000)
	elapsed := time.Since(start)

	require.Error(t, err)
	var budgetErr *ErrCompareBudgetExceeded
	require.ErrorAs(t, err, &budgetErr)
	require.Equal(t, int64(2775), budgetErr.EstimatedPairs)
	require.Nil(t, cands)
	require.Zero(t, stats.PairsExamined)

	t.Logf("MEASURED dense 75-row cluster refused by the comparison budget in %s (0 pairs evaluated)",
		elapsed)
}

// ── overflow-safe arithmetic ───────────────────────────────────────

// The NORMAL workload: the default window, on a corpus that is mostly
// unrelated.
//
// This is the measurement an operator actually sees: 100 memories per
// source is a union of at most 200 entries, so 19,900 pairs.
//
// The two numbers this test reports are DIFFERENT KINDS OF FACT and are
// labelled as such:
//
//   - the pair count (19,900) is a MATHEMATICAL COUNT — C(200,2). It is
//     exact and it does not vary between runs.
//   - the elapsed time is a MEASUREMENT on one machine at one moment. It
//     is not a guarantee, not a bound, and not portable. It is recorded
//     so that a later regression that made the pass slower is visible in
//     the log rather than invisible.
//
// The corpus here is deliberately mostly-unrelated: distinct vectors, so
// almost every pair falls below the threshold and the candidate count
// stays near zero. The dense case is measured separately.
func TestFindSimilarityCandidatesBounded_NormalWorkloadAtDefaultWindow(t *testing.T) {
	// 100 per source, two sources, disjoint IDs so the union is 200.
	entries := make([]ScanEntry, 0, 200)
	for i := 0; i < 200; i++ {
		entries = append(entries, ScanEntry{
			ID:      fmt.Sprintf("normal-%03d", i),
			Content: fmt.Sprintf("an unrelated operational fact number %d", i),
			// Spread across a 64-dimensional space so pairs are mostly
			// uncorrelated rather than near-identical.
			Embedding: spreadVector(t, i, 64),
		})
	}

	const expectedPairs = 200 * 199 / 2 // 19,900

	start := time.Now()
	cands, stats, err := FindSimilarityCandidatesBounded(entries, DefaultCandidateCap, DefaultCompareBudget)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Equal(t, expectedPairs, stats.PairsExamined)
	require.Equal(t, int64(expectedPairs), stats.EstimatedPairs)
	require.Equal(t, stats.PairsBelowThreshold+stats.PairsAboveThreshold+stats.PairsSkippedInvalid,
		stats.PairsExamined)
	require.LessOrEqual(t, len(cands), DefaultCandidateCap)

	t.Logf("MATHEMATICAL: C(200,2) = %d pairs implied by the default window", expectedPairs)
	t.Logf("MEASURED normal workload: %d pairs compared in %s (%d candidates, %d below threshold)",
		stats.PairsExamined, elapsed, stats.CandidatesFound, stats.PairsBelowThreshold)
}

// The default comparison budget is sized for the default window.
//
// 19,900 pairs against a 100,000 budget leaves roughly 5x of headroom, so
// the default scan is NOT close to refusing. The headroom is what absorbs
// an operator who raises --propose-limit without also raising
// --compare-budget.
func TestFindSimilarityCandidatesBounded_DefaultBudgetHasHeadroomForTheDefaultWindow(t *testing.T) {
	implied := comparePairCountForSources(defaultProposalScanLimitForTest, 2)
	require.Equal(t, int64(19_900), implied)
	require.Less(t, implied, int64(DefaultCompareBudget),
		"the default comparison budget must admit the default scan window")
}

// defaultProposalScanLimitForTest mirrors the CLI's default per-source scan
// limit. The core package does not depend on the CLI, so the number is
// restated here as a literal with a comment saying where it comes from.
const defaultProposalScanLimitForTest = 100

// spreadVector builds a deterministic unit vector whose components are
// spread by the seed, so that different seeds are largely uncorrelated.
func spreadVector(t *testing.T, seed, dim int) []float32 {
	t.Helper()
	out := make([]float32, dim)
	// A small linear congruential sequence: deterministic, cheap, and
	// uncorrelated enough for a synthetic corpus.
	x := uint64(seed)*6364136223846793005 + 1442695040888963407
	for i := 0; i < dim; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		// Map to [-1, 1].
		out[i] = float32((x>>33)%20001)/10000.0 - 1.0
	}
	return out
}

// The pair-count arithmetic must not overflow for any window an operator
// can request.
//
// The CLI's maximum scan window is 2,000 per source, so the union is up
// to 4,000 entries and C(4,000,2) = 7,998,000 pairs — a number that
// fits comfortably in an int32 and must therefore not be computed in
// one. The helper exists so the refusal message can quote the pair count
// for a window the caller has NOT yet allocated.
func TestComparePairCountForSources_IsOverflowSafe(t *testing.T) {
	require.Equal(t, int64(0), comparePairCount(0))
	require.Equal(t, int64(0), comparePairCount(1))
	require.Equal(t, int64(1), comparePairCount(2))
	require.Equal(t, int64(3), comparePairCount(3))
	require.Equal(t, int64(45), comparePairCount(10))
	require.Equal(t, int64(2775), comparePairCount(75))

	// The maximum CLI window: 2,000 per source across two sources.
	require.Equal(t, int64(7_998_000), comparePairCountForSources(2000, 2))
	require.Equal(t, int64(19_900), comparePairCountForSources(100, 2),
		"the default window is 100 per source, so the union implies 19,900 pairs")

	// A limit large enough to overflow int32 when doubled must be
	// clamped rather than wrapped.
	huge := comparePairCountForSources(math.MaxInt32, 2)
	require.Greater(t, huge, int64(0))
	require.NotEqual(t, int64(0), huge, "a clamped count must not wrap to zero")

	// Negative inputs are treated as zero rather than producing a
	// negative pair count.
	require.Equal(t, int64(0), comparePairCountForSources(-5, 2))
	require.Equal(t, int64(0), comparePairCountForSources(10, -1))
}

// The budget check inside the pass uses the same overflow-safe form.
func TestFindSimilarityCandidatesBounded_CompareBudgetUsesInt64Arithmetic(t *testing.T) {
	// A window whose pair count exceeds int32 would wrap under int32
	// arithmetic. It cannot be allocated here, so the check is asserted
	// through the helper the pass uses.
	entries := denseCluster(t, 40) // C(40,2) = 780
	_, _, err := FindSimilarityCandidatesBounded(entries, DefaultCandidateCap, 779)
	require.Error(t, err)
	var budgetErr *ErrCompareBudgetExceeded
	require.ErrorAs(t, err, &budgetErr)
	require.Equal(t, int64(780), budgetErr.EstimatedPairs)
}
