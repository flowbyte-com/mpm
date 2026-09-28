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
host-agnostic: it never names a specific adapter. Adapter opt-in
adapters (e.g. mpm-memory-openclaw on this machine, where the
managed block is intentionally absent) ship with `opt_in: true` so
the generic pass leaves them alone.

The contract:
  - Successful reconciliation converges installed managed blocks to
    the current canonical render.
  - User content outside the managed block is preserved.
  - Opt-in adapters are NEVER auto-installed.
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


def reconcile_one(adapter_dir: Path, manifest: dict, home: str) -> bool:
    """Reconcile one adapter. Returns True on success."""
    label = manifest.get("host_label", adapter_dir.name)
    if manifest.get("opt_in", False):
        reason = manifest.get("skip_reason", "opt-in adapter")
        print(f"[reconcile]  {label}: SKIP ({reason})")
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
