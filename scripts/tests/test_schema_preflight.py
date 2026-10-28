#!/usr/bin/env python3
"""test_schema_preflight.py — the schema-collision preflight.

The substrate creates its tables with `CREATE TABLE IF NOT EXISTS`, and
SQLite's rule for that statement leaves an existing table of the same
name ALONE. A database carrying an unrelated table named
`lifecycle_events` therefore keeps it forever, the substrate's own
schema is never applied, and the failure surfaces later as an error
about a column.

These tests pin the preflight that turns that silent incompatibility
into an explicit refusal, and they pin the two properties that make the
refusal safe to rely on:

  * the comparison is against the SUBSTRATE'S OWN DDL, extracted from
    the Go schema source — not against a second copy of the shape that
    could drift;
  * the live database is never WRITTEN to. It is read through SQLite's
    online backup API into a scratch file, because a newer MPM binary
    would apply pending migrations the moment it opened the database.
"""

import hashlib
import os
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
HELPER = REPO_ROOT / "scripts" / "lib" / "schema_preflight.py"
SCHEMA_GO = REPO_ROOT / "internal" / "core" / "schema.go"


def run(*args, **kwargs):
    return subprocess.run(
        [sys.executable, str(HELPER), *args],
        capture_output=True,
        text=True,
        timeout=120,
        **kwargs,
    )


def sha256(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


class SchemaPreflightTest(unittest.TestCase):
    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-schema-preflight-"))
        self.addCleanup(__import__("shutil").rmtree, self._tmp, ignore_errors=True)
        self.ddl = self._tmp / "lifecycle_events.sql"
        r = run("extract", "--go", str(SCHEMA_GO), "--table", "lifecycle_events",
                "--out", str(self.ddl))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue(self.ddl.is_file())

    # ------------------------------------------------------------------
    # extraction
    # ------------------------------------------------------------------
    def test_the_ddl_is_extracted_from_the_go_source(self):
        text = self.ddl.read_text()
        self.assertTrue(
            text.lstrip().upper().startswith("CREATE TABLE IF NOT EXISTS LIFECYCLE_EVENTS"),
            text[:120],
        )
        self.assertIn("event_id", text)

    def test_extraction_of_an_unknown_table_fails_loudly(self):
        r = run("extract", "--go", str(SCHEMA_GO), "--table", "no_such_table",
                "--out", str(self._tmp / "x.sql"))
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("no DDL", r.stderr)

    # ------------------------------------------------------------------
    # the check
    # ------------------------------------------------------------------
    def _make_db(self, name: str, statements: list[str]) -> Path:
        p = self._tmp / name
        conn = sqlite3.connect(p)
        for s in statements:
            conn.execute(s)
        conn.commit()
        conn.close()
        return p

    def test_a_database_without_the_table_is_absent_not_a_collision(self):
        db = self._make_db("absent.db", ["CREATE TABLE memories (id TEXT)"])
        r = run("check", "--db", str(db), "--ddl", str(self.ddl), "--json")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn('"status": "absent"', r.stdout)

    def test_a_database_with_the_substrates_own_table_is_compatible(self):
        # Build the reference table by applying the substrate's own DDL,
        # then check it against itself.
        db = self._tmp / "compatible.db"
        conn = sqlite3.connect(db)
        conn.executescript(self.ddl.read_text())
        conn.commit()
        conn.close()
        r = run("check", "--db", str(db), "--ddl", str(self.ddl), "--json")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn('"status": "compatible"', r.stdout)

    def test_an_unrelated_table_of_the_same_name_is_a_collision(self):
        db = self._make_db(
            "collision.db",
            [
                "CREATE TABLE lifecycle_events (id TEXT PRIMARY KEY, note TEXT)",
                "INSERT INTO lifecycle_events VALUES ('x', 'unrelated')",
            ],
        )
        r = run("check", "--db", str(db), "--ddl", str(self.ddl), "--json")
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn('"status": "collision"', r.stdout)
        self.assertIn("event_id", r.stdout)

    def test_the_collision_report_names_the_remedy(self):
        db = self._make_db(
            "remedy.db",
            ["CREATE TABLE lifecycle_events (id TEXT PRIMARY KEY, note TEXT)"],
        )
        r = run("check", "--db", str(db), "--ddl", str(self.ddl))
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn("SCHEMA COLLISION", r.stderr)
        self.assertIn("CREATE TABLE IF NOT EXISTS", r.stderr)
        self.assertIn("remedy", r.stderr)

    def test_a_missing_database_is_not_a_collision(self):
        r = run("check", "--db", str(self._tmp / "absent-file.db"), "--ddl", str(self.ddl))
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    # ------------------------------------------------------------------
    # the live database is never written to
    # ------------------------------------------------------------------
    def test_the_check_does_not_write_to_the_database(self):
        db = self._make_db(
            "readonly.db",
            [
                "CREATE TABLE lifecycle_events (id TEXT PRIMARY KEY, note TEXT)",
                "INSERT INTO lifecycle_events VALUES ('x', 'unrelated')",
            ],
        )
        before = sha256(db)
        r = run("check", "--db", str(db), "--ddl", str(self.ddl))
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertEqual(
            sha256(db), before,
            "the preflight modified the database it was asked to inspect",
        )

    def test_the_check_leaves_no_sidecar_files_next_to_the_database(self):
        db = self._make_db(
            "sidecars.db",
            ["CREATE TABLE lifecycle_events (id TEXT PRIMARY KEY, note TEXT)"],
        )
        run("check", "--db", str(db), "--ddl", str(self.ddl))
        for suffix in ("-wal", "-shm", "-journal"):
            self.assertFalse(
                Path(str(db) + suffix).exists(),
                f"the preflight created {db.name}{suffix} next to the live database",
            )


if __name__ == "__main__":
    unittest.main()
