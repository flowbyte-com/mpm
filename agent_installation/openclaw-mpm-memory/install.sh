#!/usr/bin/env bash
#
# install.sh — openclaw-mpm-memory adapter installer
#
# Owns the complete OpenClaw-specific setup for this plugin:
#
#   1. Locate or bootstrap MPM (canonical install paths first; PATH last)
#   2. Link/install this plugin into OpenClaw's plugin registry
#   3. Enable the plugin entry
#   4. Write plugin config:
#        plugins.entries.openclaw-mpm-memory.config.mpmBin (absolute)
#        plugins.entries.openclaw-mpm-memory.hooks.allowConversationAccess = true
#        plugins.entries.openclaw-mpm-memory.hooks.allowPromptInjection    = true
#   5. Switch plugins.slots.memory to openclaw-mpm-memory
#   6. Bounded safe gateway restart (when a gateway service exists)
#   7. Verify (openclaw plugins list + doctor)
#
# Idempotent: every step is safe to re-run. Plugin install uses --link
# when supported, otherwise falls back to a non-link install of the
# resolved plugin path. --force is never used (would overwrite unrelated
# state).
#
# Does NOT modify shell startup files. Does NOT touch the MPM substrate
# install (that lives in the root scripts/install.sh; this adapter
# assumes the operator ran that first OR this installer will bootstrap
# if it is missing).
#
# CWD-independent: the plugin directory is resolved from BASH_SOURCE[0],
# so the installer works from any current working directory.
#
# Plugin identity is read from openclaw.plugin.json (id="openclaw-mpm-memory")
# — do not rename.

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
  # If $SCRIPT_PATH is relative, resolve relative to $LINK_DIR.
  case "$SCRIPT_PATH" in
    /*) ;;
    *)  SCRIPT_PATH="$LINK_DIR/$SCRIPT_PATH" ;;
  esac
done
SCRIPT_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"

PLUGIN_ID="$(grep -oE '"id"[[:space:]]*:[[:space:]]*"[^"]+"' "$SCRIPT_DIR/openclaw.plugin.json" \
  | head -n1 | sed -E 's/.*"id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
if [ -z "$PLUGIN_ID" ]; then
  printf '[openclaw-mpm-memory install] ERROR: could not parse plugin id from %s/openclaw.plugin.json\n' "$SCRIPT_DIR" >&2
  exit 2
fi

# MPM canonical locations. Order matters: the substrate install (root
# scripts/install.sh) writes ~/.mpm/bin/mpm and symlinks it into
# ~/.local/bin/mpm. We accept either. We never rely on PATH resolution
# alone — on a freshly-created ~/.local/bin the current login session
# may not have it on PATH yet (proved in clean-profile Linux Mint test).
HOME_DIR="${HOME:-}"
MPM_CANONICAL_PRIMARY="$HOME_DIR/.mpm/bin/mpm"
MPM_CANONICAL_SYMLINK="$HOME_DIR/.local/bin/mpm"
PLUGIN_INSTALL_TIMEOUT="${OPENCLAW_PLUGIN_INSTALL_TIMEOUT:-30}"
GATEWAY_RESTART_TIMEOUT="${OPENCLAW_GATEWAY_RESTART_TIMEOUT:-20}"
MPM_BOOTSTRAP_URL="${MPM_BOOTSTRAP_URL:-}"

log()  { printf '[openclaw-mpm-memory install] %s\n' "$*" >&2; }
warn() { printf '[openclaw-mpm-memory install] WARN: %s\n' "$*" >&2; }
err()  { printf '[openclaw-mpm-memory install] ERROR: %s\n' "$*" >&2; }

# --------------------------------------------------------------------------
# 1. Locate MPM
# --------------------------------------------------------------------------
#
# Discovery order (deterministic, never relies on a freshly-touched
# PATH entry):
#   1. $HOME/.mpm/bin/mpm                  — the canonical install root
#   2. $HOME/.local/bin/mpm                — the canonical user symlink
#   3. command -v mpm                       — last-resort PATH lookup
#   4. absent — bootstrap (only if explicitly enabled by the operator
#      to keep this adapter predictable; no network call by default)
#
# We verify the resolved binary by absolute path. We do NOT mutate
# shell startup files.

locate_mpm() {
  if [ -x "$MPM_CANONICAL_PRIMARY" ]; then
    MPM_BIN="$MPM_CANONICAL_PRIMARY"
    log "mpm resolved via canonical install path: $MPM_BIN"
    return 0
  fi
  if [ -x "$MPM_CANONICAL_SYMLINK" ]; then
    # Resolve through the symlink so we record the real path.
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
  if [ -n "$MPM_BOOTSTRAP_URL" ]; then
    warn "mpm not found; bootstrapping via MPM_BOOTSTRAP_URL"
    if command -v curl >/dev/null 2>&1; then
      if ! curl -fsSL "$MPM_BOOTSTRAP_URL" | bash; then
        err "MPM bootstrap failed. Run the root scripts/install.sh once, then re-run this installer."
        exit 1
      fi
    elif command -v wget >/dev/null 2>&1; then
      if ! wget -qO- "$MPM_BOOTSTRAP_URL" | bash; then
        err "MPM bootstrap failed. Run the root scripts/install.sh once, then re-run this installer."
        exit 1
      fi
    else
      err "mpm not found and neither curl nor wget is available. Run scripts/install.sh first."
      exit 1
    fi
    # Re-resolve after bootstrap.
    if ! locate_mpm; then
      err "mpm still not resolvable after bootstrap. Run scripts/install.sh manually, then re-run this installer."
      exit 1
    fi
  else
    err "mpm not found on this host. Run scripts/install.sh from the repo root, then re-run this installer."
    err "  Canonical paths checked: $MPM_CANONICAL_PRIMARY, $MPM_CANONICAL_SYMLINK"
    err "  To bootstrap from a URL in unattended flows, set MPM_BOOTSTRAP_URL=<url>."
    exit 1
  fi
fi

# Verify the resolved binary actually answers. This is the single
# authoritative "mpm is working" check; we never call bare `mpm` because
# the gateway-relevant test is whether the absolute path resolves.
if ! "$MPM_BIN" --version >/dev/null 2>&1; then
  err "mpm binary at $MPM_BIN is present but does not execute cleanly. Re-run scripts/install.sh."
  exit 1
fi
log "mpm version: $("$MPM_BIN" --version 2>&1 | head -n1)"

# --------------------------------------------------------------------------
# 2. OpenClaw CLI presence
# --------------------------------------------------------------------------
#
# Everything from here on needs the OpenClaw CLI. If it is missing we
# cannot complete the OpenClaw-specific install path; report clearly
# and stop rather than half-configuring the plugin.

if ! command -v openclaw >/dev/null 2>&1; then
  err "openclaw CLI not on PATH. Install OpenClaw first, then re-run this installer."
  err "  Plugin dir (already prepared for linking): $SCRIPT_DIR"
  exit 1
fi

# --------------------------------------------------------------------------
# 3. Install/link the plugin
# --------------------------------------------------------------------------
#
# Link is the dev/MV install path. We pass --link so source edits in
# $SCRIPT_DIR take effect without a re-install. The plugin path is
# absolute (resolved from BASH_SOURCE[0]) — no caller CWD dependency.
#
# `openclaw plugins install` is idempotent: if the plugin is already
# linked/installed it will report so without error in 2026.9.4. We do
# NOT pass --force (would overwrite unrelated state) and we do NOT
# pass --accept-capabilities (operator must accept capabilities
# explicitly per their security policy).

log "installing plugin '$PLUGIN_ID' from $SCRIPT_DIR"
if ! timeout "${PLUGIN_INSTALL_TIMEOUT}s" \
    openclaw plugins install "$SCRIPT_DIR" --link \
      >/tmp/openclaw-mpm-memory-install.log 2>&1; then
  warn "openclaw plugins install failed; see /tmp/openclaw-mpm-memory-install.log"
  warn "retrying without --link (operator may need to install from a published artifact)"
  if ! timeout "${PLUGIN_INSTALL_TIMEOUT}s" \
      openclaw plugins install "$SCRIPT_DIR" \
        >>/tmp/openclaw-mpm-memory-install.log 2>&1; then
    err "plugin install failed twice. Inspect /tmp/openclaw-mpm-memory-install.log and re-run."
    exit 1
  fi
fi

# --------------------------------------------------------------------------
# 4. Enable the plugin entry
# --------------------------------------------------------------------------

if openclaw plugins inspect "$PLUGIN_ID" --json >/dev/null 2>&1 \
   || openclaw plugins list --json 2>/dev/null | grep -q "\"$PLUGIN_ID\""; then
  if timeout 10s openclaw plugins enable "$PLUGIN_ID" \
       >>/tmp/openclaw-mpm-memory-install.log 2>&1; then
    log "plugin entry '$PLUGIN_ID' enabled"
  else
    warn "openclaw plugins enable failed (already enabled or no-op); continuing"
  fi
else
  err "plugin '$PLUGIN_ID' not visible to openclaw plugins list after install. Inspect /tmp/openclaw-mpm-memory-install.log."
  exit 1
fi

# --------------------------------------------------------------------------
# 5. Persist plugin config (absolute mpmBin + both hook permission flags)
# --------------------------------------------------------------------------
#
# Two flags are REQUIRED for the typed-hook wake-context path:
#   allowConversationAccess (non-bundled plugin opt-in)
#   allowPromptInjection     (agent_turn_prepare lands in OpenClaw's
#                             promptInjectionHookNameSet)
# Without both, the plugin runs but wake-context injection is silently
# blocked at register time. We persist both unconditionally.

CONFIG_SET() {
  local key="$1" value="$2"
  timeout 10s openclaw config set "$key" "$value" \
    >>/tmp/openclaw-mpm-memory-install.log 2>&1
}

log "persisting plugins.entries.$PLUGIN_ID.config.mpmBin=$MPM_BIN"
if ! CONFIG_SET "plugins.entries.$PLUGIN_ID.config.mpmBin" "$MPM_BIN"; then
  warn "failed to persist mpmBin automatically. Operator run manually:"
  warn "  openclaw config set plugins.entries.$PLUGIN_ID.config.mpmBin $MPM_BIN"
fi

log "persisting plugins.entries.$PLUGIN_ID.hooks.allowConversationAccess=true"
if ! CONFIG_SET "plugins.entries.$PLUGIN_ID.hooks.allowConversationAccess" "true"; then
  warn "failed to persist hooks.allowConversationAccess. Operator run manually:"
  warn "  openclaw config set plugins.entries.$PLUGIN_ID.hooks.allowConversationAccess true"
fi

log "persisting plugins.entries.$PLUGIN_ID.hooks.allowPromptInjection=true"
if ! CONFIG_SET "plugins.entries.$PLUGIN_ID.hooks.allowPromptInjection" "true"; then
  warn "failed to persist hooks.allowPromptInjection. Operator run manually:"
  warn "  openclaw config set plugins.entries.$PLUGIN_ID.hooks.allowPromptInjection true"
fi

# --------------------------------------------------------------------------
# 6. Switch the memory slot
# --------------------------------------------------------------------------

log "switching plugins.slots.memory to $PLUGIN_ID"
if ! CONFIG_SET "plugins.slots.memory" "$PLUGIN_ID"; then
  warn "failed to switch plugins.slots.memory. Operator run manually:"
  warn "  openclaw config set plugins.slots.memory $PLUGIN_ID"
fi

# --------------------------------------------------------------------------
# 7. memory-core coexistence
# --------------------------------------------------------------------------
#
# We intentionally do NOT touch memory-core's enabled flag. The
# README's "(Optional, recommended) Silence memory-core" wording
# reflects operator preference — many OpenClaw installs keep both
# running and rely on the slot assignment to direct which one serves.
# Auto-disabling a stock OpenClaw plugin is a destructive cross-cutting
# change that belongs to operator policy, not this installer.
#
# We surface the current state and the manual one-liner so the
# operator can make the decision deliberately.

if timeout 5s openclaw plugins list --json 2>/dev/null \
     | grep -q '"id": "memory-core"'; then
  memcore_state="$(timeout 5s openclaw config get plugins.entries.memory-core.enabled 2>/dev/null || echo "?")"
  log "memory-core detected (enabled=$memcore_state). The slot assignment above directs memory_search to $PLUGIN_ID."
  log "To silence memory-core (recommended for clarity, not required):"
  log "  openclaw config set plugins.entries.memory-core.enabled false"
fi

# --------------------------------------------------------------------------
# 8. Bounded gateway restart
# --------------------------------------------------------------------------
#
# We previously observed the foreground TUI/gateway and systemd service
# competing for gateway ownership. The 2026.9.4 `openclaw gateway
# restart --safe --wait <bounded>` semantics are appropriate: OpenClaw
# drains active work within the wait, then restarts. We use --safe +
# a bounded --wait so the installer never hangs indefinitely.
#
# If no gateway service exists (Docker container, minimal CI env), we
# skip silently — no destructive attempt to start one.

if timeout 5s openclaw gateway status --json >/dev/null 2>&1; then
  log "requesting bounded safe gateway restart (timeout ${GATEWAY_RESTART_TIMEOUT}s)"
  if ! timeout "${GATEWAY_RESTART_TIMEOUT}s" \
      openclaw gateway restart --safe --wait "${GATEWAY_RESTART_TIMEOUT}s" \
        >>/tmp/openclaw-mpm-memory-install.log 2>&1; then
    warn "gateway restart hit the bounded timeout or returned non-zero."
    warn "The plugin config above is persisted; the operator can restart manually:"
    warn "  openclaw gateway restart --safe"
  fi
else
  log "no gateway service detected; skipping gateway restart (config above is persisted and active on next gateway start)"
fi

# --------------------------------------------------------------------------
# 9. Verify
# --------------------------------------------------------------------------

if timeout 5s openclaw plugins inspect "$PLUGIN_ID" --json >/dev/null 2>&1; then
  log "plugin visible to openclaw: $PLUGIN_ID"
else
  warn "openclaw plugins inspect $PLUGIN_ID did not return cleanly; inspect /tmp/openclaw-mpm-memory-install.log"
fi

cat >&2 <<'NEXT'
[openclaw-mpm-memory install] Done.

Verification (optional):
  openclaw doctor --lint --only core/doctor/memory-search --json
  openclaw plugins list                          # confirm enabled

If anything above reported WARN, see /tmp/openclaw-mpm-memory-install.log
for the captured openclaw CLI output. Plugin config and slot switch are
persisted even when gateway restart was skipped or bounded.
NEXT
