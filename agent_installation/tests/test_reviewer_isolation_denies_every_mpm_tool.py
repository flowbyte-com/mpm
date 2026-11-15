"""
test_reviewer_isolation_denies_every_mpm_tool.py — a reviewer subagent
must not be able to reach the production MPM substrate.

WHY THIS EXISTS
---------------
A review subagent runs in a child session with the parent's tool
surface. On 2026-11-15 a review subagent wrote a memory into the
operator's LIVE MPM database through the `mpm_memory` tool, against an
explicit instruction not to touch it.

A written instruction was not enough, and could not have been: the tool
was still REGISTERED for the reviewer, so "do not call it" was the only
thing standing between a review note and a write into the operator's
substrate.

The fix is the PERMISSION layer. The reviewer agent definition
(`agent_installation/mpm-opencode/templates/agents/mpm-reviewer.md`)
denies every MPM tool by name, and a denied tool is removed from the
reviewer's tool list entirely - the reviewer cannot call it and cannot
see it.

WHAT THIS PINS
--------------
  1. Every tool the REGISTRY publishes is denied to the reviewer. The
     registry (`internal/core/tools/registry_list.go`) is the single
     authoritative list of every MPM tool, so a NEW tool cannot be
     added without the deny list being extended - the reviewer's
     isolation cannot quietly erode.
  2. The deny list has no STALE entries: a rule for a tool that no
     longer exists is dead weight that hides a real gap.
  3. The reviewer cannot EDIT - a reviewer that could change what it
     reviews is not a reviewer.
  4. The reviewer cannot reach the substrate from the COMMAND LINE
     either: `mpm call` and `mpm-memory` are denied shell patterns.
  5. The comparison is not vacuous: the registry must parse into a
     non-zero set of tools, and the reviewer must deny a non-zero
     number of them.

THE REGISTRY IS THE AUTHORITY
------------------------------
`internal/core/tools/registry_list.go` is the same list the MCP server
and `mpm call` both iterate. The PLUGIN registers a subset of it (it
omits the tools an agent reaches through `mpm call`), so the deny list
covers the registry's whole set: the plugin's subset by permission, and
the rest by the shell rules that close `mpm call`.
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
REGISTRY = REPO_ROOT / "internal" / "core" / "tools" / "registry_list.go"
REVIEWER = (
    REPO_ROOT
    / "agent_installation"
    / "mpm-opencode"
    / "templates"
    / "agents"
    / "mpm-reviewer.md"
)


def _registry_tools() -> set[str]:
    """The names the registry publishes."""
    source = REGISTRY.read_text(encoding="utf-8")
    return set(re.findall(r'Name:\s*"(mpm_[a-z_]+)"', source))


def _permission_rules() -> list[dict[str, str]]:
    """The reviewer's permission rules, parsed from its frontmatter.

    The frontmatter is a flat list of three-key mappings, so it is
    parsed directly rather than through a YAML dependency: the
    agent_installation suite runs on the standard library alone.
    """
    text = REVIEWER.read_text(encoding="utf-8")
    if not text.startswith("---\n"):
        raise AssertionError("the reviewer agent has no frontmatter")
    end = text.find("\n---\n", 4)
    if end < 0:
        raise AssertionError("the reviewer agent's frontmatter is unterminated")
    frontmatter = text[4:end]

    rules: list[dict[str, str]] = []
    current: dict[str, str] = {}
    for line in frontmatter.splitlines():
        item = re.match(r"^\s*-\s+(\w+):\s*(.+?)\s*$", line)
        if item:
            if current:
                rules.append(current)
            current = {item.group(1): item.group(2).strip('"')}
            continue
        field = re.match(r"^\s+(\w+):\s*(.+?)\s*$", line)
        if field and current:
            current[field.group(1)] = field.group(2).strip('"')
    if current:
        rules.append(current)
    return rules


def _denied_actions() -> set[str]:
    """The actions the reviewer denies."""
    return {
        rule["action"]
        for rule in _permission_rules()
        if rule.get("effect") == "deny"
    }


class ReviewerIsolationTest(unittest.TestCase):
    """The reviewer agent's isolation from the production substrate."""

    def test_registry_parses(self) -> None:
        """The registry must parse into a non-zero set of tools."""
        tools = _registry_tools()
        self.assertGreater(
            len(tools), 0, "the registry parsed into no tools; the comparison is vacuous"
        )

    def test_reviewer_denies_a_non_zero_number_of_tools(self) -> None:
        """The reviewer must deny a non-zero number of tools."""
        denied = _denied_actions()
        mpm_denied = {a for a in denied if a.startswith("mpm_")}
        self.assertGreater(
            len(mpm_denied),
            0,
            "the reviewer denies no MPM tool; the comparison is vacuous",
        )

    def test_every_registry_tool_is_denied(self) -> None:
        """Every tool the registry publishes is denied to the reviewer.

        THIS IS THE LOAD-BEARING ASSERTION. A tool added to the
        registry without a matching deny rule would be reachable from a
        reviewer, which is exactly the failure this guards.
        """
        tools = _registry_tools()
        denied = _denied_actions()
        missing = sorted(tool for tool in tools if tool not in denied)
        self.assertEqual(
            [],
            missing,
            "the reviewer does not deny these registry tools; add a deny rule "
            "for each to "
            "agent_installation/mpm-opencode/templates/agents/mpm-reviewer.md",
        )

    def test_the_deny_list_has_no_stale_entries(self) -> None:
        """A deny rule for a tool that no longer exists is dead weight."""
        tools = _registry_tools()
        denied = _denied_actions()
        stale = sorted(
            action
            for action in denied
            if action.startswith("mpm_") and action not in tools
        )
        self.assertEqual(
            [],
            stale,
            "the reviewer denies MPM tools the registry no longer publishes",
        )

    def test_the_reviewer_cannot_edit(self) -> None:
        """A reviewer that could change what it reviews is not a reviewer."""
        self.assertIn("edit", _denied_actions())

    def test_the_reviewer_cannot_spawn_agents(self) -> None:
        """A reviewer must not launch a further agent without the isolation."""
        self.assertIn("subagent", _denied_actions())

    def test_the_shell_cannot_reach_the_substrate(self) -> None:
        """The command line is the other way into the substrate."""
        rules = _permission_rules()
        shell_denials = {
            rule.get("resource", "")
            for rule in rules
            if rule.get("action") == "shell" and rule.get("effect") == "deny"
        }
        for pattern in ("*mpm call *", "*mpm-memory *"):
            self.assertIn(
                pattern,
                shell_denials,
                f"the shell rule {pattern!r} is missing; the reviewer could "
                "reach the substrate from the command line",
            )

    def test_the_template_is_the_one_that_is_installed(self) -> None:
        """The installer must ship THIS template, not a copy that can drift."""
        installer = (
            REPO_ROOT
            / "agent_installation"
            / "mpm-opencode"
            / "scripts"
            / "install_reviewer_agent.py"
        )
        self.assertTrue(
            installer.exists(),
            "the reviewer agent has no installer; a definition nobody installs "
            "is not an isolation",
        )
        source = installer.read_text(encoding="utf-8")
        self.assertIn(
            "templates/agents/mpm-reviewer.md",
            source,
            "the installer does not ship the canonical reviewer template",
        )


if __name__ == "__main__":
    unittest.main()
