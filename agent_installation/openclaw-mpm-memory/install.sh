#!/usr/bin/env bash
# Bootstrap MPM for the openclaw-mpm-memory plugin.
# Idempotent: detects existing installs and skips.

set -euo pipefail

log() { printf '[openclaw-mpm-memory install] %s\n' "$*" >&2; }

# 1. Detect mpm on PATH
if command -v mpm >/dev/null 2>&1; then
  log "mpm already on PATH: $(command -v mpm)"
else
  log "mpm not found on PATH; attempting bootstrap..."
  MPM_INSTALL_URL="${MPM_INSTALL_URL:-https://flowbyte.com/mpm/install.sh}"
  if command -v curl >/dev/null 2>&1; then
    if curl -fsSL "$MPM_INSTALL_URL" | bash; then
      log "MPM bootstrap script finished."
    else
      log "WARNING: MPM bootstrap script failed. Install MPM manually then re-run this script."
      exit 1
    fi
  else
    log "ERROR: curl not available and mpm not on PATH. Install mpm first."
    exit 1
  fi
fi

# 2. Verify mpm is callable
if ! command -v mpm >/dev/null 2>&1; then
  log "ERROR: mpm still not on PATH after bootstrap."
  exit 1
fi
mpm --version >/dev/null || { log "mpm binary present but not callable"; exit 1; }
log "mpm version: $(mpm --version 2>&1 | head -1)"

# 3. Ensure the scheduler daemon is alive (systemd --user unit)
if command -v systemctl >/dev/null 2>&1; then
  if systemctl --user is-active mpm-scheduler >/dev/null 2>&1; then
    log "mpm-scheduler already active"
  elif systemctl --user list-unit-files mpm-scheduler.service >/dev/null 2>&1; then
    log "starting mpm-scheduler (systemd --user)..."
    systemctl --user start mpm-scheduler || log "WARN: failed to start mpm-scheduler (continuing — read-only MPM still works)"
  else
    log "NOTE: mpm-scheduler.service not installed. Read-only MPM access works; scheduled wake injection requires the unit to be enabled separately."
  fi
else
  log "NOTE: systemctl not available on PATH; skipping mpm-scheduler check. mpm read-only access still works."
fi

# 4. Persist the absolute mpm path to plugin config.
#
# Why: the gateway runs under a systemd --user service with a stripped PATH,
# so a bare `mpmBin: "mpm"` config (PATH-resolved) fails at runtime with
# `spawn mpm ENOENT`. Writing the absolute path here closes that gap once
# at install time instead of every operator hitting it post-install.
#
# Skip silently if `openclaw` is not on PATH (e.g. minimal environments,
# Docker containers) — the operator can run the equivalent `openclaw
# config set` command themselves later. Failures of `openclaw config set`
# are non-fatal: the plugin still works once the operator sets it.
if command -v openclaw >/dev/null 2>&1; then
  MPM_ABS_PATH="$(command -v mpm)"
  if [ -n "${MPM_ABS_PATH}" ]; then
    log "persisting mpmBin=${MPM_ABS_PATH} to plugin config (absolute path avoids systemd PATH gotcha)"
    if openclaw config set plugins.entries.openclaw-mpm-memory.config.mpmBin "${MPM_ABS_PATH}" >/dev/null 2>&1; then
      log "mpmBin persisted. Restart the gateway to apply."
    else
      log "WARN: failed to persist mpmBin automatically. Run manually:"
      log "  openclaw config set plugins.entries.openclaw-mpm-memory.config.mpmBin ${MPM_ABS_PATH}"
    fi
  fi
else
  log "NOTE: openclaw CLI not on PATH; skipping automatic mpmBin persistence. After linking the plugin, set manually:"
  log "  openclaw config set plugins.entries.openclaw-mpm-memory.config.mpmBin \$(command -v mpm)"
fi

# 5. Persist hooks.allowConversationAccess=true (opt-in for typed hooks).
#
# Why: the plugin uses the typed hook `agent_turn_prepare` to inject
# wake context as `prependContext`. OpenClaw requires non-bundled plugins
# to explicitly opt in to typed hooks via this flag, otherwise the
# gateway silently blocks the hook at register time (the plugin keeps
# running but wake-context injection never fires). Added 0.1.3.
#
# This step is also non-fatal — the operator can set it manually.
if command -v openclaw >/dev/null 2>&1; then
  if openclaw config set plugins.entries.openclaw-mpm-memory.hooks.allowConversationAccess true >/dev/null 2>&1; then
    log "hooks.allowConversationAccess=true persisted (enables agent_turn_prepare typed hook)."
  else
    log "WARN: failed to persist hooks.allowConversationAccess. Run manually:"
    log "  openclaw config set plugins.entries.openclaw-mpm-memory.hooks.allowConversationAccess true"
  fi
else
  log "NOTE: openclaw CLI not on PATH; skipping hooks.allowConversationAccess persistence. Set manually after linking."
fi

# 6. Print summary + next steps
cat >&2 <<'NEXT'
[openclaw-mpm-memory install] Done.

Next steps (one-time):
  1. Link the plugin (dev install):
       openclaw plugins install ./openclaw-mpm-memory --link
  2. Enable the plugin entry:
       openclaw config set plugins.entries.openclaw-mpm-memory.enabled true
  3. Switch the memory slot from the default to this plugin:
       openclaw config set plugins.slots.memory openclaw-mpm-memory
  4. (Optional) Disable the default memory-core so it doesn't double-serve:
       openclaw config set plugins.entries.memory-core.enabled false
  5. Restart the gateway:
       openclaw gateway restart
  6. Verify:
       openclaw doctor --lint --only core/doctor/memory-search --json

Or just run `openclaw doctor --fix` after step 1 — doctor will detect the slot
configuration and surface anything missing.
NEXT
