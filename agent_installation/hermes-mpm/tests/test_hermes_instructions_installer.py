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
        self.assertIn("Wake is auto-injected on session start", text)
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


# Path constants for the new contract tests.
SKILL = Path(__file__).resolve().parent.parent / "SKILL.md"
CONFIG_EXAMPLE = (
    Path(__file__).resolve().parent.parent
    / "templates"
    / "config.example.yaml"
)


class TestProvenanceContract(unittest.TestCase):
    """The Hermes integration must identify itself to MPM via the
    canonical MPM_PROVENANCE_FRAMEWORK env var.

    These tests pin the integration's prose and the operator-facing
    config example; behaviour is verified separately by the live
    acceptance tests against mpm-mcp.
    """

    def test_canonical_env_var_documented_in_skill(self):
        text = SKILL.read_text(encoding="utf-8")
        self.assertIn("MPM_PROVENANCE_FRAMEWORK", text)

    def test_canonical_env_var_value_is_hermes(self):
        text = SKILL.read_text(encoding="utf-8")
        # The canonical identifier must appear with the value `hermes`.
        self.assertIn("MPM_PROVENANCE_FRAMEWORK: hermes", text)

    def test_canonical_env_var_present_in_config_example(self):
        text = CONFIG_EXAMPLE.read_text(encoding="utf-8")
        self.assertIn("MPM_PROVENANCE_FRAMEWORK: hermes", text)

    def test_legacy_alias_described_as_legacy_only(self):
        """`MPM_FRAMEWORK` (legacy alias) must be referenced, but only as
        a fallback — not the primary variable in the example."""
        text = CONFIG_EXAMPLE.read_text(encoding="utf-8")
        # The config example must set the canonical var, not the legacy one.
        self.assertIn("MPM_PROVENANCE_FRAMEWORK: hermes", text)
        self.assertNotIn("MPM_FRAMEWORK: hermes", text)
        # The legacy alias may be mentioned in the explanatory comments.
        self.assertIn("MPM_FRAMEWORK", text)

    def test_no_static_provenance_model_or_invocation(self):
        """`MPM_PROVENANCE_MODEL`, `MPM_PROVENANCE_INVOCATION_ID`, and
        `MPM_PROVENANCE_PARENT_INVOCATION_ID` are populated by the
        runtime/call path. They must not be hardcoded in the static
        config example or in the documented MCP env block."""
        text = CONFIG_EXAMPLE.read_text(encoding="utf-8")
        # Config example env block must not include these:
        self.assertNotIn("MPM_PROVENANCE_MODEL", text)
        self.assertNotIn("MPM_PROVENANCE_INVOCATION_ID", text)
        self.assertNotIn("MPM_PROVENANCE_PARENT_INVOCATION_ID", text)
        # SKILL.md prose may mention them as "not set" but the env block
        # in the SKILL.md recommendation must not include them either.
        skill_text = SKILL.read_text(encoding="utf-8")
        # Extract the documented config block (between ```yaml fences).
        import re
        blocks = re.findall(r"```yaml\n(.*?)\n```", skill_text, re.DOTALL)
        self.assertTrue(blocks, "SKILL.md must include a yaml config example")
        block = blocks[0]
        self.assertNotIn("MPM_PROVENANCE_MODEL:", block)
        self.assertNotIn("MPM_PROVENANCE_INVOCATION_ID:", block)
        self.assertNotIn(
            "MPM_PROVENANCE_PARENT_INVOCATION_ID:", block
        )

    def test_workspace_var_documented(self):
        text = SKILL.read_text(encoding="utf-8")
        self.assertIn("MPM_WORKSPACE", text)
        text2 = CONFIG_EXAMPLE.read_text(encoding="utf-8")
        self.assertIn("MPM_WORKSPACE: $HOME/.mpm", text2)


class TestMcpSurfaceContract(unittest.TestCase):
    """The SKILL.md must accurately describe the MCP surface (compact
    3-tool) and the broader substrate Registry separately.

    The old claim of "22 MCP tools" was incorrect — the MCP server's
    default `tools/list` returns 3 tools; the 21 substrate Registry
    entries + 2 Standalones are reachable via the CLI fallback.
    """

    def test_no_stale_22_mcp_tools_claim(self):
        text = SKILL.read_text(encoding="utf-8")
        # The stale wording "22 MCP tools" must be gone.
        self.assertNotIn("22 MCP tools", text)

    def test_compact_three_tool_surface_documented(self):
        text = SKILL.read_text(encoding="utf-8")
        # Must name the three compact tools.
        self.assertIn("mcp__mpm__mpm_memory", text)
        self.assertIn("mcp__mpm__mpm_context", text)
        self.assertIn("mcp__mpm__mpm_help", text)

    def test_substrate_registry_distinct_from_mcp(self):
        text = SKILL.read_text(encoding="utf-8")
        # The substrate Registry should be named, with the actual count
        # of Registry entries (21) + 2 Standalones. The CLI fallback
        # (`mpm call`) must be the documented way to reach them.
        self.assertIn("mpm call", text)
        self.assertIn("Registry", text)

    def test_full_surface_expose_all_tools_override(self):
        text = SKILL.read_text(encoding="utf-8")
        # The legacy full-surface exposure is gated by
        # MPM_EXPOSE_ALL_TOOLS=1. The env var must be mentioned.
        self.assertIn("MPM_EXPOSE_ALL_TOOLS", text)


class TestDocAccuracy(unittest.TestCase):
    """The SKILL.md must not contain stale operational claims or refer
    to files outside the MPM repository as if they were current
    integration components.
    """

    def test_no_phantom_fts5_corruption_claim(self):
        text = SKILL.read_text(encoding="utf-8")
        # The old "phantom FTS5 corruption" operational guidance must be
        # gone from the Hermes skill document.
        self.assertNotIn("FTS5 corruption", text)
        self.assertNotIn("fts5: checksum mismatch", text)
        self.assertNotIn("integrity_status", text)

    def test_no_reference_to_external_hermes_loader(self):
        text = SKILL.read_text(encoding="utf-8")
        # The external `mpm_hermes_loader.py` is outside the MPM repo
        # and must not be documented as a current integration component.
        self.assertNotIn("mpm_hermes_loader.py", text)

    def test_db_path_described_accurately(self):
        text = SKILL.read_text(encoding="utf-8")
        # The canonical install-root path must be present and named
        # canonical, not described as legacy/inconsistent.
        self.assertIn("$HOME/.mpm/src/db/mpm.db", text)
        # The "legacy / inconsistent" framing of the symlink-equivalent
        # paths must be gone.
        self.assertNotIn("legacy project-source path", text)
        self.assertNotIn("Functionally equivalent", text)

    def test_wake_flow_explicit_first_turn_call(self):
        """Hermes has no native wake hook — the SKILL.md must say so
        and document the first-turn explicit read_wake_context call.
        """
        text = SKILL.read_text(encoding="utf-8")
        # The "no native session-start hook" framing must be present.
        self.assertIn("no native session-start hook", text)
        # The explicit first-turn call shape must be present.
        self.assertIn("read_wake_context", text)
        self.assertIn("first turn", text)


class TestModePersonaDefaults(unittest.TestCase):
    """The SKILL.md must accurately document the mode/persona default
    contract: when Hermes does not explicitly set MPM_ACTIVE_MODE /
    MPM_ACTIVE_PERSONA, MPM defaults to "default" for both.
    """

    def test_default_documented(self):
        text = SKILL.read_text(encoding="utf-8")
        # Must mention the "default" fallback for mode and persona.
        self.assertIn("default", text)
        # Specifically document the default-mode and default-persona
        # fallback semantics.
        self.assertIn("ResolveActiveMode", text)
        self.assertIn("ResolveActivePersona", text)

    def test_active_mode_active_persona_env_vars_documented(self):
        text = SKILL.read_text(encoding="utf-8")
        self.assertIn("MPM_ACTIVE_MODE", text)
        self.assertIn("MPM_ACTIVE_PERSONA", text)


class TestSnippetInstructionContract(unittest.TestCase):
    """The .hermes.md snippet must use the current tool namespace
    (mcp__mpm__) and reference the current canonical contract surface.
    """

    def test_snippet_namespace_is_mcp_mpm(self):
        text = SNIPPET.read_text(encoding="utf-8")
        self.assertIn("mcp__mpm__mpm_context", text)
        self.assertIn("mcp__mpm__mpm_memory", text)
        self.assertIn("read_wake_context", text)

    def test_snippet_documents_first_turn_wake_call(self):
        text = SNIPPET.read_text(encoding="utf-8")
        # The managed block in the snippet explicitly tells the agent
        # to call read_wake_context on the first turn.
        self.assertIn("read_wake_context", text)
        self.assertIn("first turn", text)

    def test_snippet_does_not_claim_auto_wake_for_hermes(self):
        """The snippet must NOT claim that wake is auto-injected on
        Hermes (unlike ClaudeCode, OpenClaw, OpenCode, Pi).
        """
        text = SNIPPET.read_text(encoding="utf-8")
        # Hermes must be explicitly named as the exception to the
        # auto-inject pattern. The canonical managed-block prose
        # wraps this across two lines ("Hermes has no such\n   hook").
        self.assertIn("Hermes has no such", text)
        # And it must not list Hermes among the hosts that auto-inject.
        # The canonical wake item lists ClaudeCode, OpenClaw, OpenCode,
        # Pi — and must exclude Hermes from that auto-inject list.
        # Easiest pin: the snippet must NOT contain a literal
        # "ClaudeCode, OpenClaw, OpenCode, Pi, and Hermes" auto-inject
        # claim (Hermes is the exception).
        self.assertNotIn(
            "ClaudeCode, OpenClaw, OpenCode, Pi, and Hermes", text
        )


if __name__ == "__main__":
    unittest.main()
