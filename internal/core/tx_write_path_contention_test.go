package internal

// Tests for issue #164 — write paths that lose their artifact to ordinary
// SQLite lock contention.
//
// THE DEFECT CLASS
// ----------------
// Issue #158 fixed a STALE SNAPSHOT (SQLITE_BUSY_SNAPSHOT, extended code
// 517): a deferred transaction whose read snapshot went stale because
// another connection committed after the snapshot was taken. That error
// returns in microseconds and cannot be cleared by waiting.
//
// This file is about the OTHER busy class: ordinary lock contention
// (SQLITE_BUSY, extended code 5), which IS a wait and IS clearable — by
// the statement-level retry that ExecTracked performs when it is given a
// budget.
//
// Two write paths had no such budget:
//
//   - AddLesson ran its dedup probe and its write as bare `dm.db`
//     statements, with no transaction and no retry. Ordinary contention
//     lost the lesson outright.
//   - addEvidenceInTx passed `retries=0` to ExecTracked, which disables
//     the statement-level retry entirely. Ordinary contention lost the
//     evidence row.
//
// Both write DURABLE KNOWLEDGE. The evidence is worse than a lost cache
// entry: every confidence computation that depends on the observation is
// computed from a set that is missing it.
//
// THE METHOD
// ----------
// The same standard as #163: a file-backed WAL database opened with the
// PRODUCTION DSN, and deterministic contention produced by a second pool
// holding the WAL write lock.
//
// WHEN THE LOCK IS RELEASED IS THE POINT
// --------------------------------------
// The holder must be released at the moment the writer has genuinely
// collided with the lock, and not before — otherwise the busy handler
// absorbs the contention and the write succeeds with no retry at all,
// which would make the test pass vacuously.
//
// The collision is detected through the retry counter: it moves only
// AFTER a failed write attempt. That signal is shape-agnostic, which
// matters because the two transaction shapes fail differently:
//
//   - a transaction that has already READ takes a snapshot, and its
//     write cannot upgrade while the lock is held: the attempt returns
//     SQLITE_BUSY immediately, without waiting out the busy timeout;
//   - a transaction whose FIRST statement is the write waits out the
//     5000 ms busy timeout and then gets SQLITE_BUSY.
//
// So the release is scheduled on the counter's first movement. The
// sequence is:
//
//	without the fix: the write fails outright ("database is locked") and
//	                  the artifact is LOST — the test reports the
//	                  outcome, not a proxy;
//	with the fix:    the first attempt fails, the counter moves, the
//	                  holder releases, and the retry lands the artifact.
//
// Every test asserts the OUTCOME (the artifact exists) and, separately,
// that the collision was real.

import (
	"database/sql"
	"testing"
	"time"
)

// The WAL contention harness (newWALTestDM, holdWALWriteLock) is shared
// with tx_busy_snapshot_test.go — the same file-backed WAL database with
// the production DSN, and the same holder (an UNCOMMITTED write on a
// second pool over the same file). It is not re-declared here: Go
// rejects a duplicate declaration in one package, and a copy would drift.

// runUnderContention runs fn while a competing connection holds the WAL
// write lock for busyTimeoutWindow, and returns fn's error.
//
// The release is scheduled, not awaited: the writer must experience the
// contention while the lock is held, and the lock must be released while
// the writer is retrying.
func runUnderContention(t *testing.T, dm *DatabaseManager, competitor *sql.DB, fn func() error) error {
	t.Helper()

	release := holdWALWriteLock(t, competitor)
	defer release()

	errCh := make(chan error, 1)
	go func() { errCh <- fn() }()

	// Wait for the collision. The retry counter moves only after a
	// failed write attempt, so it is the signal that the writer is
	// provably inside the busy window.
	deadline := time.Now().Add(30 * time.Second)
	for dm.BusyRetryCount() == 0 {
		select {
		case err := <-errCh:
			// The write finished before any retry happened. That is
			// the pre-fix behaviour (the artifact was lost) or a
			// contention that never materialised. Either way the
			// caller reports the outcome; the release below is a
			// formality.
			release()
			return err
		default:
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	release()

	select {
	case err := <-errCh:
		return err
	case <-time.After(90 * time.Second):
		t.Fatal("the write did not complete")
		return nil
	}
}

// TestAddLessonSurvivesOrdinaryContention is the core #164 test.
//
// BEFORE the fix, this exact scenario produced:
//
//	AddLesson under contention -> database is locked
//
// and the lesson was never recorded. AFTER the fix, the write rides the
// statement-level retry budget and the lesson lands — exactly once.
func TestAddLessonSurvivesOrdinaryContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	err := runUnderContention(t, dm, competitor, func() error {
		_, err := dm.AddLesson(
			"a lesson written while another writer holds the lock",
			LessonTypeInsight,
			[]string{"tag"},
			"session-1",
		)
		return err
	})
	if err != nil {
		t.Fatalf("the lesson write must survive ordinary contention: %v", err)
	}

	// The contention was real: the retry counter moved.
	if dm.BusyRetryCount() == 0 {
		t.Fatal("the retry counter never moved: the contention never happened, so this test proves nothing")
	}

	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM lessons_base`).Scan(&n); err != nil {
		t.Fatalf("count lessons: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one lesson, got %d", n)
	}
}

// TestAddLessonWritesExactlyOneLessonUnderContention asserts the write is
// idempotent under the transaction restart.
//
// AddLesson's callback READS before it WRITES (the dedup probe), so it is
// replayable by the stale-snapshot restart. The replay must not leave two
// lessons behind: the id is generated INSIDE the callback, so a restart
// mints a fresh id and the abandoned attempt's row rolls back with its
// transaction.
func TestAddLessonWritesExactlyOneLessonUnderContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	err := runUnderContention(t, dm, competitor, func() error {
		_, err := dm.AddLesson("a lesson written while the snapshot went stale", LessonTypeInsight, nil, "session-1")
		return err
	})
	if err != nil {
		t.Fatalf("the lesson write must survive: %v", err)
	}
	if dm.BusyRetryCount() == 0 {
		t.Fatal("the retry counter never moved: the contention never happened, so this test proves nothing")
	}

	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM lessons_base`).Scan(&n); err != nil {
		t.Fatalf("count lessons: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one lesson, got %d — the write is not idempotent under a restart", n)
	}
}

// TestAddLessonReinforcesRatherThanDuplicates asserts the read-modify-write
// branch behaves under contention.
//
// A lesson with the same content is REINFORCED, not duplicated: the
// reinforcement count goes up and no second row appears. The reinforce is
// an `UPDATE ... SET reinforcement_count = reinforcement_count + 1`, so a
// transaction restart must apply it exactly once — the abandoned attempt's
// increment rolls back with its transaction.
func TestAddLessonReinforcesRatherThanDuplicates(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	const content = "a lesson that will be reinforced under contention"
	first, err := dm.AddLesson(content, LessonTypeInsight, nil, "session-1")
	if err != nil {
		t.Fatalf("first lesson: %v", err)
	}

	err = runUnderContention(t, dm, competitor, func() error {
		_, err := dm.AddLesson(content, LessonTypeInsight, nil, "session-1")
		return err
	})
	if err != nil {
		t.Fatalf("the reinforce must survive ordinary contention: %v", err)
	}
	if dm.BusyRetryCount() == 0 {
		t.Fatal("the retry counter never moved: the contention never happened, so this test proves nothing")
	}

	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM lessons_base`).Scan(&n); err != nil {
		t.Fatalf("count lessons: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected one lesson (reinforced, not duplicated), got %d", n)
	}

	stored, err := dm.GetLesson(first.ID)
	if err != nil {
		t.Fatalf("GetLesson: %v", err)
	}
	if stored.ReinforcementCount != 2 {
		t.Fatalf("expected reinforcement_count 2 (one reinforcement applied exactly once), got %d", stored.ReinforcementCount)
	}
}

// TestAddEvidenceSurvivesOrdinaryContention is the evidence half of #164.
//
// BEFORE the fix, this exact scenario produced:
//
//	AddEvidence under contention -> insert evidence: database is locked
//
// and the observation was never recorded — so every confidence
// computation that depends on it was computed from a set that is missing
// it.
func TestAddEvidenceSurvivesOrdinaryContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	// The evidence write recomputes the artifact's confidence, so the
	// artifact must exist.
	if _, err := dm.db.Exec(
		`INSERT INTO memories (id, collection, content, created_at) VALUES ('artifact-x', 'memories', 'x', ?)`,
		time.Now().Unix(),
	); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	err := runUnderContention(t, dm, competitor, func() error {
		_, err := dm.AddEvidence(EvidenceInput{
			ArtifactID:         "artifact-x",
			ArtifactType:       "memory",
			Type:               "observation",
			SourceGroup:        "api_response",
			CreatedBy:          "tester",
			IndependenceFactor: 1.0,
		})
		return err
	})
	if err != nil {
		t.Fatalf("the evidence write must survive ordinary contention: %v", err)
	}
	if dm.BusyRetryCount() == 0 {
		t.Fatal("the retry counter never moved: the contention never happened, so this test proves nothing")
	}

	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM evidence`).Scan(&n); err != nil {
		t.Fatalf("count evidence: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one evidence row, got %d", n)
	}
}

// TestAppendWorkEventStandaloneSurvivesOrdinaryContention covers the
// standalone (node == nil) branch of AppendWorkEvent.
//
// That branch used a bare `dm.db.Exec` for the work_events INSERT and
// for every works-projection UPDATE — no retry at all. It is reached by
// the evidence path's `evidence_observed` event, which is how a work
// item records that it was observed.
func TestAppendWorkEventStandaloneSurvivesOrdinaryContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	if _, err := dm.db.Exec(
		`INSERT INTO works (id, title, content, created_at) VALUES ('work-x', 'a work', 'content', ?)`,
		time.Now().Unix(),
	); err != nil {
		t.Fatalf("seed work: %v", err)
	}

	err := runUnderContention(t, dm, competitor, func() error {
		_, err := dm.AppendWorkEvent("work-x", WorkEvent{
			EventType: WorkEventTypeNoteAppended,
			Note:      "an observation recorded under contention",
		}, nil, nil)
		return err
	})
	if err != nil {
		t.Fatalf("the standalone work event must survive ordinary contention: %v", err)
	}
	if dm.BusyRetryCount() == 0 {
		t.Fatal("the retry counter never moved: the contention never happened, so this test proves nothing")
	}

	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM work_events WHERE work_id = 'work-x'`).Scan(&n); err != nil {
		t.Fatalf("count work events: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one work event, got %d", n)
	}
}

// TestDeleteReferenceSurvivesOrdinaryContention covers the reference
// delete path, whose four DELETEs asked for retries=0.
//
// A DELETE is a write, and an unretried write loses its effect to
// ordinary contention. The delete is inside a transaction, so a failed
// attempt rolls back and the retried attempt is the only one that lands.
func TestDeleteReferenceSurvivesOrdinaryContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	if _, err := dm.db.Exec(
		`INSERT INTO reference_docs (id, title, file_path, source_type, content, content_hash, import_reason, total_chunks, created_at)
		 VALUES ('doc-x', 'a doc', '/tmp/doc-x', 'file', 'content', 'hash-x', 'test', 0, ?)`,
		time.Now().Unix(),
	); err != nil {
		t.Fatalf("seed reference doc: %v", err)
	}

	err := runUnderContention(t, dm, competitor, func() error {
		return dm.DeleteReference("doc-x")
	})
	if err != nil {
		t.Fatalf("the reference delete must survive ordinary contention: %v", err)
	}
	if dm.BusyRetryCount() == 0 {
		t.Fatal("the retry counter never moved: the contention never happened, so this test proves nothing")
	}

	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM reference_docs WHERE id = 'doc-x'`).Scan(&n); err != nil {
		t.Fatalf("count reference docs: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected the document to be deleted, found %d rows", n)
	}
}

// TestOrdinaryContentionIsNotTheSnapshotClass is the negative control for
// this file.
//
// It pins the distinction the whole issue rests on: ordinary contention
// is extended code 5 and IS clearable by waiting; a stale snapshot is
// extended code 517 and is NOT. If the two were ever conflated, the
// #163 fix would start recovering the wrong class.
func TestOrdinaryContentionIsNotTheSnapshotClass(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	release := holdWALWriteLock(t, competitor)
	defer release()

	tx, err := dm.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	node := &txNode{tx: tx, dm: dm}
	_, execErr := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('writer', 'z')`, 0)
	if execErr == nil {
		t.Fatal("expected the write to conflict with the holder's lock")
	}
	if IsBusySnapshotError(execErr) {
		t.Fatalf("ordinary contention must not be classified as a snapshot conflict: %v", execErr)
	}
	if !IsBusyError(execErr) {
		t.Fatalf("ordinary contention must be classified as busy: %v", execErr)
	}
}
