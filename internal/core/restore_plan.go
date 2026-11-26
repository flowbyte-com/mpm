package internal

// Restore replay planner (contract §4.6 pipeline order).
//
// Fixed order: validator → classification → canonical managed-trigger
// verification → parse → resequence → semantic validation → destructive
// window → replay → canonical managed triggers → additive migrations →
// final canonical schema + foreign-key verification → commit.
//
// Managed triggers are never executed from dump text: required ones are
// installed from the trusted canonical constants, non-deferred triggers
// before row replay and the deferred set after. Additive migrations run
// idempotently in the same transaction. The final check compares the
// live managed schema against the full canonical inventory and requires
// an empty PRAGMA foreign_key_check.

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// restoreDeferredTriggers is the seven-trigger deferred set: the four
// existing creation-time guards plus TR10, TR19 and TR20 (both the
// UPDATE and the INSERT guard implement the one TR20 rule).
var restoreDeferredTriggers = map[string]bool{
	"trg_agent_tasks_initial_state":                   true,
	"trg_agent_tasks_unclaimed_at_creation":           true,
	"trg_agent_tasks_records_are_born_empty":          true,
	"trg_agent_task_sessions_epoch_is_current":        true,
	"trg_agent_tasks_superseded_shape":                true,
	"trg_agent_task_sessions_admission_guard":         true,
	"trg_agent_task_sessions_completion_guard":        true,
	"trg_agent_task_sessions_completion_insert_guard": true,
}

// restorePlan is the validated replay program.
type restorePlan struct {
	class              restoreClass
	inventory          map[string]string // canonical name -> normalized body
	schemaStmts        []string          // CREATE TABLE/INDEX/VIEW, dump order
	userTriggerStmts   []string          // non-managed triggers, replayed AFTER the rows
	rowStmts           []string          // resequenced INSERTs
	immediateTriggers  []string          // canonical CREATE TRIGGER text (non-deferred managed)
	deferredTriggers   []string          // canonical CREATE TRIGGER text (deferred managed)
	requiredTriggerSet []string
}

// PlanRestore validates the dump and builds the replay program. It
// performs no writes; every failure leaves the live database untouched.
func PlanRestore(statements []string) (*restorePlan, error) {
	class, inv, err := classifyRestoreDump(statements)
	if err != nil {
		return nil, err
	}
	inventory, err := canonicalManagedTriggers()
	if err != nil {
		return nil, err
	}
	install, err := verifyManagedTriggers(class, inv, inventory)
	if err != nil {
		return nil, err
	}
	model, err := parseRestoreModel(statements)
	if err != nil {
		return nil, err
	}
	if err := validateRestoreHistory(model, class); err != nil {
		return nil, err
	}
	plan := &restorePlan{class: class, inventory: inventory}
	// Schema replay: everything except managed TRIGGER CREATEs (those
	// come from canonical text, never from the dump).
	//
	// NON-MANAGED triggers are held back until AFTER the rows, which is
	// where `sqlite3 .dump` puts them. Replaying them before the data
	// makes them fire during the data copy — an FTS5 maintenance
	// trigger would write to a virtual table the dump does not (and
	// cannot legally) recreate, and a genuine backup would fail to
	// restore (F1). The deferral is also the faithful order: a dump's
	// data precedes its triggers.
	//
	// Transaction-control and sqlite-catalog statements are skipped
	// exactly as the pre-contract replay did (nested transactions
	// conflict with the outer Go tx; the catalog rows are redundant and
	// the driver refuses them).
	// addSchema classifies one replayable statement. It FAILS CLOSED:
	// a trigger whose name or ON target cannot be resolved is refused
	// here rather than appended to the user-trigger list. Appending was
	// the F13 fail-open path: the statement would have been executed
	// after the rows, on whatever table its body named (F13/F11).
	addSchema := func(stmt string) error {
		if skipReplayStatement(stmt) {
			return nil
		}
		upper := strings.ToUpper(strings.TrimSpace(stmt))
		if strings.HasPrefix(upper, "CREATE TRIGGER") || strings.HasPrefix(upper, "CREATE TEMP TRIGGER") {
			name, _, err := parseTriggerStatement(stmt)
			if err != nil {
				return fmt.Errorf("restore: unclassifiable trigger statement: %w", err)
			}
			if _, ok := inventory[name]; ok {
				// Canonical managed triggers are installed from trusted
				// text, never from the dump.
				return nil
			}
			target, ok := parseTriggerTarget(stmt)
			if !ok {
				return fmt.Errorf(
					"restore: unclassifiable trigger %q: its ON target cannot be resolved", name)
			}
			if managedRestoreTables[target] {
				return fmt.Errorf(
					"restore: unexpected_trigger: %s is defined on the managed table %s", name, target)
			}
			plan.userTriggerStmts = append(plan.userTriggerStmts, stmt)
			return nil
		}
		plan.schemaStmts = append(plan.schemaStmts, stmt)
		return nil
	}
	for _, stmt := range model.createStmts {
		if err := addSchema(stmt); err != nil {
			return nil, err
		}
	}
	for _, stmt := range model.triggerStmts {
		if err := addSchema(stmt); err != nil {
			return nil, err
		}
	}
	for _, stmt := range model.other {
		if err := addSchema(stmt); err != nil {
			return nil, err
		}
	}
	resequenced, err := model.resequencedInserts()
	if err != nil {
		return nil, err
	}
	for _, stmt := range resequenced {
		if skipReplayStatement(stmt) {
			continue
		}
		plan.rowStmts = append(plan.rowStmts, stmt)
	}
	required := canonicalTriggerNamesForClass(class, inventory)
	plan.requiredTriggerSet = required
	for _, name := range required {
		text := install[name]
		if restoreDeferredTriggers[name] {
			plan.deferredTriggers = append(plan.deferredTriggers, text)
		} else {
			plan.immediateTriggers = append(plan.immediateTriggers, text)
		}
	}
	return plan, nil
}

// canonicalSchemaInventory returns the expected managed tables with
// their columns in order, plus the expected managed index names, parsed
// from the same DDL the migrations install.
func canonicalSchemaInventory() (map[string][]string, []string) {
	tables := map[string][]string{}
	var indexes []string
	seenIndex := map[string]bool{}
	for _, src := range canonicalTriggerSources() {
		_ = src
	}
	for _, ddl := range []string{agentAssignmentDDL, agentTaskSessionDDL, agentTaskSupersessionTableDDL} {
		upper := strings.ToUpper(ddl)
		pos := 0
		for {
			idx := strings.Index(upper[pos:], "CREATE TABLE")
			if idx < 0 {
				break
			}
			start := pos + idx
			// Find the matching close paren at depth 0.
			depth := 0
			end := -1
			inStr := false
			for i := start; i < len(ddl); i++ {
				c := ddl[i]
				if c == '\'' {
					if inStr && i+1 < len(ddl) && ddl[i+1] == '\'' {
						i++
						continue
					}
					inStr = !inStr
					continue
				}
				if inStr {
					continue
				}
				if c == '(' {
					depth++
				} else if c == ')' {
					depth--
					if depth == 0 {
						end = i
						break
					}
				}
			}
			if end > start {
				name, cols, err := parseCreateTableColumns(ddl[start : end+1])
				if err == nil && name != "" {
					tables[strings.ToLower(name)] = lowerAll(cols)
				}
			}
			pos = start + len("CREATE TABLE")
			_ = end
		}
		// Indexes.
		for _, stmt := range splitIndexStatements(ddl) {
			name := indexStatementName(stmt)
			if name != "" && !seenIndex[name] {
				seenIndex[name] = true
				indexes = append(indexes, name)
			}
		}
	}
	sort.Strings(indexes)
	return tables, indexes
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

// splitIndexStatements extracts CREATE [UNIQUE] INDEX statements.
func splitIndexStatements(ddl string) []string {
	var out []string
	upper := strings.ToUpper(ddl)
	i := 0
	for i < len(ddl) {
		ci, ui := -1, ""
		if idx := strings.Index(upper[i:], "CREATE UNIQUE INDEX"); idx >= 0 {
			ci, ui = i+idx, "CREATE UNIQUE INDEX"
		}
		if idx := strings.Index(upper[i:], "CREATE INDEX"); idx >= 0 && (ci < 0 || i+idx < ci) {
			ci, ui = i+idx, "CREATE INDEX"
		}
		if ci < 0 {
			break
		}
		semi := strings.Index(ddl[ci:], ";")
		if semi < 0 {
			break
		}
		out = append(out, ddl[ci:ci+semi+1])
		i = ci + len(ui)
	}
	return out
}

// indexStatementName extracts the index name from a CREATE INDEX statement.
func indexStatementName(stmt string) string {
	upper := strings.ToUpper(strings.TrimSpace(stmt))
	rest := upper
	if idx := strings.Index(rest, "INDEX"); idx >= 0 {
		rest = strings.TrimSpace(stmt[idx+len("INDEX"):])
	}
	if strings.HasPrefix(strings.ToUpper(rest), "IF NOT EXISTS") {
		rest = strings.TrimSpace(rest[len("IF NOT EXISTS"):])
	}
	tok := rest
	if i := strings.IndexAny(rest, " \t\n\r("); i >= 0 {
		tok = rest[:i]
	}
	tok = strings.Trim(tok, "\"'`[]")
	if idx := strings.LastIndex(tok, "."); idx >= 0 {
		tok = tok[idx+1:]
	}
	return strings.ToLower(tok)
}

// tableColumns lists a live table's columns in order.
//
// The table is bound as a PARAMETER through SQLite's table-valued
// pragma function, so this path builds no SQL text: the repository's
// zero-tolerance `sql-built` gate stays at zero and no table name is
// ever interpolated into a statement (F4).
func tableColumns(tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.Query(
		`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, fmt.Errorf("restore: read columns of %s: %w", table, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("restore: scan columns of %s: %w", table, err)
		}
		cols = append(cols, strings.ToLower(name))
	}
	return cols, rows.Err()
}

// skipReplayStatement reports the statements the replay never
// executes: transaction control (it would conflict with the outer Go
// transaction) and sqlite catalog INSERTs (redundant; the driver
// refuses them). Matches the pre-contract replay's skips exactly.
func skipReplayStatement(stmt string) bool {
	upper := strings.ToUpper(strings.TrimSpace(stmt))
	fields := strings.Fields(upper)
	if len(fields) == 0 {
		return true
	}
	switch fields[0] {
	case "BEGIN", "COMMIT", "ROLLBACK", "END":
		return true
	}
	if fields[0] == "INSERT" && len(fields) >= 3 && fields[1] == "INTO" {
		tbl := fields[2]
		if i := strings.IndexAny(tbl, "("); i >= 0 {
			tbl = tbl[:i]
		}
		switch strings.ToLower(tbl) {
		case "sqlite_schema", "sqlite_master", "sqlite_temp_schema", "sqlite_temp_master":
			return true
		}
	}
	return false
}

// ensureSchemaMigrationsTable creates the sentinel table if the dump
// did not carry it (very old dumps). Migrations probe it unconditionally.
func ensureSchemaMigrationsTable(tx *sql.Tx) error {
	var n int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&n); err != nil {
		return fmt.Errorf("restore: read sqlite_master: %w", err)
	}
	if n > 0 {
		return nil
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		id TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("restore: create schema_migrations: %w", err)
	}
	return nil
}

// runAdditiveMigrations brings pre-feature dumps to the current schema
// inside the restore transaction. Idempotent via sentinels.
func runAdditiveMigrations(tx *sql.Tx) error {
	if err := MigrateAgentAssignments(tx); err != nil {
		return fmt.Errorf("restore: migrate assignments: %w", err)
	}
	if err := MigrateAgentTaskSessions(tx); err != nil {
		return fmt.Errorf("restore: migrate sessions: %w", err)
	}
	if err := MigrateAgentTaskSupersessions(tx); err != nil {
		return fmt.Errorf("restore: migrate supersessions: %w", err)
	}
	return nil
}

// managedRestoreTableList is the four tables whose schema the final
// verification owns, derived from managedRestoreTables so the two
// definitions cannot drift.
func managedRestoreTableList() []string {
	tables := make([]string, 0, len(managedRestoreTables))
	for tbl := range managedRestoreTables {
		tables = append(tables, tbl)
	}
	sort.Strings(tables)
	return tables
}

// verifyFinalManagedSchema compares the live managed schema against the
// full canonical inventory: every table with its columns, every index,
// every canonical trigger with its trusted body AND its table binding,
// and NO other trigger on a managed table.
//
// Triggers are enumerated through sqlite_master's authoritative
// tbl_name, not through a name prefix. A dump-supplied trigger with an
// arbitrary name is therefore visible here even when an earlier
// classification defect let it through: this check is the independent
// backstop (F13/N28/N33).
func verifyFinalManagedSchema(tx *sql.Tx, inventory map[string]string) error {
	managedTables := managedRestoreTableList()
	inList := strings.Join(quotedList(managedTables), ",")
	rows, err := tx.Query(`SELECT type, name, tbl_name, COALESCE(sql,'') FROM sqlite_master
		WHERE (type='table' AND name IN (` + inList + `))
		   OR (type='index' AND tbl_name IN (` + inList + `))
		   OR type='trigger'`)
	if err != nil {
		return fmt.Errorf("restore: read the final schema: %w", err)
	}
	defer rows.Close()
	liveTables := map[string]bool{}
	liveIndexes := map[string]bool{}
	liveColumns := map[string][]string{}
	// ALL triggers are read: a canonical NAME bound to the wrong table
	// must be visible too, not only triggers bound to a managed table.
	liveTriggerBodies := map[string]string{}
	liveTriggerTables := map[string]string{}
	for rows.Next() {
		var typ, name, tbl, sqlText string
		if err := rows.Scan(&typ, &name, &tbl, &sqlText); err != nil {
			return fmt.Errorf("restore: scan the final schema: %w", err)
		}
		switch typ {
		case "table":
			liveTables[name] = true
			cols, err := tableColumns(tx, name)
			if err != nil {
				return err
			}
			liveColumns[name] = cols
		case "index":
			if !strings.HasPrefix(name, "sqlite_") {
				liveIndexes[name] = true
			}
		case "trigger":
			liveTriggerBodies[name] = normalizeTriggerBody(sqlText)
			liveTriggerTables[name] = strings.ToLower(tbl)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("restore: iterate the final schema: %w", err)
	}
	// Expected tables, columns and indexes come from the same canonical
	// DDL the migrations install.
	wantTables, wantIndexes := canonicalSchemaInventory()
	for tbl, wantCols := range wantTables {
		gotCols, ok := liveColumns[tbl]
		if !ok {
			return fmt.Errorf("restore: final_canonical_schema_mismatch: table %q missing", tbl)
		}
		if strings.Join(gotCols, ",") != strings.Join(wantCols, ",") {
			return fmt.Errorf("restore: final_canonical_schema_mismatch: table %q columns %v, want %v",
				tbl, gotCols, wantCols)
		}
	}
	for _, idx := range wantIndexes {
		if !liveIndexes[idx] {
			return fmt.Errorf("restore: final_canonical_schema_mismatch: index %q missing", idx)
		}
	}
	// Every canonical trigger must exist, carry its trusted body, and be
	// bound to the table its canonical definition names.
	for name, wantBody := range inventory {
		got, ok := liveTriggerBodies[name]
		if !ok {
			return fmt.Errorf("restore: final_canonical_schema_mismatch: trigger %q missing", name)
		}
		if got != wantBody {
			return fmt.Errorf("restore: final_canonical_schema_mismatch: trigger %q drifted", name)
		}
		wantTable, err := canonicalTriggerTable(name)
		if err != nil {
			return err
		}
		if err := checkTriggerBinding(name, liveTriggerTables[name], wantTable); err != nil {
			return err
		}
	}
	// NO other trigger may be defined on a managed table. This is the
	// check that catches a trigger whose name is not canonical, which a
	// name-prefix query could never see.
	managed := map[string]bool{}
	for _, tbl := range managedTables {
		managed[tbl] = true
	}
	var extra []string
	for name, tbl := range liveTriggerTables {
		if _, canonical := inventory[name]; canonical {
			continue
		}
		if managed[tbl] {
			extra = append(extra, name+"@"+tbl)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		return fmt.Errorf("restore: final_canonical_schema_mismatch: unexpected triggers on managed tables: %s",
			strings.Join(extra, ","))
	}
	var fkViolations int
	fkRows, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("restore: foreign_key_check: %w", err)
	}
	defer fkRows.Close()
	for fkRows.Next() {
		fkViolations++
	}
	if err := fkRows.Err(); err != nil {
		return fmt.Errorf("restore: read foreign_key_check: %w", err)
	}
	if fkViolations > 0 {
		return fmt.Errorf("restore: %d foreign key violations after replay", fkViolations)
	}
	return nil
}

// checkTriggerBinding reports a canonical trigger whose live table
// binding differs from the table its canonical definition names.
//
// The body comparison already includes the `ON <table>` clause, so a
// rebind normally reports as drift first; this guard is the explicit,
// independent statement of the same invariant (the review requires the
// binding itself to be checked, not inferred).
func checkTriggerBinding(name, got, want string) error {
	if got == want {
		return nil
	}
	return fmt.Errorf(
		"restore: final_canonical_schema_mismatch: trigger %q is defined on %q, want %q",
		name, got, want)
}

// quotedList renders names as a SQL list of literals. The names are the
// package's own constants, never input.
func quotedList(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "'" + strings.ReplaceAll(n, "'", "''") + "'"
	}
	return out
}

// canonicalTriggerTable returns the managed table a canonical trigger is
// defined ON, from its trusted statement header.
func canonicalTriggerTable(name string) (string, error) {
	stmt, err := canonicalTriggerStatement(name)
	if err != nil {
		return "", err
	}
	target, ok := parseTriggerTarget(stmt)
	if !ok {
		return "", fmt.Errorf("restore: canonical trigger %q has no resolvable ON target", name)
	}
	return target, nil
}

// ExecuteSchemaStmt executes one schema statement inside the restore
// transaction.
func execRestoreStmt(tx *sql.Tx, stmt string) error {
	if _, err := tx.Exec(stmt); err != nil {
		return err
	}
	return nil
}

// ExecuteSchema replays the schema statements (tables, indexes, views,
// non-managed triggers) in dump order, then ensures the sentinel table
// exists: dumps predate or omit its DDL, but row replay (sentinel
// INSERTs) and the additive migrations probe it unconditionally.
func (p *restorePlan) ExecuteSchema(tx *sql.Tx) error {
	for i, stmt := range p.schemaStmts {
		if err := execRestoreStmt(tx, stmt); err != nil {
			return fmt.Errorf("restore schema statement %d: %w", i+1, err)
		}
	}
	if err := ensureSchemaMigrationsTable(tx); err != nil {
		return err
	}
	return nil
}

// ExecuteImmediateTriggers installs the non-deferred canonical managed
// triggers from trusted text.
func (p *restorePlan) ExecuteImmediateTriggers(tx *sql.Tx) error {
	for _, stmt := range p.immediateTriggers {
		if err := execRestoreStmt(tx, stmt); err != nil {
			return fmt.Errorf("restore immediate trigger: %w", err)
		}
	}
	return nil
}

// ExecuteRows replays the resequenced INSERTs: non-managed projects
// rows first, then managed tables in dependency order.
func (p *restorePlan) ExecuteRows(tx *sql.Tx) error {
	for i, stmt := range p.rowStmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("restore row statement %d: %w", i+1, err)
		}
	}
	return nil
}

// ExecuteUserTriggers installs the dump's non-managed triggers AFTER
// the rows, which is the order `sqlite3 .dump` emits them in and the
// order that keeps a maintenance trigger from firing on a table the
// dump cannot recreate.
func (p *restorePlan) ExecuteUserTriggers(tx *sql.Tx) error {
	for i, stmt := range p.userTriggerStmts {
		if err := execRestoreStmt(tx, stmt); err != nil {
			return fmt.Errorf("restore user trigger %d: %w", i+1, err)
		}
	}
	return nil
}

// ExecuteDeferredTriggers installs the deferred canonical managed
// triggers from trusted text, after the rows they govern have landed.
func (p *restorePlan) ExecuteDeferredTriggers(tx *sql.Tx) error {
	for _, stmt := range p.deferredTriggers {
		if err := execRestoreStmt(tx, stmt); err != nil {
			return fmt.Errorf("restore deferred trigger: %w", err)
		}
	}
	return nil
}

// RunAdditiveMigrations brings pre-feature dumps to the current schema
// inside the restore transaction. Idempotent via sentinels.
func (p *restorePlan) RunAdditiveMigrations(tx *sql.Tx) error {
	if err := ensureSchemaMigrationsTable(tx); err != nil {
		return err
	}
	return runAdditiveMigrations(tx)
}

// VerifyFinal runs the final canonical schema and foreign-key
// verification before commit.
func (p *restorePlan) VerifyFinal(tx *sql.Tx) error {
	return verifyFinalManagedSchema(tx, p.inventory)
}

// Class returns the detected backup class (for reporting).
func (p *restorePlan) Class() string {
	switch p.class {
	case restoreClassAware:
		return "supersession-aware"
	case restoreClassPR3:
		return "pre-feature (sessions)"
	case restoreClassPR1:
		return "pre-feature (assignments)"
	default:
		return "ordinary"
	}
}
