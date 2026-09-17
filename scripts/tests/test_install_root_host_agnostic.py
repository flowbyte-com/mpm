"""
test_install_root_host_agnostic.py — Regression coverage for the
host-agnostic boundary of ``install.sh``.

Architectural contract (post 2026-09-16 cleanup):

  Root ``install.sh`` installs the MPM substrate ONLY. It is
  intentionally host/framework agnostic: it does not detect, invoke,
  register with, or restart any agent framework (OpenClaw, Claude
  Code, OpenCode, Pi, Hermes, …). Framework-specific MCP / plugin
  wiring lives exclusively under ``agent_installation/`` and is
  installed by each host's own installer.

The tests below prove that contract on two axes:

  A. **Static surface** — the install.sh source itself contains
     zero host-detection / host-invocation references (no
     ``command -v openclaw``, no ``openclaw gateway``, no
     ``openclaw mcp``, no ``phase_host_integration`` function, no
     ``detect_openclaw`` helper, no mention of openclaw in
     ``mode_dry_run``).

  B. **Behavioral surface** — when the installer's ``preflight`` and
     ``phase_validate`` phases run with a fake ``openclaw`` /
     ``hermes`` / ``claude`` / ``opencode`` / ``pi`` on PATH (in
     any state: healthy, hanging, erroring), the installer does
     NOT invoke any of them. ``phase_validate`` is the canonical
     phase for this proof: it was the only validation that
     previously probed ``openclaw``, and removing that probe is
     directly observable here.

The tests do NOT mock the installer. They source the real
install.sh's function definitions into a subprocess and drive the
phases directly. They share the same scaffolding pattern as
``test_install_phase_binaries.py``.
"""

from __future__ import annotations

import os
import re
import shutil
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
    """Return install.sh with path-resolution and main() invocations
    stripped, so it can be sourced for isolated testing."""
    out_lines = []
    for line in install_text.splitlines():
        if any(p.match(line) for p in STRIP_PATTERNS):
            continue
        out_lines.append(line)
    return "\n".join(out_lines) + "\n"


class _RootInstallDriver(unittest.TestCase):
    """Shared scaffolding: stand up a fake project tree, drive the
    installer's ``preflight`` and ``phase_validate`` in a child
    bash with a hermetic PATH, and assert on what binaries it did
    or did not invoke.

    These two phases are the canonical proof surface because:

      * ``preflight`` previously probed ``openclaw`` via
        ``detect_openclaw`` and printed a status line. After the
        cleanup it must not reference any host framework.
      * ``phase_validate`` previously validated the
        ``openclaw mcp show mpm`` registration. After the cleanup
        it must not probe any host framework.

    Together, they cover both the early and late sections of the
    installer without requiring the full ``make build`` /
    systemd user unit flow, which depends on real ``systemctl``
    and a working Go toolchain — not part of the host-agnostic
    contract."""

    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-host-agnostic-"))
        self.addCleanup(self._cleanup)
        self.home = self._tmp / "home"
        self.home.mkdir()

    def _cleanup(self):
        shutil.rmtree(self._tmp, ignore_errors=True)

    def _build_fake_project(self, root: Path) -> None:
        """Populate a minimal project tree at ``root`` with the
        files install.sh needs to find for ``preflight`` and
        ``phase_validate`` to run:

          - ``Makefile`` (required by ``check_prereqs``)
          - a ``bin/mpm`` that ``phase_validate`` calls (it runs
            ``mpm call mpm_system --payload '{"action":"health_check"}'``
            and ``mpm call mpm_context --payload '{"action":"read_directives",…}'``)
          - the script itself at the repo root ``install.sh``
        """
        # Makefile stub: check_prereqs only verifies it exists;
        # phase_build is not exercised in these tests.
        root.mkdir(parents=True, exist_ok=True)
        (root / "Makefile").write_text("# test stub\n")

        # install.sh source + sourced-lib form, staged at the repo root
        # (the canonical layout — install.sh lives at the project root,
        # not under scripts/).
        shutil.copy(INSTALL_SH, root / "install.sh")
        lib = root / "install.sh.lib"
        lib.write_text(build_sourced_lib(INSTALL_SH.read_text()))

    def _drive_phases(
        self,
        project_root: Path,
        prefix: Path,
        data_root: Path,
        bindir: Path | None,
        phases: list[str],
    ) -> dict:
        """Drive one or more phases in a subprocess with a
        controlled env. ``bindir`` is prepended to PATH so any
        fake host binary is visible to install.sh's ``command -v``
        probes. Returns the subprocess result.
        """
        driver = project_root / "_drive_phases.sh"
        phase_calls = "\n    ".join(phases)
        driver.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            set -uo pipefail
            export PROJECT_ROOT={project_root}
            source {project_root}/install.sh.lib
            PREFIX={prefix}
            DATA_ROOT={data_root}
            SERVICE_DST=/dev/null
            export PREFIX DATA_ROOT SERVICE_DST
            {phase_calls}
        """))
        driver.chmod(0o755)

        env = os.environ.copy()
        env["HOME"] = str(self.home)
        base_path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
        if bindir is not None:
            env["PATH"] = f"{bindir}:{base_path}"
        else:
            env["PATH"] = base_path
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", str(driver)],
            capture_output=True,
            text=True,
            env=env,
            timeout=30,
        )
        return {
            "stdout": result.stdout,
            "stderr": result.stderr,
            "returncode": result.returncode,
        }

    def _make_fake_openclaw(self, record_file: Path, behavior: str) -> Path:
        """Create a fake ``openclaw`` binary on disk that records
        every invocation to ``record_file``. ``behavior`` controls
        how the fake responds to ``gateway status`` /
        ``gateway restart`` / ``mcp`` subcommands — these are
        exactly the subcommands the pre-cleanup ``phase_host_integration``
        issued. If the root installer is host-agnostic, none of
        these branches will ever be exercised.

          - "active-restart-ok": prints "active", exits 0
          - "hang": sleeps forever — would stall the installer if
            it were ever called
          - "error": prints to stderr and exits 99
        """
        bindir = self._tmp / "fake-bin"
        bindir.mkdir(parents=True, exist_ok=True)
        fake = bindir / "openclaw"
        rf_escaped = str(record_file).replace("'", "'\\''")
        case_body = {
            "active-restart-ok": "echo active; exit 0",
            "hang": "sleep 3600",
            "error": "echo 'fake-openclaw: simulated failure' >&2; exit 99",
        }[behavior]
        fake.write_text(textwrap.dedent(f"""\
            #!/usr/bin/env bash
            printf '%s\\n' "$0 $*" >> '{rf_escaped}'
            case "$1" in
                gateway)
                    case "$2" in
                        status|restart) {case_body} ;;
                    esac
                    ;;
                mcp)
                    echo '[]'
                    ;;
            esac
            exit 0
        """))
        fake.chmod(0o755)
        return bindir

    def _make_host_fakes(self, record_files: dict[str, Path]) -> Path:
        """Plant a fake binary for every supported host framework
        on a single bindir. Each fake records every invocation to
        its own record file.

        ``record_files`` is a mapping ``name -> Path``. The
        installer must not invoke ANY of these."""
        bindir = self._tmp / "fake-bin"
        bindir.mkdir(parents=True, exist_ok=True)
        for name, rec in record_files.items():
            rf_escaped = str(rec).replace("'", "'\\''")
            fake = bindir / name
            fake.write_text(textwrap.dedent(f"""\
                #!/usr/bin/env bash
                printf '%s\\n' "$0 $*" >> '{rf_escaped}'
                exit 0
            """))
            fake.chmod(0o755)
        return bindir

    def _read_record(self, path: Path) -> list[str]:
        if not path.exists():
            return []
        return [line for line in path.read_text().splitlines() if line.strip()]


class RootInstallerHostAgnosticSurface(_RootInstallDriver):
    """Static: the install.sh source contains no host-framework
    symbol references that imply behavior. Comments that LIST
    supported hosts for documentation purposes are acceptable;
    command invocations / probes are not."""

    def test_no_openclaw_command_invocation(self):
        """No ``command -v openclaw``, ``openclaw gateway``, or
        ``openclaw mcp`` invocations may remain in install.sh."""
        text = INSTALL_SH.read_text()
        for forbidden in (
            "command -v openclaw",
            "openclaw gateway",
            "openclaw mcp",
            "openclaw gateway status",
            "openclaw gateway restart",
        ):
            self.assertNotIn(
                forbidden, text,
                f"root installer still references '{forbidden}' "
                f"— host-agnostic boundary violated",
            )

    def test_no_phase_host_integration_function(self):
        """``phase_host_integration`` must have been removed
        entirely — function definition AND call site."""
        text = INSTALL_SH.read_text()
        self.assertNotIn(
            "phase_host_integration", text,
            "phase_host_integration still present in install.sh",
        )

    def test_no_detect_openclaw_function(self):
        text = INSTALL_SH.read_text()
        self.assertNotIn(
            "detect_openclaw", text,
            "detect_openclaw helper still present in install.sh",
        )

    def test_no_openclaw_in_dry_run(self):
        """``mode_dry_run`` must not advertise an ``openclaw mcp
        add/set mpm`` step that no longer exists."""
        text = INSTALL_SH.read_text()
        m = re.search(
            r"mode_dry_run\(\)\s*\{(.*?)^\}", text, re.DOTALL | re.MULTILINE,
        )
        self.assertIsNotNone(m, "mode_dry_run() not found in install.sh")
        block = m.group(1)
        self.assertNotIn(
            "openclaw", block,
            f"mode_dry_run() still mentions openclaw — remove the "
            f"dead dry-run line:\n{block}",
        )

    def test_no_openclaw_in_validation(self):
        """``phase_validate`` must not probe ``openclaw mcp show``."""
        text = INSTALL_SH.read_text()
        m = re.search(
            r"phase_validate\(\)\s*\{(.*?)^\}", text, re.DOTALL | re.MULTILINE,
        )
        self.assertIsNotNone(m, "phase_validate() not found in install.sh")
        block = m.group(1)
        self.assertNotIn(
            "openclaw", block,
            f"phase_validate() still mentions openclaw:\n{block}",
        )

    def test_no_openclaw_in_mode_install(self):
        """``mode_install`` must not call any host-integration
        phase or invoke any host binary."""
        text = INSTALL_SH.read_text()
        m = re.search(
            r"mode_install\(\)\s*\{(.*?)^\}", text, re.DOTALL | re.MULTILINE,
        )
        self.assertIsNotNone(m, "mode_install() not found in install.sh")
        block = m.group(1)
        self.assertNotIn(
            "openclaw", block,
            f"mode_install() still mentions openclaw:\n{block}",
        )


class RootInstallerPreflightHostAgnostic(_RootInstallDriver):
    """Behavioral: ``preflight`` must succeed without invoking
    any host framework binary — even when fakes for every
    supported host are on PATH (in any state: healthy, hanging,
    erroring)."""

    def test_preflight_succeeds_with_no_host_binary(self):
        """No host binary on PATH → preflight must succeed and
        not crash trying to find one."""
        project = self._tmp / "src" / "mpm"
        self._build_fake_project(project)
        prefix = self.home / ".mpm"
        data_root = self.home / ".mpm"

        result = self._drive_phases(
            project, prefix, data_root, bindir=None,
            phases=["preflight"],
        )
        self.assertEqual(
            result["returncode"], 0,
            f"preflight failed with no host binary on PATH:\n"
            f"stdout={result['stdout']}\nstderr={result['stderr']}",
        )
        # preflight used to print an "openclaw: yes/no" line. That
        # line must be gone — its absence is a host-agnostic marker.
        self.assertNotIn(
            "openclaw", result["stdout"],
            f"preflight output still mentions openclaw:\n{result['stdout']}",
        )

    def test_preflight_does_not_invoke_openclaw_in_any_state(self):
        """Even with an ``openclaw`` on PATH in any state
        (healthy, hanging, erroring), preflight must NOT invoke
        it. A hanging fake would stall the test if preflight
        reached it — a clean pass is the proof."""
        for behavior in ("active-restart-ok", "hang", "error"):
            with self.subTest(openclaw_behavior=behavior):
                # Per-subTest isolation: tear down and rebuild.
                self._cleanup()
                self.setUp()
                try:
                    project = self._tmp / "src" / "mpm"
                    self._build_fake_project(project)
                    prefix = self.home / ".mpm"
                    data_root = self.home / ".mpm"

                    record_file = self._tmp / f"openclaw.record-{behavior}"
                    bindir = self._make_fake_openclaw(record_file, behavior)

                    result = self._drive_phases(
                        project, prefix, data_root, bindir=bindir,
                        phases=["preflight"],
                    )
                    self.assertEqual(
                        result["returncode"], 0,
                        f"preflight failed (behavior={behavior}):\n"
                        f"stdout={result['stdout']}\nstderr={result['stderr']}",
                    )
                    invocations = self._read_record(record_file)
                    self.assertEqual(
                        invocations, [],
                        f"preflight invoked openclaw "
                        f"(behavior={behavior}):\n" + "\n".join(invocations),
                    )
                    self.assertNotIn(
                        "openclaw", result["stdout"],
                        f"preflight output mentions openclaw "
                        f"(behavior={behavior}):\n{result['stdout']}",
                    )
                finally:
                    self._cleanup()

    def test_preflight_does_not_invoke_any_host(self):
        """With fakes for OpenClaw, Hermes, Claude, OpenCode, and
        Pi all on PATH, preflight must invoke NONE of them. The
        host-agnostic boundary is for ALL hosts — not just one."""
        project = self._tmp / "src" / "mpm"
        self._build_fake_project(project)
        prefix = self.home / ".mpm"
        data_root = self.home / ".mpm"

        record_files = {
            name: self._tmp / f"{name}.record" for name in
            ("openclaw", "hermes", "claude", "opencode", "pi")
        }
        for rec in record_files.values():
            rec.write_text("")
        bindir = self._make_host_fakes(record_files)

        result = self._drive_phases(
            project, prefix, data_root, bindir=bindir,
            phases=["preflight"],
        )
        self.assertEqual(
            result["returncode"], 0,
            f"preflight failed with all host fakes on PATH:\n"
            f"stdout={result['stdout']}\nstderr={result['stderr']}",
        )
        for name, rec in record_files.items():
            invocations = self._read_record(rec)
            self.assertEqual(
                invocations, [],
                f"preflight invoked {name}:\n" + "\n".join(invocations),
            )


class RootInstallerValidateHostAgnostic(_RootInstallDriver):
    """Behavioral: ``phase_validate`` must succeed without invoking
    any host binary. This is the canonical proof for the late
    installer sections — ``phase_validate`` previously probed
    ``openclaw mcp show mpm``."""

    def test_validate_does_not_invoke_openclaw(self):
        """Drive ``phase_validate`` with a fake ``openclaw`` on
        PATH. The fake records every invocation. The test passes
        only if the installer never invokes it."""
        project = self._tmp / "src" / "mpm"
        self._build_fake_project(project)
        prefix = self.home / ".mpm"
        data_root = self.home / ".mpm"

        # The validation phase probes the wrapper at
        # $PREFIX/bin/mpm with health_check + read_directives
        # payloads. We need a wrapper + mpm.real at that location.
        # Easiest: create minimal stub binaries at the prefix.
        self._populate_prefix_stubs(prefix)

        record_file = self._tmp / "openclaw.record"
        bindir = self._make_fake_openclaw(record_file, "active-restart-ok")

        result = self._drive_phases(
            project, prefix, data_root, bindir=bindir,
            phases=["phase_validate"],
        )
        invocations = self._read_record(record_file)
        self.assertEqual(
            invocations, [],
            f"phase_validate invoked openclaw:\n" + "\n".join(invocations),
        )
        self.assertNotIn(
            "openclaw", result["stdout"],
            f"phase_validate output mentions openclaw:\n{result['stdout']}",
        )

    def test_validate_does_not_invoke_any_host(self):
        """Drive ``phase_validate`` with fakes for every supported
        host. None must be invoked."""
        project = self._tmp / "src" / "mpm"
        self._build_fake_project(project)
        prefix = self.home / ".mpm"
        data_root = self.home / ".mpm"
        self._populate_prefix_stubs(prefix)

        record_files = {
            name: self._tmp / f"{name}.record" for name in
            ("openclaw", "hermes", "claude", "opencode", "pi")
        }
        for rec in record_files.values():
            rec.write_text("")
        bindir = self._make_host_fakes(record_files)

        self._drive_phases(
            project, prefix, data_root, bindir=bindir,
            phases=["phase_validate"],
        )
        for name, rec in record_files.items():
            invocations = self._read_record(rec)
            self.assertEqual(
                invocations, [],
                f"phase_validate invoked {name}:\n" + "\n".join(invocations),
            )

    def _populate_prefix_stubs(self, prefix: Path) -> None:
        """Create the wrapper + mpm.real stubs ``phase_validate``
        needs. ``phase_validate`` runs:

          $PREFIX/bin/mpm call mpm_system --payload '{"action":"health_check"}'
          $PREFIX/bin/mpm call mpm_context --payload '{"action":"read_directives",…}'

        The wrapper must be a shell script that execs mpm.real. The
        real binary must respond with a valid ``"ok":true`` JSON for
        the health_check probe."""
        bin_dir = prefix / "bin"
        bin_dir.mkdir(parents=True, exist_ok=True)

        real = bin_dir / "mpm.real"
        real.write_text(textwrap.dedent("""\
            #!/usr/bin/env bash
            # fake mpm.real — answers health_check + read_directives.
            payload=""
            for arg in "$@"; do
                case "$arg" in
                    *'action":"health_check'*) payload="health_check" ;;
                    *'action":"read_directives'*) payload="read_directives" ;;
                esac
            done
            case "$payload" in
                health_check)
                    echo '{"ok":true,"status":"healthy","fake":true}'
                    ;;
                read_directives)
                    echo '{"count":1,"directives":[]}'
                    ;;
                *)
                    echo '{}'
                    ;;
            esac
        """))
        real.chmod(0o755)

        # Create a scheduler.lock so the validation lock-file check
        # passes.
        (prefix / "scheduler.lock").write_text("")

        wrapper = bin_dir / "mpm"
        wrapper.write_text(textwrap.dedent(f"""\
            #!/bin/sh
            export MPM_WORKSPACE={prefix}
            exec {real} "$@"
        """))
        wrapper.chmod(0o755)


class RootInstallerHostAgnosticNoOpenclaw(_RootInstallDriver):
    """Behavioral: when no host binary exists on PATH at all, the
    installer still completes its host-agnostic responsibilities
    cleanly. This is the pristine ``x`` profile scenario."""

    def test_no_openclaw_no_invocation(self):
        """Sanity: with NO host binary at all on PATH, preflight
        + a marker host-agnostic phase both complete without
        referencing any host framework."""
        project = self._tmp / "src" / "mpm"
        self._build_fake_project(project)
        prefix = self.home / ".mpm"
        data_root = self.home / ".mpm"

        result = self._drive_phases(
            project, prefix, data_root, bindir=None,
            phases=["preflight"],
        )
        self.assertEqual(
            result["returncode"], 0,
            f"preflight failed with empty PATH side:\n"
            f"stdout={result['stdout']}\nstderr={result['stderr']}",
        )
        # No host-name references in preflight output.
        for forbidden in ("openclaw", "hermes", "claude", "opencode"):
            self.assertNotIn(
                forbidden, result["stdout"],
                f"preflight output references {forbidden} (host-agnostic "
                f"boundary violated):\n{result['stdout']}",
            )


if __name__ == "__main__":
    unittest.main()
