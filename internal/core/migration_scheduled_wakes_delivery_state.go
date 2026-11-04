// migration_scheduled_wakes_delivery_state.go — upgrade-in-place
// migration for the scheduler's delivery-state axis (issue #178,
// Stage A / M1).
//
// Adds five nullable columns to scheduled_wakes and one partial index:
//
//	delivery_state     TEXT     NULL | 'claimed' | 'delivered'
//	lease_expires_at   INTEGER
//	claimed_by         TEXT     fencing token
//	claimed_at         INTEGER
//	delivered_at       INTEGER
//
//	CREATE INDEX IF NOT EXISTS idx_sw_delivery_claim
//	  ON scheduled_wakes(delivery_state, lease_expires_at)
//	  WHERE delivery_state IS NOT NULL;
//
// WHAT THIS MIGRATION IS — AND IS NOT
// -----------------------------------
//
// This is the FIRST stage of the delivery-state redesign specified in
// issue #178 and in PR #219 (docs/dev/issue-178-delivery-state-design-2026-11-04.md,
// which lands with that PR). It is STRICTLY ADDITIVE and
// BEHAVIOUR-NEUTRAL:
//
//   - no row is modified. There is no backfill, and there must never be
//     one: no column on the row records "the handler ran" (fired_by is
//     written only by the resolver, dispatched_at only by the
//     notification claim, and metadata.last_error is set to NULL on a
//     SUCCESSFUL settlement), so a backfill would fabricate a delivery
//     for the four retirement paths that never ran a handler — and for
//     kind='cron' rows, which are never deliverable at all;
//   - no reader, no writer and no predicate changes. The axis is
//     carried by columns nothing reads yet;
//   - delivery_state starts NULL on every row, and NULL is the honest
//     value: "not under scheduler-dispatch tracking".
//
// The base DDL in internal/core/schema.go is deliberately NOT changed.
// `dispatched_at` and `fired_by` are migration-only columns too, and
// adding these five to the base DDL would create a second definition
// of them and a drift risk. A fresh database and a migrated database
// therefore have literally identical schemas, produced by one code
// path.
//
// WHERE IT RUNS
// -------------
//
// Inside the FATAL migration transaction in internal/core/db.go, next
// to MigrateWakeResolverFiredBy and MigrateScheduledWakesDispatchedAt.
// It is NOT in SafeMigrations: that loop is best-effort and silences
// errors (it only warns), and a silently-missing column is far worse
// than a failed open. A failure here rolls the whole migration back
// and NewDatabaseManager returns an error.
//
// ROLLBACK
// ---------
//
// Binary rollback is SAFE: the columns are additive and NULL-defaulted,
// so an older binary ignores them.
//
// Schema rollback is FORBIDDEN, and the schema enforces it: with the
// partial index present, `ALTER TABLE scheduled_wakes DROP COLUMN
// delivery_state` is refused by SQLite ("error in index
// idx_sw_delivery_claim after drop column: no such column:
// delivery_state"). Dropping the index first does permit the rewrite —
// which is exactly why the index must not be dropped.
//
// Idempotent via the scheduled_wakes_delivery_state_v1 sentinel, and
// convergent: each column is probed with columnExists first, so a
// partially-migrated database (a crash between the ALTERs and the
// sentinel) converges on the next open instead of failing on a
// duplicate column.
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// deliveryStateColumns are the five columns M1 adds to scheduled_wakes,
// in the order they are added. The order is the column order of a
// migrated table, so it is part of the schema's observable shape.
var deliveryStateColumns = []struct {
	name string
	decl string
}{
	// delivery_state is the axis itself. NULL = "not under
	// scheduler-dispatch tracking": every notification wake, every
	// retired wake and every wake this binary has not yet dispatched.
	// There is deliberately NO 'pending' state — nothing ever writes
	// it, and the eligibility question ("is it due?") is answered by
	// target_time, not by a state.
	{name: "delivery_state", decl: "TEXT"},
	// lease_expires_at is the claim's lease expiry, in Unix-epoch
	// seconds, evaluated on the DB clock.
	{name: "lease_expires_at", decl: "INTEGER"},
	// claimed_by is the claim owner's fencing token, not an
	// observability field: a settlement must match it or it is
	// refused.
	{name: "claimed_by", decl: "TEXT"},
	// claimed_at is when the claim was taken, in Unix-epoch seconds.
	{name: "claimed_at", decl: "INTEGER"},
	// delivered_at is when the handler's effect completed, in
	// Unix-epoch seconds.
	{name: "delivered_at", decl: "INTEGER"},
}

// MigrateScheduledWakesDeliveryState adds the delivery-state axis to
// scheduled_wakes: five nullable columns, one partial index, one
// sentinel. Idempotent via sentinel; convergent via column probes.
func MigrateScheduledWakesDeliveryState(tx *sql.Tx) error {
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"scheduled_wakes_delivery_state_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check scheduled_wakes_delivery_state_v1 sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}

	// The five columns, each probed first: a database interrupted
	// between the ALTERs and the sentinel (a crash, a rolled-back
	// transaction) must converge on the next open rather than fail on
	// a duplicate column.
	for _, col := range deliveryStateColumns {
		exists, err := columnExists(tx, "scheduled_wakes", col.name)
		if err != nil {
			return fmt.Errorf("probe %s column: %w", col.name, err)
		}
		if exists {
			continue
		}
		// Every column is nullable with no default: NULL is the
		// honest value for a row this axis has never touched, and a
		// default would fabricate delivery history on every existing
		// row.
		if _, err := tx.Exec(
			fmt.Sprintf("ALTER TABLE scheduled_wakes ADD COLUMN %s %s", col.name, col.decl),
		); err != nil {
			return fmt.Errorf("add %s column: %w", col.name, err)
		}
	}

	// The reclaim predicate is `delivery_state = 'claimed' AND
	// lease_expires_at < ?`. The partial index makes it a range scan
	// over a set that is EMPTY at migration time (no backfill), so
	// creating it is instant regardless of table size.
	//
	// It is also what makes the schema forward-only: while this
	// index exists, SQLite refuses `ALTER TABLE scheduled_wakes DROP
	// COLUMN delivery_state`.
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_sw_delivery_claim
		 ON scheduled_wakes(delivery_state, lease_expires_at)
		 WHERE delivery_state IS NOT NULL`,
	); err != nil {
		return fmt.Errorf("create idx_sw_delivery_claim: %w", err)
	}

	// The sentinel goes LAST: it is the "this migration ran" record,
	// and it must not be written for a migration that did not finish.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"scheduled_wakes_delivery_state_v1", time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record scheduled_wakes_delivery_state_v1 sentinel: %w", err)
	}
	return nil
}
