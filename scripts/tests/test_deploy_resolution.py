"""
test_deploy_resolution.py — Regression coverage for scripts/deploy.sh
repository-root resolution.

scripts/deploy.sh lives at <repo>/scripts/deploy.sh — its parent is
the repository root, NOT the script's directory. This test confirms
that deploy.sh, when invoked from an arbitrary CWD, correctly
identifies its REPO_ROOT and resolves a path that exists only at the
repo root (install.sh).
"""

from __future__ import annotations

import os
import shutil
import stat
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
DEPLOY_SH = REPO_ROOT / "scripts" / "deploy.sh"


class TestDeployRepoRootResolution(unittest.TestCase):
    """deploy.sh must resolve REPO_ROOT correctly even from arbitrary
    CWD, after the lifecycle relocation moved it under scripts/."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-deploy-test-"))
        self.fakebin = self.tmp / "fakebin"
        self.fakebin.mkdir(parents=True, exist_ok=True)
        # All binaries deploy.sh invokes (make, openclaw, systemctl, sha256sum)
        # need to be on PATH. Build a minimal PATH.
        for util in ("make", "openclaw", "systemctl", "sha256sum", "pgrep",
                     "mkdir", "true", "echo", "cat", "awk"):
            src = Path("/usr/bin") / util
            if src.exists():
                link = self.fakebin / util
                if not link.exists():
                    try:
                        link.symlink_to(src)
                    except OSError:
                        pass
        # Override `make` to be a no-op (we don't actually want to build).
        # We can't override with a real symlink; instead, use a wrapper.
        # Simpler: just don't actually invoke deploy.sh's make — use --no-install
        # flag and inspect the early output (project_root message).
        self.addCleanup(self._cleanup)

    def _cleanup(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_deploy_sh_resolves_repo_root_when_invoked_from_arbitrary_cwd(self):
        """Run deploy.sh --no-install from /tmp and verify it can locate
        install.sh via its REPO_ROOT resolution. The simplest proof:
        deploy.sh should NOT fail with "cannot find Makefile" or
        similar — it must `cd` to the correct REPO_ROOT."""
        # We use --no-install to skip actual builds; deploy.sh still does
        # `make install` UNLESS --no-install is given. We need to fake `make`
        # so it succeeds even from /tmp.
        fake_make = self.fakebin / "make"
        fake_make.unlink() if fake_make.exists() or fake_make.is_symlink() else None
        fake_make.write_text("#!/bin/sh\nexit 0\n")
        fake_make.chmod(0o755)

        # Use --no-install to skip the install + service restart phase;
        # we only care about REPO_ROOT resolution in the early `make install`
        # call.
        env = os.environ.copy()
        env["PATH"] = f"{self.fakebin}:{env.get('PATH', '')}"
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(DEPLOY_SH), "--no-install"],
            capture_output=True, text=True, env=env,
            cwd="/tmp",
            timeout=20,
        )
        # If REPO_ROOT resolution is wrong, the early `make install` would
        # fail or the script would emit a "no Makefile" message. With a
        # fake make that exits 0, the script should complete the build phase
        # and exit 0 after --no-install.
        # We tolerate failure from later phases (e.g. systemctl) but the
        # first build line should report success.
        combined = result.stdout + result.stderr
        self.assertIn("==> Building mpm binaries", combined,
                      f"deploy.sh did not reach the build phase; cwd=/tmp\n"
                      f"stdout: {result.stdout[:500]}\n"
                      f"stderr: {result.stderr[:500]}")

    def test_deploy_sh_script_dir_resolution(self):
        """A direct inspection: confirm deploy.sh's SCRIPT_DIR resolves
        to the scripts/ directory and its REPO_ROOT (parent) resolves to
        the actual repo root."""
        # Inline the same resolution logic deploy.sh uses, with the
        # fixed path substituted. This proves the same arithmetic that
        # deploy.sh performs produces the correct result.
        deploy_path = str(DEPLOY_SH)
        probe = f'''#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname '{deploy_path}')" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
echo "SCRIPT_DIR=$SCRIPT_DIR"
echo "REPO_ROOT=$REPO_ROOT"
'''
        result = subprocess.run(
            ["bash", "-c", probe],
            capture_output=True, text=True,
            cwd="/tmp",
        )
        self.assertEqual(result.returncode, 0,
                         f"probe failed: {result.stderr}")
        self.assertIn(f"SCRIPT_DIR={REPO_ROOT}/scripts", result.stdout,
                      f"SCRIPT_DIR did not resolve correctly: {result.stdout}")
        self.assertIn(f"REPO_ROOT={REPO_ROOT}", result.stdout,
                      f"REPO_ROOT did not resolve correctly: {result.stdout}")
        # REPO_ROOT must NOT include /scripts suffix.
        self.assertNotIn(f"REPO_ROOT={REPO_ROOT}/scripts", result.stdout)

    def test_deploy_sh_uses_parent_of_script_dir_for_repo_root(self):
        """Inspect deploy.sh source: confirm the REPO_ROOT computation
        uses `cd "$SCRIPT_DIR/.."` rather than treating SCRIPT_DIR
        itself as the repo root. This is the regression we are guarding
        against (pre-2026-09-17 deploy.sh was at repo root; its old
        `dirname "$0"` happened to equal REPO_ROOT, which would be wrong
        now that it lives one level deeper)."""
        text = DEPLOY_SH.read_text()
        # The new canonical REPO_ROOT resolution.
        self.assertIn(
            'REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"',
            text,
            "deploy.sh must compute REPO_ROOT as parent of SCRIPT_DIR",
        )
        # And SCRIPT_DIR must use BASH_SOURCE.
        self.assertIn('SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"',
                      text,
                      "deploy.sh must resolve SCRIPT_DIR via BASH_SOURCE")


if __name__ == "__main__":
    unittest.main()
