package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestScannerCoverage_AllMemoriesWritersScanContent enforces the invariant
// from docs/security-review-2026-06-15: every Go function that writes to
// the `memories` table MUST call isSensitiveContent OR isPoisoned on the
// user-supplied content (or pass it through a helper that does, such as
// SaveMemory / MemoryStore.AddMemory / AddMemoryWithWeight).
//
// This is a static-analysis test (not a runtime test). It walks the
// internal/ and cmd/mpm/ trees, finds every func that contains a literal
// `INSERT INTO memories` SQL statement, and verifies the same function
// (or a function it calls) invokes the scanner. The whitelist of
// "trusted writers" with fixed content (no user input) is explicit.
//
// If you add a new write path, this test will tell you to scan it.
func TestScannerCoverage_AllMemoriesWritersScanContent(t *testing.T) {
	// trustedWriter lists functions that intentionally bypass the scanner
	// because their content is a hardcoded constant (no user input).
	trustedWriters := map[string]bool{
		"saveSelfHealState": true, // self-heal rate-limit marker
	}

	dirs := []string{"internal", "cmd/mpm"}

	scannerCallees := map[string]bool{
		"isSensitiveContent": true,
		"isPoisoned":         true,
		"ScanContentForWrite": true, // exported wrapper for callers outside internal/
	}

	writers := map[string][]string{} // func name -> file:line of INSERT

	for _, dir := range dirs {
		// Tests run with cwd = the package directory (internal/), so:
		//   internal/ -> parse from "."
		//   cmd/mpm/  -> parse from "../cmd/mpm"
		path := dir
		if dir == "internal" {
			path = "."
		} else if dir == "cmd/mpm" {
			path = "../cmd/mpm"
		}
		collectWriters(path, writers)
	}

	for fname, sites := range writers {
		if trustedWriters[fname] {
			continue
		}
		// Each write site must be in a function whose body (or a function it calls)
		// invokes the scanner. We check:
		//   1. Does the function body itself call isSensitiveContent / isPoisoned?
		//   2. Is the function name in scannerCallees (transitively scanning wrappers)?
		//   3. Does any caller of this function scan before invoking it?
		if bodyCallsScanner(fname, scannerCallees) {
			continue
		}
		if scannerCallees[fname] {
			continue
		}
		if anyCallerScans(fname, scannerCallees, dirs) {
			continue
		}

		t.Errorf("write path %s (at %s) does not scan user content for secrets/poison; "+
			"add isSensitiveContent()/isPoisoned() check before the INSERT, "+
			"or add the function to trustedWriters with a justifying comment",
			fname, strings.Join(sites, ", "))
	}
}

// collectWriters walks dir, finds every function whose body contains a
// literal `INSERT INTO memories` statement, and records func-name -> file:line.
//
// Skips:
//   - _test.go files (tests insert fixtures; not production write paths)
//   - INSERTs into memories_fts (the FTS5 virtual table, shadow table)
func collectWriters(dir string, writers map[string][]string) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
	if err != nil {
		return
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			pos := fset.Position(f.Pos())
			filename := pos.Filename
			if strings.HasSuffix(filename, "_test.go") {
				continue
			}
			if strings.HasSuffix(filename, "scanner_coverage_test.go") {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					return true
				}
				var insertSites []token.Pos
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					bl, ok := n.(*ast.BasicLit)
					if !ok || bl.Kind != token.STRING {
						return true
					}
					if strings.Contains(bl.Value, "INSERT INTO memories") &&
						!strings.Contains(bl.Value, "INSERT INTO memories_fts") {
						insertSites = append(insertSites, bl.Pos())
					}
					return true
				})
				if len(insertSites) == 0 {
					return true
				}
				name := fn.Name.Name
				for _, p := range insertSites {
					writers[name] = append(writers[name], fset.Position(p).String())
				}
				return true
			})
		}
	}
}

// calledName returns the trailing identifier name of a CallExpr's Fun,
// whether it's a bare identifier (isSensitiveContent) or a selector
// expression (mpminternal.ScanContentForWrite).
func calledName(call *ast.CallExpr) (string, bool) {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name, true
	case *ast.SelectorExpr:
		return fn.Sel.Name, true
	}
	return "", false
}

// bodyCallsScanner returns true if fname's body contains a direct call to
// any of the scanner callees (isSensitiveContent / isPoisoned / ScanContentForWrite).
func bodyCallsScanner(fname string, scannerCallees map[string]bool) bool {
	dirs := []string{".", "../cmd/mpm"}
	for _, dir := range dirs {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, nil, 0)
		if err != nil {
			continue
		}
		for _, pkg := range pkgs {
			for _, f := range pkg.Files {
				var found bool
				ast.Inspect(f, func(n ast.Node) bool {
					fn, ok := n.(*ast.FuncDecl)
					if !ok || fn.Body == nil || fn.Name.Name != fname {
						return true
					}
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						call, ok := n.(*ast.CallExpr)
						if !ok {
							return true
						}
						name, ok := calledName(call)
						if !ok {
							return true
						}
						if scannerCallees[name] {
							found = true
							return false
						}
						return true
					})
					return false
				})
				if found {
					return true
				}
			}
		}
	}
	return false
}

// anyCallerScans walks all functions and checks whether any caller of fname
// invokes a scanner callee. Catches the wrapper pattern: a public function
// like handleChallenge calls a private writer, and the wrapper scans.
func anyCallerScans(target string, scannerCallees map[string]bool, dirs []string) bool {
	for _, dir := range dirs {
		path := dir
		if dir == "internal" {
			path = "."
		} else if dir == "cmd/mpm" {
			path = "../cmd/mpm"
		}
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, path, nil, 0)
		if err != nil {
			continue
		}
		for _, pkg := range pkgs {
			for _, f := range pkg.Files {
				if strings.HasSuffix(fset.Position(f.Pos()).Filename, "_test.go") {
					continue
				}
				var found bool
				ast.Inspect(f, func(n ast.Node) bool {
					fn, ok := n.(*ast.FuncDecl)
					if !ok || fn.Body == nil {
						return true
					}
					callsTarget := false
					callsScanner := false
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						call, ok := n.(*ast.CallExpr)
						if !ok {
							return true
						}
						name, ok := calledName(call)
						if !ok {
							return true
						}
						if name == target {
							callsTarget = true
						}
						if scannerCallees[name] {
							callsScanner = true
						}
						return true
					})
					if callsTarget && callsScanner {
						found = true
						return false
					}
					return true
				})
				if found {
					return true
				}
			}
		}
	}
	return false
}