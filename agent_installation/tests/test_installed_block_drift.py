"""
test_installed_block_drift.py — Regression test for stale installed
managed blocks across all persistent-file hosts.

Background
----------

The render script `agent_installation/scripts/render_managed_blocks.py`
produces per-host template snippets from the canonical
`MPM_AGENT_INTEGRATION_SNIPPETS.md` source. Its `--check` mode asserts
byte-for-byte parity between:

  1. The rendered per-host snippet (output of the render script)
  2. The copy/paste example embedded in the canonical source

That parity check is a *renderer* invariant — it catches drift between
the canonical source and the checked-in snippets. It does NOT cover
the next hop in the chain: the snippet → installed-persistent-file
step that the per-host `install.sh` (or the `make refresh-installed`
target) performs.

History of the gap
------------------

The 2026-09-27 managed-instruction contract bump (1.0.0 -> 1.2.0)
expanded the canonical block from 7 to 11 numbered invariants. The
renderer was updated and the snippets were regenerated, but the
installed persistent files for three of the five supported hosts
(Pi, OpenCode, Claude Code) were not refreshed, because:

  - `make refresh-installed` is the only path that writes installed
    blocks, and it is invoked manually (or by the per-host
    `install.sh` on a clean install), never automatically.
  - The `mpm_integration_check.md` host-specific subsections (17.2 Pi,
    17.4 Claude, 17.5 OpenCode) verify marker presence only, not
    content parity against the canonical render.
  - `git pull` of the MPM substrate does not trigger a refresh.

The result: every host that had been installed before the contract
bump continued to load the older 7-section managed block. Operators
got no signal that their agents were running on stale behavioural
instructions.

What this test asserts
----------------------

For every persistent-file host (Pi, Claude Code, OpenCode, OpenClaw),
if the canonical install target file exists on the test machine, the
managed section between `<!-- BEGIN MPM-MANAGED SECTION:* -->` and
`<!-- END MPM-MANAGED SECTION:* -->` must byte-match what the
canonical render would produce today.

Hosts whose install target is absent are *skipped* — this test
intentionally does not assert presence. The question of whether a
host is installed at all is a separate concern from the question of
whether an installed block is current.

Running
-------

  cd ~/.mpm/agent_installation
  python3 -m unittest tests.test_installed_block_drift -v

Or via the project test target (`make test`, which already includes
this file alongside the existing renderer parity tests).

Reparing a drift
----------------

When this test fails, the per-host installed block has drifted from
the canonical render. To repair, run the canonical refresh path:

  make refresh-installed

This regenerates the snippets and re-runs each per-host installer in
idempotent mode; the installer takes a backup of the old block and
writes the current one in place, preserving any user content outside
the managed-section markers.
"""

from __future__ import annotations

import importlib.util
import os
import re
import sys
import unittest
from pathlib import Path


# Path derivation matches test_render_managed_blocks.py: derive from
# the test file location so the suite runs in CI, fresh clones, and
# alternate mounts without hardcoded paths.
AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
RENDER_SCRIPT = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"


# Host-by-host install target map. Each entry pairs the adapter name
# (as registered in render_managed_blocks.ADAPTERS) with the canonical
# install path the per-host installer writes to. The list deliberately
# covers ONLY persistent-file hosts; runtime-injection-only hosts
# (Hermes via its session-start hook, when the persistent file is
# absent) are out of scope for this test.
#
# Path resolution follows the same conventions as the per-host
# installers themselves (mpm-pi/scripts/install_agents_instructions.py
# uses Path.home() / ".pi" / "agent" / "AGENTS.md" for user scope; the
# mpm-claude-code installer uses Path.home() / ".claude" / "CLAUDE.md";
# the mpm-opencode installer uses Path.home() / ".config" / "opencode"
# / "AGENTS.md"; the mpm-memory-openclaw installer writes to the
# resolved agent workspace SOUL.md). All five paths are $HOME-anchored
# so the test runs in any user environment.
#
# Adding a new persistent-file host: append an entry here AND register
# the adapter in render_managed_blocks.ADAPTERS.
HOST_INSTALL_TARGETS: list[dict] = [
    {
        "host": "pi",
        "adapter": "mpm-pi",
        "filename": "AGENTS.md",
        "relative_to_home": ".pi/agent/AGENTS.md",
        "outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->",
        "outer_end": "<!-- END MPM-MANAGED SECTION:pi-instructions -->",
    },
    {
        "host": "claude_code",
        "adapter": "mpm-claude-code",
        "filename": "CLAUDE.md",
        "relative_to_home": ".claude/CLAUDE.md",
        "outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->",
        "outer_end": "<!-- END MPM-MANAGED SECTION:claude-code-instructions -->",
    },
    {
        "host": "opencode",
        "adapter": "mpm-opencode",
        "filename": "AGENTS.md",
        "relative_to_home": ".config/opencode/AGENTS.md",
        "outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->",
        "outer_end": "<!-- END MPM-MANAGED SECTION:opencode-instructions -->",
    },
    {
        "host": "openclaw",
        "adapter": "mpm-memory-openclaw",
        # OpenClaw resolves the SOUL.md path per-agent from
        # openclaw.json -> agents.entries.<id>.workspace. The default
        # location is $HOME/.openclaw/workspace/SOUL.md per the
        # canonical installer and INSTALL.md.
        "filename": "SOUL.md",
        "relative_to_home": ".openclaw/workspace/SOUL.md",
        "outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->",
        "outer_end": "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->",
    },
]


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


def _canonical_block_for_host(adapter_name: str) -> str:
    """Render the full per-host snippet using the render script's
    `compose_snippet` (canonical block + adapter tool-prefix + host
    header + host footer) and prepend/append the host's outer
    managed-section markers. This is exactly what the per-host
    installer would write into the install target today (modulo any
    user content outside the markers, which the installer
    preserves).

    Note: the snippet is the FULL host-specific composition
    (header + rendered block + footer), not just the bare
    universal canonical block. Hosts that wrap the universal
    block with their own host-specific text (notably Pi, which
    prepends the MPM-pi load-order context and appends the
    Pi-specific load-order notes) need that wrapping for
    byte-equality."""
    canonical_text = CANONICAL_SOURCE.read_text(encoding="utf-8")
    canonical_block = _render.extract_canonical_block(canonical_text)
    adapter = next(a for a in _render.ADAPTERS if a["name"] == adapter_name)
    rendered = _render.render_for_host(canonical_block, adapter["tool_prefix"])
    snippet = _render.compose_snippet(
        rendered, adapter["header"], adapter["footer"],
    )
    return f'{adapter["copy_paste_outer_begin"]}\n{snippet}{adapter["copy_paste_outer_end"]}\n'


def _extract_installed_block(text: str, outer_begin: str, outer_end: str) -> str | None:
    """Return the slice of `text` between the outer managed-section
    markers, or None if the markers are absent or only one is present
    (corrupted target — same rule the per-host installer applies)."""
    if (outer_begin in text) != (outer_end in text):
        return None
    begin = text.find(outer_begin)
    if begin == -1:
        return None
    end = text.find(outer_end, begin + len(outer_begin))
    if end == -1:
        return None
    end_line_end = text.find("\n", end)
    if end_line_end == -1:
        end_line_end = len(text)
    else:
        end_line_end += 1
    return text[begin:end_line_end]


class InstalledBlockDrift(unittest.TestCase):
    """For each persistent-file host whose install target exists, the
    installed managed section must byte-match the canonical render.

    Absence is *not* a failure (handled by separate presence tests).
    The per-host installer runs idempotently; the canonical refresh
    path (`make refresh-installed`) re-runs all five installers in
    sequence and the final `render_managed_blocks.py --check` asserts
    snippet parity."""

    def test_all_installed_blocks_match_canonical_render(self):
        failures: list[str] = []
        skips: list[str] = []

        for entry in HOST_INSTALL_TARGETS:
            install_path = Path.home() / entry["relative_to_home"]
            if not install_path.is_file():
                skips.append(
                    f"{entry['host']}: install target absent ({install_path}) — "
                    "skipping (presence is a separate concern)"
                )
                continue

            try:
                expected = _canonical_block_for_host(entry["adapter"])
            except StopIteration:
                failures.append(
                    f"{entry['host']}: adapter '{entry['adapter']}' not "
                    "registered in render_managed_blocks.ADAPTERS — "
                    "HOST_INSTALL_TARGETS is out of sync"
                )
                continue

            text = install_path.read_text(encoding="utf-8")
            installed = _extract_installed_block(
                text, entry["outer_begin"], entry["outer_end"],
            )
            if installed is None:
                # Block absent (or one-sided) at an existing install
                # target. This is a *presence* concern, not a
                # *currency* concern: the operator has not yet
                # installed the persistent managed block on this
                # host, or has uninstalled it. `make
                # refresh-installed` does NOT fix this; the
                # per-host install.sh does. Skip with a clear note
                # so the drift detector stays scoped to its named
                # invariant (byte-parity, when present).
                skips.append(
                    f"{entry['host']}: install target exists at "
                    f"{install_path} but managed-section markers are "
                    f"absent (BEGIN={entry['outer_begin'] in text}, "
                    f"END={entry['outer_end'] in text}) — "
                    "skipping drift check (run per-host install.sh to "
                    "add the managed block; this test asserts only "
                    "byte-parity when the block is present)"
                )
                continue

            if installed != expected:
                # Show the *first* divergent line plus a byte offset to
                # help the operator locate the drift. A full unified
                # diff would be too noisy for a test failure summary.
                expected_lines = expected.splitlines(keepends=True)
                installed_lines = installed.splitlines(keepends=True)
                first_diff = None
                for i, (a, b) in enumerate(zip(expected_lines, installed_lines)):
                    if a != b:
                        first_diff = (i, a, b)
                        break
                if first_diff is None:
                    # Length mismatch with same prefix.
                    first_diff = (
                        min(len(expected_lines), len(installed_lines)),
                        expected_lines[first_diff[0]] if first_diff[0] < len(expected_lines) else "<missing in expected>\n",
                        installed_lines[first_diff[0]] if first_diff[0] < len(installed_lines) else "<missing in installed>\n",
                    )
                line_no, exp, ins = first_diff
                failures.append(
                    f"{entry['host']}: installed managed block at "
                    f"{install_path} drifted from canonical render.\n"
                    f"  expected bytes: {len(expected)}\n"
                    f"  installed bytes: {len(installed)}\n"
                    f"  first divergent line: {line_no + 1}\n"
                    f"    expected: {exp.rstrip()[:200]}\n"
                    f"    installed: {ins.rstrip()[:200]}\n"
                    f"  repair: make refresh-installed"
                )

        if skips:
            for s in skips:
                sys.stderr.write(f"[skip] {s}\n")

        if failures:
            self.fail(
                "Installed managed blocks drifted from canonical render:\n  - "
                + "\n  - ".join(failures)
            )


if __name__ == "__main__":
    unittest.main(verbosity=2)
