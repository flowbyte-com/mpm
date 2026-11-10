package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// extractLifecycleDDL pulls the lifecycle_events CREATE TABLE statement
// out of the Go schema source. It mirrors schema_preflight.extract_go_ddl
// for the one table install.sh embeds in the bundle; a checkout whose
// DDL is not extractable simply ships no DDL, and the preflight
// degrades gracefully rather than failing the build.
func extractLifecycleDDL(src string) (string, error) {
	const prefix = "CREATE TABLE IF NOT EXISTS lifecycle_events"
	var spans []string
	i := 0
	for {
		a := strings.Index(src[i:], "`")
		if a < 0 {
			break
		}
		a += i
		b := strings.Index(src[a+1:], "`")
		if b < 0 {
			break
		}
		b += a + 1
		spans = append(spans, src[a+1:b])
		i = b + 1
	}
	var cands []string
	for _, s := range spans {
		// The span is upper-cased and so must be the prefix: comparing
		// an upper-cased span against a mixed-case prefix can never
		// match, and the failure is SILENT — stageSchemaDDL treats an
		// unextractable DDL as "this checkout ships no DDL" and
		// returns nil, so the bundle was published with no schema
		// preflight and the collision check degraded to a no-op.
		//
		// scripts/lib/schema_preflight.py compares prefix.upper(), and
		// the two must agree or the Go side silently ships less than
		// the shell side.
		if strings.HasPrefix(strings.ToUpper(strings.TrimLeft(s, " \t\n")), strings.ToUpper(prefix)) {
			cands = append(cands, s)
		}
	}
	if len(cands) != 1 {
		return "", fmt.Errorf("no unambiguous lifecycle_events DDL (%d candidates)", len(cands))
	}
	ddl := cands[0]
	if !strings.HasSuffix(strings.TrimSpace(ddl), ";") {
		return "", fmt.Errorf("lifecycle_events DDL does not end with ';'")
	}
	return ddl, nil
}

// SchemaPreflightRelPath is where a bundle carries the substrate's
// lifecycle_events DDL.
//
// WHY THE BUNDLE CARRIES IT. The schema-collision preflight compares
// the live table against the DDL the substrate itself would apply. A
// bundle that carries that DDL lets the check run without a source
// checkout; a bundle that does not carry it degrades the check to a
// no-op, SILENTLY. That silent degradation is the failure this
// existence is here to prevent, so the preflight is a required part of
// a candidate bundle — not an optional extra.
const SchemaPreflightRelPath = "schema/lifecycle_events.sql"

// schemaPreflightProblems checks a bundle's schema preflight and
// returns one message per problem.
//
// REQUIRED (candidate scope): the preflight must be present and it must
// be EXACTLY ONE supported CREATE TABLE lifecycle_events statement. A
// bundle without one would install a runtime whose collision preflight
// cannot run; a bundle with a file that merely CONTAINS such a
// statement would have its file compiled by the updater before anyone
// knows what is in it.
//
// OPTIONAL (installed scope): a release promoted before the preflight
// travelled with the bundle may not carry one. That is a documented
// historic gap, not a corruption, so it is reported and not refused —
// but if the file IS there, it must satisfy the same rule.
func schemaPreflightProblems(bundleDir string, required bool) []string {
	path := filepath.Join(bundleDir, filepath.FromSlash(SchemaPreflightRelPath))
	fi, err := os.Lstat(path)
	if err != nil {
		if !required {
			return nil
		}
		return []string{fmt.Sprintf("the bundle carries no schema preflight (%s); the schema-collision preflight would degrade to a no-op", SchemaPreflightRelPath)}
	}
	if !fi.Mode().IsRegular() {
		return []string{fmt.Sprintf("the bundle's schema preflight (%s) is not a regular file", SchemaPreflightRelPath)}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{fmt.Sprintf("the bundle's schema preflight (%s) could not be read: %v", SchemaPreflightRelPath, err)}
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return []string{fmt.Sprintf("the bundle's schema preflight (%s) is empty; the schema-collision preflight would have nothing to compare the live table against", SchemaPreflightRelPath)}
	}
	var problems []string
	for _, why := range preflightStatementProblems(text) {
		problems = append(problems, fmt.Sprintf("the bundle's schema preflight (%s) is not one supported CREATE TABLE lifecycle_events statement: %s", SchemaPreflightRelPath, why))
	}
	return problems
}

// ---------------------------------------------------------------------------
// The preflight statement rule
// ---------------------------------------------------------------------------
//
// WHAT THIS IS. The schema-collision preflight COMPILES the candidate's
// reference DDL against an in-memory database and compares the shape
// it produces with the live table's shape (internal/update/preflight.go,
// checkTableShape). So the file is EXECUTED, and it is executed before
// anything else has proved what it contains.
//
// That makes the file's CONTENT a security boundary, and it is why a
// substring match is not enough. A file that contains
// "CREATE TABLE lifecycle_events (...)" somewhere is not a reference
// DDL: it may also contain a DROP, a DELETE, an ATTACH that writes a
// file outside the database, or a second statement whose whole purpose
// is to make the collision check pass.
//
// THE RULE. The file must be EXACTLY ONE statement:
//
//  1. it holds exactly one top-level statement — a ';' inside a
//     comment, a string literal or a quoted identifier does not end a
//     statement, and the substrate's own DDL contains a ';' inside a
//     comment, so a naive "count the semicolons" rule would reject
//     the canonical output;
//  2. that statement is terminated by ';', and nothing but whitespace
//     and comments follows it;
//  3. it begins with CREATE TABLE, optionally followed by IF NOT
//     EXISTS, and names the table lifecycle_events;
//  4. it opens a column list, its parentheses balance, and it ends
//     with the closing one.
//
// WHAT IT PROVES, AND WHAT IT DOES NOT. It proves the file is a
// single, well-formed CREATE TABLE for lifecycle_events — so
// compiling it can only create that one table, and it can do nothing
// else to the process that compiles it.
//
// It does NOT prove the statement is the substrate's DDL. The bundle
// ASSERTS that; nothing in the release format can check it (see
// SchemaIdentityIsUnproven). The validator therefore reports what it
// can actually establish, and does not claim an authenticity it does
// not have.
//
// THE TWO IMPLEMENTATIONS. This is the Go half; the shell half is
// rb_validate_schema_preflight in scripts/lib/runtime_bundle.sh. The
// parity test
// (TestTheSchemaPreflightRuleIsTheSameInGoAndInTheShell) runs both
// over the same matrix and requires the same verdict.

// SchemaIdentityIsUnproven records what the release format can and
// cannot establish about the preflight's provenance.
//
// The bundle carries the substrate's lifecycle_events DDL as
// schema/lifecycle_events.sql. Nothing ties that file to the schema
// the candidate's binary will actually apply: the binary embeds the
// DDL but exposes no way to read it back, and the bundle carries no
// source. So a bundle that ships a DIFFERENT — but syntactically
// valid — CREATE TABLE for lifecycle_events passes every check, and
// the collision check then compares the live table against the
// bundle's own claim rather than against the substrate's schema.
//
// That is a property of the FORMAT, not of this implementation, and
// it is the same property the bundle's host-asset declaration has: a
// publisher who controls the archive controls the binaries in it. The
// honest consequence is that the validator's success message must not
// claim the file IS the substrate's DDL. It says what it checked.
const SchemaIdentityIsUnproven = "the release format carries the preflight as a file the bundle asserts; nothing in it can independently prove that the file is the substrate's DDL, so a syntactically valid statement for a DIFFERENT schema would pass and could mask a genuine collision"

// preflightStatementProblems checks that raw is exactly one supported
// CREATE TABLE lifecycle_events statement, and returns one message per
// problem.
//
// See the block comment above for the rule and for why a substring
// match is not enough.
func preflightStatementProblems(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{"the file is empty"}
	}
	scan := scanPreflight(raw)
	var problems []string
	if scan.statements != 1 {
		problems = append(problems, fmt.Sprintf("the file must hold exactly one statement; it holds %d", scan.statements))
	}
	stmt := ""
	if len(scan.ends) > 0 {
		idx := scan.ends[0]
		stmt = strings.TrimSpace(scan.code[:idx])
		if rest := strings.TrimSpace(scan.code[idx+1:]); rest != "" {
			problems = append(problems, "the file carries SQL after the declaration; the preflight must be the declaration and nothing else")
		}
	}
	if stmt == "" {
		problems = append(problems, "the file holds no statement")
		return problems
	}
	if why := preflightHeadProblem(stmt); why != "" {
		problems = append(problems, why)
	}
	if !strings.HasSuffix(stmt, ")") {
		problems = append(problems, "the statement does not end with the closing parenthesis of its column list")
	}
	if scan.balance != 0 || scan.minBalance < 0 {
		problems = append(problems, "the statement's parentheses do not balance")
	}
	return problems
}

// preflightHeadProblem checks that a statement begins with
//
//	CREATE TABLE [IF NOT EXISTS] lifecycle_events (
//
// and returns "" when it does.
func preflightHeadProblem(stmt string) string {
	p := 0
	skipSpace := func() {
		for p < len(stmt) && isSQLSpace(stmt[p]) {
			p++
		}
	}
	word := func() string {
		start := p
		for p < len(stmt) && isSQLWordChar(stmt[p]) {
			p++
		}
		return stmt[start:p]
	}
	skipSpace()
	if !strings.EqualFold(word(), "CREATE") {
		return "the statement does not begin with CREATE TABLE"
	}
	skipSpace()
	if !strings.EqualFold(word(), "TABLE") {
		return "the statement does not begin with CREATE TABLE"
	}
	// IF NOT EXISTS is optional.
	mark := p
	skipSpace()
	if strings.EqualFold(word(), "IF") {
		skipSpace()
		if !strings.EqualFold(word(), "NOT") {
			return "the statement does not begin with CREATE TABLE"
		}
		skipSpace()
		if !strings.EqualFold(word(), "EXISTS") {
			return "the statement does not begin with CREATE TABLE"
		}
	} else {
		p = mark
	}
	skipSpace()
	name, ok := readSQLIdentifier(stmt, &p)
	if !ok {
		return "the statement does not name a table"
	}
	if !strings.EqualFold(name, "lifecycle_events") {
		return fmt.Sprintf("the statement declares the table %q, not lifecycle_events", name)
	}
	skipSpace()
	if p >= len(stmt) || stmt[p] != '(' {
		return "the statement does not open a column list"
	}
	return ""
}

// readSQLIdentifier reads a table name — bare, "quoted", `quoted` or
// [bracketed] — and returns it unquoted.
func readSQLIdentifier(s string, p *int) (string, bool) {
	if *p >= len(s) {
		return "", false
	}
	switch s[*p] {
	case '"', '`':
		quote := s[*p]
		start := *p + 1
		i := start
		for i < len(s) {
			if s[i] == quote {
				if i+1 < len(s) && s[i+1] == quote {
					i += 2
					continue
				}
				*p = i + 1
				return s[start:i], true
			}
			i++
		}
		return "", false
	case '[':
		start := *p + 1
		i := start
		for i < len(s) {
			if s[i] == ']' {
				*p = i + 1
				return s[start:i], true
			}
			i++
		}
		return "", false
	}
	start := *p
	for *p < len(s) && isSQLWordChar(s[*p]) {
		*p++
	}
	if *p == start {
		return "", false
	}
	return s[start:*p], true
}

func isSQLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isSQLWordChar(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// preflightScan is what a walk over a preflight's text establishes.
type preflightScan struct {
	// code is the text with every comment removed. String literals and
	// quoted identifiers are kept verbatim.
	code string
	// statements counts the top-level ';' terminators.
	statements int
	// ends holds the offset in code of each top-level ';'. It is
	// needed because a ';' INSIDE a string literal survives into the
	// code, so the first ';' in the code is not necessarily the end of
	// the first statement.
	ends []int
	// balance is the parenthesis balance at the end of the text.
	balance int
	// minBalance is the lowest balance reached; below zero means a
	// closing parenthesis with no opening one.
	minBalance int
}

// scanPreflight walks the text of a preflight and reports what it
// contains.
//
// IT IS COMMENT- AND STRING-AWARE, and it has to be: the substrate's
// own DDL contains a ';' inside a line comment, so a rule that counts
// semicolons without looking at comments would reject the canonical
// output. It also has to be string-aware, because a ';' inside a
// string literal does not end a statement.
//
// The scanner never executes anything. It is a lexical pass, and it is
// the same pass the shell validator performs.
func scanPreflight(text string) preflightScan {
	var out strings.Builder
	out.Grow(len(text))
	var scan preflightScan
	i := 0
	n := len(text)
	for i < n {
		c := text[i]
		switch {
		case c == '-' && i+1 < n && text[i+1] == '-':
			// A line comment runs to the end of the line.
			for i < n && text[i] != '\n' {
				i++
			}
			out.WriteByte(' ')
		case c == '/' && i+1 < n && text[i+1] == '*':
			// A block comment runs to its closing delimiter. SQLite
			// does not nest them.
			i += 2
			for i+1 < n && !(text[i] == '*' && text[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			} else {
				i = n
			}
			out.WriteByte(' ')
		case c == '\'' || c == '"' || c == '`':
			quote := c
			out.WriteByte(c)
			i++
			for i < n {
				if text[i] == quote {
					// A doubled quote is an escaped quote, not the
					// end of the literal.
					if i+1 < n && text[i+1] == quote {
						out.WriteByte(quote)
						out.WriteByte(quote)
						i += 2
						continue
					}
					out.WriteByte(quote)
					i++
					break
				}
				out.WriteByte(text[i])
				i++
			}
		case c == ';':
			scan.statements++
			scan.ends = append(scan.ends, out.Len())
			out.WriteByte(c)
			i++
		case c == '(':
			scan.balance++
			if scan.balance < scan.minBalance {
				scan.minBalance = scan.balance
			}
			out.WriteByte(c)
			i++
		case c == ')':
			scan.balance--
			if scan.balance < scan.minBalance {
				scan.minBalance = scan.balance
			}
			out.WriteByte(c)
			i++
		default:
			out.WriteByte(c)
			i++
		}
	}
	scan.code = out.String()
	return scan
}
