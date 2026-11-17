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
// by a space (which keeps the tokens either side of it separate):
//
//	DE/*x*/LETE FROM t   -> removed: DELETE FROM t
//	DELETE/*x*/FROM t    -> spaced:  DELETE FROM t
//
// The SPACED reading is SQLite's own tokenization: a comment is
// whitespace, so SQLite NEVER merges the tokens either side of one.
// Every EXECUTABLE write is therefore visible in the spaced reading,
// and the full check - the first word, the single-statement rule and
// the write-keyword scan - runs on it.
//
// The REMOVED reading sees what SQLite does not: a keyword split by a
// comment. It contributes exactly ONE rule - if its first word is a
// WRITE keyword, the text is refused. That catches `DE/*x*/LETE`,
// which SQLite rejects as a syntax error and which therefore costs
// nothing to refuse, WITHOUT manufacturing a keyword out of two
// unrelated tokens - which is what would make `SELECT up/*x*/date` a
// false positive.
//
//// WHAT THE GUARD CHECKS
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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
//
// `GRANT` and `REVOKE` are in the list, and in NEITHER the write list:
// SQLite implements neither, so `GRANT;` and `REVOKE;` are syntax errors.
// A leading one is refused by the first-word rule, which is all there is
// to refuse.
var p6SQLKeywords = []string{
	"SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "DROP", "ALTER",
	"CREATE", "PRAGMA", "VACUUM", "ATTACH", "DETACH", "BEGIN", "COMMIT",
	"ROLLBACK", "SAVEPOINT", "RELEASE", "REASSIGN", "REINDEX", "ANALYZE",
	"WITH", "EXPLAIN", "END",
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
	"SAVEPOINT", "RELEASE", "REASSIGN", "REINDEX", "ANALYZE",
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

// p6SQLWord is one word of a scrubbed SQL text.
type p6SQLWord struct {
	// Text is the word itself.
	Text string

	// FunctionCall reports whether the word is immediately followed by
	// an opening parenthesis - that is, whether it names a FUNCTION.
	FunctionCall bool
}

// p6SQLWords splits a SQL text into its keywords and identifiers.
//
// A word is a run of letters, digits and UNDERSCORES, so an identifier
// like `last_update` is ONE word and is never mistaken for the keyword
// `UPDATE`.
//
// A word immediately followed by `(` names a FUNCTION, and a function
// call is never a statement keyword: `SELECT replace(x,'a','b')` is a
// read, and `REPLACE` is a write keyword only in `REPLACE INTO`.
func p6SQLWords(sql string) []p6SQLWord {
	runes := []rune(sql)
	var words []p6SQLWord
	var current strings.Builder

	// flush ends the word that ends just before `end`, and records
	// whether an opening parenthesis follows it.
	flush := func(end int) {
		if current.Len() == 0 {
			return
		}
		word := p6SQLWord{Text: current.String()}
		i := end
		for i < len(runes) && (runes[i] == ' ' || runes[i] == '\t' ||
			runes[i] == '\r' || runes[i] == '\n') {
			i++
		}
		if i < len(runes) && runes[i] == '(' {
			word.FunctionCall = true
		}
		words = append(words, word)
		current.Reset()
	}

	for i, r := range runes {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			current.WriteRune(r)
		default:
			flush(i)
		}
	}
	if current.Len() > 0 {
		flush(len(runes))
	}
	return words
}

// p6SQLTopLevelWords returns the words of a scrubbed SQL text that sit
// OUTSIDE every parenthesised group.
//
// A parenthesised group is a subquery or a CTE body, and SQLite allows
// no DML in either - a CTE body that holds a DELETE is a syntax error,
// and so is a subquery that holds one. So the words that can carry a
// write are the TOP-LEVEL ones: the statement's own verb, and the
// words around it.
func p6SQLTopLevelWords(code string) []p6SQLWord {
	runes := []rune(code)
	var words []p6SQLWord
	var current strings.Builder
	depth := 0

	flush := func(end int) {
		if current.Len() == 0 {
			return
		}
		word := p6SQLWord{Text: current.String()}
		i := end
		for i < len(runes) && (runes[i] == ' ' || runes[i] == '\t' ||
			runes[i] == '\r' || runes[i] == '\n') {
			i++
		}
		if i < len(runes) && runes[i] == '(' {
			word.FunctionCall = true
		}
		if depth == 0 {
			words = append(words, word)
		}
		current.Reset()
	}

	for i, r := range runes {
		switch {
		case r == '(':
			depth++
			flush(i)
		case r == ')':
			flush(i)
			depth--
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			current.WriteRune(r)
		default:
			flush(i)
		}
	}
	flush(len(runes))
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

// p6SQLBody normalises a scrubbed SQL text: upper-cased, with the
// trailing whitespace and a single trailing semicolon removed.
func p6SQLBody(code string) string {
	body := strings.TrimRight(strings.ToUpper(code), " \t\r\n")
	body = strings.TrimSuffix(body, ";")
	body = strings.TrimRight(body, " \t\r\n")
	return body
}

// p6SQLFirstKeyword returns the first keyword of a scrubbed SQL text.
//
// It returns the empty string when the text has no words, or when its
// first word is not a SQL keyword - which is how a non-SQL constant
// ("skill_package", "manifest_identity") is told from a statement.
func p6SQLFirstKeyword(code string) string {
	words := p6SQLWords(p6SQLBody(code))
	if len(words) == 0 {
		return ""
	}
	first := strings.ToUpper(words[0].Text)
	if !p6WordIn(first, p6SQLKeywords) {
		return ""
	}
	return first
}

// p6IsSQLStatement reports whether a SQL text is a STATEMENT at all.
//
// A text is a statement if EITHER reading's first word is a SQL
// keyword: the spaced reading is SQLite's own tokenization, and the
// removed reading sees a keyword that a comment split.
func p6IsSQLStatement(sql string) bool {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return false
	}
	for _, spaceComments := range []bool{false, true} {
		if p6SQLFirstKeyword(p6ScrubSQL(trimmed, spaceComments)) != "" {
			return true
		}
	}
	return false
}

// p6SQLStatementProblem describes why a SQL text is not a single read
// statement, or returns the empty string when it is one.
//
// The check is STATEMENT-based, and it is anchored on SQLITE'S OWN
// GRAMMAR rather than on a keyword list:
//
//   - the SPACED reading IS SQLite's tokenization: SQLite treats a
//     comment as whitespace, so `DELETE/*x*/FROM t` is two tokens,
//     DELETE and FROM, and every EXECUTABLE write is visible in it;
//
//   - a statement must be a SINGLE statement - a `;` anywhere but at
//     the very end means another statement follows;
//
//   - a statement that BEGINS with `SELECT` is a read, full stop:
//     SQLite allows no DML in a subquery (`SELECT * FROM (DELETE …)` is
//     a syntax error), no DML in a CTE body, and no `SELECT … INTO`. So
//     a statement that begins with SELECT needs no further check;
//
//   - a statement that BEGINS with `WITH` is read only if it carries no
//     write keyword at the TOP LEVEL. SQLite's only legal main
//     statements after a CTE list are SELECT, INSERT, UPDATE, DELETE
//     and REPLACE, and the last four are write keywords; the main
//     statement is at the top level, so the top-level words carry it.
//
//     The price is OVER-STRICTNESS: a write keyword used as an
//     IDENTIFIER at the top level - a CTE's own name, a column
//     reference in the main statement - is refused too, even though
//     SQLite would accept it. The same word inside the COLUMN LIST
//     (`WITH x (release) AS ...`) is not affected, because it sits
//     inside parentheses. The reader's own statements contain no
//     `WITH`, so nothing it owns is affected, and the refusal fails
//     CLOSED;
//
//   - anything else - `DELETE`, `INSERT`, `PRAGMA`, `EXPLAIN`, `END`,
//     `VACUUM`, … - is refused, because the reader issues plain SELECTs
//     and nothing else.
//
// The REMOVED reading rejoins a keyword that a comment split, which
// SQLite does NOT do. It contributes exactly ONE rule: if its first word
// is a WRITE keyword, the text is refused. That catches `DE/*x*/LETE` -
// which SQLite rejects as a syntax error, so refusing it costs nothing -
// WITHOUT manufacturing a keyword out of two unrelated tokens, which is
// what would make `SELECT up/*x*/date` a false positive.
//
// A word immediately followed by `(` names a FUNCTION, and a function
// call is never a statement keyword: `SELECT replace(x,'a','b')` is a
// read.
func p6SQLStatementProblem(sql string) string {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return ""
	}

	spacedWords := p6SQLWords(p6SQLBody(p6ScrubSQL(trimmed, true)))
	if len(spacedWords) == 0 {
		return ""
	}
	spacedFirst := strings.ToUpper(spacedWords[0].Text)

	removedFirst := ""
	if removedWords := p6SQLWords(p6SQLBody(p6ScrubSQL(trimmed, false))); len(removedWords) > 0 {
		removedFirst = strings.ToUpper(removedWords[0].Text)
	}

	// The effective first keyword: SQLite's own, unless SQLite's is
	// not a keyword at all and the removed reading has one - which is
	// what happens when a comment splits the first keyword.
	effective := spacedFirst
	if !p6WordIn(spacedFirst, p6SQLKeywords) && p6WordIn(removedFirst, p6SQLKeywords) {
		effective = removedFirst
	}

	// A write keyword under EITHER reading is a write.
	for _, first := range []string{spacedFirst, removedFirst} {
		if p6WordIn(first, p6SQLWriteKeywords) {
			return "the statement begins with " + first + ", not SELECT"
		}
	}

	// A single statement. The `;` is looked for in the SPACED body,
	// which is SQLite's tokenization: a `;` inside a comment or a
	// string literal is not a separator.
	body := p6SQLBody(p6ScrubSQL(trimmed, true))
	if strings.Contains(body, ";") {
		return "the statement carries more than one statement"
	}

	switch effective {
	case "SELECT":
		// A SELECT cannot write: SQLite allows no DML in a
		// subquery, none in a CTE body, and no `SELECT … INTO`.
		// Nothing more to check.
	case "WITH":
		// The main statement follows the CTE list, at the top
		// level, so the top-level words carry it.
		for _, word := range p6SQLTopLevelWords(body) {
			if word.FunctionCall {
				continue
			}
			upper := strings.ToUpper(word.Text)
			if p6WordIn(upper, p6SQLWriteKeywords) {
				return "the statement carries the write keyword " + upper
			}
		}
	default:
		return "the statement begins with " + effective + ", not SELECT"
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
// walks ALL of them: the reader is the four files together, and a guard
// that covered only one of them would leave half of the reader
// unguarded.
//
// The third file is the skill-document loader. It is part of the reader
// because the `read` action loads the skill and its package provenance
// from ONE snapshot, and the skill's own SELECTs are therefore part of
// the read path the guard covers. Without it, a write could hide in the
// skill loader and the guard would never see it.
//
// The fourth file is the EXPORTER (P7). The exporter is a read path too -
// it reads a package, verifies it and serialises it - and its contract is
// that it never repairs, never mutates and never rescans. The guard
// makes that structural: a write could not hide in the exporter and go
// unseen.
var p6ReaderSourceFiles = []string{
	"skill_package_read.go",
	"skill_package_read_errors.go",
	"skill_document.go",
	"skill_package_export.go",
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

// p6GuardDigest is the SHA-256 of a snippet, as the matrix pins it.
func p6GuardDigest(snippet string) string {
	sum := sha256.Sum256([]byte(snippet))
	return hex.EncodeToString(sum[:])
}

// TestP6PrintGuardRouteDigests prints the digest of every required
// route, so the pin is generated rather than hand-typed.
//
// It prints nothing unless MPM_P6_PRINT_DIGESTS is set, so it does not
// pollute the test output.
func TestP6PrintGuardRouteDigests(t *testing.T) {
	if os.Getenv("MPM_P6_PRINT_DIGESTS") == "" {
		t.Skip("set MPM_P6_PRINT_DIGESTS to print the route digests")
	}
	for _, evasion := range p6GuardEvasions {
		if _, required := p6GuardRequiredRoutes[evasion.Name]; required {
			t.Logf("%q: %q,", evasion.Name, p6GuardDigest(evasion.Snippet))
		}
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

// p6GuardEvasionCount is the number of routes the matrix must have.
//
// It is pinned so that the matrix cannot quietly SHRINK: removing a
// route without changing this number fails the test, and the failure
// says exactly what happened.
const p6GuardEvasionCount = 22

// p6GuardControlCount is the number of controls the matrix must have.
//
// It is pinned for the same reason: a control that is dropped is a
// legitimate read the guard is no longer proven to accept.
const p6GuardControlCount = 17

// p6GuardRequiredRoutes are the routes that have caught a real defect
// at least once.
//
// They are pinned by name AND by the SHA-256 of their snippet, so that
// the route which caught the last bug can be neither deleted nor
// quietly substituted: a name pin alone stops a DELETION, and only the
// digest stops a SWAP.
//
// The digests are generated by `p6PrintGuardRouteDigests` - a test
// that prints them - so they are never hand-typed.
var p6GuardRequiredRoutes = map[string]string{
	"a write through ExecContext":                 "9fb342efc1e5988429151a78e082097874a59b083bff5fd1fc67f22f5785191d",
	"a method value":                              "a92eb81b2f9efda2ea578636be3e039250b56e6979eb63ee3bca5366a80d937c",
	"a method value passed as an argument":        "4443359bd763835656d6062aef92d97162d2123f0d09294fb161020bd50f52b1",
	"a write smuggled through QueryContext":       "7e5e04902e2495c9f125a2c9926ad8ba8c0d0a8992a0eef2aea0c7da595d91b1",
	"a write smuggled through PrepareContext":     "d3aaf28f17f9f003ebe3425991baa4949928789be547d8a57df04febe5f909c3",
	"a write in a USED constant":                  "9a65f6f1924993fd1a82d4e88dbdaa94cede28ce6f0302b5bb80bf83801bd8c4",
	"a write in an UNUSED constant":               "73b592094c7ed69b094037561734619ed3b33dead92cfa3b3fa93713be2ad534",
	"a write in a CONCATENATED constant":          "95498530ac92e3c0075f24371cc386d21a1a508c30c990f40ceed0fe93bd8356",
	"a write in a WITH ... UPDATE constant":       "f52d954b9cdc4fc5b62828dc6cae28c8a337c7634182c6cc811ca406dabe51b9",
	"a write behind a comment-SPLIT keyword":      "9eb29003179dfcd4e21e6a6b504a9bc0371421afd7beda4bc4602cb250034511",
	"a write with a comment BETWEEN its keywords": "5528eaa1a03e529f96b300459f0d7ab07017dc5ff7f5266c53f6c101149287e8",
	"a dead SQL constant":                         "ab9fcc0ad31f454a862d8b7f79e9b32871f30f8da33131ee97560b98d9525ef5",
	"an END - a transaction control":              "a4c7d4ddf78e91ad03ba99bd8c6bf5ac120c48c946d1d5e879a824818e20bfca",
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
	{"a statement whose SQL is a concatenation of literals",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT path FROM skill_package_files WHERE skill_id = ? \"+\"AND 1=1\", skillID)\n"},
	{"a statement whose SQL is built at run time",
		"\t_, _ = tx.QueryContext(ctx, fmt.Sprintf(\"SELECT %s FROM skill_package_files\", \"path\"), skillID)\n"},
	{"a multi-statement SELECT;DELETE",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT 1; DELETE FROM skill_package_files\", skillID)\n"},
	{"a dead SQL constant",
		"\tconst p6GuardDead = \"SELECT 1 FROM skill_package_files\"\n\t_ = p6GuardDead\n"},
	{"an END - a transaction control",
		"\t_, _ = tx.QueryContext(ctx, \"END\", skillID)\n"},
	{"a REVOKE",
		"\t_, _ = tx.QueryContext(ctx, \"REVOKE\", skillID)\n"},
}

// p6GuardControls are the legitimate statements the guard must NOT
// refuse.
//
// A guard that refuses a legitimate read is worse than no guard: it
// teaches its reader to route around it.
var p6GuardControls = []p6GuardControl{
	{"a plain SELECT, written as a raw backtick literal",
		"\t_, _ = tx.QueryContext(ctx, `SELECT path FROM skill_package_files WHERE skill_id = ?`, skillID)\n"},
	{"a plain SELECT, written as a double-quoted literal",
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
	{"a function whose name is a statement keyword",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT replace(path, 'a', 'b') FROM skill_package_files\", skillID)\n"},
	{"a column named after a statement keyword",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT replace FROM skill_package_files\", skillID)\n"},
	{"an identifier split by a comment",
		"\t_, _ = tx.QueryContext(ctx, \"SELECT up/*x*/date FROM skill_package_files\", skillID)\n"},
	{"a CTE whose body is a read",
		"\t_, _ = tx.QueryContext(ctx, \"WITH x AS (SELECT 1 AS n) SELECT n FROM x\", skillID)\n"},
}

// TestP6TheGuardRefusesARouteSQLiteReallyExecutes pins that the
// matrix is not merely about the guard's CLASSIFIER.
//
// The matrix shows that the guard refuses these texts. That is not the
// same as showing that the texts would write - a statement SQLite
// rejects as a syntax error proves nothing. So one route is checked
// against SQLite itself: it is run against a real database, and the row
// it would have removed is gone.
//
// The database is an in-memory one, created here, with the reader's
// own table. The live MPM database is never opened.
func TestP6TheGuardRefusesARouteSQLiteReallyExecutes(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open the in-memory database: %v", err)
	}
	// `:memory:` gives EVERY connection its own private database, so
	// the pool must not open a second one: the table would then live in
	// a different database from the query. This handle is the test's
	// own, so a single connection is safe - the shared DatabaseManager's
	// single-connection trap does not apply here.
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec("CREATE TABLE skill_package_files (skill_id TEXT, path TEXT, role TEXT)"); err != nil {
		t.Fatalf("create the table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO skill_package_files VALUES ('s1', 'SKILL.md', 'entrypoint')"); err != nil {
		t.Fatalf("insert the row: %v", err)
	}

	// The only side effect a SELECT could otherwise have is loading a
	// native extension. This build refuses it - SQLite is compiled with
	// SQLITE_OMIT_LOAD_EXTENSION - so the function is refused at run
	// time. The assertion pins that, so a change to the build is
	// visible here rather than in a security review.
	if _, err := db.Exec("SELECT load_extension('/nonexistent-mpm-p6.so')"); err == nil {
		t.Errorf("the in-memory database loaded an extension: this build no longer refuses one")
	}

	// A legitimate read leaves the row alone.
	if _, err := db.Exec("SELECT path FROM skill_package_files"); err != nil {
		t.Fatalf("the legitimate read failed: %v", err)
	}
	if rows := p6CountRows(t, db); rows != 1 {
		t.Fatalf("the legitimate read changed the table: %d rows, want 1", rows)
	}

	// The route: a comment where a space belongs. SQLite treats the
	// comment as whitespace, so this is a real DELETE.
	const route = "DELETE/*x*/FROM skill_package_files"

	// The guard refuses it.
	if problem := p6SQLStatementProblem(route); problem == "" {
		t.Errorf("the guard did not refuse %q, which SQLite executes", route)
	}

	// And SQLite really executes it.
	if _, err := db.Exec(route); err != nil {
		t.Fatalf("SQLite refused the route %q: %v", route, err)
	}
	if rows := p6CountRows(t, db); rows != 0 {
		t.Errorf("the route %q left %d rows: it does not write, so the matrix proves nothing", route, rows)
	}
}

// p6CountRows counts the rows of the reader's own table.
func p6CountRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM skill_package_files").Scan(&rows); err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	return rows
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
	if len(p6GuardEvasions) != p6GuardEvasionCount {
		t.Errorf("the evasion matrix has %d routes, want %d: a route cannot be removed without changing the pinned count",
			len(p6GuardEvasions), p6GuardEvasionCount)
	}
	names := map[string]string{}
	for _, evasion := range p6GuardEvasions {
		if _, duplicate := names[evasion.Name]; duplicate {
			t.Errorf("the evasion matrix has two routes named %q: every route must be distinct", evasion.Name)
		}
		names[evasion.Name] = p6GuardDigest(evasion.Snippet)
	}
	for required, digest := range p6GuardRequiredRoutes {
		actual, present := names[required]
		if !present {
			t.Errorf("the evasion matrix is missing the route %q: it caught a real defect once, and cannot be dropped", required)
			continue
		}
		if actual != digest {
			t.Errorf("the route %q was changed: its snippet no longer matches the pinned digest %s (it is now %s)", required, digest, actual)
		}
	}

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
	if len(p6GuardControls) != p6GuardControlCount {
		t.Errorf("the control matrix has %d controls, want %d: a control cannot be removed without changing the pinned count",
			len(p6GuardControls), p6GuardControlCount)
	}
	names := map[string]bool{}
	for _, control := range p6GuardControls {
		if names[control.Name] {
			t.Errorf("the control matrix has two controls named %q: every control must be distinct", control.Name)
		}
		names[control.Name] = true
	}

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
