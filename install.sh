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
#      into the checkout's .build/bin/ — disposable developer artifacts
#   2. Promotes them to $HOME/.mpm/bin/ (canonical install — same root as
#      data). Promotion is explicit and unconditional; the installed
#      `mpm` IS the compiled binary (no shell wrapper — MPM_WORKSPACE
#      defaulting is handled inside the binary)
#   3. Creates $HOME/.mpm/ as the runtime data root (0700/0600 enforced
#      by the binary's startup gate)
#   4. Installs and enables the USER-level systemd service at
#      $HOME/.config/systemd/user/mpm-scheduler.service
#   5. Enables systemd user lingering (loginctl enable-linger) so the
#      scheduler survives logout
#   6. Creates ~/.local/bin and symlinks `mpm` and `mpm-mcp` into it
#      (so subprocesses that inherit the user PATH can resolve them).
#      Internal daemons (mpm-scheduler, mpm-critic, mpm-telemetry) are
#      NOT exposed on PATH — they live only at $PREFIX/bin/ and are
#      invoked by the scheduler / systemd, never directly by the user.
#   7. Warns (read-only) if legacy data exists at /var/lib/mpm/mpm.db —
#      operator must migrate manually if they want to keep it. The
#      script NEVER touches /var/lib/mpm, NEVER invokes sudo, and NEVER
#      tears down a legacy system unit. The legacy `--system` install
#      path has been removed.
#   8. Validates the substrate end-to-end (mpm health_check, scheduler
#      service, lock file, directives count).
#   9. On ecryptfs encrypted homes (linger + default.target invisibility),
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
#   ./install.sh              # full user-space install (no sudo)
#   ./install.sh --check      # preflight only (no changes)
#   ./install.sh --dry-run    # print intended actions
#   ./install.sh --validate   # post-install check
#   ./install.sh --uninstall  # remove installed artifacts
#
# (The dedicated uninstaller at ./uninstall.sh is the canonical
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
# install.sh lives at the repo root. REPO_ROOT is its own directory,
# which is also the canonical install prefix in production ($HOME/.mpm).
readonly REPO_ROOT="$SCRIPT_DIR"
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
# Absolute path to the Makefile's build output directory, discovered from
# `make print-build-dir` during phase_build. It is deliberately NOT
# hardcoded here: the installer and the Makefile previously each carried
# their own copy of the "bin" literal, and a one-sided edit would point the
# installer at a directory `make build` no longer writes to. Empty until
# phase_build runs, and never defaulted to a guessed path — an unresolved
# value is a hard error (see build_dir).
BUILD_DIR=""

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
#
# build_dir echoes the Makefile's build output directory. It is a function
# rather than a bare "$BUILD_DIR" read so that an unresolved value fails at
# the point of use instead of silently expanding to "/" or "" — the latter
# would turn every subsequent path into "$PROJECT_ROOT//mpm".
build_dir() {
    if [ -z "$BUILD_DIR" ]; then
        die "internal error: build output directory unknown (phase_build did not run?)" 2
    fi
    printf '%s\n' "$BUILD_DIR"
}

phase_build() {
    note "BUILD"
    cd "$PROJECT_ROOT"

    # The Makefile's `build` target itself detects and removes a stale
    # wrapper at the build path before invoking `go build`, so this phase
    # does not need to duplicate that cleanup. Running `make build` here
    # also handles the legacy wrapper case for users who invoke install.sh
    # against a checkout that was previously installed with the older
    # wrapper+real layout.
    #
    # Ask make where it writes BEFORE building, so a missing `print-build-dir`
    # target is reported as a missing target rather than as five confusing
    # "build did not produce X" errors against a path we guessed wrong.
    #
    # Two defences, because this value gates the whole install:
    #
    #   1. `-s --no-print-directory` suppress the noise make emits to
    #      STDOUT when invoked as a recursive sub-make from inside another
    #      recipe ("make[1]: Entering directory '...'") or when not
    #      silent. Those go to stdout, so `2>/dev/null` alone does not
    #      catch them.
    #   2. The sed extracts the first absolute path from anywhere in the
    #      output. This is what makes the install survive GNU make's
    #      dry-run mode: make propagates MAKEFLAGS to sub-makes, so when
    #      the installer runs from inside a recipe under `make -n` (the
    #      build-config guard test does exactly this:
    #      `make -n test` -> test-agent-installation -> install.sh), the
    #      nested `make print-build-dir` does not RUN its recipe and
    #      instead prints the recipe text, e.g.
    #
    #          echo '/tmp/.../.build/bin'
    #
    #      Requiring the output to be exactly one bare path made that
    #      abort with "could not determine build output directory".
    #      Pulling the path out of the echoed form keeps a legitimate
    #      install working while still refusing anything that does not
    #      resolve to an absolute path.
    if ! BUILD_DIR="$(
            _pbd="$(make -s --no-print-directory print-build-dir 2>/dev/null)"
            # Prefer a bare absolute path (the normal case)...
            printf '%s\n' "$_pbd" | sed -n 's/^\(\/[^ '\''"]*\)$/\1/p' | head -1
            # ...else the path quoted inside an echoed recipe.
            if [ "$_pbd" != "$(printf '%s\n' "$_pbd" | sed -n 's/^\(\/[^ '\''"]*\)$/\1/p' | head -1)" ]; then
                printf '%s\n' "$_pbd" | sed -n "s/^echo '\(\/[^']*\)'\$/\\1/p" | head -1
            fi
        )" || [ -z "$BUILD_DIR" ]; then
        die "could not determine build output directory from the Makefile (\`make print-build-dir\`);
this installer deliberately does not guess the path — fix the Makefile or use an
older checkout of install.sh with a matching Makefile." 2
    fi
    case "$BUILD_DIR" in
        /*) ;;
        *) die "make print-build-dir returned a non-absolute path '$BUILD_DIR'" 2 ;;
    esac
    log "build output: $BUILD_DIR"

    if ! make build; then
        die "make build failed" 2
    fi
    for bin in mpm mpm-mcp mpm-scheduler mpm-critic mpm-telemetry; do
        [ -x "$(build_dir)/$bin" ] || die "build did not produce $(build_dir)/$bin" 2
    done

    # The whole point of the build/install split. These are two different
    # directories and must stay that way: writing build output into $PREFIX/bin
    # is what previously let a plain build overwrite the running services.
    # Checked after the build rather than only before, because a symlinked
    # or overridden BUILD_DIR could resolve differently by this point.
    _install_bin="$(cd "$PREFIX" 2>/dev/null && realpath -m "$PREFIX/bin" || printf '%s' "$PREFIX/bin")"
    if [ "$(realpath -m "$BUILD_DIR")" = "$_install_bin" ]; then
        die "build output ($BUILD_DIR) and install target ($_install_bin) are the same
directory. Refusing to continue: promotion must copy distinct files, and an
ordinary build must never write into the directory the running services execute
from. Set BUILD_DIR to a scratch subdirectory of the checkout." 2
    fi

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

    # Migrate any pre-existing wrapper+real layout from older installs.
    #
    # Historical installs wrote a shell wrapper to $PREFIX/bin/mpm and the
    # compiled binary to $PREFIX/bin/mpm.real. The wrapper is no longer
    # needed — the Go binary itself defaults MPM_WORKSPACE to $HOME/.mpm
    # via internal/core/config.GetMPMDir() when the env var is unset, so
    # wrapping the binary in a shell shim is redundant. Removing the
    # wrapper also restores a clean separation between the Makefile-owned
    # build artifact at $PREFIX/bin/mpm (which must remain an ELF) and
    # any installation-only files. After this migration block, the layout
    # is: $PREFIX/bin/<bin> = compiled Go binary, nothing else.
    if [ -f "$PREFIX/bin/mpm.real" ] || [ -f "$PREFIX/bin/mpm.pre-wrapper."* ] 2>/dev/null; then
        log "  removing legacy wrapper artifacts from prior install"
        rm -f "$PREFIX/bin/mpm.real"
        rm -f "$PREFIX/bin/mpm.pre-wrapper."*
    fi

    # Daemon binaries (raw ELF, owned by current user in user mode).
    #
    # The source is the Makefile's build output directory, discovered in
    # phase_build via `make print-build-dir`. It is a scratch subdirectory
    # of the checkout and is DISTINCT from $PREFIX/bin even when the
    # checkout is $PREFIX itself — the canonical install.
    #
    # ``mpm`` itself is installed here too (no separate .real or wrapper).
    # The Go binary handles MPM_WORKSPACE defaulting internally, so there
    # is no reason for the installer to write a non-ELF anywhere.
    #
    # SAME-FILE IS AN ERROR, NOT A SKIP. This loop used to treat
    # ``[ "$src" -ef "$dst" ]`` as "already installed, skip" — a guard
    # that existed only because source and destination used to be the same
    # inode whenever the checkout WAS the prefix. Now that they are two
    # distinct paths, reaching that state means the build/install split has
    # been violated by something (a symlinked BUILD_DIR, a stale
    # configuration), and the correct response is to stop. Skipping would
    # report a successful install of a binary that was never copied, which
    # is precisely the "success printed over a partial install" failure
    # mode this installer already guards against elsewhere.
    for bin in mpm mpm-scheduler mpm-critic mpm-mcp mpm-telemetry; do
        local src="$(build_dir)/$bin"
        local dst="$PREFIX/bin/$bin"
        if [ ! -f "$src" ]; then
            die "build did not produce $src (run make build first)" 2
        fi
        if [ "$src" -ef "$dst" ]; then
            die "refusing to install $bin: build output and install target are the same file
  src: $src
  dst: $dst
Build output must be a scratch subdirectory of the checkout, separate from the
install prefix. Something has aliased them (symlinked BUILD_DIR, or a checkout
whose .build/ points at the install) — fix that rather than letting the
installer report a deployment that did not happen." 2
        fi
        install -m 0755 "$src" "$dst"
        log "  installed $dst"
    done
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
    # Track per-path state so the log distinguishes newly-created
    # directories from pre-existing ones (truthful output).
    #
    # Pre-existence is decided BEFORE `install -d` runs, by inspecting
    # the inode (or any kind of entry) at the target path. We do NOT
    # infer creation from the directory existing AFTER `install -d`,
    # because `install -d` is idempotent — running it on an existing
    # directory must not be misreported as a creation.
    _preexisted_src_db=0
    [ -e "$DATA_ROOT/src/db" ] && _preexisted_src_db=1
    install -d -m 0700 "$DATA_ROOT/src/db"
    chmod 0700 "$DATA_ROOT/src/db"
    if [ "$_preexisted_src_db" = "1" ]; then
        log "  verified existing $DATA_ROOT/src/db (mode 0700)"
    else
        log "  created $DATA_ROOT/src/db (mode 0700)"
    fi
    _preexisted_backups=0
    [ -e "$DATA_ROOT/backups/critic-pre" ] && _preexisted_backups=1
    install -d -m 0700 "$DATA_ROOT/backups/critic-pre"
    chmod 0700 "$DATA_ROOT/backups/critic-pre"
    if [ "$_preexisted_backups" = "1" ]; then
        log "  verified existing $DATA_ROOT/backups/critic-pre (mode 0700)"
    else
        log "  created $DATA_ROOT/backups/critic-pre (mode 0700)"
    fi
    unset _preexisted_src_db _preexisted_backups

    if [ ! -f "$DATA_ROOT/src/db/mpm.db" ]; then
        log "  no database at $DATA_ROOT/src/db/mpm.db"
        log "  it will be initialized on first 'mpm' CLI invocation"
    else
        log "  database present at $DATA_ROOT/src/db/mpm.db"
    fi

    # 2026-09-18 hardening pass: existing-DB-tree permission
    # normalization. Forward enforcement is handled by the
    # process-wide umask in the binaries (see EnforcePrivateUmask),
    # but pre-existing state created before that hardening — or by
    # an old install that used a permissive umask — is normalized
    # here on every install/repair pass. Scope is intentionally
    # limited to $DATA_ROOT/src/db (NOT the whole data root, NOT
    # the install tree).
    #
    # Invariants enforced:
    #   - directories under src/db/   -> 0700
    #   - regular files under src/db/  -> 0600
    # Symlinks are NOT followed (no -L) so external targets are
    # never chmod'd. Filenames with whitespace/newlines are handled
    # by the NUL-delimited -print0/-read.
    if [ -d "$DATA_ROOT/src/db" ]; then
        local db_root
        db_root="$(cd "$DATA_ROOT/src/db" && pwd)"
        local _fixed=0 _checked=0
        while IFS= read -r -d '' p; do
            _checked=$((_checked + 1))
            # Tighten directories to 0700. Skip symlinks.
            if [ -L "$p" ]; then
                continue
            fi
            if [ -d "$p" ]; then
                chmod 0700 "$p" 2>/dev/null && _fixed=$((_fixed + 1))
            elif [ -f "$p" ]; then
                chmod 0600 "$p" 2>/dev/null && _fixed=$((_fixed + 1))
            fi
        done < <(find "$db_root" -mindepth 1 -print0 2>/dev/null)
        # Touch the root too in case it was permissive.
        chmod 0700 "$db_root" 2>/dev/null || true
        if [ "$_fixed" -gt 0 ]; then
            log "  tightened DB-tree perms: $_fixed entr(ies) (out of $_checked)"
        fi
    fi
}

phase_service() {
    note "SYSTEMD SERVICE"
    install -d -m 0755 "$(dirname "$SERVICE_DST")"

    # USER-level unit only. The legacy SYSTEM-level template has been
    # removed from contrib/systemd/ — only one install path exists.
    local service_src="${PROJECT_ROOT}/contrib/systemd/${SERVICE_NAME}.service.user"

    # Rewrite the template's canonical `%h/.mpm` placeholder to the prefix
    # actually being installed.
    #
    # The unit used to be copied verbatim, so a PREFIX override installed a
    # unit that still pointed at %h/.mpm — binaries at the default location,
    # data at the overridden one, and two install roots in one service. The
    # failure is quiet: systemd starts the scheduler happily from the old
    # prefix while the operator believes the new one is live.
    #
    # DATA_ROOT, not PREFIX, is what gets substituted: every %h/.mpm path in
    # the template is a RUNTIME path (workspace, DB, backups, and the two
    # subprocess binaries), and DATA_ROOT is the runtime root. With the
    # defaults they are the same directory, so this is a no-op for the
    # canonical install.
    local service_tmp
    service_tmp="$(mktemp)"
    sed "s|%h/\.mpm|${DATA_ROOT}|g" "$service_src" > "$service_tmp"
    install -m 0644 "$service_tmp" "$SERVICE_DST"
    rm -f "$service_tmp"
    log "  installed $SERVICE_DST"
    if [ "$DATA_ROOT" != "$HOME/.mpm" ]; then
        log "  unit paths rewritten for DATA_ROOT=$DATA_ROOT"
    fi

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


# ---------------------------------------------------------------------------
# phase_agent_reconcile
# ---------------------------------------------------------------------------
#
# Reconcile installed host managed blocks to the current canonical render.
#
# Why: this installer (the documented "git pull && ./install.sh" update path)
# rebuilds the binary but does NOT otherwise touch host state. Without this
# phase, an installed ~/.claude/CLAUDE.md, ~/.config/opencode/AGENTS.md, or
# ~/.pi/agent/AGENTS.md whose managed section was written from an older
# canonical contract would silently drift further on every update.
#
# This phase delegates to the host-agnostic generic entry point at
# agent_installation/scripts/reconcile_managed_blocks.py. That script
# discovers every adapter with a `reconcile.json` manifest and invokes
# each adapter's installer. Adapters that declare a `reconcile_if` gate
# are reconciled only when that host's integration is already installed
# on this machine, so MPM never opts a machine into a host it does not
# use. The root installer stays host-agnostic: it never names a
# specific adapter or host.
#
# Failure mode: each adapter is invoked independently. A failure in one
# adapter is logged as WARN; the install continues for the remaining
# adapters. Operators can re-run the failed adapter's installer or
# `make refresh-installed` to retry.
# hosts. Operators can re-run the failed host's installer or
# `make refresh-installed` to retry.
phase_agent_reconcile() {
    note "AGENT INSTALL RECONCILE"
    local reconcile_script="$PROJECT_ROOT/agent_installation/scripts/reconcile_managed_blocks.py"
    if [ ! -f "$reconcile_script" ]; then
        warn "  reconcile_managed_blocks.py missing at $reconcile_script"
        warn "  (this is unexpected on a stock MPM checkout)"
        return 0
    fi
    if python3 "$reconcile_script" --home "$HOME"; then
        log "  reconciliation ok"
    else
        warn "  one or more adapters reported non-zero exit (continuing)"
        return 1
    fi
}


phase_runtime_assets() {
    note "RUNTIME DEFINITION ASSETS"
    local assets_script="$PROJECT_ROOT/scripts/install_runtime_assets.py"
    if [ ! -f "$assets_script" ]; then
        warn "  install_runtime_assets.py missing at $assets_script"
        warn "  (this is unexpected on a stock MPM checkout)"
        return 0
    fi
    # Reconciles mode/ persona/ drills/ into $DATA_ROOT. Same script
    # `make install` calls, so the two install surfaces cannot drift
    # into different ownership semantics.
    #
    # Non-zero is FATAL here, unlike phase_agent_reconcile. A host whose
    # runtime root has no mode/persona definitions routes nothing and
    # injects no persona, which looks like a working install that is
    # silently inert. Failing loudly is the correct outcome; the
    # reconciler's own output names the specific conflicting files.
    if python3 "$assets_script" \
            --source-root "$PROJECT_ROOT" \
            --runtime-root "$DATA_ROOT"; then
        log "  runtime definitions reconciled"
    else
        err "  runtime-asset reconciliation failed (see above)"
        return 1
    fi
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
    phase_runtime_assets
    phase_service
    phase_agent_reconcile
    phase_validate
    note "INSTALL COMPLETE"
    # Use the canonical path so the next-steps commands are runnable
    # in the installer's own shell (where ~/.local/bin may not yet
    # be on PATH — see phase_symlinks' on-PATH warning). Defining
    # `cli` once avoids backslash-escape gymnastics inside the log
    # double-quoted strings below.
    local cli="$PREFIX/bin/mpm"
    log "  Mode:       USER-SPACE (no sudo, no /var/lib/mpm)"
    log "  CLI:        $cli (compiled binary; MPM_WORKSPACE defaults to $DATA_ROOT internally)"
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
    log "                  mpm config profile set default api_key            # hidden prompt (no echo)"
    log "                  mpm config component set memory default            # bind component to profile"
    log "  automation:     printf '%s' \"\$KEY\" | mpm config profile set default api_key --stdin"
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
    log "  # build output is $(make -C "$PROJECT_ROOT" -s --no-print-directory print-build-dir 2>/dev/null || echo '<BUILD_DIR>')"
    log "  #   (scratch, inside the checkout; distinct from \$PREFIX/bin even when"
    log "  #    the checkout IS \$PREFIX, as in the canonical \$HOME/.mpm install)"
    log "  install -m 0755 <BUILD_DIR>/mpm           -> $PREFIX/bin/mpm"
    log "  install -m 0755 <BUILD_DIR>/mpm-scheduler -> $PREFIX/bin/mpm-scheduler"
    log "  install -m 0755 <BUILD_DIR>/mpm-critic    -> $PREFIX/bin/mpm-critic"
    log "  install -m 0755 <BUILD_DIR>/mpm-mcp       -> $PREFIX/bin/mpm-mcp"
    log "  install -m 0755 <BUILD_DIR>/mpm-telemetry -> $PREFIX/bin/mpm-telemetry"
    log "  (legacy cleanup: rm -f $PREFIX/bin/mpm.real $PREFIX/bin/mpm.pre-wrapper.* from older installs)"
    log "  symlink $PREFIX/bin/mpm     -> $LOCAL_BIN/mpm"
    log "  symlink $PREFIX/bin/mpm-mcp -> $LOCAL_BIN/mpm-mcp"
    log "  install -d -m 0700 $DATA_ROOT/src/db $DATA_ROOT/backups/critic-pre  (and chmod 0700 to harden pre-existing dirs)"
    log "  reconcile runtime definitions (mode/ persona/ drills/) into $DATA_ROOT via"
    log "    scripts/install_runtime_assets.py --source-root $PROJECT_ROOT --runtime-root $DATA_ROOT"
    log "    (provisioned as owned copies, so the runtime root needs no repository"
    log "     tree; locally modified definitions are preserved, untouched ones"
    log "     refresh from source, custom files are never touched)"
    log "  loginctl enable-linger $USER_NAME"
    log "  install -m 0644 .../contrib/systemd/${SERVICE_NAME}.service.user -> $SERVICE_DST"
    log "  systemctl --user daemon-reload && enable --now $SERVICE_NAME"
    if is_ecryptfs_home; then
        log "  ecryptfs detected → install ~/.config/autostart/mpm-post-decrypt.desktop (daemon-reload + start after decrypt)"
        log "  and: systemctl --user add-wants graphical-session.target mpm-scheduler.service (secondary)"
    fi
    log "  validate via systemctl status + mpm health_check"
    log "  reconcile installed host managed blocks via the host-agnostic"
    log "    agent_installation/scripts/reconcile_managed_blocks.py entry"
    log "    point (idempotent; preserves user content outside managed block;"
    log "    reconciles a host only when its integration is already installed)"
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

    # All five current-layout binaries under $PREFIX/bin/.
    for bin in mpm mpm-mcp mpm-scheduler mpm-critic mpm-telemetry; do
        if [ -f "$PREFIX/bin/$bin" ]; then
            rm -f "$PREFIX/bin/$bin"
            log "  removed $PREFIX/bin/$bin"
        fi
    done

    # Legacy wrapper + mpm.real artefacts from older installs.
    for pat in "$PREFIX/bin/mpm.real" "$PREFIX/bin/mpm.pre-wrapper."*; do
        # shellcheck disable=SC2086
        for f in $pat; do
            if [ -f "$f" ]; then
                rm -f "$f"
                log "  removed legacy $f"
            fi
        done
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
  (default)       Full install: preflight, build, install binaries + service,
                  reconcile host managed blocks, validate
  --check         Preflight only — verify environment, no changes
  --dry-run       Print intended actions, no changes
  --validate      Post-install validation (read-only)
  --reconcile     Reconcile host managed blocks to canonical render only
                  (the documented "git pull && ./install.sh --reconcile"
                  path after a content-only MPM update)
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
  $SCRIPT_NAME --reconcile      # reconcile host managed blocks only
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
            --reconcile)    MODE="reconcile"; shift ;;
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

mode_reconcile() {
    note "RECONCILE (managed-block reconciliation only)"
    preflight
    phase_agent_reconcile
    note "RECONCILE COMPLETE"
}

main() {
    parse_args "$@"
    resolve_paths

    case "$MODE" in
        install)   mode_install ;;
        check)     mode_check ;;
        dry_run)   mode_dry_run ;;
        validate)  mode_validate ;;
        reconcile) mode_reconcile ;;
        uninstall) mode_uninstall ;;
        *)         err "unknown mode: $MODE"; usage; exit 1 ;;
    esac
}

main "$@"