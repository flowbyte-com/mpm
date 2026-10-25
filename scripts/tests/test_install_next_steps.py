"""The installer's "next steps" must advertise commands that exist.

Background
----------
`install.sh` ends by printing a short list of commands an operator should
run after the install. One of them used to be

    mpm call read_wake_context

`read_wake_context` is an *action* of the `mpm_context` tool, not a tool
name, so `mpm call read_wake_context` fails with

    unknown tool: read_wake_context. available: mpm_memory, ...

That is the first command a new user is told to run: a dead end at the
exact moment they are checking that the install worked. The canonical
form is the registry-backed tool call.

These tests pin the emitted text. The behavioural half (that the
canonical command returns a wake context) is covered by the fresh-install
acceptance run and by the CLI dispatcher tests.
"""

from __future__ import annotations

import re
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
INSTALL_SH = REPO_ROOT / "install.sh"


def _source() -> str:
    return INSTALL_SH.read_text(encoding="utf-8")


def test_next_steps_do_not_advertise_an_action_as_a_tool() -> None:
    src = _source()
    assert "call read_wake_context" not in src, (
        "install.sh advertises `mpm call read_wake_context`, but "
        "read_wake_context is an action of mpm_context, not a tool name; "
        "the command fails with 'unknown tool'."
    )


def test_next_steps_advertise_the_canonical_wake_context_call() -> None:
    src = _source()
    pattern = re.compile(
        r"call mpm_context --payload '\{\\\"action\\\":\\\"read_wake_context\\\"\}'"
    )
    assert pattern.search(src), (
        "install.sh no longer prints the canonical wake-context call; "
        "expected mpm call mpm_context --payload '{\"action\":\"read_wake_context\"}'"
    )
