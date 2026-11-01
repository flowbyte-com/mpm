// bare_family_unknown_test.go — a bare unknown token after a family
// name must fail visibly.
//
// WHAT THIS PINS
// --------------
// The router intentionally skips the first token for subcommand
// families, so `mpm ops --nonsense` is reported as an unknown
// SUBCOMMAND by the dispatcher rather than as an unknown option
// (see cli_options_registry.go). Ops, work, config and handoff honor
// that contract: the dispatcher rejects the token with a nonzero
// exit. Memory, lesson, topic and session did not: their default
// branch printed the family help page and exited 0, silently
// swallowing the operator's mistake — so `mpm memory --bogus` and
// `mpm memory frobnicate` both SUCCEEDED.
//
// The dangerous direction is the one that matters: a script invoking
// a typo'd subcommand exited 0, and an operator who mistyped a flag
// was told nothing was wrong (SPEC exit codes: 1 for unknown
// subcommand / unknown flag, nothing written).
//
// Runs against an isolated workspace; the live installation is never
// touched (same fixture as the CLI help contract).
package main

import (
	"strings"
	"testing"
)

// TestBareFamily_UnknownTokenFailsVisibly drives the four core
// families with a bare unknown token — once as an option-shaped
// token, once as a plain subcommand-shaped token — and requires a
// nonzero exit that names the token.
func TestBareFamily_UnknownTokenFailsVisibly(t *testing.T) {
	bin, ws := cliContractFixture(t)

	families := []string{"memory", "lesson", "topic", "session"}
	probes := []string{"--definitely-not-a-real-flag", "frobnicate"}

	for _, fam := range families {
		for _, tok := range probes {
			fam, tok := fam, tok
			t.Run(fam+"_"+tok, func(t *testing.T) {
				out, code := runContractCLI(t, bin, ws, fam, tok)
				if code == 0 {
					t.Errorf("`mpm %s %s` exited 0 with an unrecognised token; it must fail:\n%s",
						fam, tok, out)
				}
				if !strings.Contains(out, tok) {
					t.Errorf("the rejection must name the offending token %q:\n%s", tok, out)
				}
			})
		}
	}
}

// TestBareFamily_ValidLeavesStillSucceed guards the other direction:
// the fix must not turn a valid leaf invocation into an error, and
// help requests must keep exiting 0.
func TestBareFamily_ValidLeavesStillSucceed(t *testing.T) {
	bin, ws := cliContractFixture(t)

	valid := [][]string{
		{"memory", "list"},
		{"lesson", "list"},
		{"topic", "list"},
		{"session", "list"},
	}
	for _, args := range valid {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			out, code := runContractCLI(t, bin, ws, args...)
			if code != 0 {
				t.Errorf("`mpm %s` exited %d; a valid leaf must succeed:\n%s",
					strings.Join(args, " "), code, out)
			}
		})
	}

	helpForms := [][]string{
		{"memory", "--help"},
		{"lesson", "--help"},
		{"topic", "--help"},
		{"session", "--help"},
		{"memory", "help"},
	}
	for _, args := range helpForms {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			out, code := runContractCLI(t, bin, ws, args...)
			if code != 0 {
				t.Errorf("`mpm %s` exited %d; a help request must succeed:\n%s",
					strings.Join(args, " "), code, out)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("`mpm %s` printed nothing; a help request must print the help", strings.Join(args, " "))
			}
		})
	}
}
