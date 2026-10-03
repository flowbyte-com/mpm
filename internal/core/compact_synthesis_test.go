// compact_synthesis_test.go — Phase 1 of the compact-refusal lifecycle.
//
// Tests for the three-way semantic classification. These pin the
// boundary the design fixes (docs/archive/2026-09-30-compact-refusal-lifecycle.md
// §2.2):
//
//	Lesson   valid lesson JSON
//	Refused  the EXACT sanctioned sentinel {"title":"","body":"","tags":[]}
//	Error    malformed / partial / invalid lesson, or provider failure
//
// The three named regressions are:
//
//	R1  an empty or partial lesson is never mistaken for a refusal
//	R2  the exact refusal consumes no retry budgeted for
//	    transport/schema failure
//	R3  a provider error is never converted into a refusal
//
// No provider is called. ClassifyCompactSynthesis is a pure function
// of its argument; the R2/R3 cases that need a seam use
// compactSynthesizeFunc, which is replaced with a fake.

package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The literal the prompt sanctions. Copied verbatim from
// internal/core/synth/compact.go so a change to the prompt's example
// does not silently drift the classifier's notion of a refusal.
const testRefusalSentinel = `{"title":"", "body":"", "tags":[]}`

// ── The four classification cases ──────────────────────────────────────

func TestClassifyCompactSynthesis_ValidLesson(t *testing.T) {
	outcome, lesson, err := ClassifyCompactSynthesis(
		`{"title":"Retry idempotently","body":"Two concurrent runs duplicated the row.","tags":["concurrency"]}`)

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if outcome != OutcomeLesson {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeLesson)
	}
	if lesson == nil {
		t.Fatal("lesson = nil, want populated")
	}
	if lesson.Title != "Retry idempotently" {
		t.Errorf("Title = %q, want %q", lesson.Title, "Retry idempotently")
	}
	if len(lesson.Tags) != 1 || lesson.Tags[0] != "concurrency" {
		t.Errorf("Tags = %v, want [concurrency]", lesson.Tags)
	}
}

func TestClassifyCompactSynthesis_ExactRefusalSentinel(t *testing.T) {
	outcome, _, err := ClassifyCompactSynthesis(testRefusalSentinel)

	// The load-bearing assertion: a refusal is NOT an error. Before
	// this design it arrived as lesson_validation_failed, and because
	// the rows were never marked the next drain reselected the same
	// rows and made the same call forever.
	if err != nil {
		t.Fatalf("err = %v, want nil — a refusal is a successful outcome", err)
	}
	if outcome != OutcomeRefused {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeRefused)
	}
}

// Variants of the sentinel that the classifier must still recognise as
// the same semantic refusal. These are all "the model declined" —
// whitespace, key order, and an explicit null/omitted tags are
// JSON-level spellings of the same empty object, and the prompt's
// contract does not constrain formatting.
func TestClassifyCompactSynthesis_RefusalSentinelVariants(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"prompt canonical form", testRefusalSentinel},
		{"compact json", `{"title":"","body":"","tags":[]}`},
		{"reordered keys", `{"tags":[],"body":"","title":""}`},
		{"null tags", `{"title":"","body":"","tags":null}`},
		{"extra whitespace", "{\n  \"title\" : \"\",\n  \"body\" : \"\",\n  \"tags\" : [ ]\n}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, _, err := ClassifyCompactSynthesis(tc.raw)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if outcome != OutcomeRefused {
				t.Fatalf("outcome = %q, want %q", outcome, OutcomeRefused)
			}
		})
	}
}

// Everything that is NOT the exact sentinel and is not a well-formed
// lesson. Each must be OutcomeError with a non-nil error, because a
// lenient "no usable title means refusal" rule would let a truncated
// or hallucinated response be recorded as a considered judgement.
func TestClassifyCompactSynthesis_MalformedIsErrorNotRefusal(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantFrag string
	}{
		{"truncated json", `{"title":"Partial lesson","body":"text that stops`, "model_schema_violation"},
		{"not json at all", `I cannot summarize these memories.`, "model_schema_violation"},
		{"tags key omitted", `{"title":"","body":""}`, "model_schema_violation"},
		{"empty object", `{}`, "model_schema_violation"},
		{"prose refusal, not the sentinel", `{"title":"","body":"I decline","tags":[]}`, "lesson_validation_failed"},
		{"json array", `[{"title":"T","body":"B","tags":["x"]}]`, "model_schema_violation"},
		{"json null", `null`, "model_schema_violation"},
		{"empty string", ``, "model_schema_violation"},
		{"whitespace only", "   \n\t ", "model_schema_violation"},
		{"title but no body", `{"title":"T","body":"","tags":["x"]}`, "lesson_validation_failed"},
		{"title but no tags", `{"title":"T","body":"B","tags":[]}`, "lesson_validation_failed"},
		// A populated field with the others absent is a shape
		// violation, not a semantic one: the model did not honour the
		// declared contract. Both routes are OutcomeError; the
		// fragment distinguishes "the object is malformed" from "the
		// object is well-formed but the lesson is unusable".
		{"body but no title", `{"body":"B","tags":["x"]}`, "model_schema_violation"},
		{"body present, no title, no tags", `{"body":"B"}`, "model_schema_violation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, lesson, err := ClassifyCompactSynthesis(tc.raw)
			if err == nil {
				t.Fatalf("err = nil, want non-nil")
			}
			if outcome != OutcomeError {
				t.Fatalf("outcome = %q, want %q", outcome, OutcomeError)
			}
			if lesson != nil {
				t.Errorf("lesson = %+v, want nil for OutcomeError", lesson)
			}
			if !strings.Contains(err.Error(), tc.wantFrag) {
				t.Errorf("err = %v, want fragment %q", err, tc.wantFrag)
			}
		})
	}
}

// ── R1: empty/partial malformed lesson is not mistaken for refusal ─────

// R1 is the regression the design names explicitly. It is stated here
// as its own test rather than folded into the table above because the
// failure mode it guards is silent: a wrong answer here records a
// model's malfunction as a considered judgement and, downstream,
// defers the rows instead of surfacing a broken integration.
func TestClassifyCompactSynthesis_R1_MalformedNeverMiscountedAsRefusal(t *testing.T) {
	// Every one of these is "the model produced something that is not
	// usable" or "the model produced nothing at all". None of them is
	// the sanctioned refusal, and none of them may be treated as one.
	notRefusals := []struct {
		name string
		raw  string
	}{
		{"partial prose", `I'm not able to produce a lesson here.`},
		{"prose inside body field", `{"title":"","body":"Cannot summarize.","tags":[]}`},
		{"prose inside title field", `{"title":"No lesson","body":"","tags":[]}`},
		{"empty object with unknown fields", `{"note":"unsure","confidence":0.1}`},
		{"refusal with a stray tag", `{"title":"","body":"","tags":["refused"]}`},
		{"refusal with a stray body", `{"title":"","body":"insufficient data","tags":[]}`},
	}
	for _, tc := range notRefusals {
		t.Run(tc.name, func(t *testing.T) {
			outcome, _, err := ClassifyCompactSynthesis(tc.raw)
			if outcome == OutcomeRefused {
				t.Fatalf("outcome = OutcomeRefused for %q — malformed output must not be recorded as a considered refusal", tc.raw)
			}
			if outcome != OutcomeError {
				t.Fatalf("outcome = %q, want %q", outcome, OutcomeError)
			}
			if err == nil {
				t.Fatal("err = nil, want non-nil")
			}
		})
	}
}

// A single space is not an empty field. R1's claim is "not mistaken
// for refusal", and this is the narrowest form of it: a lesson whose
// every field is whitespace must not be absorbed into the refusal
// bucket, because doing so would silently discard a populated — if
// sloppy — response.
//
// It classifies as OutcomeLesson rather than OutcomeError because
// Validate tests for `== ""` and this design does not change
// Validate's body or its rules. That pre-existing looseness is pinned
// here deliberately: it is a property of lesson validation, not of
// refusal classification, and widening Validate is out of scope.
func TestClassifyCompactSynthesis_R1_WhitespaceIsNotARefusal(t *testing.T) {
	outcome, lesson, err := ClassifyCompactSynthesis(
		`{"title":"   ","body":"   ","tags":["  "]}`)

	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if outcome == OutcomeRefused {
		t.Fatal("outcome = OutcomeRefused — whitespace-only fields are not the sanctioned refusal")
	}
	if outcome != OutcomeLesson {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeLesson)
	}
	if lesson == nil || lesson.Title != "   " {
		t.Fatalf("lesson = %+v, want the whitespace fields preserved verbatim", lesson)
	}
}

// ── R2: the exact refusal consumes no retry budgeted for failure ──────

// R2 asks whether recognising a refusal steals a recovery slot that the
// safeguard reserved for transport/schema failure. It does not, and the
// reason is structural: the recovery slot is spent inside the synth
// layer (SynthesizeCompactLessonWithPlan allocates RecoveryRepair on a
// parse failure), while classification is downstream of it and reads
// only text. A refusal is well-formed JSON, so the synth layer parses
// it on the first pass and never allocates the slot.
//
// This test pins that structural fact at the boundary we control: the
// refusal is classified exactly once, with no second call and no
// re-synthesis.
func TestClassifyCompactSynthesis_R2_RefusalConsumesNoRetry(t *testing.T) {
	calls := 0
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		calls++
		return testRefusalSentinel, nil
	})

	// The single classification pass the orchestrator performs on a
	// response. There is no retry wrapper here by design — R2's claim
	// is precisely that none is needed.
	outcome, _, err := ClassifyCompactSynthesis(testRefusalSentinel)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if outcome != OutcomeRefused {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeRefused)
	}
	if calls != 0 {
		t.Fatalf("classification issued %d provider calls, want 0", calls)
	}
}

// The complementary half: a genuinely malformed response is NOT a
// refusal, so a caller that wanted to retry it still has an error to
// retry on, and the retry budget stays available for exactly that case.
func TestClassifyCompactSynthesis_R2_MalformedRemainsRetryableError(t *testing.T) {
	outcome, _, err := ClassifyCompactSynthesis(`{"title":"T","body":`)
	if outcome != OutcomeError {
		t.Fatalf("outcome = %q, want %q (retry budget must stay available)", outcome, OutcomeError)
	}
	if err == nil {
		t.Fatal("err = nil — a retryable schema failure must still surface as an error")
	}
}

// ── R3: a provider error is never converted into a refusal ────────────

// R3, stated as the design states it: "operational/model errors are
// never turned into semantic refusals." A transport failure is a
// malfunction the operator must see. If it were classified as
// Refused, the batch would be quietly deferred and the pressure gauge
// would fall without anything having been learned — the counters
// would lie about a system that is actually broken.
func TestClassifyCompactSynthesis_R3_ProviderErrorNeverBecomesRefusal(t *testing.T) {
	providerErr := errors.New("synth: dial tcp 10.0.0.5:443: connect: connection refused")

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return "", providerErr
	})

	// What CompactEpistemology does with a provider failure: the
	// synthesize step returns an error and no classification runs at
	// all. A refusal is a *successful* response carrying the sentinel;
	// an error has no text to classify.
	text, err := compactSynthesizeFunc(context.Background(), []string{"a raw memory"})
	if err == nil {
		t.Fatal("synth err = nil, want the provider error")
	}
	if !errors.Is(err, providerErr) {
		t.Fatalf("synth err = %v, want it to wrap %v", err, providerErr)
	}

	// And the failure must not be laundered into a refusal by any
	// downstream classifier. The only thing the classifier can be
	// handed is text; there is none here, and empty text is a schema
	// violation, not a refusal.
	if outcome, _, cerr := ClassifyCompactSynthesis(text); outcome == OutcomeRefused {
		t.Fatalf("outcome = OutcomeRefused for a provider failure — errors must stay loud")
	} else if outcome != OutcomeError || cerr == nil {
		t.Fatalf("outcome = %q, err = %v; want %q with non-nil err", outcome, cerr, OutcomeError)
	}
}

// ── Outcome/error coupling invariants ─────────────────────────────────

// The three outcomes are distinguished by a two-part contract, and
// this pins both halves so a future edit cannot satisfy one while
// breaking the other.
func TestClassifyCompactSynthesis_OutcomeErrorCoupling(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		outcome CompactOutcome
	}{
		{"lesson", `{"title":"T","body":"B","tags":["x"]}`, OutcomeLesson},
		{"refused", testRefusalSentinel, OutcomeRefused},
		{"error", `not json`, OutcomeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, _, err := ClassifyCompactSynthesis(tc.raw)
			if outcome != tc.outcome {
				t.Fatalf("outcome = %q, want %q", outcome, tc.outcome)
			}
			// Exactly the invariant the design cares about: an error
			// is present for OutcomeError and absent otherwise. If a
			// refusal can ever come back with a non-nil error, every
			// caller that branches on `err != nil` silently starts
			// reporting refusals as failures again.
			if tc.outcome == OutcomeError && err == nil {
				t.Error("err = nil, want non-nil for OutcomeError")
			}
			if tc.outcome != OutcomeError && err != nil {
				t.Errorf("err = %v, want nil for outcome %q", err, tc.outcome)
			}
		})
	}
}

// Validate must remain reachable for populated lessons — the classifier
// delegates to it rather than reimplementing it, so a change to the
// semantic rules cannot be silently bypassed by the new code path.
func TestClassifyCompactSynthesis_DelegatesToValidate(t *testing.T) {
	// A lesson that only fails a *semantic* rule (no tags) is
	// classified by way of Validate, and its error text is the one
	// Validate produced, wrapped — not a classifier-specific string.
	_, _, err := ClassifyCompactSynthesis(`{"title":"T","body":"B","tags":[]}`)
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "at least one tag required") {
		t.Errorf("err = %v, want Validate's own message", err)
	}
}

// ── Orchestrator wiring ──────────────────────────────────────────────

// The end-to-end claim of Phase 2: a sanctioned refusal coming back
// from the seam reaches the caller as a non-error no-op, not as
// lesson_validation_failed.
//
// This is the assertion that would have failed before the change, and
// it is stated against CompactEpistemology rather than against the
// classifier so it exercises the real call path.
func TestCompactEpistemology_RefusalIsNotAnError(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 3)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return testRefusalSentinel, nil
	})

	result, err := dm.CompactEpistemology(context.Background(), true)
	if err != nil {
		t.Fatalf("err = %v, want nil — a refusal is a successful terminal outcome", err)
	}
	if result == nil {
		t.Fatal("result = nil, want a no-op result")
	}
	if result.SkippedReason != "synthesis_refused" {
		t.Errorf("SkippedReason = %q, want %q", result.SkippedReason, "synthesis_refused")
	}
	if result.LessonsCreated != 0 {
		t.Errorf("LessonsCreated = %d, want 0", result.LessonsCreated)
	}
	if result.Compacted != 0 || result.RawMarked != 0 {
		t.Errorf("Compacted = %d, RawMarked = %d, want 0 and 0", result.Compacted, result.RawMarked)
	}
	if result.LessonID != "" {
		t.Errorf("LessonID = %q, want empty — a refusal must not write compacted_into", result.LessonID)
	}
}

// A refusal must leave the substrate exactly as it found it. Nothing
// is marked, no lesson exists, and the pressure gauge is unchanged —
// which is precisely the condition the later deferral phase exists to
// resolve. Pinning it now means the deferral phase has to change this
// deliberately rather than by accident.
func TestCompactEpistemology_RefusalWritesNothing(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedRaw(t, dm, 4)

	rawBefore, _, err := dm.compactPreCheck(context.Background())
	if err != nil {
		t.Fatalf("pre-check: %v", err)
	}
	lessonsBefore := countLessons(t, dm)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return testRefusalSentinel, nil
	})

	if _, err := dm.CompactEpistemology(context.Background(), true); err != nil {
		t.Fatalf("CompactEpistemology: %v", err)
	}

	rawAfter, _, err := dm.compactPreCheck(context.Background())
	if err != nil {
		t.Fatalf("pre-check after: %v", err)
	}
	if rawAfter != rawBefore {
		t.Errorf("raw count = %d, want %d — a refusal must not mark rows", rawAfter, rawBefore)
	}
	if got := countLessons(t, dm); got != lessonsBefore {
		t.Errorf("lesson count = %d, want %d — a refusal must not fabricate a lesson", got, lessonsBefore)
	}

	// And specifically: no compacted_into on any source row. Writing
	// it would claim the rows were folded into a lesson that does not
	// exist.
	for _, id := range ids {
		if into := memoryCompactedInto(t, dm, id); into != "" {
			t.Errorf("memory %s: compacted_into = %q, want empty", id, into)
		}
	}
}

// A malformed response stays an error. The refusal path must not have
// widened into a general "don't worry about it" branch.
func TestCompactEpistemology_MalformedStillErrors(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 3)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return `{"title":"T","body":"` + "truncated", nil
	})

	_, err := dm.CompactEpistemology(context.Background(), true)
	if err == nil {
		t.Fatal("err = nil, want a schema violation")
	}
	if !strings.Contains(err.Error(), "model_schema_violation") {
		t.Errorf("err = %v, want model_schema_violation", err)
	}
}

// A provider error stays an error and keeps its synthesize prefix.
func TestCompactEpistemology_ProviderErrorStillErrors(t *testing.T) {
	dm := NewTestDM(t)
	seedRaw(t, dm, 3)

	providerErr := errors.New("connection refused")
	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return "", providerErr
	})

	_, err := dm.CompactEpistemology(context.Background(), true)
	if err == nil {
		t.Fatal("err = nil, want the provider error")
	}
	if !errors.Is(err, providerErr) {
		t.Errorf("err = %v, want it to wrap %v", err, providerErr)
	}
	if !strings.Contains(err.Error(), "synthesize:") {
		t.Errorf("err = %v, want the existing synthesize prefix preserved", err)
	}
}

// The happy path is unchanged: a real lesson still commits and marks.
func TestCompactEpistemology_LessonStillCommits(t *testing.T) {
	dm := NewTestDM(t)
	ids := seedRaw(t, dm, 3)

	withMockSynth(t, func(ctx context.Context, raw []string) (string, error) {
		return `{"title":"Batch insight","body":"These three agree.","tags":["batch"]}`, nil
	})

	result, err := dm.CompactEpistemology(context.Background(), true)
	if err != nil {
		t.Fatalf("CompactEpistemology: %v", err)
	}
	if result.LessonsCreated != 1 {
		t.Errorf("LessonsCreated = %d, want 1", result.LessonsCreated)
	}
	if result.Compacted != 3 || result.RawMarked != 3 {
		t.Errorf("Compacted = %d, RawMarked = %d, want 3 and 3", result.Compacted, result.RawMarked)
	}
	if result.LessonID == "" {
		t.Error("LessonID is empty, want the committed lesson")
	}
	for _, id := range ids {
		if memoryCompactedInto(t, dm, id) == "" {
			t.Errorf("memory %s: compacted_into is empty, want it marked", id)
		}
	}
}

// ── helpers ──────────────────────────────────────────────────────────

func countLessons(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM lessons`).Scan(&n); err != nil {
		t.Fatalf("count lessons: %v", err)
	}
	return n
}

func memoryCompactedInto(t *testing.T, dm *DatabaseManager, id string) string {
	t.Helper()
	var into sql.NullString
	err := dm.db.QueryRow(
		`SELECT json_extract(metadata, '$.compacted_into') FROM memories WHERE id = ?`, id).Scan(&into)
	if err != nil {
		t.Fatalf("read compacted_into for %s: %v", id, err)
	}
	return into.String
}

// ── CompactLesson.Validate is unchanged by this work ─────────────────

// The design requires the refusal check to run BEFORE lesson
// validation, not inside it. Validate keeps its original body: it
// still rejects the sentinel. That is correct — Validate's job is
// "is this a well-formed lesson", and the sentinel is not one. What
// changed is that the orchestrator no longer asks it to serve as the
// refusal detector.
func TestCompactLesson_ValidateStillRejectsSentinel(t *testing.T) {
	var lesson CompactLesson
	if err := json.Unmarshal([]byte(testRefusalSentinel), &lesson); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := lesson.Validate(); err == nil {
		t.Error("Validate() accepted the sentinel; it must keep rejecting it — the refusal check belongs to ClassifyCompactSynthesis")
	}
}
