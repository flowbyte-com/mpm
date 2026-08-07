// Transaction boundary safety rule — the ported audit-tx classifier.
//
// Inventories every manual database transaction — `.Begin()` / `.BeginTx()`
// — and verifies each is governed by a deferred `tx.Rollback()` in a
// covering position.
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
//	tx-explicit-rollback — Rollback() called on error paths but NO deferred
//	                    rollback: leaks on panic or on a future early return
//	tx-commit-only     — Commit present, no Rollback found ANYWHERE in the
//	                    enclosing function: leaks every non-commit path
//	tx-no-rollback     — neither Commit nor Rollback found: open tx with no
//	                    release observed at all
//	tx-unknown         — pattern did not fit (needs review)
//
// Order-sensitivity: a `defer tx.Rollback()` only covers a transaction
// whose Begin executed BEFORE the defer in statement order. The
// double-assignment class (`tx, err := q1; defer tx.Rollback(); tx, err =
// q2`) is caught — the first defer captured q1's tx, q2 is never rolled
// back.
//
// Gate: zero-tolerance for tx-explicit-rollback / tx-commit-only /
// tx-no-rollback / tx-unknown.
package audit

import (
	"fmt"
	"go/ast"
	"io"
)

// TXClass is the tx vocabulary.
type TXClass = string

const (
	TXClassDeferRollback    TXClass = "tx-defer-rollback"
	TXClassDelegate         TXClass = "tx-delegate"
	TXClassExplicitRollback TXClass = "tx-explicit-rollback"
	TXClassCommitOnly       TXClass = "tx-commit-only"
	TXClassNoRollback       TXClass = "tx-no-rollback"
	TXClassUnknown          TXClass = "tx-unknown"
)

// txMethods are the transaction-opening calls the rule covers.
var txMethods = map[string]bool{
	"Begin":   true,
	"BeginTx": true,
}

// TXRule inventories Begin/BeginTx sites and their rollback coverage.
type TXRule struct{}

// NewTXRule constructs the transaction-boundary rule.
func NewTXRule() Rule { return &TXRule{} }

// Name implements Rule.
func (r *TXRule) Name() string { return "tx" }

// Prepare implements Rule (no cross-file state).
func (r *TXRule) Prepare(files []*File) {}

// AuditFile implements Rule.
func (r *TXRule) AuditFile(f *File) []Site {
	return findSites(f, func(call *ast.CallExpr, _ ast.Node) bool {
		return r.isTxCall(call)
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
func (r *TXRule) EmitMarkdown(w io.Writer, sites []Site) {
	counts := CountByClass(sites)

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
	bad := counts[TXClassExplicitRollback] + counts[TXClassCommitOnly] +
		counts[TXClassNoRollback] + counts[TXClassUnknown]
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

// GateChecks implements Rule. All four gated classes default to zero.
func (r *TXRule) GateChecks(sites []Site, max map[string]int) []GateCheck {
	counts := CountByClass(sites)
	checks := []GateCheck{
		{TXClassExplicitRollback, counts[TXClassExplicitRollback], max[TXClassExplicitRollback]},
		{TXClassCommitOnly, counts[TXClassCommitOnly], max[TXClassCommitOnly]},
		{TXClassNoRollback, counts[TXClassNoRollback], max[TXClassNoRollback]},
		{TXClassUnknown, counts[TXClassUnknown], max[TXClassUnknown]},
	}
	return ResolveRatchet(checks)
}

// isTxCall returns true if the call is a transaction-opening db method:
// .Begin / .BeginTx.
func (r *TXRule) isTxCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return txMethods[sel.Sel.Name]
}

func (r *TXRule) patternString(call *ast.CallExpr) string {
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

// classify determines the rollback-control class for one Begin site.
// Order-sensitive: a defer only covers the assignment that precedes it.
func (r *TXRule) classify(txCall ast.Node, parentMap map[ast.Node]ast.Node) (TXClass, string) {
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

	funcBody := EnclosingFuncBody(parentMap, txCall)
	if funcBody == nil {
		return TXClassUnknown, varName
	}

	cover := r.coveringBlocks(txCall, parentMap, varName, ErrVarName(parent))

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
			if r.deferTargets(stmt, varName) {
				anyDefer = true
				if Covers(cover, stmt, parentMap) {
					coveringDefer = true
				}
			}
		case *ast.ExprStmt:
			if call, ok := stmt.X.(*ast.CallExpr); ok {
				if r.rollbackTargets(call, varName) {
					explicit = true
				}
				if r.commitTargets(call, varName) {
					commit = true
				}
			}
		case *ast.AssignStmt:
			// `_ = tx.Rollback()` — the discard-assignment rollback idiom.
			for _, rhs := range stmt.Rhs {
				if call, ok := rhs.(*ast.CallExpr); ok {
					if r.rollbackTargets(call, varName) {
						explicit = true
					}
					if r.commitTargets(call, varName) {
						commit = true
					}
				}
			}
		case *ast.CallExpr:
			if r.txPassedToHelper(stmt, varName) && InCoverPosition(stmt, cover, parentMap) {
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
func (r *TXRule) txPassedToHelper(call *ast.CallExpr, varName string) bool {
	if r.rollbackTargets(call, varName) || r.commitTargets(call, varName) {
		return false
	}
	for _, a := range call.Args {
		if id, ok := a.(*ast.Ident); ok && id.Name == varName {
			return true
		}
	}
	return false
}

// rollbackTargets returns true if call is `tx.Rollback()`.
func (r *TXRule) rollbackTargets(call *ast.CallExpr, varName string) bool {
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

// commitTargets returns true if call is `tx.Commit()`.
func (r *TXRule) commitTargets(call *ast.CallExpr, varName string) bool {
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

// deferTargets returns true if the defer is `defer tx.Rollback()` or a
// deferred closure whose body calls `tx.Rollback()` (the commit-or-rollback
// funnel used by WithTx and friends).
func (r *TXRule) deferTargets(d *ast.DeferStmt, varName string) bool {
	call := d.Call
	if r.rollbackTargets(call, varName) {
		return true
	}
	if fn, ok := call.Fun.(*ast.FuncLit); ok {
		rolls := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if inner, ok := n.(*ast.CallExpr); ok && r.rollbackTargets(inner, varName) {
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

// coveringBlocks returns (block, after-index) positions in which a close
// statement would cover the site. Mirrors the closes logic: the containing
// block after the site, plus each enclosing block after the inner statement;
// if/for inits bring the body in from index 0.
//
// Guard bodies: the canonical MPM idiom
//
//	tx, err := db.Begin()
//	if err != nil { ... } else { defer tx.Rollback(); ... }
//
// defers inside an if whose condition tests the site's err var (or tx var)
// cover the site, so those if bodies (and else branches) are added as
// covering positions from index 0.
func (r *TXRule) coveringBlocks(node ast.Node, parentMap map[ast.Node]ast.Node, varName, errVar string) []BlockPos {
	var out []BlockPos
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			if block, ok := parentMap[stmt].(*ast.BlockStmt); ok {
				out = append(out, BlockPos{Block: block, After: StmtIndexInBlock(block, stmt) + 1})
				// Guard bodies: if/else statements AFTER the site that test
				// the site's tx or err var extend coverage into their blocks.
				for i := StmtIndexInBlock(block, stmt) + 1; i < len(block.List); i++ {
					ifs, ok := block.List[i].(*ast.IfStmt)
					if !ok {
						continue
					}
					if GuardCondSite(ifs, varName, errVar) {
						if ifs.Body != nil {
							out = append(out, BlockPos{Block: ifs.Body, After: 0})
						}
						if elseBlock, ok := ifs.Else.(*ast.BlockStmt); ok {
							out = append(out, BlockPos{Block: elseBlock, After: 0})
						}
					}
				}
				n = block
				continue
			}
			if ifs, ok := parentMap[stmt].(*ast.IfStmt); ok && ifs.Init == stmt && ifs.Body != nil {
				out = append(out, BlockPos{Block: ifs.Body, After: 0})
				if ifs.Else != nil {
					if elseBlock, ok := ifs.Else.(*ast.BlockStmt); ok {
						out = append(out, BlockPos{Block: elseBlock, After: 0})
					}
				}
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
