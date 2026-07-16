package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestDatabaseManagerIsOnlyOwnerOfSqlOpen enforces the invariant from
// CLAUDE.md: DatabaseManager is the *only* owner of *sql.DB connections.
// Other code paths must use the shared DatabaseManager (via NewDatabaseManager
// or via an injected dm *DatabaseManager).
//
// The rule exists because:
//   1. SQLite has a global write lock per file. Opening multiple connections
//      to the same file leads to SQLITE_BUSY races.
//   2. The single-connection model enforces SetMaxOpenConns(1)-ish behaviour
//      for predictable contention.
//   3. The hostile-audit bug (RunLifecycleDecayAndArchival opening a new
//      connection per tick) was caused by a violation of this rule.
//
// Whitelist of acceptable sql.Open call sites:
//   - internal/db.go: NewDatabaseManager opens the main DB
//   - internal/adapters.go: opens EXTERNAL sqlite files (foreign DBs, not
//     the workspace's mpm.db). Acceptable because they don't compete with
//     the main connection.
//   - internal/ingest.go: same — external sqlite files for import.
//   - cmd/mpm/main.go: opens the workspace DB read-only (`?mode=ro`) for
//     deep-scan checks; safe because reads don't contend with writes.
//   - cmd/mpm/handlers_backup.go: opens the workspace DB read-only for
//     backup verification; same reasoning.
//   - internal/testhelpers.go: opens per-test in-memory shared-cache DBs
//     for the test suite; not a production connection.
//
// If you need a new *sql.DB connection, add the call site to this whitelist
// with a justifying comment, OR (preferred) extend DatabaseManager.
func TestDatabaseManagerIsOnlyOwnerOfSqlOpen(t *testing.T) {
	whitelist := map[string]bool{
		// Production files where sql.Open is acceptable. Path is the
		// file's basename so the test works regardless of cwd.
		"db.go":              true, // canonical owner of the main DB connection
		"adapters.go":        true, // opens foreign sqlite files for ingest
		"ingest.go":          true, // opens foreign sqlite files for ingest
		"main.go":            true, // read-only opens for doctor deep-scan
		"handlers_backup.go":       true, // read-only opens for backup integrity check
		"sql_dump_validator.go":    true, // read-only opens for restore-db schema introspection (C-2)
		"live_schema_validator.go": true, // read-only opens for restore-db live-schema variant (C-2)
		"route_render.go":    true, // read-only opens for wake-context route rendering
		"testhelpers.go":     true, // opens hermetic in-memory DBs for tests; not a production connection
	}

	// Scan from the internal/core/ package directory. The test runs with
	// cwd = internal/core/, so "." scans core and "../cmd/mpm" scans the CLI.
	//
	// 2026-07-16 F-007 expansion: added ../scheduler and ../critic which
	// both follow the F-007 contract — they accept a *sql.DB via New()
	// and never call sql.Open directly. Any future regression that re-opens
	// the workspace DB in those packages will fail this test.
	dirs := []string{".", "../cmd/mpm", "../scheduler", "../critic"}
	violations := map[string][]string{} // file -> lines

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
					if fn.Sel.Name != "Open" {
						return true
					}
					x, ok := fn.X.(*ast.Ident)
					if !ok || x.Name != "sql" {
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
		t.Errorf("sql.Open in %s is not whitelisted. The single-connection rule says "+
			"DatabaseManager is the only owner of *sql.DB connections. Either:\n"+
			"  1. Use the existing DatabaseManager via NewDatabaseManager / an injected *DatabaseManager\n"+
			"  2. Add %q to the whitelist in sqlopen_owner_test.go with a justifying comment\n"+
			"Violations at: %s", file, file, strings.Join(sites, ", "))
	}
}