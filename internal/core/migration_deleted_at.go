package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// MigrateDeletedAtToUnixEpoch converts any existing TEXT deleted_at values
// to INTEGER Unix epoch, matching the expires_at convention. Idempotent via
// the schema_migrations sentinel.
//
// Must run BEFORE any handler executes. Caller (DatabaseManager.init)
// wraps in a transaction:
//
//   tx.Begin()
//   MigrateDeletedAtToUnixEpoch(tx)
//   tx.Commit()
//
// Why INTEGER: SQLite compares INTEGER < TEXT as TRUE regardless of value,
// so mixing the two formats in the GC sweep's "deleted_at < ..." comparison
// causes premature shredding of recently-deleted rows. Unifying on integer
// matches the expires_at precedent and keeps comparisons numeric.
func MigrateDeletedAtToUnixEpoch(tx *sql.Tx) error {
	// 1. Sentinel check — bail if already applied.
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"deleted_at_unified_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}

	// 2. Convert TEXT rows to INTEGER Unix epoch. The typeof() guard
	// ensures we don't double-process rows that are already integer
	// (e.g. from a partially-applied prior migration).
	if _, err := tx.Exec(`
		UPDATE memories
		SET deleted_at = CAST(strftime('%s', deleted_at) AS INTEGER)
		WHERE deleted_at IS NOT NULL
		  AND typeof(deleted_at) = 'text'
	`); err != nil {
		return fmt.Errorf("backfill deleted_at: %w", err)
	}

	// 3. Record sentinel so future runs short-circuit.
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"deleted_at_unified_v1",
		time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record sentinel: %w", err)
	}
	return nil
}
