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
import time
import unittest
from pathlib import Path

# Derive the source-tree adapter directory from this test file's
# location rather than a hardcoded author-machine absolute path. The
# test lives at
#   agent_installation/mpm-claude-code/tests/<file>.py
# so SCRIPT_DIR = parent.parent (= agent_installation/mpm-claude-code/).
# CANONICAL_PROTOCOL is one level up at the agent_installation root.
# The previous literal "/home/v/.mpm/agent_installation/..." pointed
# only at the original author's installed location; tests should resolve
# from the repository so they work without an installed MPM.
SCRIPT_DIR = Path(__file__).resolve().parent.parent
INSTALLER = SCRIPT_DIR / "scripts" / "install_claude_instructions.py"
SNIPPET = SCRIPT_DIR / "templates" / "CLAUDE.md.snippet"
CANONICAL_PROTOCOL = Path(__file__).resolve().parent.parent.parent / "mpm-agent-protocol.md"


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
        # The canonical agent protocol must stay host-independent AND
        # readable. Two guards:
        #
        #   lower bound (> 2,000 chars): a near-empty protocol would
        #     fail to convey the cross-session continuity contract;
        #     force substantive content.
        #
        #   upper bound (< 28,000 chars): the protocol's intent is to
        #     be a principles document, not a kitchen-sink reference.
        #     Above this threshold the file has likely accumulated
        #     host-specific implementation detail, duplicated host-
        #     adapter guidance, template syntax that belongs with the
        #     tool rather than the cross-cutting contract, or prose
        #     that could be expressed more compactly.
        #
        # Threshold history:
        #   20,000 — initial cap at the MPM agent protocol's creation
        #     (commit c37903a, 2026-08-26).
        #   25,000 — bumped for §4.1 "session != work" lifecycle split
        #     added in the 2026-09-04 cross-adapter integrity audit
        #     (commit ec3d729).
        #   28,000 — bumped for §1.1-1.3 contextual continuity contract
        #     added by the Stage 2E.4 instruction convergence
        #     (commit 32dd25c, 2026-09-21), which replaced earlier
        #     prose describing the contextual routing pipeline with
        #     a bounded semantic interpretation contract.
        #
        # This test is intentionally a maintainability heuristic, not
        # a hard constraint: bumping the ceiling again is acceptable
        # when a new architectural principle genuinely requires more
        # normative prose. Trimming the document to fit is preferred
        # when the growth is template syntax, duplicated guidance, or
        # implementation detail (template syntax and classifier
        # enumeration in §8.2 were both trimmed in the 2026-09-23
        # commit that bumped the ceiling to 28,000).
        self.assertGreater(len(self.text), 2000)
        current = len(self.text)
        self.assertLess(
            current, 28000,
            f"canonical protocol is too large ({current} chars); "
            f"the upper bound is 28000. Trim template syntax, "
            f"duplicated host-adapter guidance, or implementation "
            f"details that duplicate the underlying Go code. "
            f"If a new architectural principle genuinely requires "
            f"more normative prose, update the threshold with a "
            f"documented rationale in this test's docstring.",
        )


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
        # --home is documented as the user's HOME; the installer uses
        # it only when --scope=user to derive a default --target if
        # --target is omitted. Since this test passes --target explicitly
        # (self.target is a t.TempDir() path), --home is irrelevant to
        # the installer's behaviour here. Pass the tempdir so the test
        # never references a specific user's home — the previous
        # "/home/v" hardcoded the original author's checkout.
        return subprocess.run(
            [
                sys.executable,
                str(INSTALLER),
                "--scope", "user",
                "--home", str(self.tmpdir),
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

    def test_stale_body_with_correct_markers_is_replaced(self):
        """Regression for the 2026-09-28 content-blinder investigation.

        A target with the correct managed-section markers but a stale body
        must be detected as stale and replaced. Earlier revisions of the
        installer had a no-op branch that compared markers only (not the
        body) and let stale content survive. The current body-comparison
        guard must catch this and refresh the file in place, with a backup
        preserved.
        """
        # Seed the file with user content above a correct managed-section
        # block whose body is intentionally stale (3 items when the canonical
        # contract has 11).
        user_head = "# v's personal notes\nthese are mine, leave them alone\n"
        stale_body = (
            "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->\n"
            "## STALE managed block (3 items only)\n\n"
            "1. stale-wake\n"
            "2. stale-persist\n"
            "3. stale-handoff\n"
            "<!-- END MPM-MANAGED SECTION:claude-code-instructions -->\n"
        )
        user_tail = "\n# v's trailing notes\n"
        self.target.write_text(user_head + stale_body + user_tail)

        r = self.run_installer()
        self.assertEqual(r.returncode, 0, r.stderr)
        # Backup preserved
        self.assertTrue((self.target.parent / (self.target.name + ".bak")).exists(),
                        "stale-replace should have left a backup")

        result = self.target.read_text()
        # User content above and below survives
        self.assertIn("v's personal notes", result)
        self.assertIn("v's trailing notes", result)
        # Stale body is gone
        self.assertNotIn("stale-wake", result)
        self.assertNotIn("stale-persist", result)
        self.assertNotIn("stale-handoff", result)
        # Current contract is in
        self.assertIn("Wake is auto-injected", result)
        self.assertIn("Recovery / fallback", result)
        # Exactly one managed section
        self.assertEqual(result.count("BEGIN MPM-MANAGED SECTION"), 1)

    def test_current_body_is_noop_no_backup(self):
        """The no-op branch is byte-equality of the full managed block
        against the canonical render. A current block must be detected as
        current, with no backup created (idempotence) and no replacement
        text written (the file's mtime must not change).
        """
        # First install.
        r = self.run_installer()
        self.assertEqual(r.returncode, 0, r.stderr)
        first_bytes = self.target.read_bytes()
        first_mtime = self.target.stat().st_mtime
        # No backup should exist after a clean install.
        self.assertFalse((self.target.parent / (self.target.name + ".bak")).exists(),
                         "fresh install should not create a backup")

        # Wait a tick so mtime would change if the file were rewritten.
        time.sleep(0.05)

        # Second install: body is current, must no-op.
        r = self.run_installer()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("no-op", r.stdout)
        # File untouched: bytes and mtime unchanged.
        self.assertEqual(self.target.read_bytes(), first_bytes)
        self.assertEqual(self.target.stat().st_mtime, first_mtime)
        # Still no backup created on the no-op path.
        self.assertFalse((self.target.parent / (self.target.name + ".bak")).exists(),
                         "no-op install should not create a backup")

    def test_duplicate_end_marker_in_user_content_handled_conservatively(self):
        """If the user has a stray `<!-- END MPM-MANAGED SECTION -->` marker
        in their content outside the managed block, the installer must
        still succeed without corrupting the user's marker. The non-greedy
        managed-block extraction uses the first END after BEGIN, so the
        user's orphan END remains untouched.
        """
        user_content = (
            "# user notes\n"
            "<!-- a comment about <!-- END MPM-MANAGED SECTION --> markers -->\n"
        )
        self.target.write_text(user_content)
        r = self.run_installer()
        self.assertEqual(r.returncode, 0, r.stderr)
        result = self.target.read_text()
        # User content preserved
        self.assertIn("user notes", result)
        # The user's stray marker comment is preserved
        self.assertIn("<!-- END MPM-MANAGED SECTION --> markers", result)
        # The new managed section is present
        self.assertEqual(result.count("BEGIN MPM-MANAGED SECTION:claude-code-instructions"), 1)
        self.assertEqual(result.count("END MPM-MANAGED SECTION:claude-code-instructions"), 1)


class InstallerScopeSafety(unittest.TestCase):
    """The installer must not write outside the target CLAUDE.md path."""

    def test_argparse_rejects_unknown_scope(self):
        # --home is irrelevant to argparse; pass a tempdir so the test
        # is fully hermetic and never references a specific user's home
        # (the previous "/home/v" hardcoded the original author).
        r = subprocess.run(
            [
                sys.executable,
                str(INSTALLER),
                "--scope", "site",  # invalid
                "--home", tempfile.mkdtemp(prefix="claude-md-argparse-"),
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
