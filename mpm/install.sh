#!/usr/bin/env bash
#
# install.sh — MPM user-space install (alpha baseline)
#
# Provisions the MPM cognitive substrate in a single user's context,
# no root required. This is the only install path; the legacy
# /var/lib/mpm + system systemd mode has been removed.
#
# Ownership boundary (host-agnostic contract, enforced since 2026-09-16):
#   This script owns the MPM substrate ONLY. It is intentionally
#   host/framework agnostic: it does NOT detect, invoke, register
#   with, or restart any agent framework (OpenClaw, Claude Code,
#   OpenCode, Pi, Hermes, …). Framework-specific MCP / plugin
#   wiring lives exclusively under agent_installation/ and is
#   installed by each host's own installer. See AUTO_AGENT_INSTALL.md
#   for framework discovery and per-host dispatch.
#
# What this script does:
#   1. Builds binaries (mpm, mpm-mcp, mpm-scheduler, mpm-critic, mpm-telemetry)
#   2. Installs binaries to $HOME/.mpm/bin/ (canonical — same root as data)
#   3. Writes a workspace-setting wrapper at $HOME/.mpm/bin/mpm
#   4. Creates $HOME/.mpm/ as the runtime data root (0700/0600 enforced
#      by the binary's startup gate)
#   5. Installs and enables the USER-level systemd service at
#      $HOME/.config/systemd/user/mpm-scheduler.service
#   6. Enables systemd user lingering (loginctl enable-linger) so the
#      scheduler survives logout
#   7. Creates ~/.local/bin and symlinks `mpm` and `mpm-mcp` into it
#      (so subprocesses that inherit the user PATH can resolve them).
#      Internal daemons (mpm-scheduler, mpm-critic, mpm-telemetry) are
#      NOT exposed on PATH — they live only at $PREFIX/bin/ and are
#      invoked by the scheduler / systemd, never directly by the user.
#   8. Warns (read-only) if legacy data exists at /var/lib/mpm/mpm.db —
#      operator must migrate manually if they want to keep it. The
#      script NEVER touches /var/lib/mpm, NEVER invokes sudo, and NEVER
#      tears down a legacy system unit. The legacy `--system` install
#      path has been removed.
#   9. Validates the substrate end-to-end (mpm health_check, scheduler
#      service, lock file, directives count).
#  10. On ecryptfs encrypted homes (linger + default.target invisibility),
#      installs an XDG autostart entry ~/.config/autostart/mpm-post-decrypt.desktop
#      that runs after login/decrypt: daemon-reload + start scheduler (and
#      telemetry if present), plus graphical-session.target Wants as secondary.
#
# Migration guidance: framework-specific setup (OpenClaw MCP, Claude
# Code instructions, OpenCode plugin, etc.) belongs under
# agent_installation/<host>-mpm/ and is installed by that host's
# own installer.
#
# Multi-tenant / multi-user safety:
#   - No root required for the default flow; everything lives in $HOME
#   - Runtime data perms are 0700/0600 (enforced by the binary at startup,
#     not by this script)
#   - No global state mutations outside $HOME
#   - Single canonical install location per user ($HOME/.mpm) — no PATH
#     ordering, no /usr/local copies, no XDG split, no drift between
#     shells
#   - The PATH surface is exactly two entries: ~/.local/bin/mpm and
#     ~/.local/bin/mpm-mcp. Internal daemons never appear there.
#
# Usage:
#   ./mpm/install.sh              # full user-space install (no sudo)
#   ./mpm/install.sh --check      # preflight only (no changes)
#   ./mpm/install.sh --dry-run    # print intended actions
#   ./mpm/install.sh --validate   # post-install check
#   ./mpm/install.sh --uninstall  # remove installed artifacts
#
# (The dedicated uninstaller at ./mpm/uninstall.sh is the canonical
# removal entry point — it supports --dry-run / --purge / --shred.
# This script keeps --uninstall as a thin alias for backward
# compatibility.)
#
# Environment overrides:
#   PREFIX       Install prefix (default: $HOME/.mpm)
#   DATA_ROOT    Runtime data root (default: $HOME/.mpm)
#   USER_NAME    Target user (default: current user)
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
# install.sh now lives at <repo>/mpm/install.sh — its parent is the
# repository root. Derive REPO_ROOT explicitly rather than equating it
# to SCRIPT_DIR, so paths like <repo>/agent_installation resolve correctly.
readonly REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
# PROJECT_ROOT stays equal to SCRIPT_DIR (the installer's working
# directory) for backward-compatibility with code that has historically
# referred to $PROJECT_ROOT for build/script locations — but for new
# callers prefer $REPO_ROOT for repo-wide paths.
readonly PROJECT_ROOT="$REPO_ROOT"
readonly SERVICE_NAME="mpm-scheduler"
# Read-only migration warning target. The script NEVER writes here and
# NEVER invokes sudo. Legacy data migration is the operator's job.
readonly LEGACY_DATA="/var/lib/mpm"
readonly LOG_PREFIX="[mpm-install]"
# User PATH surface for the two binaries subprocesses should resolve by name.
# Internal daemons (mpm-scheduler, mpm-critic, mpm-telemetry) are NOT
# symlinked here — they live only at $PREFIX/bin/ and are invoked by
# systemd, never directly by the user. This is the post-install fix
# against PATH corruption: a minimal, predictable surface.
readonly LOCAL_BIN="${HOME}/.local/bin"

# ---------- mutable state (set by parse_args / preflight) ----------
MODE="install"
ASSUME_YES=0
USER_NAME="${SUDO_USER:-${USER:-$(id -un)}}"

# ---------- paths (always user-space; resolved by resolve_paths) ----------
PREFIX=""
DATA_ROOT=""
SERVICE_DST=""

# ---------- logging ----------
log()  { printf '%s %s\n' "$LOG_PREFIX" "$*" >&2; }
warn() { printf '%s WARN: %s\n' "$LOG_PREFIX" "$*" >&2; }
err()  { printf '%s ERROR: %s\n' "$LOG_PREFIX" "$*" >&2; }
die()  { err "$1"; exit "${2:-1}"; }
note() { printf '\n%s ===== %s =====\n' "$LOG_PREFIX" "$*" >&2; }

# ---------- helpers ----------
resolve_user_home() {
    if [ -z "$USER_NAME" ]; then
        die "cannot determine target user" 1
    fi
    if ! id "$USER_NAME" >/dev/null 2>&1; then
        die "user '$USER_NAME' does not exist" 1
    fi
}

# Resolve PREFIX / DATA_ROOT / SERVICE_DST permanently to user space.
# Called after parse_args.
#
# PREFIX and DATA_ROOT both default to $HOME/.mpm — i.e. PREFIX/bin and
# DATA_ROOT are siblings under the same root. Single canonical install
# location. No PATH ordering. No drift. The legacy `--system` mode
# (which wrote to /usr/local + /var/lib/mpm) has been removed.
resolve_paths() {
    PREFIX="${PREFIX:-$HOME/.mpm}"
    DATA_ROOT="${DATA_ROOT:-$HOME/.mpm}"
    SERVICE_DST="$HOME/.config/systemd/user/${SERVICE_NAME}.service"
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

# Detect the init system. systemd is the only supported init; the
# legacy `--system` install path that wrote to /etc/systemd/system
# has been removed (see AUTO_AGENT_INSTALL.md for host-specific
# wiring; this script owns the MPM substrate only).
detect_init_system() {
    if command -v systemctl >/dev/null 2>&1 && \
       ([ -d /run/systemd/system ] || [ -d /run/user/"$(id -u)"/systemd ]); then
        echo "systemd"
    else
        echo "none"
    fi
}

# ---------- ecryptfs detection ----------
# Ubuntu encrypted home (ecryptfs) + linger is incompatible: user@UID
# starts at boot via linger (08:01) before PAM unwraps ecryptfs on
# login (08:17). Unit files in ~/.config/systemd/user/ are inside the
# encrypted tree and are invisible until decrypt. default.target is
# reached at 08:01 before they appear, so Wants= are never evaluated
# and Restart= does not help. Fix: post-decrypt autostart that does
# daemon-reload + start after login. See mpm-scheduler.service.user
# comment + commit 14ac32b. Detects via mount, findmnt, and
# /home/.ecryptfs marker.
is_ecryptfs_home() {
    if mount 2>/dev/null | grep -q "on ${HOME} type ecryptfs"; then
        return 0
    fi
    if findmnt -n -o FSTYPE "${HOME}" 2>/dev/null | grep -q ecryptfs; then
        return 0
    fi
    if [ -d "/home/.ecryptfs/${USER_NAME}" ] || [ -d "${HOME}/.ecryptfs" ]; then
        # marker exists but mount may not be active (e.g. after logout) — treat as ecryptfs host
        if mount 2>/dev/null | grep -q "ecryptfs"; then
            return 0
        fi
    fi
    return 1
}

# ---------- legacy detection (READ-ONLY) ----------
# For alpha testers upgrading from the old /var/lib/mpm + system
# systemd install. The script NEVER touches legacy paths and NEVER
# invokes sudo. We only WARN if legacy data exists, and print the
# manual migration recipe. The operator must execute the migration
# themselves; this script's contract is "do no harm to legacy state."
detect_legacy() {
    local legacy_data_present=0
    [ -f "$LEGACY_DATA/src/db/mpm.db" ] && legacy_data_present=1

    if [ $legacy_data_present -eq 1 ]; then
        warn "════════════════════════════════════════════════════════════════"
        warn "LEGACY DATA DETECTED (read-only)"
        warn "  $LEGACY_DATA/src/db/mpm.db exists from an old install"
        warn "  this new install creates a FRESH database at $DATA_ROOT"
        warn "  if you want to preserve your old memories/decisions/theories,"
        warn "  migrate manually AFTER this install completes:"
        warn "    sudo systemctl stop $SERVICE_NAME        # stop the legacy unit"
        warn "    sudo cp -a $LEGACY_DATA/src/db/mpm.db* $DATA_ROOT/src/db/"
        warn "    sudo chown -R \$USER:\$USER $DATA_ROOT"
        warn "    sudo systemctl disable --now $SERVICE_NAME   # remove legacy unit"
        warn ""
        warn "  This installer will not run any of these commands. Migration"
        warn "  is your responsibility. The legacy --system mode has been"
        warn "  removed; MPM is user-space only as of this release."
        warn "════════════════════════════════════════════════════════════════"
    fi
}

# ---------- state inspection ----------
inspect_existing_state() {
    local has_data=0 has_service=0 has_binaries=0 has_wrapper=0 has_user_unit=0
    [ -f "$DATA_ROOT/src/db/mpm.db" ] && has_data=1
    [ -f "$SERVICE_DST" ] && has_service=1
    [ -x "$PREFIX/bin/mpm-scheduler" ] && has_binaries=1
    [ -f "$PREFIX/bin/mpm" ] && has_wrapper=1
    [ -f "$HOME/.config/systemd/user/${SERVICE_NAME}.service" ] && has_user_unit=1

    log "install mode:    user-space (no sudo, no /var/lib/mpm)"
    log "existing install state:"
    log "  data dir:      $([ -d "$DATA_ROOT" ] && echo present || echo absent)"
    log "  database:      $([ $has_data -eq 1 ] && echo present || echo absent)"
    log "  service unit:  $([ $has_service -eq 1 ] && echo installed || echo absent)"
    log "  binaries:      $([ $has_binaries -eq 1 ] && echo present || echo absent)"
    log "  mpm wrapper:   $([ $has_wrapper -eq 1 ] && echo present || echo absent)"
    log "  user unit:     $([ $has_user_unit -eq 1 ] && echo present || echo absent)"
}

# ---------- preflight ----------
# Resolve go consistently with the Makefile (Makefile:53): prefer
# `$PATH`, fall back to the canonical Go install location. Fresh
# Linux Mint / Ubuntu installs and many CI images have Go extracted
# to /usr/local/go but the directory isn't on PATH for non-login
# shells. Requiring the user to mutate PATH just to run the
# installer would be hostile; mirror the Makefile's resolution
# instead.
GO_BIN=""
if command -v go >/dev/null 2>&1; then
    GO_BIN="$(command -v go)"
elif [ -x /usr/local/go/bin/go ]; then
    GO_BIN="/usr/local/go/bin/go"
fi
export GO_BIN

check_prereqs() {
    log "checking prerequisites..."
    [ -n "$GO_BIN" ] || die "Go not found in PATH or at /usr/local/go/bin/go" 1
    command -v systemctl >/dev/null 2>&1 || die "systemctl not found (systemd required)" 1
    [ -d "$PROJECT_ROOT" ] || die "project root not found: $PROJECT_ROOT" 1
    [ -f "$PROJECT_ROOT/Makefile" ] || die "Makefile not found in $PROJECT_ROOT" 1
    log "  ✓ go:       $GO_BIN"
    log "  ✓ systemd:  present"
    log "  ✓ project:  $PROJECT_ROOT"
}

preflight() {
    note "PREFLIGHT"
    resolve_user_home
    log "target user:  $USER_NAME"
    log "project:      $PROJECT_ROOT"
    log "prefix:       $PREFIX"
    log "data root:    $DATA_ROOT"
    log "install mode: USER-SPACE (no sudo required)"
    log "  systemd unit: $SERVICE_DST"

    local init_sys
    init_sys=$(detect_init_system)
    log "init system:  $init_sys"
    [ "$init_sys" = "systemd" ] || die "systemd required (got: $init_sys)" 1

    local os
    os=$(detect_os)
    log "OS:           $os (tested on Ubuntu 24.04)"

    check_prereqs
    detect_legacy
    inspect_existing_state
    log "preflight ok"
}

# ---------- phases ----------
phase_build() {
    note "BUILD"
    cd "$PROJECT_ROOT"

    # Idempotency fix for the canonical install layout where the repo lives
    # at $HOME/.mpm (i.e. $PREFIX). In that case $PROJECT_ROOT/bin/mpm and
    # $PREFIX/bin/mpm are the same file, and the wrapper written by a
    # previous install is now sitting at bin/mpm. `go build -o bin/mpm`
    # refuses to overwrite a non-object file with that name, which would
    # abort the installer at the build step on re-run. The actual binary
    # content is preserved at bin/mpm.real (the same-prefix idempotency
    # note in phase_binaries documents why); remove the stale wrapper so
    # `make build` can write a fresh ELF. Other binaries (mpm-mcp,
    # mpm-scheduler, mpm-critic, mpm-telemetry) have no wrapper and
    # re-build cleanly.
    if [ "$PROJECT_ROOT/bin/mpm" -ef "$PREFIX/bin/mpm" ] \
       && [ -f "$PROJECT_ROOT/bin/mpm" ] \
       && [ "$(head -c 2 "$PROJECT_ROOT/bin/mpm" 2>/dev/null || true)" = "#!" ]; then
        log "  removing stale wrapper at $PROJECT_ROOT/bin/mpm (real binary preserved at mpm.real)"
        rm -f "$PROJECT_ROOT/bin/mpm"
    fi

    if ! make build; then
        die "make build failed" 2
    fi
    for bin in mpm mpm-mcp mpm-scheduler mpm-critic mpm-telemetry; do
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

    # Daemon binaries (raw ELF, owned by current user in user mode).
    #
    # When the user cloned the repository directly to $HOME/.mpm — the
    # canonical install prefix — PROJECT_ROOT/bin/$bin and PREFIX/bin/$bin
    # resolve to the SAME file. Coreutils' install(1) refuses to copy a
    # file onto itself and ``set -e`` would abort the installer before
    # the wrapper is written or any later phase runs. ``[ -ef ]`` is a
    # POSIX test that returns true when both paths refer to the same
    # inode (handles direct equality AND symlink resolution), which is
    # the right notion of "same file" for this case. If source and dest
    # are the same file, the binary is already at the install target —
    # nothing to do.
    for bin in mpm-scheduler mpm-critic mpm-mcp mpm-telemetry; do
        local src="$PROJECT_ROOT/bin/$bin"
        local dst="$PREFIX/bin/$bin"
        if [ "$src" -ef "$dst" ]; then
            log "  $dst is build output (same file as $src) — skipping copy"
        else
            install -m 0755 "$src" "$dst"
            log "  installed $dst"
        fi
    done

    # Real mpm binary (renamed to .real so the wrapper can claim the
    # canonical name). ``mpm`` and ``mpm.real`` are different filenames
    # so install(1) is happy even when PROJECT_ROOT == PREFIX — but we
    # still guard with ``-ef`` to be safe against the (unlikely) case
    # of an existing ``mpm.real`` symlink resolving to the source.
    #
    # Idempotency note: in the same-prefix case the wrapper written by
    # a previous install overwrites ``$PROJECT_ROOT/bin/mpm``. Re-running
    # the installer would then copy the WRAPPER (not the real binary)
    # to ``.real``. Detect a wrapper at the source and skip — the
    # existing ``.real`` from the prior install is still correct.
    local real_src="$PROJECT_ROOT/bin/mpm"
    local real_dst="$PREFIX/bin/mpm.real"
    if [ "$real_src" -ef "$real_dst" ]; then
        log "  $real_dst is build output (same file as $real_src) — skipping copy"
    elif [ -f "$real_dst" ] && [ "$(head -c 2 "$real_src" 2>/dev/null || true)" = "#!" ]; then
        log "  $real_src is already a wrapper — preserving existing $real_dst"
    else
        install -m 0755 "$real_src" "$real_dst"
        log "  installed $real_dst"
    fi

    # Wrapper: sets MPM_WORKSPACE then exec's the real binary.
    #
    # Wrapper ordering invariant: ``mpm.real`` MUST exist at this path
    # before we overwrite ``mpm`` with the wrapper, otherwise the wrapper
    # would exec a non-existent binary. The install above already
    # created ``mpm.real``; do not move the wrapper write before it.
    #
    # In the same-prefix case, backing up the existing ``mpm`` (which is
    # the build's real binary) before overwriting it would just create
    # a useless ``mpm.pre-wrapper.*`` sidecar — the binary's content is
    # already preserved as ``mpm.real``. Skip the backup to keep the
    # install layout tidy.
    if [ ! "$PROJECT_ROOT/bin/mpm" -ef "$PREFIX/bin/mpm" ]; then
        backup_raw_binary_if_present "$PREFIX/bin/mpm" >/dev/null
    else
        log "  source tree at $PREFIX/bin/mpm is the build output — wrapper will replace it directly"
    fi

    cat > "$PREFIX/bin/mpm" <<WRAPPER
#!/bin/sh
# mpm CLI wrapper — installed by mpm/install.sh
# Routes CLI to the per-user workspace regardless of CWD.
# Override at invocation: MPM_WORKSPACE=/tmp/foo mpm call …
exec env MPM_WORKSPACE=\${MPM_WORKSPACE:-${DATA_ROOT}} ${PREFIX}/bin/mpm.real "\$@"
WRAPPER
    chmod 0755 "$PREFIX/bin/mpm"
    log "  installed wrapper $PREFIX/bin/mpm -> $PREFIX/bin/mpm.real"
}

# Symlink mpm + mpm-mcp into ~/.local/bin so subprocesses that inherit
# the user PATH can resolve them by name.
#
# Strictly limited to user-invokable binaries:
#   - mpm       (CLI)
#   - mpm-mcp   (MCP stdio server, spawned by Claude Code / OpenClaw)
#
# Internal daemons are NOT symlinked:
#   - mpm-scheduler   (invoked only by systemd --user)
#   - mpm-critic      (invoked only by mpm-scheduler via fork+exec)
#   - mpm-telemetry   (invoked only by systemd --user)
# Putting those on PATH would invite accidental direct invocation and
# drift; keeping them inside $PREFIX/bin/ is the documented boundary.
#
# ~/.local/bin is the freedesktop.org standard user-PATH location and
# is on PATH by default for almost every modern desktop Linux shell
# (bash, zsh, fish). It's preferred over ~/.mpm/bin because the latter
# is a tool-specific install root, not a generic PATH surface.
phase_symlinks() {
    note "USER PATH SYMLINKS"
    install -d -m 0755 "$LOCAL_BIN"

    # Symlink mpm and mpm-mcp only. Replace any existing symlink/file
    # at the target (the legacy installer may have left stale entries).
    for name in mpm mpm-mcp; do
        local target="$LOCAL_BIN/$name"
        local source="$PREFIX/bin/$name"
        if [ ! -e "$source" ]; then
            die "expected $source to exist (phase_binaries must run first)" 3
        fi
        rm -f "$target"
        ln -s "$source" "$target"
        log "  $target -> $source"
    done

    # Warn (non-fatal) if ~/.local/bin is not on the user's current PATH.
    # We don't error because (a) some shells source PATH lazily, and
    # (b) many agents spawn subprocesses with an explicit PATH that
    # already includes ~/.local/bin. The warning is just a heads-up.
    #
    # 2026-09-16 fresh-profile fix: pre-fix wording claimed "new shells
    # will pick it up automatically (XDG default)". That is empirically
    # inaccurate. ~/.profile conditionally adds ~/.local/bin to PATH,
    # but ordinary new terminal windows inside an existing graphical
    # login inherit the desktop environment and do NOT process
    # .profile. So `bash -ic 'command -v mpm'` fails on a fresh
    # install while `bash -lc 'command -v mpm'` succeeds. The accurate
    # framing is: a new LOGIN session will normally pick it up
    # (because that's when the shell sources .profile); for the
    # current shell, the operator must either export PATH or invoke
    # the canonical binary path directly. The installer never modifies
    # the parent shell environment.
    case ":${PATH:-}:" in
        *":$LOCAL_BIN:"*) log "  $LOCAL_BIN is on PATH (good)" ;;
        *)
            warn "  $LOCAL_BIN is NOT on your current PATH"
            warn "  a new login session will normally pick it up automatically"
            warn "  for this shell, use:"
            warn "    export PATH=\"\$HOME/.local/bin:\$PATH\""
            warn "  or invoke:"
            warn "    $PREFIX/bin/mpm"
            ;;
    esac

    # PATH-shadow detection (2026-09-14 release-pass). If `command -v mpm`
    # resolves to a path other than the canonical $PREFIX/bin/mpm
    # (via the symlink chain $LOCAL_BIN/mpm -> $PREFIX/bin/mpm), warn
    # the operator. We compare canonicalized paths (readlink -f) so the
    # check survives symlink chains. The shadowing binary is NEVER
    # removed automatically — the operator decides what to do. (A stale
    # repo-root `./mpm` binary was the most common offender; see
    # install.sh history.)
    if command -v mpm >/dev/null 2>&1; then
        local resolved
        resolved=$(command -v mpm 2>/dev/null) || true
        if [ -n "$resolved" ] && command -v readlink >/dev/null 2>&1; then
            local canonical_resolved canonical_expected
            canonical_resolved=$(readlink -f "$resolved" 2>/dev/null) || canonical_resolved="$resolved"
            canonical_expected=$(readlink -f "$PREFIX/bin/mpm" 2>/dev/null) || canonical_expected="$PREFIX/bin/mpm"
            if [ "$canonical_resolved" != "$canonical_expected" ]; then
                warn "  PATH-shadow detected: \`command -v mpm\` -> $canonical_resolved"
                warn "  expected canonical install:                 $canonical_expected"
                warn "  the shadowing executable will NOT be removed automatically."
                warn "  resolve by fixing your shell PATH (remove the shadowing directory)"
                warn "  or replacing the binary at the shadowing path."
            else
                log "  \`command -v mpm\` -> $canonical_resolved  (canonical)"
            fi
        fi
    fi
}

phase_data_dir() {
    note "DATA DIRECTORY"
    # Runtime perms enforced by the binary at startup (AssertUserDirPerms0700
    # + TightenFilePerms0600 sweep on the data root). Here we create the
    # structure at 0700 so the disclosure surface is closed at install time
    # — the runtime sweep is defence in depth, not the primary gate.
    #
    # Both `src/db` (the SQLite database + WAL/SHM sidecars + audit
    # mirror) and `backups/` (the backup tree, including `critic-pre`
    # snapshot dumps) hold confidential cognitive data per
    # `docs/SECURITY.md` §"A note specifically about memory contents".
    # 0755 would let other local users enumerate the database filename
    # and probe sidecars even when the parent data root is 0700.
    #
    # The chmod after install -d mirrors the canonical blobstore
    # pattern (`internal/blobstore/fs.go` MkdirAll + os.Chmod): a
    # pre-existing permissive directory is corrected on re-install,
    # so idempotence does not preserve an insecure state.
    install -d -m 0700 "$DATA_ROOT/src/db" "$DATA_ROOT/backups/critic-pre"
    chmod 0700 "$DATA_ROOT/src/db" "$DATA_ROOT/backups/critic-pre"
    log "  created $DATA_ROOT/{src/db,backups/critic-pre} (mode 0700)"

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

    # USER-level unit only. The legacy SYSTEM-level template has been
    # removed from contrib/systemd/ — only one install path exists.
    local service_src="${PROJECT_ROOT}/contrib/systemd/${SERVICE_NAME}.service.user"

    install -m 0644 "$service_src" "$SERVICE_DST"
    log "  installed $SERVICE_DST"

    # User-space service. Two extra concerns vs the legacy system mode:
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

    # Encrypted home fix: user@UID with linger starts before ecryptfs decrypt,
    # so default.target is reached before ~/.config/systemd/user/ is visible.
    # Install an XDG autostart entry that runs after login/decrypt and does
    # daemon-reload + start. Idempotent — safe to re-run.
    phase_ecryptfs_autostart
}

phase_ecryptfs_autostart() {
    if ! is_ecryptfs_home; then
        return 0
    fi
    note "ECRYPTFS POST-DECRYPT WORKAROUND"
    log "encrypted home detected (ecryptfs on $HOME) — installing autostart fix"
    log "  linger + ecryptfs: user@UID starts before decrypt, so"
    log "  default.target (≈08:01) is reached before unit files in"
    log "  ~/.config/systemd/user/ are visible (decrypt at login ≈08:17)."
    log "  Fix: XDG autostart entry that runs after decrypt: daemon-reload + start"
    local autostart_dir="$HOME/.config/autostart"
    local autostart_file="$autostart_dir/mpm-post-decrypt.desktop"
    install -d -m 0755 "$autostart_dir"
    # Build Exec line: always scheduler, plus telemetry if its unit exists
    local exec_cmd="sh -c \"systemctl --user daemon-reload; systemctl --user start mpm-scheduler.service"
    if [ -f "$HOME/.config/systemd/user/mpm-telemetry.service" ]; then
        exec_cmd="$exec_cmd; systemctl --user start mpm-telemetry.service 2>/dev/null || true"
    fi
    exec_cmd="$exec_cmd\""
    cat > "$autostart_file" <<EOF
[Desktop Entry]
Type=Application
Name=MPM Post-Decrypt Reload (ecryptfs fix)
Comment=Reload systemd user manager after ecryptfs decrypt and start MPM scheduler. Installed by mpm install.sh for linger+ecryptfs hosts. See mpm-scheduler.service.user comment and commit 14ac32b.
Exec=$exec_cmd
Hidden=false
NoDisplay=false
X-GNOME-Autostart-enabled=true
X-Cinnamon-Autostart-enabled=true
X-MATE-Autostart-enabled=true
EOF
    chmod 0644 "$autostart_file"
    log "  installed $autostart_file"
    # Also add graphical-session wants as secondary (harmless, requires daemon-reload above to be visible)
    if [ -f "$SERVICE_DST" ]; then
        systemctl --user add-wants graphical-session.target "$SERVICE_NAME" 2>/dev/null || true
        log "  also added Wants=graphical-session.target for $SERVICE_NAME (secondary, needs daemon-reload)"
    fi
    if [ -f "$HOME/.config/systemd/user/mpm-telemetry.service" ]; then
        systemctl --user add-wants graphical-session.target mpm-telemetry.service 2>/dev/null || true
        log "  also added Wants=graphical-session.target for mpm-telemetry.service"
    fi
    systemctl --user daemon-reload 2>/dev/null || true
}


phase_validate() {
    note "VALIDATION"
    local errors=0

    # 1. Service (user scope only — system mode has been removed)
    if systemctl --user is-active --quiet "$SERVICE_NAME"; then
        log "  ✓ systemd service active (user scope)"
    else
        err "  ✗ systemd service NOT active (user scope)"
        errors=$((errors + 1))
    fi

    # 2. CLI wrapper (uses wrapper which sets MPM_WORKSPACE)
    if "$PREFIX/bin/mpm" call mpm_system --payload '{"action":"health_check"}' 2>/dev/null \
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

    # 4. Directives seeded (warn-only — install does not auto-seed)
    local directives_count=0
    local directives_json
    directives_json=$("$PREFIX/bin/mpm" call mpm_context --payload '{"action":"read_directives","params":{}}' 2>/dev/null \
        | grep -oE '"count":[0-9]+' | head -1 | grep -oE '[0-9]+' || true)
    if [ -n "$directives_json" ]; then
        directives_count="$directives_json"
    fi
    if [ "$directives_count" -gt 0 ]; then
        log "  ✓ directives present ($directives_count)"
    else
        warn "  ! no directives found"
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
    phase_symlinks
    phase_data_dir
    phase_service
    phase_validate
    note "INSTALL COMPLETE"
    # Use the canonical path so the next-steps commands are runnable
    # in the installer's own shell (where ~/.local/bin may not yet
    # be on PATH — see phase_symlinks' on-PATH warning). Defining
    # `cli` once avoids backslash-escape gymnastics inside the log
    # double-quoted strings below.
    local cli="$PREFIX/bin/mpm"
    log "  Mode:       USER-SPACE (no sudo, no /var/lib/mpm)"
    log "  CLI:        $cli (wrapper) -> $PREFIX/bin/mpm.real"
    log "  PATH:       $LOCAL_BIN/mpm + $LOCAL_BIN/mpm-mcp  (via symlinks)"
    log "  Daemon:     $(systemctl --user is-active $SERVICE_NAME) ($SERVICE_DST)"
    log "  Logs:       journalctl --user -u $SERVICE_NAME -f"
    log "  Data root:  $DATA_ROOT"
    log ""
    log "next steps (manual):"
    log "  $cli status                # verify DB reachable"
    log "  $cli call read_wake_context  # first agent tool call"
    log ""
    log "tip: ~/.local/bin/mpm will normally be available after a new login session;"
    log "     until then, use $cli directly."
    log ""
    log "configuration (required for LLM-backed features — synthesis, critic, review):"
    log "  config file:    $DATA_ROOT/mpm_config.json (canonical; checked first)"
    log "  env-file path:  ~/.config/mpm/mpm.env (optional override; systemd unit sources it via EnvironmentFile=-)"
    log "  setup:          mpm config profile add default --model <model> --base-url <url>"
    log "                  mpm config profile set default api_key <key>   # writes 0600 to mpm_config.json"
    log "                  mpm config component set memory default         # bind component to profile"
    log "  the installer did NOT create, store, request, or echo any API key or secret."
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
    log "  install -m 0755 .../bin/mpm-telemetry -> $PREFIX/bin/mpm-telemetry"
    log "  install -m 0755 .../bin/mpm           -> $PREFIX/bin/mpm.real"
    log "  write wrapper $PREFIX/bin/mpm"
    log "  symlink $PREFIX/bin/mpm     -> $LOCAL_BIN/mpm"
    log "  symlink $PREFIX/bin/mpm-mcp -> $LOCAL_BIN/mpm-mcp"
    log "  install -d -m 0700 $DATA_ROOT/src/db $DATA_ROOT/backups/critic-pre  (and chmod 0700 to harden pre-existing dirs)"
    log "  loginctl enable-linger $USER_NAME"
    log "  install -m 0644 .../contrib/systemd/${SERVICE_NAME}.service.user -> $SERVICE_DST"
    log "  systemctl --user daemon-reload && enable --now $SERVICE_NAME"
    if is_ecryptfs_home; then
        log "  ecryptfs detected → install ~/.config/autostart/mpm-post-decrypt.desktop (daemon-reload + start after decrypt)"
        log "  and: systemctl --user add-wants graphical-session.target mpm-scheduler.service (secondary)"
    fi
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

    if [ -f "$SERVICE_DST" ]; then
        systemctl --user disable --now "$SERVICE_NAME" 2>/dev/null || true
        rm -f "$SERVICE_DST"
        systemctl --user daemon-reload 2>/dev/null || true
        log "  removed $SERVICE_DST"
    fi
    # Ecryptfs autostart workaround (installed by phase_ecryptfs_autostart)
    local autostart_file="$HOME/.config/autostart/mpm-post-decrypt.desktop"
    if [ -f "$autostart_file" ]; then
        rm -f "$autostart_file"
        log "  removed $autostart_file (ecryptfs autostart)"
    fi
    # Graphical-session wants added as secondary for ecryptfs hosts
    if [ -L "$HOME/.config/systemd/user/graphical-session.target.wants/mpm-scheduler.service" ]; then
        rm -f "$HOME/.config/systemd/user/graphical-session.target.wants/mpm-scheduler.service"
        log "  removed graphical-session want for mpm-scheduler"
    fi
    if [ -L "$HOME/.config/systemd/user/graphical-session.target.wants/mpm-telemetry.service" ]; then
        rm -f "$HOME/.config/systemd/user/graphical-session.target.wants/mpm-telemetry.service"
        log "  removed graphical-session want for mpm-telemetry"
    fi
    systemctl --user daemon-reload 2>/dev/null || true
    # Note: loginctl enable-linger is intentionally NOT undone —
    # operator may have other user services that benefit.

    # Remove the user-PATH symlinks. Internal daemons (mpm-scheduler,
    # mpm-critic, mpm-telemetry) are NOT symlinked and don't need this.
    for symlink in "$LOCAL_BIN/mpm" "$LOCAL_BIN/mpm-mcp"; do
        if [ -L "$symlink" ] || [ -f "$symlink" ]; then
            rm -f "$symlink"
            log "  removed $symlink"
        fi
    done

    # All five binaries under $PREFIX/bin/. Wrapper + real + daemons.
    for bin in mpm mpm.real mpm-scheduler mpm-critic mpm-mcp mpm-telemetry; do
        if [ -f "$PREFIX/bin/$bin" ]; then
            rm -f "$PREFIX/bin/$bin"
            log "  removed $PREFIX/bin/$bin"
        fi
    done

    log "uninstall complete"
    log "  source code at $PROJECT_ROOT is untouched"
    log "  data at $DATA_ROOT is preserved (remove manually if desired)"
}

# ---------- arg parsing ----------
usage() {
    cat <<USAGE
$SCRIPT_NAME — MPM install (user-space only)

Usage: $SCRIPT_NAME [mode] [options]

Modes (default: install):
  (default)       Full install: preflight, build, install binaries + service
  --check         Preflight only — verify environment, no changes
  --dry-run       Print intended actions, no changes
  --validate      Post-install validation (read-only)
  --uninstall     Remove installed artifacts (data preserved)

Options:
  --prefix <path>      Install prefix (default: \$HOME/.mpm)
  --data-root <path>   Runtime data root (default: \$HOME/.mpm)
  --user <name>        Target user (default: current user)
  --yes                Skip confirmation prompts
  -h, --help           Show this help

Examples:
  $SCRIPT_NAME                  # full user-space install (no sudo)
  $SCRIPT_NAME --check          # environment check (no changes)
  $SCRIPT_NAME --dry-run        # show intended actions
  $SCRIPT_NAME --validate       # verify install
  $SCRIPT_NAME --uninstall      # remove install (data preserved)

Exit codes:
  0  success
  1  preflight failed
  2  build failed
  3  install failed
  4  service start failed
  5  validation failed

Notes:
  - The user-space install requires no root.
  - The legacy --system mode (sudo / /var/lib/mpm / /etc/systemd/system)
    has been removed. If you find a stale legacy unit at
    /etc/systemd/system/mpm-scheduler.service, disable it manually with
    sudo before running this installer; the installer will NOT touch it.
  - This script NEVER invokes sudo and NEVER writes outside \$HOME.
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