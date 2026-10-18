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

    # Symlink check: if the original path differs from the realpath,
    # the original was a symlink. Reject any symlink whose target is
    # outside the disposable set (we don't have a concept of "inside
    # the disposable set" yet, but we do know that pointing into
    # LIVE_STORE_SENTINELS is forbidden).
    for sentinel in LIVE_STORE_SENTINELS:
        sentinel_real = _realpath(Path(sentinel))
        try:
            real.relative_to(sentinel_real)
            raise WorkspaceError(
                f"refusing to use snapshot workspace {snap}: "
                f"resolves inside live-store sentinel {sentinel_real}"
            )
        except ValueError:
            pass  # not inside this sentinel, keep checking

    return real


def assert_safe_output_dir(out: Path, snap_real: Path) -> Path:
    """Resolve an output directory and refuse if it lands inside the
    snapshot workspace, inside any live-store sentinel, or has
    permissions wider than 0o700.

    The output dir is required for real runs (per C4(2)). The
    --smoke and --selftest modes do not require it.
    """
    out.mkdir(parents=True, exist_ok=True, mode=0o700)
    real = _realpath(out)

    try:
        real.relative_to(snap_real)
        raise WorkspaceError(
            f"refusing output dir {out}: inside snapshot workspace {snap_real}"
        )
    except ValueError:
        pass

    for sentinel in LIVE_STORE_SENTINELS:
        sentinel_real = _realpath(Path(sentinel))
        try:
            real.relative_to(sentinel_real)
            raise WorkspaceError(
                f"refusing output dir {out}: inside live-store sentinel {sentinel_real}"
            )
        except ValueError:
            pass

    # Permission check. If a pre-existing dir is wider than 0o700,
    # refuse; the harness must not write into a permissive dir.
    mode = stat.S_IMODE(os.stat(real).st_mode)
    if mode & 0o077:
        raise WorkspaceError(
            f"refusing output dir {real}: permissions {oct(mode)} are wider "
            f"than 0o700; the harness requires private output dirs"
        )

    return real


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
    """
    root_real = _realpath(root)
    bad: list[Path] = []
    for dirpath, dirnames, filenames in os.walk(root_real, followlinks=False):
        for name in dirnames + filenames:
            p = Path(dirpath) / name
            if p.is_symlink():
                target = _realpath(p)
                try:
                    target.relative_to(root_real)
                except ValueError:
                    bad.append(p)
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
    """
    fake_home = snap / "fakehome"
    fake_home.mkdir(parents=True, exist_ok=True, mode=0o700)
    (fake_home / ".mpm").mkdir(parents=True, exist_ok=True, mode=0o700)

    env: dict[str, str] = {
        k: os.environ[k] for k in SAFE_ENV_KEYS if k in os.environ
    }
    env["MPM_WORKSPACE"] = str(snap)
    env["HOME"] = str(fake_home)
    # DANGEROUS_INHERITED_KEYS are intentionally absent. Do not set
    # them to ""; the empty string is still a non-empty override at
    # handlers_shred_database_reset.go:397.
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
