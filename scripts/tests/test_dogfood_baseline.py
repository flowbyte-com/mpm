"""Tests for `scripts/dogfood_baseline.sh`.

The script is the instrument the whole seven-day evaluation rests on, so
three properties of it are asserted here, and each is asserted against a
**deliberately broken fixture** as well as a working one. A gate that only
ever sees a healthy tree proves nothing.

1. **Fail closed.** A measurement that cannot be taken must never be
   reported as a measurement of zero. A missing table, an unreadable
   database and a missing file must all produce a non-zero exit and a
   named gap.

2. **Version differences are not gaps.** The pre-upgrade schema has no
   `lesson_proposals` table. That is a *supported state*, and it must be
   reported as a version difference — explicitly NOT as an empty proposal
   queue.

3. **The script contains no production-changing command.** The incident
   recorded in the plan's §12.3 happened because a shell executed
   copy-pasteable examples that changed the live installation. The script
   must never be able to do that, and the assertion below is the
   regression guard.

The fixtures are built from an embedded DDL rather than from the live
database: the tests must not read, write, or depend on `~/.mpm`.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import sqlite3
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPT = REPO_ROOT / "scripts" / "dogfood_baseline.sh"

# The columns `scripts/dogfood_baseline.sh` actually reads. This is a
# *test fixture*, not a schema: it exists so the script can be exercised
# against a known shape without touching any live database.
FIXTURE_DDL = """
CREATE TABLE memories (
    id TEXT PRIMARY KEY, collection TEXT, content TEXT, tags TEXT,
    metadata TEXT, created_at INTEGER, deleted_at INTEGER);
CREATE TABLE lessons_base (
    id TEXT PRIMARY KEY, type TEXT, content TEXT, tags TEXT,
    reinforcement_count INTEGER DEFAULT 1, source_session_id TEXT,
    created TEXT, content_hash TEXT, confidence REAL DEFAULT 0.7,
    deleted_at INTEGER);
CREATE TABLE evidence (
    id TEXT PRIMARY KEY, artifact_id TEXT, artifact_type TEXT, type TEXT,
    source_group TEXT, strength REAL, independence_factor REAL,
    created_by TEXT, created_at INTEGER, expires_at INTEGER,
    notes TEXT, reference_url TEXT);
CREATE TABLE retrieval_metadata (
    node_id TEXT PRIMARY KEY, node_type TEXT, reuse_count INTEGER,
    last_retrieved_at INTEGER, success_count INTEGER,
    created_at INTEGER, updated_at INTEGER);
CREATE TABLE tool_invocations (
    id TEXT PRIMARY KEY, session_id TEXT, tool_name TEXT, action TEXT,
    invocation_id TEXT, actor_kind TEXT, framework_name TEXT,
    payload_hash TEXT, result_status TEXT, started_at INTEGER,
    completed_at INTEGER, duration_ms INTEGER, error_message TEXT,
    mpm_session_id TEXT, framework_session_id TEXT,
    outcome_class TEXT, outcome_code TEXT);
CREATE TABLE artifact_provenance (
    id TEXT PRIMARY KEY, artifact_id TEXT, artifact_type TEXT,
    created_at INTEGER, schema_version TEXT, actor_kind TEXT,
    actor_id TEXT, framework_name TEXT, session_id TEXT,
    invocation_id TEXT);
CREATE TABLE session_handoffs (
    id TEXT PRIMARY KEY, session_id TEXT, ended_at INTEGER,
    ended_state TEXT, summary TEXT, commitments TEXT,
    open_questions TEXT, read_at INTEGER, read_by TEXT,
    created_at INTEGER, mpm_session_id TEXT, framework_session_id TEXT);
CREATE TABLE scheduled_wakes (
    id TEXT PRIMARY KEY, target_time INTEGER, created_at INTEGER,
    fired INTEGER);
CREATE TABLE audit_cluster_proposals (
    cluster_key TEXT PRIMARY KEY, component TEXT, message_hash TEXT,
    count INTEGER, last_seen TEXT, status TEXT);
CREATE TABLE epistemic_cascade_outbox (
    id TEXT PRIMARY KEY, invalidation_event_id TEXT, status TEXT,
    created_at INTEGER);
CREATE TABLE system_audit_log (
    id TEXT PRIMARY KEY, level TEXT, component TEXT, message TEXT,
    stack_trace TEXT, context TEXT, created_at TEXT, invocation_id TEXT,
    mpm_session_id TEXT, framework_name TEXT, framework_session_id TEXT,
    event_code TEXT);
CREATE TABLE works (
    id TEXT PRIMARY KEY, title TEXT, content TEXT, status TEXT,
    verification TEXT, created_at INTEGER, updated_at INTEGER,
    completed_at INTEGER);
"""

# The post-upgrade table. Added — or not — to make the two schema states.
PROPOSALS_DDL = """
CREATE TABLE lesson_proposals (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL DEFAULT 'insight',
    content TEXT NOT NULL,
    tags JSON,
    created TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','promoted','rejected')),
    source_session_id TEXT,
    framework_name TEXT,
    model_name TEXT,
    source_ids TEXT,
    promoted_lesson_id TEXT,
    reviewed_at INTEGER);
CREATE INDEX idx_lesson_proposals_status
    ON lesson_proposals(status, created);
"""


def build_fixture(path: Path, *, proposals: bool, seed: bool = True) -> Path:
    """Create a disposable database with the fixture schema.

    `proposals=True` is the post-upgrade shape; `False` is the
    pre-upgrade shape.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    con = sqlite3.connect(path)
    try:
        con.executescript(FIXTURE_DDL)
        if proposals:
            con.executescript(PROPOSALS_DDL)
        if seed:
            con.executescript(
                """
                INSERT INTO memories VALUES
                  ('m1','memories','a memory','["x"]','{}',1000,NULL);
                INSERT INTO lessons_base VALUES
                  ('l1','insight','a lesson','[]',1,'s1','2026-10-01T00:00:00Z',
                   'h',0.7,NULL);
                INSERT INTO evidence VALUES
                  ('e1','m1','memory','support','g',1.0,1.0,'cli',1000,NULL,NULL,NULL);
                INSERT INTO retrieval_metadata VALUES
                  ('m1','memory',3,2000,0,1000,2000);
                INSERT INTO tool_invocations VALUES
                  ('t1','sess','mpm_memory','save','inv-1','agent','opencode',
                   'sha256:x','success',1000,1001,1,NULL,'mpm-sess',NULL,
                   'ok','');
                INSERT INTO artifact_provenance VALUES
                  ('p1','m1','memory',1000,'v1','agent','mpm_cli','opencode',
                   'sess','inv-1');
                INSERT INTO session_handoffs VALUES
                  ('h1','sess',1000,'clean','summary','[]','[]',NULL,NULL,
                   1000,'mpm-sess',NULL);
                INSERT INTO scheduled_wakes VALUES ('w1',1000,1000,1);
                INSERT INTO audit_cluster_proposals VALUES
                  ('k1','web_db','h',1,'2026-10-01','active');
                INSERT INTO epistemic_cascade_outbox VALUES
                  ('o1','e1','pending',1000);
                INSERT INTO system_audit_log VALUES
                  ('a1','warn','handoff','EndSessionV2 insert failed',NULL,NULL,
                   '2026-10-27 18:21:56',NULL,NULL,NULL,NULL,NULL);
                INSERT INTO works VALUES
                  ('w1','t','c','open',NULL,1000,1000,NULL);
                """
            )
            if proposals:
                con.executescript(
                    """
                    INSERT INTO lesson_proposals VALUES
                      ('pr1','insight','{"title":"t","body":"b"}','[]',
                       '2026-10-30T16:34:03Z','pending','sess','opencode','',
                       '["a","b"]',NULL,NULL),
                      ('pr2','insight','an agent fact','[]',
                       '2026-10-30T16:40:00Z','pending','sess2','opencode','',
                       NULL,NULL,NULL),
                      ('pr3','insight','promoted','[]',
                       '2026-10-30T16:35:00Z','promoted','sess3','opencode','',
                       NULL,'l9',1793377962),
                      ('pr4','insight','rejected','[]',
                       '2026-10-30T16:36:00Z','rejected','sess4','opencode','',
                       NULL,NULL,1793377962);
                    """
                )
        con.commit()
    finally:
        con.close()
    return path


HOST_FIXTURE_DDL = """
CREATE TABLE session_v2 (
    id TEXT PRIMARY KEY, directory TEXT, project_id TEXT, cost REAL,
    tokens_input INTEGER, tokens_cache_read INTEGER, tokens_output INTEGER,
    tokens_reasoning INTEGER, tokens_cache_write INTEGER,
    time_created INTEGER);
CREATE TABLE session_message (
    id TEXT PRIMARY KEY, session_id TEXT, data TEXT);
INSERT INTO session_v2 VALUES
  ('s1','/home/v/projects/themailroom',NULL,0.0,10,20,30,40,0,1000);
INSERT INTO session_message VALUES
  ('m1','s1','{"model":{"providerID":"minimax-coding-plan"},"cost":0}');
"""


def build_host_fixture(path: Path) -> Path:
    """A disposable host session database.

    The script treats an unreadable host database as a MEASUREMENT GAP
    (the token accounting is a required measurement), so a test that
    relied on the operator's real OpenCode database would fail on a
    machine that has none. The fixture makes the test hermetic.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    con = sqlite3.connect(path)
    try:
        con.executescript(HOST_FIXTURE_DDL)
        con.commit()
    finally:
        con.close()
    return path


def run_script(db: Path, *extra: str) -> subprocess.CompletedProcess:
    """Run the script against a fixture, with a scratch MPM workspace.

    The scratch workspace carries a scheduler-state file whose pid is the
    *test process itself*, which is alive, so the scheduler checks pass
    and the run exercises the healthy path rather than the stop path.
    """
    env = dict(os.environ)
    with tempfile.TemporaryDirectory() as tmp:
        ws = Path(tmp) / "workspace"
        run_dir = ws / "run"
        run_dir.mkdir(parents=True, exist_ok=True)
        (run_dir / "scheduler.state").write_text(
            json.dumps(
                {
                    "last_tick_unix": int(time.time()),
                    "process_started_unix": int(time.time()) - 60,
                    "tick_count": 7,
                    "pid": os.getpid(),
                    "last_status": "ok",
                    "last_error": "",
                }
            )
        )
        host_db = build_host_fixture(Path(tmp) / "host" / "opencode.db")
        env["MPM_WORKSPACE"] = str(ws)
        return subprocess.run(
            [
                "bash",
                str(SCRIPT),
                "--db",
                str(db),
                "--host-db",
                str(host_db),
                "--no-wake",
                *extra,
            ],
            capture_output=True,
            text=True,
            env=env,
            timeout=300,
        )


class TestFailClosed(unittest.TestCase):
    """A measurement that cannot be taken is never reported as zero."""

    def test_missing_database_exits_non_zero(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            missing = Path(tmp) / "nope.db"
            proc = run_script(missing)
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("not readable", proc.stdout + proc.stderr)

    def test_missing_table_is_a_named_gap_and_non_zero_exit(self) -> None:
        """Drop a table the script reads: the report must be INCOMPLETE."""
        with tempfile.TemporaryDirectory() as tmp:
            db = build_fixture(Path(tmp) / "mpm.db", proposals=True)
            con = sqlite3.connect(db)
            con.execute("DROP TABLE artifact_provenance")
            con.execute("DROP TABLE session_handoffs")
            con.commit()
            con.close()
            proc = run_script(db)
        self.assertEqual(proc.returncode, 2, proc.stdout[-3000:])
        self.assertIn("MEASUREMENT GAPS", proc.stdout)
        self.assertIn("artifact_provenance", proc.stdout)
        self.assertIn("session_handoffs", proc.stdout)

    def test_broken_measurement_reads_unavailable_not_zero(self) -> None:
        """The failed measurement must say UNAVAILABLE, never 0."""
        with tempfile.TemporaryDirectory() as tmp:
            db = build_fixture(Path(tmp) / "mpm.db", proposals=True)
            con = sqlite3.connect(db)
            con.execute("DROP TABLE artifact_provenance")
            con.commit()
            con.close()
            proc = run_script(db)
        self.assertIn("UNAVAIL", proc.stdout)

    def test_missing_host_database_is_a_gap_not_a_zero(self) -> None:
        """The token accounting is a REQUIRED measurement.

        A host without the OpenCode session database therefore produces an
        INCOMPLETE report, not a report that says "0 tokens". This is the
        behaviour that failed on CI when the test relied on the operator's
        real session database; the fixture above makes the test hermetic.
        """
        with tempfile.TemporaryDirectory() as tmp:
            db = build_fixture(Path(tmp) / "mpm.db", proposals=True)
            env = dict(os.environ)
            with tempfile.TemporaryDirectory() as ws:
                run_dir = Path(ws) / "run"
                run_dir.mkdir(parents=True, exist_ok=True)
                (run_dir / "scheduler.state").write_text(
                    json.dumps(
                        {
                            "last_tick_unix": int(time.time()),
                            "process_started_unix": int(time.time()) - 60,
                            "tick_count": 7,
                            "pid": os.getpid(),
                            "last_status": "ok",
                            "last_error": "",
                        }
                    )
                )
                env["MPM_WORKSPACE"] = ws
                proc = subprocess.run(
                    [
                        "bash",
                        str(SCRIPT),
                        "--db",
                        str(db),
                        "--host-db",
                        str(Path(tmp) / "no-such-host.db"),
                        "--no-wake",
                    ],
                    capture_output=True,
                    text=True,
                    env=env,
                    timeout=300,
                )
        self.assertEqual(proc.returncode, 2, proc.stdout[-3000:])
        self.assertIn("host session database not readable", proc.stdout)
        self.assertIn("token accounting is unavailable", proc.stdout)

    def test_a_complete_run_reports_no_gaps(self) -> None:
        """The positive control: a healthy fixture is clean and exits 0."""
        with tempfile.TemporaryDirectory() as tmp:
            db = build_fixture(Path(tmp) / "mpm.db", proposals=True)
            proc = run_script(db)
        self.assertEqual(proc.returncode, 0, proc.stdout[-3000:])
        self.assertIn("none. Every measurement in this report was taken.", proc.stdout)


class TestSchemaVersionDifference(unittest.TestCase):
    """A pre-upgrade schema is a version difference, not an empty queue."""

    def test_pre_upgrade_schema_reports_a_version_difference(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            db = build_fixture(Path(tmp) / "mpm.db", proposals=False)
            proc = run_script(db)
        self.assertEqual(proc.returncode, 0, proc.stdout[-3000:])
        self.assertIn("VERSION DIFFERENCE", proc.stdout)
        self.assertIn("NOT an empty queue", proc.stdout)
        self.assertIn("pre-upgrade (no lesson_proposals)", proc.stdout)
        # A version difference is NOT a measurement gap.
        self.assertIn("none. Every measurement in this report was taken.", proc.stdout)

    def test_pre_upgrade_schema_does_not_report_an_empty_queue(self) -> None:
        """The failure this guards against: '0 pending proposals' on a
        pre-upgrade database reads as 'the queue is empty'."""
        with tempfile.TemporaryDirectory() as tmp:
            db = build_fixture(Path(tmp) / "mpm.db", proposals=False)
            proc = run_script(db)
        self.assertNotIn("proposals total / pending", proc.stdout)

    def test_post_upgrade_schema_reports_the_lifecycle(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            db = build_fixture(Path(tmp) / "mpm.db", proposals=True)
            proc = run_script(db)
        self.assertEqual(proc.returncode, 0, proc.stdout[-3000:])
        self.assertIn("post-upgrade (lesson_proposals present)", proc.stdout)
        self.assertIn("proposals total / pending / promoted / rejected", proc.stdout)
        # The fixture has 4 proposals: 2 pending, 1 promoted, 1 rejected.
        self.assertIn("4 / 2 / 1 / 1", proc.stdout)
        # One compaction-origin proposal (the {"title":…} envelope).
        self.assertIn("compaction-origin proposals", proc.stdout)
        self.assertIn("1 of 4", proc.stdout)
        # The source_ids signal is reported separately from origin.
        self.assertIn("proposals carrying cited sources", proc.stdout)
        # The provenance fields are reported.
        self.assertIn("provenance populated:", proc.stdout)

    def test_compaction_detection_does_not_treat_cited_sources_as_compaction(self) -> None:
        """`source_ids` non-empty is a SECONDARY signal.

        The fixture's compaction proposal carries source_ids; the agent
        proposal does not. An agent proposal WITH source_ids would be
        counted as 'carrying cited sources' but NOT as compaction-origin,
        so the two counts must be independent.
        """
        with tempfile.TemporaryDirectory() as tmp:
            db = build_fixture(Path(tmp) / "mpm.db", proposals=True)
            proc = run_script(db)
        self.assertIn("compaction-origin proposals                       : 1 of 4", proc.stdout)
        self.assertIn("proposals carrying cited sources (source_ids)     : 1 of 4", proc.stdout)


class TestLessonSupportClaim(unittest.TestCase):
    """The corrected wording, and the withdrawn claim."""

    def test_zero_evidence_is_not_called_an_unsupported_assertion(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            db = build_fixture(Path(tmp) / "mpm.db", proposals=True)
            proc = run_script(db)
        self.assertIn("lessons with NO DIRECT evidence link", proc.stdout)
        self.assertIn("NOT a claim that the lesson is false", proc.stdout)
        self.assertNotIn("unsupported-assertion watch list", proc.stdout)


class TestNoProductionChangingCommands(unittest.TestCase):
    """The script must never be able to change the live installation.

    This is the regression guard for the incident recorded in the plan's
    §12.3. The script is run by the operator every day, often by an
    agent, and it is about a live system.
    """

    # Commands that would change the live installation, the operator's
    # credential file, or the service state. None may appear at all.
    FORBIDDEN = (
        "systemctl",
        "mpm config profile set",
        "mpm config set",
        "mpm lesson promote",
        "mpm lesson reject",
        "mpm deploy",
        "mpm install",
        "mpm-scheduler --interval",
        "mpm-scheduler run",
        "chmod",
        "install -m",
        "shutdown",
        "kill ",
        "pkill",
        "tee ",
    )

    def test_script_contains_no_production_changing_command(self) -> None:
        source = SCRIPT.read_text()
        for needle in self.FORBIDDEN:
            with self.subTest(needle=needle):
                self.assertTrue(
                    needle not in source,
                    f"the script must not contain {needle!r}",
                )

    def test_destructive_removal_is_confined_to_the_scratch_directory(self) -> None:
        """`rm -rf` is allowed exactly once, and only on the script's own
        temporary directory."""
        source = SCRIPT.read_text()
        occurrences = [
            line.strip()
            for line in source.splitlines()
            if "rm -rf" in line or "rm -f" in line
        ]
        self.assertTrue(occurrences, "expected the scratch cleanup to exist")
        for line in occurrences:
            self.assertTrue(
                "$SNAP_DIR" in line,
                f"a removal outside the scratch directory: {line}",
            )

    def test_a_full_run_changes_nothing_outside_the_scratch_directory(self) -> None:
        """The empirical form of the same property.

        A fake HOME is populated with the paths the script could plausibly
        touch — the workspace database, the scheduler state file, the
        credential file, the host session database — and every byte of it
        is compared before and after a full run. Nothing may change.
        """
        import hashlib

        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp) / "home"
            ws = home / ".mpm"
            (ws / "src" / "db").mkdir(parents=True)
            (ws / "run").mkdir(parents=True)
            (home / ".config" / "mpm").mkdir(parents=True)
            (home / ".local" / "share" / "opencode").mkdir(parents=True)

            build_fixture(ws / "src" / "db" / "mpm.db", proposals=True)
            (ws / "run" / "scheduler.state").write_text(
                json.dumps(
                    {
                        "last_tick_unix": int(time.time()),
                        "process_started_unix": int(time.time()) - 60,
                        "tick_count": 7,
                        "pid": os.getpid(),
                        "last_status": "ok",
                        "last_error": "",
                    }
                )
            )
            # A credential file the script must never read into its output
            # and never write. Its content is a sentinel, not a real key.
            cred = home / ".config" / "mpm" / "mpm.env"
            cred.write_text("MINIMAX_API_KEY=sentinel-not-a-real-key\n")
            os.chmod(cred, 0o600)

            host_db = home / ".local" / "share" / "opencode" / "opencode.db"
            con = sqlite3.connect(host_db)
            con.executescript(
                """
                CREATE TABLE session_v2 (
                    id TEXT PRIMARY KEY, directory TEXT, project_id TEXT,
                    cost REAL, tokens_input INTEGER, tokens_cache_read INTEGER,
                    tokens_output INTEGER, tokens_reasoning INTEGER,
                    tokens_cache_write INTEGER, time_created INTEGER);
                CREATE TABLE session_message (
                    id TEXT PRIMARY KEY, session_id TEXT, data TEXT);
                INSERT INTO session_v2 VALUES
                  ('s1','/home/v/projects/themailroom',NULL,0.0,10,20,30,40,0,1000);
                INSERT INTO session_message VALUES
                  ('m1','s1','{"model":{"providerID":"minimax-coding-plan"},"cost":0}');
                """
            )
            con.commit()
            con.close()

            def manifest() -> dict[str, str]:
                out: dict[str, str] = {}
                for path in sorted(home.rglob("*")):
                    if path.is_file():
                        out[str(path.relative_to(home))] = hashlib.sha256(
                            path.read_bytes()
                        ).hexdigest()
                return out

            before = manifest()
            env = dict(os.environ)
            env["HOME"] = str(home)
            env["MPM_WORKSPACE"] = str(ws)
            proc = subprocess.run(
                [
                    "bash",
                    str(SCRIPT),
                    "--db",
                    str(ws / "src" / "db" / "mpm.db"),
                    "--host-db",
                    str(host_db),
                    "--no-wake",
                ],
                capture_output=True,
                text=True,
                env=env,
                timeout=300,
            )
            after = manifest()

        self.assertEqual(before, after, "the script modified something outside its scratch directory")
        # And the sentinel must not have leaked into the report.
        self.assertNotIn("sentinel-not-a-real-key", proc.stdout + proc.stderr)

    def test_script_opens_the_database_read_only(self) -> None:
        source = SCRIPT.read_text()
        self.assertIn("mode=ro", source)
        self.assertIn(".backup", source)


class TestShellSyntax(unittest.TestCase):
    def test_script_parses(self) -> None:
        proc = subprocess.run(
            ["bash", "-n", str(SCRIPT)], capture_output=True, text=True
        )
        self.assertEqual(proc.returncode, 0, proc.stderr)

    @unittest.skipIf(shutil.which("shellcheck") is None, "shellcheck is not installed")
    def test_shellcheck_is_clean_at_warning_level(self) -> None:
        proc = subprocess.run(
            ["shellcheck", "-S", "warning", str(SCRIPT)],
            capture_output=True,
            text=True,
        )
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)


if __name__ == "__main__":
    unittest.main()
