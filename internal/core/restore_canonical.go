package internal

// Restore canonical trigger identity (contract §4.6/B1).
//
// The dump is never a source of executable trigger code for the four
// managed tables (agent_tasks, agent_task_events, agent_task_sessions,
// agent_task_supersessions). For every managed-table trigger the restore
// handler extracts the statement and its name, rejects duplicates and any
// trigger not in the canonical inventory for the detected schema class,
// normalises the definition, and compares it with the trusted canonical
// DDL constant used by the migration and its pinning tests. A same-named
// trigger with an altered or silently weakened body is refused before
// replay and can never replace an authoritative recovery guard.
//
// Normalisation ignores IF NOT EXISTS, identifier quoting and schema
// qualification, comments and whitespace; it case-folds SQL
// keywords/identifiers and preserves string literals byte for byte.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// managedRestoreTables are the tables whose triggers are canonical.
var managedRestoreTables = map[string]bool{
	"agent_tasks":              true,
	"agent_task_events":        true,
	"agent_task_sessions":      true,
	"agent_task_supersessions": true,
}

// canonicalTriggerSources are the trusted DDL texts, in migration order.
// The supersession entry is assembled at runtime from the same helpers
// the migration uses, so the inventory cannot drift from the migration.
func canonicalTriggerSources() []struct {
	class string
	ddl   string
} {
	return []struct {
		class string
		ddl   string
	}{
		{"pr1", agentAssignmentDDL},
		{"sessions", agentTaskSessionDDL},
		{"recovery", agentTaskSupersessionTableDDL + agentTaskSupersessionTriggerDDL},
	}
}

// canonicalManagedTriggers parses the trusted DDL into name → normalized
// body. Every CREATE TRIGGER statement in the migration DDL contributes
// exactly one entry; a duplicate name in the trusted text is a
// programming error.
func canonicalManagedTriggers() (map[string]string, error) {
	out := map[string]string{}
	for _, src := range canonicalTriggerSources() {
		for _, stmt := range splitTriggerStatements(src.ddl) {
			name, body, err := parseTriggerStatement(stmt)
			if err != nil {
				return nil, err
			}
			if _, dup := out[name]; dup {
				return nil, fmt.Errorf("restore: duplicate canonical trigger %q", name)
			}
			out[name] = normalizeTriggerBody(body)
		}
	}
	return out, nil
}

// canonicalTriggerNamesForClass returns the required managed-trigger set
// for a backup class. Pre-feature classes require only the triggers
// their schema could carry; the supersession-aware class requires all
// of them. Triggers added by in-transaction additive migrations are
// installed by the migration, not required from the dump.
func canonicalTriggerNamesForClass(class restoreClass, inventory map[string]string) []string {
	// Ordinary backups carry no assignment schema: there is nothing to
	// verify. Migrations add the schema in-transaction.
	if class == restoreClassOrdinary {
		return nil
	}
	allowed := map[string]bool{"pr1": true}
	if class == restoreClassPR3 || class == restoreClassAware {
		allowed["sessions"] = true
	}
	if class == restoreClassAware {
		allowed["recovery"] = true
	}
	bySource := map[string][]string{}
	for _, src := range canonicalTriggerSources() {
		for _, stmt := range splitTriggerStatements(src.ddl) {
			name, _, err := parseTriggerStatement(stmt)
			if err != nil {
				continue
			}
			bySource[src.class] = append(bySource[src.class], name)
		}
	}
	var names []string
	for srcClass, list := range bySource {
		if !allowed[srcClass] {
			continue
		}
		for _, n := range list {
			if _, ok := inventory[n]; ok {
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	return names
}

// splitTriggerStatements splits migration DDL into its CREATE TRIGGER
// statements, honouring string literals and line comments. Each
// migration trigger contains exactly one BEGIN...END block, so the
// statement ends at the END closed by a semicolon.
func splitTriggerStatements(ddl string) []string {
	return extractTriggersByEnd(ddl)
}

// extractTriggersByEnd finds CREATE TRIGGER ... END; spans honouring
// strings and comments.
func extractTriggersByEnd(ddl string) []string {
	var out []string
	upper := strings.ToUpper(ddl)
	i := 0
	for i < len(ddl) {
		idx := strings.Index(upper[i:], "CREATE TRIGGER")
		if idx < 0 {
			break
		}
		start := i + idx
		// Find the terminating END; : the END that closes the
		// trigger is followed by optional whitespace then `;`.
		// Inner BEGIN...END blocks (CASE has no END; the body has
		// a single BEGIN...END) — the migration triggers each
		// contain exactly one BEGIN and one closing END.
		j := start
		inStr := false
		inLineComment := false
		end := -1
		for j < len(ddl) {
			c := ddl[j]
			if inLineComment {
				if c == '\n' {
					inLineComment = false
				}
				j++
				continue
			}
			if c == '\'' {
				if inStr && j+1 < len(ddl) && ddl[j+1] == '\'' {
					j += 2
					continue
				}
				inStr = !inStr
				j++
				continue
			}
			if inStr {
				j++
				continue
			}
			if c == '-' && j+1 < len(ddl) && ddl[j+1] == '-' {
				inLineComment = true
				j += 2
				continue
			}
			if (c == 'E' || c == 'e') && isTriggerEnd(ddl, upper, j) {
				// Confirm `;` follows (allow whitespace/comments).
				k := j + 3
				for k < len(ddl) && (ddl[k] == ' ' || ddl[k] == '\t' || ddl[k] == '\n' || ddl[k] == '\r') {
					k++
				}
				if k < len(ddl) && ddl[k] == ';' {
					end = k + 1
					break
				}
			}
			j++
		}
		if end < 0 {
			break
		}
		out = append(out, ddl[start:end])
		i = end
	}
	return out
}

// isTriggerEnd reports whether ddl[j:j+3] is the word END.
func isTriggerEnd(ddl, upper string, j int) bool {
	if j+3 > len(ddl) || upper[j:j+3] != "END" {
		return false
	}
	before := j == 0 || !isIdentChar(ddl[j-1])
	after := j+3 >= len(ddl) || !isIdentChar(ddl[j+3])
	return before && after
}

func isIdentChar(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// parseTriggerStatement splits a CREATE TRIGGER statement into its name
// and its body (everything from the trigger name onward is the body for
// normalisation; the name is extracted from the header).
//
// The name may be bare, quoted, or schema-qualified, and the qualifier
// and the name may be quoted independently — `"main"."trg_x"`,
// `main.trg_x`, `[main].[trg_x]` and `trg_x` all name `trg_x` (B1).
func parseTriggerStatement(stmt string) (name, body string, err error) {
	s := strings.TrimSpace(stmt)
	upper := strings.ToUpper(s)
	if !strings.HasPrefix(upper, "CREATE TRIGGER") {
		return "", "", fmt.Errorf("restore: not a trigger statement")
	}
	rest := strings.TrimSpace(s[len("CREATE TRIGGER"):])
	restUpper := strings.ToUpper(rest)
	if strings.HasPrefix(restUpper, "IF NOT EXISTS") {
		rest = strings.TrimSpace(rest[len("IF NOT EXISTS"):])
	}
	name, _, ok := parseQualifiedIdentifier(rest)
	if !ok || name == "" {
		return "", "", fmt.Errorf("restore: trigger without a name")
	}
	return name, s, nil
}

// parseTriggerTarget returns the table a CREATE TRIGGER is defined ON.
//
// The target lives in the validated statement header (`... ON <table>
// ... BEGIN`), so it is parsed from there rather than guessed from the
// body. A body that merely READS a managed table does not make its
// trigger a managed-table trigger (F11).
func parseTriggerTarget(stmt string) (string, bool) {
	s := stripSQLComments(strings.TrimSpace(stmt))
	upper := strings.ToUpper(s)
	if !strings.HasPrefix(upper, "CREATE TRIGGER") {
		return "", false
	}
	// The header ends at BEGIN (or at the WHEN clause's expression,
	// which follows ON). Scan tokens and keep the LAST `ON` token
	// before BEGIN: the header grammar contains exactly one, and the
	// event's optional column list cannot contain an unquoted ON.
	begin := strings.Index(upper, "BEGIN")
	header := s
	if begin >= 0 {
		header = s[:begin]
	}
	tokens := tokenizeSQL(header)
	target := ""
	for i, t := range tokens {
		if t.kind != sqlTokenWord || !strings.EqualFold(t.text, "ON") || i+1 >= len(tokens) {
			continue
		}
		// The target may be schema-qualified (`main.memories`,
		// `"main"."memories"`): take the FINAL component.
		j := i + 1
		if tokens[j].kind != sqlTokenWord && tokens[j].kind != sqlTokenIdentifier {
			continue
		}
		component := tokens[j].text
		j++
		for j+1 < len(tokens) &&
			tokens[j].kind == sqlTokenOperator && tokens[j].text == "." &&
			(tokens[j+1].kind == sqlTokenWord || tokens[j+1].kind == sqlTokenIdentifier) {
			component = tokens[j+1].text
			j += 2
		}
		target = component
	}
	if target == "" {
		return "", false
	}
	return strings.ToLower(target), true
}

// ── identifier and token scanning ────────────────────────────────────────

// scanIdentifier consumes one SQL identifier at the start of s and
// returns its unquoted text plus the remainder. Quoted forms
// ("x", `x`, [x]) unquote; a doubled closing quote is a literal.
func scanIdentifier(s string) (ident, rest string, ok bool) {
	s = strings.TrimLeft(s, " \t\n\r\f")
	if s == "" {
		return "", s, false
	}
	switch s[0] {
	case '"', '`', '[':
		open := s[0]
		closeCh := open
		if open == '[' {
			closeCh = ']'
		}
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			c := s[i]
			if c != closeCh {
				b.WriteByte(c)
				continue
			}
			if i+1 < len(s) && s[i+1] == closeCh {
				b.WriteByte(closeCh)
				i++
				continue
			}
			return b.String(), s[i+1:], true
		}
		return "", s, false // unterminated
	default:
		i := 0
		for i < len(s) && isIdentChar(s[i]) {
			i++
		}
		if i == 0 {
			return "", s, false
		}
		return s[:i], s[i:], true
	}
}

// parseQualifiedIdentifier parses `[schema .] name` and returns the
// FINAL component — the object's own name — plus the remainder.
func parseQualifiedIdentifier(s string) (name, rest string, ok bool) {
	name, rest, ok = scanIdentifier(s)
	if !ok {
		return "", s, false
	}
	for {
		r := strings.TrimLeft(rest, " \t\n\r\f")
		if !strings.HasPrefix(r, ".") {
			return name, rest, true
		}
		next, r2, ok2 := scanIdentifier(r[1:])
		if !ok2 {
			return name, rest, true
		}
		name, rest = next, r2
	}
}

// trimIdentifierToken returns the final component of a possibly
// qualified identifier token, or "" when it is not one.
func trimIdentifierToken(tok string) string {
	name, _, ok := parseQualifiedIdentifier(strings.TrimSpace(tok))
	if !ok {
		return ""
	}
	return name
}

// ── token-aware normalisation ────────────────────────────────────────────

// sqlTokenKind classifies one token in the comparison form.
type sqlTokenKind int

const (
	sqlTokenWord       sqlTokenKind = iota // keyword, identifier or number
	sqlTokenIdentifier                     // a QUOTED identifier, unquoted
	sqlTokenString                         // a single-quoted literal, verbatim
	sqlTokenOperator                       // operator or punctuation
)

// sqlToken is one token of the comparison form.
type sqlToken struct {
	kind sqlTokenKind
	text string
}

// sqlOperators are the multi-character operators SQLite recognises.
// Matching is longest-first so `->>` cannot be read as `-` `>` `>`.
var sqlOperators = []string{
	"->>", "||", "<=", ">=", "<>", "!=", "==", "<<", ">>", "->",
}

// tokenizeSQL splits SQL text into tokens while preserving string
// literals and unquoting quoted identifiers. Comments must already be
// stripped. Tokens are case-folded EXCEPT string literals, which are
// preserved byte for byte: only their characters carry meaning.
func tokenizeSQL(s string) []sqlToken {
	var out []sqlToken
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '\'':
			start := i
			i++
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			out = append(out, sqlToken{kind: sqlTokenString, text: s[start:i]})
		case c == '"' || c == '`' || c == '[':
			ident, rest, ok := scanIdentifier(s[i:])
			if !ok {
				// Unterminated quote: keep the remainder as one token
				// so the comparison cannot silently drop it.
				out = append(out, sqlToken{kind: sqlTokenIdentifier, text: strings.ToUpper(s[i:])})
				return out
			}
			out = append(out, sqlToken{kind: sqlTokenIdentifier, text: strings.ToUpper(ident)})
			i = len(s) - len(rest)
		default:
			if matched := matchOperator(s[i:]); matched != "" {
				out = append(out, sqlToken{kind: sqlTokenOperator, text: matched})
				i += len(matched)
				continue
			}
			if isIdentChar(c) {
				start := i
				for i < len(s) && isIdentChar(s[i]) {
					i++
				}
				out = append(out, sqlToken{kind: sqlTokenWord, text: strings.ToUpper(s[start:i])})
				continue
			}
			out = append(out, sqlToken{kind: sqlTokenOperator, text: string(c)})
			i++
		}
	}
	return out
}

// matchOperator returns the longest operator at the start of s.
func matchOperator(s string) string {
	for _, op := range sqlOperators {
		if strings.HasPrefix(s, op) {
			return op
		}
	}
	switch s[0] {
	case '=', '<', '>', '(', ')', ',', ';', '.', '+', '-', '*', '/', '%', '&', '|', '~':
		return string(s[0])
	}
	return ""
}

// normalizeTriggerBody renders the canonical comparison form: comments
// stripped, IF NOT EXISTS dropped, identifier quoting and schema
// qualification removed, tokens separated by a single space, keywords
// and unquoted identifiers case-folded, string literals preserved byte
// for byte. Whitespace differences — including those around
// multi-character operators such as `||` — carry no meaning (B1).
func normalizeTriggerBody(s string) string {
	s = stripSQLComments(s)
	// Drop IF NOT EXISTS (case-insensitive, word-boundaried).
	reIfNotExists := regexp.MustCompile(`(?i)\bIF\s+NOT\s+EXISTS\b`)
	s = reIfNotExists.ReplaceAllString(s, " ")
	tokens := tokenizeSQL(s)
	// Drop a `main.` schema qualifier wherever it appears. The
	// qualifier is an identifier token followed by a dot operator;
	// quoted and bare forms have already converged.
	filtered := make([]sqlToken, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		if (tokens[i].kind == sqlTokenWord || tokens[i].kind == sqlTokenIdentifier) &&
			tokens[i].text == "MAIN" &&
			i+1 < len(tokens) && tokens[i+1].kind == sqlTokenOperator && tokens[i+1].text == "." {
			i++ // skip the qualifier AND its dot
			continue
		}
		filtered = append(filtered, tokens[i])
	}
	// A trailing statement terminator is not part of the definition:
	// sqlite_master stores trigger text without it and the migration
	// text carries it.
	for len(filtered) > 0 && filtered[len(filtered)-1].kind == sqlTokenOperator &&
		filtered[len(filtered)-1].text == ";" {
		filtered = filtered[:len(filtered)-1]
	}
	parts := make([]string, len(filtered))
	for i, t := range filtered {
		parts[i] = t.text
	}
	return strings.Join(parts, " ")
}
