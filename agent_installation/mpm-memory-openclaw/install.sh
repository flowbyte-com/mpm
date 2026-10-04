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
# install (that lives in the install.sh; this adapter
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
# install.sh) writes ~/.mpm/bin/mpm and symlinks it into
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
OPENCLAW_GATEWAY_VERIFY_TIMEOUT="${OPENCLAW_GATEWAY_VERIFY_TIMEOUT:-15}"
OPENCLAW_UPDATE_REPAIR_TIMEOUT="${OPENCLAW_UPDATE_REPAIR_TIMEOUT:-60}"
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
        err "MPM bootstrap failed. Run the install.sh once, then re-run this installer."
        exit 1
      fi
    elif command -v wget >/dev/null 2>&1; then
      if ! wget -qO- "$MPM_BOOTSTRAP_URL" | bash; then
        err "MPM bootstrap failed. Run the install.sh once, then re-run this installer."
        exit 1
      fi
    else
      err "mpm not found and neither curl nor wget is available. Run install.sh first."
      exit 1
    fi
    # Re-resolve after bootstrap.
    if ! locate_mpm; then
      err "mpm still not resolvable after bootstrap. Run install.sh manually, then re-run this installer."
      exit 1
    fi
  else
    err "mpm not found on this host. Run install.sh from the repo root, then re-run this installer."
    err "  Canonical paths checked: $MPM_CANONICAL_PRIMARY, $MPM_CANONICAL_SYMLINK"
    err "  To bootstrap from a URL in unattended flows, set MPM_BOOTSTRAP_URL=<url>."
    exit 1
  fi
fi

# Verify the resolved binary actually answers. This is the single
# authoritative "mpm is working" check; we never call bare `mpm` because
# the gateway-relevant test is whether the absolute path resolves.
if ! "$MPM_BIN" --version >/dev/null 2>&1; then
  err "mpm binary at $MPM_BIN is present but does not execute cleanly. Re-run install.sh."
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
# namespace migration. OpenClaw's plugin registry, however, keys
# config by id. A host that previously ran an older install carries
# entries under the legacy id (plugins.entries.openclaw-mpm-memory.*,
# plugins.slots.memory=openclaw-mpm-memory) — these persist
# INDEPENDENTLY of whether the legacy linked directory still exists on
# disk.
#
# The CRITICAL real-world upgrade case this section handles: the
# repository has been git-mv'd from `agent_installation/openclaw-mpm-memory/`
# to `agent_installation/mpm-memory-openclaw/`. The old linked directory
# no longer exists. A naive "legacy rootDir == $SCRIPT_DIR" ownership
# test FAILS in exactly this case (the new $SCRIPT_DIR is a different
# path from the legacy recorded rootDir) and silently leaves stale
# config behind, with `openclaw config validate` continuing to report
# `plugins.load.paths: plugin path not found: ...openclaw-mpm-memory`.
#
# Ownership evidence (strongest to weakest):
#
#   1. inspect_root_match      legacy rootDir from `plugins inspect`
#                               resolves to the canonical former path
#                               ($(dirname "$SCRIPT_DIR")/openclaw-mpm-memory)
#   2. inspect_root_different  legacy rootDir resolves to some other
#                               existing path → CONFLICT, refuse
#   3. registry_path_match     install record under legacy id has
#                               sourcePath / installPath matching the
#                               canonical former path
#   4. registry_path_different install record has a different existing
#                               path → CONFLICT, refuse
#   5. inspect_or_registry_failed
#                               inspect / registry unavailable (the
#                               path has vanished); fall back to
#                               config-key evidence
#   6. slot_points_to_legacy   plugins.slots.memory = openclaw-mpm-memory
#   7. entry_key_present       plugins.entries.openclaw-mpm-memory.<k>
#                               has a non-empty value for any known key
#   8. no_evidence             nothing to migrate; no-op
#
# Cases 1, 3, 5-with-6, 5-with-7, 6, 7 → OWNED → migrate
# Cases 2, 4 → CONFLICT → fail closed with operator action
# Case 8 → no action
#
# We do NOT blindly trust `plugins inspect` succeeding — the linked
# directory may have been renamed out from under the registry record.
# We do NOT silently leave both ids active.
#
# Each migration step is independent — failures are logged as WARN
# (the operator can rerun the installer until the legacy id is gone)
# but the installer never fails the install because a single legacy
# key was missing.

LEGACY_PLUGIN_ID="openclaw-mpm-memory"
# Canonical former adapter directory: the pre-2026-09-17 sibling of
# $SCRIPT_DIR. Used to recognize ownership when the legacy path no
# longer exists on disk — the realpath comparison below tolerates
# either form (path still present or already removed).
LEGACY_ADAPTER_DIR_RAW="$(cd "$(dirname "$SCRIPT_DIR")" 2>/dev/null && printf '%s/openclaw-mpm-memory' "$(_q="${PWD:-}"; printf '%s' "$_q")")"
# Resolve via readlink -f when the path exists; otherwise normalize
# the literal we built above. readlink -f fails on missing paths, so
# we try, then fall back to a manual resolution.
if [ -d "$(dirname "$SCRIPT_DIR")/openclaw-mpm-memory" ]; then
  LEGACY_ADAPTER_DIR="$(cd "$(dirname "$SCRIPT_DIR")/openclaw-mpm-memory" 2>/dev/null && pwd -P)" \
    || LEGACY_ADAPTER_DIR="$(dirname "$SCRIPT_DIR")/openclaw-mpm-memory"
else
  LEGACY_ADAPTER_DIR="$(dirname "$SCRIPT_DIR")/openclaw-mpm-memory"
fi
OUR_REAL="$(cd "$SCRIPT_DIR" && pwd -P 2>/dev/null || printf '%s' "$SCRIPT_DIR")"
LEGACY_REAL="$LEGACY_ADAPTER_DIR"

# Helper: try to read a JSON field out of an openclaw command. Returns
# empty if the command failed or the JSON did not contain the field.
# Tolerates the config-warnings header that the CLI prints before JSON.
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
# Walk dotted field path through nested dicts.
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

# Helper: extract a list of strings from a JSON field. Used for
# installRecords[id].sourcePath etc. (each is a string, but we accept
# lists to be defensive).
extract_json_field_or_empty() {
  local field="$1" input="$2"
  extract_json_field "$field" "$input"
}

# Step A: probe the legacy plugin id. inspect may fail (linked root
# vanished) — that is NOT a reason to skip migration.
legacy_inspect="$(timeout "${OPENCLAW_PLUGIN_INSPECT_TIMEOUT}s" \
  openclaw plugins inspect "$LEGACY_PLUGIN_ID" --json 2>/dev/null || true)"
legacy_inspect_root="$(extract_json_field "plugin.rootDir" "$legacy_inspect")"
legacy_inspect_ok="$(extract_json_field "ok" "$legacy_inspect")"

# Step B: probe the registry for install records. registry may also
# fail (e.g. when the entire config is invalid because of the stale
# load paths). We treat that as "no registry evidence available" and
# rely on config keys below.
legacy_registry="$(timeout "${OPENCLAW_PLUGIN_INSPECT_TIMEOUT}s" \
  openclaw plugins registry --json 2>/dev/null || true)"
legacy_registry_source="$(extract_json_field "persisted.installRecords.${LEGACY_PLUGIN_ID}.sourcePath" "$legacy_registry")"
legacy_registry_install="$(extract_json_field "persisted.installRecords.${LEGACY_PLUGIN_ID}.installPath" "$legacy_registry")"

# Step C: probe legacy config keys (these survive even when the linked
# directory is gone — this is what makes the vanished-path case
# observable).
legacy_slot_val="$(timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
  openclaw config get plugins.slots.memory 2>/dev/null || true)"
legacy_to_canonical_migrate_get() {
  timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
    openclaw config get "plugins.entries.$LEGACY_PLUGIN_ID.$1" 2>/dev/null \
    || true
}
legacy_entry_mpmBin="$(legacy_to_canonical_migrate_get "config.mpmBin")"
legacy_entry_enabled="$(legacy_to_canonical_migrate_get "enabled")"
legacy_entry_hook_aca="$(legacy_to_canonical_migrate_get "hooks.allowConversationAccess")"
legacy_entry_hook_api="$(legacy_to_canonical_migrate_get "hooks.allowPromptInjection")"
legacy_entry_has_value=""
for v in "$legacy_entry_mpmBin" "$legacy_entry_enabled" "$legacy_entry_hook_aca" "$legacy_entry_hook_api"; do
  case "$v" in
    ""|"null"|"undefined") ;;
    *) legacy_entry_has_value=1; break ;;
  esac
done

# Step D: classify ownership.
#
# Precedence (strongest to weakest — config-only fallback is LAST):
#
#   A. inspect_root_match       live `plugins inspect` returns a rootDir
#                                matching the canonical former path
#   B. inspect_root_different   live inspect returns a DIFFERENT existing
#                                rootDir → CONFLICT (refuse, do not fall
#                                through to config-only)
#   C. registry_path_match       installRecords[id].sourcePath matches
#                                the canonical former path
#   D. registry_path_different   installRecords[id] points at another
#                                EXISTING path → CONFLICT (refuse, do not
#                                fall through to config-only)
#   E. slot_points_to_legacy     plugins.slots.memory == legacy id
#                                (config-only fallback; only used when A-D
#                                produced no ownership verdict)
#   F. entry_key_present         plugins.entries.<id>.<k> non-empty
#                                (config-only fallback; same gating)
#   G. no_evidence               nothing to migrate → no-op
#
# CRITICAL SAFETY PROPERTY:
#   B and D always win over E/F. The fallback chain (E → F) only runs
#   when neither A-D produced an ownership verdict. An unrelated plugin
#   that genuinely owns the legacy id and points at a different source
#   will always be detected via inspect or registry (whichever survives)
#   before config-only evidence is consulted. Config-only evidence is
#   only ever used to claim ownership of an id that has no
#   inspect/registry ownership record (typical real-world case after
#   git mv: install record survives but inspect fails because the
#   linked directory vanished).
legacy_ownership="none"
legacy_evidence="no_evidence"
legacy_conflict_path=""

# Helper: compare two paths even if one doesn't exist. We compare
# both the readlink -f resolved form (when possible) and the literal
# form so a missing legacy directory still matches a recorded literal
# path that points at the same canonical former location.
paths_match() {
  local a="$1" b="$2"
  # Empty-arg guard — both sides must be non-empty.
  if [ -z "$a" ] || [ -z "$b" ]; then
    return 1
  fi
  # Exact literal match.
  if [ "$a" = "$b" ]; then
    return 0
  fi
  # Both resolved via pwd -P when readable.
  local ra rb
  ra="$(cd "$a" 2>/dev/null && pwd -P)" || ra="$a"
  rb="$(cd "$b" 2>/dev/null && pwd -P)" || rb="$b"
  if [ "$ra" = "$rb" ]; then
    return 0
  fi
  return 1
}

# Python's json.dumps renders JSON booleans as "true"/"false" (lowercase).
# Our extractor prints them verbatim. The empty-string fallthrough covers the
# inspect unavailable / non-OK cases. We accept "true", "True", "1", and
# also accept the bare non-empty case (Python's "True" is the only path that
# produces capital-T — we accept it as a safety net).
if [ "$legacy_inspect_ok" = "true" ] || [ "$legacy_inspect_ok" = "True" ] || [ "$legacy_inspect_ok" = "1" ]; then
  # inspect succeeded — strongest evidence (A or B).
  if [ -n "$legacy_inspect_root" ] && paths_match "$legacy_inspect_root" "$LEGACY_ADAPTER_DIR"; then
    legacy_ownership="owned"
    legacy_evidence="inspect_root_match:$legacy_inspect_root"
  elif [ -n "$legacy_inspect_root" ]; then
    # B: inspect_root_different — STOP HERE. Do NOT fall through to
    # registry/config fallback. An unrelated plugin owns the legacy id.
    legacy_ownership="conflict"
    legacy_conflict_path="$legacy_inspect_root"
    legacy_evidence="inspect_root_different:$legacy_inspect_root"
  fi
elif [ -n "$legacy_registry_source" ] || [ -n "$legacy_registry_install" ]; then
  # inspect unavailable but registry shows an install record (C or D).
  for rp in "$legacy_registry_source" "$legacy_registry_install"; do
    if [ -n "$rp" ] && paths_match "$rp" "$LEGACY_ADAPTER_DIR"; then
      legacy_ownership="owned"
      legacy_evidence="registry_path_match:$rp"
      break
    elif [ -n "$rp" ] && [ -e "$rp" ]; then
      # D: registry_path_different — STOP HERE. Do NOT fall through to
      # config fallback. An unrelated plugin owns the legacy id.
      legacy_ownership="conflict"
      legacy_conflict_path="$rp"
      legacy_evidence="registry_path_different:$rp"
      break
    fi
  done
fi

# Config-only fallback (E, F). ONLY consulted when neither inspect nor
# registry produced an ownership verdict (i.e. the legacy id has no
# surviving install record at all). If we reach here with
# legacy_ownership still "none", inspect AND registry both failed
# to find the legacy id in OpenClaw's data — the typical real-world
# post-git-mv state. The legacy plugin id `openclaw-mpm-memory` is
# repo-owned; if config keys still reference it, we claim ownership
# unless stronger evidence above contradicted (which cannot happen
# because stronger evidence would have set ownership to owned or
# conflict before reaching this block).
if [ "$legacy_ownership" = "none" ]; then
  if [ "$legacy_slot_val" = "$LEGACY_PLUGIN_ID" ]; then
    legacy_ownership="owned"
    legacy_evidence="slot_points_to_legacy"
  elif [ -n "$legacy_entry_has_value" ]; then
    legacy_ownership="owned"
    legacy_evidence="entry_key_present"
  fi
fi

# Step E: act on the ownership verdict.
case "$legacy_ownership" in
  none)
    : # no migration needed
    ;;
  conflict)
    err "legacy plugin id '$LEGACY_PLUGIN_ID' is already installed but points at a different source:"
    err "  recorded source: $legacy_conflict_path"
    err "  this adapter:    $SCRIPT_DIR"
    err "  expected former canonical location of this adapter:"
    err "    $LEGACY_ADAPTER_DIR"
    err "this installer will not seize an unrelated plugin installation."
    err "operator actions:"
    err "  (a) if the existing '$LEGACY_PLUGIN_ID' is a stale install of THIS adapter from"
    err "      an unrelated copy, uninstall it manually, then re-run $0:"
    err "        openclaw plugins uninstall $LEGACY_PLUGIN_ID"
    err "        $0"
    err "  (b) if the existing '$LEGACY_PLUGIN_ID' is from a completely different source,"
    err "      leave it alone and rename this adapter's id in $SCRIPT_DIR/openclaw.plugin.json"
    exit 1
    ;;
  owned)
    log "legacy plugin id '$LEGACY_PLUGIN_ID' recognised as this adapter (evidence: $legacy_evidence) — migrating to '$PLUGIN_ID'"
    # Migrate each entry key individually. Each migration is wrapped
    # in its own bounded call; failure on one key does not abort the
    # rest, so a stale config with partial legacy state cleans up
    # gracefully on re-run.
    legacy_to_canonical_migrate() {
      local key="$1"
      local val
      val="$(legacy_to_canonical_migrate_get "$key")"
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
    if [ "$legacy_slot_val" = "$LEGACY_PLUGIN_ID" ]; then
      if timeout "${OPENCLAW_CONFIG_TIMEOUT}s" \
          openclaw config set plugins.slots.memory "$PLUGIN_ID" \
            >>"$INSTALL_LOG" 2>&1; then
        log "  migrated plugins.slots.memory: $LEGACY_PLUGIN_ID → $PLUGIN_ID"
      else
        warn "  failed to migrate plugins.slots.memory (operator can re-run)"
      fi
    fi
    # Uninstall the legacy id now that config has been migrated.
    # Best-effort — if the install record has already been swept by
    # `openclaw doctor --fix` (which removes stale load.paths entries)
    # the uninstall may fail with "plugin not found". That is fine:
    # the config is migrated and the legacy id has no remaining
    # config-backed state. We treat non-zero as WARN.
    #
    # Post-uninstall reconciliation: re-probe the legacy id so we can
    # distinguish the three observable outcomes:
    #
    #   legacy_uninstall_rc=0          → uninstall succeeded
    #   legacy_uninstall_rc!=0 + probe ok:false → install record was already
    #                                       gone (doctor --fix swept it; this
    #                                       is the benign pre-existing case)
    #   legacy_uninstall_rc!=0 + probe ok:true  → STALE INSTALL STILL PRESENT;
    #                                       escalate to WARN with a clear
    #                                       operator-facing message naming
    #                                       the remaining cleanup commands.
    #
    # The legacy id being unselected as the memory slot AND having no
    # entry-key references is sufficient for "no competing MPM memory
    # plugin id". A surviving but unselected registry record is a
    # non-selected fossil and is acceptable.
    legacy_uninstall_rc=0
    if timeout "${OPENCLAW_PLUGIN_INSTALL_TIMEOUT}s" \
        openclaw plugins uninstall "$LEGACY_PLUGIN_ID" \
          >>"$INSTALL_LOG" 2>&1; then
      legacy_uninstall_rc=0
      log "  uninstalled legacy plugin id '$LEGACY_PLUGIN_ID'"
    else
      legacy_uninstall_rc=1
      log "  openclaw plugins uninstall returned non-zero for '$LEGACY_PLUGIN_ID'"
    fi
    # Reconciliation probe: does the legacy id still resolve to a live
    # install record? Re-use the inspect path; a non-empty legacy rootDir
    # means the record is still there.
    legacy_post_probe_root="$(extract_json_field "plugin.rootDir" \
      "$(timeout "${OPENCLAW_PLUGIN_INSPECT_TIMEOUT}s" \
        openclaw plugins inspect "$LEGACY_PLUGIN_ID" --json 2>/dev/null || true)")"
    if [ -n "$legacy_post_probe_root" ]; then
      # Stale install record survived. This is the dangerous case —
      # the install record still points at the canonical former path,
      # which no longer exists, so OpenClaw will surface a 'plugin
      # path not found' warning until the operator cleans it up.
      warn "  legacy plugin id '$LEGACY_PLUGIN_ID' still has a live install record at"
      warn "    $legacy_post_probe_root"
      warn "  the legacy id is no longer selected as the memory slot and its config"
      warn "  entries are gone, so it will not compete with the canonical plugin."
      warn "  operator cleanup (one of):"
      warn "    openclaw plugins uninstall $LEGACY_PLUGIN_ID"
      warn "    openclaw doctor --fix"
      warn "  re-run this installer after cleanup."
    else
      if [ "$legacy_uninstall_rc" -ne 0 ]; then
        log "  legacy plugin id '$LEGACY_PLUGIN_ID' has no live install record (install record was already swept; config is migrated)"
      else
        log "  legacy plugin id '$LEGACY_PLUGIN_ID' has no live install record (verified post-uninstall)"
      fi
    fi
    ;;
esac

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
#                                    mpm_memory_search, mpm_memory_get) and they
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
  log "memory-core detected (enabled=$memcore_state). The slot assignment above directs mpm_memory_search to $PLUGIN_ID."
  log "To silence memory-core (recommended for clarity, not required):"
  log "  openclaw config set plugins.entries.memory-core.enabled false"
fi

# --------------------------------------------------------------------------
# 9. Converge OpenClaw state (openclaw update repair)
# --------------------------------------------------------------------------
#
# OpenClaw 2026.9.5 added a startup migration-inputs consistency check
# (readStartupMigrationSnapshot / assertStartupConfigUnchanged) that
# refuses to start the gateway with exit 78 if the config was modified
# too recently before a restart. The error message is "OpenClaw
# migration inputs changed during startup; refusing to report the
# gateway ready." Empirically, the trigger window is the few seconds
# after a `openclaw config set` or `plugins install` write before the
# next gateway start.
#
# The supported convergence primitive is `openclaw update repair`. It
# runs targetConfigConvergence which writes the final plugin/config
# inventory into the migration identity, so the next start sees a
# settled config and the consistency check passes. We run it AFTER all
# plugin/config/slot mutations performed by this installer are complete
# and BEFORE any gateway restart.
#
# Convergence vs. unrelated finalization:
#   * `openclaw update repair` returns exit 0 on full convergence OR
#     convergence-with-warnings. The completion-cache step
#     ("native no-replace move is unavailable on this filesystem" on
#     filesystems that lack atomic no-replace renames) is a
#     finalization-stage warning, NOT a convergence failure. The CLI
#     prints "Update finalization completed with warnings." and exits 0
#     when targetConfigConvergence completed. We treat exit 0 as
#     success and let stdout/stderr diagnostics flow to $INSTALL_LOG.
#   * Non-zero exit means targetConfigConvergence or a load-bearing
#     convergence step failed. We treat that as a real failure: the
#     next gateway start will likely hit the migration-inputs check
#     and exit 78. The installer surfaces this clearly and refuses to
#     claim total success.
#
# `openclaw update repair` deliberately stops the managed gateway
# during its lifecycle, so the post-repair gateway status may report
# stopped. The systemd --user unit restarts the gateway on its own
# once the repair returns; this installer just verifies the result.

log "converging OpenClaw state (openclaw update repair)"
REPAIR_OK=1
REPAIR_RC=0
if timeout "${OPENCLAW_UPDATE_REPAIR_TIMEOUT}s" \
    openclaw update repair \
      >>"$INSTALL_LOG" 2>&1; then
  log "  update repair converged"
else
  REPAIR_OK=0
  REPAIR_RC=$?
  warn "update repair returned non-zero (rc=$REPAIR_RC)."
  warn "  plugin/config state has NOT been fully converged."
  warn "  the next gateway start may fail with status 78/CONFIG"
  warn "  (\"migration inputs changed during startup\")."
  warn "  operator recovery: openclaw update repair && openclaw doctor --fix"
  warn "  see $INSTALL_LOG"
fi

# --------------------------------------------------------------------------
# 10. Bounded gateway lifecycle (status + restart + post-restart verify)
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
# Post-restart, we probe status a second time with a separate bounded
# timeout. This is the load-bearing fix for the 2026.9.5 migration
# refusal: a successful restart --safe that subsequently hits the
# startup check and exits 78 leaves the gateway stopped, and we MUST
# surface that rather than print a misleading "Done.".

# Convert status seconds to ms for the CLI flag (CLI requires integer ms).
status_timeout_ms=$(( OPENCLAW_GATEWAY_STATUS_TIMEOUT * 1000 ))
[ "$status_timeout_ms" -ge 1000 ] || status_timeout_ms=1000

GATEWAY_RESTART_OK=0
GATEWAY_REACHABLE_AFTER_RESTART=0
RESTART_ATTEMPTED=0

# Probe 1: is a gateway reachable right now? If not, the systemd unit
# (or another supervisor) owns lifecycle and we leave it alone.
if timeout "${OPENCLAW_GATEWAY_STATUS_TIMEOUT}s" \
   openclaw gateway status --json --timeout "$status_timeout_ms" \
     >/dev/null 2>&1; then
  if [ "$REPAIR_OK" -ne 1 ]; then
    warn "gateway reachable but update repair did not converge; skipping restart."
    warn "  issuing restart now is likely to fail with exit 78 and leave"
    warn "  the gateway stopped. Operator recovery:"
    warn "    openclaw update repair"
    warn "    systemctl --user restart openclaw-gateway.service"
  else
    RESTART_ATTEMPTED=1
    log "requesting bounded safe gateway restart (timeout ${OPENCLAW_GATEWAY_RESTART_TIMEOUT}s)"
    if timeout "${OPENCLAW_GATEWAY_RESTART_TIMEOUT}s" \
        openclaw gateway restart --safe \
          >>"$INSTALL_LOG" 2>&1; then
      GATEWAY_RESTART_OK=1
    else
      warn "gateway restart command returned non-zero (bounded timeout or refused)."
      warn "  the plugin config above is persisted; the operator can re-run:"
      warn "    openclaw gateway restart --safe"
    fi

    # Probe 2: is the gateway actually running after the restart?
    # The 2026.9.5 migration-inputs check fires AFTER our restart
    # command returns successfully — exit 78 can show up seconds
    # later. We must verify the gateway came back up.
    verify_timeout_ms=$(( OPENCLAW_GATEWAY_VERIFY_TIMEOUT * 1000 ))
    [ "$verify_timeout_ms" -ge 1000 ] || verify_timeout_ms=1000
    if timeout "${OPENCLAW_GATEWAY_VERIFY_TIMEOUT}s" \
       openclaw gateway status --json --timeout "$verify_timeout_ms" \
         >/dev/null 2>&1; then
      GATEWAY_REACHABLE_AFTER_RESTART=1
      log "post-restart gateway reachable"
    else
      warn "post-restart gateway is NOT reachable."
      warn "  this is the 2026.9.5 migration-inputs failure mode."
      warn "  the gateway likely exited 78 during startup."
      warn "  operator recovery:"
      warn "    openclaw update repair"
      warn "    systemctl --user restart openclaw-gateway.service"
    fi
  fi
else
  log "no gateway service detected (status timed out or non-responsive); skipping gateway restart."
  log "  config above is persisted and will apply on the next gateway start."
  log "  if systemd --user owns this gateway it will auto-restart; otherwise"
  log "  start it manually with: openclaw gateway run"
fi

# --------------------------------------------------------------------------
# 11. Verify + final classification
# --------------------------------------------------------------------------

if timeout "${OPENCLAW_PLUGIN_INSPECT_TIMEOUT}s" \
   openclaw plugins inspect "$PLUGIN_ID" --json >/dev/null 2>&1; then
  log "plugin visible to openclaw: $PLUGIN_ID"
else
  warn "openclaw plugins inspect $PLUGIN_ID did not return cleanly; inspect $INSTALL_LOG"
fi

# ---------------------------------------------------------------------------
# Step 10: write the MPM managed block into the active agent's SOUL.md
# ---------------------------------------------------------------------------
#
# Persistent behavioural contract: the OpenClaw agent's SOUL.md (resolved
# per-agent from openclaw.json → agents.entries.<id>.workspace / SOUL.md)
# receives the canonical MPM managed block via the per-host renderer.
# The renderer output lives at templates/SOUL.md.snippet (regenerated by
# scripts/render_managed_blocks.py on every canonical change).
#
# The installer is idempotent: a fresh SOUL.md receives the snippet as-is;
# an existing SOUL.md has only the bracketed managed section refreshed —
# persona and user content outside the markers is preserved verbatim.
# Persona is the agent's identity material (808 Core Truths template on
# fresh installs, persona config on customised installs); it is NEVER
# modified by this step. A backup is written before any modification.

if [ ! -f "$SCRIPT_DIR/templates/SOUL.md.snippet" ]; then
  warn "templates/SOUL.md.snippet missing at $SCRIPT_DIR; skipping SOUL.md managed-block install"
  warn "  regenerate via: python3 scripts/render_managed_blocks.py"
  SOUL_MD_OK=0
elif ! command -v python3 >/dev/null 2>&1; then
  warn "python3 not available — SOUL.md managed-block install skipped"
  SOUL_MD_OK=0
else
  if python3 "$SCRIPT_DIR/scripts/install_openclaw_instructions.py" \
        --home "$HOME_DIR" \
        --agent-id main \
        --snippet "$SCRIPT_DIR/templates/SOUL.md.snippet" \
        >>"$INSTALL_LOG" 2>&1; then
    log "SOUL.md managed block installed/refreshed"
    SOUL_MD_OK=1
  else
    warn "SOUL.md managed block install failed (rc=$?); inspect $INSTALL_LOG"
    SOUL_MD_OK=0
  fi
fi

# Final classification. Failure conditions:
#   (A) update repair did NOT converge — we know the next start will
#       hit exit 78 and leave the gateway stopped.
#   (B) we attempted a restart (gateway was reachable) AND the gateway
#       is NOT reachable afterward. This catches the 2026.9.5 case
#       where the restart --safe command returned but the new gateway
#       process exited 78 during startup, AND the case where the
#       restart command itself failed and the gateway stayed down.
#   (C) SOUL.md managed-block install failed. The installer promises
#       the agent's SOUL.md carries the canonical MPM behavioural
#       block; an install failure means that promise is unmet, even
#       if the gateway itself is reachable.
# Anything else (gateway unreachable from the start, OR restart
# succeeded but gateway remained reachable, OR gateway stayed up
# despite a restart failure) is installer success.
INSTALL_FAILED=0
if [ "$REPAIR_OK" -ne 1 ]; then
  INSTALL_FAILED=1
elif [ "$RESTART_ATTEMPTED" -eq 1 ] && [ "$GATEWAY_REACHABLE_AFTER_RESTART" -ne 1 ]; then
  INSTALL_FAILED=1
elif [ "$SOUL_MD_OK" -ne 1 ]; then
  INSTALL_FAILED=1
fi

if [ "$INSTALL_FAILED" -ne 0 ]; then
  cat >&2 <<NEXT
[mpm-memory-openclaw install] FAILED.

The plugin config and slot switch above are persisted, but OpenClaw did
not settle into a healthy state. Do NOT treat this install as successful.

Operator recovery:
  openclaw update repair
  systemctl --user restart openclaw-gateway.service
  openclaw gateway status --deep

Inspect $INSTALL_LOG for captured openclaw CLI output.
NEXT
  exit 1
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
