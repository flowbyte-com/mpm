// d7_string_slice_parsing_test.go — regression coverage for M5 defect D7:
// `internal.ParseStringSliceOr` silently discarded valid Go-native
// `[]string` input.
//
// # THE DEFECT
//
// `parseStringSliceAny` accepted two shapes: a JSON-decoded `[]interface{}`
// of strings, and a comma-separated string. A Go-native `[]string` fell
// through to the nil return, so every identifier a caller supplied in that
// shape DISAPPEARED — without an error, and without any indication that
// the list had been lost.
//
// The consequence was a data-integrity failure, not a convenience bug: a
// theory proposal could be created with NO citations even though its author
// had declared them, and the author was told it succeeded.
//
// These tests use no database: the parser is a pure function. The
// handler-level and end-to-end coverage is in
// internal/core/tools/d7_string_slice_tools_test.go.
package internal

import (
	"reflect"
	"testing"
)

// ── 1. Native []string ──────────────────────────────────────────────────

// TestD7_ParseStringSliceOr_NativeStringSlice pins requirement 1.
//
// A Go-native string slice is a COMPLETE representation of the parameter,
// exactly like the JSON-decoded array form. It is accepted, and it is
// copied: the caller keeps ownership of the slice it handed over.
func TestD7_ParseStringSliceOr_NativeStringSlice(t *testing.T) {
	in := []string{"mem-1", "mem-2"}
	got := ParseStringSliceOr(in)
	if !reflect.DeepEqual(got, []string{"mem-1", "mem-2"}) {
		t.Fatalf("ParseStringSliceOr([]string) = %#v, want [mem-1 mem-2]", got)
	}

	// The copy must not alias the caller's slice.
	in[0] = "mutated"
	if got[0] != "mem-1" {
		t.Errorf("the result aliases the caller's slice: got %q after the caller mutated it", got[0])
	}
}

// ── 2. JSON-decoded arrays ──────────────────────────────────────────────

// TestD7_ParseStringSliceOr_JSONDecodedArrays pins requirement 2: the
// existing JSON-decoded behaviour is preserved exactly.
func TestD7_ParseStringSliceOr_JSONDecodedArrays(t *testing.T) {
	got := ParseStringSliceOr([]interface{}{"a", "b"})
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ParseStringSliceOr([]interface{}) = %#v, want [a b]", got)
	}

	// A JSON-ENCODED array string (the shape an MCP framework produces
	// when it coerces an array argument to a string).
	got = ParseStringSliceOr(`["a","b"]`)
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ParseStringSliceOr(json array string) = %#v, want [a b]", got)
	}

	// The compact comma-separated form.
	got = ParseStringSliceOr("a,b")
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ParseStringSliceOr(csv) = %#v, want [a b]", got)
	}
}

// ── 3. Empty arrays and nil ─────────────────────────────────────────────

// TestD7_ParseStringSliceOr_EmptyArraysAndNil pins requirement 3.
//
// The two representations must agree: an empty JSON array and an empty
// native slice are both an empty list; a nil native slice and an absent
// parameter are both "no value".
func TestD7_ParseStringSliceOr_EmptyArraysAndNil(t *testing.T) {
	if got := ParseStringSliceOr(nil); got != nil {
		t.Errorf("ParseStringSliceOr(nil) = %#v, want nil", got)
	}
	if got := ParseStringSliceOr([]string(nil)); got != nil {
		t.Errorf("ParseStringSliceOr([]string(nil)) = %#v, want nil", got)
	}
	if got := ParseStringSliceOr(""); got != nil {
		t.Errorf("ParseStringSliceOr(\"\") = %#v, want nil", got)
	}

	empty := ParseStringSliceOr([]string{})
	if empty == nil {
		t.Errorf("ParseStringSliceOr([]string{}) = nil, want an empty (non-nil) list")
	}
	if len(empty) != 0 {
		t.Errorf("ParseStringSliceOr([]string{}) = %#v, want an empty list", empty)
	}

	// And the JSON form agrees with it.
	jsonEmpty := ParseStringSliceOr([]interface{}{})
	if jsonEmpty == nil {
		t.Errorf("ParseStringSliceOr([]interface{}{}) = nil, want an empty (non-nil) list")
	}
	if len(jsonEmpty) != 0 {
		t.Errorf("ParseStringSliceOr([]interface{}{}) = %#v, want an empty list", jsonEmpty)
	}
}

// ── 4. Missing optional parameters ──────────────────────────────────────

// TestD7_ParseStringSliceOr_MissingOptionalParameter pins requirement 4.
//
// A missing key is a nil value, and a nil value is "no list" — the
// caller's local default applies.
func TestD7_ParseStringSliceOr_MissingOptionalParameter(t *testing.T) {
	p := map[string]interface{}{"other": "x"}
	got := ParseStringSliceOr(p["tags"])
	if got != nil {
		t.Errorf("ParseStringSliceOr(missing key) = %#v, want nil", got)
	}
}

// ── 5. Malformed and mixed-type arrays ──────────────────────────────────

// TestD7_ParseStringSliceOr_MalformedAndMixedType pins requirement 5 for
// the LENIENT form.
//
// The lenient form keeps its documented fallback semantics: a non-string
// element is DROPPED, not rejected. That is what the callers that want a
// fallback ask for, and the strict form exists for the callers that do
// not. The point of this test is that the fallback is now a DELIBERATE,
// documented shape rather than an accident.
func TestD7_ParseStringSliceOr_MalformedAndMixedType(t *testing.T) {
	got := ParseStringSliceOr([]interface{}{"a", 42, "b"})
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ParseStringSliceOr(mixed array) = %#v, want [a b] (the lenient form drops the malformed element)", got)
	}

	// A wholly non-array value is "no list".
	if got := ParseStringSliceOr(42); got != nil {
		t.Errorf("ParseStringSliceOr(42) = %#v, want nil", got)
	}
	if got := ParseStringSliceOr(map[string]interface{}{"a": 1}); got != nil {
		t.Errorf("ParseStringSliceOr(map) = %#v, want nil", got)
	}
}

// ── 6. Correct fallback semantics ───────────────────────────────────────

// TestD7_ParseStringSliceStrict_WholeList pins requirement 6: the strict
// form's fallback semantics.
//
// The strict form accepts every shape the lenient form accepts — so no
// working caller loses a shape — and rejects exactly the cases where the
// lenient form would LOSE an element.
func TestD7_ParseStringSliceStrict_WholeList(t *testing.T) {
	// Absent → an empty list, no error.
	got, err := ParseStringSliceStrict("source_ids", nil)
	if err != nil {
		t.Fatalf("ParseStringSliceStrict(nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("ParseStringSliceStrict(nil) = %#v, want an empty list", got)
	}

	// Native slice → accepted whole.
	got, err = ParseStringSliceStrict("source_ids", []string{"a", "b"})
	if err != nil {
		t.Fatalf("ParseStringSliceStrict([]string): %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ParseStringSliceStrict([]string) = %#v, want [a b]", got)
	}

	// JSON array → accepted whole.
	got, err = ParseStringSliceStrict("source_ids", []interface{}{"a", "b"})
	if err != nil {
		t.Fatalf("ParseStringSliceStrict([]interface{}): %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ParseStringSliceStrict([]interface{}) = %#v, want [a b]", got)
	}

	// A JSON-encoded array string and the compact CSV form are whole
	// parses too — no element is lost, so they are accepted.
	got, err = ParseStringSliceStrict("source_ids", `["a","b"]`)
	if err != nil {
		t.Fatalf("ParseStringSliceStrict(json array string): %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ParseStringSliceStrict(json array string) = %#v, want [a b]", got)
	}
	got, err = ParseStringSliceStrict("source_ids", "a,b")
	if err != nil {
		t.Fatalf("ParseStringSliceStrict(csv): %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ParseStringSliceStrict(csv) = %#v, want [a b]", got)
	}
}

// TestD7_ParseStringSliceStrict_RejectsLossyInput pins the difference
// between the two forms.
//
// A malformed ELEMENT is an error, not a drop: a list accepted in part is
// a list the caller did not ask for.
func TestD7_ParseStringSliceStrict_RejectsLossyInput(t *testing.T) {
	// A mixed-type array.
	_, err := ParseStringSliceStrict("source_ids", []interface{}{"a", 42, "b"})
	if err == nil {
		t.Fatalf("ParseStringSliceStrict(mixed array) returned no error — a partially accepted list must be rejected")
	}
	if got, want := err.Error(), "source_ids[1] must be a string, got int"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}

	// A wholly non-list value. The lenient form read it as "no value at
	// all" — which is how a caller's citations vanished.
	for _, v := range []interface{}{42, true, map[string]interface{}{"a": 1}} {
		if _, err := ParseStringSliceStrict("source_ids", v); err == nil {
			t.Errorf("ParseStringSliceStrict(%#v) returned no error", v)
		}
	}
}

// TestD7_ParseStringSliceArrayOnly_SchemaContract pins the ARRAY-ONLY
// strict form — the contract for a field the schema declares as an array.
//
// It is deliberately stricter than ParseStringSliceStrict about a SCALAR:
// a declared array field given a scalar is a caller mistake worth
// reporting, not a shape to guess at. The error strings are the ones the
// boundary already used, unchanged.
func TestD7_ParseStringSliceArrayOnly_SchemaContract(t *testing.T) {
	// Absent → an empty list.
	got, err := ParseStringSliceArrayOnly("tags", nil)
	if err != nil {
		t.Fatalf("ParseStringSliceArrayOnly(nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("ParseStringSliceArrayOnly(nil) = %#v, want an empty list", got)
	}

	// A native slice → accepted (D7: it used to be rejected).
	got, err = ParseStringSliceArrayOnly("tags", []string{"a", "b"})
	if err != nil {
		t.Fatalf("ParseStringSliceArrayOnly([]string): %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ParseStringSliceArrayOnly([]string) = %#v, want [a b]", got)
	}

	// A JSON array → accepted.
	got, err = ParseStringSliceArrayOnly("tags", []interface{}{"a"})
	if err != nil {
		t.Fatalf("ParseStringSliceArrayOnly([]interface{}): %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("ParseStringSliceArrayOnly([]interface{}) = %#v, want [a]", got)
	}

	// A scalar → rejected, with the ORIGINAL error string.
	_, err = ParseStringSliceArrayOnly("tags", "a,b")
	if err == nil {
		t.Fatalf("ParseStringSliceArrayOnly(scalar) returned no error")
	}
	if got, want := err.Error(), "tags must be an array of strings, got string"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}

	// A malformed element → rejected, with the ORIGINAL error string.
	_, err = ParseStringSliceArrayOnly("tags", []interface{}{"a", 42})
	if err == nil {
		t.Fatalf("ParseStringSliceArrayOnly(mixed array) returned no error")
	}
	if got, want := err.Error(), "tags[1] must be a string, got int"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

// ── 7 & 8. Complete preservation; no partial list ───────────────────────

// TestD7_CitationListIsAcceptedWholeOrNotAtAll pins requirements 7 and 8
// at the parser level.
//
// A citation list is either represented exactly or rejected. There is no
// third outcome in which a caller supplies three identifiers and two are
// recorded.
func TestD7_CitationListIsAcceptedWholeOrNotAtAll(t *testing.T) {
	supplied := []string{"mem-1", "mem-2", "mem-3"}

	// The native form: all three survive.
	got, err := ParseStringSliceStrict("source_ids", supplied)
	if err != nil {
		t.Fatalf("ParseStringSliceStrict: %v", err)
	}
	if !reflect.DeepEqual(got, supplied) {
		t.Errorf("parsed = %#v, want %#v — a citation list must be preserved whole", got, supplied)
	}

	// The JSON form: all three survive, identically.
	got, err = ParseStringSliceStrict("source_ids", []interface{}{"mem-1", "mem-2", "mem-3"})
	if err != nil {
		t.Fatalf("ParseStringSliceStrict: %v", err)
	}
	if !reflect.DeepEqual(got, supplied) {
		t.Errorf("parsed = %#v, want %#v — the two representations must agree", got, supplied)
	}

	// The partially-valid form: rejected outright, so no shorter list is
	// ever produced.
	if _, err := ParseStringSliceStrict("source_ids", []interface{}{"mem-1", 42, "mem-3"}); err == nil {
		t.Errorf("a partially valid citation list was accepted — it must be rejected")
	}
}
