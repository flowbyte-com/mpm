// d005_reject_unknown_subcmd_test.go — regressions for audit finding D-005.
//
// D-005: `mpm decisions <unknown-subcommand>` silently fell through to
// the legacy list mode. Same anti-pattern in `mpm theories <bad-filter>`.
// The fix adds explicit `default:` cases that return a non-zero exit
// code and a list of valid options.
package main

import (
	"strings"
	"testing"
)

// TestD005_DecisionsRejectsUnknownSubcommand pins the headline regression:
// a typo'd subcommand must not silently fall through to the legacy list.
//
// handleDecisions is called with the verb already stripped (the router
// passes args[2:] for `mpm decisions ...`), so we pass the raw subcommand
// here — no "decisions" prefix.
func TestD005_DecisionsRejectsUnknownSubcommand(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	out := captureBoth(t, func() {
		rc := handleDecisions([]string{"bogusaction"})
		if rc == 0 {
			t.Errorf("handleDecisions with unknown subcommand returned rc=0; want non-zero (silent fallthrough regression)")
		}
	})

	if !strings.Contains(out, "Unknown subcommand") {
		t.Errorf("expected 'Unknown subcommand' message, got:\n%s", out)
	}
	if !strings.Contains(out, "bogusaction") {
		t.Errorf("error should echo the typo'd verb, got:\n%s", out)
	}
	if !strings.Contains(out, "show") || !strings.Contains(out, "list") || !strings.Contains(out, "query") {
		t.Errorf("error should list valid subcommands, got:\n%s", out)
	}
}

// TestD005_DecisionsAcceptsValidSubcommand pins the positive contract: the
// three documented subcommands must continue to work after the rejection
// logic was added.
func TestD005_DecisionsAcceptsValidSubcommand(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	// Each subcommand must accept its arg without returning an "Unknown"
	// error. (The list/query paths may return empty results with rc=0
	// since the test DB is fresh; the assertion is on the error path.)
	cases := []string{"show", "list", "query"}
	for _, sub := range cases {
		t.Run(sub, func(t *testing.T) {
			out := captureBoth(t, func() {
				handleDecisions([]string{sub})
			})
			if strings.Contains(out, "Unknown subcommand") {
				t.Errorf("subcommand %q was wrongly rejected: %s", sub, out)
			}
		})
	}
}

// TestD005_DecisionsNoArgsStillLists pins the backward-compat contract:
// `mpm decisions` with no args must keep the legacy list behaviour so
// operators' muscle memory is preserved. Adding explicit rejection
// logic for unknown subcommands must NOT break the no-args path.
func TestD005_DecisionsNoArgsStillLists(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	out := captureBoth(t, func() {
		handleDecisions([]string{})
	})
	if strings.Contains(out, "Unknown subcommand") {
		t.Errorf("no-args invocation should not trigger Unknown-subcommand error, got:\n%s", out)
	}
}

// TestD005_TheoriesRejectsUnknownFilter pins the sibling regression: a
// typo'd status filter must not silently fall through to "no theories".
func TestD005_TheoriesRejectsUnknownFilter(t *testing.T) {
	dm := newTestDMForCmd(t)
	if dm == nil {
		t.Skip("DB unavailable")
	}

	out := captureBoth(t, func() {
		rc := runTheories(dm, []string{"bogusfilter"})
		if rc == 0 {
			t.Errorf("runTheories with unknown filter returned rc=0; want non-zero")
		}
	})

	if !strings.Contains(out, "Unknown filter") {
		t.Errorf("expected 'Unknown filter' message, got:\n%s", out)
	}
	if !strings.Contains(out, "bogusfilter") {
		t.Errorf("error should echo the typo'd filter, got:\n%s", out)
	}
}

// TestD005_TheoriesAcceptsValidFilters pins the positive contract: the
// documented filters continue to work after rejection logic was added.
func TestD005_TheoriesAcceptsValidFilters(t *testing.T) {
	dm := newTestDMForCmd(t)
	if dm == nil {
		t.Skip("DB unavailable")
	}

	validFilters := []string{"all", "pending", "resolved", "proven", "disproven", "list", "ls"}
	for _, f := range validFilters {
		t.Run(f, func(t *testing.T) {
			out := captureBoth(t, func() {
				runTheories(dm, []string{f})
			})
			if strings.Contains(out, "Unknown filter") {
				t.Errorf("valid filter %q was wrongly rejected: %s", f, out)
			}
		})
	}
}

// TestD005_TheoriesHelpStillWorks pins the help path: `mpm theories help`
// must still print usage and exit 0, not be rejected by the new default
// branch.
func TestD005_TheoriesHelpStillWorks(t *testing.T) {
	dm := newTestDMForCmd(t)
	if dm == nil {
		t.Skip("DB unavailable")
	}

	out := captureBoth(t, func() {
		runTheories(dm, []string{"help"})
	})
	if strings.Contains(out, "Unknown filter") {
		t.Errorf("'help' subcommand should not trigger Unknown-filter error, got:\n%s", out)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("'help' should print Usage line, got:\n%s", out)
	}
}
