// provenance_migration.go — alpha-3 telemetry migration for
// artifact_provenance.
//
// Historical note, retained because it explains the shape of the fix.
//
// This migration was originally written conservatively. SQLite's ALTER TABLE
// RENAME re-validates triggers globally; the lessons view's INSTEAD OF
// triggers reference FTS5 virtual tables, and re-validation was feared to
// fail where FTS5 is not fully wired into the running connection. That fear
// is why the original version refused to touch the CHECK at all and only
// logged the state. It also described the 'handoff'/'directive' widening as
// DEFERRED.
//
// Both parts of that reasoning are now obsolete, and the file no longer
// behaves that way:
//
//  1. The CHECK widening is NOT deferred. migrateArtifactProvenanceWorkType
//     rebuilds the table inside a transaction and brings every legacy
//     database to the canonical vocabulary in
//     ProvenanceArtifactTypes. Leaving a database able to reject a
//     canonical artifact_type indefinitely is a correctness bug, not a
//     documented migration boundary.
//
//  2. The FTS5 concern does not apply to this rebuild. migrateArtifactProvenanceWorkType
//     runs BEFORE migrateLessonsToView in initUnifiedSchema (db.go), and the
//     rename it performs targets artifact_provenance — a table no FTS5
//     trigger references. The lessons FTS virtual tables are created later,
//     against the final schema, so they are never present to re-validate.
//     TestProvenanceMigration_RunsBeforeLessonsView pins that ordering.
//
// The other two alpha-3 changes are unchanged and still owned elsewhere:
//
//  1. parent_invocation_id column: added via SafeMigrations BEFORE this
//     file's migration runs. Idempotent via isDuplicateColumnError.
//
//  2. idx_provenance_parent_invocation index: created via CommonIndexes
//     AFTER this file's migration. It references a column SafeMigrations
//     just added.
package internal

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
)

// migrateArtifactProvenanceSchema reports the artifact_provenance schema
// state for operators. It is DIAGNOSTIC ONLY.
//
// Ownership of the alpha-3 provenance work is split across three functions
// by design, and this comment exists so the split is not re-litigated:
//
//   - SafeMigrations            adds the parent_invocation_id column.
//   - migrateArtifactProvenanceWorkType
//     owns the artifact_type CHECK vocabulary and the analytics views. It
//     runs immediately before this function and performs the rebuild. It is
//     the ONLY thing that changes schema here; see its doc comment.
//   - CommonIndexes             creates idx_provenance_parent_invocation.
//
// This function therefore has no schema responsibility left to own. It used
// to claim the CHECK widening was "deferred" and log a warning telling the
// operator to reset the database; that claim became false when
// migrateArtifactProvenanceWorkType gained the rebuild, and it is gone.
//
// It remains because initUnifiedSchema calls it (db.go), so it is a useful
// single place to assert the post-migration invariant in the log, and
// because removing a call the boot path depends on is a larger change than
// this audit is scoped for. It reports; it does not migrate.
//
// Safe to call on every boot. Idempotent. Never fails init.
func (dm *DatabaseManager) migrateArtifactProvenanceSchema() error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("db not initialized")
	}

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

	columnPresent := strings.Contains(createSQL, "parent_invocation_id")
	vocabCanonical := vocabularyMatchesCanonical(artifactTypeVocabulary(createSQL))
	viewsCanonical, err := dm.provenanceViewsAreCanonical()
	if err != nil {
		return fmt.Errorf("migrateArtifactProvenanceSchema: %w", err)
	}

	if columnPresent && vocabCanonical && viewsCanonical {
		slog.Info("artifact_provenance: alpha-3 schema fully applied",
			"parent_invocation_id", "present",
			"artifact_type_vocabulary", "canonical",
			"analytics_views", "canonical",
		)
		return nil
	}

	// Reached only if migrateArtifactProvenanceWorkType, which runs
	// immediately before this function, declined to act. After the
	// correctness fix that should not happen: the rebuild triggers on
	// exactly these conditions. Log loudly rather than failing init — a
	// usable database with imperfect telemetry beats a boot loop — but say
	// what is actually wrong instead of pointing at a reset that is no
	// longer the remedy.
	slog.Warn("artifact_provenance: schema not fully canonical after migration",
		"parent_invocation_id", columnPresent,
		"artifact_type_vocabulary_canonical", vocabCanonical,
		"analytics_views_canonical", viewsCanonical,
	)
	return nil
}

// provenanceViewNames are the analytics views that depend on
// artifact_provenance. Both are canonical schema objects; the migration
// treats them as one set rather than special-casing either.
var provenanceViewNames = []string{
	"v_model_memory_yield",
	"v_model_theory_utility",
}

// provenanceViewDDL returns the canonical CREATE VIEW statements from
// BaseTables, keyed by view name.
//
// The migration must not carry its own copy of this SQL. A second copy is
// exactly how the previous version of this function ended up dropping both
// views and never recreating them: the CREATE text sat in a slice, unused,
// while only the DROP half of the loop was wired up. Reading the canonical
// definition removes the failure mode — if the migration needs to create a
// view, it gets the real text, and a view added to BaseTables is picked up
// without touching this file.
func provenanceViewDDL() (map[string]string, error) {
	out := make(map[string]string, len(provenanceViewNames))
	for _, stmt := range BaseTables {
		for _, name := range provenanceViewNames {
			if strings.Contains(stmt, "CREATE VIEW IF NOT EXISTS "+name+" ") ||
				strings.Contains(stmt, "CREATE VIEW IF NOT EXISTS "+name+"\n") {
				out[name] = stmt
			}
		}
	}
	for _, name := range provenanceViewNames {
		if out[name] == "" {
			return nil, fmt.Errorf(
				"provenanceViewDDL: BaseTables no longer defines %q; the provenance "+
					"migration cannot restore it", name)
		}
	}
	return out, nil
}

// artifactTypeVocabulary extracts the accepted artifact_type values from a
// CREATE TABLE statement's artifact_type CHECK clause.
//
// Why this parses the CHECK rather than searching the whole statement.
// A substring search over the full DDL cannot distinguish:
//
//   - a value inside the CHECK from one inside an unrelated clause,
//   - a table that ACCEPTS the value from one that merely NAMES it in a
//     comment or a default.
//
// schema.go carries a SQL comment inside this very table's DDL, and
// artifact_provenance_adoption has a similar-looking CHECK on a different
// table, so a naive search is genuinely misleading here. The predicate reads
// the CHECK clause the constraint is actually enforced from.
//
// It returns nil when no artifact_type CHECK is found, which callers must
// treat as "unknown" — never as "already correct".
func artifactTypeVocabulary(createSQL string) []string {
	const marker = "CHECK (artifact_type IN ("
	start := strings.Index(createSQL, marker)
	if start < 0 {
		return nil
	}
	rest := createSQL[start+len(marker):]
	end := strings.Index(rest, ")")
	if end < 0 {
		return nil
	}
	var vocab []string
	for _, part := range strings.Split(rest[:end], ",") {
		v := strings.TrimSpace(part)
		v = strings.TrimPrefix(v, "'")
		v = strings.TrimSuffix(v, "'")
		if v != "" {
			vocab = append(vocab, v)
		}
	}
	return vocab
}

// vocabularyMatchesCanonical reports whether vocab is exactly the canonical
// artifact_type set. Order is irrelevant; membership and cardinality are not.
func vocabularyMatchesCanonical(vocab []string) bool {
	if len(vocab) != len(ProvenanceArtifactTypes) {
		return false
	}
	have := make(map[string]bool, len(vocab))
	for _, v := range vocab {
		have[v] = true
	}
	for _, want := range ProvenanceArtifactTypes {
		if !have[want] {
			return false
		}
	}
	return true
}

// provenanceViewsAreCanonical reports whether every provenance analytics view
// exists, is bound to the canonical table, and is free of any reference to
// the migration's temporary artifact_provenance_old.
//
// "Missing" counts as not-canonical on purpose. A database whose views were
// dropped by an interrupted earlier run is exactly the state this predicate
// must repair rather than skip.
func (dm *DatabaseManager) provenanceViewsAreCanonical() (bool, error) {
	for _, name := range provenanceViewNames {
		var viewSQL string
		err := dm.db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='view' AND name=?`, name).Scan(&viewSQL)
		if err == sql.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read view %q: %w", name, err)
		}
		if strings.Contains(viewSQL, "artifact_provenance_old") {
			return false, nil
		}
		if !strings.Contains(viewSQL, "artifact_provenance") {
			return false, nil
		}
	}
	return true, nil
}

// migrateArtifactProvenanceWorkType brings artifact_provenance to the
// canonical artifact_type vocabulary and repairs its analytics views.
//
// SQLite cannot ALTER a CHECK constraint, so widening the vocabulary requires
// rebuilding the table inside a transaction:
//
//	BEGIN
//	  DROP VIEW <both analytics views>
//	  CREATE TABLE artifact_provenance_work (... canonical CHECK ...)
//	  INSERT INTO artifact_provenance_work SELECT * FROM artifact_provenance
//	  DROP TABLE artifact_provenance
//	  ALTER TABLE artifact_provenance_work RENAME TO artifact_provenance
//	  CREATE INDEX ... (the five provenance indexes)
//	  CREATE VIEW ... (both analytics views, from BaseTables)
//	COMMIT
//
// Two properties this function must hold, both of which the previous version
// violated:
//
//   - The rebuild is skipped ONLY when the live CHECK vocabulary is already
//     exactly canonical AND both analytics views are already canonical.
//     Proving that one canonical word (e.g. 'work') appears somewhere in the
//     DDL is not the contract: a database can accept 'work' while still
//     rejecting 'handoff' and 'directive', and skipping it silently leaves
//     that database permanently unable to record handoffs or directives.
//
//   - Table and views migrate as ONE transactional unit. SQLite DDL is
//     transactional, so the views are dropped INSIDE the transaction: a
//     failure anywhere in the rebuild rolls the drops back with it. Dropping
//     them outside would leave a database whose table is intact but whose
//     analytics views have been destroyed, which is unrecoverable without a
//     schema rebuild.
//
// Safe to call on every boot.
func (dm *DatabaseManager) migrateArtifactProvenanceWorkType() error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("db not initialized")
	}

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

	// §18 fail-closed. A table that still accepts an artifact_type outside
	// the canonical vocabulary is carrying data this migration cannot
	// represent. Rebuilding would fail on the INSERT ... SELECT (the new
	// CHECK rejects the row), and silently dropping or coercing the row
	// would discard provenance the operator may still need. Refuse, and
	// leave the existing table and views untouched.
	if vocab := artifactTypeVocabulary(createSQL); vocab != nil && !vocabularyMatchesCanonical(vocab) {
		var unknown []string
		for _, v := range vocab {
			if !containsString(ProvenanceArtifactTypes, v) {
				unknown = append(unknown, v)
			}
		}
		if len(unknown) > 0 {
			return fmt.Errorf(
				"migrateArtifactProvenanceWorkType: artifact_provenance accepts non-canonical "+
					"artifact_type value(s) %v; refusing to migrate so the rows are neither "+
					"discarded nor coerced. Inspect the affected rows, then either correct them "+
					"or re-create the table",
				unknown)
		}
	}

	needsTableRebuild := !vocabularyMatchesCanonical(artifactTypeVocabulary(createSQL))

	viewsCanonical, err := dm.provenanceViewsAreCanonical()
	if err != nil {
		return fmt.Errorf("migrateArtifactProvenanceWorkType: %w", err)
	}

	if !needsTableRebuild && viewsCanonical {
		return nil
	}

	viewDDL, err := provenanceViewDDL()
	if err != nil {
		return fmt.Errorf("migrateArtifactProvenanceWorkType: %w", err)
	}

	tx, err := dm.db.Begin()
	if err != nil {
		return fmt.Errorf("migrateArtifactProvenanceWorkType: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Views are dropped inside the transaction so a later failure restores
	// them. SQLite does not rewrite view bodies when a table is renamed, so
	// they must be recreated against the rebuilt table regardless.
	for _, name := range provenanceViewNames {
		if _, err := tx.Exec("DROP VIEW IF EXISTS " + name); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: drop view %s: %w", name, err)
		}
	}

	if needsTableRebuild {
		// Replacement DDL. The CHECK vocabulary is generated from
		// ProvenanceArtifactTypes, the same source schema.go uses, so the
		// migration cannot widen past — or fall short of — canonical.
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
		CHECK (artifact_type IN (` + ProvenanceArtifactTypeSQL() + `)),
		CHECK (actor_kind IN ('agent','human','import','system','unknown'))
	)`

		if _, err := tx.Exec(newDDL); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: create new table: %w", err)
		}

		if _, err := tx.Exec(`INSERT INTO artifact_provenance_work SELECT * FROM artifact_provenance`); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: insert select: %w", err)
		}

		if _, err := tx.Exec(`ALTER TABLE artifact_provenance RENAME TO artifact_provenance_old`); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: rename old: %w", err)
		}

		if _, err := tx.Exec(`ALTER TABLE artifact_provenance_work RENAME TO artifact_provenance`); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: rename: %w", err)
		}

		if _, err := tx.Exec(`DROP TABLE artifact_provenance_old`); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: drop old table: %w", err)
		}

		// The five provenance indexes that belong to the rebuilt table. The
		// sixth (idx_provenance_parent_invocation) is created by
		// CommonIndexes after this function returns, because it indexes a
		// column SafeMigrations adds earlier in the boot path.
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
	}

	// Restore both analytics views inside the same transaction.
	for _, name := range provenanceViewNames {
		if _, err := tx.Exec(viewDDL[name]); err != nil {
			return fmt.Errorf("migrateArtifactProvenanceWorkType: recreate view %s: %w", name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrateArtifactProvenanceWorkType: commit: %w", err)
	}

	if needsTableRebuild {
		slog.Info("artifact_provenance: CHECK vocabulary widened to canonical",
			"vocabulary", ProvenanceArtifactTypes)
	} else {
		slog.Info("artifact_provenance: restored provenance analytics views")
	}
	return nil
}
