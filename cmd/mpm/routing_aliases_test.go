// routing_aliases_test.go — unknown commands fail, aliases resolve.
//
// WHAT THIS PINS
// --------------
//  1. An unrecognised command is an error, never an implicit memory
//     search. `mpm lss` exits nonzero, names the token, suggests the
//     closest command WITHOUT executing it, and performs no retrieval.
//  2. `doc` is an explicit alias for `doctor`; `r` is the alias for
//     `recall`; the old `s` recall alias is gone.
//  3. Explicit `mpm recall <query>` and `mpm r <query>` have identical
//     semantics (text and JSON).
//  4. No configured alias collides with a registered command name,
//     and no two commands share an alias — so an alias can never
//     shadow or hijack a real command.
//
// Runs against isolated workspaces; the live installation is never
// touched (same fixture as the CLI help contract).
package main

import (
	"strings"
	"testing"
)

// TestRouting_UnknownCommandFailsWithoutRetrieval drives unknown
// commands end to end: nonzero exit, the token named, a suggestion
// for close typos (and none for unrelated tokens), and — critically —
// no memory search output.
func TestRouting_UnknownCommandFailsWithoutRetrieval(t *testing.T) {
	bin, ws := cliContractFixture(t)

	cases := []struct {
		name       string
		args       []string
		suggestion string // "" means no suggestion expected
	}{
		{"close_typo", []string{"lss"}, `"ls"`},
		{"recall_typo", []string{"recoll"}, `"recall"`},
		{"multi_token", []string{"lss", "foo"}, `"ls"`},
		{"unrelated", []string{"pinoeques"}, ""},
		{"removed_alias", []string{"s"}, `"ls"`}, // resolves nowhere; "ls" is the legitimate suggestion
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			out, code := runContractCLI(t, bin, ws, tc.args...)
			if code == 0 {
				t.Errorf("`mpm %s` exited 0; an unknown command must fail:\n%s",
					strings.Join(tc.args, " "), out)
			}
			if !strings.Contains(out, tc.args[0]) {
				t.Errorf("the rejection must name the offending token %q:\n%s", tc.args[0], out)
			}
			if strings.Contains(out, "No memories found for:") ||
				strings.Contains(out, "No candidates returned for:") {
				t.Errorf("an unknown command must never run a memory search:\n%s", out)
			}
			if tc.suggestion == "" {
				if strings.Contains(out, "Did you mean") {
					t.Errorf("did not expect a suggestion for %q:\n%s", tc.args[0], out)
				}
			} else if !strings.Contains(out, "Did you mean "+tc.suggestion) {
				t.Errorf("expected suggestion %s for %q:\n%s",
					tc.suggestion, tc.args[0], out)
			}
		})
	}
}

// TestRouting_AliasResolution checks the command table directly: `r`
// resolves to recall, `doc` resolves to doctor, `s` resolves to
// nothing, and unknown names resolve to nothing.
func TestRouting_AliasResolution(t *testing.T) {
	r := NewRouter()

	if got := r.resolveCommand("r"); got == nil || got.Name != "recall" {
		t.Errorf("`r` should resolve to recall, got %+v", got)
	}
	if got := r.resolveCommand("doc"); got == nil || got.Name != "doctor" {
		t.Errorf("`doc` should resolve to doctor, got %+v", got)
	}
	if got := r.resolveCommand("recall"); got == nil || got.Name != "recall" {
		t.Errorf("`recall` should resolve to recall, got %+v", got)
	}
	if got := r.resolveCommand("doctor"); got == nil || got.Name != "doctor" {
		t.Errorf("`doctor` should resolve to doctor, got %+v", got)
	}
	if got := r.resolveCommand("s"); got != nil {
		t.Errorf("`s` must no longer resolve to anything, got %+v", got)
	}
	if got := r.resolveCommand("nosuchcmd"); got != nil {
		t.Errorf("unknown names must not resolve, got %+v", got)
	}
}

// TestRouting_AliasEndToEnd proves the aliases work through the real
// binary: `mpm r --help` prints the recall page, `mpm doc --help`
// prints the doctor page, and `mpm s` is an unknown command.
func TestRouting_AliasEndToEnd(t *testing.T) {
	bin, ws := cliContractFixture(t)

	if out, code := runContractCLI(t, bin, ws, "r", "--help"); code != 0 {
		t.Errorf("`mpm r --help` exited %d; an alias help request must succeed:\n%s", code, out)
	}
	docOut, docCode := runContractCLI(t, bin, ws, "doc", "--help")
	if docCode != 0 {
		t.Fatalf("`mpm doc --help` exited %d:\n%s", docCode, docOut)
	}
	doctorOut, doctorCode := runContractCLI(t, bin, ws, "doctor", "--help")
	if doctorCode != 0 {
		t.Fatalf("`mpm doctor --help` exited %d:\n%s", doctorCode, doctorOut)
	}
	if docOut != doctorOut {
		t.Errorf("`mpm doc --help` must print the doctor page:\n--- doc ---\n%s\n--- doctor ---\n%s",
			docOut, doctorOut)
	}
	if out, code := runContractCLI(t, bin, ws, "s", "anything"); code == 0 {
		t.Errorf("`mpm s anything` exited 0; the removed alias must not resolve:\n%s", out)
	}
}

// TestRouting_RecallAliasIdenticalSemantics pins requirement 6:
// `mpm r <query>` and `mpm recall <query>` produce byte-identical
// output in both human and JSON modes.
func TestRouting_RecallAliasIdenticalSemantics(t *testing.T) {
	bin, ws := cliContractFixture(t)

	for _, extra := range [][]string{{}, {"--json"}} {
		viaRecall, codeRecall := runContractCLI(t, bin, ws, append([]string{"recall", "lss"}, extra...)...)
		viaAlias, codeAlias := runContractCLI(t, bin, ws, append([]string{"r", "lss"}, extra...)...)
		if codeRecall != codeAlias {
			t.Errorf("exit codes differ (recall=%d, r=%d) for flags %v:\n%s\n%s",
				codeRecall, codeAlias, extra, viaRecall, viaAlias)
		}
		if viaRecall != viaAlias {
			t.Errorf("output differs between `recall` and `r` for flags %v:\n--- recall ---\n%s\n--- r ---\n%s",
				extra, viaRecall, viaAlias)
		}
		if codeRecall != 0 {
			t.Errorf("explicit recall must succeed, got %d:\n%s", codeRecall, viaRecall)
		}
		if strings.Contains(viaRecall, "Unknown command") || strings.Contains(viaRecall, "Did you mean") {
			t.Errorf("explicit recall must not take the unknown-command path:\n%s", viaRecall)
		}
	}
}

// TestRouting_NoAliasCollidesWithCommands audits every configured
// alias: none may equal a registered command name (which would make
// resolution ambiguous), and none may be claimed by two commands.
func TestRouting_NoAliasCollidesWithCommands(t *testing.T) {
	r := NewRouter()

	names := map[string]bool{}
	for name := range r.Commands {
		names[name] = true
	}
	owners := map[string]string{}
	for name, cmd := range r.Commands {
		for _, a := range cmd.Aliases {
			if a == "" {
				t.Errorf("command %q declares an empty alias", name)
			}
			if names[a] {
				t.Errorf("alias %q of command %q collides with a registered command name", a, name)
			}
			if prev, dup := owners[a]; dup {
				t.Errorf("alias %q is claimed by both %q and %q", a, prev, name)
			}
			owners[a] = name
		}
	}
}
