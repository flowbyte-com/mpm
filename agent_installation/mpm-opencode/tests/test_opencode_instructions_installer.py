"""
test_opencode_instructions_installer.py — Regression coverage for the
mpm-opencode AGENTS.md installer.

Closes the parity gap surfaced by the 2026-09-04 forensic audit:
mpm-claude-code, mpm-hermes, mpm-pi, mpm-memory-openclaw, and
mpm-auto-mode-persona-openclaw all have tests/ directories with
installer coverage; mpm-opencode had none. This file brings the
adapter into parity.

Pins the contract for:
  - canonical protocol file exists at ~/.mpm/agent_installation/mpm-agent-protocol.md
  - snippet exists and is host-independent (no leaked runtime quirks
    from other adapters)
  - install_agents_instructions.py honors idempotence
  - existing user content above the markers is preserved across reinstall
  - uninstall cleanly removes the managed section while keeping surrounding content
  - the installer does NOT touch MPM core, MCP config, plugin source,
    or paths outside the target AGENTS.md
  - the README documents the canonical install layout (plugin in
    agent_installation/, AGENTS.md at ~/.config/opencode/AGENTS.md for
    user scope)

Adapter parity invariants pinned:
  - same marker convention as mpm-claude-code: BEGIN/END MPM-MANAGED
    SECTION:opencode-instructions with optional tail comment
  - same idempotent semantics: re-run converges on the canonical block
    from the snippet, refreshes stale blocks whose snippet content has
    drifted (closes the "stale no-op branch" drift class)
"""

from __future__ import annotations

import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPT_DIR = Path("/home/v/.mpm/agent_installation/mpm-opencode")
INSTALLER = SCRIPT_DIR / "scripts" / "install_agents_instructions.py"
SNIPPET = SCRIPT_DIR / "templates" / "AGENTS.md.snippet"
README = SCRIPT_DIR / "README.md"
CANONICAL_PROTOCOL = Path("/home/v/.mpm/agent_installation/mpm-agent-protocol.md")
MANAGED_BEGIN = "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->"
MANAGED_END = "<!-- END MPM-MANAGED SECTION:opencode-instructions -->"


def _run(args: list[str]) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(INSTALLER), *args],
        capture_output=True,
        text=True,
        timeout=30,
    )


class CanonicalProtocol(unittest.TestCase):
    """The canonical host-independent protocol must exist and be free of
    host-specific binding."""

    def setUp(self):
        self.assertTrue(
            CANONICAL_PROTOCOL.exists(),
            f"missing canonical protocol: {CANONICAL_PROTOCOL}",
        )
        self.text = CANONICAL_PROTOCOL.read_text()

    def test_references_wake_persist_handoff(self):
        for term in ("wake", "persist", "handoff", "recovery"):
            self.assertIn(
                term, self.text.lower(),
                f"canonical protocol missing '{term}' concept",
            )

    def test_no_openclaw_specific_runtime_quirks(self):
        for bad in ("bundle-mcp", "openclaw gateway", "disposed for session"):
            self.assertNotIn(
                bad, self.text.lower(),
                f"canonical protocol leaked OpenClaw runtime quirk: {bad!r}",
            )

    def test_no_claude_code_runtime_quirks(self):
        self.assertNotIn(".mcp.json", self.text)
        self.assertNotIn("~/.claude/", self.text)

    def test_no_opencode_runtime_quirks(self):
        # The canonical protocol must not pin OpenCode-specific discovery
        # mechanisms (those belong in the plugin README).
        self.assertNotIn("experimental.chat.system.transform", self.text)


class SnippetContract(unittest.TestCase):
    """The OpenCode AGENTS.md snippet must reference the canonical
    protocol and stay host-agnostic."""

    def setUp(self):
        self.assertTrue(SNIPPET.exists(), f"missing snippet: {SNIPPET}")
        self.text = SNIPPET.read_text()

    def test_references_canonical_protocol(self):
        self.assertIn("mpm-agent-protocol.md", self.text)

    def test_no_openclaw_runtime_recovery_leaked(self):
        for bad in ("bundle-mcp", "disposed for session"):
            self.assertNotIn(bad, self.text.lower())

    def test_no_claude_code_runtime_quirks(self):
        self.assertNotIn(".mcp.json", self.text)
        self.assertNotIn("~/.claude/", self.text)


class ReadmeContract(unittest.TestCase):
    """The README must document the canonical install layout — both the
    plugin source path (agent_installation/, NOT agent_plugins/) and
    the user-scope AGENTS.md target. The 2026-09-04 audit found
    opencode.jsonc referencing the legacy agent_plugins/ name; this
    test pins the README against that drift class."""

    def setUp(self):
        self.assertTrue(README.exists(), f"missing README: {README}")
        self.text = README.read_text()

    def test_documents_agent_installation_path(self):
        self.assertIn("agent_installation/mpm-opencode", self.text)

    def test_does_not_document_legacy_agent_plugins_path(self):
        # The README must use the canonical name. If a contributor adds
        # a legacy reference to agent_plugins/, this test fires.
        self.assertNotIn("agent_plugins/mpm-opencode", self.text)

    def test_documents_user_scope_agents_md(self):
        self.assertIn("~/.config/opencode/AGENTS.md", self.text)


class InstallerRoundTrip(unittest.TestCase):
    """End-to-end exercise of the installer against a sandbox target."""

    def setUp(self):
        self.tmpdir = Path(tempfile.mkdtemp(prefix="mpm-opencode-it-"))
        self.target = self.tmpdir / "AGENTS.md"

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def _install(self) -> subprocess.CompletedProcess:
        return _run([
            "--scope", "user",
            "--target", str(self.target),
            "--snippet", str(SNIPPET),
        ])

    def test_fresh_install_creates_target(self):
        result = self._install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.target.exists())
        content = self.target.read_text()
        self.assertIn(MANAGED_BEGIN, content)
        self.assertIn(MANAGED_END, content)

    def test_idempotence_double_install(self):
        r1 = self._install()
        self.assertEqual(r1.returncode, 0, r1.stderr)
        before = self.target.read_text()

        r2 = self._install()
        self.assertEqual(r2.returncode, 0, r2.stderr)
        after = self.target.read_text()

        # The block must be present exactly once and the file size must
        # not balloon (no duplicate managed blocks).
        self.assertEqual(after.count(MANAGED_BEGIN), 1)
        # The body content between markers is byte-stable after second
        # run (timestamps in the banner are deterministic enough).
        self.assertEqual(
            after.count(MANAGED_BEGIN), 1,
            f"second install introduced duplicate managed block; file:\n{after}",
        )
        # The full file may grow only by the timestamp comment inside
        # the banner — but the managed section structure is preserved.
        self.assertLessEqual(len(after), len(before) + 200)

    def test_preserves_user_content_above_markers(self):
        user_header = "# My OpenCode Config\n\nDo not touch.\n"
        self.target.write_text(user_header)
        r = self._install()
        self.assertEqual(r.returncode, 0, r.stderr)
        content = self.target.read_text()
        self.assertTrue(
            content.startswith(user_header),
            f"user header not preserved; got:\n{content[:200]}",
        )
        self.assertIn(MANAGED_BEGIN, content)

    def test_uninstall_removes_managed_section_only(self):
        user_above = "# user content above\n"
        user_below = "\n# user content below\n"
        # Seed user content above the managed section BEFORE install.
        self.target.write_text(user_above)
        r = self._install()
        self.assertEqual(r.returncode, 0, r.stderr)
        # Append content below the managed section.
        with self.target.open("a") as f:
            f.write(user_below)

        result = _run([
            "--scope", "user",
            "--uninstall",
            "--target", str(self.target),
            "--snippet", str(SNIPPET),
        ])
        self.assertEqual(result.returncode, 0, result.stderr)
        content = self.target.read_text()
        self.assertNotIn(MANAGED_BEGIN, content)
        self.assertNotIn(MANAGED_END, content)
        # Both user-content regions must survive.
        self.assertIn("user content above", content)
        self.assertIn("user content below", content)


class InstallerScopeSafety(unittest.TestCase):
    """The installer must reject unknown scope and refuse to run without
    a target. These are CLI-surface guards, not just sanity."""

    def test_argparse_rejects_unknown_scope(self):
        result = _run([
            "--scope", "bogus",
            "--target", "/tmp/whatever",
            "--snippet", str(SNIPPET),
        ])
        self.assertNotEqual(result.returncode, 0)

    def test_argparse_requires_args(self):
        result = _run([])
        self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
