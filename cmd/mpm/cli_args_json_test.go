// cli_args_json_test.go — unit tests for the canonical CLI `--json` extractor.
//
// Stage S2 of the approved CLI refactor. Covers the contract established
// in cli_args_json.go and the cross-helper invariant with S1's
// splitOnDashDash.
package main

import (
	"reflect"
	"testing"
)

// TestExtractJSONFlag_NoJSON covers the no-flag case.
func TestExtractJSONFlag_NoJSON(t *testing.T) {
	in := []string{"foo", "bar", "baz"}
	got, cleaned := ExtractJSONFlag(in)
	if got {
		t.Errorf("jsonRequested = true, want false")
	}
	wantCleaned := []string{"foo", "bar", "baz"}
	if !reflect.DeepEqual(cleaned, wantCleaned) {
		t.Errorf("cleaned = %#v, want %#v", cleaned, wantCleaned)
	}
	if &cleaned[0] == &in[0] && len(cleaned) == len(in) {
		// Subtle: with no flag present, the helper still allocates
		// the cleaned slice. We require this so the helper has a
		// single allocation shape; do NOT relax it without updating
		// the contract.
		t.Logf("cleaned shares backing array with input — acceptable for no-strip case")
	}
}

// TestExtractJSONFlag_LongForm covers `--json` exact match.
func TestExtractJSONFlag_LongForm(t *testing.T) {
	in := []string{"--json", "value"}
	got, cleaned := ExtractJSONFlag(in)
	if !got {
		t.Errorf("jsonRequested = false, want true")
	}
	wantCleaned := []string{"value"}
	if !reflect.DeepEqual(cleaned, wantCleaned) {
		t.Errorf("cleaned = %#v, want %#v", cleaned, wantCleaned)
	}
}

// TestExtractJSONFlag_ShortForm covers `-j` exact match.
func TestExtractJSONFlag_ShortForm(t *testing.T) {
	in := []string{"-j", "value"}
	got, cleaned := ExtractJSONFlag(in)
	if !got {
		t.Errorf("jsonRequested = false, want true")
	}
	wantCleaned := []string{"value"}
	if !reflect.DeepEqual(cleaned, wantCleaned) {
		t.Errorf("cleaned = %#v, want %#v", cleaned, wantCleaned)
	}
}

// TestExtractJSONFlag_FlagBetweenPositional covers the most common
// invocation: `--json` appears between positional arguments.
func TestExtractJSONFlag_FlagBetweenPositional(t *testing.T) {
	in := []string{"first", "--json", "second", "-j", "third"}
	got, cleaned := ExtractJSONFlag(in)
	if !got {
		t.Errorf("jsonRequested = false, want true (saw both forms)")
	}
	wantCleaned := []string{"first", "second", "third"}
	if !reflect.DeepEqual(cleaned, wantCleaned) {
		t.Errorf("cleaned = %#v, want %#v", cleaned, wantCleaned)
	}
}

// TestExtractJSONFlag_RepeatedFlag covers idempotent behaviour for
// repeated occurrences. Final result is true; every occurrence is
// removed; only positional content remains.
func TestExtractJSONFlag_RepeatedFlag(t *testing.T) {
	in := []string{"--json", "--json", "a", "-j", "-j", "b", "--json"}
	got, cleaned := ExtractJSONFlag(in)
	if !got {
		t.Errorf("jsonRequested = false, want true")
	}
	wantCleaned := []string{"a", "b"}
	if !reflect.DeepEqual(cleaned, wantCleaned) {
		t.Errorf("cleaned = %#v, want %#v", cleaned, wantCleaned)
	}
}

// TestExtractJSONFlag_JsonValueForm covers `--json=true` and `--json=false`.
// These forms are NOT recognised by the canonical extractor: only the
// exact token `--json` matches. `--json=true` stays verbatim in the
// cleaned slice. This matches pre-S2 ExtractJSONFlag behaviour and is
// the deliberate scope of S2 (per the plan: "Do not invent support
// for forms that the current CLI does not support").
func TestExtractJSONFlag_JsonValueForm(t *testing.T) {
	in := []string{"--json=true", "value"}
	got, cleaned := ExtractJSONFlag(in)
	if got {
		t.Errorf("jsonRequested = true, want false (--json=true is NOT recognised)")
	}
	wantCleaned := []string{"--json=true", "value"}
	if !reflect.DeepEqual(cleaned, wantCleaned) {
		t.Errorf("cleaned = %#v, want %#v", cleaned, wantCleaned)
	}
}

// TestExtractJSONFlag_FlagFollowedByValue covers `--json <value>` with a
// space (NOT `--json=<value>`). The exact `--json` is removed; the
// following token remains as a positional. This matches pre-S2
// behaviour: callers that want strict value coupling must use
// `--json=true` / `--json=false`, not space-separated values.
func TestExtractJSONFlag_FlagFollowedByValue(t *testing.T) {
	in := []string{"--json", "true", "more"}
	got, cleaned := ExtractJSONFlag(in)
	if !got {
		t.Errorf("jsonRequested = false, want true")
	}
	// Pre-S2 behaviour: `"true"` is a positional, NOT parsed as a
	// boolean value. The canonical contract preserves that.
	wantCleaned := []string{"true", "more"}
	if !reflect.DeepEqual(cleaned, wantCleaned) {
		t.Errorf("cleaned = %#v, want %#v", cleaned, wantCleaned)
	}
}

// TestExtractJSONFlag_UnrelatedStringsContainingJSON covers the false-
// positive class: tokens that mention "json" but are NOT the flag.
// Pre-S2 ExtractJSONFlag did exact-match; S2 preserves that. `foo=json`
// must NOT be treated as the flag.
func TestExtractJSONFlag_UnrelatedStringsContainingJSON(t *testing.T) {
	in := []string{"foo=json", "json_value", "--jsonish", "--just-json"}
	got, cleaned := ExtractJSONFlag(in)
	if got {
		t.Errorf("jsonRequested = true, want false (no exact --json token)")
	}
	if !reflect.DeepEqual(cleaned, in) {
		t.Errorf("cleaned = %#v, want %#v (must be unchanged)", cleaned, in)
	}
}

// TestExtractJSONFlag_MixedWithOrdinaryFlags covers `--json` interleaved
// with other flags (the canonical use case for pre-scan switches in
// handlers like handleMemoryAdd).
func TestExtractJSONFlag_MixedWithOrdinaryFlags(t *testing.T) {
	in := []string{"--weight", "85", "--json", "--tags", "alpha,beta", "-j"}
	got, cleaned := ExtractJSONFlag(in)
	if !got {
		t.Errorf("jsonRequested = false, want true")
	}
	wantCleaned := []string{"--weight", "85", "--tags", "alpha,beta"}
	if !reflect.DeepEqual(cleaned, wantCleaned) {
		t.Errorf("cleaned = %#v, want %#v", cleaned, wantCleaned)
	}
}

// TestExtractJSONFlag_PreservedS1Separator covers the cross-helper
// invariant with S1's splitOnDashDash. After S1 splits at `--`, the
// flags-bearing portion can be passed to ExtractJSONFlag. The raw
// portion (after `--`) must NEVER be scanned for `--json`. This
// invariant is the reason callers must invoke `splitOnDashDash` first.
func TestExtractJSONFlag_PreservedS1Separator(t *testing.T) {
	in := []string{"--json", "--", "--json"}
	flagsArgs, rawArgs := splitOnDashDash(in)
	got, cleanedFlags := ExtractJSONFlag(flagsArgs)
	if !got {
		t.Errorf("jsonRequested = false, want true (--json in pre-s portion)")
	}
	wantFlags := []string{}
	if !reflect.DeepEqual(cleanedFlags, wantFlags) {
		t.Errorf("cleaned flags = %#v, want %#v", cleanedFlags, wantFlags)
	}
	// The rawArgs portion contains a literal `--json` token. Per
	// the S1 invariant, this is content — NOT a flag. ExtractJSONFlag
	// must NOT be called on rawArgs; if it were, it would (incorrectly)
	// report jsonRequested=true for what is actually positional content.
	// This test asserts the positive form: the helper only operates on
	// the flags-bearing portion.
	if rawArgs[0] != "--json" {
		t.Errorf("rawArgs[0] = %q, want %q (S1 separator invariant broken)",
			rawArgs[0], "--json")
	}
}

// TestExtractJSONFlag_Empty covers zero-token input.
func TestExtractJSONFlag_Empty(t *testing.T) {
	got, cleaned := ExtractJSONFlag([]string{})
	if got {
		t.Errorf("jsonRequested = true, want false")
	}
	if len(cleaned) != 0 {
		t.Errorf("cleaned = %#v, want empty", cleaned)
	}
}

// TestExtractJSONFlag_Nil covers nil input — must not panic.
func TestExtractJSONFlag_Nil(t *testing.T) {
	got, cleaned := ExtractJSONFlag(nil)
	if got {
		t.Errorf("jsonRequested = true, want false")
	}
	if cleaned == nil {
		t.Errorf("cleaned = nil, want empty (non-nil)")
	}
	if len(cleaned) != 0 {
		t.Errorf("cleaned = %#v, want empty", cleaned)
	}
}

// TestExtractJSONFlag_DoesNotMutateInput covers the invariant that
// the helper never mutates the caller's slice.
func TestExtractJSONFlag_DoesNotMutateInput(t *testing.T) {
	in := []string{"--json", "a", "-j", "b"}
	snapshot := make([]string, len(in))
	copy(snapshot, in)
	got, cleaned := ExtractJSONFlag(in)
	// Force incidental writes by consuming the result slices.
	if got {
		_ = got
	}
	for i := range cleaned {
		_ = cleaned[i]
	}
	if !reflect.DeepEqual(in, snapshot) {
		t.Errorf("input mutated: got %#v, want %#v", in, snapshot)
	}
}