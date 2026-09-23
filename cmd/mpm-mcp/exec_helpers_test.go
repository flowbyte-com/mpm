// exec_helpers_test.go — Shared helpers for cmd/mpm-mcp tests that
// need to spawn a real mpm-mcp subprocess.
//
// Mirrors cmd/mpm/exec_helpers_test.go. The shared build helper at
// the cmd/mpm level is intentionally NOT imported across packages
// (test binaries under cmd/mpm-mcp are separate; cross-package
// test helpers complicate Go test caching). Each cmd-side package
// gets its own minimal helper.
//
// Background: pre-fix fixtures hardcoded
// "/home/v/workspace/projects/mpm/bin/mpm-mcp" — the original
// author's checkout. Under any other user the binary did not
// exist and the test failed with "no such file or directory".
//
// Hermetic repair: build the current source tree's mpm-mcp into
// t.TempDir() and use that exact binary against an isolated
// MPM_WORKSPACE. The test exercises the source under test, not
// a specific user's install.

package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// mcpCmd builds a fresh `mpm-mcp` binary from the current source
// tree (./cmd/mpm-mcp) into a per-test temp directory and returns
// its absolute path. The build uses the same flags as production
// (`-tags fts5`). Per-test isolation guarantees the test exercises
// the current code under test regardless of host environment.
//
// Cached by Go's build cache; sub-second on warm cache.
func mcpCmd(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-mcp-test")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build mpm-mcp: %v\n%s", err, out)
	}
	return bin
}
