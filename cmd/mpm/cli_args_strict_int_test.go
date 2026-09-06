// cli_args_strict_int_test.go — unit tests for the strict bounded-
// integer CLI parser.
//
// Stage S3 of the approved CLI refactor. Covers the canonical contract
// in cli_args_strict_int.go.
package main

import (
	"strings"
	"testing"
)

// TestParseBoundedInt_MinValid covers the minimum valid value.
func TestParseBoundedInt_MinValid(t *testing.T) {
	got, err := parseBoundedInt("1", "limit", 1, 10000)
	if err != nil {
		t.Errorf("err: got %v, want nil", err)
	}
	if got != 1 {
		t.Errorf("got %d, want 1", got)
	}
}

// TestParseBoundedInt_MaxValid covers the maximum valid value.
func TestParseBoundedInt_MaxValid(t *testing.T) {
	got, err := parseBoundedInt("10000", "limit", 1, 10000)
	if err != nil {
		t.Errorf("err: got %v, want nil", err)
	}
	if got != 10000 {
		t.Errorf("got %d, want 10000", got)
	}
}

// TestParseBoundedInt_InteriorValue covers a value between bounds.
func TestParseBoundedInt_InteriorValue(t *testing.T) {
	got, err := parseBoundedInt("42", "limit", 1, 10000)
	if err != nil {
		t.Errorf("err: got %v, want nil", err)
	}
	if got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

// TestParseBoundedInt_ZeroWhenPermitted covers 0 as a valid value when
// the lower bound is <= 0. This is the canonical "0 is meaningful,
// not a sentinel" test from the plan.
func TestParseBoundedInt_ZeroWhenPermitted(t *testing.T) {
	got, err := parseBoundedInt("0", "weight", 0, 100)
	if err != nil {
		t.Errorf("err: got %v, want nil (0 is valid when lo=0)", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

// TestParseBoundedInt_ZeroBelowLo covers 0 as invalid when lo > 0.
// 0 must NOT silently fall through to a default.
func TestParseBoundedInt_ZeroBelowLo(t *testing.T) {
	got, err := parseBoundedInt("0", "limit", 1, 10000)
	if err == nil {
		t.Errorf("err = nil, want error (0 is below lo=1)")
	}
	if got != 0 {
		t.Errorf("got %d, want 0 (zero value on error)", got)
	}
	if !strings.Contains(err.Error(), "0") || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error message %q must mention the value and 'out of range'", err.Error())
	}
}

// TestParseBoundedInt_NegativeRejected covers negative values below lo.
func TestParseBoundedInt_NegativeRejected(t *testing.T) {
	got, err := parseBoundedInt("-1", "limit", 1, 10000)
	if err == nil {
		t.Errorf("err = nil, want error (-1 is below lo=1)")
	}
	if got != 0 {
		t.Errorf("got %d, want 0 (zero value on error)", got)
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error message %q must mention 'out of range'", err.Error())
	}
}

// TestParseBoundedInt_EmptyRejected covers empty input. Per the plan,
// empty is rejected so the caller cannot collapse omitted-vs-explicit.
func TestParseBoundedInt_EmptyRejected(t *testing.T) {
	got, err := parseBoundedInt("", "limit", 1, 10000)
	if err == nil {
		t.Errorf("err = nil, want error (empty is rejected)")
	}
	if got != 0 {
		t.Errorf("got %d, want 0 (zero value on error)", got)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error message %q must mention 'empty'", err.Error())
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error message %q must mention the field name", err.Error())
	}
}

// TestParseBoundedInt_MalformedRejected covers non-integer text. abc,
// "1.5", "1e2" must all error — silent truncation/default is the bug
// class S3 is fixing.
func TestParseBoundedInt_MalformedRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"alpha", "abc"},
		{"decimal", "1.5"},
		{"scientific", "1e2"},
		{"hex", "0x5"},
		{"zero-decimal", "0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseBoundedInt(tc.in, "limit", 1, 10000)
			if err == nil {
				t.Errorf("err = nil for %q, want error", tc.in)
			}
			if !strings.Contains(err.Error(), "invalid integer") {
				t.Errorf("error %q must mention 'invalid integer'", err.Error())
			}
		})
	}
}

// TestParseBoundedInt_WhitespaceRejected covers leading/trailing
// whitespace. Per the plan: "Do not silently truncate: 1.5 → 1".
// Likewise, do not silently trim.
func TestParseBoundedInt_WhitespaceRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"leading-space", " 5"},
		{"trailing-space", "5 "},
		{"both", " 5 "},
		{"tab-leading", "\t5"},
		{"newline-trailing", "5\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseBoundedInt(tc.in, "limit", 1, 10000)
			if err == nil {
				t.Errorf("err = nil for %q, want error (whitespace rejected)", tc.in)
			}
		})
	}
}

// TestParseBoundedInt_AboveHiRejected covers values above hi.
func TestParseBoundedInt_AboveHiRejected(t *testing.T) {
	_, err := parseBoundedInt("10001", "limit", 1, 10000)
	if err == nil {
		t.Errorf("err = nil, want error (10001 is above hi=10000)")
	}
	if !strings.Contains(err.Error(), "10001") || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error %q must mention the value and 'out of range'", err.Error())
	}
}

// TestParseBoundedInt_IntOverflow covers values that exceed int range.
// strconv.Atoi returns *strconv.NumError for this; the helper must
// surface it as a deterministic "invalid integer" error, NOT panic
// or silently coerce.
func TestParseBoundedInt_IntOverflow(t *testing.T) {
	// 9223372036854775808 = MaxInt64 + 1; Atoi fails with range error.
	_, err := parseBoundedInt("9223372036854775808", "limit", 1, 10000)
	if err == nil {
		t.Errorf("err = nil, want error (overflow)")
	}
	if !strings.Contains(err.Error(), "invalid integer") {
		t.Errorf("error %q must mention 'invalid integer'", err.Error())
	}
}

// TestParseBoundedInt_BoundZero covers the case where lo == hi == 0.
// Only the value 0 is accepted.
func TestParseBoundedInt_BoundZero(t *testing.T) {
	got, err := parseBoundedInt("0", "x", 0, 0)
	if err != nil {
		t.Errorf("err: got %v, want nil", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
	_, err = parseBoundedInt("1", "x", 0, 0)
	if err == nil {
		t.Errorf("err = nil, want error (1 is above hi=0)")
	}
}

// TestParseBoundedInt_NegativeRange covers bounds that include
// negative values, e.g. lo=-100.
func TestParseBoundedInt_NegativeRange(t *testing.T) {
	got, err := parseBoundedInt("-50", "offset", -100, 100)
	if err != nil {
		t.Errorf("err: got %v, want nil", err)
	}
	if got != -50 {
		t.Errorf("got %d, want -50", got)
	}
}

// TestParseBoundedInt_FieldNameInError covers that the field name
// appears in the error message for all rejection paths.
func TestParseBoundedInt_FieldNameInError(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"malformed", "abc"},
		{"below", "-1"},
		{"above", "99999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseBoundedInt(tc.input, "myfield", 1, 100)
			if err == nil {
				t.Fatalf("err = nil for %q", tc.input)
			}
			if !strings.Contains(err.Error(), "myfield") {
				t.Errorf("error %q must mention field name 'myfield'", err.Error())
			}
		})
	}
}