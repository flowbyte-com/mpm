package internal

// test_db_safety_test.go — repo-wide AST guard enforcing the test
// isolation invariant from CLAUDE.md §3:
//
//     Tests must not open or mutate the user's production MPM
//     database through default production paths.
//
// # Why AST rather than grep
//
// The dangerous calls are constructors, not string matches. A grep for
// `NewMemoryStore` hits comments, doc examples, and this guard's own
// rule table. An AST walk sees only real call expressions, so the rule
// is exactly "some test function calls this constructor" — which is
// the thing that actually reaches the live database.
//
// # What is detected
//
//   - NewDatabaseManager("") / NewDatabaseManager('') — falls through
//     to config.GetMPMDir() → $HOME/.mpm/src/db/mpm.db
//
//   - NewMemoryStore(<anything>) — the path argument is DISCARDED
//     (`func NewMemoryStore(_ string)` at memory.go:190), so every
//     call resolves config.GetMPMDir(). This is why the rule cannot be
//     narrowed to an empty-literal argument the way the
//     NewDatabaseManager rule is: passing a real path changes nothing.
//     A MemoryStore built this way is inert until something calls
//     InitSQLite() — at which point it writes the full DDL (tables +
//     4 FTS5 virtual tables) to the live database. That is exactly
//     what the old TestGetByIDNilDB did by accident: it called
//     GetByID() on such a store, and GetByID lazily calls
//     InitSQLite() when s.DB == nil (memory.go:1185).
//
// # Discovery scope
//
// The repo is a multi-module tree with NO go.work:
//
//	github.com/flowbyte-com/mpm              (root)
//	github.com/flowbyte-com/mpm-core         (internal/core)
//	github.com/flowbyte-com/mpm-core/tools   (internal/core/tools)
//	github.com/flowbyte-com/mpm/release_acceptance
//	github.com/flowbyte-com/mpm/scripts
//
// A guard that resolved the *nearest* go.mod would stop at
// internal/core/go.mod and never see cmd/mpm, internal/scheduler,
// internal/core/tools, or release_acceptance — 265+ test files, two of
// which live in modules no `make test` target traverses at all. That
// is precisely how the previous version of this guard became
// decorative. The shared findRepoRoot helper therefore walks to the
// directory holding BOTH go.mod and Makefile — only the repository
// root has both — and the walk covers the whole tree from there.
// Pinned by TestDBSafety_ScanCoversEveryModuleArea.
//
// # Why it runs in -short
//
// Both gates that matter pass -short: `make test-core-precommit`
// (pre-commit) and the CI "Core tests" step. A guard that skips under
// -short is a guard that never runs in either of the two places it
// exists to protect. The scan is a few hundred AST parses of files the
// build has already read; trading correctness for a fraction of a
// second is not worth it. Pinned by TestDBSafety_NotShortSkipped.
//
// # False positives
//
// Legitimate isolation does not go through these constructors at all —
// it goes through NewTestDM / NewTestLocalOnlyDM / NewTestSharedDM (all
// of which route through t.TempDir() or an in-memory DSN), or through
// NewDatabaseManager(t.TempDir()), whose non-empty argument the rule
// does not match. A test that genuinely needs a raw constructor adds
// itself to testDBSafetyWhitelist by REPO-RELATIVE path with a
// justification; a bare filename would collide across modules.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// dbLeakRule is one production-path constructor that a test must not
// call. `emptyFirstArgOnly` distinguishes the two shapes:
//
//   - NewDatabaseManager: a non-empty path argument genuinely
//     redirects the database, so only the empty-literal form leaks.
//   - NewMemoryStore: the argument is discarded, so EVERY call leaks.
//     Rules with emptyFirstArgOnly == false match on constructor name
//     alone.
type dbLeakRule struct {
	ctor              string
	emptyFirstArgOnly bool
	reason            string
}

// productionDBRules is the rule set. Adding a constructor here is the
// supported way to widen the guard; do not add one-off string greps.
var productionDBRules = []dbLeakRule{
	{
		ctor:              "NewDatabaseManager",
		emptyFirstArgOnly: true,
		reason:            `NewDatabaseManager("") falls through to config.GetMPMDir()`,
	},
	{
		ctor:              "NewMemoryStore",
		emptyFirstArgOnly: false,
		reason:            `NewMemoryStore discards its path argument; every call resolves config.GetMPMDir()`,
	},
}

// testDBSafetyWhitelist enumerates the test files that legitimately
// need a production-path constructor, keyed by REPO-RELATIVE path,
// with a one-line justification for each. An entry with an empty
// justification is itself a guard failure — the whole point of the
// whitelist is that every exception is argued for in review.
var testDBSafetyWhitelist = map[string]string{
	"internal/core/database_manager_arg_test.go":                      "pins the constructor's argument-handling contract (HonoursProjectRoot, EmptyArg_FallsBackToGetMPMDir, UnwritableHome_NoSpuriousError)",
	"internal/core/foreign_keys_test.go":                              "needs the real file-DB DSN with _foreign_keys=1; NewTestDM would skip the DSN and silently disable FK enforcement",
	"internal/core/shared_attach_test.go":                             "two tests (BadPathFallsBackGracefully, ReadOnlyEnv) construct a DatabaseManager after manipulating MPM_SHARED_DB / MPM_SHARED_READONLY, which requires the raw ctor",
	"internal/core/handoff_identity_test.go":                          "needs the real file-DB DSN with _foreign_keys=1 to drive close+reopen across migration boundaries (recovery-across-restart + sentinel-gated migration round-trip); NewTestDM's in-memory store cannot simulate on-disk persistence",
	"internal/core/migration_session_handoffs_mpm_session_id_test.go": "needs the real file-DB DSN with MPM_WORKSPACE override to drive the additive migration path end-to-end (column presence, sentinel round-trip, legacy-row survival); same pattern as handoff_identity_test.go",

	// The five entries below were invisible to the previous version of
	// this guard, which resolved the nearest go.mod and therefore
	// stopped at internal/core. Each one is genuinely isolated: it
	// redirects config.GetMPMDir() via MPM_WORKSPACE to a t.TempDir()
	// root BEFORE constructing, so the empty-string argument never
	// reaches $HOME/.mpm. The redirection is invisible to a static AST
	// scan, which is why isolation has to be asserted in review and
	// pinned here rather than inferred.
	"cmd/mpm/handlers_backup_singleton_test.go":         "sets MPM_WORKSPACE to t.TempDir() immediately before NewDatabaseManager(\"\"); needs a real on-disk DBPath (not NewTestDM's in-memory) so DBPath() is a real path and the _foreign_keys pragma is observable",
	"cmd/mpm/handlers_restore_safety_test.go":           "setupRestoreSafetyTest sets MPM_WORKSPACE to t.TempDir() before NewDatabaseManager(\"\"); the handler under test does os.Rename/sql.Open/os.Remove on the DB file, which an in-memory DM cannot support",
	"cmd/mpm/work_integration_test.go":                  "newWorkTestDM and its callers set MPM_WORKSPACE to t.TempDir() before NewDatabaseManager(\"\"); the test deliberately reopens the same file in a second DM to prove cross-session persistence",
	"release_acceptance/cross_agent_continuity_test.go": "every test calls makeAcceptanceWorkspace(t), which sets MPM_WORKSPACE to a t.TempDir() root before NewDatabaseManager(\"\"); the scenario is built to reopen one SQLite file in a second DM to simulate a separate agent process, which is the property under test",
	"release_acceptance/supersession_trace_test.go":     "same makeAcceptanceWorkspace(t) isolation; the second NewDatabaseManager(\"\") is the deliberate process-restart reopen that the supersession trace asserts across",
}

// dbSafetySkipDirs are directory names never descended into. testdata
// holds fixture files that are not compiled as part of any package and
// may legitimately contain constructor-shaped text.
var dbSafetySkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".claude":      true,
	"bin":          true,
	"testdata":     true,
}

// expectedScanAreas are the areas the guard must demonstrably reach.
// Each is asserted to exist AND to contribute at least one scanned
// test file, so that a scope regression (renamed directory, walk that
// stops early, module moved out of tree) is a hard failure rather than
// a silently smaller scan.
var expectedScanAreas = []string{
	"cmd/mpm",
	"internal/core",
	"internal/core/tools",
	"internal/scheduler",
	"release_acceptance",
	"scripts",
}

// TestDBSafety_NoLiveDBLeakInTests walks every _test.go file in the
// repo and fails if any file outside the whitelist calls a
// production-path database constructor. This is the regression guard
// for the 2026-08-12 "tests polluting live DB" incident and for the
// 2026-09-29 TestGetByIDNilDB finding, where a test built a
// MemoryStore through the live-path constructor and then triggered
// InitSQLite() — writing the full schema to the user's real database.
func TestDBSafety_NoLiveDBLeakInTests(t *testing.T) {
	// Every whitelist entry must carry a justification. An unexplained
	// exception is indistinguishable from a hole in the guard.
	for rel, why := range testDBSafetyWhitelist {
		if strings.TrimSpace(why) == "" {
			t.Errorf("testDBSafetyWhitelist[%q] has an empty justification; "+
				"every exception must be argued for in review", rel)
		}
	}

	root := findRepoRoot(t)
	files, err := collectTestFiles(root)
	if err != nil {
		t.Fatalf("collect test files under %s: %v", root, err)
	}

	var violations []string
	for _, rel := range files {
		reason, leaked := liveDBLeakInFile(filepath.Join(root, rel))
		if !leaked {
			continue
		}
		if _, whitelisted := testDBSafetyWhitelist[rel]; whitelisted {
			continue
		}
		violations = append(violations, rel+": "+reason)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("%d test file(s) call a production-path DB constructor outside the safety whitelist:\n  %s\n\n"+
			"Fix: build the store through NewTestDM(t) / NewTestLocalOnlyDM(t) / NewTestSharedDM(t) "+
			"(see internal/core/testhelpers.go), or through NewDatabaseManager(t.TempDir()). "+
			"A MemoryStore built for tests needs DM+DB set from a hermetic DatabaseManager "+
			"(see freshMemoryStore in internal/core/memory_test.go). "+
			"If the call is genuinely required, add the REPO-RELATIVE path to testDBSafetyWhitelist "+
			"in internal/core/test_db_safety_test.go with a one-line justification.",
			len(violations),
			strings.Join(violations, "\n  "))
	}
}

// TestDBSafety_ScanCoversEveryModuleArea pins discovery scope. The
// previous guard resolved the nearest go.mod — internal/core — and so
// covered only internal/core while its own comment claimed repo-wide
// coverage. This asserts the walk reaches every area the repo actually
// has test files in, including the two nested modules that no `make
// test` target traverses.
func TestDBSafety_ScanCoversEveryModuleArea(t *testing.T) {
	root := findRepoRoot(t)
	files, err := collectTestFiles(root)
	if err != nil {
		t.Fatalf("collect test files under %s: %v", root, err)
	}

	counts := make(map[string]int, len(expectedScanAreas))
	for _, rel := range files {
		for _, area := range expectedScanAreas {
			if rel == area || strings.HasPrefix(rel, area+"/") {
				counts[area]++
			}
		}
	}

	for _, area := range expectedScanAreas {
		abs := filepath.Join(root, filepath.FromSlash(area))
		if _, err := os.Stat(abs); err != nil {
			t.Errorf("expected scan area %q is missing from the tree: %v\n"+
				"If it was moved or removed, update expectedScanAreas so the guard's "+
				"scope stays honest.", area, err)
			continue
		}
		if counts[area] == 0 {
			t.Errorf("expected scan area %q contributed 0 scanned test files; the walk is "+
				"not reaching it (repo root resolved to %s)", area, root)
		}
	}
}

// TestDBSafety_NotShortSkipped pins that this guard does not opt out
// of -short. Both real gates (`make test-core-precommit` and the CI
// "Core tests" step) pass -short, so a testing.Short() skip here
// would silently disable the guard everywhere it matters. Asserted
// against this file's own source so the pin cannot drift from the
// implementation.
//
// The check is AST-based, not a substring search: the diagnostic text
// below names "testing.Short" in a string literal, and a naive
// substring match would report the message that describes the rule as
// a violation of it.
func TestDBSafety_NotShortSkipped(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the guard source")
	}
	src, err := os.ReadFile(thisFile)
	if err != nil {
		t.Fatalf("read guard source %s: %v", thisFile, err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), thisFile, src, 0)
	if err != nil {
		t.Fatalf("parse guard source %s: %v", thisFile, err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			return true
		}
		pkg, isPkg := sel.X.(*ast.Ident)
		if isPkg && pkg.Name == "testing" && sel.Sel.Name == "Short" {
			t.Errorf("%s calls testing.Short(); the production-DB guard must run under -short "+
				"because both the pre-commit and CI gates pass -short", filepath.Base(thisFile))
		}
		return true
	})
}

// TestDBSafety_DetectsProductionPathConstructorInTestFile proves the
// matcher catches the exact patterns that caused the real incidents,
// in a file the guard does not otherwise know about. This is the
// positive half of the contract: without it, a matcher that silently
// stopped matching would still leave the guard "green".
func TestDBSafety_DetectsProductionPathConstructorInTestFile(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "NewMemoryStore with empty arg",
			src: `package p

import "testing"

func TestX(t *testing.T) {
	store := NewMemoryStore("")
	_ = store
}
`,
		},
		{
			// The argument is discarded by NewMemoryStore, so a
			// plausible-looking non-empty path leaks just the same.
			name: "NewMemoryStore with a real-looking path",
			src: `package p

import "testing"

func TestX(t *testing.T) {
	store := NewMemoryStore("/tmp/some/test/dir")
	_ = store
}
`,
		},
		{
			name: "qualified NewMemoryStore from a sibling package",
			src: `package p

import (
	"testing"

	mpminternal "example.com/mpm/internal"
)

func TestX(t *testing.T) {
	store := mpminternal.NewMemoryStore("")
	_ = store
}
`,
		},
		{
			name: "NewDatabaseManager with empty arg",
			src: `package p

import "testing"

func TestX(t *testing.T) {
	dm, _ := NewDatabaseManager("")
	_ = dm
}
`,
		},
		{
			name: "NewDatabaseManager assigned via nested selector",
			src: `package p

import "testing"

func TestX(t *testing.T) {
	dm, _ := internal.NewDatabaseManager("")
	_ = dm
}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Written to a temp dir, deliberately outside the repo, so
			// this fixture is never itself picked up by a repo scan.
			path := filepath.Join(t.TempDir(), "synthetic_leak_test.go")
			if err := os.WriteFile(path, []byte(tc.src), 0o600); err != nil {
				t.Fatalf("write synthetic fixture: %v", err)
			}
			reason, leaked := liveDBLeakInFile(path)
			if !leaked {
				t.Fatalf("guard did not detect the production-path constructor in:\n%s\n"+
					"The matcher has stopped catching a real leak pattern.", tc.src)
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("leak detected but no reason recorded; a violation must be explainable")
			}
		})
	}
}

// TestDBSafety_AllowsIsolatedFixture is the negative half: hermetic
// test construction must not be flagged. A guard that rejects the
// sanctioned helpers gets disabled within a week, so this pins the
// exclusions as deliberately as the detections.
func TestDBSafety_AllowsIsolatedFixture(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "NewDatabaseManager with t.TempDir",
			src: `package p

import "testing"

func TestX(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dm.Close()
}
`,
		},
		{
			name: "sanctioned hermetic DatabaseManager helpers",
			src: `package p

import "testing"

func TestX(t *testing.T) {
	local := NewTestLocalOnlyDM(t)
	shared := NewTestSharedDM(t)
	mem := NewTestDM(t)
	_, _, _ = local, shared, mem
}
`,
		},
		{
			name: "hermetic MemoryStore built from a test DatabaseManager",
			src: `package p

import "testing"

func TestX(t *testing.T) {
	store := freshMemoryStore(t)
	_ = store
}
`,
		},
		{
			name: "MemoryStore struct literal bound to a hermetic DM",
			src: `package p

import "testing"

func TestX(t *testing.T) {
	dm := NewTestDM(t)
	store := &MemoryStore{
		DM: dm,
		DB: &SQLiteConnection{DB: dm.SQLDB()},
	}
	_ = store
}
`,
		},
		{
			name: "constructor name appearing only in prose",
			src: `package p

import "testing"

// Regression: NewMemoryStore("") used to reach the live database.
func TestX(t *testing.T) {
	store := NewTestDM(t)
	_ = store
}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "synthetic_ok_test.go")
			if err := os.WriteFile(path, []byte(tc.src), 0o600); err != nil {
				t.Fatalf("write synthetic fixture: %v", err)
			}
			if reason, leaked := liveDBLeakInFile(path); leaked {
				t.Fatalf("guard flagged an isolated fixture (%s):\n%s\n"+
					"Hermetic test construction must not be rejected, or the guard "+
					"will be disabled.", reason, tc.src)
			}
		})
	}
}

// TestDBSafety_EmptyScanScopeIsAnError pins that an empty walk is a
// hard failure. A scope bug that silently yields zero scanned files
// makes the guard pass vacuously — the worst possible failure mode for
// a safety gate, because it looks exactly like success.
func TestDBSafety_EmptyScanScopeIsAnError(t *testing.T) {
	empty := t.TempDir()
	files, err := collectTestFiles(empty)
	if err == nil {
		t.Fatalf("collectTestFiles(%s) returned %d files and no error; an empty scan "+
			"scope must be an error, not a silent pass", empty, len(files))
	}
	if !strings.Contains(err.Error(), "no Go test files") {
		t.Errorf("expected the empty-scope error to name the cause, got: %v", err)
	}
}

// collectTestFiles returns every _test.go file under root, as
// slash-separated paths relative to root and sorted. It returns an
// error when the walk yields nothing: see
// TestDBSafety_EmptyScanScopeIsAnError.
func collectTestFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if dbSafetySkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no Go test files found under %s: the scan scope is empty, "+
			"which would make the guard pass vacuously", root)
	}
	sort.Strings(files)
	return files, nil
}

// liveDBLeakInFile reports whether a Go source file contains a
// production-path database constructor call, and if so why.
func liveDBLeakInFile(path string) (string, bool) {
	fset := token.NewFileSet()
	src, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		// Parse errors are caught by `go build` / `go vet`. Failing here
		// too would mask the real build error behind a guard failure, so
		// a file we cannot parse is treated as un-scannable rather than
		// clean. `go vet` is a hard gate in CI and in pre-commit, so an
		// unparseable file never reaches a green build.
		return "", false
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if reason, found := liveDBLeakInFunc(fn); found {
			return reason, true
		}
	}
	return "", false
}

// liveDBLeakInFunc applies the rule set to a single function body.
func liveDBLeakInFunc(fn *ast.FuncDecl) (string, bool) {
	var reason string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if reason != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calleeName(call.Fun)
		for _, rule := range productionDBRules {
			if name != rule.ctor {
				continue
			}
			if rule.emptyFirstArgOnly && !hasEmptyStringFirstArg(call) {
				continue
			}
			reason = rule.reason
			return false
		}
		return true
	})
	return reason, reason != ""
}

// calleeName returns the trailing identifier of a call target, so that
// `NewMemoryStore`, `internal.NewMemoryStore`, and
// `mpminternal.NewMemoryStore` all match by name. Matching on the
// trailing identifier rather than the package qualifier means a new
// subpackage or an import alias is caught without a rule update.
func calleeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

// hasEmptyStringFirstArg reports whether the call's first argument is
// an empty string literal.
func hasEmptyStringFirstArg(call *ast.CallExpr) bool {
	if len(call.Args) < 1 {
		return false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	return lit.Value == `""` || lit.Value == `''`
}

// findRepoRoot is defined once, in build_config_invariants_test.go:
// it walks up until it finds a directory holding BOTH go.mod and
// Makefile. That conjunction is what distinguishes the repository
// root from the nested modules — verified across the whole tree, only
// the root has a Makefile, so the walk cannot stop at
// internal/core/go.mod (`mpm-core`) and silently narrow the scan to
// one subtree while the surrounding comment still claims repo-wide
// coverage. Reusing that helper rather than declaring a second
// root-finder keeps one definition to reason about; a near-duplicate
// pair is exactly how the two drifted in the first place.
