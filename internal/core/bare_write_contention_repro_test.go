package internal

// bare_write_contention_repro_test.go — issue #178's reproductions.
//
// WHAT THIS FILE IS
// -----------------
// Issue #178 claims that a direct pool write (`dm.db.Exec(...)`) loses
// its artifact under ordinary lock contention, because such a write has
// no statement-level retry budget — only SQLite's own `_busy_timeout`
// from the production DSN. When the contention outlasts that timeout,
// the write fails and the artifact is gone.
//
// A claim like that is only worth something if something makes it true.
// This file is that something: four deterministic reproductions on an
// isolated, file-backed WAL database using the PRODUCTION DSN, so the
// `_busy_timeout=5000` behaviour under test is the behaviour the
// substrate actually ships.
//
// THE REPRODUCTIONS
// -----------------
//   1. TestRepro_BarePoolWriteLosesArtifactUnderOrdinaryContention
//      The claim itself: a bare pool write fails under contention and
//      the artifact never lands.
//
//   2. TestRepro_GuardedPoolWriteSurvivesTheSameContention
//      The negative control: the IDENTICAL statement routed through
//      `ExecTracked(query, 5, ...)` survives the same contention. This
//      is what makes (1) a defect rather than a law of nature.
//
//   3. TestRepro_SwallowedWriteLosesArtifactWithNoSignal
//      The severity discriminator: a site whose error is DROPPED loses
//      the artifact with no error, no log line, no counter — the caller
//      cannot know the write was lost.
//
//   4. TestRepro_RetryingABusyStatementDoesNotDoubleApply
//      The correction-strategy precondition: a statement that fails
//      with SQLITE_BUSY had NO effect, so adding a retry budget to an
//      incrementing statement is safe. This is why the fix is a retry
//      budget and not "the caller should try again".
//
// WHAT THIS FILE DOES NOT CLAIM
// -----------------------------
// These reproductions exercise the substrate's own primitives. They are
// not a claim that any specific production call site is wrong — that
// classification lives in the audit document, keyed by the site's
// transaction ownership, error handling and reachability.

import (
	"database/sql"
	"testing"
	"time"
)

// bareWriteBusyWindow is the production `_busy_timeout` (5000ms) as
// carried by the DSN in db.go. A bare pool write gets exactly this long
// to acquire the write lock and no more.
const bareWriteBusyWindow = 5 * time.Second

// TestRepro_BarePoolWriteLosesArtifactUnderOrdinaryContention proves
// the defect claim in issue #178.
//
// A competitor connection holds the WAL write lock. The bare pool write
// waits out the production busy timeout, fails, and the artifact — a
// memory row, the substrate's most important durable object — is not
// written. The error IS returned, so a caller that checks it can react;
// but there is no retry, so the write is simply lost.
func TestRepro_BarePoolWriteLosesArtifactUnderOrdinaryContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	release := holdWALWriteLock(t, competitor)
	defer release()

	const id = "repro-bare-pool-loss"
	_, err := dm.db.Exec(
		`INSERT INTO memories (id, collection, content, created_at) VALUES (?, 'memories', ?, 0)`,
		id, "the artifact that must survive",
	)

	// The write fails.
	if err == nil {
		t.Fatalf("expected the bare pool write to fail under contention, got nil error")
	}
	if !IsBusyError(err) {
		t.Fatalf("expected an ordinary busy error (not a snapshot conflict), got: %v", err)
	}
	// The class matters. A SQLITE_BUSY_SNAPSHOT is NOT clearable by
	// retrying the statement — only a restart of the transaction clears
	// it — so a snapshot conflict would NOT be the defect #178 is about.
	// This must be the ordinary, wait-clearable class.
	if IsBusySnapshotError(err) {
		t.Fatalf("expected ordinary contention, got a stale-snapshot conflict: %v", err)
	}
	t.Logf("bare pool write failed with: %v", err)

	// And the artifact is gone.
	release()
	var n int
	if qErr := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, id).Scan(&n); qErr != nil {
		t.Fatalf("counting the artifact: %v", qErr)
	}
	if n != 0 {
		t.Fatalf("expected the artifact to be ABSENT after the failed write, found %d row(s)", n)
	}
}

// TestRepro_GuardedPoolWriteSurvivesTheSameContention is the negative
// control that makes the previous test a defect rather than a law of
// nature.
//
// The statement is IDENTICAL. The only difference is that it is routed
// through `ExecTracked(query, 5, ...)`, which retries an ordinary busy
// error. The lock is released only AFTER the first attempt has already
// burned the full production busy window, so the guarded path can only
// succeed by actually retrying.
//
// The test is deterministic: it waits for the retry counter to move
// before releasing the lock, so it never depends on a sleep landing in
// the right place.
func TestRepro_GuardedPoolWriteSurvivesTheSameContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	release := holdWALWriteLock(t, competitor)
	defer release()

	const id = "repro-guarded-pool-survives"
	done := make(chan error, 1)
	go func() {
		_, err := dm.ExecTracked(
			`INSERT INTO memories (id, collection, content, created_at) VALUES (?, 'memories', ?, 0)`,
			5, id, "the artifact that must survive",
		)
		done <- err
	}()

	// The first attempt has now failed and the retry budget has taken
	// over. That is the moment the bare write would already have given
	// up.
	waitFor(t, 60*time.Second, "the statement-level retry to engage", func() bool {
		return dm.BusyRetryCount() > 0
	})

	// Release the lock: the retry is now able to acquire it.
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected the guarded write to succeed after the lock was released, got: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the guarded write never completed")
	}

	// The artifact landed.
	var n int
	if qErr := dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, id).Scan(&n); qErr != nil {
		t.Fatalf("counting the artifact: %v", qErr)
	}
	if n != 1 {
		t.Fatalf("expected the artifact to be PRESENT after the guarded write, found %d row(s)", n)
	}
}

// TestRepro_SwallowedWriteLosesArtifactWithNoSignal is the severity
// discriminator.
//
// Not every bare pool write is equally bad. A write whose error is
// CHECKED and PROPAGATED at least tells the caller the artifact was
// lost; the caller can then decide. A write whose error is DROPPED
// loses the artifact with no error, no log line and no counter — and
// the caller cannot know.
//
// `markMemorySynthCooldown` (internal/core/synthesis_auto.go) is a
// real instance of the second shape: it stamps `last_synthesized_at`
// on a memory so the near-miss detector does not re-merge it. Its
// error is discarded, and its consequence is a DUPLICATE synthesis —
// the substrate re-merges a memory it had already merged.
func TestRepro_SwallowedWriteLosesArtifactWithNoSignal(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	const memID = "repro-swallowed-cooldown"
	if _, err := dm.db.Exec(
		`INSERT INTO memories (id, collection, content, created_at) VALUES (?, 'memories', ?, 0)`,
		memID, "a memory awaiting its synthesis cooldown",
	); err != nil {
		t.Fatalf("seeding the memory: %v", err)
	}

	release := holdWALWriteLock(t, competitor)
	defer release()

	// The function has no error result at all. There is nothing for the
	// caller to check, log, or retry on.
	markMemorySynthCooldown(dm, memID)

	release()

	var lastSynth sql.NullInt64
	if err := dm.db.QueryRow(
		`SELECT last_synthesized_at FROM memories WHERE id = ?`, memID,
	).Scan(&lastSynth); err != nil {
		t.Fatalf("reading the cooldown stamp: %v", err)
	}
	if lastSynth.Valid {
		t.Fatalf("expected the cooldown stamp to be LOST, found %d", lastSynth.Int64)
	}
}

// TestRepro_RetryingABusyStatementDoesNotDoubleApply is the
// precondition for the correction strategy.
//
// Several of the sites in the audit are COUNTER INCREMENTS
// (`success_count = success_count + 1`, `run_count = run_count + 1`,
// `reinforcement_count = reinforcement_count + ?`). The instinct is
// that adding a retry budget to an increment is dangerous, because a
// retry might apply the increment twice.
//
// It is not. A statement that returns SQLITE_BUSY had NO effect: SQLite
// refuses to start the write while another writer holds the lock. The
// retry therefore applies the increment exactly once.
//
// This matters for the recommendation: the correct correction for an
// incrementing site is a retry budget, not "leave it alone because it
// is not idempotent". (The genuinely non-idempotent case is a retry at
// the TRANSACTION level, where the callback is replayed — and
// `WithTx` already owns that budget.)
func TestRepro_RetryingABusyStatementDoesNotDoubleApply(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	const nodeID = "repro-increment-node"
	if _, err := dm.db.Exec(
		`INSERT INTO retrieval_metadata (node_id, node_type, reuse_count, success_count)
		 VALUES (?, 'memory', 0, 0)`,
		nodeID,
	); err != nil {
		t.Fatalf("seeding retrieval_metadata: %v", err)
	}

	release := holdWALWriteLock(t, competitor)
	defer release()

	done := make(chan error, 1)
	go func() {
		// Exactly the shape of retrieval_metadata.go's IncrementSuccess.
		_, err := dm.ExecTracked(`
			INSERT INTO retrieval_metadata (node_id, node_type, reuse_count, last_retrieved_at, success_count)
			VALUES (?, ?, 0, NULL, 1)
			ON CONFLICT(node_id) DO UPDATE SET
				success_count = retrieval_metadata.success_count + 1,
				updated_at = CAST(strftime('%s','now') AS INTEGER)
		`, 5, nodeID, "memory")
		done <- err
	}()

	waitFor(t, 60*time.Second, "the statement-level retry to engage", func() bool {
		return dm.BusyRetryCount() > 0
	})
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected the incrementing write to succeed, got: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the incrementing write never completed")
	}

	var successCount int
	if err := dm.db.QueryRow(
		`SELECT success_count FROM retrieval_metadata WHERE node_id = ?`, nodeID,
	).Scan(&successCount); err != nil {
		t.Fatalf("reading success_count: %v", err)
	}
	if successCount != 1 {
		t.Fatalf("expected success_count == 1 (a busy statement has no effect, so a retry cannot double-apply), got %d", successCount)
	}
}
