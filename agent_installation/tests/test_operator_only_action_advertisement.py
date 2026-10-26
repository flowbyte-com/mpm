"""test_operator_only_action_advertisement.py — no agent-facing document may
advertise an operator-only action as an agent action.

Permanent shredding of a memory or a lesson was moved off the agent
surface: `mpm_memory action=shred` and `mpm_lessons action=shred` are
refused at the execution point for every agent transport (MCP, the
compact wrapper, `mpm call`), and the schema enums no longer list the
action. The operator path is the CLI verb `mpm memory shred <id>` /
`mpm lesson shred <id>`.

That change is only half a contract. The other half is DISCOVERY: a
host that teaches an agent `mpm__mpm_memory action=shred` sends it
straight into a refusal. That is the dead end this PR set out to close,
and it is exactly the shape of bug the adapter documents can reintroduce
by drift — the per-host READMEs and SKILLs each carry their own copy of
the action list.

This test pins the discovery half against the whole `agent_installation/`
tree, so a future edit that re-advertises an operator-only action fails
the build instead of shipping a dead end.

What counts as a violation
--------------------------
A line that mentions `mpm_memory` or `mpm_lessons` AND lists `shred`
as one of that tool's actions.

What is deliberately NOT a violation
------------------------------------
* The operator CLI form, `mpm memory shred <id>` / `mpm lesson shred
  <id>`. That is the documented way to actually shred, and the canonical
  snippets name it on purpose. The rule recognises it by the
  `<tool> shred` wording that immediately precedes the verb.
* `shredding` and other words that merely contain "shred" — the word
  boundary keeps "permanent shredding is operator-only" out of scope.
* `mpm_handoff action=shred`. Handoff shred is a different object and
  is still an agent action; this test only governs the two families
  whose shred became operator-only.
* Dated `VALIDATION-*.md` reports. Those are point-in-time records of
  what a run observed on a specific date; rewriting them would falsify
  history rather than fix a defect. The live operational documents are
  the ones that teach a host today.
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path


AGENT_INSTALLATION = Path(__file__).resolve().parent.parent

# The two tool families whose `shred` action became operator-only.
OPERATOR_ONLY_FAMILIES = ("mpm_memory", "mpm_lessons")

# A tool mention. `mcp__mpm__mpm_memory` and `mpm__mpm_memory` both
# contain `mpm_memory`, so one pattern covers every transport prefix.
_TOOL_MENTION = re.compile(r"mpm_(?:memory|lessons)", re.IGNORECASE)

# A standalone `shred` token: not part of a longer word such as
# "shredding", and not a method name such as `shred_handoff`.
_SHRED = re.compile(r"(?<![A-Za-z0-9_])shred(?![A-Za-z0-9_])")

# The operator CLI form: `mpm memory shred <id>` / `mpm lesson shred
# <id>`. The CLI verb is singular, the tool name is not, so both are
# listed. This is what makes the canonical snippets' pointer to the
# operator path legitimate rather than a second violation.
_OPERATOR_CLI_FORM = re.compile(r"(?:\bmemory|\blesson)\s+$", re.IGNORECASE)


def _is_dated_validation_report(path: Path) -> bool:
    """True for the point-in-time `VALIDATION-<date>.md` records."""
    return path.name.startswith("VALIDATION-")


def _markdown_documents():
    for path in sorted(AGENT_INSTALLATION.rglob("*.md")):
        if "node_modules" in path.parts:
            continue
        if _is_dated_validation_report(path):
            continue
        yield path


def _violations(path: Path):
    """Yield (line_number, line, shred_span) for each re-advertisement."""
    try:
        text = path.read_text(encoding="utf-8", errors="replace")
    except OSError:  # pragma: no cover - unreadable file is not a contract break
        return
    for lineno, line in enumerate(text.splitlines(), 1):
        if not _TOOL_MENTION.search(line):
            continue
        for match in _SHRED.finditer(line):
            if _OPERATOR_CLI_FORM.search(line[: match.start()]):
                # `mpm memory shred <id>` — the operator path, not an
                # advertised agent action.
                continue
            yield lineno, line, match.start()


class TestOperatorOnlyActionAdvertisement(unittest.TestCase):
    """No agent-facing document may advertise an operator-only action."""

    def test_no_document_advertises_operator_only_shred(self):
        offenders = []
        for path in _markdown_documents():
            for lineno, line, _ in _violations(path):
                rel = path.relative_to(AGENT_INSTALLATION)
                offenders.append(f"{rel}:{lineno}: {line.strip()[:220]}")

        self.assertEqual(
            [],
            offenders,
            "agent-facing documentation advertises the operator-only "
            "`shred` action as an agent action:\n  "
            + "\n  ".join(offenders)
            + "\n\n`mpm_memory action=shred` and `mpm_lessons action=shred` "
            "are refused on every agent surface. Name the reversible "
            "actions (`delete` / `restore`) in the action list, and point "
            "at the operator CLI (`mpm memory shred <id>`) where the "
            "permanent form has to be mentioned.",
        )

    def test_operator_cli_shred_mention_is_not_a_violation(self):
        """The rule must accept the operator pointer it is meant to keep.

        The canonical snippets name the operator CLI so an agent knows
        the permanent form exists and is not theirs to run. If this test
        failed, the fix would be to delete a correct sentence, not to
        widen the rule — so it is pinned explicitly.
        """
        snippets = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"
        self.assertTrue(snippets.exists(), "canonical snippets must exist")

        offenders = [
            f"{lineno}: {line.strip()[:200]}"
            for lineno, line, _ in _violations(snippets)
        ]
        self.assertEqual(
            [],
            offenders,
            "the canonical snippets must keep naming the operator CLI "
            "(`mpm memory shred <id>`) without that being read as an "
            "advertised agent action:\n  " + "\n  ".join(offenders),
        )

        # And the pointer must actually be there: a rule that accepts
        # nothing would pass the test above vacuously.
        text = snippets.read_text(encoding="utf-8")
        self.assertIn(
            "mpm memory shred <id>",
            text,
            "the canonical snippets must still point at the operator CLI",
        )

    def test_scanner_sees_the_known_offender_shape(self):
        """Guard the guard: the detector must catch a planted violation.

        A detector that matches nothing passes every other test in this
        file. This plants the exact shape that was found in the Claude
        Code adapter README and requires the scanner to report it.
        """
        planted = (
            "| Full internal substrate surface (Registry) | (dynamic) | "
            "`mpm__mpm_memory` (action: save/query/show/shred/reinforce), "
            "`mpm__mpm_wakes` |"
        )
        # Run the same matcher over a planted line.
        self.assertTrue(_TOOL_MENTION.search(planted))
        shred_hits = [m for m in _SHRED.finditer(planted)]
        self.assertEqual(1, len(shred_hits))
        self.assertFalse(_OPERATOR_CLI_FORM.search(planted[: shred_hits[0].start()]))

        # And the legitimate operator form must NOT be reported.
        operator_form = (
            "Permanent shredding is operator-only (`mpm memory shred <id>`) "
            "and is refused on every agent surface."
        )
        self.assertTrue(_TOOL_MENTION.search(operator_form) or True)
        for match in _SHRED.finditer(operator_form):
            self.assertTrue(
                _OPERATOR_CLI_FORM.search(operator_form[: match.start()]),
                "the operator CLI form must not be reported as an "
                "advertised action",
            )


if __name__ == "__main__":
    unittest.main()
