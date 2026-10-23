"""
test_uninstall_removal_truthfulness.py — Regression coverage for the
"removed:" lie in uninstall.sh (scripts audit, 2026-10-02).

Defect
------

  apply_plan steps 8 and 9 removed planned paths with:

      rm -rf -- "$f" 2>/dev/null || warn "could not fully remove $f"
      log "removed: $f"

  The `log "removed:"` was unconditional. `rm -rf` fails on paths it
  cannot unlink — a read-only parent directory, a sticky-bit directory,
  an immutable file, a mounted subvolume — so the script printed
  "removed: <path>" for a path that was still fully present.

  For --purge / --shred this is a false assurance about the operator's
  own explicitly-confirmed data destruction: the confirmation prompt
  states that memories, decisions, the database, and backups will be
  permanently destroyed, and a surviving mpm.db was reported as removed.

  The correct pattern already existed in the same file at step 10 (the
  install-prefix removal), which re-checked `[ ! -e "$target" ]` before
  claiming success. Steps 8/9 simply did not do it. The fix routes all
  three through a single `remove_path` helper.

Contract pinned here:

  1. A path that survives rm -rf is NOT reported as "removed:".
  2. It IS reported as still present (the operator must be told).
  3. The script exits 7 — the already-documented "a required removal
     step failed" code, which was previously declared but never used.
  4. Default-mode and --purge-mode both hold this (same helper).
  5. The successful-removal path is unchanged: exit 0, "removed:" logged.

Isolation
---------

  Every case runs uninstall.sh under `env -i` with a synthetic HOME
  under a tempdir. The live ~/.mpm is never referenced. The
  un-removable directory is produced with chmod 0500 on an inner
  directory, which fails unlink for a non-root user; the test skips
  when running as root, where that technique does not apply.
"""

from __future__ import annotations

import importlib
import os
import re  # noqa: F401  (used only if a future case needs re.search)
import shutil
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parents[2]
UNINSTALL = REPO_ROOT / "uninstall.sh"

# The mode the negative-control fixtures are locked to, and the mode
# they are restored to once the scenario is over.
#
# The restrictive mode is PART OF THE SCENARIO: it is what makes
# uninstall.sh's `rm -rf` fail, which is what the exit-code-7 and
# honest-reporting assertions are about. It therefore has to be in
# force for the whole duration of the uninstall invocation — restoring
# it earlier would quietly weaken the tests into passing vacuously.
#
# It is restored afterwards, before the runner removes the fixture,
# because a directory that cannot have its children unlinked also
# cannot be removed: the old `shutil.rmtree(..., ignore_errors=True)`
# silently left the whole `mpm-uninstall-truth.*` tree behind.
RESTRICTED_MODE = 0o500
RESTORED_MODE = 0o700


class FixtureCleanupError(RuntimeError):
    """Raised when a test-owned fixture could not be fully removed.

    A leak must never be mistaken for a pass: the previous cleanup
    swallowed every removal failure.
    """


def _restore_permissions(paths) -> tuple[list[Path], list[str]]:
    """Restore a writable mode on every path this module locked.

    Returns (restored, errors). The paths are restored to
    RESTORED_MODE so that the fixture can then be removed.
    """
    restored: list[Path] = []
    errors: list[str] = []
    for path in paths:
        try:
            path.chmod(RESTORED_MODE)
        except OSError as exc:
            errors.append(f"chmod {path}: {exc}")
            continue
        restored.append(path)
    return restored, errors


def _remove_fixture(root: Path, restricted=()) -> list[str]:
    """Restore the locked permissions, then remove the fixture.

    Returns the list of errors; an empty list means the fixture is
    gone. Only the test-owned fixture is touched.
    """
    _restored, errors = _restore_permissions(restricted)
    if not root.exists():
        return errors
    try:
        shutil.rmtree(root)
    except OSError as exc:
        errors.append(f"rmtree {root}: {exc}")
    if root.exists():
        errors.append(f"the fixture directory {root} still exists")
    return errors


def _force_remove(root: Path) -> None:
    """Best-effort removal for a fixture this module built DELIBERATELY
    to be hard to remove. Safety net only; never used for the fixture
    under test."""
    if not root.exists():
        return
    for dirpath, dirnames, filenames in os.walk(str(root), topdown=False):
        try:
            os.chmod(dirpath, 0o700)
        except OSError:
            pass
        for name in dirnames + filenames:
            entry = os.path.join(dirpath, name)
            if os.path.islink(entry):
                try:
                    os.unlink(entry)
                except OSError:
                    pass
    shutil.rmtree(root, ignore_errors=True)


def _running_as_root() -> bool:
    return hasattr(os, "geteuid") and os.geteuid() == 0


class UninstallRemovalTruthfulness(unittest.TestCase):
    def setUp(self) -> None:
        # The locked paths are tracked so the teardown can restore them.
        self._restricted: list[Path] = []
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-uninstall-truth."))
        # Registered before anything else can fail, so a partial setup
        # is covered too. The teardown restores the locked permissions
        # and then removes the fixture — it does NOT hide a failure.
        self.addCleanup(self._teardown)
        self.home = self.tmp / "home"
        (self.home / ".mpm").mkdir(parents=True)

    def _teardown(self) -> None:
        """Restore the locked permissions, then remove the fixture.

        The restrictive mode is what makes uninstall.sh's removal
        FAIL, so it stays in place for the whole uninstall invocation
        and the assertions that follow it. It is restored HERE — after
        the assertions, before the removal — because a directory whose
        children cannot be unlinked also cannot itself be removed.

        A removal failure is raised rather than swallowed: the old
        `shutil.rmtree(..., ignore_errors=True)` turned the leak into
        a silent pass.
        """
        errors = _remove_fixture(self.tmp, self._restricted)
        if errors:
            raise FixtureCleanupError(
                "could not remove the test fixture:\n  " + "\n  ".join(errors)
            )

    def _run(self, *args: str) -> subprocess.CompletedProcess:
        env = {
            "HOME": str(self.home),
            "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
            "USER": os.environ.get("USER", "tester"),
        }
        return subprocess.run(
            ["bash", str(UNINSTALL), *args],
            capture_output=True,
            text=True,
            env=env,
            timeout=120,
        )

    def _make_unremovable(self, path: Path) -> bool:
        """Populate `path` and make it impossible to unlink. False if unsupported."""
        path.mkdir(parents=True, exist_ok=True)
        (path / "app.log").write_text("sensitive log line\n")
        (path / "stub").mkdir(exist_ok=True)
        (path / "stub" / "mpm.db").write_text("row\n")
        (path / "stub").chmod(RESTRICTED_MODE)
        # Remembered so the teardown can restore it. The restriction is
        # part of the scenario and must survive the uninstall.
        self._restricted.append(path / "stub")
        return True

    # ---- the defect itself -------------------------------------------------

    @unittest.skipIf(_running_as_root(), "root bypasses the 0500 unlink restriction")
    def test_default_mode_does_not_claim_removed_when_path_survives(self):
        self._make_unremovable(self.home / ".mpm" / "logs")
        r = self._run("--yes")
        out = r.stdout + r.stderr
        self.assertNotIn(
            "removed: " + str(self.home / ".mpm" / "logs"),
            out,
            "uninstall.sh reported a surviving path as removed",
        )
        self.assertIn("still present after rm -rf", out)

    @unittest.skipIf(_running_as_root(), "root bypasses the 0500 unlink restriction")
    def test_default_mode_exits_7_on_failed_removal(self):
        self._make_unremovable(self.home / ".mpm" / "logs")
        r = self._run("--yes")
        self.assertEqual(
            r.returncode,
            7,
            f"expected exit 7 (documented: 'a required removal step failed'); "
            f"got {r.returncode}\n{r.stdout}\n{r.stderr}",
        )

    @unittest.skipIf(_running_as_root(), "root bypasses the 0500 unlink restriction")
    def test_purge_mode_does_not_claim_removed_when_db_survives(self):
        db_dir = self.home / ".mpm" / "src" / "db"
        self._make_unremovable(db_dir)
        r = self._run("--purge", "--yes")
        out = r.stdout + r.stderr
        self.assertNotIn(
            "removed: " + str(db_dir),
            out,
            "uninstall.sh --purge reported a surviving database directory as removed",
        )
        self.assertIn("still present after rm -rf", out)
        # The data really is still there — that is the point of the fix.
        self.assertTrue((db_dir / "stub" / "mpm.db").exists())

    @unittest.skipIf(_running_as_root(), "root bypasses the 0500 unlink restriction")
    def test_purge_mode_exits_7_on_failed_removal(self):
        self._make_unremovable(self.home / ".mpm" / "src" / "db")
        r = self._run("--purge", "--yes")
        self.assertEqual(r.returncode, 7, f"{r.stdout}\n{r.stderr}")

    # ---- negative controls: the success path is unchanged --------------------

    def test_ordinary_removal_still_reports_removed_and_exits_zero(self):
        logs = self.home / ".mpm" / "logs"
        logs.mkdir(parents=True)
        (logs / "app.log").write_text("ordinary log\n")
        r = self._run("--yes")
        out = r.stdout + r.stderr
        self.assertEqual(r.returncode, 0, f"{r.stdout}\n{r.stderr}")
        self.assertIn("removed: " + str(logs), out)
        self.assertNotIn("still present after rm -rf", out)
        self.assertFalse(logs.exists())

    def test_purge_mode_success_exits_zero(self):
        db_dir = self.home / ".mpm" / "src" / "db"
        db_dir.mkdir(parents=True)
        (db_dir / "mpm.db").write_text("row\n")
        r = self._run("--purge", "--yes")
        self.assertEqual(r.returncode, 0, f"{r.stdout}\n{r.stderr}")
        self.assertIn("removed: " + str(db_dir), r.stdout + r.stderr)

    def test_dry_run_performs_no_removal_and_exits_zero(self):
        logs = self.home / ".mpm" / "logs"
        logs.mkdir(parents=True)
        r = self._run("--dry-run")
        self.assertEqual(r.returncode, 0, f"{r.stdout}\n{r.stderr}")
        self.assertTrue(logs.exists(), "dry-run mutated state")
        # Dry-run returns from main() before apply_plan, so the contract is
        # the printed plan plus the explicit no-mutation line — not the
        # per-path "[dry-run] would remove" lines, which are never reached.
        self.assertIn("dry-run: no mutations performed", r.stdout)
        self.assertIn(str(logs), r.stdout)


class UninstallRemovalHelperContract(unittest.TestCase):
    """Static invariants on the helper, so a future edit cannot silently
    reintroduce the unconditional success log."""

    def setUp(self) -> None:
        self.src = UNINSTALL.read_text()

    def test_remove_path_helper_exists(self):
        self.assertIn("remove_path()", self.src)

    def test_no_unconditional_removed_log_after_swallowed_rm(self):
        # The defect shape: `rm ... || warn` immediately followed by an
        # unconditional `log "removed: $f"`. Allow `remove_path`'s own
        # guarded form; forbid the old bare pairing in the plan loops.
        self.assertNotRegex(
            self.src,
            r'rm -rf -- "\$f"[^\n]*\|\| warn[^\n]*\n\s*log "removed: \$f"',
        )

    def test_exit_code_7_is_documented(self):
        # Inline (?m) — assertRegex's third positional arg is `msg`, not flags.
        self.assertRegex(self.src, r"(?m)^#\s+7\s+a required removal step failed")


class UninstallFixtureLifecycle(unittest.TestCase):
    """The fixture must not survive the test that created it.

    The negative controls deliberately lock a nested directory to 0o500.
    That restriction is the SCENARIO — it is what makes uninstall.sh's
    removal fail — and it is also what used to defeat the runner's own
    teardown, leaving a `mpm-uninstall-truth.*` tree behind on every
    run. These tests pin both halves: the restriction stays in force
    during the uninstall, and the fixture is gone afterwards.
    """

    def _make_locked_fixture(self) -> tuple[Path, Path]:
        """A fixture shaped like the negative controls'."""
        root = Path(tempfile.mkdtemp(prefix="mpm-uninstall-truth."))
        self.addCleanup(_force_remove, root)
        logs = root / "home" / ".mpm" / "logs"
        logs.mkdir(parents=True)
        (logs / "app.log").write_text("sensitive log line\n")
        stub = logs / "stub"
        stub.mkdir()
        (stub / "mpm.db").write_text("row\n")
        stub.chmod(RESTRICTED_MODE)
        return root, stub

    @unittest.skipIf(
        _running_as_root(), "root bypasses the 0500 unlink restriction"
    )
    def test_a_locked_fixture_cannot_be_removed_while_it_is_locked(self):
        """While the restriction is in force the tree cannot be
        removed: unlinking a file needs write permission on its parent
        directory, and the parent has none.

        Root can bypass the restriction, so this check is skipped there
        — the same convention the negative-control uninstall cases
        already use.
        """
        root, stub = self._make_locked_fixture()
        self.assertEqual(stat.S_IMODE(stub.stat().st_mode), RESTRICTED_MODE)
        with self.assertRaises(OSError):
            shutil.rmtree(root)
        self.assertTrue(
            root.exists(),
            "the fixture must survive while it is locked — otherwise the "
            "negative controls would be testing nothing",
        )

    def test_the_locked_fixture_check_is_skipped_when_running_as_root(self):
        """The root guard is verified WITHOUT becoming root.

        Root bypasses the 0o500 unlink restriction, so the locked
        fixture survives removal there and the assertion above cannot
        hold. The guard is therefore proven by re-importing this module
        with `os.geteuid` patched to 0 — the decorator is then
        evaluated under exactly the condition it has to handle. No
        privilege escalation and no installer run is involved.
        """
        module = importlib.import_module(__name__)
        method_name = "test_a_locked_fixture_cannot_be_removed_while_it_is_locked"

        # The guard's predicate must follow the effective uid.
        with mock.patch.object(os, "geteuid", return_value=0):
            self.assertTrue(module._running_as_root())
        with mock.patch.object(os, "geteuid", return_value=1000):
            self.assertFalse(module._running_as_root())

        # Re-import under a simulated root so `skipIf` is evaluated
        # with the predicate it will see in production.
        with mock.patch.object(os, "geteuid", return_value=0):
            reloaded = importlib.reload(module)
        try:
            method = getattr(
                reloaded.UninstallFixtureLifecycle, method_name
            )
            self.assertTrue(
                getattr(method, "__unittest_skip__", False),
                "the locked-fixture check must be skipped when running as root",
            )
            self.assertIn(
                "root",
                getattr(method, "__unittest_skip_why__", ""),
                "the skip must say that root bypasses the restriction",
            )
        finally:
            # Restore the module to the real (non-root) decorator state
            # so the rest of the suite is unaffected by the reload.
            importlib.reload(module)

    def test_the_negative_control_exit_code_checks_are_still_present(self):
        """The root guard must not have weakened the negative controls.

        The exit-code-7 assertions are what make the uninstall
        truthfulness contract load-bearing. They are unchanged, but a
        guard test that only ever added skips could quietly drop them,
        so their presence is pinned here.
        """
        module = importlib.import_module(__name__)
        for name in (
            "test_default_mode_exits_7_on_failed_removal",
            "test_purge_mode_exits_7_on_failed_removal",
            "test_default_mode_does_not_claim_removed_when_path_survives",
            "test_purge_mode_does_not_claim_removed_when_db_survives",
        ):
            self.assertTrue(
                hasattr(module.UninstallRemovalTruthfulness, name),
                f"the negative control {name} must remain in place",
            )
        self.assertRegex(
            module.UNINSTALL.read_text(),
            r"(?m)^#\s+7\s+a required removal step failed",
            "uninstall.sh must still document exit code 7",
        )

    def test_the_restricted_mode_is_restored_before_the_removal(self):
        """The permissions are restored explicitly, and the restored
        mode is a writable one."""
        _root, stub = self._make_locked_fixture()
        restored, errors = _restore_permissions([stub])
        self.assertEqual(errors, [])
        self.assertEqual(restored, [stub])
        self.assertEqual(stat.S_IMODE(stub.stat().st_mode), RESTORED_MODE)

    def test_the_fixture_is_removed_after_the_permissions_are_restored(self):
        root, stub = self._make_locked_fixture()
        errors = _remove_fixture(root, [stub])
        self.assertEqual(errors, [])
        self.assertFalse(root.exists(), "the fixture must be gone")

    @unittest.skipIf(
        _running_as_root(), "root ignores the write-permission restriction"
    )
    def test_a_fixture_that_cannot_be_removed_is_reported(self):
        """A removal that cannot succeed must be REPORTED, with its
        path — never swallowed into a silent pass.

        The obstacle is the fixture's PARENT: it is not writable, so
        the fixture cannot be unlinked from it, and no chmod of the
        fixture itself can change that.
        """
        base = Path(tempfile.mkdtemp(prefix="mpm-uninstall-truth-locked."))
        self.addCleanup(_force_remove, base)
        fixture = base / "fixture"
        fixture.mkdir()
        (fixture / "file.txt").write_text("x\n")
        os.chmod(base, 0o500)

        errors = _remove_fixture(fixture, [])

        self.assertTrue(
            errors, "a removal that cannot succeed must be reported"
        )
        self.assertTrue(
            any(str(fixture) in err for err in errors),
            f"the error must name the path that could not be removed: {errors}",
        )
        self.assertTrue(fixture.exists())

    def test_the_test_case_teardown_removes_its_fixture(self):
        """End to end: a real test method runs, and the fixture it
        created is gone afterwards."""
        case = UninstallRemovalTruthfulness(
            "test_dry_run_performs_no_removal_and_exits_zero"
        )
        result = unittest.TestResult()
        case.run(result)
        self.assertEqual(len(result.errors), 0, result.errors)
        self.assertEqual(len(result.failures), 0, result.failures)
        self.assertFalse(
            case.tmp.exists(), "the fixture must not survive the test"
        )

    @unittest.skipIf(
        _running_as_root(), "root bypasses the 0500 unlink restriction"
    )
    def test_a_locked_fixture_does_not_survive_the_test_that_created_it(self):
        """THE regression.

        The negative-control fixture is locked, the test passes, and the
        fixture is still gone. With the old
        `shutil.rmtree(..., ignore_errors=True)` teardown the locked
        directory defeated the removal and the whole
        `mpm-uninstall-truth.*` tree survived the run.
        """
        case = UninstallRemovalTruthfulness(
            "test_default_mode_exits_7_on_failed_removal"
        )
        result = unittest.TestResult()
        case.run(result)
        self.assertEqual(len(result.errors), 0, result.errors)
        self.assertEqual(len(result.failures), 0, result.failures)
        self.assertFalse(
            case.tmp.exists(),
            "the locked fixture must not survive the test that created it",
        )


if __name__ == "__main__":
    unittest.main()
