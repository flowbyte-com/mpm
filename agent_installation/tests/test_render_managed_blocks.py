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
  - The canonical block teaches all ten responsibilities using
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


def _matrix_rows(block: str) -> list[tuple[str, str]]:
    """Return the behaviour matrix's (situation, destination) rows.

    The header row and the markdown separator row are excluded; every
    other table row is a behaviour row."""
    rows: list[tuple[str, str]] = []
    for line in block.splitlines():
        stripped = line.strip()
        if not stripped.startswith("|"):
            continue
        m = _MATRIX_ROW_RE.match(stripped)
        if not m:
            continue
        rows.append((m.group(1), m.group(2)))
    return rows


# The behavioural destinations the matrix must teach. The set is the
# substrate's write surfaces: a fact, a decision, a lesson, evidence, a
# theory, work state, a skill, a reference, and a handoff — plus the
# explicit "no write" row that keeps the block from turning persistence
# into a reflex.
#
# Each entry is (tool, action, situation-keyword). The action is the
# live registry's action for that tool; the tool-reference stability
# table in the canonical source carries the same vocabulary, and
# `test_managed_block_behaviour_matrix.py` checks both against the
# registry itself.
REQUIRED_MATRIX_DESTINATIONS = (
    ("mpm_memory", "save"),
    ("mpm_decisions", "record"),
    ("mpm_lessons", "save"),
    ("mpm_evidence", "add"),
    ("mpm_theories", "propose"),
    ("mpm_work", "create"),
    ("mpm_skills", "save"),
    ("mpm_references", "add"),
    ("mpm_handoff", "write"),
)


def _flat(text: str) -> str:
    """Collapse all whitespace runs to single spaces.

    The canonical block's prose is hard-wrapped at the source's column
    width, so an anchor that spans a wrap point would not match the raw
    text. Anchors are matched against this flattened form instead, which
    makes them independent of where the source happens to wrap.
    """
    return re.sub(r"\s+", " ", text)


# The behaviour matrix's row shape, and the destinations it must carry.
#
# Contract 4.0.0 (2026-10-28) replaced the block's ten numbered prose
# responsibilities with a behaviour matrix: a two-column markdown table
# whose left column names the SITUATION and whose right column names the
# MPM destination that situation belongs in.
#
# The destination vocabulary is not a stylistic choice. It is the set of
# write surfaces the substrate actually exposes, and each row's tool and
# action are verified against the live registry by
# `test_matrix_destinations_are_live_tool_actions` in
# `test_managed_block_behaviour_matrix.py`.
_MATRIX_ROW_RE = re.compile(r"^\|\s*(?!-{2,})(?!Situation)(.+?)\s*\|\s*(.+?)\s*\|\s*$")


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
        """The canonical block must use bare canonical tool names for any
        tool it names directly — no `{TOOL_PREFIX}` placeholder, no
        host-specific prefix text.

        Post-2026-09-29 simplification: the canonical block now names
        only `mpm_context` (for the wake-context recovery call) and
        `mpm_call` (for the CLI fallback). Other tool names are
        captured in `mpm_help` discovery, the protocol preamble, and
        per-tool descriptions — none of which the canonical block
        needs to repeat. The list below is therefore tight: just the
        tools the block actually invokes."""
        for name in ("mpm_context",):
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

    def test_block_teaches_the_behaviour_matrix(self):
        """The canonical block must name every write destination the
        substrate exposes, with the action that writes it.

        Contract 4.0.0 replaced the ten prose responsibilities with a
        behaviour matrix. The set below is the substrate's write
        surfaces — a fact, a decision, a lesson, evidence, a theory, work
        state, a skill, a reference, and a handoff — and the block's
        matrix must teach each one, plus the explicit "no write" row
        that stops persistence from becoming a reflex.

        The list is asserted as a subset rather than as an exhaustive
        set, so a future destination can be added without editing this
        test; but nothing may be REMOVED without editing it, which is
        the point."""
        rows = _matrix_rows(self.block)
        self.assertTrue(rows, "the managed block must carry a behaviour matrix")
        destinations = {d for _, d in rows}
        for tool, action in REQUIRED_MATRIX_DESTINATIONS:
            self.assertTrue(
                any(
                    f"`{tool}`" in d and f"`{action}`" in d
                    for d in destinations
                ),
                f"the behaviour matrix must teach {tool} action {action}; "
                f"the matrix teaches {sorted(destinations)}",
            )
        # The negative control: a row that says to write nothing.
        self.assertTrue(
            any(d.strip().lower() == "nothing" for d in destinations),
            "the behaviour matrix must carry a 'no write' row; without it "
            "the matrix reads as 'always persist something'",
        )

    def test_matrix_rows_use_the_invocation_form(self):
        """Every matrix destination must name its action with the word
        `action` between the tool and the action.

        The renderer decides whether a backticked tool name is an
        *invocation* reference (and therefore receives a host's
        transport prefix) or a *prose* mention (and therefore does not).
        The signal is the `action` keyword in the same sentence — or a
        backticked, snake_case action identifier attached by a connector
        such as `using`.

        A single-word action (`save`, `record`, `add`) is NOT
        snake_case, so without the `action` keyword the renderer would
        treat the tool as prose and a future host with a namespaced
        transport would ship a broken tool name. This is the mechanism
        that keeps the matrix renderable for any host, and it is worth
        a test."""
        for situation, destination in _matrix_rows(self.block):
            if "`mpm_" not in destination:
                continue
            self.assertIn(
                "action", destination,
                f"matrix row {situation!r} names a tool without the "
                f"`action` keyword: {destination!r}. The renderer would "
                f"treat that tool as a prose mention and would not "
                f"prefix it for a namespaced host.",
            )

    def test_block_uses_stable_non_negotiable_wording(self):
        """The block must NOT use brittle numbered phrasings like
        'The seven non-negotiable invariants below...' which break
        when the count changes. The canonical block uses the stable
        'Your responsibilities' framing instead."""
        self.assertNotIn(
            "The seven non-negotiable invariants below",
            self.block,
            "canonical block uses brittle numbered invariant wording",
        )

    def test_block_omits_stale_note_param(self):
        """Regression for the 2026-09-04 audit finding: the canonical
        block must NOT teach agents to pass `note` to `mpm_handoff
        write` — the schema removed `note`, the snippet must agree.

        Post-2026-09-29: the canonical block dropped handoff-param
        syntax entirely. `mpm_handoff`'s parameter shape lives in
        `internal/core/tools/registry_list.go::mpm_handoff` schema
        (the `required: ["summary"]` line) and in the
        `onboarding_drift_test.go::TestOnboardingDrift_mpm_handoff`
        contract test. If the canonical block ever re-introduces
        handoff-param prose, this test catches the regression."""
        # The canonical block must not teach the stale `note` field.
        self.assertNotIn(
            "note", self.block,
            "canonical block must not teach handoff write's stale 'note' field",
        )

    def test_block_describes_compact_projection_semantically(self):
        """The wake-projection description must NOT pin a specific
        field count. The canonical block dropped payload field
        enumeration entirely — the wake envelope documents its fields,
        the block orients the user on how to treat what it returns.

        The semantic anchor is the "pointers, not a complete or
        unquestionable account" phrasing. It must be matched against the
        flattened text: the canonical source hard-wraps its prose, so
        the raw substring would not survive the wrap point."""
        # No `9-field`, `10-field`, `N-field` wording.
        for bad in ("9-field", "10-field", "11-field", "8-field"):
            self.assertNotIn(
                bad, self.block,
                f"wake projection description pins a brittle field count: {bad!r}",
            )
        # The semantic description IS present.
        self.assertIn(
            "not a complete or unquestionable account", _flat(self.block),
            "wake-projection semantic description missing",
        )


class CanonicalBlockSelection(unittest.TestCase):
    """The extractor must pick the canonical block out of a document
    that contains several managed blocks.

    The canonical source is not just the contract: it also carries a
    copy/paste example per supported host, and every one of those
    examples contains a full managed block. The examples are host-
    rendered — they carry a tool-namespace prefix — so the block the
    extractor returns is the difference between "the contract, ready
    to render" and "one host's already-rendered copy".

    Getting this wrong is silent. A block extracted from the wrong
    occurrence still has the right markers, still has the right
    responsibilities, and still renders — it just renders the wrong
    text into every host, with one host's prefix baked in. These tests
    pin the selection, not the presence.
    """

    # The heading the canonical source uses for the host-neutral block.
    # The extractor does not parse headings; this is the landmark a
    # test uses to prove the right occurrence was chosen.
    CANONICAL_HEADING = "# The canonical managed block (host-neutral)"

    BLOCK_RE = re.compile(
        r"<!-- BEGIN MPM MANAGED BLOCK -->\n.*?<!-- END MPM MANAGED BLOCK -->\n",
        re.DOTALL,
    )

    def setUp(self):
        self.text = CANONICAL_SOURCE.read_text(encoding="utf-8")
        self.block = _render.extract_canonical_block(self.text)
        self.matches = list(self.BLOCK_RE.finditer(self.text))

    def test_the_source_really_does_contain_several_blocks(self):
        """The scenario this class guards is only real if the document
        contains more than one managed block."""
        self.assertGreater(
            len(self.matches), 1,
            "the canonical source is expected to carry a managed block in "
            "each copy/paste example as well as the canonical one",
        )
        self.assertIn(
            self.CANONICAL_HEADING, self.text,
            "the canonical source is expected to label the host-neutral "
            "block so the selection can be verified against it",
        )

    def test_the_extractor_returns_the_canonical_occurrence(self):
        """The returned block is the one under the host-neutral
        heading — not one of the copy/paste examples."""
        heading_at = self.text.index(self.CANONICAL_HEADING)
        after_heading = [m for m in self.matches if m.start() > heading_at]
        self.assertEqual(
            len(after_heading), 1,
            "expected exactly one managed block after the host-neutral "
            f"heading, found {len(after_heading)}",
        )
        self.assertEqual(
            self.block, after_heading[0].group(0),
            "the extractor did not return the canonical block",
        )

    def test_a_prefixed_example_earlier_in_the_document_is_not_selected(self):
        """A host-PREFIXED example that appears BEFORE the canonical
        block must not be what the extractor returns.

        Note what this does NOT claim about the live document. The only
        copy/paste example that ships today is OpenCode's, and OpenCode
        renders the canonical tool names bare — so that example is
        byte-identical to the canonical block, and inequality with it
        would carry no information about the selection. The prefix is
        what makes an example distinguishable from the canonical block,
        and no shipped host uses one.

        So the property is checked against a SYNTHETIC document that
        does contain a prefixed example, placed before the canonical
        block exactly as a copy/paste example would be. The extractor's
        rule is "the LAST occurrence of the universal markers"; a
        regression that selected the FIRST occurrence — or that matched
        on something other than the markers — would return the
        prefixed example here.

        This is a control, not a restatement of the live document: it
        exercises the mis-selection shape that the live document can
        no longer produce.
        """
        prefixed_example = (
            "<!-- BEGIN MPM MANAGED BLOCK -->\n"
            "1. **Wake on session start.** Call `mpm__mpm_context` action\n"
            "   `read_wake_context`.\n"
            "<!-- END MPM MANAGED BLOCK -->\n"
        )
        synthetic = (
            "# A host copy/paste example\n\n"
            "<!-- BEGIN MPM-MANAGED SECTION:some-host-instructions -->\n"
            + prefixed_example
            + "<!-- END MPM-MANAGED SECTION:some-host-instructions -->\n\n"
            "# The canonical managed block (host-neutral)\n\n"
            + self.block
        )

        # The control is only meaningful if the synthetic document really
        # does contain a prefixed occurrence to mis-select.
        occurrences = list(self.BLOCK_RE.finditer(synthetic))
        self.assertEqual(
            len(occurrences), 2,
            "the synthetic document must contain exactly two managed "
            f"blocks (a prefixed example and the canonical one); found "
            f"{len(occurrences)}",
        )
        self.assertIn(
            "mpm__mpm_context", occurrences[0].group(0),
            "the synthetic document's FIRST occurrence must be the "
            "prefixed one, or the control proves nothing",
        )

        selected = _render.extract_canonical_block(synthetic)
        self.assertNotEqual(
            selected, prefixed_example,
            "the extractor returned the prefixed example instead of the "
            "canonical block",
        )
        self.assertEqual(
            selected, self.block,
            "the extractor did not return the canonical block from a "
            "document that contains a prefixed example before it",
        )

    def test_bare_name_examples_are_identical_to_the_canonical_block(self):
        """The OpenCode and Pi examples equal the canonical block.

        This is the property that makes the previous test's scope
        precise: for a bare-name host the "example" IS the canonical
        text, so byte-equality with it carries no information about
        which occurrence was selected.
        """
        heading_at = self.text.index(self.CANONICAL_HEADING)
        examples = [m.group(0) for m in self.matches if m.start() < heading_at]
        bare = [
            example for example in examples
            if "mpm__mpm_" not in example and "mcp__mpm__mpm_" not in example
        ]
        self.assertTrue(
            bare,
            "expected at least one bare-name copy/paste example",
        )
        for example in bare:
            self.assertEqual(
                self.block, example,
                "a bare-name copy/paste example must equal the canonical "
                "block byte-for-byte",
            )

    def test_the_extracted_block_carries_no_host_prefix(self):
        """The canonical block is host-neutral. An example would carry
        its host's namespace prefix."""
        for prefix in ("mpm__mpm_", "mcp__mpm__mpm_"):
            self.assertNotIn(
                prefix, self.block,
                f"the extracted block carries the host-specific prefix "
                f"{prefix!r}; an example block was extracted",
            )

    def test_the_selection_does_not_depend_on_the_examples(self):
        """Removing every copy/paste example must not change what the
        extractor returns.

        The extraction is a last-occurrence rule over a document whose
        canonical block is written last. If the result were somehow
        derived from the examples, removing them would change it. This
        test pins that the examples are inert as far as the canonical
        block is concerned.
        """
        heading_at = self.text.index(self.CANONICAL_HEADING)
        without_examples = self.text[heading_at:]
        extracted = _render.extract_canonical_block(without_examples)
        self.assertEqual(
            extracted, self.block,
            "the extracted canonical block changed when the copy/paste "
            "examples were removed; the selection is coupled to them",
        )

    def test_the_selected_block_is_what_the_renderer_renders(self):
        """The selected block, rendered with a bare prefix, is the
        canonical output — and it is idempotent under re-rendering."""
        rendered = _render.render_for_host(self.block, "")
        self.assertEqual(
            rendered, self.block,
            "the canonical block is not stable under a bare-prefix "
            "render; the extracted occurrence is not the canonical one",
        )


class PerHostRendering(unittest.TestCase):
    """render_for_host() must apply each host's tool-namespace prefix
    correctly.

    OpenCode is the only supported host since the 2026-10-27 adapter
    removal, and its prefix is empty. The prefixed cases stay in the
    suite: `render_for_host` is a SHARED renderer, and a future host
    with a transport prefix must still be rendered correctly — so the
    property is pinned on the renderer, not on the shipped adapter
    set."""

    def setUp(self):
        self.block = _render.extract_canonical_block(
            CANONICAL_SOURCE.read_text(encoding="utf-8"),
        )

    def _render_host(self, prefix: str) -> str:
        return _render.render_for_host(self.block, prefix)

    def test_claude_code_uses_mpm_double_namespace(self):
        """Claude renders `mpm_context` as `mpm__mpm_context` when it
        appears in an invocation context (the "Inherit before you
        begin" responsibility of the canonical block: `mpm_context`
        action `read_wake_context`). The responsibility is named, not
        numbered: a test that reached for "rule 1" would stop pointing
        at the right sentence the moment the contract was renumbered.
        The block names only `mpm_context` and `mpm_call` directly;
        other tools live in mpm_help / tool descriptions, so the
        assertion below checks the one tool the canonical block
        actually invokes."""
        out = self._render_host("mpm__")
        self.assertIn("mpm__mpm_context", out)
        # No triple-prefix regression.
        self.assertNotIn("mpm__mpm_mpm_handoff", out)

    def test_opencode_uses_bare_names(self):
        """OpenCode has an empty prefix; rendered output preserves
        bare canonical names."""
        out = self._render_host("")
        self.assertNotIn("mpm__mpm_", out)
        self.assertNotIn("mcp__mpm__mpm_", out)

    def test_hermes_uses_mcp_namespace(self):
        """Hermes renders `mpm_context` as `mcp__mpm__mpm_context`."""
        out = self._render_host("mcp__mpm__")
        self.assertIn("mcp__mpm__mpm_context", out)
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
    """The retired adapters must NOT be in the renderer\'s adapter list.

    The renderer is the single source of the managed-block text every
    host installs. An adapter that is retired but still listed here
    would keep generating a snippet for a host MPM no longer
    supports — and `render_managed_blocks.py --check` would keep
    demanding that the snippet exist.
    """

    RETIRED = (
        "mpm-claude-code",
        "mpm-hermes",
        "mpm-pi",
        "mpm-memory-openclaw",
        "mpm-auto-mode-persona-openclaw",
    )

    def test_retired_adapters_are_not_in_the_adapter_list(self):
        names = {a["name"] for a in _render.ADAPTERS}
        for retired in self.RETIRED:
            self.assertNotIn(
                retired, names,
                f"{retired} was retired on 2026-10-27 and must not be in "
                "the renderer\'s adapter list",
            )

    def test_opencode_is_in_the_adapter_list(self):
        names = {a["name"] for a in _render.ADAPTERS}
        self.assertIn(
            "mpm-opencode", names,
            "mpm-opencode is the only supported host and must be in "
            "ADAPTERS",
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
        """End-to-end lock on the actual canonical source: the renderer
        must transform the invocation reference to `mpm_context` in
        the "Inherit before you begin" responsibility of the canonical
        block, while leaving prose-only mentions of MPM (no backtick
        tokens in an invocation context) alone.

        The canonical block names exactly one tool in an invocation
        context: `mpm_context`, recovered "using `read_wake_context`".
        Every other "mpm_*" or "MPM" mention is prose. The renderer's
        prose/invocation distinction must still work end-to-end on the
        live source — this test exercises the actual file, not a
        synthetic input."""
        text = CANONICAL_SOURCE.read_text(encoding="utf-8")
        block = _render.extract_canonical_block(text)

        # The invocation reference must receive each host's transport
        # prefix. The anchor is matched against the flattened text: the
        # canonical source hard-wraps its prose, so the raw substring
        # would not survive the wrap point.
        prefix_to_expected = {
            "mpm__":      "recover it through `mpm__mpm_context`",
            "mcp__mpm__": "recover it through `mcp__mpm__mpm_context`",
            "":            "recover it through `mpm_context`",
        }
        for prefix, expected_prefix_phrase in prefix_to_expected.items():
            rendered = _render.render_for_host(block, prefix)
            self.assertIn(
                expected_prefix_phrase, _flat(rendered),
                f"invocation `mpm_context` did not get the expected "
                f"prefix under {prefix!r}. Expected substring "
                f"{expected_prefix_phrase!r} in rendered output.",
            )

        # Sanity: the read_wake_context action must survive the prefix
        # substitution (the action name is host-neutral).
        for prefix in ("mpm__", "mcp__mpm__", ""):
            rendered = _render.render_for_host(block, prefix)
            self.assertIn(
                "read_wake_context", rendered,
                f"action name `read_wake_context` lost under prefix "
                f"{prefix!r}",
            )

        # The block also contains a "mpm call <tool>" mention in the
        # "Discover MPM rather than assuming its limits" responsibility.
        # This is a prose mention (not a backtick-bounded `mpm_<X>` token
        # the renderer's regex matches), so it must NOT receive the host
        # prefix — and the substring "mpm call" must survive under every
        # prefix.
        for prefix in ("mpm__", "mcp__mpm__", ""):
            rendered = _render.render_for_host(block, prefix)
            self.assertIn(
                "mpm call", rendered,
                f"`mpm call` (prose) lost under prefix {prefix!r}",
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


# ---------------------------------------------------------------------------
# Behavioural-commitment coverage tests.
# ---------------------------------------------------------------------------
#
# These tests pin the behavioural commitments of the canonical managed
# block, not the old verbose wording. They replace the prior
# INVARIANT_LABELS substring set with one concept-presence test per
# commitment, and carry a generous size ceiling to stop future verbose
# regression. The ceiling is chosen from the actual accepted block size
# with headroom, not from an arbitrary token target.

# Concept-presence anchors: stable phrases each of the behavioural
# commitments must carry. The set was assembled from the approved
# inheritance-oriented contract (protocol version 3.0.0); each anchor
# is unique to its commitment.
#
# Anchors are matched against the whitespace-collapsed text (see
# `_flat`), so the canonical source's hard wrapping cannot break a
# match. Each anchor is a phrase that would only survive a rewrite
# that changed the commitment itself — not one that merely renames a
# heading.
#
# WHERE AN ANCHOR IS LOOKED FOR.
#
# Contract 4.0.0 (2026-10-28) made the reference stack explicitly
# layered: the always-on managed block carries the operational core
# (the behaviour matrix, the persistence trigger, the uncertainty rule,
# the handoff contract, the wake/recovery path, the discovery path),
# and the canonical protocol carries the full behavioural contract.
#
# So a commitment is satisfied when its anchor appears in EITHER the
# managed block OR the canonical protocol — and the protocol's
# "behavioural commitments" section states all twelve in one place,
# which is where most of them now live. What the stack must never do
# is LOSE a commitment: that is what this test exists to prevent, and
# it is why the union is the right assertion rather than the block
# alone.
#
# Each entry is (block_anchor, protocol_anchor): the first is what the
# always-on surface must carry, the second is what the deeper layer
# must carry. Either may be None when the layer legitimately does not
# need to name it.
COMMITMENT_ANCHORS = {
    # 1. Inheritance and successor responsibility.
    "inheritance": (
        "Your session ends; your knowledge need not.",
        "You are both an inheritor of previous agents' knowledge and a "
        "predecessor of agents yet to come.",
    ),
    # 2. Meaningful persistence during work, not only at the end.
    "persistence_during_work": (
        "Persist as soon as it becomes useful",
        "Do not allow a session ending, context compaction or interrupted "
        "task to erase knowledge that could benefit future work.",
    ),
    # 3. Decisions preserved with their reasoning.
    "decisions_with_reasoning": (
        "Choice and rationale",
        "Preserve what was decided, why it was chosen, the alternatives "
        "considered",
    ),
    # 4. Theories carried with their uncertainty and evidence.
    "theories_with_uncertainty": (
        "Unverified hypothesis — never a fact or lesson",
        "Never silently promote speculation into certainty.",
    ),
    # 5. Discovery before use.
    "discovery_first": (
        "Discover unfamiliar operations with `mpm_help` action",
        "Do not mistake an undiscovered capability for a nonexistent one.",
    ),
    # 6. Event-driven maintenance of inherited knowledge.
    "event_driven_maintenance": (
        "When your work changes what is known",
        "Maintenance should follow meaningful changes, not arbitrary "
        "schedules.",
    ),
    # 7. Targeted retrieval rather than ceremonial searching.
    "targeted_retrieval": (
        "follow its pointers",
        "Do not perform broad memory searches merely to demonstrate that "
        "maintenance occurred.",
    ),
    # 8. Outdated, incorrect and uncertain records kept distinct.
    "outdated_incorrect_uncertain": (
        "stored state may be outdated or superseded",
        "Available evidence does not yet establish whether the knowledge "
        "remains valid.",
    ),
    # 9. Provenance preserved when knowledge is corrected.
    "preserved_provenance": (
        "do not rewrite history",
        "Do not launder history.",
    ),
    # 10. Continuation state that is actionable, not a title.
    "actionable_continuation": (
        "a title is not continuation state",
        "Preserve the reasoning, relevant progress, unresolved dependencies "
        "and next concrete action needed to resume the work.",
    ),
    # 11. Selective persistence — value over volume.
    "selective_persistence": (
        "Routine or transient information",
        "Avoid trivia, redundant memories, unsupported certainty, transient "
        "noise and unnecessary sensitive information.",
    ),
    # 12. The closing principle.
    "closing_principle": (
        "Do not make your successors rediscover what you already learned.",
        "Do not make your successors rediscover what you already learned, "
        "repeat what you already disproved, or abandon what you began to "
        "understand.",
    ),
}

# Size ceiling — a safeguard against accidental bloat, NOT an
# optimisation target.
#
# The 2026-09-29 approved compact block was 2951 bytes / 422 words
# (host-neutral canonical). Ceilings were raised for issue #25 to 5000
# bytes / 750 words.
#
# The approved inheritance-oriented contract (protocol version 3.0.0)
# replaced that block wholesale at 11224 bytes / 1564 words. The
# ceiling was raised to 14000 bytes / 2000 words to match: the contract
# is deliberately behavioural rather than enumerative, and compressing
# it back toward the old ceiling would undo the approved wording.
#
# Contract 4.0.0 (2026-10-28) replaced THAT block with the compact
# behaviour matrix: 2544 bytes / 375 words. The ceiling is now 3100
# bytes / 460 words — about 22% headroom, the same shape of margin the
# 3.0.0 ceiling carried.
#
# The ceiling remains a backstop against runaway growth. If a future
# genuinely essential instruction would exceed it, raise the ceiling
# rather than mutilating the instruction.
COMPACT_BLOCK_BYTE_CEILING = 3100
COMPACT_BLOCK_WORD_CEILING = 460


class CompactBlockSizeCeiling(unittest.TestCase):
    """Guard against accidental instruction bloat.

    Deliberately loose. The accepted block sits far below the ceiling,
    so the gate only fires on runaway growth — not on an author writing
    a necessary instruction at its natural length.
    """

    def setUp(self):
        self.text = CANONICAL_SOURCE.read_text(encoding="utf-8")
        # The canonical (host-neutral) block is the LAST managed block
        # in the source. Extract it for the size check.
        self.block = _render.extract_canonical_block(self.text)

    def test_canonical_block_under_byte_ceiling(self):
        """The canonical (host-neutral) block must stay under 5000 bytes.

        A backstop against returning to the old 12492-byte verbose shape,
        which reintroduced operational detail that belongs in tool
        descriptions rather than the agent-facing behavioural contract.
        It is not a budget to optimise against."""
        size = len(self.block.encode("utf-8"))
        self.assertLessEqual(
            size, COMPACT_BLOCK_BYTE_CEILING,
            f"canonical managed block is {size} bytes; ceiling is "
            f"{COMPACT_BLOCK_BYTE_CEILING}. If an essential instruction "
            f"legitimately needs this much room, raise the ceiling — do "
            f"not compress the instruction to fit it.",
        )

    def test_canonical_block_under_word_ceiling(self):
        """The canonical (host-neutral) block must stay under 750 words.

        The word count is what actually governs how much instruction an
        agent must read, so it is the meaningful bloat guard; the byte
        ceiling is a proxy for it that also catches formatting drift.
        Same rule: raise this rather than mutilate the instruction."""
        size = len(self.block.split())
        self.assertLessEqual(
            size, COMPACT_BLOCK_WORD_CEILING,
            f"canonical managed block is {size} words; ceiling is "
            f"{COMPACT_BLOCK_WORD_CEILING}. Raise the ceiling if the "
            f"instruction needs the room; do not trim it to fit.",
        )

    def test_every_host_render_stays_within_the_ceilings(self):
        """The budget policy applies uniformly to every supported host.

        The ceiling tests above measure the host-neutral canonical block.
        Each adapter re-renders it with its own tool prefix, and those
        prefixes are longer (`mcp__mpm__mpm_references`), so the bytes an
        agent actually loads can exceed the canonical measurement. Word
        count is unaffected — a prefix rewrites a token, it does not add
        one — so it is the byte ceiling that needs per-host coverage.
        """
        for adapter in _render.ADAPTERS:
            rendered = _render.render_for_host(self.block, adapter["tool_prefix"])
            size = len(rendered.encode("utf-8"))
            self.assertLessEqual(
                size, COMPACT_BLOCK_BYTE_CEILING,
                f"{adapter['name']} renders {size} bytes for the host to "
                f"load; ceiling is {COMPACT_BLOCK_BYTE_CEILING}. The "
                f"instruction-budget policy applies to every host equally.")
            words = len(rendered.split())
            self.assertLessEqual(
                words, COMPACT_BLOCK_WORD_CEILING,
                f"{adapter['name']} renders {words} words; ceiling is "
                f"{COMPACT_BLOCK_WORD_CEILING}")


class ManagedBlockDuplicationGuard(unittest.TestCase):
    """Catch ACCIDENTAL bloat, which is what the size ceilings exist to
    catch.

    The instruction-budget policy (see "Instruction budget policy" in
    MPM_AGENT_INTEGRATION_SNIPPETS.md) is explicit that ceilings are
    optimisation safeguards, not correctness constraints: essential
    capability discovery must never be dropped to fit a budget. So
    these tests must not measure "is the block too long" — they measure
    "has the block started repeating itself", which is the growth mode
    that is always a defect regardless of budget.
    """

    def setUp(self):
        self.text = CANONICAL_SOURCE.read_text(encoding="utf-8")
        self.block = _render.extract_canonical_block(self.text)

    def _sentences(self) -> list[str]:
        # Strip the HTML comment header, which legitimately repeats the
        # same sentence shape across blocks, and unwrap hard wrapping
        # so a sentence split across lines counts once.
        body = re.sub(r"<!--.*?-->", " ", self.block, flags=re.DOTALL)
        body = re.sub(r"[#*`]", " ", body)
        body = re.sub(r"\s+", " ", body)
        parts = re.split(r"(?<=[.!?])\s+", body)
        return [p.strip() for p in parts if len(p.strip()) > 25]

    def test_no_sentence_is_duplicated(self):
        sentences = self._sentences()
        counts: dict[str, int] = {}
        for s in sentences:
            counts[s] = counts.get(s, 0) + 1
        dupes = {s: n for s, n in counts.items() if n > 1}
        self.assertEqual(
            {}, dupes,
            "the managed block repeats identical sentences: "
            f"{dupes}. Duplicated instruction is pure bloat — it costs "
            f"agent context and signals an unfinished edit.")

    def test_no_matrix_row_repeats_another_matrix_row(self):
        """Each matrix row should say something the others do not.

        Catches a copy-paste edit that clones a row's situation text or
        its destination.

        Contract 4.0.0 replaced the block's ten numbered prose
        responsibilities with a behaviour matrix, so the shape this
        guard watches changed with it: the matrix is the block's
        structure now. The old heading regex found nothing here, which
        made this test vacuously true — a rename that emptied the
        structure would have passed. It is now a real assertion about
        the structure that ships."""
        rows = _matrix_rows(self.block)
        self.assertGreaterEqual(
            len(rows), 8,
            "expected the behaviour matrix to carry at least 8 rows; "
            f"found {len(rows)}",
        )
        situations = [re.sub(r"\s+", " ", s).strip().lower() for s, _ in rows]
        self.assertEqual(
            len(situations), len(set(situations)),
            f"duplicate matrix situations: {situations}",
        )
        destinations = [d.strip().lower() for _, d in rows]
        self.assertEqual(
            len(destinations), len(set(destinations)),
            f"duplicate matrix destinations: {destinations}",
        )

    def test_instruction_budget_policy_is_stated_in_canonical_source(self):
        """The budget policy must be documented where the block is
        authored, so a future author meets it before writing rather than
        after being told by a failing test."""
        low = self.text.lower()
        # The policy is prose in a hard-wrapped markdown file, so match
        # against whitespace-collapsed text rather than raw lines.
        flat = re.sub(r"\s+", " ", low)
        for phrase in ("instruction budget policy",
                       "not correctness constraints",
                       "never be omitted merely to satisfy"):
            self.assertIn(phrase, flat,
                          f"the canonical source must state {phrase!r}; the "
                          f"budget policy is what stops the ceilings being "
                          f"read as a correctness constraint")

    def test_budget_policy_does_not_grow_the_managed_block(self):
        """The policy is documentation for maintainers. It must not leak
        into the agent-facing block, which is loaded into every session."""
        self.assertNotIn(
            "Instruction budget policy", self.block,
            "the budget policy is maintainer documentation and must not "
            "appear in the agent-facing managed block")


class InheritedIntelligenceCommitmentCoverage(unittest.TestCase):
    """Pin the behavioural commitments of the MPM reference stack.

    Each commitment has anchoring phrases that capture its behavioural
    load. The test fails if any commitment's anchor is missing,
    regardless of how the rest of the prose is worded.

    The anchors are matched against the whitespace-collapsed text, so a
    hard-wrap in the canonical source cannot make a present commitment
    look absent. They are also matched by meaning, not by position: no
    anchor names a section number, so re-ordering the commitments does
    not break the coverage.

    SINCE CONTRACT 4.0.0 — THE STACK, NOT THE BLOCK ALONE.

    The reference stack is layered: the always-on managed block carries
    the operational core and the canonical protocol carries the full
    behavioural contract. Every commitment must therefore appear in the
    UNION, and the two layers are asserted separately: the block anchor
    must be in the managed block (it is the always-on surface, so a
    commitment that only exists in the protocol is a commitment the
    agent may never meet), and the protocol anchor must be in the
    protocol (it is the contract, so a commitment that only exists in
    the block has nowhere to elaborate).

    The previous form of this test asserted the old anchors against the
    block alone. That was correct when the block carried all twelve
    commitments in prose. It is not correct now, and keeping it would
    have forced either a lie (a block that re-grew to 2,000 tokens) or
    a silent deletion (a commitment quietly dropped from the contract).
    """

    def setUp(self):
        self.block = _render.extract_canonical_block(
            CANONICAL_SOURCE.read_text(encoding="utf-8"),
        )
        self.flat = _flat(self.block)
        self.protocol_flat = _flat(
            (AGENT_INSTALLATION / "mpm-agent-protocol.md").read_text(
                encoding="utf-8",
            )
        )

    def _assert_commitment(self, key: str) -> None:
        block_anchor, protocol_anchor = COMMITMENT_ANCHORS[key]
        self.assertIn(
            block_anchor, self.flat,
            f"commitment {key!r} is missing its always-on anchor "
            f"{block_anchor!r} from the managed block",
        )
        self.assertIn(
            protocol_anchor, self.protocol_flat,
            f"commitment {key!r} is missing its contract anchor "
            f"{protocol_anchor!r} from the canonical protocol",
        )

    def test_inheritance_and_successor_responsibility(self):
        self._assert_commitment("inheritance")

    def test_meaningful_persistence_during_work(self):
        self._assert_commitment("persistence_during_work")

    def test_decisions_preserve_their_reasoning(self):
        self._assert_commitment("decisions_with_reasoning")

    def test_theories_carry_uncertainty_and_evidence(self):
        self._assert_commitment("theories_with_uncertainty")

    def test_discovery_precedes_use(self):
        self._assert_commitment("discovery_first")

    def test_maintenance_is_event_driven(self):
        self._assert_commitment("event_driven_maintenance")

    def test_retrieval_is_targeted_not_ceremonial(self):
        self._assert_commitment("targeted_retrieval")

    def test_outdated_incorrect_and_uncertain_are_distinguished(self):
        self._assert_commitment("outdated_incorrect_uncertain")

    def test_provenance_is_preserved_when_knowledge_is_corrected(self):
        self._assert_commitment("preserved_provenance")

    def test_continuation_state_is_actionable(self):
        self._assert_commitment("actionable_continuation")

    def test_persistence_is_selective(self):
        self._assert_commitment("selective_persistence")

    def test_the_closing_principle_is_present(self):
        self._assert_commitment("closing_principle")


class CompactBlockForbiddenSubstrings(unittest.TestCase):
    """Pin the slim canonical block's anti-pattern set: enumerations
    and host-specific detail that belong elsewhere.

    These are the categories the 2026-09-29 simplification specifically
    removed. The test fails if any reappears, because re-introduction
    would mean the simplification was undone in the wording rather
    than the structure.
    """

    def setUp(self):
        self.block = _render.extract_canonical_block(
            CANONICAL_SOURCE.read_text(encoding="utf-8"),
        )

    def test_no_hardcoded_tool_count(self):
        """The canonical block must NOT carry a hard-coded tool count.
        Counts in permanent instructions were the failure class
        commit ee382813 was written to close. Today the compact
        surface is 3 tools; tomorrow it could be 5. The block must
        describe behaviour, not surface shape."""
        for bad in (
            "21 Registry", "22-tool", "22 tool", "3-tool compact",
            "3 tool compact", "compact MCP surface is exactly",
            "compact native MCP surface intentionally exposes only",
        ):
            self.assertNotIn(
                bad, self.block,
                f"canonical block carries hard-coded tool count: {bad!r}",
            )

    def test_no_host_namespace_prefix(self):
        """The canonical block is host-neutral. Host-specific
        prefixes (`mpm__mpm_`, `mcp__mpm__mpm_`) belong in the
        per-adapter copy/paste examples and the per-host template
        snippets, never in the canonical source."""
        for prefix in ("mpm__mpm_", "mcp__mpm__mpm_"):
            self.assertNotIn(
                prefix, self.block,
                f"canonical block leaks host-specific prefix {prefix!r}",
            )

    def test_no_placeholder_text(self):
        self.assertNotIn(
            "{TOOL_PREFIX}", self.block,
            "canonical block contains placeholder text",
        )


# Note: an earlier revision of this file carried a
# `CompactBlockDeletedConcepts` test class that forbade substrings
# like "mpm_lessons", "write_handoff", "version-bound" — terms that
# happened to disappear with the verbose block but are not
# architecturally forbidden. The class was removed because negative
# assertions should protect architecture and behaviour, not fossilise
# today's exact prose. A future refactor that re-introduces one of
# those substrings in service of a different rule should not have to
# fight a brittle substring gate; the architecture-level
# `CompactBlockForbiddenSubstrings` (hardcoded counts, host
# prefixes, placeholder text) is the right place for negative
# assertions.


class CopyPasteExampleStructure(unittest.TestCase):
    """Every copy/paste example in the canonical source must be a
    well-formed, parseable fenced markdown block.

    Added 2026-09-29 after the instruction-architecture rewrite silently
    dropped the ```markdown fence and the
    `<!-- BEGIN MPM MANAGED BLOCK:mpm-hermes -->` opener from the Hermes
    example. The renderer still produced byte-correct adapter snippets
    (the copy/paste examples are hand-maintained prose, not renderer
    output), so every render-parity gate stayed green — but the Hermes
    example stopped parsing as a managed block, and
    test_stage2e4_behavioral_contract.ContinuityContract dropped to 3
    host blocks instead of 4.

    This is the structural class of defect those gates could not see:
    renderer parity proves the renderer is consistent, not that the
    hand-maintained examples around it are well-formed.
    """

    def test_each_copy_paste_example_is_fenced_and_parseable(self):
        """Each copy/paste example must sit inside a ```markdown fence
        and yield exactly one parseable managed block bounded by its
        adapter's outer markers.

        extract_copy_paste_block is the same parser the renderer uses, so
        this asserts the example is well-formed for a human pasting it
        into a host file — not merely equal to the rendered output.
        """
        text = CANONICAL_SOURCE.read_text(encoding="utf-8")
        for adapter in _render.ADAPTERS:
            with self.subTest(adapter=adapter["name"]):
                outer_begin = adapter["copy_paste_outer_begin"]
                outer_end = adapter["copy_paste_outer_end"]

                # Locate the real example span. The canonical source
                # documents each marker convention inline, backtick-wrapped,
                # so a naive search hits the prose mention first — and a
                # non-greedy regex from prose would swallow the real example
                # as its own match. Scan opener positions directly: the real
                # example's opener starts a line (prose mentions are
                # mid-sentence, inside backticks) and is followed by a closer
                # with an inner managed block between them.
                example_span = None
                idx = 0
                while True:
                    start = text.find(outer_begin, idx)
                    if start == -1:
                        break
                    idx = start + 1
                    at_line_start = start == 0 or text[start - 1] == "\n"
                    if not at_line_start:
                        continue
                    close = text.find(outer_end, start + len(outer_begin))
                    if close == -1:
                        continue
                    if "<!-- BEGIN MPM MANAGED BLOCK -->" in text[start:close]:
                        example_span = (start, close)
                        break
                self.assertIsNotNone(
                    example_span,
                    f"{adapter['name']}: no copy/paste example containing an "
                    f"inner managed block was found between the outer markers",
                )

                # 1. The example must live inside a ```markdown fence, so
                #    the marker comments are presented as pasteable
                #    content rather than as live document prose.
                preceding = text[:example_span[0]]
                last_open = preceding.rfind("```markdown")
                last_close = preceding.rfind("```\n")
                self.assertGreater(
                    last_open, last_close,
                    f"{adapter['name']}: copy/paste example must open with "
                    f"a ```markdown fence before the outer marker",
                )

                # 2. The parser must recover a non-empty block, which
                #    requires both outer markers and one inner universal
                #    managed block nested between them.
                block = _render.extract_copy_paste_block(
                    text, outer_begin, outer_end, adapter["name"],
                )
                self.assertTrue(
                    block.strip(),
                    f"{adapter['name']}: extracted managed block is empty",
                )

                # 3. Exactly one universal managed block, and the outer
                #    closer must follow the inner closer.
                self.assertEqual(
                    block.count("<!-- BEGIN MPM MANAGED BLOCK -->"), 1,
                    f"{adapter['name']}: copy/paste example must contain "
                    f"exactly one universal managed block opener",
                )
                inner_end = text.index(
                    "<!-- END MPM MANAGED BLOCK -->", example_span[0])
                self.assertLess(
                    inner_end, example_span[1],
                    f"{adapter['name']}: managed block must close before "
                    f"the outer copy/paste closer",
                )

    def test_every_file_backed_host_has_an_extractable_block(self):
        """Every persistent-file host must yield a parseable managed
        block.

        This is the invariant whose breakage made the Hermes defect
        visible downstream as a host-count failure (ContinuityContract
        extracted 3 of 4 when the Hermes outer markers were lost). The
        count is derived from the adapter list rather than hard-coded,
        so it cannot rot when an adapter is added or retired.
        """
        text = CANONICAL_SOURCE.read_text(encoding="utf-8")
        expected = {
            a["name"] for a in _render.ADAPTERS
            if a["name"] != "mpm-memory-openclaw"
        }
        found = set()
        for adapter in _render.ADAPTERS:
            if adapter["name"] == "mpm-memory-openclaw":
                continue  # OpenClaw inherits the universal block
            block = _render.extract_copy_paste_block(
                text,
                adapter["copy_paste_outer_begin"],
                adapter["copy_paste_outer_end"],
                adapter["name"],
            )
            if block:
                found.add(adapter["name"])
        self.assertEqual(
            found, expected,
            "every file-backed host must have an extractable managed "
            f"block; got {sorted(found)}",
        )


if __name__ == "__main__":
    unittest.main()
