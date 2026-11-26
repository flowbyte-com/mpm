package main

// handlers_restore_backup_roundtrip_test.go — the REAL backup round trip.
//
// The earlier restore tests build dumps by hand. A hand-built dump is a
// claim about the dump format, and the claim was WRONG: a genuine
// `sqlite3 .dump` carries `replace(...)`, `char(...)` and blob literals
// in ordinary application tables, and the restore refused them (F1);
// re-rendering then coerced numeric-looking TEXT to numbers (F6).
//
// These tests exercise the real commands' entry points — `handleBackup`
// (which shells out to `sqlite3 .dump`) and `handleRestoreDB` — and
// compare LOGICAL CONTENTS AND SQLITE STORAGE TYPES before and after.
// Row counts and exit codes are not evidence of preservation.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/assignment"
	"github.com/stretchr/testify/require"
)

// managedRoundTripTables are the tables compared value-for-value.
var managedRoundTripTables = []string{
	"projects",
	"agent_tasks",
	"agent_task_sessions",
	"agent_task_supersessions",
	"agent_task_events",
}

// realBackup writes a genuine `sqlite3 .dump` of the workspace database
// through the real backup handler and returns its path and text.
func realBackup(t *testing.T, dm *mpminternal.DatabaseManager, name string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.Equal(t, 0, handleBackup([]string{"mpm", path}),
		"the real backup command must succeed")
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, body, "the backup must carry statements")
	return path, string(body)
}

// tableSnapshot reads every row of a table as an ordered list of typed
// columns, so a test can compare VALUES and STORAGE CLASSES rather than
// counts. Ordering is by a stable key per table.
func tableSnapshot(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	cols := liveColumnNames(t, db, table)
	if len(cols) == 0 {
		t.Fatalf("table %s has no columns", table)
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = fmt.Sprintf("%q", c)
	}
	// typeof() for every column: the storage class is part of what a
	// restore must preserve, and it is what the numeric-looking-TEXT
	// defect changed.
	sel := make([]string, len(cols))
	for i, c := range quoted {
		sel[i] = fmt.Sprintf("typeof(%s), quote(%s)", c, c)
	}
	order := "rowid"
	if table == "schema_migrations" {
		order = "id"
	}
	rows, err := db.Query(fmt.Sprintf(
		"SELECT %s FROM %s ORDER BY %s", strings.Join(sel, ", "), table, order))
	require.NoError(t, err, "read %s", table)
	defer rows.Close()
	// Each column contributes TWO values: its storage class and its
	// quoted value.
	nvals := 2 * len(cols)
	var out []string
	for rows.Next() {
		vals := make([]interface{}, nvals)
		ptrs := make([]interface{}, nvals)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...), "scan %s", table)
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	require.NoError(t, rows.Err())
	return out
}

// liveColumnNames lists a live table's columns.
func liveColumnNames(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	require.NoError(t, err)
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		cols = append(cols, n)
	}
	return cols
}

// assertTableUnchanged compares one table between two connections.
func assertTableUnchanged(t *testing.T, before, after *sql.DB, table string) {
	t.Helper()
	want := tableSnapshot(t, before, table)
	got := tableSnapshot(t, after, table)
	require.Equal(t, len(want), len(got), "%s lost or gained rows", table)
	for i := range want {
		require.Equal(t, want[i], got[i], "%s row %d changed", table, i)
	}
}

// assertCanonicalRestoredDatabase requires the restored database to
// carry an empty foreign-key check and the same managed trigger set the
// pre-restore database carried (whose bodies the restore pipeline has
// already verified against canonical text before commit).
func assertCanonicalRestoredDatabase(t *testing.T, before, after *sql.DB) {
	t.Helper()
	var fkViolations int
	rows, err := after.Query(`PRAGMA foreign_key_check`)
	require.NoError(t, err)
	for rows.Next() {
		fkViolations++
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.Zero(t, fkViolations, "the restored database has foreign-key violations")

	triggerNames := func(db *sql.DB) []string {
		rs, err := db.Query(
			`SELECT name FROM sqlite_master
			  WHERE type='trigger' AND name LIKE 'trg\_agent\_task%' ESCAPE '\'
			  ORDER BY name`)
		require.NoError(t, err)
		defer rs.Close()
		var out []string
		for rs.Next() {
			var n string
			require.NoError(t, rs.Scan(&n))
			out = append(out, n)
		}
		return out
	}
	wantTriggers := triggerNames(before)
	require.NotEmpty(t, wantTriggers, "the fixture must carry managed triggers")
	require.Equal(t, wantTriggers, triggerNames(after),
		"the restored database must carry the same managed triggers")
}

// seedRichTree builds an ordinary assignment tree with real attempts,
// events and evidence so the round trip has execution state to carry.
func seedRichTree(t *testing.T, store *assignment.Store, projectID string) {
	t.Helper()
	auth := assignment.NewLocalOperatorAuthority("operator")
	root, _, err := store.CreateRoot(auth, assignment.CreateRootParams{
		ProjectID: projectID, Title: "the root", Objective: "the objective",
		Recipient: "alpha", IdempotencyKey: "idem:rt-real:root",
		PermittedOperations: []string{"read", "write"},
		EligibleRecipients:  []string{"alpha", "beta"},
		MaxDepth:            3, MaxTasks: 16, MaxConcurrency: 2, MaxResourceUnits: 100,
	})
	require.NoError(t, err)
	child, _, err := store.Delegate(assignment.DelegateParams{
		ParentID: root.ID, Title: "the child", Objective: "the work",
		Recipient: "alpha", Role: assignment.RoleImplementer,
		IdempotencyKey: "idem:rt-real:child", ResourceUnits: 2,
		Actor: auth.Actor(),
	})
	require.NoError(t, err)
	actor := assignment.Actor{Address: "alpha", Kind: "agent"}
	for _, step := range []func(assignment.FencedParams) (*assignment.Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		_, err := step(assignment.FencedParams{ID: child.ID, Actor: actor})
		require.NoError(t, err)
	}
	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "alpha", LeaseSeconds: 600, Actor: actor,
	})
	require.NoError(t, err)
	_, err = store.Start(assignment.FencedParams{
		ID: child.ID, Epoch: claimed.ClaimEpoch, Actor: actor})
	require.NoError(t, err)
	_, err = store.BeginAttempt(assignment.BeginAttemptParams{
		TaskID: child.ID, Epoch: claimed.ClaimEpoch, Runtime: assignment.RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_rt_real",
		CorrelationRef: "corr-rt-real", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	require.NoError(t, err)
	_, err = store.Note(assignment.NoteParams{
		ID: child.ID, Note: "a note with 'quotes', unicode ✓ and\ntwo lines", Actor: actor})
	require.NoError(t, err)
}

// TestRealBackupRoundTripPreservesManagedRowsIsT117 backs the workspace
// up with the REAL backup command and restores it with the REAL restore
// command, then compares every managed row — values and storage classes
// — plus the canonical schema and foreign keys.
func TestRealBackupRoundTripPreservesManagedRowsIsT117(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	resetDBSingleton(t)
	dm := getDBConcrete()
	require.NotNil(t, dm)
	dbPath := dm.DBPath()
	require.NoError(t, func() error {
		_, err := dm.SQLDB().Exec(`INSERT INTO projects (id, created_by) VALUES ('cpj-rtreal00000001','rt')`)
		return err
	}())
	seedRichTree(t, assignment.NewStore(dm), "cpj-rtreal00000001")

	_, dump := realBackup(t, dm, "real.sql")
	// The dump must carry the representations that broke the old
	// pipeline; otherwise this test would pass over a dump that never
	// exercised them.
	require.Contains(t, dump, "replace(", "the real dump must carry replace(...) text")
	require.Contains(t, dump, "X'", "the real dump must carry blob literals")

	before := openRestored(t, dbPath)

	// The restore handler closes the singleton and rewrites the file.
	require.Equal(t, 0, runRestoreDB(t, dm, dump), "the real restore must accept its own backup")
	after := openRestored(t, dbPath)

	for _, table := range managedRoundTripTables {
		assertTableUnchanged(t, before, after, table)
	}
	assertCanonicalRestoredDatabase(t, before, after)

	// Representative non-managed application data survives verbatim.
	// `memories` carries the seeded directives, including the rows the
	// dump spells with replace(...).
	var seeded int
	require.NoError(t, after.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'directives'`).Scan(&seeded))
	require.Positive(t, seeded, "the seeded directives must survive the restore")
	var beforeSeeded int
	require.NoError(t, before.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'directives'`).Scan(&beforeSeeded))
	require.Equal(t, beforeSeeded, seeded, "the restored directives differ in number")

	// A second generation: back the restored database up and restore it
	// again. A restore that corrupts on the first pass often fails the
	// second, and the contract's round trip is not one-way.
	resetDBSingleton(t)
	dm2 := getDBConcrete()
	require.NotNil(t, dm2)
	_, gen2Dump := realBackup(t, dm2, "gen2.sql")
	require.Equal(t, 0, runRestoreDB(t, dm2, gen2Dump),
		"the second-generation restore must accept the restored database's backup")
	gen2 := openRestored(t, dbPath)
	for _, table := range managedRoundTripTables {
		assertTableUnchanged(t, before, gen2, table)
	}
	assertCanonicalRestoredDatabase(t, before, gen2)
}

// TestRealBackupPreservesSQLValueTypesIsF6 proves numeric-looking TEXT,
// empty strings, NULL, quotes, unicode and multiline values keep BOTH
// their value and their SQLite storage class through a real round trip.
func TestRealBackupPreservesSQLValueTypesIsF6(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	resetDBSingleton(t)
	dm := getDBConcrete()
	require.NotNil(t, dm)
	dbPath := dm.DBPath()
	_, err := dm.SQLDB().Exec(`INSERT INTO projects (id, created_by) VALUES ('cpj-rttypes0000001','rt')`)
	require.NoError(t, err)
	store := assignment.NewStore(dm)
	auth := assignment.NewLocalOperatorAuthority("operator")

	multiline := "line one\nline two\r\nline three"
	root, _, err := store.CreateRoot(auth, assignment.CreateRootParams{
		ProjectID: "cpj-rttypes0000001",
		Title:     "007",
		Objective: multiline,
		Recipient: "00123",
		// The allow-list carries numeric-looking names too.
		EligibleRecipients:  []string{"00123", "-0", "1e5"},
		IdempotencyKey:      "idem:types:root",
		PermittedOperations: []string{"read"},
		MaxDepth:            2, MaxTasks: 8, MaxConcurrency: 1, MaxResourceUnits: 100,
	})
	require.NoError(t, err)
	_ = root

	// A direct row pins the awkward literals the service does not take
	// as arguments: a long digit string, a quote, an empty string and a
	// NULL-versus-empty pair in two different columns.
	long := strings.Repeat("0123456789", 6)
	_, err = dm.SQLDB().Exec(`
		INSERT INTO agent_tasks
			(id, project_id, root_id, parent_id, depth, title, objective, recipient,
			 role, state, idempotency_key, resource_units, issue_ref, pr_ref,
			 permitted_operations, eligible_recipients)
		VALUES ('at-typed00000001','cpj-rttypes0000001',?,?,1,
			?,?,?,'implementer','enqueued','idem:types:typed',0,NULL,'', '[]','[]')`,
		root.ID, root.ID, long, "it's a value with \"quotes\" and unicode ✓", "1e5")
	require.NoError(t, err)
	// NULL in issue_ref, empty string in pr_ref: distinct values.
	var issueNull, prEmpty sql.NullString
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT issue_ref, pr_ref FROM agent_tasks WHERE id='at-typed00000001'`).
		Scan(&issueNull, &prEmpty))
	require.False(t, issueNull.Valid, "the fixture must carry a NULL, not an empty string")
	require.True(t, prEmpty.Valid)
	require.Equal(t, "", prEmpty.String)

	_, dump := realBackup(t, dm, "types.sql")
	before := openRestored(t, dbPath)
	require.Equal(t, 0, runRestoreDB(t, dm, dump), "the typed dump must restore")
	after := openRestored(t, dbPath)

	assertTableUnchanged(t, before, after, "agent_tasks")

	// The headline assertions, named so a failure is unambiguous.
	for _, c := range []struct{ id, col, want string }{
		{"at-typed00000001", "title", long},
		{"at-typed00000001", "objective", "it's a value with \"quotes\" and unicode ✓"},
		{"at-typed00000001", "recipient", "1e5"},
		{root.ID, "title", "007"},
		{root.ID, "objective", multiline},
		{root.ID, "recipient", "00123"},
	} {
		var got, kind string
		require.NoError(t, after.QueryRow(
			`SELECT quote(`+c.col+`), typeof(`+c.col+`) FROM agent_tasks WHERE id=?`, c.id).
			Scan(&got, &kind))
		require.Equal(t, "text", kind, "%s.%s must stay TEXT", c.id, c.col)
		require.Equal(t, "'"+strings.ReplaceAll(c.want, "'", "''")+"'", got,
			"%s.%s must keep its value", c.id, c.col)
	}
	var issueKind string
	require.NoError(t, after.QueryRow(
		`SELECT typeof(issue_ref) FROM agent_tasks WHERE id='at-typed00000001'`).Scan(&issueKind))
	require.Equal(t, "null", issueKind, "NULL must survive as NULL")
	var prKind string
	require.NoError(t, after.QueryRow(
		`SELECT typeof(pr_ref) FROM agent_tasks WHERE id='at-typed00000001'`).Scan(&prKind))
	require.Equal(t, "text", prKind, "an empty string must survive as TEXT")
}

// TestRestoreRefusesInsteadOfCorruptingOnUnknownLiteralIsF1 keeps the
// fail-closed boundary intact after the passthrough change: a MANAGED
// row with a literal the parser does not recognise is refused, and the
// original database is untouched.
func TestRestoreRefusesInsteadOfCorruptingOnUnknownLiteralIsF1(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	dump := buildRecoveryDump(t, dm.SQLDB())
	// A managed row cannot carry an arbitrary expression, so replace a
	// value with one the closed grammar must refuse.
	dump = strings.Replace(dump,
		"'idem:rt:child'", "abs(-1)", 1)
	restoreMarker(t, dm.SQLDB(), taskID)
	assertRestoreRefusedAndIntact(t, dm, dump, "unsupported literal")
}
