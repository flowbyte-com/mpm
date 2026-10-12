"""test_claude_mcp_registration.py — user-scope MCP registration contract.

The defect this suite exists to prevent: the Claude Code adapter wrote its
server registration to `~/.claude/.mcp.json`, which is **not** a Claude Code
discovery path in any scope. The file landed on disk, install.sh reported
success, and no MCP server ever started.

The real scopes are:

    user     ~/.claude.json                      mcpServers.mpm
    local    ~/.claude.json                      projects.<abs cwd>.mcpServers.mpm
    project  <project>/.mcp.json                 mcpServers.mpm

with local > project > user precedence, so a leftover local entry silently
shadows the user-scope registration.

Two layers of coverage:

  * Unit tests drive `scripts/claude_mcp_config.py` against synthetic config
    files. Fast, and they pin where each scope actually lives.
  * Integration tests run the real `install.sh` against the real `claude` CLI
    inside a throwaway HOME + CLAUDE_CONFIG_DIR, so scope resolution, merge
    behaviour, and backup semantics are exercised rather than assumed.

The integration tests are skipped when the `claude` CLI is unavailable rather
than silently passing — a suite that skips itself quietly is how the original
defect stayed invisible.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

PLUGIN_DIR = Path(__file__).resolve().parent.parent
INSTALL_SH = PLUGIN_DIR / "install.sh"
TEMPLATE = PLUGIN_DIR / ".mcp.json.template"
HELPER = PLUGIN_DIR / "scripts" / "claude_mcp_config.py"

sys_path_importable = True
try:
    import importlib.util

    _spec = importlib.util.spec_from_file_location("claude_mcp_config", HELPER)
    cfgmod = importlib.util.module_from_spec(_spec)
    _spec.loader.exec_module(cfgmod)
except Exception:  # pragma: no cover - surfaced as a skip below
    sys_path_importable = False


def has_claude() -> bool:
    return shutil.which("claude") is not None


class Sandbox:
    """Isolated HOME + CLAUDE_CONFIG_DIR + project dir.

    `CLAUDE_CONFIG_DIR` relocates BOTH `~/.claude.json` and `settings.json`
    into a single directory, so a sandbox here is genuinely complete — the
    real host config is never read or written by these tests.
    """

    def __init__(self):
        self.root = Path(tempfile.mkdtemp(prefix="mpm-cc-reg-"))
        self.home = self.root / "home"
        self.cfg = self.root / "claude-cfg"
        self.project = self.root / "project"
        for d in (self.home, self.cfg, self.project):
            d.mkdir(parents=True, exist_ok=True)
        # install.sh pre-flight requires an executable mpm-mcp under HOME.
        bin_dir = self.home / ".mpm" / "bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        (bin_dir / "mpm-mcp").write_text("#!/bin/sh\nexit 0\n")
        (bin_dir / "mpm-mcp").chmod(0o755)

    @property
    def user_config(self) -> Path:
        return self.cfg / ".claude.json"

    def read_user_config(self) -> dict:
        try:
            return json.loads(self.user_config.read_text())
        except (OSError, json.JSONDecodeError):
            return {}

    def seed_user_config(self, cfg: dict) -> None:
        self.user_config.write_text(json.dumps(cfg, indent=2))

    def seed_local(self, project: Path | None = None, mpm=None, **extra) -> None:
        """Write a local-scope registration into projects.<project>.mcpServers."""
        project = project or self.project
        cfg = self.read_user_config()
        servers = {"mpm": mpm} if mpm else {}
        servers.update(extra)
        cfg.setdefault("projects", {}).setdefault(str(project.resolve()), {})
        cfg["projects"][str(project.resolve())]["mcpServers"] = servers
        self.seed_user_config(cfg)

    def seed_project_file(self, mpm=None, **extra) -> None:
        servers = {"mpm": mpm} if mpm else {}
        servers.update(extra)
        (self.project / ".mcp.json").write_text(json.dumps({"mcpServers": servers}, indent=2))

    def env(self, **extra) -> dict:
        e = dict(os.environ)
        e.update({
            "HOME": str(self.home),
            "CLAUDE_CONFIG_DIR": str(self.cfg),
            # The live handshake has no meaning in a sandbox; the registration
            # contract is what these tests are about.
            "MPM_INSTALL_SKIP_PROBE": "1",
        })
        e.update(extra)
        return e

    def run(self, *args, expect_ok=True) -> subprocess.CompletedProcess:
        p = subprocess.run(
            ["bash", str(INSTALL_SH), *args],
            cwd=str(self.project), env=self.env(),
            capture_output=True, text=True, timeout=180,
        )
        if expect_ok and p.returncode != 0:
            raise AssertionError(
                f"install.sh {args} failed ({p.returncode})\n"
                f"--- stdout ---\n{p.stdout}\n--- stderr ---\n{p.stderr}"
            )
        return p

    def cleanup(self) -> None:
        shutil.rmtree(self.root, ignore_errors=True)


@unittest.skipUnless(sys_path_importable, "config helper not importable")
class ScopeLocation(unittest.TestCase):
    """Pin where each scope actually lives. These are the facts that were wrong."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-cc-unit-"))
        self.config = self.tmp / ".claude.json"
        self.project = self.tmp / "proj"
        self.project.mkdir()
        self.desired = cfgmod.desired_entry(TEMPLATE, "/fake/home")

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_desired_entry_comes_from_template(self):
        self.assertEqual(self.desired["command"], "/fake/home/.mpm/bin/mpm-mcp")
        self.assertEqual(self.desired["env"]["MPM_PROVENANCE_FRAMEWORK"], "claude-code")
        self.assertEqual(self.desired["env"]["MPM_FRAMEWORK"], "claude-code")
        self.assertEqual(self.desired["env"]["MPM_WORKSPACE"], "/fake/home/.mpm")

    def test_user_scope_lives_in_dot_claude_json(self):
        self.config.write_text(json.dumps({"mcpServers": {"mpm": self.desired}}))
        self.assertEqual(cfgmod.user_entry(self.config)["command"], self.desired["command"])

    def test_claude_dir_mcp_json_is_not_a_discovery_path(self):
        """The original defect, pinned as a negative.

        A registration written to ~/.claude/.mcp.json is invisible to every
        scope lookup. Claude Code does not read that path in any scope, so a
        config there registers nothing — which is exactly why the adapter's
        install.sh reported success while no server ever started.
        """
        claude_dir = self.tmp / ".claude"
        claude_dir.mkdir()
        (claude_dir / ".mcp.json").write_text(json.dumps({"mcpServers": {"mpm": self.desired}}))

        # The real user-scope file exists and simply has no mpm in it.
        self.config.write_text(json.dumps({"mcpServers": {"other": {"command": "/bin/echo"}}}))
        self.assertIsNone(cfgmod.user_entry(self.config))
        self.assertIsNone(cfgmod.local_entry(self.config, self.project))
        self.assertIsNone(cfgmod.project_entry(self.project))
        self.assertEqual(
            cfgmod.find_shadows(self.config, self.project, self.desired), [],
            "a file at ~/.claude/.mcp.json must not be mistaken for a registration",
        )

    def test_local_scope_lives_under_projects_key(self):
        self.config.write_text(json.dumps({
            "projects": {str(self.project.resolve()): {
                "mcpServers": {"mpm": self.desired}}}}))
        self.assertEqual(
            cfgmod.local_entry(self.config, self.project)["command"],
            self.desired["command"],
        )

    def test_local_scope_is_per_project(self):
        other = self.tmp / "other"
        other.mkdir()
        self.config.write_text(json.dumps({
            "projects": {str(self.project.resolve()): {
                "mcpServers": {"mpm": self.desired}}}}))
        self.assertIsNone(cfgmod.local_entry(self.config, other))

    def test_project_scope_lives_in_project_mcp_json(self):
        (self.project / ".mcp.json").write_text(json.dumps({"mcpServers": {"mpm": self.desired}}))
        self.assertEqual(cfgmod.project_entry(self.project)["command"], self.desired["command"])

    def test_absent_and_corrupt_configs_read_as_empty(self):
        self.assertIsNone(cfgmod.user_entry(self.tmp / "nope.json"))
        bad = self.tmp / "bad.json"
        bad.write_text("{not json")
        self.assertIsNone(cfgmod.user_entry(bad))


@unittest.skipUnless(sys_path_importable, "config helper not importable")
class Classification(unittest.TestCase):
    """Decide what migration is allowed to remove."""

    def setUp(self):
        self.desired = cfgmod.desired_entry(TEMPLATE, "/fake/home")

    def test_identical_entry_is_canonical(self):
        self.assertEqual(cfgmod.classify(dict(self.desired), self.desired), "canonical")

    def test_manual_env_less_entry_is_legacy(self):
        entry = {"type": "stdio", "command": "/somewhere/mpm-mcp", "args": [], "env": {}}
        self.assertEqual(cfgmod.classify(entry, self.desired), "legacy")

    def test_symlinked_binary_with_no_env_is_legacy(self):
        """The shape a hand-made `claude mcp add mpm -- /path/mpm-mcp` produces."""
        entry = {"type": "stdio", "command": "/home/v/.local/bin/mpm-mcp", "args": [], "env": {}}
        self.assertEqual(cfgmod.classify(entry, self.desired), "legacy")

    def test_foreign_env_keys_make_it_custom(self):
        entry = {"command": "/somewhere/mpm-mcp", "env": {"MY_TOKEN": "x"}}
        self.assertEqual(cfgmod.classify(entry, self.desired), "custom")

    def test_non_mpm_command_is_foreign(self):
        entry = {"command": "/usr/bin/true", "env": {}}
        self.assertEqual(cfgmod.classify(entry, self.desired), "foreign")


@unittest.skipUnless(has_claude(), "claude CLI not on PATH")
class InstallerIntegration(unittest.TestCase):
    """End-to-end installer behaviour against the real Claude Code CLI."""

    def setUp(self):
        self.sb = Sandbox()
        self.addCleanup(self.sb.cleanup)

    # ── fresh install ──────────────────────────────────────────────
    def test_fresh_install_registers_at_user_scope_with_canonical_env(self):
        self.sb.run()
        entry = cfgmod.user_entry(self.sb.user_config)
        self.assertIsNotNone(entry, "no user-scope mpm entry was written")
        self.assertEqual(entry["command"], str(self.sb.home / ".mpm" / "bin" / "mpm-mcp"))
        self.assertEqual(entry["env"]["MPM_WORKSPACE"], str(self.sb.home / ".mpm"))
        self.assertEqual(entry["env"]["MPM_PROVENANCE_FRAMEWORK"], "claude-code")
        self.assertEqual(entry["env"]["MPM_FRAMEWORK"], "claude-code")

    def test_installer_never_writes_claude_dir_mcp_json(self):
        """Regression: the original defect wrote here and Claude Code ignored it."""
        self.sb.run()
        self.assertFalse(
            (self.sb.cfg / ".mcp.json").exists(),
            "installer wrote ~/.claude/.mcp.json, which Claude Code does not read",
        )

    def test_unrelated_user_servers_are_preserved(self):
        self.sb.seed_user_config({"mcpServers": {"other": {"command": "/bin/echo", "env": {}}}})
        self.sb.run()
        cfg = json.loads(self.sb.user_config.read_text())
        self.assertIn("other", cfg["mcpServers"])
        self.assertIn("mpm", cfg["mcpServers"])

    # ── idempotency ────────────────────────────────────────────────
    def test_reinstall_is_idempotent(self):
        self.sb.run()
        first = json.loads(self.sb.user_config.read_text())
        out = self.sb.run()
        second = json.loads(self.sb.user_config.read_text())
        self.assertEqual(first["mcpServers"]["mpm"], second["mcpServers"]["mpm"],
                         "re-running the installer changed the registration")
        self.assertIn("already registered and current", out.stdout)

    def test_reinstall_does_not_duplicate_the_server(self):
        self.sb.run()
        self.sb.run()
        self.sb.run()
        cfg = json.loads(self.sb.user_config.read_text())
        names = [k for k in cfg["mcpServers"]]
        self.assertEqual(names.count("mpm"), 1)

    def test_stale_user_entry_is_updated_not_left_in_place(self):
        """`claude mcp add` refuses to overwrite and exits 0.

        A blind re-add would therefore report success while leaving the stale
        entry in place forever. The installer must detect and correct it.
        """
        self.sb.seed_user_config({"mcpServers": {"mpm": {
            "type": "stdio", "command": "/old/path/mpm-mcp", "args": [], "env": {},
        }}})
        self.sb.run()
        entry = cfgmod.user_entry(self.sb.user_config)
        self.assertEqual(entry["command"], str(self.sb.home / ".mpm" / "bin" / "mpm-mcp"))
        self.assertEqual(entry["env"]["MPM_PROVENANCE_FRAMEWORK"], "claude-code")

    # ── shadow detection ───────────────────────────────────────────
    def test_install_reports_shadowing_local_entry(self):
        self.sb.seed_local(mpm={"type": "stdio", "command": "/somewhere/mpm-mcp", "args": [], "env": {}})
        out = self.sb.run()
        self.assertIn("shadowing MPM registration", out.stdout)
        self.assertIn("--migrate-shadow", out.stdout)

    def test_install_does_not_remove_shadowing_entries(self):
        self.sb.seed_local(mpm={"type": "stdio", "command": "/somewhere/mpm-mcp", "args": [], "env": {}})
        self.sb.run()
        self.assertIsNotNone(cfgmod.local_entry(self.sb.user_config, self.sb.project),
                             "install silently removed a project configuration")

    def test_install_reports_shadowing_project_entry(self):
        self.sb.seed_project_file(mpm={"type": "stdio", "command": "/somewhere/mpm-mcp", "env": {}})
        out = self.sb.run()
        self.assertIn("shadowing MPM registration", out.stdout)

    def test_clean_install_reports_no_shadows(self):
        out = self.sb.run()
        self.assertNotIn("shadowing MPM registration", out.stdout)

    # ── migration ──────────────────────────────────────────────────
    def test_migrate_removes_legacy_local_entry(self):
        self.sb.seed_local(mpm={"type": "stdio", "command": "/somewhere/mpm-mcp", "args": [], "env": {}})
        self.sb.run("--migrate-shadow")
        self.assertIsNone(cfgmod.local_entry(self.sb.user_config, self.sb.project))

    def test_migrate_preserves_unrelated_servers_in_the_same_file(self):
        """The config file holds every project's state; removal must be surgical."""
        self.sb.seed_local(
            mpm={"type": "stdio", "command": "/somewhere/mpm-mcp", "args": [], "env": {}},
            other={"command": "/bin/echo"},
        )
        self.sb.run("--migrate-shadow")
        servers = json.loads(self.sb.user_config.read_text())["projects"][
            str(self.sb.project.resolve())]["mcpServers"]
        self.assertIn("other", servers)
        self.assertNotIn("mpm", servers)

    def test_migrate_backs_up_before_removing(self):
        self.sb.seed_local(mpm={"type": "stdio", "command": "/somewhere/mpm-mcp", "args": [], "env": {}})
        self.sb.run("--migrate-shadow")
        backups = list((self.sb.cfg / "backups").glob("mpm-claude-code-*"))
        self.assertTrue(backups, "no backup directory was created")
        found = [f for d in backups for f in d.glob("*.before-migrate")]
        self.assertTrue(found, "no pre-migration backup was written")
        backed_up = json.loads(found[0].read_text())
        self.assertIn("mpm", backed_up["projects"][str(self.sb.project.resolve())]["mcpServers"],
                      "backup does not contain the entry it was taken to preserve")

    def test_migrate_backed_up_file_can_restore_the_entry(self):
        self.sb.seed_local(mpm={"type": "stdio", "command": "/somewhere/mpm-mcp", "args": [], "env": {}})
        self.sb.run("--migrate-shadow")
        backup = next(f for d in (self.sb.cfg / "backups").glob("mpm-claude-code-*")
                      for f in d.glob("*.before-migrate"))
        shutil.copy(backup, self.sb.user_config)
        self.assertIsNotNone(cfgmod.local_entry(self.sb.user_config, self.sb.project),
                             "restoring the backup did not bring the entry back")

    def test_migrate_leaves_custom_entries_alone(self):
        """An `mpm` entry carrying config this adapter never wrote is the user's."""
        self.sb.seed_local(mpm={"type": "stdio", "command": "/somewhere/mpm-mcp", "env": {"MY_TOKEN": "x"}})
        self.sb.run("--migrate-shadow")
        self.assertIsNotNone(cfgmod.local_entry(self.sb.user_config, self.sb.project))

    def test_migrate_is_a_noop_when_nothing_shadows(self):
        self.sb.run()
        out = self.sb.run("--migrate-shadow")
        self.assertIn("nothing to migrate", out.stdout)

    def test_migrate_removes_project_scoped_entry(self):
        self.sb.seed_project_file(mpm={"type": "stdio", "command": "/somewhere/mpm-mcp", "env": {}})
        self.sb.run("--migrate-shadow")
        self.assertIsNone(cfgmod.project_entry(self.sb.project))

    # ── uninstall ──────────────────────────────────────────────────
    def test_uninstall_removes_user_scope_entry(self):
        self.sb.run()
        self.sb.run("--uninstall")
        self.assertIsNone(cfgmod.user_entry(self.sb.user_config))

    def test_uninstall_preserves_unrelated_servers(self):
        self.sb.seed_user_config({"mcpServers": {"other": {"command": "/bin/echo", "env": {}}}})
        self.sb.run()
        self.sb.run("--uninstall")
        cfg = json.loads(self.sb.user_config.read_text())
        self.assertIn("other", cfg.get("mcpServers", {}))

    def test_uninstall_warns_about_remaining_shadows_without_removing_them(self):
        self.sb.seed_local(mpm={"type": "stdio", "command": "/somewhere/mpm-mcp", "args": [], "env": {}})
        self.sb.run()
        out = self.sb.run("--uninstall")
        self.assertIn("shadowing MPM registration", out.stdout)
        self.assertIsNotNone(cfgmod.local_entry(self.sb.user_config, self.sb.project))


if __name__ == "__main__":
    unittest.main(verbosity=2)