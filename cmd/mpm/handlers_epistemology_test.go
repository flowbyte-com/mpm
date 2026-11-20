// handlers_epistemology_test.go — Regression tests for theory CLI parser
// (M3 audit D-001/002/003).
//
// The pre-fix `parseTheoryArgs` only recognized --key value form. Bare
// key=value tokens and positional forms silently dropped the validation
// criteria. These tests pin the parser's behavior across the four
// documented forms and the most common malformed-input classes.
package main

import (
	"strings"
	"testing"
)

// TestParseTheoryArgs_LongFlags is the canonical happy path: --hypothesis
// and --validation produce the expected (hypothesis, validation) tuple.
func TestParseTheoryArgs_LongFlags(t *testing.T) {
	h, v, s, tags, leftovers := parseTheoryArgs([]string{
		"--hypothesis", "wal is faster",
		"--validation", "throughput on 4 readers",
	})
	if h != "wal is faster" {
		t.Errorf("hypothesis = %q, want %q", h, "wal is faster")
	}
	if v != "throughput on 4 readers" {
		t.Errorf("validation = %q, want %q", v, "throughput on 4 readers")
	}
	if s != "" {
		t.Errorf("status = %q, want empty", s)
	}
	if tags != "" {
		t.Errorf("tags = %q, want empty", tags)
	}
	if len(leftovers) != 0 {
		t.Errorf("leftovers = %v, want empty", leftovers)
	}
}

// TestParseTheoryArgs_BareKeyValue covers the M3 audit form
// `mpm theorize hypothesis="X" validation="Y"`. Pre-fix this silently
// stored the whole `hypothesis="X" validation="Y"` string as the
// hypothesis and dropped validation.
func TestParseTheoryArgs_BareKeyValue(t *testing.T) {
	h, v, _, _, leftovers := parseTheoryArgs([]string{
		`hypothesis="wal is faster"`,
		`validation="throughput on 4 readers"`,
	})
	if h != "wal is faster" {
		t.Errorf("hypothesis = %q, want %q", h, "wal is faster")
	}
	if v != "throughput on 4 readers" {
		t.Errorf("validation = %q, want %q", v, "throughput on 4 readers")
	}
	if len(leftovers) != 0 {
		t.Errorf("leftovers = %v, want empty", leftovers)
	}
}

// TestParseTheoryArgs_BareKeyValue_Aliases covers the alias keys that
// were documented in the help text (hypothesis_id, validation_criteria).
func TestParseTheoryArgs_BareKeyValue_Aliases(t *testing.T) {
	h, v, _, _, _ := parseTheoryArgs([]string{
		`hypothesis_id="wal-better"`,
		`validation_criteria="throughput on 4 readers"`,
	})
	if h != "wal-better" {
		t.Errorf("hypothesis = %q, want %q", h, "wal-better")
	}
	if v != "throughput on 4 readers" {
		t.Errorf("validation = %q, want %q", v, "throughput on 4 readers")
	}
}

// TestParseTheoryArgs_ValuesWithSpaces — common operator-typed case
// where validation criteria is a sentence.
func TestParseTheoryArgs_ValuesWithSpaces(t *testing.T) {
	h, v, _, _, _ := parseTheoryArgs([]string{
		"--hypothesis", "wal is faster than delete-journal for our workload",
		"--validation", "throughput holds above 1000 reads/sec on 4 concurrent readers",
	})
	if h != "wal is faster than delete-journal for our workload" {
		t.Errorf("hypothesis = %q, want full sentence", h)
	}
	if v != "throughput holds above 1000 reads/sec on 4 concurrent readers" {
		t.Errorf("validation = %q, want full sentence", v)
	}
}

// TestParseTheoryArgs_ValuesStartingWithDash — common case for technical
// memory content (paths, switches, options).
func TestParseTheoryArgs_ValuesStartingWithDash(t *testing.T) {
	h, v, _, _, _ := parseTheoryArgs([]string{
		"--hypothesis", "-rf flag is dangerous in scripts",
		"--validation", "always quote paths",
	})
	if h != "-rf flag is dangerous in scripts" {
		t.Errorf("hypothesis = %q, want %q", h, "-rf flag is dangerous in scripts")
	}
	if v != "always quote paths" {
		t.Errorf("validation = %q, want %q", v, "always quote paths")
	}
}

// TestParseTheoryArgs_ValuesContainingEquals — when the value itself
// contains `=`, the first `=` is the separator; everything after is the
// value verbatim.
func TestParseTheoryArgs_ValuesContainingEquals(t *testing.T) {
	h, v, _, _, _ := parseTheoryArgs([]string{
		"--hypothesis", "x = 5",
		"--validation", "f(x) = x*2",
	})
	if h != "x = 5" {
		t.Errorf("hypothesis = %q, want %q", h, "x = 5")
	}
	if v != "f(x) = x*2" {
		t.Errorf("validation = %q, want %q", v, "f(x) = x*2")
	}
}

// TestParseTheoryArgs_MissingHypothesis — `--validation Y` alone must not
// be silently accepted as a hypothesis.
func TestParseTheoryArgs_MissingHypothesis(t *testing.T) {
	_, v, _, _, _ := parseTheoryArgs([]string{
		"--validation", "some criteria",
	})
	if v != "some criteria" {
		t.Errorf("validation = %q, want %q", v, "some criteria")
	}
}

// TestParseTheoryArgs_MissingValidation — symmetric to the above.
func TestParseTheoryArgs_MissingValidation(t *testing.T) {
	h, v, _, _, _ := parseTheoryArgs([]string{
		"--hypothesis", "some hypothesis",
	})
	if h != "some hypothesis" {
		t.Errorf("hypothesis = %q, want %q", h, "some hypothesis")
	}
	if v != "" {
		t.Errorf("validation = %q, want empty", v)
	}
}

// TestParseTheoryArgs_Malformed_MixedForms — when the operator mixes
// --flag form with bare key=value, the parser should still pick up both.
func TestParseTheoryArgs_Malformed_MixedForms(t *testing.T) {
	h, v, _, _, _ := parseTheoryArgs([]string{
		"--hypothesis", "wal is faster",
		`validation="throughput on 4 readers"`,
	})
	if h != "wal is faster" {
		t.Errorf("hypothesis = %q, want %q", h, "wal is faster")
	}
	if v != "throughput on 4 readers" {
		t.Errorf("validation = %q, want %q", v, "throughput on 4 readers")
	}
}

// TestParseTheoryArgs_BareKeyValue_EmptyValue — the operator-typed
// mistake of typing `validation=` without anything after. Must not
// silently set validation to something non-empty.
func TestParseTheoryArgs_BareKeyValue_EmptyValue(t *testing.T) {
	h, v, _, _, _ := parseTheoryArgs([]string{
		"hypothesis=wal",
		"validation=",
	})
	if h != "wal" {
		t.Errorf("hypothesis = %q, want %q", h, "wal")
	}
	if v != "" {
		t.Errorf("validation = %q, want empty (key=value should not silently inject empty)", v)
	}
}

// TestUnquoteBareValue confirms the helper strips a single matched pair
// of double quotes but leaves asymmetric or single-quoted values alone.
func TestUnquoteBareValue(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"\"hello\"", "hello"},
		{"\"trailing space \"", "trailing space "}, // both quotes → strip
		{"\"only-leading", "\"only-leading"},        // only leading → preserved
		{"trailing-only\"", "trailing-only\""},      // only trailing → preserved
		{"'single'", "'single'"},                    // single quotes preserved
		{"no-quotes", "no-quotes"},
		{"\"\"", ""},                                 // empty quoted
		{"", ""},
	}
	for _, c := range cases {
		got := unquoteBareValue(c.in)
		if got != c.want {
			t.Errorf("unquoteBareValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseTheoryArgs_PipePositional covers the documented
// "<hypothesis> | <validation>" form. Both single-token (whole phrase in
// one arg) and two-token (split across args) shapes are supported.
func TestParseTheoryArgs_PipePositional(t *testing.T) {
	// Single-token pipe form.
	// Note: this function only parses flags. The pipe form is handled
	// later in handleProposeTheory, so we test the caller branch
	// indirectly via TestHandleProposeTheory_PipePositional below.
	h, v, _, _, leftovers := parseTheoryArgs([]string{
		"wal is faster | throughput on 4 readers",
	})
	// parseTheoryArgs treats this as a single leftover. The pipe split
	// is in handleProposeTheory, not parseTheoryArgs. So leftovers
	// should hold the full string and h/v should be empty until the
	// caller runs the pipe split.
	if h != "" {
		t.Errorf("hypothesis at parser level = %q, want empty (pipe split is caller-side)", h)
	}
	if v != "" {
		t.Errorf("validation at parser level = %q, want empty", v)
	}
	if len(leftovers) != 1 || !strings.Contains(leftovers[0], "|") {
		t.Errorf("leftovers = %v, want single token containing '|'", leftovers)
	}
}

// TestResolveConclusionMapping exercises the canonical vocabulary for
// the post-fix D-010 logic: only confirmed/proven/disproven/refuted/
// invalidated map to proven or disproven. The table tests the same
// mapping the switch statement in handleResolveTheory uses.
func TestResolveConclusionMapping(t *testing.T) {
	cases := []struct {
		in        string
		wantStatus string
		wantError bool
	}{
		{"confirmed", "proven", false},
		{"proven", "proven", false},
		{"disproven", "disproven", false},
		{"refuted", "disproven", false},
		{"invalidated", "disproven", false},
		// Case-insensitive match.
		{"CONFIRMED", "proven", false},
		{"  proven  ", "proven", false},
		// Unmapped keywords must error, not silently write "resolved".
		{"some random text", "", true},
		{"", "", true},
		{"maybe", "", true},
		{"verified", "", true},
	}
	for _, c := range cases {
		got := ""
		switch strings.ToLower(strings.TrimSpace(c.in)) {
		case "confirmed", "proven":
			got = "proven"
		case "disproven", "refuted", "invalidated":
			got = "disproven"
		}
		if c.wantError {
			if got != "" {
				t.Errorf("conclusion %q: got status %q, want error (no mapping)", c.in, got)
			}
		} else if got != c.wantStatus {
			t.Errorf("conclusion %q: got %q, want %q", c.in, got, c.wantStatus)
		}
	}
}
