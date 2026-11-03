package internal

// cascade_materialization_idempotency_test.go — issue #178, phase 3.
//
// WHAT THIS FILE PINS
// -------------------
// Two defects in the cascade materializer, both of which survive the
// corrections that phases 1 and 2 landed:
//
//   1. A RE-MATERIALIZATION WAS NOT IDEMPOTENT. The theory's cascade
//      metadata carried a wall-clock `generated_at`, so two runs of the
//      same intent produced two DIFFERENT identity hashes. The save
//      layer's F19 identity probe — the mechanism that is SUPPOSED to
//      collapse an identical live save onto the existing row — therefore
//      never fired, and a re-run created a SECOND theory row: a
//      duplicated artifact asserting the same conclusion about the same
//      downstream artifact, with the first one orphaned (no outbox row
//      pointing at it) and still promotable by an operator.
//
//      The materializer's own docstring claimed the opposite — "the theory
//      is identified by the invalidation_event_id + downstream_artifact_id,
//      so a re-run produces the same theory" — so the code and its
//      documented contract disagreed.
//
//   2. THE STATE TRANSITIONS WERE NOT PROTECTED AGAINST CONTENTION.
//      markMaterialized, markFailed and requeueIntent were bare pool
//      writes with no statement-level retry budget, so an ordinary
//      SQLITE_BUSY lost the transition. The intent then stays 'processing',
//      where the restart reaper revives it — and requeueIntent is the ONLY
//      writer of attempt_count on the retry path, so a lost requeue means
//      the dead-letter cap never advances.
//
// WHY THE RE-MATERIALIZATION IS REACHABLE AT ALL
// ---------------------------------------------
// The reaper in claimCascadeIntents resets 'processing' rows older than
// five minutes back to 'pending'. So a lost transition is not a permanent
// stranding — it is a DELAY, and the delay is paid by re-running the
// materialization. Before this change, re-running it produced a duplicate
// theory.
//
// THE TECHNIQUES (both already established in this package)
// ---------------------------------------------------------
//   - SURVIVAL: hold the WAL write lock, run the real function, release
//     only after the statement-level retry has engaged
//     (BusyRetryCount > 0), and assert the transition landed. A bare pool
//     write has no retry to engage, so it fails when the hold outlasts
//     the busy timeout.
//   - CONTROL: the identical statement through the bare path, under the
//     identical contention, must FAIL with an ordinary SQLITE_BUSY
//     (extended code 5) and must not land. That proves the contention is
//     real — the test cannot pass because contention never occurred — and
//     it distinguishes the ordinary busy class (which a statement retry
//     clears) from SQLITE_BUSY_SNAPSHOT (extended 517), which it does
//     not.
//   - PROPAGATION: a transition blocked by a trigger must be REPORTED by
//     the batch report, not only in an audit line.
//
// The shared primitive (a busy statement had no effect, so a retry cannot
// double-apply) is already proven by
// TestRepro_RetryingABusyStatementDoesNotDoubleApply and is not re-proven
// here.

import (
	"errors"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

// seedCascadeIntent inserts an outbox row in the given status and returns
// the matching CascadeIntent. The created_at is fixed, because the
// materializer's cascade metadata is derived from it — a run of the
// materializer must produce the SAME metadata every time.
func seedCascadeIntent(t *testing.T, dm *DatabaseManager, id, status string, attemptCount int) CascadeIntent {
	t.Helper()
	const createdAt = int64(1_700_000_000)
	if _, err := dm.db.Exec(
		`INSERT INTO epistemic_cascade_outbox
			(id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
			 downstream_artifact_id, downstream_artifact_type, status,
			 attempt_count, created_at, updated_at)
		 VALUES (?, 'ev-repro', 'dead-repro', 'memory', 'down-repro', 'theory', ?, ?, ?, ?)`,
		id, status, attemptCount, createdAt, createdAt); err != nil {
		t.Fatalf("seeding the outbox: %v", err)
	}
	return CascadeIntent{
		ID:                     id,
		InvalidationEventID:    "ev-repro",
		DeadArtifactID:         "dead-repro",
		DeadArtifactType:       "memory",
		DownstreamArtifactID:   "down-repro",
		DownstreamArtifactType: "theory",
		CascadeDepth:           1,
		Reason:                 "theory_disproven",
		Status:                 status,
		AttemptCount:           attemptCount,
		CreatedAt:              createdAt,
		UpdatedAt:              createdAt,
	}
}

// countCascadeTheories counts the theory rows the cascade materializer
// produced for the repro's invalidation.
func countCascadeTheories(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories
		 WHERE collection = 'theories' AND json_extract(metadata, '$.cascade') = 1`).Scan(&n); err != nil {
		t.Fatalf("counting cascade theories: %v", err)
	}
	return n
}

// TestCascadeMaterialization_ReRunReusesTheSameTheory is the regression
// for defect 1: a re-materialization of the SAME intent must not create a
// second theory.
//
// It fails against the pre-fix implementation, which produced two rows and
// two ids, and passes after.
func TestCascadeMaterialization_ReRunReusesTheSameTheory(t *testing.T) {
	dm, _ := newWALTestDM(t)
	cm := NewCascadeMaterializer(dm, CascadeMaterializerOptions{})

	intent := seedCascadeIntent(t, dm, "repro-idem-intent", "processing", 0)

	first, err := cm.materializeTheory(t.Context(), intent)
	if err != nil {
		t.Fatalf("first materialization: %v", err)
	}
	if first == "" {
		t.Fatalf("the first materialization returned no theory id")
	}

	// The SECOND run is the one that happens in production when a state
	// transition is lost: the restart reaper revives the intent and the
	// materializer runs again for the SAME intent.
	//
	// The sleep is REQUIRED, not padding. The cascade metadata's
	// `generated_at` has RFC3339 SECOND resolution, so two runs inside the
	// same second produce the same stamp and the defect hides itself.
	// The reaper's window is five minutes, so a real re-materialization
	// ALWAYS crosses a second boundary — the duplicate is the normal
	// production case, and the test has to reproduce it rather than luck
	// into agreeing with the fix.
	time.Sleep(1100 * time.Millisecond)

	second, err := cm.materializeTheory(t.Context(), intent)
	if err != nil {
		t.Fatalf("re-materialization: %v", err)
	}

	if first != second {
		t.Fatalf("a re-materialization of the same intent must reuse the existing theory, "+
			"not create a second one: first=%q second=%q", first, second)
	}

	if n := countCascadeTheories(t, dm); n != 1 {
		t.Fatalf("a re-materialization must not create a duplicate theory artifact: %d cascade theories exist", n)
	}
}

// TestCascadeTransition_ABareWriteLosesTheTransitionUnderContention is
// the CONTROL for the survival tests below.
//
// It executes the identical statement through the bare pool path, under
// the identical contention, and asserts that the write is LOST with an
// ordinary SQLITE_BUSY. Without this control the survival tests could
// pass because the contention never actually bit.
func TestCascadeTransition_ABareWriteLosesTheTransitionUnderContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)
	intent := seedCascadeIntent(t, dm, "repro-bare-intent", "processing", 0)

	release := holdWALWriteLock(t, competitor)
	defer release()

	_, err := dm.db.Exec(`
		UPDATE epistemic_cascade_outbox
		SET status = 'materialized', materialized_theory_id = ?, updated_at = ?
		WHERE id = ?
	`, "theory-bare", time.Now().Unix(), intent.ID)
	if err == nil {
		t.Fatalf("the control must LOSE the write: a bare pool write has no retry budget, " +
			"so the contention must defeat it — otherwise the survival tests below prove nothing")
	}

	// The class matters: an ordinary SQLITE_BUSY (extended 5) is a WAIT
	// that a statement-level retry clears. SQLITE_BUSY_SNAPSHOT (extended
	// 517) is NOT — that class belongs to a transaction.
	var se sqlite3.Error
	if !errors.As(err, &se) {
		t.Fatalf("the control must fail with a driver error, got: %v", err)
	}
	if se.ExtendedCode != 5 {
		t.Fatalf("the control must fail with ORDINARY SQLITE_BUSY (extended 5), got extended %d "+
			"(517 would be the snapshot class, which a statement retry does not clear)", se.ExtendedCode)
	}

	// And the write must not have landed.
	var status string
	if err := dm.db.QueryRow(
		`SELECT status FROM epistemic_cascade_outbox WHERE id = ?`, intent.ID).Scan(&status); err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if status != "processing" {
		t.Fatalf("the bare write must not have landed, but the row says status=%q", status)
	}
}

// TestCascadeTransition_SurvivesOrdinaryContention is the regression for
// defect 2: each state transition must survive the ordinary contention
// that defeats a bare pool write.
//
// It fails against the pre-fix implementation, whose transitions were bare
// writes, and passes after.
func TestCascadeTransition_SurvivesOrdinaryContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)
	cm := NewCascadeMaterializer(dm, CascadeMaterializerOptions{})
	intent := seedCascadeIntent(t, dm, "repro-survive-intent", "processing", 0)

	release := holdWALWriteLock(t, competitor)
	defer release()

	done := make(chan error, 1)
	go func() {
		done <- cm.markMaterialized(intent.ID, "theory-survived")
	}()

	// Non-vacuity: the statement-level retry must actually engage. If the
	// transition returns first, the transition was LOST — that is the
	// pre-fix behaviour and it must fail the test immediately rather than
	// hang.
	deadline := time.After(45 * time.Second)
	for {
		if dm.BusyRetryCount() > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("markMaterialized must survive ordinary contention, but the transition was lost: %v", err)
		case <-deadline:
			t.Fatalf("the busy retry never engaged — the write did not meet contention at all, " +
				"so this test would pass without proving anything")
		case <-time.After(20 * time.Millisecond):
		}
	}
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("markMaterialized must survive ordinary contention, got: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("markMaterialized never completed")
	}

	// The transition landed, exactly once.
	var status, theoryID string
	if err := dm.db.QueryRow(
		`SELECT status, materialized_theory_id FROM epistemic_cascade_outbox WHERE id = ?`,
		intent.ID).Scan(&status, &theoryID); err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if status != "materialized" {
		t.Fatalf("the transition was lost under contention: status=%q", status)
	}
	if theoryID != "theory-survived" {
		t.Fatalf("the transition applied the wrong value: materialized_theory_id=%q", theoryID)
	}
}

// TestCascadeTransition_RequeueSurvivesOrdinaryContention covers the
// transition that carries the DEAD-LETTER CAP.
//
// attempt_count is written by requeueIntent and nowhere else on the retry
// path. A lost requeue means the counter does not advance, so the cap can
// never be reached for that intent.
func TestCascadeTransition_RequeueSurvivesOrdinaryContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)
	cm := NewCascadeMaterializer(dm, CascadeMaterializerOptions{})
	intent := seedCascadeIntent(t, dm, "repro-requeue-intent", "processing", 0)

	release := holdWALWriteLock(t, competitor)
	defer release()

	done := make(chan error, 1)
	go func() {
		done <- cm.requeueIntent(intent.ID, 2, time.Now().Unix()+60)
	}()

	deadline := time.After(45 * time.Second)
	for {
		if dm.BusyRetryCount() > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("requeueIntent must survive ordinary contention, but the transition was lost: %v", err)
		case <-deadline:
			t.Fatalf("the busy retry never engaged — the write did not meet contention at all")
		case <-time.After(20 * time.Millisecond):
		}
	}
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("requeueIntent must survive ordinary contention, got: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("requeueIntent never completed")
	}

	var status string
	var attemptCount int
	if err := dm.db.QueryRow(
		`SELECT status, attempt_count FROM epistemic_cascade_outbox WHERE id = ?`,
		intent.ID).Scan(&status, &attemptCount); err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if status != "pending" {
		t.Fatalf("the requeue was lost under contention: status=%q", status)
	}
	// The counter advanced EXACTLY once — the retry applies the SET once,
	// it does not increment it twice.
	if attemptCount != 2 {
		t.Fatalf("the requeue must set attempt_count exactly once, got %d (want 2)", attemptCount)
	}
}

// TestCascadeTransition_ReportsAFailureThatDidNotLand is the PROPAGATION
// property: a transition that genuinely cannot land must be visible in the
// batch report, not only in an audit line.
//
// Before the change the report said nothing about a lost transition: the
// batch returned a clean-looking summary while an intent sat in
// 'processing'.
func TestCascadeTransition_ReportsAFailureThatDidNotLand(t *testing.T) {
	dm, _ := newWALTestDM(t)
	cm := NewCascadeMaterializer(dm, CascadeMaterializerOptions{})
	intent := seedCascadeIntent(t, dm, "repro-report-intent", "processing", 0)

	// Block every write to the outbox, so the transition cannot land.
	blockTable(t, dm, "epistemic_cascade_outbox", "repro_block_outbox")

	_, err := cm.processIntent(t.Context(), intent)
	if err == nil {
		t.Fatalf("a transition that did not land must be reported to the caller")
	}
	if !contains(err.Error(), intent.ID) {
		t.Fatalf("the reported error must name the intent whose transition failed, got: %v", err)
	}
}

// TestCascadeBatch_ReportCountsATransitionThatDidNotLand is the same
// property at the batch level: the operator's summary must show that a
// transition did not land.
//
// The trigger blocks ONLY the terminal transition (status='failed'), so the
// claim transition ('processing') still lands and the batch reaches the
// transition that fails.
func TestCascadeBatch_ReportCountsATransitionThatDidNotLand(t *testing.T) {
	dm, _ := newWALTestDM(t)
	cm := NewCascadeMaterializer(dm, CascadeMaterializerOptions{})
	seedCascadeIntent(t, dm, "repro-batch-intent", "pending", 0)

	if _, err := dm.db.Exec(
		`CREATE TRIGGER repro_block_failed BEFORE UPDATE OF status ON epistemic_cascade_outbox
		 WHEN NEW.status = 'failed' BEGIN SELECT RAISE(ABORT, 'terminal transition blocked'); END;`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = dm.db.Exec(`DROP TRIGGER IF EXISTS repro_block_failed`)
	})

	report, err := cm.MaterializeBatch(t.Context(), 10)
	if err != nil {
		t.Fatalf("the batch must not fail outright: the transition failure is REPORTED, not fatal: %v", err)
	}
	if report.TransitionFailed != 1 {
		t.Fatalf("the report must count the transition that did not land: TransitionFailed=%d (want 1), "+
			"report=%+v", report.TransitionFailed, report)
	}
}
