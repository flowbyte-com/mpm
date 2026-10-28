#!/usr/bin/env python3
"""state_fingerprint.py <db> [--content|--full]

Prints a stable fingerprint of a MPM database's LOGICAL content.

  --content   the operator's knowledge only — what an upgrade must not change.
  --full      also the audit trail (tool_invocations) and the schema.

VOLATILE COLUMNS ARE EXCLUDED
-----------------------------
A memory row carries lifecycle bookkeeping: `last_accessed_at`,
`updated_at`, `reinforcement_count`, `runtime_seconds_since_access`,
`retrieval_priority`, `importance`, `weight`. Reading a memory updates
some of them, and the background decay adjusts others. Hashing the raw
row therefore reports a difference after nothing more than a query —
which teaches an operator to ignore the check, and that is how a real
difference gets missed.

What is hashed is the IDENTITY and the CONTENT: the id, the collection,
the text, the tags, the metadata, and the timestamps that record when the
thing was written (not when it was last looked at).

Decisions and theories are NOT separate tables: they are `memories` rows
with collection='decisions' / collection='theories', so fingerprinting
`memories` covers all three. `lessons` is a VIEW over `lessons_base`;
querying the view fingerprints the visible lesson content.
"""
import hashlib
import sqlite3
import sys

VOLATILE = {
    "last_accessed_at",
    "updated_at",
    "reinforcement_count",
    "runtime_seconds_since_access",
    "runtime_last_accrued_at",
    "retrieval_priority",
    "importance",
    "weight",
    "confidence",
    "last_synthesized_at",
    "confidence_checked_at",
    "embedding",
    "embedding_source",
    "embedding_dimension",
    "embedding_model",
    "toxicity_score",
    "is_long_term",
    "synthetic",
    "promoted_at",
    "content_hash",
    "identity_hash",
    "read_at",
    "read_by",
    "applied_at",
    "accessed_at",
    "call_count",
    "last_called_at",
}

CONTENT_TABLES = ("memories", "lessons", "evidence")
FULL_TABLES = CONTENT_TABLES + ("tool_invocations", "schema_migrations", "sessions")


def fingerprint(c, table, drop_volatile):
    try:
        cols = [r[1] for r in c.execute(f'PRAGMA table_info("{table}")')]
    except Exception:
        return None
    if not cols:
        return None
    if drop_volatile:
        cols = [c2 for c2 in cols if c2 not in VOLATILE]
    if not cols:
        return None
    sel = ", ".join(f'"{c2}"' for c2 in cols)
    try:
        rows = c.execute(f'SELECT {sel} FROM "{table}"').fetchall()
    except Exception:
        return None
    h = hashlib.sha256()
    h.update(f"{table}:{len(rows)}\n".encode())
    for r in sorted(repr(x) for x in rows):
        h.update(r.encode())
        h.update(b"\x00")
    return h


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: state_fingerprint.py <db> [--content|--full]", file=sys.stderr)
        return 2
    db = sys.argv[1]
    mode = "full"
    for a in sys.argv[2:]:
        if a == "--content":
            mode = "content"
        elif a == "--full":
            mode = "full"
    tables = CONTENT_TABLES if mode == "content" else FULL_TABLES
    try:
        c = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    except Exception as e:
        print("FINGERPRINT-ERROR:" + str(e))
        return 0
    h = hashlib.sha256()
    try:
        for t in tables:
            fh = fingerprint(c, t, mode == "content")
            if fh is not None:
                h.update(fh.digest())
        print(h.hexdigest())
    finally:
        c.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
