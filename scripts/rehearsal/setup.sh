#!/usr/bin/env bash
# scripts/rehearsal/setup.sh — build the rehearsal's source tree and the
# runtime bundle it rehearses against.
#
# This is the artefact that proves two things at once:
#
#   1. "install from a release archive without Git" — the source tree
#      here has no .git, no history, and nothing borrowed from an
#      enclosing repository.
#   2. "a Git-free tree can still carry a trustworthy identity" — the
#      producer states the revision it exported
#      (MPM_BUILD_COMMIT / MPM_BUILD_DIRTY), and the build stamps it.
#
# The source export comes from `git archive <sha>`, which is a genuine
# release archive: no .git, no history, no uncommitted changes. When no
# revision is given, the WORKING TREE is copied instead — that is what
# rehearses a change that has not been committed yet.
#
# Usage:
#   setup.sh <revision> <full-commit-sha>   # a release archive
#   setup.sh                                # the working tree
#
# Environment:
#   MPM_SOURCE_REPO   the checkout to export from (default: this repo)
#   REH_WORK          where the work products go (default: ./.work)

set -euo pipefail

REH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
. "$REH_DIR/lib.sh"

# A stale record from a previous run must not be read by this one.
rm -f "$REH_WORK/rehearsal-revision.env"

REPO="${MPM_SOURCE_REPO:-$(cd "$REH_DIR/../.." && pwd -P)}"
COMMIT="${2:-}"
REV_REQUESTED="${1:-${REH_REV}}"

# The revision is the ACTUAL short commit SHA of the tree under test.
#
# The operator may ask for "HEAD" or a tag; the assertions compare
# against what the binary reports, and what the binary reports is the
# short SHA. Resolving it here means the suites never have to guess, and
# the source tree and the bundle are named consistently.
if [ -n "$COMMIT" ]; then
  COMMIT="$(git -C "$REPO" rev-parse "$COMMIT^{commit}")"
else
  COMMIT="$(git -C "$REPO" rev-parse HEAD)"
fi
REV="$(printf '%s' "$COMMIT" | cut -c1-8)"

# The source state of the tree under test.
#
# It is COMPUTED from the source checkout, never asserted as "false". A
# rehearsal of the working tree carries uncommitted changes, and a
# binary that claims to be clean when it is not is exactly the
# dishonesty the provenance contract exists to prevent.
BUILD_DIRTY="$(cd "$REPO" && bash scripts/build-provenance.sh dirty)"

printf 'REH_REV=%s\nREH_COMMIT=%s\nEXPECT_DIRTY=%s\n' \
  "$REV" "$COMMIT" "$BUILD_DIRTY" \
  > "$REH_WORK/rehearsal-revision.env"
SRC="$REH_WORK/src-$REV"

if [ -n "${2:-}" ]; then
  echo "── 1. the release archive ──────────────────────────────────────────"
  safe_rm "$SRC"
  mkdir -p "$SRC"
  git -C "$REPO" archive "$COMMIT" | tar -x -C "$SRC"
  [ -e "$SRC/.git" ] && { echo "the archive carries .git — that is wrong" >&2; exit 1; }
  echo "   exported $COMMIT with no .git"
else
  echo "── 1. the working tree, copied without Git metadata ───────────────"
  safe_rm "$SRC"
  mkdir -p "$SRC"
  # A copy rather than an archive: the point is to rehearse the code as
  # it stands, including the changes that are not committed yet.
  #
  # The work directory is EXCLUDED: it is inside the tree being copied,
  # and tar would otherwise read the very files it is writing.
  workrel="./${REH_WORK#"$REPO"/}"
  tar --exclude=.git --exclude=.build --exclude=node_modules \
      --exclude="$workrel" \
      -C "$REPO" -cf - . | tar -C "$SRC" -xf -
  [ -e "$SRC/.git" ] && { echo "the copy carries .git — that is wrong" >&2; exit 1; }
  echo "   copied the working tree with no .git (HEAD=$COMMIT)"
fi

echo "── 2. the build ────────────────────────────────────────────────────"
# The producer states the revision. The build script VALIDATES the
# statement — a malformed SHA, or one that contradicts the checkout, is
# refused — so the identity is asserted, never guessed.
export MPM_BUILD_COMMIT="$COMMIT"
export MPM_BUILD_DIRTY="$BUILD_DIRTY"
echo "   producer assertion: commit=$COMMIT dirty=$MPM_BUILD_DIRTY"
( cd "$SRC" && make build 2>&1 | tail -3 )
"$SRC/.build/bin/mpm" --version | head -1

echo "── 3. the plugin payload ──────────────────────────────────────────"
# The OpenCode payload's compiled artefacts are not in the archive (they
# are build output). They are copied in and compiled with the real
# toolchain so the bundle carries a payload that actually loads.
if [ -d "$REPO/agent_installation/mpm-opencode/node_modules" ]; then
  mkdir -p "$SRC/agent_installation/mpm-opencode"
  cp -a "$REPO/agent_installation/mpm-opencode/node_modules" \
        "$SRC/agent_installation/mpm-opencode/node_modules" 2>/dev/null || true
  ( cd "$SRC/agent_installation/mpm-opencode" && ./node_modules/.bin/tsc ) || {
    echo "setup.sh: the TypeScript payload did not compile — the bundle would" >&2
    echo "          carry an unloadable adapter. Install the plugin's" >&2
    echo "          dependencies first." >&2
    exit 1
  }
fi

echo "── 4. the bundle ──────────────────────────────────────────────────"
bash "$REH_DIR/build-bundle.sh" "$SRC" "$REH_WORK/bundle-$REV" "$REV" "$COMMIT" \
     "$(cd "$SRC" && MPM_BUILD_COMMIT="$COMMIT" MPM_BUILD_DIRTY="$MPM_BUILD_DIRTY" bash scripts/build-provenance.sh build-epoch)"

echo
echo "The rehearsal is staged at $REH_WORK"
echo "  source tree : $SRC"
echo "  bundle      : $REH_WORK/bundle-$REV"
echo "  revision    : $REV ($COMMIT)"
echo "  requested   : $REV_REQUESTED"
