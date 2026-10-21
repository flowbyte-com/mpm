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

# version reads the authoritative product version from the VERSION file.
version() {
    if [ -f VERSION ]; then
        tr -d ' \t\n\r' < VERSION
    else
        echo "dev"
    fi
}

# commit reports the full source commit SHA, or "unknown".
commit() {
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
    local root
    root="$(git_root)"
    if [ -n "$root" ]; then
        git -C "$root" rev-parse --short=8 HEAD 2>/dev/null || echo "unknown"
    else
        echo "unknown"
    fi
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
dirty() {
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

# ldflags emits the linker payload that stamps the provenance.
ldflags() {
    printf -- '-X %s.Version=%s -X %s.Commit=%s -X %s.ShortCommit=%s -X %s.Dirty=%s -X %s.BuildType=%s' \
        "$BUILDINFO_PKG" "$(version)" \
        "$BUILDINFO_PKG" "$(commit)" \
        "$BUILDINFO_PKG" "$(short_commit)" \
        "$BUILDINFO_PKG" "$(dirty)" \
        "$BUILDINFO_PKG" "$(build_type)"
}

# all_fields emits every field as "key=value" lines.
all_fields() {
    printf 'version=%s\n' "$(version)"
    printf 'commit=%s\n' "$(commit)"
    printf 'short_commit=%s\n' "$(short_commit)"
    printf 'dirty=%s\n' "$(dirty)"
    printf 'build_type=%s\n' "$(build_type)"
    printf 'ldflags=%s\n' "$(ldflags)"
}

case "${1:-all}" in
    version) version ;;
    commit) commit ;;
    short-commit) short_commit ;;
    dirty) dirty ;;
    build-type) build_type ;;
    ldflags) ldflags ;;
    all) all_fields ;;
    *)
        echo "usage: $(basename "$0") {version|commit|short-commit|dirty|build-type|ldflags|all}" >&2
        exit 2
        ;;
esac
