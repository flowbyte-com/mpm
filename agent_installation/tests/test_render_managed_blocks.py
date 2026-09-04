"""
test_render_managed_blocks.py — Drift and parity detection for the
canonical MPM_AGENT_INTEGRATION_SNIPPETS.md source.

The render script `agent_installation/scripts/render_managed_blocks.py`
reads the canonical source and produces per-adapter template snippets.
These tests pin:

  - Canonical source exists at the documented path.
  - extract_canonical_block() returns the host-neutral canonical block
    (the LAST occurrence of the universal managed-block markers in the
    file; earlier occurrences are host-rendered copy/paste examples).
  - The canonical block uses bare canonical tool names (no {TOOL_PREFIX}
    placeholder; no host-specific prefix text).
  - The canonical block teaches all seven behavioural invariants using
    their stable label strings.
  - The canonical block does NOT advertise `note` as a parameter on
    `mpm_handoff write` (regression for the 2026-09-04 audit finding;
    the schema removed `note`).
  - The canonical block describes compact wake projection semantically
    rather than pinning a specific field count.
  - For every persistent-file host, the rendered canonical block applies
    that host's tool-namespace prefix correctly (Claude `mpm__`,
    OpenCode/Pi bare, Hermes `mcp__mpm__`).
  - The four copy/paste example sections match the rendered canonical
    block for their host byte-for-byte.
  - The checked-in adapter template snippets match the rendered output
    byte-for-byte (idempotency: re-rendering must converge).
  - OpenClaw is NOT in the adapter list (it uses runtime injection).

When these tests fail, regenerate snippets with:
    python3 agent_installation/scripts/render_managed_blocks.py
…and update the canonical source if the change is intentional.
"""

from __future__ import annotations

import importlib.util
import re
import sys
import unittest
from pathlib import Path


AGENT_INSTALLATION = Path("/home/v/workspace/projects/mpm/agent_installation")
RENDER_SCRIPT = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"


def _load_render_module():
    spec = importlib.util.spec_from_file_location(
        "render_managed_blocks", RENDER_SCRIPT,
    )
    assert spec and spec.loader, "render script must be importable"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


_render = _load_render_module()


# The seven behavioural-invariant label strings the canonical block must
# teach. Order is the order they appear in the canonical block.
INVARIANT_LABELS = (
    "Wake on session start",
    "Persist during work",
    "Skill discovery before reinventing",
    "Handoff before genuine session closure",
    "Session closure is not work completion",
    "MPM is the source of truth",
    "Recovery / fallback",
)


class CanonicalSourceExists(unittest.TestCase):
    """The canonical snippets file must exist at the documented path
    and not be truncated."""

    def test_canonical_file_exists(self):
        self.assertTrue(
            CANONICAL_SOURCE.is_file(),
            f"canonical source missing: {CANONICAL_SOURCE}",
        )

    def test_canonical_file_is_non_trivial(self):
        self.assertGreater(
            CANONICAL_SOURCE.stat().st_size, 1000,
            "canonical source too small — likely truncated",
        )


class CanonicalBlockExtraction(unittest.TestCase):
    """extract_canonical_block() must return the host-neutral canonical
    block. It uses LAST-occurrence semantics because the canonical
    source contains earlier host-rendered copies in the copy/paste
    example sections."""

    def setUp(self):
        self.block = _render.extract_canonical_block(
            CANONICAL_SOURCE.read_text(encoding="utf-8"),
        )

    def test_block_has_begin_marker(self):
        self.assertTrue(self.block.startswith("<!-- BEGIN MPM MANAGED BLOCK -->"))

    def test_block_has_end_marker(self):
        self.assertTrue(self.block.rstrip().endswith("<!-- END MPM MANAGED BLOCK -->"))

    def test_block_has_source_line(self):
        """The canonical block must reference its source path so future
        archaeology can trace provenance."""
        self.assertIn(
            "<!-- source: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md -->",
            self.block,
        )

    def test_block_uses_bare_canonical_names(self):
        """The canonical block must use bare canonical tool names
        (`mpm_handoff`, `mpm_memory`) — no `{TOOL_PREFIX}` placeholder,
        no host-specific prefix text. (References to tools outside the
        universal seven live in the tool-reference table and the
        protocol preamble, not in the agent-facing block itself —
        brief §13: avoid protocol duplication.)"""
        for name in ("mpm_context", "mpm_memory", "mpm_decisions", "mpm_lessons",
                     "mpm_topics", "mpm_references", "mpm_skills", "mpm_handoff",
                     "mpm_scratchpad", "mpm_work"):
            self.assertIn(name, self.block, f"canonical block missing {name!r}")
        # No placeholder text.
        self.assertNotIn(
            "{TOOL_PREFIX}", self.block,
            "canonical block must not contain {TOOL_PREFIX} placeholder text",
        )
        # No host-specific prefix text.
        for prefix in ("mpm__mpm_", "mcp__mpm__mpm_"):
            self.assertNotIn(
                prefix, self.block,
                f"host-specific prefix {prefix!r} leaked into canonical source",
            )

    def test_block_teaches_all_seven_invariants(self):
        """The canonical block must enumerate the seven behavioural
        invariants by their stable label strings."""
        for label in INVARIANT_LABELS:
            self.assertIn(label, self.block, f"canonical block missing invariant: {label!r}")

    def test_block_uses_stable_non_negotiable_wording(self):
        """The block must use the stable wording for the invariants
        section header — not brittle numbered phrasings like
        'The seven non-negotiable invariants below...' which break
        when the count changes."""
        self.assertIn(
            "The following are the non-negotiable MPM behavioural",
            self.block,
            "canonical block missing stable invariant-header wording",
        )
        self.assertNotIn(
            "The seven non-negotiable invariants below",
            self.block,
            "canonical block uses brittle numbered invariant wording",
        )

    def test_block_omits_stale_note_param(self):
        """Regression for the 2026-09-04 audit finding: the canonical
        block must NOT teach agents to pass `note` to `mpm_handoff
        write` — the schema removed `note`, the snippet must agree."""
        # The handoff write section must list its params without `note`.
        m = re.search(
            r"action `write` with `params:\s*\{([^}]+)\}",
            self.block,
        )
        self.assertIsNotNone(m, "handoff write params block not found")
        params = m.group(1)
        for token in (t.strip() for t in params.split(",")):
            self.assertFalse(
                token.startswith("note"),
                f"handoff write params still teach stale 'note' field: {params!r}",
            )

    def test_block_describes_compact_projection_semantically(self):
        """The wake-projection description must NOT pin a specific
        field count (the count varies with session state: 8 always-on
        plus 0/2 conditional when `LastHandoff` is set)."""
        # No `9-field`, `10-field`, `N-field` wording.
        for bad in ("9-field", "10-field", "11-field", "8-field"):
            self.assertNotIn(
                bad, self.block,
                f"wake projection description pins a brittle field count: {bad!r}",
            )
        # The semantic description IS present.
        self.assertIn(
            "id+summary envelope", self.block,
            "wake projection semantic description missing",
        )


class PerHostRendering(unittest.TestCase):
    """render_for_host() must apply each host's tool-namespace prefix
    correctly: Claude `mpm__`, OpenCode/Pi bare, Hermes `mcp__mpm__`."""

    def setUp(self):
        self.block = _render.extract_canonical_block(
            CANONICAL_SOURCE.read_text(encoding="utf-8"),
        )

    def _render_host(self, prefix: str) -> str:
        return _render.render_for_host(self.block, prefix)

    def test_claude_code_uses_mpm_double_namespace(self):
        out = self._render_host("mpm__")
        self.assertIn("mpm__mpm_handoff", out)
        self.assertIn("mpm__mpm_memory", out)
        self.assertIn("mpm__mpm_context", out)
        # No triple-prefix regression.
        self.assertNotIn("mpm__mpm_mpm_handoff", out)

    def test_opencode_uses_bare_names(self):
        out = self._render_host("")
        # Bare `mpm_handoff` is still present; no transport wrapper.
        self.assertIn("`mpm_handoff`", out)
        self.assertNotIn("mpm__mpm_", out)
        self.assertNotIn("mcp__mpm__mpm_", out)

    def test_pi_uses_bare_names(self):
        out = self._render_host("")
        self.assertIn("`mpm_handoff`", out)
        self.assertNotIn("mpm__mpm_", out)
        self.assertNotIn("mcp__mpm__mpm_", out)

    def test_hermes_uses_mcp_namespace(self):
        out = self._render_host("mcp__mpm__")
        self.assertIn("mcp__mpm__mpm_handoff", out)
        self.assertIn("mcp__mpm__mpm_memory", out)
        # No double-prefix on the transport.
        self.assertNotIn("mcp__mpm__mpm_mpm_handoff", out)

    def test_render_for_host_is_idempotent(self):
        """rendering with an empty prefix must leave the block byte-equivalent
        to input (used for OpenCode and Pi)."""
        out = self._render_host("")
        self.assertEqual(out, self.block)


class CopyPasteExampleParity(unittest.TestCase):
    """Each host's copy/paste example in the canonical source must match
    the rendered canonical block for that host byte-for-byte. Hand-edited
    examples that drift from the canonical block cause this to fail."""

    def setUp(self):
        self.text = CANONICAL_SOURCE.read_text(encoding="utf-8")
        self.rendered_by_host = _render.copy_paste_rendered(CANONICAL_SOURCE)

    def _adapter(self, name: str):
        for a in _render.ADAPTERS:
            if a["name"] == name:
                return a
        self.fail(f"adapter {name!r} not in ADAPTERS list")

    def test_claude_copy_paste_matches_rendered(self):
        a = self._adapter("claude-code-mpm")
        actual = _render.extract_copy_paste_block(
            self.text, a["copy_paste_outer_begin"],
            a["copy_paste_outer_end"], a["name"],
        )
        self.assertEqual(
            actual, self.rendered_by_host[a["name"]],
            "Claude copy/paste example drifted from canonical block",
        )

    def test_opencode_copy_paste_matches_rendered(self):
        a = self._adapter("opencode-mpm")
        actual = _render.extract_copy_paste_block(
            self.text, a["copy_paste_outer_begin"],
            a["copy_paste_outer_end"], a["name"],
        )
        self.assertEqual(
            actual, self.rendered_by_host[a["name"]],
            "OpenCode copy/paste example drifted from canonical block",
        )

    def test_pi_copy_paste_matches_rendered(self):
        a = self._adapter("pi-mpm")
        actual = _render.extract_copy_paste_block(
            self.text, a["copy_paste_outer_begin"],
            a["copy_paste_outer_end"], a["name"],
        )
        self.assertEqual(
            actual, self.rendered_by_host[a["name"]],
            "Pi copy/paste example drifted from canonical block",
        )

    def test_hermes_copy_paste_matches_rendered(self):
        a = self._adapter("hermes-mpm")
        actual = _render.extract_copy_paste_block(
            self.text, a["copy_paste_outer_begin"],
            a["copy_paste_outer_end"], a["name"],
        )
        self.assertEqual(
            actual, self.rendered_by_host[a["name"]],
            "Hermes copy/paste example drifted from canonical block",
        )


class ByteForByteParity(unittest.TestCase):
    """The checked-in adapter template snippets must match the rendered
    output byte-for-byte. Drift here means the canonical source changed
    without regenerating snippets."""

    def test_render_matches_checked_in_snippets(self):
        rendered = _render.render_all(CANONICAL_SOURCE, AGENT_INSTALLATION)
        mismatches: list[str] = []
        for adapter in _render.ADAPTERS:
            checked_in = _render.snippet_path(AGENT_INSTALLATION, adapter)
            if not checked_in.is_file():
                mismatches.append(f"missing checked-in snippet: {checked_in}")
                continue
            actual = checked_in.read_text(encoding="utf-8")
            expected = rendered[adapter["name"]]
            if actual != expected:
                mismatches.append(
                    f"{adapter['name']}: snippet drifted from canonical "
                    f"source. Regenerate with:\n"
                    f"    python3 scripts/render_managed_blocks.py"
                )
        if mismatches:
            self.fail("\n".join(mismatches))


class AdapterExclusion(unittest.TestCase):
    """OpenClaw uses runtime injection — it must NOT be in the adapter
    list (no persistent managed file). The runtime binding is verified
    through the OpenClaw adapter's own test surface, not through
    managed-block parity."""

    def test_openclaw_not_in_adapter_list(self):
        names = {a["name"] for a in _render.ADAPTERS}
        self.assertNotIn(
            "openclaw-mpm-memory", names,
            "openclaw-mpm-memory uses runtime injection — must not be in ADAPTERS",
        )
        self.assertNotIn(
            "openclaw-mpm-auto-mode-persona", names,
            "openclaw-mpm-auto-mode-persona uses runtime injection — must not be in ADAPTERS",
        )


class RenderScriptInvariants(unittest.TestCase):
    """The render script itself must keep its module surface stable
    (these are the test-import contract)."""

    def test_module_exposes_public_api(self):
        for name in (
            "extract_canonical_block",
            "render_for_host",
            "render_all",
            "copy_paste_rendered",
            "compose_snippet",
            "extract_copy_paste_block",
            "ADAPTERS",
            "snippet_path",
        ):
            self.assertTrue(
                hasattr(_render, name),
                f"render script missing public symbol: {name}",
            )

    def test_all_adapters_have_required_keys(self):
        required_keys = (
            "name", "tool_prefix", "snippet_path",
            "copy_paste_outer_begin", "copy_paste_outer_end",
            "header", "footer",
        )
        for adapter in _render.ADAPTERS:
            for key in required_keys:
                self.assertIn(
                    key, adapter,
                    f"adapter {adapter.get('name', '?')!r} missing key: {key!r}",
                )
            self.assertIsInstance(adapter["tool_prefix"], str)

    def test_adapter_names_are_unique(self):
        names = [a["name"] for a in _render.ADAPTERS]
        self.assertEqual(
            len(names), len(set(names)),
            f"duplicate adapter names: {names}",
        )

    def test_render_for_host_is_pure(self):
        """render_for_host must not mutate the canonical block — it is
        called on shared module state."""
        text = CANONICAL_SOURCE.read_text(encoding="utf-8")
        block = _render.extract_canonical_block(text)
        before = block
        _render.render_for_host(block, "mpm__")
        self.assertEqual(block, before,
                         "render_for_host mutated its input (impure)")


class CanonicalVersionMarker(unittest.TestCase):
    """The top-of-file HTML comment pins the managed-instruction
    contract version. Pinned by §C-3 of the known-debt closure pass."""

    # Semver: MAJOR.MINOR.PATCH (each numeric).
    _SEMVER_RE = re.compile(
        r"^<!--\s*mpm_agent_integration_version:\s*"
        r"(?P<major>0|[1-9]\d*)\.(?P<minor>0|[1-9]\d*)\.(?P<patch>0|[1-9]\d*)"
        r"\s*-->$"
    )

    def _marker_line(self) -> str | None:
        for line in CANONICAL_SOURCE.read_text(encoding="utf-8").splitlines():
            if "mpm_agent_integration_version" in line:
                return line.strip()
        return None

    def test_marker_is_present(self):
        line = self._marker_line()
        self.assertIsNotNone(line,
                             "canonical source must carry the "
                             "mpm_agent_integration_version marker")

    def test_marker_is_valid_semver(self):
        line = self._marker_line()
        self.assertIsNotNone(line)
        m = self._SEMVER_RE.match(line.strip())
        self.assertIsNotNone(
            m,
            f"version marker must be valid semver in an HTML comment, "
            f"got: {line!r}"
        )

    def test_exactly_one_marker_at_top_of_file(self):
        """The marker must appear exactly once at the top of the file
        (before the H1). The Versioning section may mention the marker
        in prose, but only the top-of-file one counts as the contract
        version pointer."""
        text = CANONICAL_SOURCE.read_text(encoding="utf-8")
        lines = text.splitlines()
        # Find the line index of the H1.
        h1_idx = None
        for i, line in enumerate(lines):
            if line.startswith("# "):
                h1_idx = i
                break
        self.assertIsNotNone(h1_idx, "canonical source must start with an H1")
        head = "\n".join(lines[:h1_idx])
        count = head.count("mpm_agent_integration_version")
        self.assertEqual(
            count, 1,
            f"exactly one version marker expected at the top of the "
            f"file (before the H1), found {count}"
        )


if __name__ == "__main__":
    unittest.main()
