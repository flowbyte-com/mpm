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

# The standalone updater. It is RELEASE-SCOPED in the bundle
# (bin/mpm-update, so it rolls back with the release and is validated
# with everything else) AND provisioned a second time as a REAL file at
# the top of the data root, where a version switch cannot reach it.
#
# WHY BOTH. The updater is the tool that repairs a broken runtime, so it
# cannot live only inside the runtime it repairs: in the versioned layout
# bin/ is a symlink into the active release, so a binary placed there
# dies with that release — exactly what must not happen to the recovery
# tool. The data-root copy is state, survives version switches, and is
# what the operator runs directly when `current` is broken.
RB_UPDATER_BINARY="mpm-update"

# Rollback state for the standalone updater.
#
# The previous executable is COPIED aside (never moved out of the way)
# and a journal records whether there was one at all. Both are created
# before the replacement and removed only when the promotion commits, so
# a promotion that rolls back can put the previous recovery tool back
# exactly as it was — including the case where there was none.
RB_UPDATER_BACKUP_NAME=".$RB_UPDATER_BINARY.previous"
RB_UPDATER_JOURNAL_NAME=".$RB_UPDATER_BINARY.rollback"

# The runtime binaries the substrate itself needs, in the canonical
# promotion order. This is the list the Go verifier
# (internal/update/verify.go runtimeBinaries) enforces, and the list the
# non-bundle install path promotes. Kept separate so the difference is
# explicit: a release built before the updater existed has these five
# and no updater, and the updater must still be able to verify it.
RB_CORE_BINARIES="mpm mpm-scheduler mpm-critic mpm-mcp mpm-telemetry"

# The binaries a complete runtime contains: the five above plus the
# standalone updater. Defined once here so the bundle validator, the
# installer, the release assembler and the tests cannot disagree about
# what a complete runtime is.
#
# The updater is in this list because a bundle that omitted it could
# never provision the standalone copy: the installer would report a
# successful install over an installation with no recovery executable,
# and no later update could repair it (an already-current update returns
# before the provisioning step). See rb_provision_stable_updater below.
RB_BINARIES="$RB_CORE_BINARIES $RB_UPDATER_BINARY"

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

# Where the declaration travels INSIDE a bundle.
#
# A bundle must describe its own contents. The validator that checks a
# candidate must not depend on a source tree it does not have (a
# downloaded release) or on a validator built from a different revision
# than the bundle (a stale one checking a newer bundle). Shipping the
# declaration with the thing it declares removes both dependencies.
#
# The Go updater reads this copy (internal/update/payload.go,
# bundlePayloadManifestRelPath) and the shell stages it from the same
# file, so the two producers emit the same shape.
RB_BUNDLE_PAYLOAD_MANIFEST="agent_installation/runtime_payloads.json"

# Paths a bundle must NEVER carry. These are the state half of the
# installation; a bundle that carried them would let a version switch
# replace the operator's data.
RB_FORBIDDEN_IN_BUNDLE="src/db src/backups backups mpm_config.json mode persona drills active.json"

# RB_BUNDLE_ENTRIES is the bundle's top-level shape: every entry a
# complete bundle carries, and every entry the published archive must
# carry.
#
# WHY IT IS ONE LIST AND NOT TWO. The archive list used to be written
# out by hand in scripts/assemble-release-bundle.sh, and a test compared
# it against a COPY of the same list. Two copies of a list is exactly
# how the archive came to omit config/ — the directory that holds the
# host-asset declaration — while the bundle it was cut from carried it.
#
# So the shape is declared ONCE, here, and both the assembler and the
# test read it. A bundle entry added to this list is in the archive by
# construction.
RB_BUNDLE_ENTRIES="bin agent_installation config schema RELEASE.json"

# RB_SCHEMA_PREFLIGHT_REL is where the bundle carries the substrate's
# lifecycle_events DDL, so the schema-collision preflight has something
# to compare the live table against without a source checkout.
RB_SCHEMA_PREFLIGHT_REL="schema/lifecycle_events.sql"

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

    # The bundle carries its own declaration.
    #
    # The staged payload is validated HERE, but the bundle is validated
    # again later — by the installer, and by the updater, possibly by a
    # different build of the validator. A bundle that says what it
    # carries can be checked against its own contents; one that does
    # not can only be checked against a list somebody else remembers.
    local bundle_manifest="$bundle_dir/$RB_BUNDLE_PAYLOAD_MANIFEST"
    mkdir -p "$(dirname "$bundle_manifest")" || {
        rb_err "could not create the directory for the bundle's payload declaration"
        return 1
    }
    if ! cp -p "$manifest" "$bundle_manifest"; then
        rb_err "could not copy the runtime payload declaration into the bundle"
        return 1
    fi
    rb_log "  payload declaration carried at $RB_BUNDLE_PAYLOAD_MANIFEST"

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

# rb_validate_host_assets checks a bundle against its own host-asset
# declaration, and refuses the bundle when the declaration is absent,
# empty, incomplete, unsafe, or contradicted by the bundle's contents.
#
# WHY THIS IS A REFUSAL AND NOT A WARNING. The installed managed block
# advertises ~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md
# and ~/.mpm/agent_installation/mpm-agent-protocol.md, and the
# per-adapter installers import the shared scripts under
# agent_installation/scripts/ by path. A bundle that omits them installs
# an installation whose own instructions point at files that do not
# exist — and the omission is invisible until an agent follows the
# pointer.
#
# The bundle declares its assets (config/runtime-assets.json), so this
# check needs no source tree. The declaration is written by
# scripts/install_runtime_assets.py — the same authority that
# provisions the assets — so it cannot drift from what was installed.
#
# SCOPE. "candidate" is a bundle being assembled for installation: every
# file it carries must be declared. "installed" is a bundle that was
# already installed: only the declared assets are checked, because a
# release directory accumulates files after installation (the runtime's
# own bytecode caches, the adapter's build-provenance sidecar) and
# refusing it for those would push the operator towards deleting an
# immutable rollback target.
#
# This is the shell twin of internal/update/hostassets.go. The two must
# agree: the Go validator refuses a bundle the shell validator accepts,
# or the other way round, and the bundle that reaches an operator
# depends on which tool ran.
rb_validate_host_assets() {
    local bundle="$1" scope="${2:-candidate}"
    python3 - "$bundle" "$scope" <<'PY'
import hashlib, json, os, sys

bundle = sys.argv[1]
scope = sys.argv[2] if len(sys.argv) > 2 else "candidate"
problems = []

manifest_path = os.path.join(bundle, "config", "runtime-assets.json")
if not os.path.isfile(manifest_path):
    sys.stderr.write(
        "the bundle carries no host-asset declaration (config/runtime-assets.json); "
        "the integration assets the installed instructions point at would be missing\n")
    sys.exit(1)

try:
    manifest = json.load(open(manifest_path, "r", encoding="utf-8"))
except Exception as exc:
    sys.stderr.write(f"the bundle's host-asset declaration is not readable JSON: {exc}\n")
    sys.exit(1)

version = manifest.get("version")
if version != 1:
    sys.stderr.write(
        f"the bundle's host-asset declaration is version {version!r}; "
        "this build understands version 1\n")
    sys.exit(1)

assets = manifest.get("assets") or {}
if not assets:
    sys.stderr.write(
        "the bundle's host-asset declaration names no assets, so the integration "
        "assets were never provisioned\n")
    sys.exit(1)


def key_problem(rel):
    """Mirror internal/update/hostassets.go:hostAssetKeyProblem exactly.

    The rules are the authority's own (scripts/install_runtime_assets.py
    refuses an absolute path or a '..' segment), plus a normal-form
    requirement so the same file is not named two ways.
    """
    if not rel:
        return "the bundle's host-asset declaration names an empty path"
    if "\\" in rel:
        return (f"the bundle's host-asset declaration names {rel!r}, which contains a "
                "backslash — an asset path uses '/' as its separator")
    if rel.startswith("/") or os.path.isabs(rel):
        return (f"the bundle's host-asset declaration names {rel!r}, which is an absolute "
                "path — an asset path is relative to the bundle root")
    if len(rel) >= 2 and rel[1] == ":":
        return (f"the bundle's host-asset declaration names {rel!r}, which carries a drive "
                "prefix — an asset path is relative to the bundle root")
    for seg in rel.split("/"):
        if seg == "..":
            return (f"the bundle's host-asset declaration names {rel!r}, which contains a "
                    "'..' segment — an asset path cannot leave the bundle root")
    if os.path.normpath(rel) != rel:
        return (f"the bundle's host-asset declaration names {rel!r}, which is not a plain "
                "relative path — the declaration must name the asset exactly as it sits "
                "in the bundle")
    return None


def stays_inside(root, resolved):
    """The containment backstop: the resolved path must remain under root."""
    root = os.path.realpath(root)
    parent = os.path.realpath(os.path.dirname(resolved))
    candidate = os.path.join(parent, os.path.basename(resolved))
    return candidate == root or candidate.startswith(root + os.sep)


declared = set()
for rel, rec in sorted(assets.items()):
    why = key_problem(rel)
    if why:
        problems.append(why)
        continue
    declared.add(rel)
    full = os.path.join(bundle, *rel.split("/"))
    if not stays_inside(bundle, full):
        problems.append(
            f"the bundle declares the host asset {rel!r}, which resolves outside the "
            "bundle root")
        continue
    if not os.path.isfile(full) or os.path.islink(full):
        problems.append(f"the bundle declares the host asset {rel!r} but does not carry it")
        continue
    want = (rec or {}).get("sha256", "")
    if not want:
        problems.append(f"the bundle declares the host asset {rel!r} with no digest")
        continue
    h = hashlib.sha256()
    with open(full, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    got = h.hexdigest()
    if got.lower() != want.lower():
        problems.append(
            f"the bundle's host asset {rel!r} has digest {got} but the declaration says {want}")

# The completeness rule is a CONSTRUCTION rule: it applies to a
# candidate, not to a release that has been in use.
if scope == "candidate":
    # The payload files are declared by the bundle's own runtime payload
    # declaration, not by the asset declaration.
    payload_manifest = os.path.join(bundle, "agent_installation", "runtime_payloads.json")
    if not os.path.isfile(payload_manifest):
        problems.append("the bundle carries no runtime payload declaration (agent_installation/runtime_payloads.json)")
    else:
        declared.add("agent_installation/runtime_payloads.json")
        try:
            payload = json.load(open(payload_manifest, "r", encoding="utf-8"))
            for record in payload.get("payloads", []):
                for f in record.get("files", []):
                    declared.add(f"agent_installation/{record.get('adapter', '')}/{f}")
        except Exception as exc:
            problems.append(f"the bundle's runtime payload declaration is not readable JSON: {exc}")

    # Every file under agent_installation/ must be declared by something.
    root = os.path.join(bundle, "agent_installation")
    for dirpath, _dirnames, filenames in os.walk(root):
        for name in filenames:
            full = os.path.join(dirpath, name)
            rel = os.path.relpath(full, bundle).replace(os.sep, "/")
            if rel not in declared:
                problems.append(
                    f"the bundle carries {rel!r} under agent_installation/ but no declaration names it")

for problem in problems:
    sys.stderr.write(problem + "\n")
sys.exit(1 if problems else 0)
PY
}

# rb_validate_schema_preflight checks the bundle's schema preflight.
#
# WHY IT IS CHECKED. The schema-collision preflight compares the live
# table against the DDL the substrate itself would apply. A bundle that
# does not carry that DDL degrades the check to a no-op, silently — the
# check reports success because it compared the live table against
# nothing.
#
# REQUIRED for a candidate. OPTIONAL for an installed release: a release
# promoted before the preflight travelled with the bundle may not carry
# one, which is a documented historic gap rather than a corruption. If
# the file IS there it must be valid in both scopes.
#
# The rule is the same one internal/update/ddl.go applies, so the two
# validators cannot disagree about what counts as a preflight.
rb_validate_schema_preflight() {
    local bundle="$1" required="${2:-1}"
    python3 - "$bundle" "$required" "$RB_SCHEMA_PREFLIGHT_REL" <<'PY'
import os, re, sys

bundle = sys.argv[1]
required = sys.argv[2] != "0"
# The path comes from the shell constant, so the shell validator and
# the Go validator (internal/update/ddl.go:SchemaPreflightRelPath)
# cannot drift apart.
rel = sys.argv[3] if len(sys.argv) > 3 else "schema/lifecycle_events.sql"
path = os.path.join(bundle, *rel.split("/"))

if not os.path.isfile(path) or os.path.islink(path):
    if required:
        sys.stderr.write(
            f"the bundle carries no schema preflight ({rel}); the schema-collision "
            "preflight would degrade to a no-op\n")
        sys.exit(1)
    sys.exit(0)

try:
    text = open(path, "r", encoding="utf-8").read()
except Exception as exc:
    sys.stderr.write(f"the bundle's schema preflight ({rel}) could not be read: {exc}\n")
    sys.exit(1)

if not text.strip():
    sys.stderr.write(
        f"the bundle's schema preflight ({rel}) is empty; the schema-collision preflight "
        "would have nothing to compare the live table against\n")
    sys.exit(1)

if not re.search(r"CREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?lifecycle_events\b",
                 text, re.IGNORECASE):
    sys.stderr.write(
        f"the bundle's schema preflight ({rel}) does not declare the lifecycle_events "
        "table; it must carry the substrate's own CREATE TABLE for it\n")
    sys.exit(1)

sys.exit(0)
PY
}


# ---------- validation ----------

# rb_reported_commit <binary>
#
# The build commit a runtime binary reports about itself.
#
# WHY THE CONSUMER READS TO EOF
# -----------------------------
# The obvious way to write this is
#
#     "$binary" --version | awk '/commit:/{print $2; exit}'
#
# and it is wrong in a way that only shows up when the producer has
# more than one thing to say. The `exit` closes the read end of the
# pipe the moment the commit line is seen. If the producer still has
# output to write — a second section, a trailing line, anything — that
# write now fails with EPIPE and the producer is killed by SIGPIPE.
#
# Under `set -o pipefail` the pipeline's status is then the producer's
# SIGPIPE status, 141. The statement that captures it is a bare
# assignment under `set -e`, so 141 becomes the CALLER's exit status:
# an installation that dies with 141 and says nothing at all about what
# it was doing. The identity was extracted correctly; only the STATUS
# was wrong, and the status is what killed the run.
#
# Reading to EOF costs one extra read and cannot change the value: the
# FIRST commit line is still the one that is reported. The count is
# what keeps it to the first one, and the ABSENCE of `exit` is what
# keeps the pipe open until the producer is done.
#
# WHY THE PRODUCER'S STATUS IS THE HELPER'S BUSINESS
# --------------------------------------------------
# The probe is a pipeline, and a pipeline has no status of its own.
# Without `pipefail` its status is the LAST command's — the parser's —
# and with `pipefail` it is the rightmost NONZERO status, which is the
# producer's only when the producer failed AND the parser did not.
#
# That made the producer's failure a property of the CALLER's shell
# configuration rather than of the probe:
#
#   * `scripts/assemble-release-bundle.sh` runs with `set -u` only, so
#     the pipeline's status was the parser's 0, and a binary that
#     printed a valid commit and THEN failed had that commit accepted;
#   * a helper invoked inside `if ! ...` has `errexit` SUSPENDED for
#     its whole body, so even under `pipefail` the failed assignment
#     did not stop `rb_validate_bundle` — the validation carried on
#     with the very commit the failing producer had printed.
#
# So the two statuses are read AT THE PIPELINE BOUNDARY, inside a
# subshell whose shell options are this helper's own: `errexit` off, so
# the pipeline cannot abort before its statuses have been read, and the
# producer's and the parser's statuses taken from `PIPESTATUS`
# separately, so neither is inferred from the other. Nothing here
# depends on the caller's `pipefail`, and nothing here turns the
# caller's `errexit` off.
#
# THE CONTRACT, which is the same for every caller:
#
#   * 0 — the producer exited 0 and the parser exited 0. The commit is
#     on stdout, and it is EMPTY if the producer reported no commit.
#   * the PRODUCER's own status — the producer failed. Nothing is
#     printed on stdout: a failed producer yields NO identity.
#   * the PARSER's status — the parser failed.
#
# A caller must therefore CHECK the status, not only the value: the
# value alone cannot distinguish "the binary reported no commit" from
# "the binary failed to report anything at all".
#
# The Go mirror of these checks (internal/update/verify.go) has always
# failed closed on a producer that exits nonzero — it treats the failed
# run as "no commit reported" and refuses the bundle. This helper now
# agrees with it.
rb_reported_commit() {
    local binary="${1:-}"
    [ -n "$binary" ] || return 1

    # `status` is the helper's own verdict; `out` is the identity, and
    # it is printed only when the probe succeeded.
    local out status=0
    out="$(
        # errexit off: the pipeline's status is read below, not acted on
        # here. pipefail off: neither status is inferred from the
        # pipeline's, because PIPESTATUS carries both of them.
        set +e
        set +o pipefail
        "$binary" --version 2>/dev/null | awk '/commit:/{ if (count == 0) { print $2; count = 1 } }'
        # BOTH statuses are read in ONE command: PIPESTATUS is
        # overwritten by every command, so reading them one at a time
        # would lose the producer's.
        local producer_status="${PIPESTATUS[0]}" parser_status="${PIPESTATUS[1]}"
        if [ "$producer_status" -ne 0 ]; then
            printf 'rb_reported_commit: %s --version failed (exit %s)\n' \
                "$binary" "$producer_status" >&2
            exit "$producer_status"
        fi
        if [ "$parser_status" -ne 0 ]; then
            printf 'rb_reported_commit: the commit parser failed (exit %s)\n' \
                "$parser_status" >&2
            exit "$parser_status"
        fi
        exit 0
    )" || status=$?

    if [ "$status" -ne 0 ]; then
        # The reason is already on stderr. No identity is printed: a
        # failed probe yields nothing for a caller to trust.
        return "$status"
    fi
    printf '%s\n' "$out"
    return 0
}

# rb_version_output <binary>
#
# The binary's own version output, fail-closed.
#
# WHY IT EXISTS
# -------------
# rb_reported_commit answers ONE question: what COMMIT does the binary
# report? Some callers need a different answer. The rehearsal asserts
# the IDENTITY FORMAT — "MPM 0.1.0-dev+g<short-commit>.t<build-epoch>" —
# which is the FIRST LINE of the output and is not a field of the
# commit. This helper returns the output itself, so the caller can take
# what it needs from the captured string.
#
# WHY IT IS NOT A PIPELINE
# -----------------------
# rb_reported_commit is a pipeline because it PARSES while it reads.
# This helper does not parse: it returns the output and lets the caller
# take what it needs from the captured string afterwards. So there is no
# pipe — and a pipe is where the SIGPIPE hazard lives (see
# rb_reported_commit's header above). A parser that stops at the first
# line closes the pipe, and a producer that still has output to write
# is killed with SIGPIPE, exit 141.
#
# Reading the producer's output with a command substitution reads it to
# EOF and cannot close the pipe early, so no parser can kill the
# producer. And because the probe is a single COMMAND and not a
# pipeline, its status IS the producer's status: there is no second
# status to reconcile, and nothing depends on the caller's `pipefail`.
#
# THE CONTRACT, which is the same shape as rb_reported_commit's:
#
#   * 0 — the producer exited 0. The output is on stdout, and it is
#     EMPTY if the producer reported nothing at all.
#   * the PRODUCER's own status — the producer failed. Nothing is
#     printed on stdout: a failed producer yields NO output.
#
# A caller must therefore CHECK the status, not only the value: the
# value alone cannot distinguish "the binary reported nothing" from
# "the binary failed to report anything at all".
#
# The producer's stderr is discarded, exactly as rb_reported_commit
# discards it. A version probe reads the identity, and a diagnostic
# line mixed into the captured output would corrupt it.
rb_version_output() {
    local binary="${1:-}"
    [ -n "$binary" ] || return 1

    # `out` and `status` are assigned SEPARATELY on purpose:
    # `local out="$(...)"` would mask the command substitution's status
    # behind `local`'s own, and the status is the whole point.
    local out status=0
    out="$("$binary" --version 2>/dev/null)" || status=$?
    if [ "$status" -ne 0 ]; then
        printf 'rb_version_output: %s --version failed (exit %s)\n' \
            "$binary" "$status" >&2
        return "$status"
    fi
    printf '%s\n' "$out"
    return 0
}

# rb_version_first_line <binary>
#
# The binary's build identity — the FIRST line of its version output.
#
# Same contract as rb_version_output, and the same reason it exists:
# the identity assertions match against the first line, and the first
# line is taken from the CAPTURED output, never from a pipe. Nothing
# closes the pipe early, so a producer that still has output to write
# is never killed with SIGPIPE.
#
# For the mpm binaries the first line is the build identity:
#
#     MPM 0.1.0-dev+g06c029ba.t1792679720
#
# The telemetry sidecar prints its own heading first ("MPM · Telemetry ·
# version"), so its first line is that heading and its build identity is
# on a later line. That is a property of the sidecar's renderer, not of
# this helper, and a caller that needs the sidecar's identity asks for
# the COMMIT instead.
rb_version_first_line() {
    local binary="${1:-}"
    [ -n "$binary" ] || return 1

    # The probe itself is rb_version_output's, so the contract is
    # defined once. Its diagnostic is suppressed here and replaced with
    # one that names THIS helper — a caller that asked for the first
    # line should not be told about rb_version_output.
    local out status=0
    out="$(rb_version_output "$binary" 2>/dev/null)" || status=$?
    if [ "$status" -ne 0 ]; then
        printf 'rb_version_first_line: %s --version failed (exit %s)\n' \
            "$binary" "$status" >&2
        return "$status"
    fi
    # The first line, taken from the captured string: everything up to
    # the first newline. A command substitution strips trailing
    # newlines, so a single-line output has no newline left and
    # `%%` leaves it whole.
    printf '%s\n' "${out%%$'\n'*}"
    return 0
}

# rb_validate_bundle <bundle_dir> [expected_rev]
#
# Everything that must be true before a bundle is allowed to become the
# active runtime. A refusal here means the bundle is NOT installed and
# the pointer is NOT moved — the installation keeps running whatever it
# was running before.

# rb_validate_bundle proves a CANDIDATE bundle before it is exposed.
#
# EVERY FILE IT CARRIES MUST BE ACCOUNTED FOR. A candidate is assembled
# in a fresh staging directory, so an undeclared file in one is a file
# nobody provisioned and nobody owns.
#
# rb_validate_installed_release checks a bundle that was ALREADY
# INSTALLED, and asks a different question: is it INTACT? A release
# directory accumulates files after installation — the runtime's own
# Python bytecode caches, the adapter's build-provenance sidecar — and
# the construction rules would refuse a release for being used. The
# declared assets must still be present with the digests they declare;
# that is what detects a corrupted release.
#
# The host-asset DECLARATION is required in both: a release that
# carries none was built before the host-asset contract existed and is
# genuinely incomplete, and detecting that must not be traded away for
# the convenience of an installed directory.
rb_validate_bundle() {
    local bundle="$1" expected_rev="${2:-}"
    rb_validate_bundle_scope "$bundle" "$expected_rev" candidate
}

rb_validate_installed_release() {
    local bundle="$1" expected_rev="${2:-}"
    rb_validate_bundle_scope "$bundle" "$expected_rev" installed
}

rb_validate_bundle_scope() {
    local bundle="$1" expected_rev="${2:-}" scope="${3:-candidate}"
    local bin rel

    [ -d "$bundle" ] || { rb_err "bundle directory does not exist: $bundle"; return 1; }

    # 1. The runtime binaries (the five substrate binaries plus the
    #    standalone updater — see RB_BINARIES). A release carrying no
    #    updater would be one whose recovery executable can never be
    #    provisioned, so its absence is a validation failure, not a
    #    warning.
    for bin in $RB_BINARIES; do
        local p="$bundle/bin/$bin"
        if [ ! -e "$p" ]; then
            rb_err "bundle is missing bin/$bin"
            if [ "$bin" = "$RB_UPDATER_BINARY" ]; then
                rb_err "the updater is the recovery executable: without it a broken runtime has no"
                rb_err "repair path, and no later update can create one (an already-current update"
                rb_err "returns before the provisioning step)."
            fi
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
    #
    #    The probe's STATUS is checked as well as its value, and the
    #    check is explicit (`if ! ...`) rather than implied by the
    #    assignment's status: this function is called from inside `if
    #    ! rb_validate_bundle ...`, and `errexit` is SUSPENDED for the
    #    whole body of a function invoked in a condition context — so a
    #    failed assignment would not have stopped the validation.
    local commit reported
    commit="$(rb_release_field "$bundle" commit)"
    if [ -n "$commit" ] && [ "$commit" != "unknown" ]; then
        reported=""
        if ! reported="$(rb_reported_commit "$bundle/bin/mpm")"; then
            rb_err "the bundle's mpm binary failed to report its build commit"
            rb_err "(its --version exited nonzero — a binary that cannot report its own"
            rb_err "identity successfully cannot be trusted to report the right one)"
            return 1
        fi
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

    # 6. The release-scoped host assets.
    #
    # The bundle declares them (config/runtime-assets.json) and the
    # declaration is checked against the bundle's own contents. A
    # bundle without the integration assets installs an installation
    # whose managed block points at files that do not exist.
    if ! rb_validate_host_assets "$bundle" "$scope"; then
        rb_err "the bundle's host integration assets are missing or do not match the bundle's own declaration"
        return 1
    fi

    # 7. The schema preflight.
    #
    # The schema-collision preflight compares the live table against the
    # DDL the substrate itself would apply. A bundle that does not carry
    # that DDL degrades the check to a no-op, silently.
    local preflight_required=0
    [ "$scope" = "candidate" ] && preflight_required=1
    if ! rb_validate_schema_preflight "$bundle" "$preflight_required"; then
        rb_err "the bundle's schema preflight is missing or does not declare the table it exists to protect"
        return 1
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

# ---------- the standalone recovery executable ----------

# rb_provision_stable_updater <data_root> <release_dir> [rev]
#
# Install the release's updater as a REAL file at <data_root>/mpm-update,
# beside the state rather than inside a release.
#
# WHY THIS EXISTS. The release bundle carries bin/mpm-update so that it
# is versioned, validates with the rest of the runtime and rolls back
# with it. That copy is NOT reachable as a recovery path: in the
# versioned layout <data root>/bin is a symlink into the active release,
# so a binary there dies with the release it is meant to repair. The
# recovery executable must be independent of the thing it recovers, so a
# second, stable copy is provisioned here, at the top of the data root,
# where a version switch cannot reach it.
#
# ATOMICITY. The previous executable is COPIED aside and the validated
# staged copy is renamed straight over the destination. There is no
# interval in which <data root>/mpm-update does not exist — the recovery
# tool must not be missing at the moment it becomes necessary. Any failure
# removes the staged file and leaves the existing executable exactly as it
# was. That is the difference between "the installer failed" and "the
# installer failed and now you cannot recover either".
#
# Returns 0 when the data root holds a good standalone updater; non-zero
# when it does not, having left whatever was there before untouched.
rb_provision_stable_updater() {
    local data_root="$1" release_dir="$2" rev="${3:-}"
    local src="$release_dir/bin/$RB_UPDATER_BINARY"
    local dst="$data_root/$RB_UPDATER_BINARY"
    local stage="$data_root/.$RB_UPDATER_BINARY.stage.$$"
    local previous="$data_root/$RB_UPDATER_BACKUP_NAME"
    local journal="$data_root/$RB_UPDATER_JOURNAL_NAME"

    # 1. The SOURCE must be the validated, executable release copy. A
    #    bundle that reached here without it is refused rather than
    #    silently reported as provisioned (the defect this closes: a
    #    success banner over an installation with no recovery tool).
    if [ ! -f "$src" ]; then
        rb_err "the release carries no bin/$RB_UPDATER_BINARY (expected $src)"
        rb_err "refusing to activate a runtime whose recovery executable cannot be provisioned"
        return 1
    fi
    if [ ! -x "$src" ]; then
        rb_err "the release's bin/$RB_UPDATER_BINARY is not executable"
        return 1
    fi

    # 2. Stage beside the destination. Nothing visible has changed yet.
    rm -f "$stage" 2>/dev/null || true
    if ! cp "$src" "$stage"; then
        rb_err "could not stage the standalone updater at $stage"
        rm -f "$stage" 2>/dev/null || true
        return 1
    fi
    if ! chmod 0755 "$stage"; then
        rb_err "could not make the staged updater executable"
        rm -f "$stage" 2>/dev/null || true
        return 1
    fi

    # 3. VALIDATE BEFORE PROMOTION. The staged copy must be a regular,
    #    executable, byte-identical copy of the release's updater. A
    #    truncated or substituted copy must never reach the data root,
    #    because this is the file an operator runs when nothing else
    #    works.
    local reason=""
    if [ -L "$stage" ]; then
        reason="the staged updater is a symlink"
    elif [ ! -f "$stage" ]; then
        reason="the staged updater is not a regular file"
    elif [ ! -x "$stage" ]; then
        reason="the staged updater is not executable"
    elif ! cmp -s "$src" "$stage"; then
        reason="the staged updater is not byte-identical to the release's copy"
    fi
    if [ -n "$reason" ]; then
        rb_err "$reason — refusing to provision a recovery executable that could be wrong"
        rm -f "$stage" 2>/dev/null || true
        return 1
    fi

    # 4. PRESERVE, THEN REPLACE IN ONE RENAME.
    #
    #    The previous executable is COPIED aside, never moved: moving it
    #    would leave <data root>/mpm-update absent for the interval
    #    between the two renames, and that interval is exactly when an
    #    operator whose runtime just failed might run it. There must be no
    #    instant at which the recovery executable does not exist.
    #
    #    So: copy the old one aside (a rollback copy, not a displacement),
    #    then rename(2) the validated staged file straight over the
    #    destination. rename(2) is atomic, so a reader sees either the old
    #    executable or the new one — never neither, never a truncated file.
    #
    #    rb_rename_over, NOT `mv -f`. GNU mv treats a destination that is a
    #    SYMLINK TO A DIRECTORY as a directory operand and moves the source
    #    INSIDE the linked directory, leaving the symlink in place. That is
    #    a containment violation — the recovery executable would be written
    #    into whatever directory the symlink named and `mpm-update` would
    #    still not be an executable. rb_rename_over uses `mv -T` (or
    #    os.rename), which replaces the destination entry itself.
    local previous_state="none"
    if [ -e "$dst" ] || [ -L "$dst" ]; then
        if [ ! -f "$dst" ] && [ ! -L "$dst" ]; then
            rb_err "the existing updater at $dst is neither a regular file nor a symlink"
            rm -f "$stage" 2>/dev/null || true
            return 1
        fi
        rm -f "$previous" 2>/dev/null || true
        # -a preserves mode and symlinks as symlinks, so an odd prior
        # state is restored faithfully rather than being normalised away.
        #
        # $previous is removed immediately above, so this writes into a
        # path that does not exist: it cannot descend into a stale symlink
        # to a directory. (A same-UID process racing that one-instruction
        # window is outside the threat model — the bundle lock already
        # excludes concurrent installers.)
        if ! cp -a "$dst" "$previous"; then
            rb_err "could not preserve the existing updater at $previous"
            rm -f "$stage" "$previous" 2>/dev/null || true
            return 1
        fi
        previous_state="previous"
    fi

    # The journal is written BEFORE the swap. A crash between the swap and
    # the journal would otherwise leave a new executable with no record of
    # what to put back, and the promotion's rollback would silently keep
    # the wrong one.
    rm -f "$journal" 2>/dev/null || true
    if ! printf '%s\n' "$previous_state" > "$journal"; then
        rb_err "could not record the updater rollback state at $journal"
        rm -f "$stage" "$previous" 2>/dev/null || true
        return 1
    fi

    if ! rb_rename_over "$stage" "$dst"; then
        # rename(2) failed, so the destination is UNTOUCHED — the old
        # executable is still in place. Nothing to restore.
        rb_err "could not install the standalone updater at $dst"
        rm -f "$stage" "$previous" "$journal" 2>/dev/null || true
        return 1
    fi

    # 5. Read back. The provisioning step reports success only for a real
    #    file: a symlink here would resolve into a release, which is the
    #    failure mode this function exists to prevent.
    if [ -L "$dst" ] || [ ! -f "$dst" ] || [ ! -x "$dst" ]; then
        rb_err "the standalone updater at $dst is not a real executable after provisioning"
        return 1
    fi

    rb_log "  standalone updater: $dst (real file, independent of releases/${rev:-?})"
    return 0
}

# rb_restore_stable_updater <data_root>
#
# Put back whatever the last rb_provision_stable_updater replaced, using
# the journal it wrote. Called when a promotion ROLLS BACK: the pointer
# is being returned to the previous runtime, so the recovery executable
# must be returned with it — otherwise the installation is left with a
# recovery tool from a release that is no longer active.
#
# A first installation is the interesting case: the journal says "none",
# and the restored state is "no standalone updater". Removing it is
# correct, not destructive — there was nothing there before.
#
# Returns 0 when the data root matches its pre-provisioning state (or when
# provisioning never ran), non-zero when it does not.
#
# The restore uses rb_rename_over for the same reason provisioning does: a
# destination that is a symlink to a directory must be REPLACED, not
# entered. A saved state that was itself a symlink is restored as that
# exact symlink, because the journal's copy was taken with `cp -a` (which
# does not dereference).
rb_restore_stable_updater() {
    local data_root="$1"
    local dst="$data_root/$RB_UPDATER_BINARY"
    local previous="$data_root/$RB_UPDATER_BACKUP_NAME"
    local journal="$data_root/$RB_UPDATER_JOURNAL_NAME"

    # No journal: provisioning never reached the swap, so the destination
    # is whatever it was. Nothing to undo.
    [ -e "$journal" ] || return 0

    local state
    state="$(cat "$journal" 2>/dev/null || true)"
    case "$state" in
        previous)
            if [ ! -e "$previous" ] && [ ! -L "$previous" ]; then
                rb_err "the journal says a previous updater existed but $previous is gone"
                return 1
            fi
            if ! rb_rename_over "$previous" "$dst"; then
                rb_err "could not restore the previous updater from $previous"
                return 1
            fi
            rb_log "  recovery executable restored to its previous version"
            ;;
        none)
            # `rm` never follows a symlink operand, so this removes the
            # entry (real file or link) and never touches anything it
            # pointed at.
            rm -f "$dst" 2>/dev/null || true
            rb_log "  recovery executable removed (there was none before this promotion)"
            ;;
        *)
            rb_err "unreadable updater rollback journal at $journal (content: ${state:-<empty>})"
            return 1
            ;;
    esac
    rm -f "$journal" 2>/dev/null || true
    return 0
}

# rb_commit_stable_updater <data_root>
#
# Drop the rollback state once a promotion has fully committed. Until this
# runs, the previous executable and its journal are the material a
# rollback needs.
rb_commit_stable_updater() {
    local data_root="$1"
    rm -f "$data_root/$RB_UPDATER_BACKUP_NAME" 2>/dev/null || true
    rm -f "$data_root/$RB_UPDATER_JOURNAL_NAME" 2>/dev/null || true
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
