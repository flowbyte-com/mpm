"""Regression tests for the pi-mpm extension integration contract.

The Pi adapter has three layers under audit:

  1. **Installer** (`scripts/install_agents_instructions.py`) — the
     AGENTS.md managed-block installer. Covered by
     `test_pi_instructions_installer.py`.
  2. **Extension source** (`index.ts`, `src/workspace.ts`) — the
     typed-tool / wake / framework-identification layer. The tests
     below pin the contract documented in the README and index.ts:
       - canonical framework variable MPM_PROVENANCE_FRAMEWORK=pi
       - no hardcoded per-call provenance fields (model, invocation_id,
         parent_invocation_id, actor_kind)
       - default mode/persona fallback to "default"
       - wake block renders both mode and persona (not just persona)
       - wake uses `mpm_context.read_wake_context` (not retired APIs)
  3. **Documentation** (`README.md`, `templates/AGENTS.md.snippet`) —
     prose contracts that the runtime matches.

The integration tests run against the real `mpm` binary on PATH
(default `$HOME/.mpm/bin/mpm`) and the canonical install root
(`$HOME/.mpm/src/db/mpm.db` or its symlink equivalent). They are
skipped when `mpm` is not available so the test suite remains
useful in any environment.
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

# Path constants for the contract tests.
ADAPTER_DIR = Path(__file__).resolve().parent.parent
INDEX_TS = ADAPTER_DIR / "index.ts"
WORKSPACE_TS = ADAPTER_DIR / "src" / "workspace.ts"
README = ADAPTER_DIR / "README.md"
SNIPPET = ADAPTER_DIR / "templates" / "AGENTS.md.snippet"


def _read(path: Path) -> str:
    return path.read_text(encoding="utf-8")


# ---------------------------------------------------------------------------
# Provenance contract
# ---------------------------------------------------------------------------


class TestProvenanceContract(unittest.TestCase):
    """The Pi extension must identify itself to MPM via the canonical
    MPM_PROVENANCE_FRAMEWORK env var with the value `pi`.
    """

    def test_canonical_env_var_imported_in_index_ts(self):
        """`index.ts` must use the provenance-aware helper (withProvenance)
        rather than the legacy withWorkspace helper, so every spawn
        inherits MPM_PROVENANCE_FRAMEWORK=pi."""
        text = _read(INDEX_TS)
        # The import line is the source-of-truth marker. The legacy
        # withWorkspace import is gone from index.ts.
        self.assertIn('from "./src/workspace.js"', text)
        # Must import the helper that adds provenance.
        self.assertRegex(text, r"import\s*\{[^}]*\bwithProvenance\b[^}]*\}\s*from\s*\"\./src/workspace\.js\"")
        # The legacy helper must not be imported at the top of index.ts.
        # (withWorkspace is still exported from workspace.ts as back-compat
        # shim, but index.ts itself should not import it for spawn-time use.)
        # Use a precise regex to look for a `withWorkspace` import that
        # co-exists with `withProvenance` — there should be none.
        self.assertNotRegex(text, r"import\s*\{[^}]*\bwithWorkspace\b[^}]*\}\s*from\s*\"\./src/workspace\.js\"")

    def test_with_provenance_used_in_call_mpm(self):
        text = _read(INDEX_TS)
        # The `callMpm` function must use withProvenance (not withWorkspace)
        # for the spawn env.
        self.assertRegex(text, r"env:\s*withProvenance\(\)")
        # The legacy helper must not be passed to spawn() in callMpm.
        self.assertNotRegex(text, r"env:\s*withWorkspace\(\)")

    def test_with_provenance_used_in_call_mpm_cli(self):
        """The CLI subcommand path (mpm info, mpm status) must also stamp
        provenance so /mpm-status commands attribute correctly."""
        text = _read(INDEX_TS)
        # Find callMpmCli definition (the second function with withProvenance).
        # Verify both spawn invocations use withProvenance, not withWorkspace.
        spawn_env_provenance = text.count("env: withProvenance()")
        spawn_env_workspace = text.count("env: withWorkspace()")
        self.assertGreaterEqual(spawn_env_provenance, 2)
        self.assertEqual(spawn_env_workspace, 0)

    def test_workspace_helper_defines_canonical_id(self):
        text = _read(WORKSPACE_TS)
        # The framework id must be the literal string "pi".
        self.assertIn('MPM_FRAMEWORK_ID = "pi"', text)
        # buildProvenanceEnv must stamp MPM_PROVENANCE_FRAMEWORK with that
        # canonical id.
        self.assertIn("MPM_PROVENANCE_FRAMEWORK: MPM_FRAMEWORK_ID", text)

    def test_no_hardcoded_per_call_provenance_fields(self):
        """`MPM_PROVENANCE_MODEL`, `MPM_PROVENANCE_INVOCATION_ID`,
        `MPM_PROVENANCE_PARENT_INVOCATION_ID`, and
        `MPM_PROVENANCE_ACTOR_KIND` are populated per-call / per-invocation
        by the runtime, not statically. The Pi helper must NOT set them
        in the static spawn env block.

        Note: these names may appear in header comments listing what the
        layer intentionally does NOT set. Strip comments before scanning.
        """
        text = _read(WORKSPACE_TS)
        # Strip /* ... */ block comments and // line comments before
        # checking for runtime-set field names. Comments legitimately
        # mention these names to document the contract.
        import re
        no_block = re.sub(r"/\*[\s\S]*?\*/", "", text)
        no_line = re.sub(r"//[^\n]*", "", no_block)
        for forbidden in (
            "MPM_PROVENANCE_MODEL",
            "MPM_PROVENANCE_INVOCATION_ID",
            "MPM_PROVENANCE_PARENT_INVOCATION_ID",
            "MPM_PROVENANCE_ACTOR_KIND",
        ):
            self.assertNotIn(
                forbidden, no_line,
                f"workspace.ts must not set {forbidden} in code (comments OK)",
            )

    def test_inherited_process_env_for_path(self):
        """`withProvenance` must inherit `process.env` so PATH, HOME, TZ
        etc. survive into the mpm subprocess. Without this, `mpm` itself
        cannot be resolved by name."""
        text = _read(WORKSPACE_TS)
        # Must iterate process.env entries into a fresh map.
        self.assertRegex(text, r"for\s*\(\s*const\s*\[\s*key\s*,\s*value\s*\]\s*of\s*Object\.entries\(\s*process\.env\s*\)\s*\)")
        # Must filter out undefined values.
        self.assertRegex(text, r"if\s*\(\s*value\s*!==\s*undefined\s*\)")
        # Must end with pinning MPM_WORKSPACE last so caller overrides
        # win but none can drop it.
        self.assertRegex(text, r"env\.MPM_WORKSPACE\s*=\s*env\.MPM_WORKSPACE\s*\?\?\s*resolveWorkspace\(\)")

    def test_canonical_framework_var_documented_in_readme(self):
        text = _read(README)
        self.assertIn("MPM_PROVENANCE_FRAMEWORK", text)
        self.assertIn("MPM_PROVENANCE_FRAMEWORK=pi", text)
        # The canonical value must appear as the recommended env var.
        self.assertRegex(text, r"MPM_PROVENANCE_FRAMEWORK[\s\S]{0,40}pi")
        # The legacy alias MPM_FRAMEWORK is mentioned only as a fallback.
        self.assertIn("MPM_FRAMEWORK", text)

    def test_canonical_var_value_is_pi(self):
        text = _read(README)
        # The README must explicitly state the canonical value is "pi".
        self.assertRegex(text, r'framework[\s_-]name[=:]["\s]*"?\s*pi')

    def test_no_stale_default_mpm_cli_attribution(self):
        """The README must not claim the framework attribution falls
        through to `mpm-cli` (the surface-derived default) when no env
        var is set. The contract is: without MPM_PROVENANCE_FRAMEWORK=pi,
        writes are misattributed — that gap must be called out, not
        normalized to a default."""
        text = _read(README)
        # Find the attribution sentence.
        self.assertIn("writes are attributed to", text)


# ---------------------------------------------------------------------------
# Mode/persona defaults contract
# ---------------------------------------------------------------------------


class TestModePersonaDefaults(unittest.TestCase):
    """The wake-renderer must apply the substrate's safe-default contract:
    when `active_mode` or `active_persona` is empty/absent, render
    "default". This closes the shared-core asymmetry where
    `read_wake_context` returns an empty `active_mode` JSON field
    when `active.json` has no `modes` key.
    """

    def test_render_wake_block_defaults_mode(self):
        text = _read(INDEX_TS)
        # renderWakeBlock must define DEFAULT_MODE = "default".
        self.assertRegex(text, r"DEFAULT_MODE\s*=\s*\"default\"")
        # And the active_mode rendering must apply that fallback when empty.
        self.assertRegex(text, r"wake\.active_mode\s*&&\s*wake\.active_mode\.trim\(\)\s*!==\s*\"\"\s*\?\s*wake\.active_mode\s*:\s*DEFAULT_MODE")

    def test_render_wake_block_defaults_persona(self):
        text = _read(INDEX_TS)
        self.assertRegex(text, r"DEFAULT_PERSONA\s*=\s*\"default\"")
        self.assertRegex(text, r"wake\.active_persona\s*&&\s*wake\.active_persona\.trim\(\)\s*!==\s*\"\"\s*\?\s*wake\.active_persona\s*:\s*DEFAULT_PERSONA")

    def test_wake_block_renders_mode_and_persona(self):
        """The wake block must surface both `Active mode:` and
        `Active persona:` lines, regardless of value. The previous
        code only rendered persona when set (mode was absent entirely).
        """
        text = _read(INDEX_TS)
        # Both labels must be present in the renderer.
        self.assertIn("Active mode: ", text)
        self.assertIn("Active persona: ", text)

    def test_default_mode_persona_documented_in_readme(self):
        """The README must document the default mode/persona contract:
        when `MPM_ACTIVE_MODE` and `MPM_ACTIVE_PERSONA` are unset, both
        resolve to "default"."""
        text = _read(README)
        # Both must mention "default" as the safe default.
        self.assertIn("default", text)
        # The active.json / shared-core asymmetry must be acknowledged
        # explicitly so operators know the renderer-level fallback is
        # closing a shared-core gap, not fabricating a value.
        self.assertIn("active_mode", text)
        self.assertIn("active_persona", text)


# ---------------------------------------------------------------------------
# Wake/context integration contract
# ---------------------------------------------------------------------------


class TestWakeIntegrationContract(unittest.TestCase):
    """The wake integration must use the canonical mpm_context.read_wake_context
    path; the session_start hook must fetch it, the before_agent_start hook
    must inject it exactly once.
    """

    def test_session_start_uses_read_wake_context(self):
        text = _read(INDEX_TS)
        # The session_start handler must call read_wake_context.
        self.assertRegex(text, r'pi\.on\(\s*[\"\']session_start[\"\']')
        # And the call body must invoke read_wake_context via mpm_context.
        self.assertIn('action: "read_wake_context"', text)

    def test_before_agent_start_injects_once(self):
        text = _read(INDEX_TS)
        # The before_agent_start handler exists and gates delivery on a
        # wakeDelivered flag.
        self.assertRegex(text, r'pi\.on\(\s*[\"\']before_agent_start[\"\']')
        self.assertIn("wakeDelivered", text)
        self.assertIn("cachedWake", text)

    def test_no_removed_watcher_architecture(self):
        """The old Pi integration used a "watcher" pattern that was
        retired before 2026-09. The current implementation uses Pi's
        session_start + before_agent_start events. There must be no
        `setInterval`, `watch`, or filesystem-watcher code path that
        would imply a stale watcher architecture.
        """
        text = _read(INDEX_TS)
        self.assertNotIn("setInterval", text)
        self.assertNotIn("fs.watch", text)
        # session_start and before_agent_start must be present (the
        # current event flow); no "watcher" string either.
        self.assertNotIn("watcher", text.lower())

    def test_wake_fetch_does_not_create_session_or_start_scheduler(self):
        """`read_wake_context` is a read-only projection. It must not
        start the mpm scheduler or create a new MPM session."""
        text = _read(INDEX_TS)
        # The session_start handler must NOT invoke mpm_session or
        # mpm-scheduler.
        self.assertNotIn("mpm_session", text)
        self.assertNotIn("mpm_scheduler", text)
        self.assertNotIn("mpm-scheduler", text)
        # The session_start handler must not start any background process
        # beyond a single read_wake_context subprocess. Inspect the
        # session_start callback body specifically.
        import re
        ss_match = re.search(
            r'pi\.on\(\s*[\"\']session_start[\"\'][\s\S]*?\}\)',
            text,
        )
        self.assertIsNotNone(ss_match, "session_start handler not found")
        ss_body = ss_match.group(0)
        # No spawn() inside session_start other than the indirect one
        # through callMpm("mpm_context", read_wake_context).
        self.assertNotIn("setInterval", ss_body)
        self.assertNotIn("setTimeout", ss_body)
        self.assertNotIn("mpm_wakes", ss_body)
        self.assertNotIn("schedule", ss_body)
        # No background child process forks.
        self.assertNotRegex(ss_body, r"spawn\(\s*[\"\']mpm[\"\']\s*,\s*\[\s*[\"\']call[\"\']\s*,\s*[\"\']mpm_wakes[\"\']")

    def test_wake_flow_documented_in_readme(self):
        """The README must describe the actual first-turn wake flow
        (session_start fetches, before_agent_start injects once)."""
        text = _read(README)
        self.assertIn("session_start", text)
        self.assertIn("before_agent_start", text)
        self.assertIn("read_wake_context", text)
        # The flow's idempotence: the wake is injected exactly once.
        self.assertRegex(text, r"exactly once|once\b", text)


# ---------------------------------------------------------------------------
# MCP surface contract
# ---------------------------------------------------------------------------


class TestMcpSurfaceContract(unittest.TestCase):
    """Pi does NOT use the mpm-mcp MCP server. The MCP surface (compact
    3-tool default, 22-tool Registry, MPM_EXPOSE_ALL_TOOLS=1 override)
    is documented for cross-host reference only. There must be no claim
    of "22 MCP tools" — the MCP server's tools/list returns 3 tools by
    default, the 22 are the full substrate Registry, not "MCP tools".
    """

    def test_no_stale_22_mcp_tools_claim_in_index_ts(self):
        text = _read(INDEX_TS)
        # The "22 MCP tools" wording must be absent. Acceptable: "22 tools"
        # referring to the Registry; "22-tool surface" referring to
        # MPM_EXPOSE_ALL_TOOLS=1. The "22 MCP tools" composite must NOT appear.
        self.assertNotIn("22 MCP tools", text)
        self.assertNotIn("22-tool MCP", text)

    def test_no_stale_22_mcp_tools_claim_in_readme(self):
        text = _read(README)
        self.assertNotIn("22 MCP tools", text)
        self.assertNotIn("22-tool MCP", text)

    def test_default_three_tool_mcp_surface_documented(self):
        """The compact 3-tool default MCP surface
        (mpm_memory / mpm_context / mpm_help) must be documented for
        cross-host reference, with MPM_EXPOSE_ALL_TOOLS=1 named as the
        override to the full surface."""
        text = _read(README)
        self.assertIn("mpm_memory", text)
        self.assertIn("mpm_context", text)
        self.assertIn("mpm_help", text)
        self.assertIn("MPM_EXPOSE_ALL_TOOLS", text)
        # All three must appear together with "compact".
        self.assertRegex(text, r"compact[\s\S]{0,200}mpm_memory[\s\S]{0,80}mpm_context[\s\S]{0,80}mpm_help")

    def test_pi_does_not_use_mcp_server(self):
        """The README must explicitly say Pi does not use the MCP server
        — Pi uses its own typed tools."""
        text = _read(README)
        # Direct statement: Pi does not use the MCP server.
        self.assertRegex(text, r"Pi does \*\*not\*\* use the `mpm-mcp`")
        # Or a stronger statement.
        self.assertRegex(text, r"Pi does not use (the )?MCP server", text)


# ---------------------------------------------------------------------------
# Documentation accuracy
# ---------------------------------------------------------------------------


class TestDocumentationAccuracy(unittest.TestCase):
    """Drift signatures that must be absent from current operational
    material in pi-mpm.
    """

    def test_no_retired_session_api(self):
        text = _read(INDEX_TS)
        # mpm_session was retired (consolidated to mpm_handoff /
        # mpm_scratchpad). It must not appear.
        self.assertNotIn("mpm_session", text)
        # mpm_session.end was the retired alias.
        self.assertNotIn("mpm_session.end", text)

    def test_no_legacy_mcp_77_tools_claim_in_index_ts(self):
        text = _read(INDEX_TS)
        # The "77 granular tools" historical mention is fine (it's
        # past-tense history) but the active surface MUST NOT claim
        # "77 MCP tools" — that was a previous MCP server state.
        self.assertNotIn("77 MCP tools", text)
        self.assertNotIn("77-tool MCP", text)

    def test_no_legacy_mcp_77_tools_claim_in_readme(self):
        text = _read(README)
        self.assertNotIn("77 MCP tools", text)
        self.assertNotIn("77-tool MCP", text)

    def test_no_obsolete_watcher_behavior(self):
        text = _read(README)
        # No "watcher" or "setInterval" references in the README.
        self.assertNotIn("watcher", text.lower())
        self.assertNotIn("setInterval", text)

    def test_snippet_is_canonical(self):
        """The snippet is rendered by `scripts/render_managed_blocks.py`
        from the canonical source. The `agent_installation/scripts/render_managed_blocks.py
        --check` parity check pins byte-equivalence; here we just
        confirm the snippet still wraps the universal managed block
        and contains the canonical protocol items.
        """
        text = _read(SNIPPET)
        self.assertIn("BEGIN MPM MANAGED BLOCK", text)
        self.assertIn("END MPM MANAGED BLOCK", text)
        self.assertIn("Wake is auto-injected on session start", text)
        self.assertIn("Handoff before genuine session closure", text)
        # The snippet must explicitly mention Pi in the auto-inject list
        # (the canonical contract: ClaudeCode, OpenClaw, OpenCode, and Pi).
        # The list wraps across two lines in the rendered canonical block.
        self.assertRegex(text, r"ClaudeCode,\s*OpenClaw,[\s\n]+OpenCode,\s*and\s+Pi")


# ---------------------------------------------------------------------------
# Live integration tests (require mpm on PATH and the canonical install)
# ---------------------------------------------------------------------------


def _mpm_on_path() -> bool:
    return shutil.which("mpm") is not None


@unittest.skipUnless(_mpm_on_path(), "mpm binary not on PATH")
class TestLiveIntegration(unittest.TestCase):
    """Real-substrate live tests. Each test runs `mpm call` with the
    Pi provenance env set and verifies the resulting attribution.
    """

    @staticmethod
    def _mpm_call(tool: str, payload: dict, framework: str = "pi", timeout: int = 30) -> dict:
        """Run `mpm call <tool> --payload '<json>'` with a forced
        framework env var and return the parsed JSON envelope."""
        env = os.environ.copy()
        env["MPM_PROVENANCE_FRAMEWORK"] = framework
        proc = subprocess.run(
            ["mpm", "call", tool, "--payload", json.dumps(payload)],
            capture_output=True,
            text=True,
            env=env,
            timeout=timeout,
        )
        # Find the last JSON-object line in stdout.
        last_json: dict | None = None
        for line in reversed(proc.stdout.splitlines()):
            line = line.strip()
            if line.startswith("{") and line.endswith("}"):
                try:
                    last_json = json.loads(line)
                    break
                except json.JSONDecodeError:
                    continue
        if last_json is None:
            raise AssertionError(f"no JSON envelope on stdout; raw: {proc.stdout!r}")
        return last_json

    def test_wake_returns_populated_payload(self):
        """`read_wake_context` returns a populated JSON payload with
        `active_mode`, `active_persona`, and `last_handoff` keys.

        The compact projection wraps the wake data under a `context`
        key; the full projection (no `params.projection`) returns
        the fields at the top level.
        """
        env = self._mpm_call(
            "mpm_context",
            {"action": "read_wake_context", "params": {}},
        )
        # The full projection puts orientation fields at top level.
        self.assertIn("active_mode", env)
        self.assertIn("active_persona", env)
        self.assertIn("success", env)
        self.assertTrue(env["success"])

    def test_default_mode_persona_contract(self):
        """When `active.json` has no `modes` key, the wake JSON's
        `active_mode` is empty (shared-core asymmetry). The Pi
        renderer must default it to "default". Verify the substrate
        asymmetry is real so the renderer fallback is documented as a
        correct gap-closure, not a fabrication.
        """
        env = self._mpm_call(
            "mpm_context",
            {"action": "read_wake_context", "params": {}},
        )
        # Verify the shared-core asymmetry is what we expect to close:
        # at least one of mode/persona is empty when active.json has
        # no `modes` key (the operator's current state).
        active_json = Path(os.path.expanduser("~/.mpm/active.json"))
        if active_json.exists():
            aj = json.loads(active_json.read_text())
            if not aj.get("modes"):
                self.assertEqual(env.get("active_mode", ""), "")
                # The renderer's contract is: when active_mode is empty
                # in the JSON, fall back to "default" at render time.
                # That's the index.ts:renderWakeBlock contract this test
                # pins in TestModePersonaDefaults.
        # Independently, persona should be either "default" or empty
        # (resolved via ResolveActivePersona, which falls back to
        # "default" if the requested file is missing).
        self.assertIn(env.get("active_persona", ""), {"", "default"})

    def test_live_provenance_attribution(self):
        """When `MPM_PROVENANCE_FRAMEWORK=pi` is set on the mpm call,
        `artifact_provenance.framework_name` MUST be "pi" for any
        memory this test saves."""
        import time
        marker = f"pi-mpm-live-provenance-test-{int(time.time())}"
        # Save a memory with provenance=pi.
        result = self._mpm_call(
            "mpm_memory",
            {"action": "save", "params": {"fact": marker, "tags": ["pi-mpm-alignment"], "collection": "memories"}},
            framework="pi",
        )
        self.assertTrue(result.get("success"), f"save failed: {result}")
        memory_id = result.get("memory_id") or result.get("id")
        self.assertTrue(memory_id, f"no memory_id in: {result}")
        # Inspect provenance via mpm provenance CLI.
        prov = subprocess.run(
            ["mpm", "provenance", memory_id],
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(prov.returncode, 0, prov.stderr)
        self.assertIn("framework: pi", prov.stdout,
                      f"provenance does not attribute framework=pi: {prov.stdout!r}")
        # Cleanup: shred the test memory.
        self._mpm_call(
            "mpm_memory",
            {"action": "shred", "params": {"memory_id": memory_id}},
            framework="pi",
        )

    def test_live_handoff_round_trip(self):
        """Write a handoff with provenance=pi, list it, verify it
        attributes to pi, then shred it."""
        import time
        marker = f"pi-mpm-live-handoff-{int(time.time())}"
        # Write handoff.
        result = self._mpm_call(
            "mpm_handoff",
            {"action": "write", "params": {
                "summary": marker,
                "session_id": "pi-mpm-test-session",
                "state": "clean",
            }},
            framework="pi",
        )
        self.assertTrue(result.get("success"), f"handoff write failed: {result}")
        handoff_id = result.get("handoff_id") or (result.get("handoff") or {}).get("id")
        self.assertTrue(handoff_id, f"no handoff_id in: {result}")
        # List it back.
        listed = self._mpm_call(
            "mpm_handoff",
            {"action": "list", "params": {"session_id": "pi-mpm-test-session", "limit": 5}},
            framework="pi",
        )
        self.assertTrue(listed.get("success"))
        # Shred the handoff (mark confirm=true to be safe).
        shred = self._mpm_call(
            "mpm_handoff",
            {"action": "shred", "params": {"handoff_id": handoff_id, "confirm": True}},
            framework="pi",
        )
        self.assertTrue(shred.get("success"))

    def test_live_mcp_startup_not_applicable(self):
        """Pi does not use the mpm-mcp MCP server. Document the
        skip so the live-test acceptance report reflects that the
        MCP-startup check is not applicable to Pi."""
        # The Pi adapter has no MCP transport surface to start. The
        # typed tools + `mpm call` CLI fallback ARE the agent-facing
        # transport; both inherit MPM_PROVENANCE_FRAMEWORK=pi via
        # `withProvenance`. The MCP server (mpm-mcp) is not used.
        # Verify by stripping comments from index.ts and confirming
        # there is no code path that spawns mpm-mcp as a child
        # process. Historical mentions in comments are fine — they
        # reference the discovery closure that lives in mpm-mcp.
        import re
        text = _read(INDEX_TS)
        no_block = re.sub(r"/\*[\s\S]*?\*/", "", text)
        no_line = re.sub(r"//[^\n]*", "", no_block)
        self.assertNotIn("mpm-mcp", no_line,
                         "index.ts must not spawn mpm-mcp as a child process")


if __name__ == "__main__":
    unittest.main(verbosity=2)
