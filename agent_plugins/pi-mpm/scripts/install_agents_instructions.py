#!/usr/bin/env python3
"""Idempotent installer for the MPM behavioral-protocol snippet into a
Pi-compatible AGENTS.md (or CLAUDE.md).

Pi's documented startup context files (per pi docs):

  1. ~/.pi/agent/AGENTS.md (or CLAUDE.md) — global, all projects
  2. AGENTS.md in any parent directory of cwd (walking up)
  3. AGENTS.md in cwd (current project)

This installer targets AGENTS.md by default; use --filename CLAUDE.md
if you prefer that name. Scope (user vs project) is selectable.

This script is a thin adaptation of the opencode-mpm installer with
Pi defaults baked in. The marker convention and idempotence contract
match the opencode-mpm installer exactly:

- Fresh target: write header + managed block.
- Existing managed block: no-op (or replace if content differs).
- Existing target with no managed block: append, preserving user
  content above and below markers.
- Corrupted target (one marker without the other): abort safely
  with backup.
- Uninstall: removes the managed block, unlinks the file if it
  would be empty otherwise, with backup-before-mutate.

Exit codes:
  0 - success / no-op
  1 - install / write error
  2 - corrupted target refusal
  3 - invalid argument

Usage:
    # Global (host-wide) install — writes ~/.pi/agent/AGENTS.md:
    python3 install_agents_instructions.py \\
        --snippet /path/to/AGENTS.md.snippet

    # Project-scope install — writes ./AGENTS.md:
    python3 install_agents_instructions.py \\
        --scope project \\
        --snippet /path/to/AGENTS.md.snippet

    # Custom target:
    python3 install_agents_instructions.py \\
        --target /path/to/AGENTS.md \\
        --snippet /path/to/AGENTS.md.snippet

    # Uninstall:
    python3 install_agents_instructions.py --uninstall
"""

from __future__ import annotations

import argparse
import os
import re
import shutil
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path

# Marker convention — must match opencode-mpm installer.
MARKER_BEGIN = "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->"
MARKER_END = "<!-- END MPM-MANAGED SECTION:pi-instructions -->"


def _backup(path: Path) -> Path | None:
    if not path.exists():
        return None
    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    bak = path.with_name(f"{path.name}.bak.{ts}")
    shutil.copy2(path, bak)
    return bak


def _atomic_write(path: Path, content: str) -> None:
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


def _render_managed(snippet: str) -> str:
    return f"{MARKER_BEGIN}\n{snippet}{MARKER_END}\n"


def _find_managed(text: str) -> tuple[int, int] | None:
    begin = text.find(MARKER_BEGIN)
    if begin == -1:
        return None
    end = text.find(MARKER_END, begin + len(MARKER_BEGIN))
    if end == -1:
        return None
    end_line_end = text.find("\n", end)
    if end_line_end == -1:
        end_line_end = len(text)
    else:
        end_line_end += 1
    return begin, end_line_end


def install(target: Path, snippet: str) -> str:
    block = _render_managed(snippet)
    if not target.exists():
        _atomic_write(target, block)
        return "fresh"

    text = target.read_text(encoding="utf-8")
    if (MARKER_BEGIN in text) != (MARKER_END in text):
        return "aborted"

    rng = _find_managed(text)
    if rng is not None:
        begin, end = rng
        new_text = text[:begin] + block + text[end:]
        if new_text == text:
            return "no-op"
        _backup(target)
        _atomic_write(target, new_text)
        return "replaced"

    sep = "" if text.endswith("\n") else "\n"
    new_text = text + sep + "\n" + block
    _backup(target)
    _atomic_write(target, new_text)
    return "appended"


def uninstall(target: Path) -> str:
    if not target.exists():
        return "absent"

    text = target.read_text(encoding="utf-8")
    if (MARKER_BEGIN in text) != (MARKER_END in text):
        return "aborted"

    rng = _find_managed(text)
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


def _resolve_target(scope: str, target: str | None, target_dir: str | None, filename: str) -> Path:
    if target:
        return Path(target).expanduser().resolve()
    if target_dir:
        return (Path(target_dir).expanduser().resolve() / filename)
    if scope == "user":
        return (Path.home() / ".pi" / "agent" / filename).expanduser().resolve()
    if scope == "project":
        return (Path.cwd() / filename).resolve()
    raise ValueError(f"unknown scope: {scope}")


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--scope", choices=("user", "project"), default="user",
                    help="user (default; ~/.pi/agent/<file>) or project (cwd/<file>)")
    ap.add_argument("--target", help="Explicit target file (overrides --scope).")
    ap.add_argument("--target-dir", help="Directory to install into; writes to <dir>/<filename>. Overrides --scope.")
    ap.add_argument("--filename", default="AGENTS.md",
                    help="Filename within the resolved directory (default: AGENTS.md).")
    ap.add_argument("--snippet", help="Path to snippet file (required for install).")
    ap.add_argument("--uninstall", action="store_true")
    args = ap.parse_args(argv)

    try:
        target = _resolve_target(args.scope, args.target, args.target_dir, args.filename)
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
