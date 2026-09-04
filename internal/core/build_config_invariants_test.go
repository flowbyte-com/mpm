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
