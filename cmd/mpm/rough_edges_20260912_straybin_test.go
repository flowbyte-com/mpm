// Item 8 (REMOVE): no regular-file binary named mpm (or siblings) may
// live at the repo root — a bare `go build ./cmd/mpm` drops a non-FTS5
// binary there that shadows the canonical bin/mpm in scripts and PATH
// confusion. Symlinks are tolerated (historical go-tooling shim);
// regular files are not.
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRough_Item8_NoStrayRootBinary(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot locate test file")
	}
	root := filepath.Dir(filepath.Dir(thisFile)) // cmd/mpm/.. → repo root
	for _, name := range []string{"mpm", "mpm-mcp", "mpm-scheduler", "mpm-critic", "mpm-telemetry"} {
		p := filepath.Join(root, name)
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("lstat %s: %v", p, err)
		}
		if fi.Mode().IsRegular() {
			t.Errorf("stray root binary %s (regular file) — canonical output is bin/%s; remove it (see `make clean`)", p, name)
		}
	}
}
