#!/usr/bin/env bash
#
# install.sh — Materialize the OpenCode ↔ MPM integration.
#
# Idempotent. Performs four ordered responsibilities:
#
#   1. Compile dist/ from canonical source (if dist/index.v2.js is
#      missing, older than any module under src/, or was built from a
#      different source revision). The build DEPENDENCIES are installed
#      by this script when they are missing — `npm ci` against the
#      committed package-lock.json. There is no manual compilation
#      step: an installation that cannot build the payload says so and
#      registers nothing.
#   2. Provision the compiled payload into the MPM runtime root via the
#      shared stage_runtime_package.sh library, and validate it. The
#      plugin[] entry is never written for a path that holds nothing.
#   3. Refresh the OpenCode plugin config entry in
#      ~/.config/opencode/opencode.jsonc. Detects and migrates
#      STALE entries that reference the pre-2026-09-17 legacy
#      'opencode-mpm' directory. New entries use the canonical
#      'mpm-opencode' path.
#   4. Refresh the AGENTS.md behavioral managed block (delegates to
#      install_agents_instructions.py for the actual content work).
#
# Migration behavior (Part 3 — registration refresh):
#
#   Before 2026-09-17: opencode.jsonc pointed at
#       file:///.../.mpm/agent_installation/opencode-mpm/dist/index.js
#   2026-09-17 .. 2026-10-16: opencode.jsonc pointed at
#       file:///.../.mpm/agent_installation/mpm-opencode/dist/index.js
#   After this script: opencode.jsonc points at the PACKAGE DIRECTORY
#       file:///.../.mpm/agent_installation/mpm-opencode
#
#   The 2026-09-17 namespace refresh fixed `opencode-mpm` → `mpm-opencode`
#   but kept registering a FILE url. OpenCode 2 (v2.0.30, verified live)
#   refuses a file target ("configured plugin path must be a directory")
#   and, once a directory is registered, resolves it by looking for a
#   ROOT-LEVEL `server.*` / `index.*` module inside the directory — it
#   never consults package.json, and it drops a directory without one
#   SILENTLY (no warning; /api/plugin stays all-builtin). The
#   dist/index.js registration therefore loaded nothing from 2026-09-17
#   through 2026-10-16 while every install and --verify reported success.
#
#   The staged package now carries `server.js` at its root — a shim that
#   re-exports the compiled payload in dist/index.v2.js — and the staged
#   contract is validated for exactly that shape before registration.
#
#   The legacy directory at ~/.mpm/agent_installation/opencode-mpm/
#   is REPO-OWNED state (left behind by the namespace migration
#   `git mv`). If it still exists, we remove ONLY the generated
#   dist/ subdirectory (which is .gitignored and never tracked).
#   Any user-installed source code in the old directory is left
#   alone.
#
#   The script does NOT:
#     - delete ~/.config/opencode/opencode.jsonc
#     - rewrite unrelated plugin entries
#     - delete files outside the legacy MPM-owned dist/
#
# External filesystem references — ownership invariant
# ----------------------------------------------------
#
# This installer writes state ONLY under:
#   - $HOME/.mpm/agent_installation/mpm-opencode/  (the integration itself)
#   - $HOME/.config/opencode/                       (host config managed blocks)
#
# It does NOT create entries inside npm's global tree, /usr/local,
# or any other package-manager-owned directory. If a future installer
# must create an external reference (for example, to expose a plugin
# dependency to a host that requires it on a global path), that
# reference is MPM-owned by construction and MUST ship with:
#   - explicit ownership/provenance so a future namespace rename
#     can locate it
#   - migration logic in the namespace-refresh path (Part 3 above)
#   - uninstall handling that removes it
#   - verification (--verify) that surfaces it as a failure when
#     dangling
#
# Rationale: a dangling symlink inside an unrelated package manager's
# state (e.g. npm's global node_modules) can corrupt that package
# manager's view of its own tree. The 2026-09-25 OpenCode incident
# showed that npm 12 + @npmcli/arborist silently swallow ENOENT when
# readdirScoped hits a dangling @-scoped entry, making the entire
# global tree invisible to `npm ls -g` and `npm rm -g`. MPM must not
# create that class of latent external-state unless it owns the
# cleanup. It does not own cleanup of references it did not create.
#
# Usage:
#   ./install.sh             # install / refresh
#   ./install.sh --uninstall # remove plugin entry + AGENTS.md block
#   ./install.sh --verify    # run validation tests
#
# CWD-independent: the plugin directory is resolved from BASH_SOURCE[0],
# so the installer works from any current working directory.

set -euo pipefail

# --------------------------------------------------------------------------
# Identity + paths
# --------------------------------------------------------------------------

# Resolve our own directory without depending on caller CWD.
SCRIPT_PATH="${BASH_SOURCE[0]}"
# Resolve symlinks so ./install.sh that points elsewhere still works.
while [ -L "$SCRIPT_PATH" ]; do
    LINK_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"
    SCRIPT_PATH="$(readlink "$SCRIPT_PATH")"
    case "$SCRIPT_PATH" in
        /*) ;;
        *)  SCRIPT_PATH="$LINK_DIR/$SCRIPT_PATH" ;;
    esac
done
SCRIPT_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"

# OpenCode adapter identity comes from the exported "id" field in
# src/index.ts. We can also read the package name from package.json as a
# stable surrogate; the install does not need to compile TS.
PLUGIN_ID="$(grep -oE '"name"[[:space:]]*:[[:space:]]*"[^"]+"' "$SCRIPT_DIR/package.json" \
    | head -n1 | sed -E 's/.*"name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
if [ -z "$PLUGIN_ID" ]; then
    printf '[mpm-opencode install] ERROR: could not parse plugin name from %s/package.json\n' "$SCRIPT_DIR" >&2
    exit 2
fi

# Canonical MPM install — same resolution rule as mpm-opencode/src/index.ts
# and mpm-memory-openclaw/install.sh. Order matters: the substrate install
# (install.sh) writes ~/.mpm/bin/mpm and symlinks it into
# ~/.local/bin/mpm. We accept either. We never rely on PATH resolution
# alone — on a freshly-created ~/.local/bin the current login session
# may not have it on PATH yet (proved in clean-profile Linux Mint test).
HOME_DIR="${HOME:?HOME must be set}"
OPENCODE_CONFIG_DIR="${HOME_DIR}/.config/opencode"
OPENCODE_CONFIG="${OPENCODE_CONFIG_DIR}/opencode.jsonc"
OPENCODE_PLUGIN_DIR="${OPENCODE_CONFIG_DIR}/plugin"

# Where MPM keeps installed adapter assets.
#
# MPM_WORKSPACE is the same override the mpm-pi installer honours, and the
# same variable mpm itself reads to locate the workspace. Previously this
# was hardcoded to $HOME/.mpm, so an operator running against a relocated
# workspace got a plugin[] entry pointing at a directory that was never
# populated — the registration looked correct and loaded nothing. The
# default is unchanged, so an unset MPM_WORKSPACE behaves exactly as before.
MPM_HOME_DIR="${MPM_WORKSPACE:-${HOME_DIR}/.mpm}"
PLUGIN_INSTALL_DIR="${MPM_HOME_DIR}/agent_installation"

# Canonical adapter install locations (new and legacy).
CANONICAL_ADAPTER_DIR="${PLUGIN_INSTALL_DIR}/mpm-opencode"
LEGACY_ADAPTER_DIR="${PLUGIN_INSTALL_DIR}/opencode-mpm"
# The registration is the plugin PACKAGE DIRECTORY. OpenCode 2 resolves a
# directory plugin by a root-level server/index module inside it (never
# package.json); a file registration is refused outright, and a directory
# without the root entry is dropped silently. See the header comments.
CANONICAL_PLUGIN_ENTRY="file://${CANONICAL_ADAPTER_DIR}"

INSTALL_LOG="/tmp/mpm-opencode-install.log"
: > "$INSTALL_LOG" 2>/dev/null || true

log()  { printf '[mpm-opencode install] %s\n' "$*" >&2; }
warn() { printf '[mpm-opencode install] WARN: %s\n' "$*" >&2; }
err()  { printf '[mpm-opencode install] ERROR: %s\n' "$*" >&2; }
die()  { err "$1"; exit "${2:-1}"; }

# --------------------------------------------------------------------------
# Argument parsing
# --------------------------------------------------------------------------

MODE="install"
for arg in "$@"; do
    case "$arg" in
        --uninstall) MODE="uninstall" ;;
        --verify)    MODE="verify" ;;
        --help|-h)
            sed -n '2,74p' "$0"
            exit 0
            ;;
        *)  err "unknown option: $arg"; exit 1 ;;
    esac
done

# --------------------------------------------------------------------------
# Step 1: compile dist/ if missing or stale
# --------------------------------------------------------------------------
#
# The local TypeScript compiler (./node_modules/.bin/tsc) is the
# canonical build. We rebuild only when the staged payload
# (dist/index.v2.js) is missing, some module under src/ is newer than
# it, or the payload was built from a DIFFERENT source revision — the
# incremental build keeps this fast on repeat invocations. See
# build_dist() for why we don't use `npx tsc` (the deprecated npm
# package lookup).

# The build-provenance sidecar. It records WHICH source revision
# produced the compiled payload, so "the deployed payload matches the
# deployed revision" is a fact that can be checked rather than assumed.
#
# It is deliberately NOT part of OPENCODE_RUNTIME_MANIFEST: the manifest
# is an explicit allowlist of files the runtime package needs to LOAD,
# and this file is evidence, not payload. It is published into the
# runtime package after staging (see publish_build_provenance) so the
# deployed copy — the one OpenCode actually loads — carries it.
PROVENANCE_FILE="dist/mpm-build-provenance.json"

# --------------------------------------------------------------------------
# build_provenance_commit
# --------------------------------------------------------------------------
#
# The revision the payload is built from, in priority order:
#
#   1. MPM_BUILD_COMMIT — exported by the root installer and by the
#      Makefile's refresh-installed target. This is the revision of the
#      release being installed.
#   2. git -C <adapter dir> rev-parse HEAD — correct when the adapter
#      lives inside an MPM checkout (the live ~/.mpm runtime root is
#      one).
#   3. "unknown" — a statement of absence, never a guess. The same
#      convention the Go binaries use for their own provenance.
build_provenance_commit() {
    if [ -n "${MPM_BUILD_COMMIT:-}" ] && [ "$MPM_BUILD_COMMIT" != "unknown" ]; then
        printf '%s' "$MPM_BUILD_COMMIT"
        return 0
    fi
    if command -v git >/dev/null 2>&1; then
        local commit
        commit="$(git -C "$SCRIPT_DIR" rev-parse HEAD 2>/dev/null || true)"
        if [ -n "$commit" ]; then
            printf '%s' "$commit"
            return 0
        fi
    fi
    printf 'unknown'
}

# provenance_recorded_commit — the revision the CURRENT payload was
# built from, or "" when there is no sidecar (or it cannot be read).
provenance_recorded_commit() {
    local file="$SCRIPT_DIR/$PROVENANCE_FILE"
    [ -f "$file" ] || { printf ''; return 0; }
    command -v python3 >/dev/null 2>&1 || { printf ''; return 0; }
    python3 - "$file" <<'PY' 2>/dev/null || printf ''
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        print(json.load(fh).get("source_commit", "") or "")
except Exception:
    print("")
PY
}

# payload_sha256 — the hash of the compiled payload, so the sidecar
# identifies the exact bytes that were deployed.
payload_sha256() {
    python3 - "$1" <<'PY'
import hashlib, sys
digest = hashlib.sha256()
with open(sys.argv[1], "rb") as fh:
    for chunk in iter(lambda: fh.read(65536), b""):
        digest.update(chunk)
print(digest.hexdigest())
PY
}

# write_build_provenance — record the revision, payload hash and build
# time of the payload just produced.
write_build_provenance() {
    local payload="$SCRIPT_DIR/dist/index.v2.js"
    [ -f "$payload" ] || return 0
    if ! command -v python3 >/dev/null 2>&1; then
        warn "python3 not found — the build provenance sidecar was not written"
        return 0
    fi
    local commit sha built_at
    commit="$(build_provenance_commit)"
    sha="$(payload_sha256 "$payload")"
    built_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if ! python3 - "$SCRIPT_DIR/$PROVENANCE_FILE" "$commit" "$sha" "$built_at" <<'PY'
import json, os, sys
path, commit, sha, built_at = sys.argv[1:5]
data = {
    "adapter": "mpm-opencode",
    "source_commit": commit,
    "payload_sha256": sha,
    "built_at": built_at,
}
tmp = path + ".tmp"
with open(tmp, "w", encoding="utf-8") as fh:
    json.dump(data, fh, indent=2, sort_keys=True)
    fh.write("\n")
os.replace(tmp, path)
PY
    then
        warn "could not write the build provenance sidecar"
        return 0
    fi
}

# publish_build_provenance — copy the sidecar into the PUBLISHED runtime
# package. The manifest does not carry it (it is evidence, not payload),
# so without this the deployed copy — the one OpenCode loads — could not
# be tied back to a revision.
publish_build_provenance() {
    local src="$SCRIPT_DIR/$PROVENANCE_FILE"
    local dst="$CANONICAL_ADAPTER_DIR/$PROVENANCE_FILE"
    [ -f "$src" ] || return 0
    mkdir -p "$(dirname "$dst")" 2>/dev/null || true
    cp -p "$src" "$dst" 2>/dev/null || true
}

# --------------------------------------------------------------------------
# npm_output_tail
# --------------------------------------------------------------------------
#
# Print the tail of npm's own output so the operator sees WHY the
# dependency install failed — with anything that looks like a credential
# redacted, because npm can echo a registry URL that carries a token and
# installer output reaches terminals and CI logs.
npm_output_tail() {
    local file="$1" lines="${2:-8}" text
    [ -f "$file" ] || return 0
    text="$(tail -n "$lines" "$file" 2>/dev/null || true)"
    [ -n "$text" ] || return 0
    printf '%s\n' "$text" \
        | sed -E \
            -e 's#(_authToken=)[^&[:space:]]+#\1REDACTED#g' \
            -e 's#((npm|NPM|auth|AUTH|token|TOKEN|password|PASSWORD)[[:space:]]*[:=][[:space:]]*)[^[:space:]]+#\1REDACTED#g' \
        >&2
}

# --------------------------------------------------------------------------
# ensure_toolchain
# --------------------------------------------------------------------------
#
# Obtain the adapter's build dependencies.
#
# The compiled payload is produced by the TypeScript compiler in
# node_modules/.bin/tsc. node_modules/ is gitignored, so it is never
# present in a checkout — the installer has to create it.
#
# Until this existed, the installer simply died with "TypeScript
# compiler not found ... run 'npm ci'" and stopped there. That left the
# operator with a manual step that NO installation path performed, so a
# fresh install reported success while the plugin loaded nothing.
#
# `npm ci` (not `npm install`) is the canonical bootstrap because the
# committed package-lock.json pins the dependency graph. It is NOT run
# with --offline: a fresh machine has no npm cache, and assuming one is
# exactly the assumption that made the manual workaround fail.
ensure_toolchain() {
    if ! command -v node >/dev/null 2>&1; then
        err "Node.js is required to build the mpm-opencode plugin but was not found on PATH."
        err "  Install Node.js (18 or newer) and re-run this installer."
        err "  MPM will not register a plugin it cannot build."
        exit 3
    fi
    if ! command -v npm >/dev/null 2>&1; then
        err "npm is required to install the mpm-opencode plugin's build dependencies"
        err "  but was not found on PATH (node was found at $(command -v node))."
        err "  Install npm and re-run this installer."
        exit 3
    fi

    log "installing build dependencies (npm ci) in $SCRIPT_DIR"
    local npm_out="$INSTALL_LOG.npm"
    local rc=0
    (cd "$SCRIPT_DIR" && npm ci --no-audit --no-fund) >>"$npm_out" 2>&1 || rc=$?
    if [ "$rc" -eq 0 ]; then
        log "build dependencies installed"
        return 0
    fi
    err "npm ci failed (exit $rc) in $SCRIPT_DIR — the plugin's build dependencies could not be installed."
    err "  Full output: $npm_out"
    npm_output_tail "$npm_out" 8
    err "  Check access to the npm registry, then re-run this installer."
    exit 3
}

build_dist() {
    # The staged payload is dist/index.v2.js (the OpenCode-2 plugin
    # implementation); dist/index.js remains the V1-era build and is not
    # part of the staged package. Rebuild when the payload is missing or
    # ANY source module under src/ is newer than it.
    local payload="$SCRIPT_DIR/dist/index.v2.js"
    local newer=""
    if [ ! -f "$payload" ]; then
        newer="missing"
    else
        local f
        for f in "$SCRIPT_DIR"/src/*.ts; do
            [ -e "$f" ] || continue
            if [ "$f" -nt "$payload" ]; then
                newer="$f"
                break
            fi
        done
        if [ -z "$newer" ]; then
            # mtime alone cannot see a revision change that leaves
            # src/*.ts byte-identical (a release that touches no
            # adapter file, or a checkout that restores the same
            # content). The recorded provenance is what makes the
            # payload's revision verifiable rather than assumed.
            local recorded current
            recorded="$(provenance_recorded_commit)"
            current="$(build_provenance_commit)"
            if [ -z "$recorded" ]; then
                newer="no recorded build provenance"
            elif [ "$recorded" != "$current" ]; then
                newer="revision $current (payload was built from $recorded)"
            fi
        fi
    fi
    if [ -n "$newer" ]; then
        log "rebuilding dist/ from source ($newer)..."
        # Use the local TypeScript binary from node_modules/.bin/. We
        # intentionally do NOT use `npx tsc` here: `npx tsc` resolves
        # the npm package name `tsc` (the deprecated 2.0.4 stub that
        # prints "This is not the tsc command you are looking for")
        # rather than the local TypeScript compiler. The
        # node_modules/.bin/tsc path is hermetic and only depends on
        # the adapter's devDependencies having been populated. The
        # committed package-lock.json (lockfileVersion 3) makes
        # `npm ci` the canonical bootstrap on a fresh checkout; plain
        # `npm install` still works but is NOT what gates like
        # `make test-agent-adapters` exercise in CI.
        local tsc_bin="$SCRIPT_DIR/node_modules/.bin/tsc"
        if [ ! -x "$tsc_bin" ]; then
            # The compiler is missing because the dependencies were
            # never installed. Install them rather than telling the
            # operator to do it by hand.
            ensure_toolchain
            tsc_bin="$SCRIPT_DIR/node_modules/.bin/tsc"
            if [ ! -x "$tsc_bin" ]; then
                die "the TypeScript compiler is still missing at $tsc_bin after installing the build dependencies" 3
            fi
        fi
        if ! (cd "$SCRIPT_DIR" && "$tsc_bin" 2>>"$INSTALL_LOG") >>"$INSTALL_LOG" 2>&1; then
            err "the TypeScript compilation failed (tsc exited non-zero)."
            err "  Full output: $INSTALL_LOG"
            tail -n 20 "$INSTALL_LOG" 2>/dev/null | sed -E \
                -e 's#(_authToken=)[^&[:space:]]+#\1REDACTED#g' >&2 || true
            exit 2
        fi
        write_build_provenance
        log "dist/ rebuilt"
    else
        log "dist/ is up to date"
    fi
}

# --------------------------------------------------------------------------
# Step 2: provision the runtime package
# --------------------------------------------------------------------------
#
# OpenCode loads the plugin at runtime from the file:// URL written below.
# That URL names a path inside the runtime root, which Git populates only
# when ~/.mpm *is* the source checkout — an invariant that no longer holds
# now that checkouts and runtime roots are separate trees.
#
# build_dist() compiles into $SCRIPT_DIR, and the registration names
# $CANONICAL_ADAPTER_DIR. When the installer runs from anywhere but the
# installed workspace those are different directories, so the config got an
# entry resolving to a path holding no dist/index.js. OpenCode reports no
# error for an unresolvable plugin URL; it just loads nothing.
#
# Provisioning is therefore explicit here, using the shared library rather
# than a copy loop: manifest allowlist, staged into a sibling and swapped
# into place atomically, validated before OpenCode is told about it,
# idempotent on re-run, and preserving any operator file already in the
# package.

STAGE_LIBRARY="${SCRIPT_DIR}/../scripts/stage_runtime_package.sh"

# Explicit allowlist. server.js is the root-level ENTRY the OpenCode
# loader resolves a directory plugin by (it never consults package.json).
# tsc emits one .js per .ts under src/ with rootDir=src and outDir=dist, so
# the dist/ entries are the complete compiled payload; index.v2.js is the
# implementation the loader actually runs. package.json travels with it
# because OpenCode resolves the plugin's module metadata through the
# package root. Development surface (install.sh, tests/, src/,
# reconcile.json, node_modules) is excluded.
#
# If tsc ever emits a module this list omits, the unresolved-import check in
# the validator fails the stage rather than publishing a package that would
# throw on load. The list cannot silently drift out of date.
#
# NOTE: dist/index.js is deliberately EXCLUDED.
#
# Background: dist/index.js is the legacy V1 emit — the plugin
# entry that the 2026-09-17 .. 2026-10-16 installer registered
# in opencode.jsonc and that OpenCode 2 silently dropped
# (the registration defect tracked by issue #35 and the
# runtime evidence recorded in VALIDATION-2026-08-19.md).
#
# The canonical payload today is dist/index.v2.js (the V2
# loader surface, loaded via the root-level server.js shim).
# dist/index.js is no longer the entry, has no caller, and
# staging it would re-couple the package to the V1 surface
# shape — exactly the regression class the staging validator
# is meant to prevent. tsc emits both .js files because the
# source tree still ships src/index.ts (V1, retained for
# history); this manifest is the boundary that drops the V1
# emit before promotion.
#
# If a future V3 emerges and the V1 emit stops being
# generated by tsc, this note can be deleted alongside the
# corresponding `src/index.ts` removal. Until then, omitting
# dist/index.js here is the load-bearing step.
OPENCODE_RUNTIME_MANIFEST=(
    server.js
    dist/index.v2.js
    dist/workspace.js
    dist/resolve-mpm-binary.js
    package.json
)

stage_opencode_runtime_package() {
    if [ ! -f "$STAGE_LIBRARY" ]; then
        err "runtime provisioning library not found at $STAGE_LIBRARY"
        err "  this installer cannot safely provision its own payload"
        exit 2
    fi
    # shellcheck source=../scripts/stage_runtime_package.sh
    . "$STAGE_LIBRARY"

    local staged
    if ! staged="$(MPM_RUNTIME_PACKAGE_VALIDATOR=mpm_validate_runtime_package_opencode \
                  stage_runtime_package "$SCRIPT_DIR" "$MPM_HOME_DIR" \
                  "mpm-opencode" "${OPENCODE_RUNTIME_MANIFEST[@]}")"; then
        err "runtime package provisioning failed for mpm-opencode; refusing to register an unvalidated payload"
        exit 3
    fi
    CANONICAL_ADAPTER_DIR="$(cd "$staged" && pwd -P)"
    CANONICAL_PLUGIN_ENTRY="file://${CANONICAL_ADAPTER_DIR}"
    log "runtime package staged and validated: $CANONICAL_ADAPTER_DIR"
}

# --------------------------------------------------------------------------
# Step 3: detect and migrate OpenCode plugin config
# --------------------------------------------------------------------------
#
# The namespace migration `git mv opencode-mpm → mpm-opencode` moved
# tracked source. It did NOT touch ~/.config/opencode/opencode.jsonc —
# that file is user-owned and the migration is silent. Real installs
# carry a stale `file:///.../opencode-mpm/dist/index.js` entry that
# breaks the plugin loader.
#
# This step is the repo-owned namespace-refresh: detect the stale path
# in opencode.jsonc, update it to the canonical path. We do NOT
# delete unrelated plugins; we do NOT touch other config sections.

migrate_opencode_config() {
    mkdir -p "$OPENCODE_PLUGIN_DIR" 2>/dev/null || true
    mkdir -p "$OPENCODE_CONFIG_DIR" 2>/dev/null || true

    if [ ! -f "$OPENCODE_CONFIG" ]; then
        # No config yet — create one with just the plugin entry. The "plugins"
        # key is the OpenCode 2 canonical name ("plugin" is the V1 key the
        # v2 loader still tolerates for compatibility).
        cat > "$OPENCODE_CONFIG" <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "plugins": [
    "${CANONICAL_PLUGIN_ENTRY}"
  ]
}
EOF
        log "created ${OPENCODE_CONFIG} with canonical plugin entry"
        return 0
    fi

    if ! command -v python3 >/dev/null 2>&1; then
        err "python3 is required to migrate ${OPENCODE_CONFIG} safely."
        err "  MPM will NOT edit the file by shell substitution — that corrupts"
        err "  other plugin entries and any URL inside a string value."
        err "  Install python3 and re-run, or replace 'opencode-mpm' with"
        err "  'mpm-opencode' in ${OPENCODE_CONFIG} by hand."
        return 1
    fi

    # opencode.jsonc is JSONC: users put // comments and trailing commas in
    # it. json.loads() rejects those, and the previous implementation
    # treated that rejection as "nothing to do" and exited 0 — so the shell
    # reported a successful migration while the stale opencode-mpm entry
    # survived. jsonc_config.py strips comments and trailing commas with a
    # string-aware scanner (a regex would eat the "https://" in a $schema
    # URL), splices only the plugin array so unrelated keys and the
    # operator's comments survive byte-for-byte, and exits non-zero when the
    # config cannot be parsed or walked safely.
    local migrator="${SCRIPT_DIR}/scripts/jsonc_config.py"
    if [ ! -f "$migrator" ]; then
        err "JSONC migration helper missing at ${migrator}"
        err "  ${OPENCODE_CONFIG} was NOT modified."
        return 1
    fi

    if ! python3 "$migrator" "$OPENCODE_CONFIG" "$CANONICAL_PLUGIN_ENTRY"; then
        err "opencode.jsonc migration failed — file left untouched."
        err "  MPM is NOT registered with OpenCode; its plugin will not load."
        return 1
    fi

    log "opencode.jsonc migrated to canonical plugin entry"
    return 0
}

# --------------------------------------------------------------------------
# Step 4: refresh the AGENTS.md behavioral managed block
# --------------------------------------------------------------------------
#
# Delegates to the existing Python installer. We pass the canonical
# target + snippet paths so the managed block always reflects current
# source. The Python installer is idempotent.

refresh_agents_md() {
    local installer="$SCRIPT_DIR/scripts/install_agents_instructions.py"
    local snippet="$SCRIPT_DIR/templates/AGENTS.md.snippet"
    local target="$OPENCODE_CONFIG_DIR/AGENTS.md"
    if [ ! -f "$installer" ]; then
        warn "AGENTS.md installer missing at $installer; skipping managed-block refresh"
        return 0
    fi
    if [ ! -f "$snippet" ]; then
        warn "AGENTS.md snippet missing at $snippet; skipping managed-block refresh"
        return 0
    fi
    if ! python3 "$installer" \
            --scope user \
            --target "$target" \
            --snippet "$snippet" \
            >>"$INSTALL_LOG" 2>&1; then
        warn "AGENTS.md installer failed (see $INSTALL_LOG); continuing"
    else
        log "AGENTS.md managed block refreshed at $target"
    fi
}

# --------------------------------------------------------------------------
# Step 5: clean up the legacy generated directory (when safe)
# --------------------------------------------------------------------------
#
# After the namespace migration, the legacy directory
# ~/.mpm/agent_installation/opencode-mpm/ may still exist with a
# generated dist/ subdirectory (which was .gitignored and never
# tracked). We remove ONLY the generated dist/, not user code.
#
# This step is best-effort. If anything goes wrong we warn and proceed.

cleanup_legacy_generated() {
    if [ ! -d "$LEGACY_ADAPTER_DIR" ]; then
        return 0
    fi
    # dist/ is the only generated subdirectory in the adapter layout.
    # node_modules/ may also be generated but is sometimes hand-installed
    # for development; leave it.
    if [ -d "$LEGACY_ADAPTER_DIR/dist" ]; then
        if rm -rf "$LEGACY_ADAPTER_DIR/dist" 2>/dev/null; then
            log "removed generated dist/ under $LEGACY_ADAPTER_DIR"
        else
            warn "could not remove $LEGACY_ADAPTER_DIR/dist (operator cleanup)"
        fi
    fi
}

# --------------------------------------------------------------------------
# Uninstall
# --------------------------------------------------------------------------

do_uninstall() {
    log "uninstalling mpm-opencode..."
    # Remove the AGENTS.md managed block.
    local installer="$SCRIPT_DIR/scripts/install_agents_instructions.py"
    if [ -f "$installer" ]; then
        local target="$OPENCODE_CONFIG_DIR/AGENTS.md"
        if python3 "$installer" --scope user --target "$target" \
            --uninstall >>"$INSTALL_LOG" 2>&1; then
            log "removed managed block from $target"
        else
            err "AGENTS.md uninstall failed (see $INSTALL_LOG); leaving managed block in place"
            exit 1
        fi
    fi
    # Remove the plugin entry from opencode.jsonc. Delegated to
    # jsonc_config.py in --remove-mpm mode: every MPM-owned registration
    # shape (the canonical package directory, the 2026-09-17..10-16
    # dist/index.js file registration, and every legacy opencode-mpm
    # variant) is removed from both the V2 `plugins` key and the legacy
    # `plugin` key, unrelated plugins and all comments survive, and a key
    # whose array becomes empty is removed entirely. The previous inline
    # implementation parsed with json.loads (which dies on commented
    # configs — uninstall silently did nothing) and rewrote the whole file
    # with json.dump, destroying every comment it succeeded on.
    if [ -f "$OPENCODE_CONFIG" ] && command -v python3 >/dev/null 2>&1; then
        if python3 "${SCRIPT_DIR}/scripts/jsonc_config.py" \
                --remove-mpm "$OPENCODE_CONFIG" "$CANONICAL_PLUGIN_ENTRY" >>"$INSTALL_LOG" 2>&1; then
            log "removed MPM registration entries from $OPENCODE_CONFIG"
        else
            err "opencode.jsonc uninstall failed — file left untouched (see $INSTALL_LOG)."
            err "  MPM registrations may remain in $OPENCODE_CONFIG."
            exit 1
        fi
    fi
}

# --------------------------------------------------------------------------
# Main dispatch
# --------------------------------------------------------------------------

case "$MODE" in
    install)
        log "installing mpm-opencode plugin (id=$PLUGIN_ID) from $SCRIPT_DIR"
        build_dist
        # Registration names a path. Provision and validate it before
        # writing anything, so an entry never exists without a payload.
        stage_opencode_runtime_package
        # The provenance sidecar is evidence, not payload: it is not in
        # the runtime manifest, so it is published into the staged
        # package explicitly. Without this the DEPLOYED payload could
        # not be tied back to a revision.
        publish_build_provenance
        # A failed migration means MPM is NOT registered with OpenCode.
        # Continuing would print "done" and a verification summary that
        # claims the plugin entry points at the canonical path — the exact
        # false-success this step previously produced on commented configs.
        if ! migrate_opencode_config; then
            err "install aborted: OpenCode plugin registration did not complete."
            exit 1
        fi
        refresh_agents_md
        cleanup_legacy_generated
        log "done"
        # State what was actually checked, and what was not. An install
        # log that lists only successes reads as "the plugin works", which
        # this installer cannot establish: OpenCode exposes no
        # non-interactive surface that reports which plugins it loaded
        # (every `opencode` subcommand requires a live authenticated
        # environment), and an unresolvable plugin URL is silent.
        log "verification (checked here):"
        if [ -f "$CANONICAL_ADAPTER_DIR/server.js" ] && [ -s "$CANONICAL_ADAPTER_DIR/server.js" ]; then
            log "  [ok]   root entry server.js exists at the registered package"
        else
            log "  [FAIL] root entry server.js missing — OpenCode drops the registration silently"
        fi
        if [ -f "$CANONICAL_ADAPTER_DIR/dist/index.v2.js" ] && [ -s "$CANONICAL_ADAPTER_DIR/dist/index.v2.js" ]; then
            log "  [ok]   compiled payload exists behind the registered path"
        else
            log "  [FAIL] compiled payload dist/index.v2.js missing at the registered path"
        fi
        if python3 "${SCRIPT_DIR}/scripts/jsonc_config.py" \
                --check-entry "$OPENCODE_CONFIG" "$CANONICAL_PLUGIN_ENTRY" >/dev/null 2>&1; then
            log "  [ok]   plugins[] in $OPENCODE_CONFIG registers the package directory"
        else
            log "  [FAIL] $OPENCODE_CONFIG does not register the package directory"
            log "         (a historical file-path registration also fails this check)"
        fi
        if [ -f "$CANONICAL_ADAPTER_DIR/$PROVENANCE_FILE" ]; then
            log "  [ok]   build provenance recorded for the deployed payload"
            log "         $(cat "$CANONICAL_ADAPTER_DIR/$PROVENANCE_FILE" | tr -d '\n' | sed 's/  */ /g')"
        else
            log "  [FAIL] build provenance missing at $CANONICAL_ADAPTER_DIR/$PROVENANCE_FILE"
        fi
        log "  [ok]   AGENTS.md managed block refreshed"
        log "NOT verified:"
        log "  whether OpenCode actually loads the plugin. Installer checks cannot"
        log "  prove runtime loading. With a running OpenCode service, ask the"
        log "  server itself and require a NON-builtin plugin entry:"
        log "    opencode api GET /api/plugin"
        log "  (an all-builtin result means the plugin did NOT load); or invoke an"
        log "  mpm_* tool from an OpenCode session."
        if [ -d "$LEGACY_ADAPTER_DIR" ]; then
            log "  legacy directory remains at $LEGACY_ADAPTER_DIR"
            log "  operator can remove manually: rm -rf '$LEGACY_ADAPTER_DIR'"
        fi
        ;;
    uninstall)
        do_uninstall
        log "done"
        ;;
    verify)
        # Verify the canonical install state.
        ok=1
        # Check the payload the *registration* names, not the one this
        # script happens to be sitting next to. Verifying SCRIPT_DIR is
        # what let a checkout pass --verify while the registered
        # file:// URL pointed at nothing.
        #
        # The registration is the PACKAGE DIRECTORY; the loader-resolvable
        # root entry plus the compiled payload are both required — a
        # directory without server.js "verifies" as an existing directory
        # while the loader drops it silently.
        if [ ! -f "${CANONICAL_ADAPTER_DIR}/server.js" ] || [ ! -s "${CANONICAL_ADAPTER_DIR}/server.js" ]; then
            err "verify: registered package has no root entry server.js"
            err "  expected: ${CANONICAL_ADAPTER_DIR}/server.js (re-run install)"
            err "  a directory plugin without that root-level module is dropped"
            err "  silently by the OpenCode loader (no warning is printed)"
            ok=0
        fi
        if [ ! -f "${CANONICAL_ADAPTER_DIR}/dist/index.v2.js" ] || [ ! -s "${CANONICAL_ADAPTER_DIR}/dist/index.v2.js" ]; then
            err "verify: compiled payload missing at the registered path"
            err "  expected: ${CANONICAL_ADAPTER_DIR}/dist/index.v2.js"
            ok=0
        fi
        if [ ! -x "$SCRIPT_DIR/node_modules/.bin/tsc" ]; then
            warn "verify: local TypeScript compiler missing at $SCRIPT_DIR/node_modules/.bin/tsc"
            warn "  (the installer installs it automatically — re-run this installer)"
        fi
        # The deployed payload's provenance. Reported, not asserted: the
        # recorded revision is the one the payload was BUILT from, which
        # is the fact an operator needs when the installed tree and the
        # checkout disagree.
        if [ -f "$CANONICAL_ADAPTER_DIR/$PROVENANCE_FILE" ]; then
            recorded="$(python3 - "$CANONICAL_ADAPTER_DIR/$PROVENANCE_FILE" <<'PY' 2>/dev/null || printf 'unknown'
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        print(json.load(fh).get("source_commit", "unknown"))
except Exception:
    print("unknown")
PY
)"
            current="$(build_provenance_commit)"
            log "verify: deployed payload was built from revision ${recorded}"
            if [ "$recorded" != "$current" ]; then
                warn "verify: the installed source revision is ${current},"
                warn "  but the deployed payload was built from ${recorded}"
                warn "  (re-run this installer to rebuild the payload)"
            fi
        else
            warn "verify: no build provenance at $CANONICAL_ADAPTER_DIR/$PROVENANCE_FILE"
            warn "  (the payload predates provenance recording; re-run this installer)"
        fi
        if [ ! -f "$OPENCODE_CONFIG" ]; then
            err "verify: $OPENCODE_CONFIG missing"
            ok=0
        fi
        if [ -f "$OPENCODE_CONFIG" ] && grep -q 'opencode-mpm' "$OPENCODE_CONFIG"; then
            err "verify: $OPENCODE_CONFIG still references legacy 'opencode-mpm'"
            ok=0
        fi
        # Presence of the canonical entry, not merely absence of the legacy
        # one. A config holding only unrelated plugins passed the checks
        # above with MPM entirely unregistered — the registration check has
        # to be for the thing we claim to have installed.
        #
        # Parsed with the JSONC-aware reader, not grep, so a commented or
        # comma-trailing config is understood and a URL in another field
        # cannot masquerade as a match.
        if [ -f "$OPENCODE_CONFIG" ]; then
            if ! python3 "${SCRIPT_DIR}/scripts/jsonc_config.py" \
                    --check-entry "$OPENCODE_CONFIG" "$CANONICAL_PLUGIN_ENTRY" 2>>"$INSTALL_LOG"; then
                err "verify: $OPENCODE_CONFIG does not register the canonical package directory"
                err "  expected: $CANONICAL_PLUGIN_ENTRY"
                err "  a surviving historical file-path registration (…/dist/index.js)"
                err "  is the load-nothing defect this check exists to catch; re-run install"
                err "  or hand-edit the entry to the package directory."
                ok=0
            fi
        fi
        [ "$ok" = "1" ] || exit 1
        log "verify: ok"
        ;;
    *)
        err "unknown mode: $MODE"
        exit 1
        ;;
esac
