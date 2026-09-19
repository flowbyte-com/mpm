#!/usr/bin/env bash
# fake-openclaw.sh — fake openclaw binary used by
# tests/test_uninstall_openclaw.mjs. Records every invocation and
# replies to the subcommands the uninstaller uses. Behaviour is
# parameterised by env vars:
#   FAKE_OPENCLAW_CANONICAL_MEMORY_STATE
#   FAKE_OPENCLAW_CANONICAL_AUTO_STATE
#   FAKE_OPENCLAW_LEGACY_MEMORY_STATE
#   FAKE_OPENCLAW_LEGACY_AUTO_STATE
#     absent         — inspect returns ok:false (no plugin record).
#     linked         — inspect returns plugin.rootDir == adapter path.
#     linked_at_alt  — inspect returns plugin.rootDir == some other path.
#     unresolvable   — plugin field present, rootDir missing.
#   FAKE_OPENCLAW_MEMORY_SLOT
#     unset|empty    — slot is unset (config get returns "").
#     mpm-memory-openclaw / openclaw-mpm-memory / memory-core / <other>
#   FAKE_OPENCLAW_FAIL_UNINSTALL=1 — make uninstall return non-zero.
#   FAKE_OPENCLAW_HANG_UNINSTALL=1 — sleep 60s on uninstall.
# The fake uses FAKE_OPENCLAW_INV_FILE as the invocation log path
# (set by the test driver; default /tmp/fake-openclaw-inv.jsonl).

# DO NOT enable `set -u` here: the test driver sets all
# FAKE_OPENCLAW_* env vars explicitly, but we want a stable script
# even if some vars are missing.
set -eo pipefail

INV="${FAKE_OPENCLAW_INV_FILE:-/tmp/fake-openclaw-inv.jsonl}"
mkdir -p "$(dirname "$INV")"
touch "$INV"

UNINSTALLED_LIST="${FAKE_OPENCLAW_UNINSTALLED_LIST:-}"

# State defaults.
: "${FAKE_OPENCLAW_CANONICAL_MEMORY_STATE:=absent}"
: "${FAKE_OPENCLAW_CANONICAL_AUTO_STATE:=absent}"
: "${FAKE_OPENCLAW_LEGACY_MEMORY_STATE:=absent}"
: "${FAKE_OPENCLAW_LEGACY_AUTO_STATE:=absent}"
: "${FAKE_OPENCLAW_CANONICAL_MEMORY_PATH:=__BPE_ADAPTER_MEMORY__}"
: "${FAKE_OPENCLAW_CANONICAL_AUTO_PATH:=__BPE_ADAPTER_AUTO__}"
: "${FAKE_OPENCLAW_HANG_UNINSTALL:=0}"
: "${FAKE_OPENCLAW_FAIL_UNINSTALL:=0}"

is_uninstalled() {
  local id="$1"
  case ":${UNINSTALLED_LIST}:" in
    *":${id}:"*) return 0 ;;
    *) return 1 ;;
  esac
}

printf '{ "argv": %s, "pwd": "%s" }\n' \
  "$(printf '%s' "$*" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))' 2>/dev/null || printf 'null')" \
  "$PWD" >> "$INV"

cmd="$1"
shift || true

case "$cmd" in
  config)
    sub="$1"; shift || true
    case "$sub" in
      set)
        printf '  -> config set %s %s\n' "$1" "$2" >> "$INV"
        exit 0
        ;;
      get)
        printf '  -> config get %s\n' "$1" >> "$INV"
        case "$1" in
          plugins.slots.memory)
            printf '%s\n' "${FAKE_OPENCLAW_MEMORY_SLOT}"
            ;;
          *)
            printf '\n'
            ;;
        esac
        exit 0
        ;;
      unset)
        printf '  -> config unset %s\n' "$1" >> "$INV"
        exit 0
        ;;
      file)
        printf '/dev/null\n'
        exit 0
        ;;
    esac
    exit 0
    ;;
  plugins)
    sub="$1"; shift || true
    case "$sub" in
      inspect)
        plugin_id="$1"; shift || true
        printf '  -> plugins inspect %s\n' "$plugin_id" >> "$INV"
        if is_uninstalled "$plugin_id"; then
          cat <<JSON
{ "ok": false, "error": { "type": "cli_error", "message": "Plugin not found: $plugin_id" } }
JSON
          exit 0
        fi
        case "$plugin_id" in
          mpm-memory-openclaw)
            state="${FAKE_OPENCLAW_CANONICAL_MEMORY_STATE}"
            path_value="${FAKE_OPENCLAW_CANONICAL_MEMORY_PATH}"
            ;;
          mpm-auto-mode-persona-openclaw)
            state="${FAKE_OPENCLAW_CANONICAL_AUTO_STATE}"
            path_value="${FAKE_OPENCLAW_CANONICAL_AUTO_PATH}"
            ;;
          openclaw-mpm-memory)
            state="${FAKE_OPENCLAW_LEGACY_MEMORY_STATE}"
            path_value="/home/v/workspace/projects/mpm/agent_installation/openclaw-mpm-memory"
            ;;
          openclaw-mpm-auto-mode-persona)
            state="${FAKE_OPENCLAW_LEGACY_AUTO_STATE}"
            path_value="/home/v/workspace/projects/mpm/agent_installation/openclaw-mpm-auto-mode-persona"
            ;;
          *)
            cat <<JSON
{ "ok": false, "error": { "type": "cli_error", "message": "Plugin not found: $plugin_id" } }
JSON
            exit 0
            ;;
        esac
        case "$state" in
          absent)
            cat <<JSON
{ "ok": false, "error": { "type": "cli_error", "message": "Plugin not found: $plugin_id" } }
JSON
            ;;
          linked|linked_at_alt)
            cat <<JSON
{
  "ok": true,
  "plugin": {
    "id": "$plugin_id",
    "name": "$plugin_id (fake)",
    "version": "0.0.0-fake",
    "format": "openclaw",
    "source": "$path_value/index.js",
    "rootDir": "$path_value",
    "status": "loaded"
  }
}
JSON
            ;;
          unresolvable)
            cat <<JSON
{
  "ok": true,
  "plugin": {
    "id": "$plugin_id",
    "name": "$plugin_id (fake unresolvable)",
    "version": "0.0.0-fake",
    "format": "openclaw",
    "source": "<unknown>",
    "status": "loaded"
  }
}
JSON
            ;;
        esac
        exit 0
        ;;
      uninstall)
        printf '  -> plugins uninstall %s\n' "$*" >> "$INV"
        if [ "${FAKE_OPENCLAW_HANG_UNINSTALL}" = "1" ]; then
          sleep 60
        fi
        if [ "${FAKE_OPENCLAW_FAIL_UNINSTALL}" = "1" ]; then
          exit 7
        fi
        NEW_LIST="${UNINSTALLED_LIST}"
        for id in "$@"; do
          if [ -z "${NEW_LIST}" ]; then
            NEW_LIST="${id}"
          elif ! is_uninstalled "${id}"; then
            NEW_LIST="${NEW_LIST}:${id}"
          fi
        done
        UNINSTALLED_LIST="${NEW_LIST}"
        export FAKE_OPENCLAW_UNINSTALLED_LIST="${UNINSTALLED_LIST}"
        exit 0
        ;;
      list)
        printf '  -> plugins list\n' >> "$INV"
        echo '[]'
        exit 0
        ;;
      registry)
        printf '  -> plugins registry\n' >> "$INV"
        echo '{}'
        exit 0
        ;;
    esac
    exit 0
    ;;
  gateway)
    sub="$1"; shift || true
    case "$sub" in
      status)
        printf '  -> gateway status %s\n' "$*" >> "$INV"
        exit 0
        ;;
      restart)
        printf '  -> gateway restart %s\n' "$*" >> "$INV"
        exit 0
        ;;
    esac
    exit 0
    ;;
  --version)
    printf 'OpenClaw 2026.9.5-test\n'
    exit 0
    ;;
esac
exit 0