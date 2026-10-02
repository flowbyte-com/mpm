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

import os
import re  # noqa: F401  (used only if a future case needs re.search)
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
UNINSTALL = REPO_ROOT / "uninstall.sh"


def _running_as_root() -> bool:
    return hasattr(os, "geteuid") and os.geteuid() == 0


class UninstallRemovalTruthfulness(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-uninstall-truth."))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.home = self.tmp / "home"
        (self.home / ".mpm").mkdir(parents=True)

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
        (path / "stub").chmod(0o500)
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


if __name__ == "__main__":
    unittest.main()
