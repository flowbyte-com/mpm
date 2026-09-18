"""
test_uninstall_self_staging.py — Behavioral regression coverage for the
REAL destructive self-staging / self-removal / shred end-to-end path.

This test executes the actual ``uninstall.sh --shred --yes`` against a
freshly-created disposable production-shaped HOME. It is the canonical
proof that, in the canonical production topology where PROJECT_ROOT ==
PREFIX == ~/.mpm, the uninstaller:

  - stages itself off-tree BEFORE deletion of the install prefix,
  - secure-overwrites every regular file beneath the three sensitive
    roots AND the standalone config / env files,
  - removes the original install prefix,
  - cleans up its own stage directory,
  - leaves everything OUTSIDE the MPM tree untouched.

The test does NOT touch ``/home/v/.mpm`` or ``~/workspace/projects/mpm``.
It hard-codes a disposable TMPHOME under ``/tmp`` and asserts that
TMPHOME is not the real ``$HOME``.

A ``shred`` wrapper is placed early on PATH so every invocation of GNU
shred is logged to a file OUTSIDE the disposable MPM root. The wrapper
delegates to the real ``/usr/bin/shred`` so the actual secure-overwrite
semantics are exercised (the test does not fake shredding).
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
UNINSTALL_SH = REPO_ROOT / "uninstall.sh"


def _read(path: Path) -> str:
    return path.read_text(encoding="utf-8", errors="ignore")


class TestUninstallSelfStaging(unittest.TestCase):
    """REAL destructive --shred --yes with PROJECT_ROOT == PREFIX.

    Disposable production-shaped HOME under ``/tmp``; never the real
    ``$HOME`` (asserted). Verifies the canonical shred contract
    end-to-end: stage-self -> shred -> remove original tree -> clean
    stage dir -> preserve outside targets.
    """

    # The seven canary / sensitive files we expect to see in the shred log.
    EXPECTED_SHRED_TARGETS = (
        "src/db/shred-test/future-secret.xyz",
        "backups/shred-test/random-copy.bin",
        "logs/shred-test/new-format.whatever",
        "src/db/a/b/c/future-format.random-extension",
        "mpm_config.json",
        ".config/mpm/mpm.env",
    )

    # Files that MUST NOT be shredded (external target + symlink that
    # only points at it).
    EXPECTED_NOT_SHREDDED = (
        "outside-shred-target.txt",
        "home-survival-sentinel.txt",
    )

    def setUp(self):
        if UNINSTALL_SH is None or not UNINSTALL_SH.exists():
            self.skipTest("uninstall.sh not found")
        # Hard isolation: TMPHOME must NOT be the real $HOME, must NOT
        # be inside any real /home/v tree, and must NOT be inside the
        # dev repo.
        real_home = os.environ.get("HOME", "")
        assert real_home, "test environment must define $HOME"
        self.real_home = real_home

        self.tmp_root = Path(tempfile.mkdtemp(prefix="mpm-self-staging-"))
        self.assertNotEqual(str(self.tmp_root), real_home,
                            "TMPHOME must differ from real $HOME")
        self.assertFalse(
            str(self.tmp_root).startswith("/home/v/"),
            "TMPHOME must not be under /home/v/",
        )
        self.assertFalse(
            str(self.tmp_root).startswith(str(REPO_ROOT) + "/"),
            "TMPHOME must not be inside the dev repo",
        )

        self.disposable_mpm = self.tmp_root / ".mpm"
        self.staging_parent = self.tmp_root / "staging-parent"
        self.staging_parent.mkdir(parents=True, exist_ok=True)

        self.shred_log = self.tmp_root / "shred-wrapper.log"
        self.stdout_log = self.tmp_root / "uninstall.stdout"
        self.stderr_log = self.tmp_root / "uninstall.stderr"
        self.outer_target = self.tmp_root / "outside-shred-target.txt"
        self.home_sentinel = self.tmp_root / "home-survival-sentinel.txt"

        # Build disposable production-shaped tree.
        self._build_disposable_tree()
        self._build_shred_wrapper()

    def tearDown(self):
        shutil.rmtree(self.tmp_root, ignore_errors=True)

    # ------------------------------------------------------------------
    # Setup helpers
    # ------------------------------------------------------------------
    def _build_disposable_tree(self):
        mpm = self.disposable_mpm
        for d in ("src/db", "backups", "logs", "mode", "persona",
                  "migrations", "bin", "run"):
            (mpm / d).mkdir(parents=True, exist_ok=True)
            (mpm / d).chmod(0o700)

        # Sensitive roots with their subdirs.
        for sub in ("src/db/shred-test", "backups/shred-test",
                    "logs/shred-test", "src/db/a/b/c"):
            (mpm / sub).mkdir(parents=True, exist_ok=True)
            (mpm / sub).chmod(0o700)

        # Canary regular files.
        (mpm / "src/db/shred-test/future-secret.xyz").write_text("db-canary")
        (mpm / "backups/shred-test/random-copy.bin").write_text("backup-canary")
        (mpm / "logs/shred-test/new-format.whatever").write_text("log-canary")
        (mpm / "mpm_config.json").write_text("config-canary")

        # Deeply nested arbitrary future-format file (no allowlist).
        (mpm / "src/db/a/b/c/future-format.random-extension").write_text(
            "deep-canary")

        # XDG-style env file outside $PREFIX.
        xdg_cfg = self.tmp_root / ".config" / "mpm"
        xdg_cfg.mkdir(parents=True, exist_ok=True)
        xdg_cfg.chmod(0o700)
        (xdg_cfg / "mpm.env").write_text("env-canary")

        # Outside target that MUST survive.
        self.outer_target.write_text("MUST SURVIVE\n")

        # Symlink inside DB shred root pointing OUTSIDE the MPM tree.
        (mpm / "src/db/shred-test/outside-link").symlink_to(
            str(self.outer_target))

        # HOME survival sentinel.
        self.home_sentinel.write_text("HOME SURVIVED\n")

        # Stage a copy of the real uninstall.sh at the production path.
        shutil.copy(UNINSTALL_SH, mpm / "uninstall.sh")
        (mpm / "uninstall.sh").chmod(0o755)

    def _build_shred_wrapper(self):
        # Place a `shred` wrapper EARLY on PATH; it logs every invocation
        # to shred-wrapper.log and then delegates to /usr/bin/shred.
        wrapper_dir = self.staging_parent / "shred-wrapper"
        wrapper_dir.mkdir(parents=True, exist_ok=True)
        wrapper = wrapper_dir / "shred"
        wrapper.write_text(
            "#!/usr/bin/env bash\n"
            "echo \"$(date -u +%FT%TZ) SHRED-WRAPPER $@\" >> \"$SHRED_LOG_FILE\"\n"
            "exec /usr/bin/shred \"$@\"\n"
        )
        wrapper.chmod(0o755)
        self.wrapper_dir = wrapper_dir

    # ------------------------------------------------------------------
    # The test
    # ------------------------------------------------------------------
    def test_real_shred_self_staging_end_to_end(self):
        # Sanity pre-state.
        self.assertTrue(self.disposable_mpm.is_dir(),
                        "disposable MPM root must exist before shred")
        self.assertTrue(self.disposable_mpm.joinpath(
            "uninstall.sh").is_file(),
            "uninstall.sh must exist at canonical production path")

        env = {
            **os.environ,
            "HOME": str(self.tmp_root),
            # Make uninstall.sh's mktemp calls land under our staging parent
            # so we can observe the staging dir.
            "TMPDIR": str(self.staging_parent),
            "PATH": f"{self.wrapper_dir}:/usr/bin:/bin",
            "SHRED_LOG_FILE": str(self.shred_log),
        }
        proc = subprocess.run(
            ["bash", str(self.disposable_mpm / "uninstall.sh"),
             "--shred", "--yes"],
            capture_output=True,
            text=True,
            env=env,
        )

        # ---- Acceptance criteria ----

        # A. Exit 0.
        self.assertEqual(proc.returncode, 0,
                         f"uninstall --shred --yes failed:\n"
                         f"stdout: {proc.stdout}\nstderr: {proc.stderr}")

        # Capture output for debugging.
        self.stdout_log.write_text(proc.stdout)
        self.stderr_log.write_text(proc.stderr)

        # C. Original disposable root is gone.
        self.assertFalse(self.disposable_mpm.exists(),
                         f"disposable MPM root must be gone after --shred; "
                         f"still present: {self.disposable_mpm}")

        # D. Therefore original uninstall.sh is gone.
        self.assertFalse(self.disposable_mpm.joinpath("uninstall.sh").exists())

        # F. No staging directory remains beneath staging_parent.
        # (shred-wrapper may remain; that is OUR wrapper, not uninstaller's.)
        leftovers = [
            p for p in self.staging_parent.iterdir()
            if p.name.startswith("mpm-uninstall-stage.")
        ]
        self.assertEqual(leftovers, [],
                         f"staged uninstaller dir leaked: {leftovers}")

        # G. Outside symlink target survives.
        self.assertTrue(self.outer_target.is_file(),
                        "outside target must survive")
        self.assertEqual(self.outer_target.read_text(), "MUST SURVIVE\n")

        # H. HOME sentinel survives.
        self.assertTrue(self.home_sentinel.is_file(),
                        "HOME sentinel must survive")
        self.assertEqual(self.home_sentinel.read_text(), "HOME SURVIVED\n")

        # I. Standalone env file gone.
        env_path = self.tmp_root / ".config" / "mpm" / "mpm.env"
        self.assertFalse(env_path.exists(),
                         f"standalone mpm.env must be gone: {env_path}")
        # Parent dir may also be gone (shred removed it).
        cfg_dir = self.tmp_root / ".config" / "mpm"
        self.assertFalse(cfg_dir.exists(),
                         f"XDG config dir must be gone: {cfg_dir}")

        # ---- Shred invocation evidence ----

        # The wrapper MUST have logged every shred invocation. Filter out
        # the version-test entry that the staged copy performs.
        log_text = _read(self.shred_log)
        shred_lines = [
            line for line in log_text.splitlines()
            if "SHRED-WRAPPER" in line and "--version" not in line
        ]
        # Each expected target must appear in some shred invocation.
        for target in self.EXPECTED_SHRED_TARGETS:
            self.assertTrue(
                any(target in line for line in shred_lines),
                f"shred was NOT invoked on expected target: {target}\n"
                f"shred log:\n{log_text}",
            )

        # The symlink and outside target must NOT appear.
        for forbidden in self.EXPECTED_NOT_SHREDDED:
            self.assertFalse(
                any(forbidden in line for line in shred_lines),
                f"shred was unexpectedly invoked on: {forbidden}\n"
                f"shred log:\n{log_text}",
            )

        # ---- Ordering evidence ----
        # In the captured stdout, every "shredding: ...logs/..." line must
        # appear BEFORE any "removed: .../.mpm/logs" line — shred happens
        # before the logs directory is rm-rf'd.
        combined = proc.stdout
        shred_log_idx = combined.find(
            "shredding:")  # first shred invocation
        # find first shred line that mentions logs/<file>
        shred_lines_logs = [
            i for i, line in enumerate(combined.splitlines())
            if "shredding:" in line and "logs/" in line
        ]
        removed_lines_logs = [
            i for i, line in enumerate(combined.splitlines())
            if "removed:" in line and "/.mpm/logs" in line
        ]
        self.assertTrue(shred_lines_logs,
                        "no shred line mentioning logs/* found in stdout")
        self.assertTrue(removed_lines_logs,
                        "no 'removed: ...logs' line found in stdout")
        self.assertLess(max(shred_lines_logs), min(removed_lines_logs),
                        "shred of logs/* must precede rm -rf of logs")

        # ---- Staging evidence ----
        # The staged-copy plan should have project_root = staging dir,
        # install prefix = $TMPHOME/.mpm (the original tree).
        self.assertIn("staged uninstaller to", combined,
                      "expected 'staged uninstaller to' message")
        self.assertIn("re-executing staged copy", combined,
                      "expected 're-executing staged copy' message")
        # After staging, install prefix is still the original TMPHOME/.mpm.
        # Once staged, the staged copy reports its own project_root which
        # is in staging-parent; this confirms execution continued from
        # the staged copy while the original tree was still being torn down.
        self.assertIn("install prefix:", combined)
        # Confirm the post-staged plan was emitted (proves staged exec ran).
        self.assertGreaterEqual(
            combined.count("=== destruction plan ==="), 2,
            "expected destruction plan to be printed twice (once before "
            "staging and once after re-exec)")

        # Original uninstall.sh GONE *during* shred — printed "removed:"
        # for the install prefix as a clear acceptance marker.
        self.assertIn("removed: " + str(self.disposable_mpm) +
                      " (install prefix)",
                      combined,
                      "expected explicit 'removed: $PREFIX (install prefix)' "
                      "log line proving the install tree was deleted")
        self.assertIn("install prefix gone:", combined,
                      "expected 'install prefix gone' confirmation log")
        self.assertIn("uninstall complete", combined,
                      "expected 'uninstall complete' log line")
        self.assertIn("removed staged copy at", combined,
                      "expected 'removed staged copy at' log line")


if __name__ == "__main__":
    unittest.main()
