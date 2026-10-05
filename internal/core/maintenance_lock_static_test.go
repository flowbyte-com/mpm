package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaintenanceLeaseOnlyThroughHelpers enforces the H-4 invariant
// that the flock-based maintenance lease on `<dbPath>.lock` is
// acquired and released ONLY through the canonical helpers in
// maintenance_lock.go. Raw syscall.Flock calls in production code are
// forbidden, for two reasons:
//
//  1. Cross-platform correctness. The shared/exclusive protocol
//     described at the top of maintenance_lock.go depends on Linux
//     flock's per-process FD behaviour (LOCK_SH + LOCK_EX merge on the
//     same process). macOS/BSD flock is per-FD and would deadlock
//     against itself in the merge path. The helpers
//     (AcquireSharedMaintenanceLock, AcquireExclusiveMaintenanceLock,
//     ReleaseSharedMaintenanceLock, ReleaseExclusiveMaintenanceLock)
//     are the only places that intentionally decide when to merge
//     vs. release; duplicating flock elsewhere invites a future
//     contributor to re-introduce the Linux-merge-only bug.
//
//  2. Lock-identity correctness. MaintenanceLockPath (and the
//     ActiveDBPath helper) are the SINGLE source of truth for the
//     canonical lock file path, including EvalSymlinks resolution and
//     the MPM_DB_LOCK test backstop. A raw syscall.Flock call against
//     a hand-constructed `<dbPath>.lock` string would diverge from
//     the canonical identity and break the protocol across symlink
//     aliases (e.g. /real/workspace vs /symlink/to/workspace).
//
// Whitelist of acceptable syscall.Flock call sites:
//   - internal/core/maintenance_lock.go: the canonical helpers
//     themselves. This is the only production code in the repo that
//     is permitted to call syscall.Flock directly.
//   - internal/scheduler/scheduler.go: AcquireLock is the cascade-
//     preflight helper for `<cascadeLockPath>`, NOT the maintenance
//     lease. It is a separate lock file with a separate protocol; the
//     test guards the maintenance lease, not the cascade preflight.
//   - cmd/mpm/handlers_shred_database_reset.go: this used to call
//     syscall.Flock directly inside preflightShredDatabase; the §11
//     corrective revision re-routed it through
//     AcquireExclusiveMaintenanceLockAt. The whitelist below reflects
//     the corrected state — if a future contributor re-introduces a
//     raw syscall.Flock call here, the test fails and the regression
//     is caught at PR time.
//   - All _test.go files: tests legitimately need raw flock for
//     scenarios the helpers don't (multiple holders, stale FDs,
//     cross-process scenarios via subprocess helpers, etc.).
//
// If you need a new maintenance-lease acquisition pattern, add a
// helper to maintenance_lock.go (or extend an existing one) and route
// the caller through that helper. Do NOT inline syscall.Flock.
func TestMaintenanceLeaseOnlyThroughHelpers(t *testing.T) {
	// Production files where raw syscall.Flock is acceptable. Basename
	// match so the test works regardless of cwd. The path-to-cwd
	// resolution mirrors TestDatabaseManagerIsOnlyOwnerOfSqlOpen.
	whitelist := map[string]bool{
		// The canonical helpers themselves — the only place a raw
		// flock is acceptable in production code.
		"maintenance_lock.go": true,
		// Cascade preflight. NOT a maintenance-lease acquisition;
		// cascade.lock is a separate file with a separate protocol.
		"scheduler.go": true,
		// Session-ID coordination flock on `<root>/active.json.lock`
		// (NOT `<dbPath>.lock`). acquireFlock + withActiveJSONFlock
		// prevent split-brain between concurrent processes reading/
		// writing the mpm_session_id field. This is a separate
		// concern from the maintenance lease and pre-dates H-4 —
		// the test guards the maintenance lease, not session ID
		// coordination. If a future contributor adds a raw flock
		// here for the DB path, it would still be caught by
		// TestMaintenanceLockIdentityOnlyThroughHelpers (the .lock
		// string whitelist); this whitelist entry only opts the
		// file OUT of the syscall.Flock inspection.
		"session_persistence.go": true,
	}

	// Scan from the internal/core/ directory. Mirrors
	// TestDatabaseManagerIsOnlyOwnerOfSqlOpen's scan footprint so the
	// regression covers the same surface.
	dirs := []string{".", "../cmd/mpm", "../scheduler", "../critic"}
	violations := map[string][]string{} // file -> call positions

	for _, dir := range dirs {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
		if err != nil {
			continue
		}
		for _, pkg := range pkgs {
			for _, f := range pkg.Files {
				filename := fset.Position(f.Pos()).Filename
				base := filepath.Base(filename)
				if strings.HasSuffix(base, "_test.go") {
					continue
				}
				ast.Inspect(f, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					fn, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if fn.Sel.Name != "Flock" {
						return true
					}
					x, ok := fn.X.(*ast.Ident)
					if !ok || x.Name != "syscall" {
						return true
					}
					pos := fset.Position(call.Pos())
					if !whitelist[base] {
						violations[base] = append(violations[base], pos.String())
					}
					return true
				})
			}
		}
	}

	for file, sites := range violations {
		t.Errorf("raw syscall.Flock in %s is not whitelisted. The maintenance-lease rule says "+
			"every flock on <dbPath>.lock must go through the canonical helpers in "+
			"internal/core/maintenance_lock.go (AcquireSharedMaintenanceLock / "+
			"AcquireExclusiveMaintenanceLock / ReleaseSharedMaintenanceLock / "+
			"ReleaseExclusiveMaintenanceLock). Either:\n"+
			"  1. Route the call through the canonical helper\n"+
			"  2. If the flock is NOT a maintenance-lease acquisition (e.g. cascade.lock, "+
			"a per-feature lock), add %q to the whitelist in maintenance_lock_static_test.go "+
			"with a justifying comment\n"+
			"Violations at: %s", file, file, strings.Join(sites, ", "))
	}
}

// TestMaintenanceLockIdentityOnlyThroughHelpers enforces that the
// canonical lock file path is computed from one source of truth
// (MaintenanceLockPath) — and not hand-constructed as
// "<dbPath>.lock" in callers. The latter would diverge from the
// canonical identity if MaintenanceLockPath ever changes its scheme
// (e.g. a future hash-based identity for multi-workspace support), and
// would silently break the cross-symlink aliasing guarantee.
//
// Whitelist: maintenance_lock.go is the ONLY production file
// permitted to construct a "<dbPath>.lock" string. Every other
// production file MUST use MaintenanceLockPath or the env-var-
// override route inside the canonical helpers.
func TestMaintenanceLockIdentityOnlyThroughHelpers(t *testing.T) {
	whitelist := map[string]bool{
		// The canonical helpers themselves.
		"maintenance_lock.go": true,
	}

	dirs := []string{".", "../cmd/mpm", "../scheduler", "../critic"}
	violations := map[string][]string{} // file -> matching lines

	for _, dir := range dirs {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
		if err != nil {
			continue
		}
		for _, pkg := range pkgs {
			for _, f := range pkg.Files {
				filename := fset.Position(f.Pos()).Filename
				base := filepath.Base(filename)
				if strings.HasSuffix(base, "_test.go") {
					continue
				}
				// Scan for string literals that match the maintenance
				// lock file suffix ".lock" combined with a database
				// file path. The pattern catches ".lock" appended to
				// any string variable whose name suggests a DB path
				// (dbPath, lockPath is allowed when MaintenanceLockPath
				// returned it). The heuristic: a string literal that
				// ends with ".lock" inside an expression that looks
				// like a path concatenation is flagged.
				ast.Inspect(f, func(n ast.Node) bool {
					lit, ok := n.(*ast.BasicLit)
					if !ok || lit.Kind.String() != "string" {
						return true
					}
					val := strings.Trim(lit.Value, `"`)
					if !strings.HasSuffix(val, ".lock") {
						return true
					}
					// Skip constants like ".pre-restore" — only flag
					// bare ".lock" or "<something>.lock" pattern.
					if !strings.HasSuffix(val, "/.lock") && !strings.Contains(val, "db.lock") && val != ".lock" {
						return true
					}
					pos := fset.Position(lit.Pos())
					if !whitelist[base] {
						violations[base] = append(violations[base], pos.String()+" ("+val+")")
					}
					return true
				})
			}
		}
	}

	for file, sites := range violations {
		t.Errorf("hand-constructed .lock string in %s. The maintenance-lock identity rule says "+
			"the canonical lock file path is computed ONLY via internal/core.MaintenanceLockPath. "+
			"Hand-constructing %q would diverge from the canonical identity if the helper ever "+
			"changes its scheme, and would silently break cross-symlink aliasing. Either:\n"+
			"  1. Route the caller through MaintenanceLockPath (or the canonical helpers in "+
			"maintenance_lock.go)\n"+
			"  2. If the .lock string is for a NON-maintenance lock file (e.g. cascade.lock, "+
			"scheduler.lock), add %q to the whitelist with the matching string\n"+
			"Violations at: %s", file, file, file, strings.Join(sites, ", "))
	}
}
