// help_safety_20260914_config_test.go — Regression coverage for
// the 2026-09-14 release-pass `mpm config` subcommand-specific
// help pages.
//
// Prior to the fix, `mpm config profile --help` (and component /
// detect-embedding variants) all collapsed to the generic
// `mpm config --help` "Configure AI provider" fallback. The fix
// adds dedicated subcommand-specific help pages that route
// through the canonical visual grammar.

package main

import (
	"strings"
	"testing"
)

// configHelpSafetyMatrix enumerates the newly-fixed nested help
// surfaces and the assertions each must satisfy:
//
//   1. Exit code 0.
//   2. Output mentions the subcommand name (so a reader knows
//      which subcommand they're looking at).
//   3. Output does NOT contain the legacy `Configure AI provider`
//      heading — that wording conflates LLM provider with
//      embedding model configuration.
type configHelpSafetyCase struct {
	Command       []string
	ExpectedSubs  string // substring that must appear in output
	Disallowed    string // substring that must NOT appear in output
}

var configHelpSafetyMatrix = []configHelpSafetyCase{
	{
		Command:      []string{"config", "profile", "--help"},
		ExpectedSubs: "Config profile",
		Disallowed:   "Configure the AI provider",
	},
	{
		Command:      []string{"config", "profile", "-h"},
		ExpectedSubs: "Config profile",
		Disallowed:   "Configure the AI provider",
	},
	{
		Command:      []string{"config", "component", "--help"},
		ExpectedSubs: "Config component",
		Disallowed:   "Configure the AI provider",
	},
	{
		Command:      []string{"config", "component", "-h"},
		ExpectedSubs: "Config component",
		Disallowed:   "Configure the AI provider",
	},
	{
		Command:      []string{"config", "detect-embedding", "--help"},
		ExpectedSubs: "Config detect-embedding",
		Disallowed:   "Configure the AI provider",
	},
	{
		Command:      []string{"config", "detect-embedding", "-h"},
		ExpectedSubs: "Config detect-embedding",
		Disallowed:   "Configure the AI provider",
	},
	{
		// Plain `mpm config --help` reaches the canonical config page.
		Command:      []string{"config", "--help"},
		ExpectedSubs: "LLM provider",
		Disallowed:   "Configure the AI provider",
	},
	{
		Command:      []string{"config", "-h"},
		ExpectedSubs: "LLM provider",
		Disallowed:   "Configure the AI provider",
	},
}

// TestHelpSafety_20260914ConfigSubcommandHelp proves each newly-
// fixed nested config surface exits 0 with the dedicated help page
// (not the legacy generic fallback). Pre-fix this test would fail
// because every nested surface emitted the 3-line generic
// `Configure AI provider` text.
func TestHelpSafety_20260914ConfigSubcommandHelp(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	for _, c := range configHelpSafetyMatrix {
		t.Run(strings.Join(c.Command, "_"), func(t *testing.T) {
			workspace := t.TempDir()
			stdout, _, exit := runMpmParity(t, bin, workspace, c.Command...)
			if exit != 0 {
				t.Logf("--help for %v exited %d (informational)", c.Command, exit)
			}
			out := string(stdout)
			if !strings.Contains(out, c.ExpectedSubs) {
				t.Fatalf("`%s` must contain %q (canonical subcommand heading).\nGot:\n%s", strings.Join(c.Command, " "), c.ExpectedSubs, out)
			}
			if strings.Contains(out, c.Disallowed) {
				t.Fatalf("`%s` must NOT contain legacy wording %q.\nGot:\n%s", strings.Join(c.Command, " "), c.Disallowed, out)
			}
		})
	}
}
