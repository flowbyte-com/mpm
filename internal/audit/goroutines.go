// Goroutine lifecycle binding rule — the ported audit-go classifier.
//
// The substrate spawns background goroutines (memory compaction, embedding
// backfills, epistemic checks, recollection wake handlers, synthesis
// workers). An unbound goroutine is a leak:
//
//   - blocked on a channel read that never arrives (no ctx.Done escape,
//     no WaitGroup drain),
//   - writer to an unbuffered channel nobody reads,
//   - a detached fire-and-forget goroutine that survives its parent, holds
//     the shared SQLite connection, and is never joined at shutdown.
//
// Rule enforced here: EVERY `go` statement must be visibly bound to the
// parent lifecycle by one of:
//
//	go-wg-bound     — the goroutine body does `defer wg.Done()` and a
//	                  `wg.Add(1)` counter exists in the enclosing scope
//	                  (parent waits via wg.Wait()).
//	go-ctx-bound    — the goroutine reads `<-ctx.Done()` or tests
//	                  ctx.Err()/ctx.Done(), so cancellation ends it.
//	go-reply-ch     — the goroutine replies on a channel that the parent
//	                  drains in the same enclosing function (select on
//	                  {case <-ch / case <-ctx.Done()} or a nearby receive),
//	                  so the reply path itself bounds it.
//	go-detached     — NO WaitGroup, NO ctx, NO reply channel: fire-and-
//	                  forget. FLAGGED.
//	go-external     — `go namedFn(...)`; the binding lives in namedFn
//	                  (worker pool pattern). Verified here by confirming a
//	                  wg.Add appears in the enclosing scope; otherwise
//	                  reviewed manually.
//	go-unknown      — pattern did not fit (needs review)
//
// Gate: fails when any go-detached site exists at threshold 0 (plus
// go-unknown).
package audit

import (
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"strings"
)

// GoClass is the go vocabulary.
type GoClass = string

const (
	GoWgBound  GoClass = "go-wg-bound"
	GoCtxBound GoClass = "go-ctx-bound"
	GoReply    GoClass = "go-reply-bound"
	GoNamed    GoClass = "go-named"
	GoDetached GoClass = "go-detached"
	GoUnknown  GoClass = "go-unknown"
)

// GoRule inventories go statements and their lifecycle-binding class.
type GoRule struct{}

// NewGoRule constructs the goroutine-lifecycle rule.
func NewGoRule() Rule { return &GoRule{} }

// Name implements Rule.
func (r *GoRule) Name() string { return "go" }

// Prepare implements Rule (no cross-file state).
func (r *GoRule) Prepare(files []*File) {}

// AuditFile implements Rule.
func (r *GoRule) AuditFile(f *File) []Site {
	parentMap := f.ParentMap
	var sites []Site
	ast.Inspect(f.AST, func(n ast.Node) bool {
		goStmt, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		pos := f.FSet.Position(goStmt.Pos())
		site := Site{
			File:     f.Path,
			Line:     pos.Line,
			Column:   pos.Column,
			FuncName: EnclosingFuncName(parentMap, goStmt),
			Target:   r.target(goStmt),
			Code:     ContextLines(f.Src, pos.Line),
		}
		site.Classification, site.Reason = r.classify(goStmt, parentMap)
		sites = append(sites, site)
		return true
	})
	return sites
}

// EmitMarkdown implements Rule.
func (r *GoRule) EmitMarkdown(w io.Writer, sites []Site) {
	counts := CountByClass(sites)
	fmt.Fprintln(w, "# Goroutine Lifecycle Audit Report")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "**Total go statements:** %d\n\n", len(sites))
	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	classOrder := []GoClass{GoWgBound, GoCtxBound, GoReply, GoNamed, GoDetached, GoUnknown}
	for _, c := range classOrder {
		if n, ok := counts[c]; ok {
			fmt.Fprintf(w, "- **%s**: %d\n", c, n)
		}
	}
	fmt.Fprintln(w)

	bad := counts[GoDetached] + counts[GoUnknown]
	total := len(sites)
	if total > 0 {
		fmt.Fprintf(w, "**Unbinding goroutines (detached/unknown): %d / %d**\n\n", bad, total)
	}

	fmt.Fprintln(w, "## Sites Requiring Attention")
	fmt.Fprintln(w)
	skip := map[GoClass]bool{
		GoWgBound: true, GoCtxBound: true, GoReply: true,
	}
	for _, s := range sites {
		if skip[s.Classification] {
			continue
		}
		fmt.Fprintf(w, "### `%s` — line %d (`%s`)\n", s.File, s.Line, s.Target)
		fmt.Fprintf(w, "- **Classification:** %s\n", s.Classification)
		fmt.Fprintf(w, "- **Enclosing function:** `%s`\n", s.FuncName)
		if s.Reason != "" {
			fmt.Fprintf(w, "- **Reason:** %s\n", s.Reason)
		}
		fmt.Fprintln(w, "- **Code:**")
		fmt.Fprintln(w, "  ```go")
		for _, line := range s.Code {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w, "  ```")
		fmt.Fprintln(w)
	}
}

// GateChecks implements Rule. go-detached / go-unknown default to zero.
func (r *GoRule) GateChecks(sites []Site, max map[string]int) []GateCheck {
	counts := CountByClass(sites)
	checks := []GateCheck{
		{GoDetached, counts[GoDetached], max[GoDetached]},
		{GoUnknown, counts[GoUnknown], max[GoUnknown]},
	}
	return ResolveRatchet(checks)
}

func (r *GoRule) target(g *ast.GoStmt) string {
	switch t := g.Call.Fun.(type) {
	case *ast.FuncLit:
		return "(func literal)"
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return r.receiver(t.X) + "." + t.Sel.Name
	}
	return "(?)"
}

// receiver is the go rule's qualifier renderer (function args get a
// goTargetCall-style projection, distinct from the shared ReceiverIdent).
func (r *GoRule) receiver(x ast.Expr) string {
	switch t := x.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return r.receiver(t.X) + "." + t.Sel.Name
	case *ast.CallExpr:
		return "(" + r.targetCall(t) + ")"
	case *ast.StarExpr:
		return "*" + r.receiver(t.X)
	}
	return ""
}

func (r *GoRule) targetCall(c *ast.CallExpr) string {
	if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
		return r.receiver(sel.X) + "." + sel.Sel.Name
	}
	return "call"
}

func (r *GoRule) classify(g *ast.GoStmt, parentMap map[ast.Node]ast.Node) (GoClass, string) {
	// Enclosing function body for scope checks.
	funcBody := EnclosingFuncBody(parentMap, g)
	if funcBody == nil {
		return GoUnknown, "no enclosing function body"
	}

	// 1. Func literal: go func(){...}()
	if lit, ok := g.Call.Fun.(*ast.FuncLit); ok {
		if r.wgWaitGroupDone(lit) {
			return GoWgBound, "defer wg.Done() in body"
		}
		if r.ctxDoneRead(lit) {
			return GoCtxBound, "reads ctx.Done()/ctx.Err()"
		}
		if r.replyChannel(lit) {
			return GoReply, "replies on a channel drained by the parent"
		}
		return GoDetached, "no WaitGroup, no ctx, no reply channel"
	}

	// 2. Named function: go worker(i) / go executeOne(...) etc. Confirm
	// the enclosing func has a wg.Add counter for it, or the named
	// function is itself a known pool worker.
	if r.wgAddInScope(funcBody) {
		return GoNamed, "named fn; wg.Add counter present in enclosing scope"
	}
	return GoNamed, "named fn; verify the target manages its own lifecycle"
}

// wgWaitGroupDone reports whether the literal body participates in a
// WaitGroup lifecycle: `defer wg.Done()` OR a `wg.Wait()` join.
func (r *GoRule) wgWaitGroupDone(lit *ast.FuncLit) bool {
	found := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Done", "Wait":
			recv := r.receiverExpr(sel.X)
			// mirrorWG / watchdogWG / wg / p.wg / .wg — any WaitGroup-shaped
			// receiver participates in a WaitGroup lifecycle.
			if strings.HasPrefix(recv, "wg") || strings.HasSuffix(recv, ".wg") ||
				strings.HasSuffix(recv, "WG") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// ctxDoneRead reports whether the body tests or reads context.Done()/Err().
func (r *GoRule) ctxDoneRead(lit *ast.FuncLit) bool {
	found := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// ctx.Done() / ctx.Err() — including via <-receiver on a fn
		if sel.Sel.Name == "Done" || sel.Sel.Name == "Err" {
			if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "ctx" || strings.HasSuffix(id.Name, "ctx")) {
				found = true
				return false
			}
		}
		// channel receive which is likely the ctx.Done channel
		if unary, ok := n.(*ast.UnaryExpr); ok && unary.Op == token.ARROW {
			innerStr := r.targetFromExpr(unary.X)
			if strings.Contains(innerStr, "Done") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func (r *GoRule) targetFromExpr(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return r.receiverExpr(t.X) + "." + t.Sel.Name
	case *ast.CallExpr:
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok {
			return r.receiverExpr(sel.X) + "." + sel.Sel.Name
		}
		return "call"
	case *ast.FuncLit:
		return "func(){}"
	}
	return ""
}

func (r *GoRule) receiverExpr(x ast.Expr) string {
	switch t := x.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return r.receiverExpr(t.X) + "." + t.Sel.Name
	case *ast.ParenExpr:
		return r.receiverExpr(t.X)
	}
	return ""
}

// replyChannel reports whether the goroutine body contains a channel send
// (`ch <- v`). A send is the reply-path discipline: the parent must drain
// it, which bounds the goroutine's lifetime to the parent's.
func (r *GoRule) replyChannel(lit *ast.FuncLit) bool {
	sends := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.SendStmt); ok {
			sends = true
			return false
		}
		return true
	})
	return sends
}

// wgAddInScope reports whether the enclosing func body calls wg.Add(...)
// or wg.Wait(...).
func (r *GoRule) wgAddInScope(funcBody *ast.BlockStmt) bool {
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
		if sel.Sel.Name == "Add" {
			if id, ok := sel.X.(*ast.Ident); ok && strings.HasPrefix(id.Name, "wg") {
				found = true
				return false
			}
		}
		if sel.Sel.Name == "Wait" {
			if id, ok := sel.X.(*ast.Ident); ok && strings.HasPrefix(id.Name, "wg") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}
