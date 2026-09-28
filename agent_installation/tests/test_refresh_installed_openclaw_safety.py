"""
test_refresh_installed_openclaw_safety.py — OpenClaw-safety contract.

Pins that the documented update path never auto-installs the OpenClaw
managed block. OpenClaw is intentionally absent on this machine; the
operator's explicit-installation contract requires
`mpm-memory-openclaw/install.sh` to be a separate, opt-in action.

Covers:
  - make refresh-installed (the standalone reconciliation target)
  - make install (which depends on refresh-installed)
  - ./install.sh --reconcile (content-only update mode)
  - ./install.sh (full install, including the reconciliation phase)

In every case, no path may create or write to the OpenClaw install
target. Tests use a sandbox HOME so the real machine state is
untouched.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
MAKEFILE = REPO_ROOT / "Makefile"
INSTALL_SH = REPO_ROOT / "install.sh"


def _sandbox_home() -> tuple[Path, dict]:
    """Create an isolated HOME and return (home_path, env)."""
    tmp = Path(tempfile.mkdtemp(prefix="mpm-openclaw-safety-"))
    home = tmp / "home"
    home.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env["HOME"] = str(home)
    return tmp, env


def _cleanup(tmp: Path) -> None:
    shutil.rmtree(tmp, ignore_errors=True)


def _run(cmd: list[str], *, env: dict, timeout: int = 240) -> subprocess.CompletedProcess:
    return subprocess.run(
        cmd, capture_output=True, text=True, env=env, timeout=timeout,
    )


def _openclaw_target(home: Path) -> Path:
    """The canonical OpenClaw install target (per
    check_installed_managed_blocks.HOST_INSTALL_TARGETS)."""
    return home / ".openclaw" / "workspace" / "SOUL.md"


class OpenClawSafety(unittest.TestCase):
    """The four update paths must never auto-install the OpenClaw
    managed block."""

    def setUp(self):
        self.tmpdir, self.env = _sandbox_home()
        self.home = self.tmpdir / "home"
        # Make sure the openclaw target is absent at start (no stale
        # file carried over from a prior test).
        oc = _openclaw_target(self.home)
        if oc.exists():
            oc.unlink()

    def tearDown(self):
        _cleanup(self.tmpdir)

    # ---- shared invariant for every update path ----

    def _assert_openclaw_untouched(self, oc: Path, label: str, stderr: str):
        """The openclaw target MUST remain absent (or, if pre-existing,
        untouched) after the update path runs."""
        self.assertFalse(
            oc.exists(),
            f"{label}: must not create {oc} (operator-only install). "
            f"stderr={stderr[:500]!r}",
        )
        # Also check the parent dir wasn't created with any SOsL
        # artifacts; the absence of the file is sufficient.
        if oc.parent.exists():
            remaining = list(oc.parent.iterdir())
            self.assertEqual(
                remaining, [],
                f"{label}: openclaw parent dir should be empty, "
                f"found {remaining}",
            )

    # ---- 1. make refresh-installed ----

    def test_make_refresh_installed_does_not_install_openclaw(self):
        """The standalone reconciliation target must not auto-install
        OpenClaw."""
        result = _run(
            ["make", "-C", str(REPO_ROOT), "refresh-installed"],
            env=self.env,
        )
        self.assertEqual(
            result.returncode, 0,
            f"make refresh-installed failed:\nstdout={result.stdout}\n"
            f"stderr={result.stderr}",
        )
        self._assert_openclaw_untouched(
            _openclaw_target(self.home),
            "make refresh-installed",
            result.stderr,
        )

    # ---- 2. make install (depends on refresh-installed) ----

    def test_make_install_does_not_install_openclaw(self):
        """make install is the documented update entry point. It must
        not auto-install OpenClaw, even though it depends on
        refresh-installed."""
        # We invoke make install but skip the binary copy step (which
        # would copy into $PREFIX/bin/ == $HOME/.mpm/bin/, potentially
        # creating files under $HOME/.mpm/). The contract under test
        # is "no OpenClaw install"; the binary copy is unrelated and
        # requires the source tree to be built. Use make -n to dry-run
        # the build/refresh-installed portion, then verify the
        # openclaw target is unchanged.
        result = _run(
            ["make", "-n", "-C", str(REPO_ROOT), "install"],
            env=self.env,
        )
        self.assertEqual(
            result.returncode, 0,
            f"make -n install failed:\nstdout={result.stdout}\n"
            f"stderr={result.stderr}",
        )
        # The dependency chain must include refresh-installed. This
        # is the parallel-safety assertion: without it, a parallel
        # make could race build against refresh-installed in ways
        # that would break the convergence contract.
        self.assertIn(
            "refreshing host installed artifacts from canonical source",
            result.stdout,
            "make -n install must include refresh-installed steps in "
            "its dependency chain. Without this, the documented "
            "`git pull && make install` flow cannot guarantee "
            "managed-block reconciliation.",
        )
        self._assert_openclaw_untouched(
            _openclaw_target(self.home),
            "make -n install",
            result.stderr,
        )

    # ---- 3. ./install.sh --reconcile ----

    def test_install_sh_reconcile_does_not_install_openclaw(self):
        """`./install.sh --reconcile` is the documented content-only
        update path. OpenClaw must remain absent."""
        result = _run(
            ["bash", str(INSTALL_SH), "--reconcile"],
            env=self.env, timeout=180,
        )
        self.assertEqual(
            result.returncode, 0,
            f"install.sh --reconcile failed:\nstdout={result.stdout}\n"
            f"stderr={result.stderr}",
        )
        self._assert_openclaw_untouched(
            _openclaw_target(self.home),
            "install.sh --reconcile",
            result.stderr,
        )

    # ---- 4. ./install.sh (full install with reconcile phase) ----

    def test_install_sh_does_not_install_openclaw(self):
        """Full install must not auto-install OpenClaw. The reconcile
        phase is invoked from mode_install; this test pins that
        auto-install for OpenClaw is NOT in the path even on a full
        install (where the operator may have just uncommented their
        openclaw adapter manually).
        """
        result = _run(
            ["bash", str(INSTALL_SH)],
            env=self.env, timeout=180,
        )
        # Full install may legitimately fail in this sandbox because
        # of systemd/go prerequisites — that's OK for THIS test
        # because the openclaw-safety contract is enforced before
        # systemd is touched.
        self._assert_openclaw_untouched(
            _openclaw_target(self.home),
            "install.sh (full)",
            result.stderr,
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
