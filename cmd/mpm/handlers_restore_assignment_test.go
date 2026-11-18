// handlers_restore_assignment_test.go — AT8 end to end, for the
// Continuity 001 assignment storage.
//
// The assignment migration adds two tables to CanonicalMPMSchema, the
// restore-db allow-list. The acceptance gate (AT8) says: "`mpm
// restore-db` ACCEPTS it".
//
// The allow-list assertion lives in internal/core
// (TestAgentAssignmentAT8RestoreAllowList), because that is where
// CanonicalMPMSchema lives. This file closes the loop through the
// COMMAND: it drives the real `handleRestoreDB` handler over a real
// dump and requires it to succeed.
//
// WHY THE DUMP IS GENERATED, NOT HAND-WRITTEN
//
//	A hand-written dump would test a hand-written dump: it would only
//	prove that the validator accepts the shape the test author
//	imagined. Instead this test builds the dump FROM THE LIVE SCHEMA
//	of a hermetic workspace — the real sqlite_master.sql text of every
//	table, every index and every trigger, plus the real rows. So the
//	dump is what `sqlite3 .dump` would produce for the assignment
//	schema, and the restore has to cope with the approved DDL as it
//	actually stands.
//
// WHY THE STATEMENT ORDER IS CREATE / CREATE INDEX / CREATE TRIGGER /
// INSERT
//
//	`agent_tasks` carries a SELF-REFERENCING composite foreign key —
//	(root_id, project_id) and (parent_id, project_id) both reference
//	agent_tasks(id, project_id) — and SQLite checks a composite foreign
//	key against its parent-key UNIQUE index at DML time. So the
//	index must exist before any INSERT, or the restore fails with
//	"foreign key mismatch" on a dump that is perfectly valid SQL.
//
//	The triggers come after the indexes and before the rows, so that
//	the rows are inserted THROUGH the approved rules. A row that the
//	schema refuses would fail the restore — which is the point: the
//	dump must restore a schema that accepts its own rows.
//
//	`sqlite3 .dump` emits the data before the indexes, which would
//	NOT restore a schema with composite foreign keys. That is a
//	property of the dump tool, not of the schema, and it is out of
//	scope here — what matters is that a dump carrying this schema is
//	ACCEPTED and restores cleanly.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// assignmentRestoreTables are the tables the dump carries: the two
// assignment tables and the project they belong to.
var assignmentRestoreTables = []string{
	"projects",
	"agent_tasks",
	"agent_task_events",
}

// assignmentRestoreIndexes are the seven indexes the approved DDL
// creates. They are named explicitly so a missing index fails by name.
var assignmentRestoreIndexes = []string{
	"idx_agent_task_events_created",
	"idx_agent_tasks_id_project",
	"idx_agent_tasks_idempotency_key",
	"idx_agent_tasks_project",
	"idx_agent_tasks_recipient_state",
	"idx_agent_tasks_root",
	"idx_agent_tasks_state_updated",
}

// assignmentRestoreTriggers are the twenty-nine triggers the approved
// DDL creates. They are named explicitly so a missing trigger fails by
// name.
//
// The list is the WHOLE inventory, not a subset. The fifth review found
// that it named twenty-five of the twenty-six the DDL created — the
// fourth review's update-path guard was missing — so the dumps this file
// builds restored a database WITHOUT that guard, and the assertions,
// which only looked for the listed names, could not see the omission. A
// stale inventory here is a silently weaker restore, so the test also
// compares the list against the LIVE schema.
var assignmentRestoreTriggers = []string{
	"trg_agent_task_events_id_is_not_reused",
	"trg_agent_task_events_monotonic",
	"trg_agent_task_events_no_delete",
	"trg_agent_task_events_no_update",
	"trg_agent_tasks_approval_is_immutable",
	"trg_agent_tasks_awaiting_action_is_immutable",
	"trg_agent_tasks_claim_discipline",
	"trg_agent_tasks_claim_needs_a_live_assignment",
	"trg_agent_tasks_claim_sets_lease",
	"trg_agent_tasks_claim_sets_owner",
	"trg_agent_tasks_depth_is_immutable",
	"trg_agent_tasks_eligible_recipients_insert",
	"trg_agent_tasks_eligible_recipients_update",
	"trg_agent_tasks_id_is_immutable",
	"trg_agent_tasks_id_is_not_reused",
	"trg_agent_tasks_initial_state",
	"trg_agent_tasks_key_is_immutable",
	"trg_agent_tasks_lease_changes_only_on_a_claim",
	"trg_agent_tasks_not_deletable",
	"trg_agent_tasks_owner_changes_only_on_claim",
	"trg_agent_tasks_permitted_operations_insert",
	"trg_agent_tasks_permitted_operations_update",
	"trg_agent_tasks_role_is_immutable",
	"trg_agent_tasks_root_recipient_is_eligible",
	"trg_agent_tasks_root_recipient_is_eligible_on_update",
	"trg_agent_tasks_state_transition",
	"trg_agent_tasks_tree_shape",
	"trg_agent_tasks_tree_shape_on_update",
	"trg_agent_tasks_unclaimed_at_creation",
}

// seedAssignmentRows writes one project, one well-formed ROOT assignment
// and one ledger entry.
//
// The assignment satisfies every rule the schema enforces: it is born
// enqueued and unclaimed, it carries the root's budgets, it names at
// least one permitted operation, and its own recipient is among its
// eligible recipients. The ledger entry carries index 0, which is the
// first index for that assignment.
//
// This is the storage's own guarantee, exercised here rather than
// assumed: if the row did not satisfy the schema, the restore would
// fail.
func seedAssignmentRows(t *testing.T, db *sql.DB) {
	t.Helper()

	_, err := db.Exec(`INSERT INTO projects (id, created_by)
		VALUES ('cpj-0123456789abcdef', 'assignment-restore-test')`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO agent_tasks
		(id, project_id, root_id, parent_id, depth, title, objective,
		 recipient, role, state, idempotency_key,
		 max_depth, max_tasks, max_concurrency, max_resource_units,
		 permitted_operations, eligible_recipients)
		VALUES ('at-0123456789abcdef', 'cpj-0123456789abcdef', 'at-0123456789abcdef',
			NULL, 0, 'the assignment', 'the objective', 'alpha', 'implementer',
			'enqueued', 'github:issue:253', 1, 4, 2, 100, '["read"]', '["alpha"]')`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO agent_task_events
		(id, task_id, event_index, event_type)
		VALUES ('ate-0123456789abcdef', 'at-0123456789abcdef', 0, 'created')`)
	require.NoError(t, err)
}

// buildAssignmentDump renders a dump of the assignment schema from the
// live schema: the real sqlite_master.sql text of every table, index
// and trigger, plus one INSERT per row.
//
// The INSERT statements are rendered by SQLite itself (quote() per
// column, group_concat for the value list), so a NULL stays a NULL and a
// JSON string keeps its quotes. NOTE that quote() renders EVERY value as
// TEXT — an integer comes back as '0' — and the column's affinity is
// what restores the type when the row is inserted again.
func buildAssignmentDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("PRAGMA foreign_keys=OFF;\n")
	b.WriteString("BEGIN TRANSACTION;\n")

	// 1. The tables.
	for _, table := range assignmentRestoreTables {
		var ddl string
		row := db.QueryRow(
			"SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table,
		)
		require.NoError(t, row.Scan(&ddl), "the table %s must exist", table)
		require.NotEmpty(t, ddl)
		b.WriteString(ddl)
		b.WriteString(";\n")
	}

	// 2. The indexes. They must exist before the INSERTs: the
	//    composite foreign keys are checked against their parent-key
	//    UNIQUE index at DML time, so an INSERT that precedes the
	//    index would fail with "foreign key mismatch".
	for _, index := range assignmentRestoreIndexes {
		var ddl string
		row := db.QueryRow(
			"SELECT sql FROM sqlite_master WHERE type='index' AND name=?", index,
		)
		require.NoError(t, row.Scan(&ddl), "the index %s must exist", index)
		require.NotEmpty(t, ddl)
		b.WriteString(ddl)
		b.WriteString(";\n")
	}

	// 3. The triggers. They come before the rows so that the rows are
	//    inserted THROUGH the approved rules.
	for _, trigger := range assignmentRestoreTriggers {
		var ddl string
		row := db.QueryRow(
			"SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?", trigger,
		)
		require.NoError(t, row.Scan(&ddl), "the trigger %s must exist", trigger)
		require.NotEmpty(t, ddl)
		b.WriteString(ddl)
		b.WriteString(";\n")
	}

	// 4. The rows.
	for _, table := range assignmentRestoreTables {
		cols := assignmentDumpColumns(t, db, table)
		require.NotEmpty(t, cols, "the table %s must have columns", table)

		quoted := make([]string, 0, len(cols))
		values := make([]string, 0, len(cols))
		for _, c := range cols {
			quoted = append(quoted, `"`+c+`"`)
			values = append(values, `quote("`+c+`")`)
		}
		q := fmt.Sprintf(
			`SELECT 'INSERT INTO "%s" (%s) VALUES (' || group_concat(%s, ',') || ');' FROM "%s"`,
			table, strings.Join(quoted, ", "), strings.Join(values, " || ',' || "), table,
		)

		rows, err := db.Query(q)
		require.NoError(t, err, "render the rows of %s", table)
		for rows.Next() {
			var stmt string
			require.NoError(t, rows.Scan(&stmt))
			b.WriteString(stmt)
			b.WriteString("\n")
		}
		require.NoError(t, rows.Err())
		rows.Close()
	}

	b.WriteString("COMMIT;\n")
	return b.String()
}

// assignmentDumpColumns returns a table's column names in declaration
// order.
func assignmentDumpColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%q)", table))
	require.NoError(t, err)
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		require.NoError(t, rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk))
		cols = append(cols, name)
	}
	require.NoError(t, rows.Err())
	return cols
}

// TestHandleRestoreDB_AcceptsAssignmentDump — AT8, through the command.
//
// A dump carrying the two assignment tables — with their real DDL,
// their real indexes, their real triggers and real rows — is fed to
// `mpm restore-db`. The command must ACCEPT it (exit 0), and the
// restored database must carry all two tables, all seven indexes and
// all twenty-nine triggers, AND the rows.
//
// The trigger list is the whole inventory, and this test also compares
// it against the LIVE schema: a trigger the DDL creates and the list
// does not name would restore a weaker schema, and only the comparison
// can see it.
func TestHandleRestoreDB_AcceptsAssignmentDump(t *testing.T) {
	dm := setupRestoreSafetyTest(t)
	dbPath := dm.DBPath()
	require.NotEmpty(t, dbPath)

	// Seed one project, one assignment and one ledger entry, so the
	// dump carries data and not just structure.
	seedAssignmentRows(t, dm.SQLDB())

	dump := buildAssignmentDump(t, dm.SQLDB())
	dumpPath := filepath.Join(t.TempDir(), "assignment.sql")
	require.NoError(t, os.WriteFile(dumpPath, []byte(dump), 0o600))

	// The dump must actually carry the tables, or the acceptance below
	// would prove nothing.
	for _, table := range assignmentRestoreTables {
		require.Contains(t, dump, table,
			"the generated dump must declare %s", table)
	}

	// The trigger inventory must be the WHOLE inventory.
	//
	// The fifth review found that the list named twenty-five of the
	// twenty-six triggers the DDL creates, so the dump restored a
	// database without the fourth review's update-path guard — and
	// every assertion below still passed, because the assertions only
	// looked for the LISTED names. Comparing the list against the live
	// schema is what makes a stale inventory visible.
	live := map[string]bool{}
	rows, err := dm.SQLDB().Query(
		`SELECT name FROM sqlite_master WHERE type='trigger' AND name LIKE 'trg_agent_task%'`,
	)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		live[name] = true
	}
	require.NoError(t, rows.Err())
	rows.Close()

	for name := range live {
		require.Contains(t, assignmentRestoreTriggers, name,
			"the live schema has the trigger %s and the restore inventory does not, "+
				"so the restored database would be missing a rule", name)
	}
	require.Len(t, assignmentRestoreTriggers, len(live),
		"the restore inventory names %d triggers and the live schema has %d — "+
			"the two inventories must be the same set",
		len(assignmentRestoreTriggers), len(live))

	// H-4 contract: the test-process DM holds LOCK_SH on
	// `<dbPath>.lock`. Release it before the handler asks for LOCK_EX,
	// exactly as TestHandleRestoreDB_SuccessOverwritesOriginal does.
	_ = dm.Close()
	dbManager = nil
	dbManagerInitErr = nil
	dbManagerOnce = sync.Once{}

	driveStdinYes(t)

	exit := handleRestoreDB([]string{"mpm", dumpPath})
	require.Equal(t, 0, exit,
		"mpm restore-db must accept a dump of the assignment schema — "+
			"the two tables are on CanonicalMPMSchema, which is the allow-list "+
			"restore-db validates against")

	// The restored database carries the two tables, the seven indexes
	// and the twenty-nine triggers. The restore replaces the file, so
	// this reads the NEW inode at dbPath.
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	require.NoError(t, err)
	defer db.Close()

	for _, table := range []string{"agent_tasks", "agent_task_events"} {
		var n int
		row := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table,
		)
		require.NoError(t, row.Scan(&n))
		require.Equal(t, 1, n,
			"the restored database must carry the table %s", table)
	}
	for _, index := range assignmentRestoreIndexes {
		var n int
		row := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", index,
		)
		require.NoError(t, row.Scan(&n))
		require.Equal(t, 1, n,
			"the restored database must carry the index %s", index)
	}
	for _, trigger := range assignmentRestoreTriggers {
		var n int
		row := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?", trigger,
		)
		require.NoError(t, row.Scan(&n))
		require.Equal(t, 1, n,
			"the restored database must carry the trigger %s", trigger)
	}

	// And the ROWS came back — the restore carries data, not just
	// structure.
	var state, recipient string
	var epoch int64
	row := db.QueryRow(
		`SELECT state, recipient, claim_epoch FROM agent_tasks WHERE id = ?`,
		"at-0123456789abcdef",
	)
	require.NoError(t, row.Scan(&state, &recipient, &epoch))
	require.Equal(t, "enqueued", state)
	require.Equal(t, "alpha", recipient)
	require.Equal(t, int64(0), epoch)

	var eventType string
	row = db.QueryRow(
		`SELECT event_type FROM agent_task_events WHERE id = ?`,
		"ate-0123456789abcdef",
	)
	require.NoError(t, row.Scan(&eventType))
	require.Equal(t, "created", eventType)
}

// buildAssignmentDumpInDumpOrder renders the dump in the shape
// `sqlite3 .dump` produces — which is the shape `mpm backup` writes,
// because that command IS `sqlite3 .dump`.
//
// The differences from buildAssignmentDump are all in the ORDER:
//
//   - the dump opens with `PRAGMA foreign_keys=OFF;` and
//     `BEGIN TRANSACTION;`;
//   - each table's DATA follows its own CREATE TABLE, so the rows are
//     inserted BEFORE the indexes and before the triggers;
//   - the indexes and the triggers come at the end.
//
// That order matters, and the reason is the composite foreign key:
// agent_tasks references agent_tasks(id, project_id), and SQLite checks a
// composite foreign key against its parent-key UNIQUE index at DML time.
// With the pragma off — as the real dump has it — the rows insert
// cleanly; with the pragma on, the same order fails with "foreign key
// mismatch".
//
// The first test in this file uses the ORDER that proves the rows satisfy
// the schema (indexes and triggers first, then the rows). This one proves
// the other thing: that a dump in the shape the backup command actually
// writes restores.
func buildAssignmentDumpInDumpOrder(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("PRAGMA foreign_keys=OFF;\n")
	b.WriteString("BEGIN TRANSACTION;\n")

	// The tables, each followed by its own rows.
	for _, table := range assignmentRestoreTables {
		var ddl string
		row := db.QueryRow(
			"SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table,
		)
		require.NoError(t, row.Scan(&ddl), "the table %s must exist", table)
		require.NotEmpty(t, ddl)
		b.WriteString(ddl)
		b.WriteString(";\n")

		cols := assignmentDumpColumns(t, db, table)
		require.NotEmpty(t, cols, "the table %s must have columns", table)

		quoted := make([]string, 0, len(cols))
		values := make([]string, 0, len(cols))
		for _, c := range cols {
			quoted = append(quoted, `"`+c+`"`)
			values = append(values, `quote("`+c+`")`)
		}
		q := fmt.Sprintf(
			`SELECT 'INSERT INTO "%s" (%s) VALUES (' || group_concat(%s, ',') || ');' FROM "%s"`,
			table, strings.Join(quoted, ", "), strings.Join(values, " || ',' || "), table,
		)
		rows, err := db.Query(q)
		require.NoError(t, err, "render the rows of %s", table)
		for rows.Next() {
			var stmt string
			require.NoError(t, rows.Scan(&stmt))
			b.WriteString(stmt)
			b.WriteString("\n")
		}
		require.NoError(t, rows.Err())
		rows.Close()
	}

	// The indexes, then the triggers.
	for _, index := range assignmentRestoreIndexes {
		var ddl string
		row := db.QueryRow(
			"SELECT sql FROM sqlite_master WHERE type='index' AND name=?", index,
		)
		require.NoError(t, row.Scan(&ddl), "the index %s must exist", index)
		require.NotEmpty(t, ddl)
		b.WriteString(ddl)
		b.WriteString(";\n")
	}
	for _, trigger := range assignmentRestoreTriggers {
		var ddl string
		row := db.QueryRow(
			"SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?", trigger,
		)
		require.NoError(t, row.Scan(&ddl), "the trigger %s must exist", trigger)
		require.NotEmpty(t, ddl)
		b.WriteString(ddl)
		b.WriteString(";\n")
	}

	b.WriteString("COMMIT;\n")
	return b.String()
}

// TestHandleRestoreDB_AcceptsAssignmentDumpInDumpOrder — AT8, in the
// shape the backup command actually writes.
//
// `mpm backup` IS `sqlite3 .dump`, so the dump that has to restore is
// the one `.dump` produces: the data before the indexes, and the foreign
// keys turned off for the duration. This test builds exactly that shape
// and requires `mpm restore-db` to accept it and to restore the rows.
//
// The fourth review found that the first test's dump was NOT that shape,
// and that the shape it used would fail under the handler's conditions if
// the foreign keys were on. Both statements are true, and the second one
// is the reason this test exists: the storage has to survive the backup
// the operator actually takes.
func TestHandleRestoreDB_AcceptsAssignmentDumpInDumpOrder(t *testing.T) {
	dm := setupRestoreSafetyTest(t)
	dbPath := dm.DBPath()
	require.NotEmpty(t, dbPath)

	seedAssignmentRows(t, dm.SQLDB())

	dump := buildAssignmentDumpInDumpOrder(t, dm.SQLDB())
	dumpPath := filepath.Join(t.TempDir(), "assignment-dump-order.sql")
	require.NoError(t, os.WriteFile(dumpPath, []byte(dump), 0o600))

	// The dump must be in the shape the backup command writes.
	require.Contains(t, dump, "PRAGMA foreign_keys=OFF;")
	require.Contains(t, dump, "BEGIN TRANSACTION;")

	_ = dm.Close()
	dbManager = nil
	dbManagerInitErr = nil
	dbManagerOnce = sync.Once{}

	driveStdinYes(t)

	exit := handleRestoreDB([]string{"mpm", dumpPath})
	require.Equal(t, 0, exit,
		"mpm restore-db must accept a dump in the shape `sqlite3 .dump` "+
			"produces, because that is the shape `mpm backup` writes")

	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	require.NoError(t, err)
	defer db.Close()

	// The tables are there, and so are the rows.
	for _, table := range []string{"agent_tasks", "agent_task_events"} {
		var n int
		row := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table,
		)
		require.NoError(t, row.Scan(&n))
		require.Equal(t, 1, n, "the restored database must carry the table %s", table)
	}
	var n int
	row := db.QueryRow(`SELECT COUNT(*) FROM agent_tasks WHERE id = ?`, "at-0123456789abcdef")
	require.NoError(t, row.Scan(&n))
	require.Equal(t, 1, n, "the restored database must carry the assignment's row")
	row = db.QueryRow(`SELECT COUNT(*) FROM agent_task_events WHERE id = ?`, "ate-0123456789abcdef")
	require.NoError(t, row.Scan(&n))
	require.Equal(t, 1, n, "the restored database must carry the ledger entry")
}
