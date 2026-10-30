#!/usr/bin/env python3
"""live_gate.py — the rehearsal's live-installation isolation gate.

WHAT THIS IS
------------
The rehearsal proves that the runtime-migration procedure can be executed
without touching the live installation.  The gate is the FILE-STATE half
of that proof: it takes a classified snapshot of the live tree before
and after the rehearsal, and it decides whether anything changed that
the rehearsal must not have changed.

WHY IT IS NOT A SINGLE SNAPSHOT
-------------------------------
The gate this replaces hashed `(path, size, mtime)` over a list of live
paths, including the live DATABASE FILE.  That assertion was invalid,
and it failed for a reason that had nothing to do with the rehearsal:

    The live scheduler records health checks, and SQLite checkpoints the
    WAL.  Both of those change the live database's SIZE and MTIME.  So
    the snapshot differed across the rehearsal window and the suite
    reported "the live tree's snapshot is unchanged" as a FAILURE while
    the rehearsal had touched nothing at all.

That is a false positive, and a false positive in an isolation gate is
worse than no gate: it teaches the operator to ignore the check, and that
is exactly how a real change gets missed.

THE CLASSIFICATION
------------------
The live installation is a LIVE thing.  Other writers exist — the
scheduler, the host's other agent sessions, the operator — so "is it
byte-identical?" is the wrong question for most of it.  The right
question is per-ARTIFACT, and each artifact gets a class with ONE rule:

  PROTECTED    The artifacts that genuinely must not change, and whose
               BYTES are the thing that matters: the installed runtime
               payload, the host assets, the systemd unit, the OpenCode
               configuration, the managed instructions, and THE RELEASE
               POINTER that names the active revision.  Compared by
               CONTENT (path set, mode, sha256) and by LINK TARGET.
               Their mtime is deliberately NOT part of the comparison:
               mtime records when a file was written, not what it
               contains.

  CONFIGURATION  `system_config` — the substrate's configuration.  A
               change here is a real finding, with ONE narrow, measured
               exception: the run-marker keys, whose whole content is
               "when did this last run".  Every other key is a FAILURE.

  AUDIT        `tool_invocations` and `system_audit_log` — the audit
               trail.  Rows may be ADDED.  A row that is MODIFIED or
               REMOVED is a FAILURE: an audit trail is append-only, and
               rewriting history is a finding in itself.

  KNOWLEDGE    `memories`, `lessons`, `evidence` — the operator's
               knowledge, with the volatile bookkeeping columns
               excluded.  This is the substrate's own established
               definition of "the operator's knowledge"
               (state_fingerprint.py --content), so the live database is
               measured by exactly the same instrument as the simulated
               one.

  BOOKKEEPING  Everything else — the live system's own state: its
               counters, its model-utility views, its work records.

THE ACCOUNTING RULE — WHY THE LAST TWO CLASSES ARE NOT HARD FAILURES
--------------------------------------------------------------------
A hard failure on the knowledge tables would be a false positive, and
the measurement says so plainly.  Over four 45-second windows with the
live system running (the scheduler, the host's agent sessions and the
operator all writing):

    memories             +8   +1        the agent sessions record decisions
    evidence             +1              and theories continuously
    lesson_proposals     +4              the compaction drain's proposals
    memory_revisions     +8   +2        every write carries a revision
    tool_invocations    +26  +10        the audit trail of those writes
    system_config        +1   +1        the run markers, rewritten in place

`memories` and `evidence` ARE the knowledge tables, and they grew.  A
gate that failed on any knowledge change would fail on an idle live
system, which is the same defect this gate exists to remove.

So the gate does not ask "did the knowledge change?" — the live system
legitimately changes it.  It asks a question that CAN be answered:

    THE ACCOUNTING RULE.  Every write the live system makes goes
    through MPM, and MPM records the invocation in its audit trail.
    So a change to the live database that the audit trail does NOT
    account for is UNATTRIBUTED — and the rehearsal's own writes would
    be unattributed too.  An unattributed change is a FAILURE.

The rule is narrow, explicit and fail-closed: knowledge and
bookkeeping changes are permitted only while the audit trail recorded
something in the same window; anything else fails.

THE RELEASE POINTER
-------------------
The versioned layout puts the active revision behind a SYMLINK:

    <data root>/current -> releases/<rev>
    <data root>/bin -> current/bin
    <data root>/agent_installation -> current/agent_installation

Hashing the resolved files alone cannot see the pointer change: the
same content behind a different pointer is a different installation.
The gate therefore records the LITERAL link target of each of those
paths, and the release's own identity record (`current/RELEASE.json`).
Repointing the pointer is a FAILURE even when the content behind it is
byte-identical — which is exactly the case a content hash cannot see.

THE GATE FAILS CLOSED
----------------------
If the live database cannot be read, the gate does not report "nothing
changed" — it reports that it could not tell.  A gate that cannot
measure must not pass.

EXIT CODES
----------
  0  nothing unauthorized changed
  1  an unauthorized change was detected
  2  the gate could not measure (the live database is unreadable, the
     live tree is missing, or the snapshots are not comparable)

USAGE
-----
  live_gate.py snapshot --live-mpm PATH --oc-config PATH
                       --systemd-user PATH [--db PATH] --out FILE
  live_gate.py compare BEFORE.json AFTER.json [--quiet]
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sqlite3
import sys

VERSION = 2

# ── the protected artifacts ────────────────────────────────────────────────
#
# These are the artifacts whose CONTENT must not change during the
# rehearsal.  The live DATABASE is deliberately NOT among them: the live
# system writes to it, so its bytes move for reasons that have nothing
# to do with the rehearsal.  It is measured by class instead.
PROTECTED_DIRS = ("bin", "agent_installation")
PROTECTED_FILES = (
    "systemd/mpm-scheduler.service",
    "opencode/opencode.jsonc",
    "opencode/AGENTS.md",
    "current/RELEASE.json",   # the active release's own identity record
)

# ── the release pointer ────────────────────────────────────────────────────
#
# The versioned layout resolves the runtime through symlinks.  These
# paths are recorded by their LITERAL link target, so repointing the
# active revision is visible even when the content behind it is
# byte-identical.
POINTER_PATHS = ("current", "bin", "agent_installation")

# ── the configuration table ────────────────────────────────────────────────
#
# `system_config` holds CONFIGURATION: keys whose values change what the
# system does.  A change there is a real finding — EXCEPT for the
# run-marker keys, whose entire content is "when did this last run".
# Those are the live system's own bookkeeping.
#
# MEASURED, not guessed.  With the production scheduler running and the
# host's agent sessions active, exactly two keys of `system_config`
# moved in place across a three-minute window:
#
#   scheduler_active_uptime   the scheduler's liveness counter
#   compaction.last_run       the compaction drain's last-run marker
#
# Every other key is a FAILURE.
CONFIG_RUN_MARKERS = {
    "scheduler_active_uptime": ("raw_json", "updated_at"),
    "compaction.last_run": ("raw_json", "updated_at"),
}

# ── the audit trail ────────────────────────────────────────────────────────
#
# Append-only.  A row that is modified or removed is a FAILURE.
AUDIT_TABLES = (
    "tool_invocations",
    "system_audit_log",
)

# The domain tables — the operator's knowledge.  The same three the
# substrate's own state fingerprint defines.
DOMAIN_TABLES = ("memories", "lessons", "evidence")

# The columns that record bookkeeping rather than content.  Reading a
# memory updates them, and the background decay adjusts others; hashing
# the raw row would report a difference after nothing more than a query.
VOLATILE_COLUMNS = {
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


class MeasurementError(Exception):
    """The gate could not measure.  It must not report success."""


# ── hashing helpers ────────────────────────────────────────────────────────


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def file_entry(path: str) -> dict:
    """{mode, size, sha256} for one regular file."""
    try:
        st = os.stat(path)
        return {
            "mode": oct(st.st_mode & 0o7777),
            "size": st.st_size,
            "sha256": sha256_file(path),
        }
    except OSError as e:
        raise MeasurementError(f"cannot read the protected file {path}: {e}") from e


def link_entry(path: str) -> dict:
    """What a protected path IS: a symlink (and where it points), a
    directory, a file, or absent.

    The LITERAL link target is recorded, not the resolved path: the
    question is "does this installation still name the same revision?",
    and `releases/A` and `releases/B` are different installations even
    when they hold identical bytes.
    """
    if not os.path.lexists(path):
        return {"absent": True}
    if os.path.islink(path):
        return {"symlink": os.readlink(path)}
    if os.path.isdir(path):
        return {"kind": "directory"}
    return {"kind": "file"}


def walk_files(root: str) -> list[str]:
    """Every regular file under root, following symlinked directories.

    os.walk's default is to NOT descend into a symlinked directory,
    which would leave the content behind such a link unmeasured.  The
    walk therefore tracks the real paths it has already visited, so a
    symlink cycle cannot make it loop.
    """
    out: list[str] = []
    seen: set[str] = set()

    def visit(path: str) -> None:
        try:
            real = os.path.realpath(path)
        except OSError:
            return
        if real in seen:
            return
        seen.add(real)
        try:
            entries = sorted(os.scandir(path), key=lambda e: e.name)
        except OSError as e:
            raise MeasurementError(f"cannot read the protected directory {path}: {e}") from e
        for entry in entries:
            if entry.is_symlink():
                # The link's own target is recorded separately; the
                # content behind it is reached by following it.
                if entry.is_dir():
                    visit(entry.path)
                else:
                    out.append(entry.path)
                continue
            if entry.is_dir():
                visit(entry.path)
            elif entry.is_file():
                out.append(entry.path)

    if os.path.isdir(root):
        visit(root)
    return sorted(out)


def protected_inventory(live_mpm: str, oc_config: str, systemd_user: str) -> dict:
    """{abs path: {mode, size, sha256}} for every protected artifact.

    A protected artifact that does not exist is recorded as absent with
    its own entry, so that an artifact that APPEARS during the
    rehearsal is a change, not a non-event.
    """
    inv: dict[str, dict] = {}
    for rel in PROTECTED_DIRS:
        root = os.path.join(live_mpm, rel)
        if not os.path.isdir(root):
            inv[root] = {"absent": True}
            continue
        for path in walk_files(root):
            inv[path] = file_entry(path)
    for rel in PROTECTED_FILES:
        if rel.startswith("systemd/"):
            p = os.path.join(systemd_user, rel[len("systemd/"):])
        elif rel.startswith("opencode/"):
            p = os.path.join(oc_config, rel[len("opencode/"):])
        elif rel.startswith("current/"):
            p = os.path.join(live_mpm, rel)
        else:
            p = os.path.join(live_mpm, rel)
        if not os.path.exists(p):
            inv[p] = {"absent": True}
            continue
        inv[p] = file_entry(p)
    return inv


def pointer_inventory(live_mpm: str) -> dict:
    """{abs path: link entry} for the release pointer and its symlinks."""
    return {
        os.path.join(live_mpm, rel): link_entry(os.path.join(live_mpm, rel))
        for rel in POINTER_PATHS
    }


# ── the database ───────────────────────────────────────────────────────────


def table_columns(c: sqlite3.Connection, table: str):
    """([column names], [primary-key column indexes]) or (None, None)."""
    try:
        info = list(c.execute(f'PRAGMA table_info("{table}")'))
    except Exception:
        return None, None
    if not info:
        return None, None
    cols = [r[1] for r in info]
    pk = [r[5] for r in info if len(r) > 5 and r[5]]
    return cols, pk


def table_fingerprint(c: sqlite3.Connection, table: str, drop_volatile: bool):
    """(sha256, row count) for one table, or None when it does not exist.

    A table that EXISTS but cannot be read is NOT None: it raises
    MeasurementError.  Silently skipping it would be a "could not
    measure" reported as "nothing changed" — the exact failure shape
    this gate exists to remove.
    """
    cols, _ = table_columns(c, table)
    if not cols:
        return None
    if drop_volatile:
        cols = [x for x in cols if x not in VOLATILE_COLUMNS]
    if not cols:
        return None
    sel = ", ".join(f'"{x}"' for x in cols)
    try:
        rows = c.execute(f'SELECT {sel} FROM "{table}"').fetchall()
    except Exception as e:
        raise MeasurementError(
            f"cannot read the live table {table}: {e}"
        ) from e
    h = hashlib.sha256()
    h.update(f"{table}:{len(rows)}\n".encode())
    for r in sorted(repr(x) for x in rows):
        h.update(r.encode())
        h.update(b"\x00")
    return h.hexdigest(), len(rows)


def table_rows(c: sqlite3.Connection, table: str, drop_volatile: bool = False) -> dict:
    """{row key: row hash} for one table.

    The row key is the primary key when the table has one, so an APPEND
    and an IN-PLACE UPDATE can be told apart.  A table without a primary
    key is keyed by its own content, so a change shows as one removal
    and one addition.

    For the DOMAIN tables the volatile bookkeeping columns are excluded,
    so that this projection and the DOMAIN fingerprint are the SAME
    instrument: a read that bumps `last_accessed_at` is not a change to
    the operator's knowledge.
    """
    cols, pk = table_columns(c, table)
    if not cols:
        return {}
    if drop_volatile:
        cols = [x for x in cols if x not in VOLATILE_COLUMNS]
        if not cols:
            return {}
    sel = ", ".join(f'"{x}"' for x in cols)
    try:
        rows = c.execute(f'SELECT {sel} FROM "{table}"').fetchall()
    except Exception as e:
        raise MeasurementError(
            f"cannot read the live table {table}: {e}"
        ) from e
    out: dict[str, str] = {}
    for r in rows:
        if pk:
            key = "|".join(repr(r[i]) for i in pk if i < len(r))
        else:
            key = ""
        h = hashlib.sha256(repr(r).encode()).hexdigest()[:32]
        if key in out:
            # A duplicate primary key is impossible; fall back to the
            # content hash so the row is still counted.
            key = f"{key}#{h[:8]}"
        out[key] = h
    return out


def config_state(c: sqlite3.Connection) -> dict:
    """{key: {column: value}} for the configuration table."""
    cols, _ = table_columns(c, "system_config")
    if not cols:
        return {}
    sel = ", ".join(f'"{x}"' for x in cols)
    try:
        rows = c.execute(f'SELECT {sel} FROM "system_config"').fetchall()
    except Exception as e:
        raise MeasurementError(f"cannot read the live configuration: {e}") from e
    out: dict[str, dict] = {}
    for r in rows:
        out[str(r[0])] = {name: (repr(v) if not isinstance(v, (str, int, float, type(None))) else v)
                          for name, v in zip(cols, r)}
    return out


def database_state(db_path: str) -> dict:
    """The classified state of the live database.

    Raises MeasurementError when the database exists but cannot be read:
    a gate that cannot measure must not report success.
    """
    if not os.path.exists(db_path):
        return {"present": False}

    try:
        c = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    except Exception as e:
        raise MeasurementError(f"cannot open the live database {db_path}: {e}") from e

    state: dict = {"present": True, "path": db_path, "tables": {}, "rows": {},
                   "domain": {}, "schema": {}, "config": {}}
    try:
        names = [r[0] for r in c.execute(
            "SELECT name FROM sqlite_master WHERE type IN ('table','view') ORDER BY name")]
        for t in names:
            if t.startswith("sqlite_"):
                continue
            cols, _ = table_columns(c, t)
            if cols:
                state["schema"][t] = cols
            fp = table_fingerprint(c, t, drop_volatile=(t in DOMAIN_TABLES))
            if fp is not None:
                state["tables"][t] = {"hash": fp[0], "rows": fp[1]}
            state["rows"][t] = table_rows(c, t, drop_volatile=(t in DOMAIN_TABLES))
        for t in DOMAIN_TABLES:
            fp = table_fingerprint(c, t, drop_volatile=True)
            if fp is None:
                # The table does not exist in this schema.  That is a
                # measurement of the schema, not of the content.
                state["domain"][t] = None
            else:
                state["domain"][t] = fp[0]
        state["config"] = config_state(c)
    except MeasurementError:
        raise
    except Exception as e:
        raise MeasurementError(f"cannot read the live database {db_path}: {e}") from e
    finally:
        c.close()
    return state


def file_state(path: str) -> dict:
    """Size and mtime of the database FILE — reported, never asserted."""
    if not os.path.exists(path):
        return {"present": False}
    st = os.stat(path)
    return {"present": True, "size": st.st_size, "mtime": st.st_mtime}


# ── snapshot ────────────────────────────────────────────────────────────────


def do_snapshot(args) -> int:
    db_path = args.db or os.path.join(args.live_mpm, "src", "db", "mpm.db")
    snap = {
        "version": VERSION,
        "protected": protected_inventory(args.live_mpm, args.oc_config, args.systemd_user),
        "pointers": pointer_inventory(args.live_mpm),
        "db": database_state(db_path),
        "db_file": file_state(db_path),
    }
    with open(args.out, "w") as fh:
        json.dump(snap, fh, indent=1, sort_keys=True)
    return 0


# ── compare ────────────────────────────────────────────────────────────────


def compare_protected(before: dict, after: dict) -> list[str]:
    """The protected artifacts that changed, named."""
    problems = []
    paths = sorted(set(before) | set(after))
    for p in paths:
        b = before.get(p)
        a = after.get(p)
        if b == a:
            continue
        if b is None:
            problems.append(f"{p}: APPEARED (it did not exist before the rehearsal)")
            continue
        if a is None:
            problems.append(f"{p}: DISAPPEARED")
            continue
        if b.get("absent") and not a.get("absent"):
            problems.append(f"{p}: APPEARED (it did not exist before the rehearsal)")
            continue
        if a.get("absent"):
            problems.append(f"{p}: DISAPPEARED")
            continue
        diffs = []
        if b.get("sha256") != a.get("sha256"):
            diffs.append("content changed")
        if b.get("mode") != a.get("mode"):
            diffs.append(f"mode {b.get('mode')} -> {a.get('mode')}")
        if b.get("size") != a.get("size"):
            diffs.append(f"size {b.get('size')} -> {a.get('size')}")
        problems.append(f"{p}: {', '.join(diffs)}")
    return problems


def compare_pointers(before: dict, after: dict) -> list[str]:
    """The release pointer and its symlinks that changed, named.

    This is the check a content hash cannot make: the same bytes
    behind a different pointer are a DIFFERENT installation.
    """
    problems = []
    for p in sorted(set(before) | set(after)):
        b = before.get(p)
        a = after.get(p)
        if b == a:
            continue
        if b is None:
            problems.append(f"{p}: APPEARED")
            continue
        if a is None:
            problems.append(f"{p}: DISAPPEARED")
            continue
        if b.get("absent") and not a.get("absent"):
            problems.append(f"{p}: APPEARED (the live installation has no "
                            f"release pointer before the rehearsal, and one now)")
            continue
        if a.get("absent"):
            problems.append(f"{p}: DISAPPEARED (the release pointer is gone)")
            continue
        if b.get("symlink") != a.get("symlink"):
            problems.append(
                f"{p}: the release pointer moved "
                f"({b.get('symlink') or b.get('kind')} -> {a.get('symlink') or a.get('kind')})"
            )
        elif b.get("kind") != a.get("kind"):
            problems.append(
                f"{p}: changed from {b.get('kind')} to {a.get('kind')}"
            )
    return problems


def compare_schema(before: dict, after: dict) -> list[str]:
    """Tables that appeared or disappeared."""
    b = set((before.get("schema") or {}).keys())
    a = set((after.get("schema") or {}).keys())
    problems = []
    for t in sorted(a - b):
        problems.append(f"{t}: a NEW table appeared in the live database")
    for t in sorted(b - a):
        problems.append(f"{t}: a table DISAPPEARED from the live database")
    return problems


def compare_config(before: dict, after: dict) -> list[str]:
    """Configuration changes that the run-marker exception does not cover.

    The exception is narrow: only the named run-marker keys may change,
    and only in the named columns.  Everything else is a FAILURE.
    """
    b = before or {}
    a = after or {}
    problems = []
    for key in sorted(set(b) | set(a)):
        if key not in b:
            problems.append(f"{key}: a NEW configuration key appeared")
            continue
        if key not in a:
            problems.append(f"{key}: a configuration key DISAPPEARED")
            continue
        allowed = CONFIG_RUN_MARKERS.get(key)
        for col in sorted(set(b[key]) | set(a[key])):
            if b[key].get(col) == a[key].get(col):
                continue
            if allowed and col in allowed:
                continue
            if allowed:
                problems.append(
                    f"{key}.{col}: changed, and only {', '.join(allowed)} may "
                    f"change for this run marker"
                )
            else:
                problems.append(
                    f"{key}.{col}: the live CONFIGURATION changed, and "
                    f"{key} is not a run marker"
                )
    return problems


def row_delta(before: dict, after: dict) -> dict:
    """{table: (added, changed, removed)} for every table that moved."""
    out = {}
    b = before or {}
    a = after or {}
    for t in sorted(set(b) | set(a)):
        B = b.get(t) or {}
        A = a.get(t) or {}
        added = len(set(A) - set(B))
        removed = len(set(B) - set(A))
        changed = sum(1 for k in set(A) & set(B) if A[k] != B[k])
        if added or removed or changed:
            out[t] = (added, changed, removed)
    return out


def audit_accounted(before: dict, after: dict) -> bool:
    """Did the live system's own audit trail record anything in the window?

    Every write the live system makes goes through MPM, and MPM records
    the invocation in `tool_invocations`.  So an audit row appearing in
    the window is what makes the window's other movement attributable
    to the live system rather than to something the rehearsal did.
    """
    b = (before or {}).get("tool_invocations") or {}
    a = (after or {}).get("tool_invocations") or {}
    return len(a) > len(b) or bool(set(a) - set(b))


def compare(before: dict, after: dict):
    """(failures, evidence) for the two snapshots."""
    failures: list[str] = []
    evidence: list[str] = []

    bdb = before.get("db") or {}
    adb = after.get("db") or {}

    for p in compare_protected(before.get("protected") or {},
                               after.get("protected") or {}):
        failures.append(f"a protected artifact changed: {p}")

    for p in compare_pointers(before.get("pointers") or {},
                              after.get("pointers") or {}):
        failures.append(f"the release pointer changed: {p}")

    for s in compare_schema(bdb, adb):
        failures.append(f"the live database's schema changed: {s}")

    for c in compare_config(bdb.get("config") or {}, adb.get("config") or {}):
        failures.append(f"the live configuration changed: {c}")

    # The permitted movement of a run marker is EVIDENCE, not silence:
    # it is what distinguishes "the scheduler was busy" from "something
    # rewrote the configuration".
    for key in sorted(set(bdb.get("config") or {}) | set(adb.get("config") or {})):
        allowed = CONFIG_RUN_MARKERS.get(key)
        if not allowed:
            continue
        B = (bdb.get("config") or {}).get(key) or {}
        A = (adb.get("config") or {}).get(key) or {}
        moved = [col for col in sorted(set(A) | set(B))
                 if A.get(col) != B.get(col)]
        if moved:
            evidence.append(
                f"the run marker {key} moved ({', '.join(moved)}) — "
                f"expected: it records when the live system last ran"
            )

    # The AUDIT trail is append-only.
    b_rows = bdb.get("rows") or {}
    a_rows = adb.get("rows") or {}
    for t in AUDIT_TABLES:
        if t not in b_rows and t not in a_rows:
            continue
        B = b_rows.get(t) or {}
        A = a_rows.get(t) or {}
        modified = sum(1 for k in set(A) & set(B) if A[k] != B[k])
        removed = len(set(B) - set(A))
        added = len(set(A) - set(B))
        if modified:
            failures.append(
                f"the audit trail was rewritten: {modified} row(s) of {t} changed in place"
            )
        if removed:
            failures.append(
                f"the audit trail lost rows: {removed} row(s) of {t} disappeared"
            )
        if added:
            evidence.append(
                f"the audit trail grew: {t} +{added} row(s) — this is the "
                f"live system's own record of what it did"
            )

    # The KNOWLEDGE tables, and everything else, are permitted only
    # while the audit trail accounts for the window.
    delta = row_delta(b_rows, a_rows)
    accounted = audit_accounted(b_rows, a_rows)

    knowledge_moved = {t: d for t, d in delta.items() if t in DOMAIN_TABLES}
    # The domain fingerprint is the substrate's own definition; it is
    # the thing the SIMULATED database is compared with.
    bdom = bdb.get("domain") or {}
    adom = adb.get("domain") or {}
    domain_moved = [t for t in sorted(set(bdom) | set(adom))
                    if bdom.get(t) != adom.get(t)]
    for t in domain_moved:
        knowledge_moved.setdefault(t, (0, 0, 0))

    if knowledge_moved and not accounted:
        for t, (added, changed, removed) in sorted(knowledge_moved.items()):
            failures.append(
                f"the operator's knowledge changed and NOTHING in the live "
                f"system's audit trail accounts for it: {t} "
                f"(+{added} added, ~{changed} changed, -{removed} removed)"
            )
    elif knowledge_moved:
        for t, (added, changed, removed) in sorted(knowledge_moved.items()):
            evidence.append(
                f"the operator's knowledge moved in {t} "
                f"(+{added} added, ~{changed} changed, -{removed} removed) "
                f"— ACCOUNTED FOR by the live system's own audit trail"
            )

    # Everything else is the live system's bookkeeping.
    for t, (added, changed, removed) in sorted(delta.items()):
        if t in DOMAIN_TABLES or t in AUDIT_TABLES:
            continue
        if t == "system_config":
            # The CONFIGURATION class above already ruled on this
            # table, key by key.  It is not also bookkeeping.
            continue
        line = (f"{t}: rows +{added} ~{changed} -{removed}")
        if accounted:
            evidence.append(
                f"the live system's own bookkeeping moved — {line} "
                f"— ACCOUNTED FOR by the audit trail"
            )
        else:
            failures.append(
                f"the live database changed and NOTHING in the live system's "
                f"audit trail accounts for it: {line}"
            )

    # The database FILE's size and mtime are reported, never asserted:
    # the live scheduler writes to it and SQLite checkpoints the WAL.
    bf = before.get("db_file") or {}
    af = after.get("db_file") or {}
    if bf.get("present") and af.get("present") and (
        bf.get("size") != af.get("size") or bf.get("mtime") != af.get("mtime")
    ):
        evidence.append(
            "the live database FILE changed "
            f"(size {bf.get('size')} -> {af.get('size')}, "
            f"mtime {bf.get('mtime')} -> {af.get('mtime')}) "
            "— expected: the live scheduler records health checks and SQLite "
            "checkpoints the WAL. The file's bytes are NOT the assertion; the "
            "domain state and the configuration above are."
        )

    return failures, evidence


def do_compare(args) -> int:
    try:
        with open(args.before) as fh:
            before = json.load(fh)
        with open(args.after) as fh:
            after = json.load(fh)
    except Exception as e:
        print(f"LIVE-GATE: cannot read the snapshots: {e}", file=sys.stderr)
        return 2
    if before.get("version") != after.get("version"):
        print("LIVE-GATE: the two snapshots were taken by different gate versions",
              file=sys.stderr)
        return 2

    failures, evidence = compare(before, after)

    if not args.quiet:
        print("LIVE-GATE: the live installation's isolation")
        prot = before.get("protected") or {}
        prot_changed = compare_protected(prot, after.get("protected") or {})
        ptr = before.get("pointers") or {}
        ptr_changed = compare_pointers(ptr, after.get("pointers") or {})
        bdom = (before.get("db") or {}).get("domain") or {}
        adom = (after.get("db") or {}).get("domain") or {}
        dom_changed = [t for t in sorted(set(bdom) | set(adom))
                       if bdom.get(t) != adom.get(t)]
        print(f"  protected artifacts checked : {len(prot)}")
        print(f"  protected artifacts changed : {len(prot_changed)}")
        print(f"  release pointers checked    : {len(ptr)}")
        for p in sorted(ptr):
            entry = ptr[p]
            what = entry.get("symlink") or entry.get("kind") or "absent"
            print(f"      {os.path.basename(p):<20s} -> {what}")
        print(f"  release pointers changed    : {len(ptr_changed)}")
        print(f"  domain tables checked       : {len(bdom)}")
        print(f"  domain tables changed       : {len(dom_changed)}")
        if evidence:
            print("  movement observed (reported, with the evidence that accounts for it):")
            for e in evidence:
                print(f"    - {e}")
        if not evidence:
            print("  no database movement at all")

    if failures:
        print("", file=sys.stderr)
        print("LIVE-GATE: UNAUTHORISED CHANGE IN THE LIVE INSTALLATION", file=sys.stderr)
        for f in failures:
            print(f"  {f}", file=sys.stderr)
        return 1
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    s = sub.add_parser("snapshot")
    s.add_argument("--live-mpm", required=True)
    s.add_argument("--oc-config", required=True)
    s.add_argument("--systemd-user", required=True)
    s.add_argument("--db", default=None)
    s.add_argument("--out", required=True)
    s.set_defaults(func=do_snapshot)

    c = sub.add_parser("compare")
    c.add_argument("before")
    c.add_argument("after")
    c.add_argument("--quiet", action="store_true")
    c.set_defaults(func=do_compare)

    args = ap.parse_args()
    try:
        return args.func(args)
    except MeasurementError as e:
        print(f"LIVE-GATE: could not measure: {e}", file=sys.stderr)
        print("LIVE-GATE: a gate that cannot measure must not report success.",
              file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
