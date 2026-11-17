// wake_fold_contention_test.go — the wake fold's settlement must
// survive a concurrent commit.
//
// WHY THIS FILE EXISTS.
//
// issue #178 (F2): the wake fold's SETTLEMENT write — the "mark fired"
// UPDATE — was, before this slice, issued inside a transaction the fold
// opened ITSELF with `dm.db.Begin()` and wrote with a raw `tx.Exec`.
//
// That shape bypasses BOTH retry layers:
//
//   - the statement-level budget cannot clear SQLITE_BUSY_SNAPSHOT,
//     because the stale snapshot belongs to the transaction and does
//     not change between attempts; and
//   - there is no enclosing `dm.WithTx` to RESTART the transaction
//     against a fresh snapshot.
//
// So a concurrent commit between the fold's SELECT and its UPDATE took
// the settlement, and the agent SILENTLY missed the wake it was owed:
// the fold returned "mark wake fired: database is locked" and the wake
// stayed pending with `fired = 0`.
//
// `CheckPendingWakes` now runs under `dm.WithTx`, and its settlement
// write carries the substrate's statement-level budget
// (`wakeFoldWriteRetries`).
//
// This file pins that recovery with the repository's OWN technique —
// the deterministic statement-boundary gate from
// `wake_resolve_contention_test.go` — and it carries a NEGATIVE CONTROL
// that transcribes the pre-fix shape, so the test cannot pass by being
// vacuous.
//
// The fold's `SQLITE_BUSY_SNAPSHOT` recovery is one of the outcomes the
// remediation mission requires to be PRESERVED, so it gets a committed
// regression test here rather than only an independent reviewer's
// untracked probe.

package internal

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCheckPendingWakes_RecoversFromAStaleSnapshot is the committed
// regression test for the fold's snapshot recovery.
//
// The interleaving is deterministic: the gate pauses the fold's UPDATE
// at PREPARE time — AFTER the transaction has taken its read snapshot
// and BEFORE the write executes — and the competing commit lands in
// that window. The fold's snapshot is then stale, and its write cannot
// upgrade.
//
// The fold must recover: no error, the wake delivered, and the wake
// SETTLED.
func TestCheckPendingWakes_RecoversFromAStaleSnapshot(t *testing.T) {
	h := newResolveContentionHarness(t, 2)
	dm := h.dm

	// A DUE agent-owned wake, so the fold selects it.
	wakeID := seedResolveContentionWake(t, dm, "notification")
	if _, err := dm.ExecTracked(
		`UPDATE scheduled_wakes SET target_time = ? WHERE id = ?`,
		5, int(time.Now().Unix()-60), wakeID,
	); err != nil {
		t.Fatalf("make the wake due: %v", err)
	}

	// The gate pauses the fold's UPDATE at PREPARE time. It is armed
	// ONCE PER CONNECTION, so the RESTARTED transaction's UPDATE is not
	// gated — which is what lets the recovery complete.
	barrier := newResolveBarrier()
	installResolveBarrier(t, h.pool, barrier, 2)

	type foldOutcome struct {
		due []map[string]interface{}
		err error
	}
	out := make(chan foldOutcome, 1)
	go func() {
		due, err := dm.CheckPendingWakes(time.Now(), nil)
		out <- foldOutcome{due: due, err: err}
	}()

	// The fold has taken its snapshot and is at its UPDATE boundary.
	select {
	case <-barrier.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the fold never reached its UPDATE boundary")
	}

	// Advance the WAL from the competitor, WITHOUT touching the fold's
	// wake: the fold's read snapshot is now stale.
	advanceWAL(t, h.competitor)

	// Let the fold's write execute against the stale snapshot.
	barrier.releaseOne()

	var res foldOutcome
	select {
	case res = <-out:
	case <-time.After(30 * time.Second):
		t.Fatal("the fold never returned")
	}

	if res.err != nil {
		t.Fatalf("the fold failed to recover from the stale snapshot: %v", res.err)
	}
	if len(res.due) != 1 {
		t.Fatalf("the fold delivered %d wake(s), want 1", len(res.due))
	}
	state := readResolveWakeState(t, dm, wakeID)
	if state.Fired != 1 {
		t.Fatalf("the wake's fired flag is %d after the fold, want 1 — the settlement was lost", state.Fired)
	}
	if !state.FiredAt.Valid {
		t.Error("the wake was settled without a fired_at stamp")
	}
}

// TestCheckPendingWakes_PreFixShapeLosesItsSettlement is the NEGATIVE
// CONTROL.
//
// It transcribes the PRE-FIX fold shape — its own `dm.db.Begin()` and
// a raw `tx.Exec` — and runs it under the SAME interleaving. The
// pre-fix shape must lose the settlement, which is what proves the
// recovery test above is not vacuous.
//
// Without this control, the recovery test could pass because the
// interleaving never produced a stale snapshot at all.
func TestCheckPendingWakes_PreFixShapeLosesItsSettlement(t *testing.T) {
	h := newResolveContentionHarness(t, 2)
	dm := h.dm

	wakeID := seedResolveContentionWake(t, dm, "notification")
	if _, err := dm.ExecTracked(
		`UPDATE scheduled_wakes SET target_time = ? WHERE id = ?`,
		5, int(time.Now().Unix()-60), wakeID,
	); err != nil {
		t.Fatalf("make the wake due: %v", err)
	}

	barrier := newResolveBarrier()
	installResolveBarrier(t, h.pool, barrier, 2)

	errCh := make(chan error, 1)
	go func() {
		errCh <- checkPendingWakesPrefixShape(dm, time.Now(), wakeID)
	}()

	select {
	case <-barrier.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the pre-fix fold never reached its UPDATE boundary")
	}

	advanceWAL(t, h.competitor)
	barrier.releaseOne()

	var err error
	select {
	case err = <-errCh:
	case <-time.After(30 * time.Second):
		t.Fatal("the pre-fix fold never returned")
	}

	if err == nil {
		t.Fatal("the pre-fix fold shape did NOT lose its settlement; the interleaving is not " +
			"producing a stale snapshot, so the recovery test is vacuous")
	}
	if !strings.Contains(err.Error(), "mark wake fired") {
		t.Fatalf("the pre-fix fold failed with %v; the expected shape is \"mark wake fired: ...\"", err)
	}
	state := readResolveWakeState(t, dm, wakeID)
	if state.Fired != 0 {
		t.Fatalf("the pre-fix fold's wake was settled (fired = %d); the pre-fix shape is supposed to lose it", state.Fired)
	}
}

// advanceWAL commits a write from the competitor pool, so the WAL
// advances and any open read snapshot becomes stale.
//
// It deliberately does NOT touch the fold's wake: the wake's settlement
// state is the thing under test.
func advanceWAL(t *testing.T, competitor *sql.DB) {
	t.Helper()
	if _, err := competitor.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		fmt.Sprintf("wk-wal-advance-%d", time.Now().UnixNano()),
		time.Now().Unix()+3600,
		"wal advance",
		"contending-writer",
		`{"kind":"notification"}`,
	); err != nil {
		t.Fatalf("competing commit: %v", err)
	}
}

// checkPendingWakesPrefixShape is the PRE-FIX fold's transaction shape,
// transcribed from main.
//
// It opens its own transaction with a bare `dm.db.Begin()` and writes
// with a raw `tx.Exec`, so it bypasses both the statement-level budget
// (which cannot clear a stale snapshot) and the enclosing `dm.WithTx`
// (which would restart it against a fresh one).
//
// It is here as a NEGATIVE CONTROL only. It is not production code and
// must never be.
func checkPendingWakesPrefixShape(dm *DatabaseManager, now time.Time, wakeID string) error {
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var id string
	if err := tx.QueryRow(
		`SELECT id FROM scheduled_wakes WHERE id = ? AND fired = 0`, wakeID,
	).Scan(&id); err != nil {
		return fmt.Errorf("select pending wake: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE scheduled_wakes SET fired = 1, fired_at = ? WHERE id = ? AND fired = 0`,
		now.Unix(), id,
	); err != nil {
		return fmt.Errorf("mark wake fired: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit wake tx: %w", err)
	}
	return nil
}
