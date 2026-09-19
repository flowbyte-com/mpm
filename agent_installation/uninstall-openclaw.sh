#!/usr/bin/env bash
#
# uninstall-openclaw.sh — Remove MPM's OpenClaw integration safely.
#
# OWNERSHIP MODEL
# ===============
#
# This uninstaller owns ONLY state that is clearly attributable to:
#
#   Canonical plugin ids:
#     mpm-memory-openclaw
#     mpm-auto-mode-persona-openclaw
#
#   Legacy plugin ids (pre-2026-09-17 namespace migration; safe to
#   remove if present because the canonical adapters took over the
#   namespace and no other plugin should be using these ids):
#     openclaw-mpm-memory
#     openclaw-mpm-auto-mode-persona
#
# It explicitly does NOT touch:
#
#   - OpenClaw itself (the CLI binary, gateway service, workspace,
#     logs, completions, etc.)
#   - The MPM substrate (anything under ~/.mpm — binaries, DB,
#     config, scheduler, wakes, memories, theories, directives,
#     mode/persona state)
#   - ~/.local/bin/mpm or any mpm symlink
#   - Unrelated OpenClaw plugins (any plugin id NOT in the four
#     owned ids above)
#   - Unrelated openclaw.json config keys
#   - Unrelated memory-core / other memory plugin entries
#   - User workspace data
#   - Shell startup files (.bashrc, .zshrc, .profile, ...)
#
# If ownership is ambiguous (e.g. a plugin id is registered but its
# recorded rootDir points at a path that is not one of ours), the
# uninstaller WARNS and SKIPS — it never blindly seizes another
# installation.
#
# USE
# ===
#
#     ./uninstall-openclaw.sh                  # interactive (confirm)
#     ./uninstall-openclaw.sh --yes            # skip confirmation
#     ./uninstall-openclaw.sh --dry-run        # show plan only
#     ./uninstall-openclaw.sh --help
#
# After completion, OpenClaw remains installed and the MPM substrate
# is untouched. Only the OpenClaw-side MPM adapter state is removed.
#
# CWD-INDEPENDENT: the canonical adapter paths are resolved from
# BASH_SOURCE[0] (this script's own location) so the operator can run
# it from any working directory.
#
# OpenClaw 2026.9.5 CLI contract (verified on this host):
#   `openclaw plugins uninstall <id>` removes plugin settings, the
#   install record, the load path, and (for memory plugins) resets
#   `plugins.slots.memory` to the default ("memory-core"). The
#   `--dry-run` form shows the plan without mutation.
#
# Idempotency: re-running on a clean host is a no-op. Each plugin id
# is probed first; missing plugins log "not installed" and the script
# proceeds to the next.
#
# Capability / policy cleanup: openclaw stores capability grants
# internally to its registry. They are removed as a side-effect of
# `plugins uninstall` for the owning plugin id. We do NOT attempt to
# scan or remove capability grants by hand — the CLI owns that
# lifecycle.

set -euo pipefail

# --------------------------------------------------------------------------
# Identity + paths
# --------------------------------------------------------------------------

SCRIPT_PATH="${BASH_SOURCE[0]}"
# Resolve symlinks so ./uninstall-openclaw.sh that points elsewhere
# still works.
while [ -L "$SCRIPT_PATH" ]; do
  LINK_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"
  SCRIPT_PATH="$(readlink "$SCRIPT_PATH")"
  case "$SCRIPT_PATH" in
    /*) ;;
    *)  SCRIPT_PATH="$LINK_DIR/$SCRIPT_PATH" ;;
  esac
done
SCRIPT_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"

# Canonical adapter directories. We read the plugin id from each
# adapter's openclaw.plugin.json (not hard-coded) so renames stay
# in sync with the manifest.
ADAPTER_MEMORY_DIR="$SCRIPT_DIR/mpm-memory-openclaw"
ADAPTER_AUTO_DIR="$SCRIPT_DIR/mpm-auto-mode-persona-openclaw"

read_plugin_id_from_manifest() {
  local manifest="$1"
  if [ ! -f "$manifest" ]; then
    return 1
  fi
  grep -oE '"id"[[:space:]]*:[[:space:]]*"[^"]+"' "$manifest" \
    | head -n1 | sed -E 's/.*"id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/'
}

CANONICAL_ID_MEMORY="$(read_plugin_id_from_manifest \
  "$ADAPTER_MEMORY_DIR/openclaw.plugin.json" || true)"
CANONICAL_ID_AUTO="$(read_plugin_id_from_manifest \
  "$ADAPTER_AUTO_DIR/openclaw.plugin.json" || true)"

# Legacy ids derived from the canonical former adapter directory
# names (pre-2026-09-17 namespace migration). We compute these from
# the sibling-of-adapter naming convention: legacy plugin id
# `<openclaw>-<adapter>` was the reverse of the canonical
# `<adapter>-<openclaw>`.
LEGACY_ID_MEMORY="openclaw-mpm-memory"
LEGACY_ID_AUTO="openclaw-mpm-auto-mode-persona"

# Historical (canonical former) adapter source paths. The legacy
# adapter directories lived at $SCRIPT_DIR/openclaw-mpm-memory and
# $SCRIPT_DIR/openclaw-mpm-auto-mode-persona — siblings of the
# canonical adapters, since the pre-2026-09-17 namespace migration
# git-mv'd them to the current mpm-<adapter>-openclaw names. The
# install.sh's LEGACY_ADAPTER_DIR uses the same derivation:
# $(dirname "$CANONICAL_ADAPTER_DIR")/<legacy-dir-name>.
# (Resolved later, after real_adapter_dir is defined.)
LEGACY_ADAPTER_DIR_MEMORY="$SCRIPT_DIR/openclaw-mpm-memory"
LEGACY_ADAPTER_DIR_AUTO="$SCRIPT_DIR/openclaw-mpm-auto-mode-persona"

# CLI timeout budgets. Each openclaw command is bounded by an outer
# `timeout` wrapper; we don't rely on the CLI's own --timeout for
# every command because not every subcommand supports it.
UNINSTALL_TIMEOUT="${OPENCLAW_PLUGIN_UNINSTALL_TIMEOUT:-30}"
INSPECT_TIMEOUT="${OPENCLAW_PLUGIN_INSPECT_TIMEOUT:-10}"
CONFIG_TIMEOUT="${OPENCLAW_CONFIG_TIMEOUT:-10}"
GATEWAY_STATUS_TIMEOUT="${OPENCLAW_GATEWAY_STATUS_TIMEOUT:-10}"
GATEWAY_RESTART_TIMEOUT="${OPENCLAW_GATEWAY_RESTART_TIMEOUT:-20}"

LOG_FILE="${UNINSTALL_LOG:-/tmp/mpm-openclaw-uninstall.log}"
: > "$LOG_FILE" 2>/dev/null || true

log()  { printf '[mpm-openclaw uninstall] %s\n' "$*" >&2; }
warn() { printf '[mpm-openclaw uninstall] WARN: %s\n' "$*" >&2; }
err()  { printf '[mpm-openclaw uninstall] ERROR: %s\n' "$*" >&2; }

# --------------------------------------------------------------------------
# CLI parsing
# --------------------------------------------------------------------------

DRY_RUN=0
ASSUME_YES=0
SHOW_HELP=0

usage() {
  cat <<USAGE
Usage: uninstall-openclaw.sh [options]

Remove MPM's OpenClaw integration cleanly. OpenClaw and the MPM
substrate are NOT touched.

Options:
  --dry-run     Show the plan without making any changes.
  --yes         Skip the confirmation prompt.
  -h, --help    Show this help and exit.

What this removes:
  - Plugin registrations: mpm-memory-openclaw, mpm-auto-mode-persona-openclaw
  - Legacy registrations (when present and clearly ours):
      openclaw-mpm-memory, openclaw-mpm-auto-mode-persona
  - Per-plugin config under plugins.entries.<id>
  - Plugin load paths under plugins.load.paths
  - Memory slot reset (only when slot pointed at one of our plugins)

What this deliberately preserves:
  - OpenClaw itself (binary, service, workspace, logs)
  - The MPM substrate (~/.mpm, ~/.local/bin/mpm, mpm scheduler state)
  - Any plugin id that points at an unrelated source (conflict)
  - Unrelated openclaw.json config keys
  - Shell startup files, user data, memory-core and other plugins

USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --yes|-y)  ASSUME_YES=1; shift ;;
    -h|--help) SHOW_HELP=1; shift ;;
    *)
      err "unknown option: $1"
      usage >&2
      exit 2
      ;;
  esac
done

if [ "$SHOW_HELP" = "1" ]; then
  usage
  exit 0
fi

# --------------------------------------------------------------------------
# Plan: probe every owned plugin id
# --------------------------------------------------------------------------
#
# A "plan row" is one owned plugin id with the verdict about its
# current state:
#
#   absent              — id not in registry; nothing to do.
#   owned               — id registered, rootDir resolves to OUR
#                         adapter path. Safe to uninstall.
#   conflict                — id registered but rootDir points at a
#                         different path. Refuse; don't seize.
#   unresolvable        — id registration exists but rootDir does
#                         not resolve and is not our path. Skip
#                         with a warning.

# Compare two filesystem paths even when one doesn't exist. We
# compare the readlink -f resolved form (canonicalizes hardlinks)
# and fall back to the literal form when readlink -f fails.
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

# Resolve the real path of an adapter directory (handles hardlinks).
real_adapter_dir() {
  local d="$1"
  if [ -z "$d" ]; then
    return 1
  fi
  readlink -f "$d" 2>/dev/null || printf '%s' "$d"
}

OUR_REAL_MEMORY="$(real_adapter_dir "$ADAPTER_MEMORY_DIR" || true)"
OUR_REAL_AUTO="$(real_adapter_dir "$ADAPTER_AUTO_DIR" || true)"
# Now resolve the historical (legacy) adapter source paths using the
# same realpath comparison as for canonical adapters.
LEGACY_REAL_MEMORY="$(real_adapter_dir "$LEGACY_ADAPTER_DIR_MEMORY" || true)"
LEGACY_REAL_AUTO="$(real_adapter_dir "$LEGACY_ADAPTER_DIR_AUTO" || true)"

# JSON field extractor (mirrors install.sh). Returns empty if the
# JSON is missing the field or python3 isn't available.
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

# Probe a plugin id. Outputs two lines (verdict + root_dir).
# Always outputs BOTH lines so callers can parse them uniformly.
# Verdict values:
#   absent              — id not in registry (no plugin object).
#   owned               — id registered, rootDir resolves to OUR
#                         adapter path. Safe to uninstall.
#   conflict            — id registered but rootDir points at a
#                         different path. Refuse; don't seize.
#   unresolvable        — id registration exists but rootDir does
#                         not resolve and is not our path.
#
# Detection: the 2026.9.5 inspect shape is:
#   - For a missing plugin: top-level { ok: false, error: {...} }
#     with NO `plugin` field.
#   - For an installed plugin: top-level has a `plugin` field with
#     id/rootDir/etc. There is no top-level `ok` field (or it is
#     null). We therefore probe the `plugin` object's presence
#     instead of the `ok` field.
probe_plugin() {
  local plugin_id="$1"
  local expected_real="$2"  # adapter's real path; "" means any path is allowed
  local inspect_out
  inspect_out="$(timeout "${INSPECT_TIMEOUT}s" \
    openclaw plugins inspect "$plugin_id" --json 2>/dev/null || true)"
  local ok root_dir verdict
  verdict=""
  root_dir=""
  ok="$(extract_json_field "ok" "$inspect_out")"
  # Python's print(False) outputs "False" (capital F); we accept both
  # spellings to be defensive against the JSON boolean representation
  # change in the underlying python helper.
  case "$ok" in
    false|False|0) verdict="absent" ;;
    *) ;;
  esac
  if [ "$verdict" = "absent" ]; then
    : # already set above
  else
    root_dir="$(extract_json_field "plugin.rootDir" "$inspect_out")"
    if [ -z "$root_dir" ]; then
      verdict="unresolvable"
    elif [ -n "$expected_real" ] && paths_match "$root_dir" "$expected_real"; then
      verdict="owned"
    else
      verdict="conflict"
    fi
  fi
  printf '%s\n%s\n' "$verdict" "$root_dir"
}

# Probe a legacy plugin id with strict ownership precedence.
#
# The legacy ids (openclaw-mpm-memory, openclaw-mpm-auto-mode-persona)
# are repo-owned. We must NOT delete an unrelated plugin that happens
# to claim one of those ids; we require actual ownership evidence.
#
# Outputs three lines: <verdict>\n<root_or_evidence>\n<evidence_kind>.
#
# Verdict values (mirrors install.sh's precedence):
#
#   absent    — id not in registry.
#   owned     — strong source evidence OR (no source evidence but a
#               weak fallback evidence of MPM ownership).
#   conflict  — id registered at an UNRELATED existing source path;
#               refuse to delete.
#   ambiguous — id absent OR (no source metadata AND no weak
#               fallback evidence); do not delete by name alone.
#
# Evidence precedence:
#   A. inspect rootDir matches legacy adapter path   → owned
#   B. inspect rootDir resolves to a DIFFERENT existing path
#      → conflict (do NOT fall through to fallback)
#   C. registry installRecords[id].sourcePath /
#      installPath matches legacy adapter path       → owned
#   D. registry sourcePath / installPath resolves to
#      a DIFFERENT existing path                     → conflict
#   E. (no source verdict) AND plugins.slots.memory = legacy-id
#      → owned (weak fallback)
#   F. (no source verdict) AND plugins.entries.<id>.*
#      has any non-empty value                       → owned (weak fallback)
#   G. (no source verdict) AND no fallback evidence  → ambiguous
#
# Cases B and D always win over E and F. The fallback chain (E, F)
# only runs when neither A–D produced a verdict.
probe_legacy_plugin() {
  local plugin_id="$1"
  local expected_legacy_real="$2"
  local inspect_out registry_out
  local ok root_dir verdict evidence_path
  verdict="ambiguous"
  evidence_path=""
  local_evidence_kind=""

  # Step 1: inspect (strong evidence: A or B).
  inspect_out="$(timeout "${INSPECT_TIMEOUT}s" \
    openclaw plugins inspect "$plugin_id" --json 2>/dev/null || true)"
  ok="$(extract_json_field "ok" "$inspect_out")"
  case "$ok" in
    true|True|1)
      root_dir="$(extract_json_field "plugin.rootDir" "$inspect_out")"
      if [ -n "$root_dir" ] && paths_match "$root_dir" "$expected_legacy_real"; then
        verdict="owned"
        evidence_path="$root_dir"
        local_evidence_kind="inspect_root_match"
      elif [ -n "$root_dir" ] && [ -e "$root_dir" ]; then
        verdict="conflict"
        evidence_path="$root_dir"
        local_evidence_kind="inspect_root_different"
      fi
      ;;
    false|False|0|"")
      : # inspect unavailable or missing; fall through to registry
      ;;
  esac

  # Step 2: registry (strong evidence: C or D), only if inspect was
  # not definitive.
  if [ "$verdict" = "ambiguous" ]; then
    registry_out="$(timeout "${INSPECT_TIMEOUT}s" \
      openclaw plugins registry --json 2>/dev/null || true)"
    local rp_source rp_install rp
    rp_source="$(extract_json_field \
      "persisted.installRecords.${plugin_id}.sourcePath" "$registry_out")"
    rp_install="$(extract_json_field \
      "persisted.installRecords.${plugin_id}.installPath" "$registry_out")"
    for rp in "$rp_source" "$rp_install"; do
      if [ -z "$rp" ]; then continue; fi
      if paths_match "$rp" "$expected_legacy_real"; then
        verdict="owned"
        evidence_path="$rp"
        local_evidence_kind="registry_path_match"
        break
      elif [ -e "$rp" ]; then
        verdict="conflict"
        evidence_path="$rp"
        local_evidence_kind="registry_path_different"
        break
      fi
    done
  fi

  # Step 3: weak fallback (E or F), only when no source verdict.
  # We only enter here when verdict is still "ambiguous" (no
  # inspect OR registry source verdict). Cases B and D are already
  # handled above; we cannot reach here with verdict="conflict".
  if [ "$verdict" = "ambiguous" ]; then
    local slot_val
    slot_val="$(timeout "${CONFIG_TIMEOUT}s" \
      openclaw config get plugins.slots.memory 2>/dev/null || true)"
    if [ "$slot_val" = "$plugin_id" ]; then
      verdict="owned"
      evidence_path="<slot_fallback>"
      local_evidence_kind="slot_points_to_legacy"
    fi
  fi
  if [ "$verdict" = "ambiguous" ]; then
    local ev_mpmBin ev_enabled
    ev_mpmBin="$(timeout "${CONFIG_TIMEOUT}s" \
      openclaw config get "plugins.entries.${plugin_id}.config.mpmBin" \
        2>/dev/null || true)"
    ev_enabled="$(timeout "${CONFIG_TIMEOUT}s" \
      openclaw config get "plugins.entries.${plugin_id}.enabled" \
        2>/dev/null || true)"
    case "$ev_mpmBin$ev_enabled" in
      *null*) ;;
      *)
        if [ -n "$ev_mpmBin" ] || [ -n "$ev_enabled" ]; then
          verdict="owned"
          evidence_path="<config_fallback>"
          local_evidence_kind="entry_key_present"
        fi
        ;;
    esac
  fi

  printf '%s\n%s\n%s\n' "$verdict" "$evidence_path" "$local_evidence_kind"
}

# Probe the memory slot ownership.
probe_slot() {
  local slot_val
  slot_val="$(timeout "${CONFIG_TIMEOUT}s" \
    openclaw config get plugins.slots.memory 2>/dev/null || true)"
  case "$slot_val" in
    "$CANONICAL_ID_MEMORY"|"$LEGACY_ID_MEMORY") printf '%s' "$slot_val" ;;
    "") printf '' ;;
    *)  printf '%s' "$slot_val" ;;
  esac
}

# --------------------------------------------------------------------------
# Pre-flight checks
# --------------------------------------------------------------------------

if ! command -v openclaw >/dev/null 2>&1; then
  err "openclaw CLI not on PATH. Install OpenClaw first."
  err "This uninstaller cannot operate without the openclaw binary."
  exit 1
fi

# Build the plan.
log "probing MPM OpenClaw integration state"
PLAN_ROWS=""
if [ -n "$CANONICAL_ID_MEMORY" ]; then
  PR_OUT="$(probe_plugin "$CANONICAL_ID_MEMORY" "$OUR_REAL_MEMORY")"
  PR_VERDICT="$(printf '%s\n' "$PR_OUT" | sed -n '1p')"
  PR_ROOT="$(printf '%s\n' "$PR_OUT" | sed -n '2p')"
  PLAN_ROWS="${PLAN_ROWS}CANONICAL|mpm-memory-openclaw|$CANONICAL_ID_MEMORY|$PR_VERDICT|$PR_ROOT
"
fi
if [ -n "$CANONICAL_ID_AUTO" ]; then
  PR_OUT="$(probe_plugin "$CANONICAL_ID_AUTO" "$OUR_REAL_AUTO")"
  PR_VERDICT="$(printf '%s\n' "$PR_OUT" | sed -n '1p')"
  PR_ROOT="$(printf '%s\n' "$PR_OUT" | sed -n '2p')"
  PLAN_ROWS="${PLAN_ROWS}CANONICAL|mpm-auto-mode-persona-openclaw|$CANONICAL_ID_AUTO|$PR_VERDICT|$PR_ROOT
"
fi
# Legacy ids: use the strict ownership precedence (probe_legacy_plugin)
# that mirrors install.sh. We require actual ownership evidence:
#   A. inspect rootDir matches the historical legacy adapter path; OR
#   B. registry install record matches the historical legacy adapter
#      path; OR
#   C. weak fallback: legacy slot or legacy entries have non-empty
#      values (only when no source verdict exists).
# Anything else (conflict or ambiguous) is reported and NOT removed
# by name alone. We must NOT seize an unrelated plugin that happens
# to claim one of these legacy ids.
PR_OUT="$(probe_legacy_plugin "$LEGACY_ID_MEMORY" "$LEGACY_REAL_MEMORY")"
PR_VERDICT="$(printf '%s\n' "$PR_OUT" | sed -n '1p')"
PR_ROOT="$(printf '%s\n' "$PR_OUT" | sed -n '2p')"
PR_EVIDENCE="$(printf '%s\n' "$PR_OUT" | sed -n '3p')"
PLAN_ROWS="${PLAN_ROWS}LEGACY|mpm-memory-openclaw|$LEGACY_ID_MEMORY|$PR_VERDICT|$PR_ROOT|$PR_EVIDENCE
"
PR_OUT="$(probe_legacy_plugin "$LEGACY_ID_AUTO" "$LEGACY_REAL_AUTO")"
PR_VERDICT="$(printf '%s\n' "$PR_OUT" | sed -n '1p')"
PR_ROOT="$(printf '%s\n' "$PR_OUT" | sed -n '2p')"
PR_EVIDENCE="$(printf '%s\n' "$PR_OUT" | sed -n '3p')"
PLAN_ROWS="${PLAN_ROWS}LEGACY|mpm-auto-mode-persona-openclaw|$LEGACY_ID_AUTO|$PR_VERDICT|$PR_ROOT|$PR_EVIDENCE
"

CURRENT_SLOT="$(probe_slot)"

# Render the plan in human-readable form.
log "plan:"
printf '%s\n' "$PLAN_ROWS" | while IFS='|' read -r ROW_KIND ROW_ADAPTER ROW_ID ROW_VERDICT ROW_ROOT ROW_EVIDENCE; do
  [ -z "$ROW_VERDICT" ] && continue
  case "$ROW_VERDICT" in
    absent)
      printf '  [skip]    %-25s %-35s not installed\n' "$ROW_ADAPTER" "$ROW_ID" >&2
      ;;
    owned)
      printf '  [uninst]  %-25s %-35s will be uninstalled (rootDir=%s)\n' \
        "$ROW_ADAPTER" "$ROW_ID" "$ROW_ROOT" >&2
      ;;
    conflict)
      printf '  [refuse]  %-25s %-35s registered at UNRELATED rootDir=%s (skipping)\n' \
        "$ROW_ADAPTER" "$ROW_ID" "$ROW_ROOT" >&2
      ;;
    unresolvable)
      printf '  [skip]    %-25s %-35s registered but rootDir unresolvable\n' \
        "$ROW_ADAPTER" "$ROW_ID" >&2
      ;;
    ambiguous)
      printf '  [ambig]   %-25s %-35s no ownership evidence (will not remove by name alone)\n' \
        "$ROW_ADAPTER" "$ROW_ID" >&2
      ;;
    *)
      printf '  [skip]    %-25s %-35s unknown verdict (%s)\n' \
        "$ROW_ADAPTER" "$ROW_ID" "$ROW_VERDICT" >&2
      ;;
  esac
done

# Slot handling summary.
case "$CURRENT_SLOT" in
  "")
    log "memory slot is unset; nothing to clean" >&2
    ;;
  "$CANONICAL_ID_MEMORY"|"$LEGACY_ID_MEMORY")
    log "memory slot = $CURRENT_SLOT (MPM-owned); will be reset to default ('memory-core')" >&2
    ;;
  *)
    log "memory slot = $CURRENT_SLOT (NOT MPM-owned); leaving untouched" >&2
    ;;
esac

if [ "$DRY_RUN" = "1" ]; then
  log "dry-run complete; no changes made"
  exit 0
fi

# --------------------------------------------------------------------------
# Confirmation
# --------------------------------------------------------------------------

if [ "$ASSUME_YES" != "1" ]; then
  printf '%s' "Proceed with uninstall? [y/N] " >&2
  read -r REPLY
  case "$REPLY" in
    y|Y|yes|YES) ;;
    *)
      log "aborted by user"
      exit 0
      ;;
  esac
fi

# --------------------------------------------------------------------------
# Execution
# --------------------------------------------------------------------------

CHANGED=0

uninstall_one() {
  local row="$1"
  local row_kind row_id row_verdict row_root row_evidence
  IFS='|' read -r row_kind row_adapter row_id row_verdict row_root row_evidence <<EOF
$row
EOF
  case "$row_verdict" in
    owned)
      log "uninstalling $row_id (rootDir=$row_root)"
      # `openclaw plugins uninstall` is interactive by default in
      # 2026.9.5; --force skips the confirmation prompt. The
      # uninstaller has already verified ownership before this
      # call, so --force is safe here.
      if timeout "${UNINSTALL_TIMEOUT}s" \
          openclaw plugins uninstall --force "$row_id" \
            >>"$LOG_FILE" 2>&1; then
        CHANGED=1
        log "  $row_id uninstalled"
      else
        warn "  failed to uninstall $row_id (operator can re-run; see $LOG_FILE)"
      fi
      # `plugins uninstall` removes the install record and load
      # path but, in 2026.9.5, leaves the user's
      # `plugins.entries.<id>` subtree (with `enabled:false`) in
      # openclaw.json. The CLI also reports a config warning
      # ("plugin disabled ... but config is present") until we
      # clear those subtrees. We do that explicitly.
      if timeout "${CONFIG_TIMEOUT}s" \
          openclaw config unset "plugins.entries.$row_id" \
            >>"$LOG_FILE" 2>&1; then
        log "  cleared plugins.entries.$row_id residue"
      fi
      # Legacy id also has config residue under the legacy key.
      case "$row_id" in
        mpm-memory-openclaw|openclaw-mpm-memory)
          timeout "${CONFIG_TIMEOUT}s" \
            openclaw config unset "plugins.entries.openclaw-mpm-memory" \
              >>"$LOG_FILE" 2>&1 || true
          ;;
        mpm-auto-mode-persona-openclaw|openclaw-mpm-auto-mode-persona)
          timeout "${CONFIG_TIMEOUT}s" \
            openclaw config unset "plugins.entries.openclaw-mpm-auto-mode-persona" \
              >>"$LOG_FILE" 2>&1 || true
          ;;
      esac
      ;;
    conflict)
      warn "refusing to uninstall $row_id — registered at unrelated rootDir=$row_root"
      warn "  this plugin is owned by a different source; manual operator action required"
      ;;
    ambiguous)
      warn "not uninstalling $row_id — no ownership evidence (state=$row_verdict, evidence=$row_evidence)"
      warn "  ownership cannot be proven for this id; manual operator action required"
      ;;
    absent|unresolvable|"")
      log "  $row_id: nothing to do (state=$row_verdict)"
      # Even when the plugin was already absent, the
      # plugins.entries.<id> residue may still be present in
      # openclaw.json from a previous install. Clear it for the
      # owned canonical + legacy ids to keep the baseline clean.
      case "$row_id" in
        mpm-memory-openclaw)
          timeout "${CONFIG_TIMEOUT}s" \
            openclaw config unset "plugins.entries.$row_id" \
              >>"$LOG_FILE" 2>&1 || true
          timeout "${CONFIG_TIMEOUT}s" \
            openclaw config unset "plugins.entries.openclaw-mpm-memory" \
              >>"$LOG_FILE" 2>&1 || true
          ;;
        mpm-auto-mode-persona-openclaw)
          timeout "${CONFIG_TIMEOUT}s" \
            openclaw config unset "plugins.entries.$row_id" \
              >>"$LOG_FILE" 2>&1 || true
          timeout "${CONFIG_TIMEOUT}s" \
            openclaw config unset "plugins.entries.openclaw-mpm-auto-mode-persona" \
              >>"$LOG_FILE" 2>&1 || true
          ;;
      esac
      ;;
  esac
}

while IFS= read -r ROW; do
  [ -z "$ROW" ] && continue
  uninstall_one "$ROW"
done <<EOF
$PLAN_ROWS
EOF

# Slot cleanup (defensive; openclaw plugins uninstall of the memory
# plugin should have already reset the slot, but if for any reason
# it didn't, we make the slot explicit so the post-state matches the
# documented expectation).
SLOT_NOW="$(probe_slot)"
case "$SLOT_NOW" in
  "$CANONICAL_ID_MEMORY"|"$LEGACY_ID_MEMORY")
    log "memory slot still = $SLOT_NOW after uninstall; resetting to default 'memory-core'"
    if timeout "${CONFIG_TIMEOUT}s" \
        openclaw config set plugins.slots.memory "memory-core" \
          >>"$LOG_FILE" 2>&1; then
      CHANGED=1
      log "  plugins.slots.memory reset to 'memory-core'"
    else
      warn "  failed to reset plugins.slots.memory (operator can run:"
      warn "    openclaw config set plugins.slots.memory memory-core)"
    fi
    ;;
  "")
    # Slot already unset — nothing to do.
    ;;
  *)
    log "memory slot = $SLOT_NOW (not MPM-owned); leaving untouched"
    ;;
esac

# --------------------------------------------------------------------------
# Post-state verification + bounded gateway restart
# --------------------------------------------------------------------------

log "verifying post-state"
POST_VERIFIED=1
for ROW in $(printf '%s\n' "$PLAN_ROWS"); do
  [ -z "$ROW" ] && continue
  ROW_ID="$(printf '%s' "$ROW" | awk -F'|' '{print $3}')"
  POST_STATE="$(probe_plugin "$ROW_ID" "$(real_adapter_dir "$(printf '%s' "$ROW" | awk -F'|' '{print $2}')")" || true)"
  POST_VERDICT="${POST_STATE%%$'\n'*}"
  case "$POST_VERDICT" in
    absent)
      log "  verified: $ROW_ID is gone"
      ;;
    conflict)
      warn "  $ROW_ID still registered at an unrelated rootDir (we did not touch it)"
      ;;
    owned)
      warn "  $ROW_ID still registered as ours (uninstall reported success but the record survived)"
      warn "  operator can re-run or run:"
      warn "    openclaw plugins uninstall $ROW_ID"
      ;;
    unresolvable)
      warn "  $ROW_ID registered but rootDir unresolvable (cannot verify)"
      ;;
  esac
done

# Gateway restart — only if anything actually changed AND the
# gateway is reachable. We do NOT start the gateway if it was not
# already running.
if [ "$CHANGED" = "1" ]; then
  status_timeout_ms=$(( GATEWAY_STATUS_TIMEOUT * 1000 ))
  [ "$status_timeout_ms" -ge 1000 ] || status_timeout_ms=1000
  if timeout "${GATEWAY_STATUS_TIMEOUT}s" \
     openclaw gateway status --json --timeout "$status_timeout_ms" \
       >/dev/null 2>&1; then
    log "requesting bounded safe gateway restart (timeout ${GATEWAY_RESTART_TIMEOUT}s)"
    if timeout "${GATEWAY_RESTART_TIMEOUT}s" \
        openclaw gateway restart --safe \
          >>"$LOG_FILE" 2>&1; then
      log "  gateway restart ok"
    else
      warn "  gateway restart hit the bounded timeout or returned non-zero."
      warn "  config is already persisted; operator can run:"
      warn "    openclaw gateway restart --safe"
    fi
  else
    log "no gateway service detected; config will apply on next gateway start"
  fi
else
  log "no changes; skipping gateway restart"
fi

# --------------------------------------------------------------------------
# MPM substrate sanity check (proof of preservation)
# --------------------------------------------------------------------------

if [ -x /home/v/.mpm/bin/mpm ]; then
  if /home/v/.mpm/bin/mpm --version >/dev/null 2>&1; then
    log "MPM substrate preserved: $(/home/v/.mpm/bin/mpm --version 2>&1 | head -n1)"
  else
    warn "MPM binary at /home/v/.mpm/bin/mpm is present but does not execute cleanly"
  fi
else
  warn "/home/v/.mpm/bin/mpm not found — this uninstaller never modified the substrate,"
  warn "  but if you expected MPM to be installed, investigate before continuing."
fi

cat >&2 <<NEXT
[mpm-openclaw uninstall] Done.

OpenClaw remains installed. The MPM substrate (~/.mpm) was not touched.
If you want to re-add the MPM OpenClaw integration, run:

  $ADAPTER_MEMORY_DIR/install.sh
  $ADAPTER_AUTO_DIR/install.sh

Verification:
  openclaw plugins list --json        # neither MPM plugin id should appear
  openclaw plugins registry --json     # neither MPM install record should appear
  mpm --version                        # substrate is untouched
NEXT