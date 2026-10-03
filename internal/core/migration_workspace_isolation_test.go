// migration_workspace_isolation_test.go — regression guard for the
// migration-backup test-isolation defect.
//
// # The defect
//
// RunMigration and UndoMigration resolve their backup destination through
// config.GetWorkspace() — NOT through the DatabaseManager passed to them —
// and GetWorkspace falls back to $HOME/.mpm when MPM_WORKSPACE is unset.
// The DatabaseManager those tests use is in-memory, so it offers no
// protection at all: takeBackup's `VACUUM INTO` wrote a real file into the
// operator's live ~/.mpm/migrations/ on every run, under a fresh timestamped
// name. Unbounded accumulation, invisible to `git status` because the path
// is gitignored.
//
// TestRunMigration_IdempotentAndProvenanceGated was the only test that
// triggered it. The fix is test-side (isolateMigrationWorkspace in
// embedding_migration_test.go): the production $HOME/.mpm fallback is
// correct behaviour and must not change to accommodate a test.
//
// # Why this guard is dynamic, not static
//
// A static AST rule could assert "this test file calls RunMigration, so it
// must also call t.Setenv". That is checkable, but it is a proxy: it
// cannot tell whether the setenv happens BEFORE the call, whether a
// helper the test calls sets it, or whether the production code resolves
// the workspace the way the AST assumed. The failure mode here is
// specifically "writes to a real path on a real filesystem", so the guard
// observes the real thing: it runs the actual test in a subprocess whose
// HOME points at a sentinel directory, then asks what appeared on disk
// underneath it.
//
// test_db_safety_test.go's constructor guard does not cover this: it
// matches NewDatabaseManager/NewMemoryStore call sites, and this defect
// never calls either. The classes are genuinely different.

package internal

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMigrationBackup_StaysInsideIsolatedWorkspace is the core
// regression. It re-runs the affected test with HOME and MPM_WORKSPACE
// both pointed at a sentinel tree, and fails if anything the test wrote
// lands anywhere other than that tree.
//
// The sentinel tree is what makes the assertion meaningful: with the fix
// removed, config.GetWorkspace() falls back to $HOME/.mpm, which under
// this environment is the sentinel's .mpm — the test would write there and
// the guard would see it. The real operator's ~/.mpm is never a target
// because HOME is overridden for the child.
func TestMigrationBackup_StaysInsideIsolatedWorkspace(t *testing.T) {
	// Locate this package directory so the child `go test` runs against
	// the same sources this guard was compiled from.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the package under test")
	}
	pkgDir := filepath.Dir(thisFile)

	sentinel := t.TempDir()
	home := filepath.Join(sentinel, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", home, err)
	}
	// NOTE: only the HOME fallback is made reachable here. There is
	// deliberately no second workspace directory and no parent-pinned
	// MPM_WORKSPACE — see the env comment below for why that would make
	// the guard vacuous. A correctly isolated test selects its own
	// t.TempDir(), which lives outside the sentinel entirely, so nothing
	// of the fixed behaviour is observable from in here. That is the
	// point: the only thing that can show up is a leak.

	// Record the sentinel's state BEFORE the run, so the assertion is
	// about what the test CREATED rather than what happened to exist.
	before := listFiles(t, sentinel)

	cmd := exec.Command("go", "test", "-tags", "fts5", "-count=1",
		"-run", "^TestRunMigration_IdempotentAndProvenanceGated$", ".")
	cmd.Dir = pkgDir

	// MPM_WORKSPACE is deliberately ABSENT from the child environment.
	//
	// This is the whole point of the guard, and getting it wrong makes
	// the guard vacuous. If the parent pins MPM_WORKSPACE, then
	// config.GetWorkspace() resolves to the parent's value no matter what
	// the child test does — the test's own isolation is never exercised,
	// the backup lands somewhere this guard calls "correct", and the
	// defect passes silently. The defect is specifically the $HOME/.mpm
	// fallback, so reproducing it requires the fallback to be reachable.
	//
	// Overriding HOME alone is what recreates the hazard faithfully: it
	// is the operator's real ~/.mpm that the test would otherwise hit.
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1",
		// Point the child's module and build caches at the REAL ones,
		// outside the sentinel. Overriding HOME otherwise makes `go
		// test` populate $HOME/.cache/go-build, $HOME/go/pkg/mod and
		// $HOME/.config/go — real writes, but the toolchain's, and
		// identical whether or not the defect is present. `go env` is
		// consulted rather than os.Getenv because these are often unset
		// in the environment while still having a valid default, and
		// passing an empty value would silently re-enable the default
		// (which is derived from HOME — the thing being overridden).
		"GOMODCACHE="+goEnv(t, "GOMODCACHE"),
		"GOCACHE="+goEnv(t, "GOCACHE"),
		"GOTELEMETRY=off",
		"GOTOOLCHAIN=local",
	)
	// Defence in depth: strip any inherited MPM_* workspace override. If
	// the ambient environment pinned one, the fallback would be
	// unreachable and the guard would pass for the wrong reason.
	cmd.Env = withoutEnvPrefix(cmd.Env, "MPM_WORKSPACE=")
	cmd.Env = withoutEnvPrefix(cmd.Env, "MPM_DB_PATH=")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child go test failed (%v); cannot attribute pollution.\n%s",
			err, out)
	}

	after := listFiles(t, sentinel)
	created := difference(after, before)

	// The assertion is about MPM-owned state, which is the invariant that
	// actually matters. The Go toolchain unavoidably writes some counters
	// of its own beneath HOME (telemetry, and the module/build caches when
	// the environment does not pin them); those are identical whether or
	// not the defect is present, and chasing them with environment
	// variables would make this guard test the toolchain instead.
	//
	// Anything the test legitimately creates lives in ITS OWN t.TempDir(),
	// which is outside the sentinel entirely and therefore invisible here.
	// What must never appear is MPM state under the sentinel HOME, because
	// home/.mpm is exactly where the operator's live state would be.
	var escaped []string
	for _, rel := range created {
		if isMPMOwnedPath(rel) {
			escaped = append(escaped, rel)
		}
	}

	if len(escaped) > 0 {
		t.Fatalf("migration test wrote MPM state outside its isolated workspace "+
			"(sentinel %s):\n  %s\n\n"+
			"These landed under the sentinel HOME, which is where the operator's real\n"+
			"~/.mpm would be. A test must call isolateMigrationWorkspace(t) BEFORE\n"+
			"RunMigration/UndoMigration so takeBackup's config.GetWorkspace()\n"+
			"resolves the temp root rather than the $HOME/.mpm fallback.",
			sentinel, strings.Join(escaped, "\n  "))
	}

}

// withoutEnvPrefix removes every "PREFIX..." entry from an env slice.
func withoutEnvPrefix(env []string, prefix string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// isMPMOwnedPath reports whether a sentinel-relative path is MPM state
// rather than Go toolchain bookkeeping.
//
// The toolchain's sentinel-HOME footprints are <home>/.cache/ (build
// cache), <home>/.config/ (telemetry counters) and <home>/go/ (module
// cache). MPM's are everything else — and critically, a leak through the
// $HOME/.mpm fallback always appears as <home>/.mpm/..., never under
// those three.
//
// The comparison is made on the path RELATIVE TO THE SENTINEL HOME, so
// the home/ prefix is stripped first. Matching against the full
// sentinel-relative path would miss every one of them and report Go's
// telemetry as an MPM leak.
func isMPMOwnedPath(rel string) bool {
	underHome := strings.HasPrefix(rel, "home/")
	if !underHome {
		// Anything outside home/ that is not the isolated workspace
		// itself is MPM state by elimination.
		return true
	}
	for _, toolchain := range []string{".cache/", ".config/", "go/"} {
		if strings.HasPrefix(strings.TrimPrefix(rel, "home/"), toolchain) {
			return false
		}
	}
	return true
}

// TestMigrationBackup_StillExercisedInTempWorkspace is the other half of
// the contract. The fix must not have worked by suppressing the backup:
// RunMigration's real behaviour, including takeBackup, must still run and
// still produce its artifact — just inside the temp workspace.
func TestMigrationBackup_StillExercisedInTempWorkspace(t *testing.T) {
	ws := isolateMigrationWorkspace(t)

	dm := NewTestDM(t)
	defer dm.Close()

	// takeBackup is the exact production call the defect turned loose.
	// Exercising it directly keeps this assertion independent of whatever
	// RunMigration's fixtures happen to seed.
	backupPath, err := takeBackup(dm)
	if err != nil {
		t.Fatalf("takeBackup failed: %v", err)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("takeBackup reported %s but no file exists there: %v", backupPath, err)
	}

	rel, err := filepath.Rel(ws, backupPath)
	if err != nil {
		t.Fatalf("relativise %s against %s: %v", backupPath, ws, err)
	}
	if strings.HasPrefix(rel, "..") {
		t.Fatalf("backup escaped the isolated workspace: %s is not under %s",
			backupPath, ws)
	}
	if want := "migrations"; !strings.HasPrefix(rel, want+string(filepath.Separator)) {
		t.Fatalf("backup path = %q; expected it beneath %q/", rel, want)
	}
}

// TestIsolateMigrationWorkspace_PinsEnv is a fast unit check on the helper
// itself, so a regression in the helper is reported as a helper regression
// rather than as an opaque downstream write.
func TestIsolateMigrationWorkspace_PinsEnv(t *testing.T) {
	ws := isolateMigrationWorkspace(t)
	if got := os.Getenv("MPM_WORKSPACE"); got != ws {
		t.Fatalf("MPM_WORKSPACE = %q, want the isolated workspace %q", got, ws)
	}
	if _, err := os.Stat(ws); err != nil {
		t.Fatalf("isolated workspace %s was not created: %v", ws, err)
	}
	// Two calls in one test must not silently share a directory, or a
	// test could read a backup another test wrote.
	if other := isolateMigrationWorkspace(t); other == ws {
		t.Fatalf("isolateMigrationWorkspace returned the same directory twice (%s); "+
			"each call must isolate independently", ws)
	}
}

// goEnv returns the Go toolchain's resolved value for a setting such as
// GOCACHE. It exists because the environment variables are frequently
// unset while still having a valid default derived from HOME — and HOME
// is exactly what this guard overrides, so the default would follow the
// override straight back into the sentinel.
func goEnv(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		t.Fatalf("go env %s: %v", name, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		t.Fatalf("go env %s returned an empty value; the child go test would "+
			"fall back to a HOME-derived default inside the sentinel", name)
	}
	return v
}

// listFiles returns every file under root, as slash-separated relative
// paths, so two snapshots can be diffed by value.
func listFiles(t *testing.T, root string) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = struct{}{}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// difference returns the entries present in after but not before.
func difference(after, before map[string]struct{}) []string {
	var added []string
	for k := range after {
		if _, existed := before[k]; !existed {
			added = append(added, k)
		}
	}
	sortStrings(added)
	return added
}

// sortStrings is a tiny insertion sort. The slices here are a handful of
// file paths, so pulling in sort for this would be noise.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
