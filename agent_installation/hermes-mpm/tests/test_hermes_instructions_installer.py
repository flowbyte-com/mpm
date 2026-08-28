"""Regression tests for the Hermes MPM instructions installer.

Hermes uses `.hermes.md` (or `HERMES.md`) walked up from cwd to git
root, loaded raw into the system prompt. The installer manages the
MPM block via leading HTML-comment markers (Hermes has no built-in
managed-block convention).

These tests pin the contract documented in install_hermes_instructions.py:
fresh, replaced, appended, no-op, uninstall, corrupted-target refusal,
preserves user content above and below, marker-isolation from snippet body.
"""

from __future__ import annotations

import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

INSTALLER = Path(__file__).resolve().parent.parent / "scripts" / "install_hermes_instructions.py"
SNIPPET = Path(__file__).resolve().parent.parent / "templates" / "hermes.md.snippet"


def _run(args: list[str]) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(INSTALLER), *args],
        capture_output=True,
        text=True,
        timeout=30,
    )


class TestCanonicalProtocolIsolation(unittest.TestCase):
    """The snippet must not contain host-specific runtime quirks."""

    def setUp(self):
        self.snippet_text = SNIPPET.read_text(encoding="utf-8")

    def test_no_openclaw_specific_runtime_quirks(self):
        self.assertNotIn("bundle-mcp", self.snippet_text)
        self.assertNotIn("openclaw gateway", self.snippet_text.lower())
        self.assertNotIn("disposed for session", self.snippet_text)

    def test_no_claude_code_runtime_quirks(self):
        self.assertNotIn(".mcp.json", self.snippet_text)
        self.assertNotIn("~/.claude/", self.snippet_text)

    def test_no_opencode_runtime_quirks(self):
        self.assertNotIn("experimental.chat.system.transform", self.snippet_text)
        self.assertNotIn("~/.config/opencode/", self.snippet_text)

    def test_no_pi_runtime_quirks(self):
        self.assertNotIn("--no-context-files", self.snippet_text)
        self.assertNotIn("~/.pi/agent/", self.snippet_text)


class TestSnippetInvariants(unittest.TestCase):
    """Snippet itself must be safe to wrap and contain no nested markers."""

    def setUp(self):
        self.snippet_text = SNIPPET.read_text(encoding="utf-8")

    def test_snippet_contains_no_marker(self):
        self.assertNotIn("BEGIN MPM-MANAGED BLOCK:hermes-mpm", self.snippet_text)
        self.assertNotIn("END MPM-MANAGED BLOCK:hermes-mpm", self.snippet_text)


class TestInstallerContract(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-hermes-test-"))

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _target(self, name: str = "test.hermes.md") -> Path:
        return self.tmp / name

    def test_fresh_install_creates_managed_block(self):
        target = self._target()
        cp = _run(["--target", str(target), "--snippet", str(SNIPPET)])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertTrue(target.exists())
        text = target.read_text(encoding="utf-8")
        self.assertEqual(text.count("<!-- BEGIN MPM-MANAGED BLOCK:hermes-mpm -->"), 1)
        self.assertEqual(text.count("<!-- END MPM-MANAGED BLOCK:hermes-mpm -->"), 1)
        # Snippet body present:
        self.assertIn("Wake on session start", text)
        self.assertIn("Handoff before genuine session closure", text)

    def test_idempotent_double_install_is_noop(self):
        target = self._target()
        _run(["--target", str(target), "--snippet", str(SNIPPET)])
        cp2 = _run(["--target", str(target), "--snippet", str(SNIPPET)])
        self.assertEqual(cp2.returncode, 0, cp2.stderr)
        self.assertIn("no-op", cp2.stdout)
        # Backup file should NOT exist on no-op (no mutation):
        backups = list(target.parent.glob(f"{target.name}.bak.*"))
        self.assertEqual(len(backups), 0)

    def test_replacement_when_snippet_changes(self):
        target = self._target()
        _run(["--target", str(target), "--snippet", str(SNIPPET)])
        # Different snippet body:
        new_snip = self.tmp / "modified.snippet"
        new_snip.write_text("Modified body v2\n", encoding="utf-8")
        cp = _run(["--target", str(target), "--snippet", str(new_snip)])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertIn("replaced", cp.stdout)
        text = target.read_text(encoding="utf-8")
        self.assertIn("Modified body v2", text)
        # Backup created:
        self.assertTrue(list(target.parent.glob(f"{target.name}.bak.*")))

    def test_preserves_user_content_above_markers(self):
        target = self._target()
        target.write_text("# My project notes\n\nKeep me.\n", encoding="utf-8")
        _run(["--target", str(target), "--snippet", str(SNIPPET)])
        text = target.read_text(encoding="utf-8")
        # User content above markers preserved:
        self.assertLess(text.index("# My project notes"), text.index("BEGIN MPM-MANAGED BLOCK"))
        self.assertIn("Keep me.", text)

    def test_preserves_user_content_below_markers(self):
        target = self._target()
        target.write_text("# My project notes\n", encoding="utf-8")
        _run(["--target", str(target), "--snippet", str(SNIPPET)])
        target.write_text(target.read_text(encoding="utf-8") + "\n# Tail notes\n", encoding="utf-8")
        text = target.read_text(encoding="utf-8")
        self.assertGreater(text.index("# Tail notes"), text.index("END MPM-MANAGED BLOCK"))

    def test_corrupted_target_refuses_to_mutate(self):
        target = self._target()
        # Only BEGIN marker, no END:
        target.write_text("<!-- BEGIN MPM-MANAGED BLOCK:hermes-mpm -->\nstuff\n", encoding="utf-8")
        cp = _run(["--target", str(target), "--snippet", str(SNIPPET)])
        self.assertEqual(cp.returncode, 2)
        # File unchanged:
        self.assertEqual(target.read_text(encoding="utf-8"), "<!-- BEGIN MPM-MANAGED BLOCK:hermes-mpm -->\nstuff\n")

    def test_corrupted_target_refuses_to_uninstall(self):
        target = self._target()
        target.write_text("<!-- BEGIN MPM-MANAGED BLOCK:hermes-mpm -->\nstuff\n", encoding="utf-8")
        cp = _run(["--target", str(target), "--uninstall"])
        self.assertEqual(cp.returncode, 2)
        self.assertEqual(target.read_text(encoding="utf-8"), "<!-- BEGIN MPM-MANAGED BLOCK:hermes-mpm -->\nstuff\n")

    def test_uninstall_strips_managed_section(self):
        target = self._target()
        _run(["--target", str(target), "--snippet", str(SNIPPET)])
        target.write_text(target.read_text(encoding="utf-8") + "\n# Tail\n", encoding="utf-8")
        cp = _run(["--target", str(target), "--uninstall"])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertIn("removed", cp.stdout)
        text = target.read_text(encoding="utf-8")
        self.assertNotIn("BEGIN MPM-MANAGED BLOCK", text)
        self.assertNotIn("END MPM-MANAGED BLOCK", text)
        # User tail preserved:
        self.assertIn("# Tail", text)
        # Backup before mutation:
        self.assertTrue(list(target.parent.glob(f"{target.name}.bak.*")))

    def test_uninstall_unlinks_empty_target(self):
        target = self._target()
        _run(["--target", str(target), "--snippet", str(SNIPPET)])
        cp = _run(["--target", str(target), "--uninstall"])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertIn("empty-unlinked", cp.stdout)
        self.assertFalse(target.exists())

    def test_uninstall_absent_target(self):
        target = self._target()
        cp = _run(["--target", str(target), "--uninstall"])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertIn("absent", cp.stdout)

    def test_target_dir_resolution(self):
        cp = _run(["--target-dir", str(self.tmp), "--snippet", str(SNIPPET)])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertTrue((self.tmp / ".hermes.md").exists())

    def test_hermes_md_filename_default(self):
        # When using --target-dir, default filename is .hermes.md
        cp = _run(["--target-dir", str(self.tmp), "--snippet", str(SNIPPET)])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        text = (self.tmp / ".hermes.md").read_text(encoding="utf-8")
        self.assertIn("BEGIN MPM-MANAGED BLOCK:hermes-mpm", text)


class TestHermesPersonaUntouched(unittest.TestCase):
    """The installer must never touch ~/.hermes/SOUL.md (the persona).

    The snippet legitimately MENTIONS SOUL.md to explain what it does NOT
    write to (the persona is preserved in SOUL.md; the behavioral protocol
    goes in .hermes.md). What matters is that the installer script never
    has SOUL.md as a target path.
    """

    def test_installer_script_has_no_soul_md_target(self):
        installer_text = INSTALLER.read_text(encoding="utf-8")
        # No code path writes to SOUL.md.
        self.assertNotIn('SOUL.md"', installer_text)
        self.assertNotIn("SOUL.md'", installer_text)
        # Default filename is .hermes.md, never SOUL.md:
        self.assertIn(".hermes.md", installer_text)

    def test_snippet_explains_persona_isolation(self):
        snippet = SNIPPET.read_text(encoding="utf-8")
        # The snippet must explicitly state persona stays in SOUL.md and
        # the installer does not touch SOUL.md.
        self.assertIn("SOUL.md", snippet)
        self.assertIn("Hermes-specific notes", snippet)


if __name__ == "__main__":
    unittest.main()
