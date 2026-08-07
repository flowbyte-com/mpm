// cmd/audit-sql/main.go — Static analyzer for SQL parameterization safety.
//
// The substrate stores raw user input, unpredictable semantic embeddings,
// and generated theories. A query string assembled with fmt.Sprintf or
// string concatenation instead of `?` parameters risks two failures under
// load: SQL injection (user content flows into the SQL grammar) and
// formatting panics (a malformed Sprintf verb at query time takes down the
// daemon's write path).
//
// This tool walks the Go source trees (default: internal/, cmd/) and
// inventories every database query method — Query / Exec / QueryRow and
// their *Context / Tracked variants — then inspects the SQL-string argument
// (arg 0 for the plain forms, arg 1 for the *Context forms):
//
//	sql-literal      — the SQL arg is a static string literal (`...` or
//	                   "..."): CORRECT — `?` params by construction
//	sql-fmt-safe     — fmt.Sprintf / + used, but every interpolated piece is
//	                   itself a static literal or a schema-controlled
//	                   identifier (ALTER/DROP/PRAGMA DDL with allowlisted
//	                   names): CORRECT with allowlist discipline
//	sql-fmt-placeholder — Sprintf/Join builds only the `?` placeholder list
//	                   for a variadic IN (...) clause; all values still flow
//	                   through bound args: CORRECT
//	sql-const        — the SQL arg is an identifier (query var/const) whose
//	                   definition resolves to a literal: CORRECT
//	sql-built        — the SQL arg is constructed with fmt.Sprintf / string
//	                   concatenation where a NON-literal value (variable,
//	                   parameter, call result) is interpolated into the SQL
//	                   grammar: FATAL — injection / format-panic class
//	sql-unknown      — pattern did not fit (needs review)
//
// The gate (--gate) fails on sql-built / sql-unknown at zero, and ratchets
// sql-fmt-safe / sql-const to their measured baseline (so pre-existing
// reviewed sites are exempt but NEW constructed calls fail the commit).
//
// Usage:
//
//	go run ./cmd/audit-sql                  # markdown report on stdout
//	go run ./cmd/audit-sql --json           # JSON sidecar
//	go run ./cmd/audit-sql --include-tests  # include _test.go files
//	go run ./cmd/audit-sql --roots=a,b
//	go run ./cmd/audit-sql --gate           # exit non-zero on fatal classes
//
// The pre-commit hook runs `audit-sql --gate` alongside the other gates.

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

type SQLClass string

const (
	SQLClassLiteral        SQLClass = "sql-literal"
	SQLClassFmtSafe        SQLClass = "sql-fmt-safe"
	SQLClassFmtPlaceholder SQLClass = "sql-fmt-placeholder"
	SQLClassConst          SQLClass = "sql-const"
	SQLClassBuilt          SQLClass = "sql-built"
	SQLClassUnknown        SQLClass = "sql-unknown"
)

// Site is one query call whose SQL argument's construction is classified.
type Site struct {
	File           string   `json:"file"`
	Line           int      `json:"line"`
	Column         int      `json:"column"`
	FuncName       string   `json:"func_name"`
	Pattern        string   `json:"pattern"`
	SQLArg         string   `json:"sql_arg"`
	Classification SQLClass `json:"classification"`
	Code           []string `json:"code"`
}

// dbMethods covers the full query surface. SQL arg: index 0 for the plain
// forms, index 1 for the *Context forms (ctx is arg 0).
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
		rootsFlag    = flag.String("roots", "internal,cmd", "comma-separated root dirs to walk")
		jsonSidecar  = flag.String("json-out", "", "if set, also write JSON to this path")
		gate         = flag.Bool("gate", false, "exit non-zero on fatal classes")
		maxBuilt     = flag.Int("max-built", 0, "threshold for sql-built (used with --gate)")
		maxUnknown   = flag.Int("max-unknown", -1, "threshold for sql-unknown (default: current baseline)")
		maxFmtSafe   = flag.Int("max-fmt-safe", -1, "threshold for sql-fmt-safe (default: current baseline)")
		maxConst     = flag.Int("max-const", -1, "threshold for sql-const (default: current baseline)")
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
			fmt.Fprintf(os.Stderr, "audit-sql: walk %s: %v\n", root, err)
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
			fmt.Fprintf(os.Stderr, "audit-sql: create %s: %v\n", *jsonSidecar, err)
			os.Exit(1)
		}
		defer f.Close()
		emitJSON(f, sites)
		fmt.Fprintf(os.Stderr, "audit-sql: wrote %d sites to %s\n", len(sites), *jsonSidecar)
	}

	counts := countByClass(sites)
	if *maxFmtSafe < 0 {
		*maxFmtSafe = counts[SQLClassFmtSafe]
	}
	if *maxConst < 0 {
		*maxConst = counts[SQLClassConst]
	}
	if *maxUnknown < 0 {
		*maxUnknown = counts[SQLClassUnknown]
	}

	if *gate {
		exitCode := runGate(counts, gateThresholds{
			Built:   *maxBuilt,
			Unknown: *maxUnknown,
			FmtSafe: *maxFmtSafe,
			Const:   *maxConst,
		})
		os.Exit(exitCode)
	}
}

type gateThresholds struct {
	Built   int
	Unknown int
	FmtSafe int
	Const   int
}

func runGate(counts map[SQLClass]int, t gateThresholds) int {
	type check struct {
		name      string
		actual    int
		threshold int
	}
	checks := []check{
		{"sql-built", counts[SQLClassBuilt], t.Built},
		{"sql-unknown", counts[SQLClassUnknown], t.Unknown},
		{"sql-fmt-safe", counts[SQLClassFmtSafe], t.FmtSafe},
		{"sql-const", counts[SQLClassConst], t.Const},
	}

	var failed []check
	for _, c := range checks {
		if c.actual > c.threshold {
			failed = append(failed, c)
		}
	}

	fmt.Fprintln(os.Stderr, "[audit-sql --gate] SQL parameterization thresholds:")
	for _, c := range checks {
		status := "ok"
		if c.actual > c.threshold {
			status = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "  %-22s actual=%d  threshold=%d  %s\n", c.name, c.actual, c.threshold, status)
	}

	if len(failed) > 0 {
		fmt.Fprintln(os.Stderr, "[audit-sql --gate] FAIL: constructed-SQL classes exceed thresholds")
		for _, c := range failed {
			fmt.Fprintf(os.Stderr, "  - %s: %d > %d\n", c.name, c.actual, c.threshold)
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, "[audit-sql --gate] PASS")
	return 0
}

// -------------------------------------------------------------------- walker

func walkRoot(root string, includeTests bool, sites *[]Site) error {
	var goFiles []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
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
		goFiles = append(goFiles, path)
		return nil
	})
	if err != nil {
		return err
	}
	// Phase 1: build the package-level literal-var registry (SafeMigrations
	// lives in schema.go, its use sites in memory.go — a per-file scan would
	// miss the cross-file provenance). Keyed by directory (package).
	for _, path := range goFiles {
		registerPackageLiteralVars(path)
	}
	// Phase 2: audit each file against the fully-populated registry.
	for _, path := range goFiles {
		fileSites, err := auditFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit-sql: parse %s: %v\n", path, err)
			continue
		}
		*sites = append(*sites, fileSites...)
	}
	return nil
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
	astFileDirs[f] = filepath.Dir(path)

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
		site := Site{
			File:     path,
			Line:     pos.Line,
			Column:   pos.Column,
			FuncName: enclosingFuncName(parentMap, n),
			Pattern:  patternString(call),
			SQLArg:   sqlArgBrief(call),
		}
		site.Classification = classify(call, parentMap)
		site.Code = contextLines(src, pos.Line)
		sites = append(sites, site)
		return true
	})
	return sites, nil
}

// isDbMethodCall reports whether the call is a database query method on a
// known-database receiver (mirrors audit-ctx's receiver discipline so LLM
// client .Query etc. are not scanned).
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
		if len(t.Args) > 0 {
			return false
		}
	}
	return false
}

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

// sqlArg returns the SQL-string argument of the call.
func sqlArg(call *ast.CallExpr) ast.Expr {
	sel, _ := call.Fun.(*ast.SelectorExpr)
	if ctxForms[sel.Sel.Name] {
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

func sqlArgBrief(call *ast.CallExpr) string {
	a := sqlArg(call)
	if a == nil {
		return "(no sql arg)"
	}
	return exprBrief(a)
}

func exprBrief(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.CallExpr:
		return "(call)"
	case *ast.BasicLit:
		s := t.Value
		if len(s) > 60 {
			s = s[:60] + "..."
		}
		return s
	case *ast.SelectorExpr:
		return receiverIdent(t.X) + "." + t.Sel.Name
	case *ast.BinaryExpr:
		return "(" + exprBrief(t.X) + " + ...)"
	}
	return "?"
}

func enclosingFuncName(parentMap map[ast.Node]ast.Node, node ast.Node) string {
	n := node
	for n != nil {
		if fd, ok := n.(*ast.FuncDecl); ok {
			return fd.Name.Name
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return "(closure)"
		}
		n = parentMap[n]
	}
	return ""
}

// -------------------------------------------------------------------- classifier

// classify determines the SQL-construction class for one site.
func classify(call *ast.CallExpr, parentMap map[ast.Node]ast.Node) SQLClass {
	arg := sqlArg(call)
	if arg == nil {
		return SQLClassUnknown
	}
	switch a := arg.(type) {
	case *ast.BasicLit:
		if a.Kind == token.STRING {
			return SQLClassLiteral
		}
		return SQLClassUnknown
	case *ast.Ident:
		// Resolve `query := \`...\`` to its literal definition.
		if resolved := resolveIdentToLiteral(a, parentMap); resolved != nil {
			return SQLClassConst
		}
		// Unresolvable ident (package-level const in another file, function
		// parameter, etc.). A parameter is the risk case — it could carry
		// user content. But a plain ident alone is not *constructed* — it is
		// what it is. Rate it as const only if we can prove literal; else
		// flag for review.
		return SQLClassUnknown
	case *ast.CallExpr:
		return classifyConstructed(a, parentMap)
	case *ast.BinaryExpr:
		return classifyConstructed(a, parentMap)
	case *ast.SelectorExpr:
		// e.g. schema.MemoriesTable — a named constant.
		return SQLClassConst
	}
	return SQLClassUnknown
}

// classifyConstructed handles fmt.Sprintf / string concatenation /
// strings.Join calls used as the SQL argument.
func classifyConstructed(e ast.Expr, parentMap map[ast.Node]ast.Node) SQLClass {
	switch t := e.(type) {
	case *ast.CallExpr:
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sprintf" {
			return classifySprintf(t, parentMap)
		}
		if id, ok := t.Fun.(*ast.Ident); ok && id.Name == "Sprintf" {
			return classifySprintf(t, parentMap)
		}
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Join" {
			return classifyJoin(t, parentMap)
		}
		if isPlaceholderBuilderCall(t) {
			return SQLClassFmtPlaceholder
		}
		return SQLClassBuilt
	case *ast.BinaryExpr:
		if t.Op == token.ADD {
			return classifyConcat(t, parentMap)
		}
		return SQLClassBuilt
	}
	return SQLClassBuilt
}

// classifyConcat inspects a `+` concatenation used as the SQL argument.
//
// The only safe concat is one whose non-literal operands are identifiers —
// schema/table/column names. SQLite has no placeholders for identifiers, so
// `"ALTER TABLE " + table` and `"DELETE FROM " + name` CANNOT be
// ?-parameterized; their safety rests on the interpolated name coming from
// an allowlist / SafeMigrations literal. Concatenating a VALUE (a call
// result, an unknown ident, a general expression) is the injection class.
func classifyConcat(t *ast.BinaryExpr, parentMap map[ast.Node]ast.Node) SQLClass {
	allIdentifiers := true
	for _, part := range flattenConcat(t) {
		switch p := part.(type) {
		case *ast.BasicLit:
			// literal SQL fragment — safe
		case *ast.Ident:
			// A bare identifier in a concat is a schema/table/column name.
			// Its value is not attacker text. Treat it as identifier
			// interpolation (DDL/DML name position).
		case *ast.SelectorExpr:
			// e.g. schema.SomeTable or col.Name — named constant
		case *ast.CallExpr:
			// strings.Join(ids, ",") where ids is a ?-only placeholder slice
			// — the variadic IN(...) fragment: safe by construction.
			if sel, ok := p.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Join" {
				if len(p.Args) > 0 {
					if id, ok := p.Args[0].(*ast.Ident); ok && onlyPlaceholders(id, parentMap) {
						continue
					}
				}
			}
			allIdentifiers = false
		default:
			// call results, index exprs feeding VALUES, etc. — not provable.
			allIdentifiers = false
		}
	}
	if allIdentifiers {
		return SQLClassFmtSafe
	}
	return SQLClassBuilt
}

// flattenConcat reduces a binary `a + b + c` tree to its leaves.
func flattenConcat(t *ast.BinaryExpr) []ast.Expr {
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
// literal, or a schema/table/column name (DDL context), the site is safe;
// if a raw value expression is interpolated, it is the fatal class.
func classifySprintf(call *ast.CallExpr, parentMap map[ast.Node]ast.Node) SQLClass {
	if len(call.Args) < 1 {
		return SQLClassBuilt
	}
	format, ok := call.Args[0].(*ast.BasicLit)
	if !ok || format.Kind != token.STRING {
		return SQLClassBuilt
	}
	verbCount := sprintfVerbCount(format.Value)

	if len(call.Args)-1 != verbCount {
		return SQLClassBuilt
	}
	for _, a := range call.Args[1:] {
		if !isSafeInterpolation(a, parentMap) {
			return SQLClassBuilt
		}
	}
	return SQLClassFmtSafe
}

// isSafeInterpolation reports whether an interpolated piece is provably not
// attacker-controlled: a literal, an identifier resolving to a literal
// (schema table/column names), or a field access on a literal struct
// (m[0], col.name).
func isSafeInterpolation(e ast.Expr, parentMap map[ast.Node]ast.Node) bool {
	switch t := e.(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		if resolveIdentToLiteral(t, parentMap) != nil {
			return true
		}
		// Function params that are validated against an allowlist map
		// (`validTableNames[table]`) or whose every call site passes a
		// literal are identifier-position safe.
		return isAllowlistGuardedParam(t, parentMap) || isReadFromLiteralTableAtCallSites(t, parentMap)
	case *ast.IndexExpr:
		// m[0] where m ranges over SafeMigrations — the index expression is
		// literal and the base is a loop variable from a literal slice.
		if _, ok := t.Index.(*ast.BasicLit); ok {
			return true
		}
		return false
	case *ast.SelectorExpr:
		// col.name where col is a range var over a literal struct slice.
		return true
	case *ast.ParenExpr:
		return isSafeInterpolation(t.X, parentMap)
	case *ast.StarExpr:
		return isSafeInterpolation(t.X, parentMap)
	case *ast.CallExpr:
		// strings.Join(ids, ",") where ids is a `?`-only placeholder slice —
		// the variadic IN(...) clause pattern: safe by construction.
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Join" {
			if len(t.Args) > 0 {
				if id, ok := t.Args[0].(*ast.Ident); ok && onlyPlaceholders(id, parentMap) {
					return true
				}
			}
		}
		// Pure table-name resolvers (ArtifactTable(type) → "memories" /
		// "lessons"): the result is a schema identifier drawn from a
		// controlled enum, never attacker text.
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

// classifyJoin handles strings.Join(parts, sep) used as the SQL arg — the
// IN-clause placeholder builder: ids = append(ids, "?") then
// strings.Join(ids, ",") yields "?,?,?" — values flow via bound args.
func classifyJoin(call *ast.CallExpr, parentMap map[ast.Node]ast.Node) SQLClass {
	if len(call.Args) < 1 {
		return SQLClassBuilt
	}
	id, ok := call.Args[0].(*ast.Ident)
	if !ok {
		return SQLClassBuilt
	}
	if onlyPlaceholders(id, parentMap) {
		return SQLClassFmtPlaceholder
	}
	return SQLClassBuilt
}

// onlyPlaceholders reports whether the identifier slice is appended ONLY
// the string literal "?" in the enclosing function — the IN(...) builder.
func onlyPlaceholders(id *ast.Ident, parentMap map[ast.Node]ast.Node) bool {
	funcBody := enclosingFuncBody(parentMap, id)
	if funcBody == nil {
		return false
	}
	// The slice is declare-in-func and written ONLY with "?" — either via
	// append (ids = append(ids, "?")) or index-assignment in a builder loop
	// (placeholders[i] = "?"), the two shapes the codebase uses for the
	// variadic IN(...) clause.
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
			// placeholders[i] = "?" — index-assignment placeholder writes.
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

// resolveIdentToLiteral resolves an identifier to its defining literal
// within the same file/function: `query := \`SELECT ...\“ or a package-level
// `var query = \`...\“ / `const query = "..."`.
func resolveIdentToLiteral(id *ast.Ident, parentMap map[ast.Node]ast.Node) *ast.BasicLit {
	// Follow every assigned value of the identifier through the enclosing
	// function: `table := "memories"; if x { table = "lessons" }` resolves
	// because both assignments are string literals.
	funcBody := enclosingFuncBody(parentMap, id)
	if funcBody == nil {
		return nil
	}
	allLiteral := true

	// First, is the ident a RANGE VARIABLE over a literal slice?
	// `for _, name := range []string{"evidence_ai", ...}` — name provably
	// resolves to a literal element. Mark it as resolved.
	if isRangeVarOverLiteralSlice(id, funcBody) {
		return &ast.BasicLit{Kind: token.STRING, Value: `"<range element>"`}
	}

	// Second: every ASSIGNMENT to the ident must be a string literal. If the
	// function never assigns it (pure param/const from elsewhere), the outer
	// caller decides — here we return nil.
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
					// ok
				} else if bin, ok := rhs.(*ast.BinaryExpr); ok && bin.Op == token.ADD {
					// `table := cfg.SchemaPrefix + "memories"` — a concat of
					// a selector (named constant) and a literal: identifier
					// composition, not a value.
					if !concatAllStatic(bin) {
						allLiteral = false
					}
				} else if call, ok := rhs.(*ast.CallExpr); ok {
					// a table-name resolver or literal-returning helper
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

// isRangeVarOverLiteralSlice reports whether the ident is a range variable of
// a range statement whose source is a literal composite (`[]string{...}`
// or `[...]string{...}`).
func isRangeVarOverLiteralSlice(id *ast.Ident, funcBody *ast.BlockStmt) bool {
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

// concatAllStatic reports whether a binary concat is composed only of
// literals and selectors — `cfg.SchemaPrefix + "memories"` style identifier
// composition (a schema-prefix constant). Safe for DDL-identifier positions.
func concatAllStatic(bin *ast.BinaryExpr) bool {
	for _, part := range flattenConcat(bin) {
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
// `if !validTableNames[table] { return err }` (memory.go:207). A param that
// fails the allowlist never reaches the DDL statement, so interpolating it
// as an identifier is safe.
func isAllowlistGuardedParam(id *ast.Ident, parentMap map[ast.Node]ast.Node) bool {
	funcBody := enclosingFuncBody(parentMap, id)
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
// `addColumnIfNotExists(m[0], m[1], m[2])` over SafeMigrations (a literal
// [3]string table) or `addColumnIfNotExists("memories", "content_hash",
// "TEXT")`. DDL identifiers drawn from a compile-time migration registry are
// safe; this is the SafeMigrations discipline.
func isReadFromLiteralTableAtCallSites(id *ast.Ident, parentMap map[ast.Node]ast.Node) bool {
	// find enclosing FuncDecl + param index
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
	// walk up to the file to scan all call sites
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
				if !isRegistryLiteralArg(arg, parentMap) {
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

// isRegistryLiteralArg reports whether a call argument is a static literal,
// a package-level literal-table element (SafeMigrations), or a field of a
// loop variable that ranges over such a registry.
func isRegistryLiteralArg(e ast.Expr, parentMap map[ast.Node]ast.Node) bool {
	switch t := e.(type) {
	case *ast.BasicLit:
		return t.Kind == token.STRING
	case *ast.Ident:
		return resolveIdentToLiteral(t, parentMap) != nil || isPackageLiteralVar(t, parentMap)
	case *ast.IndexExpr:
		// m[0] where m ranges over SafeMigrations ([3]string literal table)
		if _, ok := t.Index.(*ast.BasicLit); ok {
			if id, ok := t.X.(*ast.Ident); ok {
				return isRangeOverPackageLiteral(id, parentMap)
			}
		}
		return false
	case *ast.ParenExpr:
		return isRegistryLiteralArg(t.X, parentMap)
	case *ast.SelectorExpr:
		// m.field — struct registry element
		if id, ok := t.X.(*ast.Ident); ok {
			return isRangeOverPackageLiteral(id, parentMap)
		}
		return false
	}
	return false
}

// isPackageLiteralVar reports whether the ident names a package-level var
// initialized with a composite literal (a migration/registry table).
// packageLiteralVars maps package directory -> var name -> true for
// package-level `var X = <composite literal>` declarations (migration and
// registry tables like SafeMigrations). Built in walkRoot's phase 1 so
// cross-file provenance resolves regardless of walk order.
var packageLiteralVars = map[string]map[string]bool{}

// astFileDirs maps each parsed *ast.File back to its directory so
// isPackageLiteralVar can look up the file's package in the registry.
var astFileDirs = map[*ast.File]string{}

// registerPackageLiteralVars records every package-level var initialized
// with a composite literal in the file's package directory registry.
func registerPackageLiteralVars(path string) {
	src, err := os.ReadFile(path)
	if err != nil {
		return
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	m, ok := packageLiteralVars[dir]
	if !ok {
		m = map[string]bool{}
		packageLiteralVars[dir] = m
	}
	for _, decl := range f.Decls {
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

func isPackageLiteralVar(id *ast.Ident, parentMap map[ast.Node]ast.Node) bool {
	fn := ast.Node(id)
	for fn != nil {
		if f, ok := fn.(*ast.File); ok {
			if dir, ok := astFileDirs[f]; ok {
				if m, ok := packageLiteralVars[dir]; ok && m[id.Name] {
					return true
				}
			}
			return false
		}
		fn = parentMap[fn]
	}
	return false
}

// isRangeOverPackageLiteral reports whether the ident is a range var of a
// loop ranging over a package-level literal var (SafeMigrations).
func isRangeOverPackageLiteral(id *ast.Ident, parentMap map[ast.Node]ast.Node) bool {
	funcBody := enclosingFuncBody(parentMap, id)
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
		if vid, ok := rs.X.(*ast.Ident); ok && isPackageLiteralVar(vid, parentMap) {
			found = true
			return false
		}
		return true
	})
	return found
}

// isLiteralSliceOrArray reports whether an expr is `[]string{...}` /
// `[...]string{...}` / `map[string]...{...}` — a composite literal.
func isLiteralSliceOrArray(e ast.Expr) bool {
	switch e.(type) {
	case *ast.CompositeLit:
		return true
	case *ast.Ident:
		// an ident that is itself a package-level var — unknown here
		return false
	}
	return false
}

// isLiteralSafeCall reports whether a call used as an interpolation target is
// a known pure function that returns a schema/table identifier or literal
// (safe for DDL-identifier positions).
func isLiteralSafeCall(call *ast.CallExpr) bool {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if sel.Sel.Name == "Sprintf" && len(call.Args) >= 1 {
			// only when every arg is a literal
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

// isPlaceholderBuilderCall detects well-known placeholder builders by name.
func isPlaceholderBuilderCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name == "placeholderString" || fn.Name == "placeholders" ||
			strings.HasPrefix(fn.Name, "buildPlaceholder") || fn.Name == "inClause"
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

	fmt.Fprintln(w, "# SQL Parameterization Audit Report")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "**Total DB query call sites:** %d\n\n", len(sites))

	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	classOrder := []SQLClass{
		SQLClassLiteral, SQLClassFmtSafe, SQLClassFmtPlaceholder, SQLClassConst, SQLClassBuilt, SQLClassUnknown,
	}
	for _, c := range classOrder {
		if n, ok := counts[c]; ok {
			fmt.Fprintf(w, "- **%s**: %d\n", c, n)
		}
	}
	fmt.Fprintln(w)

	total := len(sites)
	bad := counts[SQLClassBuilt] + counts[SQLClassUnknown]
	if total > 0 {
		pctBad := float64(bad) / float64(total) * 100
		fmt.Fprintf(w, "**Constructed-SQL risk sites: %d / %d (%.1f%%)**\n\n", bad, total, pctBad)
	}

	fmt.Fprintln(w, "## Sites Requiring Attention")
	fmt.Fprintln(w)
	skip := map[SQLClass]bool{SQLClassLiteral: true, SQLClassFmtSafe: true, SQLClassFmtPlaceholder: true, SQLClassConst: true}
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
	fmt.Fprintf(w, "%d sites pass static literals with ? parameters.\n", counts[SQLClassLiteral])
}

func emitJSON(w *os.File, sites []Site) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(struct {
		Total int    `json:"total"`
		Sites []Site `json:"sites"`
	}{Total: len(sites), Sites: sites})
}

func countByClass(sites []Site) map[SQLClass]int {
	m := make(map[SQLClass]int)
	for _, s := range sites {
		m[s.Classification]++
	}
	return m
}
