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
          # Legacy plugin entry fields — used by the entries_only
          # fallback. Tests set FAKE_OPENCLAW_LEGACY_*_ENTRY_VALUE
          # to a non-empty string to simulate a non-empty legacy
          # entries subtree.
          plugins.entries.openclaw-mpm-memory.config.mpmBin)
            printf '%s\n' "${FAKE_OPENCLAW_LEGACY_MEMORY_ENTRY_VALUE:-}"
            ;;
          plugins.entries.openclaw-mpm-memory.enabled)
            # Mirror mpmBin: if either entry field has a value,
            # the entries fallback fires.
            if [ -n "${FAKE_OPENCLAW_LEGACY_MEMORY_ENTRY_VALUE:-}" ]; then
              printf 'true\n'
            else
              printf '\n'
            fi
            ;;
          plugins.entries.openclaw-mpm-auto-mode-persona.config.mpmBin)
            printf '%s\n' "${FAKE_OPENCLAW_LEGACY_AUTO_ENTRY_VALUE:-}"
            ;;
          plugins.entries.openclaw-mpm-auto-mode-persona.enabled)
            if [ -n "${FAKE_OPENCLAW_LEGACY_AUTO_ENTRY_VALUE:-}" ]; then
              printf 'true\n'
            else
              printf '\n'
            fi
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
            # Path used by inspect for the legacy plugin id. The
            # uninstaller matches this against the historical
            # legacy adapter source path. The default below is
            # $HOME-derived so the fake is host-agnostic — the
            # previous literal "/home/v/workspace/projects/mpm/..."
            # assumed the original author's checkout.
            path_value="${FAKE_OPENCLAW_LEGACY_MEMORY_PATH:-${HOME}/legacy-adapter/openclaw-mpm-memory}"
            ;;
          openclaw-mpm-auto-mode-persona)
            state="${FAKE_OPENCLAW_LEGACY_AUTO_STATE}"
            path_value="${FAKE_OPENCLAW_LEGACY_AUTO_PATH:-${HOME}/legacy-adapter/openclaw-mpm-auto-mode-persona}"
            ;;
          *)
            cat <<JSON
{ "ok": false, "error": { "type": "cli_error", "message": "Plugin not found: $plugin_id" } }
JSON
            exit 0
            ;;
        esac
        case "$state" in
          absent|vanished|slot_only|entries_only|ambiguous)
            # These states all return "absent" from inspect so the
            # uninstaller falls through to the registry / fallback
            # path. The registry fields and config keys are set
            # below to make each state's ownership signal surface
            # from the right source.
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
        # Build a registry JSON that reflects the legacy state
        # expectations. When state is "vanished" we return a
        # registry install record matching the legacy adapter
        # source path; when "linked_at_alt" with an inspect that's
        # already conflict we still let registry tell the same
        # story (registry_path_different) — but in our precedence
        # inspect wins, so the test never reaches registry in that
        # case. We also expose FAKE_OPENCLAW_LEGACY_REGISTRY_PATH so
        # tests can force a specific registry install record
        # independently of inspect.
        REGISTRY_OUT='{"ok":true,"state":"fresh","refreshReasons":[],"differences":[],"persisted":{"version":1,"installRecords":{}}}'
        # Build legacy install records if requested via env.
        # Tests set FAKE_OPENCLAW_LEGACY_*_REGISTRY_PATH to either
        # the legacy canonical path (vanished -> registry_path_match)
        # or an arbitrary path (registry_path_different if existing).
        if [ -n "${FAKE_OPENCLAW_LEGACY_MEMORY_REGISTRY_PATH+x}" ] && [ -n "${FAKE_OPENCLAW_LEGACY_MEMORY_REGISTRY_PATH}" ]; then
          REGISTRY_OUT=$(REGISTRY_OUT="$REGISTRY_OUT" \
            FAKE_OPENCLAW_LEGACY_MEMORY_REGISTRY_PATH="$FAKE_OPENCLAW_LEGACY_MEMORY_REGISTRY_PATH" \
            python3 -c '
import json, os, sys
raw = os.environ["REGISTRY_OUT"]
mp = os.environ["FAKE_OPENCLAW_LEGACY_MEMORY_REGISTRY_PATH"]
data = json.loads(raw)
data["persisted"]["installRecords"]["openclaw-mpm-memory"] = {
    "source": "path",
    "sourcePath": mp,
    "installPath": mp,
    "version": "0.1.3",
}
print(json.dumps(data))
')
        fi
        if [ -n "${FAKE_OPENCLAW_LEGACY_AUTO_REGISTRY_PATH+x}" ] && [ -n "${FAKE_OPENCLAW_LEGACY_AUTO_REGISTRY_PATH}" ]; then
          REGISTRY_OUT=$(REGISTRY_OUT="$REGISTRY_OUT" \
            FAKE_OPENCLAW_LEGACY_AUTO_REGISTRY_PATH="$FAKE_OPENCLAW_LEGACY_AUTO_REGISTRY_PATH" \
            python3 -c '
import json, os, sys
raw = os.environ["REGISTRY_OUT"]
ap = os.environ["FAKE_OPENCLAW_LEGACY_AUTO_REGISTRY_PATH"]
data = json.loads(raw)
data["persisted"]["installRecords"]["openclaw-mpm-auto-mode-persona"] = {
    "source": "path",
    "sourcePath": ap,
    "installPath": ap,
    "version": "0.1.0",
}
print(json.dumps(data))
')
        fi
        printf '%s\n' "$REGISTRY_OUT"
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