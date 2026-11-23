#!/usr/bin/env python3
"""
reviewer_bwrap.py — an OS-CONFINED sandbox for a REVIEW subagent.

WHY THIS EXISTS
---------------
The reviewer's isolation used to be enforced at the PERMISSION layer:
the production tools were denied by name, the binaries were refused at
the command position, and a live workspace could not be opened with the
read tool.  That is a BARRIER, not a jail.  A permission rule is a
pattern match on a command string, and a route the rules do not name is
still a route.

The ENVIRONMENT sandbox (`reviewer_sandbox.py`) is the second layer, and
it is an ENVIRONMENT layer: inside it `HOME`, `TMPDIR` and
`MPM_WORKSPACE` point at disposable directories, so a command that gets
through lands in the sandbox's own database.  That is not filesystem
confinement either — the process still sees the operator's files,
because an environment variable is a resolution rule, not a boundary.

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
  2. `exec` executes a command inside the namespace.  The command is read
     as a JSON ARRAY FROM STDIN, so it never reaches this script's own
     option parser — there is no wrapper option for it to reach.
  3. `run` is the same engine for a human at a terminal: the command
     follows a literal `--`.
  4. `probe` asks the namespace what it can see.  It builds a
     DISPOSABLE sentinel tree and checks that the sentinel is INVISIBLE
     from inside — never the real substrate.

THE NAMESPACE
-------------
  * the runtime is bound READ-ONLY (/usr, and the merged-usr symlinks);
  * the host's TOOLCHAIN, discovered on the host's own PATH, is bound
    READ-ONLY BY DESCRIPTOR as well — unless it is protected, and the
    protection covers the whole home, so no toolchain under the
    operator's home is ever bound;
  * `/etc` is a SYNTHETIC directory: a passwd and a group file that
    name the sandbox's own identity, and a copy of the host's
    `alternatives` symlinks.  The host's own `/etc` — and with it the
    trust store at `/etc/ssl`, the private keys under `/etc/ssl/private`
    and the operator's account database — is NOT bound;
  * the reviewer's tree is bound READ-WRITE at a FIXED prefix
    (/sandbox), so the reviewer sees a tidy, disposable world;
  * EVERYTHING else IS NOT BOUND AT ALL — so it does not exist inside
    the namespace.  This is the part a permission matcher cannot do: an
    unbound path is not refused, it is ABSENT;
  * /tmp inside the namespace is a PRIVATE tmpfs, so the host's /tmp
    is invisible too;
  * the environment is CLEARED and then set to the disposable values,
    so the operator's API keys and shell settings do not leak in;
  * the network is unshared, so an MCP endpoint or a remote service
    cannot be reached from inside;
  * every capability is dropped, nested user namespaces are refused,
    and the namespace dies with its parent.

THE BOUNDARY IS THE KERNEL'S
-----------------------------
The writable part of the tree is bound by FILE DESCRIPTOR, not by path.
Each directory is opened with `O_NOFOLLOW`, so a path that is swapped
for a symlink between the check and the mount cannot be mounted: the
descriptor names the inode, and the inode is the one that was checked.

THE SANDBOX NEVER TOUCHES THE OPERATOR'S WORKSPACE
---------------------------------------------------
The script refuses to operate when its base resolves inside an MPM
runtime root, inside the operator's home, inside a system directory, or
inside any directory the caller declares protected.  The protection
list comes from the PASSWD DATABASE, not from `$HOME`: an environment
variable is agent-influenceable, and a security authority the agent can
influence is not an authority.

It creates directories; it never removes anything outside its base, and
it never writes inside the workspace it sandboxed.

USAGE
-----
    reviewer_bwrap.py --base DIR init --source REPO
    reviewer_bwrap.py --base DIR exec < payload.json
    reviewer_bwrap.py --base DIR run -- go test ./...
    reviewer_bwrap.py --base DIR probe
"""

from __future__ import annotations

import argparse
import json
import os
import pwd
import shutil
import stat
import subprocess
import sys
import tempfile
from pathlib import Path

BWRAP = "/usr/bin/bwrap"

# The runtime the sandbox needs.  Everything else is invisible.
#
# This list is an ALLOW-LIST: a path in it is a path the reviewer can
# READ.  Adding a path for convenience is a capability grant.  `/etc`
# is deliberately NOT here — the sandbox carries a synthetic one, and
# the host's trust store (`/etc/ssl`, and the private keys under
# `/etc/ssl/private`) is not something a code review needs.
RUNTIME_DIRS = (
    "/usr",
    "/usr/local",
)

# The toolchain a review may need, DISCOVERED on the host.
#
# `RUNTIME_DIRS` above is the SYSTEM's toolchain, which on a
# conventional install lives under /usr.  A host that keeps its
# toolchain elsewhere — a CI runner's hosted tool cache, for instance
# — would otherwise give the reviewer no compiler at all, and the
# review would quietly degrade to reading code and guessing.
#
# So the script discovers the toolchain on the HOST and binds it
# READ-ONLY.  This is a HOST-SIDE decision: the script runs before the
# namespace exists, on the host's own PATH, and the model contributes
# nothing to it.
#
# A discovered root is refused when it resolves inside a protected root
# — and the rule here is STRICTER than the base's: the HOME is protected
# WHOLE, so no root under the home is ever bound, and no ancestor of the
# home is either.  A discovered root is a directory the reviewer can
# READ, so a root that carried the operator's credentials with it would
# defeat the boundary the whole sandbox exists to establish.  (The base
# is judged more leniently on purpose: the plugin puts it inside the
# home, and it is the reviewer's OWN tree.)
#
# The root is also bound BY DESCRIPTOR, like the reviewer's own tree, so
# a root swapped for a symlink between the check and the mount cannot
# change what is mounted.
TOOLCHAIN_PROGRAMS = ("go",)

# The paths the namespace mounts FRESH.  A bind under one of them is
# either masked by the fresh mount (the private `/tmp`) or cannot be
# resolved at all (the source would be looked up inside the fresh
# mount).  So a toolchain under one of them is not bound: the reviewer
# simply has no compiler there, which is a limitation rather than a
# failure.
FRESH_MOUNT_PATHS = ("/tmp", "/proc", "/dev", "/sys")

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

# The directories inside the operator's home that hold credentials,
# keys, private configuration and the substrate's own runtime root.
# A sandbox base must never sit inside one of them.
PROTECTED_HOME_SUBDIRS = (
    ".mpm",
    ".config",
    ".local",
    ".ssh",
    ".aws",
    ".gnupg",
    ".kube",
    ".docker",
    ".cargo",
    ".npm",
    ".password-store",
    ".vault",
    ".netrc",
    ".npmrc",
    ".pypirc",
    ".gitconfig",
    ".awsctl",
    ".terraform.d",
    ".azure",
    ".gcloud",
)

# The system directories.  A sandbox base inside one of them is never
# disposable.
PROTECTED_SYSTEM_DIRS = (
    "/bin",
    "/boot",
    "/dev",
    "/etc",
    "/lib",
    "/lib32",
    "/lib64",
    "/proc",
    "/run",
    "/sbin",
    "/srv",
    "/sys",
    "/usr",
    "/var",
)

# The exit code the script uses for a SANDBOX-level failure, as
# distinct from the exit code of the command that ran inside it.
SANDBOX_FAILURE = 125

# The file descriptor the `exec` envelope is written to.
ENVELOPE_FD = 3


class SandboxRefused(SystemExit):
    """The sandbox refused to operate.  Carries the reason."""


def refuse(why: str) -> "SandboxRefused":
    """Build the refusal the caller raises."""
    return SandboxRefused(why)


# --------------------------------------------------------------------------
# The protected set
# --------------------------------------------------------------------------
#
# The list is built from the PASSWD DATABASE.  `$HOME` is agent-
# influenceable: an environment variable that decides what is protected
# is not a security authority, because the agent can set it.  The passwd
# entry cannot be set by the agent, so it is the authority here.
#
# `$HOME` is still consulted — as an ADDITIONAL protected root, never as
# a replacement for the passwd one.  An attacker who sets
# `HOME=/tmp/attacker` therefore ADDS a protected root and removes
# none.


def operator_home() -> Path | None:
    """The operator's home, from the passwd database."""
    try:
        return Path(pwd.getpwuid(os.getuid()).pw_dir)
    except (KeyError, OSError):
        return None


def env_home() -> Path | None:
    """`$HOME`, if it is set.

    Consulted as an ADDITION to the passwd home, never as a replacement
    for it: an attacker who sets `HOME=/tmp/attacker` therefore ADDS
    protected roots and removes none.
    """
    value = os.environ.get("HOME")
    return Path(value) if value else None


def home_roots() -> list[Path]:
    """The homes THEMSELVES.

    A sandbox base may not BE the operator's home — the home holds
    everything — but it may live inside a part of it that is not
    sensitive.  The plugin puts the sandbox in the operator's own cache
    directory, and a guard that refused every base inside the home
    would refuse the one the plugin actually uses: the tool would then
    never run at all, which is a failure dressed as a safeguard.

    What is protected inside the home is the SENSITIVE part of it —
    the credentials, the keys, the private configuration and the
    substrate's own runtime root — and those are what
    `protected_roots` lists.
    """
    roots: list[Path] = []
    for home in (operator_home(), env_home()):
        if home is not None:
            roots.append(home)
    return roots


def protected_roots(extra: list[Path] | None = None) -> list[Path]:
    """Every directory a sandbox base must never resolve inside.

    The list is built from the PASSWD DATABASE.  `$HOME` is
    agent-influenceable: an environment variable that decides what is
    protected is not a security authority, because the agent can set
    it.  The passwd entry cannot be set by the agent, so it is the
    authority here.
    """
    roots: list[Path] = []

    for home in (operator_home(), env_home()):
        if home is None:
            continue
        for name in PROTECTED_HOME_SUBDIRS:
            roots.append(home / name)
        # A checkout is usually under the home, but it does not have to
        # be: protect the conventional source roots too.
        for name in ("src", "projects", "code", "work", "dev"):
            roots.append(home / name)

    roots.extend(Path(p) for p in PROTECTED_SYSTEM_DIRS)
    if extra:
        roots.extend(Path(p) for p in extra)
    return roots


# --------------------------------------------------------------------------
# The base
# --------------------------------------------------------------------------


def _is_within(child: Path, parent: Path) -> bool:
    """Is `child` inside `parent`?"""
    try:
        child.relative_to(parent)
    except ValueError:
        return False
    return True


def _symlink_component(path: Path) -> Path | None:
    """The first component of `path` that is a symlink, if there is one.

    The check walks the path from the root, so it catches a symlinked
    ANCESTOR and not only a symlinked leaf.  A symlink in the path is
    how a target gets swapped between the check and the mount.
    """
    current = Path(path.anchor or "/")
    for part in path.parts[1:] if path.is_absolute() else path.parts:
        current = current / part
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            continue
        except OSError:
            return current
        if stat.S_ISLNK(info.st_mode):
            return current
    return None


def _assert_no_symlink(path: Path) -> None:
    """Refuse a path any component of which is a symlink.

    The check walks the path from the root, so it catches a symlinked
    ANCESTOR and not only a symlinked leaf.  A symlink in the path is
    how a base gets swapped between the check and the mount.
    """
    offender = _symlink_component(path)
    if offender is not None:
        raise refuse(
            f"refusing to sandbox {path}: {offender} is a symlink, and a "
            "symlinked base can be swapped for another target"
        )


def resolve_base(base: Path, extra_protected: list[Path] | None = None) -> Path:
    """Validate a base directory and return its canonical form.

    The base is refused when it is the root, when it is inside a
    protected root, when it resolves through a live runtime root, or
    when any component of it is a symlink.
    """
    base = Path(base).expanduser()
    if not base.is_absolute():
        raise refuse(f"refusing to sandbox {base}: the base must be an absolute path")

    _assert_no_symlink(base)

    resolved = Path(os.path.realpath(base))
    if resolved == Path("/"):
        raise refuse(f"refusing to sandbox {resolved}: it is the root")

    for root in protected_roots(extra_protected):
        try:
            root_resolved = Path(os.path.realpath(root))
        except OSError:
            root_resolved = root
        if resolved == root_resolved:
            raise refuse(f"refusing to sandbox {resolved}: it is a protected directory")
        if _is_within(resolved, root_resolved):
            raise refuse(f"refusing to sandbox {resolved}: it is inside {root_resolved}")

    # The homes THEMSELVES: a base may not BE the home.  It may live
    # inside a part of it that is not sensitive — the plugin's own
    # default base is under the operator's cache directory, and refusing
    # it would make the tool permanently unusable.
    for root in home_roots():
        try:
            root_resolved = Path(os.path.realpath(root))
        except OSError:
            root_resolved = root
        if resolved == root_resolved:
            raise refuse(
                f"refusing to sandbox {resolved}: it is the operator's home"
            )

    for marker in LIVE_MARKERS:
        if marker in resolved.parts:
            raise refuse(
                f"refusing to sandbox {resolved}: it is inside a live runtime root"
            )

    return resolved


def refuse_inside(base: Path, why: str, extra_protected: list[Path] | None = None) -> None:
    """Refuse to operate on a path that is not a disposable tree."""
    resolve_base(Path(base), extra_protected)


# --------------------------------------------------------------------------
# The disposable tree
# --------------------------------------------------------------------------


def _write(path: Path, text: str, mode: int = 0o644) -> None:
    path.write_text(text, encoding="utf-8")
    os.chmod(path, mode)


def _synthetic_etc(target: Path) -> None:
    """Build the sandbox's own `/etc`.

    The host's `/etc` is NOT bound.  It carries the trust store, the
    private keys, the host's account database and the operator's home
    paths — none of which a code review needs, and every one of which a
    reviewer must not read.

    What the sandbox needs instead is an account database that names
    the sandbox's own identity, and the `alternatives` symlinks so that
    `cc` and `awk` resolve.
    """
    target.mkdir(parents=True, exist_ok=True)
    os.chmod(target, 0o755)

    uid = os.getuid()
    gid = os.getgid()
    _write(
        target / "passwd",
        "root:x:0:0:root:/root:/bin/sh\n"
        f"reviewer:x:{uid}:{gid}:reviewer:{PREFIX}/home:/bin/sh\n"
        "nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n",
    )
    _write(
        target / "group",
        "root:x:0:\n"
        f"reviewer:x:{gid}:\n"
        "nobody:x:65534:\n",
    )
    _write(
        target / "nsswitch.conf",
        "passwd: files\ngroup: files\nhosts: files dns\n",
    )

    # The `alternatives` directory is a set of SYMLINKS into /usr.  It
    # is copied, not bound: a copy cannot be a route to anything the
    # namespace does not already expose.
    host_alternatives = Path("/etc/alternatives")
    if host_alternatives.is_dir():
        try:
            shutil.copytree(
                host_alternatives,
                target / "alternatives",
                symlinks=True,
                ignore=shutil.ignore_patterns("__pycache__"),
            )
        except OSError:
            pass


def _scrub_clone_metadata(target: Path) -> None:
    """Remove the operator's PATHS from the clone's own metadata.

    A `git clone` records where it came from: in `.git/config` as the
    origin remote, in the reflog as "clone: from <path>", and in
    `.git/FETCH_HEAD`.  Those are absolute paths under the operator's
    home — they tell a less-trusted reviewer where the operator's
    checkout is, and therefore what the operator's account is called.

    A review needs none of it.  The copy is already made, and the
    remote is never used again, so the records are REMOVED rather than
    rewritten into something plausible: a scrubbed file that still
    looks like a real one invites a reader to trust a value that was
    edited, and the honest state is an absent one.

    What is NOT removed is the history itself, and the identities in
    it.  The history is the review material, and the commit identities
    are public in a public repository — removing them would change
    what is under review to make the sandbox tidier.
    """
    git_dir = target / ".git"
    if not git_dir.is_dir():
        return

    # The remote: `git remote remove origin` drops the whole section,
    # including the fetch refspec that carries the path.
    try:
        subprocess.run(
            ["git", "-C", str(target), "remote", "remove", "origin"],
            capture_output=True,
            text=True,
            timeout=60,
        )
    except (OSError, subprocess.SubprocessError):
        pass

    # The reflog and the fetch heads: they record the clone's source.
    for name in ("logs", "FETCH_HEAD", "ORIG_HEAD", "MERGE_HEAD"):
        path = git_dir / name
        try:
            if path.is_dir():
                shutil.rmtree(path, ignore_errors=True)
            elif path.exists() or path.is_symlink():
                path.unlink()
        except OSError:
            pass

    # Anything else in the config that names an absolute location.
    config = git_dir / "config"
    try:
        text = config.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return
    cleaned = _strip_path_bearing_config(text)
    if cleaned != text:
        try:
            config.write_text(cleaned, encoding="utf-8")
        except OSError:
            pass


def _strip_path_bearing_config(text: str) -> str:
    """Drop the lines of a git config that name a host location.

    A git config is INI-shaped: sections in brackets, `key = value`
    lines under them.  A value that starts with `/` or `~` is a
    location on this host, and a location on this host is exactly what
    the reviewer must not be told.
    """
    kept: list[str] = []
    skipping = False
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith("["):
            skipping = stripped.startswith("[remote")
        if skipping:
            continue
        if "=" in stripped:
            _, _, value = stripped.partition("=")
            value = value.strip()
            if value.startswith("/") or value.startswith("~"):
                continue
        kept.append(line)
    return "\n".join(kept) + "\n"


def _sync_repository(source: Path, target: Path) -> None:
    """Copy the repository under review into the sandbox.

    The copy is a real COPY: the operator's checkout is never the
    writable tree, and a write inside the sandbox cannot reach the
    original through a hard link.

    A git clone gives the reviewer the history and a clean index.  The
    working tree's own uncommitted changes are overlaid afterwards, so
    the copy is what the operator is actually looking at.  The clone's
    OWN metadata — the paths it recorded about its source — is scrubbed
    afterwards, because a review does not need to know where the
    operator's checkout lives.
    """
    target.parent.mkdir(parents=True, exist_ok=True)
    if target.exists():
        shutil.rmtree(target, ignore_errors=True)

    cloned = False
    if (source / ".git").exists():
        result = subprocess.run(
            ["git", "clone", "--no-hardlinks", "--quiet", str(source), str(target)],
            capture_output=True,
            text=True,
            timeout=600,
        )
        cloned = result.returncode == 0
    if not cloned:
        shutil.copytree(
            source,
            target,
            symlinks=True,
            ignore=shutil.ignore_patterns(
                ".git", "node_modules", "__pycache__", "target", ".venv"
            ),
        )
        return

    # The clone recorded where it came from.  The review does not need
    # to know.
    _scrub_clone_metadata(target)

    # Overlay the working tree's uncommitted changes and untracked
    # files, so the copy is the tree the operator is looking at.
    try:
        status = subprocess.run(
            ["git", "-C", str(source), "status", "--porcelain=v1", "-z",
             "--untracked-files=normal"],
            capture_output=True,
            text=True,
            timeout=120,
        )
    except (OSError, subprocess.SubprocessError):
        return
    if status.returncode != 0:
        return
    entries = status.stdout.split("\0")
    index = 0
    while index < len(entries):
        entry = entries[index]
        index += 1
        if not entry:
            continue
        if len(entry) < 4:
            continue
        code = entry[:2]
        path_text = entry[3:]
        # In the `-z` form a rename or a copy is followed by its
        # ORIGINAL path as a second NUL-separated entry.
        if code[0] in ("R", "C"):
            index += 1
        if not path_text or path_text.startswith(".git"):
            continue
        src = source / path_text
        dst = target / path_text
        try:
            if "D" in code:
                # Deleted in the working tree: the clone must not keep
                # a file the operator has removed.
                if dst.exists() or dst.is_symlink():
                    if dst.is_dir() and not dst.is_symlink():
                        shutil.rmtree(dst, ignore_errors=True)
                    else:
                        dst.unlink()
                continue
            if src.is_dir() and not src.is_symlink():
                dst.mkdir(parents=True, exist_ok=True)
                for child in src.iterdir():
                    if child.name == ".git":
                        continue
                    _copy_entry(child, dst / child.name)
            elif src.exists() or src.is_symlink():
                dst.parent.mkdir(parents=True, exist_ok=True)
                _copy_entry(src, dst)
        except OSError:
            continue


def _copy_entry(src: Path, dst: Path) -> None:
    """Copy one file or symlink, preserving the kind."""
    if src.is_symlink():
        link = os.readlink(src)
        if dst.exists() or dst.is_symlink():
            dst.unlink()
        os.symlink(link, dst)
        return
    shutil.copy2(src, dst)


def build_tree(base: Path, source: Path | None) -> dict:
    """The disposable tree: a repo copy and the reviewer's scratch."""
    base.mkdir(parents=True, exist_ok=True)
    os.chmod(base, 0o700)

    for name in ("home", "tmp", "workspace", "findings"):
        path = base / name
        path.mkdir(parents=True, exist_ok=True)
        os.chmod(path, 0o700)

    _synthetic_etc(base / "etc")

    if source is not None:
        source = Path(source).resolve()
        if not source.is_dir():
            raise refuse(f"the source {source} is not a directory")
        _sync_repository(source, base / "source" / "repo")
    else:
        # The repository slot still has to EXIST: bubblewrap refuses to
        # bind a path that is not there, and the whole namespace fails
        # to start when one bind fails.
        repo = base / "source" / "repo"
        repo.mkdir(parents=True, exist_ok=True)
        os.chmod(repo, 0o700)

    return {
        "base": base,
        "repo": base / "source" / "repo",
        "home": base / "home",
        "tmp": base / "tmp",
        "workspace": base / "workspace",
        "findings": base / "findings",
        "etc": base / "etc",
        # The host's toolchain, discovered ONCE.  The bind and the
        # PATH must agree on the same list, so the list is computed
        # here and read from here — never computed twice and hoped to
        # match.
        "toolchain_roots": toolchain_roots(),
    }


# --------------------------------------------------------------------------
# The namespace
# --------------------------------------------------------------------------


def open_tree(tree: dict) -> dict:
    """Open each bindable directory with `O_NOFOLLOW`.

    The descriptor names the INODE, so a path that is swapped for a
    symlink after this point cannot change what is mounted.

    The host's DISCOVERED toolchain is opened the same way: a
    path-based bind of a toolchain root would leave a window between
    the check and the mount, and the reviewer's own tree does not have
    that window.
    """
    fds: dict[str, int] = {}
    try:
        for name in TREE:
            fds[name] = os.open(
                tree[name],
                os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
            )
        fds["etc"] = os.open(
            tree["etc"],
            os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
        )
        for index, root in enumerate(tree.get("toolchain_roots", [])):
            fds[f"toolchain:{index}"] = os.open(
                root,
                os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
            )
    except OSError as exc:
        for fd in fds.values():
            os.close(fd)
        raise refuse(f"the sandbox tree could not be opened: {exc}")
    return fds


def close_tree(fds: dict) -> None:
    for fd in fds.values():
        try:
            os.close(fd)
        except OSError:
            pass


def _is_protected_path(path: Path) -> bool:
    """Is this path inside a protected root?

    The same question the base is asked, asked of the toolchain roots
    the sandbox discovers: a hostile PATH must not be able to point the
    sandbox at the operator's keys.

    The HOME is protected WHOLE here, and not only in its sensitive
    parts.  That is DELIBERATELY stricter than the base's rule, and
    the difference is the point:

      * the BASE may live inside a harmless part of the home, because
        the plugin puts it there (`~/.cache/mpm-opencode/review-sandbox`)
        and a guard that refused every base inside the home would refuse
        the one the plugin actually uses;
      * a DISCOVERED ROOT is a directory the reviewer can READ.  A root
        inside the home carries the home's credentials with it, so no
        root under the home may be bound at all.

    An ANCESTOR of the home is refused for the same reason: binding
    `/home` would carry every account's home with it.  The root itself
    is refused because a bind of `/` would carry the whole filesystem.

    (The defect this closes: a PATH entry whose `bin` directory is a
    direct child of the home — `~/bin` — made the DISCOVERY derive the
    home ITSELF as the root, because the derivation stripped one
    directory level.  The home was then bound READ-ONLY into the
    namespace, and a reviewer could read the operator's credentials with
    a plain `cat`.  Measured, reproduced and closed.)
    """
    try:
        resolved = Path(os.path.realpath(path))
    except OSError:
        return True
    if resolved == Path("/"):
        return True
    for root in protected_roots():
        try:
            root_resolved = Path(os.path.realpath(root))
        except OSError:
            root_resolved = root
        if resolved == root_resolved or _is_within(resolved, root_resolved):
            return True
    # The homes themselves, and everything under them, and everything
    # above them.
    for root in home_roots():
        try:
            root_resolved = Path(os.path.realpath(root))
        except OSError:
            root_resolved = root
        if resolved == root_resolved:
            return True
        if _is_within(resolved, root_resolved):
            return True
        if _is_within(root_resolved, resolved):
            return True
    # EVERY account's home, not only the operator's.  Another account's
    # home is not the operator's data, but it is the same class of
    # disclosure: a PATH entry that named it would carry that account's
    # credentials into the namespace with it.  A host that keeps a
    # toolchain in a home directory loses that toolchain to the
    # reviewer, which is a limitation rather than a hole.
    if _is_within(resolved, Path("/home")):
        return True
    return False


def _toolchain_root_for(program: Path) -> Path:
    """The root that HOLDS a tool: `<root>/bin/<program>` -> `<root>`.

    Only a directory that is literally named `bin` is stripped.  A
    program that does not live in a `bin` directory keeps its own
    directory, so a PATH entry can never collapse onto something larger
    than the directory that actually holds the tool — the defect that
    made a home-relative `~/bin` entry yield the HOME itself.
    """
    parent = program.parent
    return parent.parent if parent.name == "bin" else parent


def toolchain_roots() -> list[Path]:
    """The host's toolchain roots, discovered on the HOST's PATH.

    A root qualifies when it holds a `bin` directory with the tool in
    it — the binding is what makes the tool runnable inside the
    namespace.  A root the static runtime list already covers is not
    returned: binding it twice is noise, and the static bind is
    already there.

    The discovery is HOST-SIDE.  The model contributes nothing to it:
    the script reads the host's own environment, before the namespace
    exists, and binds what it finds READ-ONLY.
    """
    roots: list[Path] = []
    seen: set[str] = set()

    candidates: list[Path] = []
    goroot = os.environ.get("GOROOT")
    if goroot:
        candidates.append(Path(goroot))
    for directory in os.environ.get("PATH", "").split(os.pathsep):
        if not directory:
            continue
        for program in TOOLCHAIN_PROGRAMS:
            candidate = Path(directory) / program
            try:
                if not candidate.is_file() or not os.access(candidate, os.X_OK):
                    continue
                resolved = Path(os.path.realpath(candidate))
            except OSError:
                continue
            candidates.append(_toolchain_root_for(resolved))

    for root in candidates:
        try:
            resolved = Path(os.path.realpath(root))
        except OSError:
            continue
        if not resolved.is_dir() or not (resolved / "bin").is_dir():
            continue
        if resolved == Path("/"):
            continue  # the root would carry the whole filesystem
        if any(_is_within(resolved, Path(base)) for base in RUNTIME_DIRS):
            continue  # the static runtime list already binds it
        if any(_is_within(resolved, Path(fresh)) for fresh in FRESH_MOUNT_PATHS):
            continue  # the namespace mounts these fresh: a bind there is masked
        if _is_protected_path(resolved):
            continue
        if _symlink_component(resolved) is not None:
            # Belt and braces: the root was already resolved with
            # `realpath`, so in the steady state this cannot fire.  What
            # actually closes the window is the DESCRIPTOR bind below.
            continue
        key = str(resolved)
        if key in seen:
            continue
        seen.add(key)
        roots.append(resolved)
    return roots


def sandbox_path(roots: list[Path] | None = None) -> str:
    """The PATH inside the namespace.

    The static entries first, then the discovered toolchain's own `bin`
    directory: a host whose Go lives elsewhere still gets a `go` the
    reviewer can run.
    """
    if roots is None:
        roots = toolchain_roots()
    entries = ["/usr/local/go/bin", "/usr/local/bin", "/usr/bin", "/bin"]
    for root in roots:
        entries.insert(0, f"{root}/bin")
    return ":".join(entries)


def bwrap_args(tree: dict, fds: dict | None = None) -> list:
    """The bwrap argument list: a minimal, writable-only-where-needed root.

    With `fds` given, the reviewer's tree and the synthetic `/etc` are
    bound BY DESCRIPTOR.  Without it, the caller gets the static shape
    only — which is what the contract tests assert on.
    """
    args = [BWRAP]
    for path in RUNTIME_DIRS:
        if os.path.exists(path):
            args += ["--ro-bind", path, path]
    # The host's toolchain, wherever the host keeps it: READ-ONLY, bound
    # BY DESCRIPTOR (so a root swapped for a symlink after the check
    # cannot change what is mounted), and only when it is not already
    # covered by the static list.
    for index, root in enumerate(tree.get("toolchain_roots", [])):
        fd = fds.get(f"toolchain:{index}") if fds is not None else None
        if fd is None:
            continue
        args += ["--ro-bind-fd", str(fd), str(root)]
    for link, target in MERGED_SYMLINKS.items():
        if os.path.islink(link):
            args += ["--symlink", target, link]
    if fds is not None:
        # The synthetic /etc: the host's trust store and account
        # database are NOT reachable through it.
        args += ["--ro-bind-fd", str(fds["etc"]), "/etc"]
        # The reviewer's own tree: read-write, by descriptor.
        for name in TREE:
            args += ["--bind-fd", str(fds[name]), f"{PREFIX}/{name}"]
    # THE PID NAMESPACE MUST EXIST BEFORE /proc IS MOUNTED. The first
    # version mounted /proc and only then asked bubblewrap to unshare the
    # PID namespace; the mount therefore exposed the HOST's /proc, and a
    # command could read the caller's environment through
    # /proc/1/environ. The unshare flags are emitted first.
    args += [
        "--unshare-all",
        "--unshare-user",
        "--disable-userns",
        "--assert-userns-disabled",
        "--die-with-parent",
        "--new-session",
    ]
    # The kernel's pseudo-filesystems and a PRIVATE /tmp.
    args += ["--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp"]
    # The environment: disposable HOME, TMPDIR and MPM_WORKSPACE.
    #
    # `--clearenv` is emitted before the `--setenv` flags because bwrap
    # CLEARS the environment first and then applies the `--setenv`
    # variables, whatever order the two appear in.  The order here is
    # not what makes it work — it is what makes it READ correctly, so
    # that nobody "fixes" it into a form that looks equivalent and is
    # not.
    args += ["--clearenv"]
    args += [
        "--setenv", "HOME", f"{PREFIX}/home",
        "--setenv", "TMPDIR", f"{PREFIX}/tmp",
        "--setenv", "MPM_WORKSPACE", f"{PREFIX}/workspace",
        "--setenv", "MPM_REVIEW_SANDBOX", PREFIX,
        "--setenv", "MPM_REVIEW_FINDINGS", f"{PREFIX}/findings",
        "--setenv", "GOCACHE", f"{PREFIX}/tmp/gocache",
        "--setenv", "GOPATH", f"{PREFIX}/tmp/gopath",
        "--setenv", "GOMODCACHE", f"{PREFIX}/tmp/gomodcache",
        "--setenv", "GOPROXY", "off",
        "--setenv", "GOFLAGS", "-mod=mod",
        "--setenv", "PATH", sandbox_path(tree.get("toolchain_roots")),
        "--setenv", "TERM", "dumb",
        "--setenv", "LANG", "C.UTF-8",
    ]
    # A sandbox that cannot enforce the disabled nested user namespace
    # does not start. The unshare flags were emitted before the /proc
    # mount above.
    return args


def namespace_argv(tree: dict, fds: dict, command: list) -> list:
    """The full bwrap argv: the options, the SEPARATOR, the command.

    The `--` is the boundary.  Everything after it is the COMMAND and
    the command's own arguments; bubblewrap parses nothing after it as
    an option.  Without the separator a command vector could carry
    `--bind / /` and mount the host's root into the namespace.
    """
    return bwrap_args(tree, fds) + ["--"] + list(command)


def validate_command(command: list) -> list:
    """The command vector, checked.

    Two rules.  The command may not be empty, and its FIRST element
    may not begin with `-`: a command is a program, and a program named
    like an option is an attempt to reach the caller's option parser.
    (The `exec` path never reaches a parser at all — the command
    arrives as JSON on stdin — and the `--` separator stops bubblewrap
    from parsing it either.  This is the third lock on the same door.)
    """
    if not command:
        raise refuse("no command to run")
    for element in command:
        if not isinstance(element, str):
            raise refuse("the command must be a list of strings")
        if "\0" in element:
            raise refuse("the command must not contain a NUL")
    if command[0].startswith("-"):
        raise refuse(
            f"refusing to run {command[0]!r}: a command may not begin with an option"
        )
    return list(command)


def execute(tree: dict, command: list, cwd: str | None = None,
            timeout_ms: int | None = None) -> dict:
    """Run a command inside the namespace.

    There is NO fallback: if the namespace cannot be built, the command
    does not run at all.  A sandbox that cannot be established is a
    refusal, not a degraded mode.
    """
    command = validate_command(command)

    if cwd is not None and cwd not in TREE:
        raise refuse(f"the working directory must be one of {TREE}, not {cwd!r}")

    if not os.path.exists(BWRAP):
        raise refuse(
            f"bubblewrap is not installed at {BWRAP}; the sandbox cannot be "
            "established and the command was NOT run"
        )

    fds = open_tree(tree)
    try:
        args = namespace_argv(tree, fds, command)
        if cwd is not None:
            # `--chdir` must come before the separator.
            head, tail = args[:-1 - len(command)], args[-1 - len(command):]
            args = head + ["--chdir", f"{PREFIX}/{cwd}"] + tail
        timeout = None if timeout_ms is None else timeout_ms / 1000.0
        try:
            result = subprocess.run(
                args,
                capture_output=True,
                text=True,
                timeout=timeout,
                pass_fds=tuple(fds.values()),
            )
        except subprocess.TimeoutExpired as exc:
            return {
                "ok": False,
                "stage": "timeout",
                "error": f"the command timed out after {timeout_ms} ms",
                "exit_code": None,
                "stdout": exc.stdout.decode() if isinstance(exc.stdout, bytes) else (exc.stdout or ""),
                "stderr": exc.stderr.decode() if isinstance(exc.stderr, bytes) else (exc.stderr or ""),
            }
        except OSError as exc:
            raise refuse(f"the namespace could not be started: {exc}")
        return {
            "ok": True,
            "stage": "ran",
            "exit_code": result.returncode,
            "stdout": result.stdout,
            "stderr": result.stderr,
        }
    finally:
        close_tree(fds)


def run(tree: dict, command: list) -> int:
    """The human-facing path: the command's own streams and exit code."""
    result = execute(tree, command)
    sys.stdout.write(result.get("stdout") or "")
    sys.stderr.write(result.get("stderr") or "")
    return int(result.get("exit_code") or 0)


def sentinel_experiment(tree: dict) -> int:
    """Ask the sandbox what it can see.  Nothing real is touched."""
    protected = tree["base"] / "protected"
    protected.mkdir(parents=True, exist_ok=True)
    (protected / "sentinel.txt").write_text(
        "DISPOSABLE SENTINEL - not real data\n", encoding="utf-8"
    )
    (protected / "fake-mpm.db").write_bytes(b"DISPOSABLE SENTINEL DB\n")

    home = operator_home()
    home_text = str(home) if home is not None else "/nonexistent-home"

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
            'echo "--- the trust store ---"; '
            'if [ -e /etc/ssl ]; then echo "REACHABLE (BAD): /etc/ssl"; '
            'else echo "INVISIBLE (GOOD): /etc/ssl"; fi; '
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
            home=home_text,
            prefix=PREFIX,
        ),
    ]
    print("--- the sandbox's view ---")
    code = run(tree, probe)
    print(f"--- the probe's exit code: {code} ---")
    return code


# --------------------------------------------------------------------------
# The command line
# --------------------------------------------------------------------------


def _read_envelope() -> dict:
    """Read the `exec` payload from stdin: `{"argv": [...], "cwd": ...}`."""
    raw = sys.stdin.read()
    if not raw.strip():
        raise refuse("the exec payload is empty")
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise refuse(f"the exec payload is not JSON: {exc}")
    if not isinstance(payload, dict):
        raise refuse("the exec payload must be a JSON object")
    argv = payload.get("argv")
    if not isinstance(argv, list):
        raise refuse("the exec payload must carry an `argv` LIST")
    cwd = payload.get("cwd")
    if cwd is not None and not isinstance(cwd, str):
        raise refuse("the exec payload's `cwd` must be a string")
    timeout = payload.get("timeout_ms")
    if timeout is not None and not isinstance(timeout, int):
        raise refuse("the exec payload's `timeout_ms` must be an integer")
    return {"argv": argv, "cwd": cwd, "timeout_ms": timeout}


def _write_envelope(envelope: dict) -> None:
    """Write the envelope to the reserved descriptor."""
    try:
        os.write(ENVELOPE_FD, (json.dumps(envelope) + "\n").encode("utf-8"))
    except OSError:
        pass


# Credentials that belong to the INFERENCE process, never to the sandboxed
# command or to bubblewrap's own init process. `--clearenv` clears the
# COMMAND's environment, not bubblewrap's init process (/proc/1), so the
# wrapper scrubs its own environment before it spawns bwrap.
SCRUBBED_ENVIRONMENT = (
    "MINIMAX_API_KEY",
    "DEEPSEEK_API_KEY",
    "MPM_OPENCODE_REVIEWER_KEY",
    "MPM_OPENCODE_SIDECAR_KEY",
    "OPENAI_API_KEY",
    "ANTHROPIC_API_KEY",
)


def scrub_credentials() -> None:
    for name in SCRUBBED_ENVIRONMENT:
        os.environ.pop(name, None)


def main(argv: list | None = None) -> int:
    scrub_credentials()
    parser = argparse.ArgumentParser(
        description="an OS-confined sandbox for a review subagent"
    )
    parser.add_argument("--base", default=None, help="the sandbox base directory")
    parser.add_argument(
        "--protect",
        action="append",
        default=[],
        help="a directory the sandbox base must never be inside (repeatable)",
    )
    sub = parser.add_subparsers(dest="command", required=True)
    init = sub.add_parser("init", help="build the disposable tree")
    init.add_argument("--source", default=None, help="the repository to copy in")
    sub.add_parser("exec", help="run a command described by a JSON payload on stdin")
    run_parser = sub.add_parser("run", help="run a command inside the sandbox")
    run_parser.add_argument("argv", nargs=argparse.REMAINDER)
    sub.add_parser("probe", help="the disposable sentinel experiment")
    roots_parser = sub.add_parser(
        "roots",
        help="print the toolchain roots the discovery would bind, and nothing else",
    )
    args = parser.parse_args(argv)

    protected = [Path(p) for p in args.protect]

    # The discovery answer comes FIRST and touches nothing: it creates
    # no directory and reads no tree, so it can be asked about a host
    # without changing it.
    if args.command == "roots":
        print("ROOTS:")
        for root in toolchain_roots():
            print(root)
        return 0

    try:
        base = (
            Path(args.base)
            if args.base
            else Path(tempfile.gettempdir()) / f"mpm-review-sbx-{os.getuid()}"
        )
        base = resolve_base(base, protected)
        base.mkdir(parents=True, exist_ok=True)
        os.chmod(base, 0o700)
        tree = build_tree(base, getattr(args, "source", None))

        if args.command == "probe":
            return sentinel_experiment(tree)
        if args.command == "init":
            print(f"the sandbox base: {base}")
            for name in TREE:
                print(f"  {PREFIX}/{name}: {tree[name]}")
            return 0
        if args.command == "exec":
            payload = _read_envelope()
            try:
                result = execute(
                    tree,
                    payload["argv"],
                    cwd=payload.get("cwd"),
                    timeout_ms=payload.get("timeout_ms"),
                )
            except SandboxRefused as refusal:
                envelope = {"ok": False, "stage": "refused", "error": str(refusal)}
                _write_envelope(envelope)
                print(str(refusal), file=sys.stderr)
                return SANDBOX_FAILURE
            result["base"] = str(base)
            result["sandbox"] = PREFIX
            _write_envelope(result)
            sys.stdout.write(result.get("stdout") or "")
            sys.stderr.write(result.get("stderr") or "")
            return int(result.get("exit_code") or 0)

        command = list(args.argv)
        if command and command[0] == "--":
            command = command[1:]
        if not command:
            parser.error("run needs a command")
        return run(tree, command)
    except SandboxRefused as refusal:
        print(str(refusal), file=sys.stderr)
        return SANDBOX_FAILURE


if __name__ == "__main__":
    sys.exit(main())
