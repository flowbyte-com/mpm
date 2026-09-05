package internal

import (
	"os"
	"path/filepath"
	"regexp"
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
		t.Errorf("Makefile must define CGO_CFLAGS:=-DSQLITE_ENABLE_FTS5=1 at the top "+
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
		t.Errorf("scripts/pre-commit must invoke `make test-core-precommit` "+
			"so the FTS5 flag set is owned by the Makefile (got pre-commit "+
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
