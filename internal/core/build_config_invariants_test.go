package internal

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestBuildConfig_MakefileHasRaceDetectorTarget pins that the canonical
// race-detector-enabled test invocation lives in the Makefile, so future
// editors do not silently regress it to bare `go test -race ./...` (which
// compiles the mattn/go-sqlite3 driver without FTS5 and breaks the search,
// pointer-resolution, and scheduler-triggers code paths that
// unconditionally assume FTS5 is compiled in).
//
// The FTS5 capability is gated by:
//   - CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1  (C-level compile flag for SQLite)
//   - -tags fts5                         (Go build tag for the driver)
//
// Both must be present when invoking the test runner with the race
// detector enabled. Bare `go test -race ./...` (no flags) cannot
// succeed in this codebase without disabling FTS5 — which would
// regress architectural guarantees (FTS5 integrity).
//
// Regression target: this test fails when the Makefile's `test-race`
// recipe loses the FTS5 build flags or the race detector flag. It does
// not run the test suite itself — that is the developer's invocation.
func TestBuildConfig_MakefileHasRaceDetectorTarget(t *testing.T) {
	makefilePath := filepath.Join(findRepoRoot(t), "Makefile")
	data, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read %s: %v", makefilePath, err)
	}
	text := string(data)

	phonyRE := regexp.MustCompile(`(?m)^\.PHONY:[^\n]*\btest-race\b`)
	if !phonyRE.MatchString(text) {
		t.Fatalf("Makefile `.PHONY` line must include `test-race` target.\n" +
			"  Why: bare `go test -race ./...` compiles go-sqlite3 without FTS5 support,\n" +
			"  which cascades into 8 pre-existing test failures across\n" +
			"  cmd/mpm and internal/scheduler (search JSON envelope,\n" +
			"  memory-quality source stats, projection pointer resolve,\n" +
			"  scheduler drill tick). The canonical race-enabled test\n" +
			"  command is `make test-race`, which carries both the CGO\n" +
			"  FTS5 compile flag and the Go build tag.")
	}

	recipeRE := regexp.MustCompile(
		`(?m)^test-race:[^\n]*\n((?:^[ \t].*\n?)+)`,
	)
	match := recipeRE.FindStringSubmatch(text)
	if len(match) < 2 {
		t.Fatalf("Makefile must define a `test-race:` target recipe (got empty body)")
	}
	recipe := match[1]

	if !strings.Contains(recipe, "-race") {
		t.Errorf("`test-race` recipe must include `-race` flag (got: %q)", recipe)
	}
	if !strings.Contains(recipe, "-tags fts5") {
		t.Errorf("`test-race` recipe must pass `-tags fts5` to go test (got: %q)", recipe)
	}

	// CGO_CFLAGS pattern check: the recipe must reference either the
	// Makefile-level CGO_CFLAGS variable (e.g. `$(CGO_CFLAGS)`) or the
	// literal -DSQLITE_ENABLE_FTS5=1 string. Both are valid; what matters
	// is that FTS5 is enabled at compile time.
	cgoInRecipe := strings.Contains(recipe, "CGO_CFLAGS=") ||
		strings.Contains(recipe, "CGO_CFLAGS ")
	if !cgoInRecipe {
		t.Errorf("`test-race` recipe must set CGO_CFLAGS to enable FTS5 (got: %q)", recipe)
	}
	if !strings.Contains(text, "CGO_CFLAGS := -DSQLITE_ENABLE_FTS5=1") {
		t.Errorf("Makefile must define CGO_CFLAGS:=-DSQLITE_ENABLE_FTS5=1 at the top " +
			"so every test target inherits FTS5 (got Makefile without this definition)")
	}
	if strings.Contains(recipe, "CGO_CFLAGS=") &&
		!strings.Contains(recipe, "$(CGO_CFLAGS)") &&
		!strings.Contains(recipe, "${CGO_CFLAGS}") {
		// Recipe sets CGO_CFLAGS but does not reference the Make variable —
		// risk of drift if the variable definition changes.
		t.Errorf("`test-race` recipe sets CGO_CFLAGS but does not reference the "+
			"Makefile-level `$(CGO_CFLAGS)` variable; risk of drift if the "+
			"FTS5 flag definition changes (got: %q)", recipe)
	}
}

// TestBuildConfig_PrecommitUsesMakefileTestTarget pins the structural
// invariant that scripts/pre-commit invokes the test runner through the
// Makefile (which owns the FTS5 flag set) rather than calling
// `go test` directly (which silently relies on a warm build cache to
// have already compiled go-sqlite3 with FTS5).
//
// Why this matters: a fresh checkout (CI, new contributor) has no
// build cache, so a direct `go test -tags fts5 ./...` invocation
// compiles mattn/go-sqlite3 with whatever CGO_CFLAGS happens to be in
// the environment — typically unset. Without FTS5 compiled in, the
// FTS5-dependent code paths in internal/core (FTS5 table creation,
// shared triggers, hybrid search) panic with "no such module: fts5"
// or return bm25-fts-only when bm25-strong was expected. The error
// surfaces in unrelated diffs and looks like a regression in the
// change being committed.
//
// The fix: pre-commit invokes `make test-core-precommit`, a Makefile
// target whose recipe inlines both CGO_CFLAGS=$(CGO_CFLAGS) and
// -tags fts5. The Makefile is the single source of truth for the
// flag set; TestBuildConfig_MakefileHasRaceDetectorTarget (above)
// already pins the Makefile's test-race recipe; this test pins the
// parallel pre-commit target AND locks the hook to use it.
//
// Regression target: this test fails when scripts/pre-commit
// re-introduces a direct `go test ... -run ...` invocation, OR when
// the Makefile loses the `test-core-precommit` target, OR when the
// target's recipe loses CGO_CFLAGS / -tags fts5.
func TestBuildConfig_PrecommitUsesMakefileTestTarget(t *testing.T) {
	root := findRepoRoot(t)

	// 1. The Makefile must define `test-core-precommit` with the FTS5
	//    flag set in the recipe (mirrors `test` and `test-race`).
	makefilePath := filepath.Join(root, "Makefile")
	makeData, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read %s: %v", makefilePath, err)
	}
	makeText := string(makeData)

	phonyRE := regexp.MustCompile(`(?m)^\.PHONY:[^\n]*\btest-core-precommit\b`)
	if !phonyRE.MatchString(makeText) {
		t.Errorf("Makefile `.PHONY` line must include `test-core-precommit` target.\n" +
			"  Why: scripts/pre-commit invokes this target so the FTS5 flag\n" +
			"  set (CGO_CFLAGS + -tags fts5) has a single source of truth.\n" +
			"  Bare `go test -short -tags fts5 ./...` in pre-commit relies\n" +
			"  on the build cache being warm; on a fresh checkout it fails\n" +
			"  with confusing errors that look like unrelated regressions.")
	}

	recipeRE := regexp.MustCompile(
		`(?m)^test-core-precommit:[^\n]*\n((?:^[ \t].*\n?)+)`,
	)
	match := recipeRE.FindStringSubmatch(makeText)
	if len(match) < 2 {
		t.Fatalf("Makefile must define a `test-core-precommit:` target recipe (got empty body)")
	}
	recipe := match[1]

	if !strings.Contains(recipe, "-tags fts5") {
		t.Errorf("`test-core-precommit` recipe must pass `-tags fts5` to go test (got: %q)", recipe)
	}
	if !strings.Contains(recipe, "$(CGO_CFLAGS)") &&
		!strings.Contains(recipe, "${CGO_CFLAGS}") {
		t.Errorf("`test-core-precommit` recipe must reference $(CGO_CFLAGS) "+
			"so the FTS5 C-level compile flag is set (got: %q)", recipe)
	}

	// 2. scripts/pre-commit must invoke `make test-core-precommit`
	//    AND must not contain a live `go test ... -run ...` line for
	//    the subset (the historical pattern that this commit retires).
	precommitPath := filepath.Join(root, "scripts", "pre-commit")
	hookData, err := os.ReadFile(precommitPath)
	if err != nil {
		t.Fatalf("read %s: %v", precommitPath, err)
	}
	hookText := string(hookData)

	if !strings.Contains(hookText, "make test-core-precommit") {
		t.Errorf("scripts/pre-commit must invoke `make test-core-precommit` " +
			"so the FTS5 flag set is owned by the Makefile (got pre-commit " +
			"without this invocation)")
	}

	// Strip comment lines (start with `#`) before scanning for live
	// `go test` invocations. The hook intentionally documents the
	// historical pattern in a comment block; only live code is gated.
	var liveLines []string
	for _, line := range strings.Split(hookText, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		liveLines = append(liveLines, line)
	}
	liveText := strings.Join(liveLines, "\n")

	if strings.Contains(liveText, "go test") && strings.Contains(liveText, "-run") {
		t.Errorf("scripts/pre-commit contains a live `go test ... -run ...` line.\n" +
			"  Direct `go test` in the hook bypasses the Makefile's FTS5 flag\n" +
			"  set and silently breaks on a fresh checkout. Route the test\n" +
			"  subset through `make test-core-precommit` instead.\n" +
			"  (Comments documenting the historical pattern are allowed.)")
	}
}

// findRepoRoot walks up from the test's working directory until it
// finds the directory containing both go.mod and Makefile. The MPM
// repository uses a non-default test working dir so the simple
// `os.Getwd()` approach used by some packages does not work.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err1 := os.Stat(filepath.Join(dir, "go.mod")); err1 == nil {
			if _, err2 := os.Stat(filepath.Join(dir, "Makefile")); err2 == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate repo root (no go.mod + Makefile found)")
	return ""
}

// ---------------------------------------------------------------------------
// Nested tools module: gate wiring
//
// internal/core/tools is a separate Go module. `go test ./...` from the
// root module does not descend into it, so for as long as nothing named
// it explicitly, a fully green repository-wide test result said nothing
// about it. That is how three source-scanning guards in that module sat
// permanently disarmed: TestOutputPolicy_OnlyMCPEnforces skipped on
// every run (its path resolution used an unset MPM_WORKSPACE), and the
// parity and adapter guards failed open on an empty discovered
// population. They were repaired on 2026-09-29 and are fast, but a
// repaired guard that no gate executes is still not a guard.
//
// The tests below pin the wiring. They are deliberately behavioural
// where that is possible — they run `make -n`, run `go test -list`, and
// run the installer — rather than matching recipe text, so they fail
// when the wiring breaks and not when the recipe is merely reworded.
// ---------------------------------------------------------------------------

// validTargetNameRE matches a Makefile target/header name.
var validTargetNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// makeTargetNames returns the set of target names the Makefile defines,
// parsed from `^<name>:` recipe headers. Targets written with a
// `VAR=value` override header (`test-race:` here uses the plain form,
// but `install:` style headers exist elsewhere) are matched on the part
// before any `:`.
func makeTargetNames(t *testing.T, makefilePath string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read %s: %v", makefilePath, err)
	}
	names := map[string]bool{}
	// Parsed line-wise rather than with a regex: Go's RE2 has no negative
	// lookahead, and the thing being excluded (`NAME := value`, optionally
	// written `NAME:=value` with no space) is a variable assignment that
	// must not be mistaken for a target. Checking the character after the
	// colon directly is clearer than encoding "colon not followed by ="
	// in a pattern.
	for _, line := range strings.Split(string(data), "\n") {
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		name := line[:colon]
		if name == "" || strings.HasPrefix(name, " ") || strings.HasPrefix(name, "\t") {
			continue // indented: a recipe line, not a header
		}
		if !validTargetNameRE.MatchString(name) {
			continue
		}
		if strings.HasPrefix(line[colon+1:], "=") {
			continue // `NAME:=value` is an assignment
		}
		names[name] = true
	}
	if len(names) == 0 {
		t.Fatalf("parsed zero Makefile targets from %s; the header regex is broken and every "+
			"assertion below would pass vacuously", makefilePath)
	}
	return names
}

// TestBuildConfig_ToolsModuleIsInFullTestGates is the core wiring
// contract: the nested tools module must be reachable from the
// repository's real test paths, not merely commented about.
//
// Checked by expanding the recipes with `make -n` rather than by reading
// the Makefile as text, so a target that exists but is not actually
// invoked by `make test` is caught, and a rewording that does not change
// behaviour is not.
func TestBuildConfig_ToolsModuleIsInFullTestGates(t *testing.T) {
	root := findRepoRoot(t)

	for _, target := range []string{"test", "test-race"} {
		t.Run(target, func(t *testing.T) {
			out, err := runMakeDryRun(t, root, target)
			if err != nil {
				t.Fatalf("`make -n %s` failed: %v\n%s", target, err, out)
			}
			// The tools module is a separate module, so its tests can only
			// be reached by an explicit `cd` into it. Accept either the
			// module path or a delegating sub-make.
			if !strings.Contains(out, "internal/core/tools") {
				t.Errorf("`make -n %s` does not reference internal/core/tools.\n"+
					"  A green `%s` therefore says nothing about the nested tools\n"+
					"  module, whose guards cover MCP output-policy enforcement,\n"+
					"  registry/dispatcher parity, and cross-language adapter\n"+
					"  schema drift.\n  Expanded recipe:\n%s", target, target, out)
			}
		})
	}
}

// TestBuildConfig_ToolsPrecommitTargetExists pins that the fast guard
// subset has a real, FTS5-correct Make target, since the pre-commit hook
// routes through the Makefile for the flag set (the reason
// TestBuildConfig_PrecommitUsesMakefileTestTarget exists).
func TestBuildConfig_ToolsPrecommitTargetExists(t *testing.T) {
	root := findRepoRoot(t)
	makefilePath := filepath.Join(root, "Makefile")

	names := makeTargetNames(t, makefilePath)
	for _, want := range []string{"test-tools-precommit", "test-tools"} {
		if !names[want] {
			t.Errorf("Makefile has no `%s` target. The nested tools module needs both a fast "+
				"guard subset target and a full-module target.", want)
		}
	}

	// The pre-commit recipe must carry the same FTS5 flag discipline as
	// every other test target; a nested module is exactly the place
	// someone assumes `./...` already covered it and drops the flags.
	recipe := makeRecipe(t, makefilePath, "test-tools-precommit")
	if !strings.Contains(recipe, "-tags fts5") {
		t.Errorf("`test-tools-precommit` recipe must pass `-tags fts5` (got: %q)", recipe)
	}
	if !strings.Contains(recipe, "$(CGO_CFLAGS)") && !strings.Contains(recipe, "${CGO_CFLAGS}") {
		t.Errorf("`test-tools-precommit` recipe must reference $(CGO_CFLAGS) so FTS5 is "+
			"compiled in (got: %q)", recipe)
	}
	if !strings.Contains(recipe, "internal/core/tools") {
		t.Errorf("`test-tools-precommit` recipe must enter the nested module directory "+
			"(got: %q)", recipe)
	}
}

// TestBuildConfig_ToolsGuardSubsetIsNonVacuous is the behavioural half
// of the pre-commit gate: a `-run` regex that matches nothing produces
// a PASSING `go test`, so a typo or a renamed test would silently turn
// the gate off exactly the way the original guards were turned off.
//
// This expands the subset regex the Makefile actually uses and requires
// it to resolve to real tests, including the three guards the gate
// exists to run. It duplicates no recipe text — it reads the regex out
// of the Makefile and asks the toolchain what it matches.
func TestBuildConfig_ToolsGuardSubsetIsNonVacuous(t *testing.T) {
	root := findRepoRoot(t)
	makefilePath := filepath.Join(root, "Makefile")

	// Pull the guard regex out of the Makefile rather than restating it,
	// so this test cannot pass while the recipe uses a different set.
	regex, ok := makeVariable(t, makefilePath, "TOOLS_GUARD_RUN")
	if !ok {
		t.Fatal("Makefile does not define TOOLS_GUARD_RUN. The pre-commit guard subset must be " +
			"a single named Make variable so this contract and the recipe cannot drift apart.")
	}
	required, ok := makeVariable(t, makefilePath, "TOOLS_REQUIRED_GUARDS")
	if !ok {
		t.Fatal("Makefile does not define TOOLS_REQUIRED_GUARDS (the guards that must never drop " +
			"out of the subset).")
	}

	matched := listMatchingTests(t, root, regex)
	if len(matched) == 0 {
		t.Fatalf("the pre-commit guard regex %q matches ZERO tests. `go test -run` with no "+
			"matches exits 0, so the gate would pass without running anything.", regex)
	}

	for _, guard := range strings.Fields(required) {
		found := false
		for _, m := range matched {
			if m == guard {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("required guard %q is not matched by TOOLS_GUARD_RUN=%q.\n"+
				"  Matched tests: %v\n"+
				"  A guard that is silently dropped from the subset is worse than no gate, "+
				"because the gate still reports green.", guard, regex, matched)
		}
	}
}

// TestBuildConfig_InstallHooksInstallsCanonicalHook exercises the
// installer for real rather than asserting on its recipe text.
//
// The hook's own header told users to run `make install-hooks` for as
// long as the Makefile had no such target. The recipe-string style of
// check could have caught that; running the target is stronger, and it
// also pins the properties that actually matter: it installs the
// TRACKED scripts/pre-commit, the installed copy is executable, and
// running it twice is a no-op.
//
// GIT_HOOKS_DIR is redirected into t.TempDir(), so this never writes to
// the operator's real .git/hooks.
func TestBuildConfig_InstallHooksInstallsCanonicalHook(t *testing.T) {
	root := findRepoRoot(t)
	makefilePath := filepath.Join(root, "Makefile")
	if !makeTargetNames(t, makefilePath)["install-hooks"] {
		t.Fatal("Makefile has no `install-hooks` target, but scripts/pre-commit instructs " +
			"users to run it. Either add the target or fix the instruction.")
	}

	scriptPath := filepath.Join(root, "scripts", "pre-commit")
	want, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}

	sandbox := t.TempDir()
	// Run twice: the second run must not fail, and must leave the same
	// bytes. `cp` is idempotent, but a target that appended or that
	// required a clean slate would break here.
	for attempt := 1; attempt <= 2; attempt++ {
		out, err := runMake(t, root, sandbox, "install-hooks")
		if err != nil {
			t.Fatalf("`make install-hooks` failed on attempt %d: %v\n%s", attempt, err, out)
		}

		installed := filepath.Join(sandbox, "pre-commit")
		got, err := os.ReadFile(installed)
		if err != nil {
			t.Fatalf("attempt %d: install-hooks did not create %s: %v", attempt, installed, err)
		}
		if string(got) != string(want) {
			t.Errorf("attempt %d: installed hook differs from tracked scripts/pre-commit.\n"+
				"  The installed hook is what git actually runs; if it drifts from the tracked\n"+
				"  source, local gates and CI gates are testing different things.", attempt)
		}

		info, err := os.Stat(installed)
		if err != nil {
			t.Fatalf("stat installed hook: %v", err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("attempt %d: installed hook is not executable (mode %v). git silently "+
				"skips a non-executable hook, so the gates would not run at all.", attempt, info.Mode().Perm())
		}
	}
}

// TestBuildConfig_PrecommitAdvertisesOnlyExistingMakeTargets closes the
// gap that let `make install-hooks` sit advertised in the hook header
// for as long as it did: nothing checked that a command the hook tells
// users to run actually exists.
//
// Comments are scanned deliberately. The stale instruction was in a
// comment, not live code, so a live-lines-only check — the one
// TestBuildConfig_PrecommitUsesMakefileTestTarget uses for its `go test`
// assertion — would have missed it.
func TestBuildConfig_PrecommitAdvertisesOnlyExistingMakeTargets(t *testing.T) {
	root := findRepoRoot(t)
	makefilePath := filepath.Join(root, "Makefile")
	names := makeTargetNames(t, makefilePath)

	data, err := os.ReadFile(filepath.Join(root, "scripts", "pre-commit"))
	if err != nil {
		t.Fatalf("read scripts/pre-commit: %v", err)
	}

	// `make <target>` / `$(MAKE) <target>` as an instruction to the reader.
	advertised := regexp.MustCompile(`(?:make|\$\(MAKE\))\s+([a-z][a-z0-9-]*)`)
	seen := map[string]bool{}
	for _, m := range advertised.FindAllStringSubmatch(string(data), -1) {
		seen[m[1]] = true
	}
	if len(seen) == 0 {
		t.Fatalf("found no `make <target>` instructions in scripts/pre-commit; the scan regex " +
			"is broken and this guard would pass vacuously")
	}

	var missing []string
	for target := range seen {
		if !names[target] {
			missing = append(missing, target)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("scripts/pre-commit tells users to run `make %s`, but the Makefile defines no "+
			"such target.\n  An advertised command that does not exist sends the reader to a "+
			"\"command not found\" instead of installing the hook — which is how the hook went "+
			"uninstalled on any host that followed its own instructions.", strings.Join(missing, "`, `make "))
	}
}

// ---------------------------------------------------------------------------
// Helpers for the gate-wiring tests above.
//
// Every helper here executes a real command (make, go test -list) rather
// than pattern-matching recipe text. Recipe text is what these tests
// exist to stop depending on: a rewording that preserves behaviour
// should not fail a gate, and a target that exists but is never invoked
// should.
// ---------------------------------------------------------------------------

// runMakeDryRun expands a target's recipe without running it (`make -n`)
// and returns the expanded text.
func runMakeDryRun(t *testing.T, dir string, target string) (string, error) {
	t.Helper()
	return runMake(t, dir, "", "-n", target)
}

// runMake runs `make` in dir. When hooksDir is non-empty it is passed as
// GIT_HOOKS_DIR, which redirects install-hooks into a sandbox so the
// operator's real .git/hooks is never written.
func runMake(t *testing.T, dir, hooksDir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("make", args...)
	cmd.Dir = dir
	if hooksDir != "" {
		cmd.Env = append(os.Environ(), "GIT_HOOKS_DIR="+hooksDir)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// makeRecipe returns the raw recipe body lines for a Makefile target.
func makeRecipe(t *testing.T, makefilePath, target string) string {
	t.Helper()
	data, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read %s: %v", makefilePath, err)
	}
	// Same line-wise approach as makeTargetNames: locate the header, then
	// take the following run of indented lines as its recipe body.
	lines := strings.Split(string(data), "\n")
	header := target + ":"
	var body []string
	found := false
	for _, line := range lines {
		if found {
			if line != "" && (line[0] == ' ' || line[0] == '\t') {
				body = append(body, line)
				continue
			}
			break
		}
		if strings.HasPrefix(line, header) && !strings.HasPrefix(line[len(header):], "=") {
			found = true
		}
	}
	if len(body) == 0 {
		t.Fatalf("Makefile defines no recipe body for `%s`", target)
	}
	return strings.Join(body, "\n")
}

// makeVariable returns the value of a `NAME := value` / `NAME = value`
// Makefile assignment, with surrounding quotes and spaces trimmed.
func makeVariable(t *testing.T, makefilePath, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read %s: %v", makefilePath, err)
	}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*:?=\s*(.+)$`)
	m := re.FindStringSubmatch(string(data))
	if len(m) < 2 {
		return "", false
	}
	return strings.Trim(strings.TrimSpace(m[1]), `"'`), true
}

// listMatchingTests asks the toolchain which tests a `-run` regex
// selects, by running `go test -list`. This is the check that makes an
// empty-regex gate impossible: `go test -run` exits 0 when nothing
// matches, so the emptiness has to be detected outside the runner.
func listMatchingTests(t *testing.T, repoRoot, regex string) []string {
	t.Helper()
	cmd := exec.Command("go", "test", "-tags", "fts5", "-list", regex, "./")
	cmd.Dir = filepath.Join(repoRoot, "internal", "core", "tools")
	cmd.Env = append(os.Environ(),
		"CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1",
		"CGO_LDFLAGS=-lm",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("`go test -list %s` in internal/core/tools failed: %v\n%s", regex, err, out)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// `go test -list` prints one test name per line, then a trailing
		// `ok <pkg>` status line that is not a test.
		if strings.HasPrefix(line, "Test") {
			names = append(names, line)
		}
	}
	sort.Strings(names)
	return names
}
