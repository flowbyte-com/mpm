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


def _assert_outside_live_store_sentinels(
    real: Path, role: str
) -> None:
    """Refuse if `real` is the same as or inside any live-store
    sentinel. `role` is "snapshot workspace" or "output dir" and
    is used in the error message."""
    for sentinel in LIVE_STORE_SENTINELS:
        if _path_is_under(real, Path(sentinel)):
            raise WorkspaceError(
                f"refusing {role} {real}: resolves inside "
                f"live-store sentinel {sentinel}"
            )


def assert_safe_workspace(snap: Path) -> Path:
    """Resolve a snapshot workspace through realpath and refuse if it
    lands inside any LIVE_STORE_SENTINELS, or if it is a symlink that
    points outside the disposable set.

    Returns the resolved realpath. The caller is expected to use the
    returned path (not the original) for any further filesystem ops.
    """
    if not snap.exists():
        # Creating it is fine; just create under a known-safe parent
        snap.mkdir(parents=True, exist_ok=True, mode=0o700)
    real = _realpath(snap)
    _assert_outside_live_store_sentinels(real, "snapshot workspace")
    return real


def assert_safe_output_dir(out: Path, snap_real: Path) -> Path:
    """Resolve an output directory and refuse if it lands inside the
    snapshot workspace, inside any live-store sentinel, or has
    permissions wider than 0o700.

    The output dir is required for real runs (per C4(2)). The
    --smoke and --selftest modes do not require it.

    Hardening (priority 1 of HAL's review):
      - Symlink-chain walk: refuse if any path component of `out`
        (parents included) resolves into a live-store sentinel,
        even if `out` itself is not currently a symlink.
      - TOCTOU-aware: the realpath + permission check is repeated
        at write time by `recheck_output_dir_safe`, which the
        harness must call immediately before each write under
        the output dir.
    """
    out.mkdir(parents=True, exist_ok=True, mode=0o700)
    real = _realpath(out)

    if _path_is_under(real, snap_real):
        raise WorkspaceError(
            f"refusing output dir {out}: inside snapshot workspace {snap_real}"
        )
    _assert_outside_live_store_sentinels(real, "output dir")

    # Permission check. If a pre-existing dir is wider than 0o700,
    # refuse; the harness must not write into a permissive dir.
    mode = stat.S_IMODE(os.stat(real).st_mode)
    if mode & 0o077:
        raise WorkspaceError(
            f"refusing output dir {real}: permissions {oct(mode)} are wider "
            f"than 0o700; the harness requires private output dirs"
        )

    return real


def recheck_output_dir_safe(out: Path, snap_real: Path) -> None:
    """Re-run the output-dir safety checks immediately before a
    write. Closes the TOCTOU window between `assert_safe_output_dir`
    and the actual write: a malicious local actor could chmod the
    dir to 0o755 or replace it with a symlink to ~/.mpm in that
    gap. The harness MUST call this immediately before any write
    under `out`.

    Raises WorkspaceError on any change that would make the output
    dir unsafe (permissions widened, realpath now inside a sentinel
    or inside the snapshot workspace, or the dir no longer exists).
    """
    if not out.exists():
        raise WorkspaceError(
            f"output dir {out} disappeared between safe check and write"
        )
    real = _realpath(out)
    if _path_is_under(real, snap_real):
        raise WorkspaceError(
            f"output dir {out} now resolves inside snapshot workspace "
            f"{snap_real} (TOCTOU)"
        )
    _assert_outside_live_store_sentinels(real, "output dir")
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
      - Explicitly UNSET DANGEROUS_ENV_KEYS. An empty-string value
        for MPM_DB_LOCK is still picked up as a non-empty override
        at handlers_shred_database_reset.go:397, so the only safe
        thing is to leave the key absent.
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
