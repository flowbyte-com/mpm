#!/usr/bin/env python3
"""test_rehearsal_live_gate.py — the rehearsal's live-installation gate.

THE DEFECT THIS FILE EXISTS FOR
-------------------------------
The rehearsal asserted that the live installation was untouched by
hashing `(path, size, mtime)` over a list of live paths, INCLUDING the
live database file.

That assertion was invalid. The live scheduler records health checks and
SQLite checkpoints the WAL, so the live database's size and mtime move
on their own:

    t=0    content-fingerprint=5882dd04...  size=22536192
    t=60   content-fingerprint=5882dd04...  size=22556672
    t=240  content-fingerprint=5882dd04...  size=22622208

The database grew by 86 KB over four minutes while the operator's
knowledge did not change by one bit. The old assertion reported that as
"the live tree's snapshot is unchanged" FAILED, for a reason that had
nothing to do with the rehearsal. A false positive in an isolation gate
is worse than no gate: it teaches the operator to ignore the check, and
that is how a real change gets missed.

WHAT IS PINNED HERE
--------------------
The gate is CLASSIFIED, and both halves are proved:

  * ordinary scheduler activity — the audit trail growing, the
    scheduler's liveness counter being rewritten, a memory's volatile
    columns being bumped by a read — does NOT fail the gate;
  * an unauthorised change — to the protected configuration, to an
    executable, to the managed instructions, or to the operator's
    knowledge inside the database — DOES fail it.

And the gate FAILS CLOSED: when it cannot measure, it does not report
success.

THE FIXTURE
------------
A DISPOSABLE stand-in for the live tree, built fresh for every test.
Nothing here resolves into the operator's real ~/.mpm, and the gate is
never pointed at it.
"""

import json
import os
import re
import shutil
import sqlite3
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
REHEARSAL = REPO_ROOT / "scripts" / "rehearsal"
GATE = REHEARSAL / "live_gate.py"
LIB = REHEARSAL / "lib.sh"

SCHEMA = """
CREATE TABLE memories (
  id TEXT PRIMARY KEY,
  collection TEXT NOT NULL,
  content TEXT NOT NULL,
  tags TEXT,
  metadata TEXT,
  created_at INTEGER,
  updated_at INTEGER,
  last_accessed_at INTEGER,
  confidence REAL
);
CREATE TABLE lessons_base (
  id TEXT PRIMARY KEY,
  body TEXT NOT NULL,
  created_at INTEGER
);
CREATE VIEW lessons AS SELECT id, body, created_at FROM lessons_base;
CREATE TABLE evidence (
  id TEXT PRIMARY KEY,
  artifact_id TEXT NOT NULL,
  artifact_type TEXT NOT NULL,
  content TEXT,
  created_at INTEGER
);
CREATE TABLE tool_invocations (
  id TEXT PRIMARY KEY,
  session_id TEXT,
  tool_name TEXT,
  actor_kind TEXT,
  framework_name TEXT,
  started_at INTEGER
);
CREATE TABLE system_config (
  key TEXT PRIMARY KEY,
  raw_json TEXT,
  updated_at INTEGER
);
"""


class LiveGateFixture(unittest.TestCase):
    """A disposable stand-in for the live installation."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-live-gate-test-"))
        self.addCleanup(shutil.rmtree, self._tmp, ignore_errors=True)

        self.live_mpm = self._tmp / "mpm"
        self.oc_config = self._tmp / "opencode"
        self.systemd_user = self._tmp / "systemd"
        for d in (self.live_mpm, self.oc_config, self.systemd_user):
            d.mkdir(parents=True, exist_ok=True)

        # The protected artifacts.
        (self.live_mpm / "bin").mkdir()
        (self.live_mpm / "bin" / "mpm").write_text("#!/bin/sh\necho mpm\n")
        (self.live_mpm / "agent_installation" / "mpm-opencode").mkdir(parents=True)
        (self.live_mpm / "agent_installation" / "mpm-opencode" / "server.js").write_text(
            "export default async function () { return { name: 'mpm' }; }\n")
        (self.oc_config / "opencode.jsonc").write_text('{\n  // the operator\'s config\n}\n')
        (self.oc_config / "AGENTS.md").write_text("# managed instructions\n")
        (self.systemd_user / "mpm-scheduler.service").write_text(
            "[Service]\nExecStart=%h/.mpm/bin/mpm-scheduler\n")

        # The live database.
        self.db = self.live_mpm / "src" / "db" / "mpm.db"
        self.db.parent.mkdir(parents=True)
        self._make_db()

    def _make_db(self):
        c = sqlite3.connect(self.db)
        c.executescript(SCHEMA)
        c.execute(
            "INSERT INTO memories (id, collection, content, created_at) VALUES (?,?,?,?)",
            ("m1", "memories", "the operator's memory", 1),
        )
        c.execute("INSERT INTO lessons_base (id, body, created_at) VALUES (?,?,?)",
                  ("l1", "the operator's lesson", 1))
        c.execute(
            "INSERT INTO evidence (id, artifact_id, artifact_type, content, created_at)"
            " VALUES (?,?,?,?,?)", ("e1", "m1", "memory", "supports", 1))
        c.execute(
            "INSERT INTO tool_invocations (id, tool_name, actor_kind, framework_name,"
            " started_at) VALUES (?,?,?,?,?)",
            ("t1", "mpm_context", "human", "mpm-cli", 1))
        c.execute("INSERT INTO system_config (key, raw_json, updated_at) VALUES (?,?,?)",
                  ("scheduler_active_uptime", '{"active_seconds":10}', 1))
        c.commit()
        c.close()

    # ------------------------------------------------------------------
    # driving the gate
    # ------------------------------------------------------------------
    def snapshot(self, name: str = "snap") -> Path:
        out = self._tmp / f"{name}.json"
        r = subprocess.run(
            ["python3", str(GATE), "snapshot",
             "--live-mpm", str(self.live_mpm),
             "--oc-config", str(self.oc_config),
             "--systemd-user", str(self.systemd_user),
             "--out", str(out)],
            capture_output=True, text=True, timeout=120,
        )
        if r.returncode != 0:
            raise AssertionError(f"the snapshot failed: {r.stdout}\n{r.stderr}")
        return out

    def compare(self, before: Path, after: Path):
        r = subprocess.run(
            ["python3", str(GATE), "compare", str(before), str(after)],
            capture_output=True, text=True, timeout=120,
        )
        return r

    def assert_clean(self, before: Path, after: Path):
        r = self.compare(before, after)
        self.assertEqual(
            r.returncode, 0,
            f"the gate must pass on ordinary activity:\n{r.stdout}\n{r.stderr}",
        )
        return r

    def assert_detected(self, before: Path, after: Path, needle: str):
        r = self.compare(before, after)
        self.assertEqual(
            r.returncode, 1,
            f"the gate must DETECT the unauthorised change:\n{r.stdout}\n{r.stderr}",
        )
        self.assertIn(
            needle, r.stderr + r.stdout,
            f"the diagnostic must name what changed ({needle!r}):\n{r.stdout}\n{r.stderr}",
        )
        return r


class TestOrdinaryActivityDoesNotFail(LiveGateFixture):
    """Ordinary scheduler activity must not trip the gate."""

    def test_the_audit_trail_growing_is_not_a_failure(self):
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute(
            "INSERT INTO tool_invocations (id, tool_name, actor_kind, framework_name,"
            " started_at) VALUES (?,?,?,?,?)",
            ("t2", "mpm_context", "human", "mpm-cli", 2),
        )
        c.commit()
        c.close()
        after = self.snapshot("after")
        r = self.assert_clean(before, after)
        self.assertIn("tool_invocations", r.stdout,
                      "the movement must be REPORTED as evidence")

    def test_the_scheduler_liveness_counter_being_rewritten_is_not_a_failure(self):
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("UPDATE system_config SET raw_json = ?, updated_at = ?",
                  ('{"active_seconds":20}', 2))
        c.commit()
        c.close()
        after = self.snapshot("after")
        r = self.assert_clean(before, after)
        self.assertIn("system_config", r.stdout)

    def test_a_read_that_bumps_the_volatile_columns_is_not_a_failure(self):
        # Reading a memory updates last_accessed_at and the decay adjusts
        # confidence. Hashing the raw row would report a difference after
        # nothing more than a query.
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("UPDATE memories SET last_accessed_at = 99, confidence = 0.5,"
                  " updated_at = 99 WHERE id = 'm1'")
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_clean(before, after)

    def test_the_database_file_growing_is_not_a_failure(self):
        # The file's bytes move for reasons that have nothing to do with
        # the rehearsal. The assertion is the DOMAIN state, not the file.
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("VACUUM")
        c.execute(
            "INSERT INTO tool_invocations (id, tool_name, actor_kind, framework_name,"
            " started_at) VALUES (?,?,?,?,?)",
            ("t3", "mpm_context", "human", "mpm-cli", 3))
        c.commit()
        c.close()
        after = self.snapshot("after")
        r = self.assert_clean(before, after)
        self.assertIn("live database FILE changed", r.stdout,
                      "the file movement must be REPORTED, as evidence, not asserted")


class TestUnauthorisedChangesAreDetected(LiveGateFixture):
    """The negative control: the gate must DETECT a real change."""

    def test_a_modified_configuration_file_is_detected(self):
        before = self.snapshot("before")
        (self.oc_config / "opencode.jsonc").write_text(
            '{\n  // the operator\'s config — TAMPERED\n}\n')
        after = self.snapshot("after")
        self.assert_detected(before, after, "opencode.jsonc")

    def test_a_modified_managed_instruction_is_detected(self):
        before = self.snapshot("before")
        (self.oc_config / "AGENTS.md").write_text("# managed instructions — TAMPERED\n")
        after = self.snapshot("after")
        self.assert_detected(before, after, "AGENTS.md")

    def test_a_modified_executable_is_detected(self):
        before = self.snapshot("before")
        (self.live_mpm / "bin" / "mpm").write_text("#!/bin/sh\necho pwned\n")
        after = self.snapshot("after")
        self.assert_detected(before, after, "bin/mpm")

    def test_a_new_executable_is_detected(self):
        before = self.snapshot("before")
        (self.live_mpm / "bin" / "mpm-extra").write_text("unexpected\n")
        after = self.snapshot("after")
        self.assert_detected(before, after, "mpm-extra")

    def test_a_removed_executable_is_detected(self):
        before = self.snapshot("before")
        (self.live_mpm / "bin" / "mpm").unlink()
        after = self.snapshot("after")
        self.assert_detected(before, after, "bin/mpm")

    def test_a_modified_host_asset_is_detected(self):
        before = self.snapshot("before")
        (self.live_mpm / "agent_installation" / "mpm-opencode" / "server.js").write_text(
            "export default async function () { return { name: 'TAMPERED' }; }\n")
        after = self.snapshot("after")
        self.assert_detected(before, after, "server.js")

    def test_a_modified_systemd_unit_is_detected(self):
        before = self.snapshot("before")
        (self.systemd_user / "mpm-scheduler.service").write_text(
            "[Service]\nExecStart=/bin/false\n")
        after = self.snapshot("after")
        self.assert_detected(before, after, "mpm-scheduler.service")

    def test_a_written_operator_memory_is_detected(self):
        # THE database case: the test must NOT treat all database
        # mutations as harmless. A write to the operator's knowledge is
        # a real finding.
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute(
            "INSERT INTO memories (id, collection, content, created_at) VALUES (?,?,?,?)",
            ("m2", "memories", "an unauthorised memory", 2),
        )
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_detected(before, after, "memories")

    def test_a_modified_lesson_is_detected(self):
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("UPDATE lessons_base SET body = 'tampered' WHERE id = 'l1'")
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_detected(before, after, "lessons")

    def test_a_new_table_in_the_live_database_is_detected(self):
        # A rehearsal that applied a migration to the live database is
        # the incident the rehearsal's own README documents.
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("CREATE TABLE surprise (id TEXT PRIMARY KEY)")
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_detected(before, after, "surprise")


class TestNonVacuity(LiveGateFixture):
    """The gate must catch what the old assertion could not."""

    # ── the rule the gate replaces ────────────────────────────────────────
    #
    # The old assertion hashed (path, size, mtime) over a list of live
    # paths that INCLUDED the live database file.  It is reproduced here
    # verbatim, so the comparison between it and the gate is measured
    # rather than asserted.
    OLD_RULE_PATHS = (
        ("live_mpm", "bin"),
        ("live_mpm", "src", "db", "mpm.db"),
        ("live_mpm", "agent_installation"),
        ("systemd_user", "mpm-scheduler.service"),
        ("oc_config", "opencode.jsonc"),
        ("oc_config", "AGENTS.md"),
    )

    def old_rule(self) -> str:
        import hashlib
        lines = []
        for parts in self.OLD_RULE_PATHS:
            p = os.path.join(str(getattr(self, parts[0])), *parts[1:])
            if os.path.isdir(p):
                for dirpath, dirnames, filenames in os.walk(p):
                    dirnames.sort()
                    for name in sorted(filenames):
                        f = os.path.join(dirpath, name)
                        st = os.stat(f)
                        lines.append(f"{f} {st.st_size} {st.st_mtime}")
            elif os.path.exists(p):
                st = os.stat(p)
                lines.append(f"{p} {st.st_size} {st.st_mtime}")
            else:
                lines.append(f"{p} ABSENT")
        return hashlib.sha256("\n".join(sorted(lines)).encode()).hexdigest()

    def test_the_old_rule_would_have_failed_where_the_gate_passes(self):
        # THE A/B PROOF.  Ordinary scheduler activity — the audit trail
        # grows, the database file grows with it, the file's mtime moves
        # — and NOTHING about the installation changes.  The old rule
        # called that a failure; the gate calls it what it is.
        old_before = self.old_rule()
        before = self.snapshot("before")

        c = sqlite3.connect(self.db)
        c.execute(
            "INSERT INTO tool_invocations (id, tool_name, actor_kind, framework_name,"
            " started_at) VALUES (?,?,?,?,?)",
            ("t9", "mpm_context", "human", "mpm-cli", 9))
        c.commit()
        c.close()
        # Make the file itself grow and its mtime move, exactly as a WAL
        # checkpoint does.
        c = sqlite3.connect(self.db)
        c.execute("VACUUM")
        c.close()
        os.utime(self.db, None)

        old_after = self.old_rule()
        after = self.snapshot("after")

        self.assertNotEqual(
            old_before, old_after,
            "the old rule must have seen a change here — otherwise this "
            "test proves nothing about the difference between the two",
        )
        self.assert_clean(before, after)

    # A same-length substitution, so the change preserves the file's
    # size.  The old assertion compared size and mtime, so a change like
    # this — content changed, size and mtime restored — was invisible
    # to it.
    SAME_LENGTH = ("config", "EVIL!!")

    def _tamper_same_size_same_mtime(self):
        p = self.oc_config / "opencode.jsonc"
        original = p.read_text()
        st = os.stat(p)
        self.assertIn(self.SAME_LENGTH[0], original)
        p.write_text(original.replace(*self.SAME_LENGTH))
        self.assertEqual(
            os.stat(p).st_size, st.st_size,
            "the change must preserve the size, or the proof is void",
        )
        os.utime(p, (st.st_atime, st.st_mtime))
        self.assertEqual(os.stat(p).st_mtime, st.st_mtime,
                         "the change must restore the mtime, or the proof is void")

    def test_a_same_size_change_with_a_restored_mtime_is_detected(self):
        # THE NON-VACUITY PROOF.
        before = self.snapshot("before")
        self._tamper_same_size_same_mtime()
        after = self.snapshot("after")
        self.assert_detected(before, after, "opencode.jsonc")

    def test_the_old_size_and_mtime_assertion_would_have_missed_it(self):
        # The same change, measured the OLD way, to show the difference is
        # real and not a claim.
        p = self.oc_config / "opencode.jsonc"
        before = self.snapshot("before")
        size_before, mtime_before = os.stat(p).st_size, os.stat(p).st_mtime
        self._tamper_same_size_same_mtime()
        size_after, mtime_after = os.stat(p).st_size, os.stat(p).st_mtime

        # The old assertion hashed (path, size, mtime).  Both are
        # identical here, so it would have reported "unchanged" — while
        # the protected configuration had been rewritten.
        self.assertEqual(size_before, size_after)
        self.assertEqual(mtime_before, mtime_after)
        self.assertNotEqual(p.read_text(), '{\n  // the operator\'s config\n}\n')

        # And the gate still catches it.
        after = self.snapshot("after")
        self.assert_detected(before, after, "opencode.jsonc")


class TestTheGateFailsClosed(LiveGateFixture):
    """A gate that cannot measure must not report success."""

    def test_an_unreadable_database_is_a_measurement_failure(self):
        # The database path is a directory: it exists, and it cannot be
        # opened. The gate must say it could not measure.
        for suffix in ("", "-wal", "-shm"):
            p = Path(str(self.db) + suffix)
            if p.is_dir():
                p.rmdir()
            elif p.exists():
                p.unlink()
        self.db.mkdir()
        out = self._tmp / "snap.json"
        r = subprocess.run(
            ["python3", str(GATE), "snapshot",
             "--live-mpm", str(self.live_mpm),
             "--oc-config", str(self.oc_config),
             "--systemd-user", str(self.systemd_user),
             "--out", str(out)],
            capture_output=True, text=True, timeout=120,
        )
        self.assertEqual(r.returncode, 2, r.stdout + r.stderr)
        self.assertIn("could not measure", r.stderr)

    def test_a_missing_live_installation_is_not_a_measurement_failure(self):
        # Nothing installed means nothing to protect. That is a clean
        # measurement, not a failure to measure.
        empty = self._tmp / "empty"
        empty.mkdir()
        out = self._tmp / "snap.json"
        r = subprocess.run(
            ["python3", str(GATE), "snapshot",
             "--live-mpm", str(empty),
             "--oc-config", str(empty),
             "--systemd-user", str(empty),
             "--out", str(out)],
            capture_output=True, text=True, timeout=120,
        )
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_snapshots_from_different_gate_versions_are_not_comparable(self):
        before = self.snapshot("before")
        after = self.snapshot("after")
        doc = json.loads(after.read_text())
        doc["version"] = doc["version"] + 1
        after.write_text(json.dumps(doc))
        r = self.compare(before, after)
        self.assertEqual(r.returncode, 2, r.stdout + r.stderr)


class TestTheRehearsalWiring(LiveGateFixture):
    """The rehearsal's own code path must drive the gate."""

    def test_lib_sh_wrappers_measure_the_tree_and_compare_it(self):
        work = self._tmp / "work"
        work.mkdir()
        script = textwrap.dedent(f"""\
            set -uo pipefail
            REH_DIR="{REHEARSAL}"
            LIVE_MPM="{self.live_mpm}"
            LIVE_OC_CONFIG="{self.oc_config}"
            LIVE_SYSTEMD_USER="{self.systemd_user}"
            REH_WORK="{work}"
            . "{LIB}"
            live_gate_snapshot "{work}/a.json" || exit 1
            live_gate_snapshot "{work}/b.json" || exit 1
            live_gate_compare "{work}/a.json" "{work}/b.json"
            echo "rc=$?"
        """)
        r = subprocess.run(["bash", "-c", script], capture_output=True,
                           text=True, timeout=120)
        self.assertIn("rc=0", r.stdout, r.stderr)

    def test_the_rehearsal_no_longer_uses_the_invalid_size_and_mtime_snapshot(self):
        # The old helper compared (path, size, mtime) over a list that
        # included the live database. It must be gone, so no future
        # caller can reach for it.
        lib = LIB.read_text()
        self.assertIsNone(
            re.search(r"^live_snapshot\s*\(\)\s*\{", lib, re.M),
            "the invalid size/mtime snapshot must not come back",
        )
        for suite in ("10_obligations.sh", "20_negative.sh"):
            text = (REHEARSAL / suite).read_text()
            self.assertNotIn("live_snapshot", text,
                             f"{suite} must use the classified gate")
            self.assertIn("live_gate_compare", text,
                          f"{suite} must use the classified gate")


if __name__ == "__main__":
    unittest.main()
