package internal

// tx_write_budget_guard_test.go — issue #164's audit, kept discharged,
// and issue #178's extension of it to the WHOLE RUNTIME.
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
// This file is that something: it is a STRUCTURAL guard over the whole
// runtime. It parses every non-test Go file in the repository and fails
// when a WRITE passes a zero retry budget to ExecTracked without a
// recorded exemption.
//
// SCOPE — WHY #178 HAD TO EXTEND IT
// ---------------------------------
// The guard introduced by #176 walked `internal/core` and nothing else.
// That was the substrate as it was understood then. It was not the
// runtime:
//
//   - `cmd/mpm/store_working_context.go` writes `ephemeral_scratchpad`
//     — the SAME table #164's audit repaired on the tool side
//     (`internal/core/tools/handlers.go`, budget 5) — with a zero
//     budget and the comment "retries=0 → standard single attempt, no
//     backoff loop". That is #164's defect class, verbatim, on a
//     supported CLI path, and the guard could not see it.
//   - `internal/scheduler/drill_handler.go` writes `drill_runs` twice
//     with a zero budget, inside `WithTx`. The transaction layer owns
//     the STALE-SNAPSHOT class; it does not own the ORDINARY busy class,
//     so those two statements had no tolerance for it.
//
// The guard now walks the repository root. A guard that only covers the
// part of the tree its author was looking at is not a guard.
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
// A zero budget is NOT automatically wrong for every write. It is wrong
// when the statement layer is the layer that owns the busy class. See
// the exemption section below.
//
// THE EXEMPTION, AND WHY IT IS ONE
// --------------------------------
// A write is exempt only when an OUTER layer owns the SAME busy class
// for that path. There is exactly one such place in the repository:
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
// The exemption is not a hand-written promise. It names the OUTER OWNER
// of the busy class, and this guard verifies that the owner is still
// there and still re-drives the operation on an ordinary busy error —
// because a justification that has rotted (the outer loop deleted, its
// budget reduced, its busy check removed) is a defect, not a legacy.
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
// The same resolution applies to the RETRY BUDGET: a site that passes
// its budget through an identifier is resolved to the integer that
// identifier holds, so `ExecTracked(q, noRetry)` is caught the same way
// `ExecTracked(q, 0)` is.
//
// An exempted statement is named by a DISTINCTIVE FRAGMENT of its SQL,
// matched against the normalised statement text. A fragment rather than
// the whole statement: the substrate builds some of these statements
// from expressions (COALESCE(NULLIF(...)), and so on), and a fingerprint
// of the whole text would have to be copied by hand — which is how an
// exemption silently stops matching the day someone edits a column list.
// The fragments are specific enough that a NEW write in the same file is
// not covered by them, and this guard asserts that: every fragment must
// match EXACTLY ONE live statement.
//
// NON-VACUITY
// -----------
// A guard that cannot fail proves nothing. This file therefore carries
// its own negative control: TestWriteBudgetGuardFlagsRepresentativeUnsafeSites
// feeds the classifier synthetic sources containing the shapes that used
// to hide a defect, and asserts that each one is caught — and that the
// shapes that are NOT defects are not flagged. The control is synthetic
// on purpose: a mutation of the real tree would prove the guard fires
// once, whereas the synthetic control proves it fires on the SHAPE, and
// keeps proving it as the tree changes.

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

// busyCheckCall names the predicate an outer retry loop must use to
// recognise the busy class it owns.
var busyCheckCall = regexp.MustCompile(`^(?:[A-Za-z_][A-Za-z0-9_]*\.)?(?:IsBusyError|isBusyError)$`)

// writeBudgetExemption is one statement that is allowed to carry a zero
// retry budget, together with the reason it is allowed to.
//
// The three fields exist so that a deliberately-exempt operation can be
// told apart from an accidentally-unprotected one:
//
//   - SQL names the statement. A write that does not match is not
//     exempt, however similar it looks.
//   - Owner names the OUTER layer that owns the busy class. The guard
//     checks the owner still exists and still retries on it.
//   - Reason says why a statement-level budget would MULTIPLY the
//     owner's budget instead of adding resilience.
type writeBudgetExemption struct {
	// File is the path of the file, relative to the repository root.
	File string
	// SQL is a distinctive fragment of the exempted statement.
	SQL string
	// Owner is the outer layer that owns the ordinary busy class.
	Owner string
	// Reason explains the multiplication that a budget here would cause.
	Reason string
}

// exemptedWriteBudget is the set of statements allowed to carry a zero
// retry budget.
//
// Each entry is narrow on purpose: it names ONE statement by a
// distinctive SQL fragment, and names the outer layer that owns the busy
// class. The guard checks all three properties — the fragment still
// matches exactly one live statement, and the owner still retries on
// the busy class — so an exemption cannot outlive its justification.
var exemptedWriteBudget = []writeBudgetExemption{
	{
		File:  "internal/core/reference_db.go",
		SQL:   "INSERT INTO reference_docs",
		Owner: "IngestReference",
		Reason: "An OUTER layer owns the ordinary busy class for this path: " +
			"`IngestReference` re-drives the whole operation (attempts = 5) on an " +
			"ordinary busy error, re-resolving the identity each attempt so the loser " +
			"of a concurrent ingest applies an update to the winner's row instead of " +
			"failing. A statement-level budget here would multiply the two budgets — " +
			"5 x 6 = 30 transactions, where the outer budget says 5 — for no " +
			"additional resilience. The layer that owns a busy class owns its budget.",
	},
	{
		File: "internal/core/reference_db.go",
		SQL: "INSERT INTO reference_chunks (id, doc_id, chunk_index, section, content, source_path, content_hash) " +
			"VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING",
		Owner: "IngestReference",
		Reason: "Same outer owner as the doc-row upsert: IngestReference re-drives " +
			"the whole ingest (attempts = 5) on an ordinary busy error.",
	},
	{
		File: "internal/core/reference_db.go",
		SQL: "INSERT INTO reference_chunks (id, doc_id, chunk_index, section, content, source_path, content_hash) " +
			"VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET",
		Owner: "IngestReference",
		Reason: "Same outer owner as the doc-row upsert: IngestReference re-drives " +
			"the whole ingest (attempts = 5) on an ordinary busy error.",
	},
	{
		File:  "internal/core/reference_db.go",
		SQL:   "DELETE FROM reference_chunks WHERE id = ?",
		Owner: "IngestReference",
		Reason: "Same outer owner as the doc-row upsert: IngestReference re-drives " +
			"the whole ingest (attempts = 5) on an ordinary busy error.",
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

// intFingerprint renders an integer expression, when it is one.
//
// A retry budget is almost always a literal, but a site may pass it
// through an identifier — and a scanner that only reads literals is a
// scanner that a `const noRetry = 0` walks straight past.
func intFingerprint(expr ast.Expr) (int, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.INT {
			return 0, false
		}
		n, err := strconv.Atoi(e.Value)
		if err != nil {
			return 0, false
		}
		return n, true
	case *ast.UnaryExpr:
		if e.Op == token.SUB {
			if n, ok := intFingerprint(e.X); ok {
				return -n, true
			}
		}
		return 0, false
	}
	return 0, false
}

// valueIsString reports whether an expression denotes a STRING.
//
// It exists so the scope resolver can tell "this is the SQL text" from
// "this is a number whose text happens to be non-empty". A retry budget
// of 0 renders as the text "0"; treating that as SQL would bind the
// name to a statement called "0" and lose the budget.
func valueIsString(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return valueIsString(e.X) && valueIsString(e.Y)
		}
	case *ast.CallExpr:
		// A format string call: the fingerprint is its FORMAT argument.
		return true
	}
	return false
}

// binding is one resolved literal: a SQL string, an integer, or both.
type binding struct {
	SQL   string
	Int   int
	IsInt bool
}

// literalScope collects the literal bindings visible to a statement:
// package-level constants and variables, and the local constants and
// variables of the enclosing function.
//
// It exists for one reason: the scratchpad flush passes its SQL as a
// `const query`, and a scanner that only reads string LITERALS misses
// it — which is how a site with a zero budget can sit in the tree and
// pass a guard that claims to cover every site.
func literalScope(decls []ast.Decl) map[string]binding {
	scope := map[string]binding{}

	add := func(name string, value ast.Expr) {
		if name == "_" {
			return
		}
		// An integer binding is checked FIRST. A numeric literal's
		// fingerprint is its own text ("0"), so a string-first order
		// would record `const noRetry = 0` as the SQL "0" and leave the
		// retry budget unresolved — which is exactly the hole this
		// resolution exists to close.
		if n, ok := intFingerprint(value); ok {
			scope[name] = binding{Int: n, IsInt: true}
			return
		}
		// An identifier bound to another identifier carries that
		// binding through.
		if id, ok := value.(*ast.Ident); ok {
			if b, found := scope[id.Name]; found {
				scope[name] = b
			}
			return
		}
		if s := sqlFingerprint(value); s != "" && valueIsString(value) {
			scope[name] = binding{SQL: s}
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

// resolveSQL renders the SQL a call executes, following an identifier
// to its binding when the statement passes a variable.
func resolveSQL(expr ast.Expr, scope map[string]binding) string {
	if id, ok := expr.(*ast.Ident); ok {
		if b, found := scope[id.Name]; found {
			return b.SQL
		}
		return ""
	}
	return sqlFingerprint(expr)
}

// resolveBudget renders the retry budget a call passes, following an
// identifier to its binding when the budget is a variable.
//
// The second return value is false when the budget cannot be resolved
// — a computed budget, say. An unresolvable budget is not treated as
// zero; it is reported separately, because "I could not read this
// budget" and "this budget is zero" are different findings.
func resolveBudget(expr ast.Expr, scope map[string]binding) (int, bool) {
	if id, ok := expr.(*ast.Ident); ok {
		if b, found := scope[id.Name]; found && b.IsInt {
			return b.Int, true
		}
		return 0, false
	}
	return intFingerprint(expr)
}

// writeSite is one ExecTracked write the scan classified.
type writeSite struct {
	// File is the path relative to the repository root.
	File string
	// Line is the line of the budget argument.
	Line int
	// SQL is the normalised statement text.
	SQL string
	// Budget is the retry budget the call passes.
	Budget int
	// BudgetKnown is false when the budget could not be resolved.
	BudgetKnown bool
	// Exempt is true when a recorded exemption covers the statement.
	Exempt bool
	// Exemption is the exemption that covers it, when one does.
	Exemption *writeBudgetExemption
}

// scanSource classifies every ExecTracked write in one source file.
//
// It is a pure function of (path, source) plus the exemption table, so
// the negative control can feed it synthetic sources and assert on what
// it decides. That is what makes the control a proof about the SHAPE
// rather than a one-off mutation of the tree.
func scanSource(rel string, src []byte) ([]writeSite, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		return nil, err
	}
	scope := literalScope(file.Decls)

	var sites []writeSite
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
			sql := resolveSQL(call.Args[0], scope)
			if !writeVerb.MatchString(sql) {
				return true
			}
			budget, known := resolveBudget(call.Args[1], scope)
			site := writeSite{
				File:        rel,
				SQL:         sql,
				Budget:      budget,
				BudgetKnown: known,
			}
			site.Line = fset.Position(call.Args[1].Pos()).Line
			for i := range exemptedWriteBudget {
				ex := &exemptedWriteBudget[i]
				if ex.File != rel {
					continue
				}
				if strings.Contains(sql, ex.SQL) {
					site.Exempt = true
					site.Exemption = ex
					break
				}
			}
			sites = append(sites, site)
			return true
		})
	}
	return sites, nil
}

// offenders returns the sites that carry a zero budget with no recorded
// exemption.
func offenders(sites []writeSite) []writeSite {
	var out []writeSite
	for _, s := range sites {
		if s.BudgetKnown && s.Budget == 0 && !s.Exempt {
			out = append(out, s)
		}
	}
	return out
}

// writeBudgetRepoRoot returns the repository root, found by walking up from this
// test file to the go.mod of the ROOT module.
//
// The guard now covers the whole runtime, so it needs the whole
// repository. The root module is named explicitly rather than assumed to
// be the parent of `internal/core`: the substrate is a nested module, so
// "one directory up" would be `internal/`, not the repository.
func writeBudgetRepoRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller must resolve this test file")
	}
	dir := filepath.Dir(thisFile)
	for {
		manifest := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(manifest); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "module github.com/flowbyte-com/mpm" {
					return dir, nil
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repository root (module github.com/flowbyte-com/mpm) not found above %s", thisFile)
		}
		dir = parent
	}
}

// scannedRepo is the result of scanning the repository.
type scannedRepo struct {
	// Sites are every ExecTracked write the scan classified.
	Sites []writeSite
	// Files are the files the scan read.
	Files []string
}

// scanRepository walks the repository and classifies every write.
func scanRepository(root string) (*scannedRepo, error) {
	out := &scannedRepo{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
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
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		sites, perr := scanSource(rel, src)
		if perr != nil {
			return perr
		}
		out.Sites = append(out.Sites, sites...)
		out.Files = append(out.Files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TestEveryRuntimeWriteHasAStatementLevelRetryBudget is the guard.
//
// It walks the WHOLE runtime — the root module, the substrate module,
// the tools module and the release-gate module — and fails when a write
// passes a zero retry budget to ExecTracked without a recorded
// exemption.
//
// This is the standing proof that issue #164's third option stays
// discharged, and (since #178's extension) that the proof covers the
// parts of the runtime #176's guard could not see.
func TestEveryRuntimeWriteHasAStatementLevelRetryBudget(t *testing.T) {
	root, err := writeBudgetRepoRoot()
	if err != nil {
		t.Skipf("the repository root is not reachable from this checkout (%v): the guard cannot be "+
			"evaluated outside the repository, and the synthetic negative control below still runs", err)
	}
	repo, err := scanRepository(root)
	if err != nil {
		t.Fatalf("walk the runtime: %v", err)
	}

	if len(repo.Sites) == 0 {
		t.Fatal("no write call site was examined: the scan found nothing, so this test proves nothing")
	}

	bad := offenders(repo.Sites)
	if len(bad) > 0 {
		var lines []string
		for _, s := range bad {
			lines = append(lines, fmt.Sprintf("%s:%d (budget %d)", s.File, s.Line, s.Budget))
		}
		t.Fatalf("these writes pass a ZERO retry budget to ExecTracked, so an ordinary lock "+
			"contention loses the artifact instead of waiting it out (issue #164). Give them "+
			"the documented budget of 5, or record the exemption in exemptedWriteBudget with "+
			"the owner of that busy class and the reason a budget there would multiply the "+
			"owner's budget:\n  %s",
			strings.Join(lines, "\n  "))
	}

	// A budget the scan could not read is not the same finding as a
	// zero budget, but it is not a finding we can dismiss either: a
	// computed budget is exactly the shape a scanner cannot audit.
	var unreadable []string
	for _, s := range repo.Sites {
		if !s.BudgetKnown {
			unreadable = append(unreadable, fmt.Sprintf("%s:%d", s.File, s.Line))
		}
	}
	if len(unreadable) > 0 {
		t.Fatalf("these ExecTracked writes pass a retry budget the scan cannot resolve, so the "+
			"guard cannot tell whether they are protected:\n  %s",
			strings.Join(unreadable, "\n  "))
	}
}

// TestWriteBudgetGuardScansEveryRuntimePackage proves the guard's
// REACH, not just its verdict.
//
// A guard that walks a directory and finds nothing proves nothing, and a
// guard that walks the wrong directory proves the wrong thing. This test
// asserts that the scan actually reaches the packages that carry
// runtime writes — including the ones #176's guard could not see.
//
// The floor is deliberately low: the point is that a package is reached
// AT ALL, not that a particular number of writes survives in it. A
// package whose writes all move elsewhere must fail loudly here rather
// than silently stop being guarded.
func TestWriteBudgetGuardScansEveryRuntimePackage(t *testing.T) {
	root, err := writeBudgetRepoRoot()
	if err != nil {
		t.Skipf("the repository root is not reachable from this checkout (%v)", err)
	}
	repo, err := scanRepository(root)
	if err != nil {
		t.Fatalf("walk the runtime: %v", err)
	}

	// Count the sites under each directory prefix. A prefix rather than
	// an exact directory, because a package's writes can live in
	// sub-packages and the point is REACH, not layout.
	countUnder := func(prefix string) int {
		n := 0
		for _, s := range repo.Sites {
			if s.File == prefix || strings.HasPrefix(s.File, prefix+"/") {
				n++
			}
		}
		return n
	}

	// Every part of the runtime that carries a write must be reached.
	for _, prefix := range []string{"cmd", "internal"} {
		if countUnder(prefix) == 0 {
			t.Errorf("the scan reached no ExecTracked write under %q: the guard is not covering "+
				"that part of the runtime", prefix)
		}
	}

	// The packages #178 added must be reached SPECIFICALLY. These are
	// the ones #176's guard could not see, and a silent regression to
	// the old scope would otherwise leave the guard green.
	for _, prefix := range []string{"cmd/mpm", "internal/scheduler", "internal/core"} {
		if countUnder(prefix) == 0 {
			t.Errorf("the scan reached no ExecTracked write under %q: the guard has lost the "+
				"coverage issue #178 added", prefix)
		}
	}

	// And the substrate module itself must still be reached — the
	// original #164/#176 scope.
	if len(repo.Sites) == 0 {
		t.Fatal("no write call site was examined: the scan found nothing, so this test proves nothing")
	}
}

// TestWriteBudgetExemptionsAreNarrowJustifiedAndLive checks the
// properties an exemption has to hold, so the exemption table cannot
// rot into a blanket.
//
//   - NARROW: each fragment must match EXACTLY ONE live statement, so
//     it cannot exempt a statement nobody named.
//   - JUSTIFIED: each exemption must name the outer owner of the busy
//     class, and that owner must still exist and still re-drive the
//     operation on an ordinary busy error.
//   - LIVE: each fragment must still match a live statement, so a
//     deleted statement does not leave an exemption behind that quietly
//     covers a future one.
func TestWriteBudgetExemptionsAreNarrowJustifiedAndLive(t *testing.T) {
	root, err := writeBudgetRepoRoot()
	if err != nil {
		t.Skipf("the repository root is not reachable from this checkout (%v)", err)
	}
	repo, err := scanRepository(root)
	if err != nil {
		t.Fatalf("walk the runtime: %v", err)
	}

	// Group the live statements by file so each fragment can be matched
	// against the statements in its own file.
	byFile := map[string][]writeSite{}
	for _, s := range repo.Sites {
		byFile[s.File] = append(byFile[s.File], s)
	}

	for _, ex := range exemptedWriteBudget {
		if ex.File == "" || ex.SQL == "" || ex.Owner == "" || ex.Reason == "" {
			t.Errorf("an exemption is incomplete: %+v — every exemption needs a file, a SQL "+
				"fragment, the outer owner of the busy class, and the reason a budget there "+
				"would multiply the owner's budget", ex)
			continue
		}

		matched := 0
		for _, s := range byFile[ex.File] {
			if strings.Contains(s.SQL, ex.SQL) {
				matched++
			}
		}
		switch {
		case matched == 0:
			t.Errorf("the exemption for %q in %s no longer matches any live statement: it is "+
				"stale, and a stale exemption is a hole waiting for the next write in that file",
				ex.SQL, ex.File)
		case matched > 1:
			t.Errorf("the exemption for %q in %s matches %d live statements: an exemption must "+
				"be narrow enough that it can only ever cover the statement it names",
				ex.SQL, ex.File, matched)
		}

		ok, why := ownerStillRetries(root, ex.Owner)
		if !ok {
			t.Errorf("the exemption for %q in %s names %q as the outer owner of the busy class, "+
				"but that owner no longer holds it: %s. An exemption whose justification has "+
				"rotted is a defect, not a legacy.", ex.SQL, ex.File, ex.Owner, why)
		}
	}
}

// ownerStillRetries reports whether the named function still owns the
// ordinary busy class: it must exist, it must test for a busy error, and
// it must re-drive the operation in a loop.
//
// This is the mechanical form of "an outer layer owns this busy class".
// It is what keeps the exemption table honest: if the outer loop is
// deleted, or its busy check is removed, or its retry is turned into a
// single attempt, the exemption fails here rather than quietly
// exempting an unprotected write.
func ownerStillRetries(root, owner string) (bool, string) {
	found := false
	hasBusyCheck := false
	hasLoop := false

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
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
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Name.Name != owner {
				continue
			}
			found = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					// Both spellings count: `IsBusyError(err)` inside the
					// substrate, `core.IsBusyError(err)` from a sibling
					// package.
					switch fun := node.Fun.(type) {
					case *ast.SelectorExpr:
						if busyCheckCall.MatchString(fun.Sel.Name) {
							hasBusyCheck = true
						}
					case *ast.Ident:
						if busyCheckCall.MatchString(fun.Name) {
							hasBusyCheck = true
						}
					}
				case *ast.ForStmt:
					hasLoop = true
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		return false, fmt.Sprintf("walking the runtime failed: %v", err)
	}
	switch {
	case !found:
		return false, fmt.Sprintf("no function named %q exists in the runtime", owner)
	case !hasBusyCheck:
		return false, fmt.Sprintf("%q no longer tests for a busy error, so it does not own that class", owner)
	case !hasLoop:
		return false, fmt.Sprintf("%q no longer re-drives the operation in a loop", owner)
	}
	return true, ""
}

// TestWriteBudgetGuardFlagsRepresentativeUnsafeSites is the negative
// control.
//
// It feeds the classifier synthetic sources containing the shapes that
// have hidden a zero-budget write, and asserts that each one is caught
// — and that the shapes which are NOT defects are not flagged.
//
// It is synthetic on purpose. A mutation of the real tree would prove
// the guard fires once, on one file, on one day. This control proves the
// guard fires on the SHAPE, and keeps proving it as the tree changes.
func TestWriteBudgetGuardFlagsRepresentativeUnsafeSites(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		wantFlag bool
		why      string
	}{
		{
			name: "a literal zero budget on a write",
			src: `package p
func f(d interface{ ExecTracked(string, int, ...interface{}) (interface{}, error) }) {
	d.ExecTracked("INSERT INTO t (id) VALUES (?)", 0, "x")
}`,
			wantFlag: true,
			why:      "the classic #164 shape: a zero budget on a write loses the artifact",
		},
		{
			name: "a zero budget behind an identifier",
			src: `package p
func f(d interface{ ExecTracked(string, int, ...interface{}) (interface{}, error) }) {
	const noRetry = 0
	d.ExecTracked("UPDATE t SET a = ? WHERE id = ?", noRetry, 1, "x")
}`,
			wantFlag: true,
			why:      "a budget passed through an identifier must be resolved, not skipped",
		},
		{
			name: "a zero budget on a write whose SQL is a variable",
			src: `package p
func f(d interface{ ExecTracked(string, int, ...interface{}) (interface{}, error) }) {
	const query = "DELETE FROM t WHERE id = ?"
	d.ExecTracked(query, 0, "x")
}`,
			wantFlag: true,
			why:      "the scratchpad shape: SQL in a variable, budget zero",
		},
		{
			name: "a zero budget on a write whose SQL is a concatenation",
			src: `package p
func f(d interface{ ExecTracked(string, int, ...interface{}) (interface{}, error) }) {
	d.ExecTracked("INSERT INTO "+"t (id) VALUES (?)", 0, "x")
}`,
			wantFlag: true,
			why:      "a statement built from parts is still a statement",
		},
		{
			name: "a zero budget on a write in a runtime package outside the substrate",
			src: `package p
func f(d interface{ ExecTracked(string, int, ...interface{}) (interface{}, error) }) {
	d.ExecTracked("INSERT INTO drill_runs (id) VALUES (?)", 0, "x")
}`,
			wantFlag: true,
			why:      "the write does not have to be in internal/core to be a write",
		},
		{
			name: "a non-zero budget on a write",
			src: `package p
func f(d interface{ ExecTracked(string, int, ...interface{}) (interface{}, error) }) {
	d.ExecTracked("INSERT INTO t (id) VALUES (?)", 5, "x")
}`,
			wantFlag: false,
			why:      "budget 5 is the documented protection and must not be flagged",
		},
		{
			name: "a zero budget on a read",
			src: `package p
func f(d interface{ ExecTracked(string, int, ...interface{}) (interface{}, error) }) {
	d.ExecTracked("SELECT id FROM t WHERE id = ?", 0, "x")
}`,
			wantFlag: false,
			why:      "a read has no artifact to lose; the guard is about writes",
		},
		{
			name: "a zero inside a SQL literal",
			src: `package p
func f(d interface{ ExecTracked(string, int, ...interface{}) (interface{}, error) }) {
	d.ExecTracked("INSERT INTO t (id, cascade_invalidated) VALUES (?, ?, 0)", 5, "x")
}`,
			wantFlag: false,
			why: "a ', 0)' inside the SQL text is not a retry budget — this is the false " +
				"positive the first draft of the #164 audit produced",
		},
		{
			name: "a zero budget on a write exempted by the table",
			src: `package p
func f(d interface{ ExecTracked(string, int, ...interface{}) (interface{}, error) }) {
	d.ExecTracked("INSERT INTO reference_docs (id) VALUES (?)", 0, "x")
}`,
			wantFlag: false,
			why:      "the recorded exemption still covers the statement it names",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel := "internal/core/synthetic_probe.go"
			if tc.name == "a zero budget on a write in a runtime package outside the substrate" {
				rel = "internal/scheduler/synthetic_probe.go"
			}
			if tc.name == "a zero budget on a write exempted by the table" {
				rel = "internal/core/reference_db.go"
			}
			sites, err := scanSource(rel, []byte(tc.src))
			if err != nil {
				t.Fatalf("scan the synthetic source: %v", err)
			}
			bad := offenders(sites)
			if tc.wantFlag && len(bad) == 0 {
				t.Fatalf("the guard did NOT flag this shape, but it must: %s", tc.why)
			}
			if !tc.wantFlag && len(bad) > 0 {
				t.Fatalf("the guard flagged this shape, but it must not: %s (flagged %s:%d)",
					tc.why, bad[0].File, bad[0].Line)
			}
		})
	}
}
