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
//	    owner; a task is born unclaimed; the role is immutable; the root
//	    carries the budgets and a child carries none; the delegation tree
//	    is a tree contained in ONE project; the idempotency key is unique;
//	    the ledger is append-only and monotonic; the operator's gate names
//	    its action.
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
//
// WHAT THE SECOND INDEPENDENT REVIEW CHANGED
//
//	The first version of this suite was reviewed independently and eleven
//	blocking findings came back. Every one was a real defect, and each
//	now has a REGRESSION test that fails without the fix. The findings
//	that were about the TESTS themselves — an assertion that could not
//	fail, a probe that passed for the wrong reason, a "whole row"
//	comparison that compared one column — are closed here too.
package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
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
//
// The twenty-six triggers carry NINETEEN rules: seven rules are split across
// several triggers so that a test can isolate one clause from another
// (the id and the key, the tree shape, the root's recipient, the two lists,
// the claim discipline and the operator's gate).
var agentAssignmentTriggers = []string{
	"trg_agent_task_events_id_is_not_reused",
	"trg_agent_task_events_monotonic",
	"trg_agent_task_events_no_delete",
	"trg_agent_task_events_no_update",
	"trg_agent_tasks_approval_is_immutable",
	"trg_agent_tasks_awaiting_action_is_immutable",
	"trg_agent_tasks_claim_discipline",
	"trg_agent_tasks_claim_sets_lease",
	"trg_agent_tasks_claim_sets_owner",
	"trg_agent_tasks_depth_is_immutable",
	"trg_agent_tasks_eligible_recipients_insert",
	"trg_agent_tasks_eligible_recipients_update",
	"trg_agent_tasks_id_is_not_reused",
	"trg_agent_tasks_initial_state",
	"trg_agent_tasks_key_is_not_reused",
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

// agentAssignmentObjectNames is every object the migration creates: the
// tables, the indexes and the triggers. The inventory test compares the
// migration's ACTUAL delta against this list, so an object that appears
// without a decision is visible.
var agentAssignmentObjectNames = func() []string {
	out := make([]string, 0, len(agentAssignmentTables)+len(agentAssignmentIndexes)+len(agentAssignmentTriggers))
	out = append(out, agentAssignmentTables...)
	out = append(out, agentAssignmentIndexes...)
	out = append(out, agentAssignmentTriggers...)
	sort.Strings(out)
	return out
}()

// canonicalMPMSchemaBeforeContinuity001SHA is the SHA-256 of the restore
// allow-list as it stood BEFORE this PR, joined with newlines.
//
// It is the exact-delta anchor: the allow-list must gain EXACTLY the two
// assignment tables, so removing those two names from the current list must
// reproduce the pre-change list byte for byte. A pin is used rather than a
// literal copy of a 55-entry list, because a literal copy is a second place
// to forget to update.
const canonicalMPMSchemaBeforeContinuity001SHA = "2648c5a157ea9f1a1f67e588956cccf7cd6e493f7cd1739f74761ba11be63197"

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

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

// jsonArray renders a JSON array of the given strings.
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
			recipient, role, state, idempotency_key) VALUES (?, ?, ?, ?, 1, 'the title', 'the objective', ?, 'implementer', 'enqueued', ?)
	`, id, projectID, rootID, parentID, recipient, idem)
	if err != nil {
		t.Fatalf("insert the child assignment %q: %v", id, err)
	}
	return id
}

// assignmentMustExec runs a statement and fails the test if it errors.
func assignmentMustExec(t *testing.T, db *sql.DB, query string, args ...interface{}) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// assignmentMustRefuse runs a statement and fails the test if it SUCCEEDS.
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

// assignmentMustRefuseWith runs a statement, requires it to be REFUSED, and
// requires the refusal to name the expected rule.
//
// This is the isolation the second review asked for. A refusal that happens
// for the WRONG reason — a different constraint firing first — proves
// nothing about the rule under test, so the message is part of the
// assertion.
func assignmentMustRefuseWith(t *testing.T, db *sql.DB, what, wantMessage string, query string, args ...interface{}) {
	t.Helper()
	err := assignmentMustRefuse(t, db, what, query, args...)
	if err == nil {
		return
	}
	if !strings.Contains(err.Error(), wantMessage) {
		t.Errorf("%s was refused, but NOT by the rule under test: the error was %q and it does "+
			"not mention %q. The refusal is real, but it is the wrong refusal, so the rule "+
			"under test is not pinned.", what, err.Error(), wantMessage)
	}
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

// assignmentRowSnapshot renders a WHOLE row as a canonical string.
//
// The second review found that a test which called a one-column comparison
// "the whole row" was comparing one column. This reads every column, in
// the table's own order, and renders the value together with its Go type,
// so a change to any field — including one that changes only the type —
// is visible.
func assignmentRowSnapshot(t *testing.T, db *sql.DB, table, id string) string {
	t.Helper()
	rows, err := db.Query("SELECT * FROM "+table+" WHERE id = ?", id)
	if err != nil {
		t.Fatalf("read the row from %s: %v", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("read the columns of %s: %v", table, err)
	}
	if !rows.Next() {
		t.Fatalf("the row %q is not in %s", id, table)
	}
	vals := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("scan the row from %s: %v", table, err)
	}
	parts := make([]string, 0, len(cols))
	for i, c := range cols {
		parts = append(parts, fmt.Sprintf("%s=%#v(%T)", c, vals[i], vals[i]))
	}
	return strings.Join(parts, "|")
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
//
// The second independent review is why this hash moved once already: the
// eleven blocking findings it returned were all closed in the DDL, and a
// DDL that has changed is a DDL whose pin must move with it.
func TestAgentAssignmentDDLIsPinned(t *testing.T) {
	sum := sha256.Sum256([]byte(agentAssignmentDDL))
	got := hex.EncodeToString(sum[:])

	const want = "f760d233724e39278cbaca2ec548e9c5736a56f7c7c514338aa881d3c045d5c8"
	if got != want {
		t.Errorf("the DDL's SHA-256 is %s but the pin says %s. If the DDL changed on purpose, "+
			"recompute the hash and say so in the commit message; if it did not, the storage has "+
			"drifted from the design note and the contract is no longer what was reviewed.",
			got, want)
	}
}

// TestAgentAssignmentDDLIsTheOnlyDefinition guards the ONE DDL path.
//
// There is exactly one text that creates these tables. A second definition
// — in a schema slice, in a test fixture, in a migration — would let the
// two drift, which is the failure PR #224's Stage 1 avoided by owning its
// DDL in one place.
//
// The check is a source scan of the package: the assignment tables are
// declared in exactly one file, and that file is the migration.
func TestAgentAssignmentDDLIsTheOnlyDefinition(t *testing.T) {
	tables, err := scanSchemaSources(t)
	if err != nil {
		t.Fatalf("scan the schema sources: %v", err)
	}
	for _, name := range agentAssignmentTables {
		if !tables[name] {
			t.Errorf("the table %q is not declared in any source file; the source-scan guard "+
				"that keeps the restore allow-list honest cannot see it", name)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AT1 — an existing database gains the objects and changes nothing else
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentAssignmentAT1ExistingDatabaseMigration — AT1.
//
// A disposable file-backed WAL database is built, a representative row is
// written into `memories`, and the assignment objects are REMOVED so the
// migration has real work to do. The migration is then run against it: the
// two tables, the seven indexes and the twenty-six triggers exist BY NAME,
// the sentinel is recorded exactly once, the pre-existing row is untouched,
// and the set of schema objects changed by the migration is EXACTLY the
// assignment objects.
//
// The second review found that the first version of this test snapshotted
// the schema AFTER the boot path had already migrated, so the sentinel
// short-circuited the re-run and nothing was actually exercised. This
// version strips the objects first, so the migration really does apply.
func TestAgentAssignmentAT1ExistingDatabaseMigration(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()

	// A representative pre-existing row.
	assignmentMustExec(t, db, `INSERT INTO memories (id, collection, content, created_at)
		VALUES ('mem-pre-existing', 'memories', 'the original content', 1700000000)`)

	// Put the database back into its pre-assignment state, and remove the
	// sentinel so the migration really runs.
	stripAssignmentObjects(t, dm)
	before := assignmentSchemaObjects(t, db)

	// The migration must not find the objects already present.
	for _, name := range agentAssignmentObjectNames {
		if _, ok := before[name]; ok {
			t.Fatalf("the object %q survived the strip, so this test would not exercise the "+
				"migration's creation path", name)
		}
	}

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

	// Nothing else changed, and EXACTLY the assignment objects were added.
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
	var added []string
	for name := range after {
		if _, ok := before[name]; !ok {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	want := append([]string{}, agentAssignmentObjectNames...)
	sort.Strings(want)
	if len(added) != len(want) {
		t.Fatalf("the migration added %d objects (%v), want exactly %d (%v)",
			len(added), added, len(want), want)
	}
	for i := range want {
		if added[i] != want[i] {
			t.Fatalf("the migration added %v, want exactly %v", added, want)
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
//
// The comparison is of the WHOLE row, field by field, with the field's
// type — the second review found that the first version compared one
// column and called it the whole row.
func TestAgentAssignmentAT3AT4NoFabricatedState(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()

	assignmentMustExec(t, db, `INSERT INTO memories (id, collection, content, created_at)
		VALUES ('mem-at34', 'memories', 'the original content', 1700000000)`)

	before := assignmentRowSnapshot(t, db, "memories", "mem-at34")

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

	// The pre-existing row is byte-identical, field by field.
	after := assignmentRowSnapshot(t, db, "memories", "mem-at34")
	if before != after {
		t.Errorf("the migration changed the pre-existing row:\n  before: %s\n  after:  %s",
			before, after)
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
// aborts the sentinel write, and the test requires the failure to BE that
// failure: a migration that failed earlier — before the DDL ran — would
// satisfy "no table survives" without ever having exercised the rollback
// of a successful DDL. The second review found exactly that weakness.
func TestAgentAssignmentAT6FailedMigrationRollsBack(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()

	// Strip the assignment objects so the migration has work to do.
	stripAssignmentObjects(t, dm)

	// Inject the failure at the sentinel write.
	assignmentMustExec(t, db, `
		CREATE TRIGGER zz_agent_assignment_sentinel_abort
		BEFORE INSERT ON schema_migrations
		WHEN NEW.id = '`+agentAssignmentSentinelID+`'
		BEGIN
			SELECT RAISE(ABORT, 'injected failure at the sentinel write');
		END`)

	err := MigrateAgentAssignmentsInTx(t, db)
	if err == nil {
		t.Fatal("the migration SUCCEEDED despite the injected failure; the rollback is not being tested")
	}
	if !strings.Contains(err.Error(), "injected failure at the sentinel write") {
		t.Fatalf("the migration failed for the WRONG reason: %v. The test must fail at the "+
			"sentinel write, after the DDL has already succeeded, or it does not test the "+
			"rollback of a successful DDL.", err)
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
// succeed and leave the full schema AND the sentinel. A migration that
// could not recover would need an operator to intervene on every
// interrupted boot, and a recovery that left the schema but not the
// sentinel would re-run the DDL on every subsequent open.
func TestAgentAssignmentAT6RecoveryAfterInjectedFailure(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()

	stripAssignmentObjects(t, dm)
	assignmentMustExec(t, db, `
		CREATE TRIGGER zz_agent_assignment_sentinel_abort
		BEFORE INSERT ON schema_migrations
		WHEN NEW.id = '`+agentAssignmentSentinelID+`'
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
	for _, trigger := range agentAssignmentTriggers {
		var n int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM main.sqlite_master WHERE type = 'trigger' AND name = ?", trigger,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", trigger, err)
		}
		if n != 1 {
			t.Errorf("the trigger %q is missing after the recovery run (count %d)", trigger, n)
		}
	}

	// The sentinel IS recorded after the recovery — exactly once.
	var sentinels int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE id = ?", agentAssignmentSentinelID,
	).Scan(&sentinels); err != nil {
		t.Fatalf("count the sentinels: %v", err)
	}
	if sentinels != 1 {
		t.Errorf("the sentinel is recorded %d times after the recovery run, want exactly 1", sentinels)
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
// Two things, and the second review was right that the first version only
// did the first:
//
//  1. the allow-list gained EXACTLY the two assignment names — not three,
//     not one — and nothing else moved. The pre-change list is pinned by
//     hash, so removing the two new names from the current list must
//     reproduce it exactly;
//  2. a dump carrying the REAL DDL of the two tables is ACCEPTED. The
//     first version validated a toy table; the real dump is the one a
//     backup would carry.
func TestAgentAssignmentAT8RestoreAllowList(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()

	// 1. the exact delta.
	var without []string
	for _, name := range CanonicalMPMSchema {
		if name == "agent_tasks" || name == "agent_task_events" {
			continue
		}
		without = append(without, name)
	}
	sum := sha256.Sum256([]byte(strings.Join(without, "\n")))
	got := hex.EncodeToString(sum[:])
	if got != canonicalMPMSchemaBeforeContinuity001SHA {
		t.Errorf("the restore allow-list did not gain EXACTLY the two assignment names. "+
			"With them removed it hashes to %s; the pre-change allow-list hashes to %s. "+
			"The allow-list gained or lost something else.", got, canonicalMPMSchemaBeforeContinuity001SHA)
	}
	for _, table := range agentAssignmentTables {
		found := false
		for _, name := range CanonicalMPMSchema {
			if name == table {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the table %q is not in CanonicalMPMSchema; the restore allow-list would "+
				"refuse a legitimate dump", table)
		}
	}

	// 2. a dump carrying the REAL assignment schema is accepted.
	//
	// The dump carries the REAL DDL of every assignment object — the two
	// tables, the seven indexes and the twenty-five triggers, exactly as
	// `sqlite3 .dump` would emit them.
	//
	// What this test does NOT do is prove that the dump RESTORES. The
	// third review found that the earlier version of this fixture
	// carried a row that would not even execute — the INSERT omitted the
	// budgets and the allow-list, so the defaults applied and the row
	// was refused by the schema's own rules — and the test passed
	// anyway, because the validator ACCEPTS text without executing it.
	//
	// So the restore itself is proved where it can be: by the COMMAND,
	// in cmd/mpm/handlers_restore_assignment_test.go, which drives the
	// real `mpm restore-db` over a dump generated from the live schema
	// and requires the restored database to carry the tables, the
	// indexes, the triggers AND the rows. This test keeps the
	// allow-list's half of the contract: the validator accepts the
	// schema.
	objects := assignmentSchemaObjects(t, db)
	var dump strings.Builder
	for _, name := range agentAssignmentObjectNames {
		sqlText, ok := objects[name]
		if !ok {
			t.Fatalf("the object %q is missing from the migrated database", name)
		}
		dump.WriteString(sqlText + ";\n")
	}

	v := NewCanonicalDumpValidator()
	if err := v.Validate(dump.String()); err != nil {
		t.Errorf("a dump carrying the REAL assignment schema was rejected: %v. MPM creates "+
			"those tables, so the restore allow-list must carry them — otherwise every "+
			"backup taken after this change is refused.", err)
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

	assignmentMustRefuseWith(t, db, "an assignment created in the 'completed' state",
		"created in the 'enqueued' state", `
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

// TestAssignmentIsCreatedUnclaimed pins the second creation rule.
//
// An assignment is born UNCLAIMED: epoch 0, no owner, no lease. The second
// review found that the claim discipline checked UPDATES only, so a task
// could be born at epoch 87 with an owner and no lease — a fence that had
// never been claimed, and therefore a fence that could not be reasoned
// about.
func TestAssignmentIsCreatedUnclaimed(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// Born with an epoch.
	assignmentMustRefuseWith(t, db, "an assignment born with a claim epoch",
		"created unclaimed", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key, claim_epoch,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-born-epoch', ?, 'at-born-epoch', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:born-epoch', 87, 1, 4, 2, 100, '["read"]', '["alpha"]')
	`, projectID)

	// Born with an owner.
	assignmentMustRefuseWith(t, db, "an assignment born with an owner",
		"created unclaimed", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key, owner,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-born-owner', ?, 'at-born-owner', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:born-owner', 'alice', 1, 4, 2, 100, '["read"]', '["alpha"]')
	`, projectID)

	// Born with a lease.
	assignmentMustRefuseWith(t, db, "an assignment born with a lease",
		"created unclaimed", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key, lease_expires_at,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-born-lease', ?, 'at-born-lease', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:born-lease', 1900000000, 1, 4, 2, 100, '["read"]', '["alpha"]')
	`, projectID)

	// And the well-formed one IS born unclaimed.
	id := insertRootAssignment(t, dm, projectID, "at-born-unclaimed", "alpha", "implementer", "idem:born-unclaimed")
	var epoch int64
	var owner sql.NullString
	var lease sql.NullInt64
	if err := db.QueryRow(
		`SELECT claim_epoch, owner, lease_expires_at FROM agent_tasks WHERE id = ?`, id,
	).Scan(&epoch, &owner, &lease); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if epoch != 0 {
		t.Errorf("the assignment was born at epoch %d, want 0", epoch)
	}
	if owner.Valid {
		t.Errorf("the assignment was born with the owner %q, want none", owner.String)
	}
	if lease.Valid {
		t.Errorf("the assignment was born with the lease %d, want none", lease.Int64)
	}
}

// TestAssignmentStateMachineIsRoleAware pins the transition rules.
//
// The separations are the point: delivery is not acceptance, acceptance is
// not execution, execution is not completion, completion is not review, and
// review is not approval. A state machine that collapsed any two of them
// would make the ledger lie about what happened.
//
// The second review found two holes here, and both are now covered:
//
//   - the implementer could reach `awaiting_operator` from `completed`,
//     SKIPPING the independent review;
//   - the role was MUTABLE, so a relabel selected a different path and a
//     role-only write evaded the path check entirely.
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
		switch step.to {
		case "awaiting_operator":
			// The gate names the action it awaits.
			assignmentMustExec(t, db, `
				UPDATE agent_tasks SET state = ?, awaiting_action = 'merge PR #259' WHERE id = ?`,
				step.to, impl)
		case "operator_approved":
			// The approval names its action, its revision and its source.
			assignmentMustExec(t, db, `
				UPDATE agent_tasks
				SET state = ?, approval_action = 'merge PR #259', approval_revision = 'abc123',
				    approval_source = 'operator', approval_evidence = 'docs/evidence/x.json'
				WHERE id = ?`, step.to, impl)
		default:
			assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step.to, impl)
		}
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
	assignmentMustRefuseWith(t, db, "an implementer jumping from in_progress to review_passed",
		"illegal state transition", `
		UPDATE agent_tasks SET state = 'review_passed' WHERE id = ?`, impl2)

	// The implementer may NOT reach the operator's gate without the
	// independent review. This is the hole the second review found.
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'completed' WHERE id = ?`, impl2)
	assignmentMustRefuseWith(t, db, "an implementer jumping from completed to awaiting_operator",
		"illegal state transition", `
		UPDATE agent_tasks SET state = 'awaiting_operator' WHERE id = ?`, impl2)

	// Nor may it jump straight to the approval.
	assignmentMustRefuseWith(t, db, "an implementer jumping from completed to operator_approved",
		"illegal state transition", `
		UPDATE agent_tasks SET state = 'operator_approved' WHERE id = ?`, impl2)

	// The reviewer's path: a review ends at review_passed (it passed) or
	// at completed (it asked for changes) — never at the operator's gate.
	rev := insertRootAssignment(t, dm, projectID, "at-rev", "charlie", "reviewer", "idem:rev")
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'delivered' WHERE id = ?`, rev)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'acknowledged' WHERE id = ?`, rev)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'accepted' WHERE id = ?`, rev)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'in_progress' WHERE id = ?`, rev)
	assignmentMustRefuseWith(t, db, "a reviewer jumping from in_progress to awaiting_operator",
		"illegal state transition", `
		UPDATE agent_tasks SET state = 'awaiting_operator' WHERE id = ?`, rev)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'review_passed' WHERE id = ?`, rev)

	// A reviewer's terminal state is terminal.
	assignmentMustRefuseWith(t, db, "a review_passed reviewer being moved on",
		"illegal state transition", `
		UPDATE agent_tasks SET state = 'completed' WHERE id = ?`, rev)

	// A reviewer's `completed` — the review asked for changes — is
	// terminal too.
	rev2 := insertRootAssignment(t, dm, projectID, "at-rev2", "charlie", "reviewer", "idem:rev2")
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'delivered' WHERE id = ?`, rev2)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'acknowledged' WHERE id = ?`, rev2)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'accepted' WHERE id = ?`, rev2)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'in_progress' WHERE id = ?`, rev2)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'completed' WHERE id = ?`, rev2)
	assignmentMustRefuseWith(t, db, "a completed reviewer being moved on",
		"illegal state transition", `
		UPDATE agent_tasks SET state = 'review_passed' WHERE id = ?`, rev2)
}

// TestAssignmentCancellationIsUniversal pins the withdrawal rule.
//
// `cancelled` is the UNIVERSAL withdrawal: legal from every state except
// itself — including `operator_approved`, because an operator may withdraw
// an approved assignment, and including a reviewer's `completed`.
//
// The second review found that cancellation was NOT universal: an approved
// task and a reviewer in `completed` or `review_passed` could not be
// cancelled, and the design note called those states terminal.
func TestAssignmentCancellationIsUniversal(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// Every state a task can legitimately reach, and then be cancelled
	// from.
	implementerPath := []string{
		"delivered", "acknowledged", "accepted", "in_progress",
		"completed", "review_passed",
	}
	for i, target := range implementerPath {
		id := insertRootAssignment(t, dm, projectID,
			fmt.Sprintf("at-cancel-impl-%d", i), "alpha", "implementer",
			fmt.Sprintf("idem:cancel-impl-%d", i))
		for _, step := range implementerPath[:i+1] {
			assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step, id)
		}
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, target, id)
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'cancelled' WHERE id = ?`, id)
	}

	// From the operator's gate.
	gate := insertRootAssignment(t, dm, projectID, "at-cancel-gate", "alpha", "implementer", "idem:cancel-gate")
	for _, step := range []string{"delivered", "acknowledged", "accepted", "in_progress",
		"completed", "review_passed"} {
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step, gate)
	}
	assignmentMustExec(t, db, `
		UPDATE agent_tasks SET state = 'awaiting_operator', awaiting_action = 'merge PR #259'
		WHERE id = ?`, gate)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'cancelled' WHERE id = ?`, gate)

	// From an APPROVED assignment — the operator may withdraw it.
	approved := insertRootAssignment(t, dm, projectID, "at-cancel-approved", "alpha", "implementer", "idem:cancel-approved")
	for _, step := range []string{"delivered", "acknowledged", "accepted", "in_progress",
		"completed", "review_passed"} {
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step, approved)
	}
	assignmentMustExec(t, db, `
		UPDATE agent_tasks SET state = 'awaiting_operator', awaiting_action = 'merge PR #259'
		WHERE id = ?`, approved)
	assignmentMustExec(t, db, `
		UPDATE agent_tasks
		SET state = 'operator_approved', approval_action = 'merge PR #259',
		    approval_revision = 'abc123', approval_source = 'operator'
		WHERE id = ?`, approved)
	assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'cancelled' WHERE id = ?`, approved)

	// From a reviewer's terminal states.
	for i, target := range []string{"completed", "review_passed"} {
		id := insertRootAssignment(t, dm, projectID,
			fmt.Sprintf("at-cancel-rev-%d", i), "charlie", "reviewer",
			fmt.Sprintf("idem:cancel-rev-%d", i))
		for _, step := range []string{"delivered", "acknowledged", "accepted", "in_progress", target} {
			assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step, id)
		}
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = 'cancelled' WHERE id = ?`, id)
	}

	// But a cancelled assignment is not revived.
	assignmentMustRefuseWith(t, db, "a cancelled assignment being revived",
		"illegal state transition", `
		UPDATE agent_tasks SET state = 'enqueued' WHERE id = ?`, gate)
	assignmentMustRefuseWith(t, db, "a cancelled assignment being completed",
		"illegal state transition", `
		UPDATE agent_tasks SET state = 'completed' WHERE id = ?`, gate)
}

// TestAssignmentRoleIsImmutable pins the role.
//
// The transition path is selected by the role. If the role were mutable, a
// relabel would select a DIFFERENT path — and a role-only write would
// evade the path check entirely, because the state did not change. The
// second review found exactly that hole.
func TestAssignmentRoleIsImmutable(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	id := insertRootAssignment(t, dm, projectID, "at-role", "alpha", "implementer", "idem:role")

	assignmentMustRefuseWith(t, db, "an implementer relabelled as a reviewer",
		"the role is immutable", `
		UPDATE agent_tasks SET role = 'reviewer' WHERE id = ?`, id)

	// A write that sets the role to the value it already has changes
	// nothing, so it is accepted: the rule is about a CHANGE of role, and
	// a no-op is not one.
	assignmentMustExec(t, db, `UPDATE agent_tasks SET role = 'implementer' WHERE id = ?`, id)

	// The COMBINED case: a relabel and a state transition in ONE
	// statement. The second review found that the first version covered
	// only the role-only write, and the combined case is the one that
	// actually matters — a single UPDATE that selects a different path.
	assignmentMustRefuseWith(t, db, "a relabel and a transition in one statement",
		"the role is immutable", `
		UPDATE agent_tasks SET role = 'reviewer', state = 'delivered' WHERE id = ?`, id)

	// And the same, expressed as a CASE so the new value is not a
	// literal.
	assignmentMustRefuseWith(t, db, "a relabel expressed as a CASE",
		"the role is immutable", `
		UPDATE agent_tasks SET role = CASE WHEN id = ? THEN 'reviewer' ELSE role END WHERE id = ?`,
		id, id)

	var role string
	if err := db.QueryRow(`SELECT role FROM agent_tasks WHERE id = ?`, id).Scan(&role); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if role != "implementer" {
		t.Errorf("the role is %q after the refused relabels, want implementer", role)
	}
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
//
// Each refusal names the rule that refused it, because the second review
// found that the ownerless-claim probe was satisfied by a DIFFERENT
// constraint (the column CHECK that a lease implies an owner) and so did
// not pin the trigger's own owner requirement. The claims are split into
// four triggers precisely so that each rule can be isolated.
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
	assignmentMustRefuseWith(t, db, "a claim epoch that jumps by two",
		"the claim epoch advances by exactly one", `
		UPDATE agent_tasks
		SET claim_epoch = claim_epoch + 2, owner = 'alpha', lease_expires_at = 1900000000
		WHERE id = ?`, id)

	// The epoch cannot be reset.
	assignmentMustRefuseWith(t, db, "a claim epoch that is reset to zero",
		"the claim epoch advances by exactly one", `
		UPDATE agent_tasks
		SET claim_epoch = 0, owner = 'alpha', lease_expires_at = 1900000000
		WHERE id = ?`, id)

	// The owner cannot change without a claim.
	assignmentMustRefuseWith(t, db, "an owner change with no claim",
		"only a claim may change the owner", `
		UPDATE agent_tasks SET owner = 'mallory' WHERE id = ?`, id)

	// A claim must set an owner — and the refusal must name the TRIGGER's
	// rule, not the column CHECK that a lease implies an owner.
	assignmentMustRefuseWith(t, db, "a claim with no owner",
		"a claim must set an owner", `
		UPDATE agent_tasks
		SET claim_epoch = claim_epoch + 1, owner = NULL, lease_expires_at = 1900000000
		WHERE id = ?`, id)

	// A claim must set a lease.
	assignmentMustRefuseWith(t, db, "a claim with no lease",
		"a claim must set a lease", `
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

// TestAssignmentFenceCannotBeForgedByANonIntegerEpoch pins the epoch's
// TYPE.
//
// The second review found that INTEGER affinity does not make a number an
// INTEGER: at REAL 1e30, 'epoch + 1' equals 'epoch', so the owner could be
// changed without the fence advancing. The epoch is a fencing token, and a
// fencing token that does not advance is not a fence.
func TestAssignmentFenceCannotBeForgedByANonIntegerEpoch(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// A REAL epoch is refused at creation.
	assignmentMustRefuse(t, db, "an assignment born with a REAL epoch", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key, claim_epoch,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-real-epoch', ?, 'at-real-epoch', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:real-epoch', 1e30, 1, 4, 2, 100, '["read"]', '["alpha"]')
	`, projectID)

	// And a REAL epoch cannot be written either.
	id := insertRootAssignment(t, dm, projectID, "at-real-update", "alpha", "implementer", "idem:real-update")
	assignmentMustRefuse(t, db, "an assignment's epoch set to a REAL", `
		UPDATE agent_tasks SET claim_epoch = 1e30 WHERE id = ?`, id)

	// The same holds for the budget columns: a TEXT budget is not a
	// bound.
	assignmentMustRefuse(t, db, "a root whose max_tasks is the text 'many'", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-text-budget', ?, 'at-text-budget', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:text-budget', 1, 'many', 2, 100, '["read"]', '["alpha"]')
	`, projectID)
}

// TestAssignmentIdempotencyKeyMustBeText pins the key's TYPE.
//
// The second review found that a BLOB key and a TEXT key with the identical
// bytes are DIFFERENT values to SQLite, and a UNIQUE index holds both — so
// the duplicate suppression could be defeated with a BLOB.
func TestAssignmentIdempotencyKeyMustBeText(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// The TEXT key lands.
	assignmentMustExec(t, db, `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-text-key', ?, 'at-text-key', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'github:issue:253', 1, 4, 2, 100, '["read"]', '["alpha"]')`, projectID)

	// The BLOB key with the identical bytes is refused: the key's TYPE is
	// part of its identity.
	assignmentMustRefuse(t, db, "a BLOB idempotency key with the bytes of a TEXT one", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-blob-key', ?, 'at-blob-key', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', x'6769746875623a69737375653a323533', 1, 4, 2, 100, '["read"]', '["alpha"]')`, projectID)
}

// TestAssignmentPrimaryKeyMustNotBeNull pins the identifier.
//
// 'TEXT PRIMARY KEY' does NOT imply NOT NULL in SQLite, and a NULL id made
// the root equality UNKNOWN — and an UNKNOWN operand in a CHECK is not a
// violation. So a depth-zero row pointing at someone else's root was
// accepted. The second review found this.
func TestAssignmentPrimaryKeyMustNotBeNull(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	assignmentMustRefuse(t, db, "an assignment with a NULL id", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES (NULL, ?, NULL, NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:null-id', 1, 4, 2, 100, '["read"]', '["alpha"]')`, projectID)

	// And the ledger's identifier likewise.
	assignmentMustRefuse(t, db, "a ledger entry with a NULL id", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES (NULL, 'at-whatever', 0, 'created')`)
}

// TestAssignmentRootCarriesTheBudgetsAndAChildCarriesNone pins the
// delegation shape.
//
// Inheritance is STRUCTURAL: a root has the budget columns and a child has
// none, so a child cannot widen what it was given — there is no column in
// which to widen it.
//
// The second review found that the child branch constrained
// `permitted_operations` but NOT `eligible_recipients`, so a child could
// store its own expanded allow-list. Both are now constrained, and the
// good-child assertion covers EVERY budget column, not two of them.
func TestAssignmentRootCarriesTheBudgetsAndAChildCarriesNone(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// A root WITHOUT the budgets is refused.
	assignmentMustRefuse(t, db, "a root without the budget columns", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key, permitted_operations, eligible_recipients) VALUES ('at-nobudget', ?, 'at-nobudget', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:nobudget', '["read"]', '["alpha"]')
	`, projectID)

	// A root with an EMPTY permitted_operations is refused: a root that
	// may do nothing is a silent misconfiguration, not a safe default.
	assignmentMustRefuse(t, db, "a root with no permitted operations", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-noops', ?, 'at-noops', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:noops', 1, 4, 2, 100, '[]', '["alpha"]')
	`, projectID)

	// A root whose permitted_operations contains a NULL is refused: the
	// CHECK requires an ELEMENT, not a NAMED operation.
	assignmentMustRefuseWith(t, db, "a root whose permitted operation is null",
		"a permitted operation must be a non-null string", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-nullop', ?, 'at-nullop', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:nullop', 1, 4, 2, 100, '[null]', '["alpha"]')
	`, projectID)

	// A root whose permitted_operations contains a NUMBER is refused.
	assignmentMustRefuseWith(t, db, "a root whose permitted operation is a number",
		"a permitted operation must be a non-null string", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-numop', ?, 'at-numop', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:numop', 1, 4, 2, 100, '[7]', '["alpha"]')
	`, projectID)

	root := insertRootAssignment(t, dm, projectID, "at-budgetroot", "alpha", "implementer", "idem:budgetroot")

	// A child WITH budget columns is refused.
	assignmentMustRefuse(t, db, "a child carrying budget columns", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units) VALUES ('at-childbudget', ?, ?, ?, 1, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:childbudget', 5, 99, 9, 999)
	`, projectID, root, root)

	// A child with its OWN eligible_recipients is refused: the allow-list
	// belongs to the root, and a child that carried one could widen it.
	assignmentMustRefuse(t, db, "a child carrying its own eligible recipients", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key
		) VALUES ('at-childrecipients', ?, ?, ?, 1, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:childrecipients', '["charlie","mallory"]')
	`, projectID, root, root)

	// A child that claims to be its own root is refused.
	assignmentMustRefuse(t, db, "a child that is its own root", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key) VALUES ('at-selfroot', ?, 'at-selfroot', ?, 1, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:selfroot')
	`, projectID, root)

	// A depth-0 row with a parent is refused.
	assignmentMustRefuse(t, db, "a depth-0 assignment with a parent", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-depth0parent', ?, ?, ?, 0, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:depth0parent', 1, 4, 2, 100, '["read"]', '["alpha"]')
	`, projectID, root, root)

	// The well-formed child is accepted, and it carries NO budget at all.
	child := insertChildAssignment(t, dm, projectID, "at-goodchild", root, root, "charlie", "idem:goodchild")
	var maxDepth, maxTasks, maxConcurrency, maxResource sql.NullInt64
	var permitted, eligible string
	if err := db.QueryRow(`
		SELECT max_depth, max_tasks, max_concurrency, max_resource_units,
		       permitted_operations, eligible_recipients
		  FROM agent_tasks WHERE id = ?`, child).
		Scan(&maxDepth, &maxTasks, &maxConcurrency, &maxResource, &permitted, &eligible); err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if maxDepth.Valid || maxTasks.Valid || maxConcurrency.Valid || maxResource.Valid {
		t.Errorf("the child carries budgets (max_depth=%v max_tasks=%v max_concurrency=%v "+
			"max_resource_units=%v); a child inherits and must not hold a budget of its own",
			maxDepth, maxTasks, maxConcurrency, maxResource)
	}
	if permitted != "[]" {
		t.Errorf("the child carries the permitted operations %s, want an empty list", permitted)
	}
	if eligible != "[]" {
		t.Errorf("the child carries the eligible recipients %s, want an empty list", eligible)
	}
}

// TestAssignmentTreeShapeIsATree pins the tree.
//
// The composite foreign keys keep the tree inside ONE project, but they do
// not make it a TREE. The second review found three ways to write a row
// that satisfied every foreign key and was still not a delegation tree:
//
//   - a child that is its OWN parent;
//   - a child that cites a NON-ROOT as its budget root;
//   - a child whose parent belongs to a DIFFERENT tree.
//
// All three are refused now.
func TestAssignmentTreeShapeIsATree(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	root := insertRootAssignment(t, dm, projectID, "at-treeroot", "alpha", "implementer", "idem:treeroot")
	child := insertChildAssignment(t, dm, projectID, "at-treechild", root, root, "charlie", "idem:treechild")
	sibling := insertChildAssignment(t, dm, projectID, "at-treesibling", root, root, "delta", "idem:treesibling")

	// A child that is its own parent.
	assignmentMustRefuseWith(t, db, "a child that is its own parent",
		"the delegation tree is malformed", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key) VALUES ('at-selfparent', ?, 'at-selfparent', 'at-selfparent', 1, 't', 'o', 'charlie',
			'implementer', 'enqueued', 'idem:selfparent')
	`, projectID)

	// A child that cites a NON-ROOT as its budget root.
	assignmentMustRefuseWith(t, db, "a child whose budget root is not a root",
		"the delegation tree is malformed", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key) VALUES ('at-nonrootroot', ?, ?, ?, 1, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:nonrootroot')
	`, projectID, child, root)

	// A child whose parent belongs to a DIFFERENT tree.
	otherRoot := insertRootAssignment(t, dm, projectID, "at-otherroot", "alpha", "implementer", "idem:otherroot")
	assignmentMustRefuseWith(t, db, "a child whose parent is in another tree",
		"the delegation tree is malformed", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key) VALUES ('at-mixedtree', ?, ?, ?, 1, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:mixedtree')
	`, projectID, root, otherRoot)

	// A well-formed grandchild is accepted.
	assignmentMustExec(t, db, `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key) VALUES ('at-grandchild', ?, ?, ?, 2, 't', 'o', 'delta', 'implementer',
			'enqueued', 'idem:grandchild')
	`, projectID, root, child)

	// The tree is not only checked on INSERT. A child re-pointed at a
	// NON-ROOT would leave the tree malformed, and the write must be
	// refused.
	assignmentMustRefuseWith(t, db, "a child re-pointed at a non-root",
		"the delegation tree is malformed", `
		UPDATE agent_tasks SET root_id = ? WHERE id = ?`, child, child)

	// And a child moved to another project is refused — by the composite
	// foreign key, which pairs the reference with the child's OWN project.
	otherProject := seedProject(t, dm, "cpj-"+strings.Repeat("e", 16))
	assignmentMustRefuse(t, db, "a child moved to another project",
		`UPDATE agent_tasks SET project_id = ? WHERE id = ?`, otherProject, child)

	// A CYCLE. Two children pointed at each other would satisfy every
	// other condition — the root is a root, the parent is in the same
	// tree, and nobody is their own parent — and the "tree" would have a
	// loop in it. The second review found this: the depth condition is
	// what excludes it, because following the parent chain must strictly
	// DECREASE the depth, so the chain cannot come back to its start.
	assignmentMustRefuseWith(t, db, "the first half of a cycle",
		"the delegation tree is malformed", `
		UPDATE agent_tasks SET parent_id = ? WHERE id = ?`, sibling, child)
	assignmentMustRefuseWith(t, db, "the second half of a cycle",
		"the delegation tree is malformed", `
		UPDATE agent_tasks SET parent_id = ? WHERE id = ?`, child, sibling)

	// And a child's depth must be its parent's depth plus one. A child
	// that claims a depth of five under a parent at depth zero is not in
	// the tree at all — and the depth is IMMUTABLE, so the write is
	// refused by the depth rule rather than by the tree rule.
	assignmentMustRefuseWith(t, db, "a child claiming a depth its parent does not have",
		"the depth is immutable", `
		UPDATE agent_tasks SET depth = 5 WHERE id = ?`, child)

	// On the INSERT path there is no depth to change, so the tree rule
	// is what refuses it.
	assignmentMustRefuseWith(t, db, "a child INSERTED with a depth its parent does not have",
		"the delegation tree is malformed", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key
		) VALUES ('at-wrongdepth', ?, ?, ?, 5, 't', 'o', 'delta', 'implementer',
			'enqueued', 'idem:wrongdepth')
	`, projectID, root, root)

	// And the child's own tree is intact after both refusals: a refused
	// write must leave the row exactly as it was.
	var rootID, project string
	var depth int64
	if err := db.QueryRow(
		`SELECT root_id, project_id, depth FROM agent_tasks WHERE id = ?`, child,
	).Scan(&rootID, &project, &depth); err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if rootID != root || project != projectID || depth != 1 {
		t.Errorf("the child carries (root_id=%q, project_id=%q, depth=%d) after the refusals, "+
			"want (%q, %q, 1)", rootID, project, depth, root, projectID)
	}
}

// TestAssignmentTreeIsContainedInOneProject pins the containment boundary.
//
// The project id is denormalised onto the assignment and held honest by a
// COMPOSITE foreign key. So a child cannot name a root of another project,
// and it cannot name a parent of another project — the boundary is a
// property of the storage, not of the code that happens to respect it.
//
// The second review found that the two probes used the SAME foreign root
// AND the SAME foreign parent, so either foreign key alone made the test
// pass: removing one of them would not have been noticed. The two probes
// are separate here — each moves exactly ONE reference across the
// boundary.
func TestAssignmentTreeIsContainedInOneProject(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectA := seedProject(t, dm, "cpj-"+strings.Repeat("a", 16))
	projectB := seedProject(t, dm, "cpj-"+strings.Repeat("b", 16))

	rootA := insertRootAssignment(t, dm, projectA, "at-root-a", "alpha", "implementer", "idem:root-a")
	childA := insertChildAssignment(t, dm, projectA, "at-child-a", rootA, rootA, "charlie", "idem:child-a")

	// A child in project B whose ROOT is in project A. The parent is in
	// project A too, so this probe violates BOTH composite foreign keys:
	// it is the TREE-SHAPE TRIGGER that refuses it, and the composite
	// keys are what refuse the DELETE of a root that has a child (see
	// TestAssignmentDeletionIsRestricted). The two layers cover
	// different paths, and the control harness measures both.
	assignmentMustRefuse(t, db, "a child whose root is in another project", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key) VALUES ('at-crossroot', ?, ?, ?, 1, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:crossroot')
	`, projectB, rootA, childA)

	// A child in project B whose PARENT is in project A. The root is in
	// project A as well — and that is fine here, because the composite
	// key pairs it with the child's OWN project, so the root reference is
	// already broken by the child's project. To isolate the PARENT
	// foreign key, the root reference must be valid: so the child names
	// a root of project B and a parent of project A.
	rootB := insertRootAssignment(t, dm, projectB, "at-root-b", "alpha", "implementer", "idem:root-b")
	assignmentMustRefuse(t, db, "a child whose parent is in another project", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key) VALUES ('at-crossparent', ?, ?, ?, 1, 't', 'o', 'charlie', 'implementer',
			'enqueued', 'idem:crossparent')
	`, projectB, rootB, childA)

	// An assignment in a project that does not exist is refused.
	assignmentMustRefuse(t, db, "an assignment in a project that does not exist", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-noproject', 'cpj-' || 'cccccccccccccccc', 'at-noproject', NULL, 0, 't', 'o',
			'alpha', 'implementer', 'enqueued', 'idem:noproject', 1, 4, 2, 100, '["read"]', '["alpha"]')
	`)
}

// TestAssignmentDeletionIsRestricted pins the ON DELETE RESTRICT.
//
// A project with an assignment cannot be deleted, and a root with a child
// cannot be deleted. Without that, the ledger could be destroyed by a
// cascade.
//
// The second review found that the ledger's delete probe targeted a root
// that was ALREADY blocked by a child, so it passed for the wrong reason:
// the ledger's own foreign key was never exercised. The probe here targets
// a CHILDLESS task that has a ledger entry.
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

	// A CHILDLESS task with a ledger entry: the ledger's own foreign key
	// is what refuses the delete here, and nothing else.
	lonely := insertRootAssignment(t, dm, projectID, "at-dellonely", "alpha", "implementer", "idem:dellonely")
	assignmentMustExec(t, db, `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-del-1', ?, 0, 'created')`, lonely)
	assignmentMustRefuse(t, db, "deleting a childless task that has a ledger entry",
		`DELETE FROM agent_tasks WHERE id = ?`, lonely)
}

// TestAssignmentClaimedTaskCannotBeDeleted pins the epoch's lifetime.
//
// An assignment is CANCELLED, never deleted. And the rule is stated in
// terms of the claim epoch, because that is what closes the hole the second
// review found: an unreferenced task could be deleted and re-INSERTED,
// which resets its epoch — and a reset epoch makes a STALE token current
// again.
//
// A task that was EVER claimed holds an epoch, and a task that holds an
// epoch cannot be deleted, so its id cannot be reused.
func TestAssignmentClaimedTaskCannotBeDeleted(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	claimed := insertRootAssignment(t, dm, projectID, "at-claimed", "alpha", "implementer", "idem:claimed")
	assignmentMustExec(t, db, `
		UPDATE agent_tasks
		SET claim_epoch = claim_epoch + 1, owner = 'alpha', lease_expires_at = 1900000000
		WHERE id = ?`, claimed)

	assignmentMustRefuseWith(t, db, "deleting a claimed assignment",
		"is cancelled, never deleted", `
		DELETE FROM agent_tasks WHERE id = ?`, claimed)

	// An UNCLAIMED task may still be removed — it never issued a token,
	// so there is no stale token to revive.
	unclaimed := insertRootAssignment(t, dm, projectID, "at-unclaimed-del", "alpha", "implementer", "idem:unclaimed-del")
	assignmentMustExec(t, db, `DELETE FROM agent_tasks WHERE id = ?`, unclaimed)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_tasks WHERE id = ?`, unclaimed).Scan(&n); err != nil {
		t.Fatalf("count the deleted assignment: %v", err)
	}
	if n != 0 {
		t.Errorf("the unclaimed assignment survived the delete (count %d)", n)
	}
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
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-idem-2', ?, 'at-idem-2', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'github:issue:253', 1, 4, 2, 100, '["read"]', '["alpha"]')
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

// TestAssignmentLedgerIsOrderedAndMonotonic pins the ledger's shape.
//
// The event index is UNIQUE per assignment and strictly increasing, so the
// ledger is ordered and monotonic.
//
// The second review found that the first version tested a DUPLICATE index
// and called it a gap-free ledger: the ledger permitted GAPS, and it
// permitted a REWRITE and a DELETION, while the contract called it
// append-only. So the vocabulary changed — the ledger is MONOTONIC, not
// gap-free, because a writer that reserves an index and then dies leaves a
// hole — and the append-only property is now ENFORCED, not promised.
func TestAssignmentLedgerIsOrderedAndMonotonic(t *testing.T) {
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

	// A duplicate index is refused.
	assignmentMustRefuse(t, db, "a duplicate event index for the same assignment", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-l3', ?, 1, 'acknowledged')`, id)

	// A REWRITE is refused: the ledger is append-only.
	assignmentMustRefuseWith(t, db, "a rewrite of a ledger entry",
		"the ledger is append-only", `
		UPDATE agent_task_events SET event_type = 'note' WHERE id = 'ate-l1'`)

	// A DELETION is refused: the ledger is append-only.
	assignmentMustRefuseWith(t, db, "the deletion of a ledger entry",
		"the ledger is append-only", `
		DELETE FROM agent_task_events WHERE id = 'ate-l1'`)

	// An index that does not advance is refused.
	assignmentMustRefuseWith(t, db, "a ledger entry whose index does not advance",
		"the event index must be strictly greater than the current maximum", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-l4', ?, 0, 'note')`, id)

	// A SKIPPED index is allowed — the ledger is monotonic, not gap-free,
	// because a writer that reserves an index and then dies leaves a hole.
	assignmentMustExec(t, db, `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-l5', ?, 7, 'note')`, id)

	// A ledger entry for an assignment that does not exist is refused.
	assignmentMustRefuse(t, db, "a ledger entry for an assignment that does not exist", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-l6', 'at-no-such-task', 0, 'created')`)

	// A ledger entry with an unknown event type is refused.
	assignmentMustRefuse(t, db, "a ledger entry with an unknown event type", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-l7', ?, 9, 'vibed')`, id)
}

// TestAssignmentOperatorGateNamesItsAction pins the operator's gate.
//
// The operator's decision (external authority, internal attribution) says
// that an approval request must identify the EXACT action and the target
// revision, and that a recorded approval must identify its action, its
// revision and its source. Without them there is nothing for a staleness
// check to compare against, and the gate would be satisfiable by a bare
// transition.
//
// The second review found that `operator_approved` was reachable with every
// approval field NULL.
func TestAssignmentOperatorGateNamesItsAction(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// The implementer's path up to the gate.
	id := insertRootAssignment(t, dm, projectID, "at-gate", "alpha", "implementer", "idem:gate")
	for _, step := range []string{"delivered", "acknowledged", "accepted", "in_progress",
		"completed", "review_passed"} {
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step, id)
	}

	// The gate refuses a transition that names no action.
	assignmentMustRefuseWith(t, db, "the operator's gate with no named action",
		"must name the exact action it awaits", `
		UPDATE agent_tasks SET state = 'awaiting_operator' WHERE id = ?`, id)

	// And with the action named, it lands.
	assignmentMustExec(t, db, `
		UPDATE agent_tasks
		SET state = 'awaiting_operator', awaiting_action = 'merge PR #259'
		WHERE id = ?`, id)

	// The approval refuses a transition that names no action, revision or
	// source.
	assignmentMustRefuseWith(t, db, "an approval with no named action, revision or source",
		"must name its action, its revision and its source", `
		UPDATE agent_tasks SET state = 'operator_approved' WHERE id = ?`, id)

	// With all three, and the evidence pointer, it lands.
	assignmentMustExec(t, db, `
		UPDATE agent_tasks
		SET state = 'operator_approved',
		    approval_action = 'merge PR #259',
		    approval_revision = 'abc123',
		    approval_source = 'operator',
		    approval_evidence = 'docs/evidence/at-gate/manifest.json'
		WHERE id = ?`, id)

	var action, revision, source string
	if err := db.QueryRow(`
		SELECT approval_action, approval_revision, approval_source
		  FROM agent_tasks WHERE id = ?`, id).
		Scan(&action, &revision, &source); err != nil {
		t.Fatalf("read the approval: %v", err)
	}
	if action != "merge PR #259" || revision != "abc123" || source != "operator" {
		t.Errorf("the approval carries (%q, %q, %q), want the recorded values", action, revision, source)
	}

	// The gate's record is EVIDENCE, and evidence is not rewritten: the
	// action the operator is being asked to authorise cannot be cleared
	// once the assignment is at the gate.
	gated := insertRootAssignment(t, dm, projectID, "at-gate2", "alpha", "implementer", "idem:gate2")
	for _, step := range []string{"delivered", "acknowledged", "accepted", "in_progress",
		"completed", "review_passed"} {
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step, gated)
	}
	assignmentMustExec(t, db, `
		UPDATE agent_tasks SET state = 'awaiting_operator', awaiting_action = 'merge PR #259'
		WHERE id = ?`, gated)
	assignmentMustRefuseWith(t, db, "the awaited action being cleared at the gate",
		"is fixed once the gate is reached", `
		UPDATE agent_tasks SET awaiting_action = NULL WHERE id = ?`, gated)

	// And a recorded approval cannot have its source cleared.
	assignmentMustRefuseWith(t, db, "a recorded approval having its source cleared",
		"evidence is not rewritten", `
		UPDATE agent_tasks SET approval_source = NULL WHERE id = ?`, id)
	assignmentMustRefuseWith(t, db, "a recorded approval having its revision cleared",
		"evidence is not rewritten", `
		UPDATE agent_tasks SET approval_revision = '' WHERE id = ?`, id)
}

// TestAssignmentRootRecipientMustBeEligible pins the root's allow-list.
//
// The DDL says it in so many words: "A root's own recipient must be among
// them". Without the check, a root addressed to 'alpha' could carry an
// allow-list of ['bob'] — a root that is addressed to an agent the tree
// may not target — and every child of it would inherit an allow-list that
// excludes its own parent.
//
// It also closes an ASYMMETRY the second review found: an empty
// `permitted_operations` was refused for a root, and an empty
// `eligible_recipients` was accepted. Both are the same misconfiguration.
func TestAssignmentRootRecipientMustBeEligible(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// A root whose recipient is not among its own eligible recipients.
	assignmentMustRefuseWith(t, db, "a root whose recipient is not eligible",
		"a root's own recipient must be among its eligible recipients", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units,
			permitted_operations, eligible_recipients
		) VALUES ('at-noteligible', ?, 'at-noteligible', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:noteligible', 1, 4, 2, 100, '["read"]', '["bob"]')
	`, projectID)

	// A root with an EMPTY eligible_recipients.
	assignmentMustRefuseWith(t, db, "a root with no eligible recipients",
		"a root's own recipient must be among its eligible recipients", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units,
			permitted_operations, eligible_recipients
		) VALUES ('at-norecipients', ?, 'at-norecipients', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:norecipients', 1, 4, 2, 100, '["read"]', '[]')
	`, projectID)

	// And the UPDATE path: the allow-list cannot be narrowed to exclude
	// the root's own recipient.
	id := insertRootAssignment(t, dm, projectID, "at-eligible", "alpha", "implementer", "idem:eligible")
	assignmentMustRefuseWith(t, db, "a root's allow-list narrowed to exclude its recipient",
		"a root's own recipient must be among its eligible recipients", `
		UPDATE agent_tasks SET eligible_recipients = '["bob"]' WHERE id = ?`, id)

	// Nor can the recipient be moved out of the allow-list.
	assignmentMustRefuseWith(t, db, "a root's recipient moved out of its allow-list",
		"a root's own recipient must be among its eligible recipients", `
		UPDATE agent_tasks SET recipient = 'mallory' WHERE id = ?`, id)

	// The well-formed root is still accepted, and its allow-list names
	// its recipient.
	var eligible string
	if err := db.QueryRow(
		`SELECT eligible_recipients FROM agent_tasks WHERE id = ?`, id).Scan(&eligible); err != nil {
		t.Fatalf("read the root: %v", err)
	}
	if !strings.Contains(eligible, "alpha") {
		t.Errorf("the root's eligible recipients are %s, want them to name its recipient", eligible)
	}
}

// TestAssignmentGateRecordIsImmutable pins the gate's record.
//
// The second review found that the record was only protected against
// being CLEARED, not against being REWRITTEN: the approval's revision
// could be re-pointed at a different value, which defeats the staleness
// comparison against the head revision, and the action the operator is
// asked to authorise could be swapped for another one.
//
// So the rule is now: once the gate is reached, neither record changes —
// not to a different value, not to nothing.
func TestAssignmentGateRecordIsImmutable(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	id := insertRootAssignment(t, dm, projectID, "at-immutable", "alpha", "implementer", "idem:immutable")
	for _, step := range []string{"delivered", "acknowledged", "accepted", "in_progress",
		"completed", "review_passed"} {
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step, id)
	}
	assignmentMustExec(t, db, `
		UPDATE agent_tasks SET state = 'awaiting_operator', awaiting_action = 'merge PR #259'
		WHERE id = ?`, id)

	// The awaited action cannot be REWRITTEN while the gate is reached.
	assignmentMustRefuseWith(t, db, "the awaited action rewritten at the gate",
		"is fixed once the gate is reached", `
		UPDATE agent_tasks SET awaiting_action = 'merge PR #999' WHERE id = ?`, id)

	// Nor cleared.
	assignmentMustRefuseWith(t, db, "the awaited action cleared at the gate",
		"is fixed once the gate is reached", `
		UPDATE agent_tasks SET awaiting_action = NULL WHERE id = ?`, id)

	// The approval is recorded, and from then on it is EVIDENCE.
	assignmentMustExec(t, db, `
		UPDATE agent_tasks
		SET state = 'operator_approved',
		    approval_action = 'merge PR #259',
		    approval_revision = 'abc123',
		    approval_source = 'operator'
		WHERE id = ?`, id)

	assignmentMustRefuseWith(t, db, "the recorded approval's revision rewritten",
		"evidence is not rewritten", `
		UPDATE agent_tasks SET approval_revision = 'totally-different' WHERE id = ?`, id)
	assignmentMustRefuseWith(t, db, "the recorded approval's action rewritten",
		"evidence is not rewritten", `
		UPDATE agent_tasks SET approval_action = 'merge PR #999' WHERE id = ?`, id)
	assignmentMustRefuseWith(t, db, "the recorded approval's source rewritten",
		"evidence is not rewritten", `
		UPDATE agent_tasks SET approval_source = 'mallory' WHERE id = ?`, id)

	// And it cannot be cleared either.
	assignmentMustRefuseWith(t, db, "the recorded approval's revision cleared",
		"evidence is not rewritten", `
		UPDATE agent_tasks SET approval_revision = NULL WHERE id = ?`, id)

	// The action is still fixed after the gate has been passed.
	assignmentMustRefuseWith(t, db, "the awaited action cleared after the gate",
		"is fixed once the gate is reached", `
		UPDATE agent_tasks SET awaiting_action = NULL WHERE id = ?`, id)

	// And the record is intact.
	var action, revision, source, awaited string
	if err := db.QueryRow(`
		SELECT approval_action, approval_revision, approval_source, awaiting_action
		  FROM agent_tasks WHERE id = ?`, id).
		Scan(&action, &revision, &source, &awaited); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if action != "merge PR #259" || revision != "abc123" || source != "operator" ||
		awaited != "merge PR #259" {
		t.Errorf("the record carries (%q, %q, %q, %q) after the refusals, want the recorded values",
			action, revision, source, awaited)
	}
}

// TestAssignmentIllegalTransitionIsReportedAsSuch pins the ORDER of the
// checks.
//
// The state machine's trigger carries the path check and the gate's field
// rules, with the path checked FIRST — because the order in which SQLite
// fires SEPARATE triggers on the same event is not specified. The second
// review found that the rule was still not true: two SEPARATE triggers
// (the gate's immutability rules) competed with it, and an illegal
// transition was reported as a cleared gate.
//
// The fix is that the gate's immutability rules fire only while the state
// is UNCHANGED. So a write that also moves the state is governed by the
// state machine alone, and the refusal for an illegal transition is an
// illegal transition.
func TestAssignmentIllegalTransitionIsReportedAsSuch(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	id := insertRootAssignment(t, dm, projectID, "at-order", "alpha", "implementer", "idem:order")
	for _, step := range []string{"delivered", "acknowledged", "accepted", "in_progress",
		"completed", "review_passed"} {
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step, id)
	}
	assignmentMustExec(t, db, `
		UPDATE agent_tasks SET state = 'awaiting_operator', awaiting_action = 'merge PR #259'
		WHERE id = ?`, id)

	// An illegal transition that ALSO touches the awaited action is
	// reported as an illegal transition, not as a cleared gate.
	assignmentMustRefuseWith(t, db, "an illegal transition that also clears the action",
		"illegal state transition", `
		UPDATE agent_tasks SET state = 'enqueued', awaiting_action = NULL WHERE id = ?`, id)
}

// TestAssignmentLedgerStatesComeFromTheVocabulary pins the ledger's states.
//
// The second review found that `from_state` and `to_state` carried no
// vocabulary, so the ledger could record a state the state machine does
// not have. A ledger that can record a state that does not exist is a
// ledger that can lie.
func TestAssignmentLedgerStatesComeFromTheVocabulary(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	id := insertRootAssignment(t, dm, projectID, "at-ledgervocab", "alpha", "implementer", "idem:ledgervocab")

	assignmentMustRefuse(t, db, "a ledger entry with a state outside the vocabulary", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type, from_state, to_state)
		VALUES ('ate-bogus', ?, 0, 'created', 'bogus', 'also-bogus')`, id)

	assignmentMustRefuse(t, db, "a ledger entry whose to_state is outside the vocabulary", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type, from_state, to_state)
		VALUES ('ate-bogus2', ?, 0, 'created', 'enqueued', 'pondering')`, id)

	// And the real states are accepted.
	assignmentMustExec(t, db, `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type, from_state, to_state)
		VALUES ('ate-real', ?, 0, 'created', 'enqueued', 'delivered')`, id)
}

// TestAssignmentTextColumnsRejectBlobs pins the type of the evidence.
//
// The second review found that the `typeof()` guards covered the
// identifiers and the keys but not the evidence POINTERS and the gate's
// record — so a BLOB could be stored where a revision belongs. A BLOB
// compares unequal to every real revision, which is a staleness check
// that never fires.
func TestAssignmentTextColumnsRejectBlobs(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// A BLOB in the gate's action.
	assignmentMustRefuse(t, db, "a BLOB awaiting action", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units,
			permitted_operations, eligible_recipients, awaiting_action
		) VALUES ('at-blobaction', ?, 'at-blobaction', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:blobaction', 1, 4, 2, 100, '["read"]', '["alpha"]', x'616263')
	`, projectID)

	// A BLOB in an evidence pointer.
	assignmentMustRefuse(t, db, "a BLOB head revision", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units,
			permitted_operations, eligible_recipients, head_sha
		) VALUES ('at-blobsha', ?, 'at-blobsha', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:blobsha', 1, 4, 2, 100, '["read"]', '["alpha"]', x'616263')
	`, projectID)

	// A BLOB in the ledger's attribution.
	id := insertRootAssignment(t, dm, projectID, "at-blobactor", "alpha", "implementer", "idem:blobactor")
	assignmentMustRefuse(t, db, "a BLOB actor address in the ledger", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type, actor_address)
		VALUES ('ate-blobactor', ?, 0, 'created', x'616263')`, id)
}

// TestAssignmentPermittedOperationsMustBeStrings pins the operation
// vocabulary.
//
// The second review found that the element guard tested `typeof(value)`,
// and that for a NON-SCALAR element json_each's `value` column is the
// element's JSON SERIALISATION — which is TEXT. So an object or an array
// passed as a "permitted operation". The discriminator has to be
// json_each's TYPE column.
func TestAssignmentPermittedOperationsMustBeStrings(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	for _, ops := range []string{
		`["a",{"b":1}]`,  // an object
		`[["read"]]`,     // an array
		`[true]`,         // a boolean
		`[7]`,            // a number
		`[null]`,         // a null
	} {
		assignmentMustRefuseWith(t, db, "a permitted operation that is not a string ("+ops+")",
			"a permitted operation must be a non-null string", `
			INSERT INTO agent_tasks (
				id, project_id, root_id, parent_id, depth, title, objective,
				recipient, role, state, idempotency_key,
				max_depth, max_tasks, max_concurrency, max_resource_units,
				permitted_operations, eligible_recipients
			) VALUES ('at-badop', ?, 'at-badop', NULL, 0, 't', 'o', 'alpha', 'implementer',
				'enqueued', 'idem:badop', 1, 4, 2, 100, ?, '["alpha"]')
		`, projectID, ops)
	}

	// And the UPDATE path.
	id := insertRootAssignment(t, dm, projectID, "at-badopupd", "alpha", "implementer", "idem:badopupd")
	assignmentMustRefuseWith(t, db, "a permitted operation replaced by an object",
		"a permitted operation must be a non-null string", `
		UPDATE agent_tasks SET permitted_operations = '["a",{"b":1}]' WHERE id = ?`, id)

	// A plain list of strings is accepted.
	assignmentMustExec(t, db, `
		UPDATE agent_tasks SET permitted_operations = '["read","write"]' WHERE id = ?`, id)
}

// TestAssignmentTreeCannotBeMadeIntoACycle pins the tree against a cycle.
//
// The second review's finding was that a cycle could be built by two
// UPDATEs that each changed a depth, and the third review found that the
// fix — "the parent's depth must be exactly one less" — did NOT close it,
// because a BEFORE trigger sees only the row being written. Two statements
// could each satisfy the condition against the other statement's new
// depth, and the parent chain would never terminate.
//
// The fix is the depth's IMMUTABILITY: a depth that never changes makes
// the chain strictly decreasing by construction.
//
// The test covers the exact construction the third review used.
func TestAssignmentTreeCannotBeMadeIntoACycle(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	root := insertRootAssignment(t, dm, projectID, "at-cycroot", "alpha", "implementer", "idem:cycroot")
	c1 := insertChildAssignment(t, dm, projectID, "at-cyc1", root, root, "charlie", "idem:cyc1")
	c2 := insertChildAssignment(t, dm, projectID, "at-cyc2", root, root, "delta", "idem:cyc2")

	// The third review's construction: each statement raises a child's
	// depth AND points it at the other child.
	assignmentMustRefuse(t, db, "the first half of a depth-shifting cycle", `
		UPDATE agent_tasks SET depth = 2, parent_id = ? WHERE id = ?`, c2, c1)
	assignmentMustRefuse(t, db, "the second half of a depth-shifting cycle", `
		UPDATE agent_tasks SET depth = 3, parent_id = ? WHERE id = ?`, c1, c2)

	// And the depth cannot be changed at all, on its own.
	assignmentMustRefuseWith(t, db, "a child's depth raised on its own",
		"the depth is immutable", `
		UPDATE agent_tasks SET depth = 5 WHERE id = ?`, c1)

	// The tree is intact: the two children still hang from the root.
	var parent1, parent2 string
	var depth1, depth2 int64
	if err := db.QueryRow(
		`SELECT parent_id, depth FROM agent_tasks WHERE id = ?`, c1,
	).Scan(&parent1, &depth1); err != nil {
		t.Fatalf("read the first child: %v", err)
	}
	if err := db.QueryRow(
		`SELECT parent_id, depth FROM agent_tasks WHERE id = ?`, c2,
	).Scan(&parent2, &depth2); err != nil {
		t.Fatalf("read the second child: %v", err)
	}
	if parent1 != root || parent2 != root || depth1 != 1 || depth2 != 1 {
		t.Errorf("the children are (%q,%d) and (%q,%d) after the refusals, want (%q,1) and (%q,1)",
			parent1, depth1, parent2, depth2, root, root)
	}
}

// TestAssignmentChildCannotBePromotedToARoot pins the promotion hole.
//
// The third review found that both tree-shape triggers carry
// `WHEN NEW.depth > 0`, so a write that set the depth to zero was checked
// by NEITHER of them: a child could be rewritten into a root, and its own
// descendants would be left with a parent whose root and depth no longer
// matched theirs — with no trigger re-checking a row it did not write.
//
// The depth's immutability closes it.
func TestAssignmentChildCannotBePromotedToARoot(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	root := insertRootAssignment(t, dm, projectID, "at-promroot", "alpha", "implementer", "idem:promroot")
	child := insertChildAssignment(t, dm, projectID, "at-promchild", root, root, "charlie", "idem:promchild")
	// The grandchild sits one level below the child, so it is written
	// with the depth its parent implies.
	assignmentMustExec(t, db, `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key
		) VALUES ('at-promgrand', ?, ?, ?, 2, 't', 'o', 'delta', 'implementer',
			'enqueued', 'idem:promgrand')
	`, projectID, root, child)

	// The third review's construction: promote the child to a root,
	// carrying the budgets a root needs.
	assignmentMustRefuse(t, db, "a child promoted to a root", `
		UPDATE agent_tasks
		SET depth = 0, parent_id = NULL, root_id = ?,
		    max_depth = 3, max_tasks = 10, max_concurrency = 2, max_resource_units = 100,
		    permitted_operations = '["read"]', eligible_recipients = '["alpha"]'
		WHERE id = ?`, child, child)

	// And the depth alone.
	assignmentMustRefuse(t, db, "a child's depth lowered to zero", `
		UPDATE agent_tasks SET depth = 0 WHERE id = ?`, child)

	// The tree is intact.
	var depth int64
	var parent sql.NullString
	if err := db.QueryRow(
		`SELECT parent_id, depth FROM agent_tasks WHERE id = ?`, child,
	).Scan(&parent, &depth); err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if !parent.Valid || parent.String != root || depth != 1 {
		t.Errorf("the child carries (parent=%v, depth=%d) after the refusals, want (%q, 1)",
			parent, depth, root)
	}
	var grandDepth int64
	if err := db.QueryRow(
		`SELECT depth FROM agent_tasks WHERE id = ?`, "at-promgrand",
	).Scan(&grandDepth); err != nil {
		t.Fatalf("read the grandchild: %v", err)
	}
	if grandDepth != 2 {
		t.Errorf("the grandchild is at depth %d, want 2", grandDepth)
	}
}

// TestAssignmentCannotBeReplaced pins the INSERT OR REPLACE hole.
//
// The third review found that EVERY rule enforced by a BEFORE UPDATE or a
// BEFORE DELETE trigger was defeated by `INSERT OR REPLACE`, because
// REPLACE is DELETE + INSERT: the UPDATE triggers never fire, and SQLite
// does not fire the DELETE triggers for REPLACE's implicit delete unless
// the recursive_triggers pragma is on — which MPM does not set.
//
// So a claimed task could be re-inserted with its epoch reset to zero,
// and a ledger entry could be destroyed outright.
//
// The defence is a BEFORE INSERT trigger that refuses an INSERT whose id
// or whose idempotency key already exists: SQLite runs the trigger
// BEFORE the conflict resolution, so the conflicting row is still there
// and the REPLACE is refused as a whole.
func TestAssignmentCannotBeReplaced(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// A CLAIMED task. Replacing it would reset the epoch to zero and
	// make a stale token current again.
	claimed := insertRootAssignment(t, dm, projectID, "at-replace", "alpha", "implementer", "idem:replace")
	assignmentMustExec(t, db, `
		UPDATE agent_tasks
		SET claim_epoch = claim_epoch + 1, owner = 'alpha', lease_expires_at = 1900000000
		WHERE id = ?`, claimed)

	assignmentMustRefuse(t, db, "a claimed task replaced by a fresh row", `
		INSERT OR REPLACE INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units,
			permitted_operations, eligible_recipients
		) VALUES (?, ?, ?, NULL, 0, 't', 'o', 'alpha', 'reviewer', 'enqueued', ?,
			1, 4, 2, 100, '["read"]', '["alpha"]')
	`, claimed, projectID, claimed, "idem:replace")

	// The claimed task is untouched.
	var role, state string
	var epoch int64
	var owner sql.NullString
	if err := db.QueryRow(
		`SELECT role, state, claim_epoch, owner FROM agent_tasks WHERE id = ?`, claimed,
	).Scan(&role, &state, &epoch, &owner); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if role != "implementer" || state != "enqueued" || epoch != 1 || !owner.Valid {
		t.Errorf("the task carries (role=%q, state=%q, epoch=%d, owner=%v) after the refused "+
			"REPLACE, want (implementer, enqueued, 1, set)", role, state, epoch, owner)
	}

	// A replacement that conflicts on the IDEMPOTENCY KEY rather than
	// the id would destroy the OTHER row, so the key is guarded too.
	assignmentMustRefuse(t, db, "a task replaced through the idempotency key", `
		INSERT OR REPLACE INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units,
			permitted_operations, eligible_recipients
		) VALUES ('at-replace-other', ?, 'at-replace-other', NULL, 0, 't', 'o', 'alpha',
			'implementer', 'enqueued', ?,
			1, 4, 2, 100, '["read"]', '["alpha"]')
	`, projectID, "idem:replace")

	// And the LEDGER. A replaced ledger entry would be destroyed, and
	// the append-only triggers would never fire.
	assignmentMustExec(t, db, `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-replace', ?, 0, 'created')`, claimed)
	assignmentMustRefuse(t, db, "a ledger entry replaced", `
		INSERT OR REPLACE INTO agent_task_events (id, task_id, event_index, event_type, note)
		VALUES ('ate-replace', ?, 1, 'note', 'REWRITTEN')`, claimed)

	// The original entry is intact.
	var eventType string
	if err := db.QueryRow(
		`SELECT event_type FROM agent_task_events WHERE id = ?`, "ate-replace",
	).Scan(&eventType); err != nil {
		t.Fatalf("read the ledger entry: %v", err)
	}
	if eventType != "created" {
		t.Errorf("the ledger entry is %q after the refused REPLACE, want created", eventType)
	}
}

// TestAssignmentCannotBeReplacedByAnUpdate pins the OTHER half of the
// replacement hole.
//
// The fourth review found that the INSERT guard was not enough: an
// `UPDATE OR REPLACE` resolves a uniqueness conflict by DELETING the
// conflicting row, and SQLite does not fire the DELETE triggers for that
// implicit delete either — unless the recursive_triggers pragma is on,
// which MPM does not set.
//
// So a CLAIMED assignment could be destroyed by an UPDATE that merely set
// a colliding idempotency key: its id was freed, and a stale token
// became current again. That is exactly the hole the first review closed,
// reachable by a different statement.
//
// The defence is a BEFORE UPDATE trigger that refuses a change that would
// collide with another row's id or idempotency key: a BEFORE trigger can
// see the collision before the statement runs, so the conflict is refused
// rather than resolved.
func TestAssignmentCannotBeReplacedByAnUpdate(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// The victim: claimed, so it holds an epoch and an owner.
	victim := insertRootAssignment(t, dm, projectID, "at-victim", "alpha", "implementer", "key-victim")
	assignmentMustExec(t, db, `
		UPDATE agent_tasks
		SET claim_epoch = claim_epoch + 1, owner = 'alpha', lease_expires_at = 1900000000
		WHERE id = ?`, victim)

	// The other row, which will try to take the victim's key.
	other := insertRootAssignment(t, dm, projectID, "at-other", "alpha", "implementer", "key-other")

	assignmentMustRefuse(t, db, "a claimed assignment destroyed by an UPDATE OR REPLACE", `
		UPDATE OR REPLACE agent_tasks SET idempotency_key = 'key-victim' WHERE id = ?`, other)

	// The victim is intact: its row, its epoch and its owner.
	var epoch int64
	var owner sql.NullString
	if err := db.QueryRow(
		`SELECT claim_epoch, owner FROM agent_tasks WHERE id = ?`, victim,
	).Scan(&epoch, &owner); err != nil {
		t.Fatalf("read the victim: %v", err)
	}
	if epoch != 1 || !owner.Valid || owner.String != "alpha" {
		t.Errorf("the victim carries (epoch=%d, owner=%v) after the refused UPDATE, "+
			"want (1, alpha)", epoch, owner)
	}

	// And the id cannot be reused afterwards.
	assignmentMustRefuse(t, db, "the victim's id re-used after the refused UPDATE", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units,
			permitted_operations, eligible_recipients
		) VALUES (?, ?, ?, NULL, 0, 't', 'o', 'alpha', 'implementer', 'enqueued', ?,
			1, 4, 2, 100, '["read"]', '["alpha"]')
	`, victim, projectID, victim, "key-victim-2")

	// An ordinary key change still works — the rule is about a COLLISION,
	// not about changing a key.
	assignmentMustExec(t, db, `
		UPDATE agent_tasks SET idempotency_key = 'key-other-2' WHERE id = ?`, other)
	assignmentMustExec(t, db, `
		UPDATE agent_tasks SET idempotency_key = 'key-other-2' WHERE id = ?`, other)
}

// TestAssignmentGateRecordIsImmutableWhileLeavingTheGate pins the gate's
// record against the statement that LEAVES the gate.
//
// The second review closed the case of a write that does not move the
// state. The third review found the other case: the trigger fired only
// `WHEN OLD.state = NEW.state`, so the very statement that moves the
// assignment out of the gate could rewrite or clear the awaited action —
// and the operator would be asked to authorise "merge PR #259" while the
// row said "merge PR #999".
//
// The rule now lives inside the state machine's trigger, where the PATH
// is checked first, so an illegal transition is still reported as an
// illegal transition.
func TestAssignmentGateRecordIsImmutableWhileLeavingTheGate(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	id := insertRootAssignment(t, dm, projectID, "at-leavegate", "alpha", "implementer", "idem:leavegate")
	for _, step := range []string{"delivered", "acknowledged", "accepted", "in_progress",
		"completed", "review_passed"} {
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step, id)
	}
	assignmentMustExec(t, db, `
		UPDATE agent_tasks SET state = 'awaiting_operator', awaiting_action = 'merge PR #259'
		WHERE id = ?`, id)

	// The awaited action cannot be CLEARED on the way out.
	assignmentMustRefuseWith(t, db, "the awaited action cleared while approving",
		"is fixed once the gate is reached", `
		UPDATE agent_tasks
		SET state = 'operator_approved', awaiting_action = NULL,
		    approval_action = 'merge PR #259', approval_revision = 'abc123', approval_source = 'operator'
		WHERE id = ?`, id)

	// Nor REWRITTEN.
	assignmentMustRefuseWith(t, db, "the awaited action rewritten while approving",
		"is fixed once the gate is reached", `
		UPDATE agent_tasks
		SET state = 'operator_approved', awaiting_action = 'merge PR #999',
		    approval_action = 'merge PR #259', approval_revision = 'abc123', approval_source = 'operator'
		WHERE id = ?`, id)

	// Nor on the way to cancellation.
	assignmentMustRefuseWith(t, db, "the awaited action rewritten while cancelling",
		"is fixed once the gate is reached", `
		UPDATE agent_tasks SET state = 'cancelled', awaiting_action = 'merge PR #999' WHERE id = ?`, id)

	// The record is intact.
	var awaited string
	if err := db.QueryRow(
		`SELECT awaiting_action FROM agent_tasks WHERE id = ?`, id).Scan(&awaited); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if awaited != "merge PR #259" {
		t.Errorf("the awaited action is %q after the refusals, want %q", awaited, "merge PR #259")
	}
}

// TestAssignmentApprovalRecordIsImmutable covers every column of the
// approval.
//
// The second review closed the action, the revision and the source. The
// third review found that the evidence POINTER and the moment the
// approval was recorded were still freely rewritable — and the evidence
// pointer is the thing the published staleness and attribution check will
// be built on. An approval whose evidence can be swapped is not evidence.
func TestAssignmentApprovalRecordIsImmutable(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	id := insertRootAssignment(t, dm, projectID, "at-approvev", "alpha", "implementer", "idem:approvev")
	for _, step := range []string{"delivered", "acknowledged", "accepted", "in_progress",
		"completed", "review_passed"} {
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step, id)
	}
	assignmentMustExec(t, db, `
		UPDATE agent_tasks SET state = 'awaiting_operator', awaiting_action = 'merge PR #259'
		WHERE id = ?`, id)
	assignmentMustExec(t, db, `
		UPDATE agent_tasks
		SET state = 'operator_approved',
		    approval_action = 'merge PR #259',
		    approval_revision = 'abc123',
		    approval_source = 'operator',
		    approval_evidence = 'docs/evidence/at-approvev/manifest.json',
		    approved_at = 1700000000
		WHERE id = ?`, id)

	for _, tc := range []struct{ what, column, value string }{
		{"the evidence pointer rewritten", "approval_evidence", "'ev://TAMPERED'"},
		{"the evidence pointer cleared", "approval_evidence", "NULL"},
		{"the recorded moment rewritten", "approved_at", "999999"},
		{"the recorded moment cleared", "approved_at", "NULL"},
	} {
		assignmentMustRefuseWith(t, db, tc.what, "evidence is not rewritten", `
			UPDATE agent_tasks SET `+tc.column+` = `+tc.value+` WHERE id = ?`, id)
	}

	// The record is intact.
	var evidence string
	var approvedAt int64
	if err := db.QueryRow(
		`SELECT approval_evidence, approved_at FROM agent_tasks WHERE id = ?`, id,
	).Scan(&evidence, &approvedAt); err != nil {
		t.Fatalf("read the approval: %v", err)
	}
	if evidence != "docs/evidence/at-approvev/manifest.json" || approvedAt != 1700000000 {
		t.Errorf("the approval carries (%q, %d) after the refusals, want the recorded values",
			evidence, approvedAt)
	}
}

// TestAssignmentApprovalMustNameTheSameAction pins the equality rule.
//
// The gate names the exact action the operator is asked to authorise; the
// approval names the exact action that was authorised. If the two differ,
// the row does not say what was approved relative to what was asked — and
// the staleness comparison has nothing to compare.
func TestAssignmentApprovalMustNameTheSameAction(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	id := insertRootAssignment(t, dm, projectID, "at-sameaction", "alpha", "implementer", "idem:sameaction")
	for _, step := range []string{"delivered", "acknowledged", "accepted", "in_progress",
		"completed", "review_passed"} {
		assignmentMustExec(t, db, `UPDATE agent_tasks SET state = ? WHERE id = ?`, step, id)
	}
	assignmentMustExec(t, db, `
		UPDATE agent_tasks SET state = 'awaiting_operator', awaiting_action = 'merge PR #259'
		WHERE id = ?`, id)

	assignmentMustRefuseWith(t, db, "an approval for a different action",
		"must name the SAME action the gate awaited", `
		UPDATE agent_tasks
		SET state = 'operator_approved', approval_action = 'DEPLOY TO PRODUCTION',
		    approval_revision = 'abc123', approval_source = 'operator'
		WHERE id = ?`, id)

	// And the matching one is accepted.
	assignmentMustExec(t, db, `
		UPDATE agent_tasks
		SET state = 'operator_approved', approval_action = 'merge PR #259',
		    approval_revision = 'abc123', approval_source = 'operator'
		WHERE id = ?`, id)
}

// TestAssignmentTimestampsAreTyped pins the timestamps' type.
//
// The third review found that the three timestamp columns carried no
// typeof() guard, while the DDL's own header claimed that every column's
// type is guarded. A TEXT updated_at sorts after every INTEGER in the
// three indexes that order by it, so a corrupted timestamp makes its row
// look like the newest one.
func TestAssignmentTimestampsAreTyped(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	id := insertRootAssignment(t, dm, projectID, "at-stamps", "alpha", "implementer", "idem:stamps")

	for _, tc := range []struct{ what, column, value string }{
		{"a TEXT created_at", "created_at", "'not-a-time'"},
		{"a REAL created_at", "created_at", "1.5"},
		{"a TEXT updated_at", "updated_at", "'not-a-time'"},
		{"a REAL updated_at", "updated_at", "1e300"},
		{"a BLOB updated_at", "updated_at", "x'00'"},
	} {
		assignmentMustRefuse(t, db, tc.what,
			`UPDATE agent_tasks SET `+tc.column+` = `+tc.value+` WHERE id = ?`, id)
	}

	// And the ledger's timestamp.
	assignmentMustRefuse(t, db, "a TEXT ledger timestamp", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type, created_at)
		VALUES ('ate-stamp', ?, 0, 'created', 'not-a-time')`, id)
}

// TestAssignmentEligibleRecipientsMustBeStrings pins the allow-list's
// elements.
//
// The permitted operations had an element guard; the eligible recipients
// did not, so a root could carry an allow-list holding an object, a
// boolean, a number or a null — and PR 2's membership check will consult
// that list.
func TestAssignmentEligibleRecipientsMustBeStrings(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	for _, list := range []string{
		`[{"a":1},"alpha"]`,  // an object
		`[["alpha"]]`,        // an array
		`[true,"alpha"]`,     // a boolean
		`[42,"alpha"]`,       // a number
		`[null,"alpha"]`,     // a null
		`["","alpha"]`,       // an empty address
	} {
		assignmentMustRefuseWith(t, db, "an eligible recipient that is not an address ("+list+")",
			"an eligible recipient must be a non-null string", `
			INSERT INTO agent_tasks (
				id, project_id, root_id, parent_id, depth, title, objective,
				recipient, role, state, idempotency_key,
				max_depth, max_tasks, max_concurrency, max_resource_units,
				permitted_operations, eligible_recipients
			) VALUES ('at-badelig', ?, 'at-badelig', NULL, 0, 't', 'o', 'alpha', 'implementer',
				'enqueued', 'idem:badelig', 1, 4, 2, 100, '["read"]', ?)
		`, projectID, list)
	}

	// The UPDATE path.
	id := insertRootAssignment(t, dm, projectID, "at-eligupd", "alpha", "implementer", "idem:eligupd")
	assignmentMustRefuseWith(t, db, "an allow-list holding an object",
		"an eligible recipient must be a non-null string", `
		UPDATE agent_tasks SET eligible_recipients = '[{"a":1},"alpha"]' WHERE id = ?`, id)

	// And a plain list of addresses is accepted.
	assignmentMustExec(t, db, `
		UPDATE agent_tasks SET eligible_recipients = '["alpha","beta"]' WHERE id = ?`, id)
}

// TestAssignmentEmptyPermittedOperationIsRefused pins the empty operation.
//
// A root that may do nothing is a silent misconfiguration rather than a
// safe default, and an operation named '' is that same misconfiguration
// one level down.
func TestAssignmentEmptyPermittedOperationIsRefused(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	assignmentMustRefuseWith(t, db, "a root whose only permitted operation is empty",
		"a permitted operation must be a non-null string", `
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units,
			permitted_operations, eligible_recipients
		) VALUES ('at-emptyop', ?, 'at-emptyop', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'idem:emptyop', 1, 4, 2, 100, '[""]', '["alpha"]')
	`, projectID)

	id := insertRootAssignment(t, dm, projectID, "at-emptyopupd", "alpha", "implementer", "idem:emptyopupd")
	assignmentMustRefuseWith(t, db, "a permitted operation replaced by an empty one",
		"a permitted operation must be a non-null string", `
		UPDATE agent_tasks SET permitted_operations = '["read",""]' WHERE id = ?`, id)
}

// TestAssignmentLedgerEpochMustBeNonNegative pins the ledger's epoch.
//
// The assignment's own epoch has a `>= 0` bound; the ledger's did not, so
// a ledger entry could record a negative epoch.
func TestAssignmentLedgerEpochMustBeNonNegative(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	id := insertRootAssignment(t, dm, projectID, "at-negepoch", "alpha", "implementer", "idem:negepoch")

	assignmentMustRefuse(t, db, "a ledger entry recording a negative epoch", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type, claim_epoch)
		VALUES ('ate-neg', ?, 0, 'note', -5)`, id)

	// And a non-integer one.
	assignmentMustRefuse(t, db, "a ledger entry recording a REAL epoch", `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type, claim_epoch)
		VALUES ('ate-neg2', ?, 0, 'note', 1.5)`, id)
}

// TestAssignmentWithALedgerCannotBeDeleted pins the dangling-ledger hole.
//
// The not-deletable trigger used to fire only when the task had been
// claimed. A task with a ledger entry but no claim could be deleted —
// with the foreign-key pragma off, leaving the ledger entries pointing at
// an assignment that no longer existed.
//
// The trigger now covers a task that has a ledger, and it does not depend
// on the pragma.
func TestAssignmentWithALedgerCannotBeDeleted(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	id := insertRootAssignment(t, dm, projectID, "at-ledgerdel", "alpha", "implementer", "idem:ledgerdel")
	assignmentMustExec(t, db, `
		INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-ledgerdel', ?, 0, 'created')`, id)

	assignmentMustRefuseWith(t, db, "a task with a ledger deleted",
		"is cancelled, never deleted", `
		DELETE FROM agent_tasks WHERE id = ?`, id)

	// The row is intact.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_tasks WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count the assignment: %v", err)
	}
	if n != 1 {
		t.Errorf("the assignment survived the refused delete with count %d, want 1", n)
	}
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

	// The transition used here is one that NO OTHER rule refuses: an
	// implementer may not skip its independent review. A transition into
	// 'operator_approved' would be refused by the APPROVAL rule even with
	// the state machine dropped, so the control would be measuring the
	// wrong rule.
	if _, err := db.Exec(`UPDATE agent_tasks SET state = 'review_passed' WHERE id = ?`, impl); err != nil {
		t.Fatalf("with the state-machine trigger dropped, the illegal transition was still "+
			"refused (%v) — so the trigger is not what refuses it, and the suite's state-machine "+
			"assertions are not testing the trigger", err)
	}
	// The transition landed, which is the proof the control works.
	var state string
	if err := db.QueryRow(`SELECT state FROM agent_tasks WHERE id = ?`, impl).Scan(&state); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if state != "review_passed" {
		t.Errorf("with the trigger dropped the illegal transition did not land (state %q); "+
			"the control is not measuring what it claims", state)
	}
}

// TestAssignmentClaimControlIsLoadBearing is the control for the claim
// discipline.
//
// The control asserts on the AFFECTED ROWS and on the resulting values, so
// a write that matched no row cannot pass as a success. The second review
// found that the first version checked only that the statement did not
// error, which a zero-match UPDATE satisfies.
func TestAssignmentClaimControlIsLoadBearing(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// The claim discipline is carried by FOUR triggers, and each of the
	// other three would refuse the writes below on its own. The control
	// therefore removes all four, so it measures the discipline and not
	// one of its parts.
	for _, trigger := range []string{
		"trg_agent_tasks_claim_discipline",
		"trg_agent_tasks_claim_sets_owner",
		"trg_agent_tasks_claim_sets_lease",
		"trg_agent_tasks_owner_changes_only_on_claim",
	} {
		assignmentMustExec(t, db, "DROP TRIGGER "+trigger)
	}

	id := insertRootAssignment(t, dm, projectID, "at-ctrl2", "alpha", "implementer", "idem:ctrl2")

	// With the trigger dropped, an epoch that jumps by two is accepted.
	res, err := db.Exec(`
		UPDATE agent_tasks
		SET claim_epoch = claim_epoch + 2, owner = 'alpha', lease_expires_at = 1900000000
		WHERE id = ?`, id)
	if err != nil {
		t.Fatalf("with the claim trigger dropped, the epoch jump was still refused (%v) — "+
			"the suite's claim assertions are not testing the trigger", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("read the affected rows: %v", err)
	}
	if affected != 1 {
		t.Fatalf("the control's UPDATE affected %d rows, want 1. A zero-match write would "+
			"pass as a success, and the control would be measuring nothing.", affected)
	}
	var epoch int64
	if err := db.QueryRow(`SELECT claim_epoch FROM agent_tasks WHERE id = ?`, id).Scan(&epoch); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if epoch != 2 {
		t.Errorf("with the trigger dropped the epoch is %d, want 2 — the control did not land", epoch)
	}

	// And the owner can change with no claim at all.
	res, err = db.Exec(`UPDATE agent_tasks SET owner = 'mallory' WHERE id = ?`, id)
	if err != nil {
		t.Fatalf("with the claim trigger dropped, the owner change was still refused (%v)", err)
	}
	affected, err = res.RowsAffected()
	if err != nil {
		t.Fatalf("read the affected rows: %v", err)
	}
	if affected != 1 {
		t.Fatalf("the control's owner UPDATE affected %d rows, want 1", affected)
	}
	var owner string
	if err := db.QueryRow(`SELECT owner FROM agent_tasks WHERE id = ?`, id).Scan(&owner); err != nil {
		t.Fatalf("read the assignment: %v", err)
	}
	if owner != "mallory" {
		t.Errorf("with the trigger dropped the owner is %q, want mallory — the control did not land", owner)
	}
}

// TestAssignmentIdempotencyControlIsLoadBearing is the control for the
// unique idempotency key.
func TestAssignmentIdempotencyControlIsLoadBearing(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "")

	// The idempotency key is enforced by TWO layers: the unique index
	// and the id-not-reused trigger (which exists to close the
	// INSERT OR REPLACE hole). The control removes BOTH, because the
	// question this control answers is "does the suite depend on the
	// rule at all", and removing one layer would leave the other to
	// refuse the write.
	assignmentMustExec(t, db, `DROP INDEX idx_agent_tasks_idempotency_key`)
	assignmentMustExec(t, db, `DROP TRIGGER trg_agent_tasks_id_is_not_reused`)

	insertRootAssignment(t, dm, projectID, "at-ctrl3", "alpha", "implementer", "github:issue:253")

	if _, err := db.Exec(`
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key,
			max_depth, max_tasks, max_concurrency, max_resource_units, permitted_operations, eligible_recipients) VALUES ('at-ctrl3b', ?, 'at-ctrl3b', NULL, 0, 't', 'o', 'alpha', 'implementer',
			'enqueued', 'github:issue:253', 1, 4, 2, 100, '["read"]', '["alpha"]')
	`, projectID); err != nil {
		t.Fatalf("with the unique index and the id-not-reused trigger both dropped, the "+
			"duplicate was still refused (%v) — the suite's idempotency assertion is not "+
			"testing the rule", err)
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

// TestAgentAssignmentObjectInventoryIsStable pins the EXACT set of objects
// the migration creates.
//
// The second review found that the first version rejected extra TRIGGERS
// but said nothing about extra TABLES or extra INDEXES. The measure is
// therefore the migration's DELTA: the objects present in a migrated
// database minus the objects present in a stripped one must be exactly the
// assignment objects — no more and no fewer, in any category.
func TestAgentAssignmentObjectInventoryIsStable(t *testing.T) {
	migrated := NewTestLocalOnlyDM(t)
	stripped := NewTestLocalOnlyDM(t)
	stripAssignmentObjects(t, stripped)

	migratedObjects := assignmentSchemaObjects(t, migrated.SQLDB())
	strippedObjects := assignmentSchemaObjects(t, stripped.SQLDB())

	var added []string
	for name := range migratedObjects {
		if _, ok := strippedObjects[name]; !ok {
			added = append(added, name)
		}
	}
	sort.Strings(added)

	want := append([]string{}, agentAssignmentObjectNames...)
	sort.Strings(want)

	if len(added) != len(want) {
		t.Errorf("the migration created %d objects (%v), want exactly %d (%v)",
			len(added), added, len(want), want)
		return
	}
	for i := range want {
		if added[i] != want[i] {
			t.Errorf("the migration created %v, want exactly %v", added, want)
			return
		}
	}

	// And no object was MODIFIED by the migration.
	for name, sqlText := range strippedObjects {
		migratedSQL, ok := migratedObjects[name]
		if !ok {
			t.Errorf("the migration removed the object %q", name)
			continue
		}
		if migratedSQL != sqlText {
			t.Errorf("the migration changed %q:\n  before: %s\n  after:  %s", name, sqlText, migratedSQL)
		}
	}
}
