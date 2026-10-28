#!/usr/bin/env bash
# build-provenance.sh — the ONE convention that stamps build provenance
# into every MPM binary.
#
# Usage:
#   scripts/build-provenance.sh version       # the product version
#   scripts/build-provenance.sh commit        # the full source commit SHA
#   scripts/build-provenance.sh short-commit  # the abbreviated source commit SHA
#   scripts/build-provenance.sh dirty         # "true" | "false" | "unknown"
#   scripts/build-provenance.sh build-type    # "development" | "release"
#   scripts/build-provenance.sh build-epoch   # the UTC Unix epoch second of the build
#   scripts/build-provenance.sh ldflags       # the -ldflags payload
#   scripts/build-provenance.sh               # every field, "key=value" lines
#
# WHY A SCRIPT AND NOT MAKE INLINE
#
# The Makefile, CI and install.sh must all produce byte-identical
# provenance, and install.sh reaches its binaries through `make build`.
# A script is the only place all three can share without copying the
# rules. The script resolves the repository root from its OWN location,
# so it works from any working directory and from a copy of itself
# placed in another repository.
#
# WHERE THE VALUES COME FROM
#
#   version  the VERSION file at the repository root — the single
#            authoritative product version. Nothing else may define it.
#   commit   `git rev-parse HEAD`, resolved at BUILD time and baked into
#            the binary. A binary must be able to report its provenance
#            with no git checkout present, so nothing is resolved at run
#            time.
#   dirty    whether the source tree differed from that commit. See
#            dirty() for exactly what counts.
#   build    "release" only for a formally tagged release; "development"
#            for everything else. The repository's archive/* preservation
#            tags are NOT release tags.
#   epoch    the UTC Unix epoch second the build ran at. SOURCE_DATE_EPOCH
#            wins when it holds a non-negative integer — the
#            reproducible-builds convention — so two builds of the same
#            source produce the same identity.
#
# THE OVERRIDES: MPM_BUILD_COMMIT AND MPM_BUILD_DIRTY
#
# A Git-free source tree cannot report a commit, because it has no
# history to report — so an exported tree's binaries say "unknown",
# which is honest but not useful. A release archive is not an unknown
# commit: the builder knows exactly which revision it exported, and
# that fact has to survive into the shipped binary.
#
# So the builder states it:
#
#   MPM_BUILD_COMMIT=<full sha>   the revision this source was exported
#                                  from. Stamped verbatim.
#   MPM_BUILD_DIRTY=true|false     the source state of that export.
#                                  "unknown" is also accepted.
#
# The overrides are ASSERTIONS by the producer, never guesses. They are
# validated before anything is stamped:
#
#   - MPM_BUILD_COMMIT must be a full lowercase hex SHA (40 characters,
#     or 64 for a SHA-256 repository). Anything else is refused, because
#     a typo must not become a commit that no operator can verify.
#   - MPM_BUILD_DIRTY must be exactly "true", "false" or "unknown".
#   - When the tree IS an MPM Git checkout, the override must AGREE
#     with the checkout. An override that contradicts the tree is a
#     builder mistake, and it is refused rather than believed.
#
# With neither variable set, the behaviour is exactly what it always
# was.
#
# WHEN THE VALUES ARE NOT ESTABLISHED, THEY ARE "unknown"
#
# An exported tarball, a vendored copy, a source tree nested inside an
# unrelated repository — none of them has MPM's Git history. The commit
# and the dirty state are then reported as "unknown", never guessed and
# never borrowed from whatever repository happens to enclose the tree.
# The build still succeeds and still stamps the product version.
#
# A build is a "release" only when the tree is CLEAN as well as tagged.
# A locally modified tree on a release tag is not the released artifact.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd -P)"
cd "$REPO_ROOT"

# The Go package that carries the provenance. Every MPM binary links it,
# so `-X` against this path is what actually reaches the binary.
BUILDINFO_PKG="github.com/flowbyte-com/mpm/internal/buildinfo"

# The Go module path that identifies an MPM source tree. Every MPM
# binary is built from this module; a tree that is not this module has
# no MPM provenance to report, whatever repository encloses it.
MPM_MODULE="github.com/flowbyte-com/mpm"

# is_mpm_source_tree reports whether this directory is an MPM source
# tree.
#
# The go.mod module directive is the identity. The pattern below is
# $MPM_MODULE with its dots escaped for the regex.
is_mpm_source_tree() {
    if [ ! -f "$REPO_ROOT/go.mod" ]; then
        return 1
    fi
    local escaped="${MPM_MODULE//./\\.}"
    grep -Eq "^module[[:space:]]+${escaped}([[:space:]]|\$)" "$REPO_ROOT/go.mod"
}

# git_root prints the Git working tree that owns THIS source tree, or
# nothing at all when there is none.
#
# Git resolves a repository by searching PARENT directories, so a source
# tree with no Git metadata of its own would otherwise inherit the
# history of whatever repository encloses it. That commit says nothing
# about MPM, and stamping it into an MPM binary would be a lie an
# operator could not detect. The working tree must therefore BE this
# tree, and this tree must be an MPM source tree in the first place.
git_root() {
    if ! is_mpm_source_tree; then
        return 0
    fi
    local toplevel
    toplevel="$(git -C "$REPO_ROOT" rev-parse --show-toplevel 2>/dev/null || true)"
    if [ -z "$toplevel" ]; then
        return 0
    fi
    toplevel="$(cd "$toplevel" && pwd -P)"
    if [ "$toplevel" != "$REPO_ROOT" ]; then
        return 0
    fi
    printf '%s\n' "$REPO_ROOT"
}

# validate_overrides checks the producer's provenance assertions BEFORE
# anything is stamped, and refuses the build when they are wrong.
#
# It runs once, at dispatch, so a failure aborts the script with a
# non-zero status instead of producing a binary that claims an identity
# it does not have.
validate_overrides() {
    local c="${MPM_BUILD_COMMIT:-}"
    if [ -n "$c" ]; then
        if ! [[ "$c" =~ ^[0-9a-f]{40}$ ]] && ! [[ "$c" =~ ^[0-9a-f]{64}$ ]]; then
            echo "build-provenance.sh: MPM_BUILD_COMMIT must be a full lowercase hex commit SHA (40 characters, or 64 for a SHA-256 repository); got '$c'" >&2
            exit 2
        fi
        # When the tree IS an MPM checkout, the assertion must agree
        # with the checkout. A contradiction means the builder
        # exported one revision and stamped another.
        local root head
        root="$(git_root)"
        if [ -n "$root" ]; then
            head="$(git -C "$root" rev-parse HEAD 2>/dev/null || true)"
            if [ -n "$head" ] && [ "$head" != "$c" ]; then
                echo "build-provenance.sh: MPM_BUILD_COMMIT ('$c') contradicts HEAD ('$head') in $root; refusing to stamp a revision the tree does not have" >&2
                exit 2
            fi
        fi
    fi

    local d="${MPM_BUILD_DIRTY:-}"
    if [ -n "$d" ]; then
        case "$d" in
            true|false|unknown) : ;;
            *)
                echo "build-provenance.sh: MPM_BUILD_DIRTY must be exactly 'true', 'false' or 'unknown'; got '$d'" >&2
                exit 2
                ;;
        esac
        # Same rule for the source state: an override that contradicts
        # the tree is refused, not believed.
        if [ "$d" != "unknown" ]; then
            local root actual
            root="$(git_root)"
            if [ -n "$root" ]; then
                actual="$(dirty_from_checkout)"
                if [ "$actual" != "$d" ]; then
                    echo "build-provenance.sh: MPM_BUILD_DIRTY ('$d') contradicts the checkout's actual state ('$actual') in $root; refusing to stamp a source state the tree does not have" >&2
                    exit 2
                fi
            fi
        fi
    fi
}

# version reads the authoritative product version from the VERSION file.
version() {
    if [ -f VERSION ]; then
        tr -d ' \t\n\r' < VERSION
    else
        echo "dev"
    fi
}

# commit reports the full source commit SHA, or "unknown".
#
# The producer's MPM_BUILD_COMMIT assertion wins over the checkout: it
# exists precisely for the case where there is no checkout to ask.
commit() {
    if [ -n "${MPM_BUILD_COMMIT:-}" ]; then
        printf '%s\n' "$MPM_BUILD_COMMIT"
        return
    fi
    local root
    root="$(git_root)"
    if [ -n "$root" ]; then
        git -C "$root" rev-parse HEAD 2>/dev/null || echo "unknown"
    else
        echo "unknown"
    fi
}

# short_commit reports the abbreviated source commit SHA (8 characters,
# matching `git rev-parse --short=8`), or "unknown".
short_commit() {
    if [ -n "${MPM_BUILD_COMMIT:-}" ]; then
        printf '%s\n' "$(shorten_commit "$MPM_BUILD_COMMIT")"
        return
    fi
    local root
    root="$(git_root)"
    if [ -n "$root" ]; then
        git -C "$root" rev-parse --short=8 HEAD 2>/dev/null || echo "unknown"
    else
        echo "unknown"
    fi
}

# shorten_commit abbreviates a full commit SHA to the canonical eight
# characters.
shorten_commit() {
    printf '%s' "${1:0:8}"
}

# dirty reports whether the built source differed from the commit.
#
# Two things make a build dirty:
#
#   1. a tracked file differs from HEAD (staged or unstaged);
#   2. an untracked file would be COMPILED into the binary.
#
# Only Go sources count for (2). A forensic artefact, a scratch file or a
# test file is not linked into the shipped binary, so it must not make
# every build look dirty — otherwise the indicator stops carrying
# information. Files matched by .gitignore are excluded by git itself.
#
# The answer is "unknown" when there is no MPM checkout to compare
# against: an exported source tree has no baseline, and reporting it as
# clean would claim a certainty nobody has.
#
# The producer's MPM_BUILD_DIRTY assertion wins over the checkout, for
# the same reason MPM_BUILD_COMMIT does: an exported tree has no
# baseline to compare against, and the builder knows what it exported.
dirty() {
    if [ -n "${MPM_BUILD_DIRTY:-}" ]; then
        printf '%s\n' "$MPM_BUILD_DIRTY"
        return
    fi
    dirty_from_checkout
}

# dirty_from_checkout is dirty() WITHOUT the producer's assertion: the
# state of the checkout itself, or "unknown" when there is no checkout.
#
# It is separate because validate_overrides() needs the checkout's own
# answer in order to CHECK the assertion against it — asking the
# assertion about itself would be circular.
dirty_from_checkout() {
    local root
    root="$(git_root)"
    if [ -z "$root" ]; then
        echo "unknown"
        return
    fi
    if ! git -C "$root" rev-parse --verify --quiet HEAD >/dev/null 2>&1; then
        # A repository with no commits has no baseline to differ from.
        echo "unknown"
        return
    fi
    if ! git -C "$root" diff --quiet HEAD -- 2>/dev/null; then
        echo "true"
        return
    fi
    local untracked_go
    untracked_go="$(git -C "$root" ls-files --others --exclude-standard 2>/dev/null \
        | grep -E '(^|/)[^/]+\.go$' \
        | grep -vE '(^|/)[^/]*_test\.go$' || true)"
    if [ -n "$untracked_go" ]; then
        echo "true"
        return
    fi
    echo "false"
}

# build_type distinguishes a formally tagged release from an ordinary
# development build.
#
# A build is a release only when ALL of the following hold:
#
#   1. HEAD sits EXACTLY on a tag whose name is the VERSION file's
#      content (with or without a leading "v");
#   2. that version is a stable release rather than a pre-release — a
#      pre-release such as 0.1.0-dev is a development version;
#   3. the source tree is CLEAN.
#
# The repository's archive/* preservation tags are never release tags.
#
# Requirement (3) is what keeps a locally modified build from claiming
# official release status. The tag says which revision the tree is
# based on; the working tree says whether that revision is what was
# actually compiled. A build that changed the source after the tag was
# written is not the released artifact, and reporting it as one would
# tell an operator that two different things are the same release.
build_type() {
    local version
    version="$(version)"
    local root
    root="$(git_root)"
    if [ -z "$root" ]; then
        echo "development"
        return
    fi
    local tag
    while IFS= read -r tag; do
        [ -n "$tag" ] || continue
        if [ "$tag" != "$version" ] && [ "$tag" != "v$version" ]; then
            continue
        fi
        # A pre-release version (-dev, -rc1, ...) is not a formal release.
        case "$version" in
            *-*) echo "development"; return ;;
        esac
        # A release build must come from a CLEAN tree.
        if [ "$(dirty)" != "false" ]; then
            echo "development"
            return
        fi
        echo "release"
        return
    done < <(git -C "$root" tag --points-at HEAD 2>/dev/null || true)
    echo "development"
}

# build_epoch reports the UTC Unix epoch second at which the build ran.
#
# The value is captured HERE, at build time, and stamped into the binary.
# It is never re-derived when the version command runs: a binary that
# reported its build time from the clock at the moment it was asked would
# claim a new build time on every invocation.
#
# SOURCE_DATE_EPOCH wins when it holds a non-negative integer. That is the
# reproducible-builds convention, and honouring it means two builds of the
# same source produce the same identity — which is what makes a build
# verifiable by anyone who rebuilds it.
#
# A SOURCE_DATE_EPOCH that is not a non-negative integer is not silently
# ignored: the build time is reported as unknown, with a warning on
# stderr. Ignoring it would stamp the current clock into a binary whose
# builder asked for a reproducible one.
build_epoch() {
    if [ -n "${SOURCE_DATE_EPOCH:-}" ]; then
        if [[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]]; then
            printf '%s\n' "$SOURCE_DATE_EPOCH"
            return
        fi
        echo "build-provenance.sh: SOURCE_DATE_EPOCH is not a non-negative integer; the build time is unknown" >&2
        echo "unknown"
        return
    fi
    date -u +%s
}

# ldflags emits the linker payload that stamps the provenance.
ldflags() {
    printf -- '-X %s.Version=%s -X %s.Commit=%s -X %s.ShortCommit=%s -X %s.Dirty=%s -X %s.BuildType=%s -X %s.BuildEpoch=%s' \
        "$BUILDINFO_PKG" "$(version)" \
        "$BUILDINFO_PKG" "$(commit)" \
        "$BUILDINFO_PKG" "$(short_commit)" \
        "$BUILDINFO_PKG" "$(dirty)" \
        "$BUILDINFO_PKG" "$(build_type)" \
        "$BUILDINFO_PKG" "$(build_epoch)"
}

# all_fields emits every field as "key=value" lines.
all_fields() {
    printf 'version=%s\n' "$(version)"
    printf 'commit=%s\n' "$(commit)"
    printf 'short_commit=%s\n' "$(short_commit)"
    printf 'dirty=%s\n' "$(dirty)"
    printf 'build_type=%s\n' "$(build_type)"
    printf 'build_epoch=%s\n' "$(build_epoch)"
    printf 'ldflags=%s\n' "$(ldflags)"
}

# The producer's assertions are validated BEFORE anything is stamped.
# A malformed or contradictory assertion aborts the script here, so no
# binary is ever produced that claims an identity it does not have.
validate_overrides

case "${1:-all}" in
    version) version ;;
    commit) commit ;;
    short-commit) short_commit ;;
    dirty) dirty ;;
    build-type) build_type ;;
    build-epoch) build_epoch ;;
    ldflags) ldflags ;;
    all) all_fields ;;
    *)
        echo "usage: $(basename "$0") {version|commit|short-commit|dirty|build-type|build-epoch|ldflags|all}" >&2
        exit 2
        ;;
esac