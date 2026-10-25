"""
test_reconcile_error_classification.py — issue #71.

The generic reconciler (``reconcile_managed_blocks.py``) has three
outcomes that must not be conflated:

  rc=0   every adapter reconciled (or was safely skipped)
  rc=1   an adapter failed (advisory — the install continues)
  rc=2   a REQUIRED provisioning step failed (the install must not
         report success)

A crash in the reconciler itself is a FOURTH outcome. The script's own
failure says nothing about any adapter: it may have died before invoking
one. It used to surface as rc=1 — indistinguishable from an adapter
failure — so install.sh printed "one or more adapters reported non-zero
exit (continuing)", ran validation, printed INSTALL COMPLETE and exited 0
while the host integration state was unknown.

The distinction these tests pin:

  * a non-executable adapter installer names the adapter and says it
    could not be invoked (an adapter failure, rc=1), rather than dying
    with an uncaught PermissionError;
  * an uncaught internal exception exits 70, so the caller can treat it
    as fatal;
  * an ordinary adapter failure is still rc=1.

Each test builds a self-contained fake ``agent_installation/`` tree and
runs the REAL reconciler out of it, so discovery, argument resolution and
the subprocess boundary are exercised rather than mocked.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
RECONCILER = (
    REPO_ROOT / "agent_installation" / "scripts" / "reconcile_managed_blocks.py"
)


class _ReconcilerHarness(unittest.TestCase):
    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-reconcile-errors-"))
        self.addCleanup(lambda: shutil.rmtree(self._tmp, ignore_errors=True))
        self.home = self._tmp / "home"
        self.home.mkdir()
        self.agent_installation = self._tmp / "agent_installation"
        (self.agent_installation / "scripts").mkdir(parents=True)
        shutil.copy(
            RECONCILER, self.agent_installation / "scripts" / RECONCILER.name
        )
        self.adapter = self.agent_installation / "mpm-fakehost"
        self.adapter.mkdir()

    def _write_manifest(self, manifest) -> None:
        (self.adapter / "reconcile.json").write_text(
            json.dumps(manifest, indent=2), encoding="utf-8"
        )

    def _manifest(self) -> dict:
        return {
            "host_label": "fakehost",
            "description": "fake managed-block installer",
            "installer": "manage.sh",
            "installer_type": "bash",
            "args": [],
        }

    def _run(self) -> subprocess.CompletedProcess:
        env = os.environ.copy()
        env["HOME"] = str(self.home)
        return subprocess.run(
            [
                sys.executable,
                str(self.agent_installation / "scripts" / RECONCILER.name),
                "--home",
                str(self.home),
            ],
            capture_output=True,
            text=True,
            timeout=60,
            env=env,
        )


class InvocationFailureNamesTheAdapter(_ReconcilerHarness):
    def test_non_executable_installer_is_reported_as_an_adapter_failure(self):
        self._write_manifest(self._manifest())
        installer = self.adapter / "manage.sh"
        installer.write_text("#!/usr/bin/env bash\nexit 0\n")
        installer.chmod(0o644)  # exists, but cannot be executed

        result = self._run()
        combined = result.stdout + result.stderr

        self.assertEqual(
            result.returncode, 1,
            "a non-executable adapter installer is an adapter failure "
            "(rc=1), not an internal crash:\n" + combined,
        )
        self.assertIn(
            "fakehost", combined,
            "the failure must name the adapter whose installer could not "
            "be invoked:\n" + combined,
        )
        self.assertIn(
            "could not be invoked", combined,
            "the message must say the installer could not be invoked, not "
            "report a generic adapter failure:\n" + combined,
        )
        self.assertNotIn(
            "Traceback (most recent call last)", combined,
            "a predictable invocation failure must not escape as an "
            "uncaught Python exception:\n" + combined,
        )


class InternalCrashIsAFourthOutcome(_ReconcilerHarness):
    def test_uncaught_internal_error_exits_70(self):
        # Valid JSON, wrong shape: a manifest must be an object. This
        # currently raises AttributeError inside main().
        (self.adapter / "reconcile.json").write_text("[1, 2, 3]\n")

        result = self._run()
        combined = result.stdout + result.stderr

        self.assertEqual(
            result.returncode, 70,
            "an uncaught reconciler error must exit with its own code so "
            "the installer can treat it as fatal, not as an advisory "
            "adapter failure:\n" + combined,
        )
        self.assertIn(
            "internal error", result.stderr,
            "the crash must be labelled as the reconciler's own failure:\n"
            + combined,
        )
        self.assertNotIn(
            "reported non-zero exit", combined,
            "the reconciler crashed before it could establish that any "
            "adapter failed:\n" + combined,
        )


class AdapterFailureStillExitsOne(_ReconcilerHarness):
    def test_executable_adapter_that_fails_is_still_advisory(self):
        self._write_manifest(self._manifest())
        installer = self.adapter / "manage.sh"
        installer.write_text(
            "#!/usr/bin/env bash\necho 'managed block did not converge' >&2\n"
            "exit 3\n"
        )
        installer.chmod(0o755)

        result = self._run()

        self.assertEqual(
            result.returncode, 1,
            "an ordinary adapter failure must remain advisory (rc=1):\n"
            + result.stdout + result.stderr,
        )
        self.assertIn(
            "FAIL (rc=3)", result.stdout,
            "the adapter failure must still be reported with its status:\n"
            + result.stdout + result.stderr,
        )


if __name__ == "__main__":
    unittest.main()
