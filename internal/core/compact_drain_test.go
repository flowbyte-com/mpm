// compact_drain_test.go — drain-mode regression coverage for
// CompactEpistemologyDrain. The per-batch primitive
// (CompactEpistemology) keeps its existing test surface in
// compact_test.go; this file pins the drain loop behavior.
//
// Invariants pinned:
//
//  1. Empty substrate — drain is a successful no-op
//  2. <50 eligible rows — one batch
//  3. Exactly 50 eligible rows — one batch of 50
//  4. 100 eligible rows — two batches
//  5. 101 eligible rows — three batches (50 + 50 + 1)
//  6. 250 eligible rows — five full batches
//  7. force=false stops at threshold
//  8. force=true drains below threshold
//  9. LLM failure mid-drain leaves earlier batches committed
// 10. Validation failure mid-drain leaves earlier batches committed
// 11. Retry after partial failure resumes without duplicating work
// 12. Oldest-first ordering preserved across batches
// 13. max_batches safety cap stops at the cap with raw_remaining
// 14. Idempotency: full-success invocation + repeat = single-shot second time
// 15. Per-batch LLM input never exceeds compactBatchSize
// 16. Empty force=true invocation with no rows is a no-op
// 17. Per-batch primitive still works (regression guard)
//
// Threshold semantics: the default threshold in compactPreCheck is 100.
// Multi-batch tests use force=true so the threshold gate is bypassed;
// the explicit threshold-semantic tests (7, 8) use the default to
// verify the gate behaves correctly under each force mode.

package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// resetSubstate wipes memories and lessons so each test starts from
// a known empty substrate. lessons_base is the underlying table for
// the `lessons` view + INSTEAD OF triggers; deleting from the base
// is the canonical reset path used by compact_test.go too.
func resetSubstate(t *testing.T, dm *DatabaseManager) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(`DELETE FROM memories`); err != nil {
		t.Fatalf("clear memories: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`DELETE FROM lessons_base`); err != nil {
		t.Fatalf("clear lessons_base: %v", err)
	}
	// Reset any stale compaction config from prior tests.
	if _, err := dm.SQLDB().Exec(`DELETE FROM system_config WHERE key IN ('compaction','compaction.last_run')`); err != nil {
		t.Fatalf("clear compaction config: %v", err)
	}
}

// seedRaw inserts n raw memories with monotonically increasing
// created_at so oldest-first ordering is deterministic. Returns the
// IDs in insertion order (oldest first).
func seedRaw(t *testing.T, dm *DatabaseManager, n int) []string {
	t.Helper()
	resetSubstate(t, dm)
	ids := make([]string, 0, n)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("raw-drain-%05d", i)
		ids = append(ids, id)
		ts := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at)
			VALUES (?, 'memories', ?, ?, ?)
		`, id, "seed-"+id, ts, ts); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	return ids
}

// drainSynthOK returns canned valid lessons for every batch.
func drainSynthOK() func(ctx context.Context, raw []string) (string, error) {
	return func(ctx context.Context, raw []string) (string, error) {
		lesson := CompactLesson{
			Title: "Drain Lesson",
			Body:  "Body of " + strings.Repeat("x", len(raw)),
			Tags:  []string{"drain-test"},
		}
		b, _ := json.Marshal(lesson)
		return string(b), nil
	}
}

// ── 1. Empty substrate ──────────────────────────────────────────────────

func TestDrain_Empty(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 0)

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !res.Success {
		t.Errorf("Success: got false, want true (empty is no-op success)")
	}
	if res.BatchesProcessed != 0 {
		t.Errorf("BatchesProcessed: got %d, want 0", res.BatchesProcessed)
	}
	if res.RawProcessed != 0 {
		t.Errorf("RawProcessed: got %d, want 0", res.RawProcessed)
	}
	if res.LessonsCreated != 0 {
		t.Errorf("LessonsCreated: got %d, want 0", res.LessonsCreated)
	}
	if res.RawRemaining != 0 {
		t.Errorf("RawRemaining: got %d, want 0", res.RawRemaining)
	}
	if res.StopReason != "drained" {
		t.Errorf("StopReason: got %q, want %q", res.StopReason, "drained")
	}
	if res.SkippedReason != "no_raw_memories" {
		t.Errorf("SkippedReason: got %q, want %q", res.SkippedReason, "no_raw_memories")
	}
}

// ── 2. <50 eligible rows ────────────────────────────────────────────────

func TestDrain_SmallBatch(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 30)

	withMockSynth(t, drainSynthOK())

	// force=true bypasses the default threshold of 100 so the small
	// backlog actually drains.
	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !res.Success {
		t.Errorf("Success: got false, want true")
	}
	if res.BatchesProcessed != 1 {
		t.Errorf("BatchesProcessed: got %d, want 1", res.BatchesProcessed)
	}
	if res.RawProcessed != 30 {
		t.Errorf("RawProcessed: got %d, want 30", res.RawProcessed)
	}
	if res.LessonsCreated != 1 {
		t.Errorf("LessonsCreated: got %d, want 1", res.LessonsCreated)
	}
	if res.RawRemaining != 0 {
		t.Errorf("RawRemaining: got %d, want 0", res.RawRemaining)
	}
	if res.StopReason != "drained" {
		t.Errorf("StopReason: got %q, want %q", res.StopReason, "drained")
	}
	if len(res.LessonIDs) != 1 {
		t.Errorf("LessonIDs: got %d, want 1", len(res.LessonIDs))
	}
}

// ── 3. Exactly 50 eligible rows ─────────────────────────────────────────

func TestDrain_ExactBoundary(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 50)

	withMockSynth(t, drainSynthOK())

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.BatchesProcessed != 1 {
		t.Errorf("BatchesProcessed: got %d, want 1 (50 fits in one batch)", res.BatchesProcessed)
	}
	if res.RawProcessed != 50 {
		t.Errorf("RawProcessed: got %d, want 50", res.RawProcessed)
	}
	if res.RawRemaining != 0 {
		t.Errorf("RawRemaining: got %d, want 0", res.RawRemaining)
	}
	if res.StopReason != "drained" {
		t.Errorf("StopReason: got %q, want %q", res.StopReason, "drained")
	}
}

// ── 4. 100 eligible rows — two batches ─────────────────────────────────

func TestDrain_TwoFullBatches(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 100)

	withMockSynth(t, drainSynthOK())

	// 100 rows: iter 0 extracts 50, iter 1 extracts the remaining 50,
	// iter 2 sees raw=0 and skips. Two batches total.
	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.BatchesProcessed != 2 {
		t.Errorf("BatchesProcessed: got %d, want 2", res.BatchesProcessed)
	}
	if res.RawProcessed != 100 {
		t.Errorf("RawProcessed: got %d, want 100", res.RawProcessed)
	}
	if res.LessonsCreated != 2 {
		t.Errorf("LessonsCreated: got %d, want 2", res.LessonsCreated)
	}
	if res.RawRemaining != 0 {
		t.Errorf("RawRemaining: got %d, want 0", res.RawRemaining)
	}
	if res.StopReason != "drained" {
		t.Errorf("StopReason: got %q, want %q", res.StopReason, "drained")
	}
	if len(res.LessonIDs) != 2 {
		t.Errorf("LessonIDs: got %d, want 2", len(res.LessonIDs))
	}
}

// ── 5. 101 eligible rows — three batches (50 + 50 + 1) ───────────────

func TestDrain_WithRemainder(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 101)

	withMockSynth(t, drainSynthOK())

	// 101 rows: iter 0 → 50, iter 1 → 50, iter 2 → 1 (the partial
	// batch), iter 3 sees raw=0 and skips. Three batches, [50,50,1].
	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.BatchesProcessed != 3 {
		t.Errorf("BatchesProcessed: got %d, want 3 (50+50+1)", res.BatchesProcessed)
	}
	if res.RawProcessed != 101 {
		t.Errorf("RawProcessed: got %d, want 101", res.RawProcessed)
	}
	if res.LessonsCreated != 3 {
		t.Errorf("LessonsCreated: got %d, want 3", res.LessonsCreated)
	}
	if res.RawRemaining != 0 {
		t.Errorf("RawRemaining: got %d, want 0", res.RawRemaining)
	}
	if res.StopReason != "drained" {
		t.Errorf("StopReason: got %q, want %q", res.StopReason, "drained")
	}
}

// ── 6. 250 eligible rows — five full batches ─────────────────────────

func TestDrain_LargeBounded(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 250)

	// Track per-batch LLM input size — must never exceed compactBatchSize.
	var maxBatchSize int
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		if len(raw) > maxBatchSize {
			maxBatchSize = len(raw)
		}
		return drainSynthOK()(ctx, raw)
	})

	// 250 rows / 50 per batch = exactly 5 batches; iter 5 sees raw=0.
	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.BatchesProcessed != 5 {
		t.Errorf("BatchesProcessed: got %d, want 5 (250/50)", res.BatchesProcessed)
	}
	if res.RawProcessed != 250 {
		t.Errorf("RawProcessed: got %d, want 250", res.RawProcessed)
	}
	if maxBatchSize > compactBatchSize {
		t.Errorf("LLM input exceeded batch cap: got %d, want <=%d", maxBatchSize, compactBatchSize)
	}
}

// ── 7. force=false stops at threshold ─────────────────────────────────
//
// Default threshold is 100. With 200 eligible rows and force=false, the
// drain runs while raw > threshold and stops when raw <= threshold on
// the next pre-check: 200 → 150 → 100 → skip(below_threshold).

func TestDrain_ForceFalse_StopsAtThreshold(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 200)
	// Threshold stays at default 100.

	withMockSynth(t, drainSynthOK())

	res, err := dm.CompactEpistemologyDrain(context.Background(), false, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.BatchesProcessed != 2 {
		t.Errorf("BatchesProcessed: got %d, want 2 (drained to threshold)", res.BatchesProcessed)
	}
	if res.RawProcessed != 100 {
		t.Errorf("RawProcessed: got %d, want 100", res.RawProcessed)
	}
	if res.RawRemaining != 100 {
		t.Errorf("RawRemaining: got %d, want 100 (rows still eligible, now at threshold)", res.RawRemaining)
	}
	if res.StopReason != "drained" {
		t.Errorf("StopReason: got %q, want %q", res.StopReason, "drained")
	}
	if res.SkippedReason != "below_threshold" {
		t.Errorf("SkippedReason: got %q, want %q (first pre-check that found raw=100)", res.SkippedReason, "below_threshold")
	}
}

// ── 8. force=true drains below threshold ──────────────────────────────
//
// Default threshold 100. force=true drains every eligible row regardless
// of threshold. 130 rows → 50 + 50 + 30.

func TestDrain_ForceTrue_BypassesThreshold(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 130)
	// Threshold stays at default 100.

	withMockSynth(t, drainSynthOK())

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.BatchesProcessed != 3 {
		t.Errorf("BatchesProcessed: got %d, want 3 (50+50+30)", res.BatchesProcessed)
	}
	if res.RawProcessed != 130 {
		t.Errorf("RawProcessed: got %d, want 130", res.RawProcessed)
	}
	if res.RawRemaining != 0 {
		t.Errorf("RawRemaining: got %d, want 0", res.RawRemaining)
	}
	if res.StopReason != "drained" {
		t.Errorf("StopReason: got %q, want %q", res.StopReason, "drained")
	}
}

// ── 9. LLM failure mid-drain leaves earlier batches committed ───────

func TestDrain_LLMFailureMidDrain_PartialCommit(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 120)

	// 120 rows / 50 = 3 batches. Force the second batch to fail.
	callCount := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		callCount++
		if callCount == 2 {
			return "", errors.New("synthesize: simulated batch-2 failure")
		}
		return drainSynthOK()(ctx, raw)
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err == nil {
		t.Fatal("expected error from batch-2 LLM failure; got nil")
	}
	if !strings.Contains(err.Error(), "simulated batch-2 failure") {
		t.Errorf("error: got %v, want contains 'simulated batch-2 failure'", err)
	}

	// Partial aggregate: batch 1 was committed, batch 2 failed.
	if res.Success {
		t.Errorf("Success: got true, want false (mid-drain failure)")
	}
	if res.BatchesProcessed != 1 {
		t.Errorf("BatchesProcessed: got %d, want 1", res.BatchesProcessed)
	}
	if res.RawProcessed != 50 {
		t.Errorf("RawProcessed: got %d, want 50 (only batch 1)", res.RawProcessed)
	}
	if res.FailedBatch != 2 {
		t.Errorf("FailedBatch: got %d, want 2", res.FailedBatch)
	}
	if res.FailureReason == "" {
		t.Error("FailureReason: empty, want the underlying error")
	}
	if res.StopReason != "failed" {
		t.Errorf("StopReason: got %q, want %q", res.StopReason, "failed")
	}

	// Verify DB state: batch 1's 50 rows are marked, batch 2's 50 are
	// untouched, and 20 rows remain eligible.
	var marked, unmarked int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE json_extract(metadata,'$.compacted_into') IS NOT NULL`).Scan(&marked)
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE json_extract(metadata,'$.compacted_into') IS NULL AND deleted_at IS NULL`).Scan(&unmarked)
	if marked != 50 {
		t.Errorf("marked rows: got %d, want 50", marked)
	}
	if unmarked != 70 {
		t.Errorf("unmarked rows: got %d, want 70 (batch 2 = 50 + remaining 20)", unmarked)
	}

	// Verify the lesson from batch 1 is durable.
	var lessonCount int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM lessons`).Scan(&lessonCount)
	if lessonCount != 1 {
		t.Errorf("lessons: got %d, want 1 (only batch 1)", lessonCount)
	}
}

// ── 10. Validation failure mid-drain — same expectations ─────────────

func TestDrain_ValidationFailureMidDrain_PartialCommit(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 80)

	// 80 rows: iter 0 → 50, iter 1 → 30 (the partial batch). Force
	// the second batch to fail validation by returning the refusal
	// sentinel.
	callCount := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		callCount++
		if callCount == 2 {
			// Refusal sentinel — valid JSON, empty fields.
			return `{"title":"","body":"","tags":[]}`, nil
		}
		return drainSynthOK()(ctx, raw)
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err == nil {
		t.Fatal("expected error from batch-2 validation failure; got nil")
	}
	if res.BatchesProcessed != 1 {
		t.Errorf("BatchesProcessed: got %d, want 1", res.BatchesProcessed)
	}
	if res.RawProcessed != 50 {
		t.Errorf("RawProcessed: got %d, want 50", res.RawProcessed)
	}
	if res.FailedBatch != 2 {
		t.Errorf("FailedBatch: got %d, want 2", res.FailedBatch)
	}

	// DB state: batch 1 marked (50 rows), batch 2 untouched (30 rows).
	var marked, unmarked int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE json_extract(metadata,'$.compacted_into') IS NOT NULL`).Scan(&marked)
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE json_extract(metadata,'$.compacted_into') IS NULL AND deleted_at IS NULL`).Scan(&unmarked)
	if marked != 50 {
		t.Errorf("marked rows: got %d, want 50", marked)
	}
	if unmarked != 30 {
		t.Errorf("unmarked rows: got %d, want 30", unmarked)
	}
}

// ── 11. Retry after partial failure resumes without duplicating ─────

func TestDrain_RetryAfterFailure_ResumesFromRemaining(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 120)

	// First invocation: 120 rows. iter 0 succeeds (50), iter 1 fails.
	callCount := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		callCount++
		if callCount == 2 {
			return "", errors.New("synthesize: first-call batch-2 failure")
		}
		return drainSynthOK()(ctx, raw)
	})

	res1, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err == nil {
		t.Fatal("first drain: expected error, got nil")
	}
	if res1.BatchesProcessed != 1 {
		t.Errorf("first drain BatchesProcessed: got %d, want 1", res1.BatchesProcessed)
	}

	// Second invocation: 70 rows remain. iter 0 → 50, iter 1 → 20.
	withMockSynth(t, drainSynthOK())

	res2, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if res2.BatchesProcessed != 2 {
		t.Errorf("second drain BatchesProcessed: got %d, want 2 (50+20)", res2.BatchesProcessed)
	}
	if res2.RawProcessed != 70 {
		t.Errorf("second drain RawProcessed: got %d, want 70", res2.RawProcessed)
	}
	if res2.RawRemaining != 0 {
		t.Errorf("second drain RawRemaining: got %d, want 0", res2.RawRemaining)
	}

	// Verify: total lessons = 3 (1 from first drain + 2 from second),
	// no duplicates in compacted_into across the 120 raw rows.
	var lessonCount int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM lessons`).Scan(&lessonCount)
	if lessonCount != 3 {
		t.Errorf("lessons: got %d, want 3", lessonCount)
	}
	var unmarked int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE json_extract(metadata,'$.compacted_into') IS NULL AND deleted_at IS NULL`).Scan(&unmarked)
	if unmarked != 0 {
		t.Errorf("unmarked rows after retry: got %d, want 0", unmarked)
	}
}

// ── 12. Oldest-first ordering preserved across batches ──────────────

func TestDrain_OldestFirstOrdering(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedRaw(t, dm, 120)
	// Expected order: oldest 50 (ids[0:50]), then next 50
	// (ids[50:100]), then final 20 (ids[100:120]).

	withMockSynth(t, drainSynthOK())

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.BatchesProcessed != 3 {
		t.Fatalf("BatchesProcessed: got %d, want 3", res.BatchesProcessed)
	}

	// Build a map: memory_id → lesson_id it was compacted into.
	rows, err := dm.SQLDB().Query(`SELECT id, json_extract(metadata,'$.compacted_into') FROM memories`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	memToLesson := map[string]string{}
	for rows.Next() {
		var id, lesson string
		if err := rows.Scan(&id, &lesson); err != nil {
			t.Fatalf("scan: %v", err)
		}
		memToLesson[id] = lesson
	}

	// All 120 ids should be mapped.
	for _, id := range ids {
		if _, ok := memToLesson[id]; !ok {
			t.Errorf("memory %s not marked", id)
		}
	}

	// Verify ordering: each batch's lesson_id must cover a contiguous
	// slice of the input ids in order. Group by lesson_id and check
	// that the indices form a contiguous monotonic range.
	lessonToIndices := map[string][]int{}
	for i, id := range ids {
		lid := memToLesson[id]
		lessonToIndices[lid] = append(lessonToIndices[lid], i)
	}
	if len(lessonToIndices) != 3 {
		t.Fatalf("lessons: got %d, want 3", len(lessonToIndices))
	}
	for lid, idxs := range lessonToIndices {
		for k := 1; k < len(idxs); k++ {
			if idxs[k] != idxs[k-1]+1 {
				t.Errorf("lesson %s: indices not contiguous: %v", lid, idxs)
				break
			}
		}
		if len(idxs) > 50 {
			t.Errorf("lesson %s: batch size %d exceeds 50", lid, len(idxs))
		}
	}
}

// ── 13a. max_batches safety cap stops at the cap ────────────────────

func TestDrain_MaxBatchesCap(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 1000)

	withMockSynth(t, drainSynthOK())

	// Cap at 3 batches = 150 raw max.
	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 3)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.BatchesProcessed != 3 {
		t.Errorf("BatchesProcessed: got %d, want 3", res.BatchesProcessed)
	}
	if res.RawProcessed != 150 {
		t.Errorf("RawProcessed: got %d, want 150", res.RawProcessed)
	}
	if res.StopReason != "max_batches" {
		t.Errorf("StopReason: got %q, want %q", res.StopReason, "max_batches")
	}
	if res.RawRemaining != 850 {
		t.Errorf("RawRemaining: got %d, want 850 (1000 - 150)", res.RawRemaining)
	}
}

// ── 13b. max_batches hard cap clamps absurd requests ──────────────

func TestDrain_MaxBatchesHardCap(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 10000)

	withMockSynth(t, drainSynthOK())

	// Caller requests 1_000_000 — should be capped at the hard ceiling.
	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 1_000_000)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.BatchesProcessed != compactDrainMaxBatchesHardCap {
		t.Errorf("BatchesProcessed: got %d, want %d (hard cap)", res.BatchesProcessed, compactDrainMaxBatchesHardCap)
	}
	if res.BatchesProcessed > compactDrainMaxBatchesHardCap {
		t.Errorf("BatchesProcessed exceeded hard cap: got %d, want <=%d",
			res.BatchesProcessed, compactDrainMaxBatchesHardCap)
	}
}

// ── 14. Idempotency — repeat call after full success is a no-op ─────

func TestDrain_IdempotentAfterFullSuccess(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 100)

	withMockSynth(t, drainSynthOK())

	// First drain: 100 rows / 50 per batch = 2 batches.
	res1, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if res1.BatchesProcessed != 2 {
		t.Errorf("first drain BatchesProcessed: got %d, want 2", res1.BatchesProcessed)
	}

	// Second drain: should be a no-op (no eligible rows).
	res2, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if res2.BatchesProcessed != 0 {
		t.Errorf("second drain BatchesProcessed: got %d, want 0", res2.BatchesProcessed)
	}
	if res2.RawProcessed != 0 {
		t.Errorf("second drain RawProcessed: got %d, want 0", res2.RawProcessed)
	}
	if res2.SkippedReason != "no_raw_memories" {
		t.Errorf("second drain SkippedReason: got %q, want %q", res2.SkippedReason, "no_raw_memories")
	}
	if res2.StopReason != "drained" {
		t.Errorf("second drain StopReason: got %q, want %q", res2.StopReason, "drained")
	}

	// Lesson count unchanged: exactly 2 lessons, not 4.
	var lessonCount int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM lessons`).Scan(&lessonCount)
	if lessonCount != 2 {
		t.Errorf("lessons after repeat: got %d, want 2 (no duplicate synthesis)", lessonCount)
	}
}

// ── 15. Per-batch LLM input never exceeds compactBatchSize ─────────

func TestDrain_PerBatchSizeBounded(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 250)

	var observedSizes []int
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		observedSizes = append(observedSizes, len(raw))
		return drainSynthOK()(ctx, raw)
	})

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.BatchesProcessed != 5 {
		t.Errorf("BatchesProcessed: got %d, want 5", res.BatchesProcessed)
	}
	// 250 rows / 50 per batch = 5 calls. Each call extracts exactly 50
	// (the last batch extracts the remaining 50, not 0 — extractRawBatch
	// returns whatever is left, capped at compactBatchSize).
	if len(observedSizes) != 5 {
		t.Fatalf("synth calls: got %d, want 5 (250/50)", len(observedSizes))
	}
	for i, n := range observedSizes {
		if n > compactBatchSize {
			t.Errorf("batch %d size %d exceeds %d", i, n, compactBatchSize)
		}
		if n != compactBatchSize {
			t.Errorf("batch %d size: got %d, want %d (every batch extracts the cap)", i, n, compactBatchSize)
		}
	}
}

// ── 16. Empty force=true invocation with no rows is a no-op ────────

func TestDrain_ForceEmptyNoOp(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 0)

	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.BatchesProcessed != 0 {
		t.Errorf("BatchesProcessed: got %d, want 0", res.BatchesProcessed)
	}
	if res.SkippedReason != "no_raw_memories" {
		t.Errorf("SkippedReason: got %q, want %q", res.SkippedReason, "no_raw_memories")
	}
}

// ── 17. CompactEpistemology (per-batch primitive) still works ─────
//
// Re-pinned here to guard against accidental breakage of the
// primitive by the drain wrapper. The full test surface for the
// primitive is in compact_test.go; this is a smoke check that
// drain and primitive still coexist.

func TestDrain_PrimitiveStillCallable(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 5)
	// Threshold lowered so the primitive fires on 5 rows.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO system_config (key, raw_json, content_hash)
		VALUES ('compaction', '{"raw_threshold":1}', '')
	`); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	withMockSynth(t, drainSynthOK())

	res, err := dm.CompactEpistemology(context.Background(), false)
	if err != nil {
		t.Fatalf("primitive: %v", err)
	}
	if res == nil {
		t.Fatal("result nil; expected non-nil on success")
	}
	if res.SkippedReason != "" {
		t.Errorf("primitive skipped unexpectedly: %q", res.SkippedReason)
	}
	if res.Compacted != 5 {
		t.Errorf("primitive Compacted: got %d, want 5", res.Compacted)
	}
	if res.LessonsCreated != 1 {
		t.Errorf("primitive LessonsCreated: got %d, want 1", res.LessonsCreated)
	}
}
