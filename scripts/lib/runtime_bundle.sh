#!/usr/bin/env bash
# scripts/lib/runtime_bundle.sh — versioned runtime bundles, and the
# active-runtime pointer.
#
# WHY THIS EXISTS
#
# An MPM installation has two halves with two different lifetimes:
#
#   * the RUNTIME — five binaries, the host adapters, the plugin
#     payload. It is replaced WHOLESALE on every upgrade.
#   * the STATE — the database, the backups, the operator's
#     configuration and definitions. It must survive every upgrade,
#     unchanged.
#
# Today both halves live in one directory, so "upgrade" means "copy five
# binaries over the top of a directory that also holds the database".
# That is why every upgrade is a chance to overwrite something that was
# never supposed to be overwritten.
#
# This library implements the split:
#
#   <data root>/
#     releases/<rev>/       a versioned, Git-free runtime bundle
#     current -> releases/<rev>     THE ACTIVE POINTER
#     staging/<rev>/        where a candidate bundle is assembled
#     src/db/  backups/  mpm_config.json  mode/  persona/   the STATE
#
# The pointer is a SYMLINK, and promotion is a rename(2) of a new
# symlink over it. rename(2) is atomic, so there is no instant at which
# `current` fails to resolve: a reader sees the old revision or the new
# one, never a gap and never a mixture. Rollback is the same operation
# with the previous target.
#
# THE INVARIANTS THIS LIBRARY ENFORCES
#
#   1. A bundle contains NO state. It is rejected if it carries
#      src/db, backups, mpm_config.json, mode/ or persona/ — because a
#      bundle that carried the operator's data would make every version
#      switch a chance to replace it.
#
#   2. A bundle is Git-free. It is rejected if it carries .git: the
#      installed runtime must not depend on repository metadata.
#
#   3. A bundle is validated BEFORE it is exposed. The five binaries
#      must be present, regular and executable; the build identity in
#      RELEASE.json must match the binaries actually inside; and the
#      binaries must report the revision they claim.
#
#   4. A release directory is only ever created by an atomic rename of
#      a fully-assembled, fully-validated candidate. `current` is
#      repointed only after that rename has succeeded, so the pointer
#      never names a directory that does not exist.
#
# EXIT CODES
#
#   0   success
#   1   a refusal (the reason is on stderr)
#
# Sourced by install.sh. Every function is safe to call repeatedly.

# ---------- logging ----------
# The Makefile sources libraries with no logging helpers defined, so fall
# back to plain stderr rather than requiring every caller to provide them.
rb_log() {
    if declare -F log >/dev/null 2>&1; then log "$@"; else printf '%s\n' "$*" >&2; fi
}
rb_warn() {
    if declare -F warn >/dev/null 2>&1; then warn "$@"; else printf 'WARN: %s\n' "$*" >&2; fi
}
rb_err() {
    if declare -F err >/dev/null 2>&1; then err "$@"; else printf 'ERROR: %s\n' "$*" >&2; fi
}

# The five binaries, in the canonical promotion order. Defined once here
# so the bundle validator, the installer and the tests cannot disagree
# about what a complete runtime is.
RB_BINARIES="mpm mpm-scheduler mpm-critic mpm-mcp mpm-telemetry"

# The OpenCode runtime payload manifest.
#
# CANONICAL SOURCE: agent_installation/mpm-opencode/install.sh defines
# OPENCODE_RUNTIME_MANIFEST. The two lists MUST agree: a payload the
# adapter installs but the bundle omits is a bundle that registers a
# plugin which loads NOTHING — the silent failure the staging validator
# exists to prevent. scripts/tests/test_runtime_bundle.py parses the
# adapter's list and fails if the two drift apart.
#
# dist/index.js is deliberately absent: it is the legacy V1 emit, and
# staging it would re-couple the package to the V1 surface shape.
RB_OPENCODE_MANIFEST="server.js dist/index.v2.js dist/wake-context.js dist/event-envelope.js dist/event-mapper.js dist/event-subscriber.js dist/workspace.js dist/resolve-mpm-binary.js package.json"

# Paths a bundle must NEVER carry. These are the state half of the
# installation; a bundle that carried them would let a version switch
# replace the operator's data.
RB_FORBIDDEN_IN_BUNDLE="src/db src/backups backups mpm_config.json mode persona drills active.json"

# ---------- path helpers ----------

rb_releases_dir() { printf '%s\n' "$1/releases"; }
rb_current_link() { printf '%s\n' "$1/current"; }
rb_staging_dir()  { printf '%s\n' "$1/staging"; }

# ---------- atomic rename over an existing path ----------
#
# rename(2) replaces the destination atomically. `mv -T` is the shell
# spelling of that on GNU coreutils; the `-T` is LOAD-BEARING, because
# without it `mv src dst` where `dst` is a symlink to a directory moves
# `src` INTO that directory instead of replacing the link.
#
# Where `mv -T` is unavailable, python3's os.rename is the same
# operation with the same atomicity. Both paths are used, because the
# pointer swap is the one step in the upgrade that must never be
# observed half-done.
_RB_RENAME_OVER=""
_rb_probe_rename_over() {
    [ -n "$_RB_RENAME_OVER" ] && return 0
    local probe
    probe="$(mktemp -d "${TMPDIR:-/tmp}/mpm-rb-probe.XXXXXX")" || return 1
    if ln -s target "$probe/link" 2>/dev/null && \
       ln -s other "$probe/tmp" 2>/dev/null && \
       mv -T "$probe/tmp" "$probe/link" 2>/dev/null && \
       [ "$(readlink "$probe/link" 2>/dev/null)" = "other" ]; then
        _RB_RENAME_OVER="mv"
    elif command -v python3 >/dev/null 2>&1 && \
         python3 - "$probe/tmp" "$probe/link" <<'PY' 2>/dev/null
import os, sys
os.rename(sys.argv[1], sys.argv[2])
PY
    then
        _RB_RENAME_OVER="python3"
    else
        rm -rf "$probe"
        return 1
    fi
    rm -rf "$probe"
    return 0
}

# rb_rename_over <src> <dst> — atomically replace <dst> with <src>.
rb_rename_over() {
    local src="$1" dst="$2"
    if ! _rb_probe_rename_over; then
        rb_err "no atomic rename is available on this system (mv -T and python3 both failed)"
        rb_err "the active-runtime pointer cannot be swapped safely; refusing to continue"
        return 1
    fi
    case "$_RB_RENAME_OVER" in
        mv)
            mv -T "$src" "$dst"
            ;;
        python3)
            python3 - "$src" "$dst" <<'PY'
import os, sys
os.rename(sys.argv[1], sys.argv[2])
PY
            ;;
        *)
            return 1
            ;;
    esac
}

# ---------- the build identity ----------

# rb_write_release <bundle_dir> <rev> <commit> <epoch> <version>
#
# RELEASE.json is the bundle's identity, readable WITHOUT executing Git
# and without running any binary. It is what an operator, a rollback
# procedure or a support conversation can read to know what is
# installed.
rb_write_release() {
    local bundle="$1" rev="$2" commit="$3" epoch="$4" version="$5"
    local built_at="unknown"
    built_at="$(date -u -d "@$epoch" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || printf 'unknown')"
    cat > "$bundle/RELEASE.json" <<EOF
{
  "revision": "$rev",
  "commit": "$commit",
  "short_commit": "$(printf '%s' "$commit" | cut -c1-8)",
  "build_epoch": $epoch,
  "version": "$version",
  "built_at": "$built_at",
  "bundle_layout": "versioned-bundle/1"
}
EOF
}

# rb_release_field <bundle_dir> <field> — read one field from RELEASE.json.
rb_release_field() {
    local bundle="$1" field="$2"
    [ -f "$bundle/RELEASE.json" ] || return 1
    python3 - "$bundle/RELEASE.json" "$field" <<'PY' 2>/dev/null
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(1)
v = d.get(sys.argv[2], "")
print(v if v is not None else "")
PY
}

# ---------- validation ----------

# rb_validate_bundle <bundle_dir> [expected_rev]
#
# Everything that must be true before a bundle is allowed to become the
# active runtime. A refusal here means the bundle is NOT installed and
# the pointer is NOT moved — the installation keeps running whatever it
# was running before.
rb_validate_bundle() {
    local bundle="$1" expected_rev="${2:-}"
    local bin rel

    [ -d "$bundle" ] || { rb_err "bundle directory does not exist: $bundle"; return 1; }

    # 1. The five binaries.
    for bin in $RB_BINARIES; do
        local p="$bundle/bin/$bin"
        if [ ! -e "$p" ]; then
            rb_err "bundle is missing bin/$bin"
            return 1
        fi
        if [ ! -f "$p" ]; then
            rb_err "bundle's bin/$bin is not a regular file"
            return 1
        fi
        if [ ! -x "$p" ]; then
            rb_err "bundle's bin/$bin is not executable"
            return 1
        fi
    done

    # 2. No Git metadata. The installed runtime must not depend on a
    #    repository: the whole point of D5 is that the identity is
    #    embedded at link time.
    if [ -e "$bundle/.git" ]; then
        rb_err "the bundle carries .git — a runtime bundle must be Git-free"
        return 1
    fi
    if [ -d "$bundle" ] && find "$bundle" -name .git -print -quit 2>/dev/null | grep -q .; then
        rb_err "the bundle carries a .git directory somewhere inside it — a runtime bundle must be Git-free"
        return 1
    fi

    # 3. No state. A bundle that carried the operator's data would make
    #    every version switch a chance to replace it.
    for rel in $RB_FORBIDDEN_IN_BUNDLE; do
        if [ -e "$bundle/$rel" ]; then
            rb_err "the bundle carries '$rel' — a runtime bundle must contain no persistent state"
            rb_err "(the state belongs in the data root, where a version switch cannot reach it)"
            return 1
        fi
    done

    # 4. The identity.
    if [ ! -f "$bundle/RELEASE.json" ]; then
        rb_err "bundle has no RELEASE.json — its build identity would be unreadable"
        return 1
    fi
    local rev
    rev="$(rb_release_field "$bundle" revision)" || {
        rb_err "bundle's RELEASE.json is not readable JSON"
        return 1
    }
    if [ -z "$rev" ]; then
        rb_err "bundle's RELEASE.json names no revision"
        return 1
    fi
    if [ -n "$expected_rev" ] && [ "$rev" != "$expected_rev" ]; then
        rb_err "bundle's RELEASE.json names revision '$rev' but '$expected_rev' was expected"
        return 1
    fi

    # 5. The binaries must report the revision they claim. This is the
    #    check that a bundle is not a MIX of two builds.
    local commit reported
    commit="$(rb_release_field "$bundle" commit)"
    if [ -n "$commit" ] && [ "$commit" != "unknown" ]; then
        reported="$("$bundle/bin/mpm" --version 2>/dev/null | awk '/commit:/{print $2; exit}')"
        if [ -z "$reported" ]; then
            rb_err "the bundle's mpm binary did not report a build commit"
            return 1
        fi
        if [ "$reported" != "$commit" ]; then
            rb_err "the bundle's mpm binary reports commit '$reported' but RELEASE.json says '$commit'"
            rb_err "(the bundle is a mixture of two builds — refusing to install it)"
            return 1
        fi
    fi

    return 0
}

# ---------- installing a release ----------

# rb_install_release <candidate_dir> <data_root> <rev>
#
# Move a fully-assembled, fully-validated candidate into
# releases/<rev>. The move is a rename(2) — the candidate and the
# release directory are on the same filesystem because the candidate is
# assembled under <data root>/staging — so the release directory either
# does not exist or is complete. It is never half-populated.
#
# If releases/<rev> already exists, the candidate is refused: a release
# is a specific revision, and overwriting it in place would silently
# change what a rollback to that revision would give back. The operator
# either picks a different revision label or removes the old release.
rb_install_release() {
    local candidate="$1" data_root="$2" rev="$3"
    local releases final
    releases="$(rb_releases_dir "$data_root")"
    final="$releases/$rev"

    [ -d "$candidate" ] || { rb_err "candidate bundle does not exist: $candidate"; return 1; }
    install -d -m 0755 "$releases" || { rb_err "cannot create $releases"; return 1; }

    if [ -e "$final" ]; then
        rb_err "release '$rev' is already installed at $final"
        rb_err "a release is immutable — installing a different build under the same"
        rb_err "label would change what a rollback to that label would give back."
        rb_err "choose another label (--rev), or remove $final first."
        return 1
    fi

    rb_rename_over "$candidate" "$final" || {
        rb_err "could not install the bundle into $final"
        return 1
    }
    rb_log "  release installed at releases/$rev"
    return 0
}

# ---------- the active pointer ----------

# rb_current_target <data_root> — what `current` points at, relative to
# the data root (e.g. "releases/<rev>"), or the empty string.
rb_current_target() {
    local data_root="$1"
    local link
    link="$(rb_current_link "$data_root")"
    [ -L "$link" ] || return 1
    readlink "$link" 2>/dev/null || return 1
}

# rb_activate <data_root> <rev> — make releases/<rev> the active
# runtime.
#
# The swap is a rename(2) of a freshly-created symlink over `current`,
# so a concurrent reader sees either the old revision or the new one.
rb_activate() {
    local data_root="$1" rev="$2"
    local current tmp
    current="$(rb_current_link "$data_root")"
    tmp="$data_root/.current.swap.$$"

    if [ ! -d "$(rb_releases_dir "$data_root")/$rev" ]; then
        rb_err "cannot activate '$rev': no such release"
        return 1
    fi

    rm -f "$tmp" 2>/dev/null || true
    ln -s "releases/$rev" "$tmp" || {
        rb_err "could not create the replacement pointer at $tmp"
        return 1
    }
    if ! rb_rename_over "$tmp" "$current"; then
        rm -f "$tmp" 2>/dev/null || true
        rb_err "could not swap the active pointer"
        return 1
    fi
    rb_log "  active pointer: current -> releases/$rev"
    return 0
}

# rb_restore_pointer <data_root> <target> — point `current` back at a
# previous target. The target is the value rb_current_target returns
# ("releases/<rev>").
rb_restore_pointer() {
    local data_root="$1" target="$2"
    local current tmp
    current="$(rb_current_link "$data_root")"
    tmp="$data_root/.current.swap.$$"
    [ -n "$target" ] || { rm -f "$current" 2>/dev/null || true; return 0; }
    rm -f "$tmp" 2>/dev/null || true
    ln -s "$target" "$tmp" || return 1
    rb_rename_over "$tmp" "$current" || { rm -f "$tmp" 2>/dev/null || true; return 1; }
    return 0
}

# ---------- layout conversion ----------
#
# A checkout-style installation has REAL directories where the versioned
# layout has symlinks into the active bundle:
#
#     bin/                    -> current/bin
#     agent_installation/     -> current/agent_installation
#
# The conversion moves the real directories ASIDE rather than deleting
# them: they are the rollback material for a failed conversion, and they
# are the operator's own source tree until they are deliberately
# removed. The moved-aside paths are recorded in
# <data root>/.legacy-layout-paths so the rollback procedure can find
# them without guessing.

# rb_convert_layout <data_root> <prefix> <current_target>
#
# The third argument is the value `current` holds, so the symlinks are
# written with the same relative target the pointer uses.
rb_convert_layout() {
    local data_root="$1" prefix="$2" target="${3:-}"
    local name p legacy rc=0
    [ -n "$target" ] || { rb_err "no active pointer target — refusing to convert the layout"; return 1; }

    for name in bin agent_installation; do
        p="$prefix/$name"
        if [ -L "$p" ]; then
            ln -sfn "current/$name" "$p" || { rb_err "could not repoint the $name symlink"; rc=1; continue; }
            rb_log "  $p -> current/$name"
            continue
        fi
        if [ -e "$p" ]; then
            legacy="$prefix/.${name}.legacy.$$"
            if ! mv "$p" "$legacy"; then
                rb_err "could not move the existing $name aside (to $legacy)"
                rc=1
                continue
            fi
            if ! ln -s "current/$name" "$p"; then
                mv "$legacy" "$p" 2>/dev/null || true
                rb_err "could not create the $name symlink; the previous directory was restored"
                rc=1
                continue
            fi
            printf '%s\n' "$legacy" >> "$data_root/.legacy-layout-paths"
            rb_log "  $p -> current/$name (the previous directory is preserved at ${legacy##*/})"
            continue
        fi
        # Nothing there yet — a fresh data root. Create the symlink.
        if ! ln -s "current/$name" "$p"; then
            rb_err "could not create the $name symlink"
            rc=1
            continue
        fi
        rb_log "  $p -> current/$name"
    done
    return "$rc"
}

# rb_revert_layout <data_root> <prefix>
#
# Undo rb_convert_layout. The real directories that were moved aside are
# moved back, and the symlinks that replaced them are removed.
#
# This is the recovery half of a failed promotion: a promotion that
# fails after the pointer was swapped must leave the installation
# exactly as it was, not pointing at a revision whose conversion did
# not complete.
#
# Only the directories that were ACTUALLY moved aside are restored. A
# path that was already a symlink before the conversion keeps being one
# — and because the pointer is restored to the previous revision, it
# resolves into the previous release, which is correct.
rb_revert_layout() {
    local data_root="$1" prefix="$2"
    local record="$data_root/.legacy-layout-paths"
    local legacy name rc=0
    [ -f "$record" ] || return 0

    while IFS= read -r legacy; do
        [ -n "$legacy" ] || continue
        if [ ! -e "$legacy" ]; then
            rb_err "the preserved directory $legacy is missing — it cannot be restored"
            rc=1
            continue
        fi
        name="$(basename "$legacy" | sed 's/^\.//; s/\.legacy\.[0-9]*$//')"
        if [ -L "$prefix/$name" ]; then
            if ! rm -f "$prefix/$name"; then
                rb_err "could not remove the $name symlink at $prefix/$name"
                rc=1
                continue
            fi
        fi
        if mv "$legacy" "$prefix/$name"; then
            rb_log "  $name restored to its pre-upgrade directory"
        else
            rb_err "could not restore $name from $legacy"
            rc=1
        fi
    done < "$record"

    # The record itself is consumed: a second revert must not try to
    # restore the same directories again.
    : > "$record"
    return "$rc"
}
