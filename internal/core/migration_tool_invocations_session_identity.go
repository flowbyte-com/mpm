// migration_tool_invocations_session_identity.go — additive ALTER
// TABLE migration that introduces the `mpm_session_id` and
// `framework_session_id` columns on `tool_invocations`, plus the two
// non-unique composite indexes that match the existing
// `(session_id, started_at DESC)` shape for the new identity
// dimensions.
//
// Why
// ───
// Pre-Stage-2C.1, tool_invocations only recorded the legacy per-process
// `session_id` (a UUID generated lazily on first `getOrMakeSessionID()`
// call in cmd/mpm/session_runtime.go). That field is the right axis
// for "did these calls happen in the same CLI dispatch process" but
// the WRONG axis for "did these calls happen in the same MPM
// continuity session" or "did they share a native framework session".
//
// Recent_activity's `session_id=<...>` filter operated on that legacy
// column. Operators who wanted "this MPM session's activity" or "this
// OpenClaw session's activity" had no first-class field to filter on.
//
// This migration adds two new nullable columns:
//
//   - mpm_session_id        — canonical MPM-owned session identity.
//                             Filled from ActiveContext.MPMSessionID
//                             at audit time. NULL for rows written
//                             before this migration or when the call
//                             happened outside an active MPM session.
//   - framework_session_id  — host-owned session identifier.
//                             Filled from
//                             ActiveContext.FrameworkSessionID
//                             (= MPM_PROVENANCE_FRAMEWORK_SESSION_ID
//                             env var). NULL when the host has no
//                             native session id (Pi, Hermes without
//                             hooks, Claude Code without
//                             MPM_SESSION_ID).
//
// Migration shape
// ───────────────
//
//   1. Sentinel gate (`tool_invocations_session_identity_v1`).
//   2. Schema-shape probe via pragma_table_info — if both columns
//      already present (fresh DB after DDL flip), write sentinel
//      and return.
//   3. ADD COLUMN mpm_session_id TEXT.
//   4. ADD COLUMN framework_session_id TEXT.
//   5. CREATE INDEX idx_tool_invocations_mpm_session on
//      (mpm_session_id, completed_at DESC). IF NOT EXISTS.
//   6. CREATE INDEX idx_tool_invocations_framework_session on
//      (framework_session_id, completed_at DESC). IF NOT EXISTS.
//   7. Sentinel write.
//
// Both columns are nullable. Legacy rows are NOT backfilled — the
// brief is explicit that historical identity must come from
// authoritative context at the time of the event, never guessed
// from timestamps, framework alone, parent_invocation_id, or
// mutable current active.json. Pre-migration rows survive verbatim
// with NULL on the new dimensions; their original session_id is
// unchanged.
//
// Indexes
// ───────
//
// The composite indexes mirror the shape of the existing
// idx_tool_invocations_session on (session_id, started_at DESC).
// Routing-stage queries that filter by mpm_session_id and order by
// recency can use the new index without a filesort. Cost: two
// extra b-tree indexes per row. Justified because routing-stage
// candidate generation is a hot path; speculative indexes are not
// added beyond what the documented filter shape requires.

package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// toolInvocationsSessionIdentitySentinel is the schema_migrations
// row that gates this migration.
const toolInvocationsSessionIdentitySentinel = "tool_invocations_session_identity_v1"

// MigrateToolInvocationsSessionIdentity adds mpm_session_id and
// framework_session_id columns to tool_invocations plus the two
// composite indexes. Idempotent via the
// tool_invocations_session_identity_v1 sentinel.
//
// Caller (DatabaseManager.initUnifiedSchema) wraps this in the
// same migration transaction as the other migrations.
func MigrateToolInvocationsSessionIdentity(tx *sql.Tx) error {
	// 1. Sentinel check — bail if already applied.
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		toolInvocationsSessionIdentitySentinel,
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check %s sentinel: %w", toolInvocationsSessionIdentitySentinel, err)
	}
	if applied > 0 {
		return nil
	}

	// 2. Schema-shape probe — bail if BOTH columns already present.
	allPresent := true
	for _, col := range []string{"mpm_session_id", "framework_session_id"} {
		var count int
		err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('tool_invocations') WHERE name = ?`, col,
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("probe tool_invocations.%s: %w", col, err)
		}
		if count == 0 {
			allPresent = false
			break
		}
	}
	if allPresent {
		// Both columns present — likely a fresh DB after the DDL
		// flip but before this migration shipped. Write sentinel
		// and return; the index CREATEs use IF NOT EXISTS.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
			toolInvocationsSessionIdentitySentinel, time.Now().Unix(),
		); err != nil {
			return fmt.Errorf("record %s sentinel (already-applied path): %w", toolInvocationsSessionIdentitySentinel, err)
		}
		return nil
	}

	// 3. ADD COLUMN mpm_session_id TEXT. Nullable so legacy rows
	// remain valid and so callers outside an active MPM session
	// can still write.
	if _, err := tx.Exec(
		`ALTER TABLE tool_invocations ADD COLUMN mpm_session_id TEXT`,
	); err != nil {
		return fmt.Errorf("add column tool_invocations.mpm_session_id: %w", err)
	}

	// 4. ADD COLUMN framework_session_id TEXT. Nullable. Never
	// synthesized from mpm_session_id — invariant #6 of the brief.
	if _, err := tx.Exec(
		`ALTER TABLE tool_invocations ADD COLUMN framework_session_id TEXT`,
	); err != nil {
		return fmt.Errorf("add column tool_invocations.framework_session_id: %w", err)
	}

	// 5. Composite index on (mpm_session_id, completed_at DESC).
	//    IF NOT EXISTS guards the already-applied path.
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_tool_invocations_mpm_session
			ON tool_invocations(mpm_session_id, completed_at DESC)`,
	); err != nil {
		return fmt.Errorf("create idx_tool_invocations_mpm_session: %w", err)
	}

	// 6. Composite index on (framework_session_id, completed_at DESC).
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_tool_invocations_framework_session
			ON tool_invocations(framework_session_id, completed_at DESC)`,
	); err != nil {
		return fmt.Errorf("create idx_tool_invocations_framework_session: %w", err)
	}

	// 7. Sentinel write.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		toolInvocationsSessionIdentitySentinel, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record %s sentinel: %w", toolInvocationsSessionIdentitySentinel, err)
	}

	return nil
}
