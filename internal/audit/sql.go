// SQL parameterization rule — the ported audit-sql classifier.
//
// The substrate stores raw user input, unpredictable semantic embeddings,
// and generated theories. A query string assembled with fmt.Sprintf or
// string concatenation instead of `?` parameters risks two failures under
// load: SQL injection (user content flows into the SQL grammar) and
// formatting panics (a malformed Sprintf verb at query time takes down the
// daemon's write path).
//
// Every database query method — Query / Exec / QueryRow and their *Context
// / Tracked variants — gets its SQL-string argument (arg 0 for the plain
// forms, arg 1 for the *Context forms) classified:
//
//	sql-literal          — SQL arg is a static string literal: CORRECT —
//	                       ? parameters by construction
//	sql-fmt-safe         — fmt.Sprintf / + used but every interpolated piece
//	                       is a literal or a schema-controlled identifier
//	                       (ALTER/DROP/PRAGMA DDL with allowlisted names):
//	                       CORRECT with allowlist discipline
//	sql-fmt-placeholder  — Sprintf/Join builds only the `?` placeholder list
//	                       for a variadic IN (...) clause; all values still
//	                       flow through bound args: CORRECT
//	sql-const            — SQL arg is an identifier (query var/const) whose
//	                       definition resolves to a literal: CORRECT
//	sql-built            — SQL arg constructed where a NON-literal value is
//	                       interpolated into the SQL grammar: FATAL
//	sql-unknown          — pattern did not fit (needs review)
//
// The cross-file provenance registry (package-level `var X = <composite>`
// migration tables like SafeMigrations, declared in schema.go, consumed in
// memory.go) is built in Prepare — the port of audit-sql's phase 1/phase 2
// split — so per-file classification resolves it regardless of walk order.
//
// Gate: sql-built at zero (default), sql-unknown / sql-fmt-safe / sql-const
// ratcheted to their measured baseline.
package audit

import (
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"strings"
)

// SQLClass is the SQL vocabulary.
type SQLClass = string

const (
	SQLLiteral        SQLClass = "sql-literal"
	SQLFmtSafe        SQLClass = "sql-fmt-safe"
	SQLFmtPlaceholder SQLClass = "sql-fmt-placeholder"
	SQLConst          SQLClass = "sql-const"
	SQLBuilt          SQLClass = "sql-built"
	SQLUnknown        SQLClass = "sql-unknown"
)

// SQLRule inventories query call sites and classifies their SQL-argument
// construction.
type SQLRule struct {
	// pkgVars is the phase-1 registry: package dir -> var name -> true for
	// package-level `var X = <composite literal>` declarations (migration
	// and registry tables like SafeMigrations). Built in Prepare so
	// cross-file provenance resolves regardless of walk order.
	pkgVars  map[string]map[string]bool
	fileDirs map[*ast.File]string
}

// NewSQLRule constructs the SQL parameterization rule.
func NewSQLRule() Rule { return &SQLRule{} }

// Name implements Rule.
func (r *SQLRule) Name() string { return "sql" }

// Prepare implements Rule. Phase 1 of audit-sql: builds the package-literal
// registry from every shared file's AST, and maps each file node to its
// directory so classification can resolve cross-file provenance.
func (r *SQLRule) Prepare(files []*File) {
	r.pkgVars = map[string]map[string]bool{}
	r.fileDirs = map[*ast.File]string{}
	for _, f := range files {
		r.fileDirs[f.AST] = f.Dir
		dir := f.Dir
		m, ok := r.pkgVars[dir]
		if !ok {
			m = map[string]bool{}
			r.pkgVars[dir] = m
		}
		for _, decl := range f.AST.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if _, isLit := vs.Values[i].(*ast.CompositeLit); isLit {
							m[name.Name] = true
						}
					}
				}
			}
		}
	}
}

// AuditFile implements Rule.
func (r *SQLRule) AuditFile(f *File) []Site {
	parentMap := f.ParentMap
	return findSites(f, func(call *ast.CallExpr, _ ast.Node) bool {
		return IsDbMethodCall(call)
	}, func(call *ast.CallExpr, _ ast.Node) Site {
		pos := f.FSet.Position(call.Pos())
		site := Site{
			File:     f.Path,
			Line:     pos.Line,
			Column:   pos.Column,
			FuncName: EnclosingFuncNameOrClosure(parentMap, call),
			Pattern:  r.patternString(call),
			SQLArg:   r.sqlArgBrief(call),
		}
		site.Classification = r.classify(call, parentMap)
		site.Code = ContextLines(f.Src, pos.Line)
		return site
	})
}

// EmitMarkdown implements Rule.
func (r *SQLRule) EmitMarkdown(w io.Writer, sites []Site) {
	counts := CountByClass(sites)

	fmt.Fprintln(w, "# SQL Parameterization Audit Report")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "**Total DB query call sites:** %d\n\n", len(sites))

	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	classOrder := []SQLClass{
		SQLLiteral, SQLFmtSafe, SQLFmtPlaceholder, SQLConst, SQLBuilt, SQLUnknown,
	}
	for _, c := range classOrder {
		if n, ok := counts[c]; ok {
			fmt.Fprintf(w, "- **%s**: %d\n", c, n)
		}
	}
	fmt.Fprintln(w)

	total := len(sites)
	bad := counts[SQLBuilt] + counts[SQLUnknown]
	if total > 0 {
		pctBad := float64(bad) / float64(total) * 100
		fmt.Fprintf(w, "**Constructed-SQL risk sites: %d / %d (%.1f%%)**\n\n", bad, total, pctBad)
	}

	fmt.Fprintln(w, "## Sites Requiring Attention")
	fmt.Fprintln(w)
	skip := map[SQLClass]bool{
		SQLLiteral: true, SQLFmtSafe: true, SQLFmtPlaceholder: true, SQLConst: true,
	}
	for _, s := range sites {
		if skip[s.Classification] {
			continue
		}
		fmt.Fprintf(w, "### `%s` — line %d (%s)\n", s.File, s.Line, s.Pattern)
		fmt.Fprintf(w, "- **Classification:** %s\n", s.Classification)
		fmt.Fprintf(w, "- **Enclosing function:** `%s`\n", s.FuncName)
		fmt.Fprintf(w, "- **SQL argument:** `%s`\n", s.SQLArg)
		fmt.Fprintln(w, "- **Code:**")
		fmt.Fprintln(w, "  ```go")
		for _, line := range s.Code {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w, "  ```")
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "## sql-literal Sites (correct)")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d sites pass static literals with ? parameters.\n", counts[SQLLiteral])
}

// GateChecks implements Rule. sql-built defaults to zero; sql-unknown /
// sql-fmt-safe / sql-const ratchet to measured baseline.
func (r *SQLRule) GateChecks(sites []Site, max map[string]int) []GateCheck {
	counts := CountByClass(sites)
	checks := []GateCheck{
		{SQLBuilt, counts[SQLBuilt], max[SQLBuilt]},
		{SQLUnknown, counts[SQLUnknown], max[SQLUnknown]},
		{SQLFmtSafe, counts[SQLFmtSafe], max[SQLFmtSafe]},
		{SQLConst, counts[SQLConst], max[SQLConst]},
	}
	return ResolveRatchet(checks)
}

func (r *SQLRule) patternString(call *ast.CallExpr) string {
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

// sqlArg returns the SQL-string argument of the call.
func (r *SQLRule) sqlArg(call *ast.CallExpr) ast.Expr {
	sel, _ := call.Fun.(*ast.SelectorExpr)
	if CtxForms[sel.Sel.Name] {
		if len(call.Args) < 2 {
			return nil
		}
		return call.Args[1]
	}
	if len(call.Args) < 1 {
		return nil
	}
	return call.Args[0]
}

func (r *SQLRule) sqlArgBrief(call *ast.CallExpr) string {
	a := r.sqlArg(call)
	if a == nil {
		return "(no sql arg)"
	}
	return ExprBrief(a)
}

// classify determines the SQL-construction class for one site.
func (r *SQLRule) classify(call *ast.CallExpr, parentMap map[ast.Node]ast.Node) SQLClass {
	arg := r.sqlArg(call)
	if arg == nil {
		return SQLUnknown
	}
	switch a := arg.(type) {
	case *ast.BasicLit:
		if a.Kind == token.STRING {
			return SQLLiteral
		}
		return SQLUnknown
	case *ast.Ident:
		if resolved := r.resolveIdentToLiteral(a, parentMap); resolved != nil {
			return SQLConst
		}
		return SQLUnknown
	case *ast.CallExpr:
		return r.classifyConstructed(a, parentMap)
	case *ast.BinaryExpr:
		return r.classifyConstructed(a, parentMap)
	case *ast.SelectorExpr:
		return SQLConst
	}
	return SQLUnknown
}

// classifyConstructed handles fmt.Sprintf / string concatenation /
// strings.Join calls used as the SQL argument.
func (r *SQLRule) classifyConstructed(e ast.Expr, parentMap map[ast.Node]ast.Node) SQLClass {
	switch t := e.(type) {
	case *ast.CallExpr:
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sprintf" {
			return r.classifySprintf(t, parentMap)
		}
		if id, ok := t.Fun.(*ast.Ident); ok && id.Name == "Sprintf" {
			return r.classifySprintf(t, parentMap)
		}
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Join" {
			return r.classifyJoin(t, parentMap)
		}
		if r.isPlaceholderBuilderCall(t) {
			return SQLFmtPlaceholder
		}
		return SQLBuilt
	case *ast.BinaryExpr:
		if t.Op == token.ADD {
			return r.classifyConcat(t, parentMap)
		}
		return SQLBuilt
	}
	return SQLBuilt
}

// classifyConcat inspects a `+` concatenation used as the SQL argument. The
// only safe concat is one whose non-literal operands are identifiers —
// schema/table/column names.
func (r *SQLRule) classifyConcat(t *ast.BinaryExpr, parentMap map[ast.Node]ast.Node) SQLClass {
	allIdentifiers := true
	for _, part := range r.flattenConcat(t) {
		switch p := part.(type) {
		case *ast.BasicLit:
		case *ast.Ident:
		case *ast.SelectorExpr:
		case *ast.CallExpr:
			if sel, ok := p.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Join" {
				if len(p.Args) > 0 {
					if id, ok := p.Args[0].(*ast.Ident); ok && r.onlyPlaceholders(id, parentMap) {
						continue
					}
				}
			}
			allIdentifiers = false
		default:
			allIdentifiers = false
		}
	}
	if allIdentifiers {
		return SQLFmtSafe
	}
	return SQLBuilt
}

func (r *SQLRule) flattenConcat(t *ast.BinaryExpr) []ast.Expr {
	var out []ast.Expr
	var walk func(e ast.Expr)
	walk = func(e ast.Expr) {
		if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
			walk(b.X)
			walk(b.Y)
			return
		}
		out = append(out, e)
	}
	walk(t)
	return out
}

// classifySprintf inspects fmt.Sprintf(query, parts...): if every
// interpolated part is a static literal, an identifier that resolves to a
// literal, or a schema/table/column name, the site is safe; if a raw value
// expression is interpolated, it is the fatal class.
func (r *SQLRule) classifySprintf(call *ast.CallExpr, parentMap map[ast.Node]ast.Node) SQLClass {
	if len(call.Args) < 1 {
		return SQLBuilt
	}
	// The format string is sometimes split across two adjacent string
	// literals joined with `+` — common for SQL statements that exceed
	// 80 cols. The Go parser preserves this as a *ast.BinaryExpr of two
	// *ast.BasicLits (concatAllStatic can prove the parts are all string
	// literals, so the result is still a static format).
	formatValue, ok := stringLitConcat(call.Args[0])
	if !ok {
		return SQLBuilt
	}
	verbCount := sprintfVerbCount(formatValue)

	if len(call.Args)-1 != verbCount {
		return SQLBuilt
	}
	for _, a := range call.Args[1:] {
		if !r.isSafeInterpolation(a, parentMap) {
			return SQLBuilt
		}
	}
	return SQLFmtSafe
}

// stringLitConcat returns the concatenated string value of an expression
// that is either a single string *ast.BasicLit, or a flat *ast.BinaryExpr
// tree whose leaves are all string *ast.BasicLits joined by `+`. This is
// the shape Go's parser produces for `fmt.Sprintf("foo %s" + "bar %s",
// ...)` — long SQL statements that exceed the 80-col limit are typically
// written this way. Returns ("", false) for any other shape.
func stringLitConcat(e ast.Expr) (string, bool) {
	switch t := e.(type) {
	case *ast.BasicLit:
		if t.Kind != token.STRING {
			return "", false
		}
		return t.Value, true
	case *ast.BinaryExpr:
		if t.Op != token.ADD {
			return "", false
		}
		left, ok := stringLitConcat(t.X)
		if !ok {
			return "", false
		}
		right, ok := stringLitConcat(t.Y)
		if !ok {
			return "", false
		}
		return left + right, true
	}
	return "", false
}

// isSafeInterpolation reports whether an interpolated piece is provably not
// attacker-controlled: a literal, an identifier resolving to a literal, or a
// field access on a literal struct.
func (r *SQLRule) isSafeInterpolation(e ast.Expr, parentMap map[ast.Node]ast.Node) bool {
	switch t := e.(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		if r.resolveIdentToLiteral(t, parentMap) != nil {
			return true
		}
		return r.isAllowlistGuardedParam(t, parentMap) || r.isReadFromLiteralTableAtCallSites(t, parentMap)
	case *ast.IndexExpr:
		if _, ok := t.Index.(*ast.BasicLit); ok {
			return true
		}
		return false
	case *ast.SelectorExpr:
		return true
	case *ast.ParenExpr:
		return r.isSafeInterpolation(t.X, parentMap)
	case *ast.StarExpr:
		return r.isSafeInterpolation(t.X, parentMap)
	case *ast.CallExpr:
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Join" {
			if len(t.Args) > 0 {
				if id, ok := t.Args[0].(*ast.Ident); ok && r.onlyPlaceholders(id, parentMap) {
					return true
				}
			}
		}
		if id, ok := t.Fun.(*ast.Ident); ok && isTableNameResolver(id.Name) {
			return true
		}
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok && isTableNameResolver(sel.Sel.Name) {
			return true
		}
		return false
	}
	return false
}

// classifyJoin handles strings.Join(parts, sep) used as the SQL arg — the
// IN-clause placeholder builder.
func (r *SQLRule) classifyJoin(call *ast.CallExpr, parentMap map[ast.Node]ast.Node) SQLClass {
	if len(call.Args) < 1 {
		return SQLBuilt
	}
	id, ok := call.Args[0].(*ast.Ident)
	if !ok {
		return SQLBuilt
	}
	if r.onlyPlaceholders(id, parentMap) {
		return SQLFmtPlaceholder
	}
	return SQLBuilt
}

// onlyPlaceholders reports whether the identifier slice is appended ONLY the
// string literal "?" in the enclosing function — the IN(...) builder.
func (r *SQLRule) onlyPlaceholders(id *ast.Ident, parentMap map[ast.Node]ast.Node) bool {
	funcBody := EnclosingFuncBody(parentMap, id)
	if funcBody == nil {
		return false
	}
	declaredHere := false
	only := true
	ast.Inspect(funcBody, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				if lhsID, ok := lhs.(*ast.Ident); ok && lhsID.Name == id.Name {
					for _, rhs := range node.Rhs {
						if call, ok := rhs.(*ast.CallExpr); ok {
							switch fn := call.Fun.(type) {
							case *ast.Ident:
								if fn.Name == "make" {
									declaredHere = true
								} else if fn.Name == "append" {
									if len(call.Args) == 2 {
										if lit, ok := call.Args[1].(*ast.BasicLit); !ok || lit.Value != `"?"` {
											only = false
										}
									}
								}
							}
						}
					}
				}
			}
		case *ast.IndexExpr:
			if idxID, ok := node.X.(*ast.Ident); ok && idxID.Name == id.Name {
				if parent, ok := parentMap[node]; ok {
					if assign, ok := parent.(*ast.AssignStmt); ok {
						for _, rhs := range assign.Rhs {
							if lit, ok := rhs.(*ast.BasicLit); !ok || lit.Value != `"?"` {
								only = false
							}
						}
					}
				}
			}
		}
		return true
	})
	return declaredHere && only
}

// resolveIdentToLiteral resolves an identifier to its defining literal.
func (r *SQLRule) resolveIdentToLiteral(id *ast.Ident, parentMap map[ast.Node]ast.Node) *ast.BasicLit {
	funcBody := EnclosingFuncBody(parentMap, id)
	if funcBody == nil {
		return nil
	}
	allLiteral := true

	if r.isRangeVarOverLiteralSlice(id, funcBody) {
		return &ast.BasicLit{Kind: token.STRING, Value: `"<range element>"`}
	}

	assignCount := 0
	ast.Inspect(funcBody, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok {
			found := false
			for _, lhs := range assign.Lhs {
				if lhsID, ok := lhs.(*ast.Ident); ok && lhsID.Name == id.Name {
					found = true
					break
				}
			}
			if !found {
				return true
			}
			assignCount++
			for _, rhs := range assign.Rhs {
				if lit, ok := rhs.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				} else if bin, ok := rhs.(*ast.BinaryExpr); ok && bin.Op == token.ADD {
					if !r.concatAllStatic(bin) {
						allLiteral = false
					}
				} else if call, ok := rhs.(*ast.CallExpr); ok {
					if !isLiteralSafeCall(call) {
						allLiteral = false
					}
				} else {
					allLiteral = false
				}
			}
		}
		return true
	})
	if assignCount == 0 {
		return nil
	}
	if allLiteral {
		return &ast.BasicLit{Kind: token.STRING, Value: `"<literal-assigned>"`}
	}
	return nil
}

func (r *SQLRule) isRangeVarOverLiteralSlice(id *ast.Ident, funcBody *ast.BlockStmt) bool {
	found := false
	ast.Inspect(funcBody, func(n ast.Node) bool {
		rs, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		value := rs.Value
		if value == nil {
			return true
		}
		if vid, ok := value.(*ast.Ident); ok && vid.Name == id.Name {
			if isLiteralSliceOrArray(rs.X) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func (r *SQLRule) concatAllStatic(bin *ast.BinaryExpr) bool {
	for _, part := range r.flattenConcat(bin) {
		switch part.(type) {
		case *ast.BasicLit:
		case *ast.SelectorExpr:
		default:
			return false
		}
	}
	return true
}

// isAllowlistGuardedParam reports whether the function param is validated
// against a package-level allowlist map inside the function —
// `if !validTableNames[table] { return err }`.
func (r *SQLRule) isAllowlistGuardedParam(id *ast.Ident, parentMap map[ast.Node]ast.Node) bool {
	funcBody := EnclosingFuncBody(parentMap, id)
	if funcBody == nil {
		return false
	}
	guarded := false
	ast.Inspect(funcBody, func(n ast.Node) bool {
		idx, ok := n.(*ast.IndexExpr)
		if !ok {
			return true
		}
		if ix, ok := idx.Index.(*ast.Ident); ok && ix.Name == id.Name {
			if _, ok := idx.X.(*ast.Ident); ok {
				guarded = true
				return false
			}
		}
		return true
	})
	return guarded
}

// isReadFromLiteralTableAtCallSites reports whether a function param is
// passed only literal/registry values at every call site in the same file —
// the SafeMigrations discipline.
func (r *SQLRule) isReadFromLiteralTableAtCallSites(id *ast.Ident, parentMap map[ast.Node]ast.Node) bool {
	var funcName string
	paramIndex := -1
	n := ast.Node(id)
	for n != nil {
		if fd, ok := n.(*ast.FuncDecl); ok {
			funcName = fd.Name.Name
			if fd.Type != nil && fd.Type.Params != nil {
				idx := 0
				for _, f := range fd.Type.Params.List {
					for _, pn := range f.Names {
						if pn.Name == id.Name {
							paramIndex = idx
						}
						idx++
					}
				}
			}
			break
		}
		n = parentMap[n]
	}
	if funcName == "" || paramIndex < 0 {
		return false
	}
	fn := ast.Node(n)
	for fn != nil {
		if f, ok := fn.(*ast.File); ok {
			callSites := 0
			allLiteral := true
			ast.Inspect(f, func(cn ast.Node) bool {
				call, ok := cn.(*ast.CallExpr)
				if !ok {
					return true
				}
				var called string
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					called = sel.Sel.Name
				} else if ident, ok := call.Fun.(*ast.Ident); ok {
					called = ident.Name
				}
				if called != funcName {
					return true
				}
				callSites++
				if len(call.Args) <= paramIndex {
					allLiteral = false
					return true
				}
				arg := call.Args[paramIndex]
				if !r.isRegistryLiteralArg(arg, parentMap) {
					allLiteral = false
				}
				return true
			})
			return callSites > 0 && allLiteral
		}
		fn = parentMap[fn]
	}
	return false
}

func (r *SQLRule) isRegistryLiteralArg(e ast.Expr, parentMap map[ast.Node]ast.Node) bool {
	switch t := e.(type) {
	case *ast.BasicLit:
		return t.Kind == token.STRING
	case *ast.Ident:
		return r.resolveIdentToLiteral(t, parentMap) != nil || r.isPackageLiteralVar(t, parentMap)
	case *ast.IndexExpr:
		if _, ok := t.Index.(*ast.BasicLit); ok {
			if id, ok := t.X.(*ast.Ident); ok {
				return r.isRangeOverPackageLiteral(id, parentMap)
			}
		}
		return false
	case *ast.ParenExpr:
		return r.isRegistryLiteralArg(t.X, parentMap)
	case *ast.SelectorExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return r.isRangeOverPackageLiteral(id, parentMap)
		}
		return false
	}
	return false
}

// isPackageLiteralVar reports whether the ident names a package-level var
// initialized with a composite literal, resolved through the phase-1
// registry keyed by the *using file's* directory.
func (r *SQLRule) isPackageLiteralVar(id *ast.Ident, parentMap map[ast.Node]ast.Node) bool {
	fn := ast.Node(id)
	for fn != nil {
		if f, ok := fn.(*ast.File); ok {
			if dir, ok := r.fileDirs[f]; ok {
				if m, ok := r.pkgVars[dir]; ok && m[id.Name] {
					return true
				}
			}
			return false
		}
		fn = parentMap[fn]
	}
	return false
}

func (r *SQLRule) isRangeOverPackageLiteral(id *ast.Ident, parentMap map[ast.Node]ast.Node) bool {
	funcBody := EnclosingFuncBody(parentMap, id)
	if funcBody == nil {
		return false
	}
	found := false
	ast.Inspect(funcBody, func(n ast.Node) bool {
		rs, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		var rangeVar string
		if v, ok := rs.Value.(*ast.Ident); ok {
			rangeVar = v.Name
		}
		if rangeVar != id.Name {
			return true
		}
		if vid, ok := rs.X.(*ast.Ident); ok && r.isPackageLiteralVar(vid, parentMap) {
			found = true
			return false
		}
		return true
	})
	return found
}

// isPlaceholderBuilderCall detects well-known placeholder builders by name.
func (r *SQLRule) isPlaceholderBuilderCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name == "placeholderString" || fn.Name == "placeholders" ||
			strings.HasPrefix(fn.Name, "buildPlaceholder") || fn.Name == "inClause"
	}
	return false
}

// -------------------------------------------------------------------- shared helpers

// isTableNameResolver reports whether a function name is a known pure
// table-name resolver used to pick a schema identifier from a controlled
// enum. The result is safe for DDL positions because it can only ever
// produce one of a fixed set of literal table names.
func isTableNameResolver(name string) bool {
	switch name {
	case "ArtifactTable", "artifactTable", "resolveTable", "tableNameFor", "tableForType":
		return true
	}
	return false
}

// sprintfVerbCount counts the real `%` verbs in a format string, correctly
// skipping the literal `%%` escape (recall.go's `strftime('%%s','now')` is
// an escaped percent that must not count as a %s verb).
func sprintfVerbCount(format string) int {
	count := 0
	for i := 0; i < len(format); {
		if format[i] == '%' {
			if i+1 < len(format) && format[i+1] == '%' {
				i += 2
				continue
			}
			count++
			i += 2
			continue
		}
		i++
	}
	return count
}

// isLiteralSliceOrArray reports whether an expr is `[]string{...}` /
// `[...]string{...}` / `map[string]...{...}` — a composite literal.
func isLiteralSliceOrArray(e ast.Expr) bool {
	switch e.(type) {
	case *ast.CompositeLit:
		return true
	case *ast.Ident:
		return false
	}
	return false
}

// isLiteralSafeCall reports whether a call used as an interpolation target
// is a known pure function that returns a schema/table identifier or literal
// (safe for DDL-identifier positions).
func isLiteralSafeCall(call *ast.CallExpr) bool {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if sel.Sel.Name == "Sprintf" && len(call.Args) >= 1 {
			ok := true
			for _, a := range call.Args[1:] {
				if _, ok := a.(*ast.BasicLit); !ok {
					ok = false
				}
			}
			return ok
		}
		return isTableNameResolver(sel.Sel.Name)
	}
	if id, ok := call.Fun.(*ast.Ident); ok {
		return isTableNameResolver(id.Name)
	}
	return false
}
