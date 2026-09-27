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


# Path is derived from the test file location so the suite runs in any
# environment (CI, fresh clone, alternate mount). Previously hardcoded
# to /home/v/workspace/projects/mpm/agent_installation, which broke in
# every other workspace AND in the github actions runner (where the
# checkout lands at /home/runner/work/mpm/mpm/...).
AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
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


# The behavioural-invariant label strings the canonical block must
# teach. Substring match — the bold header text after the numbered
# item marker is enough. Order is the order they appear in the canonical
# block. The list grew from 7 to 10 invariants in the 2026-09-27
# managed-instruction contract bump (1.1.0 -> 1.2.0): substrate
# discoverability (#3), skill lifecycle expansion (#4), work
# lifecycle separation (#6 from old #5), reference acquisition
# (#8), and the explicit session-closure-is-not-work-completion
# principle (#7).
INVARIANT_LABELS = (
    "Wake is auto-injected on session start",
    "Persist during work",
    "Look beyond the compact tool surface",
    "Discover, create, and refine MPM skills",
    "Handoff before genuine session closure",
    "Track durable objectives as work items",
    "Session closure is not work completion",
    "Acquire and retain authoritative references",
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

    def test_block_teaches_all_behavioural_invariants(self):
        """The canonical block must enumerate every behavioural invariant
        by its stable label string. The contract version 1.2.0 (Sept 2026
        expansion) lifted the count from 7 to 10: substrate
        discoverability, skill creation lifecycle, durable work
        tracking, and reference acquisition each became an explicit
        numbered invariant."""
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
        write` — the schema removed `note`, the snippet must agree.

        Sept 2026 update: the canonical block now teaches the compact
        MCP path (`mpm_context action=write_handoff`) as the primary
        reference and the substrate path (`mpm_handoff action=write`)
        only via the `mpm call` CLI escape hatch. The handoff params
        block is now written under the compact path; the test accepts
        either form and asserts neither teaches the stale `note` field.
        """
        # The handoff write/read section lists its params without `note`.
        # Accept either the compact-surface MCP form (`write_handoff` /
        # `read_handoff`) or the substrate `mpm_handoff` form.
        m = re.search(
            r"action `(?:write_handoff|write)` with\s+`params:\s*\{([^}]+)\}",
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
        a = self._adapter("mpm-claude-code")
        actual = _render.extract_copy_paste_block(
            self.text, a["copy_paste_outer_begin"],
            a["copy_paste_outer_end"], a["name"],
        )
        self.assertEqual(
            actual, self.rendered_by_host[a["name"]],
            "Claude copy/paste example drifted from canonical block",
        )

    def test_opencode_copy_paste_matches_rendered(self):
        a = self._adapter("mpm-opencode")
        actual = _render.extract_copy_paste_block(
            self.text, a["copy_paste_outer_begin"],
            a["copy_paste_outer_end"], a["name"],
        )
        self.assertEqual(
            actual, self.rendered_by_host[a["name"]],
            "OpenCode copy/paste example drifted from canonical block",
        )

    def test_pi_copy_paste_matches_rendered(self):
        a = self._adapter("mpm-pi")
        actual = _render.extract_copy_paste_block(
            self.text, a["copy_paste_outer_begin"],
            a["copy_paste_outer_end"], a["name"],
        )
        self.assertEqual(
            actual, self.rendered_by_host[a["name"]],
            "Pi copy/paste example drifted from canonical block",
        )

    def test_hermes_copy_paste_matches_rendered(self):
        a = self._adapter("mpm-hermes")
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
    """mpm-auto-mode-persona-openclaw uses runtime persona injection
    and must NOT be in the adapter list — it has no persistent managed
    file. mpm-memory-openclaw IS in the adapter list (per the 2026-09
    persistent-block architecture) and writes SOUL.md as the persistent
    behavioural contract on top of the runtime wake injection layer."""

    def test_persona_plugin_not_in_adapter_list(self):
        names = {a["name"] for a in _render.ADAPTERS}
        self.assertNotIn(
            "mpm-auto-mode-persona-openclaw", names,
            "mpm-auto-mode-persona-openclaw uses runtime injection — must not be in ADAPTERS",
        )

    def test_memory_openclaw_in_adapter_list(self):
        # mpm-memory-openclaw is now a persistent-block host.
        names = {a["name"] for a in _render.ADAPTERS}
        self.assertIn(
            "mpm-memory-openclaw", names,
            "mpm-memory-openclaw is a persistent-block host; must be in ADAPTERS",
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


class RendererSemanticGuard(unittest.TestCase):
    """Lock the renderer's SEMANTICS — distinguish prose tool mentions
    from `mpm_X action Y` invocation references by sentence context.

    Pre-fix (D-R1, 2026-09-04): the renderer's regex rewrote every
    `mpm_X` token, so prose mentions like `mpm_decisions` / `mpm_lessons`
    in the wake-payload description got rewritten for Claude/Hermes but
    stayed bare in the per-host copy/paste examples — causing 2 drift(s).

    Post-fix: the renderer reads the sentence tail after each token;
    if `\baction(?:s)?\b` does NOT appear in the same sentence, the
    token is a prose mention and is preserved (NOT rewritten). This
    test class locks the heuristic against:
      * synthetic prose-only paragraphs (must stay bare)
      * synthetic invocation references (must transform)
      * prose-before-invocation in same paragraph (the Plan-agent
        flagged asymmetry)
      * multi-prose paragraph with single later invocation
      * Hermes prose-unchanged / invocation-rewritten (different prefix)
      * end-to-end lock on the actual canonical block — rendered
        canonical must preserve prose mentions in the wake-payload
        description exactly as they appear in the source.
    """

    # Synthetic canonical-shaped fragments used across multiple tests.
    _PROSE_ONLY = (
        "Decisions and lessons live behind `mpm_decisions` / "
        "`mpm_lessons`, not in the wake payload.\n"
    )
    _INVOCATION_ONLY = (
        "Persist during work via `mpm_memory` action `save`, "
        "`mpm_decisions` action `record`, and `mpm_lessons` "
        "action `save`.\n"
    )

    def test_prose_only_paragraph_leaves_tokens_bare(self):
        """No `action` keyword anywhere → all `mpm_X` tokens stay bare,
        regardless of host prefix. This is the canonical D-R1 prose
        case: `mpm_decisions` / `mpm_lessons` in the wake-payload
        description must NEVER get a transport prefix."""
        for prefix in ("mpm__", "mcp__mpm__"):
            out = _render.render_for_host(self._PROSE_ONLY, prefix)
            self.assertIn(
                "`mpm_decisions`", out,
                f"prose mention of `mpm_decisions` got rewritten under "
                f"prefix {prefix!r}: {out!r}",
            )
            self.assertIn(
                "`mpm_lessons`", out,
                f"prose mention of `mpm_lessons` got rewritten under "
                f"prefix {prefix!r}: {out!r}",
            )
            # And the prefix did NOT slip into a prose token.
            self.assertNotIn(
                f"{prefix}mpm_decisions", out,
                f"prose mention got transport prefix under {prefix!r}",
            )
            self.assertNotIn(
                f"{prefix}mpm_lessons", out,
                f"prose mention got transport prefix under {prefix!r}",
            )

    def test_invocation_only_paragraph_transforms_tokens(self):
        """`mpm_X action Y` triple → token transforms under all real
        prefixes. This guards against the renderer becoming too
        conservative and accidentally leaving invocation references
        bare for prefix hosts."""
        for prefix in ("mpm__", "mcp__mpm__"):
            out = _render.render_for_host(self._INVOCATION_ONLY, prefix)
            self.assertIn(
                f"`{prefix}mpm_memory`", out,
                f"`mpm_memory` invocation not transformed under "
                f"prefix {prefix!r}: {out!r}",
            )
            self.assertIn(
                f"`{prefix}mpm_decisions`", out,
                f"`mpm_decisions` invocation not transformed under "
                f"prefix {prefix!r}: {out!r}",
            )
            self.assertIn(
                f"`{prefix}mpm_lessons`", out,
                f"`mpm_lessons` invocation not transformed under "
                f"prefix {prefix!r}: {out!r}",
            )

    def test_prose_before_invocation_in_same_paragraph(self):
        """The Plan-agent-flagged asymmetry: when prose mention precedes
        invocation reference in the same paragraph (sentence), the
        prose must stay bare and the invocation must transform. The
        renderer must distinguish the two by sentence context, not by
        paragraph scope (paragraph scope would either ignore the
        prose distinction entirely or rewrite both as a single unit)."""
        mixed = (
            "Decisions and lessons live behind `mpm_decisions`, not in "
            "wake. Persist via `mpm_memory` action `save`.\n"
        )
        out = _render.render_for_host(mixed, "mpm__")
        # Prose `mpm_decisions` must stay bare.
        self.assertIn(
            "`mpm_decisions`", out,
            f"prose `mpm_decisions` got rewritten (Plan-agent "
            f"asymmetry regression): {out!r}",
        )
        self.assertNotIn(
            "`mpm__mpm_decisions`", out,
            f"prose `mpm_decisions` got transport prefix despite "
            f"being a prose mention: {out!r}",
        )
        # Invocation `mpm_memory` MUST transform.
        self.assertIn(
            "`mpm__mpm_memory`", out,
            f"invocation `mpm_memory` did NOT transform (renderer "
            f"became too conservative): {out!r}",
        )

    def test_multi_prose_paragraph_with_single_later_invocation(self):
        """Multiple prose mentions in one paragraph, followed by a
        single invocation reference. All prose tokens must stay bare;
        only the invocation token transforms. This is the strongest
        guard against the renderer regressing back to its
        paragraph-scope 'rewrite every token' behaviour."""
        mixed = (
            "Use `mpm_decisions` for traceability, `mpm_lessons` for "
            "durable learnings, and `mpm_topics` for clustering. "
            "Persist via `mpm_memory` action `save`.\n"
        )
        out = _render.render_for_host(mixed, "mpm__")
        # All three prose mentions stay bare.
        for prose in ("`mpm_decisions`", "`mpm_lessons`", "`mpm_topics`"):
            self.assertIn(
                prose, out,
                f"prose {prose} got rewritten: {out!r}",
            )
        # ...and the prose tokens must not be prefixed.
        for prose_prefixed in (
            "`mpm__mpm_decisions`", "`mpm__mpm_lessons`",
            "`mpm__mpm_topics`",
        ):
            self.assertNotIn(
                prose_prefixed, out,
                f"prose token got transport prefix: {prose_prefixed!r}",
            )
        # Invocation transforms.
        self.assertIn(
            "`mpm__mpm_memory`", out,
            f"invocation `mpm_memory` did NOT transform: {out!r}",
        )

    def test_hermes_prose_unchanged_invocation_rewritten(self):
        """Hermes has a different prefix (`mcp__mpm__`). Same semantic
        distinction must hold: prose bare, invocation rewritten with
        the Hermes prefix. Guards against the renderer accidentally
        being prefix-sensitive at the prose layer."""
        mixed = (
            "Decisions and lessons live behind `mpm_decisions`, not in "
            "wake. Persist via `mpm_memory` action `save`.\n"
        )
        out = _render.render_for_host(mixed, "mcp__mpm__")
        # Prose stays bare under Hermes prefix.
        self.assertIn(
            "`mpm_decisions`", out,
            f"Hermes prose got rewritten: {out!r}",
        )
        self.assertNotIn(
            "`mcp__mpm__mpm_decisions`", out,
            f"Hermes prose got transport prefix: {out!r}",
        )
        # Invocation rewrites with the Hermes prefix.
        self.assertIn(
            "`mcp__mpm__mpm_memory`", out,
            f"Hermes invocation did NOT rewrite: {out!r}",
        )

    def test_empty_prefix_returns_block_unchanged(self):
        """OpenCode and Pi have empty prefix. Rendered output must be
        byte-identical to input regardless of any prose/invocation
        distinction — the heuristic must short-circuit cleanly for
        the empty-prefix case."""
        block = self._PROSE_ONLY + self._INVOCATION_ONLY
        out = _render.render_for_host(block, "")
        self.assertEqual(
            out, block,
            "empty-prefix render must be byte-identical to input",
        )

    def test_rendered_canonical_preserves_prose_in_wake_payload(self):
        """End-to-end lock on the actual canonical source: rendered
        canonical must preserve the bare prose mentions of
        `mpm_decisions` / `mpm_lessons` in the §1 wake-payload
        description (proving the renderer reads the actual file, not
        just synthetic inputs). This is the regression test that
        closes D-R1.

        Locks on the specific §1 prose context — the wake-payload
        description line that says "Decisions live in `mpm_decisions`;
        lessons in `mpm_lessons`." That line contains no `action`
        keyword, so both tokens must remain bare under every host
        prefix. (The §2 invocation reference to `mpm_decisions action
        record` DOES still transform — that's an invocation, not a
        prose mention, and must not regress.)

        The §5 prose mention of `mpm_work` is INTENTIONALLY rewritten
        by the host prefix — the canonical source uses parens form
        (`mpm_work` (Lifecycle: action `create` to open, ...)) so the
        `action` keyword lives in the same sentence as `mpm_work`,
        making it a per-host actionable mention. The previous prose
        form (`mpm_work`. Lifecycle: action `create` ...) had a
        period immediately after the backtick that broke the
        renderer's per-sentence heuristic, leaving Claude/Hermes
        snippets with bare `mpm_work` — a real doc-quality gap. The
        2026-09-06 fix moved the prose to parens form; this test
        asserts the rewritten form is what shows up under prefixed
        adapters.

        Sept 2026 expansion: the work-lifecycle rule moved from §5 to
        §6 (track durable objectives as work items) and now uses the
        action-form prose "Use `mpm_work` action `create` to open"
        instead of the legacy parens form. The semantic guard still
        holds: `action` in the same sentence keeps `mpm_work` a
        per-host actionable reference, so the host prefix must apply."""
        text = CANONICAL_SOURCE.read_text(encoding="utf-8")
        block = _render.extract_canonical_block(text)

        # The §1 prose line is what D-R1 is about: prose mentions of
        # `mpm_decisions` / `mpm_lessons` in the wake-payload
        # description. It must remain bare under every host prefix.
        prose_phrase = (
            "Decisions live in `mpm_decisions`; lessons in "
            "`mpm_lessons`."
        )
        for prefix in ("mpm__", "mcp__mpm__"):
            rendered = _render.render_for_host(block, prefix)
            self.assertIn(
                prose_phrase, rendered,
                f"§1 prose phrase drifted under prefix {prefix!r} — "
                f"`mpm_decisions` / `mpm_lessons` in the wake-payload "
                f"description got rewritten (D-R1 regression)",
            )

        # §6 (Sept 2026 contract): action-form prose `mpm_work action
        # create` lives in the new work-lifecycle rule. The renderer
        # must still apply the prefix because `action` is in the same
        # sentence as `mpm_work` — same semantic guard as the legacy
        # parens form, just new wording.
        prefix_to_expected = {
            "mpm__":      "Use `mpm__mpm_work` action `create` to open",
            "mcp__mpm__": "Use `mcp__mpm__mpm_work` action `create` to open",
            "":            "Use `mpm_work` action `create` to open",
        }
        for prefix, expected_prefix_phrase in prefix_to_expected.items():
            rendered = _render.render_for_host(block, prefix)
            self.assertIn(
                expected_prefix_phrase, rendered,
                f"§6 prose `mpm_work` did not get the expected prefix "
                f"under prefix {prefix!r}. Expected substring "
                f"{expected_prefix_phrase!r} in rendered output.",
            )

        # Sanity: §2 invocations in the same block DO still transform.
        # This guards against the renderer regressing to "never
        # transform" instead of "transform only invocations".
        rendered_claude = _render.render_for_host(block, "mpm__")
        self.assertIn(
            "`mpm__mpm_memory`", rendered_claude,
            "§2 invocation `mpm_memory` did NOT transform — "
            "renderer heuristic is too conservative (regressed away "
            "from invocation handling)",
        )
        self.assertIn(
            "`mpm__mpm_decisions` action `record`", rendered_claude,
            "§2 invocation `mpm_decisions action record` did NOT "
            "transform — renderer regressed to no-op",
        )


class InstructionsPrimerContract(unittest.TestCase):
    """Pin the `instructions_primer` output that mpm-mcp embeds via
    //go:embed and ships in the MCP `initialize.instructions` field.
    Pinned 2026-09-08 after the OpenClaw integration observed the
    agent attempt `mcp__mpm__mpm_handoff` (a non-existent tool on the
    default 3-tool surface). The pre-fix primer said only "via
    `mpm_context` action"; an agent that saw `mpm_handoff` in
    `mpm_help list` output could not connect the two. The fix is to
    pin the exact action name in the primer so the contract is
    unambiguous.

    These tests assert the rendered primer's behavioral contract.
    Drift against the embedded `cmd/mpm-mcp/instructions_primer.txt`
    is caught separately by the Go drift test (`TestInstructionsPrimerDrift`)
    and by `python3 scripts/render_managed_blocks.py --check`.
    """

    _PRIMER_PATH = AGENT_INSTALLATION.parent / "cmd" / "mpm-mcp" / "instructions_primer.txt"

    def _primer_text(self) -> str:
        return _render.render_instructions_primer(CANONICAL_SOURCE)

    def test_primer_is_text(self):
        primer = self._primer_text()
        self.assertIsInstance(primer, str)
        self.assertGreater(len(primer), 0)

    def test_primer_names_handoff_action_explicitly(self):
        """The OpenClaw 2026-09-08 regression. The primer must name
        the exact action (`write_handoff`) so an agent that reads it
        does not attempt the substrate tool name
        (`mcp__mpm__mpm_handoff`) instead."""
        primer = self._primer_text()
        self.assertIn(
            "Handoff before genuine session closure",
            primer,
            "primer must carry the Handoff bullet",
        )
        # The bullet MUST name the action explicitly:
        #   "(via `mpm_context` action `write_handoff`)."
        self.assertRegex(
            primer,
            r"Handoff before genuine session closure \(via `mpm_context` action `write_handoff`\)\.",
            "primer's handoff bullet must explicitly name the "
            "write_handoff action — the pre-fix primer said only "
            "'via `mpm_context` action' which let agents confuse it "
            "with the substrate mpm_handoff tool. See OpenClaw "
            "2026-09-08 final-session regression.",
        )

    def test_primer_omits_substrate_handoff_tool_name(self):
        """The primer's MCP `instructions` field must not advertise
        the substrate `mpm_handoff` tool — it is not in the default
        3-tool surface. If this fails, a future contributor has
        reintroduced the substrate path as an MCP option, which would
        silently allow agents to call a tool the OpenClaw runtime
        filters out."""
        primer = self._primer_text()
        # The primer may mention `mpm_handoff` only in the CLI fallback
        # footer context. We assert the bullet lines themselves do not
        # name the tool as an MCP option.
        for line in primer.splitlines():
            if line.startswith("- "):
                self.assertNotIn(
                    "`mpm_handoff`",
                    line,
                    f"primer bullet must not advertise substrate "
                    f"`mpm_handoff` as an MCP option: {line!r}",
                )

    def test_primer_embedded_file_byte_matches_renderer(self):
        """The file mpm-mcp embeds via //go:embed must byte-match
        the renderer's output. Drift between the two is also caught
        by the Go-side drift test, but pinning it from Python too
        catches renderer-side regressions that the Go test cannot
        (e.g. the renderer being invoked with a stale canonical
        source)."""
        self.assertTrue(
            self._PRIMER_PATH.exists(),
            f"embedded primer missing at {self._PRIMER_PATH}",
        )
        rendered = self._primer_text()
        embedded = self._PRIMER_PATH.read_text(encoding="utf-8")
        self.assertEqual(
            rendered,
            embedded,
            "rendered primer does not match embedded "
            "cmd/mpm-mcp/instructions_primer.txt; re-run "
            "`python3 scripts/render_managed_blocks.py --dump "
            "instructions > cmd/mpm-mcp/instructions_primer.txt`",
        )


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
