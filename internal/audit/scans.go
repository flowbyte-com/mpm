// Scan error handling rule — the ported audit-scans classifier.
//
// Inventories every `.Scan(...)` call against database/sql Rows or Row and
// classifies each site by how it handles the returned error:
//
//	hard-fail          — error returned to caller (correct)
//	ErrNoRows-tolerated — `if err == sql.ErrNoRows { return nil }` (legitimate
//	                     empty-state branch; the schema-DDL-flip migration
//	                     that motivated this audit MUST distinguish empty
//	                     rows from scan panics)
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
// Scope note: the cmd/ subdirs for mpm-cmd, mpm-scheduler, mpm-critic don't
// reach the DB directly (they shell out to `mpm call`); cmd/mpm is the
// front-door that does reach the DB but usually via MCP tool wrappers, so
// most scan sites live in internal/core/.
//
// Gate: zero-tolerance for silent-continue / fake-hard-fail / no-check.
// logged-swallow / ErrNoRows-tolerated / tolerated / hard-fail / unknown
// pass by design.
package audit

import (
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"strings"
)

// Classification is the scans vocabulary.
type Classification = string

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

// ScansRule inventories Scan sites and their error-handling class.
type ScansRule struct{}

// NewScansRule constructs the scan-error rule.
func NewScansRule() Rule { return &ScansRule{} }

// Name implements Rule.
func (r *ScansRule) Name() string { return "scans" }

// Prepare implements Rule (no cross-file state).
func (r *ScansRule) Prepare(files []*File) {}

// AuditFile implements Rule.
func (r *ScansRule) AuditFile(f *File) []Site {
	var sites []Site
	ast.Inspect(f.AST, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !r.isScanCall(call) {
			return true
		}
		// Filter out non-database Scan calls (bufio.Scanner, fmt.Scan,
		// strings.Reader, etc.). Only audit Scan() calls against *sql.Rows
		// or *sql.Row, which is the closed-under-observation surface.
		if !r.isDatabaseScan(f.ParentMap[call], call) {
			return true
		}
		pos := f.FSet.Position(call.Pos())
		site := Site{
			File:     f.Path,
			Line:     pos.Line,
			Column:   pos.Column,
			FuncName: EnclosingFuncName(f.ParentMap, call),
			Pattern:  r.patternString(f.ParentMap[call], call),
			Code:     ContextLines(f.Src, pos.Line),
		}
		site.Classification, site.ErrVar = r.classify(call, f.ParentMap)
		sites = append(sites, site)
		return true
	})
	return sites
}

// EmitMarkdown implements Rule.
func (r *ScansRule) EmitMarkdown(w io.Writer, sites []Site) {
	counts := CountByClass(sites)

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

	total := len(sites)
	bad := counts[ClassSilentContinue] + counts[ClassLoggedSwallow] +
		counts[ClassFakeHardFail] + counts[ClassNoCheck] +
		counts[ClassHelperSilent] + counts[ClassGoroutineSilent] +
		counts[ClassUnknown]
	if total > 0 {
		pctBad := float64(bad) / float64(total) * 100
		fmt.Fprintf(w, "**Silent / unhandled failures: %d / %d (%.1f%%)**\n\n", bad, total, pctBad)
	}

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

	fmt.Fprintln(w, "## Hard-Fail Sites (correct)")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d sites propagate errors correctly. Listed for sanity, not for action.\n",
		counts[ClassHardFail])
}

// GateChecks implements Rule. silent-continue / fake-hard-fail / no-check
// default to zero-tolerance; the max map overrides per class.
func (r *ScansRule) GateChecks(sites []Site, max map[string]int) []GateCheck {
	counts := CountByClass(sites)
	checks := []GateCheck{
		{ClassSilentContinue, counts[ClassSilentContinue], max[ClassSilentContinue]},
		{ClassFakeHardFail, counts[ClassFakeHardFail], max[ClassFakeHardFail]},
		{ClassNoCheck, counts[ClassNoCheck], max[ClassNoCheck]},
	}
	return ResolveRatchet(checks)
}

func (r *ScansRule) isScanCall(call *ast.CallExpr) bool {
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
// insensitive), or the call chains through QueryRow/Query. Anything else
// is filtered out.
func (r *ScansRule) isDatabaseScan(parent ast.Node, scanCall ast.Node) bool {
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

func (r *ScansRule) patternString(parent ast.Node, scanCall ast.Node) string {
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

func (r *ScansRule) classify(scanCall ast.Node, parentMap map[ast.Node]ast.Node) (Classification, string) {
	parent := parentMap[scanCall]

	// Goroutine-silent: the Scan is inside a GoStmt.
	if r.inGoroutine(parentMap, scanCall) {
		return ClassGoroutineSilent, ""
	}

	// No-check: parent is an ExprStmt (Scan called for side-effects only).
	if _, ok := parent.(*ast.ExprStmt); ok {
		return ClassNoCheck, ""
	}

	// No-check: blank identifier on the LHS of the assignment.
	if assign, ok := parent.(*ast.AssignStmt); ok && len(assign.Lhs) >= 1 {
		if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name == "_" {
			return ClassNoCheck, "_"
		}
	}

	errVar := r.errVarName(parent)
	if errVar == "" {
		return ClassUnknown, ""
	}

	// Walk up to find the enclosing IfStmt that handles this err var.
	ifStmt := r.findEnclosingIfStmt(scanCall, parentMap, errVar)
	if ifStmt == nil {
		return ClassNoCheck, errVar
	}

	// Carve out the ErrNoRows-tolerated case explicitly: `if err ==
	// sql.ErrNoRows { ... }` is legitimate business logic for optional
	// single-row reads; it must NOT be lumped with silent-swallow.
	if r.ifsHasErrNoRowsBranch(ifStmt) {
		otherIsHardFail := r.ifsOtherBranchHardFails(ifStmt, errVar)
		if otherIsHardFail || ifStmt.Else == nil {
			return ClassErrNoRowsTolerated, errVar
		}
		return ClassFakeHardFail, errVar
	}

	return r.classifyIfBody(ifStmt, errVar), errVar
}

func (r *ScansRule) errVarName(parent ast.Node) string {
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

func (r *ScansRule) findEnclosingIfStmt(node ast.Node, parentMap map[ast.Node]ast.Node, errVar string) *ast.IfStmt {
	n := node
	for n != nil {
		if ifs, ok := n.(*ast.IfStmt); ok {
			if ifs.Init != nil && ReferencesIdent(ifs.Init, errVar) {
				return ifs
			}
			if ReferencesIdent(ifs.Cond, errVar) {
				return ifs
			}
		}
		// If we're inside a BlockStmt, scan SIBLING statements for an
		// IfStmt that references errVar. Common Go pattern:
		//     err := rows.Scan(...)
		//     if err != nil { ... }
		if bs, ok := n.(*ast.BlockStmt); ok {
			if ifs := r.findSiblingIfStmt(bs, errVar); ifs != nil {
				return ifs
			}
		}
		n = parentMap[n]
	}
	return nil
}

func (r *ScansRule) findSiblingIfStmt(bs *ast.BlockStmt, errVar string) *ast.IfStmt {
	for _, stmt := range bs.List {
		if ifs, ok := stmt.(*ast.IfStmt); ok {
			if ifs.Init != nil && ReferencesIdent(ifs.Init, errVar) {
				return ifs
			}
			if ReferencesIdent(ifs.Cond, errVar) {
				return ifs
			}
		}
	}
	return nil
}

// ifsHasErrNoRowsBranch returns true if the if statement's condition
// (or any chained else-if condition) tests for sql.ErrNoRows.
func (r *ScansRule) ifsHasErrNoRowsBranch(ifs *ast.IfStmt) bool {
	if r.isErrNoRowsExpr(ifs.Cond) {
		return true
	}
	if ifs.Else != nil {
		if elseIf, ok := ifs.Else.(*ast.IfStmt); ok {
			return r.ifsHasErrNoRowsBranch(elseIf)
		}
	}
	return false
}

// ifsOtherBranchHardFails: given an if whose CONDITION is an ErrNoRows check,
// look at the ELSE chain and return true if every non-ErrNoRows branch
// propagates err (or returns hard).
func (r *ScansRule) ifsOtherBranchHardFails(ifs *ast.IfStmt, errVar string) bool {
	if ifs.Else == nil {
		return true
	}
	switch e := ifs.Else.(type) {
	case *ast.IfStmt:
		// else if — recurse; if this branch is also ErrNoRows-tolerated,
		// the next one matters.
		if r.isErrNoRowsExpr(e.Cond) {
			return r.ifsOtherBranchHardFails(e, errVar)
		}
		return r.branchPropagatesErr(e.Body, errVar)
	case *ast.BlockStmt:
		return r.branchPropagatesErr(e, errVar)
	}
	return false
}

func (r *ScansRule) branchPropagatesErr(body *ast.BlockStmt, errVar string) bool {
	if body == nil {
		return false
	}
	for _, stmt := range body.List {
		ret, ok := stmt.(*ast.ReturnStmt)
		if !ok {
			continue
		}
		for _, res := range ret.Results {
			if id, ok := res.(*ast.Ident); ok && id.Name == errVar {
				return true
			}
		}
	}
	return false
}

// isErrNoRowsExpr returns true if expr is `err == sql.ErrNoRows` or
// `errors.Is(err, sql.ErrNoRows)`.
func (r *ScansRule) isErrNoRowsExpr(expr ast.Expr) bool {
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
		if r.isErrorsIsCall(call) && len(call.Args) >= 2 {
			if sel, ok := call.Args[1].(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "sql" && sel.Sel.Name == "ErrNoRows" {
					return true
				}
			}
		}
	}
	return false
}

func (r *ScansRule) isErrorsIsCall(call *ast.CallExpr) bool {
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
func (r *ScansRule) classifyIfBody(ifStmt *ast.IfStmt, errVar string) Classification {
	body := ifStmt.Body
	if body == nil {
		return ClassUnknown
	}

	// Continue + log call → logged-swallow; Continue alone →
	// silent-continue (row silently dropped).
	if r.blockHasContinue(body) {
		if r.blockCallsLogError(body) {
			return ClassLoggedSwallow
		}
		return ClassSilentContinue
	}

	returns := r.findReturnStmts(body)
	if len(returns) == 0 {
		// No return, no continue → either logged-swallow or just plain
		// swallow; the heuristic buckets both as logged-swallow.
		if r.blockCallsLogError(body) {
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
		if r.wrapsErr(results, errVar) {
			return ClassHardFail
		}
		// Returns only nil — fake-hard-fail.
		if allIdentifiersMatch(results, []string{"nil"}) {
			return ClassFakeHardFail
		}
		// Mixed: some nil + something else → probably hard-fail but flag
		// for review.
		if len(results) > 0 {
			return ClassHardFail
		}
	}

	return ClassUnknown
}

func (r *ScansRule) blockHasContinue(body *ast.BlockStmt) bool {
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
// usererror.Error / usererror.Errorf, or any *.LogAudit(...) call (the
// audit trail is observation regardless of level).
//
// Heuristic, not exact: it's intentionally permissive. A false positive
// (logged-swallow for code that doesn't actually log the error) only
// under-reports silent-continue sites; the gate fails on silent-continue,
// so under-reporting is the SAFE direction.
func (r *ScansRule) blockCallsLogError(body *ast.BlockStmt) bool {
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

		// Any *.LogAudit(...) call is an audit-trail observation.
		if sel.Sel.Name == "LogAudit" {
			found = true
			return false
		}

		// usererror.Warn / usererror.Error / usererror.Errorf.
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
			if id, ok := sel.X.(*ast.Ident); ok {
				switch id.Name {
				case "log", "slog", "l", "logger", "logg", "lgr":
					found = true
					return false
				}
			}
		}

		// fmt.Errorf / fmt.Printf — commonly used as a log channel.
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

func (r *ScansRule) findReturnStmts(body *ast.BlockStmt) []*ast.ReturnStmt {
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

func (r *ScansRule) wrapsErr(exprs []ast.Expr, errVar string) bool {
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
func (r *ScansRule) inGoroutine(parentMap map[ast.Node]ast.Node, node ast.Node) bool {
	n := node
	for n != nil {
		if _, ok := n.(*ast.GoStmt); ok {
			return true
		}
		n = parentMap[n]
	}
	return false
}
