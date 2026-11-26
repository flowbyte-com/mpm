package internal

// Restore row parsing, resequencing and historical replay model
// (contract §4.6/B2–B3).
//
// Only managed tables (agent_tasks, agent_task_sessions,
// agent_task_supersessions, agent_task_events) plus projects and
// schema_migrations are parsed. Every other statement passes through in
// dump order. Resequencing puts non-managed project rows first, then
// managed rows in dependency order, so the lineage's project foreign
// key and the task/session/event references resolve at replay.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// restoreValueKind is the SQL type of one parsed literal.
//
// The kind is CARRIED through parsing rather than re-derived from the
// text at render time. A value's SQL type is a property of how the dump
// spelled it (quoted or bare), not of what the characters happen to look
// like: `'007'` is TEXT and must stay `007`, never become the integer 7.
type restoreValueKind int

const (
	restoreKindNull restoreValueKind = iota
	restoreKindText
	restoreKindNumber
	restoreKindBlob
)

// restoreValue is one parsed literal.
type restoreValue struct {
	kind restoreValueKind
	// text holds the DECODED text for kindText and the original SQL
	// token for kindNumber/kindBlob.
	text string
}

// isNull reports a NULL literal.
func (v restoreValue) isNull() bool { return v.kind == restoreKindNull }

// restoreRow is one parsed INSERT row.
type restoreRow struct {
	table string
	cols  []string
	vals  []restoreValue
}

// restoreModel is the parsed content of one dump.
type restoreModel struct {
	// createStmts are non-trigger CREATE statements in dump order.
	createStmts []string
	// tableCols maps lowercase table name to CREATE order columns.
	tableCols map[string][]string
	// managed rows by table (lowercase).
	managed map[string][]restoreRow
	// projects rows (resequenced first).
	projects []restoreRow
	// passthrough INSERT statements (non-managed, non-projects) in order.
	passthrough []string
	// sentinels from schema_migrations rows.
	sentinels map[string]bool
	// triggerStmts are raw CREATE TRIGGER statements (validated
	// separately; installed from canonical text, not executed).
	triggerStmts []string
	// other statements (BEGIN/COMMIT/DELETE sqlite_sequence/PRAGMA...)
	other []string
}

// restoreParsedTables are the tables whose rows are parsed for semantic
// inspection or dependency ordering (contract §4.6/B2). Every other
// table's INSERT statements pass through VERBATIM: the dump is the
// authority for them, and the canonical validator already vetted them.
//
// Parsing all INSERTs was the F1 defect: a genuine `sqlite3 .dump`
// carries `replace(...)`, `char(...)` and blob literals in ordinary
// application tables (memories, FTS shadow tables, artifacts), and a
// parser that runs before the target is known refuses legal dumps.
var restoreParsedTables = map[string]bool{
	"agent_tasks":              true,
	"agent_task_events":        true,
	"agent_task_sessions":      true,
	"agent_task_supersessions": true,
	"projects":                 true,
	"schema_migrations":        true,
}

// parseRestoreModel parses validated dump statements into the replay
// model. Column lists come from the dump's own CREATE TABLE text; a
// managed INSERT without a resolvable column order is refused rather
// than guessed.
func parseRestoreModel(statements []string) (*restoreModel, error) {
	m := &restoreModel{
		tableCols: map[string][]string{},
		managed:   map[string][]restoreRow{},
		sentinels: map[string]bool{},
	}
	for _, stmt := range statements {
		upper := strings.ToUpper(strings.TrimSpace(stmt))
		switch {
		case strings.HasPrefix(upper, "CREATE TABLE"):
			name, cols, err := parseCreateTableColumns(stmt)
			if err != nil {
				return nil, err
			}
			m.tableCols[strings.ToLower(name)] = cols
			m.createStmts = append(m.createStmts, stmt)
		case strings.HasPrefix(upper, "CREATE TRIGGER"):
			m.triggerStmts = append(m.triggerStmts, stmt)
		case strings.HasPrefix(upper, "CREATE "):
			m.createStmts = append(m.createStmts, stmt)
		case strings.HasPrefix(upper, "INSERT INTO"):
			// Inspect the TARGET before parsing values. Only the
			// tables this pipeline reasons about are parsed; anything
			// else is the dump's own business and replays verbatim.
			table, err := insertTargetTable(stmt)
			if err != nil {
				return nil, err
			}
			lower := strings.ToLower(table)
			if !restoreParsedTables[lower] {
				m.passthrough = append(m.passthrough, stmt)
				continue
			}
			row, err := parseInsertRow(stmt, m.tableCols)
			if err != nil {
				return nil, err
			}
			if managedRestoreTables[lower] {
				m.managed[lower] = append(m.managed[lower], *row)
				continue
			}
			if lower == "projects" {
				m.projects = append(m.projects, *row)
				continue
			}
			// schema_migrations: sentinel ids feed classification, and
			// the row itself replays verbatim.
			for _, v := range row.vals {
				if !v.isNull() {
					m.sentinels[v.text] = true
				}
			}
			m.passthrough = append(m.passthrough, stmt)
		default:
			m.other = append(m.other, stmt)
		}
	}
	// Late column resolution: a CREATE TABLE may follow its INSERTs in
	// dump order. Re-resolve is unnecessary — sqlite3 .dump emits
	// schema first — but an INSERT that arrived before its table
	// definition fails closed instead of guessing.
	return m, nil
}

// insertTargetTable reads the table token of an INSERT statement
// without parsing its values. This is the F1 boundary: the target
// decides whether the statement is managed rows or passthrough, and no
// value grammar is applied to a statement that will not be replayed
// through the model.
func insertTargetTable(stmt string) (string, error) {
	upper := strings.ToUpper(stmt)
	idx := strings.Index(upper, "INSERT INTO")
	if idx < 0 {
		return "", fmt.Errorf("restore: not an INSERT statement")
	}
	rest := strings.TrimSpace(stmt[idx+len("INSERT INTO"):])
	if rest == "" {
		return "", fmt.Errorf("restore: INSERT without a target")
	}
	tok := rest
	if i := strings.IndexAny(rest, " \t\n\r("); i >= 0 {
		tok = rest[:i]
	}
	table := trimIdentifierToken(tok)
	if table == "" {
		return "", fmt.Errorf("restore: INSERT without a target")
	}
	return table, nil
}

// parseCreateTableColumns extracts the table name and top-level column
// names from a CREATE TABLE statement.
func parseCreateTableColumns(stmt string) (string, []string, error) {
	upper := strings.ToUpper(stmt)
	idx := strings.Index(upper, "TABLE")
	if idx < 0 {
		return "", nil, fmt.Errorf("restore: not a CREATE TABLE statement")
	}
	rest := strings.TrimSpace(stmt[idx+len("TABLE"):])
	restUpper := strings.ToUpper(rest)
	if strings.HasPrefix(restUpper, "IF NOT EXISTS") {
		rest = strings.TrimSpace(rest[len("IF NOT EXISTS"):])
	}
	paren := strings.Index(rest, "(")
	if paren < 0 {
		return "", nil, fmt.Errorf("restore: CREATE TABLE without a column list")
	}
	name := strings.Trim(strings.TrimSpace(rest[:paren]), "\"'`[]")
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[idx+1:]
	}
	body := rest[paren:]
	// Find the matching close paren at depth 0.
	depth := 0
	end := -1
	inStr := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '\'' {
			if inStr && i+1 < len(body) && body[i+1] == '\'' {
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
	if end < 0 {
		return "", nil, fmt.Errorf("restore: unbalanced CREATE TABLE %q", name)
	}
	inner := stripSQLComments(body[1:end])
	var cols []string
	for _, part := range splitTopLevel(inner) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		upper := strings.ToUpper(part)
		// Skip table-level constraints.
		first := part
		if i := strings.IndexAny(part, " \t\n\r("); i >= 0 {
			first = part[:i]
		}
		fu := strings.ToUpper(strings.Trim(first, "\"'`[]"))
		switch fu {
		case "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "CONSTRAINT":
			continue
		}
		_ = upper
		col := strings.Trim(strings.TrimSpace(first), "\"'`[]")
		if idx := strings.LastIndex(col, "."); idx >= 0 {
			col = col[idx+1:]
		}
		if col != "" {
			cols = append(cols, col)
		}
	}
	if name == "" || len(cols) == 0 {
		return "", nil, fmt.Errorf("restore: CREATE TABLE %q has no columns", name)
	}
	return name, cols, nil
}

// splitTopLevel splits on commas at paren depth 0, honouring strings.
func splitTopLevel(s string) []string {
	var out []string
	depth := 0
	inStr := false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
			if inStr && i+1 < len(s) && s[i+1] == '\'' {
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
		} else if c == ',' && depth == 0 {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// parseInsertRow parses one INSERT statement, which may carry a column
// list and one or more VALUES tuples. Multi-row VALUES produce one
// restoreRow per tuple.
func parseInsertRow(stmt string, tableCols map[string][]string) (*restoreRow, error) {
	table, err := insertTargetTable(stmt)
	if err != nil {
		return nil, err
	}
	upper := strings.ToUpper(stmt)
	idx := strings.Index(upper, "INSERT INTO")
	rest := strings.TrimSpace(stmt[idx+len("INSERT INTO"):])
	// Table token ends at whitespace or '('.
	tok := rest
	if i := strings.IndexAny(rest, " \t\n\r("); i >= 0 {
		tok = rest[:i]
	}
	after := strings.TrimSpace(rest[len(tok):])
	var cols []string
	if strings.HasPrefix(after, "(") {
		depth := 0
		end := -1
		inStr := false
		for i := 0; i < len(after); i++ {
			c := after[i]
			if c == '\'' {
				if inStr && i+1 < len(after) && after[i+1] == '\'' {
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
		if end < 0 {
			return nil, fmt.Errorf("restore: unbalanced column list in INSERT INTO %q", table)
		}
		for _, part := range splitTopLevel(after[1:end]) {
			col := strings.Trim(strings.TrimSpace(part), "\"'`[]")
			if idx := strings.LastIndex(col, "."); idx >= 0 {
				col = col[idx+1:]
			}
			cols = append(cols, col)
		}
		after = strings.TrimSpace(after[end+1:])
	} else {
		known, ok := tableCols[strings.ToLower(table)]
		if !ok || len(known) == 0 {
			return nil, fmt.Errorf("restore: INSERT INTO %q without a column list and no known schema", table)
		}
		cols = known
	}
	upperAfter := strings.ToUpper(after)
	if !strings.HasPrefix(upperAfter, "VALUES") {
		// SELECT-form INSERTs are not replayable row data.
		return nil, fmt.Errorf("restore: only VALUES-form INSERTs are replayable")
	}
	after = strings.TrimSpace(after[len("VALUES"):])
	tuples, err := splitValueTuples(after)
	if err != nil {
		return nil, err
	}
	if len(tuples) != 1 {
		return nil, fmt.Errorf("restore: multi-row INSERTs are replayed one row at a time")
	}
	vals, err := parseValueTuple(tuples[0])
	if err != nil {
		return nil, err
	}
	if len(vals) != len(cols) {
		return nil, fmt.Errorf("restore: INSERT INTO %q has %d values for %d columns", table, len(vals), len(cols))
	}
	return &restoreRow{table: table, cols: cols, vals: vals}, nil
}

// splitValueTuples splits "(...), (...)" into tuple bodies.
func splitValueTuples(s string) ([]string, error) {
	var out []string
	depth := 0
	inStr := false
	start := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' {
			if inStr && i+1 < len(s) && s[i+1] == '\'' {
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
			if depth == 0 {
				start = i
			}
			depth++
		} else if c == ')' {
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, s[start+1:i])
				start = -1
			} else if depth < 0 {
				return nil, fmt.Errorf("restore: unbalanced VALUES list")
			}
		} else if c == ';' && depth == 0 {
			break
		}
	}
	if inStr || depth != 0 || len(out) == 0 {
		return nil, fmt.Errorf("restore: malformed VALUES list")
	}
	return out, nil
}

// sqlNumberRe matches a SQL numeric literal. Parsing a token as a Go
// float is NOT the same question: Go accepts "Inf" and "NaN", which are
// not SQL literals, and it ignores the leading zeros that make the
// token's SPELLING significant.
var sqlNumberRe = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$|^[+-]?0[xX][0-9a-fA-F]+$`)

// parseValueTuple parses one "(...)" body into typed literals. Every SQL
// value kind a genuine `sqlite3 .dump` emits is recognised:
//
//   - NULL;
//   - single-quoted text (with ” escapes);
//   - the bounded replace(<text>, '<escape>', char(<code>)) form the
//     shell uses for control characters, nested left-to-right;
//   - blob literals X'…';
//   - integer and real literals, TRUE/FALSE (stored as 1/0).
//
// Anything else fails closed. This is deliberately a closed grammar, not
// an expression evaluator: the validator already refused anything that
// is not a canonical table INSERT, and a dump expression this parser
// does not recognise is a dump shape the restore does not claim to
// support.
func parseValueTuple(body string) ([]restoreValue, error) {
	parts := splitTopLevel(body)
	out := make([]restoreValue, 0, len(parts))
	for _, part := range parts {
		p := strings.TrimSpace(part)
		if p == "" {
			return nil, fmt.Errorf("restore: empty value in tuple")
		}
		up := strings.ToUpper(p)
		if up == "NULL" {
			out = append(out, restoreValue{kind: restoreKindNull})
			continue
		}
		if strings.HasPrefix(p, "X'") || strings.HasPrefix(p, "x'") {
			if !strings.HasSuffix(p, "'") || len(p) < 3 {
				return nil, fmt.Errorf("restore: unterminated blob literal")
			}
			out = append(out, restoreValue{kind: restoreKindBlob, text: p})
			continue
		}
		if strings.HasPrefix(p, "'") || isReplaceLiteral(p) {
			v, err := parseDumpTextLiteral(p)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			continue
		}
		if up == "TRUE" {
			out = append(out, restoreValue{kind: restoreKindNumber, text: "1"})
			continue
		}
		if up == "FALSE" {
			out = append(out, restoreValue{kind: restoreKindNumber, text: "0"})
			continue
		}
		if sqlNumberRe.MatchString(p) {
			out = append(out, restoreValue{kind: restoreKindNumber, text: p})
			continue
		}
		return nil, fmt.Errorf("restore: unsupported literal %q", p)
	}
	return out, nil
}

// isReplaceLiteral reports the sqlite shell's escaped-text form.
func isReplaceLiteral(p string) bool {
	lower := strings.ToLower(strings.TrimSpace(p))
	return strings.HasPrefix(lower, "replace(") && strings.HasSuffix(lower, ")")
}

// parseDumpTextLiteral parses a recognised TEXT representation from a
// SQLite dump: a single-quoted literal, or the shell's
// replace(<text>, '<escape>', char(<code>)) wrapper for control
// characters (the shell escapes LF and CR, choosing an escape token that
// does not collide with the text and escaping the token itself when it
// would).
//
// The recursion is bounded by the nesting the shell emits (one wrapper
// per escaped character class) and the grammar is closed: the inner
// value must be another recognised text representation, the search token
// a plain literal, and the replacement exactly char(<integer>). No other
// expression shape is interpreted.
func parseDumpTextLiteral(p string) (restoreValue, error) {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "'") {
		decoded, rest, err := decodeQuotedLiteral(p)
		if err != nil {
			return restoreValue{}, err
		}
		if strings.TrimSpace(rest) != "" {
			return restoreValue{}, fmt.Errorf("restore: trailing text after string literal %q", p)
		}
		return restoreValue{kind: restoreKindText, text: decoded}, nil
	}
	if !isReplaceLiteral(p) {
		return restoreValue{}, fmt.Errorf("restore: unsupported text literal %q", p)
	}
	inner := strings.TrimSpace(p[len("replace(") : len(p)-1])
	args := splitTopLevel(inner)
	if len(args) != 3 {
		return restoreValue{}, fmt.Errorf("restore: replace() needs three arguments")
	}
	base, err := parseDumpTextLiteral(strings.TrimSpace(args[0]))
	if err != nil {
		return restoreValue{}, err
	}
	esc, rest, err := decodeQuotedLiteral(strings.TrimSpace(args[1]))
	if err != nil {
		return restoreValue{}, err
	}
	if strings.TrimSpace(rest) != "" {
		return restoreValue{}, fmt.Errorf("restore: unsupported replace() search token %q", args[1])
	}
	code, err := parseCharCall(strings.TrimSpace(args[2]))
	if err != nil {
		return restoreValue{}, err
	}
	if esc == "" {
		return restoreValue{}, fmt.Errorf("restore: replace() with an empty search token")
	}
	base.text = strings.ReplaceAll(base.text, esc, string(rune(code)))
	return base, nil
}

// decodeQuotedLiteral decodes a single-quoted SQL literal, returning the
// decoded text and the remainder after the closing quote.
func decodeQuotedLiteral(p string) (string, string, error) {
	if !strings.HasPrefix(p, "'") {
		return "", p, fmt.Errorf("restore: not a string literal")
	}
	var b strings.Builder
	for i := 1; i < len(p); i++ {
		c := p[i]
		if c != '\'' {
			b.WriteByte(c)
			continue
		}
		if i+1 < len(p) && p[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		return b.String(), p[i+1:], nil
	}
	return "", p, fmt.Errorf("restore: unterminated string literal")
}

// parseCharCall parses the shell's char(<integer>) replacement argument.
func parseCharCall(p string) (int, error) {
	if len(p) < len("char()") || !strings.EqualFold(p[:len("char(")], "char(") || p[len(p)-1] != ')' {
		return 0, fmt.Errorf("restore: replace() replacement must be char(<code>)")
	}
	n, err := strconv.Atoi(strings.TrimSpace(p[len("char(") : len(p)-1]))
	if err != nil || n < 0 || n > 0x10FFFF {
		return 0, fmt.Errorf("restore: invalid char() code in %q", p)
	}
	return n, nil
}

// rowMap renders a parsed row as column->value.
func (r restoreRow) rowMap() map[string]restoreValue {
	m := map[string]restoreValue{}
	for i, c := range r.cols {
		if i < len(r.vals) {
			m[strings.ToLower(c)] = r.vals[i]
		}
	}
	return m
}

// resequencedInserts returns replay-order INSERT statements: projects
// rows first (the lineage's project foreign key), then managed tables
// in dependency order, preserving within-table dump order.
func (m *restoreModel) resequencedInserts() ([]string, error) {
	// Ordering constraints (all inside one transaction):
	// - projects first (the lineage's project foreign key);
	// - a task needs its parent present;
	// - a lineage needs its superseded task and named attempt present
	//   (TR3/TR4 read them live);
	// - a reserved-key task needs its naming lineage present (TR9);
	// - a session needs its task present (foreign key).
	// Ordinary tasks land first; lineages, reserved tasks and reserved
	// sessions follow in dependency order to a fixed point. Anything
	// unplaceable names a missing dependency or closes a cycle (T92),
	// which the semantic check already refused pre-window — failing
	// closed here too rather than silently dropping a row.
	reserved := map[string]bool{}
	for _, r := range m.managed["agent_tasks"] {
		rm := r.rowMap()
		if id, ok := rm["id"]; ok {
			if key, ok := rm["idempotency_key"]; ok && !key.isNull() &&
				strings.HasPrefix(key.text, "supersede:") {
				reserved[id.text] = true
			}
		}
	}
	var out []string
	for _, r := range m.projects {
		out = append(out, renderInsert(r))
	}
	tasks := append([]restoreRow{}, m.managed["agent_tasks"]...)
	sessions := append([]restoreRow{}, m.managed["agent_task_sessions"]...)
	lineages := append([]restoreRow{}, m.managed["agent_task_supersessions"]...)
	insertedTasks := map[string]bool{}
	insertedSessions := map[string]bool{}
	insertedLineages := map[string]bool{}
	rowID := func(r restoreRow) string {
		id, _ := r.rowMap()["id"]
		return id.text
	}
	for len(tasks) > 0 || len(sessions) > 0 || len(lineages) > 0 {
		progress := false
		var keep []restoreRow
		for _, r := range tasks {
			rm := r.rowMap()
			id, _ := rm["id"]
			parent, _ := rm["parent_id"]
			parentOK := parent.isNull() || parent.text == "" || insertedTasks[parent.text]
			namedOK := true
			if reserved[id.text] {
				namedOK = false
				for _, l := range m.managed["agent_task_supersessions"] {
					lm := l.rowMap()
					lid, _ := lm["id"]
					if !insertedLineages[lid.text] {
						continue
					}
					rep, _ := lm["replacement_task_id"]
					rev, _ := lm["reviewer_task_id"]
					if rep.text == id.text || rev.text == id.text {
						namedOK = true
						break
					}
				}
			}
			if parentOK && namedOK {
				out = append(out, renderInsert(r))
				insertedTasks[id.text] = true
				progress = true
			} else {
				keep = append(keep, r)
			}
		}
		tasks = keep
		keep = nil
		for _, r := range sessions {
			rm := r.rowMap()
			taskID, _ := rm["task_id"]
			if insertedTasks[taskID.text] {
				out = append(out, renderInsert(r))
				id, _ := rm["id"]
				insertedSessions[id.text] = true
				progress = true
			} else {
				keep = append(keep, r)
			}
		}
		sessions = keep
		keep = nil
		for _, r := range lineages {
			rm := r.rowMap()
			sup, _ := rm["superseded_task_id"]
			att, _ := rm["attempt_id"]
			if insertedTasks[sup.text] && insertedSessions[att.text] {
				out = append(out, renderInsert(r))
				insertedLineages[rowID(r)] = true
				progress = true
			} else {
				keep = append(keep, r)
			}
		}
		lineages = keep
		if !progress {
			var stuck []string
			for _, r := range tasks {
				stuck = append(stuck, "task:"+rowID(r))
			}
			for _, r := range sessions {
				stuck = append(stuck, "session:"+rowID(r))
			}
			for _, r := range lineages {
				stuck = append(stuck, "lineage:"+rowID(r))
			}
			return nil, fmt.Errorf("restore: missing dependency or cycle involving %s", strings.Join(stuck, ", "))
		}
	}
	for _, r := range m.managed["agent_task_events"] {
		out = append(out, renderInsert(r))
	}
	out = append(out, m.passthrough...)
	return out, nil
}

// renderInsert emits the explicit column list and a VALUES tuple whose
// literals keep their PARSED TYPE. Text is always quoted; numbers and
// blobs replay their validated original token. Nothing infers a type
// from a value's characters, so `'007'`, `'1e5'` and `'-0'` survive a
// round trip unchanged (F6).
func renderInsert(r restoreRow) string {
	var b strings.Builder
	b.WriteString("INSERT INTO \"")
	b.WriteString(strings.ReplaceAll(r.table, "\"", "\"\""))
	b.WriteString("\" (")
	for i, c := range r.cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("\"")
		b.WriteString(strings.ReplaceAll(c, "\"", "\"\""))
		b.WriteString("\"")
	}
	b.WriteString(") VALUES (")
	for i, v := range r.vals {
		if i > 0 {
			b.WriteString(", ")
		}
		switch v.kind {
		case restoreKindNull:
			b.WriteString("NULL")
		case restoreKindNumber, restoreKindBlob:
			b.WriteString(v.text)
		default:
			b.WriteString("'")
			b.WriteString(strings.ReplaceAll(v.text, "'", "''"))
			b.WriteString("'")
		}
	}
	b.WriteString(")")
	return b.String()
}

// managedTableSet lists tables the historical validator governs.
func managedTableSet() []string {
	tables := []string{}
	for tbl := range managedRestoreTables {
		tables = append(tables, tbl)
	}
	sort.Strings(tables)
	return tables
}
