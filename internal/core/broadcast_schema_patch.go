// broadcast_schema_patch.go — Shared DDL for Arc 2 (Active Dissemination).
//
// Three new tables, all in the `shared` schema:
//
//   1. shared.bcast_sessions  — Active agent registry. Heartbeat-discovered.
//      Every MPM tool call that touches the shared DB silently bumps
//      this row. Discovery = SELECT WHERE last_heartbeat > now - 24h.
//
//   2. shared.bcast_event_wakes — The fan-out surface. Each row is one
//      "an epistemic event happened, you should know" notification,
//      targeted at one session. wake_id is a deterministic SHA-256
//      prefix → INSERT OR IGNORE for O(1) dedup with no
//      application-level state.
//
//   3. shared.bcast_agents   — Agent identity cache. Stable across reboots;
//      shared.bcast_sessions is "this run", shared.bcast_agents is "this agent".
//
// Installed by DatabaseManager.attachShared alongside the FTS5 sync
// triggers and SharedContradictionLogDDL. Idempotent (CREATE TABLE
// IF NOT EXISTS, CREATE INDEX IF NOT EXISTS).
//
// Lifecycle: schemas added at install time, never dropped. If the
// schema evolves, SafeMigrations handles ALTER COLUMN (see
// SafeMigrate in db.go). On downgrade, drop tables manually after
// a backup.
//
// NAMING: All three tables are prefixed `bcast_` to avoid colliding
// with the BaseTables-local `sessions`, `agents`, etc. (which the
// `attachShared` loop installs into the shared schema with its own
// schema — the local "sessions" table stores handoff content, not
// active-agent rows, so re-using the bare name would silently
// collide). The prefix `bcast_` makes the namespace obvious at
// query time: anything in `shared.bcast_*` is Arc 2 fan-out state.
//
// Schema decisions:
//   - bcast_sessions.last_heartbeat is NOT indexed directly; we use
//     two helper indexes (agent_id, last_heartbeat DESC) and
//     (last_heartbeat DESC) so the discovery query is O(active).
//   - bcast_event_wakes.wake_id is a PRIMARY KEY (not UNIQUE) so the
//     dedup mechanism is O(1) at the SQLite engine level.
//   - bcast_event_wakes.fired=0 partial index gives the receiving side
//     O(pending) lookups regardless of total wakes written.
//   - bcast_event_wakes.content_hash stores the SHA-256 of (content +
//     sorted tags) so two different versions of the "same" memory
//     produce different wake IDs and thus different wake rows.

package internal

// SharedBcastSessionsDDL is the CREATE TABLE + index for the shared
// active-agent registry. Installed by attachShared. The table is
// append-mostly (every heartbeat is INSERT OR REPLACE) so row
// count is bounded by the number of distinct agents seen in
// the lifetime of the shared DB.
const SharedBcastSessionsDDL = `
CREATE TABLE IF NOT EXISTS shared.bcast_sessions (
    session_id      TEXT PRIMARY KEY,
    agent_id        TEXT NOT NULL,
    hostname        TEXT,
    last_heartbeat  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    boot_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    metadata        TEXT
);
CREATE INDEX IF NOT EXISTS shared.idx_bcast_sessions_agent
    ON bcast_sessions (agent_id, last_heartbeat DESC);
CREATE INDEX IF NOT EXISTS shared.idx_bcast_sessions_heartbeat
    ON bcast_sessions (last_heartbeat DESC);
`

// SharedBcastEventWakesDDL is the CREATE TABLE + indexes for the shared
// event-wake fan-out surface. The PRIMARY KEY on wake_id is the
// entire dedup mechanism: deterministic sha256 prefix + INSERT OR
// IGNORE = O(1) no-op for re-broadcasts.
//
// fired=0 partial index gives the receiving side (CheckPendingEventWakes)
// O(pending) lookups regardless of how many wakes have been picked up
// historically.
//
// Note on the indexes: SQLite's ATTACH model forbids `CREATE INDEX
// ... ON shared.bcast_event_wakes(...)` from the main connection.
// The workaround is to prefix the INDEX NAME with `shared.` and use
// the bare table name. Same workaround as Arc 1's contradiction
// log indexes. See contradiction_schema_patch.go for the empirical
// verification of this constraint.
//
// Note on the foreign key: the obvious FK `FOREIGN KEY (target_session)
// REFERENCES shared.bcast_sessions(session_id)` is rejected by
// SQLite on ATTACH-attached schemas ("near '.': syntax error",
// verified empirically). Same limitation as CREATE INDEX. We treat
// the FK as advisory (no PRAGMA foreign_keys=ON enforcement anyway)
// and rely on the broadcast code path to validate targets before
// INSERT.
const SharedBcastEventWakesDDL = `
CREATE TABLE IF NOT EXISTS shared.bcast_event_wakes (
    wake_id         TEXT PRIMARY KEY,
    target_session  TEXT NOT NULL,
    source_agent    TEXT NOT NULL,
    memory_id       TEXT NOT NULL,
    kind            TEXT NOT NULL,
    content_hash    TEXT NOT NULL,
    rationale       TEXT,
    fired           INTEGER NOT NULL DEFAULT 0,
    fired_at        DATETIME,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    metadata        TEXT
);
CREATE INDEX IF NOT EXISTS shared.idx_bcast_event_wakes_target_pending
    ON bcast_event_wakes (target_session, created_at ASC)
    WHERE fired = 0;
CREATE INDEX IF NOT EXISTS shared.idx_bcast_event_wakes_source
    ON bcast_event_wakes (source_agent, created_at DESC);
CREATE INDEX IF NOT EXISTS shared.idx_bcast_event_wakes_memory
    ON bcast_event_wakes (memory_id, created_at DESC);
`

// SharedBcastAgentsDDL is the CREATE TABLE + index for the shared
// agent identity cache. Stable across reboots; lets us answer
// questions like "how often does agent 808 broadcast vs. receive"
// without aggregating session rows.
const SharedBcastAgentsDDL = `
CREATE TABLE IF NOT EXISTS shared.bcast_agents (
    agent_id        TEXT PRIMARY KEY,
    first_seen      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    total_heartbeats INTEGER NOT NULL DEFAULT 0,
    metadata        TEXT
);
CREATE INDEX IF NOT EXISTS shared.idx_bcast_agents_last_seen
    ON bcast_agents (last_seen DESC);
`