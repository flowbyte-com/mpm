// schema.go - Shared SQLite schema definitions for MPM
// Version: 2026-03-28 (Consolidated Schema)
// All tables should be defined here and imported by DatabaseManager and MemoryStore

package internal

// BaseTables contains the core table creation statements.
// These are always created on database initialization.
var BaseTables = []string{
	// Sessions table - stores session metadata and transcripts
	`CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY, session_id TEXT NOT NULL, content TEXT NOT NULL,
		content_hash TEXT UNIQUE NOT NULL, created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		source_path TEXT, metadata JSON, embedding BLOB
	);`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_session_id ON sessions(session_id);`,

	// Topics table - organizes memories and sessions into topics
	`CREATE TABLE IF NOT EXISTS topics (
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, description TEXT,
		parent_topic_id TEXT, tags JSON, is_active INTEGER DEFAULT 1,
		created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)), embedding BLOB
	);`,

	// Topic memberships - many-to-many relationship between memories/sessions and topics
	`CREATE TABLE IF NOT EXISTS topic_memberships (
		memory_id TEXT, session_id TEXT, topic_id TEXT NOT NULL,
		role TEXT DEFAULT 'related', created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		PRIMARY KEY (memory_id, topic_id),
		UNIQUE (session_id, topic_id),
		CHECK (memory_id IS NOT NULL OR session_id IS NOT NULL)
	);`,

	// Memories table - core storage for memory content
	`CREATE TABLE IF NOT EXISTS memories (
		id TEXT PRIMARY KEY, collection TEXT NOT NULL, content TEXT NOT NULL,
		session_id TEXT, tags JSON, metadata JSON, embedding BLOB,
		created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		source_db TEXT, source_id TEXT, promoted_at REAL,
		FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE SET NULL
	);`,

	// System config table - stores system configuration snapshots
	`CREATE TABLE IF NOT EXISTS system_config (
		key TEXT PRIMARY KEY,
		raw_json TEXT NOT NULL,
		content_hash TEXT NOT NULL,
		updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		config_snapshot JSON
	);`,

	// Schema migrations table - sentinel rows mark which one-shot data
	// migrations have already run. Used by MigrateDeletedAtToUnixEpoch
	// (deleted_at_unified_v1) and any future data migrations that need
	// to be idempotent across restarts.
	`CREATE TABLE IF NOT EXISTS schema_migrations (
		id TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	);`,

	// Lessons table - stores learned lessons (insights, warnings, practices)
	`CREATE TABLE IF NOT EXISTS lessons (
		id TEXT PRIMARY KEY,
		type TEXT NOT NULL DEFAULT 'insight',
		content TEXT NOT NULL,
		tags JSON,
		reinforcement_count INTEGER DEFAULT 1,
		source_session_id TEXT,
		created TEXT NOT NULL,
		content_hash TEXT
	);`,

	// Retrieval metadata — Observability Layer for "Adaptive Retrieval"
	// (2026-07-26). 1:1 mapping with any cognitive node (Memory,
	// Lesson, Decision, Theory, Skill). Tracks how often each node is
	// surfaced into agent working memory. Search ranking is NOT altered
	// by this table; the data is observability only until a future
	// ranker chooses to consume it. See retrieval_ranker.go for the
	// RetrievalRanker interface and DefaultRanker (preserves today's
	// ftsScore pass-through behavior bit-for-bit).
	`CREATE TABLE IF NOT EXISTS retrieval_metadata (
		node_id TEXT PRIMARY KEY,
		node_type TEXT NOT NULL,
		reuse_count INTEGER DEFAULT 0,
		last_retrieved_at INTEGER,
		success_count INTEGER DEFAULT 0,
		created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
	);`,

	// Raw memories table - for v6 ingest workflow
	`CREATE TABLE IF NOT EXISTS raw_memories (
		id            TEXT PRIMARY KEY,
		source_id     TEXT,
		source_db     TEXT NOT NULL DEFAULT 'openclaw',
		content_hash  TEXT NOT NULL,
		text          TEXT NOT NULL,
		metadata      TEXT NOT NULL,
		ingested_at   REAL NOT NULL,
		status        TEXT NOT NULL DEFAULT 'pending',
		llm_verdict   TEXT,
		llm_notes     TEXT,
		reviewer_prompt TEXT,
		expires_at    REAL,
		import_batch  TEXT,
		updated_at    REAL NOT NULL
	);`,

	// External database cursors - tracks sync position with external dbs
	`CREATE TABLE IF NOT EXISTS external_db_cursors (
		db_label TEXT PRIMARY KEY,
		last_cursor TEXT NOT NULL,
		updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
	);`,

	// Epistemic pressure view — powers the wake_context surface. Two
	// scalar counts: raw_count (memories in the 'memories' collection
	// not yet marked as rolled-up into a lesson) and lesson_count
	// (durable lessons currently in the substrate).
	//
	// Convention: a raw memory is "compacted" once it has
	//   metadata.compacted_into = <lesson_id>
	// set on its row. Missing/null/empty metadata AND metadata without
	// that key both count as raw (the conservative default — better to
	// over-count than to assume compaction that never happened).
	//
	// Lessons are hard-deleted (no deleted_at column on lessons_base),
	// so COUNT(*) on the lessons view naturally excludes shredded
	// lessons — no filter needed.
	//
	// Used by internal.GatherWakeContext to surface the
	// epistemic_pressure block on every wake. Cheap: two indexed
	// COUNT(*) queries against the (collection, deleted_at, ...) and
	// lessons views. Sub-millisecond at realistic substrate sizes.
	`CREATE VIEW IF NOT EXISTS epistemic_pressure_v AS
	SELECT
	  (SELECT COUNT(*) FROM memories
	   WHERE collection = 'memories'
	     AND deleted_at IS NULL
	     AND (metadata IS NULL OR metadata = ''
	          OR json_extract(metadata, '$.compacted_into') IS NULL)
	  ) AS raw_count,
	  (SELECT COUNT(*) FROM lessons) AS lesson_count`,

	// Memory revisions table - historical ledger for point-in-time reconstruction
	`CREATE TABLE IF NOT EXISTS memory_revisions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		memory_id TEXT NOT NULL,
		version INTEGER NOT NULL,
		content TEXT NOT NULL,
		weight REAL NOT NULL DEFAULT 1.0,
		collection TEXT NOT NULL,
		is_long_term INTEGER NOT NULL DEFAULT 0,
		is_challenged INTEGER NOT NULL DEFAULT 0,
		challenged_theory_id TEXT,
		created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		FOREIGN KEY(memory_id) REFERENCES memories(id) ON DELETE CASCADE,
		UNIQUE(memory_id, version)
	);`,

	// Evidence table — typed, source-grouped evidence for confidence calculation.
	`CREATE TABLE IF NOT EXISTS evidence (
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
	);`,

	`CREATE INDEX IF NOT EXISTS idx_evidence_artifact ON evidence(artifact_id, artifact_type);`,
	`CREATE INDEX IF NOT EXISTS idx_evidence_type ON evidence(type);`,
	`CREATE INDEX IF NOT EXISTS idx_evidence_source ON evidence(source_group);`,
	`CREATE INDEX IF NOT EXISTS idx_evidence_creator ON evidence(created_by);`,
	`CREATE INDEX IF NOT EXISTS idx_evidence_expires ON evidence(expires_at);`,

	// Confidence history — append-only ledger of every confidence value ever computed.
	`CREATE TABLE IF NOT EXISTS confidence_history (
		id              TEXT PRIMARY KEY,
		artifact_id     TEXT NOT NULL,
		artifact_type   TEXT NOT NULL,
		confidence      REAL NOT NULL,
		computed_at     INTEGER NOT NULL,
		evidence_count  INTEGER NOT NULL,
		trigger         TEXT NOT NULL CHECK (trigger IN ('evidence_added','evidence_updated','evidence_deleted','evidence_expired','decay_tick','concept_drift','manual_recompute'))
	);`,

	`CREATE INDEX IF NOT EXISTS idx_conf_history_artifact ON confidence_history(artifact_id, artifact_type, computed_at);`,

	// artifacts view — union of memories (filtered by collection) and lessons,
	// with the `type` column distinguishing the four artifact kinds. The 0.5
	// confidence default matches the spec's neutral point; the application sets
	// the per-type initial value at insert time. Lessons carries its own `type`
	// column (the lesson type taxonomy), so we shadow it with the artifact
	// discriminator via a subquery; lessons also lacks `collection` and
	// `created_at`, so we supply literals and a CAST.
	`CREATE VIEW IF NOT EXISTS artifacts AS
		SELECT 'memory'   AS type, id, collection, retrieval_priority, importance, confidence, created_at
		FROM memories WHERE collection IN ('memories','') OR collection IS NULL
		UNION ALL
		SELECT 'theory'   AS type, id, collection, retrieval_priority, importance, confidence, created_at
		FROM memories WHERE collection = 'theories'
		UNION ALL
		SELECT 'decision' AS type, id, collection, retrieval_priority, importance, confidence, created_at
		FROM memories WHERE collection = 'decisions'
		UNION ALL
		SELECT 'lesson'   AS type, id, 'lessons' AS collection, retrieval_priority, importance, confidence, CAST(created AS DATETIME) AS created_at
		FROM lessons;`,

	// legacy_weight view — compatibility shim for v2. Maps the new fields back
	// to a single number. The floor of 0.01 keeps old commands from seeing zero.
	`CREATE VIEW IF NOT EXISTS legacy_weight AS
		SELECT id, collection, MAX(0.01, (retrieval_priority + importance) / 2.0) AS legacy_weight
		FROM memories
		UNION ALL
		SELECT id, 'lessons' AS collection, MAX(0.01, (retrieval_priority + importance) / 2.0) AS legacy_weight
		FROM lessons;`,

	// Synth ledger — persistent content_hash dedup for AutoSynthesize.
	// content_hash is the PK so re-ingesting the same content always
	// hits O(1) lookup. first_run_at is the original timestamp;
	// last_run_at is bumped on each re-sight (watchdog observability);
	// run_count is the cumulative number of times this content was
	// seen (dedup hit counter). result_memory_id points at the
	// synthesized memory produced on first_run_at — useful for
	// forensic trails when the originals have since been soft-deleted.
	`CREATE TABLE IF NOT EXISTS synth_runs (
		content_hash TEXT PRIMARY KEY,
		first_run_at INTEGER NOT NULL,
		last_run_at  INTEGER NOT NULL,
		run_count    INTEGER NOT NULL DEFAULT 1,
		result_memory_id TEXT
	);`,

	// ── Epistemic Cascades (2026-08-04) ─────────────────────────────
	//
	// Two-table substrate for the cascade feature. The outbox holds
	// one row per (invalidation_event, downstream_artifact) pair — the
	// dedup key from the design spec — and the materializer claims
	// `pending` rows, writes a theory per row, and stamps
	// `materialized_theory_id`. The provenance table is the
	// (source, downstream) citation log: a theory's `dependencies` JSON
	// provides the explicit edge, and a retrieval-time citation (the
	// `source_ids` recorded when a decision/theory was surfaced) provides
	// the implicit one. Both edges are unioned during dependency
	// discovery so a downstream artifact can be cascade-targeted even
	// without an explicit `dependencies` declaration.
	//
	// Outbox fields follow the design spec
	// (docs/superpowers/specs/2026-08-04-epistemic-cascades-design.md):
	//   id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
	//   downstream_artifact_id, downstream_artifact_type,
	//   trigger_evidence_id (nullable), cascade_depth, reason,
	//   status, materialized_theory_id (nullable), retry metadata,
	//   timestamps.
	//
	// The status CHECK enforces the four legal states
	// (pending|processing|materialized|failed) so a typo in a future
	// materializer patch is rejected at the storage boundary rather
	// than silently producing rows that no worker claims.
	//
	// The UNIQUE (dead_artifact_id, downstream_artifact_id,
	// invalidation_event_id) is the dedup key — two invalidations of
	// the same dead artifact against the same downstream collapse
	// into one intent, but two different downstream artifacts each
	// get their own row.
	//
	// Timestamps are INTEGER Unix-epoch seconds, matching the
	// codebase-wide convention set by the timestamps_unified_v1
	// migration (see migration_timestamps.go).
	`CREATE TABLE IF NOT EXISTS epistemic_cascade_outbox (
		id                        TEXT PRIMARY KEY,
		invalidation_event_id     TEXT NOT NULL,
		dead_artifact_id          TEXT NOT NULL,
		dead_artifact_type        TEXT NOT NULL,
		downstream_artifact_id    TEXT NOT NULL,
		downstream_artifact_type  TEXT NOT NULL,
		trigger_evidence_id       TEXT,
		cascade_depth             INTEGER NOT NULL DEFAULT 0,
		reason                    TEXT NOT NULL DEFAULT '',
		status                    TEXT NOT NULL DEFAULT 'pending'
		                          CHECK (status IN ('pending','processing','materialized','failed')),
		materialized_theory_id    TEXT,
		attempt_count             INTEGER NOT NULL DEFAULT 0,
		next_retry_at             INTEGER,
		terminal_error            TEXT,
		created_at                INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		updated_at                INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		UNIQUE (dead_artifact_id, downstream_artifact_id, invalidation_event_id)
	);`,
	`CREATE INDEX IF NOT EXISTS idx_epistemic_cascade_outbox_event
		ON epistemic_cascade_outbox(invalidation_event_id);`,
	`CREATE INDEX IF NOT EXISTS idx_epistemic_cascade_outbox_dead
		ON epistemic_cascade_outbox(dead_artifact_id);`,
	`CREATE INDEX IF NOT EXISTS idx_epistemic_cascade_outbox_status_retry
		ON epistemic_cascade_outbox(status, next_retry_at);`,

	// epistemic_provenance: typed citation log. One row per
	// (source, downstream, event) — recorded when a decision or theory
	// cites another artifact in context. Together with the explicit
	// `memories.dependencies` JSON, this gives the cascade dependency
	// discovery both kinds of edges the design spec calls out.
	//
	// event_id is the invalidation/recall event that produced the
	// citation; for retrieval-time citations it is the wake id, for
	// explicit `record_decision` citations it is the decision's own
	// id. The pair (source_id, downstream_id, event_id) is unique
	// so re-recording the same citation is a no-op.
	`CREATE TABLE IF NOT EXISTS epistemic_provenance (
		id              TEXT PRIMARY KEY,
		source_id       TEXT NOT NULL,
		source_type     TEXT NOT NULL,
		downstream_id   TEXT NOT NULL,
		downstream_type TEXT NOT NULL,
		event_id        TEXT NOT NULL,
		created_at      INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		UNIQUE (source_id, downstream_id, event_id)
	);`,
	`CREATE INDEX IF NOT EXISTS idx_epistemic_provenance_source
		ON epistemic_provenance(source_id);`,
	`CREATE INDEX IF NOT EXISTS idx_epistemic_provenance_downstream
		ON epistemic_provenance(downstream_id);`,

	// ── Artifact Provenance (2026-08-08) ─────────────────────────────
	//
	// First-class creation telemetry for memories, theories, lessons,
	// decisions. The schema records the declared execution context at
	// artifact creation time. Designed to answer:
	//
	//   "Which model produced which memory, and what happened to it?"
	//
	// One UNIQUE (artifact_id, artifact_type) constraint enforces the
	// "one provenance row per artifact" invariant at the storage
	// boundary. A second hook that records provenance for the same
	// artifact fails with a UNIQUE violation, not a silent duplicate.
	//
	// Spec: docs/superpowers/specs/2026-08-08-artifact-provenance-design.md
	//
	// schema_version is semantic (not additive). Additive nullable
	// fields do not require a version bump; a version bump is reserved
	// for semantic changes to existing fields. Default is 'v1'.
	//
	// thinking_visible is INTEGER (0/1) for SQLite portability — the
	// BOOLEAN alias is not preserved through sql.Dump.
	//
	// The two CHECK constraints enforce the artifact_type and
	// actor_kind vocabularies at the storage boundary; a typo in a
	// caller is rejected by the database, not silently propagated.
	`CREATE TABLE IF NOT EXISTS artifact_provenance (
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
		-- SQL comment inside the DDL: traces agent-of-agent invocation
		-- trees. Distinct from parent_artifact_id (which traces artifact
		-- causality); this column traces invocation causality. See the
		-- Go comment above the block for the full rationale.
		parent_invocation_id TEXT,
		provider_metadata    TEXT,
		UNIQUE (artifact_id, artifact_type),
		-- SQL comment inside the DDL: artifact_type covers the full
		-- substrate surface for telemetry. The widening from the
		-- original ('memory','theory','lesson','decision') is enforced
		-- for existing alpha DBs by migrateArtifactProvenanceSchema.
		CHECK (artifact_type IN ('memory','theory','lesson','decision','handoff','directive','work')),
		CHECK (actor_kind IN ('agent','human','import','system','unknown'))
	);`,
	// Go comment block: Reserved for the artifact_relations table.
	// Cross-artifact edges (derived_from, supersedes, contradicts,
	// supports, duplicates, promotes, challenges, resolves) will
	// migrate here once it ships. Current scattered fields
	// (metadata.derived_from_local_id, metadata.derived_from_skill_id,
	// metadata.challenged_theory_id, etc.) are placeholders. NOT
	// building before alpha — see decision
	// artifact_relations_reserved_2026-08-19.
	`CREATE INDEX IF NOT EXISTS idx_provenance_artifact
		ON artifact_provenance(artifact_id, artifact_type);`,
	`CREATE INDEX IF NOT EXISTS idx_provenance_model
		ON artifact_provenance(provider_name, model_name);`,
	`CREATE INDEX IF NOT EXISTS idx_provenance_actor
		ON artifact_provenance(actor_kind, framework_name);`,
	`CREATE INDEX IF NOT EXISTS idx_provenance_session
		ON artifact_provenance(session_id);`,
	`CREATE INDEX IF NOT EXISTS idx_provenance_invocation
		ON artifact_provenance(invocation_id);`,
	// idx_provenance_parent_invocation is created in CommonIndexes
	// (after SafeMigrations has added the column for legacy alpha DBs)
	// rather than here, so the CREATE INDEX doesn't run before the
	// column exists.

	// ── Analytics Views (2026-08-08) ─────────────────────────────
	//
	// Both views are descriptive lifecycle measures, NOT quality scores.
	// "Survived_30d" is mechanically defined as:
	//   (artifact.deleted_at IS NULL AND artifact.weight >= 1
	//    AND now - artifact.created_at >= 30 days)
	//
	// "Challenged" is defined as: metadata->>'$.status' = 'challenged'
	// (the challenge system stores status in metadata, not a column).
	//
	// Spec: docs/superpowers/specs/2026-08-08-artifact-provenance-design.md
	// (see "Analytics views" section). The CLI surface consumes
	// v_model_memory_yield as `mpm provenance model-yield`. The
	// v_model_theory_utility view is reachable via `mpm exec-sql`.
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
		GROUP BY p.provider_name, p.model_name, p.framework_name, p.framework_adapter;`,
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
		GROUP BY p.provider_name, p.model_name, p.thinking_level;`,

	// ── Capability Lifecycle (2026-08-05) ─────────────────────────────
	//
	// Four-table substrate for the capability lifecycle subsystem.
	// Spec: docs/architecture/capability-lifecycle.md
	//
	//   capabilities:           stateful artifact ledger. State machine
	//                           CHECK enforced at storage boundary;
	//                           "live version" is `state='active' AND
	//                           superseded_by_id IS NULL` (single query,
	//                           no JOIN).
	//
	//   capability_invocations: high-write execution telemetry. Strict
	//                           separation from capability_events so the
	//                           metrics aggregation pipeline never has
	//                           to filter synthetic state-change events
	//                           out of real execution data.
	//
	//   capability_dependencies: junction table for dependency graph.
	//                           Reverse lookups power the recursive CTE
	//                           cascade walkers (fracture + rollback).
	//                           FKs are self-referencing with ON DELETE
	//                           CASCADE so a retired capability's
	//                           dependency rows vacuum with it.
	//
	//   capability_events:      state transitions and synthetic events
	//                           (rollback, dependency_shatter, promotion,
	//                           source_hash_mismatch). What `mpm skill
	//                           audit <id>` reads for the lineage diff.
	//
	// Foreign key targets:
	//   - capabilities.author_theory_id → memories(id) (the application
	//     layer enforces `collection='theories'`; SQLite FK can't
	//     enforce collection since it's just a column value).
	//   - capabilities.created_from_id  → capabilities(id) (self-ref,
	//     supports the revision/fork lineage walker in §5.2).
	//   - capabilities.superseded_by_id → capabilities(id) (self-ref,
	//     shadow flag for the live-version query).
	`CREATE TABLE IF NOT EXISTS capabilities (
		id              TEXT PRIMARY KEY,
		name            TEXT NOT NULL UNIQUE,
		purpose         TEXT NOT NULL,
		source_code     TEXT NOT NULL,
		source_language TEXT NOT NULL DEFAULT 'bash',
		source_hash     TEXT NOT NULL,

		state            TEXT NOT NULL DEFAULT 'draft'
		                 CHECK (state IN (
		                     'draft','linted','validated','probation','active',
		                     'degraded','needs_revision','fractured','rolled_back','retired'
		                 )),
		execution_domain TEXT NOT NULL DEFAULT 'sandbox'
		                 CHECK (execution_domain IN (
		                     'sandbox','restricted','trusted','operator'
		                 )),
		state_changed_at INTEGER NOT NULL,

		author_theory_id TEXT,
		author_agent     TEXT,
		created_from_id  TEXT,
		superseded_by_id TEXT,

		success_count       INTEGER NOT NULL DEFAULT 0,
		failure_count       INTEGER NOT NULL DEFAULT 0,
		fracture_count      INTEGER NOT NULL DEFAULT 0,
		last_invoked_at     INTEGER,
		last_failure_at     INTEGER,
		last_failure_stderr TEXT,
		avg_latency_ms      REAL    NOT NULL DEFAULT 0,

		probation_required_success_count INTEGER NOT NULL DEFAULT 5,
		probation_max_failure_rate       REAL    NOT NULL DEFAULT 0.10,
		promoted_at                      INTEGER,

		embedding BLOB,
		tags     TEXT NOT NULL DEFAULT '[]',

		created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		deleted_at INTEGER,
		metadata   TEXT NOT NULL DEFAULT '{}',

		FOREIGN KEY (author_theory_id)  REFERENCES memories(id)     ON DELETE SET NULL,
		FOREIGN KEY (created_from_id)   REFERENCES capabilities(id) ON DELETE SET NULL,
		FOREIGN KEY (superseded_by_id)  REFERENCES capabilities(id) ON DELETE SET NULL
	);`,
	`CREATE INDEX IF NOT EXISTS idx_capabilities_state        ON capabilities(state);`,
	`CREATE INDEX IF NOT EXISTS idx_capabilities_domain       ON capabilities(execution_domain);`,
	`CREATE INDEX IF NOT EXISTS idx_capabilities_author       ON capabilities(author_theory_id);`,
	`CREATE INDEX IF NOT EXISTS idx_capabilities_superseded   ON capabilities(superseded_by_id);`,
	`CREATE INDEX IF NOT EXISTS idx_capabilities_last_invoked ON capabilities(last_invoked_at DESC);`,
	`CREATE INDEX IF NOT EXISTS idx_capabilities_observation  ON capabilities(state, promoted_at);`,

	// capability_invocations: pure execution telemetry. Synthetic
	// state-change events go to capability_events — never here.
	`CREATE TABLE IF NOT EXISTS capability_invocations (
		id                  TEXT PRIMARY KEY,
		capability_id       TEXT NOT NULL,
		invoked_at          INTEGER NOT NULL,
		exit_code           INTEGER NOT NULL,
		duration_ms         INTEGER NOT NULL,
		stderr              TEXT,
		invocation_context  TEXT,
		cascade_invalidated INTEGER NOT NULL DEFAULT 0,

		FOREIGN KEY (capability_id) REFERENCES capabilities(id) ON DELETE CASCADE
	);`,
	`CREATE INDEX IF NOT EXISTS idx_invocations_capability_time ON capability_invocations(capability_id, invoked_at DESC);`,
	`CREATE INDEX IF NOT EXISTS idx_invocations_recent_failures ON capability_invocations(capability_id, exit_code);`,
	`CREATE INDEX IF NOT EXISTS idx_invocations_gc              ON capability_invocations(invoked_at);`,

	// capability_dependencies: reverse-lookup junction for the
	// cascade walkers. ON DELETE CASCADE on both FKs so a retired
	// capability's dependency rows vacuum with it.
	`CREATE TABLE IF NOT EXISTS capability_dependencies (
		capability_id TEXT NOT NULL,
		depends_on_id TEXT NOT NULL,
		added_at      INTEGER NOT NULL,
		PRIMARY KEY (capability_id, depends_on_id),
		FOREIGN KEY (capability_id) REFERENCES capabilities(id) ON DELETE CASCADE,
		FOREIGN KEY (depends_on_id)  REFERENCES capabilities(id) ON DELETE CASCADE
	);`,
	`CREATE INDEX IF NOT EXISTS idx_deps_depends_on ON capability_dependencies(depends_on_id);`,

	// capability_events: state transitions and synthetic events.
	// Strict separation from capability_invocations.
	`CREATE TABLE IF NOT EXISTS capability_events (
		id            TEXT PRIMARY KEY,
		capability_id TEXT NOT NULL,
		event_type    TEXT NOT NULL,
		occurred_at   INTEGER NOT NULL,
		actor         TEXT NOT NULL,
		from_state    TEXT,
		to_state      TEXT,
		reason        TEXT,
		related_id    TEXT,
		metadata      TEXT NOT NULL DEFAULT '{}',

		FOREIGN KEY (capability_id) REFERENCES capabilities(id) ON DELETE CASCADE
	);`,
	`CREATE INDEX IF NOT EXISTS idx_events_capability_time ON capability_events(capability_id, occurred_at DESC);`,
	`CREATE INDEX IF NOT EXISTS idx_events_type           ON capability_events(event_type, occurred_at DESC);`,
	`CREATE INDEX IF NOT EXISTS idx_events_actor          ON capability_events(actor);`,
}

// ReferenceTables contains the reference-library table creation statements.
// Single source of truth for the reference schema — run by
// DatabaseManager.initUnifiedSchema() in production and by InitSchema in
// tests. There is no longer a separate ReferenceDB type with its own
// connection: every reference write goes through *DatabaseManager, which
// holds the unified *sql.DB shared with the rest of MPM.
//
// content uses TEXT NOT NULL DEFAULT '' so callers that do not store the
// full source text (the common case for streaming ingest) don't have to
// supply it explicitly. Older AddReference paths that wrote nothing into
// content would otherwise fail on the NOT NULL column.
var ReferenceTables = []string{
	// Reference documents table - single row per imported document.
	// file_path vs source_path: schema uses file_path; ReferenceDoc
	// struct uses SourcePath. AddReference writes SourcePath into file_path.
	`CREATE TABLE IF NOT EXISTS reference_docs (
		id TEXT PRIMARY KEY, title TEXT NOT NULL, file_path TEXT,
		source_path TEXT, source_type TEXT, tags TEXT,
		content TEXT NOT NULL DEFAULT '', content_hash TEXT,
		import_reason TEXT,
		total_chunks INTEGER DEFAULT 0, last_indexed INTEGER,
		created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
	);`,

	// Reference chunks table - stores chunked content of reference documents.
	// ON DELETE CASCADE ensures chunks do not orphan when their parent doc
	// is removed (DeleteReference relies on this).
	// content_hash stores sha256(content) so AddReference can diff against
	// existing rows without re-reading chunk content — chunk_hash diff
	// ingest reuses unchanged chunks and deletes orphans atomically.
	// embedding stores a JSON-marshalled []float32 (matches the
	// memories.embedding column convention) for semantic search. NULL
	// means "not yet embedded" — EmbedReferenceChunks fills it in a
	// separate phase after AddReference. The embedding column is cleared
	// by AddReference's ON CONFLICT(id) DO UPDATE branch whenever a
	// chunk's content changes, so re-embedding is forced.
	`CREATE TABLE IF NOT EXISTS reference_chunks (
		id TEXT PRIMARY KEY, doc_id TEXT NOT NULL, chunk_index INTEGER NOT NULL,
		section TEXT, content TEXT NOT NULL, source_path TEXT,
		content_hash TEXT, embedding BLOB,
		FOREIGN KEY (doc_id) REFERENCES reference_docs(id) ON DELETE CASCADE
	);`,

	// Reference interactions table - audit trail of retrieval events.
	// Written whenever SearchReferenceChunks surfaces a chunk (or doc) for a query.
	// This is the substrate the admission function reads to know which references
	// have been used, in what context, and how often. Per decision 4f1c1fbc41a7a765
	// and b90e4fe54507c3b9: interaction is the third primitive; without it, the
	// reference-to-memory path is not observable.
	`CREATE TABLE IF NOT EXISTS reference_interactions (
		id TEXT PRIMARY KEY,
		doc_id TEXT NOT NULL,
		chunk_id TEXT,
		query TEXT NOT NULL,
		search_kind TEXT,
		rank INTEGER,
		score REAL,
		created_at INTEGER NOT NULL,
		FOREIGN KEY (doc_id) REFERENCES reference_docs(id) ON DELETE CASCADE
	);`,

	// Admission log - audit trail for the admission function (Phase 3).
	// One row per evaluation. Different semantics from reference_interactions
	// (which is retrieval audit); this is admission audit. The two are kept
	// separate so each can be queried and analyzed on its own.
	`CREATE TABLE IF NOT EXISTS admission_log (
		id TEXT PRIMARY KEY,
		doc_id TEXT NOT NULL,
		chunk_id TEXT,
		admit INTEGER NOT NULL,
		content TEXT,
		confidence REAL,
		reason TEXT,
		justification TEXT,
		admission_model TEXT,
		created_at INTEGER NOT NULL,
		FOREIGN KEY (doc_id) REFERENCES reference_docs(id) ON DELETE CASCADE
	);`,
}

// WorkTables contains the work-items table and indexes.
var WorkTables = []string{
	`CREATE TABLE IF NOT EXISTS works (
		id            TEXT PRIMARY KEY,
		title        TEXT NOT NULL,
		content      TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL DEFAULT 'open'
		             CHECK (status IN ('open', 'done', 'cancelled')),
		verification TEXT NOT NULL DEFAULT 'unverified'
		             CHECK (verification IN ('unverified','verified','partial','contradicted')),
		created_at   INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		updated_at   INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		completed_at INTEGER,
		session_id   TEXT
	);`,
	`CREATE INDEX IF NOT EXISTS idx_works_status ON works(status);`,
	`CREATE INDEX IF NOT EXISTS idx_works_session ON works(session_id);`,
	`CREATE INDEX IF NOT EXISTS idx_works_created ON works(created_at DESC);`,
	// Serves the wake-context open-works query (status='open' ORDER BY
	// updated_at DESC LIMIT 5) without a sort step.
	`CREATE INDEX IF NOT EXISTS idx_works_status_updated ON works(status, updated_at DESC);`,

	// work_events: append-only ledger of Work state transitions (event-sourced).
	// UNIQUE(work_id, event_index) enforces monotonic event_index per work item.
	// CHECK constraint enumerates the exact event type vocabulary.
	// Phase 2 adds evidence fields for provenance/verification design.
	`CREATE TABLE IF NOT EXISTS work_events (
		id                    TEXT PRIMARY KEY,
		work_id               TEXT NOT NULL,
		event_index           INTEGER NOT NULL,
		event_type            TEXT NOT NULL
		                      CHECK (event_type IN (
		                        'created','note_appended','completed',
		                        'cancelled','reopened',
		                        'title_updated','content_updated',
		                        'claimed_complete','evidence_observed'
		                      )),
		created_at            INTEGER NOT NULL
		                      DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		invocation_id         TEXT,
		parent_invocation_id TEXT,
		note                  TEXT,
		title                 TEXT,
		content               TEXT,
		directive_ids         TEXT DEFAULT '[]',
		UNIQUE(work_id, event_index)
	);`,
	`CREATE INDEX IF NOT EXISTS idx_work_events_work_id ON work_events(work_id);`,
	`CREATE INDEX IF NOT EXISTS idx_work_events_invocation ON work_events(invocation_id);`,
}

// ReferenceIndexes contains the indexes that support the reference tables.
// Separate from CommonIndexes so isolated reference DBs (test fixtures)
// get the matching indexes too — without them, queries against the audit
// tables degenerate to full scans.
var ReferenceIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_reference_chunks_doc_id ON reference_chunks(doc_id);`,
	`CREATE INDEX IF NOT EXISTS idx_reference_chunks_section ON reference_chunks(section);`,
	`CREATE INDEX IF NOT EXISTS idx_interactions_doc ON reference_interactions(doc_id);`,
	`CREATE INDEX IF NOT EXISTS idx_interactions_chunk ON reference_interactions(chunk_id);`,
	`CREATE INDEX IF NOT EXISTS idx_interactions_query ON reference_interactions(query);`,
	`CREATE INDEX IF NOT EXISTS idx_admission_log_doc ON admission_log(doc_id);`,
	`CREATE INDEX IF NOT EXISTS idx_admission_log_admit ON admission_log(admit);`,
	`CREATE INDEX IF NOT EXISTS idx_admission_log_created ON admission_log(created_at);`,
}

// CommonIndexes contains indexes for fast lookups.
var CommonIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_topic_memberships_topic ON topic_memberships(topic_id);`,
	`CREATE INDEX IF NOT EXISTS idx_topic_memberships_memory ON topic_memberships(memory_id);`,
	`CREATE INDEX IF NOT EXISTS idx_topic_memberships_session ON topic_memberships(session_id);`,
	`CREATE INDEX IF NOT EXISTS idx_topics_parent ON topics(parent_topic_id);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_session ON memories(session_id);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_collection ON memories(collection);`,
	// Serves the F19 duplicate-save lookup (same collection + content hash).
	// Lives in CommonIndexes because content_hash is added by SafeMigrations,
	// which run before this block.
	`CREATE INDEX IF NOT EXISTS idx_memories_coll_hash ON memories(collection, content_hash);`,
	// F19 hard constraint: one live row per identity per collection. The
	// partial predicate excludes legacy NULL/empty identity rows so index
	// creation cannot fail on databases with historical duplicates.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_memories_identity_live ON memories(collection, identity_hash) WHERE deleted_at IS NULL AND identity_hash IS NOT NULL AND identity_hash != '';`,
	`CREATE INDEX IF NOT EXISTS idx_memories_deleted ON memories(deleted_at);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_collection_deleted_created ON memories(collection, deleted_at, created_at);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_longterm ON memories(is_long_term, weight);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_reinforcement ON memories(reinforcement_count);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_expires ON memories(expires_at);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_accessed ON memories(last_accessed_at);`,
	`CREATE INDEX IF NOT EXISTS idx_lessons_type ON lessons(type);`,
	`CREATE INDEX IF NOT EXISTS idx_lessons_reinforcement ON lessons(reinforcement_count);`,
	`CREATE INDEX IF NOT EXISTS idx_raw_memories_status ON raw_memories(status);`,
	`CREATE INDEX IF NOT EXISTS idx_raw_memories_expires ON raw_memories(expires_at);`,
	`CREATE INDEX IF NOT EXISTS idx_raw_memories_import_batch ON raw_memories(import_batch);`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_raw_memories_source_dedup ON raw_memories(source_db, source_id);`,
	`CREATE INDEX IF NOT EXISTS idx_revisions_timeline ON memory_revisions(memory_id, created_at);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_retrieval_priority ON memories(retrieval_priority);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_importance ON memories(importance);`,
	`CREATE INDEX IF NOT EXISTS idx_lessons_retrieval_priority ON lessons(retrieval_priority);`,
	`CREATE INDEX IF NOT EXISTS idx_lessons_importance ON lessons(importance);`,

	// Composite indexes for heavy write-path queries (DecayWeights, ConsolidateMemories, AutoPrunePolicy, SpacedReinforcementReview)
	`CREATE INDEX IF NOT EXISTS idx_memories_decay ON memories(collection, deleted_at, is_long_term, weight);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_reinforcement ON memories(collection, deleted_at, reinforcement_count, weight);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_prune ON memories(collection, deleted_at, updated_at);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_spaced_review ON memories(collection, deleted_at, is_long_term, weight, last_accessed_at);`,
	`CREATE INDEX IF NOT EXISTS idx_memories_last_synth ON memories(collection, deleted_at, last_synthesized_at);`,
	`CREATE INDEX IF NOT EXISTS idx_synth_runs_last_run ON synth_runs(last_run_at);`,

	// System audit log — runtime anomalies (errors, warnings, fatal conditions)
	// AND deliberate state-mutation events (info). The agent queries this via
	// the query_audit_log MCP tool to surface what went wrong, especially
	// across sessions. A 30-day TTL is enforced by the gc sweep (see
	// internal/gc.go and the runOpsMaintain hook) — not by SQLite triggers,
	// because audit retention is a policy, not an invariant.
	//
	// Two audiences share the table:
	//   * warn/error/fatal — anomalies. Cluster-detected by LogAudit's
	//     upsertClusterCounter side-effect (gated on level != info).
	//   * info — deliberate state mutations with downstream blast radius
	//     (irrecoverable deletes, cross-agent writes, theory state
	//     transitions). NOT cluster-detected. Forensic trail only.
	//
	// For existing databases, the CHECK is relaxed via the one-shot
	// migration in audit.go::migrateAuditLevelConstraint (called from
	// initUnifiedSchema). That migration requires the column list here
	// to stay in sync — see the IMPORTANT note on that function.
	`CREATE TABLE IF NOT EXISTS system_audit_log (
		id          TEXT PRIMARY KEY,
		level       TEXT NOT NULL CHECK (level IN ('info','warn','error','fatal','critical')),
		component   TEXT NOT NULL,
		message     TEXT NOT NULL,
		stack_trace TEXT,
		context     JSON,
		created_at  INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
	);`,
	`CREATE INDEX IF NOT EXISTS idx_audit_level_created ON system_audit_log(level, created_at);`,
	`CREATE INDEX IF NOT EXISTS idx_audit_component ON system_audit_log(component);`,
	`CREATE INDEX IF NOT EXISTS idx_audit_created ON system_audit_log(created_at);`,

	// audit_cluster_proposals: detector rows for error clusters. A cluster
	// is (component, message_hash). When the same error fires
	// ClusterThreshold times within ClusterWindowDays, a row lands here.
	// Lifecycle: status='active' (agent should investigate), 'snoozed'
	// (deferred until snooze_until), 'resolved' (handled or stopped firing).
	// Read-side: wake_context.AuditSummary surfaces active + expired-snooze
	// rows, filtered against existing pending theories + recent decisions
	// so the agent sees "known" vs "unknown" clusters.
	`CREATE TABLE IF NOT EXISTS audit_cluster_proposals (
		cluster_key   TEXT PRIMARY KEY,
		component     TEXT NOT NULL,
		message_hash  TEXT NOT NULL,
		count         INTEGER NOT NULL DEFAULT 1,
		first_seen    INTEGER NOT NULL,
		last_seen     INTEGER NOT NULL,
		status        TEXT NOT NULL DEFAULT 'active'
		              CHECK (status IN ('active','snoozed','resolved')),
		snooze_until  INTEGER,
		created_at    INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		updated_at    INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
	);`,
	`CREATE INDEX IF NOT EXISTS idx_acp_component ON audit_cluster_proposals(component);`,
	`CREATE INDEX IF NOT EXISTS idx_acp_status_snooze ON audit_cluster_proposals(status, snooze_until);`,
	`CREATE INDEX IF NOT EXISTS idx_acp_last_seen ON audit_cluster_proposals(last_seen);`,

	// Session handoffs: structured end-of-session record that the next session
	// pulls from wake context. One row per session, marked as read when
	// surfaced in wake. Distinct from the dormant `sessions` table (which
	// holds content snapshots) — handoffs are bootstrap data, not logs.
	// 90-day TTL enforced by gc sweep.
	`CREATE TABLE IF NOT EXISTS session_handoffs (
		id            TEXT PRIMARY KEY,
		session_id    TEXT NOT NULL UNIQUE,
		ended_at      INTEGER NOT NULL,
		ended_state   TEXT NOT NULL CHECK (ended_state IN ('clean','crashed','interrupted','force_end')),
		summary       TEXT NOT NULL,
		commitments   JSON NOT NULL DEFAULT '[]',
		open_questions JSON NOT NULL DEFAULT '[]',
		read_at       INTEGER,
		read_by       TEXT,
		created_at    INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
	);`,
	`CREATE INDEX IF NOT EXISTS idx_handoffs_unread ON session_handoffs(read_at, ended_at DESC);`,
	`CREATE INDEX IF NOT EXISTS idx_handoffs_ended ON session_handoffs(ended_at);`,
	`CREATE INDEX IF NOT EXISTS idx_handoffs_session ON session_handoffs(session_id);`,

	// Scheduled wakes: agent-intended delayed callbacks. The "opportunistic"
	// scheduler works because ANY mpm-mcp call (from any session/agent)
	// invokes CheckPendingWakes before returning, surfacing due wakes in
	// the response payload. No long-lived process, no cron, no ticker.
	//
	//   - target_time:  unix epoch seconds; wake is due when <= now().
	//   - fired:        0 = not yet surfaced; 1 = surfaced at fired_at.
	//   - recurring_rule: agent's own cron-like spec (e.g. "+24h", "next monday").
	//                    DEPRECATED 2026-07-23 — the column remains in the
	//                    schema for backward compatibility with existing
	//                    code paths (Wake.RecurringRule field, schedule_wake
	//                    tool payload), but is NEVER honored by the daemon.
	//                    Recurring workflows now live in the dedicated
	//                    `scheduled_tasks` table (CRUD via the upcoming
	//                    `manage_scheduled_task` MCP tool) — the daemon's
	//                    60s tick loop polls that table and injects a
	//                    standard `scheduled_wakes` row at each fire.
	//                    A future schema-version bump can drop this column.
	//   - theory_id:    optional pointer to a pending theory to evaluate.
	//
	// The composite index idx_scheduled_wakes_due supports the hot path:
	//   SELECT ... WHERE fired = 0 AND target_time <= ?1
	// which must run on every single MPM call.
	//
	// GC: 30-day TTL on fired rows via ops maintain (matches handoffs/audit
	// retention). Unfired rows with target_time < now - 90d are stale;
	// surfaced as `overdue` in list_wakes for inspection.
	`CREATE TABLE IF NOT EXISTS scheduled_wakes (
		id              TEXT PRIMARY KEY,
		target_time     INTEGER NOT NULL,
		reason          TEXT NOT NULL,
		theory_id       TEXT,
		recurring_rule  TEXT,
		fired           INTEGER NOT NULL DEFAULT 0,
		fired_at        INTEGER,
		created_by      TEXT NOT NULL,
		created_at      INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		metadata        JSON
	);`,
	`CREATE INDEX IF NOT EXISTS idx_scheduled_wakes_due ON scheduled_wakes(fired, target_time);`,
	`CREATE INDEX IF NOT EXISTS idx_scheduled_wakes_theory ON scheduled_wakes(theory_id);`,
	`CREATE INDEX IF NOT EXISTS idx_scheduled_wakes_created ON scheduled_wakes(created_at);`,

	// ── Scheduled Tasks (Agentic Cron) ──────────────────────────────
	//
	// A first-class registry of recurring agentic workflows. The
	// mpm-scheduler daemon's 60s tick loop polls
	//   WHERE status='active' AND next_run_at <= ?
	// and for each due task: (a) inserts a `scheduled_wakes` row with
	// reason='cron:<task_id>' and metadata.source='cron' / directive_id,
	// then (b) rolls over next_run_at to the next cron occurrence. The
	// three operations (read, inject, rollover) wrap in a single SQLite
	// transaction so a daemon crash between wake-injection and rollover
	// can't double-fire.
	//
	//   - id:           semantic slug the agent picks (e.g.
	//                   "epistemic-compaction"). Re-using an ID updates
	//                   the existing row (ON CONFLICT).
	//   - cron_expr:    standard 5-field cron, parsed by robfig/cron/v3.
	//                   next_run_at is pre-computed at upsert time, so
	//                   the daemon never parses cron strings on the hot
	//                   path.
	//   - directive_id: the directive the agent reads when it wakes.
	//                   Resolved via read_directives at wake-time.
	//   - status:       active | paused. Pausing halts injection
	//                   without losing the schedule (rows are kept for
	//                   forensics; the row is never hard-deleted).
	//   - last_run_at:  the last injection timestamp (not wake-processing
	//                   time — that's the agent's own concern).
	//   - next_run_at:  the pre-computed next cron occurrence (UTC).
	`CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id           TEXT PRIMARY KEY,
		name         TEXT NOT NULL,
		cron_expr    TEXT NOT NULL,
		directive_id TEXT NOT NULL,
		status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused')),
		last_run_at  INTEGER,
		next_run_at  INTEGER NOT NULL,
		created_at   INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		updated_at   INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
	);`,
	`CREATE INDEX IF NOT EXISTS idx_scheduled_tasks_poll ON scheduled_tasks(status, next_run_at);`,

	// Ephemeral Scratchpad — single-row-per-session volatile thesis
	// storage. Used by agents (808 in particular) to checkpoint
	// working thoughts that aren't ready for permanent memory. When a
	// session ends without promotion, the row becomes an "orphan"
	// surfaced on next session's wake context with age tagging
	// ([Fresh]/[Dormant]/[Expired]). The agent decides whether to
	// promote_scratchpad, amend, or discard.
	//
	// `decay_at` is TTL metadata only — set on flush and reset on every
	// UPSERT (now + 24h). Future `mpm ops gc --scratchpads` reads it to
	// vacuum. Wake-context surface query IGNORES it: orphan-surfacing
	// is the whole point, and filtering by decay would hide exactly
	// the rows that need attention (trashed sessions whose TTL has
	// expired but whose thesis never made it to memory).
	//
	// `updated_at` is the per-flush evolution timestamp — multiple
	// flushes to the same session_id let us measure how actively a
	// thesis is being refined. PRIMARY KEY is session_id (single-row-
	// per-session invariant).
	`CREATE TABLE IF NOT EXISTS ephemeral_scratchpad (
		session_id TEXT PRIMARY KEY,
		thesis TEXT NOT NULL,
		supporting JSON,
		created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		decay_at INTEGER
	);`,

	// ── IVF (Inverted File) Vector Index ──
	//
	// Replaces the O(n) brute-force cosine scan in VectorMatch with an
	// approximate nearest-neighbor lookup. The win: for N vectors
	// partitioned into K clusters, candidate generation costs K cosine
	// comparisons against the centroids (in-memory) + N/K * probe_p
	// comparisons against the candidate rows (in SQL). At N=100k, K=1k,
	// probe_p=4: ~400 candidate rows instead of 100k. That's a 250x
	// reduction in the cosine work, with negligible recall loss at the
	// default probe_p=4 (95-99% vs brute force).
	//
	// Why in-SQLite (not a parallel HNSW file): the unit of backup
	// should be a single file. A parallel-file index introduces a
	// state-sync tax (SQLite commit + HNSW update must both land, or
	// drift; startup must rebuild from SQLite). Putting the index in
	// regular SQLite tables keeps everything in one transaction space
	// and one backup artifact.
	//
	// Why no triggers (despite the precedent of FTS5 sync triggers):
	// SQLite triggers that call back into Go via RegisterFunc cause
	// CGO deadlocks (documented at db.go around the evidence-ghost-
	// triggers note). Pure-SQL triggers computing cosine against
	// centroids would require json_each per centroid per INSERT — slow
	// and ugly. Instead, every write path that touches the embedding
	// column calls assignToCluster(dm, memoryID, vec) explicitly. The
	// vector_assignments row is written in the same transaction as the
	// memory row.
	//
	// Schema:
	//   vector_clusters:    one row per cluster. centroid is a packed
	//                       float32[dim] little-endian blob. n_vectors
	//                       and variance are diagnostic telemetry for
	//                       the rebalance command's cluster-quality
	//                       report.
	//   vector_assignments: one row per memory. cluster_id foreign-keys
	//                       to vector_clusters.cluster_id. The compound
	//                       index supports the IVF candidate-generation
	//                       SELECT (WHERE cluster_id IN (?, ?, ...))
	//                       and the single-row lookup by memory_id for
	//                       re-assignment after an embedding update.
	`CREATE TABLE IF NOT EXISTS vector_clusters (
		cluster_id INTEGER PRIMARY KEY,
		centroid   BLOB    NOT NULL,
		n_vectors  INTEGER NOT NULL DEFAULT 0,
		variance   REAL    NOT NULL DEFAULT 0.0,
		updated_at INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER))
	);`,
	`CREATE TABLE IF NOT EXISTS vector_assignments (
		memory_id  TEXT PRIMARY KEY,
		cluster_id INTEGER NOT NULL,
		updated_at INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
		FOREIGN KEY (cluster_id) REFERENCES vector_clusters(cluster_id)
	);`,
	`CREATE INDEX IF NOT EXISTS idx_vector_assignments_cluster
		ON vector_assignments(cluster_id);`,
	`CREATE INDEX IF NOT EXISTS idx_vector_assignments_updated
		ON vector_assignments(updated_at);`,

	// ── Behavioral Drill Audit (2026-08-12) ──────────────────────────────
	//
	// tool_invocations backs the mpm drills orchestrator. Every CLI and MCP
	// tool dispatch writes a row here (audit hooks in cmd/mpm/call.go and
	// cmd/mpm-mcp/) so that drill_handler.go can derive a per-session
	// tool-call sequence and score compliance against drill_runs.expect.
	//
	// The composite (session_id, started_at) index is the only hot path —
	// drill scorers read "give me all calls in session X in time order" —
	// so it lives in BaseTables rather than CommonIndexes.
	`CREATE TABLE IF NOT EXISTS tool_invocations (
		id              TEXT PRIMARY KEY,
		session_id      TEXT NOT NULL,
		tool_name       TEXT NOT NULL,
		action          TEXT NOT NULL,
		invocation_id   TEXT NOT NULL,
		actor_kind      TEXT NOT NULL,
		framework_name  TEXT,
		payload_hash    TEXT NOT NULL,
		result_status   TEXT NOT NULL CHECK (result_status IN ('success','error')),
		started_at      INTEGER NOT NULL,
		completed_at    INTEGER,
		duration_ms     INTEGER,
		error_message   TEXT
	);`,
	`CREATE INDEX IF NOT EXISTS idx_tool_invocations_session
		ON tool_invocations(session_id, started_at DESC);`,
	`CREATE INDEX IF NOT EXISTS idx_tool_invocations_invocation
		ON tool_invocations(invocation_id);`,
	`CREATE INDEX IF NOT EXISTS idx_tool_invocations_tool_time
		ON tool_invocations(tool_name, started_at DESC);`,

	// idx_provenance_parent_invocation: powers the agent invocation
	// tree reconstruction query (WHERE parent_invocation_id = ?).
	// Lives in CommonIndexes (not BaseTables) because the column was
	// added in alpha-3 telemetry hardening — SafeMigrations adds it
	// first, then this CREATE INDEX runs against the live column.
	// migrateArtifactProvenanceSchema also rebuilds it inside its
	// table-recreate transaction; CREATE INDEX IF NOT EXISTS makes
	// both paths idempotent.
	`CREATE INDEX IF NOT EXISTS idx_provenance_parent_invocation
		ON artifact_provenance(parent_invocation_id);`,

	// ── Drill Run Ledger (2026-08-12) ──────────────────────────────
	//
	// One row per drill execution. The compatibility-matrix query
	// (cmd/mpm/drill_cmds.go handleDrillsReport) groups by (drill_id,
	// framework) and surfaces the latest verdict; the composite index
	// keeps that scan cheap as the ledger grows.
	`CREATE TABLE IF NOT EXISTS drill_runs (
		id              TEXT PRIMARY KEY,
		drill_id        TEXT NOT NULL,
		framework       TEXT NOT NULL,
		session_id      TEXT NOT NULL,
		status          TEXT NOT NULL CHECK (status IN ('running','passed','failed','error')),
		verdict         JSON,
		started_at      INTEGER NOT NULL,
		completed_at    INTEGER,
		duration_ms     INTEGER,
		error_message   TEXT
	);`,
	`CREATE INDEX IF NOT EXISTS idx_drill_runs_session
		ON drill_runs(session_id);`,
	`CREATE INDEX IF NOT EXISTS idx_drill_runs_drill
		ON drill_runs(drill_id, started_at DESC);`,
}

// BlobsTable contains the blob storage table for MCP result spilling.
var BlobsTable = []string{
	`CREATE TABLE IF NOT EXISTS blobs (
		id             TEXT PRIMARY KEY,
		source_tool    TEXT NOT NULL,
		source_call_id TEXT,
		session_id     TEXT,
		size_bytes     INTEGER NOT NULL,
		content_type   TEXT NOT NULL DEFAULT 'application/json',
		created_at     INTEGER NOT NULL,
		expires_at     INTEGER NOT NULL,
		checksum       TEXT
	);`,
	`CREATE INDEX IF NOT EXISTS idx_blobs_expires_at ON blobs(expires_at);`,
	`CREATE INDEX IF NOT EXISTS idx_blobs_session_id ON blobs(session_id);`,
}

// SafeMigrations contains column additions that may be needed for existing databases.
// Format: table name, column name, column type
var SafeMigrations = [][3]string{
	// alpha-3 telemetry: parent_invocation_id on artifact_provenance
	// (pre-added by SafeMigrations so CommonIndexes' CREATE INDEX
	// succeeds before the broader CHECK widening in
	// migrateArtifactProvenanceSchema runs).
	{"artifact_provenance", "parent_invocation_id", "TEXT"},
	{"topics", "parent_topic_id", "TEXT"},
	{"topics", "embedding", "BLOB"},
	{"memories", "embedding", "BLOB"},
	{"memories", "deleted_at", "INTEGER"},
	{"memories", "reference_id", "TEXT"},
	{"memories", "content_hash", "TEXT"},
	// F19: persisted duplicate-identity key (collection+content+tags+
	// stable metadata). Empty for legacy rows; the partial unique index
	// below therefore only constrains NEW writes and cannot fail to build
	// on databases that already contain pre-F19 duplicates.
	{"memories", "identity_hash", "TEXT"},
	{"memories", "is_prime_directive", "INTEGER DEFAULT 0"},
	{"memories", "toxicity_score", "REAL"},
	{"memories", "session_id", "TEXT"},
	{"memories", "is_long_term", "INTEGER DEFAULT 0"},
	{"memories", "weight", "REAL DEFAULT 1.0"},
	{"memories", "reinforcement_count", "INTEGER DEFAULT 0"},
	{"memories", "last_accessed_at", "INTEGER"},
	{"memories", "updated_at", "INTEGER"},
	{"memories", "expires_at", "INTEGER"},
	{"memories", "source_db", "TEXT"},
	{"memories", "source_id", "TEXT"},
	{"memories", "promoted_at", "REAL"},
	{"sessions", "embedding", "BLOB"},
	{"sessions", "metadata", "JSON"},
	{"raw_memories", "next_retry", "TEXT"},
	{"raw_memories", "attempt", "INTEGER DEFAULT 0"},
	{"memories", "retrieval_priority", "REAL NOT NULL DEFAULT 0.5"},
	{"memories", "importance",         "REAL NOT NULL DEFAULT 0.5"},
	{"memories", "confidence",         "REAL NOT NULL DEFAULT 0.8"},
	// is_global marks rows that originated as shared (cross-agent) rules.
	// Local writes always set 0; shared writes (via record_global_rule or
	// promote_to_global in Phase 3) set 1. The shared DB schema mirrors
	// the local DB so this migration is applied to both via attachShared.
	{"memories", "is_global",          "INTEGER NOT NULL DEFAULT 0"},
	{"lessons",  "retrieval_priority", "REAL NOT NULL DEFAULT 0.5"},
	{"memories", "last_synthesized_at", "INTEGER"},
	{"lessons",  "importance",         "REAL NOT NULL DEFAULT 0.5"},
	{"lessons",  "confidence",         "REAL NOT NULL DEFAULT 0.7"},
	{"reference_docs", "import_reason", "TEXT"},
	{"reference_chunks", "content_hash", "TEXT"},
	{"reference_chunks", "embedding",     "BLOB"},
	// dependencies: JSON array of artifact IDs that this theory depends on
	// (forward edges: theory -> memory/lesson/theory). Populated only on
	// collection='theories' rows. FireStaleFoundationWakes scans this column
	// on memory delete to detect "foundational rotted, theory orphaned".
	// Format: JSON array of strings, e.g. ["mem-abc","les-def"].
	{"memories", "dependencies",       "TEXT"},

	// work_events migration (Phase 2 work primitive v2):
	// migrated_at tracks which works rows have been seeded with initial created events.
	{"works", "migrated_at", "INTEGER"},

	// Phase 2 provenance/verification (Aug 23 incident):
	// verification status on works; evidence fields on work_events.
	// Git-specific columns removed — Git state now recorded as evidence
	// table rows (type: observation), not hardcoded event columns.
	// Duplicate provenance columns removed — join via invocation_id to
	// artifact_provenance instead.
	{"works", "verification", "TEXT"},
	{"work_events", "directive_ids", "TEXT DEFAULT '[]'"},
}
