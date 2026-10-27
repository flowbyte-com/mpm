package internal

// m5_trust_gate_test.go — the mechanical guarantee that the M5 contract tests
// can NEVER be silently absorbed into a required green CI gate.
//
// WHY THIS FILE EXISTS.
//
// The M5 trust-defect suite is split in two:
//
//   - m5_trust_characterization_test.go — asserts the CURRENT behaviour
//     (including the defective behaviour). ALWAYS compiled, ALWAYS green.
//   - m5_trust_contract_test.go — asserts the M5-COMPLIANT behaviour. It
//     FAILS today, because the defects are real and unfixed.
//
// The contract tests are gated behind the `m5contract` build tag. The Makefile
// and both CI test jobs invoke `go test -tags fts5` with no additional tags,
// so the contract files are not compiled by any green gate.
//
// "Not compiled by any green gate" is a claim that can rot: a CI edit, a
// Makefile edit, or a stray `-tags` addition could absorb a failing contract
// test into a required check and turn the sprint's red signal green. This
// file makes that claim CHECKABLE, and it is itself always compiled and
// always green.
//
// WHAT IS ASSERTED.
//
//  1. Every test file that declares an M5 CONTRACT test function carries the
//     `m5contract` build constraint, verified through go/build/constraint
//     (the canonical evaluator, not a string match).
//  2. The contract test FILES are absent from the default build and present
//     under the tag — verified with go/build, i.e. by the same constraint
//     evaluation the toolchain itself uses.
//  3. No CHARACTERIZATION test file carries a build constraint, so the green
//     side of the suite cannot be quietly excluded either.
//  4. Neither the Makefile nor any CI workflow mentions the `m5contract`
//     tag, so no required green check can enable it.
//
// WHAT IS DELIBERATELY NOT ASSERTED.
//
// The existence of at least one contract test file. The retirement procedure
// deletes a contract test when its defect is fixed or accepted, and promotes
// a repaired contract into ordinary CI coverage, so requiring a non-empty
// population would break the retirement. What is required is the
// INVARIANT — every contract test is tagged, no characterization test is
// tagged — which holds both before and after retirement.
//
// For the same reason the per-package "the tagged build must compile a
// contract file" requirement is CONDITIONAL on that package actually
// declaring a contract test. Session B's P3b (2026-10-27) retired the
// critic package's only contract test — defect D2 was fixed and the
// contract was promoted into ordinary CI coverage — and a package with no
// contract test left must not be forced to carry an empty contract file.
//
// ISOLATION. This file reads repository sources and constructs no database.

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// m5ContractTag is the build tag that gates the M5 contract suite.
const m5ContractTag = "m5contract"

// m5ContractTestPrefix is the function-name prefix reserved for the
// M5-COMPLIANT contract suite.
const m5ContractTestPrefix = "TestM5Contract"

// m5CharacterizationTestPrefix is the function-name prefix reserved for the
// CURRENT-BEHAVIOUR characterization suite.
const m5CharacterizationTestPrefix = "TestM5Characterization"

// m5ContractFileName is the file-name prefix of the M5 contract suite. The
// gate test uses the file name (not the function name) because
// go/build.Package exposes only file names for a build.
const m5ContractFileName = "m5_trust_contract_test.go"

// m5ContractDirs are the package directories that may host M5 contract tests,
// relative to the repository root.
var m5ContractDirs = []string{
	"internal/core",
	"internal/critic",
}

// m5DeclaredTestFuncs returns the names of the test functions declared in a
// Go source file, by parsing it.
func m5DeclaredTestFuncs(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		if strings.HasPrefix(fn.Name.Name, "Test") {
			out = append(out, fn.Name.Name)
		}
	}
	return out
}

// m5FileInBuild reports whether the toolchain would compile the given file
// under the given build context.
//
// build.Context.MatchFile is the toolchain's OWN evaluator of build
// constraints — the same one cmd/go uses to decide whether a file belongs to
// a build. Using it rather than a string match on the file text means this
// guard cannot disagree with the compiler about what is compiled.
func m5FileInBuild(t *testing.T, ctx *build.Context, path string) bool {
	t.Helper()
	match, err := ctx.MatchFile(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		t.Fatalf("MatchFile(%s): %v", path, err)
	}
	return match
}

// TestM5TrustGate_ContractTestsAreExcludedFromEveryGreenGate is the guard.
//
// It fails if a contract test can be run by a default build, or if a CI
// workflow or the Makefile can enable the tag that runs them.
func TestM5TrustGate_ContractTestsAreExcludedFromEveryGreenGate(t *testing.T) {
	root := findRepoRoot(t)

	// ── 1 & 3: the tag discipline, per file ──────────────────────────────
	files, err := collectTestFiles(root)
	if err != nil {
		t.Fatalf("collect test files under %s: %v", root, err)
	}

	defaultCtx := build.Default
	taggedCtx := build.Default
	taggedCtx.BuildTags = append(append([]string{}, build.Default.BuildTags...), m5ContractTag)

	contractFiles := 0
	characterizationFiles := 0
	// dirsWithContract records which package directories declare at least
	// one contract test. A directory with NO contract test must not be
	// required to carry a contract FILE — see the retirement note below.
	dirsWithContract := map[string]bool{}
	for _, rel := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		funcs := m5DeclaredTestFuncs(t, path)

		hasContract := false
		hasCharacterization := false
		for _, fn := range funcs {
			if strings.HasPrefix(fn, m5ContractTestPrefix) {
				hasContract = true
			}
			if strings.HasPrefix(fn, m5CharacterizationTestPrefix) {
				hasCharacterization = true
			}
		}
		if !hasContract && !hasCharacterization {
			continue
		}

		satisfiesDefault := m5FileInBuild(t, &defaultCtx, path)
		satisfiesTagged := m5FileInBuild(t, &taggedCtx, path)

		if hasContract {
			contractFiles++
			dirsWithContract[filepath.ToSlash(filepath.Dir(rel))] = true
			if satisfiesDefault {
				t.Errorf("%s declares %s* functions but is compiled by the "+
					"DEFAULT build — a contract test would be running in a "+
					"required green gate", rel, m5ContractTestPrefix)
			}
			if !satisfiesTagged {
				t.Errorf("%s declares %s* functions but is NOT compiled under "+
					"the %q tag — the contract suite would be unreachable",
					rel, m5ContractTestPrefix, m5ContractTag)
			}
		}

		if hasCharacterization {
			characterizationFiles++
			if !satisfiesDefault {
				t.Errorf("%s declares %s* functions but is EXCLUDED from the "+
					"default build — the green side of the suite must always "+
					"run", rel, m5CharacterizationTestPrefix)
			}
		}
	}

	if characterizationFiles == 0 {
		t.Errorf("no %s* test file was found — the characterization suite must "+
			"always run, or the defects have no green pin",
			m5CharacterizationTestPrefix)
	}
	t.Logf("M5 trust suite: %d contract-tagged file(s), %d always-compiled "+
		"characterization file(s)", contractFiles, characterizationFiles)

	// ── 2: the toolchain's own view of the build ─────────────────────────
	//
	// go/build evaluates build constraints exactly as the toolchain does, so
	// this is the authoritative check that the contract files are absent from
	// the default build and present under the tag.
	for _, dir := range m5ContractDirs {
		abs := filepath.Join(root, filepath.FromSlash(dir))
		if _, err := os.Stat(abs); err != nil {
			t.Fatalf("expected %s to exist: %v", dir, err)
		}

		defaultPkg, err := defaultCtx.ImportDir(abs, 0)
		if err != nil {
			t.Fatalf("build.Default.ImportDir(%s): %v", dir, err)
		}
		taggedPkg, err := taggedCtx.ImportDir(abs, 0)
		if err != nil {
			t.Fatalf("taggedCtx.ImportDir(%s): %v", dir, err)
		}

		if got := m5ContractFilesIn(defaultPkg.TestGoFiles); len(got) != 0 {
			t.Errorf("%s: the DEFAULT build compiles contract test file(s) %v — "+
				"a failing contract test would be inside a required green gate",
				dir, got)
		}
		// RETIREMENT. A contract test is deleted when its defect is fixed
		// or accepted, and a repaired contract is PROMOTED into ordinary
		// CI coverage. When a package has no contract test left, requiring
		// it to carry a contract FILE would break that retirement — the
		// guard would fail a correct fix for the wrong reason.
		//
		// So the requirement is conditional on the package actually
		// DECLARING a contract test. The invariant this guard protects is
		// "every contract test is tagged, no characterization test is
		// tagged" — and that holds whether or not a package still has
		// contract tests at all.
		if dirsWithContract[dir] {
			if got := m5ContractFilesIn(taggedPkg.TestGoFiles); len(got) == 0 {
				t.Errorf("%s: the %q-tagged build does NOT compile %s — the "+
					"contract suite is unreachable", dir, m5ContractTag,
					m5ContractFileName)
			}
		} else {
			t.Logf("%s: no contract test remains in this package (retired) — "+
				"the contract-file requirement does not apply", dir)
		}
		t.Logf("%s: default build compiles %d test file(s), %d contract file(s); "+
			"tagged build compiles %d test file(s), %d contract file(s)", dir,
			len(defaultPkg.TestGoFiles), len(m5ContractFilesIn(defaultPkg.TestGoFiles)),
			len(taggedPkg.TestGoFiles), len(m5ContractFilesIn(taggedPkg.TestGoFiles)))
	}

	// ── 4: no green gate may enable the tag ──────────────────────────────
	//
	// The Makefile and the CI workflows are the only things that decide what
	// a required green check compiles. If either mentions the tag, the
	// contract suite becomes part of a green gate.
	checked := 0
	for _, rel := range []string{"Makefile"} {
		path := filepath.Join(root, rel)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		checked++
		if strings.Contains(string(body), m5ContractTag) {
			t.Errorf("%s mentions the %q build tag — the M5 contract suite "+
				"would be inside a required green gate", rel, m5ContractTag)
		}
	}

	workflowDir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		rel := ".github/workflows/" + e.Name()
		body, err := os.ReadFile(filepath.Join(workflowDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		checked++
		if strings.Contains(string(body), m5ContractTag) {
			t.Errorf("%s mentions the %q build tag — the M5 contract suite "+
				"would be inside a required green CI gate", rel, m5ContractTag)
		}
	}
	t.Logf("checked %d gate definition(s) for the %q tag", checked, m5ContractTag)
}

// m5ContractFilesIn returns the contract-suite file names present in the
// given build's test file list.
func m5ContractFilesIn(files []string) []string {
	var out []string
	for _, f := range files {
		if strings.HasPrefix(f, m5ContractFileName) {
			out = append(out, f)
		}
	}
	return out
}
