package internal

// tx_busy_layering_contract_test.go — issue #178, phase 2.
//
// THE LAYERING DOCTRINE THIS PINS
// -------------------------------
// Exactly one layer owns each busy class:
//
//   - the STATEMENT layer (txNode.ExecTracked) owns the ordinary busy
//     that arrives WITH a wait — it retries in place with backoff;
//   - the TRANSACTION layer (WithTx) owns the STALE SNAPSHOT class —
//     it rolls back and re-runs the callback against a fresh snapshot;
//   - the OPERATION layer (e.g. capability Store.withRetry) owns the
//     ordinary busy that survives the statement budget — it re-drives
//     the whole operation.
//
// A transaction that reads before it writes, while another connection
// holds the WAL write lock uncommitted, fails IMMEDIATELY with plain
// SQLITE_BUSY (extended code 5, zero milliseconds — the busy handler is
// never invoked, because the connection already holds a read lock).
// Retrying the statement in place cannot succeed, and restarting the
// whole transaction here would multiply the operation layer's budget
// (the Store's withRetry already re-drives ordinary busy up to its own
// budget of 4; an inner restart would turn that into up to 12
// transactions). So WithTx surfaces ordinary busy to the caller after
// the statement budget is spent, exactly once per call, and the
// operation layer decides.
//
// This test pins the inner side of that contract: the callback runs
// EXACTLY ONCE for an ordinary-busy failure. The outer side is pinned
// by the capability Store's own tests
// (TestRecordInvocation_ExhaustedRetryReturnsErrTelemetryFailed expects
// exactly 4 write attempts; TestWithRetry_DoesNotRedriveAnAmbiguousCommit
// expects exactly 1 run). Together they enforce the layering from both
// sides: if anyone makes WithTx restart on ordinary busy, THIS test
// fails (the callback runs 3 times); if anyone makes the Store stop
// re-driving ordinary busy, THOSE tests fail.
//
// WHAT THIS MEANS FOR READ-THEN-WRITE TRANSACTIONS
// -------------------------------------------------
// A read-then-write transaction that meets an uncommitted write lock
// fails after burning the statement budget. The remedy is NOT a
// transaction-level restart here — it is, in order of preference:
//
//  1. take the write lock FIRST (write-first statement ordering, as
//     the phase-1 synthesis merge does: the sources' soft-delete
//     leads, so the write lock is taken where the busy handler IS
//     invoked);
//  2. let the operation layer re-drive (the Store.withRetry pattern);
//  3. accept the failure and surface it — which is what this test
//     asserts.

import (
	"testing"
	"time"
)

// TestWithRetry_RunsCallbackOnceOnOrdinaryBusy pins the transaction
// layer's half of the busy-class contract: an ordinary SQLITE_BUSY is
// returned to the caller after the statement budget is spent. It is
// never restarted here — restart belongs to the operation layer.
func TestWithRetry_RunsCallbackOnceOnOrdinaryBusy(t *testing.T) {
	dm, competitor := newWALTestDM(t)
	release := holdWALWriteLock(t, competitor)
	defer release()

	runs := 0
	done := make(chan error, 1)
	go func() {
		done <- dm.WithTx(func(node DBNode) error {
			runs++
			// Read first: this is what makes the transaction hold a
			// read lock when the write arrives, so the write fails
			// immediately with plain SQLITE_BUSY.
			var n int
			if err := node.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&n); err != nil {
				return err
			}
			_, err := node.ExecTracked(
				`INSERT INTO tx_probe (id, v) VALUES (?, 'x')`, 5, "repro-once")
			return err
		})
	}()

	// Hold past the statement budget so the outcome is determined, not
	// racy: with the lock held, every in-place retry fails immediately.
	time.Sleep(6 * time.Second)
	release()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected the ordinary-busy failure to surface to the caller")
		}
		if !IsBusyError(err) {
			t.Fatalf("expected a busy error, got: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the transaction never completed")
	}

	if runs != 1 {
		t.Fatalf("the callback ran %d times, want exactly 1 — ordinary busy "+
			"must surface to the caller (the operation layer owns the retry), "+
			"never restart here", runs)
	}

	// And nothing landed: the failed statement had no effect, and the
	// abandoned transaction was rolled back.
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM tx_probe WHERE id = 'repro-once'`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 0 {
		t.Fatalf("the failed transaction left a row behind")
	}
}
