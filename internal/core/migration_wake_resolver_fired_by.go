// migration_wake_resolver_fired_by.go — upgrade-in-place migration
// for the explicit wake resolution surface (2026-09-19 release-pass).
//
// Adds the fired_by TEXT column to scheduled_wakes. The column records
// who/what transitioned the wake fired=1, so the audit trail can
// distinguish:
//   - cascade-materializer firings (created_by="cascade-materializer"
//     AND fired_by="cascade-materializer")
//   - cascade-reconciler crash-recovery firings
//     (created_by="cascade-reconciler" AND fired_by="cascade-reconciler")
//   - explicit operator/agent resolutions
//     (created_by="<original>" AND fired_by="wake-resolver")
//
// Idempotent via the wake_resolver_fired_by_v1 sentinel pattern.
//
// Why a separate migration rather than a DDL flip: existing on-disk
// databases created before this commit have the scheduled_wakes
// table without the column. ALTER TABLE ADD COLUMN is a SQLite O(1)
// operation (no table rewrite), so this is safe to apply at boot.
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// MigrateWakeResolverFiredBy adds the fired_by column to scheduled_wakes.
// Idempotent via sentinel.
func MigrateWakeResolverFiredBy(tx *sql.Tx) error {
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"wake_resolver_fired_by_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check wake_resolver_fired_by_v1 sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}

	exists, err := columnExists(tx, "scheduled_wakes", "fired_by")
	if err != nil {
		return fmt.Errorf("probe fired_by column: %w", err)
	}
	if !exists {
		// fired_by is optional metadata (we never assert non-null),
	 // matching the wake lifecycle semantics: an unfired wake has
	 // no fired_by. SQLite allows ADD COLUMN without NOT NULL
	 // without a default; NULL is the natural state pre-resolution.
		if _, err := tx.Exec(
			`ALTER TABLE scheduled_wakes ADD COLUMN fired_by TEXT`,
		); err != nil {
			return fmt.Errorf("add fired_by column: %w", err)
		}
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"wake_resolver_fired_by_v1", time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record wake_resolver_fired_by_v1 sentinel: %w", err)
	}
	return nil
}