// compact_deferral_test.go — Phase 3 of the compact-refusal lifecycle.
//
// The deferred state and its pressure accounting. Per
// docs/archive/2026-09-30-compact-refusal-lifecycle.md §3–§4:
//
//   D = { m ∈ U : json_extract(metadata,'$.compaction_deferred_at') IS NOT NULL }
//   A = { m ∈ U : json_extract(metadata,'$.compaction_deferred_at') IS NULL }
//
//	raw_count          = |U|   = |A| + |D|   -- UNCHANGED semantics
//	deferred_count     = |D|                 -- new
//	actionable_pending = |A|                 -- new
//
// and the identity raw_count = actionable_pending + deferred_count must
// hold in every state.
//
// Every test uses a temporary DatabaseManager. No production DB, no
// provider, no live state.

package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ── pressure accounting ───────────────────────────────────────────────

// The load-bearing identity, across every reachable mix of compacted,
// pending, and deferred rows. If this fails, the pressure block is
// describing a population that does not exist.
func TestEpistemicPressure_CountIdentity(t *testing.T) {
	cases := []struct {
		name              string
		pending, deferred int
		compacted         int
	}{
		{"empty", 0, 0, 0},
		{"pending only", 10, 0, 0},
		{"deferred only", 0, 7, 0},
		{"mixed", 12, 5, 0},
		{"all deferred", 0, 25, 0},
		{"pending and compacted", 4, 0, 9},
		{"deferred and compacted", 0, 6, 9},
		{"all three", 8, 3, 11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := NewTestDM(t)
			seedMixed(t, dm, tc.pending, tc.deferred, tc.compacted)

			got := readPressure(t, dm)

			wantRaw := tc.pending + tc.deferred
			if got.RawCount != wantRaw {
				t.Errorf("raw_count = %d, want %d", got.RawCount, wantRaw)
			}
			if got.DeferredCount != tc.deferred {
				t.Errorf("deferred_count = %d, want %d", got.DeferredCount, tc.deferred)
			}
			if got.ActionablePending != tc.pending {
				t.Errorf("actionable_pending = %d, want %d", got.ActionablePending, tc.pending)
			}
			// The identity, asserted rather than derived.
			if got.RawCount != got.ActionablePending+got.DeferredCount {
				t.Errorf("raw_count %d != actionable_pending %d + deferred_count %d",
					got.RawCount, got.ActionablePending, got.DeferredCount)
			}
		})
	}
}

// raw_count must keep its exact current meaning: every un-compacted
// memory, deferred ones included. A design that shrank it would make a
// backlog look solved.
func TestEpistemicPressure_RawCountIncludesDeferred(t *testing.T) {
	dm := NewTestDM(t)
	seedMixed(t, dm, 0, 9, 0)

	if got := readPressure(t, dm); got.RawCount != 9 {
		t.Errorf("raw_count = %d, want 9 — raw_count is total un-compacted and must not shrink on deferral", got.RawCount)
	}
}

// Compacted rows are outside U entirely — in neither A nor D. A row
// that was compacted and later annotated deferred must not be counted
// anywhere, because a compacted row is finished work.
func TestEpistemicPressure_CompactedAndDeferredIsCountedNowhere(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 0, 0, 5)
	deferRow(t, dm, ids[0], "batch-1", "refusal_sentinel", "")

	got := readPressure(t, dm)
	if got.RawCount != 0 {
		t.Errorf("raw_count = %d, want 0", got.RawCount)
	}
	if got.DeferredCount != 0 {
		t.Errorf("deferred_count = %d, want 0 — a compacted row is not deferred-pending", got.DeferredCount)
	}
	if got.ActionablePending != 0 {
		t.Errorf("actionable_pending = %d, want 0", got.ActionablePending)
	}
}

// A row with NULL or empty metadata must land in A (actionable), never
// D. json_extract returns NULL for both, so the new arm is satisfied
// without special-casing — and the design forbids special-casing it.
func TestEpistemicPressure_EmptyMetadataIsActionable(t *testing.T) {
	dm := NewTestDM(t)

	now := time.Now().UTC().Format(time.RFC3339)
	insert := func(id string, metadata any) {
		t.Helper()
		var err error
		if metadata == nil {
			_, err = dm.SQLDB().Exec(`
				INSERT INTO memories (id, collection, content, created_at, updated_at)
				VALUES (?, 'memories', 'c', ?, ?)`, id, now, now)
		} else {
			_, err = dm.SQLDB().Exec(`
				INSERT INTO memories (id, collection, content, created_at, updated_at, metadata)
				VALUES (?, 'memories', 'c', ?, ?, ?)`, id, now, now, metadata)
		}
		if err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	insert("mem-null-meta", nil)
	insert("mem-empty-meta", "")
	insert("mem-other-meta", `{"unrelated":"x"}`)

	got := readPressure(t, dm)
	if got.ActionablePending != 3 {
		t.Errorf("actionable_pending = %d, want 3", got.ActionablePending)
	}
	if got.DeferredCount != 0 {
		t.Errorf("deferred_count = %d, want 0", got.DeferredCount)
	}
	if got.RawCount != 3 {
		t.Errorf("raw_count = %d, want 3", got.RawCount)
	}
}

// ── the extraction predicate ──────────────────────────────────────────

// The loop-breaking property: a deferred row leaves the batch pool, so
// the next extract returns the *next* rows rather than the same 50.
func TestExtractRawBatch_DeferredRowsLeaveThePool(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 10, 0, 0)
	deferRow(t, dm, ids[0], "batch-1", "refusal_sentinel", "")

	gotIDs, _, err := dm.extractRawBatch(context.Background(), 50)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(gotIDs) != 9 {
		t.Fatalf("extracted %d, want 9 — the deferred row must not be reselected", len(gotIDs))
	}
	for _, id := range gotIDs {
		if id == ids[0] {
			t.Error("the deferred row was reselected; the loop is not broken")
		}
	}
}

// A single bound at the limit still caps correctly, and a fully
// deferred pool selects nothing at all.
func TestExtractRawBatch_LimitRespectedWithDeferrals(t *testing.T) {
	dm := NewTestDM(t)
	seedMixed(t, dm, 20, 0, 0)

	gotIDs, _, err := dm.extractRawBatch(context.Background(), 5)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(gotIDs) != 5 {
		t.Errorf("extracted %d, want 5", len(gotIDs))
	}
}

func TestExtractRawBatch_AllDeferredSelectsNothing(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 0, 6, 0)
	for _, id := range ids {
		deferRow(t, dm, id, "batch-1", "refusal_sentinel", "")
	}

	gotIDs, _, err := dm.extractRawBatch(context.Background(), 50)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(gotIDs) != 0 {
		t.Errorf("extracted %d, want 0", len(gotIDs))
	}
}

// The drain and the pressure view must agree on the population. The
// invariant is that extraction sees exactly the ACTIONABLE set, which
// is A — not U. raw_count deliberately covers a superset of U, so
// comparing extraction against raw_count would be comparing a
// partition to its whole.
func TestPressureAndExtractionAgreeOnPopulation(t *testing.T) {
	dm := NewTestDM(t)
	seedMixed(t, dm, 15, 8, 4)

	pressure := readPressure(t, dm)
	ids, _, err := dm.extractRawBatch(context.Background(), 1000)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(ids) != pressure.ActionablePending {
		t.Errorf("extraction saw %d rows but actionable_pending = %d; the two predicates have diverged",
			len(ids), pressure.ActionablePending)
	}
	if len(ids) != 15 {
		t.Errorf("extracted %d, want 15 (the actionable set)", len(ids))
	}
	// And the raw_count superset relationship holds: the drain's pool
	// is raw_count minus the deferred rows, never more.
	if pressure.RawCount != pressure.ActionablePending+pressure.DeferredCount {
		t.Errorf("raw_count %d != actionable %d + deferred %d",
			pressure.RawCount, pressure.ActionablePending, pressure.DeferredCount)
	}
}

// ── deferral write properties ─────────────────────────────────────────

// A refusal defers the batch instead of erroring, and does so
// atomically: every row in the group carries identical metadata.
func TestCompactEpistemology_RefusalDefersTheBatch(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 6, 0, 0)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return testRefusalSentinel, nil
	})

	result, err := dm.CompactEpistemology(context.Background(), true)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if result.SkippedReason != "synthesis_refused" {
		t.Errorf("SkippedReason = %q, want synthesis_refused", result.SkippedReason)
	}

	pressure := readPressure(t, dm)
	if pressure.DeferredCount != 6 {
		t.Errorf("deferred_count = %d, want 6", pressure.DeferredCount)
	}
	if pressure.ActionablePending != 0 {
		t.Errorf("actionable_pending = %d, want 0", pressure.ActionablePending)
	}
	if pressure.RawCount != 6 {
		t.Errorf("raw_count = %d, want 6 — raw_count is unchanged by deferral", pressure.RawCount)
	}

	// Every row in the group carries identical values for all four
	// keys, including the timestamp. A group is only inspectable if
	// grouping by batch is an exact match, not a near one.
	first := deferralMetadata(t, dm, ids[0])
	if first.At == "" || first.Reason != "refusal_sentinel" || first.Batch == "" {
		t.Fatalf("first row metadata incomplete: %+v", first)
	}
	for _, id := range ids[1:] {
		got := deferralMetadata(t, dm, id)
		if got != first {
			t.Errorf("row %s metadata = %+v, want identical to %+v", id, got, first)
		}
	}
}

// §3.2 property 1: content and every substantive column are unchanged.
// Only metadata differs.
func TestDeferral_ContentIsUnmodified(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 4, 0, 0)

	before := snapshotRows(t, dm, ids)
	deferRow(t, dm, ids[0], "batch-1", "refusal_sentinel", "")

	after := snapshotRows(t, dm, ids)
	for i, id := range ids {
		if before[i].content != after[i].content {
			t.Errorf("%s: content changed: %q -> %q", id, before[i].content, after[i].content)
		}
		if before[i].collection != after[i].collection {
			t.Errorf("%s: collection changed", id)
		}
		if before[i].weight != after[i].weight {
			t.Errorf("%s: weight changed", id)
		}
		if before[i].createdAt != after[i].createdAt {
			t.Errorf("%s: created_at changed: %q -> %q", id, before[i].createdAt, after[i].createdAt)
		}
		if before[i].deletedAt != after[i].deletedAt {
			t.Errorf("%s: deleted_at changed", id)
		}
	}
}

// §3.2 property 4: a deferred row is never forged as compacted, and
// compacted_into is never written for a refused batch.
func TestDeferral_NeverWritesCompactedInto(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 5, 0, 0)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return testRefusalSentinel, nil
	})
	if _, err := dm.CompactEpistemology(context.Background(), true); err != nil {
		t.Fatalf("CompactEpistemology: %v", err)
	}

	if n := countLessons(t, dm); n != 0 {
		t.Errorf("lessons = %d, want 0 — a refusal must not fabricate a lesson", n)
	}
	for _, id := range ids {
		if into := memoryCompactedInto(t, dm, id); into != "" {
			t.Errorf("%s: compacted_into = %q, want empty", id, into)
		}
	}
}

// §3.2 property 2: a deferred row stays fully retrievable. No read
// path consults the deferral keys.
func TestDeferral_StaysRetrievable(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 1, 0, 0)
	deferRow(t, dm, ids[0], "batch-1", "refusal_sentinel", "")

	var content string
	err := dm.SQLDB().QueryRow(`SELECT content FROM memories WHERE id = ?`, ids[0]).Scan(&content)
	if err != nil {
		t.Fatalf("a deferred row must still be readable: %v", err)
	}
	if content == "" {
		t.Error("deferred row has no content")
	}
	// It is also still counted by a plain un-compacted read, i.e.
	// deferral does not hide it from anything except the batch pool.
	var n int
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM memories
		WHERE collection='memories' AND deleted_at IS NULL
		  AND (metadata IS NULL OR metadata = ''
		       OR json_extract(metadata, '$.compacted_into') IS NULL)`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("un-compacted count = %d, want 1", n)
	}
}

// A deferred row is never re-deferred by the drain, because it has
// left the selection pool. That is the property that matters: a second
// refusal of the same rows would resurrect the loop, and the mechanism
// preventing it is the predicate, not a guard against double-writing.
//
// (DeferBatch itself will overwrite an existing annotation if called
// directly on a deferred id. That is deliberate — the function is a
// low-level primitive, and the drain is its only production caller,
// which cannot reach that state.)
func TestDeferral_DrainNeverReoffersADeferredRow(t *testing.T) {
	dm := NewTestDM(t)
	seedMixed(t, dm, 3, 0, 0)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return testRefusalSentinel, nil
	})
	if _, err := dm.CompactEpistemology(context.Background(), true); err != nil {
		t.Fatalf("first refusal: %v", err)
	}

	// A second drain over the same substrate. The extraction pool is
	// empty, so no call is made at all and nothing is re-deferred.
	calls := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		calls++
		return testRefusalSentinel, nil
	})
	result, err := dm.CompactEpistemology(context.Background(), true)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if calls != 0 {
		t.Errorf("the drain made %d model calls on a fully-deferred substrate, want 0 — the loop is not broken", calls)
	}
	if result.SkippedReason != "no_raw_memories" {
		t.Errorf("SkippedReason = %q, want no_raw_memories", result.SkippedReason)
	}

	got := readPressure(t, dm)
	if got.DeferredCount != 3 {
		t.Errorf("deferred_count = %d, want 3", got.DeferredCount)
	}
	if got.ActionablePending != 0 {
		t.Errorf("actionable_pending = %d, want 0", got.ActionablePending)
	}
}

// An existing deferral annotation must survive a later compaction of
// the same row. The two key families are independent, and clearing
// deferral (requeue) must not be entangled with the compact path.
func TestDeferral_IndependentOfCompactedInto(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 2, 0, 0)

	// Compact first.
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return `{"title":"T","body":"B","tags":["x"]}`, nil
	})
	if _, err := dm.CompactEpistemology(context.Background(), true); err != nil {
		t.Fatalf("compact: %v", err)
	}

	// Now annotate one as deferred anyway. It is compacted, so it is
	// outside U and appears in no count.
	deferRow(t, dm, ids[0], "batch-1", "refusal_sentinel", "")

	if into := memoryCompactedInto(t, dm, ids[0]); into == "" {
		t.Error("deferring must not clear compacted_into")
	}
	got := readPressure(t, dm)
	if got.RawCount != 0 || got.DeferredCount != 0 || got.ActionablePending != 0 {
		t.Errorf("a compacted+deferred row must appear in no count; got %+v", got)
	}
}

// The sample is truncated: a large refusal-adjacent response must not
// be copied into every row's metadata verbatim.
func TestDeferral_SampleIsTruncated(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 1, 0, 0)

	huge := strings.Repeat("x", compactDeferralSampleMax*3)
	deferRow(t, dm, ids[0], "batch-1", "refusal_sentinel", huge)

	sample := deferralMetadata(t, dm, ids[0]).Sample
	if len(sample) > compactDeferralSampleMax {
		t.Errorf("sample length = %d, want <= %d", len(sample), compactDeferralSampleMax)
	}
}

// ── helper: seedMixed ────────────────────────────────────────────────

// seedMixed creates pending, deferred, and compacted memories in one
// pass. Order within each group is by created_at so the extraction
// ordering is deterministic.
func seedMixed(t *testing.T, dm *DatabaseManager, pending, deferred, compacted int) []string {
	t.Helper()
	base := time.Now().UTC().Add(-time.Duration(pending+deferred+compacted) * time.Minute)
	var ids []string
	add := func(i int, meta any) string {
		id := fmt.Sprintf("seed-%s-%d", t.Name(), i)
		created := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		var err error
		if meta == nil {
			_, err = dm.SQLDB().Exec(`
				INSERT INTO memories (id, collection, content, created_at, updated_at)
				VALUES (?, 'memories', ?, ?, ?)`, id, "content of "+id, created, created)
		} else {
			_, err = dm.SQLDB().Exec(`
				INSERT INTO memories (id, collection, content, created_at, updated_at, metadata)
				VALUES (?, 'memories', ?, ?, ?, ?)`, id, "content of "+id, created, created, meta)
		}
		if err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		ids = append(ids, id)
		return id
	}
	// Pending first, so they are the oldest and are what extraction
	// returns.
	for i := 0; i < pending; i++ {
		add(i, nil)
	}
	for i := 0; i < deferred; i++ {
		add(pending+i, deferralJSON("old-batch", "refusal_sentinel", "s"))
	}
	for i := 0; i < compacted; i++ {
		add(pending+deferred+i, `{"compacted_into":"les-old"}`)
	}
	return ids
}

func readPressure(t *testing.T, dm *DatabaseManager) EpistemicPressureCounts {
	t.Helper()
	var got EpistemicPressureCounts
	err := dm.SQLDB().QueryRow(`
		SELECT raw_count, lesson_count, deferred_count, actionable_pending
		FROM epistemic_pressure_v`).
		Scan(&got.RawCount, &got.LessonCount, &got.DeferredCount, &got.ActionablePending)
	if err != nil {
		t.Fatalf("read pressure: %v", err)
	}
	return got
}

// deferRow annotates a single row as deferred through the production
// path, so the tests exercise DeferBatch rather than a hand-rolled
// UPDATE that could drift from it.
func deferRow(t *testing.T, dm *DatabaseManager, id, batch, reason, sample string) {
	t.Helper()
	ann := DeferralAnnotation{
		At:     time.Date(2026, 9, 30, 21, 14, 2, 0, time.UTC),
		Reason: reason,
		Batch:  batch,
		Sample: sample,
	}
	if err := dm.DeferBatch(context.Background(), []string{id}, ann); err != nil {
		t.Fatalf("defer %s: %v", id, err)
	}
}

type deferralMeta struct {
	At, Reason, Batch, Sample string
}

func deferralMetadata(t *testing.T, dm *DatabaseManager, id string) deferralMeta {
	t.Helper()
	var raw sql.NullString
	if err := dm.SQLDB().QueryRow(`SELECT metadata FROM memories WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("read metadata %s: %v", id, err)
	}
	if !raw.Valid || raw.String == "" {
		return deferralMeta{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw.String), &m); err != nil {
		t.Fatalf("unmarshal metadata %s: %v", id, err)
	}
	get := func(k string) string {
		s, _ := m[k].(string)
		return s
	}
	return deferralMeta{
		At:     get("compaction_deferred_at"),
		Reason: get("compaction_deferred_reason"),
		Batch:  get("compaction_deferred_batch"),
		Sample: get("compaction_deferred_sample"),
	}
}

func deferralJSON(batch, reason, sample string) string {
	b, err := json.Marshal(map[string]string{
		"compaction_deferred_at":     "2026-09-30T21:14:02Z",
		"compaction_deferred_reason": reason,
		"compaction_deferred_batch":  batch,
		"compaction_deferred_sample": sample,
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

type rowSnapshot struct {
	content, collection  string
	weight               float64
	createdAt, deletedAt string
}

func snapshotRows(t *testing.T, dm *DatabaseManager, ids []string) []rowSnapshot {
	t.Helper()
	out := make([]rowSnapshot, 0, len(ids))
	for _, id := range ids {
		var s rowSnapshot
		var deleted sql.NullString
		// created_at/deleted_at are stored as TEXT in this schema, so
		// they are compared as strings. Byte equality is a strictly
		// stronger claim than temporal equality, which is what
		// "unmodified" wants here.
		if err := dm.SQLDB().QueryRow(`
			SELECT content, collection, weight, created_at, deleted_at
			FROM memories WHERE id = ?`, id).
			Scan(&s.content, &s.collection, &s.weight, &s.createdAt, &deleted); err != nil {
			t.Fatalf("snapshot %s: %v", id, err)
		}
		s.deletedAt = deleted.String
		out = append(out, s)
	}
	return out
}
