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
is exactly how a REAL change gets missed.

THE CLASSIFICATION
------------------
The live installation is a LIVE thing.  Other writers exist — the
scheduler above all — so "is it byte-identical?" is the wrong question
for the database.  The right question is per-ARTIFACT:

  PROTECTED    The artifacts that genuinely must not change, and whose
               BYTES are the thing that matters: the installed binaries,
               the host assets, the systemd unit, the OpenCode
               configuration, and the managed instructions.  These are
               compared by CONTENT (path set, mode, sha256).  Their
               mtime is deliberately NOT part of the comparison: mtime
               records when a file was written, not what it contains.

  DOMAIN      The operator's KNOWLEDGE inside the live database — the
               `memories`, `lessons` and `evidence` content, with the
               volatile bookkeeping columns excluded.  This is the
               substrate's own established definition of "the operator's
               knowledge" (state_fingerprint.py --content), so the live
               database is measured by exactly the same instrument as the
               simulated one.  A change here is a REAL finding: the
               rehearsal must not write the operator's knowledge.

  OPERATIONAL The tables the live system maintains for its own
               bookkeeping.  These are allowed to move, and the gate
               REPORTS the movement with the delta as the EVIDENCE that
               separates "the scheduler was busy" from "something wrote
               the operator's knowledge".

EVERYTHING ELSE in the database is reported as evidence and is NOT
asserted, because the rehearsal's own process tree is separately proven
not to have opened the live database at all (the syscall detector), and
because the live binary may legitimately migrate the live database
while the rehearsal runs.

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

VERSION = 1

# ── the protected artifacts ────────────────────────────────────────────────
#
# These are the artifacts whose CONTENT must not change during the
# rehearsal.  The live DATABASE is deliberately NOT among them: the live
# scheduler writes to it, so its bytes move for reasons that have
# nothing to do with the rehearsal.  It is measured by class instead.
PROTECTED_DIRS = ("bin", "agent_installation")
PROTECTED_FILES = (
    "systemd/mpm-scheduler.service",
    "opencode/opencode.jsonc",
    "opencode/AGENTS.md",
)

# ── the operational tables ─────────────────────────────────────────────────
#
# MEASURED, not guessed.  With the production scheduler running against
# the live database, these are the only two tables that moved over a
# two-minute observation window (the audit trail grew by 55 rows; the
# scheduler's liveness counter was rewritten).  Every other table was
# byte-identical.
#
# A table that changes and is NOT in this set is reported as evidence
# outside the expected set — see report_operational().
OPERATIONAL_TABLES = {
    "tool_invocations": "the audit trail — the live plugin's polling and the scheduler's health checks",
    "system_config": "the scheduler's liveness counter (scheduler_active_uptime)",
}

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
        for dirpath, dirnames, filenames in os.walk(root):
            dirnames.sort()
            for name in sorted(filenames):
                p = os.path.join(dirpath, name)
                try:
                    st = os.stat(p)
                    inv[p] = {
                        "mode": oct(st.st_mode & 0o7777),
                        "size": st.st_size,
                        "sha256": sha256_file(p),
                    }
                except OSError as e:
                    raise MeasurementError(f"cannot read the protected file {p}: {e}") from e
    for rel in PROTECTED_FILES:
        if rel.startswith("systemd/"):
            p = os.path.join(systemd_user, rel[len("systemd/"):])
        elif rel.startswith("opencode/"):
            p = os.path.join(oc_config, rel[len("opencode/"):])
        else:
            p = os.path.join(live_mpm, rel)
        if not os.path.exists(p):
            inv[p] = {"absent": True}
            continue
        try:
            st = os.stat(p)
            inv[p] = {
                "mode": oct(st.st_mode & 0o7777),
                "size": st.st_size,
                "sha256": sha256_file(p),
            }
        except OSError as e:
            raise MeasurementError(f"cannot read the protected file {p}: {e}") from e
    return inv


def table_fingerprint(c: sqlite3.Connection, table: str, drop_volatile: bool):
    """(sha256, row count) for one table, or None when it does not exist."""
    try:
        cols = [r[1] for r in c.execute(f'PRAGMA table_info("{table}")')]
    except Exception:
        return None
    if not cols:
        return None
    if drop_volatile:
        cols = [x for x in cols if x not in VOLATILE_COLUMNS]
    if not cols:
        return None
    sel = ", ".join(f'"{x}"' for x in cols)
    try:
        rows = c.execute(f'SELECT {sel} FROM "{table}"').fetchall()
    except Exception:
        return None
    h = hashlib.sha256()
    h.update(f"{table}:{len(rows)}\n".encode())
    for r in sorted(repr(x) for x in rows):
        h.update(r.encode())
        h.update(b"\x00")
    return h.hexdigest(), len(rows)


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

    state: dict = {"present": True, "path": db_path, "tables": {}, "domain": {},
                   "schema": {}}
    try:
        names = [r[0] for r in c.execute(
            "SELECT name FROM sqlite_master WHERE type IN ('table','view') ORDER BY name")]
        for t in names:
            if t.startswith("sqlite_"):
                continue
            cols = [r[1] for r in c.execute(f'PRAGMA table_info("{t}")')]
            if cols:
                state["schema"][t] = cols
            fp = table_fingerprint(c, t, drop_volatile=(t in DOMAIN_TABLES))
            if fp is not None:
                state["tables"][t] = {"hash": fp[0], "rows": fp[1]}
        for t in DOMAIN_TABLES:
            fp = table_fingerprint(c, t, drop_volatile=True)
            if fp is None:
                # The table does not exist in this schema.  That is a
                # measurement of the schema, not of the content.
                state["domain"][t] = None
            else:
                state["domain"][t] = fp[0]
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


def compare_domain(before: dict, after: dict) -> list[str]:
    """The domain tables whose content changed."""
    problems = []
    b = before.get("domain") or {}
    a = after.get("domain") or {}
    for t in sorted(set(b) | set(a)):
        if b.get(t) != a.get(t):
            problems.append(
                f"{t}: the operator's knowledge changed "
                f"({b.get(t)} -> {a.get(t)})"
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


def report_operational(before: dict, after: dict) -> tuple[list[str], list[str]]:
    """(expected operational movement, movement outside the expected set).

    The first list is the EVIDENCE that separates "the live system was
    busy" from "something wrote the operator's knowledge": it names every
    table that moved as ordinary activity, with the delta.
    """
    expected = []
    unexpected = []
    bt = before.get("tables") or {}
    at = after.get("tables") or {}
    for t in sorted(set(bt) | set(at)):
        b = bt.get(t)
        a = at.get(t)
        if b == a:
            continue
        brows = b.get("rows") if b else None
        arows = a.get("rows") if a else None
        delta = (arows - brows) if (brows is not None and arows is not None) else None
        if t in OPERATIONAL_TABLES:
            expected.append(
                f"{t}: rows {brows} -> {arows}"
                + (f" ({delta:+d})" if delta is not None else "")
                + f"  [{OPERATIONAL_TABLES[t]}]"
            )
        else:
            unexpected.append(
                f"{t}: rows {brows} -> {arows}"
                + (f" ({delta:+d})" if delta is not None else "")
            )
    return expected, unexpected


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

    failures = []
    evidence = []

    protected = compare_protected(before.get("protected") or {},
                                  after.get("protected") or {})
    for p in protected:
        failures.append(f"a protected artifact changed: {p}")

    schema = compare_schema(before.get("db") or {}, after.get("db") or {})
    for s in schema:
        failures.append(f"the live database's schema changed: {s}")

    domain = compare_domain(before.get("db") or {}, after.get("db") or {})
    for d in domain:
        failures.append(f"the live database's domain state changed: {d}")

    expected, unexpected = report_operational(before.get("db") or {},
                                               after.get("db") or {})
    for u in unexpected:
        evidence.append(f"a table OUTSIDE the expected operational set moved: {u}")

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
            f"mtime {int(bf.get('mtime', 0))} -> {int(af.get('mtime', 0))}) "
            "— expected: the live scheduler records health checks and SQLite "
            "checkpoints the WAL. The file's bytes are NOT the assertion; the "
            "domain state above is."
        )

    if not args.quiet:
        print("LIVE-GATE: the live installation's isolation")
        print(f"  protected artifacts checked : {len(before.get('protected') or {})}")
        print(f"  protected artifacts changed : {len(protected)}")
        print(f"  domain tables checked       : {len((before.get('db') or {}).get('domain') or {})}")
        print(f"  domain tables changed       : {len(domain)}")
        if expected:
            print("  EXPECTED operational movement (the live system's own bookkeeping):")
            for e in expected:
                print(f"    - {e}")
        if evidence:
            print("  other movement observed (reported, not asserted):")
            for e in evidence:
                print(f"    - {e}")
        if not expected and not evidence:
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
