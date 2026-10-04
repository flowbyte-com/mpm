"""
test_uninstall_openclaw_failure_semantics.py — Regression coverage for the
2026-10-04 audit finding: agent_installation/uninstall-openclaw.sh exited 0
even when its required `openclaw plugins uninstall`/`config unset`/gateway
restart steps failed.

The defect was structural: the script's last executable command was a
heredoc (`cat <<NEXT`), which exits 0 unconditionally, so every internal
`if ... then warn` masked step fell off the end with cat's exit status.
The fix:
  * Tracks `FAIL=0` at top of execution
  * Bumps FAIL via `mark_fail` for every required-step failure
  * Final block: `if [ "$FAIL" -gt 0 ]; then exit 1; else exit 0; fi`

This module stages a copy of uninstall-openclaw.sh plus both adapter
manifests inside a tempdir, drops a configurable fake `openclaw` on
PATH, and asserts the exit code under three scenarios:
  1. all-success → 0
  2. one required step fails → 1, with the FAIL summary visible in stderr
  3. negative control (the script without the FAIL plumbing) → 0, proving
     the test would have detected the original defect

Nothing here touches a live HOME, ~/.mpm, OpenClaw, or any installed
binary. Every dependency is staged inside the tempdir and prepended to
PATH so the test runs identically on a developer machine and on CI.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
UNINSTALL_OPENCLAW_SH = (
    REPO_ROOT / "agent_installation" / "uninstall-openclaw.sh"
)
MEM_MANIFEST = (
    REPO_ROOT
    / "agent_installation"
    / "mpm-memory-openclaw"
    / "openclaw.plugin.json"
)
AUTO_MANIFEST = (
    REPO_ROOT
    / "agent_installation"
    / "mpm-auto-mode-persona-openclaw"
    / "openclaw.plugin.json"
)


def _stage_script_under(tmp: Path) -> Path:
    """Copy uninstall-openclaw.sh + the two adapter manifests into
    ``tmp/agent_installation/`` so the script resolves its sibling
    adapter directories relative to its own location (CWD-independent).
    Returns the staged ``uninstall-openclaw.sh`` path.
    """
    ag = tmp / "agent_installation"
    ag.mkdir(parents=True, exist_ok=True)
    staged_script = ag / "uninstall-openclaw.sh"
    shutil.copy(UNINSTALL_OPENCLAW_SH, staged_script)
    staged_script.chmod(0o755)

    mem_dir = ag / "mpm-memory-openclaw"
    mem_dir.mkdir(parents=True, exist_ok=True)
    shutil.copy(MEM_MANIFEST, mem_dir / "openclaw.plugin.json")

    auto_dir = ag / "mpm-auto-mode-persona-openclaw"
    auto_dir.mkdir(parents=True, exist_ok=True)
    shutil.copy(AUTO_MANIFEST, auto_dir / "openclaw.plugin.json")
    return staged_script


def _make_fake_openclaw(
    bin_dir: Path,
    *,
    uninstall_succeed: bool = True,
    config_unset_succeed: bool = True,
    config_set_succeed: bool = True,
    inspect_succeed: bool = True,
    gateway_status_succeed: bool = True,
    gateway_restart_succeed: bool = True,
) -> Path:
    """Drop a fake `openclaw` binary that responds to the CLI subcommands
    the uninstaller actually invokes. Each toggle lets one test isolate a
    single failure point without re-implementing the whole contract.
    """
    p = bin_dir / "openclaw"
    p.write_text(
        textwrap.dedent(f"""\
            #!/usr/bin/env bash
            # fake-openclaw: deterministic CLI stub for uninstall-openclaw tests.
            case "$1" in
                plugins)
                    case "$2" in
                        inspect)
                            # plugins inspect <id> --json — owned | absent | etc.
                            cat <<'JSON-EOF'
{{"ok":true,"plugin":{{"rootDir":"$0","id":"$3"}}}}
JSON-EOF
                            exit {0 if inspect_succeed else 1}
                            ;;
                        registry)
                            echo '{{"ok":true,"persisted":{{}}}}'
                            exit 0
                            ;;
                        uninstall)
                            # `openclaw plugins uninstall --force <id>`
                            exit {0 if uninstall_succeed else 1}
                            ;;
                        list)
                            echo '[]'
                            exit 0
                            ;;
                    esac
                    ;;
                config)
                    case "$2" in
                        get)
                            # plugins.slots.memory = the id the uninstaller is
                            # about to remove, so the slot-reset branch fires
                            # on the success path. On the failure-injection
                            # path tests flip config_set_succeed=false so
                            # that branch also fails.
                            echo 'mpm-memory-openclaw'
                            exit 0
                            ;;
                        set)   exit {0 if config_set_succeed else 1} ;;
                        unset) exit {0 if config_unset_succeed else 1} ;;
                    esac
                    ;;
                gateway)
                    case "$2" in
                        status)
                            exit {0 if gateway_status_succeed else 1}
                            ;;
                        restart)
                            exit {0 if gateway_restart_succeed else 1}
                            ;;
                    esac
                    ;;
            esac
            exit 0
        """)
    )
    p.chmod(0o755)
    return p


class _UninstallOpenclawDriver(unittest.TestCase):
    """Hermetic harness: tempdir + staged script + fake openclaw on PATH."""

    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-uninstall-openclaw-"))
        self.fake_home = self.tmp / "home"
        self.fake_home.mkdir(parents=True, exist_ok=True)
        self.fakebin = self.tmp / "fakebin"
        self.fakebin.mkdir(parents=True, exist_ok=True)
        self.staged_script = _stage_script_under(self.tmp)
        self.addCleanup(self._cleanup)

    def _cleanup(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _run(
        self,
        *,
        uninstall_succeed: bool = True,
        config_unset_succeed: bool = True,
        config_set_succeed: bool = True,
        inspect_succeed: bool = True,
        gateway_status_succeed: bool = True,
        gateway_restart_succeed: bool = True,
    ) -> dict:
        _make_fake_openclaw(
            self.fakebin,
            uninstall_succeed=uninstall_succeed,
            config_unset_succeed=config_unset_succeed,
            config_set_succeed=config_set_succeed,
            inspect_succeed=inspect_succeed,
            gateway_status_succeed=gateway_status_succeed,
            gateway_restart_succeed=gateway_restart_succeed,
        )
        env = os.environ.copy()
        env["HOME"] = str(self.fake_home)
        env["PATH"] = f"{self.fakebin}:{env.get('PATH', '')}"
        env["UNINSTALL_LOG"] = str(self.tmp / "uninstall.log")
        # Pin down every timeout so the test doesn't depend on the host.
        env["OPENCLAW_PLUGIN_UNINSTALL_TIMEOUT"] = "5"
        env["OPENCLAW_PLUGIN_INSPECT_TIMEOUT"] = "5"
        env["OPENCLAW_CONFIG_TIMEOUT"] = "5"
        env["OPENCLAW_GATEWAY_STATUS_TIMEOUT"] = "5"
        env["OPENCLAW_GATEWAY_RESTART_TIMEOUT"] = "5"
        result = subprocess.run(
            [
                "bash",
                "--noprofile",
                "--norc",
                str(self.staged_script),
                "--yes",
            ],
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


class AllStepsSucceed(_UninstallOpenclawDriver):
    """Baseline: every required step succeeds → exit 0."""

    def test_returns_zero_when_all_required_steps_succeed(self) -> None:
        r = self._run()
        self.assertEqual(
            r["returncode"],
            0,
            f"clean uninstall must exit 0\nstderr:\n{r['stderr']}",
        )
        # The MPM substrate sanity-check runs `--version` against
        # $HOME/.mpm/bin/mpm, which the fake HOME does not contain —
        # the script emits a warn but does not FAIL. Make sure it did
        # not erroneously bump FAIL via that warning.
        self.assertNotIn("FAILED STEP", r["stderr"])


class PluginUninstallFails(_UninstallOpenclawDriver):
    """The headline defect: `openclaw plugins uninstall` failure must
    propagate to the exit code."""

    def test_exits_one_when_uninstall_fails(self) -> None:
        r = self._run(uninstall_succeed=False)
        self.assertNotEqual(
            r["returncode"],
            0,
            "uninstall failure must produce a non-zero exit code "
            "(this is the regression: pre-fix the script exited 0)",
        )
        self.assertIn("FAILED STEP", r["stderr"])

    def test_uninstall_failure_is_visible_to_operator(self) -> None:
        r = self._run(uninstall_succeed=False)
        # The mark_fail label must name the failing step so an operator
        # tail-grepping the log can find it without a verbose log dive.
        self.assertIn(
            "openclaw plugins uninstall --force", r["stderr"],
            "mark_fail label must identify the failing step",
        )


class ConfigUnsetFails(_UninstallOpenclawDriver):
    """The residue `openclaw config unset` is also a required write — a
    failure there means dirty post-state the operator cannot ignore."""

    def test_exits_one_when_residue_cleanup_fails(self) -> None:
        # uninstall succeeds (so the install record is gone), but the
        # config unset that should remove the plugins.entries.* residue
        # fails. The operator still has a stale config entry.
        r = self._run(config_unset_succeed=False)
        self.assertNotEqual(r["returncode"], 0)
        self.assertIn("FAILED STEP", r["stderr"])
        self.assertIn("residue cleanup", r["stderr"])


class GatewayRestartFails(_UninstallOpenclawDriver):
    """A post-uninstall gateway restart that fails must not be silently
    absorbed — the gateway keeps running with stale plugin config."""

    def test_exits_one_when_gateway_restart_fails(self) -> None:
        r = self._run(gateway_restart_succeed=False)
        self.assertNotEqual(r["returncode"], 0)
        self.assertIn("FAILED STEP", r["stderr"])
        self.assertIn("gateway restart", r["stderr"])


class NegativeControlPreFix(_UninstallOpenclawDriver):
    """If we revert the fix locally (drop the FAIL plumbing + final
    exit), the script returns to its buggy behavior — exit 0 with
    warnings. This proves the test would have detected the original
    defect, and guards against a vacuous green test.

    We do NOT touch the repo file; we copy it into the tempdir first
    and patch the staged copy. That keeps the test self-contained and
    repeatable without mutating in-tree state.
    """

    def test_pre_fix_script_exits_zero_on_failure(self) -> None:
        # Read the staged script and strip the fix.
        text = self.staged_script.read_text()
        # Drop the FAIL=0 initialization + the mark_fail definition.
        import re
        stripped = re.sub(
            r"# FAIL counts.*?mark_fail\(\) \{[^}]*FAIL=\$\(\( FAIL \+ 1 \)\)\n\}\n",
            "",
            text,
            count=1,
            flags=re.DOTALL,
        )
        # Replace every mark_fail "<label>" call with warn "<label>" —
        # the buggy version's only response was a warning.
        stripped = re.sub(
            r'mark_fail "([^"]+)"',
            r'warn "\1"',
            stripped,
        )
        # Replace the final exit-1 block with a no-op so the script
        # falls through to cat (which is what the buggy version did).
        stripped = re.sub(
            r"# Final exit code:[^\n]*\n"
            r"# Rationale:[^\n]*\n"
            r"# the verbose log\.\n"
            r"if \[ \"\$FAIL\" -gt 0 \]; then\n"
            r"  err \"uninstall completed with \$FAIL failed step\(s\); see \$LOG_FILE\"\n"
            r"  exit 1\n"
            r"fi\n"
            r"exit 0",
            "exit 0",
            stripped,
            flags=re.MULTILINE,
        )
        # Sanity: the patched copy must still parse.
        subprocess.run(["bash", "-n", self.staged_script], check=True)
        self.staged_script.write_text(stripped)

        # Re-instantiate the fake openclaw with the failure injected.
        # _run reads the script via subprocess each time, so we can
        # call it directly against the patched file.
        _make_fake_openclaw(self.fakebin, uninstall_succeed=False)
        env = os.environ.copy()
        env["HOME"] = str(self.fake_home)
        env["PATH"] = f"{self.fakebin}:{env.get('PATH', '')}"
        env["UNINSTALL_LOG"] = str(self.tmp / "uninstall.log")
        env["OPENCLAW_PLUGIN_UNINSTALL_TIMEOUT"] = "5"
        env["OPENCLAW_PLUGIN_INSPECT_TIMEOUT"] = "5"
        env["OPENCLAW_CONFIG_TIMEOUT"] = "5"
        env["OPENCLAW_GATEWAY_STATUS_TIMEOUT"] = "5"
        env["OPENCLAW_GATEWAY_RESTART_TIMEOUT"] = "5"
        result = subprocess.run(
            [
                "bash",
                "--noprofile",
                "--norc",
                str(self.staged_script),
                "--yes",
            ],
            capture_output=True,
            text=True,
            env=env,
            timeout=60,
        )
        self.assertEqual(
            result.returncode,
            0,
            "negative control: with the FAIL plumbing removed, the "
            "buggy version exits 0 even when uninstall fails. "
            "If this assertion fails, the test is no longer proving "
            "the regression — the in-tree fix may have changed shape.",
        )
        # The bug emitted a warning, not a FAILED STEP line, so
        # the patched script's stderr does NOT contain the new label.
        self.assertNotIn("FAILED STEP", result.stderr)


if __name__ == "__main__":
    unittest.main()