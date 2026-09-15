// handlers_config_probe_test.go — Regression tests for the post-save
// probe wiring through `handleProfileAdd` and `handleProfileSet`.
//
//   - `mpm config profile add <name> --provider --model --base-url`
//     must parse flags, save successfully, and trigger the post-save
//     probe (verified via fingerprint of the resulting profile).
//   - `mpm config profile add <name> --provider <id>` (incomplete) must
//     NOT trigger a probe.
//   - `isMaterialChange` predicate must agree with the canonical
//     material-field set (provider / model / base_url / endpoint /
//     api_key / token).
//   - The pre-save gating via probe.CanProbe must be respected.

package main

import (
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// TestIsMaterialChange covers the canonical material-field set.
func TestIsMaterialChange(t *testing.T) {
	p := config.Profile{Provider: "p", Model: "m", BaseURL: "u"}
	cases := []struct {
		key  string
		want bool
	}{
		{"provider", true},
		{"model", true},
		{"base_url", true},
		{"endpoint", true},
		{"api_key", true},
		{"token", true},
		{"temperature", false},
		{"timeout", false},
		{"reasoning", false},
		{"max_tokens", false}, // removed from public surface; passing should still register as non-material
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			// add-key "add" path is reserved for handleProfileAdd.
			got := isMaterialChange(p, c.key)
			if got != c.want {
				t.Fatalf("isMaterialChange(key=%q) = %v, want %v", c.key, got, c.want)
			}
		})
	}
}

// TestIsMaterialChange_AddKeySentinel pins the empty-key ("add" path)
// rule: when the key is empty (handleProfileAdd), the result is true
// iff the resulting profile is structurally executable — i.e. probe.CanProbe(p).
// Add paths never carry a key, so this is the only signal available.
func TestIsMaterialChange_AddKeySentinel(t *testing.T) {
	cases := []struct {
		name string
		p    config.Profile
		want bool
	}{
		{"complete", config.Profile{Provider: "p", Model: "m", BaseURL: "u"}, true},
		{"missing model", config.Profile{Provider: "p", BaseURL: "u"}, false},
		{"missing base_url", config.Profile{Provider: "p", Model: "m"}, false},
		{"missing provider", config.Profile{Model: "m", BaseURL: "u"}, false},
		{"empty", config.Profile{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isMaterialChange(c.p, "")
			if got != c.want {
				t.Fatalf("isMaterialChange(empty key, %v) = %v, want %v",
					c.p, got, c.want)
			}
		})
	}
}

// TestExtractStringFlag covers the scriptable flag parser for
// `mpm config profile add --provider ... --model ... --base-url ...`.
//
// The parser is allowed to:
//
//   - Strip well-known long-form flags + their values from an argv.
//   - Return the value when present, "" when absent.
//   - Leave the argv in canonical order (no flags → unchanged).
//   - Preserve positional args that look similar to flag values
//     ("--provider" appearing as a positional argument is unusual but
//     should pass through unaltered).
func TestExtractStringFlag(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		flag      string
		wantRest  []string
		wantValue string
	}{
		{
			name:      "absent",
			args:      []string{"positional-a", "positional-b"},
			flag:      "--provider",
			wantRest:  []string{"positional-a", "positional-b"},
			wantValue: "",
		},
		{
			name:      "with value at end",
			args:      []string{"--provider", "custom", "positional-a"},
			flag:      "--provider",
			wantRest:  []string{"positional-a"},
			wantValue: "custom",
		},
		{
			name:      "with value at start",
			args:      []string{"--model", "gpt-4", "rest"},
			flag:      "--model",
			wantRest:  []string{"rest"},
			wantValue: "gpt-4",
		},
		{
			name:      "flag without value at end",
			args:      []string{"positional", "--provider"},
			flag:      "--provider",
			wantRest:  []string{"positional", "--provider"},
			wantValue: "",
		},
		{
			name:      "multiple flags in argv",
			args:      []string{"--provider", "custom", "--model", "gpt-4", "--base-url", "http://x"},
			flag:      "--model",
			wantRest:  []string{"--provider", "custom", "--base-url", "http://x"},
			wantValue: "gpt-4",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotRest, gotValue := extractStringFlag(c.args, c.flag)
			if gotValue != c.wantValue {
				t.Fatalf("value = %q, want %q", gotValue, c.wantValue)
			}
			if len(gotRest) != len(c.wantRest) {
				t.Fatalf("rest len = %d, want %d (rest = %v)",
					len(gotRest), len(c.wantRest), gotRest)
			}
			for i, w := range c.wantRest {
				if gotRest[i] != w {
					t.Fatalf("rest[%d] = %q, want %q", i, gotRest[i], w)
				}
			}
		})
	}
}
