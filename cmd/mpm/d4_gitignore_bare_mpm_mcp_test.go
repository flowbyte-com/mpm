// d4_gitignore_bare_mpm_mcp_test.go — drift lock for the .gitignore
// scope defect (D4 audit, 2026-09-04).
//
// Audit claim: a bare `mpm-mcp` line in .gitignore is over-broad —
// it matches any file or directory with that literal name anywhere
// in the repo, including source files under cmd/mpm-mcp/ (the MCP
// server's own source dir). The bare pattern silently hides new
// files added under cmd/mpm-mcp/ unless those files already live in
// the git index.
//
// Reproducer (pre-fix):
//
//   $ git check-ignore -v cmd/mpm-mcp/call.go
//   .gitignore:109:mpm-mcp    cmd/mpm-mcp/call.go
//   $ git check-ignore -v cmd/mpm-mcp/main.go
//   .gitignore:109:mpm-mcp    cmd/mpm-mcp/main.go
//
// A bare pattern (no leading slash, no slashes, no wildcards) is the
// exact shape that gitignore matches ANY path with that basename.
// The canonical targets already have explicit ignores (bin/ at line 7
// and cmd/mpm-mcp/mpm-mcp at line 132), so the bare name is both
// redundant AND a latent trap.
//
// Test strategy: structural scan of .gitignore for the bare `mpm-mcp`
// pattern. Watching the test fail forces the fix; watching it pass
// after the fix pins the contract — any future reintroduction of
// the bare pattern (e.g. by a careless copy-paste from a sibling
// binary rule like `/mpm-critic` at line 19) trips immediately.
//
// Companion rules that ARE legitimate:
//   - `/mpm-critic`     (line 19)  — leading-slash → repo-root only
//   - `/mpm-scheduler`  (line 20)  — leading-slash → repo-root only
//   - `mpm-mcp`         (line 109) — bare → ANY path ← the defect
//
// The fix narrows line 109 into explicit binary paths. The bin/
// rule (line 7) already covers the canonical output.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestD4_GitignoreHasNoBareMpmMCPPattern pins that .gitignore does not
// contain a bare `mpm-mcp` pattern. The bare pattern over-matches —
// any file with that name in any subtree (source, doc, fixture, etc.)
// is silently ignored.
//
// Walking the repo at runtime (`git check-ignore -v`) would re-use
// the very machinery the audit surfaced; structural scan is the
// cheaper, more honest pin. The repo root is computed from the test's
// location (`cmd/mpm/` → two levels up), so the test stays portable
// if the package is ever moved.
func TestD4_GitignoreHasNoBareMpmMCPPattern(t *testing.T) {
	// Locate the repo root. `go test` runs with cwd = the package dir
	// (`cmd/mpm/`); the canonical pattern is 2 levels up.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Dir(filepath.Dir(cwd))
	gitignorePath := filepath.Join(root, ".gitignore")
	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Skipf(".gitignore not found at %s: %v", gitignorePath, err)
	}

	var offenders []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		// Skip blanks, comments, and negation lines (those un-ignore
		// something and use a different syntax; not the audit's class).
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		// A bare pattern has no leading slash and no `/` inside.
		// That is the structural shape that matches the literal name
		// in every directory of the repo.
		if strings.HasPrefix(line, "/") {
			continue
		}
		if strings.Contains(line, "/") {
			continue
		}
		// Single-char wildcards (`*`, `[…]`) are structurally similar
		// to a bare-name pattern but are NOT the audit's class; pin
		// the literal-name case only and leave more general linting
		// for a future audit. (The brief says: "Do not enlarge scope".)
		if strings.ContainsAny(line, "*?[") {
			continue
		}
		if line == "mpm-mcp" {
			offenders = append(offenders, line)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("D4 .gitignore scope defect: .gitignore contains bare pattern(s) %v "+
			"that over-match (audit surfaced on 2026-09-04). A bare name pattern matches "+
			"any file/directory with that name in any subdir, including source files "+
			"under cmd/mpm-mcp/. Use a leading-slash root-anchored pattern (e.g. "+
			"`/mpm-mcp`) or an explicit relative path (e.g. `bin/mpm-mcp`). The "+
			"canonical targets are already covered by `bin/` (line 7) and "+
			"`cmd/mpm-mcp/mpm-mcp` (line 132).", offenders)
	}
}
