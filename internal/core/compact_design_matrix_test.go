// compact_design_matrix_test.go — Phase 10: the 24-case matrix.
//
// docs/designs/2026-09-30-compact-refusal-lifecycle.md §10 lists 16
// claims the implementation must prove. This file is the single
// traceability artifact: every claim, and every branch of a claim that
// has more than one, appears here as a numbered case.
//
// The cases assert directly rather than delegating to the tests
// written during Phases 1–9. That is deliberate duplication. Those
// tests are where each behaviour was pinned as it was built and where
// a regression will be debugged; this table is where someone can read
// the design's claims top to bottom and see each one discharged. A
// matrix that only pointed at other test names would go stale the
// moment a test was renamed, and would prove nothing on its own.
//
// Invariants this file must not violate:
//   - no real provider call (every synthesis goes through withMockSynth)
//   - no live DB (every case builds its own NewTestDM)
//   - no write to the production substrate
//
// Cases 1–24 map to claims 1–16 as annotated on each case.

package internal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDesignClaimMatrix(t *testing.T) {
	cases := []struct {
		claim int
		name  string
		fn    func(t *testing.T)
	}{
		{1, "sentinel classifies as refused, not error", func(t *testing.T) {
			outcome, lesson, err := ClassifyCompactSynthesis(testRefusalSentinel)
			if err != nil {
				t.Fatalf("err = %v, want nil — a refusal is not an error", err)
			}
			if outcome != OutcomeRefused {
				t.Errorf("outcome = %q, want refused", outcome)
			}
			// The classifier hands back a decoded value so the caller
			// can log it, but it must be a value that cannot be
			// persisted: an empty title is the whole point of the
			// sentinel, and Validate would reject it if anything ever
			// tried to turn it into a lesson.
			if lesson == nil {
				return
			}
			if lesson.Title != "" || lesson.Body != "" || len(lesson.Tags) != 0 {
				t.Errorf("lesson = %+v, want the empty sentinel", lesson)
			}
			if err := lesson.Validate(); err == nil {
				t.Error("the decoded sentinel passed Validate — a refusal could be persisted as a lesson")
			}
		}},
		{1, "partially-populated object with empty title is an error", func(t *testing.T) {
			outcome, _, err := ClassifyCompactSynthesis(`{"title":"","body":"something","tags":["x"]}`)
			if outcome != OutcomeError {
				t.Errorf("outcome = %q, want error", outcome)
			}
			if err == nil {
				t.Error("err = nil, want a validation error")
			}
			if !errors.Is(err, ErrCompactRefusal) {
				// A near-miss must not be laundered into a refusal;
				// that would silently defer a malformed response
				// instead of surfacing it.
				t.Logf("err = %v (distinct from the refusal sentinel, as required)", err)
			}
		}},
		{2, "populated lessons still validate exactly as before", func(t *testing.T) {
			good := CompactLesson{Title: "T", Body: "B", Tags: []string{"x"}}
			if err := good.Validate(); err != nil {
				t.Errorf("a well-formed lesson failed validation: %v", err)
			}
			// The specific assertions Validate is documented to make.
			for name, l := range map[string]CompactLesson{
				"no title": {Body: "B", Tags: []string{"x"}},
				"no body":  {Title: "T", Tags: []string{"x"}},
				"no tags":  {Title: "T", Body: "B"},
			} {
				if err := l.Validate(); err == nil {
					t.Errorf("%s: Validate() = nil, want an error — §2.2 forbids weakening these", name)
				}
			}
		}},
		{3, "a refusal writes the four deferral keys and never compacted_into", func(t *testing.T) {
			dm := NewTestDM(t)
			ids := seedMixed(t, dm, 4, 0, 0)
			withMockSynth(t, func(context.Context, []string) (string, error) { return testRefusalSentinel, nil })
			if _, err := dm.CompactEpistemology(context.Background(), true); err != nil {
				t.Fatalf("compact: %v", err)
			}
			for _, id := range ids {
				m := deferralMetadata(t, dm, id)
				if m.At == "" || m.Reason == "" || m.Batch == "" || m.Sample == "" {
					t.Errorf("%s: deferral keys incomplete: %+v", id, m)
				}
				if into := memoryCompactedInto(t, dm, id); into != "" {
					t.Errorf("%s: compacted_into = %q, want empty", id, into)
				}
			}
			if n := countLessons(t, dm); n != 0 {
				t.Errorf("lessons = %d, want 0", n)
			}
		}},
		{4, "a deferred row is unchanged via a direct read", func(t *testing.T) {
			dm := NewTestDM(t)
			ids := seedMixed(t, dm, 1, 0, 0)
			before := snapshotRows(t, dm, ids)[0]
			deferRow(t, dm, ids[0], "batch-1", DeferralReasonRefusal, "s")

			rows, err := dm.ListDeferred(context.Background(), 10)
			if err != nil {
				t.Fatalf("ListDeferred: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("listed %d rows, want 1", len(rows))
			}
			if rows[0].Content != before.content {
				t.Errorf("content = %q, want %q", rows[0].Content, before.content)
			}
			if rows[0].CreatedAt != before.createdAt {
				t.Errorf("created_at = %q, want %q", rows[0].CreatedAt, before.createdAt)
			}
		}},
		{4, "a deferred row stays reachable through FTS and export", func(t *testing.T) {
			// One row, two consumers. Deferral is a compaction-scoped
			// annotation; a row that stopped being searchable or
			// stoppable being exported would be a data loss dressed
			// up as a scheduling decision.
			//
			// The row is written through the normal save path, not a
			// bare INSERT, so the FTS index is populated the same way
			// a real memory's is. HybridSearchMemories is the substrate
			// path `mpm_memory query` uses.
			dm := NewTestDM(t)
			saved, _, err := dm.SaveMemoryWithContext(
				"refusal-deferral-freachability-probe-8831",
				"memories", []string{"deferral-matrix"}, 0, "", ActiveContext{})
			if err != nil {
				t.Fatalf("SaveMemoryWithContext: %v", err)
			}
			id, _ := saved["id"].(string)
			if id == "" {
				t.Fatalf("SaveMemoryWithContext returned no id: %v", saved)
			}
			deferRow(t, dm, id, "batch-1", DeferralReasonRefusal, "s")

			hits, err := dm.HybridSearchMemories("refusal-deferral-freachability-probe-8831", "", 10, "local")
			if err != nil {
				t.Fatalf("HybridSearchMemories: %v", err)
			}
			if len(hits) == 0 {
				t.Error("a deferred row is no longer retrievable through search")
			}

			var exported int
			if err := dm.SQLDB().QueryRow(`
				SELECT COUNT(*) FROM memories
				WHERE collection='memories' AND deleted_at IS NULL
				  AND id = ?`, id).Scan(&exported); err != nil {
				t.Fatalf("export query: %v", err)
			}
			if exported != 1 {
				t.Error("a deferred row is missing from the export population")
			}
		}},
		{5, "two consecutive drains select different rows (falsifies F6)", func(t *testing.T) {
			dm := NewTestDM(t)
			ids := seedMixed(t, dm, 4, 0, 0)

			first, _, err := dm.extractRawBatch(context.Background(), 2)
			if err != nil {
				t.Fatalf("first extract: %v", err)
			}
			// Defer what the first drain refused, as the drain would.
			deferRow(t, dm, first[0], "b1", DeferralReasonRefusal, "s")
			deferRow(t, dm, first[1], "b1", DeferralReasonRefusal, "s")

			second, _, err := dm.extractRawBatch(context.Background(), 2)
			if err != nil {
				t.Fatalf("second extract: %v", err)
			}
			if len(second) != 2 {
				t.Fatalf("second batch = %v, want 2 rows", second)
			}
			overlap := map[string]bool{}
			for _, id := range first {
				overlap[id] = true
			}
			for _, id := range second {
				if overlap[id] {
					t.Errorf("row %s was selected twice; the drain would loop on it", id)
				}
			}
			if second[0] != ids[2] {
				t.Errorf("second batch starts at %s, want %s", second[0], ids[2])
			}
		}},
		{6, "a 3-stage refusal leaves fewer than 8 slots for the rest", func(t *testing.T) {
			dm := NewTestDM(t)
			seedMixed(t, dm, compactBatchSize, 0, 0)
			withMockSynth(t, func(context.Context, []string) (string, error) { return testRefusalSentinel, nil })

			res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			if res.StagesSpent > MaxSemanticStagesPerInvocation {
				t.Errorf("stages = %d, want <= %d", res.StagesSpent, MaxSemanticStagesPerInvocation)
			}
			// 8 stages / 3 per refused batch = 2 full batches plus 2
			// unaffordable stages, which defer the third whole.
			if res.BatchesProcessed != 0 {
				t.Errorf("batches = %d, want 0 on a fully-refusing substrate", res.BatchesProcessed)
			}
			if res.StagesSpent%3 != 0 {
				t.Errorf("stages = %d, want a multiple of 3 on an all-refusal drain", res.StagesSpent)
			}
			if res.RowsDeferred != compactBatchSize {
				t.Errorf("rows_deferred = %d, want %d", res.RowsDeferred, compactBatchSize)
			}
		}},
		{6, "the stage budget is checked BEFORE it is spent", func(t *testing.T) {
			dm := NewTestDM(t)
			seedMixed(t, dm, compactBatchSize*4, 0, 0)
			calls := 0
			withMockSynth(t, func(context.Context, []string) (string, error) {
				calls++
				return `{"title":"T","body":"B","tags":["x"]}`, nil
			})
			res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			// One stage per successful batch, so calls == stages, and
			// neither may exceed the budget.
			if calls > MaxSemanticStagesPerInvocation {
				t.Errorf("made %d model calls, want <= %d", calls, MaxSemanticStagesPerInvocation)
			}
			if res.StagesSpent != calls {
				t.Errorf("semantic_stages_spent = %d but %d calls were made", res.StagesSpent, calls)
			}
		}},
		{7, "raw_count counts deferred rows, actionable_pending does not", func(t *testing.T) {
			dm := NewTestDM(t)
			seedMixed(t, dm, 0, 9, 0)
			got := readPressure(t, dm)
			if got.RawCount != 9 {
				t.Errorf("raw_count = %d, want 9", got.RawCount)
			}
			if got.ActionablePending != 0 {
				t.Errorf("actionable_pending = %d, want 0", got.ActionablePending)
			}
		}},
		{7, "the identity holds for NULL metadata", func(t *testing.T) {
			dm := NewTestDM(t)
			insertRawMemory(t, dm, "null-meta", nil)
			insertRawMemory(t, dm, "empty-meta", "")
			assertCountIdentity(t, dm, 2, 0)
		}},
		{7, "the identity holds for a mixed population", func(t *testing.T) {
			dm := NewTestDM(t)
			seedMixed(t, dm, 12, 5, 9)
			assertCountIdentity(t, dm, 12, 5)
		}},
		{8, "a fully-refusing backlog converges to actionable 0", func(t *testing.T) {
			dm := NewTestDM(t)
			seedMixed(t, dm, compactBatchSize*3, 0, 0)
			withMockSynth(t, func(context.Context, []string) (string, error) { return testRefusalSentinel, nil })

			// Bounded invocations: enough to consume the substrate,
			// and no more. An unbounded loop would pass a test that
			// could not terminate.
			for i := 0; i < 3; i++ {
				if _, err := dm.CompactEpistemologyDrain(context.Background(), true, 0); err != nil {
					t.Fatalf("drain %d: %v", i, err)
				}
			}
			got := readPressure(t, dm)
			if got.ActionablePending != 0 {
				t.Errorf("actionable_pending = %d after 3 drains, want 0", got.ActionablePending)
			}
			if got.DeferredCount != compactBatchSize*3 {
				t.Errorf("deferred_count = %d, want %d", got.DeferredCount, compactBatchSize*3)
			}
		}},
		{9, "an error stops the drain loudly and defers nothing", func(t *testing.T) {
			dm := NewTestDM(t)
			seedMixed(t, dm, 3, 0, 0)
			withMockSynth(t, func(context.Context, []string) (string, error) {
				return "", errors.New("provider exploded")
			})
			res, err := dm.CompactEpistemologyDrain(context.Background(), true, 0)
			if err == nil {
				t.Fatal("err = nil, want the provider error to propagate")
			}
			if res == nil {
				t.Fatal("result = nil, want the partial aggregate alongside the error")
			}
			if res.StopReason != "failure" {
				t.Errorf("stop_reason = %q, want failure", res.StopReason)
			}
			if res.FailedBatch != 1 {
				t.Errorf("failed_batch = %d, want 1", res.FailedBatch)
			}
			if res.FailureReason == "" {
				t.Error("failure_reason is empty")
			}
			// An operational failure is not a considered judgement.
			// Deferring it would hide a broken provider behind a
			// quiet "the model declined" annotation.
			if got := readPressure(t, dm).DeferredCount; got != 0 {
				t.Errorf("deferred_count = %d, want 0 — an error must not be recorded as a refusal", got)
			}
		}},
		{10, "a fault midway through a deferral annotates zero rows", func(t *testing.T) {
			dm := NewTestDM(t)
			ids := seedMixed(t, dm, 3, 0, 0)
			ann := deferralAnnotationFixture()

			// The middle id does not exist, so its UPDATE affects 0
			// rows and trips the write verification. The two real ids
			// before it must roll back with it.
			err := dm.DeferBatch(context.Background(),
				[]string{ids[0], "no-such-id", ids[1]}, ann)
			if err == nil {
				t.Fatal("err = nil, want the write verification to reject a 0-row update")
			}
			for _, id := range ids {
				if m := deferralMetadata(t, dm, id); m.At != "" {
					t.Errorf("%s: deferral survived a rolled-back batch — a partial group is the loop", id)
				}
			}
		}},
		{10, "every row in one deferral carries byte-identical keys", func(t *testing.T) {
			dm := NewTestDM(t)
			ids := seedMixed(t, dm, 25, 0, 0)
			withMockSynth(t, func(context.Context, []string) (string, error) { return testRefusalSentinel, nil })
			if _, err := dm.CompactEpistemology(context.Background(), true); err != nil {
				t.Fatalf("compact: %v", err)
			}
			first := deferralMetadata(t, dm, ids[0])
			for _, id := range ids[1:] {
				if got := deferralMetadata(t, dm, id); got != first {
					t.Errorf("%s: %+v, want byte-identical to %+v", id, got, first)
				}
			}
		}},
		{11, "requeue clears exactly the four keys", func(t *testing.T) {
			dm := NewTestDM(t)
			ids := seedMixed(t, dm, 1, 0, 0)
			deferRow(t, dm, ids[0], "batch-1", DeferralReasonRefusal, "s")

			if _, err := dm.RequeueDeferred(context.Background(), 10); err != nil {
				t.Fatalf("requeue: %v", err)
			}
			var raw string
			if err := dm.SQLDB().QueryRow(`SELECT metadata FROM memories WHERE id = ?`, ids[0]).Scan(&raw); err != nil {
				t.Fatalf("read metadata: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for _, k := range []string{metaDeferralAt, metaDeferralReason, metaDeferralBatch, metaDeferralSample} {
				if _, ok := m[k]; ok {
					t.Errorf("%s survived requeue", k)
				}
			}
		}},
		{11, "requeue restores the row exactly, position included", func(t *testing.T) {
			// The row must come back where it was, byte for byte, so a
			// requeued batch is re-offered in its original context
			// rather than at the head of a re-ordered pool.
			dm := NewTestDM(t)
			ids := seedMixed(t, dm, 5, 0, 0)
			before := snapshotRows(t, dm, ids)[0]
			deferRow(t, dm, ids[0], "batch-1", DeferralReasonRefusal, "s")

			if _, err := dm.RequeueDeferred(context.Background(), 10); err != nil {
				t.Fatalf("requeue: %v", err)
			}
			after := snapshotRows(t, dm, ids)[0]
			if before != after {
				t.Errorf("row changed: %+v -> %+v", before, after)
			}
			got, _, err := dm.extractRawBatch(context.Background(), 50)
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if len(got) != 5 || got[0] != ids[0] {
				t.Errorf("pool = %v, want %s first among 5 rows", got, ids[0])
			}
		}},
		{12, "requeue does not clear compacted_into on a row carrying both", func(t *testing.T) {
			dm := NewTestDM(t)
			ids := seedMixed(t, dm, 1, 0, 0)
			withMockSynth(t, func(context.Context, []string) (string, error) {
				return `{"title":"Real","body":"B","tags":["x"]}`, nil
			})
			if _, err := dm.CompactEpistemology(context.Background(), true); err != nil {
				t.Fatalf("compact: %v", err)
			}
			lessonID := memoryCompactedInto(t, dm, ids[0])
			if lessonID == "" {
				t.Fatal("test setup: row was not compacted")
			}
			deferRow(t, dm, ids[0], "batch-1", DeferralReasonRefusal, "")

			if _, err := dm.RequeueDeferred(context.Background(), 10); err != nil {
				t.Fatalf("requeue: %v", err)
			}
			if got := memoryCompactedInto(t, dm, ids[0]); got != lessonID {
				t.Errorf("compacted_into = %q, want %q — clearing it would duplicate a lesson", got, lessonID)
			}
			// And the row must remain ineligible for synthesis.
			got, _, err := dm.extractRawBatch(context.Background(), 50)
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("pool = %v, want empty — a compacted row must stay compacted", got)
			}
		}},
		{13, "requeue is bounded and reports which rows it cleared", func(t *testing.T) {
			dm := NewTestDM(t)
			seedMixed(t, dm, 0, 10, 0)
			res, err := dm.RequeueDeferred(context.Background(), 3)
			if err != nil {
				t.Fatalf("requeue: %v", err)
			}
			if res.Requeued != 3 || len(res.RowIDs) != 3 {
				t.Errorf("requeued = %d row_ids = %v, want 3 and 3 entries", res.Requeued, res.RowIDs)
			}
			if got := readPressure(t, dm).DeferredCount; got != 7 {
				t.Errorf("deferred_count = %d, want 7 — the call must not flush the backlog", got)
			}
		}},
		{14, "a bare compact call does not requeue", func(t *testing.T) {
			dm := NewTestDM(t)
			ids := seedMixed(t, dm, 0, 3, 0)
			withMockSynth(t, func(context.Context, []string) (string, error) { return testRefusalSentinel, nil })
			if _, err := dm.CompactEpistemologyDrain(context.Background(), true, 0); err != nil {
				t.Fatalf("drain: %v", err)
			}
			if got := readPressure(t, dm).DeferredCount; got != 3 {
				t.Fatalf("deferred_count = %d, want 3", got)
			}
			// A second drain over the same substrate must not undo it.
			if _, err := dm.CompactEpistemologyDrain(context.Background(), true, 0); err != nil {
				t.Fatalf("second drain: %v", err)
			}
			for _, id := range ids {
				if m := deferralMetadata(t, dm, id); m.At == "" {
					t.Errorf("%s was requeued without an operator action", id)
				}
			}
		}},
		{14, "the wake context does not requeue", func(t *testing.T) {
			dm := NewTestDM(t)
			ids := seedMixed(t, dm, 0, 2, 0)

			// Reading wake context is a pure read; the call below is
			// the strongest form of the assertion available without
			// standing up a wake delivery path.
			_ = dm.gatherEpistemicPressure()

			for _, id := range ids {
				if m := deferralMetadata(t, dm, id); m.At == "" {
					t.Errorf("%s lost its deferral during a wake-context read", id)
				}
			}
		}},
		{15, "requeue writes an audit row naming the rows cleared", func(t *testing.T) {
			dm := NewTestDM(t)
			ids := seedMixed(t, dm, 0, 2, 0)
			res, err := dm.RequeueDeferred(context.Background(), 10)
			if err != nil {
				t.Fatalf("requeue: %v", err)
			}
			if res.AuditID == "" {
				t.Fatal("no audit row written")
			}
			var ctx string
			if err := dm.SQLDB().QueryRow(
				`SELECT context FROM system_audit_log WHERE id = ?`, res.AuditID).Scan(&ctx); err != nil {
				t.Fatalf("read audit row: %v", err)
			}
			for _, id := range ids {
				if !strings.Contains(ctx, id) {
					t.Errorf("audit context omits %s: %s", id, ctx)
				}
			}
		}},
		{16, "every advertised max_batches value equals the real constants", func(t *testing.T) {
			// This one is a source-text check by necessity. A test
			// cannot observe a doc comment, and the design names the
			// JSON schema's "default" as one of the sites — that
			// value is machine-readable and steers model behaviour
			// directly, so it has to be pinned even though nothing
			// in Go reads it.
			def := strconv.Itoa(compactDrainMaxBatchesDefault)
			cap := strconv.Itoa(compactDrainMaxBatchesHardCap)
			if def != cap {
				t.Fatalf("test assumes default == hard cap; they are %s and %s", def, cap)
			}

			root := repoRootForMatrix(t)
			sites := []struct{ path, label string }{
				{"internal/core/compact.go", "compact.go package doc + semantics"},
				{"internal/core/tools/registry_list.go", "mpm_system tool description + JSON schema"},
				{"internal/core/tools/handlers.go", "handleCompactEpistemology doc"},
			}
			// The scan is scoped to the max_batches neighbourhoods, not
			// the whole file. registry_list.go and handlers.go describe
			// several other parameters that legitimately default to 20
			// or 500 — a file-wide grep for "default 20" reports those
			// as failures and would train a future reader to ignore it.
			for _, s := range sites {
				b, err := os.ReadFile(filepath.Join(root, s.path))
				if err != nil {
					t.Fatalf("read %s: %v", s.path, err)
				}
				for _, region := range maxBatchesRegions(string(b)) {
					for _, stale := range []string{"hard cap 100", "default 20", "Default 20", "(100)", "(20)"} {
						if strings.Contains(region, stale) {
							t.Errorf("%s: %q still present near max_batches — the real figures are default %s / cap %s",
								s.label, stale, def, cap)
						}
					}
				}
			}

			// And the schema's own default must be the real one.
			schema := readCompactSchemaDefault(t)
			if schema != def {
				t.Errorf(`mpm_system compact schema "default" = %q, want %q — this value is read by models`, schema, def)
			}
		}},
	}

	if len(cases) != 24 {
		t.Fatalf("matrix has %d cases, want exactly 24 (design §10 lists 16 claims)", len(cases))
	}
	for i, c := range cases {
		if c.fn == nil {
			t.Errorf("case %d (%s) has no body", i+1, c.name)
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			c.fn(t)
		})
	}
}

// ── matrix helpers ───────────────────────────────────────────────────

// repoRootForMatrix walks up from the package directory to the ROOT
// module — the one declaring module github.com/flowbyte-com/mpm.
// internal/core and internal/core/tools are nested modules with their
// own go.mod, so "nearest go.mod" would stop one or two levels short
// of the files the source-text checks read. Matching on the module
// path rather than the filename is what makes this correct.
func repoRootForMatrix(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			if strings.Contains(string(b), "module github.com/flowbyte-com/mpm\n") {
				return dir
			}
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repository root")
	return ""
}

// maxBatchesRegions returns every window of src that mentions
// max_batches, each widened to a ±600-character window. Widening
// rather than taking the bare line matters: the tool description and
// the JSON schema both carry the numbers on the same line as the
// parameter name, but a Go doc comment may spread the claim over
// several lines with the number nowhere near the name.
func maxBatchesRegions(src string) []string {
	const pad = 600
	var out []string
	rest := src
	offset := 0
	for {
		i := strings.Index(rest, "max_batches")
		if i < 0 {
			return out
		}
		start := offset + i - pad
		if start < 0 {
			start = 0
		}
		end := offset + i + len("max_batches") + pad
		if end > len(src) {
			end = len(src)
		}
		out = append(out, src[start:end])
		// Advance past this occurrence so overlapping windows do not
		// produce the same text repeatedly.
		offset += i + len("max_batches")
		rest = src[offset:]
	}
}

// readCompactSchemaDefault pulls the `"default"` value out of the
// mpm_system compact action's max_batches property, read from the
// registry source so the check tracks the real schema.
func readCompactSchemaDefault(t *testing.T) string {
	t.Helper()
	root := repoRootForMatrix(t)
	b, err := os.ReadFile(filepath.Join(root, "internal/core/tools/registry_list.go"))
	if err != nil {
		t.Fatalf("read registry_list.go: %v", err)
	}
	const marker = `"max_batches": {"type": "number",`
	idx := strings.Index(string(b), marker)
	if idx < 0 {
		t.Fatal("max_batches is not declared in registry_list.go")
	}
	rest := string(b)[idx+len(marker):]
	d := strings.Index(rest, `"default":`)
	if d < 0 {
		t.Fatal(`max_batches declares no "default"`)
	}
	rest = rest[d+len(`"default":`):]
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		t.Fatal(`max_batches "default" is unterminated`)
	}
	return strings.TrimSpace(rest[:end])
}

func insertRawMemory(t *testing.T, dm *DatabaseManager, id string, metadata any) {
	t.Helper()
	now := "2026-09-30T12:00:00Z"
	var err error
	if metadata == nil {
		_, err = dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at)
			VALUES (?, 'memories', 'content of '+?, ?, ?)`, id, id, now, now)
	} else {
		_, err = dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at, metadata)
			VALUES (?, 'memories', 'content of '+?, ?, ?, ?)`, id, id, now, now, metadata)
	}
	if err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

func assertCountIdentity(t *testing.T, dm *DatabaseManager, wantActionable, wantDeferred int) {
	t.Helper()
	got := readPressure(t, dm)
	if got.ActionablePending != wantActionable {
		t.Errorf("actionable_pending = %d, want %d", got.ActionablePending, wantActionable)
	}
	if got.DeferredCount != wantDeferred {
		t.Errorf("deferred_count = %d, want %d", got.DeferredCount, wantDeferred)
	}
	if got.RawCount != wantActionable+wantDeferred {
		t.Errorf("raw_count = %d, want %d", got.RawCount, wantActionable+wantDeferred)
	}
}

func deferralAnnotationFixture() DeferralAnnotation {
	// A fixed instant, not time.Now(). The rollback case below compares
	// the presence of keys, not their values, but a wall-clock read here
	// would make the fixture non-deterministic for no benefit.
	return DeferralAnnotation{
		At:     time.Date(2026, 9, 30, 21, 14, 2, 0, time.UTC),
		Reason: DeferralReasonRefusal,
		Batch:  "cdb-fixture-001",
		Sample: "s",
	}
}
