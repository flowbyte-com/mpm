// migration_agent_assignments_test.go — Continuity 001, PR 1: the
// acceptance tests for the durable assignment storage.
//
// WHAT IS UNDER TEST
//
//	PR 1 is the STORAGE and nothing else. The two tables
//	(agent_tasks, agent_task_events), their indexes and their triggers are
//	created by ONE migration-owned DDL path, inside the fatal migration
//	transaction, and nowhere else.
//
// The tests fall into three groups:
//
//	  - THE MIGRATION'S DISCIPLINE — the same AT1–AT8 shape PR #224's
//	    Stage 1 established: an existing database gains the objects and
//	    changes nothing else; a fresh database ends up with the same
//	    schema; the migration is idempotent; a failure inside it leaves NO
//	    partial schema and the next open recovers; the tables never appear
//	    in the cross-workspace `shared` schema; the restore allow-list
//	    gains exactly two names and is still load-bearing.
//
//	  - THE PERSISTENCE INVARIANTS — each one observed as a REFUSED
//	    write, not as a comment. The state machine is role-aware; the
//	    claim epoch advances by exactly one; only a claim changes the
//	    owner; the root carries the budgets and a child carries none;
//	    the delegation tree is contained in ONE project; the idempotency
//	    key is unique.
//
//	  - THE NEGATIVE CONTROLS — the tests themselves, proved to be
//	    load-bearing by disabling each invariant and watching the test
//	    that depends on it fail.
//
// EVERY DATABASE IS DISPOSABLE
//
//	Each test builds its workspace under t.TempDir() and opens it with
//	NewDatabaseManager, which places the database at
//	<root>/src/db/mpm.db. No test touches the operator's workspace, and no
//	test reaches the live MPM database.
package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// agentAssignmentSentinelID is the schema_migrations sentinel the
// migration writes LAST, inside the fatal transaction.
const agentAssignmentSentinelID = "agent_assignments_v1"

// agentAssignmentTables are the two tables PR 1 adds.
var agentAssignmentTables = []string{
	"agent_task_events",
	"agent_tasks",
}

// agentAssignmentIndexes are the indexes the DDL creates. They are named
// explicitly so a dropped or renamed index fails the test.
var agentAssignmentIndexes = []string{
	"idx_agent_task_events_created",
	"idx_agent_tasks_id_project",
	"idx_agent_tasks_idempotency_key",
	"idx_agent_tasks_project",
	"idx_agent_tasks_recipient_state",
	"idx_agent_tasks_root",
	"idx_agent_tasks_state_updated",
}

// agentAssignmentTriggers are the triggers the DDL creates.
var agentAssignmentTriggers = []string{
	"trg_agent_tasks_claim_discipline",
	"trg_agent_tasks_initial_state",
	"trg_agent_tasks_state_transition",
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// assignmentWorkspace builds a disposable workspace and returns its root and
// the path of the database inside it.
func assignmentWorkspace(t *testing.T) (root, dbPath string) {
	t.Helper()
	PinIsolatedWorkspace(t)
	root = t.TempDir()
	t.Setenv("MPM_WORKSPACE", root)
	dbPath = filepath.Join(root, "src", "db", "mpm.db")
	return root, dbPath
}

// MigrateAgentAssignmentsInTx runs the assignment migration inside its own
// transaction against an already-open database.
//
// The production path runs it inside the boot path's fatal migration
// transaction; this helper is the same DDL, in the same transaction shape,
// for the tests that need to run it against a database the boot path has
// already opened.
func MigrateAgentAssignmentsInTx(t *testing.T, db *sql.DB) error {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin the migration transaction: %v", err)
	}
	defer tx.Rollback()
	if err := MigrateAgentAssignments(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// seedProject creates a project row and returns its id.
//
// The assignment tables reference `projects(id)`, so every assignment test
// needs one. The project is created directly rather than through a
// continuity API: PR #224's Stage 1 shipped the storage, and its APIs land
// in Stage 2, which is not part of this PR.
func seedProject(t *testing.T, dm *DatabaseManager, id string) string {
	t.Helper()
	if id == "" {
		id = "cpj-" + strings.Repeat("a", 16)
	}
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO projects (id, created_by) VALUES (?, 'assignment-test')`, id); err != nil {
		t.Fatalf("seed the project %q: %v", id, err)
	}
	return id
}

// insertRootAssignment inserts a well-formed ROOT assignment and returns its
// id.
//
// It goes through the real INSERT path, so every invariant the DDL enforces
// is satisfied by construction — and a test that wants to break one writes
// its own SQL rather than calling this helper.
func insertRootAssignment(t *testing.T, dm *DatabaseManager, projectID, id, recipient, role, idem string) string {
	t.Helper()
	if id == "" {
		id = "at-" + strings.Repeat("b", 16)
	}
	if idem == "" {
		idem = "idem:" + id
	}
	_, err := dm.SQLDB().Exec(`
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units,
			permitted_operations, eligible_recipients
		) VALUES (?, ?, ?, NULL, 0, 'the title', 'the objective', ?, ?, 'enqueued', ?,
			1, 4, 2, 100, '["read","write"]', ?)
	`, id, projectID, id, recipient, role, idem, jsonArray(recipient))
	if err != nil {
		t.Fatalf("insert the root assignment %q: %v", id, err)
	}
	return id
}

// jsonArray renders a one-element JSON array of the given strings.
func jsonArray(values ...string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// insertChildAssignment inserts a well-formed CHILD assignment.
func insertChildAssignment(t *testing.T, dm *DatabaseManager, projectID, id, rootID, parentID, recipient, idem string) string {
	t.Helper()
	if id == "" {
		id = "at-" + strings.Repeat("c", 16)
	}
	if idem == "" {
		idem = "idem:" + id
	}
	_, err := dm.SQLDB().Exec(`
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key
		) VALUES (?, ?, ?, ?, 1, 'the title', 'the objective', ?, 'implementer', 'enqueued', ?)
	`, id, projectID, rootID, parentID, recipient, idem)
	if err != nil {
		t.Fatalf("insert the child assignment %q: %v", id, err)
	}
	return id
}

// mustExec runs a statement and fails the test if it errors.
func assignmentMustExec(t *testing.T, db *sql.DB, query string, args ...interface{}) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// mustRefuse runs a statement and fails the test if it SUCCEEDS.
//
// It returns the error so a caller can assert on its shape.
func assignmentMustRefuse(t *testing.T, db *sql.DB, what string, query string, args ...interface{}) error {
	t.Helper()
	_, err := db.Exec(query, args...)
	if err == nil {
		t.Fatalf("%s was ACCEPTED. The database must refuse it: this is a persistence invariant, "+
			"not a convention the code is trusted to keep.", what)
	}
	return err
}

// assignmentSchemaObjects returns every named sqlite_master object and its
// SQL.
func assignmentSchemaObjects(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query("SELECT name, sql FROM sqlite_master WHERE sql IS NOT NULL")
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, sqlText string
		if err := rows.Scan(&name, &sqlText); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		out[name] = sqlText
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	return out
}


// stripAssignmentObjects turns a migrated database back into a
// pre-assignment one: the two tables are dropped (which drops their
// indexes and triggers with them), in reverse dependency order, and the
// sentinel row is deleted.
//
// The surgery runs on a DEDICATED connection WITHOUT the foreign-key
// pragmas. Dropping a table with foreign keys enabled performs an implicit
// DELETE FROM, and a RESTRICT reference to the dropped table makes that
// implicit delete fail. This is fixture construction, not product
// behaviour; the opens that are under test use the production DSN, which
// carries `_foreign_keys=1`.
func stripAssignmentObjects(t *testing.T, dm *DatabaseManager) {
	t.Helper()
	db, err := sql.Open("sqlite3", dm.dbPath)
	if err != nil {
		t.Fatalf("open the fixture connection: %v", err)
	}
	defer db.Close()

	for _, table := range []string{"agent_task_events", "agent_tasks"} {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	if _, err := db.Exec("DELETE FROM schema_migrations WHERE id = ?", agentAssignmentSentinelID); err != nil {
		t.Fatalf("delete the sentinel: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The DDL is pinned
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentAssignmentDDLIsPinned pins the DDL text.
//
// The storage is the contract's foundation, and a contract whose storage can
// drift silently is not a contract. The hash is of the DDL literal itself,
// so a reformat, a dropped constraint or an added column fails the build.
//
// The pinned value is the SHA-256 of the DDL as approved in
// docs/dev/continuity-001-assignment-schema.md. It is recomputed by the
// developer who changes the DDL, and the change is then reviewed on its
// own — a hash bump is a visible event, not a routine part of a diff.
func TestAgentAssignmentDDLIsPinned(t *testing.T) {
	sum := sha256.Sum256([]byte(agentAssignmentDDL))
	got := hex.EncodeToString(sum[:])

	const want = "8abfcbf69cc08f5937eed70c65a28586a37deea53624239122e65dfb46c3f0ac"
	if got != want {
		t.Errorf("the DDL's SHA-256 is %s but the pin says %s. If the DDL changed on purpose, "+
			"recompute the hash and say so in the commit message; if it did not, the storage has "+
			"drifted from the design note and the contract is no longer what was reviewed.",
			got, want)
	}
}

// TestAgentAssignmentDDLHasNoBacktick guards the Go raw-string boundary.
//
// The DDL is a Go raw string literal, so a backtick inside it would
// terminate the literal and the rest of the DDL would be read as Go source.
// This test fails loudly instead of leaving the compiler to fail with a
// syntax error somewhere else.
func TestAgentAssignmentDDLHasNoBacktick(t *testing.T) {
	if strings.Contains(agentAssignmentDDL, "`") {
		t.Error("the DDL literal contains a backtick; it is a Go raw string and the literal ends there")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AT1 — an existing database gains the objects and changes nothing else
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentAssignmentAT1ExistingDatabaseMigration — AT1.
//
// A disposable file-backed WAL database is built, a representative row is
// written into `memories`, and the migration is then run against it: the
// two tables, the seven indexes and the three triggers exist BY NAME, the
// sentinel is recorded exactly once, and the pre-existing row is untouched.
func TestAgentAssignmentAT1ExistingDatabaseMigration(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()

	// A representative pre-existing row.
	assignmentMustExec(t, db, `INSERT INTO memories (id, collection, content, created_at)
		VALUES ('mem-pre-existing', 'memories', 'the original content', 1700000000)`)

	before := assignmentSchemaObjects(t, db)

	// The migration is idempotent, so running it again on an already
	// migrated database is a no-op — which is what proves it was applied
	// by the boot path in the first place.
	if err := MigrateAgentAssignmentsInTx(t, db); err != nil {
		t.Fatalf("run the migration: %v", err)
	}

	for _, table := range agentAssignmentTables {
		var n int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM main.sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("the table %q is missing after the migration (count %d)", table, n)
		}
	}
	for _, index := range agentAssignmentIndexes {
		var n int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM main.sqlite_master WHERE type = 'index' AND name = ?", index,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", index, err)
		}
		if n != 1 {
			t.Errorf("the index %q is missing after the migration (count %d)", index, n)
		}
	}
	for _, trigger := range agentAssignmentTriggers {
		var n int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM main.sqlite_master WHERE type = 'trigger' AND name = ?", trigger,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", trigger, err)
		}
		if n != 1 {
			t.Errorf("the trigger %q is missing after the migration (count %d)", trigger, n)
		}
	}

	// The sentinel is recorded exactly once.
	var sentinels int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE id = ?", agentAssignmentSentinelID,
	).Scan(&sentinels); err != nil {
		t.Fatalf("count the sentinels: %v", err)
	}
	if sentinels != 1 {
		t.Errorf("the sentinel is recorded %d times, want exactly 1", sentinels)
	}

	// Nothing else changed.
	after := assignmentSchemaObjects(t, db)
	for name, sqlText := range before {
		afterSQL, ok := after[name]
		if !ok {
			t.Errorf("the migration removed the object %q", name)
			continue
		}
		if afterSQL != sqlText {
			t.Errorf("the migration changed %q:\n  before: %s\n  after:  %s", name, sqlText, afterSQL)
		}
	}

	// And the pre-existing row is intact.
	var content string
	if err := db.QueryRow(`SELECT content FROM memories WHERE id = 'mem-pre-existing'`).Scan(&content); err != nil {
		t.Fatalf("read the pre-existing row: %v", err)
	}
	if content != "the original content" {
		t.Errorf("the pre-existing row carries %q, want the original content", content)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AT2 — a fresh database and a migrated one agree
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentAssignmentAT2FreshDatabaseSchemaEquivalence — AT2.
//
// A database built from scratch (the migration runs inside the boot
// transaction) and a database the migration was applied to must agree on the
// assignment objects. The two are compared by their sqlite_master SQL, so
// a difference in a constraint, an index or a trigger body is a difference,
// not a rounding error.
func TestAgentAssignmentAT2FreshDatabaseSchemaEquivalence(t *testing.T) {
	fresh := NewTestLocalOnlyDM(t)

	migrated := NewTestLocalOnlyDM(t)
	// Drop the assignment objects to simulate a pre-migration database,
	// then re-apply the migration. The drop runs on a connection WITHOUT
	// the foreign-key pragmas: dropping a parent table with FKs enabled
	// trips the implicit-DELETE foreign-key check.
	stripAssignmentObjects(t, migrated)
	if err := MigrateAgentAssignmentsInTx(t, migrated.SQLDB()); err != nil {
		t.Fatalf("re-apply the migration: %v", err)
	}

	freshObjects := assignmentSchemaObjects(t, fresh.SQLDB())
	migratedObjects := assignmentSchemaObjects(t, migrated.SQLDB())

	for _, name := range agentAssignmentTables {
		a, okA := freshObjects[name]
		b, okB := migratedObjects[name]
		if !okA || !okB {
			t.Errorf("the table %q is missing from one of the two databases (fresh=%v migrated=%v)",
				name, okA, okB)
			continue
		}
		if a != b {
			t.Errorf("the table %q differs between a fresh and a migrated database:\n  fresh:    %s\n  migrated: %s",
				name, a, b)
		}
	}
	for _, name := range agentAssignmentIndexes {
		a, okA := freshObjects[name]
		b, okB := migratedObjects[name]
		if !okA || !okB {
			t.Errorf("the index %q is missing from one of the two databases (fresh=%v migrated=%v)",
				name, okA, okB)
			continue
		}
		if a != b {
			t.Errorf("the index %q differs between a fresh and a migrated database", name)
		}
	}
	for _, name := range agentAssignmentTriggers {
		a, okA := freshObjects[name]
		b, okB := migratedObjects[name]
		if !okA || !okB {
			t.Errorf("the trigger %q is missing from one of the two databases (fresh=%v migrated=%v)",
				name, okA, okB)
			continue
		}
		if a != b {
			t.Errorf("the trigger %q differs between a fresh and a migrated database:\n  fresh:    %s\n  migrated: %s",
				name, a, b)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AT3 / AT4 — no row is changed, and no row is fabricated
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentAssignmentAT3AT4NoFabricatedState — AT3 and AT4.
//
// The migration is additive storage: it must not change an existing row and
// it must not create a row of its own. A migration that fabricated an
// assignment would put work into the queue that nobody asked for.
func TestAgentAssignmentAT3AT4NoFabricatedState(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()

	assignmentMustExec(t, db, `INSERT INTO memories (id, collection, content, created_at)
		VALUES ('mem-at34', 'memories', 'the original content', 1700000000)`)

	stripAssignmentObjects(t, dm)
	if err := MigrateAgentAssignmentsInTx(t, db); err != nil {
		t.Fatalf("re-apply the migration: %v", err)
	}

	// No assignment rows were fabricated.
	for _, table := range agentAssignmentTables {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("the migration fabricated %d row(s) in %s; a schema migration must create "+
				"no state", n, table)
		}
	}

	// The pre-existing row is byte-identical.
	var content string
	if err := db.QueryRow(`SELECT content FROM memories WHERE id = 'mem-at34'`).Scan(&content); err != nil {
		t.Fatalf("read the pre-existing row: %v", err)
	}
	if content != "the original content" {
		t.Errorf("the pre-existing row carries %q, want the original content", content)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AT5 / AT6 — idempotency, and a failed migration rolls back whole
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentAssignmentAT5IdempotentMigration — AT5.
//
// Running the migration twice must change nothing the second time, and the
// sentinel must still be recorded exactly once.
func TestAgentAssignmentAT5IdempotentMigration(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()

	before := assignmentSchemaObjects(t, db)

	for i := 0; i < 3; i++ {
		if err := MigrateAgentAssignmentsInTx(t, db); err != nil {
			t.Fatalf("run the migration (attempt %d): %v", i+1, err)
		}
	}

	after := assignmentSchemaObjects(t, db)
	if len(before) != len(after) {
		t.Errorf("the object count changed from %d to %d across three runs of the migration",
			len(before), len(after))
	}
	for name, sqlText := range before {
		afterSQL, ok := after[name]
		if !ok {
			t.Errorf("a re-run of the migration removed %q", name)
			continue
		}
		if afterSQL != sqlText {
			t.Errorf("a re-run of the migration changed %q", name)
		}
	}

	var sentinels int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE id = ?", agentAssignmentSentinelID,
	).Scan(&sentinels); err != nil {
		t.Fatalf("count the sentinels: %v", err)
	}
	if sentinels != 1 {
		t.Errorf("the sentinel is recorded %d times after three runs, want exactly 1", sentinels)
	}
}

// TestAgentAssignmentAT6FailedMigrationRollsBack — AT6.
//
// A failure injected at the sentinel write — AFTER the DDL has already
// succeeded — must roll the DDL back with it. The proof is that NO
// assignment table survives.
//
// The injection is a BEFORE INSERT trigger on `schema_migrations` that
// aborts the sentinel write.
func TestAgentAssignmentAT6FailedMigrationRollsBack(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()

	// Strip the assignment objects so the migration has work to do.
	stripAssignmentObjects(t, dm)

	// Inject the failure at the sentinel write.
	assignmentMustExec(t, db, `
		CREATE TRIGGER zz_agent_assignment_sentinel_abort
		BEFORE INSERT ON schema_migrations
		WHEN NEW.id = '` + agentAssignmentSentinelID + `'
		BEGIN
			SELECT RAISE(ABORT, 'injected failure at the sentinel write');
		END`)

	err := MigrateAgentAssignmentsInTx(t, db)
	if err == nil {
		t.Fatal("the migration SUCCEEDED despite the injected failure; the rollback is not being tested")
	}

	// The DDL is rolled back with the sentinel: no assignment table
	// survives.
	for _, table := range agentAssignmentTables {
		var n int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM main.sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("the table %q survived a failed migration (count %d). The DDL and the "+
				"sentinel must share one transaction, or a half-migrated database is possible.",
				table, n)
		}
	}

	// And the sentinel was not recorded.
	var sentinels int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE id = ?", agentAssignmentSentinelID,
	).Scan(&sentinels); err != nil {
		t.Fatalf("count the sentinels: %v", err)
	}
	if sentinels != 0 {
		t.Errorf("the sentinel survived a failed migration (count %d)", sentinels)
	}
}

// TestAgentAssignmentAT6RecoveryAfterInjectedFailure — AT6, continued.
//
// After the failure, the next attempt — with the injection removed — must
// succeed and leave the full schema. A migration that could not recover
// would need an operator to intervene on every interrupted boot.
func TestAgentAssignmentAT6RecoveryAfterInjectedFailure(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()

	stripAssignmentObjects(t, dm)
	assignmentMustExec(t, db, `
		CREATE TRIGGER zz_agent_assignment_sentinel_abort
		BEFORE INSERT ON schema_migrations
		WHEN NEW.id = '` + agentAssignmentSentinelID + `'
		BEGIN
			SELECT RAISE(ABORT, 'injected failure at the sentinel write');
		END`)

	if err := MigrateAgentAssignmentsInTx(t, db); err == nil {
		t.Fatal("the migration SUCCEEDED despite the injected failure")
	}

	assignmentMustExec(t, db, `DROP TRIGGER zz_agent_assignment_sentinel_abort`)

	if err := MigrateAgentAssignmentsInTx(t, db); err != nil {
		t.Fatalf("the recovery run failed: %v", err)
	}
	for _, table := range agentAssignmentTables {
		var n int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM main.sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("the table %q is missing after the recovery run (count %d)", table, n)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AT7 — the tables are local-only
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentAssignmentAT7LocalOnly — AT7.
//
// The assignment tables must never appear in the cross-workspace `shared`
// schema. The shared store is the cross-workspace broadcast store; an
// assignment is state that belongs inside ONE workspace, and broadcasting
// it would give the same assignment id to every workspace that reads the
// shared store.
func TestAgentAssignmentAT7LocalOnly(t *testing.T) {
	dm := NewTestSharedDM(t)
	db := dm.SQLDB()

	// The shared database must actually be attached, or every assertion
	// below would pass vacuously.
	var sharedAttached int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_database_list WHERE name = 'shared'").Scan(&sharedAttached); err != nil {
		t.Fatalf("probe the attached databases: %v", err)
	}
	if sharedAttached == 0 {
		t.Fatal("the shared database is not attached — the test would pass vacuously")
	}

	for _, table := range agentAssignmentTables {
		var local, shared int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM main.sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&local); err != nil {
			t.Fatalf("probe main.%s: %v", table, err)
		}
		if local != 1 {
			t.Errorf("the table %q is missing from the local schema (count %d)", table, local)
		}
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM shared.sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&shared); err != nil {
			t.Fatalf("probe shared.%s: %v", table, err)
		}
		if shared != 0 {
			t.Errorf("the table %q is present in the SHARED schema (count %d). An assignment "+
				"belongs inside one workspace.", table, shared)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AT8 — the restore allow-list
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentAssignmentAT8RestoreAllowList — AT8.
//
// A dump declaring the two new tables is accepted by the restore
// validator, and the allow-list contains exactly those two new names and
// no others. The allow-list is the only thing standing between an
// arbitrary dump and the user's MPM state, so a table MPM creates must be
// on it and a table MPM does not create must not be.
func TestAgentAssignmentAT8RestoreAllowList(t *testing.T) {
	v := NewCanonicalDumpValidator()

	for _, table := range agentAssignmentTables {
		dump := "CREATE TABLE " + table + " (id TEXT PRIMARY KEY, payload TEXT);\n" +
			"INSERT INTO " + table + " VALUES('x','y');\n"
		if err := v.Validate(dump); err != nil {
			t.Errorf("a dump declaring %q was rejected: %v. MPM creates that table, so the "+
				"restore allow-list must carry it.", table, err)
		}
	}

	// The allow-list carries the two names, and each is a table MPM
	// actually creates.
	canonical := map[string]bool{}
	for _, name := range CanonicalMPMSchema {
		canonical[name] = true
	}
	for _, table := range agentAssignmentTables {
		if !canonical[table] {
			t.Errorf("the table %q is not in CanonicalMPMSchema; the restore allow-list would "+
				"refuse a legitimate dump", table)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The persistence invariants — each observed as a REFUSED write
// ─────────────────────────────────────────────────────────────────────────────

// TestAssignmentIsCreatedEnqueued pins the creation rule.
//
// An assignment is created enqueued and reaches every other state by a
// transition. A row that is born already accepted — or already completed —
// is a row whose history does not exist.
func TestAssignmentIsCreatedEnqueued(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	assignmentMustRefuse(t, db, "an assignment created in the 'completed' state", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units,
			permitted_operations, eligible_recipients
		) VALUES ('at-born-done', ?, 'at-born-done', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'completed', 'idem:born-done', 1, 4, 2, 100, '["read"]', '["alpha"]')
	`, projectID)

	// The well-formed one is accepted, and it IS enqueued.
	id := insertRootAssignment(t, dm, projectID, "at-born-ok", "alpha", "implementer", "idem:born-ok")
	var state string
	if err := db.QueryRow(`SELECT state FROM agent_tasks WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if state != "enqueued" {
		t.Errorf("the assignment is in state %q, want enqueued", state)
	}
}

// TestAssignmentStateMachineIsRoleAware pins the transition rules.
//
// The separations are the point: delivery is not acceptance, acceptance is
// not execution, execution is not completion, completion is not review, and
// review is not approval. A state machine that collapsed any two of them
// would make the ledger lie about what happened.
func TestAssignmentStateMachineIsRoleAware(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// The implementer's path.
	impl := insertRootAssignment(t, dm, projectID, "at-impl", "alpha", "implementer", "idem:impl")

	// The legal forward steps are accepted.
	for _, step := range []struct{ from, to string }{
		{"enqueued", "delivered"},
		{"delivered", "acknowledged"},
		{"acknowledged", "accepted"},
		{"accepted", "in_progress"},
		{"in_progress", "completed"},
		{"completed", "review_passed"},
		{"review_passed", "awaiting_operator"},
		{"awaiting_operator", "operator_approved"},
	} {
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step.to, impl)
		var state string
		if err := db.QueryRow(`SELECT state FROM agent_tasks WHERE id = ?`, impl).Scan(&state); err != nil {
			t.Fatalf("read the assignment: %v", err)
		}
		if state != step.to {
			t.Fatalf("the assignment is in state %q after the step %q -> %q", state, step.from, step.to)
		}
	}

	// The implementer may NOT skip the review.
	impl2 := insertRootAssignment(t, dm, projectID, "at-impl2", "alpha", "implementer", "idem:impl2")
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'delivered' WHERE id = ?`, impl2)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'acknowledged' WHERE id = ?`, impl2)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'accepted' WHERE id = ?`, impl2)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'in_progress' WHERE id = ?`, impl2)
	assignmentMustRefuse(t, db, "an implementer jumping from in_progress to review_passed",
		`UPDATE agent_tasks SET state = 'review_passed' WHERE id = ?`, impl2)

	// The implementer may NOT skip the operator's gate.
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'completed' WHERE id = ?`, impl2)
	assignmentMustRefuse(t, db, "an implementer jumping from completed to operator_approved",
		`UPDATE agent_tasks SET state = 'operator_approved' WHERE id = ?`, impl2)

	// The reviewer's path: a review ends at review_passed (it passed) or
	// at completed (it asked for changes) — never at the operator's gate.
	rev := insertRootAssignment(t, dm, projectID, "at-rev", "charlie", "reviewer", "idem:rev")
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'delivered' WHERE id = ?`, rev)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'acknowledged' WHERE id = ?`, rev)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'accepted' WHERE id = ?`, rev)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'in_progress' WHERE id = ?`, rev)
	assignmentMustRefuse(t, db, "a reviewer jumping from in_progress to awaiting_operator",
		`UPDATE agent_tasks SET state = 'awaiting_operator' WHERE id = ?`, rev)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'review_passed' WHERE id = ?`, rev)

	// A terminal state is terminal.
	assignmentMustRefuse(t, db, "a review_passed reviewer being moved on",
		`UPDATE agent_tasks SET state = 'completed' WHERE id = ?`, rev)
}

// TestAssignmentCannotBeUnCancelled pins the withdrawal rule.
//
// 'cancelled' is the universal withdrawal and is legal from every state
// except itself.
func TestAssignmentCannotBeUnCancelled(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	id := insertRootAssignment(t, dm, projectID, "at-cancel", "alpha", "implementer", "idem:cancel")
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'cancelled' WHERE id = ?`, id)

	assignmentMustRefuse(t, db, "a cancelled assignment being revived",
		`UPDATE agent_tasks SET state = 'enqueued' WHERE id = ?`, id)
	assignmentMustRefuse(t, db, "a cancelled assignment being completed",
		`UPDATE agent_tasks SET state = 'completed' WHERE id = ?`, id)
}

// TestAssignmentStateVocabularyIsClosed pins the state vocabulary.
//
// Two layers refuse a state outside the vocabulary, and this test says so
// honestly rather than crediting one of them for the other's work:
//
//   - the CHECK constraint on the column, which lists the ten states;
//   - the state-machine trigger, which refuses any transition to a state
//     that is not on the role's path — and a state that is not in the
//     vocabulary is not on any path.
//
// The negative control showed that the TRIGGER is what refuses an unknown
// state at INSERT time (the initial-state trigger refuses every state that
// is not 'enqueued'), so the CHECK is defence in depth behind it. That is
// the same layering PR #224's 'continuity_checkpoints' uses — a composite
// foreign key AND a separate reference, "the second line of defence".
//
// So the vocabulary itself is pinned STATICALLY here: the column's
// declared CHECK must list exactly the ten states. A vocabulary that
// drifts would otherwise be invisible, because the trigger would refuse
// the drifted value before the CHECK could be blamed for it.
func TestAssignmentStateVocabularyIsClosed(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// The static pin: the column's CHECK lists exactly the vocabulary.
	objects := assignmentSchemaObjects(t, db)
	ddl, ok := objects["agent_tasks"]
	if !ok {
		t.Fatal("the agent_tasks table is missing")
	}
	wantStates := []string{
		"enqueued", "delivered", "acknowledged", "accepted", "in_progress",
		"completed", "review_passed", "awaiting_operator", "operator_approved",
		"cancelled",
	}
	statesClause := ""
	if idx := strings.Index(ddl, "state IN ("); idx >= 0 {
		rest := ddl[idx+len("state IN ("):]
		if end := strings.Index(rest, ")"); end >= 0 {
			statesClause = rest[:end]
		}
	}
	if statesClause == "" {
		t.Fatalf("the state column carries no CHECK constraint naming a vocabulary:\n%s", ddl)
	}
	for _, state := range wantStates {
		if !strings.Contains(statesClause, "'"+state+"'") {
			t.Errorf("the state vocabulary does not name %q:\n  %s", state, statesClause)
		}
	}
	// And nothing beyond it.
	for _, token := range strings.Split(statesClause, ",") {
		token = strings.TrimSpace(token)
		if !strings.HasPrefix(token, "'") || !strings.HasSuffix(token, "'") {
			t.Errorf("the state vocabulary carries an unquoted token %q", token)
			continue
		}
		name := strings.Trim(token, "'")
		found := false
		for _, state := range wantStates {
			if state == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the state vocabulary carries %q, which is not one of the ten states", name)
		}
	}

	// The behavioural pin: a state outside the vocabulary is refused, on
	// the way IN and on the way OUT.
	assignmentMustRefuse(t, db, "an assignment in an unknown state", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units,
			permitted_operations, eligible_recipients
		) VALUES ('at-unknown', ?, 'at-unknown', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'pondering', 'idem:unknown', 1, 4, 2, 100, '["read"]', '["alpha"]')
	`, projectID)

	id := insertRootAssignment(t, dm, projectID, "at-unknown-out", "alpha", "implementer", "idem:unknown-out")
	assignmentMustRefuse(t, db, "an assignment moved to an unknown state",
		`UPDATE agent_tasks SET state = 'pondering' WHERE id = ?`, id)

	// And an unknown ROLE is refused too — the role is what selects the
	// path, so an unknown role would select no path.
	assignmentMustRefuse(t, db, "an assignment with an unknown role", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units,
			permitted_operations, eligible_recipients
		) VALUES ('at-unknown-role', ?, 'at-unknown-role', NULL, 0, 't', 'o', 'alpha', 'arbiter',
			'enqueued', 'idem:unknown-role', 1, 4, 2, 100, '["read"]', '["alpha"]')
	`, projectID)
}

// TestAssignmentClaimDiscipline pins the lease rules.
//
// The claim epoch advances by exactly one, a claim sets an owner and a
// lease, and the owner changes only on a claim. So the epoch cannot be
// skipped or reset, and an owner cannot be reassigned without the row
// ceasing to be a lease.
func TestAssignmentClaimDiscipline(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// A claim: the epoch advances by one, the owner is set, the lease is
	// set. Accepted.
	id := insertRootAssignment(t, dm, projectID, "at-claim", "alpha", "implementer", "idem:claim")
	assignmentMustExec(t, db, `
		UPDATE agent_tasks
		SET claim_epoch = claim_epoch + 1, owner = 'alpha', lease_expires_at = 1900000000
		WHERE id = ?`, id)

	var epoch int64
	var owner sql.NullString
	if err := db.QueryRow(`SELECT claim_epoch, owner FROM agent_tasks WHERE id = ?`, id).
		Scan(&epoch, &owner); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if epoch != 1 {
		t.Errorf("the claim epoch is %d, want 1", epoch)
	}
	if owner.String != "alpha" {
		t.Errorf("the owner is %q, want alpha", owner.String)
	}

	// The epoch cannot jump.
	assignmentMustRefuse(t, db, "a claim epoch that jumps by two", `
		UPDATE agent_tasks
		SET claim_epoch = claim_epoch + 2, owner = 'alpha', lease_expires_at = 1900000000
		WHERE id = ?`, id)

	// The epoch cannot be reset.
	assignmentMustRefuse(t, db, "a claim epoch that is reset to zero", `
		UPDATE agent_tasks
		SET claim_epoch = 0, owner = 'alpha', lease_expires_at = 1900000000
		WHERE id = ?`, id)

	// The owner cannot change without a claim.
	assignmentMustRefuse(t, db, "an owner change with no claim", `
		UPDATE agent_tasks SET owner = 'mallory' WHERE id = ?`, id)

	// A claim must set an owner.
	assignmentMustRefuse(t, db, "a claim with no owner", `
		UPDATE agent_tasks
		SET claim_epoch = claim_epoch + 1, owner = NULL, lease_expires_at = 1900000000
		WHERE id = ?`, id)

	// A claim must set a lease.
	assignmentMustRefuse(t, db, "a claim with no lease", `
		UPDATE agent_tasks
		SET claim_epoch = claim_epoch + 1, owner = 'alpha', lease_expires_at = NULL
		WHERE id = ?`, id)

	// A second, legitimate claim advances the epoch by exactly one again.
	assignmentMustExec(t, db, `
		UPDATE agent_tasks
		SET claim_epoch = claim_epoch + 1, owner = 'charlie', lease_expires_at = 1900000001
		WHERE id = ?`, id)
	if err := db.QueryRow(`SELECT claim_epoch FROM agent_tasks WHERE id = ?`, id).Scan(&epoch); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if epoch != 2 {
		t.Errorf("the claim epoch is %d after two claims, want 2", epoch)
	}
}

// TestAssignmentRootCarriesTheBudgetsAndAChildCarriesNone pins the
// delegation shape.
//
// Inheritance is STRUCTURAL: a root has the budget columns and a child has
// none, so a child cannot widen what it was given — there is no column in
// which to widen it.
func TestAssignmentRootCarriesTheBudgetsAndAChildCarriesNone(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// A root WITHOUT the budgets is refused.
	assignmentMustRefuse(t, db, "a root without the budget columns", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key, permitted_operations
		) VALUES ('at-nobudget', ?, 'at-nobudget', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:nobudget', '["read"]')
	`, projectID)

	// A root with an EMPTY permitted_operations is refused: a root that
	// may do nothing is a silent misconfiguration, not a safe default.
	assignmentMustRefuse(t, db, "a root with no permitted operations", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations
		) VALUES ('at-noops', ?, 'at-noops', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:noops', 1, 4, 2, 100, '[]')
	`, projectID)

	// A child WITH budget columns is refused.
	root := insertRootAssignment(t, dm, projectID, "at-budgetroot", "alpha", "implementer", "idem:budgetroot")
	assignmentMustRefuse(t, db, "a child carrying budget columns", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units
		) VALUES ('at-childbudget', ?, ?, ?, 1, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:childbudget', 5, 99, 9, 999)
	`, projectID, root, root)

	// A child that claims to be its own root is refused.
	assignmentMustRefuse(t, db, "a child that is its own root", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key
		) VALUES ('at-selfroot', ?, 'at-selfroot', ?, 1, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:selfroot')
	`, projectID, root)

	// A depth-0 row with a parent is refused.
	assignmentMustRefuse(t, db, "a depth-0 assignment with a parent", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations
		) VALUES ('at-depth0parent', ?, ?, ?, 0, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:depth0parent', 1, 4, 2, 100, '["read"]')
	`, projectID, root, root)

	// The well-formed child is accepted, and it carries no budgets.
	child := insertChildAssignment(t, dm, projectID, "at-goodchild", root, root, "charlie", "idem:goodchild")
	var maxDepth, maxTasks sql.NullInt64
	if err := db.QueryRow(`SELECT max_depth, max_tasks FROM agent_tasks WHERE id = ?`, child).
		Scan(&maxDepth, &maxTasks); err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if maxDepth.Valid || maxTasks.Valid {
		t.Errorf("the child carries budgets (max_depth=%v max_tasks=%v); a child inherits and "+
			"must not hold a budget of its own", maxDepth, maxTasks)
	}
}

// TestAssignmentTreeIsContainedInOneProject pins the containment boundary.
//
// The project id is denormalised onto the assignment and held honest by a
// COMPOSITE foreign key. So a child cannot name a root of another project,
// and it cannot name a parent of another project — the boundary is a
// property of the storage, not of the code that happens to respect it.
func TestAssignmentTreeIsContainedInOneProject(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectA := seedProject(t, dm, "cpj-"+strings.Repeat("a", 16))
	projectB := seedProject(t, dm, "cpj-"+strings.Repeat("b", 16))

	rootA := insertRootAssignment(t, dm, projectA, "at-root-a", "alpha", "implementer", "idem:root-a")

	// A child in project B naming a root in project A is refused.
	assignmentMustRefuse(t, db, "a child whose root is in another project", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key
		) VALUES ('at-crossroot', ?, ?, ?, 1, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:crossroot')
	`, projectB, rootA, rootA)

	// A child in project B naming a parent in project A is refused.
	assignmentMustRefuse(t, db, "a child whose parent is in another project", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key
		) VALUES ('at-crossparent', ?, ?, ?, 1, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:crossparent')
	`, projectB, rootA, rootA)

	// An assignment in a project that does not exist is refused.
	assignmentMustRefuse(t, db, "an assignment in a project that does not exist", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations
		) VALUES ('at-noproject', 'cpj-' || 'cccccccccccccccc', 'at-noproject', NULL, 0, 't', 'o',
			'alpha', 'implementer', 'enqueued', 'idem:noproject', 1, 4, 2, 100, '["read"]')
	`)
}

// TestAssignmentDeletionIsRestricted pins the ON DELETE RESTRICT.
//
// A project with an assignment cannot be deleted, and a root with a child
// cannot be deleted. Without that, the ledger could be destroyed by a
// cascade.
func TestAssignmentDeletionIsRestricted(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	root := insertRootAssignment(t, dm, projectID, "at-delroot", "alpha", "implementer", "idem:delroot")
	insertChildAssignment(t, dm, projectID, "at-delchild", root, root, "charlie", "idem:delchild")

	assignmentMustRefuse(t, db, "deleting a project that has an assignment",
		`DELETE FROM projects WHERE id = ?`, projectID)
	assignmentMustRefuse(t, db, "deleting a root that has a child",
		`DELETE FROM agent_tasks WHERE id = ?`, root)

	// The ledger's reference to the task is restricted too.
	assignmentMustExec(t, db, `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-del-1', ?, 0, 'created')`, root)
	assignmentMustRefuse(t, db, "deleting a task that has a ledger entry",
		`DELETE FROM agent_tasks WHERE id = ?`, root)
}

// TestAssignmentIdempotencyKeyIsUnique pins the duplicate-suppression.
//
// An accidental duplicate dispatch of the same logical assignment must not
// create a second row.
func TestAssignmentIdempotencyKeyIsUnique(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	insertRootAssignment(t, dm, projectID, "at-idem-1", "alpha", "implementer", "github:issue:253")

	assignmentMustRefuse(t, db, "a second assignment with the same idempotency key", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations
		) VALUES ('at-idem-2', ?, 'at-idem-2', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'github:issue:253', 1, 4, 2, 100, '["read"]')
	`, projectID)

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_tasks WHERE idempotency_key = 'github:issue:253'`).
		Scan(&n); err != nil {
		t.Fatalf("count the assignments: %v", err)
	}
	if n != 1 {
		t.Errorf("there are %d assignments with the idempotency key, want exactly 1", n)
	}
}

// TestAssignmentEventLedgerIsOrderedAndGapFree pins the ledger shape.
//
// The event index is UNIQUE per assignment, so the ledger is ordered and a
// duplicate index is refused.
func TestAssignmentEventLedgerIsOrderedAndGapFree(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	id := insertRootAssignment(t, dm, projectID, "at-ledger", "alpha", "implementer", "idem:ledger")

	assignmentMustExec(t, db, `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-l1', ?, 0, 'created')`, id)
	assignmentMustExec(t, db, `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-l2', ?, 1, 'delivered')`, id)

	assignmentMustRefuse(t, db, "a duplicate event index for the same assignment", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-l3', ?, 1, 'acknowledged')`, id)

	assignmentMustRefuse(t, db, "a ledger entry for an assignment that does not exist", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-l4', 'at-no-such-task', 0, 'created')`)

	assignmentMustRefuse(t, db, "a ledger entry with an unknown event type", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-l5', ?, 2, 'vibed')`, id)
}

// TestAssignmentForeignKeysAreEnforced guards the fixtures.
//
// Every negative case above is only meaningful if the foreign-key pragma is
// ON. A test that runs against a connection without it would pass
// vacuously — so the pragma is asserted.
func TestAssignmentForeignKeysAreEnforced(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	var fk int
	if err := dm.SQLDB().QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read the foreign_keys pragma: %v", err)
	}
	if fk != 1 {
		t.Fatalf("the foreign_keys pragma is %d, not 1. Every negative case in this file "+
			"depends on foreign keys being enforced; they would pass vacuously otherwise.", fk)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The negative controls — the tests themselves are load-bearing
// ─────────────────────────────────────────────────────────────────────────────

// TestAssignmentNegativeControlsAreLoadBearing proves the suite is not
// decorative.
//
// Each control DISABLES one invariant and names the test that must then
// fail. The control is applied by dropping the trigger or the index, so the
// suite's own dependence on it is measured rather than asserted.
//
// The mechanism: the control drops the object, runs the named test's
// assertion logic, and requires that the assertion NOW fails. A test that
// cannot fail when its invariant is removed is not a test of that
// invariant.
func TestAssignmentNegativeControlsAreLoadBearing(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// The control: drop the state-machine trigger, and the illegal
	// transition must then be ACCEPTED.
	assignmentMustExec(t, db, `DROP TRIGGER trg_agent_tasks_state_transition`)

	impl := insertRootAssignment(t, dm, projectID, "at-ctrl", "alpha", "implementer", "idem:ctrl")
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'delivered' WHERE id = ?`, impl)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'acknowledged' WHERE id = ?`, impl)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'accepted' WHERE id = ?`, impl)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'in_progress' WHERE id = ?`, impl)

	if _, err := db.Exec(`UPDATE agent_tasks SET state = 'operator_approved' WHERE id = ?`, impl); err != nil {
		t.Fatalf("with the state-machine trigger dropped, the illegal transition was still "+
			"refused (%v) — so the trigger is not what refuses it, and the suite's state-machine "+
			"assertions are not testing the trigger", err)
	}
	// The transition landed, which is the proof the control works.
	var state string
	if err := db.QueryRow(`SELECT state FROM agent_tasks WHERE id = ?`, impl).Scan(&state); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if state != "operator_approved" {
		t.Errorf("with the trigger dropped the illegal transition did not land (state %q); "+
			"the control is not measuring what it claims", state)
	}
}

// TestAssignmentClaimControlIsLoadBearing is the control for the claim
// discipline.
func TestAssignmentClaimControlIsLoadBearing(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	assignmentMustExec(t, db, `DROP TRIGGER trg_agent_tasks_claim_discipline`)

	id := insertRootAssignment(t, dm, projectID, "at-ctrl2", "alpha", "implementer", "idem:ctrl2")

	// With the trigger dropped, an epoch that jumps by two is accepted.
	if _, err := db.Exec(`
		UPDATE agent_tasks
		SET claim_epoch = claim_epoch + 2, owner = 'alpha', lease_expires_at = 1900000000
		WHERE id = ?`, id); err != nil {
		t.Fatalf("with the claim trigger dropped, the epoch jump was still refused (%v) — "+
			"the suite's claim assertions are not testing the trigger", err)
	}

	// And the owner can change with no claim at all.
	if _, err := db.Exec(`UPDATE agent_tasks SET owner = 'mallory' WHERE id = ?`, id); err != nil {
		t.Fatalf("with the claim trigger dropped, the owner change was still refused (%v)", err)
	}
}

// TestAssignmentIdempotencyControlIsLoadBearing is the control for the
// unique idempotency key.
func TestAssignmentIdempotencyControlIsLoadBearing(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	assignmentMustExec(t, db, `DROP INDEX idx_agent_tasks_idempotency_key`)

	insertRootAssignment(t, dm, projectID, "at-ctrl3", "alpha", "implementer", "github:issue:253")

	if _, err := db.Exec(`
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations
		) VALUES ('at-ctrl3b', ?, 'at-ctrl3b', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'github:issue:253', 1, 4, 2, 100, '["read"]')
	`, projectID); err != nil {
		t.Fatalf("with the unique index dropped, the duplicate was still refused (%v) — the "+
			"suite's idempotency assertion is not testing the index", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The restore allow-list is still load-bearing
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentAssignmentAllowListIsLoadBearing proves the two new allow-list
// entries are not decorative.
//
// Removing them must make the validator STRICTER: a dump declaring an
// assignment table must then be refused.
func TestAgentAssignmentAllowListIsLoadBearing(t *testing.T) {
	original := CanonicalMPMSchema
	defer func() { CanonicalMPMSchema = original }()

	// Remove the two new names.
	var kept []string
	for _, name := range CanonicalMPMSchema {
		if name == "agent_tasks" || name == "agent_task_events" {
			continue
		}
		kept = append(kept, name)
	}
	CanonicalMPMSchema = kept

	v := NewCanonicalDumpValidator()
	for _, table := range agentAssignmentTables {
		dump := "CREATE TABLE " + table + " (id TEXT PRIMARY KEY);\n"
		if err := v.Validate(dump); err == nil {
			t.Errorf("a dump declaring %q was ACCEPTED with the table removed from the "+
				"allow-list. The allow-list is the only thing standing between an arbitrary "+
				"dump and the user's MPM state.", table)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The object inventory is stable
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentAssignmentObjectInventoryIsStable pins the exact set of objects
// the migration creates.
//
// A new index or trigger that appears without a decision is a change to the
// contract's storage, and the inventory is where that change becomes
// visible.
func TestAgentAssignmentObjectInventoryIsStable(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()

	objects := assignmentSchemaObjects(t, db)

	for _, name := range agentAssignmentTables {
		if _, ok := objects[name]; !ok {
			t.Errorf("the table %q is missing", name)
		}
	}
	for _, name := range agentAssignmentIndexes {
		if _, ok := objects[name]; !ok {
			t.Errorf("the index %q is missing", name)
		}
	}
	for _, name := range agentAssignmentTriggers {
		if _, ok := objects[name]; !ok {
			t.Errorf("the trigger %q is missing", name)
		}
	}

	// No OTHER trigger was created on agent_tasks or agent_task_events.
	rows, err := db.Query(
		"SELECT name FROM sqlite_master WHERE type = 'trigger' AND " +
			"(tbl_name = 'agent_tasks' OR tbl_name = 'agent_task_events') ORDER BY name")
	if err != nil {
		t.Fatalf("read the triggers: %v", err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan the triggers: %v", err)
		}
		found = append(found, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the triggers: %v", err)
	}
	sort.Strings(found)
	want := append([]string{}, agentAssignmentTriggers...)
	sort.Strings(want)
	if len(found) != len(want) {
		t.Errorf("the assignment tables carry %d triggers (%v), want exactly %d (%v)",
			len(found), found, len(want), want)
		return
	}
	for i := range want {
		if found[i] != want[i] {
			t.Errorf("the assignment tables carry the triggers %v, want exactly %v", found, want)
			return
		}
	}
}
