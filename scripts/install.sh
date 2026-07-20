#!/usr/bin/env bash
#
# scripts/install.sh — MPM full system install
#
# Provisions the MPM cognitive substrate on a Linux host:
#   1. Builds binaries (mpm, mpm-mcp, mpm-scheduler, mpm-critic)
#   2. Installs binaries to ${PREFIX}/bin/
#   3. Replaces /usr/local/bin/mpm with a workspace-setting wrapper
#   4. Creates /var/lib/mpm/ as the runtime data root (unencrypted)
#   5. Installs and enables the SYSTEM-level systemd service
#      (boots with the machine, independent of session/encryption)
#   6. Registers the MCP server with OpenClaw if present
#   7. Validates end-to-end
#
# Usage:
#   sudo ./scripts/install.sh              # full install
#   sudo ./scripts/install.sh --check      # preflight only (no changes)
#   sudo ./scripts/install.sh --dry-run    # print intended actions
#   sudo ./scripts/install.sh --validate   # post-install check
#   sudo ./scripts/install.sh --uninstall  # remove installed artifacts
#
# Environment overrides:
#   PREFIX      Install prefix (default: /usr/local)
#   DATA_ROOT   Runtime data root (default: /var/lib/mpm)
#   USER_NAME   Target user (default: SUDO_USER or $USER)
#
# Idempotency:
#   Re-running on an existing install detects the state and acts
#   accordingly. It does NOT auto-delete data. Existing mpm binary at
#   $PREFIX/bin/mpm is backed up before the wrapper overwrites it.
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
readonly PREFIX="${PREFIX:-/usr/local}"
readonly DATA_ROOT="${DATA_ROOT:-/var/lib/mpm}"
readonly SERVICE_NAME="mpm-scheduler"
readonly SERVICE_SRC="${PROJECT_ROOT}/contrib/systemd/${SERVICE_NAME}.service.system"
readonly SERVICE_DST="/etc/systemd/system/${SERVICE_NAME}.service"
readonly USER_LEGACY_UNIT="${HOME}/.config/systemd/user/${SERVICE_NAME}.service"
readonly LOG_PREFIX="[mpm-install]"

# ---------- mutable state (set by parse_args / preflight) ----------
MODE="install"
ASSUME_YES=0
USER_NAME="${SUDO_USER:-${USER:-}}"

# ---------- logging ----------
log()  { printf '%s %s\n' "$LOG_PREFIX" "$*" >&2; }
warn() { printf '%s WARN: %s\n' "$LOG_PREFIX" "$*" >&2; }
err()  { printf '%s ERROR: %s\n' "$LOG_PREFIX" "$*" >&2; }
die()  { err "$*"; exit "${2:-1}"; }
note() { printf '\n%s ===== %s =====\n' "$LOG_PREFIX" "$*" >&2; }

# ---------- helpers ----------
require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        die "this script must run as root (use sudo). Try: sudo $SCRIPT_NAME"
    fi
}

resolve_user_home() {
    if [ -z "$USER_NAME" ]; then
        die "cannot determine target user (no SUDO_USER or USER env var)"
    fi
    if ! id "$USER_NAME" >/dev/null 2>&1; then
        die "user '$USER_NAME' does not exist"
    fi
}

# ---------- detection ----------
detect_encryption() {
    # Returns: 'none', 'ecryptfs', 'luks', or 'unknown'
    local home_mount
    home_mount=$(findmnt -n -o FSTYPE /home 2>/dev/null || true)
    case "$home_mount" in
        ecryptfs) echo "ecryptfs" ;;
        *crypt*)  echo "luks" ;;
        "")       echo "none" ;;
        *)        echo "none" ;;
    esac
}

detect_os() {
    if [ -r /etc/os-release ]; then
        # shellcheck disable=SC1091
        . /etc/os-release
        echo "${ID:-linux}"
    else
        echo "unknown"
    fi
}

detect_init_system() {
    if [ "$(id -u)" -eq 0 ] && command -v systemctl >/dev/null 2>&1 && \
       [ -d /run/systemd/system ]; then
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

# ---------- state inspection ----------
inspect_existing_state() {
    local has_data=0 has_service=0 has_binaries=0 has_wrapper=0 has_user_legacy=0
    [ -f "$DATA_ROOT/src/db/mpm.db" ] && has_data=1
    [ -f "$SERVICE_DST" ] && has_service=1
    [ -x "$PREFIX/bin/mpm-scheduler" ] && has_binaries=1
    [ -f "$PREFIX/bin/mpm" ] && has_wrapper=1
    [ -f "$USER_LEGACY_UNIT" ] && has_user_legacy=1

    log "existing install state:"
    log "  data dir:   $([ -d "$DATA_ROOT" ] && echo present || echo absent)"
    log "  database:   $([ $has_data -eq 1 ] && echo present || echo absent)"
    log "  service:    $([ $has_service -eq 1 ] && echo installed || echo absent)"
    log "  binaries:   $([ $has_binaries -eq 1 ] && echo present || echo absent)"
    log "  mpm wrapper: $([ $has_wrapper -eq 1 ] && echo present || echo absent)"
    log "  legacy user unit: $([ $has_user_legacy -eq 1 ] && echo present || echo absent)"

    if [ $has_user_legacy -eq 1 ]; then
        warn "legacy user-level unit found at $USER_LEGACY_UNIT"
        warn "  it is superseded by the system unit but not removed automatically"
        warn "  to remove: rm $USER_LEGACY_UNIT && systemctl --user daemon-reload"
    fi
}

# ---------- preflight ----------
check_prereqs() {
    log "checking prerequisites..."
    command -v go >/dev/null 2>&1 || die "Go not found in PATH" 1
    command -v systemctl >/dev/null 2>&1 || die "systemctl not found (systemd required)" 1
    [ -d "$PROJECT_ROOT" ] || die "project root not found: $PROJECT_ROOT" 1
    [ -f "$PROJECT_ROOT/Makefile" ] || die "Makefile not found in $PROJECT_ROOT" 1
    [ -f "$SERVICE_SRC" ] || die "system unit template not found: $SERVICE_SRC" 1
    log "  ✓ go:    $(command -v go)"
    log "  ✓ systemd: present"
    log "  ✓ project: $PROJECT_ROOT"
}

preflight() {
    note "PREFLIGHT"
    require_root
    resolve_user_home
    local home_dir
    home_dir=$(getent passwd "$USER_NAME" | cut -d: -f6)
    log "target user: $USER_NAME (home: $home_dir)"
    log "project:     $PROJECT_ROOT"
    log "prefix:      $PREFIX"
    log "data root:   $DATA_ROOT"

    local init_sys
    init_sys=$(detect_init_system)
    log "init system: $init_sys"
    [ "$init_sys" = "systemd" ] || die "systemd required (got: $init_sys)" 1

    local encryption
    encryption=$(detect_encryption)
    log "home encryption: $encryption"
    if [ "$encryption" != "none" ]; then
        warn "home directory uses $encryption encryption"
        warn "this install will use a system-level service + unencrypted /var/lib/mpm"
        warn "a user-level service would silently fail on boot in this environment"
    fi

    local os
    os=$(detect_os)
    log "OS: $os (tested on Ubuntu 24.04)"

    local openclaw
    openclaw=$(detect_openclaw)
    log "openclaw: $openclaw"

    check_prereqs
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
# Returns the backup path if backup was performed, empty otherwise.
backup_raw_binary_if_present() {
    local target="$1"
    if [ ! -e "$target" ]; then
        return 0
    fi
    # A wrapper script starts with '#!'. If it does, treat as already-installed.
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

    # 3 daemon binaries (raw ELF, root-owned)
    for bin in mpm-scheduler mpm-critic mpm-mcp; do
        install -m 0755 "$PROJECT_ROOT/bin/$bin" "$PREFIX/bin/$bin"
        log "  installed $PREFIX/bin/$bin"
    done

    # Real mpm binary (renamed to .real so the wrapper can claim /usr/local/bin/mpm)
    install -m 0755 "$PROJECT_ROOT/bin/mpm" "$PREFIX/bin/mpm.real"
    log "  installed $PREFIX/bin/mpm.real"

    # Wrapper: sets MPM_WORKSPACE then exec's the real binary.
    # Any pre-existing mpm at this path is backed up first (safety).
    backup_raw_binary_if_present "$PREFIX/bin/mpm" >/dev/null

    cat > "$PREFIX/bin/mpm" <<WRAPPER
#!/bin/sh
# mpm CLI wrapper — installed by scripts/install.sh
# Routes CLI to the system-wide workspace regardless of CWD.
exec env MPM_WORKSPACE=${DATA_ROOT} ${PREFIX}/bin/mpm.real "\$@"
WRAPPER
    chmod 0755 "$PREFIX/bin/mpm"
    log "  installed wrapper $PREFIX/bin/mpm -> $PREFIX/bin/mpm.real"
}

phase_data_dir() {
    note "DATA DIRECTORY"
    install -d -m 0755 -o "$USER_NAME" -g "$USER_NAME" \
        "$DATA_ROOT/src/db" "$DATA_ROOT/backups/critic-pre"
    log "  created $DATA_ROOT/{src/db,backups/critic-pre} (owner: $USER_NAME)"

    if [ ! -f "$DATA_ROOT/src/db/mpm.db" ]; then
        log "  no database at $DATA_ROOT/src/db/mpm.db"
        log "  it will be initialized on first 'mpm' CLI invocation"
    else
        log "  database present at $DATA_ROOT/src/db/mpm.db"
    fi
}

phase_service() {
    note "SYSTEMD SERVICE"
    install -m 0644 "$SERVICE_SRC" "$SERVICE_DST"
    log "  installed $SERVICE_DST"

    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME"
    log "  enabled $SERVICE_NAME"

    # Restart (or start) and verify
    if systemctl is-active --quiet "$SERVICE_NAME"; then
        systemctl restart "$SERVICE_NAME"
    else
        systemctl start "$SERVICE_NAME"
    fi
    sleep 2

    if systemctl is-active --quiet "$SERVICE_NAME"; then
        log "  ✓ $SERVICE_NAME active"
    else
        err "  ✗ $SERVICE_NAME failed to start"
        err "  diagnostics: journalctl -u $SERVICE_NAME -n 20 --no-pager"
        die "service failed to start" 4
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

    local mcp_json
    mcp_json=$(cat <<JSON
{
  "command": "$PREFIX/bin/mpm-mcp",
  "env": {
    "MPM_WORKSPACE": "$DATA_ROOT",
    "MPM_ACTIVE_MODE": "programming",
    "MPM_ACTIVE_PERSONA": "correspondent"
  }
}
JSON
)

    # Use 'set' to update existing registration, 'add' to create new.
    # 'add' is a silent no-op on existing server, so check first.
    if openclaw mcp list 2>/dev/null | grep -q -- '^- mpm$'; then
        log "  mpm MCP exists — updating via 'set'"
        openclaw mcp set mpm "$mcp_json"
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

    # 1. Service
    if systemctl is-active --quiet "$SERVICE_NAME"; then
        log "  ✓ systemd service active"
    else
        err "  ✗ systemd service NOT active"
        errors=$((errors + 1))
    fi

    # 2. CLI wrapper
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
    # Directives are cognitive state, deliberately separate from
    # infrastructure. Operators who want the baseline bootstrap run:
    #   mpm ops init directives
    # If neither baseline nor custom directives exist, the agent boots
    # without cognitive bootstrap. Warn loudly so this is not silent.
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
    log "  CLI:        $PREFIX/bin/mpm (wrapper) -> $PREFIX/bin/mpm.real"
    log "  Daemon:     $(systemctl is-active $SERVICE_NAME) ($SERVICE_DST)"
    log "  Data root:  $DATA_ROOT"
    log "  Logs:       journalctl -u $SERVICE_NAME -f"
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
    log "  chown -R $USER_NAME:$USER_NAME $DATA_ROOT"
    log "  install -m 0644 $SERVICE_SRC $SERVICE_DST"
    log "  systemctl daemon-reload && enable --now $SERVICE_NAME"
    log "  openclaw mcp add/set mpm (if openclaw detected)"
    log "  validate via systemctl status + mpm health_check"
    log ""
    log "dry run complete (no changes made)"
}

mode_validate() {
    # Validate doesn't require root, just read-only checks
    log "VALIDATE (read-only)"
    if [ ! -f "$SERVICE_DST" ] && [ ! -f "$USER_LEGACY_UNIT" ]; then
        die "no MPM service found — run install first" 5
    fi
    phase_validate
}

mode_uninstall() {
    note "UNINSTALL"
    log "this removes installed artifacts but PRESERVES /var/lib/mpm data"
    log "to remove data too: rm -rf $DATA_ROOT (after this script completes)"

    if [ -f "$SERVICE_DST" ]; then
        systemctl disable --now "$SERVICE_NAME" 2>/dev/null || true
        rm -f "$SERVICE_DST"
        systemctl daemon-reload
        log "  removed $SERVICE_DST"
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
    log "  legacy user unit at $USER_LEGACY_UNIT is preserved (remove manually)"
}

# ---------- arg parsing ----------
usage() {
    cat <<USAGE
$SCRIPT_NAME — MPM full system install

Usage: sudo $SCRIPT_NAME [mode] [options]

Modes (default: install):
  (default)       Full install: preflight, build, install binaries + service
  --check         Preflight only — verify environment, no changes
  --dry-run       Print intended actions, no changes
  --validate      Post-install validation (read-only)
  --uninstall     Remove installed artifacts (data preserved)

Options:
  --prefix <path>   Install prefix (default: /usr/local)
  --data-root <path> Runtime data root (default: /var/lib/mpm)
  --user <name>     Target user (default: SUDO_USER)
  --yes             Skip confirmation prompts
  -h, --help        Show this help

Examples:
  sudo $SCRIPT_NAME              # full install
  sudo $SCRIPT_NAME --check      # environment check (no changes)
  sudo $SCRIPT_NAME --dry-run    # show intended actions
  sudo $SCRIPT_NAME --validate   # verify install
  sudo $SCRIPT_NAME --uninstall  # remove install (data preserved)

Exit codes:
  0  success
  1  preflight failed
  2  build failed
  3  install failed
  4  service start failed
  5  validation failed
USAGE
}

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --check)        MODE="check"; shift ;;
            --dry-run)      MODE="dry_run"; shift ;;
            --validate)     MODE="validate"; shift ;;
            --uninstall)    MODE="uninstall"; shift ;;
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
