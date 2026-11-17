package internal

// shared_primitive_budget_guard_test.go — issue #178, first remediation
// slice: the guard for the SHARED-PRIMITIVE writes.
//
// WHY A SEPARATE GUARD
// --------------------
// Some of this slice's sites cannot be guarded by the bare-pool ratchet
// alone.
//
// The review and the weight primitives (recordMemoryReviewOn,
// weakenMemoryOn) are deliberately SHARED: one function whose executor
// is a pool on one call site and a transaction on another, so that the
// validation and the read-back assertion cannot drift apart. The pool
// call site now passes a budgeted executor (poolMemExecer), so the
// write is no longer bare there.
//
// But the ratchet is SYNTACTIC. It sees `ex.Exec(...)` — an Exec on a
// non-transaction receiver — and it cannot see which concrete executor
// the call site passes. So it keeps flagging the site, and the site
// stays in the allowlist with the reason `shared-primitive`.
//
// That is the correct direction for the ratchet: a NEW bare write
// through the same primitive must still trip its surplus check. But it
// means the ratchet cannot prove that the pool path is budgeted, and
// the allowlist entry would otherwise rest on an assertion nobody
// checks.
//
// THIS FILE IS THE CHECK. It is an AST guard, and it asserts:
//
//  1. REACH — every function in the runtime that takes an
//     Exec-bearing interface parameter is CLASSIFIED. A shared
//     primitive that nobody listed cannot slip past the guard by being
//     written; the guard finds it and fails until it is classified.
//
//  2. BUDGET — for a shared primitive that writes AGENT-FACING STATE,
//     every construction of a poolMemExecer carries a NON-ZERO retry
//     budget (a zero budget is the #164 defect class in a new place),
//     and every call site passes either a poolMemExecer (the pool path)
//     or a transaction (the transactional path). Passing a raw pool
//     handle there would reintroduce the bare write this slice
//     removed.
//
//  3. ADAPTER — the REAL poolMemExecer.Exec routes through execTracked
//     with the stored budget — otherwise the budget would be carried
//     and then silently dropped.
//
//  4. DDL-ONLY — a shared primitive classified as migration DDL must
//     execute NO DML. That classification is the reason its pool path
//     is allowed to pass a raw pool handle: a retry of `CREATE TRIGGER`
//     (which has no `IF NOT EXISTS`) fails with "already exists", so
//     the remedy for that class is a transaction, not a statement
//     budget. If the primitive ever starts writing rows, the
//     classification is wrong and the guard says so.
//
// It is a structural guard: it fails when the SHAPE changes, not when a
// count drifts.
//
// WHY THE DETECTION LIVES IN PURE FUNCTIONS
// -----------------------------------------
// The independent review of 2026-11-06 (finding M2) showed that
// three of the four assertions above could be BROKEN — the
// `routesThroughExecTracked` check made to always return true, the
// reach check made to never fire, the DML pattern made to match
// nothing — and the WHOLE SUITE stayed green. The assertions were not
// pinned by anything: they were tree-only checks, and a tree-only
// check whose DETECTION can be switched off proves nothing about its
// own detection.
//
// That is exactly the argument the record makes against a guard that
// checks only what its author already knew about — turned back on the
// guard itself.
//
// The fix is to put the DETECTION in pure functions over (rel, src),
// the way the bare-pool ratchet already does with
// `scanBarePoolSource`, so that a synthetic source can be fed through
// them and the resulting FINDING can be asserted directly. A control
// that feeds a deliberately broken shape and REQUIRES the
// corresponding finding is a control that fails when the detection is
// broken. That is the difference between an assertion and a control.
//
// The controls, one per assertion that was unpinned:
//
//   - TestPoolMemExecerRoutesThroughTheTrackedPolicy — the ADAPTER
//     assertion. A synthetic adapter that spends a LITERAL budget
//     must be flagged (the budget would be carried and then dropped);
//     the same adapter spending its own `retries` field must not be.
//
//   - TestSharedPrimitiveReachDetectsUnclassifiedPrimitive — the
//     REACH assertion. A synthetic function that takes an
//     Exec-bearing interface and has no classification must be
//     flagged; the same function WITH a classification must not be.
//
//   - TestSharedPrimitiveDDLOnlyDetectsRowWritingPrimitive — the
//     DDL-ONLY assertion. A synthetic classMigrationDDL primitive
//     that writes a row must be flagged; the same primitive
//     classified as classPoolWrite must not be.
//
// The ADAPTER assertion additionally has a RUNTIME proof, in
// bare_pool_write_contention_test.go: the real poolMemExecer.Exec
// waits ordinary contention out and lands its write, where a bare
// pool write of the same statement loses the artifact. The AST
// control says the adapter routes through the tracked policy; the
// runtime control says the tracked policy is worth routing through.
//
// WHY THE DML DETECTION SEES THROUGH COMMENTS
// --------------------------------------------
// Finding M3 of the same review: the DDL-ONLY check used to match
// `^\s*(INSERT INTO|UPDATE …|DELETE FROM|REPLACE INTO)`, so a
// row-writing statement written with an explanatory comment above it
// — which is exactly what a migration author writes — did not match,
// and the assertion could be defeated by adding a comment. The
// detection now strips leading whitespace and leading SQL comments
// (`--` to end of line, `/* … */`) before matching. The match stays
// ANCHORED at the first token of the statement: the row-writing DML
// inside a `CREATE TRIGGER … BEGIN DELETE FROM … END` is part of the
// schema definition, not a row write by the primitive, and must stay
// classified as DDL.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The classes a shared primitive can carry.
//
// The class is the answer to: what does this primitive write, and
// therefore what is the right remedy if its pool path loses the write?
const (
	// classPoolWrite: the primitive writes agent-facing state (a
	// memory's weight, a review record). Losing the write discards
	// state, so the pool path carries a statement-level budget.
	classPoolWrite = "pool-write"

	// classMigrationDDL: the primitive writes SCHEMA (a view, a
	// trigger). Its pool path is a repair path, and the repair is
	// self-healing: the next boot detects the missing schema and
	// repairs it again. A retry is not the remedy, because a retry of
	// `CREATE TRIGGER` (no `IF NOT EXISTS`) fails with "already
	// exists" — so the transaction, not the budget, is what makes
	// this atomic.
	classMigrationDDL = "migration-ddl"
)

// sharedPrimitives classifies every shared primitive in the runtime.
//
// The list is not the source of truth for EXISTENCE: the guard
// DISCOVERS every function that takes an interface-typed parameter with
// an `Exec` method, and fails if such a function is not classified
// here. A new shared primitive therefore cannot slip past the guard by
// being written; it has to be classified, and the classification is
// what the guard then checks.
//
// The two pool-write entries are this slice's sites. The migration-DDL
// entry was found BY the guard's own reach check on 2026-11-06 — see
// the record's §4.3.
var sharedPrimitives = map[string]string{
	"weakenMemoryOn":               classPoolWrite,
	"recordMemoryReviewOn":         classPoolWrite,
	"createLessonsViewAndTriggers": classMigrationDDL,
}

// The findings the guard can report.
//
// They are named constants rather than free-form messages so that a
// synthetic control can assert on the KIND of finding it provoked:
// "the detector stopped detecting" has to be distinguishable from "the
// detector reported something else".
const (
	// findingUnclassifiedPrimitive: a function takes an Exec-bearing
	// interface and has no classification.
	findingUnclassifiedPrimitive = "unclassified-shared-primitive"

	// findingMigrationDDLWithDML: a classMigrationDDL primitive
	// executes a row-writing statement.
	findingMigrationDDLWithDML = "migration-ddl-writes-rows"

	// findingExecutorWithoutBudget: a poolMemExecer is built without
	// a resolvable retries budget.
	findingExecutorWithoutBudget = "pool-mem-execer-without-a-budget"

	// findingExecutorWithZeroBudget: a poolMemExecer is built with a
	// non-positive retries budget.
	findingExecutorWithZeroBudget = "pool-mem-execer-with-a-zero-budget"

	// findingAdapterDropsBudget: the poolMemExecer.Exec method does
	// not route through execTracked with its own retries field.
	findingAdapterDropsBudget = "adapter-drops-its-retry-budget"

	// findingPrimitiveWithoutExecutor: a shared primitive is called
	// with no executor at all.
	findingPrimitiveWithoutExecutor = "shared-primitive-called-with-no-executor"

	// findingPrimitiveWithRawPoolHandle: a shared primitive is called
	// with something that is neither a budgeted poolMemExecer nor a
	// transaction.
	findingPrimitiveWithRawPoolHandle = "shared-primitive-called-with-a-raw-pool-handle"

	// findingUnparsableFile: the file could not be parsed, so nothing
	// about it could be checked.
	findingUnparsableFile = "file-could-not-be-parsed"
)

// sharedPrimitiveFinding is one problem the guard found.
type sharedPrimitiveFinding struct {
	// Kind is one of the finding constants above.
	Kind string
	// Rel is the repository-relative path of the file the finding is
	// in.
	Rel string
	// Detail explains the finding in a sentence.
	Detail string
}

// sharedPrimitiveScan is the result of scanning one file: the findings,
// plus what the scan SAW. The "saw" flags are whole-tree REACH checks
// — they prove the scan reached the constructs it is supposed to
// check, so a guard that stopped matching anything at all cannot pass
// by having nothing to match.
type sharedPrimitiveScan struct {
	Findings []sharedPrimitiveFinding

	SawBudgetedExecutor bool
	SawPoolCallSite     bool
	SawTxCallSite       bool
	SawTrackedAdapter   bool
}

// sqlDMLPattern matches a SQL statement that WRITES ROWS.
//
// The object keyword is required so that an English sentence in an
// error message ("create lessons view failed") is not mistaken for a
// statement. Only the verbs that mutate rows are listed: a
// migration-DDL primitive that starts executing one of these is
// misclassified, and the guard says so.
//
// The match is made against the statement with its LEADING COMMENTS
// STRIPPED (see stripLeadingSQLComments). Before 2026-11-06 the
// pattern was `^\s*(…)`, and `^\s*` matched only whitespace — so a
// row-writing statement written under an explanatory comment did not
// match, and the DDL-ONLY assertion could be defeated by adding a
// comment. A migration author writes the comment; the guard has to
// see through it.
//
// The match stays ANCHORED at the first token of the statement. The
// row-writing DML inside `CREATE TRIGGER … BEGIN DELETE FROM … END`
// is part of the schema definition, not a row write performed by the
// primitive, and must remain classified as DDL.
var sqlDMLPattern = regexp.MustCompile(`(?is)^(INSERT\s+INTO|UPDATE\s+[a-z_]|DELETE\s+FROM|REPLACE\s+INTO)`)

// stripLeadingSQLComments removes leading whitespace and leading SQL
// comments from a statement.
//
// It handles the two comment forms SQLite accepts:
//
//   - a line comment: `--` up to the end of the line;
//   - a block comment: `/* … */`, which may span lines.
//
// It loops, because a statement may be preceded by several comments
// (`-- one\n-- two\nINSERT …`) or by a block comment that is itself
// followed by a line comment. An unterminated comment consumes the
// rest of the string, which is the safe direction: the remainder is
// not a statement this guard can classify.
func stripLeadingSQLComments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n\v\f")
		switch {
		case strings.HasPrefix(s, "--"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return ""
			}
			s = s[i+1:]
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s, "*/")
			if i < 0 {
				return ""
			}
			s = s[i+2:]
		default:
			return s
		}
	}
}

// TestSharedPrimitivePoolPathsCarryAStatementLevelBudget is the guard.
func TestSharedPrimitivePoolPathsCarryAStatementLevelBudget(t *testing.T) {
	root, err := writeBudgetRepoRoot()
	if err != nil {
		t.Skipf("the repository root is not reachable from this checkout (%v)", err)
	}

	var paths []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "node_modules" || base == ".git" || base == ".build" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the runtime: %v", err)
	}

	// Per package (directory), the interface types that expose an Exec
	// method: those are the executor abstractions a shared primitive
	// can be written against.
	execInterfacesByDir := map[string]map[string]bool{}
	for _, path := range paths {
		dir := filepath.Dir(path)
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			continue
		}
		for name := range execInterfacesInFile(file) {
			if execInterfacesByDir[dir] == nil {
				execInterfacesByDir[dir] = map[string]bool{}
			}
			execInterfacesByDir[dir][name] = true
		}
	}

	var findings []sharedPrimitiveFinding
	sawBudgetedExecutor := false
	sawPoolCallSite := false
	sawTxCallSite := false
	sawTrackedAdapter := false

	for _, path := range paths {
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", rel, rerr)
		}
		scan := scanSharedPrimitiveSource(rel, src, execInterfacesByDir[filepath.Dir(path)], sharedPrimitives)
		findings = append(findings, scan.Findings...)
		sawBudgetedExecutor = sawBudgetedExecutor || scan.SawBudgetedExecutor
		sawPoolCallSite = sawPoolCallSite || scan.SawPoolCallSite
		sawTxCallSite = sawTxCallSite || scan.SawTxCallSite
		sawTrackedAdapter = sawTrackedAdapter || scan.SawTrackedAdapter
	}

	for _, f := range findings {
		switch f.Kind {
		case findingUnparsableFile:
			t.Errorf("%s: %s — the guard could not read the file, so it proved nothing about it", f.Rel, f.Detail)
		case findingUnclassifiedPrimitive:
			t.Errorf("%s: %s", f.Rel, f.Detail)
		case findingMigrationDDLWithDML:
			t.Errorf("%s: %s", f.Rel, f.Detail)
		case findingExecutorWithoutBudget:
			t.Errorf("%s: %s", f.Rel, f.Detail)
		case findingExecutorWithZeroBudget:
			t.Errorf("%s: %s", f.Rel, f.Detail)
		case findingAdapterDropsBudget:
			t.Errorf("%s: %s", f.Rel, f.Detail)
		case findingPrimitiveWithoutExecutor:
			t.Errorf("%s: %s", f.Rel, f.Detail)
		case findingPrimitiveWithRawPoolHandle:
			t.Errorf("%s: %s", f.Rel, f.Detail)
		default:
			t.Errorf("%s: %s", f.Rel, f.Detail)
		}
	}

	if !sawBudgetedExecutor {
		t.Error("no poolMemExecer construction was found — the shared-primitive pool path " +
			"is no longer budgeted, or the guard has lost reach")
	}
	if !sawPoolCallSite {
		t.Error("no shared-primitive call site passes a poolMemExecer — the guard has lost reach")
	}
	if !sawTxCallSite {
		t.Error("no shared-primitive call site passes a transaction — the guard has lost reach")
	}
	if !sawTrackedAdapter {
		t.Error("the poolMemExecer.Exec method was not found — the adapter's write path is " +
			"no longer checked, or the guard has lost reach")
	}
}

// scanSharedPrimitiveSource parses one file and returns everything the
// shared-primitive guard finds in it.
//
// It is a PURE function of its arguments: no tree walk, no test
// context, no global state beyond what the caller passes in. That is
// what makes the synthetic controls possible — a control feeds a
// source that deliberately breaks one of the guard's assertions and
// asserts that the corresponding FINDING appears.
//
//   - execInterfaces is the set of Exec-bearing interface names
//     visible to the file (the guard collects them per package; the
//     file's own interfaces are added here, so a synthetic source can
//     declare its own executor interface).
//   - classified maps a shared primitive's name to its class. The
//     runtime passes sharedPrimitives; a synthetic control passes its
//     own map, so it can prove that a classification is what silences
//     the reach finding.
func scanSharedPrimitiveSource(rel string, src []byte, execInterfaces map[string]bool, classified map[string]string) sharedPrimitiveScan {
	var out sharedPrimitiveScan

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		out.Findings = append(out.Findings, sharedPrimitiveFinding{
			Kind:   findingUnparsableFile,
			Rel:    rel,
			Detail: err.Error(),
		})
		return out
	}

	visible := map[string]bool{}
	for name := range execInterfaces {
		visible[name] = true
	}
	for name := range execInterfacesInFile(file) {
		visible[name] = true
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			// The adapter's own write path, checked wherever it
			// lives. It is a method, so it takes no executor
			// parameter and is not itself a shared primitive.
			if node.Recv != nil && len(node.Recv.List) > 0 &&
				node.Name.Name == "Exec" && node.Body != nil &&
				receiverTypeName(node.Recv.List[0].Type) == "poolMemExecer" {
				out.SawTrackedAdapter = true
				if !routesThroughExecTracked(node.Body, node.Recv.List[0].Names[0].Name) {
					out.Findings = append(out.Findings, sharedPrimitiveFinding{
						Kind: findingAdapterDropsBudget,
						Rel:  rel,
						Detail: "poolMemExecer.Exec does not route its write through " +
							"execTracked with the adapter's own retries field — the budget " +
							"would be carried and then silently dropped",
					})
				}
			}

			// REACH: a function that takes an executor interface is
			// a shared primitive, whether or not anyone has
			// remembered to classify it.
			class, isShared := sharedPrimitiveClassOf(node, visible, classified)
			if !isShared {
				return true
			}
			if class == "" {
				out.Findings = append(out.Findings, sharedPrimitiveFinding{
					Kind: findingUnclassifiedPrimitive,
					Rel:  rel,
					Detail: node.Name.Name + " takes an executor interface, so its pool path " +
						"needs a classification — add it to sharedPrimitives so the " +
						"guard checks it (a pool-write primitive needs a statement " +
						"budget; a migration-DDL primitive must execute no DML)",
				})
				return true
			}
			if class == classMigrationDDL {
				if stmt, ok := dmlStatementIn(node); ok {
					out.Findings = append(out.Findings, sharedPrimitiveFinding{
						Kind: findingMigrationDDLWithDML,
						Rel:  rel,
						Detail: node.Name.Name + " is classified as " + class + ", but it executes the " +
							"row-writing statement " + strconv.Quote(firstSQLWords(stmt)) +
							" — a pool-write primitive needs a statement budget on its " +
							"pool path, so reclassify it",
					})
				}
			}
		case *ast.CompositeLit:
			if ident, ok := node.Type.(*ast.Ident); !ok || ident.Name != "poolMemExecer" {
				return true
			}
			out.SawBudgetedExecutor = true
			retries, ok := compositeIntField(node, "retries")
			if !ok {
				out.Findings = append(out.Findings, sharedPrimitiveFinding{
					Kind: findingExecutorWithoutBudget,
					Rel:  rel,
					Detail: "a poolMemExecer is built without a resolvable retries budget — " +
						"the pool path would carry no budget at all",
				})
				return true
			}
			if retries <= 0 {
				out.Findings = append(out.Findings, sharedPrimitiveFinding{
					Kind: findingExecutorWithZeroBudget,
					Rel:  rel,
					Detail: "a poolMemExecer is built with a retry budget of " +
						strconv.Itoa(retries) + " — a zero budget is the issue #164 defect class: " +
						"the statement-level retry is disabled and the write is lost to " +
						"ordinary contention",
				})
			}
		case *ast.CallExpr:
			name := calleeName(node.Fun)
			class, ok := classified[name]
			if !ok || class != classPoolWrite {
				return true
			}
			if len(node.Args) == 0 {
				out.Findings = append(out.Findings, sharedPrimitiveFinding{
					Kind: findingPrimitiveWithoutExecutor,
					Rel:  rel,
					Detail: name + " is called with no executor",
				})
				return true
			}
			arg := node.Args[0]
			switch {
			case isPoolMemExecerLiteral(arg):
				out.SawPoolCallSite = true
			case isTransactionExecutor(arg):
				out.SawTxCallSite = true
			default:
				out.Findings = append(out.Findings, sharedPrimitiveFinding{
					Kind: findingPrimitiveWithRawPoolHandle,
					Rel:  rel,
					Detail: name + " is called with " + exprText(arg) + ", which is neither a budgeted " +
						"poolMemExecer nor a transaction — a raw pool handle here is a bare " +
						"pool write, the defect class this slice removed",
				})
			}
		}
		return true
	})

	return out
}

// execInterfacesInFile returns the names of the Exec-bearing
// interface types declared in one file.
//
// An interface that exposes an Exec method is an executor abstraction:
// a shared primitive can be written against it, and that is what the
// REACH check keys on.
func execInterfacesInFile(file *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			iface, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				continue
			}
			for _, m := range iface.Methods.List {
				for _, nm := range m.Names {
					if nm.Name == "Exec" {
						out[ts.Name.Name] = true
					}
				}
			}
		}
	}
	return out
}

// sharedPrimitiveClassOf reports whether a function is a shared
// primitive (it takes an Exec-bearing interface parameter) and, if so,
// its class.
//
// The second return value is false when the function is NOT a shared
// primitive; the first is then the empty string.
func sharedPrimitiveClassOf(fn *ast.FuncDecl, execInterfaces map[string]bool, classified map[string]string) (string, bool) {
	for _, p := range funcParams(fn) {
		if execInterfaces[p] {
			return classified[fn.Name.Name], true
		}
	}
	return "", false
}

// dmlStatementIn reports whether a function body contains a SQL
// statement that writes rows.
//
// The statement is matched with its LEADING COMMENTS STRIPPED, so a
// row-writing statement written under an explanatory comment is still
// seen. The FIRST such statement is returned, so the finding names the
// same statement on every run.
func dmlStatementIn(fn *ast.FuncDecl) (string, bool) {
	if fn.Body == nil {
		return "", false
	}
	found := ""
	ok := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if ok {
			return false
		}
		lit, isLit := n.(*ast.BasicLit)
		if !isLit || lit.Kind != token.STRING {
			return true
		}
		s, unqErr := strconv.Unquote(lit.Value)
		if unqErr != nil {
			return true
		}
		if sqlDMLPattern.MatchString(stripLeadingSQLComments(s)) {
			found = stripLeadingSQLComments(s)
			ok = true
		}
		return true
	})
	return found, ok
}

// firstSQLWords renders the first few words of a statement, for an
// error message.
func firstSQLWords(s string) string {
	fields := strings.Fields(s)
	if len(fields) > 6 {
		fields = fields[:6]
	}
	return strings.Join(fields, " ")
}

// funcParams returns the type names of a function's parameters.
//
// Only the bare identifier names are returned — a parameter declared as
// `ex memExecer` yields "memExecer". A parameter declared as a pointer
// or with an inline interface type yields its rendered form, which will
// not match an interface name; that is the fail-closed direction, since
// the REACH check then cannot prove the function is a shared primitive
// and simply does not treat it as one.
func funcParams(fn *ast.FuncDecl) []string {
	if fn.Type == nil || fn.Type.Params == nil {
		return nil
	}
	var out []string
	for _, fld := range fn.Type.Params.List {
		if len(fld.Names) == 0 {
			out = append(out, typeName(fld.Type))
			continue
		}
		for range fld.Names {
			out = append(out, typeName(fld.Type))
		}
	}
	return out
}

// typeName renders a type expression as a bare identifier when it is one.
func typeName(t ast.Expr) string {
	switch x := t.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return typeName(x.X)
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// receiverTypeName returns the name of a method receiver's type.
func receiverTypeName(t ast.Expr) string {
	switch x := t.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return receiverTypeName(x.X)
	}
	return ""
}

// routesThroughExecTracked reports whether a method body calls
// execTracked with the receiver's own `retries` field as the budget.
//
// The budget has to be the FIELD, not a literal: a literal would be a
// second, independent budget that could drift from the one the adapter
// carries.
func routesThroughExecTracked(body *ast.BlockStmt, recvName string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if calleeName(call.Fun) != "execTracked" {
			return true
		}
		if len(call.Args) < 3 {
			return true
		}
		sel, ok := call.Args[2].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "retries" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != recvName {
			return true
		}
		found = true
		return true
	})
	return found
}

// compositeIntField returns the integer value of a named field in a
// composite literal.
func compositeIntField(lit *ast.CompositeLit, field string) (int, bool) {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != field {
			continue
		}
		lit, ok := kv.Value.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			return 0, false
		}
		n, err := strconv.Atoi(lit.Value)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// isPoolMemExecerLiteral reports whether an expression is a
// poolMemExecer composite literal.
func isPoolMemExecerLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	ident, ok := lit.Type.(*ast.Ident)
	return ok && ident.Name == "poolMemExecer"
}

// isTransactionExecutor reports whether an expression names a
// transaction.
//
// The test is deliberately narrow and syntactic: an identifier named
// "tx" (the tree's universal convention for a transaction handle) or a
// .Tx() call. Anything else has to be a budgeted poolMemExecer.
func isTransactionExecutor(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "tx"
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
			return sel.Sel.Name == "Tx"
		}
	case *ast.SelectorExpr:
		return x.Sel.Name == "Tx"
	}
	return false
}

// exprText renders an expression for an error message.
func exprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprText(x.X) + "." + x.Sel.Name
	case *ast.CompositeLit:
		if ident, ok := x.Type.(*ast.Ident); ok {
			return ident.Name + "{...}"
		}
		return "composite literal"
	case *ast.CallExpr:
		return calleeName(x.Fun) + "(...)"
	}
	return "expression"
}

// findingKinds returns the kinds of the findings, sorted, for a
// failure message.
func findingKinds(findings []sharedPrimitiveFinding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Kind)
	}
	sort.Strings(out)
	return out
}

// hasFinding reports whether the scan produced a finding of the given
// kind.
func hasFinding(scan sharedPrimitiveScan, kind string) bool {
	for _, f := range scan.Findings {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// THE SYNTHETIC CONTROLS
// ---------------------------------------------------------------------------
//
// Each control below feeds a SYNTHETIC source through the same
// detection the tree-wide guard uses, and asserts that the expected
// finding appears (or does not). A control fails when its protection
// is disabled — which is what the independent review of 2026-11-06
// found the tree-only assertions could not do.
//
// The synthetic sources are deliberately small. They exist to pin the
// DETECTION, not to restate the runtime.

// TestPoolMemExecerRoutesThroughTheTrackedPolicy is the synthetic
// control for the ADAPTER assertion.
//
// The record named this control before it existed. It is here now, and
// it pins the thing it names: the adapter's Exec must route its write
// through execTracked with the adapter's OWN retries field.
//
// Two synthetic adapters:
//
//   - one that spends a LITERAL budget (`execTracked(p.db, query, 5,
//     …)`) must be FLAGGED. A literal is a second, independent budget
//     that can drift from the one the adapter carries — and the whole
//     point of the adapter is that the budget it carries is the budget
//     it spends.
//   - one that spends its OWN field (`execTracked(p.db, query,
//     p.retries, …)`) must NOT be flagged.
//
// Without this control, replacing the field with a literal — the
// reviewer's mutation R3 — leaves the suite green.
func TestPoolMemExecerRoutesThroughTheTrackedPolicy(t *testing.T) {
	const adapterTemplate = `package p

import "database/sql"

type poolMemExecer struct {
	db      *sql.DB
	retries int
}

func (p poolMemExecer) Exec(query string, args ...any) (sql.Result, error) {
	out := execTracked(p.db, query, %s, nil, args...)
	return out.Result, out.Err
}
`

	fieldBudget := strings.Replace(adapterTemplate, "%s", "p.retries", 1)
	literalBudget := strings.Replace(adapterTemplate, "%s", "5", 1)

	// The adapter that spends its own field: no finding at all.
	scanField := scanSharedPrimitiveSource("internal/core/probe.go", []byte(fieldBudget), nil, nil)
	if len(scanField.Findings) != 0 {
		t.Fatalf("an adapter that routes through execTracked with its own retries field "+
			"must not be flagged, got findings %v: %+v",
			findingKinds(scanField.Findings), scanField.Findings)
	}
	if !scanField.SawTrackedAdapter {
		t.Fatal("the synthetic adapter was not recognised as the tracked adapter: " +
			"the scan reached nothing, so the negative case below would prove nothing")
	}

	// The adapter that spends a LITERAL: must be flagged as dropping
	// its budget.
	scanLiteral := scanSharedPrimitiveSource("internal/core/probe.go", []byte(literalBudget), nil, nil)
	if !hasFinding(scanLiteral, findingAdapterDropsBudget) {
		t.Fatalf("an adapter that spends a LITERAL budget instead of its own retries field "+
			"must be flagged as dropping its budget, got findings %v: %+v",
			findingKinds(scanLiteral.Findings), scanLiteral.Findings)
	}
	if len(scanLiteral.Findings) != 1 {
		t.Fatalf("expected exactly one finding for the literal-budget adapter, got %v: %+v",
			findingKinds(scanLiteral.Findings), scanLiteral.Findings)
	}
}

// TestSharedPrimitiveReachDetectsUnclassifiedPrimitive is the
// synthetic control for the REACH assertion.
//
// The guard's reach check is what stops a shared primitive from
// appearing without a classification — and without a classification
// there is no budget on its pool path. The detection used to live only
// inside the tree walk, so switching it off (the reviewer's probe:
// `isShared = false`) left the suite green.
//
// Two scans of the same synthetic source:
//
//   - with an EMPTY classification map, the primitive must be flagged
//     as unclassified;
//   - with a classification, the same source must NOT be flagged —
//     which is what proves the classification is what silences the
//     finding, rather than the source being uninteresting.
func TestSharedPrimitiveReachDetectsUnclassifiedPrimitive(t *testing.T) {
	const src = `package p

import "database/sql"

type memExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

func unclassifiedPrimitive(ex memExecer) error {
	_, err := ex.Exec("INSERT INTO probe_target VALUES (?)", 1)
	return err
}
`

	// Unclassified: the reach check must fire.
	scanUnclassified := scanSharedPrimitiveSource("internal/core/probe.go", []byte(src), nil, map[string]string{})
	if !hasFinding(scanUnclassified, findingUnclassifiedPrimitive) {
		t.Fatalf("a function that takes an Exec-bearing interface and has no classification "+
			"must be flagged as unclassified, got findings %v: %+v",
			findingKinds(scanUnclassified.Findings), scanUnclassified.Findings)
	}
	if len(scanUnclassified.Findings) != 1 {
		t.Fatalf("expected exactly one finding for the unclassified primitive, got %v: %+v",
			findingKinds(scanUnclassified.Findings), scanUnclassified.Findings)
	}

	// Classified: the same source must be clean. If it were not, the
	// control above would pass for the wrong reason.
	scanClassified := scanSharedPrimitiveSource("internal/core/probe.go", []byte(src), nil,
		map[string]string{"unclassifiedPrimitive": classPoolWrite})
	if len(scanClassified.Findings) != 0 {
		t.Fatalf("a CLASSIFIED shared primitive must not be flagged, got findings %v: %+v",
			findingKinds(scanClassified.Findings), scanClassified.Findings)
	}
}

// TestSharedPrimitiveDDLOnlyDetectsRowWritingPrimitive is the
// synthetic control for the DDL-ONLY assertion.
//
// A primitive classified as migration DDL is allowed to pass a raw
// pool handle on its pool path, because a retry of `CREATE TRIGGER`
// fails with "already exists" — so the transaction, not the budget,
// is what makes that atomic. The classification is only honest while
// the primitive writes NO ROWS.
//
// The control proves the DDL-only detection fires: a classMigrationDDL
// primitive that writes a row must be flagged. The same primitive
// classified as classPoolWrite must not be.
func TestSharedPrimitiveDDLOnlyDetectsRowWritingPrimitive(t *testing.T) {
	const src = `package p

import "database/sql"

type execQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func migrationPrimitive(q execQuerier) error {
	_, err := q.Exec("INSERT INTO lessons_base (id) VALUES (?)", 1)
	return err
}
`

	scanDDL := scanSharedPrimitiveSource("internal/core/probe.go", []byte(src), nil,
		map[string]string{"migrationPrimitive": classMigrationDDL})
	if !hasFinding(scanDDL, findingMigrationDDLWithDML) {
		t.Fatalf("a classMigrationDDL primitive that writes a row must be flagged, "+
			"got findings %v: %+v",
			findingKinds(scanDDL.Findings), scanDDL.Findings)
	}
	if len(scanDDL.Findings) != 1 {
		t.Fatalf("expected exactly one finding for the row-writing DDL primitive, got %v: %+v",
			findingKinds(scanDDL.Findings), scanDDL.Findings)
	}

	scanPool := scanSharedPrimitiveSource("internal/core/probe.go", []byte(src), nil,
		map[string]string{"migrationPrimitive": classPoolWrite})
	if len(scanPool.Findings) != 0 {
		t.Fatalf("a classPoolWrite primitive may write rows and must not be flagged by the "+
			"DDL-only check, got findings %v: %+v",
			findingKinds(scanPool.Findings), scanPool.Findings)
	}
}

// TestSharedPrimitiveDDLOnlySeesThroughLeadingComments is the control
// for review finding M3.
//
// The DDL-only check used to match `^\s*(INSERT INTO|UPDATE …|DELETE
// FROM|REPLACE INTO)`. `^\s*` matched only WHITESPACE, so a
// row-writing statement written under an explanatory comment did not
// match — and the assertion could be defeated by adding a comment.
//
// That is not a theoretical bypass: a migration author writes the
// comment, and the comment is exactly what a reviewer would not look
// past.
//
// The control asserts:
//
//   - a row-writing statement with a leading line comment, a leading
//     block comment, leading whitespace, or a combination of them IS
//     flagged, for INSERT, UPDATE and DELETE;
//   - ordinary DDL is NOT flagged — including a `CREATE TRIGGER` whose
//     body contains a DELETE, because that DELETE is part of the
//     schema definition and not a row the primitive writes.
func TestSharedPrimitiveDDLOnlySeesThroughLeadingComments(t *testing.T) {
	const template = `package p

import "database/sql"

type execQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func migrationPrimitive(q execQuerier) error {
	_, err := q.Exec(%s, 1)
	return err
}
`

	rowWriting := []struct {
		name string
		stmt string
	}{
		{"a plain INSERT", `"INSERT INTO lessons_base (id) VALUES (?)"`},
		{"an INSERT behind a line comment", "\"-- the backfill writes rows\\nINSERT INTO lessons_base (id) VALUES (?)\""},
		{"an INSERT behind a block comment", "\"/* the backfill writes rows */\\nINSERT INTO lessons_base (id) VALUES (?)\""},
		{"an INSERT behind whitespace and a line comment", "\"\\n\\t  -- explain\\n  INSERT INTO lessons_base (id) VALUES (?)\""},
		{"an INSERT behind two comments", "\"-- one\\n-- two\\nINSERT INTO lessons_base (id) VALUES (?)\""},
		{"an UPDATE behind a line comment", "\"-- widen the window\\nUPDATE lessons_base SET weight = ? WHERE id = ?\""},
		{"a DELETE behind a block comment", "\"/* sweep the old rows */\\nDELETE FROM lessons_base WHERE id = ?\""},
	}
	for _, tc := range rowWriting {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(template, "%s", tc.stmt, 1)
			scan := scanSharedPrimitiveSource("internal/core/probe.go", []byte(src), nil,
				map[string]string{"migrationPrimitive": classMigrationDDL})
			if !hasFinding(scan, findingMigrationDDLWithDML) {
				t.Fatalf("a row-writing statement behind a comment must be flagged, got findings %v: %+v",
					findingKinds(scan.Findings), scan.Findings)
			}
		})
	}

	notRowWriting := []struct {
		name string
		stmt string
	}{
		{"a CREATE VIEW", `"CREATE VIEW IF NOT EXISTS lessons AS SELECT * FROM lessons_base"`},
		{"a CREATE TABLE", `"CREATE TABLE IF NOT EXISTS lessons_base (id TEXT PRIMARY KEY)"`},
		{
			// The row-writing DML inside a trigger body is part of
			// the SCHEMA definition. The primitive does not write that
			// row; the trigger does, when the view is written to. So
			// the statement stays classified as DDL.
			"a CREATE TRIGGER whose body deletes",
			"\"CREATE TRIGGER lessons_instead_of_delete\\nINSTEAD OF DELETE ON lessons\\nBEGIN\\n\\tDELETE FROM lessons_base WHERE id = OLD.id;\\nEND\"",
		},
		{
			"a CREATE TRIGGER behind a comment",
			"\"-- the delete trigger\\nCREATE TRIGGER lessons_instead_of_delete\\nINSTEAD OF DELETE ON lessons\\nBEGIN\\n\\tDELETE FROM lessons_base WHERE id = OLD.id;\\nEND\"",
		},
	}
	for _, tc := range notRowWriting {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(template, "%s", tc.stmt, 1)
			scan := scanSharedPrimitiveSource("internal/core/probe.go", []byte(src), nil,
				map[string]string{"migrationPrimitive": classMigrationDDL})
			if hasFinding(scan, findingMigrationDDLWithDML) {
				t.Fatalf("ordinary DDL must not be flagged as a row write, got findings %v: %+v",
					findingKinds(scan.Findings), scan.Findings)
			}
		})
	}
}

// TestSQLDMLPatternSeesThroughLeadingComments is the unit-level
// control for the comment stripping itself, so the behaviour is pinned
// independently of the guard that consumes it.
//
// It also pins the ANCHOR: the pattern must match at the START of the
// statement (after the comments), not anywhere inside it — otherwise a
// trigger body would be mistaken for a row write.
func TestSQLDMLPatternSeesThroughLeadingComments(t *testing.T) {
	// The stripping itself, pinned independently of the pattern: a
	// comment must be REMOVED, not merely tolerated.
	stripped := []struct {
		in   string
		want string
	}{
		{"-- a comment\nINSERT INTO t VALUES (?)", "INSERT INTO t VALUES (?)"},
		{"/* a comment */\nINSERT INTO t VALUES (?)", "INSERT INTO t VALUES (?)"},
		{"/* a\nmultiline\ncomment */\nINSERT INTO t VALUES (?)", "INSERT INTO t VALUES (?)"},
		{"-- one\n/* two */\n\tINSERT INTO t VALUES (?)", "INSERT INTO t VALUES (?)"},
		{"  \n\t INSERT INTO t VALUES (?)", "INSERT INTO t VALUES (?)"},
		{"-- a comment", ""},
		{"/* unterminated", ""},
		{"INSERT INTO t VALUES (?)", "INSERT INTO t VALUES (?)"},
	}
	for _, tc := range stripped {
		if got := stripLeadingSQLComments(tc.in); got != tc.want {
			t.Errorf("stripLeadingSQLComments(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	rowWriting := []string{
		"INSERT INTO t VALUES (?)",
		"  \n\t INSERT INTO t VALUES (?)",
		"-- a comment\nINSERT INTO t VALUES (?)",
		"-- a comment\n-- another\nINSERT INTO t VALUES (?)",
		"/* a comment */\nINSERT INTO t VALUES (?)",
		"/* a\nmultiline\ncomment */\nINSERT INTO t VALUES (?)",
		"/* one */ -- two\nINSERT INTO t VALUES (?)",
		"-- one\n/* two */\n\tINSERT INTO t VALUES (?)",
		"UPDATE t SET a = 1",
		"-- a comment\nUPDATE t SET a = 1",
		"/* a comment */\nUPDATE t SET a = 1",
		"DELETE FROM t",
		"-- a comment\nDELETE FROM t",
		"/* a comment */\nDELETE FROM t",
		"REPLACE INTO t VALUES (1)",
		"-- a comment\nREPLACE INTO t VALUES (1)",
	}
	for _, stmt := range rowWriting {
		if !sqlDMLPattern.MatchString(stripLeadingSQLComments(stmt)) {
			t.Errorf("a row-writing statement must be recognised: %q", stmt)
		}
	}

	notRowWriting := []string{
		"CREATE TABLE t (a)",
		"-- a comment\nCREATE TABLE t (a)",
		"/* a comment */\nCREATE VIEW v AS SELECT * FROM t",
		"CREATE TRIGGER trg BEGIN DELETE FROM t; END",
		"-- a comment\nCREATE TRIGGER trg BEGIN DELETE FROM t; END",
		"SELECT * FROM t",
		"-- just a comment",
		"/* just a comment */",
		"-- a comment\n-- and another",
		"PRAGMA wal_checkpoint(TRUNCATE)",
	}
	for _, stmt := range notRowWriting {
		if sqlDMLPattern.MatchString(stripLeadingSQLComments(stmt)) {
			t.Errorf("a statement that does not write rows must not be recognised as one: %q", stmt)
		}
	}
}
