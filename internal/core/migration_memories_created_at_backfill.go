package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// MigrateMemoriesCreatedAtBackfill repairs rows whose created_at was
// written as NULL — a defect from the pre-2026-09-04 seed path: the
// insertSeedRow INSERT OR IGNORE omitted created_at from its column
// list, and databases that lack the ``DEFAULT (CAST(strftime('%s','now')
// AS INTEGER))`` clause on the `memories.created_at` column (legacy
// installs that went through the column-affinity rebuild without
// inheriting the DEFAULT) accepted NULL on every seeded directive.
//
// Effect: read_directives scans created_at into a Go string, which
// fails with `converting NULL to string is unsupported` on any
// NULL row. The reader fails before wake can render the prime-
// directives catalogue, so the agent boots without constitutional
// context. One row in production (mpm-seed-epistemic-compaction-policy,
// inserted 2026-09-01 by the epistemic-compaction seed commit).
//
// Backfill value: COALESCE(updated_at, CAST(strftime('%s','now') AS
// INTEGER)). updated_at is preferred when present — it tracks the last
// legitimate write — and falls back to "now" for the rare row where
// both columns are NULL (the broken seed path also omits updated_at).
//
// Idempotent: gated on schema_migrations.created_at_backfill_v1. Safe
// to call on every initUnifiedSchema run; the sentinel short-circuits
// subsequent invocations. Wrapped in the same migration tx as the
// other Migrate* calls, so the UPDATE and the sentinel INSERT are
// atomic.
//
// Pair with the writer-path correction in seed/engine.go
// (insertSeedRow now provides created_at explicitly) to prevent
// recurrence on fresh databases.
func MigrateMemoriesCreatedAtBackfill(tx *sql.Tx) error {
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"created_at_backfill_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check created_at_backfill_v1 sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}
	if _, err := tx.Exec(`
		UPDATE memories
		SET created_at = COALESCE(
			updated_at,
			CAST(strftime('%s','now') AS INTEGER)
		)
		WHERE created_at IS NULL
	`); err != nil {
		return fmt.Errorf("backfill NULL created_at: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"created_at_backfill_v1",
		time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record created_at_backfill_v1 sentinel: %w", err)
	}
	return nil
}