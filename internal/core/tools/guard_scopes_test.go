// guard_scopes_test.go — shared scope resolution for the static-analysis
// guards in this package.
//
// # Why this file exists
//
// Three guards in this package (TestOutputPolicy_OnlyMCPEnforces,
// TestParity_AllActionTools_LockEverySurface, and
// TestAdapterCallsites_MatchGoSchema) inspect source outside the Go
// package they live in: the CLI and MCP-server source trees under
// cmd/, and the JavaScript/TypeScript adapters under
// agent_installation/. Each one originally resolved those paths
// independently and each one had the same two failure modes:
//
//  1. Discovery could return ZERO targets and the guard would still
//     report success — or, worse, `t.Skipf` and report nothing at
//     all. A guard that inspects nothing passes, so a refactor that
//     moved or renamed the thing it guards silently disarmed it.
//  2. Discovery depended on the process working directory, which for
//     `go test` is the package source directory regardless of where
//     the command was invoked. Tests that happened to work from one
//     directory failed or skipped from another.
//
// The helpers below fix both, once, for all three guards:
//
//   - Paths resolve from this file's own compiled-in location
//     (runtime.Caller), not from os.Getwd(). A guard behaves the same
//     whether `go test` is launched from the tools package, from the
//     repository root, or from a CI runner's build directory.
//   - Every scope accessor returns an ERROR for an absent or empty
//     population instead of an empty success. The caller turns that
//     error into a test failure. There is no code path in this file
//     that reports "I looked at nothing" as success.
//
// The regression tests at the bottom of the file pin that contract by
// pointing each accessor at a deliberately missing scope and requiring
// an error. Without them, the next person to "make the guard tolerant
// of an empty result" would reintroduce the defect with a green suite.
package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRootMarker is the pair of files that identifies the MPM
// repository root. go.mod alone is ambiguous — this is a three-module
// repo, and both internal/core and internal/core/tools carry one.
// The Makefile exists only at the top level, so the conjunction is
// unambiguous and does not depend on a path depth that a future
// reorganisation would invalidate.
var repoRootMarker = []string{"go.mod", "Makefile"}

// guardRepoRoot returns the MPM repository root, derived from the
// source location of this file at compile time.
//
// This is the fix for the working-directory dependency. os.Getwd()
// under `go test` returns the package directory
// (internal/core/tools), never the directory the operator or CI
// invoked the command from, so any guard that reasons about "where am
// I" is reasoning about the wrong thing. runtime.Caller(0) gives the
// absolute path of this source file, which is the same on every host
// and every invocation style.
//
// Fails the test when the root cannot be found rather than falling
// back to a relative guess: a fallback would let the guards "run" and
// inspect nothing.
func guardRepoRoot(t *testing.T) string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("guardRepoRoot: runtime.Caller(0) failed; cannot locate this source file, " +
			"so no source-tree scope can be resolved. Refusing to run the guards against an unknown root.")
	}

	dir := filepath.Dir(thisFile)
	// The deepest legitimate distance from this file to the repo root is
	// 3 (internal/core/tools → internal/core → internal → .). Allow a
	// generous 8 so a future module nesting does not silently break
	// resolution; the marker check, not the depth, is what actually
	// decides.
	for i := 0; i < 8; i++ {
		all := true
		for _, m := range repoRootMarker {
			if _, err := os.Stat(filepath.Join(dir, m)); err != nil {
				all = false
				break
			}
		}
		if all {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	t.Fatalf("guardRepoRoot: could not find a directory containing %s, walking up from %s. "+
		"The repository layout changed; update guardRepoRoot's marker rather than letting the "+
		"source-scanning guards fall back to a working-directory guess.",
		strings.Join(repoRootMarker, " + "), filepath.Dir(thisFile))
	return "" // unreachable; satisfies the compiler.
}

// nonTestGoFilesIn returns the names of the non-test .go files directly
// inside dir.
//
// "Non-test" is deliberate: the guards that use this helper assert
// properties of PRODUCTION source. Including _test.go files would widen
// the population with files whose references are legitimate by
// definition — a test asserting a symbol is absent necessarily names
// it.
//
// Returns an error when dir does not exist, is not a directory, or
// contains no non-test .go files. All three are "the guard is about to
// inspect nothing", which is never a legitimate success for a guard.
func nonTestGoFilesIn(dir string) ([]string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("scope directory %s is not readable: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("scope path %s is not a directory", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot read scope directory %s: %w", dir, err)
	}

	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("scope directory %s contains no non-test .go files; "+
			"the guard would inspect nothing, which is a silent pass, not a pass", dir)
	}
	return out, nil
}

// adapterRoot returns the agent_installation directory, the JavaScript
// and TypeScript adapter tree the cross-language schema guard walks.
//
// agent_installation/ is a contractual location, not a convenience:
// it is what the installer copies adapters out of and what the plugins
// are published from. If it moves, the guard must be updated to point
// at the new location — it must not quietly stop guarding anything.
func adapterRoot(repoRoot string) (string, error) {
	candidate := filepath.Join(repoRoot, "agent_installation")
	info, err := os.Stat(candidate)
	if err != nil {
		return "", fmt.Errorf("agent_installation/ not found at %s: %w", candidate, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s exists but is not a directory", candidate)
	}
	return candidate, nil
}

// actionEnumOf returns the action enum a registry tool advertises, and
// whether one is declared at all.
//
// A tool with no action enum is a legitimate registry state: several
// tools (mpm_resolve, mpm_blob_read, mpm_blob_search,
// mpm_log_to_changelog, mpm_retrieval_diagnose, mpm_request_review)
// take pointer URIs or a single fixed operation rather than an action
// verb, and correctly advertise no action surface.
//
// The error return is for a genuinely broken registry entry — an
// unknown tool name or a schema that is not valid JSON — which is
// always a defect and never a "not applicable".
func actionEnumOf(toolName string) (enum []string, declaresEnum bool, err error) {
	tool, ok := ByName(toolName)
	if !ok {
		return nil, false, fmt.Errorf("registry has no tool named %q", toolName)
	}

	var schema struct {
		Properties struct {
			Action struct {
				Enum []string `json:"enum"`
			} `json:"action"`
		} `json:"properties"`
	}
	if uerr := json.Unmarshal(tool.Schema, &schema); uerr != nil {
		return nil, false, fmt.Errorf("%s: schema is not valid JSON: %w", toolName, uerr)
	}
	return schema.Properties.Action.Enum, len(schema.Properties.Action.Enum) > 0, nil
}

// toolsDeclaringActionEnum returns every registered tool whose schema
// advertises an action enum.
//
// This is the discovery primitive that replaced a hand-maintained list
// of tool names. The list was correct on the day it was written and
// became a liability immediately: a new tool with an action enum
// joins the registry, is not on the list, and is therefore never
// checked. Deriving the population from the registry means the guard
// cannot fall behind the thing it guards.
//
// Errors when the registry is empty, because "no tools declare an
// action enum" from an empty registry is a broken build, not a
// satisfied contract.
func toolsDeclaringActionEnum() (tools []string, all []string, err error) {
	all = Names()
	if len(all) == 0 {
		return nil, nil, fmt.Errorf("the tool registry is empty; the parity guard has nothing to inspect")
	}
	for _, name := range all {
		_, declares, aerr := actionEnumOf(name)
		if aerr != nil {
			return nil, nil, aerr
		}
		if declares {
			tools = append(tools, name)
		}
	}
	if len(tools) == 0 {
		return nil, all, fmt.Errorf("none of the %d registered tools declares an action enum; "+
			"the parity guard would inspect nothing", len(all))
	}
	return tools, all, nil
}

// checkParityPopulation rejects a parity sweep that would inspect
// nothing, and reports how much it actually covered when it passes.
//
// It is split out of the sweep body so the rule is directly testable.
// The behaviour it encodes is the one the original loop got wrong:
// a loop whose body can call t.Skipf aborts the whole test on the
// first inapplicable tool, so a registry in which one tool loses its
// action enum silently stops the entire sweep from checking anything.
func checkParityPopulation(scope string, checked int, names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("%s: the tool registry is empty; nothing to inspect", scope)
	}
	if checked == 0 {
		return fmt.Errorf("%s: none of the %d registered tools declares an action enum; "+
			"the sweep would inspect nothing, which is a silent pass rather than a pass", scope, len(names))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Regression tests for the scope contract itself.
//
// Each of these constructs a deliberately absent or empty scope and
// requires the corresponding accessor to report an error. They exist
// because a guard that fails open is worse than no guard: it converts
// a broken invariant into a green suite, and the green suite is the
// signal everyone downstream trusts.
// ---------------------------------------------------------------------------

// TestGuardScopes_RepoRootIgnoresWorkingDirectory proves the root
// resolver is anchored to the compiled-in source location. It changes
// the process working directory to a directory that contains no
// marker files and asserts the resolver still finds the real root —
// the property the original os.Getwd()-based resolution lacked.
func TestGuardScopes_RepoRootIgnoresWorkingDirectory(t *testing.T) {
	want := guardRepoRoot(t)

	// t.Chdir restores the previous directory when the test ends.
	// Go 1.24+; this repo builds on a toolchain that provides it.
	t.Chdir(t.TempDir())

	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(original, "go.mod")); err == nil {
		t.Fatalf("test setup is wrong: the temp cwd %s contains a go.mod and cannot prove independence", original)
	}

	if got := guardRepoRoot(t); got != want {
		t.Errorf("guardRepoRoot() = %q with cwd=%s; want %q. The resolver must not depend on the "+
			"process working directory, which is the package directory under `go test` regardless of "+
			"where the command was invoked", got, original, want)
	}
}

// TestGuardScopes_MissingDirectoryIsAnError covers the case that
// disarmed TestOutputPolicy_OnlyMCPEnforces: the CLI source tree
// moving or being renamed. The guard must fail, not pass quietly.
func TestGuardScopes_MissingDirectoryIsAnError(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "cmd", "mpm-does-not-exist")

	files, err := nonTestGoFilesIn(missing)
	if err == nil {
		t.Fatalf("nonTestGoFilesIn(%q) returned %d files and no error; a missing scope must be an "+
			"explicit failure, or the guard silently inspects nothing and reports success", missing, len(files))
	}
	if len(files) != 0 {
		t.Errorf("nonTestGoFilesIn returned %d files alongside its error; an errored lookup must return "+
			"no population so a caller that ignores the error still cannot pass on a fabricated result",
			len(files))
	}
}

// TestGuardScopes_EmptyDirectoryIsAnError covers the subtler case: the
// directory exists but the guard's population within it is empty.
// A present-but-empty scope is the state a refactor leaves behind when
// it moves every file out of a directory it forgot to delete, and it is
// the case most likely to be "fixed" by someone adding a zero-length
// guard without noticing the check is now vacuous.
func TestGuardScopes_EmptyDirectoryIsAnError(t *testing.T) {
	base := t.TempDir()

	t.Run("no_go_files", func(t *testing.T) {
		dir := filepath.Join(base, "only-readme")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("nothing here\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if files, err := nonTestGoFilesIn(dir); err == nil {
			t.Errorf("nonTestGoFilesIn(%q) = %d files, nil; want an error for an empty population", dir, len(files))
		}
	})

	// A directory whose only Go files are tests is, for a production
	// source guard, an empty population. Asserting it errors keeps the
	// "non-test" filter honest — if someone drops that filter to make
	// a failing guard pass, this case stops being an error and fails.
	t.Run("only_test_files", func(t *testing.T) {
		dir := filepath.Join(base, "tests-only")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte("package x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if files, err := nonTestGoFilesIn(dir); err == nil {
			t.Errorf("nonTestGoFilesIn(%q) = %d files, nil; a test-only directory is an empty "+
				"population for a production-source guard", dir, len(files))
		}
	})
}

// TestGuardScopes_ExistingDirectoryIsUsable is the positive
// counterweight: the empty-population rule must not be so eager that a
// real source directory is rejected. Without this, "fail on empty"
// could be satisfied by a resolver that always errors, which is a
// guard that always fails — equally useless and much harder to notice.
func TestGuardScopes_ExistingDirectoryIsUsable(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "pkg")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for name, body := range map[string]string{
		"main.go":       "package main\n",
		"helper.go":     "package main\n",
		"main_test.go":  "package main\n",
		"notes.txt":     "ignored\n",
		"sub/inner.go":  "package main\n",
		"vendor.go.txt": "not go source\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}

	files, err := nonTestGoFilesIn(dir)
	if err != nil {
		t.Fatalf("nonTestGoFilesIn(%q) returned error on a populated directory: %v", dir, err)
	}
	if len(files) != 2 {
		names := make([]string, 0, len(files))
		for _, f := range files {
			names = append(names, filepath.Base(f))
		}
		t.Errorf("nonTestGoFilesIn(%q) = %v; want exactly main.go and helper.go "+
			"(test files, subdirectories, and non-.go files must be excluded)", dir, names)
	}
}

// TestGuardScopes_MissingAdapterRootIsAnError is the regression for
// TestAdapterCallsites_MatchGoSchema's first skip: agent_installation
// not being where the guard expected. That skip meant the entire
// cross-language schema guard reported success while checking zero
// JavaScript adapters — on a repository whose whole failure class is
// "the Go registry changed and a JS adapter did not".
func TestGuardScopes_MissingAdapterRootIsAnError(t *testing.T) {
	base := t.TempDir()

	if _, err := adapterRoot(base); err == nil {
		t.Errorf("adapterRoot(%q) returned no error for a tree with no agent_installation/; "+
			"the cross-language guard must fail when its adapter tree is absent", base)
	}

	// A file where the directory is expected is equally wrong and must
	// not be accepted as a directory root.
	notDir := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := adapterRoot(notDir); err == nil {
		t.Errorf("adapterRoot(%q) returned no error when agent_installation/ is a regular file", notDir)
	}
}

// TestGuardScopes_AdapterRootResolvesInThisRepository is the positive
// half: from a real repository root, the adapter tree resolves. Paired
// with the negative test above, this pins the accessor to "finds the
// real tree, and refuses everything else" rather than "always errors".
func TestGuardScopes_AdapterRootResolvesInThisRepository(t *testing.T) {
	root := guardRepoRoot(t)
	got, err := adapterRoot(root)
	if err != nil {
		t.Fatalf("adapterRoot(%s): %v", root, err)
	}
	if !strings.HasSuffix(got, "agent_installation") {
		t.Errorf("adapterRoot(%s) = %q; want a path ending in agent_installation", root, got)
	}
}

// TestGuardScopes_ParityDiscoveryCoversTheWholeRegistry pins the
// population the parity sweep actually inspects.
//
// Before this accessor existed, the sweep iterated a hand-written list
// of 15 tool names. The list happened to match the registry exactly, so
// a count assertion alone would prove nothing. The assertion that
// matters is structural: every tool that declares an action enum must
// be in the discovered set, and the discovered set must be a strict
// function of the registry rather than of any list in this file. If
// someone reintroduces a hardcoded list, the coverage assertion in
// TestGuardScopes_ParityDiscoveryIsRegistryDerived fails as soon as
// the registry gains a tool.
func TestGuardScopes_ParityDiscoveryIsRegistryDerived(t *testing.T) {
	tools, all, err := toolsDeclaringActionEnum()
	if err != nil {
		t.Fatalf("toolsDeclaringActionEnum: %v", err)
	}
	if len(tools) >= len(all) {
		t.Errorf("discovered %d enum tools out of %d registered; the registry should contain at least "+
			"one tool that advertises no action enum (mpm_resolve and friends), so a result of %d/%d "+
			"suggests the enum probe is matching everything rather than the real action-enum shape",
			len(tools), len(all), len(tools), len(all))
	}

	// Every discovered tool must genuinely declare an enum, and every
	// non-discovered tool must genuinely not. This is the property that
	// makes the sweep's coverage claim meaningful.
	for _, name := range tools {
		if _, declares, aerr := actionEnumOf(name); aerr != nil || !declares {
			t.Errorf("%s is in the enum-tools set but actionEnumOf reports declaresEnum=%v (err=%v)", name, declares, aerr)
		}
	}
	inSet := make(map[string]bool, len(tools))
	for _, n := range tools {
		inSet[n] = true
	}
	for _, name := range all {
		_, declares, aerr := actionEnumOf(name)
		if aerr != nil {
			t.Errorf("actionEnumOf(%s): %v", name, aerr)
			continue
		}
		if declares && !inSet[name] {
			t.Errorf("%s declares an action enum but is absent from the discovered set; the parity "+
				"sweep would not check it", name)
		}
	}
}

// TestGuardScopes_ParitySweepRejectsEmptyPopulation is the regression
// for the defect that made the original parity sweep decorative: it
// iterated a loop and called t.Skipf inside it, so the FIRST tool with
// an empty action enum aborted the entire test as a skip. Every
// remaining tool — and the guard itself — reported nothing.
//
// The sweep now calls checkParityPopulation before iterating. This
// test drives that function directly with the empty populations the
// loop could be handed, and requires an error for each.
func TestGuardScopes_ParitySweepRejectsEmptyPopulation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checked int
		names   []string
	}{
		{name: "nil_registry", checked: 0, names: nil},
		{name: "empty_registry", checked: 0, names: []string{}},
		{name: "registry_with_no_enum_tools", checked: 0, names: []string{"mpm_resolve", "mpm_blob_read"}},
		{name: "counts_disagree", checked: 0, names: []string{"mpm_memory"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkParityPopulation("parity sweep", tc.checked, tc.names); err == nil {
				t.Errorf("checkParityPopulation(checked=%d, names=%v) = nil; a sweep that inspects "+
					"nothing must report an error, not success", tc.checked, tc.names)
			}
		})
	}
}

// TestGuardScopes_ParitySweepAcceptsRealPopulation is the positive
// counterweight. Without it, the rule above could be satisfied by a
// function that always errors — a guard that always fails, which is
// just as useless and considerably harder to diagnose.
func TestGuardScopes_ParitySweepAcceptsRealPopulation(t *testing.T) {
	tools, all, err := toolsDeclaringActionEnum()
	if err != nil {
		t.Fatalf("toolsDeclaringActionEnum: %v", err)
	}
	if err := checkParityPopulation("parity sweep", len(tools), all); err != nil {
		t.Errorf("checkParityPopulation rejected the real registry population (%d enum tools of %d): %v",
			len(tools), len(all), err)
	}
}
