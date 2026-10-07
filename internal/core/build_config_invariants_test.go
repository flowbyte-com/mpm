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

// makeVariable returns the value of a `NAME := value` / `NAME = value` /
// `NAME ?= value` Makefile assignment, with surrounding quotes and spaces
// trimmed. The conditional `?=` form matters here: PREFIX is declared
// with it, so a reader that only understood `:=` reports PREFIX as
// undefined rather than reading its real value.
func makeVariable(t *testing.T, makefilePath, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read %s: %v", makefilePath, err)
	}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*(?:\?|:)?=\s*(.+)$`)
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

// ---------------------------------------------------------------------------
// Gate failure propagation
// ---------------------------------------------------------------------------

// makeInstallRecipe returns the recipe body of the Makefile's `install`
// target, with the recipe prefix (@, -) and line continuations preserved
// exactly as make would hand them to the shell.
//
// Extracting the REAL recipe (rather than restating its shape in the test)
// is the point: the defect this guards against is a property of those exact
// bytes. A paraphrase in the test could keep passing while the Makefile
// regressed, or vice versa.
func makeInstallRecipe(t *testing.T) string {
	t.Helper()
	path := filepath.Join(findRepoRoot(t), "Makefile")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")

	start := -1
	for i, l := range lines {
		// Match the `install` target by its load-bearing prerequisites rather
		// than by exact text. Pinning the whole line would fail every time a
		// legitimate prerequisite is added — and `install-runtime-assets` is
		// exactly such an addition. What must never change is that install
		// still builds first and still promotes binaries; both are asserted
		// below, and separately by the install-sync tests.
		if !strings.HasPrefix(l, "install:") {
			continue
		}
		prereqs := strings.Fields(strings.TrimPrefix(l, "install:"))
		hasBuild, hasPromote := false, false
		for _, p := range prereqs {
			switch p {
			case "build":
				hasBuild = true
			case "refresh-installed":
				hasPromote = true
			}
		}
		if hasBuild && hasPromote {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("Makefile no longer has an `install` target depending on both " +
			"`build` and `refresh-installed`; update makeInstallRecipe to match the current shape.")
	}

	var body []string
	for i := start; i < len(lines); i++ {
		l := lines[i]
		// A non-indented, non-empty line ends the recipe.
		if l != "" && !strings.HasPrefix(l, "\t") && !strings.HasPrefix(l, " ") {
			break
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		body = append(body, strings.TrimPrefix(l, "\t"))
	}
	if len(body) == 0 {
		t.Fatalf("`install` target has an empty recipe")
	}
	return strings.Join(body, "\n")
}

// TestBuildConfig_InstallSyncHasNoMaskedFailure pins the invariant that
// `make install` cannot report success when a mandatory step failed.
//
// The defect: the five `install -m755` calls were each suffixed `|| true`
// AND joined with `;` inside a single `if ... fi` shell invocation. A recipe
// line's exit status is the status of the LAST command in it, so after every
// copy failed the trailing `echo` still exited 0, the following recipe line
// printed "✓ Canonical binaries at ...", and `make install` returned 0.
//
// Note that removing `|| true` ALONE would not have fixed it: the `;` chain
// would still swallow the failure the same way. Both properties are asserted
// below — the absence of the error-swallowing suffix, and the presence of
// `&&` sequencing — because either one alone is insufficient.
//
// Why `make install` specifically: it is a validation target. Its closing
// line makes a factual claim about five binaries being present in
// $(PREFIX)/bin. That claim must not be printed over a partial install.
//
// Regression target: this test fails if the `install` recipe regains
// `|| true` on a mandatory step, or stops checking the transaction's
// result.
func TestBuildConfig_InstallSyncHasNoMaskedFailure(t *testing.T) {
	recipe := makeInstallRecipe(t)

	// Promotion is one transaction call now, not a chain of copies. Locate
	// the promotion invocation and everything after it, so the assertions
	// do not accidentally match unrelated `||` uses in the target.
	promoBranch := ""
	inPromo := false
	for _, line := range strings.Split(recipe, "\n") {
		if strings.Contains(line, "mpm_promote_binaries") {
			inPromo = true
		}
		if inPromo {
			promoBranch += line + "\n"
		}
	}
	if strings.TrimSpace(promoBranch) == "" {
		t.Fatalf("`install` recipe no longer calls mpm_promote_binaries; " +
			"update this test to match the current shape.")
	}

	if strings.Contains(promoBranch, "|| true") {
		t.Errorf("`make install` re-introduced `|| true` around the promotion.\n"+
			"  A recipe line exits with the status of its LAST command, so swallowing\n"+
			"  the failure lets the trailing echo report success over a rolled-back\n"+
			"  deploy.  offending branch:\n%s", indentForMessage(promoBranch))
	}

	// The result must be captured and checked. Capturing `$?` and never
	// testing it is the `;`-chain bug wearing a different hat. Inside a
	// `bash -c '...'` recipe body make escapes `$` as `$$`, so both spellings
	// are legitimate here.
	if !strings.Contains(promoBranch, "_rc=") {
		t.Errorf("`make install` does not capture the promotion's exit status.\n"+
			"  Without it a failed transaction would be reported as a successful\n"+
			"  install over a rolled-back binary set.  offending branch:\n%s",
			indentForMessage(promoBranch))
	}
	if !strings.Contains(promoBranch, "_rc -ne 0") {
		t.Errorf("`make install` does not test the promotion's exit status.\n"+
			"  A non-zero transaction must abort before the success line.\n"+
			"  offending branch:\n%s", indentForMessage(promoBranch))
	}

	// The whole body runs under `bash -e` so an unexpected non-zero between
	// the start of the block and the explicit check aborts rather than
	// sliding through.
	if !strings.Contains(recipe, "bash -e -c") {
		t.Errorf("`make install` no longer runs its body under `bash -e`.\n"+
			"  The promotion library uses bash arrays, and `-e` keeps any\n"+
			"  unexpected failure from being reported as a completed install.\n"+
			"  recipe:\n%s", indentForMessage(recipe))
	}
}

// TestBuildConfig_InstallSyncFailsWhenCopyFails is the empirical half of the
// guard: it executes the REAL `install` recipe with a stubbed `install`
// command that always fails, and asserts the enclosing target exits non-zero
// and never prints its success claim.
//
// This is what makes the invariant checkable rather than merely asserted. A
// static scan can only confirm the Makefile does not contain a known-bad
// token; it cannot confirm the shell actually propagates. This test runs the
// shell.
//
// Isolation: the recipe body is written to a temp Makefile under t.TempDir()
// with PREFIX redirected there, and a stub `install` earlier on PATH always
// exits 1. No real $(PREFIX), no real $HOME, no live database, and neither
// `build` nor `refresh-installed` is invoked.
func TestBuildConfig_InstallSyncFailsWhenCopyFails(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make not available: %v", err)
	}

	tmp := t.TempDir()
	stubDir := filepath.Join(tmp, "stubbin")
	if err := os.MkdirAll(stubDir, 0o755); err != nil {
		t.Fatalf("mkdir stubbin: %v", err)
	}
	stub := filepath.Join(stubDir, "install")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho '[stub] refusing to install' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	// A non-symlinked build dir and a non-symlinked PREFIX, both inside the
	// temp sandbox, so the copy branch is the one that executes.
	buildDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatalf("mkdir buildDir: %v", err)
	}
	prefix := filepath.Join(tmp, "prefix")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		t.Fatalf("mkdir prefix: %v", err)
	}

	recipe := makeInstallRecipe(t)

	// The real recipe, verbatim, under a probe target. Only the variable
	// definitions and the target name are ours; the shell text is the
	// Makefile's own.
	var b strings.Builder
	b.WriteString("BINARY_NAME := mpm\n")
	b.WriteString("MCP_BINARY  := mpm-mcp\n")
	b.WriteString("SCHED_BINARY := mpm-scheduler\n")
	b.WriteString("CRITIC_BINARY := mpm-critic\n")
	b.WriteString("TELEMETRY_BINARY := mpm-telemetry\n")
	b.WriteString("BUILD_DIR   := " + buildDir + "\n")
	b.WriteString("PREFIX      := " + prefix + "\n")
	b.WriteString("\n.PHONY: probe\nprobe:\n")
	for _, line := range strings.Split(recipe, "\n") {
		b.WriteString("\t" + line + "\n")
	}

	probeMakefile := filepath.Join(tmp, "Makefile.probe")
	if err := os.WriteFile(probeMakefile, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write probe makefile: %v", err)
	}

	cmd := exec.Command("make", "-f", probeMakefile, "probe")
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Errorf("`make install` reported SUCCESS while every binary copy failed.\n"+
			"  This is the masked-failure defect: a mandatory step failed but the\n"+
			"  enclosing target exited 0. Chain the copy steps with `&&`.\n"+
			"  output:\n%s", indentForMessage(string(out)))
	}

	if strings.Contains(string(out), "✓ Canonical binaries at") {
		t.Errorf("`make install` printed its success claim despite a failed copy.\n"+
			"  The claim asserts all five binaries are present in PREFIX/bin, so it\n"+
			"  must not be printed when the sync failed.\n"+
			"  output:\n%s", indentForMessage(string(out)))
	}
}

func indentForMessage(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Root-module package coverage in the canonical gates
// ---------------------------------------------------------------------------

// TestBuildConfig_CriticInCanonicalGates pins that internal/critic is
// executed by BOTH canonical Go gates.
//
// WHY THIS NEEDS PINNING. The gates enumerate root-module packages
// individually (./cmd/..., ./internal/telemetry/..., internal/core,
// ./internal/scheduler/...) rather than a single ./... , because
// internal/core is a separate nested module. Any package added under
// internal/ is therefore INVISIBLE to both gates until someone edits
// this Makefile by hand.
//
// That is not hypothetical. internal/critic was omitted for its entire
// life: every Tranche-5 fix to StaleMemoryHunt — including the
// cumulative-active-uptime settling rewrite and its 15 regression
// tests — passed `make test` and `make release-gate` without a single
// critic test executing. The gates were green while the code they were
// supposed to cover never ran.
//
// Both gates are checked, not just `test`, because `release-gate` is
// defined as `test-race test-release`, so a package absent from
// test-race is absent from release-gate too (test-release only runs
// the separate release_acceptance module).
//
// The match is scoped to each target's OWN recipe region, extracted
// the same way TestBuildConfig_MakefileHasRaceDetectorTarget does it.
// A whole-file substring search would be satisfied by a comment
// mentioning the package, which is precisely the vacuous pass this
// guard exists to prevent.
func TestBuildConfig_CriticInCanonicalGates(t *testing.T) {
	makefilePath := filepath.Join(findRepoRoot(t), "Makefile")
	data, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read %s: %v", makefilePath, err)
	}
	text := string(data)

	for _, target := range []struct {
		name    string
		flag    string
		example string
	}{
		{"test", "-tags fts5", "make test"},
		{"test-race", "-race -tags fts5", "make test-race"},
	} {
		t.Run(target.name, func(t *testing.T) {
			recipeRE := regexp.MustCompile(
				`(?m)^` + regexp.QuoteMeta(target.name) + `:[^\n]*\n((?:^[ \t].*\n?)+)`,
			)
			match := recipeRE.FindStringSubmatch(text)
			if len(match) < 2 {
				t.Fatalf("Makefile must define a `%s:` target recipe (got empty body)", target.name)
			}
			recipe := match[1]

			if !strings.Contains(recipe, "./internal/critic/...") {
				t.Errorf("`%s` recipe does not execute ./internal/critic/...\n"+
					"  The Critic's tests are silently skipped, so a green %s can\n"+
					"  mean the critic package never ran. Add:\n"+
					"    CGO_CFLAGS=$(CGO_CFLAGS) CGO_LDFLAGS=$(CGO_LDFLAGS) $(GO) test %s -v ./internal/critic/...\n"+
					"  recipe:\n%s", target.name, target.example, target.flag, indentForMessage(recipe))
			}

			// The critic line must carry the same build configuration as its
			// neighbours. A critic run compiled without FTS5 would fail
			// differently from the rest of the root module and could be
			// "fixed" by dropping the flags, reintroducing exactly the drift
			// TestBuildConfig_MakefileHasRaceDetectorTarget guards against.
			criticLine := ""
			for _, line := range strings.Split(recipe, "\n") {
				if strings.Contains(line, "./internal/critic/...") {
					criticLine = line
					break
				}
			}
			if criticLine == "" {
				return // already reported above
			}
			for _, required := range []string{
				"$(CGO_CFLAGS)", "$(CGO_LDFLAGS)", "$(GO) test",
				"-tags fts5", "-v",
			} {
				if !strings.Contains(criticLine, required) {
					t.Errorf("critic line in `%s` is missing %q; it must use the same\n"+
						"  build configuration as every other root-module package (got: %q)",
						target.name, required, criticLine)
				}
			}
			if strings.Contains(target.name, "race") && !strings.Contains(criticLine, "-race") {
				t.Errorf("critic line in `%s` must pass -race (got: %q)", target.name, criticLine)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Build artifact vs installed binary — regression guards A–F
//
// THE DEFECT THESE GUARD. Before the build/install split, BUILD_DIR was
// `bin`, which IS $(PREFIX)/bin whenever the checkout is the install
// prefix — the canonical layout, `~/.mpm`. So:
//
//     make build == overwrite the binaries the running services execute
//
// The split moves developer artifacts to `.build/bin` and makes `make
// install` the only thing that writes $(PREFIX)/bin.
//
// Each guard below pins ONE property of that split, and each is written
// so that reverting the corresponding half of the fix turns it red. A
// guard that cannot fail is worse than no guard, because it reads as
// coverage.
//
// Region-scoped, not whole-file: makeRecipe/makeVariable extract the
// exact target region, so a comment elsewhere in the Makefile cannot
// satisfy (or break) a guard. That distinction is the whole reason the
// existing helpers in this file exist.
// ---------------------------------------------------------------------------

// makeFilePath returns the path to the repository Makefile.
func makeFilePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(findRepoRoot(t), "Makefile")
}

// TestBuildConfig_BuildDirIsAScratchSubdirectoryNotTheInstall — GUARD A
//
// BUILD_DIR must not be `bin`, must not be `$(PREFIX)/bin`, and must not
// be any path that is the live install. Checked on RESOLVED absolute
// paths, because the pre-fix guard compared literal strings ("bin" vs
// "/home/v/.mpm/bin") which are never equal even when they name the
// same directory — that comparison passed while the two were one dir.
func TestBuildConfig_BuildDirIsAScratchSubdirectoryNotTheInstall(t *testing.T) {
	repoRoot := findRepoRoot(t)
	makefile := makeFilePath(t)

	buildDir, ok := makeVariable(t, makefile, "BUILD_DIR")
	if !ok {
		t.Fatalf("Makefile must define BUILD_DIR")
	}
	prefix, ok := makeVariable(t, makefile, "PREFIX")
	if !ok {
		t.Fatalf("Makefile must define PREFIX")
	}

	absBuild := resolveMakePath(t, repoRoot, buildDir)
	absPrefixBin := resolveMakePath(t, repoRoot, prefix+"/bin")

	if absBuild == absPrefixBin {
		t.Fatalf("BUILD_DIR resolves to %s, which IS $(PREFIX)/bin.\n"+
			"  This re-merges the checkout and the live install: `make build` would\n"+
			"  overwrite the binaries systemd ExecStart runs, so an ordinary build\n"+
			"  IS a deployment.\n"+
			"  BUILD_DIR must be a disposable scratch directory inside the checkout.",
			absBuild)
	}
	if filepath.Base(absBuild) == "bin" && filepath.Dir(absBuild) == repoRoot {
		t.Fatalf("BUILD_DIR resolves to the checkout's own bin/ (%s).\n"+
			"  When the checkout is the install prefix (~/.mpm) that directory IS\n"+
			"  the live install. Use a dot-prefixed scratch dir such as `.build/bin`.",
			absBuild)
	}
	// Build output must be local to THIS checkout (brief §4: no /tmp, no
	// symlink into the install), so it must live under the checkout root.
	rel, err := filepath.Rel(repoRoot, absBuild)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		t.Fatalf("BUILD_DIR resolves to %s, which is OUTSIDE the checkout (%s).\n"+
			"  Build output must stay local to each checkout/worktree so parallel\n"+
			"  worktrees do not clobber each other's artifacts, and so it can never\n"+
			"  be the install root.", absBuild, repoRoot)
	}
	if rel == "." {
		t.Fatalf("BUILD_DIR resolves to the checkout root itself (%s)", absBuild)
	}
}

// resolveMakePath turns a Makefile path value into an absolute path,
// expanding the `$(HOME)` reference PREFIX carries. Only `$(VAR)` forms
// that appear in the Makefile's own path variables are expanded; anything
// else is left literal rather than guessed at.
func resolveMakePath(t *testing.T, repoRoot, value string) string {
	t.Helper()
	v := strings.TrimSpace(value)
	if v == "" {
		t.Fatalf("empty Makefile path value")
	}
	// Expand the environment references the Makefile's own path variables
	// use. Anything still unresolved afterwards is a reference this helper
	// cannot evaluate faithfully; failing loudly beats resolving half of it.
	v = strings.ReplaceAll(v, "$(HOME)", os.Getenv("HOME"))
	v = strings.ReplaceAll(v, "${HOME}", os.Getenv("HOME"))
	if strings.Contains(v, "$(") {
		t.Fatalf("Makefile path value %q still contains an unexpanded reference after "+
			"HOME substitution; this helper only expands $(HOME)", value)
	}
	if !filepath.IsAbs(v) {
		v = filepath.Join(repoRoot, v)
	}
	// EvalSymlinks on a not-yet-existing path fails, so resolve the parent
	// when needed and re-append the leaf.
	abs, err := filepath.Abs(v)
	if err != nil {
		t.Fatalf("resolve %q: %v", v, err)
	}
	if _, err := os.Lstat(abs); err == nil {
		if resolved, rErr := filepath.EvalSymlinks(abs); rErr == nil {
			return resolved
		}
		return abs
	}
	parent := filepath.Dir(abs)
	if resolved, rErr := filepath.EvalSymlinks(parent); rErr == nil {
		return filepath.Join(resolved, filepath.Base(abs))
	}
	return abs
}

// TestBuildConfig_BuildRecipeWritesAllFiveToBuildDir — GUARD B
//
// Every `go build -o` in the build target must target $(BUILD_DIR).
// The defect shape is a single stray `-o bin/mpm-scheduler` in a recipe
// that is otherwise correct — invisible to any check that only looks at
// BUILD_DIR's value.
func TestBuildConfig_BuildRecipeWritesAllFiveToBuildDir(t *testing.T) {
	makefile := makeFilePath(t)
	recipe := makeRecipe(t, makefile, "build")

	outRE := regexp.MustCompile(`-o\s+(\S+)\s+\./cmd/(\S+)`)
	matches := outRE.FindAllStringSubmatch(recipe, -1)
	if len(matches) == 0 {
		t.Fatalf("no `go build ... -o <path> ./cmd/...` lines found in the build recipe:\n%s",
			indentForMessage(recipe))
	}

	for _, m := range matches {
		outPath, cmd := m[1], m[2]
		if !strings.HasPrefix(outPath, "$(BUILD_DIR)/") {
			t.Errorf("build target writes %s ./cmd/%s outside $(BUILD_DIR).\n"+
				"  Every developer artifact must land under BUILD_DIR; a path that\n"+
				"  escapes it can land on the live install.", outPath, cmd)
		}
		if !strings.HasPrefix(outPath, "$(BUILD_DIR)/$(") {
			t.Errorf("build output %s is not expressed via a $(BINARY) variable.\n"+
				"  Hardcoding the filename lets the artifact and BUILD_DIR drift apart.", outPath)
		}
	}

	// All five daemons/CLI must be built, not just whichever were added
	// last. A guard on "some -o lines are under BUILD_DIR" would pass on a
	// recipe that dropped mpm-telemetry.
	for _, v := range []string{
		"$(BINARY_NAME)", "$(MCP_BINARY)", "$(SCHED_BINARY)",
		"$(CRITIC_BINARY)", "$(TELEMETRY_BINARY)",
	} {
		if !strings.Contains(recipe, "-o $(BUILD_DIR)/"+v) {
			t.Errorf("build recipe does not produce $(BUILD_DIR)/%s.\n"+
				"  All five binaries must be built to BUILD_DIR; the install step\n"+
				"  promotes all five or fails.\n  recipe:\n%s",
				v, indentForMessage(recipe))
		}
	}
}

// TestBuildConfig_InstallPromotesAllFiveFromBuildDir — GUARD C
//
// `make install` must promote all five from BUILD_DIR to $(PREFIX)/bin, and
// must be all-or-fail. The pre-fix recipe had a coincidence branch that
// printed "bin/ is the canonical location; no copy needed" and skipped the
// copy — which was unreachable in the canonical layout, so the copy ran
// unconditionally, copying files onto themselves. The comment read as a
// proof that nothing needed doing while the install was being rewritten.
//
// The five `install -m755` copies were themselves the original bug: five
// independent in-place writes leave a MIXED release if one fails. Promotion
// is now one transaction in scripts/lib/binary_transaction.sh, shared with
// install.sh so the two surfaces cannot promote in different orders. This
// test therefore pins that the recipe drives THAT transaction with
// $(BUILD_DIR) and $(PREFIX)/bin, that the canonical set still contains all
// five, and that a non-zero transaction result aborts the target.
func TestBuildConfig_InstallPromotesAllFiveFromBuildDir(t *testing.T) {
	makefile := makeFilePath(t)
	recipe := makeRecipe(t, makefile, "install")

	// `make install` is the ONLY thing allowed to write $(PREFIX)/bin, and
	// it must do so through the shared transaction, taking both endpoints
	// explicitly.
	if !strings.Contains(recipe, "mpm_promote_binaries") {
		t.Errorf("install recipe does not call mpm_promote_binaries.\n"+
			"  `make install` is the ONLY thing allowed to write $(PREFIX)/bin, and\n"+
			"  promotion must go through the one transaction that install.sh also\n"+
			"  uses. Five independent copies would leave a mixed release on\n"+
			"  failure.\n  recipe:\n%s", indentForMessage(recipe))
	}
	if !strings.Contains(recipe, `mpm_promote_binaries "$(BUILD_DIR)" "$(PREFIX)/bin"`) {
		t.Errorf("install recipe does not promote $(BUILD_DIR) into $(PREFIX)/bin.\n"+
			"  Both endpoints must be explicit; the build output and the install\n"+
			"  target may not be conflated.  recipe:\n%s", indentForMessage(recipe))
	}
	if !strings.Contains(recipe, "binary_transaction.sh") {
		t.Errorf("install recipe does not source scripts/lib/binary_transaction.sh.\n"+
			"  The transaction must be shared with install.sh so the two promotion\n"+
			"  surfaces cannot drift apart again.  recipe:\n%s",
			indentForMessage(recipe))
	}

	// The canonical promoted set must still be all five. (That the build
	// recipe still PRODUCES all five into $(BUILD_DIR) is pinned separately
	// by TestBuildConfig_BuildRecipeWritesAllFiveToBuildDir; this test is
	// about what the install recipe promotes.)
	lib := filepath.Join(findRepoRoot(t), "scripts", "lib", "binary_transaction.sh")
	libText, err := os.ReadFile(lib)
	if err != nil {
		t.Fatalf("read binary_transaction.sh: %v", err)
	}
	set := ""
	for _, line := range strings.Split(string(libText), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "BT_DEFAULT_BINARIES=") {
			set = strings.TrimSpace(strings.TrimPrefix(
				strings.TrimSpace(line), "BT_DEFAULT_BINARIES="))
			break
		}
	}
	if set == "" {
		t.Fatalf("BT_DEFAULT_BINARIES not found in binary_transaction.sh")
	}
	for _, name := range []string{
		"mpm", "mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry",
	} {
		if !strings.Contains(set, name) {
			t.Errorf("the promoted binary set no longer contains `%s`: %q\n"+
				"  A missing binary means it silently keeps whatever version the\n"+
				"  install already had, while the target still reports success.",
				name, set)
		}
	}

	// The promotion must be a hard error, never a silent skip, when the
	// two directories collide.
	if !strings.Contains(recipe, "exit 1") {
		t.Errorf("install recipe has no hard failure for the same-directory case.\n"+
			"  If BUILD_DIR and $(PREFIX)/bin ever resolve to one directory there\n"+
			"  is nothing to promote; reporting success would be a lie.\n  recipe:\n%s",
			indentForMessage(recipe))
	}

	// All five, or none: a failed transaction must abort the target before
	// the closing success claim is printed.
	if !strings.Contains(recipe, "FAIL: transactional binary promotion did not complete") {
		t.Errorf("install recipe does not check the transaction's result.\n"+
			"  A non-zero transaction means the previous binary set was restored;\n"+
			"  the target must say so and exit non-zero instead of printing its\n"+
			"  success line over a rolled-back deploy.  recipe:\n%s",
			indentForMessage(recipe))
	}
}

// TestBuildConfig_CleanCannotRemoveTheInstall — GUARD D
//
// `make clean` removes BUILD_DIR only. The pre-fix clean did
// `rm -rf bin`, which in the canonical layout deleted the live install
// outright — the running scheduler's binary gone.
func TestBuildConfig_CleanCannotRemoveTheInstall(t *testing.T) {
	recipe := makeRecipe(t, makeFilePath(t), "clean")

	rmRE := regexp.MustCompile(`(?m)^[^\n]*\brm\b[^\n]*$`)
	for _, line := range rmRE.FindAllString(recipe, -1) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(line, "$(PREFIX)") {
			t.Errorf("`make clean` runs a command that references $(PREFIX):\n    %s\n"+
				"  clean must never touch the install prefix; it removes developer\n"+
				"  artifacts ($(BUILD_DIR)) only.", trimmed)
		}
		// A bare `rm -rf bin` is the exact pre-fix shape. Allow `rm -f ./mpm`
		// style stray-root cleanup only if it names the binary, not a dir.
		if regexp.MustCompile(`\brm\b[^|;]*\bbin\b`).MatchString(line) &&
			!strings.Contains(line, "$(BUILD_DIR)") {
			t.Errorf("`make clean` removes a path containing `bin` that is not $(BUILD_DIR):\n    %s\n"+
				"  In the canonical layout the checkout's bin/ IS $(PREFIX)/bin.", trimmed)
		}
	}
}

// TestBuildConfig_ReleaseAndTestSubprocessesUseDeveloperArtifacts — GUARD E
//
// Every test/release harness that execs a built binary must resolve the
// DEVELOPER artifact. The pre-fix failure was silent: a harness that
// fell back to ~/.mpm/bin/mpm kept passing while exercising whatever was
// installed, so a broken build looked green.
func TestBuildConfig_ReleaseAndTestSubprocessesUseDeveloperArtifacts(t *testing.T) {
	repoRoot := findRepoRoot(t)

	// (relative path, required substring identifying the developer artifact)
	harnesses := []struct {
		path string
		want string
	}{
		{"scripts/work_acceptance.sh", ".build/bin/mpm"},
		{"release_acceptance/public_cli_helpers_test.go", `"..", ".build", "bin", "mpm"`},
		{"cmd/mpm/exec_helpers_test.go", `builtCLIRelPath = "../../.build/bin"`},
		{"scripts/smoke_shared.sh", ".build/bin/mpm"},
		{"scripts/smoke_telemetry.sh", ".build/bin/mpm-telemetry"},
		{"scripts/verify-alpha-blocker-fixes.sh", ".build/bin/mpm"},
	}

	for _, h := range harnesses {
		t.Run(h.path, func(t *testing.T) {
			full := filepath.Join(repoRoot, h.path)
			data, err := os.ReadFile(full)
			if err != nil {
				t.Fatalf("read %s: %v", full, err)
			}
			if !strings.Contains(string(data), h.want) {
				t.Errorf("%s does not resolve the developer build artifact (want %q).\n"+
					"  It must exec $(BUILD_DIR)/mpm, not the installed binary.\n"+
					"  A harness pointed at ~/.mpm/bin/mpm tests the INSTALLED code, so a\n"+
					"  broken working tree still reports green.", h.path, h.want)
			}
			// The failure mode this guards is a FALLBACK, so the absence
			// of a silent installed-binary fallback matters as much as the
			// presence of the artifact path.
			for _, forbidden := range []string{
				".mpm/bin/mpm\"}", "$HOME/.mpm/bin/mpm\"",
				"|| $HOME/.mpm/bin/mpm", ":-~/.mpm/bin/mpm",
			} {
				if strings.Contains(string(data), forbidden) {
					t.Errorf("%s contains an installed-binary fallback %q.\n"+
						"  A missing developer artifact must FAIL the test, not silently\n"+
						"  substitute the installed production binary.", h.path, forbidden)
				}
			}
		})
	}
}

// TestBuildConfig_SystemdStillPointsAtTheInstalledBinaries — GUARD F
//
// The systemd unit is the runtime ABI and must keep pointing at
// $(PREFIX)/bin — the separation must not be "fixed" by pointing the
// service at a build directory, which would make the unit depend on a
// disposable checkout artifact.
func TestBuildConfig_SystemdStillPointsAtTheInstalledBinaries(t *testing.T) {
	repoRoot := findRepoRoot(t)
	// The CANONICAL unit template that install.sh copies verbatim
	// (Makefile SERVICE_SRC). Not ~/.config/systemd/user/... : that path
	// holds the INSTALLED copy on a live host and does not exist in a
	// fresh checkout, so reading it here would either fail on every clone
	// or — worse — silently match a stray file some earlier test left in
	// the tree and pass without ever reading the tracked source.
	unitPath := filepath.Join(repoRoot, "contrib", "systemd", "mpm-scheduler.service.user")
	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read %s: %v", unitPath, err)
	}
	text := string(data)

	// The guard is only meaningful if install.sh actually installs THIS
	// file, so pin the wiring too: a unit that is correct but no longer
	// the one that gets installed is not the deployed unit.
	mk, mkErr := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if mkErr != nil {
		t.Fatalf("read Makefile: %v", mkErr)
	}
	src, ok := makeVariable(t, filepath.Join(repoRoot, "Makefile"), "SERVICE_SRC")
	if !ok {
		t.Fatalf("Makefile must define SERVICE_SRC")
	}
	if !strings.Contains(string(mk), src) {
		t.Errorf("Makefile does not reference SERVICE_SRC=%s", src)
	}

	execStart := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ExecStart=") {
			execStart = strings.TrimSpace(line)
			break
		}
	}
	if execStart == "" {
		t.Fatalf("mpm-scheduler.service has no ExecStart= line")
	}

	if !strings.Contains(execStart, "%h/.mpm/bin/mpm-scheduler") {
		t.Errorf("systemd ExecStart does not point at the installed binary:\n    %s\n"+
			"  The runtime ABI is ~/.mpm/bin/. It must not be repointed at the\n"+
			"  developer build directory.", execStart)
	}
	if strings.Contains(execStart, ".build") || strings.Contains(text, ".build/bin") {
		t.Errorf("mpm-scheduler.service references the developer build directory:\n%s\n"+
			"  $(BUILD_DIR) is a disposable checkout artifact that `make clean`\n"+
			"  removes. The service must execute the installed binary.", execStart)
	}
}
