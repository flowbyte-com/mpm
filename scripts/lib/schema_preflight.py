#!/usr/bin/env python3
"""schema_preflight.py — refuse a schema COLLISION before it becomes a
silent incompatibility.

THE PROBLEM
-----------

The substrate creates its tables with ``CREATE TABLE IF NOT EXISTS``.
SQLite's rule for that statement is not "create the table if it is
missing" — it is "create the table if it is missing, and otherwise leave
whatever is there ALONE".

So a database that already contains a table named ``lifecycle_events``
which is NOT the substrate's ``lifecycle_events`` keeps that unrelated
table forever. The substrate's own schema is never applied. The
substrate then writes to it with the columns it expects, and fails at
some later, unrelated moment with an error that names a column, not the
collision that caused it.

This was found by the runtime-migration rehearsal (case S5): an
unrelated table named ``lifecycle_events`` survives the upgrade, and the
upgrade reports success.

THE RULE
--------

Before a new binary is allowed near a live database, the shape of every
table the substrate owns is compared against the shape the substrate
itself would create. A table that is ABSENT is fine — the substrate
will create it. A table whose shape MATCHES is fine — it is the
substrate's own table. A table whose shape DIFFERS is a COLLISION, and
the preflight refuses.

Nothing here opens the live database with an MPM binary. The live
database is read through SQLite's online backup API into a scratch file,
and the comparison is made against the scratch copy. A newer MPM binary
would apply pending migrations the moment it opened the database, so
"read the live database" must never mean "run the new binary against
it".

TWO SUBCOMMANDS
----------------

    extract   pull one table's DDL out of the Go schema source, so the
              authority travels with the bundle instead of requiring a
              source checkout at install time.

    check     compare a live database's table against that DDL.

EXIT CODES
----------

    0   compatible (or the table is absent)
    1   COLLISION — the shapes differ
    2   usage or I/O error
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sqlite3
import sys
import tempfile

# --------------------------------------------------------------------------
# Go source extraction
# --------------------------------------------------------------------------


def extract_go_ddl(go_path: str, table: str) -> str:
    """Return the substrate's DDL for `table` from a Go source file.

    The DDL lives in a Go raw string literal (backtick-delimited). A raw
    string cannot contain a backtick, so the literals can be found by
    pairing backticks in order. Only a literal that BEGINS with the
    table's CREATE statement is considered, so identifiers and prose
    elsewhere in the file are not mistaken for DDL.

    A DDL built by concatenating Go expressions (the substrate does this
    for one table, to inline a generated CHECK vocabulary) is NOT
    self-contained and is therefore not extractable. The caller is told
    so explicitly rather than being handed a fragment that would not
    apply.
    """
    with open(go_path, "r", encoding="utf-8") as fh:
        src = fh.read()

    prefix = f"CREATE TABLE IF NOT EXISTS {table}"
    spans: list[str] = []
    i = 0
    while True:
        a = src.find("`", i)
        if a < 0:
            break
        b = src.find("`", a + 1)
        if b < 0:
            break
        spans.append(src[a + 1 : b])
        i = b + 1

    candidates = [s for s in spans if s.lstrip().upper().startswith(prefix.upper())]
    if not candidates:
        raise LookupError(
            f"no DDL for table {table!r} in {go_path} "
            f"(looked for a raw string starting with {prefix!r})"
        )
    if len(candidates) > 1:
        raise LookupError(
            f"ambiguous DDL for table {table!r} in {go_path}: {len(candidates)} candidates"
        )
    ddl = candidates[0]
    if "`" in ddl:
        raise LookupError(
            f"the DDL for table {table!r} in {go_path} is assembled from Go "
            f"string concatenation and cannot be extracted as a single statement"
        )
    if not ddl.rstrip().endswith(";"):
        raise LookupError(
            f"the DDL for table {table!r} in {go_path} does not end with ';' "
            f"— it is probably assembled from more than one literal"
        )
    return ddl


# --------------------------------------------------------------------------
# Shape extraction
# --------------------------------------------------------------------------

_COMMENT_BLOCK = re.compile(r"/\*.*?\*/", re.DOTALL)
_COMMENT_LINE = re.compile(r"--[^\n]*")


def strip_sql_comments(sql: str) -> str:
    """Remove SQL comments.

    Both the reference DDL and the live table's stored CREATE statement
    carry comments, and the comments themselves mention CHECK
    constraints. Comparing the comment-bearing texts would compare prose.
    """
    sql = _COMMENT_BLOCK.sub(" ", sql)
    sql = _COMMENT_LINE.sub(" ", sql)
    return sql


def extract_checks(sql: str) -> list[str]:
    """Return the CHECK constraint expressions of a CREATE TABLE, sorted.

    The scan is parenthesis-balanced, because the substrate's CHECKs
    contain nested parentheses (``CHECK (x IN ('a','b'))``) and a naive
    regex would truncate them. The result is normalised so that two
    texts differing only in quoting or whitespace compare equal.
    """
    sql = strip_sql_comments(sql)
    upper = sql.upper()
    out: list[str] = []
    i = 0
    while True:
        j = upper.find("CHECK", i)
        if j < 0:
            break
        k = j + len("CHECK")
        while k < len(sql) and sql[k].isspace():
            k += 1
        if k >= len(sql) or sql[k] != "(":
            i = j + len("CHECK")
            continue
        depth = 0
        m = k
        quote = None
        while m < len(sql):
            c = sql[m]
            if quote is not None:
                if c == quote:
                    quote = None
            elif c in "'\"`":
                quote = c
            elif c == "(":
                depth += 1
            elif c == ")":
                depth -= 1
                if depth == 0:
                    break
            m += 1
        if m >= len(sql):
            # Unbalanced — take the rest of the text.
            out.append(sql[j:])
            break
        out.append(sql[j : m + 1])
        i = m + 1
    return sorted(normalise_sql(c) for c in out)


def normalise_sql(text: str) -> str:
    """Collapse whitespace and unify identifier/quote spelling."""
    text = text.replace('"', "'").replace("`", "'")
    return re.sub(r"\s+", " ", text).strip()


def table_shape(db: sqlite3.Connection, table: str) -> dict | None:
    """Return the canonical shape of `table`, or None if it does not exist."""
    row = db.execute(
        "SELECT name FROM sqlite_master WHERE type='table' AND name=?", (table,)
    ).fetchone()
    if row is None:
        return None
    cols = []
    for r in db.execute(f'PRAGMA table_info("{table}")'):
        cols.append(
            {
                "name": r[1],
                "type": (r[2] or "").upper(),
                "notnull": int(r[3]),
                "default": r[4],
            }
        )
    ddl_row = db.execute(
        "SELECT sql FROM sqlite_master WHERE type='table' AND name=?", (table,)
    ).fetchone()
    ddl = ddl_row[0] if ddl_row and ddl_row[0] else ""
    idx = sorted(
        r[0]
        for r in db.execute(
            "SELECT name FROM sqlite_master WHERE type='index' AND tbl_name=? "
            "AND sql IS NOT NULL",
            (table,),
        )
    )
    trg = sorted(
        r[0]
        for r in db.execute(
            "SELECT name FROM sqlite_master WHERE type='trigger' AND tbl_name=?", (table,)
        )
    )
    return {
        "columns": cols,
        "checks": extract_checks(ddl),
        "indexes": idx,
        "triggers": trg,
    }


def shape_diff(reference: dict, live: dict) -> list[str]:
    """Return human-readable differences between two shapes."""
    diffs: list[str] = []

    ref_cols = reference["columns"]
    live_cols = live["columns"]
    ref_names = [c["name"] for c in ref_cols]
    live_names = [c["name"] for c in live_cols]

    if ref_names != live_names:
        missing = [n for n in ref_names if n not in live_names]
        extra = [n for n in live_names if n not in ref_names]
        if missing:
            diffs.append(f"columns the substrate expects but the table does not have: {missing}")
        if extra:
            diffs.append(f"columns the table has that the substrate does not know: {extra}")
        if not missing and not extra:
            diffs.append(f"columns are in a different order: {live_names} vs {ref_names}")

    live_by_name = {c["name"]: c for c in live_cols}
    for ref in ref_cols:
        got = live_by_name.get(ref["name"])
        if got is None:
            continue
        if ref["type"] != got["type"]:
            diffs.append(f"column {ref['name']}: type {got['type']!r} vs expected {ref['type']!r}")
        if ref["notnull"] != got["notnull"]:
            diffs.append(
                f"column {ref['name']}: NOT NULL is {'set' if got['notnull'] else 'unset'} "
                f"but the substrate declares it {'set' if ref['notnull'] else 'unset'}"
            )
        rd = ref["default"]
        gd = got["default"]
        if (rd or "") != (gd or ""):
            diffs.append(
                f"column {ref['name']}: default {gd!r} vs expected {rd!r}"
            )

    ref_checks = reference["checks"]
    live_checks = live["checks"]
    if ref_checks != live_checks:
        only_ref = [c for c in ref_checks if c not in live_checks]
        only_live = [c for c in live_checks if c not in ref_checks]
        for c in only_ref:
            diffs.append(f"CHECK constraint the substrate requires but the table does not have: {c}")
        for c in only_live:
            diffs.append(f"CHECK constraint the table has that the substrate does not declare: {c}")

    return diffs


# --------------------------------------------------------------------------
# The live database is read through a SNAPSHOT
# --------------------------------------------------------------------------


def snapshot(live_db: str, scratch_dir: str) -> str:
    """Copy the live database into `scratch_dir` via the online backup API.

    A file copy of a WAL-mode database is not a consistent snapshot, and
    opening the live file read-write would let SQLite create WAL/SHM
    sidecars in it. The backup API gives a consistent copy and never
    writes to the source.
    """
    dest = os.path.join(scratch_dir, "live-snapshot.db")
    src = sqlite3.connect(f"file:{live_db}?mode=ro", uri=True)
    try:
        dst = sqlite3.connect(dest)
        try:
            src.backup(dst)
        finally:
            dst.close()
    finally:
        src.close()
    return dest


def build_reference(ddl: str, scratch_dir: str) -> str:
    """Apply the substrate's DDL to an empty scratch database."""
    dest = os.path.join(scratch_dir, "reference.db")
    conn = sqlite3.connect(dest)
    try:
        conn.executescript(ddl)
        conn.commit()
    finally:
        conn.close()
    return dest


# --------------------------------------------------------------------------
# Subcommands
# --------------------------------------------------------------------------


def cmd_extract(args: argparse.Namespace) -> int:
    ddl = extract_go_ddl(args.go, args.table)
    out = args.out
    if out == "-":
        sys.stdout.write(ddl)
        return 0
    os.makedirs(os.path.dirname(os.path.abspath(out)) or ".", exist_ok=True)
    with open(out, "w", encoding="utf-8") as fh:
        fh.write(ddl)
    return 0


def cmd_check(args: argparse.Namespace) -> int:
    if not os.path.exists(args.db):
        print(f"schema-preflight: no database at {args.db}", file=sys.stderr)
        return 0
    if not os.path.exists(args.ddl):
        print(f"schema-preflight: no reference DDL at {args.ddl}", file=sys.stderr)
        return 2

    with open(args.ddl, "r", encoding="utf-8") as fh:
        ddl = fh.read()

    with tempfile.TemporaryDirectory(prefix="mpm-schema-preflight.") as scratch:
        ref_path = build_reference(ddl, scratch)
        ref_conn = sqlite3.connect(f"file:{ref_path}?mode=ro", uri=True)
        reference = table_shape(ref_conn, args.table)
        ref_conn.close()
        if reference is None:
            print(
                f"schema-preflight: the reference DDL does not create {args.table}",
                file=sys.stderr,
            )
            return 2

        snap_path = snapshot(args.db, scratch)
        live_conn = sqlite3.connect(f"file:{snap_path}?mode=ro", uri=True)
        live = table_shape(live_conn, args.table)
        live_conn.close()

    if live is None:
        result = {
            "status": "absent",
            "table": args.table,
            "detail": "the live database does not have this table; the substrate will create it",
        }
        _emit(result, args.json)
        return 0

    diffs = shape_diff(reference, live)
    if not diffs:
        result = {
            "status": "compatible",
            "table": args.table,
            "detail": "the live table's shape matches the substrate's own DDL",
        }
        _emit(result, args.json)
        return 0

    result = {
        "status": "collision",
        "table": args.table,
        "detail": (
            f"the live database carries a table named {args.table} whose shape is "
            f"NOT the substrate's {args.table}"
        ),
        "differences": diffs,
        "remedy": (
            "SQLite's CREATE TABLE IF NOT EXISTS leaves an existing table of the same "
            "name alone, so the substrate's own schema would never be applied to it. "
            "Rename or drop the unrelated table, or migrate it into the substrate's "
            "shape, before installing this revision."
        ),
    }
    _emit(result, args.json)
    return 1


def _emit(result: dict, as_json: bool) -> None:
    if as_json:
        print(json.dumps(result, indent=2, sort_keys=True))
        return
    status = result["status"]
    if status == "collision":
        print(f"schema-preflight: SCHEMA COLLISION on table {result['table']!r}", file=sys.stderr)
        print(f"  {result['detail']}", file=sys.stderr)
        for d in result.get("differences", []):
            print(f"    - {d}", file=sys.stderr)
        print(f"  remedy: {result['remedy']}", file=sys.stderr)
    elif status == "compatible":
        print(f"schema-preflight: {result['table']} is compatible with the substrate's schema")
    else:
        print(f"schema-preflight: {result['table']} — {result['detail']}")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="schema_preflight.py",
        description="refuse a schema collision before it becomes a silent incompatibility",
    )
    sub = parser.add_subparsers(dest="cmd", required=True)

    p_extract = sub.add_parser("extract", help="pull a table's DDL out of the Go schema source")
    p_extract.add_argument("--go", required=True, help="path to internal/core/schema.go")
    p_extract.add_argument("--table", default="lifecycle_events")
    p_extract.add_argument("--out", required=True, help="output file, or '-' for stdout")
    p_extract.set_defaults(func=cmd_extract)

    p_check = sub.add_parser("check", help="compare a live database's table against a reference DDL")
    p_check.add_argument("--db", required=True, help="path to the live database")
    p_check.add_argument("--ddl", required=True, help="path to the reference DDL")
    p_check.add_argument("--table", default="lifecycle_events")
    p_check.add_argument("--json", action="store_true", help="emit a JSON report")
    p_check.set_defaults(func=cmd_check)

    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
