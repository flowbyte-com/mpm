"""
test_install_openclaw_instructions.py — Regression coverage for the
SOUL.md managed-block installer for OpenClaw.

Pins the contract documented at the top of
scripts/install_openclaw_instructions.py:

  * Fresh target: create with snippet content as-is.
  * Existing target without managed section: insert at end.
  * Existing target with managed section: refresh only that section;
    persona / user content outside the markers preserved verbatim.
  * Idempotent: a second run with the same snippet leaves the file
    byte-stable outside the managed section.
  * Stale managed section is refreshed to the current snippet.
  * Empty (but valid) target: receives the managed section.
  * Mismatched markers fail closed without modifying the file.
  * Uninstall removes only the managed section, leaving user content
    intact.

All tests are hermetic. They construct an isolated HOME with a
synthesised openclaw.json (no live OpenClaw state touched).
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

INSTALLER = Path(__file__).resolve().parent.parent / "scripts" / "install_openclaw_instructions.py"
SNIPPET = Path(__file__).resolve().parent.parent / "templates" / "SOUL.md.snippet"


def _run(args: list[str], home: Path, openclaw_config: dict | None,
         target_arg: str | None = None) -> subprocess.CompletedProcess:
    """Run the installer with a hermetic HOME."""
    if openclaw_config is not None:
        config_dir = home / ".openclaw"
        config_dir.mkdir(parents=True, exist_ok=True)
        (config_dir / "openclaw.json").write_text(
            json.dumps(openclaw_config), encoding="utf-8",
        )
    args = list(args)
    if target_arg:
        args += ["--target", target_arg]
    return subprocess.run(
        [sys.executable, str(INSTALLER), "--home", str(home), *args],
        capture_output=True, text=True, timeout=30,
    )


def _stage_snippet(tmp: Path) -> Path:
    """Write a minimal snippet into a temp file. Mirrors the renderer
    output: header + canonical block + footer, no outer section markers
    (those are added by the installer)."""
    snippet = tmp / "SOUL.md.snippet"
    snippet.write_text(
        "<!-- source: test snippet -->\n"
        "\n"
        "## MPM behavioural contract\n"
        "\n"
        "Test content A.\n",
        encoding="utf-8",
    )
    return snippet


class _Base(unittest.TestCase):
    def setUp(self):
        self._tmp = Path(tempfile.mkdtemp(prefix="mpm-openclaw-install-"))
        self.addCleanup(self._cleanup)
        self.home = self._tmp / "home"
        self.home.mkdir()
        self.snippet = _stage_snippet(self._tmp)
        self.config = {
            "agents": {
                "defaults": {"workspace": str(self.home / ".openclaw/workspace")},
                "entries": {
                    "main": {"workspace": str(self.home / ".openclaw/workspace")},
                },
            },
        }
        self.target = self.home / ".openclaw/workspace/SOUL.md"

    def _cleanup(self):
        shutil.rmtree(self._tmp, ignore_errors=True)


class FreshInstall(_Base):
    def test_fresh_target_receives_snippet(self):
        result = _run(["--snippet", str(self.snippet)], self.home, self.config)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.target.exists())
        text = self.target.read_text(encoding="utf-8")
        self.assertIn("Test content A.", text)
        self.assertIn("<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->", text)
        self.assertIn("<!-- END MPM-MANAGED SECTION:openclaw-instructions -->", text)

    def test_fresh_target_has_exactly_one_block(self):
        _run(["--snippet", str(self.snippet)], self.home, self.config)
        text = self.target.read_text(encoding="utf-8")
        self.assertEqual(
            text.count("<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->"),
            1,
        )
        self.assertEqual(
            text.count("<!-- END MPM-MANAGED SECTION:openclaw-instructions -->"),
            1,
        )


class PreservesPersona(_Base):
    def test_existing_persona_content_preserved_verbatim(self):
        # Pre-populate with persona-like content
        existing = (
            "# 808 Persona\n"
            "\n"
            "## Core Truths\n"
            "\n"
            "Be genuinely helpful.\n"
        )
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text(existing, encoding="utf-8")

        result = _run(["--snippet", str(self.snippet)], self.home, self.config)
        self.assertEqual(result.returncode, 0, result.stderr)

        text = self.target.read_text(encoding="utf-8")
        # Persona content preserved verbatim, above the managed block
        self.assertIn("Be genuinely helpful.", text)
        # The block is now present below the persona
        self.assertIn("<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->", text)
        self.assertIn("Test content A.", text)

    def test_user_content_after_block_preserved(self):
        # Content both before AND after the block
        existing = (
            "USER_NOTES_BEFORE\n"
            "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->\n"
            "old managed block\n"
            "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->\n"
            "USER_NOTES_AFTER\n"
        )
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text(existing, encoding="utf-8")

        result = _run(["--snippet", str(self.snippet)], self.home, self.config)
        self.assertEqual(result.returncode, 0, result.stderr)

        text = self.target.read_text(encoding="utf-8")
        self.assertIn("USER_NOTES_BEFORE", text)
        self.assertIn("USER_NOTES_AFTER", text)
        self.assertIn("Test content A.", text)
        self.assertNotIn("old managed block", text)


class Idempotency(_Base):
    def test_second_run_is_byte_stable_outside_managed(self):
        _run(["--snippet", str(self.snippet)], self.home, self.config)
        first = self.target.read_text(encoding="utf-8")
        r2 = _run(["--snippet", str(self.snippet)], self.home, self.config)
        self.assertEqual(r2.returncode, 0, r2.stderr)
        second = self.target.read_text(encoding="utf-8")
        self.assertEqual(first, second,
            "second install altered file bytes outside the managed section")


class RefreshStale(_Base):
    def test_stale_block_replaced_with_current(self):
        # Pre-populate with stale managed content
        existing = (
            "USER_NOTES\n"
            "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->\n"
            "<!-- BEGIN MPM MANAGED BLOCK -->\n"
            "\n"
            "## STALE OLD CONTENT\n"
            "<!-- END MPM MANAGED BLOCK -->\n"
            "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->\n"
        )
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text(existing, encoding="utf-8")

        result = _run(["--snippet", str(self.snippet)], self.home, self.config)
        self.assertEqual(result.returncode, 0, result.stderr)
        text = self.target.read_text(encoding="utf-8")
        self.assertIn("Test content A.", text)
        self.assertNotIn("STALE OLD CONTENT", text)
        self.assertIn("USER_NOTES", text)


class EmptyTarget(_Base):
    def test_empty_target_receives_block(self):
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text("", encoding="utf-8")
        result = _run(["--snippet", str(self.snippet)], self.home, self.config)
        self.assertEqual(result.returncode, 0, result.stderr)
        text = self.target.read_text(encoding="utf-8")
        self.assertIn("<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->", text)
        self.assertIn("Test content A.", text)


class CanonicalParity(_Base):
    def test_installed_block_matches_snippet_managed_section(self):
        # The installer wraps the renderer-produced snippet body in
        # outer section markers. The installed managed section must
        # contain the snippet body verbatim between its banner and
        # footer lines.
        _run(["--snippet", str(self.snippet)], self.home, self.config)
        text = self.target.read_text(encoding="utf-8")
        snippet_text = self.snippet.read_text(encoding="utf-8")

        begin = "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->"
        end = "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->"
        installed_begin = text.index(begin)
        installed_end = text.index(end) + len(end)
        installed = text[installed_begin:installed_end]

        # The snippet body must appear verbatim between the installer's
        # banner (4 comment lines after the BEGIN marker) and the footer.
        # The banner's source line identifies the snippet path; the
        # remaining 4 banner lines + the snippet body + footer compose
        # the installed section.
        snippet_lines = snippet_text.rstrip().splitlines()
        self.assertTrue(
            any(snippet_lines[0].strip() in line for line in
                installed.splitlines()[:8]),
            f"snippet body not found in installed section:\n{installed}",
        )


class MismatchedMarkersFailClosed(_Base):
    def test_mismatched_markers_do_not_modify_file(self):
        # Mismatched: 1 BEGIN, no END
        existing = (
            "USER_NOTES\n"
            "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->\n"
            "partial block without end marker\n"
        )
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text(existing, encoding="utf-8")

        result = _run(["--snippet", str(self.snippet)], self.home, self.config)
        self.assertNotEqual(result.returncode, 0,
            "installer must fail closed on mismatched markers")
        # File unchanged
        self.assertEqual(
            self.target.read_text(encoding="utf-8"), existing,
            "installer must not modify file when markers mismatch",
        )


class UninstallRemovesOnlyManagedSection(_Base):
    def test_uninstall_leaves_user_content(self):
        existing = (
            "USER_NOTES\n"
            "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->\n"
            "<!-- BEGIN MPM MANAGED BLOCK -->\n"
            "managed text\n"
            "<!-- END MPM MANAGED BLOCK -->\n"
            "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->\n"
            "MORE_USER_NOTES\n"
        )
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text(existing, encoding="utf-8")

        result = _run(["--uninstall", "--snippet", str(self.snippet)],
                      self.home, self.config)
        self.assertEqual(result.returncode, 0, result.stderr)
        text = self.target.read_text(encoding="utf-8")
        self.assertNotIn("<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->", text)
        self.assertNotIn("managed text", text)
        self.assertIn("USER_NOTES", text)
        self.assertIn("MORE_USER_NOTES", text)

    def test_uninstall_no_block_is_noop(self):
        self.target.parent.mkdir(parents=True, exist_ok=True)
        self.target.write_text("USER_NOTES\n", encoding="utf-8")
        result = _run(["--uninstall", "--snippet", str(self.snippet)],
                      self.home, self.config)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            self.target.read_text(encoding="utf-8"), "USER_NOTES\n",
        )


class TargetResolution(_Base):
    def test_target_resolved_from_openclaw_json(self):
        # Move the workspace to a different path; installer should
        # discover and write to the new SOUL.md location.
        alt_workspace = self.home / "alt/workspace"
        alt_workspace.mkdir(parents=True, exist_ok=True)
        config = {
            "agents": {
                "entries": {
                    "main": {"workspace": str(alt_workspace)},
                },
            },
        }
        result = _run(["--snippet", str(self.snippet)], self.home, config)
        self.assertEqual(result.returncode, 0, result.stderr)
        alt_target = alt_workspace / "SOUL.md"
        self.assertTrue(alt_target.exists())
        self.assertIn("Test content A.", alt_target.read_text(encoding="utf-8"))

    def test_explicit_target_overrides_discovery(self):
        explicit = self.home / "explicit/SOUL.md"
        explicit.parent.mkdir(parents=True, exist_ok=True)
        explicit.write_text("PRE-EXISTING\n", encoding="utf-8")
        result = _run(["--snippet", str(self.snippet)], self.home,
                      self.config, target_arg=str(explicit))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(explicit.exists())
        self.assertIn("Test content A.", explicit.read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
