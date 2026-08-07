// cmd/audit-scans/main.go — Static analyzer for silent Scan error handling.
//
// Walks Go source trees (default: internal/, cmd/) and inventories every
// `.Scan(...)` call against database/sql Rows or Row. Classifies each site
// by how it handles the returned error:
//
//	hard-fail          — error returned to caller (correct)
//	ErrNoRows-tolerated — `if err == sql.ErrNoRows { return nil }` (legitimate
//	                     empty-state branch; v explicitly carved this out
//	                     2026-08-06 — the schema-DDL-flip migration that
//	                     motivated this audit MUST distinguish empty rows
//	                     from scan panics)
//	tolerated          — explicitly documented expected-empty case
//	silent-continue    — `if err != nil { continue }` (row silently dropped)
//	logged-swallow     — error logged but not returned
//	fake-hard-fail     — `if err != nil { return nil }` (catches ALL errors
//	                     and returns nil — the worst class, looks correct
//	                     but is functionally identical to silent-continue)
//	no-check           — error assigned to `_` or discarded as ExprStmt
//	helper-silent      — Scan wrapped in a helper that swallows internally
//	goroutine-silent   — Scan inside a goroutine with no error propagation
//	unknown            — pattern didn't fit any classifier (needs review)
//
// Usage:
//
//	go run ./cmd/audit-scans                   # markdown report on stdout
//	go run ./cmd/audit-scans --json            # JSON sidecar (for diffing)
//	go run ./cmd/audit-scans --include-tests   # include _test.go files
//	go run ./cmd/audit-scans --roots=internal/core,cmd/mpm
//
// Why this exists (per "Truth once. Views everywhere."):
//
//	Before the table-rebuild migration (internal/core/migration_timestamps.go
//	deferral → schema DDL flip), we need to know exactly how many silent
//	failure sites exist so the post-migration test suite becomes a
//	data-integrity tripwire instead of a rubber stamp. The closed-under-
//	observation principle (v, 2026-08-06): a substrate that cannot see its
//	own errors confidently hallucinates a reality where no memories exist.
//
// Scope note: scans only the .go files in the named roots. The cmd/ subdirs
// for mpm-mcp, mpm-scheduler, mpm-critic don't reach the DB directly
// (they shell out to `mpm call`); the cmd/mpm directory is the front-door
// that does reach the DB but usually via MCP tool wrappers, so most of the
// scan sites will be in internal/core/.

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

type Classification string

const (
	ClassHardFail           Classification = "hard-fail"
	ClassErrNoRowsTolerated Classification = "ErrNoRows-tolerated"
	ClassTolerated          Classification = "tolerated"
	ClassSilentContinue     Classification = "silent-continue"
	ClassLoggedSwallow      Classification = "logged-swallow"
	ClassFakeHardFail       Classification = "fake-hard-fail"
	ClassNoCheck            Classification = "no-check"
	ClassHelperSilent       Classification = "helper-silent"
	ClassGoroutineSilent    Classification = "goroutine-silent"
	ClassUnknown            Classification = "unknown"
)

// Site is one Scan() call site with its classification and surrounding context.
type Site struct {
	File           string   `json:"file"`
	Line           int      `json:"line"`
	Column         int      `json:"column"`
	FuncName       string   `json:"func_name"`
	Pattern        string   `json:"pattern"`
	Classification Classification `json:"classification"`
	ErrVar         string   `json:"err_var"`
	Code           []string `json:"code"`
}

// -------------------------------------------------------------------- main

func main() {
	var (
		jsonOut      = flag.Bool("json", false, "emit JSON sidecar instead of markdown")
		includeTests = flag.Bool("include-tests", false, "scan _test.go files too")
		rootsFlag    = flag.String("roots", "internal/core,cmd/mpm", "comma-separated root dirs to walk")
		jsonSidecar  = flag.String("json-out", "", "if set, also write JSON to this path")
		gate         = flag.Bool("gate", false, "exit non-zero if any fatal-class count exceeds its threshold (silent-continue / fake-hard-fail / no-check default to 0)")
		maxSilent    = flag.Int("max-silent-continue", 0, "threshold for silent-continue (used with --gate)")
		maxFakeFail  = flag.Int("max-fake-hard-fail", 0, "threshold for fake-hard-fail (used with --gate)")
		maxNoCheck   = flag.Int("max-no-check", 0, "threshold for no-check (used with --gate)")
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
			fmt.Fprintf(os.Stderr, "audit-scans: walk %s: %v\n", root, err)
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
			fmt.Fprintf(os.Stderr, "audit-scans: create %s: %v\n", *jsonSidecar, err)
			os.Exit(1)
		}
		defer f.Close()
		emitJSON(f, sites)
		fmt.Fprintf(os.Stderr, "audit-scans: wrote %d sites to %s\n", len(sites), *jsonSidecar)
	}

	if *gate {
		exitCode := runGate(sites, gateThresholds{
			SilentContinue: *maxSilent,
			FakeHardFail:   *maxFakeFail,
			NoCheck:        *maxNoCheck,
		})
		os.Exit(exitCode)
	}
}

// gateThresholds holds the maximum allowed count for each fatal classification.
// Defaults are 0 (zero-tolerance). The pre-commit hook uses these to gate
// commits: any fatal-class site above the threshold aborts the commit.
//
// logged-swallow / ErrNoRows-tolerated / tolerated / hard-fail / unknown
// are NOT gated — they pass by design. Only the three "fatal" classes
// above fail the gate, because they each represent a distinct silent-
// failure mode:
//
//	silent-continue — row dropped, no observation at all
//	fake-hard-fail  — return-nil-on-error looks correct but isn't
//	no-check        — error discarded at the call site
type gateThresholds struct {
	SilentContinue int
	FakeHardFail   int
	NoCheck        int
}

// runGate evaluates the gate thresholds against the scan sites and returns
// the appropriate exit code (0 = pass, 1 = fail). Output goes to stderr so
// the markdown/JSON report on stdout stays machine-parseable.
func runGate(sites []Site, t gateThresholds) int {
	counts := countByClass(sites)

	type check struct {
		name      string
		actual    int
		threshold int
	}
	checks := []check{
		{"silent-continue", counts[ClassSilentContinue], t.SilentContinue},
		{"fake-hard-fail", counts[ClassFakeHardFail], t.FakeHardFail},
		{"no-check", counts[ClassNoCheck], t.NoCheck},
	}

	var failed []check
	for _, c := range checks {
		if c.actual > c.threshold {
			failed = append(failed, c)
		}
	}

	fmt.Fprintln(os.Stderr, "[audit-scans --gate] fatal-class thresholds:")
	for _, c := range checks {
		status := "ok"
		if c.actual > c.threshold {
			status = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "  %-18s actual=%d  threshold=%d  %s\n", c.name, c.actual, c.threshold, status)
	}

	if len(failed) > 0 {
		fmt.Fprintln(os.Stderr, "[audit-scans --gate] FAIL: fatal-class thresholds exceeded")
		for _, c := range failed {
			fmt.Fprintf(os.Stderr, "  - %s: %d > %d\n", c.name, c.actual, c.threshold)
		}
		return 1
	}

	fmt.Fprintln(os.Stderr, "[audit-scans --gate] PASS")
	return 0
}

// -------------------------------------------------------------------- walker

func walkRoot(root string, includeTests bool, sites *[]Site) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip vendor and hidden dirs
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
			fmt.Fprintf(os.Stderr, "audit-scans: parse %s: %v\n", path, err)
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

	parentMap := make(map[ast.Node]ast.Node)
	var stack []ast.Node
	var sites []Site

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

		call, ok := n.(*ast.CallExpr)
		if !ok || !isScanCall(call) {
			return true
		}

		// Filter out non-database Scan calls (bufio.Scanner, fmt.Scan,
		// strings.Reader, etc.). Only audit Scan() calls against *sql.Rows
		// or *sql.Row, which is the closed-under-observation surface that
		// matters for the schema-DDL migration.
		if !isDatabaseScan(parentMap[n], n) {
			return true
		}

		pos := fset.Position(n.Pos())
		site := Site{
			File:     path,
			Line:     pos.Line,
			Column:   pos.Column,
			FuncName: enclosingFuncName(parentMap, n, f),
			Pattern:  patternString(parentMap[n], n),
			Code:     contextLines(src, pos.Line),
		}
		site.Classification, site.ErrVar = classify(n, parentMap)
		sites = append(sites, site)
		return true
	})

	return sites, nil
}

func isScanCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return sel.Sel.Name == "Scan"
}

// isDatabaseScan returns true if the receiver of the .Scan() call looks
// like a database/sql Rows or Row. Excludes bufio.Scanner, fmt.Scans,
// strings.Reader.Scan, etc.
//
// Heuristic: receiver is an identifier named rows/row/lrows/rrow (case
// insensitive first letter), or the call chains through QueryRow/Query.
// Anything else is filtered out — the audit's purpose is the database
// observation surface, not all Scan calls in the codebase.
func isDatabaseScan(parent ast.Node, scanCall ast.Node) bool {
	call := scanCall.(*ast.CallExpr)
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	// Direct receiver: x.Scan(...) where x is an Ident.
	if id, ok := sel.X.(*ast.Ident); ok {
		name := strings.ToLower(id.Name)
		switch name {
		case "rows", "row", "rrows", "lrows", "rrow", "scanrow":
			return true
		}
		// bufio.Scanner has receiver 'scanner' — exclude.
		if name == "scanner" {
			return false
		}
		// Fall through: chained call below will catch.
	}

	// Chained: foo.QueryRow(...).Scan(...) or foo.Query(...).Scan(...).
	if innerCall, ok := sel.X.(*ast.CallExpr); ok {
		if innerSel, ok := innerCall.Fun.(*ast.SelectorExpr); ok {
			switch innerSel.Sel.Name {
			case "QueryRow", "QueryRowContext", "Query", "QueryContext":
				return true
			}
		}
	}

	// Unknown — don't assume database.
	return false
}

func patternString(parent ast.Node, scanCall ast.Node) string {
	call := scanCall.(*ast.CallExpr)
	sel, _ := call.Fun.(*ast.SelectorExpr)
	switch x := sel.X.(type) {
	case *ast.Ident:
		return x.Name + ".Scan"
	case *ast.CallExpr:
		// Likely db.QueryRow(...).Scan or rows.Next()-style chain
		if innerSel, ok := x.Fun.(*ast.SelectorExpr); ok {
			if innerID, ok := innerSel.X.(*ast.Ident); ok {
				return fmt.Sprintf("%s.%s(...).Scan", innerID.Name, innerSel.Sel.Name)
			}
		}
		return "(...).Scan"
	}
	return "?Scan"
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

func classify(scanCall ast.Node, parentMap map[ast.Node]ast.Node) (Classification, string) {
	parent := parentMap[scanCall]

	// Goroutine-silent: the enclosing FuncDecl is inside a GoStmt, AND
	// the error is captured but not propagated via a channel/callback.
	if inGoroutine(parentMap, scanCall) {
		return ClassGoroutineSilent, ""
	}

	// No-check: parent is an ExprStmt (Scan was called for side-effects
	// only, no assignment). E.g. db.QueryRow(...).Scan(&x) where &x is
	// already a field assignment and err is discarded.
	if _, ok := parent.(*ast.ExprStmt); ok {
		return ClassNoCheck, ""
	}

	// No-check: blank identifier on the LHS of the assignment.
	if assign, ok := parent.(*ast.AssignStmt); ok && len(assign.Lhs) >= 1 {
		if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name == "_" {
			return ClassNoCheck, "_"
		}
	}

	// From here we expect an error variable to exist on the LHS.
	errVar := errVarName(parent)
	if errVar == "" {
		return ClassUnknown, ""
	}

	// Walk up to find the enclosing IfStmt that handles this err var.
	ifStmt := findEnclosingIfStmt(scanCall, parentMap, errVar)
	if ifStmt == nil {
		return ClassNoCheck, errVar
	}

	// Carve out the ErrNoRows-tolerated case explicitly (per v, 2026-08-06).
	// `if err == sql.ErrNoRows { ... }` is legitimate business logic for
	// optional single-row reads; it must NOT be lumped with silent-swallow.
	if ifsHasErrNoRowsBranch(ifStmt) {
		// The ErrNoRows branch is tolerated; the OTHER branch (if present)
		// must still propagate the error. Check both.
		otherIsHardFail := ifsOtherBranchHardFails(ifStmt, errVar)
		if otherIsHardFail || ifStmt.Else == nil {
			return ClassErrNoRowsTolerated, errVar
		}
		// Else branch doesn't propagate → fake-hard-fail.
		return ClassFakeHardFail, errVar
	}

	// Plain `if err != nil { ... }` — inspect body.
	return classifyIfBody(ifStmt, errVar), errVar
}

func errVarName(parent ast.Node) string {
	assign, ok := parent.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) < 1 {
		return ""
	}
	id, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}

func findEnclosingIfStmt(node ast.Node, parentMap map[ast.Node]ast.Node, errVar string) *ast.IfStmt {
	n := node
	for n != nil {
		if ifs, ok := n.(*ast.IfStmt); ok {
			if ifs.Init != nil && referencesIdent(ifs.Init, errVar) {
				return ifs
			}
			if referencesIdent(ifs.Cond, errVar) {
				return ifs
			}
		}
		// If we're inside a BlockStmt, scan SIBLING statements for an
		// IfStmt that references errVar. Common Go pattern:
		//     err := rows.Scan(...)
		//     if err != nil { ... }
		// — the IfStmt is a sibling of the assignment, not an ancestor.
		if bs, ok := n.(*ast.BlockStmt); ok {
			if ifs := findSiblingIfStmt(bs, errVar); ifs != nil {
				return ifs
			}
		}
		n = parentMap[n]
	}
	return nil
}

func findSiblingIfStmt(bs *ast.BlockStmt, errVar string) *ast.IfStmt {
	for _, stmt := range bs.List {
		if ifs, ok := stmt.(*ast.IfStmt); ok {
			if ifs.Init != nil && referencesIdent(ifs.Init, errVar) {
				return ifs
			}
			if referencesIdent(ifs.Cond, errVar) {
				return ifs
			}
		}
	}
	return nil
}

func referencesIdent(expr ast.Node, name string) bool {
	if expr == nil {
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

// ifsHasErrNoRowsBranch returns true if the if statement's condition
// (or any chained else-if condition) tests for sql.ErrNoRows.
func ifsHasErrNoRowsBranch(ifs *ast.IfStmt) bool {
	if isErrNoRowsExpr(ifs.Cond) {
		return true
	}
	if ifs.Else != nil {
		if elseIf, ok := ifs.Else.(*ast.IfStmt); ok {
			return ifsHasErrNoRowsBranch(elseIf)
		}
	}
	return false
}

// ifsOtherBranchHardFails: given an if whose CONDITION is an ErrNoRows check,
// look at the ELSE chain and return true if every non-ErrNoRows branch
// propagates err (or returns hard).
func ifsOtherBranchHardFails(ifs *ast.IfStmt, errVar string) bool {
	if ifs.Else == nil {
		return true
	}
	switch e := ifs.Else.(type) {
	case *ast.IfStmt:
		// else if — recurse
		// If this branch is also ErrNoRows-tolerated, the next one matters.
		if isErrNoRowsExpr(e.Cond) {
			return ifsOtherBranchHardFails(e, errVar)
		}
		// Otherwise, check this branch propagates err.
		return branchPropagatesErr(e.Body, errVar)
	case *ast.BlockStmt:
		return branchPropagatesErr(e, errVar)
	}
	return false
}

func branchPropagatesErr(body *ast.BlockStmt, errVar string) bool {
	if body == nil {
		return false
	}
	// Look for any return that mentions errVar.
	for _, stmt := range body.List {
		ret, ok := stmt.(*ast.ReturnStmt)
		if !ok {
			continue
		}
		for _, r := range ret.Results {
			if id, ok := r.(*ast.Ident); ok && id.Name == errVar {
				return true
			}
		}
	}
	return false
}

// isErrNoRowsExpr returns true if expr is `err == sql.ErrNoRows` or
// `errors.Is(err, sql.ErrNoRows)`.
func isErrNoRowsExpr(expr ast.Expr) bool {
	if expr == nil {
		return false
	}
	// err == sql.ErrNoRows
	if be, ok := expr.(*ast.BinaryExpr); ok && be.Op == token.EQL {
		if sel, ok := be.Y.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "sql" && sel.Sel.Name == "ErrNoRows" {
				return true
			}
		}
	}
	// errors.Is(err, sql.ErrNoRows)
	if call, ok := expr.(*ast.CallExpr); ok {
		if isErrorsIsCall(call) && len(call.Args) >= 2 {
			if sel, ok := call.Args[1].(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "sql" && sel.Sel.Name == "ErrNoRows" {
					return true
				}
			}
		}
	}
	return false
}

func isErrorsIsCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "errors" && sel.Sel.Name == "Is" {
		return true
	}
	return false
}

// classifyIfBody inspects the body of an `if err != nil { ... }` block.
func classifyIfBody(ifStmt *ast.IfStmt, errVar string) Classification {
	body := ifStmt.Body
	if body == nil {
		return ClassUnknown
	}

	// Continue + log call → logged-swallow (error observed via log/Warn/Error
	// before being dropped; not propagated, but not silent).
	// Continue alone → silent-continue (row silently dropped).
	if blockHasContinue(body) {
		if blockCallsLogError(body) {
			return ClassLoggedSwallow
		}
		return ClassSilentContinue
	}

	returns := findReturnStmts(body)
	if len(returns) == 0 {
		// No return, no continue → either logged-swallow or just plain swallow.
		// Heuristic: if any statement calls log/slog at error/warn level,
		// it's logged-swallow; otherwise it's a bare swallow.
		if blockCallsLogError(body) {
			return ClassLoggedSwallow
		}
		return ClassLoggedSwallow // same bucket for inventory
	}

	// Inspect each return: does it propagate err, return nil only,
	// or wrap in fmt.Errorf?
	for _, ret := range returns {
		if ret.Results == nil {
			// Bare return — uses named results. Heuristic: probably OK.
			return ClassHardFail
		}
		results := ret.Results
		if allIdentifiersMatch(results, []string{errVar}) {
			return ClassHardFail
		}
		// Wrapped error: fmt.Errorf("...: %w", err)
		if wrapsErr(results, errVar) {
			return ClassHardFail
		}
		// Returns only nil — fake-hard-fail.
		if allIdentifiersMatch(results, []string{"nil"}) {
			return ClassFakeHardFail
		}
		// Mixed: some nil + something else → probably hard-fail but flag for review.
		if len(results) > 0 {
			return ClassHardFail
		}
	}

	return ClassUnknown
}

func blockHasContinue(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if bs, ok := n.(*ast.BranchStmt); ok && bs.Tok == token.CONTINUE {
			found = true
			return false
		}
		return true
	})
	return found
}

// blockCallsLogError returns true if the block contains any call that
// observes an error condition: standard log/slog at warn/error level,
// fmt.Errorf (commonly used as a wrapping log), MPM's usererror.Warn /
// usererror.Error / usererror.Errorf, or any *.LogAudit(...) call
// (the audit trail is observation regardless of level).
//
// Heuristic, not exact: it's intentionally permissive. A false positive
// (logged-swallow for code that doesn't actually log the error) only
// under-reports silent-continue sites; the pre-commit gate fails on
// silent-continue, so under-reporting is the SAFE direction.
func blockCallsLogError(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		// Any *.LogAudit(...) call is an audit-trail observation —
		// the error is being recorded regardless of severity level.
		if sel.Sel.Name == "LogAudit" {
			found = true
			return false
		}

		// usererror.Warn / usererror.Error / usererror.Errorf —
		// the MPM CLI-facing logging primitive. Detect by package name.
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "usererror" {
			switch sel.Sel.Name {
			case "Warn", "Error", "Errorf":
				found = true
				return false
			}
		}

		// log.Error / log.Warn / slog.Error / slog.Warn / etc.
		switch sel.Sel.Name {
		case "Error", "Errorf", "Warn", "Warnf":
			// Conservative: only treat the call as a log if the receiver
			// is one of the known logging packages OR is a bare call
			// (rare in MPM but possible).
			if id, ok := sel.X.(*ast.Ident); ok {
				switch id.Name {
				case "log", "slog", "l", "logger", "logg", "lgr":
					found = true
					return false
				}
			}
		}

		// fmt.Errorf / fmt.Printf — commonly used as a log channel in
		// older MPM code (LogAudit body often wraps fmt.Sprintf).
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "fmt" {
			switch sel.Sel.Name {
			case "Errorf", "Printf", "Sprintf":
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func findReturnStmts(body *ast.BlockStmt) []*ast.ReturnStmt {
	var rets []*ast.ReturnStmt
	ast.Inspect(body, func(n ast.Node) bool {
		if ret, ok := n.(*ast.ReturnStmt); ok {
			rets = append(rets, ret)
		}
		return true
	})
	return rets
}

func allIdentifiersMatch(exprs []ast.Expr, names []string) bool {
	if len(exprs) != len(names) {
		return false
	}
	for i, e := range exprs {
		id, ok := e.(*ast.Ident)
		if !ok {
			return false
		}
		if id.Name != names[i] {
			return false
		}
	}
	return true
}

func wrapsErr(exprs []ast.Expr, errVar string) bool {
	if len(exprs) == 0 {
		return false
	}
	first, ok := exprs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := first.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if id, ok := sel.X.(*ast.Ident); ok && id.Name == "fmt" && sel.Sel.Name == "Errorf" {
		// Look for `%w, errVar` in the args.
		for _, arg := range first.Args[1:] {
			if id, ok := arg.(*ast.Ident); ok && id.Name == errVar {
				return true
			}
		}
	}
	return false
}

// inGoroutine returns true if node is contained within a GoStmt somewhere
// in its ancestor chain.
func inGoroutine(parentMap map[ast.Node]ast.Node, node ast.Node) bool {
	n := node
	for n != nil {
		if _, ok := n.(*ast.GoStmt); ok {
			return true
		}
		n = parentMap[n]
	}
	return false
}

// -------------------------------------------------------------------- context capture

// contextLines returns up to 7 lines of source around `line` (3 before,
// the line itself marked with '> ', 3 after). Used to make report entries
// self-explanatory.
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

	fmt.Fprintln(w, "# Scan Audit Report")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "**Total Scan calls:** %d\n\n", len(sites))

	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	classOrder := []Classification{
		ClassHardFail, ClassErrNoRowsTolerated, ClassTolerated,
		ClassSilentContinue, ClassLoggedSwallow, ClassFakeHardFail,
		ClassNoCheck, ClassHelperSilent, ClassGoroutineSilent,
		ClassUnknown,
	}
	for _, c := range classOrder {
		if n, ok := counts[c]; ok {
			fmt.Fprintf(w, "- **%s**: %d\n", c, n)
		}
	}
	fmt.Fprintln(w)

	// Verdicts
	total := len(sites)
	bad := counts[ClassSilentContinue] + counts[ClassLoggedSwallow] +
		counts[ClassFakeHardFail] + counts[ClassNoCheck] +
		counts[ClassHelperSilent] + counts[ClassGoroutineSilent] +
		counts[ClassUnknown]
	if total > 0 {
		pctBad := float64(bad) / float64(total) * 100
		fmt.Fprintf(w, "**Silent / unhandled failures: %d / %d (%.1f%%)**\n\n", bad, total, pctBad)
	}

	// Sites requiring attention: every class EXCEPT hard-fail and tolerated.
	fmt.Fprintln(w, "## Sites Requiring Attention")
	fmt.Fprintln(w)
	skip := map[Classification]bool{
		ClassHardFail:           true,
		ClassErrNoRowsTolerated: true,
		ClassTolerated:          true,
	}
	for _, s := range sites {
		if skip[s.Classification] {
			continue
		}
		fmt.Fprintf(w, "### `%s` — line %d (%s)", s.File, s.Line, s.Pattern)
		fmt.Fprintln(w)
		fmt.Fprintf(w, "- **Classification:** %s\n", s.Classification)
		fmt.Fprintf(w, "- **Enclosing function:** `%s`\n", s.FuncName)
		if s.ErrVar != "" {
			fmt.Fprintf(w, "- **Err variable:** `%s`\n", s.ErrVar)
		}
		fmt.Fprintln(w, "- **Code:**")
		fmt.Fprintln(w, "  ```go")
		for _, line := range s.Code {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w, "  ```")
		fmt.Fprintln(w)
	}

	// And a final tally of hard-fail sites so we know the good news too.
	fmt.Fprintln(w, "## Hard-Fail Sites (correct)")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d sites propagate errors correctly. Listed for sanity, not for action.\n",
		counts[ClassHardFail])
}

func emitJSON(w *os.File, sites []Site) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(struct {
		Total int   `json:"total"`
		Sites []Site `json:"sites"`
	}{Total: len(sites), Sites: sites})
}

func countByClass(sites []Site) map[Classification]int {
	m := make(map[Classification]int)
	for _, s := range sites {
		m[s.Classification]++
	}
	return m
}
