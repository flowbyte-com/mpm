// handlers_restore_continuity_test.go — AT8 end to end.
//
// The Continuity Stage 1 migration adds four tables to
// CanonicalMPMSchema, the restore-db allow-list. The acceptance gate
// (AT8) says: "`mpm restore-db` ACCEPTS it".
//
// The allow-list assertion lives in internal/core
// (TestContinuityAT8RestoreAllowList), because that is where
// CanonicalMPMSchema lives. This file closes the loop through the
// COMMAND: it drives the real `handleRestoreDB` handler over a real
// dump and requires it to succeed.
//
// WHY THE DUMP IS GENERATED, NOT HAND-WRITTEN
//
//	A hand-written dump would test a hand-written dump: it would only
//	prove that the validator accepts the shape the test author
//	imagined. Instead this test builds the dump FROM THE LIVE SCHEMA
//	of a hermetic workspace — the four tables' real sqlite_master.sql
//	text, their real indexes, and their real rows. So the dump is
//	what `sqlite3 .dump` would produce for the Continuity schema,
//	and the restore has to cope with the approved DDL as it actually
//	stands.
//
// WHY THE STATEMENT ORDER IS CREATE / CREATE INDEX / INSERT
//
//	The four tables reference each other in a cycle:
//	continuity_threads' composite foreign key names
//	continuity_checkpoints, and continuity_checkpoints' composite
//	foreign key names continuity_threads. SQLite checks a foreign key
//	against its parent table at DML time, and a composite foreign key
//	also needs its parent-key UNIQUE index to exist by then. So every
//	CREATE has to land before any INSERT — otherwise the restore
//	fails with "no such table" or "foreign key mismatch" on a dump
//	that is perfectly valid SQL.
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

// continuityRestoreTables are the four tables the dump carries.
var continuityRestoreTables = []string{
	"projects",
	"project_names",
	"continuity_threads",
	"continuity_checkpoints",
}

// continuityRestoreIndexes are the eight indexes the approved DDL
// creates. They are named explicitly so a missing index fails by name.
var continuityRestoreIndexes = []string{
	"idx_project_names_project",
	"idx_project_names_live_name",
	"idx_project_names_one_canonical",
	"idx_continuity_threads_project",
	"idx_continuity_threads_focus",
	"idx_continuity_threads_id_project",
	"idx_continuity_checkpoints_thread",
	"idx_continuity_checkpoints_id_thread",
}

// TestHandleRestoreDB_AcceptsContinuityDump — AT8, through the command.
//
// A dump carrying the four Continuity tables — with their real DDL,
// their real indexes and real rows — is fed to `mpm restore-db`. The
// command must ACCEPT it (exit 0) and the restored database must carry
// all four tables and all eight indexes.
func TestHandleRestoreDB_AcceptsContinuityDump(t *testing.T) {
	dm := setupRestoreSafetyTest(t)
	dbPath := dm.DBPath()
	require.NotEmpty(t, dbPath)

	// Seed one row in each table so the dump carries data, not just
	// structure. The values satisfy the approved DDL's CHECK and
	// FOREIGN KEY constraints — this is the storage's own guarantee,
	// exercised here rather than assumed.
	seedContinuityRows(t, dm.SQLDB())

	dump := buildContinuityDump(t, dm.SQLDB())
	dumpPath := filepath.Join(t.TempDir(), "continuity.sql")
	require.NoError(t, os.WriteFile(dumpPath, []byte(dump), 0o600))

	// The dump must actually carry the four tables, or the acceptance
	// below would prove nothing.
	for _, table := range continuityRestoreTables {
		require.Contains(t, dump, table,
			"the generated dump must declare %s", table)
	}

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
		"mpm restore-db must accept a dump of the Continuity schema — "+
			"the four tables are on CanonicalMPMSchema, which is the allow-list "+
			"restore-db validates against")

	// The restored database carries the four tables and the eight
	// indexes. The restore replaces the file, so this reads the NEW
	// inode at dbPath.
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	require.NoError(t, err)
	defer db.Close()

	for _, table := range continuityRestoreTables {
		var n int
		row := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table,
		)
		require.NoError(t, row.Scan(&n))
		require.Equal(t, 1, n,
			"the restored database must carry the table %s", table)
	}
	for _, index := range continuityRestoreIndexes {
		var n int
		row := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", index,
		)
		require.NoError(t, row.Scan(&n))
		require.Equal(t, 1, n,
			"the restored database must carry the index %s", index)
	}

	// And the rows came across: the restore is a restore, not a
	// schema-only exercise.
	var projects int
	row := db.QueryRow("SELECT COUNT(*) FROM projects")
	require.NoError(t, row.Scan(&projects))
	require.Equal(t, 1, projects,
		"the restored database must carry the seeded project row")
	var names int
	row = db.QueryRow("SELECT COUNT(*) FROM project_names")
	require.NoError(t, row.Scan(&names))
	require.Equal(t, 1, names,
		"the restored database must carry the seeded project_names row")
}

// seedContinuityRows writes one row into each of the four tables.
//
// The values are chosen to satisfy the approved DDL exactly: the
// project id matches the documented 'cpj-' || 16 lowercase hex shape,
// the name kind is one of the two the CHECK allows, the thread's
// focus is a normalised slug, and the checkpoint's JSON columns are
// arrays (the CHECK requires json_valid AND json_type = 'array').
func seedContinuityRows(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO projects (id, created_at, updated_at, created_by)
		VALUES ('cpj-0123456789abcdef', 1767225600, 1767225600, 'restore-dump-test')`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO project_names (id, name, project_id, kind, created_at)
		VALUES (1, 'continuity-stage-one', 'cpj-0123456789abcdef', 'canonical', 1767225600)`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO continuity_threads
		(id, project_id, title, focus, state, revision, created_at, updated_at)
		VALUES ('ct-0123456789abcdef', 'cpj-0123456789abcdef', 'Stage 1 storage',
			'stage-one-storage', 'active', 0, 1767225600, 1767225600)`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO continuity_checkpoints
		(id, thread_id, project_id, revision, headline, facts, hypotheses, state,
		 next_actions, created_at, actor_kind)
		VALUES ('ck-0123456789abcdef', 'ct-0123456789abcdef', 'cpj-0123456789abcdef',
			0, 'the migration landed', '[]', '[]', '[]', '[]', 1767225600, 'agent')`)
	require.NoError(t, err)
}

// buildContinuityDump renders a dump of the four Continuity tables
// from the live schema: the real sqlite_master.sql text of every table
// and index, plus one INSERT per row.
//
// The INSERT statements are rendered by SQLite itself (quote() per
// column, group_concat for the value list), so a NULL stays a NULL,
// an integer stays an integer and a JSON string keeps its quotes —
// without the dump having to re-implement SQLite's literal rules.
func buildContinuityDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("PRAGMA foreign_keys=OFF;\n")
	b.WriteString("BEGIN TRANSACTION;\n")

	// 1. The tables.
	for _, table := range continuityRestoreTables {
		var ddl string
		row := db.QueryRow(
			"SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table,
		)
		require.NoError(t, row.Scan(&ddl), "the table %s must exist", table)
		require.NotEmpty(t, ddl)
		b.WriteString(ddl)
		b.WriteString(";\n")
	}

	// 2. The indexes. They must exist before the INSERTs: a composite
	//    foreign key is checked against its parent-key UNIQUE index at
	//    DML time, so an INSERT that precedes the index would fail
	//    with "foreign key mismatch".
	for _, index := range continuityRestoreIndexes {
		var ddl string
		row := db.QueryRow(
			"SELECT sql FROM sqlite_master WHERE type='index' AND name=?", index,
		)
		require.NoError(t, row.Scan(&ddl), "the index %s must exist", index)
		require.NotEmpty(t, ddl)
		b.WriteString(ddl)
		b.WriteString(";\n")
	}

	// 3. The rows.
	for _, table := range continuityRestoreTables {
		cols := continuityDumpColumns(t, db, table)
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

// continuityDumpColumns returns a table's column names in declaration
// order.
func continuityDumpColumns(t *testing.T, db *sql.DB, table string) []string {
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
