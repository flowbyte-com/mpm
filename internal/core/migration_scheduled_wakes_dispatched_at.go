// migration_scheduled_wakes_dispatched_at.go — upgrade-in-place migration
// for the deadline-driven scheduler's notification-wake dispatch audit
// trail (release-blocker repair 2026-09-23).
//
// Adds the dispatched_at INTEGER column to scheduled_wakes. The column
// records when the scheduler's deadline-driven drain last attempted to
// dispatch a notification-kind row, independent of the wake's fired
// state. Pre-fix the scheduler flipped fired=1 on dispatch, which
// removed the wake from every normal delivery surface (mpm continue,
// mpm wake, mpm_context read_wake_context, the opportunistic <system_wake_notification>
// fold on every MPM call). The seeded directive
// `mpm-seed-wake-triage-policy` describes the canonical contract that
// these surfaces must satisfy; the failing implementation did not meet
// it for one-shot notification wakes scheduled via mpm_wakes schedule.
//
// New state machine:
//
//	fired=0  dispatched_at=NULL        scheduled, target_time not yet reached
//	fired=0  dispatched_at=<epoch>     scheduler tried to fire; awaiting user/agent acknowledgement
//	fired=1                             user/agent acknowledged (via CheckPendingWakes fold or explicit ResolveWake)
//
// Idempotent via the scheduled_wakes_dispatched_at_v1 sentinel pattern
// (matching migration_wake_resolver_fired_by.go).
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// MigrateScheduledWakesDispatchedAt adds the dispatched_at column to scheduled_wakes.
// Idempotent via sentinel.
func MigrateScheduledWakesDispatchedAt(tx *sql.Tx) error {
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"scheduled_wakes_dispatched_at_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check scheduled_wakes_dispatched_at_v1 sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}

	exists, err := columnExists(tx, "scheduled_wakes", "dispatched_at")
	if err != nil {
		return fmt.Errorf("probe dispatched_at column: %w", err)
	}
	if !exists {
		// dispatched_at is nullable. NULL = scheduler has not yet
		// attempted dispatch (the wake is still waiting for
		// target_time, or target_time has elapsed but the scheduler
		// hasn't ticked since). Matches the wake lifecycle
		// semantics: an unfired wake has no dispatched_at.
		if _, err := tx.Exec(
			`ALTER TABLE scheduled_wakes ADD COLUMN dispatched_at INTEGER`,
		); err != nil {
			return fmt.Errorf("add dispatched_at column: %w", err)
		}
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"scheduled_wakes_dispatched_at_v1", time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record scheduled_wakes_dispatched_at_v1 sentinel: %w", err)
	}
	return nil
}
