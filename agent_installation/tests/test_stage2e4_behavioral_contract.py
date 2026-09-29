"""Stage 2E.4 cross-host behavioral contract.

Verifies that every maintained host integration's managed instruction
block teaches the canonical continuity contract:

  - read_wake_context is the NORMAL continuity entry point
  - contextual_focus is bounded inherited working awareness, not truth
  - agents follow pointers for deeper detail (not bulk-list categories)
  - recent_activity is for explicit factual-history investigation
  - contextual_candidates / contextual_selection /
    contextual_materialization are DIAGNOSTIC, NOT routine startup
"""

import re
import unittest

# Markdown emphasis and code spans are presentation, not meaning. The
# protocol is wrapped prose, so an assertion about the contract it
# teaches should not depend on where a line wrapped or whether a word
# is bolded/backticked. `_prose` normalises both away.
_EMPHASIS_RE = re.compile(r"\*\*|\*|`")


def _prose(text: str) -> str:
    """Return `text` as lowercase-normalised prose: line wraps
    collapsed, markdown emphasis and code-span markers removed.

    Used to assert the SEMANTICS the protocol teaches rather than one
    particular rendering of the words that teach it.
    """
    return re.sub(r"\s+", " ", _EMPHASIS_RE.sub("", text))


CANONICAL_SNIPPETS = (
    __import__("pathlib")
    .Path(__file__)
    .parent.parent.joinpath("MPM_AGENT_INTEGRATION_SNIPPETS.md")
)

CANONICAL_PROTOCOL = (
    __import__("pathlib")
    .Path(__file__)
    .parent.parent.joinpath("mpm-agent-protocol.md")
)


def _extract_block(text: str, begin: str, end: str) -> str:
    """Extract the substring between two markers, exclusive."""
    i = text.index(begin)
    j = text.index(end, i + len(begin))
    return text[i + len(begin):j]


def _all_canonical_blocks(text: str) -> list[tuple[str, str]]:
    """Find every (host_label, managed_block) inside the snippets file.

    Each copy-paste example contains one outer `MANAGED SECTION` (or
    `MANAGED BLOCK:mpm-hermes`) marker wrapping one inner `MANAGED BLOCK`
    pair. The behavioral contract lives in the inner block, so extract
    that.
    """
    outer_markers = [
        ("Claude Code", "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->",
            "<!-- END MPM-MANAGED SECTION:claude-code-instructions -->"),
        ("OpenCode", "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->",
            "<!-- END MPM-MANAGED SECTION:opencode-instructions -->"),
        ("Pi", "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->",
            "<!-- END MPM-MANAGED SECTION:pi-instructions -->"),
        ("Hermes", "<!-- BEGIN MPM MANAGED BLOCK:mpm-hermes -->",
            "<!-- END MPM MANAGED BLOCK:mpm-hermes -->"),
    ]
    blocks = []
    for host, outer_begin, outer_end in outer_markers:
        # Skip the description-backticks occurrence: find the FIRST
        # marker that is NOT preceded by a backtick (descriptions wrap
        # the marker in ``).
        idx = 0
        i = -1
        while True:
            cand = text.find(outer_begin, idx)
            if cand == -1:
                break
            # Check if this is the description (preceded by `` ` ``).
            prev = text[max(0, cand - 1):cand]
            if prev == "`":
                idx = cand + 1
                continue
            i = cand
            break
        if i == -1:
            continue
        j = text.find(outer_end, i + len(outer_begin))
        if j == -1:
            continue
        outer = text[i:j + len(outer_end)]
        ib = outer.find("<!-- BEGIN MPM MANAGED BLOCK -->")
        ie = outer.find("<!-- END MPM MANAGED BLOCK -->")
        if ib == -1 or ie == -1:
            continue
        blocks.append((host, outer[ib:ie + len("<!-- END MPM MANAGED BLOCK -->")]))
    return blocks


class ContinuityContract(unittest.TestCase):
    """Every maintained host integration must teach the canonical
    continuity contract in its managed block."""

    def setUp(self):
        self.text = CANONICAL_SNIPPETS.read_text(encoding="utf-8")
        self.blocks = _all_canonical_blocks(self.text)
        # At minimum: 4 host blocks (claude-code, opencode, pi, hermes).
        # The OpenClaw hosts inherit the universal canonical block.
        self.assertGreaterEqual(
            len(self.blocks), 4,
            f"expected ≥ 4 host blocks; got {len(self.blocks)}",
        )

    def test_each_block_present(self):
        # Spot-check that the 4 supported hosts with their own copy/paste
        # block appear. OpenClaw hosts inherit the universal canonical
        # block via the same prefix as Claude Code.
        hosts = {host for host, _ in self.blocks}
        for required in (
            "Claude Code", "OpenCode", "Pi", "Hermes",
        ):
            self.assertIn(
                required, hosts,
                f"missing copy/paste example for host: {required}",
            )

    def test_read_wake_context_described_as_continuity_entry_point(self):
        """Every block must teach wake orientation AND the
        read_wake_context recovery path.

        2026-09-29: the required phrase was the exact string
        "Wake is auto-injected on session start". The approved compact
        block states the same behavioural fact in rule 1 ("MPM wake
        context normally arrives at session start ... If you do not have
        it, fetch it through `mpm_context` action `read_wake_context` as
        a recovery path"), and adds the integration-failure signal that
        repeated absence is a host bug rather than normal operation.
        This asserts the behaviour, not one wording of it.
        """
        for host, block in self.blocks:
            with self.subTest(host=host):
                # The action name must still be present and reachable.
                self.assertIn(
                    "read_wake_context", block,
                    f"{host}: managed block must name the "
                    f"read_wake_context recovery path",
                )
                # Wake normally arrives at session start (orientation).
                normalized = re.sub(r"\s+", " ", block)
                self.assertTrue(
                    any(p in normalized for p in (
                        "Wake is auto-injected on session start",
                        "wake context normally arrives at session start",
                    )),
                    f"{host}: managed block must teach that wake context "
                    f"arrives at session start",
                )
                # Absent wake on an auto-injecting host is a defect, not
                # normal operation — the recovery path is for that case.
                self.assertTrue(
                    "recovery path" in normalized or "integration problem" in normalized,
                    f"{host}: managed block must frame read_wake_context as "
                    f"the recovery path and repeated absence as an "
                    f"integration problem",
                )

    def test_pointer_semantics_survive_as_rule_two(self):
        """2026-09-29 replacement for
        test_contextual_focus_described_in_every_block.

        The compact managed block no longer names `contextual_focus` —
        that term is wake-payload internals, not a behavioural decision
        the agent makes before tool selection. The BEHAVIOUR it taught
        is preserved verbatim as rule 2: wake context is bounded
        inherited awareness, not a ranking or a verdict, and pointers
        are followed when the summary is insufficient. This asserts that
        behaviour survives rendering into every host.
        """
        for host, block in self.blocks:
            with self.subTest(host=host):
                normalized = re.sub(r"\s+", " ", block)
                self.assertTrue(
                    "inherited awareness" in normalized
                    or "inherited working awareness" in normalized,
                    f"{host}: managed block must teach that wake context is "
                    f"bounded inherited awareness",
                )
                self.assertTrue(
                    "not a ranking or a verdict" in normalized
                    or "not a ranking" in normalized,
                    f"{host}: managed block must teach that wake context is "
                    f"not a ranking or a verdict",
                )
                # The staleness/supersession caveat that the old
                # contextual_focus wording carried.
                for concept in ("superseded", "hypothesis", "stale"):
                    self.assertIn(
                        concept, block,
                        f"{host}: managed block must teach that stored "
                        f"state may be outdated ({concept})",
                    )

    def test_pointer_following_taught_in_every_block(self):
        """Every block must tell the agent to follow pointers when
        detail is insufficient (not bulk-list substrate categories)."""
        for host, block in self.blocks:
            with self.subTest(host=host):
                self.assertIn(
                    "pointer", block,
                    f"{host}: managed block missing pointer-following guidance",
                )
                self.assertIn(
                    "follow", block.lower(),
                    f"{host}: managed block missing pointer-following "
                    "instruction",
                )

    def test_diagnostic_surface_distinction_not_in_permanent_prose(self):
        """2026-09-29 replacement for
        test_diagnostic_actions_marked_as_diagnostic.

        The distinction between the canonical `recent_activity` feed and
        the routing/diagnostic `contextual_*` actions has moved OUT of
        permanent managed prose and INTO the compact `mpm_context` tool
        description, which every host sees at tools/list time. That is
        the canonical, production-facing coverage:
        cmd/mpm-mcp/mpm_context_compact_distinction_test.go asserts the
        registered tool description names recent_activity as
        chronological/observational and the three contextual_* actions
        as diagnostic surfaces outside the normal session-start
        workflow.

        Requiring the term in every managed block would duplicate that
        coverage in five places and re-inflate the block with wake-
        pipeline internals the agent does not act on. This asserts the
        architectural boundary instead: permanent managed prose must
        not reintroduce the diagnostic-surface vocabulary.
        """
        for host, block in self.blocks:
            with self.subTest(host=host):
                for term in ("contextual_candidates",
                             "contextual_selection",
                             "contextual_materialization"):
                    self.assertNotIn(
                        term, block,
                        f"{host}: managed block must not carry "
                        f"{term} (owned by the mpm_context tool "
                        f"description, not permanent managed prose)",
                    )

    def test_no_legacy_pre_2e4_wake_instructions(self):
        """Every block must NOT include the stale pre-Stage-2E.4
        instruction pattern of telling agents to manually invoke
        contextual_candidates when wake does not surface what they
        need."""
        for host, block in self.blocks:
            with self.subTest(host=host):
                self.assertNotIn(
                    "When the wake projection does not surface",
                    block,
                    f"{host}: stale pre-2E.4 'wake projection does not "
                    "surface ... invoke contextual_candidates' wording remains",
                )

    def test_recent_activity_role_distinguished(self):
        """The contextual_focus guidance in every block must NOT
        conflate recent_activity (the chronological feed) with
        contextual_focus (the inherited working awareness).
        The recent_activity action itself is documented in the tool
        reference table at the bottom of the canonical source; the
        managed instruction blocks do not need to repeat it — they
        only need to avoid the conflation."""
        for host, block in self.blocks:
            with self.subTest(host=host):
                # The block must distinguish the two by name when it
                # mentions contextual_focus.
                self.assertNotIn(
                    "contextual_focus is recent_activity", block,
                    f"{host}: conflated contextual_focus with recent_activity",
                )


class SharedProtocolContract(unittest.TestCase):
    """The shared agent protocol document must teach the same
    contract and reference contextual_focus semantics."""

    def setUp(self):
        self.text = CANONICAL_PROTOCOL.read_text(encoding="utf-8")
        # Prose form: collapse line wraps and markdown emphasis so an
        # assertion about meaning is not broken by where the editor
        # wrapped a line or where an author placed a `**bold**` span.
        # The protocol is wrapped prose, so any assertion against raw
        # text is coupled to the current wrapping — which is why the
        # 2026-09-29 pass replaced the literal "diagnostic surface" and
        # "follow the pointer" checks with these semantic forms.
        self.prose = _prose(self.text)

    def test_protocol_teaches_continuity_contract(self):
        self.assertIn(
            "contextual_focus", self.text,
            "shared protocol missing contextual_focus section",
        )
        self.assertIn(
            "inherited working awareness", self.prose,
            "shared protocol missing 'inherited working awareness' phrasing",
        )

    def test_protocol_diagnostic_action_role(self):
        """The `contextual_*` actions must be taught as diagnostic
        inspection surfaces, explicitly not part of normal
        session-start routing.

        2026-09-29: this previously asserted the literal
        "diagnostic surface" against the raw file. The protocol says
        "**diagnostic** surfaces" (§1) and "The three `contextual_*`
        actions are diagnostic" (§1.2) — the markdown bold span breaks
        the literal, so the test failed against wording that teaches
        the contract correctly. Asserting the meaning rather than one
        rendering of it.
        """
        self.assertIn(
            "diagnostic surfaces for inspecting the routing pipeline",
            self.prose,
            "shared protocol must mark the contextual_* actions as "
            "diagnostic surfaces for inspecting the routing pipeline",
        )
        # All three diagnostic actions must be named, so the contract
        # cannot silently drop one of them.
        for action in ("contextual_candidates", "contextual_selection",
                       "contextual_materialization"):
            self.assertIn(
                action, self.prose,
                f"shared protocol must name {action} as a diagnostic action",
            )
        # §1.2: they are NOT part of the normal session-start workflow.
        self.assertIn(
            "they are not part of the normal session-start workflow",
            self.prose.lower(),
            "shared protocol must say the contextual_* actions are not "
            "part of the normal session-start workflow",
        )

    def test_protocol_pointer_following(self):
        """The protocol must teach following a pointer with the
        appropriate domain tool when a summary is insufficient.

        2026-09-29: this previously asserted the literal
        "follow the pointer" against the raw file. The protocol wraps
        that instruction across lines 46-47 ("...follow\\n  the
        pointer with the appropriate domain tool") and again at 75-76
        ("follow the\\n  `pointer` / `artifact_id`"), so the literal
        never appeared in the raw text even though both statements
        were present and correct. Assert the instruction's substance.
        """
        self.assertIn(
            "follow the pointer with the appropriate domain tool",
            self.prose,
            "shared protocol must teach following the pointer with the "
            "appropriate domain tool",
        )
        # The instruction is conditional on the summary being
        # insufficient, and names the pointer fields to act on.
        self.assertIn(
            "when detail is sufficient, proceed. when it is not, follow",
            self.prose.lower(),
            "shared protocol must teach that pointer-following is "
            "conditional on the bounded detail being insufficient",
        )
        for field in ("pointer", "artifact_id"):
            self.assertIn(
                field, self.prose,
                f"shared protocol pointer-following must name {field}",
            )
        # Guard against the opposite error: bulk-listing every
        # substrate category is explicitly not the contract.
        self.assertIn(
            "bulk listing every substrate category at session start is "
            "not the contract",
            self.prose.lower(),
            "shared protocol must rule out bulk-listing substrate "
            "categories as the alternative to pointer-following",
        )

    def test_protocol_degraded_focus_guidance(self):
        self.assertIn(
            "degraded", self.text,
            "shared protocol missing degraded-focus guidance",
        )
        self.assertIn(
            "Continue using the legacy wake context", self.text,
            "shared protocol missing 'continue using legacy wake context' "
            "instruction for degraded focus",
        )

    def test_protocol_no_stale_wake_invocation_candidates(self):
        """Shared protocol must NOT instruct agents to invoke
        contextual_candidates when the wake does not surface what
        they need."""
        self.assertNotIn(
            "invoke\n`mpm_context contextual_candidates`",
            self.text,
            "shared protocol has stale 'invoke contextual_candidates' "
            "wording for missing wake coverage",
        )


if __name__ == "__main__":
    unittest.main()
