package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// MigrateDeletedAtZeroToNull normalizes legacy `deleted_at = 0` rows to
// NULL, the current soft-delete sentinel.
//
// Background: pre-2026-08-19 code paths wrote `deleted_at = 0` for "not
// deleted" (SQLite dynamic typing stores the INTEGER 0 as a distinct
// storage class from NULL), while every read path — HybridSearch,
// GetMemory, HealthStats, memory expiry, the FTS triggers — predicates on
// `deleted_at IS NULL`. The result is the amputation failure documented
// in the 2026-08-19 tiered-fallback-seeding fix: 327 live memories
// (including three real operator prime directives) were invisible to the
// entire retrieval layer while the health check reported a handful of
// "active" rows. The 2026-08-19 live-DB repair converted these rows
// manually; this migration makes the conversion idempotent at boot so a
// restored backup or an unmigrated database file can never silently
// vanish from retrieval again.
//
// Safety: `deleted_at = 0` matches every storage form of zero under
// INTEGER affinity (INTEGER 0, REAL 0.0, TEXT '0'), never NULL, and never
// a real soft-delete epoch (soft-deletes write strftime('%s','now'),
// always > 0). Converting zero to NULL is lossless — zero is not a
// meaningful delete timestamp.
//
// Idempotency: sentinel-guarded via schema_migrations
// (deleted_at_zero_normalized_v1), INSERT OR IGNORE so two concurrent
// processes converge cleanly. Runs inside the same initUnifiedSchema
// transaction as the other migrations, before any handler executes.
func MigrateDeletedAtZeroToNull(tx *sql.Tx) error {
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"deleted_at_zero_normalized_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check deleted_at_zero_normalized_v1 sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}

	if _, err := tx.Exec(`UPDATE memories SET deleted_at = NULL WHERE deleted_at = 0`); err != nil {
		return fmt.Errorf("normalize deleted_at=0 to NULL: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"deleted_at_zero_normalized_v1",
		time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record deleted_at_zero_normalized_v1 sentinel: %w", err)
	}
	return nil
}