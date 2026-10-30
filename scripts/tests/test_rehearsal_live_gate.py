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
FINGERPRINT = REHEARSAL / "state_fingerprint.py"
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
  content_hash TEXT,
  updated_at INTEGER,
  config_snapshot JSON
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
        c.execute("INSERT INTO system_config (key, raw_json, updated_at) VALUES (?,?,?)",
                  ("compaction.last_run", '{"last_run_at":"2026-10-30T00:00:00Z"}', 1))
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
        c.execute("UPDATE system_config SET raw_json = ?, updated_at = ?"
                  " WHERE key = 'scheduler_active_uptime'",
                  ('{"active_seconds":20}', 2))
        c.commit()
        c.close()
        after = self.snapshot("after")
        r = self.assert_clean(before, after)
        self.assertIn("scheduler_active_uptime", r.stdout,
                      "the run marker's movement must be REPORTED as evidence")

    def test_the_compaction_run_marker_being_rewritten_is_not_a_failure(self):
        # The second measured run marker: the compaction drain's
        # last-run record.  It is a run marker too, so it is permitted
        # — but ONLY because it is named.
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("UPDATE system_config SET raw_json = ?, updated_at = ?"
                  " WHERE key = 'compaction.last_run'",
                  ('{"last_run_at":"2026-10-30T16:34:03Z"}', 2))
        c.commit()
        c.close()
        after = self.snapshot("after")
        r = self.assert_clean(before, after)
        self.assertIn("compaction.last_run", r.stdout)

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


class TestTheConfigurationExceptionsAreNarrow(LiveGateFixture):
    """`system_config` is CONFIGURATION — with narrow, named exceptions.

    The gate used to classify the whole table as "operational", so any
    change to any key passed.  That is too broad: configuration is
    written by the installer and by deliberate operator action, never
    by ordinary activity.  The exception is now key-scoped: only the
    measured run markers may move, and only in the measured columns.
    """

    def test_a_change_to_a_configuration_key_that_is_not_a_run_marker_is_detected(self):
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("INSERT INTO system_config (key, raw_json, updated_at)"
                  " VALUES (?,?,?)", ("model.default", '{"model":"x"}', 2))
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_detected(before, after, "model.default")

    def test_a_change_to_an_existing_configuration_value_is_detected(self):
        c = sqlite3.connect(self.db)
        c.execute("INSERT INTO system_config (key, raw_json, updated_at)"
                  " VALUES (?,?,?)", ("model.default", '{"model":"x"}', 1))
        c.commit()
        c.close()
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("UPDATE system_config SET raw_json = ? WHERE key = ?",
                  ('{"changed":true}', "model.default"))
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_detected(before, after, "model.default")

    def test_a_removed_configuration_key_is_detected(self):
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("INSERT INTO system_config (key, raw_json, updated_at)"
                  " VALUES (?,?,?)", ("model.default", '{"model":"x"}', 2))
        c.commit()
        c.close()
        mid = self.snapshot("mid")
        c = sqlite3.connect(self.db)
        c.execute("DELETE FROM system_config WHERE key = ?", ("model.default",))
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_detected(mid, after, "model.default")

    def test_a_run_marker_changing_in_a_column_it_may_not_change_is_detected(self):
        # The exception is narrow in the COLUMN as well as in the key:
        # a run marker may record WHEN it last ran, not anything else.
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("UPDATE system_config SET content_hash = ?"
                  " WHERE key = 'scheduler_active_uptime'", ("abc123",))
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_detected(before, after, "scheduler_active_uptime")

    def test_a_new_configuration_key_appearing_is_detected(self):
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("INSERT INTO system_config (key, raw_json, updated_at)"
                  " VALUES (?,?,?)", ("model.default", '{"model":"x"}', 2))
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_detected(before, after, "model.default")


class TestTheAuditTrailIsAppendOnly(LiveGateFixture):
    """The audit trail may grow; it may not be rewritten."""

    def test_a_modified_audit_row_is_detected(self):
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("UPDATE tool_invocations SET tool_name = ? WHERE id = ?",
                  ("mpm_something_else", "t1"))
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_detected(before, after, "tool_invocations")

    def test_a_removed_audit_row_is_detected(self):
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("DELETE FROM tool_invocations WHERE id = ?", ("t1",))
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_detected(before, after, "tool_invocations")


class TestTheAccountingRule(LiveGateFixture):
    """The live database has legitimate writers.  The gate must tell
    their writes apart from an unattributed one — and fail closed.

    THE MEASUREMENT that forces this rule: over four 45-second windows
    with the live system running, the operator's own knowledge tables
    GREW (`memories` +8, `evidence` +1, `lesson_proposals` +4) because
    the host's agent sessions were recording decisions and theories.  A
    gate that failed on any knowledge change would fail on an idle live
    system — the same defect this gate exists to remove.

    So the gate does not ask "did the knowledge change?".  It asks a
    question that CAN be answered: does the live system's own audit
    trail account for the change?
    """

    def _add_audit_row(self, n="t2"):
        c = sqlite3.connect(self.db)
        c.execute(
            "INSERT INTO tool_invocations (id, tool_name, actor_kind, framework_name,"
            " started_at) VALUES (?,?,?,?,?)",
            (n, "mpm_memory", "agent", "opencode", 2))
        c.commit()
        c.close()

    def test_a_knowledge_change_accounted_for_by_the_audit_trail_is_not_a_failure(self):
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute(
            "INSERT INTO memories (id, collection, content, created_at) VALUES (?,?,?,?)",
            ("m2", "memories", "a decision the agent recorded", 2))
        c.commit()
        c.close()
        self._add_audit_row()
        after = self.snapshot("after")
        r = self.assert_clean(before, after)
        self.assertIn("ACCOUNTED FOR", r.stdout,
                      "the accounting must be REPORTED, so a reader can see "
                      "why the change was permitted")

    def test_an_unattributed_knowledge_change_is_a_failure(self):
        # The same knowledge change, with NOTHING in the audit trail to
        # account for it.  This is the shape a rehearsal-induced write
        # would have, and it must fail.
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute(
            "INSERT INTO memories (id, collection, content, created_at) VALUES (?,?,?,?)",
            ("m2", "memories", "an unattributed memory", 2))
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_detected(before, after, "memories")

    def test_an_unattributed_bookkeeping_change_is_a_failure(self):
        before = self.snapshot("before")
        c = sqlite3.connect(self.db)
        c.execute("CREATE TABLE topics (id TEXT PRIMARY KEY, name TEXT)")
        c.commit()
        c.close()
        mid = self.snapshot("mid")
        c = sqlite3.connect(self.db)
        c.execute("INSERT INTO topics (id, name) VALUES (?,?)", ("t1", "a topic"))
        c.commit()
        c.close()
        after = self.snapshot("after")
        self.assert_detected(mid, after, "topics")


class TestTheReleasePointer(LiveGateFixture):
    """The active revision, and the symlinks that resolve into it.

    The versioned layout puts the runtime behind a symlink:

        <data root>/current -> releases/<rev>
        <data root>/bin -> current/bin

    Hashing the resolved files alone CANNOT see the pointer change: the
    same bytes behind a different pointer are a different installation.
    These tests use a stand-in whose two releases hold IDENTICAL
    content, so the pointer is the ONLY thing that differs — which is
    precisely the case a content hash cannot see.
    """

    def _build_versioned_layout(self):
        """Replace the stand-in's flat layout with the versioned one."""
        releases = self.live_mpm / "releases"
        for rev in ("revA", "revB"):
            (releases / rev / "bin").mkdir(parents=True)
            (releases / rev / "bin" / "mpm").write_text("#!/bin/sh\necho mpm\n")
            (releases / rev / "RELEASE.json").write_text('{"revision": "%s"}\n' % rev)
            (releases / rev / "agent_installation").mkdir(parents=True)
            (releases / rev / "agent_installation" / "server.js").write_text(
                "export default async function () { return { name: 'mpm' }; }\n")
        # The old flat directories become the symlinks.
        for name in ("bin", "agent_installation"):
            d = self.live_mpm / name
            if d.is_dir() and not d.is_symlink():
                shutil.rmtree(d)
        os.symlink("releases/revA", self.live_mpm / "current")
        os.symlink("current/bin", self.live_mpm / "bin")
        os.symlink("current/agent_installation", self.live_mpm / "agent_installation")

    def test_repointing_the_active_revision_is_detected(self):
        self._build_versioned_layout()
        before = self.snapshot("before")
        # Repoint the active revision.  The content behind it is
        # IDENTICAL, so only the pointer differs.
        os.remove(self.live_mpm / "current")
        os.symlink("releases/revB", self.live_mpm / "current")
        after = self.snapshot("after")
        self.assert_detected(before, after, "the release pointer moved")

    def test_repointing_the_bin_symlink_is_detected(self):
        self._build_versioned_layout()
        before = self.snapshot("before")
        os.remove(self.live_mpm / "bin")
        os.symlink("current/agent_installation", self.live_mpm / "bin")
        after = self.snapshot("after")
        self.assert_detected(before, after, "the release pointer moved")

    def test_the_release_pointer_disappearing_is_detected(self):
        self._build_versioned_layout()
        before = self.snapshot("before")
        os.remove(self.live_mpm / "current")
        after = self.snapshot("after")
        self.assert_detected(before, after, "DISAPPEARED")

    def test_the_release_identity_record_is_detected(self):
        self._build_versioned_layout()
        before = self.snapshot("before")
        (self.live_mpm / "current" / "RELEASE.json").write_text(
            '{"revision": "revB"}\n')
        after = self.snapshot("after")
        self.assert_detected(before, after, "RELEASE.json")

    def test_the_content_behind_the_pointer_is_still_hashed(self):
        # The pointer check must not REPLACE the content check: the
        # binaries behind the pointer are still protected.
        self._build_versioned_layout()
        before = self.snapshot("before")
        (self.live_mpm / "current" / "bin" / "mpm").write_text("#!/bin/sh\necho pwned\n")
        after = self.snapshot("after")
        self.assert_detected(before, after, "mpm")

    def test_an_unchanged_versioned_layout_passes(self):
        self._build_versioned_layout()
        before = self.snapshot("before")
        after = self.snapshot("after")
        self.assert_clean(before, after)


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

    def test_a_table_that_exists_but_cannot_be_read_is_not_skipped(self):
        # A table whose SCHEMA is readable but whose ROWS are not — a
        # corrupt b-tree page, say.  The gate must NOT silently exclude
        # it from the comparison: a table it cannot read is a table it
        # cannot measure, and "could not measure" reported as "nothing
        # changed" is the exact failure this gate exists to remove.
        import importlib.util
        spec = importlib.util.spec_from_file_location("lg", GATE)
        lg = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(lg)

        class _UnreadableTable:
            """A connection whose PRAGMA works and whose SELECT does not."""

            def execute(self, sql):
                if sql.strip().upper().startswith("PRAGMA"):
                    # PRAGMA table_info returns (cid, name, type, ...).
                    return [(0, "id"), (1, "v")]
                raise sqlite3.OperationalError("database disk image is malformed")

        with self.assertRaises(lg.MeasurementError) as caught:
            lg.table_fingerprint(_UnreadableTable(), "bad", drop_volatile=False)
        self.assertIn("cannot read the live table bad", str(caught.exception))


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


class TestTheDomainIsMeasuredByTheSameInstrument(LiveGateFixture):
    """The live database and the simulated one must be measured alike.

    The rehearsal proves that a migration left the SIMULATED state
    unchanged with `state_fingerprint.py --content`.  The gate proves
    that the LIVE state is unchanged.  Those two claims are only
    comparable if both are measured by the SAME instrument — so the
    gate's domain definition must not drift away from the fingerprint's.

    If someone changes the fingerprint's definition, this test fails and
    says why: the live database and the simulated database would then be
    measured by two different rulers, and "unchanged" would mean two
    different things.
    """

    def _load(self, name, path):
        import importlib.util
        spec = importlib.util.spec_from_file_location(name, path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_the_volatile_column_sets_are_identical(self):
        sf = self._load("sf", FINGERPRINT)
        lg = self._load("lg", GATE)
        self.assertEqual(
            set(sf.VOLATILE), set(lg.VOLATILE_COLUMNS),
            "the gate must exclude exactly the columns the substrate's "
            "own fingerprint excludes",
        )

    def test_the_domain_tables_are_the_fingerprint_s_content_tables(self):
        sf = self._load("sf", FINGERPRINT)
        lg = self._load("lg", GATE)
        self.assertEqual(
            tuple(sf.CONTENT_TABLES), tuple(lg.DOMAIN_TABLES),
            "the gate's domain tables must be the fingerprint's content "
            "tables",
        )

    def test_the_per_table_hash_is_byte_identical(self):
        # The same table, hashed by the fingerprint and by the gate, must
        # produce the SAME digest.  This is the "same instrument" claim,
        # measured rather than asserted.
        sf = self._load("sf", FINGERPRINT)
        lg = self._load("lg", GATE)
        c = sqlite3.connect(f"file:{self.db}?mode=ro", uri=True)
        try:
            for table in lg.DOMAIN_TABLES:
                expected = sf.fingerprint(c, table, True)
                got = lg.table_fingerprint(c, table, drop_volatile=True)
                if expected is None or got is None:
                    continue
                self.assertEqual(
                    expected.hexdigest(), got[0],
                    f"the gate and the fingerprint must hash {table} alike",
                )
        finally:
            c.close()


class TestTheSuitesFailClosed(LiveGateFixture):
    """A suite whose gate cannot measure must FAIL, not assert vacuously.

    The gate itself failing closed is not enough: the SUITE must not
    swallow the failure and proceed to a vacuous assertion.  Both
    suites are driven here against a disposable, deliberately unreadable
    stand-in for the live tree — the operator's real installation is
    never pointed at.
    """

    # The suite's OWN failure message — printed by reh_fail, not by the
    # gate.  Asserting on the gate's own output would not prove the
    # suite reacted to it: the gate prints "could not measure" whether
    # or not the suite acts on it.
    SUITE_GUARD_MESSAGE = ("the live-tree snapshot could not be taken "
                           "— the isolation gate cannot measure")

    def _run_suite(self, suite: str):
        # A live tree whose database cannot be opened: the path exists
        # and is a directory.
        broken_mpm = self._tmp / "broken-mpm"
        (broken_mpm / "src" / "db").mkdir(parents=True)
        (broken_mpm / "src" / "db" / "mpm.db").mkdir()
        oc = self._tmp / "broken-oc"
        sd = self._tmp / "broken-sd"
        oc.mkdir()
        sd.mkdir()
        work = self._tmp / "reh-work"
        work.mkdir()
        env = dict(os.environ)
        env.update({
            "REH_WORK": str(work),
            "REH_REV": "HEAD",
            "REH_COMMIT": "0" * 40,
            "EXPECT_COMMIT": "0" * 40,
            "LIVE_MPM": str(broken_mpm),
            "LIVE_OC_CONFIG": str(oc),
            "LIVE_SYSTEMD_USER": str(sd),
        })
        return subprocess.run(
            ["bash", str(REHEARSAL / suite)],
            capture_output=True, text=True, timeout=300, env=env,
            cwd=str(REHEARSAL.parent.parent),
        )

    def _assert_fails_closed(self, suite: str, isolation_assertion: str):
        r = self._run_suite(suite)
        out = r.stdout + r.stderr

        # The suite must REPORT the measurement failure in its own
        # summary.  This is the assertion that distinguishes "the suite
        # reacted" from "the gate printed something and the suite
        # carried on regardless".
        self.assertIn(
            self.SUITE_GUARD_MESSAGE, out,
            f"{suite} must report the measurement failure itself:\n{out}",
        )
        self.assertIn("failed: 1", out,
                      f"{suite} must count the measurement failure:\n{out}")

        # And it must NOT have reached a vacuous isolation assertion.
        self.assertNotIn(
            isolation_assertion, r.stdout,
            f"{suite} must not reach a vacuous isolation assertion:\n{out}",
        )

    def test_the_obligations_suite_fails_when_the_gate_cannot_measure(self):
        self._assert_fails_closed(
            "10_obligations.sh",
            "the live installation was not modified by the migration",
        )

    def test_the_negative_suite_fails_when_the_gate_cannot_measure(self):
        self._assert_fails_closed(
            "20_negative.sh",
            "the live installation is untouched by the negative tests",
        )


if __name__ == "__main__":
    unittest.main()
