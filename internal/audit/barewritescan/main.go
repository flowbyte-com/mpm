// Command barewritescan inventories the direct database writes in a Go
// tree and classifies each one by the protection it actually has.
//
// It is an AST scan, not a regular expression, because a regex cannot
// tell a retry budget of 0 from a ", 0)" inside a SQL string literal,
// and it cannot see a statement whose SQL is a VARIABLE.
//
// WHY IT EXISTS
// -------------
// Issue #178 asked for a complete inventory of the direct writes that
// bypass ExecTracked's retry budget. The audit is recorded in
// docs/dev/bare-write-risk-assessment-2026-10-29.md. This command is
// the tool that produced the inventory, kept in the tree so the numbers
// can be re-derived and a regression can be seen.
//
// WHAT IT REPORTS
// ---------------
// One line per write call site:
//
//	kind    pool | tx | guarded
//	                 pool    — a bare pool write (dm.db.Exec, dm.SQLDB().Exec)
//	                 tx      — a write owned by an open transaction
//	                 guarded — ExecTracked(query, retries, ...)
//	budget  the retry budget, for guarded sites (-1 otherwise)
//	err     checked | ignored — is the error result bound?
//	prop    propagated | swallowed | dropped — does the caller ever see it?
//	fnerr   yes | no — does the enclosing function return an error?
//	tx      true | false — is the site lexically inside a WithTx callback?
//
// THE COLUMNS THAT DECIDE RISK
// -----------------------------
// `err` alone does not decide it. A site that BINDS the error but never
// returns it is not the same as a site that propagates it: the first
// loses the artifact silently, the second at least tells the caller.
// `prop` is the column that separates them.
//
// The propagation decision is STATEMENT-SCOPED — it looks at the
// `if err != nil { ... }` that immediately follows the write — because a
// function-level check mis-classifies any function that contains both a
// propagating and a non-propagating write.
//
// IT IS NOT A GATE
// ----------------
// This command reports; it does not fail a build. Turning it into one
// is a separate decision (the audit document recommends a ratchet, not
// a zero-tolerance gate, because the current baseline is 172 bare pool
// writes).
//
// Usage:
//
//	go run ./internal/audit/barewritescan <root>...
//
// Example:
//
//	go run ./internal/audit/barewritescan internal cmd

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	writeVerb = regexp.MustCompile(`(?i)^\s*(INSERT|UPDATE|DELETE|REPLACE|CREATE|ALTER|DROP)\b`)
	tableRE   = regexp.MustCompile(`(?i)(?:INTO|FROM|UPDATE)\s+([A-Za-z_][A-Za-z0-9_]*)`)
)

type Site struct {
	Kind     string
	File     string
	Line     int
	Func     string
	Verb     string
	Table    string
	SQL      string
	Receiver string
	ErrHand  string
	Budget   int
	Prop     string
	FnErr    string
	InWithTx bool
}

func normalizeSQL(s string) string { return strings.Join(strings.Fields(s), " ") }

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

func stringScope(decls []ast.Decl) map[string]string {
	scope := map[string]string{}
	add := func(name string, value ast.Expr) {
		if name == "_" {
			return
		}
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
					add(name.Name, vs.Values[i])
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
						if id, ok := lhs.(*ast.Ident); ok {
							add(id.Name, s.Rhs[i])
						}
					}
				case *ast.ValueSpec:
					if len(s.Values) != len(s.Names) {
						return true
					}
					for i, name := range s.Names {
						add(name.Name, s.Values[i])
					}
				}
				return true
			})
		}
	}
	return scope
}

func resolveSQL(expr ast.Expr, scope map[string]string) string {
	if id, ok := expr.(*ast.Ident); ok {
		if s, found := scope[id.Name]; found {
			return s
		}
		return ""
	}
	return sqlFingerprint(expr)
}

// isTxReceiver reports whether the receiver of a call is a transaction
// rather than a pool.
//
// The substrate names transactions `tx` and pools `db`/`DB`/`SQLDB`, and
// a transaction's writes are transaction-owned even when the tx was
// passed in as a parameter rather than opened lexically here.
func isTxReceiver(recv ast.Expr) bool {
	switch e := recv.(type) {
	case *ast.Ident:
		n := strings.ToLower(e.Name)
		return strings.Contains(n, "tx")
	case *ast.SelectorExpr:
		switch e.Sel.Name {
		case "db", "DB", "SQLDB":
			return false
		}
		return isTxReceiver(e.X)
	}
	return false
}

func receiverName(recv ast.Expr) string {
	switch e := recv.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return receiverName(e.X) + "." + e.Sel.Name
	case *ast.CallExpr:
		if len(e.Args) > 0 {
			return receiverName(e.Args[0])
		}
		return "call()"
	}
	return "?"
}

// fnReturnsError reports whether the function declares an error result.
func fnReturnsError(fd *ast.FuncDecl) string {
	if fd.Type.Results == nil {
		return "no"
	}
	for _, r := range fd.Type.Results.List {
		if id, ok := r.Type.(*ast.Ident); ok && id.Name == "error" {
			return "yes"
		}
	}
	return "no"
}

// propagation decides whether a captured error actually reaches the
// caller, or is swallowed (logged and dropped) inside the function.
//
// It is STATEMENT-SCOPED first: the canonical shape is
//
//	_, err := db.Exec(...)
//	if err != nil { return nil, fmt.Errorf("...: %w", err) }
//
// so the decision is made by looking at the statement that FOLLOWS the
// write, not by scanning the whole function. A function-level scan
// over-reports "propagated" whenever one write in a function propagates
// and a second one does not — which is exactly the shape of the
// silent-loss sites this scan exists to find.
//
// It then falls back to a function-level check (the error identifier is
// visibly returned or wrapped) for the shapes that do not use the
// canonical if-immediately-after form.
func propagation(fd *ast.FuncDecl, errIdent string) string {
	if errIdent == "" {
		return "n/a"
	}
	propagated := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.ReturnStmt:
			for _, r := range st.Results {
				if mentionsIdent(r, errIdent) {
					propagated = true
				}
			}
		case *ast.CallExpr:
			// fmt.Errorf("...: %w", err) — a wrapped error.
			if sel, ok := st.Fun.(*ast.SelectorExpr); ok {
				if sel.Sel.Name == "Errorf" {
					for _, a := range st.Args {
						if mentionsIdent(a, errIdent) {
							propagated = true
						}
					}
				}
			}
		}
		return true
	})
	if propagated {
		return "propagated"
	}
	return "swallowed"
}

// stmtPropagation decides propagation from the statement that FOLLOWS
// the write.
//
// The canonical shape is:
//
//	_, err := db.Exec(...)
//	if err != nil { return ..., fmt.Errorf("...: %w", err) }
//
// so the statement-scoped answer is: is the next statement an `if` whose
// condition tests the error identifier, and does its body return?
//
// Returns "unknown" when the shape is not recognisable, so the caller
// can fall back to the function-level check.
func stmtPropagation(fd *ast.FuncDecl, errIdent string, writeStmt ast.Stmt) string {
	if writeStmt == nil {
		return "unknown"
	}
	// Form 1: `if _, err := db.Exec(...); err != nil { ... }` — the write
	// is the Init of the if-statement itself.
	ifs := enclosingIfInit(fd.Body, writeStmt)
	if ifs == nil {
		// Form 2: the write is its own statement; the `if` that tests
		// the error is the statement that FOLLOWS it.
		next := nextStatement(fd.Body, writeStmt)
		if next == nil {
			return "unknown"
		}
		var ok bool
		ifs, ok = next.(*ast.IfStmt)
		if !ok {
			return "unknown"
		}
	}
	if !mentionsIdent(ifs.Cond, errIdent) {
		return "unknown"
	}
	// The `if err != nil` body: does it return?
	returned := false
	ast.Inspect(ifs.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.ReturnStmt); ok {
			returned = true
		}
		return true
	})
	if returned {
		return "propagated"
	}
	// The error was tested but not returned — logged and dropped.
	return "swallowed"
}

// enclosingIfInit finds the `if` statement whose Init is the given
// statement — the `if _, err := f(); err != nil { ... }` shape.
func enclosingIfInit(root ast.Node, target ast.Stmt) *ast.IfStmt {
	var found *ast.IfStmt
	ast.Inspect(root, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		if ifs, ok := n.(*ast.IfStmt); ok {
			if ifs.Init == target {
				found = ifs
				return false
			}
		}
		return true
	})
	return found
}

// nextStatement finds the statement that follows `target` in the
// statement list that contains it, at any nesting depth.
func nextStatement(root ast.Node, target ast.Stmt) ast.Stmt {
	var found ast.Stmt
	var walk func(list []ast.Stmt) bool
	walk = func(list []ast.Stmt) bool {
		for i, st := range list {
			if st == target {
				if i+1 < len(list) {
					found = list[i+1]
				}
				return true
			}
			// Recurse into the statement's own sub-lists.
			switch t := st.(type) {
			case *ast.BlockStmt:
				if walk(t.List) {
					return true
				}
			case *ast.IfStmt:
				if walk(t.Body.List) {
					return true
				}
				if t.Else != nil {
					if blk, ok := t.Else.(*ast.BlockStmt); ok {
						if walk(blk.List) {
							return true
						}
					}
				}
			case *ast.ForStmt:
				if walk(t.Body.List) {
					return true
				}
			case *ast.RangeStmt:
				if walk(t.Body.List) {
					return true
				}
			case *ast.SwitchStmt:
				for _, cc := range t.Body.List {
					blk, ok := cc.(*ast.CaseClause)
					if !ok {
						continue
					}
					if walk(blk.Body) {
						return true
					}
				}
			case *ast.TypeSwitchStmt:
				for _, cc := range t.Body.List {
					blk, ok := cc.(*ast.CaseClause)
					if !ok {
						continue
					}
					if walk(blk.Body) {
						return true
					}
				}
			case *ast.SelectStmt:
				for _, cc := range t.Body.List {
					blk, ok := cc.(*ast.CaseClause)
					if !ok {
						continue
					}
					if walk(blk.Body) {
						return true
					}
				}
			case *ast.LabeledStmt:
				if walk([]ast.Stmt{t.Stmt}) {
					return true
				}
			}
		}
		return false
	}
	switch r := root.(type) {
	case *ast.BlockStmt:
		walk(r.List)
	case *ast.FuncDecl:
		if r.Body != nil {
			walk(r.Body.List)
		}
	}
	return found
}

// mentionsIdent reports whether the expression mentions the identifier.
func mentionsIdent(e ast.Expr, name string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return true
	})
	return found
}

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{"."}
	}
	var sites []Site

	for _, root := range roots {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return nil
			}
			scope := stringScope(f.Decls)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				// Positions covered by a WithTx callback literal.
				inWithTx := map[token.Pos]bool{}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "WithTx" {
						return true
					}
					for _, arg := range call.Args {
						fl, ok := arg.(*ast.FuncLit)
						if !ok {
							continue
						}
						ast.Inspect(fl.Body, func(m ast.Node) bool {
							if m == nil {
								return false
							}
							inWithTx[m.Pos()] = true
							return true
						})
					}
					return true
				})

				// The statement kind each call is embedded in, so the
				// error handling can be judged.
				stmtOf := map[token.Pos]ast.Stmt{}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch st := n.(type) {
					case *ast.AssignStmt:
						for _, rhs := range st.Rhs {
							if c, ok := rhs.(*ast.CallExpr); ok {
								stmtOf[c.Pos()] = st
							}
						}
					case *ast.ExprStmt:
						if c, ok := st.X.(*ast.CallExpr); ok {
							stmtOf[c.Pos()] = st
						}
					}
					return true
				})

				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					method := sel.Sel.Name
					if method != "Exec" && method != "ExecContext" && method != "ExecTracked" {
						return true
					}
					if len(call.Args) == 0 {
						return true
					}
					sql := resolveSQL(call.Args[0], scope)
					if sql == "" {
						sql = "<non-literal>"
					}
					verb := ""
					if m := writeVerb.FindStringSubmatch(sql); m != nil {
						verb = strings.ToUpper(m[1])
					}
					if verb == "" {
						return true // not a write (PRAGMA, SELECT, ...)
					}
					table := ""
					if m := tableRE.FindStringSubmatch(sql); m != nil {
						table = m[1]
					}
					kind := "pool"
					if isTxReceiver(sel.X) {
						kind = "tx"
					}
					// A guarded write: ExecTracked(query, retries, ...).
					budget := -1
					if method == "ExecTracked" {
						kind = "guarded"
						if len(call.Args) >= 2 {
							if lit, ok := call.Args[1].(*ast.BasicLit); ok {
								if n, err := strconv.Atoi(lit.Value); err == nil {
									budget = n
								}
							}
						}
					}
					// Error handling: is the error looked at?
					//
					// `Exec`/`ExecContext` return (sql.Result, error).
					// The error is the SECOND return value, so in
					// `_, err = db.Exec(...)` the `_` binds the RESULT
					// and the error IS captured. Only when the second
					// slot is `_` (or the whole call is an ExprStmt) is
					// the error genuinely discarded.
					errHand := "unknown"
					errIdent := ""
					switch st := stmtOf[call.Pos()].(type) {
					case *ast.AssignStmt:
						errSlot := -1
						for i, rhs := range st.Rhs {
							if rhs == call {
								errSlot = i + 1 // error is return #2
								break
							}
						}
						if errSlot >= 0 && errSlot < len(st.Lhs) {
							if id, ok := st.Lhs[errSlot].(*ast.Ident); ok {
								if id.Name == "_" {
									errHand = "ignored"
								} else {
									errHand = "checked"
									errIdent = id.Name
								}
							} else {
								errHand = "checked"
							}
						} else {
							errHand = "unknown"
						}
					case *ast.ExprStmt:
						errHand = "ignored" // the result is dropped entirely
					}
					prop := "n/a"
					if errHand == "checked" && errIdent != "" {
						prop = stmtPropagation(fd, errIdent, stmtOf[call.Pos()])
						if prop == "unknown" {
							prop = propagation(fd, errIdent)
						}
					} else if errHand == "ignored" {
						prop = "dropped"
					}
					pos := fset.Position(call.Pos())
					sites = append(sites, Site{
						Kind:     kind,
						File:     path,
						Line:     pos.Line,
						Func:     fd.Name.Name,
						Verb:     verb,
						Table:    table,
						SQL:      sql,
						Receiver: receiverName(sel.X),
						ErrHand:  errHand,
						Budget:   budget,
						Prop:     prop,
						FnErr:    fnReturnsError(fd),
						InWithTx: inWithTx[call.Pos()],
					})
					return true
				})
			}
			return nil
		})
	}

	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})

	for _, s := range sites {
		fmt.Printf("%s\t%s:%d\t%s\t%s\t%s\tbudget=%d\terr=%s\tprop=%s\tfnerr=%s\ttx=%v\trecv=%s\n",
			s.Kind, s.File, s.Line, s.Func, s.Verb, s.Table, s.Budget, s.ErrHand, s.Prop, s.FnErr, s.InWithTx, s.Receiver)
	}
}
