#!/usr/bin/env python3
"""Makefile `install:` promotion-failure report — issue #70.

The recipe body runs under `bash -e`. It used to call
``mpm_promote_binaries`` bare and then capture ``$?``:

    mpm_promote_binaries ...;
    _rc=$?;
    if [ $_rc -ne 0 ]; then ... fi;

Under errexit the shell aborts at the failing call, so the ``_rc`` capture
and the whole tailored failure report are unreachable. A failed promotion
is still reported by the transaction library itself, but the recipe's own
diagnosis never appears.

These tests extract the REAL recipe body from the Makefile, substitute the
sandbox paths the way make would, and execute it. They do not run `make
install` (which would build and touch a prefix); they run only the
post-promotion shell segment under test.

Everything is staged in a temporary directory. No live prefix, workspace,
database or service is touched.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
MAKEFILE = REPO_ROOT / "Makefile"

BINARIES = ("mpm", "mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry")


def extract_install_recipe(makefile_text: str) -> str:
    """Return the `bash -e -c '...'` body of the install: target.

    The body is single-quoted in the Makefile and contains no single
    quote of its own, so the closing quote is the next ``'`` at end of
    line.
    """
    match = re.search(r"@bash -e -c '(.*?)'\n", makefile_text, re.DOTALL)
    if not match:
        raise AssertionError("could not locate the install recipe body")
    return match.group(1)


class InstallPromotionFailureReport(unittest.TestCase):
    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-make-install-report-"))
        self.addCleanup(lambda: shutil.rmtree(self._tmp, ignore_errors=True))
        self.build = self._tmp / "build"
        self.prefix = self._tmp / "prefix"
        self.bindir = self.prefix / "bin"
        self.build.mkdir()
        self.bindir.mkdir(parents=True)

        for name in BINARIES:
            candidate = self.build / name
            candidate.write_text(f"#!/usr/bin/env bash\necho NEW-{name}\n")
            candidate.chmod(0o755)
            installed = self.bindir / name
            installed.write_text(f"#!/usr/bin/env bash\necho OLD-{name}\n")
            installed.chmod(0o755)

        self.recipe = extract_install_recipe(MAKEFILE.read_text(encoding="utf-8"))
        # make expands its own variables and collapses `$$` to `$` before
        # handing the recipe to the shell. Do the same, for the two paths
        # this recipe uses.
        self.script = (
            self.recipe
            .replace("$(BUILD_DIR)", str(self.build))
            .replace("$(PREFIX)", str(self.prefix))
            .replace("$$", "$")
        )

    def _run(self, *, fail_after: int | None = None,
             break_restoration: bool = False) -> subprocess.CompletedProcess:
        env = os.environ.copy()
        env["HOME"] = str(self._tmp)
        env["PATH"] = "/usr/bin:/bin"
        if fail_after is not None:
            env["MPM_INSTALL_TEST_FAIL_AFTER"] = str(fail_after)
        if break_restoration:
            stub = self._tmp / "stub"
            stub.mkdir(exist_ok=True)
            (stub / "mv").write_text(
                "#!/usr/bin/env bash\n"
                'src=""\n'
                'for a in "$@"; do case "$a" in -*) continue;; esac; '
                'src="$a"; break; done\n'
                'case "$src" in *.mpm-rollback.*/*) exit 1;; esac\n'
                'exec /usr/bin/mv "$@"\n'
            )
            (stub / "mv").chmod(0o755)
            env["PATH"] = f"{stub}:/usr/bin:/bin"
        return subprocess.run(
            ["bash", "-e", "-c", self.script],
            cwd=REPO_ROOT, env=env, capture_output=True, text=True, timeout=120,
        )

    def test_recipe_body_is_the_transaction_not_a_reimplementation(self):
        self.assertIn("mpm_promote_binaries", self.script)
        self.assertIn(". scripts/lib/binary_transaction.sh", self.script)
        self.assertTrue(self.script.strip())

    def test_successful_promotion_reports_the_deployment(self):
        result = self._run()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("(deployed ", result.stdout)
        self.assertNotIn("FAIL: transactional binary promotion", result.stdout
                         + result.stderr)

    def test_failed_promotion_prints_the_failure_report_exactly_once(self):
        result = self._run(fail_after=2)
        self.assertNotEqual(result.returncode, 0,
                            "a failed promotion reported success")
        combined = result.stdout + result.stderr
        marker = "FAIL: transactional binary promotion did not complete (rc=10)"
        self.assertEqual(
            combined.count(marker), 1,
            "the recipe's post-promotion failure report did not run (or ran "
            "more than once). Under `bash -e` a bare mpm_promote_binaries "
            "aborts before the rc capture, making the report dead code.\n"
            f"combined output:\n{combined}",
        )

    def test_incomplete_rollback_report_does_not_claim_restoration(self):
        result = self._run(fail_after=2, break_restoration=True)
        self.assertNotEqual(result.returncode, 0)
        combined = result.stdout + result.stderr
        self.assertIn(
            "INCOMPLETE", combined,
            "the Makefile report must distinguish an incomplete rollback "
            "(rc=11) from a clean one (rc=10)",
        )
        self.assertNotIn(
            "has been restored", combined,
            "the Makefile claimed the previous set was restored even though "
            "the transaction reported an incomplete rollback",
        )


if __name__ == "__main__":
    unittest.main()
