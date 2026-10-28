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

# ── the runtime package payloads, staged through the real allowlist ───────
# The allowlist comes from the SAME data manifest the installer uses
# (scripts/runtime_payloads.json).
#
# It used to be a THIRD hard-coded copy — the adapter's own
# OPENCODE_RUNTIME_MANIFEST, the installer's RB_OPENCODE_MANIFEST, and
# this one. Two of them are guarded by a contract test; this one was
# not, and it was already stale the moment the data manifest was
# introduced. A copy that nothing checks is a copy that drifts.
#
# The staging CALL stays the rehearsal's own. The rehearsal must
# exercise the staging path independently of the installer's own
# wrapper, or a bug in that wrapper would be invisible to it.
# shellcheck source=/dev/null
. "$SRC/agent_installation/scripts/stage_runtime_package.sh"

MANIFEST_FILE="$SRC/scripts/runtime_payloads.json"
if [ ! -f "$MANIFEST_FILE" ]; then
  echo "build-bundle: the runtime payload manifest is missing at $MANIFEST_FILE" >&2
  exit 1
fi

# rb_payload_records emits one record per declared payload:
#   <adapter>|<plugin id>|<validator>|<file> <file> ...
# shellcheck source=/dev/null
. "$SRC/scripts/lib/runtime_bundle.sh"

records="$(rb_payload_records "$MANIFEST_FILE")" || {
  echo "build-bundle: the runtime payload manifest could not be read" >&2
  exit 1
}

staged_count=0
while IFS='|' read -r adapter plugin_id validator files; do
  [ -n "$adapter" ] || continue
  staged_count=$((staged_count + 1))
  src_dir="$SRC/agent_installation/$adapter"
  if [ ! -d "$src_dir" ]; then
    echo "build-bundle: the release declares a payload for '$adapter' but $src_dir does not exist" >&2
    exit 1
  fi
  IFS=' ' read -r -a file_list <<< "$files"
  if [ "${#file_list[@]}" -eq 0 ]; then
    echo "build-bundle: the payload for '$adapter' lists no files" >&2
    exit 1
  fi
  staged="$(MPM_RUNTIME_PACKAGE_VALIDATOR="$validator" \
    stage_runtime_package "$src_dir" "$DEST" "$plugin_id" "${file_list[@]}")"
  echo "  [stage] $adapter payload at $staged"

  # The payload's provenance sidecar names the revision it was built from.
  printf '{"commit":"%s","built":"%s"}\n' "$COMMIT" "$EPOCH" \
    > "$DEST/agent_installation/$adapter/dist/mpm-build-provenance.json"
done <<EOF
$records
EOF

if [ "$staged_count" -eq 0 ]; then
  echo "build-bundle: the release declares no runtime package payload" >&2
  exit 1
fi

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
