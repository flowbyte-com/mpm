// evidence_migration.go — alpha: add 'work' to evidence.artifact_type CHECK.
//
// The work_events and work verification system requires evidence rows to reference
// work artifacts (artifact_type = 'work'). The evidence table was created before
// 'work' was added to the artifact_type enum, so existing databases have a
// CHECK constraint that only allows ('memory','theory','decision','lesson').
//
// SQLite does not support ALTER TABLE for CHECK constraints, so the migration
// uses a transactional table recreation (same pattern as
// migrateAuditLevelConstraint in audit.go and migrateArtifactProvenanceWorkType
// in provenance_migration.go).
//
// BEGIN
//   1. Preserve any triggers on memories that reference the evidence table.
//   2. CREATE TABLE evidence_new (... new CHECK with 'work' ...)
//   3. INSERT INTO evidence_new SELECT <explicit column list> FROM evidence
//      (never SELECT * — see the note at the copy step)
//   4. DROP TABLE evidence
//   5. ALTER TABLE evidence_new RENAME TO evidence
//   6. Recreate the preserved triggers (SQLite updates internal references).
//   7. Recreate the 5 evidence indexes.
// COMMIT
//
// Idempotent: reads the current CREATE TABLE from sqlite_master and skips
// if 'work' is already in the CHECK.
package internal

import (
	"database/sql"
	"fmt"
	"strings"
)

// migrateEvidenceWorkType extends the evidence.artifact_type CHECK constraint
// to include 'work'. Called from initUnifiedSchema; must succeed before any
// handler runs so that evidence can be attached to work artifacts.
func (dm *DatabaseManager) migrateEvidenceWorkType() error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("db not initialized")
	}

	// 1. Read current CREATE TABLE to check if 'work' is already accepted.
	var createSQL string
	err := dm.db.QueryRow(`
		SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'evidence'
	`).Scan(&createSQL)
	if err == sql.ErrNoRows {
		// Table doesn't exist yet — CREATE TABLE IF NOT EXISTS will apply
		// the correct constraint on first run. No-op.
		return nil
	}
	if err != nil {
		return fmt.Errorf("migrateEvidenceWorkType: read sqlite_master: %w", err)
	}

	// 2. Idempotency check.
	if strings.Contains(createSQL, "'work'") {
		return nil
	}

	// 3. Preserve any triggers that reference the evidence table (e.g.
	// memory_source_evidence_ai on memories inserts into evidence). SQLite
	// fires these during the rename step and they reference "evidence" by
	// name, so we must drop them before rename and recreate after.
	// SQLite's internal references are updated automatically on recreation.
	triggerNames, triggerSQLs, err := dm.preserveTriggersReferencing("evidence")
	if err != nil {
		return fmt.Errorf("migrateEvidenceWorkType: preserve triggers: %w", err)
	}

	// 4. Transactional table recreation.
	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("migrateEvidenceWorkType: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Self-heal the column this migration's copy list depends on.
	// SafeMigrations normally adds reference_url before we get here, but
	// that loop only slog.Warn's on failure — it does not abort init — so
	// a database whose ALTER failed is reachable. Without this, the copy
	// below would fail with "no such column: reference_url". ADD COLUMN is
	// idempotent here (duplicate-column error is swallowed), so running it
	// unconditionally is safe on both a fresh and an already-migrated table.
	if _, err := tx.Exec(`ALTER TABLE evidence ADD COLUMN reference_url TEXT`); err != nil {
		if !isDuplicateColumnError(err) {
			return fmt.Errorf("migrateEvidenceWorkType: ensure reference_url: %w", err)
		}
	}

	const newDDL = `CREATE TABLE evidence_new (
		id                  TEXT PRIMARY KEY,
		artifact_id         TEXT NOT NULL,
		artifact_type       TEXT NOT NULL CHECK (artifact_type IN ('memory','theory','decision','lesson','work')),
		type                TEXT NOT NULL,
		source_group        TEXT NOT NULL,
		strength            REAL NOT NULL CHECK (strength >= -1.0 AND strength <= 1.0),
		independence_factor REAL NOT NULL DEFAULT 1.0,
		created_by          TEXT NOT NULL,
		created_at          INTEGER NOT NULL,
		expires_at          INTEGER,
		notes               TEXT,
		-- reference_url must be listed here. SafeMigrations runs BEFORE this
		-- function (db.go: SafeMigrations loop, then migrateEvidenceWorkType),
		-- so on a legacy database the column has ALREADY been added by the
		-- time this recreate runs. Omitting it would drop every stored
		-- reference URL on the way through.
		-- NOTE: every line of a SQL comment must use the SQL dash-dash form.
		-- A slash-slash line inside this string is not a comment to SQLite
		-- and aborts the CREATE with a syntax error.
		reference_url       TEXT
	)`

	if _, err := tx.Exec(newDDL); err != nil {
		return fmt.Errorf("migrateEvidenceWorkType: create new table: %w", err)
	}

	// Copy all rows (evidence rows are artifact-type-restricted at insert time,
	// so no existing row can violate the new CHECK).
	//
	// The column list is explicit, never `SELECT *`. This function hardcodes
	// the destination table's shape in newDDL while the source table's shape
	// is whatever the live database plus SafeMigrations have accumulated. A
	// `SELECT *` here silently couples the two: any column added to `evidence`
	// between BaseTables and this migration (reference_url, and every future
	// addition) makes the copy supply more values than evidence_new declares,
	// and the migration fails at boot with an arity error. Naming the columns
	// makes that coupling impossible.
	if _, err := tx.Exec(`
		INSERT INTO evidence_new
			(id, artifact_id, artifact_type, type, source_group, strength,
			 independence_factor, created_by, created_at, expires_at,
			 notes, reference_url)
		SELECT
			id, artifact_id, artifact_type, type, source_group, strength,
			independence_factor, created_by, created_at, expires_at,
			notes, reference_url
		FROM evidence
	`); err != nil {
		return fmt.Errorf("migrateEvidenceWorkType: copy rows: %w", err)
	}

	// Drop preserved triggers BEFORE the rename so they don't fire during it.
	for _, name := range triggerNames {
			if _, err := tx.Exec(`DROP TRIGGER ` + name); err != nil {
			return fmt.Errorf("migrateEvidenceWorkType: drop trigger %s: %w", name, err)
		}
	}

	if _, err := tx.Exec(`DROP TABLE evidence`); err != nil {
		return fmt.Errorf("migrateEvidenceWorkType: drop old table: %w", err)
	}

	if _, err := tx.Exec(`ALTER TABLE evidence_new RENAME TO evidence`); err != nil {
		return fmt.Errorf("migrateEvidenceWorkType: rename: %w", err)
	}

	// Recreate the preserved triggers. SQLite updates internal table references
	// in the trigger bodies automatically on recreation.
	for i, sqlText := range triggerSQLs {
		if _, err := tx.Exec(sqlText); err != nil {
			return fmt.Errorf("migrateEvidenceWorkType: recreate trigger %s: %w", triggerNames[i], err)
		}
	}

	// Rebuild all 5 indexes. CommonIndexes uses CREATE INDEX IF NOT EXISTS so
	// this is safe to run on fresh installs too.
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_evidence_artifact ON evidence(artifact_id, artifact_type)`,
		`CREATE INDEX IF NOT EXISTS idx_evidence_type ON evidence(type)`,
		`CREATE INDEX IF NOT EXISTS idx_evidence_source ON evidence(source_group)`,
		`CREATE INDEX IF NOT EXISTS idx_evidence_creator ON evidence(created_by)`,
		`CREATE INDEX IF NOT EXISTS idx_evidence_expires ON evidence(expires_at)`,
	}
	for _, idx := range indexes {
		if _, err := tx.Exec(idx); err != nil {
			return fmt.Errorf("migrateEvidenceWorkType: create index: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrateEvidenceWorkType: commit: %w", err)
	}
	return nil
}

// preserveTriggersReferencing returns names and full SQL for every trigger in
// the database whose body references tableName. Used to preserve triggers that
// INSERT INTO evidence (or another table being migrated) so they can be
// restored after the rename step.
func (dm *DatabaseManager) preserveTriggersReferencing(tableName string) (names, sqls []string, err error) {
	rows, err := dm.db.Query(`
		SELECT name, sql FROM sqlite_master
		WHERE type = 'trigger' AND sql LIKE '%' || ? || '%'
	`, tableName)
	if err != nil {
		return nil, nil, fmt.Errorf("preserveTriggersReferencing: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n, s string
		if err := rows.Scan(&n, &s); err != nil {
			return nil, nil, fmt.Errorf("preserveTriggersReferencing: scan: %w", err)
		}
		names = append(names, n)
		sqls = append(sqls, s)
	}
	return names, sqls, rows.Err()
}
