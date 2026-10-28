#!/usr/bin/env bash
# rehearsal/build-bundle.sh — assemble a VERSIONED RUNTIME BUNDLE.
#
# A bundle is the replaceable half of an MPM installation:
#
#   <bundle>/
#     bin/                     the five binaries
#     agent_installation/      the release-scoped host assets (per-adapter
#                              reconcilers, templates, the behavioural
#                              protocol) AND the staged OpenCode payload
#     config/runtime-assets.json   the manifest of what this bundle owns
#     RELEASE.json             the build identity — the revision, readable
#                              without executing Git
#
# It contains NO persistent state, NO operator configuration and NO Git
# metadata. Everything in it is replaced on every upgrade.
#
# The bundle is assembled with the EXISTING installer machinery, not with
# new machinery:
#
#   * `install_runtime_assets.py --groups host-assets` provisions the
#     release-scoped assets (the proposed split; before the split this
#     script provisioned everything into one root).
#   * `stage_runtime_package.sh` stages the OpenCode payload through its
#     explicit manifest allowlist and validates it before it is exposed.
#   * `scripts/build-provenance.sh` supplies the build identity.
#
# Usage: build-bundle.sh <source-tree> <bundle-dest> <revision> <commit> <build-epoch>

set -euo pipefail

SRC="${1:?source tree}"
DEST="${2:?bundle destination}"
REV="${3:?revision}"
COMMIT="${4:?commit}"
EPOCH="${5:?build epoch}"

MANIFEST=(
    server.js
    dist/index.v2.js
    dist/wake-context.js
    dist/event-envelope.js
    dist/event-mapper.js
    dist/event-subscriber.js
    dist/workspace.js
    dist/resolve-mpm-binary.js
    package.json
)

rm -rf "$DEST"
mkdir -p "$DEST/bin"

# ── the binaries ──────────────────────────────────────────────────────────
for b in mpm mpm-mcp mpm-critic mpm-scheduler mpm-telemetry; do
  if [ ! -x "$SRC/.build/bin/$b" ]; then
    echo "build-bundle: missing $SRC/.build/bin/$b" >&2
    exit 1
  fi
  install -m 0755 "$SRC/.build/bin/$b" "$DEST/bin/$b"
done

# ── the release-scoped host assets ────────────────────────────────────────
python3 "$SRC/scripts/install_runtime_assets.py" \
  --source-root "$SRC" --runtime-root "$DEST" --groups host-assets \
  | sed 's/^/  [assets] /'

# ── the OpenCode payload, staged through the real allowlist ──────────────
# shellcheck source=/dev/null
. "$SRC/agent_installation/scripts/stage_runtime_package.sh"
staged="$(MPM_RUNTIME_PACKAGE_VALIDATOR=mpm_validate_runtime_package_opencode \
  stage_runtime_package "$SRC/agent_installation/mpm-opencode" "$DEST" \
  "mpm-opencode" "${MANIFEST[@]}")"
echo "  [stage] payload at $staged"

# The payload's provenance sidecar names the revision it was built from.
printf '{"commit":"%s","built":"%s"}\n' "$COMMIT" "$EPOCH" \
  > "$DEST/agent_installation/mpm-opencode/dist/mpm-build-provenance.json"

# ── the build identity ────────────────────────────────────────────────────
cat > "$DEST/RELEASE.json" <<EOF
{
  "revision": "$REV",
  "commit": "$COMMIT",
  "short_commit": "$(printf '%s' "$COMMIT" | cut -c1-8)",
  "build_epoch": $EPOCH,
  "version": "$(bash "$SRC/scripts/build-provenance.sh" version 2>/dev/null || echo unknown)",
  "built_at": "$(date -u -d "@$EPOCH" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)",
  "bundle_layout": "versioned-bundle/1"
}
EOF

# ── the bundle must be Git-free ──────────────────────────────────────────
if [ -e "$DEST/.git" ]; then
  echo "build-bundle: the bundle carries .git" >&2
  exit 1
fi

echo "  [bundle] $DEST ($(du -sh "$DEST" | cut -f1), $(find "$DEST" -type f | wc -l) files)"
