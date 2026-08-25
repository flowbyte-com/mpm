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
//   3. INSERT INTO evidence_new SELECT * FROM evidence
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
		notes               TEXT
	)`

	if _, err := tx.Exec(newDDL); err != nil {
		return fmt.Errorf("migrateEvidenceWorkType: create new table: %w", err)
	}

	// Copy all rows (evidence rows are artifact-type-restricted at insert time,
	// so no existing row can violate the new CHECK).
	if _, err := tx.Exec(`INSERT INTO evidence_new SELECT * FROM evidence`); err != nil {
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
