"""
test_install_reconcile_exit_contract.py — the root installer's
handling of the generic reconciler's exit codes.

Background
----------

`agent_installation/scripts/reconcile_managed_blocks.py` distinguishes
three failure modes:

  rc=1  advisory — a managed block did not converge. One broken
        adapter must not abort an install for every other host.
  rc=2  a REQUIRED provisioning step failed — a host integration MPM
        builds (a compiled plugin payload and its registration) could
        not be produced. The install MUST NOT report success: the
        host is wired to an artifact that does not exist.
  rc=70 the reconciler itself failed (an uncaught internal error).
        The host integration state is unknown, so the install MUST NOT
        report success. Distinct from rc=1 so that the reconciler's own
        crash is never mistaken for an adapter failure.

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

    def _drive(self, body: str, *, errexit: bool = False) -> subprocess.CompletedProcess:
        driver = self.project / "_drive.sh"
        shell_opts = "set -euo pipefail" if errexit else "set -uo pipefail"
        driver.write_text(
            textwrap.dedent(f"""\
                #!/usr/bin/env bash
                {shell_opts}
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


    def test_phase_reports_internal_error_code(self):
        """A crash in the reconciler is neither an adapter failure (1)
        nor a required provisioning failure (2). It has its own code."""
        self._stage_reconciler(70)
        result = self._drive("phase_agent_reconcile")
        self.assertEqual(
            result.returncode, 70, result.stdout + result.stderr
        )

    def test_phase_does_not_swallow_provenance_failure_under_errexit(self):
        """The `phase || rc=$?` capture idiom disables errexit inside the
        function body. A failing inner step must be guarded explicitly,
        not rely on the caller's suppression."""
        self._stage_reconciler(0)
        result = self._drive(
            'export_build_provenance() { return 7; }; '
            'rc=0; phase_agent_reconcile || rc=$?; echo "rc=$rc"',
            errexit=True,
        )
        self.assertIn(
            "rc=7", result.stdout,
            "a failing export_build_provenance was swallowed because the "
            "phase relied on the caller's errexit suppression:\n"
            + result.stdout + result.stderr,
        )

    def test_internal_error_stops_the_install(self):
        """rc=70 must NOT be treated as an advisory warning."""
        self._stage_reconciler(70)
        result = self._drive(
            'rc=0; phase_agent_reconcile || rc=$?; '
            'outcome=0; reconcile_outcome "$rc" || outcome=$?; '
            'echo "outcome=$outcome"'
        )
        self.assertIn("outcome=70", result.stdout)
        self.assertNotIn("outcome=0", result.stdout)

    def test_mode_reconcile_uses_a_distinct_exit_code(self):
        """`--reconcile` must not exit 1, which the usage table reserves
        for "preflight failed"."""
        for reconciler_rc in (1, 2, 70):
            with self.subTest(reconciler_rc=reconciler_rc):
                self._stage_reconciler(reconciler_rc)
                result = self._drive(
                    'preflight() { return 0; }; '
                    'rc=0; mode_reconcile || rc=$?; echo "mode_rc=$rc"'
                )
                self.assertIn(
                    "mode_rc=6", result.stdout,
                    f"reconciler rc={reconciler_rc} did not map to the "
                    f"dedicated reconciliation-failed code 6:\n"
                    + result.stdout + result.stderr,
                )
                self.assertIn("RECONCILE COMPLETE", result.stderr)

    def test_mode_reconcile_still_exits_zero_on_success(self):
        self._stage_reconciler(0)
        result = self._drive(
            'preflight() { return 0; }; '
            'rc=0; mode_reconcile || rc=$?; echo "mode_rc=$rc"'
        )
        self.assertIn("mode_rc=0", result.stdout)


    def test_install_docs_do_not_claim_stderr_names_the_adapter(self):
        """Issue #72 item 3: the per-adapter lines are stdout, not stderr.

        The installer's own messages go to stderr, but the reconciler's
        per-adapter lines — the only place an adapter is named — are
        printed to stdout. The docs used to say the installer "names the
        adapter" while recommending stderr-only capture, which could not
        show it.
        """
        doc = (REPO_ROOT / "docs" / "INSTALL.md").read_text(encoding="utf-8")
        self.assertNotIn(
            "installer says so, names the adapter", doc,
            "the docs claim the stderr message names the adapter; it does not",
        )
        self.assertIn(
            "written to **stdout**", doc,
            "the docs must say where the adapter-naming lines are written",
        )


if __name__ == "__main__":
    unittest.main()
