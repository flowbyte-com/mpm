"""
test_install_reconcile_exit_contract.py — the root installer's
handling of the generic reconciler's exit codes.

Background
----------

`agent_installation/scripts/reconcile_managed_blocks.py` distinguishes
two failure modes:

  rc=1  advisory — a managed block did not converge. One broken
        adapter must not abort an install for every other host.
  rc=2  a REQUIRED provisioning step failed — a host integration MPM
        builds (a compiled plugin payload and its registration) could
        not be produced. The install MUST NOT report success: the
        host is wired to an artifact that does not exist.

The root installer stays host-agnostic — it never names an adapter or
a host — so it cannot know WHICH host failed. It honours the
reconciler's exit-code contract instead. These tests pin that
contract, because the false-success this mechanism exists to prevent
is exactly what a well-meaning "one adapter failed, carry on" would
reintroduce.

The tests drive the real `phase_agent_reconcile` and
`reconcile_outcome` out of the real install.sh (sourced as a library),
with a fake reconciler script standing in for the generic one.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
INSTALL_SH = REPO_ROOT / "install.sh"

STRIP_PATTERNS = (
    re.compile(r"^\s*readonly\s+SCRIPT_DIR\s*="),
    re.compile(r"^\s*readonly\s+PROJECT_ROOT\s*="),
    re.compile(r'^\s*main\s+"\$@"\s*$'),
)


def build_sourced_lib(install_text: str) -> str:
    out_lines = []
    for line in install_text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out_lines.append(line)
    return "\n".join(out_lines) + "\n"


class ReconcileExitContract(unittest.TestCase):
    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-reconcile-contract-"))
        self.addCleanup(lambda: shutil.rmtree(self._tmp, ignore_errors=True))
        self.home = self._tmp / "home"
        self.home.mkdir()
        self.project = self._tmp / "project"
        self.project.mkdir()
        # The reconciler the phase invokes.
        scripts = self.project / "agent_installation" / "scripts"
        scripts.mkdir(parents=True)
        # build-provenance.sh is invoked by export_build_provenance.
        prov = self.project / "scripts"
        prov.mkdir(parents=True)
        shutil.copy(
            REPO_ROOT / "scripts" / "build-provenance.sh",
            prov / "build-provenance.sh",
        )
        (self.project / "install.sh.lib").write_text(
            build_sourced_lib(INSTALL_SH.read_text())
        )

    def _stage_reconciler(self, exit_code: int) -> None:
        path = (
            self.project
            / "agent_installation"
            / "scripts"
            / "reconcile_managed_blocks.py"
        )
        path.write_text(
            textwrap.dedent(f"""\
                #!/usr/bin/env python3
                import sys
                sys.stderr.write("fake reconciler: failing as configured\\n")
                sys.exit({exit_code})
                """)
        )

    def _drive(self, body: str) -> subprocess.CompletedProcess:
        driver = self.project / "_drive.sh"
        driver.write_text(
            textwrap.dedent(f"""\
                #!/usr/bin/env bash
                set -uo pipefail
                export PROJECT_ROOT={self.project}
                source {self.project}/install.sh.lib
                export HOME={self.home}
                {body}
                """)
        )
        driver.chmod(0o755)
        env = os.environ.copy()
        env["HOME"] = str(self.home)
        env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
        return subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver)],
            capture_output=True,
            text=True,
            env=env,
            timeout=60,
        )

    def test_phase_reports_success_when_the_reconciler_succeeds(self):
        self._stage_reconciler(0)
        result = self._drive("phase_agent_reconcile")
        self.assertEqual(
            result.returncode, 0, result.stdout + result.stderr
        )

    def test_phase_reports_one_for_an_advisory_failure(self):
        self._stage_reconciler(1)
        result = self._drive("phase_agent_reconcile")
        self.assertEqual(
            result.returncode, 1, result.stdout + result.stderr
        )

    def test_phase_reports_two_for_a_required_provisioning_failure(self):
        self._stage_reconciler(2)
        result = self._drive("phase_agent_reconcile")
        self.assertEqual(
            result.returncode, 2, result.stdout + result.stderr
        )

    def test_advisory_failure_does_not_stop_the_install(self):
        """rc=1 must let the install continue."""
        self._stage_reconciler(1)
        result = self._drive(
            'rc=0; phase_agent_reconcile || rc=$?; '
            'outcome=0; reconcile_outcome "$rc" || outcome=$?; '
            'echo "outcome=$outcome"'
        )
        self.assertIn("outcome=0", result.stdout)

    def test_required_failure_stops_the_install(self):
        """rc=2 must NOT let the install continue."""
        self._stage_reconciler(2)
        result = self._drive(
            'rc=0; phase_agent_reconcile || rc=$?; '
            'outcome=0; reconcile_outcome "$rc" || outcome=$?; '
            'echo "outcome=$outcome"'
        )
        self.assertIn("outcome=2", result.stdout)
        self.assertNotIn("outcome=0", result.stdout)

    def test_install_mode_treats_rc_two_as_fatal(self):
        """mode_install must not continue past a required failure."""
        text = INSTALL_SH.read_text()
        m = re.search(
            r"mode_install\(\)\s*\{(.*?)^\}", text, re.DOTALL | re.MULTILINE
        )
        self.assertIsNotNone(m, "mode_install() not found in install.sh")
        block = m.group(1)
        self.assertIn(
            "reconcile_outcome",
            block,
            "mode_install does not route the reconciler outcome through "
            "reconcile_outcome",
        )
        self.assertIn(
            "die",
            block,
            "mode_install does not stop on a required provisioning failure",
        )


if __name__ == "__main__":
    unittest.main()
