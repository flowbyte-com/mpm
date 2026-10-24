"""
test_install_completion_reporting.py — Regression coverage for the
installer's completion reporting (HAL-9001 Finding 6).

--------------------------------------------------------------------------
The defect
--------------------------------------------------------------------------

An operator's full-install transcript stopped at the final host-adapter
reconciliation entry. It never printed ``VALIDATION`` or
``INSTALL COMPLETE``, and it printed no error of any kind. Two
independent defects in the installer produced that single symptom, and
both had to be corrected for the report to be trustworthy again.

Defect A — the promotion library destroyed the installer's stderr.

``scripts/lib/binary_transaction.sh`` closed its lock file with::

    exec 9>&- 2>/dev/null || true

``exec`` with no command applies its redirections to the SHELL, not to
the command. So the ``2>/dev/null`` — written to silence a possible
"bad file descriptor" complaint from closing fd 9 — instead pointed the
shell's stderr at ``/dev/null`` for the REST OF THE SCRIPT.

Every later installer message goes to stderr (``log``, ``warn``, ``err``,
``die`` and ``note`` all use ``>&2``). After ``phase_binaries`` ran, the
installer's own output was therefore discarded wholesale: the
``VALIDATION`` banner, the ``INSTALL COMPLETE`` banner, every warning,
and — critically — every FATAL error message. A failing validation
exited 5 saying nothing at all.

``install.sh`` and the Makefile's ``install:`` target share this
library, so the Makefile's post-promotion failure report was lost the
same way.

Defect B — a non-fatal adapter warning was a fatal exit.

``phase_agent_reconcile`` returns non-zero when a host adapter fails,
and its own documentation says that is a WARNING: "A failure in one
adapter is logged as WARN; the install continues for the remaining
adapters." But ``mode_install`` called it bare::

    phase_agent_reconcile

under ``set -euo pipefail``. An uncaptured non-zero return aborts the
script at the call site, so the install stopped immediately after the
reconciliation — before validation, before the completion banner. The
documented non-fatal condition was a silent fatal exit.

--------------------------------------------------------------------------
The reproductions
--------------------------------------------------------------------------

Both defects were reproduced before being fixed, in a hermetic tree
that touches nothing outside its own temporary directory.

Observed at baseline (``aaa1540``) for the combination — real
``phase_binaries``, one failing adapter::

    [mpm-install] ===== BINARIES =====
    [mpm-install]   promotion committed: 5 binaries
    [reconcile]  mpm-pi: FAIL (rc=1); stderr=stub adapter failure
    === EXIT CODE: 1 ===

That is the operator's transcript exactly: the last visible line is the
final adapter entry, there is no ``VALIDATION``, no ``INSTALL
COMPLETE``, and no error message — because the install aborted (Defect
B) and its error text had already been discarded (Defect A).

Observed after the fixes, same harness::

    [mpm-install] WARN:   one or more adapters reported non-zero exit (continuing)
    [mpm-install] WARN:   agent adapter reconciliation reported warnings (non-fatal).
    [mpm-install] ===== VALIDATION =====
    [mpm-install] validation passed
    [mpm-install] ===== INSTALL COMPLETE =====
    === EXIT CODE: 0 ===

--------------------------------------------------------------------------
Scope
--------------------------------------------------------------------------

Nothing here touches a live workspace, a database, a service, or the
network. The installer is driven through a stripped copy with every
host-touching phase stubbed, and the promotion library is exercised
against fake binaries in a temporary directory.
"""

import os
import re
import shutil
import stat
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
INSTALL_SH = REPO_ROOT / "install.sh"
PROMOTION_LIB = REPO_ROOT / "scripts" / "lib" / "binary_transaction.sh"

# Markers in install.sh that prevent the script from running main()
# during test sourcing, and that would otherwise pin PROJECT_ROOT to
# the test's own location. Same convention as
# scripts/tests/test_install_phase_binaries.py.
STRIP_PATTERNS = (
    re.compile(r"^\s*readonly\s+SCRIPT_DIR\s*="),
    re.compile(r"^\s*readonly\s+PROJECT_ROOT\s*="),
    re.compile(r'^\s*main\s+"\$@"\s*$'),
)

BINARIES = ("mpm", "mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry")

# A marker written to stderr AFTER the thing under test has run. If
# the marker is absent from stderr, the process lost stderr and every
# message the caller would have printed afterwards was discarded.
STDERR_MARKER = "MPM-STDERR-STILL-WORKS-9d1f"


def strip_for_sourcing(install_text: str) -> str:
    """Return install.sh with path-resolution and main() stripped, so it
    can be sourced for isolated testing."""
    out = []
    for line in install_text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out.append(line)
    return "\n".join(out) + "\n"


class _StdErrSurvivalBase(unittest.TestCase):
    """Shared scaffolding for the shell-driving tests."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-install-report-"))
        self.addCleanup(shutil.rmtree, self._tmp, ignore_errors=True)

    def _child_env(self, home: Path, extra: dict | None = None) -> dict:
        env = os.environ.copy()
        env["HOME"] = str(home)
        # Keep the child hermetic and quiet about the operator's own
        # environment: no inherited MPM_WORKSPACE, no inherited build
        # provenance, and a PATH that puts our own stubs first.
        for key in ("MPM_WORKSPACE", "MPM_INSTALL_TEST_FAIL_AFTER",
                    "MPM_BUILD_VERSION", "MPM_BUILD_COMMIT"):
            env.pop(key, None)
        if extra:
            env.update(extra)
        return env


class PromotionPreservesStderr(_StdErrSurvivalBase):
    """Defect A — the promotion library must not swallow the caller's
    stderr."""

    def _run_promotion(self, fail_after: str | None = None) -> subprocess.CompletedProcess:
        build = self._tmp / "build"
        bindir = self._tmp / "prefix" / "bin"
        build.mkdir(parents=True)
        bindir.mkdir(parents=True)
        for name in BINARIES:
            f = build / name
            f.write_text(f"#!/bin/sh\necho 'fake {name}'\n")
            f.chmod(0o755)

        driver = self._tmp / "drive.sh"
        driver.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            set -uo pipefail
            . {PROMOTION_LIB}
            mpm_promote_binaries "{build}" "{bindir}"
            rc=$?
            echo "PROMOTION_RC=$rc" >&2
            echo "{STDERR_MARKER}" >&2
            exit 0
        """))
        driver.chmod(0o755)

        env = self._child_env(self._tmp)
        if fail_after is not None:
            env["MPM_INSTALL_TEST_FAIL_AFTER"] = fail_after
        return subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver)],
            capture_output=True, text=True, env=env,
        )

    def test_a_successful_promotion_leaves_stderr_usable(self):
        result = self._run_promotion()
        self.assertIn(
            STDERR_MARKER, result.stderr,
            "stderr was destroyed by a successful binary promotion: every "
            "installer message after phase_binaries would be discarded.\n"
            f"stderr: {result.stderr!r}\nstdout: {result.stdout!r}",
        )

    def test_a_failed_promotion_leaves_stderr_usable(self):
        # The failure report the caller prints AFTER promotion returns
        # must be visible. Before the fix this was lost too.
        result = self._run_promotion(fail_after="2")
        self.assertIn(
            STDERR_MARKER, result.stderr,
            "stderr was destroyed by a FAILED binary promotion: the "
            "caller's failure report was discarded, so a failed promotion "
            "announced itself with nothing but an exit code.\n"
            f"stderr: {result.stderr!r}\nstdout: {result.stdout!r}",
        )

    def test_the_unlock_does_not_redirect_the_shells_stderr(self):
        # A direct, minimal assertion on the defect itself, so a future
        # rewrite cannot reintroduce it in a different shape.
        (self._tmp / "prefix" / "bin").mkdir(parents=True)
        driver = self._tmp / "unlock.sh"
        driver.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            set -uo pipefail
            . {PROMOTION_LIB}
            _bt_lock "{self._tmp / 'prefix' / 'bin'}" || exit 1
            _bt_unlock
            echo "{STDERR_MARKER}" >&2
        """))
        driver.chmod(0o755)
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver)],
            capture_output=True, text=True, env=self._child_env(self._tmp),
        )
        self.assertIn(
            STDERR_MARKER, result.stderr,
            "_bt_unlock redirected the shell's stderr to /dev/null for the "
            "rest of the script.\n"
            f"stderr: {result.stderr!r}\nstdout: {result.stdout!r}",
        )


class InstallCompletionReporting(_StdErrSurvivalBase):
    """Defects A and B as the operator sees them: the install's own
    report."""

    def setUp(self):
        super().setUp()
        self.home = self._tmp / "home"
        self.home.mkdir()
        self.project = self._tmp / "project"
        (self.project / "scripts" / "lib").mkdir(parents=True)
        (self.project / "agent_installation" / "scripts").mkdir(parents=True)
        # The REAL promotion library, so the test exercises the shipped
        # code rather than a copy of it.
        shutil.copy2(PROMOTION_LIB, self.project / "scripts" / "lib" / "binary_transaction.sh")
        # A stripped install.sh, so it can be sourced.
        (self.project / "install.sh.lib").write_text(
            strip_for_sourcing(INSTALL_SH.read_text(encoding="utf-8"))
        )
        self.prefix = self._tmp / "prefix"
        self.data_root = self._tmp / "data"
        self.data_root.mkdir()
        (self.data_root / "scheduler.lock").write_text("")

        # A stub `systemctl` so phase_validate's service probe cannot
        # reach the operator's real user bus.
        stub_bin = self._tmp / "stubbin"
        stub_bin.mkdir()
        systemctl = stub_bin / "systemctl"
        systemctl.write_text("#!/bin/sh\nexit 0\n")
        systemctl.chmod(0o755)

        # A stub `mpm` that satisfies phase_validate's health_check and
        # read_directives probes. When `mpm_ok` is False the health
        # check fails, which is how the fatal-validation case is built.
        build = self.project / ".build" / "bin"
        build.mkdir(parents=True)
        for name in BINARIES:
            if name == "mpm":
                f = build / name
                f.write_text(textwrap.dedent(f"""\
                    #!/bin/sh
                    for a in "$@"; do
                      case "$a" in
                        *health_check*) echo '{{"ok":true}}'; exit 0 ;;
                        *read_directives*) echo '{{"count":5}}'; exit 0 ;;
                      esac
                    done
                    echo '{{"ok":true}}'
                """))
            else:
                f = build / name
                f.write_text(f"#!/bin/sh\necho 'fake {name}'\n")
            f.chmod(0o755)

    def _write_reconcile_stub(self, exit_code: int) -> None:
        verdict = "ok" if exit_code == 0 else f"FAIL (rc={exit_code})"
        script = self.project / "agent_installation" / "scripts" / "reconcile_managed_blocks.py"
        script.write_text(textwrap.dedent(f"""\
            import sys
            print("[reconcile]  mpm-pi: {verdict}; stderr=stub")
            sys.exit({exit_code})
        """))

    def _drive(self, body: str, extra_env: dict | None = None) -> subprocess.CompletedProcess:
        driver = self.project / "_drive.sh"
        driver.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            set -uo pipefail
            export PROJECT_ROOT={self.project}
            source {self.project}/install.sh.lib
            PREFIX={self.prefix}
            DATA_ROOT={self.data_root}
            SERVICE_DST=/dev/null
            BUILD_DIR={self.project}/.build/bin
            export PREFIX DATA_ROOT SERVICE_DST BUILD_DIR

            # Every host-touching phase is stubbed. phase_binaries and
            # phase_agent_reconcile are the REAL ones: they are where
            # the two defects live.
            preflight() {{ :; }}
            phase_build() {{ :; }}
            phase_symlinks() {{ :; }}
            phase_data_dir() {{ :; }}
            phase_runtime_assets() {{ :; }}
            phase_service() {{ :; }}
            {body}
        """))
        driver.chmod(0o755)

        env = self._child_env(
            self.home,
            {"PATH": f"{self._tmp / 'stubbin'}:{os.environ['PATH']}"},
        )
        if extra_env:
            env.update(extra_env)
        return subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver)],
            capture_output=True, text=True, env=env,
        )

    # ------------------------------------------------------------------
    # Defect B — the non-fatal adapter warning
    # ------------------------------------------------------------------
    def test_a_failing_adapter_does_not_abort_the_install(self):
        """A failing host adapter is a WARNING. The install must still
        reach validation and print its completion banner."""
        self._write_reconcile_stub(1)
        result = self._drive("mode_install\n")
        combined = result.stdout + result.stderr

        self.assertIn(
            "VALIDATION", combined,
            "the install never reached validation: a failing adapter "
            "aborted it. The adapter failure is documented as non-fatal.\n"
            f"stderr: {result.stderr!r}\nstdout: {result.stdout!r}",
        )
        self.assertIn(
            "INSTALL COMPLETE", combined,
            "the install never printed its completion banner.\n"
            f"stderr: {result.stderr!r}\nstdout: {result.stdout!r}",
        )
        self.assertEqual(
            result.returncode, 0,
            "the install exited non-zero because of a non-fatal adapter "
            f"warning.\nstderr: {result.stderr!r}\nstdout: {result.stdout!r}",
        )
        self.assertIn(
            "non-fatal", combined,
            "the adapter failure was not reported as the non-fatal "
            f"condition it is.\nstderr: {result.stderr!r}",
        )

    def test_a_successful_adapter_reconciliation_completes_the_install(self):
        self._write_reconcile_stub(0)
        result = self._drive("mode_install\n")
        combined = result.stdout + result.stderr
        self.assertIn("VALIDATION", combined, combined)
        self.assertIn("INSTALL COMPLETE", combined, combined)
        self.assertEqual(result.returncode, 0, result.stderr)

    # ------------------------------------------------------------------
    # Defect A — the swallowed stderr, seen through the installer
    # ------------------------------------------------------------------
    def test_a_successful_install_prints_its_completion_banner(self):
        """The completion banner goes to stderr. If the promotion
        swallowed stderr, the banner was invisible even though the
        install had succeeded."""
        self._write_reconcile_stub(0)
        result = self._drive("mode_install\n")
        self.assertIn(
            "INSTALL COMPLETE", result.stderr,
            "the installer's completion banner never reached stderr: the "
            "promotion library destroyed stderr for the rest of the "
            f"script.\nstderr: {result.stderr!r}\nstdout: {result.stdout!r}",
        )

    def test_a_fatal_validation_failure_is_reported_and_stays_fatal(self):
        """The fatal/non-fatal distinction must survive: a failed
        validation is still a failed install, AND it must say so."""
        # Break the health_check probe so phase_validate's check fails.
        mpm = self.project / ".build" / "bin" / "mpm"
        mpm.write_text("#!/bin/sh\necho '{\"ok\":false}'\n")
        mpm.chmod(0o755)
        self._write_reconcile_stub(0)

        result = self._drive("mode_install\n")
        combined = result.stdout + result.stderr

        self.assertNotIn(
            "INSTALL COMPLETE", combined,
            "the install reported completion even though validation "
            f"failed.\nstderr: {result.stderr!r}",
        )
        self.assertIn(
            "validation failed", combined,
            "the fatal validation error was not reported. It was written "
            "to stderr, and stderr had been destroyed by the promotion.\n"
            f"stderr: {result.stderr!r}\nstdout: {result.stdout!r}",
        )
        self.assertNotEqual(
            result.returncode, 0,
            "a failed validation must remain a failed install.",
        )

    # ------------------------------------------------------------------
    # The dedicated reconcile mode
    # ------------------------------------------------------------------
    def test_the_reconcile_mode_reports_completion_and_fails_loudly(self):
        """--reconcile exists to reconcile. A failing adapter must not
        abort the mode silently before its completion banner, and the
        exit status must still say the reconciliation was incomplete."""
        self._write_reconcile_stub(1)
        result = self._drive("mode_reconcile\n")
        combined = result.stdout + result.stderr
        self.assertIn(
            "RECONCILE COMPLETE", combined,
            "the reconcile mode aborted before reporting completion.\n"
            f"stderr: {result.stderr!r}\nstdout: {result.stdout!r}",
        )
        self.assertNotEqual(
            result.returncode, 0,
            "the reconcile mode exited 0 even though an adapter failed.",
        )


class InstallerSourceContract(unittest.TestCase):
    """Static guards on the two shapes that regressed."""

    def setUp(self):
        self.source = INSTALL_SH.read_text(encoding="utf-8")

    def test_mode_install_captures_the_reconcile_return_code(self):
        """An uncaptured non-zero return from phase_agent_reconcile
        aborts the script under `set -e`. The call must be guarded."""
        # (?m) so ^ and $ anchor per LINE. Without it, ^ matches only
        # at the start of the whole file, the pattern cannot reach the
        # call site, and this check passes vacuously — a silent-pass
        # test is worse than no test.
        self.assertNotRegex(
            self.source,
            r"(?m)^[ \t]*phase_agent_reconcile[ \t]*$",
            "phase_agent_reconcile is called bare; under `set -e` its "
            "non-zero return aborts the install before validation.",
        )
        self.assertRegex(
            self.source,
            r"phase_agent_reconcile[ \t]*\|\|[ \t]*\w+=\$\?",
            "the reconcile return code must be captured, not left to "
            "errexit.",
        )

    def test_the_promotion_library_does_not_redirect_the_shells_stderr(self):
        lib = PROMOTION_LIB.read_text(encoding="utf-8")
        self.assertNotRegex(
            lib,
            r"(?m)^[ \t]*exec[ \t]+9>&-[ \t]*2>/dev/null",
            "`exec 9>&- 2>/dev/null` applies the stderr redirection to "
            "the SHELL, silencing every installer message for the rest "
            "of the script.",
        )


if __name__ == "__main__":
    unittest.main()
