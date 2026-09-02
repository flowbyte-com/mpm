// scheduled_tasks_migration.go — alpha-5 D-12.1: alter scheduled_tasks
// timestamp column TYPEs from DATETIME to INTEGER.
//
// The mattn/go-sqlite3 driver returns Go types based on the column's
// declared TYPE, not its storage class. So even though the
// timestamps_unified_v1 migration converted the storage class of
// DATETIME-typed columns to INTEGER Unix-epoch seconds, the column
// declarations (DATETIME) tell the driver to return time.Time. Scanning
// time.Time into an int64 (or sql.NullInt64) raises:
//
//   "converting driver.Value type time.Time (\"…\") to a int64: invalid syntax"
//
// and breaks `mpm tasks list` and `mpm_wakes list_tasks` on legacy
// databases.
//
// SQLite does not support ALTER COLUMN, so the migration uses the same
// transactional table recreation pattern as migrateEvidenceWorkType /
// migrateAuditLevelConstraint:
//
// BEGIN
//   1. Read current CREATE TABLE — idempotency check
//   2. CREATE TABLE scheduled_tasks_new (... INTEGER columns ...)
//   3. INSERT INTO scheduled_tasks_new SELECT * FROM scheduled_tasks
//      — storage class is already INTEGER after timestamps_unified_v1,
//        so direct copy preserves values
//   4. DROP TABLE scheduled_tasks
//   5. ALTER TABLE scheduled_tasks_new RENAME TO scheduled_tasks
//   6. Recreate the idx_scheduled_tasks_poll index (CREATE INDEX IF NOT EXISTS
//      pattern — safe on fresh installs too)
// COMMIT
//
// Idempotency: skips if no scheduled_tasks column has DATETIME/TIMESTAMP/DATE
// type. Safe to run on every initUnifiedSchema call.

package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// migrateScheduledTasksToIntegerColumns alters scheduled_tasks timestamp
// columns from DATETIME to INTEGER. Called from initUnifiedSchema after
// timestamps_unified_v1 has finished converting storage class. Must run
// before any handler that scans scheduled_tasks timestamp columns.
func (dm *DatabaseManager) migrateScheduledTasksToIntegerColumns() error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("db not initialized")
	}

	// 1. Probe whether any of the four timestamp columns still has a
	//    legacy DECLARED TYPE. storage class may already be INTEGER
	//    (timestamps_unified_v1 converts values), but the driver uses
	//    the DECLARED TYPE — that's the structural gap.
	rows, err := dm.db.Query(`
		SELECT type FROM pragma_table_info('scheduled_tasks')
		 WHERE name IN ('last_run_at','next_run_at','created_at','updated_at')
	`)
	if err != nil {
		return fmt.Errorf("migrateScheduledTasksToIntegerColumns: probe types: %w", err)
	}
	defer rows.Close()
	var anyLegacy bool
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return fmt.Errorf("migrateScheduledTasksToIntegerColumns: scan type: %w", err)
		}
		// NUMERIC affinity covers DATETIME/DATE/TIMESTAMP; INTEGER is the
		// post-migration target.
		switch t {
		case "DATETIME", "DATE", "TIMESTAMP":
			anyLegacy = true
		}
	}
	if !anyLegacy {
		return nil // already INTEGER — no-op
	}

	// 2. Transactional table recreation. Storage class is already INTEGER
	//    (timestamps_unified_v1 ran first), so a literal row copy is value-safe.
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("migrateScheduledTasksToIntegerColumns: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const newTableDDL = `CREATE TABLE scheduled_tasks_new (
		id           TEXT PRIMARY KEY,
		name         TEXT NOT NULL,
		cron_expr    TEXT NOT NULL,
		directive_id TEXT NOT NULL,
		status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused')),
		last_run_at  INTEGER,
		next_run_at  INTEGER NOT NULL,
		created_at   INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		updated_at   INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
	)`

	if _, err := tx.Exec(newTableDDL); err != nil {
		return fmt.Errorf("migrateScheduledTasksToIntegerColumns: create new table: %w", err)
	}

	// 3. Copy all rows. Cast TEXT-stored values to INTEGER via strftime so
	//    any row the prior migration missed (shouldn't happen — DDL change
	//    in timestamps_unified_v1 was about storage class, not values) still
	//    lands as a parseable integer or NULL.
	if _, err := tx.Exec(`
		INSERT INTO scheduled_tasks_new
		    (id, name, cron_expr, directive_id, status,
		     last_run_at, next_run_at, created_at, updated_at)
		SELECT
		    id, name, cron_expr, directive_id, status,
		    CASE WHEN typeof(last_run_at)  = 'text' THEN CAST(strftime('%s', last_run_at)  AS INTEGER) ELSE last_run_at  END,
		    CASE WHEN typeof(next_run_at)  = 'text' THEN CAST(strftime('%s', next_run_at)  AS INTEGER) ELSE next_run_at  END,
		    CASE WHEN typeof(created_at)   = 'text' THEN CAST(strftime('%s', created_at)   AS INTEGER) ELSE created_at   END,
		    CASE WHEN typeof(updated_at)   = 'text' THEN CAST(strftime('%s', updated_at)   AS INTEGER) ELSE updated_at   END
		FROM scheduled_tasks
	`); err != nil {
		return fmt.Errorf("migrateScheduledTasksToIntegerColumns: copy rows: %w", err)
	}

	if _, err := tx.Exec(`DROP TABLE scheduled_tasks`); err != nil {
		return fmt.Errorf("migrateScheduledTasksToIntegerColumns: drop old table: %w", err)
	}

	if _, err := tx.Exec(`ALTER TABLE scheduled_tasks_new RENAME TO scheduled_tasks`); err != nil {
		return fmt.Errorf("migrateScheduledTasksToIntegerColumns: rename: %w", err)
	}

	// 4. Rebuild the poll index. CREATE INDEX IF NOT EXISTS — safe on fresh installs.
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_scheduled_tasks_poll ON scheduled_tasks(status, next_run_at)`); err != nil {
		return fmt.Errorf("migrateScheduledTasksToIntegerColumns: create index: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrateScheduledTasksToIntegerColumns: commit: %w", err)
	}
	return nil
}

// scanScheduledTaskTimestamp is the belt-and-suspenders guard for any
// database that survives the migration with DATETIME-typed columns. The
// mattn/go-sqlite3 driver returns time.Time when the column is declared
// DATETIME, even if the storage class is INTEGER — so a defensive scan
// helper is kept for callers that prefer runtime tolerance over relying on
// the migration alone. Returns the int64 epoch (or zero) and a "was the
// value usable" flag.
func scanScheduledTaskTimestamp(src interface{}) (int64, bool) {
	switch v := src.(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case nil:
		return 0, false
	case string:
		// Some legacy drivers return string for DATETIME columns. Parse
		// via SQLite's strftime round-trip — we don't have a direct SQL
		// connection here, so this is best-effort; the migration path
		// is the primary fix.
		return 0, false
	case time.Time:
		// mattn/go-sqlite3 returns time.Time for DATETIME columns.
		return v.Unix(), true
	}
	return 0, false
}

// guard against sql package unused-import false-positive in callers that
// only use scanScheduledTaskTimestamp.
var _ = sql.ErrNoRows