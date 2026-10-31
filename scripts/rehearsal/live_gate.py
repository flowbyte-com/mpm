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
    through MPM, and MPM records the ORIGIN of what it wrote.  So a
    change to the live database whose origin the substrate does not
    record is UNATTRIBUTED — and the rehearsal's own writes would be
    unattributed too.  An unattributed change is a FAILURE.

THE ORIGIN CHAIN — WHY "THE AUDIT TRAIL MOVED" IS NOT ENOUGH
--------------------------------------------------------------
The previous version of this rule asked a WEAKER question: "did the
audit trail record ANY invocation in this window?"  That is temporal
correlation, not attribution.  ONE unrelated legitimate invocation — a
wake-context read, a scheduler health check — satisfied the rule for
EVERY concurrent change, including an unauthorised one, and the gate
then reported that unauthorised write as "ACCOUNTED FOR by the live
system's own audit trail".  That is a false claim, and an isolation
gate that makes false claims is worse than no gate at all.

The substrate records the origin of every artifact it writes:

    artifact_provenance.artifact_id -> invocation_id
                                    -> tool_invocations.invocation_id

That chain is the real primitive, and the rehearsal's own writes never
pass through it: they are raw SQL, they carry no provenance row, and
the chain does not name them.  So the chain can tell the live system's
writes from the rehearsal's.

THE RULE NOW.  Every change to the operator's knowledge is attributed
to a recorded invocation, or it is UNVERIFIED.  There is no third
answer, and no change is ever described as "accounted for" unless the
evidence establishes its origin:

    ATTRIBUTED   the substrate's origin chain names a recorded
                 invocation for THIS row (or a recorded call's
                 execution window covers the moment it changed).  The
                 invocation is named.

    UNVERIFIED   the origin cannot be established.  In a KNOWLEDGE
                 table this is a FAILURE — the gate cannot certify
                 that the rehearsal did not make it, and a gate that
                 cannot certify must not pass.

    CORRELATED   the movement is of a class the recorded invocations
                 produce, but the substrate binds those rows to no
                 invocation individually.  This is reported as
                 unverified-but-consistent.  It is NEVER called
                 "accounted for", and it is a KNOWN LIMITATION, stated
                 in the report rather than hidden.

The KNOWLEDGE tables are therefore strict; the bookkeeping tables are
reported with their limitation named.  And the rehearsal's own
independent syscall detector is what covers the case neither class can
prove on its own: a write the rehearsal made.  The two detectors are
reported separately, and the syscall detector's absence is reported as
an absence — never as "zero hits".

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
import time

VERSION = 3

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

# ── the attribution ────────────────────────────────────────────────────────
#
# The id column of each knowledge table.  `lessons` is a VIEW over
# `lessons_base`, and a view has no primary key, so the gate keys the
# knowledge rows by this column instead of by the view's empty primary
# key — which is what made the per-row delta of `lessons` meaningless.
DOMAIN_ID_COLUMN = {
    "memories": "id",
    "lessons": "id",
    "evidence": "id",
}

# The columns that record WHEN the row was written.  A mutation's own
# timestamp is what binds it to the recorded call that made it: the
# substrate stamps `unixepoch()` into the columns it sets, and a tool
# call runs between its own `started_at` and `completed_at`.
DOMAIN_TIME_COLUMNS = {
    "memories": ("created_at", "updated_at", "deleted_at"),
    "lessons": ("created", "updated_at", "deleted_at"),
    "evidence": ("created_at", "updated_at", "deleted_at"),
}

# The surfaces that can produce each kind of knowledge change.
#
# `mpm_memory` with action `delete` is the only writer that sets
# `memories.deleted_at`: the soft delete is one UPDATE that stops at
# `deleted_at` and does not re-record the artifact's provenance, so it
# is the one knowledge mutation the origin chain cannot bind on its own.
MEMORY_DELETE_SURFACE = ("mpm_memory", "delete")

# An explicitly delivered observation: the substrate records NO
# provenance for the evidence row itself (only the auto-capture rows,
# which a trigger creates inside the artifact's own transaction, carry
# a link).  The delivery is therefore bound to the recorded
# `mpm_evidence` call whose execution window covers it.
EVIDENCE_WRITE_SURFACE = ("mpm_evidence", "add")

# How far back the attribution index reaches.  The window the gate
# compares is minutes long; the index exists so that a provenance row
# naming an invocation can be resolved, and so that the recorded calls
# near the window can be bound to the mutations they made.
#
# The bound is a TIME window, not a count, and that matters: a bounded
# set that moves forward with the clock can only LOSE old entries, so
# "present in the later snapshot, absent from the earlier one" means
# exactly "new in the window" and never "aged out".
ATTRIBUTION_WINDOW_SECONDS = 6 * 3600


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


def domain_row_map(c: sqlite3.Connection, table: str) -> dict:
    """{row id: {"hash": h, "created": ts, "updated": ts, "deleted": ts}}.

    The hash is computed over exactly the columns the DOMAIN fingerprint
    uses — the volatile bookkeeping columns excluded — so this map and
    the fingerprint are the SAME instrument: a read that bumps
    `last_accessed_at` is not a change to the operator's knowledge.

    The map is keyed by the row's own id rather than by the table's
    primary key, because `lessons` is a VIEW: a view has no primary
    key, so the generic row map collapses the whole table into one
    entry and its per-row delta is meaningless.  Keying by the id is
    what makes "this row appeared" and "this row changed" answerable
    per row — which is the whole point of an attribution.

    The timestamps are the row's own record of when it was written.
    """
    cols, _ = table_columns(c, table)
    if not cols:
        return {}
    keep = [x for x in cols if x not in VOLATILE_COLUMNS]
    if not keep:
        return {}
    idcol = DOMAIN_ID_COLUMN.get(table)
    tcols = [x for x in DOMAIN_TIME_COLUMNS.get(table, ()) if x in cols]
    want = list(keep)
    for x in ([idcol] if idcol and idcol in cols else []) + tcols:
        if x not in want:
            want.append(x)
    sel = ", ".join(f'"{x}"' for x in want)
    try:
        rows = c.execute(f'SELECT {sel} FROM "{table}"').fetchall()
    except Exception as e:
        raise MeasurementError(
            f"cannot read the live table {table}: {e}"
        ) from e
    idx = {name: i for i, name in enumerate(want)}
    out: dict[str, dict] = {}
    for r in rows:
        h = hashlib.sha256(repr(tuple(r[idx[c]] for c in keep)).encode()).hexdigest()[:32]
        d: dict = {"hash": h}
        for x in tcols:
            v = r[idx[x]]
            if isinstance(v, (int, float)):
                d[x] = int(v)
        rid = r[idx[idcol]] if idcol in idx else None
        out[str(rid) if rid is not None else h] = d
    return out


def attribution_state(c: sqlite3.Connection) -> dict:
    """The origin chain, captured as the snapshot's attribution index.

    Four pieces, each read from the substrate's own tables:

      invocations   invocation_id -> the calls it covers.  A CALL is one
                    row of `tool_invocations`: the substrate records one
                    per tool invocation, and one invocation_id may
                    cover several calls (the OpenCode plugin stamps one
                    invocation id per host session).

      calls         the individual calls with the execution window
                    [started_at, completed_at] each one covers.  A
                    mutation's own timestamp falls inside the window of
                    the call that made it.

      origins       artifact_id -> the provenance row that records the
                    artifact's origin, with the key of that row so the
                    comparison can tell a provenance row that was
                    WRITTEN IN THIS WINDOW from one that predates it.

      evidence      evidence_id -> the row's own link to the artifact it
                    supports, and whether it was auto-captured (a
                    trigger wrote it inside the artifact's transaction)
                    or explicitly delivered.

      bindings      table -> row key -> invocation_id, for every table
                    that carries an invocation id of its own.  These
                    are the tables whose rows can be attributed
                    individually without the provenance chain.
    """
    state: dict = {"invocations": {}, "calls": {}, "origins": {},
                   "evidence": {}, "bindings": {}, "binding_tables": []}
    cutoff = int(time.time()) - ATTRIBUTION_WINDOW_SECONDS

    cols, _ = table_columns(c, "tool_invocations")
    if cols:
        want = [x for x in ("invocation_id", "tool_name", "action", "actor_kind",
                            "framework_name", "started_at", "completed_at",
                            "result_status") if x in cols]
        sel = ", ".join(f'"{x}"' for x in want)
        try:
            rows = c.execute(f'SELECT {sel} FROM "tool_invocations"').fetchall()
        except Exception as e:
            raise MeasurementError(
                f"cannot read the live audit trail: {e}") from e
        for r in rows:
            d = dict(zip(want, r))
            iid = d.get("invocation_id")
            started = _as_int(d.get("started_at"))
            if not iid or started is None or started < cutoff:
                continue
            completed = _as_int(d.get("completed_at"))
            inv = state["invocations"].setdefault(iid, {
                "calls": 0, "last": 0, "tools": [], "frameworks": [],
                "actors": [], "actions": [], "statuses": []})
            inv["calls"] += 1
            inv["last"] = max(inv["last"], completed or started)
            for key, dest in (("tool_name", "tools"), ("framework_name", "frameworks"),
                              ("actor_kind", "actors"), ("action", "actions"),
                              ("result_status", "statuses")):
                v = d.get(key)
                if v is not None and v not in inv[dest]:
                    inv[dest].append(v)
            state["calls"].setdefault(iid, []).append([
                started, completed if completed is not None else started,
                d.get("tool_name"), d.get("action"), d.get("framework_name"),
                d.get("actor_kind"), d.get("result_status")])

    cols, pk = table_columns(c, "artifact_provenance")
    if cols:
        want = [x for x in ("artifact_id", "artifact_type", "invocation_id") if x in cols]
        # The primary key travels with the row so that the comparison
        # can tell a provenance row written in this window from one that
        # predates it.  The key is in the SAME form the generic row map
        # uses, so the two maps can be compared directly.
        pkcols = [cols[i] for i in (pk or [])]
        sel = ", ".join(f'"{x}"' for x in (want + [x for x in pkcols if x not in want]))
        try:
            rows = c.execute(f'SELECT {sel} FROM "artifact_provenance"').fetchall()
        except Exception as e:
            raise MeasurementError(
                f"cannot read the live provenance ledger: {e}") from e
        names = want + [x for x in pkcols if x not in want]
        for r in rows:
            d = dict(zip(names, r))
            aid = d.get("artifact_id")
            if not aid:
                continue
            o = {"invocation_id": d.get("invocation_id"),
                 "artifact_type": d.get("artifact_type")}
            if pkcols:
                o["key"] = "|".join(repr(d.get(x)) for x in pkcols)
            state["origins"][str(aid)] = o

    cols, _ = table_columns(c, "evidence")
    if cols:
        want = [x for x in ("id", "artifact_id", "source_group", "created_by",
                            "created_at") if x in cols]
        sel = ", ".join(f'"{x}"' for x in want)
        try:
            rows = c.execute(f'SELECT {sel} FROM "evidence"').fetchall()
        except Exception as e:
            raise MeasurementError(
                f"cannot read the live evidence table: {e}") from e
        for r in rows:
            d = dict(zip(want, r))
            if d.get("id") is None:
                continue
            state["evidence"][str(d["id"])] = {
                "artifact_id": d.get("artifact_id"),
                "source_group": d.get("source_group"),
                "created_by": d.get("created_by"),
                "created_at": _as_int(d.get("created_at")),
            }

    # Every table that carries an invocation id of its own can bind its
    # rows to a recorded invocation without the provenance chain.
    try:
        names = [r[0] for r in c.execute(
            "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")]
    except Exception as e:
        raise MeasurementError(
            f"cannot enumerate the live database's tables: {e}") from e
    for t in names:
        if t.startswith("sqlite_") or t in AUDIT_TABLES:
            continue
        cols, pk = table_columns(c, t)
        if not cols or "invocation_id" not in cols or not pk:
            continue
        sel = ", ".join(f'"{x}"' for x in cols)
        try:
            rows = c.execute(f'SELECT {sel} FROM "{t}"').fetchall()
        except Exception as e:
            raise MeasurementError(
                f"cannot read the live table {t}: {e}") from e
        ii = cols.index("invocation_id")
        binds: dict[str, str] = {}
        for r in rows:
            iid = r[ii]
            if not iid:
                continue
            key = "|".join(repr(r[i]) for i in pk if i < len(r))
            binds[key] = iid
        state["bindings"][t] = binds
        state["binding_tables"].append(t)

    return state


def _as_int(v):
    """An int, or None.  SQLite columns are not typed by the schema."""
    if isinstance(v, bool):
        return None
    if isinstance(v, int):
        return v
    if isinstance(v, float):
        return int(v)
    try:
        return int(str(v))
    except (TypeError, ValueError):
        return None


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
                   "domain": {}, "schema": {}, "config": {}, "domain_rows": {},
                   "attribution": {}}
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
            state["domain_rows"][t] = domain_row_map(c, t)
        state["config"] = config_state(c)
        state["attribution"] = attribution_state(c)
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
        # WHEN the snapshot was taken.  The attribution needs it: a
        # recorded invocation counts as active in the window when its
        # last activity is at or after the earlier snapshot's moment.
        "at": int(time.time()),
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


def row_delta_keys(before: dict, after: dict) -> dict:
    """{table: {"added": [...], "changed": [...], "removed": [...]}}.

    The same delta as `row_delta`, but with the KEYS.  A count cannot be
    attributed; a key can.
    """
    out = {}
    b = before or {}
    a = after or {}
    for t in sorted(set(b) | set(a)):
        B = b.get(t) or {}
        A = a.get(t) or {}
        added = sorted(set(A) - set(B))
        removed = sorted(set(B) - set(A))
        changed = sorted(k for k in set(A) & set(B) if A[k] != B[k])
        if added or removed or changed:
            out[t] = {"added": added, "changed": changed, "removed": removed}
    return out


class Attribution:
    """The window's evidence about WHO WROTE WHAT — and where it stops.

    The substrate records the origin of every artifact it writes:

        artifact_provenance.artifact_id -> invocation_id
                                        -> tool_invocations.invocation_id

    This class follows that chain, and it answers three questions about
    the changes the two snapshots describe:

      * is there a recorded invocation behind this change, and which one?
      * was that invocation ACTIVE inside this window?
      * what can be said when the answer is no?

    The answers are deliberately not symmetric.  A knowledge change with
    no established origin is a FAILURE: the gate cannot certify that the
    rehearsal did not make it, and a gate that cannot certify must not
    pass.  A bookkeeping change with no per-row origin is CORRELATED:
    the substrate writes those rows as a side effect of an invocation
    and binds no individual row to one, so the gate reports the
    limitation instead of pretending to an attribution it does not have.
    """

    def __init__(self, before: dict, after: dict):
        self.bdb = before.get("db") or {}
        self.adb = after.get("db") or {}
        self.b = self.bdb.get("attribution") or {}
        self.a = self.adb.get("attribution") or {}
        self.before_at = _as_int(before.get("at")) or 0
        self.after_at = _as_int(after.get("at")) or 0
        self.b_rows = self.bdb.get("rows") or {}
        self.a_rows = self.adb.get("rows") or {}
        self.b_dom = self.bdb.get("domain_rows") or {}
        self.a_dom = self.adb.get("domain_rows") or {}
        self.b_inv = self.b.get("invocations") or {}
        self.a_inv = self.a.get("invocations") or {}

    # ── the recorded invocations ─────────────────────────────────────────

    def active_invocations(self) -> dict:
        """The invocations that did something inside this window.

        An invocation is active when its last recorded activity is at
        or after the earlier snapshot.  A time-bounded index can only
        lose old entries as the clock moves forward, so an invocation
        that is present in the later snapshot and absent from the
        earlier one is genuinely new — never one that merely aged out.
        """
        out = {}
        for iid, inv in self.a_inv.items():
            if _as_int(inv.get("last")) is not None and _as_int(inv.get("last")) >= self.before_at:
                out[iid] = inv
        return out

    def recorded_invocation(self, iid) -> bool:
        """Is this invocation id one the live audit trail records?"""
        return bool(iid) and iid in self.a_inv

    # ── the origin chain ─────────────────────────────────────────────────

    def provenance_written_in_window(self, artifact_id) -> bool:
        """Did the substrate record this artifact's origin IN THE WINDOW?

        The provenance row is written in the same transaction as the
        artifact it describes.  So a provenance row that appeared or was
        rewritten inside the window is the substrate's own record that
        IT wrote the artifact inside the window.
        """
        o = (self.a.get("origins") or {}).get(str(artifact_id))
        if not o:
            return False
        key = o.get("key")
        if not key:
            return False
        b = (self.b_rows.get("artifact_provenance") or {}).get(key)
        a = (self.a_rows.get("artifact_provenance") or {}).get(key)
        if a is None:
            return False
        if b is None:
            return True          # the provenance row is NEW
        return b != a            # or it was rewritten in place

    def origin_of(self, artifact_id):
        """(invocation_id, why) for an artifact's recorded origin."""
        o = (self.a.get("origins") or {}).get(str(artifact_id))
        if not o:
            return None, "the substrate recorded no provenance for it"
        iid = o.get("invocation_id")
        if not self.recorded_invocation(iid):
            return None, (f"its provenance names invocation {iid}, which the "
                          "live audit trail does not record")
        return iid, f"invocation {iid}"

    # ── the per-row attribution ──────────────────────────────────────────

    def attribute_by_containment(self, ts, surface):
        """(invocation_id, why) for a mutation bound to a call's window.

        The substrate stamps `unixepoch()` into the columns it sets, and
        a tool call runs between its own `started_at` and `completed_at`.
        A mutation whose own timestamp falls inside the execution window
        of exactly one recorded call of the surface that can produce it
        was made by that call.

        EXACTLY one.  Two candidate calls means the origin is not
        established: the gate says so rather than picking one.
        """
        tool, action = surface
        if ts is None:
            return None, "the row carries no timestamp that could bind it to a call"
        candidates = []
        for iid, calls in (self.a.get("calls") or {}).items():
            for call in calls:
                started, completed = call[0], call[1]
                if call[2] != tool:
                    continue
                if action is not None and call[3] != action:
                    continue
                if started is None or completed is None:
                    continue
                if started <= ts <= completed:
                    candidates.append((iid, call))
        if not candidates:
            return None, f"no recorded {tool} call covers the moment it changed"
        if len(candidates) > 1:
            return None, (f"{len(candidates)} recorded {tool} calls cover the "
                          "moment it changed, so its origin is not established")
        iid, call = candidates[0]
        return iid, (f"the recorded {tool}/{call[3]} call at {call[0]} covers "
                     "the moment it changed")

    def attribute_added(self, table: str, key: str):
        """(invocation_id, why) for a knowledge row that APPEARED."""
        if table == "evidence":
            link = (self.a.get("evidence") or {}).get(key) or {}
            if link.get("source_group") == "auto_capture":
                # The auto-capture row is written by an AFTER INSERT
                # trigger on `memories`, INSIDE the artifact's own
                # transaction.  Its origin is therefore its parent's
                # origin — not a correlation, a shared transaction.
                parent = link.get("artifact_id")
                iid, why = self.origin_of(parent)
                if iid and self.provenance_written_in_window(parent):
                    return iid, (f"the auto-capture row was written in the same "
                                 f"transaction as {parent} ({why})")
                return None, (f"its auto-capture parent {parent} was not itself "
                              "written in this window"
                              if iid else
                              f"its auto-capture parent {parent} has no recorded origin")
            # An explicitly delivered observation.  The substrate records
            # no provenance for the evidence row itself, so the origin is
            # bound to the recorded `mpm_evidence` call whose execution
            # window covers the delivery.
            return self.attribute_by_containment(link.get("created_at"),
                                                  EVIDENCE_WRITE_SURFACE)
        iid, why = self.origin_of(key)
        if iid and self.provenance_written_in_window(key):
            return iid, f"the substrate recorded its origin in this window ({why})"
        if iid:
            return None, ("its provenance predates this window, so the write that "
                          "added it was not the substrate's own")
        return None, why

    def attribute_changed(self, table: str, key: str):
        """(invocation_id, why) for a knowledge row that CHANGED."""
        B = (self.b_dom.get(table) or {}).get(key) or {}
        A = (self.a_dom.get(table) or {}).get(key) or {}
        iid, why = self.origin_of(key)
        if iid and self.provenance_written_in_window(key):
            return iid, f"the substrate re-recorded its origin in this window ({why})"
        # The soft delete is the one knowledge mutation the origin chain
        # cannot bind on its own: the substrate sets `deleted_at` and
        # stops there, without re-recording the artifact's provenance.
        b_del, a_del = B.get("deleted"), A.get("deleted")
        if a_del is not None and a_del != b_del:
            return self.attribute_by_containment(a_del, MEMORY_DELETE_SURFACE)
        # A write that bumped `updated_at` is bound by that timestamp.
        b_upd, a_upd = B.get("updated"), A.get("updated")
        if a_upd is not None and a_upd != b_upd:
            return self.attribute_by_containment(a_upd, ("mpm_memory", None))
        return None, ("its provenance predates this window and no recorded call "
                      "covers the moment it changed")

    def domain_delta(self, table: str):
        """(added, changed, removed) row ids for one knowledge table."""
        B = (self.b_dom.get(table) or {})
        A = (self.a_dom.get(table) or {})
        added = sorted(k for k in A if k not in B)
        removed = sorted(k for k in B if k not in A)
        changed = sorted(k for k in A if k in B and A[k].get("hash") != B[k].get("hash"))
        return added, changed, removed

    def attribute_bookkeeping(self, table: str, moved_keys):
        """(attributed, unverified) for the rows of a bookkeeping table.

        A table that carries its own `invocation_id` binds each of its
        rows to the invocation that wrote it, so the rows can be
        attributed individually.  A table that does not is a SIDE-EFFECT
        table: the substrate writes it as a consequence of an invocation
        and binds no individual row to one.  That is reported as a
        correlation with its limitation named — never as an attribution.
        """
        if table not in (self.a.get("binding_tables") or []):
            return [], []
        binds = (self.a.get("bindings") or {}).get(table) or {}
        attributed = []
        unverified = []
        for key in moved_keys:
            iid = binds.get(key)
            if iid and self.recorded_invocation(iid):
                attributed.append((key, iid))
            elif iid:
                unverified.append((key, f"it names invocation {iid}, which the live "
                                        "audit trail does not record"))
            else:
                unverified.append((key, "no recorded invocation is bound to it"))
        return attributed, unverified


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

    # The KNOWLEDGE tables.  Every change is attributed to a recorded
    # invocation or it is UNVERIFIED — and an UNVERIFIED change in a
    # knowledge table is a FAILURE.  There is no "the audit trail moved,
    # so it must have been the live system": that is the false claim
    # this rule used to make.
    attr = Attribution(before, after)
    delta = row_delta(b_rows, a_rows)
    delta_keys = row_delta_keys(b_rows, a_rows)

    knowledge_moved = {}
    for t in DOMAIN_TABLES:
        added, changed, removed = attr.domain_delta(t)
        if added or changed or removed:
            knowledge_moved[t] = (added, changed, removed)
    # The domain fingerprint is the substrate's own definition; it is
    # the thing the SIMULATED database is compared with.
    bdom = bdb.get("domain") or {}
    adom = adb.get("domain") or {}
    domain_moved = [t for t in sorted(set(bdom) | set(adom))
                    if bdom.get(t) != adom.get(t)]
    for t in domain_moved:
        knowledge_moved.setdefault(t, ([], [], []))

    for t in sorted(knowledge_moved):
        added, changed, removed = knowledge_moved[t]
        for key in added:
            origin, why = attr.attribute_added(t, key)
            if origin:
                evidence.append(
                    f"the operator's knowledge moved in {t}: the row {key} "
                    f"appeared, and {why} — ATTRIBUTED"
                )
            else:
                failures.append(
                    f"the operator's knowledge changed and its origin is "
                    f"UNVERIFIED: the row {key} appeared in {t}, and {why}"
                )
        for key in changed:
            origin, why = attr.attribute_changed(t, key)
            if origin:
                evidence.append(
                    f"the operator's knowledge moved in {t}: the row {key} "
                    f"changed, and {why} — ATTRIBUTED"
                )
            else:
                failures.append(
                    f"the operator's knowledge changed and its origin is "
                    f"UNVERIFIED: the row {key} changed in {t}, and {why}"
                )
        for key in removed:
            failures.append(
                f"the operator's knowledge changed and its origin is "
                f"UNVERIFIED: the row {key} DISAPPEARED from {t}, and the "
                "substrate records no origin for a removal"
            )

    # Everything else is the live system's bookkeeping.
    active = attr.active_invocations()
    for t, (added, changed, removed) in sorted(delta.items()):
        if t in DOMAIN_TABLES or t in AUDIT_TABLES:
            continue
        if t == "system_config":
            # The CONFIGURATION class above already ruled on this
            # table, key by key.  It is not also bookkeeping.
            continue
        line = (f"{t}: rows +{added} ~{changed} -{removed}")
        keys = delta_keys.get(t) or {}
        moved = list(keys.get("added") or []) + list(keys.get("changed") or [])
        attributed, unverified = attr.attribute_bookkeeping(t, moved)
        if unverified:
            for key, why in unverified:
                failures.append(
                    f"the live database changed and the origin is UNVERIFIED: "
                    f"the row {key} of {t} moved, and {why}"
                )
            continue
        if attributed:
            named = sorted({iid for _, iid in attributed})
            evidence.append(
                f"the live system's own bookkeeping moved — {line} — "
                f"ATTRIBUTED to {len(named)} recorded invocation(s): "
                f"{', '.join(named[:4])}"
            )
            continue
        # A side-effect table: the substrate writes it as a consequence
        # of an invocation and binds no individual row to one.  That is
        # a CORRELATION, and it is reported as one — with the
        # limitation named, and never as "accounted for".
        if active:
            evidence.append(
                f"the live system's own bookkeeping moved — {line} — "
                f"CORRELATED with {len(active)} recorded invocation(s); the "
                "substrate binds these rows to no invocation individually, so "
                "this is NOT an attribution"
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
