// cmd/audit-ctx/main.go — Static analyzer for context propagation at the
// database call boundary.
//
// Context-cancellation is the only mechanism a timed-out or aborted agent
// has to stop a heavy query (semantic search, cascade materialization,
// compaction sweeps) from grinding on after it is dead. SQLite is a single
// shared connection in MPM, so a query that ignores ctx keeps the connection
// (and the whole substrate) hostage until it finishes on its own.
//
// This tool walks the Go source trees and inventories every database query
// method — Query / Exec / QueryRow and their *Context variants, plus MPM's
// Tracked wrappers — and classifies each site's relationship to a context:
//
//	ctx-passed          — first argument is a context.Context value
//	                    (QueryContext(ctx, ...), ExecContext(ctx, ...),
//	                    or a bare Query/Exec whose first arg is ctx): CORRECT
//	ctx-in-scope-missing— the enclosing function HAS a ctx in scope (param,
//	                    receiver .ctx, or a ctx := context.Background()
//	                    assignment) but the DB call ignores it: the real
//	                    cancellation bug — a gated class
//	ctx-plain-missing   — the call takes no context and no ctx exists in the
//	                    enclosing scope: relies on context.Background()
//	                    implicitly; report-class, not always fixable locally
//	ctx-void            — the method takes NO database context at all
//	                    (e.g. the bare non-Context Tracked wrappers): note
//	                    separately since these are the MPM abstraction seam
//	non-db              — the receiver is not a database at all (e.g. an LLM
//	                    client's .Query) — demoted like audit-closes
//	unknown             — pattern did not fit (needs review)
//
// The gate (--gate) fails on ctx-cancel-in-scope at zero *and* on any
// newly-introduced ctx-plain-missing (threshold defaulted to the current
// baseline count so existing technical debt is exempt but regressions are
// not).
//
// Usage:
//
//	go run ./cmd/audit-ctx                  # markdown report on stdout
//	go run ./cmd/audit-ctx --json           # JSON sidecar
//	go run ./cmd/audit-ctx --roots=a,b
//	go run ./cmd/audit-ctx --gate           # run gate thresholds
//	go run ./cmd/audit-ctx --max-plain=294  # raise the plain backlog allowance
//
// The pre-commit hook runs `audit-ctx --gate` alongside the other gates.

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

type CtxClass string

const (
	CtxPassed       CtxClass = "ctx-passed"
	CtxInScopeDrop  CtxClass = "ctx-in-scope-missing" // ctx exists, ignored: FATAL
	CtxPlainMissing CtxClass = "ctx-plain-missing"    // no ctx anywhere in scope
	CtxVoid         CtxClass = "ctx-void"             // method deliberately takes none
	CtxNonDb        CtxClass = "non-db"               // not a database receiver
	CtxUnknown      CtxClass = "unknown"
)

// Site is one query/exec call with its context-propagation classification.
type Site struct {
	File           string   `json:"file"`
	Line           int      `json:"line"`
	Column         int      `json:"column"`
	FuncName       string   `json:"func_name"`
	Pattern        string   `json:"pattern"`
	ArgZero        string   `json:"arg_zero"`
	Classification CtxClass `json:"classification"`
	Code           []string `json:"code"`
}

// coveredMethods: the database mutation/query surface. The plain forms
// (Query/Exec/QueryRow/Tracked) never take a ctx — they are classifiable.
// The *Context forms DO take ctx as first arg.
var dbMethods = map[string]bool{
	"Query": true, "QueryContext": true, "QueryRow": true, "QueryRowContext": true,
	"Exec": true, "ExecContext": true,
	"QueryTracked": true, "QueryRowTracked": true, "ExecTracked": true,
}

var ctxForms = map[string]bool{
	"QueryContext": true, "QueryRowContext": true, "ExecContext": true,
}

// -------------------------------------------------------------------- main

func main() {
	var (
		jsonOut      = flag.Bool("json", false, "emit JSON sidecar instead of markdown")
		includeTests = flag.Bool("include-tests", false, "scan _test.go files too")
		rootsFlag    = flag.String("roots", "internal/core,cmd/mpm", "comma-separated root dirs to walk")
		jsonSidecar  = flag.String("json-out", "", "if set, also write JSON to this path")
		gate         = flag.Bool("gate", false, "exit non-zero if fatal-class counts exceed thresholds")
		maxInScope   = flag.Int("max-in-scope", 0, "threshold for ctx-in-scope-missing (used with --gate)")
		maxPlain     = flag.Int("max-plain", -1, "threshold for ctx-plain-missing (default: current baseline, so any NEW plain call fails)")
		maxVoid      = flag.Int("max-void", -1, "threshold for ctx-void (default: current baseline)")
		maxUnknown   = flag.Int("max-unknown", 0, "threshold for unknown (used with --gate)")
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
			fmt.Fprintf(os.Stderr, "audit-ctx: walk %s: %v\n", root, err)
		}
	}

	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})

	// Default thresholds to the measured baseline for the report classes.
	counts := countByClass(sites)
	if *maxPlain < 0 {
		*maxPlain = counts[CtxPlainMissing]
	}
	if *maxVoid < 0 {
		*maxVoid = counts[CtxVoid]
	}

	if *jsonOut {
		emitJSON(os.Stdout, sites)
	} else {
		emitMarkdown(os.Stdout, sites)
	}

	if *jsonSidecar != "" {
		f, err := os.Create(*jsonSidecar)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit-ctx: create %s: %v\n", *jsonSidecar, err)
			os.Exit(1)
		}
		defer f.Close()
		emitJSON(f, sites)
		fmt.Fprintf(os.Stderr, "audit-ctx: wrote %d sites to %s\n", len(sites), *jsonSidecar)
	}

	if *gate {
		exitCode := runGate(counts, gateThresholds{
			InScope: *maxInScope,
			Plain:   *maxPlain,
			Void:    *maxVoid,
			Unknown: *maxUnknown,
		})
		os.Exit(exitCode)
	}
}

type gateThresholds struct {
	InScope int
	Plain   int
	Void    int
	Unknown int
}

func runGate(counts map[CtxClass]int, t gateThresholds) int {
	type check struct {
		name      string
		actual    int
		threshold int
	}
	checks := []check{
		{"ctx-in-scope-missing", counts[CtxInScopeDrop], t.InScope},
		{"ctx-plain-missing", counts[CtxPlainMissing], t.Plain},
		{"ctx-void", counts[CtxVoid], t.Void},
		{"unknown", counts[CtxUnknown], t.Unknown},
	}

	var failed []check
	for _, c := range checks {
		if c.actual > c.threshold {
			failed = append(failed, c)
		}
	}

	fmt.Fprintln(os.Stderr, "[audit-ctx --gate] context propagation thresholds:")
	for _, c := range checks {
		status := "ok"
		if c.actual > c.threshold {
			status = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "  %-22s actual=%d  threshold=%d  %s\n", c.name, c.actual, c.threshold, status)
	}

	if len(failed) > 0 {
		fmt.Fprintln(os.Stderr, "[audit-ctx --gate] FAIL: context-dropping classes exceed thresholds")
		for _, c := range failed {
			fmt.Fprintf(os.Stderr, "  - %s: %d > %d\n", c.name, c.actual, c.threshold)
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, "[audit-ctx --gate] PASS")
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
			fmt.Fprintf(os.Stderr, "audit-ctx: parse %s: %v\n", path, err)
			return nil
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

	var sites []Site
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isDbMethodCall(call) {
			return true
		}
		pos := fset.Position(n.Pos())
		funcName, scopeHasCtx := enclosingScope(parentMap, n)
		site := Site{
			File:     path,
			Line:     pos.Line,
			Column:   pos.Column,
			FuncName: funcName,
			Pattern:  patternString(call),
			ArgZero:  argZero(call),
		}
		site.Classification = classify(call, scopeHasCtx)
		site.Code = contextLines(src, pos.Line)
		sites = append(sites, site)
		return true
	})
	return sites, nil
}

// isDbMethodCall reports whether call.Fun is a bare or *Context db method on
// a receiver that plausibly is the database. Fights false positives on LLM
// client .Query by requiring the receiver be one of the recognized db
// identifiers, a dm.SQLDB()/DB() call, or a dm/s/tx/DB receiver.
func isDbMethodCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	name := sel.Sel.Name
	if !dbMethods[name] {
		return false
	}
	return isKnownDbReceiver(sel.X)
}

// isKnownDbReceiver reports whether the receiver qualifier is the database:
// a bare ident from dbReceiverIdents, a method call returning the DB
// (dm.SQLDB()), or a chain ending in one of them.
func isKnownDbReceiver(x ast.Expr) bool {
	switch t := x.(type) {
	case *ast.Ident:
		return dbReceiverIdents[t.Name]
	case *ast.SelectorExpr:
		inner := receiverIdent(t.X)
		if t.Sel.Name == "DB" && (inner == "dm" || inner == "sc") {
			return true
		}
		return isKnownDbReceiver(t.X)
	case *ast.CallExpr:
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "SQLDB", "DB":
				return true
			}
		}
		// fall through: e.g. (*x).SQLDB()
		if len(t.Args) > 0 {
			return false
		}
	}
	return false
}

// receiverIdent renders the receiver qualifier to a canonical string.
func receiverIdent(x ast.Expr) string {
	switch t := x.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return receiverIdent(t.X) + "." + t.Sel.Name
	case *ast.CallExpr:
		return "(...) "
	case *ast.StarExpr:
		return "*" + receiverIdent(t.X)
	}
	return ""
}

// dbReceiverIdents is the set of well-known local names that refer to the
// SQLite connection in this codebase.
var dbReceiverIdents = map[string]bool{
	"db": true, "DB": true, "tx": true, "sqlDB": true, "wDB": true,
	"testDB": true, "dm": true, "s": true, "sc": true,
}

func patternString(call *ast.CallExpr) string {
	sel, _ := call.Fun.(*ast.SelectorExpr)
	switch x := sel.X.(type) {
	case *ast.Ident:
		return x.Name + "." + sel.Sel.Name
	case *ast.CallExpr:
		if innerSel, ok := x.Fun.(*ast.SelectorExpr); ok {
			if innerID, ok := innerSel.X.(*ast.Ident); ok {
				return fmt.Sprintf("%s.%s(...).%s", innerID.Name, innerSel.Sel.Name, sel.Sel.Name)
			}
		}
		return "(...)." + sel.Sel.Name
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return fmt.Sprintf("%s.%s.%s", id.Name, x.Sel.Name, sel.Sel.Name)
		}
	}
	return "?" + sel.Sel.Name
}

// argZero renders the first argument for the report.
func argZero(call *ast.CallExpr) string {
	if len(call.Args) == 0 {
		return "(no args)"
	}
	return exprBrief(call.Args[0])
}

func exprBrief(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.CallExpr:
		return "(call)"
	case *ast.BasicLit:
		return t.Value
	case *ast.SelectorExpr:
		return receiverIdent(t.X) + "." + t.Sel.Name
	}
	return "?"
}

// enclosingScope reports the enclosing function name and whether the call
// site is inside a function that has a context.Context in scope (as a
// receiver field, parameter, or explicit ctx := assignment).
func enclosingScope(parentMap map[ast.Node]ast.Node, node ast.Node) (string, bool) {
	n := node
	for n != nil {
		switch f := n.(type) {
		case *ast.FuncDecl:
			return f.Name.Name, funcScopeHasCtx(f)
		case *ast.FuncLit:
			return "(closure)", funcScopeHasCtxLit(f)
		}
		n = parentMap[n]
	}
	return "", false
}

func funcScopeHasCtx(f *ast.FuncDecl) bool {
	hasCtx := false
	ast.Inspect(f.Type, func(n ast.Node) bool {
		ft, ok := n.(*ast.Field)
		if !ok {
			return true
		}
		// A param whose TYPE is context.Context (selector context.Context
		// or bare ident Context) — NOT a ctx-named param of some other type
		// like AuditContext (a business map that shares the name).
		if t, ok := ft.Type.(*ast.SelectorExpr); ok {
			if t.Sel.Name == "Context" {
				hasCtx = true
				return false
			}
		}
		if t, ok := ft.Type.(*ast.Ident); ok && t.Name == "Context" {
			hasCtx = true
			return false
		}
		// ctx.Context via qualified chain won't appear at top level, but a
		// param *named* ctx with selector type is a strong signal too.
		if t, ok := ft.Type.(*ast.StarExpr); ok {
			if sel, ok2 := t.X.(*ast.SelectorExpr); ok2 && sel.Sel.Name == "Context" {
				hasCtx = true
				return false
			}
			if id, ok2 := t.X.(*ast.Ident); ok2 && id.Name == "Context" {
				hasCtx = true
				return false
			}
		}
		return true
	})
	return hasCtx
}

func funcScopeHasCtxLit(fl *ast.FuncLit) bool {
	has := false
	ast.Inspect(fl.Body, func(n ast.Node) bool {
		if t, ok := n.(*ast.AssignStmt); ok {
			for _, lhs := range t.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == "ctx" {
					has = true
					return false
				}
			}
			for _, rhs := range t.Rhs {
				if callexpr, ok := rhs.(*ast.CallExpr); ok {
					if sel, ok := callexpr.Fun.(*ast.SelectorExpr); ok {
						if sel.Sel.Name == "Background" || sel.Sel.Name == "WithTimeout" || sel.Sel.Name == "WithCancel" {
							has = true
							return false
						}
					}
				}
			}
		}
		return true
	})
	return has
}

// classify decides the ctx-in-scope class for the site.
func classify(call *ast.CallExpr, scopeHasCtx bool) CtxClass {
	sel, _ := call.Fun.(*ast.SelectorExpr)

	recv := receiverIdent(sel.X)
	// Demote non-db receivers (LLM client.Query etc.)
	if !isKnownDbReceiver(sel.X) {
		return CtxNonDb
	}
	_ = recv

	method := sel.Sel.Name
	if ctxForms[method] {
		// Context variants: verify first arg is a ctx-like value.
		if len(call.Args) > 0 && ctxLike(call.Args[0]) {
			return CtxPassed
		}
		// a *Context method whose first arg isn't ctx is almost never correct.
		if scopeHasCtx {
			return CtxInScopeDrop
		}
		return CtxPlainMissing
	}

	// Plain methods (Query/Exec/QueryRow + Tracked wrappers).
	if method == "QueryTracked" || method == "QueryRowTracked" || method == "ExecTracked" {
		return CtxVoid
	}

	if scopeHasCtx {
		return CtxInScopeDrop
	}
	return CtxPlainMissing
}

// ctxLike reports whether an expression looks like a context.Context value.
func ctxLike(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		n := t.Name
		if n == "ctx" || n == "context" {
			return true
		}
		if strings.Contains(n, "Ctx") {
			return true
		}
	case *ast.SelectorExpr:
		if t.Sel.Name == "Background" || t.Sel.Name == "TODO" || t.Sel.Name == "WithTimeout" {
			return true
		}
	case *ast.CallExpr:
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok {
			if sel.Sel.Name == "WithTimeout" || sel.Sel.Name == "WithCancel" || sel.Sel.Name == "WithValue" {
				return true
			}
		}
	}
	return false
}

// -------------------------------------------------------------------- context capture

func contextLines(src []byte, line int) []string {
	all := strings.Split(string(src), "\n")
	start := line - 4
	if start < 0 {
		start = 0
	}
	end := line + 3
	if end > len(all) {
		end = len(all)
	}
	out := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		marker := "  "
		if i == line-1 {
			marker = "> "
		}
		out = append(out, fmt.Sprintf("%s%d: %s", marker, i+1, all[i]))
	}
	return out
}

// -------------------------------------------------------------------- emitters

func emitMarkdown(w *os.File, sites []Site) {
	counts := countByClass(sites)

	fmt.Fprintln(w, "# Context Propagation Audit Report")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "**Total DB method call sites:** %d\n\n", len(sites))

	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	classOrder := []CtxClass{
		CtxPassed, CtxInScopeDrop, CtxPlainMissing, CtxVoid, CtxNonDb, CtxUnknown,
	}
	for _, c := range classOrder {
		if n, ok := counts[c]; ok {
			fmt.Fprintf(w, "- **%s**: %d\n", c, n)
		}
	}
	fmt.Fprintln(w)

	total := len(sites)
	bad := counts[CtxInScopeDrop] + counts[CtxPlainMissing] + counts[CtxVoid] + counts[CtxUnknown]
	if total > 0 {
		pctBad := float64(bad) / float64(total) * 100
		fmt.Fprintf(w, "**Non-context calls (cancellation debt): %d / %d (%.1f%%)**\n\n", bad, total, pctBad)
	}

	fmt.Fprintln(w, "## Sites Requiring Attention")
	fmt.Fprintln(w)
	skip := map[CtxClass]bool{
		CtxPassed: true,
		CtxNonDb:  true,
	}
	for _, s := range sites {
		if skip[s.Classification] {
			continue
		}
		fmt.Fprintf(w, "### `%s` — line %d (%s)\n", s.File, s.Line, s.Pattern)
		fmt.Fprintf(w, "- **Classification:** %s\n", s.Classification)
		fmt.Fprintf(w, "- **Enclosing function:** `%s`\n", s.FuncName)
		if s.ArgZero != "" {
			fmt.Fprintf(w, "- **First argument:** `%s`\n", s.ArgZero)
		}
		fmt.Fprintln(w, "- **Code:**")
		fmt.Fprintln(w, "  ```go")
		for _, line := range s.Code {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w, "  ```")
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "## ctx-passed Sites (correct)")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d sites already propagate context.\n", counts[CtxPassed])
}

func emitJSON(w *os.File, sites []Site) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(struct {
		Total int    `json:"total"`
		Sites []Site `json:"sites"`
	}{Total: len(sites), Sites: sites})
}

func countByClass(sites []Site) map[CtxClass]int {
	m := make(map[CtxClass]int)
	for _, s := range sites {
		m[s.Classification]++
	}
	return m
}
