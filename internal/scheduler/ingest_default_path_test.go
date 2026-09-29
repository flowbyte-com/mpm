// internal/scheduler/ingest_default_path_test.go
//
// Regression test for the ingest handler's canonical default path.
//
// Background: before this fix, the production ingest handler's
// default target was a hardcoded literal constant
// "/home/v/.mpm/run/ingest.md". On any host other than the original
// author's, the handler would silently watch the wrong path — a
// fresh ingestion at the user's real $HOME/.mpm/run/ingest.md would
// not be picked up, and the scheduler would log a steady stream of
// "lstat failed: no such file or directory" warnings against the
// author's nonexistent path.
//
// Fix: openclawIngestDefaultPath() now resolves to $HOME/.mpm/run/
// ingest.md at call time via os.UserHomeDir(), so the handler watches
// the canonical install root on whatever host it's running on.
//
// This file pins the contract behaviourally: the path is DERIVED FROM
// HOME, demonstrated by showing it tracks a synthetic HOME. It does
// not assert anything about any particular developer's username.

package scheduler

import (
	"path/filepath"
	"strings"
	"testing"
)

// historicalHardcodedIngestPath is the exact literal that caused the
// incident. It is pinned by EQUALITY, not by substring search, and it
// is not the primary defence — the derivation test below is. Keeping
// it as an exact constant is safe: it names one specific value that
// existed, rather than a pattern that would also match a correct
// implementation running on the author's own machine (which is
// precisely how the previous version of this test broke).
const historicalHardcodedIngestPath = "/home/v/.mpm/run/ingest.md"

// openclawIngestRelativeSuffix is the canonical relative layout under
// the home directory. The mpm-memory-openclaw plugin's
// flushPlanResolver writes to this path; if the trailing layout
// drifts, the handler and plugin diverge and the bridge silently
// breaks.
var openclawIngestRelativeSuffix = []string{".mpm", "run", "ingest.md"}

// TestOpenclawIngestDefaultPath_DerivesFromHome is the real contract
// test: the default is computed from the CURRENT process's home
// directory, not embedded.
//
// Method: run the resolver under two different synthetic homes. A
// derived implementation produces a different, correct answer for
// each; a hardcoded one returns the same string both times. This
// proves derivation without naming any username, so the test is
// independent of who runs it — the failure mode that made the
// previous version of this test environment-dependent.
//
// HOME is controlled rather than read from the ambient environment so
// the assertions are exact instead of conditional.
func TestOpenclawIngestDefaultPath_DerivesFromHome(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	if homeA == homeB {
		t.Fatal("t.TempDir returned the same path twice; cannot distinguish derivation")
	}

	t.Setenv("HOME", homeA)
	gotA := openclawIngestDefaultPath()

	t.Setenv("HOME", homeB)
	gotB := openclawIngestDefaultPath()

	// Absolute is required: the scheduler watcher uses os.Lstat, which
	// needs an absolute target — a relative path would anchor to the
	// process cwd at tick time, producing wildly different behaviour
	// across invocations.
	for _, tc := range []struct {
		home string
		got  string
	}{{homeA, gotA}, {homeB, gotB}} {
		if !filepath.IsAbs(tc.got) {
			t.Errorf("with HOME=%q, openclawIngestDefaultPath() = %q; want an absolute path", tc.home, tc.got)
		}
		want := filepath.Join(append([]string{tc.home}, openclawIngestRelativeSuffix...)...)
		if tc.got != want {
			t.Errorf("with HOME=%q, openclawIngestDefaultPath() = %q; want %q (derived from HOME)", tc.home, tc.got, want)
		}
	}

	// The core assertion: the result FOLLOWS HOME. A hardcoded
	// implementation returns one constant for both homes and fails
	// here.
	if gotA == gotB {
		t.Fatalf("openclawIngestDefaultPath() returned %q for two different HOME values; "+
			"the path is embedded, not derived from the user's home directory", gotA)
	}

	// The specific historical regression, pinned by equality.
	for _, got := range []string{gotA, gotB} {
		if got == historicalHardcodedIngestPath {
			t.Errorf("openclawIngestDefaultPath() returned the hardcoded incident value %q", got)
		}
	}
}

// TestOpenclawIngestDefaultPath_FallsBackWhenHomeUnavailable covers
// the other branch: with no resolvable home, the resolver returns the
// bare relative convention rather than an empty or "/"-anchored path.
// On unix os.UserHomeDir() reports an error when $HOME is empty, so
// clearing it is a faithful way to exercise the fallback.
func TestOpenclawIngestDefaultPath_FallsBackWhenHomeUnavailable(t *testing.T) {
	t.Setenv("HOME", "")

	got := openclawIngestDefaultPath()
	want := filepath.Join(openclawIngestRelativeSuffix...)
	if got != want {
		t.Fatalf("with HOME unset, openclawIngestDefaultPath() = %q; want the relative fallback %q", got, want)
	}
}

// TestOpenclawIngestDefaultPath_HonoursMPMWorkspace is a no-op
// sanity check: the current API does not honour MPM_WORKSPACE
// because the canonical ingest target is a fixed relative layout
// under the user's data root, not the project workspace. This test
// documents the contract so a future "make it configurable" patch
// has to acknowledge the change. (If you need workspace-relative
// behaviour, use newIngestHandlerWithPath — that test seam is
// explicitly the configurable surface.)
func TestOpenclawIngestDefaultPath_HonoursMPMWorkspace(t *testing.T) {
	// Controlled home so the assertion cannot be satisfied by an
	// ambient-environment coincidence.
	t.Setenv("HOME", t.TempDir())
	ignoredWorkspace := filepath.Join(t.TempDir(), "some-workspace-that-must-be-ignored")
	t.Setenv("MPM_WORKSPACE", ignoredWorkspace)

	got := openclawIngestDefaultPath()
	if strings.Contains(got, "some-workspace-that-must-be-ignored") {
		t.Errorf("openclawIngestDefaultPath() must not derive from MPM_WORKSPACE; got %q", got)
	}
}
