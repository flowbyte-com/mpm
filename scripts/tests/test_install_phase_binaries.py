"""
test_install_phase_binaries.py — Regression coverage for install.sh

Pins the CURRENT contract for ``phase_binaries`` and ``phase_symlinks``
(post-``dd91fbf8``).

--------------------------------------------------------------------------
The contract these tests pin
--------------------------------------------------------------------------

  Case A (separate layout) — ``PROJECT_ROOT != PREFIX``
    All five binaries — ``mpm``, ``mpm-scheduler``, ``mpm-critic``,
    ``mpm-mcp``, ``mpm-telemetry`` — are copied into ``$PREFIX/bin/``
    via ``install -m 0755``. ``mpm`` is installed *directly*; the
    installer authors no file of its own into ``$PREFIX/bin/``.

  Case B (colocated checkout) — ``PROJECT_ROOT == PREFIX`` (the repo is
    cloned directly to ``$HOME/.mpm``, the canonical install prefix)
    This used to be the dangerous layout: build output lived at
    ``$PROJECT_ROOT/bin``, which here IS the directory the running
    services execute from, so an ordinary ``make build`` overwrote the
    live binaries and the installer then had to skip a
    ``install(1) ... are the same file`` copy. Build output now lives at
    ``$PROJECT_ROOT/.build/bin``, so the two are distinct directories and
    promotion is a plain copy. This layout is now ordinary.

  Case C (build output aliased onto the install)
    Forces the old broken state back — ``BUILD_DIR`` pointed at
    ``$PREFIX/bin`` — and asserts the phase REFUSES. It previously
    skipped the copy and reported success, which is a deployment that
    never happened.

  After the phase, in cases A and B:
    * ``$PREFIX/bin/<bin>`` is byte-identical to the build output
    * ``$PREFIX/bin/mpm.real`` does NOT exist
    * no file in ``$PREFIX/bin`` is a shell wrapper (none starts with
      ``#!``)
    * build output in ``$BUILD_DIR`` is copied, not moved

  Legacy cleanup — an install that finds ``$PREFIX/bin/mpm.real`` or
    ``$PREFIX/bin/mpm.pre-wrapper.*`` from an older install removes
    them. These artifacts must never be *produced*, but they may still
    exist on an operator's disk, so the migration path keeps real
    coverage.

  PATH links (``phase_symlinks``) — ``$HOME/.local/bin/mpm`` and
    ``$HOME/.local/bin/mpm-mcp`` are symlinks to the canonical
    ``$PREFIX/bin/`` binaries, and nothing else is linked.

--------------------------------------------------------------------------
Why this file was rewritten (2026-10-02)
--------------------------------------------------------------------------

The previous revision asserted the *superseded* wrapper contract:
``$PREFIX/bin/mpm.real`` holds the real ELF and ``$PREFIX/bin/mpm`` is
a generated ``#!`` wrapper that ``exec``s it. ``dd91fbf8`` ("drop mpm
wrapper in favour of compiled-binary layout") removed wrapper
generation from ``install.sh`` — the Go binary defaults
``MPM_WORKSPACE`` to ``$HOME/.mpm`` internally, so a shell shim adds
nothing — and the binary is now installed directly as ``mpm``.

The file was left asserting the old contract and produced 2 failures
and 3 errors. It was also invisible to every validation gate (no
supported target ran anything under ``scripts/tests/``), which is why
the drift went unnoticed; see the ``test-install-contract`` target in
the Makefile for the gate side of the fix.

Per-test disposition of the old revision:

  test_installs_all_daemon_binaries    STILL VALID, widened to include
                                       ``mpm`` (installed here now too)
  test_installs_mpm_real_and_wrapper   OBSOLETE — replaced by
                                       ``test_installs_compiled_mpm_directly``
                                       and ``test_produces_no_wrapper_artifacts``
  test_preserves_source_binaries       STILL VALID, kept as-is
  test_does_not_fail_with_same_file_error
                                       STILL VALID — but the mechanism it
                                       guards against is now impossible
                                       to reach accidentally (Case C
                                       proves the installer refuses when
                                       forced into it)
  test_daemon_binaries_exist_at_prefix STILL VALID, widened like the first
  test_mpm_real_preserved_as_real_binary
                                       OBSOLETE — folded into the
                                       no-``mpm.real`` assertion
  test_wrapper_replaces_mpm_correctly  OBSOLETE — folded into the
                                       no-shebang assertion
  test_wrapper_functional_end_to_end   The wrapper-specific expectation is
                                       obsolete; the underlying concern
                                       ("the installed ``mpm`` is directly
                                       runnable") is still valid and is kept
                                       as ``test_installed_mpm_is_directly_runnable``
  test_same_layout_repeat_safe         STILL VALID as an idempotency test;
                                       its two wrapper assertions were
                                       obsolete and are replaced

--------------------------------------------------------------------------
Fixture fidelity
--------------------------------------------------------------------------

The four daemon fakes are written with genuine ELF magic
(``\\x7fE`` + ``L``) rather than a shell shebang, so the
"not a shell wrapper" assertion is backed by the same first two bytes
``install.sh`` and the Makefile's stale-wrapper cleanup actually test
(``head -c 2 ... | grep -q '^#!'``). They are never executed.

The ``mpm`` fake is a runnable script so the functional test can invoke
it. It is a fixture stand-in, not a Go binary; the property under test
is that the installer *copies the build output verbatim* rather than
authoring a wrapper, which the byte-identity assertions check directly
and without relying on the fixture's own bytes.

These tests do NOT mock the installer — they source the real
install.sh's function definitions into a subprocess and drive
``phase_binaries`` directly, so any regression in the install logic
itself is caught. Everything happens under a ``tempfile`` directory with
a synthetic ``HOME``; no live install is performed and no real
workspace, database, service, or network is touched.
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


# Derive the repo root from this file's location rather than a
# hardcoded author-machine absolute path. The tests live at
# scripts/tests/<file>.py, so REPO_ROOT is two parents up:
# scripts/tests -> scripts -> <repo-root>. The previous literal
# "/home/v/workspace/projects/mpm" pointed at the original author's
# checkout and resolved to a non-existent path for every other user.
REPO_ROOT = Path(__file__).resolve().parent.parent.parent
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

# What a compiled binary starts with, and what a shell wrapper starts
# with. install.sh and the Makefile both discriminate on exactly this
# two-byte prefix.
ELF_MAGIC = b"\x7fE"
SHEBANG = b"#!"


def build_sourced_lib(install_text: str) -> str:
    """Return the install.sh script with path-resolution and main()
    invocations stripped, so it can be sourced for isolated testing."""
    out_lines = []
    for line in install_text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out_lines.append(line)
    return "\n".join(out_lines) + "\n"


class _InstallDriver(unittest.TestCase):
    """Shared scaffolding: stand up a fake project tree, drive
    install.sh functions in a child bash, assert on the result."""

    # The canonical binary set, in the order install.sh declares it.
    BINARIES = ("mpm", "mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry")
    DAEMONS = ("mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry")
    # Only these two get a PATH link.
    LINKED = ("mpm", "mpm-mcp")
    # Wrapper-era artifacts an older install may have left behind.
    LEGACY_ARTIFACTS = ("mpm.real", "mpm.pre-wrapper.20260101-000000")

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-install-test-"))
        self.addCleanup(self._cleanup)
        self.home = self._tmp / "home"
        self.home.mkdir()

    def _cleanup(self):
        shutil.rmtree(self._tmp, ignore_errors=True)

    # ------------------------------------------------------------------
    # fixture
    # ------------------------------------------------------------------
    def _build_fake_project(self, root: Path) -> None:
        """Populate a minimal project tree at ``root`` with the files
        install.sh needs to find.

        Each fake carries a per-root sentinel so tests can verify the
        installed file is the build output and not something the
        installer authored. Daemon fakes get real ELF magic; the ``mpm``
        fake is a runnable script so the functional test can execute it.
        """
        # Build output lives in .build/bin — a scratch subdirectory of the
        # checkout, and deliberately NOT the install prefix's bin/. The
        # fixture stages them as separate directories so the tests would
        # still pass if the installer copied onto itself.
        (root / ".build" / "bin").mkdir(parents=True, exist_ok=True)
        sentinel = f"fake-binary-{root.name}-{os.getpid()}\n"
        self._sentinel = sentinel

        for name in self.BINARIES:
            p = root / ".build" / "bin" / name
            if name == "mpm":
                # Runnable: echoes its sentinel and MPM_WORKSPACE so the
                # functional test can observe the environment the
                # installed binary sees. This fixture necessarily carries
                # a shebang (it must be executable), which is why the
                # "not a wrapper" byte check is scoped to the ELF-magic
                # daemons and mpm is covered by byte-identity instead.
                p.write_text(
                    "#!/usr/bin/env bash\n"
                    f"echo {sentinel.strip()}\n"
                    'echo "workspace=${MPM_WORKSPACE:-<unset>}"\n'
                )
            else:
                # ELF-magic stub. Never executed, only installed and
                # inspected, so real ELF bytes cost nothing and make the
                # shebang assertions faithful to production.
                p.write_bytes(ELF_MAGIC + b"L" + sentinel.encode())
            p.chmod(0o755)

        # Stage install.sh at the project root so the sourced lib sees
        # the same paths the real installer would.
        shutil.copy(INSTALL_SH, root / "install.sh")
        (root / "install.sh.lib").write_text(
            build_sourced_lib(INSTALL_SH.read_text())
        )

    def _assert_fixture_is_real(self) -> None:
        """Non-vacuity guard. If the fixture is wrong, every assertion
        built on it passes for the wrong reason."""
        for name in self.BINARIES:
            p = self._project / ".build" / "bin" / name
            self.assertTrue(p.is_file(), f"fixture missing binary {name}")
            self.assertIn(self._sentinel, p.read_bytes().decode(errors="replace"))
        for name in self.DAEMONS:
            head = (self._project / ".build" / "bin" / name).read_bytes()[:2]
            self.assertEqual(
                head, ELF_MAGIC,
                f"daemon fixture {name} must carry ELF magic so the "
                f"not-a-wrapper assertion is faithful; got {head!r}",
            )
            self.assertNotEqual(head, SHEBANG)

    # ------------------------------------------------------------------
    # driver
    # ------------------------------------------------------------------
    def _drive(self, project_root: Path, prefix: Path, data_root: Path,
               phases: str = "phase_binaries",
               build_dir: Path | None = None) -> dict:
        """Run the named install.sh phases in a subprocess with
        controlled env, and return stdout/stderr/returncode.

        ``PROJECT_ROOT``/``BUILD_DIR``/``PREFIX``/``DATA_ROOT`` are assigned
        AFTER the sourced lib is read, because install.sh declares
        PREFIX/DATA_ROOT as bare (non-readonly) assignments at the top that
        would wipe any pre-source export. ``LOCAL_BIN`` is ``readonly`` and
        derived from ``HOME``, which the caller controls via the child env.

        ``build_dir`` defaults to the fixture's ``<project>/.build/bin``,
        i.e. what ``make print-build-dir`` reports. Pass it explicitly to
        force an aliased/misconfigured build output and prove the phase
        refuses it.
        """
        if build_dir is None:
            build_dir = project_root / ".build" / "bin"
        driver = project_root / "_drive.sh"
        driver.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            set -uo pipefail
            export PROJECT_ROOT={project_root}
            source {project_root}/install.sh.lib
            BUILD_DIR={build_dir}
            PREFIX={prefix}
            DATA_ROOT={data_root}
            SERVICE_DST=/dev/null
            export BUILD_DIR PREFIX DATA_ROOT SERVICE_DST
            {phases}
        """))
        driver.chmod(0o755)

        # --noprofile --norc keeps the child hermetic; HOME is overridden
        # so the readonly LOCAL_BIN resolves inside the temp tree.
        env = os.environ.copy()
        env["HOME"] = str(self.home)
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

    # ------------------------------------------------------------------
    # assertions
    # ------------------------------------------------------------------
    def _assert_ok(self, result: dict, what: str):
        self.assertEqual(
            result["returncode"], 0,
            f"{what} failed:\n  stderr: {result['stderr']}\n"
            f"  stdout: {result['stdout']}",
        )

    def _assert_executable(self, path: Path, msg: str):
        self.assertTrue(path.exists(), f"missing: {path} ({msg})")
        mode = path.stat().st_mode
        self.assertTrue(
            mode & stat.S_IXUSR,
            f"not executable (mode={oct(mode & 0o777)}): {path} ({msg})",
        )

    def _assert_is_build_output(self, path: Path, msg: str):
        """Assert ``path`` is a verbatim copy of the corresponding
        build artifact — the strongest form of "installed directly"."""
        src = self._project / ".build" / "bin" / path.name
        self.assertTrue(src.is_file(), f"no build output for {path.name}")
        self.assertEqual(
            path.read_bytes(), src.read_bytes(),
            f"{path} is not the build output ({msg}); the installer must "
            f"copy $BUILD_DIR/{path.name} verbatim",
        )

    def _assert_not_a_wrapper(self, path: Path, msg: str):
        """Byte-level check. Only meaningful for the ELF-magic daemon
        fixtures — the ``mpm`` fixture must itself be a runnable script
        and so necessarily starts with ``#!``. For ``mpm`` the equivalent
        guarantee is byte-identity with the build output (see
        ``_assert_is_build_output`` and its negative control)."""
        head = path.read_bytes()[:2]
        self.assertNotEqual(
            head, SHEBANG,
            f"{path} is a shell wrapper ({msg}); phase_binaries must never "
            f"author a wrapper — the compiled binary is installed directly",
        )

    def _assert_absent(self, path: Path, msg: str):
        self.assertFalse(
            path.exists(), f"unexpected artifact present: {path} ({msg})"
        )

    def _assert_no_wrapper_artifacts(self, prefix: Path, msg: str):
        """No wrapper-era artifact exists anywhere in the bin dir."""
        bindir = prefix / "bin"
        self._assert_absent(bindir / "mpm.real", f"no mpm.real ({msg})")
        stale = sorted(bindir.glob("mpm.pre-wrapper.*"))
        self.assertEqual(
            stale, [], f"stale wrapper backups remain ({msg}): {stale}"
        )
        for name in self.DAEMONS:
            self._assert_not_a_wrapper(bindir / name, msg)
        # mpm: covered by byte-identity, which a wrapper could not satisfy.
        self._assert_is_build_output(bindir / "mpm", msg)


# ======================================================================
# Case A — separate layout
# ======================================================================
class PhaseBinariesSeparateLayout(_InstallDriver):
    """PROJECT_ROOT != PREFIX (the standard install layout)."""

    def setUp(self):
        super().setUp()
        self._project = self._tmp / "src" / "mpm"
        self._build_fake_project(self._project)
        self._assert_fixture_is_real()
        self._prefix = self.home / ".mpm"

    def test_installs_all_binaries(self):
        result = self._drive(self._project, self._prefix, self._prefix)
        self._assert_ok(result, "phase_binaries (separate layout)")

        for name in self.BINARIES:
            path = self._prefix / "bin" / name
            self._assert_executable(path, "installed binary")
            self._assert_is_build_output(path, "separate layout")

    def test_installs_compiled_mpm_directly(self):
        """mpm is installed as the compiled binary, not renamed away to
        mpm.real and not replaced by a generated wrapper."""
        result = self._drive(self._project, self._prefix, self._prefix)
        self._assert_ok(result, "phase_binaries (separate layout)")

        mpm = self._prefix / "bin" / "mpm"
        self._assert_executable(mpm, "mpm")
        self._assert_is_build_output(mpm, "separate layout")
        self.assertIn(
            self._sentinel, mpm.read_text(),
            f"installed mpm lost the build-output sentinel ({self._sentinel!r})",
        )

    def test_produces_no_wrapper_artifacts(self):
        """The defining post-dd91fbf8 invariant: the installer writes
        nothing of its own into $PREFIX/bin/."""
        result = self._drive(self._project, self._prefix, self._prefix)
        self._assert_ok(result, "phase_binaries (separate layout)")

        self._assert_no_wrapper_artifacts(
            self._prefix, "separate layout")

    def test_preserves_source_binaries(self):
        """In the separate layout, source binaries in PROJECT_ROOT/bin/
        must remain intact — phase_binaries copies, it does not move."""
        result = self._drive(self._project, self._prefix, self._prefix)
        self._assert_ok(result, "phase_binaries (separate layout)")

        for name in self.BINARIES:
            src = self._project / ".build" / "bin" / name
            self._assert_executable(src, "source binary")
            self.assertIn(
                self._sentinel, src.read_bytes().decode(errors="replace"),
                f"source {name} was modified or consumed",
            )


# ======================================================================
# Case B — checkout IS the install prefix (the canonical $HOME/.mpm layout)
# ======================================================================
class PhaseBinariesColocatedCheckout(_InstallDriver):
    """PROJECT_ROOT == PREFIX: the repo IS cloned to the canonical prefix.

    Historically this was the dangerous layout. `make build` wrote to
    `bin/`, which here IS the install directory systemd executes from, so
    an ordinary build overwrote the live binaries; the installer then hit
    `install(1): 'X' and 'X' are the same file` and needed an `-ef` skip to
    survive. Both halves of that are gone.

    It is now an ordinary working layout: build output is
    `$PROJECT_ROOT/.build/bin`, the install target is `$PROJECT_ROOT/bin`,
    and they are two different directories. Promotion is a plain copy.
    """

    def setUp(self):
        super().setUp()
        # The project IS the install prefix.
        self._project = self.home / ".mpm"
        self._build_fake_project(self._project)
        self._assert_fixture_is_real()
        self._prefix = self._project

    def test_build_output_and_install_target_are_distinct(self):
        """The precondition the rest of this class relies on. If these
        ever collide, every other test here is asserting the wrong thing."""
        build_dir = self._project / ".build" / "bin"
        install_dir = self._prefix / "bin"
        self.assertNotEqual(
            build_dir.resolve(), install_dir.resolve(),
            "fixture is degenerate: build output and install target must "
            "be distinct directories",
        )

    def test_does_not_fail_with_same_file_error(self):
        result = self._drive(self._project, self._prefix, self._prefix)
        # Match coreutils' exact phrasing so we don't false-positive on
        # the installer's own log messages, which mention "same file".
        combined = result["stderr"] + result["stdout"]
        self.assertNotIn(
            "are the same file", combined,
            f"install(1) refused to copy a file onto itself — this is the bug:\n"
            f"  stderr: {result['stderr']}\n  stdout: {result['stdout']}",
        )
        self._assert_ok(result, "phase_binaries (colocated checkout)")

    def test_binaries_exist_at_prefix(self):
        result = self._drive(self._project, self._prefix, self._prefix)
        self._assert_ok(result, "phase_binaries (colocated checkout)")

        for name in self.BINARIES:
            path = self._prefix / "bin" / name
            self._assert_executable(path, "binary (colocated checkout)")
            self._assert_is_build_output(path, "colocated checkout")

    def test_produces_no_wrapper_artifacts(self):
        result = self._drive(self._project, self._prefix, self._prefix)
        self._assert_ok(result, "phase_binaries (colocated checkout)")

        self._assert_no_wrapper_artifacts(self._prefix, "colocated checkout")

    def test_build_output_survives_promotion(self):
        """The split's whole purpose: promoting to the install must leave
        the build artifacts in place, because they are now somewhere
        else rather than the same files."""
        result = self._drive(self._project, self._prefix, self._prefix)
        self._assert_ok(result, "phase_binaries (colocated checkout)")

        for name in self.BINARIES:
            src = self._project / ".build" / "bin" / name
            self._assert_executable(src, "build output after promotion")
            self.assertIn(
                self._sentinel, src.read_bytes().decode(errors="replace"),
                f"build output {name} was modified or consumed by install",
            )

    def test_installed_mpm_is_directly_runnable(self):
        """Functional check: the installed $PREFIX/bin/mpm runs, and
        observes the caller's environment directly. In the wrapper era
        this invocation went through a shim that exported
        MPM_WORKSPACE=$DATA_ROOT; the compiled binary receives nothing
        the caller did not set, which is the whole reason the wrapper
        was dropped (internal/core/config defaults it to $HOME/.mpm)."""
        result = self._drive(self._project, self._prefix, self._prefix)
        self._assert_ok(result, "phase_binaries (colocated checkout)")

        proc = subprocess.run(
            [str(self._prefix / "bin" / "mpm")],
            capture_output=True, text=True,
            env={"PATH": "/usr/bin:/bin"},  # hermetic
        )
        self.assertEqual(
            proc.returncode, 0,
            f"installed mpm is not directly runnable: stderr={proc.stderr}",
        )
        self.assertIn(self._sentinel, proc.stdout)
        self.assertIn(
            "workspace=<unset>", proc.stdout,
            f"the installed mpm must not have a wrapper injecting "
            f"MPM_WORKSPACE; got stdout={proc.stdout!r}",
        )


# ======================================================================
# Case C — build output ALIASED onto the install target (must refuse)
# ======================================================================
class BuildOutputAliasedOntoInstall(_InstallDriver):
    """The defect this whole split exists to make impossible, forced
    back into existence to prove the installer rejects it.

    Reached when `.build/bin` is symlinked at the install's `bin/`, or
    BUILD_DIR is overridden to the install path. Previously the `-ef`
    branch SKIPPED the copy and reported success — a deployment that never
    happened. It is now a hard error.
    """

    def setUp(self):
        super().setUp()
        self._project = self._tmp / "src" / "mpm"
        self._build_fake_project(self._project)
        self._prefix = self.home / ".mpm"
        # Model the alias faithfully: the build artifacts ARE the install
        # files, so src and dst are the same inodes. Staging empty files
        # would trip the existence check first and never reach the
        # collision handling this class is about.
        (self._prefix / "bin").mkdir(parents=True)
        for name in self.BINARIES:
            shutil.copy(
                self._project / ".build" / "bin" / name,
                self._prefix / "bin" / name,
            )
        self._aliased_build_dir = self._prefix / "bin"

    def _drive_aliased(self) -> dict:
        return self._drive(
            self._project, self._prefix, self._prefix,
            build_dir=self._aliased_build_dir,
        )

    def test_refuses_instead_of_skipping(self):
        result = self._drive_aliased()
        combined = result["stderr"] + result["stdout"]
        self.assertNotEqual(
            result["returncode"], 0,
            "installer reported success while refusing to copy anything — "
            "that is the 'success over a partial install' failure mode:\n"
            f"  stderr: {result['stderr']}",
        )
        self.assertIn(
            "same file", combined,
            f"expected the installer to name the same-file collision:\n"
            f"  stderr: {result['stderr']}\n  stdout: {result['stdout']}",
        )

    def test_does_not_report_a_skip(self):
        """The old wording. Its absence is the behavioural change."""
        result = self._drive_aliased()
        self.assertNotIn(
            "skipping copy", result["stderr"] + result["stdout"],
            "installer still SKIPS the copy when src and dst are the same "
            f"file; it must refuse. stderr={result['stderr']!r}",
        )

    def test_leaves_install_target_untouched(self):
        """A refusal must not have half-modified anything.

        The files are already present (that is the premise), so the
        invariant is that they are byte-identical afterwards — the phase
        died before copying, rather than truncating and rewriting them.
        """
        before = {
            name: (self._prefix / "bin" / name).read_bytes()
            for name in self.BINARIES
        }
        result = self._drive_aliased()
        self.assertNotEqual(result["returncode"], 0)
        for name in self.BINARIES:
            path = self._prefix / "bin" / name
            self.assertEqual(
                path.read_bytes(), before[name],
                f"{path} was modified by a phase that refused to install",
            )


# ======================================================================
# Legacy cleanup — artifacts must never be produced, but may still be
# found on an operator's disk from a pre-dd91fbf8 install.
# ======================================================================
class LegacyWrapperCleanup(_InstallDriver):
    """install.sh promises to migrate the wrapper+real layout forward."""

    def setUp(self):
        super().setUp()
        self._project = self._tmp / "src" / "mpm"
        self._build_fake_project(self._project)
        self._assert_fixture_is_real()
        self._prefix = self.home / ".mpm"
        self._bindir = self._prefix / "bin"
        self._bindir.mkdir(parents=True)

    def _seed_legacy(self) -> None:
        for name in self.LEGACY_ARTIFACTS:
            (self._bindir / name).write_bytes(ELF_MAGIC + b"Lold-wrapper-era\n")

    def test_removes_legacy_wrapper_artifacts(self):
        self._seed_legacy()
        result = self._drive(self._project, self._prefix, self._prefix)
        self._assert_ok(result, "phase_binaries with legacy artifacts")

        for name in self.LEGACY_ARTIFACTS:
            self._assert_absent(
                self._bindir / name, "install must clean up legacy layout")
        self._assert_no_wrapper_artifacts(self._prefix, "after legacy cleanup")

    def test_still_installs_binaries_after_legacy_cleanup(self):
        """Cleanup must not abort the phase before the real install."""
        self._seed_legacy()
        result = self._drive(self._project, self._prefix, self._prefix)
        self._assert_ok(result, "phase_binaries with legacy artifacts")

        for name in self.BINARIES:
            self._assert_executable(self._bindir / name, "after legacy cleanup")

    def test_does_not_recreate_legacy_artifacts_on_reinstall(self):
        self._seed_legacy()
        self._assert_ok(
            self._drive(self._project, self._prefix, self._prefix),
            "first install")
        self._assert_ok(
            self._drive(self._project, self._prefix, self._prefix),
            "second install")
        self._assert_no_wrapper_artifacts(self._prefix, "after reinstall")


# ======================================================================
# PATH links
# ======================================================================
class PhaseSymlinks(_InstallDriver):
    """$HOME/.local/bin/{mpm,mpm-mcp} are symlinks to $PREFIX/bin/."""

    def setUp(self):
        super().setUp()
        self._project = self._tmp / "src" / "mpm"
        self._build_fake_project(self._project)
        self._assert_fixture_is_real()
        self._prefix = self.home / ".mpm"
        self._local_bin = self.home / ".local" / "bin"

    def _run_both_phases(self) -> dict:
        return self._drive(
            self._project, self._prefix, self._prefix,
            phases="phase_binaries\nphase_symlinks",
        )

    def test_links_only_mpm_and_mpm_mcp(self):
        result = self._run_both_phases()
        self._assert_ok(result, "phase_binaries + phase_symlinks")

        for name in self.LINKED:
            link = self._local_bin / name
            self.assertTrue(
                link.is_symlink(),
                f"{link} is not a symlink; the canonical layout installs "
                f"the compiled binary at $PREFIX/bin and links it onto PATH",
            )
            self.assertEqual(
                os.readlink(link), str(self._prefix / "bin" / name),
                f"{link} must point at the canonical $PREFIX binary",
            )
            self.assertTrue(
                (link.resolve()).is_file(),
                f"{link} resolves to nothing",
            )

        for name in self.DAEMONS:
            if name in self.LINKED:
                continue
            self._assert_absent(
                self._local_bin / name,
                "only mpm and mpm-mcp get a PATH link; the daemons are "
                "invoked by the scheduler, not by name on PATH",
            )

    def test_replaces_a_stale_path_entry(self):
        """An older install may have left a regular file where the link
        belongs; rm -f then ln -s must replace it, not fail."""
        self._local_bin.mkdir(parents=True)
        stale = self._local_bin / "mpm"
        stale.write_text("#!/bin/sh\necho stale\n")
        stale.chmod(0o755)

        result = self._run_both_phases()
        self._assert_ok(result, "phase_symlinks over a stale regular file")

        self.assertTrue(
            stale.is_symlink(),
            f"{stale} was not replaced by a symlink",
        )
        self.assertEqual(os.readlink(stale), str(self._prefix / "bin" / "mpm"))


# ======================================================================
# Idempotency
# ======================================================================
class PhaseBinariesIdempotency(_InstallDriver):
    """Re-running the phase must be safe in either layout."""

    def _assert_stable(self, prefix: Path, msg: str):
        for name in self.BINARIES:
            path = prefix / "bin" / name
            self._assert_executable(path, msg)
            self._assert_is_build_output(path, msg)
        self._assert_no_wrapper_artifacts(prefix, msg)

    def test_colocated_checkout_repeat_safe(self):
        """Re-running against a checkout that IS the prefix must be a
        plain, repeatable copy. There is no `-ef` skip to take any more:
        the second run copies the same distinct files the first one did.
        """
        self._project = self.home / ".mpm"
        self._build_fake_project(self._project)
        self._assert_fixture_is_real()
        self._prefix = self._project

        self._assert_ok(
            self._drive(self._project, self._prefix, self._prefix), "first run")
        self._assert_stable(self._prefix, "after first run")

        r2 = self._drive(self._project, self._prefix, self._prefix)
        self._assert_ok(r2, "second run")
        self.assertNotIn(
            "skipping copy", r2["stderr"],
            "the -ef skip is gone; a second colocated run is an ordinary "
            f"copy. stderr={r2['stderr']!r}",
        )
        self._assert_stable(self._prefix, "after second run")

    def test_separate_layout_repeat_safe(self):
        self._project = self._tmp / "src" / "mpm"
        self._build_fake_project(self._project)
        self._assert_fixture_is_real()
        self._prefix = self.home / ".mpm"

        self._assert_ok(
            self._drive(self._project, self._prefix, self._prefix), "first run")
        self._assert_ok(
            self._drive(self._project, self._prefix, self._prefix), "second run")
        self._assert_stable(self._prefix, "after two separate-layout runs")


# ======================================================================
# Negative controls — prove the assertions above can actually fail
# ======================================================================
class NegativeControls(_InstallDriver):
    """If the checkers below are vacuous, every behavioural test above
    passes for the wrong reason. Each control reproduces the *superseded*
    wrapper-era layout and asserts the checker rejects it."""

    def setUp(self):
        super().setUp()
        self._project = self._tmp / "src" / "mpm"
        self._build_fake_project(self._project)
        self._prefix = self.home / ".mpm"
        self._bindir = self._prefix / "bin"
        self._bindir.mkdir(parents=True)

    def test_build_output_checker_rejects_a_wrapper(self):
        """The exact shape the old contract produced: the real binary
        moved aside to mpm.real, a generated wrapper at mpm."""
        mpm = self._bindir / "mpm"
        (self._bindir / "mpm.real").write_bytes(
            (self._project / ".build" / "bin" / "mpm").read_bytes())
        mpm.write_text(
            "#!/usr/bin/env bash\n"
            'export MPM_WORKSPACE=/somewhere\n'
            f'exec "{self._bindir / "mpm.real"}" "$@"\n'
        )
        with self.assertRaises(AssertionError) as ctx:
            self._assert_is_build_output(mpm, "negative control")
        self.assertIn("not the build output", str(ctx.exception))

    def test_wrapper_artifact_checker_rejects_a_populated_bin_dir(self):
        (self._bindir / "mpm.real").write_bytes(ELF_MAGIC + b"Lold\n")
        with self.assertRaises(AssertionError) as ctx:
            self._assert_no_wrapper_artifacts(self._prefix, "negative control")
        self.assertIn("mpm.real", str(ctx.exception))

    def test_not_a_wrapper_checker_rejects_a_shebang(self):
        (self._bindir / "mpm-critic").write_text("#!/bin/sh\necho hi\n")
        with self.assertRaises(AssertionError) as ctx:
            self._assert_not_a_wrapper(
                self._bindir / "mpm-critic", "negative control")
        self.assertIn("shell wrapper", str(ctx.exception))


# ======================================================================
# Static invariants over install.sh itself
# ======================================================================
class ContractStillDeclaredInInstallScript(unittest.TestCase):
    """A guard against the contract being silently dropped from the
    source. The behavioural tests above would fail loudly, but these
    pin the *declaration* so a future rewrite that empties the binary
    list or deletes the -ef guard is diagnosable at the declaration."""

    def setUp(self):
        self.src = INSTALL_SH.read_text()

    def test_binary_list_is_the_canonical_five(self):
        m = re.search(r"for bin in ([^\n;]+); do", self.src)
        self.assertIsNotNone(m, "could not find the binary loop in phase_binaries")
        declared = m.group(1).split()
        self.assertEqual(sorted(declared), sorted(_InstallDriver.BINARIES))

    def test_same_inode_guard_is_present(self):
        """The same-inode test is retained, but its consequence INVERTED.

        It used to select a "skip the copy" branch. That branch was
        reachable only when the build output and the install target were
        the same file — the very condition the build/install split now
        prevents. It now selects a refusal (``die``), because reaching it
        means something aliased the two directories, and reporting a
        successful install of a binary that was never copied is the exact
        failure this guard exists to prevent.
        """
        self.assertRegex(self.src, r'if \[ "\$src" -ef "\$dst" \]; then')
        self.assertNotIn(
            "skipping copy", self.src,
            "the -ef branch must refuse, not skip; a skip would report a "
            "deployment that did not happen",
        )

    def test_legacy_cleanup_block_is_present(self):
        self.assertIn('rm -f "$PREFIX/bin/mpm.real"', self.src)
        self.assertIn('rm -f "$PREFIX/bin/mpm.pre-wrapper."*', self.src)

    def test_no_wrapper_is_generated(self):
        """The installer must not contain the wrapper template the old
        contract produced."""
        self.assertNotIn("mpm CLI wrapper", self.src)
        self.assertNotIn("mpm.real\" \\", self.src)

    def test_symlink_names_are_mpm_and_mpm_mcp(self):
        m = re.search(r"for name in (mpm [^\n;]+); do", self.src)
        self.assertIsNotNone(m, "could not find the symlink loop")
        self.assertEqual(sorted(m.group(1).split()),
                         sorted(_InstallDriver.LINKED))


if __name__ == "__main__":
    unittest.main()
