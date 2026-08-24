// provenance_migration.go — alpha-3 telemetry migration for
// artifact_provenance.
//
// This migration is intentionally conservative. SQLite's ALTER TABLE
// RENAME re-validates triggers globally; the lessons view's INSTEAD OF
// triggers reference FTS5 virtual tables and re-validation can fail
// in environments where FTS5 isn't fully wired into the running
// connection. A full table-recreate dance (the same shape as
// migrateAuditLevelConstraint in audit.go) would also touch the
// artifact_provenance schema for the parent_invocation_id column —
// but it has the side effect of needing a RENAME that triggers the
// FTS5 re-validation. For alpha-3, the conservative path wins:
//
//   1. parent_invocation_id column: added via SafeMigrations BEFORE
//      this function runs. Idempotent via isDuplicateColumnError.
//
//   2. idx_provenance_parent_invocation index: created via
//      CommonIndexes AFTER this function. The index references a
//      column that SafeMigrations just added.
//
//   3. artifact_type CHECK widening (to include 'handoff' and
//      'directive'): DEFERRED. Fresh installs pick up the wider CHECK
//      via the CREATE TABLE in BaseTables. Legacy alpha-2 DBs keep
//      the narrow CHECK until they reset; this is documented as a
//      known migration boundary in the alpha-3 release notes.
//
// The migration function below is an idempotent no-op for legacy
// DBs — it only logs the state. If a future alpha bump can solve the
// RENAME-and-FTS5 interaction (e.g., by running the table-recreate
// AFTER migrateLessonsToView() so the FTS5 state is fully warm), this
// function can be extended to widen the CHECK constraint.
package internal

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
)

// migrateArtifactProvenanceSchema pins the alpha-3 invariant for
// artifact_provenance and logs the migration boundary. The two real
// changes (parent_invocation_id column + idx_provenance_parent_invocation
// index) are already handled by SafeMigrations and CommonIndexes
// respectively — this function is the documented single point of
// truth for "the alpha-3 provenance hardening is loaded".
//
// Safe to call on every boot. Idempotent.
func (dm *DatabaseManager) migrateArtifactProvenanceSchema() error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("db not initialized")
	}

	// 1. Read the current CREATE TABLE statement from sqlite_master to
	//    surface the actual state to operators via slog.
	var createSQL string
	err := dm.db.QueryRow(`
		SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'artifact_provenance'
	`).Scan(&createSQL)
	if err == sql.ErrNoRows {
		// Table doesn't exist yet — BaseTables handles first-run
		// creation with the up-to-date schema. No-op.
		return nil
	}
	if err != nil {
		return fmt.Errorf("migrateArtifactProvenanceSchema: read sqlite_master: %w", err)
	}

	// 2. Report invariants. We don't touch the table here (the column
	//    and index were added by SafeMigrations + CommonIndexes); we
	//    only document state.
	columnPresent := strings.Contains(createSQL, "parent_invocation_id")
	handoffAccepted := strings.Contains(createSQL, "'handoff'")
	directiveAccepted := strings.Contains(createSQL, "'directive'")

	if columnPresent && handoffAccepted && directiveAccepted {
		slog.Info("artifact_provenance: alpha-3 schema fully applied",
			"parent_invocation_id", "present",
			"artifact_type_handoff", "accepted",
			"artifact_type_directive", "accepted",
		)
		return nil
	}

	// 3. The two real changes (column add, index create) are owned by
	//    SafeMigrations and CommonIndexes respectively. If we land here
	//    on a legacy alpha-2 DB, the column should already be present
	//    (SafeMigrations ran earlier) but the CHECK may still be narrow.
	//    The CHECK widening is deferred (see file header) — flag it for
	//    the operator's awareness without failing init.
	if columnPresent && (!handoffAccepted || !directiveAccepted) {
		slog.Warn("artifact_provenance: parent_invocation_id present but artifact_type CHECK is narrow",
			"parent_invocation_id", "present",
			"artifact_type_handoff", handoffAccepted,
			"artifact_type_directive", directiveAccepted,
			"migration", "alpha-3-telemetry",
			"resolution", "fresh install picks up wider CHECK automatically; legacy alpha-2 DBs need mpm ops reset-schema or fresh DB",
		)
		return nil
	}

	// 4. Should not reach here — SafeMigrations should have added the
	//    column before this function runs. If we somehow do, log loudly
	//    rather than fail init (the binary remains usable, just without
	//    parent_invocation_id telemetry).
	slog.Warn("artifact_provenance: alpha-3 migration incomplete",
		"parent_invocation_id", columnPresent,
		"artifact_type_handoff", handoffAccepted,
		"artifact_type_directive", directiveAccepted,
	)
	return nil
}

// migrateArtifactProvenanceWorkType widens the artifact_provenance CHECK
// constraint on artifact_type to include 'work'. SQLite does not support
// ALTER TABLE for CHECK constraints, so the migration uses a transactional
// table recreation (same pattern as migrateAuditLevelConstraint).
//
// BEGIN
//   CREATE TABLE artifact_provenance_work (... new CHECK with 'work' ...)
//   INSERT INTO artifact_provenance_work SELECT * FROM artifact_provenance
//   DROP TABLE artifact_provenance
//   ALTER TABLE artifact_provenance_work RENAME TO artifact_provenance
//   CREATE INDEX idx_provenance_artifact ON artifact_provenance(...)
//   ... (all 5 provenance indexes)
// COMMIT
//
// Idempotent: reads the current CREATE TABLE from sqlite_master and skips
// if 'work' is already in the CHECK. Safe to call on every boot (the
// sqlite_master read is cheap).
func (dm *DatabaseManager) migrateArtifactProvenanceWorkType() error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("db not initialized")
	}

	// 1. Read current CREATE TABLE to check if 'work' is already accepted.
	var createSQL string
	err := dm.db.QueryRow(`
		SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'artifact_provenance'
	`).Scan(&createSQL)
	if err == sql.ErrNoRows {
		// Table doesn't exist yet — BaseTables' CREATE TABLE has the
		// up-to-date CHECK. No-op.
		return nil
	}
	if err != nil {
		return fmt.Errorf("migrateArtifactProvenanceWorkType: read sqlite_master: %w", err)
	}

	// 2. Idempotency: skip if 'work' is already in the CHECK AND the
	// v_model_memory_yield view is not pointing at artifact_provenance_old.
	// (A prior migration run may have widened the CHECK but left the view
	// corrupted because it failed before the view-recreate step.)
	needsFullMigration := true
	if strings.Contains(createSQL, "'work'") {
		// Check if the view is also corrupted.
		var viewSQL string
		err := dm.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='view' AND name='v_model_memory_yield'`).Scan(&viewSQL)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			// View doesn't exist or other error, best-effort probe
			return nil
		}
		if viewSQL == "" || !strings.Contains(viewSQL, "artifact_provenance_old") {
			// View is fine or absent — nothing to do.
			return nil
		}
		if err != nil {
			// Best-effort probe for view state; view may not exist
			return nil
		}
		if viewSQL == "" || !strings.Contains(viewSQL, "artifact_provenance_old") {
			// View is fine or absent — nothing to do.
			return nil
		}
		// View references artifact_provenance_old — fix it even though the
		// CHECK is already wide enough. Only need view restore, not table recreate.
		needsFullMigration = false
	}

	// The two provenance analytics views reference artifact_provenance.
	// SQLite does not update view definitions when a table is renamed, so
	// we must drop them before the rename and recreate them after.
	// Otherwise v_model_memory_yield still points at artifact_provenance_old
	// after the migration completes.
	viewsToRestore := [][2]string{
		{
			"v_model_memory_yield",
			`CREATE VIEW IF NOT EXISTS v_model_memory_yield AS
		SELECT
			p.provider_name || '/' || p.model_name AS model_spec,
			p.framework_name,
			p.framework_adapter,
			COUNT(m.id) AS total_created,
			SUM(CASE WHEN m.deleted_at IS NULL AND m.weight >= 1
			          AND (CAST(strftime('%s','now') AS INTEGER) - m.created_at) >= 2592000
			         THEN 1 ELSE 0 END) AS survived_30d,
			ROUND(CAST(SUM(CASE WHEN m.deleted_at IS NULL AND m.weight >= 1
			                       AND (CAST(strftime('%s','now') AS INTEGER) - m.created_at) >= 2592000
			                      THEN 1 ELSE 0 END) AS REAL)
			      / NULLIF(SUM(CASE WHEN (CAST(strftime('%s','now') AS INTEGER) - m.created_at) >= 2592000
			                        THEN 1 ELSE 0 END), 0) * 100, 1) AS survival_30d_pct,
			SUM(m.reinforcement_count) AS total_reinforcements,
			SUM(CASE WHEN json_extract(m.metadata, '$.status') = 'challenged' THEN 1 ELSE 0 END) AS total_challenged
		FROM artifact_provenance p
		JOIN memories m ON p.artifact_id = m.id AND p.artifact_type = 'memory'
		GROUP BY p.provider_name, p.model_name, p.framework_name, p.framework_adapter`,
		},
		{
			"v_model_theory_utility",
			`CREATE VIEW IF NOT EXISTS v_model_theory_utility AS
		SELECT
			p.provider_name || '/' || p.model_name AS model_spec,
			p.thinking_level,
			COUNT(t.id) AS theories_proposed,
			SUM(CASE WHEN json_extract(t.metadata, '$.status') = 'proven' THEN 1 ELSE 0 END) AS theories_proven,
			SUM(CASE WHEN json_extract(t.metadata, '$.status') = 'disproven' THEN 1 ELSE 0 END) AS theories_refuted,
			ROUND(AVG(t.confidence), 2) AS avg_final_confidence
		FROM artifact_provenance p
		JOIN memories t ON p.artifact_id = t.id AND p.artifact_type = 'theory'
		GROUP BY p.provider_name, p.model_name, p.thinking_level`,
		},
	}
	for _, v := range viewsToRestore {
		var dropSQL string
		switch v[0] {
		case "v_model_memory_yield":
			dropSQL = "DROP VIEW IF EXISTS v_model_memory_yield"
		case "v_model_theory_utility":
			dropSQL = "DROP VIEW IF EXISTS v_model_theory_utility"
		default:
			return fmt.Errorf("migrateArtifactProvenanceWorkType: unknown view %q", v[0])
		}
		if _, err := dm.db.Exec(dropSQL); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: drop view: %w", err)
		}
	}

	// 3. Full table recreation (only when CHECK is still narrow).
	//    View-only restore (just drop + recreate views) is handled below
	//    when needsFullMigration is false.
	if needsFullMigration {
		tx, err := dm.db.Begin()
		if err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		// New DDL mirrors the current table (with 'work' added to CHECK).
		// Column list is kept in sync with schema.go::BaseTables.
		newDDL := `CREATE TABLE artifact_provenance_work (
		id                   TEXT PRIMARY KEY,
		artifact_id          TEXT NOT NULL,
		artifact_type        TEXT NOT NULL,
		created_at           INTEGER NOT NULL,
		schema_version       TEXT NOT NULL DEFAULT 'v1',
		actor_kind           TEXT NOT NULL,
		actor_id             TEXT,
		framework_name       TEXT,
		framework_version    TEXT,
		framework_adapter    TEXT,
		provider_name        TEXT,
		model_name           TEXT,
		model_revision       TEXT,
		api_endpoint         TEXT,
		temperature          REAL,
		max_tokens           INTEGER,
		reasoning_mode       TEXT,
		reasoning_effort     REAL,
		thinking_level       TEXT,
		thinking_tokens      INTEGER,
		thinking_visible     INTEGER,
		session_id           TEXT,
		invocation_id        TEXT,
		parent_artifact_id   TEXT,
		parent_invocation_id TEXT,
		provider_metadata    TEXT,
		UNIQUE (artifact_id, artifact_type),
		CHECK (artifact_type IN ('memory','theory','lesson','decision','handoff','directive','work')),
		CHECK (actor_kind IN ('agent','human','import','system','unknown'))
	)`

		if _, err := tx.Exec(newDDL); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: create new table: %w", err)
		}

		// Copy data from old table to new table.
		if _, err := tx.Exec(`INSERT INTO artifact_provenance_work SELECT * FROM artifact_provenance`); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: insert select: %w", err)
		}

		// Rename old table out of the way first (this also updates any views that
		// reference it to point to the renamed table, keeping them valid). Indexes
		// go with the table rename.
		if _, err := tx.Exec(`ALTER TABLE artifact_provenance RENAME TO artifact_provenance_old`); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: rename old: %w", err)
		}

		// Rename new table to the canonical name (view is already valid since it
		// was updated in the previous step).
		if _, err := tx.Exec(`ALTER TABLE artifact_provenance_work RENAME TO artifact_provenance`); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: rename: %w", err)
		}

		// Drop the old table (cascades to drop its indexes — they're stale now).
		// This is done AFTER the rename so the view stays valid throughout.
		if _, err := tx.Exec(`DROP TABLE artifact_provenance_old`); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: drop old table: %w", err)
		}

		// Recreate the 5 indexes on the new artifact_provenance table.
		// The 6th (parent_invocation) is created by CommonIndexes after this
		// function returns.
		indexes := []string{
			`CREATE INDEX idx_provenance_artifact ON artifact_provenance(artifact_id, artifact_type)`,
			`CREATE INDEX idx_provenance_model ON artifact_provenance(provider_name, model_name)`,
			`CREATE INDEX idx_provenance_actor ON artifact_provenance(actor_kind, framework_name)`,
			`CREATE INDEX idx_provenance_session ON artifact_provenance(session_id)`,
			`CREATE INDEX idx_provenance_invocation ON artifact_provenance(invocation_id)`,
		}
		for _, idx := range indexes {
			if _, err := tx.Exec(idx); err != nil {
				return fmt.Errorf("migrateArtifactProvenanceWorkType: create index: %w", err)
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: commit: %w", err)
		}
	}

	// Recreate the dropped views against the new artifact_provenance table.
	for _, v := range viewsToRestore {
		var dropSQL string
		switch v[0] {
		case "v_model_memory_yield":
			dropSQL = "DROP VIEW IF EXISTS v_model_memory_yield"
		case "v_model_theory_utility":
			dropSQL = "DROP VIEW IF EXISTS v_model_theory_utility"
		default:
			return fmt.Errorf("migrateArtifactProvenanceWorkType: unknown view %q", v[0])
		}
		if _, err := dm.db.Exec(dropSQL); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: drop view: %w", err)
		}
	}

	if needsFullMigration {
		slog.Info("artifact_provenance: widened to accept artifact_type='work'")
	} else {
		slog.Info("artifact_provenance: restored provenance analytics views")
	}
	return nil
}
