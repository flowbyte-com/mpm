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
        # sandbox HOME --------------------------------------------------
        #
        # deploy.sh runs `make install` UNCONDITIONALLY; `--no-install`
        # only skips the *service restart* half. Two facts make an
        # un-sandboxed run write to the real installation:
        #
        #   1. deploy.sh:32 prepends "$HOME/.mpm/bin:$HOME/.local/bin:
        #      /usr/local/bin:/usr/bin:/bin" to PATH. That places the
        #      real /usr/bin/make AHEAD of any make the caller put
        #      earlier in PATH, so a caller-supplied fake is bypassed.
        #   2. make's PREFIX defaults to $(HOME)/.mpm, so the promotion
        #      target follows HOME.
        #
        # Pointing HOME at a sandbox neutralises BOTH: CANONICAL_BIN
        # ($HOME/.mpm/bin) and make's PREFIX resolve inside the sandbox,
        # and the fake `make` goes in $HOME/.mpm/bin — the exact slot
        # deploy.sh prepends first, so the stub wins the lookup.
        #
        # Even if the stub were bypassed, the real `make install` would
        # promote into the sandbox, not the operator's live install.
        self.home = self.tmp / "home"
        self.sandbox_bin = self.home / ".mpm" / "bin"
        self.sandbox_bin.mkdir(parents=True, exist_ok=True)
        self.make_stub = self.sandbox_bin / "make"
        self.make_stub.write_text(
            "#!/bin/sh\n"
            "# Test stub: record that we were invoked, then succeed\n"
            "# without building or installing anything.\n"
            f"printf 'invoked cwd=%s\\n' \"$(pwd)\" >> '{self.tmp}/make-invocations'\n"
            "exit 0\n"
        )
        self.make_stub.chmod(0o755)

        # Utilities deploy.sh invokes (sha256sum, pgrep, awk, ...) still
        # need to resolve; symlink the real ones.
        for util in ("sha256sum", "pgrep", "mkdir", "true", "echo", "cat", "awk"):
            src = Path("/usr/bin") / util
            if src.exists():
                link = self.fakebin / util
                if not link.exists():
                    try:
                        link.symlink_to(src)
                    except OSError:
                        pass

        # Hard-fail stubs for the two utilities that reach outside the
        # process. deploy.sh should never call them under --no-install;
        # if that ever regresses we want a loud test failure, not a real
        # `systemctl --user restart` or gateway restart on this machine.
        for guard in ("systemctl", "openclaw"):
            stub = self.fakebin / guard
            stub.write_text(
                "#!/bin/sh\n"
                f"echo 'GUARD: deploy.sh invoked {guard}; this test must never "
                "reach service control' >&2\n"
                "exit 97\n"
            )
            stub.chmod(0o755)

        self.addCleanup(self._cleanup)

    def _cleanup(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_deploy_sh_resolves_repo_root_when_invoked_from_arbitrary_cwd(self):
        """Run deploy.sh --no-install from /tmp and verify it can locate
        install.sh via its REPO_ROOT resolution. The simplest proof:
        deploy.sh should NOT fail with "cannot find Makefile" or
        similar — it must `cd` to the correct REPO_ROOT.

        The run is sandboxed (see setUp): deploy.sh's `make install` is
        unconditional and targets $(HOME)/.mpm, so HOME is pinned to a
        temp dir for the duration of this test."""
        env = os.environ.copy()
        env["HOME"] = str(self.home)
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
        combined = result.stdout + result.stderr
        self.assertIn("==> Building mpm binaries", combined,
                      f"deploy.sh did not reach the build phase; cwd=/tmp\n"
                      f"stdout: {result.stdout[:500]}\n"
                      f"stderr: {result.stderr[:500]}")

        # --- Hermeticity guards -------------------------------------
        #
        # These are not incidental. The pre-fix version of this test put
        # its fake `make` in a fakebin that deploy.sh's own PATH prepend
        # shadowed, so the REAL /usr/bin/make ran `make install` with
        # PREFIX defaulting to the operator's $HOME/.mpm — a test in the
        # canonical gate was replacing the live installation binaries.
        # `--no-install` did not prevent this; it only skips the restart.

        # 1. The stub must actually have been the make that ran.
        invocations = self.tmp / "make-invocations"
        self.assertTrue(
            invocations.exists(),
            "the sandboxed `make` stub was never invoked, so deploy.sh "
            "reached a different make than the one this test controls — "
            "its PATH prepend may once again be shadowing the sandbox.\n"
            f"combined output:\n{combined}",
        )
        # 2. deploy.sh must have cd'd to REPO_ROOT before invoking make.
        self.assertIn(f"invoked cwd={REPO_ROOT}", invocations.read_text(),
                      "deploy.sh did not run `make` from REPO_ROOT; "
                      "repository-root resolution is broken.")
        # 3. Nothing may have been promoted into the live install root.
        live_prefix = Path(os.path.expanduser("~")) / ".mpm" / "bin"
        sandbox_is_not_live = self.sandbox_bin.resolve() != live_prefix.resolve()
        self.assertTrue(
            sandbox_is_not_live,
            "sandbox $HOME/.mpm/bin resolves onto the live install "
            f"({live_prefix}); this test would write production binaries",
        )

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
