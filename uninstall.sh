#!/usr/bin/env bash
# uninstall.sh — MPM substrate removal tool (user-facing, host-agnostic).
#
# This script is the canonical entry point for removing the MPM substrate
# (binaries, services, autostart, PATH symlinks) from a single user's
# context. It is intentionally framework/host agnostic:
#
#   * It does NOT invoke, detect, restart, or remove any host integration
#     (OpenClaw, OpenCode, Claude Code, Pi, Hermes). Host integrations
#     are adapter-owned and live under agent_installation/mpm-*/.
#
#   * It does NOT undo shared account/system state it does not own
#     (e.g. loginctl enable-linger). Shared state may be relied on by
#     other user services; leaving it intact is the safer choice.
#
# Three escalation levels:
#
#   default       Remove runtime. PRESERVE persistent MPM data, config,
#                 backups, env files. Allows clean reinstall against
#                 the same database.
#
#   --purge       Default + delete persistent MPM state (databases,
#                 backups, config, env). Destructive. Requires
#                 confirmation or --yes.
#
#   --shred       --purge semantics + best-effort secure overwrite of
#                 every regular file beneath MPM-owned data roots
#                 BEFORE removal. Requires GNU `shred` and explicit
#                 confirmation (or --yes). NOT guaranteed forensic
#                 erasure on SSD/CoW/snapshot/journaled storage.
#
# Usage:
#   ./uninstall.sh                    # runtime-only uninstall
#   ./uninstall.sh --dry-run          # preview what would happen
#   ./uninstall.sh --purge            # also delete data + config
#   ./uninstall.sh --shred            # --purge + best-effort overwrite
#   ./uninstall.sh --purge --yes      # noninteractive purge
#   ./uninstall.sh --shred --yes      # noninteractive shred
#   ./uninstall.sh --help             # full help
#
# Exit codes:
#   0   success (or dry-run completed)
#   2   invalid arguments / ambiguous flag combination
#   3   shred capability missing (for destructive --shred)
#   4   destructive root failed safety validation
#   5   user declined confirmation
#   6   a shred step failed
#   7   a required removal step failed
#   8   uninstall is operating from inside an install tree it will
#       recursively remove; cannot safely stage self-removal
#
# CWD-independent: this script resolves its own location via BASH_SOURCE.

set -euo pipefail

# ---------- identity / paths ----------

SCRIPT_PATH="${BASH_SOURCE[0]}"
# Resolve symlinks so a wrapper that points elsewhere still works.
while [ -L "$SCRIPT_PATH" ]; do
    LINK_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"
    SCRIPT_PATH="$(readlink "$SCRIPT_PATH")"
    case "$SCRIPT_PATH" in
        /*) ;;
        *)  SCRIPT_PATH="$LINK_DIR/$SCRIPT_PATH" ;;
    esac
done
SCRIPT_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"
# PROJECT_ROOT is the directory containing this script. Used for self-removal
# staging and for printing what tree we're acting on.
readonly PROJECT_ROOT="$SCRIPT_DIR"
readonly SCRIPT_NAME="uninstall.sh"

# ---------- argument parsing ----------

MODE="default"        # default | purge | shred
DRY_RUN=0
ASSUME_YES=0
SHRED_BIN=""

usage() {
    cat <<USAGE
$SCRIPT_NAME — MPM substrate removal tool (user-facing, host-agnostic).

Usage:
  $SCRIPT_NAME                     # runtime-only uninstall (data preserved)
  $SCRIPT_NAME --dry-run           # preview default uninstall
  $SCRIPT_NAME --purge --dry-run   # preview purge scope
  $SCRIPT_NAME --shred --dry-run   # preview shred scope
  $SCRIPT_NAME --purge             # remove runtime + persistent state
  $SCRIPT_NAME --shred             # remove + best-effort secure overwrite
  $SCRIPT_NAME --purge --yes       # noninteractive purge
  $SCRIPT_NAME --shred --yes       # noninteractive shred

Flags:
  --dry-run       Print the destruction plan; perform no mutations.
  --purge         Also delete persistent MPM state (databases, backups,
                  config). Requires confirmation or --yes.
  --shred         Like --purge but secure-overwrite every regular file
                  under MPM data roots before removal. Requires GNU
                  shred and confirmation or --yes.
  --yes           Skip interactive confirmation.
  -h, --help      Show this help.

Notes:
  * --purge and --shred are mutually exclusive. Choose one.
  * --shred is best-effort only. SSD, CoW, snapshot, journaled and
    virtualised storage are NOT guaranteed to be physically erased.
  * Host integrations (OpenClaw, OpenCode, Claude Code, Pi, Hermes)
    are NOT removed by this script. Use the corresponding adapter's
    uninstaller under agent_installation/mpm-*/ if you need to
    remove a host integration.
USAGE
}

# Detect --purge/--shred conflict at parse time by re-scanning $@.
PURGE_SEEN=0
SHRED_SEEN=0
for _arg in "$@"; do
    case "$_arg" in
        --purge) PURGE_SEEN=1 ;;
        --shred) SHRED_SEEN=1 ;;
    esac
done
if [ "$PURGE_SEEN" = "1" ] && [ "$SHRED_SEEN" = "1" ]; then
    printf '%s: --purge and --shred are mutually exclusive; choose one\n' "$SCRIPT_NAME" >&2
    exit 2
fi

# Save original args so main() can forward them across the exec for
# stage-then-exec self-removal.
declare -a ORIGINAL_ARGS
ORIGINAL_ARGS=("$@")

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run) DRY_RUN=1; shift ;;
        --purge)   MODE="purge"; shift ;;
        --shred)   MODE="shred"; shift ;;
        --yes|-y)  ASSUME_YES=1; shift ;;
        -h|--help) usage; exit 0 ;;
        --) shift; break ;;
        -*)
            printf '%s: unknown option: %s\n' "$SCRIPT_NAME" "$1" >&2
            printf 'Run "%s --help" for usage.\n' "$SCRIPT_NAME" >&2
            exit 2
            ;;
        *)
            printf '%s: unexpected positional argument: %s\n' "$SCRIPT_NAME" "$1" >&2
            exit 2
            ;;
    esac
done

# ---------- logging helpers ----------

log()  { printf '[mpm-uninstall] %s\n' "$*"; }
warn() { printf '[mpm-uninstall] WARN: %s\n' "$*" >&2; }
err()  { printf '[mpm-uninstall] ERROR: %s\n' "$*" >&2; }
die()  { err "$1"; exit "${2:-1}"; }

# ---------- canonical MPM paths ----------

# PREFIX is the canonical install prefix. Mirrors install.sh default.
PREFIX="${MPM_PREFIX:-$HOME/.mpm}"
# DATA_ROOT is the persistent data root. By default = PREFIX.
DATA_ROOT="${MPM_DATA_ROOT:-$HOME/.mpm}"

LOCAL_BIN="$HOME/.local/bin"
SYSTEMD_USER_DIR="$HOME/.config/systemd/user"
GRAPHICAL_WANTS_DIR="$SYSTEMD_USER_DIR/graphical-session.target.wants"
AUTOSTART_DIR="$HOME/.config/autostart"
ENV_DIR="$HOME/.config/mpm"

SERVICE_NAME="mpm-scheduler"
SERVICE_DST="$SYSTEMD_USER_DIR/${SERVICE_NAME}.service"
TELEMETRY_SERVICE_DST="$SYSTEMD_USER_DIR/mpm-telemetry.service"
AUTOSTART_FILE="$AUTOSTART_DIR/mpm-post-decrypt.desktop"

# MPM-owned bin names. The first two are the only ones exposed on the
# user PATH (symlinks in ~/.local/bin). The rest are internal daemons.
PATH_BINARIES=(mpm mpm-mcp)
ALL_BINARIES=(mpm mpm.real mpm-mcp mpm-scheduler mpm-critic mpm-telemetry)

# Runtime-only artefacts (removed by default uninstall). These are
# sockets, lock files, and pid files — NOT persistent data.
RUNTIME_PATHS=(
    "$PREFIX/run"
    "$PREFIX/scheduler.lock"
    "$PREFIX/active.json"
    "$PREFIX/active.json.lock"
    "$PREFIX/cascade.lock"
    "$PREFIX/logs"
)

# Persistent state (removed by --purge/--shred ONLY).
PURGE_PATHS=(
    "$DATA_ROOT/src/db"
    "$DATA_ROOT/backups"
    "$DATA_ROOT/mpm_config.json"
    "$DATA_ROOT/mode"
    "$DATA_ROOT/persona"
    "$DATA_ROOT/migrations"
    "$ENV_DIR"
)

# ---------- safety helpers ----------

# Refuse to act on a destructive root that resolves to a forbidden path.
# Usage: validate_destructive_root "label" "/path/to/root"
#
# Defensive checks (spec §14):
#   - empty string
#   - resolves to "/", ".", "..", "$HOME"
#   - parent resolves to "/" (would shred top-level filesystem area)
#   - parent resolves to "$HOME" (would shred the user's home root)
#   - resolves to a path that is NOT under the canonical MPM install
#     prefix ($HOME/.mpm) — defence in depth against operator typo /
#     environment-variable injection / symlink redirection.
validate_destructive_root() {
    local label="$1"
    local raw="$2"

    [ -n "$raw" ] || die "destructive root for $label is empty" 4

    # Resolve to canonical absolute path. Use realpath if available,
    # otherwise a manual cd+pwd fallback.
    local resolved
    if resolved="$(realpath -m -- "$raw" 2>/dev/null)"; then
        :
    else
        resolved="$(cd "$(dirname -- "$raw")" 2>/dev/null && pwd)/$(basename -- "$raw")"
    fi

    # Reject empty / trivial roots.
    case "$resolved" in
        "/"|"")
            die "refusing to act on $label = '$resolved' (forbidden root)" 4
            ;;
    esac
    case "$(basename -- "$resolved")" in
        "."|"..")
            die "refusing to act on $label = '$resolved' (forbidden basename)" 4
            ;;
    esac

    # Reject HOME itself.
    local home_resolved
    home_resolved="$(realpath -m -- "$HOME" 2>/dev/null || echo "$HOME")"
    if [ "$resolved" = "$home_resolved" ]; then
        die "refusing to act on $label = HOME ('$resolved')" 4
    fi

    # Reject top-level filesystem areas: any destructive root whose
    # parent is "/" or "$HOME" is too broad. Examples: "/src/db" or
    # "/home/v/foo" — both are forbidden. The canonical MPM install is
    # $HOME/.mpm, so destructive roots must be at least three levels
    # deep beneath $HOME.
    local parent
    parent="$(dirname -- "$resolved")"
    case "$parent" in
        "/"|"$home_resolved")
            die "refusing to act on $label = '$resolved' (parent '$parent' too broad)" 4
            ;;
    esac

    # Defence-in-depth: reject anything NOT under a known MPM-owned
    # canonical path. The canonical MPM install is $HOME/.mpm, plus
    # a small set of XDG-known MPM-owned dirs that are NOT under that
    # install root (env file, etc.). This catches operator typos, env
    # injection, and symlink redirection to non-MPM paths.
    case "$resolved" in
        # Canonical install root.
        "$home_resolved/".mpm) ;;
        "$home_resolved/".mpm/*) ;;
        # XDG-owned MPM dirs (env file at ~/.config/mpm/mpm.env).
        # Listed explicitly; NOT a "search the home tree" — these are
        # hard-coded XDG spec paths that MPM owns.
        "$home_resolved/".config/mpm) ;;
        "$home_resolved/".config/mpm/*) ;;
        *)
            die "refusing to act on $label = '$resolved' (not under a canonical MPM-owned root)" 4
            ;;
    esac

    printf '%s' "$resolved"
}

# Verify a symlink at $path actually points at $expected_target
# (canonical MPM-owned binary). Refuse to delete if it points elsewhere.
verify_owned_symlink() {
    local path="$1"
    local expected_target="$2"
    [ -L "$path" ] || return 1
    local link_target
    link_target="$(readlink "$path" 2>/dev/null)" || return 1
    # Resolve to absolute (canonical) form for comparison.
    local abs_target
    abs_target="$(cd "$(dirname -- "$path")" && cd "$(dirname -- "$link_target")" 2>/dev/null && pwd)/$(basename -- "$link_target")" || return 1
    abs_target="${abs_target%/}"
    expected_target="${expected_target%/}"
    [ "$abs_target" = "$expected_target" ]
}

# ---------- plan construction ----------

# Populate global arrays with the destruction plan. Performs no mutations.
build_plan() {
    PLAN_RUNTIME_SERVICES=()
    PLAN_RUNTIME_UNITS=()
    PLAN_RUNTIME_AUTOSTART=()
    PLAN_RUNTIME_SYMLINKS=()
    PLAN_RUNTIME_BINARIES=()
    PLAN_RUNTIME_GRAPHICAL_WANTS=()
    PLAN_RUNTIME_LOCKS=()
    PLAN_PURGE_DIRS=()
    PLAN_PURGE_FILES=()
    PLAN_SHRED_DIRS=()
    PLAN_SHRED_FILES=()

    # 1. Services (default uninstall stops them; --purge/--shred also
    #    remove the unit file).
    for svc in "$SERVICE_NAME" mpm-telemetry; do
        local svc_unit="$SYSTEMD_USER_DIR/${svc}.service"
        if [ -f "$svc_unit" ]; then
            PLAN_RUNTIME_SERVICES+=("$svc")
            PLAN_RUNTIME_UNITS+=("$svc_unit")
        fi
    done

    # 2. Autostart (default uninstall removes it; ecryptfs workaround).
    if [ -f "$AUTOSTART_FILE" ]; then
        PLAN_RUNTIME_AUTOSTART+=("$AUTOSTART_FILE")
    fi

    # 3. Symlinks. Only remove if they point at the expected MPM binary.
    for bin in "${PATH_BINARIES[@]}"; do
        local link="$LOCAL_BIN/$bin"
        if [ -L "$link" ] || [ -f "$link" ]; then
            local expected="$PREFIX/bin/$bin"
            if verify_owned_symlink "$link" "$expected"; then
                PLAN_RUNTIME_SYMLINKS+=("$link")
            else
                warn "skipping $link — does not point at $expected (refusing to delete foreign executable)"
            fi
        fi
    done

    # 4. Installed binaries.
    for bin in "${ALL_BINARIES[@]}"; do
        local p="$PREFIX/bin/$bin"
        if [ -f "$p" ] || [ -L "$p" ]; then
            PLAN_RUNTIME_BINARIES+=("$p")
        fi
    done

    # 5. Graphical-session wants (ecryptfs secondary).
    for svc in "$SERVICE_NAME" mpm-telemetry; do
        local want="$GRAPHICAL_WANTS_DIR/${svc}.service"
        if [ -L "$want" ]; then
            PLAN_RUNTIME_GRAPHICAL_WANTS+=("$want")
        fi
    done

    # 6. Runtime locks / sockets / logs (removed by default uninstall).
    for p in "${RUNTIME_PATHS[@]}"; do
        if [ -e "$p" ]; then
            PLAN_RUNTIME_LOCKS+=("$p")
        fi
    done

    # 7. Purge directories (--purge / --shred). Validate each.
    for p in "${PURGE_PATHS[@]}"; do
        if [ -e "$p" ]; then
            local validated
            validated="$(validate_destructive_root "purge path $p" "$p")" || exit 4
            PLAN_PURGE_DIRS+=("$validated")
        fi
    done

    # 8. Shred targets (--shred only): every regular file under
    #    the MPM-owned data root, plus a few additional sensitive roots.
    #    Add the canonical DB root as a shred root if it exists.
    local db_root="$DATA_ROOT/src/db"
    if [ -d "$db_root" ]; then
        local validated_db
        validated_db="$(validate_destructive_root "shred db root" "$db_root")" || exit 4
        PLAN_SHRED_DIRS+=("$validated_db")
        while IFS= read -r -d '' f; do
            PLAN_SHRED_FILES+=("$f")
        done < <(find "$validated_db" -xdev -type f -print0 2>/dev/null || true)
    fi
    # Also shred the config file (single sensitive regular file).
    local cfg="$DATA_ROOT/mpm_config.json"
    if [ -f "$cfg" ]; then
        local validated_cfg
        validated_cfg="$(validate_destructive_root "shred config" "$cfg")" || exit 4
        PLAN_SHRED_FILES+=("$validated_cfg")
    fi
    # And the env file (may contain provider keys).
    local envf="$ENV_DIR/mpm.env"
    if [ -f "$envf" ]; then
        local validated_env
        validated_env="$(validate_destructive_root "shred env file" "$envf")" || exit 4
        PLAN_SHRED_FILES+=("$validated_env")
    fi
}

# ---------- plan execution ----------

print_plan() {
    log "=== destruction plan ==="
    log "mode:                $MODE"
    log "dry-run:             $([ "$DRY_RUN" = "1" ] && echo yes || echo no)"
    log "project root:        $PROJECT_ROOT"
    log "install prefix:      $PREFIX"
    log "data root:           $DATA_ROOT"
    log "systemd user dir:    $SYSTEMD_USER_DIR"
    log "autostart file:      $AUTOSTART_FILE"
    log ""
    log "services to stop:"
    if [ "${#PLAN_RUNTIME_SERVICES[@]}" -eq 0 ]; then
        log "  (none)"
    else
        for s in "${PLAN_RUNTIME_SERVICES[@]}"; do log "  $s"; done
    fi
    log ""
    log "systemd units to remove:"
    if [ "${#PLAN_RUNTIME_UNITS[@]}" -eq 0 ]; then
        log "  (none)"
    else
        for f in "${PLAN_RUNTIME_UNITS[@]}"; do log "  $f"; done
    fi
    log ""
    log "autostart artefacts to remove:"
    if [ "${#PLAN_RUNTIME_AUTOSTART[@]}" -eq 0 ]; then
        log "  (none)"
    else
        for f in "${PLAN_RUNTIME_AUTOSTART[@]}"; do log "  $f"; done
    fi
    log ""
    log "PATH symlinks to remove:"
    if [ "${#PLAN_RUNTIME_SYMLINKS[@]}" -eq 0 ]; then
        log "  (none)"
    else
        for f in "${PLAN_RUNTIME_SYMLINKS[@]}"; do log "  $f"; done
    fi
    log ""
    log "installed binaries to remove:"
    if [ "${#PLAN_RUNTIME_BINARIES[@]}" -eq 0 ]; then
        log "  (none)"
    else
        for f in "${PLAN_RUNTIME_BINARIES[@]}"; do log "  $f"; done
    fi
    log ""
    log "graphical-session wants to remove:"
    if [ "${#PLAN_RUNTIME_GRAPHICAL_WANTS[@]}" -eq 0 ]; then
        log "  (none)"
    else
        for f in "${PLAN_RUNTIME_GRAPHICAL_WANTS[@]}"; do log "  $f"; done
    fi
    log ""
    log "runtime locks/sockets/logs to remove:"
    if [ "${#PLAN_RUNTIME_LOCKS[@]}" -eq 0 ]; then
        log "  (none)"
    else
        for f in "${PLAN_RUNTIME_LOCKS[@]}"; do log "  $f"; done
    fi

    if [ "$MODE" != "default" ]; then
        log ""
        log "persistent state to delete ($MODE):"
        if [ "${#PLAN_PURGE_DIRS[@]}" -eq 0 ]; then
            log "  (none)"
        else
            for f in "${PLAN_PURGE_DIRS[@]}"; do log "  $f"; done
        fi
    fi

    if [ "$MODE" = "shred" ]; then
        log ""
        log "sensitive files to secure-overwrite before deletion:"
        if [ "${#PLAN_SHRED_FILES[@]}" -eq 0 ]; then
            log "  (none)"
        else
            log "  count: ${#PLAN_SHRED_FILES[@]}"
            for f in "${PLAN_SHRED_FILES[@]}"; do log "  $f"; done
        fi
    fi

    log ""
    log "preserved by default uninstall:"
    log "  $DATA_ROOT/src/db"
    log "  $DATA_ROOT/backups"
    log "  $DATA_ROOT/mpm_config.json"
    log "  $DATA_ROOT/mode"
    log "  $DATA_ROOT/persona"
    log "  $DATA_ROOT/migrations"
    log "  $ENV_DIR/mpm.env (if present)"
}

# Probe shred availability; fail loudly if destructive --shred was
# requested without it. Dry-run --shred is allowed without shred.
require_shred() {
    if [ -z "$SHRED_BIN" ]; then
        if command -v shred >/dev/null 2>&1; then
            SHRED_BIN="$(command -v shred)"
        fi
    fi
    if [ -z "$SHRED_BIN" ]; then
        die "GNU 'shred' not found on PATH; cannot perform destructive --shred" 3
    fi
    # Sanity-check that shred actually runs.
    if ! "$SHRED_BIN" --version >/dev/null 2>&1; then
        die "'$SHRED_BIN' did not execute cleanly; refusing destructive --shred" 3
    fi
}

# Apply the destruction plan. For destructive modes, perform all
# mutations; for dry-run, do nothing.
apply_plan() {
    local do_it=1
    [ "$DRY_RUN" = "1" ] && do_it=0

    # 1. Stop MPM-owned services first.
    if [ "${#PLAN_RUNTIME_SERVICES[@]}" -gt 0 ]; then
        if command -v systemctl >/dev/null 2>&1; then
            for s in "${PLAN_RUNTIME_SERVICES[@]}"; do
                if [ "$do_it" = "1" ]; then
                    log "stopping service: $s"
                    systemctl --user disable --now "$s" 2>/dev/null || warn "  could not stop $s (continuing)"
                else
                    log "[dry-run] would stop service: $s"
                fi
            done
        else
            warn "systemctl not on PATH; skipping service stop"
        fi
    fi

    # 2. Secure-overwrite sensitive files (--shred only).
    if [ "$MODE" = "shred" ]; then
        local shred_status=0
        if [ "$do_it" = "1" ]; then
            require_shred
            for f in "${PLAN_SHRED_FILES[@]}"; do
                log "shredding: $f"
                if ! "$SHRED_BIN" -f -n 1 -z -u "$f" 2>/dev/null; then
                    err "shred failed for: $f"
                    shred_status=1
                fi
            done
            if [ "$shred_status" -ne 0 ]; then
                die "one or more shred steps failed" 6
            fi
        else
            log "[dry-run] would shred ${#PLAN_SHRED_FILES[@]} sensitive file(s)"
        fi
    fi

    # 3. Remove systemd unit files.
    for f in "${PLAN_RUNTIME_UNITS[@]}"; do
        if [ "$do_it" = "1" ]; then
            rm -f -- "$f"
            log "removed: $f"
        else
            log "[dry-run] would remove: $f"
        fi
    done
    if [ "$do_it" = "1" ] && command -v systemctl >/dev/null 2>&1; then
        systemctl --user daemon-reload 2>/dev/null || true
        systemctl --user reset-failed 2>/dev/null || true
    fi

    # 4. Autostart artefacts.
    for f in "${PLAN_RUNTIME_AUTOSTART[@]}"; do
        if [ "$do_it" = "1" ]; then
            rm -f -- "$f"
            log "removed: $f"
        else
            log "[dry-run] would remove: $f"
        fi
    done

    # 5. Graphical-session wants.
    for f in "${PLAN_RUNTIME_GRAPHICAL_WANTS[@]}"; do
        if [ "$do_it" = "1" ]; then
            rm -f -- "$f"
            log "removed: $f"
        else
            log "[dry-run] would remove: $f"
        fi
    done

    # 6. Symlinks (only owned ones — see build_plan).
    for f in "${PLAN_RUNTIME_SYMLINKS[@]}"; do
        if [ "$do_it" = "1" ]; then
            rm -f -- "$f"
            log "removed: $f"
        else
            log "[dry-run] would remove: $f"
        fi
    done

    # 7. Installed binaries.
    for f in "${PLAN_RUNTIME_BINARIES[@]}"; do
        if [ "$do_it" = "1" ]; then
            rm -f -- "$f"
            log "removed: $f"
        else
            log "[dry-run] would remove: $f"
        fi
    done

    # 8. Runtime locks / sockets / logs.
    for f in "${PLAN_RUNTIME_LOCKS[@]}"; do
        if [ "$do_it" = "1" ]; then
            rm -rf -- "$f" 2>/dev/null || warn "could not fully remove $f"
            log "removed: $f"
        else
            log "[dry-run] would remove: $f"
        fi
    done

    # 9. Persistent state directories (--purge / --shred).
    if [ "$MODE" != "default" ]; then
        for f in "${PLAN_PURGE_DIRS[@]}"; do
            if [ "$do_it" = "1" ]; then
                rm -rf -- "$f" 2>/dev/null || warn "could not fully remove $f"
                log "removed: $f"
            else
                log "[dry-run] would remove: $f"
            fi
        done
    fi
}

# ---------- confirmation ----------

require_confirmation() {
    local prompt="$1"
    local expected="$2"
    if [ "${MPM_UNINSTALL_STAGED:-0}" = "1" ]; then
        log "confirmation skipped (staged re-exec)"
        return 0
    fi
    if [ "$ASSUME_YES" = "1" ]; then
        log "confirmation skipped (--yes)"
        return 0
    fi
    if [ ! -t 0 ]; then
        die "destructive operation requires interactive confirmation; use --yes or --dry-run" 5
    fi
    printf '%s\n' "$prompt" >&2
    printf '> ' >&2
    local ans
    if ! IFS= read -r ans; then
        die "no confirmation received" 5
    fi
    if [ "$ans" != "$expected" ]; then
        die "confirmation failed (expected: $expected)" 5
    fi
}

# ---------- self-removal staging ----------

# When --purge/--shred is run from inside an install tree that may be
# recursively removed (e.g. the repo was cloned to ~/.mpm and PROJECT_ROOT
# is /home/<u>/.mpm itself), we cannot safely continue while our own
# source tree is being deleted. Stage a copy of this script to a secure
# temp location, exec it, and clean up on exit.
#
# IMPORTANT: this must be called AFTER confirmation but BEFORE the
# destructive apply. The exec re-runs main(), but we set MPM_UNINSTALL_STAGED
# so the re-executed process skips confirmation.
stage_self_if_needed() {
    # Only stage for destructive modes that touch the install tree.
    [ "$MODE" = "purge" ] || [ "$MODE" = "shred" ] || return 0
    [ "$DRY_RUN" = "0" ] || return 0

    # Normalise PROJECT_ROOT and PREFIX to absolute canonical form.
    # Use realpath -m (no-symlinks) so we follow the symlink and check
    # the canonical underlying path.
    local canonical_root canonical_prefix
    canonical_root="$(realpath "$PROJECT_ROOT" 2>/dev/null || realpath -m -- "$PROJECT_ROOT" 2>/dev/null || echo "$PROJECT_ROOT")"
    canonical_prefix="$(realpath "$PREFIX" 2>/dev/null || realpath -m -- "$PREFIX" 2>/dev/null || echo "$PREFIX")"

    # If PROJECT_ROOT is a descendant of PREFIX (or equal), our source
    # tree is in the destructive path.
    case "$canonical_root" in
        "$canonical_prefix")       ;;   # equal => stage
        "$canonical_prefix"/*)     ;;   # descendant of prefix => stage
        *)
            return 0
            ;;
    esac

    # Stage.
    local stage_dir
    stage_dir="$(mktemp -d -t mpm-uninstall-stage.XXXXXXXXXX)" || die "could not create stage dir" 8
    chmod 0700 "$stage_dir" || true
    local staged="$stage_dir/uninstall.sh"
    if ! cp -p -- "$SCRIPT_PATH" "$staged"; then
        rm -rf -- "$stage_dir"
        die "could not stage uninstaller to $staged" 8
    fi
    chmod 0755 "$staged" || true
    log "staged uninstaller to $staged (PROJECT_ROOT is inside destructive path)"
    log "re-executing staged copy (skipping confirmation)..."
    # Exec the staged copy. MPM_UNINSTALL_STAGED=1 tells the re-executed
    # process that confirmation is already done. MPM_UNINSTALL_STAGED_AT
    # marks the staged path so we can clean it up after apply.
    exec env MPM_UNINSTALL_STAGED=1 \
        MPM_UNINSTALL_STAGED_AT="$staged" \
        "$staged" "$@"
}

# ---------- main ----------

main() {
    # Refuse --purge --shred as redundant/ambiguous. (The early
    # conflict check above the while-loop already exits non-zero
    # when both flags are present.)
    if [ "$MODE" = "purge" ] && [ "$SHRED_SEEN" = "1" ]; then
        die "--purge and --shred are mutually exclusive; choose one" 2
    fi

    # Build the plan first; this also performs path validation.
    build_plan

    # Always print the plan.
    print_plan

    if [ "$DRY_RUN" = "1" ]; then
        log "dry-run: no mutations performed"
        exit 0
    fi

    # Confirm for destructive modes BEFORE staging-then-exec, so the
    # user only confirms once.
    case "$MODE" in
        purge)
            require_confirmation "This will permanently delete all MPM persistent state.

Type PURGE MPM to continue:" \
                                 "PURGE MPM"
            log "PURGE confirmation received"
            ;;
        shred)
            require_confirmation "$(cat <<EOF
This will permanently destroy all MPM memories, directives,
decisions, theories, lessons, evidence, work state, handoffs,
database backups, telemetry data, and MPM-owned logs.

Best-effort secure overwrite will be attempted before deletion.

Type SHRED MPM to continue:
EOF
                                 )" \
                                 "SHRED MPM"
            log "SHRED confirmation received"
            ;;
    esac

    # Now that the user has confirmed, stage-then-exec if our source
    # tree is inside the destructive path. After re-exec the new
    # process will skip confirmation because we set the staged marker.
    if [ "$MODE" = "purge" ] || [ "$MODE" = "shred" ]; then
        if [ -z "${MPM_UNINSTALL_STAGED:-}" ]; then
            stage_self_if_needed "${ORIGINAL_ARGS[@]}"
        fi
    fi

    apply_plan

    log "uninstall complete"
    log "host integrations are not modified by the substrate uninstaller."
    log "Remove or refresh them using the corresponding agent_installation adapter."

    if [ -n "${MPM_UNINSTALL_STAGED_AT:-}" ]; then
        # We were re-executed from a staged copy. Clean up.
        rm -rf -- "${MPM_UNINSTALL_STAGED_AT}" 2>/dev/null || true
        log "removed staged copy at ${MPM_UNINSTALL_STAGED_AT}"
    fi
}

main "$@"
