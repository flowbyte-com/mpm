// Package audit consolidates the eight static analyzers that previously
// lived as standalone binaries (cmd/audit-scans, audit-closes, audit-tx,
// audit-ctx, audit-go, audit-mutex, audit-sql, audit-fd) into one shared
// engine. cmd/mpm-lint wraps this package: it parses each Go file once and
// runs every rule's classification logic concurrently over the same ASTs.
//
// Each rule inventories one class of call sites (rows.Close coverage, Scan
// error handling, transaction rollback, context propagation, goroutine
// lifecycle binding, mutex unlock pairing, SQL parameterization, file
// descriptor closing) and classifies each site with a rule-specific
// vocabulary. The --gate mode exits non-zero when a fatal class exceeds its
// threshold; thresholds default to the -1 ratchet convention (measure the
// current baseline) for review classes and 0 for zero-tolerance classes.
package audit

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// File is one Go source file parsed once and shared by every rule.
type File struct {
	Path string
	FSet *token.FileSet
	AST  *ast.File
	Src  []byte
	Dir  string
	// ParentMap links every node to its parent, built in one ast.Inspect
	// pass. All rules rely on it for order-sensitive classification.
	ParentMap map[ast.Node]ast.Node
}

// Site is one audited call site. The seven universal fields appear in every
// rule's report; rule-specific fields are omitted from JSON when empty.
type Site struct {
	File           string   `json:"file"`
	Line           int      `json:"line"`
	Column         int      `json:"column"`
	FuncName       string   `json:"func_name"`
	Pattern        string   `json:"pattern"`
	Classification string   `json:"classification"`
	Code           []string `json:"code"`
	// Rule-specific: rows/tx/file/mutex receiver variable.
	VarName string `json:"var_name,omitempty"`
	// Mutex rule: Lock / RLock / TryLock.
	LockMethod string `json:"lock_method,omitempty"`
	// Scans rule: the error variable the site's handling was classified on.
	ErrVar string `json:"err_var,omitempty"`
	// Go rule: the goroutine target and the binding reason.
	Target string `json:"target,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Ctx rule: rendered first argument.
	ArgZero string `json:"arg_zero,omitempty"`
	// SQL rule: rendered SQL argument.
	SQLArg string `json:"sql_arg,omitempty"`
}

// Rule audits parsed files for one class of sites.
type Rule interface {
	// Name is the rule identifier used by cmd/mpm-lint's --rule flag and
	// the JSON report keys (closes, scans, tx, ctx, go, mutex, sql, fd).
	Name() string
	// Prepare runs once after all files are collected but before any file
	// is audited. Used for cross-file registries (the SQL rule's
	// package-level literal-var table, which lives in one file and is
	// consumed in another).
	Prepare(files []*File)
	// AuditFile classifies the sites of one parsed file.
	AuditFile(f *File) []Site
	// EmitMarkdown renders the rule's report section.
	EmitMarkdown(w io.Writer, sites []Site)
	// GateChecks builds the gate's class-vs-threshold checks from the
	// sites and the max map (keyed by class name; -1 = ratchet to the
	// measured baseline).
	GateChecks(sites []Site, max map[string]int) []GateCheck
}

// GateCheck is one class-vs-threshold comparison in a gate run.
type GateCheck struct {
	Name      string
	Actual    int
	Threshold int
}

// Run collects and parses every .go file under roots (once), prepares each
// rule, then runs the rules concurrently over the shared files. Returns the
// sorted sites per rule name.
func Run(roots []string, includeTests bool, rules []Rule) map[string][]Site {
	files := collectFiles(roots, includeTests)
	for _, r := range rules {
		r.Prepare(files)
	}
	out := make(map[string][]Site, len(rules))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, r := range rules {
		wg.Add(1)
		go func(r Rule) {
			defer wg.Done()
			var sites []Site
			for _, f := range files {
				sites = append(sites, r.AuditFile(f)...)
			}
			SortSites(sites)
			mu.Lock()
			out[r.Name()] = sites
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	return out
}

// collectFiles walks the roots (skipping vendor, node_modules, and hidden
// dirs), parses each eligible .go file once, and returns the shared File
// set. Files that fail to parse are reported to stderr and skipped for all
// rules.
func collectFiles(roots []string, includeTests bool) []*File {
	var paths []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
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
			paths = append(paths, path)
			return nil
		})
	}
	var files []*File
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[mpm-lint] read %s: %v\n", path, err)
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[mpm-lint] parse %s: %v\n", path, err)
			continue
		}
		parentMap := make(map[ast.Node]ast.Node)
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
		files = append(files, &File{
			Path:      path,
			FSet:      fset,
			AST:       f,
			Src:       src,
			Dir:       filepath.Dir(path),
			ParentMap: parentMap,
		})
	}
	return files
}

// SortSites orders sites by file, then line — the stable report order every
// standalone audit used.
func SortSites(sites []Site) {
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
}

// CountByClass tallies sites per classification.
func CountByClass(sites []Site) map[string]int {
	m := make(map[string]int)
	for _, s := range sites {
		m[s.Classification]++
	}
	return m
}

// EmitJSON writes the {total, sites} document every standalone audit wrote.
func EmitJSON(w io.Writer, sites []Site) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(struct {
		Total int    `json:"total"`
		Sites []Site `json:"sites"`
	}{Total: len(sites), Sites: sites})
}

// ResolveRatchet applies the -1 convention: a negative threshold ratchets
// to the measured count, grandfathering the existing baseline while failing
// any NEW site.
func ResolveRatchet(checks []GateCheck) []GateCheck {
	for i := range checks {
		if checks[i].Threshold < 0 {
			checks[i].Threshold = checks[i].Actual
		}
	}
	return checks
}

// RunGate evaluates the checks and prints the verdict to stderr (so the
// report on stdout stays machine-parseable). Returns 0 (pass) or 1 (fail).
func RunGate(rule string, checks []GateCheck) int {
	var failed []GateCheck
	for _, c := range checks {
		if c.Actual > c.Threshold {
			failed = append(failed, c)
		}
	}

	fmt.Fprintf(os.Stderr, "[mpm-lint --gate] %s thresholds:\n", rule)
	for _, c := range checks {
		status := "ok"
		if c.Actual > c.Threshold {
			status = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "  %-22s actual=%d  threshold=%d  %s\n", c.Name, c.Actual, c.Threshold, status)
	}

	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "[mpm-lint --gate] FAIL: %s\n", rule)
		for _, c := range failed {
			fmt.Fprintf(os.Stderr, "  - %s: %d > %d\n", c.Name, c.Actual, c.Threshold)
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, "[mpm-lint --gate] PASS")
	return 0
}

// -------------------------------------------------------------------- shared helpers

// findSites iterates every CallExpr in the file, keeps those matching, and
// collects a Site per hit. The parent node (the statement/expression that
// directly contains the call) is passed to both predicates so rules can
// classify by their call's context (assignment, return, discard, ...).
func findSites(f *File, match func(call *ast.CallExpr, parent ast.Node) bool, build func(call *ast.CallExpr, parent ast.Node) Site) []Site {
	var sites []Site
	ast.Inspect(f.AST, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		parent := f.ParentMap[call]
		if !match(call, parent) {
			return true
		}
		sites = append(sites, build(call, parent))
		return true
	})
	return sites
}

// ContextLines returns up to 7 lines of source around `line` (3 before, the
// line itself marked with '> ', 3 after). The standard capture used by every
// rule except audit-fd.
func ContextLines(src []byte, line int) []string {
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

// FDContextLines is audit-fd's capture variant (6 lines, '▶' marker).
func FDContextLines(src []byte, line int) []string {
	lines := strings.Split(string(src), "\n")
	if line < 0 || line-1 >= len(lines) {
		return nil
	}
	from := line - 4
	if from < 0 {
		from = 0
	}
	to := line + 2
	if to > len(lines) {
		to = len(lines)
	}
	out := []string{}
	for i := from; i < to; i++ {
		marker := " "
		if i == line-1 {
			marker = "▶"
		}
		out = append(out, fmt.Sprintf("%3d: %s %s", i+1, marker, lines[i]))
	}
	return out
}

// EnclosingFuncName returns the name of the innermost enclosing FuncDecl
// (closures do not stop the climb and do not get their own name). Used by
// closes, scans, tx, and go.
func EnclosingFuncName(parentMap map[ast.Node]ast.Node, node ast.Node) string {
	n := node
	for n != nil {
		if fd, ok := n.(*ast.FuncDecl); ok {
			return fd.Name.Name
		}
		n = parentMap[n]
	}
	return ""
}

// EnclosingFuncNameOrClosure is the variant that reports "(closure)" when
// the site sits inside a function literal. Used by ctx, mutex, and sql.
func EnclosingFuncNameOrClosure(parentMap map[ast.Node]ast.Node, node ast.Node) string {
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

// EnclosingFuncNameSkipClosure is audit-fd's variant: a site inside a
// closure is attributed to the enclosing named function.
func EnclosingFuncNameSkipClosure(parentMap map[ast.Node]ast.Node, node ast.Node) string {
	n := node
	for n != nil {
		if fd, ok := n.(*ast.FuncDecl); ok {
			return fd.Name.Name
		}
		n = parentMap[n]
	}
	return ""
}

// EnclosingFuncBody returns the block of the innermost enclosing function
// (decl or literal).
func EnclosingFuncBody(parentMap map[ast.Node]ast.Node, node ast.Node) *ast.BlockStmt {
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

// ReferencesIdent reports whether an expression mentions the named
// identifier anywhere inside it.
func ReferencesIdent(expr ast.Node, name string) bool {
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

// ErrVarName returns the name of the error variable in a
// `x, err := site()` assignment.
func ErrVarName(parent ast.Node) string {
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

// GuardCondSite reports whether an if statement guards the site: its
// condition references the site var or the site's error var. Bodies of such
// ifs are covering positions for a deferred close/rollback.
func GuardCondSite(ifs *ast.IfStmt, varName, errVar string) bool {
	if varName != "" && ReferencesIdent(ifs.Cond, varName) {
		return true
	}
	if errVar != "" && ReferencesIdent(ifs.Cond, errVar) {
		return true
	}
	return false
}

// BlockPos is a (block, after-index) covering position.
type BlockPos struct {
	Block *ast.BlockStmt
	After int
}

// StmtIndexInBlock returns the index of the statement in its block.
func StmtIndexInBlock(block *ast.BlockStmt, stmt ast.Stmt) int {
	for i, s := range block.List {
		if s == stmt {
			return i
		}
	}
	return -1
}

// Covers reports whether the statement sits at a position at-or-after a
// boundary in one of the covering blocks.
func Covers(cover []BlockPos, stmt ast.Stmt, parentMap map[ast.Node]ast.Node) bool {
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

// InCoverPosition reports whether the node climbs to a Statement in a
// covering position for the site.
func InCoverPosition(node ast.Node, cover []BlockPos, parentMap map[ast.Node]ast.Node) bool {
	n := node
	for n != nil {
		if stmt, ok := n.(ast.Stmt); ok {
			return Covers(cover, stmt, parentMap)
		}
		n = parentMap[n]
	}
	return false
}

// ExprBrief renders an expression for report output.
func ExprBrief(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.CallExpr:
		return "(call)"
	case *ast.BasicLit:
		return t.Value
	case *ast.SelectorExpr:
		return ReceiverIdent(t.X) + "." + t.Sel.Name
	}
	return "?"
}

// ReceiverIdent renders a receiver qualifier to a canonical string
// (e.g. `dm.SQLDB(...)` chain).
func ReceiverIdent(x ast.Expr) string {
	switch t := x.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return ReceiverIdent(t.X) + "." + t.Sel.Name
	case *ast.CallExpr:
		return "(...) "
	case *ast.StarExpr:
		return "*" + ReceiverIdent(t.X)
	}
	return ""
}

// -------------------------------------------------------------------- db vocabulary

// The context-propagation (ctx) and SQL parameterization (sql) rules share
// the same database method surface and receiver discipline. It lives here
// once, in the shared package, instead of being duplicated across two rules.

// dbMethods is the database mutation/query surface. The plain forms
// (Query/Exec/QueryRow/Tracked) never take a ctx — they are classifiable.
// The *Context forms DO take ctx as first arg.
var dbMethods = map[string]bool{
	"Query": true, "QueryContext": true, "QueryRow": true, "QueryRowContext": true,
	"Exec": true, "ExecContext": true,
	"QueryTracked": true, "QueryRowTracked": true, "ExecTracked": true,
}

// CtxForms are the database methods that take a context.Context as their
// first argument.
var CtxForms = map[string]bool{
	"QueryContext": true, "QueryRowContext": true, "ExecContext": true,
}

// dbReceiverIdents is the set of well-known local names that refer to the
// SQLite connection in this codebase.
var dbReceiverIdents = map[string]bool{
	"db": true, "DB": true, "tx": true, "sqlDB": true, "wDB": true,
	"testDB": true, "dm": true, "s": true, "sc": true,
}

// IsDbMethodCall reports whether the call is a database query method on a
// known-database receiver (mirrors the receiver discipline so LLM client
// .Query etc. are not scanned).
func IsDbMethodCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	name := sel.Sel.Name
	if !dbMethods[name] {
		return false
	}
	return IsKnownDbReceiver(sel.X)
}

// IsKnownDbReceiver reports whether the receiver is a database: a bare
// ident from dbReceiverIdents, a method call returning the DB
// (dm.SQLDB()), or a chain ending in one of them.
func IsKnownDbReceiver(x ast.Expr) bool {
	switch t := x.(type) {
	case *ast.Ident:
		return dbReceiverIdents[t.Name]
	case *ast.SelectorExpr:
		inner := ReceiverIdent(t.X)
		if t.Sel.Name == "DB" && (inner == "dm" || inner == "sc") {
			return true
		}
		return IsKnownDbReceiver(t.X)
	case *ast.CallExpr:
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "SQLDB", "DB":
				return true
			}
		}
		// fall through: e.g. (*x).SQLDB()
		if len(t.Args) > 0 {
			return false
		}
	}
	return false
}
