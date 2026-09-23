// no_machine_specific_paths_guard_test.go — Repository-level guard.
//
// Pins the invariant established by the test-infrastructure cleanup
// (commit after 18f3f21) and tightened by the follow-up that removed
// the installed-binary fallback (the present commit): Go test fixtures
// under cmd/mpm and cmd/mpm-mcp must NOT reference machine/user-
// specific absolute paths. A source-tree regression test must run the
// binary built from that source; if the build fails, the test must
// fail with the build error. A stale installed executable must never
// make a broken source checkout pass.
//
// Background: a long-lived set of tests under cmd/mpm hardcoded
// "/home/v/.mpm/bin/mpm" (and cmd/mpm-mcp tests hardcoded
// "/home/v/.openclaw/.../mpm-mcp") as the path to the binary they
// exercised. That path was the install location on the original
// author's machine; under any other Unix user or environment the
// tests failed at exec.Command with "no such file or directory".
//
// The cleanup migrated every fixture to use the shared
// {mpm,mcp}Cmd helpers which build the source tree's binary into
// t.TempDir(). The follow-up removed the residual installed-binary
// fall-back (latent dead code that masked future build failures).
// This test scans all `*_test.go` files in BOTH packages for any
// residual machine-absolute path literal and fails fast on a hit,
// so a future regression (someone re-introducing a hardcoded path)
// is caught before it costs anyone a `make test` run.
//
// Scope:
//   - cmd/mpm and cmd/mpm-mcp test files only. Other packages have
//     their own patterns; this guard covers the two CLI-executable
//     test packages specifically.
//   - Checks for two known-bad path fragments: "/home/v/" and the
//     absolute-install pattern "/.mpm/bin/mpm" (which is a prefix
//     match for both bin/mpm and bin/mpm-mcp).
//   - Whitelists files whose doc-comment headers legitimately
//     reference the historical path strings.
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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scannedDirs lists every directory the guard inspects. Add a path
// here when introducing a new CLI-executable-test package.
//
// Relative to the repository root: cmd/mpm (mpm) and cmd/mpm-mcp
// (mpm-mcp). The cmd/mpm path is the package this guard lives in;
// cmd/mpm-mcp is a sibling package with its own fall-back that
// was removed in the same cleanup.
var scannedDirs = []string{
	"cmd/mpm",
	"cmd/mpm-mcp",
}

// TestNoMachineSpecificPathsInFixtures scans every _test.go file in
// the scanned packages for hardcoded machine/user paths. Fails if a
// match is found, listing the file and the literal that violated the
// guard. Whitelisted files (those whose doc-comment headers
// legitimately reference the historical path strings) are skipped.
func TestNoMachineSpecificPathsInFixtures(t *testing.T) {
	// Files whose doc-comment header legitimately references the
	// historical path strings. The strings appear in prose, not in
	// a fixture. Add entries only when the string is genuinely a
	// reference to the historical problem, not a fixture.
	whitelistByPath := map[string]bool{
		// cmd/mpm fixtures (this package):
		// The shared helper file's doc-comment header describes the
		// historical problem the cleanup fixed.
		"cmd/mpm/exec_helpers_test.go": true,
		// This guard's own failure messages reference the bad
		// patterns so the test is self-explanatory when it fires.
		"cmd/mpm/no_machine_specific_paths_guard_test.go": true,
		// The continue_overdue_renderer_regression_test.go's
		// doc-comment header narrates the discovery of the
		// renderer gap (and the previous fired-wake delivery
		// defect) but the path string does not appear in any
		// fixture.
		"cmd/mpm/continue_overdue_renderer_regression_test.go": true,
		// The fired-wake delivery release test's filename carries
		// "fired_wake_delivery" for release-tracking. Its
		// doc-comment header narrates the discovery but the path
		// string does not appear in any fixture.
		"cmd/mpm/release_pass_20260923_fired_wake_delivery_test.go": true,

		// cmd/mpm-mcp fixtures (sibling package):
		// The shared helper file's doc-comment header describes the
		// historical problem the cleanup fixed.
		"cmd/mpm-mcp/exec_helpers_test.go": true,
	}

	// Patterns we forbid in test fixtures (the historical bug):
	badPatterns := []string{
		// The original author's hardcoded install path. This is
		// the exact literal that broke the test under any other
		// user. Any reappearance is a regression.
		"/home/v/",
		// The install-prefix + bin + binary literal. The
		// "/.mpm/bin/mpm" string is a prefix of "/.mpm/bin/mpm-mcp",
		// so the same pattern covers both bin/mpm and bin/mpm-mcp
		// fixtures. A test that wants a specific binary should reach
		// for {mpm,mcp}Cmd + {mpmRun,mcpRun}.
		"/.mpm/bin/mpm",
	}

	fset := token.NewFileSet()

	for _, dir := range scannedDirs {
		entries, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil {
			t.Fatalf("glob %s/*_test.go: %v", dir, err)
		}
		for _, path := range entries {
			rel := path // already relative to cwd
			if !strings.HasPrefix(rel, dir+string(filepath.Separator)) {
				// Defence-in-depth: only scan files inside the
				// declared dir.
				continue
			}
			if whitelistByPath[rel] {
				continue
			}
			src, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				// Skip unparseable files (likely generated); the
				// test is about source code fixtures, not syntax.
				continue
			}
			// Walk every string literal in the AST. We don't try to
			// be clever about whether the literal is in a fixture
			// vs a doc-comment; any literal containing the bad
			// fragment is a violation.
			ast.Inspect(src, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				val := strings.Trim(lit.Value, "`\"")
				for _, pat := range badPatterns {
					if strings.Contains(val, pat) {
						pos := fset.Position(lit.Pos())
						t.Errorf("%s contains forbidden machine-specific path literal %q at %s:%d\n"+
							"Source-tree regression tests must run the binary built from this source tree. "+
							"Use the shared mpmCmd/mpmRun helpers (cmd/mpm/exec_helpers_test.go) or "+
							"mcpCmd (cmd/mpm-mcp/exec_helpers_test.go) to build a hermetic binary into "+
							"t.TempDir() instead.",
							rel, pat, pos.Filename, pos.Line)
					}
				}
				return true
			})
		}
	}

	// Be helpful: surface whether the guard ran anything. If a future
	// contributor adds a new test package without adding it to
	// scannedDirs, this line still passes (silent skip). The
	// contributor would need to notice the package isn't guarded
	// and add it themselves; the failure message above is the
	// real signal.
	if os.Getenv("MPM_TEST_PATH_GUARD_VERBOSE") != "" {
		var totalScanned int
		for _, dir := range scannedDirs {
			entries, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
			totalScanned += len(entries)
		}
		t.Logf("no_machine_specific_paths_guard: scanned %d _test.go files across %v", totalScanned, scannedDirs)
	}
}
