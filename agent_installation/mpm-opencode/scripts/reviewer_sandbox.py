#!/usr/bin/env python3
"""
reviewer_sandbox.py — a disposable sandbox for a REVIEW subagent.

WHY THIS EXISTS
---------------
The reviewer's isolation is enforced at the PERMISSION layer: the
production MPM tools are denied by name, the MPM binaries are refused
at the command position, and a live MPM workspace cannot be opened with
the read tool.

That is a BARRIER, not a jail. The shell runs with the host user's
filesystem, process and network authority, and a permission rule is a
pattern match on a command string. A route the rules do not name is
still a route.

So the sandbox is the second layer, and it is the one that does not
depend on the pattern being complete: inside the sandbox, `HOME`,
`TMPDIR` and `MPM_WORKSPACE` all point at disposable directories. An
`mpm remember` that gets through lands in the sandbox's own database.
The operator's runtime root is not reachable from inside the sandbox,
because the substrate resolves its workspace from those variables
(`MPM_WORKSPACE` -> `$HOME/.mpm` -> the current directory).

WHAT IT DOES
------------
  1. Creates (or reuses) a sandbox directory holding four directories:
       home/       - the disposable HOME
       tmp/        - the disposable TMPDIR
       workspace/  - the disposable MPM_WORKSPACE
       findings/   - the FILESYSTEM scratchpad for the review
  2. `run` executes a command with those variables set.
  3. `env` prints the `export` lines, so a caller can do:
       eval "$(reviewer_sandbox.py env)"

THE SANDBOX NEVER TOUCHES THE OPERATOR'S WORKSPACE
---------------------------------------------------
The base directory defaults to a directory under the system temporary
directory, and the script REFUSES to operate when the base resolves
inside an MPM runtime root. It creates directories; it never removes
anything outside the base, and it never writes inside the workspace it
sandboxed.

USAGE
-----
    reviewer_sandbox.py init                 # create it, print the paths
    reviewer_sandbox.py env                  # print the export lines
    reviewer_sandbox.py run -- go test ./... # run a command inside it
    reviewer_sandbox.py path                 # print the sandbox root
    reviewer_sandbox.py --base DIR ...       # choose another base
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
import tempfile
from pathlib import Path

# SANDBOX_DIRNAME is the directory the sandbox lives in, under the base.
SANDBOX_DIRNAME = "mpm-review-sandbox"

# LIVE_MARKERS are the path components that mark an MPM runtime root.
# A base that resolves through one of them is refused: the sandbox must
# never be created inside the thing it is meant to isolate.
LIVE_MARKERS = (".mpm",)


def _default_base() -> Path:
    """The directory the sandbox is created under."""
    override = os.environ.get("MPM_REVIEW_SANDBOX_BASE")
    if override:
        return Path(override).expanduser()
    return Path(tempfile.gettempdir()) / SANDBOX_DIRNAME


def _refuses_live_root(base: Path) -> str | None:
    """The reason this base is refused, or None when it is fine."""
    resolved = base.resolve()
    for part in resolved.parts:
        if part in LIVE_MARKERS:
            return (
                f"the sandbox base {resolved} is inside an MPM runtime root "
                f"(it contains {part!r}); refusing to create the sandbox there"
            )
    return None


def _sandbox_root(base: Path) -> Path:
    return base / SANDBOX_DIRNAME


def _ensure_sandbox(base: Path) -> dict[str, Path]:
    """Create the sandbox's directories, and return them."""
    root = _sandbox_root(base)
    directories = {
        "root": root,
        "home": root / "home",
        "tmp": root / "tmp",
        "workspace": root / "workspace",
        "findings": root / "findings",
    }
    for path in directories.values():
        path.mkdir(parents=True, exist_ok=True)
    return directories


def _environment(directories: dict[str, Path]) -> dict[str, str]:
    """The environment a sandboxed command runs with."""
    env = dict(os.environ)
    env["HOME"] = str(directories["home"])
    env["TMPDIR"] = str(directories["tmp"])
    env["MPM_WORKSPACE"] = str(directories["workspace"])
    # The marker lets a tool that cares report that it is sandboxed.
    env["MPM_REVIEW_SANDBOX"] = str(directories["root"])
    # The findings scratchpad is where a review belongs.
    env["MPM_REVIEW_FINDINGS"] = str(directories["findings"])
    return env


def _print_paths(directories: dict[str, Path]) -> None:
    for name in ("root", "home", "tmp", "workspace", "findings"):
        print(f"{name}={directories[name]}")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="A disposable sandbox for a review subagent.",
    )
    parser.add_argument(
        "--base",
        type=Path,
        default=None,
        help="the directory the sandbox is created under "
        "(default: $MPM_REVIEW_SANDBOX_BASE or a directory under the "
        "system temporary directory)",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("init", help="create the sandbox and print its paths")
    sub.add_parser("env", help="print the export lines for the sandbox")
    sub.add_parser("path", help="print the sandbox root")

    run = sub.add_parser("run", help="run a command inside the sandbox")
    run.add_argument("argv", nargs=argparse.REMAINDER,
                     help="the command, after `--`")

    args = parser.parse_args(argv)

    base = args.base.expanduser() if args.base else _default_base()
    refusal = _refuses_live_root(base)
    if refusal:
        print(refusal, file=sys.stderr)
        return 1

    if args.command == "path":
        print(_sandbox_root(base))
        return 0

    directories = _ensure_sandbox(base)

    if args.command == "init":
        _print_paths(directories)
        return 0

    if args.command == "env":
        env = _environment(directories)
        for key in ("HOME", "TMPDIR", "MPM_WORKSPACE", "MPM_REVIEW_FINDINGS"):
            print(f"export {key}={env[key]}")
        return 0

    if args.command == "run":
        command = list(args.argv)
        if command and command[0] == "--":
            command = command[1:]
        if not command:
            print("no command to run", file=sys.stderr)
            return 2
        return subprocess.call(command, env=_environment(directories))

    print(f"unknown command: {args.command}", file=sys.stderr)
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
