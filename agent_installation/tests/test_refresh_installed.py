"""
test_refresh_installed.py — Structural smoke test for `make refresh-installed`.

The target refreshes every persistent-file host's managed-block installer
in place. Each installer is content-aware (idempotent when current,
refreshes when stale), so re-running is safe. This test verifies the
target is wired up correctly:

  - The target is registered in the Makefile's `.PHONY` line.
  - The AGENT_INSTALL_DIR variable resolves to a real directory.
  - Every Python installer script referenced by the target exists.
  - Every per-host template snippet the installers consume exists.
  - `render_managed_blocks.py --check` (the post-refresh verifier) is
    referenced correctly.

This is structural only — the end-to-end refresh is exercised by
running `make refresh-installed` itself (output shows each adapter's
status and a final render check pass). The unit-test counterpart lives
in `test_cross_adapter_contract_parity.py` for the installers' content-
aware behaviour.
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path


# Path is derived from the test file location so the suite runs in any
# environment (CI, fresh clone, alternate mount). Previously hardcoded
# to /home/v/workspace/projects/mpm, which broke in every other
# workspace AND in the github actions runner.
REPO_ROOT = Path(__file__).resolve().parents[2]
MAKEFILE = REPO_ROOT / "Makefile"
AGENT_INSTALL_DIR = REPO_ROOT / "agent_installation"


_TARGET_BLOCK_RE = re.compile(
    r"^refresh-installed:\s*\n(?P<body>(?:^[ \t].*\n?)+)",
    re.MULTILINE,
)


class RefreshInstalledTargetRegistered(unittest.TestCase):
    """`make refresh-installed` must be registered in `.PHONY` and define
    a working recipe."""

    @classmethod
    def setUpClass(cls):
        cls.makefile_text = MAKEFILE.read_text(encoding="utf-8")
        m = _TARGET_BLOCK_RE.search(cls.makefile_text)
        cls.recipe_body = m.group("body") if m else ""

    def test_phony_contains_refresh_installed(self):
        # Use MULTILINE so `^` matches the start of any line, not just
        # the start of the file (the `.PHONY` line is preceded by many
        # comment lines in the Makefile).
        self.assertRegex(
            self.makefile_text,
            r"(?m)^\.PHONY:.*\brefresh-installed\b",
            "Makefile `.PHONY` line must list `refresh-installed`",
        )

    def test_target_block_exists(self):
        self.assertTrue(
            bool(self.recipe_body),
            "Makefile must define a `refresh-installed:` target recipe",
        )

    def test_recipe_invokes_render_script(self):
        self.assertIn(
            "render_managed_blocks.py", self.recipe_body,
            "refresh-installed recipe must invoke render_managed_blocks.py",
        )

    def test_recipe_invokes_claude_installer(self):
        self.assertIn(
            "install_claude_instructions.py", self.recipe_body,
            "refresh-installed recipe must invoke install_claude_instructions.py",
        )

    def test_recipe_invokes_opencode_install_sh(self):
        # mpm-opencode's install.sh performs the namespace-refresh
        # migration (opencode.jsonc stale entry → canonical path) plus
        # the dist/ rebuild. refresh-installed must invoke it.
        self.assertIn(
            "mpm-opencode/install.sh", self.recipe_body,
            "refresh-installed recipe must invoke mpm-opencode/install.sh for namespace refresh",
        )

    def test_recipe_invokes_opencode_and_pi_installer(self):
        # OpenCode and Pi share the same installer filename but live in
        # different adapter directories — both invocations should appear.
        self.assertIn(
            "mpm-opencode/scripts/install_agents_instructions.py", self.recipe_body,
            "refresh-installed recipe must invoke mpm-opencode's installer",
        )
        self.assertIn(
            "mpm-pi/scripts/install_agents_instructions.py", self.recipe_body,
            "refresh-installed recipe must invoke mpm-pi's installer",
        )

    def test_post_refresh_render_check_is_invoked(self):
        # The recipe must end with `render_managed_blocks.py --check` to
        # verify byte-for-byte parity after refresh.
        self.assertIn(
            "--check", self.recipe_body,
            "refresh-installed must end with render_managed_blocks.py --check",
        )


class RefreshInstalledPathsResolve(unittest.TestCase):
    """Every script and snippet referenced by the refresh target must
    exist on disk. Catches drift if an installer script is renamed or a
    template snippet is moved."""

    def test_agent_installation_dir_exists(self):
        self.assertTrue(
            AGENT_INSTALL_DIR.is_dir(),
            f"AGENT_INSTALL_DIR does not exist: {AGENT_INSTALL_DIR}",
        )

    def test_render_script_exists(self):
        self.assertTrue(
            (AGENT_INSTALL_DIR / "scripts" / "render_managed_blocks.py").is_file(),
            "render_managed_blocks.py missing",
        )

    def test_claude_code_installer_exists(self):
        self.assertTrue(
            (AGENT_INSTALL_DIR / "mpm-claude-code" / "scripts" / "install_claude_instructions.py").is_file(),
            "mpm-claude-code installer missing",
        )

    def test_opencode_installer_exists(self):
        self.assertTrue(
            (AGENT_INSTALL_DIR / "mpm-opencode" / "scripts" / "install_agents_instructions.py").is_file(),
            "mpm-opencode installer missing",
        )

    def test_pi_installer_exists(self):
        self.assertTrue(
            (AGENT_INSTALL_DIR / "mpm-pi" / "scripts" / "install_agents_instructions.py").is_file(),
            "mpm-pi installer missing",
        )

    def test_hermes_installer_exists(self):
        # The hermes installer is part of the supported adapter set
        # even though `make refresh-installed` skips the installed-
        # block step (no installed managed block in this environment).
        # The script itself must still exist on disk.
        self.assertTrue(
            (AGENT_INSTALL_DIR / "mpm-hermes" / "scripts" / "install_hermes_instructions.py").is_file(),
            "mpm-hermes installer missing",
        )

    def test_all_template_snippets_exist(self):
        """The installers consume these per-host rendered snippets."""
        expected = {
            "mpm-claude-code": "mpm-claude-code/templates/CLAUDE.md.snippet",
            "mpm-opencode":    "mpm-opencode/templates/AGENTS.md.snippet",
            "mpm-pi":          "mpm-pi/templates/AGENTS.md.snippet",
            "mpm-hermes":      "mpm-hermes/templates/hermes.md.snippet",
        }
        for name, rel in expected.items():
            path = AGENT_INSTALL_DIR / rel
            self.assertTrue(
                path.is_file(),
                f"{name}: template snippet missing at {path}",
            )


if __name__ == "__main__":
    unittest.main(verbosity=2)
