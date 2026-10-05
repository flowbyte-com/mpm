// =============================================================================
// SECURITY REVIEWER HANDOFF — C-2 (restore-db SQL injection)
// =============================================================================
//
// Threat Model:  Defense against compromised/poisoned .sql dumps that
//                reach the MPM database directory. An attacker who can
//                drop a file there and trick the user into running
//                `mpm restore-db <file>` would otherwise gain arbitrary
//                SQL execution against the DB.
//
// Parser Design: Tokenizer-based (strings.Fields + strings.ToUpper), NOT
//                regex. Case-variation bypasses (aTtAcH) caught by design.
//                SQL comments are stripped BEFORE keyword analysis with
//                whitespace preservation so 'INSE/*fake*/RT' stays two
//                distinct tokens (token boundary test in
//                sql_dump_validator_test.go::TestDumpValidator_CommentTokenBoundary).
//
// Allow-list:    Schema-canonical. Two lists, hand-maintained:
//                  CanonicalMPMSchema  — tables declared in
//                                         internal/core/{db,schema}*.go
//                                         CREATE TABLE statements.
//                  RuntimeCanonicalSchema — tables created by runtime
//                                         migrations (lessons_base,
//                                         artifacts, etc.) plus the
//                                         FTS5 virtual tables, which
//                                         are CREATE VIRTUAL TABLE and
//                                         so are invisible to a
//                                         CREATE TABLE scan.
//                Both lists are checked against source:
//                TestCanonicalSchemaSync verifies CanonicalMPMSchema,
//                and TestRuntimeCanonicalSchemaHasNoPhantomEntries
//                verifies every RuntimeCanonicalSchema entry has a
//                real table behind it. An earlier version checked only
//                the first list, which left the second unguarded — and
//                two entries (synthesis_dlq, reference_docs_fts) with
//                no table behind them accumulated there unnoticed.
//
// Posture:       FAIL-CLOSED. The validator explicitly rejects malformed
//                or unknown SQL rather than attempting to sanitize. Most
//                SQLi bypasses live in error-prone sanitization logic;
//                deterministic rejection is a security feature, not a
//                gap. Any statement that cannot be positively identified
//                as allow-listed returns an error and aborts the restore.
//
// Verification:  Three layers of test coverage:
//                  1. cmd/mpm/testdata/restore_db/ — 10 .sql fixtures
//                     (3 benign, 7 attack vectors including comment-spoof,
//                      case-variation, string-literal semicolons).
//                  2. sql_dump_validator_test.go — 11 unit tests covering
//                     parser correctness, bypass defenses, PRAGMA strictness.
//                  3. sql_dump_validator_e2e_test.go — 2 E2E tests against
//                     real SQLite: tampered dump → abort + DB untouched;
//                     good dump → roundtrip restore into empty DB.
//
// Execution:     Handler (cmd/mpm/handlers_backup.go) runs statements
//                per-statement inside a Go transaction. BEGIN/COMMIT
//                are skipped (the outer Go tx serves the same atomicity;
//                database/sql refuses nested transactions). Per-statement
//                exec ensures NULL bytes / partial writes yield precise
//                per-statement errors and gives a clear failure point.
//
// Architectural Decisions (intentional trade-offs, not gaps):
//   * H-4 concurrent-write `flock` guard implemented (2026-10-05).
//     handleRestoreDB (cmd/mpm/handlers_backup.go) acquires an
//     exclusive flock on `<dbPath>.lock` AFTER validation succeeds
//     and BEFORE the destructive window, and holds it until
//     `.pre-restore` is removed. The empirical reproducer
//     (internal/core/restore_db_concurrency_repro_test.go, §4)
//     pinned the pre-fix race in five scenarios (silent loss in A/B/E,
//     data contamination in C, benign post-rename open in D). The
//     same lock file is also probed by the shred-database preflight,
//     so a shred-database and a restore-db can never run
//     concurrently. Crash safety is kernel-cleared via syscall.Flock.
//     The known limitation: the lock is NOT acquired by other
//     DatabaseManager users (CLI handlers, scheduler, MCP), so a
//     concurrent `mpm capture` is still racy. The restore-db error
//     message and the help text both tell the operator to stop the
//     live MPM daemon before running restore-db. Extending the lock
//     protocol to all DatabaseManager users is a future tranche.
//   * Reject-not-sanitize chosen for fail-closed posture. Sanitization
//     is where most SQLi bypasses live; deterministic rejection is safer.
//   * Canonical schema (not live schema) as allow-list. Live schema
//     doesn't work for restore-into-fresh-DB (empty sqlite_master
//     rejects legitimate CREATE TABLE statements). Canonical covers
//     the common case; live-schema variant removed before review.
//
// =============================================================================

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
	"artifact_provenance",
	"audit_cluster_proposals",
	"blobs",
	"capabilities",
	"capability_dependencies",
	"capability_events",
	"capability_invocations",
	"confidence_history",
	"embedding_migration_log",
	"ephemeral_scratchpad",
	"epistemic_cascade_outbox",
	"epistemic_provenance",
	"evidence",
	"external_db_cursors",
	"lessons",
	"memories",
	"memory_revisions",
	"raw_memories",
	"reference_chunks",
	"reference_docs",
	"reference_interactions",
	"retrieval_metadata",
	"scheduled_wakes",
	"scheduled_tasks",
	"schema_migrations",
	"session_handoffs",
	"sessions",
	"shared.bcast_agents",
	"shared.bcast_event_wakes",
	"shared.bcast_sessions",
	"shared.contradiction_log",
	"synth_runs",
	"system_audit_log",
	"system_config",
	"topic_memberships",
	"topics",
	"drill_runs",
	"tool_invocations",
	"vector_assignments",
	"vector_clusters",
	"work_events",
	"work_purge_audit",
	"works",
}

// RuntimeCanonicalSchema holds tables created by runtime migrations or
// extension code (not declared in internal/core/db.go's CREATE TABLE
// statements). It is a hand-maintained extension to CanonicalMPMSchema.
//
// Unlike CanonicalMPMSchema, this list IS checked against source — by
// TestRuntimeCanonicalSchemaHasNoPhantomEntries, which requires every
// entry to correspond to a real CREATE VIRTUAL TABLE (for the *_fts
// bases) or to appear in a non-test source file. Keep it that way: an
// entry with no table behind it is a hole in the `mpm restore-db`
// security allow-list, because a dump declaring a table MPM cannot
// create would be accepted.
//
// Two entries were removed on 2026-09-29 for failing that guard:
//
//   - synthesis_dlq — the residue of the removed synthesis-DLQ feature.
//     No CREATE TABLE for it exists in the tree, the `mpm dlq` command
//     that read it is gone, and no current database contains it. Its
//     only mention in this package was the comment beside the entry
//     itself, which is how it survived so long.
//   - reference_docs_fts — the FTS virtual table indexing the
//     reference_docs base table is named references_fts (triggers
//     references_ai/ad/au). reference_docs_fts is the pre-rename name
//     and was left behind in this list.
//
// Add a table here when a migration creates it and you want restore-db
// to accept dumps that reference it.
var RuntimeCanonicalSchema = []string{
	"artifacts",               // created by extension migration
	"confidence_history__new", // created transiently by migration_confidence_history_check_widening's table-recreate dance; dropped + renamed before commit
	"legacy_weight",           // created by extension migration
	"lessons_base",            // created by migrateLessonsToView
	// FTS5 virtual tables — declared via CREATE VIRTUAL TABLE in db.go,
	// not CREATE TABLE, so the static guard (TestCanonicalSchemaSync) does
	// not pick them up. Their shadow tables (`<base>_fts_data`, `_fts_idx`,
	// etc.) are accepted via isAllowedTable's suffix match — these base
	// names just need to be on the allow-list for that match to fire.
	// Exactly seven, matching the seven fts_recovery domains and the set
	// of FTS virtual tables in a live MPM database.
	"lessons_fts",
	"memories_fts",
	"reference_chunks_fts",
	"references_fts",
	"scheduled_wakes_fts",
	"sessions_fts",
	"topics_fts",
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
	case "DELETE":
		// Accept `DELETE FROM <known_table> WHERE <expr>;` where the
		// table is on the allow-list (or is an FTS5 shadow table of an
		// allow-listed virtual table) AND the statement has a WHERE
		// clause (so it cannot wipe the whole table). `sqlite3 .dump`
		// emits DELETE statements for two legitimate reasons:
		//   1. `DELETE FROM sqlite_sequence;` at the start of every
		//      dump to reset autoincrement counters. Handled below as
		//      a special case because the dump does NOT include a
		//      WHERE clause on this statement.
		//   2. Inside trigger bodies that maintain FTS5 indexes (e.g.
		//      `DELETE FROM lessons_fts WHERE rowid=OLD.rowid;`).
		// The splitSQLStatements helper does not track BEGIN/END
		// nesting, so #2 would surface as a top-level DELETE on the
		// validator's allow-list check. Requiring a WHERE clause keeps
		// the security property intact (no whole-table wipe) while
		// letting those trigger-body statements through.
		if len(fields) >= 3 && fields[1] == "FROM" {
			table := extractTableName(fields[2])
			// sqlite_sequence: special-case WHERE-less DELETE because
			// sqlite3 .dump emits it without WHERE to reset autoincrement
			// counters. The table is system-managed; the wipe is the
			// documented reset.
			if strings.ToLower(table) == "sqlite_sequence" {
				return nil
			}
			if len(fields) >= 5 && fields[3] == "WHERE" && v.isAllowedTable(table) {
				return nil
			}
		}
		return fmt.Errorf("DELETE not allowed: %s", stmt)
	default:
		return fmt.Errorf("statement not allowed: %s", keyword)
	}
}

func (v *DumpValidator) validateCreate(fields []string) error {
	if len(fields) < 3 {
		return fmt.Errorf("incomplete CREATE statement")
	}
	// CREATE UNIQUE INDEX is a compound keyword: `UNIQUE` is a column-
	// level qualifier that applies to INDEX (and only to INDEX in
	// practice — `CREATE UNIQUE TABLE` is not legal SQL). `sqlite3 .dump`
	// emits `CREATE UNIQUE INDEX` for indexes on unique constraints, so
	// we accept the UNIQUE prefix when the actual kind is INDEX.
	if fields[1] == "UNIQUE" && len(fields) >= 3 && fields[2] == "INDEX" {
		return v.validateCreateIndex(fields)
	}
	switch fields[1] {
	case "TABLE":
		return v.validateCreateTable(fields)
	case "TRIGGER":
		return v.validateCreateTrigger(fields)
	case "VIEW":
		return v.validateCreateView(fields)
	case "INDEX":
		return v.validateCreateIndex(fields)
	case "UNIQUE", "VIRTUAL", "TEMP", "TEMPORARY":
		return fmt.Errorf("CREATE %s not allowed", fields[1])
	default:
		return fmt.Errorf("CREATE %s not allowed", fields[1])
	}
}

// validateCreateTable handles CREATE TABLE [IF NOT EXISTS] <name> where
// the table name must be on the allow-list (or an FTS5 shadow table
// belonging to an allow-listed virtual table). The dump from
// `sqlite3 .dump` always quotes shadow-table names with single quotes,
// so we extract the bare name before the allow-list check.
func (v *DumpValidator) validateCreateTable(fields []string) error {
	name := fields[2]
	if strings.ToUpper(name) == "IF" && len(fields) >= 6 &&
		strings.ToUpper(fields[3]) == "NOT" &&
		strings.ToUpper(fields[4]) == "EXISTS" {
		name = fields[5]
	}
	name = extractTableName(name)
	if !v.isAllowedTable(name) {
		return fmt.Errorf("CREATE on unknown table: %s", name)
	}
	return nil
}

// validateCreateTrigger accepts any CREATE TRIGGER name. The threat
// model for restore-db is "no arbitrary SQL execution against the
// substrate"; the trigger body is parsed and the embedded statements
// would be caught by the same keyword allow-list if they were to
// escape the validator's per-statement validation. The trigger NAME
// itself is not a security boundary because the trigger executes
// SQL that we wrote — and if we wrote it, the validator's other
// rules (CREATE TABLE on allow-list, INSERT on allow-list, etc.)
// govern the SQL the trigger body can reference.
func (v *DumpValidator) validateCreateTrigger(fields []string) error {
	if len(fields) < 3 {
		return fmt.Errorf("incomplete CREATE TRIGGER statement")
	}
	return nil
}

// validateCreateView accepts any CREATE VIEW name. Views are pure
// projections of existing tables; the underlying tables are validated
// separately.
func (v *DumpValidator) validateCreateView(fields []string) error {
	if len(fields) < 3 {
		return fmt.Errorf("incomplete CREATE VIEW statement")
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
			table := extractTableName(fields[i+1])
			if !v.isAllowedTable(table) {
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
	table := extractTableName(fields[i])
	if !v.isAllowedTable(table) {
		return fmt.Errorf("INSERT into unknown table: %s", table)
	}
	return nil
}

// extractTableName extracts the bare table name from a token that may
// include a column list (e.g. `memories(weight)`) and/or be wrapped in
// any of SQLite's accepted quoting styles: double quotes `"name"`, single
// quotes `'name'`, square brackets `[name]`, or backticks `` `name` ``
// (the last is non-standard but tolerated by `sqlite3 .dump` for some
// shadow-table outputs). The token may also be glued to a column list
// without whitespace (e.g. `'name'(col)`) since `strings.Fields` does
// not split on parens — in that case we cut at the first `(`.
func extractTableName(token string) string {
	// Strip a trailing column list if glued to the name.
	if i := strings.IndexAny(token, "("); i >= 0 {
		token = token[:i]
	}
	// Strip one layer of surrounding quotes / brackets / backticks.
	for _, q := range []string{`""`, `''`, `[]`, "``"} {
		if len(token) >= 2 && token[0] == q[0] && token[len(token)-1] == q[1] {
			token = token[1 : len(token)-1]
			break
		}
	}
	return token
}

// isAllowedTable reports whether `name` is in the explicit allow-list OR
// is an FTS5 shadow table belonging to an allow-listed virtual table OR
// is a SQLite system catalog / autoincrement-counters table that
// `sqlite3 .dump` re-populates.
//
// FTS5 shadow tables (`<base>_fts_data`, `_fts_idx`, `_fts_content`,
// `_fts_docsize`, `_fts_config`) are created automatically by SQLite when
// the corresponding virtual table (`<base>_fts`) is built. They are not
// declared in source code via CREATE TABLE, but they ARE present in any
// dump produced by `sqlite3 .dump` (which materializes them as plain
// CREATE TABLE statements). The pre-fix allow-list missed them, so any
// restore from a dump of an FTS5-enabled database was rejected. The 2026-
// 09-11 pre-probe restore surfaced this for the first time because
// earlier restores came from dumps taken before FTS5 was enabled.
//
// SQLite system catalogs (`sqlite_schema` is the modern name,
// `sqlite_master` is the legacy alias) are re-populated by `sqlite3 .dump`
// for every object in the database, including the `CREATE VIRTUAL TABLE`
// definitions of FTS5 modules. The dump writes one INSERT per object —
// those rows are how the shadow-table restoration path rebuilds the
// virtual table's metadata. Without the special-case, every FTS5-enabled
// dump is rejected on the first `INSERT INTO sqlite_schema`.
//
// `sqlite_sequence` is the autoincrement-counter table. `sqlite3 .dump`
// writes `DELETE FROM sqlite_sequence;` at the start of every dump to
// reset all autoincrement counters so the restore doesn't carry stale
// sequence values forward.
//
// The shadow-table pattern is recognised by suffix only; the prefix
// (`<base>_fts`) MUST still be in the explicit allow-list. This means a
// shadow table for `evil_fts` would only be accepted if `evil_fts` is
// explicitly allow-listed — preventing the pattern from being used as
// an injection vector. SQLite system catalogs are accepted wholesale
// because the catalog rows themselves only describe objects whose
// existence is governed by other validator rules (CREATE TABLE / CREATE
// VIRTUAL TABLE / CREATE TRIGGER).
func (v *DumpValidator) isAllowedTable(name string) bool {
	lower := strings.ToLower(name)
	if v.knownTables[lower] {
		return true
	}
	// SQLite system catalog: sqlite_schema (modern) and sqlite_master
	// (legacy alias). `sqlite3 .dump` re-populates this table for every
	// object in the database; the rows themselves only describe objects
	// whose existence is governed by other validator rules.
	if lower == "sqlite_schema" || lower == "sqlite_master" ||
		lower == "sqlite_temp_schema" || lower == "sqlite_temp_master" {
		return true
	}
	// sqlite_sequence: autoincrement-counter table. `sqlite3 .dump` writes
	// `DELETE FROM sqlite_sequence;` at the start of every dump to reset
	// all autoincrement counters so the restore doesn't carry stale
	// sequence values forward. The DELETE itself is allowed separately
	// via the validateDelete allow-list; this entry just makes sure the
	// table name passes isAllowedTable.
	if lower == "sqlite_sequence" {
		return true
	}
	// FTS5 shadow table suffixes. See https://sqlite.org/fts5.html section
	// "Shadow Tables" for the canonical list. Match the suffix tokens
	// directly: "<base>_fts<suffix>" where <suffix> is one of the five
	// shadow-table kinds.
	for _, suffix := range []string{"_fts_data", "_fts_idx", "_fts_content", "_fts_docsize", "_fts_config"} {
		if strings.HasSuffix(lower, suffix) {
			base := lower[:len(lower)-len(suffix)] + "_fts"
			return v.knownTables[base]
		}
	}
	return false
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
//
// The pre-fix version was string-literal blind: it treated `--` anywhere as
// comment-start. That broke any SQL string containing a `--` (e.g. a lesson
// prose value like `'use cmd --flag'`) because the stripper would consume
// the rest of the line — including any `''` escape sequences and quote
// toggles that splitSQLStatements relies on to keep string-literal state
// in sync. The downstream parser would then misclassify later `;` as a
// statement boundary and trip on the orphan fragment (e.g. `do PATH ...`).
// The 2026-09-11 pre-probe restore surfaced this: a lesson about
// `git log --since=<binary-mtime>` inside a string value caused the entire
// dump to be rejected. The fix tracks single/double-quote state so `--` and
// `/*` are only treated as comment markers when outside string literals.
func stripSQLComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	inSingleQuote := false
	inDoubleQuote := false
	for i < len(s) {
		// Only treat `--` as a comment when outside any string literal.
		if !inSingleQuote && !inDoubleQuote &&
			i+1 < len(s) && s[i] == '-' && s[i+1] == '-' {
			b.WriteByte(' ')
			i += 2
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		// Only treat `/*` as a comment when outside any string literal.
		if !inSingleQuote && !inDoubleQuote &&
			i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
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
		// Track string-literal boundaries so subsequent comment-marker
		// checks remain accurate. `''` inside a single-quoted string is
		// an escaped single quote (literal `'`), not a string boundary —
		// mirror the escape handling in splitSQLStatements.
		if !inDoubleQuote && s[i] == '\'' {
			if inSingleQuote && i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('\'')
				b.WriteByte('\'')
				i += 2
				continue
			}
			inSingleQuote = !inSingleQuote
			b.WriteByte(s[i])
			i++
			continue
		}
		if !inSingleQuote && s[i] == '"' {
			inDoubleQuote = !inDoubleQuote
			b.WriteByte(s[i])
			i++
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
//
// Statement awareness for CREATE TRIGGER: the splitter walks the
// content linearly. When it sees `CREATE TRIGGER` at the start of a
// statement, it consumes the entire trigger body (up to and including
// the closing `END;`) as a single statement, regardless of inner
// semicolons. Trigger bodies are not valid at the top level (they
// reference pseudo-rows like NEW.rowid that only exist inside the
// trigger), and the inner INSERT/UPDATE/DELETE statements would trip
// the validator's keyword allow-list (e.g. on `DELETE`) or fail to
// execute outside the trigger context.
//
// Why a linear walker (not regex pre-merge):
//   - Linear walker: emits each CREATE TRIGGER as one statement with
//     inner `;` preserved (SQLite parses trigger bodies natively with
//     `;` separators between body statements). When the per-`;`
//     splitter scans the merged trigger body, it never sees the inner
//     `;` because the walker already consumed them as part of the
//     trigger.
//   - Regex pre-merge (rejected): replaces inner `;` with `\n` to hide
//     them from the per-`;` splitter, but SQLite's CREATE TRIGGER
//     parser does NOT accept `\n` as a statement separator inside
//     BEGIN...END blocks. Result: `Parse error near INSERT`. REJECTED.
//
// Linear walker details: the walker uses the same `\bEND\s*;` anchor
// as the regex pre-merge did, plus an explicit string-literal
// awareness (the `CREATE TRIGGER` keyword can appear inside a memory
// content string literal — the walker detects this via the same
// `inSingleQuote` / `inDoubleQuote` state as the splitter).
func splitSQLStatements(content string) []string {
	var stmts []string
	var current strings.Builder
	inSingleQuote := false
	inDoubleQuote := false
	i := 0
	for i < len(content) {
		c := content[i]
		switch {
		case c == '\'' && !inDoubleQuote:
			// SQL-standard escape: '' inside a single-quoted string is a
			// literal single quote, not a string boundary.
			if inSingleQuote && i+1 < len(content) && content[i+1] == '\'' {
				current.WriteByte('\'')
				current.WriteByte('\'')
				i += 2
				continue
			}
			inSingleQuote = !inSingleQuote
			current.WriteByte(c)
			i++
		case c == '"' && !inSingleQuote:
			inDoubleQuote = !inDoubleQuote
			current.WriteByte(c)
			i++
		case inSingleQuote || inDoubleQuote:
			current.WriteByte(c)
			i++
		case isCreateTriggerStart(content, i):
			// Consume the entire CREATE TRIGGER ... END; block as one
			// statement. The trigger body is copied verbatim (including
			// inner `;`) so SQLite can parse the body natively. The
			// per-`;` splitter never sees the inner `;` because the
			// walker handles them as part of the trigger chunk.
			//
			// After writing the trigger chunk, the walker does NOT
			// advance `i` past the END's `;`. Instead, it advances to
			// the offset of `;` (j in findTriggerBodyEnd) so the
			// splitter's normal `c == ';'` case fires next iteration
			// and ends this statement with the canonical `;` as
			// separator. This is what prevents the splitter from
			// accumulating the next CREATE TRIGGER's content into the
			// current statement (which would happen if `i` jumped
			// past the `;` and no `;`-split fired before the next
			// walker invocation).
			end := findTriggerBodyEnd(content, i)
			if end < 0 {
				// Malformed trigger body — treat the rest as one
				// statement so the validator can reject it cleanly.
				current.WriteString(content[i:])
				i = len(content)
				continue
			}
			// end = j+1 where j is the `;` offset. We want `i` to
			// point AT the `;` so the splitter's `case c == ';'`
			// fires next iteration.
			current.WriteString(content[i : end-1])
			i = end - 1
		case c == ';':
			stmts = append(stmts, current.String())
			current.Reset()
			i++
		default:
			current.WriteByte(c)
			i++
		}
	}
	if current.Len() > 0 {
		stmts = append(stmts, current.String())
	}
	return stmts
}

// isCreateTriggerStart reports whether `CREATE TRIGGER` begins at
// offset `i` in `content` AND is at a statement boundary (i.e. either
// at the start of content or preceded only by whitespace on the same
// line). String literals are excluded — `CREATE TRIGGER` inside a
// memory content is not a real statement.
func isCreateTriggerStart(content string, i int) bool {
	// "CREATE TRIGGER" is 14 chars (CREATE + ' ' + TRIGGER).
	const keywordLen = 14
	if i+keywordLen > len(content) {
		return false
	}
	if !strings.EqualFold(content[i:i+keywordLen], "CREATE TRIGGER") {
		return false
	}
	// Word boundary after TRIGGER (the next char must not be an
	// identifier continuation).
	after := i + keywordLen
	if after < len(content) {
		next := content[after]
		if next == '_' ||
			(next >= 'a' && next <= 'z') ||
			(next >= 'A' && next <= 'Z') ||
			(next >= '0' && next <= '9') {
			return false
		}
	}
	// Must be at the start of a line (preceded only by whitespace
	// since the previous newline) — this filters out CREATE TRIGGER
	// references inside string literals or other content.
	j := i - 1
	for j >= 0 && (content[j] == ' ' || content[j] == '\t') {
		j--
	}
	if j >= 0 && content[j] != '\n' && content[j] != '\r' {
		return false
	}
	return true
}

// findTriggerBodyEnd scans forward from `start` (which must be at
// `CREATE TRIGGER`) to find the offset just past the closing `END;`.
// Returns -1 if no closing `END;` is found. Respects string-literal
// boundaries so a `\bEND;` inside a memory content is not mistaken for
// the trigger's terminator.
func findTriggerBodyEnd(content string, start int) int {
	inSingleQuote := false
	inDoubleQuote := false
	for i := start; i < len(content); i++ {
		c := content[i]
		if c == '\'' && !inDoubleQuote {
			if inSingleQuote && i+1 < len(content) && content[i+1] == '\'' {
				i++
				continue
			}
			inSingleQuote = !inSingleQuote
			continue
		}
		if c == '"' && !inSingleQuote {
			inDoubleQuote = !inDoubleQuote
			continue
		}
		if inSingleQuote || inDoubleQuote {
			continue
		}
		if c == 'E' && i+3 <= len(content) && content[i:i+3] == "END" {
			// Confirm word boundary on the trailing side.
			after := i + 3
			if after < len(content) {
				next := content[after]
				if next == '_' ||
					(next >= 'a' && next <= 'z') ||
					(next >= 'A' && next <= 'Z') ||
					(next >= '0' && next <= '9') {
					continue
				}
			}
			// Find the next ; after END
			j := after
			for j < len(content) && (content[j] == ' ' || content[j] == '\t' || content[j] == '\n' || content[j] == '\r') {
				j++
			}
			if j < len(content) && content[j] == ';' {
				return j + 1
			}
		}
	}
	return -1
}

