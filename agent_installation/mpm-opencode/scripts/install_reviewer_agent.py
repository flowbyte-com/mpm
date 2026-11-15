#!/usr/bin/env python3
"""
install_reviewer_agent.py — install the ISOLATED REVIEWER agent
definition for OpenCode.

WHY THIS EXISTS
---------------
A review subagent runs in a child session with the parent's tool
surface. On 2026-11-15 a review subagent wrote a memory into the
operator's LIVE MPM database through the `mpm_memory` tool, against an
explicit instruction not to touch it.

The fix is the PERMISSION layer: the reviewer agent definition denies
every MPM tool by name, and a denied tool is removed from the
reviewer's tool list entirely. The definition is only an isolation if it
is actually INSTALLED, which is what this script does.

WHAT IT DOES
------------
  1. Copies the canonical template
     (`agent_installation/mpm-opencode/templates/agents/mpm-reviewer.md`)
     into `~/.config/opencode/agents/mpm-reviewer.md`.
  2. Refuses to overwrite an existing file unless `--force` is given.
  3. Prints what it did, and what to do to make it take effect.

OpenCode watches its configuration directories and reloads agents after
a save, so an installed definition is live without a restart. `/reload`
forces it if the host is slow to notice.

USAGE
-----
    python3 install_reviewer_agent.py            # install or report
    python3 install_reviewer_agent.py --force    # overwrite an existing file
    python3 install_reviewer_agent.py --dry-run  # report only
    python3 install_reviewer_agent.py --target DIR
"""

from __future__ import annotations

import argparse
import shutil
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
TEMPLATE = HERE.parent / "templates" / "agents" / "mpm-reviewer.md"
DEFAULT_TARGET_DIR = Path.home() / ".config" / "opencode" / "agents"
AGENT_NAME = "mpm-reviewer.md"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Install the isolated reviewer agent for OpenCode.",
    )
    parser.add_argument(
        "--target",
        type=Path,
        default=DEFAULT_TARGET_DIR,
        help="the agents directory to install into (default: %(default)s)",
    )
    parser.add_argument(
        "--force",
        action="store_true",
        help="overwrite an existing definition",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="report what would happen, and change nothing",
    )
    args = parser.parse_args(argv)

    if not TEMPLATE.exists():
        print(f"the canonical template is missing: {TEMPLATE}", file=sys.stderr)
        return 1

    target = args.target.expanduser() / AGENT_NAME

    if target.exists() and not args.force:
        print(
            f"{target} already exists; it was NOT overwritten.\n"
            "Re-run with --force to replace it.",
            file=sys.stderr,
        )
        return 1

    if args.dry_run:
        print(f"would install {TEMPLATE} -> {target}")
        return 0

    try:
        args.target.expanduser().mkdir(parents=True, exist_ok=True)
        # Copy to a temporary name and rename, so a reader never sees a
        # half-written definition.
        staging = target.with_suffix(target.suffix + ".new")
        shutil.copyfile(TEMPLATE, staging)
        staging.replace(target)
    except OSError as exc:
        print(f"could not install the reviewer agent: {exc}", file=sys.stderr)
        return 1

    print(f"installed the isolated reviewer agent: {target}")
    print(
        "OpenCode reloads agents after a configuration change; run /reload "
        "if the host has not noticed yet.\n"
        "Launch it with:  subagent(agent=\"mpm-reviewer\", ...)"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
