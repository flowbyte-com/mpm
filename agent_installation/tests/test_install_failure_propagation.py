"""
test_install_failure_propagation.py — Required-step failure propagation
across the MPM agent-integration installers.

Scope: four installers where the audit surfaced a real (non-cosmetic)
masking pattern — a required step that failed silently and the script
still printed "Done." with exit 0.

  mpm-auto-mode-persona-openclaw/install.sh
      Steps 5 (mpmBin persistence), 6 (update repair), and 8 (plugins
      inspect) were all `warn`-only. The script claimed success on any
      combination of failures. Fix: INSTALL_FAILED classification
      (mirrors mpm-memory-openclaw), exit 1 with diagnostic block on
      failure.

  mpm-memory-openclaw/install.sh
      SOUL.md managed-block install failure downgraded to warn.
      Fix: SOUL_MD_OK classification; included in INSTALL_FAILED.

  mpm-opencode/install.sh, mpm-pi/install.sh
      AGENTS.md `--uninstall` failure downgraded to warn, leaving
      half-cleaned config. Fix: explicit exit 1 on uninstall failure.

Each scenario:
  - sandbox HOME + synthetic openclaw / pi config
  - stub host CLIs (`openclaw`, `python3` if needed for the renderer)
  - force ONE required step to fail
  - assert exit non-zero AND "Done." absent from stderr

Negative-control: every required step succeeds → exit 0 AND "Done."
present.

NO real installer is run against the user's live HOME — the brief's
hard constraint is honored (temp HOME, fakebin PATH, XDG override).
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
AGENT_INSTALLATION = REPO_ROOT / "agent_installation"

AUTO_MODE_INSTALL = AGENT_INSTALLATION / "mpm-auto-mode-persona-openclaw" / "install.sh"
MEMORY_OPENCLAW_INSTALL = AGENT_INSTALLATION / "mpm-memory-openclaw" / "install.sh"
OPENCODE_INSTALL = AGENT_INSTALLATION / "mpm-opencode" / "install.sh"
PI_INSTALL = AGENT_INSTALLATION / "mpm-pi" / "install.sh"

# The OpenClaw config shape the auto-mode installer reads at startup
# (to resolve `agents.entries.<id>.workspace`). Staging this lets the
# install get past preflight and reach the steps under test.
AUTO_MODE_PLUGIN_ID = "mpm-auto-mode-persona-openclaw"
MEMORY_PLUGIN_ID = "mpm-memory-openclaw"


# ---------------------------------------------------------------------------
# Sandbox harness
# ---------------------------------------------------------------------------


class _InstallerSandbox:
    """Hermetic harness: temp HOME + fake openclaw on PATH.

    The brief forbids any test that touches the real installer against
    the live HOME. Everything is staged under a tempdir; the only thing
    that touches the real filesystem is the install.sh script itself,
    which writes only into the tempdir.
    """

    def __init__(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-install-prop-"))
        self.fake_home = self.tmp / "home"
        self.fake_home.mkdir(parents=True, exist_ok=True)
        self.fakebin = self.tmp / "fakebin"
        self.fakebin.mkdir(parents=True, exist_ok=True)

    def cleanup(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def env(self, install_log: str | None = None) -> dict:
        env = os.environ.copy()
        env["HOME"] = str(self.fake_home)
        env["PATH"] = f"{self.fakebin}:{env.get('PATH', '')}"
        env.pop("MPM_WORKSPACE", None)
        if install_log is not None:
            env["MPM_INSTALL_LOG"] = install_log
        # Tight timeouts so a misbehaving stub doesn't hang the test.
        env["OPENCLAW_PLUGIN_INSPECT_TIMEOUT"] = "5"
        env["OPENCLAW_CONFIG_TIMEOUT"] = "5"
        env["OPENCLAW_UPDATE_REPAIR_TIMEOUT"] = "5"
        env["OPENCLAW_GATEWAY_STATUS_TIMEOUT"] = "5"
        env["OPENCLAW_GATEWAY_RESTART_TIMEOUT"] = "5"
        return env


# ---------------------------------------------------------------------------
# Fake-CLI builders
# ---------------------------------------------------------------------------

def _write_fake_openclaw(
    bin_dir: Path,
    *,
    config_set_succeed: bool = True,
    update_repair_succeed: bool = True,
    plugins_inspect_succeed: bool = True,
    gateway_status_succeed: bool = True,
    gateway_restart_succeed: bool = True,
    plugins_install_succeed: bool = True,
) -> Path:
    """Fake `openclaw` for the auto-mode + memory installers.

    Each toggle isolates one required step. Defaults reflect the
    happy-path (every CLI step returns 0).

    The `plugins inspect` step reports `rootDir` from $ADAPTER_DIR
    (set by the test harness via env). The installer's preflight
    compares that to its own resolved path; if both resolve to the
    same dir, the install is "already linked from here" and the
    install-step branch is skipped. That is the only way to drive
    the post-install steps (mpmBin config, update repair, plugins
    inspect verify) under test.
    """
    p = bin_dir / "openclaw"
    p.write_text(
        textwrap.dedent(f"""\
            #!/usr/bin/env bash
            # fake-openclaw for installer-failure-propagation tests.
            ADAPTER="${{ADAPTER_DIR:-/nonexistent}}"
            case "$1" in
                plugins)
                    case "$2" in
                        install)
                            exit {0 if plugins_install_succeed else 1}
                            ;;
                        inspect)
                            echo "{{\\"ok\\":true,\\"plugin\\":{{\\"id\\":\\"$3\\",\\"rootDir\\":\\"$ADAPTER\\"}}}}"
                            exit {0 if plugins_inspect_succeed else 1}
                            ;;
                        list)
                            echo '[]'
                            exit 0
                            ;;
                    esac
                    ;;
                config)
                    case "$2" in
                        set)   exit {0 if config_set_succeed else 1} ;;
                        get)   echo 'mpm-memory-openclaw'; exit 0 ;;
                    esac
                    ;;
                update)
                    case "$2" in
                        repair) exit {0 if update_repair_succeed else 1} ;;
                    esac
                    ;;
                gateway)
                    case "$2" in
                        status) exit {0 if gateway_status_succeed else 1} ;;
                        restart) exit {0 if gateway_restart_succeed else 1} ;;
                    esac
                    ;;
                doctor) exit 0 ;;
            esac
            exit 0
        """)
    )
    p.chmod(0o755)
    return p


def _stage_openclaw_state(home: Path, plugin_id: str) -> None:
    """Pre-stage a minimal ~/.openclaw/openclaw.json so the installer
    can resolve the agent workspace and reach the steps under test.

    Mirrors what mpm-memory-openclaw / mpm-auto-mode-persona-openclaw
    installers write on a real run, so we don't have to drive the full
    plugin-install path through the fake.
    """
    config_dir = home / ".openclaw"
    workspace = config_dir / "workspace"
    workspace.mkdir(parents=True, exist_ok=True)
    config = config_dir / "openclaw.json"
    config.write_text(json.dumps({
        "agents": {
            "defaults": {"workspace": str(workspace)},
            "entries": {"main": {"workspace": str(workspace)}},
        },
        "plugins": {
            "entries": {
                plugin_id: {
                    "enabled": True,
                    "config": {"mpmBin": "/usr/bin/false"},
                },
            },
        },
    }), encoding="utf-8")
    # SOUL.md in the agent workspace so the install_openclaw_instructions.py
    # renderer has a file to operate on.
    (workspace / "SOUL.md").write_text(
        "# SOUL.md\n", encoding="utf-8",
    )


def _stage_mpm_payload(home: Path) -> None:
    """Stage a minimal ~/.mpm install so locate_mpm() in the openclaw
    installers resolves."""
    (home / ".mpm" / "bin").mkdir(parents=True, exist_ok=True)
    (home / ".mpm" / "bin" / "mpm").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
    (home / ".mpm" / "bin" / "mpm").chmod(0o755)


def _run_installer(
    install_script: Path,
    sandbox: _InstallerSandbox,
    *,
    args: list[str] | None = None,
    adapter_dir: Path | None = None,
) -> dict:
    args = args or []
    install_log = sandbox.tmp / "install.log"
    env = sandbox.env(install_log=str(install_log))
    # The fake-openclaw's `plugins inspect` reports this path so the
    # installer's preflight sees the plugin as already linked from
    # this adapter's own source dir (the "linked-from-here" branch).
    # That skips the destructive install step and lets us drive the
    # post-install CLI calls under test.
    env["ADAPTER_DIR"] = str(adapter_dir or install_script.parent)
    result = subprocess.run(
        ["bash", "--noprofile", "--norc", str(install_script), *args],
        capture_output=True,
        text=True,
        env=env,
        timeout=60,
    )
    return {
        "stdout": result.stdout,
        "stderr": result.stderr,
        "returncode": result.returncode,
    }


# ---------------------------------------------------------------------------
# mpm-auto-mode-persona-openclaw
# ---------------------------------------------------------------------------


class _AutoModeDriver(unittest.TestCase):
    def setUp(self) -> None:
        self.sb = _InstallerSandbox()
        self.addCleanup(self.sb.cleanup)
        _stage_mpm_payload(self.sb.fake_home)
        _stage_openclaw_state(self.sb.fake_home, AUTO_MODE_PLUGIN_ID)

    def _run(self, **fake_kwargs) -> dict:
        _write_fake_openclaw(self.sb.fakebin, **fake_kwargs)
        return _run_installer(AUTO_MODE_INSTALL, self.sb)


class AutoModeHappyPath(_AutoModeDriver):
    def test_clean_install_exits_zero_with_done(self) -> None:
        r = self._run()
        self.assertEqual(r["returncode"], 0, f"stderr:\n{r['stderr']}")
        self.assertIn("Done.", r["stderr"])


class AutoModeRequiredStepFails(_AutoModeDriver):
    """The headline regression: a required step fails. Pre-fix the
    script printed "Done." and exited 0. Post-fix it must exit 1 and
    print "FAILED" with a precise diagnostic."""

    def test_mpmBin_config_failure_propagates(self) -> None:
        r = self._run(config_set_succeed=False)
        self.assertNotEqual(r["returncode"], 0)
        self.assertNotIn("[mpm-auto-mode-persona-openclaw install] Done.", r["stderr"])
        self.assertIn("FAILED", r["stderr"])

    def test_update_repair_failure_propagates(self) -> None:
        r = self._run(update_repair_succeed=False)
        self.assertNotEqual(r["returncode"], 0)
        self.assertNotIn("[mpm-auto-mode-persona-openclaw install] Done.", r["stderr"])
        self.assertIn("FAILED", r["stderr"])

    def test_plugins_inspect_failure_propagates(self) -> None:
        r = self._run(plugins_inspect_succeed=False)
        self.assertNotEqual(r["returncode"], 0)
        self.assertNotIn("[mpm-auto-mode-persona-openclaw install] Done.", r["stderr"])
        self.assertIn("FAILED", r["stderr"])


# ---------------------------------------------------------------------------
# mpm-memory-openclaw: SOUL.md install failure must propagate
# ---------------------------------------------------------------------------


def _stage_memory_openclaw_state(home: Path) -> None:
    """Memory-openclaw: pre-stage the workspace + a SOUL.md the
    install_openclaw_instructions.py renderer can read."""
    config_dir = home / ".openclaw"
    workspace = config_dir / "workspace"
    workspace.mkdir(parents=True, exist_ok=True)
    (workspace / "SOUL.md").write_text("# SOUL.md\n", encoding="utf-8")
    config = config_dir / "openclaw.json"
    config.write_text(json.dumps({
        "agents": {
            "defaults": {"workspace": str(workspace)},
            "entries": {"main": {"workspace": str(workspace)}},
        },
        "plugins": {
            "entries": {
                MEMORY_PLUGIN_ID: {
                    "enabled": True,
                    "config": {"mpmBin": "/usr/bin/false"},
                },
            },
        },
    }), encoding="utf-8")


def _fake_python_for_soUL_fail(bin_dir: Path) -> Path:
    """Drop a fake `python3` that fails ONLY for the
    install_openclaw_instructions.py invocation (so the SOUL.md
    managed-block step returns non-zero).

    Detection: argv contains `install_openclaw_instructions.py`.
    """
    p = bin_dir / "python3"
    p.write_text(textwrap.dedent("""\
        #!/usr/bin/env bash
        # fake-python3 for SOUL.md install-failure test
        for arg in "$@"; do
            case "$arg" in
                *install_openclaw_instructions.py*)
                    echo "fake: simulated install failure" >&2
                    exit 1
                    ;;
            esac
        done
        # Pass-through for any other python invocation the installer
        # makes (opencode.jsonc / settings.json migrators, etc).
        exec /usr/bin/env -i PATH="$PATH" /usr/bin/python3 "$@"
    """))
    p.chmod(0o755)
    return p


class _MemoryOpenclawDriver(unittest.TestCase):
    def setUp(self) -> None:
        self.sb = _InstallerSandbox()
        self.addCleanup(self.sb.cleanup)
        _stage_mpm_payload(self.sb.fake_home)
        _stage_memory_openclaw_state(self.sb.fake_home)

    def _run(self, **fake_kwargs) -> dict:
        _write_fake_openclaw(self.sb.fakebin, **fake_kwargs)
        # The renderer failure is injected via a python3 wrapper so
        # we don't disturb the host python.
        _fake_python_for_soUL_fail(self.sb.fakebin)
        return _run_installer(MEMORY_OPENCLAW_INSTALL, self.sb)


class MemoryOpenclawSoulMdInstallFails(_MemoryOpenclawDriver):
    def test_soul_md_install_failure_propagates(self) -> None:
        r = self._run()
        self.assertNotEqual(
            r["returncode"], 0,
            f"SOUL.md install failure must exit non-zero; "
            f"pre-fix the script printed Done. and exited 0.\n"
            f"stderr:\n{r['stderr']}",
        )
        self.assertNotIn(
            "[mpm-memory-openclaw install] Done.", r["stderr"],
            "SOUL.md install failure must NOT show the success Done. banner",
        )
        self.assertIn("FAILED", r["stderr"])


# ---------------------------------------------------------------------------
# mpm-opencode + mpm-pi: AGENTS.md --uninstall failure must propagate
# ---------------------------------------------------------------------------


def _stage_opencode_user_state(home: Path) -> None:
    """Stage ~/.config/opencode/opencode.jsonc with the canonical
    entry already present, so the opencode installer's migration
    step is a no-op and we can drive --uninstall directly."""
    config_dir = home / ".config" / "opencode"
    config_dir.mkdir(parents=True, exist_ok=True)
    config = config_dir / "opencode.jsonc"
    config.write_text(json.dumps({
        "$schema": "https://opencode.ai/config.json",
        "plugin": [f"file://{home}/.mpm/agent_installation/mpm-opencode/dist/index.js"],
    }), encoding="utf-8")


def _stage_pi_user_state(home: Path) -> None:
    """Stage ~/.pi/agent/settings.json with the canonical extension
    entry so the install --uninstall path has a file to mutate."""
    settings_dir = home / ".pi" / "agent"
    settings_dir.mkdir(parents=True, exist_ok=True)
    settings = settings_dir / "settings.json"
    settings.write_text(json.dumps({
        "extensions": [f"{home}/.mpm/agent_installation/mpm-pi"],
    }), encoding="utf-8")


def _fake_python_uninstall_fail(bin_dir: Path) -> Path:
    """Fake python3 that fails ONLY for install_agents_instructions.py
    --uninstall invocations."""
    p = bin_dir / "python3"
    p.write_text(textwrap.dedent("""\
        #!/usr/bin/env bash
        # fake-python3 for AGENTS.md --uninstall-failure test
        saw_uninstall=0
        saw_installer=0
        for arg in "$@"; do
            case "$arg" in
                --uninstall) saw_uninstall=1 ;;
                *install_agents_instructions.py*) saw_installer=1 ;;
            esac
        done
        if [ "$saw_uninstall" = "1" ] && [ "$saw_installer" = "1" ]; then
            echo "fake: simulated uninstall failure" >&2
            exit 1
        fi
        exec /usr/bin/env -i PATH="$PATH" /usr/bin/python3 "$@"
    """))
    p.chmod(0o755)
    return p


class _OpencodeUninstallDriver(unittest.TestCase):
    def setUp(self) -> None:
        self.sb = _InstallerSandbox()
        self.addCleanup(self.sb.cleanup)
        _stage_opencode_user_state(self.sb.fake_home)
        _fake_python_uninstall_fail(self.sb.fakebin)

    def _run(self) -> dict:
        return _run_installer(OPENCODE_INSTALL, self.sb, args=["--uninstall"])


class OpencodeUninstallFailurePropagates(_OpencodeUninstallDriver):
    def test_uninstall_failure_exits_nonzero(self) -> None:
        r = self._run()
        self.assertNotEqual(
            r["returncode"], 0,
            f"AGENTS.md uninstall failure must exit non-zero; "
            f"pre-fix the script exited 0 and left a managed block "
            f"in place. stderr:\n{r['stderr']}",
        )


class _PiUninstallDriver(unittest.TestCase):
    def setUp(self) -> None:
        self.sb = _InstallerSandbox()
        self.addCleanup(self.sb.cleanup)
        _stage_pi_user_state(self.sb.fake_home)
        _fake_python_uninstall_fail(self.sb.fakebin)

    def _run(self) -> dict:
        return _run_installer(PI_INSTALL, self.sb, args=["--uninstall"])


class PiUninstallFailurePropagates(_PiUninstallDriver):
    def test_uninstall_failure_exits_nonzero(self) -> None:
        r = self._run()
        self.assertNotEqual(
            r["returncode"], 0,
            f"AGENTS.md uninstall failure must exit non-zero; "
            f"pre-fix the script exited 0 and left a managed block "
            f"in place. stderr:\n{r['stderr']}",
        )


# ---------------------------------------------------------------------------
# Negative-control proof: removing the new exit 1 brings back the bug
# ---------------------------------------------------------------------------
#
# We stage a copy of the auto-mode installer with the classification
# block stripped, run it under the same fake-CLI failure injection,
# and assert that the regression test would have caught the original
# defect (i.e., the unmodified copy exits 0 and prints Done.).
#
# This is the non-vacuity proof the brief asks for. We do NOT mutate
# the repo installer — only a tempdir copy.


class NegativeControlAutoMode(unittest.TestCase):
    def setUp(self) -> None:
        self.sb = _InstallerSandbox()
        self.addCleanup(self.sb.cleanup)
        _stage_mpm_payload(self.sb.fake_home)
        _stage_openclaw_state(self.sb.fake_home, AUTO_MODE_PLUGIN_ID)

    def _stage_unfixed_copy(self) -> Path:
        """Copy the auto-mode installer and strip the new classification
        block so it behaves like the pre-fix version: every CLI step
        warns on failure; "Done." always prints.

        Stages the staged script next to a copy of the adapter's
        openclaw.plugin.json so the installer's PLUGIN_ID parse
        (`grep openclaw.plugin.json`) finds its source manifest.
        """
        src = AUTO_MODE_INSTALL.read_text(encoding="utf-8")
        # Drop the entire Required-classification block + the FAILED
        # branch it gates. The regression test only needs the pre-fix
        # behavior on Step 5/6/8 failure to be exit 0 + Done.
        split_marker = "# --------------------------------------------------------------------------\n# Required-classification"
        stripped = src.split(split_marker)[0]
        trailing_fallback = textwrap.dedent("""\

            cat >&2 <<NEXT
            [mpm-auto-mode-persona-openclaw install] Done.
            NEXT
        """)
        staged_dir = self.sb.tmp / "adapter_unfixed"
        staged_dir.mkdir(parents=True, exist_ok=True)
        staged = staged_dir / "install.sh"
        staged.write_text(stripped + trailing_fallback, encoding="utf-8")
        staged.chmod(0o755)
        # Sidecar the openclaw.plugin.json so the installer's PLUGIN_ID
        # parse at line 64 sees the canonical id.
        src_manifest = AUTO_MODE_INSTALL.parent / "openclaw.plugin.json"
        if src_manifest.exists():
            (staged_dir / "openclaw.plugin.json").write_text(
                src_manifest.read_text(encoding="utf-8"),
                encoding="utf-8",
            )
        return staged

    def test_pre_fix_script_returns_zero_and_claims_done(self) -> None:
        """Proof the regression test is not vacuous: with the fix
        removed, the same fake-CLI failure injection produces the
        bug behavior (exit 0 + Done.)."""
        _write_fake_openclaw(self.sb.fakebin, update_repair_succeed=False)
        unfixed = self._stage_unfixed_copy()
        r = _run_installer(unfixed, self.sb)
        self.assertEqual(
            r["returncode"], 0,
            f"unfixed-script + failing update_repair must exit 0 "
            f"(this is the bug); the regression test must assert "
            f"against this exact pre-fix behavior. stderr:\n{r['stderr']}",
        )
        self.assertIn("Done.", r["stderr"])


if __name__ == "__main__":
    unittest.main()