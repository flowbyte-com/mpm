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

# 4. Print summary + next steps
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
