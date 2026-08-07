// Context propagation rule — the ported audit-ctx classifier.
//
// Context-cancellation is the only mechanism a timed-out or aborted agent
// has to stop a heavy query (semantic search, cascade materialization,
// compaction sweeps) from grinding on after it is dead. SQLite is a single
// shared connection in MPM, so a query that ignores ctx keeps the connection
// (and the whole substrate) hostage until it finishes on its own.
//
// Each database query method is classified by its relationship to a context:
//
//	ctx-passed          — first argument is a context.Context value:
//	                    CORRECT
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
// Gate: fails on ctx-in-scope-missing at zero *and* on any newly-introduced
// ctx-plain-missing (threshold defaulted to the current baseline count so
// existing debt is exempt but regressions are not). ctx-void ratchets
// similarly.
package audit

import (
	"fmt"
	"go/ast"
	"io"
	"strings"
)

// CtxClass is the ctx vocabulary.
type CtxClass = string

const (
	CtxPassed       CtxClass = "ctx-passed"
	CtxInScopeDrop  CtxClass = "ctx-in-scope-missing" // ctx exists, ignored: FATAL
	CtxPlainMissing CtxClass = "ctx-plain-missing"    // no ctx anywhere in scope
	CtxVoid         CtxClass = "ctx-void"             // method deliberately takes none
	CtxNonDb        CtxClass = "non-db"               // not a database receiver
	CtxUnknown      CtxClass = "unknown"
)

// CtxRule inventories query/exec sites and their context-propagation class.
type CtxRule struct{}

// NewCtxRule constructs the context-propagation rule.
func NewCtxRule() Rule { return &CtxRule{} }

// Name implements Rule.
func (r *CtxRule) Name() string { return "ctx" }

// Prepare implements Rule (no cross-file state).
func (r *CtxRule) Prepare(files []*File) {}

// AuditFile implements Rule.
func (r *CtxRule) AuditFile(f *File) []Site {
	parentMap := f.ParentMap
	var sites []Site
	ast.Inspect(f.AST, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !IsDbMethodCall(call) {
			return true
		}
		pos := f.FSet.Position(call.Pos())
		funcName, scopeHasCtx := r.enclosingScope(parentMap, call)
		site := Site{
			File:     f.Path,
			Line:     pos.Line,
			Column:   pos.Column,
			FuncName: funcName,
			Pattern:  r.patternString(call),
			ArgZero:  r.argZero(call),
		}
		site.Classification = r.classify(call, scopeHasCtx)
		site.Code = ContextLines(f.Src, pos.Line)
		sites = append(sites, site)
		return true
	})
	return sites
}

// EmitMarkdown implements Rule.
func (r *CtxRule) EmitMarkdown(w io.Writer, sites []Site) {
	counts := CountByClass(sites)

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

// GateChecks implements Rule. ctx-in-scope-missing / unknown default to
// zero; ctx-plain-missing and ctx-void ratchet to their measured baselines.
func (r *CtxRule) GateChecks(sites []Site, max map[string]int) []GateCheck {
	counts := CountByClass(sites)
	checks := []GateCheck{
		{CtxInScopeDrop, counts[CtxInScopeDrop], max[CtxInScopeDrop]},
		{CtxPlainMissing, counts[CtxPlainMissing], max[CtxPlainMissing]},
		{CtxVoid, counts[CtxVoid], max[CtxVoid]},
		{CtxUnknown, counts[CtxUnknown], max[CtxUnknown]},
	}
	return ResolveRatchet(checks)
}

func (r *CtxRule) patternString(call *ast.CallExpr) string {
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
func (r *CtxRule) argZero(call *ast.CallExpr) string {
	if len(call.Args) == 0 {
		return "(no args)"
	}
	return ExprBrief(call.Args[0])
}

// enclosingScope reports the enclosing function name and whether the call
// site is inside a function that has a context.Context in scope (as a
// receiver field, parameter, or explicit ctx := assignment).
func (r *CtxRule) enclosingScope(parentMap map[ast.Node]ast.Node, node ast.Node) (string, bool) {
	n := node
	for n != nil {
		switch f := n.(type) {
		case *ast.FuncDecl:
			return f.Name.Name, r.scopeHasCtx(f)
		case *ast.FuncLit:
			return "(closure)", r.scopeHasCtxLit(f)
		}
		n = parentMap[n]
	}
	return "", false
}

func (r *CtxRule) scopeHasCtx(f *ast.FuncDecl) bool {
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

func (r *CtxRule) scopeHasCtxLit(fn *ast.FuncLit) bool {
	has := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		t, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
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
		return true
	})
	return has
}

// classify decides the ctx-in-scope class for the site.
func (r *CtxRule) classify(call *ast.CallExpr, scopeHasCtx bool) CtxClass {
	sel, _ := call.Fun.(*ast.SelectorExpr)

	// Demote non-db receivers (LLM client.Query etc.)
	if !IsKnownDbReceiver(sel.X) {
		return CtxNonDb
	}

	method := sel.Sel.Name
	if CtxForms[method] {
		// Context variants: verify first arg is a ctx-like value.
		if len(call.Args) > 0 && r.ctxLike(call.Args[0]) {
			return CtxPassed
		}
		// a *Context method whose first arg isn't ctx is almost always
		// wrong.
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
func (r *CtxRule) ctxLike(e ast.Expr) bool {
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
