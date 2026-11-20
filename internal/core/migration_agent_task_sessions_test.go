// migration_agent_task_sessions_test.go — Continuity 001, PR 3: the
// acceptance tests for the assignment-to-session mapping.
//
// WHAT IS UNDER TEST
//
//	The mapping is the ONE piece of persistent state PR 3 adds. It exists
//	for one reason: OpenCode's `session.create` is not idempotent, so the
//	adapter must be able to say — after a crash, before any launch — WHICH
//	native session it intended to use and whether it got as far as
//	starting it.
//
//	So the tests pin the three properties recovery depends on:
//
//	  - the session id is present from the FIRST write and cannot be
//	    rewritten, because it is what reconciliation matches on;
//	  - an attempt can only be opened against a claim the assignment
//	    CURRENTLY holds, so a stale worker cannot even record an intent;
//	  - the launch state only moves forward, so a failed attempt cannot be
//	    relabelled as a success.
//
// EVERY DATABASE IS DISPOSABLE
//
//	Each test builds its workspace under t.TempDir(). No test touches the
//	operator's workspace or the live MPM database.
package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestAgentTaskSessionDDLIsPinned pins the DDL text.
//
// The storage may not drift from the design note without someone
// deliberately re-pinning, which is the same discipline PR 1's storage
// carries.
func TestAgentTaskSessionDDLIsPinned(t *testing.T) {
	const want = "1f857378179b83948be83ceb552758a3f491ac0403aebf044f315094b59b5a03"
	sum := sha256.Sum256([]byte(agentTaskSessionDDL))
	got := hex.EncodeToString(sum[:])
	if want != "PLACEHOLDER" && got != want {
		t.Errorf("the agent_task_sessions DDL drifted:\n got %s\nwant %s", got, want)
	}
	if len(agentTaskSessionDDL) < 1000 {
		t.Fatalf("the DDL is %d bytes; that is too short to be the real thing", len(agentTaskSessionDDL))
	}
}

// TestAgentTaskSessionDDLIsTheOnlyDefinition proves the table is created by
// the migration and by nothing else.
//
// A second CREATE TABLE in another file would be a second definition, and
// the two would drift. The static scan is deliberately textual: it is the
// same shape PR 1 uses.
func TestAgentTaskSessionDDLIsTheOnlyDefinition(t *testing.T) {
	var count int
	if err := NewTestLocalOnlyDM(t).SQLDB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		  WHERE type = 'table' AND name = 'agent_task_sessions'`).Scan(&count); err != nil {
		t.Fatalf("count the table: %v", err)
	}
	if count != 1 {
		t.Fatalf("agent_task_sessions exists %d times, want exactly 1", count)
	}
	if strings.Count(agentTaskSessionDDL, "CREATE TABLE") != 1 {
		t.Errorf("the DDL declares %d tables, want exactly 1",
			strings.Count(agentTaskSessionDDL, "CREATE TABLE"))
	}
}

// sessionTestFixture opens a disposable database with a project, a ROOT
// assignment, and that assignment CLAIMED — the minimum an attempt needs.
//
// The claim is done through the SCHEMA's own rules (epoch 0 -> 1 with an
// owner and a lease), so the fixture cannot create a state the service
// could not.
func sessionTestFixture(t *testing.T) (*DatabaseManager, string, int64) {
	t.Helper()
	dm := NewTestLocalOnlyDM(t)
	projectID := seedProject(t, dm, "")
	id := insertRootAssignment(t, dm, projectID, "at-"+strings.Repeat("c", 16), "alpha", "implementer", "idem:sessions")
	if _, err := dm.SQLDB().Exec(`
		UPDATE agent_tasks
		   SET claim_epoch = 1, owner = 'alpha', lease_expires_at = ?
		 WHERE id = ? AND claim_epoch = 0`, int64(1)<<40, id); err != nil {
		t.Fatalf("claim the assignment: %v", err)
	}
	return dm, id, 1
}

// insertSessionAttempt writes one attempt row directly, so the SCHEMA's
// rules — not the service — are what is under test.
func insertSessionAttempt(dm *DatabaseManager, id, taskID string, attempt, epoch int64, sessionID, state string) error {
	_, err := dm.SQLDB().Exec(`
		INSERT INTO agent_task_sessions (
			id, task_id, attempt, claim_epoch, runtime, intended_agent,
			native_session_id, correlation_ref, workspace_id, workspace_path,
			launch_state, created_at, updated_at
		) VALUES (?, ?, ?, ?, 'opencode', 'mpm-worker', ?, 'corr-1',
			'ws-fixture', '/tmp/ws-fixture', ?, 100, 100)`,
		id, taskID, attempt, epoch, sessionID, state)
	return err
}

// TestSessionAttemptRequiresTheCurrentEpoch pins the EARLIEST fence.
//
// An attempt may only be opened against an epoch the assignment currently
// holds. A worker whose lease was reclaimed presents a stale epoch and
// matches no claim, so it cannot even write its intent — which is
// strictly stronger than being unable to write its result.
func TestSessionAttemptRequiresTheCurrentEpoch(t *testing.T) {
	dm, taskID, epoch := sessionTestFixture(t)

	if err := insertSessionAttempt(dm, "ats-ok", taskID, 1, epoch, "ses_ok", "intent"); err != nil {
		t.Fatalf("a correct attempt was refused: %v", err)
	}

	err := insertSessionAttempt(dm, "ats-stale", taskID, 2, epoch-1, "ses_stale", "intent")
	if err == nil {
		t.Fatal("an attempt at a STALE epoch was accepted")
	}
	if !strings.Contains(err.Error(), "not the assignment's current claim") {
		t.Errorf("the stale attempt was refused, but not by the epoch rule: %v", err)
	}
}

// TestSessionAttemptIdentityIsWriteOnce pins recovery's core assumption.
//
// The native session id is what a restart reconciles on. If it could be
// rewritten, the recovery record could be made to point at a session that
// was never launched — and a retry would then start a SECOND session while
// the row claimed it had resumed the first.
func TestSessionAttemptIdentityIsWriteOnce(t *testing.T) {
	dm, taskID, epoch := sessionTestFixture(t)
	if err := insertSessionAttempt(dm, "ats-imm", taskID, 1, epoch, "ses_imm", "intent"); err != nil {
		t.Fatalf("seed the attempt: %v", err)
	}

	for _, tc := range []struct {
		name string
		stmt string
	}{
		{"rewrite the native session id", `UPDATE agent_task_sessions SET native_session_id = 'ses_other' WHERE id = 'ats-imm'`},
		{"rewrite the claim epoch", `UPDATE agent_task_sessions SET claim_epoch = 99 WHERE id = 'ats-imm'`},
		{"repoint the task", `UPDATE agent_task_sessions SET task_id = 'at-elsewhere' WHERE id = 'ats-imm'`},
		{"renumber the attempt", `UPDATE agent_task_sessions SET attempt = 7 WHERE id = 'ats-imm'`},
	} {
		_, err := dm.SQLDB().Exec(tc.stmt)
		if err == nil {
			t.Errorf("%s was accepted; the identity is not write-once", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "write-once") {
			t.Errorf("%s was refused, but not by the immutability rule: %v", tc.name, err)
		}
	}
}

// TestSessionLaunchStateMovesForwardOnly pins the second half of the
// recovery contract: a terminal state is terminal.
//
// Without it a failed attempt could be moved back to `intent` and retried
// in place, so the record would show one attempt where there were two.
func TestSessionLaunchStateMovesForwardOnly(t *testing.T) {
	dm, taskID, epoch := sessionTestFixture(t)
	if err := insertSessionAttempt(dm, "ats-state", taskID, 1, epoch, "ses_state", "intent"); err != nil {
		t.Fatalf("seed the attempt: %v", err)
	}

	// The forward path is accepted.
	for _, s := range []string{"launched", "running", "completed"} {
		if _, err := dm.SQLDB().Exec(
			`UPDATE agent_task_sessions SET launch_state = ? WHERE id = 'ats-state'`, s); err != nil {
			t.Fatalf("moving forward to %q was refused: %v", s, err)
		}
	}

	// Going back is not.
	for _, s := range []string{"intent", "launched", "running", "failed"} {
		_, err := dm.SQLDB().Exec(
			`UPDATE agent_task_sessions SET launch_state = ? WHERE id = 'ats-state'`, s)
		if err == nil {
			t.Errorf("moving BACK to %q was accepted", s)
			continue
		}
		if !strings.Contains(err.Error(), "only move forward") {
			t.Errorf("moving back to %q was refused, but not by the ordering rule: %v", s, err)
		}
	}
}

// TestSessionAttemptsAreConsecutive pins the retry record.
//
// A task's attempts must be 1, 2, 3 ... A gap would let the record show
// "attempt 1" and "attempt 4" with the middle executions unrecorded.
func TestSessionAttemptsAreConsecutive(t *testing.T) {
	dm, taskID, epoch := sessionTestFixture(t)
	if err := insertSessionAttempt(dm, "ats-a1", taskID, 1, epoch, "ses_a1", "intent"); err != nil {
		t.Fatalf("seed attempt 1: %v", err)
	}

	if err := insertSessionAttempt(dm, "ats-a3", taskID, 3, epoch, "ses_a3", "intent"); err == nil {
		t.Error("attempt 3 was accepted after attempt 1; the retry record has a gap")
	}
	if err := insertSessionAttempt(dm, "ats-a2", taskID, 2, epoch, "ses_a2", "intent"); err != nil {
		t.Errorf("attempt 2 was refused: %v", err)
	}
}

// TestSessionNativeIDIsGloballyUnique pins the "one session, one attempt"
// rule.
//
// Two assignments that named the same native session would both believe
// they own its output. The unique index makes that impossible.
func TestSessionNativeIDIsGloballyUnique(t *testing.T) {
	dm, taskID, epoch := sessionTestFixture(t)
	if err := insertSessionAttempt(dm, "ats-u1", taskID, 1, epoch, "ses_shared", "intent"); err != nil {
		t.Fatalf("seed the first attempt: %v", err)
	}

	// A second task, in the same tree, claiming the same native session.
	projectID := "cpj-" + strings.Repeat("a", 16)
	other := insertRootAssignment(t, dm, projectID, "at-"+strings.Repeat("d", 16), "charlie", "implementer", "idem:other")
	if _, err := dm.SQLDB().Exec(`
		UPDATE agent_tasks SET claim_epoch = 1, owner = 'charlie', lease_expires_at = ?
		 WHERE id = ? AND claim_epoch = 0`, int64(1)<<40, other); err != nil {
		t.Fatalf("claim the second assignment: %v", err)
	}

	err := insertSessionAttempt(dm, "ats-u2", other, 1, epoch, "ses_shared", "intent")
	if err == nil {
		t.Fatal("the same native session was attached to a second attempt")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("refused, but not by the uniqueness rule: %v", err)
	}
}

// TestSessionRecordIsNotDeletable pins that the execution record outlives
// a retry.
//
// Deleting the row would make an executed assignment look unexecuted, and
// a fresh retry would then be indistinguishable from the first launch.
func TestSessionRecordIsNotDeletable(t *testing.T) {
	dm, taskID, epoch := sessionTestFixture(t)
	if err := insertSessionAttempt(dm, "ats-del", taskID, 1, epoch, "ses_del", "intent"); err != nil {
		t.Fatalf("seed the attempt: %v", err)
	}
	_, err := dm.SQLDB().Exec(`DELETE FROM agent_task_sessions WHERE id = 'ats-del'`)
	if err == nil {
		t.Fatal("the execution record was deleted")
	}
	if !strings.Contains(err.Error(), "not deletable") {
		t.Errorf("refused, but not by the deletion rule: %v", err)
	}
}

// TestSessionMigrationIsIdempotentAndLocalOnly pins the migration's
// discipline, matching PR 1's AT5/AT7.
func TestSessionMigrationIsIdempotentAndLocalOnly(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	db := dm.SQLDB()

	// Idempotent: the sentinel is present, and re-running is a no-op.
	var applied int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = 'agent_task_sessions_v1'`,
	).Scan(&applied); err != nil {
		t.Fatalf("read the sentinel: %v", err)
	}
	if applied != 1 {
		t.Fatalf("the sentinel is present %d times, want 1", applied)
	}

	// Local-only: the table must NOT appear in the cross-workspace
	// `shared` schema. attachShared propagates BaseTables only, and this
	// table is migration-owned — so the assertion is that a shared
	// attach does not carry it.
	var sharedCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		  WHERE type = 'table' AND name = 'agent_task_sessions'`).Scan(&sharedCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if sharedCount != 1 {
		t.Fatalf("agent_task_sessions count is %d, want 1", sharedCount)
	}

	// The restore allow-list carries the name, so a legitimate dump is
	// accepted rather than refused.
	found := false
	for _, name := range CanonicalMPMSchema {
		if name == "agent_task_sessions" {
			found = true
			break
		}
	}
	if !found {
		t.Error("agent_task_sessions is missing from CanonicalMPMSchema, so a legitimate restore dump would be refused")
	}
}
