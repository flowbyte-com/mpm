// cmd/audit-closes/main.go — Static analyzer for rows.Close() coverage.
//
// The companion to cmd/audit-scans (which audits Scan-error handling). This
// tool walks Go source trees (default: internal/, cmd/) and inventories
// every database/sql *Rows-producing call — .Query(), .QueryContext(),
// .QueryTracked() — and verifies each result is covered by a
// `defer rows.Close()` before the function can return.
//
// Why this matters: a *sql.Rows that is never closed never returns its
// database connection to the pool. MPM runs a single shared SQLite
// connection; under agent concurrency, a handful of unclosed rows sites
// silently exhausts the pool and deadlocks the substrate. (QueryRow is
// NOT audited — *sql.Row closes its underlying rows on Scan by contract.)
//
// Each site is classified by how its rows are closed:
//
//	defer-close       — `defer rows.Close()` present in a covering position
//	                   (correct; runs on every return path including panic)
//	loop-defer        — `defer rows.Close()` sits INSIDE a for loop: rows
//	                   accumulate until function return, holding the single
//	                   SQLite connection hostage for the whole loop
//	explicit-close    — `rows.Close()` called inline, not deferred: leaks on
//	                   any early return inside the loop body
//	delegate          — rows handed to a helper call; closing responsibility
//	                   lives elsewhere (helper must close; verify by hand)
//	wrapper-return    — function returns rows to its caller (`return rows, err`);
//	                   the caller owns the close (e.g. QueryTracked itself)
//	no-close          — no Close() anywhere in the enclosing function: LEAK
//	rows-discarded    — Query() result assigned to `_` or called bare: LEAK
//	unknown           — pattern didn't fit (needs review)
//
// Order-sensitivity: the analyzer is position-aware. A `defer rows.Close()`
// only covers a site whose assignment executes BEFORE the defer in the same
// statement order. The double-assignment bug class — `rows, err := q1;
// defer rows.Close(); rows, err = q2` — is caught: q2's rows is never
// closed because the defer captured q1's value at defer time.
//
// Usage:
//
//	go run ./cmd/audit-closes                  # markdown report on stdout
//	go run ./cmd/audit-closes --json           # JSON sidecar (for diffing)
//	go run ./cmd/audit-closes --include-tests  # include _test.go files
//	go run ./cmd/audit-closes --roots=internal/core,cmd/mpm
//	go run ./cmd/audit-closes --gate           # exit non-zero on fatal classes
//
// The pre-commit hook (scripts/pre-commit) runs `audit-closes --gate`
// alongside `audit-scans --gate` whenever .go files are staged.

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

type QClass string

const (
	QClassDeferClose    QClass = "defer-close"
	QClassLoopDefer     QClass = "loop-defer"
	QClassExplicitClose QClass = "explicit-close"
	QClassDelegate      QClass = "delegate"
	QClassWrapperReturn QClass = "wrapper-return"
	QClassNoClose       QClass = "no-close"
	QClassRowsDiscarded QClass = "rows-discarded"
	QClassNonDB         QClass = "non-db"
	QClassUnknown       QClass = "unknown"
)

// Site is one Query-family call site with its close classification.
type Site struct {
	File           string   `json:"file"`
	Line           int      `json:"line"`
	Column         int      `json:"column"`
	FuncName       string   `json:"func_name"`
	Pattern        string   `json:"pattern"`
	VarName        string   `json:"var_name"`
	Classification QClass   `json:"classification"`
	Code           []string `json:"code"`
}

// queryMethods are the *sql.Rows-producing calls the audit covers.
// QueryRow / QueryRowTracked are excluded by design: *sql.Row closes the
// underlying rows automatically on Scan.
var queryMethods = map[string]bool{
	"Query":         true,
	"QueryContext":  true,
	"QueryTracked":  true,
}

// -------------------------------------------------------------------- main

func main() {
	var (
		jsonOut        = flag.Bool("json", false, "emit JSON sidecar instead of markdown")
		includeTests   = flag.Bool("include-tests", false, "scan _test.go files too")
		rootsFlag      = flag.String("roots", "internal/core,cmd/mpm", "comma-separated root dirs to walk")
		jsonSidecar    = flag.String("json-out", "", "if set, also write JSON to this path")
		gate           = flag.Bool("gate", false, "exit non-zero if any fatal-class count exceeds its threshold (no-close / rows-discarded / loop-defer default to 0)")
		maxNoClose     = flag.Int("max-no-close", 0, "threshold for no-close (used with --gate)")
		maxDiscarded   = flag.Int("max-rows-discarded", 0, "threshold for rows-discarded (used with --gate)")
		maxLoopDefer   = flag.Int("max-loop-defer", 0, "threshold for loop-defer (used with --gate)")
		maxUnknown     = flag.Int("max-unknown", 0, "threshold for unknown (used with --gate)")
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
			fmt.Fprintf(os.Stderr, "audit-closes: walk %s: %v\n", root, err)
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
			fmt.Fprintf(os.Stderr, "audit-closes: create %s: %v\n", *jsonSidecar, err)
			os.Exit(1)
		}
		defer f.Close()
		emitJSON(f, sites)
		fmt.Fprintf(os.Stderr, "audit-closes: wrote %d sites to %s\n", len(sites), *jsonSidecar)
	}

	if *gate {
		exitCode := runGate(sites, gateThresholds{
			NoClose:     *maxNoClose,
			Discarded:   *maxDiscarded,
			LoopDefer:   *maxLoopDefer,
			Unknown:     *maxUnknown,
		})
		os.Exit(exitCode)
	}
}

type gateThresholds struct {
	NoClose   int
	Discarded int
	LoopDefer int
	Unknown   int
}

// runGate evaluates the gate thresholds and returns the exit code
// (0 = pass, 1 = fail). Output goes to stderr so the report on stdout
// stays machine-parseable.
func runGate(sites []Site, t gateThresholds) int {
	counts := countByClass(sites)

	type check struct {
		name      string
		actual    int
		threshold int
	}
	checks := []check{
		{"no-close", counts[QClassNoClose], t.NoClose},
		{"rows-discarded", counts[QClassRowsDiscarded], t.Discarded},
		{"loop-defer", counts[QClassLoopDefer], t.LoopDefer},
		{"unknown", counts[QClassUnknown], t.Unknown},
	}

	var failed []check
	for _, c := range checks {
		if c.actual > c.threshold {
			failed = append(failed, c)
		}
	}

	fmt.Fprintln(os.Stderr, "[audit-closes --gate] rows.Close() coverage thresholds:")
	for _, c := range checks {
		status := "ok"
		if c.actual > c.threshold {
			status = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "  %-16s actual=%d  threshold=%d  %s\n", c.name, c.actual, c.threshold, status)
	}

	if len(failed) > 0 {
		fmt.Fprintln(os.Stderr, "[audit-closes --gate] FAIL: connection-leak classes exceed thresholds")
		for _, c := range failed {
			fmt.Fprintf(os.Stderr, "  - %s: %d > %d\n", c.name, c.actual, c.threshold)
		}
		return 1
	}

	fmt.Fprintln(os.Stderr, "[audit-closes --gate] PASS")
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
			fmt.Fprintf(os.Stderr, "audit-closes: parse %s: %v\n", path, err)
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

	// Pass 1: build the complete parent map so classify sees every node
	// (including defers that appear AFTER a given site in source order).
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

	// Pass 2: classify every Query-family call site.
	var sites []Site
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isQueryCall(call) {
			return true
		}

		pos := fset.Position(n.Pos())
		site := Site{
			File:     path,
			Line:     pos.Line,
			Column:   pos.Column,
			FuncName: enclosingFuncName(parentMap, n, f),
			Pattern:  patternString(call),
		}
		site.Classification, site.VarName = classify(call, parentMap)
		site.Code = contextLines(src, pos.Line)
		sites = append(sites, site)
		return true
	})

	return sites, nil
}

// isQueryCall returns true if the call is a *sql.Rows-producing database
// query: .Query / .QueryContext / .QueryTracked. Method-name filtering is
// sufficient — unlike Scan, none of these names collide with common
// non-database APIs in this codebase.
func isQueryCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return queryMethods[sel.Sel.Name]
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

func enclosingFuncName(parentMap map[ast.Node]ast.Node, node ast.Node, f *ast.File) string {
	n := node
	for n != nil {
		if fd, ok := n.(*ast.FuncDecl); ok {
			return fd.Name.Name
		}
		n = parentMap[n]
	}
	return ""
}

// -------------------------------------------------------------------- classifier

// classify determines the close coverage class for one Query site.
// Order-sensitive: a defer only covers the assignment that precedes it.
func classify(queryCall ast.Node, parentMap map[ast.Node]ast.Node) (QClass, string) {
	parent := parentMap[queryCall]

	// Discarded result: `_ := db.Query(...)` or bare `db.Query(...)`.
	if assign, ok := parent.(*ast.AssignStmt); ok && len(assign.Lhs) >= 1 {
		if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name == "_" {
			return QClassRowsDiscarded, "_"
		}
	}
	if _, ok := parent.(*ast.ExprStmt); ok {
		return QClassRowsDiscarded, ""
	}

	// Wrapper: `return db.Query(...)` — caller closes.
	if _, ok := parent.(*ast.ReturnStmt); ok {
		return QClassWrapperReturn, ""
	}

	// Delegate: `helper(db.Query(...))` — helper owns the rows.
	if _, ok := parent.(*ast.CallExpr); ok {
		return QClassDelegate, ""
	}

	varName := ""
	if assign, ok := parent.(*ast.AssignStmt); ok && len(assign.Lhs) >= 1 {
		if id, ok := assign.Lhs[0].(*ast.Ident); ok {
			varName = id.Name
		}
	}
	if varName == "" {
		return QClassUnknown, ""
	}

	funcBody := enclosingFuncBody(parentMap, queryCall)
	if funcBody == nil {
		return QClassUnknown, varName
	}

	// The covering positions: blocks where a defer (at or after the site's
	// statement) would cover this site.
	cover := coveringBlocks(queryCall, parentMap, varName, errVarName(parent))

	// Deferred closes, in order-sensitive positions.
	loopDefer := false
	plainDefer := false
	// Explicit (non-deferred) closes in covering positions.
	explicit := false
	// A `return rows, ...` that hands the rows to the caller.
	wrappedReturn := false
	// rows passed by value to a helper call (helper owns the close).
	delegate := false

	ast.Inspect(funcBody, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.DeferStmt:
			if deferTargetsVar(stmt, varName) && covers(cover, stmt, parentMap) {
				if inLoop(stmt, parentMap, funcBody) {
					loopDefer = true
				} else {
					plainDefer = true
				}
				return false // one matching defer suffices
			}
		case *ast.ExprStmt:
			if call, ok := stmt.X.(*ast.CallExpr); ok {
				if closeTargetsVar(call, varName) && covers(cover, stmt, parentMap) {
					explicit = true
					return false
				}
			}
		case *ast.ReturnStmt:
			if covers(cover, stmt, parentMap) {
				for _, r := range stmt.Results {
					if id, ok := r.(*ast.Ident); ok && id.Name == varName {
						wrappedReturn = true
						return false
					}
				}
			}
		case *ast.CallExpr:
			if callPassesVar(stmt, varName) && !closeTargetsVar(stmt, varName) &&
				inCoverPosition(stmt, cover, parentMap) {
				delegate = true
			}
		}
		return true
	})

	if plainDefer {
		return QClassDeferClose, varName
	}
	if loopDefer {
		return QClassLoopDefer, varName
	}
	if wrappedReturn {
		return QClassWrapperReturn, varName
	}
	if explicit {
		return QClassExplicitClose, varName
	}
	if delegate {
		return QClassDelegate, varName
	}
	// A site that looks like a database leak but whose result var is never
	// consumed as *sql.Rows (no .Next/.Scan/.Close/.Err, no helper pass) is
	// almost certainly a non-database method named Query (e.g. an LLM
	// client). Downgrade to non-db instead of a false leak.
	if !rowsVarLike(funcBody, varName) {
		return QClassNonDB, varName
	}
	return QClassNoClose, varName
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

// inCoverPosition reports whether `expr.Statement` sits in a covering
// position for the site (climbed to the first Statement that is a direct
// child of a block in `cover`).
func inCoverPosition(node ast.Node, cover []blockPos, parentMap map[ast.Node]ast.Node) bool {
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			if covers(cover, stmt, parentMap) {
				return true
			}
			return false
		}
		n = parentMap[n]
	}
	return false
}

// rowsVarLike reports whether varName is consumed as database/sql Rows
// anywhere in the function: `rows.Next()`, `rows.Scan(...)`, `rows.Err()`,
// `rows.Columns()`, or `rows.Close()`. Used to demote non-database Query
// calls (LLM clients, etc.) that would otherwise read as leaks.
func rowsVarLike(funcBody *ast.BlockStmt, varName string) bool {
	found := false
	ast.Inspect(funcBody, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != varName {
			return true
		}
		switch sel.Sel.Name {
		case "Next", "Scan", "Err", "Columns", "NextResultSet", "Close":
			found = true
			return false
		}
		return true
	})
	return found
}

// coveringBlocks returns the (block, after-index) positions in which a
// close statement would cover the site: the block containing the site's
// statement after its index, plus (recursively) each enclosing block after
// the inner statement's index. A Query inside an `if ... := ...; ` init
// also gets the if-body as a covering position (defer inside the body
// covers it).
//
// Guard bodies: the canonical MPM idiom
//
//	rows, err := db.Query(...)
//	if err == nil {
//	    defer rows.Close()
//	    ...
//	}
//
// defers inside an if whose condition tests the site's err var (or rows
// var) cover the site — the defer runs before any use of rows, and the
// `rows != nil` sanity is implied by err == nil. Those if bodies are added
// as covering positions from index 0.
func coveringBlocks(node ast.Node, parentMap map[ast.Node]ast.Node, varName, errVar string) []blockPos {
	var out []blockPos
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			if block, ok := parentMap[stmt].(*ast.BlockStmt); ok {
				out = append(out, blockPos{block: block, after: stmtIndexInBlock(block, stmt) + 1})
				// Guard bodies: if statements AFTER the site that test the
				// site var (err == nil / err != nil / rows != nil) extend
				// coverage into their bodies.
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
			// Statement whose parent is not a block: IfStmt/ForStmt init,
			// CaseClause body, etc. For if/for init, the construct's body
			// is a covering position from index 0.
			if ifs, ok := parentMap[stmt].(*ast.IfStmt); ok && ifs.Init == stmt && ifs.Body != nil {
				out = append(out, blockPos{block: ifs.Body, after: 0})
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

// errVarName returns the name of the error variable in `rows, err := q`.
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

// guardCondSite reports whether an if statement is a guard-testing the site
// variable: its condition references the rows var or the query's error var
// (e.g. `err == nil`, `rows != nil`). Bodies of such ifs are covering
// positions for the deferred close.
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

// covers reports whether stmt (a DeferStmt / ExprStmt / ReturnStmt) sits in
// a covering position for the site: its enclosing block must be one of the
// site's covering blocks, and its index must be at/after the recorded
// boundary.
func covers(cover []blockPos, stmt ast.Stmt, parentMap map[ast.Node]ast.Node) bool {
	block, ok := parentMap[stmt].(*ast.BlockStmt)
	if !ok {
		return false
	}
	idx := stmtIndexInBlock(block, stmt)
	for _, c := range cover {
		if c.block == block && idx >= c.after {
			return true
		}
	}
	return false
}

// deferTargetsVar returns true if the defer is `defer rows.Close()` or a
// deferred closure whose body calls `rows.Close()` (the closure form is
// equivalent: it runs at function return).
func deferTargetsVar(d *ast.DeferStmt, varName string) bool {
	call := d.Call
	if closeTargetsVar(call, varName) {
		return true
	}
	// Deferred func literal: `defer func() { ... rows.Close() ... }()`.
	if fn, ok := call.Fun.(*ast.FuncLit); ok {
		closes := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if inner, ok := n.(*ast.CallExpr); ok && closeTargetsVar(inner, varName) {
				closes = true
				return false
			}
			return true
		})
		return closes
	}
	return false
}

// closeTargetsVar returns true if call is `rows.Close()` (receiver is the
// named var).
func closeTargetsVar(call *ast.CallExpr, varName string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Close" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == varName
}

// inLoop reports whether stmt is inside a for loop (any ForStmt between it
// and the function body). A defer inside a loop accumulates rows until
// function return instead of releasing the connection per iteration.
func inLoop(stmt ast.Stmt, parentMap map[ast.Node]ast.Node, funcBody *ast.BlockStmt) bool {
	n := ast.Node(stmt)
	for n != nil && n != funcBody {
		if _, ok := n.(*ast.ForStmt); ok {
			return true
		}
		n = parentMap[n]
	}
	return false
}

func stmtIndexInBlock(block *ast.BlockStmt, target ast.Stmt) int {
	for i, s := range block.List {
		if s == target {
			return i
		}
	}
	return -1
}

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

	fmt.Fprintln(w, "# rows.Close() Audit Report")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "**Total Query-family call sites:** %d\n\n", len(sites))

	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	classOrder := []QClass{
		QClassDeferClose, QClassWrapperReturn, QClassLoopDefer,
		QClassExplicitClose, QClassDelegate, QClassNoClose,
		QClassRowsDiscarded, QClassNonDB, QClassUnknown,
	}
	for _, c := range classOrder {
		if n, ok := counts[c]; ok {
			fmt.Fprintf(w, "- **%s**: %d\n", c, n)
		}
	}
	fmt.Fprintln(w)

	total := len(sites)
	bad := counts[QClassNoClose] + counts[QClassRowsDiscarded] +
		counts[QClassLoopDefer] + counts[QClassUnknown]
	if total > 0 {
		pctBad := float64(bad) / float64(total) * 100
		fmt.Fprintf(w, "**Uncovered rows (leak risk): %d / %d (%.1f%%)**\n\n", bad, total, pctBad)
	}

	fmt.Fprintln(w, "## Sites Requiring Attention")
	fmt.Fprintln(w)
	skip := map[QClass]bool{
		QClassDeferClose:    true,
		QClassWrapperReturn: true,
		QClassNonDB:         true,
	}
	for _, s := range sites {
		if skip[s.Classification] {
			continue
		}
		fmt.Fprintf(w, "### `%s` — line %d (%s)", s.File, s.Line, s.Pattern)
		fmt.Fprintln(w)
		fmt.Fprintf(w, "- **Classification:** %s\n", s.Classification)
		fmt.Fprintf(w, "- **Enclosing function:** `%s`\n", s.FuncName)
		if s.VarName != "" {
			fmt.Fprintf(w, "- **Rows variable:** `%s`\n", s.VarName)
		}
		fmt.Fprintln(w, "- **Code:**")
		fmt.Fprintln(w, "  ```go")
		for _, line := range s.Code {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w, "  ```")
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "## defer-close Sites (correct)")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d sites are covered by a deferred close.\n", counts[QClassDeferClose])
}

func emitJSON(w *os.File, sites []Site) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(struct {
		Total int   `json:"total"`
		Sites []Site `json:"sites"`
	}{Total: len(sites), Sites: sites})
}

func countByClass(sites []Site) map[QClass]int {
	m := make(map[QClass]int)
	for _, s := range sites {
		m[s.Classification]++
	}
	return m
}
