package internal

// test_db_safety_test.go — AST-based guard against test files accidentally
// calling NewDatabaseManager("") without routing through a hermetic
// helper. NewDatabaseManager("") falls through to config.GetMPMDir(),
// which under a normal `go test ./...` invocation defaults to
// ~/.mpm/src/db/mpm.db — the live production database.
//
// Why this matters:
//   - Every `make test` invocation (or `go test ./...` from a dev shell)
//     that exercises a test file using this pattern silently inserts
//     test fixtures into production state.
//   - The watchdog.jsonl log accumulates the noise, drowning the real
//     error signal that production code paths emit.
//   - Memory tables (memories, capabilities, evidence, ...) accumulate
//     orphan rows whose parent memory is torn down at t.Cleanup, leaving
//     FK orphans that surface as PRAGMA foreign_key_check violations.
//
// History (2026-08-12): pre-guard, six test files used the unsafe
// pattern; five were refactored to NewTestLocalOnlyDM / NewTestSharedDM.
// Three files legitimately need the raw constructor (database_manager_arg_test
// validates the ctor itself; foreign_keys_test needs the real DSN with
// _foreign_keys=1; shared_attach_test has two tests that manipulate
// MPM_SHARED_DB post-init). The whitelist below pins those exceptions;
// everything else fails the build.
//
// Future expansion: if a new package is added that needs the ctor
// directly, add its path to the whitelist with a justifying comment.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testDBSafetyWhitelist enumerates the files that legitimately need to
// call NewDatabaseManager("") directly, with a one-line justification
// for each. Any file outside this list that contains the call must be
// refactored to NewTestLocalOnlyDM, NewTestSharedDM, or NewTestDM.
var testDBSafetyWhitelist = map[string]string{
	"database_manager_arg_test.go": "pins the constructor's argument-handling contract (HonoursProjectRoot, EmptyArg_FallsBackToGetMPMDir, UnwritableHome_NoSpuriousError)",
	"foreign_keys_test.go":         "needs the real file-DB DSN with _foreign_keys=1; NewTestDM would skip the DSN and silently disable FK enforcement",
	"shared_attach_test.go":        "two tests (BadPathFallsBackGracefully, ReadOnlyEnv) construct a DatabaseManager after manipulating MPM_SHARED_DB / MPM_SHARED_READONLY, which requires the raw ctor",
}

// TestTestDBSafety_NoLiveDBLeakInTests walks every _test.go file in the
// repo and fails if any file outside the whitelist calls
// NewDatabaseManager("") or NewDatabaseManager(''). This is the regression
// guard for the 2026-08-12 "tests polluting live DB" incident — the
// watchdog had accumulated 247-484 test-fixture queries per day from
// tests writing to ~/.mpm/src/db/mpm.db.
//
// Discovery scope: this test lives in internal/core but walks the entire
// repo (relative to the module root) so that cmd/mpm, internal/scheduler,
// and any future packages are covered by the same gate. The walk is
// bounded by skipList to avoid scanning vendored dependencies.
func TestTestDBSafety_NoLiveDBLeakInTests(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping repo-wide AST scan in -short mode")
	}

	moduleRoot := findModuleRoot(t)
	skipDirs := map[string]bool{
		".git":         true,
		"node_modules": true,
		"vendor":       true,
		".claude":      true,
	}

	var violations []string
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if containsLiveDBLeak(path) {
			rel, _ := filepath.Rel(moduleRoot, path)
			if _, whitelisted := testDBSafetyWhitelist[filepath.Base(rel)]; !whitelisted {
				violations = append(violations, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module root %s: %v", moduleRoot, err)
	}

	if len(violations) > 0 {
		t.Fatalf("%d test file(s) call NewDatabaseManager(\"\") outside the safety whitelist:\n  %s\n\n"+
			"Fix: refactor to NewTestDM(t) / NewTestLocalOnlyDM(t) / NewTestSharedDM(t) "+
			"(see internal/core/testhelpers.go). If the call is genuinely required, "+
			"add the file to testDBSafetyWhitelist in internal/core/test_db_safety_test.go "+
			"with a one-line justification.",
			len(violations),
			strings.Join(violations, "\n  "))
	}
}

// containsLiveDBLeak parses the file as Go AST and returns true if any
// call expression invokes NewDatabaseManager (or internal.NewDatabaseManager
// / mpminternal.NewDatabaseManager / etc.) with an empty-string literal
// as the first argument.
func containsLiveDBLeak(path string) bool {
	fset := token.NewFileSet()
	src, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		// Parse errors should be caught by `go vet`/`go build`; here we
		// only care about the AST shape, so silently ignore parse failures
		// to avoid masking the real build error with a guard failure.
		return false
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if found {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if !isNewDatabaseManagerCall(call.Fun) {
				return true
			}
			if len(call.Args) < 1 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if lit.Value == `""` || lit.Value == `''` {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// isNewDatabaseManagerCall matches `NewDatabaseManager`,
// `internal.NewDatabaseManager`, `mpminternal.NewDatabaseManager`, etc.
// The AST selector form is "package.Ident"; the bare form is just "Ident".
// We match by the trailing identifier only — package name is checked
// loosely so future internal subpackages and aliases are caught too.
func isNewDatabaseManagerCall(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "NewDatabaseManager"
	case *ast.SelectorExpr:
		return e.Sel.Name == "NewDatabaseManager"
	}
	return false
}

// findModuleRoot walks up from the test's source directory until it
// finds a go.mod, then returns that directory. The test file lives in
// internal/core so this typically returns the repo root in 2-3 hops.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod from %s upwards", dir)
		}
		dir = parent
	}
}