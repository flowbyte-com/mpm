// compact_split_test.go — Phase 4 of the compact-refusal lifecycle.
//
// Bounded refusal recovery (docs/archive/2026-09-30-compact-refusal-lifecycle.md
// §5): one deterministic positional split, at most two child attempts,
// then deferral. No recursion, no clustering, no embeddings.
//
// The load-bearing property is the stage budget: a drain invocation
// spends at most MaxSemanticStagesPerInvocation (8) model attempts, and
// the invariant is structural — both budget checks happen BEFORE the
// spend, so there is no path on which the counter reaches 9.

package internal

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// ── the split rule ───────────────────────────────────────────────────

// The split is positional and total, with the larger child on the
// right. Pinned at every size the drain can actually produce.
func TestSplitRows_DeterministicPositional(t *testing.T) {
	cases := []struct {
		size        int
		wantLeft    int
		wantRight   int
		wantNoSplit bool
	}{
		{size: 50, wantLeft: 25, wantRight: 25},
		{size: 49, wantLeft: 24, wantRight: 25}, // odd: larger child goes right
		{size: 51, wantLeft: 25, wantRight: 26},
		{size: 2, wantLeft: 1, wantRight: 1},
		{size: 1, wantNoSplit: true},
		{size: 0, wantNoSplit: true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("size_%d", tc.size), func(t *testing.T) {
			rows := make([]string, tc.size)
			for i := range rows {
				rows[i] = fmt.Sprintf("row-%02d", i)
			}

			left, right, _, _, ok := splitRowSet(rows, rows)

			if tc.wantNoSplit {
				if ok {
					t.Fatalf("split attempted on %d rows; want no split", tc.size)
				}
				return
			}
			if !ok {
				t.Fatalf("no split on %d rows, want a split", tc.size)
			}
			if len(left) != tc.wantLeft || len(right) != tc.wantRight {
				t.Errorf("split = %d/%d, want %d/%d", len(left), len(right), tc.wantLeft, tc.wantRight)
			}
			// Total: no row is lost or duplicated. This is the property
			// that makes "both halves refused → all 50 deferred"
			// actually mean all 50.
			if len(left)+len(right) != tc.size {
				t.Errorf("split lost rows: %d + %d != %d", len(left), len(right), tc.size)
			}
			// Contiguous and in order, so a refusal of one half says
			// nothing about the other.
			for i, id := range left {
				if id != rows[i] {
					t.Errorf("left[%d] = %s, want %s — the split must be a positional prefix", i, id, rows[i])
				}
			}
			for i, id := range right {
				if id != rows[tc.wantLeft+i] {
					t.Errorf("right[%d] = %s, want %s — the split must be a positional suffix", i, id, rows[tc.wantLeft+i])
				}
			}
		})
	}
}

// Two runs on the same backlog split identically. That is what makes
// the cost predictable and the behaviour testable.
func TestSplitRows_IsDeterministic(t *testing.T) {
	rows := make([]string, 37)
	for i := range rows {
		rows[i] = fmt.Sprintf("row-%02d", i)
	}
	l1, r1, _, _, _ := splitRowSet(rows, rows)
	l2, r2, _, _, _ := splitRowSet(rows, rows)
	for i := range l1 {
		if l1[i] != l2[i] || r1[i] != r2[i] {
			t.Fatalf("split is not deterministic at index %d", i)
		}
	}
}

// ── the stage budget ─────────────────────────────────────────────────

// The headline invariant: a drain never spends more than 8 stages, on
// any path, including the total-failure path where every batch refuses
// AND subdivides.
func TestDrain_StageBudgetNeverExceedsEight(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 400)

	calls := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		calls++
		return testRefusalSentinel, nil // refuse everything
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if calls > MaxSemanticStagesPerInvocation {
		t.Errorf("drain spent %d stages, want <= %d", calls, MaxSemanticStagesPerInvocation)
	}
	if res.StagesSpent != calls {
		t.Errorf("StagesSpent = %d, but %d model calls were made — the accounting must be honest", res.StagesSpent, calls)
	}
	if res.StopReason != "max_batches_reached" {
		t.Errorf("StopReason = %q, want max_batches_reached", res.StopReason)
	}
}

// 9 would be the off-by-one. Assert the exact bound, not just "at most".
func TestDrain_StageBudgetIsExactlyEightOnTotalFailure(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 1000)

	calls := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		calls++
		return testRefusalSentinel, nil
	})

	if _, err := dm.CompactEpistemologyDrain(context.Background(), true, 0); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if calls != MaxSemanticStagesPerInvocation {
		t.Errorf("drain spent %d stages on the total-failure path, want exactly %d", calls, MaxSemanticStagesPerInvocation)
	}
}

// A batch is never abandoned half-processed. If the budget cannot
// afford both children, the WHOLE batch is deferred — the design's
// explicit alternative to spending a stage on the first child and
// leaving the second pending, which is the F6 failure in miniature.
//
// This is observed at stage 7: batch 5's parent refusal leaves 1 stage,
// which cannot afford 2 children, so that batch must be deferred whole.
func TestDrain_UnaffordableSplitDefersWholeBatch(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 400) // 8 full batches of 50

	calls := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		calls++
		return testRefusalSentinel, nil
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	// 6 stages on two subdivided batches (3+3), then batches 3 and 4
	// admitted at 1 stage each and deferred whole (7, 8). Batches 5
	// and 6 are never admitted.
	if calls != 8 {
		t.Fatalf("stages = %d, want 8", calls)
	}

	// Every row must be accounted for: compacted, deferred, or still
	// pending — and nothing half-done.
	pressure := readPressure(t, dm)
	if pressure.RawCount != 400 {
		t.Errorf("raw_count = %d, want 400", pressure.RawCount)
	}
	// Batches 1-2: both halves refused → all 100 deferred.
	// Batches 3-4: refused, split unaffordable → 100 deferred.
	// Batches 5-8: never attempted → 200 still actionable.
	if pressure.DeferredCount != 200 {
		t.Errorf("deferred_count = %d, want 200", pressure.DeferredCount)
	}
	if pressure.ActionablePending != 200 {
		t.Errorf("actionable_pending = %d, want 200", pressure.ActionablePending)
	}
	if res.RowsDeferred != 200 {
		t.Errorf("RowsDeferred = %d, want 200", res.RowsDeferred)
	}
}

// ── recovery: a split that succeeds salvages rows ────────────────────

// The whole point of choosing B over A: a contradictory pair inside an
// otherwise-coherent batch must not cost the other 48 rows.
func TestDrain_SplitRecoversACoherentHalf(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 50)

	// Parent refuses. Left half succeeds. Right half refuses.
	call := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		call++
		if len(raw) == 50 {
			return testRefusalSentinel, nil
		}
		if len(raw) == 25 && call == 2 {
			return `{"title":"Half of it is coherent","body":"The first 25 agree.","tags":["partial"]}`, nil
		}
		return testRefusalSentinel, nil
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.LessonsCreated != 1 {
		t.Errorf("LessonsCreated = %d, want 1 — the coherent half should still compact", res.LessonsCreated)
	}
	if res.RawProcessed != 25 {
		t.Errorf("RawProcessed = %d, want 25", res.RawProcessed)
	}
	// The refused half is deferred, not retried.
	pressure := readPressure(t, dm)
	if pressure.DeferredCount != 25 {
		t.Errorf("deferred_count = %d, want 25", pressure.DeferredCount)
	}
	if pressure.ActionablePending != 0 {
		t.Errorf("actionable_pending = %d, want 0", pressure.ActionablePending)
	}
	// And no row was written compacted_into for the refused half.
	if n := countCompacted(t, dm); n != 25 {
		t.Errorf("compacted rows = %d, want 25", n)
	}
}

// Both halves refusing defers the whole batch — 50 rows in two atomic
// groups, one per child.
//
// Two groups, not one, and that is the correct shape: each child is its
// own deferral with its own atomicity boundary and its own batch id, so
// "these 25 were refused together for this reason" remains answerable.
// Collapsing them into a single id would assert a grouping that no
// single atomic operation actually produced.
func TestDrain_BothHalvesRefusedDefersWholeBatch(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedRaw(t, dm, 50)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return testRefusalSentinel, nil
	})

	if _, err := dm.CompactEpistemologyDrain(context.Background(), true, 0); err != nil {
		t.Fatalf("drain: %v", err)
	}

	pressure := readPressure(t, dm)
	if pressure.DeferredCount != 50 {
		t.Errorf("deferred_count = %d, want 50 — both halves refused, so the whole batch is deferred", pressure.DeferredCount)
	}

	// Exactly two groups, corresponding to the two children, each
	// internally consistent.
	batches := map[string]int{}
	for _, id := range ids {
		m := deferralMetadata(t, dm, id)
		if m.Batch == "" {
			t.Fatalf("row %s has no batch id", id)
		}
		if m.Reason != DeferralReasonRefusal {
			t.Errorf("row %s reason = %q, want %q", id, m.Reason, DeferralReasonRefusal)
		}
		batches[m.Batch]++
	}
	if len(batches) != 2 {
		t.Errorf("got %d deferral groups, want 2 (one per child): %v", len(batches), batches)
	}
	for batch, n := range batches {
		if n != 25 {
			t.Errorf("group %s has %d rows, want 25", batch, n)
		}
	}
}

// ── terminal-state accounting ────────────────────────────────────────

// A single-row batch cannot be split, so it defers on the parent's
// refusal at a cost of 1 stage rather than 3. A naive "always split"
// implementation would make two empty halves and a pointless second
// call.
func TestDrain_SingleRowBatchDefersWithoutSplitting(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 1)

	calls := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		calls++
		return testRefusalSentinel, nil
	})

	if _, err := dm.CompactEpistemologyDrain(context.Background(), true, 0); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if calls != 1 {
		t.Errorf("made %d model calls for a 1-row batch, want exactly 1", calls)
	}
	pressure := readPressure(t, dm)
	if pressure.DeferredCount != 1 {
		t.Errorf("deferred_count = %d, want 1", pressure.DeferredCount)
	}
}

// A 2-row batch splits to 1/1. Both children cannot succeed on their own
// material, so both refuse and both defer — and the drain still
// terminates rather than recursing further.
func TestDrain_TwoRowBatchSplitsToOneOne(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 2)

	calls := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		calls++
		return testRefusalSentinel, nil
	})

	if _, err := dm.CompactEpistemologyDrain(context.Background(), true, 0); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if calls != 3 {
		t.Errorf("made %d calls for a 2-row batch, want 3 (parent + two singletons)", calls)
	}
	pressure := readPressure(t, dm)
	if pressure.DeferredCount != 2 {
		t.Errorf("deferred_count = %d, want 2", pressure.DeferredCount)
	}
}

// ── the happy path is untouched ──────────────────────────────────────

// A successful batch costs exactly 1 stage and subdivides never.
func TestDrain_SuccessCostsOneStagePerBatch(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 200) // 4 full batches

	calls := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		calls++
		return `{"title":"T","body":"B","tags":["x"]}`, nil
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if calls != 4 {
		t.Errorf("made %d calls for 4 successful batches, want 4", calls)
	}
	if res.StagesSpent != 4 {
		t.Errorf("StagesSpent = %d, want 4", res.StagesSpent)
	}
	if res.BatchesProcessed != 4 {
		t.Errorf("BatchesProcessed = %d, want 4", res.BatchesProcessed)
	}
	if res.RowsDeferred != 0 {
		t.Errorf("RowsDeferred = %d, want 0", res.RowsDeferred)
	}
}

// ── errors stay loud ─────────────────────────────────────────────────

// A genuine error is NOT a refusal and is NOT subdivided. It stops the
// drain and reports, and its rows stay pending — the design's named
// residual, kept visible rather than converted into a deferral.
func TestDrain_ErrorStopsTheDrainAndIsNotSubdivided(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 200)

	providerErr := errors.New("connection refused")
	calls := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		calls++
		if calls == 2 {
			return "", providerErr
		}
		return `{"title":"T","body":"B","tags":["x"]}`, nil
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err == nil {
		t.Fatal("err = nil, want the provider error to surface")
	}
	if !errors.Is(err, providerErr) {
		t.Errorf("err = %v, want it to wrap %v", err, providerErr)
	}
	if res.StopReason != "failure" {
		t.Errorf("StopReason = %q, want failure", res.StopReason)
	}
	// One attempt, not three: an error is not something a split fixes.
	if calls != 2 {
		t.Errorf("made %d calls, want 2 — an erroring batch must not be subdivided", calls)
	}

	pressure := readPressure(t, dm)
	if pressure.DeferredCount != 0 {
		t.Errorf("deferred_count = %d, want 0 — an operational failure must not be recorded as a considered refusal", pressure.DeferredCount)
	}
	// The errored batch's rows stay pending, and stay visible.
	if pressure.ActionablePending != 150 {
		t.Errorf("actionable_pending = %d, want 150 (batch 1 compacted, errored batch still pending)", pressure.ActionablePending)
	}
}

// ── helper ───────────────────────────────────────────────────────────

func countCompacted(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories
		 WHERE json_extract(metadata, '$.compacted_into') IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count compacted: %v", err)
	}
	return n
}

// ── the progress invariant (Phase 5) ─────────────────────────────────

// C5: one sanctioned refusal can never prevent later pending memories
// from being considered forever.
//
// The claim is structural rather than empirical — every terminal state
// writes to the extraction predicate — so this test walks the three
// states and asserts that each one shrinks the actionable pool.
func TestProgressInvariant_EveryTerminalStateShrinksThePool(t *testing.T) {
	cases := []struct {
		name         string
		respond      func(ctx context.Context, raw []string) (string, error)
		wantPool     int
		wantDeferred int
	}{
		{
			name: "lesson committed leaves the pool",
			respond: func(ctx context.Context, raw []string) (string, error) {
				return `{"title":"T","body":"B","tags":["x"]}`, nil
			},
			wantPool: 0, wantDeferred: 0,
		},
		{
			name: "refusal deferred leaves the pool",
			respond: func(ctx context.Context, raw []string) (string, error) {
				return testRefusalSentinel, nil
			},
			wantPool: 0, wantDeferred: 50,
		},
		{
			name: "error stops the drain loudly and does NOT shrink the pool",
			respond: func(ctx context.Context, raw []string) (string, error) {
				return "", errors.New("upstream 503")
			},
			wantPool: 50, wantDeferred: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := NewTestDM(t)
			seedRaw(t, dm, 50)
			withMockSynth(t, tc.respond)

			_, _ = dm.CompactEpistemologyDrain(context.Background(), true, 0)

			pressure := readPressure(t, dm)
			if pressure.ActionablePending != tc.wantPool {
				t.Errorf("actionable_pending = %d, want %d", pressure.ActionablePending, tc.wantPool)
			}
			if pressure.DeferredCount != tc.wantDeferred {
				t.Errorf("deferred_count = %d, want %d", pressure.DeferredCount, tc.wantDeferred)
			}
		})
	}
}

// The end-to-end F6 scenario: a fully-refusing backlog drains to
// actionable_pending == 0. Before this lifecycle it never converged —
// each drain reselected the same rows and got the same refusal.
func TestProgressInvariant_FullyRefusingBacklogConverges(t *testing.T) {
	dm := NewTestDM(t)
	// 110 rows: batches of 50, 50, and 10 — the F6 shape.
	seedRaw(t, dm, 110)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return testRefusalSentinel, nil
	})

	// More than one invocation, to prove it is not a one-invocation
	// coincidence but an actual fixed point.
	for i := 0; i < 3; i++ {
		if _, err := dm.CompactEpistemologyDrain(context.Background(), true, 0); err != nil {
			t.Fatalf("invocation %d: %v", i, err)
		}
		if got := readPressure(t, dm).ActionablePending; got != 0 {
			t.Fatalf("invocation %d: actionable_pending = %d, want 0 — the backlog must converge", i, got)
		}
	}

	pressure := readPressure(t, dm)
	if pressure.DeferredCount != 110 {
		t.Errorf("deferred_count = %d, want 110", pressure.DeferredCount)
	}
	// raw_count is unchanged: deferring shrinks what is actionable,
	// not what is outstanding.
	if pressure.RawCount != 110 {
		t.Errorf("raw_count = %d, want 110 — raw_count measures backlog, not actionability", pressure.RawCount)
	}
	if pressure.ActionablePending != 0 {
		t.Errorf("actionable_pending = %d, want 0", pressure.ActionablePending)
	}
}

// A later batch is still reached after an earlier one refuses. Without
// this, a refusal could block later rows across invocations — the
// invariant is about a single refusal not being able to hold the pool.
func TestProgressInvariant_RefusalDoesNotBlockLaterRows(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 150) // 3 full batches

	// Batch 1 refuses and subdivides; batches 2 and 3 succeed.
	call := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		call++
		if len(raw) == 50 && call == 1 {
			return testRefusalSentinel, nil
		}
		return `{"title":"T","body":"B","tags":["x"]}`, nil
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	// The refused batch cost 3 stages; the two after it cost 1 each.
	// 5 stages total, well inside the budget, so both later batches ran.
	if res.LessonsCreated < 2 {
		t.Errorf("LessonsCreated = %d, want >= 2 — later batches must still be reached", res.LessonsCreated)
	}
	pressure := readPressure(t, dm)
	if pressure.ActionablePending != 0 {
		t.Errorf("actionable_pending = %d, want 0", pressure.ActionablePending)
	}
}
