"""
test_install_phase_binaries.py — Regression coverage for install.sh

Pins the contract for ``phase_binaries``:

  Case A (separate layout) — ``PROJECT_ROOT != PREFIX``
    Daemon binaries (mpm-scheduler, mpm-critic, mpm-mcp, mpm-telemetry)
    are installed into ``$PREFIX/bin/`` via ``install -m 0755``.
    The ``mpm`` binary is renamed to ``mpm.real`` and a wrapper script
    is written at ``$PREFIX/bin/mpm``.

  Case B (same layout) — ``PROJECT_ROOT == PREFIX`` (e.g. user cloned
  the repo directly to ``$HOME/.mpm``)
    The build output already lives at the install target. ``install
    -m 0755 src dst`` would refuse to copy a file onto itself. The
    phase must detect this and skip the per-binary copy (preserving the
    real binary in place), while still preserving the real binary as
    ``mpm.real`` before the wrapper replaces ``mpm``.

  Wrapper ordering — in BOTH cases, after the phase completes:
    * ``$PREFIX/bin/mpm.real`` is the real ELF binary (not a wrapper)
    * ``$PREFIX/bin/mpm`` is the wrapper script (starts with ``#!``)
    * the wrapper ``exec``s ``mpm.real``

The bug being regressed against: prior to the fix, ``install -m 0755
$PROJECT_ROOT/bin/mpm-scheduler $PREFIX/bin/mpm-scheduler`` failed
with ``install: ... are the same file`` in the same-layout case,
``set -e`` aborted before the wrapper was written, and the remaining
install phases never ran.

These tests do NOT mock the installer — they source the real
install.sh's function definitions into a subprocess and drive
``phase_binaries`` directly, so any regression in the install logic
itself is caught.
"""

from __future__ import annotations

import os
import re
import shutil
import stat
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path


REPO_ROOT = Path("/home/v/workspace/projects/mpm").resolve()
INSTALL_SH = REPO_ROOT / "install.sh"

# Markers in install.sh that prevent the script from running main()
# during test sourcing. PROJECT_ROOT is computed from $0 inside the
# script as ``readonly``, which would override anything we export; we
# strip those two lines so the driver can control them.
STRIP_PATTERNS = (
    re.compile(r"^\s*readonly\s+SCRIPT_DIR\s*="),
    re.compile(r"^\s*readonly\s+PROJECT_ROOT\s*="),
    re.compile(r'^\s*main\s+"\$@"\s*$'),
)


def build_sourced_lib(install_text: str) -> str:
    """Return the install.sh script with path-resolution and main()
    invocations stripped, so it can be sourced for isolated testing."""
    out_lines = []
    for line in install_text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out_lines.append(line)
    return "\n".join(out_lines) + "\n"


class _PhaseBinariesDriver(unittest.TestCase):
    """Shared scaffolding: stand up a fake project tree, drive
    phase_binaries in a child bash, assert on the resulting layout."""

    BINARIES = ("mpm", "mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry")
    DAEMONS = ("mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry")

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-install-test-"))
        self.addCleanup(self._cleanup)

    def _cleanup(self):
        shutil.rmtree(self._tmp, ignore_errors=True)

    def _build_fake_project(self, root: Path, sentinel: str | None = None) -> None:
        """Populate a minimal project tree at ``root`` with the files
        install.sh needs to find. The sentinel string is embedded in
        every fake binary so tests can verify content survives the
        install. If omitted, defaults to a per-root sentinel.

        The fake ``mpm`` binary also echoes ``MPM_WORKSPACE`` so the
        functional end-to-end test can verify the wrapper propagates the
        env var to the underlying real binary.
        """
        (root / "bin").mkdir(parents=True, exist_ok=True)
        if sentinel is None:
            sentinel = f"# fake-binary-{root.name}-{os.getpid()}\n"
        # mpm binary gets extra functional body: print MPM_WORKSPACE so we
        # can verify the wrapper execs the real binary with the env var set.
        mpm_body = sentinel + "echo \"${MPM_WORKSPACE:-}\"\n"
        for name in self.BINARIES:
            p = root / "bin" / name
            if name == "mpm":
                p.write_text(mpm_body)
            else:
                p.write_text(sentinel)
            p.chmod(0o755)
        self._last_sentinel = sentinel
        # Stage install.sh itself at the project root (not under
        # scripts/) — the canonical layout is install.sh at the repo
        # root and scripts/deploy.sh under scripts/. We copy install.sh
        # to the project root so the sourced-lib sees the same paths
        # the real installer would.
        shutil.copy(INSTALL_SH, root / "install.sh")
        lib = root / "install.sh.lib"
        lib.write_text(build_sourced_lib(INSTALL_SH.read_text()))

    def _drive(self, project_root: Path, prefix: Path, data_root: Path) -> dict:
        """Run phase_binaries in a subprocess with controlled env.

        Returns the subprocess result (stdout/stderr/returncode).
        ``PROJECT_ROOT``/``PREFIX``/``DATA_ROOT`` are exported so the
        unquoted heredoc in phase_binaries expands them into the wrapper.
        """
        driver = project_root / "_drive.sh"
        # install.sh declares PREFIX/DATA_ROOT/SERVICE_DST as bare
        # variable assignments at the top (NOT readonly), so any export
        # we do before sourcing is wiped. We source first, then assign
        # the test-controlled values, then invoke phase_binaries.
        driver.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            set -uo pipefail
            export PROJECT_ROOT={project_root}
            source {project_root}/install.sh.lib
            PREFIX={prefix}
            DATA_ROOT={data_root}
            SERVICE_DST=/dev/null
            export PREFIX DATA_ROOT SERVICE_DST
            phase_binaries
        """))
        driver.chmod(0o755)

        # Use --noprofile --norc to keep env hermetic; HOME is overridden
        # separately so PREFIX defaults don't leak in.
        env = os.environ.copy()
        env["HOME"] = str(self._tmp / "home")
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver)],
            capture_output=True,
            text=True,
            env=env,
        )
        return {
            "stdout": result.stdout,
            "stderr": result.stderr,
            "returncode": result.returncode,
        }

    def _assert_executable(self, path: Path, msg: str):
        self.assertTrue(path.exists(), f"missing: {path} ({msg})")
        mode = path.stat().st_mode
        self.assertTrue(
            mode & stat.S_IXUSR,
            f"not executable (mode={oct(mode & 0o777)}): {path} ({msg})",
        )

    def _assert_is_wrapper(self, path: Path, msg: str):
        text = path.read_bytes()
        self.assertTrue(
            text.startswith(b"#!"),
            f"wrapper must start with shebang: {path} ({msg})",
        )
        body = text.decode()
        self.assertIn("mpm.real", body, f"wrapper does not reference mpm.real: {path}")
        self.assertIn(
            "MPM_WORKSPACE", body,
            f"wrapper does not set MPM_WORKSPACE: {path}",
        )

    def _assert_is_real_binary(self, path: Path, sentinel: str, msg: str):
        """Assert that ``path`` is the real binary, not the wrapper.

        Real-binary check: file does NOT contain the wrapper template
        body ('mpm CLI wrapper'). For mpm binaries, also check the
        sentinel marker is present (which the wrapper would not have)."""
        text = path.read_text()
        self.assertNotIn(
            "mpm CLI wrapper", text,
            f"mpm.real must not contain wrapper text: {path} ({msg})",
        )
        if path.name in ("mpm", "mpm.real"):
            self.assertIn(
                sentinel, text,
                f"real-binary sentinel '{sentinel}' missing from: {path} ({msg})",
            )


class PhaseBinariesSeparateLayout(_PhaseBinariesDriver):
    """Case A: PROJECT_ROOT != PREFIX (the standard install layout)."""

    def test_installs_all_daemon_binaries(self):
        home = self._tmp / "home"
        home.mkdir()
        project = self._tmp / "src" / "mpm"
        self._build_fake_project(project)
        prefix = home / ".mpm"
        data_root = home / ".mpm"

        result = self._drive(project, prefix, data_root)
        self.assertEqual(
            result["returncode"], 0,
            f"phase_binaries failed in separate layout:\n"
            f"  stderr: {result['stderr']}\n"
            f"  stdout: {result['stdout']}",
        )

        for name in self.DAEMONS:
            self._assert_executable(prefix / "bin" / name, "daemon binary")

    def test_installs_mpm_real_and_wrapper(self):
        home = self._tmp / "home"
        home.mkdir()
        project = self._tmp / "src" / "mpm"
        self._build_fake_project(project)
        prefix = home / ".mpm"
        data_root = home / ".mpm"

        result = self._drive(project, prefix, data_root)
        self.assertEqual(result["returncode"], 0, result["stderr"])

        self._assert_is_real_binary(prefix / "bin" / "mpm.real",
                                    self._last_sentinel, "separate layout")
        self._assert_is_wrapper(prefix / "bin" / "mpm", "separate layout")

    def test_preserves_source_binaries(self):
        """In the separate layout, source binaries in PROJECT_ROOT/bin/
        must remain intact — phase_binaries copies them, doesn't move them."""
        home = self._tmp / "home"
        home.mkdir()
        project = self._tmp / "src" / "mpm"
        self._build_fake_project(project)
        prefix = home / ".mpm"
        data_root = home / ".mpm"

        result = self._drive(project, prefix, data_root)
        self.assertEqual(result["returncode"], 0, result["stderr"])

        # Source mpm still the real binary (not a wrapper).
        self._assert_is_real_binary(project / "bin" / "mpm",
                                    self._last_sentinel,
                                    "source preservation (separate)")


class PhaseBinariesSameLayout(_PhaseBinariesDriver):
    """Case B: PROJECT_ROOT == PREFIX (repo cloned to canonical prefix).

    This is the regression case the installer originally broke on.
    Without the fix, ``install -m 0755 $PROJECT_ROOT/bin/mpm-scheduler
    $PREFIX/bin/mpm-scheduler`` fails with 'are the same file' and
    ``set -e`` aborts the installer before the wrapper is written.
    """

    def test_does_not_fail_with_same_file_error(self):
        home = self._tmp / "home"
        home.mkdir()
        # Project IS the install prefix.
        project = home / ".mpm"
        self._build_fake_project(project)
        prefix = home / ".mpm"
        data_root = home / ".mpm"

        result = self._drive(project, prefix, data_root)
        # No 'are the same file' error from install(1). The installer
        # must reach and complete phase_binaries cleanly. (We match
        # coreutils' exact phrasing so we don't false-positive on the
        # installer's own log messages that mention "same file".)
        combined = result["stderr"] + result["stdout"]
        self.assertNotIn(
            "are the same file", combined,
            f"install(1) refused to copy a file onto itself — this is the bug:\n"
            f"  stderr: {result['stderr']}\n"
            f"  stdout: {result['stdout']}",
        )
        self.assertEqual(
            result["returncode"], 0,
            f"phase_binaries failed in same layout:\n"
            f"  stderr: {result['stderr']}\n"
            f"  stdout: {result['stdout']}",
        )

    def test_daemon_binaries_exist_at_prefix(self):
        home = self._tmp / "home"
        home.mkdir()
        project = home / ".mpm"
        self._build_fake_project(project)
        prefix = home / ".mpm"
        data_root = home / ".mpm"

        result = self._drive(project, prefix, data_root)
        self.assertEqual(result["returncode"], 0, result["stderr"])

        for name in self.DAEMONS:
            self._assert_executable(prefix / "bin" / name, "daemon (same layout)")

    def test_mpm_real_preserved_as_real_binary(self):
        """The real mpm binary must exist at $PREFIX/bin/mpm.real
        BEFORE the wrapper overwrites $PREFIX/bin/mpm."""
        home = self._tmp / "home"
        home.mkdir()
        project = home / ".mpm"
        self._build_fake_project(project)
        prefix = home / ".mpm"
        data_root = home / ".mpm"

        result = self._drive(project, prefix, data_root)
        self.assertEqual(result["returncode"], 0, result["stderr"])

        self._assert_is_real_binary(prefix / "bin" / "mpm.real",
                                    self._last_sentinel, "same layout")

    def test_wrapper_replaces_mpm_correctly(self):
        """$PREFIX/bin/mpm must be the wrapper, exec'ing mpm.real."""
        home = self._tmp / "home"
        home.mkdir()
        project = home / ".mpm"
        self._build_fake_project(project)
        prefix = home / ".mpm"
        data_root = home / ".mpm"

        result = self._drive(project, prefix, data_root)
        self.assertEqual(result["returncode"], 0, result["stderr"])

        self._assert_is_wrapper(prefix / "bin" / "mpm", "same layout")
        # Wrapper points at the absolute .real path.
        wrapper_text = (prefix / "bin" / "mpm").read_text()
        self.assertIn(
            str(prefix / "bin" / "mpm.real"), wrapper_text,
            "wrapper must exec an absolute path to mpm.real",
        )

    def test_wrapper_functional_end_to_end(self):
        """Functional check: invoking $PREFIX/bin/mpm in the same-layout
        case should exec the real binary (which writes our fake-binary
        sentinel to stdout) with MPM_WORKSPACE exported to DATA_ROOT."""
        home = self._tmp / "home"
        home.mkdir()
        project = home / ".mpm"
        self._build_fake_project(project)
        prefix = home / ".mpm"
        data_root = home / ".mpm"

        result = self._drive(project, prefix, data_root)
        self.assertEqual(result["returncode"], 0, result["stderr"])

        # The fake real binary is a #! sh script that echoes the
        # MPM_WORKSPACE env. We invoke the wrapper and assert the env
        # reaches the underlying binary.
        proc = subprocess.run(
            [str(prefix / "bin" / "mpm")],
            capture_output=True, text=True,
            env={"PATH": "/usr/bin:/bin"},  # hermetic
        )
        self.assertEqual(
            proc.returncode, 0,
            f"wrapper invocation failed: stderr={proc.stderr}",
        )
        self.assertEqual(
            proc.stdout.strip(), str(data_root),
            f"wrapper did not propagate MPM_WORKSPACE={data_root} to "
            f"mpm.real; got stdout={proc.stdout!r}",
        )


class PhaseBinariesIdempotency(_PhaseBinariesDriver):
    """Running phase_binaries twice in either layout must not break."""

    def test_same_layout_repeat_safe(self):
        home = self._tmp / "home"
        home.mkdir()
        project = home / ".mpm"
        self._build_fake_project(project)
        prefix = home / ".mpm"
        data_root = home / ".mpm"

        r1 = self._drive(project, prefix, data_root)
        self.assertEqual(r1["returncode"], 0, r1["stderr"])

        # Second run — wrapper detection should kick in (binary already
        # starts with '#!').
        r2 = self._drive(project, prefix, data_root)
        self.assertEqual(
            r2["returncode"], 0,
            f"second run failed: stderr={r2['stderr']}",
        )
        # mpm.real still the real binary, mpm still the wrapper.
        self._assert_is_wrapper(prefix / "bin" / "mpm", "idempotent run")
        self._assert_is_real_binary(prefix / "bin" / "mpm.real",
                                    self._last_sentinel, "idempotent run")


if __name__ == "__main__":
    unittest.main()
