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
	// The name is the next token, possibly quoted or schema-qualified.
	nameTok := rest
	if i := strings.IndexAny(rest, " \t\n\r("); i >= 0 {
		nameTok = rest[:i]
	}
	name = strings.Trim(nameTok, "\"'`[]")
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" {
		return "", "", fmt.Errorf("restore: trigger without a name")
	}
	return name, s, nil
}

// normalizeTriggerBody renders the canonical comparison form: comments
// stripped, IF NOT EXISTS dropped, identifier quoting and schema
// qualification removed, whitespace collapsed, SQL keywords and
// identifiers case-folded, string literals preserved byte for byte.
func normalizeTriggerBody(s string) string {
	s = stripSQLComments(s)
	// Drop IF NOT EXISTS (case-insensitive, word-boundaried).
	reIfNotExists := regexp.MustCompile(`(?i)\bIF\s+NOT\s+EXISTS\b`)
	s = reIfNotExists.ReplaceAllString(s, " ")
	// Strip identifier quoting: "x", [x], `x` -> x. String literals
	// use single quotes and are preserved.
	var b strings.Builder
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
			if inStr && i+1 < len(s) && s[i+1] == '\'' {
				b.WriteString("''")
				i++
				continue
			}
			inStr = !inStr
			b.WriteByte(c)
			continue
		}
		if inStr {
			b.WriteByte(c)
			continue
		}
		if c == '"' || c == '[' || c == ']' || c == '`' {
			continue
		}
		b.WriteByte(c)
	}
	s = b.String()
	// Drop schema qualification on known objects: main.<name> -> <name>.
	// Conservative: only the main schema qualifier.
	reSchema := regexp.MustCompile(`(?i)\bmain\s*\.\s*`)
	s = reSchema.ReplaceAllString(s, "")
	// Collapse whitespace and case-fold outside string literals.
	// Spaces adjacent to operator punctuation (= < > ( ) , ;) carry no
	// meaning and are dropped, so `a='x'` and `a = 'x'` compare equal.
	// String literals are preserved byte for byte.
	var out strings.Builder
	inStr = false
	needSpace := false
	isPunct := func(c byte) bool {
		switch c {
		case '=', '<', '>', '(', ')', ',', ';':
			return true
		}
		return false
	}
	lastWritten := func() byte {
		s := out.String()
		if len(s) == 0 {
			return 0
		}
		return s[len(s)-1]
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
			if inStr && i+1 < len(s) && s[i+1] == '\'' {
				out.WriteString("''")
				i++
				continue
			}
			inStr = !inStr
			out.WriteByte(c)
			needSpace = false
			continue
		}
		if inStr {
			out.WriteByte(c)
			continue
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' {
			needSpace = true
			continue
		}
		if needSpace {
			if out.Len() > 0 && !isPunct(lastWritten()) && !isPunct(c) {
				out.WriteByte(' ')
			}
			needSpace = false
		}
		if c >= 'a' && c <= 'z' {
			c = c - 'a' + 'A'
		}
		out.WriteByte(c)
	}
	// Trailing statement terminator: sqlite_master stores trigger text
	// without it, migration text carries it.
	return strings.TrimSuffix(strings.TrimSpace(out.String()), ";")
}
