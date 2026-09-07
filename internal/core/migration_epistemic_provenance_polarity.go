// migration_epistemic_provenance_polarity.go — additive migration for the
// positive-direction (constructive) cascade feature.
//
// Adds a `polarity` column to the `epistemic_provenance` table with a
// CHECK constraint restricting values to NULL, 'assumes_true', or
// 'assumes_false'. The NULL default is load-bearing for the
// back-compat invariant: existing rows (none of which carry polarity)
// must never trigger a positive-direction cascade. See
// docs/constructive-cascade-design.md for the rationale.
//
// Why a separate migration rather than adding to SafeMigrations:
// SafeMigrations is `ALTER TABLE ADD COLUMN column TYPE` (column-ADD
// only, no constraint support, no idempotency sentinel). The CHECK
// constraint must be inline on the ADD COLUMN for SQLite to accept it.
// Idempotency via the schema_migrations sentinel pattern (see
// migration_cascade_wake_scheduled.go for the canonical precedent).
//
// SQLite syntax supported: ALTER TABLE ... ADD COLUMN ... TYPE ... CHECK(...).
// Verified against SQLite 3.x.
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// polarityConstraint is the inline CHECK constraint for the polarity
// column. Three values are allowed: NULL (default — back-compat, must
// never fire positive cascade), 'assumes_true' (downstream reasons as
// if upstream is true — existing invalidation cascade pattern, no
// positive trigger), 'assumes_false' (downstream reasons as if upstream
// is false/unknown — the new positive-trigger case).
//
// The constraint is written here once and quoted into the ALTER TABLE
// statement; it must stay in sync with the comment block above and
// with the discovery code (see discoverCascadeTargets in
// cascade_outbox.go for the WHERE polarity='assumes_false' filter).
// Drift here means either a silently-allowed polarity the cascade code
// doesn't know about (low harm) or a forbidden polarity the code emits
// (high harm — writes will fail). The CHECK constraint ensures the
// latter fails loudly at the storage boundary, the same pattern
// (sqlite_master.sql + transactional CREATE TABLE_NEW/INSERT/SELECT/
// DROP/RENAME) used by migrateAuditLevelConstraint. We do not need
// the table-recreation step because ADD COLUMN with inline constraint
// is the simplest path that satisfies the constraint-at-write-time
// invariant this project already pays C.1 enforcement discipline for.
const polarityConstraint = "CHECK (polarity IS NULL OR polarity IN ('assumes_true', 'assumes_false'))"

// MigrateEpistemicProvenancePolarity adds the polarity column to the
// epistemic_provenance table. Idempotent via the
// `epistemic_provenance_polarity_v1` sentinel in schema_migrations.
//
// Side-effect scope:
//   - Adds polarity TEXT NULL to epistemic_provenance (with CHECK).
//   - All existing rows read back with polarity=NULL.
//   - The existing invalidation-cascade path (which scans
//     epistemic_provenance for any row referencing a dead artifact)
//     is unaffected because its WHERE clauses don't mention polarity.
func MigrateEpistemicProvenancePolarity(tx *sql.Tx) error {
	const sentinel = "epistemic_provenance_polarity_v1"

	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		sentinel,
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check %s sentinel: %w", sentinel, err)
	}
	if applied > 0 {
		return nil
	}

	// Probe column existence via pragma. Belt-and-braces — SafeMigrations
	// doesn't list this row, so an existing DB might be missing the
	// column. probe_table_info returns zero rows for a missing table too,
	// but the table is in BaseTables, so it always exists.
	exists, err := columnExists(tx, "epistemic_provenance", "polarity")
	if err != nil {
		return fmt.Errorf("probe polarity column: %w", err)
	}
	if !exists {
		// SQLite ALTER TABLE ADD COLUMN with inline CHECK is a no-table-
		// rewrite O(1) operation. NULL default means no backfill required.
		const addSQL = `ALTER TABLE epistemic_provenance ADD COLUMN polarity TEXT ` + polarityConstraint
		if _, err := tx.Exec(addSQL); err != nil {
			return fmt.Errorf("add polarity column: %w", err)
		}
	}

	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		sentinel, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record %s sentinel: %w", sentinel, err)
	}
	return nil
}