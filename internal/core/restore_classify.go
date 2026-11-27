package internal

// Restore backup classification and managed-trigger verification
// (contract §4.6/B1–B2).
//
// Classification uses table+sentinel schema markers, never the presence
// or absence of lineage rows. A post-feature database with zero
// supersessions is still supersession-aware.

import (
	"fmt"
	"sort"
	"strings"
)

// restoreClass is the detected backup class.
type restoreClass int

const (
	// restoreClassOrdinary has no assignment tables: normal restore,
	// migrations add assignment/session/supersession schema.
	restoreClassOrdinary restoreClass = iota
	// restoreClassPR1 is a recognised pre-feature (PR1-only) dump:
	// agent_tasks + agent_assignments_v1, no session/supersession
	// markers. Legacy replay + migrations add session and
	// supersession schema.
	restoreClassPR1
	// restoreClassPR3 is a recognised pre-feature (post-PR3) dump:
	// agent_task_sessions + agent_task_sessions_v1, no supersession
	// markers. Legacy replay + migrations add supersession schema.
	restoreClassPR3
	// restoreClassAware is supersession-aware: agent_task_supersessions
	// table and agent_task_supersessions_v1 sentinel. Historical
	// replay, B3 temporal and legacy checks, idempotent migrations.
	restoreClassAware
)

// restoreSentinels are the schema_migrations markers.
const (
	restoreSentinelAssignments   = "agent_assignments_v1"
	restoreSentinelSessions      = "agent_task_sessions_v1"
	restoreSentinelSupersessions = "agent_task_supersessions_v1"
)

// restoreDumpInventory is what the dump claims to carry.
type restoreDumpInventory struct {
	tables    map[string]bool
	sentinels map[string]bool
	triggers  map[string]string // name -> normalized body (dump text)
	// triggerTargets maps a trigger name to the table its validated
	// statement header defines it ON. Identification uses this, never a
	// body-text scan: a trigger that merely READS a managed table is
	// not a managed-table trigger (F11).
	triggerTargets map[string]string
	// triggerTargetKnown records whether the target was POSITIVELY
	// resolved. A trigger whose target is unknown is refused rather
	// than replayed: absence of proof that it is unmanaged is not proof
	// that it is safe (F13).
	triggerTargetKnown map[string]bool
	// triggerOrder counts definitions per name (duplicates).
	triggerCounts map[string]int
	// unexpected managed triggers (on managed tables, not canonical).
	unexpected []string
}

// classifyRestoreDump determines the backup class from schema markers.
// Tables come from CREATE TABLE statements; sentinels from INSERT INTO
// schema_migrations rows. Lineage-row presence is never consulted.
func classifyRestoreDump(statements []string) (restoreClass, *restoreDumpInventory, error) {
	inv := &restoreDumpInventory{
		tables:             map[string]bool{},
		sentinels:          map[string]bool{},
		triggers:           map[string]string{},
		triggerTargets:     map[string]string{},
		triggerTargetKnown: map[string]bool{},
		triggerCounts:      map[string]int{},
	}
	for _, stmt := range statements {
		upper := strings.ToUpper(strings.TrimSpace(stmt))
		switch {
		case strings.HasPrefix(upper, "CREATE TABLE"):
			name := dumpCreateTableName(stmt)
			if name != "" {
				inv.tables[strings.ToLower(name)] = true
			}
		case strings.HasPrefix(upper, "CREATE TRIGGER"):
			name, _, err := parseTriggerStatement(stmt)
			if err != nil {
				return restoreClassOrdinary, nil, err
			}
			inv.triggerCounts[name]++
			if _, ok := inv.triggers[name]; !ok {
				inv.triggers[name] = normalizeTriggerBody(stmt)
			}
			if target, ok := parseTriggerTarget(stmt); ok {
				inv.triggerTargets[name] = target
				inv.triggerTargetKnown[name] = true
			}
		case strings.HasPrefix(upper, "INSERT INTO"):
			if tbl, vals := dumpInsertTarget(stmt); strings.ToLower(tbl) == "schema_migrations" {
				for _, v := range vals {
					inv.sentinels[v] = true
				}
			}
		}
	}
	hasSupersessionTable := inv.tables["agent_task_supersessions"]
	hasSupersessionSentinel := inv.sentinels[restoreSentinelSupersessions]
	hasSessionsTable := inv.tables["agent_task_sessions"]
	hasSessionsSentinel := inv.sentinels[restoreSentinelSessions]
	hasAssignmentTable := inv.tables["agent_tasks"] || inv.tables["agent_task_events"]
	hasAssignmentSentinel := inv.sentinels[restoreSentinelAssignments]

	switch {
	case hasSupersessionTable && hasSupersessionSentinel:
		return restoreClassAware, inv, nil
	case hasSupersessionTable != hasSupersessionSentinel &&
		(hasSupersessionTable || hasSupersessionSentinel):
		return restoreClassOrdinary, nil,
			fmt.Errorf("incomplete_supersession_schema: the supersession table and sentinel must appear together")
	case hasSessionsTable && hasSessionsSentinel:
		return restoreClassPR3, inv, nil
	case hasAssignmentTable && hasAssignmentSentinel:
		return restoreClassPR1, inv, nil
	case (hasAssignmentTable || hasSessionsTable) && !hasAssignmentSentinel && !hasSessionsSentinel:
		return restoreClassOrdinary, nil,
			fmt.Errorf("unrecognised_assignment_schema: assignment tables without a recognised sentinel are refused")
	default:
		return restoreClassOrdinary, inv, nil
	}
}

// dumpCreateTableName extracts the table name from a CREATE TABLE
// statement (handles IF NOT EXISTS, quoting, schema qualification).
func dumpCreateTableName(stmt string) string {
	fields := strings.Fields(strings.ToUpper(stmt))
	// Find the token after TABLE / IF NOT EXISTS.
	upper := strings.ToUpper(stmt)
	idx := strings.Index(upper, "TABLE")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(stmt[idx+len("TABLE"):])
	restUpper := strings.ToUpper(rest)
	if strings.HasPrefix(restUpper, "IF NOT EXISTS") {
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
	_ = fields
	return tok
}

// dumpInsertTarget extracts the INSERT table and, for schema_migrations,
// the id values from a single-row VALUES clause.
func dumpInsertTarget(stmt string) (string, []string) {
	upper := strings.ToUpper(stmt)
	idx := strings.Index(upper, "INSERT INTO")
	if idx < 0 {
		return "", nil
	}
	rest := strings.TrimSpace(stmt[idx+len("INSERT INTO"):])
	tok := rest
	if i := strings.IndexAny(rest, " \t\n\r("); i >= 0 {
		tok = rest[:i]
	}
	tok = strings.Trim(tok, "\"'`[]")
	if idx := strings.LastIndex(tok, "."); idx >= 0 {
		tok = tok[idx+1:]
	}
	var vals []string
	if strings.EqualFold(tok, "schema_migrations") {
		vals = dumpSingleQuotedValues(stmt)
	}
	return tok, vals
}

// dumpSingleQuotedValues extracts single-quoted literals from a VALUES
// clause (used for sentinel ids).
func dumpSingleQuotedValues(stmt string) []string {
	upper := strings.ToUpper(stmt)
	vi := strings.Index(upper, "VALUES")
	if vi < 0 {
		return nil
	}
	rest := stmt[vi:]
	var out []string
	for i := 0; i < len(rest); i++ {
		if rest[i] != '\'' {
			continue
		}
		j := i + 1
		var b strings.Builder
		for j < len(rest) {
			if rest[j] == '\'' {
				if j+1 < len(rest) && rest[j+1] == '\'' {
					b.WriteByte('\'')
					j += 2
					continue
				}
				break
			}
			b.WriteByte(rest[j])
			j++
		}
		out = append(out, b.String())
		i = j
	}
	return out
}

// verifyManagedTriggers enforces the B1 trust boundary for one backup
// class: every required managed trigger must be present with a
// byte-identical normalized body (created from canonical text, never
// from the dump), and no other managed-table trigger may appear.
//
// It returns the canonical installation map (name → canonical CREATE
// TRIGGER statement) for the required set.
func verifyManagedTriggers(class restoreClass, inv *restoreDumpInventory, inventory map[string]string) (map[string]string, error) {
	// Managed triggers are the canonical triggers on managed tables.
	// The dump's trigger text is a presence/equality claim only.
	required := canonicalTriggerNamesForClass(class, inventory)
	requiredSet := map[string]bool{}
	for _, n := range required {
		requiredSet[n] = true
	}
	// Duplicates within the dump.
	var dups []string
	for name, count := range inv.triggerCounts {
		if count > 1 && isManagedDumpTrigger(name, inventory) {
			dups = append(dups, name)
		}
	}
	sort.Strings(dups)
	if len(dups) > 0 {
		return nil, fmt.Errorf("duplicate_trigger_definition: %s", strings.Join(dups, ","))
	}
	// Unexpected: a managed-table trigger outside the canonical
	// inventory for this class. A canonical trigger not required for
	// this class (e.g. a recovery trigger in a pre-feature dump) is
	// unexpected: a pre-feature dump must not carry future guards.
	//
	// An unknown trigger is unexpected only when its VALIDATED header
	// defines it ON a managed table. A trigger on an unrelated table
	// that merely references a managed table in its body is a
	// legitimate user object and replays verbatim (F11).
	var unexpected []string
	for name := range inv.triggers {
		if isManagedDumpTrigger(name, inventory) && !requiredSet[name] {
			unexpected = append(unexpected, name)
			continue
		}
		if isManagedDumpTrigger(name, inventory) {
			continue
		}
		// FAIL CLOSED (F13): a trigger whose validated header cannot be
		// resolved cannot be proven to be unrelated to the managed
		// tables, so it is refused rather than replayed.
		if !inv.triggerTargetKnown[name] {
			unexpected = append(unexpected, name)
			continue
		}
		if managedRestoreTables[inv.triggerTargets[name]] {
			unexpected = append(unexpected, name)
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		return nil, fmt.Errorf("unexpected_trigger: %s", strings.Join(unexpected, ","))
	}
	// Missing or modified required triggers.
	var missing []string
	var modified []string
	for _, name := range required {
		dumpBody, ok := inv.triggers[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if dumpBody != inventory[name] {
			modified = append(modified, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(modified)
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing_canonical_trigger: %s", strings.Join(missing, ","))
	}
	if len(modified) > 0 {
		return nil, fmt.Errorf("modified_canonical_trigger: %s", strings.Join(modified, ","))
	}
	// Build the canonical installation map from trusted text.
	install := map[string]string{}
	for _, name := range required {
		stmt, err := canonicalTriggerStatement(name)
		if err != nil {
			return nil, err
		}
		install[name] = stmt
	}
	return install, nil
}

// isManagedDumpTrigger reports whether the name is a known canonical
// managed trigger.
func isManagedDumpTrigger(name string, inventory map[string]string) bool {
	_, ok := inventory[name]
	return ok
}

// canonicalTriggerStatement returns the trusted CREATE TRIGGER text for
// one canonical trigger, extracted from the migration DDL.
func canonicalTriggerStatement(name string) (string, error) {
	for _, src := range canonicalTriggerSources() {
		for _, stmt := range splitTriggerStatements(src.ddl) {
			n, _, err := parseTriggerStatement(stmt)
			if err != nil {
				continue
			}
			if n == name {
				return stmt, nil
			}
		}
	}
	return "", fmt.Errorf("restore: canonical text missing for trigger %q", name)
}
