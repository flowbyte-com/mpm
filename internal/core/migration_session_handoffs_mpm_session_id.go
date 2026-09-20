// migration_session_handoffs_mpm_session_id.go — additive ALTER TABLE
// migration that introduces the `mpm_session_id` and
// `framework_session_id` columns on `session_handoffs`, plus a
// non-unique index on `mpm_session_id` for correlation queries.
//
// Why
// ───
// The pre-existing session_handoffs table has:
//   - `id`            TEXT PRIMARY KEY  (MPM-generated handoff_id)
//   - `session_id`    TEXT UNIQUE       (legacy, nullable, external)
//
// The new identity model requires three correlated columns:
//   - `id`                    — handoff_id, REQUIRED, MPM-owned PK.
//   - `mpm_session_id`        — REQUIRED going forward, MPM-owned.
//                                Sticky across CLI/MCP/process
//                                boundaries within one interaction
//                                lifecycle. NOT unique — one MPM
//                                session has many handoffs.
//   - `framework_session_id`  — OPTIONAL, host-owned. Nullable. NEVER
//                                filled with the MPM ID as a
//                                convenience. NULL when the host has
//                                no native session ID (Pi, Hermes
//                                without hooks).
//   - `session_id`            — LEGACY. Stays untouched in this pass.
//                                Future migrations may deprecate.
//
// Migration shape
// ───────────────
//
//   1. Sentinel gate (`session_handoffs_mpm_session_id_v1`).
//   2. Schema-shape probe via `pragma_table_info` — if both columns
//      already present, write sentinel and return (defensive belt-
//      and-suspenders for DBs created after DDL flip but before
//      this migration ships).
//   3. ADD COLUMN mpm_session_id TEXT (nullable).
//   4. ADD COLUMN framework_session_id TEXT (nullable).
//   5. CREATE INDEX IF NOT EXISTS idx_handoffs_mpm_session ON
//      session_handoffs(mpm_session_id). Non-unique — see header.
//   6. Write sentinel to schema_migrations (INSERT OR IGNORE for
//      concurrent-boot convergence).
//
// All statements are idempotent: ADD COLUMN has no IF NOT EXISTS in
// older SQLite, so the schema-shape probe gates re-execution before
// any DDL fires. CREATE INDEX uses IF NOT EXISTS. The sentinel
// INSERT OR IGNORE handles races between two concurrent boots.
//
// Caller (DatabaseManager.initUnifiedSchema) wraps this in the same
// migration transaction that runs the other migrations. Atomicity
// with the rest of the schema evolution is preserved.

package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// sessionHandoffsMPMSessionIDSentinel is the schema_migrations row
// that gates this migration. Bumping the suffix (e.g. _v2) is the
// way to force re-execution if a future change needs to alter the
// column shape.
const sessionHandoffsMPMSessionIDSentinel = "session_handoffs_mpm_session_id_v1"

// MigrateSessionHandoffsMPMSessionID adds `mpm_session_id` and
// `framework_session_id` columns to `session_handoffs`, plus the
// non-unique `idx_handoffs_mpm_session` index. Idempotent via the
// `session_handoffs_mpm_session_id_v1` sentinel.
//
// Caller (DatabaseManager.initUnifiedSchema) wraps in the same
// migration transaction. If any earlier migration in the tx fails
// and rolls back, this migration never commits; if this migration
// fails, all earlier work rolls back too.
func MigrateSessionHandoffsMPMSessionID(tx *sql.Tx) error {
	// 1. Sentinel check — bail if already applied.
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		sessionHandoffsMPMSessionIDSentinel,
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check %s sentinel: %w", sessionHandoffsMPMSessionIDSentinel, err)
	}
	if applied > 0 {
		return nil
	}

	// 2. Schema-shape probe — bail if BOTH columns already present. This
	// is defense-in-depth: the canonical DDL in schema.go also declares
	// these columns, so a fresh DB after the DDL flip will see them
	// at CommonIndexes time. If both columns already exist, write the
	// sentinel and return — running ADD COLUMN would error on a
	// duplicate column.
	allPresent := true
	for _, col := range []string{"mpm_session_id", "framework_session_id"} {
		var count int
		err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('session_handoffs') WHERE name = ?`, col,
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("probe session_handoffs.%s: %w", col, err)
		}
		if count == 0 {
			allPresent = false
			break
		}
	}
	if allPresent {
		// Both columns present — likely a fresh DB created after the
		// DDL flip but before this migration shipped. Write the
		// sentinel and return; the index CREATE uses IF NOT EXISTS.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
			sessionHandoffsMPMSessionIDSentinel, time.Now().Unix(),
		); err != nil {
			return fmt.Errorf("record %s sentinel (already-applied path): %w", sessionHandoffsMPMSessionIDSentinel, err)
		}
		return nil
	}

	// 3. ADD COLUMN mpm_session_id TEXT. SQLite has no ADD COLUMN IF
	// NOT EXISTS — the schema-shape probe above gates re-execution.
	// The column is intentionally nullable: existing rows have no
	// mpm_session_id, and going forward every EndSession path
	// explicitly populates this column (either via auto-resolution
	// or via caller-supplied override).
	if _, err := tx.Exec(
		`ALTER TABLE session_handoffs ADD COLUMN mpm_session_id TEXT`,
	); err != nil {
		return fmt.Errorf("add column mpm_session_id: %w", err)
	}

	// 4. ADD COLUMN framework_session_id TEXT. Nullable. Never
	// auto-populated from mpm_session_id — invariant #5 in the
	// plan: framework_session_id is host-owned, NULL when absent.
	if _, err := tx.Exec(
		`ALTER TABLE session_handoffs ADD COLUMN framework_session_id TEXT`,
	); err != nil {
		return fmt.Errorf("add column framework_session_id: %w", err)
	}

	// 5. CREATE INDEX on mpm_session_id. Non-unique: one MPM session
	// can have many handoff rows (write, scratchpad flush, EOS,
	// periodic events all share the same mpm_session_id). IF NOT
	// EXISTS guards against the already-applied case above.
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_handoffs_mpm_session ON session_handoffs(mpm_session_id)`,
	); err != nil {
		return fmt.Errorf("create idx_handoffs_mpm_session: %w", err)
	}

	// 6. Sentinel write — INSERT OR IGNORE so two concurrent boots
	// racing on this migration converge to one sentinel row.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		sessionHandoffsMPMSessionIDSentinel, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record %s sentinel: %w", sessionHandoffsMPMSessionIDSentinel, err)
	}

	return nil
}
