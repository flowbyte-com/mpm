package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// MigrateExpiresAtZeroToNull normalizes legacy `expires_at = 0` rows to
// NULL, the current no-expiry sentinel.
//
// Background: pre-2026-08-19 code paths wrote `expires_at = 0` for "no
// TTL" or as the legacy expiry artifact (SQLite dynamic typing stores the
// INTEGER 0 as a distinct storage class from NULL), while every read path
// with a TTL filter — GetMemory, GetMemoriesByRelevance, HybridSearch,
// wake-context retrieval — predicates on `expires_at IS NULL OR expires_at
// > now`. Zero is never a meaningful expiry timestamp, but it matches the
// "expired" predicate, so legacy rows carrying it were invisible to the
// retrieval layer while still being counted in stats — and, worse, they
// were one `mpm prune` (PruneExpired: `expires_at IS NOT NULL AND
// expires_at < now`) away from hard deletion. Live-DB inspection on
// 2026-08-20 found 328 such rows, including prime directives at weight 100
// that are load-bearing for agent behavior (ReadDirectives ignores the TTL
// clause, so they were live but exposed to the prune hazard).
//
// Safety: `expires_at = 0` matches every storage form of zero under
// INTEGER affinity (INTEGER 0, REAL 0.0, TEXT '0'), never NULL, and never
// a real TTL epoch (SetMemoryTTL writes time.Unix(), always > 0 for
// non-zero times). Converting zero to NULL is lossless — zero is not a
// meaningful expiration timestamp.
//
// Idempotency: sentinel-guarded via schema_migrations
// (expires_at_zero_normalized_v1), INSERT OR IGNORE so two concurrent
// processes converge cleanly. Runs inside the same initUnifiedSchema
// transaction as the other migrations, before any handler executes.
func MigrateExpiresAtZeroToNull(tx *sql.Tx) error {
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"expires_at_zero_normalized_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check expires_at_zero_normalized_v1 sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}

	if _, err := tx.Exec(`UPDATE memories SET expires_at = NULL WHERE expires_at = 0`); err != nil {
		return fmt.Errorf("normalize expires_at=0 to NULL: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"expires_at_zero_normalized_v1",
		time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record expires_at_zero_normalized_v1 sentinel: %w", err)
	}
	return nil
}