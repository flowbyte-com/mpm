"""
test_install_created_vs_verified.py — Behavioral regression coverage for
``install.sh`` phase_data_dir's truthful "created" vs "verified existing"
output.

Architectural contract:

  The installer must distinguish in its log output between:
    - directories it actually created on this run ("created");
    - directories that already existed before this run ("verified existing").

  Re-installs on existing state MUST NOT claim "created".

  Both $DATA_ROOT/src/db and $DATA_ROOT/backups/critic-pre are in scope.

  Mode is enforced at 0700 in both cases (defence-in-depth: the runtime
  sweep re-tightens, but the installer is the primary gate).

The tests below are BEHAVIORAL — they exercise real filesystem state
transitions in an isolated temporary HOME, drive the actual install.sh
phase_data_dir via subprocess (no mock), and pin both the output strings
AND the on-disk mode bits.

Pre-existence is decided by the installer by checking the inode at the
target path BEFORE running `install -d`; we verify that property directly
by pre-creating dirs (in some cases at wrong permissions) and confirming
the installer reports truthfully.

These tests do NOT mutate any real ``~/.mpm`` or ``~/workspace/projects/mpm``.
"""

from __future__ import annotations

import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
INSTALL_SH = REPO_ROOT / "install.sh"


# Markers in install.sh that prevent the script from running main()
# during test sourcing. PROJECT_ROOT / REPO_ROOT / SCRIPT_DIR are computed
# from $0 inside the script as ``readonly``, which would override anything
# we export; we strip those lines so the driver can control them.
STRIP_PATTERNS = (
    re.compile(r"^\s*readonly\s+SCRIPT_DIR\s*="),
    re.compile(r"^\s*readonly\s+REPO_ROOT\s*="),
    re.compile(r"^\s*readonly\s+PROJECT_ROOT\s*="),
    re.compile(r'^\s*main\s+"\$@"\s*$'),
)


def _strip_patterns(install_text: str) -> str:
    """Strip lines matching STRIP_PATTERNS so the test driver can
    override env-derived readonly declarations and skip main()."""
    out_lines = []
    for line in install_text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out_lines.append(line)
    return "\n".join(out_lines) + "\n"


def _run_phase_data_dir(data_root: Path) -> dict:
    """Drive ``phase_data_dir`` in a fresh subprocess with controlled DATA_ROOT.

    Returns a dict with ``returncode``, ``stdout``, ``stderr``.
    """
    install_text = INSTALL_SH.read_text()
    sourced = _strip_patterns(install_text)
    driver_lines = [
        'export DATA_ROOT="{}"'.format(str(data_root)),
        'phase_data_dir',
    ]
    with tempfile.NamedTemporaryFile("w", suffix=".sh", delete=False) as f:
        f.write(sourced + "\n" + "\n".join(driver_lines) + "\n")
        driver_path = Path(f.name)
    driver_path.chmod(0o755)
    try:
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver_path)],
            capture_output=True,
            text=True,
            env={**os.environ, "PATH": "/usr/bin:/bin"},
        )
    finally:
        driver_path.unlink()
    return {
        "returncode": result.returncode,
        "stdout": result.stdout,
        "stderr": result.stderr,
    }


class TestDataDirCreatedVsVerified(unittest.TestCase):
    """``phase_data_dir`` MUST truthfully report whether each managed
    directory was created by this run or already existed, AND MUST
    enforce 0700 in both cases."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-test-datadir-"))
        self.data_root = self.tmp / ".mpm"
        self.src_db = self.data_root / "src" / "db"
        self.backups_critic_pre = self.data_root / "backups" / "critic-pre"

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _mode(self, p: Path) -> str:
        return oct(p.stat().st_mode & 0o777)

    def _combined_output(self, result: dict) -> str:
        return result["stdout"] + result["stderr"]

    # ------------------------------------------------------------------
    # CASE 1: Fresh — neither directory exists before the invocation.
    # ------------------------------------------------------------------
    def test_case_1_fresh_run_says_created_for_each(self):
        """Pre: both dirs absent. Post: 'created' for both; no
        'verified existing' lines at all; modes 0700."""
        result = _run_phase_data_dir(self.data_root)
        self.assertEqual(result["returncode"], 0,
                         f"phase_data_dir failed:\nstderr: {result['stderr']}")
        out = self._combined_output(result)

        # Truthful per-path claims.
        self.assertIn(f"created {self.src_db} (mode 0700)", out)
        self.assertIn(f"created {self.backups_critic_pre} (mode 0700)", out)

        # No false "verified existing" claims on a fresh run.
        self.assertNotIn("verified existing", out)

        # Wording is consistent — never the old "validated existing".
        self.assertNotIn("validated existing", out)

        # Final state on disk.
        self.assertTrue(self.src_db.is_dir())
        self.assertTrue(self.backups_critic_pre.is_dir())
        self.assertEqual(self._mode(self.src_db), "0o700")
        self.assertEqual(self._mode(self.backups_critic_pre), "0o700")

    # ------------------------------------------------------------------
    # CASE 2: Pre-existing correct (0700) mode.
    # ------------------------------------------------------------------
    def test_case_2_preexisting_0700_says_verified_existing(self):
        """Pre: both dirs exist at 0700. Post: 'verified existing' for
        both; no 'created' lines; mode preserved at 0700."""
        # Pre-create at the canonical mode.
        self.src_db.mkdir(parents=True, exist_ok=True)
        self.src_db.chmod(0o700)
        self.backups_critic_pre.mkdir(parents=True, exist_ok=True)
        self.backups_critic_pre.chmod(0o700)
        # Mark with a sentinel file so we can prove the dir was not
        # replaced by `install -d` (install -d on an existing dir is a
        # no-op, so the sentinel should still be there afterwards).
        sentinel = self.src_db / "pre-existing-sentinel.txt"
        sentinel.write_text("I existed before this run")

        result = _run_phase_data_dir(self.data_root)
        self.assertEqual(result["returncode"], 0,
                         f"phase_data_dir failed:\nstderr: {result['stderr']}")
        out = self._combined_output(result)

        # Truthful per-path claims.
        self.assertIn(f"verified existing {self.src_db} (mode 0700)", out)
        self.assertIn(f"verified existing {self.backups_critic_pre} (mode 0700)", out)

        # No false "created" claims.
        self.assertNotIn(f"created {self.src_db}", out)
        self.assertNotIn(f"created {self.backups_critic_pre}", out)

        # Wording is consistent.
        self.assertNotIn("validated existing", out)

        # Mode preserved, sentinel preserved.
        self.assertEqual(self._mode(self.src_db), "0o700")
        self.assertEqual(self._mode(self.backups_critic_pre), "0o700")
        self.assertTrue(sentinel.is_file(),
                        "pre-existing sentinel should still exist after run")
        self.assertEqual(sentinel.read_text(),
                         "I existed before this run")

    # ------------------------------------------------------------------
    # CASE 3: Pre-existing INCORRECT (0755) mode — must be verified, NOT
    # created, AND must be hardened to 0700.
    # ------------------------------------------------------------------
    def test_case_3_preexisting_0755_says_verified_existing_and_hardens(self):
        """Pre: both dirs exist at 0755. Post: 'verified existing' for
        both (NOT 'created'); mode re-hardened to 0700."""
        self.src_db.mkdir(parents=True, exist_ok=True)
        self.src_db.chmod(0o755)
        self.backups_critic_pre.mkdir(parents=True, exist_ok=True)
        self.backups_critic_pre.chmod(0o755)
        # Verify pre-condition.
        self.assertEqual(self._mode(self.src_db), "0o755")
        self.assertEqual(self._mode(self.backups_critic_pre), "0o755")

        result = _run_phase_data_dir(self.data_root)
        self.assertEqual(result["returncode"], 0)
        out = self._combined_output(result)

        # Truthful claims.
        self.assertIn(f"verified existing {self.src_db} (mode 0700)", out)
        self.assertIn(f"verified existing {self.backups_critic_pre} (mode 0700)", out)
        self.assertNotIn(f"created {self.src_db}", out)
        self.assertNotIn(f"created {self.backups_critic_pre}", out)
        self.assertNotIn("validated existing", out)

        # Hardened to 0700 (this is the canonical chmod-after-mkdir
        # defence-in-depth discipline).
        self.assertEqual(self._mode(self.src_db), "0o700")
        self.assertEqual(self._mode(self.backups_critic_pre), "0o700")


class TestDataDirPhaseSourceInvariants(unittest.TestCase):
    """Source-level invariants for phase_data_dir that must remain true
    so the behavioral pins above don't regress silently.

    These are intentionally minimal: they check the structure of the
    pre-existence detection (must run BEFORE install -d for each path)
    and the literal wording of the two output strings."""

    def test_install_phase_data_dir_runs_pre_existence_check_before_install_d(self):
        install_text = INSTALL_SH.read_text()
        # Find the phase_data_dir body.
        m = re.search(
            r"phase_data_dir\(\)\s*\{(.+?)^}",
            install_text,
            re.DOTALL | re.MULTILINE,
        )
        self.assertIsNotNone(m, "phase_data_dir function not found")
        body = m.group(1)

        # For src/db: the [ -e "$DATA_ROOT/src/db" ] pre-existence probe
        # MUST appear BEFORE `install -d -m 0700 "$DATA_ROOT/src/db"`.
        idx_probe_src = body.find('[ -e "$DATA_ROOT/src/db" ]')
        idx_install_src = body.find('install -d -m 0700 "$DATA_ROOT/src/db"')
        self.assertNotEqual(idx_probe_src, -1,
                             "src/db pre-existence probe not found in phase_data_dir")
        self.assertNotEqual(idx_install_src, -1,
                             "src/db install -d not found in phase_data_dir")
        self.assertLess(idx_probe_src, idx_install_src,
                        "src/db: pre-existence probe must run BEFORE install -d")

        # For backups/critic-pre: same invariant.
        idx_probe_bak = body.find('[ -e "$DATA_ROOT/backups/critic-pre" ]')
        idx_install_bak = body.find(
            'install -d -m 0700 "$DATA_ROOT/backups/critic-pre"')
        self.assertNotEqual(idx_probe_bak, -1,
                             "backups/critic-pre probe not found")
        self.assertNotEqual(idx_install_bak, -1,
                             "backups/critic-pre install -d not found")
        self.assertLess(idx_probe_bak, idx_install_bak,
                        "backups/critic-pre: probe must run BEFORE install -d")

    def test_install_phase_data_dir_uses_verified_existing_wording(self):
        install_text = INSTALL_SH.read_text()
        self.assertIn("verified existing $DATA_ROOT/src/db (mode 0700)",
                      install_text)
        self.assertIn("verified existing $DATA_ROOT/backups/critic-pre (mode 0700)",
                      install_text)
        # Old wording must NOT remain in active source.
        self.assertNotIn("validated existing $DATA_ROOT/src/db",
                         install_text)
        self.assertNotIn("validated existing $DATA_ROOT/backups/critic-pre",
                         install_text)


if __name__ == "__main__":
    unittest.main()
