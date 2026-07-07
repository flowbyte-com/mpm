// contradiction_schema_patch.go — schema additions for Arc 1.
//
// The contradiction_log table lives in the SHARED database (not
// local) because the conflict queue is a workstation-wide concern:
// if Agent A on project-foo detects a contradiction involving a
// global rule set by Agent B on project-bar, Agent C (and the
// operator) need to see the queue.
//
// This file is a "patch" (not a new BaseTable) because the table
// belongs to the shared DB schema namespace, not the local one.
// The actual CREATE TABLE statement is installed in
// DatabaseManager.attachShared alongside the FTS5 sync triggers
// from 61a8418. This file holds the DDL as a string constant so
// the SQL is in one place and testable independently of attach.
//
// INVARIANTS:
//   1. memory_id_a and memory_id_b are sorted lexicographically at
//      write time (EnqueueContradiction) so (A,B) and (B,A) hit
//      the same UNIQUE constraint row. This protects against the
//      "order swapped at the call site" class of bug.
//   2. resolved_at IS NULL is the "unresolved" filter. The
//      partial index makes that query O(unresolved) instead of
//      O(total rows) — operators can clear the queue without
//      re-scanning the entire history.
//   3. resolution_memory_id is a soft FK to shared.memories.id
//      (SQLite doesn't enforce FKs without explicit PRAGMA, so we
//      treat it as advisory). The resolution memory itself is the
//      durable audit trail; this column is the index for "show me
//      the resolution for this queue row".
//   4. The (memory_id_a, memory_id_b, detected_at) UNIQUE constraint
//      dedupes duplicate detections of the same pair. The
//      detected_at timestamp is part of the key so that two
//      genuine re-detections of the same contradiction (after a
//      manual reset, for example) don't collide.

package internal

// SharedContradictionLogDDL is the CREATE TABLE + index for the
// shared contradiction queue. Installed by attachShared. Lives in
// this file so the SQL is testable without going through the full
// DatabaseManager constructor.
//
// Note: no FTS5 mirror for contradiction_log itself. The `evidence`
// column is structured JSON, not free-form searchable text. Search
// across the queue is done by the resolution_memory's FTS row
// (the resolution memory has the full narrative). The queue is
// for the operator's eyes, not the search engine's.
//
// Note on the indexes: SQLite's ATTACH model forbids `CREATE INDEX
// ... ON shared.contradiction_log(...)` from the main connection
// (verified empirically — "near '.': syntax error"). The workaround
// is to prefix the INDEX NAME with `shared.` and use the bare table
// name. This creates the index in the shared DB, scoped to that
// schema. Partial indexes with WHERE clauses work in this form
// (resolved_at IS NULL gives us the unresolved worklist
// optimization). The "detected_at" full index is for the operator's
// "show me the queue in chronological order" query.
const SharedContradictionLogDDL = `
CREATE TABLE IF NOT EXISTS shared.contradiction_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    memory_id_a TEXT NOT NULL,
    memory_id_b TEXT NOT NULL,
    evidence TEXT NOT NULL,
    similarity REAL,
    detected_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    detected_by TEXT,
    resolved_at TEXT,
    resolution_memory_id TEXT,
    UNIQUE(memory_id_a, memory_id_b, detected_at)
);

CREATE INDEX IF NOT EXISTS shared.idx_contradiction_log_detected_at
    ON contradiction_log(detected_at);

CREATE INDEX IF NOT EXISTS shared.idx_contradiction_log_unresolved
    ON contradiction_log(resolved_at)
    WHERE resolved_at IS NULL;
`
