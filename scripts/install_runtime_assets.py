#!/usr/bin/env python3
"""install_runtime_assets.py — provision runtime definition assets.

WHY THIS EXISTS
---------------

`mode/*.md`, `persona/*.md`, and `drills/*.yaml` are loaded at runtime
(see internal/core/router.go:97-98, internal/core/mode.go:44,
internal/core/persona.go:41, cmd/mpm/drill_cmds.go:96). Historically
they were git-tracked source sitting at the runtime root, which only
worked because the checkout IS `~/.mpm`. If the source checkout ever
moves to e.g. `~/src/mpm`, a pure runtime root at `~/.mpm` would have
no mode, persona, or drill definitions at all.

This script changes the OWNERSHIP of those directories without changing
any runtime lookup path. `$MPM_WORKSPACE/mode/`, `.../persona/`, and
`.../drills/` remain the only places the binaries look. The router,
CLI, MCP server, scheduler, and drill runner are untouched.

Both install surfaces call this one script:

    make install            -> Makefile target `install-runtime-assets`
    ./install.sh            -> phase_runtime_assets

`make build`, `make test`, and `make release-gate` MUST NOT invoke it.
A build is not a deployment.

OWNERSHIP MODEL
---------------

Three files own the same directory, and conflating them is how upgrades
destroy operator work. The manifest is what makes the difference
recoverable:

    stock + unmodified   MPM provisioned the file; refresh it when
                         upstream changes (CASE D).

    stock + modified     operator edited a provisioned file. Preserve
                         it and report the conflict (CASE E).

    custom               the file is not in the source tree at all.
                         Never touched, never listed in the manifest.

Ownership is recorded in `<runtime-root>/config/runtime-assets.json`,
which records the content hash of what was last installed per relative
path. That hash is the only durable question the next run has to
answer: "is what I find on disk still what I put there?"

Two extremes are explicitly rejected. Blind `cp -f` destroys local
edits. Copy-if-missing means a stock definition can never receive an
upstream fix. The cases below are the space between them.

MANIFEST SAFETY
---------------

The manifest is written atomically (`<target>.tmp` + `os.replace`), the
same idiom used by internal/scheduler/state.go:118-129 and
internal/core/config/config.go:177-181. It contains only bounded
metadata: relative paths and SHA-256 hex digests. It never contains
absolute source-checkout paths, file contents, database rows, or
credentials.

A malformed, partial, or unknown-version manifest FAILS CLOSED. It is
never silently treated as "fresh", because a fresh manifest plus a
blind copy is exactly how local edits get destroyed.

EXIT CODES
----------

    0   reconciliation completed (conflicts may still be reported)
    1   usage / environment error
    2   manifest unreadable, malformed, or unknown version (fail closed)
    3   filesystem error while provisioning
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import sys
import tempfile
from pathlib import Path

# Version of the manifest schema. Bump when the on-disk shape changes.
# An unknown version is refused rather than migrated implicitly: a
# migration path from a schema this installer does not understand would
# be guesswork about ownership, and guessing wrong overwrites operator
# files.
MANIFEST_VERSION = 1

MANIFEST_RELPATH = Path("config") / "runtime-assets.json"

# Source-managed asset directories. The value is the file suffix that
# marks a provisionable definition file.
#
# README.md is deliberately INCLUDED. It is genuine operator
# documentation for a runtime directory the operator now owns and edits,
# and every runtime loader already rejects it as a selectable
# definition via internal.IsDefinitionFile (internal/core/
# definition.go:66), consulted by router_loader.go:85, mode.go:137, and
# persona.go:124. Shipping the docs alongside the definitions they
# document is more useful than shipping definitions with no explanation.
ASSET_DIRS = {
    "mode": ".md",
    "persona": ".md",
    "drills": ".yaml",
}

# Runtime directories are created 0755 and definition files 0644: owned
# by the installing user and freely editable, because the operator is
# expected to customize them. This deliberately matches what a
# git checkout produces (see the umask policy note in
# internal/core/config/umask.go:50 — mode/ and persona/ are operator
# config, and config/fileperms.go:22-25 explicitly excludes them from
# the 0700/0600 sensitive-state tightening).
DIR_MODE = 0o755
FILE_MODE = 0o644

# Runtime root is private; it holds the database and backups.
RUNTIME_ROOT_MODE = 0o700


class ManifestError(Exception):
    """Manifest is unreadable, malformed, or of an unknown version.

    Always fatal. See MANIFEST SAFETY above.
    """


class Result:
    """Accumulates per-asset outcomes and the operator-facing report."""

    def __init__(self) -> None:
        self.actions: list[tuple[str, str, str]] = []  # (kind, relpath, detail)

    def add(self, kind: str, relpath: str, detail: str = "") -> None:
        self.actions.append((kind, relpath, detail))

    def conflicts(self) -> list[tuple[str, str, str]]:
        return [a for a in self.actions if a[0] == "conflict"]

    def changed(self) -> list[tuple[str, str, str]]:
        return [a for a in self.actions if a[0] in ("provisioned", "refreshed", "adopted")]


# --------------------------------------------------------------------------
# Hashing
# --------------------------------------------------------------------------


def sha256_file(path: Path) -> str:
    """Return the hex SHA-256 of a file's contents.

    Chunked so a large asset cannot blow the interpreter's memory
    budget; definition files are small today but this is cheap
    insurance for operator-authored files that land here.
    """
    h = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


# --------------------------------------------------------------------------
# Atomic writes
# --------------------------------------------------------------------------


def atomic_write_bytes(target: Path, data: bytes, mode: int = FILE_MODE) -> None:
    """Write `data` to `target` atomically.

    Writes a sibling temp file in the SAME directory (so os.replace is a
    same-filesystem rename and therefore atomic per POSIX rename(2)),
    fsyncs it, then renames into place. A concurrent reader — an agent
    reading a mode file while an install runs — sees either the old
    bytes or the new bytes, never a truncated file.

    The temp file is created with O_EXCL semantics via mkstemp so two
    concurrent installs cannot collide on the same scratch name.
    """
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(
        dir=str(target.parent), prefix=target.name + ".", suffix=".tmp"
    )
    tmp = Path(tmp_name)
    try:
        with os.fdopen(fd, "wb") as fh:
            fh.write(data)
            fh.flush()
            os.fsync(fh.fileno())
        os.chmod(tmp, mode)
        os.replace(tmp, target)
    except BaseException:
        # Never leave a partial scratch file behind for the next run to
        # trip over, and never leave the target half-written.
        try:
            tmp.unlink()
        except OSError:
            pass
        raise
    # Durability of the rename itself.
    try:
        dir_fd = os.open(str(target.parent), os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError:
        # Directory fsync is not supported everywhere (and is not
        # required for correctness of the rename itself). The rename has
        # already happened at this point.
        pass


def atomic_write_manifest(target: Path, payload: dict) -> None:
    """Serialize and atomically install the manifest.

    The manifest carries no mode bits of its own worth speaking of; it
    lives inside the runtime root's private config directory, so it is
    written 0600 like other internal state.
    """
    text = json.dumps(payload, indent=2, sort_keys=True) + "\n"
    atomic_write_bytes(target, text.encode("utf-8"), mode=0o600)


# --------------------------------------------------------------------------
# Manifest
# --------------------------------------------------------------------------


def load_manifest(path: Path) -> dict:
    """Load and validate the manifest, failing closed on any doubt.

    A missing manifest is the legitimate fresh-install case and yields an
    empty record set. Anything PRESENT but not exactly what this version
    expects raises ManifestError. The distinction matters: "absent"
    means we have never claimed ownership of anything, while "present
    and unreadable" means we may have — and treating that as fresh would
    authorize overwrites we never actually know are safe.
    """
    if not path.exists():
        return {"version": MANIFEST_VERSION, "assets": {}}

    try:
        raw = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ManifestError(f"cannot read manifest {path}: {exc}") from exc

    if raw.strip() == "":
        # An interrupted write that predates the atomic-rename guarantee
        # can leave a zero-length file. Same reasoning as unreadable:
        # we cannot prove we own nothing.
        raise ManifestError(
            f"manifest {path} is empty; refusing to treat it as a fresh install "
            "because ownership of existing files cannot be proven. "
            "Restore it from backup, or move it aside to force an explicit re-adoption."
        )

    try:
        data = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ManifestError(
            f"manifest {path} is not valid JSON ({exc}); refusing to continue. "
            "Restore it from backup, or move it aside to force an explicit re-adoption."
        ) from exc

    if not isinstance(data, dict):
        raise ManifestError(f"manifest {path} must be a JSON object, got {type(data).__name__}")

    version = data.get("version")
    if version != MANIFEST_VERSION:
        raise ManifestError(
            f"manifest {path} declares version {version!r}, this installer understands "
            f"{MANIFEST_VERSION}. Refusing to continue rather than guess at ownership. "
            "Upgrade the installer, or move the manifest aside to re-adopt explicitly."
        )

    assets = data.get("assets")
    if not isinstance(assets, dict):
        raise ManifestError(f"manifest {path} has no valid 'assets' object")

    for rel, entry in assets.items():
        if not isinstance(entry, dict) or not isinstance(entry.get("sha256"), str):
            raise ManifestError(
                f"manifest {path} entry {rel!r} is malformed (expected an object with a "
                "string 'sha256')"
            )
        # Ownership records are runtime-root-relative by construction.
        # A leading slash or a '..' segment would let a corrupted
        # manifest steer reconciliation outside the runtime root.
        p = Path(rel)
        if p.is_absolute() or ".." in p.parts:
            raise ManifestError(
                f"manifest {path} entry {rel!r} is not a safe runtime-relative path"
            )

    return {"version": MANIFEST_VERSION, "assets": dict(assets)}


def save_manifest(path: Path, manifest: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    atomic_write_manifest(path, manifest)


# --------------------------------------------------------------------------
# Source discovery
# --------------------------------------------------------------------------


def discover_source_assets(source_root: Path) -> list[str]:
    """Return sorted runtime-relative paths of all source-managed assets.

    Sorted so reconciliation order — and therefore the operator-facing
    report — is deterministic across runs.
    """
    found: list[str] = []
    for subdir, suffix in ASSET_DIRS.items():
        d = source_root / subdir
        if not d.is_dir():
            continue
        for entry in sorted(d.iterdir()):
            if entry.is_file() and entry.suffix == suffix:
                found.append(f"{subdir}/{entry.name}")
    return sorted(found)


# --------------------------------------------------------------------------
# Reconciliation
# --------------------------------------------------------------------------


def _same_file(a: Path, b: Path) -> bool:
    """True when a and b are the same filesystem object.

    Handles the transitional canonical layout, where the source checkout
    IS the runtime root and `mode/default.md` on the source side is the
    very same inode as its destination. Without this check the copier
    would read and rewrite a file with itself.
    """
    try:
        return a.samefile(b)
    except OSError:
        # One of them does not exist yet, or is unreadable. Fall back to
        # a resolved-path comparison, which still catches the case where
        # neither exists yet but the paths are textually the same file.
        try:
            return os.path.realpath(a) == os.path.realpath(b)
        except OSError:
            return False


def reconcile(source_root: Path, runtime_root: Path, dry_run: bool = False) -> Result:
    """Reconcile source-managed assets into the runtime root.

    Implements the ownership cases:

      A  destination absent, never managed      -> provision, record
      B  destination present, no record,        -> adopt without rewriting
         byte-identical to source
      C  destination present, no record,        -> preserve, report as
         differs from source                      operator-owned
      D  destination matches its recorded hash  -> source unchanged: no-op
                                                   source changed:   refresh
      E  destination differs from recorded hash -> preserve, report conflict
      F  source removed a managed file          -> retain runtime copy,
                                                   drop the record
      G  destination for a managed file is      -> respect it as an
         missing                                  intentional local deletion

    Case G is the conservative one. Resurrecting a file an operator
    deliberately deleted is surprising and, for a mode or persona,
    silently reactivates behavior they turned off. The manifest record
    is intentionally RETAINED in this case so the deletion stays
    respected on every subsequent run instead of decaying back into
    case A on the next upgrade.
    """
    result = Result()
    manifest_path = runtime_root / MANIFEST_RELPATH
    manifest = load_manifest(manifest_path)
    records: dict[str, dict] = manifest["assets"]

    source_assets = discover_source_assets(source_root)
    source_set = set(source_assets)

    if not dry_run and not runtime_root.exists():
        runtime_root.mkdir(parents=True, exist_ok=True)
        try:
            os.chmod(runtime_root, RUNTIME_ROOT_MODE)
        except OSError:
            pass

    for rel in source_assets:
        src = source_root / rel
        dst = runtime_root / rel
        rec = records.get(rel)
        src_hash = sha256_file(src)

        if not dst.exists():
            if rec is not None:
                # CASE G — previously managed, now absent. Treat as an
                # intentional local deletion; do not resurrect.
                result.add("kept-deleted", rel, "previously provisioned, now removed locally")
                continue

            # CASE A — absent and never managed. Provision it.
            # Directory creation is a write too: --dry-run must leave the
            # runtime root exactly as it found it, empty scaffolding
            # included, or the flag reports a no-op it did not perform.
            if not dry_run:
                dst.parent.mkdir(parents=True, exist_ok=True)
                try:
                    os.chmod(dst.parent, DIR_MODE)
                except OSError:
                    pass
                atomic_write_bytes(dst, src.read_bytes(), mode=FILE_MODE)
            records[rel] = {"sha256": src_hash}
            result.add("provisioned", rel, src_hash[:12])
            continue

        # The transitional case: source root == runtime root.
        if _same_file(src, dst):
            # Adopting in place is the only safe action. Copying a file
            # onto itself would truncate it first on most implementations.
            if rec is None:
                records[rel] = {"sha256": src_hash}
                result.add("adopted", rel, "source and destination are the same file")
            else:
                rec["sha256"] = src_hash
                result.add("unchanged", rel, "source and destination are the same file")
            continue

        dst_hash = sha256_file(dst)

        if rec is None:
            if dst_hash == src_hash:
                # CASE B — identical to source but unrecorded. Adopt it
                # WITHOUT rewriting, so the file's mtime stays put and
                # any tooling watching the directory sees no churn.
                records[rel] = {"sha256": src_hash}
                result.add("adopted", rel, "byte-identical to source, adopted without rewrite")
            else:
                # CASE C — differs from source and we never claimed it.
                # Claiming ownership would arm a future refresh that
                # overwrites operator work.
                result.add(
                    "operator-owned",
                    rel,
                    "differs from source and was never provisioned; preserving untouched",
                )
            continue

        if dst_hash != rec["sha256"]:
            # CASE E — we provisioned this, and it no longer matches what
            # we wrote. The operator edited it. Never overwrite.
            result.add(
                "conflict",
                rel,
                "locally modified since install; preserving. Upstream changes are NOT applied. "
                "Reconcile manually, or move the file aside and re-run to re-adopt upstream.",
            )
            continue

        # CASE D — destination is exactly what we last installed.
        if src_hash == rec["sha256"]:
            result.add("unchanged", rel, "up to date")
            continue

        # Upstream changed and our copy is untouched: safe refresh.
        previous = rec["sha256"]
        if not dry_run:
            atomic_write_bytes(dst, src.read_bytes(), mode=FILE_MODE)
        rec["sha256"] = src_hash
        result.add(
            "refreshed",
            rel,
            f"upstream changed {previous[:12]} -> {src_hash[:12]}; refreshed from source",
        )

    # CASE F — a managed file the source tree no longer ships.
    for rel in sorted(set(records) - source_set):
        dst = runtime_root / rel
        if not dst.exists():
            # Case G territory: operator removed it. Drop the record —
            # with no file and no source, there is nothing left to own.
            del records[rel]
            result.add("forgotten", rel, "managed file and source copy both absent")
            continue
        dst_hash = sha256_file(dst)
        if dst_hash == records[rel]["sha256"]:
            # Unmodified runtime copy of an upstream-deleted definition.
            # Retain it: the operator may be actively using it, and
            # deleting a working definition because upstream dropped it
            # is a destructive surprise. Ownership is released so the
            # file is never touched again.
            del records[rel]
            result.add(
                "unmanaged",
                rel,
                "no longer shipped upstream; runtime copy retained and ownership released",
            )
        else:
            del records[rel]
            result.add(
                "unmanaged",
                rel,
                "no longer shipped upstream and locally modified; retained untouched",
            )

    if not dry_run:
        save_manifest(manifest_path, {"version": MANIFEST_VERSION, "assets": records})

    return result


# --------------------------------------------------------------------------
# Reporting
# --------------------------------------------------------------------------


def report(result: Result, dry_run: bool) -> None:
    prefix = "[runtime-assets] DRY RUN " if dry_run else "[runtime-assets] "
    by_kind: dict[str, list[tuple[str, str]]] = {}
    for kind, rel, detail in result.actions:
        by_kind.setdefault(kind, []).append((rel, detail))

    if not result.actions:
        print(f"{prefix}nothing to reconcile; runtime definitions already match source")
        return

    for kind in (
        "provisioned",
        "refreshed",
        "adopted",
        "unchanged",
        "kept-deleted",
        "forgotten",
        "unmanaged",
        "operator-owned",
        "conflict",
    ):
        entries = by_kind.get(kind)
        if not entries:
            continue
        for rel, detail in entries:
            line = f"{prefix}{kind:>14}  {rel}"
            if detail:
                line += f"  ({detail})"
            print(line)

    conflicts = result.conflicts()
    if conflicts:
        print()
        print(
            f"{prefix}{len(conflicts)} file(s) have local modifications that upstream "
            "changes did not touch:"
        )
        for rel, _ in conflicts:
            print(f"{prefix}    - {rel}")
        print(
            f"{prefix}These files are operator-owned now. Nothing was overwritten. "
            "Review each diff, then either keep your version (delete the manifest entry "
            "by moving the file aside) or accept upstream (delete the file and re-run)."
        )


# --------------------------------------------------------------------------
# Entry point
# --------------------------------------------------------------------------


def default_runtime_root() -> Path:
    env = os.environ.get("MPM_WORKSPACE", "").strip()
    if env:
        return Path(env).expanduser()
    return Path.home() / ".mpm"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Provision mode/persona/drill definitions into an MPM runtime root.",
    )
    parser.add_argument(
        "--source-root",
        type=Path,
        default=Path(__file__).resolve().parent.parent,
        help="Checkout providing the stock definitions (default: this script's repository).",
    )
    parser.add_argument(
        "--runtime-root",
        type=Path,
        default=None,
        help="Destination runtime root (default: $MPM_WORKSPACE, else ~/.mpm).",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Report intended operations without writing anything.",
    )
    args = parser.parse_args(argv)

    source_root = args.source_root.expanduser().resolve()
    runtime_root = (
        args.runtime_root.expanduser().resolve()
        if args.runtime_root
        else default_runtime_root().resolve()
    )

    if not source_root.is_dir():
        print(f"[runtime-assets] FAIL: source root {source_root} is not a directory", file=sys.stderr)
        return 1

    try:
        result = reconcile(source_root, runtime_root, dry_run=args.dry_run)
    except ManifestError as exc:
        print(f"[runtime-assets] FAIL: {exc}", file=sys.stderr)
        return 2
    except OSError as exc:
        print(f"[runtime-assets] FAIL: filesystem error: {exc}", file=sys.stderr)
        return 3

    report(result, args.dry_run)
    return 0


if __name__ == "__main__":
    sys.exit(main())