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
	schemaStmts        []string          // CREATE TABLE/INDEX/VIEW + non-managed triggers, dump order
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
	// come from canonical text, never from the dump). Non-managed
	// triggers replay verbatim. Transaction-control and sqlite-catalog
	// statements are skipped exactly as the pre-contract replay did
	// (nested transactions conflict with the outer Go tx; the catalog
	// rows are redundant and the driver refuses them).
	for _, stmt := range model.createStmts {
		if skipReplayStatement(stmt) {
			continue
		}
		upper := strings.ToUpper(strings.TrimSpace(stmt))
		if strings.HasPrefix(upper, "CREATE TRIGGER") {
			name, _, err := parseTriggerStatement(stmt)
			if err != nil {
				return nil, err
			}
			if _, ok := inventory[name]; ok {
				continue
			}
		}
		plan.schemaStmts = append(plan.schemaStmts, stmt)
	}
	for _, stmt := range model.triggerStmts {
		if skipReplayStatement(stmt) {
			continue
		}
		name, _, err := parseTriggerStatement(stmt)
		if err != nil {
			return nil, err
		}
		if _, ok := inventory[name]; ok {
			continue
		}
		plan.schemaStmts = append(plan.schemaStmts, stmt)
	}
	for _, stmt := range model.other {
		if skipReplayStatement(stmt) {
			continue
		}
		upper := strings.ToUpper(strings.TrimSpace(stmt))
		if strings.HasPrefix(upper, "CREATE TRIGGER") {
			name, _, err := parseTriggerStatement(stmt)
			if err != nil {
				return nil, err
			}
			if _, ok := inventory[name]; ok {
				continue
			}
		}
		plan.schemaStmts = append(plan.schemaStmts, stmt)
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
func tableColumns(tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("restore: read columns of %s: %w", table, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
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

// verifyFinalManagedSchema compares the live managed schema against the
// full canonical inventory: every table with its columns, every index,
// every trigger body. Drift fails closed with
// final_canonical_schema_mismatch.
func verifyFinalManagedSchema(tx *sql.Tx, inventory map[string]string) error {
	rows, err := tx.Query(`SELECT type, name, tbl_name, COALESCE(sql,'') FROM sqlite_master
		WHERE (type='table' AND name IN ('agent_tasks','agent_task_events','agent_task_sessions','agent_task_supersessions'))
		   OR (type='index' AND tbl_name IN ('agent_tasks','agent_task_events','agent_task_sessions','agent_task_supersessions'))
		   OR (type='trigger' AND name LIKE 'trg\_agent\_task%' ESCAPE '\')`)
	if err != nil {
		return fmt.Errorf("restore: read the final schema: %w", err)
	}
	defer rows.Close()
	liveTables := map[string]bool{}
	liveIndexes := map[string]bool{}
	liveTriggers := map[string]string{}
	liveColumns := map[string][]string{}
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
			liveTriggers[name] = normalizeTriggerBody(sqlText)
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
	for name, wantBody := range inventory {
		got, ok := liveTriggers[name]
		if !ok {
			return fmt.Errorf("restore: final_canonical_schema_mismatch: trigger %q missing", name)
		}
		if got != wantBody {
			return fmt.Errorf("restore: final_canonical_schema_mismatch: trigger %q drifted", name)
		}
	}
	var extra []string
	for name := range liveTriggers {
		if _, ok := inventory[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		return fmt.Errorf("restore: final_canonical_schema_mismatch: unexpected triggers %s", strings.Join(extra, ","))
	}
	var fkViolations int
	fkRows, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("restore: foreign_key_check: %w", err)
	}
	defer fkRows.Close()
	for fkRows.Next() {
		fkViolations++
		var table, rowid, ftable, fkid string
		_ = fkRows.Scan(&table, &rowid, &ftable, &fkid)
		if fkViolations == 1 {
			_ = table
		}
	}
	if err := fkRows.Err(); err != nil {
		return fmt.Errorf("restore: read foreign_key_check: %w", err)
	}
	if fkViolations > 0 {
		return fmt.Errorf("restore: %d foreign key violations after replay", fkViolations)
	}
	return nil
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
