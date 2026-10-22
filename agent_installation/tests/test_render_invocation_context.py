"""
test_render_invocation_context.py — The renderer must recognise an
invocation reference that names the action directly.

`render_for_host()` applies a host's transport-namespace prefix only to
`` `mpm_X` `` tokens that occur in an invocation context. The canonical
behavioural contract (contract version 3.0.0) names the action without
the word "action":

    Wake context may arrive automatically. If it does not, recover it
    through `mpm_context` using `read_wake_context`.

Under the previous rule that sentence is a *prose* mention, so Claude
Code would have been handed a bare `mpm_context` — a capability name
that is not a callable tool identifier on that host. The renderer must
therefore treat a backticked action identifier that follows the tool
token in the same sentence as an invocation signal.

These tests pin that rule, and pin the boundary cases that keep the
rule from over-matching:

  - a second *tool* name in the same sentence is not an action;
  - a backticked file path is not an action;
  - the pre-existing "`mpm_X` action `Y`" form still works;
  - a bare, unbacked mention is never rewritten.
"""

from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path


AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
RENDER_SCRIPT = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"


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


class ActionIdentifierIsAnInvocation(unittest.TestCase):
    """A backticked action identifier after the tool token marks an
    invocation reference, exactly as the word "action" does."""

    def test_action_identifier_without_the_action_keyword_is_prefixed(self):
        block = (
            "If it does not, recover it through `mpm_context` using "
            "`read_wake_context`.\n"
        )
        out = _render.render_for_host(block, "mpm__")
        self.assertIn("`mpm__mpm_context`", out)
        self.assertIn("`read_wake_context`", out)

    def test_action_identifier_with_the_action_keyword_is_prefixed(self):
        """The pre-existing "`mpm_X` action `Y`" form must keep working
        unchanged — this is a widening, not a replacement."""
        block = "Fetch it through `mpm_context` action `read_wake_context`.\n"
        out = _render.render_for_host(block, "mpm__")
        self.assertIn("`mpm__mpm_context`", out)

    def test_hermes_prefix_is_applied_to_the_same_form(self):
        block = (
            "If it does not, recover it through `mpm_context` using "
            "`read_wake_context`.\n"
        )
        out = _render.render_for_host(block, "mcp__mpm__")
        self.assertIn("`mcp__mpm__mpm_context`", out)

    def test_empty_prefix_is_identity(self):
        block = (
            "If it does not, recover it through `mpm_context` using "
            "`read_wake_context`.\n"
        )
        self.assertEqual(_render.render_for_host(block, ""), block)


class ProseMentionsAreNotInvocations(unittest.TestCase):
    """The widened rule must not turn ordinary prose into an
    invocation reference."""

    def test_a_second_tool_name_is_not_an_action_identifier(self):
        """"`mpm_context` and `mpm_memory` are available." names two
        tools in prose. The first is followed by a backticked token, but
        that token is a tool name, not an action of the first tool."""
        block = "The tools `mpm_context` and `mpm_memory` are available.\n"
        out = _render.render_for_host(block, "mpm__")
        self.assertIn("`mpm_context`", out)
        self.assertNotIn("`mpm__mpm_context`", out)

    def test_a_backticked_path_is_not_an_action_identifier(self):
        block = (
            "The `mpm_context` capability is documented in "
            "`~/.mpm/agent_installation/mpm-agent-protocol.md`.\n"
        )
        out = _render.render_for_host(block, "mpm__")
        self.assertIn("`mpm_context`", out)
        self.assertNotIn("`mpm__mpm_context`", out)

    def test_a_single_word_backticked_value_is_not_an_action_identifier(self):
        """A snake_case identifier is the action signal. A lone word is
        ambiguous — it may be a value, not an action name."""
        block = "Call `mpm_context` with `now`.\n"
        out = _render.render_for_host(block, "mpm__")
        self.assertIn("`mpm_context`", out)
        self.assertNotIn("`mpm__mpm_context`", out)

    def test_an_action_identifier_in_another_sentence_does_not_count(self):
        """The action must be named in the *same* sentence as the tool
        token. A later sentence must not turn a prose mention into an
        invocation."""
        block = (
            "The tools `mpm_context` and `mpm_memory` are available. "
            "Use `read_wake_context` to fetch wake.\n"
        )
        out = _render.render_for_host(block, "mpm__")
        self.assertIn("`mpm_context`", out)
        self.assertNotIn("`mpm__mpm_context`", out)

    def test_bare_unbacked_mention_is_never_rewritten(self):
        block = "Call mpm_context with read_wake_context.\n"
        out = _render.render_for_host(block, "mpm__")
        self.assertEqual(out, block)


if __name__ == "__main__":
    unittest.main()
