// rows.Close() coverage rule — the ported audit-closes classifier.
//
// Walks every database/sql *Rows-producing call — .Query(),
// .QueryContext(), .QueryTracked() — and verifies each result is covered by
// a `defer rows.Close()` before the function can return.
//
// Why this matters: a *sql.Rows that is never closed never returns its
// database connection to the pool. MPM runs a single shared SQLite
// connection; under agent concurrency, a handful of unclosed rows sites
// silently exhausts the pool and deadlocks the substrate. (QueryRow is NOT
// audited — *sql.Row closes its underlying rows on Scan by contract.)
//
// Each site is classified by how its rows are closed:
//
//	defer-close       — `defer rows.Close()` present in a covering position
//	                   (correct; runs on every return path including panic)
//	loop-defer        — `defer rows.Close()` sits INSIDE a loop: rows
//	                   accumulate until function return, holding the single
//	                   SQLite connection hostage for the whole loop
//	explicit-close    — `rows.Close()` called inline, not deferred: leaks on
//	                   any early return
//	delegate          — rows handed to a helper call; the helper owns close
//	wrapper-return    — function returns rows to its caller (`return rows, err`);
//	                   the caller owns the close (e.g. QueryTracked itself)
//	no-close          — no Close() anywhere in the enclosing function: LEAK
//	rows-discarded    — Query() result assigned to `_` or called bare: LEAK
//	unknown           — pattern didn't fit (needs review)
//
// Order-sensitivity: the classifier is position-aware. A `defer rows.Close()`
// only covers a site whose assignment executes BEFORE the defer in the same
// statement order. The double-assignment bug class — `rows, err := q1;
// defer rows.Close(); rows, err = q2` — is caught: q2's rows is never
// closed because the defer captured q1's value at defer time.
//
// Gate: zero-tolerance for no-close / rows-discarded / loop-defer / unknown.
package audit

import (
	"fmt"
	"go/ast"
	"io"
)

// QClass is the closes vocabulary.
type QClass = string

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

// queryMethods are the *sql.Rows-producing calls the rule covers.
// QueryRow / QueryRowTracked are excluded by design: *sql.Row closes the
// underlying rows automatically on Scan.
var queryMethods = map[string]bool{
	"Query":        true,
	"QueryContext": true,
	"QueryTracked": true,
}

// ClosesRule inventories Query-family sites and their close coverage.
type ClosesRule struct{}

// NewClosesRule constructs the rows-close rule.
func NewClosesRule() Rule { return &ClosesRule{} }

// Name implements Rule.
func (r *ClosesRule) Name() string { return "closes" }

// Prepare implements Rule (no cross-file state).
func (r *ClosesRule) Prepare(files []*File) {}

// AuditFile implements Rule.
func (r *ClosesRule) AuditFile(f *File) []Site {
	return findSites(f, func(call *ast.CallExpr, _ ast.Node) bool {
		return r.isQueryCall(call)
	}, func(call *ast.CallExpr, parent ast.Node) Site {
		pos := f.FSet.Position(call.Pos())
		site := Site{
			File:     f.Path,
			Line:     pos.Line,
			Column:   pos.Column,
			FuncName: EnclosingFuncName(f.ParentMap, call),
			Pattern:  r.patternString(call),
			Code:     ContextLines(f.Src, pos.Line),
		}
		site.Classification, site.VarName = r.classify(call, f.ParentMap)
		return site
	})
}

// EmitMarkdown implements Rule.
func (r *ClosesRule) EmitMarkdown(w io.Writer, sites []Site) {
	counts := CountByClass(sites)

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

// GateChecks implements Rule. no-close / rows-discarded / loop-defer /
// unknown default to zero-tolerance; the max map overrides per class.
func (r *ClosesRule) GateChecks(sites []Site, max map[string]int) []GateCheck {
	counts := CountByClass(sites)
	checks := []GateCheck{
		{QClassNoClose, counts[QClassNoClose], max[QClassNoClose]},
		{QClassRowsDiscarded, counts[QClassRowsDiscarded], max[QClassRowsDiscarded]},
		{QClassLoopDefer, counts[QClassLoopDefer], max[QClassLoopDefer]},
		{QClassUnknown, counts[QClassUnknown], max[QClassUnknown]},
	}
	return ResolveRatchet(checks)
}

func (r *ClosesRule) isQueryCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return queryMethods[sel.Sel.Name]
}

func (r *ClosesRule) patternString(call *ast.CallExpr) string {
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

// classify determines the close coverage class for one Query site.
// Order-sensitive: a defer only covers the assignment that precedes it.
func (r *ClosesRule) classify(queryCall ast.Node, parentMap map[ast.Node]ast.Node) (QClass, string) {
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

	funcBody := EnclosingFuncBody(parentMap, queryCall)
	if funcBody == nil {
		return QClassUnknown, varName
	}

	// The covering positions: blocks where a defer (at or after the site's
	// statement) would cover this site.
	cover := r.coveringBlocks(queryCall, parentMap, varName, ErrVarName(parent))

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
			if r.deferTargets(stmt, varName) && Covers(cover, stmt, parentMap) {
				if r.inLoop(stmt, parentMap, funcBody) {
					loopDefer = true
				} else {
					plainDefer = true
				}
				return false // one matching defer suffices
			}
		case *ast.ExprStmt:
			if call, ok := stmt.X.(*ast.CallExpr); ok {
				if r.closeTargets(call, varName) && Covers(cover, stmt, parentMap) {
					explicit = true
					return false
				}
			}
		case *ast.ReturnStmt:
			if Covers(cover, stmt, parentMap) {
				for _, res := range stmt.Results {
					if id, ok := res.(*ast.Ident); ok && id.Name == varName {
						wrappedReturn = true
						return false
					}
				}
			}
		case *ast.CallExpr:
			if r.callPassesVar(stmt, varName) && !r.closeTargets(stmt, varName) &&
				InCoverPosition(stmt, cover, parentMap) {
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
	if !r.rowsVarLike(funcBody, varName) {
		return QClassNonDB, varName
	}
	return QClassNoClose, varName
}

// callPassesVar reports whether call passes the named var as an argument.
func (r *ClosesRule) callPassesVar(call *ast.CallExpr, varName string) bool {
	for _, a := range call.Args {
		if id, ok := a.(*ast.Ident); ok && id.Name == varName {
			return true
		}
	}
	return false
}

// rowsVarLike reports whether varName is consumed as database/sql Rows
// anywhere in the function: `rows.Next()`, `rows.Scan(...)`, `rows.Err()`,
// `rows.Columns()`, or `rows.Close()`. Used to demote non-database Query
// calls (LLM clients, etc.) that would otherwise read as leaks.
func (r *ClosesRule) rowsVarLike(funcBody *ast.BlockStmt, varName string) bool {
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
func (r *ClosesRule) coveringBlocks(query ast.Node, parentMap map[ast.Node]ast.Node, varName, errVar string) []BlockPos {
	var out []BlockPos
	n := query
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			if block, ok := parentMap[stmt].(*ast.BlockStmt); ok {
				out = append(out, BlockPos{Block: block, After: StmtIndexInBlock(block, stmt) + 1})
				// Guard bodies: if statements AFTER the query that test the
				// site var (err == nil / err != nil / rows != nil) extend
				// coverage into their bodies.
				for i := StmtIndexInBlock(block, stmt) + 1; i < len(block.List); i++ {
					ifs, ok := block.List[i].(*ast.IfStmt)
					if !ok {
						continue
					}
					if GuardCondSite(ifs, varName, errVar) {
						out = append(out, BlockPos{Block: ifs.Body, After: 0})
						if elseBlock, ok := ifs.Else.(*ast.BlockStmt); ok {
							out = append(out, BlockPos{Block: elseBlock, After: 0})
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
				out = append(out, BlockPos{Block: ifs.Body, After: 0})
				n = ifs
				continue
			}
			if fr, ok := parentMap[stmt].(*ast.ForStmt); ok && fr.Init == stmt && fr.Body != nil {
				out = append(out, BlockPos{Block: fr.Body, After: 0})
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

// deferTargets reports whether the defer is `defer rows.Close()` or a
// deferred closure whose body calls `rows.Close()` (the closure form is
// equivalent: it runs at function return).
func (r *ClosesRule) deferTargets(d *ast.DeferStmt, varName string) bool {
	call := d.Call
	if r.closeTargets(call, varName) {
		return true
	}
	if fn, ok := call.Fun.(*ast.FuncLit); ok {
		closes := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if inner, ok := n.(*ast.CallExpr); ok && r.closeTargets(inner, varName) {
				closes = true
				return false
			}
			return true
		})
		return closes
	}
	return false
}

// closeTargets reports whether call is `rows.Close()` on the named var.
func (r *ClosesRule) closeTargets(call *ast.CallExpr, varName string) bool {
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
func (r *ClosesRule) inLoop(stmt ast.Stmt, parentMap map[ast.Node]ast.Node, funcBody *ast.BlockStmt) bool {
	n := ast.Node(stmt)
	for n != nil && n != funcBody {
		if _, ok := n.(*ast.ForStmt); ok {
			return true
		}
		n = parentMap[n]
	}
	return false
}
