package internal

// wake_settlement.go — the SYSTEM-kind terminal transition for a
// scheduled wake.
//
// WHY THIS FILE EXISTS
// --------------------
// The scheduler settles a wake by setting `fired = 1`. It used to do
// that with a bare `db.Exec` — no statement-level retry budget.
//
// The settlement is not an ordinary write. It is the ONLY record that a
// wake was delivered. If it is lost:
//
//   - the wake stays `fired = 0`;
//   - the scheduler's due-wake query (`WHERE fired = 0 AND target_time
//     <= ?`) selects it again;
//   - and the wake's HANDLER RUNS A SECOND TIME.
//
// A lost artifact is recoverable — the agent can write it again. A
// duplicated side effect is not: a broadcast fans out to other agents,
// a drill spawns a harness, a cron task injects a wake. Once they have
// happened, they cannot be un-done.
//
// So the settlement carries the substrate's documented budget of 5.
// Ordinary contention is a WAIT, and waiting it out is exactly what the
// budget buys.
//
// WHY IT LIVES HERE AND NOT IN THE SCHEDULER
// -------------------------------------------
// `scheduled_wakes` is a core table. Core already owns ScheduleWake,
// ResolveWake and CheckPendingWakes. The scheduler's settlement was a
// duplicate of that ownership, written against a raw pool because the
// scheduler holds a *sql.DB rather than a DatabaseManager.
//
// This file gives the scheduler the operation it needs, in the package
// that owns the table, with the substrate's own retry policy applied —
// and with ONE implementation of that policy (`execTracked`), rather
// than a second retry loop living in the scheduler.
//
// WHAT THIS DOES NOT ESTABLISH
// ----------------------------
// It does NOT make wake delivery exactly-once.
//
// The scheduler is a singleton (flock on a PID file), so two schedulers
// never execute the same wake concurrently. What remains is a CRASH
// window: if the process dies between the handler completing and the
// settlement landing, the wake is still `fired = 0` and the next
// process executes the handler again.
//
// Closing that window needs a delivery-state redesign — a durable claim
// with a lease, plus handlers that are idempotent or carry their own
// deduplication — not a retry budget. The audit record for that is
// docs/dev/issue-178-corrections-phase-1-2026-10-29.md. Until it is
// done, the honest statement is: delivery is AT-MOST-ONCE for a
// handler that fails, and AT-LEAST-ONCE across a crash.

import (
	"database/sql"
	"time"
)

// WakeSettlementRetryBudget is the statement-level retry budget for the
// wake settlement.
//
// It is the documented budget (5) that every other write in the
// substrate carries. It is NOT a second budget layered on top of
// another: the settlement is a single pool statement with no enclosing
// transaction, so the statement layer is the layer that owns the
// ordinary busy class for this path.
const WakeSettlementRetryBudget = 5

// MarkWakeFired marks a scheduled wake terminal: `fired = 1`, with the
// error recorded in the wake's metadata when the handler failed.
//
// It is idempotent in the sense every settlement needs: re-running it
// records the same terminal state again. It is NOT a claim — it does
// not check whether the wake was already fired, because the caller
// (the scheduler) has already run the handler by the time it settles,
// and refusing to settle because the row already says `fired = 1`
// would turn a duplicate settlement into a lost one.
//
// The write is a single statement, so it is atomic by itself: it either
// lands whole or not at all. There is no COMMIT to be ambiguous about.
func MarkWakeFired(db *sql.DB, wakeID string, lastError string) error {
	if db == nil {
		return nil
	}
	if wakeID == "" {
		return nil
	}

	now := time.Now().Unix()
	var out execOutcome
	if lastError == "" {
		out = execTracked(db, `
			UPDATE scheduled_wakes
			SET fired = 1, fired_at = ?,
			  metadata = json_set(COALESCE(metadata,'{}'), '$.last_error', NULL)
			WHERE id = ?`,
			WakeSettlementRetryBudget, nil, now, wakeID)
	} else {
		out = execTracked(db, `
			UPDATE scheduled_wakes
			SET fired = 1, fired_at = ?,
			  metadata = json_set(COALESCE(metadata,'{}'), '$.last_error', ?)
			WHERE id = ?`,
			WakeSettlementRetryBudget, nil, now, lastError, wakeID)
	}
	return out.Err
}
