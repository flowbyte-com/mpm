#!/usr/bin/env bash
#
# scripts/install.sh — MPM user-space install (alpha baseline)
#
# Provisions the MPM cognitive substrate in a single user's context,
# no root required. This is the alpha baseline install path; the
# legacy /var/lib/mpm + system systemd install is reachable via
# `MPM_SYSTEM=1 sudo ./scripts/install.sh` for operators who genuinely
# want shared system state.
#
# What this script does:
#   1. Builds binaries (mpm, mpm-mcp, mpm-scheduler, mpm-critic)
#   2. Installs binaries to $HOME/.mpm/bin/ (canonical — same root as data)
#   3. Writes a workspace-setting wrapper at $HOME/.mpm/bin/mpm
#   4. Creates $HOME/.mpm/ as the runtime data root (0700/0600 enforced
#      by the binary's startup gate)
#   5. Installs and enables the USER-level systemd service at
#      $HOME/.config/systemd/user/mpm-scheduler.service
#   6. Enables systemd user lingering (loginctl enable-linger) so the
#      scheduler survives logout
#   7. Tears down any LEGACY /etc/systemd/system/mpm-scheduler.service
#      that may still be polling /var/lib/mpm — prevents split-brain
#      dual-scheduler scenario for upgrading alpha testers
#   8. Warns loudly if legacy data exists at /var/lib/mpm/mpm.db —
#      operator must migrate manually if they want to keep it
#   9. Registers the MCP server with OpenClaw if present
#  10. Validates end-to-end
#
# Multi-tenant / multi-user safety:
#   - No root required for the default flow; everything lives in $HOME
#   - Runtime data perms are 0700/0600 (enforced by the binary at startup,
#     not by this script)
#   - No global state mutations outside $HOME
#   - Single canonical install location per user ($HOME/.mpm) — no PATH
#     ordering, no /usr/local copies, no XDG split, no drift between
#     shells
#
# Usage:
#   ./scripts/install.sh              # full user-space install (no sudo)
#   ./scripts/install.sh --check      # preflight only (no changes)
#   ./scripts/install.sh --dry-run    # print intended actions
#   ./scripts/install.sh --validate   # post-install check
#   ./scripts/install.sh --uninstall  # remove installed artifacts
#   sudo ./scripts/install.sh --system # legacy system install path
#   MPM_SYSTEM=1 ./scripts/install.sh # same as --system (env form)
#
# Environment overrides (work in both user and system modes):
#   PREFIX       Install prefix (default: $HOME/.mpm or /usr/local)
#   DATA_ROOT    Runtime data root (default: $HOME/.mpm or /var/lib/mpm)
#   USER_NAME    Target user (default: current user)
#
# Idempotency:
#   Re-running on an existing install detects the state and acts
#   accordingly. It does NOT auto-delete data. Existing mpm binary at
#   $PREFIX/bin/mpm is backed up before the wrapper overwrites it.
#   Legacy system-service teardown is gated behind a confirmation
#   prompt unless --yes is passed.
#
# Exit codes:
#   0  success
#   1  preflight failed (prereqs missing, env wrong)
#   2  build failed
#   3  install failed (permissions, disk)
#   4  service start failed
#   5  validation failed

set -euo pipefail

# ---------- constants ----------
readonly SCRIPT_NAME=$(basename "$0")
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
readonly PROJECT_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
readonly SERVICE_NAME="mpm-scheduler"
readonly LEGACY_SYSTEM_UNIT="/etc/systemd/system/${SERVICE_NAME}.service"
readonly LEGACY_DATA="/var/lib/mpm"
readonly LOG_PREFIX="[mpm-install]"

# ---------- mode resolution ----------
# User mode = default. System mode = legacy /var/lib/mpm install path.
# System mode is opt-in (--system flag OR MPM_SYSTEM=1) and requires root.
USE_SYSTEM=0
if [ "${MPM_SYSTEM:-0}" = "1" ]; then
    USE_SYSTEM=1
fi

# ---------- mutable state (set by parse_args / preflight) ----------
MODE="install"
ASSUME_YES=0
USER_NAME="${SUDO_USER:-${USER:-$(id -un)}}"

# ---------- paths (depend on USE_SYSTEM; resolved after parse_args) ----------
PREFIX=""
DATA_ROOT=""
SERVICE_DST=""

# ---------- logging ----------
log()  { printf '%s %s\n' "$LOG_PREFIX" "$*" >&2; }
warn() { printf '%s WARN: %s\n' "$LOG_PREFIX" "$*" >&2; }
err()  { printf '%s ERROR: %s\n' "$LOG_PREFIX" "$*" >&2; }
die()  { err "$*"; exit "${2:-1}"; }
note() { printf '\n%s ===== %s =====\n' "$LOG_PREFIX" "$*" >&2; }

# ---------- helpers ----------
require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        die "this mode requires root (use sudo). Try: sudo $SCRIPT_NAME --system" 1
    fi
}

resolve_user_home() {
    if [ -z "$USER_NAME" ]; then
        die "cannot determine target user" 1
    fi
    if ! id "$USER_NAME" >/dev/null 2>&1; then
        die "user '$USER_NAME' does not exist" 1
    fi
}

# Resolve PREFIX / DATA_ROOT / SERVICE_DST based on USE_SYSTEM.
# Called after parse_args so --system flag has been processed.
#
# User mode: PREFIX and DATA_ROOT both default to $HOME/.mpm — i.e.
# PREFIX/bin and DATA_ROOT are siblings under the same root. Single
# canonical install location. No PATH ordering. No drift.
resolve_paths() {
    if [ $USE_SYSTEM -eq 1 ]; then
        PREFIX="${PREFIX:-/usr/local}"
        DATA_ROOT="${DATA_ROOT:-/var/lib/mpm}"
        SERVICE_DST="/etc/systemd/system/${SERVICE_NAME}.service"
    else
        PREFIX="${PREFIX:-$HOME/.mpm}"
        DATA_ROOT="${DATA_ROOT:-$HOME/.mpm}"
        SERVICE_DST="$HOME/.config/systemd/user/${SERVICE_NAME}.service"
    fi
}

# ---------- detection ----------
detect_os() {
    if [ -r /etc/os-release ]; then
        . /etc/os-release
        echo "${ID:-linux}"
    else
        echo "unknown"
    fi
}

detect_init_system() {
    if command -v systemctl >/dev/null 2>&1 && \
       ([ -d /run/systemd/system ] || [ -d /run/user/"$(id -u)"/systemd ]); then
        echo "systemd"
    else
        echo "none"
    fi
}

detect_openclaw() {
    if command -v openclaw >/dev/null 2>&1; then
        echo "yes"
    else
        echo "no"
    fi
}

# ---------- legacy detection ----------
# For alpha testers upgrading from the old /var/lib/mpm + system
# systemd install. Detects:
#   1. Old system unit at /etc/systemd/system/mpm-scheduler.service
#   2. Old data at /var/lib/mpm
# Both are non-fatal — we warn and let the operator decide. The
# teardown of the old service is opt-in (prompts by default, auto
# with --yes) because killing a running scheduler without explicit
# consent is rude even when migrating to a replacement.
detect_legacy() {
    local legacy_unit_present=0 legacy_data_present=0
    [ -f "$LEGACY_SYSTEM_UNIT" ] && legacy_unit_present=1
    [ -f "$LEGACY_DATA/src/db/mpm.db" ] && legacy_data_present=1

    if [ $legacy_unit_present -eq 1 ]; then
        warn "════════════════════════════════════════════════════════════════"
        warn "LEGACY SYSTEM-SERVICE DETECTED"
        warn "  $LEGACY_SYSTEM_UNIT exists (from old /var/lib/mpm install)"
        warn "  it polls the OLD database at $LEGACY_DATA and will keep"
        warn "  running until you disable it. Running this new install"
        warn "  alongside it = split-brain dual-scheduler scenario."
        warn "════════════════════════════════════════════════════════════════"
        if [ $ASSUME_YES -eq 1 ]; then
            log "  --yes: auto-disabling legacy unit"
            teardown_legacy_unit
        else
            local ans
            printf '%s Disable and stop the legacy system service now? [y/N] ' "$LOG_PREFIX"
            read -r ans
            case "$ans" in
                y|Y|yes|YES) teardown_legacy_unit ;;
                *)            warn "  legacy service left running — you can disable it later with:" \
                                   warn "    sudo systemctl disable --now $SERVICE_NAME" ;;
            esac
        fi
    fi

    if [ $legacy_data_present -eq 1 ]; then
        warn "════════════════════════════════════════════════════════════════"
        warn "LEGACY DATA DETECTED"
        warn "  $LEGACY_DATA/src/db/mpm.db exists from an old install"
        warn "  this new install creates a FRESH database at $DATA_ROOT"
        warn "  if you want to preserve your old memories/decisions/theories,"
        warn "  migrate manually before continuing:"
        warn "    sudo systemctl stop $SERVICE_NAME"
        warn "    sudo cp -a $LEGACY_DATA/src/db/mpm.db* $DATA_ROOT/src/db/"
        warn "    sudo chown -R \$USER:\$USER $DATA_ROOT"
        warn "    sudo systemctl start $SERVICE_NAME"
        warn "════════════════════════════════════════════════════════════════"
    fi
}

# teardown_legacy_unit disables and removes the old /etc/systemd/system/
# unit. Requires root (the unit is owned by root). If we don't have
# root, log loud and bail — operator can run sudo systemctl manually.
teardown_legacy_unit() {
    if [ "$(id -u)" -ne 0 ]; then
        warn "  cannot disable legacy unit without root (it lives at $LEGACY_SYSTEM_UNIT)"
        warn "  run: sudo systemctl disable --now $SERVICE_NAME"
        warn "  then re-run this script"
        return 0
    fi
    if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
        log "  stopping legacy $SERVICE_NAME"
        systemctl stop "$SERVICE_NAME" || warn "  stop failed (continuing)"
    fi
    if systemctl is-enabled --quiet "$SERVICE_NAME" 2>/dev/null; then
        log "  disabling legacy $SERVICE_NAME"
        systemctl disable "$SERVICE_NAME" || warn "  disable failed (continuing)"
    fi
    if [ -f "$LEGACY_SYSTEM_UNIT" ]; then
        rm -f "$LEGACY_SYSTEM_UNIT"
        log "  removed $LEGACY_SYSTEM_UNIT"
    fi
    systemctl daemon-reload 2>/dev/null || true
}

# ---------- state inspection ----------
inspect_existing_state() {
    local has_data=0 has_service=0 has_binaries=0 has_wrapper=0 has_user_unit=0
    [ -f "$DATA_ROOT/src/db/mpm.db" ] && has_data=1
    [ -f "$SERVICE_DST" ] && has_service=1
    [ -x "$PREFIX/bin/mpm-scheduler" ] && has_binaries=1
    [ -f "$PREFIX/bin/mpm" ] && has_wrapper=1
    [ -f "$HOME/.config/systemd/user/${SERVICE_NAME}.service" ] && has_user_unit=1

    log "install mode:    $([ $USE_SYSTEM -eq 1 ] && echo system || echo user)"
    log "existing install state:"
    log "  data dir:      $([ -d "$DATA_ROOT" ] && echo present || echo absent)"
    log "  database:      $([ $has_data -eq 1 ] && echo present || echo absent)"
    log "  service unit:  $([ $has_service -eq 1 ] && echo installed || echo absent)"
    log "  binaries:      $([ $has_binaries -eq 1 ] && echo present || echo absent)"
    log "  mpm wrapper:   $([ $has_wrapper -eq 1 ] && echo present || echo absent)"
    if [ $USE_SYSTEM -eq 0 ]; then
        log "  user unit:     $([ $has_user_unit -eq 1 ] && echo present || echo absent)"
    fi
}

# ---------- preflight ----------
check_prereqs() {
    log "checking prerequisites..."
    command -v go >/dev/null 2>&1 || die "Go not found in PATH" 1
    command -v systemctl >/dev/null 2>&1 || die "systemctl not found (systemd required)" 1
    [ -d "$PROJECT_ROOT" ] || die "project root not found: $PROJECT_ROOT" 1
    [ -f "$PROJECT_ROOT/Makefile" ] || die "Makefile not found in $PROJECT_ROOT" 1
    log "  ✓ go:       $(command -v go)"
    log "  ✓ systemd:  present"
    log "  ✓ project:  $PROJECT_ROOT"
}

preflight() {
    note "PREFLIGHT"
    [ $USE_SYSTEM -eq 1 ] && require_root
    resolve_user_home
    log "target user:  $USER_NAME"
    log "project:      $PROJECT_ROOT"
    log "prefix:       $PREFIX"
    log "data root:    $DATA_ROOT"

    if [ $USE_SYSTEM -eq 0 ]; then
        log "install mode: USER-SPACE (no sudo required)"
        log "  systemd unit: $SERVICE_DST"
    else
        log "install mode: SYSTEM (legacy /var/lib/mpm path)"
    fi

    local init_sys
    init_sys=$(detect_init_system)
    log "init system:  $init_sys"
    [ "$init_sys" = "systemd" ] || die "systemd required (got: $init_sys)" 1

    local os
    os=$(detect_os)
    log "OS:           $os (tested on Ubuntu 24.04)"

    local openclaw
    openclaw=$(detect_openclaw)
    log "openclaw:     $openclaw"

    check_prereqs
    detect_legacy
    inspect_existing_state
    log "preflight ok"
}

# ---------- phases ----------
phase_build() {
    note "BUILD"
    cd "$PROJECT_ROOT"
    if ! make build; then
        die "make build failed" 2
    fi
    for bin in mpm mpm-mcp mpm-scheduler mpm-critic; do
        [ -x "$PROJECT_ROOT/bin/$bin" ] || die "build did not produce bin/$bin" 2
    done
    log "build complete"
}

# Backup any pre-existing binary at $1 if it's an ELF (not a wrapper).
backup_raw_binary_if_present() {
    local target="$1"
    if [ ! -e "$target" ]; then
        return 0
    fi
    local first_bytes
    first_bytes=$(head -c 2 "$target" 2>/dev/null || true)
    if [ "$first_bytes" = "#!" ]; then
        log "  $target is already a wrapper — keeping"
        return 0
    fi
    local ts backup
    ts=$(date +%Y%m%d-%H%M%S)
    backup="${target}.pre-wrapper.${ts}"
    log "  backing up raw binary: $target -> $backup"
    mv "$target" "$backup"
    printf '%s\n' "$backup"
}

phase_binaries() {
    note "BINARIES"
    install -d -m 0755 "$PREFIX/bin"

    # 3 daemon binaries (raw ELF, owned by current user in user mode)
    for bin in mpm-scheduler mpm-critic mpm-mcp; do
        install -m 0755 "$PROJECT_ROOT/bin/$bin" "$PREFIX/bin/$bin"
        log "  installed $PREFIX/bin/$bin"
    done

    # Real mpm binary (renamed to .real so the wrapper can claim the canonical name)
    install -m 0755 "$PROJECT_ROOT/bin/mpm" "$PREFIX/bin/mpm.real"
    log "  installed $PREFIX/bin/mpm.real"

    # Wrapper: sets MPM_WORKSPACE then exec's the real binary.
    # Any pre-existing mpm at this path is backed up first (safety).
    backup_raw_binary_if_present "$PREFIX/bin/mpm" >/dev/null

    cat > "$PREFIX/bin/mpm" <<WRAPPER
#!/bin/sh
# mpm CLI wrapper — installed by scripts/install.sh
# Routes CLI to the per-user workspace regardless of CWD.
exec env MPM_WORKSPACE=${DATA_ROOT} ${PREFIX}/bin/mpm.real "\$@"
WRAPPER
    chmod 0755 "$PREFIX/bin/mpm"
    log "  installed wrapper $PREFIX/bin/mpm -> $PREFIX/bin/mpm.real"
}

phase_data_dir() {
    note "DATA DIRECTORY"
    # Runtime perms enforced by the binary at startup (AssertUserDirPerms0700
    # + TightenFilePerms0600). Here we just create the structure; the binary
    # tightens perms before opening the DB.
    install -d -m 0755 "$DATA_ROOT/src/db" "$DATA_ROOT/backups/critic-pre"
    log "  created $DATA_ROOT/{src/db,backups/critic-pre}"

    if [ ! -f "$DATA_ROOT/src/db/mpm.db" ]; then
        log "  no database at $DATA_ROOT/src/db/mpm.db"
        log "  it will be initialized on first 'mpm' CLI invocation"
    else
        log "  database present at $DATA_ROOT/src/db/mpm.db"
    fi
}

phase_service() {
    note "SYSTEMD SERVICE"
    install -d -m 0755 "$(dirname "$SERVICE_DST")"

    # Pick the right unit template (user vs system). Both templates
    # exist in contrib/systemd/ for the alpha; the .user template is
    # the canonical one going forward.
    local service_src
    if [ -f "${PROJECT_ROOT}/contrib/systemd/${SERVICE_NAME}.service.user" ]; then
        service_src="${PROJECT_ROOT}/contrib/systemd/${SERVICE_NAME}.service.user"
    else
        service_src="${PROJECT_ROOT}/contrib/systemd/${SERVICE_NAME}.service.system"
    fi

    install -m 0644 "$service_src" "$SERVICE_DST"
    log "  installed $SERVICE_DST"

    if [ $USE_SYSTEM -eq 1 ]; then
        systemctl daemon-reload
        systemctl enable "$SERVICE_NAME"
        if systemctl is-active --quiet "$SERVICE_NAME"; then
            systemctl restart "$SERVICE_NAME"
        else
            systemctl start "$SERVICE_NAME"
        fi
        sleep 2
        if systemctl is-active --quiet "$SERVICE_NAME"; then
            log "  ✓ $SERVICE_NAME active (system)"
        else
            err "  ✗ $SERVICE_NAME failed to start"
            err "  diagnostics: journalctl -u $SERVICE_NAME -n 20 --no-pager"
            die "service failed to start" 4
        fi
    else
        # User-space service. Two extra concerns vs system:
        #   1. Need loginctl enable-linger so the user service survives
        #      logout / session end (default user services die with the
        #      session — fatal for a long-running scheduler).
        #   2. systemctl --user needs XDG_RUNTIME_DIR; the env var may
        #      need to be set explicitly for non-interactive shells.
        local runtime_dir="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
        if [ ! -d "$runtime_dir" ]; then
            warn "XDG_RUNTIME_DIR ($runtime_dir) not present"
            warn "user systemd services may not start until you log in interactively"
        fi

        log "  enabling user lingering (loginctl enable-linger)"
        loginctl enable-linger "$USER_NAME" 2>/dev/null \
            || warn "  loginctl enable-linger failed (the service will stop at logout)"

        systemctl --user daemon-reload
        systemctl --user enable "$SERVICE_NAME"
        log "  enabled $SERVICE_NAME (user)"

        if systemctl --user is-active --quiet "$SERVICE_NAME"; then
            systemctl --user restart "$SERVICE_NAME"
        else
            systemctl --user start "$SERVICE_NAME"
        fi
        sleep 2
        if systemctl --user is-active --quiet "$SERVICE_NAME"; then
            log "  ✓ $SERVICE_NAME active (user)"
        else
            err "  ✗ $SERVICE_NAME failed to start"
            err "  diagnostics: journalctl --user -u $SERVICE_NAME -n 20 --no-pager"
            die "user service failed to start" 4
        fi
    fi
}

phase_host_integration() {
    note "HOST INTEGRATION"
    if ! command -v openclaw >/dev/null 2>&1; then
        log "openclaw not detected — skipping MCP registration"
        log "  (after installing openclaw, run manually:)"
        log "    openclaw mcp add mpm --command $PREFIX/bin/mpm-mcp --env MPM_WORKSPACE=$DATA_ROOT"
        return 0
    fi

    log "openclaw detected — registering mpm MCP"

    if openclaw mcp list 2>/dev/null | grep -q -- '^- mpm$'; then
        log "  mpm MCP exists — updating via 'set'"
        openclaw mcp set mpm "$(cat <<JSON
{
  "command": "$PREFIX/bin/mpm-mcp",
  "env": {
    "MPM_WORKSPACE": "$DATA_ROOT",
    "MPM_ACTIVE_MODE": "programming",
    "MPM_ACTIVE_PERSONA": "correspondent"
  }
}
JSON
)"
    else
        log "  mpm MCP not registered — adding"
        openclaw mcp add mpm \
            --command "$PREFIX/bin/mpm-mcp" \
            --env "MPM_WORKSPACE=$DATA_ROOT" \
            --env "MPM_ACTIVE_MODE=programming" \
            --env "MPM_ACTIVE_PERSONA=correspondent"
    fi

    log "  restarting gateway to load MCP config"
    if openclaw gateway restart; then
        log "  ✓ gateway restarted"
    else
        warn "  gateway restart failed — run manually: openclaw gateway restart"
    fi
}

phase_validate() {
    note "VALIDATION"
    local errors=0

    # 1. Service (active under either user or system scope)
    if [ $USE_SYSTEM -eq 1 ]; then
        if systemctl is-active --quiet "$SERVICE_NAME"; then
            log "  ✓ systemd service active (system scope)"
        else
            err "  ✗ systemd service NOT active (system scope)"
            errors=$((errors + 1))
        fi
    else
        if systemctl --user is-active --quiet "$SERVICE_NAME"; then
            log "  ✓ systemd service active (user scope)"
        else
            err "  ✗ systemd service NOT active (user scope)"
            errors=$((errors + 1))
        fi
    fi

    # 2. CLI wrapper (uses wrapper which sets MPM_WORKSPACE)
    if "$PREFIX/bin/mpm" call health_check --payload '{}' 2>/dev/null \
        | grep -q '"ok":true'; then
        log "  ✓ CLI wrapper functional (health_check ok)"
    else
        err "  ✗ CLI wrapper health_check FAILED"
        errors=$((errors + 1))
    fi

    # 3. Lock file
    if [ -f "$DATA_ROOT/scheduler.lock" ]; then
        log "  ✓ lock file present at $DATA_ROOT/scheduler.lock"
    else
        warn "  ! lock file not found (daemon may not have ticked yet)"
    fi

    # 4. MCP registration (if openclaw present)
    if command -v openclaw >/dev/null 2>&1; then
        if openclaw mcp show mpm 2>/dev/null | grep -q "MPM_WORKSPACE.*$DATA_ROOT"; then
            log "  ✓ MCP registration points at $DATA_ROOT"
        else
            warn "  ! MCP registration may be stale — verify with 'openclaw mcp show mpm'"
        fi
    fi

    # 5. Prime directives seeded (warn-only — install does not auto-seed)
    local directives_count=0
    local directives_json
    directives_json=$("$PREFIX/bin/mpm" call read_directives --payload '{}' 2>/dev/null \
        | grep -oE '"count":[0-9]+' | head -1 | grep -oE '[0-9]+' || true)
    if [ -n "$directives_json" ]; then
        directives_count="$directives_json"
    fi
    if [ "$directives_count" -gt 0 ]; then
        log "  ✓ prime directives present ($directives_count)"
    else
        warn "  ! no prime directives found"
        warn "    the agent will boot without cognitive bootstrap"
        warn "    to seed the baseline, run: $PREFIX/bin/mpm ops init directives"
    fi

    if [ $errors -gt 0 ]; then
        die "validation failed ($errors error(s))" 5
    fi
    log "validation passed"
}

# ---------- modes ----------
mode_install() {
    preflight
    phase_build
    phase_binaries
    phase_data_dir
    phase_service
    phase_host_integration
    phase_validate
    note "INSTALL COMPLETE"
    if [ $USE_SYSTEM -eq 1 ]; then
        log "  Mode:       SYSTEM (legacy /var/lib/mpm)"
    else
        log "  Mode:       USER-SPACE"
    fi
    log "  CLI:        $PREFIX/bin/mpm (wrapper) -> $PREFIX/bin/mpm.real"
    if [ $USE_SYSTEM -eq 1 ]; then
        log "  Daemon:     $(systemctl is-active $SERVICE_NAME) ($SERVICE_DST)"
        log "  Logs:       journalctl -u $SERVICE_NAME -f"
    else
        log "  Daemon:     $(systemctl --user is-active $SERVICE_NAME) ($SERVICE_DST)"
        log "  Logs:       journalctl --user -u $SERVICE_NAME -f"
    fi
    log "  Data root:  $DATA_ROOT"
    log ""
    log "next steps (manual):"
    log "  mpm ops init directives   # seed prime directives (cognitive rules)"
    log "  mpm status                # verify DB reachable"
    log "  mpm call read_wake_context   # first agent tool call"
}

mode_check() {
    preflight
    log "check complete (no changes made)"
}

mode_dry_run() {
    log "DRY RUN — printing intended actions, no changes will be made"
    preflight
    log ""
    log "would execute:"
    log "  cd $PROJECT_ROOT && make build"
    log "  install -m 0755 .../bin/mpm-scheduler -> $PREFIX/bin/mpm-scheduler"
    log "  install -m 0755 .../bin/mpm-critic    -> $PREFIX/bin/mpm-critic"
    log "  install -m 0755 .../bin/mpm-mcp       -> $PREFIX/bin/mpm-mcp"
    log "  install -m 0755 .../bin/mpm           -> $PREFIX/bin/mpm.real"
    log "  write wrapper $PREFIX/bin/mpm"
    log "  mkdir -p $DATA_ROOT/src/db $DATA_ROOT/backups/critic-pre"
    if [ $USE_SYSTEM -eq 1 ]; then
        log "  install -m 0644 .../contrib/systemd/${SERVICE_NAME}.service.system -> $SERVICE_DST"
        log "  systemctl daemon-reload && enable --now $SERVICE_NAME"
    else
        log "  loginctl enable-linger $USER_NAME"
        log "  install -m 0644 .../contrib/systemd/${SERVICE_NAME}.service.user -> $SERVICE_DST"
        log "  systemctl --user daemon-reload && enable --now $SERVICE_NAME"
    fi
    log "  openclaw mcp add/set mpm (if openclaw detected)"
    log "  validate via systemctl status + mpm health_check"
    log ""
    log "dry run complete (no changes made)"
}

mode_validate() {
    log "VALIDATE (read-only)"
    if [ ! -f "$SERVICE_DST" ]; then
        die "no MPM service found at $SERVICE_DST — run install first" 5
    fi
    phase_validate
}

mode_uninstall() {
    note "UNINSTALL"
    log "this removes installed artifacts but PRESERVES $DATA_ROOT"
    log "to remove data too: rm -rf $DATA_ROOT (after this script completes)"

    if [ $USE_SYSTEM -eq 1 ]; then
        if [ -f "$SERVICE_DST" ]; then
            [ "$(id -u)" -eq 0 ] && systemctl disable --now "$SERVICE_NAME" 2>/dev/null || true
            rm -f "$SERVICE_DST"
            systemctl daemon-reload 2>/dev/null || true
            log "  removed $SERVICE_DST"
        fi
    else
        if [ -f "$SERVICE_DST" ]; then
            systemctl --user disable --now "$SERVICE_NAME" 2>/dev/null || true
            rm -f "$SERVICE_DST"
            systemctl --user daemon-reload 2>/dev/null || true
            log "  removed $SERVICE_DST"
        fi
        # Note: loginctl enable-linger is intentionally NOT undone —
        # operator may have other user services that benefit.
    fi

    for bin in mpm mpm.real mpm-scheduler mpm-critic mpm-mcp; do
        if [ -f "$PREFIX/bin/$bin" ]; then
            rm -f "$PREFIX/bin/$bin"
            log "  removed $PREFIX/bin/$bin"
        fi
    done

    log "uninstall complete"
    log "  source code at $PROJECT_ROOT is untouched"
    log "  data at $DATA_ROOT is preserved (remove manually if desired)"
    log "  legacy system unit at $LEGACY_SYSTEM_UNIT is preserved (remove manually)"
}

# ---------- arg parsing ----------
usage() {
    cat <<USAGE
$SCRIPT_NAME — MPM install (user-space by default)

Usage: $SCRIPT_NAME [mode] [options]

Modes (default: install):
  (default)       Full install: preflight, build, install binaries + service
  --check         Preflight only — verify environment, no changes
  --dry-run       Print intended actions, no changes
  --validate      Post-install validation (read-only)
  --uninstall     Remove installed artifacts (data preserved)

Options:
  --prefix <path>      Install prefix (user default: \$HOME/.local/bin;
                        system default: /usr/local)
  --data-root <path>   Runtime data root (user default: \$HOME/.mpm;
                        system default: /var/lib/mpm)
  --user <name>        Target user (default: current user)
  --system             Use legacy /var/lib/mpm + system systemd install
                        (requires root via sudo). Default is user-space.
  --yes                Skip confirmation prompts (auto-disable legacy unit
                        when detected, accept default DATA_ROOT, etc.)
  -h, --help           Show this help

Examples:
  $SCRIPT_NAME                  # full user-space install (no sudo)
  $SCRIPT_NAME --check          # environment check (no changes)
  $SCRIPT_NAME --dry-run        # show intended actions
  $SCRIPT_NAME --validate       # verify install
  $SCRIPT_NAME --uninstall      # remove install (data preserved)
  sudo $SCRIPT_NAME --system    # legacy /var/lib/mpm + system service

Exit codes:
  0  success
  1  preflight failed
  2  build failed
  3  install failed
  4  service start failed
  5  validation failed

Notes:
  - The default (user-space) install requires no root.
  - Legacy /var/lib/mpm + sudo installs are still supported via --system
    for operators who genuinely need shared system state.
  - On upgrade from a legacy install, the script detects the old system
    unit and offers to disable it before installing the new user unit.
USAGE
}

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --check)        MODE="check"; shift ;;
            --dry-run)      MODE="dry_run"; shift ;;
            --validate)     MODE="validate"; shift ;;
            --uninstall)    MODE="uninstall"; shift ;;
            --system)       USE_SYSTEM=1; shift ;;
            --prefix)       PREFIX="$2"; shift 2 ;;
            --data-root)    DATA_ROOT="$2"; shift 2 ;;
            --user)         USER_NAME="$2"; shift 2 ;;
            --yes)          ASSUME_YES=1; shift ;;
            -h|--help)      usage; exit 0 ;;
            *)              err "unknown option: $1"; usage; exit 1 ;;
        esac
    done
}

main() {
    parse_args "$@"
    resolve_paths

    case "$MODE" in
        install)   mode_install ;;
        check)     mode_check ;;
        dry_run)   mode_dry_run ;;
        validate)  mode_validate ;;
        uninstall) mode_uninstall ;;
        *)         err "unknown mode: $MODE"; usage; exit 1 ;;
    esac
}

main "$@"