"""Real MPM schema for fixture-based harness tests (v4 review).

The pre-v4 schema fixture in `bench/tests/test_fingerprint.py` was a
stub: it had only the columns the tests queried, which is fine for
its own scope. The harness's mutation snapshot, however, needs a
schema that matches what an MPM subprocess actually creates, so a
mutating query produces a diff the snapshot detects. v4 review:
query the real schema; any column mismatch is a hard error, not a
silent comparison-equal placeholder.

The schema is derived from `internal/core/schema.go` in the
production MPM source tree. The exact DDL below is a subset of
the production table definitions — only the tables and columns
the harness reads for its mutation check. Real MPM has many
more tables and columns, but the harness's contract is just
"the tables I read exist with the columns I read". The
fixture-builder applies the relevant SafeMigrations so the
final shape matches what `mpm remember` produces on a fresh
workspace.

Tables covered (all on the LOCAL database, not the shared DB):
  memories, reference_chunks, retrieval_metadata
"""

from __future__ import annotations

import sqlite3
from pathlib import Path


# Schema DDL derived from internal/core/schema.go (BaseTables,
# SafeMigrations, CommonIndexes) and the reference_chunks table
# definition at line 789. This is the schema a fresh `mpm
# remember` produces on a real workspace; the harness's
# mutation snapshot expects to read it.
MPM_DISPOSABLE_SCHEMA_DDL: tuple[str, ...] = (
    # memories — internal/core/schema.go:78-85, 1408-1600 (SafeMigrations).
    """
    CREATE TABLE IF NOT EXISTS memories (
        id TEXT PRIMARY KEY,
        collection TEXT NOT NULL,
        content TEXT NOT NULL,
        session_id TEXT,
        tags JSON,
        metadata JSON,
        embedding BLOB,
        created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)) NOT NULL,
        updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
        source_db TEXT,
        source_id TEXT,
        promoted_at REAL,
        reference_id TEXT,
        content_hash TEXT,
        identity_hash TEXT,
        is_prime_directive INTEGER DEFAULT 0,
        toxicity_score REAL,
        is_long_term INTEGER DEFAULT 0,
        weight REAL DEFAULT 1.0,
        reinforcement_count INTEGER DEFAULT 0,
        last_accessed_at INTEGER,
        expires_at INTEGER,
        retrieval_priority REAL NOT NULL DEFAULT 0.5,
        importance REAL NOT NULL DEFAULT 0.5,
        confidence REAL NOT NULL DEFAULT 0.8,
        is_global INTEGER NOT NULL DEFAULT 0,
        last_synthesized_at INTEGER,
        embedding_source TEXT NOT NULL DEFAULT 'provider',
        embedding_dimension INTEGER,
        embedding_model TEXT,
        synthetic INTEGER NOT NULL DEFAULT 0,
        deleted_at INTEGER,
        dependencies TEXT,
        runtime_seconds_since_access INTEGER NOT NULL DEFAULT 0,
        runtime_last_accrued_at INTEGER
    )
    """,
    # retrieval_metadata — internal/core/schema.go:125-133.
    """
    CREATE TABLE IF NOT EXISTS retrieval_metadata (
        node_id TEXT PRIMARY KEY,
        node_type TEXT NOT NULL,
        reuse_count INTEGER DEFAULT 0,
        last_retrieved_at INTEGER,
        success_count INTEGER DEFAULT 0,
        created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
        updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
    )
    """,
    # reference_chunks — internal/core/schema.go:789-792 + SafeMigrations.
    """
    CREATE TABLE IF NOT EXISTS reference_chunks (
        id TEXT PRIMARY KEY,
        doc_id TEXT NOT NULL,
        chunk_index INTEGER NOT NULL,
        section TEXT,
        content TEXT NOT NULL,
        content_hash TEXT,
        embedding BLOB,
        embedding_source TEXT NOT NULL DEFAULT 'provider',
        embedding_dimension INTEGER,
        embedding_model TEXT
    )
    """,
)


def build_mpm_disposable_db(path: Path) -> None:
    """Create a disposable SQLite DB at `path` with the real MPM
    schema (subset sufficient for the harness's mutation check).

    v4 review: this is the schema the harness's `_snapshot_full`
    expects. Any test that exercises `_snapshot_full` MUST use
    this schema (or a superset of it) so the columns the
    snapshot reads are present. A test that builds a different
    schema will surface as a `SnapshotSchemaError` at the
    snapshot call, which is the load-bearing "fail closed"
    behavior the v4 review requires.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    con = sqlite3.connect(str(path))
    try:
        for ddl in MPM_DISPOSABLE_SCHEMA_DDL:
            con.execute(ddl)
        con.commit()
    finally:
        con.close()


def vec_blob(values: list[float]) -> bytes:
    """Encode a list of floats as a JSON BLOB. Mirrors the BLOB
    format used by `internal/core/reference_db.go:10-19`."""
    import json
    return json.dumps(values).encode("utf-8")
