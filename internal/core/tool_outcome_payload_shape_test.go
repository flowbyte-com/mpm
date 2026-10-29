// tool_outcome_payload_shape_test.go — the payload-shape family is a
// CALLER mistake, and the classifier must say so (issue #159).
//
// WHY THIS FILE EXISTS
// --------------------
// The live-runtime audit (docs/dev/live-runtime-health-audit-2026-10-28.md,
// PR #157) found 45 of 105 error rows in the live dispatch audit recorded as
//
//	outcome_class = 'internal', outcome_code = 'unclassified'
//
// and every one of them was a caller sending the wrong payload shape:
//
//	commitments must be an array of strings, got map[string]interface {}
//	tags must be an array of strings, got string
//	field `limit` must be a number (float64/int), got string
//	payload has top-level field(s) [...] outside the params envelope
//	patch: must be a JSON object (got null/absent)
//	EndSessionV2: insert: CHECK constraint failed: ended_state IN (...)
//
// `internal` is the class that means "MPM could not attribute this". A class
// that is 43 % agent noise is a class in which a real substrate fault is
// invisible.
//
// WHAT IS PINNED HERE
// -------------------
//   1. Each shape family maps to a STABLE, MEANINGFUL outcome_code.
//   2. The classification comes from the TYPE, not from the message: the
//      same text delivered as a bare error still classifies as
//      internal/unclassified. This is the non-vacuity proof — a broader
//      substring rule would fail it.
//   3. Genuine substrate faults, schema defects and unexpected internal
//      errors keep their classes. The fix must not swallow them.
//   4. A CHECK constraint failure is reclassified as validation ONLY for the
//      one known, verified tool-boundary case (handoff `ended_state`). Every
//      other CHECK failure stays a system fault.
package internal

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ── 1. The shape families ──────────────────────────────────────────────────

func TestClassifyError_PayloadShapeFamiliesAreValidation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{
			name: "array",
			err:  NewPayloadShapeError(PayloadShapeArray, "tags", "tags must be an array of strings, got string"),
			code: "invalid_array_shape",
		},
		{
			name: "object",
			err:  NewPayloadShapeError(PayloadShapeObject, "patch", "patch: must be a JSON object (got null/absent)"),
			code: "invalid_object_shape",
		},
		{
			name: "numeric",
			err:  NewPayloadShapeError(PayloadShapeNumeric, "limit", "field `limit` must be a number (float64/int), got string"),
			code: "invalid_numeric_field",
		},
		{
			name: "envelope",
			err: NewPayloadShapeError(PayloadShapeEnvelope, "mpm_memory",
				`mpm_memory: payload has top-level field(s) [fact] outside the params envelope`),
			code: "invalid_params_envelope",
		},
		{
			name: "scalar type",
			err:  NewPayloadShapeError(PayloadShapeType, "projection", "projection must be a string, got float64"),
			code: "invalid_type",
		},
		{
			name: "enum",
			err:  NewPayloadShapeError(PayloadShapeEnum, "state", `field `+ "`state`" + ` must be one of [clean, crashed, interrupted, force_end], got "bogus"`),
			code: "invalid_enum_value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, code := ClassifyError(tc.err)
			if class != OutcomeClassValidation {
				t.Errorf("class = %q, want %q", class, OutcomeClassValidation)
			}
			if code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
		})
	}
}

// ── 2. Non-vacuity: the TYPE is what does the work ─────────────────────────

// TestClassifyError_PayloadShapeIsRecognisedByTypeNotByText is the
// non-vacuity proof for the whole fix.
//
// Every message below is byte-identical to the one the typed error carries.
// Delivered as a bare error, none of them matches any of the classifier's
// string cases, and all of them classify as internal/unclassified — which is
// exactly the defect.
//
// If someone "fixes" this by widening the substring rules, these assertions
// break, and the test says why: the rule would have to match on message text
// that a substrate error could also produce.
func TestClassifyError_PayloadShapeIsRecognisedByTypeNotByText(t *testing.T) {
	bare := []struct {
		name string
		msg  string
	}{
		{"array", "tags must be an array of strings, got string"},
		{"object", "patch: must be a JSON object (got null/absent)"},
		{"numeric", "field `limit` must be a number (float64/int), got string"},
		{"envelope", "mpm_memory: payload has top-level field(s) [fact] outside the params envelope"},
		{"type", "projection must be a string, got float64"},
		{"enum", `field ` + "`state`" + ` must be one of [clean, crashed, interrupted, force_end], got "bogus"`},
	}
	for _, tc := range bare {
		t.Run(tc.name, func(t *testing.T) {
			class, code := ClassifyError(errors.New(tc.msg))
			if class == OutcomeClassValidation {
				t.Errorf("a BARE error with this text classified as validation/%s — "+
					"the fix must come from the typed error, not from a substring rule "+
					"over message text (a substrate error could produce the same words)",
					code)
			}
			if class != OutcomeClassInternal || code != "unclassified" {
				t.Errorf("bare error classified as %s/%s, want internal/unclassified", class, code)
			}
		})
	}
}

// A handler may WRAP the typed error (several do: "reinforce: %w"). The
// classification must survive the wrap.
func TestClassifyError_PayloadShapeSurvivesWrapping(t *testing.T) {
	inner := NewPayloadShapeError(PayloadShapeNumeric, "weight",
		"field `weight` must be a number (float64/int), got string")
	wrapped := fmt.Errorf("reinforce: %w", inner)

	class, code := ClassifyError(wrapped)
	if class != OutcomeClassValidation {
		t.Errorf("class = %q, want %q", class, OutcomeClassValidation)
	}
	if code != "invalid_numeric_field" {
		t.Errorf("code = %q, want invalid_numeric_field", code)
	}
	// The message the caller sees is the handler's, not the wrapper's.
	if !strings.Contains(wrapped.Error(), "must be a number") {
		t.Errorf("wrapped message lost the caller-facing text: %q", wrapped.Error())
	}
}

// ── 3. Genuine faults keep their classes ──────────────────────────────────

// TestClassifyError_SubstrateFaultsAreNotReclassified is the guard on the
// other side of the fix. A malformed payload and a broken substrate are
// different things, and the class that says "system fault" has to keep
// meaning that.
func TestClassifyError_SubstrateFaultsAreNotReclassified(t *testing.T) {
	cases := []struct {
		name  string
		msg   string
		class ToolOutcomeClass
		code  string
	}{
		{"wal busy", "database is locked", OutcomeClassSubstrate, "wal_busy_timeout"},
		{"sqlite busy", "SQLITE_BUSY: database is locked", OutcomeClassSubstrate, "wal_busy_timeout"},
		{"missing table", "no such table: memories", OutcomeClassSubstrate, "substrate_schema"},
		{"missing column", "no such column: foo", OutcomeClassSubstrate, "substrate_schema"},
		{"integrity", "integrity check failed", OutcomeClassSubstrate, "integrity_check_failed"},
		{"not found", "memory not found", OutcomeClassNotFound, "artifact_not_found"},
		{"unexpected", "something nobody anticipated", OutcomeClassInternal, "unclassified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, code := ClassifyError(errors.New(tc.msg))
			if class != tc.class {
				t.Errorf("class = %q, want %q", class, tc.class)
			}
			if code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
		})
	}
}

// TestClassifyError_OnlyTheKnownCheckConstraintBecomesValidation pins the
// narrowness the issue demands: "Do not reclassify all SQL CHECK failures as
// caller mistakes; only known, verified tool-boundary validation cases
// belong there."
//
// The handoff `ended_state` CHECK is one such case, and it is a BACKSTOP —
// the write path now validates the enum before the write (see
// ValidateHandoffEndedState), so the CHECK is the last line of defence.
//
// Every other CHECK failure — a schema defect, an internal invariant break —
// must stay a system fault.
func TestClassifyError_OnlyTheKnownCheckConstraintBecomesValidation(t *testing.T) {
	// The known case.
	class, code := ClassifyError(errors.New(
		"EndSessionV2: insert: CHECK constraint failed: ended_state"))
	if class != OutcomeClassValidation || code != "invalid_enum_value" {
		t.Errorf("ended_state CHECK classified as %s/%s, want validation/invalid_enum_value", class, code)
	}

	// Every other CHECK failure stays internal.
	for _, msg := range []string{
		"insert into memories: CHECK constraint failed: weight",
		"insert into memories: CHECK constraint failed: is_prime_directive",
		"CHECK constraint failed: some_other_column",
	} {
		class, code := ClassifyError(errors.New(msg))
		if class == OutcomeClassValidation {
			t.Errorf("%q classified as validation/%s — only the verified ended_state "+
				"case may move; another CHECK failure can be a schema defect", msg, code)
		}
		if class != OutcomeClassInternal {
			t.Errorf("%q classified as %s, want internal", msg, class)
		}
	}
}

// ── 4. The outcome_code contract ───────────────────────────────────────────

// The outcome_code is a bounded identifier: no spaces, no quotes, no SQL, no
// caller content. A new code that breaks this breaks every dashboard that
// groups by it.
func TestPayloadShapeOutcomeCodesAreBoundedIdentifiers(t *testing.T) {
	for _, kind := range []PayloadShapeKind{
		PayloadShapeArray, PayloadShapeObject, PayloadShapeNumeric,
		PayloadShapeEnvelope, PayloadShapeType, PayloadShapeEnum,
	} {
		code := kind.OutcomeCode()
		if code == "" {
			t.Errorf("%s: empty outcome_code", kind)
			continue
		}
		for _, r := range code {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
			if !ok {
				t.Errorf("%s: outcome_code %q contains %q — codes are lowercase ASCII identifiers", kind, code, r)
			}
		}
		if strings.Contains(code, "://") || strings.Contains(code, " ") {
			t.Errorf("%s: outcome_code %q must not contain spaces or URLs", kind, code)
		}
	}
}

// The classification must remain inside the closed class vocabulary — a new
// class is a schema change, and this fix is not one.
func TestPayloadShapeClassificationUsesAKnownClass(t *testing.T) {
	err := NewPayloadShapeError(PayloadShapeArray, "tags", "tags must be an array of strings, got string")
	class, _ := ClassifyError(err)
	if !ValidClass(class) {
		t.Errorf("class %q is not in the closed vocabulary", class)
	}
}

// ── 5. The handoff ended_state boundary ───────────────────────────────────

// The four permitted values are the table's CHECK constraint, the tool
// schema's enum, and the boundary validation — one list, three places.
func TestValidateHandoffEndedState_NamesTheFourPermittedValues(t *testing.T) {
	if len(HandoffEndedStates) != 4 {
		t.Fatalf("HandoffEndedStates has %d entries, want 4: %v", len(HandoffEndedStates), HandoffEndedStates)
	}
	for _, allowed := range []string{HandoffClean, HandoffCrashed, HandoffInterrupted, HandoffForceEnd} {
		if err := ValidateHandoffEndedState(allowed); err != nil {
			t.Errorf("ValidateHandoffEndedState(%q) = %v, want nil", allowed, err)
		}
	}

	err := ValidateHandoffEndedState("bogus")
	if err == nil {
		t.Fatal("an out-of-enum ended_state was accepted")
	}
	for _, allowed := range HandoffEndedStates {
		if !strings.Contains(err.Error(), allowed) {
			t.Errorf("error %q does not name the permitted value %q", err.Error(), allowed)
		}
	}

	// The error is typed, so it classifies as validation.
	class, code := ClassifyError(err)
	if class != OutcomeClassValidation {
		t.Errorf("class = %q, want %q", class, OutcomeClassValidation)
	}
	if code != "invalid_enum_value" {
		t.Errorf("code = %q, want invalid_enum_value", code)
	}
}
