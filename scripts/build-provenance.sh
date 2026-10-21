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
# WHEN THERE IS NO GIT CHECKOUT (an exported tarball, a vendored copy):
# commit/short-commit report "unknown" and dirty reports "unknown". The
# build still succeeds and still stamps the product version.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

# The Go package that carries the provenance. Every MPM binary links it,
# so `-X` against this path is what actually reaches the binary.
BUILDINFO_PKG="github.com/flowbyte-com/mpm/internal/buildinfo"

# in_git_repo reports whether this checkout has usable git metadata.
in_git_repo() {
    git rev-parse --git-dir >/dev/null 2>&1
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
    if in_git_repo; then
        git rev-parse HEAD 2>/dev/null || echo "unknown"
    else
        echo "unknown"
    fi
}

# short_commit reports the abbreviated source commit SHA (8 characters,
# matching `git rev-parse --short=8`), or "unknown".
short_commit() {
    if in_git_repo; then
        git rev-parse --short=8 HEAD 2>/dev/null || echo "unknown"
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
dirty() {
    if ! in_git_repo; then
        echo "unknown"
        return
    fi
    if ! git diff --quiet HEAD -- 2>/dev/null; then
        echo "true"
        return
    fi
    local untracked_go
    untracked_go="$(git ls-files --others --exclude-standard 2>/dev/null \
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
# A build is a release only when HEAD sits EXACTLY on a tag whose name is
# the VERSION file's content (with or without a leading "v") and that
# version is a stable release rather than a pre-release. A pre-release
# such as 0.1.0-dev is a development version, and the repository's
# archive/* preservation tags are never release tags.
build_type() {
    local version
    version="$(version)"
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
        echo "release"
        return
    done < <(git tag --points-at HEAD 2>/dev/null || true)
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
