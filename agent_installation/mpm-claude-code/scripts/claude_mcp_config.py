#!/usr/bin/env python3
"""claude_mcp_config.py — read-only inspection of Claude Code MCP config.

This module NEVER writes Claude Code's configuration. Registration and
removal go through Claude Code's supported CLI (`claude mcp add` /
`claude mcp remove`); this helper exists so the installer can *decide*
between no-op, update, and warn — and so that decision is testable without
a live Claude Code process.

It is also the single place that knows where each scope actually lives,
which is the fact the original adapter got wrong:

    user    ~/.claude.json                       -> mcpServers.mpm
    local   ~/.claude.json                       -> projects.<abs cwd>.mcpServers.mpm
    project <project>/.mcp.json                   -> mcpServers.mpm

`~/.claude/.mcp.json` is **not** a discovery path in any scope. Writing
there produced a config Claude Code silently ignored.

Precedence is local > project > user, so a local or project `mpm` entry
shadows the user-scope registration for that project.

CLI:
    desired-entry --template T --home H
    user-entry    --config PATH
    find-shadows  --config PATH --project-dir D --template T --home H
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

# Entry name this adapter owns. Nothing else is ever inspected or removed.
SERVER_NAME = "mpm"


def desired_entry(template_path: str | Path, home: str | Path) -> dict:
    """Return the canonical `mpm` server entry from the adapter template.

    The template stays the source of truth for the env block; this is how
    the installer honours it without re-implementing it in shell.
    """
    raw = Path(template_path).read_text().replace("${HOME}", str(home))
    data = json.loads(raw)
    data.pop("_comment", None)
    return data["mcpServers"][SERVER_NAME]


def _load(path: Path) -> dict:
    """Load a JSON config, tolerating absence and corruption."""
    try:
        with open(path) as f:
            data = json.load(f)
    except (OSError, json.JSONDecodeError):
        return {}
    return data if isinstance(data, dict) else {}


def user_entry(config_path: str | Path) -> dict | None:
    """The user-scope `mpm` entry, or None if not registered."""
    return _load(Path(config_path)).get("mcpServers", {}).get(SERVER_NAME)


def local_entry(config_path: str | Path, project_dir: str | Path) -> dict | None:
    """The local-scope `mpm` entry for one project directory.

    Local scope lives inside the user config under a `projects` map keyed
    by absolute path, which is why a local registration is invisible to a
    user-scope install and shadows it.
    """
    key = str(Path(project_dir).resolve())
    entry = _load(Path(config_path)).get("projects", {}).get(key, {})
    return entry.get("mcpServers", {}).get(SERVER_NAME)


def project_entry(project_dir: str | Path) -> dict | None:
    """The project-scope `mpm` entry from <project>/.mcp.json, or None."""
    return _load(Path(project_dir) / ".mcp.json").get("mcpServers", {}).get(SERVER_NAME)


def classify(entry: dict, desired: dict) -> str:
    """Label an existing entry relative to the canonical one.

    canonical — identical command and env; redundant with user scope.
    legacy     — an MPM entry the template would produce, but carrying no
                 env block (the shape produced by a manual `claude mcp add`
                 without -e, and by the obsolete ~/.claude/.mcp.json era).
    custom     — an `mpm` entry whose command or env does not come from
                 this adapter. Treated as user-owned: never auto-removed.
    foreign    — not an MPM entry at all.
    """
    cmd = entry.get("command", "")
    if not ("mpm-mcp" in cmd or cmd.rsplit("/", 1)[-1].startswith("mpm")):
        return "foreign"
    env = entry.get("env") or {}
    want_env = desired.get("env") or {}
    if cmd == desired.get("command") and env == want_env:
        return "canonical"
    # An MPM command with no env, or with a subset of the canonical env
    # keys and no foreign keys, is this adapter's own work.
    if all(k in want_env for k in env):
        return "legacy"
    return "custom"


def find_shadows(
    config_path: str | Path,
    project_dir: str | Path,
    desired: dict,
    scan_all_projects: bool = False,
) -> list[dict]:
    """Locate local/project-scope `mpm` entries that shadow user scope.

    Returns one record per shadowing entry. `scan_all_projects` widens the
    local-scope search to every project in the config, which is what a user
    wants before removing anything: a shadow only matters for the project
    it is attached to, but an operator migrating away from local scope
    needs to know about all of them.
    """
    found: list[dict] = []
    config_path, project_dir = Path(config_path), Path(project_dir)

    def add(scope: str, file: Path, project: str, entry: dict | None) -> None:
        if entry is None:
            return
        found.append({
            "scope": scope,
            "file": str(file),
            "project": project,
            "command": entry.get("command", ""),
            "env": entry.get("env") or {},
            "classification": classify(entry, desired),
        })

    if scan_all_projects:
        for path, cfg in _load(config_path).get("projects", {}).items():
            add("local", config_path, path,
                (cfg.get("mcpServers") or {}).get(SERVER_NAME))
    else:
        add("local", config_path, str(project_dir.resolve()),
            local_entry(config_path, project_dir))

    add("project", project_dir / ".mcp.json", str(project_dir.resolve()),
        project_entry(project_dir))
    return found


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("desired-entry")
    p.add_argument("--template", required=True)
    p.add_argument("--home", required=True)

    p = sub.add_parser("user-entry")
    p.add_argument("--config", required=True)

    p = sub.add_parser("find-shadows")
    p.add_argument("--config", required=True)
    p.add_argument("--project-dir", required=True)
    p.add_argument("--template", required=True)
    p.add_argument("--home", required=True)
    p.add_argument("--all-projects", action="store_true")

    a = ap.parse_args(argv)
    if a.cmd == "desired-entry":
        out = desired_entry(a.template, a.home)
    elif a.cmd == "user-entry":
        out = user_entry(a.config)
    else:
        out = find_shadows(a.config, a.project_dir,
                           desired_entry(a.template, a.home), a.all_projects)
    json.dump(out, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())