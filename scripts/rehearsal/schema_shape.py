#!/usr/bin/env python3
"""schema_shape.py <db> <table>

Prints the canonical shape of a table in a SQLite database: for every
column, its name, declared type, NOT NULL flag, default and CHECK
constraints, plus the table's indexes and triggers.

The canonical form is what a schema comparison needs: two tables with
the same columns in a different order are the same table; two tables
whose CHECK constraints differ are NOT the same table, and SQLite will
not repair the difference on its own.
"""
import re
import sqlite3
import sys


def canonical(db, table):
    c = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    out = []
    try:
        cols = c.execute(f'PRAGMA table_info("{table}")').fetchall()
        if not cols:
            return None
        for r in cols:
            out.append((r[1], (r[2] or "").upper(), int(r[3]), r[4] or ""))
        ddl = c.execute(
            "SELECT sql FROM sqlite_master WHERE type='table' AND name=?", (table,)
        ).fetchone()
        checks = sorted(re.findall(r"CHECK\s*\([^)]*\)", ddl[0])) if ddl and ddl[0] else []
        idx = sorted(
            r[0] for r in c.execute(
                "SELECT name FROM sqlite_master WHERE type='index' AND tbl_name=? AND sql IS NOT NULL",
                (table,),
            )
        )
        trg = sorted(
            r[0] for r in c.execute(
                "SELECT name FROM sqlite_master WHERE type='trigger' AND tbl_name=?", (table,)
            )
        )
        return {"columns": out, "checks": checks, "indexes": idx, "triggers": trg}
    finally:
        c.close()


def main() -> int:
    if len(sys.argv) < 3:
        print("usage: schema_shape.py <db> <table>", file=sys.stderr)
        return 2
    shape = canonical(sys.argv[1], sys.argv[2])
    if shape is None:
        print(f"TABLE-ABSENT {sys.argv[2]}")
        return 0
    for name, typ, nn, dflt in shape["columns"]:
        print(f"COLUMN {name} {typ} notnull={nn} default={dflt}")
    for ck in shape["checks"]:
        print(f"CHECK {ck}")
    for i in shape["indexes"]:
        print(f"INDEX {i}")
    for t in shape["triggers"]:
        print(f"TRIGGER {t}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
