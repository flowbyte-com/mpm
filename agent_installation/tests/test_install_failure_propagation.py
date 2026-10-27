"""
test_install_failure_propagation.py — Required-step failure propagation
across the MPM agent-integration installers.

Scope: the installers where the audit surfaced a real (non-cosmetic)
masking pattern — a required step that failed silently and the script
still printed "Done." with exit 0.

  mpm-opencode/install.sh
      AGENTS.md `--uninstall` failure was downgraded to warn, leaving
      half-cleaned config. Fix: explicit exit 1 on uninstall failure.

Each scenario:
  - sandbox HOME + synthetic opencode config
  - stub host CLIs (a fake `python3` that fails the uninstall call)
  - force ONE required step to fail
  - assert exit non-zero AND "Done." absent from stderr

The OpenClaw (auto-mode + memory) and Pi installers carried the same
class of coverage; they were retired with their adapters on 2026-10-27
(Session A's adjudication on PR #126), so their scenarios left with
them. The OpenCode scenario below is the survivor and still pins the
property.

NO real installer is run against the user's live HOME — the brief's
hard constraint is honored (temp HOME, fakebin PATH, XDG override).
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
AGENT_INSTALLATION = REPO_ROOT / "agent_installation"

OPENCODE_INSTALL = AGENT_INSTALLATION / "mpm-opencode" / "install.sh"


# ---------------------------------------------------------------------------
# Sandbox harness
# ---------------------------------------------------------------------------


class _InstallerSandbox:
    """Hermetic harness: temp HOME + a fakebin on PATH.

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
    if adapter_dir is None:
        adapter_dir = install_script.parent
    env["ADAPTER_DIR"] = str(adapter_dir)
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
# mpm-opencode
# ---------------------------------------------------------------------------


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


if __name__ == "__main__":
    unittest.main()
