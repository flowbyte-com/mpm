// cli_args_separator_test.go — unit tests for the POSIX `--` separator helper.
//
// Stage S1 of the approved CLI refactor. Covers the eight required
// cases plus edge cases that have appeared in MPM defect history
// (D3, leading-dash content, repeated `--` tokens).
package main

import (
	"reflect"
	"testing"
)

// TestSplitOnDashDash_SeparatorOnePositional covers case 1: separator
// with one positional value after it.
func TestSplitOnDashDash_SeparatorOnePositional(t *testing.T) {
	flags, raw := splitOnDashDash([]string{"--", "-foo"})
	wantFlags := []string{}
	wantRaw := []string{"-foo"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags = %#v, want %#v", flags, wantFlags)
	}
	if !reflect.DeepEqual(raw, wantRaw) {
		t.Errorf("raw = %#v, want %#v", raw, wantRaw)
	}
}

// TestSplitOnDashDash_SeparatorMultiplePositional covers case 2:
// separator followed by multiple positional values.
func TestSplitOnDashDash_SeparatorMultiplePositional(t *testing.T) {
	flags, raw := splitOnDashDash([]string{"--json", "--", "-foo", "--bar", "baz"})
	wantFlags := []string{"--json"}
	wantRaw := []string{"-foo", "--bar", "baz"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags = %#v, want %#v", flags, wantFlags)
	}
	if !reflect.DeepEqual(raw, wantRaw) {
		t.Errorf("raw = %#v, want %#v", raw, wantRaw)
	}
}

// TestSplitOnDashDash_PositionalBeginningWithSingleDash covers case 3:
// a single positional value that begins with `-`. This is the canonical
// D3 motivating case — `mpm memory add -- ---yaml-front-matter`.
func TestSplitOnDashDash_PositionalBeginningWithSingleDash(t *testing.T) {
	flags, raw := splitOnDashDash([]string{"--", "---yaml-front-matter"})
	if len(flags) != 0 {
		t.Errorf("flags = %#v, want empty", flags)
	}
	if len(raw) != 1 || raw[0] != "---yaml-front-matter" {
		t.Errorf("raw = %#v, want [---yaml-front-matter]", raw)
	}
}

// TestSplitOnDashDash_MultipleDashPrefixedPositionals covers case 4:
// several positional values that all begin with `-`.
func TestSplitOnDashDash_MultipleDashPrefixedPositionals(t *testing.T) {
	flags, raw := splitOnDashDash([]string{"--", "-foo", "--bar", "-baz"})
	if len(flags) != 0 {
		t.Errorf("flags = %#v, want empty", flags)
	}
	wantRaw := []string{"-foo", "--bar", "-baz"}
	if !reflect.DeepEqual(raw, wantRaw) {
		t.Errorf("raw = %#v, want %#v", raw, wantRaw)
	}
}

// TestSplitOnDashDash_NoSeparator covers case 5: no separator at all.
// raw must be nil so callers can distinguish "no separator" from
// "separator with empty raw".
func TestSplitOnDashDash_NoSeparator(t *testing.T) {
	in := []string{"--json", "-j", "value"}
	flags, raw := splitOnDashDash(in)
	wantFlags := []string{"--json", "-j", "value"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags = %#v, want %#v", flags, wantFlags)
	}
	if raw != nil {
		t.Errorf("raw = %#v, want nil", raw)
	}
	// Verify no-copy contract: when no separator is present, the
	// returned flags slice must be the same backing array as the
	// input (saves an allocation on the common path).
	if &flags[0] != &in[0] {
		t.Errorf("expected flags to share backing array with input (no-copy); got different array")
	}
}

// TestSplitOnDashDash_SeparatorAtEnd covers case 6: `--` is the last
// token. raw must be empty (not nil, but length 0) to distinguish
// from case 5.
func TestSplitOnDashDash_SeparatorAtEnd(t *testing.T) {
	flags, raw := splitOnDashDash([]string{"--json", "--"})
	wantFlags := []string{"--json"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags = %#v, want %#v", flags, wantFlags)
	}
	if raw == nil {
		t.Errorf("raw = nil, want empty (non-nil)")
	}
	if len(raw) != 0 {
		t.Errorf("raw = %#v, want empty", raw)
	}
}

// TestSplitOnDashDash_RepeatedSeparatorAfterFirst covers case 7:
// repeated `--` tokens after the first must be returned in raw.
// Only the first `--` ends option processing.
func TestSplitOnDashDash_RepeatedSeparatorAfterFirst(t *testing.T) {
	flags, raw := splitOnDashDash([]string{"--", "--", "-foo"})
	if len(flags) != 0 {
		t.Errorf("flags = %#v, want empty", flags)
	}
	wantRaw := []string{"--", "-foo"}
	if !reflect.DeepEqual(raw, wantRaw) {
		t.Errorf("raw = %#v, want %#v", raw, wantRaw)
	}
}

// TestSplitOnDashDash_OrdinaryFlagsBeforeSeparator covers case 8:
// ordinary flags appearing before the separator must be returned in
// flags untouched.
func TestSplitOnDashDash_OrdinaryFlagsBeforeSeparator(t *testing.T) {
	in := []string{"--json", "-j", "--weight", "85", "--", "---content"}
	flags, raw := splitOnDashDash(in)
	wantFlags := []string{"--json", "-j", "--weight", "85"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags = %#v, want %#v", flags, wantFlags)
	}
	wantRaw := []string{"---content"}
	if !reflect.DeepEqual(raw, wantRaw) {
		t.Errorf("raw = %#v, want %#v", raw, wantRaw)
	}
}

// TestSplitOnDashDash_EmptyInput covers the zero-token case.
func TestSplitOnDashDash_EmptyInput(t *testing.T) {
	flags, raw := splitOnDashDash([]string{})
	if len(flags) != 0 {
		t.Errorf("flags = %#v, want empty", flags)
	}
	if raw != nil {
		t.Errorf("raw = %#v, want nil", raw)
	}
}

// TestSplitOnDashDash_NilInput covers a nil input — must not panic.
func TestSplitOnDashDash_NilInput(t *testing.T) {
	flags, raw := splitOnDashDash(nil)
	if len(flags) != 0 {
		t.Errorf("flags = %#v, want empty", flags)
	}
	if raw != nil {
		t.Errorf("raw = %#v, want nil", raw)
	}
}

// TestSplitOnDashDash_DoesNotMutateInput covers the invariant that
// the helper never mutates the caller's slice.
func TestSplitOnDashDash_DoesNotMutateInput(t *testing.T) {
	in := []string{"--json", "--", "-foo", "--bar"}
	snapshot := make([]string, len(in))
	copy(snapshot, in)
	flags, raw := splitOnDashDash(in)
	// Force any incidental write paths by reading from the returned
	// slices.
	for i := range flags {
		_ = flags[i]
	}
	for i := range raw {
		_ = raw[i]
	}
	if !reflect.DeepEqual(in, snapshot) {
		t.Errorf("input mutated: got %#v, want %#v", in, snapshot)
	}
}

// TestSplitOnDashDash_LongPrefixDoesNotMatch covers the negative case:
// tokens that LOOK like flags but aren't actually `--` (e.g. `---`
// or `--something`) must not be treated as the separator. Only the
// exact token `--` ends option processing.
func TestSplitOnDashDash_LongPrefixDoesNotMatch(t *testing.T) {
	flags, raw := splitOnDashDash([]string{"---yaml", "--something"})
	// Neither matches the exact `--` token, so flags contains both
	// and raw is nil.
	wantFlags := []string{"---yaml", "--something"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags = %#v, want %#v", flags, wantFlags)
	}
	if raw != nil {
		t.Errorf("raw = %#v, want nil", raw)
	}
}