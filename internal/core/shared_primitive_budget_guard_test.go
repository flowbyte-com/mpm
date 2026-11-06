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

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
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

// sqlDMLPattern matches a SQL statement that WRITES ROWS.
//
// The object keyword is required so that an English sentence in an
// error message ("create lessons view failed") is not mistaken for a
// statement. Only the verbs that mutate rows are listed: a
// migration-DDL primitive that starts executing one of these is
// misclassified, and the guard says so.
var sqlDMLPattern = regexp.MustCompile(`(?is)^\s*(INSERT\s+INTO|UPDATE\s+[a-z_]|DELETE\s+FROM|REPLACE\s+INTO)`)

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
						if nm.Name != "Exec" {
							continue
						}
						if execInterfacesByDir[dir] == nil {
							execInterfacesByDir[dir] = map[string]bool{}
						}
						execInterfacesByDir[dir][ts.Name.Name] = true
					}
				}
			}
		}
	}

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
		dir := filepath.Dir(path)
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
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
					sawTrackedAdapter = true
					if !routesThroughExecTracked(node.Body, node.Recv.List[0].Names[0].Name) {
						t.Errorf("%s: poolMemExecer.Exec does not route its write through "+
							"execTracked with the adapter's own retries field — the budget "+
							"would be carried and then silently dropped", rel)
					}
				}

				// REACH: a function that takes an executor interface is
				// a shared primitive, whether or not anyone has
				// remembered to classify it.
				class, isShared := sharedPrimitiveClassOf(node, execInterfacesByDir[dir])
				if !isShared {
					return true
				}
				if class == "" {
					t.Errorf("%s: %s takes an executor interface, so its pool path "+
						"needs a classification — add it to sharedPrimitives so the "+
						"guard checks it (a pool-write primitive needs a statement "+
						"budget; a migration-DDL primitive must execute no DML)",
						rel, node.Name.Name)
					return true
				}
				if class == classMigrationDDL {
					if stmt, ok := dmlStatementIn(node); ok {
						t.Errorf("%s: %s is classified as %s, but it executes the "+
							"row-writing statement %q — a pool-write primitive needs a "+
							"statement budget on its pool path, so reclassify it",
							rel, node.Name.Name, class, firstSQLWords(stmt))
					}
				}
			case *ast.CompositeLit:
				if ident, ok := node.Type.(*ast.Ident); !ok || ident.Name != "poolMemExecer" {
					return true
				}
				sawBudgetedExecutor = true
				retries, ok := compositeIntField(node, "retries")
				if !ok {
					t.Errorf("%s: a poolMemExecer is built without a resolvable retries budget — "+
						"the pool path would carry no budget at all", rel)
					return true
				}
				if retries <= 0 {
					t.Errorf("%s: a poolMemExecer is built with a retry budget of %d — "+
						"a zero budget is the issue #164 defect class: the statement-level retry "+
						"is disabled and the write is lost to ordinary contention", rel, retries)
				}
			case *ast.CallExpr:
				name := calleeName(node.Fun)
				class, ok := sharedPrimitives[name]
				if !ok || class != classPoolWrite {
					return true
				}
				if len(node.Args) == 0 {
					t.Errorf("%s: %s is called with no executor", rel, name)
					return true
				}
				arg := node.Args[0]
				switch {
				case isPoolMemExecerLiteral(arg):
					sawPoolCallSite = true
				case isTransactionExecutor(arg):
					sawTxCallSite = true
				default:
					t.Errorf("%s: %s is called with %s, which is neither a budgeted "+
						"poolMemExecer nor a transaction — a raw pool handle here is a bare "+
						"pool write, the defect class this slice removed",
						rel, name, exprText(arg))
				}
			}
			return true
		})
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

// sharedPrimitiveClassOf reports whether a function is a shared
// primitive (it takes an Exec-bearing interface parameter) and, if so,
// its class.
//
// The second return value is false when the function is NOT a shared
// primitive; the first is then the empty string.
func sharedPrimitiveClassOf(fn *ast.FuncDecl, execInterfaces map[string]bool) (string, bool) {
	for _, p := range funcParams(fn) {
		if execInterfaces[p] {
			return sharedPrimitives[fn.Name.Name], true
		}
	}
	return "", false
}

// dmlStatementIn reports whether a function body contains a SQL
// statement that writes rows.
func dmlStatementIn(fn *ast.FuncDecl) (string, bool) {
	if fn.Body == nil {
		return "", false
	}
	found := ""
	ok := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, isLit := n.(*ast.BasicLit)
		if !isLit || lit.Kind != token.STRING {
			return true
		}
		s, unqErr := strconv.Unquote(lit.Value)
		if unqErr != nil {
			return true
		}
		if sqlDMLPattern.MatchString(s) {
			found = s
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
