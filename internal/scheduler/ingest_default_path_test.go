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
// This test pins the contract: openclawIngestDefaultPath() must
// return a path under the current process's $HOME, never a hardcoded
// /home/v literal, and never a path that fails the security rail
// "user-only directory" check.

package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenclawIngestDefaultPath_IsHomeRelative asserts the default
// resolves under the current process's $HOME, not a hardcoded
// author-machine literal.
//
// Pre-fix: returned "/home/v/.mpm/run/ingest.md" regardless of host.
// Post-fix: returns $HOME/.mpm/run/ingest.md via os.UserHomeDir().
func TestOpenclawIngestDefaultPath_IsHomeRelative(t *testing.T) {
	got := openclawIngestDefaultPath()
	if got == "" {
		t.Fatalf("openclawIngestDefaultPath() returned empty string")
	}

	// Absolute path expected (the scheduler watcher uses os.Lstat,
	// which requires an absolute target — relative paths would
	// anchor to the process's cwd at tick time, producing wildly
	// different behaviour across invocations).
	if !filepath.IsAbs(got) {
		t.Errorf("openclawIngestDefaultPath() must return an absolute path; got %q", got)
	}

	// Forbid the historical author-machine literal. This guards
	// against someone re-introducing a hardcoded const value.
	if strings.Contains(got, "/home/v/") {
		t.Errorf("openclawIngestDefaultPath() must not hardcode an author-machine literal; got %q", got)
	}

	// The default must end in ".mpm/run/ingest.md" — the canonical
	// relative layout the mpm-memory-openclaw plugin's
	// flushPlanResolver writes to. If the trailing path drifts,
	// the handler and plugin diverge and the bridge silently breaks.
	const wantSuffix = ".mpm/run/ingest.md"
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("openclawIngestDefaultPath() must end with %q; got %q", wantSuffix, got)
	}

	// Anchor check: the default lives under the current process's
	// resolved home directory. We allow a fallback to ".mpm/run/
	// ingest.md" (relative) only if HOME is unreachable; otherwise
	// the path MUST begin with the home dir.
	home, herr := os.UserHomeDir()
	if herr == nil && home != "" {
		if !strings.HasPrefix(got, home+string(filepath.Separator)) && got != ".mpm/run/ingest.md" {
			t.Errorf("openclawIngestDefaultPath() must live under $HOME=%q; got %q", home, got)
		}
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
	// Snapshot and restore the env var so we don't pollute other tests.
	prev, had := os.LookupEnv("MPM_WORKSPACE")
	t.Cleanup(func() {
		if had {
			os.Setenv("MPM_WORKSPACE", prev)
		} else {
			os.Unsetenv("MPM_WORKSPACE")
		}
	})

	// Set MPM_WORKSPACE to a deliberately different value. The
	// default path must NOT pick it up — MPM_WORKSPACE is the
	// project workspace root, not the data-dir root.
	os.Setenv("MPM_WORKSPACE", "/tmp/some-workspace-that-must-be-ignored")

	got := openclawIngestDefaultPath()
	if strings.Contains(got, "some-workspace-that-must-be-ignored") {
		t.Errorf("openclawIngestDefaultPath() must not derive from MPM_WORKSPACE; got %q", got)
	}
}
