package internal

// tx_write_budget_guard_test.go — issue #164's audit, kept discharged.
//
// WHY THIS FILE EXISTS
// --------------------
// Issue #164's third option was: "Audit every `retries=0` write call site
// and decide, per site, whether it genuinely cannot contend."
//
// PR #169 corrected the sites the issue named and claimed the audit was
// done. It was not: the substrate still had twenty-six `retries=0`
// writes, and two of them were the tool-surface scratchpad flush and
// discard. The claim was wrong, and a claim like that is only worth
// something if something makes it true.
//
// This file is that something. It is a STRUCTURAL guard over the whole
// substrate: it parses every non-test Go file under internal/core and
// fails when a WRITE passes a zero retry budget to ExecTracked.
//
// WHY A ZERO BUDGET IS A DEFECT AND NOT A STYLE CHOICE
// -----------------------------------------------------
// `ExecTracked(query, retries, ...)` retries the statement on an ordinary
// SQLITE_BUSY when `retries` allows it. `retries=0` disables the
// statement-level retry entirely. Ordinary contention IS a wait and IS
// clearable by retrying the statement, so a zero budget does not mean
// "this write cannot be contended" — it means "if this write meets
// contention, the artifact is lost".
//
// Issue #164 said it plainly: "several call sites read `0` as 'this
// cannot be contended', which is not true of any write."
//
// THE ONE EXEMPTION, AND WHY IT IS AN EXEMPTION
// ----------------------------------------------
// A write is exempt only when an OUTER layer owns the same busy class
// for that path. There is exactly one such place in the substrate:
// internal/core/reference_db.go, whose writes run inside
// `IngestReference`'s five-attempt retry loop. That loop re-drives the
// whole operation on an ordinary busy error and re-resolves the identity
// each attempt, so the loser of a concurrent ingest applies an update
// instead of failing.
//
// A statement-level budget THERE would multiply the two budgets — five
// outer attempts times six inner attempts = thirty transactions, where
// the outer budget says five — for no additional resilience. That is the
// multiplication trap AGENTS.md §3 names, and it is why the exemption is
// keyed to the exact statements rather than to the file: a NEW write in
// that file, with different SQL, is caught by this guard like any other.
//
// HOW THE SCAN WORKS
// ------------------
// go/parser and go/ast, not a regular expression. Two reasons, and both
// of them were learned the hard way:
//
//   - A regex cannot tell a retry budget of `0` from a `, 0)` inside a
//     SQL string literal. The first draft of this audit did exactly that
//     and reported a false positive on capability/store_invocation.go,
//     whose invocation INSERT carries
//     `cascade_invalidated) VALUES (?, ?, ?, ?, ?, ?, ?, 0)` in its SQL.
//
//   - A regex cannot see a statement whose SQL is a VARIABLE. The
//     scratchpad flush — one of the two sites this audit was opened for
//     — passes a `const query` rather than a literal, so a scan that
//     only reads literals misses it entirely. The AST knows the
//     difference; the scanner below resolves the identifier to its
//     string value first.
//
// An exempted statement is named by a DISTINCTIVE FRAGMENT of its SQL,
// matched against the normalised statement text. A fragment rather than
// the whole statement: the substrate builds some of these statements
// from expressions (COALESCE(NULLIF(...)), and so on), and a
// fingerprint of the whole text would have to be copied by hand — which
// is how an exemption silently stops matching the day someone edits a
// column list. The fragments are specific enough that a NEW write in
// the same file is not covered by them.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// writeVerb matches a SQL statement that WRITES.
var writeVerb = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|REPLACE)\b`)

// exemptedWriteBudget is the set of statements allowed to carry a zero
// retry budget, keyed by the file they live in (relative to
// internal/core) and the distinctive SQL fragment they execute.
//
// The reason each one is exempt is recorded at the call site; this map
// only names them, so the guard can tell "an exemption the author
// wrote down" from "an exemption nobody wrote down".
var exemptedWriteBudget = map[string][]string{
	"reference_db.go": {
		// The doc-row upsert.
		"INSERT INTO reference_docs",
		// The inserted-chunks upsert.
		"INSERT INTO reference_chunks (id, doc_id, chunk_index, section, content, source_path, content_hash) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING",
		// The updated-chunks upsert.
		"INSERT INTO reference_chunks (id, doc_id, chunk_index, section, content, source_path, content_hash) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET",
		// The orphan-chunk delete.
		"DELETE FROM reference_chunks WHERE id = ?",
	},
}

// normalizeSQL collapses whitespace so a fingerprint survives
// reindentation of the statement.
func normalizeSQL(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// sqlFingerprint renders a SQL expression as a comparable string.
//
// A plain string literal is itself the fingerprint. A concatenation (the
// substrate builds a few statements from a table name) is rendered by
// substituting its parts. A `fmt.Sprintf` is fingerprinted by its FORMAT
// string, which is what distinguishes two statements that differ only in
// a table name.
//
// An identifier is resolved by the caller, through the scope map, before
// it reaches here.
func sqlFingerprint(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			s, err := strconv.Unquote(e.Value)
			if err != nil {
				return e.Value
			}
			return normalizeSQL(s)
		}
		return e.Value
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return sqlFingerprint(e.X) + " " + sqlFingerprint(e.Y)
		}
	case *ast.CallExpr:
		if len(e.Args) > 0 {
			return sqlFingerprint(e.Args[0])
		}
	}
	return ""
}

// stringScope collects the string-valued bindings visible to a
// statement: package-level constants, and the local constants and
// variables of the enclosing function.
//
// It exists for one reason: the scratchpad flush passes its SQL as a
// `const query`, and a scanner that only reads string LITERALS misses
// it — which is how a site with a zero budget can sit in the tree and
// pass a guard that claims to cover every site.
func stringScope(decls []ast.Decl) map[string]string {
	scope := map[string]string{}

	add := func(name string, value ast.Expr) {
		if s := sqlFingerprint(value); s != "" {
			scope[name] = s
		}
	}

	for _, decl := range decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Values) != len(vs.Names) {
					continue
				}
				for i, name := range vs.Names {
					if name.Name != "_" {
						add(name.Name, vs.Values[i])
					}
				}
			}
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			ast.Inspect(d.Body, func(n ast.Node) bool {
				switch s := n.(type) {
				case *ast.AssignStmt:
					if len(s.Lhs) != len(s.Rhs) {
						return true
					}
					for i, lhs := range s.Lhs {
						if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
							add(id.Name, s.Rhs[i])
						}
					}
				case *ast.ValueSpec:
					if len(s.Values) != len(s.Names) {
						return true
					}
					for i, name := range s.Names {
						if name.Name != "_" {
							add(name.Name, s.Values[i])
						}
					}
				}
				return true
			})
		}
	}
	return scope
}

// resolveSQL renders the SQL a call executes, following an identifier
// to its binding when the statement passes a variable.
func resolveSQL(expr ast.Expr, scope map[string]string) string {
	if id, ok := expr.(*ast.Ident); ok {
		if s, found := scope[id.Name]; found {
			return s
		}
		return ""
	}
	return sqlFingerprint(expr)
}

// TestEverySubstrateWriteHasAStatementLevelRetryBudget is the guard.
//
// It walks the whole substrate — internal/core and everything under it,
// including the nested tools module — and fails when a write passes a
// zero retry budget to ExecTracked without a recorded exemption.
//
// This is the standing proof that issue #164's third option stays
// discharged. A new write that reaches for `retries=0` fails here
// rather than shipping a lost-artifact path.
func TestEverySubstrateWriteHasAStatementLevelRetryBudget(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller must resolve this test file")
	}
	root := filepath.Dir(thisFile)

	var offenders []string
	var examined int

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "node_modules" || base == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		scope := stringScope(file.Decls)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "ExecTracked" {
					return true
				}
				if len(call.Args) < 2 {
					return true
				}
				// The budget must be the literal 0.
				lit, ok := call.Args[1].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT || lit.Value != "0" {
					return true
				}
				// The first argument must be a write.
				sql := resolveSQL(call.Args[0], scope)
				if !writeVerb.MatchString(sql) {
					return true
				}
				examined++
				for _, exempt := range exemptedWriteBudget[rel] {
					if strings.Contains(sql, exempt) {
						return true
					}
				}
				pos := fset.Position(call.Args[1].Pos())
				offenders = append(offenders, fmt.Sprintf("%s:%d", rel, pos.Line))
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the substrate: %v", err)
	}

	if examined == 0 {
		t.Fatal("no write call site was examined: the scan found nothing, so this test proves nothing")
	}
	if len(offenders) > 0 {
		t.Fatalf("these writes pass a ZERO retry budget to ExecTracked, so an ordinary lock "+
			"contention loses the artifact instead of waiting it out (issue #164). Give them "+
			"the documented budget of 5, or record the exemption in exemptedWriteBudget with "+
			"the reason the outer layer owns that busy class:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
