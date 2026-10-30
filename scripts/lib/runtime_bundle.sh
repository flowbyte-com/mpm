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
#
# The manifest itself lives in scripts/runtime_payloads.json — DATA, not
# code — so that the staging code below names no adapter and the root
# installer stays host-agnostic.
RB_PAYLOAD_MANIFEST_RELPATH="scripts/runtime_payloads.json"

# The name of the file the payloads are declared in, relative to the
# repository root.
RB_PAYLOAD_MANIFEST="runtime_payloads.json"

# Paths a bundle must NEVER carry. These are the state half of the
# installation; a bundle that carried them would let a version switch
# replace the operator's data.
RB_FORBIDDEN_IN_BUNDLE="src/db src/backups backups mpm_config.json mode persona drills active.json"

# ---------- path helpers ----------

rb_releases_dir() { printf '%s\n' "$1/releases"; }
rb_current_link() { printf '%s\n' "$1/current"; }
rb_staging_dir()  { printf '%s\n' "$1/staging"; }

# ---------- canonical paths ----------
#
# rb_canon_path <path>
#
# The canonical absolute form of <path>: the components that exist are
# resolved (so a symlinked home directory compares equal to its
# target), the rest is normalised lexically, and the path is NOT
# required to exist.
#
# It exists to make one question decidable: "are these two paths the
# same directory?". Two paths are the same directory if and only if
# their canonical forms are equal.
rb_canon_path() {
    local p="$1" out=""
    [ -n "$p" ] || return 1
    if command -v realpath >/dev/null 2>&1; then
        if out="$(realpath -m -- "$p" 2>/dev/null)" && [ -n "$out" ]; then
            printf '%s\n' "$out"
            return 0
        fi
    fi
    if command -v python3 >/dev/null 2>&1; then
        if out="$(python3 -c 'import os,sys;print(os.path.realpath(sys.argv[1]))' "$p" 2>/dev/null)" \
           && [ -n "$out" ]; then
            printf '%s\n' "$out"
            return 0
        fi
    fi
    # Neither resolver is available: compare the paths lexically. This
    # is weaker (it does not resolve symlinks) but it never invents a
    # difference that is not there.
    printf '%s\n' "$p"
}

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

# ---------- the runtime package payloads ----------

# rb_payload_records <manifest>
#
# Emit one record per declared payload, in a form the shell can read:
#
#     <adapter>|<plugin id>|<validator>|<file> <file> ...
#
# The records come from a DATA file, so this function names no adapter:
# adding a host adapter is a data change, not a code change, and the
# root installer stays host-agnostic.
rb_payload_records() {
    python3 - "$1" <<'PY' 2>&1
import json, sys
try:
    with open(sys.argv[1]) as fh:
        doc = json.load(fh)
except Exception as exc:
    sys.exit(f"the runtime payload manifest could not be read: {exc}")
if not isinstance(doc, dict) or not isinstance(doc.get("payloads"), list):
    sys.exit("the runtime payload manifest has no 'payloads' list")
for entry in doc["payloads"]:
    for key in ("adapter", "plugin_id", "validator", "files"):
        if key not in entry:
            sys.exit(f"a runtime payload manifest entry is missing {key!r}: {entry!r}")
    if not entry["files"]:
        sys.exit(f"the runtime payload for {entry['adapter']!r} lists no files")
    print("|".join([
        str(entry["adapter"]),
        str(entry["plugin_id"]),
        str(entry["validator"]),
        " ".join(str(f) for f in entry["files"]),
    ]))
PY
}

# rb_stage_payloads <source_root> <bundle_dir>
#
# Stage every runtime package payload this release ships into the
# bundle, through the host-agnostic staging library and its explicit
# file allowlist.
#
# FAIL CLOSED. A host's registration points into the bundle. A bundle
# without a validated payload registers a plugin that loads NOTHING —
# silently, because the plugin registry stays empty. So a payload that
# cannot be staged, or a payload the release declares but does not
# ship, REFUSES the promotion.
#
# A release that ships NO payload at all is fine: the manifest declares
# none, and there is nothing to stage.
rb_stage_payloads() {
    local source_root="$1" bundle_dir="$2"
    local manifest="$source_root/$RB_PAYLOAD_MANIFEST_RELPATH"
    local stage_lib="$source_root/agent_installation/scripts/stage_runtime_package.sh"

    if [ ! -f "$manifest" ]; then
        rb_err "the runtime payload manifest is missing at $manifest"
        return 1
    fi
    if [ ! -f "$stage_lib" ]; then
        rb_err "the runtime package staging library is missing at $stage_lib"
        return 1
    fi

    local records
    records="$(rb_payload_records "$manifest")" || {
        rb_err "$records"
        return 1
    }
    if [ -z "$records" ]; then
        rb_log "  this release ships no runtime package payload"
        return 0
    fi

    # shellcheck source=/dev/null
    . "$stage_lib"

    local adapter plugin_id validator files src staged count=0
    local -a file_list
    while IFS='|' read -r adapter plugin_id validator files; do
        [ -n "$adapter" ] || continue
        count=$((count + 1))
        src="$source_root/agent_installation/$adapter"
        if [ ! -d "$src" ]; then
            rb_err "the release declares a runtime payload for '$adapter' but $src does not exist"
            return 1
        fi
        IFS=' ' read -r -a file_list <<< "$files"
        if ! staged="$(MPM_RUNTIME_PACKAGE_VALIDATOR="$validator" \
                stage_runtime_package "$src" "$bundle_dir" "$plugin_id" \
                "${file_list[@]}")"; then
            rb_err "the runtime payload for '$adapter' could not be staged and validated"
            return 1
        fi
        rb_log "  runtime payload staged: $staged"
    done <<< "$records"

    rb_log "  staged $count runtime package payload(s)"
    return 0
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

# THE LAYOUT HAS ONE ROOT.
#
# The stable paths are written with the RELATIVE target "current/<name>",
# so they resolve against the directory that holds them — the install
# prefix. The active pointer lives at <data root>/current. The two only
# agree when the prefix and the data root are the SAME directory.
#
# With two different directories the conversion creates a
# "current/<name>" symlink that resolves against a `current` that does
# not exist: a DANGLING symlink that names no binary and no host asset,
# while the conversion reports success. The pointer swap has already
# happened by then, so the installation is left pointing at a revision
# whose stable paths resolve to nothing.
#
# This is a REFUSAL, not a repair. Supporting two roots would need a
# second pointer — one per root — and two pointers are two sources of
# truth for which revision is active. The documented layout (docs/
# INSTALL.md §1g), the rehearsal's model (scripts/rehearsal/mpm-upgrade)
# and the migration plan (§4.3) all describe ONE root, and the
# conversion's own relative target is written for exactly that.

# rb_layout_paths_compatible <data_root> <prefix>
#
# 0 when the two name the same directory; non-zero, with the reason on
# stderr, when they do not.
rb_layout_paths_compatible() {
    local data_root="$1" prefix="$2"
    local canon_data canon_prefix

    if [ -z "$data_root" ] || [ -z "$prefix" ]; then
        rb_err "the versioned bundle layout needs both an install prefix and a data root"
        rb_err "  install prefix: ${prefix:-<empty>}"
        rb_err "  data root:      ${data_root:-<empty>}"
        return 1
    fi

    canon_data="$(rb_canon_path "$data_root")" || return 1
    canon_prefix="$(rb_canon_path "$prefix")" || return 1

    if [ "$canon_data" != "$canon_prefix" ]; then
        rb_err "the versioned bundle layout requires the install prefix and the data root to be the SAME directory"
        rb_err "the stable paths are written as the relative target 'current/<name>', which resolves inside the"
        rb_err "install prefix — but the active pointer lives at <data root>/current. With two different"
        rb_err "directories the stable paths would dangle while the conversion reported success."
        rb_err "  install prefix: $canon_prefix"
        rb_err "  data root:      $canon_data"
        rb_err "install with --prefix and --data-root naming the same directory, or install without --bundle"
        return 1
    fi
    return 0
}

# rb_legacy_path_name <path> <prefix>
#
# The name of the preserved directory <path>, or a non-zero return when
# <path> is not one. A preserved directory is
#
#     <prefix>/.<name>.legacy.<pid>
#
# where <name> is one of the convertible names.
#
# The check is what stops a malformed rollback record from naming an
# arbitrary path: acting on such an entry would MOVE that path into
# the install prefix, which is how a corrupted record becomes a
# destructive one.
#
# The result is in RB_LEGACY_NAME.
rb_legacy_path_name() {
    local path="$1" prefix="$2"
    local base dir suffix canon_prefix canon_dir
    RB_LEGACY_NAME=""

    base="${path##*/}"
    dir="${path%/*}"
    [ "$dir" != "$path" ] || dir="."

    case "$base" in
        .bin.legacy.*|.agent_installation.legacy.*) ;;
        *) return 1 ;;
    esac
    suffix="${base##*.legacy.}"
    case "$suffix" in
        ''|*[!0-9]*) return 1 ;;
    esac

    canon_prefix="$(rb_canon_path "$prefix")" || return 1
    canon_dir="$(rb_canon_path "$dir")" || return 1
    [ "$canon_dir" = "$canon_prefix" ] || return 1

    RB_LEGACY_NAME="${base#.}"
    RB_LEGACY_NAME="${RB_LEGACY_NAME%.legacy.*}"
    [ -n "$RB_LEGACY_NAME" ] || return 1
    return 0
}

# rb_convert_layout <data_root> <prefix> <current_target>
#
# The third argument is the value `current` holds, so the symlinks are
# written with the same relative target the pointer uses.
rb_convert_layout() {
    local data_root="$1" prefix="$2" target="${3:-}"
    local name p legacy rc=0
    [ -n "$target" ] || { rb_err "no active pointer target — refusing to convert the layout"; return 1; }

    # REFUSE BEFORE ANY FILESYSTEM MUTATION. A conversion that cannot
    # produce a resolvable stable path must not leave a dangling
    # symlink behind and call it success.
    rb_layout_paths_compatible "$data_root" "$prefix" || return 1

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
#
# FAIL CLOSED. The rollback record is the only statement of what was
# moved aside, so it is read as a statement that can be FALSE:
#
#   * A TRUNCATED record — one whose last line has no trailing
#     newline — must not be read as a shorter list. The dropped entry
#     is the operator's directory, and a shorter list restores less
#     while reporting a successful recovery.
#   * A MALFORMED entry — one that does not name a preserved
#     directory inside the install prefix — must not be acted on at
#     all. Acting on it would move an arbitrary path into the install
#     prefix.
#   * A MISSING or empty record must not hide conversion material that
#     is still sitting in the prefix: the conversion can be
#     interrupted between moving a directory aside and recording it.
#   * A FAILED revert must not consume the record. The record is the
#     operator's evidence of what is still unrestored, and the
#     installer points at it when the rollback does not complete.
#
# The function therefore returns 0 only when it can PROVE the layout is
# back to its pre-conversion shape: every recorded directory is
# restored, and no preserved directory is left in the prefix.
rb_revert_layout() {
    local data_root="$1" prefix="$2"
    local record="$data_root/.legacy-layout-paths"
    local legacy name rc=0
    local pending="" pending_tmp
    RB_LEGACY_NAME=""

    if [ -z "$data_root" ] || [ -z "$prefix" ]; then
        rb_err "the layout rollback needs both an install prefix and a data root"
        rb_err "  install prefix: ${prefix:-<empty>}"
        rb_err "  data root:      ${data_root:-<empty>}"
        return 1
    fi

    # ---- 1. the recorded entries ----
    #
    # `|| [ -n "$legacy" ]` is LOAD-BEARING. `read` alone returns
    # non-zero at EOF even when it has read a final line that has no
    # trailing newline, and `while read` drops that line — so a
    # truncated record would be read as a shorter list, and the
    # operator's directory would be silently left unrestored while
    # the rollback reported success.
    if [ -f "$record" ]; then
        while IFS= read -r legacy || [ -n "$legacy" ]; do
            [ -n "$legacy" ] || continue

            # An entry must name a preserved directory INSIDE the
            # install prefix. A malformed entry is reported and NOT
            # acted on: moving it would move an arbitrary path.
            if ! rb_legacy_path_name "$legacy" "$prefix"; then
                rb_err "the rollback record $record names '$legacy', which is not a preserved directory"
                rb_err "inside the install prefix — the record is malformed and the entry was NOT acted on"
                rc=1
                pending="$pending$legacy
"
                continue
            fi
            name="$RB_LEGACY_NAME"

            if [ ! -e "$legacy" ] && [ ! -L "$legacy" ]; then
                rb_err "the preserved directory $legacy is missing — it cannot be restored"
                rc=1
                pending="$pending$legacy
"
                continue
            fi

            if [ -L "$prefix/$name" ]; then
                if ! rm -f "$prefix/$name"; then
                    rb_err "could not remove the $name symlink at $prefix/$name"
                    rc=1
                    pending="$pending$legacy
"
                    continue
                fi
            elif [ -e "$prefix/$name" ]; then
                # Something real is sitting where the conversion put
                # its symlink. It is not ours to overwrite.
                rb_err "$prefix/$name is a real directory, not the symlink the conversion created —"
                rb_err "the preserved $legacy was NOT moved over it. Restore it by hand."
                rc=1
                pending="$pending$legacy
"
                continue
            fi

            # `mv -T` is LOAD-BEARING here too. Without it, `mv src dst`
            # where dst is an existing directory moves src INTO dst
            # instead of restoring it, so the operator's directory ends
            # up nested one level deeper and the rollback is reported
            # as a success.
            if ! mv -T "$legacy" "$prefix/$name" 2>/dev/null; then
                if ! rb_rename_over "$legacy" "$prefix/$name"; then
                    rb_err "could not restore $name from $legacy"
                    rc=1
                    pending="$pending$legacy
"
                    continue
                fi
            fi
            rb_log "  $name restored to its pre-upgrade directory"
        done < "$record"
    fi

    # ---- 2. the material the record did not name ----
    #
    # A missing or empty record cannot be taken as proof that nothing
    # was moved aside: the conversion can be interrupted between
    # moving a directory aside and recording it. The PREFIX is the
    # authority on what is still unrestored, so it is checked
    # directly. A preserved directory that is still there means the
    # conversion is not fully undone, whatever the record says.
    for name in bin agent_installation; do
        for legacy in "$prefix/.${name}.legacy."*; do
            [ -e "$legacy" ] || [ -L "$legacy" ] || continue
            rb_err "the preserved directory $legacy is still in the install prefix — the layout conversion"
            rb_err "is NOT fully undone. It is not named by the rollback record $record, or its restoration failed."
            rb_err "  restore it by hand:  rm -f $prefix/$name && mv -T $legacy $prefix/$name"
            rc=1
            pending="$pending$legacy
"
        done
    done

    # ---- 3. the record ----
    #
    # The record is consumed ONLY when the revert fully succeeded: a
    # second revert must not try to restore the same directories again.
    # A FAILED revert leaves the operator their evidence — the record
    # is rewritten to name exactly what is still pending, atomically,
    # because it is a file another process may read.
    if [ "$rc" -eq 0 ]; then
        # The record is consumed ONLY when there is one. A rollback on a
        # data root that was never converted must not leave behind a
        # file that says nothing.
        if [ -f "$record" ]; then
            : > "$record"
        fi
    elif [ -n "$pending" ] || [ -f "$record" ]; then
        pending_tmp="$record.pending.$$"
        if printf '%s' "$pending" > "$pending_tmp" 2>/dev/null &&
           rb_rename_over "$pending_tmp" "$record" 2>/dev/null; then
            :
        else
            rm -f "$pending_tmp" 2>/dev/null || true
            rb_err "the rollback record $record could not be rewritten; the pending entries are:"
            printf '%s' "$pending" >&2
        fi
    fi
    return "$rc"
}
