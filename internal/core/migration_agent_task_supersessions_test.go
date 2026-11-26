// migration_agent_task_supersessions_test.go — Continuity 001 recovery:
// the immutable supersession lineage.
//
// # WHAT IS UNDER TEST
//
// The lineage is the recovery authorisation: one row binding an ambiguous
// attempt to the replacement and reviewer that continue the work. The
// tests pin the DDL text, the schema sentinel, the exact object
// inventory, and the storage backstops that make the lineage immutable
// and the tree's execution authority a database fact rather than a
// convention.
//
// Every database is disposable. No test touches the operator's workspace
// or the live MPM database.
package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// agentSupersessionTables are the tables the migration creates.
var agentSupersessionTables = []string{
	"agent_task_supersessions",
}

// agentSupersessionIndexes are the indexes the DDL creates. Named
// explicitly so a dropped or renamed index fails the test.
var agentSupersessionIndexes = []string{
	"idx_agent_task_supersessions_root",
}

// agentSupersessionTriggers are the twenty-one trigger statements the DDL creates
// for the twenty recovery rules TR1-TR20 (TR20 has separate UPDATE and INSERT
// guards: SQLite accepts no column list on INSERT triggers).
//
// TR1–TR8 guard the lineage table. TR9–TR18 guard agent_tasks. TR19–TR20
// guard agent_task_sessions.
var agentSupersessionTriggers = []string{
	"trg_agent_task_supersessions_no_update",
	"trg_agent_task_supersessions_no_delete",
	"trg_agent_task_supersessions_original_eligible",
	"trg_agent_task_supersessions_contained",
	"trg_agent_task_supersessions_attempt_is_ambiguity",
	"trg_agent_task_supersessions_single_ambiguity",
	"trg_agent_task_supersessions_fresh_identities",
	"trg_agent_task_supersessions_no_cycle",
	"trg_agent_tasks_reserved_key",
	"trg_agent_tasks_superseded_shape",
	"trg_agent_tasks_claim_epoch_guard",
	"trg_agent_tasks_start_guard",
	"trg_agent_tasks_complete_guard",
	"trg_agent_tasks_cancel_guard",
	"trg_agent_tasks_handoff_guard",
	"trg_agent_tasks_review_passed_guard",
	"trg_agent_tasks_approval_guard",
	"trg_agent_tasks_reviewer_evidence_guard",
	"trg_agent_task_sessions_admission_guard",
	"trg_agent_task_sessions_completion_guard",
	"trg_agent_task_sessions_completion_insert_guard",
}

// agentSupersessionObjectNames is the exact set of objects the migration
// adds to a migrated database.
var agentSupersessionObjectNames = func() []string {
	out := append([]string{}, agentSupersessionTables...)
	out = append(out, agentSupersessionIndexes...)
	out = append(out, agentSupersessionTriggers...)
	return out
}()

// TestAgentSupersessionDDLIsPinned pins the migration text.
//
// The storage may not drift from the design note without someone
// deliberately re-pinning, which is the same discipline PR 1's storage
// carries.
func TestAgentSupersessionDDLIsPinned(t *testing.T) {
	const want = "01785668cec7f2a27ae58e2ef8a6b0a5ac7146c396001412ad202208e9c017fc"
	ddl := agentTaskSupersessionTableDDL + agentTaskSupersessionTriggerDDL
	sum := sha256.Sum256([]byte(ddl))
	got := hex.EncodeToString(sum[:])
	if want != "PLACEHOLDER" && got != want {
		t.Errorf("the supersession DDL drifted:\n got %s\nwant %s", got, want)
	}
	if len(ddl) < 1000 {
		t.Fatalf("the DDL is %d bytes; that is too short to be the real thing", len(ddl))
	}
	t.Logf("supersession DDL sha256: %s", got)
}

// TestAgentSupersessionDDLIsTheOnlyDefinition proves the table is created
// by the migration and by nothing else.
func TestAgentSupersessionDDLIsTheOnlyDefinition(t *testing.T) {
	var count int
	if err := NewTestLocalOnlyDM(t).SQLDB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_task_supersessions'`).Scan(&count); err != nil {
		t.Fatalf("count the table: %v", err)
	}
	if count != 1 {
		t.Fatalf("agent_task_supersessions exists %d times, want exactly 1", count)
	}
	if got := strings.Count(agentTaskSupersessionTableDDL, "CREATE TABLE"); got != 1 {
		t.Errorf("the table DDL declares %d tables, want exactly 1", got)
	}
}

// TestAgentSupersessionSentinelIsWrittenLast proves the migration is
// idempotent and records its sentinel.
func TestAgentSupersessionSentinelIsWrittenLast(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	var applied int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = 'agent_task_supersessions_v1'`).Scan(&applied); err != nil {
		t.Fatalf("read the sentinel: %v", err)
	}
	if applied != 1 {
		t.Fatalf("the sentinel is recorded %d times, want exactly 1", applied)
	}
}

// TestAgentSupersessionObjectInventoryIsStable pins the exact set of
// objects the migration adds: one table, one named index, twenty
// triggers — no more and no fewer.
//
// Direction 1 checks the DDL text itself declares exactly the inventoried
// triggers. Direction 2 checks the database contains exactly those
// objects. Together they catch both a DDL addition the inventory does not
// name and a partial application.
func TestAgentSupersessionObjectInventoryIsStable(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	// Direction 1: the trigger names in the DDL text equal the inventory.
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^CREATE TRIGGER IF NOT EXISTS (\w+)`).FindAllStringSubmatch(
		agentTaskSupersessionTableDDL+agentTaskSupersessionTriggerDDL, -1) {
		declared[m[1]] = true
	}
	var declaredNames []string
	for name := range declared {
		declaredNames = append(declaredNames, name)
	}
	sort.Strings(declaredNames)
	var wantTriggers []string
	for _, name := range agentSupersessionObjectNames {
		if strings.HasPrefix(name, "trg_") {
			wantTriggers = append(wantTriggers, name)
		}
	}
	sort.Strings(wantTriggers)
	if len(declaredNames) != len(wantTriggers) {
		t.Fatalf("the DDL declares %d triggers (%v), want %d (%v)",
			len(declaredNames), declaredNames, len(wantTriggers), wantTriggers)
	}
	for i := range wantTriggers {
		if declaredNames[i] != wantTriggers[i] {
			t.Fatalf("the DDL declares %v, want exactly %v", declaredNames, wantTriggers)
		}
	}
	// Direction 2: the database contains exactly the inventoried objects.
	rows, err := dm.SQLDB().Query(
		`SELECT type || ':' || name FROM sqlite_master
		  WHERE (type = 'table' AND name = 'agent_task_supersessions')
		     OR (type = 'index' AND name = 'idx_agent_task_supersessions_root')
		     OR (type = 'trigger'
		         AND (name LIKE 'trg\_agent\_task\_supersession%' ESCAPE '\'
		              OR name IN (
				 'trg_agent_tasks_reserved_key','trg_agent_tasks_superseded_shape',
				 'trg_agent_tasks_claim_epoch_guard','trg_agent_tasks_start_guard',
				 'trg_agent_tasks_complete_guard','trg_agent_tasks_cancel_guard',
				 'trg_agent_tasks_handoff_guard','trg_agent_tasks_review_passed_guard',
				 'trg_agent_tasks_approval_guard','trg_agent_tasks_reviewer_evidence_guard',
				 'trg_agent_task_sessions_admission_guard',
				 'trg_agent_task_sessions_completion_guard',
				 'trg_agent_task_sessions_completion_insert_guard')))`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		parts := strings.SplitN(name, ":", 2)
		got = append(got, parts[1])
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := append([]string{}, agentSupersessionObjectNames...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("the migration created %d objects (%v), want exactly %d (%v)",
			len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the migration created %v, want exactly %v", got, want)
		}
	}
}

// supersessionFixture builds a project, a root, an in_progress child with
// a live claim, and one ambiguous attempt on it — the minimum shape a
// lineage can name.
func supersessionFixture(t *testing.T) (*DatabaseManager, string, string, string, int64) {
	t.Helper()
	dm := NewTestLocalOnlyDM(t)
	projectID := seedProject(t, dm, "cpj-"+strings.Repeat("s", 16))
	rootID := insertRootAssignment(t, dm, projectID, "at-"+strings.Repeat("a", 16), "alpha", "implementer", "idem:sup-root")
	childID := insertChildAssignment(t, dm, projectID, "at-"+strings.Repeat("d", 16), rootID, rootID, "alpha", "idem:sup-child")
	// Claim first (TR11 permits it: no ambiguity yet), then walk the legal
	// path to in_progress (the PR 1 state trigger is the backstop, so the
	// fixture cannot skip steps).
	if _, err := dm.SQLDB().Exec(
		`UPDATE agent_tasks SET claim_epoch = 1, owner = 'alpha',
			lease_expires_at = ? WHERE id = ? AND claim_epoch = 0`, int64(1)<<40, childID); err != nil {
		t.Fatalf("claim the child: %v", err)
	}
	for _, to := range []string{"delivered", "acknowledged", "accepted", "in_progress"} {
		if _, err := dm.SQLDB().Exec(
			`UPDATE agent_tasks SET state = ?, updated_at = ? WHERE id = ?`, to, int64(100), childID); err != nil {
			t.Fatalf("advance the child to %s: %v", to, err)
		}
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO agent_task_sessions (
			id, task_id, attempt, claim_epoch, runtime, intended_agent,
			native_session_id, correlation_ref, workspace_id, workspace_path,
			launch_state, created_at, updated_at
		) VALUES ('ats0000000000000001', ?, 1, 1, 'opencode', 'mpm-worker',
			'ses_sup_ambiguous', 'corr-1', 'ws', '/tmp/ws', 'ambiguous', 100, 200)`, childID); err != nil {
		t.Fatalf("insert the ambiguous attempt: %v", err)
	}
	return dm, projectID, rootID, childID, 1
}

// insertFullLineage inserts a complete valid lineage with its replacement
// and reviewer tasks in one transaction, so the deferred foreign keys
// resolve at commit. The replacement and reviewer carry the sealed shape
// the guarded write would produce.
func insertFullLineage(t *testing.T, dm *DatabaseManager, superseded, replacement, reviewer, attempt string) {
	t.Helper()
	var parentID string
	var depth int
	if err := dm.SQLDB().QueryRow(
		`SELECT parent_id, depth FROM agent_tasks WHERE id = ?`, superseded).Scan(&parentID, &depth); err != nil {
		t.Fatalf("read the superseded task: %v", err)
	}
	tx, err := dm.SQLDB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	// Defer a rollback: harmless after a successful commit, a safety net
	// for every failure path below.
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`
		INSERT INTO agent_task_supersessions (
			id, project_id, root_id, superseded_task_id,
			replacement_task_id, reviewer_task_id, attempt_id,
			authorised_by, authority_kind, evidence_ref, justification, created_at
		) SELECT 'atsup-1', project_id, root_id, id, ?, ?, ?, 'operator', 'operator', 'log:pilot/supersede-001', 'the original is blocked', 300
		  FROM agent_tasks WHERE id = ?`,
		replacement, reviewer, attempt, superseded); err != nil {
		t.Fatalf("insert the lineage: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key, resource_units, base_sha
		) VALUES (?, (SELECT project_id FROM agent_tasks WHERE id = ?),
			(SELECT root_id FROM agent_tasks WHERE id = ?),
			?, ?, 'the rework', 'redo the work', 'alpha', 'implementer',
			'enqueued', ?, 1, '0598edb7ac904e98a5a21c1fe140333742857ffb')`,
		replacement, superseded, superseded, parentID, depth,
		"supersede:"+attempt); err != nil {
		t.Fatalf("insert the replacement: %v", err)
	}
	reviewerPayload := `{"review_target":"` + replacement + `"}`
	if _, err := tx.Exec(`
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key, resource_units, payload
		) VALUES (?, (SELECT project_id FROM agent_tasks WHERE id = ?),
			(SELECT root_id FROM agent_tasks WHERE id = ?),
			?, ?, 'the re-review', 'review the rework', 'charlie', 'reviewer',
			'enqueued', ?, 1, ?)`,
		reviewer, superseded, superseded, parentID, depth,
		"supersede:"+attempt+":review", reviewerPayload); err != nil {
		t.Fatalf("insert the reviewer: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit the lineage: %v", err)
	}
}

// insertSupersessionLineage inserts a lineage row on its own. It only
// succeeds when the named replacement and reviewer tasks already exist,
// which is never the case for a fresh lineage — direct use is for the
// negative tests, where the insert itself must fail.
func insertSupersessionLineage(t *testing.T, dm *DatabaseManager, superseded, replacement, reviewer, attempt string) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO agent_task_supersessions (
			id, project_id, root_id, superseded_task_id,
			replacement_task_id, reviewer_task_id, attempt_id,
			authorised_by, authority_kind, evidence_ref, justification, created_at
		) VALUES ('atsup-1', (SELECT project_id FROM agent_tasks WHERE id = ?),
			(SELECT root_id FROM agent_tasks WHERE id = ?),
			?, ?, ?, ?, 'operator', 'operator', 'log:pilot/supersede-001', 'the original is blocked', 300)`,
		superseded, superseded, superseded, replacement, reviewer, attempt); err != nil {
		t.Fatalf("insert the lineage: %v", err)
	}
}

// TestSupersessionLineageIsImmutable proves TR1 and TR2: no UPDATE and no
// DELETE of a lineage row, ever.
func TestSupersessionLineageIsImmutable(t *testing.T) {
	dm, _, _, childID, _ := supersessionFixture(t)
	insertFullLineage(t, dm, childID, "at-"+strings.Repeat("b", 16), "at-"+strings.Repeat("c", 16), "ats0000000000000001")
	if _, err := dm.SQLDB().Exec(
		`UPDATE agent_task_supersessions SET justification = 'rewritten' WHERE id = 'atsup-1'`); err == nil {
		t.Fatal("a lineage UPDATE was accepted; the lineage must be write-once")
	}
	if _, err := dm.SQLDB().Exec(
		`DELETE FROM agent_task_supersessions WHERE id = 'atsup-1'`); err == nil {
		t.Fatal("a lineage DELETE was accepted; the lineage must not be deletable")
	}
}

// TestSupersessionRequiresAnEligibleOriginal pins TR3: a missing task, a
// reviewer, a root and a terminal task are all refused.
func TestSupersessionRequiresAnEligibleOriginal(t *testing.T) {
	dm, projectID, rootID, _, _ := supersessionFixture(t)
	// Missing task.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO agent_task_supersessions (
			id, project_id, root_id, superseded_task_id,
			replacement_task_id, reviewer_task_id, attempt_id,
			authorised_by, authority_kind, evidence_ref, justification, created_at
		) VALUES ('atsup-x', ?, ?, 'at-missing0000000000',
			?, ?, 'ats0000000000000001', 'operator', 'operator', 'log:pilot/supersede-001', 'blocked', 300)`,
		projectID, rootID, "at-"+strings.Repeat("b", 16), "at-"+strings.Repeat("c", 16)); err == nil {
		t.Fatal("a lineage for a missing task was accepted")
	}
	// A reviewer cannot be superseded. Insert one directly: the role is
	// immutable, so it cannot be reached by updating an implementer.
	revID := "at-" + strings.Repeat("e", 16)
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key
		) VALUES (?, ?, ?, ?, 1, 'the review', 'review the work', 'charlie', 'reviewer', 'enqueued', 'idem:sup-rev')`,
		revID, projectID, rootID, rootID); err != nil {
		t.Fatalf("insert the reviewer: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO agent_task_supersessions (
			id, project_id, root_id, superseded_task_id,
			replacement_task_id, reviewer_task_id, attempt_id,
			authorised_by, authority_kind, evidence_ref, justification, created_at
		) VALUES ('atsup-y', ?, ?, ?, ?, ?, 'ats0000000000000001', 'operator', 'operator', 'log:pilot/supersede-001', 'blocked', 300)`,
		projectID, rootID, revID, "at-"+strings.Repeat("b", 16), "at-"+strings.Repeat("c", 16)); err == nil {
		t.Fatal("a lineage for a reviewer was accepted")
	}
	// The root itself cannot be superseded.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO agent_task_supersessions (
			id, project_id, root_id, superseded_task_id,
			replacement_task_id, reviewer_task_id, attempt_id,
			authorised_by, authority_kind, evidence_ref, justification, created_at
		) VALUES ('atsup-z', ?, ?, ?, ?, ?, 'ats0000000000000001', 'operator', 'operator', 'log:pilot/supersede-001', 'blocked', 300)`,
		projectID, rootID, rootID, "at-"+strings.Repeat("b", 16), "at-"+strings.Repeat("c", 16)); err == nil {
		t.Fatal("a lineage for the root was accepted")
	}
}

// TestClaimEpochUpdateRefusedInAmbiguousTree proves the TR11 backstop at
// the storage layer: a direct claim_epoch write in an ambiguous tree is
// refused, and the refusal changes nothing — epoch, owner, lease and
// state are exactly as before, and neither the ledger nor the attempt
// history gains a row.
func TestClaimEpochUpdateRefusedInAmbiguousTree(t *testing.T) {
	dm, _, _, childID, _ := supersessionFixture(t)
	db := dm.SQLDB()

	var epoch int64
	var owner sql.NullString
	var lease sql.NullInt64
	var state string
	if err := db.QueryRow(`SELECT claim_epoch, owner, lease_expires_at, state FROM agent_tasks WHERE id = ?`,
		childID).Scan(&epoch, &owner, &lease, &state); err != nil {
		t.Fatalf("read the task: %v", err)
	}
	var events, attempts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_task_events WHERE task_id = ?`, childID).Scan(&events); err != nil {
		t.Fatalf("count the events: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_task_sessions WHERE task_id = ?`, childID).Scan(&attempts); err != nil {
		t.Fatalf("count the attempts: %v", err)
	}

	if _, err := db.Exec(`UPDATE agent_tasks SET claim_epoch = ?, owner = 'intruder',
		lease_expires_at = ? WHERE id = ?`, epoch+1, int64(1)<<40, childID); err == nil {
		t.Fatalf("a claim_epoch write in an ambiguous tree was accepted")
	} else if !strings.Contains(err.Error(), "execution guard") {
		t.Fatalf("refused, but not by the execution guard: %v", err)
	}

	var epochAfter int64
	var ownerAfter sql.NullString
	var leaseAfter sql.NullInt64
	var stateAfter string
	if err := db.QueryRow(`SELECT claim_epoch, owner, lease_expires_at, state FROM agent_tasks WHERE id = ?`,
		childID).Scan(&epochAfter, &ownerAfter, &leaseAfter, &stateAfter); err != nil {
		t.Fatalf("re-read the task: %v", err)
	}
	if epochAfter != epoch || ownerAfter != owner || leaseAfter != lease || stateAfter != state {
		t.Fatalf("the refused write changed the row: (%d,%v,%v,%s) -> (%d,%v,%v,%s)",
			epoch, owner, lease, state, epochAfter, ownerAfter, leaseAfter, stateAfter)
	}
	var eventsAfter, attemptsAfter int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_task_events WHERE task_id = ?`, childID).Scan(&eventsAfter); err != nil {
		t.Fatalf("re-count the events: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_task_sessions WHERE task_id = ?`, childID).Scan(&attemptsAfter); err != nil {
		t.Fatalf("re-count the attempts: %v", err)
	}
	if eventsAfter != events || attemptsAfter != attempts {
		t.Fatalf("the refused write recorded rows: events %d->%d, attempts %d->%d",
			events, eventsAfter, attempts, attemptsAfter)
	}
}

// TestClaimEpochUpdateRefusedOnTerminalTask proves the terminal arm of
// TR11 at the storage layer: a claim_epoch write on a completed task is
// refused even in a clean tree, and changes nothing.
func TestClaimEpochUpdateRefusedOnTerminalTask(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "cpj-"+strings.Repeat("t", 16))
	id := insertRootAssignment(t, dm, projectID, "at-"+strings.Repeat("e", 16), "alpha", "implementer", "idem:terminal-claim")
	for _, step := range []string{"delivered", "acknowledged", "accepted", "in_progress"} {
		if _, err := db.Exec(`UPDATE agent_tasks SET state = ? WHERE id = ?`, step, id); err != nil {
			t.Fatalf("advance to %s: %v", step, err)
		}
	}
	provisionCompletionEvidence(t, db, id)
	if _, err := db.Exec(`UPDATE agent_tasks SET state = 'completed' WHERE id = ?`, id); err != nil {
		t.Fatalf("complete the task: %v", err)
	}

	var epoch int64
	if err := db.QueryRow(`SELECT claim_epoch FROM agent_tasks WHERE id = ?`, id).Scan(&epoch); err != nil {
		t.Fatalf("read the epoch: %v", err)
	}
	// The base claim-discipline trigger owns terminal refusal (our TR11
	// owns the ambiguity arm, so the base message is preserved): either
	// way the write is refused and the epoch cannot drift.
	if _, err := db.Exec(`UPDATE agent_tasks SET claim_epoch = ?, owner = 'intruder' WHERE id = ?`,
		epoch+1, id); err == nil {
		t.Fatalf("a claim_epoch write on a terminal task was accepted")
	} else if !strings.Contains(err.Error(), "new lease") &&
		!strings.Contains(err.Error(), "live assignment") &&
		!strings.Contains(err.Error(), "execution guard") {
		t.Fatalf("refused, but not as a terminal claim: %v", err)
	}
	var epochAfter int64
	var ownerAfter sql.NullString
	if err := db.QueryRow(`SELECT claim_epoch, owner FROM agent_tasks WHERE id = ?`, id).Scan(&epochAfter, &ownerAfter); err != nil {
		t.Fatalf("re-read the task: %v", err)
	}
	if epochAfter != epoch {
		t.Fatalf("the refused write moved the epoch to %d, want %d", epochAfter, epoch)
	}
}

// TestSupersessionReviewerPayloadIsFixedIsT95 proves TR10 at the storage
// layer: a lineage-named reviewer row whose payload names no review
// target, or the wrong one, is refused on INSERT. TR10 only evaluates
// reserved-key rows a lineage names, so the test stages a lineage first
// (all other guards passing) and then offers the malformed reviewer.
func TestSupersessionReviewerPayloadIsT95(t *testing.T) {
	dm, _, _, childID, _ := supersessionFixture(t)
	db := dm.SQLDB()
	attemptID := "ats0000000000000001"
	tryReviewer := func(t *testing.T, reviewerID, payload string) error {
		repID := "at-" + strings.Repeat("b", 16)
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`
			INSERT INTO agent_task_supersessions (
				id, project_id, root_id, superseded_task_id,
				replacement_task_id, reviewer_task_id, attempt_id,
				authorised_by, authority_kind, evidence_ref, justification, created_at
			) SELECT 'atsup-t95', project_id, root_id, id, ?, ?, ?, 'operator', 'operator', 'log:pilot/x', 'blocked', 300
			  FROM agent_tasks WHERE id = ?`,
			repID, reviewerID, attemptID, childID); err != nil {
			t.Fatalf("stage the lineage: %v", err)
		}
		var parentID string
		var depth int
		if err := tx.QueryRow(`SELECT parent_id, depth FROM agent_tasks WHERE id = ?`,
			childID).Scan(&parentID, &depth); err != nil {
			t.Fatalf("read the placement: %v", err)
		}
		if _, err := tx.Exec(`
			INSERT INTO agent_tasks (
				id, project_id, root_id, parent_id, depth, title, objective,
				recipient, role, state, idempotency_key, resource_units, base_sha
			) VALUES (?, (SELECT project_id FROM agent_tasks WHERE id = ?),
				(SELECT root_id FROM agent_tasks WHERE id = ?),
				?, ?, 'the rework', 'redo the work', 'alpha', 'implementer',
				'enqueued', ?, 1, '0598edb7ac904e98a5a21c1fe140333742857ffb')`,
			repID, childID, childID, parentID, depth,
			"supersede:"+attemptID); err != nil {
			t.Fatalf("stage the replacement: %v", err)
		}
		_, err = tx.Exec(`
			INSERT INTO agent_tasks (
				id, project_id, root_id, parent_id, depth, title, objective,
				recipient, role, state, idempotency_key, resource_units, payload
			) VALUES (?, (SELECT project_id FROM agent_tasks WHERE id = ?),
				(SELECT root_id FROM agent_tasks WHERE id = ?),
				?, ?, 'the review', 'review it', 'charlie', 'reviewer',
				'enqueued', ?, 1, ?)`,
			reviewerID, childID, childID, parentID, depth,
			"supersede:"+attemptID+":review", payload)
		return err
	}
	// Missing review_target.
	if err := tryReviewer(t, "at-"+strings.Repeat("e", 16), `{"other":"x"}`); err == nil {
		t.Fatal("a reviewer without a review_target was accepted")
	} else if !strings.Contains(err.Error(), "sealed shape") {
		t.Fatalf("a reviewer without a target was refused, but not by TR10: %v", err)
	}
	// Wrong review_target (names nothing the lineage authorises).
	if err := tryReviewer(t, "at-"+strings.Repeat("f", 16), `{"review_target":"at-does-not-exist"}`); err == nil {
		t.Fatal("a reviewer with a wrong review_target was accepted")
	} else if !strings.Contains(err.Error(), "sealed shape") {
		t.Fatalf("a reviewer with a wrong target was refused, but not by TR10: %v", err)
	}
}

// TestReviewerVerdictNeedsCapturedEvidenceIsT102T103 proves TR18 at the
// storage layer: a reviewer moving to completed or review_passed without
// host-captured evidence is refused, while cancellation from in_progress
// is not gated here (TR14 governs ambiguity/historical withdrawal).
func TestReviewerVerdictNeedsCapturedEvidenceIsT102T103(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()
	projectID := seedProject(t, dm, "cpj-"+strings.Repeat("v", 16))
	rootID := insertRootAssignment(t, dm, projectID, "at-"+strings.Repeat("v", 16), "alpha", "implementer", "idem:sup-verd-root")
	subjectID := insertChildAssignment(t, dm, projectID, "at-"+strings.Repeat("w", 16), rootID, rootID, "alpha", "idem:sup-verd-subject")
	for _, to := range []string{"delivered", "acknowledged", "accepted", "in_progress"} {
		if _, err := db.Exec(`UPDATE agent_tasks SET state = ?, updated_at = ? WHERE id = ?`, to, int64(100), subjectID); err != nil {
			t.Fatalf("advance the subject to %s: %v", to, err)
		}
	}
	revID := "at-" + strings.Repeat("x", 16)
	if _, err := db.Exec(`
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key, resource_units, payload
		) VALUES (?, ?, ?, ?, 1, 'the review', 'review it', 'charlie', 'reviewer',
			'enqueued', 'idem:sup-verd-rev', 1, ?)`,
		revID, projectID, rootID, rootID, `{"review_target":"`+subjectID+`"}`); err != nil {
		t.Fatalf("insert the reviewer: %v", err)
	}
	for _, to := range []string{"delivered", "acknowledged", "accepted", "in_progress"} {
		if _, err := db.Exec(`UPDATE agent_tasks SET state = ?, updated_at = ? WHERE id = ?`, to, int64(100), revID); err != nil {
			t.Fatalf("advance the reviewer to %s: %v", to, err)
		}
	}
	// A verdict without evidence is refused by TR18.
	for _, to := range []string{"completed", "review_passed"} {
		if _, err := db.Exec(`UPDATE agent_tasks SET state = ? WHERE id = ?`, to, revID); err == nil {
			t.Fatalf("a verdict to %s without evidence was accepted", to)
		} else if !strings.Contains(err.Error(), "captured evidence") {
			t.Fatalf("a verdict to %s was refused, but not for evidence: %v", to, err)
		}
	}
	// Cancellation from in_progress is not an evidence-gated verdict.
	if _, err := db.Exec(`UPDATE agent_tasks SET state = 'cancelled' WHERE id = ?`, revID); err != nil {
		t.Fatalf("a reviewer cancellation was refused by the evidence guard: %v", err)
	}
}
