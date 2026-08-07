// Mutex lock/Unlock discipline rule — the ported audit-mutex classifier.
//
// The substrate is a single shared SQLite connection with background worker
// goroutines. A lock acquired without a covering deferred release (or an
// unlock that can be skipped by control flow) is a permanent deadlock under
// load: the worker that took it never releases, and because every other
// goroutine funnels through DatabaseManager, one stray lock freezes the
// whole brain until an operator intervenes.
//
// Classes:
//
//	mu-defer-unlock    — `defer mu.Unlock()` / `defer mu.RUnlock()` present
//	                    in a covering position: CORRECT
//	mu-delegated       — the mutex receiver is handed to another function
//	                    (ownership transfers; callee must unlock): OK
//	mu-explicit-unlock — Unlock() present in the same block after the Lock
//	                    with no early return between — the "brief critical
//	                    section" idiom: correct today, but must not grow
//	mu-no-unlock       — no unlock anywhere in a safe position: FATAL
//	mu-unknown         — pattern did not fit (needs review)
//
// Order-sensitivity: a deferred unlock only covers a lock acquired before
// it; an explicit unlock only counts if no return sits between it and the
// lock in the same block.
//
// Gate: mu-no-unlock / mu-unknown at zero; mu-explicit-unlock ratchets to
// its measured baseline (so new non-deferred locks fail the commit).
package audit

import (
	"fmt"
	"go/ast"
	"io"
)

// MutexClass is the mutex vocabulary.
type MutexClass = string

const (
	MuDeferUnlock    MutexClass = "mu-defer-unlock"
	MuDelegated      MutexClass = "mu-delegated"
	MuExplicitUnlock MutexClass = "mu-explicit-unlock"
	MuNoUnlock       MutexClass = "mu-no-unlock"
	MuUnknown        MutexClass = "mu-unknown"
)

// mutexLockMethods are the acquisition calls the rule covers.
var mutexLockMethods = map[string]bool{
	"Lock":    true,
	"RLock":   true,
	"TryLock": true,
}

// MutexRule inventories lock acquisition sites and their unlock coverage.
type MutexRule struct{}

// NewMutexRule constructs the mutex-discipline rule.
func NewMutexRule() Rule { return &MutexRule{} }

// Name implements Rule.
func (r *MutexRule) Name() string { return "mutex" }

// Prepare implements Rule (no cross-file state).
func (r *MutexRule) Prepare(files []*File) {}

// AuditFile implements Rule.
func (r *MutexRule) AuditFile(f *File) []Site {
	parentMap := f.ParentMap
	var sites []Site
	ast.Inspect(f.AST, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isMutexLockCall(call) {
			return true
		}
		pos := f.FSet.Position(call.Pos())
		lockMethod := mutexLockMethodName(call)
		site := Site{
			File:       f.Path,
			Line:       pos.Line,
			Column:     pos.Column,
			FuncName:   EnclosingFuncNameOrClosure(parentMap, call),
			Pattern:    mutexPatternString(call),
			LockMethod: lockMethod,
		}
		site.Classification, site.VarName = r.classify(call, parentMap, lockMethod)
		site.Code = ContextLines(f.Src, pos.Line)
		sites = append(sites, site)
		return true
	})
	return sites
}

// isMutexLockCall matches the acquisition call surface.
func isMutexLockCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return mutexLockMethods[sel.Sel.Name]
}

func mutexLockMethodName(call *ast.CallExpr) string {
	sel, _ := call.Fun.(*ast.SelectorExpr)
	return sel.Sel.Name
}

func mutexPatternString(call *ast.CallExpr) string {
	sel, _ := call.Fun.(*ast.SelectorExpr)
	switch x := sel.X.(type) {
	case *ast.Ident:
		return x.Name + "." + sel.Sel.Name
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name + "." + x.Sel.Name + "." + sel.Sel.Name
		}
	case *ast.StarExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return "*" + id.Name + "." + sel.Sel.Name
		}
	}
	return "?" + sel.Sel.Name
}

// mutexReceiver renders the receiver qualifier to a canonical string.
func mutexReceiver(x ast.Expr) string {
	switch t := x.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return mutexReceiver(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + mutexReceiver(t.X)
	}
	return ""
}

// unlockMethodFor returns the release method that pairs with the lock method.
func unlockMethodFor(lockMethod string) string {
	switch lockMethod {
	case "Lock", "TryLock":
		return "Unlock"
	case "RLock":
		return "RUnlock"
	}
	return ""
}

// classify determines the unlock-coverage class for one Lock/RLock site.
func (r *MutexRule) classify(lockCall ast.Node, parentMap map[ast.Node]ast.Node, lockMethod string) (MutexClass, string) {
	call, _ := lockCall.(*ast.CallExpr)
	if call == nil {
		return MuUnknown, ""
	}
	sel, _ := call.Fun.(*ast.SelectorExpr)
	recv := mutexReceiver(sel.X)
	if recv == "" {
		return MuUnknown, ""
	}
	unlockMethod := unlockMethodFor(lockMethod)

	funcBody := EnclosingFuncBody(parentMap, lockCall)
	if funcBody == nil {
		return MuUnknown, recv
	}

	cover := r.coveringBlocks(lockCall, parentMap)

	coveringDefer := false
	delegated := false

	ast.Inspect(funcBody, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.DeferStmt:
			if deferTargetsUnlock(stmt, recv, unlockMethod) && covers(cover, stmt, parentMap) {
				coveringDefer = true
			}
		case *ast.CallExpr:
			if passesReceiver(stmt, recv) {
				delegated = true
			}
		}
		return true
	})

	if coveringDefer {
		return MuDeferUnlock, recv
	}
	if delegated {
		return MuDelegated, recv
	}
	if r.hasSafeExplicitUnlock(lockCall, funcBody, parentMap, recv, unlockMethod) {
		return MuExplicitUnlock, recv
	}
	return MuNoUnlock, recv
}

// hasSafeExplicitUnlock reports whether an Unlock exists in the same block
// after the Lock with no early return between them — the brief critical
// section idiom.
func (r *MutexRule) hasSafeExplicitUnlock(lockCall ast.Node, funcBody *ast.BlockStmt, parentMap map[ast.Node]ast.Node, recv, unlockMethod string) bool {
	lockContainer, lockIdx := containingContainerAndIndex(lockCall, parentMap)
	if lockContainer == nil {
		return false
	}

	foundUnlock := false
	start := lockIdx + 1

	ast.Inspect(funcBody, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !unlockTargets(call, recv, unlockMethod) {
			return true
		}
		uc, ui := containingContainerAndIndex(call, parentMap)
		if uc != lockContainer {
			return true
		}
		if ui <= lockIdx {
			return true
		}
		for i := start; i < ui; i++ {
			if returnsUnsafe(containerStmtAt(lockContainer, i), recv, unlockMethod, parentMap) {
				return true // keep scanning; other unlocks may be safe
			}
		}
		foundUnlock = true
		return false
	})
	return foundUnlock
}

// returnsUnsafe reports whether a statement contains a return that is NOT
// itself preceded by an unlock of the receiver in the return's own block.
func returnsUnsafe(stmt ast.Stmt, recv, unlockMethod string, parentMap map[ast.Node]ast.Node) bool {
	unsafe := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		rs, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		container, idx := containingContainerAndIndex(rs, parentMap)
		if container == nil {
			unsafe = true
			return false
		}
		for i := 0; i < idx; i++ {
			if isUnlockStmt(containerStmtAt(container, i), recv, unlockMethod) {
				return true // this return is guarded
			}
		}
		if unlockBefore(rs, parentMap, recv, unlockMethod) {
			return true
		}
		unsafe = true
		return false
	})
	return unsafe
}

// isUnlockStmt reports whether stmt is a bare `mu.Unlock()` expression
// statement targeting the receiver.
func isUnlockStmt(stmt ast.Stmt, recv, unlockMethod string) bool {
	es, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	return unlockTargets(call, recv, unlockMethod)
}

// unlockBefore reports whether an unlock of the receiver dominates the node:
// the node sits AFTER an unlock in its own enclosing block, or after an
// unlock in any enclosing block.
func unlockBefore(n ast.Node, parentMap map[ast.Node]ast.Node, recv, unlockMethod string) bool {
	container, idx := containingContainerAndIndex(n, parentMap)
	if unlockPrecedesInContainer(container, idx, recv, unlockMethod) {
		return true
	}
	stmtInContainer := enclosingStmtOfContainer(container, parentMap)
	for stmtInContainer != nil {
		inner := stmtInContainer
		parent := parentMap[inner]
		for parent != nil {
			if b, ok := parent.(*ast.BlockStmt); ok {
				if unlockPrecedesInContainer(b, outerIdxOf(b, inner), recv, unlockMethod) {
					return true
				}
				cur := b
				for {
					if enc := enclosingStmtOfContainer(cur, parentMap); enc != nil {
						stmtInContainer = enc
						break
					}
					break
				}
				break
			}
			if s, isStmt := parent.(ast.Stmt); isStmt {
				stmtInContainer = s
				break
			}
			parent = parentMap[parent]
		}
	}
	return false
}

// unlockPrecedesInContainer reports whether an unlock of the receiver appears
// in the container before index idx.
func unlockPrecedesInContainer(c ast.Node, idx int, recv, unlockMethod string) bool {
	if c == nil {
		return false
	}
	for i := 0; i < idx; i++ {
		if isUnlockStmt(containerStmtAt(c, i), recv, unlockMethod) {
			return true
		}
	}
	return false
}

// enclosingStmtOfContainer returns the statement that directly contains the
// container node.
func enclosingStmtOfContainer(block ast.Node, parentMap map[ast.Node]ast.Node) ast.Stmt {
	parent := parentMap[block]
	for parent != nil {
		if s, ok := parent.(ast.Stmt); ok {
			return s
		}
		parent = parentMap[parent]
	}
	return nil
}

// outerIdxOf returns the index of stmt within its directly enclosing block.
func outerIdxOf(block *ast.BlockStmt, stmt ast.Stmt) int {
	if block == nil || stmt == nil {
		return -1
	}
	for i, s := range block.List {
		if s == stmt {
			return i
		}
	}
	return -1
}

// containingContainerAndIndex returns the raw container node (BlockStmt /
// CaseClause / CommClause) and the index of the node's containing statement.
func containingContainerAndIndex(node ast.Node, parentMap map[ast.Node]ast.Node) (ast.Node, int) {
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			switch p := parentMap[stmt].(type) {
			case *ast.BlockStmt:
				return p, StmtIndexInBlock(p, stmt)
			case *ast.CaseClause:
				return p, rClauseIndexOf(p, stmt)
			case *ast.CommClause:
				return p, rClauseIndexOf2(p, stmt)
			}
		}
		n = parentMap[n]
	}
	return nil, -1
}

// containerStmtAt returns the i-th statement of a container node.
func containerStmtAt(c ast.Node, i int) ast.Stmt {
	switch cc := c.(type) {
	case *ast.BlockStmt:
		if i >= 0 && i < len(cc.List) {
			return cc.List[i]
		}
	case *ast.CaseClause:
		if i >= 0 && i < len(cc.Body) {
			return cc.Body[i]
		}
	case *ast.CommClause:
		if i >= 0 && i < len(cc.Body) {
			return cc.Body[i]
		}
	}
	return nil
}

func rClauseIndexOf(c *ast.CaseClause, stmt ast.Stmt) int {
	for i, s := range c.Body {
		if s == stmt {
			return i
		}
	}
	return -1
}

func rClauseIndexOf2(c *ast.CommClause, stmt ast.Stmt) int {
	for i, s := range c.Body {
		if s == stmt {
			return i
		}
	}
	return -1
}

// deferTargetsUnlock reports whether the defer is `defer mu.Unlock()` /
// `defer mu.RUnlock()` for the receiver, or a deferred closure that does so.
func deferTargetsUnlock(d *ast.DeferStmt, recv, unlockMethod string) bool {
	call := d.Call
	if unlockTargets(call, recv, unlockMethod) {
		return true
	}
	if fn, ok := call.Fun.(*ast.FuncLit); ok {
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if inner, ok := n.(*ast.CallExpr); ok && unlockTargets(inner, recv, unlockMethod) {
				found = true
				return false
			}
			return true
		})
		return found
	}
	return false
}

// unlockTargets reports whether the call is `mu.Unlock()` / `mu.RUnlock()`
// matching the receiver.
func unlockTargets(call *ast.CallExpr, recvString, method string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	return mutexReceiver(sel.X) == recvString
}

// passesReceiver reports whether the call passes the receiver (or a selector
// ending in it) as an argument to another function.
func passesReceiver(call *ast.CallExpr, recvString string) bool {
	if unlockTargets(call, recvString, "Unlock") || unlockTargets(call, recvString, "RUnlock") {
		return false
	}
	for _, a := range call.Args {
		if id, ok := a.(*ast.Ident); ok && id.Name == recvString {
			return true
		}
		if selx, ok := a.(*ast.SelectorExpr); ok && mutexReceiver(selx) == recvString {
			return true
		}
	}
	return false
}

// coveringBlocks returns (block, after-index) positions in which a deferred
// unlock would cover the site.
func (r *MutexRule) coveringBlocks(node ast.Node, parentMap map[ast.Node]ast.Node) []BlockPos {
	var out []BlockPos
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			if block, ok := parentMap[stmt].(*ast.BlockStmt); ok {
				out = append(out, BlockPos{Block: block, After: StmtIndexInBlock(block, stmt) + 1})
				n = block
				continue
			}
			n = parentMap[stmt]
			continue
		}
		n = parentMap[n]
	}
	return out
}

// covers reports whether the statement sits at a position at-or-after an
// index in one of the covering blocks.
func covers(cover []BlockPos, stmt ast.Stmt, parentMap map[ast.Node]ast.Node) bool {
	block, ok := parentMap[stmt].(*ast.BlockStmt)
	if !ok {
		return false
	}
	idx := StmtIndexInBlock(block, stmt)
	for _, c := range cover {
		if c.Block == block && idx >= c.After {
			return true
		}
	}
	return false
}

// EmitMarkdown implements Rule.
func (r *MutexRule) EmitMarkdown(w io.Writer, sites []Site) {
	counts := CountByClass(sites)

	fmt.Fprintln(w, "# Mutex Lock/Unlock Coverage Audit Report")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "**Total Lock/RLock/TryLock call sites:** %d\n\n", len(sites))

	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	classOrder := []MutexClass{
		MuDeferUnlock, MuDelegated, MuExplicitUnlock, MuNoUnlock, MuUnknown,
	}
	for _, c := range classOrder {
		if n, ok := counts[c]; ok {
			fmt.Fprintf(w, "- **%s**: %d\n", c, n)
		}
	}
	fmt.Fprintln(w)

	total := len(sites)
	bad := counts[MuNoUnlock] + counts[MuExplicitUnlock] + counts[MuUnknown]
	if total > 0 {
		pctBad := float64(bad) / float64(total) * 100
		fmt.Fprintf(w, "**Potentially unbound locks (deadlock risk): %d / %d (%.1f%%)**\n\n", bad, total, pctBad)
	}

	fmt.Fprintln(w, "## Sites Requiring Attention")
	fmt.Fprintln(w)
	skip := map[MutexClass]bool{MuDeferUnlock: true, MuDelegated: true}
	for _, s := range sites {
		if skip[s.Classification] {
			continue
		}
		fmt.Fprintf(w, "### `%s` — line %d (%s)\n", s.File, s.Line, s.Pattern)
		fmt.Fprintf(w, "- **Classification:** %s\n", s.Classification)
		fmt.Fprintf(w, "- **Enclosing function:** `%s`\n", s.FuncName)
		if s.VarName != "" {
			fmt.Fprintf(w, "- **Mutex receiver:** `%s`\n", s.VarName)
		}
		fmt.Fprintln(w, "- **Code:**")
		fmt.Fprintln(w, "  ```go")
		for _, line := range s.Code {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w, "  ```")
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "## mu-defer-unlock Sites (correct)")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d sites are covered by a deferred unlock.\n", counts[MuDeferUnlock])
}

// GateChecks implements Rule. mu-no-unlock / mu-unknown at zero;
// mu-explicit-unlock ratchets to measured baseline.
func (r *MutexRule) GateChecks(sites []Site, max map[string]int) []GateCheck {
	counts := CountByClass(sites)
	checks := []GateCheck{
		{MuNoUnlock, counts[MuNoUnlock], max[MuNoUnlock]},
		{MuUnknown, counts[MuUnknown], max[MuUnknown]},
		{MuExplicitUnlock, counts[MuExplicitUnlock], max[MuExplicitUnlock]},
	}
	return ResolveRatchet(checks)
}
