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
// SELF-HEALING SHAPE
// ------------------
//
// The five columns AND the index are ENSURED on every open, not only on
// the first. The sentinel records that the migration ran; it is not a
// licence to stop repairing.
//
// The reason is the index. In Stage A it has no reader: its predicate
// is `delivery_state IS NOT NULL`, and every row is NULL, so the partial
// index matches ZERO rows and does nothing except enforce the
// forward-only rule. That makes it the single most droppable object in
// the schema — "clean up unused indexes" is precisely the tidy-up a
// reviewer of a storage-only stage would propose. A sentinel-guarded
// migration would never notice the loss, and the schema's one enforced
// invariant would be silently and permanently gone.
//
// So the order is: shape first, sentinel last. The columns are probed
// one by one (columnExists) and added if missing, the index is created
// if absent, and only then is the sentinel considered. A database
// interrupted between the ALTERs and the sentinel converges on the next
// open instead of failing on a duplicate column; a database whose index
// was dropped gets it back; and a database whose axis was dropped
// entirely (index first, then the column — the only order in which the
// column can go) gets the whole axis back, NULL-defaulted and
// unread at this stage, so restoring the shape is lossless.
//
// Two limits, stated rather than hidden:
//
//   - The repair restores the SHAPE, not the values. The columns are
//     NULL-defaulted and nothing reads or writes them yet, so no data
//     can be lost by a restore. A later stage that writes the axis
//     must revisit this: after that, a dropped column would carry
//     data, and restoring it would be lossy.
//   - `CREATE INDEX IF NOT EXISTS` cannot detect a same-named index
//     with a DIFFERENT definition; SQLite leaves it alone. Detecting
//     that would mean reading sqlite_master and comparing the SQL. The
//     only way to get one is an external hand-edit, so the repair
//     covers the dropped-index case, which is the realistic one.
//
// Every failure here is returned, and the caller runs this inside the
// FATAL migration transaction: a failure rolls the whole migration back
// and NewDatabaseManager returns an error. Nothing here is best-effort
// and nothing here is swallowed.
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// m1SentinelID is the schema_migrations sentinel M1 records. It marks
// "this migration ran"; it is not a licence to stop repairing the
// shape.
const m1SentinelID = "scheduled_wakes_delivery_state_v1"

// m1IndexName is the partial index M1 creates. It is the sole
// enforcement of the forward-only rule, so the migration recreates it
// on every open.
const m1IndexName = "idx_sw_delivery_claim"

// deliveryStateIndexDDL is the exact DDL of the partial index.
//
// It is a constant, not an inline literal, so that the migration and
// the tests create the index from ONE definition. That matters for
// more than tidiness: SQLite stores the statement text it was given in
// sqlite_master, so two paths that spell the index differently produce
// two databases whose schemas differ in TEXT while being identical in
// substance. A schema-equivalence assertion is only meaningful if
// both sides were built from the same text.
const deliveryStateIndexDDL = `CREATE INDEX IF NOT EXISTS idx_sw_delivery_claim
	 ON scheduled_wakes(delivery_state, lease_expires_at)
	 WHERE delivery_state IS NOT NULL`

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
// sentinel.
//
// The SHAPE (the columns and the index) is ensured on every open; the
// sentinel is recorded once. Both are idempotent by construction, so
// the order of the two is a matter of what must never be lost: the
// sentinel is written LAST, because it is the "this migration ran"
// record and must not exist for a migration that did not finish, and
// the shape is ensured FIRST, because the index is the only thing
// enforcing the forward-only rule and a dropped index must be repaired
// even on a database whose sentinel is already present.
//
// Every error is returned. The caller runs this inside the FATAL
// migration transaction, so a failure rolls the whole migration back
// and NewDatabaseManager returns an error.
func MigrateScheduledWakesDeliveryState(tx *sql.Tx) error {
	if err := ensureDeliveryStateColumns(tx); err != nil {
		return err
	}
	if err := ensureDeliveryStateIndex(tx); err != nil {
		return err
	}

	// The sentinel goes LAST: it is the "this migration ran" record,
	// and it must not be written for a migration that did not finish.
	var applied int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		m1SentinelID,
	).Scan(&applied); err != nil {
		return fmt.Errorf("check %s sentinel: %w", m1SentinelID, err)
	}
	if applied > 0 {
		return nil
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		m1SentinelID, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record %s sentinel: %w", m1SentinelID, err)
	}
	return nil
}

// ensureDeliveryStateColumns adds the five axis columns if they are not
// already present.
//
// Each column is probed first, so a database interrupted between the
// ALTERs and the sentinel converges on the next open rather than
// failing on a duplicate column. The same probe is what repairs a
// database whose axis was dropped out from under a migrated schema.
func ensureDeliveryStateColumns(tx *sql.Tx) error {
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
	return nil
}

// ensureDeliveryStateIndex creates the partial index if it is absent.
//
// The reclaim predicate is `delivery_state = 'claimed' AND
// lease_expires_at < ?`. The partial index makes it a range scan over
// a set that is EMPTY at migration time (no backfill), so creating it
// is instant regardless of table size.
//
// It is also what makes the schema forward-only: while this index
// exists, SQLite refuses `ALTER TABLE scheduled_wakes DROP COLUMN
// delivery_state`.
//
// This runs on EVERY open, not only on the first. The index has no
// reader in Stage A — it matches zero rows and exists purely to
// enforce the forward-only rule — which makes it the object in this
// schema most likely to be "cleaned up" by someone who believes it is
// unused. A sentinel-guarded migration would never notice the loss.
func ensureDeliveryStateIndex(tx *sql.Tx) error {
	if _, err := tx.Exec(deliveryStateIndexDDL); err != nil {
		return fmt.Errorf("create %s: %w", m1IndexName, err)
	}
	return nil
}
