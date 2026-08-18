package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// MigrateLastAccessedAtToUnixEpoch is a focused, idempotent sweep that
// converts any TEXT-stored `last_accessed_at` values on the `memories` and
// `shared.memories` tables to INTEGER Unix-epoch seconds. The
// 2026-07-07 timestamps_unified_v1 migration already normalizes the
// column, but the broader sweep only ran once per database and several
// write paths continued to insert `CURRENT_TIMESTAMP` (SQLite TEXT
// format) for the column, leaving a window in which a fresh write could
// reintroduce the drift on top of a previously-cleaned database.
//
// This migration is the focused remediation: a one-shot pass that
// converts any TEXT residue back to INTEGER so the
// `converting driver.Value type time.Time to a int64` scan failures
// reported by GC and doctor stop firing.
//
// Idempotent via the `last_accessed_at_drift_v1` sentinel. The
// typeof() guard skips already-integer rows so re-running is a no-op.
//
// Scope: only the `memories` and `shared.memories` tables. The shared
// `memories` is a structural mirror of the main `memories` (same DDL
// via attachShared), so it carries the same drift and the same fix.
// Broadcast / contradiction_log / bcast_* tables have their own
// intentionally TEXT timestamp columns — out of scope here.
//
// Caller (DatabaseManager.initUnifiedSchema) wraps in a transaction:
//
//	tx.Begin()
//	MigrateLastAccessedAtToUnixEpoch(tx)
//	tx.Commit()
func MigrateLastAccessedAtToUnixEpoch(tx *sql.Tx) error {
	// 1. Sentinel check — bail if already applied.
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"last_accessed_at_drift_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check last_accessed_at_drift_v1 sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}

	// 2. Normalize the main `memories.last_accessed_at` column. The
	//    typeof() guard skips already-integer rows; the strftime() guard
	//    protects against junk that SQLite can't parse (in which case
	//    strftime returns NULL — we leave the row untouched so it
	//    surfaces via doctor rather than being silently zeroed).
	if _, err := tx.Exec(`
		UPDATE memories
		SET last_accessed_at = CAST(strftime('%s', last_accessed_at) AS INTEGER)
		WHERE last_accessed_at IS NOT NULL
		  AND typeof(last_accessed_at) = 'text'
		  AND strftime('%s', last_accessed_at) IS NOT NULL
	`); err != nil {
		return fmt.Errorf("normalize memories.last_accessed_at: %w", err)
	}

	// 3. Normalize `shared.memories.last_accessed_at`. The shared DB is
	//    attached but not always present (test paths / fresh DBs skip
	//    attachShared). Probe pragma_table_info on the shared schema
	//    before writing — a missing table is a clean skip, not an error.
	if _, err := tx.Exec(`
		UPDATE shared.memories
		SET last_accessed_at = CAST(strftime('%s', last_accessed_at) AS INTEGER)
		WHERE last_accessed_at IS NOT NULL
		  AND typeof(last_accessed_at) = 'text'
		  AND strftime('%s', last_accessed_at) IS NOT NULL
	`); err != nil {
		// shared.memories absent on test / single-DB paths — surface
		// only if the error is anything other than "no such table".
		if !isNoSuchTableError(err) {
			return fmt.Errorf("normalize shared.memories.last_accessed_at: %w", err)
		}
	}

	// 4. Record sentinel so future runs short-circuit.
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"last_accessed_at_drift_v1",
		time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record last_accessed_at_drift_v1 sentinel: %w", err)
	}
	return nil
}
