// os.File descriptor leak rule — the ported audit-fd classifier.
//
// The substrate is a long-lived daemon that runs background goroutines
// (synthesis, idle dream, lifecycle decay) against a single shared SQLite
// connection. Every open file descriptor that is not eventually closed
// accumulates: under agent concurrency a handful of leaked FDs silently
// exhausts the process's ulimit -n and makes the whole daemon unresponsive
// (every subsequent open fails with EMFILE). FD hygiene is a load-path
// invariant, not a style preference.
//
// Classes:
//
//	fd-defer-close    — `defer f.Close()` present in a covering position
//	                    (correct; runs on every return path including panic)
//	fd-explicit-close — f.Close() called inline on every path (incl. error
//	                    returns) but not deferred
//	fd-returned       — the *os.File is returned to the caller (e.g.
//	                    scheduler.AcquireLock): ownership transfers
//	fd-delegate       — f is passed to a helper call that takes ownership
//	fd-no-close       — no Close() anywhere in the enclosing function: LEAK
//	fd-remove-only    — only os.Remove / defer os.Remove present, no Close:
//	                    LEAK (an unlinked-but-open descriptor still consumes
//	                    the fd table until exit)
//	fd-unknown        — pattern didn't fit (needs review)
//
// Order-sensitivity mirrors audit-closes: a `defer f.Close()` only covers its
// own assignment, and a close placed at the top of a loop body must not count
// a per-iteration open.
//
// Gate: fd-no-close / fd-remove-only at zero; fd-unknown and
// fd-explicit-close ratchet to their measured baseline (pre-existing
// reviewed sites exempt, new sites fail).
package audit

import (
	"fmt"
	"go/ast"
	"io"
)

// FDClass is the fd vocabulary.
type FDClass = string

const (
	FDDeferClose    FDClass = "fd-defer-close"
	FDExplicitClose FDClass = "fd-explicit-close"
	FDReturned      FDClass = "fd-returned"
	FDDelegate      FDClass = "fd-delegate"
	FDNoClose       FDClass = "fd-no-close"
	FDRemoveOnly    FDClass = "fd-remove-only"
	FDUnknown       FDClass = "fd-unknown"
)

// openCalls is the *os.File-producing call surface.
var openCalls = map[string]bool{
	"Open":         true,
	"OpenFile":     true,
	"Create":       true,
	"CreateTemp":   true,
	"OpenInCloser": true, // os.OpenFile via helper — matched separately
}

// FDRule inventories os.File open sites and classifies their close coverage.
type FDRule struct{}

// NewFDRule constructs the file-descriptor rule.
func NewFDRule() Rule { return &FDRule{} }

// Name implements Rule.
func (r *FDRule) Name() string { return "fd" }

// Prepare implements Rule (no cross-file state).
func (r *FDRule) Prepare(files []*File) {}

// AuditFile implements Rule.
func (r *FDRule) AuditFile(f *File) []Site {
	parentMap := f.ParentMap
	var sites []Site
	ast.Inspect(f.AST, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isOpenCall(call) {
			return true
		}
		pos := f.FSet.Position(call.Pos())
		if pos.Line == 0 {
			return true // synthetic nodes — skip
		}
		site := Site{
			File:     f.Path,
			Line:     pos.Line,
			Column:   pos.Column,
			FuncName: EnclosingFuncNameSkipClosure(parentMap, call),
			Pattern:  fdPatternString(call),
		}
		site.Classification, site.VarName = r.classify(call, parentMap)
		site.Code = FDContextLines(f.Src, pos.Line)
		sites = append(sites, site)
		return true
	})
	return sites
}

// isOpenCall returns true for os.Open / os.OpenFile / os.Create /
// os.CreateTemp — the *os.File-producing calls the audit covers.
func isOpenCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if !openCalls[sel.Sel.Name] {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "os"
}

func fdPatternString(call *ast.CallExpr) string {
	sel, _ := call.Fun.(*ast.SelectorExpr)
	return "os." + sel.Sel.Name
}

// classify determines the close coverage for one *os.File open site.
func (r *FDRule) classify(openCall ast.Node, parentMap map[ast.Node]ast.Node) (FDClass, string) {
	parent := parentMap[openCall]
	varName := ""

	if assign, ok := parent.(*ast.AssignStmt); ok && len(assign.Lhs) >= 1 {
		if id, ok := assign.Lhs[0].(*ast.Ident); ok {
			varName = id.Name
		}
	}
	if varName == "" {
		return FDUnknown, ""
	}

	funcBody := EnclosingFuncBody(parentMap, openCall)
	if funcBody == nil {
		return FDUnknown, varName
	}

	cover := r.coveringBlocks(openCall, parentMap, varName, ErrVarName(parent))

	deferClose := false
	explicitClose := false
	returned := false
	delegated := false

	ast.Inspect(funcBody, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.DeferStmt:
			if deferTargetsVar(stmt, varName) && InCoverPosition(stmt, cover, parentMap) {
				deferClose = true
				return false
			}
		case *ast.ExprStmt:
			if call, ok := stmt.X.(*ast.CallExpr); ok {
				if fdCloseTargetsVar(call, varName) && InCoverPosition(stmt, cover, parentMap) {
					explicitClose = true
					return false
				}
			}
		case *ast.AssignStmt:
			if len(stmt.Rhs) == 1 {
				if call, ok := stmt.Rhs[0].(*ast.CallExpr); ok &&
					fdCloseTargetsVar(call, varName) && InCoverPosition(stmt, cover, parentMap) {
					explicitClose = true
					return false
				}
			}
		case *ast.IfStmt:
			if stmt.Init != nil {
				var call *ast.CallExpr
				if es, ok := stmt.Init.(*ast.ExprStmt); ok {
					call, _ = es.X.(*ast.CallExpr)
				} else if assign, ok := stmt.Init.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 {
					call, _ = assign.Rhs[0].(*ast.CallExpr)
				}
				if call != nil && fdCloseTargetsVar(call, varName) && InCoverPosition(stmt, cover, parentMap) {
					explicitClose = true
					return false
				}
			}
		case *ast.ReturnStmt:
			if InCoverPosition(stmt, cover, parentMap) {
				for _, ret := range stmt.Results {
					if id, ok := ret.(*ast.Ident); ok && id.Name == varName {
						returned = true
						return false
					}
				}
			}
		case *ast.CallExpr:
			if fdCallPassesVar(stmt, varName) && InCoverPosition(stmt, cover, parentMap) {
				delegated = true
				return false
			}
		}
		return true
	})

	if deferClose {
		return FDDeferClose, varName
	}
	if returned {
		return FDReturned, varName
	}
	if explicitClose {
		return FDExplicitClose, varName
	}
	if delegated {
		return FDDelegate, varName
	}
	if fdHasRemoveFor(funcBody, varName) {
		return FDRemoveOnly, varName
	}
	return FDNoClose, varName
}

// coveringBlocks returns the (block, after-index) positions in which a
// close/defer would cover the site — the block after the open statement,
// plus each enclosing block after the inner statement, plus guard bodies
// (an `if err != nil { ... }` that tests the site's error var).
func (r *FDRule) coveringBlocks(node ast.Node, parentMap map[ast.Node]ast.Node, varName, errVar string) []BlockPos {
	var out []BlockPos
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			if block, ok := parentMap[stmt].(*ast.BlockStmt); ok {
				out = append(out, BlockPos{Block: block, After: StmtIndexInBlock(block, stmt) + 1})
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
			if ifs, ok := parentMap[stmt].(*ast.IfStmt); ok && ifs.Init == stmt && ifs.Body != nil {
				out = append(out, BlockPos{Block: ifs.Body, After: 0})
				if elseBlock, ok := ifs.Else.(*ast.BlockStmt); ok {
					out = append(out, BlockPos{Block: elseBlock, After: 0})
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

// fdCloseTargetsVar reports whether call is `f.Close()`-ish on the var.
func fdCloseTargetsVar(call *ast.CallExpr, varName string) bool {
	return closeTargetsVar(call, varName)
}

// fdCallPassesVar reports whether call passes the named var as an argument.
func fdCallPassesVar(call *ast.CallExpr, varName string) bool {
	for _, a := range call.Args {
		if id, ok := a.(*ast.Ident); ok && id.Name == varName {
			return true
		}
	}
	return false
}

// fdHasRemoveFor reports whether the function body ever removes the named
// temp file (defer os.Remove(path) or os.Remove(path)).
func fdHasRemoveFor(funcBody *ast.BlockStmt, varName string) bool {
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
			if rc, ok := a.(*ast.CallExpr); ok {
				if csel, ok := rc.Fun.(*ast.SelectorExpr); ok && csel.Sel.Name == "Name" {
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
	t := stmt.Call
	if closeTargetsVar(t, varName) {
		return true
	}
	if sel, ok := t.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Close" {
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == varName {
			return true
		}
	}
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
	return false
}

// EmitMarkdown implements Rule.
func (r *FDRule) EmitMarkdown(w io.Writer, sites []Site) {
	fmt.Fprintln(w, "# os.File close coverage")
	fmt.Fprintln(w)
	for _, cls := range []FDClass{
		FDDeferClose, FDExplicitClose, FDReturned,
		FDDelegate, FDNoClose, FDRemoveOnly, FDUnknown,
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

// GateChecks implements Rule. fd-no-close / fd-remove-only default to zero;
// fd-unknown / fd-explicit-close ratchet to measured baseline.
func (r *FDRule) GateChecks(sites []Site, max map[string]int) []GateCheck {
	counts := CountByClass(sites)
	checks := []GateCheck{
		{FDNoClose, counts[FDNoClose], max[FDNoClose]},
		{FDRemoveOnly, counts[FDRemoveOnly], max[FDRemoveOnly]},
		{FDUnknown, counts[FDUnknown], max[FDUnknown]},
		{FDExplicitClose, counts[FDExplicitClose], max[FDExplicitClose]},
	}
	return ResolveRatchet(checks)
}
