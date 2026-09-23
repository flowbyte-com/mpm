#!/usr/bin/env python3
"""Regression tests for the Pi integration installer.

Covers the 2026-09-22 Pi settings-path drift defect: the live
~/.pi/agent/settings.json on this machine referenced the legacy
agent_plugins/pi-mpm path even after the source-tree rename to
agent_installation/mpm-pi in commit ee91d167 (2026-09-17). The fix is
a new install.sh that detects the legacy paths and migrates them to
the canonical ~/.mpm/agent_installation/mpm-pi entry.

Run: python3 -m unittest tests/test_pi_settings_installer.py -v
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]
INSTALL_SCRIPT = REPO / "agent_installation" / "mpm-pi" / "install.sh"


def _setup_temp_workspace(tmpdir: str) -> dict:
    """Build a hermetic test environment with $HOME and $MPM_WORKSPACE
    pointing at temp directories. Returns the paths dictionary."""
    fake_home = Path(tmpdir) / "home"
    fake_mpm = Path(tmpdir) / "mpm"
    fake_pi = fake_home / ".pi" / "agent"
    fake_pi.mkdir(parents=True, exist_ok=True)
    # Mirror the canonical Pi adapter layout under the fake MPM root.
    adapter_dir = fake_mpm / "agent_installation" / "mpm-pi"
    adapter_dir.mkdir(parents=True, exist_ok=True)
    (adapter_dir / "index.ts").write_text(
        "// fake mpm-pi adapter for installer test\n"
        "export default function piMpmExtension(pi) {}\n",
        encoding="utf-8",
    )
    return {
        "home": fake_home,
        "mpm": fake_mpm,
        "pi_settings": fake_pi / "settings.json",
        "adapter_dir": adapter_dir,
    }


def _run_installer(env: dict, *args: str) -> subprocess.CompletedProcess:
    """Run install.sh under the given env (must include HOME + MPM_WORKSPACE)."""
    cmd = [str(INSTALL_SCRIPT), *args]
    return subprocess.run(
        cmd,
        env=env,
        capture_output=True,
        text=True,
        check=False,
        timeout=30,
    )


class TestPiSettingsInstaller(unittest.TestCase):
    """The Pi installer's settings.json refresh behavior."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="mpm-pi-install-test-")
        self.paths = _setup_temp_workspace(self.tmp)
        self.env = {
            **os.environ,
            "HOME": str(self.paths["home"]),
            "MPM_WORKSPACE": str(self.paths["mpm"]),
            "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        }

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    # -- Defect regression ---------------------------------------------------

    def test_migrates_legacy_agent_plugins_pi_mpm_entry(self):
        """Reproduce the original drift: settings.json references
        ~/.mpm/agent_plugins/pi-mpm (the pre-2026-09-17 path). The
        installer must replace that entry with the canonical
        ~/.mpm/agent_installation/mpm-pi path."""
        # Seed a pre-migration settings.json that preserves the user's
        # unrelated keys — exactly the state the live system was in
        # before this fix.
        #
        # The legacy extension path is built from the hermetic temp
        # MPM root (self.paths["mpm"]) rather than the original
        # author's checkout path "/home/v/.mpm/agent_plugins/pi-mpm".
        # The migration logic only needs ANY non-canonical extension
        # path to exercise the legacy -> canonical rewrite; using a
        # hermetic path keeps the test independent of the host.
        legacy_path = str(self.paths["mpm"] / "agent_plugins" / "pi-mpm")
        self.paths["pi_settings"].write_text(json.dumps({
            "lastChangelogVersion": "0.85.1",
            "defaultThinkingLevel": "high",
            "defaultProvider": "minimax",
            "defaultModel": "MiniMax-M3",
            "theme": "dark",
            "extensions": [
                legacy_path,
            ],
            "packages": ["npm:pi-mcp-adapter"],
        }, indent=2) + "\n", encoding="utf-8")

        proc = _run_installer(self.env)
        self.assertEqual(proc.returncode, 0,
                         f"installer failed: stderr={proc.stderr}")

        data = json.loads(self.paths["pi_settings"].read_text())
        canonical = str(self.paths["adapter_dir"])

        # Legacy entry gone, canonical entry present, exactly once.
        self.assertNotIn(legacy_path,
                         str(data["extensions"]))
        self.assertIn(canonical, data["extensions"])
        self.assertEqual(data["extensions"].count(canonical), 1,
                         "duplicate canonical entries indicate a bug")

        # Unrelated keys preserved verbatim.
        self.assertEqual(data["lastChangelogVersion"], "0.85.1")
        self.assertEqual(data["defaultProvider"], "minimax")
        self.assertEqual(data["theme"], "dark")
        self.assertEqual(data["packages"], ["npm:pi-mcp-adapter"])

        # A backup file was created next to settings.json before the
        # mutation, per the project's installer convention.
        backups = list(self.paths["pi_settings"].parent.glob(
            "settings.json.mpm-install.bak.*"))
        self.assertEqual(len(backups), 1,
                         "expected exactly one backup before mutation")

    def test_migrates_legacy_agent_installation_pi_mpm_entry(self):
        """The other historical legacy form: ~/.mpm/agent_installation/pi-mpm
        (pre-2026-09-17 directory name, post-rename-of-agent_plugins/).
        Also migrates to the canonical mpm-pi path."""
        # Legacy path under the hermetic temp MPM root, not the original
        # author's checkout. The migration logic only needs ANY non-
        # canonical path under agent_installation/ to exercise the
        # rewrite; using a hermetic path keeps the test host-independent.
        legacy_path = str(self.paths["mpm"] / "agent_installation" / "pi-mpm")
        self.paths["pi_settings"].write_text(json.dumps({
            "extensions": [
                legacy_path,
            ],
        }, indent=2) + "\n", encoding="utf-8")

        proc = _run_installer(self.env)
        self.assertEqual(proc.returncode, 0)

        data = json.loads(self.paths["pi_settings"].read_text())
        canonical = str(self.paths["adapter_dir"])
        self.assertEqual(data["extensions"], [canonical])

    # -- Idempotency ----------------------------------------------------------

    def test_second_run_is_no_op(self):
        """Re-running the installer with the canonical entry already
        present must not duplicate it and must not corrupt other keys."""
        self.paths["pi_settings"].write_text(json.dumps({
            "theme": "dark",
            "extensions": [str(self.paths["adapter_dir"])],
        }, indent=2) + "\n", encoding="utf-8")

        proc1 = _run_installer(self.env)
        self.assertEqual(proc1.returncode, 0)
        first = self.paths["pi_settings"].read_text()

        proc2 = _run_installer(self.env)
        self.assertEqual(proc2.returncode, 0)
        # The file content must be byte-equivalent after the second run.
        # A backup is still created (the project's installer always
        # backs up before mutation), but the file itself does not change
        # because no migration is needed.
        self.assertEqual(self.paths["pi_settings"].read_text(), first)
        data = json.loads(first)
        self.assertEqual(data["extensions"].count(
            str(self.paths["adapter_dir"])), 1)

    # -- Fresh install -------------------------------------------------------

    def test_fresh_install_creates_minimal_settings(self):
        """When ~/.pi/agent/settings.json does not exist, the installer
        creates one containing the canonical extension entry. No other
        keys are invented — the user owns the rest of their config."""
        self.assertFalse(self.paths["pi_settings"].exists())

        proc = _run_installer(self.env)
        self.assertEqual(proc.returncode, 0)

        self.assertTrue(self.paths["pi_settings"].exists())
        data = json.loads(self.paths["pi_settings"].read_text())
        self.assertEqual(data, {
            "extensions": [str(self.paths["adapter_dir"])],
        })

    def test_fresh_install_does_not_backup(self):
        """Backups only happen before a mutation; a fresh install is a
        write not a mutation, so no backup is created."""
        self.assertFalse(self.paths["pi_settings"].exists())

        proc = _run_installer(self.env)
        self.assertEqual(proc.returncode, 0)

        backups = list(self.paths["pi_settings"].parent.glob(
            "settings.json.mpm-install.bak.*"))
        self.assertEqual(backups, [])

    # -- Adapter payload gate -------------------------------------------------

    def test_fails_clearly_when_canonical_adapter_missing(self):
        """If the canonical adapter payload is not installed, the installer
        must fail clearly rather than write a dead path into Pi settings."""
        # Remove the canonical adapter index file.
        (self.paths["adapter_dir"] / "index.ts").unlink()

        proc = _run_installer(self.env)
        self.assertEqual(proc.returncode, 2)
        self.assertIn("canonical Pi adapter index not found",
                      proc.stderr + proc.stdout)

        # No mutation to settings.json.
        self.assertFalse(self.paths["pi_settings"].exists())

    # -- Corruption safety ----------------------------------------------------

    def test_refuses_to_edit_malformed_json(self):
        """If settings.json exists but is not valid JSON, the installer
        refuses rather than overwrites with a corrupt replacement."""
        self.paths["pi_settings"].write_text(
            "{ this is not valid json\n", encoding="utf-8")

        proc = _run_installer(self.env)
        self.assertNotEqual(proc.returncode, 0)

        # Original malformed content preserved (with backup alongside).
        self.assertEqual(
            self.paths["pi_settings"].read_text(),
            "{ this is not valid json\n",
        )
        backups = list(self.paths["pi_settings"].parent.glob(
            "settings.json.mpm-install.bak.*"))
        self.assertEqual(len(backups), 1,
                         "malformed file must be backed up before refusal")

    # -- Verify mode ----------------------------------------------------------

    def test_verify_mode_detects_legacy(self):
        """`install.sh --verify` exits non-zero when the settings file
        has a legacy entry or lacks the canonical entry. Exit-zero
        indicates the live config matches the contract."""
        # Legacy path under the hermetic temp MPM root — see
        # test_migrates_legacy_agent_plugins_pi_mpm_entry for rationale.
        legacy_path = str(self.paths["mpm"] / "agent_plugins" / "pi-mpm")
        self.paths["pi_settings"].write_text(json.dumps({
            "extensions": [legacy_path],
        }) + "\n", encoding="utf-8")

        proc = _run_installer(self.env, "--verify")
        self.assertEqual(proc.returncode, 1)

        # After verification fails, no mutation should have occurred.
        data = json.loads(self.paths["pi_settings"].read_text())
        self.assertEqual(data["extensions"],
                         [legacy_path])

    def test_verify_mode_passes_clean_state(self):
        """`install.sh --verify` exits zero when the canonical entry is
        present and no legacy entries remain."""
        self.paths["pi_settings"].write_text(json.dumps({
            "extensions": [str(self.paths["adapter_dir"])],
        }) + "\n", encoding="utf-8")

        proc = _run_installer(self.env, "--verify")
        self.assertEqual(proc.returncode, 0,
                         f"verify failed: stderr={proc.stderr}")


class TestPiInstallerShellSyntax(unittest.TestCase):
    """Bash syntax check; cheap regression guard for syntax breakage."""

    def test_bash_n_passes(self):
        proc = subprocess.run(
            ["bash", "-n", str(INSTALL_SCRIPT)],
            capture_output=True, text=True, check=False, timeout=10,
        )
        self.assertEqual(proc.returncode, 0,
                         f"bash -n failed: {proc.stderr}")


if __name__ == "__main__":
    unittest.main()