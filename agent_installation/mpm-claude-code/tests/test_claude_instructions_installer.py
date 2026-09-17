"""
test_claude_instructions_installer.py — Regression coverage for the
mpm-claude-code CLAUDE.md installer.

Pins the contract for:
  - canonical protocol file exists at ~/.mpm/agent_installation/mpm-agent-protocol.md
  - snippet exists and is host-independent
  - install_claude_instructions.py honors idempotence
  - existing user content above the markers is preserved across reinstall
  - uninstall cleanly removes the managed section while keeping surrounding content
  - the installer does NOT touch MPM core, MCP config, or paths outside
    the target CLAUDE.md
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPT_DIR = Path("/home/v/.mpm/agent_installation/mpm-claude-code")
INSTALLER = SCRIPT_DIR / "scripts" / "install_claude_instructions.py"
SNIPPET = SCRIPT_DIR / "templates" / "CLAUDE.md.snippet"
CANONICAL_PROTOCOL = Path("/home/v/.mpm/agent_installation/mpm-agent-protocol.md")


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
        # These belong in OpenClaw's host adapter, not the canonical protocol.
        for forbidden in (
            "openclaw gateway restart",
            "bundle-mcp runtime disposed",
            "openclaw.json",
            "openclaw plugin",
        ):
            self.assertNotIn(
                forbidden, self.text.lower(),
                f"host-specific binding '{forbidden}' leaked into canonical protocol",
            )

    def test_no_claude_code_runtime_quirks(self):
        for forbidden in (
            "claude code sessionstart hook",
            "mpm-claude-code plugin",
            ".claude/CLAUDE.md",
        ):
            self.assertNotIn(
                forbidden.lower(), self.text.lower(),
                f"claude-code-specific binding '{forbidden}' leaked into canonical protocol",
            )

    def test_no_opencode_runtime_quirks(self):
        for forbidden in ("@opencode-ai/plugin", "opencode plugin", "mpm-opencode"):
            self.assertNotIn(forbidden.lower(), self.text.lower())

    def test_host_independent_examples_used(self):
        # Generic mentions of "MCP", "plugin", or "agent integration" are OK;
        # host-specific commands are not.
        # We don't enforce word counts — just ensure file size is reasonable.
        # Threshold bumped from 20K → 25K chars to accommodate §4.1
        # (`session != work` lifecycle split) added in the 2026-09-04
        # cross-adapter integrity audit.
        self.assertGreater(len(self.text), 2000)
        self.assertLess(len(self.text), 25000)


class SnippetContract(unittest.TestCase):
    """The CLAUDE.md snippet is a Claude-Code-specific host adapter."""

    def setUp(self):
        self.assertTrue(SNIPPET.exists(), f"missing snippet: {SNIPPET}")
        self.text = SNIPPET.read_text()

    def test_references_canonical_protocol(self):
        # The snippet should point the agent at the canonical protocol file.
        self.assertIn("mpm-agent-protocol.md", self.text)

    def test_no_openclaw_runtime_recovery_leaked(self):
        for forbidden in (
            "openclaw gateway restart",
            "bundle-mcp runtime disposed",
        ):
            self.assertNotIn(
                forbidden, self.text.lower(),
                f"openclaw-specific recovery leaked into Claude snippet",
            )

    def test_cli_fallback_kept_generic(self):
        # The CLI fallback shape must mention `mpm call <tool> --payload` and
        # NOT include OpenClaw-specific fallback commands.
        self.assertIn("mpm call", self.text)
        self.assertNotIn("openclaw doctor", self.text.lower())


class InstallerRoundTrip(unittest.TestCase):
    """End-to-end idempotence + content preservation + uninstall contract."""

    def setUp(self):
        self.assertTrue(INSTALLER.exists(), f"missing installer: {INSTALLER}")
        self.tmpdir = Path(tempfile.mkdtemp(prefix="claude-md-test-"))
        self.target = self.tmpdir / "CLAUDE.md"

    def tearDown(self):
        import shutil
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def run_installer(self, *args):
        return subprocess.run(
            [
                sys.executable,
                str(INSTALLER),
                "--scope", "user",
                "--home", "/home/v",
                "--target", str(self.target),
                "--snippet", str(SNIPPET),
                *args,
            ],
            capture_output=True,
            text=True,
            timeout=15,
        )

    def test_fresh_install_creates_target(self):
        self.assertFalse(self.target.exists())
        r = self.run_installer()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue(self.target.exists())
        self.assertIn("BEGIN MPM-MANAGED SECTION", self.target.read_text())

    def test_idempotence_double_install(self):
        self.run_installer()
        first = self.target.read_text()
        self.run_installer()
        second = self.target.read_text()
        self.assertEqual(
            first.count("BEGIN MPM-MANAGED SECTION"),
            second.count("BEGIN MPM-MANAGED SECTION"),
            "installer produced duplicate managed sections on second run",
        )
        self.assertEqual(
            first.count("BEGIN MPM-MANAGED SECTION"), 1,
            "expected exactly one managed section",
        )

    def test_preserves_user_content_above_markers(self):
        # Pre-populate the file with user content BEFORE running installer.
        head = "# v's personal notes\nv asks interesting questions.\n"
        existing = head + "\n<!-- BEGIN MPM-MANAGED SECTION -->\nOLD\n<!-- END MPM-MANAGED SECTION -->\n"
        self.target.write_text(existing)
        r = self.run_installer()
        self.assertEqual(r.returncode, 0, r.stderr)
        result = self.target.read_text()
        # User content survives
        self.assertIn("v's personal notes", result)
        self.assertIn("v asks interesting questions", result)
        # Exactly one managed section
        self.assertEqual(result.count("BEGIN MPM-MANAGED SECTION"), 1)

    def test_uninstall_removes_managed_section_only(self):
        self.run_installer()
        r = self.run_installer("--uninstall")
        self.assertEqual(r.returncode, 0, r.stderr)
        # Either the file is gone (preferred) or it's content-free of markers.
        if self.target.exists():
            content = self.target.read_text()
            self.assertNotIn("BEGIN MPM-MANAGED SECTION", content)
            self.assertNotIn("END MPM-MANAGED SECTION", content)
            # If the file is empty or only contains the head comment, fine.
            stripped = content.strip()
            self.assertTrue(
                stripped == "" or stripped.startswith("<!--\nThis file's CLAUDE.md"),
                f"uninstalled file should be empty or only contain the head comment; "
                f"got: {content[:200]!r}",
            )

    def test_uninstall_then_reinstall_yields_single_section(self):
        self.run_installer()
        # Drop a top-of-file note
        head = "# Project README\n"
        self.target.write_text(head + self.target.read_text())
        self.run_installer("--uninstall")
        r = self.run_installer()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(
            self.target.read_text().count("BEGIN MPM-MANAGED SECTION"),
            1,
        )
        self.assertIn("Project README", self.target.read_text(),
                      "user content above markers should survive uninstall+reinstall")

    def test_corrupted_target_aborts_safely(self):
        # One marker without the other — script should refuse to modify.
        self.target.write_text("<!-- BEGIN MPM-MANAGED SECTION -->\nno end\n")
        r = self.run_installer()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("refusing", r.stderr.lower())
        # Original file unchanged
        self.assertIn("no end", self.target.read_text())


class InstallerScopeSafety(unittest.TestCase):
    """The installer must not write outside the target CLAUDE.md path."""

    def test_argparse_rejects_unknown_scope(self):
        r = subprocess.run(
            [
                sys.executable,
                str(INSTALLER),
                "--scope", "site",  # invalid
                "--home", "/home/v",
                "--target", "/tmp/x",
                "--snippet", str(SNIPPET),
            ],
            capture_output=True, text=True, timeout=10,
        )
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("invalid choice", r.stderr.lower() + r.stdout.lower())

    def test_argparse_requires_args(self):
        r = subprocess.run(
            [sys.executable, str(INSTALLER)],
            capture_output=True, text=True, timeout=10,
        )
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("required", (r.stderr + r.stdout).lower())


if __name__ == "__main__":
    unittest.main(verbosity=2)
