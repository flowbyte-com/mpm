// w6_lint_dispatch_test.go — W-6 (debt burn-down, 2026-09-01)
// regression test for the `mpm lint` dispatch regression.
//
// Bug: `mpm lint` returned "unknown flag 'lint'" because the top-level
// dispatch in cmd/mpm/router.go passed `args` (which still contained
// "lint" at index 0) into handleLint. The handler's flag-parsing loop
// then saw "lint" itself as an unknown flag.
//
// Fix: top-level dispatch now strips the command name, matching the
// handleWhy / handleOps convention.
//
// Invariant: `mpm lint` (zero positional args) must NOT print
// "unknown flag 'lint'" and must reach the handler body that performs
// the actual lint work. The test exercises both the in-process router
// (fast) and the subprocess binary (true end-to-end), and confirms an
// unrelated command (e.g. `mpm status`) still parses correctly.

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestW6_LintDispatch_DoesNotRejectCommandName is the in-process unit
// test. The router must accept `mpm lint` (zero flags) and dispatch
// to handleLint without the flag loop treating "lint" as unknown.
func TestW6_LintDispatch_DoesNotRejectCommandName(t *testing.T) {
	router := NewRouter()
	out := captureBoth(t, func() {
		router.Execute([]string{"lint"})
	})

	// Pre-fix: stderr contained "unknown flag \"lint\"".
	if strings.Contains(out, "unknown flag \"lint\"") {
		t.Fatalf("W-6 regression: `mpm lint` rejected its own command name: %s", out)
	}
	if strings.Contains(out, "unknown flag 'lint'") {
		t.Fatalf("W-6 regression: `mpm lint` rejected its own command name: %s", out)
	}

	// Post-fix: dispatch reached the handler body. The handler either
	// prints a lint report or returns silently (clean exit). Either
	// way, it must NOT emit a "no help available" hint (which would
	// indicate the wrong code path was taken).
	if strings.Contains(out, "no help available") {
		t.Fatalf("W-6 regression: `mpm lint` routed to help instead of handler: %s", out)
	}
}

// TestW6_LintDispatch_HelpFlag is the second invariant: --help must
// surface lint-specific usage text (not the generic "unknown flag"
// error from before). The CLI router intercepts --help at the
// top level and prints the registered-command description from the
// registry ("Validate router frontmatter (YAML + regex compile)").
// This proves the dispatch did not regress: --help reaches the
// help handler, not the flag loop inside handleLint.
func TestW6_LintDispatch_HelpFlag(t *testing.T) {
	router := NewRouter()
	out := captureBoth(t, func() {
		router.Execute([]string{"lint", "--help"})
	})

	if strings.Contains(out, "unknown flag") {
		t.Fatalf("W-6 regression: `mpm lint --help` returned 'unknown flag': %s", out)
	}
	// The router-level help contains the registered command description.
	if !strings.Contains(out, "Validate router frontmatter") {
		t.Fatalf("W-6 regression: `mpm lint --help` did not print router help: %s", out)
	}
}

// TestW6_LintDispatch_UnrelatedCommandUnchanged ensures the dispatch
// fix did not break sibling commands. `mpm status` must still parse
// and dispatch correctly.
func TestW6_LintDispatch_UnrelatedCommandUnchanged(t *testing.T) {
	router := NewRouter()
	out := captureBoth(t, func() {
		router.Execute([]string{"status"})
	})

	// We don't care about status output shape — only that it dispatched.
	if strings.Contains(out, "unknown command \"status\"") {
		t.Fatalf("W-6 regression: unrelated `mpm status` regressed: %s", out)
	}
	if strings.Contains(out, "unknown flag") {
		t.Fatalf("W-6 regression: unrelated `mpm status` got 'unknown flag': %s", out)
	}
}

// TestW6_LintDispatch_SubprocessEndToEnd is the end-to-end test that
// proves the user-facing binary `bin/mpm lint` works. This is the
// canonical invocation; tests that only exercise the in-process router
// miss environment-isolation / binary-build issues.
//
// Skipped if bin/mpm is not built (mirrors the convention in
// help_regression_test.go).
func TestW6_LintDispatch_SubprocessEndToEnd(t *testing.T) {
	binPath := requireBuiltCLI(t)

	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	run := func(args ...string) (string, int) {
		cmd := exec.Command(binPath, args...)
		// Capture both stdout and stderr. exec.Output only gets stdout,
		// but the W-6 regression emitted its diagnostic on stderr.
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return string(out), code
	}

	// 1. `bin/mpm lint` (zero flags) — the primary regression.
	out, code := run("lint")
	if strings.Contains(out, "unknown flag") {
		t.Fatalf("W-6 regression in subprocess: %s", out)
	}
	if code != 0 && code != 1 {
		// 0 = clean, 1 = lint issues found (legitimate). Anything else
		// is an operational error (the W-6 bug was a 1 + "unknown flag"
		// stderr text, which we already filter above).
		t.Fatalf("W-6 regression in subprocess: unexpected exit code %d: %s", code, out)
	}

	// 2. `bin/mpm lint --help` — router-level help text (intercepted
	// before reaching the handler).
	out, _ = run("lint", "--help")
	if strings.Contains(out, "unknown flag") {
		t.Fatalf("W-6 regression in subprocess (--help): %s", out)
	}
	if !strings.Contains(out, "Validate router frontmatter") {
		t.Fatalf("W-6 regression in subprocess (--help): router help missing: %s", out)
	}

	// 3. `bin/mpm ops lint` (the parent route) must still work.
	out, code = run("ops", "lint")
	if strings.Contains(out, "unknown flag") {
		t.Fatalf("W-6 regression in subprocess (ops route): %s", out)
	}
	if code != 0 && code != 1 {
		t.Fatalf("W-6 regression in subprocess (ops route): unexpected exit code %d: %s", code, out)
	}
}

// captureBoth captures stdout+stderr from fn. Defined in
// f8_f10_regression_test.go (shared test helper). This file reuses it
// without redeclaration.
