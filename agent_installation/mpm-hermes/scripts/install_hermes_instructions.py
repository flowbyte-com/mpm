#!/usr/bin/env python3
"""Idempotent installer for the MPM behavioral-protocol snippet into a
project-level `.hermes.md` (or `HERMES.md`) at a given target directory.

Hermes walks up from cwd looking for `.hermes.md` / `HERMES.md` to git
root. The user-level persona lives in `~/.hermes/SOUL.md` and is loaded
raw by `load_soul_md()`; do NOT touch SOUL.md from this installer.
This installer writes ONLY the project-level behavioral protocol file.

Marker convention: unlike Claude Code / OpenCode, Hermes does not have
a managed-marker convention. This installer uses a leading header
comment block (HTML comments at file start) as a stable marker so
re-runs can detect and replace the MPM section idempotently.

Contract:
- Fresh target: write header + snippet body.
- Existing file with MPM header marker: replace the MPM section,
  preserving user content above and below.
- Existing file without MPM header marker: append snippet at the end.
- Corrupted target (unterminated MPM marker): abort safely with backup.
- Uninstall: strip the MPM section, unlink file if would be empty,
  with backup before mutation.

Exit codes:
  0 - success / no-op
  1 - install / write error
  2 - corrupted target refusal
  3 - invalid argument

Usage:
    python3 install_hermes_instructions.py \
        --target-dir /path/to/project \
        --snippet /path/to/hermes.md.snippet

    # Or write to an explicit file (useful for tests):
    python3 install_hermes_instructions.py \
        --target /path/to/.hermes.md \
        --snippet /path/to/hermes.md.snippet

    # Uninstall:
    python3 install_hermes_instructions.py \
        --target-dir /path/to/project \
        --uninstall
"""

from __future__ import annotations

import argparse
import shutil
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path

# Header marker — written at the top of the produced .hermes.md.
# Hermes doesn't have a managed-block convention; we use HTML comments
# so they remain invisible in rendered output but stable for grep.
HERMES_MPM_MARKER_BEGIN = "<!-- BEGIN MPM-MANAGED BLOCK:mpm-hermes -->"
HERMES_MPM_MARKER_END = "<!-- END MPM-MANAGED BLOCK:mpm-hermes -->"

# Default file name in target dir if --target not given.
DEFAULT_FILENAME = ".hermes.md"


def _backup(path: Path) -> Path | None:
    """Copy path → path.bak.YYYYMMDDTHHMMSSZ. Returns backup path or None."""
    if not path.exists():
        return None
    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    bak = path.with_name(f"{path.name}.bak.{ts}")
    shutil.copy2(path, bak)
    return bak


def _atomic_write(path: Path, content: str) -> None:
    """Write content to path atomically via temp + rename. fsync best-effort."""
    parent = path.parent
    parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(prefix=path.name + ".", dir=parent)
    try:
        with open(fd, "w", encoding="utf-8") as f:
            f.write(content)
        Path(tmp_name).replace(path)
    except Exception:
        try:
            Path(tmp_name).unlink()
        except OSError:
            pass
        raise


def _render_block(snippet: str) -> str:
    """Wrap snippet body in marker comments. Idempotent re-render."""
    return (
        f"{HERMES_MPM_MARKER_BEGIN}\n"
        f"{snippet}"
        f"{HERMES_MPM_MARKER_END}\n"
    )


def _find_mpm_section(text: str) -> tuple[int, int] | None:
    """Return (begin_idx, end_idx) of marker block (inclusive of end line),
    or None if absent. Refuses to return ranges for corrupted markers."""
    begin = text.find(HERMES_MPM_MARKER_BEGIN)
    if begin == -1:
        return None
    end = text.find(HERMES_MPM_MARKER_END, begin + len(HERMES_MPM_MARKER_BEGIN))
    if end == -1:
        return None
    end_line_end = text.find("\n", end)
    if end_line_end == -1:
        end_line_end = len(text)
    else:
        end_line_end += 1
    return begin, end_line_end


def install(target: Path, snippet: str) -> str:
    """Install the MPM block into target. Returns status string.

    Status values: 'fresh', 'replaced', 'appended', 'no-op', 'aborted'
    """
    block = _render_block(snippet)

    if not target.exists():
        _atomic_write(target, block)
        return "fresh"

    text = target.read_text(encoding="utf-8")

    # Corrupted target check: one marker without the other.
    has_begin = HERMES_MPM_MARKER_BEGIN in text
    has_end = HERMES_MPM_MARKER_END in text
    if has_begin != has_end:
        return "aborted"

    rng = _find_mpm_section(text)
    if rng is not None:
        begin, end = rng
        new_text = text[:begin] + block + text[end:]
        if new_text == text:
            return "no-op"
        _backup(target)
        _atomic_write(target, new_text)
        return "replaced"

    # No MPM block present — append.
    sep = "" if text.endswith("\n") else "\n"
    new_text = text + sep + "\n" + block
    _backup(target)
    _atomic_write(target, new_text)
    return "appended"


def uninstall(target: Path) -> str:
    """Remove the MPM block from target. Returns status string.

    Status: 'removed', 'absent', 'aborted', 'empty-unlinked'
    """
    if not target.exists():
        return "absent"

    text = target.read_text(encoding="utf-8")
    has_begin = HERMES_MPM_MARKER_BEGIN in text
    has_end = HERMES_MPM_MARKER_END in text
    if has_begin != has_end:
        return "aborted"

    rng = _find_mpm_section(text)
    if rng is None:
        return "absent"

    begin, end = rng
    new_text = (text[:begin] + text[end:]).rstrip() + "\n"
    _backup(target)
    if not new_text.strip():
        target.unlink()
        return "empty-unlinked"
    _atomic_write(target, new_text)
    return "removed"


def _resolve_target(target: str | None, target_dir: str | None) -> Path:
    if target:
        return Path(target).expanduser().resolve()
    if target_dir:
        d = Path(target_dir).expanduser().resolve()
        return d / DEFAULT_FILENAME
    raise ValueError("either --target or --target-dir is required")


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--target", help="Explicit target file (overrides --target-dir).")
    ap.add_argument("--target-dir", help="Directory to install into; writes to <dir>/.hermes.md.")
    ap.add_argument("--snippet", help="Path to snippet file (required for install).")
    ap.add_argument("--uninstall", action="store_true", help="Remove the MPM block instead of installing it.")
    args = ap.parse_args(argv)

    try:
        target = _resolve_target(args.target, args.target_dir)
    except ValueError as e:
        print(f"error: {e}", file=sys.stderr)
        return 3

    try:
        if args.uninstall:
            status = uninstall(target)
            print(f"uninstall: {status} ({target})")
            return 0 if status != "aborted" else 2

        if not args.snippet:
            print("error: --snippet is required for install", file=sys.stderr)
            return 3

        snippet_path = Path(args.snippet).expanduser().resolve()
        if not snippet_path.is_file():
            print(f"error: snippet not found: {snippet_path}", file=sys.stderr)
            return 1

        snippet = snippet_path.read_text(encoding="utf-8")
        status = install(target, snippet)
        print(f"install: {status} ({target})")
        return 0 if status != "aborted" else 2
    except OSError as e:
        print(f"write error: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
