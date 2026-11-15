// skill_package_read_guard_test.go — the static guard that pins the
// package reader's read-only property.
//
// WHY THIS FILE EXISTS
// --------------------
// The reader's read snapshot is a DEFERRED transaction: the SQLite
// driver ignores `sql.TxOptions{ReadOnly}` and issues a plain `BEGIN`.
// The snapshot's read-only nature therefore rests on DISCIPLINE, and a
// discipline that is merely asserted is a discipline that will one day
// be broken by accident.
//
// This file makes the discipline STRUCTURAL: it walks the reader's own
// source and proves that the reader issues no write and owns no
// statement that is anything but a single read.
//
// And it makes the guard's own evidence REPRODUCIBLE. The guard is a
// FUNCTION over source text, not a side effect of a test, so the
// evasion matrix below can be pinned in the repository: each evasion is
// spliced into a COPY of the reader, the guard is run against the
// copy, and the guard is required to report it. A manual sweep proves
// nothing on the next commit; a pinned matrix does.
//
// THE SCRUBBER HAS TWO READINGS
// ------------------------------
// SQLite treats a SQL comment as WHITESPACE. So a comment can be
// removed entirely (which rejoins a keyword split by it) or replaced
// by a space (which keeps the tokens either side of it separate) — and
// each reading catches a write the other misses:
//
//	DE/*x*/LETE FROM t   → removed: DELETE FROM t   → a write
//	DELETE/*x*/FROM t    → spaced:  DELETE FROM t    → a write
//
// A statement is therefore judged under BOTH readings, and refused if
// EITHER says it is not a single read. Neither reading alone is
// sound; together they cover both shapes.
//
// WHAT THE GUARD CHECKS
// ---------------------
//   - a CALL to a database method is inspected, and the SQL it carries
//     is checked — so a write smuggled through `QueryContext` is caught,
//     and so is one prepared through `PrepareContext`;
//   - a bare REFERENCE to a database method — a method value — is
//     refused, so `exec := db.ExecContext; exec(ctx, "UPDATE …")`
//     cannot slip past the call walk;
//   - every string constant the file declares is checked: one that is a
//     STATEMENT must be a single read statement, so a write cannot hide
//     in a constant — used or not, concatenated or not;
//   - a statement whose SQL is neither a literal nor one of the reader's
//     own constants is refused, so a statement built at runtime cannot
//     slip past either;
//   - every SQL constant the file declares must be USED by a statement,
//     so a statement no call site issues is still a statement the reader
//     owns — and so a file cannot declare SQL it never runs.
//
// Both literal forms are decoded, so a statement written as a raw
// backtick string and one written as a double-quoted string are checked
// alike — and a double-quoted SELECT is not a false positive.

package internal

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// p6DatabaseMethodNames are the methods that issue a statement.
//
// A call to one of them is a statement; a bare REFERENCE to one of them
// is a method value, which the call walk cannot see.
//
// `Prepare` and `PrepareContext` are in the set because a prepared
// statement's SQL lives at the PREPARE site: the `*sql.Stmt` it returns
// carries the SQL forward, so a write prepared there would be executed by
// a later `stmt.Query…` call that carries no SQL of its own.
var p6DatabaseMethodNames = map[string]bool{
	"Exec":            true,
	"ExecContext":     true,
	"Query":           true,
	"QueryRow":        true,
	"QueryContext":    true,
	"QueryRowContext": true,
	"Prepare":         true,
	"PrepareContext":  true,
}

// p6SQLKeywords are the keywords a SQL statement can begin with.
//
// A text whose first word is one of them is a STATEMENT, and it is
// therefore subject to the statement check. A text whose first word is
// not one of them — "skill_package", "utf-8", "manifest_identity" — is
// not a statement, and is left alone.
//
// `WITH` is in the list because a CTE is a statement: `WITH x AS (…)
// SELECT …` is a legitimate read, and `WITH x AS (… ) UPDATE …` is a
// write, and the write-keyword rule tells the two apart.
//
// `EXPLAIN` is in the list so that `EXPLAIN DELETE …` is classified as a
// statement — and then refused, because it does not begin with `SELECT`.
var p6SQLKeywords = []string{
	"SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "DROP", "ALTER",
	"CREATE", "PRAGMA", "VACUUM", "ATTACH", "DETACH", "BEGIN", "COMMIT",
	"ROLLBACK", "SAVEPOINT", "RELEASE", "REASSIGN", "GRANT", "REINDEX",
	"ANALYZE", "WITH", "EXPLAIN",
}

// p6SQLWriteKeywords are the keywords that make a statement a WRITE.
//
// A statement that carries one of them anywhere — not merely at its
// start — is a write, and the reader issues none.
//
// `END` is deliberately absent: it is a CASE terminator, and adding it
// would make every `SELECT CASE … END` look like a write.
var p6SQLWriteKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "REPLACE", "DROP", "ALTER", "CREATE",
	"PRAGMA", "VACUUM", "ATTACH", "DETACH", "BEGIN", "COMMIT", "ROLLBACK",
	"SAVEPOINT", "RELEASE", "REASSIGN", "GRANT", "REINDEX", "ANALYZE",
}

// p6UnquoteSQL decodes a Go string literal into the SQL it carries.
//
// It handles BOTH literal forms — the raw backtick string and the
// double-quoted interpreted string — so a statement written either way is
// checked, rather than only the raw form being checked and the other
// raising a false positive.
func p6UnquoteSQL(literal string) (string, bool) {
	unquoted, err := strconv.Unquote(literal)
	if err != nil {
		return "", false
	}
	return unquoted, true
}

// p6ConstStringValue returns the string a constant expression carries.
//
// It handles a plain literal and a CONCATENATION of literals, because
// `"DELETE " + "FROM …"` is a constant too — and a write written that
// way must not slip past the constant check.
func p6ConstStringValue(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		return p6UnquoteSQL(e.Value)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := p6ConstStringValue(e.X)
		if !ok {
			return "", false
		}
		right, ok := p6ConstStringValue(e.Y)
		if !ok {
			return "", false
		}
		return left + right, true
	case *ast.ParenExpr:
		return p6ConstStringValue(e.X)
	}
	return "", false
}

// p6ScrubSQL strips the parts of a SQL text that are not statement
// structure: comments, string literals and quoted identifiers.
//
// What is left is the statement's CODE — the words that decide what it
// does.
//
// A comment is handled in ONE OF TWO WAYS, and the guard judges the text
// under BOTH:
//
//   - REMOVED (spaceComments = false): the comment disappears, so a
//     keyword split by a comment is rejoined — `DE/*x*/LETE` becomes
//     `DELETE`;
//   - SPACED (spaceComments = true): the comment becomes a space, so the
//     tokens either side of it stay separate — `DELETE/*x*/FROM t`
//     becomes `DELETE FROM t`.
//
// Neither reading alone is sound. SQLite treats a comment as whitespace,
// so a statement is only a write if EITHER reading says so.
//
// A string literal is consumed WHOLE, including a doubled quote: two
// quotes in a row are an ESCAPED quote inside the literal, not its end.
// The literal's contents are never inspected, so a keyword inside one is
// never seen.
//
// SQLite's bracket identifier quoting (`[name]`) is consumed whole as
// well, so `SELECT [a;b]` is one identifier and not a statement with a
// separator in it.
func p6ScrubSQL(sql string, spaceComments bool) string {
	var b strings.Builder
	runes := []rune(sql)
	for i := 0; i < len(runes); i++ {
		switch {
		case runes[i] == '-' && i+1 < len(runes) && runes[i+1] == '-':
			// The comment runs to the end of the line, and the line
			// break goes with it: `DE--x\nLETE` must become
			// `DELETE`, not `DE LETE`.
			//
			// The index is left ON the newline, because the loop's own
			// increment then steps past it — advancing it here as well
			// would swallow the first character after the comment.
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			if spaceComments {
				b.WriteRune(' ')
			}
		case runes[i] == '/' && i+1 < len(runes) && runes[i+1] == '*':
			i += 2
			for i+1 < len(runes) && !(runes[i] == '*' && runes[i+1] == '/') {
				i++
			}
			i++
			if spaceComments {
				b.WriteRune(' ')
			}
		case runes[i] == '\'' || runes[i] == '"' || runes[i] == '`':
			quote := runes[i]
			i++
			for i < len(runes) {
				if runes[i] == quote {
					if i+1 < len(runes) && runes[i+1] == quote {
						// A doubled quote is an ESCAPED quote, not
						// the end of the literal.
						i += 2
						continue
					}
					break
				}
				i++
			}
			b.WriteRune(' ')
		case runes[i] == '[':
			for i < len(runes) && runes[i] != ']' {
				i++
			}
			b.WriteRune(' ')
		default:
			b.WriteRune(runes[i])
		}
	}
	return b.String()
}

// p6SQLWords splits a SQL text into its keywords and identifiers.
//
// A word is a run of letters, digits and UNDERSCORES, so an identifier
// like `last_update` is ONE word and is never mistaken for the keyword
// `UPDATE`.
func p6SQLWords(sql string) []string {
	var words []string
	var current strings.Builder
	for _, r := range sql {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			current.WriteRune(r)
		default:
			if current.Len() > 0 {
				words = append(words, current.String())
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		words = append(words, current.String())
	}
	return words
}

// p6WordIn reports whether a word is in a keyword list.
func p6WordIn(word string, list []string) bool {
	for _, candidate := range list {
		if word == candidate {
			return true
		}
	}
	return false
}

// p6SQLFirstKeyword returns the first keyword of a scrubbed SQL text.
//
// It returns the empty string when the text has no words, or when its
// first word is not a SQL keyword — which is how a non-SQL constant
// ("skill_package", "manifest_identity") is told from a statement.
func p6SQLFirstKeyword(code string) string {
	body := strings.TrimRight(strings.ToUpper(code), " \t\r\n")
	body = strings.TrimSuffix(body, ";")
	body = strings.TrimRight(body, " \t\r\n")
	words := p6SQLWords(body)
	if len(words) == 0 {
		return ""
	}
	first := strings.ToUpper(words[0])
	if !p6WordIn(first, p6SQLKeywords) {
		return ""
	}
	return first
}

// p6IsSQLStatement reports whether a SQL text is a STATEMENT at all.
//
// The text is judged under BOTH scrub readings: a text that is a
// statement under either reading is a statement.
func p6IsSQLStatement(sql string) bool {
	for _, spaceComments := range []bool{false, true} {
		if p6SQLFirstKeyword(p6ScrubSQL(sql, spaceComments)) != "" {
			return true
		}
	}
	return false
}

// p6SQLStatementProblemUnder describes why ONE scrubbed reading of a SQL
// text is not a single read statement, or returns the empty string when
// it is one.
//
// The check is STATEMENT-based, not prefix-based, and it covers:
//
//   - the statement must BEGIN with `SELECT` — or with `WITH`, because a
//     CTE is a statement too. A `WITH … UPDATE` is caught by the
//     write-keyword rule below, so allowing `WITH` does not admit a
//     write; and `EXPLAIN` is refused, because the reader issues plain
//     SELECTs and nothing else;
//   - the statement must be a SINGLE statement — a `;` anywhere but at
//     the very end means a second statement follows;
//   - the statement must carry NO write keyword anywhere — so a
//     `SELECT 1; DELETE …` is caught by the second rule and a
//     `SELECT … WHERE x = 'UPDATE'` is not, because the string literal
//     is scrubbed first.
func p6SQLStatementProblemUnder(code string) string {
	body := strings.TrimRight(strings.ToUpper(code), " \t\r\n")
	body = strings.TrimSuffix(body, ";")
	body = strings.TrimRight(body, " \t\r\n")

	words := p6SQLWords(body)
	if len(words) == 0 {
		return ""
	}
	first := strings.ToUpper(words[0])
	if !p6WordIn(first, p6SQLKeywords) {
		// Not a statement under this reading: no opinion.
		return ""
	}
	if first != "SELECT" && first != "WITH" {
		return "the statement begins with " + first + ", not SELECT"
	}
	if strings.Contains(body, ";") {
		return "the statement carries more than one statement"
	}
	for _, word := range words[1:] {
		upper := strings.ToUpper(word)
		if p6WordIn(upper, p6SQLWriteKeywords) {
			return "the statement carries the write keyword " + upper
		}
	}
	return ""
}

// p6SQLStatementProblem describes why a SQL text is not a single read
// statement, or returns the empty string when it is one.
//
// The text is judged under BOTH scrub readings, and refused if EITHER
// says it is not a single read — because a comment is whitespace to
// SQLite, and each reading catches a shape the other misses.
func p6SQLStatementProblem(sql string) string {
	for _, spaceComments := range []bool{false, true} {
		if problem := p6SQLStatementProblemUnder(p6ScrubSQL(sql, spaceComments)); problem != "" {
			return problem
		}
	}
	return ""
}

// p6ReaderWriteProblems walks ONE reader source file and returns every
// way in which it issues a write or owns a statement that is not a
// single read statement, together with the number of statements it
// found. An empty problem list means the file is clean.
//
// The source is passed in rather than read from disk, so the guard can
// be run against a MODIFIED COPY of the reader — which is what makes
// the evasion matrix below reproducible.
func p6ReaderWriteProblems(sourceFile string, source []byte) ([]string, int) {
	var problems []string
	problem := func(format string, args ...any) {
		problems = append(problems, sourceFile+": "+fmt.Sprintf(format, args...))
	}

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, sourceFile, source, 0)
	if err != nil {
		return []string{sourceFile + ": the source does not parse: " + err.Error()}, 0
	}

	// The SQL the reader's own constants declare — at the top level AND
	// inside a function, because a function-local constant is just as
	// much a statement the reader owns. A constant may be a plain
	// literal or a CONCATENATION of literals, and both are resolved.
	consts := map[string]string{}
	ast.Inspect(parsed, func(n ast.Node) bool {
		gen, ok := n.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			return true
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				value, ok := p6ConstStringValue(vs.Values[i])
				if !ok {
					continue
				}
				consts[name.Name] = value
			}
		}
		return true
	})

	// The database-method selectors that are the `Fun` of a CALL. A
	// selector that is not in this set is a bare reference — a method
	// value — and the reader must not bind one.
	calledSelectors := map[token.Pos]bool{}

	// The constants a statement actually used. A SQL constant no
	// statement uses is dead SQL the reader still owns.
	usedConstants := map[string]bool{}

	queries := 0
	ast.Inspect(parsed, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if !p6DatabaseMethodNames[sel.Sel.Name] {
			return true
		}
		calledSelectors[sel.Pos()] = true

		switch sel.Sel.Name {
		case "Exec", "ExecContext":
			problem("issues a write through %s at line %d: the reader must never write",
				sel.Sel.Name, fset.Position(call.Pos()).Line)
			return true
		}
		queries++

		// The context variants take the context first; the plain ones do
		// not. The SQL is the first argument after the context.
		sqlArg := 0
		switch sel.Sel.Name {
		case "QueryContext", "QueryRowContext", "PrepareContext":
			sqlArg = 1
		}
		if len(call.Args) <= sqlArg {
			problem("issues a statement with no SQL at line %d", fset.Position(call.Pos()).Line)
			return true
		}
		// The SQL is either a literal or one of the reader's own
		// constants; both are resolved here, so a statement smuggled in
		// through either route is caught.
		var sql string
		switch arg := call.Args[sqlArg].(type) {
		case *ast.BasicLit:
			if arg.Kind != token.STRING {
				problem("issues a statement whose SQL is not a string at line %d",
					fset.Position(call.Pos()).Line)
				return true
			}
			resolved, ok := p6UnquoteSQL(arg.Value)
			if !ok {
				problem("issues a statement whose SQL literal does not decode at line %d",
					fset.Position(call.Pos()).Line)
				return true
			}
			sql = resolved
		case *ast.Ident:
			resolved, ok := consts[arg.Name]
			if !ok {
				problem("issues a statement whose SQL is %s, which is not one of its own constants at line %d",
					arg.Name, fset.Position(call.Pos()).Line)
				return true
			}
			usedConstants[arg.Name] = true
			sql = resolved
		default:
			problem("issues a statement whose SQL is neither a literal nor a constant at line %d",
				fset.Position(call.Pos()).Line)
			return true
		}
		if p := p6SQLStatementProblem(sql); p != "" {
			problem("issues a statement that is not a single read statement at line %d: %s: %q",
				fset.Position(call.Pos()).Line, p, sql)
		}
		return true
	})

	// ── The method-value route. ──────────────────────────────────────
	//
	// A bare reference to a database method is a METHOD VALUE. The call
	// walk cannot see what it is later called with, so the reference
	// itself is refused: `exec := db.ExecContext` is a write waiting to
	// happen, and the reader has no reason to bind one.
	ast.Inspect(parsed, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if !p6DatabaseMethodNames[sel.Sel.Name] {
			return true
		}
		if calledSelectors[sel.Pos()] {
			return true
		}
		problem("binds the database method %s at line %d: the reader must never bind a database method",
			sel.Sel.Name, fset.Position(sel.Pos()).Line)
		return true
	})

	// ── The constant route. ───────────────────────────────────────────
	//
	// Every string constant the file declares is checked: one that is a
	// STATEMENT must be a single read statement. A write can therefore
	// hide in a constant — used or not, at the top level or inside a
	// function, written as a literal or as a concatenation.
	for name, sql := range consts {
		if !p6IsSQLStatement(sql) {
			continue
		}
		if p := p6SQLStatementProblem(sql); p != "" {
			problem("the SQL constant %s is not a single read statement: %s: %q", name, p, sql)
		}
	}

	// ── The dead-SQL route. ───────────────────────────────────────────
	//
	// A SQL constant no statement uses is still a statement the reader
	// owns, and a file that declares SQL it never runs is a file whose
	// guard has nothing to check. Both are refused.
	for name, sql := range consts {
		if !p6IsSQLStatement(sql) {
			continue
		}
		if !usedConstants[name] {
			problem("the SQL constant %s is never used by a statement: %q", name, sql)
		}
	}

	// A file that declares SQL but issues no statement has a guard that
	// has nothing to check — so it is refused. A file that declares no
	// SQL at all (the refusal vocabulary) is fine: it is not a reader.
	if queries == 0 {
		for name, sql := range consts {
			if p6IsSQLStatement(sql) {
				problem("issues no query at all, yet declares the SQL constant %s: %q", name, sql)
			}
		}
	}
	return problems, queries
}

// p6ReaderSourceFiles are the files the reader consists of. The guard
// walks ALL of them: the reader is the two files together, and a guard
// that covered only one of them would leave half of the reader
// unguarded.
var p6ReaderSourceFiles = []string{
	"skill_package_read.go",
	"skill_package_read_errors.go",
}

// TestP6TheReaderNeverWrites pins that the reader issues no write.
//
// The read snapshot is a DEFERRED transaction: the SQLite driver ignores
// `sql.TxOptions{ReadOnly}` and issues a plain `BEGIN`, so the snapshot's
// read-only nature rests on DISCIPLINE. This test makes the discipline
// structural: the reader's source contains no `Exec` call at all, it
// binds no database method, and EVERY statement it owns — issued or
// merely declared — is a single read statement.
//
// It is a STATIC check on purpose. A dynamic one could only prove that
// the paths a test happens to exercise did not write; a source check
// proves that there is no statement they could have written.
func TestP6TheReaderNeverWrites(t *testing.T) {
	total := 0
	for _, file := range p6ReaderSourceFiles {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		problems, statements := p6ReaderWriteProblems(file, source)
		for _, p := range problems {
			t.Errorf("%s", p)
		}
		total += statements
	}
	if total == 0 {
		t.Fatalf("the reader files issue no query at all: the static guard found nothing to check")
	}
}

// p6GuardAnchor is a line in the reader that every evasion snippet is
// spliced in front of.
//
// It is a real statement the reader issues, so a snippet that replaces
// it with a write is a write the reader would issue — and the guard is
// run against the spliced copy.
const p6GuardAnchor = "\trows, err := tx.QueryContext(ctx, packageFilesQuery, skillID)"

// p6GuardEvasion is one way of writing in the reader, and the way the
// guard is expected to notice.
type p6GuardEvasion struct {
	// Name identifies the route.
	Name string

	// Snippet is the code spliced in front of the anchor.
	Snippet string
}

// p6GuardControl is a legitimate statement the guard must NOT refuse.
type p6GuardControl struct {
	// Name identifies the control.
	Name string

	// Snippet is the code spliced in front of the anchor.
	Snippet string
}

// p6GuardEvasions are the routes the guard must catch.
//
// Each is spliced into a COPY of the reader and the guard is run
// against the copy. The list is the evidence the §6.12 claim rests on,
// and it is pinned here so that it cannot quietly shrink.
var p6GuardEvasions = []p6GuardEvasion{
	{"a write through ExecContext",
		"\t_, _ = tx.ExecContext(ctx, \"UPDATE skill_package_files SET role = 'x'\")\n"},
	{"a write through Exec",
		"\t_, _ = tx.Exec(\"UPDATE skill_package_files SET role = 'x'\")\n"},
	{"a method value",
		"\texec := tx.ExecContext\n\t_, _ = exec(ctx, \"UPDATE skill_package_files SET role = 'x'\")\n"},
	{"a method value passed as an argument",
		"\tp6GuardHelper(tx.ExecContext)\n"},
	{"a write smuggled through QueryContext",
		"\t_, _ = tx.QueryContext(ctx, \"DELETE FROM skill_package_files WHERE skill_id = ?\", skillID)\n"},
	{"a write smuggled through PrepareContext",
		"\tstmt, _ := tx.PrepareContext(ctx, \"DELETE FROM skill_package_files WHERE skill_id = ?\")\n\t_ = stmt\n"},
	{"a write executed through a prepared statement",
		"\tstmt, _ := tx.PrepareContext(ctx, \"SELECT 1\")\n\t_, _ = stmt.ExecContext(ctx)\n"},
	{"a write in a USED constant",
		"\tconst p6GuardWrite = \"DELETE FROM skill_package_files\"\n\t_, _ = tx.QueryContext(ctx, p6GuardWrite, skillID)\n"},
	{"a write in an UNUSED constant",
		"\tconst p6GuardWrite = \"DELETE FROM skill_package_files\"\n\t_ = p6GuardWrite\n"},
	{"a write in a CONCATENATED constant",
		"\tconst p6GuardWrite = \"DELETE \" + \"FROM skill_package_files\"\n\t_ = p6GuardWrite\n"},
	{"a write in a WITH ... UPDATE constant",
		"\tconst p6GuardWrite = \"WITH x AS (SELECT 1) UPDATE skill_package_files SET role = 'x'\"\n\t_ = p6GuardWrite\n"},
	{"a write in a comment-prefixed constant",
		"\tconst p6GuardWrite = \"-- a note\\nDELETE FROM skill_package_files\"\n\t_ = p6GuardWrite\n"},
	{"a write behind a comment-SPLIT keyword",
		"\t_, _ = tx.QueryContext(ctx, \"DE/*x*/LETE FROM skill_package_files\", skillID)\n"},
	{"a write behind a comment-SPLIT keyword, line comment",
		"\t_, _ = tx.QueryContext(ctx, \"DE--x\\nLETE FROM skill_package_files\", skillID)\n"},
	{"a write with a comment BETWEEN its keywords",
		"\t_, _ = tx.QueryContext(ctx, \"DELETE/*x*/FROM skill_package_files\", skillID)\n"},
	{"an EXPLAIN DELETE",
		"\t_, _ = tx.QueryContext(ctx, \"EXPLAIN DELETE FROM skill_package_files\", skillID)\n"},
	{"a statement whose SQL is built at runtime",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT path FROM skill_package_files WHERE skill_id = ? \"+\"AND 1=1\", skillID)\n"},
	{"a multi-statement SELECT;DELETE",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT 1; DELETE FROM skill_package_files\", skillID)\n"},
	{"a dead SQL constant",
		"\tconst p6GuardDead = \"SELECT 1 FROM skill_package_files\"\n\t_ = p6GuardDead\n"},
}

// p6GuardControls are the legitimate statements the guard must NOT
// refuse.
//
// A guard that refuses a legitimate read is worse than no guard: it
// teaches its reader to route around it.
var p6GuardControls = []p6GuardControl{
	{"a plain SELECT",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT path FROM skill_package_files WHERE skill_id = ?\", skillID)\n"},
	{"a double-quoted SELECT",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT path FROM skill_package_files WHERE skill_id = ?\", skillID)\n"},
	{"a trailing semicolon",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT path FROM skill_package_files;\", skillID)\n"},
	{"a UNION subquery",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT path FROM skill_package_files WHERE skill_id = ? UNION SELECT path FROM skill_package_files\", skillID)\n"},
	{"a read-only CTE",
		"\t_, _ = tx.QueryContext(ctx, \"WITH x AS (SELECT 1 AS n) SELECT n FROM x\", skillID)\n"},
	{"a write keyword inside a string literal",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT path FROM skill_package_files WHERE status = 'UPDATE'\", skillID)\n"},
	{"an identifier that contains a write keyword",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT last_update FROM skill_package_files\", skillID)\n"},
	{"a semicolon inside a string literal",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT path FROM skill_package_files WHERE path = 'a;b'\", skillID)\n"},
	{"a doubled quote inside a string literal",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT path FROM skill_package_files WHERE path = 'it''s'\", skillID)\n"},
	{"a dash-dash inside a string literal",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT path FROM skill_package_files WHERE path = 'a--b'\", skillID)\n"},
	{"a bracket identifier containing a semicolon",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT [a;b] FROM skill_package_files\", skillID)\n"},
	{"a bracket identifier containing a write keyword",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT [DELETE] FROM skill_package_files\", skillID)\n"},
	{"a bind placeholder",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT path FROM skill_package_files WHERE skill_id = ?\", skillID)\n"},
}

// p6GuardHelper is the helper the method-value-as-an-argument route
// needs. It is never called at runtime: the guard refuses the
// REFERENCE before the reader could call it.
func p6GuardHelper(f any) {}

// TestP6TheReadOnlyGuardCatchesEveryEvasion pins the guard's own
// evidence.
//
// Each evasion is spliced into a COPY of the reader and the guard is run
// against the copy. The guard must REPORT it.
//
// This is the difference between a guard that is believed to work and
// one that is demonstrated to work — and it is demonstrated HERE, in the
// repository, so that the demonstration survives the next commit. A
// manual sweep proves nothing on the next commit; a pinned matrix does.
func TestP6TheReadOnlyGuardCatchesEveryEvasion(t *testing.T) {
	real, err := os.ReadFile("skill_package_read.go")
	if err != nil {
		t.Fatalf("read the reader: %v", err)
	}
	if !strings.Contains(string(real), p6GuardAnchor) {
		t.Fatalf("the reader does not contain the guard anchor %q: the matrix cannot be spliced", p6GuardAnchor)
	}

	for _, evasion := range p6GuardEvasions {
		mutated := strings.Replace(string(real), p6GuardAnchor, evasion.Snippet+p6GuardAnchor, 1)
		problems, _ := p6ReaderWriteProblems("skill_package_read.go", []byte(mutated))
		if len(problems) == 0 {
			t.Errorf("the guard did not catch %s", evasion.Name)
			continue
		}
		t.Logf("%s → %s", evasion.Name, problems[0])
	}
}

// TestP6TheReadOnlyGuardRefusesNoLegitimateRead pins that the guard
// does not refuse a legitimate read.
//
// A guard that refuses a legitimate statement is worse than no guard:
// it teaches its reader to route around it.
func TestP6TheReadOnlyGuardRefusesNoLegitimateRead(t *testing.T) {
	real, err := os.ReadFile("skill_package_read.go")
	if err != nil {
		t.Fatalf("read the reader: %v", err)
	}

	for _, control := range p6GuardControls {
		mutated := strings.Replace(string(real), p6GuardAnchor, control.Snippet+p6GuardAnchor, 1)
		problems, _ := p6ReaderWriteProblems("skill_package_read.go", []byte(mutated))
		if len(problems) != 0 {
			t.Errorf("the guard refused %s: %v", control.Name, problems)
		}
	}
}
