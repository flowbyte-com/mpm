// no_machine_specific_paths_guard_test.go — Repository-level guard.
//
// Pins the invariant established by the test-infrastructure cleanup
// (commit after 18f3f21): Go test fixtures under cmd/mpm must NOT
// reference machine/user-specific absolute paths.
//
// Background: a long-lived set of tests under cmd/mpm hardcoded
// "/home/v/.mpm/bin/mpm" as the path to the mpm binary they
// exercised. That path was the install location on the original
// author's machine; under any other Unix user or environment the
// tests failed at exec.Command with "no such file or directory".
//
// The cleanup migrated every fixture to use the shared mpmCmd
// helper which builds the source tree's binary into t.TempDir().
// This test scans all `*_test.go` files in the package for any
// residual machine-absolute path literals and fails fast on a hit,
// so a future regression (someone re-introducing a hardcoded path)
// is caught before it costs anyone a `make test` run.
//
// Scope:
//   - cmd/mpm test files only (the package whose tests held the
//     fixture problem). Other packages have their own guards.
//   - Checks for two known-bad path fragments: "/home/v/" and the
//     absolute-install pattern "/.mpm/bin/mpm".
//   - Whitelists the new exec_helpers_test.go file (which legitimately
//     describes the historical problem in its doc-comment header)
//     and this guard file itself (which references the bad patterns
//     in its failure messages).
//
// This is intentionally narrow. We do NOT match "/home/" generally
// (legitimate documentation / synthetic test data uses other home
// paths) nor "/.mpm/" generally (the canonical install location is
// correct in production code).

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoMachineSpecificPathsInFixtures scans every _test.go file in
// the cmd/mpm package for hardcoded machine/user paths. Fails if a
// match is found, listing the file and the literal that violated the
// guard. Whitelisted files (the helper that intentionally documents
// the historical problem, and this guard itself) are skipped.
func TestNoMachineSpecificPathsInFixtures(t *testing.T) {
	whitelist := map[string]bool{
		// The exec_helpers_test.go file's doc-comment header
		// describes the historical problem the cleanup fixed. The
		// string appears in prose, not in a fixture.
		"exec_helpers_test.go": true,
		// This guard's own failure messages reference the bad
		// patterns so the test is self-explanatory when it fires.
		"no_machine_specific_paths_guard_test.go": true,
		// The continue_overdue_renderer_regression_test.go's
		// doc-comment header narrates the discovery of the renderer
		// gap (and the previous fired-wake delivery defect) but the
		// path string does not appear in any fixture.
		"continue_overdue_renderer_regression_test.go": true,
		// The fired-wake delivery release test's filename carries
		// "fired_wake_delivery" for release-tracking. Its doc-comment
		// header narrates the discovery but the path string does
		// not appear in any fixture.
		"release_pass_20260923_fired_wake_delivery_test.go": true,
	}

	// Patterns we forbid in test fixtures (the historical bug):
	badPatterns := []string{
		// The original author's hardcoded install path. This is
		// the exact literal that broke the test under any other
		// user. Any reappearance is a regression.
		"/home/v/",
		// The install-prefix + bin + binary literal. Same
		// regression in different packaging. A test that wants a
		// specific binary should reach for mpmCmd / mpmRun.
		"/.mpm/bin/mpm",
	}

	fset := token.NewFileSet()
	entries, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob *_test.go: %v", err)
	}

	for _, path := range entries {
		name := filepath.Base(path)
		if whitelist[name] {
			continue
		}
		src, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			// Skip unparseable files (likely generated); the test
			// is about source code fixtures, not syntax.
			continue
		}
		// Walk every string literal in the AST. We don't try to be
		// clever about whether the literal is in a fixture vs a
		// doc-comment; any literal containing the bad fragment is
		// a violation.
		ast.Inspect(src, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val := strings.Trim(lit.Value, "`\"")
			for _, pat := range badPatterns {
				if strings.Contains(val, pat) {
					t.Errorf("%s contains forbidden machine-specific path literal %q at %s:%d\n"+
						"Use the shared mpmCmd/mpmRun helpers (exec_helpers_test.go) "+
						"to build a hermetic binary into t.TempDir() instead.",
						name, pat, fset.Position(lit.Pos()).Filename, fset.Position(lit.Pos()).Line)
				}
			}
			return true
		})
	}
}
