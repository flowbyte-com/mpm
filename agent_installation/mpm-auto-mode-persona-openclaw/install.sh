#!/usr/bin/env bash
#
# install.sh — mpm-auto-mode-persona-openclaw adapter installer.
#
# Owns the complete OpenClaw-specific setup for this plugin:
#
#   1. Locate MPM via the canonical install paths
#      ($HOME/.mpm/bin/mpm first, then $HOME/.local/bin/mpm).
#      The plugin's runtime subprocess MUST use an absolute path
#      because the OpenClaw gateway runs under systemd --user with
#      a stripped PATH — a bare 'mpm' (the plugin's manifest
#      default) fails with `spawn mpm ENOENT`.
#   2. Install/link the plugin (only when required): --link --force
#      --accept-capabilities.
#   3. Persist absolute mpmBin to plugins.entries.<id>.config.mpmBin.
#   4. Run `openclaw update repair` to converge pending state
#      migrations (otherwise the plugin shows up as "state
#      migration pending" in `openclaw doctor` and the gateway
#      refuses to apply the install).
#   5. Bounded safe gateway restart (when a gateway service is
#      reachable).
#   6. Verify (openclaw plugins inspect + plugins list).
#
# This installer mirrors mpm-memory-openclaw/install.sh but is
# simpler because the auto-mode adapter has no hooks, no slot
# switch, and no capability schema beyond the standard `mpmBin`.
#
# Idempotency contract (OpenClaw 2026.9.5):
#   * Plugin absent → run `openclaw plugins install . --link --force
#     --accept-capabilities`.
#   * Plugin already linked from THIS adapter's absolute path →
#     skip the install step entirely (no destructive re-install).
#   * Plugin installed but pointing at a different source → fail
#     closed; we do not seize an unrelated installation.
#
# Does NOT modify shell startup files. Does NOT touch the MPM
# substrate install (that lives in the root install.sh; this
# adapter assumes the operator ran that first).
#
# CWD-independent: the plugin directory is resolved from
# BASH_SOURCE[0], so the installer works from any current working
# directory.
#
# Plugin identity is read from openclaw.plugin.json (id=
# "mpm-auto-mode-persona-openclaw") — do not rename.

set -euo pipefail

# --------------------------------------------------------------------------
# Identity + paths
# --------------------------------------------------------------------------

SCRIPT_PATH="${BASH_SOURCE[0]}"
while [ -L "$SCRIPT_PATH" ]; do
  LINK_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"
  SCRIPT_PATH="$(readlink "$SCRIPT_PATH")"
  case "$SCRIPT_PATH" in
    /*) ;;
    *)  SCRIPT_PATH="$LINK_DIR/$SCRIPT_PATH" ;;
  esac
done
SCRIPT_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"

PLUGIN_ID="$(grep -oE '"id"[[:space:]]*:[[:space:]]*"[^"]+"' "$SCRIPT_DIR/openclaw.plugin.json" \
  | head -n1 | sed -E 's/.*"id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
if [ -z "$PLUGIN_ID" ]; then
  printf '[mpm-auto-mode-persona-openclaw install] ERROR: could not parse plugin id from %s/openclaw.plugin.json\n' "$SCRIPT_DIR" >&2
  exit 2
fi

# MPM canonical locations. Discovery order is deterministic and
# never relies on a freshly-touched PATH entry. The OpenClaw gateway
# runs under systemd --user with a stripped PATH, so a PATH-resolved
# 'mpm' typically fails at runtime with `spawn mpm ENOENT`. We
# resolve an absolute path here and persist it to config.mpmBin.
HOME_DIR="${HOME:-}"
MPM_CANONICAL_PRIMARY="$HOME_DIR/.mpm/bin/mpm"
MPM_CANONICAL_SYMLINK="$HOME_DIR/.local/bin/mpm"

# OpenClaw CLI timeout budgets.
OPENCLAW_PLUGIN_INSTALL_TIMEOUT="${OPENCLAW_PLUGIN_INSTALL_TIMEOUT:-30}"
OPENCLAW_PLUGIN_INSPECT_TIMEOUT="${OPENCLAW_PLUGIN_INSPECT_TIMEOUT:-10}"
OPENCLAW_CONFIG_TIMEOUT="${OPENCLAW_CONFIG_TIMEOUT:-10}"
OPENCLAW_GATEWAY_STATUS_TIMEOUT="${OPENCLAW_GATEWAY_STATUS_TIMEOUT:-10}"
OPENCLAW_GATEWAY_RESTART_TIMEOUT="${OPENCLAW_GATEWAY_RESTART_TIMEOUT:-20}"
OPENCLAW_UPDATE_REPAIR_TIMEOUT="${OPENCLAW_UPDATE_REPAIR_TIMEOUT:-60}"
OPENCLAW_DOCTOR_FIX_TIMEOUT="${OPENCLAW_DOCTOR_FIX_TIMEOUT:-60}"

INSTALL_LOG="/tmp/mpm-auto-mode-persona-openclaw-install.log"
: > "$INSTALL_LOG" 2>/dev/null || true

log()  { printf '[mpm-auto-mode-persona-openclaw install] %s\n' "$*" >&2; }
warn() { printf '[mpm-auto-mode-persona-openclaw install] WARN: %s\n' "$*" >&2; }
err()  { printf '[mpm-auto-mode-persona-openclaw install] ERROR: %s\n' "$*" >&2; }

# --------------------------------------------------------------------------
# 1. Locate MPM
# --------------------------------------------------------------------------

locate_mpm() {
  if [ -x "$MPM_CANONICAL_PRIMARY" ]; then
    MPM_BIN="$MPM_CANONICAL_PRIMARY"
    log "mpm resolved via canonical install path: $MPM_BIN"
    return 0
  fi
  if [ -x "$MPM_CANONICAL_SYMLINK" ]; then
    if MPM_BIN="$(readlink -f "$MPM_CANONICAL_SYMLINK" 2>/dev/null)"; then
      [ -x "$MPM_BIN" ] || MPM_BIN="$MPM_CANONICAL_SYMLINK"
    else
      MPM_BIN="$MPM_CANONICAL_SYMLINK"
    fi
    log "mpm resolved via canonical user symlink: $MPM_BIN"
    return 0
  fi
  if MPM_BIN="$(command -v mpm 2>/dev/null)" && [ -n "$MPM_BIN" ] && [ -x "$MPM_BIN" ]; then
    log "mpm resolved via PATH: $MPM_BIN"
    return 0
  fi
  MPM_BIN=""
  return 1
}

if ! locate_mpm; then
  err "mpm not found on this host. Run install.sh from the repo root, then re-run this installer."
  err "  Canonical paths checked: $MPM_CANONICAL_PRIMARY, $MPM_CANONICAL_SYMLINK"
  exit 1
fi

if ! "$MPM_BIN" --version >/dev/null 2>&1; then
  err "mpm binary at $MPM_BIN is present but does not execute cleanly. Re-run root install.sh."
  exit 1
fi
log "mpm version: $("$MPM_BIN" --version 2>&1 | head -n1)"

# --------------------------------------------------------------------------
# 2. OpenClaw CLI presence
# --------------------------------------------------------------------------

if ! command -v openclaw >/dev/null 2>&1; then
  err "openclaw CLI not on PATH. Install OpenClaw first, then re-run this installer."
  err "  Plugin dir (already prepared for linking): $SCRIPT_DIR"
  exit 1
fi

# --------------------------------------------------------------------------
# 3. Inspect existing plugin state
# --------------------------------------------------------------------------
#
# Three states:
#   absent              — plugin id not in OpenClaw's registry.
#   linked-from-here    — plugin id registered AND rootDir equals
#                         this adapter's directory. Skip install step.
#   conflicting         — plugin id registered but rootDir points
#                         elsewhere. Fail closed (no seize).

# Helper: extract a JSON field from a possibly-prefixed openclaw
# output. Tolerates the config-warnings header lines that the CLI
# prints before JSON.
extract_json_field() {
  local field="$1"
  local input="$2"
  if [ -z "$input" ] || ! command -v python3 >/dev/null 2>&1; then
    return 0
  fi
  printf '%s\n' "$input" | FIELD="$field" python3 -c '
import json, sys
raw = sys.stdin.read()
lines = raw.split("\n")
start = None
for i, line in enumerate(lines):
    if line.startswith("{"):
        start = i
        break
if start is None:
    sys.exit(0)
try:
    data = json.loads("\n".join(lines[start:]))
except Exception:
    sys.exit(0)
import os
field = os.environ.get("FIELD", "")
node = data
for part in field.split("."):
    if isinstance(node, dict):
        node = node.get(part)
    elif isinstance(node, list):
        try:
            node = node[int(part)]
        except (ValueError, IndexError):
            sys.exit(0)
    else:
        sys.exit(0)
    if node is None:
        sys.exit(0)
if isinstance(node, (dict, list)):
    sys.exit(0)
print(node)
' 2>/dev/null
}

# Helper: compare two filesystem paths even when one doesn't
# exist. Resolves symlinks (and follows the canonical-form
# resolution used by the memory adapter's installer).
paths_match() {
  local a="$1" b="$2"
  if [ -z "$a" ] || [ -z "$b" ]; then
    return 1
  fi
  if [ "$a" = "$b" ]; then
    return 0
  fi
  local ra rb
  ra="$(readlink -f "$a" 2>/dev/null || printf '%s' "$a")"
  rb="$(readlink -f "$b" 2>/dev/null || printf '%s' "$b")"
  if [ "$ra" = "$rb" ]; then
    return 0
  fi
  return 1
}

PLUGIN_STATE="absent"
PLUGIN_EXISTING_ROOT=""
inspect_output="$(timeout "${OPENCLAW_PLUGIN_INSPECT_TIMEOUT}s" \
  openclaw plugins inspect "$PLUGIN_ID" --json 2>/dev/null || true)"
ok="$(extract_json_field "ok" "$inspect_output")"
parsed_root="$(extract_json_field "plugin.rootDir" "$inspect_output")"
if [ -n "$parsed_root" ] && [ "$ok" != "false" ] && [ "$ok" != "False" ]; then
  our_real="$(cd "$SCRIPT_DIR" && pwd -P 2>/dev/null || printf '%s' "$SCRIPT_DIR")"
  their_real="$(cd "$parsed_root" 2>/dev/null && pwd -P 2>/dev/null || printf '%s' "$parsed_root")"
  if [ "$their_real" = "$our_real" ]; then
    PLUGIN_STATE="linked-from-here"
    PLUGIN_EXISTING_ROOT="$our_real"
  else
    PLUGIN_STATE="conflicting"
    PLUGIN_EXISTING_ROOT="$their_real"
  fi
fi

# --------------------------------------------------------------------------
# 4. Install / link the plugin (state-driven)
# --------------------------------------------------------------------------

case "$PLUGIN_STATE" in
  linked-from-here)
    log "plugin '$PLUGIN_ID' already linked from $PLUGIN_EXISTING_ROOT; skipping install step"
    ;;
  conflicting)
    err "plugin '$PLUGIN_ID' is already installed but points at a different source."
    err "  registered rootDir: $PLUGIN_EXISTING_ROOT"
    err "  this adapter path:  $SCRIPT_DIR"
    err "this installer will not silently overwrite an unrelated plugin source."
    err "operator actions:"
    err "  (a) uninstall the conflicting install:"
    err "        openclaw plugins uninstall $PLUGIN_ID"
    err "        $0"
    err "  (b) leave it alone and rename this adapter's id in $SCRIPT_DIR/openclaw.plugin.json"
    exit 1
    ;;
  absent|*)
    log "installing plugin '$PLUGIN_ID' from $SCRIPT_DIR (--link --force --accept-capabilities)"
    # The OpenClaw CLI parses the trailing path argument strictly:
    # passing '.' here installs the plugin whose manifest lives in
    # the current directory. Passing "./<plugin-dir>" was tried but
    # 2026.9.5 concatenates that path onto $PWD and rejects the
    # resulting non-existent path. Using '.' is the documented
    # form that matches the auto-mode README.
    if ! timeout "${OPENCLAW_PLUGIN_INSTALL_TIMEOUT}s" \
        openclaw plugins install . --link --force --accept-capabilities \
          >>"$INSTALL_LOG" 2>&1; then
      err "fresh plugin install failed. Tail of $INSTALL_LOG:"
      tail -n 20 "$INSTALL_LOG" >&2 || true
      err "if the CLI printed 'state migration is pending', that is"
      err "expected on a fresh install; the post-install 'openclaw"
      err "update repair' step below is the documented convergence."
      exit 1
    fi
    ;;
esac

# --------------------------------------------------------------------------
# 5. Persist absolute mpmBin (PATH-gotcha mitigation)
# --------------------------------------------------------------------------
#
# The OpenClaw gateway runs as a systemd --user service with a
# stripped PATH. A bare 'mpm' (the plugin manifest's documented
# default) fails at runtime with `spawn mpm ENOENT`. We persist
# the absolute path that locate_mpm resolved above. Same gotcha
# as mpm-memory-openclaw (see its README for the longer writeup).

log "persisting plugins.entries.$PLUGIN_ID.config.mpmBin=$MPM_BIN"
if ! timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
    openclaw config set "plugins.entries.$PLUGIN_ID.config.mpmBin" "$MPM_BIN" \
      >>"$INSTALL_LOG" 2>&1; then
  warn "failed to persist mpmBin automatically. Operator run manually:"
  warn "  openclaw config set plugins.entries.$PLUGIN_ID.config.mpmBin $MPM_BIN"
fi

# --------------------------------------------------------------------------
# 6. Converge pending state migration
# --------------------------------------------------------------------------
#
# On a fresh install, OpenClaw 2026.9.5 leaves the install record in
# a "state migration pending" state until the parent releases it.
# Without convergence, `openclaw doctor` warns and the install is
# not fully visible to the gateway. `openclaw update repair` is the
# documented one-shot convergence (also surfaced by `doctor --fix`).
# We run update repair here as part of the install so a clean
# `doctor --lint` after install does not re-surface the warning.

log "converging pending state migration (openclaw update repair)"
if timeout "${OPENCLAW_UPDATE_REPAIR_TIMEOUT}s" \
    openclaw update repair \
      >>"$INSTALL_LOG" 2>&1; then
  log "  update repair ok"
else
  warn "update repair returned non-zero (operator can run:"
  warn "  openclaw update repair && openclaw doctor --fix"
  warn "see $INSTALL_LOG)"
fi

# --------------------------------------------------------------------------
# 7. Reconcile gateway state with install convergence
# --------------------------------------------------------------------------
#
# `openclaw update repair` deliberately stops the managed gateway as
# part of its convergence workflow (see the install log). After it
# returns, the gateway may be in one of three states:
#
#   (a) running (already restarted by update repair)
#   (b) stopped, awaiting systemd restart (the typical case when
#       update repair's completion-cache step didn't restart it)
#   (c) starting (mid-boot)
#
# We attempt a bounded safe restart only when the gateway is reachable
# AND the install changed something. If the gateway is down (case b)
# we leave it alone — the systemd --user unit is configured to
# auto-restart on failure, so the operator does NOT need to act.

status_timeout_ms=$(( OPENCLAW_GATEWAY_STATUS_TIMEOUT * 1000 ))
[ "$status_timeout_ms" -ge 1000 ] || status_timeout_ms=1000

if timeout "${OPENCLAW_GATEWAY_STATUS_TIMEOUT}s" \
   openclaw gateway status --json --timeout "$status_timeout_ms" \
     >/dev/null 2>&1; then
  log "requesting bounded safe gateway restart (timeout ${OPENCLAW_GATEWAY_RESTART_TIMEOUT}s)"
  if ! timeout "${OPENCLAW_GATEWAY_RESTART_TIMEOUT}s" \
      openclaw gateway restart --safe \
        >>"$INSTALL_LOG" 2>&1; then
    warn "gateway restart hit the bounded timeout or returned non-zero."
    warn "  config is already persisted; operator can run:"
    warn "    openclaw gateway restart --safe"
  fi
else
  # The gateway is not reachable. This is expected after
  # `update repair` because that command stops the gateway during
  # its own lifecycle. The systemd --user unit is configured to
  # auto-restart on failure, so we leave the restart to the unit
  # rather than racing against it.
  log "no gateway service detected (update repair may have stopped it; systemd --user will auto-restart)."
  log "  config is already persisted and will apply on the next gateway start."
fi

# --------------------------------------------------------------------------
# 8. Verify
# --------------------------------------------------------------------------

if timeout "${OPENCLAW_PLUGIN_INSPECT_TIMEOUT}s" \
   openclaw plugins inspect "$PLUGIN_ID" --json >/dev/null 2>&1; then
  log "plugin visible to openclaw: $PLUGIN_ID"
else
  warn "openclaw plugins inspect $PLUGIN_ID did not return cleanly; inspect $INSTALL_LOG"
fi

cat >&2 <<NEXT
[mpm-auto-mode-persona-openclaw install] Done.

Verification (optional):
  openclaw plugins list --json            # confirm enabled
  openclaw doctor --lint --json | grep -i migration   # confirm no pending migration

If anything above reported WARN, see $INSTALL_LOG
for the captured openclaw CLI output. Plugin config is persisted
even when gateway restart was skipped or bounded.
NEXT