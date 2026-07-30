package internal

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// timestampWhitelist identifies Go source locations where it's acceptable to
// write RFC3339-formatted strings into a JSON sidecar (active.json, scratchpad
// JSON, working-context JSON). These are NOT database columns and stay RFC3339.
//
// Format: "relative/path/file.go:lineno" → "reason".
var timestampWhitelist = map[string]string{
	// Populated below as the implementer identifies legitimate sites.
	// Example:
	// "cmd/mpm/handlers_stance.go:69": "active.json Updated field, not a DB column",
}

// TestTimestampWritePaths_NoRFC3339IntoMigratedColumns walks every Go file in
// internal/ and cmd/ and asserts no call to time.Now().UTC().Format(time.RFC3339)
// flows into a migrated database column. SQLite's manifest typing would
// silently accept strings stored in INTEGER columns; this test catches that
// at build time.
func TestTimestampWritePaths_NoRFC3339IntoMigratedColumns(t *testing.T) {
	if len(allTimestampsToMigrate) == 0 {
		t.Fatal("allTimestampsToMigrate is empty; test would pass vacuously")
	}

	fset := token.NewFileSet()
	migratedCols := map[string]bool{}
	for _, tc := range allTimestampsToMigrate {
		migratedCols[tc.col] = true
	}

	violations := []string{}

	// Tests run with cwd = the package directory (internal/core/), so:
	//   internal/ -> parse from "."
	//   cmd/mpm/  -> parse from "../../cmd/mpm"
	dirs := []string{".", "../../cmd/mpm"}

	for _, path := range dirs {
		pkgs, err := parser.ParseDir(fset, path, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("ParseDir(%q): %v", path, err)
		}
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				pos := fset.Position(file.Pos())
				if strings.HasSuffix(pos.Filename, "_test.go") {
					continue
				}

				ast.Inspect(file, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if sel.Sel.Name != "Exec" && sel.Sel.Name != "Query" && sel.Sel.Name != "QueryRow" {
						return true
					}

					if len(call.Args) < 1 {
						return true
					}
					sqlLit, ok := call.Args[0].(*ast.BasicLit)
					if !ok || sqlLit.Kind != token.STRING {
						return true
					}
					sql := strings.Trim(sqlLit.Value, "\"`")
					hasMigratedCol := false
					for col := range migratedCols {
						if strings.Contains(sql, col) {
							hasMigratedCol = true
							break
						}
					}
					if !hasMigratedCol {
						return true
					}

					for _, arg := range call.Args[1:] {
						if isRFC3339FormatCall(arg) {
							argPos := fset.Position(arg.Pos())
							key := relativeKey(argPos)
							if _, ok := timestampWhitelist[key]; !ok {
								violations = append(violations, fmt.Sprintf(
									"%s: RFC3339-format call flows into migrated column. Add to timestampWhitelist with justification, or change to time.Now().Unix()",
									key,
								))
							}
						}
					}
					return true
				})
			}
		}
	}

	if len(violations) > 0 {
		for _, v := range violations {
			t.Error(v)
		}
		t.Fatalf("%d timestamp write-path violations found", len(violations))
	}
}

// isRFC3339FormatCall reports whether expr matches the shape
//   time.Now().UTC().Format(time.RFC3339)
// (or time.Now().Format(time.RFC3339)).
func isRFC3339FormatCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "Format" {
		return false
	}
	if len(call.Args) < 1 {
		return false
	}
	argSel, ok := call.Args[0].(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if argSel.Sel.Name != "RFC3339" && argSel.Sel.Name != "RFC3339Nano" {
		return false
	}
	recv := sel.X
	if utc, ok := recv.(*ast.CallExpr); ok {
		if s, ok := utc.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "UTC" {
			recv = s.X
		}
	}
	nowCall, ok := recv.(*ast.CallExpr)
	if !ok {
		return false
	}
	nowSel, ok := nowCall.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return nowSel.Sel.Name == "Now"
}

// relativeKey produces a repo-root-relative path:line key for use in error
// messages and the timestampWhitelist map. Walks up from the file's directory
// looking for the nearest enclosing go.mod (which identifies the module that
// owns the file). Falls back to the absolute filename when no go.mod ancestor
// is found (e.g. running outside the repo).
//
// Why walk up from the file rather than from cwd: tests run with cwd =
// internal/core/ (which has its own go.mod, since core is a separate module),
// but the file being inspected may live under ../../cmd/mpm, which is owned
// by the parent module's go.mod. Anchoring on the file's directory makes the
// produced key correct regardless of which module owns the source file.
func relativeKey(pos token.Position) string {
	fileDir := filepath.Dir(pos.Filename)
	root := fileDir
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return fmt.Sprintf("%s:%d", pos.Filename, pos.Line)
		}
		root = parent
	}
	key := pos.Filename
	if strings.HasPrefix(key, root+"/") {
		key = strings.TrimPrefix(key, root+"/")
	}
	return fmt.Sprintf("%s:%d", key, pos.Line)
}
