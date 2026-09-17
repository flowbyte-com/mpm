#!/usr/bin/env bash
#
# install.sh — mpm-memory-openclaw adapter installer
#
# Owns the complete OpenClaw-specific setup for this plugin:
#
#   1. Locate or bootstrap MPM (canonical install paths first; PATH last)
#   2. Inspect existing plugin state via `openclaw plugins inspect --json`
#   3. Install/link this plugin into OpenClaw (only when required) using
#        --link --force --accept-capabilities
#   4. Enable the plugin entry
#   5. Write plugin config:
#        plugins.entries.mpm-memory-openclaw.config.mpmBin (absolute)
#        plugins.entries.mpm-memory-openclaw.hooks.allowConversationAccess = true
#        plugins.entries.mpm-memory-openclaw.hooks.allowPromptInjection    = true
#   6. Switch plugins.slots.memory to mpm-memory-openclaw
#   7. Surface (not auto-apply) the memory-core coexistence one-liner
#   8. Bounded safe gateway restart (when a gateway service is reachable)
#   9. Verify (openclaw plugins inspect + plugins list)
#
# Idempotency contract (OpenClaw 2026.9.4):
#   * Plugin absent → run `openclaw plugins install <path> --link --force
#     --accept-capabilities`. --force is the non-ClawHub trust acknowledgement;
#     --accept-capabilities is the declared-capability consent required by
#     OpenClaw 2026.9.4 for plugins that declare a non-empty surface. We are
#     installing a first-party adapter from a trusted local source, so both
#     flags are required for a deterministic fresh install.
#   * Plugin already linked from THIS adapter's absolute path → skip the
#     install step entirely. Re-running the installer must NOT re-issue
#     `plugins install` for an already-correctly-linked plugin (that would
#     emit unnecessary trust warnings and bump installedAt timestamps).
#   * Plugin installed but pointing at a different source → fail with a
#     clear operator action. The installer never silently overwrites an
#     unrelated plugin source.
#
# Gateway lifecycle:
#   * `openclaw gateway status --json` is wrapped in `timeout` AND uses the
#     CLI's own --timeout option. Both layers are required — the CLI timeout
#     bounds the RPC probe, the outer `timeout` bounds the whole process.
#   * `openclaw gateway restart --safe` is used (NOT `--safe --wait`: those
#     flags are mutually exclusive in 2026.9.4 — --wait is described as "For
#     non-safe restarts (plain restart); not compatible with --force or
#     --safe"). --safe already has bounded-wait semantics; we apply an
#     outer `timeout` for the hard cap.
#   * Gateway absent / inactive / unresponsive → config above is already
#     persisted; the installer skips the restart with a clear log line
#     rather than failing the whole install.
#
# Does NOT modify shell startup files. Does NOT touch the MPM substrate
# install (that lives in the root scripts/install.sh; this adapter
# assumes the operator ran that first OR this installer will bootstrap
# if it is missing).
#
# CWD-independent: the plugin directory is resolved from BASH_SOURCE[0],
# so the installer works from any current working directory.
#
# Plugin identity is read from openclaw.plugin.json (id="mpm-memory-openclaw")
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
  printf '[mpm-memory-openclaw install] ERROR: could not parse plugin id from %s/openclaw.plugin.json\n' "$SCRIPT_DIR" >&2
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

# OpenClaw CLI timeout budgets (override via env). Each OpenClaw
# lifecycle command we issue is bounded TWICE:
#   1. external `timeout` shell guard, and
#   2. where supported, the CLI's own --timeout flag (e.g. gateway status).
# Both layers exist because we have observed the foreground TUI/gateway
# and systemd service competing for gateway ownership and individual
# probes (especially `gateway status` with --json RPC probes) can
# otherwise hang for tens of seconds.
OPENCLAW_PLUGIN_INSTALL_TIMEOUT="${OPENCLAW_PLUGIN_INSTALL_TIMEOUT:-30}"
OPENCLAW_GATEWAY_STATUS_TIMEOUT="${OPENCLAW_GATEWAY_STATUS_TIMEOUT:-10}"
OPENCLAW_GATEWAY_RESTART_TIMEOUT="${OPENCLAW_GATEWAY_RESTART_TIMEOUT:-20}"
OPENCLAW_PLUGIN_INSPECT_TIMEOUT="${OPENCLAW_PLUGIN_INSPECT_TIMEOUT:-10}"
OPENCLAW_CONFIG_TIMEOUT="${OPENCLAW_CONFIG_TIMEOUT:-10}"
MPM_BOOTSTRAP_URL="${MPM_BOOTSTRAP_URL:-}"
INSTALL_LOG="/tmp/mpm-memory-openclaw-install.log"
: > "$INSTALL_LOG" 2>/dev/null || true

log()  { printf '[mpm-memory-openclaw install] %s\n' "$*" >&2; }
warn() { printf '[mpm-memory-openclaw install] WARN: %s\n' "$*" >&2; }
err()  { printf '[mpm-memory-openclaw install] ERROR: %s\n' "$*" >&2; }

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
# 3. Legacy plugin-id migration (one-time, idempotent)
# --------------------------------------------------------------------------
#
# The canonical plugin id for this adapter changed from
# `openclaw-mpm-memory` to `mpm-memory-openclaw` in the 2026-09-17
# namespace migration. Both ids resolve to the same plugin code at this
# path; OpenClaw's plugin registry, however, keys config by id. A host
# that previously ran an older install carries entries under the legacy
# id (e.g. plugins.entries.openclaw-mpm-memory.config.mpmBin,
# plugins.slots.memory=openclaw-mpm-memory, plugins.entries.openclaw-mpm-memory.enabled).
#
# Strategy (alpha-friendly, minimal):
#   1. Probe the legacy id via `plugins inspect` (bounded timeout).
#   2. If the legacy id is NOT registered → nothing to migrate, skip.
#   3. If the legacy id IS registered and points at $SCRIPT_DIR →
#      it is THIS adapter under the old name. Migrate:
#        plugins.entries.openclaw-mpm-memory.{config,enabled,hooks}
#            → plugins.entries.mpm-memory-openclaw.{config,enabled,hooks}
#        plugins.slots.memory = openclaw-mpm-memory
#            → plugins.slots.memory = mpm-memory-openclaw
#      then `openclaw plugins uninstall openclaw-mpm-memory` to
#      release the legacy id.
#   4. If the legacy id is registered but points elsewhere → leave it
#      alone (it is a different installation, not ours to seize).
#
# Each migration step is independent — failures are logged and
# non-fatal, but the operator can rerun the installer until the legacy
# id is gone. We do NOT silently leave both ids active.

LEGACY_PLUGIN_ID="openclaw-mpm-memory"
legacy_inspect="$(timeout "${OPENCLAW_PLUGIN_INSPECT_TIMEOUT}s" \
  openclaw plugins inspect "$LEGACY_PLUGIN_ID" --json 2>/dev/null || true)"
legacy_state="absent"
legacy_existing_root=""
if [ -n "$legacy_inspect" ] && command -v python3 >/dev/null 2>&1; then
  legacy_parsed="$(
    printf '%s\n' "$legacy_inspect" | python3 -c '
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
if not data.get("ok", True):
    sys.exit(0)
plugin = data.get("plugin") or {}
print(plugin.get("rootDir", ""))
' 2>/dev/null || true
  )"
  if [ -n "$legacy_parsed" ]; then
    legacy_state="present"
    legacy_existing_root="$legacy_parsed"
  fi
fi

if [ "$legacy_state" = "present" ]; then
  our_real="$(cd "$SCRIPT_DIR" && pwd -P 2>/dev/null || printf '%s' "$SCRIPT_DIR")"
  their_real="$(cd "$legacy_existing_root" 2>/dev/null && pwd -P 2>/dev/null || printf '%s' "$legacy_existing_root")"
  if [ "$their_real" = "$our_real" ]; then
    log "legacy plugin id '$LEGACY_PLUGIN_ID' found, pointing at this adapter — migrating to '$PLUGIN_ID'"
    # Migrate config: openclaw config patch {old: new} preserves nested structure.
    # Each property is patched individually because the schema is per-key.
    legacy_to_canonical_migrate() {
      local key="$1"
      # Read the legacy value, write it under the canonical key, then unset the legacy key.
      local val
      val="$(timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
        openclaw config get "plugins.entries.$LEGACY_PLUGIN_ID.$key" 2>/dev/null || true)"
      case "$val" in
        ""|"null"|"undefined")
          : # legacy key absent or unset
          ;;
        *)
          if timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
              openclaw config set "plugins.entries.$PLUGIN_ID.$key" "$val" \
                >>"$INSTALL_LOG" 2>&1; then
            log "  migrated plugins.entries.$LEGACY_PLUGIN_ID.$key → plugins.entries.$PLUGIN_ID.$key"
            timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
              openclaw config unset "plugins.entries.$LEGACY_PLUGIN_ID.$key" \
                >>"$INSTALL_LOG" 2>&1 || true
          else
            warn "  failed to migrate plugins.entries.$LEGACY_PLUGIN_ID.$key (operator can re-run)"
          fi
          ;;
      esac
    }
    legacy_to_canonical_migrate "config.mpmBin"
    legacy_to_canonical_migrate "config.timeoutMs"
    legacy_to_canonical_migrate "config.scope"
    legacy_to_canonical_migrate "config.limitDefault"
    legacy_to_canonical_migrate "enabled"
    legacy_to_canonical_migrate "hooks.allowConversationAccess"
    legacy_to_canonical_migrate "hooks.allowPromptInjection"
    # Migrate the memory slot if it pointed at the legacy id.
    slot_val="$(timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
      openclaw config get plugins.slots.memory 2>/dev/null || true)"
    if [ "$slot_val" = "$LEGACY_PLUGIN_ID" ]; then
      if timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
          openclaw config set plugins.slots.memory "$PLUGIN_ID" \
            >>"$INSTALL_LOG" 2>&1; then
        log "  migrated plugins.slots.memory: $LEGACY_PLUGIN_ID → $PLUGIN_ID"
      else
        warn "  failed to migrate plugins.slots.memory (operator can re-run)"
      fi
    fi
    # Uninstall the legacy id now that config has been migrated.
    if timeout "${OPENCLAW_PLUGIN_INSTALL_TIMEOUT}s" \
        openclaw plugins uninstall "$LEGACY_PLUGIN_ID" \
          >>"$INSTALL_LOG" 2>&1; then
      log "  uninstalled legacy plugin id '$LEGACY_PLUGIN_ID'"
    else
      warn "  failed to uninstall legacy plugin id '$LEGACY_PLUGIN_ID' (config is migrated; safe to remove manually)"
    fi
  else
    log "legacy plugin id '$LEGACY_PLUGIN_ID' present but pointing at $legacy_existing_root — not ours, leaving untouched"
  fi
fi

# --------------------------------------------------------------------------
# 4. Inspect existing plugin state
# --------------------------------------------------------------------------
#
# Three states are distinguished before any `plugins install` is issued:
#
#   absent              — plugin id is not in OpenClaw's registry.
#   linked-from-here    — plugin id is registered AND its rootDir
#                         equals our $SCRIPT_DIR. Re-running the
#                         installer is a true no-op for the install step.
#   conflicting         — plugin id is registered AND its rootDir points
#                         somewhere else. We must NOT silently overwrite
#                         the unrelated source; the installer fails with
#                         a clear operator action.
#
# Detection uses `openclaw plugins inspect <id> --json` (a fast sqlite
# read) bounded by an outer `timeout` and the (default) CLI --json-only
# path (no RPC probe). On parse failure or non-zero CLI exit we treat
# the result as "unknown" and fall through to a fresh install — the
# install step itself will surface the real error if the path is
# unreachable.

PLUGIN_STATE="absent"
PLUGIN_EXISTING_ROOT=""
inspect_output="$(timeout "${OPENCLAW_PLUGIN_INSPECT_TIMEOUT}s" \
  openclaw plugins inspect "$PLUGIN_ID" --json 2>/dev/null || true)"
if [ -n "$inspect_output" ] && command -v python3 >/dev/null 2>&1; then
  parsed_root="$(
    printf '%s\n' "$inspect_output" | python3 -c '
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
if not data.get("ok", True):
    sys.exit(0)
plugin = data.get("plugin") or {}
print(plugin.get("rootDir", ""))
' 2>/dev/null || true
  )"
  if [ -n "$parsed_root" ]; then
    # Compare absolute paths via realpath to handle symlinks in either side.
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
fi

# --------------------------------------------------------------------------
# 4. Install/link the plugin (state-driven)
# --------------------------------------------------------------------------
#
# OpenClaw 2026.9.4 install contract:
#
#   --force                          Confirm non-ClawHub sources and
#                                    overwrite an existing plugin. We
#                                    accept this ONLY for the local
#                                    trusted source path; the conflict
#                                    case above prevents it from
#                                    overwriting an unrelated plugin.
#   --accept-capabilities            Required when the plugin declares
#                                    capabilities (this plugin does:
#                                    memory_search, memory_get) and they
#                                    have not been accepted yet. Without
#                                    this flag, `plugins install` returns
#                                    "Plugin X requires capability
#                                    consent" and aborts.
#   --link                           Symlink the local source instead of
#                                    copying. Required so source edits in
#                                    $SCRIPT_DIR take effect without a
#                                    re-install.
#
# Failure modes we explicitly handle:
#
#   * `plugins install` rejected because the plugin already exists at a
#     different path (e.g. a previous non-link install in
#     ~/.openclaw/extensions/<id>) → fail clearly with operator action.
#     We do NOT silently fall back to a copy install — that would
#     change the deployment topology the operator chose.
#   * Capability consent required but not provided → we have already
#     provided --accept-capabilities. Anything else is a real error
#     and is surfaced.

case "$PLUGIN_STATE" in
  linked-from-here)
    log "plugin '$PLUGIN_ID' already linked from $PLUGIN_EXISTING_ROOT; skipping install step (no destructive overwrite, no trust-warning noise)"
    ;;
  conflicting)
    err "plugin '$PLUGIN_ID' is already installed but points at a different source."
    err "  registered rootDir: $PLUGIN_EXISTING_ROOT"
    err "  this adapter path:  $SCRIPT_DIR"
    err "this installer will not silently overwrite an unrelated plugin source."
    err "operator actions (pick exactly one):"
    err "  (a) the existing install is THIS adapter from another path — uninstall it first, then re-run this installer:"
    err "        openclaw plugins uninstall $PLUGIN_ID"
    err "        $0"
    err "  (b) the existing install is unrelated — pick a different plugin id (rename $SCRIPT_DIR/openclaw.plugin.json and update PLUGIN_ID)."
    exit 1
    ;;
  absent|*)
    log "installing plugin '$PLUGIN_ID' from $SCRIPT_DIR (--link --force --accept-capabilities)"
    if ! timeout "${OPENCLAW_PLUGIN_INSTALL_TIMEOUT}s" \
        openclaw plugins install "$SCRIPT_DIR" --link --force --accept-capabilities \
          >>"$INSTALL_LOG" 2>&1; then
      # Inspect the captured log to give the operator an actionable error
      # rather than a bare "exit 1".
      err "fresh plugin install failed. Tail of $INSTALL_LOG:"
      tail -n 20 "$INSTALL_LOG" >&2 || true
      err "this installer requires --force (non-ClawHub trust acknowledgement)"
      err "and --accept-capabilities (declared capability consent). Both are passed"
      err "automatically. If this fails, the most common cause is that the plugin id"
      err "is already registered at a different source path; check with:"
      err "  openclaw plugins inspect $PLUGIN_ID --json"
      err "and either uninstall the conflicting install or rename this adapter's id."
      exit 1
    fi
    ;;
esac

# --------------------------------------------------------------------------
# 5. Enable the plugin entry
# --------------------------------------------------------------------------

if timeout "${OPENCLAW_PLUGIN_INSPECT_TIMEOUT}s" \
   openclaw plugins inspect "$PLUGIN_ID" --json >/dev/null 2>&1; then
  if timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
      openclaw plugins enable "$PLUGIN_ID" \
        >>"$INSTALL_LOG" 2>&1; then
    log "plugin entry '$PLUGIN_ID' enabled"
  else
    warn "openclaw plugins enable failed (already enabled or no-op); continuing"
  fi
else
  err "plugin '$PLUGIN_ID' not visible to openclaw plugins inspect after install. Inspect $INSTALL_LOG."
  exit 1
fi

# --------------------------------------------------------------------------
# 6. Persist plugin config (absolute mpmBin + both hook permission flags)
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
  timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
    openclaw config set "$key" "$value" \
      >>"$INSTALL_LOG" 2>&1
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
# 7. Switch the memory slot
# --------------------------------------------------------------------------

log "switching plugins.slots.memory to $PLUGIN_ID"
if ! CONFIG_SET "plugins.slots.memory" "$PLUGIN_ID"; then
  warn "failed to switch plugins.slots.memory. Operator run manually:"
  warn "  openclaw config set plugins.slots.memory $PLUGIN_ID"
fi

# --------------------------------------------------------------------------
# 8. memory-core coexistence
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

if timeout "${OPENCLAW_PLUGIN_INSPECT_TIMEOUT}s" \
   openclaw plugins list --json 2>/dev/null \
     | grep -q '"id": "memory-core"'; then
  memcore_state="$(timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
    openclaw config get plugins.entries.memory-core.enabled 2>/dev/null || echo "?")"
  log "memory-core detected (enabled=$memcore_state). The slot assignment above directs memory_search to $PLUGIN_ID."
  log "To silence memory-core (recommended for clarity, not required):"
  log "  openclaw config set plugins.entries.memory-core.enabled false"
fi

# --------------------------------------------------------------------------
# 9. Bounded gateway lifecycle (status + restart)
# --------------------------------------------------------------------------
#
# `openclaw gateway status --json` is bounded TWICE:
#   1. external `timeout ${OPENCLAW_GATEWAY_STATUS_TIMEOUT}s` shell guard
#   2. the CLI's own `--timeout` flag (we pass status_timeout_ms, default
#      equal to OPENCLAW_GATEWAY_STATUS_TIMEOUT in seconds * 1000)
#
# `openclaw gateway restart --safe` is bounded by an outer `timeout`
# wrapper. We do NOT pass --wait: in OpenClaw 2026.9.4 --wait is
# documented as "For non-safe restarts (plain restart); not compatible
# with --force or --safe", so --safe --wait is an invalid combination.
# --safe already has bounded-wait semantics ("may force after the
# timeout expires"); the outer `timeout` enforces the hard cap.
#
# If the gateway is absent / unhealthy / unresponsive, we skip the
# restart silently and report it. Configuration above is already
# persisted and will apply on the next gateway start.

# Convert status seconds to ms for the CLI flag (CLI requires integer ms).
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
    warn "The plugin config above is persisted; the operator can restart manually:"
    warn "  openclaw gateway restart --safe"
  fi
else
  log "no gateway service detected (status timed out or non-responsive); skipping gateway restart."
  log "  config above is persisted and will apply on the next gateway start."
fi

# --------------------------------------------------------------------------
# 10. Verify
# --------------------------------------------------------------------------

if timeout "${OPENCLAW_PLUGIN_INSPECT_TIMEOUT}s" \
   openclaw plugins inspect "$PLUGIN_ID" --json >/dev/null 2>&1; then
  log "plugin visible to openclaw: $PLUGIN_ID"
else
  warn "openclaw plugins inspect $PLUGIN_ID did not return cleanly; inspect $INSTALL_LOG"
fi

cat >&2 <<NEXT
[mpm-memory-openclaw install] Done.

Verification (optional):
  openclaw doctor --lint --only core/doctor/memory-search --json
  openclaw plugins list                          # confirm enabled

If anything above reported WARN, see $INSTALL_LOG
for the captured openclaw CLI output. Plugin config and slot switch are
persisted even when gateway restart was skipped or bounded.
NEXT
