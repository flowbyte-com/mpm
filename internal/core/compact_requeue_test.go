// compact_requeue_test.go — Phase 6 of the compact-refusal lifecycle.
//
// Requeue returns a deferred row to the selectable pool. Its semantics
// are specified as R1–R5 in
// docs/designs/2026-09-30-compact-refusal-lifecycle.md §3.3, and each
// rule below maps to one of them.
//
// Requeue is the one operation here that a human performs to override a
// machine decision, which is why it is the one that has to be exactly
// invertible: a requeued row must be byte-identical to a row that had
// never been deferred, or the audit trail lies about what the operator
// actually did.

package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ── R1: explicit operator action only ────────────────────────────────

// A bare compact call must not requeue. The drain's refusal path must
// not be able to reach requeue: a refusal was a considered judgement,
// and silently re-offering the same rows on a timer would reintroduce
// the loop this whole design removes, just slower.
func TestRequeue_IsNeverImplicit(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 0, 4, 0)

	// A normal drain over a fully-deferred substrate.
	if _, err := dm.CompactEpistemologyDrain(context.Background(), true, 0); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := readPressure(t, dm).DeferredCount; got != 4 {
		t.Fatalf("deferred_count = %d, want 4", got)
	}

	// Deferral is a no-op on requeued rows unless requeue was called
	// explicitly — and nothing above called it.
	if got := readPressure(t, dm).ActionablePending; got != 0 {
		t.Errorf("actionable_pending = %d, want 0 — a drain must never requeue", got)
	}
	for _, id := range ids {
		if m := deferralMetadata(t, dm, id); m.At == "" {
			t.Errorf("row %s lost its deferral without an explicit requeue", id)
		}
	}
}

// The single-batch primitive behaves the same way.
func TestRequeue_NotReachedByCompactEpistemology(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 0, 2, 0)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return `{"title":"T","body":"B","tags":["x"]}`, nil
	})
	if _, err := dm.CompactEpistemology(context.Background(), true); err != nil {
		t.Fatalf("compact: %v", err)
	}
	for _, id := range ids {
		if m := deferralMetadata(t, dm, id); m.At == "" {
			t.Errorf("row %s was requeued by a plain compact call", id)
		}
	}
}

// ── R2: bounded target ───────────────────────────────────────────────

// A single call requeues at most limit rows, oldest first, and reports
// which ones. There is no unbounded mode.
func TestRequeue_IsBoundedAndOrdered(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 0, 10, 0)

	res, err := dm.RequeueDeferred(context.Background(), 4)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if res.Requeued != 4 {
		t.Errorf("Requeued = %d, want 4", res.Requeued)
	}
	if len(res.RowIDs) != 4 {
		t.Fatalf("RowIDs = %v, want 4 entries", res.RowIDs)
	}
	// Oldest first — the same ordering the drain uses, so the
	// operator requeues the oldest deferrals and the behaviour is
	// predictable.
	want := ids[:4]
	for i, id := range res.RowIDs {
		if id != want[i] {
			t.Errorf("RowIDs[%d] = %s, want %s — requeue must be oldest-first", i, id, want[i])
		}
	}
	// The rest stay deferred.
	if got := readPressure(t, dm).DeferredCount; got != 6 {
		t.Errorf("deferred_count = %d, want 6", got)
	}
}

// A limit larger than the backlog requeues what exists and no more.
func TestRequeue_LimitExceedingBacklogIsClamped(t *testing.T) {
	dm := NewTestDM(t)
	seedMixed(t, dm, 0, 3, 0)

	res, err := dm.RequeueDeferred(context.Background(), 1000)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if res.Requeued != 3 {
		t.Errorf("Requeued = %d, want 3", res.Requeued)
	}
	if got := readPressure(t, dm).DeferredCount; got != 0 {
		t.Errorf("deferred_count = %d, want 0", got)
	}
}

// A non-positive limit means "use the default", which is
// compactBatchSize — one batch's worth, never "all". There is no value
// of this argument that means unbounded, which is the property R2
// actually asks for.
func TestRequeue_NonPositiveLimitMeansTheBatchDefault(t *testing.T) {
	for _, limit := range []int{0, -1, -100} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			// A fresh backlog per case: these limits all mean the
			// same thing, and running them against one substrate
			// would measure the cumulative drain rather than the
			// default.
			dm := NewTestDM(t)
			seedMixed(t, dm, 0, compactBatchSize+20, 0)

			res, err := dm.RequeueDeferred(context.Background(), limit)
			if err != nil {
				t.Fatalf("requeue: %v", err)
			}
			if res.Requeued != compactBatchSize {
				t.Errorf("Requeued = %d, want the %d default", res.Requeued, compactBatchSize)
			}
			if res.Limit != compactBatchSize {
				t.Errorf("Limit = %d, want %d", res.Limit, compactBatchSize)
			}
			// The backlog is only partly undone, which is the
			// point: the default is a bound, not a flush.
			if got := readPressure(t, dm).DeferredCount; got != 20 {
				t.Errorf("deferred_count = %d, want 20", got)
			}
		})
	}
}

// A limit above the hard cap is clamped to it.
func TestRequeue_LimitIsClampedToHardCap(t *testing.T) {
	dm := NewTestDM(t)
	seedMixed(t, dm, 0, 5, 0)

	// Well past any sensible requeue size. The call must succeed and
	// requeue only the rows that exist.
	res, err := dm.RequeueDeferred(context.Background(), 1_000_000)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if res.Requeued != 5 {
		t.Errorf("Requeued = %d, want 5", res.Requeued)
	}
}

// ── R3: clears only deferral metadata ────────────────────────────────

// A requeued row is byte-identical to a row that had never been
// deferred, in every column except the four keys requeue removed.
func TestRequeue_RowIsByteIdenticalApartFromDeferralKeys(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 1, 0, 0)
	target := ids[0]

	before := snapshotRows(t, dm, []string{target})[0]
	deferRow(t, dm, target, "batch-1", "refusal_sentinel", "some sample")
	// A pre-existing, unrelated metadata key must survive the round
	// trip — requeue removes what it created, not everything.
	_, err := dm.SQLDB().Exec(
		`UPDATE memories SET metadata = json_set(metadata, '$.unrelated', 'keepme') WHERE id = ?`, target)
	if err != nil {
		t.Fatalf("add unrelated key: %v", err)
	}

	if _, err := dm.RequeueDeferred(context.Background(), 10); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	after := snapshotRows(t, dm, []string{target})[0]
	if before.content != after.content {
		t.Errorf("content changed: %q -> %q", before.content, after.content)
	}
	if before.collection != after.collection {
		t.Error("collection changed")
	}
	if before.weight != after.weight {
		t.Error("weight changed")
	}
	if before.createdAt != after.createdAt {
		t.Errorf("created_at changed: %q -> %q", before.createdAt, after.createdAt)
	}
	if before.deletedAt != after.deletedAt {
		t.Error("deleted_at changed")
	}

	m := deferralMetadata(t, dm, target)
	if m.At != "" || m.Reason != "" || m.Batch != "" || m.Sample != "" {
		t.Errorf("deferral keys survived: %+v", m)
	}
	var unrelated string
	if err := dm.SQLDB().QueryRow(
		`SELECT json_extract(metadata, '$.unrelated') FROM memories WHERE id = ?`, target).
		Scan(&unrelated); err != nil {
		t.Fatalf("read unrelated key: %v", err)
	}
	if unrelated != "keepme" {
		t.Errorf("unrelated metadata = %q, want it preserved", unrelated)
	}
}

// All four keys are gone — not three.
func TestRequeue_ClearsAllFourKeys(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 1, 0, 0)

	deferRow(t, dm, ids[0], "cdb-1", "refusal_sentinel", "a truncated sample")
	if m := deferralMetadata(t, dm, ids[0]); m.Sample == "" {
		t.Fatal("test setup: sample key was not written")
	}

	if _, err := dm.RequeueDeferred(context.Background(), 10); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	var raw string
	if err := dm.SQLDB().QueryRow(`SELECT metadata FROM memories WHERE id = ?`, ids[0]).Scan(&raw); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	for _, key := range []string{metaDeferralAt, metaDeferralReason, metaDeferralBatch, metaDeferralSample} {
		if containsKey(raw, key) {
			t.Errorf("metadata still contains %q: %s", key, raw)
		}
	}
}

// ── R4: never clears compacted_into ──────────────────────────────────

// The asymmetry that matters most. A row carrying BOTH keys was
// compacted at some earlier point and later annotated deferred;
// clearing compacted_into would make it eligible for re-synthesis of
// content that already has a lesson, duplicating a lesson and silently
// undoing real work.
func TestRequeue_NeverClearsCompactedInto(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 1, 0, 0)
	target := ids[0]

	// Compact it for real.
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return `{"title":"Real lesson","body":"B","tags":["x"]}`, nil
	})
	if _, err := dm.CompactEpistemology(context.Background(), true); err != nil {
		t.Fatalf("compact: %v", err)
	}
	lessonID := memoryCompactedInto(t, dm, target)
	if lessonID == "" {
		t.Fatal("test setup: row was not compacted")
	}

	// Now also annotate it deferred — the both-keys state.
	deferRow(t, dm, target, "batch-1", "refusal_sentinel", "")

	// Requeue.
	if _, err := dm.RequeueDeferred(context.Background(), 10); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	if got := memoryCompactedInto(t, dm, target); got != lessonID {
		t.Errorf("compacted_into = %q, want %q — requeue must never clear it", got, lessonID)
	}
	if m := deferralMetadata(t, dm, target); m.At != "" {
		t.Errorf("deferral keys survived requeue: %+v", m)
	}
	// And the row is still compacted: requeue did not make it
	// eligible again.
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE json_extract(metadata, '$.compacted_into') IS NOT NULL`).
		Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("compacted rows = %d, want 1", n)
	}
	// The stronger claim, and the one that matters: the row is not
	// merely annotated correctly, it is NOT offered to the model again.
	// If it were, a second drain would synthesize a duplicate lesson
	// from content that already has one.
	selected, _, err := dm.extractRawBatch(context.Background(), 50)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(selected) != 0 {
		t.Errorf("extracted %v after requeue, want nothing — the row is compacted and must stay compacted", selected)
	}
}

// ── R5: auditable ────────────────────────────────────────────────────

// A requeue is the one operation where a human overrides the model's
// judgement, so it must leave a record naming what was returned to the
// pool. AuditInfo, not an anomaly level: it is a deliberate state
// mutation, and it must never trigger cluster detection or wake an
// agent as if something had gone wrong.
func TestRequeue_WritesAnAuditRow(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 0, 3, 0)

	res, err := dm.RequeueDeferred(context.Background(), 10)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if res.AuditID == "" {
		t.Fatal("AuditID is empty — the requeue left no forensic trail")
	}

	var (
		level, component, message, context string
	)
	if err := dm.SQLDB().QueryRow(`
		SELECT level, component, message, context
		FROM system_audit_log WHERE id = ?`, res.AuditID).
		Scan(&level, &component, &message, &context); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if level != "info" {
		t.Errorf("level = %q, want info — a deliberate requeue is not an anomaly", level)
	}
	if component != "compact" {
		t.Errorf("component = %q, want compact", component)
	}
	if !strings.Contains(message, "3") {
		t.Errorf("message = %q, want it to state the count", message)
	}
	// The context names the exact rows, so the operator's action is
	// verifiable afterwards rather than inferred from a count.
	for _, id := range ids {
		if !strings.Contains(context, id) {
			t.Errorf("audit context does not name %s: %s", id, context)
		}
	}
}

// A no-op requeue — nothing deferred — writes no audit row. The trail
// records decisions that changed state, not calls that did nothing.
func TestRequeue_NoAuditRowWhenNothingWasDeferred(t *testing.T) {
	dm := NewTestDM(t)

	if _, err := dm.RequeueDeferred(context.Background(), 10); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE component = 'compact'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("audit rows = %d, want 0", n)
	}
}

// ── the ListDeferred guard ────────────────────────────────────────────

// Regression. ListDeferred's json_extract conjunct needs the same
// null/empty guard as the view's, and a deferral table is precisely
// where empty-metadata rows accumulate: every un-compacted memory that
// predates this change has NULL or ” metadata.
//
// Without the guard, json_extract(”) raises "malformed JSON" and the
// operator's entire inspection view — the thing R5 tells them to use
// before deciding whether to requeue — fails on any realistic dataset.
func TestListDeferred_SurvivesEmptyMetadataRows(t *testing.T) {
	dm := NewTestDM(t)
	seedMixed(t, dm, 1, 1, 0)

	now := "2026-09-30T00:00:00Z"
	for id, meta := range map[string]any{
		"null-meta":  nil,
		"empty-meta": "",
	} {
		var err error
		if meta == nil {
			_, err = dm.SQLDB().Exec(`
				INSERT INTO memories (id, collection, content, created_at, updated_at)
				VALUES (?, 'memories', 'c', ?, ?)`, id, now, now)
		} else {
			_, err = dm.SQLDB().Exec(`
				INSERT INTO memories (id, collection, content, created_at, updated_at, metadata)
				VALUES (?, 'memories', 'c', ?, ?, ?)`, id, now, now, meta)
		}
		if err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	rows, err := dm.ListDeferred(context.Background(), 50)
	if err != nil {
		t.Fatalf("ListDeferred errored with empty-metadata rows present: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("listed %d rows, want 1 — only the deferred one", len(rows))
	}
}

// The same guard protects the requeue target selection, for the same
// reason and with the same failure mode.
func TestRequeue_SelectsCleanlyAlongsideEmptyMetadataRows(t *testing.T) {
	dm := NewTestDM(t)
	seedMixed(t, dm, 0, 2, 0)

	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, metadata)
		VALUES ('empty-meta', 'memories', 'c', '2026-09-30T00:00:00Z', '2026-09-30T00:00:00Z', '')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := dm.RequeueDeferred(context.Background(), 10)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if res.Requeued != 2 {
		t.Errorf("Requeued = %d, want 2", res.Requeued)
	}
	// And the empty-metadata row is still counted correctly. It was
	// always actionable, and requeueing the other two does not change
	// that: all three are actionable, none deferred.
	got := readPressure(t, dm)
	if got.ActionablePending != 3 || got.DeferredCount != 0 {
		t.Errorf("pressure = %+v, want actionable 3 / deferred 0", got)
	}
}

// ── the round trip ───────────────────────────────────────────────────

// Requeue returns the row to the pool at its original created_at
// position, so it is selected again in its original order.
func TestRequeue_RestoresOriginalSelectionOrder(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 5, 0, 0)

	// Defer the OLDEST row, then requeue it.
	deferRow(t, dm, ids[0], "batch-1", "refusal_sentinel", "")

	selected, _, err := dm.extractRawBatch(context.Background(), 50)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, id := range selected {
		if id == ids[0] {
			t.Fatal("a deferred row was still selected")
		}
	}

	if _, err := dm.RequeueDeferred(context.Background(), 10); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	selected, _, err = dm.extractRawBatch(context.Background(), 50)
	if err != nil {
		t.Fatalf("extract after requeue: %v", err)
	}
	if len(selected) != 5 {
		t.Fatalf("selected %d rows, want 5", len(selected))
	}
	if selected[0] != ids[0] {
		t.Errorf("first selected = %s, want %s — the row must return to its original created_at position", selected[0], ids[0])
	}
}

// Requeueing a row that was never deferred is a no-op, not an error.
func TestRequeue_NoOpOnNonDeferredRows(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 3, 0, 0)

	res, err := dm.RequeueDeferred(context.Background(), 10)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if res.Requeued != 0 {
		t.Errorf("Requeued = %d, want 0", res.Requeued)
	}
	if len(res.RowIDs) != 0 {
		t.Errorf("RowIDs = %v, want empty", res.RowIDs)
	}
	// The rows are untouched and still selectable.
	selected, _, err := dm.extractRawBatch(context.Background(), 50)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(selected) != 3 {
		t.Errorf("selected %d rows, want 3", len(selected))
	}
	_ = ids
}

// Requeue does NOT itself offer the rows to the model. It returns them
// to the pool; the next drain decides what to do with them, and spends
// its own budget doing so.
func TestRequeue_DoesNotScheduleASynthesis(t *testing.T) {
	dm := NewTestDM(t)
	seedMixed(t, dm, 0, 5, 0)

	calls := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		calls++
		return `{"title":"T","body":"B","tags":["x"]}`, nil
	})

	if _, err := dm.RequeueDeferred(context.Background(), 10); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if calls != 0 {
		t.Errorf("requeue made %d model calls, want 0 — it returns rows to the pool, it does not synthesize", calls)
	}
}

// A re-deferral after a requeue gets a DIFFERENT batch id, so repeated
// requeue-then-refuse cycles are traceable.
func TestRequeue_RedeferralGetsANewBatchID(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedMixed(t, dm, 1, 0, 0)

	deferRow(t, dm, ids[0], "batch-1", "refusal_sentinel", "")
	first := deferralMetadata(t, dm, ids[0]).Batch

	if _, err := dm.RequeueDeferred(context.Background(), 10); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return testRefusalSentinel, nil
	})
	if _, err := dm.CompactEpistemology(context.Background(), true); err != nil {
		t.Fatalf("recompact: %v", err)
	}

	second := deferralMetadata(t, dm, ids[0]).Batch
	if second == "" {
		t.Fatal("row was not re-deferred")
	}
	if second == first {
		t.Errorf("batch id reused across a requeue cycle: %q — repeated cycles must be traceable", second)
	}
}

func containsKey(metadataJSON, key string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(metadataJSON), &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
