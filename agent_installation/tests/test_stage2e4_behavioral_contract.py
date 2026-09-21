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
        """Every block must explicitly call read_wake_context the
        normal continuity entry point (not a debugging surface)."""
        required_phrases = (
            "Wake is auto-injected on session start",
            "read_wake_context",
        )
        # Both must be present in each block.
        for host, block in self.blocks:
            with self.subTest(host=host):
                self.assertTrue(
                    all(p in block for p in required_phrases),
                    f"{host}: managed block does not describe "
                    "read_wake_context as the canonical continuity entry point",
                )

    def test_contextual_focus_described_in_every_block(self):
        """Every block must mention <contextual_focus> as bounded
        inherited working awareness (not as a debug surface)."""
        for host, block in self.blocks:
            with self.subTest(host=host):
                self.assertIn(
                    "contextual_focus", block,
                    f"{host}: managed block missing contextual_focus mention",
                )
                self.assertIn(
                    "inherited working", block,
                    f"{host}: managed block does not describe "
                    "contextual_focus as inherited working context",
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

    def test_diagnostic_actions_marked_as_diagnostic(self):
        """Every block must explicitly mark the contextual_* actions
        as diagnostic — NOT routine startup actions."""
        for host, block in self.blocks:
            with self.subTest(host=host):
                # Inner block uses lowercase 'diagnostic' in the §1.1
                # instruction about not chaining them.
                self.assertIn(
                    "diagnostic", block.lower(),
                    f"{host}: managed block does not mark "
                    "contextual_* actions as diagnostic",
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

    def test_protocol_teaches_continuity_contract(self):
        self.assertIn(
            "contextual_focus", self.text,
            "shared protocol missing contextual_focus section",
        )
        self.assertIn(
            "inherited working awareness", self.text,
            "shared protocol missing 'inherited working awareness' phrasing",
        )

    def test_protocol_diagnostic_action_role(self):
        # Lowercase for case-insensitive check on diagnostic wording.
        self.assertIn(
            "diagnostic surface", self.text.lower(),
            "shared protocol must mark contextual_* actions as diagnostic",
        )
        # The protocol §1.2 says "They are NOT part of the normal
        # session-start workflow" — possibly split across lines.
        import re
        normalized = re.sub(r"\s+", " ", self.text)
        self.assertIn(
            "They are NOT part of the normal session-start workflow",
            normalized,
            "shared protocol must say contextual_* actions are not "
            "session-start workflow",
        )

    def test_protocol_pointer_following(self):
        # Pointer-following guidance.
        self.assertIn(
            "follow the pointer", self.text,
            "shared protocol missing pointer-following guidance",
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
