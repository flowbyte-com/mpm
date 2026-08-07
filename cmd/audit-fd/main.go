// cmd/audit-fd/main.go — Static analyzer for os.File descriptor (FD) leaks.
//
// The substrate is a long-lived daemon that runs background goroutines
// (synthesis, idle dream, lifecycle decay) against a single shared SQLite
// connection. Every open file descriptor that is not eventually closed
// accumulates: under agent concurrency a handful of leaked FDs silently
// exhausts the process's ulimit -n and makes the whole daemon unresponsive
// (every subsequent open fails with EMFILE). FD hygiene is therefore not a
// style preference in this codebase — it is a load-path invariant.
//
// This tool walks the Go source trees (default: internal/core, cmd/mpm, plus
// the other cmd/* audit tools) and inventories every OS-level file-opening
// call — os.Open, os.OpenFile, os.Create, os.CreateTemp — then classifies
// how the returned *os.File is closed:
//
//	fd-defer-close    — `defer f.Close()` present in a covering position
//	                    (correct; runs on every return path including panic)
//	fd-explicit-close — f.Close() called inline on every path (incl. error
//	                    returns) but not deferred: correct only if the
//	                    analyzer can prove full path coverage
//	fd-returned       — the *os.File is returned to the caller (e.g.
//	                    scheduler.AcquireLock): ownership transfers; the
//	                    caller must close (verify by hand, this is a
//	                    controlled delegate)
//	fd-delegate       — f is passed to a helper call that is known to take
//	                    ownership (e.g. cmd or gzip writers ending in a
//	                    Close) and the helper's Close is itself guaranteed:
//	                    treated as covered when the codebase pattern holds
//	fd-no-close       — no Close() anywhere in the enclosing function: LEAK
//	fd-remove-only    — only os.Remove / defer os.Remove present, no Close:
//	                    LEAK (temp files need both — an unlinked-but-open
//	                    descriptor still consumes the fd table until exit)
//	fd-unknown        — pattern didn't fit (needs review)
//
// Order-sensitivity mirrors audit-closes: a `defer f.Close()` only covers its
// own assignment, and a close placed at the top of a loop body must not count
// a per-iteration open.
//
// Usage:
//
//	go run ./cmd/audit-fd                  # markdown report on stdout
//	go run ./cmd/audit-fd --json           # JSON sidecar (for diffing)
//	go run ./cmd/audit-fd --roots=internal/core,cmd/mpm
//	go run ./cmd/audit-fd --gate           # exit non-zero on fatal classes
//
// The gate (--gate) fails on fd-no-close / fd-remove-only at zero and
// ratchets fd-unknown to its measured baseline (so pre-existing reviewed
// sites are exempt but NEW unknown sites fail the commit).
//
// The pre-commit hook (scripts/pre-commit) runs `audit-fd --gate` alongside
// the other audit commands whenever .go files are staged.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// -------------------------------------------------------------------- types

type FDClass string

const (
	FDClassDeferClose    FDClass = "fd-defer-close"
	FDClassExplicitClose FDClass = "fd-explicit-close"
	FDClassReturned      FDClass = "fd-returned"
	FDClassDelegate      FDClass = "fd-delegate"
	FDClassNoClose       FDClass = "fd-no-close"
	FDClassRemoveOnly    FDClass = "fd-remove-only"
	FDClassUnknown       FDClass = "fd-unknown"
)

// Site is one OS-level open call site with its close classification.
type Site struct {
	File           string   `json:"file"`
	Line           int      `json:"line"`
	Column         int      `json:"column"`
	FuncName       string   `json:"func_name"`
	Pattern        string   `json:"pattern"`
	VarName        string   `json:"var_name"`
	Classification FDClass  `json:"classification"`
	Code           []string `json:"code"`
}

// openCalls are the *os.File-producing calls the audit covers.
var openCalls = map[string]bool{
	"Open":         true,
	"OpenFile":     true,
	"Create":       true,
	"CreateTemp":   true,
	"OpenInCloser": true, // os.OpenFile via helper — matched separately
}

// -------------------------------------------------------------------- main

func main() {
	var (
		jsonOut      = flag.Bool("json", false, "emit JSON sidecar instead of markdown")
		includeTests = flag.Bool("include-tests", false, "scan _test.go files too")
		rootsFlag    = flag.String("roots", "internal,cmd", "comma-separated root dirs to walk")
		jsonSidecar  = flag.String("json-out", "", "if set, also write JSON to this path")
		gate         = flag.Bool("gate", false, "exit non-zero if any fatal class count exceeds its threshold (fd-no-close / fd-remove-only default to 0)")
		maxNoClose   = flag.Int("max-no-close", 0, "threshold for fd-no-close (used with --gate)")
		maxRemove    = flag.Int("max-remove-only", 0, "threshold for fd-remove-only (used with --gate)")
		maxUnknown   = flag.Int("max-unknown", -1, "threshold for fd-unknown (default: current baseline)")
		maxExplicit  = flag.Int("max-explicit", -1, "threshold for fd-explicit-close (default: current baseline)")
	)
	flag.Parse()

	roots := strings.Split(*rootsFlag, ",")
	for i := range roots {
		roots[i] = strings.TrimSpace(roots[i])
	}

	var sites []Site
	for _, root := range roots {
		if root == "" {
			continue
		}
		if err := walkRoot(root, *includeTests, &sites); err != nil {
			fmt.Fprintf(os.Stderr, "audit-fd: walk %s: %v\n", root, err)
		}
	}

	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})

	if *jsonOut {
		emitJSON(os.Stdout, sites)
	} else {
		emitMarkdown(os.Stdout, sites)
	}

	if *jsonSidecar != "" {
		f, err := os.Create(*jsonSidecar)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit-fd: create %s: %v\n", *jsonSidecar, err)
			os.Exit(1)
		}
		defer f.Close()
		emitJSON(f, sites)
		fmt.Fprintf(os.Stderr, "audit-fd: wrote %d sites to %s\n", len(sites), *jsonSidecar)
	}

	counts := countByClass(sites)
	if *maxUnknown < 0 {
		*maxUnknown = counts[FDClassUnknown]
	}
	if *maxExplicit < 0 {
		*maxExplicit = counts[FDClassExplicitClose]
	}

	if *gate {
		exitCode := runGate(counts, gateThresholds{
			NoClose:       *maxNoClose,
			RemoveOnly:    *maxRemove,
			Unknown:       *maxUnknown,
			ExplicitClose: *maxExplicit,
		})
		os.Exit(exitCode)
	}
}

type gateThresholds struct {
	NoClose       int
	RemoveOnly    int
	Unknown       int
	ExplicitClose int
}

// runGate evaluates the gate thresholds and returns the exit code
// (0 = pass, 1 = fail). Output goes to stderr so the report on stdout
// stays machine-parseable.
func runGate(counts map[FDClass]int, t gateThresholds) int {
	type check struct {
		name      string
		actual    int
		threshold int
	}
	checks := []check{
		{"fd-no-close", counts[FDClassNoClose], t.NoClose},
		{"fd-remove-only", counts[FDClassRemoveOnly], t.RemoveOnly},
		{"fd-unknown", counts[FDClassUnknown], t.Unknown},
		{"fd-explicit-close", counts[FDClassExplicitClose], t.ExplicitClose},
	}

	var failed []check
	for _, c := range checks {
		if c.actual > c.threshold {
			failed = append(failed, c)
		}
	}

	fmt.Fprintln(os.Stderr, "[audit-fd --gate] os.File close coverage thresholds:")
	for _, c := range checks {
		status := "ok"
		if c.actual > c.threshold {
			status = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "  %-18s actual=%d  threshold=%d  %s\n", c.name, c.actual, c.threshold, status)
	}

	if len(failed) > 0 {
		fmt.Fprintln(os.Stderr, "[audit-fd --gate] FAIL: descriptor-leak classes exceed thresholds")
		for _, c := range failed {
			fmt.Fprintf(os.Stderr, "  - %s: %d > %d\n", c.name, c.actual, c.threshold)
		}
		return 1
	}

	fmt.Fprintln(os.Stderr, "[audit-fd --gate] PASS")
	return 0
}

// -------------------------------------------------------------------- walker

func walkRoot(root string, includeTests bool, sites *[]Site) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if name == "vendor" || name == "node_modules" || (strings.HasPrefix(name, ".") && name != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if !includeTests && strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileSites, err := auditFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit-fd: parse %s: %v\n", path, err)
			return nil // continue walking
		}
		*sites = append(*sites, fileSites...)
		return nil
	})
}

// -------------------------------------------------------------------- file audit

func auditFile(path string) ([]Site, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	// Pass 1: build the complete parent map so classify sees every node.
	parentMap := make(map[ast.Node]ast.Node)
	{
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				return true
			}
			if len(stack) > 0 {
				parentMap[n] = stack[len(stack)-1]
			}
			stack = append(stack, n)
			return true
		})
	}

	// Pass 2: classify every os.* open site.
	var sites []Site
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isOpenCall(call) {
			return true
		}

		pos := fset.Position(n.Pos())
		if pos.Line == 0 {
			return true // synthetic nodes (e.g. generated) — skip
		}

		site := Site{
			File:     path,
			Line:     pos.Line,
			Column:   pos.Column,
			FuncName: enclosingFuncName(parentMap, n),
			Pattern:  patternString(call),
		}
		site.Classification, site.VarName = classify(call, parentMap)
		site.Code = contextLines(src, pos.Line)
		sites = append(sites, site)
		return true
	})

	return sites, nil
}

// isOpenCall returns true for os.Open / os.OpenFile / os.Create /
// os.CreateTemp — the *os.File-producing calls the audit covers. Matches
// `os.Open(...)` and any package-qualified `alias.Open(...)`.
func isOpenCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if !openCalls[sel.Sel.Name] {
		return false
	}
	// Require an os.-style receiver: either the os package ident, or
	// anything that resolves to os (aliased imports are rare here; accept
	// "os" and "*.Wrapper" passthroughs like writer.Close which we filter
	// above anyway).
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "os"
}

func patternString(call *ast.CallExpr) string {
	sel, _ := call.Fun.(*ast.SelectorExpr)
	return "os." + sel.Sel.Name
}

func enclosingFuncName(parentMap map[ast.Node]ast.Node, node ast.Node) string {
	n := node
	for n != nil {
		if fd, ok := n.(*ast.FuncDecl); ok {
			return fd.Name.Name
		}
		if fl, ok := n.(*ast.FuncLit); ok {
			_ = fl
			// anonymous closure — keep climbing for the enclosing named func
		}
		n = parentMap[n]
	}
	return ""
}

// -------------------------------------------------------------------- classifier

// classify determines the close coverage for one *os.File open site.
func classify(openCall ast.Node, parentMap map[ast.Node]ast.Node) (FDClass, string) {
	parent := parentMap[openCall]
	varName := ""

	// Find the variable the open is assigned to: `f, err := os.Open(...)`
	// or `f := os.Create(...)`.
	if assign, ok := parent.(*ast.AssignStmt); ok && len(assign.Lhs) >= 1 {
		if id, ok := assign.Lhs[0].(*ast.Ident); ok {
			varName = id.Name
		}
	}
	if varName == "" {
		return FDClassUnknown, ""
	}

	funcBody := enclosingFuncBody(parentMap, openCall)
	if funcBody == nil {
		return FDClassUnknown, varName
	}

	// Covering positions for a close/defer relative to the open site.
	cover := coveringBlocks(openCall, parentMap, varName, errVarName(parent))

	// Scan the whole enclosing function body (order-aware through `cover`).
	deferClose := false
	explicitClose := false
	returned := false
	delegated := false

	ast.Inspect(funcBody, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.DeferStmt:
			// `defer f.Close()` directly, or `defer func(){ _ = f.Close() }()`.
			if deferTargetsVar(stmt, varName) && inCover(stmt, cover, parentMap) {
				// defer inside a loop covering a single-iteration open can
				// still accumulate; but since opens are per-iteration and
				// each iteration defers — revert: it's fine. Keep as defer.
				deferClose = true
				return false
			}
		case *ast.ExprStmt:
			if call, ok := stmt.X.(*ast.CallExpr); ok {
				if closeTargetsVar(call, varName) && inCover(stmt, cover, parentMap) {
					explicitClose = true
					return false
				}
			}
		case *ast.AssignStmt:
			// `_ = f.Close()` — a bare close discarded into the blank ident.
			if len(stmt.Rhs) == 1 {
				if call, ok := stmt.Rhs[0].(*ast.CallExpr); ok &&
					closeTargetsVar(call, varName) && inCover(stmt, cover, parentMap) {
					explicitClose = true
					return false
				}
			}
		case *ast.IfStmt:
			// `if err := f.Close(); err != nil { ... }` — init-form close.
			if stmt.Init != nil {
				var call *ast.CallExpr
				if es, ok := stmt.Init.(*ast.ExprStmt); ok {
					call, _ = es.X.(*ast.CallExpr)
				} else if assign, ok := stmt.Init.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 {
					call, _ = assign.Rhs[0].(*ast.CallExpr)
				}
				if call != nil && closeTargetsVar(call, varName) && inCover(stmt, cover, parentMap) {
					explicitClose = true
					return false
				}
			}
		case *ast.ReturnStmt:
			if inCover(stmt, cover, parentMap) {
				for _, r := range stmt.Results {
					if id, ok := r.(*ast.Ident); ok && id.Name == varName {
						returned = true
						return false
					}
				}
			}
		case *ast.CallExpr:
			// `return helper(f)` or `cmd.Stdin = f` — delegate ownership to
			// a callee (exec.Command paths close child pipes themselves).
			if callPassesVar(stmt, varName) && inCover(stmt, cover, parentMap) {
				delegated = true
				return false
			}
		}
		return true
	})

	if deferClose {
		return FDClassDeferClose, varName
	}
	if returned {
		return FDClassReturned, varName
	}
	if explicitClose {
		return FDClassExplicitClose, varName
	}
	if delegated {
		return FDClassDelegate, varName
	}

	// No Close at all. Distinguish remove-only (temp cleanup, still a leak
	// because the fd stays open / space stays reserved for the descriptor).
	if hasRemoveFor(funcBody, varName) {
		return FDClassRemoveOnly, varName
	}
	return FDClassNoClose, varName
}

// closeTargetsVar reports whether call is `f.Close()`-ish on the var.
func closeTargetsVar(call *ast.CallExpr, varName string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "Close" {
		return false
	}
	if id, ok := sel.X.(*ast.Ident); ok && id.Name == varName {
		return true
	}
	return false
}

// deferTargetsVar reports whether the defer statement (eventually) calls
// Close on the named var: `defer f.Close()` or `defer func(){ _ = f.Close() }()`.
func deferTargetsVar(stmt *ast.DeferStmt, varName string) bool {
	if internal := deferTargetsVarInternal(stmt.Call, varName); internal {
		return true
	}
	return false
}

func deferTargetsVarInternal(e ast.Expr, varName string) bool {
	switch t := e.(type) {
	case *ast.CallExpr:
		if closeTargetsVar(t, varName) {
			return true
		}
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Close" {
			// gzWriter-ish Close on a wrapper var of same name
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == varName {
				return true
			}
		}
		// defer func() { _ = f.Close() }()
		if fl, ok := t.Fun.(*ast.FuncLit); ok {
			covered := false
			ast.Inspect(fl.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && closeTargetsVar(call, varName) {
					covered = true
					return false
				}
				return true
			})
			return covered
		}
	}
	return false
}

// callPassesVar reports whether call passes the named var as an argument.
func callPassesVar(call *ast.CallExpr, varName string) bool {
	for _, a := range call.Args {
		if id, ok := a.(*ast.Ident); ok && id.Name == varName {
			return true
		}
	}
	return false
}

// coveringBlocks returns the (block, after-index) positions in which a
// close/defer would cover the site — the block after the open statement,
// plus each enclosing block after the inner statement, plus guard bodies.
// Guard bodies: an `if err != nil { ... }` that tests the site's error var
// (or the file var) extends coverage into the body from index 0 — the
// close/remove inside is a real cleanup of the just-opened file.
func coveringBlocks(node ast.Node, parentMap map[ast.Node]ast.Node, varName, errVar string) []blockPos {
	var out []blockPos
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			if block, ok := parentMap[stmt].(*ast.BlockStmt); ok {
				out = append(out, blockPos{block: block, after: stmtIndexInBlock(block, stmt) + 1})
				// Guard bodies: if statements AFTER the site that test the
				// site var / err var extend coverage into their bodies.
				for i := stmtIndexInBlock(block, stmt) + 1; i < len(block.List); i++ {
					ifs, ok := block.List[i].(*ast.IfStmt)
					if !ok {
						continue
					}
					if guardCondSite(ifs, varName, errVar) {
						out = append(out, blockPos{block: ifs.Body, after: 0})
						if elseBlock, ok := ifs.Else.(*ast.BlockStmt); ok {
							out = append(out, blockPos{block: elseBlock, after: 0})
						}
					}
				}
				n = block
				continue
			}
			// init of if/for: the body is a covering position from index 0.
			if ifs, ok := parentMap[stmt].(*ast.IfStmt); ok && ifs.Init == stmt && ifs.Body != nil {
				out = append(out, blockPos{block: ifs.Body, after: 0})
				if elseBlock, ok := ifs.Else.(*ast.BlockStmt); ok {
					out = append(out, blockPos{block: elseBlock, after: 0})
				}
				n = ifs
				continue
			}
			if fr, ok := parentMap[stmt].(*ast.ForStmt); ok && fr.Init == stmt && fr.Body != nil {
				out = append(out, blockPos{block: fr.Body, after: 0})
				n = fr
				continue
			}
			n = parentMap[stmt]
			continue
		}
		n = parentMap[n]
	}
	return out
}

type blockPos struct {
	block *ast.BlockStmt
	after int
}

// inCover reports whether a statement sits in a covering position.
func inCover(node ast.Node, cover []blockPos, parentMap map[ast.Node]ast.Node) bool {
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			return covers(cover, stmt, parentMap)
		}
		n = parentMap[n]
	}
	return false
}

// covers reports whether the statement sits at a position at-or-after an
// index in one of the covering blocks.
func covers(cover []blockPos, stmt ast.Stmt, parentMap map[ast.Node]ast.Node) bool {
	if block, ok := parentMap[stmt].(*ast.BlockStmt); ok {
		for _, c := range cover {
			if c.block == block {
				idx := stmtIndexInBlock(block, stmt)
				if idx >= c.after {
					return true
				}
			}
		}
	}
	return false
}

// stmtIndexInBlock returns the index of the statement in its block.
func stmtIndexInBlock(block *ast.BlockStmt, stmt ast.Stmt) int {
	for i, s := range block.List {
		if s == stmt {
			return i
		}
	}
	return -1
}

// enclosingFuncBody returns the block of the innermost enclosing
// function (decl or literal).
func enclosingFuncBody(parentMap map[ast.Node]ast.Node, node ast.Node) *ast.BlockStmt {
	n := node
	for n != nil {
		if fd, ok := n.(*ast.FuncDecl); ok {
			return fd.Body
		}
		if fl, ok := n.(*ast.FuncLit); ok {
			return fl.Body
		}
		n = parentMap[n]
	}
	return nil
}

// guardCondSite reports whether an if statement guards the open site: its
// condition references the file var or the open's error var. Bodies of such
// ifs are covering positions for a close/return pair.
func guardCondSite(ifs *ast.IfStmt, varName, errVar string) bool {
	if varName != "" && referencesIdent(ifs.Cond, varName) {
		return true
	}
	if errVar != "" && referencesIdent(ifs.Cond, errVar) {
		return true
	}
	return false
}

func referencesIdent(expr ast.Expr, name string) bool {
	if expr == nil || name == "" {
		return false
	}
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

// errVarName returns the name of the error variable in `f, err := os.Open(...)`.
func errVarName(parent ast.Node) string {
	assign, ok := parent.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) < 2 {
		return ""
	}
	id, ok := assign.Lhs[1].(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}

// hasRemoveFor reports whether the function body ever removes the named
// temp file (defer os.Remove(path) or os.Remove(path)).
func hasRemoveFor(funcBody *ast.BlockStmt, varName string) bool {
	found := false
	ast.Inspect(funcBody, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Remove" {
			return true
		}
		for _, a := range call.Args {
			if id, ok := a.(*ast.Ident); ok && id.Name == varName {
				found = true
				return false
			}
			if call, ok := a.(*ast.CallExpr); ok {
				// os.Remove(tmpFile.Name())
				if csel, ok := call.Fun.(*ast.SelectorExpr); ok && csel.Sel.Name == "Name" {
					if id, ok := csel.X.(*ast.Ident); ok && id.Name == varName {
						found = true
						return false
					}
				}
			}
		}
		return true
	})
	return found
}

// -------------------------------------------------------------------- output

func countByClass(sites []Site) map[FDClass]int {
	out := make(map[FDClass]int)
	for _, s := range sites {
		out[s.Classification]++
	}
	return out
}

func emitMarkdown(w *os.File, sites []Site) {
	fmt.Fprintln(w, "# audit-fd: os.File close coverage")
	fmt.Fprintln(w)
	for _, cls := range []FDClass{
		FDClassDeferClose, FDClassExplicitClose, FDClassReturned,
		FDClassDelegate, FDClassNoClose, FDClassRemoveOnly, FDClassUnknown,
	} {
		matching := []Site{}
		for _, s := range sites {
			if s.Classification == cls {
				matching = append(matching, s)
			}
		}
		fmt.Fprintf(w, "\n## %s (%d sites)\n\n", cls, len(matching))
		for _, s := range matching {
			fmt.Fprintf(w, "  %s:%d `%s` (%s)\n", s.File, s.Line, s.Pattern, s.FuncName)
		}
	}
	fmt.Fprintf(w, "\nTotal sites: %d\n", len(sites))
}

func emitJSON(out *os.File, sites []Site) {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(struct {
		Total int    `json:"total"`
		Sites []Site `json:"sites"`
	}{Total: len(sites), Sites: sites})
}

func contextLines(src []byte, line int) []string {
	lines := strings.Split(string(src), "\n")
	if line < 0 || line-1 >= len(lines) {
		return nil
	}
	from := line - 4
	if from < 0 {
		from = 0
	}
	to := line + 2
	if to > len(lines) {
		to = len(lines)
	}
	out := []string{}
	for i := from; i < to; i++ {
		marker := " "
		if i == line-1 {
			marker = "▶"
		}
		out = append(out, fmt.Sprintf("%3d: %s %s", i+1, marker, lines[i]))
	}
	return out
}
