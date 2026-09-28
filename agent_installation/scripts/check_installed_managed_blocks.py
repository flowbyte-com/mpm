#!/usr/bin/env python3
r"""
check_installed_managed_blocks.py — Diagnose installed managed blocks
across all persistent-file hosts.

This is the diagnostic complement to the per-host installers. It
distinguishes four states per host, in the spirit of the mpm
integration check's PASS / WARN / FAIL / ABSENT verdict vocabulary:

  PASS    Installed file present; managed-section block present; block
          byte-matches the current canonical render.
  WARN    Installed file present; managed-section block present; block
          does NOT byte-match the canonical render. The block is
          stale relative to the current canonical contract. The repair
          path is `make refresh-installed` (or the per-host install.sh
          called with the documented refresh semantics).
  ABSENT  Installed file absent (no install on this machine) OR the
          installed file is present but the managed-section markers
          are not present (operator has uninstalled the managed block
          or never installed one). This is a presence concern, not a
          currency concern. The repair path is the per-host
          install.sh.
  ERROR   Installed file present, markers present, but the file is
          structurally malformed (BEGIN without matching END, or vice
          versa). The installer would refuse to touch this; an operator
          must inspect manually.

Presence and currency are reported as separate diagnostics, per the
mpm_integration_check.md contract ("Keep presence and currency as
separate diagnostics").

Usage
-----

  python3 scripts/check_installed_managed_blocks.py
  python3 scripts/check_installed_managed_blocks.py --json

Exit code is 0 if every installed host is PASS, 1 if any WARN or
ERROR is observed, 2 if a tooling error occurred (e.g., missing
canonical source).

The script is read-only. It does not modify any installed file.
Repair is the operator's job; the per-host installer is the
canonical repair path.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import sys
from dataclasses import asdict, dataclass
from pathlib import Path


AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
RENDER_SCRIPT = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"


# Host-by-host install target map. Each entry pairs the adapter name
# (as registered in render_managed_blocks.ADAPTERS) with the canonical
# install path the per-host installer writes to. Persistent-file hosts
# only; runtime-injection-only hosts are out of scope (Hermes
# session_start hook when its persistent file is absent; OpenClaw
# without a managed block intentionally).
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
        "filename": "SOUL.md",
        "relative_to_home": ".openclaw/workspace/SOUL.md",
        "outer_begin": "<!-- BEGIN MPM-MANAGED SECTION:openclaw-instructions -->",
        "outer_end": "<!-- END MPM-MANAGED SECTION:openclaw-instructions -->",
    },
]


# Verdict vocabulary (in spirit of mpm_integration_check.md §24).
VERDICT_PASS = "PASS"
VERDICT_WARN = "WARN"
VERDICT_ABSENT = "ABSENT"
VERDICT_ERROR = "ERROR"


@dataclass
class HostResult:
    host: str
    file: str  # "present" or "absent" — is the install-target file on disk?
    block: str  # "present" or "absent" — does the file contain a well-formed managed block?
    install_path: str
    verdict: str  # one of VERDICT_*
    detail: str
    repair: str  # "" when no repair needed

    # Backwards-compat alias: older callers read `.presence`.
    # Mapping: file=present + block=present -> presence=present;
    #          file=present + block=absent   -> presence=present (block absent);
    #          file=absent                   -> presence=absent.
    # The new fields are unambiguous; `presence` is a deprecated shortcut.
    @property
    def presence(self) -> str:
        return self.file if self.block == "present" and self.file == "present" else (
            "absent" if self.file == "absent" else "present"
        )


def _load_render_module():
    spec = importlib.util.spec_from_file_location(
        "render_managed_blocks", RENDER_SCRIPT,
    )
    assert spec and spec.loader, "render script must be importable"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def _canonical_block_for_host(_render, adapter_name: str) -> str:
    canonical_text = CANONICAL_SOURCE.read_text(encoding="utf-8")
    canonical_block = _render.extract_canonical_block(canonical_text)
    adapter = next(a for a in _render.ADAPTERS if a["name"] == adapter_name)
    rendered = _render.render_for_host(canonical_block, adapter["tool_prefix"])
    snippet = _render.compose_snippet(
        rendered, adapter["header"], adapter["footer"],
    )
    return f'{adapter["copy_paste_outer_begin"]}\n{snippet}{adapter["copy_paste_outer_end"]}\n'


def _extract_installed_block(text: str, outer_begin: str, outer_end: str) -> str | None:
    has_begin = outer_begin in text
    has_end = outer_end in text
    if has_begin != has_end:
        return None
    if not has_begin:
        return None
    begin = text.find(outer_begin)
    end = text.find(outer_end, begin + len(outer_begin))
    if end == -1:
        return None
    end_line_end = text.find("\n", end)
    if end_line_end == -1:
        end_line_end = len(text)
    else:
        end_line_end += 1
    return text[begin:end_line_end]


def _classify(entry: dict, _render) -> HostResult:
    install_path = Path.home() / entry["relative_to_home"]
    if not install_path.is_file():
        return HostResult(
            host=entry["host"],
            file="absent",
            block="absent",
            install_path=str(install_path),
            verdict=VERDICT_ABSENT,
            detail="install target file does not exist",
            repair=(
                f"run {entry['adapter']}/install.sh to add the managed block"
            ),
        )

    text = install_path.read_text(encoding="utf-8")
    installed = _extract_installed_block(
        text, entry["outer_begin"], entry["outer_end"],
    )
    if installed is None:
        has_begin = entry["outer_begin"] in text
        has_end = entry["outer_end"] in text
        if has_begin and not has_end:
            detail = "BEGIN marker present, matching END marker absent"
        elif has_end and not has_begin:
            detail = "END marker present, matching BEGIN marker absent"
        else:
            detail = "managed-section markers absent"
        return HostResult(
            host=entry["host"],
            file="present",
            block="absent",
            install_path=str(install_path),
            verdict=VERDICT_ABSENT,
            detail=detail,
            repair=(
                f"run {entry['adapter']}/install.sh to add the managed block"
            ),
        )

    try:
        expected = _canonical_block_for_host(_render, entry["adapter"])
    except StopIteration:
        return HostResult(
            host=entry["host"],
            file="present",
            block="present",
            install_path=str(install_path),
            verdict=VERDICT_ERROR,
            detail=f"adapter '{entry['adapter']}' not registered in render_managed_blocks.ADAPTERS",
            repair="update HOST_INSTALL_TARGETS to match registered adapters",
        )

    if installed == expected:
        return HostResult(
            host=entry["host"],
            file="present",
            block="present",
            install_path=str(install_path),
            verdict=VERDICT_PASS,
            detail="installed block byte-matches canonical render",
            repair="",
        )

    # Currency mismatch. Show first divergent line for the operator.
    expected_lines = expected.splitlines(keepends=True)
    installed_lines = installed.splitlines(keepends=True)
    first_diff = None
    for i, (a, b) in enumerate(zip(expected_lines, installed_lines)):
        if a != b:
            first_diff = (i, a, b)
            break
    if first_diff is None:
        line_no = min(len(expected_lines), len(installed_lines))
        exp_repr = expected_lines[line_no] if line_no < len(expected_lines) else "<missing in expected>\n"
        ins_repr = installed_lines[line_no] if line_no < len(installed_lines) else "<missing in installed>\n"
    else:
        line_no, exp_repr, ins_repr = first_diff
    detail = (
        f"installed block drifted from canonical render: "
        f"expected {len(expected)} bytes vs installed {len(installed)} bytes; "
        f"first divergent line {line_no + 1}: "
        f"expected {exp_repr.rstrip()[:80]!r}, installed {ins_repr.rstrip()[:80]!r}"
    )
    return HostResult(
        host=entry["host"],
        file="present",
        block="present",
        install_path=str(install_path),
        verdict=VERDICT_WARN,
        detail=detail,
        repair="make refresh-installed",
    )


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0] if __doc__ else "")
    ap.add_argument("--json", action="store_true", help="emit machine-readable JSON")
    args = ap.parse_args()

    if not RENDER_SCRIPT.exists():
        print(f"error: render script missing: {RENDER_SCRIPT}", file=sys.stderr)
        return 2
    if not CANONICAL_SOURCE.exists():
        print(f"error: canonical source missing: {CANONICAL_SOURCE}", file=sys.stderr)
        return 2

    _render = _load_render_module()
    results = [_classify(entry, _render) for entry in HOST_INSTALL_TARGETS]

    if args.json:
        payload = []
        for r in results:
            d = asdict(r)
            # `presence` is a derived convenience field; surface it in
            # the JSON output too so consumers don't have to recompute.
            d["presence"] = r.presence
            payload.append(d)
        print(json.dumps(payload, indent=2))
    else:
        # Pretty-print: one line per host, with verdict, presence, and
        # first divergent line for WARN/ERROR. ABSENT hosts are
        # reported with a clear marker.
        verdict_glyph = {
            VERDICT_PASS: "  PASS  ",
            VERDICT_WARN: "  WARN  ",
            VERDICT_ABSENT: " ABSENT ",
            VERDICT_ERROR: "  ERROR ",
        }
        for r in results:
            glyph = verdict_glyph.get(r.verdict, r.verdict)
            line = (
                f"{glyph} {r.host:<10}  "
                f"file={r.file:<7}  block={r.block:<7}  {r.install_path}"
            )
            print(line)
            if r.detail:
                print(f"           {r.detail}")
            if r.repair:
                print(f"           repair: {r.repair}")
        # Summary line
        by_verdict = {v: sum(1 for r in results if r.verdict == v) for v in verdict_glyph}
        print()
        print(
            f"summary: PASS={by_verdict[VERDICT_PASS]} "
            f"WARN={by_verdict[VERDICT_WARN]} "
            f"ABSENT={by_verdict[VERDICT_ABSENT]} "
            f"ERROR={by_verdict[VERDICT_ERROR]}"
        )

    # Exit code: 0 if all PASS, 1 if any WARN/ERROR, 2 if ABSENT is the
    # only non-PASS state. ABSENT is informational (the host simply
    # has no managed block installed), not a failure.
    if any(r.verdict in (VERDICT_WARN, VERDICT_ERROR) for r in results):
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
