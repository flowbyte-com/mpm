// compact_deferred_readable_test.go — the readability half of the
// compact-refusal lifecycle.
//
// compact_requeue_test.go proves what a deferral does: the rows leave
// the selectable pool and only an explicit requeue brings them back.
// That is the correct behaviour for the COMPACTION selection query, and
// it is silent about every other read path.
//
// The risk this file exists to close: "the rows left the pool" is
// implemented as a predicate on one query (extractRawBatch uses
// memoryIsActionable). Nothing about that predicate touches FTS5, so
// the correct expectation is that a deferred memory remains fully
// readable — but "expected" is not "verified", and a future change that
// widened the deferral filter to a shared WHERE fragment, or added an
// FTS trigger on metadata, would silently turn "held back from
// compaction" into "invisible to the agent", which is a much more
// serious failure than the refusal loop the design set out to remove.
//
// Second job: pin the GRANULARITY at which deferral state is visible.
// A deferral writes four keys into the row's metadata document. Three
// distinct agent-facing surfaces read that column with three different
// projections, and the difference matters for what documentation is
// allowed to claim:
//
//	mpm_memory show        → dm.GetMemory, returned verbatim → metadata present
//	mpm_resolve mpm://…    → resolveMetadataFor → metadata present
//	mpm_memory query       → hybridResultsToMaps → metadata ABSENT
//
// So "content retrievable" is unconditionally true; "per-row deferral
// metadata visible" is true on the by-id surfaces and false on search.
// Search surfaces the deferral only as an aggregate, through the
// epistemic_pressure deferred_count / actionable_pending pair.
//
// Everything here runs on NewTestDM — an in-memory database unique per
// test. Nothing touches ~/.mpm/src/db/mpm.db.

package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// deferredReadableToken is the distinctive term seeded into the
// retrievable memory. Hyphenated so BuildFTS5Query tokenizes it into
// three independent prefixes — a match proves the row is genuinely in
// the FTS5 index rather than matched by a LIKE fallback on the whole
// string.
const deferredReadableToken = "DEFERRED-READABLE-7731"

// seedDeferrableMemory saves one memory through the production write
// path — SaveMemoryWithContext, not a raw INSERT. That distinction is
// load-bearing: a raw INSERT into `memories` never populates the
// memories_fts contentless-delete triggers, so the row is invisible to
// HybridSearchMemories and the test would "prove" retrievability of a
// row nothing could retrieve.
func seedDeferrableMemory(t *testing.T, dm *DatabaseManager, content string) string {
	t.Helper()
	_, mem, err := dm.SaveMemoryWithContext(
		content,
		"memories",
		[]string{"deferred-readable"},
		0, "",
		ActiveContext{},
	)
	if err != nil {
		t.Fatalf("SaveMemoryWithContext: %v", err)
	}
	if mem == nil || mem.ID == "" {
		t.Fatal("SaveMemoryWithContext returned no memory id — cannot defer a row we cannot name")
	}
	return mem.ID
}

// deferThroughRefusalPath runs the REAL refusal path end to end: the
// mock synthesizer returns the refusal sentinel, and
// CompactEpistemologyDrain defers the batch through deferRawBatch →
// DeferBatch. Hand-rolling the four metadata keys would test the test's
// own SQL rather than production behaviour, and the two can drift.
func deferThroughRefusalPath(t *testing.T, dm *DatabaseManager) {
	t.Helper()
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return testRefusalSentinel, nil
	})
	res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("CompactEpistemologyDrain: %v", err)
	}
	if res.RawDeferred == 0 {
		t.Fatalf("raw_deferred = 0 — the refusal path deferred nothing, so this test would prove nothing: %+v", res)
	}
}

// ── (a) content stays retrievable ────────────────────────────────────

// The load-bearing claim. A deferred memory is held back from
// COMPACTION selection, not from retrieval. Both the FTS5 search path
// an agent actually uses and the direct by-id read must still return
// the row with its content byte-intact.
//
// Deferral is compared before/after on the same row so a pass cannot be
// produced by a row that was never findable in the first place.
func TestDeferred_ContentStaysRetrievableThroughSearch(t *testing.T) {
	dm := NewTestDM(t)
	content := "the compaction drain refuses when " + deferredReadableToken + " appears in raw batch content"
	id := seedDeferrableMemory(t, dm, content)

	// Baseline: findable before deferral. If this fails, the seed did
	// not reach the FTS index and every later assertion is vacuous.
	before, err := dm.HybridSearchMemories(deferredReadableToken, "memories", 10, "local")
	if err != nil {
		t.Fatalf("HybridSearchMemories (before): %v", err)
	}
	if got := countHits(before, id); got != 1 {
		t.Fatalf("baseline: matched %d rows for %s, want 1 — the seed is not in the FTS index: %s",
			got, id, renderHits(before))
	}

	deferThroughRefusalPath(t, dm)

	// The row must have left the compaction pool, or "still
	// retrievable" is trivially true for the wrong reason.
	selected, _, err := dm.extractRawBatch(context.Background(), 50)
	if err != nil {
		t.Fatalf("extractRawBatch: %v", err)
	}
	for _, sel := range selected {
		if sel == id {
			t.Fatalf("row %s is still selectable by the drain — deferral did not take effect, so this test proves nothing", id)
		}
	}

	// The claim.
	after, err := dm.HybridSearchMemories(deferredReadableToken, "memories", 10, "local")
	if err != nil {
		t.Fatalf("HybridSearchMemories (after): %v", err)
	}
	if got := countHits(after, id); got != 1 {
		t.Errorf("after deferral: matched %d rows for %s, want 1 — deferral changed retrievability: %s",
			got, id, renderHits(after))
	}
	for _, hit := range after {
		if hit["id"] != id {
			continue
		}
		got, _ := hit["content"].(string)
		// HybridSearch may prepend a provenance preamble, so the
		// containment check is the honest form — a strict equality
		// would fail on a correctly-functioning preamble and teach
		// the next reader to weaken the test for the wrong reason.
		if !strings.Contains(got, content) {
			t.Errorf("deferred row content = %q, want it to contain the stored body %q", got, content)
		}
		t.Logf("matched 1 row via HybridSearchMemories; content intact (%d bytes)", len(got))
	}
}

// The direct read path, which is what `mpm_memory show` and
// `mpm_resolve mpm://memory/<id>` both bottom out in.
func TestDeferred_ContentStaysRetrievableThroughGet(t *testing.T) {
	dm := NewTestDM(t)
	content := "direct read of a deferred memory containing " + deferredReadableToken
	id := seedDeferrableMemory(t, dm, content)

	deferThroughRefusalPath(t, dm)

	mem, err := dm.GetMemory(id)
	if err != nil {
		t.Fatalf("GetMemory on a deferred row: %v — GetMemory must not filter on deferral", err)
	}
	got, _ := mem["content"].(string)
	if got != content {
		t.Errorf("GetMemory content = %q, want %q — content must be byte-identical after deferral", got, content)
	}
	if c, _ := mem["collection"].(string); c != "memories" {
		t.Errorf("collection = %q, want memories", c)
	}
	t.Logf("GetMemory returned the deferred row intact: collection=%v content_bytes=%d", mem["collection"], len(got))
}

// ── (b) get-by-id on a deferred row ──────────────────────────────────

// GetMemory's WHERE clause is `id = ? AND deleted_at IS NULL` plus the
// expiry clause. Deferral is not a tombstone, so a deferred row must
// still be individually addressable — the operator's per-row
// inspection path depends on it.
//
// Note the contrast with SoftDeleteMemory: that sets deleted_at, and
// GetMemory genuinely does hide deleted rows. If deferral ever started
// setting deleted_at, this test fails, and that is the point — the two
// states mean different things.
func TestDeferred_GetByIDReturnsTheDeferredRow(t *testing.T) {
	dm := NewTestDM(t)
	id := seedDeferrableMemory(t, dm, "get-by-id on a deferred row "+deferredReadableToken)

	deferThroughRefusalPath(t, dm)

	// Still addressable…
	mem, err := dm.GetMemory(id)
	if err != nil {
		t.Fatalf("GetMemory(%s): %v", id, err)
	}
	if mem["id"] != id {
		t.Errorf("GetMemory returned id %v, want %s", mem["id"], id)
	}

	// …and not a tombstone: deleted_at is untouched, so a deferred row
	// still counts as live memory everywhere `deleted_at IS NULL` is
	// the only filter.
	var deleted sql.NullString
	if err := dm.SQLDB().QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id).Scan(&deleted); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}
	if deleted.Valid && deleted.String != "" {
		t.Errorf("deleted_at = %q on a deferred row — deferral must not tombstone the row", deleted.String)
	}

	// The four keys are really on this row — otherwise "get-by-id
	// works on a deferred row" is vacuously true of a row that was
	// never deferred.
	m := deferralMetadata(t, dm, id)
	if m.At == "" || m.Batch == "" || m.Reason == "" {
		t.Fatalf("row %s is not actually annotated deferred: %+v", id, m)
	}
	t.Logf("get-by-id works on a deferred row; batch=%s reason=%s deferred_at=%s", m.Batch, m.Reason, m.At)
}

// ── (c) deferred counts are visible and self-consistent ──────────────

// Deferral is visible at aggregate granularity on every surface that
// reads the pressure view, and the three columns are not independent:
// raw_count = actionable_pending + deferred_count by construction. A
// reader seeing raw 5 / actionable 0 must be able to conclude "there
// is a backlog the model declined", not "there is nothing to do".
func TestDeferred_CountsAreVisibleAndSelfConsistent(t *testing.T) {
	dm := NewTestDM(t)
	const n = 6
	for i := 0; i < n; i++ {
		seedDeferrableMemory(t, dm, "pressure seed row "+deferredReadableToken+deferItoa(i)+" with filler content to keep rows distinct")
	}

	if got := readPressure(t, dm); got.DeferredCount != 0 || got.ActionablePending != n {
		t.Fatalf("pre-deferral pressure = %+v, want deferred 0 / actionable %d", got, n)
	}

	// A refusing model refuses EVERY batch it is offered, so all six
	// rows land in one deferral group and none survives as
	// actionable. That is the honest shape of a fully-refused
	// substrate, and it is the state that matters: it is exactly the
	// state a naive reader would misread as "nothing to do" if only
	// raw_count were surfaced.
	deferThroughRefusalPath(t, dm)

	counts, err := dm.PressureCounts(context.Background())
	if err != nil {
		t.Fatalf("PressureCounts: %v", err)
	}
	if counts.DeferredCount != n {
		t.Errorf("deferred_count = %d, want %d — every refused row must be counted as deferred", counts.DeferredCount, n)
	}
	if counts.ActionablePending != 0 {
		t.Errorf("actionable_pending = %d, want 0 — a refused row is not offerable", counts.ActionablePending)
	}
	if counts.RawCount != counts.ActionablePending+counts.DeferredCount {
		t.Errorf("raw_count (%d) != actionable_pending (%d) + deferred_count (%d) — the three columns are not a partition",
			counts.RawCount, counts.ActionablePending, counts.DeferredCount)
	}
	if counts.RawCount != n {
		t.Errorf("raw_count = %d, want %d — raw_count deliberately still counts deferred rows", counts.RawCount, n)
	}

	// Now return one row to the pool via the operator action, and the
	// split must move by exactly one. This is the transition the
	// columns exist to make legible: raw stays 6, deferred drops to 5,
	// actionable rises to 1.
	res, err := dm.RequeueDeferred(context.Background(), 1)
	if err != nil {
		t.Fatalf("RequeueDeferred: %v", err)
	}
	if res.Requeued != 1 {
		t.Fatalf("Requeued = %d, want 1", res.Requeued)
	}

	after, err := dm.PressureCounts(context.Background())
	if err != nil {
		t.Fatalf("PressureCounts (after requeue): %v", err)
	}
	if after.RawCount != counts.RawCount {
		t.Errorf("raw_count changed across a requeue: %d -> %d — requeue moves a row between partitions, it does not remove it",
			counts.RawCount, after.RawCount)
	}
	if after.DeferredCount != n-1 || after.ActionablePending != 1 {
		t.Errorf("after requeue: deferred_count = %d / actionable_pending = %d, want %d / 1",
			after.DeferredCount, after.ActionablePending, n-1)
	}

	// The two-column accessor must agree with the four-column one —
	// they read the same view, and a divergence would mean one of the
	// two surfaces is describing a different population.
	deferred, actionable, err := dm.DeferralCounts(context.Background())
	if err != nil {
		t.Fatalf("DeferralCounts: %v", err)
	}
	if deferred != after.DeferredCount || actionable != after.ActionablePending {
		t.Errorf("DeferralCounts = (%d, %d), PressureCounts = (%d, %d) — the two readers disagree",
			deferred, actionable, after.DeferredCount, after.ActionablePending)
	}

	t.Logf("fully refused: raw_count=%d deferred_count=%d actionable_pending=%d lesson_count=%d",
		counts.RawCount, counts.DeferredCount, counts.ActionablePending, counts.LessonCount)
	t.Logf("after requeue of 1: raw_count=%d deferred_count=%d actionable_pending=%d",
		after.RawCount, after.DeferredCount, after.ActionablePending)
}

// ListDeferred is the operator's "what is deferred and why" read. It
// must see every deferred row, with its annotation, on a substrate
// where the rows are simultaneously invisible to compaction selection.
func TestDeferred_ListDeferredSeesTheDeferredRows(t *testing.T) {
	dm := NewTestDM(t)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, seedDeferrableMemory(t, dm, "list-deferred seed "+deferredReadableToken+deferItoa(i)))
	}
	deferThroughRefusalPath(t, dm)

	rows, err := dm.ListDeferred(context.Background(), 50)
	if err != nil {
		t.Fatalf("ListDeferred: %v", err)
	}
	if len(rows) != len(ids) {
		t.Fatalf("ListDeferred returned %d rows, want %d", len(rows), len(ids))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.ID] = true
		if r.DeferredAt == "" || r.Batch == "" || r.Reason != DeferralReasonRefusal {
			t.Errorf("row %s annotation = %+v, want a full deferral annotation with reason %q",
				r.ID, r, DeferralReasonRefusal)
		}
		if r.Content == "" {
			t.Errorf("row %s listed with empty content", r.ID)
		}
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("ListDeferred omitted deferred row %s", id)
		}
	}
	t.Logf("ListDeferred returned %d rows, all annotated; sample batch=%s", len(rows), rows[0].Batch)
}

// ── (d) metadata granularity — the key discriminator ────────────────

// This is the test that decides what the documentation may claim.
//
// GetMemory returns a map that INCLUDES the raw `metadata` column as a
// JSON string. handleShowMemory (handlers.go:701) decodes it and
// returns the map verbatim, and resolveMetadataFor
// (handlers.go:7033) preserves every key except content — so on both
// by-id surfaces an agent receives the four compaction_deferred_* keys
// as a decoded object.
//
// HybridSearchMemories goes through hybridResultsToMaps
// (memory_tools.go:538), whose map has thirteen fixed keys and NO
// metadata entry. So search does not expose per-row deferral state at
// all — neither summary nor full projection, because the full
// projection reuses the same `items` maps and its
// `mem["metadata"].(string)` decode is therefore dead code on this
// path.
//
// The defensible claim is therefore two-part and narrow: content is
// retrievable everywhere, and per-row deferral metadata is inspectable
// ONLY by id.
func TestDeferred_MetadataIsExposedOnByIDSurfacesButNotSearch(t *testing.T) {
	dm := NewTestDM(t)
	id := seedDeferrableMemory(t, dm, "metadata visibility probe carrying "+deferredReadableToken)
	deferThroughRefusalPath(t, dm)

	// ── by-id surface: metadata present, decoded, deferral keys visible
	mem, err := dm.GetMemory(id)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	rawMeta, ok := mem["metadata"].(string)
	if !ok || rawMeta == "" {
		t.Fatalf("GetMemory has no metadata string (got %T) — the by-id surface cannot expose deferral keys", mem["metadata"])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(rawMeta), &decoded); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	for _, key := range []string{metaDeferralAt, metaDeferralReason, metaDeferralBatch} {
		if _, present := decoded[key]; !present {
			t.Errorf("by-id metadata is missing %q — an agent could not tell a deferred row from a live one", key)
		}
	}
	keys := deferSortedKeys(decoded)
	t.Logf("GetMemory metadata keys (agent-visible via mpm_memory show / mpm_resolve): %v", keys)

	// ── search surface: no metadata key at all
	hits, err := dm.HybridSearchMemories(deferredReadableToken, "memories", 10, "local")
	if err != nil {
		t.Fatalf("HybridSearchMemories: %v", err)
	}
	if countHits(hits, id) != 1 {
		t.Fatalf("deferred row not returned by search; cannot assert the projection shape: %s", renderHits(hits))
	}
	var searchKeys []string
	for _, hit := range hits {
		if hit["id"] == id {
			for k := range hit {
				searchKeys = append(searchKeys, k)
			}
			if _, present := hit["metadata"]; present {
				t.Errorf("HybridSearchMemories returned a metadata key (%v) — search does expose per-row deferral state after all; the docs claim and this test must both change", hit["metadata"])
			}
		}
	}
	sort.Strings(searchKeys)
	t.Logf("HybridSearchMemories row keys (agent-visible via mpm_memory query): %v", searchKeys)
}

// The negative half of (d), stated as its own test so a failure names
// the specific overstatement it forbids: search results must not leak
// the deferral keys even indirectly, and the aggregate must be the only
// place search-side callers can learn about a deferral.
func TestDeferred_SearchResultsCarryNoDeferralMetadata(t *testing.T) {
	dm := NewTestDM(t)
	id := seedDeferrableMemory(t, dm, "search projection probe "+deferredReadableToken)
	deferThroughRefusalPath(t, dm)

	hits, err := dm.HybridSearchMemories(deferredReadableToken, "memories", 10, "local")
	if err != nil {
		t.Fatalf("HybridSearchMemories: %v", err)
	}
	found := false
	for _, hit := range hits {
		if hit["id"] != id {
			continue
		}
		found = true
		for k, v := range hit {
			if strings.Contains(strings.ToLower(k), "defer") {
				t.Errorf("search row exposes a deferral-shaped field %q = %v — per-row deferral state must not ride on search", k, v)
			}
			if k == "metadata" {
				t.Errorf("search row carries raw metadata")
			}
		}
		// Sanity: content is still there. This test is about metadata
		// granularity, not about retrievability, which
		// TestDeferred_ContentStaysRetrievableThroughSearch owns.
		if c, _ := hit["content"].(string); !strings.Contains(c, deferredReadableToken) {
			t.Errorf("search row content = %q, want it to contain %s", c, deferredReadableToken)
		}
	}
	if !found {
		t.Fatalf("deferred row absent from search results")
	}
}

// ── (e) no agent-callable action clears a deferral ───────────────────

// R1 in the design is "explicit operator action only", and the existing
// guard lives in cmd/mpm (TestRequeue_IsNotReachableFromTheMCPAction),
// which reads the live tools.Registry. This is the layer beneath it: a
// repo-wide source scan proving RequeueDeferred has exactly one
// non-test caller outside internal/core, and that it is the CLI.
//
// An allowlist rather than a denylist on purpose. The rule "RequeueDeferred
// must not appear in internal/core/tools" would be satisfied by
// renaming the method, moving it to a new package, or wrapping it in a
// helper with a friendly name. An allowlist breaks on every one of
// those. The cost is that adding a legitimate caller requires editing
// this list — which is the review event the design wants.
//
// The scan covers the whole repo tree, not the nearest module, because
// internal/core/tools is a separate Go module and cmd/mpm-mcp a third.
func TestDeferred_NoAgentCallableActionClearsDeferral(t *testing.T) {
	root := findRepoRoot(t)

	// Every non-test file in the repository that names
	// RequeueDeferred. Anything new here is a new caller of the one
	// operation that undoes a model's refusal.
	allowed := map[string]string{
		"internal/core/compact_requeue.go":  "the method definition itself",
		"internal/core/core.go":             "the CoreDB interface declaration",
		"internal/core/compact_deferral.go": "doc comment naming requeue as the inverse of defer",
		"cmd/mpm/compact_cmds.go":           "the operator CLI (`mpm compact requeue-deferred`)",
	}

	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "bin" || name == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(data), "RequeueDeferred") {
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	sort.Strings(found)

	var unexpected []string
	for _, f := range found {
		if _, ok := allowed[f]; !ok {
			unexpected = append(unexpected, f)
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("RequeueDeferred is named in unexpected non-test files: %v\n"+
			"Allowed: %v\n"+
			"Requeue is an operator action by design. An agent that can call it can undo a model's\n"+
			"refusal on its own judgement, which is the deferral loop the design removes.",
			unexpected, keysOf(allowed))
	}

	// And the allowlist is not vacuous: the CLI must actually be
	// present, or a refactor that moved requeue into core would
	// satisfy the allowlist by deletion.
	cliPresent := false
	for _, f := range found {
		if f == "cmd/mpm/compact_cmds.go" {
			cliPresent = true
		}
	}
	if !cliPresent {
		t.Errorf("cmd/mpm/compact_cmds.go no longer names RequeueDeferred — the CLI requeue path moved. "+
			"Non-test files naming it: %v", found)
	}

	t.Logf("non-test files naming RequeueDeferred: %v", found)
}

// The behavioural half of (e): within the core layer, the paths an
// agent CAN reach — the drain and the wake-context gather — must leave
// a deferral untouched. This is the claim that is easy to get wrong by
// accident later: "requeue is operator-only" is only true if nothing
// on the automatic path calls it.
func TestDeferred_AutomaticPathsNeverClearDeferral(t *testing.T) {
	dm := NewTestDM(t)
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, seedDeferrableMemory(t, dm, "automatic-path probe "+deferredReadableToken+deferItoa(i)))
	}
	deferThroughRefusalPath(t, dm)

	if got := readPressure(t, dm).DeferredCount; got != 4 {
		t.Fatalf("pre-check: deferred_count = %d, want 4", got)
	}

	// A further drain over a fully-deferred substrate.
	if _, err := dm.CompactEpistemologyDrain(context.Background(), true, 0); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	// Wake-context assembly — read on every session start.
	if _, err := dm.GatherWakeContext(); err != nil {
		t.Fatalf("GatherWakeContext: %v", err)
	}

	if got := readPressure(t, dm); got.DeferredCount != 4 || got.ActionablePending != 0 {
		t.Errorf("after drain + wake: deferred_count = %d / actionable_pending = %d, want 4 / 0 — an automatic path cleared a deferral",
			got.DeferredCount, got.ActionablePending)
	}
	for _, id := range ids {
		if m := deferralMetadata(t, dm, id); m.At == "" {
			t.Errorf("row %s lost its deferral to an automatic path", id)
		}
	}
}

// ── helpers ──────────────────────────────────────────────────────────

func countHits(hits []map[string]interface{}, id string) int {
	n := 0
	for _, h := range hits {
		if h["id"] == id {
			n++
		}
	}
	return n
}

func renderHits(hits []map[string]interface{}) string {
	var sb strings.Builder
	for _, h := range hits {
		content, _ := h["content"].(string)
		sb.WriteString("\n  ")
		sb.WriteString(strings.ReplaceAll(content, "\n", " "))
	}
	return sb.String()
}

var _ = fmt.Sprintf

func deferSortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// itoa avoids importing strconv into a file whose only numeric need is
// making five seed bodies distinct from one another.
func deferItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
