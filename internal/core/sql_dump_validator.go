package internal

import (
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

// CanonicalMPMSchema is the authoritative list of tables MPM creates at
// install time (BaseTables, ReferenceTables, shared-schema tables). New
// tables added to the schema require updating this list AND
// internal/core/db.go (a static guard enforces this). This is the
// source-of-truth allow-list for `restore-db`'s C-2 validation —
// every CREATE TABLE / CREATE INDEX / INSERT statement in a restore
// dump must reference a table in this list OR RuntimeCanonicalSchema.
//
// To regenerate after a schema change:
//   1. Add the new table to internal/core/db.go (CREATE TABLE IF NOT EXISTS ...)
//   2. Add it to CanonicalMPMSchema below
//   3. Run `go test -run TestCanonicalSchemaSync ./internal/core/`
//      to verify the static guard passes
var CanonicalMPMSchema = []string{
	"admission_log",
	"audit_cluster_proposals",
	"confidence_history",
	"ephemeral_scratchpad",
	"evidence",
	"external_db_cursors",
	"lessons",
	"memories",
	"memory_revisions",
	"raw_memories",
	"reference_chunks",
	"reference_docs",
	"reference_interactions",
	"scheduled_wakes",
	"session_handoffs",
	"sessions",
	"shared.bcast_agents",
	"shared.bcast_event_wakes",
	"shared.bcast_sessions",
	"shared.contradiction_log",
	"system_audit_log",
	"system_config",
	"topic_memberships",
	"topics",
	"vector_assignments",
	"vector_clusters",
}

// RuntimeCanonicalSchema holds tables created by runtime migrations or
// extension code (not declared in internal/core/db.go's CREATE TABLE
// statements). The static guard does NOT check this list against source
// — it's a hand-maintained extension to CanonicalMPMSchema. Add a table
// here when a migration creates it and you want restore-db to accept
// dumps that reference it.
var RuntimeCanonicalSchema = []string{
	"artifacts",       // created by extension migration
	"legacy_weight",   // created by extension migration
	"lessons_base",    // created by migrateLessonsToView
	"synthesis_dlq",   // created by synthesis isolation runtime
}

// NewCanonicalDumpValidator creates a validator using the canonical MPM
// schema (union of source-declared and runtime-created tables). This is
// the recommended constructor for production restore-db use — it allows
// CREATE TABLE / INDEX / TRIGGER / VIEW and INSERT for all legitimate
// MPM tables regardless of the live DB state, which means a fresh-DB
// restore works (the dump's CREATE TABLE statements are validated
// against the canonical allow-list, not an empty live schema).
func NewCanonicalDumpValidator() *DumpValidator {
	combined := make([]string, 0, len(CanonicalMPMSchema)+len(RuntimeCanonicalSchema))
	combined = append(combined, CanonicalMPMSchema...)
	combined = append(combined, RuntimeCanonicalSchema...)
	return NewDumpValidator(combined)
}

// NewDumpValidator creates a validator with the given set of known table
// names. Used by tests and by the live-schema constructor; production
// callers should prefer NewCanonicalDumpValidator.
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
// Use this for restore-into-existing-DB cases where you want the
// allow-list restricted to what's already in the target. For
// restore-into-fresh-DB (the common case for new installs or recovery
// from total data loss), use NewCanonicalDumpValidator instead — the
// live DB has no tables, so the live-schema allow-list would be empty
// and reject the dump's CREATE TABLE statements.
func NewDumpValidatorFromDB(dbPath string) (*DumpValidator, error) {
	// Kept for callers that specifically want the live-schema variant.
	// Implementation moved to internal/live_schema_validator.go to keep
	// this file focused on the canonical-schema path.
	return newLiveSchemaDumpValidator(dbPath)
}

// Validate returns nil if content is safe to restore, or an error
// describing the first unsafe statement found. Empty content is valid.
func (v *DumpValidator) Validate(content string) error {
	_, err := v.Prepare(content)
	return err
}

// Prepare validates the dump and returns the parsed statements if safe.
// Use this to drive per-statement execution after the safety check —
// the returned statements are exactly what the validator approved, so
// executing them in a transaction preserves the allow-list guarantee.
func (v *DumpValidator) Prepare(content string) ([]string, error) {
	cleaned := stripSQLComments(content)
	statements := splitSQLStatements(cleaned)
	out := make([]string, 0, len(statements))
	for i, stmt := range statements {
		trimmed := strings.TrimSpace(stmt)
		if trimmed == "" {
			continue
		}
		if err := v.validateStatement(trimmed); err != nil {
			return nil, fmt.Errorf("statement %d: %w", i+1, err)
		}
		out = append(out, trimmed)
	}
	return out, nil
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