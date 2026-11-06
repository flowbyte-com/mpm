// migration_continuity_project_model.go — Stage 1 of Project-Scoped
// Continuity: the additive four-table storage.
//
// This migration creates the four Continuity tables and their indexes:
// projects, project_names, continuity_threads, continuity_checkpoints.
// It is the WHOLE of Stage 1 — Stage 2 adds the project APIs and the
// deletion action on top of this storage.
//
// The DDL is transcribed byte-for-byte from §4.1 of the architecture
// document (docs/dev/continuity-architecture-2026-11-04.md, PR #224, at
// commit d4ea2468a65d0a2face9b51a39e4255c595ce6ac). Its SHA-256 is
// d0b3d7dd6bb797b7449b6c71397ab8006a6a70e134e9f1323d295b022dae70f1,
// and TestContinuityProjectModelDDLIsByteIdentical pins that hash, so
// the DDL cannot drift from the approved architecture without failing
// the build.
//
// WHY A MIGRATION AND NOT BaseTables:
//
//	BaseTables is the slice attachShared propagates into the
//	cross-workspace `shared` schema (db.go:2132 and db.go:2160). A
//	project-identity table must NOT live in the cross-workspace
//	broadcast store: `projects` is the durable identity a project has
//	inside ONE workspace, and broadcasting it would give the same
//	project id to every workspace that reads the shared store. The
//	same reasoning that keeps the four tables out of the shared schema
//	keeps them out of BaseTables.
//
// ONE DDL PATH, NO SECOND DEFINITION:
//
//	The migration runs inside the fatal migration transaction on EVERY
//	open, so a fresh database and an upgraded one take the same code
//	path and the same DDL text. There is no second copy to drift, and
//	the fresh-database path cannot differ from the migrated path.
//
// THE SENTINEL IS WRITTEN LAST, ON PURPOSE:
//
//	The sentinel row is written AFTER the DDL, inside the same
//	transaction. That ordering is what makes AT6 meaningful: a failure
//	injected at the sentinel write (a BEFORE INSERT trigger that aborts
//	it) proves that the DDL — which had already succeeded — is rolled
//	back with it, leaving no partial Continuity schema.
//
//	Idempotent via the `continuity_project_model_v1` sentinel in
//	schema_migrations. The DDL statements are themselves
//	CREATE ... IF NOT EXISTS, so a re-run is a no-op even if the
//	sentinel were lost.
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// continuityProjectModelDDL is the approved Stage 1 DDL for
// Project-Scoped Continuity. See the file header for the provenance and
// the pinned SHA-256.
//
// The text is byte-for-byte §4.1 of the architecture document. Do not
// reformat it, do not split it, and do not "tidy" its comments: the
// hash test compares this literal against the approved DDL.
const continuityProjectModelDDL = `-- ── projects ────────────────────────────────────────────────────────────
-- The permanent stub. An identity that outlives every thread, every
-- checkpoint and every session. There is no project state machine: a
-- project is alive (deleted_at IS NULL) or deleted.
--
-- created_by / deleted_by carry the WRITER IDENTITY, not an owner:
-- the workspace is a single-operator trust domain and there is no
-- authorization model in this design (§2.7, §9 OD3).
CREATE TABLE IF NOT EXISTS projects (
    id          TEXT PRIMARY KEY,                    -- 'cpj-' || 16 lowercase hex
    created_at  INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at  INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    created_by  TEXT,
    deleted_at  INTEGER,                             -- NULL = alive
    deleted_by  TEXT                                -- NULL = alive
);

-- ── project_names ───────────────────────────────────────────────────────
-- The resolution domain. Every key that resolves to a project lives
-- here.
--
-- name is COLLATE NOCASE and the live-name uniqueness is a PARTIAL
-- index over live rows only. Together they make "no silent duplicate
-- project creation" a database guarantee rather than a convention,
-- and they make a name reusable after its holder is deleted while the
-- withdrawn row is retained as history.
--
-- The key is a surrogate id, not the name, precisely so that a
-- withdrawn name and its successor's live name can coexist.
CREATE TABLE IF NOT EXISTS project_names (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL COLLATE NOCASE,
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    kind         TEXT NOT NULL CHECK (kind IN ('canonical','alias')),
    created_at   INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    withdrawn_at INTEGER                            -- NULL = live
);
CREATE INDEX IF NOT EXISTS idx_project_names_project ON project_names(project_id);
-- At most one LIVE holder of a name, case-insensitively. A withdrawn
-- row is history and does not occupy the name.
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_names_live_name
    ON project_names(name COLLATE NOCASE) WHERE withdrawn_at IS NULL;
-- At most one canonical LIVE name per project.
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_names_one_canonical
    ON project_names(project_id) WHERE kind = 'canonical' AND withdrawn_at IS NULL;

-- ── continuity_threads ──────────────────────────────────────────────────
-- An independent line of work inside a project. Several may be active
-- at once; each carries its own revision for optimistic concurrency.
--
-- Every reference is ON DELETE RESTRICT: a hard delete of a project
-- with children is refused by the database, so the ledger cannot be
-- destroyed by a cascade (§2.7).
--
-- The two composite foreign keys are what make "project" a containment
-- boundary in the STORAGE:
--   * (superseded_by, project_id) -> a thread may only be superseded
--     by a thread of the SAME project;
--   * (last_checkpoint_id, id) -> the resume pointer may only name a
--     checkpoint of THIS thread, and it must exist.
CREATE TABLE IF NOT EXISTS continuity_threads (
    id                  TEXT PRIMARY KEY,            -- 'ct-' || 16 lowercase hex
    project_id          TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
    title               TEXT NOT NULL CHECK (length(CAST(title AS BLOB)) <= 200),
    focus               TEXT COLLATE NOCASE,         -- normalised slug, unique per project
    state               TEXT NOT NULL DEFAULT 'active'
                        CHECK (state IN ('active','dormant','completed','superseded')),
    revision            INTEGER NOT NULL DEFAULT 0, -- optimistic-concurrency token
    last_checkpoint_id  TEXT,                        -- the current resume brief
    superseded_by       TEXT,
    created_at          INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at          INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    completed_at        INTEGER,
    deleted_at          INTEGER,
    FOREIGN KEY (superseded_by, project_id)
        REFERENCES continuity_threads(id, project_id) ON DELETE RESTRICT,
    FOREIGN KEY (last_checkpoint_id, id)
        REFERENCES continuity_checkpoints(id, thread_id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_continuity_threads_project
    ON continuity_threads(project_id, state, updated_at DESC);
-- One live thread per focus, case-insensitively. A superseded or
-- deleted thread releases its focus, so the same focus can be
-- re-opened later.
CREATE UNIQUE INDEX IF NOT EXISTS idx_continuity_threads_focus
    ON continuity_threads(project_id, focus)
    WHERE focus IS NOT NULL AND deleted_at IS NULL AND state != 'superseded';
-- The parent key for the two composite foreign keys below.
CREATE UNIQUE INDEX IF NOT EXISTS idx_continuity_threads_id_project
    ON continuity_threads(id, project_id);

-- ── continuity_checkpoints ──────────────────────────────────────────────
-- The append-only ledger of resume briefs. A checkpoint is never
-- edited; it is superseded by a later one, and the thread's
-- last_checkpoint_id names the current one.
--
-- project_id is DENORMALISED from the thread, and the composite
-- foreign key is what keeps the denormalisation honest: a checkpoint
-- cannot name a project other than its thread's. The separate
-- projects(id) reference is the second line of defence.
--
-- The two composite foreign keys here are the other half of the
-- containment boundary:
--   * (thread_id, project_id) -> the checkpoint belongs to its thread's
--     project, and therefore to that project;
--   * (supersedes, thread_id) -> a checkpoint may only supersede
--     another checkpoint of ITS OWN thread.
CREATE TABLE IF NOT EXISTS continuity_checkpoints (
    id                   TEXT PRIMARY KEY,           -- 'ck-' || 16 lowercase hex
    thread_id            TEXT NOT NULL,
    project_id           TEXT NOT NULL,
    revision             INTEGER NOT NULL,          -- the PRE-increment thread revision observed
    headline             TEXT CHECK (length(CAST(headline AS BLOB)) <= 200),
    facts                JSON NOT NULL DEFAULT '[]'
                         CHECK (json_valid(facts) AND json_type(facts) = 'array'),
    hypotheses           JSON NOT NULL DEFAULT '[]'
                         CHECK (json_valid(hypotheses) AND json_type(hypotheses) = 'array'),
    state                JSON NOT NULL DEFAULT '[]'
                         CHECK (json_valid(state) AND json_type(state) = 'array'),
    next_actions         JSON NOT NULL DEFAULT '[]'
                         CHECK (json_valid(next_actions) AND json_type(next_actions) = 'array'),
    supersedes           TEXT,                        -- a checkpoint of THIS thread
    created_at           INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    -- The writer's identity, on the row rather than in artifact_provenance
    -- (see §4.5 for why).
    mpm_session_id       TEXT,
    framework_session_id TEXT,
    actor_kind           TEXT,
    framework_name       TEXT,
    model_name           TEXT,
    FOREIGN KEY (thread_id, project_id)
        REFERENCES continuity_threads(id, project_id) ON DELETE RESTRICT,
    FOREIGN KEY (supersedes, thread_id)
        REFERENCES continuity_checkpoints(id, thread_id) ON DELETE RESTRICT,
    FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_continuity_checkpoints_thread
    ON continuity_checkpoints(thread_id, created_at DESC);
-- The parent key for continuity_threads' (last_checkpoint_id, id) FK
-- AND for this table's own (supersedes, thread_id) FK.
CREATE UNIQUE INDEX IF NOT EXISTS idx_continuity_checkpoints_id_thread
    ON continuity_checkpoints(id, thread_id);
`

// MigrateContinuityProjectModel creates the four Project-Scoped
// Continuity tables and their indexes.
//
// It MUST be called inside the fatal migration transaction (db.go): the
// DDL and the sentinel share one transaction, so a failure at any point
// rolls the whole thing back and NewDatabaseManager returns an error
// rather than leaving a half-migrated database.
//
// Idempotent via the `continuity_project_model_v1` sentinel.
func MigrateContinuityProjectModel(tx *sql.Tx) error {
	const sentinel = "continuity_project_model_v1"

	var applied int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		sentinel,
	).Scan(&applied); err != nil {
		return fmt.Errorf("check %s sentinel: %w", sentinel, err)
	}
	if applied > 0 {
		return nil
	}

	// The DDL is executed as ONE statement string. go-sqlite3's Exec
	// walks the statement tail, so the four CREATE TABLEs and the
	// eight CREATE INDEXes run in order, inside this transaction.
	if _, err := tx.Exec(continuityProjectModelDDL); err != nil {
		return fmt.Errorf("create the Continuity tables: %w", err)
	}

	// The sentinel is written LAST. See the file header: this ordering
	// is what AT6's injected-failure probe depends on.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		sentinel, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record %s sentinel: %w", sentinel, err)
	}
	return nil
}
