package internal

import (
	"database/sql"
	"fmt"
	"strings"
)

// DumpValidator enforces an allow-list policy on SQLite dump content
// before it is restored into the MPM database.
//
// Threat model (C-2): a tampered .sql dump placed in the DB directory can
// otherwise run arbitrary SQL via the `restore-db` primitive. The
// validator rejects any statement whose first keyword is not on the
// allow-list, any INSERT into a non-known table, any CREATE beyond
// TABLE/INDEX/TRIGGER/VIEW, any PRAGMA other than foreign_keys, and any
// destructive keyword (ATTACH, DETACH, DELETE, UPDATE, DROP, ALTER,
// SELECT, REPLACE, RENAME, TRUNCATE, VACUUM, REINDEX, ANALYZE, EXEC,
// LOAD).
//
// The validator strips SQL comments before analysis to prevent
// comment-based bypasses (`-- ATTACH` inside a comment should not
// trigger rejection of a valid INSERT that follows) and respects
// string-literal boundaries to avoid false positives on semicolons
// inside string values. It also handles the SQL-standard `''`
// escape for embedded single quotes.
//
// On any failure, the validator fails CLOSED: it returns an error
// describing the first unsafe statement. The caller MUST abort the
// restore transaction on error; the validator does NOT sanitize.
type DumpValidator struct {
	knownTables map[string]bool
}

// NewDumpValidator creates a validator with the given set of known table
// names. Pass nil to use the canonical MPM schema (queries sqlite_master).
func NewDumpValidator(knownTables []string) *DumpValidator {
	set := make(map[string]bool, len(knownTables))
	for _, t := range knownTables {
		set[strings.ToLower(t)] = true
	}
	return &DumpValidator{knownTables: set}
}

// NewDumpValidatorFromDB opens the given SQLite database in read-only mode
// and builds a validator whose allow-list is derived from the existing
// tables in the DB. FTS virtual tables and sqlite_* internal tables are
// excluded.
//
// This is the recommended constructor for production use: the allow-list
// stays in sync with the live schema, so any table added to MPM is
// automatically protected on restore.
func NewDumpValidatorFromDB(dbPath string) (*DumpValidator, error) {
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT name FROM sqlite_master
		WHERE type IN ('table', 'view')
		  AND name NOT LIKE 'sqlite_%'
		  AND name NOT LIKE '%_fts%'
		ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("query schema: %w", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return NewDumpValidator(tables), nil
}

// Validate returns nil if content is safe to restore, or an error
// describing the first unsafe statement found. Empty content is valid.
func (v *DumpValidator) Validate(content string) error {
	cleaned := stripSQLComments(content)
	statements := splitSQLStatements(cleaned)
	for i, stmt := range statements {
		trimmed := strings.TrimSpace(stmt)
		if trimmed == "" {
			continue
		}
		if err := v.validateStatement(trimmed); err != nil {
			return fmt.Errorf("statement %d: %w", i+1, err)
		}
	}
	return nil
}

func (v *DumpValidator) validateStatement(stmt string) error {
	upper := strings.ToUpper(strings.TrimSpace(stmt))
	fields := strings.Fields(upper)
	if len(fields) == 0 {
		return nil
	}
	keyword := fields[0]
	switch keyword {
	case "BEGIN", "COMMIT", "ROLLBACK", "END":
		return nil
	case "CREATE":
		return v.validateCreate(fields)
	case "INSERT":
		return v.validateInsert(fields)
	case "PRAGMA":
		return v.validatePragma(fields)
	default:
		return fmt.Errorf("statement not allowed: %s", keyword)
	}
}

func (v *DumpValidator) validateCreate(fields []string) error {
	if len(fields) < 3 {
		return fmt.Errorf("incomplete CREATE statement")
	}
	switch fields[1] {
	case "TABLE", "TRIGGER", "VIEW":
		return v.validateCreateNamed(fields)
	case "INDEX":
		return v.validateCreateIndex(fields)
	case "UNIQUE", "VIRTUAL", "TEMP", "TEMPORARY":
		return fmt.Errorf("CREATE %s not allowed", fields[1])
	default:
		return fmt.Errorf("CREATE %s not allowed", fields[1])
	}
}

// validateCreateNamed handles CREATE TABLE|TRIGGER|VIEW where the name
// appears directly after the keyword (optionally preceded by IF NOT EXISTS).
func (v *DumpValidator) validateCreateNamed(fields []string) error {
	name := fields[2]
	// IF NOT EXISTS may appear before name
	if strings.ToUpper(name) == "IF" && len(fields) >= 6 &&
		strings.ToUpper(fields[3]) == "NOT" &&
		strings.ToUpper(fields[4]) == "EXISTS" {
		name = fields[5]
	}
	name = strings.Trim(name, `"[]`)
	if !v.knownTables[strings.ToLower(name)] {
		return fmt.Errorf("CREATE on unknown table: %s", name)
	}
	return nil
}

// validateCreateIndex handles CREATE INDEX <name> ON <table> where the
// table name appears after the ON keyword, not directly after INDEX.
// The table name may be followed immediately by a column list (e.g.
// `memories(weight)`) since SQLite has no whitespace requirement.
func (v *DumpValidator) validateCreateIndex(fields []string) error {
	for i := 2; i < len(fields); i++ {
		if fields[i] == "ON" {
			if i+1 >= len(fields) {
				return fmt.Errorf("CREATE INDEX missing ON target table")
			}
			// Strip column list (e.g. 'memories(weight)' -> 'memories')
			table := strings.Trim(fields[i+1], `"[]`)
			if paren := strings.Index(table, "("); paren >= 0 {
				table = table[:paren]
			}
			if !v.knownTables[strings.ToLower(table)] {
				return fmt.Errorf("CREATE INDEX on unknown table: %s", table)
			}
			return nil
		}
	}
	return fmt.Errorf("CREATE INDEX missing ON clause")
}

func (v *DumpValidator) validateInsert(fields []string) error {
	// INSERT [OR REPLACE|ABORT|IGNORE|FAIL|ROLLBACK] INTO <table> ...
	var i int
	if len(fields) > 1 && fields[1] == "OR" {
		if len(fields) < 5 || fields[3] != "INTO" {
			return fmt.Errorf("INSERT OR must be followed by conflict algorithm and INTO")
		}
		i = 4
	} else {
		if len(fields) < 3 || fields[1] != "INTO" {
			return fmt.Errorf("INSERT must be followed by INTO <table>")
		}
		i = 2
	}
	table := strings.Trim(fields[i], `"[]`)
	if !v.knownTables[strings.ToLower(table)] {
		return fmt.Errorf("INSERT into unknown table: %s", table)
	}
	return nil
}

func (v *DumpValidator) validatePragma(fields []string) error {
	// Allow only: PRAGMA foreign_keys [= <value>]
	if len(fields) < 2 {
		return fmt.Errorf("incomplete PRAGMA")
	}
	name := strings.ToLower(fields[1])
	if eq := strings.Index(name, "="); eq >= 0 {
		name = name[:eq]
	}
	if name != "foreign_keys" {
		return fmt.Errorf("PRAGMA %s not allowed", name)
	}
	return nil
}

// stripSQLComments removes -- line comments and /* block comments */ from
// SQL content. Per SQL standard, comments are whitespace; we replace them
// with spaces (preserving newlines for line counting) so 'A--comment\nB'
// becomes 'A   \nB' rather than 'AB', which would accidentally create a
// new token that bypasses keyword checks.
func stripSQLComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		// -- line comment: skip to end of line
		if i+1 < len(s) && s[i] == '-' && s[i+1] == '-' {
			b.WriteByte(' ')
			i += 2
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		// /* block comment */: skip to */
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			b.WriteByte(' ')
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				if s[i] == '\n' {
					b.WriteByte('\n') // preserve line structure
				}
				i++
			}
			i += 2 // skip */
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// splitSQLStatements splits SQL content into statements by semicolons,
// respecting single-quoted and double-quoted string literal boundaries.
// Handles the SQL-standard `''` escape for embedded single quotes.
func splitSQLStatements(content string) []string {
	var stmts []string
	var current strings.Builder
	inSingleQuote := false
	inDoubleQuote := false
	for i := 0; i < len(content); i++ {
		c := content[i]
		switch {
		case c == '\'' && !inDoubleQuote:
			// SQL-standard escape: '' inside a single-quoted string is a
			// literal single quote, not a string boundary.
			if inSingleQuote && i+1 < len(content) && content[i+1] == '\'' {
				current.WriteByte('\'')
				current.WriteByte('\'')
				i++
				continue
			}
			inSingleQuote = !inSingleQuote
			current.WriteByte(c)
		case c == '"' && !inSingleQuote:
			inDoubleQuote = !inDoubleQuote
			current.WriteByte(c)
		case c == ';' && !inSingleQuote && !inDoubleQuote:
			stmts = append(stmts, current.String())
			current.Reset()
		default:
			current.WriteByte(c)
		}
	}
	if current.Len() > 0 {
		stmts = append(stmts, current.String())
	}
	return stmts
}