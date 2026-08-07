// cmd/audit-tx/main.go — Static analyzer for transaction-boundary safety.
//
// The transaction brother of cmd/audit-closes (rows.Close coverage). This
// tool walks Go source trees (default: internal/, cmd/) and inventories
// every manual database transaction — `.Begin()` / `.BeginTx()` — and
// verifies each is governed by a deferred `tx.Rollback()` in a covering
// position.
//
// Why this matters: `tx.Rollback()` called *explicitly* on error paths (but
// not deferred) leaks the transaction — and with it the SQLite connection —
// on any panic, or on any early return a developer adds later without
// remembering the manual rollback. The safe no-op nature of rolling back
// after a commit means `defer tx.Rollback()` costs nothing on the happy
// path, so there is no excuse for a managed transaction that lacks one.
//
// Each site is classified by how its transaction is released:
//
//	tx-defer-rollback — `defer tx.Rollback()` present in a covering position,
//	                    directly or via a deferred closure that tests commit
//	                    (rollback-if-err / rollback-on-recover): CORRECT
//	tx-delegate        — tx handed to a wrapper function (WithTx-style) that
//	                    is documented to specership rollback+commit: OK but
//	                    REVIEW to confirm the wrapper commits+rollbacks
//	tx-explicit-rollback— Rollback() called on error paths but NO deferred
//	                    rollback: the anti-pattern — leaks on panic or on a
//	                    future early return
//	tx-commit-only     — Commit present, no Rollback found ANYWHERE in the
//	                    enclosing function: leaks every non-commit path
//	tx-no-rollback     — neither Commit nor Rollback found: open tx with no
//	                    release observed at all
//	tx-unknown         — pattern did not fit (needs review)
//
// Order-sensitivity: like the rows audit, a `defer tx.Rollback()` only
// covers a transaction whose Begin executed BEFORE the defer in statement
// order. The double-assignment class (`tx, err := q1; defer tx.Rollback();
// tx, err = q2`) is caught — the first defer captured q1's tx, q2 is never
// rolled back.
//
// Usage:
//
//	go run ./cmd/audit-tx                  # markdown report on stdout
//	go run ./cmd/audit-tx --json           # JSON sidecar
//	go run ./cmd/audit-tx --include-tests  # include _test.go files
//	go run ./cmd/audit-tx --roots=a,b
//	go run ./cmd/audit-tx --gate           # exit non-zero on fatal classes
//
// The pre-commit hook runs `audit-tx --gate` alongside the other gates.

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

type TXClass string

const (
	TXClassDeferRollback    TXClass = "tx-defer-rollback"
	TXClassDelegate         TXClass = "tx-delegate"
	TXClassExplicitRollback TXClass = "tx-explicit-rollback"
	TXClassCommitOnly       TXClass = "tx-commit-only"
	TXClassNoRollback       TXClass = "tx-no-rollback"
	TXClassUnknown          TXClass = "tx-unknown"
)

// Site is one Begin/BeginTx call site with its rollback coverage class.
type Site struct {
	File           string   `json:"file"`
	Line           int      `json:"line"`
	Column         int      `json:"column"`
	FuncName       string   `json:"func_name"`
	Pattern        string   `json:"pattern"`
	VarName        string   `json:"var_name"`
	Classification TXClass  `json:"classification"`
	Code           []string `json:"code"`
}

// transactionMethods are the transaction-opening calls the audit covers.
var txMethods = map[string]bool{
	"Begin":   true,
	"BeginTx": true,
}

// -------------------------------------------------------------------- main

func main() {
	var (
		jsonOut      = flag.Bool("json", false, "emit JSON sidecar instead of markdown")
		includeTests = flag.Bool("include-tests", false, "scan _test.go files too")
		rootsFlag    = flag.String("roots", "internal/core,cmd/mpm", "comma-separated root dirs to walk")
		jsonSidecar  = flag.String("json-out", "", "if set, also write JSON to this path")
		gate         = flag.Bool("gate", false, "exit non-zero if any fatal-class count exceeds its threshold (tx-explicit-rollback / tx-commit-only / tx-no-rollback / tx-unknown default to 0)")
		maxExplicit  = flag.Int("max-tx-explicit", 0, "threshold for tx-explicit-rollback (used with --gate)")
		maxCommit    = flag.Int("max-tx-commit", 0, "threshold for tx-commit-only (used with --gate)")
		maxNoRoll    = flag.Int("max-tx-no-rollback", 0, "threshold for tx-no-rollback (used with --gate)")
		maxUnknown   = flag.Int("max-tx-unknown", 0, "threshold for tx-unknown (used with --gate)")
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
			fmt.Fprintf(os.Stderr, "audit-tx: walk %s: %v\n", root, err)
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
			fmt.Fprintf(os.Stderr, "audit-tx: create %s: %v\n", *jsonSidecar, err)
			os.Exit(1)
		}
		defer f.Close()
		emitJSON(f, sites)
		fmt.Fprintf(os.Stderr, "audit-tx: wrote %d sites to %s\n", len(sites), *jsonSidecar)
	}

	if *gate {
		exitCode := runGate(sites, gateThresholds{
			Explicit: *maxExplicit,
			Commit:   *maxCommit,
			NoRoll:   *maxNoRoll,
			Unknown:  *maxUnknown,
		})
		os.Exit(exitCode)
	}
}

type gateThresholds struct {
	Explicit int
	Commit   int
	NoRoll   int
	Unknown  int
}

// runGate evaluates the gate thresholds and returns 0 (pass) or 1 (fail).
func runGate(sites []Site, t gateThresholds) int {
	counts := countByClass(sites)

	type check struct {
		name      string
		actual    int
		threshold int
	}
	checks := []check{
		{"tx-explicit-rollback", counts[TXClassExplicitRollback], t.Explicit},
		{"tx-commit-only", counts[TXClassCommitOnly], t.Commit},
		{"tx-no-rollback", counts[TXClassNoRollback], t.NoRoll},
		{"tx-unknown", counts[TXClassUnknown], t.Unknown},
	}

	var failed []check
	for _, c := range checks {
		if c.actual > c.threshold {
			failed = append(failed, c)
		}
	}

	fmt.Fprintln(os.Stderr, "[audit-tx --gate] transaction rollback coverage thresholds:")
	for _, c := range checks {
		status := "ok"
		if c.actual > c.threshold {
			status = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "  %-24s actual=%d  threshold=%d  %s\n", c.name, c.actual, c.threshold, status)
	}

	if len(failed) > 0 {
		fmt.Fprintln(os.Stderr, "[audit-tx --gate] FAIL: transaction-boundary classes exceed thresholds")
		for _, c := range failed {
			fmt.Fprintf(os.Stderr, "  - %s: %d > %d\n", c.name, c.actual, c.threshold)
		}
		return 1
	}

	fmt.Fprintln(os.Stderr, "[audit-tx --gate] PASS")
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
			fmt.Fprintf(os.Stderr, "audit-tx: parse %s: %v\n", path, err)
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

	// Pass 2: classify every Begin-family call site.
	var sites []Site
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isTxCall(call) {
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

// isTxCall returns true if the call is a transaction-opening db method:
// .Begin / .BeginTx. (BeginTx is common on both *sql.DB and custom
// connection types.) Method-name filtering is sufficient.
func isTxCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return txMethods[sel.Sel.Name]
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

// classify determines the rollback-control class for one Begin site.
// Order-sensitive: a defer only covers the assignment that precedes it.
func classify(txCall ast.Node, parentMap map[ast.Node]ast.Node) (TXClass, string) {
	parent := parentMap[txCall]

	varName := ""
	if assign, ok := parent.(*ast.AssignStmt); ok && len(assign.Lhs) >= 1 {
		if id, ok := assign.Lhs[0].(*ast.Ident); ok {
			varName = id.Name
		}
	}
	if varName == "" {
		return TXClassUnknown, ""
	}

	funcBody := enclosingFuncBody(parentMap, txCall)
	if funcBody == nil {
		return TXClassUnknown, varName
	}

	cover := coveringBlocks(txCall, parentMap, varName, errVarName(parent))

	// A covering `defer tx.Rollback()` (direct or via a deferred closure)
	// is the correct pattern. Explicit non-deferred rollback calls are the
	// anti-pattern: they cover the error paths written today but leak on a
	// panic or on a future early return. The searches below are function-
	// wide (not position-restricted) so `_ = tx.Rollback()` on error paths
	// classifies as explicit rather than silently as no-rollback.
	coveringDefer := false
	anyDefer := false  // rollback inside SOME defer (used to distinguish)
	explicit := false  // non-deferred tx.Rollback() anywhere in the function
	commit := false    // tx.Commit() anywhere in the function
	delegated := false // tx handed to a helper (WithTx-style wrapper)

	ast.Inspect(funcBody, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.DeferStmt:
			if deferTargetsVar(stmt, varName) {
				anyDefer = true
				if covers(cover, stmt, parentMap) {
					coveringDefer = true
				}
			}
		case *ast.ExprStmt:
			if call, ok := stmt.X.(*ast.CallExpr); ok {
				if rollbackTargetsVar(call, varName) {
					explicit = true
				}
				if commitTargetsVar(call, varName) {
					commit = true
				}
			}
		case *ast.AssignStmt:
			// `_ = tx.Rollback()` — the discard-assignment rollback idiom.
			for _, rhs := range stmt.Rhs {
				if call, ok := rhs.(*ast.CallExpr); ok {
					if rollbackTargetsVar(call, varName) {
						explicit = true
					}
					if commitTargetsVar(call, varName) {
						commit = true
					}
				}
			}
		case *ast.CallExpr:
			if txPassedToHelper(stmt, varName) && inCoverPosition(stmt, cover, parentMap) {
				delegated = true
			}
		}
		return true
	})

	if coveringDefer {
		return TXClassDeferRollback, varName
	}
	if anyDefer && !coveringDefer {
		// A defer exists but does not cover this Begin (e.g. it was captured
		// by a prior assignment's defer, or the Begin was reassigned into
		// the same var). Fall through: treat like no covering defer.
	}
	if delegated {
		// tx handed to a documented wrapper; verify the wrapper commits and
		// rolls back (e.g. WithTx). This is the tx-delegate class.
		return TXClassDelegate, varName
	}
	if explicit {
		return TXClassExplicitRollback, varName
	}
	if commit {
		return TXClassCommitOnly, varName
	}
	return TXClassNoRollback, varName
}

// txPassedToHelper reports whether call passes the named tx var as an arg
// (the WithTx-style wrapper pattern).
func txPassedToHelper(call *ast.CallExpr, varName string) bool {
	if rollbackTargetsVar(call, varName) || commitTargetsVar(call, varName) {
		return false
	}
	for _, a := range call.Args {
		if id, ok := a.(*ast.Ident); ok && id.Name == varName {
			return true
		}
	}
	return false
}

// rollbackTargetsVar returns true if call is `tx.Rollback()`.
func rollbackTargetsVar(call *ast.CallExpr, varName string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Rollback" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == varName
}

// commitTargetsVar returns true if call is `tx.Commit()`.
func commitTargetsVar(call *ast.CallExpr, varName string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Commit" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == varName
}

// deferTargetsVar returns true if the defer is `defer tx.Rollback()` or a
// deferred closure whose body calls `tx.Rollback()` (covers the nested
// commit-or-rollback funnel used by WithTx and friends).
func deferTargetsVar(d *ast.DeferStmt, varName string) bool {
	call := d.Call
	if rollbackTargetsVar(call, varName) {
		return true
	}
	if fn, ok := call.Fun.(*ast.FuncLit); ok {
		rolls := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if inner, ok := n.(*ast.CallExpr); ok && rollbackTargetsVar(inner, varName) {
				rolls = true
				return false
			}
			return true
		})
		return rolls
	}
	return false
}

// -------------------------------------------------------------------- coverage

type blockPos struct {
	block *ast.BlockStmt
	after int
}

// coveringBlocks returns (block, after-index) positions in which a close
// statement would cover the site. Mirrors the audit-closes logic: the
// containing block after the site, plus each enclosing block after the inner
// statement; if/for inits bring the body in from index 0.
//
// Guard bodies: the canonical MPM idiom
//
//	tx, err := db.Begin()
//	if err != nil {
//	    return ...
//	}
//	defer tx.Rollback()
//	... // or
//
//	tx, err := db.Begin()
//	if err != nil { ... } else { defer tx.Rollback(); ... }
//
// defers inside an if whose condition tests the site's err var (or tx var)
// cover the site, so those if bodies (and else branches) are added as
// covering positions from index 0.
func coveringBlocks(node ast.Node, parentMap map[ast.Node]ast.Node, varName, errVar string) []blockPos {
	var out []blockPos
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			if block, ok := parentMap[stmt].(*ast.BlockStmt); ok {
				out = append(out, blockPos{block: block, after: stmtIndexInBlock(block, stmt) + 1})
				// Guard bodies: if/else statements AFTER the site that test
				// the site's tx or err var extend coverage into their blocks.
				for i := stmtIndexInBlock(block, stmt) + 1; i < len(block.List); i++ {
					ifs, ok := block.List[i].(*ast.IfStmt)
					if !ok {
						continue
					}
					if guardCondSite(ifs, varName, errVar) {
						if ifs.Body != nil {
							out = append(out, blockPos{block: ifs.Body, after: 0})
						}
						if elseBlock, ok := ifs.Else.(*ast.BlockStmt); ok {
							out = append(out, blockPos{block: elseBlock, after: 0})
						}
					}
				}
				n = block
				continue
			}
			if ifs, ok := parentMap[stmt].(*ast.IfStmt); ok && ifs.Init == stmt && ifs.Body != nil {
				out = append(out, blockPos{block: ifs.Body, after: 0})
				if ifs.Else != nil {
					if elseBlock, ok := ifs.Else.(*ast.BlockStmt); ok {
						out = append(out, blockPos{block: elseBlock, after: 0})
					}
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

// guardCondSite reports whether an if statement is a guard testing the site
// variable: its condition references the tx var or the query's err var
// (e.g. `err != nil`, `txErr != nil`). Bodies of such ifs are covering
// positions for the deferred rollback.
func guardCondSite(ifs *ast.IfStmt, varName, errVar string) bool {
	if varName != "" && referencesIdent(ifs.Cond, varName) {
		return true
	}
	if errVar != "" && referencesIdent(ifs.Cond, errVar) {
		return true
	}
	return false
}

// errVarName returns the name of the error variable in the
// `tx, err := Begin()` assignment.
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

// covers reports whether stmt sits in a covering position for the site.
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

// inCoverPosition reports whether node climbs to a Statement in a covering
// position for the site.
func inCoverPosition(node ast.Node, cover []blockPos, parentMap map[ast.Node]ast.Node) bool {
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			return covers(cover, stmt, parentMap)
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

	fmt.Fprintln(w, "# Transaction Boundary Audit Report")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "**Total Begin/BeginTx call sites:** %d\n\n", len(sites))

	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	classOrder := []TXClass{
		TXClassDeferRollback, TXClassDelegate, TXClassExplicitRollback,
		TXClassCommitOnly, TXClassNoRollback, TXClassUnknown,
	}
	for _, c := range classOrder {
		if n, ok := counts[c]; ok {
			fmt.Fprintf(w, "- **%s**: %d\n", c, n)
		}
	}
	fmt.Fprintln(w)

	total := len(sites)
	bad := counts[TXClassExplicitRollback] + counts[TXClassCommitOnly] + counts[TXClassNoRollback] + counts[TXClassUnknown] + counts[TXClassCommitOnly] + counts[TXClassNoRollback] + counts[TXClassUnknown]
	if total > 0 {
		pctBad := float64(bad) / float64(total) * 100
		fmt.Fprintf(w, "**Uncovered transactions (leak risk): %d / %d (%.1f%%)**\n\n", bad, total, pctBad)
	}

	fmt.Fprintln(w, "## Sites Requiring Attention")
	fmt.Fprintln(w)
	skip := map[TXClass]bool{TXClassDeferRollback: true}
	for _, s := range sites {
		if skip[s.Classification] {
			continue
		}
		fmt.Fprintf(w, "### `%s` — line %d (%s)\n", s.File, s.Line, s.Pattern)
		fmt.Fprintf(w, "- **Classification:** %s\n", s.Classification)
		fmt.Fprintf(w, "- **Enclosing function:** `%s`\n", s.FuncName)
		if s.VarName != "" {
			fmt.Fprintf(w, "- **Tx variable:** `%s`\n", s.VarName)
		}
		fmt.Fprintln(w, "- **Code:**")
		fmt.Fprintln(w, "  ```go")
		for _, line := range s.Code {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w, "  ```")
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "## tx-defer-rollback Sites (correct)")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d sites are covered by a deferred rollback.\n", counts[TXClassDeferRollback])
}

func emitJSON(w *os.File, sites []Site) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(struct {
		Total int    `json:"total"`
		Sites []Site `json:"sites"`
	}{Total: len(sites), Sites: sites})
}

func countByClass(sites []Site) map[TXClass]int {
	m := make(map[TXClass]int)
	for _, s := range sites {
		m[s.Classification]++
	}
	return m
}
