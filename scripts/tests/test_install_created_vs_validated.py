"""
test_install_created_vs_validated.py — Behavioral regression coverage for
``install.sh`` phase_data_dir's truthful "created" vs "validated existing"
output.

Architectural contract:

  The installer must distinguish in its log output between:
    - directories it actually created on this run;
    - directories that already existed before this run.

  Re-installs on existing state MUST NOT claim "created".

  Both $DATA_ROOT/src/db and $DATA_ROOT/backups/critic-pre are in scope.

  Mode is enforced at 0700 in both cases (defence-in-depth: the runtime
  sweep re-tightens, but the installer is the primary gate).

The tests drive ``phase_data_dir`` directly with a controlled DATA_ROOT
in an isolated HOME — they do NOT run the full installer, and they do
NOT mutate the real ``~/.mpm``.
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
INSTALL_SH = REPO_ROOT / "install.sh"

# Markers in install.sh that prevent the script from running main()
# during test sourcing. PROJECT_ROOT/REPO_ROOT are computed from $0
# inside the script as ``readonly``, which would override anything we
# export; we strip those lines so the driver can control them.
STRIP_PATTERNS = (
    re.compile(r"^\s*readonly\s+SCRIPT_DIR\s*="),
    re.compile(r"^\s*readonly\s+REPO_ROOT\s*="),
    re.compile(r"^\s*readonly\s+PROJECT_ROOT\s*="),
    re.compile(r'^\s*main\s+"\$@"\s*$'),
)


def _sourced_install_lib(install_text: str) -> str:
    out_lines = []
    for line in install_text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out_lines.append(line)
    return "\n".join(out_lines) + "\n"


def _run_phase_data_dir(data_root: Path) -> dict:
    """Drive phase_data_dir in a subprocess with controlled DATA_ROOT.

    Returns a dict with ``returncode``, ``stdout``, ``stderr``.
    """
    install_text = INSTALL_SH.read_text()
    sourced = _sourced_install_lib(install_text)
    driver = textwrap.dedent(f"""\
        #!/usr/bin/env bash
        set -uo pipefail
        export DATA_ROOT={data_root}
        # log() / note() write to stderr in install.sh; capture both.
        phase_data_dir
    """)
    with tempfile.NamedTemporaryFile("w", suffix=".sh", delete=False) as f:
        f.write(sourced + "\n" + driver)
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


class TestDataDirCreatedVsValidated(unittest.TestCase):
    """phase_data_dir's per-path output truthfully distinguishes the
    two cases and enforces 0700 in both."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-test-datadir-"))
        self.data_root = self.tmp / ".mpm"

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _mode(self, p: Path) -> str:
        return oct(p.stat().st_mode & 0o777)

    def test_fresh_run_creates(self):
        """No pre-existing dirs -> log says 'created' for both."""
        result = _run_phase_data_dir(self.data_root)
        self.assertEqual(result["returncode"], 0,
                         f"phase_data_dir failed:\nstderr: {result['stderr']}")
        combined = result["stdout"] + result["stderr"]
        self.assertIn(f"created {self.data_root}/src/db (mode 0700)", combined)
        self.assertIn(f"created {self.data_root}/backups/critic-pre (mode 0700)",
                      combined)
        # No false "validated existing" claims on a fresh run.
        self.assertNotIn("validated existing", combined)

        # Dirs exist at 0700.
        self.assertTrue((self.data_root / "src/db").is_dir())
        self.assertTrue((self.data_root / "backups/critic-pre").is_dir())
        self.assertEqual(self._mode(self.data_root / "src/db"), "0o700")
        self.assertEqual(self._mode(self.data_root / "backups/critic-pre"),
                         "0o700")

    def test_rerun_on_existing_dirs_validates(self):
        """Pre-existing 0700 dirs -> log says 'validated existing', not 'created'."""
        # First run: create them.
        first = _run_phase_data_dir(self.data_root)
        self.assertEqual(first["returncode"], 0)

        # Second run: must say 'validated existing', not 'created'.
        second = _run_phase_data_dir(self.data_root)
        self.assertEqual(second["returncode"], 0,
                         f"phase_data_dir (rerun) failed:\nstderr: {second['stderr']}")
        combined = second["stdout"] + second["stderr"]
        self.assertIn(f"validated existing {self.data_root}/src/db (mode 0700)",
                      combined)
        self.assertIn(f"validated existing {self.data_root}/backups/critic-pre (mode 0700)",
                      combined)
        # No false "created" claims on a rerun.
        self.assertNotIn(f"created {self.data_root}/src/db", combined)
        self.assertNotIn(f"created {self.data_root}/backups/critic-pre", combined)

        # Mode preserved at 0700.
        self.assertEqual(self._mode(self.data_root / "src/db"), "0o700")
        self.assertEqual(self._mode(self.data_root / "backups/critic-pre"),
                         "0o700")

    def test_preexisting_permissive_dirs_validated_and_hardened(self):
        """Pre-existing 0755 dirs -> log says 'validated existing' AND
        mode is re-tightened to 0700."""
        self.data_root.mkdir(parents=True, exist_ok=True)
        for sub in ("src/db", "backups/critic-pre"):
            d = self.data_root / sub
            d.mkdir(parents=True, exist_ok=True)
            d.chmod(0o755)
        self.assertEqual(self._mode(self.data_root / "src/db"), "0o755")

        result = _run_phase_data_dir(self.data_root)
        self.assertEqual(result["returncode"], 0)
        combined = result["stdout"] + result["stderr"]
        # Truthfully says 'validated existing' (not 'created').
        self.assertIn(f"validated existing {self.data_root}/src/db (mode 0700)",
                      combined)
        self.assertIn(f"validated existing {self.data_root}/backups/critic-pre (mode 0700)",
                      combined)
        self.assertNotIn(f"created {self.data_root}/src/db", combined)
        self.assertNotIn(f"created {self.data_root}/backups/critic-pre", combined)

        # Mode hardened to 0700.
        self.assertEqual(self._mode(self.data_root / "src/db"), "0o700")
        self.assertEqual(self._mode(self.data_root / "backups/critic-pre"),
                         "0o700")


if __name__ == "__main__":
    unittest.main()
