#!/usr/bin/env python3
"""
reviewer_bwrap.py — an OS-CONFINED sandbox for a REVIEW subagent.

WHY THIS EXISTS
---------------
The reviewer's isolation is enforced at the PERMISSION layer: the
production tools are denied by name, the binaries are refused at the
command position, and a live workspace cannot be opened with the read
tool.  That is a BARRIER, not a jail.  A permission rule is a pattern
match on a command string, and a route the rules do not name is still
a route.

The existing sandbox (`reviewer_sandbox.py`) is the second layer, and
it is an ENVIRONMENT layer: inside it `HOME`, `TMPDIR` and
`MPM_WORKSPACE` point at disposable directories, so a command that
gets through lands in the sandbox's own database.  That is not
filesystem confinement either — the process still sees the operator's
files, because an environment variable is a resolution rule, not a
boundary.

This script is the THIRD layer, and the first one that is a boundary:
bubblewrap builds a MOUNT NAMESPACE, so a path that is not bound does
not exist inside it.  The kernel refuses the access; there is no
pattern to get wrong.

WHAT IT DOES
------------
  1. `init` builds a disposable tree:
       source/repo/    - a COPY of the repository under review (the
                         operator's checkout is never the writable tree)
       home/           - the disposable HOME
       tmp/            - the disposable TMPDIR
       workspace/      - the disposable MPM_WORKSPACE
       findings/       - the FILESYSTEM scratchpad for the review
  2. `run` executes a command inside the namespace.
  3. `probe` asks the namespace what it can see.  It builds a
     DISPOSABLE sentinel tree and checks that the sentinel is
     INVISIBLE from inside — never the real substrate.

THE NAMESPACE
-------------
  * the runtime is bound READ-ONLY (/usr, /usr/local, and the
    merged-usr symlinks);
  * the reviewer's tree is bound READ-WRITE at a FIXED prefix
    (/sandbox), so the reviewer sees a tidy, disposable world;
  * EVERYTHING ELSE IS NOT BOUND AT ALL — so it does not exist inside
    the namespace.  This is the part a permission matcher cannot do:
    an unbound path is not refused, it is ABSENT;
  * /tmp inside the namespace is a PRIVATE tmpfs, so the host's /tmp
    is invisible too;
  * the environment is CLEARED and then set to the disposable values,
    so the operator's API keys and shell settings do not leak in;
  * the network is unshared, so an MCP endpoint or a remote service
    cannot be reached from inside;
  * every capability is dropped, and the namespace dies with its
    parent.

MEASURED
--------
  The sentinel experiment: the protected tree, the live runtime root
  and the operator's home are all INVISIBLE from inside; the
  disposable tree is present and writable; there is NO NETWORK; and
  an escape attempt finds nothing.  The Go toolchain, Python and Git
  all work inside, so a review can still run the test suites.

THE SANDBOX NEVER TOUCHES THE OPERATOR'S WORKSPACE
---------------------------------------------------
The script refuses to operate when its base resolves inside an MPM
runtime root or inside the operator's home.  It creates directories;
it never removes anything outside its base, and it never writes inside
the workspace it sandboxed.

USAGE
-----
    reviewer_bwrap.py --base DIR init --source REPO
    reviewer_bwrap.py --base DIR run -- go test ./...
    reviewer_bwrap.py --base DIR probe
"""

from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

BWRAP = "/usr/bin/bwrap"

# The runtime the sandbox needs.  Everything else is invisible.
RUNTIME_DIRS = (
    "/usr",
    "/usr/local",
    "/etc/alternatives",
    "/etc/ssl",
    "/etc/passwd",
    "/etc/group",
)

# The merged-usr symlinks a modern Ubuntu uses.
MERGED_SYMLINKS = {
    "/bin": "usr/bin",
    "/sbin": "usr/sbin",
    "/lib": "usr/lib",
    "/lib64": "usr/lib64",
}

# Where the reviewer's tree appears INSIDE the namespace.
PREFIX = "/sandbox"

# The names of the disposable directories, and their place inside.
TREE = ("repo", "home", "tmp", "workspace", "findings")

# The live markers a base directory must never resolve inside.
LIVE_MARKERS = (".mpm",)


class SandboxRefused(SystemExit):
    """The sandbox refused to operate.  Carries the reason."""


def refuse_inside(base: Path, why: str) -> None:
    """Refuse to operate on a path that is not a disposable tree."""
    resolved = base.resolve()
    if str(resolved) == "/" or resolved == Path.home().resolve():
        raise SandboxRefused(f"refusing to sandbox {resolved}: {why}")
    home = Path.home().resolve()
    protected = [home / ".mpm", home / ".config", home / ".projects", home / "src"]
    protected.append(home / "projects")
    for parent in protected:
        try:
            resolved.relative_to(parent)
        except ValueError:
            continue
        raise SandboxRefused(f"refusing to sandbox {resolved}: it is inside {parent}")
    for marker in LIVE_MARKERS:
        if marker in resolved.parts:
            raise SandboxRefused(
                f"refusing to sandbox {resolved}: it is inside a live runtime root"
            )


def build_tree(base: Path, source: Path | None) -> dict:
    """The disposable tree: a repo copy and the reviewer's scratch."""
    for name in ("home", "tmp", "workspace", "findings"):
        (base / name).mkdir(parents=True, exist_ok=True)
    if source is not None:
        source = Path(source).resolve()
        if not source.is_dir():
            raise SandboxRefused(f"the source {source} is not a directory")
        target = base / "source" / "repo"
        if not target.exists():
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copytree(
                source,
                target,
                symlinks=True,
                ignore=shutil.ignore_patterns(".git", "node_modules", "__pycache__"),
            )
    else:
        # The repository slot still has to EXIST: bubblewrap refuses to
        # bind a path that is not there, and the whole namespace fails
        # to start when one bind fails.
        (base / "source" / "repo").mkdir(parents=True, exist_ok=True)
    return {
        "base": base,
        "repo": base / "source" / "repo",
        "home": base / "home",
        "tmp": base / "tmp",
        "workspace": base / "workspace",
        "findings": base / "findings",
    }


def bwrap_args(tree: dict) -> list:
    """The bwrap argument list: a minimal, writable-only-where-needed root."""
    args = [BWRAP]
    for path in RUNTIME_DIRS:
        if os.path.exists(path):
            args += ["--ro-bind", path, path]
    for link, target in MERGED_SYMLINKS.items():
        if os.path.islink(link):
            args += ["--symlink", target, link]
    # The reviewer's own tree: read-write, at a fixed namespace prefix.
    for name in TREE:
        args += ["--bind", str(tree[name]), f"{PREFIX}/{name}"]
    # The kernel's pseudo-filesystems and a PRIVATE /tmp.
    args += ["--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp"]
    # The environment: disposable HOME, TMPDIR and MPM_WORKSPACE.
    # `--clearenv` comes FIRST: bwrap processes its arguments in order,
    # so clearing after setting would erase the very variables the
    # sandbox exists to isolate.
    args += ["--clearenv"]
    args += [
        "--setenv", "HOME", f"{PREFIX}/home",
        "--setenv", "TMPDIR", f"{PREFIX}/tmp",
        "--setenv", "MPM_WORKSPACE", f"{PREFIX}/workspace",
        "--setenv", "GOCACHE", f"{PREFIX}/tmp/gocache",
        "--setenv", "GOPATH", f"{PREFIX}/tmp/gopath",
        "--setenv", "PATH", "/usr/local/bin:/usr/local/go/bin:/usr/bin:/bin",
        "--setenv", "TERM", "dumb",
    ]
    # No network, no new privileges, no shared namespaces.
    args += [
        "--unshare-all",
        "--die-with-parent",
        "--new-session",
    ]
    return args


def run(tree: dict, command: list) -> int:
    args = bwrap_args(tree) + command
    result = subprocess.run(args, capture_output=True, text=True)
    sys.stdout.write(result.stdout)
    sys.stderr.write(result.stderr)
    return result.returncode


def sentinel_experiment(tree: dict) -> int:
    """Ask the sandbox what it can see.  Nothing real is touched."""
    protected = tree["base"] / "protected"
    protected.mkdir(parents=True, exist_ok=True)
    (protected / "sentinel.txt").write_text(
        "DISPOSABLE SENTINEL - not real data\n", encoding="utf-8"
    )
    (protected / "fake-mpm.db").write_bytes(b"DISPOSABLE SENTINEL DB\n")
    # The sentinel tree is NOT bound, so it must be absent inside.
    probe = [
        "/bin/sh",
        "-c",
        (
            'echo "--- what is visible at the root ---"; ls /; '
            'echo "--- the protected tree ---"; '
            'if [ -e "{p}" ]; then echo "REACHABLE (BAD): {p}"; '
            'else echo "INVISIBLE (GOOD): {p}"; fi; '
            'echo "--- the live runtime root ---"; '
            'if [ -e "{home}/.mpm" ]; then echo "REACHABLE (BAD): {home}/.mpm"; '
            'else echo "INVISIBLE (GOOD): {home}/.mpm"; fi; '
            'echo "--- the operator home ---"; '
            'if [ -d "{home}" ]; then echo "REACHABLE (BAD): {home}"; '
            'else echo "INVISIBLE (GOOD): {home}"; fi; '
            'echo "--- the disposable tree ---"; '
            'echo "HOME=$HOME"; echo "TMPDIR=$TMPDIR"; echo "MPM_WORKSPACE=$MPM_WORKSPACE"; '
            'ls {prefix}; '
            'touch "$MPM_WORKSPACE/written-by-the-reviewer" '
            '&& echo "WRITABLE (GOOD): the workspace"; '
            'echo "--- the network ---"; '
            'if getent hosts example.com >/dev/null 2>&1; then echo "RESOLVES (BAD)"; '
            'else echo "NO NETWORK (GOOD)"; fi; '
            'echo "--- an escape attempt ---"; '
            'if ls /home >/dev/null 2>&1; then echo "ESCAPED (BAD)"; '
            'else echo "NO ESCAPE (GOOD)"; fi'
        ).format(
            p=protected,
            home=str(Path.home()),
            prefix=PREFIX,
        ),
    ]
    print("--- the sandbox's view ---")
    code = run(tree, probe)
    print(f"--- the probe's exit code: {code} ---")
    return code


def main(argv: list | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="an OS-confined sandbox for a review subagent"
    )
    parser.add_argument("--base", default=None, help="the sandbox base directory")
    sub = parser.add_subparsers(dest="command", required=True)
    init = sub.add_parser("init", help="build the disposable tree")
    init.add_argument("--source", default=None, help="the repository to copy in")
    run_parser = sub.add_parser("run", help="run a command inside the sandbox")
    run_parser.add_argument("argv", nargs=argparse.REMAINDER)
    sub.add_parser("probe", help="the disposable sentinel experiment")
    args = parser.parse_args(argv)

    base = (
        Path(args.base)
        if args.base
        else Path(tempfile.mkdtemp(prefix="mpm-review-sbx-"))
    )
    refuse_inside(base, "it is not a disposable tree")
    base.mkdir(parents=True, exist_ok=True)
    tree = build_tree(base, getattr(args, "source", None))

    if args.command == "probe":
        return sentinel_experiment(tree)
    if args.command == "init":
        print(f"the sandbox base: {base}")
        for name in TREE:
            print(f"  {PREFIX}/{name}: {tree[name]}")
        return 0
    argv = list(args.argv)
    if argv and argv[0] == "--":
        argv = argv[1:]
    if not argv:
        parser.error("run needs a command")
    return run(tree, argv)


if __name__ == "__main__":
    sys.exit(main())
