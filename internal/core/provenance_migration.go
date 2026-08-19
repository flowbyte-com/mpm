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
