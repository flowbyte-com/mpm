"""Workspace isolation and environment scrubbing (C2, C4).

This module is the foundation of the live-store isolation discipline.
Every other module that touches the filesystem or spawns a subprocess
goes through this one.

Design references (verified file:line):
  - C4(1) realpath rejection: this module
  - C4(2) output-dir explicitness: this module
  - C4(3) symlink rejection: this module
  - C4(4) private output permissions: this module
  - C4(8) env scrubbing: this module
  - MPM_SHARED_DB at cmd/mpm/handlers_shared.go:33
  - MPM_DB_LOCK at cmd/mpm/handlers_shred_database_reset.go:397
  - MPM workspace priority at internal/core/config/config.go:529-597

Hardening (priority 1 of HAL's review, 2026-10-19):
  - Re-check permissions immediately before each write under
    `output_dir` to close the TOCTOU window between the original
    check and the actual write.
  - Reject output dirs that resolve through a symlink chain into
    a live-store sentinel, even when the original path was not a
    symlink at the time of the call.
  - Reject output dirs whose parent path or any path component
    resolves into a live-store sentinel.
  - Reject any environment variable whose name starts with
    `MPM_` or `OLLAMA_` (forward-compatible against future
    overrides MPM might add).

v3 review (HAL 2026-10-19):
  - All path safety checks run BEFORE any directory creation. If
    the path resolves into a live-store sentinel or into the
    snapshot workspace, the call raises and nothing is created
    on disk. This avoids leaking side-effects into the live
    store on the rejection path.
  - Snapshot workspace helpers accept an optional `protected_root`
    so tests can simulate ~/.mpm under a tempdir without ever
    touching the real operator home.
"""

from __future__ import annotations

import os
import stat
from pathlib import Path

# Env keys that are safe to pass through to MPM subprocesses. Anything
# else must be scrubbed. This list is intentionally minimal.
SAFE_ENV_KEYS: frozenset[str] = frozenset(
    {"PATH", "LANG", "LC_ALL", "TZ", "TMPDIR"}
)

# Env keys that MPM consults and that the harness must not inherit
# from its own environment. The harness explicitly sets MPM_WORKSPACE
# and HOME; those are NOT in this list because the harness owns them.
# An empty-string MPM_DB_LOCK is still a non-empty override at
# handlers_shred_database_reset.go:397, so the only safe thing is to
# leave the key absent.
DANGEROUS_INHERITED_KEYS: frozenset[str] = frozenset(
    {
        "MPM_SHARED_DB",
        "MPM_SHARED_READONLY",
        "MPM_DB_LOCK",
        "MPM_DB_PATH",
        "OLLAMA_ENDPOINT",
        "OLLAMA_MODEL",
        "OPENAI_API_KEY",
        "ANTHROPIC_API_KEY",
    }
)

# Forward-compatible env-key prefix denylist. Any env var whose name
# starts with one of these prefixes is scrubbed even if it is not in
# DANGEROUS_INHERITED_KEYS. This guards against future MPM code that
# adds a new MPM_*/OLLAMA_*/OPENAI_* override without the harness
# being updated.
DANGEROUS_ENV_PREFIXES: tuple[str, ...] = (
    "MPM_",
    "OLLAMA_",
    "OPENAI_",
    "ANTHROPIC_",
    "OPENROUTER_",
)

# Paths that, if the realpath of a snapshot or output dir lands inside
# them, cause a hard refusal. These are the load-bearing live-store
# sentinels.
#
# v3 review: this is a module-level tuple populated at import time.
# Tests that need to simulate a "home" root can pass a custom
# `protected_root` to the assert_* helpers; the helpers do NOT
# mutate this global. Production callers see the operator's real
# home here.
LIVE_STORE_SENTINELS: tuple[str, ...] = (
    str(Path.home() / ".mpm"),
    str(Path.home()),
)


class WorkspaceError(RuntimeError):
    """Raised when a workspace fails the isolation checks."""


def _realpath(p: Path) -> Path:
    """Resolve symlinks and normalize. os.path.realpath does the work
    but does not raise on non-existent paths; we want a consistent
    result either way."""
    return Path(os.path.realpath(p))


def _path_is_under(child: Path, parent: Path) -> bool:
    """Return True if `child` is the same path as `parent` or is
    inside `parent` after symlink resolution. Both paths are
    realpath-resolved first so a symlink that points into `parent`
    is caught regardless of which form was passed in."""
    child_r = _realpath(child)
    parent_r = _realpath(parent)
    if child_r == parent_r:
        return True
    try:
        child_r.relative_to(parent_r)
        return True
    except ValueError:
        return False


def _sentinels_for(protected_root: Path | None) -> tuple[Path, ...]:
    """Return the live-store sentinels to use for safety checks.

    If `protected_root` is None, the module-level
    `LIVE_STORE_SENTINELS` (which is rooted at the operator's real
    `Path.home()`) is used. Tests pass a tempdir here to simulate
    a "home" without touching the real one.
    """
    if protected_root is None:
        return tuple(Path(s) for s in LIVE_STORE_SENTINELS)
    return (protected_root, protected_root / ".mpm")


def _assert_outside_live_store_sentinels(
    real: Path, role: str, sentinels: tuple[Path, ...]
) -> None:
    """Refuse if `real` is the same as or inside any live-store
    sentinel. `role` is "snapshot workspace" or "output dir" and
    is used in the error message. `sentinels` is the
    caller-supplied (or default) list of protected roots."""
    for sentinel in sentinels:
        if _path_is_under(real, sentinel):
            raise WorkspaceError(
                f"refusing {role} {real}: resolves inside "
                f"protected root {sentinel}"
            )


def assert_safe_workspace(
    snap: Path,
    protected_root: Path | None = None,
    create: bool = True,
) -> Path:
    """Resolve a snapshot workspace through realpath and refuse if it
    lands inside any live-store sentinel.

    v3 review: the safety check runs BEFORE any directory creation.
    If `snap` resolves into a protected root, the call raises and
    nothing is created. If the path is safe, the dir is created
    (when `create=True`) and the resolved realpath is returned.

    Args:
        snap: candidate snapshot workspace path.
        protected_root: optional override for the live-store
            sentinel set. Production callers leave this None
            (uses `Path.home()` and `Path.home() / ".mpm"`).
            Tests pass a tempdir here to simulate a "home" root
            without touching the real one.
        create: if True (default), create the dir with 0o700
            after the safety check passes.
    """
    sentinels = _sentinels_for(protected_root)
    # Resolve as if the path existed. If it does not yet, realpath
    # returns the parent-anchored absolute form. If it does, we
    # get the resolved realpath including any symlinks. Either way
    # the safety check uses the canonicalised form so a symlink
    # pointing at ~/.mpm is caught.
    real = _realpath(snap)
    _assert_outside_live_store_sentinels(real, "snapshot workspace", sentinels)
    if create:
        # Re-resolve to handle the case where `snap` is a new
        # path whose parent might have changed (rare; cheap).
        snap.mkdir(parents=True, exist_ok=True, mode=0o700)
        real = _realpath(snap)
        _assert_outside_live_store_sentinels(real, "snapshot workspace", sentinels)
    return real


def assert_safe_output_dir(
    out: Path,
    snap_real: Path,
    protected_root: Path | None = None,
    create: bool = True,
) -> Path:
    """Resolve an output directory and refuse if it lands inside the
    snapshot workspace, inside any live-store sentinel, or has
    permissions wider than 0o700.

    v3 review: the safety checks run BEFORE any directory creation.
    If the path resolves into a protected root or the snapshot
    workspace, the call raises and nothing is created on disk.
    This is the load-bearing guarantee that the rejection path
    does not leak a side-effect into the live store.

    Args:
        out: candidate output dir.
        snap_real: the resolved realpath of the snapshot workspace;
            `out` may not be inside it.
        protected_root: optional override for the live-store
            sentinel set (see `assert_safe_workspace`).
        create: if True (default), create the dir with 0o700
            after all safety checks pass.
    """
    sentinels = _sentinels_for(protected_root)
    real = _realpath(out)
    if _path_is_under(real, snap_real):
        raise WorkspaceError(
            f"refusing output dir {out}: inside snapshot workspace {snap_real}"
        )
    _assert_outside_live_store_sentinels(real, "output dir", sentinels)
    # Permission check: a pre-existing dir wider than 0o700 is
    # rejected. (If the dir does not exist yet, this check is
    # skipped; the post-create chmod ensures 0o700.)
    if real.exists():
        mode = stat.S_IMODE(os.stat(real).st_mode)
        if mode & 0o077:
            raise WorkspaceError(
                f"refusing output dir {real}: permissions {oct(mode)} are wider "
                f"than 0o700; the harness requires private output dirs"
            )
    if create:
        out.mkdir(parents=True, exist_ok=True, mode=0o700)
        # Re-resolve after creation in case any intermediate path
        # component was a symlink. Then re-run the sentinels
        # check and the permission check.
        real = _realpath(out)
        _assert_outside_live_store_sentinels(real, "output dir", sentinels)
        if _path_is_under(real, snap_real):
            raise WorkspaceError(
                f"refusing output dir {out}: inside snapshot workspace {snap_real}"
            )
        mode = stat.S_IMODE(os.stat(real).st_mode)
        if mode & 0o077:
            raise WorkspaceError(
                f"refusing output dir {real}: permissions {oct(mode)} are wider "
                f"than 0o700; the harness requires private output dirs"
            )
    return real


def recheck_output_dir_safe(
    out: Path, snap_real: Path, protected_root: Path | None = None
) -> None:
    """Re-run the output-dir safety checks immediately before a
    write. Closes the TOCTOU window between `assert_safe_output_dir`
    and the actual write: a malicious local actor could chmod the
    dir to 0o755 or replace it with a symlink to ~/.mpm in that
    gap. The harness MUST call this immediately before any write
    under `out`.

    `protected_root` follows the same convention as
    `assert_safe_output_dir` (None = use real Path.home()).
    """
    sentinels = _sentinels_for(protected_root)
    if not out.exists():
        raise WorkspaceError(
            f"output dir {out} disappeared between safe check and write"
        )
    real = _realpath(out)
    if _path_is_under(real, snap_real):
        raise WorkspaceError(
            f"refusing output dir {out}: now resolves inside snapshot workspace "
            f"{snap_real} (TOCTOU)"
        )
    _assert_outside_live_store_sentinels(real, "output dir", sentinels)
    mode = stat.S_IMODE(os.stat(real).st_mode)
    if mode & 0o077:
        raise WorkspaceError(
            f"output dir {real} permissions are now {oct(mode)}; "
            f"wider than 0o700 (TOCTOU)"
        )


def reject_unsafe_symlinks(root: Path) -> list[Path]:
    """Walk `root` and return a list of symlinks that point at a path
    outside `root`. This is the C4(3) symlink-rejection walk.

    The walk is bounded to `root` and does not follow symlinks. It
    returns the list of offending symlinks; an empty list means the
    dir is safe.

    A symlink that points inside `root` is fine (e.g. a convenience
    link to a sibling). A symlink that points outside `root` is
    rejected: the harness never wants the benchmark to read through
    a symlink into a path it has not explicitly whitelisted.

    Hardening (priority 1 of HAL's review): also refuse any
    symlink that points into a live-store sentinel, even if it
    points inside `root` (a within-root symlink to
    `~/.mpm/foo` would otherwise pass the within-root test).
    """
    root_real = _realpath(root)
    bad: list[Path] = []
    for dirpath, dirnames, filenames in os.walk(root_real, followlinks=False):
        for name in dirnames + filenames:
            p = Path(dirpath) / name
            if p.is_symlink():
                target = _realpath(p)
                # Outside the disposable set?
                try:
                    target.relative_to(root_real)
                except ValueError:
                    bad.append(p)
                    continue
                # Inside a live-store sentinel, even if the
                # within-root check passed?
                for sentinel in LIVE_STORE_SENTINELS:
                    if _path_is_under(target, Path(sentinel)):
                        bad.append(p)
                        break
    return bad


def scrubbed_env(snap: Path) -> dict[str, str]:
    """Build the env dict for a subprocess call to the MPM binary.

    Per C4(8):
      - Inherit only the SAFE_ENV_KEYS from the harness's environment.
      - Set MPM_WORKSPACE to the resolved realpath of the snapshot.
      - Set HOME to a per-snapshot fake-home, pre-created by the
        harness, so MPM's $HOME/.mpm fallback (config.go:529-597)
        resolves to the fake-home and not the operator's real home.
      - Explicitly UNSET DANGEROUS_INHERITED_KEYS. An empty-string value
        for MPM_DB_LOCK is still picked up as a non-empty override at
        handlers_shred_database_reset.go:397, so the only safe thing
        is to leave the key absent.
      - Hardening (priority 1 of HAL's review): also scrub any
        env var whose name starts with one of the
        DANGEROUS_ENV_PREFIXES. This is a forward-compatible guard
        against future MPM overrides that may not yet be in
        DANGEROUS_INHERITED_KEYS.
    """
    fake_home = snap / "fakehome"
    fake_home.mkdir(parents=True, exist_ok=True, mode=0o700)
    (fake_home / ".mpm").mkdir(parents=True, exist_ok=True, mode=0o700)

    env: dict[str, str] = {}
    for k in SAFE_ENV_KEYS:
        if k in os.environ:
            env[k] = os.environ[k]
    env["MPM_WORKSPACE"] = str(snap)
    env["HOME"] = str(fake_home)
    # DANGEROUS_INHERITED_KEYS and DANGEROUS_ENV_PREFIXES are
    # intentionally absent. Do not set them to ""; the empty string
    # is still a non-empty override at
    # handlers_shred_database_reset.go:397. The prefix guard catches
    # any future MPM_*/OLLAMA_*/OPENAI_*/ANTHROPIC_*/OPENROUTER_*
    # overrides the harness has not been told about.
    return env


def write_mpm_config(snap: Path, profile: dict[str, str], components: dict[str, str]) -> Path:
    """Write an mpm_config.json into the snapshot workspace, with the
    target embedding profile. Per C2 the config file location is
    `<workspace>/mpm_config.json` and the file is consumed by
    `internal/core/config/config.go`.

    The harness writes a minimal valid config; the live DB's config
    is never read because MPM_WORKSPACE is set to the snapshot.
    """
    import json
    cfg = {
        "profiles": profile,
        "components": components,
    }
    path = snap / "mpm_config.json"
    path.write_text(json.dumps(cfg, indent=2))
    os.chmod(path, 0o600)
    return path
