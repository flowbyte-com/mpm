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
  ABSENT  The host's integration is not installed on this machine, so
          there is no managed block to be current or stale. This is an
          informational state, not a failure: MPM never opts a machine
          into a host it does not already use.
  ERROR   Installed file present, markers present, but the file is
          structurally malformed (BEGIN without matching END, or vice
          versa). The installer would refuse to touch this; an operator
          must inspect manually.

Integration-installed, block-absent is WARN, not ABSENT
-------------------------------------------------------

The managed behavioural block is a REQUIRED part of a functional host
integration. A host can technically expose the MPM tools while still
failing to use MPM reliably, purely because the persistent block is
absent. So when a host's integration is already installed on this
machine, a missing managed block is real drift that the operator
should fix, not a healthy intentional state.

  integration not installed            -> ABSENT  (informational)
  integration installed + block current -> PASS
  integration installed + block missing -> WARN
  integration installed + block stale   -> WARN

ABSENT is reserved for "this host is not part of this machine's MPM
setup". It is no longer reachable by simply having the install target
file present without a block.

Presence and currency are reported as separate diagnostics, per the
mpm_integration_check.md contract ("Keep presence and currency as
separate diagnostics").

Usage
-----

  python3 scripts/check_installed_managed_blocks.py
  python3 scripts/check_installed_managed_blocks.py --json

Exit code is 0 if every host is PASS or ABSENT, 1 if any WARN or
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
import re
import sys
from dataclasses import asdict, dataclass, field
from pathlib import Path


AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
RENDER_SCRIPT = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"
RECONCILE_SCRIPT = AGENT_INSTALLATION / "scripts" / "reconcile_managed_blocks.py"
CONVERGENCE_SCRIPT = AGENT_INSTALLATION / "scripts" / "managed_block_convergence.py"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"

if str(AGENT_INSTALLATION / "scripts") not in sys.path:
    sys.path.insert(0, str(AGENT_INSTALLATION / "scripts"))

import managed_block_convergence as convergence  # noqa: E402


# Host-by-host install target map. Each entry pairs the adapter name
# (as registered in render_managed_blocks.ADAPTERS) with the canonical
# install path the per-host installer writes to.
#
# `integration_probe` names the adapter's reconcile.json manifest. When
# the diagnostic finds one, it asks the reconciler whether that host's
# integration is already installed on this machine, and only then
# decides whether an absent managed block is drift (WARN) or a host
# this machine simply does not use (ABSENT). Hosts without a
# `integration_probe` (the always-on persistent-instruction hosts) are
# treated as installed whenever their install target exists.
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
        # OpenClaw's persistent managed block is REQUIRED once the
        # MPM OpenClaw integration is installed. Runtime wake
        # injection supplements it; it does not replace it.
        "integration_probe": "mpm-memory-openclaw",
    },
]


# Verdict vocabulary (in spirit of mpm_integration_check.md §24).
VERDICT_PASS = "PASS"
VERDICT_WARN = "WARN"
VERDICT_ABSENT = "ABSENT"
VERDICT_ERROR = "ERROR"


# Adapters whose installer wraps the rendered snippet in its own
# section banner rather than emitting bare outer markers. Maps adapter
# name -> installer module name under `<adapter>/scripts/`. Adapters
# absent from this map use the plain outer-marker shape.
SECTION_BUILDERS: dict[str, str] = {
    "mpm-memory-openclaw": "install_openclaw_instructions",
}

# Timestamp placeholder used when asking a bannered installer to build
# its expected section. Normalized away before comparison.
TS_SENTINEL = "__MPM_TS__"


@dataclass
class HostResult:
    host: str
    file: str  # "present" or "absent" — is the install-target file on disk?
    block: str  # "present" or "absent" — does the file contain a well-formed managed block?
    install_path: str
    verdict: str  # one of VERDICT_*
    detail: str
    repair: str  # "" when no repair needed

    # Marker-shape classification, reported separately from `block` so
    # an operator can see WHAT is in the file rather than only that it
    # failed. One of MARKER_* below; "" when the file is absent.
    marker_state: str = ""
    markers: list[str] = field(default_factory=list)

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


# Marker-shape vocabulary. This is deliberately finer-grained than
# `block` (present/absent): "some MPM text exists somewhere in the
# file" is NOT a pass condition, because the file can carry a foreign
# host's block, a legacy block, two blocks, or a block whose outer
# markers are truncated — all of which are drift even though a naive
# substring search would find MPM text.
MARKER_NONE = "none"                    # no MPM marker of any kind
MARKER_OWN = "own"                      # exactly this host's section
MARKER_FOREIGN = "foreign"              # another host's / a legacy section
MARKER_DUPLICATE = "duplicate"          # more than one MPM section
MARKER_BARE = "bare"                    # a behavioural contract with no wrapper
MARKER_MALFORMED = "malformed"          # markers present but not resolvable


def _load_render_module():
    spec = importlib.util.spec_from_file_location(
        "render_managed_blocks", RENDER_SCRIPT,
    )
    assert spec and spec.loader, "render script must be importable"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def _load_reconcile_module():
    spec = importlib.util.spec_from_file_location(
        "reconcile_managed_blocks", RECONCILE_SCRIPT,
    )
    assert spec and spec.loader, "reconcile script must be importable"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def _host_integration_installed(entry: dict, home: Path) -> bool:
    """Is this host's MPM integration already installed on `home`?

    Hosts that declare an `integration_probe` defer to the
    reconciler's single source of truth for "is this integration
    present", so the diagnostic and the reconciler can never disagree
    about whether a host is in scope.

    Hosts without a probe are the always-on persistent-instruction
    hosts; for those, "installed" simply means the install target is
    on disk.
    """
    adapter_name = entry.get("integration_probe")
    if not adapter_name:
        return (home / entry["relative_to_home"]).is_file()
    try:
        reconcile = _load_reconcile_module()
    except (OSError, ImportError, AttributeError):
        # Without the reconciler we cannot prove the integration is
        # present. Fail closed toward ABSENT rather than reporting
        # spurious drift on a host this machine may not use.
        return False
    manifest_path = AGENT_INSTALLATION / adapter_name / "reconcile.json"
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return False
    installed, _why = reconcile.integration_installed(manifest, str(home))
    return installed


def _canonical_block_for_host(_render, adapter_name: str) -> str:
    """Return the exact bytes this host's installer writes today.

    Two installer shapes exist in the repository:

      plain      outer markers + rendered snippet + outer markers
                 (claude-code, opencode, pi, hermes)

      bannered   the adapter's own installer wraps the snippet in a
                 section banner carrying a `<!-- generated: TS -->`
                 line and source comments (mpm-memory-openclaw).

    For the bannered shape we must ask the adapter's installer to
    build the section, or the diagnostic compares against bytes the
    installer never produces and the host can never reach PASS.
    """
    canonical_text = CANONICAL_SOURCE.read_text(encoding="utf-8")
    canonical_block = _render.extract_canonical_block(canonical_text)
    adapter = next(a for a in _render.ADAPTERS if a["name"] == adapter_name)
    rendered = _render.render_for_host(canonical_block, adapter["tool_prefix"])
    snippet = _render.compose_snippet(
        rendered, adapter["header"], adapter["footer"],
    )

    module_name = SECTION_BUILDERS.get(adapter_name)
    if module_name is None:
        return f'{adapter["copy_paste_outer_begin"]}\n{snippet}{adapter["copy_paste_outer_end"]}\n'

    installer = _load_adapter_installer(adapter_name, module_name)
    # Build with a sentinel timestamp; `_normalize_generated_ts` erases
    # it on both sides of the comparison so the preserved install
    # timestamp does not masquerade as drift.
    return installer.build_managed_section(snippet, existing_ts=TS_SENTINEL)


def _load_adapter_installer(adapter_name: str, module_name: str):
    path = (
        AGENT_INSTALLATION / adapter_name / "scripts" / f"{module_name}.py"
    )
    spec = importlib.util.spec_from_file_location(module_name, path)
    if not spec or not spec.loader:
        raise HostResultError(
            f"adapter '{adapter_name}' declares section builder "
            f"{module_name!r} but {path} is not importable",
        )
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class HostResultError(RuntimeError):
    """Raised when a host's expected-block construction cannot proceed."""


_GENERATED_TS_RE = re.compile(r"<!-- generated: [^\n]* -->")


def _normalize_generated_ts(text: str) -> str:
    """Erase the installer's `<!-- generated: ... -->` line.

    The installers preserve an existing managed section's timestamp on
    refresh so repeated installs are byte-stable. That makes the
    timestamp a per-install fact, not part of the canonical contract,
    so it must not be compared.
    """
    return _GENERATED_TS_RE.sub("<!-- generated: <TS> -->", text)


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


def _own_host_id(entry: dict, _render) -> str:
    """The marker id this host's installer owns, e.g.
    `openclaw-instructions`. Read from the renderer's adapter table so
    the diagnostic and the renderer can never disagree about it."""
    adapter = next(
        (a for a in _render.ADAPTERS if a["name"] == entry["adapter"]), None,
    )
    if adapter is None:
        return ""
    begin = adapter["copy_paste_outer_begin"]
    m = re.search(r"MPM[- ]MANAGED [A-Z]+:([A-Za-z0-9._-]+)\s*-->", begin)
    return m.group(1) if m else ""


def _host_has_mpm_mcp_server(entry: dict, home: Path) -> bool:
    """Does this host configure an MPM MCP server on this machine?

    True only when the host's own config actually names an MPM server.
    Host-agnostic by construction: each known MCP-client host keeps its
    servers in a small, documented JSON config under `$HOME`, and an
    absent or unreadable config is treated as "no server" — which is
    the safe direction, because a false negative only downgrades the
    unreachable-prefix error to ordinary drift.
    """
    probes: dict[str, list[str]] = {
        # Claude Code / Hermes: MCP servers in a host JSON/YAML config.
        "claude_code": [".claude/.mcp.json", ".hermes/config.yaml"],
        "hermes": [".hermes/config.yaml"],
        # OpenClaw: MCP servers under `mcp.servers` in openclaw.json.
        "openclaw": [".openclaw/openclaw.json"],
    }
    needles = ("mpm",)
    for rel in probes.get(entry["host"], []):
        path = home / rel
        if not path.is_file():
            continue
        try:
            text = path.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError):
            continue
        for key in ("mcpServers", "mcp.servers", "mcp_servers", "servers"):
            if key in text and any(n in text for n in needles):
                return True
    return False


def _unreachable_tool_prefixes(text: str, entry: dict, home: Path) -> list[str]:
    """Return MCP-transport tool prefixes used in the file's MPM
    behavioural contract that this host cannot actually resolve.

    A `mcp__mpm__`-style name is produced by an MCP *client* transport
    prefixing a server's tools. A host with no MPM MCP server has no
    such namespace, so the name is unreachable there regardless of how
    current the rest of the block is.
    """
    if _host_has_mpm_mcp_server(entry, home):
        return []
    analysis = convergence.analyze(text)
    spans: list[tuple[int, int]] = [(r.start, r.end) for r in analysis.regions]
    if not spans and analysis.inner_pairs:
        spans = [(s, e) for (s, e) in analysis.inner_pairs]
    if not spans:
        return []
    contract = "\n".join(text[s:e] for s, e in spans)
    return sorted(set(re.findall(r"\bmcp__\w+__", contract)))


def _classify_markers(text: str, entry: dict, _render) -> tuple[str, list[str]]:
    """Classify the file's MPM marker shape.

    Returns (marker_state, human-readable inventory). The classification
    is structural — it asks the convergence engine what regions the file
    actually contains — so the diagnostic and the installers agree on
    what counts as an MPM-owned region by construction.
    """
    markers = convergence.describe_markers(text)
    analysis = convergence.analyze(text)
    if analysis.notes:
        markers = markers + [f"note: {n}" for n in analysis.notes]
    if not markers:
        return MARKER_NONE, markers

    own = _own_host_id(entry, _render)

    if analysis.problems:
        return MARKER_MALFORMED, markers
    if len(analysis.regions) > 1:
        return MARKER_DUPLICATE, markers
    if not analysis.regions:
        # A balanced inner contract with no outer wrapper is still an
        # effective MPM behavioural contract, so it is not "none".
        return (MARKER_BARE if analysis.inner_pairs else MARKER_NONE), markers
    host_id = analysis.regions[0].host_id
    if host_id and own and host_id != own:
        return MARKER_FOREIGN, markers
    if host_id and not own:
        return MARKER_FOREIGN, markers
    return MARKER_OWN, markers


def _classify(entry: dict, _render, home: Path) -> HostResult:
    install_path = home / entry["relative_to_home"]
    integration_installed = _host_integration_installed(entry, home)

    if not install_path.is_file():
        # The install target is gone. Whether that is drift depends
        # entirely on whether the host's integration is installed.
        if integration_installed:
            return HostResult(
                host=entry["host"],
                file="absent",
                block="absent",
                install_path=str(install_path),
                verdict=VERDICT_WARN,
                detail=(
                    "integration is installed but the managed-block file is "
                    "missing; the persistent managed block is required for "
                    "reliable MPM use"
                ),
                repair=f"run {entry['adapter']}/install.sh",
            )
        return HostResult(
            host=entry["host"],
            file="absent",
            block="absent",
            install_path=str(install_path),
            verdict=VERDICT_ABSENT,
            detail="integration not installed on this machine",
            repair="",
        )

    text = install_path.read_text(encoding="utf-8")
    marker_state, markers = _classify_markers(text, entry, _render)

    # --- Structural verdicts, checked before currency -----------------
    # A file that carries two MPM sections, another host's section, a
    # bare contract, or unresolvable markers is drift EVEN IF some
    # substring of it happens to match this host's expected block. The
    # invariant is "at most one effective MPM behavioural contract", so
    # a duplicate is reported as such rather than being scored as a
    # currency mismatch.
    if marker_state == MARKER_DUPLICATE:
        analysis = convergence.analyze(text)
        ids = ", ".join(
            sorted({r.host_id or "<unsuffixed>" for r in analysis.regions})
        )
        return HostResult(
            host=entry["host"], file="present", block="present",
            install_path=str(install_path), verdict=VERDICT_ERROR,
            detail=(
                f"{len(analysis.regions)} MPM managed sections in one file "
                f"(marker ids: {ids}); at most one effective MPM behavioural "
                f"contract is allowed per instruction file"
            ),
            repair=f"run {entry['adapter']}/install.sh to converge to one",
            marker_state=marker_state, markers=markers,
        )

    if marker_state == MARKER_MALFORMED:
        analysis = convergence.analyze(text)
        # A marker problem the engine can resolve deterministically is
        # REPAIRABLE, and the installer will repair it. Calling that
        # "ambiguous" would send an operator to manually edit a file that
        # the normal refresh path fixes safely. Only genuinely
        # unresolvable problems are ERROR; a repairable one is WARN with
        # the repair path named.
        repairable = convergence.repair_extent_or_none(text) is not None
        return HostResult(
            host=entry["host"], file="present", block="absent",
            install_path=str(install_path),
            verdict=VERDICT_WARN if repairable else VERDICT_ERROR,
            detail="; ".join(analysis.problems) + (
                " (deterministically repairable: the inner behavioural "
                "contract is balanced, so the section's extent is provable)"
                if repairable else
                " (not deterministically repairable; the installer will "
                "refuse rather than risk deleting user content)"
            ),
            repair=(
                f"run {entry['adapter']}/install.sh to repair the section"
                if repairable else
                f"inspect {install_path} manually — the MPM markers are "
                f"ambiguous and the installer will refuse to modify the file"
            ),
            marker_state=marker_state, markers=markers,
        )

    if marker_state in (MARKER_FOREIGN, MARKER_BARE):
        analysis = convergence.analyze(text)
        found = (
            f"a foreign-host section (marker id "
            f"{analysis.regions[0].host_id!r})" if analysis.regions
            else "a behavioural contract with no outer managed-section wrapper"
        )
        return HostResult(
            host=entry["host"], file="present", block="absent",
            install_path=str(install_path), verdict=VERDICT_WARN,
            detail=(
                f"file carries {found} instead of this host's "
                f"{entry['outer_begin']!r} section; MPM text is present but "
                f"the contract is not owned by this host"
            ),
            repair=f"run {entry['adapter']}/install.sh to migrate it",
            marker_state=marker_state, markers=markers,
        )

    # --- Wrong render for the detected integration mode ---------------
    # Reachability problem, not a formatting one. A host that reaches
    # MPM only through `mpm call` has no `mcp__mpm__` namespace, so a
    # managed block that instructs the agent to call one is not merely
    # stale — it names a tool that cannot exist, and the agent is told
    # to prefer it over the fallback that does work. This is reported
    # separately from byte-drift because a host can be byte-current and
    # still unusable, and because the fix is a renderer change, not a
    # reinstall.
    unreachable_prefixes = _unreachable_tool_prefixes(text, entry, home)
    if unreachable_prefixes:
        return HostResult(
            host=entry["host"], file="present", block="present",
            install_path=str(install_path), verdict=VERDICT_ERROR,
            detail=(
                f"managed block references MCP-transport tool name(s) "
                f"{unreachable_prefixes} but this host has no MPM MCP "
                f"server; those tool identifiers do not exist here and the "
                f"agent will be sent to a call that is rejected"
            ),
            repair=(
                "re-render this host's snippet with a bare canonical tool "
                "prefix (see render_managed_blocks.ADAPTERS), then run "
                f"{entry['adapter']}/install.sh"
            ),
            marker_state=marker_state, markers=markers,
        )

    installed = _extract_installed_block(
        text, entry["outer_begin"], entry["outer_end"],
    )
    if installed is None:
        has_begin = entry["outer_begin"] in text
        has_end = entry["outer_end"] in text
        if has_begin and not has_end:
            detail = "BEGIN marker present, matching END marker absent"
            return HostResult(
                host=entry["host"],
                file="present",
                block="absent",
                install_path=str(install_path),
                verdict=VERDICT_ERROR,
                detail=detail,
                repair=(
                    f"inspect {install_path} manually — the installer refuses "
                    f"to touch a half-marked managed section"
                ),
            marker_state=marker_state, markers=markers,
        )
        if has_end and not has_begin:
            detail = "END marker present, matching BEGIN marker absent"
        else:
            detail = "managed-section markers absent"

        if integration_installed:
            # The core contract change: an installed host whose
            # persistent managed block is missing is DRIFT, not a
            # healthy intentional state. Tools may be exposed while
            # the agent still fails to use MPM reliably.
            return HostResult(
                host=entry["host"],
                file="present",
                block="absent",
                install_path=str(install_path),
                verdict=VERDICT_WARN,
                detail=(
                    f"{detail}; integration is installed, so the persistent "
                    f"managed block is required for reliable MPM use"
                ),
                repair=f"run {entry['adapter']}/install.sh",
            marker_state=marker_state, markers=markers,
        )
        return HostResult(
            host=entry["host"],
            file="present",
            block="absent",
            install_path=str(install_path),
            verdict=VERDICT_ABSENT,
            detail=f"{detail}; integration not installed on this machine",
            repair="",
            marker_state=marker_state, markers=markers,
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
            marker_state=marker_state, markers=markers,
        )
    except (HostResultError, OSError) as exc:
        return HostResult(
            host=entry["host"],
            file="present",
            block="present",
            install_path=str(install_path),
            verdict=VERDICT_ERROR,
            detail=f"could not construct the expected block: {exc}",
            repair="update the diagnostic's SECTION_BUILDERS map to match "
                   "the adapter installer",
            marker_state=marker_state, markers=markers,
        )

    # The preserved install timestamp is not part of the canonical
    # contract, so normalize it on both sides before comparing.
    installed_cmp = _normalize_generated_ts(installed)
    expected_cmp = _normalize_generated_ts(expected)

    if installed_cmp == expected_cmp:
        return HostResult(
            host=entry["host"],
            file="present",
            block="present",
            install_path=str(install_path),
            verdict=VERDICT_PASS,
            detail="installed block byte-matches canonical render",
            repair="",
            marker_state=marker_state, markers=markers,
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
            marker_state=marker_state, markers=markers,
        )


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0] if __doc__ else "")
    ap.add_argument("--json", action="store_true", help="emit machine-readable JSON")
    ap.add_argument("--home", default=None,
                    help="Home directory to inspect (default: $HOME). "
                         "Exposed so tests and sandboxed diagnostics can "
                         "point at a fixture tree without touching the "
                         "real installed files.")
    args = ap.parse_args()

    if not RENDER_SCRIPT.exists():
        print(f"error: render script missing: {RENDER_SCRIPT}", file=sys.stderr)
        return 2
    if not CANONICAL_SOURCE.exists():
        print(f"error: canonical source missing: {CANONICAL_SOURCE}", file=sys.stderr)
        return 2

    home = Path(args.home).expanduser() if args.home else Path.home()
    _render = _load_render_module()
    results = [_classify(entry, _render, home) for entry in HOST_INSTALL_TARGETS]

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
            if r.marker_state and r.marker_state != MARKER_NONE:
                print(f"           markers: {r.marker_state} — {', '.join(r.markers)}")
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
