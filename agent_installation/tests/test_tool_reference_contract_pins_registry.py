"""
test_tool_reference_contract_pins_registry.py — the tool-reference
stability contract must agree with the live tool registry.

WHY THIS EXISTS
---------------
`agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` carries the
**tool-reference stability contract**: the table of per-tool action
enums and required params that an agent reads when it needs the
operational detail behind the always-on managed block. That table
claims to pin "the action enum values that agents see".

Nothing held it to that claim against the REGISTRY. The table was a
hand-maintained snapshot with a "verified against the live registry on
the date in this file's git log" note — a date that silently ages.
Other guards pinned parts of the table (the behaviour matrix, the
cross-adapter parity, the live behavioural verification), but none of
them compared the table's action lists to the registry's, so the drift
was invisible. It had accumulated:

  - `mpm_theories`, `mpm_evidence` and `mpm_confidence` had **no row
    at all**, although the managed block itself points an agent at
    `mpm_theories` action `propose`;
  - seven tools listed only a SUBSET of their live actions —
    `mpm_wakes` omitted `resolve` (the action that retires a wake),
    `mpm_work` omitted `archive`/`unarchive`, `mpm_decisions` omitted
    `show`/`list`/`query`, and so on;
  - every action the table DID name was still live, so the drift was
    invisible to a reader and to any test that only checked for stale
    names.

The drift is invisible in both directions: an agent reading the table
cannot tell that an action it needs is missing, and nothing fails when
one disappears.

WHAT THIS PINS
--------------
  1. Every tool the registry registers is DOCUMENTED in the contract —
     a table row, or a companion-standalone bullet for the tools with
     no action enum.
  2. Every tool the contract documents EXISTS in the registry — a row
     left behind for a retired tool sends an agent to a call that does
     not exist.
  3. For every documented tool with an action enum, the contract lists
     **exactly** the registry's actions: nothing stale (an action the
     registry no longer offers) and nothing missing (an action the
     registry offers but the contract does not name).
  4. The comparison is not vacuous: the registry must parse into a
     non-zero set of tools, and the contract must document a non-zero
     number of them.

THE REGISTRY IS THE AUTHORITY
------------------------------
`internal/core/tools/registry_list.go` is the single authoritative list
of every MPM tool — the same file the MCP server and `mpm call` both
iterate. The contract is derived from it, not the other way round, so a
schema change must refresh the table rather than the reverse.

This is a drift guard, not a behaviour test: it fails when the table and
the registry disagree. It never opens a database and never runs the
binary, so it is safe to run anywhere.

Run with:
    cd agent_installation && python3 -m unittest discover tests
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
CANONICAL = REPO_ROOT / "agent_installation" / "MPM_AGENT_INTEGRATION_SNIPPETS.md"
REGISTRY = REPO_ROOT / "internal" / "core" / "tools" / "registry_list.go"


def _registry_actions() -> dict[str, set[str] | None]:
    """Every tool the registry registers, with its action enum.

    ``None`` means the tool has no ``action`` enum at all (the pointer
    and blob tools take their own top-level fields).
    """
    src = REGISTRY.read_text(encoding="utf-8")
    out: dict[str, set[str] | None] = {}
    for m in re.finditer(r'\{\n\t\tName: "([^"]+)",', src):
        end = src.find("\n\t},\n", m.end())
        if end < 0:
            continue
        body = src[m.start():end]
        a = re.search(
            r'"action"\s*:\s*\{[^{}]*?"enum"\s*:\s*\[([^\]]*)\]', body, re.S
        )
        out[m.group(1)] = (
            set(re.findall(r'"([^"]+)"', a.group(1))) if a else None
        )
    return out


def _contract_section() -> str:
    """The tool-reference stability contract section of the canonical file."""
    text = CANONICAL.read_text(encoding="utf-8")
    start = text.index("# Tool-reference stability contract")
    end = text.index("# Maintenance", start)
    return text[start:end]


def _contract_rows() -> dict[str, set[str] | None]:
    """The contract's table rows: tool -> the actions it lists.

    ``None`` means the row says the tool has no action enum.

    Rows whose tool cell carries extra text — the
    ``mpm_memory`` action=``challenge`` companion row, whose second
    column lists PARAMETERS rather than actions — are not tool rows and
    are skipped.
    """
    section = _contract_section()
    rows: dict[str, set[str] | None] = {}
    for line in section.splitlines():
        if not line.startswith("|"):
            continue
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if len(cells) < 2:
            continue
        name_cell = cells[0]
        m = re.fullmatch(r"`(mpm_[a-z_]+)`", name_cell)
        if not m:
            continue  # a companion row, or a header
        name = m.group(1)
        action_cell = cells[1]
        if action_cell.startswith("(no "):
            rows[name] = None
            continue
        rows[name] = set(re.findall(r"`([a-z_]+)`", action_cell))
    return rows


def _companion_standalone_names() -> set[str]:
    """The tools the contract documents as companion standalones."""
    section = _contract_section()
    start = section.find("Companion standalones")
    if start < 0:
        return set()
    out: set[str] = set()
    for line in section[start:].splitlines():
        m = re.match(r"-\s*`(mpm_[a-z_]+)`", line.strip())
        if m:
            out.add(m.group(1))
    return out


class ToolReferenceContractPinsRegistry(unittest.TestCase):
    """The contract table and the live registry must agree."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.registry = _registry_actions()
        cls.rows = _contract_rows()
        cls.companions = _companion_standalone_names()

    def test_registry_parses(self) -> None:
        """The registry parse must not be vacuous."""
        self.assertGreaterEqual(
            len(self.registry), 20,
            f"only {len(self.registry)} tools parsed from {REGISTRY} — the "
            f"registry's shape changed and this guard would pass vacuously",
        )
        with_actions = [n for n, a in self.registry.items() if a]
        self.assertGreaterEqual(
            len(with_actions), 15,
            f"only {len(with_actions)} tools parsed with an action enum — "
            f"the registry's shape changed and this guard would pass "
            f"vacuously",
        )

    def test_every_registered_tool_is_documented(self) -> None:
        """A tool that exists must be documented in the contract."""
        documented = set(self.rows) | self.companions
        missing = sorted(n for n in self.registry if n not in documented)
        self.assertEqual(
            [], missing,
            "these tools are registered in "
            f"{REGISTRY.name} but have no row in the tool-reference "
            f"stability contract: {missing}. An agent reading the "
            f"contract cannot discover their actions.",
        )

    def test_every_documented_tool_exists(self) -> None:
        """A row must not name a tool the registry does not have.

        The mirror of the test above, and the half the action test
        cannot see: it iterates the registry, so a row left behind for
        a tool that was retired is never examined. Such a row sends an
        agent to a call that does not exist — the same failure mode as a
        missing row, in the opposite direction.
        """
        documented = set(self.rows) | self.companions
        stale = sorted(n for n in documented if n not in self.registry)
        self.assertEqual(
            [], stale,
            "the tool-reference stability contract names tools the "
            f"registry does not register: {stale}. A row for a retired "
            f"tool sends an agent to a call that does not exist.",
        )

    def test_documented_actions_match_the_registry_exactly(self) -> None:
        """A documented tool's action list must equal the registry's.

        Both directions matter. A stale action sends an agent to a call
        that does not exist; a missing action hides one that does.
        """
        problems: list[str] = []
        for name, live in sorted(self.registry.items()):
            if name not in self.rows:
                continue  # documented as a companion standalone instead
            listed = self.rows[name]
            if live is None:
                if listed is not None and listed:
                    problems.append(
                        f"{name}: the contract lists {sorted(listed)} but "
                        f"the registry gives the tool no action enum"
                    )
                continue
            if listed is None:
                problems.append(
                    f"{name}: the contract says the tool has no action "
                    f"enum, but the registry offers {sorted(live)}"
                )
                continue
            stale = sorted(listed - live)
            missing = sorted(live - listed)
            if stale:
                problems.append(
                    f"{name}: the contract names {stale}, which the "
                    f"registry does not offer (it offers {sorted(live)})"
                )
            if missing:
                problems.append(
                    f"{name}: the contract omits {missing}, which the "
                    f"registry offers (it lists {sorted(live)})"
                )
        self.assertEqual(
            [], problems,
            "the tool-reference stability contract has drifted from the "
            "live registry:\n  " + "\n  ".join(problems),
        )

    def test_contract_rows_are_not_vacuous(self) -> None:
        """The contract must actually document rows."""
        self.assertGreaterEqual(
            len(self.rows), 15,
            f"only {len(self.rows)} tool rows parsed from the contract — "
            f"the table's shape changed and this guard would pass "
            f"vacuously",
        )


if __name__ == "__main__":
    unittest.main()
