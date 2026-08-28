"""Regression tests for the Pi MPM instructions installer.

Pi looks for AGENTS.md (or CLAUDE.md) at session start per pi docs:

  1. ~/.pi/agent/AGENTS.md — global, all projects
  2. AGENTS.md in any parent directory of cwd (walking up)
  3. AGENTS.md in cwd — current project

These tests pin the contract documented in install_agents_instructions.py:
fresh, replaced, appended, no-op, uninstall, corrupted-target refusal,
preserves user content above and below, scope resolution (user vs project).
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

INSTALLER = Path(__file__).resolve().parent.parent / "scripts" / "install_agents_instructions.py"
SNIPPET = Path(__file__).resolve().parent.parent / "templates" / "AGENTS.md.snippet"


def _run(args: list[str], cwd: Path | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(INSTALLER), *args],
        capture_output=True,
        text=True,
        cwd=cwd,
        timeout=30,
    )


class TestCanonicalProtocolIsolation(unittest.TestCase):
    """The Pi snippet must not contain host-specific runtime quirks."""

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

    def test_no_hermes_runtime_quirks(self):
        self.assertNotIn(".hermes.md", self.snippet_text)
        self.assertNotIn("HERMES.md", self.snippet_text)

    def test_pi_doc_quirks_only_in_specific_section(self):
        # --no-context-files / -nc is a Pi CLI flag; it should appear only in the
        # Pi-specific notes section, not leak into the canonical protocol.
        self.assertIn("--no-context-files", self.snippet_text)
        idx = self.snippet_text.index("## Pi-specific notes")
        nc_idx = self.snippet_text.index("--no-context-files")
        self.assertGreater(nc_idx, idx)


class TestSnippetInvariants(unittest.TestCase):
    def setUp(self):
        self.snippet_text = SNIPPET.read_text(encoding="utf-8")

    def test_snippet_contains_no_marker(self):
        self.assertNotIn("BEGIN MPM-MANAGED SECTION:pi-instructions", self.snippet_text)
        self.assertNotIn("END MPM-MANAGED SECTION:pi-instructions", self.snippet_text)


class TestInstallerContract(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="mpm-pi-test-"))

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_fresh_install_user_scope(self):
        # Use --target explicitly to avoid writing to the real ~/.pi/agent/.
        target = self.tmp / ".pi" / "agent" / "AGENTS.md"
        cp = _run(["--scope", "user", "--target", str(target), "--snippet", str(SNIPPET)])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertTrue(target.exists())
        text = target.read_text(encoding="utf-8")
        self.assertEqual(text.count("<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->"), 1)
        self.assertEqual(text.count("<!-- END MPM-MANAGED SECTION:pi-instructions -->"), 1)
        self.assertIn("Wake on session start", text)
        self.assertIn("Handoff before genuine session closure", text)

    def test_fresh_install_project_scope(self):
        cp = _run(["--scope", "project", "--target-dir", str(self.tmp), "--snippet", str(SNIPPET)])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertTrue((self.tmp / "AGENTS.md").exists())

    def test_idempotent_double_install_is_noop(self):
        target = self.tmp / "AGENTS.md"
        _run(["--target", str(target), "--snippet", str(SNIPPET)])
        cp2 = _run(["--target", str(target), "--snippet", str(SNIPPET)])
        self.assertEqual(cp2.returncode, 0, cp2.stderr)
        self.assertIn("no-op", cp2.stdout)
        backups = list(target.parent.glob(f"{target.name}.bak.*"))
        self.assertEqual(len(backups), 0)

    def test_replacement_when_snippet_changes(self):
        target = self.tmp / "AGENTS.md"
        _run(["--target", str(target), "--snippet", str(SNIPPET)])
        new_snip = self.tmp / "modified.snippet"
        new_snip.write_text("Modified body v2\n", encoding="utf-8")
        cp = _run(["--target", str(target), "--snippet", str(new_snip)])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertIn("replaced", cp.stdout)
        text = target.read_text(encoding="utf-8")
        self.assertIn("Modified body v2", text)
        self.assertTrue(list(target.parent.glob(f"{target.name}.bak.*")))

    def test_preserves_user_content_above_markers(self):
        target = self.tmp / "AGENTS.md"
        target.write_text("# My project notes\n\nKeep me.\n", encoding="utf-8")
        _run(["--target", str(target), "--snippet", str(SNIPPET)])
        text = target.read_text(encoding="utf-8")
        self.assertLess(text.index("# My project notes"), text.index("BEGIN MPM-MANAGED SECTION"))
        self.assertIn("Keep me.", text)

    def test_preserves_user_content_below_markers(self):
        target = self.tmp / "AGENTS.md"
        _run(["--target", str(target), "--snippet", str(SNIPPET)])
        with target.open("a", encoding="utf-8") as f:
            f.write("\n# Tail notes\n")
        text = target.read_text(encoding="utf-8")
        self.assertGreater(text.index("# Tail notes"), text.index("END MPM-MANAGED SECTION"))

    def test_corrupted_target_refuses_to_mutate(self):
        target = self.tmp / "AGENTS.md"
        target.write_text("<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->\nstuff\n", encoding="utf-8")
        cp = _run(["--target", str(target), "--snippet", str(SNIPPET)])
        self.assertEqual(cp.returncode, 2)
        self.assertEqual(
            target.read_text(encoding="utf-8"),
            "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->\nstuff\n",
        )

    def test_corrupted_target_refuses_to_uninstall(self):
        target = self.tmp / "AGENTS.md"
        target.write_text("<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->\nstuff\n", encoding="utf-8")
        cp = _run(["--target", str(target), "--uninstall"])
        self.assertEqual(cp.returncode, 2)

    def test_uninstall_strips_managed_section(self):
        target = self.tmp / "AGENTS.md"
        _run(["--target", str(target), "--snippet", str(SNIPPET)])
        with target.open("a", encoding="utf-8") as f:
            f.write("\n# Tail\n")
        cp = _run(["--target", str(target), "--uninstall"])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertIn("removed", cp.stdout)
        text = target.read_text(encoding="utf-8")
        self.assertNotIn("BEGIN MPM-MANAGED SECTION", text)
        self.assertIn("# Tail", text)

    def test_uninstall_unlinks_empty_target(self):
        target = self.tmp / "AGENTS.md"
        _run(["--target", str(target), "--snippet", str(SNIPPET)])
        cp = _run(["--target", str(target), "--uninstall"])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertIn("empty-unlinked", cp.stdout)
        self.assertFalse(target.exists())

    def test_uninstall_absent_target(self):
        target = self.tmp / "AGENTS.md"
        cp = _run(["--target", str(target), "--uninstall"])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertIn("absent", cp.stdout)

    def test_filename_alternative(self):
        target = self.tmp / "CLAUDE.md"
        cp = _run(["--target", str(target), "--snippet", str(SNIPPET), "--filename", "CLAUDE.md"])
        self.assertEqual(cp.returncode, 0, cp.stderr)
        self.assertTrue(target.exists())

    def test_scope_user_default_path(self):
        # No --target: should resolve to ~/.pi/agent/AGENTS.md.
        # Run with HOME=tmp so the resolved path stays inside the test sandbox.
        cp = _run(
            ["--scope", "user", "--filename", "AGENTS.md", "--snippet", str(SNIPPET)],
            cwd=str(self.tmp),
        )
        # Override HOME via env for the subprocess:
        cp2 = subprocess.run(
            [sys.executable, str(INSTALLER), "--scope", "user", "--snippet", str(SNIPPET)],
            capture_output=True,
            text=True,
            env={**os.environ, "HOME": str(self.tmp)},
            timeout=10,
        )
        self.assertEqual(cp2.returncode, 0, cp2.stderr)
        # The installer wrote to <HOME>/.pi/agent/AGENTS.md.
        target = Path(str(self.tmp)) / ".pi" / "agent" / "AGENTS.md"
        self.assertTrue(target.exists())
        text = target.read_text(encoding="utf-8")
        self.assertIn("BEGIN MPM-MANAGED SECTION:pi-instructions", text)


if __name__ == "__main__":
    unittest.main()
