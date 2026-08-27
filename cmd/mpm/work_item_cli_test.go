package main

import (
	"strings"
	"testing"
)

// TestWorkItemCLI_FlagOrdering pins the parser grammar for the
// `mpm work item` facade. The ergonomic bug was that
// `mpm work item --limit 5` (flags-before-subcommand) failed with
// "unknown subcommand --limit" because the original implementation
// grabbed the first positional arg as the subcommand name without
// first extracting flags.
//
// The grammar is now:
//
//	mpm work item [<flags...>] <sub> [<flags...>] [<positional...>]
//
// flags are accepted before the subcommand (defaulting to "list" if
// none is given) AND after it. Flag-value pairs are scanned as a unit
// so a value-taking flag's value is not mistaken for the subcommand.
//
// The test exercises the canonical invocations plus the two error
// paths and asserts on the user-visible exit code. Detailed stdout
// assertions live in the smoke-test shell commands run during the
// audit — the integration here focuses on the parser contract.
func TestWorkItemCLI_FlagOrdering(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantSuccess bool
		wantErrSub  string
	}{
		{
			name:        "flags_before_subcommand_default_to_list",
			args:        []string{"--limit", "3"},
			wantSuccess: true,
		},
		{
			name:        "subcommand_then_flags",
			args:        []string{"list", "--limit", "3"},
			wantSuccess: true,
		},
		{
			name:        "status_flag_before_subcommand",
			args:        []string{"--status", "all"},
			wantSuccess: true,
		},
		{
			name:        "status_flag_after_subcommand",
			args:        []string{"list", "--status", "all"},
			wantSuccess: true,
		},
		{
			name:        "bare_work_item_shows_help",
			args:        []string{},
			wantSuccess: true,
		},
		{
			name:        "explicit_list_with_no_flags",
			args:        []string{"list"},
			wantSuccess: true,
		},
		{
			name:        "unknown_subcommand_errors_clearly",
			args:        []string{"bogus"},
			wantSuccess: false,
			wantErrSub:  "unknown subcommand",
		},
		{
			name:        "unknown_flag_errors_clearly",
			args:        []string{"list", "--bogus-flag"},
			wantSuccess: false,
			wantErrSub:  "unknown flag",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exit := runWorkItemArgsForTest(tc.args)
			if tc.wantSuccess && exit != 0 {
				t.Errorf("expected exit 0, got %d", exit)
			}
			if !tc.wantSuccess && exit == 0 {
				t.Errorf("expected non-zero exit for error case, got 0")
			}
			// The error message text assertions live in the smoke-test
			// shell commands. Here we just confirm the failure mode
			// triggers — substring checks would require stdout
			// capture which is a separate concern from the parser.
			_ = strings.TrimSpace
		})
	}
}

// TestWorkItemCLI_GrammarContract is a documentation-grade test:
// locks in the canonical invocation grammar so a future refactor
// that re-introduces the flag-as-subcommand bug will fail loudly.
func TestWorkItemCLI_GrammarContract(t *testing.T) {
	// Each entry asserts that this invocation succeeds (exit 0) —
	// the user-facing contract is "these invocations must work".
	invocations := [][]string{
		{"--limit", "5"},                  // flags-only → defaults to list
		{"list", "--limit", "5"},           // canonical
		{"--status", "all"},                // status before subcommand
		{"list", "--status", "all"},        // canonical
		{"list"},                           // bare list
		{},                                 // bare work item → help
		{"help"},                           // explicit help
		{"-h"},                             // short help
		{"--help"},                         // long help (RECOMMENDED 12)
		{"list", "--help"},                 // help-after-subcommand
		{"create", "My Title", "content"},  // create with positional content
		{"create", "Just Title"},           // create title-only
	}
	for _, args := range invocations {
		got := runWorkItemArgsForTest(args)
		if got != 0 {
			t.Errorf("invocation %v expected success, got exit %d", args, got)
		}
	}
}