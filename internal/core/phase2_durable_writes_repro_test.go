package internal

// phase2_durable_writes_repro_test.go — issue #178, phase 2: the durable
// writes that lose their artifact (or lie about it) under ordinary lock
// contention.
//
// WHAT THIS FILE PINS
// --------------------
// Five sites where a busy failure loses durable user state or reports a
// success that did not happen. Each test fails against the pre-fix
// implementation and passes after. Two techniques, both already
// established in this package:
//
//   - SURVIVAL: hold the WAL write lock, run the real function, release
//     only after the statement-level retry has engaged
//     (BusyRetryCount > 0), and assert the artifact landed. A bare pool
//     write has no retry to engage, so on the old code the write fails
//     when the hold outlasts the busy timeout.
//   - PROPAGATION: install a trigger that refuses the write, call the
//     function, and assert the error reaches the caller. A site that
//     discards the error cannot do that.
//
// The shared primitive (a busy statement had no effect, so a retry
// cannot double-apply) is already proven by
// TestRepro_RetryingABusyStatementDoesNotDoubleApply and is not
// re-proven here.

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// blockTable installs a trigger that refuses every write to the table,
// so the function under test meets a deterministic failure — no sleeps,
// no timing.
func blockTable(t *testing.T, dm *DatabaseManager, table, trigger string) {
	t.Helper()
	if _, err := dm.db.Exec(`DROP TRIGGER IF EXISTS ` + trigger); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.db.Exec(
		`CREATE TRIGGER ` + trigger + ` BEFORE INSERT ON ` + table +
			` BEGIN SELECT RAISE(ABORT, 'blocked for repro'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.db.Exec(
		`CREATE TRIGGER ` + trigger + `_u BEFORE UPDATE ON ` + table +
			` BEGIN SELECT RAISE(ABORT, 'blocked for repro'); END;`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = dm.db.Exec(`DROP TRIGGER IF EXISTS ` + trigger)
		_, _ = dm.db.Exec(`DROP TRIGGER IF EXISTS ` + trigger + `_u`)
	})
}

// TestRepro_HandoffUpsertSurvivesContention pins audit F2: the
// session-continuity record must survive a contention window that
// outlasts the busy timeout, because it is an idempotent UPSERT keyed
// on session_id.
func TestRepro_HandoffUpsertSurvivesContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)
	release := holdWALWriteLock(t, competitor)
	defer release()

	const sessionID = "repro-handoff-session"
	done := make(chan error, 1)
	go func() {
		_, err := dm.EndSessionV2(sessionID, "", "", "summary that must survive", "clean",
			[]string{"commitment"}, []string{"open question"})
		done <- err
	}()

	// The retry must engage before the lock is released: otherwise the
	// test would pass without ever meeting the contention.
	waitFor(t, 60*time.Second, "the handoff retry to engage", func() bool {
		return dm.BusyRetryCount() > 0
	})
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the handoff upsert must survive contention, got: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the handoff upsert never completed")
	}

	persisted, err := dm.GetHandoffBySessionID(sessionID)
	if err != nil {
		t.Fatalf("reading back the handoff: %v", err)
	}
	if persisted.Summary != "summary that must survive" {
		t.Fatalf("the handoff artifact was lost under contention")
	}
}

// TestRepro_ScheduleWakeSurvivesContention pins audit F3 (first half):
// scheduling a wake is a durable intent, and the insert must survive
// contention.
func TestRepro_ScheduleWakeSurvivesContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)
	release := holdWALWriteLock(t, competitor)
	defer release()

	done := make(chan error, 1)
	go func() {
		_, err := dm.ScheduleWake("repro reason", "24h", "", "repro-test", map[string]interface{}{})
		done <- err
	}()

	waitFor(t, 60*time.Second, "the schedule retry to engage", func() bool {
		return dm.BusyRetryCount() > 0
	})
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ScheduleWake must survive contention, got: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("ScheduleWake never completed")
	}

	var n int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE reason = 'repro reason'`).Scan(&n); err != nil {
		t.Fatalf("counting the wake: %v", err)
	}
	if n != 1 {
		t.Fatalf("the scheduled wake was lost under contention")
	}
}

// TestRepro_RevertToPendingReportsFailure pins audit F5: an intent the
// materializer cannot move back to pending must be REPORTED, not
// silently stranded in `processing` forever.
func TestRepro_RevertToPendingReportsFailure(t *testing.T) {
	dm, _ := newWALTestDM(t)
	const intentID = "repro-stranded-intent"
	if _, err := dm.db.Exec(
		`INSERT INTO epistemic_cascade_outbox
			(id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
			 downstream_artifact_id, downstream_artifact_type, status)
		 VALUES (?, 'ev1', 'dead1', 'memory', 'down1', 'theory', 'processing')`,
		intentID); err != nil {
		t.Fatalf("seeding the outbox: %v", err)
	}
	blockTable(t, dm, "epistemic_cascade_outbox", "repro_block_outbox")

	cm := NewCascadeMaterializer(dm, CascadeMaterializerOptions{})
	if err := cm.revertToPending(intentID); err == nil {
		t.Fatalf("a revert that did not land must be reported, not swallowed: " +
			"the intent would stay `processing` forever with no reconciler looking for it")
	} else if !strings.Contains(err.Error(), intentID) {
		t.Fatalf("the error must name the stranded intent, got: %v", err)
	}
}

// TestRepro_MarkWakeScheduledSurvivesContention pins audit F3 (second
// half) as a survival property: the booking flip already propagates
// its error, but without a retry budget a contention window loses it
// and the reconciler schedules the wake a second time.
func TestRepro_MarkWakeScheduledSurvivesContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)
	const intentID = "repro-unbooked-intent"
	if _, err := dm.db.Exec(
		`INSERT INTO epistemic_cascade_outbox
			(id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
			 downstream_artifact_id, downstream_artifact_type, status, wake_scheduled)
		 VALUES (?, 'ev2', 'dead2', 'memory', 'down2', 'theory', 'materialized', 0)`,
		intentID); err != nil {
		t.Fatalf("seeding the outbox: %v", err)
	}
	release := holdWALWriteLock(t, competitor)
	defer release()

	cm := NewCascadeMaterializer(dm, CascadeMaterializerOptions{})
	done := make(chan error, 1)
	go func() {
		done <- cm.markWakeScheduled(intentID)
	}()

	waitFor(t, 60*time.Second, "the booking retry to engage", func() bool {
		return dm.BusyRetryCount() > 0
	})
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("markWakeScheduled must survive contention, got: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("markWakeScheduled never completed")
	}

	var booked int
	if err := dm.db.QueryRow(
		`SELECT wake_scheduled FROM epistemic_cascade_outbox WHERE id = ?`, intentID).Scan(&booked); err != nil {
		t.Fatalf("reading the booking: %v", err)
	}
	if booked != 1 {
		t.Fatalf("the booking flip was lost under contention")
	}
}

// TestRepro_PoisonPillPauseReportsFailure pins the dropped error in
// ProcessScheduledTasks: a cron task that should have been paused keeps
// running and keeps re-injecting wakes when the pause write is lost.
func TestRepro_PoisonPillPauseReportsFailure(t *testing.T) {
	dm, _ := newWALTestDM(t)
	if _, err := dm.db.Exec(
		`INSERT INTO scheduled_tasks (id, name, cron_expr, directive_id, status, next_run_at)
		 VALUES ('repro-poison', 'repro poison', 'not a cron expression', 'repro-directive', 'active', 0)`); err != nil {
		t.Fatalf("seeding the poison task: %v", err)
	}
	// Block only the pause UPDATE's target state, nothing else: the
	// INSERT of the wake must still succeed so the test reaches the
	// pause write deterministically.
	if _, err := dm.db.Exec(
		`CREATE TRIGGER repro_block_pause BEFORE UPDATE OF status ON scheduled_tasks
		 BEGIN SELECT RAISE(ABORT, 'pause blocked'); END;`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = dm.db.Exec(`DROP TRIGGER IF EXISTS repro_block_pause`) })

	if _, err := ProcessScheduledTasks(dm.db); err == nil {
		t.Fatalf("a pause that did not land must be reported: " +
			"the poison task keeps running and keeps re-injecting wakes")
	} else if !strings.Contains(err.Error(), "repro-poison") {
		t.Fatalf("the error must name the task that could not be paused, got: %v", err)
	}
}

// TestRepro_BackfillSessionIDsDoesNotLie pins audit F8: the counter
// must count updates that happened, and the failure must reach the
// caller — not a success count for rows that were never written.
func TestRepro_BackfillSessionIDsDoesNotLie(t *testing.T) {
	dm, _ := newWALTestDM(t)
	store := &MemoryStore{DB: &SQLiteConnection{DB: dm.db}, DM: dm}
	const memTime = "2026-10-20T12:00:00Z"
	if _, err := dm.db.Exec(
		`INSERT INTO memories (id, collection, content, created_at) VALUES ('repro-orphan', 'memories', 'x', ?)`,
		memTime); err != nil {
		t.Fatalf("seeding the orphan memory: %v", err)
	}
	sessionsJSON := `{"agent:main:main": {"sessions": [
		{"sessionId": "s1", "sessionFile": "s1.jsonl",
		 "startedAt": "2026-10-20T11:00:00Z", "updatedAt": "2026-10-20T13:00:00Z"}
	]}}`
	jsonPath := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(jsonPath, []byte(sessionsJSON), 0600); err != nil {
		t.Fatal(err)
	}

	// Refuse the UPDATE while letting everything else through, so the
	// backfill deterministically meets a failing write.
	if _, err := dm.db.Exec(
		`CREATE TRIGGER repro_block_backfill BEFORE UPDATE OF session_id ON memories
		 BEGIN SELECT RAISE(ABORT, 'backfill blocked'); END;`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = dm.db.Exec(`DROP TRIGGER IF EXISTS repro_block_backfill`) })

	updated, err := store.BackfillSessionIDs(jsonPath)
	if err == nil {
		t.Fatalf("a backfill whose write did not land must report the failure, not (1, nil)")
	}
	if updated != 0 {
		t.Fatalf("the counter must count updates that happened: got %d, want 0", updated)
	}
	var sid sql.NullString
	if qerr := dm.db.QueryRow(`SELECT session_id FROM memories WHERE id = 'repro-orphan'`).Scan(&sid); qerr != nil {
		t.Fatalf("reading the orphan: %v", qerr)
	}
	if sid.Valid && sid.String != "" {
		t.Fatalf("the orphan must still have no session_id")
	}
}
