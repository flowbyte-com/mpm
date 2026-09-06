// cli_args_enum_test.go — unit tests for the canonical CLI enum
// validator.
//
// Stage S4 of the approved CLI refactor. Covers the contract
// established in cli_args_enum.go.
package main

import (
	"strings"
	"testing"
)

// TestParseEnum_FirstAllowed covers the first allowed value.
func TestParseEnum_FirstAllowed(t *testing.T) {
	got, err := parseEnum("all", "scope", []string{"all", "local", "shared"})
	if err != nil {
		t.Errorf("err: got %v, want nil", err)
	}
	if got != "all" {
		t.Errorf("got %q, want %q", got, "all")
	}
}

// TestParseEnum_MiddleAllowed covers a middle allowed value.
func TestParseEnum_MiddleAllowed(t *testing.T) {
	got, err := parseEnum("local", "scope", []string{"all", "local", "shared"})
	if err != nil {
		t.Errorf("err: got %v, want nil", err)
	}
	if got != "local" {
		t.Errorf("got %q, want %q", got, "local")
	}
}

// TestParseEnum_LastAllowed covers the final allowed value.
func TestParseEnum_LastAllowed(t *testing.T) {
	got, err := parseEnum("shared", "scope", []string{"all", "local", "shared"})
	if err != nil {
		t.Errorf("err: got %v, want nil", err)
	}
	if got != "shared" {
		t.Errorf("got %q, want %q", got, "shared")
	}
}

// TestParseEnum_ArbitraryInvalid covers a value not in the allowed list.
func TestParseEnum_ArbitraryInvalid(t *testing.T) {
	_, err := parseEnum("locl", "scope", []string{"all", "local", "shared"})
	if err == nil {
		t.Errorf("err = nil, want error")
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Errorf("error %q must mention field name 'scope'", err.Error())
	}
	if !strings.Contains(err.Error(), "locl") {
		t.Errorf("error %q must mention supplied value 'locl'", err.Error())
	}
	if !strings.Contains(err.Error(), "all") || !strings.Contains(err.Error(), "local") || !strings.Contains(err.Error(), "shared") {
		t.Errorf("error %q must list all allowed values", err.Error())
	}
}

// TestParseEnum_EmptyInvalid covers empty input rejection.
func TestParseEnum_EmptyInvalid(t *testing.T) {
	_, err := parseEnum("", "scope", []string{"all", "local", "shared"})
	if err == nil {
		t.Errorf("err = nil, want error")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q must mention 'empty'", err.Error())
	}
}

// TestParseEnum_WrongCaseRejected — case sensitivity is STRICT.
func TestParseEnum_WrongCaseRejected(t *testing.T) {
	_, err := parseEnum("All", "scope", []string{"all", "local", "shared"})
	if err == nil {
		t.Errorf("err = nil, want error ('All' is NOT a match for 'all')")
	}
}

// TestParseEnum_WhitespaceInvalid — leading/trailing whitespace
// must NOT silently trim.
func TestParseEnum_WhitespaceInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"leading-space", " all"},
		{"trailing-space", "all "},
		{"both", " all "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseEnum(tc.in, "scope", []string{"all", "local", "shared"})
			if err == nil {
				t.Errorf("err = nil for %q, want error (whitespace rejected)", tc.in)
			}
		})
	}
}

// TestParseEnum_SubstringMatchRejected — substring must NOT match.
// "inf" must NOT match an allowed value of "info".
func TestParseEnum_SubstringMatchRejected(t *testing.T) {
	_, err := parseEnum("inf", "level", []string{"info", "warning", "error"})
	if err == nil {
		t.Errorf("err = nil, want error (substring must not match)")
	}
}

// TestParseEnum_NoAliasMapping — confirms the helper does NOT
// silently map "warning" to "warn" or any other alias.
func TestParseEnum_NoAliasMapping(t *testing.T) {
	_, err := parseEnum("warning", "level", []string{"warn", "error"})
	if err == nil {
		t.Errorf("err = nil, want error ('warning' is NOT in allowed; mapping is caller's job)")
	}
}

// TestParseEnum_NoLowercasing — confirms case is preserved.
func TestParseEnum_NoLowercasing(t *testing.T) {
	got, err := parseEnum("Local", "scope", []string{"all", "Local", "shared"})
	if err != nil {
		t.Errorf("err: got %v, want nil", err)
	}
	if got != "Local" {
		t.Errorf("got %q, want %q (case preserved)", got, "Local")
	}
}

// TestParseEnum_AllowedEmptyRejected covers an empty allowed list
// (defensive). Any non-empty input must error.
func TestParseEnum_AllowedEmptyRejected(t *testing.T) {
	_, err := parseEnum("anything", "x", []string{})
	if err == nil {
		t.Errorf("err = nil, want error (empty allowed list means nothing matches)")
	}
}

// TestParseEnum_ErrorIncludesFieldName — covers that the field
// name appears in the error for all rejection paths.
func TestParseEnum_ErrorIncludesFieldName(t *testing.T) {
	cases := []struct{
		name  string
		input string
	}{
		{"empty", ""},
		{"invalid", "xyz"},
		{"wrong-case", "Info"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseEnum(tc.input, "myfield", []string{"info"})
			if err == nil {
				t.Fatalf("err = nil for %q", tc.input)
			}
			if !strings.Contains(err.Error(), "myfield") {
				t.Errorf("error %q must mention 'myfield'", err.Error())
			}
		})
	}
}

// TestParseEnum_PreservesExactValue — covers that the returned
// canonical value equals the input verbatim (no transformation).
func TestParseEnum_PreservesExactValue(t *testing.T) {
	values := []string{"json", "csv"}
	for _, v := range values {
		got, err := parseEnum(v, "format", values)
		if err != nil {
			t.Errorf("err: got %v, want nil", err)
		}
		if got != v {
			t.Errorf("got %q, want %q (exact value preserved)", got, v)
		}
	}
}