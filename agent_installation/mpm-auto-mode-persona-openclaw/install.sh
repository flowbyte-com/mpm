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
#   * Plugin absent → stage the runtime package, then run
#     `openclaw plugins install <runtime-package> --link --force
#     --accept-capabilities`.
#   * Plugin already linked from THIS adapter's runtime package →
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

# --------------------------------------------------------------------------
# Source tree vs runtime package
# --------------------------------------------------------------------------
#
# SCRIPT_DIR is this adapter's location in the *source checkout*. It is not
# what OpenClaw ends up linked to. Under the source/runtime split the
# runtime root is populated by explicit provisioning, so this installer
# stages a minimal, validated package into
#
#     <runtime-root>/agent_installation/<plugin-id>
#
# and links THAT. This also removes a latent CWD dependency: the previous
# `openclaw plugins install . --link` linked whatever directory the
# operator happened to be standing in, which is only the adapter by
# coincidence. Linking the resolved runtime package makes the install
# CWD-independent by construction.
#
# The runtime root is derived from the mpm binary resolved in step 1, so a
# relocated or hermetic install provisions into its own tree.
#
# The manifest is the allowlist for what ships. This plugin contributes
# no instruction-reconcile assets, so its runtime surface is just its
# entrypoint, its metadata, and the one module that entrypoint imports.

# `|| true` is load-bearing: under `set -e` a failing command substitution
# in an assignment aborts the script *before* the guard below can report
# what went wrong, so a missing sibling directory would exit silently.
MPM_STAGE_PARENT="$(cd "$(dirname "$SCRIPT_DIR")/scripts" 2>/dev/null && pwd -P || true)"
MPM_STAGE_LIB="${MPM_STAGE_PARENT:+$MPM_STAGE_PARENT/}stage_runtime_package.sh"
if [ ! -f "$MPM_STAGE_LIB" ]; then
  printf '[mpm-auto-mode-persona-openclaw install] ERROR: staging library not found: %s\n' "$MPM_STAGE_LIB" >&2
  printf '[mpm-auto-mode-persona-openclaw install] the shared provisioning library lives at agent_installation/scripts/\n' >&2
  printf '[mpm-auto-mode-persona-openclaw install] next to this adapter; run this installer from a complete checkout.\n' >&2
  exit 2
fi
# shellcheck source=../scripts/stage_runtime_package.sh
. "$MPM_STAGE_LIB"

RUNTIME_PACKAGE_MANIFEST=(
  index.js
  openclaw.plugin.json
  package.json
  README.md
  lib/workspace.js
)

PLUGIN_PKG_DIR=""

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
# 1b. Provision the runtime package
# --------------------------------------------------------------------------
#
# Must happen before plugin-state inspection: the inspection compares
# OpenClaw's recorded rootDir against the path we are about to link, and
# the install step links the runtime package rather than this checkout or
# the caller's CWD.
#
# Staging is idempotent and validated before exposure — a package that
# fails validation aborts here with the previous one intact, rather than
# reaching OpenClaw half-populated.

MPM_RUNTIME_ROOT="$(mpm_runtime_root_from_bin "$MPM_BIN")"
if [ -z "$MPM_RUNTIME_ROOT" ] || [ "$MPM_RUNTIME_ROOT" = "/" ] || [ "$MPM_RUNTIME_ROOT" = "." ]; then
  err "could not derive a sane MPM runtime root from $MPM_BIN"
  err "refusing to stage — set MPM_RUNTIME_ROOT explicitly if this install is relocated."
  exit 2
fi
log "MPM runtime root: $MPM_RUNTIME_ROOT"

if ! PLUGIN_PKG_DIR="$(stage_runtime_package "$SCRIPT_DIR" "$MPM_RUNTIME_ROOT" \
        "$PLUGIN_ID" "${RUNTIME_PACKAGE_MANIFEST[@]}")"; then
  err "runtime package provisioning failed for $PLUGIN_ID; refusing to link an unvalidated package"
  exit 3
fi
PLUGIN_PKG_DIR="$(cd "$PLUGIN_PKG_DIR" && pwd -P)"
log "runtime package staged and validated: $PLUGIN_PKG_DIR"

# --------------------------------------------------------------------------
# 2. OpenClaw CLI presence
# --------------------------------------------------------------------------

if ! command -v openclaw >/dev/null 2>&1; then
  err "openclaw CLI not on PATH. Install OpenClaw first, then re-run this installer."
  err "  Runtime package (already prepared for linking): $PLUGIN_PKG_DIR"
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
  our_real="$(cd "$PLUGIN_PKG_DIR" && pwd -P 2>/dev/null || printf '%s' "$PLUGIN_PKG_DIR")"
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
    err "  this adapter path:  $PLUGIN_PKG_DIR"
    err "this installer will not silently overwrite an unrelated plugin source."
    err "operator actions:"
    err "  (a) uninstall the conflicting install:"
    err "        openclaw plugins uninstall $PLUGIN_ID"
    err "        $0"
    err "  (b) leave it alone and rename this adapter's id in $SCRIPT_DIR/openclaw.plugin.json"
    exit 1
    ;;
  absent|*)
    log "installing plugin '$PLUGIN_ID' from $PLUGIN_PKG_DIR (--link --force --accept-capabilities)"
    # The path argument is the resolved runtime package directory.
    # It was previously '.' (the caller's working directory), which
    # only happened to be the adapter when the operator ran the
    # installer from inside it — and which, after the source/runtime
    # split, would not have been the adapter at all. An absolute
    # runtime path is CWD-independent by construction.
    if ! timeout "${OPENCLAW_PLUGIN_INSTALL_TIMEOUT}s" \
        openclaw plugins install "$PLUGIN_PKG_DIR" --link --force --accept-capabilities \
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
MPM_BIN_CONFIGURED=0
if ! timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
    openclaw config set "plugins.entries.$PLUGIN_ID.config.mpmBin" "$MPM_BIN" \
      >>"$INSTALL_LOG" 2>&1; then
  warn "failed to persist mpmBin automatically. Operator run manually:"
  warn "  openclaw config set plugins.entries.$PLUGIN_ID.config.mpmBin $MPM_BIN"
  MPM_BIN_CONFIGURED=1
else
  MPM_BIN_CONFIGURED=0
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
UPDATE_REPAIR_OK=0
if timeout "${OPENCLAW_UPDATE_REPAIR_TIMEOUT}s" \
    openclaw update repair \
      >>"$INSTALL_LOG" 2>&1; then
  log "  update repair ok"
  UPDATE_REPAIR_OK=1
else
  warn "update repair returned non-zero (operator can run:"
  warn "  openclaw update repair && openclaw doctor --fix"
  warn "see $INSTALL_LOG)"
  UPDATE_REPAIR_OK=0
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
  PLUGIN_INSPECT_OK=1
else
  warn "openclaw plugins inspect $PLUGIN_ID did not return cleanly; inspect $INSTALL_LOG"
  PLUGIN_INSPECT_OK=0
fi

# --------------------------------------------------------------------------
# Required-classification
#
# Required steps:
#     - mpmBin config persistence (Step 5). Without it the gateway
#       cannot spawn mpm at runtime under its stripped PATH. Step 5
#       comment: "this is the PATH-gotcha mitigation."
#     - update repair convergence (Step 6). Header says "without
#       convergence, doctor warns and the install is not fully visible
#       to the gateway." `openclaw doctor --lint` will re-surface the
#       migration warning on the next start until this converges.
#     - plugins inspect verification (Step 8). The installer promises
#       the plugin is visible to openclaw; an inspect failure means
#       that promise is false.
#
# Best-effort (documented):
#     - gateway restart (Step 7). The systemd --user unit is
#       configured to auto-restart on failure, so a bounded restart
#       failure is not itself an installer failure. The classification
#       intentionally does NOT depend on RESTART_RC.

INSTALL_FAILED=0
if [ "$MPM_BIN_CONFIGURED" -ne 0 ]; then
  INSTALL_FAILED=1
elif [ "$UPDATE_REPAIR_OK" -ne 1 ]; then
  INSTALL_FAILED=1
elif [ "$PLUGIN_INSPECT_OK" -ne 1 ]; then
  INSTALL_FAILED=1
fi

if [ "$INSTALL_FAILED" -ne 0 ]; then
  cat >&2 <<NEXT
[mpm-auto-mode-persona-openclaw install] FAILED.

The plugin link is in place; OpenClaw did not settle into a healthy
state. Do NOT treat this install as successful.

Operator recovery:
  openclaw config set plugins.entries.$PLUGIN_ID.config.mpmBin $MPM_BIN
  openclaw update repair
  openclaw plugins inspect $PLUGIN_ID

Inspect $INSTALL_LOG for captured openclaw CLI output.
NEXT
  exit 1
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