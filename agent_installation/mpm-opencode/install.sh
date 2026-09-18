#!/usr/bin/env bash
#
# install.sh — Materialize the OpenCode ↔ MPM integration.
#
# Idempotent. Performs three orthogonal responsibilities:
#
#   1. Compile dist/ from canonical source (if dist/index.js missing
#      or older than src/index.ts).
#   2. Refresh the OpenCode plugin config entry in
#      ~/.config/opencode/opencode.jsonc. Detects and migrates
#      STALE entries that reference the pre-2026-09-17 legacy
#      'opencode-mpm' directory. New entries use the canonical
#      'mpm-opencode' path.
#   3. Refresh the AGENTS.md behavioral managed block (delegates to
#      install_agents_instructions.py for the actual content work).
#
# Migration behavior (Part 3 — namespace refresh):
#
#   Before 2026-09-17: opencode.jsonc pointed at
#       file:///.../.mpm/agent_installation/opencode-mpm/dist/index.js
#   After this script: opencode.jsonc points at
#       file:///.../.mpm/agent_installation/mpm-opencode/dist/index.js
#
#   The legacy directory at ~/.mpm/agent_installation/opencode-mpm/
#   is REPO-OWNED state (left behind by the namespace migration
#   `git mv`). If it still exists, we remove ONLY the generated
#   dist/ subdirectory (which is .gitignored and never tracked).
#   Any user-installed source code in the old directory is left
#   alone.
#
#   The script does NOT:
#     - delete ~/.config/opencode/opencode.jsonc
#     - rewrite unrelated plugin entries
#     - delete files outside the legacy MPM-owned dist/
#
# Usage:
#   ./install.sh             # install / refresh
#   ./install.sh --uninstall # remove plugin entry + AGENTS.md block
#   ./install.sh --verify    # run validation tests
#
# CWD-independent: the plugin directory is resolved from BASH_SOURCE[0],
# so the installer works from any current working directory.

set -euo pipefail

# --------------------------------------------------------------------------
# Identity + paths
# --------------------------------------------------------------------------

# Resolve our own directory without depending on caller CWD.
SCRIPT_PATH="${BASH_SOURCE[0]}"
# Resolve symlinks so ./install.sh that points elsewhere still works.
while [ -L "$SCRIPT_PATH" ]; do
    LINK_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"
    SCRIPT_PATH="$(readlink "$SCRIPT_PATH")"
    case "$SCRIPT_PATH" in
        /*) ;;
        *)  SCRIPT_PATH="$LINK_DIR/$SCRIPT_PATH" ;;
    esac
done
SCRIPT_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"

# OpenCode adapter identity comes from the exported "id" field in
# src/index.ts. We can also read the package name from package.json as a
# stable surrogate; the install does not need to compile TS.
PLUGIN_ID="$(grep -oE '"name"[[:space:]]*:[[:space:]]*"[^"]+"' "$SCRIPT_DIR/package.json" \
    | head -n1 | sed -E 's/.*"name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
if [ -z "$PLUGIN_ID" ]; then
    printf '[mpm-opencode install] ERROR: could not parse plugin name from %s/package.json\n' "$SCRIPT_DIR" >&2
    exit 2
fi

# Canonical MPM install — same resolution rule as mpm-opencode/src/index.ts
# and mpm-memory-openclaw/install.sh. Order matters: the substrate install
# (install.sh) writes ~/.mpm/bin/mpm and symlinks it into
# ~/.local/bin/mpm. We accept either. We never rely on PATH resolution
# alone — on a freshly-created ~/.local/bin the current login session
# may not have it on PATH yet (proved in clean-profile Linux Mint test).
HOME_DIR="${HOME:?HOME must be set}"
OPENCODE_CONFIG_DIR="${HOME_DIR}/.config/opencode"
OPENCODE_CONFIG="${OPENCODE_CONFIG_DIR}/opencode.jsonc"
OPENCODE_PLUGIN_DIR="${OPENCODE_CONFIG_DIR}/plugin"
PLUGIN_INSTALL_DIR="${HOME_DIR}/.mpm/agent_installation"

# Canonical adapter install locations (new and legacy).
CANONICAL_ADAPTER_DIR="${PLUGIN_INSTALL_DIR}/mpm-opencode"
LEGACY_ADAPTER_DIR="${PLUGIN_INSTALL_DIR}/opencode-mpm"
# Canonical dist path written to opencode.jsonc plugin[].
CANONICAL_PLUGIN_ENTRY="file://${CANONICAL_ADAPTER_DIR}/dist/index.js"

INSTALL_LOG="/tmp/mpm-opencode-install.log"
: > "$INSTALL_LOG" 2>/dev/null || true

log()  { printf '[mpm-opencode install] %s\n' "$*" >&2; }
warn() { printf '[mpm-opencode install] WARN: %s\n' "$*" >&2; }
err()  { printf '[mpm-opencode install] ERROR: %s\n' "$*" >&2; }
die()  { err "$1"; exit "${2:-1}"; }

# --------------------------------------------------------------------------
# Argument parsing
# --------------------------------------------------------------------------

MODE="install"
for arg in "$@"; do
    case "$arg" in
        --uninstall) MODE="uninstall" ;;
        --verify)    MODE="verify" ;;
        --help|-h)
            sed -n '2,42p' "$0"
            exit 0
            ;;
        *)  err "unknown option: $arg"; exit 1 ;;
    esac
done

# --------------------------------------------------------------------------
# Step 1: compile dist/ if missing or stale
# --------------------------------------------------------------------------
#
# `npx tsc` is the canonical build. We rebuild only when the existing
# dist/index.js is older than src/index.ts — incremental build keeps
# this fast on repeat invocations.

build_dist() {
    if [ ! -f "$SCRIPT_DIR/dist/index.js" ] \
        || [ -f "$SCRIPT_DIR/src/index.ts" -a "$SCRIPT_DIR/src/index.ts" -nt "$SCRIPT_DIR/dist/index.js" ]; then
        log "rebuilding dist/ from source..."
        if ! (cd "$SCRIPT_DIR" && npx tsc 2>>"$INSTALL_LOG") >>"$INSTALL_LOG" 2>&1; then
            die "npx tsc failed; see $INSTALL_LOG" 2
        fi
        log "dist/index.js rebuilt"
    else
        log "dist/index.js is up to date"
    fi
}

# --------------------------------------------------------------------------
# Step 2: detect and migrate OpenCode plugin config
# --------------------------------------------------------------------------
#
# The namespace migration `git mv opencode-mpm → mpm-opencode` moved
# tracked source. It did NOT touch ~/.config/opencode/opencode.jsonc —
# that file is user-owned and the migration is silent. Real installs
# carry a stale `file:///.../opencode-mpm/dist/index.js` entry that
# breaks the plugin loader.
#
# This step is the repo-owned namespace-refresh: detect the stale path
# in opencode.jsonc, update it to the canonical path. We do NOT
# delete unrelated plugins; we do NOT touch other config sections.

migrate_opencode_config() {
    mkdir -p "$OPENCODE_PLUGIN_DIR" 2>/dev/null || true
    mkdir -p "$OPENCODE_CONFIG_DIR" 2>/dev/null || true

    if [ ! -f "$OPENCODE_CONFIG" ]; then
        # No config yet — create one with just the plugin entry.
        cat > "$OPENCODE_CONFIG" <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "plugin": [
    "${CANONICAL_PLUGIN_ENTRY}"
  ]
}
EOF
        log "created ${OPENCODE_CONFIG} with canonical plugin entry"
        return 0
    fi

    # Detect legacy 'opencode-mpm' references and migrate them.
    # We use Python for safe JSON editing — shell string replacement
    # would risk corrupting other plugin entries.
    if command -v python3 >/dev/null 2>&1; then
        if python3 - "$OPENCODE_CONFIG" "$CANONICAL_PLUGIN_ENTRY" <<'PYEOF'
import json, os, sys
config_path = os.path.realpath(sys.argv[1])
canonical = sys.argv[2]
with open(config_path, 'r', encoding='utf-8') as f:
    text = f.read()
try:
    data = json.loads(text)
except Exception:
    sys.exit(0)  # malformed config — leave it alone, OpenCode will error
plugins = data.get('plugin', [])
if not isinstance(plugins, list):
    plugins = [plugins]
migrated = False
new_plugins = []
for entry in plugins:
    # Recognise the legacy path by directory component, not by raw
    # substring (the sandbox test path /tmp/mpm-opencode-debug-XYZ/...
    # happens to contain 'mpm-opencode' as a prefix and must NOT be
    # misread as a stale entry).
    if (
        isinstance(entry, str)
        and (
            '/opencode-mpm/' in entry
            or entry.endswith('/opencode-mpm')
            or entry.endswith('opencode-mpm/dist/index.js')
        )
    ):
        new_plugins.append(canonical)
        migrated = True
    else:
        new_plugins.append(entry)
# If the canonical entry is not already present, add it.
if canonical not in new_plugins:
    # Place canonical first; preserve any other entries.
    new_plugins = [canonical] + [p for p in new_plugins if p != canonical]
    migrated = True
data['plugin'] = new_plugins
with open(config_path, 'w', encoding='utf-8') as f:
    json.dump(data, f, indent=2)
    f.write('\n')
if migrated:
    print(f"migrated stale opencode-mpm entries to {canonical}", file=sys.stderr)
PYEOF
        then
            log "opencode.jsonc updated (migrated stale entries)"
        fi
    else
        warn "python3 not available — opencode.jsonc migration skipped"
        warn "  manually replace 'opencode-mpm' with 'mpm-opencode' in ${OPENCODE_CONFIG}"
    fi
}

# --------------------------------------------------------------------------
# Step 3: refresh the AGENTS.md behavioral managed block
# --------------------------------------------------------------------------
#
# Delegates to the existing Python installer. We pass the canonical
# target + snippet paths so the managed block always reflects current
# source. The Python installer is idempotent.

refresh_agents_md() {
    local installer="$SCRIPT_DIR/scripts/install_agents_instructions.py"
    local snippet="$SCRIPT_DIR/templates/AGENTS.md.snippet"
    local target="$OPENCODE_CONFIG_DIR/AGENTS.md"
    if [ ! -f "$installer" ]; then
        warn "AGENTS.md installer missing at $installer; skipping managed-block refresh"
        return 0
    fi
    if [ ! -f "$snippet" ]; then
        warn "AGENTS.md snippet missing at $snippet; skipping managed-block refresh"
        return 0
    fi
    if ! python3 "$installer" \
            --scope user \
            --target "$target" \
            --snippet "$snippet" \
            >>"$INSTALL_LOG" 2>&1; then
        warn "AGENTS.md installer failed (see $INSTALL_LOG); continuing"
    else
        log "AGENTS.md managed block refreshed at $target"
    fi
}

# --------------------------------------------------------------------------
# Step 4: clean up the legacy generated directory (when safe)
# --------------------------------------------------------------------------
#
# After the namespace migration, the legacy directory
# ~/.mpm/agent_installation/opencode-mpm/ may still exist with a
# generated dist/ subdirectory (which was .gitignored and never
# tracked). We remove ONLY the generated dist/, not user code.
#
# This step is best-effort. If anything goes wrong we warn and proceed.

cleanup_legacy_generated() {
    if [ ! -d "$LEGACY_ADAPTER_DIR" ]; then
        return 0
    fi
    # dist/ is the only generated subdirectory in the adapter layout.
    # node_modules/ may also be generated but is sometimes hand-installed
    # for development; leave it.
    if [ -d "$LEGACY_ADAPTER_DIR/dist" ]; then
        if rm -rf "$LEGACY_ADAPTER_DIR/dist" 2>/dev/null; then
            log "removed generated dist/ under $LEGACY_ADAPTER_DIR"
        else
            warn "could not remove $LEGACY_ADAPTER_DIR/dist (operator cleanup)"
        fi
    fi
}

# --------------------------------------------------------------------------
# Uninstall
# --------------------------------------------------------------------------

do_uninstall() {
    log "uninstalling mpm-opencode..."
    # Remove the AGENTS.md managed block.
    local installer="$SCRIPT_DIR/scripts/install_agents_instructions.py"
    if [ -f "$installer" ]; then
        local target="$OPENCODE_CONFIG_DIR/AGENTS.md"
        python3 "$installer" --scope user --target "$target" \
            --uninstall >>"$INSTALL_LOG" 2>&1 \
            && log "removed managed block from $target" \
            || warn "AGENTS.md uninstall failed (see $INSTALL_LOG)"
    fi
    # Remove the plugin entry from opencode.jsonc (only our canonical one).
    if [ -f "$OPENCODE_CONFIG" ] && command -v python3 >/dev/null 2>&1; then
        python3 - "$OPENCODE_CONFIG" "$CANONICAL_PLUGIN_ENTRY" <<'PYEOF'
import json, os, sys
config_path = os.path.realpath(sys.argv[1])
canonical = sys.argv[2]
with open(config_path, 'r', encoding='utf-8') as f:
    text = f.read()
try:
    data = json.loads(text)
except Exception:
    sys.exit(0)
plugins = data.get('plugin', [])
if not isinstance(plugins, list):
    plugins = [plugins]
data['plugin'] = [p for p in plugins if p != canonical]
with open(config_path, 'w', encoding='utf-8') as f:
    json.dump(data, f, indent=2)
    f.write('\n')
PYEOF
        log "removed canonical plugin entry from $OPENCODE_CONFIG"
    fi
}

# --------------------------------------------------------------------------
# Main dispatch
# --------------------------------------------------------------------------

case "$MODE" in
    install)
        log "installing mpm-opencode plugin (id=$PLUGIN_ID) from $SCRIPT_DIR"
        build_dist
        migrate_opencode_config
        refresh_agents_md
        cleanup_legacy_generated
        log "done"
        log "verification:"
        log "  opencode.jsonc plugin[] points at $CANONICAL_PLUGIN_ENTRY"
        log "  AGENTS.md managed block is current"
        if [ -d "$LEGACY_ADAPTER_DIR" ]; then
            log "  legacy directory remains at $LEGACY_ADAPTER_DIR"
            log "  operator can remove manually: rm -rf '$LEGACY_ADAPTER_DIR'"
        fi
        ;;
    uninstall)
        do_uninstall
        log "done"
        ;;
    verify)
        # Verify the canonical install state.
        ok=1
        if [ ! -f "$SCRIPT_DIR/dist/index.js" ]; then
            err "verify: dist/index.js missing"
            ok=0
        fi
        if [ ! -f "$OPENCODE_CONFIG" ]; then
            err "verify: $OPENCODE_CONFIG missing"
            ok=0
        fi
        if [ -f "$OPENCODE_CONFIG" ] && grep -q 'opencode-mpm' "$OPENCODE_CONFIG"; then
            err "verify: $OPENCODE_CONFIG still references legacy 'opencode-mpm'"
            ok=0
        fi
        [ "$ok" = "1" ] || exit 1
        log "verify: ok"
        ;;
    *)
        err "unknown mode: $MODE"
        exit 1
        ;;
esac
