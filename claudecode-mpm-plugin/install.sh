#!/usr/bin/env bash
# install.sh — install claudecode-mpm-plugin/ into ../.claude/
#
# Always creates a project-local venv at $SRC/.venv and points the installed
# server at $SRC/.venv/bin/python3 so runtime deps are isolated.
# Resolves the mpm binary at install time (4-step fallback chain) and
# renders .mcp.json with the absolute path.
#
# Flags:
#   --symlink   (default) Symlink files; edits in source propagate.
#   --copy             Copy files; one-shot install, no link drift.
#   --uninstall        Remove .mcp.json, .claude/mpm-mcp/, .claude/skills/mpm/.

set -euo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)"
DST="$(cd "$SRC/.." && pwd)/.claude"
VENV="$SRC/.venv"
VENV_PY="$VENV/bin/python3"

ACTION="symlink"
if [ "${1:-}" = "--copy" ]; then ACTION="copy"; fi
if [ "${1:-}" = "--uninstall" ]; then ACTION="uninstall"; fi

resolve_mpm() {
    if [ -n "${MPM_BINARY:-}" ] && [ -x "$MPM_BINARY" ]; then
        echo "$MPM_BINARY"; return
    fi
    if command -v mpm >/dev/null 2>&1; then
        command -v mpm; return
    fi
    if [ -x "$SRC/../bin/mpm" ]; then
        echo "$SRC/../bin/mpm"; return
    fi
    if [ -x "$SRC/../mpm" ]; then
        echo "$SRC/../mpm"; return
    fi
    echo "error: could not locate 'mpm' binary (set \$MPM_BINARY or run 'make install')" >&2
    exit 1
}

uninstall() {
    rm -f "$SRC/../.mcp.json"
    rm -rf "$DST/mpm-mcp" "$DST/skills/mpm"
    echo "✓ Uninstalled."
}

if [ "$ACTION" = "uninstall" ]; then
    uninstall
    exit 0
fi

# Sanity checks
[ -f "$SRC/.mcp.json" ] || { echo "error: $SRC/.mcp.json missing"; exit 1; }
[ -f "$SRC/server.py" ] || { echo "error: $SRC/server.py missing"; exit 1; }
command -v python3 >/dev/null || { echo "error: python3 not in PATH"; exit 1; }

# Resolve mpm binary
MPM_PATH="$(resolve_mpm)"

# Create venv if missing
if [ ! -x "$VENV_PY" ]; then
    echo "Creating venv at $VENV ..."
    python3 -m venv "$VENV"
fi

# Install deps into venv
"$VENV_PY" -c 'import mcp' 2>/dev/null || {
    echo "Installing mcp SDK into venv..."
    "$VENV_PY" -m pip install --quiet -r "$SRC/requirements.txt"
}

# Create target dirs
mkdir -p "$DST/mpm-mcp" "$DST/skills/mpm"

# Link or copy source files
link_or_copy() {
    if [ "$ACTION" = "symlink" ]; then
        ln -sf "$1" "$2"
    else
        cp -f "$1" "$2"
    fi
}

link_or_copy "$SRC/server.py"               "$DST/mpm-mcp/server.py"
link_or_copy "$SRC/skills/mpm/SKILL.md"     "$DST/skills/mpm/SKILL.md"

# Render .mcp.json with the resolved mpm path (env block + absolute paths).
# Claude Code reads MCP servers from .mcp.json at the project root, NOT from
# .claude/mcp.json — putting it in .claude/ makes it invisible to the loader.
# Remove any stale .claude/mcp.json from older installs of this plugin.
rm -f "$DST/mcp.json"
sed "s|\${MPM_BINARY}|$MPM_PATH|g; s|\${PWD}|$SRC|g" "$SRC/.mcp.json" > "$SRC/../.mcp.json"

echo "✓ Installed ($ACTION). Restart Claude Code to pick up MCP server."
echo "  mpm binary: $MPM_PATH"
