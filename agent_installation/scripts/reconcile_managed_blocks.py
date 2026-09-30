"""
reconcile_managed_blocks.py — Generic managed-block reconciliation entry
point.

The root installer (Makefile target `refresh-installed`, root
`install.sh`) invokes this script. The script discovers every
adapter under `agent_installation/mpm-*` and reconciles its
installed managed-block file to the current canonical render via
the adapter's own Python installer.

Each adapter contributes a small `reconcile.json` manifest declaring
how to invoke its installer. The root installer remains
host-agnostic: it never names a specific adapter or host.

Safety boundary
---------------

MPM must never opt a machine into a host it does not already use.
Reconciling an adapter is safe only when that adapter's *integration*
is already installed on the machine. Two manifest fields express
this, and they are mutually exclusive:

  `opt_in: true`      The adapter is never reconciled automatically.
                       Used by adapters with no persistent managed
                       block (e.g. mpm-hermes, whose documented flow
                       is manual).

  `reconcile_if:      The adapter IS reconciled, but only once its
   {"any_of": [...]}`  integration is already present on the machine.
                       Each entry is a probe; the gate opens when ANY
                       entry matches. An entry is either a single
                       probe or an `{"all_of": [...]}` conjunction.
                       Probes are host-agnostic facts the manifest
                       names: filesystem existence, a JSON key read, a
                       literal substring read, or a conjunction of
                       those.

Why a conjunction is needed
---------------------------

"Is MPM reachable from this host?" and "has the operator already wired
MPM into this host?" are DIFFERENT facts, and only their conjunction
identifies a genuinely MPM-enabled host.

Reachability alone is far too permissive. MPM is installed on any
machine that has ANY MPM integration, so a probe of the form "the
`mpm` binary exists" would be satisfied on a machine whose OpenClaw
has never heard of MPM — silently opting an unrelated OpenClaw into
MPM, which is exactly what this gate exists to prevent.

Conversely, intent alone is too permissive. A workspace that merely
mentions MPM in passing has not been integrated.

So a host counts as integrated when it can reach MPM *and* its own
persistent instruction surface already names MPM. Neither half is
sufficient alone.

OpenClaw is the worked example, and it has TWO supported integration
modes — the manifest gates on both, because a machine legitimately
running either one must have its managed block repaired:

  native plugin mode  `mpm-memory-openclaw` installed as an OpenClaw
                      plugin, providing typed `mpm_memory_*` tools and
                      runtime wake injection.
  CLI fallback mode   no MPM plugin, but the agent reaches MPM through
                      `mpm call <tool>` over its shell tool, which the
                      managed block itself documents. This is a
                      supported mode, not a degraded one: the managed
                      block is what teaches the agent to use the
                      fallback, so on a CLI-fallback host a missing
                      block means the agent never learns MPM is
                      reachable at all.

Probe-only mode detection lives in the manifest, never in this file,
so no host-specific knowledge enters the generic reconciler.

When `reconcile_if` is present and NO probe matches, the adapter is
skipped as "integration not installed" — the machine is left exactly
as it was. MPM never creates the integration it would then have to
reconcile.

The contract:
  - Successful reconciliation converges installed managed blocks to
    the current canonical render.
  - User content outside the managed block is preserved.
  - An adapter is never reconciled unless its integration is already
    installed on this machine.
  - Failures in one adapter do NOT abort the others.

Exit code 0 on full success, non-zero if any adapter failed.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path


SCRIPT_DIR = Path(__file__).resolve().parent
AGENT_INSTALLATION_DIR = SCRIPT_DIR.parent
MANIFEST_NAME = "reconcile.json"


def discover_adapters():
    """Yield (adapter_dir, manifest_dict) for every adapter with a manifest."""
    for adapter_dir in sorted(AGENT_INSTALLATION_DIR.glob("mpm-*")):
        manifest_path = adapter_dir / MANIFEST_NAME
        if not manifest_path.is_file():
            continue
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        except json.JSONDecodeError as exc:
            print(
                f"[reconcile]  {adapter_dir.name}: SKIP "
                f"(malformed {MANIFEST_NAME}: {exc})"
            )
            continue
        yield adapter_dir, manifest


def expand_home(arg: str, home: str) -> str:
    """Expand leading ~ or ~/ in an arg to the actual home."""
    if arg == "~":
        return home
    if arg.startswith("~/") or arg.startswith("~/"):
        return os.path.join(home, arg[2:])
    return arg


def resolve_arg(arg, home: str, adapter_dir: Path):
    """Resolve an arg: expand ~ and make adapter-relative paths absolute.

    - Strings starting with "~/": expand to <home>/...
    - Strings starting with "@/": resolve relative to adapter_dir
    - Strings starting with "/": absolute, used as-is
    - Anything else: returned verbatim
    """
    if not isinstance(arg, str):
        return arg
    if arg.startswith("@/"):
        return str(adapter_dir / arg[2:])
    if arg.startswith("~"):
        return expand_home(arg, home)
    return arg


def probe_matches(probe, home: str) -> bool:
    """Evaluate one `reconcile_if` probe.

    A probe is a small host-agnostic object. Supported forms:

      {"path": "~/..."}                 -> that path exists
      {"path": "~/...", "kind": "dir"}  -> that path exists AND is a dir
      {"path": "~/...", "kind": "file"} -> that path exists AND is a file
      {"path": "~/...", "json_key": "a.b.c"}
                                        -> that file parses as JSON and the
                                           dotted key path is present and
                                           non-empty (also accepts
                                           `json_value` to require an
                                           exact scalar match)
      {"path": "~/...", "kind": "file",
       "contains": "MPM"}              -> that file contains the literal
                                          string. `contains` also accepts a
                                          list of literals, satisfied by any
                                          one of them.

    A probe may also be a CONJUNCTION:

      {"all_of": [probe, probe, ...]} -> every contained probe matches

    Conjunction exists because "this host can reach MPM" and "the operator
    already wired MPM into this host" are different facts, and only their
    conjunction identifies a host that is genuinely MPM-enabled. Reachability
    alone is far too loose: MPM is installed on any machine that has ANY MPM
    integration, so a bare "the mpm binary exists" probe would opt an
    unrelated OpenClaw into MPM. Requiring the host's own persistent
    instruction surface to already name MPM is what makes the gate safe in
    both directions.

    A `path` may also be resolved INDIRECTLY, for hosts whose integration
    surfaces live at a host-configured location rather than a fixed one:

      {"path_from": {"config": "~/.../openclaw.json",
                     "json_key": "agents.defaults.workspace",
                     "fallback": "~/.openclaw/workspace"},
       "kind": "dir"}

    This keeps host-specific path knowledge in the manifest instead of
    hard-coding one machine's layout in the generic evaluator.

    The `kind` key is optional; without it any existing filesystem
    node satisfies the probe. Keeping the probe vocabulary to
    filesystem existence, a plain JSON read, a literal substring
    read, and a config-derived path is deliberate: a probe must never
    have the side effect of creating the thing it is testing for, and
    must never shell out to a host CLI that may not be installed.
    """
    if not isinstance(probe, dict):
        return False

    all_of = probe.get("all_of")
    if all_of is not None:
        return bool(all_of) and all(probe_matches(p, home) for p in all_of)

    nested_any = probe.get("any_of")
    if nested_any is not None:
        return bool(nested_any) and any(probe_matches(p, home) for p in nested_any)

    indirect = probe.get("path_from")
    if indirect is not None:
        if not isinstance(indirect, dict):
            return False
        resolved = _resolve_indirect_path(indirect, home)
        if resolved is None:
            return False
    else:
        if "path" not in probe:
            return False
        resolved = Path(expand_home(str(probe["path"]), home))

    suffix = probe.get("suffix")
    if isinstance(suffix, str) and suffix:
        resolved = resolved / suffix.lstrip("/")

    contains = probe.get("contains")
    if contains is not None:
        if not resolved.is_file():
            return False
        try:
            text = resolved.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError):
            return False
        needles = [contains] if isinstance(contains, str) else list(contains)
        if not needles:
            return False
        return any(needle in text for needle in needles)

    json_key = probe.get("json_key")
    if json_key is not None:
        if not resolved.is_file():
            return False
        try:
            data = json.loads(resolved.read_text(encoding="utf-8"))
        except (json.JSONDecodeError, OSError, UnicodeDecodeError):
            # An unreadable or malformed host config cannot positively
            # prove the integration is installed. Fail closed.
            return False
        cursor = data
        for part in str(json_key).split("."):
            if not isinstance(cursor, dict) or part not in cursor:
                return False
            cursor = cursor[part]
        if isinstance(cursor, (dict, list, str)) and len(cursor) == 0:
            return False
        if cursor is None or cursor is False:
            return False
        expected = probe.get("json_value")
        if expected is not None and cursor != expected:
            return False
        return True

    kind = probe.get("kind")
    if kind == "dir":
        return resolved.is_dir()
    if kind == "file":
        return resolved.is_file()
    return resolved.exists()


def _resolve_indirect_path(spec: dict, home: str) -> Path | None:
    """Resolve a probe path indirectly, through a host config value.

    `spec` names a config file, a dotted key inside it, and an optional
    fallback path used when the config is absent, unreadable, or does not
    carry the key. A config value that names a workspace is expanded with
    `~` like any other probe path, so a config written as `~/ws` and one
    written as an absolute path behave identically.

    Returns None only when there is no usable path at all, which the
    caller treats as "probe does not match" — fail closed.
    """
    config = spec.get("config")
    if not isinstance(config, str):
        return None
    config_path = Path(expand_home(config, home))
    candidate: str | None = None
    json_key = spec.get("json_key")
    if config_path.is_file() and isinstance(json_key, str):
        try:
            data = json.loads(config_path.read_text(encoding="utf-8"))
        except (json.JSONDecodeError, OSError, UnicodeDecodeError):
            data = None
        if data is not None:
            cursor = data
            for part in json_key.split("."):
                if not isinstance(cursor, dict) or part not in cursor:
                    cursor = None
                    break
                cursor = cursor[part]
            if isinstance(cursor, str) and cursor:
                candidate = cursor
    if candidate is None:
        fallback = spec.get("fallback")
        if isinstance(fallback, str):
            candidate = fallback
    if candidate is None:
        return None
    return Path(expand_home(candidate, home))


def integration_installed(manifest: dict, home: str) -> tuple[bool, str]:
    """Return (installed, explanation) for an adapter's gate.

    Adapters with no `reconcile_if` gate are unconditionally eligible
    (the ordinary always-on hosts: Pi, Claude Code, OpenCode). Those
    hosts' managed blocks are MPM's own persistent instruction files
    and MPM is the party that maintains them.
    """
    gate = manifest.get("reconcile_if")
    if not isinstance(gate, dict):
        return True, "always-on adapter (no reconcile_if gate)"
    probes = gate.get("any_of") or []
    if not probes:
        # A gate with no probes can never open. Treat as closed rather
        # than as "always" — a typo must not silently opt a host in.
        return False, "reconcile_if gate declares no probes; treating as closed"
    for probe in probes:
        if probe_matches(probe, home):
            return True, f"integration detected via {_describe_probe(probe)}"
    rendered = ", ".join(_describe_probe(p) for p in probes)
    return False, f"integration not installed (no match among: {rendered})"


def _describe_probe(probe) -> str:
    """Short human label for a probe, used in gate explanations."""
    if not isinstance(probe, dict):
        return repr(probe)
    if probe.get("any_of") is not None:
        inner = ", ".join(_describe_probe(p) for p in probe["any_of"])
        return f"any_of({inner})"
    if probe.get("all_of") is not None:
        inner = ", ".join(_describe_probe(p) for p in probe["all_of"])
        return f"all_of({inner})"
    indirect = probe.get("path_from")
    if isinstance(indirect, dict):
        label = str(indirect.get("config"))
        if indirect.get("json_key"):
            label = f"{label}#{indirect['json_key']}"
    else:
        label = str(probe.get("path"))
    if isinstance(probe.get("suffix"), str) and probe["suffix"]:
        label = f"{label}{probe['suffix']}"
    if probe.get("json_key"):
        label = f"{label}#{probe['json_key']}"
    if probe.get("json_value") is not None:
        label = f"{label}=={probe['json_value']!r}"
    if probe.get("contains") is not None:
        label = f"{label}~{probe['contains']!r}"
    if probe.get("kind"):
        label = f"{label}[{probe['kind']}]"
    return label


def reconcile_one(adapter_dir: Path, manifest: dict, home: str) -> bool:
    """Reconcile one adapter. Returns True on success."""
    label = manifest.get("host_label", adapter_dir.name)
    if manifest.get("opt_in", False):
        reason = manifest.get("skip_reason", "opt-in adapter")
        print(f"[reconcile]  {label}: SKIP ({reason})")
        return True

    # Safety gate: an adapter whose integration is not already on this
    # machine is left alone. MPM never opts a machine into a host.
    installed, why = integration_installed(manifest, home)
    if not installed:
        print(f"[reconcile]  {label}: SKIP ({why})")
        return True

    installer_rel = manifest.get("installer")
    if not installer_rel:
        print(f"[reconcile]  {label}: SKIP (manifest has no 'installer')")
        return True

    installer_path = adapter_dir / installer_rel
    if not installer_path.is_file():
        print(
            f"[reconcile]  {label}: SKIP "
            f"(installer not found at {installer_path})"
        )
        return True

    installer_type = manifest.get("installer_type", "python")
    if installer_type not in ("python", "bash"):
        print(
            f"[reconcile]  {label}: SKIP "
            f"(unknown installer_type {installer_type!r})"
        )
        return True

    argv = []
    if installer_type == "python":
        argv.append(sys.executable)
    argv.append(str(installer_path))
    for raw in manifest.get("args", []):
        argv.append(resolve_arg(raw, home, adapter_dir))

    description = manifest.get("description", "")
    desc_suffix = f"  ({description})" if description else ""
    print(f"[reconcile]  {label}: invoking {installer_rel}{desc_suffix}")
    result = subprocess.run(argv, capture_output=True, text=True)
    if result.returncode == 0:
        # Surface installer stdout briefly so the operator sees the
        # no-op message; full output goes to the installer's own logs.
        stdout_tail = result.stdout.strip().splitlines()
        if stdout_tail:
            print(f"[reconcile]  {label}: ok — {stdout_tail[-1][:120]}")
        else:
            print(f"[reconcile]  {label}: ok")
        return True
    print(
        f"[reconcile]  {label}: FAIL (rc={result.returncode}); "
        f"stderr={result.stderr.strip()[:200]}"
    )
    return False


def main() -> int:
    ap = argparse.ArgumentParser(
        description="Reconcile installed managed blocks to canonical render.",
    )
    ap.add_argument(
        "--home",
        default=os.environ.get("HOME"),
        help="User HOME for installer args (default: $HOME)",
    )
    ap.add_argument(
        "--only",
        action="append",
        default=None,
        help="Restrict to a single adapter dir name (e.g. mpm-pi). Repeatable.",
    )
    args = ap.parse_args()

    discovered = list(discover_adapters())
    if args.only:
        only_set = set(args.only)
        discovered = [(d, m) for d, m in discovered if d.name in only_set]
        if not discovered:
            print(f"[reconcile]  no adapters matched --only {args.only!r}", file=sys.stderr)

    if not discovered:
        print("[reconcile]  no adapters with reconcile.json found", file=sys.stderr)
        return 0

    print(f"[reconcile]  discovering {len(discovered)} adapter(s)")
    rc = 0
    for adapter_dir, manifest in discovered:
        if not reconcile_one(adapter_dir, manifest, args.home):
            rc = 1
    return rc


if __name__ == "__main__":
    sys.exit(main())
