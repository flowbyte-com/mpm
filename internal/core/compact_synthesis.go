// compact_synthesis.go — typed classification of a compact synthesis
// response.
//
// # Why this file exists
//
// The compact_epistemology prompt (internal/core/synth/compact.go)
// SANCTIONS a refusal as a successful terminal outcome, and names the
// exact sentinel: {"title":"", "body":"", "tags":[]}. It is produced
// when the batch is contradictory, ambiguous, mathematically undefined,
// or insufficiently specified, and the model is explicitly instructed
// to preserve the refusal rather than invent a lesson to fill the
// schema.
//
// The orchestrator used to unmarshal that sentinel into a
// zero-valued CompactLesson and hand it to CompactLesson.Validate(),
// which returned "title required". A correct model behaviour therefore
// arrived at persistence as an error, and — because the rows were never
// marked — the next drain invocation reselected the identical rows and
// made the identical call. That is the infinite loop this design
// removes. See docs/archive/2026-09-30-compact-refusal-lifecycle.md
// F1–F6.
//
// # The boundary
//
// wire text → [this file] → semantic outcome → lesson validation ONLY
// for OutcomeLesson.
//
// CompactLesson.Validate is NOT consulted for the sentinel. It keeps
// its body and its sole responsibility — deciding whether a POPULATED
// lesson is well-formed — and is simply never asked to adjudicate a
// refusal, because classification runs first. That separation is the
// whole point: the malformed-output check and the refusal check stop
// sharing one code path, so neither can be weakened by changing the
// other.

package internal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// CompactOutcome is the closed three-way result of classifying one
// model synthesis response.
//
// It exists because the three cases are genuinely different and were
// previously collapsed into two: an error either meant "the model
// broke its contract" or "the model correctly declined". Only the
// first is a failure.
type CompactOutcome string

const (
	// OutcomeLesson is a real lesson was produced. It has not yet been
	// validated — call CompactLesson.Validate before treating it as a
	// candidate. A Lesson outcome with a failing Validate is
	// OutcomeError, which is why Validate is called here rather than
	// left to the caller.
	OutcomeLesson CompactOutcome = "lesson"

	// OutcomeRefused is the sanctioned refusal sentinel. NOT an error.
	// The caller defers the batch per the refusal lifecycle.
	OutcomeRefused CompactOutcome = "refused"

	// OutcomeError is a genuine malfunction: unparseable JSON, a
	// truncated or hallucinated object, a populated object that fails
	// validation, or a provider/transport failure upstream. Always
	// carries a non-nil error.
	OutcomeError CompactOutcome = "error"
)

// ErrCompactRefusal is the sentinel-free marker returned alongside
// OutcomeRefused. The classification returns a nil error for a refusal
// (it is a successful outcome), so this exists purely for callers that
// want a comparable error value to branch on without re-deriving the
// outcome. It is never returned as the error field itself.
var ErrCompactRefusal = errors.New("compact: synthesis declined (sanctioned refusal)")

// compactLessonFields are the keys the prompt's JSON contract declares.
// The prompt names all three explicitly in the refusal example, so all
// three must be PRESENT for a response to be considered well-formed —
// let alone to be considered a refusal.
var compactLessonFields = []string{"title", "body", "tags"}

// hasCompactLessonShape reports whether raw is a JSON object that
// declares every field of the compact lesson contract.
//
// This guard exists because json.Unmarshal silently accepts shapes it
// has no business accepting, and each of them lands on a zero-valued
// CompactLesson — which is byte-for-byte indistinguishable from the
// refusal sentinel once decoded:
//
//	null                            → no error, lesson stays zero
//	{}                              → no error, lesson stays zero
//	{"note":"unsure","c":0.1}       → no error, all fields ignored
//
// Without this check all three are recorded as the model's considered
// judgement. With it they are the schema violations they are.
func hasCompactLessonShape(raw string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return fmt.Errorf("not a JSON object: %w", err)
	}
	// A bare `null` unmarshals into a nil map without error.
	if fields == nil {
		return errors.New("not a JSON object")
	}
	var missing []string
	for _, name := range compactLessonFields {
		if _, ok := fields[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing declared field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// isRefusalSentinel reports whether the parsed lesson is EXACTLY the
// sanctioned sentinel: every field empty.
//
// This is deliberately exact rather than lenient. The prompt names one
// specific object, so one specific object is what counts as a refusal.
// Anything else that fails validation is a model malfunction and is
// reported as one — the alternative (treating "no usable title" as a
// refusal) would let a truncated or hallucinated response be recorded
// as a considered judgement, and would make a genuinely broken
// integration permanently quiet.
//
// It is only ever consulted after hasCompactLessonShape has confirmed
// all three fields are present, so a zero value here means the model
// really did write three empty fields.
//
// The tags arm uses len(), so a nil slice and an empty slice both
// count as empty, and `{"tags": null}` is therefore the sentinel. That
// matches json.Unmarshal semantics for the declared field and is what
// the prompt's own example produces.
func isRefusalSentinel(l *CompactLesson) bool {
	return l.Title == "" && l.Body == "" && len(l.Tags) == 0
}

// ClassifyCompactSynthesis classifies one raw model response into the
// three outcomes. It is a pure function of the response text: no DB, no
// network, no clock.
//
// The returned lesson is non-nil only for OutcomeLesson. For
// OutcomeRefused it is the parsed sentinel, which callers must not
// persist. For OutcomeError it is nil.
//
// error is non-nil for exactly OutcomeError. A refusal returns a nil
// error — reporting it as a failure is the defect this file removes.
func ClassifyCompactSynthesis(raw string) (CompactOutcome, *CompactLesson, error) {
	// 1. Wire shape. Runs before the semantic checks so a `null`, `{}`,
	//    or object with undeclared field names is a schema violation
	//    rather than an accidental zero-value "refusal".
	if err := hasCompactLessonShape(raw); err != nil {
		return OutcomeError, nil, fmt.Errorf("model_schema_violation: %w", err)
	}

	// 2. Decode.
	var lesson CompactLesson
	if err := json.Unmarshal([]byte(raw), &lesson); err != nil {
		return OutcomeError, nil, fmt.Errorf("model_schema_violation: %w", err)
	}

	// 3. Semantic classification. This runs BEFORE Validate, and
	//    Validate is never asked about the sentinel. Order matters:
	//    Validate would report the sanctioned refusal as "title
	//    required", and any future change to Validate must not be able
	//    to alter refusal detection.
	if isRefusalSentinel(&lesson) {
		return OutcomeRefused, &lesson, nil
	}

	// Not the sentinel. From here on it is an ordinary candidate, and
	// an empty title is a malformed response rather than a refusal —
	// the model produced something, just not something usable.
	if err := lesson.Validate(); err != nil {
		return OutcomeError, nil, fmt.Errorf("lesson_validation_failed: %w", err)
	}

	return OutcomeLesson, &lesson, nil
}
