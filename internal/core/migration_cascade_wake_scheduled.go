// migration_cascade_wake_scheduled.go — upgrade-in-place migration
// for H-3 (post-M3 audit, 2026-08-31).
//
// Adds the wake_scheduled INTEGER column to epistemic_cascade_outbox.
// Idempotent via the cascade_wake_scheduled_v1 sentinel pattern (see
// migration_timestamps.go for the canonical precedent).
//
// Why a separate migration rather than a DDL flip: existing on-disk
// databases created before this commit have the outbox table without
// the column. ALTER TABLE ADD COLUMN with NOT NULL DEFAULT 0 is a
// SQLite O(1) operation (no table rewrite), so this is safe to apply
// at boot. The sentinel pattern guarantees we don't re-run after a
// successful first application.
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// MigrateCascadeWakeScheduled adds the wake_scheduled column to the
// epistemic_cascade_outbox table. Idempotent via sentinel.
func MigrateCascadeWakeScheduled(tx *sql.Tx) error {
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"cascade_wake_scheduled_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check cascade_wake_scheduled_v1 sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}

	// Probe column existence via pragma. A missing table and a missing
	// column both return 0 rows from pragma_table_info; we don't want
	// to ADD COLUMN on a missing table (that would fail with a different
	// error and obscure the real problem). Existing databases always
	// have the outbox (it's in BaseTables), so this is belt-and-suspenders.
	exists, err := columnExists(tx, "epistemic_cascade_outbox", "wake_scheduled")
	if err != nil {
		return fmt.Errorf("probe wake_scheduled column: %w", err)
	}
	if !exists {
		if _, err := tx.Exec(
			`ALTER TABLE epistemic_cascade_outbox ADD COLUMN wake_scheduled INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("add wake_scheduled column: %w", err)
		}
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"cascade_wake_scheduled_v1", time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record cascade_wake_scheduled_v1 sentinel: %w", err)
	}
	return nil
}
