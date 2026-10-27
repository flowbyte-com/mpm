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

OpenCode is the only supported host since the 2026-10-27 adapter
removal, so the scenarios below are driven against OpenCode's install
target.
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

# The one shipped host's install target, relative to HOME.
OPENCODE_TARGET = ".config/opencode/AGENTS.md"

# The host's managed-section markers.
OPENCODE_BEGIN = "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->"
OPENCODE_END = "<!-- END MPM-MANAGED SECTION:opencode-instructions -->"


def _run_script(home: Path) -> subprocess.CompletedProcess:
    env = os.environ.copy()
    env["HOME"] = str(home)
    return subprocess.run(
        [sys.executable, str(SCRIPT), "--json"],
        capture_output=True, text=True, env=env, timeout=30,
    )


def _seed_opencode_config(home: Path) -> Path:
    """A bare OpenCode install: the host config exists, no MPM block."""
    config = home / ".config" / "opencode"
    config.mkdir(parents=True, exist_ok=True)
    (config / "opencode.jsonc").write_text(
        json.dumps({"$schema": "https://opencode.ai/config.json", "plugin": []}),
        encoding="utf-8",
    )
    return config


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

    def test_absent_when_the_install_target_is_not_on_disk(self):
        """A host that is present as a host, but whose MPM install
        target does not exist, is ABSENT — and remains informational.

        This is the machine state in which MPM must not opt the
        machine into the host.
        """
        home = self.tmpdir / "home_host_no_target"
        _seed_opencode_config(home)

        proc = _run_script(home)
        self.assertEqual(
            proc.returncode, 0,
            f"ABSENT must stay informational (exit 0); stdout={proc.stdout}; "
            f"stderr={proc.stderr}",
        )
        row = {r["host"]: r for r in json.loads(proc.stdout)}["opencode"]
        self.assertEqual(row["verdict"], "ABSENT", row)
        # Presence and currency are still reported unambiguously.
        self.assertEqual(row["file"], "absent")
        self.assertEqual(row["block"], "absent")

    # ---------- WARN ----------

    def test_warn_returns_one(self):
        """A host with a present-but-stale managed block must exit 1."""
        home = self.tmpdir / "home_warn"
        target = home / OPENCODE_TARGET
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(
            f"{OPENCODE_BEGIN}\n"
            "## stale body\n"
            f"{OPENCODE_END}\n"
        )
        proc = _run_script(home)
        self.assertEqual(
            proc.returncode, 1,
            f"WARN should be exit 1, got {proc.returncode}; "
            f"stdout={proc.stdout}; stderr={proc.stderr}",
        )
        payload = json.loads(proc.stdout)
        verdict_by_host = {row["host"]: row["verdict"] for row in payload}
        self.assertEqual(verdict_by_host["opencode"], "WARN")

    def test_warn_not_absent_when_the_target_exists_without_a_block(self):
        """The contract: an install target that exists without its
        managed block is WARN drift, not ABSENT.

        Previously this combination could report ABSENT and exit 0,
        which is what let a host expose MPM tools while the agent
        failed to use them, with no signal to the operator.
        """
        home = self.tmpdir / "home_target_no_block"
        _seed_opencode_config(home)
        target = home / OPENCODE_TARGET
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("# AGENTS.md\nthe operator's own notes\n", encoding="utf-8")

        proc = _run_script(home)
        self.assertEqual(
            proc.returncode, 1,
            f"existing target + missing block must exit 1; "
            f"stdout={proc.stdout}; stderr={proc.stderr}",
        )
        row = {r["host"]: r for r in json.loads(proc.stdout)}["opencode"]
        self.assertEqual(
            row["verdict"], "WARN",
            "an existing install target without its managed block is drift, "
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
        _seed_opencode_config(home)
        target = home / OPENCODE_TARGET
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("persona\n", encoding="utf-8")

        proc = _run_script(home)
        self.assertIn(proc.returncode, (0, 1), proc.stderr)
        row = {r["host"]: r for r in json.loads(proc.stdout)}["opencode"]
        self.assertEqual(row["file"], "present")
        self.assertEqual(row["block"], "absent")
        # A repair path is offered whenever the block needs work.
        self.assertTrue(row["repair"], "WARN must carry a repair path")


class RetiredHostsAreNotReported(unittest.TestCase):
    """A retired host is not MPM's business any more.

    Reporting it would tell the operator to repair an integration MPM
    no longer supports — and would turn the diagnostic's exit code
    into a function of hosts MPM has retired.
    """

    def setUp(self):
        self.tmpdir = Path(tempfile.mkdtemp(prefix="mpm-diag-retired-"))

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_a_retired_host_with_a_stale_block_is_not_reported(self):
        home = self.tmpdir / "home_retired"
        stale = home / ".claude" / "CLAUDE.md"
        stale.parent.mkdir(parents=True, exist_ok=True)
        stale.write_text(
            "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->\n"
            "## stale body\n"
            "<!-- END MPM-MANAGED SECTION:claude-code-instructions -->\n",
            encoding="utf-8",
        )

        proc = _run_script(home)
        payload = json.loads(proc.stdout)
        hosts = sorted(row["host"] for row in payload)
        self.assertEqual(
            hosts, ["opencode"],
            "the diagnostic must report exactly the shipped hosts",
        )
        self.assertEqual(
            proc.returncode, 0,
            "a stale block belonging to a RETIRED host must not fail the "
            f"diagnostic; stdout={proc.stdout}; stderr={proc.stderr}",
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
