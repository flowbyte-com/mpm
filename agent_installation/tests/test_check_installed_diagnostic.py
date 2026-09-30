"""
test_check_installed_diagnostic.py — exit-code contract for
check_installed_managed_blocks.py.

Pins the contract documented in mpm_integration_check.md §24:

  PASS only                                -> exit 0
  PASS + ABSENT (host not installed)       -> exit 0
  any WARN                                 -> exit 1
  any ERROR                                -> exit 1

ABSENT is informational: the host's integration is not installed on
this machine, so there is no managed block that could be current or
stale. It is NOT a failure of the diagnostic.

ABSENT is NOT reachable merely by having the install target file
present without a managed block. The managed behavioural block is a
required part of a functional host integration, so "integration
installed + block missing" is WARN (drift to repair), not ABSENT.

Every test runs against a sandboxed HOME. The previous version of
this file asserted against the real machine's home and pinned
"openclaw: file=present block=absent verdict=ABSENT" as the
acceptance state; that encoded the superseded assumption that an
installed OpenClaw with no managed block was healthy, and it made
the suite fail on any machine whose real state legitimately changed.
"""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

EXPERIMENT_ROOT = Path(__file__).resolve().parent.parent
SCRIPT = EXPERIMENT_ROOT / "scripts" / "check_installed_managed_blocks.py"


def _run_script(home: Path) -> subprocess.CompletedProcess:
    env = os.environ.copy()
    env["HOME"] = str(home)
    return subprocess.run(
        [sys.executable, str(SCRIPT), "--json"],
        capture_output=True, text=True, env=env, timeout=30,
    )


class ExitCodeContract(unittest.TestCase):
    """Exit-code semantics for the diagnostic.

    Uses a sandboxed HOME so it can drive the script through each
    verdict class (PASS / WARN / ABSENT / ERROR) by hand-crafting
    the host install target files.
    """

    def setUp(self):
        self.tmpdir = Path(tempfile.mkdtemp(prefix="mpm-diag-exit-"))

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    # ---------- ABSENT ----------

    def test_all_absent_returns_zero(self):
        """An empty HOME has no installed hosts: every host is ABSENT
        and the diagnostic is informational (exit 0)."""
        proc = _run_script(self.tmpdir / "home_empty")
        self.assertEqual(
            proc.returncode, 0,
            f"all-ABSENT should be exit 0, got {proc.returncode}; stderr={proc.stderr}",
        )
        payload = json.loads(proc.stdout)
        for row in payload:
            self.assertEqual(row["verdict"], "ABSENT", row)

    def test_absent_when_openclaw_integration_not_installed(self):
        """OpenClaw present as a host but its MPM integration absent
        is ABSENT, and remains informational.

        This is the machine state in which MPM must not opt the
        machine into OpenClaw.
        """
        home = self.tmpdir / "home_openclaw_no_integration"
        # A bare OpenClaw install: host config + SOUL.md, but no MPM
        # plugin entry and no extensions dir.
        (home / ".openclaw").mkdir(parents=True, exist_ok=True)
        (home / ".openclaw" / "openclaw.json").write_text(
            json.dumps({"plugins": {"entries": {"anthropic": {}}}}),
            encoding="utf-8",
        )
        workspace = home / ".openclaw" / "workspace"
        workspace.mkdir(parents=True, exist_ok=True)
        (workspace / "SOUL.md").write_text("# SOUL.md\npersona\n", encoding="utf-8")

        proc = _run_script(home)
        self.assertEqual(
            proc.returncode, 0,
            f"ABSENT must stay informational (exit 0); stdout={proc.stdout}; "
            f"stderr={proc.stderr}",
        )
        row = {r["host"]: r for r in json.loads(proc.stdout)}["openclaw"]
        self.assertEqual(row["verdict"], "ABSENT", row)
        # Presence and currency are still reported unambiguously.
        self.assertEqual(row["file"], "present")
        self.assertEqual(row["block"], "absent")

    # ---------- WARN ----------

    def test_warn_returns_one(self):
        """A host with a present-but-stale managed block must exit 1."""
        home = self.tmpdir / "home_warn"
        target = home / ".claude" / "CLAUDE.md"
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(
            "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->\n"
            "## stale body\n"
            "<!-- END MPM-MANAGED SECTION:claude-code-instructions -->\n"
        )
        proc = _run_script(home)
        self.assertEqual(
            proc.returncode, 1,
            f"WARN should be exit 1, got {proc.returncode}; "
            f"stdout={proc.stdout}; stderr={proc.stderr}",
        )
        payload = json.loads(proc.stdout)
        verdict_by_host = {row["host"]: row["verdict"] for row in payload}
        self.assertEqual(verdict_by_host["claude_code"], "WARN")

    def test_warn_not_absent_when_openclaw_integration_installed(self):
        """The contract change: an installed OpenClaw MPM integration
        with a missing managed block is WARN drift, not ABSENT.

        Previously this combination reported ABSENT and exit 0, which
        is what let a host expose MPM tools while the agent failed to
        use them, with no signal to the operator.
        """
        home = self.tmpdir / "home_openclaw_integration"
        # MPM OpenClaw integration IS installed.
        (home / ".openclaw" / "extensions" / "mpm-memory-openclaw").mkdir(
            parents=True, exist_ok=True,
        )
        # ... but the persistent managed block is missing.
        workspace = home / ".openclaw" / "workspace"
        workspace.mkdir(parents=True, exist_ok=True)
        (workspace / "SOUL.md").write_text("# SOUL.md\npersona\n", encoding="utf-8")

        proc = _run_script(home)
        self.assertEqual(
            proc.returncode, 1,
            f"installed integration + missing block must exit 1; "
            f"stdout={proc.stdout}; stderr={proc.stderr}",
        )
        row = {r["host"]: r for r in json.loads(proc.stdout)}["openclaw"]
        self.assertEqual(
            row["verdict"], "WARN",
            "an installed OpenClaw without its managed block is drift, "
            "not a healthy intentional state",
        )


class DiagnosticFields(unittest.TestCase):
    """The diagnostic must surface unambiguous file/block presence
    fields, not the legacy single `presence` field that conflates
    "file is on disk" with "managed section is in the file".
    """

    def setUp(self):
        self.tmpdir = Path(tempfile.mkdtemp(prefix="mpm-diag-fields-"))

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_fields_are_unambiguous_regardless_of_verdict(self):
        """file/block are reported independently of the verdict, so a
        WARN caused by a missing block still shows file=present."""
        home = self.tmpdir / "home_fields"
        (home / ".openclaw" / "extensions" / "mpm-memory-openclaw").mkdir(
            parents=True, exist_ok=True,
        )
        workspace = home / ".openclaw" / "workspace"
        workspace.mkdir(parents=True, exist_ok=True)
        (workspace / "SOUL.md").write_text("persona\n", encoding="utf-8")

        proc = _run_script(home)
        self.assertIn(proc.returncode, (0, 1), proc.stderr)
        row = {r["host"]: r for r in json.loads(proc.stdout)}["openclaw"]
        self.assertEqual(row["file"], "present")
        self.assertEqual(row["block"], "absent")
        # A repair path is offered whenever the block needs work.
        self.assertTrue(row["repair"], "WARN must carry a repair path")


if __name__ == "__main__":
    unittest.main(verbosity=2)
