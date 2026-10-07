#!/usr/bin/env bash
# scripts/lib/binary_transaction.sh — transactional promotion of the five
# installed MPM binaries.
#
# WHY THIS EXISTS
#
# Both promotion surfaces in this repository — install.sh's phase_binaries
# and the Makefile's `install:` target — used to copy the five binaries with
# five independent `install -m 0755 src dst` calls in a loop. That has two
# distinct failure modes, both reproduced before this file existed (see
# §4 of the transactional-install tranche):
#
#   1. PARTIAL PROMOTION. `install` writes the destination in place, so
#      between copy 1 and copy 5 the destination directory holds a MIXTURE
#      of the old and new releases. A failure partway through leaves that
#      mixture on disk permanently.
#
#   2. DESTRUCTION OF THE OLD BINARY. GNU `install` UNLINKS the destination
#      before it reads the source. If the read then fails (unreadable
#      candidate, ENOSPC, a signal), the previous binary is gone entirely —
#      not merely stale, but deleted. There was no backup to restore from.
#
# Per-file atomicity is not the requirement. Five individually-atomic renames
# can still produce a mixed release, which is exactly failure mode 1. The
# requirement is externally observable all-or-old behaviour.
#
# THE TRANSACTION
#
#   stage   — copy every candidate into a private staging directory created
#             INSIDE the destination directory, so promotion is a same-
#             filesystem rename(2) and never a cross-device copy.
#   verify  — reject the whole install if any candidate is missing, is not a
#             regular file, or is not executable. This happens BEFORE any
#             destination file is touched.
#   backup  — move each existing destination binary into a private rollback
#             directory (same filesystem; rename, so mode and ownership of
#             the old file are preserved exactly).
#   promote — move each staged binary into place with rename(2).
#   commit  — the transaction is committed the instant the LAST rename
#             succeeds. Rollback material is deleted only after that point.
#   rollback— on any failure before commit, restore every promoted binary
#             from the rollback directory, and remove any promoted binary
#             that had no old counterpart (a first install has none).
#
# COMMIT SEMANTICS (see the tranche's §6)
#
# The commit point is the completion of the fifth promotion. Everything
# before it is reversible; everything after it is not. In particular:
#
#   * Failure to delete rollback material AFTER commit is a warning, not a
#     rollback. The binaries on disk are the correct, complete new release;
#     re-promoting the old ones over them would be a second, unrequested
#     mutation. The leftover rollback directory is left in place because it
#     is the operator's only recovery copy.
#   * Failure of any LATER installer phase (symlinks, runtime assets,
#     systemd) is a post-commit failure and is reported as such. This
#     function makes the five-binary promotion transactional. It does not
#     make the whole installation transactional, and does not claim to.
#
# TEST-ONLY FAILURE SEAM
#
#   MPM_INSTALL_TEST_FAIL_AFTER=<n>   promote exactly <n> binaries, then
#                                     fail (0 = fail before promoting any).
#                                     Used to pin the partial-promotion
#                                     failure matrix. Never set in
#                                     production; the name is deliberately
#                                     conspicuous so it cannot be enabled
#                                     by accident, and it is ignored unless
#                                     the value is a non-negative integer.
#
# CONCURRENCY
#
# Promotion is serialised per destination directory with flock(2) on
# "$bin_dir/../.mpm-install.lock". The lock is released automatically by the
# kernel when the holding process exits, including on a crash, so there is no
# stale-lock file to clean up by hand. The lock is per install prefix; there
# is no machine-wide lock.
#
# EXIT CODES
#
#   0   committed — all five binaries promoted
#   10  rolled back cleanly — the destination is exactly as it was
#   11  rollback INCOMPLETE — restoration failed; rollback material has been
#       preserved and the operator must intervene
#   12  refused before touching anything (missing/invalid candidate, or the
#       lock is held by another installer)
#
# Sourced by install.sh and by the Makefile's `install:` target so the two
# promotion surfaces cannot drift apart again.

# ---------- logging ----------
# The Makefile sources this file with no logging helpers defined, so fall
# back to plain stderr rather than requiring every caller to provide them.
bt_log() {
    if declare -F log >/dev/null 2>&1; then log "$@"; else printf '%s\n' "$*" >&2; fi
}
bt_warn() {
    if declare -F warn >/dev/null 2>&1; then warn "$@"; else printf 'WARN: %s\n' "$*" >&2; fi
}
bt_err() {
    if declare -F err >/dev/null 2>&1; then err "$@"; else printf 'ERROR: %s\n' "$*" >&2; fi
}

# The canonical set, in promotion order. install.sh historically used
# "mpm mpm-scheduler mpm-critic mpm-mcp mpm-telemetry" and the Makefile used
# "mpm mpm-mcp mpm-scheduler ...". The two orders disagreed, which meant a
# partial promotion left a different mixture depending on which surface was
# used. There is now exactly one order, defined here.
BT_DEFAULT_BINARIES="mpm mpm-scheduler mpm-critic mpm-mcp mpm-telemetry"

# ---------- test seam ----------
# Promote this many binaries, then fail. Empty/unset = disabled.
_bt_fail_after="${MPM_INSTALL_TEST_FAIL_AFTER-}"

# Consume one unit of the failure budget. Returns 0 to keep going.
_bt_tick() {
    _bt_promoted="${1:-0}"
    case "$_bt_fail_after" in
        ''|*[!0-9]*) return 0 ;;
    esac
    if [ "$_bt_promoted" -eq "$_bt_fail_after" ]; then
        bt_err "MPM_INSTALL_TEST_FAIL_AFTER=$_bt_fail_after: injected failure after promoting $_bt_promoted binaries"
        return 1
    fi
    return 0
}

# ---------- per-prefix install lock ----------
_bt_lock() {
    local bin_dir="$1" lockfile
    lockfile="$(dirname "$bin_dir")/.mpm-install.lock"
    exec 9>"$lockfile" || {
        bt_err "cannot open install lock $lockfile"
        return 12
    }
    if ! flock -n 9; then
        bt_err "another installer holds the lock $lockfile; refusing to run concurrently"
        exec 9>&-
        return 12
    fi
    return 0
}

_bt_unlock() {
    exec 9>&- 2>/dev/null || true
}

# ---------- candidate validation ----------
# Every candidate must exist, be a regular file, be readable, and carry the
# executable bit. Checked for ALL candidates before ANY destination is
# touched, so a missing fifth binary can never leave the first four replaced.
_bt_validate_candidates() {
    local build_dir="$1" bin
    shift
    for bin in "$@"; do
        local src="$build_dir/$bin"
        if [ ! -e "$src" ]; then
            bt_err "build did not produce $src"
            return 12
        fi
        if [ -d "$src" ]; then
            bt_err "$src is a directory, not a binary"
            return 12
        fi
        if [ ! -f "$src" ]; then
            bt_err "$src is not a regular file"
            return 12
        fi
        if [ ! -r "$src" ]; then
            bt_err "$src is not readable"
            return 12
        fi
        if [ ! -x "$src" ]; then
            bt_err "$src is not executable"
            return 12
        fi
    done
    return 0
}

# ---------- failure reporting ----------
# The operator-facing summary line. Emitted once per failed transaction, and
# deliberately says the previous set was restored only when _bt_rollback is
# actually going to restore it — the caller emits the matching line after the
# rollback result is known.
_bt_report_failure() {
    bt_err "binary promotion failed"
}

# ---------- rollback ----------
# Restore the destination to its exact pre-promotion state.
#   $1 bin_dir  $2 rollback_dir  $3.. binaries
#
# The BACKUP step moved EVERY pre-existing destination binary out of the
# destination, not just the ones already promoted. Rollback must therefore
# restore everything that is sitting in the rollback directory, whether or not
# it had been promoted yet — otherwise a failure at binary 3 leaves binaries
# 4 and 5 missing from the prefix entirely, which is a worse outcome than the
# mixed release this transaction exists to prevent.
#
# For each binary in the set:
#   * a copy exists in the rollback dir  -> rename it back (restores the old
#     file's bytes, mode and ownership together, since it is the same inode)
#   * no copy exists, but the binary is in the destination -> it was promoted
#     during a first install, so remove it; there was no prior state
#   * neither -> the binary was absent before and is absent now; leave it
_bt_rollback() {
    local bin_dir="$1" rollback_dir="$2"
    shift 2
    local bin restored=0 failed=0 removed=0

    bt_warn "rolling back binary promotion"
    for bin in "$@"; do
        if [ -e "$rollback_dir/$bin" ] || [ -L "$rollback_dir/$bin" ]; then
            if mv -f "$rollback_dir/$bin" "$bin_dir/$bin" 2>/dev/null; then
                restored=$((restored + 1))
            else
                bt_err "ROLLBACK FAILED: could not restore $bin_dir/$bin"
                failed=1
            fi
        elif [ -e "$bin_dir/$bin" ]; then
            # Promoted during a first install — no old counterpart existed.
            if rm -f "$bin_dir/$bin" 2>/dev/null; then
                removed=$((removed + 1))
            else
                bt_err "ROLLBACK FAILED: could not remove newly installed $bin_dir/$bin"
                failed=1
            fi
        fi
    done

    if [ "$failed" -ne 0 ]; then
        bt_err "rollback incomplete: $restored restored, $removed removed, restoration errors above"
        bt_err "rollback material is preserved at $rollback_dir — do NOT delete it;"
        bt_err "it holds the only recoverable copy of your previous binaries."
        bt_err "recover manually with:  for b in $rollback_dir/*; do mv -f \"\$b\" \"$bin_dir/\"; done"
        return 11
    fi

    bt_log "  rollback complete: $restored restored, $removed removed"
    # Emitted here rather than by each caller so the statement cannot drift
    # between install.sh and the Makefile, and so it is only ever printed
    # after restoration has actually been confirmed to succeed.
    bt_err "binary promotion failed; previous binary set restored"
    return 0
}

# ---------- the transaction ----------
#
# mpm_promote_binaries <build_dir> <bin_dir> [binary names...]
mpm_promote_binaries() {
    local build_dir="$1" bin_dir="$2"
    shift 2
    local binaries=("$@")
    if [ "${#binaries[@]}" -eq 0 ]; then
        # shellcheck disable=SC2206
        binaries=($BT_DEFAULT_BINARIES)
    fi

    if [ ! -d "$build_dir" ]; then
        bt_err "build output directory does not exist: $build_dir"
        return 12
    fi

    _bt_lock "$bin_dir" || return $?
    local rc=0
    _bt_promote_transaction "$build_dir" "$bin_dir" "${binaries[@]}" || rc=$?
    _bt_unlock
    return "$rc"
}

_bt_promote_transaction() {
    local build_dir="$1" bin_dir="$2"
    shift 2
    local binaries=("$@")
    local stage_dir="" rollback_dir=""
    local bin promoted=0 have_old=0

    # --- validate every candidate before touching the destination ---
    _bt_validate_candidates "$build_dir" "${binaries[@]}" || return 12

    if [ ! -d "$bin_dir" ]; then
        mkdir -p "$bin_dir" || { bt_err "cannot create $bin_dir"; return 12; }
    fi

    # Private, same-filesystem, mode-0700 staging and rollback directories.
    # mktemp -d is used rather than a fixed name so two installers (or an
    # attacker with write access to the prefix) cannot predict or collide
    # with the path. Creating them under $bin_dir — not /tmp — is what
    # guarantees rename(2) succeeds instead of silently degrading to a
    # cross-device copy.
    stage_dir="$(mktemp -d "$bin_dir/.mpm-stage.XXXXXXXX")" || {
        bt_err "cannot create staging directory under $bin_dir"
        return 12
    }
    chmod 0700 "$stage_dir"
    rollback_dir="$(mktemp -d "$bin_dir/.mpm-rollback.XXXXXXXX")" || {
        bt_err "cannot create rollback directory under $bin_dir"
        rm -rf "$stage_dir"
        return 12
    }
    chmod 0700 "$rollback_dir"

    # --- stage every candidate ---
    # Copy (not move) so the build tree is left intact. install into our own
    # private staging directory is safe even though `install` unlinks the
    # destination first: that destination is ours and disposable.
    for bin in "${binaries[@]}"; do
        if ! install -m 0755 "$build_dir/$bin" "$stage_dir/$bin"; then
            bt_err "failed to stage $build_dir/$bin"
            rm -rf "$stage_dir" "$rollback_dir"
            return 12
        fi
    done
    bt_log "  staged ${#binaries[@]} binaries in $stage_dir"

    # --- backup every existing destination binary ---
    # rename(2), so the old file's mode and ownership are carried across
    # untouched and are restored exactly by the matching rollback rename.
    for bin in "${binaries[@]}"; do
        if [ -e "$bin_dir/$bin" ] || [ -L "$bin_dir/$bin" ]; then
            if ! mv -f "$bin_dir/$bin" "$rollback_dir/$bin"; then
                bt_err "failed to back up $bin_dir/$bin"
                # Some binaries are already out of the destination and in the
                # rollback directory. Put them back before giving up, or a
                # backup failure becomes a second, worse kind of corruption.
                _bt_rollback "$bin_dir" "$rollback_dir" "${binaries[@]}" || true
                rm -rf "$stage_dir" "$rollback_dir" 2>/dev/null || true
                return 12
            fi
            have_old=$((have_old + 1))
        fi
    done
    [ "$have_old" -gt 0 ] && bt_log "  backed up $have_old existing binaries"

    # --- promote ---
    # The tick is evaluated BEFORE each promotion, so fail_after=N fires once
    # exactly N binaries are in place. fail_after == count(binaries) therefore
    # fires on the final iteration, before the commit — the "all five
    # promoted but the transaction not yet committed" boundary, which must
    # still roll back rather than half-commit.
    for bin in "${binaries[@]}"; do
        _bt_tick "$promoted" || {
            _bt_report_failure
            _bt_rollback "$bin_dir" "$rollback_dir" "${binaries[@]}"
            local rrc=$?
            # After a SUCCESSFUL rollback the rollback directory is empty —
            # its contents are back in the destination — so it is safe to
            # remove. After a FAILED rollback it holds the only recoverable
            # copy and must survive.
            [ "$rrc" -eq 0 ] && rm -rf "$rollback_dir" 2>/dev/null
            rm -rf "$stage_dir" 2>/dev/null || true
            [ "$rrc" -eq 0 ] && rrc=10
            return "$rrc"
        }
        if ! mv -f "$stage_dir/$bin" "$bin_dir/$bin"; then
            bt_err "failed to promote $bin to $bin_dir/$bin"
            _bt_report_failure
            _bt_rollback "$bin_dir" "$rollback_dir" "${binaries[@]}"
            local prc=$?
            [ "$prc" -eq 0 ] && rm -rf "$rollback_dir" 2>/dev/null
            rm -rf "$stage_dir" 2>/dev/null || true
            [ "$prc" -eq 0 ] && prc=10
            return "$prc"
        fi
        promoted=$((promoted + 1))
        bt_log "  promoted $bin_dir/$bin"
    done

    # --- COMMIT ---
    # One final tick, so a failure can be injected at the boundary where all
    # five have been promoted but the transaction is not yet committed. That
    # state is the one most likely to be mishandled (a caller could easily
    # treat "everything is in place" as "nothing left to undo"), and it must
    # roll back like any other pre-commit failure.
    _bt_tick "$promoted" || {
        _bt_report_failure
        _bt_rollback "$bin_dir" "$rollback_dir" "${binaries[@]}"
        local crc=$?
        [ "$crc" -eq 0 ] && rm -rf "$rollback_dir" 2>/dev/null
        rm -rf "$stage_dir" 2>/dev/null || true
        [ "$crc" -eq 0 ] && crc=10
        return "$crc"
    }

    # The fifth rename above was the commit point. From here the release is
    # the new one and is not rolled back.
    rm -rf "$stage_dir" 2>/dev/null || true
    if ! rm -rf "$rollback_dir" 2>/dev/null; then
        # Post-commit cleanup failure. The binaries are correct and
        # complete; the only cost is a leftover rollback directory. Keep it —
        # deleting it is not worth the risk, and it holds a stale copy the
        # operator may want.
        bt_warn "binary promotion committed, but rollback material at $rollback_dir could not be removed"
        bt_warn "remove it manually once you are satisfied with the install:  rm -rf $rollback_dir"
    fi
    bt_log "  promotion committed: $promoted binaries"
    return 0
}
