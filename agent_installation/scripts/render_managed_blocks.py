#!/usr/bin/env python3
r"""
render_managed_blocks.py — Generate per-adapter managed-block template
snippets from the canonical MPM_AGENT_INTEGRATION_SNIPPETS.md source.

The canonical file is the single source of truth for the universal managed
block. This script:

  1. Reads the canonical source.
  2. Extracts the universal managed block (the LAST occurrence of
     <!-- BEGIN MPM MANAGED BLOCK --> ... <!-- END MPM MANAGED BLOCK -->
     in the file — that one is host-neutral; the earlier occurrences
     inside host-specific copy/paste example sections are the
     pre-rendered host forms).
  3. Renders the canonical block into each adapter's tool-namespace
     prefix by word-boundary substitution on the leading `mpm_` of
     each canonical tool name (so `mpm_handoff` becomes
     `mpm__mpm_handoff` for Claude and `mcp__mpm__mpm_handoff` for
     Hermes, while remaining `mpm_handoff` for OpenCode and Pi).
  4. Composes each adapter's full template snippet (host header +
     rendered managed block + host footer) and writes it to
     `<adapter>/templates/<file>.snippet`.
  5. Verifies byte-for-byte parity between each host's copy/paste
     example in the canonical source and the rendered output.

Usage (developer workflow):

    # Regenerate all adapter template snippets in place:
    python3 render_managed_blocks.py

    # Verify drift (no writes; render in memory and diff each adapter
    # template snippet + each copy/paste example):
    python3 render_managed_blocks.py --check

    # Render one adapter only (debugging):
    python3 render_managed_blocks.py --only mpm-opencode

    # Print the universal canonical block to stdout and exit:
    python3 render_managed_blocks.py --dump canonical

Fails closed on missing markers, drift, or unknown adapter names.
"""

from __future__ import annotations

import argparse
import difflib
import re
import sys
from pathlib import Path
from typing import Iterable


# ---------------------------------------------------------------------------
# Adapter specs — one entry per persistent-file host.
# ---------------------------------------------------------------------------
#
# `tool_prefix` is the host's transport-namespace applied as a prefix to
# each canonical tool name. `snippet_path` is where the rendered template
# is written (relative to the adapter directory). `copy_paste_outer_*` is
# the host-specific wrapper used in the copy/paste example section of the
# canonical source — used for byte-equivalence verification only. Header
# and footer are host-specific prose; they live OUTSIDE the canonical
# managed block.

ADAPTERS: list[dict] = [
    {
        "name": "mpm-claude-code",
        "tool_prefix": "mpm__",
        "snippet_path": "templates/CLAUDE.md.snippet",
        "copy_paste_outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->",
        "copy_paste_outer_end": "<!-- END MPM-MANAGED SECTION:claude-code-instructions -->",
        "header": (
            "# MPM (Memory Persistence Module) — Claude Code integration.\n"
            "\n"
            "> **Managed by the mpm-claude-code plugin.** Edit the canonical\n"
            "> managed block in\n"
            "> `~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md`,\n"
            "> not this file. The plugin's installer regenerates this section\n"
            "> on install; manual changes outside the managed-block markers\n"
            "> are preserved.\n"
        ),
        "footer": (
            "\n"
            "## Session-start hook (auto-injected wake)\n"
            "\n"
            "The `mpm-claude-code` installer wires a `SessionStart` hook\n"
            "into `~/.claude/settings.json` and materializes the hook\n"
            "command at `~/.claude/hooks/mpm-session-start`. The hook\n"
            "fetches wake context via `mpm__mpm_context` action\n"
            "`read_wake_context` and emits the required\n"
            "`hookSpecificOutput.additionalContext` JSON envelope on\n"
            "stdout — ClaudeCode injects the envelope's `additionalContext`\n"
            "into the system prompt **before** the first model turn.\n"
            "Plain-prose stdout is silently dropped.\n"
            "\n"
            "This managed section is **not** the dynamic wake delivery\n"
            "mechanism — it documents the protocol only. If the hook is\n"
            "missing or disabled, call `mpm__mpm_context` action\n"
            "`read_wake_context` manually at session start as a fallback.\n"
            "\n"
            "## Framework identification\n"
            "\n"
            "If MCP tools are unavailable for any reason, the CLI path also\n"
            "writes artifacts with provenance attribution matching the MCP\n"
            "path. Both paths go through `mpm call` and stamp identical\n"
            "`artifact_provenance` rows.\n"
        ),
    },
    {
        "name": "mpm-opencode",
        "tool_prefix": "",
        "snippet_path": "templates/AGENTS.md.snippet",
        "copy_paste_outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->",
        "copy_paste_outer_end": "<!-- END MPM-MANAGED SECTION:opencode-instructions -->",
        "header": (
            "> The mpm-opencode plugin handles session-start wake-context\n"
            "> injection automatically via its\n"
            "> `experimental.chat.system.transform` hook. This AGENTS.md\n"
            "> section adds the **behavioral** layer (handoff discipline,\n"
            "> persist-during-work, recovery) that the hook can't reasonably\n"
            "> inject. Edit the canonical managed block in\n"
            "> `~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md`,\n"
            "> not this section, for behavioral changes.\n"
        ),
        "footer": (
            "\n"
            "## OpenCode-specific notes\n"
            "\n"
            "The mpm-opencode plugin's `experimental.chat.system.transform`\n"
            "hook handles wake-context injection automatically — you do not\n"
            "need to call `read_wake_context` manually at session start.\n"
            "\n"
            "The plugin stamps provenance for every MPM call:\n"
            "`framework_name=opencode`, with `model_name` and\n"
            "`invocation_id` set per call.\n"
            "\n"
            "Host-specific recovery details (OpenCode plugin lifecycle,\n"
            "system-prompt transformation quirks) are documented in the\n"
            "plugin's README.\n"
        ),
    },
    {
        "name": "mpm-pi",
        "tool_prefix": "",
        "snippet_path": "templates/AGENTS.md.snippet",
        "copy_paste_outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->",
        "copy_paste_outer_end": "<!-- END MPM-MANAGED SECTION:pi-instructions -->",
        "header": (
            "> Pi loads AGENTS.md (or CLAUDE.md) at session start and\n"
            "> concatenates them into the system prompt (per pi docs:\n"
            "> \"What the model is told → AGENTS.md / CLAUDE.md / SYSTEM.md\").\n"
            "> Edit the canonical managed block in\n"
            "> `~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md`,\n"
            "> not this file, for behavioral changes.\n"
        ),
        "footer": (
            "\n"
            "## Pi-specific notes\n"
            "\n"
            "Pi searches for AGENTS.md in this order at session start:\n"
            "\n"
            "  1. `~/.pi/agent/AGENTS.md` (global, all projects)\n"
            "  2. AGENTS.md in any parent directory of cwd (walking up)\n"
            "  3. AGENTS.md in cwd (current project)\n"
            "\n"
            "The mpm-pi extension at\n"
            "`~/.mpm/agent_installation/mpm-pi/` registers the typed\n"
            "transport for MPM tools; this AGENTS.md adds the behavioral\n"
            "layer (wake, persist, handoff, recovery).\n"
            "\n"
            "Disable AGENTS.md loading with `--no-context-files` / `-nc`\n"
            "to bypass the MPM behavioral contract for a specific session.\n"
        ),
    },
    {
        "name": "mpm-hermes",
        "tool_prefix": "mcp__mpm__",
        "snippet_path": "templates/hermes.md.snippet",
        "copy_paste_outer_begin": "<!-- BEGIN MPM MANAGED BLOCK:mpm-hermes -->",
        "copy_paste_outer_end": "<!-- END MPM MANAGED BLOCK:mpm-hermes -->",
        "header": (
            "> Hermes uses `.hermes.md` (or `HERMES.md`) at the project\n"
            "> root as the persistent behavioral instruction surface — it\n"
            "> is concatenated into the system prompt at session start\n"
            "> alongside `~/.hermes/SOUL.md`. The persona stays in SOUL.md;\n"
            "> this file adds the MPM behavioral layer. Edit the canonical\n"
            "> managed block in\n"
            "> `~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md`,\n"
            "> not this file, for behavioral changes.\n"
        ),
        "footer": (
            "\n"
            "## Hermes-specific notes\n"
            "\n"
            "The `mpm` skill at `~/.hermes/skills/mpm/SKILL.md` covers\n"
            "operational surface (wiring, registry, prime directives). This\n"
            "`.hermes.md` covers the **behavioral** layer the skill does not\n"
            "own — the skill is loaded on task relevance, not at session\n"
            "start.\n"
            "\n"
            "Hermes does **not** have a native session-start\n"
            "wake-injection hook. The `.hermes.md` file is loaded by Hermes\n"
            "at session start, but it carries protocol guidance only —\n"
            "**not** the dynamic wake payload. The first agent turn must\n"
            "call `mcp__mpm__mpm_context` action `read_wake_context` to\n"
            "obtain wake (see protocol item 1 in the managed block above).\n"
            "On the other supported hosts (ClaudeCode, OpenClaw, OpenCode,\n"
            "Pi) the host fires the fetch automatically; Hermes is the\n"
            "exception.\n"
            "\n"
            "Hermes's `load_soul_md` reads the entire `SOUL.md` file with\n"
            "no managed-block convention; do not duplicate the MPM\n"
            "behavioral contract into SOUL.md. Persona stays in SOUL.md;\n"
            "behavioral contract lives here.\n"
            "\n"
            "Hermes does NOT enforce a managed-block convention (unlike\n"
            "Claude Code's `<!-- BEGIN/END MPM-MANAGED SECTION -->`\n"
            "markers). The installer uses this leading comment block as a\n"
            "stable anchor for re-install; the MPM managed block lives\n"
            "directly underneath.\n"
        ),
    },
    {
        "name": "mpm-memory-openclaw",
        "tool_prefix": "mcp__mpm__",
        "snippet_path": "templates/SOUL.md.snippet",
        "copy_paste_outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->",
        "copy_paste_outer_end": "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->",
        "header": (
            "> OpenClaw loads this section from `SOUL.md` at agent\n"
            "> bootstrap (per-agent workspace resolved from\n"
            "> `openclaw.json` → `agents.entries.<id>.workspace`). The\n"
            "> managed block is written by `mpm-memory-openclaw/install.sh`\n"
            "> on install and refreshed on reinstall. Persona and user\n"
            "> content outside the managed markers is preserved.\n"
            ">\n"
            "> Edit the canonical managed block in\n"
            "> `~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md`,\n"
            "> not this file, for behavioural changes.\n"
        ),
        "footer": (
            "\n"
            "## OpenClaw-specific notes\n"
            "\n"
            "### Three-layer integration\n"
            "\n"
            "OpenClaw MPM integration is three layered components, not\n"
            "substitutes:\n"
            "\n"
            "  - **persistent behavioural contract** — this SOUL.md\n"
            "    managed block. Tells the agent *how* MPM must be used\n"
            "    across the session.\n"
            "  - **dynamic session wake/context** — the\n"
            "    `mpm-memory-openclaw` plugin's\n"
            "    `session_start` → `agent_turn_prepare` hook chain\n"
            "    (returning `prependContext`). Tells the agent *what\n"
            "    context exists now*.\n"
            "  - **dynamic mode/persona routing** — the\n"
            "    `mpm-auto-mode-persona-openclaw` plugin's\n"
            "    `message:received` + `agent:bootstrap` hook chain.\n"
            "    Tells the agent the active mode/persona for this turn.\n"
            "\n"
            "Runtime wake injection is **not** a substitute for the\n"
            "persistent block. The two layers carry different content\n"
            "and answer different questions.\n"
            "\n"
            "### SOUL.md resolution\n"
            "\n"
            "The installer resolves the active agent's SOUL.md from\n"
            "`openclaw.json` → `agents.entries.<id>.workspace`. The\n"
            "default agent id is `main`; multi-agent installations\n"
            "resolve each entry's `workspace` independently. The\n"
            "managed block is written into `SOUL.md` at that path.\n"
            "\n"
            "Persona (808) and any other user content above or below\n"
            "the managed markers is preserved verbatim. The installer\n"
            "is idempotent: a second run leaves the file byte-stable\n"
            "outside the managed section, and refreshes only the\n"
            "managed section if the canonical block has changed.\n"
        ),
    },
]


# ---------------------------------------------------------------------------
# Block extraction and rendering.
# ---------------------------------------------------------------------------
#
# The canonical-source managed block uses bare canonical tool names
# (`mpm_handoff`, `mpm_memory`, etc.). The block's `<!-- BEGIN MPM MANAGED
# BLOCK -->` markers are universal (one per host-form copy/paste example, plus
# the host-neutral bare canonical at the end).

# Per-token regex on canonical tool REFERENCES (not bare mentions).
# Matches only `` `mpm_X` `` wrapped in backticks, capturing the
# `mpm_X` portion. Prose mentions of bare `mpm_X` outside backticks
# are intentionally NOT matched, so they cannot be transformed by
# accident.
#
# Why backtick-bounded rather than bare-word: the canonical source
# already wraps every tool reference in backticks (consistent with
# the file's prose style), and prose mentions like `mpm_decisions` /
# `mpm_lessons` in the wake-payload description are likewise in
# backticks. Requiring backticks is a stronger guarantee than the
# prior bare-form regex (which matched any `mpm_X` substring).
_MPM_TOKEN_RE = re.compile(r"`(mpm_[a-z]\w*)`")

# Sentence-tail boundary marker: a `.`, `?`, or `!` followed by
# whitespace, newline, or end-of-string. Used to bound the sentence
# containing a given `mpm_X` token.
_SENTENCE_END_RE = re.compile(r"[.!?](?:\s|$)")

# The keyword that flags an invocation reference: `mpm_X action Y`.
# Both singular (`action`) and plural (`actions`) are matched so
# listings like "actions `flush`, `read`, ..." are caught.
_ACTION_RE = re.compile(r"\baction(?:s)?\b")

# The universal managed-block markers (used both in canonical source and in
# the per-host copy/paste examples inside the source).
_BARE_BLOCK_RE = re.compile(
    r"<!-- BEGIN MPM MANAGED BLOCK -->\n.*?<!-- END MPM MANAGED BLOCK -->\n",
    re.DOTALL,
)

# Matches the bold imperative header of a numbered item in the canonical
# managed block, e.g. `1. **Wake on session start.**`. Captures the
# header text inside the bold markers.
_ITEM_HEADER_RE = re.compile(r"^\d+\.\s+\*\*([^*]+?)\*\*\.?\s*", re.MULTILINE)

# Matches the first `` `mpm_X` `` tool reference inside an item, used
# to pair each imperative header with its primary tool reference for
# prose rewriting checks (see render_for_host).
_ITEM_TOOL_RE = re.compile(r"`(mpm_[a-z]\w*)`")

# Matches an explicit action name in an item, used by render_for_host
# to distinguish invocation references from prose references: an item
# that says `<tool> action \`<name>\`` (e.g. `mpm_context` action
# `write_handoff`) is an invocation, so its tool token receives the
# host's transport prefix. An item that mentions a tool in prose only
# (no `action` keyword in the same sentence) keeps the bare token.
# The original "primer bullet" wording is historical; the regex is
# still correct under that definition and the primer no longer exists.
_ITEM_ACTION_RE = re.compile(
    r"`(mpm_[a-z]\w*)`\s+action\s+`([a-z][a-z_]*[a-z])`"
)


# ---------------------------------------------------------------------------

def render_for_host(block: str, prefix: str) -> str:
    """Return the canonical block rendered for one host.

    The renderer applies the host's transport-namespace prefix only to
    `` `mpm_X` `` tokens that occur in `mpm_X action Y` invocation
    context within the same sentence (i.e., the sentence containing the
    token has the `action` keyword ahead of the next sentence
    boundary). Tool references that appear in prose — with no
    invocation keyword in the same sentence — are preserved (NOT
    rewritten) so the renderer does not accidentally transform names
    that happen to appear in explanatory prose.

    With an empty prefix (OpenCode, Pi), the result is byte-identical
    to the input block.
    """
    if not prefix:
        return block
    out: list[str] = []
    cursor = 0
    for m in _MPM_TOKEN_RE.finditer(block):
        sentence_end_m = _SENTENCE_END_RE.search(block, m.end())
        sentence_end = sentence_end_m.start() if sentence_end_m else len(block)
        if _ACTION_RE.search(block, m.end(), sentence_end):
            out.append(block[cursor:m.start()])
            out.append(f"`{prefix}{m.group(1)}`")
            cursor = m.end()
        # else: prose mention — leave the original `` `mpm_X` `` in place
        # by NOT advancing the cursor past it. The trailing
        # out.append(block[cursor:]) below carries it through verbatim.
    out.append(block[cursor:])
    return "".join(out)


def extract_canonical_block(text: str) -> str:
    """Return the host-neutral canonical managed block — the LAST
    occurrence of the universal managed-block markers in the
    canonical source. The earlier occurrences are the pre-rendered
    per-host copies in the copy/paste example section."""
    matches = list(_BARE_BLOCK_RE.finditer(text))
    if not matches:
        raise ValueError(
            "no canonical managed block found (missing <!-- BEGIN/END "
            "MPM MANAGED BLOCK --> markers)",
        )
    return matches[-1].group(0)


def extract_copy_paste_block(
    text: str, outer_begin: str, outer_end: str, adapter_name: str,
) -> str:
    """Return the managed-block content inside the host-specific
    copy/paste example for one host. Used for byte-equivalence
    verification only — the example is hand-written prose and the
    rendered canonical block must equal its inner managed block.

    The outer wrapper markers may also appear in surrounding prose
    (e.g., "the block must sit between `<!-- BEGIN ... -->` and
    `<!-- END ... -->`"). We skip prose matches that do not contain
    an inner managed block and return the first match that does.
    """
    pat = re.compile(re.escape(outer_begin) + r"(.*?)" + re.escape(outer_end),
                     re.DOTALL)
    for m in pat.finditer(text):
        section = m.group(1)
        inner = _BARE_BLOCK_RE.search(section)
        if inner:
            return inner.group(0)
    raise ValueError(
        f"no copy/paste example with an inner managed block found "
        f"for {adapter_name} (looked for {outer_begin!r} ... "
        f"{outer_end!r} wrapper containing "
        f"<!-- BEGIN/END MPM MANAGED BLOCK -->)",
    )


def compose_snippet(block: str, header: str, footer: str) -> str:
    parts = [header, block, footer]
    return "".join(p if p.endswith("\n") else p + "\n" for p in parts).rstrip("\n") + "\n"


def render_all(canonical_path: Path, adapter_root: Path) -> dict[str, str]:
    """Render every adapter's template snippet."""
    text = canonical_path.read_text(encoding="utf-8")
    block = extract_canonical_block(text)
    return {
        a["name"]: compose_snippet(
            render_for_host(block, a["tool_prefix"]), a["header"], a["footer"],
        )
        for a in ADAPTERS
    }


def copy_paste_rendered(canonical_path: Path) -> dict[str, str]:
    """Return the rendered canonical block for each host as it would
    appear inside that host's copy/paste example. Used for drift
    verification."""
    text = canonical_path.read_text(encoding="utf-8")
    block = extract_canonical_block(text)
    out = {}
    for a in ADAPTERS:
        rendered = render_for_host(block, a["tool_prefix"])
        out[a["name"]] = rendered
    return out


def adapter_dir(adapter_root: Path, adapter: dict) -> Path:
    return adapter_root / adapter["name"]


def snippet_path(adapter_root: Path, adapter: dict) -> Path:
    return adapter_dir(adapter_root, adapter) / adapter["snippet_path"]


def copy_paste_example_path(canonical_path: Path, adapter: dict) -> None:
    """The copy/paste example lives inside the canonical source
    file (it is not a separate file). Path is virtual; the caller
    has already loaded the file."""
    return None


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def _write_all(rendered: dict[str, str], adapter_root: Path) -> list[Path]:
    written = []
    for adapter in ADAPTERS:
        target = snippet_path(adapter_root, adapter)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(rendered[adapter["name"]], encoding="utf-8")
        written.append(target)
    return written


def _diff_snippets(rendered: dict[str, str], adapter_root: Path) -> list[str]:
    diffs = []
    for adapter in ADAPTERS:
        target = snippet_path(adapter_root, adapter)
        existing = target.read_text(encoding="utf-8") if target.is_file() else ""
        expected = rendered[adapter["name"]]
        if existing == expected:
            continue
        diff = "".join(
            difflib.unified_diff(
                existing.splitlines(keepends=True),
                expected.splitlines(keepends=True),
                fromfile=f"checked-in:{target.name}",
                tofile=f"rendered:{adapter['name']}/{adapter['snippet_path']}",
                n=3,
            )
        )
        diffs.append(f"--- DRIFT: {adapter['name']} ({target}) ---\n{diff}")
    return diffs


def _diff_copy_paste(canonical_path: Path) -> list[str]:
    """Verify each host's copy/paste example matches the
    transport-rendered version of the bare canonical block."""
    text = canonical_path.read_text(encoding="utf-8")
    rendered_for_host = copy_paste_rendered(canonical_path)
    diffs = []
    for adapter in ADAPTERS:
        actual = extract_copy_paste_block(
            text,
            adapter["copy_paste_outer_begin"],
            adapter["copy_paste_outer_end"],
            adapter["name"],
        )
        expected = rendered_for_host[adapter["name"]]
        if actual == expected:
            continue
        diff = "".join(
            difflib.unified_diff(
                actual.splitlines(keepends=True),
                expected.splitlines(keepends=True),
                fromfile=f"copy/paste example:{adapter['name']}",
                tofile=f"rendered canonical block:{adapter['name']}",
                n=3,
            )
        )
        diffs.append(
            f"--- COPY/PASTE DRIFT: {adapter['name']} ---\n{diff}",
        )
    return diffs


def _print_block_for_dump(canonical_path: Path) -> int:
    text = canonical_path.read_text(encoding="utf-8")
    block = extract_canonical_block(text)
    sys.stdout.write(block)
    return 0


def _select_adapters(only: str | None) -> list[dict]:
    if not only:
        return list(ADAPTERS)
    matches = [a for a in ADAPTERS if a["name"] == only]
    if not matches:
        raise SystemExit(
            f"unknown adapter: {only}; choices: " + ", ".join(a["name"] for a in ADAPTERS),
        )
    return matches


def main(argv: Iterable[str] | None = None) -> int:
    here = Path(__file__).resolve().parent
    agent_installation_root = here.parent
    canonical_path = agent_installation_root / "MPM_AGENT_INTEGRATION_SNIPPETS.md"

    ap = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0])
    ap.add_argument("--check", action="store_true",
                    help="Render in memory; diff against checked-in "
                         "snippets and copy/paste examples; exit 1 on drift.")
    ap.add_argument("--only", help="Render only the named adapter.")
    ap.add_argument("--dump", choices=("canonical",),
                    help="Print the universal canonical managed block to "
                         "stdout and exit.")
    ap.add_argument("--adapter-root", type=Path,
                    default=agent_installation_root,
                    help="Path to agent_installation/ (default: alongside "
                         "this script).")
    args = ap.parse_args(list(argv) if argv is not None else None)

    if args.dump == "canonical":
        return _print_block_for_dump(canonical_path)

    selected = _select_adapters(args.only)
    global ADAPTERS
    original_adapters = ADAPTERS
    ADAPTERS = selected

    try:
        rendered = render_all(canonical_path, args.adapter_root)

        if args.check:
            diffs = _diff_snippets(rendered, args.adapter_root)
            cpdiffs = _diff_copy_paste(canonical_path)
            all_diffs = diffs + cpdiffs
            if all_diffs:
                sys.stderr.write("\n\n".join(all_diffs))
                sys.stderr.write(
                    f"\n\n[render_managed_blocks] {len(all_diffs)} drift(s) "
                    f"detected. Re-run without --check to regenerate.\n",
                )
                return 1
            print(
                f"[render_managed_blocks] {len(selected)} adapter(s) "
                f"in byte-for-byte parity with canonical source; "
                f"{len(ADAPTERS)} copy/paste example(s) in parity.",
            )
            return 0

        written = _write_all(rendered, args.adapter_root)
        for p in written:
            print(f"[render_managed_blocks] wrote {p}")
        # Also report copy/paste parity (informational only during write).
        cpdiffs = _diff_copy_paste(canonical_path)
        if cpdiffs:
            sys.stderr.write("\n\n".join(cpdiffs))
            sys.stderr.write(
                "\n[render_managed_blocks] WARNING: copy/paste examples "
                "diverged from canonical — edit them in the snippets file "
                "to match the rendered block.\n",
            )
        return 0
    finally:
        ADAPTERS = original_adapters


if __name__ == "__main__":
    sys.exit(main())
