#!/usr/bin/env bash
#
# install.sh — Materialize the Pi ↔ MPM integration.
#
# Idempotent. Performs two orthogonal responsibilities:
#
#   1. Refresh the Pi extension entry in ~/.pi/agent/settings.json.
#      Detects and migrates STALE entries that reference the
#      pre-2026-09-17 legacy 'agent_plugins/pi-mpm' path (or the
#      pre-2026-09-17 'pi-mpm' directory name). New entries use the
#      canonical '~/.mpm/agent_installation/mpm-pi' path.
#
#   2. Refresh the AGENTS.md behavioral managed block (delegates to
#      install_agents_instructions.py for the actual content work).
#
# Migration behavior (Part 1 — namespace refresh):
#
#   Before 2026-09-17: ~/.pi/agent/settings.json pointed at
#       ~/.mpm/agent_plugins/pi-mpm   (no longer exists; was renamed)
#       ~/.mpm/agent_installation/pi-mpm  (older alternate name; renamed)
#   After this script: ~/.pi/agent/settings.json points at
#       ~/.mpm/agent_installation/mpm-pi  (canonical, post-2026-09-17)
#
#   The script does NOT:
#     - delete ~/.pi/agent/settings.json
#     - rewrite unrelated Pi settings (theme, defaultProvider, etc.)
#     - mutate Pi settings the user has not opted into MPM integration
#       for (the installer only writes when the user runs it)
#
# Usage:
#   ./install.sh             # install / refresh
#   ./install.sh --uninstall # remove extension entry + AGENTS.md block
#   ./install.sh --verify    # run validation tests
#
# CWD-independent: the installer is resolved from BASH_SOURCE[0], so it
# works from any current working directory. The script edits ONLY
# ~/.pi/agent/settings.json under $HOME; it never touches $XDG_CONFIG_HOME
# or any other host-state location.

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

# Pi integration identity — the canonical source directory under the
# MPM install root. The adapter itself has no separate plugin manifest;
# Pi's extension contract is "point me at a directory containing an
# index.ts that exports piMpmExtension". The canonical directory name
# post-2026-09-17 is `mpm-pi` (under `agent_installation/`).
HOME_DIR="${HOME:?HOME must be set}"
MPM_HOME_DIR="${MPM_WORKSPACE:-${HOME_DIR}/.mpm}"
ADAPTER_DIR="${MPM_HOME_DIR}/agent_installation/mpm-pi"
ADAPTER_INDEX="${ADAPTER_DIR}/index.ts"

# Legacy paths the installer detects and migrates away from.
# - "agent_plugins/" was the pre-2026-09-17 prefix convention.
# - "pi-mpm" was the pre-2026-09-17 directory name.
LEGACY_AGENT_PLUGINS_PATTERN="agent_plugins/pi-mpm"
LEGACY_DIR_NAME_PATTERN="/pi-mpm"

# Pi settings file (the only file this installer mutates).
PI_SETTINGS_DIR="${HOME_DIR}/.pi/agent"
PI_SETTINGS="${PI_SETTINGS_DIR}/settings.json"

# AGENTS.md behavioral managed block installer + snippet.
INSTRUCTIONS_INSTALLER="${SCRIPT_DIR}/scripts/install_agents_instructions.py"
INSTRUCTIONS_SNIPPET="${SCRIPT_DIR}/templates/AGENTS.md.snippet"

log()  { printf '[mpm-pi install] %s\n' "$*" >&2; }
warn() { printf '[mpm-pi install] WARN: %s\n' "$*" >&2; }
err()  { printf '[mpm-pi install] ERROR: %s\n' "$*" >&2; }

# --------------------------------------------------------------------------
# 1. Verify the canonical adapter payload exists
# --------------------------------------------------------------------------
#
# Fail clearly if the expected integration payload is unavailable rather
# than writing a dead path into Pi settings.

if [ ! -f "$ADAPTER_INDEX" ]; then
    err "canonical Pi adapter index not found at $ADAPTER_INDEX"
    err "  MPM_WORKSPACE=${MPM_HOME_DIR} does not contain agent_installation/mpm-pi/"
    err "  Run the canonical MPM substrate installer first:"
    err "    cd ~/.mpm && ./install.sh"
    err "  Or set MPM_WORKSPACE to the directory that holds the MPM checkout."
    exit 2
fi

# --------------------------------------------------------------------------
# 2. Refresh ~/.pi/agent/settings.json extension entry
# --------------------------------------------------------------------------
#
# Behavior:
#   - File absent: create a minimal settings.json containing the
#     canonical extension entry. Preserves no prior user data because
#     there is none.
#   - File present, parseable: load JSON, locate `extensions` (default
#     empty array), drop any entries that look like a legacy MPM Pi
#     path, ensure the canonical entry is present, write back. All
#     other top-level keys are preserved verbatim.
#   - File present, malformed JSON: refuse rather than corrupt it.
#     Back up the malformed copy and abort with a clear operator
#     action.
#
# Idempotency: a second run with the canonical entry already present
# detects no legacy entries, leaves the file byte-equivalent, and
# exits cleanly.

refresh_pi_settings() {
    mkdir -p "$PI_SETTINGS_DIR" 2>/dev/null || {
        err "could not create $PI_SETTINGS_DIR"
        exit 1
    }

    if [ ! -f "$PI_SETTINGS" ]; then
        # Fresh install: write a minimal settings.json. This is the only
        # case where the installer creates a top-level user config from
        # scratch; it is intentional because the canonical reference
        # contract (agent_installation/README.md line 161) calls for the
        # extension entry to live here.
        cat > "$PI_SETTINGS" <<EOF
{
  "extensions": [
    "${ADAPTER_DIR}"
  ]
}
EOF
        log "created ${PI_SETTINGS} with canonical extension entry"
        return 0
    fi

    if ! command -v python3 >/dev/null 2>&1; then
        err "python3 not available — cannot safely edit ${PI_SETTINGS}"
        err "  manually replace any legacy MPM Pi path with:"
        err "    ${ADAPTER_DIR}"
        exit 1
    fi

    # Back up the existing settings.json before any mutation. The
    # project's installer convention (per mpm-opencode/install.sh and
    # mpm-claude-code/install.sh) is to snapshot user state before
    # editing. Backups live next to the file so they are easy to find.
    BACKUP="${PI_SETTINGS}.mpm-install.bak.$(date +%Y%m%d%H%M%S)"
    cp -p "$PI_SETTINGS" "$BACKUP"
    log "backed up existing settings to $BACKUP"

    python3 - "$PI_SETTINGS" "$ADAPTER_DIR" <<'PYEOF'
import json, os, sys

settings_path = sys.argv[1]
canonical = sys.argv[2]

with open(settings_path, "r", encoding="utf-8") as f:
    text = f.read()

# Parse to validate the file. If it fails, refuse rather than corrupt it.
try:
    data = json.loads(text)
except Exception as e:
    print(f"ERROR: {settings_path} is not valid JSON ({e}); refusing to edit", file=sys.stderr)
    print("The installer backed up the malformed file alongside settings.json.", file=sys.stderr)
    sys.exit(2)

# Preserve all top-level keys verbatim. Only the `extensions` array
# is touched.
extensions = data.get("extensions", [])
if not isinstance(extensions, list):
    extensions = [extensions]

# Legacy detection — see installer header for the historical context.
# Both forms (`agent_plugins/pi-mpm` and the older `pi-mpm` directory
# name under `agent_installation/`) are recognized as the same
# pre-2026-09-17 namespace and migrate to the canonical `mpm-pi`.
def is_legacy_mpm_pi(entry):
    if not isinstance(entry, str):
        return False
    return (
        "agent_plugins/pi-mpm" in entry
        or entry.endswith("/agent_plugins/pi-mpm")
        or "/agent_installation/pi-mpm" in entry
        or entry.endswith("/agent_installation/pi-mpm")
    )

migrated = False
new_extensions = []
for entry in extensions:
    if is_legacy_mpm_pi(entry):
        new_extensions.append(canonical)
        migrated = True
    else:
        new_extensions.append(entry)

# Ensure canonical entry is present exactly once.
if canonical not in new_extensions:
    new_extensions = [canonical] + [e for e in new_extensions if e != canonical]
    migrated = True
elif new_extensions.count(canonical) > 1:
    # Deduplicate canonical entries — defensive against historical
    # duplicates if a prior install was double-run.
    seen = set()
    deduped = []
    for e in new_extensions:
        if e == canonical:
            if canonical in seen:
                migrated = True
                continue
            seen.add(canonical)
        deduped.append(e)
    new_extensions = deduped

data["extensions"] = new_extensions

# Preserve formatting style: 2-space indent, trailing newline, no
# trailing whitespace. The Pi JSON loader accepts either with-or-
# without trailing newline; trailing newline is conventional.
with open(settings_path, "w", encoding="utf-8") as f:
    json.dump(data, f, indent=2, sort_keys=False, ensure_ascii=False)
    f.write("\n")

if migrated:
    print(f"migrated stale MPM-Pi extensions entry to {canonical}", file=sys.stderr)
PYEOF
    log "settings.json updated (legacy entries replaced, canonical entry ensured)"
}

# --------------------------------------------------------------------------
# 3. Refresh the AGENTS.md behavioral managed block
# --------------------------------------------------------------------------
#
# Same delegate that the OpenCode adapter uses. Idempotent marker-
# section overwrite; backup-before-mutate; corruption abort.

refresh_agents_md() {
    if [ ! -f "$INSTRUCTIONS_INSTALLER" ] || [ ! -f "$INSTRUCTIONS_SNIPPET" ]; then
        warn "expected $INSTRUCTIONS_INSTALLER and $INSTRUCTIONS_SNIPPET — skipping AGENTS.md"
        return 0
    fi
    if ! command -v python3 >/dev/null 2>&1; then
        warn "python3 not available — skipping AGENTS.md refresh"
        return 0
    fi
    python3 "$INSTRUCTIONS_INSTALLER" \
        --snippet "$INSTRUCTIONS_SNIPPET" \
        || warn "AGENTS.md refresh returned non-zero (operator may need to inspect)"
}

# --------------------------------------------------------------------------
# Mode dispatch
# --------------------------------------------------------------------------

case "${1:-}" in
    --uninstall)
        # Uninstall: remove the canonical extension entry; the
        # AGENTS.md installer owns its own uninstall path.
        if [ -f "$PI_SETTINGS" ]; then
            BACKUP="${PI_SETTINGS}.mpm-uninstall.bak.$(date +%Y%m%d%H%M%S)"
            cp -p "$PI_SETTINGS" "$BACKUP"
            log "backed up settings to $BACKUP"
            python3 - "$PI_SETTINGS" "$ADAPTER_DIR" <<'PYEOF'
import json, os, sys
settings_path = sys.argv[1]
canonical = sys.argv[2]
with open(settings_path, "r", encoding="utf-8") as f:
    data = json.load(f)
extensions = data.get("extensions", [])
if isinstance(extensions, list):
    new_extensions = [e for e in extensions if e != canonical]
    if new_extensions:
        data["extensions"] = new_extensions
    else:
        # No MPM-related extensions remain — drop the key entirely
        # to leave the file closer to its pre-install shape.
        data.pop("extensions", None)
with open(settings_path, "w", encoding="utf-8") as f:
    if data:
        json.dump(data, f, indent=2, ensure_ascii=False)
        f.write("\n")
    else:
        # File became empty — leave a {} placeholder so Pi keeps
        # finding a valid JSON object at the canonical location.
        f.write("{}\n")
PYEOF
            log "removed canonical MPM-Pi entry from settings.json"
        fi
        if [ -f "$INSTRUCTIONS_INSTALLER" ] && command -v python3 >/dev/null 2>&1; then
            python3 "$INSTRUCTIONS_INSTALLER" --uninstall \
                || warn "AGENTS.md uninstall returned non-zero"
        fi
        exit 0
        ;;
    --verify)
        # Verify: print the current extension entry state and exit.
        if [ ! -f "$PI_SETTINGS" ]; then
            err "settings.json does not exist"
            exit 1
        fi
        python3 - "$PI_SETTINGS" "$ADAPTER_DIR" <<'PYEOF'
import json, sys
settings_path = sys.argv[1]
canonical = sys.argv[2]
with open(settings_path, "r", encoding="utf-8") as f:
    data = json.load(f)
extensions = data.get("extensions", [])
legacy = [e for e in extensions if isinstance(e, str) and (
    "agent_plugins/pi-mpm" in e
    or "/agent_installation/pi-mpm" in e
)]
print(f"settings.json: {settings_path}")
print(f"  extensions: {extensions}")
print(f"  legacy entries: {legacy or 'none'}")
print(f"  canonical present: {canonical in extensions}")
sys.exit(1 if legacy or canonical not in extensions else 0)
PYEOF
        ;;
    "")
        refresh_pi_settings
        refresh_agents_md
        log "Pi integration installed. Restart Pi to load the extension."
        ;;
    *)
        err "unknown argument: $1"
        err "  usage: $0 [--uninstall | --verify]"
        exit 3
        ;;
esac