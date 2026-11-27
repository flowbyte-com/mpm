"""Real `mpm backup` / `mpm restore-db` round trips (Continuity 001 F1/F6/T117).

These tests drive the ACTUAL binaries, not the handlers:

    .build/bin/mpm backup <path>
    .build/bin/mpm restore-db <path>

and compare LOGICAL CONTENTS AND SQLITE STORAGE TYPES, because the
reviewed defect was silent data mutation: `007` became `7`, `1e5`
became `100000.0`, and `00123` became `123` while every exit code
stayed zero.

The binary is a BUILD PREREQUISITE of `make test-scripts`, so a missing
binary is a failure here, never a skip: a skipped data-integrity test
reads exactly like a passing one.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
MPM = REPO / ".build" / "bin" / "mpm"

PROJECT_ID = "cpj-00000000000000f1"

MANAGED_TABLES = (
    "projects",
    "agent_tasks",
    "agent_task_sessions",
    "agent_task_supersessions",
    "agent_task_events",
)


def run_mpm(workspace: str, *args: str, stdin: str | None = None) -> subprocess.CompletedProcess:
    env = dict(os.environ)
    env["MPM_WORKSPACE"] = workspace
    env["HOME"] = str(Path(workspace) / "home")
    return subprocess.run(
        [str(MPM), *args],
        input=stdin,
        text=True,
        capture_output=True,
        env=env,
        timeout=180,
    )


def sqlite(db: Path, sql: str) -> str:
    return subprocess.run(
        ["sqlite3", str(db), sql], text=True, capture_output=True, check=True
    ).stdout


def db_path(workspace: str) -> Path:
    return Path(workspace) / "src" / "db" / "mpm.db"


def snapshot(db: Path) -> dict[str, list[str]]:
    """Every managed row as `typeof|quote` per column, in a stable order."""
    out: dict[str, list[str]] = {}
    for table in MANAGED_TABLES:
        cols = [
            row
            for row in sqlite(db, f'SELECT name FROM pragma_table_info("{table}") ORDER BY cid').splitlines()
            if row
        ]
        sel = ", ".join(f'typeof("{c}"), quote("{c}")' for c in cols)
        order = "id" if table in ("schema_migrations",) else "rowid"
        rows = sqlite(db, f'SELECT {sel} FROM "{table}" ORDER BY {order}').splitlines()
        out[table] = rows
    return out


class RealBackupRoundTrip(unittest.TestCase):
    """The real commands, over disposable workspaces."""

    @classmethod
    def setUpClass(cls):
        if not MPM.is_file():
            raise AssertionError(
                f"{MPM} does not exist. `make test-scripts` depends on `build`, "
                "so a missing binary means the build prerequisite was skipped — "
                "and a skipped restore test is indistinguishable from a passing one."
            )
        if shutil.which("sqlite3") is None:
            raise AssertionError(
                "the real restore pipeline needs the `sqlite3` CLI for .dump; "
                "it is not installed, so this integration test cannot be trusted"
            )

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="mpm-real-rt-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    # ── helpers ────────────────────────────────────────────────────────────

    def workspace(self, name: str) -> str:
        ws = self.root / name
        ws.mkdir(parents=True, exist_ok=True)
        return str(ws)

    def seed_ordinary(self) -> str:
        """A real project, root and child through the real CLI."""
        ws = self.workspace("src")
        res = run_mpm(ws, "assignment", "create-project", "--id", PROJECT_ID, "--json")
        self.assertEqual(res.returncode, 0, res.stderr + res.stdout)
        res = run_mpm(
            ws, "assignment", "create-root",
            "--project-id", PROJECT_ID,
            "--title", "007",
            "--objective", "line one\nline two",
            "--recipient", "00123",
            "--idempotency-key", "idem:real:root",
            "--permitted-operation", "read,write",
            "--eligible-recipient", "00123,-0,1e5",
            "--max-depth", "3",
            "--max-tasks", "16",
            "--max-concurrency", "2",
            "--max-resource-units", "100",
            "--json",
        )
        self.assertEqual(res.returncode, 0, res.stderr + res.stdout)
        res = run_mpm(
            ws, "assignment", "delegate",
            "--parent-id", self.root_id(ws),
            "--title", "the child",
            "--objective", "a child objective",
            "--recipient", "1e5",
            "--role", "implementer",
            "--idempotency-key", "idem:real:child",
            "--json",
        )
        self.assertEqual(res.returncode, 0, res.stderr + res.stdout)
        return ws

    def root_id(self, ws: str) -> str:
        return sqlite(db_path(ws), "SELECT id FROM agent_tasks WHERE idempotency_key='idem:real:root'").strip()

    def backup(self, ws: str, name: str) -> Path:
        out = self.root / name
        res = run_mpm(ws, "backup", str(out))
        self.assertEqual(res.returncode, 0, res.stderr + res.stdout)
        self.assertTrue(out.is_file(), "the backup file was not written")
        return out

    def restore(self, ws: str, dump: Path) -> None:
        res = run_mpm(ws, "restore-db", str(dump), stdin="y\n")
        self.assertEqual(res.returncode, 0, res.stderr + res.stdout)

    # ── scenarios ──────────────────────────────────────────────────────────

    def test_01_candidate_backup_restores_and_preserves_types(self):
        """Ordinary database: real backup → real restore, values AND types."""
        ws = self.seed_ordinary()
        before_db = db_path(ws)
        before = snapshot(before_db)

        # The dump must carry the representations that broke the reviewed
        # pipeline; otherwise this test would pass over a dump that never
        # exercised them.
        dump = self.backup(ws, "ordinary.sql")
        body = dump.read_text(encoding="utf-8")
        self.assertIn("replace(", body, "the real dump must carry replace(...) text")
        self.assertIn("X'", body, "the real dump must carry blob literals")

        restored_ws = self.workspace("restored")
        self.restore(restored_ws, dump)
        after = snapshot(db_path(restored_ws))

        for table in MANAGED_TABLES:
            self.assertEqual(before[table], after[table], f"{table} changed across the round trip")

        # The headline type assertions.
        root = self.root_id(restored_ws)
        row = sqlite(
            db_path(restored_ws),
            f"SELECT title, typeof(title), recipient, typeof(recipient), "
            f"objective, typeof(objective) FROM agent_tasks WHERE id='{root}'",
        ).rstrip("\n").split("|")
        self.assertEqual(row[0], "007", "a numeric-looking title was coerced")
        self.assertEqual(row[1], "text")
        self.assertEqual(row[2], "00123", "a numeric-looking recipient was coerced")
        self.assertEqual(row[3], "text")
        self.assertEqual(row[5], "text")
        self.assertIn("\n", row[4], "the multiline objective lost its newline")

        # Canonical schema, foreign keys and execution restrictions hold.
        self.assertEqual(sqlite(db_path(restored_ws), "PRAGMA foreign_key_check").strip(), "")
        triggers = sqlite(
            db_path(restored_ws),
            "SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'trg\\_agent\\_task%' ESCAPE '\\'",
        ).strip()
        self.assertGreater(int(triggers), 0, "the restored database lost its managed triggers")

    def test_02_restored_database_backs_up_and_restores_again(self):
        """Second generation: restore the backup of a restored database."""
        ws = self.seed_ordinary()
        first = snapshot(db_path(ws))
        dump1 = self.backup(ws, "gen1.sql")

        restored_ws = self.workspace("gen1")
        self.restore(restored_ws, dump1)
        dump2 = self.backup(restored_ws, "gen2.sql")

        gen2_ws = self.workspace("gen2")
        self.restore(gen2_ws, dump2)
        second = snapshot(db_path(gen2_ws))
        for table in MANAGED_TABLES:
            self.assertEqual(first[table], second[table], f"{table} changed on the second generation")

    def test_03_supersession_aware_database_round_trips(self):
        """A supersession-aware database with ZERO lineages still restores.

        Classification is table+sentinel based; zero lineage rows is the
        normal state of a post-feature database, not a pre-feature one.
        """
        ws = self.seed_ordinary()
        self.assertEqual(
            sqlite(db_path(ws), "SELECT COUNT(*) FROM agent_task_supersessions").strip(), "0"
        )
        before = snapshot(db_path(ws))
        dump = self.backup(ws, "aware.sql")
        restored_ws = self.workspace("aware-restored")
        self.restore(restored_ws, dump)
        after = snapshot(db_path(restored_ws))
        for table in MANAGED_TABLES:
            self.assertEqual(before[table], after[table], f"{table} changed")

    def test_04_failed_restore_preserves_the_database_and_the_dump(self):
        """A refused restore leaves the live database and the source dump intact."""
        ws = self.seed_ordinary()
        good = self.backup(ws, "good.sql")
        before = snapshot(db_path(ws))
        before_hash = db_path(ws).read_bytes()

        broken = self.root / "broken.sql"
        body = good.read_text(encoding="utf-8")
        # Remove one canonical trigger: the restore must refuse, not repair.
        name = "trg_agent_tasks_reserved_key"
        lines = body.splitlines(keepends=True)
        idx = next(i for i, line in enumerate(lines) if line.startswith(f"CREATE TRIGGER {name}"))
        end = next(i for i in range(idx, len(lines)) if lines[i].rstrip().endswith("END;"))
        del lines[idx : end + 1]
        broken.write_text("".join(lines), encoding="utf-8")
        broken_bytes = broken.read_bytes()

        res = run_mpm(ws, "restore-db", str(broken), stdin="y\n")
        self.assertNotEqual(res.returncode, 0, "a dump with a missing trigger was accepted")
        self.assertIn("missing_canonical_trigger", res.stderr + res.stdout)

        after = snapshot(db_path(ws))
        for table in MANAGED_TABLES:
            self.assertEqual(before[table], after[table], f"a refused restore changed {table}")
        self.assertEqual(before_hash, db_path(ws).read_bytes(), "the live database file changed")
        self.assertEqual(broken_bytes, broken.read_bytes(), "the source dump changed")

    def test_05_restore_refuses_an_unrecognised_managed_literal(self):
        """A managed row with an expression outside the closed grammar is refused."""
        ws = self.seed_ordinary()
        good = self.backup(ws, "good.sql")
        body = good.read_text(encoding="utf-8")
        self.assertIn("'idem:real:child'", body)
        broken = self.root / "broken.sql"
        broken.write_text(body.replace("'idem:real:child'", "abs(-1)", 1), encoding="utf-8")
        res = run_mpm(ws, "restore-db", str(broken), stdin="y\n")
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("unsupported literal", res.stderr + res.stdout)


if __name__ == "__main__":
    unittest.main()
